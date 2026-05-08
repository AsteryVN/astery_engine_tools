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
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
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
	Store      *jobqueue.Store
	Resources  *resources.Manager
	Pause      func()
	Resume     func()
	Paused     func() bool
	AppVersion string
	// Pair is optional — when nil POST /v1/pair returns 503 (pairing not
	// configured). Wired by main.go when keystore + cloud client exist.
	Pair *PairDeps
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
		if err := os.WriteFile(portFile, []byte(strconv.Itoa(port)), 0o600); err != nil {
			_ = l.Close()
			return nil, fmt.Errorf("write port file: %w", err)
		}
	}
	s := &Server{token: token, listener: l, deps: deps}
	mux := http.NewServeMux()
	s.routes(mux)
	s.httpServer = &http.Server{
		Handler:           sseQueryAuthShim(s.middleware(mux)),
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

// logger returns the configured default slog logger. Using this accessor
// (instead of slog.Default() directly at call sites) keeps handler code
// agnostic of the global and lets future tests inject a per-server logger.
func (s *Server) logger() *slog.Logger { return slog.Default() }

// ─── routes ──────────────────────────────────────────────────────────────

func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("/v1/status", s.handleStatus)
	mux.HandleFunc("/v1/jobs", s.handleJobsList)
	mux.HandleFunc("/v1/pause", s.handlePause)
	mux.HandleFunc("/v1/resume", s.handleResume)
	mux.HandleFunc("/v1/capabilities", s.handleCapabilities)
	// Per-job routes — handleJobByID dispatches GET /v1/jobs/:id and
	// POST /v1/jobs/:id/cancel under one mux entry.
	mux.HandleFunc("/v1/jobs/", s.handleJobByID)
	mux.HandleFunc("/v1/pair", s.handlePair)
	mux.HandleFunc("/v1/unpair", s.handleUnpair)
	mux.HandleFunc("/v1/logs/stream", s.handleLogsStream)
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

// tauriOriginAllowlist is the narrow set of Origin header values the Tauri
// renderer is permitted to attach. macOS/Linux send `tauri://localhost`,
// Windows sends `https://tauri.localhost`. Anything else (including
// localhost cloud frontends like http://localhost:3000) is rejected — the
// loopback IPC must NEVER answer browser-tab requests.
//
// Decision rationale (regression test #2): we keep reject-all as the
// default policy and only whitelist these two exact scheme/host pairs. If a
// future Tauri minor version changes the host, the test suite will catch
// it before release.
var tauriOriginAllowlist = map[string]struct{}{
	"tauri://localhost":       {},
	"https://tauri.localhost": {},
}

// sseQueryAuthShim rewrites a ?token=… query param into an Authorization
// header for /v1/logs/stream only. The browser EventSource API has no way
// to set custom request headers, so SSE clients must pass the bearer via
// the URL. We promote it to a header here so the regular middleware does
// the actual auth check, then strip it from the URL so it never reaches
// the handler or any future access log. Loopback bind + per-boot token
// rotation bound the exposure.
func sseQueryAuthShim(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/logs/stream" && r.Header.Get("Authorization") == "" {
			q := r.URL.Query()
			if t := q.Get("token"); t != "" {
				r.Header.Set("Authorization", "Bearer "+t)
				q.Del("token")
				r.URL.RawQuery = q.Encode()
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Anti-CSRF: reject any cross-origin request on the loopback
		// server. Tauri renderer Origins are explicitly whitelisted.
		origin := r.Header.Get("Origin")
		originAllowed := origin == "" || s.allowOrigin
		if origin != "" && !s.allowOrigin {
			if _, ok := tauriOriginAllowlist[origin]; ok {
				originAllowed = true
			}
		}
		if !originAllowed {
			writeErr(w, http.StatusForbidden, "origin not allowed")
			return
		}

		// CORS response headers for allowlisted Tauri renderer origins.
		// Required because the renderer page origin (tauri://localhost on
		// macOS/Linux, https://tauri.localhost on Windows) is cross-origin
		// to the loopback http://127.0.0.1:<port> server, and any fetch
		// carrying an Authorization header is a non-simple request that
		// triggers a CORS preflight in WebKit/Blink. Without these headers
		// the renderer's fetch() rejects with TypeError "Load failed"
		// before the actual request is ever sent.
		if origin != "" {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Vary", "Origin")
			h.Set("Access-Control-Allow-Credentials", "true")
		}

		// Short-circuit CORS preflight: respond 204 with the negotiated
		// method/header allow-list, skipping the bearer check (preflights
		// never carry the Authorization header by design).
		if r.Method == http.MethodOptions && origin != "" {
			h := w.Header()
			if reqHeaders := r.Header.Get("Access-Control-Request-Headers"); reqHeaders != "" {
				h.Set("Access-Control-Allow-Headers", reqHeaders)
			} else {
				h.Set("Access-Control-Allow-Headers", "Authorization, Accept, Content-Type")
			}
			h.Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
			h.Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
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
