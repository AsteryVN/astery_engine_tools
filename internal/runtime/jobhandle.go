package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	stdsync "sync"
	"time"

	"github.com/AsteryVN/astery_engine_tools/internal/jobqueue"
	"github.com/AsteryVN/astery_engine_tools/internal/runtime/registry"
	cloudsync "github.com/AsteryVN/astery_engine_tools/internal/sync"
	"github.com/AsteryVN/astery_engine_tools/internal/upload"
)

// jobHandle implements registry.JobHandle. Owned by the scheduler;
// surrendered when Execute returns.
type jobHandle struct {
	id          string
	wl          registry.Workload
	store       *jobqueue.Store
	cloud       *cloudsync.Client
	upload      *upload.Client
	leaseToken  string
	workDir     string
	progressCh  chan registry.ProgressEvent
	progressMu  stdsync.Mutex
	progressCtx context.Context
}

// JobHandleDeps bundles inputs.
type JobHandleDeps struct {
	JobID      string
	Workload   registry.Workload
	Store      *jobqueue.Store
	Cloud      *cloudsync.Client
	Upload     *upload.Client
	LeaseToken string
	BaseTmp    string
}

// NewJobHandle constructs a job handle, including its tmp work-dir.
func NewJobHandle(ctx context.Context, deps JobHandleDeps) (*jobHandle, error) {
	wd := filepath.Join(deps.BaseTmp, "job-"+deps.JobID)
	if err := os.MkdirAll(wd, 0o700); err != nil {
		return nil, fmt.Errorf("workdir: %w", err)
	}
	h := &jobHandle{
		id:          deps.JobID,
		wl:          deps.Workload,
		store:       deps.Store,
		cloud:       deps.Cloud,
		upload:      deps.Upload,
		leaseToken:  deps.LeaseToken,
		workDir:     wd,
		progressCh:  make(chan registry.ProgressEvent, 8),
		progressCtx: ctx,
	}
	go h.pumpProgress(ctx)
	return h, nil
}

// pumpProgress consumes the channel and forwards to cloud + jobqueue. Stops
// when ctx is cancelled.
func (h *jobHandle) pumpProgress(ctx context.Context) {
	throttle := time.NewTicker(time.Second)
	defer throttle.Stop()
	var last *registry.ProgressEvent
	for {
		select {
		case <-ctx.Done():
			return
		case p, ok := <-h.progressCh:
			if !ok {
				return
			}
			last = &p
		case <-throttle.C:
			if last == nil {
				continue
			}
			_ = h.AppendEvent(ctx, "progress", last.Stage, map[string]any{"fraction": last.Fraction, "detail": last.Detail})
			if h.cloud != nil {
				_ = h.cloud.Progress(ctx, h.wl.ID, h.leaseToken, last.Fraction, last.Stage, last.Detail)
			}
			last = nil
		}
	}
}

// Cleanup removes the tmp work-dir. Called by the scheduler after Execute
// returns successfully.
func (h *jobHandle) Cleanup() {
	close(h.progressCh)
	_ = os.RemoveAll(h.workDir)
}

// ─── registry.JobHandle implementation ───────────────────────────────────

func (h *jobHandle) ID() string                          { return h.id }
func (h *jobHandle) Workload() registry.Workload         { return h.wl }
func (h *jobHandle) ProgressEvents() chan<- registry.ProgressEvent { return h.progressCh }
func (h *jobHandle) WorkDir() string                     { return h.workDir }

// AddOutput presigns an upload URL, PUTs the bytes, then records the
// manifest entry on the cloud side.
func (h *jobHandle) AddOutput(ctx context.Context, kind, keySuffix string, r io.Reader, size int64, meta map[string]any) error {
	if h.cloud == nil || h.upload == nil {
		return fmt.Errorf("output upload not configured")
	}
	// Materialize the reader to a temp file so we can checksum + retry.
	tmp := filepath.Join(h.workDir, "out-"+keySuffix)
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("create out tmp: %w", err)
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		return fmt.Errorf("copy out tmp: %w", err)
	}
	_ = f.Close()
	st, _ := os.Stat(tmp)
	if size <= 0 && st != nil {
		size = st.Size()
	}
	presign, err := h.cloud.PresignUpload(ctx, h.wl.ID, h.leaseToken, kind, keySuffix, size)
	if err != nil {
		return fmt.Errorf("presign upload: %w", err)
	}
	res, err := h.upload.UploadFile(ctx, tmp, presign.UploadURL, presign.Method, presign.Headers)
	if err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	manifest := []cloudsync.OutputManifestEntry{{
		Kind:           kind,
		StorageKey:     presign.StorageKey,
		Bytes:          res.Bytes,
		ChecksumSHA256: res.ChecksumSHA256,
		Metadata:       meta,
	}}
	if err := h.cloud.RecordOutputs(ctx, h.wl.ID, h.leaseToken, manifest); err != nil {
		return fmt.Errorf("record output: %w", err)
	}
	return nil
}

// AppendEvent persists a job_event row.
func (h *jobHandle) AppendEvent(ctx context.Context, kind, msg string, attrs map[string]any) error {
	attrsJSON := ""
	if attrs != nil {
		raw, err := json.Marshal(attrs)
		if err == nil {
			attrsJSON = string(raw)
		}
	}
	return h.store.AppendEvent(ctx, h.id, kind, msg, attrsJSON)
}

// SaveResumableState persists the executor-defined recovery hint.
func (h *jobHandle) SaveResumableState(ctx context.Context, state any) error {
	raw, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshal resumable state: %w", err)
	}
	return h.store.UpdateResumableState(ctx, h.id, string(raw))
}

// LoadResumableState restores the previously-saved state.
func (h *jobHandle) LoadResumableState(ctx context.Context, into any) error {
	job, err := h.store.GetJob(ctx, h.id)
	if err != nil {
		return err
	}
	if !job.ResumableState.Valid || job.ResumableState.String == "" {
		return nil
	}
	return json.Unmarshal([]byte(job.ResumableState.String), into)
}

// LeaseToken exposes the current lease token (used by the scheduler when
// refreshing on cloud heartbeat-renew).
func (h *jobHandle) LeaseToken() string { return h.leaseToken }
