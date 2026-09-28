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
)

// A searched answer comes back as blocks: what the model says it will look
// for, the search, the results, then the answer, cited sentence by sentence.
// The answer is the text after the last search, joined.
func TestASearchedAnswerIsTheTextAfterTheLastSearch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/messages" {
			t.Errorf("path = %q, want /messages", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer k" {
			t.Errorf("Authorization = %q", got)
		}
		if r.Header.Get("anthropic-version") == "" {
			t.Error("no anthropic-version header")
		}
		var body messagesRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(body.Tools) != 1 || body.Tools[0].Type != WebSearchTool || body.Tools[0].Name != "web_search" || body.Tools[0].MaxUses != 2 {
			t.Errorf("tools = %+v", body.Tools)
		}
		if body.Thinking == nil || body.Thinking.Type != "disabled" {
			t.Errorf("thinking = %+v, want disabled", body.Thinking)
		}
		if body.System != "sys" || len(body.Messages) != 1 || body.Messages[0].Content != "where is it?" {
			t.Errorf("prompt = %q / %+v", body.System, body.Messages)
		}
		_, _ = w.Write([]byte(`{"content":[
			{"type":"text","text":"Let me look that up."},
			{"type":"server_tool_use","id":"s1","name":"web_search","input":{"query":"x"}},
			{"type":"web_search_tool_result","tool_use_id":"s1","content":[]},
			{"type":"text","text":"{\"found\":true,"},
			{"type":"text","text":"\"name\":\"Sunrise Tiffins\"}","citations":[{"type":"web_search_result_location"}]}],
			"stop_reason":"end_turn","usage":{"input_tokens":900,"output_tokens":40,"server_tool_use":{"web_search_requests":1}}}`))
	}))
	defer srv.Close()
	c := NewClient("k", "m", srv.URL, srv.Client())
	ans, err := c.SearchJSON(context.Background(), "sys", "where is it?", 2)
	if err != nil {
		t.Fatal(err)
	}
	if ans.Text != `{"found":true,"name":"Sunrise Tiffins"}` || ans.Searches != 1 {
		t.Errorf("answer = %q, %d searches", ans.Text, ans.Searches)
	}
	calls, ok, _ := c.RecentCalls()
	if ok != 1 || calls[0].PromptTokens != 900 {
		t.Errorf("the call wasn't logged: %+v", calls)
	}
}

// A model that won't have its thinking turned off searches with the least
// thinking there is — and is asked that way from then on.
func TestAModelThatMustThinkSearchesWithTheLeastThinking(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body messagesRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		if n.Add(1) == 1 {
			if body.Thinking == nil || body.Thinking.Type != "disabled" {
				t.Errorf("first ask: thinking = %+v", body.Thinking)
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"thinking.type.disabled is not supported for this model"}}`))
			return
		}
		if body.Thinking == nil || body.Thinking.Type != "enabled" || body.Thinking.BudgetTokens != 1024 || body.MaxTokens != searchReplyTokens+1024 {
			t.Errorf("ask %d: thinking = %+v, max_tokens %d", n.Load(), body.Thinking, body.MaxTokens)
		}
		_, _ = w.Write([]byte(`{"content":[{"type":"thinking","thinking":"…"},{"type":"text","text":"{}"}],"stop_reason":"end_turn"}`))
	}))
	defer srv.Close()
	c := NewClient("k", "m", srv.URL, srv.Client())
	c.backoffFunc = instantBackoff
	c.SetThinkingFallback("low")
	for i := range 2 {
		if ans, err := c.SearchJSON(context.Background(), "", "q", 1); err != nil || ans.Text != "{}" {
			t.Fatalf("call %d: %q, %v", i, ans.Text, err)
		}
	}
	if n.Load() != 3 {
		t.Errorf("%d requests, want 3 (one refusal, then straight to thinking)", n.Load())
	}
}

// A host that doesn't run the search tool says so once; the caller is told
// plainly, and the client isn't sent to rest for it.
func TestAHostWithoutSearchSaysSo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"tools.0: unknown tool type web_search_20250305"}}`))
	}))
	defer srv.Close()
	c := NewClient("k", "m", srv.URL, srv.Client())
	c.backoffFunc = instantBackoff
	for range 4 {
		if _, err := c.SearchJSON(context.Background(), "", "q", 1); !errors.Is(err, ErrNoWebSearch) {
			t.Fatalf("err = %v, want ErrNoWebSearch", err)
		}
	}
	if !c.Health().Available {
		t.Error("a host without search sent the client to rest")
	}
}

// A cut-off answer is an error, not half a JSON object.
func TestATruncatedSearchedAnswerIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"{\"found\":tr"}],"stop_reason":"max_tokens"}`))
	}))
	defer srv.Close()
	c := NewClient("k", "m", srv.URL, srv.Client())
	if _, err := c.SearchJSON(context.Background(), "", "q", 1); err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Errorf("err = %v", err)
	}
}

// In a plain question too, a model that must think is asked for the least
// thinking when a fallback is set — with no temperature, which thinking
// refuses — and straight away from then on.
func TestAModelThatMustThinkAnswersWithTheFallbackEffort(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body chatCompletionRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		if n.Add(1) == 1 {
			if body.ReasoningEffort != "none" {
				t.Errorf("first ask: effort %q", body.ReasoningEffort)
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"thinking.type.disabled is not supported for this model"}}`))
			return
		}
		if body.ReasoningEffort != "low" || body.Temperature != nil {
			t.Errorf("ask %d: effort %q, temperature %v", n.Load(), body.ReasoningEffort, body.Temperature)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{}"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()
	c := NewClient("k", "m", srv.URL, srv.Client())
	c.backoffFunc = instantBackoff
	c.SetReasoningEffort("none")
	c.SetThinkingFallback("low")
	for i := range 2 {
		if _, err := c.GenerateJSON(context.Background(), "s", "u"); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if n.Load() != 3 {
		t.Errorf("%d requests, want 3", n.Load())
	}
	if noTemp, mustThink := c.Adapted(); !mustThink || noTemp {
		t.Errorf("adapted = noTemperature %v, thinkingRequired %v", noTemp, mustThink)
	}
	// a client made from it for the same model knows already
	if _, mustThink := c.WithOverrides("", "").Adapted(); !mustThink {
		t.Error("the override for the same model forgot the refusal")
	}
}
