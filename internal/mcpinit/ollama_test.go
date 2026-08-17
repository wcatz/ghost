package mcpinit

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/embedding"
)

// ollamaStub returns an httptest server stubbing Ollama's root and /api/tags
// endpoints, mirroring the fake in TestStatusOpencode_EmptyStoreHealthy.
func ollamaStub(models ...string) *httptest.Server {
	var sb strings.Builder
	sb.WriteString(`{"models":[`)
	for i, m := range models {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `{"name":%q}`, m)
	}
	sb.WriteString(`]}`)
	tags := sb.String()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.WriteHeader(http.StatusOK)
		case "/api/tags":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(tags))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// TestCheckOllama exercises the shared Ollama health check: a disabled config
// prints the informational line with no check result, an unreachable server
// fails with the pinned fail text, and model presence passes/fails with the
// pinned message strings.
func TestCheckOllama(t *testing.T) {
	model := "nomic-embed-text:v1.5"

	t.Run("disabled", func(t *testing.T) {
		var out bytes.Buffer
		var gotOK bool
		check := func(ok bool, pass, fail string) { gotOK = ok }
		alive := checkOllama(&out, &config.Config{Embedding: config.EmbeddingConfig{Enabled: false}}, check)
		if !strings.Contains(out.String(), "  - embedding disabled in config (FTS-only search)") {
			t.Errorf("expected disabled line, got:\n%s", out.String())
		}
		if gotOK {
			t.Error("disabled config must not report a passing check")
		}
		if alive {
			t.Error("checkOllama returned alive=true for a disabled config, want false (never probed)")
		}
	})

	t.Run("unreachable", func(t *testing.T) {
		srv := ollamaStub()
		url := srv.URL
		srv.Close()
		var out bytes.Buffer
		var ok bool
		var fail string
		check := func(b bool, p, f string) { ok, fail = b, f }
		cfg := &config.Config{Embedding: config.EmbeddingConfig{Enabled: true, OllamaURL: url, Model: model, Dimensions: 768}}
		alive := checkOllama(&out, cfg, check)
		if ok {
			t.Error("unreachable Ollama must fail the check")
		}
		if want := "Ollama unreachable at " + url + " — embeddings paused"; fail != want {
			t.Errorf("fail text = %q, want %q", fail, want)
		}
		if alive {
			t.Error("checkOllama returned alive=true for an unreachable server, want false")
		}
	})

	t.Run("model present", func(t *testing.T) {
		srv := ollamaStub(model)
		defer srv.Close()
		var out bytes.Buffer
		var ok bool
		var pass string
		check := func(b bool, p, f string) { ok, pass = b, p }
		cfg := &config.Config{Embedding: config.EmbeddingConfig{Enabled: true, OllamaURL: srv.URL, Model: model, Dimensions: 768}}
		alive := checkOllama(&out, cfg, check)
		if !ok {
			t.Error("installed model must pass the check")
		}
		if want := "Ollama model " + model + " installed"; pass != want {
			t.Errorf("pass text = %q, want %q", pass, want)
		}
		if !alive {
			t.Error("checkOllama returned alive=false for a reachable server, want true")
		}
	})

	t.Run("model missing", func(t *testing.T) {
		srv := ollamaStub("some-other-model")
		defer srv.Close()
		var out bytes.Buffer
		var ok bool
		var fail string
		check := func(b bool, p, f string) { ok, fail = b, f }
		cfg := &config.Config{Embedding: config.EmbeddingConfig{Enabled: true, OllamaURL: srv.URL, Model: model, Dimensions: 768}}
		alive := checkOllama(&out, cfg, check)
		if ok {
			t.Error("missing model must fail the check")
		}
		if want := "Ollama model " + model + " missing — run: ollama pull " + model; fail != want {
			t.Errorf("fail text = %q, want %q", fail, want)
		}
		if !alive {
			t.Error("checkOllama returned alive=false for a reachable server with a missing model, want true (Ollama itself answered)")
		}
	})
}

// TestFormatDownDuration pins the rendering of an outage duration: minute
// precision under an hour, hour+minute precision under a day, and day+hour
// precision (minutes dropped) once the outage spans a full day — matching the
// "HH:MM" granularity the marker timestamp itself is displayed at.
func TestFormatDownDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0m"},
		{30 * time.Second, "0m"}, // sub-minute truncates away
		{3 * time.Minute, "3m"},
		{59 * time.Minute, "59m"},
		{59*time.Minute + 59*time.Second, "59m"},
		{time.Hour, "1h 0m"},
		{2*time.Hour + 3*time.Minute, "2h 3m"},
		{23*time.Hour + 59*time.Minute, "23h 59m"},
		{24 * time.Hour, "1d 0h"},
		{25*time.Hour + 30*time.Minute, "1d 1h"}, // day boundary drops minutes
		{76 * time.Hour, "3d 4h"},
	}
	for _, c := range cases {
		if got := formatDownDuration(c.d); got != c.want {
			t.Errorf("formatDownDuration(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

// TestReportOllamaDownDuration_Absent verifies that a data directory with no
// down-marker file produces no output at all — a healthy or never-down
// Ollama must not print anything extra.
func TestReportOllamaDownDuration_Absent(t *testing.T) {
	var out bytes.Buffer
	reportOllamaDownDuration(&out, t.TempDir())
	if out.Len() != 0 {
		t.Errorf("expected no output for an absent marker, got:\n%s", out.String())
	}
}

// TestReportOllamaDownDuration_Malformed verifies that unparseable marker
// content is ignored rather than panicking or printing garbage — the marker
// file is best-effort bookkeeping, not a trusted format.
func TestReportOllamaDownDuration_Malformed(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, embedding.OllamaDownMarkerFilename), []byte("not a timestamp"), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	var out bytes.Buffer
	reportOllamaDownDuration(&out, dataDir)
	if out.Len() != 0 {
		t.Errorf("expected no output for a malformed marker, got:\n%s", out.String())
	}
}

// TestReportOllamaDownDuration_Present verifies that a valid marker renders
// both the down-since timestamp and a compact duration.
func TestReportOllamaDownDuration_Present(t *testing.T) {
	dataDir := t.TempDir()
	since := time.Now().Add(-(2*time.Hour + 3*time.Minute))
	marker := since.UTC().Format(time.RFC3339)
	if err := os.WriteFile(filepath.Join(dataDir, embedding.OllamaDownMarkerFilename), []byte(marker), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	var out bytes.Buffer
	reportOllamaDownDuration(&out, dataDir)

	want := fmt.Sprintf("Ollama down since %s (2h 3m)", since.UTC().Format("2006-01-02 15:04 UTC"))
	if !strings.Contains(out.String(), want) {
		t.Errorf("expected output to contain %q, got:\n%s", want, out.String())
	}
}
