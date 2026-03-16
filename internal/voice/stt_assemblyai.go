package voice

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	assemblyAIUploadURL     = "https://api.assemblyai.com/v2/upload"
	assemblyAITranscriptURL = "https://api.assemblyai.com/v2/transcript"
)

// AssemblyAISTT implements STT using the AssemblyAI transcription API.
// Uploads audio, submits for transcription, polls until complete.
type AssemblyAISTT struct {
	apiKey string
	client *http.Client
}

// NewAssemblyAISTT creates a new AssemblyAI STT backend.
func NewAssemblyAISTT(apiKey string) *AssemblyAISTT {
	return &AssemblyAISTT{
		apiKey: apiKey,
		client: &http.Client{Timeout: 120 * time.Second},
	}
}

// Transcribe converts PCM audio to text via AssemblyAI.
// Audio should be WAV format (16-bit, mono, 16kHz).
func (a *AssemblyAISTT) Transcribe(ctx context.Context, audio []byte) (string, error) {
	// Step 1: Upload audio.
	audioURL, err := a.upload(ctx, audio)
	if err != nil {
		return "", fmt.Errorf("upload: %w", err)
	}

	// Step 2: Submit transcription.
	transcriptID, err := a.submit(ctx, audioURL)
	if err != nil {
		return "", fmt.Errorf("submit: %w", err)
	}

	// Step 3: Poll for result.
	text, err := a.poll(ctx, transcriptID)
	if err != nil {
		return "", fmt.Errorf("poll: %w", err)
	}

	return text, nil
}

func (a *AssemblyAISTT) upload(ctx context.Context, audio []byte) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", assemblyAIUploadURL, bytes.NewReader(audio))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", a.apiKey)

	resp, err := a.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("upload %d: %s", resp.StatusCode, body)
	}

	var result struct {
		UploadURL string `json:"upload_url"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", err
	}
	return result.UploadURL, nil
}

func (a *AssemblyAISTT) submit(ctx context.Context, audioURL string) (string, error) {
	payload, _ := json.Marshal(map[string]interface{}{
		"audio_url":     audioURL,
		"speech_models": []string{"universal-3-pro"},
	})

	req, err := http.NewRequestWithContext(ctx, "POST", assemblyAITranscriptURL, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", a.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("submit %d: %s", resp.StatusCode, body)
	}

	var result struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", err
	}
	return result.ID, nil
}

func (a *AssemblyAISTT) poll(ctx context.Context, transcriptID string) (string, error) {
	url := assemblyAITranscriptURL + "/" + transcriptID
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
			req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
			if err != nil {
				return "", err
			}
			req.Header.Set("Authorization", a.apiKey)

			resp, err := a.client.Do(req)
			if err != nil {
				return "", err
			}
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				return "", fmt.Errorf("poll %d: %s", resp.StatusCode, body)
			}

			var result struct {
				Status string `json:"status"`
				Text   string `json:"text"`
				Error  string `json:"error"`
			}
			if err := json.Unmarshal(body, &result); err != nil {
				return "", err
			}

			switch result.Status {
			case "completed":
				return result.Text, nil
			case "error":
				return "", fmt.Errorf("transcription error: %s", result.Error)
			case "queued", "processing":
				continue
			default:
				return "", fmt.Errorf("unknown status: %s", result.Status)
			}
		}
	}
}

// Close is a no-op for the HTTP client.
func (a *AssemblyAISTT) Close() error { return nil }
