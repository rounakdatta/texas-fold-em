package llm

// websearch.go — a question answered with a web search.
//
// Some answers aren't in any model: which branches a neighbourhood tiffin
// place has, or whether a name someone half-remembers exists at all.
// Anthropic's Messages API lets the model search the web mid-answer (a tool
// the server runs; auth2api passes the request through, as it does Claude
// Code's own searches), so for those few questions fold asks that way. It is
// slower than a plain question — a search is seconds on its own — so callers
// ask it seldom and keep what it finds.

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

// WebSearchTool is the version of Anthropic's server-side search tool.
const WebSearchTool = "web_search_20250305"

// ErrNoWebSearch: the host refused the search tool. Asking again won't help;
// the caller stops asking for a while.
var ErrNoWebSearch = errors.New("llm: the host does not offer web search")

// SearchAnswer is a searched question's answer.
type SearchAnswer struct {
	Text     string // the model's answer, after its searches
	Searches int    // how many searches it ran
}

type messagesRequest struct {
	Model     string            `json:"model"`
	MaxTokens int               `json:"max_tokens"`
	System    string            `json:"system,omitempty"`
	Messages  []messagesMessage `json:"messages"`
	Tools     []messagesTool    `json:"tools"`
	Thinking  *messagesThinking `json:"thinking,omitempty"`
}

type messagesMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type messagesTool struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	MaxUses int    `json:"max_uses,omitempty"`
}

type messagesThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

type messagesResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Usage      struct {
		InputTokens   int `json:"input_tokens"`
		OutputTokens  int `json:"output_tokens"`
		ServerToolUse struct {
			WebSearchRequests int `json:"web_search_requests"`
		} `json:"server_tool_use"`
	} `json:"usage"`
}

// searchReplyTokens bounds the answer itself; a thinking budget, when the
// model insists on one, comes on top.
const searchReplyTokens = 1024

// SearchJSON asks a question the model may search the web to answer, at most
// maxSearches times, and returns its final answer: the text after its last
// search, from which a caller that asked for JSON takes the object. Thinking
// is turned off unless the model refuses (then it gets the fallback effort,
// as in GenerateJSON); a transient failure is tried once more. The breaker
// and the call log are the client's own.
func (c *Client) SearchJSON(ctx context.Context, systemPrompt, userPrompt string, maxSearches int) (_ SearchAnswer, err error) {
	if h := c.Health(); !h.Available {
		return SearchAnswer{}, fmt.Errorf("%w until %s: %s", ErrUnavailable, h.RestUntil.Format(time.RFC3339), h.Reason)
	}
	defer func() {
		if !errors.Is(err, ErrNoWebSearch) {
			c.settle(err)
		}
	}()
	if c.apiKey == "" {
		return SearchAnswer{}, errors.New("llm: api key is empty")
	}
	if maxSearches < 1 {
		maxSearches = 1
	}
	body := messagesRequest{
		Model:     c.model,
		MaxTokens: searchReplyTokens,
		System:    systemPrompt,
		Messages:  []messagesMessage{{Role: "user", Content: userPrompt}},
		Tools:     []messagesTool{{Type: WebSearchTool, Name: "web_search", MaxUses: maxSearches}},
	}
	c.mu.Lock()
	mustThink := c.thinkingRequired
	c.mu.Unlock()
	if mustThink {
		body.Thinking = thinkingFor(c.thinkingFallback)
	} else {
		body.Thinking = &messagesThinking{Type: "disabled"}
	}
	if body.Thinking != nil && body.Thinking.Type == "enabled" {
		body.MaxTokens += body.Thinking.BudgetTokens
	}

	for attempt := 0; attempt <= 1; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return SearchAnswer{}, ctx.Err()
			case <-time.After(c.backoffFunc(attempt, err)):
			}
		}
		var ans SearchAnswer
		ans, err = c.searchOnce(ctx, body)
		if err == nil {
			return ans, nil
		}
		if body.Thinking != nil && body.Thinking.Type == "disabled" && refusesNoThinking(err) {
			c.mu.Lock()
			c.thinkingRequired = true
			c.mu.Unlock()
			body.Thinking, body.MaxTokens = thinkingFor(c.thinkingFallback), searchReplyTokens
			if body.Thinking != nil {
				body.MaxTokens += body.Thinking.BudgetTokens
			}
			attempt-- // the first real try
			continue
		}
		if refusesWebSearch(err) {
			return SearchAnswer{}, fmt.Errorf("%w: %v", ErrNoWebSearch, err)
		}
		if !isRetryable(err) {
			return SearchAnswer{}, err
		}
	}
	return SearchAnswer{}, err
}

// thinkingFor is the Messages API's thinking for an effort; nil leaves it to
// the host. (The API's smallest budget is 1024.)
func thinkingFor(effort string) *messagesThinking {
	switch effort {
	case "minimal", "low":
		return &messagesThinking{Type: "enabled", BudgetTokens: 1024}
	case "medium":
		return &messagesThinking{Type: "enabled", BudgetTokens: 8192}
	case "high":
		return &messagesThinking{Type: "enabled", BudgetTokens: 24576}
	}
	return nil
}

func (c *Client) searchOnce(ctx context.Context, body messagesRequest) (SearchAnswer, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return SearchAnswer{}, fmt.Errorf("llm: marshal request: %w", err)
	}
	timeout := DefaultPerAttemptTimeout
	if c.timeout > 0 {
		timeout = c.timeout
	}
	attemptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, c.endpoint+"/messages", bytes.NewReader(buf))
	if err != nil {
		return SearchAnswer{}, fmt.Errorf("llm: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")

	start := time.Now()
	stat := CallStat{At: start}
	defer func() {
		stat.DurationMS = time.Since(start).Milliseconds()
		c.record(stat)
	}()
	resp, err := c.httpClient.Do(req)
	if err != nil {
		stat.Error = truncate(err.Error(), 160)
		return SearchAnswer{}, &transportError{err: err}
	}
	defer resp.Body.Close()
	stat.Status = resp.StatusCode
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20)) // search results ride along
	if err != nil {
		stat.Error = "read body: " + truncate(err.Error(), 140)
		return SearchAnswer{}, &transportError{err: fmt.Errorf("read body: %w", err)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		stat.Error = truncate(string(raw), 160)
		return SearchAnswer{}, &Error{Status: resp.StatusCode, Body: string(raw), RetryAfter: parseRetryAfter(resp.Header)}
	}
	var parsed messagesResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		stat.Error = "decode response"
		return SearchAnswer{}, fmt.Errorf("llm: decode response: %w (body preview: %s)", err, truncate(string(raw), 256))
	}
	stat.PromptTokens, stat.CompletionTokens = parsed.Usage.InputTokens, parsed.Usage.OutputTokens
	if parsed.StopReason == "max_tokens" {
		stat.Error = "truncated (max_tokens)"
		return SearchAnswer{}, errors.New("llm: searched answer truncated (max_tokens)")
	}
	// The answer is the text after the last search: before it, the model
	// says what it is about to look for. (A cited answer arrives as several
	// text blocks, one per citation, so they are joined.)
	from := 0
	for i, b := range parsed.Content {
		if b.Type != "text" && b.Type != "thinking" && b.Type != "redacted_thinking" {
			from = i + 1
		}
	}
	var text strings.Builder
	for _, b := range parsed.Content[from:] {
		if b.Type == "text" {
			text.WriteString(b.Text)
		}
	}
	if strings.TrimSpace(text.String()) == "" {
		stat.Error = "empty answer"
		return SearchAnswer{}, errors.New("llm: empty searched answer")
	}
	stat.OK = true
	return SearchAnswer{Text: text.String(), Searches: parsed.Usage.ServerToolUse.WebSearchRequests}, nil
}

// refusesWebSearch: a 400 that names the search tool (an unknown tool, or a
// host that doesn't run it).
func refusesWebSearch(err error) bool {
	var apiErr *Error
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusBadRequest &&
		strings.Contains(strings.ToLower(apiErr.Body), "web_search")
}
