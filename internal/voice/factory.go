package voice

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
)

// Options holds the configuration for building a voice pipeline.
type Options struct {
	STTBackend  string  // "whisper" or "subprocess" (both use whisper CLI)
	STTModel    string  // model name or path
	TTSBackend  string  // "piper", "espeak", or "none"
	TTSModel    string  // model path for piper
	TTSVoice    string  // voice name for espeak
	TTSRate     float64 // speech rate multiplier
	SilenceMs   int     // ms of silence before ending recording
	SampleRate  int     // audio sample rate (default 16000)
	InputDevice string  // ALSA device name
	Logger      *slog.Logger
}

// New builds a complete voice pipeline from the given options.
// Components that can't be initialized (missing binaries) will be nil,
// and the pipeline will degrade gracefully.
func New(opts Options, respond ResponseFunc) (*Pipeline, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	// Build STT.
	stt, err := buildSTT(opts)
	if err != nil {
		return nil, fmt.Errorf("stt: %w", err)
	}

	// Build TTS (optional — pipeline works without it).
	tts, ttsErr := buildTTS(opts)
	if ttsErr != nil {
		opts.Logger.Warn("tts unavailable, responses will be text-only", "error", ttsErr)
	}

	// Build audio source.
	source := NewALSASource(opts.InputDevice, opts.SampleRate)

	// Build audio sink (use piper's default 22050 Hz for playback).
	var sink AudioSink
	if tts != nil {
		sink = NewALSASink(opts.InputDevice, 22050)
	}

	// Build VAD.
	vad := NewEnergyVAD(800) // default threshold

	cfg := Config{
		SilenceMs:  opts.SilenceMs,
		SampleRate: opts.SampleRate,
	}

	return NewPipeline(stt, tts, source, sink, vad, respond, cfg, opts.Logger), nil
}

// buildSTT creates the STT backend.
func buildSTT(opts Options) (STT, error) {
	// Try common whisper binary names.
	binaries := []string{"whisper-cpp", "whisper", "main"}

	var binary string
	for _, b := range binaries {
		if _, err := lookPath(b); err == nil {
			binary = b
			break
		}
	}
	if binary == "" {
		return nil, fmt.Errorf("whisper binary not found (tried: %v)", binaries)
	}

	model := resolveWhisperModel(opts.STTModel)
	return NewWhisperSTT(binary, model)
}

// buildTTS creates the TTS backend.
func buildTTS(opts Options) (TTS, error) {
	switch opts.TTSBackend {
	case "none", "":
		return nil, fmt.Errorf("tts disabled")
	case "espeak":
		return NewEspeakTTS(opts.TTSVoice)
	case "piper":
		if opts.TTSModel == "" {
			return nil, fmt.Errorf("piper requires tts_model path")
		}
		tts, err := NewPiperTTS("piper", opts.TTSModel)
		if err != nil {
			return nil, err
		}
		tts.SetRate(opts.TTSRate)
		return tts, nil
	default:
		return nil, fmt.Errorf("unknown tts backend: %s", opts.TTSBackend)
	}
}

// resolveWhisperModel resolves a model name to a file path.
// If the model is already an absolute path, it's returned as-is.
// Otherwise, common locations are checked.
func resolveWhisperModel(model string) string {
	if model == "" {
		model = "base"
	}

	// Already an absolute path.
	if filepath.IsAbs(model) {
		return model
	}

	// Check common model locations.
	candidates := []string{
		filepath.Join(homeDir(), ".local", "share", "whisper", "ggml-"+model+".bin"),
		filepath.Join("/usr/share/whisper/ggml-" + model + ".bin"),
		filepath.Join("/usr/local/share/whisper/ggml-" + model + ".bin"),
		"ggml-" + model + ".bin", // current directory
	}

	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}

	// Return the raw name — NewWhisperSTT will report the error.
	return model
}

func homeDir() string {
	h, _ := os.UserHomeDir()
	return h
}

// lookPath is a testable wrapper around exec.LookPath.
var lookPath = lookPathImpl

func lookPathImpl(binary string) (string, error) {
	return exec.LookPath(binary)
}
