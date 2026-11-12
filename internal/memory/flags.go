package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	FlagKindWrong = "wrong"
	FlagKindStale = "stale"
)

// FlagReasonMax bounds the reason in RUNES, not bytes.
const FlagReasonMax = 500

var (
	ErrFlagKind = errors.New("flag a memory: kind must be \"wrong\" or \"stale\"")

	ErrFlagReason = fmt.Errorf("flag a memory: reason is required and at most %d characters", FlagReasonMax)

	ErrFlagNoMemory = errors.New("flag a memory: no memory with that id")

	ErrFlagWrongProject = errors.New("flag a memory: the memory belongs to another project")
)

// FlagMemoryRequest is one agent's flag on one memory.
type FlagMemoryRequest struct {
	ProjectID string
	MemoryID  string
	Kind      string
	Reason    string
	Agent     string
	SessionID string
}

func (s *Store) FlagMemory(ctx context.Context, req FlagMemoryRequest) error {
	if req.Kind != FlagKindWrong && req.Kind != FlagKindStale {
		return fmt.Errorf("%w: got %s", ErrFlagKind, RefusalArg("kind", req.Kind))
	}
	if strings.TrimSpace(req.Reason) == "" {
		return fmt.Errorf("%w: a flag with no reason is an unexplained objection, and the reason is what an "+
			"operator reads back later", ErrFlagReason)
	}
	if utf8.RuneCountInString(req.Reason) > FlagReasonMax {
		return fmt.Errorf("%w: reason is %d characters, the bound is %d", ErrFlagReason,
			utf8.RuneCountInString(req.Reason), FlagReasonMax)
	}

	if err := rejectSecret("reason", req.Reason); err != nil {
		return err
	}
	if strings.TrimSpace(req.ProjectID) == "" {
		return fmt.Errorf("%w: a flag with no project to attribute it to would be counted for every project",
			ErrFlagWrongProject)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, _, err := s.beginWrite(ctx, "flag-memory")
	if err != nil {
		return fmt.Errorf("flag memory: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	var owner, content string
	err = tx.QueryRowContext(ctx,
		`SELECT project_id, content FROM memories WHERE id = ?`, req.MemoryID,
	).Scan(&owner, &content)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrFlagNoMemory, RefusalArg("memory_id", req.MemoryID))
	}
	if err != nil {
		return fmt.Errorf("flag memory: read the memory: %w", err)
	}
	if owner != req.ProjectID {
		return fmt.Errorf("%w: %s", ErrFlagWrongProject, RefusalArg("memory_id", req.MemoryID))
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO memory_flags (project_id, memory_id, kind, reason, content_hash, agent, session_id)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		req.ProjectID, req.MemoryID, req.Kind, req.Reason, ContentHash(content), req.Agent, req.SessionID,
	); err != nil {
		return fmt.Errorf("flag memory: %w", err)
	}
	return tx.Commit()
}
