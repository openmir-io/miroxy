package downstream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	coredown "miroxy/core/downstream"
	"miroxy/core/ir"
	"miroxy/internal/idgen"
	"miroxy/internal/wireformat"
)

var _ coredown.DownstreamAdapter = (*OpenAIAdapter)(nil)

// OpenAIAdapter handles POST /v1/chat/completions.
// Clients: Codex, OpenCode, LiteLLM, openai-python, any OpenAI-SDK client.
type OpenAIAdapter struct{}

func (a *OpenAIAdapter) Protocol() string { return "openai" }
func (a *OpenAIAdapter) Path() string     { return "/v1/chat/completions" }

// Decode converts an OpenAI chat/completions request to the canonical IR.
// System messages inside the messages array are extracted to the top-level
// system field by OAIBodyToAnthropicRequest (standard OpenAI convention).
// The Anthropic-shaped intermediate here is an internal implementation
// detail of this one parsing step, not a pipeline-wide privilege — see
// docs/dev/DESIGNLOG.md, 2026-07-19.
func (a *OpenAIAdapter) Decode(r *http.Request) (*ir.IRRequest, string, error) {
	body, err := readBody(r)
	if err != nil {
		return nil, "", err
	}
	anthropicReq, err := wireformat.OAIBodyToAnthropicRequest(body)
	if err != nil {
		return nil, "", fmt.Errorf("invalid request body: %w", err)
	}
	irReq, err := (wireformat.AnthropicConverter{}).RequestToIR(anthropicReq)
	if err != nil {
		return nil, "", fmt.Errorf("request to IR: %w", err)
	}
	return irReq, anthropicReq.Model, nil
}

// EncodeRequest is the reverse of Decode — direct IR→OpenAI-wire, reusing
// the same conversion real upstream OpenAI dispatch already uses.
func (a *OpenAIAdapter) EncodeRequest(req *ir.IRRequest, model string) ([]byte, error) {
	return wireformat.NewOpenAIConverter(model).RequestToProvider(req)
}

func (a *OpenAIAdapter) WriteError(w http.ResponseWriter, status int, errType, msg string) {
	type oaiErr struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	}
	type envelope struct {
		Error oaiErr `json:"error"`
	}
	writeJSON(w, status, envelope{Error: oaiErr{Message: msg, Type: errType}})
}

// WriteResponseAsStream wraps the response in OpenAI chat completions SSE.
func (a *OpenAIAdapter) WriteResponseAsStream(ctx context.Context, w http.ResponseWriter, resp *ir.IRResponse, msgID, model string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		a.WriteResponse(w, resp, msgID, model)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	chatID := "chatcmpl-" + msgID
	var text string
	for _, b := range resp.Content {
		if b.Text != nil {
			text += b.Text.Text
		}
	}
	// Single content chunk delta.
	type delta struct {
		Content string `json:"content"`
	}
	type choice struct {
		Index        int   `json:"index"`
		Delta        delta `json:"delta"`
		FinishReason any   `json:"finish_reason"`
	}
	type chunk struct {
		ID      string   `json:"id"`
		Object  string   `json:"object"`
		Model   string   `json:"model"`
		Choices []choice `json:"choices"`
	}
	emit := func(c chunk) {
		b, _ := json.Marshal(c)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}
	emit(chunk{ID: chatID, Object: "chat.completion.chunk", Model: model,
		Choices: []choice{{Delta: delta{Content: text}}}})
	emit(chunk{ID: chatID, Object: "chat.completion.chunk", Model: model,
		Choices: []choice{{FinishReason: "stop"}}})
	_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func (a *OpenAIAdapter) WriteResponse(w http.ResponseWriter, resp *ir.IRResponse, msgID, model string) {
	body := wireformat.IRResponseToOAIBody(resp, msgID, model)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// WriteStream delivers OpenAI-format SSE events to the client.
func (a *OpenAIAdapter) WriteStream(ctx context.Context, w http.ResponseWriter, model string, src <-chan ir.StreamEvent) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("streaming not supported by server configuration")
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	msgID := "chatcmpl-" + idgen.NewMsgID()
	wireformat.StreamIRToOAI(ctx, src, w, flusher, msgID, model)
	return nil
}

func readBody(r *http.Request) ([]byte, error) {
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, err := r.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	return buf, nil
}
