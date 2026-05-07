// Package observability — SSE log tap.
//
// This file adds a [LogHub] that fans every slog record out to:
//
//  1. the existing stderr/stdout writer (preserves the current JSON output),
//  2. a 256-entry ring buffer replayed to new SSE subscribers on connect, and
//  3. per-subscriber buffered channels with drop-oldest backpressure so a
//     slow consumer can never stall the daemon's logging path.
//
// The hub is wired up by [Setup] in slog.go so callers don't need to change
// any existing call sites — they keep using slog.Default()/slog.Info etc.
package observability

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// LogEvent is a single slog record reshaped for SSE consumption.
type LogEvent struct {
	TS    time.Time              `json:"ts"`
	Level string                 `json:"level"` // "debug" | "info" | "warn" | "error"
	Msg   string                 `json:"msg"`
	KV    map[string]any         `json:"kv,omitempty"`
}

// LogHub fans log events out to subscribers and keeps a small replay buffer.
type LogHub interface {
	// Subscribe registers a new subscriber. The returned channel receives
	// every event the hub publishes after the replay buffer drains. The
	// returned func unsubscribes; ctx cancellation also unsubscribes.
	Subscribe(ctx context.Context) (<-chan LogEvent, func())
}

// ringSize is the replay capacity. 256 events is enough for a tail-style
// reconnect without leaking memory under sustained logging.
const ringSize = 256

// subBuf is the per-subscriber channel depth. Small on purpose — once full
// we drop oldest, never block.
const subBuf = 64

// hub is the concrete LogHub.
type hub struct {
	mu     sync.Mutex
	ring   []LogEvent // ring buffer; len == count up to ringSize, then ringSize.
	head   int        // next write index, modulo ringSize, valid once full=true.
	full   bool
	subs   map[*subscriber]struct{}
}

type subscriber struct {
	ch     chan LogEvent
	closed atomic.Bool
}

// newHub returns an empty hub.
func newHub() *hub {
	return &hub{
		ring: make([]LogEvent, 0, ringSize),
		subs: map[*subscriber]struct{}{},
	}
}

// publish stores the event in the ring and dispatches to all subscribers.
// Drop-oldest semantics: if a subscriber's channel is full, we discard the
// oldest in-flight event for that subscriber to make room for the newest.
func (h *hub) publish(ev LogEvent) {
	h.mu.Lock()
	// Append to ring.
	if !h.full {
		h.ring = append(h.ring, ev)
		if len(h.ring) == ringSize {
			h.full = true
			h.head = 0
		}
	} else {
		h.ring[h.head] = ev
		h.head = (h.head + 1) % ringSize
	}
	subs := make([]*subscriber, 0, len(h.subs))
	for s := range h.subs {
		subs = append(subs, s)
	}
	h.mu.Unlock()

	for _, s := range subs {
		if s.closed.Load() {
			continue
		}
		select {
		case s.ch <- ev:
		default:
			// Drop oldest, then deliver newest. Non-blocking.
			select {
			case <-s.ch:
			default:
			}
			select {
			case s.ch <- ev:
			default:
				// Subscriber raced into closed state; skip.
			}
		}
	}
}

// Subscribe implements LogHub.
func (h *hub) Subscribe(ctx context.Context) (<-chan LogEvent, func()) {
	s := &subscriber{ch: make(chan LogEvent, subBuf)}

	// Replay the current ring before attaching to live stream so the
	// subscriber sees a contiguous history then live events. We hold the
	// hub lock across replay+register so no live event slips between them.
	h.mu.Lock()
	replay := h.snapshotLocked()
	h.subs[s] = struct{}{}
	h.mu.Unlock()

	// Drain replay into the channel synchronously up to subBuf, then drop
	// oldest if the replay itself overflows the buffer (slow caller — same
	// drop-oldest contract as live events).
	for _, ev := range replay {
		select {
		case s.ch <- ev:
		default:
			select {
			case <-s.ch:
			default:
			}
			select {
			case s.ch <- ev:
			default:
			}
		}
	}

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, s)
			h.mu.Unlock()
			s.closed.Store(true)
			close(s.ch)
		})
	}

	// Tie ctx cancellation to unsubscribe so callers don't have to wire two
	// teardown paths.
	go func() {
		<-ctx.Done()
		cancel()
	}()

	return s.ch, cancel
}

// snapshotLocked is snapshot() but assumes the caller holds h.mu.
func (h *hub) snapshotLocked() []LogEvent {
	if !h.full {
		out := make([]LogEvent, len(h.ring))
		copy(out, h.ring)
		return out
	}
	out := make([]LogEvent, 0, ringSize)
	out = append(out, h.ring[h.head:]...)
	out = append(out, h.ring[:h.head]...)
	return out
}

// ─── slog.Handler implementation ─────────────────────────────────────────

// fanoutHandler wraps an inner slog.Handler (the existing stderr JSON
// handler) and additionally publishes every record into the [hub]. It does
// NOT replace the inner handler — both run.
type fanoutHandler struct {
	inner slog.Handler
	hub   *hub
	attrs []slog.Attr
	group string
}

func newFanoutHandler(inner slog.Handler, h *hub) *fanoutHandler {
	return &fanoutHandler{inner: inner, hub: h}
}

// Enabled defers to the inner handler — log levels are decided once.
func (f *fanoutHandler) Enabled(ctx context.Context, lvl slog.Level) bool {
	return f.inner.Enabled(ctx, lvl)
}

// Handle writes through to the inner handler AND publishes a LogEvent.
func (f *fanoutHandler) Handle(ctx context.Context, r slog.Record) error {
	// Write to stderr/stdout first so a hub-side panic can never silently
	// drop the canonical log line.
	if err := f.inner.Handle(ctx, r); err != nil {
		// Don't fail logging just because the inner handler failed —
		// continue to publish so SSE consumers still see the event.
		_ = err
	}

	kv := make(map[string]any, len(f.attrs)+r.NumAttrs())
	for _, a := range f.attrs {
		kv[a.Key] = a.Value.Any()
	}
	r.Attrs(func(a slog.Attr) bool {
		kv[a.Key] = a.Value.Any()
		return true
	})
	if len(kv) == 0 {
		kv = nil
	}

	ts := r.Time
	if ts.IsZero() {
		ts = time.Now()
	}
	f.hub.publish(LogEvent{
		TS:    ts,
		Level: levelString(r.Level),
		Msg:   r.Message,
		KV:    kv,
	})
	return nil
}

// WithAttrs forks the handler — both inner and our cached attr slice need to
// know about the new attrs so SSE subscribers see them too.
func (f *fanoutHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	merged := make([]slog.Attr, 0, len(f.attrs)+len(attrs))
	merged = append(merged, f.attrs...)
	merged = append(merged, attrs...)
	return &fanoutHandler{
		inner: f.inner.WithAttrs(attrs),
		hub:   f.hub,
		attrs: merged,
		group: f.group,
	}
}

// WithGroup defers to inner; we don't reshape KV by group for SSE consumers
// (they just see flat key/value pairs).
func (f *fanoutHandler) WithGroup(name string) slog.Handler {
	return &fanoutHandler{
		inner: f.inner.WithGroup(name),
		hub:   f.hub,
		attrs: f.attrs,
		group: name,
	}
}

// levelString maps slog levels to the strings the SSE schema expects.
func levelString(lvl slog.Level) string {
	switch {
	case lvl >= slog.LevelError:
		return "error"
	case lvl >= slog.LevelWarn:
		return "warn"
	case lvl >= slog.LevelInfo:
		return "info"
	default:
		return "debug"
	}
}
