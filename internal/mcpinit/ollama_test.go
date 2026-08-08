package mcpinit

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/config"
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
		checkOllama(&out, &config.Config{Embedding: config.EmbeddingConfig{Enabled: false}}, check)
		if !strings.Contains(out.String(), "  - embedding disabled in config (FTS-only search)") {
			t.Errorf("expected disabled line, got:\n%s", out.String())
		}
		if gotOK {
			t.Error("disabled config must not report a passing check")
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
		checkOllama(&out, cfg, check)
		if ok {
			t.Error("unreachable Ollama must fail the check")
		}
		if want := "Ollama unreachable at " + url + " — embeddings paused"; fail != want {
			t.Errorf("fail text = %q, want %q", fail, want)
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
		checkOllama(&out, cfg, check)
		if !ok {
			t.Error("installed model must pass the check")
		}
		if want := "Ollama model " + model + " installed"; pass != want {
			t.Errorf("pass text = %q, want %q", pass, want)
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
		checkOllama(&out, cfg, check)
		if ok {
			t.Error("missing model must fail the check")
		}
		if want := "Ollama model " + model + " missing — run: ollama pull " + model; fail != want {
			t.Errorf("fail text = %q, want %q", fail, want)
		}
	})
}
