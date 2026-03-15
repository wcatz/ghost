package voice

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// State represents the voice pipeline state.
type State int

const (
	StateIdle      State = iota // Waiting for push-to-talk
	StateListening              // Recording audio
	StateThinking               // Transcribing + waiting for LLM
	StateSpeaking               // Playing TTS output
)

func (s State) String() string {
	switch s {
	case StateIdle:
		return "idle"
	case StateListening:
		return "listening"
	case StateThinking:
		return "thinking"
	case StateSpeaking:
		return "speaking"
	default:
		return "unknown"
	}
}

// ResponseFunc is called with the transcribed text and should return
// the assistant's response. This is where Ghost API integration happens.
type ResponseFunc func(ctx context.Context, text string) (string, error)

// Pipeline orchestrates the voice interaction loop.
type Pipeline struct {
	stt    STT
	tts    TTS
	source AudioSource
	sink   AudioSink
	vad    VAD
	respond ResponseFunc
	logger *slog.Logger

	silenceMs  int
	sampleRate int

	mu    sync.Mutex
	state State

	// StateChange is called whenever the pipeline state changes.
	// Safe to be nil.
	OnStateChange func(State)
}

// Config holds pipeline configuration.
type Config struct {
	SilenceMs  int // ms of silence before ending recording
	SampleRate int
}

// NewPipeline creates a voice pipeline with the given components.
func NewPipeline(
	stt STT,
	tts TTS,
	source AudioSource,
	sink AudioSink,
	vad VAD,
	respond ResponseFunc,
	cfg Config,
	logger *slog.Logger,
) *Pipeline {
	if cfg.SilenceMs <= 0 {
		cfg.SilenceMs = 800
	}
	if cfg.SampleRate <= 0 {
		cfg.SampleRate = 16000
	}
	return &Pipeline{
		stt:        stt,
		tts:        tts,
		source:     source,
		sink:       sink,
		vad:        vad,
		respond:    respond,
		logger:     logger,
		silenceMs:  cfg.SilenceMs,
		sampleRate: cfg.SampleRate,
		state:      StateIdle,
	}
}

// State returns the current pipeline state.
func (p *Pipeline) State() State {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state
}

func (p *Pipeline) setState(s State) {
	p.mu.Lock()
	p.state = s
	p.mu.Unlock()
	if p.OnStateChange != nil {
		p.OnStateChange(s)
	}
}

// HandlePushToTalk executes one push-to-talk cycle:
// listen → detect silence → transcribe → get response → speak.
// Returns the transcribed text and response, or an error.
func (p *Pipeline) HandlePushToTalk(ctx context.Context) (transcript, response string, err error) {
	// 1. Start recording.
	p.setState(StateListening)
	defer p.setState(StateIdle)

	frames, err := p.source.Start(ctx)
	if err != nil {
		return "", "", fmt.Errorf("start audio capture: %w", err)
	}

	// 2. Collect audio until silence detected.
	audio, err := p.collectUntilSilence(ctx, frames)
	if err != nil {
		p.source.Stop()
		return "", "", fmt.Errorf("collect audio: %w", err)
	}
	p.source.Stop()

	if len(audio) == 0 {
		return "", "", nil // No audio captured.
	}

	p.logger.Info("audio captured", "bytes", len(audio),
		"duration_ms", len(audio)*1000/(p.sampleRate*2)) // 16-bit = 2 bytes/sample

	// 3. Transcribe.
	p.setState(StateThinking)

	transcript, err = p.stt.Transcribe(ctx, audio)
	if err != nil {
		return "", "", fmt.Errorf("transcribe: %w", err)
	}

	if transcript == "" {
		p.logger.Info("empty transcript, skipping")
		return "", "", nil
	}

	p.logger.Info("transcribed", "text", transcript)

	// 4. Get response from Ghost.
	response, err = p.respond(ctx, transcript)
	if err != nil {
		return transcript, "", fmt.Errorf("respond: %w", err)
	}

	// 5. Speak response.
	if response != "" && p.tts != nil {
		p.setState(StateSpeaking)

		audioReader, err := p.tts.Synthesize(ctx, response)
		if err != nil {
			p.logger.Error("tts synthesize", "error", err)
		} else if p.sink != nil {
			if err := p.sink.Play(ctx, audioReader); err != nil {
				p.logger.Error("audio playback", "error", err)
			}
		}
	}

	return transcript, response, nil
}

// collectUntilSilence accumulates audio frames until VAD detects
// sustained silence (silenceMs worth of non-speech frames).
func (p *Pipeline) collectUntilSilence(ctx context.Context, frames <-chan []byte) ([]byte, error) {
	var buf bytes.Buffer
	silenceStart := time.Time{}
	silenceThreshold := time.Duration(p.silenceMs) * time.Millisecond
	hasSpeech := false

	if p.vad != nil {
		p.vad.Reset()
	}

	for {
		select {
		case <-ctx.Done():
			return buf.Bytes(), ctx.Err()

		case frame, ok := <-frames:
			if !ok {
				return buf.Bytes(), nil
			}

			buf.Write(frame)

			// If no VAD, collect until channel closes (manual stop).
			if p.vad == nil {
				continue
			}

			if p.vad.IsSpeech(frame) {
				hasSpeech = true
				silenceStart = time.Time{}
			} else if hasSpeech {
				// Speech was detected before, now silence.
				if silenceStart.IsZero() {
					silenceStart = time.Now()
				} else if time.Since(silenceStart) >= silenceThreshold {
					return buf.Bytes(), nil
				}
			}
		}
	}
}

// Close releases all pipeline resources.
func (p *Pipeline) Close() error {
	var errs []error
	if p.source != nil {
		if err := p.source.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if p.sink != nil {
		if err := p.sink.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if p.stt != nil {
		if err := p.stt.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if p.tts != nil {
		if err := p.tts.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if p.vad != nil {
		if err := p.vad.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("close errors: %v", errs)
	}
	return nil
}
