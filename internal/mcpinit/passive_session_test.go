package mcpinit

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mcpinitSource returns the text of every non-test .go file in internal/mcpinit.
func mcpinitSource(t *testing.T) map[string]string {
	t.Helper()
	entries, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	out := make(map[string]string, len(entries))
	for _, name := range entries {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		out[name] = string(b)
	}
	if len(out) == 0 {
		t.Fatal("no non-test source found; the glob is looking in the wrong directory")
	}
	return out
}

// TestAFailedSessionStartReadIsReportedNotSilent is the regression test for a
// finding this PR's review raised, and it is the half of the finding that is
// reachable from here.
func TestAFailedSessionStartReadIsReportedNotSilent(t *testing.T) {
	db, _ := openFileTestDB(t)
	store := sessionStore(db)
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	var got bytes.Buffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&got, nil)))
	t.Cleanup(func() { slog.SetDefault(restore) })

	// nil sink: this test asserts on the block a FAILED read produces, and a record
	// is not written for an errored Run in the first place. See passive_record_test.go
	// for the recording path.
	memories, globals, _ := loadSessionPassive(context.Background(), store, mustHookConfig(t), "psomeproj", time.Now(), nil, "")

	if len(memories) != 0 || len(globals) != 0 {
		t.Errorf("a failed read returned %d project and %d global rows, want none — the block is "+
			"rendered from what the read found, and a broken store found nothing", len(memories), len(globals))
	}
	if !strings.Contains(got.String(), "assembly failed") {
		t.Errorf("the session-start assembly failed and nothing said so; the log held:\n%s", got.String())
	}
	// The level, not just the record. This is the assertion a Debug-logger fix
	// fails, and it is here because the failure is silent for a reason that has
	// nothing to do with the message: the process default handler runs at Info, so
	// a Debug record is dropped before it is ever formatted.
	if strings.Contains(got.String(), "level=DEBUG") {
		t.Errorf("a session-start diagnosis was logged at DEBUG, which the default handler drops: %s", got.String())
	}
}

func funcBody(t *testing.T, source, name string) string {
	t.Helper()
	lines := strings.Split(source, "\n")
	start := -1
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimRight(l, "\r"), "func "+name+"(") {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("no top-level func %s in the source read", name)
	}
	for j := start + 1; j < len(lines); j++ {
		if strings.TrimRight(lines[j], "\r") == "}" {
			return strings.Join(lines[start:j+1], "\n")
		}
	}
	t.Fatalf("func %s is not closed in the source read", name)
	return ""
}
