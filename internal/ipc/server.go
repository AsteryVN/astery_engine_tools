// Package ipc is the localhost HTTP JSON server used by the optional Tauri
// shell to control the daemon: status, jobs list, pause/resume, pairing
// proxy, log stream. Bound to 127.0.0.1; Bearer-token auth from
// <data-dir>/ipc.token. Origin header rejected (anti-CSRF on browser pages
// running on the same machine).
package ipc

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/AsteryVN/astery_engine_tools/internal/jobqueue"
	"github.com/AsteryVN/astery_engine_tools/internal/runtime/resources"
)

// Server is the loopback IPC HTTP server.
type Server struct {
	token       string
	listener    net.Listener
	httpServer  *http.Server
	deps        Deps
	allowOrigin bool // true = accept any Origin (debug only)
}

// Deps bundles read-only handles the IPC server exposes.
type Deps struct {
	Store     *jobqueue.Store
	Resources *resources.Manager
	Pause     func()
	Resume    func()
	Paused    func() bool
	AppVersion string
}

// Listen binds to 127.0.0.1:listen (e.g. ":0" for a random port). Writes
// the chosen port to <data-dir>/ipc.port for the Tauri shell to discover.
func Listen(listen, token, portFile string, deps Deps) (*Server, error) {
	if !strings.HasPrefix(listen, "127.0.0.1") && !strings.HasPrefix(listen, ":") {
		return nil, fmt.Errorf("ipc listen MUST start with 127.0.0.1 or :")
	}
	if strings.HasPrefix(listen, ":") {
		listen = "127.0.0.1" + listen
	}
	l, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, fmt.Errorf("ipc listen: %w", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if portFile != "" {
		if err := os.WriteFile(portFile, []byte(fmt.Sprintf("%d", port)), 0o600); err != nil {
			_ = l.Close()
			return nil, fmt.Errorf("write port file: %w", err)
		}
	}
	s := &Server{token: token, listener: l, deps: deps}
	mux := http.NewServeMux()
	s.routes(mux)
	s.httpServer = &http.Server{
		Handler:           s.middleware(mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      0, // SSE-friendly; rule streaming-write-timeout
		IdleTimeout:       60 * time.Second,
	}
	return s, nil
}

// Serve blocks until ctx is cancelled or the server errors out.
func (s *Server) Serve(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() { errCh <- s.httpServer.Serve(s.listener) }()
	select {
	case <-ctx.Done():
		shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.httpServer.Shutdown(shCtx)
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}

// Addr returns the bound TCP address.
func (s *Server) Addr() string { return s.listener.Addr().String() }

// ─── routes ──────────────────────────────────────────────────────────────

func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("/v1/status", s.handleStatus)
	mux.HandleFunc("/v1/jobs", s.handleJobsList)
	mux.HandleFunc("/v1/pause", s.handlePause)
	mux.HandleFunc("/v1/resume", s.handleResume)
	mux.HandleFunc("/v1/capabilities", s.handleCapabilities)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"app_version": s.deps.AppVersion,
		"paused":      s.deps.Paused(),
		"active":      s.deps.Resources.Active(),
		"timestamp":   time.Now().UTC(),
	})
}

func (s *Server) handleJobsList(w http.ResponseWriter, r *http.Request) {
	limit := atoiOr(r.URL.Query().Get("limit"), 50)
	offset := atoiOr(r.URL.Query().Get("offset"), 0)
	status := r.URL.Query().Get("status")
	jobs, err := s.deps.Store.ListJobs(r.Context(), status, limit, offset)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs})
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	s.deps.Pause()
	writeJSON(w, http.StatusOK, map[string]any{"paused": true})
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	s.deps.Resume()
	writeJSON(w, http.StatusOK, map[string]any{"paused": false})
}

func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	snap := s.deps.Resources.Probe(r.Context())
	writeJSON(w, http.StatusOK, snap)
}

// ─── middleware: bearer auth + Origin reject ────────────────────────────

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Anti-CSRF: reject any cross-origin request on the loopback server.
		if origin := r.Header.Get("Origin"); origin != "" && !s.allowOrigin {
			writeErr(w, http.StatusForbidden, "origin not allowed")
			return
		}
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if strings.TrimSpace(got) != s.token {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ─── helpers ─────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": message})
}

func atoiOr(s string, fallback int) int {
	n := 0
	if s == "" {
		return fallback
	}
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return fallback
		}
		n = n*10 + int(ch-'0')
	}
	return n
}
