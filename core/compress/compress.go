// Package compress defines the stable interface for context compression.
//
// Three execution models are supported (three-layer ward):
//
//	Builtin — deterministic Go algorithms, zero latency, no external deps (v1)
//	WASM    — pluggable sandbox for custom ML-based compressors (future)
//	GRPC    — delegate to an external compression service (future)
//
// Implementations must be safe for concurrent use.
package compress

import "context"

// ContentHint lets callers tell the compressor what kind of content it is
// receiving so it can route to the right strategy without re-detecting.
type ContentHint int

const (
	HintAuto   ContentHint = iota // detect automatically
	HintCode                      // source code
	HintJSON                      // structured JSON / API response
	HintLog                       // log output
	HintDialog                    // conversation history (multi-turn)
	HintText                      // plain prose
)

// Compressor reduces the token count of a message list to fit within a budget.
// Implementations must be safe for concurrent use.
type Compressor interface {
	Compress(ctx context.Context, req *Request) (*Result, error)
}

// Request carries the message history and compression parameters.
type Request struct {
	// System is the system prompt (always preserved, never compressed).
	System string
	// Messages is the conversation history to compress.
	Messages []Message
	// Budget is the target output token count.
	// When 0, the compressor applies a default ratio (typically 0.5).
	Budget int
	// Ratio is the target compression ratio (0.0–1.0).
	// Used only when Budget == 0. 0 means use the compressor's default.
	Ratio float64
	// Hint overrides content-type detection for all messages.
	// Use HintAuto (default) to let the compressor detect per-message.
	Hint ContentHint
}

// Result carries the compressed message list and compression stats.
type Result struct {
	System           string
	Messages         []Message
	OriginalTokens   int
	CompressedTokens int
	// Strategies records which compression passes were applied.
	// Used by "miroxy stat" to show compression breakdown.
	Strategies []string
}

// Message is a protocol-agnostic representation of a single conversation turn.
// It maps onto Anthropic and OpenAI message formats; the plugin layer handles
// the conversion at the boundary.
type Message struct {
	Role  string        // "user", "assistant", "tool", "system"
	Parts []ContentPart // one or more content segments
}

// ContentPart is one content segment within a message.
// Compression operates on Parts individually: text and tool_result parts can
// be compressed; tool_use, image, and document parts are preserved verbatim.
type ContentPart struct {
	Type string // "text", "tool_use", "tool_result", "image", "document", …

	// Text holds the extractable text for "text" and "tool_result" parts.
	// Compression reads and writes this field.
	Text string

	// ToolUseID is the tool_use ID referenced by a "tool_result" part.
	// Used by the sliding-window orphan-protection logic.
	ToolUseID string
	// ToolID is the ID of a "tool_use" part (matches ToolUseID in the result).
	ToolID string
	// ToolName is the function name in a "tool_use" part.
	ToolName string

	// Raw holds the verbatim serialised content for parts the compressor
	// does not inspect (image, document, tool_use input JSON, etc.).
	// When non-nil, Text is ignored on serialisation.
	Raw []byte
}

// CCRStore is the Compress-Cache-Retrieve key-value seam.
// Compressed output can contain retrieval markers of the form
//
//	[N items omitted. Retrieve: hash=<hash>]
//
// A future LLM tool call can use the hash to recover the original content via
// the admin API. The default implementation is in-memory (MemCCRStore); a
// bbolt-backed implementation provides durability across restarts.
type CCRStore interface {
	// Store persists content and returns a short hex hash for the marker.
	Store(content []byte) (hash string, err error)
	// Retrieve fetches previously stored content by hash.
	Retrieve(hash string) ([]byte, error)
	// Close releases underlying resources.
	Close() error
}
