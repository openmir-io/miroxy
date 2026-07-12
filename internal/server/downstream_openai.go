package server

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"miroxy/core/router"
	"miroxy/internal/idgen"
	"miroxy/internal/irc"
	"miroxy/internal/pipeline"
)

// handleChatCompletions handles POST /v1/chat/completions — the OpenAI-compatible
// downstream endpoint. Any client that speaks OpenAI Chat Completions format can
// use miroxy as a drop-in proxy: openai-python, LiteLLM, Codex, etc.
//
// Request flow:
//  1. Parse OpenAI request body → convert to Anthropic MessageRequest (IR bridge)
//  2. Route via existing pipeline (same as /v1/messages)
//  3. Convert response back to OpenAI format before writing to client
//
// The client_protocol field in config selects this handler (when "openai").
// Model name lookup uses the same model_name alias as the Anthropic endpoint.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeOAIError(w, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
		return
	}

	req, err := irc.OAIBodyToAnthropicRequest(body)
	if err != nil {
		slog.Debug("oai request parse failed", "error", err)
		writeOAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	slog.Debug("oai request decoded",
		"model", req.Model, "stream", req.Stream, "messages", len(req.Messages))

	entry, ok := s.cfg.Load().LookupModel(req.Model)
	if !ok {
		slog.Debug("oai model not found", "model", req.Model)
		writeOAIError(w, http.StatusBadRequest, "invalid_request_error",
			fmt.Sprintf("unknown model %q — see GET /v1/models for available models", req.Model))
		return
	}
	slog.Debug("oai model resolved",
		"alias", req.Model, "upstream_model", entry.UpstreamModel, "provider", entry.ProviderRef)

	if req.Stream {
		if _, ok := w.(http.Flusher); !ok {
			writeOAIError(w, http.StatusInternalServerError, "api_error",
				"streaming not supported by server configuration")
			return
		}
	}

	rt := s.routing.Load()
	c := pipeline.NewContext(r.Context(), req, router.RouteTarget{
		Invisible: entry.Invisible,
		Model: router.ModelInfo{
			Name:          entry.ModelName,
			UpstreamModel: entry.UpstreamModel,
			Provider:      entry.ProviderRef,
		},
		Selector:   rt.selectors[entry.ModelName],
		Timeout:    rt.timeouts[entry.ModelName],
		Dispatcher: s.dispatcher,
	})

	slog.Debug("oai pipeline starting", "model", req.Model, "stream", req.Stream)
	if err := s.pipe.Run(c); err != nil {
		var pe *pipeline.PipelineError
		if errors.As(err, &pe) {
			slog.Debug("oai pipeline error",
				"status", pe.Status, "type", pe.ErrType, "msg", pe.Msg)
			if pe.RawBody != nil && entry.Invisible {
				ct := pe.ContentType
				if ct == "" {
					ct = "application/json"
				}
				w.Header().Set("Content-Type", ct)
				w.WriteHeader(pe.Status)
				w.Write(pe.RawBody) //nolint:errcheck
			} else {
				writeOAIError(w, pe.Status, pe.ErrType, pe.Msg)
			}
		} else {
			slog.Debug("oai pipeline unexpected error", "error", err)
			writeOAIError(w, http.StatusInternalServerError, "api_error", err.Error())
		}
		return
	}

	msgID := "chatcmpl-" + idgen.NewMsgID()

	if c.StreamSrc() != nil {
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		slog.Debug("oai stream delivery starting", "model", req.Model, "msg_id", msgID)

		irc.StreamAnthropicSSEToOAI(r.Context(), c.StreamSrc(), w, flusher, msgID, req.Model)
		c.ReleaseUpstream(nil)
		slog.Debug("oai stream delivery done", "model", req.Model)
		return
	}

	slog.Debug("oai delivering non-stream response", "model", req.Model)
	respBody := irc.AnthropicToOAIResponseBody(c.Response, req.Model)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(respBody) //nolint:errcheck
}

// writeOAIError writes an OpenAI-format error response.
// OpenAI error envelope: {"error":{"message":"...","type":"...","code":null}}
func writeOAIError(w http.ResponseWriter, status int, errType, msg string) {
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
