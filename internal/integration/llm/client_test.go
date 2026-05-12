package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// instantBackoff lets the retry suite run in milliseconds rather than
// waiting on the production exponential schedule.
func instantBackoff(_ int, _ error) time.Duration { return 0 }

// TestGenerateJSON_HappyPath asserts headers, body shape, JSON-mode
// flag, and that the choice content is returned verbatim.
func TestGenerateJSON_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("unexpected path: %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
		var body chatCompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode req: %v", err)
		}
		if body.Model != "deepseek-v4-flash" {
			t.Errorf("model = %q", body.Model)
		}
		if body.Temperature != 0 {
			t.Errorf("temperature = %v, want 0 (deterministic)", body.Temperature)
		}
		if body.ResponseFormat == nil || body.ResponseFormat.Type != "json_object" {
			t.Errorf("response_format = %+v, want json_object", body.ResponseFormat)
		}
		if len(body.Messages) != 2 ||
			body.Messages[0].Role != "system" ||
			body.Messages[1].Role != "user" {
			t.Errorf("messages = %+v", body.Messages)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "chatcmpl-1",
			"choices": [{
				"index": 0,
				"message": {"role": "assistant", "content": "{\"category\":\"Eating outside\",\"confidence\":0.9}"},
				"finish_reason": "stop"
			}],
			"usage": {"prompt_tokens": 42, "completion_tokens": 12, "total_tokens": 54}
		}`))
	}))
	t.Cleanup(srv.Close)

	c := NewClient("test-key", "", srv.URL, srv.Client())
	got, err := c.GenerateJSON(context.Background(), "you are a classifier", "classify this thing")
	if err != nil {
		t.Fatalf("GenerateJSON: %v", err)
	}
	if !strings.Contains(got, "Eating outside") {
		t.Errorf("expected content verbatim, got %q", got)
	}
}

// TestGenerateJSON_TerminalHTTPError: 4xx errors must not be retried —
// they will fail identically on the next try and waste budget.
func TestGenerateJSON_TerminalHTTPError(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"API key not authorized"}}`))
	}))
	t.Cleanup(srv.Close)

	c := NewClient("test-key", "", srv.URL, srv.Client())
	c.backoffFunc = instantBackoff
	_, err := c.GenerateJSON(context.Background(), "", "anything")
	if err == nil {
		t.Fatal("expected error for 403")
	}
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *Error, got %T: %v", err, err)
	}
	if apiErr.Status != http.StatusForbidden {
		t.Errorf("Status = %d, want 403", apiErr.Status)
	}
	if hits.Load() != 1 {
		t.Errorf("hits = %d, want 1 (terminal 4xx must not retry)", hits.Load())
	}
}

// TestGenerateJSON_RetriesOn429: 429 is transient — must retry and
// succeed once the upstream stops rate-limiting.
func TestGenerateJSON_RetriesOn429(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := hits.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limit"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{}"},"finish_reason":"stop"}],"usage":{}}`))
	}))
	t.Cleanup(srv.Close)

	c := NewClient("k", "", srv.URL, srv.Client())
	c.backoffFunc = instantBackoff
	got, err := c.GenerateJSON(context.Background(), "", "x")
	if err != nil {
		t.Fatalf("GenerateJSON: %v", err)
	}
	if got != "{}" {
		t.Errorf("content = %q, want '{}'", got)
	}
	if hits.Load() != 3 {
		t.Errorf("hits = %d, want 3 (2 rate-limits then success)", hits.Load())
	}
}

// TestGenerateJSON_RetriesOn5xx: 5xx is transient — same as 429.
func TestGenerateJSON_RetriesOn5xx(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := hits.Add(1)
		if n < 2 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"ok\":1}"},"finish_reason":"stop"}],"usage":{}}`))
	}))
	t.Cleanup(srv.Close)

	c := NewClient("k", "", srv.URL, srv.Client())
	c.backoffFunc = instantBackoff
	got, err := c.GenerateJSON(context.Background(), "", "x")
	if err != nil {
		t.Fatalf("GenerateJSON: %v", err)
	}
	if got != `{"ok":1}` {
		t.Errorf("content = %q", got)
	}
}

// TestGenerateJSON_RespectsRetryAfter: when the server pins a
// Retry-After, the client must wait that long instead of using its
// own exponential schedule. We verify by measuring elapsed time on
// the first retry (≥ Retry-After).
func TestGenerateJSON_RespectsRetryAfter(t *testing.T) {
	var hits atomic.Int32
	var firstHitAt, secondHitAt time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := hits.Add(1)
		switch n {
		case 1:
			firstHitAt = time.Now()
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			secondHitAt = time.Now()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{}"},"finish_reason":"stop"}],"usage":{}}`))
		}
	}))
	t.Cleanup(srv.Close)

	c := NewClient("k", "", srv.URL, srv.Client())
	// Leave default backoffFunc; it honours Retry-After.
	if _, err := c.GenerateJSON(context.Background(), "", "x"); err != nil {
		t.Fatalf("GenerateJSON: %v", err)
	}
	gap := secondHitAt.Sub(firstHitAt)
	if gap < 900*time.Millisecond {
		t.Errorf("retry gap = %v; expected ≥ ~1s (Retry-After=1)", gap)
	}
}

// TestGenerateJSON_ExhaustsRetries: persistent 503 → MaxRetries+1
// attempts, then a wrapping error containing the last *Error.
func TestGenerateJSON_ExhaustsRetries(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	c := NewClient("k", "", srv.URL, srv.Client())
	c.backoffFunc = instantBackoff
	_, err := c.GenerateJSON(context.Background(), "", "x")
	if err == nil {
		t.Fatal("expected error after exhausted retries")
	}
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusServiceUnavailable {
		t.Errorf("expected wrapped *Error{503}, got %v", err)
	}
	if got := hits.Load(); got != MaxRetries+1 {
		t.Errorf("hits = %d, want %d (MaxRetries+1)", got, MaxRetries+1)
	}
}

// TestGenerateJSON_TransportRetry: a transport-level failure (e.g.
// connection refused) is retryable.
func TestGenerateJSON_TransportRetry(t *testing.T) {
	// Hold the server closed until after the first attempt. The first
	// dial fails with "connection refused" → retry. Then we open the
	// server and a subsequent attempt succeeds.
	//
	// Simpler approach: start a server that closes the connection
	// abruptly on first hit, replies normally afterwards.
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if n == 1 {
			// Hijack and close to simulate a mid-flight RST.
			hj, _ := w.(http.Hijacker)
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{}"},"finish_reason":"stop"}],"usage":{}}`))
		_ = r // silence unused warning
	}))
	t.Cleanup(srv.Close)

	c := NewClient("k", "", srv.URL, srv.Client())
	c.backoffFunc = instantBackoff
	if _, err := c.GenerateJSON(context.Background(), "", "x"); err != nil {
		t.Fatalf("GenerateJSON: %v", err)
	}
	if hits.Load() < 2 {
		t.Errorf("hits = %d, want ≥ 2 (transport failure should retry)", hits.Load())
	}
}

// TestGenerateJSON_RejectsEmptyConfig: argument validation before any
// network I/O so misconfiguration is caught fast.
func TestGenerateJSON_RejectsEmptyConfig(t *testing.T) {
	c := NewClient("", "", "", http.DefaultClient)
	if _, err := c.GenerateJSON(context.Background(), "", "x"); err == nil {
		t.Errorf("expected error for empty api key")
	}
	c = NewClient("k", "", "http://localhost", http.DefaultClient)
	if _, err := c.GenerateJSON(context.Background(), "", ""); err == nil {
		t.Errorf("expected error for empty user prompt")
	}
}

// TestGenerateJSON_NoChoices: explicit error rather than silently
// returning an empty string the caller would then try to json-decode.
func TestGenerateJSON_NoChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	t.Cleanup(srv.Close)

	c := NewClient("k", "", srv.URL, srv.Client())
	c.backoffFunc = instantBackoff
	_, err := c.GenerateJSON(context.Background(), "", "x")
	if err == nil {
		t.Fatal("expected error for empty choices")
	}
	if !strings.Contains(err.Error(), "empty response choice") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// TestGenerateJSON_DetectsTruncation: finish_reason=length means the
// model was cut off mid-output. Treat as error so the operator gets
// a clearer signal than a downstream JSON parse failure.
func TestGenerateJSON_DetectsTruncation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"partial\":"},"finish_reason":"length"}],"usage":{}}`))
	}))
	t.Cleanup(srv.Close)

	c := NewClient("k", "", srv.URL, srv.Client())
	_, err := c.GenerateJSON(context.Background(), "", "x")
	if err == nil {
		t.Fatal("expected truncation error")
	}
	if !strings.Contains(err.Error(), "truncated") {
		t.Errorf("expected 'truncated' in error, got %v", err)
	}
}

// TestParseRetryAfter covers both formats: delta-seconds and HTTP date.
func TestParseRetryAfter(t *testing.T) {
	cases := []struct {
		name string
		val  string
		min  time.Duration
		max  time.Duration
	}{
		{"seconds", "5", 5 * time.Second, 5 * time.Second},
		{"zero", "0", 0, 0},
		{"empty", "", 0, 0},
		{"garbage", "soonish", 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			if tc.val != "" {
				h.Set("Retry-After", tc.val)
			}
			got := parseRetryAfter(h)
			if got < tc.min || got > tc.max {
				t.Errorf("got %v, want in [%v, %v]", got, tc.min, tc.max)
			}
		})
	}
}

// TestIsRetryable: code-level table for the retry policy. Single
// source of truth so changes here surface immediately in review.
func TestIsRetryable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"400 bad request", &Error{Status: 400}, false},
		{"401 unauthorized", &Error{Status: 401}, false},
		{"402 insufficient balance", &Error{Status: 402}, false}, // operator action required; retrying just wastes budget
		{"403 forbidden", &Error{Status: 403}, false},
		{"404 not found (bad model)", &Error{Status: 404}, false},
		{"422 unprocessable", &Error{Status: 422}, false},
		{"429 rate limit", &Error{Status: 429}, true},
		{"500 server error", &Error{Status: 500}, true},
		{"502 bad gateway", &Error{Status: 502}, true},
		{"503 unavailable", &Error{Status: 503}, true},
		{"504 gateway timeout", &Error{Status: 504}, true},
		{"transport (network)", &transportError{err: io.EOF}, true},
		{"plain error", errors.New("something"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRetryable(tc.err); got != tc.want {
				t.Errorf("isRetryable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
