package clipvideo

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"time"
)

// Encoder is the H.264 encoder this daemon will use for the 9:16 transcode
// path. The codec name plugs into ffmpeg's `-c:v`; Args are codec-specific
// rate-control flags. HW marks hardware-accelerated paths so callers can
// log/debug the chosen backend.
type Encoder struct {
	Codec string
	HW    bool
	Args  []string
}

// FallbackEncoder is the always-available libx264 software encoder. Quality
// target ≈ CRF 20 with veryfast preset to keep wall-clock low.
var FallbackEncoder = Encoder{
	Codec: "libx264",
	HW:    false,
	Args:  []string{"-preset", "veryfast", "-crf", "20", "-tune", "fastdecode"},
}

// candidate is one encoder we'll consider during probing. priorityOrder
// captures the user-approved preference list (NVENC > AMF > QSV > VideoToolbox
// > libx264; VAAPI deferred until the blur-graph hwupload story is figured
// out).
type candidate struct {
	codec    string
	hw       bool
	args     []string
	platform string // "" = any; otherwise must match runtime.GOOS
}

var priorityOrder = []candidate{
	{
		codec: "h264_nvenc",
		hw:    true,
		// p4 = balanced quality/speed (newer p1-p7 preset namespace).
		// -b:v 0 lets -cq drive constant-quality mode.
		args: []string{"-preset", "p4", "-tune", "hq", "-rc", "vbr", "-cq", "23", "-b:v", "0"},
	},
	{
		codec:    "h264_amf",
		hw:       true,
		platform: "windows",
		// AMF is functional only on Windows. CQP (constant QP) gives
		// predictable quality across drivers.
		args: []string{"-quality", "balanced", "-rc", "cqp", "-qp_i", "22", "-qp_p", "24", "-qp_b", "26"},
	},
	{
		codec: "h264_qsv",
		hw:    true,
		// global_quality ≈ CRF for QSV. look_ahead 0 keeps single-clip
		// latency low; we don't get meaningful BD-rate savings for short clips.
		args: []string{"-preset", "medium", "-global_quality", "22", "-look_ahead", "0"},
	},
	{
		codec:    "h264_videotoolbox",
		hw:       true,
		platform: "darwin",
		// q:v ranges 0-100 (higher = better). 60 ≈ libx264 CRF 20.
		// allow_sw lets the encoder fall back to software inside the codec
		// rather than failing if the GPU is busy.
		args: []string{"-q:v", "60", "-allow_sw", "1"},
	},
}

// SelectEncoder probes available H.264 encoders on this system and returns
// the best one. Strategy:
//
//  1. Run `ffmpeg -hide_banner -encoders` once, parse the encoder list.
//  2. For each candidate (in priority order) whose codec is in that list AND
//     whose platform constraint matches: run a tiny null-encode probe
//     (lavfi color source -> /dev/null) to confirm the GPU/driver actually
//     accepts frames with the candidate's exact rate-control args.
//  3. First candidate that passes wins. If none pass, return FallbackEncoder.
//
// The probe is bounded to ~2 seconds total per candidate via context timeout.
// Returning an error here is reserved for catastrophic ffmpeg invocation
// failures (binary missing, exec denied) — degraded-but-working systems
// always return libx264 with nil error.
func SelectEncoder(ctx context.Context, ffmpegPath string) (Encoder, error) {
	listed, err := listEncoders(ctx, ffmpegPath)
	if err != nil {
		return FallbackEncoder, fmt.Errorf("list encoders: %w", err)
	}

	for _, c := range priorityOrder {
		if c.platform != "" && c.platform != runtime.GOOS {
			continue
		}
		if !listed[c.codec] {
			continue
		}
		if probeEncoder(ctx, ffmpegPath, c) {
			return Encoder{Codec: c.codec, HW: c.hw, Args: c.args}, nil
		}
	}
	return FallbackEncoder, nil
}

// listEncoders runs `ffmpeg -encoders` and returns a set of encoder names
// that appear in the output. ffmpeg lists each encoder on its own line in
// the form ` V..... h264_nvenc           NVIDIA NVENC H.264 encoder` after
// a header. We parse loosely — substring match on the encoder name field.
func listEncoders(ctx context.Context, ffmpegPath string) (map[string]bool, error) {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, ffmpegPath, "-hide_banner", "-encoders").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg -encoders: %w (output: %s)", err, tail(string(out), 256))
	}
	wanted := make([]string, 0, len(priorityOrder))
	for _, c := range priorityOrder {
		wanted = append(wanted, c.codec)
	}
	set := map[string]bool{}
	for line := range strings.SplitSeq(string(out), "\n") {
		for f := range strings.FieldsSeq(line) {
			if slices.Contains(wanted, f) {
				set[f] = true
			}
		}
	}
	return set, nil
}

// probeEncoder runs a 0.1-second null encode to verify the candidate codec
// actually works with its rate-control args on this hardware. Returns true
// on exit code 0; everything else (driver init failure, unsupported preset,
// session-limit exhaustion) is treated as "not viable now".
func probeEncoder(ctx context.Context, ffmpegPath string, c candidate) bool {
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "color=size=64x64:rate=1:duration=0.1",
		"-c:v", c.codec,
	}
	args = append(args, c.args...)
	args = append(args, "-f", "null", "-")
	cmd := exec.CommandContext(cctx, ffmpegPath, args...)
	return cmd.Run() == nil
}
