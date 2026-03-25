package mcpinit

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

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

	project, memories, learned := loadSessionContext(cwd)
	if project == "" {
		// No matching project — fall back to static reminder
		fmt.Fprintln(stdout, "Ghost memory is active. Before starting work:")
		fmt.Fprintln(stdout, "1. Call ghost_list_projects to discover known projects")
		fmt.Fprintln(stdout, "2. Call ghost_project_context with the project name")
		fmt.Fprintln(stdout, "3. Save discoveries with ghost_memory_save during work")
		return
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "## Ghost context: %s\n\n", project)

	if learned != "" {
		fmt.Fprintf(&sb, "**Summary:** %s\n\n", learned)
	}

	if len(memories) > 0 {
		fmt.Fprintf(&sb, "**Memories:**\n")
		for _, m := range memories {
			fmt.Fprintf(&sb, "- [%s] %s\n", m[0], m[1])
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

	fmt.Fprintf(&sb, "\nSave new discoveries with ghost_memory_save during work.")
	fmt.Fprintln(stdout, sb.String())
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
		if len(content) > 300 {
			content = content[:300] + "…"
		}
		out = append(out, [2]string{cat, content})
	}
	return out
}

func loadSessionContext(cwd string) (project string, memories [][2]string, learned string) {
	dataDir, err := config.DataDir()
	if err != nil {
		return
	}
	dbPath := filepath.Join(dataDir, "ghost.db")
	db, err := sql.Open("sqlite", dbPath+"?mode=ro&_pragma=journal_mode(WAL)&_pragma=busy_timeout(1000)")
	if err != nil {
		return
	}
	defer db.Close()

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
		SELECT category, content FROM memories
		WHERE project_id = ?
		ORDER BY pinned DESC, importance DESC, updated_at DESC
		LIMIT 25
	`, projectID)
	if err != nil {
		return
	}
	defer rows.Close()

	for rows.Next() {
		var cat, content string
		if err := rows.Scan(&cat, &content); err != nil {
			continue
		}
		// Truncate very long memories to keep the hook output manageable
		if len(content) > 300 {
			content = content[:300] + "…"
		}
		memories = append(memories, [2]string{cat, content})
	}
	return
}
