package gemini

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestGenerateJSON_HappyPath asserts headers, body shape, and that the
// candidate text is returned verbatim for the caller to JSON-decode.
func TestGenerateJSON_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/models/") || !strings.Contains(r.URL.Path, ":generateContent") {
			t.Errorf("unexpected path: %q", r.URL.Path)
		}
		if r.URL.Query().Get("key") != "test-key" {
			t.Errorf("missing/wrong api key in query: %q", r.URL.Query().Get("key"))
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("missing content-type header")
		}
		body, _ := io.ReadAll(r.Body)
		var got request
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode req: %v", err)
		}
		if got.GenerationConfig.ResponseMimeType != "application/json" {
			t.Errorf("expected JSON mime type, got %q", got.GenerationConfig.ResponseMimeType)
		}
		if len(got.Contents) != 1 || got.Contents[0].Parts[0].Text == "" {
			t.Errorf("user prompt missing: %+v", got.Contents)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"candidates":[{"content":{"parts":[{"text":"{\"category\":\"Eating outside\",\"confidence\":0.9}"}]}}],
			"usageMetadata":{"promptTokenCount":42,"candidatesTokenCount":12,"totalTokenCount":54}
		}`))
	}))
	t.Cleanup(srv.Close)

	c := NewClient("test-key", "gemini-3.1-flash-lite", srv.URL, srv.Client())
	got, err := c.GenerateJSON(context.Background(), "you are a classifier", "classify this thing")
	if err != nil {
		t.Fatalf("GenerateJSON: %v", err)
	}
	if !strings.Contains(got, "Eating outside") {
		t.Errorf("expected candidate text returned verbatim, got %q", got)
	}
}

// TestGenerateJSON_HTTPError surfaces a typed *Error so the classifier
// can fall through to Tier 4 cleanly.
func TestGenerateJSON_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":403,"message":"API key not authorized"}}`))
	}))
	t.Cleanup(srv.Close)

	c := NewClient("test-key", "", srv.URL, srv.Client())
	_, err := c.GenerateJSON(context.Background(), "", "anything")
	if err == nil {
		t.Fatal("expected error for 403")
	}
	gerr, ok := err.(*Error)
	if !ok {
		t.Fatalf("expected *Error, got %T: %v", err, err)
	}
	if gerr.Status != http.StatusForbidden {
		t.Errorf("expected 403, got %d", gerr.Status)
	}
}

// TestGenerateJSON_RejectsEmptyConfig
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

// TestGenerateJSON_NoCandidates returns an explicit error rather than
// silently emitting empty string.
func TestGenerateJSON_NoCandidates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[]}`))
	}))
	t.Cleanup(srv.Close)

	c := NewClient("k", "", srv.URL, srv.Client())
	_, err := c.GenerateJSON(context.Background(), "", "x")
	if err == nil {
		t.Fatal("expected error for empty candidates")
	}
	if !strings.Contains(err.Error(), "no candidates") {
		t.Errorf("expected 'no candidates' message, got %v", err)
	}
}
