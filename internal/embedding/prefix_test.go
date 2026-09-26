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

// embedRecorder is a fake Ollama: it answers the liveness probe and records the
// exact `input` string of every /api/embed request, so a test can assert on
// what was actually sent to the model rather than on how it was assembled.
type embedRecorder struct {
	mu      sync.Mutex
	inputs  []string
	dims    int
	embedOK bool
}

func newEmbedRecorder(dims int) *embedRecorder {
	return &embedRecorder{dims: dims, embedOK: true}
}

func (r *embedRecorder) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/":
			w.WriteHeader(http.StatusOK)
		case "/api/embed":
			var body struct {
				Model string `json:"model"`
				Input string `json:"input"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			r.mu.Lock()
			r.inputs = append(r.inputs, body.Input)
			ok := r.embedOK
			r.mu.Unlock()
			if !ok {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
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
// variants are embedded with the task prefix for their role, and a model that
// requires none is sent the bare text.
func TestEmbedAppliesModelTaskPrefixes(t *testing.T) {
	const text = "ollama keeps the WAL in the data directory"
	tests := []struct {
		name      string
		model     string
		wantDoc   string
		wantQuery string
	}{
		{
			name:      "nomic tagged variant",
			model:     "nomic-embed-text:v1.5",
			wantDoc:   NomicTaskDocument + text,
			wantQuery: NomicTaskQuery + text,
		},
		{
			name:      "nomic untagged resolves to latest",
			model:     "nomic-embed-text",
			wantDoc:   NomicTaskDocument + text,
			wantQuery: NomicTaskQuery + text,
		},
		{
			name:      "model with no task prefix is untouched",
			model:     "mxbai-embed-large",
			wantDoc:   text,
			wantQuery: text,
		},
		{
			// A different model that merely shares the suffix must not inherit
			// nomic's prefixes: the family is the whole name before the tag.
			name:      "other model containing nomic as substring",
			model:     "my-nomic-embed-text-clone:q4",
			wantDoc:   text,
			wantQuery: text,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := newEmbedRecorder(3)
			c := NewClient(rec.server(t).URL, tc.model, 3)
			ctx := context.Background()

			if _, err := c.EmbedDocument(ctx, text); err != nil {
				t.Fatalf("EmbedDocument: %v", err)
			}
			if _, err := c.EmbedQuery(ctx, text); err != nil {
				t.Fatalf("EmbedQuery: %v", err)
			}

			got := rec.sent()
			if len(got) != 2 {
				t.Fatalf("ollama saw %d embed calls, want 2: %v", len(got), got)
			}
			if got[0] != tc.wantDoc {
				t.Errorf("document input = %q, want %q", got[0], tc.wantDoc)
			}
			if got[1] != tc.wantQuery {
				t.Errorf("query input = %q, want %q", got[1], tc.wantQuery)
			}
		})
	}
}

// TestEmbedQueryAndDocumentAreNotInterchangeable states the reason the two
// entry points exist: the two roles are embedded differently, so a caller that
// picks the wrong one silently poisons the space it writes into.
func TestEmbedQueryAndDocumentAreNotInterchangeable(t *testing.T) {
	rec := newEmbedRecorder(2)
	c := NewClient(rec.server(t).URL, "nomic-embed-text:v1.5", 2)

	if _, err := c.EmbedDocument(context.Background(), "stored memory"); err != nil {
		t.Fatalf("EmbedDocument: %v", err)
	}
	if _, err := c.EmbedQuery(context.Background(), "stored memory"); err != nil {
		t.Fatalf("EmbedQuery: %v", err)
	}
	got := rec.sent()
	if got[0] == got[1] {
		t.Fatalf("document and query inputs are identical (%q): the task prefix is not reaching the model", got[0])
	}
}

// TestVectorIdentityCarriesModelDimensionsAndPrefix: the identity is what
// decides whether a stored vector can be compared with a query vector, so
// everything that changes the space has to change it. Prefixing is the reason
// the nomic identity moves even when the model and dimensions do not.
func TestVectorIdentityCarriesModelDimensionsAndPrefix(t *testing.T) {
	const (
		nomic = "nomic-embed-text:v1.5"
		other = "mxbai-embed-large"
	)
	tests := []struct {
		name string
		want string
		got  string
	}{
		{
			name: "nomic identity records the task prefix",
			got:  VectorIdentity(nomic, 768),
			want: "nomic-embed-text:v1.5:768+prefix",
		},
		{
			name: "prefix-free family records no prefix marker",
			got:  VectorIdentity(other, 1024),
			want: "mxbai-embed-large:1024",
		},
		{
			name: "unknown dimensions are omitted rather than guessed",
			got:  VectorIdentity(nomic, 0),
			want: "nomic-embed-text:v1.5+prefix",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("VectorIdentity = %q, want %q", tc.got, tc.want)
			}
		})
	}

	// Each of these must move the identity, or stale vectors would be kept.
	if VectorIdentity(nomic, 768) == VectorIdentity(nomic, 256) {
		t.Error("a dimension change does not change the identity: stale vectors would be kept")
	}
	if VectorIdentity(nomic, 768) == VectorIdentity(other, 768) {
		t.Error("a model change does not change the identity: stale vectors would be kept")
	}
	if VectorIdentity(nomic, 768) == VectorIdentity("nomic-embed-text:v1", 768) {
		t.Error("a tag change does not change the identity: stale vectors would be kept")
	}
}

// TestClientIdentityMatchesVectorIdentity keeps the string the worker stamps
// into memory_embeddings.model and the one the store filters on from drifting
// apart: both come from the client's configuration.
func TestClientIdentityMatchesVectorIdentity(t *testing.T) {
	c := NewClient("http://127.0.0.1:1", "nomic-embed-text:v1.5", 768)
	if got, want := c.Identity(), VectorIdentity(c.Model(), c.Dimensions()); got != want {
		t.Errorf("Client.Identity() = %q, want %q", got, want)
	}
}

func TestModelFamily(t *testing.T) {
	tests := []struct{ model, want string }{
		{"nomic-embed-text:v1.5", "nomic-embed-text"},
		{"nomic-embed-text", "nomic-embed-text"},
		{"nomic-embed-text:latest", "nomic-embed-text"},
		// Fully-qualified spellings are the same weights, and a family table
		// that missed them would skip the prefixes for a model the user
		// configured by its registry name.
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
}

// TestTaskPrefixesForQualifiedNomicName: the prefixes are keyed by family, so a
// registry-qualified name that resolved to a different family would embed
// asymmetric queries unprefixed and stamp an identity with no prefix marker —
// the fixture drift this whole mechanism exists to prevent, arrived at from the
// other direction.
func TestTaskPrefixesForQualifiedNomicName(t *testing.T) {
	const model = "registry.ollama.ai/library/nomic-embed-text:v1.5"
	p := TaskPrefixesFor(model)
	if p.Document != NomicTaskDocument || p.Query != NomicTaskQuery {
		t.Errorf("TaskPrefixesFor(%q) = %+v, want the nomic pair", model, p)
	}
	if got, want := VectorIdentity(model, 768), "registry.ollama.ai/library/nomic-embed-text:v1.5:768+prefix"; got != want {
		t.Errorf("VectorIdentity(%q) = %q, want %q", model, got, want)
	}
}

func TestTaskPrefixesForUnknownFamily(t *testing.T) {
	p := TaskPrefixesFor("some/other-model:v2")
	if p.Document != "" || p.Query != "" {
		t.Errorf("TaskPrefixesFor(unknown) = %+v, want no prefixes", p)
	}
}

// TestPrefixedHelpersMatchClient: callers with no Client of their own — the
// bench harnesses, which embed through a content-addressed cache — build their
// inputs with PrefixedDocument/PrefixedQuery, so those two have to be the
// byte-for-byte inputs EmbedDocument/EmbedQuery send. A divergence would put
// such a caller in a vector space production never searches while looking
// correct to every test that only consulted the helper.
func TestPrefixedHelpersMatchClient(t *testing.T) {
	const text = "the WAL lives in the data directory"
	for _, model := range []string{"nomic-embed-text:v1.5", "mxbai-embed-large"} {
		rec := newEmbedRecorder(3)
		c := NewClient(rec.server(t).URL, model, 3)
		ctx := context.Background()

		if _, err := c.EmbedDocument(ctx, text); err != nil {
			t.Fatalf("%s EmbedDocument: %v", model, err)
		}
		if _, err := c.EmbedQuery(ctx, text); err != nil {
			t.Fatalf("%s EmbedQuery: %v", model, err)
		}
		sent := rec.sent()
		if len(sent) != 2 {
			t.Fatalf("%s: sent %d inputs, want 2: %q", model, len(sent), sent)
		}
		if got, want := sent[0], PrefixedDocument(model, text); got != want {
			t.Errorf("%s: EmbedDocument sent %q, PrefixedDocument says %q", model, got, want)
		}
		if got, want := sent[1], PrefixedQuery(model, text); got != want {
			t.Errorf("%s: EmbedQuery sent %q, PrefixedQuery says %q", model, got, want)
		}
	}
}

// TestEmbedRejectsWrongDimensions keeps the dimension contract intact now that
// the request body carries a prefix: a short vector is still an error, not a
// silently stored row.
func TestEmbedRejectsWrongDimensions(t *testing.T) {
	rec := newEmbedRecorder(3)
	c := NewClient(rec.server(t).URL, "nomic-embed-text:v1.5", 5)
	if _, err := c.EmbedQuery(context.Background(), "x"); err == nil {
		t.Fatal("EmbedQuery with a dimension mismatch: want error, got nil")
	} else if !strings.Contains(err.Error(), "expected 5 dimensions") {
		t.Errorf("error = %v, want it to name the expected dimensions", err)
	}
}
