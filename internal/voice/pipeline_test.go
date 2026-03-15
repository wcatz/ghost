package voice

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

// --- Mock implementations ---

type mockSTT struct {
	text string
	err  error
}

func (m *mockSTT) Transcribe(_ context.Context, _ []byte) (string, error) {
	return m.text, m.err
}
func (m *mockSTT) Close() error { return nil }

type mockTTS struct {
	audio []byte
	err   error
}

func (m *mockTTS) Synthesize(_ context.Context, _ string) (io.Reader, error) {
	if m.err != nil {
		return nil, m.err
	}
	return bytes.NewReader(m.audio), nil
}
func (m *mockTTS) Close() error { return nil }

type mockSource struct {
	frames [][]byte
}

func (m *mockSource) Start(_ context.Context) (<-chan []byte, error) {
	ch := make(chan []byte, len(m.frames))
	for _, f := range m.frames {
		ch <- f
	}
	close(ch)
	return ch, nil
}
func (m *mockSource) Stop() error  { return nil }
func (m *mockSource) Close() error { return nil }

type mockSink struct {
	played []byte
}

func (m *mockSink) Play(_ context.Context, audio io.Reader) error {
	var err error
	m.played, err = io.ReadAll(audio)
	return err
}
func (m *mockSink) Stop() error  { return nil }
func (m *mockSink) Close() error { return nil }

// --- Tests ---

func TestPipeline_HandlePushToTalk(t *testing.T) {
	// Generate some "speech" frames.
	speechFrame := generateSineFrame(440, 10000, 16000, 20)
	source := &mockSource{frames: [][]byte{speechFrame, speechFrame, speechFrame}}
	stt := &mockSTT{text: "hello ghost"}
	tts := &mockTTS{audio: []byte{1, 2, 3, 4}}
	sink := &mockSink{}

	p := NewPipeline(stt, tts, source, sink, nil, func(_ context.Context, text string) (string, error) {
		return "Hi there!", nil
	}, Config{}, slog.Default())

	transcript, response, err := p.HandlePushToTalk(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if transcript != "hello ghost" {
		t.Errorf("transcript = %q, want %q", transcript, "hello ghost")
	}
	if response != "Hi there!" {
		t.Errorf("response = %q, want %q", response, "Hi there!")
	}
	if len(sink.played) == 0 {
		t.Error("expected TTS audio to be played")
	}
}

func TestPipeline_EmptyAudio(t *testing.T) {
	source := &mockSource{frames: nil} // no frames
	stt := &mockSTT{text: "should not be called"}

	p := NewPipeline(stt, nil, source, nil, nil, func(_ context.Context, text string) (string, error) {
		t.Fatal("respond should not be called on empty audio")
		return "", nil
	}, Config{}, slog.Default())

	transcript, response, err := p.HandlePushToTalk(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if transcript != "" || response != "" {
		t.Errorf("expected empty transcript/response, got %q/%q", transcript, response)
	}
}

func TestPipeline_EmptyTranscript(t *testing.T) {
	speechFrame := generateSineFrame(440, 10000, 16000, 20)
	source := &mockSource{frames: [][]byte{speechFrame}}
	stt := &mockSTT{text: ""} // whisper returned nothing

	p := NewPipeline(stt, nil, source, nil, nil, func(_ context.Context, text string) (string, error) {
		t.Fatal("respond should not be called on empty transcript")
		return "", nil
	}, Config{}, slog.Default())

	transcript, response, err := p.HandlePushToTalk(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if transcript != "" || response != "" {
		t.Errorf("expected empty transcript/response, got %q/%q", transcript, response)
	}
}

func TestPipeline_StateTransitions(t *testing.T) {
	speechFrame := generateSineFrame(440, 10000, 16000, 20)
	source := &mockSource{frames: [][]byte{speechFrame}}
	stt := &mockSTT{text: "test"}
	tts := &mockTTS{audio: []byte{1, 2}}
	sink := &mockSink{}

	var states []State
	p := NewPipeline(stt, tts, source, sink, nil, func(_ context.Context, _ string) (string, error) {
		return "reply", nil
	}, Config{}, slog.Default())
	p.OnStateChange = func(s State) {
		states = append(states, s)
	}

	_, _, err := p.HandlePushToTalk(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Expected: Listening → Thinking → Speaking → Idle
	expected := []State{StateListening, StateThinking, StateSpeaking, StateIdle}
	if len(states) != len(expected) {
		t.Fatalf("state transitions: got %v, want %v", states, expected)
	}
	for i, s := range states {
		if s != expected[i] {
			t.Errorf("state[%d] = %v, want %v", i, s, expected[i])
		}
	}
}

func TestPipeline_NoTTS(t *testing.T) {
	speechFrame := generateSineFrame(440, 10000, 16000, 20)
	source := &mockSource{frames: [][]byte{speechFrame}}
	stt := &mockSTT{text: "hello"}

	p := NewPipeline(stt, nil, source, nil, nil, func(_ context.Context, _ string) (string, error) {
		return "response without speech", nil
	}, Config{}, slog.Default())

	transcript, response, err := p.HandlePushToTalk(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if transcript != "hello" {
		t.Errorf("transcript = %q, want %q", transcript, "hello")
	}
	if response != "response without speech" {
		t.Errorf("response = %q, want %q", response, "response without speech")
	}
}

func TestCollectUntilSilence_WithVAD(t *testing.T) {
	vad := NewEnergyVAD(800)
	p := &Pipeline{
		vad:       vad,
		silenceMs: 100, // short timeout for test
	}

	speech := generateSineFrame(440, 10000, 16000, 20)
	silence := generateSilence(16000, 20)

	frames := make(chan []byte, 20)
	// Send speech frames followed by enough silence to trigger cutoff.
	frames <- speech
	frames <- speech
	for i := 0; i < 10; i++ {
		frames <- silence
	}
	close(frames)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	audio, err := p.collectUntilSilence(ctx, frames)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(audio) == 0 {
		t.Error("expected non-empty audio")
	}
}

func TestState_String(t *testing.T) {
	tests := []struct {
		state State
		want  string
	}{
		{StateIdle, "idle"},
		{StateListening, "listening"},
		{StateThinking, "thinking"},
		{StateSpeaking, "speaking"},
		{State(99), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.state.String(); got != tt.want {
			t.Errorf("State(%d).String() = %q, want %q", tt.state, got, tt.want)
		}
	}
}
