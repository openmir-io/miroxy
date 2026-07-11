package irc

import (
	"context"
	"io"

	"miroxy/core/ir"
)

// GLMConverter implements UpstreamConverter for Zhipu AI (GLM/ZAI) chat/completions.
//
// GLM uses the OpenAI-compatible endpoint at:
//   - International: https://api.z.ai/api/paas/v4/chat/completions
//   - China: https://open.bigmodel.cn/api/paas/v4/chat/completions
//
// Auth: standard Bearer token (the legacy JWT mechanism applies only to the old
// native SDK; the modern paas/v4 endpoint accepts plain API keys as Bearer).
//
// Differences from OpenAI handled at the IR level:
//   - Temperature is clamped to [0, 1] (OpenAI allows [0, 2]).
//   - Special finish reasons: "network_error" → stop, "sensitive" → content_filter.
//   - GLM thinking models return reasoning_content alongside the answer; the IR v1
//     does not model reasoning, so reasoning deltas are silently dropped.
//   - web_search_result in responses passes through as unrecognized JSON — not parsed.
type GLMConverter struct {
	OpenAIConverter
}

// NewGLMConverter returns a converter wired to the given provider model name.
func NewGLMConverter(model string) *GLMConverter {
	return &GLMConverter{OpenAIConverter{model: model, provider: "glm"}}
}

var _ UpstreamConverter = (*GLMConverter)(nil)

// Provider is declared explicitly here (not promoted from OpenAIConverter) so that
// Go's method set is unambiguous — GLMConverter overrides the embedded method.
func (*GLMConverter) Provider() string { return "glm" }

// RequestToProvider builds the GLM request body. Identical to OpenAI except
// temperature is clamped to [0, 1] (GLM rejects values above 1.0).
func (c *GLMConverter) RequestToProvider(irReq *ir.IRRequest) ([]byte, error) {
	return buildOAIRequestBody(irReq, c.model, 1.0)
}

// ResponseToIR parses the GLM response, normalizing GLM-specific finish reasons
// before the standard OpenAI reason mapping.
func (c *GLMConverter) ResponseToIR(body []byte) (*ir.IRResponse, error) {
	return responseToIR(body, normalizeGLMReason)
}

// StreamToIR parses the GLM SSE stream, normalizing GLM-specific finish reasons.
func (c *GLMConverter) StreamToIR(ctx context.Context, body io.Reader) <-chan ir.StreamEvent {
	return streamToIR(ctx, body, normalizeGLMReason)
}

// normalizeGLMReason remaps GLM-specific finish reasons to their OpenAI equivalents
// before the standard mapOAIFinishReason mapping runs.
func normalizeGLMReason(r string) string {
	switch r {
	case "network_error":
		return "stop"
	case "sensitive":
		return "content_filter"
	default:
		return r
	}
}
