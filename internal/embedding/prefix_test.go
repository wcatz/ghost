package embedding

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// embedRecorder is a fake Ollama that answers the liveness probe and records
// the exact `input` of every /api/embed request.
type embedRecorder struct {
	mu     sync.Mutex
	inputs []string
	dims   int
}

func newEmbedRecorder(dims int) *embedRecorder { return &embedRecorder{dims: dims} }

func (r *embedRecorder) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/":
			w.WriteHeader(http.StatusOK)
		case "/api/embed":
			var body struct {
				Input string `json:"input"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			r.mu.Lock()
			r.inputs = append(r.inputs, body.Input)
			r.mu.Unlock()
			vec := make([]float32, r.dims)
			for i := range vec {
				vec[i] = float32(i+1) / float32(r.dims)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string][][]float32{"embeddings": {vec}})
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (r *embedRecorder) sent() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.inputs...)
}

// TestEmbedAppliesModelTaskPrefixes pins the task-prefix contract: nomic
// variants are embedded with the prefix for their role, any other model is sent
// the bare text, and PrefixedDocument/PrefixedQuery (used by callers with no
// Client) produce byte-for-byte the inputs EmbedDocument/EmbedQuery send.
func TestEmbedAppliesModelTaskPrefixes(t *testing.T) {
	const text = "ollama keeps the WAL in the data directory"
	tests := []struct {
		name, model, wantDoc, wantQuery string
	}{
		{"nomic tagged variant", "nomic-embed-text:v1.5", NomicTaskDocument + text, NomicTaskQuery + text},
		{"nomic untagged", "nomic-embed-text", NomicTaskDocument + text, NomicTaskQuery + text},
		{"no task prefix", "mxbai-embed-large", text, text},
		// The family is the whole name before the tag, not a substring.
		{"nomic as substring", "my-nomic-embed-text-clone:q4", text, text},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := newEmbedRecorder(3)
			c := NewClient(rec.server(t).URL, tc.model, 3)
			if _, err := c.EmbedDocument(context.Background(), text); err != nil {
				t.Fatalf("EmbedDocument: %v", err)
			}
			if _, err := c.EmbedQuery(context.Background(), text); err != nil {
				t.Fatalf("EmbedQuery: %v", err)
			}
			got := rec.sent()
			if len(got) != 2 || got[0] != tc.wantDoc || got[1] != tc.wantQuery {
				t.Fatalf("sent %q, want [%q %q]", got, tc.wantDoc, tc.wantQuery)
			}
			if PrefixedDocument(tc.model, text) != got[0] || PrefixedQuery(tc.model, text) != got[1] {
				t.Errorf("PrefixedDocument/PrefixedQuery diverge from what the client sent for %s", tc.model)
			}
		})
	}
}

// TestVectorIdentityCarriesModelDimensionsAndPrefix: everything that changes
// the vector space has to change the identity, or stale vectors would be kept.
func TestVectorIdentityCarriesModelDimensionsAndPrefix(t *testing.T) {
	const nomic = "nomic-embed-text:v1.5"
	tests := []struct{ got, want string }{
		{VectorIdentity(nomic, 768), "nomic-embed-text:v1.5:768+prefix"},
		{VectorIdentity("mxbai-embed-large", 1024), "mxbai-embed-large:1024"},
		{VectorIdentity(nomic, 0), "nomic-embed-text:v1.5+prefix"},
		{VectorIdentity("registry.ollama.ai/library/"+nomic, 768), "registry.ollama.ai/library/nomic-embed-text:v1.5:768+prefix"},
	}
	for _, tc := range tests {
		if tc.got != tc.want {
			t.Errorf("VectorIdentity = %q, want %q", tc.got, tc.want)
		}
	}
	for name, other := range map[string]string{
		"dimension": VectorIdentity(nomic, 256),
		"model":     VectorIdentity("mxbai-embed-large", 768),
		"tag":       VectorIdentity("nomic-embed-text:v1", 768),
	} {
		if other == VectorIdentity(nomic, 768) {
			t.Errorf("a %s change does not change the identity", name)
		}
	}
	c := NewClient("http://127.0.0.1:1", nomic, 768)
	if got, want := c.Identity(), VectorIdentity(c.Model(), c.Dimensions()); got != want {
		t.Errorf("Client.Identity() = %q, want %q", got, want)
	}
}

func TestModelFamilyAndTaskPrefixes(t *testing.T) {
	tests := []struct{ model, want string }{
		{"nomic-embed-text:v1.5", "nomic-embed-text"},
		{"nomic-embed-text", "nomic-embed-text"},
		{"registry.ollama.ai/library/nomic-embed-text:v1.5", "nomic-embed-text"},
		{"library/nomic-embed-text", "nomic-embed-text"},
		{"mxbai-embed-large:335M", "mxbai-embed-large"},
		{"", ""},
		{":v1", ""},
		{"/nomic-embed-text:v1", "nomic-embed-text"},
	}
	for _, tc := range tests {
		if got := ModelFamily(tc.model); got != tc.want {
			t.Errorf("ModelFamily(%q) = %q, want %q", tc.model, got, tc.want)
		}
	}
	if p := TaskPrefixesFor("registry.ollama.ai/library/nomic-embed-text:v1.5"); p.Document != NomicTaskDocument || p.Query != NomicTaskQuery {
		t.Errorf("qualified nomic name = %+v, want the nomic pair", p)
	}
	if p := TaskPrefixesFor("some/other-model:v2"); p.Document != "" || p.Query != "" {
		t.Errorf("unknown family = %+v, want no prefixes", p)
	}
}

// TestEmbedRejectsWrongDimensions: a vector of the wrong length is an error,
// not a silently stored row.
func TestEmbedRejectsWrongDimensions(t *testing.T) {
	c := NewClient(newEmbedRecorder(3).server(t).URL, "nomic-embed-text:v1.5", 5)
	if _, err := c.EmbedQuery(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "expected 5 dimensions") {
		t.Errorf("EmbedQuery with a dimension mismatch: err = %v, want one naming the expected dimensions", err)
	}
}
