// Package upstream defines the south-side protocol adapter interface.
// An UpstreamAdapter converts between the canonical Anthropic IR
// (types.MessageRequest / types.MessageResponse / types.SSEEvent) and a
// specific upstream provider's wire format.
//
// Implementations live in internal/upstream/ and are injected into
// ExecutionPlan at routing time.
package upstream

import (
	"context"
	"net/http"

	"miroxy/core/cred"
	"miroxy/internal/types"
)

// UpstreamAdapter is the south-side protocol seam.
// Each upstream provider (Gemini, OpenAI-compat, Passthrough, …) has one
// implementation; adding a new provider = one new file in internal/upstream/.
type UpstreamAdapter interface {
	// ToUpstream builds the non-streaming HTTP request for this provider.
	// The credential is applied before returning.
	ToUpstream(ctx context.Context, req *types.MessageRequest, credential cred.Credential) (*http.Request, error)

	// ToUpstreamStream builds the streaming HTTP request for this provider.
	// The credential is applied before returning.
	ToUpstreamStream(ctx context.Context, req *types.MessageRequest, credential cred.Credential) (*http.Request, error)

	// FromUpstream reads and closes resp.Body, returning a canonical response.
	FromUpstream(resp *http.Response) (*types.MessageResponse, error)

	// StreamFromUpstream reads the provider SSE stream and emits canonical
	// SSE events on the returned channel.  The channel is closed when the
	// stream ends or ctx is cancelled.  The implementation is responsible for
	// closing resp.Body.
	StreamFromUpstream(ctx context.Context, resp *http.Response, msgID, modelAlias string) (<-chan types.SSEEvent, error)
}
