// Package ipc — per-job IPC handlers (GetJob, CancelJob).
//
// These complement the existing GET /v1/jobs list endpoint and let the
// Tauri shell drill into a single job row + cancel an in-flight job.
package ipc

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/AsteryVN/astery_engine_tools/internal/jobqueue"
)

// handleJobByID dispatches GET /v1/jobs/:id and POST /v1/jobs/:id/cancel
// off a single mux entry. Path layout: /v1/jobs/{id}[/cancel].
//
// NOTE: cancelJob only flips the persisted job status to "cancelled" — it
// does NOT interrupt an executor goroutine that has already claimed and
// started the job. Mid-flight cancellation is a follow-up task; today the
// renderer should treat 200 as "scheduled-for-cancel" rather than "stopped
// immediately".
func (s *Server) handleJobByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/jobs/")
	if rest == "" || rest == r.URL.Path {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	parts := strings.SplitN(rest, "/", 2)
	id := parts[0]
	if id == "" {
		writeErr(w, http.StatusBadRequest, "missing job id")
		return
	}

	switch {
	case len(parts) == 1 && r.Method == http.MethodGet:
		s.getJob(w, r, id)
	case len(parts) == 2 && parts[1] == "cancel" && r.Method == http.MethodPost:
		s.cancelJob(w, r, id)
	case len(parts) == 1:
		writeErr(w, http.StatusMethodNotAllowed, "GET required")
	case len(parts) == 2 && parts[1] == "cancel":
		writeErr(w, http.StatusMethodNotAllowed, "POST required")
	default:
		writeErr(w, http.StatusNotFound, "not found")
	}
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request, id string) {
	job, err := s.deps.Store.GetJob(r.Context(), id)
	if err != nil {
		if errors.Is(err, jobqueue.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "job not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("ipc: get job: %w", err).Error())
		return
	}
	writeJSON(w, http.StatusOK, jobDTO(job))
}

func (s *Server) cancelJob(w http.ResponseWriter, r *http.Request, id string) {
	err := s.deps.Store.Cancel(r.Context(), id)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": jobqueue.StatusCancelled})
	case errors.Is(err, jobqueue.ErrNotFound):
		writeErr(w, http.StatusNotFound, "job not found")
	case errors.Is(err, jobqueue.ErrJobTerminal):
		writeErr(w, http.StatusConflict, "job already terminal")
	default:
		s.logger().Error("cancel_job: store update", "id", id, "err", err)
		writeErr(w, http.StatusInternalServerError, "internal_error")
	}
}

// jobDTO reshapes the SQLite row into the JSON the renderer expects.
// Pulls null-fields out of sql.NullString/sql.NullTime so the wire shape is
// flat strings instead of {"String": "...", "Valid": true}.
func jobDTO(j *jobqueue.Job) map[string]any {
	out := map[string]any{
		"id":               j.ID,
		"workload_id":      j.WorkloadID,
		"organization_id":  j.OrganizationID,
		"workload_type":    j.WorkloadType,
		"workload_version": j.WorkloadVersion,
		"payload_json":     j.PayloadJSON,
		"status":           j.Status,
		"created_at":       j.CreatedAt,
		"updated_at":       j.UpdatedAt,
	}
	if j.LeaseToken.Valid {
		out["lease_token"] = j.LeaseToken.String
	}
	if j.LeaseExpiresAt.Valid {
		out["lease_expires_at"] = j.LeaseExpiresAt.Time
	}
	if j.ResumableState.Valid {
		out["resumable_state"] = j.ResumableState.String
	}
	return out
}
