package voice

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// WhisperSTT transcribes audio using whisper.cpp's CLI.
// Requires whisper-cpp or whisper.cpp main binary on PATH.
type WhisperSTT struct {
	binary string // path to whisper binary
	model  string // path to model file
}

// NewWhisperSTT creates a whisper.cpp subprocess STT.
// binary is the path to the whisper CLI binary (e.g., "whisper-cpp", "main").
// model is the path to the GGML model file.
func NewWhisperSTT(binary, model string) (*WhisperSTT, error) {
	// Resolve the binary path.
	resolved, err := exec.LookPath(binary)
	if err != nil {
		return nil, fmt.Errorf("whisper binary not found: %w", err)
	}

	// Check model file exists.
	if _, err := os.Stat(model); err != nil {
		return nil, fmt.Errorf("whisper model not found: %w", err)
	}

	return &WhisperSTT{
		binary: resolved,
		model:  model,
	}, nil
}

// Transcribe writes PCM audio to a temporary WAV file and runs whisper.
func (w *WhisperSTT) Transcribe(ctx context.Context, audio []byte) (string, error) {
	if len(audio) == 0 {
		return "", nil
	}

	// Write audio as WAV to temp file (whisper expects WAV input).
	tmpDir := os.TempDir()
	wavPath := filepath.Join(tmpDir, "ghost-stt.wav")
	if err := writeWAV(wavPath, audio, 16000); err != nil {
		return "", fmt.Errorf("write temp wav: %w", err)
	}
	defer os.Remove(wavPath)

	// Run whisper.
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, w.binary,
		"--model", w.model,
		"--file", wavPath,
		"--no-timestamps",
		"--output-txt",
		"--language", "en",
		"--threads", "4",
	)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("whisper: %w (stderr: %s)", err, stderr.String())
	}

	// whisper outputs to <input>.txt when --output-txt is used.
	txtPath := wavPath + ".txt"
	out, err := os.ReadFile(txtPath)
	if err != nil {
		// Fall back to stdout.
		return strings.TrimSpace(stdout.String()), nil
	}
	defer os.Remove(txtPath)

	return strings.TrimSpace(string(out)), nil
}

// Close releases resources.
func (w *WhisperSTT) Close() error { return nil }

// writeWAV writes raw PCM data as a WAV file (16-bit mono).
func writeWAV(path string, pcm []byte, sampleRate int) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	dataLen := uint32(len(pcm))
	fileLen := dataLen + 36 // WAV header is 44 bytes, minus 8 for RIFF header

	// RIFF header.
	f.Write([]byte("RIFF"))
	writeLEUint32(f, fileLen)
	f.Write([]byte("WAVE"))

	// fmt chunk.
	f.Write([]byte("fmt "))
	writeLEUint32(f, 16)              // chunk size
	writeLEUint16(f, 1)               // PCM format
	writeLEUint16(f, 1)               // mono
	writeLEUint32(f, uint32(sampleRate))
	writeLEUint32(f, uint32(sampleRate*2)) // byte rate (16-bit mono)
	writeLEUint16(f, 2)               // block align
	writeLEUint16(f, 16)              // bits per sample

	// data chunk.
	f.Write([]byte("data"))
	writeLEUint32(f, dataLen)
	_, err = f.Write(pcm)
	return err
}

func writeLEUint32(f *os.File, v uint32) {
	b := make([]byte, 4)
	b[0] = byte(v)
	b[1] = byte(v >> 8)
	b[2] = byte(v >> 16)
	b[3] = byte(v >> 24)
	f.Write(b)
}

func writeLEUint16(f *os.File, v uint16) {
	b := make([]byte, 2)
	b[0] = byte(v)
	b[1] = byte(v >> 8)
	f.Write(b)
}
