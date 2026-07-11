package types

import (
	"encoding/json"
	"fmt"
	"strings"
)

// MessageRequest is the Anthropic POST /v1/messages request body.
type MessageRequest struct {
	Model     string    `json:"model"`
	Messages  []Message `json:"messages"`
	MaxTokens int       `json:"max_tokens"`
	// System may be a plain JSON string or an array of content blocks —
	// the Anthropic API accepts both. Use SystemText() to get a flat string.
	System        json.RawMessage `json:"system,omitempty"`
	Stream        bool            `json:"stream"`
	Tools         []Tool          `json:"tools,omitempty"`
	ToolChoice    *ToolChoice     `json:"tool_choice,omitempty"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	TopK          *int            `json:"top_k,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
}

// SystemText returns the system prompt as a plain string regardless of whether
// the client sent it as a JSON string or as an array of content blocks.
func (r *MessageRequest) SystemText() string {
	if len(r.System) == 0 {
		return ""
	}
	// Form 1: plain string — "You are helpful."
	var s string
	if json.Unmarshal(r.System, &s) == nil {
		return s
	}
	// Form 2: array of content blocks — [{"type":"text","text":"..."}]
	var blocks []ContentBlock
	if json.Unmarshal(r.System, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// Message is a single conversation turn.
// Content may be a JSON string or a JSON array of ContentBlocks.
type Message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// TextContent returns the content as a plain string, if it is one.
func (m *Message) TextContent() (string, bool) {
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s, true
	}
	return "", false
}

// BlockContent returns the content as a slice of ContentBlocks, if structured.
func (m *Message) BlockContent() ([]ContentBlock, bool) {
	var blocks []ContentBlock
	if err := json.Unmarshal(m.Content, &blocks); err == nil {
		return blocks, true
	}
	return nil, false
}

// SetTextContent replaces the message content with a plain text string.
func (m *Message) SetTextContent(text string) {
	b, _ := json.Marshal(text)
	m.Content = b
}

// SetLastBlockText replaces the text of the last text-type ContentBlock in place,
// preserving all other blocks (system reminders, tool calls, etc.).
// Falls back to SetTextContent when content is a plain string.
func (m *Message) SetLastBlockText(text string) {
	blocks, ok := m.BlockContent()
	if !ok {
		m.SetTextContent(text)
		return
	}
	for i := len(blocks) - 1; i >= 0; i-- {
		if blocks[i].Type == "text" {
			blocks[i].Text = text
			b, _ := json.Marshal(blocks)
			m.Content = b
			return
		}
	}
}

// HasToolContent reports whether any content block is tool_use or tool_result.
func (m *Message) HasToolContent() bool {
	blocks, ok := m.BlockContent()
	if !ok {
		return false
	}
	for _, b := range blocks {
		if b.Type == "tool_use" || b.Type == "tool_result" {
			return true
		}
	}
	return false
}

type ContentBlock struct {
	Type string `json:"type"`
	// text block
	Text string `json:"text,omitempty"`
	// tool_use block
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result block
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
}

// Tool is a function definition in the request.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// ToolChoice controls how Gemini selects functions to call.
type ToolChoice struct {
	Type string `json:"type"` // "auto" | "any" | "tool"
	Name string `json:"name,omitempty"`
}

// MessageResponse is the Anthropic POST /v1/messages response body.
type MessageResponse struct {
	ID           string         `json:"id"`
	Type         string         `json:"type"`
	Role         string         `json:"role"`
	Content      []ContentBlock `json:"content"`
	Model        string         `json:"model"`
	StopReason   string         `json:"stop_reason"`
	StopSequence *string        `json:"stop_sequence"`
	Usage        Usage          `json:"usage"`
}

type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// ErrorResponse is the Anthropic error envelope.
type ErrorResponse struct {
	Type  string    `json:"type"`
	Error ErrorBody `json:"error"`
}

type ErrorBody struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// Model is one item in the GET /v1/models response.
// Fields satisfy both Anthropic and OpenAI wire formats so that clients
// using either protocol (including CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY)
// can parse the response without needing separate endpoints.
type Model struct {
	// Anthropic fields
	Type        string `json:"type"`
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	CreatedAt   string `json:"created_at"`
	// OpenAI compatibility fields
	Object  string `json:"object,omitempty"`  // "model"
	Created int64  `json:"created,omitempty"` // Unix timestamp
	OwnedBy string `json:"owned_by,omitempty"`
}

// ModelsResponse is the GET /v1/models response body.
// Satisfies both Anthropic and OpenAI list formats.
type ModelsResponse struct {
	Object  string  `json:"object,omitempty"` // "list" (OpenAI)
	Data    []Model `json:"data"`
	HasMore bool    `json:"has_more"`
	FirstID string  `json:"first_id,omitempty"`
	LastID  string  `json:"last_id,omitempty"`
}

// NormalizeSystem extracts any "system" role entry from the messages array
// and merges its text into the top-level System field.  Some clients (e.g.
// Claude Code with certain skills) inject system instructions as a message
// rather than using the dedicated field; this normalises both forms before
// validation so the rest of the pipeline never sees role=="system".
func (r *MessageRequest) NormalizeSystem() {
	var keep []Message
	var extra []string
	for _, m := range r.Messages {
		if m.Role != "system" {
			keep = append(keep, m)
			continue
		}
		if t, ok := m.TextContent(); ok && t != "" {
			extra = append(extra, t)
		}
	}
	if len(extra) == 0 {
		return
	}
	r.Messages = keep
	combined := strings.Join(extra, "\n")
	if len(r.System) == 0 {
		b, _ := json.Marshal(combined)
		r.System = b
	}
	// If System is already set we leave it alone — the top-level field wins.
}

// Validate returns an error if required fields are missing or invalid.
// Note: model is intentionally not required here — an empty model routes to
// server.default_model in the pipeline. This allows /miroxy commands to be
// sent without a model field and still be intercepted before reaching the LLM.
func (r *MessageRequest) Validate() error {
	if len(r.Messages) == 0 {
		return fmt.Errorf("messages must not be empty")
	}
	// max_tokens is defaulted to 1024 in the upstream adapter when absent;
	// not validated here so /miroxy commands work without it.
	for i, msg := range r.Messages {
		if msg.Role != "user" && msg.Role != "assistant" {
			return fmt.Errorf("messages[%d]: role must be \"user\" or \"assistant\", got %q", i, msg.Role)
		}
	}
	return nil
}
