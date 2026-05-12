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
func (c *Client) GenerateJSON(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	if c.apiKey == "" {
		return "", errors.New("llm: api key is empty")
	}
	if userPrompt == "" {
		return "", errors.New("llm: user prompt is empty")
	}

	body := chatCompletionRequest{
		Model:          c.model,
		Temperature:    0,
		MaxTokens:      DefaultMaxTokens,
		ResponseFormat: &responseFormat{Type: "json_object"},
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
		if !isRetryable(err) {
			return "", err
		}
	}
	return "", fmt.Errorf("llm: exhausted %d retries: %w", MaxRetries, lastErr)
}

// doOnce performs a single chat-completion request. Per-attempt
// timeout is enforced via a derived context so each retry gets a
// fresh deadline.
func (c *Client) doOnce(ctx context.Context, url string, buf []byte) (string, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, DefaultPerAttemptTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return "", fmt.Errorf("llm: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	start := time.Now()
	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Wrap to make the retry policy able to distinguish transport
		// errors from API errors via errors.As.
		return "", &transportError{err: err}
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", &transportError{err: fmt.Errorf("read body: %w", err)}
	}
	duration := time.Since(start)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", &Error{
			Status:     resp.StatusCode,
			Body:       string(raw),
			RetryAfter: parseRetryAfter(resp.Header),
		}
	}

	var parsed chatCompletionResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("llm: decode response: %w (body preview: %s)", err, truncate(string(raw), 256))
	}
	if len(parsed.Choices) == 0 || parsed.Choices[0].Message.Content == "" {
		return "", errors.New("llm: empty response choice")
	}
	// finish_reason=length means the model was cut off mid-output.
	// Treat as an explicit error: a truncated JSON object will fail
	// downstream parsing anyway, and surfacing it here gives a clearer
	// log message for the operator.
	if fr := parsed.Choices[0].FinishReason; fr == "length" {
		return "", fmt.Errorf("llm: response truncated (finish_reason=length); raise DefaultMaxTokens or trim prompt")
	}
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
			http.StatusGatewayTimeout:      // 504
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
