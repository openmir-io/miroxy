package pipeline

import (
	"context"

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

	// Target is the resolved upstream provider, set before pipeline.Run.
	Target router.RouteTarget

	// Response is set by UpstreamExecutor on non-streaming success.
	Response *types.MessageResponse

	// Values holds cross-plugin metadata (e.g. "auth.key_hash" written by AuthPlugin).
	Values map[string]any

	// private stream state — written by UpstreamExecutor, consumed by delivery layer
	streamSrc       <-chan types.SSEEvent
	releaseUpstream func(err error)
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
