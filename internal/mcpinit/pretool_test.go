package mcpinit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/hostevent"
	"github.com/wcatz/ghost/internal/memory"
)

func TestPreToolUseQuery(t *testing.T) {
	if _, ok := preToolUseQuery("echo hello"); ok {
		t.Fatal("unrelated command must not match")
	}
	q, ok := preToolUseQuery("kubectl apply -f k.yaml")
	if !ok || len(q.groups) == 0 {
		t.Fatal("kubectl command must match")
	}
	found := false
	for _, k := range q.terms {
		if k == "kubectl" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected kubectl term, got %v", q.terms)
	}
}

func TestRuleMatchesCommandUsesWholeWords(t *testing.T) {
	q, _ := preToolUseQuery("gh pr create --title x")
	for _, text := range []string{
		"production changes need review",
		"the project preference is tabs",
		"google algorithm stage notes",
	} {
		if ruleMatchesCommand(assemble.Item{Content: text}, q) {
			t.Fatalf("rule %q must not match gh pr create", text)
		}
	}
	if !ruleMatchesCommand(assemble.Item{Content: "gh pr titles use conventional commits"}, q) {
		t.Fatal("rule naming gh pr must match")
	}
}

// preToolRun seeds one project with the given standing rules and returns the
// hook output for command.
func preToolRun(t *testing.T, command string, rules ...string) string {
	t.Helper()
	xdg := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdg)
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", xdg)
	ghostDir := filepath.Join(xdg, "ghost")
	if err := os.MkdirAll(ghostDir, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := memory.OpenDB(filepath.Join(ghostDir, "ghost.db"))
	if err != nil {
		t.Fatal(err)
	}
	projDir := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	canonical, _ := filepath.EvalSymlinks(projDir)
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('p1', ?, 'proj')`, canonical); err != nil {
		t.Fatal(err)
	}
	for i, r := range rules {
		if _, err := db.Exec(`INSERT INTO memories (id, project_id, category, content, source, importance) VALUES (?, 'p1', 'preference', ?, 'manual', 0.9)`, fmt.Sprintf("r%d", i), r); err != nil {
			t.Fatal(err)
		}
	}
	_ = db.Close()
	payload, _ := json.Marshal(map[string]any{
		"hook_event_name": "PreToolUse",
		"session_id":      "s1",
		"cwd":             projDir,
		"tool_name":       "Bash",
		"tool_input":      map[string]any{"command": command},
	})
	var out bytes.Buffer
	runPreToolUse(hostevent.Payload{Raw: payload, HookEventName: "PreToolUse"}, &out)
	return out.String()
}

func TestRunPreToolUseSilentOnSubstringOnlyOverlap(t *testing.T) {
	got := preToolRun(t, "gh pr create --title x", "production deploys need a second pair of eyes")
	if got != "" {
		t.Fatalf("a rule with only 'production' must stay silent on gh pr create, got %q", got)
	}
}

func TestRunPreToolUseDeliversByArguments(t *testing.T) {
	cases := []struct {
		name, command, rule string
	}{
		{"assign team members", "gh issue create --title x --assignee someone", "never assign team members"},
		{"live cluster objects", "kubectl patch deploy web -p {}", "never patch, edit or apply live cluster objects"},
		{"heavy work on laptop", "go test -race ./...", "no heavy work on this laptop: no -race or full test suites"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := preToolRun(t, c.command, c.rule, "unrelated rule about tabs and spaces")
			if !strings.Contains(got, c.rule) {
				t.Fatalf("rule %q not delivered for %q: %q", c.rule, c.command, got)
			}
			if strings.Contains(got, "tabs and spaces") {
				t.Fatalf("unrelated rule delivered: %q", got)
			}
		})
	}
}

func TestRunPreToolUseSilentWhenNoMatch(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdg)
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", xdg)

	payload, _ := json.Marshal(map[string]any{
		"hook_event_name": "PreToolUse",
		"session_id":      "s1",
		"cwd":             t.TempDir(),
		"tool_name":       "Bash",
		"tool_input":      map[string]any{"command": "echo hello"},
	})
	var out bytes.Buffer
	runPreToolUse(hostevent.Payload{Raw: payload, HookEventName: "PreToolUse"}, &out)
	if out.Len() != 0 {
		t.Fatalf("expected no output, got %q", out.String())
	}
}

func TestRunPreToolUseInjectsMatchingRule(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdg)
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", xdg)

	ghostDir := filepath.Join(xdg, "ghost")
	if err := os.MkdirAll(ghostDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(ghostDir, "ghost.db")
	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	projDir := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	canonical, _ := filepath.EvalSymlinks(projDir)
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('p1', ?, 'proj')`, canonical); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO memories (id, project_id, category, content, source, importance) VALUES ('r1','p1','gotcha','never run kubectl apply without review','manual',0.9)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO memories (id, project_id, category, content, source, importance) VALUES ('f1','p1','fact','kubectl is installed','manual',0.9)`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	payload, _ := json.Marshal(map[string]any{
		"hook_event_name": "PreToolUse",
		"session_id":      "s1",
		"cwd":             projDir,
		"tool_name":       "Bash",
		"tool_input":      map[string]any{"command": "kubectl apply -f k.yaml"},
	})
	var out bytes.Buffer
	runPreToolUse(hostevent.Payload{Raw: payload, HookEventName: "PreToolUse"}, &out)
	got := out.String()
	if got == "" {
		t.Fatal("expected output for matching kubectl rule")
	}
	if !strings.Contains(got, "kubectl apply without review") {
		t.Fatalf("expected rule content in output: %s", got)
	}
	if strings.Contains(got, "kubectl is installed") {
		t.Fatalf("non-rule fact must not be injected: %s", got)
	}
	if strings.Contains(got, "permissionDecision") {
		t.Fatalf("must not emit permissionDecision: %s", got)
	}
	if !strings.Contains(got, "additionalContext") {
		t.Fatalf("expected additionalContext: %s", got)
	}
}

func TestRunPreToolUseSilentWhenNoMatchingRule(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdg)
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", xdg)

	ghostDir := filepath.Join(xdg, "ghost")
	if err := os.MkdirAll(ghostDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(ghostDir, "ghost.db")
	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	projDir := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	canonical, _ := filepath.EvalSymlinks(projDir)
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('p1', ?, 'proj')`, canonical); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO memories (id, project_id, category, content, source, importance) VALUES ('r1','p1','gotcha','never run helm upgrade blindly','manual',0.9)`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	payload, _ := json.Marshal(map[string]any{
		"hook_event_name": "PreToolUse",
		"session_id":      "s1",
		"cwd":             projDir,
		"tool_name":       "Bash",
		"tool_input":      map[string]any{"command": "kubectl apply -f k.yaml"},
	})
	var out bytes.Buffer
	runPreToolUse(hostevent.Payload{Raw: payload, HookEventName: "PreToolUse"}, &out)
	if out.Len() != 0 {
		t.Fatalf("expected no output when no rule text matches, got %q", out.String())
	}
}

func TestRunPreToolUseSilentOnPassingLaptopMention(t *testing.T) {
	for _, cmd := range []string{"go test ./...", "make build", "go test -race ./..."} {
		if got := preToolRun(t, cmd, "the dev machine is a laptop running Fedora"); got != "" {
			t.Fatalf("a rule that only mentions a laptop must stay silent on %q, got %q", cmd, got)
		}
	}
}

func TestRunPreToolUseDeliversLaptopWorkRule(t *testing.T) {
	rule := "do no laptop work like -race runs; use the remote host"
	if got := preToolRun(t, "go test -race ./...", rule); !strings.Contains(got, rule) {
		t.Fatalf("laptop+work rule not delivered: %q", got)
	}
}

func TestRunHostEvent_PreToolUseSilentOnNonClaudeHost(t *testing.T) {
	for _, source := range []string{"codex", "goose", "opencode"} {
		in := `{"contract":{"version":1,"source":"` + source + `"},"hook_event_name":"PreToolUse","session_id":"s","cwd":"/nowhere","tool_name":"Bash","tool_input":{"command":"git push origin main"}}`
		var out bytes.Buffer
		RunHostEvent("pre-tool-use", source, strings.NewReader(in), &out, io.Discard)
		if out.Len() != 0 {
			t.Errorf("--source %s must stay silent on pre-tool-use, got %q", source, out.String())
		}
	}
}

func TestSessionPassiveBudgetHonoursInjectionKnobs(t *testing.T) {
	cfg := &config.Config{Injection: config.DefaultInjectionConfig()}
	s := sessionPassiveBudget(cfg, "p1").Slices[0]
	if !s.TwoPass || s.BehaviorFloor < sessionRuleReserve || s.BehaviorFloor > sessionMemoriesCap {
		t.Fatalf("default floor must hold the rule reserve, got twoPass=%v floor=%d", s.TwoPass, s.BehaviorFloor)
	}
	if _, ok := s.CategoryCaps["gotcha"]; ok {
		t.Fatalf("a cap on a standing-rule category must not cut the reserve, got %v", s.CategoryCaps)
	}
	cfg.Injection.CategoryCaps = map[string]int{"gotcha": 4, "decision": 2}
	if s := sessionPassiveBudget(cfg, "p1").Slices[0]; s.CategoryCaps["decision"] != 2 {
		t.Fatalf("a cap on another behavior category must still apply, got %v", s.CategoryCaps)
	}
	cfg.Injection.BehaviorFloor = 0
	if s := sessionPassiveBudget(cfg, "p1").Slices[0]; s.TwoPass || s.BehaviorFloor != 0 {
		t.Fatalf("behavior_floor: 0 must switch the bias off, got twoPass=%v floor=%d", s.TwoPass, s.BehaviorFloor)
	}
	cfg.Injection.BehaviorFloor = 1000
	cfg.Injection.BehaviorCategories = []string{"decision"}
	cfg.Injection.CategoryCaps = nil
	s = sessionPassiveBudget(cfg, "p1").Slices[0]
	if s.BehaviorFloor != sessionMemoriesCap {
		t.Fatalf("floor must stay under the slice cap, got %d", s.BehaviorFloor)
	}
	if got := strings.Join(s.BehaviorCategories, ","); got != "gotcha,convention,preference" {
		t.Fatalf("session start keeps its fixed rule categories, got %s", got)
	}
}

func TestPreToolUseQueryMatchesGlobalFlags(t *testing.T) {
	for _, cmd := range []string{
		"git -C /some/dir push origin x",
		"gh -R owner/repo pr create --title x",
		"gh --repo owner/repo issue create",
		"git push",
	} {
		if _, ok := preToolUseQuery(cmd); !ok {
			t.Errorf("%q must match a trigger", cmd)
		}
	}
	for _, cmd := range []string{"git status", "git -C dir log", "gh pr view 3", "echo pushed"} {
		if _, ok := preToolUseQuery(cmd); ok {
			t.Errorf("%q must not match", cmd)
		}
	}
}
