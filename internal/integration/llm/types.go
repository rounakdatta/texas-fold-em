package llm

// Wire types for the OpenAI-compatible Chat Completions API. Compatible
// with DeepSeek, OpenAI, Groq, and any other host that speaks the same
// protocol — tight subset, only what we send/receive.
//
// Docs: https://api-docs.deepseek.com/api/create-chat-completion

type chatCompletionRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	// Temperature is a pointer so it can be left out: Anthropic models
	// behind an OpenAI-shaped gateway reject any temperature but 1 while
	// extended thinking is on, so a reasoning request sends none.
	Temperature *float64 `json:"temperature,omitempty"`
	MaxTokens   int      `json:"max_tokens"`
	// ReasoningEffort asks the host to think before answering ("low",
	// "medium", "high" …). auth2api maps it to an Anthropic thinking
	// budget; hosts that don't know the field ignore it.
	ReasoningEffort string          `json:"reasoning_effort,omitempty"`
	ResponseFormat  *responseFormat `json:"response_format,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type responseFormat struct {
	Type string `json:"type"` // "json_object"
}

type chatCompletionResponse struct {
	ID      string       `json:"id"`
	Choices []chatChoice `json:"choices"`
	Usage   chatUsage    `json:"usage"`
}

type chatChoice struct {
	Index        int         `json:"index"`
	Message      chatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}
