package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// okReply is a chat-completions answer carrying content.
func okReply(w http.ResponseWriter, content string, promptTokens int) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":      "chatcmpl-x",
		"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}},
		"usage":   map[string]any{"prompt_tokens": promptTokens, "completion_tokens": 7, "total_tokens": promptTokens + 7},
	})
}

// A model that reasons (Claude behind auth2api) refuses any temperature but
// its default while thinking, so a reasoning request must carry the effort
// and no temperature at all; without reasoning it stays deterministic.
func TestReasoningRequestsCarryTheEffortAndNoTemperature(t *testing.T) {
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		got = append(got, body)
		okReply(w, `{}`, 10)
	}))
	t.Cleanup(srv.Close)

	for _, effort := range []string{"low", "", "none"} {
		c := NewClient("k", "claude-opus-5-5", srv.URL, srv.Client())
		c.SetReasoningEffort(effort)
		c.SetMaxTokens(6000)
		if _, err := c.GenerateJSON(context.Background(), "sys", "user"); err != nil {
			t.Fatalf("effort %q: %v", effort, err)
		}
	}
	if len(got) != 3 {
		t.Fatalf("requests = %d, want 3", len(got))
	}
	low, plain, none := got[0], got[1], got[2]
	if low["reasoning_effort"] != "low" {
		t.Errorf("reasoning request carried reasoning_effort=%v, want low", low["reasoning_effort"])
	}
	if _, has := low["temperature"]; has {
		t.Errorf("reasoning request carried a temperature (%v); a thinking model rejects it", low["temperature"])
	}
	// no effort leaves thinking to the host; "none" asks for none, in so
	// many words — a Claude 5 model otherwise thinks unseen
	if _, has := plain["reasoning_effort"]; has {
		t.Errorf("no effort: sent reasoning_effort=%v", plain["reasoning_effort"])
	}
	if none["reasoning_effort"] != "none" {
		t.Errorf(`"none": reasoning_effort = %v, want "none" sent`, none["reasoning_effort"])
	}
	for name, body := range map[string]map[string]any{"no effort": plain, `"none"`: none} {
		if temp, ok := body["temperature"].(float64); !ok || temp != 0 {
			t.Errorf("%s: temperature = %v, want an explicit 0", name, body["temperature"])
		}
	}
	for i, body := range got {
		if body["max_tokens"] != float64(6000) {
			t.Errorf("request %d: max_tokens = %v, want the configured 6000", i, body["max_tokens"])
		}
		if body["model"] != "claude-opus-5-5" {
			t.Errorf("request %d: model = %v", i, body["model"])
		}
	}
}

// The engine's health is read from the UI, so every request is remembered:
// outcome, status, latency and tokens, newest first, with running totals.
func TestEveryCallIsRememberedNewestFirst(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if n.Add(1) == 3 {
			http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
			return
		}
		okReply(w, `{"ok":true}`, 1000+int(n.Load()))
	}))
	t.Cleanup(srv.Close)
	c := NewClient("k", "m", srv.URL, srv.Client())
	c.backoffFunc = instantBackoff

	_, _ = c.GenerateJSON(context.Background(), "s", "first")  // OK, 1001 tokens
	_, _ = c.GenerateJSON(context.Background(), "s", "second") // OK, 1002 tokens
	_, _ = c.GenerateJSON(context.Background(), "s", "third")  // 400: terminal

	calls, ok, failed := c.RecentCalls()
	if ok != 2 || failed != 1 || len(calls) != 3 {
		t.Fatalf("ok=%d failed=%d calls=%d, want 2, 1, 3", ok, failed, len(calls))
	}
	if calls[0].OK || calls[0].Status != 400 || !strings.Contains(calls[0].Error, "bad request") {
		t.Errorf("newest call = %+v, want the 400, saying why", calls[0])
	}
	if !calls[1].OK || calls[1].PromptTokens != 1002 || !calls[2].OK || calls[2].PromptTokens != 1001 {
		t.Errorf("older calls = %+v, %+v; want the second then the first, with their tokens", calls[1], calls[2])
	}
}

func TestOnlyTheLastFiftyCallsAreKept(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { okReply(w, `{}`, 1) }))
	t.Cleanup(srv.Close)
	c := NewClient("k", "m", srv.URL, srv.Client())
	for range 60 {
		_, _ = c.GenerateJSON(context.Background(), "s", "u")
	}
	calls, ok, _ := c.RecentCalls()
	if len(calls) != keptCalls || ok != 60 {
		t.Errorf("kept %d calls (want %d) and counted %d OK (want 60)", len(calls), keptCalls, ok)
	}
}

// Three whole calls failing in a row rest the client: the next one returns
// at once, without a request, and says until when.
func TestARunOfFailuresRestsTheClient(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "upstream down", http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)
	c := NewClient("k", "m", srv.URL, srv.Client())
	c.backoffFunc = instantBackoff

	for i := range 3 {
		if _, err := c.GenerateJSON(context.Background(), "s", "u"); err == nil {
			t.Fatalf("call %d succeeded against a failing gateway", i)
		}
	}
	if h := c.Health(); h.Available || h.FailStreak != 3 || !strings.Contains(h.Reason, "3 calls in a row") {
		t.Fatalf("after 3 failed calls, health = %+v; want resting, streak 3, with the reason", h)
	}
	before := hits.Load()
	_, err := c.GenerateJSON(context.Background(), "s", "u")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a call while resting = %v, want ErrUnavailable", err)
	}
	if hits.Load() != before {
		t.Errorf("a resting client still sent %d request(s)", hits.Load()-before)
	}
	if h := c.Health(); h.RestUntil.Before(time.Now().Add(90 * time.Second)) {
		t.Errorf("rest ends %v; want about two minutes out", h.RestUntil)
	}
}

// A success clears the streak: two failures, a success, two failures must
// not rest the client.
func TestASuccessClearsTheFailureStreak(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if n.Add(1) == 3 {
			okReply(w, `{}`, 1)
			return
		}
		http.Error(w, "nope", http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)
	c := NewClient("k", "m", srv.URL, srv.Client())
	for range 5 {
		_, _ = c.GenerateJSON(context.Background(), "s", "u")
	}
	if h := c.Health(); !h.Available || h.FailStreak != 2 {
		t.Errorf("health = %+v; want available with a streak of 2", h)
	}
	if h := c.Health(); h.LastOK.IsZero() {
		t.Errorf("the success wasn't recorded: %+v", h)
	}
}

// A refused key won't start working on the next try: one refusal rests the
// client for a while instead of hammering the gateway with it.
func TestARefusedKeyRestsTheClientAtOnce(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, `{"error":"Invalid API key"}`, http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	c := NewClient("bad", "m", srv.URL, srv.Client())
	_, _ = c.GenerateJSON(context.Background(), "s", "u")
	h := c.Health()
	if h.Available || !strings.Contains(h.Reason, "refused the key") {
		t.Fatalf("after a 403, health = %+v; want resting because the key was refused", h)
	}
	if _, err := c.GenerateJSON(context.Background(), "s", "u"); !errors.Is(err, ErrUnavailable) || hits.Load() != 1 {
		t.Errorf("second call = %v after %d requests; want ErrUnavailable and no new request", err, hits.Load())
	}
}

// A subscription at its limit answers 429 with a long Retry-After. Waiting
// that out inside the call would stall a whole classify pass for hours, so
// the client returns at once and rests for as long as it was asked.
func TestALongRetryAfterRestsInsteadOfWaiting(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Retry-After", "3600")
		http.Error(w, `{"error":"rate_limit_error"}`, http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)
	c := NewClient("k", "m", srv.URL, srv.Client())
	start := time.Now()
	_, err := c.GenerateJSON(context.Background(), "s", "u")
	if err == nil {
		t.Fatal("a 429 came back as success")
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("the call waited %v; a long Retry-After must not be waited out inline", took)
	}
	if hits.Load() != 1 {
		t.Errorf("retried %d times into a long Retry-After", hits.Load()-1)
	}
	h := c.Health()
	if h.Available || h.RestUntil.Before(time.Now().Add(59*time.Minute)) || !strings.Contains(h.Reason, "pause") {
		t.Errorf("health = %+v; want resting for about the hour it was asked", h)
	}
}

// A header asking for days is capped: the engine comes back within hours.
func TestRestIsCapped(t *testing.T) {
	c := NewClient("k", "m", "http://unused", nil)
	c.rest(72*time.Hour, "a silly header")
	if h := c.Health(); h.RestUntil.After(time.Now().Add(maxRest + time.Minute)) {
		t.Errorf("rest until %v; want at most %v", h.RestUntil, maxRest)
	}
}

// Anthropic's "overloaded" (529) is worth a second try, like a 503.
func TestOverloadedIsRetried(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if n.Add(1) == 1 {
			w.WriteHeader(529)
			return
		}
		okReply(w, `{"ok":1}`, 1)
	}))
	t.Cleanup(srv.Close)
	c := NewClient("k", "m", srv.URL, srv.Client())
	c.backoffFunc = instantBackoff
	if out, err := c.GenerateJSON(context.Background(), "s", "u"); err != nil || !strings.Contains(out, "ok") {
		t.Fatalf("after one 529: %q, %v; want the retry's answer", out, err)
	}
}

// The evaluation compares engines with a client of its own: another model
// or effort, the same gateway and key, and stats that don't mix with the
// engine making suggestions.
func TestOverridesMakeAnIndependentClient(t *testing.T) {
	var models []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body chatCompletionRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		models = append(models, body.Model+"/"+body.ReasoningEffort+"/"+r.Header.Get("Authorization"))
		okReply(w, `{}`, 1)
	}))
	t.Cleanup(srv.Close)
	base := NewClient("secret", "claude-opus-5-5", srv.URL, srv.Client())
	base.SetTimeout(90 * time.Second)
	other := base.WithOverrides("claude-sonnet-5", "medium")
	same := base.WithOverrides("", "")

	_, _ = other.GenerateJSON(context.Background(), "s", "u")
	_, _ = same.GenerateJSON(context.Background(), "s", "u")
	want := []string{"claude-sonnet-5/medium/Bearer secret", "claude-opus-5-5//Bearer secret"}
	if strings.Join(models, ",") != strings.Join(want, ",") {
		t.Errorf("requests = %v, want %v", models, want)
	}
	if _, ok, _ := base.RecentCalls(); ok != 0 {
		t.Errorf("the base client counted %d calls made by its copies", ok)
	}
	if other.timeout != 90*time.Second || other.Endpoint() != base.Endpoint() {
		t.Errorf("the copy lost the tunables: timeout %v, endpoint %q", other.timeout, other.Endpoint())
	}
}

func TestDisplayName(t *testing.T) {
	for id, want := range map[string]string{
		"claude-opus-5-5":   "Claude Opus 5.5",
		"claude-sonnet-5":   "Claude Sonnet 5",
		"claude-haiku-4-5":  "Claude Haiku 4.5",
		"deepseek-v4-flash": "DeepSeek V4 Flash",
		"gpt-5":             "GPT 5",
		"":                  "",
	} {
		if got := DisplayName(id); got != want {
			t.Errorf("DisplayName(%q) = %q, want %q", id, got, want)
		}
	}
}

// Claude Opus 5.5 refuses any temperature ("`temperature` is deprecated for
// this model", a 400). The client sends the same request without one, and
// never sends one again — while a host that takes a temperature keeps it.
func TestAHostThatRefusesATemperatureGetsNone(t *testing.T) {
	var withTemp, without atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if _, has := body["temperature"]; has {
			withTemp.Add(1)
			http.Error(w, `{"error":{"message":"`+"`temperature`"+` is deprecated for this model.","type":"invalid_request_error"}}`, http.StatusBadRequest)
			return
		}
		without.Add(1)
		okReply(w, `{"ok":true}`, 1)
	}))
	t.Cleanup(srv.Close)
	c := NewClient("k", "claude-opus-5-5", srv.URL, srv.Client())
	for i := range 3 {
		if out, err := c.GenerateJSON(context.Background(), "s", "u"); err != nil || !strings.Contains(out, "ok") {
			t.Fatalf("call %d: %q, %v", i, out, err)
		}
	}
	if withTemp.Load() != 1 || without.Load() != 3 {
		t.Errorf("requests with a temperature: %d (want the first only), without: %d (want 3)", withTemp.Load(), without.Load())
	}
	if h := c.Health(); !h.Available || h.FailStreak != 0 {
		t.Errorf("health after adapting = %+v; the refusal is not a failure of the engine", h)
	}
	// an evaluation's copy of the same model knows it already
	if _, err := c.WithOverrides("", "").GenerateJSON(context.Background(), "s", "u"); err != nil || withTemp.Load() != 1 {
		t.Errorf("the copy sent a temperature again (%d refusals), %v", withTemp.Load(), err)
	}
}
