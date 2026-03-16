package voice

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
)

// OGGToWAV converts OGG/Opus audio to 16kHz mono WAV using ffmpeg.
// Returns the WAV bytes suitable for whisper/assemblyai transcription.
func OGGToWAV(ctx context.Context, oggData []byte) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-i", "pipe:0",
		"-ar", "16000",
		"-ac", "1",
		"-f", "wav",
		"pipe:1",
	)
	cmd.Stdin = bytes.NewReader(oggData)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg: %w (stderr: %s)", err, stderr.String())
	}

	return stdout.Bytes(), nil
}

// WAVToOGG converts WAV/PCM audio to OGG/Opus for Telegram voice messages.
func WAVToOGG(ctx context.Context, wavData []byte) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-i", "pipe:0",
		"-c:a", "libopus",
		"-b:a", "64k",
		"-f", "ogg",
		"pipe:1",
	)
	cmd.Stdin = bytes.NewReader(wavData)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg: %w (stderr: %s)", err, stderr.String())
	}

	return stdout.Bytes(), nil
}
