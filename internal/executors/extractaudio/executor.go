// Package extractaudio implements registry.Executor for the
// "video:extract-audio" workload. The cloud queues this when a video has
// been uploaded; a paired desktop claims it, extracts the 16 kHz mono PCM
// WAV soundtrack with FFmpeg, and uploads the result so the cloud-side
// transcribe worker can stream it into Whisper.
//
// Workload payload shape:
//
//	{ "video_url": "https://..." }
//
// Output (one entry only):
//
//	kind:        "audio"
//	key_suffix:  "audio.wav"
//	metadata:    { duration_s, sample_rate: 16000, channels: 1, format: "pcm_s16le" }
package extractaudio

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/AsteryVN/astery_engine_tools/internal/runtime/registry"
	"github.com/AsteryVN/astery_engine_tools/internal/tools"
)

// ID is the workload type this executor handles.
const ID = "video:extract-audio"

// SupportedVersion is the highest workload_version this executor speaks.
// Bumped in lock-step with cloud-side payload changes.
const SupportedVersion = 1

// outputKind is the manifest kind cloud-side worker_transcribe.go reads.
const outputKind = "audio"

// outputKeySuffix is the storage key suffix passed to the presigner.
const outputKeySuffix = "audio.wav"

// downloadTimeout caps the master-video fetch. Sized for "large MP4 over
// a slow home connection" — bigger than internal/sync's per-call timeout.
const downloadTimeout = 30 * time.Minute

// ToolLocator is the narrow interface consumed from internal/tools.Manager.
// Defined at the consumer (this package) per project DI conventions so
// tests can substitute a stub without spinning up the real manager.
type ToolLocator interface {
	Locate(ctx context.Context, id string) (tools.Tool, error)
}

// CommandRunner wraps `os/exec`. Production code uses execRunner (real
// processes); tests use a fake that asserts argv without forking.
type CommandRunner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// Downloader streams a remote URL to a local path. Stubbed in tests.
type Downloader interface {
	Download(ctx context.Context, url, dst string) error
}

// Executor implements registry.Executor.
type Executor struct {
	tools  ToolLocator
	runner CommandRunner
	dl     Downloader
}

// New constructs an Executor wired to the production tools manager and
// real ffmpeg / HTTP runners. Override deps via NewWithDeps for tests.
func New(toolsMgr *tools.Manager) *Executor {
	return NewWithDeps(toolsMgr, &execRunner{}, &httpDownloader{})
}

// NewWithDeps is the test-friendly constructor.
func NewWithDeps(t ToolLocator, r CommandRunner, d Downloader) *Executor {
	return &Executor{tools: t, runner: r, dl: d}
}

// ID returns the workload type — required by registry.Executor.
func (e *Executor) ID() string { return ID }

// CanRun returns false for unsupported versions or wrong type.
func (e *Executor) CanRun(_ context.Context, w registry.Workload) bool {
	return w.Type == ID && w.Version <= SupportedVersion
}

// Estimate is coarse — the resource manager does the actual gating.
// Audio extraction is far cheaper than clip transcoding.
func (e *Executor) Estimate(w registry.Workload) registry.ResourceEstimate {
	return registry.ResourceEstimate{
		CPUCores:  1.0,
		RAMBytes:  512 * 1024 * 1024, // 512 MiB headroom
		DiskBytes: 2 * 1024 * 1024 * 1024,
		GPU:       false,
		Duration:  60 * time.Second,
	}
}

// Cancel is a no-op marker — context cancellation surfaces in Execute.
func (e *Executor) Cancel(jobID string) error { return nil }

// payload mirrors the cloud workload payload.
type payload struct {
	VideoURL string `json:"video_url"`
}

// Execute downloads the master, runs ffmpeg, uploads the WAV.
func (e *Executor) Execute(ctx context.Context, h registry.JobHandle) error {
	w := h.Workload()
	var p payload
	if err := decodePayload(w.Payload, &p); err != nil {
		return fmt.Errorf("extract-audio: decode payload: %w", err)
	}
	if p.VideoURL == "" {
		return fmt.Errorf("extract-audio: video_url required")
	}

	// Locate FFmpeg.
	ff, err := e.tools.Locate(ctx, "ffmpeg")
	if err != nil {
		return fmt.Errorf("extract-audio: locate ffmpeg: %w", err)
	}
	_ = h.AppendEvent(ctx, "log", "ffmpeg located",
		map[string]any{"path": ff.Path, "version": ff.Version})

	// Stage tmp files inside the job's WorkDir so the scheduler's
	// cleanup pass reaps them on success or failure.
	inputPath := filepath.Join(h.WorkDir(), "input.mp4")
	outputPath := filepath.Join(h.WorkDir(), outputKeySuffix)

	// Belt-and-braces: even though WorkDir is reaped by the scheduler,
	// remove our two artifacts explicitly so a failure doesn't leak.
	defer func() {
		_ = os.Remove(inputPath)
		_ = os.Remove(outputPath)
	}()

	if err := e.dl.Download(ctx, p.VideoURL, inputPath); err != nil {
		return fmt.Errorf("extract-audio: download master: %w", err)
	}
	_ = h.AppendEvent(ctx, "log", "master downloaded", nil)

	args := BuildExtractArgs(inputPath, outputPath)
	out, runErr := e.runner.Run(ctx, ff.Path, args...)
	if runErr != nil {
		_ = h.AppendEvent(ctx, "error", "ffmpeg failed",
			map[string]any{"stderr_tail": tail(string(out), 1024)})
		return fmt.Errorf("extract-audio: ffmpeg: %w", runErr)
	}

	st, err := os.Stat(outputPath)
	if err != nil {
		return fmt.Errorf("extract-audio: stat output: %w", err)
	}
	size := st.Size()

	// Best-effort duration parse from ffmpeg's stderr ("Duration: HH:MM:SS.ms").
	durSec := parseDurationSeconds(string(out))

	f, err := os.Open(outputPath)
	if err != nil {
		return fmt.Errorf("extract-audio: open output: %w", err)
	}
	uploadErr := h.AddOutput(ctx, outputKind, outputKeySuffix, f, size,
		map[string]any{
			"duration_s":  durSec,
			"sample_rate": 16000,
			"channels":    1,
			"format":      "pcm_s16le",
		})
	_ = f.Close()
	if uploadErr != nil {
		return fmt.Errorf("extract-audio: upload output: %w", uploadErr)
	}

	// Best-effort progress push — runtime forwards via channel.
	select {
	case h.ProgressEvents() <- registry.ProgressEvent{
		Fraction: 1.0,
		Stage:    "uploaded",
		Detail:   "audio extracted",
	}:
	default:
	}
	return nil
}

// Recover re-runs Execute. Audio extraction is cheap enough that we
// don't bother with resumable state — a recovered attempt simply re-
// downloads + re-extracts. Idempotent because outputKeySuffix is fixed.
func (e *Executor) Recover(ctx context.Context, h registry.JobHandle) error {
	return e.Execute(ctx, h)
}

// ─── helpers ────────────────────────────────────────────────────────────

// execRunner is the production CommandRunner — shells out to ffmpeg.
type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// httpDownloader is the production Downloader — streams a URL to disk.
type httpDownloader struct{}

func (httpDownloader) Download(ctx context.Context, rawURL, dst string) error {
	if err := validateDownloadURL(rawURL); err != nil {
		return fmt.Errorf("validate url: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	httpClient := &http.Client{Timeout: downloadTimeout}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("get: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("download status %d", resp.StatusCode)
	}
	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}
	defer out.Close()
	if _, err := io.Copy(out, resp.Body); err != nil {
		return fmt.Errorf("copy: %w", err)
	}
	return nil
}

// validateDownloadURL is the SSRF guard for executor downloads. The cloud
// supplies the master_video_url; a buggy or compromised cloud could direct
// the desktop at internal infrastructure (RFC1918, loopback, link-local,
// cloud metadata endpoints). We reject those at the desktop side as
// defense-in-depth even though the cloud is the trusted enqueuer.
//
// Allowed: https://… and http://… with a public-routable host.
// Rejected: any other scheme; any RFC1918 / loopback / link-local /
// IPv6-link-local / multicast destination; any IPv4 169.254.x.x (covers
// AWS/GCP metadata at 169.254.169.254).
func validateDownloadURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme %q not allowed; use http or https", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("missing host")
	}
	// Resolve the host. If it parses as a literal IP, dispatch directly;
	// otherwise the host is a DNS name and we resolve all A/AAAA records.
	var ips []net.IP
	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IP{ip}
	} else {
		resolved, err := net.LookupIP(host)
		if err != nil {
			return fmt.Errorf("resolve %q: %w", host, err)
		}
		ips = resolved
	}
	for _, ip := range ips {
		if isPrivateOrSpecialIP(ip) {
			return fmt.Errorf("destination %s resolves to non-public address %s", host, ip)
		}
	}
	return nil
}

// isPrivateOrSpecialIP returns true for any address class the SSRF guard
// must reject: loopback, link-local (incl. 169.254.169.254 metadata),
// private (RFC1918), unspecified, multicast, IPv6 unique-local (fc00::/7).
func isPrivateOrSpecialIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() ||
		ip.IsPrivate() {
		return true
	}
	return false
}

// decodePayload re-marshals the registry's map[string]any into a typed
// payload — same trick clipvideo uses.
func decodePayload(in map[string]any, target *payload) error {
	raw, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, target)
}

// parseDurationSeconds scans ffmpeg's combined output for the
// "Duration: HH:MM:SS.ms" line and converts it to fractional seconds.
// Returns 0 on miss — non-fatal; downstream cloud workers can re-derive.
func parseDurationSeconds(stderr string) float64 {
	const marker = "Duration: "
	_, rest, ok := strings.Cut(stderr, marker)
	if !ok {
		return 0
	}
	end := strings.IndexAny(rest, ",\n\r")
	if end < 0 {
		return 0
	}
	hms := strings.TrimSpace(rest[:end])
	parts := strings.Split(hms, ":")
	if len(parts) != 3 {
		return 0
	}
	h, err1 := strconv.ParseFloat(parts[0], 64)
	m, err2 := strconv.ParseFloat(parts[1], 64)
	s, err3 := strconv.ParseFloat(parts[2], 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return 0
	}
	return h*3600 + m*60 + s
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
