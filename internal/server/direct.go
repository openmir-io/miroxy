package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"miroxy/core/cred"
	"miroxy/internal/auth"
	"miroxy/internal/config"
)

// DirectServer is a pass-through proxy: it forwards every request to the
// real upstream without any protocol translation and writes a JSONL capture
// record for each exchange. Use it to build golden test fixtures and to
// inspect exactly what AI agent clients send and what providers return.
//
// Usage:  ./miroxy direct [--dump captures.jsonl]
//
// Config: reuses the normal models config — each model entry supplies
// api_base, auth_style, and the first key in key_pool as the upstream key.
type DirectServer struct {
	mux    *http.ServeMux
	routes map[string]directRoute
	client *http.Client
	dumpW  io.Writer
	mu     sync.Mutex // serialises dump writes
}

type directRoute struct {
	upstreamBase string         // e.g. "https://api.anthropic.com"
	credential   cred.Credential // typed auth — Apply() attaches it to the upstream request
}

// captureRecord is one JSONL line in the dump file.
type captureRecord struct {
	Timestamp  string          `json:"ts"`
	Model      string          `json:"model"`
	Stream     bool            `json:"stream"`
	ReqBody    json.RawMessage `json:"req_body"`
	Status     int             `json:"status"`
	ResBody    json.RawMessage `json:"res_body,omitempty"`   // non-streaming
	ResEvents  []string        `json:"res_events,omitempty"` // raw SSE lines
	DurationMS int64           `json:"duration_ms"`
	Error      string          `json:"error,omitempty"`
}

// NewDirect creates a DirectServer from the standard config.
// If dumpPath is non-empty, capture records are appended to that file;
// otherwise they are written to stdout.
func NewDirect(cfg *config.Config, dumpPath string) (*DirectServer, error) {
	routes := make(map[string]directRoute, len(cfg.ModelRoutes))
	for _, m := range cfg.ModelRoutes {
		if m.APIBase == "" {
			slog.Warn("direct: skipping model — no api_base configured", "model", m.ModelName)
			continue
		}
		var credential cred.Credential
		if len(m.KeyPool.Keys) > 0 {
			credential = credentialFromConfig(m.KeyPool.Keys[0].Key, m.AuthStyle)
		}
		routes[m.ModelName] = directRoute{
			upstreamBase: strings.TrimRight(m.APIBase, "/"),
			credential:   credential,
		}
	}

	var dumpW io.Writer = os.Stdout
	if dumpPath != "" {
		f, err := os.OpenFile(dumpPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return nil, fmt.Errorf("open dump file %q: %w", dumpPath, err)
		}
		dumpW = f
		slog.Info("direct: writing captures", "file", dumpPath)
		// f intentionally not closed — it lives for the process lifetime.
	}

	ds := &DirectServer{
		mux:    http.NewServeMux(),
		routes: routes,
		client: &http.Client{Timeout: 10 * time.Minute},
		dumpW:  dumpW,
	}

	authMW := auth.NewValidator(cfg.Auth.AllowedKeys).Middleware
	ds.mux.Handle("POST /v1/messages", authMW(http.HandlerFunc(ds.handleMessages)))
	ds.mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	models := make([]string, 0, len(routes))
	for m := range routes {
		models = append(models, m)
	}
	slog.Info("direct: ready", "routes", models)

	return ds, nil
}

// Handler returns the http.Handler for use with net/http.
func (ds *DirectServer) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		slog.Debug("request in", "method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr)
		rec := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
		ds.mux.ServeHTTP(rec, r)
		slog.Info("request handled", "method", r.Method, "path", r.URL.Path,
			"status", rec.status, "duration_ms", time.Since(start).Milliseconds())
	})
}

func (ds *DirectServer) handleMessages(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	// Slurp the body so we can both dump it and replay it to the upstream.
	reqBody, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "api_error", "failed to read request body")
		return
	}

	// Minimal parse: just model + stream. Everything else forwards verbatim.
	var peek struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(reqBody, &peek); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body")
		ds.writeDump(captureRecord{
			Timestamp: now(), ReqBody: json.RawMessage(reqBody),
			Status: http.StatusBadRequest, Error: "invalid JSON: " + err.Error(),
		})
		return
	}

	route, ok := ds.routes[peek.Model]
	if !ok {
		msg := fmt.Sprintf("model %q not in config — add api_base + key to config.yaml", peek.Model)
		writeError(w, http.StatusBadRequest, "invalid_request_error", msg)
		ds.writeDump(captureRecord{
			Timestamp: now(), Model: peek.Model, Stream: peek.Stream,
			ReqBody: json.RawMessage(reqBody), Status: http.StatusBadRequest, Error: msg,
		})
		return
	}

	// Build upstream request.
	upstreamURL := route.upstreamBase + "/v1/messages"
	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstreamURL, bytes.NewReader(reqBody))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "failed to build upstream request")
		return
	}

	// Forward safe client headers (strip auth and hop-by-hop).
	skipHeaders := map[string]bool{
		"authorization":  true,
		"x-api-key":      true,
		"host":           true,
		"content-length": true,
		"connection":     true,
	}
	for k, vv := range r.Header {
		if !skipHeaders[strings.ToLower(k)] {
			for _, v := range vv {
				upReq.Header.Add(k, v)
			}
		}
	}
	upReq.Header.Set("Content-Type", "application/json")

	// Attach upstream auth.
	if route.credential != nil {
		if err := route.credential.Apply(upReq); err != nil {
			writeError(w, http.StatusInternalServerError, "api_error", "apply credential: "+err.Error())
			return
		}
	}

	resp, err := ds.client.Do(upReq)
	if err != nil {
		writeError(w, http.StatusBadGateway, "api_error", "upstream request failed: "+err.Error())
		ds.writeDump(captureRecord{
			Timestamp: now(), Model: peek.Model, Stream: peek.Stream,
			ReqBody: json.RawMessage(reqBody), Status: http.StatusBadGateway,
			DurationMS: time.Since(start).Milliseconds(), Error: err.Error(),
		})
		return
	}
	defer resp.Body.Close()

	rec := captureRecord{
		Timestamp: now(),
		Model:     peek.Model,
		Stream:    peek.Stream,
		ReqBody:   json.RawMessage(reqBody),
		Status:    resp.StatusCode,
	}

	// Forward upstream response headers to client.
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}

	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "text/event-stream") {
		ds.forwardStream(w, r, resp, start, &rec)
	} else {
		ds.forwardNonStream(w, resp, start, &rec)
	}
}

func (ds *DirectServer) forwardNonStream(w http.ResponseWriter, resp *http.Response, start time.Time, rec *captureRecord) {
	body, err := io.ReadAll(resp.Body)
	rec.DurationMS = time.Since(start).Milliseconds()
	if err != nil {
		rec.Error = "read upstream body: " + err.Error()
		ds.writeDump(*rec)
		http.Error(w, `{"error":"failed to read upstream response"}`, http.StatusBadGateway)
		return
	}
	rec.ResBody = json.RawMessage(body)
	ds.writeDump(*rec)
	slog.Info("direct: captured", "model", rec.Model, "status", rec.Status, "duration_ms", rec.DurationMS)

	w.WriteHeader(resp.StatusCode)
	w.Write(body) //nolint:errcheck
}

func (ds *DirectServer) forwardStream(w http.ResponseWriter, r *http.Request, resp *http.Response, start time.Time, rec *captureRecord) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		rec.Error = "ResponseWriter does not support streaming"
		rec.DurationMS = time.Since(start).Milliseconds()
		ds.writeDump(*rec)
		http.Error(w, `{"error":"streaming not supported"}`, http.StatusInternalServerError)
		return
	}

	w.WriteHeader(resp.StatusCode)

	var lines []string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		select {
		case <-r.Context().Done():
			goto done
		default:
		}
		line := scanner.Text()
		lines = append(lines, line)
		fmt.Fprintln(w, line)
		flusher.Flush()
	}

done:
	rec.ResEvents = lines
	rec.DurationMS = time.Since(start).Milliseconds()
	if err := scanner.Err(); err != nil {
		rec.Error = "stream read: " + err.Error()
	}
	ds.writeDump(*rec)
	slog.Info("direct: captured stream", "model", rec.Model, "status", rec.Status,
		"events", len(lines), "duration_ms", rec.DurationMS)
}

func (ds *DirectServer) writeDump(rec captureRecord) {
	b, err := json.Marshal(rec)
	if err != nil {
		slog.Warn("direct: failed to marshal capture record", "error", err)
		return
	}
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if _, err := fmt.Fprintf(ds.dumpW, "%s\n", b); err != nil {
		slog.Warn("direct: failed to write dump record", "error", err)
	}
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }
