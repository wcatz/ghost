package voice

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
)

// PiperTTS synthesizes speech using the piper TTS CLI.
// Piper reads text from stdin and outputs raw PCM to stdout.
type PiperTTS struct {
	binary string
	model  string
	rate   float64
}

// NewPiperTTS creates a Piper subprocess TTS.
// binary is the path to the piper CLI (e.g., "piper").
// model is the path to the ONNX model file.
func NewPiperTTS(binary, model string) (*PiperTTS, error) {
	resolved, err := exec.LookPath(binary)
	if err != nil {
		return nil, fmt.Errorf("piper binary not found: %w", err)
	}

	return &PiperTTS{
		binary: resolved,
		model:  model,
		rate:   1.0,
	}, nil
}

// SetRate sets the speech rate multiplier.
func (p *PiperTTS) SetRate(rate float64) {
	if rate > 0 {
		p.rate = rate
	}
}

// Synthesize converts text to PCM audio via piper.
// Returns a reader with raw 16-bit mono PCM at 22050 Hz (piper default).
func (p *PiperTTS) Synthesize(ctx context.Context, text string) (io.Reader, error) {
	if text == "" {
		return bytes.NewReader(nil), nil
	}

	args := []string{
		"--model", p.model,
		"--output-raw",
	}
	if p.rate != 1.0 {
		args = append(args, "--length-scale", fmt.Sprintf("%.2f", 1.0/p.rate))
	}

	cmd := exec.CommandContext(ctx, p.binary, args...)
	cmd.Stdin = bytes.NewReader([]byte(text))

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("piper: %w (stderr: %s)", err, stderr.String())
	}

	return &stdout, nil
}

// Close releases resources.
func (p *PiperTTS) Close() error { return nil }

// EspeakTTS is a fallback TTS using espeak-ng.
type EspeakTTS struct {
	binary string
	voice  string
	rate   int // words per minute
}

// NewEspeakTTS creates an espeak-ng subprocess TTS.
func NewEspeakTTS(voice string) (*EspeakTTS, error) {
	resolved, err := exec.LookPath("espeak-ng")
	if err != nil {
		// Try plain espeak.
		resolved, err = exec.LookPath("espeak")
		if err != nil {
			return nil, fmt.Errorf("espeak not found: %w", err)
		}
	}

	if voice == "" {
		voice = "en"
	}

	return &EspeakTTS{
		binary: resolved,
		voice:  voice,
		rate:   175, // default WPM
	}, nil
}

// Synthesize converts text to PCM audio via espeak.
func (e *EspeakTTS) Synthesize(ctx context.Context, text string) (io.Reader, error) {
	if text == "" {
		return bytes.NewReader(nil), nil
	}

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, e.binary,
		"--stdout",
		"-v", e.voice,
		"-s", fmt.Sprintf("%d", e.rate),
		text,
	)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("espeak: %w (stderr: %s)", err, stderr.String())
	}

	// espeak --stdout outputs WAV data. We return it as-is since the
	// AudioSink (aplay) can handle WAV input.
	return &stdout, nil
}

// Close releases resources.
func (e *EspeakTTS) Close() error { return nil }
