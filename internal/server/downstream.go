package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"miroxy/core/router"
	"miroxy/internal/pipeline"
	"miroxy/internal/types"
)

// handleMessages is the downstream handler: it parses the client's Anthropic-format
// request, drives the plugin pipeline (which invokes the upstream executor as its
// terminal plugin), then renders the IR response back to the client.
//
// This is the downstream boundary — everything above the pipeline.Run call deals with
// the client protocol; everything below deals with delivering the response.
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	var req types.MessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		slog.Debug("request decode failed", "error", err)
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid request body: "+err.Error())
		return
	}
	slog.Debug("request decoded",
		"model", req.Model, "stream", req.Stream, "messages", len(req.Messages))

	req.NormalizeSystem()
	if err := req.Validate(); err != nil {
		slog.Debug("request validation failed", "model", req.Model, "error", err)
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	entry, ok := s.cfg.Load().LookupModel(req.Model)
	if !ok {
		slog.Debug("model not found in config", "model", req.Model)
		writeError(w, http.StatusBadRequest, "invalid_request_error",
			fmt.Sprintf("unknown model %q — see GET /v1/models for available models", req.Model))
		return
	}
	if entry.ModelName != req.Model {
		slog.Debug("model routed to default", "requested", req.Model, "default", entry.ModelName)
	}
	slog.Debug("model resolved",
		"alias", req.Model, "provider_model", entry.ProviderModel, "provider", entry.Provider)

	if req.Stream {
		if _, ok := w.(http.Flusher); !ok {
			slog.Debug("streaming not supported: ResponseWriter does not implement http.Flusher")
			writeError(w, http.StatusInternalServerError, "api_error",
				"streaming not supported by server configuration")
			return
		}
	}

	// Load routing state atomically — consistent snapshot across reload.
	rt := s.routing.Load()
	c := pipeline.NewContext(r.Context(), &req, router.RouteTarget{
		Invisible: entry.Invisible,
		Model: router.ModelInfo{
			Name:          entry.ModelName,
			ProviderModel: entry.ProviderModel,
			Provider:      entry.Provider,
		},
		Selector:   rt.selectors[entry.ModelName],
		Timeout:    rt.timeouts[entry.ModelName],
		Dispatcher: s.dispatcher,
	})

	slog.Debug("pipeline starting", "model", req.Model, "stream", req.Stream)
	if err := s.pipe.Run(c); err != nil {
		var pe *pipeline.PipelineError
		if errors.As(err, &pe) {
			slog.Debug("pipeline error",
				"status", pe.Status, "type", pe.ErrType, "msg", pe.Msg, "raw_body", pe.RawBody != nil)
			if pe.RawBody != nil {
				ct := pe.ContentType
				if ct == "" {
					ct = "application/json"
				}
				w.Header().Set("Content-Type", ct)
				w.WriteHeader(pe.Status)
				w.Write(pe.RawBody) //nolint:errcheck
			} else {
				writeError(w, pe.Status, pe.ErrType, pe.Msg)
			}
		} else {
			slog.Debug("pipeline unexpected error", "error", err)
			writeError(w, http.StatusInternalServerError, "api_error", err.Error())
		}
		return
	}

	// Delivery: render IR response back to the downstream client.
	if c.StreamSrc() != nil {
		flusher := w.(http.Flusher) // safe: Flusher check above precedes pipeline.Run
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		slog.Debug("stream delivery starting", "model", req.Model)

		var streamErr error
		for event := range c.StreamSrc() {
			if err := writeSSEEvent(w, event.Event, event.Data); err != nil {
				streamErr = err
				break
			}
			flusher.Flush()
		}
		slog.Debug("stream delivery done", "model", req.Model, "error", streamErr)
		c.ReleaseUpstream(streamErr)
		return
	}

	slog.Debug("delivering non-stream response", "model", req.Model)
	writeJSON(w, http.StatusOK, c.Response)
}

// --- Response helpers ---

func writeSSEEvent(w io.Writer, event string, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal SSE event %s: %w", event, err)
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
	return err
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, errType, msg string) {
	writeJSON(w, status, types.ErrorResponse{
		Type:  "error",
		Error: types.ErrorBody{Type: errType, Message: msg},
	})
}
