package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/wcatz/ghost/internal/claudeimport"
	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/project"
	"github.com/wcatz/ghost/internal/prompt"
	"github.com/wcatz/ghost/internal/provider"
	"github.com/wcatz/ghost/internal/reflection"
	"github.com/wcatz/ghost/internal/tool"
)

// Orchestrator manages multiple project sessions.
type Orchestrator struct {
	mu       sync.RWMutex
	sessions map[string]*Session // key: project ID
	client   provider.LLMProvider
	store    provider.MemoryStore
	registry *tool.Registry
	cfg      *config.Config
	logger   *slog.Logger
}

// New creates a new orchestrator.
func New(client provider.LLMProvider, store provider.MemoryStore, registry *tool.Registry, cfg *config.Config, logger *slog.Logger) *Orchestrator {
	return &Orchestrator{
		sessions: make(map[string]*Session),
		client:   client,
		store:    store,
		registry: registry,
		cfg:      cfg,
		logger:   logger,
	}
}

// StartSession initializes a project session. Idempotent — returns existing if already started.
func (o *Orchestrator) StartSession(projectPath string) (*Session, error) {
	projCtx, err := project.Detect(projectPath)
	if err != nil {
		return nil, fmt.Errorf("detect project %s: %w", projectPath, err)
	}

	o.mu.Lock()
	defer o.mu.Unlock()

	if s, ok := o.sessions[projCtx.ID]; ok {
		return s, nil
	}

	// Ensure project exists in database.
	ctx := context.Background()
	if err := o.store.EnsureProject(ctx, projCtx.ID, projCtx.Path, projCtx.Name); err != nil {
		return nil, fmt.Errorf("ensure project: %w", err)
	}

	builder := prompt.NewBuilder(o.store)
	consolidator := o.buildConsolidator()
	reflector := reflection.NewEngine(consolidator, o.store, o.logger, o.cfg.Defaults.ReflectionInterval)

	s := NewSession(
		projCtx,
		o.client,
		o.store,
		o.registry,
		builder,
		reflector,
		o.logger,
		o.cfg.API.ModelQuality,
		o.cfg.API.ModelFast,
		o.cfg.Defaults.Mode,
	)

	if o.cfg.Defaults.ApprovalMode == "yolo" {
		s.SetAutoApprove(true)
	}

	o.sessions[projCtx.ID] = s
	o.logger.Info("session started", "project", projCtx.Name, "path", projCtx.Path, "id", projCtx.ID)

	// Cold-start: import Claude Code memories first, fall back to LLM onboarding.
	count, err := o.store.CountMemories(ctx, projCtx.ID)
	if err == nil && count == 0 {
		go func() {
			bgCtx := context.Background()
			imported, importErr := claudeimport.Import(bgCtx, o.store, projCtx.ID, projCtx.Path, o.logger)
			if importErr != nil {
				o.logger.Warn("claude memory import failed", "project", projCtx.Name, "error", importErr)
			}
			if imported == 0 && o.client != nil {
				onboardProject(bgCtx, o.client, o.store, projCtx, o.logger)
			}
		}()
	}

	return s, nil
}

// ResumeSession creates a session and loads its latest conversation from SQLite.
func (o *Orchestrator) ResumeSession(projectPath string) (*Session, error) {
	s, err := o.StartSession(projectPath)
	if err != nil {
		return nil, err
	}

	// Only resume if the session has no messages (freshly created).
	if s.MessageCount() == 0 {
		ctx := context.Background()
		if err := s.Resume(ctx); err != nil {
			o.logger.Info("no previous conversation to resume", "project", s.ProjectName, "reason", err)
			// Not fatal — just start fresh.
		}
	}
	return s, nil
}

// GetSession returns an existing session or nil.
func (o *Orchestrator) GetSession(projectPath string) *Session {
	projCtx, err := project.Detect(projectPath)
	if err != nil {
		return nil
	}
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.sessions[projCtx.ID]
}

// GetSessionByID returns a session by project ID.
func (o *Orchestrator) GetSessionByID(id string) *Session {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.sessions[id]
}

// ListSessions returns all active sessions.
func (o *Orchestrator) ListSessions() []*Session {
	o.mu.RLock()
	defer o.mu.RUnlock()

	sessions := make([]*Session, 0, len(o.sessions))
	for _, s := range o.sessions {
		sessions = append(sessions, s)
	}
	return sessions
}

// StopSession gracefully shuts down a project session.
func (o *Orchestrator) StopSession(projectPath string) error {
	projCtx, err := project.Detect(projectPath)
	if err != nil {
		return err
	}

	o.mu.Lock()
	defer o.mu.Unlock()

	if s, ok := o.sessions[projCtx.ID]; ok {
		s.mu.Lock()
		s.Active = false
		s.mu.Unlock()
		delete(o.sessions, projCtx.ID)
		o.logger.Info("session stopped", "project", projCtx.Name)
	}
	return nil
}

// Shutdown stops all sessions.
func (o *Orchestrator) Shutdown(_ context.Context) error {
	o.mu.Lock()
	toStop := make([]*Session, 0, len(o.sessions))
	for _, s := range o.sessions {
		toStop = append(toStop, s)
	}
	o.sessions = make(map[string]*Session)
	o.mu.Unlock()

	for _, s := range toStop {
		s.mu.Lock()
		s.Active = false
		s.mu.Unlock()
	}
	return nil
}

// buildConsolidator creates the appropriate memory consolidator based on config.
func (o *Orchestrator) buildConsolidator() reflection.Consolidator {
	backend := o.cfg.Reflection.Backend
	if backend == "" {
		backend = "auto"
	}

	switch backend {
	case "haiku":
		return reflection.NewHaikuConsolidator(o.client)
	case "ollama":
		return reflection.NewOllamaConsolidator(o.cfg.Embedding.OllamaURL, o.cfg.Reflection.OllamaModel)
	case "sqlite":
		return reflection.NewSQLiteConsolidator()
	case "disabled":
		return nil
	default: // "auto"
		var tiers []reflection.Consolidator
		if o.client != nil {
			tiers = append(tiers, reflection.NewHaikuConsolidator(o.client))
		}
		if o.cfg.Embedding.OllamaURL != "" {
			tiers = append(tiers, reflection.NewOllamaConsolidator(o.cfg.Embedding.OllamaURL, o.cfg.Reflection.OllamaModel))
		}
		tiers = append(tiers, reflection.NewSQLiteConsolidator())
		return reflection.NewTieredConsolidator(tiers, o.logger)
	}
}
