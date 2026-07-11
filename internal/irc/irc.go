// Package irc implements the Intermediate Representation Converters (IRC).
//
// Each IRC is a bidirectional format converter for one LLM protocol dialect.
// It converts in both directions between that protocol's wire format and the
// provider-neutral core/ir types:
//
//	ParseRequest(wire bytes) → *ir.IRRequest      (downstream direction: parse client input)
//	BuildRequest(*ir.IRRequest) → wire bytes       (upstream direction: build provider payload)
//	ParseResponse(wire bytes) → *ir.IRResponse     (upstream direction: parse provider response)
//	BuildResponse(*ir.IRResponse) → wire bytes     (downstream direction: render client response)
//	ParseStream(body) → <-chan ir.StreamEvent       (upstream direction: parse provider SSE)
//	BuildStream(<-chan ir.StreamEvent) → client SSE (downstream direction: render client SSE)
//
// Adding a new protocol = one new file (e.g. openai_irc.go) + one line in registry.
// No changes to server, pipeline, upstream, or downstream code required.
package irc

import (
	"context"
	"fmt"
	"io"

	"miroxy/core/ir"
	"miroxy/internal/types"
)

// UpstreamError is returned by ResponseToIR / StreamToIR when a provider embeds
// an application-level error inside an HTTP 200 body (relay/proxy pattern).
//
// The server retry loop uses errors.As to route:
//   - HTTPStatus 429 → selector.RateLimitError (cooldown, not circuit-break)
//   - HTTPStatus >= 500 → parks the key briefly (ServerOverloadError)
//   - HTTPStatus 4xx → return error to client, no retry
type UpstreamError struct {
	HTTPStatus int
	Code       int
	Message    string
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("upstream %d: %s", e.HTTPStatus, e.Message)
}

// DownstreamConverter converts between a client's wire format and the neutral IR.
// It is the downstream (north) side of the hub — always runs native (touches the
// client SSE stream, a Core Redline).
type DownstreamConverter interface {
	// RequestToIR normalizes a client request into canonical IR form.
	RequestToIR(req *types.MessageRequest) (*ir.IRRequest, error)

	// ResponseFromIR renders an IR response into the client wire format.
	ResponseFromIR(resp *ir.IRResponse, msgID, model string) *types.MessageResponse

	// StreamFromIR drains neutral stream events and writes client SSE events.
	// Synchronous: returns when in is closed or ctx is cancelled.
	// Does not close out — the caller owns out's lifecycle.
	StreamFromIR(ctx context.Context, in <-chan ir.StreamEvent, out chan<- types.SSEEvent, msgID, model string)
}

// UpstreamConverter converts between the neutral IR and an upstream provider's
// wire format. It is the upstream (south) side of the hub — the pluggable part.
// Each provider implementation lives in its own file (gemini_irc.go, openai_irc.go…).
//
// Adding a new upstream provider = implement UpstreamConverter in one file.
type UpstreamConverter interface {
	// RequestToProvider marshals an IR request into the provider's JSON body.
	RequestToProvider(irReq *ir.IRRequest) ([]byte, error)

	// ResponseToIR parses a provider response body into IR.
	// Body-level provider errors are returned as *UpstreamError.
	ResponseToIR(body []byte) (*ir.IRResponse, error)

	// StreamToIR reads the provider's SSE body and emits neutral stream events.
	// Spawns its own goroutine; closes the returned channel when done.
	StreamToIR(ctx context.Context, body io.Reader) <-chan ir.StreamEvent

	// Provider returns the provider identifier, e.g. "gemini", "openai".
	Provider() string
}

// UpstreamBackend is the pluggable execution seam for the provider (south) side.
// It has the same shape as UpstreamConverter but is the boundary across which a
// future backend may cross a serialization boundary:
//
//   - BuiltinBackend         — direct Go call (v1, zero overhead)
//   - WASMBackend     (future) — marshal → wazero guest → unmarshal
//   - GRPCBackend     (future) — call an external translation service via pluginrt/ext
type UpstreamBackend interface {
	RequestToProvider(irReq *ir.IRRequest) ([]byte, error)
	ResponseToIR(body []byte) (*ir.IRResponse, error)
	StreamToIR(ctx context.Context, body io.Reader) <-chan ir.StreamEvent
	Provider() string
}

// BuiltinBackend wraps a UpstreamConverter for built-in (same-process) execution.
// It is the only backend in v1; it marks the boundary a WASM/gRPC backend would replace.
type BuiltinBackend struct {
	conv UpstreamConverter
}

// NewBuiltinBackend wraps a UpstreamConverter as a built-in backend.
func NewBuiltinBackend(conv UpstreamConverter) *BuiltinBackend {
	return &BuiltinBackend{conv: conv}
}

func (b *BuiltinBackend) RequestToProvider(irReq *ir.IRRequest) ([]byte, error) {
	return b.conv.RequestToProvider(irReq)
}

func (b *BuiltinBackend) ResponseToIR(body []byte) (*ir.IRResponse, error) {
	return b.conv.ResponseToIR(body)
}

func (b *BuiltinBackend) StreamToIR(ctx context.Context, body io.Reader) <-chan ir.StreamEvent {
	return b.conv.StreamToIR(ctx, body)
}

func (b *BuiltinBackend) Provider() string { return b.conv.Provider() }
