// Package tools is the runtime tool manager — discovers, verifies, and (in
// future) downloads runtime tools (FFmpeg today, CUDA / AI weights later).
// MVP only does PATH probing for FFmpeg; download path is documented in the
// architecture spec.
package tools

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Tool is a discovered runtime tool.
type Tool struct {
	ID      string // e.g. "ffmpeg"
	Path    string // absolute path
	Version string // best-effort parsed from `<tool> -version`
}

// Manager owns tool discovery + (future) provisioning.
type Manager struct {
	cache map[string]Tool
}

// New constructs a Manager with an empty cache.
func New() *Manager {
	return &Manager{cache: map[string]Tool{}}
}

// Locate returns the tool by id, discovering it if needed. MVP supports
// "ffmpeg" only. Returns ErrNotFound when the tool isn't on PATH.
func (m *Manager) Locate(ctx context.Context, id string) (Tool, error) {
	if t, ok := m.cache[id]; ok {
		return t, nil
	}
	switch id {
	case "ffmpeg":
		t, err := discoverFFmpeg(ctx)
		if err != nil {
			return Tool{}, err
		}
		m.cache[id] = t
		return t, nil
	default:
		return Tool{}, fmt.Errorf("locate %s: %w", id, ErrUnknownTool)
	}
}

// ErrNotFound is returned when the tool isn't on PATH and no fallback is
// available. ErrUnknownTool is returned when the id has no known discovery
// implementation.
var (
	ErrNotFound    = errors.New("tool not found")
	ErrUnknownTool = errors.New("unknown tool id")
)

func discoverFFmpeg(ctx context.Context) (Tool, error) {
	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		return Tool{}, fmt.Errorf("ffmpeg not on PATH: %w", ErrNotFound)
	}
	out, err := exec.CommandContext(ctx, path, "-version").CombinedOutput()
	if err != nil {
		return Tool{}, fmt.Errorf("ffmpeg version probe: %w", err)
	}
	return Tool{
		ID:      "ffmpeg",
		Path:    path,
		Version: parseFFmpegVersion(string(out)),
	}, nil
}

// parseFFmpegVersion extracts the semantic version from `ffmpeg -version`
// output. Returns "" when the line shape is unexpected — non-fatal.
func parseFFmpegVersion(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "ffmpeg version ") {
			rest := strings.TrimPrefix(line, "ffmpeg version ")
			fields := strings.Fields(rest)
			if len(fields) > 0 {
				return fields[0]
			}
		}
	}
	return ""
}
