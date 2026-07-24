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

	"miroxy/core/ir"
)

// DownstreamAdapter is the north-side protocol seam.
type DownstreamAdapter interface {
	// Protocol returns the canonical protocol name used for passthrough
	// detection ("anthropic", "openai", …).
	Protocol() string

	// Path is the HTTP path this adapter handles (e.g. "/v1/messages").
	// The server registers "POST <Path()>" automatically.
	Path() string

	// Decode parses the raw HTTP request into the canonical IR, returning
	// the client's originally-requested model name alongside it — IR itself
	// carries no model field (see core/ir/ir.go). ALL client-protocol
	// normalization happens here and nowhere else.
	Decode(r *http.Request) (req *ir.IRRequest, model string, err error)

	// EncodeRequest is the reverse of Decode — re-serializes a (possibly
	// pipeline-rewritten) canonical request into this protocol's wire bytes.
	// model is the client's originally-requested model name (see Decode).
	EncodeRequest(req *ir.IRRequest, model string) ([]byte, error)

	// WriteError writes a protocol-appropriate error response.
	WriteError(w http.ResponseWriter, status int, errType, msg string)

	// WriteResponse writes a non-streaming canonical response. msgID/model
	// are supplied by the caller — the IR carries neither.
	WriteResponse(w http.ResponseWriter, resp *ir.IRResponse, msgID, model string)

	// WriteResponseAsStream wraps a canonical response in this protocol's SSE
	// format when the client sent stream:true but the pipeline produced a
	// synchronous response (e.g. CommandPlugin short-circuit).
	// This keeps the pipeline layer free of any protocol-specific SSE knowledge.
	WriteResponseAsStream(ctx context.Context, w http.ResponseWriter, resp *ir.IRResponse, msgID, model string)

	// WriteStream converts neutral IR stream events into this protocol's SSE
	// dialect and writes them; drains src until closed or ctx is cancelled.
	WriteStream(ctx context.Context, w http.ResponseWriter, model string, src <-chan ir.StreamEvent) error
}
