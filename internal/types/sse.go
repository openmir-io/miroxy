package types

// SSEEvent is a single Anthropic streaming event ready for wire encoding.
type SSEEvent struct {
	Event string // SSE event: field value (e.g. "message_start")
	Data  any    // marshaled as the SSE data: field
}

// Anthropic streaming event sequence for a single message:
//  1. message_start       → MessageStartData
//  2. content_block_start → ContentBlockStartData  (index 0)
//  3. ping                → PingData
//  4. content_block_delta × N → ContentBlockDeltaData
//  5. content_block_stop  → ContentBlockStopData
//  6. message_delta       → MessageDeltaData       (stop_reason + output tokens)
//  7. message_stop        → MessageStopData

type MessageStartData struct {
	Type    string       `json:"type"`
	Message StartMessage `json:"message"`
}

type StartMessage struct {
	ID           string         `json:"id"`
	Type         string         `json:"type"`
	Role         string         `json:"role"`
	Content      []ContentBlock `json:"content"`
	Model        string         `json:"model"`
	StopReason   *string        `json:"stop_reason"`
	StopSequence *string        `json:"stop_sequence"`
	Usage        Usage          `json:"usage"`
}

type ContentBlockStartData struct {
	Type         string       `json:"type"`
	Index        int          `json:"index"`
	ContentBlock ContentBlock `json:"content_block"`
}

type PingData struct {
	Type string `json:"type"`
}

type ContentBlockDeltaData struct {
	Type  string    `json:"type"`
	Index int       `json:"index"`
	Delta TextDelta `json:"delta"`
}

type TextDelta struct {
	Type        string `json:"type"` // "text_delta" | "input_json_delta"
	Text        string `json:"text,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
}

type ContentBlockStopData struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
}

type MessageDeltaData struct {
	Type  string       `json:"type"`
	Delta MessageDelta `json:"delta"`
	Usage DeltaUsage   `json:"usage"`
}

type MessageDelta struct {
	StopReason   string  `json:"stop_reason"`
	StopSequence *string `json:"stop_sequence"`
}

type DeltaUsage struct {
	OutputTokens int `json:"output_tokens"`
	InputTokens  int `json:"input_tokens,omitempty"`
}

type MessageStopData struct {
	Type string `json:"type"`
}
