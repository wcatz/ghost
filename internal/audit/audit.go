// Package audit provides structured audit logging for Ghost operations.
// Every API call, memory mutation, and MCP request is recorded in SQLite.
package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// Action constants for audit events.
const (
	ActionMemoryCreate  = "memory.create"
	ActionMemoryUpdate  = "memory.update"
	ActionMemoryDelete  = "memory.delete"
	ActionMemorySearch  = "memory.search"
	ActionReflection    = "reflection"
	ActionChat          = "chat"
	ActionToolUse       = "tool.use"
	ActionMCPCall       = "mcp.call"
	ActionHTTPRequest   = "http.request"
	ActionEmbedding     = "embedding"
)

// Entry represents a single audit log entry.
type Entry struct {
	Action     string
	ProjectID  string
	User       string
	Details    map[string]interface{}
	Tokens     int
	CostUSD    float64
	DurationMs int64
}

// Logger writes audit entries to SQLite.
type Logger struct {
	db *sql.DB
}

// NewLogger creates an audit logger using the given database connection.
func NewLogger(db *sql.DB) *Logger {
	return &Logger{db: db}
}

// Log records an audit entry.
func (l *Logger) Log(ctx context.Context, e Entry) error {
	details, _ := json.Marshal(e.Details)
	if details == nil {
		details = []byte("{}")
	}

	_, err := l.db.ExecContext(ctx, `
		INSERT INTO audit_log (action, project_id, user, details, tokens, cost_usd, duration_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, e.Action, e.ProjectID, e.User, string(details), e.Tokens, e.CostUSD, e.DurationMs)
	if err != nil {
		return fmt.Errorf("audit log: %w", err)
	}
	return nil
}

// LogAsync records an audit entry without blocking. Errors are silently dropped.
func (l *Logger) LogAsync(e Entry) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = l.Log(ctx, e)
	}()
}

// Query returns recent audit entries, optionally filtered by project.
func (l *Logger) Query(ctx context.Context, projectID string, limit int) ([]StoredEntry, error) {
	var rows *sql.Rows
	var err error

	if projectID != "" {
		rows, err = l.db.QueryContext(ctx, `
			SELECT id, timestamp, action, project_id, user, details, tokens, cost_usd, duration_ms
			FROM audit_log
			WHERE project_id = ?
			ORDER BY timestamp DESC
			LIMIT ?
		`, projectID, limit)
	} else {
		rows, err = l.db.QueryContext(ctx, `
			SELECT id, timestamp, action, project_id, user, details, tokens, cost_usd, duration_ms
			FROM audit_log
			ORDER BY timestamp DESC
			LIMIT ?
		`, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("query audit log: %w", err)
	}
	defer rows.Close()

	var entries []StoredEntry
	for rows.Next() {
		var e StoredEntry
		var projectIDNull sql.NullString
		var detailsJSON string
		if err := rows.Scan(&e.ID, &e.Timestamp, &e.Action, &projectIDNull, &e.User, &detailsJSON, &e.Tokens, &e.CostUSD, &e.DurationMs); err != nil {
			return nil, err
		}
		if projectIDNull.Valid {
			e.ProjectID = projectIDNull.String
		}
		if err := json.Unmarshal([]byte(detailsJSON), &e.Details); err != nil {
			e.Details = map[string]interface{}{"raw": detailsJSON}
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// StoredEntry is an audit entry as stored in the database.
type StoredEntry struct {
	ID         string                 `json:"id"`
	Timestamp  string                 `json:"timestamp"`
	Action     string                 `json:"action"`
	ProjectID  string                 `json:"project_id"`
	User       string                 `json:"user"`
	Details    map[string]interface{} `json:"details"`
	Tokens     int                    `json:"tokens"`
	CostUSD    float64                `json:"cost_usd"`
	DurationMs int64                  `json:"duration_ms"`
}
