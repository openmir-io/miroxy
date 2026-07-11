package translator

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"miroxy/core/cred"
	"miroxy/internal/idgen"
	"miroxy/internal/irc"
	"miroxy/internal/types"
)

// Translator converts between the Anthropic wire format and an upstream provider's format.
// Each provider is wired behind this port (gemini.go's GeminiTranslator today).
//
// NOTE: this interface is intentionally unchanged. The Epic 2 swap to
// UpstreamBackend(ctx, *ir.Request, Direction) is scheduled with the miroxy-ir
// repo (see docs/design/architecture-v2-three-layer-ward.md §10). Story IR-2 builds
// the bidirectional converters + Builtin upstream seam *beneath* this stable port.
type Translator interface {
	// ToUpstream builds a non-streaming upstream *http.Request.
	// The credential is applied to the request before returning.
	ToUpstream(ctx context.Context, req *types.MessageRequest, credential cred.Credential) (*http.Request, error)

	// ToUpstreamStream builds a streaming upstream *http.Request (SSE endpoint).
	// The credential is applied to the request before returning.
	ToUpstreamStream(ctx context.Context, req *types.MessageRequest, credential cred.Credential) (*http.Request, error)

	// FromUpstream reads and closes resp.Body, returning an Anthropic MessageResponse.
	FromUpstream(resp *http.Response) (*types.MessageResponse, error)

	// StreamFromUpstream reads the upstream SSE stream and emits Anthropic SSE events on
	// the returned channel. The channel is closed when the stream ends or ctx is cancelled.
	// The implementation is responsible for closing resp.Body.
	StreamFromUpstream(ctx context.Context, resp *http.Response, msgID, modelAlias string) (<-chan types.SSEEvent, error)
}

const defaultGeminiBase = "https://generativelanguage.googleapis.com"

// GeminiTranslator implements the Translator port for Google AI Studio and
// compatible relay providers. It owns transport (endpoint URL + auth) and composes
// a Native Anthropic frontend with a pluggable provider upstream (in-process Gemini
// today). Request, response, and stream all flow through the neutral IR:
//
//	ToUpstream:         Anthropic → [downstream.RequestToIR] → IR → [upstream.RequestToProvider] → Gemini
//	FromUpstream:       Gemini    → [upstream.ResponseToIR] → IR → [downstream.ResponseFromIR] → Anthropic
//	StreamFromUpstream: Gemini SSE → [upstream.StreamToIR] → IR events → [downstream.StreamFromIR] → Anthropic SSE
type GeminiTranslator struct {
	providerModel string
	baseURL       string
	// authStyle removed: credential type now encodes how auth is applied (Apply method).

	downstream   irc.DownstreamConverter
	upstream irc.UpstreamBackend
}

var _ Translator = (*GeminiTranslator)(nil)

func newGeminiTranslator(providerModel, baseURL string) *GeminiTranslator {
	return &GeminiTranslator{
		providerModel: providerModel,
		baseURL:       baseURL,
		downstream:         irc.AnthropicConverter{},
		upstream:       irc.NewBuiltinBackend(&irc.GeminiConverter{}),
	}
}

// NewGemini creates a translator pointed at the default Google AI Studio endpoint.
func NewGemini(providerModel string) *GeminiTranslator {
	return newGeminiTranslator(providerModel, defaultGeminiBase)
}

// NewGeminiWithBase creates a translator with a custom base URL — used in integration tests.
func NewGeminiWithBase(providerModel, baseURL string) *GeminiTranslator {
	return newGeminiTranslator(providerModel, baseURL)
}

// NewGeminiWithConfig creates a translator with a custom base URL and provider model.
// If baseURL is empty, defaults to the Google AI Studio endpoint.
// Auth style (query key vs bearer) is now encoded in the Credential passed to
// ToUpstream/ToUpstreamStream — QueryCredential for ?key=, HeaderCredential for Bearer.
func NewGeminiWithConfig(providerModel, baseURL string) *GeminiTranslator {
	if baseURL == "" {
		baseURL = defaultGeminiBase
	}
	return newGeminiTranslator(providerModel, baseURL)
}

func (t *GeminiTranslator) endpointURL() string {
	return fmt.Sprintf("%s/v1beta/models/%s:generateContent", t.baseURL, t.providerModel)
}

func (t *GeminiTranslator) streamEndpointURL() string {
	return fmt.Sprintf("%s/v1beta/models/%s:streamGenerateContent?alt=sse", t.baseURL, t.providerModel)
}

func (t *GeminiTranslator) ToUpstream(ctx context.Context, req *types.MessageRequest, credential cred.Credential) (*http.Request, error) {
	return t.buildHTTPRequest(ctx, req, t.endpointURL(), credential)
}

func (t *GeminiTranslator) ToUpstreamStream(ctx context.Context, req *types.MessageRequest, credential cred.Credential) (*http.Request, error) {
	return t.buildHTTPRequest(ctx, req, t.streamEndpointURL(), credential)
}

func (t *GeminiTranslator) buildHTTPRequest(ctx context.Context, req *types.MessageRequest, url string, credential cred.Credential) (*http.Request, error) {
	slog.Debug("building upstream request",
		"provider_model", t.providerModel,
		"messages", len(req.Messages),
		"max_tokens", req.MaxTokens,
		"has_system", len(req.System) > 0,
		"tools", len(req.Tools),
	)
	irReq, err := t.downstream.RequestToIR(req)
	if err != nil {
		return nil, fmt.Errorf("IR conversion: %w", err)
	}
	body, err := t.upstream.RequestToProvider(irReq)
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

func (t *GeminiTranslator) FromUpstream(resp *http.Response) (*types.MessageResponse, error) {
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read upstream response: %w", err)
	}
	irResp, err := t.upstream.ResponseToIR(body)
	if err != nil {
		return nil, err // includes *UpstreamError for body-level provider errors (G-05)
	}
	return t.downstream.ResponseFromIR(irResp, idgen.NewMsgID(), ""), nil
}

func (t *GeminiTranslator) StreamFromUpstream(ctx context.Context, resp *http.Response, msgID, modelAlias string) (<-chan types.SSEEvent, error) {
	slog.Debug("stream started", "provider_model", t.providerModel, "msg_id", msgID, "model_alias", modelAlias)
	out := make(chan types.SSEEvent, 32)
	go func() {
		defer resp.Body.Close()
		defer close(out)
		go func() { <-ctx.Done(); resp.Body.Close() }()
		// Gemini SSE → neutral IR stream events → Anthropic SSE. StreamToIR spawns
		// its own reader goroutine; StreamFromIR drains it synchronously into out.
		irEvents := t.upstream.StreamToIR(ctx, resp.Body)
		t.downstream.StreamFromIR(ctx, irEvents, out, msgID, modelAlias)
	}()
	return out, nil
}
