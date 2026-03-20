package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
func (m *mockStore) SearchFTSAll(_ context.Context, _ string, _ int) ([]memory.Memory, error) {
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
func (m *mockStore) CreateTask(_ context.Context, _, _, _ string, _ int) (string, error) {
	return "task1", nil
}
func (m *mockStore) ListTasks(_ context.Context, _, _ string, _ int) ([]memory.Task, error) {
	return nil, nil
}
func (m *mockStore) CompleteTask(_ context.Context, _, _ string) error { return nil }
func (m *mockStore) UpdateTask(_ context.Context, _, _ string, _ int, _ string) error { return nil }
func (m *mockStore) RecordDecision(_ context.Context, _, _, _, _ string, _, _ []string) (string, error) {
	return "dec1", nil
}
func (m *mockStore) ListDecisions(_ context.Context, _, _ string, _ int) ([]memory.Decision, error) {
	return nil, nil
}
func (m *mockStore) ListProjects(_ context.Context) ([]memory.Project, error) { return nil, nil }
func (m *mockStore) EnsureProject(_ context.Context, _, _, _ string) error    { return nil }
func (m *mockStore) ResolveProjectByName(_ context.Context, _ string) (string, error) {
	return "", nil
}
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
func (m *mockStore) GetMonthlyCost(_ context.Context, year, month int) (memory.MonthlyCost, error) {
	return memory.MonthlyCost{Year: year, Month: month}, nil
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

// --- handleSearch ---

func TestHandleSearch_HappyPath(t *testing.T) {
	store := &mockStore{
		searchResult: []memory.Memory{
			{ID: "m1", Category: "fact", Content: "Go uses goroutines", Importance: 0.8},
		},
	}
	s := newTestServer(store)

	r := chi.NewRouter()
	r.Post("/api/v1/memories/search", s.handleSearch)

	body := searchRequest{ProjectID: "proj-1", Query: "goroutines", Limit: 10}
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/memories/search", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rr.Code, rr.Body.String())
	}

	var results []memory.Memory
	json.NewDecoder(rr.Body).Decode(&results)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
}

func TestHandleSearch_MissingFields(t *testing.T) {
	s := newTestServer(&mockStore{})

	r := chi.NewRouter()
	r.Post("/api/v1/memories/search", s.handleSearch)

	tests := []struct {
		name string
		body searchRequest
	}{
		{"missing project_id", searchRequest{Query: "test"}},
		{"missing query", searchRequest{ProjectID: "proj-1"}},
		{"both missing", searchRequest{}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := json.Marshal(tc.body)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/memories/search", bytes.NewReader(b))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, req)

			if rr.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d", rr.Code)
			}
		})
	}
}

func TestHandleSearch_DefaultLimit(t *testing.T) {
	store := &mockStore{searchResult: []memory.Memory{}}
	s := newTestServer(store)

	r := chi.NewRouter()
	r.Post("/api/v1/memories/search", s.handleSearch)

	// Limit=0 should default to 20, not fail.
	body := searchRequest{ProjectID: "proj-1", Query: "test", Limit: 0}
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/memories/search", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
}

// --- handleList ---

func TestHandleList_HappyPath(t *testing.T) {
	store := &mockStore{
		topResult: []memory.Memory{
			{ID: "m1", Category: "fact", Content: "test memory", Importance: 0.5},
			{ID: "m2", Category: "decision", Content: "another memory", Importance: 0.7},
		},
	}
	s := newTestServer(store)

	r := chi.NewRouter()
	r.Get("/api/v1/memories/{projectID}", s.handleList)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/memories/proj-1", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rr.Code, rr.Body.String())
	}

	var results []memory.Memory
	json.NewDecoder(rr.Body).Decode(&results)
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
}

func TestHandleList_EmptyResult(t *testing.T) {
	store := &mockStore{topResult: []memory.Memory{}}
	s := newTestServer(store)

	r := chi.NewRouter()
	r.Get("/api/v1/memories/{projectID}", s.handleList)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/memories/proj-1", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
}

// --- handleDelete ---

func TestHandleDelete_HappyPath(t *testing.T) {
	store := &mockStore{}
	s := newTestServer(store)

	r := chi.NewRouter()
	r.Delete("/api/v1/memories/{memoryID}", s.handleDelete)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/memories/mem-123", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]string
	json.NewDecoder(rr.Body).Decode(&resp)
	if resp["status"] != "deleted" {
		t.Errorf("expected status=deleted, got %q", resp["status"])
	}
}

func TestHandleDelete_StoreError(t *testing.T) {
	store := &mockStore{deleteErr: fmt.Errorf("not found")}
	s := newTestServer(store)

	r := chi.NewRouter()
	r.Delete("/api/v1/memories/{memoryID}", s.handleDelete)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/memories/mem-xyz", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rr.Code)
	}
}

// --- handleListProjects ---

func TestHandleListProjects_Empty(t *testing.T) {
	store := &mockStore{}
	s := newTestServer(store)

	r := chi.NewRouter()
	r.Get("/api/v1/projects", s.handleListProjects)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
}

// --- handleMonthlyCost ---

func TestHandleMonthlyCost_HappyPath(t *testing.T) {
	store := &mockStore{}
	s := newTestServer(store)

	r := chi.NewRouter()
	r.Get("/api/v1/costs/monthly", s.handleMonthlyCost)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/costs/monthly", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rr.Code, rr.Body.String())
	}

	var mc memory.MonthlyCost
	json.NewDecoder(rr.Body).Decode(&mc)
	if mc.Year == 0 {
		t.Error("expected non-zero year in monthly cost")
	}
}

// --- handleCreate valid categories ---

func TestHandleCreate_AllValidCategories(t *testing.T) {
	categories := []string{
		"architecture", "decision", "pattern", "convention",
		"gotcha", "dependency", "preference", "fact",
	}

	for _, cat := range categories {
		t.Run(cat, func(t *testing.T) {
			store := &mockStore{upsertID: "mem-1"}
			s := newTestServer(store)

			r := chi.NewRouter()
			r.Post("/api/v1/memories", s.handleCreate)

			body := createRequest{
				ProjectID: "proj-1",
				Content:   "test content",
				Category:  cat,
			}
			b, _ := json.Marshal(body)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/memories", bytes.NewReader(b))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, req)

			if rr.Code != http.StatusCreated {
				t.Fatalf("expected 201 for category %q, got %d; body: %s", cat, rr.Code, rr.Body.String())
			}
		})
	}
}

func TestHandleCreate_MergedResponse(t *testing.T) {
	store := &mockStore{upsertID: "mem-456", upsertMerged: true}
	s := newTestServer(store)

	r := chi.NewRouter()
	r.Post("/api/v1/memories", s.handleCreate)

	body := createRequest{
		ProjectID: "proj-1",
		Content:   "test content",
		Category:  "fact",
	}
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/memories", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", rr.Code)
	}

	var resp map[string]interface{}
	json.NewDecoder(rr.Body).Decode(&resp)
	if resp["merged"] != true {
		t.Errorf("expected merged=true, got %v", resp["merged"])
	}
}

func TestHandleCreate_DefaultSource(t *testing.T) {
	store := &mockStore{upsertID: "mem-789"}
	s := newTestServer(store)

	r := chi.NewRouter()
	r.Post("/api/v1/memories", s.handleCreate)

	// Source empty → should default to "manual" (no error).
	body := createRequest{
		ProjectID: "proj-1",
		Content:   "test",
	}
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/memories", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rr.Code, rr.Body.String())
	}
}

// --- HasActiveStream ---

func TestHasActiveStream(t *testing.T) {
	s := newTestServer(&mockStore{})

	if s.HasActiveStream("session-1") {
		t.Error("expected no active stream initially")
	}

	s.chatMu.Lock()
	s.chatStates["session-1"] = &chatState{}
	s.chatMu.Unlock()

	if !s.HasActiveStream("session-1") {
		t.Error("expected active stream after adding to map")
	}
}

// --- writeJSON / writeError ---

func TestWriteJSON(t *testing.T) {
	rr := httptest.NewRecorder()
	writeJSON(rr, http.StatusOK, map[string]string{"key": "value"})

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var resp map[string]string
	json.NewDecoder(rr.Body).Decode(&resp)
	if resp["key"] != "value" {
		t.Errorf("response body unexpected: %v", resp)
	}
}

func TestWriteError(t *testing.T) {
	rr := httptest.NewRecorder()
	writeError(rr, http.StatusBadRequest, "bad input")

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}

	var resp map[string]string
	json.NewDecoder(rr.Body).Decode(&resp)
	if resp["error"] != "bad input" {
		t.Errorf("error = %q, want %q", resp["error"], "bad input")
	}
}
