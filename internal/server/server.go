// Package server implements the ghost serve HTTP daemon.
// It exposes a REST API for memory operations and health checks.
package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/provider"
)

// Server is the ghost HTTP daemon.
type Server struct {
	store  provider.MemoryStore
	cfg    *config.ServerConfig
	logger *slog.Logger
	srv    *http.Server
}

// New creates a new server.
func New(store provider.MemoryStore, cfg *config.ServerConfig, logger *slog.Logger) *Server {
	return &Server{
		store:  store,
		cfg:    cfg,
		logger: logger,
	}
}

// Run starts the HTTP server. Blocks until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	r := chi.NewRouter()

	// Middleware.
	r.Use(middleware.Recoverer)
	r.Use(middleware.RealIP)
	r.Use(s.logMiddleware)
	r.Use(middleware.Timeout(30 * time.Second))

	// Routes.
	r.Get("/api/v1/health", s.handleHealth)
	r.Route("/api/v1/memories", func(r chi.Router) {
		r.Post("/search", s.handleSearch)
		r.Post("/", s.handleCreate)
		r.Get("/{projectID}", s.handleList)
		r.Delete("/{memoryID}", s.handleDelete)
	})
	r.Get("/api/v1/projects", s.handleListProjects)

	s.srv = &http.Server{
		Addr:              s.cfg.ListenAddr,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(_ net.Listener) context.Context { return ctx },
	}

	s.logger.Info("ghost serve starting", "addr", s.cfg.ListenAddr)

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return s.srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}

// --- Handlers ---

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "ok",
		"version": "dev",
	})
}

type searchRequest struct {
	ProjectID string `json:"project_id"`
	Query     string `json:"query"`
	Limit     int    `json:"limit"`
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	var req searchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ProjectID == "" || req.Query == "" {
		writeError(w, http.StatusBadRequest, "project_id and query are required")
		return
	}
	if req.Limit <= 0 || req.Limit > 100 {
		req.Limit = 20
	}

	// Use FTS search (hybrid search can be wired in when embedding client is available).
	memories, err := s.store.SearchFTS(r.Context(), req.ProjectID, req.Query, req.Limit)
	if err != nil {
		s.logger.Error("search", "error", err)
		writeError(w, http.StatusInternalServerError, "search failed")
		return
	}
	writeJSON(w, http.StatusOK, memories)
}

type createRequest struct {
	ProjectID  string   `json:"project_id"`
	Category   string   `json:"category"`
	Content    string   `json:"content"`
	Source     string   `json:"source"`
	Importance float32  `json:"importance"`
	Tags       []string `json:"tags"`
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ProjectID == "" || req.Content == "" {
		writeError(w, http.StatusBadRequest, "project_id and content are required")
		return
	}
	if req.Category == "" {
		req.Category = "fact"
	}
	validCategories := map[string]bool{
		"architecture": true, "decision": true, "pattern": true, "convention": true,
		"gotcha": true, "dependency": true, "preference": true, "fact": true,
	}
	if !validCategories[req.Category] {
		writeError(w, http.StatusBadRequest, "invalid category")
		return
	}
	if req.Importance < 0 {
		req.Importance = 0
	}
	if req.Importance > 1 {
		req.Importance = 1
	}
	if req.Source == "" {
		req.Source = "manual"
	}
	if req.Tags == nil {
		req.Tags = []string{}
	}

	// Auto-ensure project exists before creating memory.
	if err := s.store.EnsureProject(r.Context(), req.ProjectID, "", req.ProjectID); err != nil {
		s.logger.Error("ensure project", "error", err)
		writeError(w, http.StatusInternalServerError, "create failed")
		return
	}

	id, merged, err := s.store.Upsert(r.Context(), req.ProjectID, req.Category, req.Content, req.Source, req.Importance, req.Tags)
	if err != nil {
		s.logger.Error("create memory", "error", err)
		writeError(w, http.StatusInternalServerError, "create failed")
		return
	}

	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"id":     id,
		"merged": merged,
	})
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	if projectID == "" {
		writeError(w, http.StatusBadRequest, "project ID required")
		return
	}

	memories, err := s.store.GetTopMemories(r.Context(), projectID, 50)
	if err != nil {
		s.logger.Error("list memories", "error", err)
		writeError(w, http.StatusInternalServerError, "list failed")
		return
	}
	writeJSON(w, http.StatusOK, memories)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	memoryID := chi.URLParam(r, "memoryID")
	if memoryID == "" {
		writeError(w, http.StatusBadRequest, "memory ID required")
		return
	}

	if err := s.store.Delete(r.Context(), memoryID); err != nil {
		s.logger.Error("delete memory", "error", err)
		writeError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	projects, err := s.store.ListProjects(r.Context())
	if err != nil {
		s.logger.Error("list projects", "error", err)
		writeError(w, http.StatusInternalServerError, "list failed")
		return
	}
	writeJSON(w, http.StatusOK, projects)
}

// --- Middleware ---

func (s *Server) logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		s.logger.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", ww.Status(),
			"bytes", ww.BytesWritten(),
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

// --- Helpers ---

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

