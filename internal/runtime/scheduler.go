package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/AsteryVN/astery_engine_tools/internal/auth"
	"github.com/AsteryVN/astery_engine_tools/internal/jobqueue"
	"github.com/AsteryVN/astery_engine_tools/internal/runtime/registry"
	"github.com/AsteryVN/astery_engine_tools/internal/runtime/resources"
	cloudsync "github.com/AsteryVN/astery_engine_tools/internal/sync"
	"github.com/AsteryVN/astery_engine_tools/internal/upload"
)

// AuthRefresher is the slice of TokenSource the scheduler needs to recover
// from a 401: drop the cached bundle (so a re-pair is picked up from the
// keystore) and force a refresh roundtrip. Defined here as a small interface
// to keep the runtime package decoupled from *auth.TokenSource.
type AuthRefresher interface {
	Invalidate()
	ForceRefresh(ctx context.Context) (string, error)
}

// Scheduler pulls workloads from cloud, dispatches via the executor
// registry, and surrenders leases at completion.
type Scheduler struct {
	deps           SchedulerDeps
	paused         atomic.Bool
	reauthRequired atomic.Bool
}

// SchedulerDeps bundles inputs.
type SchedulerDeps struct {
	Registry    *registry.Registry
	Resources   *resources.Manager
	Cloud       *cloudsync.Client
	Upload      *upload.Client
	Store       *jobqueue.Store
	BaseTmp     string
	// Auth, when non-nil, lets the scheduler recover from a 401 by reloading
	// the keystore (after a re-pair) and forcing a refresh roundtrip.
	Auth AuthRefresher
}

// New constructs a Scheduler.
func NewScheduler(deps SchedulerDeps) *Scheduler {
	return &Scheduler{deps: deps}
}

// Pause stops claiming new work — in-flight jobs continue.
func (s *Scheduler) Pause() { s.paused.Store(true) }

// Resume re-enables claim attempts.
func (s *Scheduler) Resume() {
	s.paused.Store(false)
	s.reauthRequired.Store(false)
}

// Paused reports current state.
func (s *Scheduler) Paused() bool { return s.paused.Load() }

// ReauthRequired reports whether the scheduler has been parked because the
// cloud rejected both the cached session JWT and the refresh token. The UI
// should surface this and prompt the user to re-pair; Resume() clears it.
func (s *Scheduler) ReauthRequired() bool { return s.reauthRequired.Load() }

// pollInterval is how often we ask cloud for new work.
const pollInterval = 5 * time.Second

// Run drives the loop until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) {
	// Recover any in-flight jobs from a prior crash.
	s.recoverPending(ctx)

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if s.paused.Load() {
				continue
			}
			s.tick(ctx)
		}
	}
}

// claimWithReauth wraps Cloud.Claim with a one-shot recovery on 401:
//  1. Drop the in-memory token cache so a freshly-paired bundle on disk gets
//     picked up on the next BearerToken call. Retry Claim.
//  2. If reload didn't help, force a refresh roundtrip. Retry Claim.
//  3. Still 401, OR refresh itself returned ErrUnauthorized → caller pauses
//     the scheduler and surfaces ReauthRequired to the UI.
//
// Retries inside this function are bounded; the scheduler-level 5s tick
// provides any further backoff naturally.
func (s *Scheduler) claimWithReauth(ctx context.Context) (*cloudsync.ClaimResponse, error) {
	resp, err := s.deps.Cloud.Claim(ctx)
	if !errors.Is(err, cloudsync.ErrUnauthorized) || s.deps.Auth == nil {
		return resp, err
	}

	// Step 1: reload from keystore (handles re-pair under a running daemon).
	s.deps.Auth.Invalidate()
	resp, err = s.deps.Cloud.Claim(ctx)
	if !errors.Is(err, cloudsync.ErrUnauthorized) {
		if err == nil {
			slog.InfoContext(ctx, "claim recovered after keystore reload")
		}
		return resp, err
	}

	// Step 2: force a refresh roundtrip.
	if _, refreshErr := s.deps.Auth.ForceRefresh(ctx); refreshErr != nil {
		if errors.Is(refreshErr, auth.ErrUnauthorized) {
			// Refresh token rejected — only re-pair recovers.
			return nil, cloudsync.ErrUnauthorized
		}
		// Transient (network/5xx) — bubble up; the tick loop will retry.
		return nil, fmt.Errorf("force refresh: %w", refreshErr)
	}
	resp, err = s.deps.Cloud.Claim(ctx)
	if err == nil {
		slog.InfoContext(ctx, "claim recovered after force refresh")
	}
	return resp, err
}

func (s *Scheduler) tick(ctx context.Context) {
	slot, err := s.deps.Resources.Reserve(ctx)
	if err != nil {
		return // resource limit reached; try next tick
	}
	released := false
	defer func() {
		if !released {
			slot.Release()
		}
	}()

	resp, err := s.claimWithReauth(ctx)
	switch {
	case err == nil:
	case errors.Is(err, cloudsync.ErrNoWork):
		return
	case errors.Is(err, cloudsync.ErrUnauthorized):
		// Recovery already attempted inside claimWithReauth; if we still see
		// ErrUnauthorized here the refresh token is also dead → re-pair.
		s.reauthRequired.Store(true)
		s.paused.Store(true)
		slog.WarnContext(ctx, "scheduler paused — re-pair required (refresh token rejected)")
		return
	default:
		slog.WarnContext(ctx, "claim error", "error", err)
		return
	}

	w := registry.Workload{
		ID:             resp.Workload.ID,
		OrganizationID: resp.Workload.OrganizationID,
		Type:           resp.Workload.Type,
		Version:        resp.Workload.Version,
		Payload:        resp.Workload.Payload,
		RequiredCaps:   resp.Workload.RequiredCapabilities,
	}
	exec := s.deps.Registry.Lookup(w.Type)
	if exec == nil {
		slog.WarnContext(ctx, "no executor registered — surrendering",
			"workload_id", w.ID, "type", w.Type)
		_ = s.deps.Cloud.Surrender(ctx, w.ID, resp.Lease.Token, "no_executor")
		return
	}
	if !exec.CanRun(ctx, w) {
		slog.WarnContext(ctx, "executor declined CanRun — surrendering",
			"workload_id", w.ID, "type", w.Type, "version", w.Version)
		_ = s.deps.Cloud.Surrender(ctx, w.ID, resp.Lease.Token, "version_mismatch")
		return
	}

	released = true
	go func() {
		defer slot.Release()
		s.runOne(ctx, exec, w, resp.Lease)
	}()
}

// upsertJobForWorkload reconciles a freshly-claimed workload with the local
// jobs table. The schema enforces UNIQUE(workload_id), so a re-issued
// workload (cloud requeued after surrender / heartbeat-lost / etc.) must NOT
// produce a fresh INSERT — historically that fell through to UNIQUE conflict
// → surrender → cloud requeue → infinite loop.
//
// Branches:
//   - no existing row → INSERT and proceed (happy path).
//   - existing row in queued/running → take over: bump lease, treat as a
//     fresh attempt, return its job id.
//   - existing row in succeeded → cloud lost track of completion. Tell cloud
//     by calling Complete; abort local execute.
//   - existing row in failed/cancelled → terminal locally. Report Fail
//     (retryable=false) so cloud stops re-issuing this workload to us.
//
// Returns (jobID, attemptNum, ok). ok=false means caller must NOT proceed
// with execution (terminal report already sent or store error).
func (s *Scheduler) upsertJobForWorkload(ctx context.Context, w registry.Workload, lease cloudsync.LeaseDTO) (string, int, bool) {
	existing, err := s.deps.Store.GetByWorkloadID(ctx, w.ID)
	if err != nil && !errors.Is(err, jobqueue.ErrNotFound) {
		slog.WarnContext(ctx, "lookup existing job failed — surrendering",
			"workload_id", w.ID, "error", err)
		_ = s.deps.Cloud.Surrender(ctx, w.ID, lease.Token, "store_error")
		return "", 0, false
	}

	if existing == nil {
		jobID := uuid.NewString()
		payloadBytes, _ := json.Marshal(w.Payload)
		if err := s.deps.Store.CreateJob(ctx, jobqueue.InsertJobInput{
			ID:              jobID,
			WorkloadID:      w.ID,
			OrganizationID:  w.OrganizationID,
			WorkloadType:    w.Type,
			WorkloadVersion: w.Version,
			PayloadJSON:     string(payloadBytes),
			LeaseToken:      lease.Token,
			LeaseExpiresAt:  lease.ExpiresAt,
		}); err != nil {
			slog.WarnContext(ctx, "store job failed — surrendering",
				"workload_id", w.ID, "error", err)
			_ = s.deps.Cloud.Surrender(ctx, w.ID, lease.Token, "store_error")
			return "", 0, false
		}
		return jobID, 1, true
	}

	switch existing.Status {
	case jobqueue.StatusQueued, jobqueue.StatusRunning:
		// Crash recovery — daemon restarted while job was active. Take over
		// with the new lease.
		if err := s.deps.Store.UpdateLease(ctx, existing.ID, lease.Token, lease.ExpiresAt); err != nil {
			slog.WarnContext(ctx, "update lease on takeover failed — surrendering",
				"workload_id", w.ID, "job_id", existing.ID, "error", err)
			_ = s.deps.Cloud.Surrender(ctx, w.ID, lease.Token, "store_error")
			return "", 0, false
		}
		attempts, _ := s.deps.Store.AttemptCount(ctx, existing.ID)
		slog.InfoContext(ctx, "resuming workload from existing local job",
			"workload_id", w.ID, "job_id", existing.ID, "attempt", attempts+1)
		return existing.ID, attempts + 1, true

	case jobqueue.StatusSucceeded:
		// Local says we already finished this — tell cloud so it stops
		// re-issuing.
		slog.WarnContext(ctx, "workload already succeeded locally — reporting Complete to cloud",
			"workload_id", w.ID, "job_id", existing.ID)
		_ = s.deps.Cloud.Complete(ctx, w.ID, lease.Token, map[string]any{
			"executor_id":  "",
			"local_status": "already_succeeded",
		})
		return "", 0, false

	default:
		// failed / cancelled — terminal. Don't loop locally.
		slog.WarnContext(ctx, "workload already terminal locally — reporting Fail to cloud",
			"workload_id", w.ID, "job_id", existing.ID, "local_status", existing.Status)
		_ = s.deps.Cloud.Fail(ctx, w.ID, lease.Token, "local_already_attempted: "+existing.Status, false)
		return "", 0, false
	}
}

func (s *Scheduler) runOne(ctx context.Context, exec registry.Executor, w registry.Workload, lease cloudsync.LeaseDTO) {
	jobID, attemptNum, ok := s.upsertJobForWorkload(ctx, w, lease)
	if !ok {
		return
	}
	_ = s.deps.Store.UpdateStatus(ctx, jobID, jobqueue.StatusRunning)
	attemptID, _ := s.deps.Store.StartAttempt(ctx, jobID, attemptNum)

	heartbeat := cloudsync.StartHeartbeat(ctx, cloudsync.HeartbeatDeps{
		Client:     s.deps.Cloud,
		WorkloadID: w.ID,
		LeaseToken: lease.Token,
		Interval:   time.Duration(lease.HeartbeatIntervalSec) * time.Second,
		OnLost: func(err error) {
			slog.WarnContext(ctx, "lease lost", "workload_id", w.ID, "error", err)
		},
	})
	defer heartbeat.Stop()

	jh, err := NewJobHandle(ctx, JobHandleDeps{
		JobID:      jobID,
		Workload:   w,
		Store:      s.deps.Store,
		Cloud:      s.deps.Cloud,
		Upload:     s.deps.Upload,
		LeaseToken: lease.Token,
		BaseTmp:    s.deps.BaseTmp,
	})
	if err != nil {
		_ = s.deps.Cloud.Fail(ctx, w.ID, lease.Token, fmt.Sprintf("job handle: %v", err), false)
		_ = s.deps.Store.UpdateStatus(ctx, jobID, jobqueue.StatusFailed)
		_ = s.deps.Store.FinishAttempt(ctx, attemptID, "failed", err.Error())
		return
	}
	defer jh.Cleanup()

	runErr := exec.Execute(ctx, jh)
	if runErr != nil {
		retryable := ctx.Err() == nil // context cancellation = not retryable; otherwise let cloud decide
		_ = s.deps.Cloud.Fail(ctx, w.ID, lease.Token, runErr.Error(), retryable)
		_ = s.deps.Store.UpdateStatus(ctx, jobID, jobqueue.StatusFailed)
		_ = s.deps.Store.FinishAttempt(ctx, attemptID, "failed", runErr.Error())
		return
	}
	if err := s.deps.Cloud.Complete(ctx, w.ID, lease.Token, map[string]any{
		"executor_id":      exec.ID(),
		"executor_version": exec.Estimate(w).Duration.String(),
	}); err != nil {
		slog.WarnContext(ctx, "cloud complete failed", "workload_id", w.ID, "error", err)
	}
	_ = s.deps.Store.UpdateStatus(ctx, jobID, jobqueue.StatusSucceeded)
	_ = s.deps.Store.FinishAttempt(ctx, attemptID, "succeeded", "")
}

// recoverPending kicks off Recover() for jobs found in `running` state at
// boot — these had a daemon crash mid-execute. Lease has expired by now;
// the executor's Recover decides whether to resume from saved state.
func (s *Scheduler) recoverPending(ctx context.Context) {
	jobs, err := s.deps.Store.PendingRecovery(ctx)
	if err != nil {
		slog.WarnContext(ctx, "list pending recovery failed", "error", err)
		return
	}
	for _, j := range jobs {
		_ = s.deps.Store.UpdateStatus(ctx, j.ID, jobqueue.StatusFailed) // safety: cloud reclaim handles re-issue
		slog.InfoContext(ctx, "marked stranded job failed; cloud reclaimer will re-issue",
			"job_id", j.ID, "workload_id", j.WorkloadID)
	}
}
