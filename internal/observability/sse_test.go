package observability

import (
	"context"
	"log/slog"
	"runtime"
	"testing"
	"time"
)

// helperLogger builds a fresh hub + fanout handler for the test, isolated
// from package-globals so parallel tests don't cross-pollute.
func helperLogger(_ *testing.T) (*slog.Logger, *hub) {
	h := newHub()
	inner := slog.NewJSONHandler(devNull{}, &slog.HandlerOptions{Level: slog.LevelDebug})
	fan := newFanoutHandler(inner, h)
	return slog.New(fan), h
}

type devNull struct{}

func (devNull) Write(p []byte) (int, error) { return len(p), nil }

func TestHub_SubscribeReceivesLiveEvent(t *testing.T) {
	logger, h := helperLogger(t)
	ch, _ := h.Subscribe(t.Context())
	logger.Info("hello", "k", "v")

	select {
	case ev := <-ch:
		if ev.Msg != "hello" || ev.Level != "info" || ev.KV["k"] != "v" {
			t.Fatalf("unexpected event: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no event arrived")
	}
}

func TestHub_RingReplayOnSubscribe(t *testing.T) {
	logger, h := helperLogger(t)
	for i := range 10 {
		logger.Info("event", "n", i)
	}
	ch, _ := h.Subscribe(t.Context())
	got := make([]int, 0, 10)
	for range 10 {
		select {
		case ev := <-ch:
			got = append(got, int(ev.KV["n"].(int64)))
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("only got %d events", len(got))
		}
	}
	for i := range 10 {
		if got[i] != i {
			t.Fatalf("replay out of order: got[%d]=%d", i, got[i])
		}
	}
}

func TestHub_BackpressureDropsOldest(t *testing.T) {
	logger, h := helperLogger(t)
	preGoroutines := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, _ := h.Subscribe(ctx)

	// Burst 300 events with no consumer reading.
	for i := range 300 {
		logger.Info("burst", "n", i)
	}

	// Drain everything we can without blocking.
	got := drain(ch)

	// Channel cap is subBuf (64). We expect exactly subBuf events.
	if len(got) != subBuf {
		t.Fatalf("expected %d events buffered, got %d", subBuf, len(got))
	}
	// And the newest must be preserved (n=299).
	last := got[len(got)-1]
	if last.KV["n"].(int64) != 299 {
		t.Fatalf("newest dropped: last n=%v", last.KV["n"])
	}
	// First must NOT be n=0 — we dropped oldest.
	if got[0].KV["n"].(int64) == 0 {
		t.Fatal("expected oldest dropped, but n=0 still present")
	}

	cancel()
	// Give the unsubscribe goroutine a beat to exit.
	time.Sleep(50 * time.Millisecond)
	postGoroutines := runtime.NumGoroutine()
	// Allow ±2 for scheduler noise; what we care about is no leak.
	if postGoroutines > preGoroutines+2 {
		t.Fatalf("possible goroutine leak: pre=%d post=%d", preGoroutines, postGoroutines)
	}
}

func TestHub_UnsubscribeViaContext(t *testing.T) {
	logger, h := helperLogger(t)
	ctx, cancel := context.WithCancel(context.Background())
	ch, _ := h.Subscribe(ctx)

	logger.Info("first")
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("first event not delivered")
	}

	cancel()
	// Wait for unsub goroutine to remove subscriber + close channel.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		_, ok := <-ch
		if !ok {
			break
		}
	}
	logger.Info("post-cancel")
	// Channel is closed; reading from it must not block and must return
	// zero-value with ok=false.
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("event delivered after cancel")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("closed channel not draining")
	}
}

func drain(ch <-chan LogEvent) []LogEvent {
	out := []LogEvent{}
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-time.After(20 * time.Millisecond):
			return out
		}
	}
}
