package calendar

import (
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestParseICal_UTCTimestamp(t *testing.T) {
	got := parseICal("20260320T140000Z")
	want := time.Date(2026, 3, 20, 14, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("parseICal(UTC) = %v, want %v", got, want)
	}
}

func TestParseICal_LocalTimestamp(t *testing.T) {
	got := parseICal("20260320T090000")
	want := time.Date(2026, 3, 20, 9, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("parseICal(local) = %v, want %v", got, want)
	}
}

func TestParseICal_DateOnly(t *testing.T) {
	got := parseICal("20260320")
	want := time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("parseICal(date) = %v, want %v", got, want)
	}
}

func TestParseICal_Invalid(t *testing.T) {
	got := parseICal("not-a-date")
	if !got.IsZero() {
		t.Errorf("expected zero time for invalid input, got %v", got)
	}
}

func TestParseICal_Empty(t *testing.T) {
	got := parseICal("")
	if !got.IsZero() {
		t.Errorf("expected zero time for empty input, got %v", got)
	}
}

func TestNewClient_RequiresHTTPS(t *testing.T) {
	cfg := Config{URL: "http://caldav.example.com/cal", Username: "user", Password: "pass"}
	_, err := NewClient(t.Context(), cfg, nil)
	if err == nil {
		t.Fatal("expected error for HTTP URL")
	}
	if got := err.Error(); !contains(got, "HTTPS") {
		t.Errorf("error should mention HTTPS, got: %s", got)
	}
}

func TestNewClient_EmptyURLAllowed(t *testing.T) {
	// Empty URL should still attempt to create a client (may fail on discovery).
	// It should NOT trigger the HTTPS check.
	cfg := Config{URL: "", Username: "user", Password: "pass"}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	_, err := NewClient(t.Context(), cfg, logger)
	// May error from caldav discovery, but should not mention HTTPS.
	if err != nil && contains(err.Error(), "HTTPS") {
		t.Error("empty URL should not trigger HTTPS check")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && containsSlow(s, substr)
}

func containsSlow(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
