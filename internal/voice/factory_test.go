package voice

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestBuildSTT_AssemblyAI(t *testing.T) {
	opts := Options{
		STTBackend:       "assemblyai",
		AssemblyAIAPIKey: "test-key-12345",
	}
	stt, err := BuildSTT(opts)
	if err != nil {
		t.Fatalf("BuildSTT(assemblyai): %v", err)
	}
	if stt == nil {
		t.Fatal("expected non-nil STT for assemblyai")
	}
}

func TestBuildSTT_AssemblyAI_MissingKey(t *testing.T) {
	opts := Options{STTBackend: "assemblyai"}
	_, err := BuildSTT(opts)
	if err == nil {
		t.Error("expected error for assemblyai without API key")
	}
}

func TestBuildSTT_AssemblyAIBatch(t *testing.T) {
	opts := Options{
		STTBackend:       "assemblyai-batch",
		AssemblyAIAPIKey: "test-key-batch",
	}
	stt, err := BuildSTT(opts)
	if err != nil {
		t.Fatalf("BuildSTT(assemblyai-batch): %v", err)
	}
	if stt == nil {
		t.Fatal("expected non-nil STT for assemblyai-batch")
	}
}

func TestBuildSTT_AssemblyAIBatch_MissingKey(t *testing.T) {
	opts := Options{STTBackend: "assemblyai-batch"}
	_, err := BuildSTT(opts)
	if err == nil {
		t.Error("expected error for assemblyai-batch without API key")
	}
}

func TestBuildSTT_WhisperNotFound(t *testing.T) {
	// Override lookPath to always fail.
	orig := lookPath
	lookPath = func(binary string) (string, error) {
		return "", fmt.Errorf("not found: %s", binary)
	}
	t.Cleanup(func() { lookPath = orig })

	opts := Options{STTBackend: "whisper"}
	_, err := BuildSTT(opts)
	if err == nil {
		t.Error("expected error when whisper binary not found")
	}
}

func TestBuildTTS_None(t *testing.T) {
	opts := Options{TTSBackend: "none"}
	tts, err := buildTTS(opts)
	if err == nil {
		t.Error("expected error for tts_backend=none")
	}
	if tts != nil {
		t.Error("expected nil TTS")
	}
}

func TestBuildTTS_Empty(t *testing.T) {
	opts := Options{TTSBackend: ""}
	tts, err := buildTTS(opts)
	if err == nil {
		t.Error("expected error for empty tts_backend")
	}
	if tts != nil {
		t.Error("expected nil TTS")
	}
}

func TestBuildTTS_PiperMissingModel(t *testing.T) {
	opts := Options{TTSBackend: "piper", TTSModel: ""}
	_, err := buildTTS(opts)
	if err == nil {
		t.Error("expected error for piper without model path")
	}
}

func TestBuildTTS_ElevenLabs_MissingKey(t *testing.T) {
	opts := Options{TTSBackend: "elevenlabs"}
	_, err := buildTTS(opts)
	if err == nil {
		t.Error("expected error for elevenlabs without API key")
	}
}

func TestBuildTTS_ElevenLabs_MissingVoice(t *testing.T) {
	opts := Options{TTSBackend: "elevenlabs", ElevenLabsAPIKey: "key"}
	_, err := buildTTS(opts)
	if err == nil {
		t.Error("expected error for elevenlabs without voice ID")
	}
}

func TestBuildTTS_ElevenLabs_Success(t *testing.T) {
	opts := Options{
		TTSBackend:        "elevenlabs",
		ElevenLabsAPIKey:  "key",
		ElevenLabsVoiceID: "voice123",
	}
	tts, err := buildTTS(opts)
	if err != nil {
		t.Fatalf("buildTTS(elevenlabs): %v", err)
	}
	if tts == nil {
		t.Fatal("expected non-nil TTS")
	}
}

func TestBuildTTS_ElevenLabs_FallbackToTTSVoice(t *testing.T) {
	opts := Options{
		TTSBackend:       "elevenlabs",
		ElevenLabsAPIKey: "key",
		TTSVoice:         "fallback-voice",
	}
	tts, err := buildTTS(opts)
	if err != nil {
		t.Fatalf("buildTTS(elevenlabs via TTSVoice): %v", err)
	}
	if tts == nil {
		t.Fatal("expected non-nil TTS")
	}
}

func TestBuildTTS_UnknownBackend(t *testing.T) {
	opts := Options{TTSBackend: "novelai"}
	_, err := buildTTS(opts)
	if err == nil {
		t.Error("expected error for unknown backend")
	}
}

func TestResolveWhisperModel_AbsolutePath(t *testing.T) {
	got := resolveWhisperModel("/usr/share/whisper/ggml-large.bin")
	if got != "/usr/share/whisper/ggml-large.bin" {
		t.Errorf("expected absolute path unchanged, got %q", got)
	}
}

func TestResolveWhisperModel_EmptyDefault(t *testing.T) {
	// Empty model defaults to "base"; since no model file exists in temp,
	// it should return "base" as fallback.
	got := resolveWhisperModel("")
	// The function tries to find the model file. If not found, returns "base".
	if got != "base" {
		// It could also be a path if ~/.local/share/whisper/ggml-base.bin exists.
		// Just ensure it's not empty.
		if got == "" {
			t.Error("expected non-empty result")
		}
	}
}

func TestResolveWhisperModel_FoundInLocalShare(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a fake model file in the expected location.
	modelDir := filepath.Join(tmpDir, ".local", "share", "whisper")
	if err := os.MkdirAll(modelDir, 0o700); err != nil {
		t.Fatal(err)
	}
	modelPath := filepath.Join(modelDir, "ggml-tiny.bin")
	if err := os.WriteFile(modelPath, []byte("fake-model"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Override HOME so resolveWhisperModel finds it.
	t.Setenv("HOME", tmpDir)

	got := resolveWhisperModel("tiny")
	if got != modelPath {
		t.Errorf("expected %q, got %q", modelPath, got)
	}
}
