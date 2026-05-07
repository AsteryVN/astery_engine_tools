// Package resources advises the scheduler on whether to dispatch a new
// workload — concurrent-job slot tracking + CPU/RAM/disk thresholds via
// gopsutil. MVP is advisory only (in-process executors can't be hard-
// limited; future out-of-process executors get cgroups / Job Objects).
package resources

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"
)

// Limits configures Manager. Defaults applied when zero.
type Limits struct {
	MaxConcurrentJobs int
	MaxCPUPercent     float64
	MaxRAMPercent     float64
	MinDiskFreeGB     float64
}

// Defaults — small enough for a personal machine without freezing the UI.
const (
	DefaultMaxConcurrentJobs = 2
	DefaultMaxCPUPercent     = 70.0
	DefaultMaxRAMPercent     = 70.0
	DefaultMinDiskFreeGB     = 5.0
)

// Manager tracks active jobs + resource thresholds.
type Manager struct {
	limits   Limits
	active   int32 // atomic
	mu       sync.Mutex
	diskPath string // path to probe for free disk
}

// New constructs a Manager. diskPath is the path to check disk free space
// against (typically the engine-tools data dir).
func New(limits Limits, diskPath string) *Manager {
	if limits.MaxConcurrentJobs <= 0 {
		limits.MaxConcurrentJobs = DefaultMaxConcurrentJobs
	}
	if limits.MaxCPUPercent <= 0 {
		limits.MaxCPUPercent = DefaultMaxCPUPercent
	}
	if limits.MaxRAMPercent <= 0 {
		limits.MaxRAMPercent = DefaultMaxRAMPercent
	}
	if limits.MinDiskFreeGB <= 0 {
		limits.MinDiskFreeGB = DefaultMinDiskFreeGB
	}
	return &Manager{limits: limits, diskPath: diskPath}
}

// ErrResourceUnavailable is returned by Reserve when a check fails.
var ErrResourceUnavailable = errors.New("resource limit reached")

// Slot is the opaque handle returned by Reserve and surrendered via Release.
type Slot struct {
	mgr *Manager
	released bool
}

// Reserve attempts to claim a concurrent-job slot. Performs the gopsutil
// probes inline so a misbehaving system can't sneak past the limits.
func (m *Manager) Reserve(ctx context.Context) (*Slot, error) {
	if int(atomic.LoadInt32(&m.active)) >= m.limits.MaxConcurrentJobs {
		return nil, ErrResourceUnavailable
	}
	cpuPct, err := cpu.PercentWithContext(ctx, 0, false)
	if err == nil && len(cpuPct) > 0 && cpuPct[0] > m.limits.MaxCPUPercent {
		return nil, ErrResourceUnavailable
	}
	if v, err := mem.VirtualMemoryWithContext(ctx); err == nil && v.UsedPercent > m.limits.MaxRAMPercent {
		return nil, ErrResourceUnavailable
	}
	if u, err := disk.UsageWithContext(ctx, m.diskPath); err == nil {
		freeGB := float64(u.Free) / (1024 * 1024 * 1024)
		if freeGB < m.limits.MinDiskFreeGB {
			return nil, ErrResourceUnavailable
		}
	}
	atomic.AddInt32(&m.active, 1)
	return &Slot{mgr: m}, nil
}

// Release returns a slot. Idempotent.
func (s *Slot) Release() {
	if s == nil || s.released {
		return
	}
	s.released = true
	atomic.AddInt32(&s.mgr.active, -1)
}

// Active returns the current number of in-flight jobs.
func (m *Manager) Active() int {
	return int(atomic.LoadInt32(&m.active))
}

// Snapshot returns a debug-friendly view of current resource state. Used by
// /v1/capabilities IPC route.
type Snapshot struct {
	Active        int
	MaxConcurrent int
	CPUPercent    float64
	RAMUsedPct    float64
	DiskFreeGB    float64
}

// Probe returns a one-shot snapshot.
func (m *Manager) Probe(ctx context.Context) Snapshot {
	snap := Snapshot{
		Active:        m.Active(),
		MaxConcurrent: m.limits.MaxConcurrentJobs,
	}
	if cpuPct, err := cpu.PercentWithContext(ctx, 0, false); err == nil && len(cpuPct) > 0 {
		snap.CPUPercent = cpuPct[0]
	}
	if v, err := mem.VirtualMemoryWithContext(ctx); err == nil {
		snap.RAMUsedPct = v.UsedPercent
	}
	if u, err := disk.UsageWithContext(ctx, m.diskPath); err == nil {
		snap.DiskFreeGB = float64(u.Free) / (1024 * 1024 * 1024)
	}
	return snap
}
