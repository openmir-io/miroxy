// Package downstream defines the north-side protocol adapter interface.
// A DownstreamAdapter owns all encoding/decoding for one client-facing
// wire protocol.  The discriminator is the HTTP path — one path per protocol.
//
// Implementations live in internal/downstream/:
//
//	AnthropicAdapter  POST /v1/messages           (Claude Code, Cursor, Windsurf, …)
//	OpenAIAdapter     POST /v1/chat/completions    (Codex, OpenCode, LiteLLM, …)
//
// Adding a new client protocol = one new file in internal/downstream/.
// Pipeline, UpstreamAdapter, and server.go are untouched.
package downstream

import (
	"context"
	"net/http"

	"miroxy/internal/types"
)

// DownstreamAdapter is the north-side protocol seam.
type DownstreamAdapter interface {
	// Protocol returns the canonical protocol name used for passthrough
	// detection ("anthropic", "openai", …).
	Protocol() string

	// Path is the HTTP path this adapter handles (e.g. "/v1/messages").
	// The server registers "POST <Path()>" automatically.
	Path() string

	// Decode parses the raw HTTP request into the canonical IR.
	// ALL client-protocol normalization (e.g. system-message extraction,
	// role aliasing) happens here and nowhere else.
	Decode(r *http.Request) (*types.MessageRequest, error)

	// WriteError writes a protocol-appropriate error response.
	WriteError(w http.ResponseWriter, status int, errType, msg string)

	// WriteResponse writes a non-streaming canonical response.
	WriteResponse(w http.ResponseWriter, resp *types.MessageResponse)

	// WriteResponseAsStream wraps a canonical response in this protocol's SSE
	// format when the client sent stream:true but the pipeline produced a
	// synchronous response (e.g. CommandPlugin short-circuit).
	// This keeps the pipeline layer free of any protocol-specific SSE knowledge.
	WriteResponseAsStream(ctx context.Context, w http.ResponseWriter, resp *types.MessageResponse)

	// WriteStream delivers SSE events in this protocol's format.
	// It drains src until the channel is closed or ctx is cancelled.
	// Returns the first write error encountered (nil on clean close).
	WriteStream(ctx context.Context, w http.ResponseWriter, req *types.MessageRequest, src <-chan types.SSEEvent) error
}
