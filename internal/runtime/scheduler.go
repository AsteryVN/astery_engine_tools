package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/AsteryVN/astery_engine_tools/internal/jobqueue"
	"github.com/AsteryVN/astery_engine_tools/internal/runtime/registry"
	"github.com/AsteryVN/astery_engine_tools/internal/runtime/resources"
	cloudsync "github.com/AsteryVN/astery_engine_tools/internal/sync"
	"github.com/AsteryVN/astery_engine_tools/internal/upload"
)

// Scheduler pulls workloads from cloud, dispatches via the executor
// registry, and surrenders leases at completion.
type Scheduler struct {
	deps    SchedulerDeps
	paused  atomic.Bool
}

// SchedulerDeps bundles inputs.
type SchedulerDeps struct {
	Registry  *registry.Registry
	Resources *resources.Manager
	Cloud     *cloudsync.Client
	Upload    *upload.Client
	Store     *jobqueue.Store
	BaseTmp   string
}

// New constructs a Scheduler.
func NewScheduler(deps SchedulerDeps) *Scheduler {
	return &Scheduler{deps: deps}
}

// Pause stops claiming new work — in-flight jobs continue.
func (s *Scheduler) Pause()  { s.paused.Store(true) }

// Resume re-enables claim attempts.
func (s *Scheduler) Resume() { s.paused.Store(false) }

// Paused reports current state.
func (s *Scheduler) Paused() bool { return s.paused.Load() }

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

	resp, err := s.deps.Cloud.Claim(ctx)
	switch err {
	case nil:
	case cloudsync.ErrNoWork:
		return
	case cloudsync.ErrUnauthorized:
		slog.WarnContext(ctx, "claim unauthorized — caller MUST refresh")
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

func (s *Scheduler) runOne(ctx context.Context, exec registry.Executor, w registry.Workload, lease cloudsync.LeaseDTO) {
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
		slog.WarnContext(ctx, "store job failed — surrendering", "workload_id", w.ID, "error", err)
		_ = s.deps.Cloud.Surrender(ctx, w.ID, lease.Token, "store_error")
		return
	}
	_ = s.deps.Store.UpdateStatus(ctx, jobID, jobqueue.StatusRunning)
	attemptID, _ := s.deps.Store.StartAttempt(ctx, jobID, 1)

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
