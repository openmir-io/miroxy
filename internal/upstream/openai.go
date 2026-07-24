package upstream

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"miroxy/core/cred"
	"miroxy/core/ir"
	coreup "miroxy/core/upstream"
	"miroxy/internal/wireformat"
)

// OpenAICompatTranslator implements Translator for OpenAI-compatible providers
// (openai, deepseek, glm). The three providers share identical HTTP structure:
//   - Endpoint: baseURL + "/chat/completions"  (same for streaming and non-streaming)
//   - Stream flag: in the JSON body, not the URL
//   - Auth: Bearer token via HeaderCredential (applied by credential.Apply)
//
// Provider-specific differences (temperature clamp, finish_reason normalization)
// are encapsulated in the UpstreamConverter implementation injected via upstream.
type OpenAICompatAdapter struct {
	upstreamModel string
	baseURL       string // trimmed of trailing slash; no path suffix
	upstream      wireformat.UpstreamBackend
}

var _ coreup.UpstreamAdapter = (*OpenAICompatAdapter)(nil)

// NewOpenAI creates a translator for the standard OpenAI API.
// baseURL defaults to the official OpenAI endpoint when empty.
func NewOpenAI(upstreamModel, baseURL string) *OpenAICompatAdapter {
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	return &OpenAICompatAdapter{
		upstreamModel: upstreamModel,
		baseURL:       strings.TrimRight(baseURL, "/"),
		upstream:      wireformat.NewBuiltinBackend(wireformat.NewOpenAIConverter(upstreamModel)),
	}
}

// NewDeepSeek creates a translator for the DeepSeek API.
// baseURL defaults to the official DeepSeek endpoint when empty.
func NewDeepSeek(upstreamModel, baseURL string) *OpenAICompatAdapter {
	if baseURL == "" {
		baseURL = "https://api.deepseek.com/v1"
	}
	return &OpenAICompatAdapter{
		upstreamModel: upstreamModel,
		baseURL:       strings.TrimRight(baseURL, "/"),
		upstream:      wireformat.NewBuiltinBackend(wireformat.NewOpenAICompatConverter(upstreamModel, "deepseek")),
	}
}

// NewGrok creates a translator for xAI Grok.
// baseURL defaults to the official xAI endpoint when empty.
// GrokConverter is used (not the generic OpenAICompatConverter) so that
// Grok-specific overrides can be added to grok.go without touching this file.
func NewGrok(upstreamModel, baseURL string) *OpenAICompatAdapter {
	if baseURL == "" {
		baseURL = "https://api.x.ai/v1"
	}
	return &OpenAICompatAdapter{
		upstreamModel: upstreamModel,
		baseURL:       strings.TrimRight(baseURL, "/"),
		upstream:      wireformat.NewBuiltinBackend(wireformat.NewGrokConverter(upstreamModel)),
	}
}

// NewGLM creates a translator for Zhipu AI (GLM/ZAI).
// baseURL defaults to the international ZAI endpoint when empty.
// For China region, set baseURL to "https://open.bigmodel.cn/api/paas/v4".
func NewGLM(upstreamModel, baseURL string) *OpenAICompatAdapter {
	if baseURL == "" {
		baseURL = "https://api.z.ai/api/paas/v4"
	}
	return &OpenAICompatAdapter{
		upstreamModel: upstreamModel,
		baseURL:       strings.TrimRight(baseURL, "/"),
		upstream:      wireformat.NewBuiltinBackend(wireformat.NewGLMConverter(upstreamModel)),
	}
}

// endpointURL returns the chat completions URL for this provider.
// OpenAI-compatible providers use the same endpoint for streaming and non-streaming —
// the "stream" flag is in the request body, not the URL path.
func (t *OpenAICompatAdapter) endpointURL() string {
	return t.baseURL + "/chat/completions"
}

func (t *OpenAICompatAdapter) buildHTTPRequest(
	ctx context.Context,
	req *ir.IRRequest,
	credential cred.Credential,
) (*http.Request, error) {
	slog.Debug("building upstream request",
		"upstream_model", t.upstreamModel,
		"messages", len(req.Messages),
		"max_tokens", req.Gen.MaxTokens,
		"has_system", len(req.System) > 0,
		"tools", len(req.Tools),
	)
	body, err := t.upstream.RequestToProvider(req)
	if err != nil {
		return nil, fmt.Errorf("upstream RequestToProvider: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", t.endpointURL(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build upstream request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	if err := credential.Apply(httpReq); err != nil {
		return nil, fmt.Errorf("apply credential: %w", err)
	}
	return httpReq, nil
}

func (t *OpenAICompatAdapter) ToUpstream(
	ctx context.Context,
	req *ir.IRRequest,
	credential cred.Credential,
) (*http.Request, error) {
	return t.buildHTTPRequest(ctx, req, credential)
}

func (t *OpenAICompatAdapter) ToUpstreamStream(
	ctx context.Context,
	req *ir.IRRequest,
	credential cred.Credential,
) (*http.Request, error) {
	return t.buildHTTPRequest(ctx, req, credential)
}

func (t *OpenAICompatAdapter) FromUpstream(resp *http.Response) (*ir.IRResponse, error) {
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read upstream response: %w", err)
	}
	irResp, err := t.upstream.ResponseToIR(body)
	if err != nil {
		return nil, err // includes *UpstreamError for body-level provider errors
	}
	return irResp, nil
}

func (t *OpenAICompatAdapter) StreamFromUpstream(
	ctx context.Context,
	resp *http.Response,
	msgID, modelAlias string,
) (<-chan ir.StreamEvent, error) {
	slog.Debug("stream started", "upstream_model", t.upstreamModel, "msg_id", msgID, "model_alias", modelAlias)
	// OpenAI SSE → neutral IR stream events — framing into the client's own
	// wire dialect happens at the DownstreamAdapter, not here.
	irEvents := t.upstream.StreamToIR(ctx, resp.Body)
	out := make(chan ir.StreamEvent, 32)
	go func() {
		defer resp.Body.Close()
		defer close(out)
		for ev := range irEvents {
			select {
			case <-ctx.Done():
				return
			case out <- ev:
			}
		}
	}()
	return out, nil
}
