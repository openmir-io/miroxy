package types

import "encoding/json"

// GeminiRequest is the Google AI Studio generateContent request body.
type GeminiRequest struct {
	Contents          []GeminiContent    `json:"contents"`
	SystemInstruction *GeminiSysInstruct `json:"systemInstruction,omitempty"`
	GenerationConfig  *GenerationConfig  `json:"generationConfig,omitempty"`
	Tools             []GeminiTools      `json:"tools,omitempty"`
	ToolConfig        *GeminiToolConfig  `json:"toolConfig,omitempty"`
}

type GeminiContent struct {
	Role  string       `json:"role"`
	Parts []GeminiPart `json:"parts"`
}

// GeminiSysInstruct is the top-level systemInstruction field.
type GeminiSysInstruct struct {
	Parts []GeminiPart `json:"parts"`
}

type GeminiPart struct {
	Text             string                  `json:"text,omitempty"`
	Thought          bool                    `json:"thought,omitempty"`
	FunctionCall     *GeminiFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *GeminiFunctionResponse `json:"functionResponse,omitempty"`
	// ThoughtSignature is an opaque, encrypted blob binding this part to the
	// model's internal reasoning state. "Thinking" models attach it to
	// functionCall parts; every functionCall replayed in later turns must
	// carry it back verbatim or Gemini rejects the request. See
	// gemini_thoughtsig.go for how miroxy preserves it across the Anthropic
	// round-trip.
	ThoughtSignature string `json:"thoughtSignature,omitempty"`
}

type GenerationConfig struct {
	MaxOutputTokens int      `json:"maxOutputTokens,omitempty"`
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"topP,omitempty"`
	TopK            *int     `json:"topK,omitempty"`
	StopSequences   []string `json:"stopSequences,omitempty"`
}

// Tool-related request types

type GeminiTools struct {
	FunctionDeclarations []GeminiFunctionDeclaration `json:"functionDeclarations,omitempty"`
}

type GeminiFunctionDeclaration struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// GeminiFunctionCall appears in model response parts when Gemini wants to call a function.
type GeminiFunctionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

// GeminiFunctionResponse appears in user request parts to return a function result.
type GeminiFunctionResponse struct {
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

type GeminiToolConfig struct {
	FunctionCallingConfig GeminiFunctionCallingConfig `json:"functionCallingConfig"`
}

type GeminiFunctionCallingConfig struct {
	Mode                 string   `json:"mode"` // AUTO | ANY | NONE
	AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
}

// Response types

type GeminiResponse struct {
	Candidates     []GeminiCandidate     `json:"candidates"`
	UsageMetadata  GeminiUsageMetadata   `json:"usageMetadata"`
	PromptFeedback *GeminiPromptFeedback `json:"promptFeedback,omitempty"`
	Error          *GeminiError          `json:"error,omitempty"`
}

type GeminiCandidate struct {
	Content      GeminiContent `json:"content"`
	FinishReason string        `json:"finishReason"`
	Index        int           `json:"index"`
}

type GeminiUsageMetadata struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	TotalTokenCount      int `json:"totalTokenCount"`
}

// GeminiPromptFeedback is present when Gemini safety filters block the request.
type GeminiPromptFeedback struct {
	BlockReason string `json:"blockReason"`
}

// GeminiError is the error envelope from the Gemini API.
type GeminiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
}
