package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/AsteryVN/astery_engine_tools/internal/auth"
	"github.com/AsteryVN/astery_engine_tools/internal/storage"
)

// LoadOrCreateInstallationID reads the persisted installation_id from disk
// or generates a new one. Stable across reboots; lost on uninstall.
func LoadOrCreateInstallationID(layout storage.Layout) (uuid.UUID, error) {
	raw, err := os.ReadFile(layout.Install)
	if err == nil {
		id, parseErr := uuid.Parse(strings.TrimSpace(string(raw)))
		if parseErr == nil {
			return id, nil
		}
	}
	id := uuid.New()
	if err := os.WriteFile(layout.Install, []byte(id.String()), 0o600); err != nil {
		return uuid.Nil, fmt.Errorf("persist installation_id: %w", err)
	}
	return id, nil
}

// LoadOrCreateIPCToken returns the ipc.token (random 32-byte hex). Mode 0600
// so only the daemon's user can read it; Tauri shell reads from the same
// file when attaching.
func LoadOrCreateIPCToken(layout storage.Layout) (string, error) {
	if raw, err := os.ReadFile(layout.IpcToken); err == nil {
		s := strings.TrimSpace(string(raw))
		if len(s) >= 32 {
			return s, nil
		}
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("rand: %w", err)
	}
	tok := hex.EncodeToString(buf)
	if err := os.WriteFile(layout.IpcToken, []byte(tok), 0o600); err != nil {
		return "", fmt.Errorf("persist ipc token: %w", err)
	}
	return tok, nil
}

// HardwareFingerprint returns a SHA-like deterministic short string. NOT a
// security boundary — this is just stable identity glue. ed25519 + cloud-
// side verification is the future hard binding.
func HardwareFingerprint() string {
	host, _ := os.Hostname()
	uname, _ := exec.Command("uname", "-a").Output()
	return shortHash(host + "|" + runtime.GOOS + "|" + runtime.GOARCH + "|" + string(uname))
}

// shortHash returns a 64-char hex digest suitable for the hw_fingerprint
// column.
func shortHash(s string) string {
	// Use crypto/sha256 — package import via auth (which already imports it
	// via its dependency tree).
	h := auth.HashHex(s)
	return h
}

// CapabilityPayload returns the JSON the desktop posts to
// POST /v1/desktop/devices/:id/capabilities.
func CapabilityPayload(deviceID, installationID, appVersion, runtimeVersion, ffmpegPath, ffmpegVersion string) map[string]any {
	executors := []map[string]any{}
	if ffmpegVersion != "" {
		executors = append(executors, map[string]any{"id": "clip-video", "version": 1})
		// video:extract-audio executor — same ffmpeg dependency as clip-video.
		// Advertised so the cloud's claim filter routes extract-audio
		// workloads to ffmpeg-capable devices.
		executors = append(executors, map[string]any{"id": "video:extract-audio", "version": 1})
	}
	sort.Slice(executors, func(i, j int) bool { return executors[i]["id"].(string) < executors[j]["id"].(string) })
	return map[string]any{
		"device": map[string]any{
			"id":              deviceID,
			"installation_id": installationID,
			"os":              runtime.GOOS,
			"arch":            runtime.GOARCH,
			"app_version":     appVersion,
			"runtime_version": runtimeVersion,
		},
		"executors": executors,
		"tools": map[string]any{
			"ffmpeg": map[string]any{
				"available": ffmpegVersion != "",
				"version":   ffmpegVersion,
				"path":      ffmpegPath,
			},
		},
		"resources": map[string]any{
			"cpu_cores": runtime.NumCPU(),
		},
	}
}

// EnsureWorkDir prepares a per-process tmp area inside layout.Tmp.
func EnsureWorkDir(layout storage.Layout, suffix string) (string, error) {
	dir := filepath.Join(layout.Tmp, suffix)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("mkdir workdir: %w", err)
	}
	return dir, nil
}

// LogStartup emits a wide event with the current configuration.
func LogStartup(ctx context.Context, layout storage.Layout, version string) {
	slog.InfoContext(ctx, "engine-toold startup",
		"data_dir", layout.Root,
		"jobs_db", layout.JobsDB,
		"version", version,
	)
}
