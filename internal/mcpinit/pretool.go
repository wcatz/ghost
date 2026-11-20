package mcpinit

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/hostevent"
	"github.com/wcatz/ghost/internal/memory"
)

type preToolUseInput struct {
	SessionID string `json:"session_id"`
	CWD       string `json:"cwd"`
	AgentID   string `json:"agent_id"`
	ToolName  string `json:"tool_name"`
	ToolInput struct {
		Command string `json:"command"`
	} `json:"tool_input"`
}

// preToolTrigger names a command shape and the rule-text word groups that make
// a rule relevant to it. A rule matches when every word of at least one group
// appears in its text as a whole word, so a short word such as "pr" or "go"
// never matches inside "production" or "google", and a generic word alone
// never selects a rule unless it is itself the group.
type preToolTrigger struct {
	match  func(cmd string) bool
	groups [][]string
}

var (
	preToolHeavyGroups  = [][]string{{"heavy"}, {"laptop", "work"}}
	preToolAssignGroups = [][]string{{"assign"}, {"assigned"}, {"assigning"}, {"assignee"}, {"assignees"}}
)

var preToolMutatingVerbs = []string{"patch", "edit", "apply", "replace", "delete", "scale"}

func preToolGroups(parts ...[][]string) [][]string {
	var out [][]string
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func preToolTriggers() []preToolTrigger {
	var triggers []preToolTrigger
	add := func(match func(string) bool, groups ...[][]string) {
		triggers = append(triggers, preToolTrigger{match: match, groups: preToolGroups(groups...)})
	}
	contains := func(sub string) func(string) bool {
		return func(c string) bool { return strings.Contains(c, sub) }
	}
	word := func(w string) func(string) bool {
		return func(c string) bool { return hasCommandWord(c, w) }
	}
	sub := func(words ...string) func(string) bool {
		return func(c string) bool { return hasSubcommand(c, words...) }
	}
	add(sub("gh", "issue", "create"), [][]string{{"gh", "issue"}})
	add(sub("gh", "issue", "comment"), [][]string{{"gh", "issue"}})
	add(sub("gh", "pr", "create"), [][]string{{"gh", "pr"}})
	add(func(c string) bool { return hasCommandWord(c, "gh") && hasAssigneeFlag(c) }, preToolAssignGroups)
	add(word("kubectl"), [][]string{{"kubectl"}})
	add(func(c string) bool { return hasCommandWord(c, "kubectl") && mutatingVerb(c) != "" },
		[][]string{{"live", "cluster"}, {"cluster", "objects"}})
	add(word("helmfile"), [][]string{{"helmfile"}})
	add(word("helm"), [][]string{{"helm"}})
	add(sub("git", "push"), [][]string{{"git", "push"}})
	add(sub("git", "tag"), [][]string{{"git", "tag"}, {"release", "tag"}})
	add(contains("go test"), [][]string{{"go", "test"}, {"full", "test"}}, preToolHeavyGroups)
	add(contains("go build"), [][]string{{"go", "build"}}, preToolHeavyGroups)
	add(word("make"), [][]string{{"make"}}, preToolHeavyGroups)
	add(contains("npm test"), [][]string{{"npm", "test"}}, preToolHeavyGroups)
	add(contains("npm run build"), [][]string{{"npm", "build"}}, preToolHeavyGroups)
	add(contains("cargo build"), [][]string{{"cargo", "build"}}, preToolHeavyGroups)
	add(contains("cargo test"), [][]string{{"cargo", "test"}}, preToolHeavyGroups)
	add(word("pytest"), [][]string{{"pytest"}}, preToolHeavyGroups)
	add(func(c string) bool { return hasCommandWord(c, "golangci-lint") || hasCommandWord(c, "nilaway") },
		[][]string{{"golangci"}, {"nilaway"}, {"lint"}}, preToolHeavyGroups)
	add(func(c string) bool { return hasCommandWord(c, "-race") }, [][]string{{"race"}}, preToolHeavyGroups)
	return triggers
}

// hasSubcommand reports whether cmd runs words[0] followed by the rest as its
// subcommand words, allowing global flags between them, so `git -C dir push`
// and `gh -R repo pr create` match. A flag that takes a separate value (-C, -c,
// -r, --repo) has that value skipped. The command arrives lowercased, so the
// -C and -R flags are compared as -c and -r. Quoting is not parsed, so a quoted
// argument that spells the words can still match; that only adds a relevant
// rule reminder.
func hasSubcommand(cmd string, words ...string) bool {
	fields := strings.Fields(cmd)
	for i, f := range fields {
		if f != words[0] {
			continue
		}
		j, k := i+1, 1
		for j < len(fields) && k < len(words) {
			switch f := fields[j]; {
			case f == "-c" || f == "-r" || f == "--repo":
				j += 2
			case strings.HasPrefix(f, "-"):
				j++
			case f == words[k]:
				j++
				k++
			default:
				j = len(fields)
			}
		}
		if k == len(words) {
			return true
		}
	}
	return false
}

func hasCommandWord(cmd, word string) bool {
	for _, f := range strings.Fields(cmd) {
		if f == word {
			return true
		}
	}
	return false
}

func hasAssigneeFlag(cmd string) bool {
	for _, f := range strings.Fields(cmd) {
		if f == "-a" {
			return true
		}
		for _, flag := range []string{"--assignee", "--add-assignee", "--remove-assignee"} {
			if f == flag || strings.HasPrefix(f, flag+"=") {
				return true
			}
		}
	}
	return false
}

func mutatingVerb(cmd string) string {
	for _, f := range strings.Fields(cmd) {
		for _, v := range preToolMutatingVerbs {
			if f == v {
				return v
			}
		}
	}
	return ""
}

// preToolQuery is what a command asks for: the words that retrieve candidate
// rules and the groups a candidate's text must satisfy.
type preToolQuery struct {
	terms  []string
	groups [][]string
}

func preToolUseQuery(cmd string) (preToolQuery, bool) {
	c := strings.ToLower(strings.TrimSpace(cmd))
	var q preToolQuery
	if c == "" {
		return q, false
	}
	seen := map[string]bool{}
	for _, t := range preToolTriggers() {
		if !t.match(c) {
			continue
		}
		for _, g := range t.groups {
			q.groups = append(q.groups, g)
			for _, w := range g {
				if !seen[w] {
					seen[w] = true
					q.terms = append(q.terms, w)
				}
			}
		}
	}
	if v := mutatingVerb(c); v != "" && hasCommandWord(c, "kubectl") {
		q.groups = append(q.groups, []string{v})
		if !seen[v] {
			q.terms = append(q.terms, v)
		}
	}
	return q, len(q.groups) > 0
}

func ruleTextWords(text string) map[string]bool {
	words := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		words[w] = true
	}
	return words
}

func ruleMatchesCommand(it assemble.Item, q preToolQuery) bool {
	words := ruleTextWords(it.Content)
	for _, g := range q.groups {
		all := len(g) > 0
		for _, w := range g {
			if !words[w] {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

const (
	preToolMaxRules = 4
	preToolMaxBytes = 1200
)

func runPreToolUse(p hostevent.Payload, stdout io.Writer) {
	var in preToolUseInput
	if err := json.Unmarshal(p.Raw, &in); err != nil {
		return
	}
	if in.AgentID != "" {
		return
	}
	pq, ok := preToolUseQuery(in.ToolInput.Command)
	if !ok {
		return
	}

	dataDir, err := config.DataDirPath()
	if err != nil {
		return
	}
	dbPath := filepath.Join(dataDir, "ghost.db")
	db, err := memory.OpenReadDB(dbPath)
	if err != nil {
		return
	}
	defer db.Close() //nolint:errcheck
	store := sessionStore(db)

	cwd := in.CWD
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = resolved
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	projectID, _ := resolveSessionProject(ctx, store, cwd)
	if projectID == "" {
		return
	}

	cfg, err := config.Load()
	if err != nil {
		cfg = config.FallbackConfig()
	}

	query := strings.Join(pq.terms, " ")
	now := time.Now().UTC()
	var ruleItems []assemble.Item
	seen := map[string]bool{}
	collect := func(req assemble.Request) {
		res, err := assemble.Run(ctx, store, req)
		if err != nil {
			return
		}
		for _, it := range res.Items {
			if seen[it.ID] || !isSessionRuleItem(it) || !ruleMatchesCommand(it, pq) {
				continue
			}
			seen[it.ID] = true
			ruleItems = append(ruleItems, it)
		}
	}
	collect(assemble.Request{
		ProjectID:       projectID,
		Query:           query,
		Source:          assemble.SourceSearch,
		Condition:       assemble.CondFTSOnly,
		Now:             now,
		Scope:           cfg.Injection.SessionScope,
		Budget:          assemble.Budget{MaxItems: workMomentWindow},
		RelevanceCutoff: cfg.Context.RelevanceCutoff,
		SessionID:       in.SessionID,
		Logger:          sessionLog(),
	})
	collect(assemble.Request{
		ProjectID:       memory.GlobalProjectID,
		Query:           query,
		Source:          assemble.SourceSearch,
		Condition:       assemble.CondFTSOnly,
		Now:             now,
		Scope:           cfg.Injection.SessionScope,
		Budget:          assemble.Budget{MaxItems: workMomentWindow},
		RelevanceCutoff: cfg.Context.RelevanceCutoff,
		SessionID:       in.SessionID,
		Logger:          sessionLog(),
	})
	if len(ruleItems) == 0 {
		return
	}
	if len(ruleItems) > preToolMaxRules {
		ruleItems = ruleItems[:preToolMaxRules]
	}

	var b strings.Builder
	b.WriteString("Relevant rules for this command:\n")
	for _, it := range ruleItems {
		line := renderPreToolRule(it)
		if b.Len()+len(line)+1 > preToolMaxBytes {
			break
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	text := b.String()
	if strings.TrimSpace(text) == "Relevant rules for this command:" {
		return
	}

	out, err := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName":     "PreToolUse",
		"additionalContext": text,
	}})
	if err != nil {
		return
	}
	_, _ = fmt.Fprintln(stdout, string(out))
}

func renderPreToolRule(it assemble.Item) string {
	content := truncateUTF8(it.Content, 240)
	content = strings.ReplaceAll(content, "\n", " ")
	content = strings.ReplaceAll(content, "\r", " ")
	if placeholder, withheld := memory.TokenGuardPlaceholder("content", content); withheld {
		content = placeholder
	}
	return "- " + content
}
