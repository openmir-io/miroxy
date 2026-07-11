package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"miroxy/internal/dump"
)

// transparentHandler returns an http.Handler that forwards every request
// verbatim to upstreamBase, replacing only the Authorization header with apiKey.
// When store is non-nil each request/response pair is written to it with a
// shared trace_id so they can be correlated later.
func transparentHandler(upstreamBase, apiKey string, store dump.Store) http.Handler {
	target, err := url.Parse(upstreamBase)
	if err != nil {
		slog.Error("transparent: invalid upstream URL", "url", upstreamBase, "error", err)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "transparent proxy misconfigured", http.StatusInternalServerError)
		})
	}

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			// Rewrite target — client URL stays localhost:PORT, only internal target changes.
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host

			// Strip any path prefix miroxy itself added (none currently, but defensive).
			if target.Path != "" && target.Path != "/" {
				req.URL.Path = strings.TrimSuffix(target.Path, "/") + req.URL.Path
			}

			// Inject upstream API key.
			if apiKey != "" {
				req.Header.Set("Authorization", "Bearer "+apiKey)
				req.Header.Set("x-api-key", apiKey) // Anthropic style
			}
			req.Header.Del("X-Forwarded-For")
		},

		ModifyResponse: func(resp *http.Response) error {
			if store == nil {
				return nil
			}
			traceID := dump.TraceIDFrom(resp.Request.Context())
			if traceID == "" {
				return nil
			}
			// Capture response body (non-SSE only — SSE bodies are streaming).
			ct := resp.Header.Get("Content-Type")
			if strings.Contains(ct, "text/event-stream") {
				// For SSE responses wrap body to tap the stream.
				resp.Body = &sseTap{
					ReadCloser: resp.Body,
					store:      store,
					traceID:    traceID,
				}
				return nil
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				return err
			}
			resp.Body = io.NopCloser(strings.NewReader(string(body)))
			_ = store.Write(dump.Record{
				TraceID: traceID,
				Dir:     dump.DirResponse,
				Body:    json.RawMessage(body),
				Status:  resp.StatusCode,
			})
			return nil
		},
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		traceID := dump.TraceIDFrom(r.Context())
		if store != nil && traceID != "" {
			// Capture raw request body before proxy reads it.
			body, err := io.ReadAll(r.Body)
			if err == nil {
				r.Body = io.NopCloser(strings.NewReader(string(body)))
				_ = store.Write(dump.Record{
					TraceID:  traceID,
					Dir:      dump.DirRawRequest,
					Protocol: r.URL.Path,
					Body:     json.RawMessage(body),
				})
			}
		}
		proxy.ServeHTTP(w, r)
	})
}

// sseTap wraps an SSE response body and writes each event to the DumpStore.
type sseTap struct {
	io.ReadCloser
	store   dump.Store
	traceID string
	buf     strings.Builder
}

func (t *sseTap) Read(p []byte) (int, error) {
	n, err := t.ReadCloser.Read(p)
	if n > 0 {
		chunk := string(p[:n])
		t.buf.WriteString(chunk)
		t.flushEvents()
	}
	return n, err
}

func (t *sseTap) flushEvents() {
	for {
		s := t.buf.String()
		idx := strings.Index(s, "\n\n")
		if idx < 0 {
			return
		}
		block := s[:idx]
		t.buf.Reset()
		t.buf.WriteString(s[idx+2:])

		var eventType, data string
		for _, line := range strings.Split(block, "\n") {
			if after, ok := strings.CutPrefix(line, "event: "); ok {
				eventType = after
			} else if after, ok := strings.CutPrefix(line, "data: "); ok {
				data = after
			}
		}
		if data == "" || data == "[DONE]" {
			continue
		}
		_ = t.store.Write(dump.Record{
			TraceID: t.traceID,
			Dir:     dump.DirResponse,
			Event:   eventType,
			Data:    json.RawMessage(data),
		})
	}
}
