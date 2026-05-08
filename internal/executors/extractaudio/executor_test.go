package extractaudio

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/AsteryVN/astery_engine_tools/internal/runtime/registry"
	"github.com/AsteryVN/astery_engine_tools/internal/tools"
)

// ─── BuildExtractArgs ───────────────────────────────────────────────────

func TestBuildExtractArgs_ProducesWhisperFriendlyWAV(t *testing.T) {
	got := BuildExtractArgs("/tmp/in.mp4", "/tmp/out.wav")
	want := []string{
		"-y",
		"-i", "/tmp/in.mp4",
		"-vn",
		"-acodec", "pcm_s16le",
		"-ar", "16000",
		"-ac", "1",
		"/tmp/out.wav",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("BuildExtractArgs mismatch\n got: %v\nwant: %v", got, want)
	}
}

func TestBuildExtractArgs_HasOverwriteFlag(t *testing.T) {
	args := BuildExtractArgs("a", "b")
	if len(args) == 0 || args[0] != "-y" {
		t.Fatalf("expected first arg to be -y for idempotent re-runs, got %v", args)
	}
}

// ─── parseDurationSeconds ───────────────────────────────────────────────

func TestParseDurationSeconds(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want float64
	}{
		{"hms with ms", "Duration: 00:02:00.50, start: 0.000000", 120.5},
		{"hms exact", "blah\n  Duration: 01:30:45.00, bitrate: ...\n", 5445.0},
		{"missing marker", "no duration here", 0},
		{"malformed", "Duration: not-a-time, ...", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseDurationSeconds(c.in); got != c.want {
				t.Errorf("parseDurationSeconds(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

// ─── Executor.Execute ───────────────────────────────────────────────────

func TestExecute_RejectsEmptyVideoURL(t *testing.T) {
	e := NewWithDeps(stubLocator{}, &fakeRunner{}, &fakeDownloader{})
	h := newFakeHandle(t, registry.Workload{
		Type: ID, Version: 1, Payload: map[string]any{"video_url": ""},
	})
	err := e.Execute(context.Background(), h)
	if err == nil {
		t.Fatal("expected error for empty video_url")
	}
}

func TestExecute_LocatorError(t *testing.T) {
	wantErr := errors.New("ffmpeg missing")
	e := NewWithDeps(stubLocator{err: wantErr}, &fakeRunner{}, &fakeDownloader{})
	h := newFakeHandle(t, validWorkload())
	err := e.Execute(context.Background(), h)
	if err == nil || !errors.Is(err, wantErr) {
		t.Fatalf("expected wrapped locator error, got %v", err)
	}
}

func TestExecute_DownloaderError(t *testing.T) {
	wantErr := errors.New("net down")
	e := NewWithDeps(stubLocator{path: "/usr/bin/ffmpeg"}, &fakeRunner{}, &fakeDownloader{err: wantErr})
	h := newFakeHandle(t, validWorkload())
	err := e.Execute(context.Background(), h)
	if err == nil || !errors.Is(err, wantErr) {
		t.Fatalf("expected wrapped downloader error, got %v", err)
	}
}

func TestExecute_FFmpegError(t *testing.T) {
	wantErr := errors.New("encoder bork")
	e := NewWithDeps(
		stubLocator{path: "/usr/bin/ffmpeg"},
		&fakeRunner{err: wantErr, stderr: "...stuff..."},
		&fakeDownloader{writeBytes: []byte("fake mp4")},
	)
	h := newFakeHandle(t, validWorkload())
	err := e.Execute(context.Background(), h)
	if err == nil || !errors.Is(err, wantErr) {
		t.Fatalf("expected wrapped ffmpeg error, got %v", err)
	}
}

func TestExecute_HappyPath_UploadsAudioOutput(t *testing.T) {
	runner := &fakeRunner{
		// Simulate ffmpeg stderr that includes a Duration line.
		stderr: "ffmpeg version foo\nDuration: 00:01:30.25, start: 0\n",
		// On Run, also write a fake WAV to the output path.
		writeOutput: []byte("RIFF....WAVE-fake"),
	}
	dl := &fakeDownloader{writeBytes: []byte("fake mp4 bytes")}
	e := NewWithDeps(stubLocator{path: "/usr/bin/ffmpeg", version: "6.0"}, runner, dl)
	h := newFakeHandle(t, validWorkload())

	if err := e.Execute(context.Background(), h); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// FFmpeg invoked with the Whisper-friendly argv.
	if runner.gotName != "/usr/bin/ffmpeg" {
		t.Errorf("ffmpeg path = %q, want /usr/bin/ffmpeg", runner.gotName)
	}
	expectedArgs := BuildExtractArgs(
		filepath.Join(h.workDir, "input.mp4"),
		filepath.Join(h.workDir, outputKeySuffix),
	)
	if !reflect.DeepEqual(runner.gotArgs, expectedArgs) {
		t.Errorf("ffmpeg argv\n got: %v\nwant: %v", runner.gotArgs, expectedArgs)
	}

	// Exactly one output uploaded with the right kind + metadata.
	if len(h.outputs) != 1 {
		t.Fatalf("expected 1 output, got %d", len(h.outputs))
	}
	out := h.outputs[0]
	if out.kind != outputKind {
		t.Errorf("output.kind = %q, want %q", out.kind, outputKind)
	}
	if out.keySuffix != outputKeySuffix {
		t.Errorf("output.keySuffix = %q, want %q", out.keySuffix, outputKeySuffix)
	}
	for _, k := range []string{"duration_s", "sample_rate", "channels", "format"} {
		if _, ok := out.meta[k]; !ok {
			t.Errorf("output.meta missing %q", k)
		}
	}
	if got := out.meta["sample_rate"]; got != 16000 {
		t.Errorf("sample_rate = %v, want 16000", got)
	}
	if got := out.meta["channels"]; got != 1 {
		t.Errorf("channels = %v, want 1", got)
	}
	if got := out.meta["format"]; got != "pcm_s16le" {
		t.Errorf("format = %v, want pcm_s16le", got)
	}
	if got := out.meta["duration_s"].(float64); got != 90.25 {
		t.Errorf("duration_s = %v, want 90.25", got)
	}
}

func TestExecute_CleansUpTempFilesOnFailure(t *testing.T) {
	runner := &fakeRunner{err: errors.New("ffmpeg crashed")}
	dl := &fakeDownloader{writeBytes: []byte("master")}
	e := NewWithDeps(stubLocator{path: "/usr/bin/ffmpeg"}, runner, dl)
	h := newFakeHandle(t, validWorkload())

	_ = e.Execute(context.Background(), h)

	if _, err := os.Stat(filepath.Join(h.workDir, "input.mp4")); !os.IsNotExist(err) {
		t.Errorf("expected input.mp4 cleaned up, stat err = %v", err)
	}
}

// ─── CanRun ─────────────────────────────────────────────────────────────

func TestCanRun(t *testing.T) {
	e := New(nil)
	cases := []struct {
		name string
		w    registry.Workload
		want bool
	}{
		{"matches type+version", registry.Workload{Type: ID, Version: 1}, true},
		{"wrong type", registry.Workload{Type: "clip-video", Version: 1}, false},
		{"future version", registry.Workload{Type: ID, Version: 99}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := e.CanRun(context.Background(), c.w); got != c.want {
				t.Errorf("CanRun = %v, want %v", got, c.want)
			}
		})
	}
}

// ─── stubs / fakes ──────────────────────────────────────────────────────

func validWorkload() registry.Workload {
	return registry.Workload{
		Type:    ID,
		Version: 1,
		Payload: map[string]any{"video_url": "https://example.com/master.mp4"},
	}
}

type stubLocator struct {
	path    string
	version string
	err     error
}

func (s stubLocator) Locate(_ context.Context, _ string) (tools.Tool, error) {
	if s.err != nil {
		return tools.Tool{}, s.err
	}
	return tools.Tool{ID: "ffmpeg", Path: s.path, Version: s.version}, nil
}

type fakeRunner struct {
	gotName     string
	gotArgs     []string
	stderr      string
	writeOutput []byte // optional bytes to write to the output path (last argv entry)
	err         error
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.gotName = name
	f.gotArgs = args
	if len(f.writeOutput) > 0 && len(args) > 0 {
		// Output path is the final positional arg per BuildExtractArgs.
		out := args[len(args)-1]
		_ = os.WriteFile(out, f.writeOutput, 0o644)
	}
	return []byte(f.stderr), f.err
}

type fakeDownloader struct {
	writeBytes []byte
	err        error
}

func (f *fakeDownloader) Download(_ context.Context, _ string, dst string) error {
	if f.err != nil {
		return f.err
	}
	return os.WriteFile(dst, f.writeBytes, 0o644)
}

// fakeJobHandle is the minimum surface registry.JobHandle that the
// extractaudio executor touches.
type fakeJobHandle struct {
	t        *testing.T
	id       string
	workload registry.Workload
	workDir  string
	progress chan registry.ProgressEvent
	outputs  []capturedOutput
	events   []capturedEvent
}

type capturedOutput struct {
	kind, keySuffix string
	size            int64
	meta            map[string]any
	body            []byte
}

type capturedEvent struct {
	kind, msg string
	attrs     map[string]any
}

func newFakeHandle(t *testing.T, w registry.Workload) *fakeJobHandle {
	t.Helper()
	dir := t.TempDir()
	return &fakeJobHandle{
		t:        t,
		id:       "test-job",
		workload: w,
		workDir:  dir,
		progress: make(chan registry.ProgressEvent, 4),
	}
}

func (h *fakeJobHandle) ID() string                            { return h.id }
func (h *fakeJobHandle) Workload() registry.Workload           { return h.workload }
func (h *fakeJobHandle) ProgressEvents() chan<- registry.ProgressEvent {
	return h.progress
}
func (h *fakeJobHandle) WorkDir() string { return h.workDir }

func (h *fakeJobHandle) AddOutput(_ context.Context, kind, keySuffix string, r io.Reader, size int64, meta map[string]any) error {
	body, _ := io.ReadAll(r)
	h.outputs = append(h.outputs, capturedOutput{
		kind: kind, keySuffix: keySuffix, size: size, meta: meta, body: body,
	})
	return nil
}

func (h *fakeJobHandle) AppendEvent(_ context.Context, kind, msg string, attrs map[string]any) error {
	h.events = append(h.events, capturedEvent{kind: kind, msg: msg, attrs: attrs})
	return nil
}

func (h *fakeJobHandle) SaveResumableState(_ context.Context, _ any) error { return nil }
func (h *fakeJobHandle) LoadResumableState(_ context.Context, _ any) error { return nil }

// ─── SSRF guard ─────────────────────────────────────────────────────────

// TestValidateDownloadURL_RejectsNonHTTPSchemes confirms the SSRF guard
// rejects schemes a presigner would never produce. file:// is the canonical
// path-traversal escalation; ftp:// is the legacy variant; data: hands the
// downloader an in-band payload.
func TestValidateDownloadURL_RejectsNonHTTPSchemes(t *testing.T) {
	for _, raw := range []string{
		"file:///etc/passwd",
		"ftp://example.com/x.mp4",
		"data:text/plain,hello",
		"gopher://internal/",
	} {
		if err := validateDownloadURL(raw); err == nil {
			t.Errorf("validateDownloadURL(%q) = nil, want scheme-rejection error", raw)
		}
	}
}

// TestValidateDownloadURL_RejectsPrivateAndSpecialAddresses covers the SSRF
// classes the cloud-trusted enqueue must never reach: loopback, RFC1918,
// link-local (incl. 169.254.169.254 cloud metadata), unspecified, multicast,
// IPv6 loopback. We use IP literals so the test does not depend on DNS.
func TestValidateDownloadURL_RejectsPrivateAndSpecialAddresses(t *testing.T) {
	for _, raw := range []string{
		"http://127.0.0.1/x.mp4",
		"http://10.0.0.5/x.mp4",
		"http://192.168.1.10/x.mp4",
		"http://172.16.0.1/x.mp4",
		"http://169.254.169.254/latest/meta-data/", // AWS/GCP metadata
		"http://0.0.0.0/x.mp4",
		"http://[::1]/x.mp4",
		"http://[fe80::1]/x.mp4",
		"http://224.0.0.1/x.mp4", // multicast
	} {
		if err := validateDownloadURL(raw); err == nil {
			t.Errorf("validateDownloadURL(%q) = nil, want non-public-address rejection", raw)
		}
	}
}

// TestValidateDownloadURL_AllowsPublicHTTPS confirms a normal presigned URL
// to a public host (S3-style) passes. We use a literal public IP so the
// test stays hermetic — DNS resolution of "s3.amazonaws.com" would still
// hit the lookup path.
func TestValidateDownloadURL_AllowsPublicHTTPS(t *testing.T) {
	// 1.1.1.1 is the canonical "public" IP literal.
	for _, raw := range []string{
		"https://1.1.1.1/bucket/key.mp4",
		"https://1.1.1.1:443/bucket/key.mp4?signature=abc",
	} {
		if err := validateDownloadURL(raw); err != nil {
			t.Errorf("validateDownloadURL(%q) = %v, want nil", raw, err)
		}
	}
}
