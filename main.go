// Package main is texas-fold-em, a long-running HTTP broker that holds a
// Fold refresh token and issues short-lived access tokens to downstream
// consumers on demand.
//
// Design contract (see README):
//   - POST /init  seeds or re-seeds the refresh chain.
//   - GET  /token returns a currently-valid access token + device hash + user UUID.
//   - GET  /health is an unauthenticated status probe.
//
// Reliability contract:
//   - State is persisted atomically (temp-file + fsync + rename). A mid-write
//     crash cannot corrupt the refresh chain.
//   - Concurrent /token callers coalesce into a single upstream refresh.
//   - A background keep-warm loop refreshes ahead of expiry so consumers
//     rarely pay refresh latency.
//   - SIGINT/SIGTERM trigger a graceful drain with a configurable grace
//     period; in-flight requests get to finish.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/rounakdatta/texas-fold-em/internal/integration"
	"github.com/rounakdatta/texas-fold-em/internal/integration/classifier"
	"github.com/rounakdatta/texas-fold-em/internal/integration/firefly"
	"github.com/rounakdatta/texas-fold-em/internal/integration/fold"
)

// Version is baked in at build time via -ldflags. Defaults to "dev".
var Version = "dev"

func main() {
	if err := run(); err != nil {
		// Use a plain fmt here — logger may not be initialised yet.
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}

	log := newLogger(cfg.LogLevel).With("service", "texas-fold-em", "version", Version)
	log.Info("starting",
		"listen", cfg.ListenAddr,
		"state_path", cfg.StatePath,
		"api_base", cfg.APIBase,
		"refresh_lead", cfg.RefreshLead,
		"keepwarm_every", cfg.KeepWarmEvery,
	)

	store, err := OpenStore(cfg.StatePath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}

	if s := store.Get(); s.Seeded() {
		log.Info("loaded existing state",
			"expires_at", s.ExpiresAt,
			"updated_at", s.UpdatedAt,
		)
	} else {
		log.Warn("state is empty — POST /init before consumers can fetch tokens")
	}

	client := NewFoldClient(cfg.APIBase, cfg.HTTPTimeout)
	broker := NewBroker(store, client, log, cfg.RefreshLead)
	srv := NewServer(broker, cfg.BrokerKey, cfg.AdminKey, log)

	// Optional fold→firefly integration. Only fires when explicitly enabled
	// via TEXAS_FOLDEM_INTEGRATION_ENABLED — on a stock deploy this code
	// path stays cold and the broker continues to behave exactly as before.
	if cfg.IntegrationEnabled {
		intLog := log.With("component", "integration")
		intLog.Info("opening integration db", "path", cfg.StagingDBPath)
		intDB, err := integration.Open(context.Background(), cfg.StagingDBPath)
		if err != nil {
			return fmt.Errorf("open integration db: %w", err)
		}
		// Closed during graceful shutdown alongside the http server.
		defer func() {
			if err := intDB.Close(); err != nil {
				intLog.Warn("integration db close", "err", err)
			}
		}()
		srv.SetIntegration(intDB)

		// Firefly read-side client + syncer. The PAT is sourced from the
		// firefly-pat key of the Bitwarden-synced texas-fold-em-credentials
		// Secret in production deploys.
		fireflyClient := firefly.NewClient(cfg.FireflyBase, cfg.FireflyPAT, nil)
		fireflySyncer := integration.NewSyncer(intDB, fireflyClient, intLog)
		srv.SetFireflySyncer(fireflySyncer)

		// Fold read-side client + staging syncer. Bridges the broker's
		// access tokens into the data-side fold endpoint via a closure;
		// keeps the broker's auth-side client and the integration's
		// data-side client physically separate.
		foldClient := fold.NewClient(cfg.APIBase, func(ctx context.Context) (fold.AccessToken, error) {
			tok, err := broker.Token(ctx)
			if err != nil {
				return fold.AccessToken{}, err
			}
			return fold.AccessToken{
				AccessToken: tok.AccessToken,
				DeviceHash:  tok.DeviceHash,
				UserUUID:    tok.UserUUID,
				ExpiresAt:   tok.ExpiresAt,
			}, nil
		}, nil)
		foldSyncer := integration.NewFoldSyncer(intDB, foldClient, intLog)
		srv.SetFoldSyncer(foldSyncer)

		// Deterministic classifier (Tiers 1+2). PR E adds Tier-3 (LLM RAG)
		// as a fallback inside ClassifyOne; PR F adds the push step.
		cls := classifier.New(intDB.DB, intLog, classifier.DefaultConfidenceThreshold, 10)
		srv.SetClassifier(cls)

		intLog.Info("integration ready",
			"firefly_base", cfg.FireflyBase,
			"fold_base", cfg.APIBase,
			"endpoints", []string{
				"POST /admin/firefly/sync",
				"POST /admin/fold/sync",
				"POST /admin/classify",
			},
		)
	}

	// Root ctx cancels on SIGINT/SIGTERM. Everything downstream observes it.
	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	httpSrv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Spawn background workers. wg tracks them so we don't return from run()
	// while they're still running.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		broker.KeepWarm(rootCtx, cfg.KeepWarmEvery)
	}()

	// Listen in its own goroutine so the main goroutine can orchestrate
	// shutdown on signal.
	serverErr := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		log.Info("http server listening", "addr", cfg.ListenAddr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- fmt.Errorf("http server: %w", err)
			return
		}
		serverErr <- nil
	}()

	// Wait for signal OR server crash, whichever comes first.
	select {
	case <-rootCtx.Done():
		log.Info("shutdown signal received", "signal", rootCtx.Err())
	case err := <-serverErr:
		if err != nil {
			log.Error("http server failed", "err", err)
			stop() // cascade cancel to keep-warm
			wg.Wait()
			return err
		}
	}

	// Graceful drain. We give in-flight requests up to ShutdownGrace to
	// finish, then force-close whatever remains.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Warn("http shutdown did not drain in time", "err", err)
	}

	// Cancel root context (if the signal arm didn't already) so KeepWarm exits.
	stop()
	wg.Wait()
	log.Info("shutdown complete")
	return nil
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: lvl,
		// Keep time in RFC3339 with ms — useful for cross-correlating with
		// Fold's X-Request-ID in their logs.
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				a.Value = slog.StringValue(time.Now().UTC().Format("2006-01-02T15:04:05.000Z"))
			}
			return a
		},
	})
	return slog.New(h)
}
