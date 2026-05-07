package sync

import (
	"context"
	"log/slog"
	"time"
)

// HeartbeatTicker maintains a per-job heartbeat goroutine. Stops when ctx is
// cancelled OR when Stop() is called. Errors fire onLost when the cloud
// returns 409 (lease stolen) so the executor can cancel.
type HeartbeatTicker struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// HeartbeatDeps bundles inputs.
type HeartbeatDeps struct {
	Client     *Client
	WorkloadID string
	LeaseToken string
	Interval   time.Duration
	OnLost     func(err error) // fires once when the lease is lost
}

// StartHeartbeat launches a heartbeat ticker. Returns a HeartbeatTicker the
// caller MUST Stop() when execution finishes.
func StartHeartbeat(ctx context.Context, deps HeartbeatDeps) *HeartbeatTicker {
	if deps.Interval <= 0 {
		deps.Interval = 25 * time.Second
	}
	ctx, cancel := context.WithCancel(ctx)
	t := &HeartbeatTicker{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(t.done)
		ticker := time.NewTicker(deps.Interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := deps.Client.Heartbeat(ctx, deps.WorkloadID, deps.LeaseToken); err != nil {
					slog.WarnContext(ctx, "heartbeat failed", "workload_id", deps.WorkloadID, "error", err)
					if deps.OnLost != nil {
						deps.OnLost(err)
					}
					return
				}
			}
		}
	}()
	return t
}

// Stop the ticker and wait for the goroutine to exit.
func (t *HeartbeatTicker) Stop() {
	if t == nil {
		return
	}
	t.cancel()
	<-t.done
}
