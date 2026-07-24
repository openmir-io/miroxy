package server

import (
	"encoding/json"
	"net/http"

	"miroxy/internal/types"
)

// --- Response helpers ---
//
// handleMessages/handleChatCompletions (the original downstream handlers)
// were superseded by the generic makeHandler(DownstreamAdapter) in
// server.go — see its doc comment. writeJSON/writeError remain in use by
// direct.go and server.go.

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
