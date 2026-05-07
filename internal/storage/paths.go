// Package storage owns the on-disk layout for the engine-tools daemon.
// Cross-platform via adrg/xdg: Linux → $XDG_DATA_HOME/astery-engine-tools/,
// macOS → ~/Library/Application Support/astery-engine-tools/, Windows →
// %APPDATA%\astery-engine-tools\.
package storage

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/adrg/xdg"
)

// AppName is the per-OS subdirectory namespace.
const AppName = "astery-engine-tools"

// Layout describes the data dir contents. All paths are absolute and
// guaranteed to exist after EnsureLayout.
type Layout struct {
	Root     string // <data-dir>/
	Cache    string // cache/
	Tmp      string // tmp/
	Runtimes string // runtimes/
	Outputs  string // outputs/
	Logs     string // logs/
	Crashes  string // crashes/
	Secrets  string // secrets/  (only used when keyring unavailable)
	JobsDB   string // jobs.db
	IpcToken string // ipc.token
	IpcPort  string // ipc.port
	Install  string // installation.id
}

// Resolve returns the layout rooted at the override (when non-empty) or the
// per-OS XDG data dir. Paths are not yet created — call EnsureLayout.
func Resolve(override string) (Layout, error) {
	root := override
	if root == "" {
		base, err := xdg.DataFile(AppName + "/.keep")
		if err != nil {
			return Layout{}, fmt.Errorf("resolve data dir: %w", err)
		}
		root = filepath.Dir(base)
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return Layout{}, fmt.Errorf("abs data dir: %w", err)
	}
	return Layout{
		Root:     root,
		Cache:    filepath.Join(root, "cache"),
		Tmp:      filepath.Join(root, "tmp"),
		Runtimes: filepath.Join(root, "runtimes"),
		Outputs:  filepath.Join(root, "outputs"),
		Logs:     filepath.Join(root, "logs"),
		Crashes:  filepath.Join(root, "crashes"),
		Secrets:  filepath.Join(root, "secrets"),
		JobsDB:   filepath.Join(root, "jobs.db"),
		IpcToken: filepath.Join(root, "ipc.token"),
		IpcPort:  filepath.Join(root, "ipc.port"),
		Install:  filepath.Join(root, "installation.id"),
	}, nil
}

// EnsureLayout creates every directory in the layout (mode 0700 — strict so
// the secrets fallback can live next to the cache without leaking).
func (l Layout) EnsureLayout() error {
	for _, dir := range []string{l.Root, l.Cache, l.Tmp, l.Runtimes, l.Outputs, l.Logs, l.Crashes, l.Secrets} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	return nil
}
