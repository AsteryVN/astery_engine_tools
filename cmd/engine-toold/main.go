// Command engine-toold is the Astery Engine Tools daemon — primary local
// compute runtime. Pairs with the cloud, claims workloads, executes via
// pluggable executors (FFmpeg today, AI/GPU later), uploads outputs.
//
// Modes:
//   --headless         daemon only; no Tauri shell.
//   --pair <code>      one-shot: exchange the pairing display code, store
//                      the session, exit. Use to pair a fresh install via CLI.
//   --listen <addr>    IPC listen address (default 127.0.0.1:0).
//   --data-dir <path>  override the per-OS data directory.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/AsteryVN/astery_engine_tools/internal/auth"
	"github.com/AsteryVN/astery_engine_tools/internal/executors/clipvideo"
	"github.com/AsteryVN/astery_engine_tools/internal/ipc"
	"github.com/AsteryVN/astery_engine_tools/internal/jobqueue"
	"github.com/AsteryVN/astery_engine_tools/internal/observability"
	rt "github.com/AsteryVN/astery_engine_tools/internal/runtime"
	"github.com/AsteryVN/astery_engine_tools/internal/runtime/registry"
	"github.com/AsteryVN/astery_engine_tools/internal/runtime/resources"
	"github.com/AsteryVN/astery_engine_tools/internal/storage"
	cloudsync "github.com/AsteryVN/astery_engine_tools/internal/sync"
	"github.com/AsteryVN/astery_engine_tools/internal/tools"
	"github.com/AsteryVN/astery_engine_tools/internal/upload"
)

// version is overridden at build time via -ldflags.
var version = "0.1.0-dev"

func main() {
	var (
		headless    = flag.Bool("headless", true, "run without spawning the Tauri shell")
		withUI      = flag.Bool("with-ui", false, "spawn the bundled Tauri UI binary alongside the daemon")
		pairCode    = flag.String("pair", "", "exchange a pairing display code, store the session, then exit")
		dataDir     = flag.String("data-dir", "", "override the data directory")
		listen      = flag.String("listen", "127.0.0.1:0", "IPC listen address")
		// Default points at production cloud — released AppImages must work
		// out of the box for end users. Local development overrides via
		// ENGINE_CLOUD_URL env var or --cloud-url flag (the Tauri shell
		// passes the local override automatically in debug builds; see
		// tauri-app/src-tauri/src/daemon.rs).
		cloudURL = flag.String("cloud-url", envOr("ENGINE_CLOUD_URL", "https://engine.asteryvn.com/api"), "cloud control plane base URL")
		displayName = flag.String("display-name", envOr("ENGINE_DISPLAY_NAME", defaultDisplayName()), "device display name")
	)
	flag.Parse()

	layout, err := storage.Resolve(*dataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "resolve data dir:", err)
		os.Exit(1)
	}
	if err := layout.EnsureLayout(); err != nil {
		fmt.Fprintln(os.Stderr, "ensure layout:", err)
		os.Exit(1)
	}

	installID, err := rt.LoadOrCreateInstallationID(layout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "installation id:", err)
		os.Exit(1)
	}
	logger := observability.Setup(slog.LevelInfo, observability.Attrs{
		AppVersion:     version,
		RuntimeVersion: version,
		InstallationID: installID.String(),
		OS:             runtime.GOOS,
		Arch:           runtime.GOARCH,
	})
	logger.Info("engine-toold boot",
		"data_dir", layout.Root,
		"cloud_url", *cloudURL,
		"headless", *headless,
	)

	rootCtx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	keystore := auth.NewKeystore(layout.Secrets)
	pairingClient := auth.NewPairingClient(*cloudURL)

	// One-shot pairing path.
	if *pairCode != "" {
		if err := runPair(rootCtx, *pairCode, *displayName, installID.String(), pairingClient, keystore); err != nil {
			fmt.Fprintln(os.Stderr, "pair:", err)
			os.Exit(1)
		}
		fmt.Println("pairing successful")
		return
	}

	// Daemon path.
	tokenSrc := auth.NewTokenSource(keystore, pairingClient)
	bundle, err := keystore.Load()
	if err == nil {
		_ = tokenSrc.Set(bundle)
	}

	store, err := jobqueue.Open(layout.JobsDB)
	if err != nil {
		logger.Error("open jobs db", "error", err)
		os.Exit(1)
	}
	defer store.Close()

	tm := tools.New()
	if t, lerr := tm.Locate(rootCtx, "ffmpeg"); lerr == nil {
		logger.Info("ffmpeg located", "path", t.Path, "version", t.Version)
	} else {
		logger.Warn("ffmpeg not found — clip-video executor will fail at run time", "error", lerr)
	}

	reg := registry.New()
	reg.Register(clipvideo.New(tm))

	resMgr := resources.New(resources.Limits{}, layout.Root)

	cloud := cloudsync.New(cloudsync.Config{
		BaseURL:     *cloudURL,
		TokenSource: tokenSrc,
		Fingerprint: rt.HardwareFingerprint(),
	})

	uploader := upload.New()

	scheduler := rt.NewScheduler(rt.SchedulerDeps{
		Registry:  reg,
		Resources: resMgr,
		Cloud:     cloud,
		Upload:    uploader,
		Store:     store,
		BaseTmp:   layout.Tmp,
	})

	ipcToken, err := rt.LoadOrCreateIPCToken(layout)
	if err != nil {
		logger.Error("ipc token", "error", err)
		os.Exit(1)
	}
	host, _ := os.Hostname()
	pairDeps := &ipc.PairDeps{
		PairingClient: pairingClient,
		Keystore:      keystore,
		HwFingerprint: rt.HardwareFingerprint(),
		Device: auth.Device{
			InstallationID: installID.String(),
			DisplayName:    *displayName,
			Hostname:       host,
			OS:             runtime.GOOS,
			Arch:           runtime.GOARCH,
			AppVersion:     version,
			RuntimeVersion: version,
		},
		AlreadyPaired: func() bool {
			b, lerr := keystore.Load()
			return lerr == nil && b.SessionJWT != ""
		},
	}
	ipcServer, err := ipc.Listen(*listen, ipcToken, layout.IpcPort, ipc.Deps{
		Store:      store,
		Resources:  resMgr,
		Pause:      scheduler.Pause,
		Resume:     scheduler.Resume,
		Paused:     scheduler.Paused,
		AppVersion: version,
		Pair:       pairDeps,
	})
	if err != nil {
		logger.Error("ipc listen", "error", err)
		os.Exit(1)
	}
	logger.Info("ipc listening", "addr", ipcServer.Addr())

	go scheduler.Run(rootCtx)
	go func() {
		if err := ipcServer.Serve(rootCtx); err != nil && err != context.Canceled {
			logger.Warn("ipc server stopped", "error", err)
		}
	}()

	// Optional Tauri UI sidecar (inverted dev/headless-server path). Guarded
	// by sync.Once + a PID-file lock so the UI can't be spawned twice — see
	// architect spec regression test #5.
	var uiCmd *exec.Cmd
	if *withUI {
		uiCmd = spawnUIOnce(logger, layout.Root)
	}

	<-rootCtx.Done()
	logger.Info("engine-toold shutdown signal received")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if uiCmd != nil && uiCmd.Process != nil {
		stopUIChild(logger, uiCmd, shutdownCtx)
	}
	logger.Info("engine-toold exited cleanly")
}

// uiSpawnOnce gates the Tauri sidecar so multiple calls (e.g. the daemon's
// own watchdog re-entering main, or a future runtime-toggle handler) only
// spawn one process per daemon lifetime.
var uiSpawnOnce sync.Once
var uiSpawnCmd *exec.Cmd

// spawnUIOnce locates the sibling Tauri binary, writes a <data-dir>/ui.pid
// lock, and starts the child with stdout/stderr piped through slog so the
// UI's logs flow through the same SSE tap. Returns nil if a previous PID is
// still alive, or if the binary doesn't exist (degraded mode — daemon
// keeps running headless rather than failing).
func spawnUIOnce(logger *slog.Logger, dataDir string) *exec.Cmd {
	uiSpawnOnce.Do(func() {
		uiSpawnCmd = doSpawnUI(logger, dataDir)
	})
	return uiSpawnCmd
}

func doSpawnUI(logger *slog.Logger, dataDir string) *exec.Cmd {
	binPath, err := uiSiblingPath()
	if err != nil {
		logger.Error("locate ui sibling binary", "error", err)
		return nil
	}
	if _, statErr := os.Stat(binPath); statErr != nil {
		logger.Error("ui binary not found — running headless", "path", binPath, "error", statErr)
		return nil
	}

	// PID-file lock — defence-in-depth against duplicate spawns from a
	// second daemon process.
	pidPath := filepath.Join(dataDir, "ui.pid")
	if pidAlive(pidPath) {
		logger.Warn("ui pid file points at a live process — skipping spawn", "pid_file", pidPath)
		return nil
	}

	cmd := exec.Command(binPath)
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()

	if err := cmd.Start(); err != nil {
		logger.Error("spawn ui", "error", err, "path", binPath)
		return nil
	}
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
		logger.Warn("write ui pid file", "error", err, "path", pidPath)
	}
	go pumpUIOutput(logger, "ui.stdout", stdout)
	go pumpUIOutput(logger, "ui.stderr", stderr)
	logger.Info("ui spawned", "pid", cmd.Process.Pid, "path", binPath)
	return cmd
}

// uiSiblingPath resolves astery-engine-tools-ui (with .exe on Windows) next
// to the daemon binary as resolved by os.Executable().
func uiSiblingPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("os.Executable: %w", err)
	}
	dir := filepath.Dir(exe)
	name := "astery-engine-tools-ui"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(dir, name), nil
}

// pidAlive returns true if pidPath exists, contains a parseable integer,
// and that PID is still running. Errors are treated as "not alive" so a
// stale file from a crash doesn't block fresh spawns.
func pidAlive(pidPath string) bool {
	raw, err := os.ReadFile(pidPath)
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(string(raw))
	if err != nil || pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// On unix, signal 0 returns nil if the process exists. On Windows,
	// FindProcess succeeds only for live PIDs so the signal is redundant.
	if runtime.GOOS == "windows" {
		return true
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// pumpUIOutput forwards lines from the child to slog so UI logs flow
// through the SSE tap. Line-buffered via bufio.Scanner so log entries
// always arrive whole — raw r.Read can split a line mid-byte across two
// reads and produce concatenated SSE entries.
func pumpUIOutput(logger *slog.Logger, source string, r io.ReadCloser) {
	if r == nil {
		return
	}
	defer r.Close()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 4096), 1024*1024) // up to 1 MiB per line
	for scanner.Scan() {
		logger.Info("ui output", "source", source, "line", scanner.Text())
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		logger.Debug("ui output stream closed", "source", source, "error", err)
	}
}

// stopUIChild propagates SIGTERM, waits up to 5s, then SIGKILLs.
func stopUIChild(logger *slog.Logger, cmd *exec.Cmd, ctx context.Context) {
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		logger.Warn("signal ui SIGTERM", "error", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
		return
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		<-done
	}
}

// runPair performs the pairing exchange and persists the bundle.
func runPair(ctx context.Context, code, displayName, installationID string, pc *auth.PairingClient, ks auth.Keystore) error {
	host, _ := os.Hostname()
	resp, err := pc.Exchange(ctx, auth.ExchangeRequest{
		DisplayCode: code,
		Device: auth.Device{
			InstallationID: installationID,
			DisplayName:    displayName,
			Hostname:       host,
			OS:             runtime.GOOS,
			Arch:           runtime.GOARCH,
			AppVersion:     version,
			RuntimeVersion: version,
		},
		HwFingerprint: rt.HardwareFingerprint(),
	})
	if err != nil {
		return fmt.Errorf("exchange: %w", err)
	}
	bundle := auth.SessionBundle{
		DeviceID:         resp.Device.ID,
		OrganizationID:   resp.Device.OrganizationID,
		SessionJWT:       resp.Session.Token,
		SessionExpiresAt: resp.Session.ExpiresAt,
		RefreshToken:     resp.Refresh.Token,
		RefreshExpiresAt: resp.Refresh.ExpiresAt,
		HwFingerprint:    rt.HardwareFingerprint(),
	}
	return ks.Save(bundle)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func defaultDisplayName() string {
	host, _ := os.Hostname()
	if host == "" {
		host = "engine-tools"
	}
	return host
}
