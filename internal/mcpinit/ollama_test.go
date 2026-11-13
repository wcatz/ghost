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

func TestFormatDownDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0m"},
		{30 * time.Second, "0m"}, // sub-minute truncates away
		{3 * time.Minute, "3m"},
		{59*time.Minute + 59*time.Second, "59m"},
		{time.Hour, "1h 0m"},
		{24 * time.Hour, "1d 0h"},
		{25*time.Hour + 30*time.Minute, "1d 1h"}, // day boundary drops minutes
	}
	for _, c := range cases {
		if got := formatDownDuration(c.d); got != c.want {
			t.Errorf("formatDownDuration(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

// TestReportOllamaDownDuration_IgnoresAbsentOrMalformed: a missing or
// unparseable down-marker prints nothing — it is best-effort bookkeeping.
func TestReportOllamaDownDuration_IgnoresAbsentOrMalformed(t *testing.T) {
	for _, content := range []string{"", "not a timestamp"} {
		dataDir := t.TempDir()
		if content != "" {
			if err := os.WriteFile(filepath.Join(dataDir, embedding.OllamaDownMarkerFilename), []byte(content), 0o600); err != nil {
				t.Fatalf("write marker: %v", err)
			}
		}
		var out bytes.Buffer
		reportOllamaDownDuration(&out, dataDir)
		if out.Len() != 0 {
			t.Errorf("marker %q: expected no output, got:\n%s", content, out.String())
		}
	}
}
