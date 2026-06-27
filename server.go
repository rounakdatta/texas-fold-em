package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/rounakdatta/texas-fold-em/internal/integration"
	"github.com/rounakdatta/texas-fold-em/internal/integration/classifier"
	"github.com/rounakdatta/texas-fold-em/internal/integration/ui"
)

// Server wires the Broker up to HTTP. Routes are defined in Handler().
//
// The integration fields are optional: when nil, the broker behaves
// exactly as before (no /admin/firefly/sync endpoint, /health omits the
// integration block). When set, the integration routes are registered.
type Server struct {
	broker        *Broker
	brokerKey     string
	adminKey      string
	log           *slog.Logger
	started       time.Time
	integration        *integration.DB
	fireflySyncer         *integration.Syncer
	foldSyncer            *integration.FoldSyncer
	foldAccountsSyncer    *integration.FoldAccountsSyncer
	fireflyAccountsSyncer *integration.FireflyAccountsSyncer
	classifier            *classifier.Classifier
	pusher             *integration.Pusher
	uiHandler          *ui.Handler
}

// NewServer constructs a Server. Keys are required (config validation
// enforces this upstream).
func NewServer(broker *Broker, brokerKey, adminKey string, log *slog.Logger) *Server {
	return &Server{
		broker:    broker,
		brokerKey: brokerKey,
		adminKey:  adminKey,
		log:       log.With("component", "http"),
		started:   time.Now(),
	}
}

// SetIntegration attaches the integration DB to the server. Idempotent;
// passing nil clears it. Called from main only when the
// TEXAS_FOLDEM_INTEGRATION_ENABLED feature flag is true.
func (s *Server) SetIntegration(db *integration.DB) { s.integration = db }

// SetFireflySyncer attaches the firefly syncer. When set, the
// POST /admin/firefly/sync endpoint is registered.
func (s *Server) SetFireflySyncer(syncer *integration.Syncer) { s.fireflySyncer = syncer }

// SetFoldSyncer attaches the fold staging syncer. When set, the
// POST /admin/fold/sync endpoint is registered.
func (s *Server) SetFoldSyncer(syncer *integration.FoldSyncer) { s.foldSyncer = syncer }

// SetFoldAccountsSyncer attaches the fold-accounts mirror syncer. When
// set, the POST /admin/fold/accounts/sync endpoint is registered.
func (s *Server) SetFoldAccountsSyncer(syncer *integration.FoldAccountsSyncer) {
	s.foldAccountsSyncer = syncer
}

// SetFireflyAccountsSyncer attaches the firefly-accounts mirror syncer.
// When set, the POST /admin/firefly/accounts/sync endpoint is registered.
func (s *Server) SetFireflyAccountsSyncer(syncer *integration.FireflyAccountsSyncer) {
	s.fireflyAccountsSyncer = syncer
}

// SetClassifier attaches the classifier. When set, the
// POST /admin/classify endpoint is registered.
func (s *Server) SetClassifier(c *classifier.Classifier) { s.classifier = c }

// SetPusher attaches the Pusher. When set, the
// POST /admin/push/{fold_uuid} endpoint is registered.
func (s *Server) SetPusher(p *integration.Pusher) { s.pusher = p }

// SetUI attaches the review UI handler. When set, /admin/ui/* routes
// are registered.
func (s *Server) SetUI(h *ui.Handler) { s.uiHandler = h }

// Handler returns the full HTTP mux. Routes:
//
//	GET  /livez       always 200 while process is alive (k8s liveness/readiness probe)
//	GET  /health      richer status; 503 when unseeded (for humans/monitoring)
//	GET  /token       bearer brokerKey — returns access_token + device_hash + user_uuid
//	POST /init        bearer adminKey  — seeds/reseeds the refresh chain
//
// Requests to any other path return 404.
//
// /livez and /health are intentionally different endpoints: k8s probes must
// NOT gate on seeded-ness — if they did, the Service wouldn't route /init
// traffic to an unseeded pod, and you could never bootstrap it. /health is
// the human-facing status endpoint; /livez is the probe endpoint.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", s.handleLivez)
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.Handle("GET /token", s.bearer(s.brokerKey, s.handleToken))
	mux.Handle("POST /init", s.bearer(s.adminKey, s.handleInit))
	if s.fireflySyncer != nil {
		// Long-running sync — admin-key gated. Body returns the SyncReport
		// (counts + duration) so an operator can confirm the result.
		mux.Handle("POST /admin/firefly/sync", s.bearer(s.adminKey, s.handleFireflySync))
	}
	if s.foldSyncer != nil {
		mux.Handle("POST /admin/fold/sync", s.bearer(s.adminKey, s.handleFoldSync))
	}
	if s.foldAccountsSyncer != nil {
		mux.Handle("POST /admin/fold/accounts/sync", s.bearer(s.adminKey, s.handleFoldAccountsSync))
	}
	if s.fireflyAccountsSyncer != nil {
		mux.Handle("POST /admin/firefly/accounts/sync", s.bearer(s.adminKey, s.handleFireflyAccountsSync))
	}
	if s.classifier != nil {
		mux.Handle("POST /admin/classify", s.bearer(s.adminKey, s.handleClassify))
	}
	if s.pusher != nil {
		mux.Handle("POST /admin/push/{fold_uuid}", s.bearer(s.adminKey, s.handlePush))
	}
	if s.uiHandler != nil {
		// UI mounts its own routes; auth handled by the UI handler
		// (cookie or upstream-proxy/tinyauth, configured in main).
		s.uiHandler.Mount(mux)
	}
	return s.withLogging(mux)
}

// handleLivez is a constant-time "the process is serving HTTP" check.
// Deliberately ignores seeded state so k8s won't remove the pod from the
// Service endpoint list just because it hasn't been /init'd yet.
func (s *Server) handleLivez(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleHealth returns broker status. Unauthenticated so a load balancer /
// uptime check / systemd readiness probe can hit it without a key.
//
// The payload deliberately carries only non-sensitive info: is there a seed,
// when's the next refresh due, what's the user UUID. No tokens.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	status := s.broker.Status()
	type integrationStatus struct {
		Enabled bool   `json:"enabled"`
		DBPath  string `json:"db_path,omitempty"`
	}
	body := struct {
		Service     string             `json:"service"`
		Uptime      string             `json:"uptime"`
		Integration *integrationStatus `json:"integration,omitempty"`
		Status
	}{
		Service: "texas-fold-em",
		Uptime:  time.Since(s.started).Round(time.Second).String(),
		Status:  status,
	}
	if s.integration != nil {
		body.Integration = &integrationStatus{Enabled: true, DBPath: s.integration.Path()}
	}
	code := http.StatusOK
	if !status.Ready() {
		// 503 covers both "never seeded" and "tombstoned by a previous
		// rejection" — in both cases /token can't be served until /init.
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, body)
}

// handleToken returns a usable access token. Refreshes lazily under the hood.
func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	tok, err := s.broker.Token(ctx)
	if err != nil {
		s.mapBrokerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tok)
}

// handleInit accepts {refresh_token, device_hash}, validates the seed by
// exchanging it with Fold, and persists the result. Returns the resulting
// access token bundle so callers can confirm the seed works in one round
// trip.
func (s *Server) handleInit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RefreshToken string `json:"refresh_token"`
		DeviceHash   string `json:"device_hash"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body", err.Error())
		return
	}
	body.RefreshToken = strings.TrimSpace(body.RefreshToken)
	body.DeviceHash = strings.TrimSpace(body.DeviceHash)

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	tok, err := s.broker.Init(ctx, body.RefreshToken, body.DeviceHash)
	if err != nil {
		s.mapBrokerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tok)
}

// mapBrokerError converts broker-layer errors into HTTP responses. Designed
// so consumers can branch on status codes:
//
//	400 — malformed input (ErrBadInput)
//	401 — wrong bearer key (handled by bearer middleware, not here)
//	409 — broker needs /init (ErrNotSeeded)
//	410 — refresh chain dead, must re-seed (ErrRefreshRejected)
//	502 — transient upstream issue (retry)
//	504 — upstream timeout / ctx cancelled (retry)
func (s *Server) mapBrokerError(w http.ResponseWriter, err error) {
	var rej *ErrRefreshRejected
	switch {
	case errors.Is(err, ErrBadInput):
		writeErr(w, http.StatusBadRequest, "invalid input", err.Error())
	case errors.Is(err, ErrNotSeeded):
		writeErr(w, http.StatusConflict, "not initialised", err.Error())
	case errors.As(err, &rej):
		s.log.Error("refresh rejected — re-seed required", "code", rej.Code, "msg", rej.Message)
		writeErr(w, http.StatusGone, "refresh chain rejected; POST /init to recover", rej.Error())
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		writeErr(w, http.StatusGatewayTimeout, "upstream timeout", err.Error())
	default:
		s.log.Warn("broker error", "err", err)
		// Everything else we conservatively map to 502 — it's an upstream
		// HTTP call or a disk-persist error, both of which are "service
		// can't currently fulfil this request, try again". A correct
		// client retries 502; does not retry 500.
		writeErr(w, http.StatusBadGateway, "upstream error", err.Error())
	}
}

// bearer is a middleware that enforces a single fixed bearer token. It
// compares with constant time to avoid leaking key material through timing.
func (s *Server) bearer(expected string, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(h, prefix) {
			writeErr(w, http.StatusUnauthorized, "missing Authorization: Bearer <key>", "")
			return
		}
		got := h[len(prefix):]
		if subtle.ConstantTimeCompare([]byte(got), []byte(expected)) != 1 {
			s.log.Warn("auth failure", "remote", r.RemoteAddr, "path", r.URL.Path)
			writeErr(w, http.StatusUnauthorized, "invalid bearer token", "")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// withLogging wraps the mux with per-request structured logging. We log at
// Info for successes and Warn for 5xx, so prod dashboards surface the
// latter.
func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(rec, r)
		dur := time.Since(start)

		attrs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.code,
			"duration_ms", dur.Milliseconds(),
			"remote", r.RemoteAddr,
		}
		switch {
		case rec.code >= 500:
			s.log.Warn("request", attrs...)
		case rec.code >= 400:
			s.log.Info("request (client error)", attrs...)
		default:
			s.log.Debug("request", attrs...)
		}
	})
}

type statusRecorder struct {
	http.ResponseWriter
	code    int
	written bool
}

func (r *statusRecorder) WriteHeader(c int) {
	if !r.written {
		r.code = c
		r.written = true
	}
	r.ResponseWriter.WriteHeader(c)
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

type errBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
	Detail  string `json:"detail,omitempty"`
}

func writeErr(w http.ResponseWriter, code int, msg, detail string) {
	writeJSON(w, code, errBody{
		Error:   http.StatusText(code),
		Message: msg,
		Detail:  detail,
	})
}
