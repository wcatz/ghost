package embedding

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func tagsServer(t *testing.T, models ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		body := `{"models":[`
		for i, m := range models {
			if i > 0 {
				body += ","
			}
			body += `{"name":"` + m + `"}`
		}
		body += `]}`
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestHasModel(t *testing.T) {
	tests := []struct {
		name      string
		model     string
		installed []string
		want      bool
	}{
		{"exact tag match", "nomic-embed-text:v1.5", []string{"nomic-embed-text:v1.5", "llama3:latest"}, true},
		{"untagged model matches latest", "nomic-embed-text", []string{"nomic-embed-text:latest"}, true},
		{"qualified model matches bare installed", "registry.ollama.ai/library/nomic-embed-text:v1.5", []string{"nomic-embed-text:v1.5"}, true},
		{"trailing colon model matches latest", "nomic-embed-text:", []string{"nomic-embed-text:latest"}, true},
		{"qualified model wrong tag stays unmatched", "registry.ollama.ai/library/nomic-embed-text:v1.5", []string{"nomic-embed-text:latest"}, false},
		{"different namespaces do not match", "hf.co/alice/m:q", []string{"hf.co/bob/m:q"}, false},
		{"a namespace is not dropped", "user/x", []string{"x"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := tagsServer(t, tc.installed...)
			c := NewClient(srv.URL, tc.model, 768)
			got, err := c.HasModel(context.Background())
			if err != nil {
				t.Fatalf("HasModel: %v", err)
			}
			if got != tc.want {
				t.Errorf("HasModel(%q vs %v) = %v, want %v", tc.model, tc.installed, got, tc.want)
			}
		})
	}
}

func TestHasModelUnreachable(t *testing.T) {
	c := NewClient("http://127.0.0.1:1", "m", 768)
	if _, err := c.HasModel(context.Background()); err == nil {
		t.Fatal("HasModel against unreachable server: want error, got nil")
	}
}
