// Package upstream defines the south-side protocol adapter interface.
// An UpstreamAdapter converts between the canonical request/response types
// and a specific upstream provider's wire format. Its streaming leg speaks
// the neutral core/ir.StreamEvent — no client protocol is privileged here.
//
// Implementations live in internal/upstream/ and are injected into
// ExecutionPlan at routing time.
package upstream

import (
	"context"
	"net/http"

	"miroxy/core/cred"
	"miroxy/core/ir"
)

// UpstreamAdapter is the south-side protocol seam.
// Each upstream provider (Gemini, OpenAI-compat, Passthrough, …) has one
// implementation; adding a new provider = one new file in internal/upstream/.
type UpstreamAdapter interface {
	// ToUpstream builds the non-streaming HTTP request for this provider.
	// The credential is applied before returning.
	ToUpstream(ctx context.Context, req *ir.IRRequest, credential cred.Credential) (*http.Request, error)

	// ToUpstreamStream builds the streaming HTTP request for this provider.
	// The credential is applied before returning.
	ToUpstreamStream(ctx context.Context, req *ir.IRRequest, credential cred.Credential) (*http.Request, error)

	// FromUpstream reads and closes resp.Body, returning a canonical response.
	FromUpstream(resp *http.Response) (*ir.IRResponse, error)

	// StreamFromUpstream reads the provider SSE stream and emits neutral IR
	// stream events; closes the channel on stream end or ctx cancel, and owns closing resp.Body.
	StreamFromUpstream(ctx context.Context, resp *http.Response, msgID, modelAlias string) (<-chan ir.StreamEvent, error)
}
