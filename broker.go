package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// AccessToken is what consumers get back from GET /token. It contains
// everything they need to make authenticated Fold API calls themselves.
type AccessToken struct {
	AccessToken string    `json:"access_token"`
	DeviceHash  string    `json:"device_hash"`
	UserUUID    string    `json:"user_uuid"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// Broker is the core orchestration layer:
//
//   - Token() returns a currently-valid access token, refreshing lazily if
//     the cached one is within RefreshLead of expiring.
//   - Init() validates a new {refresh_token, device_hash} seed by actually
//     exchanging it for tokens, then persists the result. A bad seed is
//     rejected before overwriting known-good state.
//   - KeepWarm() is an optional background loop that refreshes ahead of
//     expiry so consumers never pay the latency of a refresh.
//
// All refresh operations are serialised through refreshMu so concurrent
// callers coalesce into a single upstream refresh (double-checked locking).
type Broker struct {
	store       *Store
	client      *FoldClient
	log         *slog.Logger
	refreshLead time.Duration

	// Serialises concurrent refreshes. We deliberately don't use
	// singleflight — a plain mutex with double-checked state reads is
	// simpler, has zero dependencies, and coalesces equally well for our
	// one-key use case.
	refreshMu sync.Mutex
}

// NewBroker wires the store, client, and logger together. refreshLead is
// how far before expiry we proactively refresh (e.g. 2m means "refresh if
// the cached access token has <2m left").
func NewBroker(store *Store, client *FoldClient, log *slog.Logger, refreshLead time.Duration) *Broker {
	return &Broker{
		store:       store,
		client:      client,
		log:         log.With("component", "broker"),
		refreshLead: refreshLead,
	}
}

// Token returns a currently-valid access token, refreshing if necessary.
// Returns ErrNotSeeded if /init has never been called, or
// *ErrRefreshRejected if Fold has revoked the refresh chain (either just
// now, or previously — we tombstone rejections so we don't retry forever).
func (b *Broker) Token(ctx context.Context) (AccessToken, error) {
	// Fast path: read current state. No refresh lock needed — we're just
	// observing the cached token.
	s := b.store.Get()
	if !s.Seeded() {
		return AccessToken{}, ErrNotSeeded
	}
	if s.Rejected() {
		// Don't hit Fold — the chain is known dead. Operator must /init.
		return AccessToken{}, &ErrRefreshRejected{Code: 0, Message: s.RejectedReason}
	}
	if time.Until(s.ExpiresAt) > b.refreshLead && s.AccessToken != "" {
		return toAccessToken(s)
	}

	// Slow path: refresh needed.
	return b.refreshLocked(ctx)
}

// Init validates a fresh seed by exchanging it with Fold and, only on
// success, persists the resulting state. It always overwrites any existing
// state (if you have the admin key, you know what you're doing).
func (b *Broker) Init(ctx context.Context, refreshToken, deviceHash string) (AccessToken, error) {
	if refreshToken == "" || deviceHash == "" {
		return AccessToken{}, fmt.Errorf("%w: refresh_token and device_hash are both required", ErrBadInput)
	}
	// Validate the JWT shape up-front so we don't pointlessly hit Fold with
	// a garbage token.
	if _, err := SubjectOfJWT(refreshToken); err != nil {
		return AccessToken{}, fmt.Errorf("%w: refresh_token is not a valid JWT: %v", ErrBadInput, err)
	}

	b.refreshMu.Lock()
	defer b.refreshMu.Unlock()

	wasRejected := b.store.Get().Rejected()

	bundle, err := b.client.RefreshTokens(ctx, refreshToken, deviceHash)
	if err != nil {
		// Note: we deliberately do NOT tombstone here. A bad /init seed is
		// the operator's problem, not Fold's revocation — we want the
		// operator to be able to retry /init with a corrected token
		// without having to do anything special.
		return AccessToken{}, fmt.Errorf("validate seed: %w", err)
	}

	// Writing a fresh State with no RejectedAt clears any previous
	// tombstone — that's the one and only way the tombstone gets lifted.
	next := State{
		RefreshToken: bundle.RefreshToken,
		AccessToken:  bundle.AccessToken,
		ExpiresAt:    bundle.ExpiresAt,
		DeviceHash:   deviceHash,
		UpdatedAt:    time.Now().UTC(),
	}
	if err := b.store.Set(next); err != nil {
		// Worst case here: Fold rotated the refresh token and we failed to
		// persist it. The next call will 401 and the operator re-seeds.
		// Log loudly so the operator doesn't miss it.
		b.log.Error("seed validation succeeded but persist failed — you must re-seed",
			"err", err)
		return AccessToken{}, fmt.Errorf("persist seed: %w", err)
	}
	if wasRejected {
		b.log.Info("broker re-initialised — tombstone cleared",
			"expires_at", next.ExpiresAt)
	} else {
		b.log.Info("broker initialised",
			"expires_at", next.ExpiresAt,
			"device_hash_prefix", prefix(deviceHash, 8))
	}
	return toAccessToken(next)
}

// refreshLocked performs a refresh under refreshMu. It re-reads state after
// locking so concurrent callers that queued up collapse into one upstream
// call.
//
// If Fold returns *ErrRefreshRejected, the state is tombstoned (RejectedAt
// set and persisted) so subsequent Token() / KeepWarm calls short-circuit
// without hitting Fold again until /init clears it. This is the key
// behaviour that prevents a dead chain from becoming a DoS vector against
// api.fold.money.
func (b *Broker) refreshLocked(ctx context.Context) (AccessToken, error) {
	b.refreshMu.Lock()
	defer b.refreshMu.Unlock()

	// Re-check after acquiring the lock. Another caller may have just
	// refreshed while we were waiting, or tombstoned the state.
	s := b.store.Get()
	if !s.Seeded() {
		return AccessToken{}, ErrNotSeeded
	}
	if s.Rejected() {
		return AccessToken{}, &ErrRefreshRejected{Code: 0, Message: s.RejectedReason}
	}
	if time.Until(s.ExpiresAt) > b.refreshLead && s.AccessToken != "" {
		return toAccessToken(s)
	}

	bundle, err := b.client.RefreshTokens(ctx, s.RefreshToken, s.DeviceHash)
	if err != nil {
		var rej *ErrRefreshRejected
		if errors.As(err, &rej) {
			b.tombstoneLocked(s, rej)
		}
		return AccessToken{}, err
	}

	next := State{
		RefreshToken: bundle.RefreshToken,
		AccessToken:  bundle.AccessToken,
		ExpiresAt:    bundle.ExpiresAt,
		DeviceHash:   s.DeviceHash,
		UpdatedAt:    time.Now().UTC(),
	}
	if err := b.store.Set(next); err != nil {
		// Rotation happened upstream but we couldn't persist the new
		// refresh token. On next call we'll re-attempt with the OLD
		// refresh token in state, which is now dead → 401 →
		// ErrRefreshRejected → operator re-seeds.
		b.log.Error("refresh succeeded upstream but state persist failed — next call will likely fail",
			"err", err)
		return AccessToken{}, fmt.Errorf("persist rotated state: %w", err)
	}
	b.log.Info("refreshed access token",
		"expires_at", next.ExpiresAt,
		"lead_seconds", int(time.Until(next.ExpiresAt).Seconds()))
	return toAccessToken(next)
}

// tombstoneLocked persists "this refresh chain is dead" to state. Caller
// must hold refreshMu. After tombstoning, Token() and KeepWarm refuse to
// hit Fold until /init replaces the state.
//
// Persist failures are logged but not returned: the original
// ErrRefreshRejected is still what the caller wants to see, and the
// in-memory store has been updated regardless of whether disk caught up.
// On the next restart without a persisted tombstone, we'd re-discover the
// rejection on first refresh attempt and tombstone again — self-healing.
func (b *Broker) tombstoneLocked(s State, rej *ErrRefreshRejected) {
	now := time.Now().UTC()
	next := s
	next.RejectedAt = &now
	next.RejectedReason = rej.Error()
	next.UpdatedAt = now
	if err := b.store.Set(next); err != nil {
		b.log.Error("failed to persist rejection tombstone (in-memory state still updated)",
			"err", err)
	}
	b.log.Error("refresh chain rejected by Fold — halting retries until /init",
		"code", rej.Code, "message", rej.Message, "rejected_at", now)
}

// KeepWarm runs until ctx is cancelled, checking every `every` whether a
// refresh is due and performing it if so. Errors are logged and swallowed —
// the next tick tries again. A 0 interval disables the loop.
//
// Two cases where the loop ticks but does nothing, on purpose:
//   - not yet seeded (waiting for /init)
//   - tombstoned after a previous rejection (waiting for /init to clear it)
//
// In both cases we don't touch Fold; we just wait for the operator.
func (b *Broker) KeepWarm(ctx context.Context, every time.Duration) {
	if every <= 0 {
		b.log.Info("keep-warm disabled")
		return
	}
	b.log.Info("keep-warm started", "every", every)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			b.log.Info("keep-warm stopped")
			return
		case <-t.C:
			s := b.store.Get()
			switch {
			case !s.Seeded():
				b.log.Debug("keep-warm tick: not seeded, idle")
				continue
			case s.Rejected():
				b.log.Debug("keep-warm tick: chain rejected, idle until /init",
					"rejected_at", s.RejectedAt, "reason", s.RejectedReason)
				continue
			case time.Until(s.ExpiresAt) > b.refreshLead:
				b.log.Debug("keep-warm tick: token still fresh", "expires_at", s.ExpiresAt)
				continue
			}
			refreshCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			_, err := b.refreshLocked(refreshCtx)
			cancel()
			if err != nil {
				var rej *ErrRefreshRejected
				switch {
				case errors.As(err, &rej):
					// Already logged + tombstoned by refreshLocked.
					// This block exists only to suppress the generic
					// "refresh failed" warn below.
				case errors.Is(err, context.Canceled):
					// shutdown racing with tick; ignore
				default:
					b.log.Warn("keep-warm: refresh failed, will retry",
						"err", err)
				}
			}
		}
	}
}

// Status returns a compact, non-sensitive view of current state. Used by
// /health to surface whether the broker is ready to serve tokens.
//
// Pointer fields are used for the timestamps so they omit cleanly when the
// broker is unseeded (time.Time's zero value doesn't satisfy omitempty).
type Status struct {
	Seeded         bool       `json:"seeded"`
	Rejected       bool       `json:"rejected,omitempty"`
	ExpiresAt      *time.Time `json:"expires_at,omitempty"`
	SecondsLeft    *int       `json:"seconds_left,omitempty"`
	UpdatedAt      *time.Time `json:"updated_at,omitempty"`
	UserUUID       string     `json:"user_uuid,omitempty"`
	RejectedAt     *time.Time `json:"rejected_at,omitempty"`
	RejectedReason string     `json:"rejected_reason,omitempty"`
}

// Ready reports whether the broker can currently serve /token. Used by the
// HTTP health handler to choose between 200 and 503.
func (s Status) Ready() bool { return s.Seeded && !s.Rejected }

// Status returns a snapshot suitable for /health.
func (b *Broker) Status() Status {
	s := b.store.Get()
	out := Status{Seeded: s.Seeded(), Rejected: s.Rejected()}
	if !s.Seeded() {
		return out
	}
	exp := s.ExpiresAt
	upd := s.UpdatedAt
	left := int(time.Until(s.ExpiresAt).Seconds())
	out.ExpiresAt = &exp
	out.UpdatedAt = &upd
	out.SecondsLeft = &left
	if sub, err := SubjectOfJWT(s.RefreshToken); err == nil {
		out.UserUUID = sub
	}
	if s.Rejected() {
		out.RejectedAt = s.RejectedAt
		out.RejectedReason = s.RejectedReason
	}
	return out
}

// toAccessToken converts a persisted State into the public AccessToken view.
// The refresh token deliberately never crosses this boundary.
func toAccessToken(s State) (AccessToken, error) {
	sub, err := SubjectOfJWT(s.RefreshToken)
	if err != nil {
		return AccessToken{}, fmt.Errorf("derive user_uuid: %w", err)
	}
	return AccessToken{
		AccessToken: s.AccessToken,
		DeviceHash:  s.DeviceHash,
		UserUUID:    sub,
		ExpiresAt:   s.ExpiresAt,
	}, nil
}

func prefix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
