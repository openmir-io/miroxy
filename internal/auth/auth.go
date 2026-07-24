package auth

import (
	"log/slog"
	"net/http"
	"strings"
)

// Validator checks inbound bearer tokens against an allowlist.
type Validator struct {
	allowed map[string]struct{}
}

// NewValidator creates a Validator from a slice of allowed API keys.
// If keys is empty, all requests pass (open mode — useful for local dev).
func NewValidator(keys []string) *Validator {
	m := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		if k != "" {
			m[k] = struct{}{}
		}
	}
	return &Validator{allowed: m}
}

// Middleware returns an http.Handler that rejects requests with invalid or missing keys.
func (v *Validator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !v.valid(r) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid or missing api-key"}}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requestKey extracts the client-supplied key, accepting either scheme a real
// downstream client sends: OpenAI/Bearer's "Authorization: Bearer <key>" or
// Anthropic's "x-api-key: <key>". Miroxy speaks both protocols, so it must
// accept whichever auth convention that protocol's own clients actually use.
func requestKey(r *http.Request) (key string, headerPresent bool) {
	if auth := r.Header.Get("Authorization"); auth != "" {
		key, _ = strings.CutPrefix(auth, "Bearer ")
		return key, true
	}
	if key := r.Header.Get("x-api-key"); key != "" {
		return key, true
	}
	return "", false
}

func (v *Validator) valid(r *http.Request) bool {
	if len(v.allowed) == 0 {
		return true // open mode
	}
	key, headerPresent := requestKey(r)
	if key == "" {
		slog.Debug("auth: rejected — missing or malformed Authorization/x-api-key header",
			"header_present", headerPresent, "path", r.URL.Path)
		return false
	}
	_, found := v.allowed[key]
	if !found {
		slog.Debug("auth: rejected — key not in allowlist", "path", r.URL.Path)
	}
	return found
}
