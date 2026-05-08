package clipvideo

import "fmt"

// buildFFmpegArgs returns the FFmpeg argv for one clip.
//
//	16:9  → stream copy (no transcode, no encoder needed).
//	9:16  → blur-padded composition: source frame is split, the bg copy is
//	        scaled+cropped to fill 1080x1920 and box-blurred, the fg copy is
//	        scaled to fit (preserving aspect, no crop), then overlaid centered.
//	        The encoder is whatever SelectEncoder picked at first-Execute time.
//
// The 9:16 graph runs entirely on CPU — hardware encoders accept the
// resulting frames via ffmpeg's automatic upload/format conversion. Moving
// the filter graph onto NPP/QSV/AMF surfaces would force per-backend filter
// rewrites; we skip that complexity for now.
func buildFFmpegArgs(master, dst string, spec ClipSpec, aspect string, enc Encoder) []string {
	if aspect == "9:16" {
		args := []string{
			"-y",
			"-ss", fmt.Sprintf("%.3f", spec.StartSeconds),
			"-to", fmt.Sprintf("%.3f", spec.EndSeconds),
			"-i", master,
			"-vf",
			"split[v1][v2];" +
				"[v1]scale=1080:1920:force_original_aspect_ratio=increase," +
				"crop=1080:1920,boxblur=10:1[bg];" +
				"[v2]scale=1080:1920:force_original_aspect_ratio=decrease[fg];" +
				"[bg][fg]overlay=(W-w)/2:(H-h)/2",
			"-c:v", enc.Codec,
		}
		args = append(args, enc.Args...)
		args = append(args,
			"-pix_fmt", "yuv420p",
			"-c:a", "aac", "-b:a", "128k",
			"-movflags", "+faststart",
			dst,
		)
		return args
	}
	// 16:9 — stream copy (no transcode).
	return []string{
		"-y",
		"-ss", fmt.Sprintf("%.3f", spec.StartSeconds),
		"-to", fmt.Sprintf("%.3f", spec.EndSeconds),
		"-i", master,
		"-c", "copy",
		"-movflags", "+faststart",
		dst,
	}
}
