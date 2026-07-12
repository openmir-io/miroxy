package pipeline

import (
	"context"
	"io"

	"miroxy/core/router"
	"miroxy/internal/types"
)

// LLMContext is the zero-copy request context that flows through all plugins.
// Plugins communicate by mutating shared pointer fields — no copying of Request/Response.
type LLMContext struct {
	// RequestCtx is the base context for the entire pipeline run (from http.Request.Context).
	RequestCtx context.Context

	// Request is the inbound Anthropic request. Plugins may mutate it (e.g. Rectifier strips fields).
	Request *types.MessageRequest

	// ClientProtocol is the protocol of the DownstreamAdapter that decoded
	// this request (e.g. "anthropic", "openai", "openai-responses") — set
	// once by the server from a.Protocol(), before pipeline.Run. This is the
	// discriminator UpstreamExecutor compares against each attempt's target
	// protocol to pick real transform vs raw passthrough; it is NOT a config
	// value — the HTTP path already determined it structurally.
	ClientProtocol string

	// RawRequestBody is the original request bytes, captured before Decode
	// folded them into Request. Used for byte-for-byte passthrough when a
	// target's protocol matches ClientProtocol.
	RawRequestBody []byte

	// Target is the resolved upstream provider, set before pipeline.Run.
	Target router.RouteTarget

	// Response is set by UpstreamExecutor on non-streaming success. When
	// Response.RawBody is non-nil (a passthrough attempt), the delivery layer
	// writes it verbatim instead of re-encoding Response's other fields.
	Response *types.MessageResponse

	// Values holds cross-plugin metadata (e.g. "auth.key_hash" written by AuthPlugin).
	Values map[string]any

	// private stream state — written by UpstreamExecutor, consumed by delivery layer
	streamSrc       <-chan types.SSEEvent
	releaseUpstream func(err error)

	// private raw-stream state — set instead of streamSrc for a passthrough
	// streaming attempt (client and upstream protocols matched): the
	// delivery layer relays rawStreamBody's bytes directly, skipping the
	// canonical SSEEvent channel entirely.
	rawStreamBody        io.ReadCloser
	rawStreamContentType string
	rawStreamStatus      int
}

// NewContext builds a fresh LLMContext for one request.
func NewContext(reqCtx context.Context, req *types.MessageRequest, target router.RouteTarget) *LLMContext {
	return &LLMContext{
		RequestCtx: reqCtx,
		Request:    req,
		Target:     target,
		Values:     make(map[string]any),
	}
}

// SetStream stores the upstream SSE channel and release callback.
// Called by UpstreamExecutor after successful stream initiation.
// The release callback must call the context cancel function.
func (c *LLMContext) SetStream(src <-chan types.SSEEvent, release func(err error)) {
	c.streamSrc = src
	c.releaseUpstream = release
}

// StreamSrc returns the upstream SSE event channel, or nil if not streaming.
func (c *LLMContext) StreamSrc() <-chan types.SSEEvent {
	return c.streamSrc
}

// ReleaseUpstream calls the release callback with the stream drain error.
// No-op when not streaming (releaseUpstream is nil).
func (c *LLMContext) ReleaseUpstream(err error) {
	if c.releaseUpstream != nil {
		c.releaseUpstream(err)
	}
}

// ReleaseFunc returns the raw release callback (or nil), so a plugin that
// wraps the stream/rawStream data path — swapping in a filtering channel or
// reader — can carry the original callback forward via SetStream/
// SetRawStream without going through ReleaseUpstream itself. Calling
// SetStream(newSrc, c.ReleaseUpstream) instead of this getter would make
// releaseUpstream point back at ReleaseUpstream, which calls
// releaseUpstream — infinite recursion. Use this getter to avoid that.
func (c *LLMContext) ReleaseFunc() func(err error) {
	return c.releaseUpstream
}

// SetRawStream stores the raw upstream response body for a passthrough
// streaming attempt (client and upstream protocols matched, so the bytes
// need no reframing) and the release callback, mirroring SetStream.
func (c *LLMContext) SetRawStream(body io.ReadCloser, contentType string, status int, release func(err error)) {
	c.rawStreamBody = body
	c.rawStreamContentType = contentType
	c.rawStreamStatus = status
	c.releaseUpstream = release
}

// RawStream returns the raw upstream response body/content-type/status set
// by SetRawStream, or ok=false when this attempt was not a raw passthrough.
func (c *LLMContext) RawStream() (body io.ReadCloser, contentType string, status int, ok bool) {
	if c.rawStreamBody == nil {
		return nil, "", 0, false
	}
	return c.rawStreamBody, c.rawStreamContentType, c.rawStreamStatus, true
}
