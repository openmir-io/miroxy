package pipeline

import (
	"context"
	"io"
	"log/slog"
	"net/http"

	"miroxy/core/ir"
	"miroxy/core/router"
)

// LLMContext is the zero-copy request context that flows through all plugins.
// Plugins communicate by mutating shared pointer fields — no copying of Request/Response.
type LLMContext struct {
	// RequestCtx is the base context for the entire pipeline run (from http.Request.Context).
	RequestCtx context.Context

	// Request is the inbound request in the neutral IR — no client protocol
	// is privileged. Plugins may mutate it (e.g. Compress trims Messages).
	Request *ir.IRRequest

	// ClientModel is the model name the client actually sent, captured by
	// Decode before IR conversion (IR carries no model field by design —
	// see core/ir/ir.go). Used for routing lookup and echoed back in
	// responses so the client sees exactly what it asked for.
	ClientModel string

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

	// RawRequestHeaders are the client's original HTTP headers, captured
	// alongside RawRequestBody for the same passthrough use — e.g. Anthropic's
	// required anthropic-version, which miroxy has no protocol-agnostic way to invent.
	RawRequestHeaders http.Header

	// EncodeRequest re-serializes Request into ClientProtocol's wire bytes —
	// set from the DownstreamAdapter that decoded this request.
	EncodeRequest func(req *ir.IRRequest) ([]byte, error)

	// RequestRewritten is set by a plugin that structurally rewrote Request
	// (e.g. Compress trimming/restructuring Messages) in a way a targeted
	// byte-level patch can't express. UpstreamExecutor calls
	// RefreshRawBodyIfRewritten before dispatching so a passthrough-eligible
	// attempt ships the rewritten content instead of the client's original
	// bytes. Warden's redactions don't need this — they are exact-substring
	// swaps applied directly to RawRequestBody too (see warden/plugin.go
	// sanitizeRequest).
	RequestRewritten bool

	// Target is the resolved upstream provider, set before pipeline.Run.
	Target router.RouteTarget

	// Response is set by UpstreamExecutor on non-streaming success, in the
	// neutral IR. A raw-passthrough attempt never populates this — see
	// SetRawResponse/RawResponse instead, exactly like the streaming path.
	Response *ir.IRResponse

	// Values holds cross-plugin metadata (e.g. "auth.key_hash" written by AuthPlugin).
	Values map[string]any

	// private stream state — written by UpstreamExecutor, consumed by delivery layer
	streamSrc       <-chan ir.StreamEvent
	releaseUpstream func(err error)

	// private raw-stream state — set instead of streamSrc for a passthrough
	// streaming attempt (client and upstream protocols matched): the
	// delivery layer relays rawStreamBody's bytes directly, skipping the
	// canonical IR stream-event channel entirely.
	rawStreamBody        io.ReadCloser
	rawStreamContentType string
	rawStreamStatus      int

	// private raw-response state — the non-streaming twin of the above, set
	// instead of Response for a passthrough attempt (client and upstream
	// protocols matched, so the bytes need no reframing through IR).
	rawResponseBody        []byte
	rawResponseContentType string
	rawResponseStatus      int
}

// NewContext builds a fresh LLMContext for one request. model is the
// client's originally-requested model name (see LLMContext.ClientModel).
func NewContext(reqCtx context.Context, req *ir.IRRequest, model string, target router.RouteTarget) *LLMContext {
	return &LLMContext{
		RequestCtx:  reqCtx,
		Request:     req,
		ClientModel: model,
		Target:      target,
		Values:      make(map[string]any),
	}
}

// SetStream stores the upstream SSE channel and release callback.
// Called by UpstreamExecutor after successful stream initiation.
// The release callback must call the context cancel function.
func (c *LLMContext) SetStream(src <-chan ir.StreamEvent, release func(err error)) {
	c.streamSrc = src
	c.releaseUpstream = release
}

// StreamSrc returns the upstream SSE event channel, or nil if not streaming.
func (c *LLMContext) StreamSrc() <-chan ir.StreamEvent {
	return c.streamSrc
}

// ReleaseUpstream calls the release callback with the stream drain error.
// No-op when not streaming (releaseUpstream is nil).
func (c *LLMContext) ReleaseUpstream(err error) {
	if c.releaseUpstream != nil {
		c.releaseUpstream(err)
	}
}

// RefreshRawBodyIfRewritten re-marshals Request into RawRequestBody when
// RequestRewritten is set — otherwise a passthrough-eligible attempt would
// ship the client's original, unrewritten bytes instead of what the
// pipeline actually produced. Called by UpstreamExecutor once Request has
// reached its final state for this retry loop (after MaxTokens defaulting),
// before any attempt can dispatch through PassthroughUpstream.
func (c *LLMContext) RefreshRawBodyIfRewritten() {
	if !c.RequestRewritten || len(c.RawRequestBody) == 0 {
		return
	}
	if c.EncodeRequest == nil {
		// Request is IR — there is no meaningful "default" wire encoding to
		// fall back to (unlike the old Anthropic-shaped canonical type,
		// json.Marshal(Request) is never a real protocol's wire format).
		slog.Warn("pipeline: no EncodeRequest set on a rewritten request awaiting passthrough")
		return
	}
	raw, err := c.EncodeRequest(c.Request)
	if err != nil {
		slog.Warn("pipeline: failed to re-marshal rewritten request for passthrough", "error", err)
		return
	}
	c.RawRequestBody = raw
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

// SetRawResponse stores a raw non-streaming upstream response body for a
// passthrough attempt — the non-streaming twin of SetRawStream. Never sets
// Response; the delivery layer checks RawResponse first.
func (c *LLMContext) SetRawResponse(body []byte, contentType string, status int) {
	c.rawResponseBody = body
	c.rawResponseContentType = contentType
	c.rawResponseStatus = status
}

// RawResponse returns the raw body/content-type/status set by
// SetRawResponse, or ok=false when this attempt was not a raw passthrough.
func (c *LLMContext) RawResponse() (body []byte, contentType string, status int, ok bool) {
	if c.rawResponseBody == nil {
		return nil, "", 0, false
	}
	return c.rawResponseBody, c.rawResponseContentType, c.rawResponseStatus, true
}
