package voice

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// ElevenLabsTTS implements TTS using the ElevenLabs REST API.
// Uses the eleven_flash_v2_5 model for low-latency synthesis.
type ElevenLabsTTS struct {
	apiKey  string
	voiceID string
	client  *http.Client
}

// NewElevenLabsTTS creates a new ElevenLabs TTS backend.
func NewElevenLabsTTS(apiKey, voiceID string) *ElevenLabsTTS {
	return &ElevenLabsTTS{
		apiKey:  apiKey,
		voiceID: voiceID,
		client:  &http.Client{Timeout: 30_000_000_000}, // 30s
	}
}

// Synthesize converts text to audio via ElevenLabs REST API.
// Returns an io.Reader streaming MP3 audio data.
func (e *ElevenLabsTTS) Synthesize(ctx context.Context, text string) (io.Reader, error) {
	url := fmt.Sprintf("https://api.elevenlabs.io/v1/text-to-speech/%s", e.voiceID)

	body, _ := json.Marshal(map[string]interface{}{
		"text":     text,
		"model_id": "eleven_flash_v2_5",
		"voice_settings": map[string]float64{
			"stability":        0.5,
			"similarity_boost": 0.75,
		},
	})

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("xi-api-key", e.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "audio/mpeg")

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("elevenlabs request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("elevenlabs %d: %s", resp.StatusCode, errBody)
	}

	// Return the response body directly — caller reads MP3 stream.
	// Note: caller is responsible for closing via the pipeline.
	return resp.Body, nil
}

// Close is a no-op for the REST client.
func (e *ElevenLabsTTS) Close() error { return nil }
