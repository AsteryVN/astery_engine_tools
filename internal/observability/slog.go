// Package observability sets up structured logging that mirrors Astery's
// wide-events convention (see wiki/sources/logging-best-practices.md in the
// cloud repo). Default attrs identify the daemon + installation + version
// so log lines are pivotable in Loki / Grafana.
package observability

import (
	"context"
	"log/slog"
	"os"
)

// Attrs is the set of default fields stamped onto every log line.
type Attrs struct {
	Service        string // always "engine-toold"
	AppVersion     string
	RuntimeVersion string
	InstallationID string
	OS             string
	Arch           string
	DeviceID       string // empty until paired
}

// Setup configures the global slog default handler. Returns the configured
// logger so the caller can pass it into modules that prefer explicit DI.
func Setup(level slog.Level, a Attrs) *slog.Logger {
	base := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	attrs := []slog.Attr{
		slog.String("service", "engine-toold"),
		slog.String("app_version", a.AppVersion),
		slog.String("runtime_version", a.RuntimeVersion),
		slog.String("installation_id", a.InstallationID),
		slog.String("os", a.OS),
		slog.String("arch", a.Arch),
	}
	if a.DeviceID != "" {
		attrs = append(attrs, slog.String("device_id", a.DeviceID))
	}
	logger := slog.New(base.WithAttrs(attrs))
	slog.SetDefault(logger)
	return logger
}

// WithJob returns a context whose slog calls include workload + job ids.
// Use inside Executor.Execute so wide events carry the per-job pivot keys.
func WithJob(ctx context.Context, workloadID, jobID, executorID string) context.Context {
	return WithAttrs(ctx,
		slog.String("workload_id", workloadID),
		slog.String("job_id", jobID),
		slog.String("executor", executorID),
	)
}

type ctxKey struct{}

// WithAttrs returns a context whose Logger() picks up the supplied attrs.
func WithAttrs(ctx context.Context, attrs ...slog.Attr) context.Context {
	prev, _ := ctx.Value(ctxKey{}).([]slog.Attr)
	merged := append([]slog.Attr(nil), prev...)
	merged = append(merged, attrs...)
	return context.WithValue(ctx, ctxKey{}, merged)
}

// Logger returns a logger with attrs added by WithAttrs (if any).
func Logger(ctx context.Context) *slog.Logger {
	attrs, _ := ctx.Value(ctxKey{}).([]slog.Attr)
	if len(attrs) == 0 {
		return slog.Default()
	}
	args := make([]any, 0, len(attrs)*2)
	for _, a := range attrs {
		args = append(args, a)
	}
	return slog.Default().With(args...)
}
