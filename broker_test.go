package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// makeJWT returns a JWT-shaped string with the given sub. Signature is dummy.
func makeJWT(sub string) string {
	head := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"` + sub + `","exp":1784037336}`))
	sig := base64.RawURLEncoding.EncodeToString([]byte("sig"))
	return head + "." + payload + "." + sig
}

// stubFold returns an httptest server that mints a new RT/AT pair on every
// /refresh call and records hit counts. Tests can set rejectOnCall to force
// a 401 on the N-th call. The minted tokens preserve the `sub` claim from
// the incoming refresh token, mirroring real Fold behaviour (rotation
// doesn't change the subject).
type stubFold struct {
	t            *testing.T
	server       *httptest.Server
	hits         atomic.Int64
	accessLife   time.Duration
	rejectOnCall int32 // 1-indexed; 0 means never
	fallbackSub  string
}

func newStubFold(t *testing.T) *stubFold {
	s := &stubFold{t: t, accessLife: 15 * time.Minute, fallbackSub: "user-uuid-001"}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := s.hits.Add(1)
		raw, _ := io.ReadAll(r.Body)

		if s.rejectOnCall > 0 && int32(n) == s.rejectOnCall {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"meta":{},"data":null,"error":{"code":2001,"message":"invalid","how_to_fix":"re-auth"}}`))
			return
		}

		// Preserve the subject from the incoming refresh token — real Fold
		// does this, and tests depend on it to verify user_uuid propagation.
		var body struct {
			RefreshToken string `json:"refresh_token"`
		}
		_ = json.Unmarshal(raw, &body)
		sub := s.fallbackSub
		if body.RefreshToken != "" {
			if extracted, err := SubjectOfJWT(body.RefreshToken); err == nil {
				sub = extracted
			}
		}

		at := makeJWT(sub)
		rt := makeJWT(sub)
		exp := time.Now().Add(s.accessLife).UTC()
		resp, _ := json.Marshal(map[string]any{
			"meta": map[string]any{"request_id": "rid"},
			"data": map[string]any{
				"token_type":    "Bearer",
				"access_token":  at,
				"refresh_token": rt,
				"expires_at":    exp.Format(time.RFC3339Nano),
			},
			"error": nil,
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(resp)
	}))
	return s
}

func (s *stubFold) Close() { s.server.Close() }
func (s *stubFold) Hits() int64 { return s.hits.Load() }

func newTestBroker(t *testing.T, stub *stubFold, refreshLead time.Duration) *Broker {
	t.Helper()
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	client := NewFoldClient(stub.server.URL, 5*time.Second)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewBroker(store, client, log, refreshLead)
}

func TestBroker_NotSeeded(t *testing.T) {
	stub := newStubFold(t)
	defer stub.Close()
	b := newTestBroker(t, stub, 2*time.Minute)

	_, err := b.Token(context.Background())
	if !errors.Is(err, ErrNotSeeded) {
		t.Fatalf("want ErrNotSeeded, got %v", err)
	}
	if stub.Hits() != 0 {
		t.Fatalf("unseeded Token() should not hit upstream; hits=%d", stub.Hits())
	}
}

func TestBroker_Init_Then_Token(t *testing.T) {
	stub := newStubFold(t)
	defer stub.Close()
	b := newTestBroker(t, stub, 2*time.Minute)

	tok, err := b.Init(context.Background(), makeJWT("user-uuid-001"), "dh-1")
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if tok.UserUUID != "user-uuid-001" {
		t.Fatalf("UserUUID = %q", tok.UserUUID)
	}
	if tok.DeviceHash != "dh-1" {
		t.Fatalf("DeviceHash = %q", tok.DeviceHash)
	}
	if tok.AccessToken == "" {
		t.Fatal("AccessToken empty")
	}
	if stub.Hits() != 1 {
		t.Fatalf("Init should consume exactly one upstream call; hits=%d", stub.Hits())
	}

	// Token() with a fresh cached token should NOT hit upstream.
	tok2, err := b.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tok2.AccessToken != tok.AccessToken {
		t.Fatal("Token() should return the cached access token")
	}
	if stub.Hits() != 1 {
		t.Fatalf("Token() with warm cache should not refresh; hits=%d", stub.Hits())
	}
}

func TestBroker_Token_RefreshesWhenNearExpiry(t *testing.T) {
	stub := newStubFold(t)
	stub.accessLife = 30 * time.Second // new tokens already within lead window
	defer stub.Close()
	b := newTestBroker(t, stub, 2*time.Minute) // refresh-lead 2m > 30s life

	if _, err := b.Init(context.Background(), makeJWT("u"), "dh"); err != nil {
		t.Fatal(err)
	}
	// Now Token() should refresh on every call because cached token is
	// always within lead window.
	for i := 0; i < 3; i++ {
		if _, err := b.Token(context.Background()); err != nil {
			t.Fatalf("Token %d: %v", i, err)
		}
	}
	// Init=1, Token*3 refreshes=3 → 4 total.
	if got := stub.Hits(); got != 4 {
		t.Fatalf("hits: got %d want 4", got)
	}
}

func TestBroker_RefreshChain_Rejected(t *testing.T) {
	stub := newStubFold(t)
	// Make Init's token already in the refresh-lead window so the next
	// Token() call forces an upstream refresh, which we'll force to 401.
	stub.accessLife = 30 * time.Second
	defer stub.Close()
	b := newTestBroker(t, stub, 2*time.Minute)

	if _, err := b.Init(context.Background(), makeJWT("u"), "dh"); err != nil {
		t.Fatal(err)
	}
	stub.rejectOnCall = 2 // the refresh triggered by Token() gets 401

	_, err := b.Token(context.Background())
	var rej *ErrRefreshRejected
	if !errors.As(err, &rej) {
		t.Fatalf("want ErrRefreshRejected, got %v", err)
	}
}

// Core regression for the "don't bombard Fold after rejection" behaviour:
// after the first 401, subsequent Token() calls must return ErrRefreshRejected
// WITHOUT hitting upstream again.
func TestBroker_Tombstone_StopsFurtherRefresh(t *testing.T) {
	stub := newStubFold(t)
	stub.accessLife = 30 * time.Second // Init's token immediately in lead window
	defer stub.Close()
	b := newTestBroker(t, stub, 2*time.Minute)

	if _, err := b.Init(context.Background(), makeJWT("u"), "dh"); err != nil {
		t.Fatal(err)
	}
	stub.rejectOnCall = 2 // the refresh triggered by the first Token() gets 401

	// First Token() call hits upstream, learns the chain is dead, tombstones.
	if _, err := b.Token(context.Background()); err == nil {
		t.Fatal("want error after rejection")
	}
	hitsAfterFirst := stub.Hits()
	if hitsAfterFirst != 2 {
		t.Fatalf("first Token() should have produced hit #2; hits=%d", hitsAfterFirst)
	}

	// The tombstone must be persisted in state.
	if !b.store.Get().Rejected() {
		t.Fatal("state should be tombstoned after rejection")
	}

	// 100 more Token() calls — zero upstream hits; all return ErrRefreshRejected.
	for i := 0; i < 100; i++ {
		_, err := b.Token(context.Background())
		var rej *ErrRefreshRejected
		if !errors.As(err, &rej) {
			t.Fatalf("call %d: want ErrRefreshRejected, got %v", i, err)
		}
	}
	if got := stub.Hits() - hitsAfterFirst; got != 0 {
		t.Fatalf("tombstoned broker must not hit upstream; extra hits=%d", got)
	}
}

// The tombstone must survive a restart. If the operator's homelab reboots
// after a rejection, we mustn't start hammering Fold again on boot.
func TestBroker_Tombstone_PersistsAcrossRestart(t *testing.T) {
	stub := newStubFold(t)
	stub.accessLife = 30 * time.Second
	defer stub.Close()

	// First broker: get it tombstoned.
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	{
		store, err := OpenStore(statePath)
		if err != nil {
			t.Fatal(err)
		}
		client := NewFoldClient(stub.server.URL, 5*time.Second)
		log := slog.New(slog.NewTextHandler(io.Discard, nil))
		b := NewBroker(store, client, log, 2*time.Minute)
		if _, err := b.Init(context.Background(), makeJWT("u"), "dh"); err != nil {
			t.Fatal(err)
		}
		stub.rejectOnCall = 2
		_, _ = b.Token(context.Background()) // triggers tombstone
	}
	hitsAtRestart := stub.Hits()
	stub.rejectOnCall = 0 // even if Fold started accepting again, we shouldn't ask

	// Second broker from the same state file.
	store2, err := OpenStore(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !store2.Get().Rejected() {
		t.Fatal("tombstone did not survive reopen")
	}
	client := NewFoldClient(stub.server.URL, 5*time.Second)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	b2 := NewBroker(store2, client, log, 2*time.Minute)

	// Fire some Token() calls; must not hit upstream.
	for i := 0; i < 5; i++ {
		_, err := b2.Token(context.Background())
		var rej *ErrRefreshRejected
		if !errors.As(err, &rej) {
			t.Fatalf("restarted broker: want ErrRefreshRejected, got %v", err)
		}
	}
	if stub.Hits() != hitsAtRestart {
		t.Fatalf("restarted broker hit upstream despite tombstone; extra=%d", stub.Hits()-hitsAtRestart)
	}
}

// After /init succeeds, the tombstone must be cleared so normal operation
// resumes.
func TestBroker_Init_ClearsTombstone(t *testing.T) {
	stub := newStubFold(t)
	stub.accessLife = 30 * time.Second
	defer stub.Close()
	b := newTestBroker(t, stub, 2*time.Minute)

	// Seed, get tombstoned.
	if _, err := b.Init(context.Background(), makeJWT("u"), "dh"); err != nil {
		t.Fatal(err)
	}
	stub.rejectOnCall = 2
	_, _ = b.Token(context.Background())
	if !b.store.Get().Rejected() {
		t.Fatal("precondition: state must be tombstoned")
	}

	// Re-init with a fresh seed (stub no longer rejecting).
	stub.rejectOnCall = 0
	stub.accessLife = 10 * time.Minute // plenty of headroom
	if _, err := b.Init(context.Background(), makeJWT("u2"), "dh"); err != nil {
		t.Fatalf("re-init: %v", err)
	}
	if b.store.Get().Rejected() {
		t.Fatal("/init must clear the tombstone")
	}

	// And Token() works normally again.
	tok, err := b.Token(context.Background())
	if err != nil {
		t.Fatalf("Token after re-init: %v", err)
	}
	if tok.UserUUID != "u2" {
		t.Fatalf("new user UUID not propagated: %q", tok.UserUUID)
	}
}

// Status/health output must distinguish "never seeded" from "seeded but
// tombstoned" so operators can tell at a glance what to do.
func TestBroker_Status_ReportsTombstone(t *testing.T) {
	stub := newStubFold(t)
	stub.accessLife = 30 * time.Second
	defer stub.Close()
	b := newTestBroker(t, stub, 2*time.Minute)

	if _, err := b.Init(context.Background(), makeJWT("u"), "dh"); err != nil {
		t.Fatal(err)
	}
	stub.rejectOnCall = 2
	_, _ = b.Token(context.Background())

	st := b.Status()
	if !st.Seeded || !st.Rejected {
		t.Fatalf("want seeded=true rejected=true, got %+v", st)
	}
	if st.Ready() {
		t.Fatal("Ready() must be false when tombstoned")
	}
	if st.RejectedAt == nil || st.RejectedReason == "" {
		t.Fatalf("RejectedAt/Reason must be populated: %+v", st)
	}
}

func TestBroker_ConcurrentTokens_CoalesceToOneRefresh(t *testing.T) {
	stub := newStubFold(t)
	// Initial token is short-lived (in the refresh-lead window) so the
	// first Token() call must refresh. The refresh itself returns a
	// long-lived token, which subsequent waiters should see via the
	// double-checked re-read — and therefore not refresh again.
	stub.accessLife = 30 * time.Second
	defer stub.Close()
	b := newTestBroker(t, stub, 2*time.Minute)

	if _, err := b.Init(context.Background(), makeJWT("u"), "dh"); err != nil {
		t.Fatal(err)
	}
	// From now on, refreshed tokens are far outside the lead window.
	stub.accessLife = 1 * time.Hour
	hitsBefore := stub.Hits()

	const N = 25
	var wg sync.WaitGroup
	wg.Add(N)
	start := make(chan struct{})
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			<-start
			if _, err := b.Token(context.Background()); err != nil {
				t.Errorf("Token: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	// Exactly 1 refresh is the expected outcome: the first caller under
	// refreshMu does the upstream call; all others find a long-lived token
	// in the re-read and return it cached. Allow up to 2 to tolerate
	// pathological scheduler interleavings.
	got := stub.Hits() - hitsBefore
	if got < 1 || got > 2 {
		t.Fatalf("concurrent Token: got %d refreshes, want 1 (coalescing expected)", got)
	}
}

func TestBroker_Init_ValidatesBeforePersist(t *testing.T) {
	stub := newStubFold(t)
	stub.rejectOnCall = 1 // first call (the validation) fails
	defer stub.Close()
	b := newTestBroker(t, stub, 2*time.Minute)

	_, err := b.Init(context.Background(), makeJWT("u"), "dh")
	if err == nil {
		t.Fatal("Init should fail when Fold rejects the seed")
	}
	if b.store.Get().Seeded() {
		t.Fatal("Init must not persist state when validation fails")
	}
}

func TestBroker_Init_RejectsMalformedJWT(t *testing.T) {
	stub := newStubFold(t)
	defer stub.Close()
	b := newTestBroker(t, stub, 2*time.Minute)

	_, err := b.Init(context.Background(), "not-a-jwt", "dh")
	if err == nil {
		t.Fatal("expected error for malformed refresh token")
	}
	if stub.Hits() != 0 {
		t.Fatal("malformed JWT should be rejected before any upstream call")
	}
}

func TestBroker_Status(t *testing.T) {
	stub := newStubFold(t)
	defer stub.Close()
	b := newTestBroker(t, stub, 2*time.Minute)

	st := b.Status()
	if st.Seeded {
		t.Fatal("fresh broker should report not seeded")
	}

	if _, err := b.Init(context.Background(), makeJWT("user-123"), "dh"); err != nil {
		t.Fatal(err)
	}
	st = b.Status()
	if !st.Seeded {
		t.Fatal("expected seeded after Init")
	}
	if st.UserUUID != "user-123" {
		t.Fatalf("UserUUID = %q", st.UserUUID)
	}
	if st.SecondsLeft == nil || *st.SecondsLeft <= 0 {
		t.Fatalf("SecondsLeft = %v", st.SecondsLeft)
	}
	if st.ExpiresAt == nil || st.UpdatedAt == nil {
		t.Fatal("ExpiresAt/UpdatedAt should be populated after Init")
	}
}
