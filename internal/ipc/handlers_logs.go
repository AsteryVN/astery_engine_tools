// Package ipc — SSE log streaming endpoint.
//
// GET /v1/logs/stream upgrades the response to text/event-stream and pumps
// every slog record published through the [observability.LogHub] until the
// client disconnects. The hub is process-wide; each subscriber gets its own
// buffered channel with drop-oldest backpressure.
package ipc

import (
	"encoding/json"
	"net/http"

	"github.com/AsteryVN/astery_engine_tools/internal/observability"
)

// handleLogsStream attaches the caller to observability.Hub() and writes
// SSE frames until r.Context() is done. Caller MUST be authenticated via
// the existing middleware; the bearer token gates the entire log feed.
func (s *Server) handleLogsStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "GET required")
		return
	}
	hub := observability.Hub()
	if hub == nil {
		writeErr(w, http.StatusServiceUnavailable, "log hub not initialized")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch, cancel := hub.Subscribe(r.Context())
	defer cancel()

	enc := json.NewEncoder(w)
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			// SSE frame: `data: <json>\n\n`. We hand-write the prefix
			// because json.Encoder appends its own newline; the second
			// newline below closes the SSE event.
			if _, err := w.Write([]byte("data: ")); err != nil {
				return
			}
			if err := enc.Encode(ev); err != nil {
				return
			}
			if _, err := w.Write([]byte("\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
