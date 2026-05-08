// Package gemini is a tiny typed client over Google AI Studio's
// generativelanguage REST API. Used by the integration's Tier-3
// classifier (LLM RAG) to reason about staged fold transactions
// against retrieved firefly history.
//
// We deliberately avoid the official google-genai SDK: it pulls in a
// large dependency tree for what amounts to one HTTP POST per
// classification. A 200-line bespoke client we own is easier to audit
// and lets us pin behaviour against a single API version.
package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultModel is what the classifier uses on a fresh deploy.
// gemini-3.1-flash-lite is the most cost-efficient model in the
// gemini-3 family at the time of writing; classification accuracy on
// our prompt is ample.
const DefaultModel = "gemini-3.1-flash-lite"

// DefaultTimeout bounds a single Generate call.
const DefaultTimeout = 30 * time.Second

// DefaultEndpoint is the v1beta REST endpoint host. Override only for
// tests via NewClient's httpClient base or a fake server.
const DefaultEndpoint = "https://generativelanguage.googleapis.com/v1beta"

// Client wraps an API key + HTTP client. Safe for concurrent use.
type Client struct {
	apiKey     string
	model      string
	endpoint   string
	httpClient *http.Client
}

// NewClient returns a configured client. apiKey is required; model
// defaults to DefaultModel; httpClient defaults to a 30-second-timeout
// client. endpoint is optional (defaults to DefaultEndpoint) — exposed
// so tests can point at httptest.Server.
func NewClient(apiKey, model, endpoint string, httpClient *http.Client) *Client {
	if model == "" {
		model = DefaultModel
	}
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: DefaultTimeout}
	}
	return &Client{
		apiKey:     apiKey,
		model:      model,
		endpoint:   strings.TrimRight(endpoint, "/"),
		httpClient: httpClient,
	}
}

// GenerateJSON sends a system instruction + user prompt and asks the
// model to reply with JSON. Returns the raw JSON string for the caller
// to unmarshal into its own structure (we don't ship Gemini-specific
// types past this boundary).
//
// Temperature defaults to 0.1 — we want consistent classification, not
// creative writing.
func (c *Client) GenerateJSON(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	if c.apiKey == "" {
		return "", errors.New("gemini: api key is empty")
	}
	if userPrompt == "" {
		return "", errors.New("gemini: user prompt is empty")
	}

	body := request{
		Contents: []contentPart{{
			Parts: []part{{Text: userPrompt}},
		}},
		GenerationConfig: generationConfig{
			Temperature:      0.1,
			MaxOutputTokens:  1024,
			ResponseMimeType: "application/json",
		},
	}
	if systemPrompt != "" {
		body.SystemInstruction = &contentPart{
			Parts: []part{{Text: systemPrompt}},
		}
	}

	buf, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("gemini: marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/models/%s:generateContent?key=%s", c.endpoint, c.model, c.apiKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return "", fmt.Errorf("gemini: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("gemini: post: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("gemini: read body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", &Error{Status: resp.StatusCode, Body: string(respBody)}
	}

	var parsed response
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("gemini: decode envelope: %w", err)
	}
	if len(parsed.Candidates) == 0 || len(parsed.Candidates[0].Content.Parts) == 0 {
		return "", errors.New("gemini: no candidates in response")
	}
	text := parsed.Candidates[0].Content.Parts[0].Text
	if text == "" {
		return "", errors.New("gemini: empty candidate text")
	}
	return text, nil
}

// Error is returned for any non-2xx response.
type Error struct {
	Status int
	Body   string
}

func (e *Error) Error() string {
	preview := e.Body
	if len(preview) > 256 {
		preview = preview[:256] + "…"
	}
	return fmt.Sprintf("gemini: HTTP %d: %s", e.Status, preview)
}

// Model returns the configured model name. Useful for log lines.
func (c *Client) Model() string { return c.model }
