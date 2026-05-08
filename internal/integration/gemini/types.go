package gemini

// Wire types for Google AI Studio's generativelanguage REST API.
// Tight subset — we only use what we send/receive.
//
// Docs: https://ai.google.dev/api/generate-content

type request struct {
	Contents          []contentPart    `json:"contents"`
	SystemInstruction *contentPart     `json:"systemInstruction,omitempty"`
	GenerationConfig  generationConfig `json:"generationConfig"`
}

type contentPart struct {
	Parts []part  `json:"parts"`
	Role  string  `json:"role,omitempty"`
}

type part struct {
	Text string `json:"text"`
}

type generationConfig struct {
	Temperature      float64 `json:"temperature,omitempty"`
	MaxOutputTokens  int     `json:"maxOutputTokens,omitempty"`
	ResponseMimeType string  `json:"responseMimeType,omitempty"`
	// We deliberately don't use responseSchema — string-typed JSON
	// + our own validation is more flexible than wiring a JSON
	// Schema through the chart-pinned google-genai-go SDK.
}

type response struct {
	Candidates    []candidate    `json:"candidates"`
	UsageMetadata *usageMetadata `json:"usageMetadata,omitempty"`
}

type candidate struct {
	Content      contentPart `json:"content"`
	FinishReason string      `json:"finishReason,omitempty"`
}

type usageMetadata struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	TotalTokenCount      int `json:"totalTokenCount"`
}
