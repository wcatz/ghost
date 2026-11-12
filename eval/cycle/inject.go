package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wcatz/ghost/eval/cycle/corpus"
	"github.com/wcatz/ghost/internal/memory"
)

var savedIDRe = regexp.MustCompile(`Memory saved \(id:\s*([0-9a-fA-F]+)\)`)

type mcpSession struct {
	sess *mcp.ClientSession
}

func startMCP(ctx context.Context, ghostBin string, env []string) (*mcpSession, error) {
	cmd := exec.Command(ghostBin, "mcp")
	cmd.Env = env
	cmd.Stderr = os.Stderr
	client := mcp.NewClient(&mcp.Implementation{Name: "ghost-evalcycle", Version: "0.1.0"}, nil)
	sess, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return nil, fmt.Errorf("connect mcp: %w", err)
	}
	return &mcpSession{sess: sess}, nil
}

func (s *mcpSession) close() { _ = s.sess.Close() }

func textOf(res *mcp.CallToolResult) string {
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

func (s *mcpSession) callText(ctx context.Context, tool string, args map[string]any) (string, error) {
	res, err := s.sess.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		return "", err
	}
	if res.IsError {
		return "", fmt.Errorf("%s failed: %s", tool, textOf(res))
	}
	return textOf(res), nil
}

func saveAll(ctx context.Context, s *mcpSession, project string, entries []corpus.Entry) (map[string]string, error) {
	ids := make(map[string]string, len(entries))
	for _, e := range entries {
		out, err := s.callText(ctx, "ghost_memory_save", map[string]any{
			"project_id": project,
			"content":    e.Content,
			"category":   e.Category,
			"importance": e.Importance,
			"tags":       e.TagsOrEmpty(),
		})
		if err != nil {
			return nil, fmt.Errorf("save %s: %w", e.Key, err)
		}
		m := savedIDRe.FindStringSubmatch(out)
		if m == nil {
			return nil, fmt.Errorf("save %s: unrecognized response %q", e.Key, out)
		}
		if prev, ok := ids[e.Key]; ok && prev != m[1] {
			return nil, fmt.Errorf("save %s: id collision %q vs %q", e.Key, prev, m[1])
		}
		ids[e.Key] = m[1]
		time.Sleep(1100 * time.Millisecond)
	}
	return ids, nil
}

func waitForEmbeddings(ctx context.Context, ollamaURL, dbPath, projectName string, timeout time.Duration) error {
	if err := ollamaReachable(ollamaURL); err != nil {
		return fmt.Errorf("embedding drain needs Ollama: %w", err)
	}
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		return err
	}
	defer db.Close() //nolint:errcheck
	deadline := time.Now().Add(timeout)
	for {
		var total, done int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM memories m JOIN projects p ON m.project_id=p.id WHERE p.name=? OR p.id=?`,
			projectName, projectName).Scan(&total); err != nil {
			return fmt.Errorf("count memories: %w", err)
		}
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM memory_embeddings e JOIN memories m ON e.memory_id=m.id `+
				`JOIN projects p ON m.project_id=p.id WHERE p.name=? OR p.id=?`,
			projectName, projectName).Scan(&done); err != nil {
			return fmt.Errorf("count embeddings: %w", err)
		}
		if total > 0 && done == total {
			fmt.Printf("  embeddings drained: %d/%d\n", done, total)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("embedding drain timeout: %d/%d embedded after %s", done, total, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func ollamaReachable(url string) error {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url + "/api/tags")
	if err != nil {
		return fmt.Errorf("GET %s/api/tags: %w (is Ollama running?)", url, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ollama status %d", resp.StatusCode)
	}
	return nil
}

func restampChronology(dbPath, projectName string, entries []corpus.Entry, ids map[string]string) error {
	db, err := memory.OpenDB(dbPath)
	if err != nil {
		return err
	}
	defer db.Close() //nolint:errcheck
	base := time.Now().UTC().Add(-time.Duration(len(entries)) * time.Minute)
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`UPDATE memories SET created_at=?, updated_at=? WHERE id=?`)
	if err != nil {
		return err
	}
	defer stmt.Close() //nolint:errcheck
	for i, e := range entries {
		id, ok := ids[e.Key]
		if !ok {
			continue
		}
		ts := base.Add(time.Duration(i) * time.Minute).Format("2006-01-02 15:04:05")
		if _, err := stmt.Exec(ts, ts, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}
