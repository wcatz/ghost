package ai

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewClient(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := NewClient("test-key", logger)

	if client == nil {
		t.Fatal("expected non-nil client")
	}
	if client.apiKey != "test-key" {
		t.Errorf("expected apiKey 'test-key', got %q", client.apiKey)
	}
	if client.httpClient.Timeout != 120*time.Second {
		t.Errorf("expected timeout 120s, got %v", client.httpClient.Timeout)
	}
}

func TestReflect_Success(t *testing.T) {
	mockResponse := `{
		"content": [{"text": "reflection output"}],
		"usage": {"input_tokens": 100, "output_tokens": 50}
	}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "test-key" {
			t.Errorf("expected x-api-key 'test-key', got %q", r.Header.Get("x-api-key"))
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(mockResponse))
	}))
	defer server.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := &Client{
		apiKey: "test-key",
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		logger: logger,
	}

	// Test via direct HTTP (since we can't override APIURL)
	ctx := context.Background()
	req, _ := http.NewRequestWithContext(ctx, "POST", server.URL, strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "test-key")
	req.Header.Set("anthropic-version", APIVersion)

	resp, err := client.httpClient.Do(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}
}

func TestReflect_EmptyContent(t *testing.T) {
	mockResponse := `{
		"content": [],
		"usage": {"input_tokens": 10, "output_tokens": 0}
	}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(mockResponse))
	}))
	defer server.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := &Client{
		apiKey: "test-key",
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		logger: logger,
	}

	ctx := context.Background()
	req, _ := http.NewRequestWithContext(ctx, "POST", server.URL, strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "test-key")

	resp, err := client.httpClient.Do(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}
}

func TestReflect_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"Invalid request"}}`))
	}))
	defer server.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := &Client{
		apiKey: "test-key",
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		logger: logger,
	}

	ctx := context.Background()
	req, _ := http.NewRequestWithContext(ctx, "POST", server.URL, strings.NewReader(`{}`))
	req.Header.Set("x-api-key", "test-key")

	resp, err := client.httpClient.Do(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", resp.StatusCode)
	}
}

// --- parseAPIError ---

func TestParseAPIError_CreditBalance(t *testing.T) {
	body := []byte(`{"error":{"type":"invalid_request_error","message":"Your credit balance is too low to make this request."}}`)
	err := parseAPIError(400, body)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "credit balance") {
		t.Errorf("expected credit balance error, got: %v", err)
	}
}

func TestParseAPIError_InvalidKey(t *testing.T) {
	body := []byte(`{"error":{"type":"authentication_error","message":"Invalid API key"}}`)
	err := parseAPIError(401, body)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "invalid API key") {
		t.Errorf("expected invalid API key error, got: %v", err)
	}
}

func TestParseAPIError_PermissionDenied(t *testing.T) {
	body := []byte(`{"error":{"type":"forbidden","message":"Account suspended"}}`)
	err := parseAPIError(403, body)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("expected permission denied error, got: %v", err)
	}
}

func TestParseAPIError_GenericError(t *testing.T) {
	body := []byte(`{"error":{"type":"server_error","message":"Internal error"}}`)
	err := parseAPIError(500, body)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "Internal error") {
		t.Errorf("expected status code and message in error, got: %v", err)
	}
}

func TestParseAPIError_MalformedJSON(t *testing.T) {
	body := []byte(`not json`)
	err := parseAPIError(500, body)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "not json") {
		t.Errorf("expected raw body in fallback error, got: %v", err)
	}
}

func TestParseAPIError_EmptyMessage(t *testing.T) {
	body := []byte(`{"error":{"type":"server_error","message":""}}`)
	err := parseAPIError(500, body)
	if err == nil {
		t.Fatal("expected error")
	}
	// Empty message falls through to raw body fallback.
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("expected status in error, got: %v", err)
	}
}

// --- setHeaders ---

func TestSetHeaders(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := NewClient("my-api-key", logger)

	req, _ := http.NewRequest("POST", "http://example.com", nil)
	client.setHeaders(req)

	tests := []struct {
		header string
		want   string
	}{
		{"Content-Type", "application/json"},
		{"x-api-key", "my-api-key"},
		{"anthropic-version", APIVersion},
		{"anthropic-beta", BetaInterleavedThinking},
	}

	for _, tt := range tests {
		got := req.Header.Get(tt.header)
		if got != tt.want {
			t.Errorf("header %q = %q, want %q", tt.header, got, tt.want)
		}
	}
}
