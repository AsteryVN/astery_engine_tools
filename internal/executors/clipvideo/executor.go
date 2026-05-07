// Package clipvideo is the FFmpeg-backed clip-video executor — the headline
// MVP workload. Implements registry.Executor.
//
// Workload payload shape:
//   {
//     "master_video_url": "https://...",
//     "target_aspect": "16:9" | "9:16",
//     "clip_specs": [{"index":0,"start_seconds":12.4,"end_seconds":41.7,"title":"..."}]
//   }
//
// Resumable: persists completed clip indices to JobHandle.SaveResumableState
// so a restarted job skips clips already done. Outputs go to JobHandle.AddOutput.
package clipvideo

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/AsteryVN/astery_engine_tools/internal/runtime/registry"
	"github.com/AsteryVN/astery_engine_tools/internal/tools"
)

// ID is the workload type this executor handles.
const ID = "clip-video"

// SupportedVersion is the highest workload_version this executor speaks.
// Bumped in lock-step with cloud-side payload changes.
const SupportedVersion = 1

// Executor implements registry.Executor.
type Executor struct {
	tools *tools.Manager
}

// New constructs an Executor.
func New(toolsMgr *tools.Manager) *Executor { return &Executor{tools: toolsMgr} }

// ID returns the workload type — required by registry.Executor.
func (e *Executor) ID() string { return ID }

// CanRun returns false for unsupported versions or wrong type.
func (e *Executor) CanRun(_ context.Context, w registry.Workload) bool {
	return w.Type == ID && w.Version <= SupportedVersion
}

// Estimate returns a coarse pre-flight estimate. We don't try to be clever —
// the resource manager makes the actual gating decision.
func (e *Executor) Estimate(w registry.Workload) registry.ResourceEstimate {
	return registry.ResourceEstimate{
		CPUCores:  1.5,
		RAMBytes:  1024 * 1024 * 1024, // 1 GiB headroom
		DiskBytes: 2 * 1024 * 1024 * 1024,
		GPU:       false,
		Duration:  90 * time.Second,
	}
}

// Cancel is a no-op marker — context cancellation surfaces in Execute.
func (e *Executor) Cancel(jobID string) error { return nil }

// resumableState is what we persist between attempts.
type resumableState struct {
	CompletedClipIndices []int `json:"completed_clip_indices"`
}

// payload mirrors the cloud workload payload.
type payload struct {
	MasterVideoURL string     `json:"master_video_url"`
	TargetAspect   string     `json:"target_aspect"`
	ClipSpecs      []ClipSpec `json:"clip_specs"`
}

// ClipSpec is one clip-cutting spec.
type ClipSpec struct {
	Index        int     `json:"index"`
	StartSeconds float64 `json:"start_seconds"`
	EndSeconds   float64 `json:"end_seconds"`
	Title        string  `json:"title,omitempty"`
}

// Execute runs the FFmpeg pipeline.
func (e *Executor) Execute(ctx context.Context, h registry.JobHandle) error {
	w := h.Workload()
	var p payload
	if err := decodePayload(w.Payload, &p); err != nil {
		return fmt.Errorf("decode payload: %w", err)
	}
	if len(p.ClipSpecs) == 0 {
		return fmt.Errorf("clip-video: no clip specs in payload")
	}
	if p.MasterVideoURL == "" {
		return fmt.Errorf("clip-video: master_video_url required")
	}
	aspect := p.TargetAspect
	if aspect == "" {
		aspect = "16:9"
	}
	if aspect != "16:9" && aspect != "9:16" {
		return fmt.Errorf("clip-video: unsupported target_aspect %q", aspect)
	}

	// Recover completed indices.
	var rs resumableState
	_ = h.LoadResumableState(ctx, &rs)
	done := make(map[int]bool, len(rs.CompletedClipIndices))
	for _, i := range rs.CompletedClipIndices {
		done[i] = true
	}

	// Locate FFmpeg.
	ff, err := e.tools.Locate(ctx, "ffmpeg")
	if err != nil {
		return fmt.Errorf("clip-video: locate ffmpeg: %w", err)
	}
	_ = h.AppendEvent(ctx, "log", "ffmpeg located", map[string]any{"path": ff.Path, "version": ff.Version})

	// Download master once.
	masterPath := filepath.Join(h.WorkDir(), "master.mp4")
	if _, err := os.Stat(masterPath); os.IsNotExist(err) {
		if dlErr := downloadFile(ctx, p.MasterVideoURL, masterPath); dlErr != nil {
			return fmt.Errorf("clip-video: download master: %w", dlErr)
		}
		_ = h.AppendEvent(ctx, "log", "master downloaded", nil)
	}

	total := len(p.ClipSpecs)
	for i, spec := range p.ClipSpecs {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if done[spec.Index] {
			continue
		}
		clipPath := filepath.Join(h.WorkDir(), fmt.Sprintf("clip-%d.mp4", spec.Index))
		args := buildFFmpegArgs(masterPath, clipPath, spec, aspect)
		cmd := exec.CommandContext(ctx, ff.Path, args...)
		out, runErr := cmd.CombinedOutput()
		if runErr != nil {
			_ = h.AppendEvent(ctx, "error", "ffmpeg failed",
				map[string]any{"index": spec.Index, "stderr_tail": tail(string(out), 1024)})
			return fmt.Errorf("clip-video: ffmpeg index %d: %w", spec.Index, runErr)
		}
		st, _ := os.Stat(clipPath)
		size := int64(0)
		if st != nil {
			size = st.Size()
		}
		f, openErr := os.Open(clipPath)
		if openErr != nil {
			return fmt.Errorf("clip-video: open output %d: %w", spec.Index, openErr)
		}
		uploadErr := h.AddOutput(ctx, "video_clip", fmt.Sprintf("clip-%d.mp4", spec.Index), f, size,
			map[string]any{
				"hook_index":   spec.Index,
				"duration_sec": spec.EndSeconds - spec.StartSeconds,
				"aspect":       aspect,
				"title":        spec.Title,
			})
		_ = f.Close()
		if uploadErr != nil {
			return fmt.Errorf("clip-video: upload output %d: %w", spec.Index, uploadErr)
		}
		done[spec.Index] = true
		rs.CompletedClipIndices = append(rs.CompletedClipIndices, spec.Index)
		_ = h.SaveResumableState(ctx, rs)

		// Best-effort progress push — runtime forwards via channel.
		select {
		case h.ProgressEvents() <- registry.ProgressEvent{
			Fraction: float64(i+1) / float64(total),
			Stage:    "encoded",
			Detail:   fmt.Sprintf("clip %d of %d", i+1, total),
		}:
		default:
		}
	}
	return nil
}

// Recover re-runs Execute — resumable state skips already-done clips.
func (e *Executor) Recover(ctx context.Context, h registry.JobHandle) error {
	return e.Execute(ctx, h)
}

// ─── helpers ────────────────────────────────────────────────────────────

// downloadFile streams a URL to disk.
func downloadFile(ctx context.Context, url, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("new req: %w", err)
	}
	httpClient := &http.Client{Timeout: 30 * time.Minute}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("get: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("download status %d", resp.StatusCode)
	}
	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}
	defer out.Close()
	if _, err := io.Copy(out, resp.Body); err != nil {
		return fmt.Errorf("copy: %w", err)
	}
	return nil
}

// buildFFmpegArgs returns the FFmpeg argv. 16:9 = stream-copy (fast); 9:16 =
// libx264 transcode with crop+scale (matches the cloud's pkg/video/clipper.go).
func buildFFmpegArgs(master, dst string, spec ClipSpec, aspect string) []string {
	if aspect == "9:16" {
		return []string{
			"-y",
			"-ss", fmt.Sprintf("%.3f", spec.StartSeconds),
			"-to", fmt.Sprintf("%.3f", spec.EndSeconds),
			"-i", master,
			"-vf", "crop=ih*9/16:ih,scale=1080:1920",
			"-c:v", "libx264", "-preset", "veryfast", "-crf", "23",
			"-c:a", "aac", "-b:a", "128k",
			"-movflags", "+faststart",
			dst,
		}
	}
	// 16:9 — stream copy (no transcode).
	return []string{
		"-y",
		"-ss", fmt.Sprintf("%.3f", spec.StartSeconds),
		"-to", fmt.Sprintf("%.3f", spec.EndSeconds),
		"-i", master,
		"-c", "copy",
		"-movflags", "+faststart",
		dst,
	}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func decodePayload(payload map[string]any, target *payload) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, target)
}

// _ keeps sync imported (not required at compile but useful when adding
// future helpers that share state).
var _ = sync.Mutex{}
