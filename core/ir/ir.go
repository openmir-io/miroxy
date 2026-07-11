// Package ir defines the Intermediate Representation (IR) for miroxy's translation layer.
//
// The IR is the canonical, provider-neutral form of an LLM chat request and response.
// Every IRC (Intermediate Representation Converter) implementation operates on
// *IRRequest and *IRResponse rather than raw provider wire types — this is the single
// contract shared across all upstream and downstream protocol converters.
//
// IRStopReason vocabulary covers all provider-neutral stop conditions; each IRC
// maps between these values and its own provider-specific strings.
//
// Schema source of truth: core/ir/ir.proto (proto3 mirror of these structs).
package ir

// IRStopReason is the provider-neutral stop reason vocabulary.
type IRStopReason string

const (
	IRStopReasonStop          IRStopReason = "stop"           // normal end of generation
	IRStopReasonToolUse       IRStopReason = "tool_use"       // model invoked one or more tools
	IRStopReasonMaxTokens     IRStopReason = "max_tokens"     // generation limit reached
	IRStopReasonContentFilter IRStopReason = "content_filter" // safety/policy block
	IRStopReasonError         IRStopReason = "error"          // provider-side error
)

// IRRequest is the normalized form of an LLM chat request.
// All format ambiguities are resolved before this struct is populated.
type IRRequest struct {
	System     string
	Messages   []IRMessage
	Tools      []IRTool
	ToolChoice *IRToolChoice
	Gen        IRGenerationConfig
	Stream     bool
}

// IRMessage is a single conversation turn.
type IRMessage struct {
	Role  string // "user" | "assistant"
	Parts []IRContentPart
}

// IRContentPart is a discriminated union of content part types.
// Exactly one of Text, ToolUse, or ToolResult is non-nil.
type IRContentPart struct {
	Text       *IRTextPart
	ToolUse    *IRToolUsePart
	ToolResult *IRToolResultPart
}

// IRTextPart is a plain text content block.
type IRTextPart struct{ Text string }

// IRToolUsePart is a model-initiated function call block.
type IRToolUsePart struct {
	ID        string
	Name      string
	InputJSON []byte
}

// IRToolResultPart is the user-provided result of a function call.
type IRToolResultPart struct {
	ToolUseID string
	Content   []IRContentPart
	IsError   bool
}

// IRTool is a function declaration available to the model.
type IRTool struct {
	Name            string
	Description     string
	InputSchemaJSON []byte
}

// IRToolChoice controls how the model selects functions.
type IRToolChoice struct {
	Type string // "auto" | "any" | "tool" | "none"
	Name string // only meaningful when Type="tool"
}

// IRGenerationConfig carries sampling and output-control parameters.
type IRGenerationConfig struct {
	Temperature *float64
	TopP        *float64
	TopK        *int
	MaxTokens   int
	StopSeqs    []string
}

// IRResponse is the normalized form of an LLM response.
type IRResponse struct {
	Content    []IRResponseBlock
	StopReason IRStopReason
	StopSeq    *string
	Usage      IRUsage
}

// IRResponseBlock is a discriminated union of response block types.
type IRResponseBlock struct {
	Text    *IRTextPart
	ToolUse *IRToolUsePart
}

// IRUsage carries token count metadata.
type IRUsage struct {
	InputTokens  int
	OutputTokens int
}
