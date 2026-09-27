// Package llm is a tiny client for OpenAI-compatible Chat Completion
// APIs. Used by the Tier-3 classifier to synthesise a structured
// firefly proposal from a raw fold transaction.
//
// Default target is DeepSeek (api.deepseek.com), but any OpenAI-API
// compatible host works — endpoint and model are both overridable
// via env vars (TEXAS_FOLDEM_LLM_BASE_URL, TEXAS_FOLDEM_LLM_MODEL).
// We stay protocol-pure rather than pulling in a provider SDK: a few
// hundred lines we own are easier to audit and pin against a single
// API version.
//
// Reliability:
//   - Retries with exponential backoff on transient failures
//     (429 + 5xx + network errors). Terminal 4xx errors return
//     immediately so a misconfigured key/model doesn't burn retry
//     budget that would fail identically.
//   - Per-attempt timeout via context; total budget = attempts ×
//     per-attempt timeout + backoffs.
//   - Honours Retry-After when present.
//
// Accuracy:
//   - temperature=0 (fully deterministic — we want consistent
//     classification, not creative writing).
//   - response_format=json_object (server enforces JSON output;
//     fewer parse failures than a "please reply in JSON" prompt).
//   - Generous max_tokens (4096) — the prompt asks the model for an
//     id-pinned JSON proposal plus a reasoning blurb, and 1024 was
//     occasionally truncating on long reasoning paths.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultModel is what the Tier-3 classifier uses on a fresh deploy.
// deepseek-v4-flash is DeepSeek's cost-efficient flagship at the time
// of writing; classification accuracy on our prompt is excellent and
// p50 latency is sub-second.
const DefaultModel = "deepseek-v4-flash"

// DefaultEndpoint is DeepSeek's OpenAI-compatible v1 base URL. Override
// via TEXAS_FOLDEM_LLM_BASE_URL to point at OpenAI / Groq / a local
// vLLM, etc.
const DefaultEndpoint = "https://api.deepseek.com/v1"

// DefaultPerAttemptTimeout bounds a single chat-completion call.
// The total budget for GenerateJSON is roughly:
//
//	(MaxRetries+1) × DefaultPerAttemptTimeout + sum(backoffs)
const DefaultPerAttemptTimeout = 60 * time.Second

// MaxRetries caps how many extra attempts we make on transient
// failure. 3 means up to 4 total HTTP requests in the worst case.
// The backoff schedule (1s, 3s, 9s with jitter, capped at 30s) keeps
// total wall-clock under 5 minutes even at the ceiling.
const MaxRetries = 3

// DefaultMaxTokens governs response length. The prompt asks for an
// id-pinned JSON plus a reasoning paragraph; 4096 leaves headroom.
const DefaultMaxTokens = 4096

// Client wraps an API key + HTTP client + retry policy. Safe for
// concurrent use by multiple Tier-3 invocations.
type Client struct {
	apiKey     string
	model      string
	endpoint   string
	httpClient *http.Client
	log        *slog.Logger

	// backoffFunc is the wait-before-attempt-N policy. Production uses
	// backoffDuration (exponential + jitter, honours Retry-After);
	// tests replace it with a zero-delay function so the retry suite
	// runs in milliseconds. Set via package-internal field access.
	backoffFunc func(int, error) time.Duration

	// Tunables for a stronger, slower model (SetTimeout, SetMaxTokens,
	// SetReasoningEffort). Zero values keep the defaults above.
	timeout         time.Duration
	maxTokens       int
	reasoningEffort string

	// calls remembers the last requests (outcome, latency, tokens) so the
	// engine's health can be read from the UI without logs or keys.
	mu        sync.Mutex
	calls     []CallStat
	okCount   int
	failCount int

	// The breaker (guarded by mu): after a long Retry-After, a refused key
	// or a run of failed calls, the client stops calling until openUntil.
	// A classify pass must not sleep for hours inside one call, and a
	// struggling gateway — or a subscription at its limit — is not helped
	// by being asked again every few seconds. Callers get ErrUnavailable at
	// once and keep what they have.
	openUntil   time.Time
	openReason  string
	failStreak  int
	lastOK      time.Time
	lastFailure string

	// noTemperature: the host refused a temperature for this model (Claude
	// Opus 5.5 does: "`temperature` is deprecated for this model"), so none
	// is sent from then on. Learned from the first refusal, not configured:
	// the same client works against a host that wants one and one that
	// rejects it.
	noTemperature bool
}

// ErrUnavailable is returned, without a request, while the client rests.
var ErrUnavailable = errors.New("llm: resting after failures")

// Health is the breaker's state, for the engine status.
type Health struct {
	Available  bool      `json:"available"`
	RestUntil  time.Time `json:"restUntil,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	FailStreak int       `json:"failStreak,omitempty"`
	LastOK     time.Time `json:"lastOk,omitempty"`
	LastError  string    `json:"lastError,omitempty"`
}

// Health reports whether the client will call now, and if not, until when.
func (c *Client) Health() Health {
	c.mu.Lock()
	defer c.mu.Unlock()
	h := Health{Available: !time.Now().Before(c.openUntil), FailStreak: c.failStreak, LastOK: c.lastOK, LastError: c.lastFailure}
	if !h.Available {
		h.RestUntil, h.Reason = c.openUntil, c.openReason
	}
	return h
}

// maxInlineWait is the longest Retry-After waited out inside a call; a
// longer one rests the client instead.
const maxInlineWait = 30 * time.Second

// maxRest caps any rest, so a bad header can't park the engine for days.
const maxRest = 6 * time.Hour

func (c *Client) rest(d time.Duration, why string) {
	if d > maxRest {
		d = maxRest
	}
	c.mu.Lock()
	if until := time.Now().Add(d); until.After(c.openUntil) {
		c.openUntil, c.openReason = until, why
	}
	c.mu.Unlock()
	if c.log != nil {
		c.log.Warn("llm: resting", "for", d.Round(time.Second), "reason", why)
	}
}

// settle updates the breaker after a whole GenerateJSON (all its attempts).
func (c *Client) settle(err error) {
	if err == nil {
		c.mu.Lock()
		c.failStreak, c.lastOK, c.lastFailure = 0, time.Now(), ""
		c.mu.Unlock()
		return
	}
	if errors.Is(err, ErrUnavailable) || errors.Is(err, context.Canceled) {
		return
	}
	var apiErr *Error
	isAPI := errors.As(err, &apiErr)
	c.mu.Lock()
	c.failStreak++
	streak := c.failStreak
	c.lastFailure = truncate(err.Error(), 200)
	c.mu.Unlock()
	switch {
	case isAPI && (apiErr.Status == http.StatusUnauthorized || apiErr.Status == http.StatusForbidden):
		// the key was refused: asking again won't change that
		c.rest(15*time.Minute, fmt.Sprintf("the gateway refused the key (HTTP %d)", apiErr.Status))
	case streak >= 3:
		// 2, 4, 8 … minutes, up to an hour
		c.rest(min(time.Duration(1<<min(streak-2, 6))*time.Minute, time.Hour), fmt.Sprintf("%d calls in a row failed", streak))
	}
}

// CallStat is one chat-completion request as the engine saw it.
type CallStat struct {
	At               time.Time `json:"at"`
	DurationMS       int64     `json:"durationMs"`
	OK               bool      `json:"ok"`
	Status           int       `json:"status,omitempty"` // HTTP status, when there was one
	Error            string    `json:"error,omitempty"`
	PromptTokens     int       `json:"promptTokens,omitempty"`
	CompletionTokens int       `json:"completionTokens,omitempty"`
}

const keptCalls = 50

func (c *Client) record(s CallStat) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s.OK {
		c.okCount++
	} else {
		c.failCount++
	}
	c.calls = append(c.calls, s)
	if len(c.calls) > keptCalls {
		c.calls = c.calls[len(c.calls)-keptCalls:]
	}
}

// RecentCalls returns the last requests, newest first, and the totals since
// the process started.
func (c *Client) RecentCalls() (calls []CallStat, ok, failed int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.calls) - 1; i >= 0; i-- {
		calls = append(calls, c.calls[i])
	}
	return calls, c.okCount, c.failCount
}

// SetTimeout bounds one attempt (default DefaultPerAttemptTimeout). A model
// that reasons before answering needs longer than a fast chat model.
func (c *Client) SetTimeout(d time.Duration) {
	if d > 0 {
		c.timeout = d
	}
}

// SetMaxTokens caps the reply (default DefaultMaxTokens).
func (c *Client) SetMaxTokens(n int) {
	if n > 0 {
		c.maxTokens = n
	}
}

// SetReasoningEffort asks the host to think before answering ("", "none",
// "low", "medium", "high"). The request then carries no temperature:
// extended thinking accepts none but the default.
func (c *Client) SetReasoningEffort(e string) { c.reasoningEffort = strings.TrimSpace(e) }

// ReasoningEffort is the configured effort, for the engine's status.
func (c *Client) ReasoningEffort() string { return c.reasoningEffort }

// WithOverrides is a client like c — same key, endpoint, HTTP client and
// tunables — with another model or reasoning effort ("" keeps c's), and
// stats and a breaker of its own. The shadow evaluation uses it to compare
// engines without disturbing the one that makes suggestions.
func (c *Client) WithOverrides(model, reasoningEffort string) *Client {
	n := NewClient(c.apiKey, c.model, c.endpoint, c.httpClient)
	if m := strings.TrimSpace(model); m != "" {
		n.model = m
	}
	n.log, n.backoffFunc = c.log, c.backoffFunc
	n.timeout, n.maxTokens, n.reasoningEffort = c.timeout, c.maxTokens, c.reasoningEffort
	c.mu.Lock()
	n.noTemperature = c.noTemperature && n.model == c.model // what this model refuses, if it's the same model
	c.mu.Unlock()
	if e := strings.TrimSpace(reasoningEffort); e != "" {
		n.reasoningEffort = e
	}
	return n
}

// NewClient returns a configured client. apiKey is required; model and
// endpoint fall back to DefaultModel / DefaultEndpoint. httpClient
// defaults to a zero-timeout client because we set per-attempt
// deadlines via context — a global http.Client timeout would
// short-circuit our retry policy.
func NewClient(apiKey, model, endpoint string, httpClient *http.Client) *Client {
	if model == "" {
		model = DefaultModel
	}
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	return &Client{
		apiKey:      apiKey,
		model:       model,
		endpoint:    strings.TrimRight(endpoint, "/"),
		httpClient:  httpClient,
		backoffFunc: backoffDuration,
	}
}

// SetLogger wires an observable logger for retries / token usage /
// errors. Optional — tests leave it nil; production attaches the
// classifier's logger.
func (c *Client) SetLogger(l *slog.Logger) { c.log = l }

// Model returns the configured model name. Used in startup log lines.
func (c *Client) Model() string { return c.model }

// Endpoint returns the configured base URL. Used in startup log lines.
func (c *Client) Endpoint() string { return c.endpoint }

// GenerateJSON sends a system + user prompt and asks the model to
// reply with a JSON object. The returned string is the raw JSON for
// the caller to unmarshal into its own structure (we don't ship
// API-specific types past this boundary).
//
// Retries automatically on 429/5xx/network errors with exponential
// backoff. Returns the original typed error on terminal failure so
// the caller can branch on it.
func (c *Client) GenerateJSON(ctx context.Context, systemPrompt, userPrompt string) (_ string, err error) {
	if h := c.Health(); !h.Available {
		return "", fmt.Errorf("%w until %s: %s", ErrUnavailable, h.RestUntil.Format(time.RFC3339), h.Reason)
	}
	defer func() { c.settle(err) }()
	if c.apiKey == "" {
		return "", errors.New("llm: api key is empty")
	}
	if userPrompt == "" {
		return "", errors.New("llm: user prompt is empty")
	}

	maxTokens := DefaultMaxTokens
	if c.maxTokens > 0 {
		maxTokens = c.maxTokens
	}
	body := chatCompletionRequest{
		Model:          c.model,
		MaxTokens:      maxTokens,
		ResponseFormat: &responseFormat{Type: "json_object"},
	}
	c.mu.Lock()
	noTemp := c.noTemperature
	c.mu.Unlock()
	if e := c.reasoningEffort; e != "" && e != "none" {
		body.ReasoningEffort = e
	} else if !noTemp {
		zero := 0.0
		body.Temperature = &zero // deterministic when not reasoning
	}
	if systemPrompt != "" {
		body.Messages = append(body.Messages, chatMessage{Role: "system", Content: systemPrompt})
	}
	body.Messages = append(body.Messages, chatMessage{Role: "user", Content: userPrompt})

	buf, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("llm: marshal request: %w", err)
	}

	url := c.endpoint + "/chat/completions"
	var lastErr error
	for attempt := 0; attempt <= MaxRetries; attempt++ {
		if attempt > 0 {
			backoff := c.backoffFunc(attempt, lastErr)
			if c.log != nil {
				c.log.Warn("llm: retrying after transient error",
					"attempt", attempt,
					"backoff_ms", backoff.Milliseconds(),
					"last_err", lastErr,
				)
			}
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(backoff):
			}
		}
		out, err := c.doOnce(ctx, url, buf)
		if err == nil {
			return out, nil
		}
		lastErr = err
		// A host that refuses the temperature gets the same request
		// without one, now and from now on.
		if body.Temperature != nil && refusesTemperature(err) {
			c.mu.Lock()
			c.noTemperature = true
			c.mu.Unlock()
			if c.log != nil {
				c.log.Warn("llm: the model refuses a temperature; sending none from now on", "model", c.model)
			}
			body.Temperature = nil
			if buf, err = json.Marshal(body); err != nil {
				return "", fmt.Errorf("llm: marshal request: %w", err)
			}
			attempt-- // not a retry of a transient failure: the first real try
			continue
		}
		if !isRetryable(err) {
			return "", err
		}
		// A long Retry-After (a subscription at its limit) is not waited
		// out inside the call: the client rests, and the caller moves on.
		var apiErr *Error
		if errors.As(err, &apiErr) && apiErr.RetryAfter > maxInlineWait {
			c.rest(apiErr.RetryAfter, fmt.Sprintf("the gateway asked for a %s pause (HTTP %d)", apiErr.RetryAfter.Round(time.Second), apiErr.Status))
			return "", err
		}
	}
	return "", fmt.Errorf("llm: exhausted %d retries: %w", MaxRetries, lastErr)
}

// doOnce performs a single chat-completion request. Per-attempt
// timeout is enforced via a derived context so each retry gets a
// fresh deadline.
func (c *Client) doOnce(ctx context.Context, url string, buf []byte) (string, error) {
	timeout := DefaultPerAttemptTimeout
	if c.timeout > 0 {
		timeout = c.timeout
	}
	attemptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return "", fmt.Errorf("llm: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	start := time.Now()
	stat := CallStat{At: start}
	defer func() {
		if stat.DurationMS == 0 {
			stat.DurationMS = time.Since(start).Milliseconds()
		}
		c.record(stat)
	}()
	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Wrap to make the retry policy able to distinguish transport
		// errors from API errors via errors.As.
		stat.Error = truncate(err.Error(), 160)
		return "", &transportError{err: err}
	}
	defer resp.Body.Close()
	stat.Status = resp.StatusCode

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		stat.Error = "read body: " + truncate(err.Error(), 140)
		return "", &transportError{err: fmt.Errorf("read body: %w", err)}
	}
	duration := time.Since(start)
	stat.DurationMS = duration.Milliseconds()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		stat.Error = truncate(string(raw), 160)
		return "", &Error{
			Status:     resp.StatusCode,
			Body:       string(raw),
			RetryAfter: parseRetryAfter(resp.Header),
		}
	}

	var parsed chatCompletionResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		stat.Error = "decode response"
		return "", fmt.Errorf("llm: decode response: %w (body preview: %s)", err, truncate(string(raw), 256))
	}
	stat.PromptTokens, stat.CompletionTokens = parsed.Usage.PromptTokens, parsed.Usage.CompletionTokens
	if len(parsed.Choices) == 0 || parsed.Choices[0].Message.Content == "" {
		stat.Error = "empty response choice"
		return "", errors.New("llm: empty response choice")
	}
	// finish_reason=length means the model was cut off mid-output.
	// Treat as an explicit error: a truncated JSON object will fail
	// downstream parsing anyway, and surfacing it here gives a clearer
	// log message for the operator.
	if fr := parsed.Choices[0].FinishReason; fr == "length" {
		stat.Error = "truncated (finish_reason=length)"
		return "", fmt.Errorf("llm: response truncated (finish_reason=length); raise DefaultMaxTokens or trim prompt")
	}
	stat.OK = true
	if c.log != nil {
		u := parsed.Usage
		c.log.Debug("llm: ok",
			"model", c.model,
			"duration_ms", duration.Milliseconds(),
			"prompt_tokens", u.PromptTokens,
			"completion_tokens", u.CompletionTokens,
			"total_tokens", u.TotalTokens,
			"finish_reason", parsed.Choices[0].FinishReason,
		)
	}
	return parsed.Choices[0].Message.Content, nil
}

// Error is returned for any non-2xx response. RetryAfter is set when
// the server included a Retry-After header (RFC 7231 §7.1.3); the
// retry loop honours it.
type Error struct {
	Status     int
	Body       string
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	preview := e.Body
	if len(preview) > 256 {
		preview = preview[:256] + "…"
	}
	return fmt.Sprintf("llm: HTTP %d: %s", e.Status, preview)
}

// transportError marks net-stack failures (dial, TLS, connection
// reset, EOF mid-body). Retryable by default — these are usually
// transient infrastructure issues that resolve on the next try.
type transportError struct{ err error }

func (e *transportError) Error() string { return "llm transport: " + e.err.Error() }
func (e *transportError) Unwrap() error { return e.err }

// refusesTemperature: a 400 that complains about the temperature.
func refusesTemperature(err error) bool {
	var apiErr *Error
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusBadRequest && strings.Contains(strings.ToLower(apiErr.Body), "temperature")
}

// isRetryable reports whether err is worth a second try. Network
// errors and a small set of HTTP status codes qualify; everything
// else is terminal so we surface it immediately.
//
// Explicitly terminal codes worth flagging:
//   - 401: bad API key
//   - 402: insufficient balance (DeepSeek; OpenAI uses 429 with a
//     specific code, which we still treat as retryable but the body
//     contains the diagnostic the operator needs)
//   - 404: bad model name
//   - 422: schema rejection
//
// All of these require operator action; retrying just burns budget
// while logging the same error.
func isRetryable(err error) bool {
	var apiErr *Error
	if errors.As(err, &apiErr) {
		switch apiErr.Status {
		case http.StatusTooManyRequests, // 429
			http.StatusInternalServerError, // 500
			http.StatusBadGateway,          // 502
			http.StatusServiceUnavailable,  // 503
			http.StatusGatewayTimeout,      // 504
			529:                            // Anthropic: overloaded
			return true
		}
		return false
	}
	var transp *transportError
	return errors.As(err, &transp)
}

// backoffDuration is the wait before attempt N (1-indexed). Honours
// the server's Retry-After when present, otherwise exponential 1s/3s/9s
// with ±25% jitter, capped at 30s. The jitter avoids thundering-herd
// against shared rate-limit windows when multiple callers hit a 429
// simultaneously.
func backoffDuration(attempt int, lastErr error) time.Duration {
	var apiErr *Error
	if errors.As(lastErr, &apiErr) && apiErr.RetryAfter > 0 {
		return apiErr.RetryAfter
	}
	base := time.Second
	mult := int64(1)
	for i := 1; i < attempt; i++ {
		mult *= 3
	}
	d := base * time.Duration(mult)
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	// ±25% jitter. rand.Int64N panics on n<=0 so guard the call.
	if half := int64(d / 2); half > 0 {
		d += time.Duration(rand.Int64N(half)) - d/4
	}
	return d
}

// parseRetryAfter handles both delta-seconds and HTTP-date formats.
// Returns 0 when absent or unparseable so the caller falls back to
// the exponential schedule.
func parseRetryAfter(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// DisplayName is a model id the way a person would say it:
// "claude-opus-5-5" → "Claude Opus 5.5", "deepseek-v4-flash" → "DeepSeek V4
// Flash". Trailing version numbers join with dots.
func DisplayName(model string) string {
	parts := strings.FieldsFunc(strings.TrimSpace(model), func(r rune) bool { return r == '-' || r == '_' || r == ' ' })
	if len(parts) == 0 {
		return ""
	}
	known := map[string]string{"claude": "Claude", "deepseek": "DeepSeek", "gpt": "GPT", "gemini": "Gemini", "llama": "Llama"}
	var words, version []string
	for i, p := range parts {
		if isDigits(p) && (len(version) > 0 || i > 0) {
			version = append(version, p)
			continue
		}
		if len(version) > 0 { // a word after the version: keep the version where it was
			words = append(words, strings.Join(version, "."))
			version = nil
		}
		if k, ok := known[strings.ToLower(p)]; ok {
			words = append(words, k)
		} else {
			words = append(words, strings.ToUpper(p[:1])+p[1:])
		}
	}
	if len(version) > 0 {
		words = append(words, strings.Join(version, "."))
	}
	return strings.Join(words, " ")
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
