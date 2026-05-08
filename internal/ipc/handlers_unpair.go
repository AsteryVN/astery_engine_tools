// Package ipc — unpair handler.
//
// POST /v1/unpair tears down the local pairing state so the user can pair
// the daemon to a different cloud account / re-pair after a stale install.
//
// The renderer (Pairing.tsx state machine) calls this when the user clicks
// "Re-pair" after the daemon refused a fresh display code with 409
// already_paired.
//
// Order of operations matters:
//
//  1. Try the cloud DELETE first (PairingClient.Unpair).
//     - 200/204 + 401 → continue (cloud row is already cleared or session
//       expired so re-pair won't collide on installation_id).
//     - 5xx / transport error → if !force, return 502 cloud_unreachable
//       so the UI can ask the user; if force, log + continue.
//     - Other 4xx → return 502 (something we didn't expect; let the UI
//       surface the verbatim error and the user can retry).
//
//  2. Sweep local jobs (queued + running) to failed with reason 'unpaired'.
//     Preserves history — operator can still see why a job died via
//     job_events. Done BEFORE keystore.Clear so a partial failure leaves
//     the daemon in a consistent state ("identity still valid, but jobs
//     terminated"). Worst-case the user re-runs unpair and the second
//     pass is a no-op via SweepActiveToFailed's idempotent fast-path.
//
//  3. Clear keystore. After this point the daemon has no identity and
//     AlreadyPaired() returns false on every IPC call.
package ipc

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/AsteryVN/astery_engine_tools/internal/auth"
	"github.com/AsteryVN/astery_engine_tools/internal/jobqueue"
)

// unpairRequest is the renderer-facing body. `force` opts into the local
// clear when the cloud is unreachable.
type unpairRequest struct {
	Force bool `json:"force"`
}

// unpairResponse is the renderer-facing success body.
type unpairResponse struct {
	ClearedJobs int  `json:"cleared_jobs"`
	Forced      bool `json:"forced"`
}

// handleUnpair tears down the local pairing state. Wired in routes() at
// /v1/unpair (POST only).
//
// Returns 503 if PairDeps not configured (matches handlePair's contract).
func (s *Server) handleUnpair(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	if s.deps.Pair == nil || s.deps.Pair.PairingClient == nil || s.deps.Pair.Keystore == nil {
		writeErr(w, http.StatusServiceUnavailable, "pairing not configured")
		return
	}
	if s.deps.Store == nil {
		writeErr(w, http.StatusServiceUnavailable, "jobqueue not configured")
		return
	}

	// Body is optional — empty body = force=false.
	var body unpairRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			s.logger().Warn("unpair: decode body", "err", err)
			writeErr(w, http.StatusBadRequest, "invalid_json")
			return
		}
	}

	bundle, err := s.deps.Pair.Keystore.Load()
	if err != nil {
		// No stored session = nothing to unpair. Treat as 409 (matches the
		// AlreadyPaired short-circuit in handlePair so the UI's state
		// machine can branch on the same code).
		if errors.Is(err, auth.ErrNoSession) {
			writeErr(w, http.StatusConflict, "not_paired")
			return
		}
		s.logger().Error("unpair: load session", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal_error")
		return
	}

	// Step 1 — cloud DELETE.
	cloudErr := s.deps.Pair.PairingClient.Unpair(r.Context(), bundle.SessionJWT, bundle.HwFingerprint)
	if cloudErr != nil {
		if errors.Is(cloudErr, auth.ErrCloudUnreachable) {
			if !body.Force {
				s.logger().Warn("unpair: cloud unreachable, force=false", "err", cloudErr)
				writeErr(w, http.StatusBadGateway, "cloud_unreachable")
				return
			}
			// force=true: log + continue. We honor the user's explicit
			// "clear local anyway" decision, with the cost being the cloud
			// row remains until they manually revoke it in the web UI
			// before re-pairing on this machine.
			s.logger().Warn("unpair: cloud unreachable, force=true — clearing local anyway", "err", cloudErr)
		} else {
			// Non-cloud-unreachable cloud error (e.g. 4xx other than 401).
			// Don't force-clear past these — they signal a real problem
			// the user should see.
			s.logger().Warn("unpair: cloud rejected", "err", cloudErr)
			writeErr(w, http.StatusBadGateway, "cloud_rejected")
			return
		}
	}

	// Step 2 — sweep local active jobs to failed.
	cleared, err := s.deps.Store.SweepActiveToFailed(r.Context(), jobqueue.ReasonUnpaired)
	if err != nil {
		// Sweep failure is logged but does NOT abort the unpair — the user
		// asked to re-pair and we already told the cloud. Better to have a
		// minor local-state inconsistency than a stuck UI.
		s.logger().Error("unpair: sweep active jobs", "err", err)
	}

	// Step 3 — clear keystore.
	if err := s.deps.Pair.Keystore.Clear(); err != nil {
		s.logger().Error("unpair: clear keystore", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal_error")
		return
	}

	slog.InfoContext(r.Context(), "unpair: complete",
		"cleared_jobs", cleared,
		"forced", body.Force && cloudErr != nil)
	writeJSON(w, http.StatusOK, unpairResponse{
		ClearedJobs: cleared,
		Forced:      body.Force && cloudErr != nil,
	})
}
