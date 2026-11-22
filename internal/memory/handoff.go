package memory

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// SessionHandoff is the short state a pre-compact hook records for the next
// session-start. It is a session-retention row: it expires on the same clock a
// session-tier memory does and is never exported or consolidated. The pointer
// fields name the raw history the handoff was derived from; the content is the
// handoff itself, stored as data and rendered through the shared renderer.
type SessionHandoff struct {
	ProjectID          string
	SessionID          string
	TranscriptPath     string
	TranscriptPosition int
	Content            string
	CreatedAt          string
	ExpiresAt          *string
}

// MaxHandoffContentBytes is the most a stored handoff keeps. mcpinit renders at
// most its own handoffByteCap of it; a test holds the store cap at or above that.
const MaxHandoffContentBytes = 8 << 10

// SaveHandoff writes one handoff for (project, session), replacing any earlier
// one from the same session: a second pre-compact in one session is the newer
// state, and the older state is not what the next session should be handed.
func (s *Store) SaveHandoff(ctx context.Context, h SessionHandoff) error {
	if h.ProjectID == "" || h.SessionID == "" {
		return fmt.Errorf("save handoff: a project and a session are required")
	}
	if h.Content == "" {
		return fmt.Errorf("save handoff: no content")
	}
	if len(h.Content) > MaxHandoffContentBytes {
		h.Content = TruncateUTF8(h.Content, MaxHandoffContentBytes)
	}
	now := time.Now().UTC()
	expires := now.Add(SessionTTL).Format(StoredStampLayout)
	nowStamp := now.Format(StoredStampLayout)

	s.mu.Lock()
	defer s.mu.Unlock()
	tx, lock, err := s.beginWrite(ctx, "save-handoff")
	if err != nil {
		return fmt.Errorf("save handoff: %w", err)
	}
	// Expiry is a deletion, not only a filter on reads: without this a
	// session-retention row of up to MaxHandoffContentBytes would sit in the
	// store forever. The sweep rides the write that creates the next handoff,
	// which is the only moment this table is touched.
	if _, err := tx.ExecContext(ctx, `DELETE FROM session_handoff WHERE expires_at <= ?`, nowStamp); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("sweep expired handoffs: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO session_handoff (project_id, session_id, transcript_path, transcript_position, content, expires_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(project_id, session_id) DO UPDATE SET
			transcript_path = excluded.transcript_path,
			transcript_position = excluded.transcript_position,
			content = excluded.content,
			created_at = datetime('now'),
			expires_at = excluded.expires_at
	`, h.ProjectID, h.SessionID, h.TranscriptPath, h.TranscriptPosition, h.Content, expires); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("save handoff: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("save handoff: %w", err)
	}
	lock.reportHold("save-handoff", time.Now())
	return nil
}

// HandoffForSession returns the unexpired handoff this exact session left, or
// false when it has none. A compacting session is handed only its own working
// state: two sessions on one project must not hand each other's task and open
// questions across a compaction.
func (s *Store) HandoffForSession(ctx context.Context, projectID, sessionID string, now time.Time) (SessionHandoff, bool, error) {
	if projectID == "" || sessionID == "" {
		return SessionHandoff{}, false, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var h SessionHandoff
	var expires string
	err := s.queryDB().QueryRowContext(ctx, `
		SELECT project_id, session_id, transcript_path, transcript_position, content, created_at, expires_at
		FROM session_handoff
		WHERE project_id = ? AND session_id = ? AND expires_at > ?
	`, projectID, sessionID, now.UTC().Format(StoredStampLayout)).Scan(
		&h.ProjectID, &h.SessionID, &h.TranscriptPath, &h.TranscriptPosition, &h.Content, &h.CreatedAt, &expires)
	if err == sql.ErrNoRows {
		return SessionHandoff{}, false, nil
	}
	if err != nil {
		return SessionHandoff{}, false, fmt.Errorf("handoff for session: %w", err)
	}
	h.ExpiresAt = &expires
	return h, true, nil
}
