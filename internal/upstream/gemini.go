package upstream

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"miroxy/core/cred"
	"miroxy/core/ir"
	coreup "miroxy/core/upstream"
	"miroxy/internal/wireformat"
)

const defaultGeminiBase = "https://generativelanguage.googleapis.com"

// GeminiTranslator implements the Translator port for Google AI Studio and
// compatible relay providers. It owns transport (endpoint URL + auth) and
// wraps a pluggable provider upstream (in-process Gemini today). Request,
// response, and stream all flow through the neutral IR — LLMContext.Request/
// Response are already IR-typed by the time they reach this adapter:
//
//	ToUpstream:         IR → [upstream.RequestToProvider] → Gemini
//	FromUpstream:       Gemini → [upstream.ResponseToIR] → IR
//	StreamFromUpstream: Gemini SSE → [upstream.StreamToIR] → IR events (client-protocol framing happens at the DownstreamAdapter, not here)
type GeminiAdapter struct {
	upstreamModel string
	baseURL       string
	// authStyle removed: credential type now encodes how auth is applied (Apply method).

	upstream wireformat.UpstreamBackend
}

var _ coreup.UpstreamAdapter = (*GeminiAdapter)(nil)

func newGeminiTranslator(upstreamModel, baseURL string) *GeminiAdapter {
	return &GeminiAdapter{
		upstreamModel: upstreamModel,
		baseURL:       baseURL,
		upstream:      wireformat.NewBuiltinBackend(&wireformat.GeminiConverter{}),
	}
}

// NewGemini creates a translator pointed at the default Google AI Studio endpoint.
func NewGemini(upstreamModel string) *GeminiAdapter {
	return newGeminiTranslator(upstreamModel, defaultGeminiBase)
}

// NewGeminiWithBase creates a translator with a custom base URL — used in integration tests.
func NewGeminiWithBase(upstreamModel, baseURL string) *GeminiAdapter {
	return newGeminiTranslator(upstreamModel, baseURL)
}

// NewGeminiWithConfig creates a translator with a custom base URL and provider model.
// If baseURL is empty, defaults to the Google AI Studio endpoint.
// Auth style (query key vs bearer) is now encoded in the Credential passed to
// ToUpstream/ToUpstreamStream — QueryCredential for ?key=, HeaderCredential for Bearer.
func NewGeminiWithConfig(upstreamModel, baseURL string) *GeminiAdapter {
	if baseURL == "" {
		baseURL = defaultGeminiBase
	}
	return newGeminiTranslator(upstreamModel, baseURL)
}

func (t *GeminiAdapter) endpointURL() string {
	return fmt.Sprintf("%s/v1beta/models/%s:generateContent", t.baseURL, t.upstreamModel)
}

func (t *GeminiAdapter) streamEndpointURL() string {
	return fmt.Sprintf("%s/v1beta/models/%s:streamGenerateContent?alt=sse", t.baseURL, t.upstreamModel)
}

func (t *GeminiAdapter) ToUpstream(ctx context.Context, req *ir.IRRequest, credential cred.Credential) (*http.Request, error) {
	return t.buildHTTPRequest(ctx, req, t.endpointURL(), credential)
}

func (t *GeminiAdapter) ToUpstreamStream(ctx context.Context, req *ir.IRRequest, credential cred.Credential) (*http.Request, error) {
	return t.buildHTTPRequest(ctx, req, t.streamEndpointURL(), credential)
}

func (t *GeminiAdapter) buildHTTPRequest(ctx context.Context, req *ir.IRRequest, url string, credential cred.Credential) (*http.Request, error) {
	slog.Debug("building upstream request",
		"upstream_model", t.upstreamModel,
		"messages", len(req.Messages),
		"max_tokens", req.Gen.MaxTokens,
		"has_system", len(req.System) > 0,
		"tools", len(req.Tools),
	)
	body, err := t.upstream.RequestToProvider(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build upstream request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if err := credential.Apply(httpReq); err != nil {
		return nil, fmt.Errorf("apply credential: %w", err)
	}
	return httpReq, nil
}

func (t *GeminiAdapter) FromUpstream(resp *http.Response) (*ir.IRResponse, error) {
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read upstream response: %w", err)
	}
	irResp, err := t.upstream.ResponseToIR(body)
	if err != nil {
		return nil, err // includes *UpstreamError for body-level provider errors (G-05)
	}
	return irResp, nil
}

func (t *GeminiAdapter) StreamFromUpstream(ctx context.Context, resp *http.Response, msgID, modelAlias string) (<-chan ir.StreamEvent, error) {
	slog.Debug("stream started", "upstream_model", t.upstreamModel, "msg_id", msgID, "model_alias", modelAlias)
	// Gemini SSE → neutral IR stream events — framing into the client's own
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
