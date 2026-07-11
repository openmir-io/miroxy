package downstream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	coredown "miroxy/core/downstream"
	"miroxy/internal/idgen"
	"miroxy/internal/irc"
	"miroxy/internal/types"
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
func (a *OpenAIAdapter) Decode(r *http.Request) (*types.MessageRequest, error) {
	var body []byte
	var err error
	if body, err = readBody(r); err != nil {
		return nil, err
	}
	req, err := irc.OAIBodyToAnthropicRequest(body)
	if err != nil {
		return nil, fmt.Errorf("invalid request body: %w", err)
	}
	return req, nil
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
func (a *OpenAIAdapter) WriteResponseAsStream(ctx context.Context, w http.ResponseWriter, resp *types.MessageResponse) {
	// Build a synthetic one-shot SSE stream and let the normal WriteStream path handle it.
	ch := make(chan types.SSEEvent, 1)
	// We can't use irc conversion here — just emit a plain done message.
	// OpenAI clients accept a single data:[DONE] after the content chunks.
	flusher, ok := w.(http.Flusher)
	if !ok {
		a.WriteResponse(w, resp)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	close(ch)

	msgID := "chatcmpl-" + resp.ID
	var text string
	for _, b := range resp.Content {
		if b.Type == "text" {
			text += b.Text
		}
	}
	// Single content chunk delta.
	type delta struct{ Content string `json:"content"` }
	type choice struct {
		Index        int    `json:"index"`
		Delta        delta  `json:"delta"`
		FinishReason any    `json:"finish_reason"`
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
	emit(chunk{ID: msgID, Object: "chat.completion.chunk", Model: resp.Model,
		Choices: []choice{{Delta: delta{Content: text}}}})
	emit(chunk{ID: msgID, Object: "chat.completion.chunk", Model: resp.Model,
		Choices: []choice{{FinishReason: "stop"}}})
	_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func (a *OpenAIAdapter) WriteResponse(w http.ResponseWriter, resp *types.MessageResponse) {
	// Model name for the OAI envelope comes from the response itself.
	body := irc.AnthropicToOAIResponseBody(resp, resp.Model)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// WriteStream delivers OpenAI-format SSE events to the client.
func (a *OpenAIAdapter) WriteStream(ctx context.Context, w http.ResponseWriter, req *types.MessageRequest, src <-chan types.SSEEvent) error {
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
	irc.StreamAnthropicSSEToOAI(ctx, src, w, flusher, msgID, req.Model)
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
