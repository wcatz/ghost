package mcpinit

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/wcatz/ghost/internal/config"
	_ "modernc.org/sqlite"
)

type sessionStartInput struct {
	CWD    string `json:"cwd"`
	Source string `json:"source"`
}

// HandleSessionStartHook is invoked by Claude Code at session start via:
//
//	ghost hook session-start
//
// Its stdout becomes visible in Claude's context as a system-reminder.
// It automatically loads project context from the ghost DB based on cwd.
func HandleSessionStartHook(stdin io.Reader, stdout io.Writer) {
	data, _ := io.ReadAll(stdin)

	var input sessionStartInput
	_ = json.Unmarshal(data, &input)

	cwd := input.CWD
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	// Resolve symlinks so cwd matches the canonical path stored in the DB.
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = resolved
	}

	project, memories, learned, tasks, decisions, interactionCount := loadSessionContext(cwd)
	if project == "" {
		// No matching project — tell Claude context is available via tools
		_, _ = fmt.Fprintln(stdout, "Ghost memory is active but no project matched this directory.")
		_, _ = fmt.Fprintln(stdout, "Save discoveries with ghost_memory_save during work.")
		return
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "## Ghost context: %s\n", project)
	fmt.Fprintf(&sb, "Use project_id: \"%s\" for all ghost_* tool calls.\n\n", project)

	if learned != "" {
		fmt.Fprintf(&sb, "**Summary:** %s\n\n", learned)
	}

	if len(memories) > 0 {
		fmt.Fprintf(&sb, "**Memories (%d shown):**\n", len(memories))
		for _, m := range memories {
			fmt.Fprintf(&sb, "- [%s] `%s` %s\n", m[1], shortID(m[0]), m[2])
		}
	}

	if len(tasks) > 0 {
		fmt.Fprintf(&sb, "\n**Open Tasks:**\n")
		for _, t := range tasks {
			fmt.Fprintf(&sb, "- [%s] `%s` %s\n", t[1], t[0], t[2])
			if t[3] != "" {
				fmt.Fprintf(&sb, "  %s\n", t[3])
			}
		}
	}

	if len(decisions) > 0 {
		fmt.Fprintf(&sb, "\n**Recent Decisions:**\n")
		for _, d := range decisions {
			fmt.Fprintf(&sb, "- **%s**: %s\n", d[0], d[1])
		}
	}

	if dataDir, err2 := config.DataDir(); err2 == nil {
		if globals := loadGlobalMemories(filepath.Join(dataDir, "ghost.db")); len(globals) > 0 {
			fmt.Fprintf(&sb, "\n**Global (applies to all projects):**\n")
			for _, m := range globals {
				fmt.Fprintf(&sb, "- [%s] %s\n", m[0], m[1])
			}
		}
	}

	if interactionCount > 0 {
		fmt.Fprintf(&sb, "\n**Session #%d** with this project.\n", interactionCount)
	}

	fmt.Fprintf(&sb, "\nSave new discoveries with ghost_memory_save during work.")
	_, _ = fmt.Fprintln(stdout, sb.String())
}

func loadGlobalMemories(dbPath string) [][2]string {
	db, err := sql.Open("sqlite", dbPath+"?mode=ro&_pragma=journal_mode(WAL)&_pragma=busy_timeout(1000)")
	if err != nil {
		return nil
	}
	defer db.Close() //nolint:errcheck

	rows, err := db.Query(`
		SELECT category, content FROM memories
		WHERE project_id = '_global'
		ORDER BY pinned DESC, importance DESC, updated_at DESC
		LIMIT 15
	`)
	if err != nil {
		return nil
	}
	defer rows.Close() //nolint:errcheck

	var out [][2]string
	for rows.Next() {
		var cat, content string
		if err := rows.Scan(&cat, &content); err != nil {
			continue
		}
		content = truncateUTF8(content, 300)
		out = append(out, [2]string{cat, content})
	}
	return out
}

func loadSessionContext(cwd string) (project string, memories [][3]string, learned string, tasks [][4]string, decisions [][2]string, interactionCount int) {
	dataDir, err := config.DataDir()
	if err != nil {
		return
	}
	dbPath := filepath.Join(dataDir, "ghost.db")
	db, err := sql.Open("sqlite", dbPath+"?mode=ro&_pragma=journal_mode(WAL)&_pragma=busy_timeout(1000)")
	if err != nil {
		return
	}
	defer db.Close() //nolint:errcheck

	// Find matching project: try full path prefix first, then cwd basename name match
	cwdBase := filepath.Base(cwd)
	var projectID string
	row := db.QueryRow(`
		SELECT id, name FROM projects
		WHERE (? LIKE path || '%' AND LENGTH(path) > 10)
		   OR name = ?
		ORDER BY LENGTH(path) DESC LIMIT 1
	`, cwd, cwdBase)
	if err := row.Scan(&projectID, &project); err != nil {
		return
	}

	// Get learned context summary
	_ = db.QueryRow(
		`SELECT learned_context FROM ghost_state WHERE project_id = ?`, projectID,
	).Scan(&learned)

	// Get top memories: pinned first, then by importance
	rows, err := db.Query(`
		SELECT id, category, content FROM memories
		WHERE project_id = ?
		ORDER BY pinned DESC, importance DESC, updated_at DESC
		LIMIT 25
	`, projectID)
	if err != nil {
		return
	}
	defer rows.Close() //nolint:errcheck

	for rows.Next() {
		var id, cat, content string
		if err := rows.Scan(&id, &cat, &content); err != nil {
			continue
		}
		content = truncateUTF8(content, 300)
		memories = append(memories, [3]string{id, cat, content})
	}

	// Get open tasks
	taskRows, err := db.Query(`
		SELECT id, status, priority, title, COALESCE(description, '')
		FROM tasks
		WHERE project_id = ? AND status IN ('pending', 'active', 'blocked')
		ORDER BY priority ASC, created_at DESC
		LIMIT 10
	`, projectID)
	if err == nil {
		defer taskRows.Close() //nolint:errcheck
		for taskRows.Next() {
			var id, status, title, desc string
			var priority int
			if err := taskRows.Scan(&id, &status, &priority, &title, &desc); err != nil {
				continue
			}
			label := fmt.Sprintf("P%d %s", priority, title)
			tasks = append(tasks, [4]string{shortID(id), status, label, truncateUTF8(desc, 200)})
		}
	}

	// Get active decisions
	decRows, err := db.Query(`
		SELECT title, decision FROM decisions
		WHERE project_id = ? AND status = 'active'
		ORDER BY created_at DESC
		LIMIT 5
	`, projectID)
	if err == nil {
		defer decRows.Close() //nolint:errcheck
		for decRows.Next() {
			var title, decision string
			if err := decRows.Scan(&title, &decision); err != nil {
				continue
			}
			decisions = append(decisions, [2]string{title, truncateUTF8(decision, 200)})
		}
	}

	// Get interaction count
	_ = db.QueryRow(
		`SELECT interaction_count FROM ghost_state WHERE project_id = ?`, projectID,
	).Scan(&interactionCount)

	return
}

// shortID returns the first 8 characters of an ID, or the full ID if shorter.
func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// truncateUTF8 truncates s to at most maxBytes bytes without breaking
// multi-byte UTF-8 characters, appending "…" if truncated.
func truncateUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	// Walk backward from maxBytes to find a valid rune boundary.
	for maxBytes > 0 && !utf8.RuneStart(s[maxBytes]) {
		maxBytes--
	}
	return s[:maxBytes] + "…"
}
