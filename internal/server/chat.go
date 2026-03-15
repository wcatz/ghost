package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/wcatz/ghost/internal/ai"
	"github.com/wcatz/ghost/internal/provider"
)

// sessionInfo is the JSON representation of a session.
type sessionInfo struct {
	ID          string    `json:"id"`
	ProjectPath string    `json:"project_path"`
	ProjectName string    `json:"project_name"`
	Mode        string    `json:"mode"`
	Active      bool      `json:"active"`
	Messages    int       `json:"messages"`
	CreatedAt   time.Time `json:"created_at"`
	LastActive  time.Time `json:"last_active_at"`
}

// pendingApproval tracks a tool approval waiting for the HTTP client.
type pendingApproval struct {
	ToolName string          `json:"tool_name"`
	Input    json.RawMessage `json:"input"`
	response chan bool
}

// chatState holds per-session streaming state for HTTP clients.
type chatState struct {
	mu       sync.Mutex
	pending  *pendingApproval
}

// --- Session Handlers ---

type createSessionRequest struct {
	Path string `json:"path"`
	Mode string `json:"mode,omitempty"`
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req createSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Path == "" {
		writeError(w, http.StatusBadRequest, "path is required")
		return
	}

	session, err := s.orchestrator.StartSession(req.Path)
	if err != nil {
		s.logger.Error("start session", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to start session")
		return
	}

	if req.Mode != "" {
		session.SetMode(req.Mode)
	}

	writeJSON(w, http.StatusCreated, sessionInfo{
		ID:          session.ProjectID,
		ProjectPath: session.ProjectPath,
		ProjectName: session.ProjectName,
		Mode:        session.Mode.Name,
		Active:      session.Active,
		Messages:    session.MessageCount(),
		CreatedAt:   session.CreatedAt,
		LastActive:  session.LastActiveAt,
	})
}

func (s *Server) handleListSessions(w http.ResponseWriter, _ *http.Request) {
	sessions := s.orchestrator.ListSessions()
	infos := make([]sessionInfo, len(sessions))
	for i, sess := range sessions {
		infos[i] = sessionInfo{
			ID:          sess.ProjectID,
			ProjectPath: sess.ProjectPath,
			ProjectName: sess.ProjectName,
			Mode:        sess.Mode.Name,
			Active:      sess.Active,
			Messages:    sess.MessageCount(),
			CreatedAt:   sess.CreatedAt,
			LastActive:  sess.LastActiveAt,
		}
	}
	writeJSON(w, http.StatusOK, infos)
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	session := s.orchestrator.GetSessionByID(id)
	if session == nil {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	if err := s.orchestrator.StopSession(session.ProjectPath); err != nil {
		s.logger.Error("stop session", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to stop session")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
}

// --- Chat SSE Handler ---

type sendRequest struct {
	Message string `json:"message"`
}

func (s *Server) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	session := s.orchestrator.GetSessionByID(id)
	if session == nil {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}

	var req sendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Message == "" {
		writeError(w, http.StatusBadRequest, "message is required")
		return
	}

	// Reject concurrent sends — only one stream per session.
	// Must happen before writing SSE headers (can't send JSON error after SSE 200).
	state := &chatState{}
	s.chatMu.Lock()
	if _, active := s.chatStates[id]; active {
		s.chatMu.Unlock()
		writeError(w, http.StatusConflict, "stream already active for this session")
		return
	}
	s.chatStates[id] = state
	s.chatMu.Unlock()

	// Set up SSE headers.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		s.chatMu.Lock()
		delete(s.chatStates, id)
		s.chatMu.Unlock()
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	// Create approval channel for this request.
	approvalCh := make(chan provider.ApprovalRequest, 1)
	defer func() {
		s.chatMu.Lock()
		// Only delete if we own the state (prevents race on cleanup).
		if s.chatStates[id] == state {
			delete(s.chatStates, id)
		}
		s.chatMu.Unlock()
	}()

	// Start streaming.
	events := session.SendAsync(r.Context(), req.Message, approvalCh)

	for {
		select {
		case evt, ok := <-events:
			if !ok {
				// Stream complete.
				writeSSE(w, flusher, "done", map[string]string{"status": "complete"})
				return
			}
			s.handleStreamEvent(w, flusher, evt)

		case approval := <-approvalCh:
			// Tool needs approval — emit event and store pending.
			// We need a bidirectional channel to both send and receive.
			respCh := make(chan bool, 1)
			// Forward the response back to the approval request's send-only channel.
			go func() {
				v := <-respCh
				approval.Response <- v
			}()
			state.mu.Lock()
			state.pending = &pendingApproval{
				ToolName: approval.ToolName,
				Input:    approval.Input,
				response: respCh,
			}
			state.mu.Unlock()

			writeSSE(w, flusher, "approval_required", map[string]interface{}{
				"tool_name": approval.ToolName,
				"input":     json.RawMessage(approval.Input),
			})

		case <-r.Context().Done():
			return
		}
	}
}

func (s *Server) handleStreamEvent(w http.ResponseWriter, flusher http.Flusher, evt ai.StreamEvent) {
	switch evt.Type {
	case "text":
		writeSSE(w, flusher, "text", map[string]string{"text": evt.Text})
	case "thinking":
		writeSSE(w, flusher, "thinking", map[string]string{"text": evt.Text})
	case "tool_use_start":
		if evt.ToolUse != nil {
			writeSSE(w, flusher, "tool_use_start", map[string]string{
				"id":   evt.ToolUse.ID,
				"name": evt.ToolUse.Name,
			})
		}
	case "tool_input_delta":
		if evt.ToolUse != nil {
			writeSSE(w, flusher, "tool_input_delta", map[string]string{
				"id":    evt.ToolUse.ID,
				"delta": evt.ToolUse.InputDelta,
			})
		}
	case "tool_use_end":
		if evt.ToolUse != nil {
			writeSSE(w, flusher, "tool_use_end", map[string]string{
				"id":   evt.ToolUse.ID,
				"name": evt.ToolUse.Name,
			})
		}
	case "done":
		data := map[string]interface{}{
			"stop_reason": evt.StopReason,
		}
		if evt.Usage != nil {
			data["usage"] = evt.Usage
		}
		writeSSE(w, flusher, "done", data)
	case "error":
		msg := "unknown error"
		if evt.Error != nil {
			msg = evt.Error.Error()
		}
		writeSSE(w, flusher, "error", map[string]string{"error": msg})
	}
}

// --- Approval Handler ---

type approvalResponse struct {
	Approved bool `json:"approved"`
}

func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req approvalResponse
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	s.chatMu.RLock()
	state, ok := s.chatStates[id]
	s.chatMu.RUnlock()

	if !ok {
		writeError(w, http.StatusNotFound, "no active stream for session")
		return
	}

	state.mu.Lock()
	pending := state.pending
	state.pending = nil
	state.mu.Unlock()

	if pending == nil {
		writeError(w, http.StatusConflict, "no pending approval")
		return
	}

	pending.response <- req.Approved
	writeJSON(w, http.StatusOK, map[string]string{"status": "responded"})
}

// --- Mode Handler ---

type modeRequest struct {
	Mode string `json:"mode"`
}

func (s *Server) handleSetMode(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	session := s.orchestrator.GetSessionByID(id)
	if session == nil {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}

	var req modeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Mode == "" {
		writeError(w, http.StatusBadRequest, "mode is required")
		return
	}

	session.SetMode(req.Mode)
	writeJSON(w, http.StatusOK, map[string]string{
		"mode": session.Mode.Name,
	})
}

// --- SSE Helper ---

func writeSSE(w http.ResponseWriter, flusher http.Flusher, eventType string, data interface{}) {
	b, err := json.Marshal(data)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, b)
	flusher.Flush()
}
