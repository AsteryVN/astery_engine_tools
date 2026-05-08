// Package extractaudio is the FFmpeg-backed extract-audio executor. It
// downloads a master video, extracts a 16 kHz mono PCM-WAV audio track,
// and uploads the result as a workload output for downstream cloud
// transcription (Whisper).
package extractaudio

// BuildExtractArgs returns the FFmpeg argv that strips video and emits a
// 16 kHz mono signed-16-bit PCM WAV — the canonical input shape for
// Whisper / faster-whisper. Caller passes absolute input + output paths.
//
// Encoding rationale:
//   -vn                 drop the video stream
//   -acodec pcm_s16le   uncompressed PCM (lossless, Whisper-friendly)
//   -ar 16000           16 kHz — Whisper's native sample rate
//   -ac 1               mono — Whisper ignores extra channels
//   -y                  overwrite without prompting (idempotent re-runs)
func BuildExtractArgs(input, output string) []string {
	return []string{
		"-y",
		"-i", input,
		"-vn",
		"-acodec", "pcm_s16le",
		"-ar", "16000",
		"-ac", "1",
		output,
	}
}
