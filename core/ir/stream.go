package ir

// Stream events are the neutral, provider-agnostic representation of a streaming
// response. A ProviderConverter (IRC south side) turns its native SSE dialect into
// a channel of StreamEvent; a FrontendConverter (IRC north side) turns that channel
// back into the client's SSE dialect.

// StreamEventKind discriminates the StreamEvent union.
type StreamEventKind string

const (
	EvStreamStart       StreamEventKind = "stream_start"
	EvContentBlockStart StreamEventKind = "content_block_start"
	EvTextDelta         StreamEventKind = "text_delta"
	EvToolCallStart     StreamEventKind = "tool_call_start"
	EvToolCallDelta     StreamEventKind = "tool_call_delta"
	EvContentBlockEnd   StreamEventKind = "content_block_end"
	EvFinish            StreamEventKind = "finish"
	EvUsage             StreamEventKind = "usage"
	EvStreamEnd         StreamEventKind = "stream_end"
)

// StreamEvent is a discriminated union: Kind names the active field.
type StreamEvent struct {
	Kind StreamEventKind

	StreamStart       *StreamStart
	ContentBlockStart *ContentBlockStart
	TextDelta         *TextDelta
	ToolCallStart     *ToolCallStart
	ToolCallDelta     *ToolCallDelta
	ContentBlockEnd   *ContentBlockEnd
	Finish            *Finish
	Usage             *UsageEvent
	StreamEnd         *StreamEnd
}

type StreamStart struct {
	ID    string
	Model string
}

type ContentBlockStart struct {
	Index     int
	BlockType string // "text" | "tool_use"
}

type TextDelta struct {
	Index int
	Text  string
}

type ToolCallStart struct {
	Index int
	ID    string
	Name  string
}

type ToolCallDelta struct {
	Index       int
	PartialJSON string
}

type ContentBlockEnd struct{ Index int }

type Finish struct{ StopReason IRStopReason }

type UsageEvent struct {
	InputTokens  int
	OutputTokens int
}

type StreamEnd struct{}
