// Package jobqueue — boot-time recovery + bulk-failure helpers.
//
// Two callers:
//
//   - cmd/engine-toold/main.go calls RecoverOrphaned at startup. Any job
//     row left in `running` from a previous (now-killed) daemon process is
//     flipped to `failed` with a stable reason. We do NOT requeue: a
//     resumed clip-video / extract-audio job would re-run ffmpeg + re-upload
//     and double-bill the cloud. The cloud workload reconciler retries
//     failed workloads on a different device anyway.
//
//   - internal/ipc/handlers_unpair.go calls SweepActiveToFailed when the
//     user clicks "Re-pair". That sweep also covers `queued` jobs: when the
//     daemon is unpairing, even queued work must terminate before the
//     keystore is cleared so the local state stays internally consistent.
//
// Both helpers preserve history — `job_events` rows are appended (kind=
// orphan_failed | unpair_failed) so the operator can see *why* a row went
// to failed even after the keystore is gone.
package jobqueue

import (
	"context"
	"errors"
	"fmt"
)

// ReasonOrphanedAfterRestart is the canonical message attached to jobs that
// the recovery sweep moved out of `running` at boot. Stable string — used
// by the cloud reconciler / FE if it ever fetches the local event stream.
const ReasonOrphanedAfterRestart = "orphaned_after_restart"

// ReasonUnpaired is the message attached to jobs the unpair handler
// terminated.
const ReasonUnpaired = "unpaired"

// RecoverOrphaned flips every row in `running` to `failed` and writes one
// `orphan_failed` event per touched job. Idempotent — a second call finds
// no `running` rows and emits no events. Safe to call before the scheduler
// starts (no concurrent writers, single-shot at boot).
//
// Returns the count of jobs that were flipped.
func (s *Store) RecoverOrphaned(ctx context.Context, reason string) (int, error) {
	if reason == "" {
		reason = ReasonOrphanedAfterRestart
	}
	return s.failActive(ctx, []string{StatusRunning}, "orphan_failed", reason)
}

// SweepActiveToFailed flips every non-terminal job (`queued` or `running`)
// to `failed`. Used by the unpair handler — once the daemon is about to
// drop its identity, even queued work that will never get scheduled must
// terminate cleanly. One `unpair_failed` event per touched job.
//
// Returns the count of jobs that were flipped.
func (s *Store) SweepActiveToFailed(ctx context.Context, reason string) (int, error) {
	if reason == "" {
		reason = ReasonUnpaired
	}
	return s.failActive(ctx, []string{StatusQueued, StatusRunning}, "unpair_failed", reason)
}

// failActive performs the shared UPDATE-then-event pattern under the
// store's write mutex + a single SQL transaction. statuses MUST be a
// non-empty list of non-terminal values; the helper does not guard against
// callers passing terminal states (callers control the set).
func (s *Store) failActive(ctx context.Context, statuses []string, eventKind, message string) (int, error) {
	if len(statuses) == 0 {
		return 0, errors.New("failActive: empty statuses")
	}
	if eventKind == "" {
		return 0, errors.New("failActive: empty eventKind")
	}

	s.wmu.Lock()
	defer s.wmu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("failActive: begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// Build the IN-list placeholders. modernc.org/sqlite supports `?` only.
	placeholders := make([]byte, 0, 2*len(statuses)-1)
	args := make([]any, 0, len(statuses))
	for i, st := range statuses {
		if i > 0 {
			placeholders = append(placeholders, ',')
		}
		placeholders = append(placeholders, '?')
		args = append(args, st)
	}

	// Step 1 — collect the IDs we're about to touch. We do this BEFORE the
	// UPDATE so we can iterate them for event-append without depending on
	// RETURNING (modernc.org/sqlite supports it but the query path is
	// clearer if we keep the two statements explicit).
	idsQuery := "SELECT id FROM jobs WHERE status IN (" + string(placeholders) + ")"
	rows, err := tx.QueryContext(ctx, idsQuery, args...)
	if err != nil {
		return 0, fmt.Errorf("failActive: select ids: %w", err)
	}
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("failActive: scan id: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("failActive: rows.Err: %w", err)
	}

	if len(ids) == 0 {
		// Idempotent fast-path — no UPDATE issued, no events written.
		if err := tx.Commit(); err != nil {
			return 0, fmt.Errorf("failActive: commit empty: %w", err)
		}
		committed = true
		return 0, nil
	}

	// Step 2 — single UPDATE with the same status filter (the set is stable
	// inside this tx since we hold the write mutex).
	updateQuery := "UPDATE jobs SET status = ?, updated_at = datetime('now') WHERE status IN (" + string(placeholders) + ")"
	updateArgs := append([]any{StatusFailed}, args...)
	if _, err := tx.ExecContext(ctx, updateQuery, updateArgs...); err != nil {
		return 0, fmt.Errorf("failActive: update: %w", err)
	}

	// Step 3 — one event per touched job.
	eventStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO job_events (job_id, kind, message) VALUES (?, ?, ?)`)
	if err != nil {
		return 0, fmt.Errorf("failActive: prepare event: %w", err)
	}
	defer eventStmt.Close()
	for _, id := range ids {
		if _, err := eventStmt.ExecContext(ctx, id, eventKind, message); err != nil {
			return 0, fmt.Errorf("failActive: insert event for %s: %w", id, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("failActive: commit: %w", err)
	}
	committed = true
	return len(ids), nil
}
