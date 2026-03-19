// Package server implements the ghost serve HTTP daemon.
// It exposes a REST API for memory operations and health checks.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/orchestrator"
	"github.com/wcatz/ghost/internal/provider"
	"github.com/wcatz/ghost/internal/voice"
)

// ApprovalNotifier is called when a tool needs approval.
// The Telegram bot implements this to forward approvals to the user's phone.
type ApprovalNotifier interface {
	NotifyApproval(sessionID, projectName, toolName string, input json.RawMessage)
}

// Server is the ghost HTTP daemon.
type Server struct {
	store             provider.MemoryStore
	cfg               *config.ServerConfig
	orchestrator      *orchestrator.Orchestrator
	approvalNotifier  ApprovalNotifier
	assemblyAIKey     string // AssemblyAI API key for token proxy
	logger            *slog.Logger
	srv               *http.Server

	// Chat streaming state.
	chatMu     sync.RWMutex
	chatStates map[string]*chatState // key: session/project ID
}

// New creates a new server.
func New(store provider.MemoryStore, cfg *config.ServerConfig, logger *slog.Logger) *Server {
	return &Server{
		store:      store,
		cfg:        cfg,
		logger:     logger,
		chatStates: make(map[string]*chatState),
	}
}

// SetOrchestrator wires the orchestrator for chat endpoints.
// Must be called before Run() if chat endpoints are needed.
func (s *Server) SetOrchestrator(o *orchestrator.Orchestrator) {
	s.orchestrator = o
}

// SetApprovalNotifier wires an external approval forwarder (e.g. Telegram bot).
func (s *Server) SetApprovalNotifier(n ApprovalNotifier) {
	s.approvalNotifier = n
}

// SetAssemblyAIKey enables the /api/v1/transcribe/token endpoint.
func (s *Server) SetAssemblyAIKey(key string) {
	s.assemblyAIKey = key
}

// Run starts the HTTP server. Blocks until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	r := chi.NewRouter()

	// Middleware (no global timeout — SSE streams run indefinitely).
	r.Use(middleware.Recoverer)
	r.Use(middleware.RealIP)
	r.Use(s.logMiddleware)

	// Routes.
	r.Get("/api/v1/health", s.handleHealth)

	// Authenticated routes — auth enforced only when auth_token is configured.
	r.Group(func(r chi.Router) {
		if s.cfg.AuthToken != "" {
			r.Use(s.authMiddleware)
		}
		r.Route("/api/v1/memories", func(r chi.Router) {
			r.Use(bodyLimitMiddleware(1 << 20)) // 1MB
			r.Post("/search", s.handleSearch)
			r.Post("/", s.handleCreate)
			r.Get("/{projectID}", s.handleList)
			r.Delete("/{memoryID}", s.handleDelete)
		})
		r.Get("/api/v1/projects", s.handleListProjects)

		// Transcription token proxy (requires AssemblyAI key).
		if s.assemblyAIKey != "" {
			r.Get("/api/v1/transcribe/token", s.handleTranscribeToken)
		}

		// Chat streaming endpoints (requires orchestrator).
		if s.orchestrator != nil {
			r.Route("/api/v1/sessions", func(r chi.Router) {
				r.Post("/", withBodyLimit(s.handleCreateSession, 1<<20))      // 1MB
				r.Get("/", s.handleListSessions)
				r.Delete("/{id}", s.handleDeleteSession)
				r.Post("/{id}/send", withBodyLimit(s.handleSendMessage, 10<<20)) // 10MB (large messages)
				r.Post("/{id}/approve", withBodyLimit(s.handleApprove, 1<<20))
				r.Post("/{id}/mode", withBodyLimit(s.handleSetMode, 1<<20))
				r.Post("/{id}/auto-approve", withBodyLimit(s.handleAutoApprove, 1<<20))
				r.Get("/{id}/history", s.handleHistory)
			})
		}
	})

	s.srv = &http.Server{
		Addr:              s.cfg.ListenAddr,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
		BaseContext:       func(_ net.Listener) context.Context { return ctx },
	}

	if s.cfg.AuthToken != "" {
		s.logger.Info("bearer auth enabled for API routes")
	} else {
		s.logger.Warn("no auth_token configured, API is unauthenticated")
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
	if err := s.store.EnsureProject(r.Context(), req.ProjectID, req.ProjectID, req.ProjectID); err != nil {
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

// handleTranscribeToken proxies a temporary AssemblyAI streaming token
// for browser-direct real-time WebSocket transcription.
func (s *Server) handleTranscribeToken(w http.ResponseWriter, r *http.Request) {
	stt := voice.NewAssemblyAIStreamSTT(s.assemblyAIKey)
	token, err := stt.CreateToken(r.Context())
	if err != nil {
		s.logger.Error("transcribe token", "error", err)
		writeError(w, http.StatusBadGateway, "failed to create streaming token")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"token":  token,
		"ws_url": "wss://streaming.assemblyai.com/v3/ws",
	})
}

// --- Middleware ---

// authMiddleware validates Bearer token authentication.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(auth, prefix) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		token := auth[len(prefix):]
		if subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.AuthToken)) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

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

// --- Body Limit ---

// bodyLimitMiddleware wraps all requests in the group with a body size limit.
func bodyLimitMiddleware(maxBytes int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// withBodyLimit wraps a single handler with a body size limit.
func withBodyLimit(h http.HandlerFunc, maxBytes int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
		}
		h(w, r)
	}
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

