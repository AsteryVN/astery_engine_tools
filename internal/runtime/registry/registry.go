// Package registry defines the Executor abstraction the runtime dispatches
// against, plus the in-process registry that holds the wired executors. New
// executors register at boot in cmd/engine-toold/main.go.
package registry

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"
)

// Workload is the desktop-side handle to a cloud-issued workload row.
// Mirrors the cloud's wire shape but trimmed to what executors need.
type Workload struct {
	ID             string         // UUID
	OrganizationID string         // UUID — every executor re-checks
	Type           string         // e.g. "video:clip"
	Version        int            // protocol version (ALWAYS check)
	Payload        map[string]any // workload-type-specific input
	RequiredCaps   map[string]any // e.g. {"ffmpeg": true}
}

// ResourceEstimate is the executor's pre-flight estimate of resource usage.
type ResourceEstimate struct {
	CPUCores  float64       // 1.0 = one full core
	RAMBytes  int64
	DiskBytes int64
	GPU       bool
	Duration  time.Duration // expected runtime
}

// ProgressEvent is what executors push during execution.
type ProgressEvent struct {
	Fraction float64 // 0..1
	Stage    string
	Detail   string
}

// JobHandle is the runtime's sandbox handed to the executor. The executor
// uses it to push progress, append events, write outputs, and persist
// resumable state.
type JobHandle interface {
	ID() string
	Workload() Workload
	ProgressEvents() chan<- ProgressEvent
	AddOutput(ctx context.Context, kind, keySuffix string, r io.Reader, size int64, meta map[string]any) error
	WorkDir() string
	AppendEvent(ctx context.Context, kind, msg string, attrs map[string]any) error
	SaveResumableState(ctx context.Context, state any) error
	LoadResumableState(ctx context.Context, into any) error
}

// Executor is the pluggable workload runner.
type Executor interface {
	ID() string
	CanRun(ctx context.Context, w Workload) bool
	Estimate(w Workload) ResourceEstimate
	Execute(ctx context.Context, h JobHandle) error
	Cancel(jobID string) error
	Recover(ctx context.Context, h JobHandle) error
}

// Registry holds the set of in-process executors keyed by Workload.Type.
type Registry struct {
	mu        sync.RWMutex
	executors map[string]Executor
}

// New constructs an empty registry.
func New() *Registry {
	return &Registry{executors: map[string]Executor{}}
}

// Register adds an executor. Panics on duplicate ID — wiring is build-time.
func (r *Registry) Register(e Executor) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.executors[e.ID()]; ok {
		panic("registry: duplicate executor id: " + e.ID())
	}
	r.executors[e.ID()] = e
}

// Lookup returns the executor for a workload type or nil if unregistered.
func (r *Registry) Lookup(workloadType string) Executor {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.executors[workloadType]
}

// All returns every registered executor — used to advertise capabilities.
func (r *Registry) All() []Executor {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Executor, 0, len(r.executors))
	for _, e := range r.executors {
		out = append(out, e)
	}
	return out
}

// ErrNoExecutor is returned by the scheduler when a workload has no matching
// registered executor (capability advertised but registration missing —
// shouldn't happen at runtime; surfaces as a wide-event).
var ErrNoExecutor = errors.New("no executor registered for workload type")
