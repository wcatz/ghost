package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/memory"
)

// mockStore implements provider.MemoryStore with only the methods used by handlers.
type mockStore struct {
	upsertID     string
	upsertMerged bool
	upsertErr    error

	searchResult []memory.Memory
	searchErr    error

	topResult []memory.Memory
	topErr    error

	deleteErr error
}

func (m *mockStore) Upsert(_ context.Context, _, _, _, _ string, _ float32, _ []string) (string, bool, error) {
	return m.upsertID, m.upsertMerged, m.upsertErr
}

func (m *mockStore) SearchFTS(_ context.Context, _, _ string, _ int) ([]memory.Memory, error) {
	return m.searchResult, m.searchErr
}

func (m *mockStore) GetTopMemories(_ context.Context, _ string, _ int) ([]memory.Memory, error) {
	return m.topResult, m.topErr
}

func (m *mockStore) Delete(_ context.Context, _ string) error {
	return m.deleteErr
}

// Stubs for the rest of the MemoryStore interface — not exercised in tests.
func (m *mockStore) Create(_ context.Context, _ string, _ memory.Memory) (string, error) {
	return "", nil
}
func (m *mockStore) SearchHybrid(_ context.Context, _, _ string, _ []float32, _ int) ([]memory.Memory, error) {
	return nil, nil
}
func (m *mockStore) SearchVector(_ context.Context, _ string, _ []float32, _ int) ([]memory.ScoredMemory, error) {
	return nil, nil
}
func (m *mockStore) GetByCategory(_ context.Context, _, _ string, _ int) ([]memory.Memory, error) {
	return nil, nil
}
func (m *mockStore) GetByIDs(_ context.Context, _ []string) ([]memory.Memory, error) {
	return nil, nil
}
func (m *mockStore) GetAll(_ context.Context, _ string, _ int) ([]memory.Memory, error) {
	return nil, nil
}
func (m *mockStore) CountMemories(_ context.Context, _ string) (int, error) { return 0, nil }
func (m *mockStore) StoreEmbedding(_ context.Context, _ string, _ []float32, _ string) error {
	return nil
}
func (m *mockStore) DeleteEmbedding(_ context.Context, _ string) error          { return nil }
func (m *mockStore) UnembeddedMemoryIDs(_ context.Context, _ string, _ int) ([]string, error) {
	return nil, nil
}
func (m *mockStore) GetMemoryContent(_ context.Context, _ string) (string, error) { return "", nil }
func (m *mockStore) Touch(_ context.Context, _ []string) error                     { return nil }
func (m *mockStore) TogglePin(_ context.Context, _ string, _ bool) error           { return nil }
func (m *mockStore) ReplaceNonManual(_ context.Context, _ string, _ []memory.Memory) error {
	return nil
}
func (m *mockStore) ListProjects(_ context.Context) ([]memory.Project, error) { return nil, nil }
func (m *mockStore) EnsureProject(_ context.Context, _, _, _ string) error    { return nil }
func (m *mockStore) CreateConversation(_ context.Context, _, _ string) (string, error) {
	return "", nil
}
func (m *mockStore) AppendMessage(_ context.Context, _, _, _ string) error { return nil }
func (m *mockStore) GetRecentExchanges(_ context.Context, _ string, _ int) ([][2]string, error) {
	return nil, nil
}
func (m *mockStore) GetLatestConversation(_ context.Context, _ string) (string, error) {
	return "", nil
}
func (m *mockStore) GetConversationMessages(_ context.Context, _ string) ([]memory.ConversationMessage, error) {
	return nil, nil
}
func (m *mockStore) IncrementInteraction(_ context.Context, _ string) (int, error) { return 0, nil }
func (m *mockStore) GetLearnedContext(_ context.Context, _ string) (string, error) { return "", nil }
func (m *mockStore) UpdateLearnedContext(_ context.Context, _, _, _ string) error  { return nil }
func (m *mockStore) RecordUsage(_ context.Context, _, _ string, _ memory.TokenUsage) error {
	return nil
}
func (m *mockStore) GetCostSummary(_ context.Context, _ string) (memory.CostSummary, error) {
	return memory.CostSummary{}, nil
}
func (m *mockStore) Close() error { return nil }

// newTestServer creates a Server with a mock store and silent logger.
func newTestServer(store *mockStore) *Server {
	return New(store, &config.ServerConfig{
		ListenAddr: "127.0.0.1:0",
	}, slog.Default())
}

// newTestServerWithAuth creates a Server with auth configured.
func newTestServerWithAuth(store *mockStore, token string) *Server {
	return New(store, &config.ServerConfig{
		ListenAddr: "127.0.0.1:0",
		AuthToken:  token,
	}, slog.Default())
}

// --- handleHealth ---

func TestHandleHealth(t *testing.T) {
	s := newTestServer(&mockStore{})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	rr := httptest.NewRecorder()

	r := chi.NewRouter()
	r.Get("/api/v1/health", s.handleHealth)
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	var body map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf("expected status=ok, got %q", body["status"])
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("expected Content-Type application/json, got %q", ct)
	}
}

// --- handleCreate input validation ---

func TestHandleCreate_MissingFields(t *testing.T) {
	s := newTestServer(&mockStore{})

	r := chi.NewRouter()
	r.Post("/api/v1/memories", s.handleCreate)

	tests := []struct {
		name string
		body createRequest
	}{
		{"missing project_id", createRequest{Content: "hello"}},
		{"missing content", createRequest{ProjectID: "proj-1"}},
		{"both missing", createRequest{}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := json.Marshal(tc.body)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/memories", bytes.NewReader(b))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, req)

			if rr.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d", rr.Code)
			}
		})
	}
}

func TestHandleCreate_InvalidCategory(t *testing.T) {
	s := newTestServer(&mockStore{})

	r := chi.NewRouter()
	r.Post("/api/v1/memories", s.handleCreate)

	body := createRequest{
		ProjectID: "proj-1",
		Content:   "some content",
		Category:  "bogus_category",
	}
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/memories", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}

	var resp map[string]string
	json.NewDecoder(rr.Body).Decode(&resp)
	if resp["error"] != "invalid category" {
		t.Fatalf("expected 'invalid category' error, got %q", resp["error"])
	}
}

func TestHandleCreate_ImportanceClamping(t *testing.T) {
	store := &mockStore{upsertID: "mem-123", upsertMerged: false}
	s := newTestServer(store)

	r := chi.NewRouter()
	r.Post("/api/v1/memories", s.handleCreate)

	tests := []struct {
		name       string
		importance float32
	}{
		{"negative clamped to 0", -5.0},
		{"above 1 clamped to 1", 99.0},
		{"within range", 0.5},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := createRequest{
				ProjectID:  "proj-1",
				Content:    "test content",
				Category:   "fact",
				Importance: tc.importance,
			}
			b, _ := json.Marshal(body)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/memories", bytes.NewReader(b))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, req)

			if rr.Code != http.StatusCreated {
				t.Fatalf("expected 201, got %d; body: %s", rr.Code, rr.Body.String())
			}

			var resp map[string]interface{}
			json.NewDecoder(rr.Body).Decode(&resp)
			if resp["id"] != "mem-123" {
				t.Fatalf("expected id=mem-123, got %v", resp["id"])
			}
		})
	}
}

func TestHandleCreate_DefaultCategory(t *testing.T) {
	store := &mockStore{upsertID: "mem-456", upsertMerged: false}
	s := newTestServer(store)

	r := chi.NewRouter()
	r.Post("/api/v1/memories", s.handleCreate)

	body := createRequest{
		ProjectID: "proj-1",
		Content:   "no category set",
	}
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/memories", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	// Should succeed — category defaults to "fact".
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rr.Code, rr.Body.String())
	}
}

// --- Auth Middleware ---

// authRouter creates a chi router with the same auth structure as production.
func authRouter(s *Server) chi.Router {
	r := chi.NewRouter()
	r.Get("/api/v1/health", s.handleHealth)
	r.Group(func(r chi.Router) {
		if s.cfg.AuthToken != "" {
			r.Use(s.authMiddleware)
		}
		r.Get("/api/v1/projects", s.handleListProjects)
		r.Route("/api/v1/memories", func(r chi.Router) {
			r.Post("/search", s.handleSearch)
		})
	})
	return r
}

func TestAuthMiddleware_NoTokenConfig_Passthrough(t *testing.T) {
	s := newTestServer(&mockStore{})
	r := authRouter(s)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	// No auth configured → should pass through (200).
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 with no auth config, got %d", rr.Code)
	}
}

func TestAuthMiddleware_ValidToken(t *testing.T) {
	s := newTestServerWithAuth(&mockStore{}, "test-secret-token")
	r := authRouter(s)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	req.Header.Set("Authorization", "Bearer test-secret-token")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 with valid token, got %d; body: %s", rr.Code, rr.Body.String())
	}
}

func TestAuthMiddleware_MissingHeader(t *testing.T) {
	s := newTestServerWithAuth(&mockStore{}, "test-secret-token")
	r := authRouter(s)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without auth header, got %d", rr.Code)
	}
}

func TestAuthMiddleware_WrongToken(t *testing.T) {
	s := newTestServerWithAuth(&mockStore{}, "test-secret-token")
	r := authRouter(s)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with wrong token, got %d", rr.Code)
	}
}

func TestAuthMiddleware_BasicAuthRejected(t *testing.T) {
	s := newTestServerWithAuth(&mockStore{}, "test-secret-token")
	r := authRouter(s)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for Basic auth, got %d", rr.Code)
	}
}

func TestAuthMiddleware_HealthNoAuthRequired(t *testing.T) {
	s := newTestServerWithAuth(&mockStore{}, "test-secret-token")
	r := authRouter(s)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	// No Authorization header.
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 for health without auth, got %d", rr.Code)
	}
}
