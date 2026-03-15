// Package voice implements a push-to-talk voice pipeline for Ghost.
// Architecture: AudioSource → VAD → STT → (Ghost API) → TTS → AudioSink
//
// All components are behind interfaces so backends can be swapped
// (whisper.cpp vs subprocess, piper vs espeak, malgo vs portaudio).
package voice

import (
	"context"
	"io"
)

// STT transcribes audio to text.
type STT interface {
	// Transcribe converts PCM audio (16-bit, mono, 16kHz) to text.
	Transcribe(ctx context.Context, audio []byte) (string, error)
	Close() error
}

// TTS synthesizes text to audio.
type TTS interface {
	// Synthesize converts text to PCM audio (16-bit, mono).
	// Returns a reader that streams the audio data.
	Synthesize(ctx context.Context, text string) (io.Reader, error)
	Close() error
}

// AudioSource captures audio from a microphone.
type AudioSource interface {
	// Start begins capturing audio. Captured frames are written to the channel.
	// Each frame is a chunk of PCM audio (16-bit, mono, 16kHz).
	Start(ctx context.Context) (<-chan []byte, error)
	Stop() error
	Close() error
}

// AudioSink plays audio through speakers.
type AudioSink interface {
	// Play streams PCM audio (16-bit, mono) to the output device.
	Play(ctx context.Context, audio io.Reader) error
	Stop() error
	Close() error
}

// VAD detects voice activity in an audio stream.
type VAD interface {
	// IsSpeech returns true if the audio frame contains speech.
	IsSpeech(frame []byte) bool
	// Reset clears internal state for a new utterance.
	Reset()
	Close() error
}
