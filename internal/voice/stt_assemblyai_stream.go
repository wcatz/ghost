package voice

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/coder/websocket"
)

const (
	assemblyAIStreamTokenURL = "https://streaming.assemblyai.com/v3/token"

	// AssemblyAIStreamWSURL is the WebSocket endpoint for real-time streaming.
	// Exported so the server token proxy can return it as the single source of truth.
	AssemblyAIStreamWSURL = "wss://streaming.assemblyai.com/v3/ws"

	// Audio chunk size: 4096 Int16 samples = 8192 bytes (~256ms at 16kHz).
	streamChunkBytes = 8192
)

// AssemblyAIStreamSTT implements STT using AssemblyAI's V3 real-time
// WebSocket API. Latency is ~200ms vs 3-10s for the batch polling API.
type AssemblyAIStreamSTT struct {
	apiKey string
	client *http.Client
}

// NewAssemblyAIStreamSTT creates a streaming AssemblyAI STT backend.
func NewAssemblyAIStreamSTT(apiKey string) *AssemblyAIStreamSTT {
	return &AssemblyAIStreamSTT{
		apiKey: apiKey,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

// Transcribe streams PCM audio (16-bit, mono, 16kHz) to AssemblyAI via
// WebSocket and returns the final transcript. The audio is sent in chunks
// matching what AssemblyAI expects.
func (s *AssemblyAIStreamSTT) Transcribe(ctx context.Context, audio []byte) (string, error) {
	if len(audio) == 0 {
		return "", nil
	}

	// Step 1: Get a temporary token.
	token, err := s.createToken(ctx)
	if err != nil {
		return "", fmt.Errorf("stream token: %w", err)
	}

	// Step 2: Dial WebSocket with streaming parameters.
	wsURL := fmt.Sprintf("%s?token=%s&sample_rate=16000&encoding=pcm_s16le", AssemblyAIStreamWSURL, token)
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return "", fmt.Errorf("websocket dial: %w", err)
	}
	defer func() { _ = conn.CloseNow() }()

	// Step 3: Send audio in chunks.
	for offset := 0; offset < len(audio); offset += streamChunkBytes {
		end := offset + streamChunkBytes
		if end > len(audio) {
			end = len(audio)
		}
		if err := conn.Write(ctx, websocket.MessageBinary, audio[offset:end]); err != nil {
			return "", fmt.Errorf("send audio: %w", err)
		}
	}

	// Step 4: Send terminate message to signal end of audio.
	terminate, _ := json.Marshal(map[string]string{"type": "Terminate"})
	if err := conn.Write(ctx, websocket.MessageText, terminate); err != nil {
		return "", fmt.Errorf("send terminate: %w", err)
	}

	// Step 5: Read Turn messages until we get end_of_turn or Termination.
	var transcript string
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			// If we already have a transcript, a close after Terminate is expected.
			if transcript != "" {
				break
			}
			return "", fmt.Errorf("read message: %w", err)
		}

		var msg streamMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}

		switch msg.Type {
		case "Turn":
			transcript = msg.Transcript
			if msg.EndOfTurn {
				_ = conn.Close(websocket.StatusNormalClosure, "done")
				return transcript, nil
			}
		case "Termination":
			_ = conn.Close(websocket.StatusNormalClosure, "terminated")
			return transcript, nil
		case "Error":
			return "", fmt.Errorf("assemblyai stream error: %s", string(data))
		}
	}

	return transcript, nil
}

// CreateToken requests a temporary auth token for browser-direct WebSocket
// streaming. Exported so the HTTP server can proxy this for web clients.
func (s *AssemblyAIStreamSTT) CreateToken(ctx context.Context) (string, error) {
	return s.createToken(ctx)
}

func (s *AssemblyAIStreamSTT) createToken(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", assemblyAIStreamTokenURL+"?expires_in_seconds=600", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", s.apiKey)

	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token request %d: %s", resp.StatusCode, body)
	}

	var result struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", err
	}
	if result.Token == "" {
		return "", fmt.Errorf("empty token in response")
	}
	return result.Token, nil
}

// Close is a no-op for the HTTP/WebSocket client.
func (s *AssemblyAIStreamSTT) Close() error { return nil }

// streamMessage represents an AssemblyAI V3 WebSocket message.
type streamMessage struct {
	Type       string `json:"type"`
	Transcript string `json:"transcript,omitempty"`
	EndOfTurn  bool   `json:"end_of_turn,omitempty"`
}
