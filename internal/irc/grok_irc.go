package irc

import (
	"context"
	"io"

	"miroxy/core/ir"
)

// GrokConverter implements UpstreamConverter for xAI Grok.
//
// As of today, Grok's chat/completions endpoint is wire-compatible with
// OpenAI Chat Completions at the IR abstraction level: same message structure,
// same tool_calls format, same finish_reason strings, same streaming delta format.
//
// This file exists as an explicit extension point. When xAI diverges from OpenAI
// (new fields, different finish reasons, live-search metadata, reasoning_content,
// etc.), add overrides here without touching openai_irc.go or other providers.
type GrokConverter struct {
	OpenAIConverter
}

// NewGrokConverter returns a converter wired to the given provider model name.
func NewGrokConverter(model string) *GrokConverter {
	return &GrokConverter{OpenAIConverter{model: model, provider: "grok"}}
}

var _ UpstreamConverter = (*GrokConverter)(nil)

func (*GrokConverter) Provider() string { return "grok" }

// RequestToProvider, ResponseToIR, StreamToIR are promoted from OpenAIConverter.
// Add overrides below when Grok-specific wire differences emerge.

// Example future override (not active):
//
//	func (c *GrokConverter) RequestToProvider(irReq *ir.IRRequest) ([]byte, error) {
//	    body, err := buildOAIRequestBody(irReq, c.model, 0)
//	    // inject Grok-specific fields, strip unsupported ones
//	    return body, err
//	}

func (c *GrokConverter) StreamToIR(ctx context.Context, body io.Reader) <-chan ir.StreamEvent {
	return c.OpenAIConverter.StreamToIR(ctx, body)
}
