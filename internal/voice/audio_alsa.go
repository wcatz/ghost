package voice

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"sync"
)

// ALSASource captures audio using arecord (ALSA utilities).
// Works on any Linux system with alsa-utils installed.
type ALSASource struct {
	device     string
	sampleRate int

	mu  sync.Mutex
	cmd *exec.Cmd
}

// NewALSASource creates an ALSA audio capture source.
// device is the ALSA device name (e.g., "default", "hw:0,0").
func NewALSASource(device string, sampleRate int) *ALSASource {
	if device == "" {
		device = "default"
	}
	if sampleRate <= 0 {
		sampleRate = 16000
	}
	return &ALSASource{
		device:     device,
		sampleRate: sampleRate,
	}
}

// Start begins recording audio. Returns a channel of PCM frames.
// Each frame is 20ms of 16-bit mono audio at the configured sample rate.
func (a *ALSASource) Start(ctx context.Context) (<-chan []byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.cmd != nil {
		return nil, fmt.Errorf("already recording")
	}

	cmd := exec.CommandContext(ctx, "arecord",
		"-D", a.device,
		"-f", "S16_LE",      // 16-bit signed little-endian
		"-c", "1",            // mono
		"-r", fmt.Sprintf("%d", a.sampleRate),
		"-t", "raw",          // raw PCM output
		"--buffer-size=1024", // small buffer for low latency
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start arecord: %w", err)
	}

	a.cmd = cmd

	// Frame size: 20ms of 16-bit mono audio.
	frameSize := a.sampleRate * 2 * 20 / 1000 // bytes per 20ms frame
	frames := make(chan []byte, 50)

	go func() {
		defer close(frames)
		buf := make([]byte, frameSize)
		for {
			n, err := io.ReadFull(stdout, buf)
			if n > 0 {
				frame := make([]byte, n)
				copy(frame, buf[:n])
				select {
				case frames <- frame:
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	return frames, nil
}

// Stop stops recording.
func (a *ALSASource) Stop() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.cmd != nil && a.cmd.Process != nil {
		_ = a.cmd.Process.Kill()
		_ = a.cmd.Wait()
		a.cmd = nil
	}
	return nil
}

// Close releases resources.
func (a *ALSASource) Close() error {
	return a.Stop()
}

// ALSASink plays audio using aplay (ALSA utilities).
type ALSASink struct {
	device     string
	sampleRate int

	mu  sync.Mutex
	cmd *exec.Cmd
}

// NewALSASink creates an ALSA audio playback sink.
func NewALSASink(device string, sampleRate int) *ALSASink {
	if device == "" {
		device = "default"
	}
	if sampleRate <= 0 {
		sampleRate = 22050 // piper default output rate
	}
	return &ALSASink{
		device:     device,
		sampleRate: sampleRate,
	}
}

// Play streams PCM audio to the output device.
func (a *ALSASink) Play(ctx context.Context, audio io.Reader) error {
	a.mu.Lock()
	cmd := exec.CommandContext(ctx, "aplay",
		"-D", a.device,
		"-f", "S16_LE",
		"-c", "1",
		"-r", fmt.Sprintf("%d", a.sampleRate),
		"-t", "raw",
	)
	cmd.Stdin = audio
	a.cmd = cmd
	a.mu.Unlock()

	err := cmd.Run()

	a.mu.Lock()
	a.cmd = nil
	a.mu.Unlock()

	if err != nil && ctx.Err() != nil {
		return ctx.Err() // context cancelled, not a real error
	}
	return err
}

// Stop stops playback.
func (a *ALSASink) Stop() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.cmd != nil && a.cmd.Process != nil {
		_ = a.cmd.Process.Kill()
		_ = a.cmd.Wait()
		a.cmd = nil
	}
	return nil
}

// Close releases resources.
func (a *ALSASink) Close() error {
	return a.Stop()
}
