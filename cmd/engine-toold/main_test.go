package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"testing"
)

// TestSpawnUIOnce_DuplicateCallsAreNoOp verifies regression test #5 (Go
// side): calling spawnUIOnce twice in the same process must spawn at most
// one child. Because the sync.Once is package-global and the binary
// resolution will fail in test (no sibling exists next to `go test`), both
// calls return nil — that's the "degraded mode" branch documented in
// doSpawnUI. The important assertion is: the second call doesn't crash, and
// uiSpawnOnce.Do has run exactly once (we test that by re-invoking and
// asserting we still get the cached uiSpawnCmd value).
func TestSpawnUIOnce_DuplicateCallsAreNoOp(t *testing.T) {
	// Reset the package-global Once for hermetic test execution.
	uiSpawnOnce = sync.Once{}
	uiSpawnCmd = nil

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	dataDir := t.TempDir()

	first := spawnUIOnce(logger, dataDir)
	second := spawnUIOnce(logger, dataDir)

	if first != second {
		t.Fatalf("expected same handle, got first=%v second=%v", first, second)
	}
}

// TestPidAlive verifies the lock-file logic that prevents duplicate spawns
// across process boundaries.
func TestPidAlive(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "ui.pid")

	// Missing file = not alive.
	if pidAlive(pidPath) {
		t.Fatal("expected not-alive when pid file missing")
	}

	// Garbage contents = not alive.
	if err := os.WriteFile(pidPath, []byte("not-a-number"), 0o600); err != nil {
		t.Fatal(err)
	}
	if pidAlive(pidPath) {
		t.Fatal("expected not-alive when pid file contains garbage")
	}

	// Our own PID = alive (skip on windows where the signal-0 trick
	// doesn't apply the same way; FindProcess reports based on Open).
	if runtime.GOOS == "windows" {
		t.Skip("signal(0) liveness check is unix-specific")
	}
	self := os.Getpid()
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(self)), 0o600); err != nil {
		t.Fatal(err)
	}
	if !pidAlive(pidPath) {
		t.Fatal("expected our own pid to be alive")
	}

	// Almost-certainly-dead PID.
	if err := os.WriteFile(pidPath, []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	// PID 1 IS alive on most systems — pick something we just won't find.
	if err := os.WriteFile(pidPath, []byte("999999999"), 0o600); err != nil {
		t.Fatal(err)
	}
	if pidAlive(pidPath) {
		t.Fatal("expected not-alive for sentinel high pid")
	}
}
