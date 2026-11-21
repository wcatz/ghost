// Command evalstandingrules replays recorded Claude Code session logs against a
// Ghost store to measure standing-rule handling. It reads only the two paths it
// is given: a directory of session logs and a path to a store.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/mcpinit"
	"github.com/wcatz/ghost/internal/memory"
)

type userTurn struct {
	session string
	at      time.Time
	text    string
	order   int
}

type missingRow struct {
	At      time.Time
	Session string
	Repeat  string
	Rule    string
}

type flaggedRow struct {
	At      time.Time
	Session string
	Text    string
}

type report struct {
	UserTurns   int
	Flagged     int
	Repeats     int
	Missing     int
	Unmatched   int
	Undated     int
	Project     string
	ListFlagged bool
	FlaggedRows []flaggedRow
	MissingRows []missingRow
}

type ruleStore interface {
	MemoriesAsOfAll(ctx context.Context, t time.Time) (*memory.AsOfSet, error)
	MemoriesAsOf(ctx context.Context, projectID string, t time.Time) (*memory.AsOfSet, error)
	GetByIDs(ctx context.Context, ids []string) ([]memory.Memory, error)
}

var (
	systemReminderRE   = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`)
	wordRE             = regexp.MustCompile(`[a-z][a-z0-9_-]{2,}`)
	repeatCorrectionRE = regexp.MustCompile(`(?i)(?:\bi\s+(?:already\s+)?(?:told|said|mentioned|mention)\b|\bwe\s+(?:went\s+over|discussed|covered|talked\s+about)\b|\blike\s+i\s+said\b|\bas\s+i\s+said\b|\bi\s+thought\s+i\s+(?:said|told)\b|\bdidn'?t\s+i\s+(?:say|tell)\b|\byou\s+keep\b|\bi\s+keep\s+(?:saying|telling)\b|\bi'?ve\s+(?:said|told|mentioned)\b|\bhow\s+many\s+times\b)`)
)

// evalStopWords holds only this tool's own function words. The generic
// development words come from the single list the working-moment floor uses,
// through mcpinit.IsGenericWord, so the two cannot drift apart again.
var evalStopWords = map[string]bool{
	"the": true, "and": true, "for": true, "that": true, "this": true, "with": true,
	"you": true, "are": true, "was": true, "were": true, "not": true, "how": true,
	"did": true, "does": true, "have": true, "has": true, "had": true, "but": true,
	"all": true, "our": true, "out": true, "get": true, "got": true, "from": true,
	"into": true, "they": true, "them": true, "then": true, "than": true, "just": true,
	"use": true, "using": true,
}

func main() {
	logs, storePath, project, listFlagged, err := parseArgs(os.Args[1:], os.Stderr)
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			os.Exit(2)
		}
		return
	}
	if err := run(logs, storePath, project, listFlagged, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func parseArgs(args []string, stderr io.Writer) (logs, storePath, project string, listFlagged bool, err error) {
	fs := flag.NewFlagSet("evalstandingrules", flag.ContinueOnError)
	fs.SetOutput(stderr)
	projectFlag := fs.String("project", "", "count rules only in this project id (and _global); empty checks every project")
	listFlag := fs.Bool("list", false, "print every user message the detector flagged")
	fs.Usage = func() {
		_, _ = fmt.Fprintln(stderr, "usage: evalstandingrules [-list] [-project id] <session-log-dir> <ghost-store>")
	}
	if err := fs.Parse(args); err != nil {
		return "", "", "", false, err
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return "", "", "", false, errors.New("expected a session-log directory and a store path")
	}
	return fs.Arg(0), fs.Arg(1), *projectFlag, *listFlag, nil
}

func run(logs, storePath, project string, listFlagged bool, out io.Writer) error {
	turns, err := readTurns(logs)
	if err != nil {
		return err
	}
	store, closeStore, err := openStore(storePath)
	if err != nil {
		return err
	}
	defer closeStore()
	result, err := analyze(turns, store, project, listFlagged)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "Session logs: %s\nStore: %s\n\n", logs, storePath); err != nil {
		return err
	}
	return result.writeTo(out)
}

// openStore opens the given store twice through the read-only handle, the same
// shape the recorded-query replay uses. No migration or write is performed.
func openStore(path string) (*memory.Store, func(), error) {
	db, err := memory.OpenReadDB(path)
	if err != nil {
		return nil, nil, err
	}
	readDB, err := memory.OpenReadDB(path)
	if err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	closeFn := func() {
		_ = readDB.Close()
		_ = db.Close()
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return memory.NewStoreWithRead(db, readDB, logger), closeFn, nil
}

func readTurns(dir string) ([]userTurn, error) {
	paths, err := jsonlFiles(dir)
	if err != nil {
		return nil, err
	}
	var turns []userTurn
	order := 0
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
		for scanner.Scan() {
			raw := strings.TrimSpace(scanner.Text())
			if raw == "" {
				continue
			}
			var item struct {
				Type      string          `json:"type"`
				Timestamp string          `json:"timestamp"`
				SessionID string          `json:"sessionId"`
				IsMeta    bool            `json:"isMeta"`
				Message   json.RawMessage `json:"message"`
			}
			if err := json.Unmarshal([]byte(raw), &item); err != nil {
				continue
			}
			if item.Type != "user" || item.IsMeta {
				continue
			}
			var message struct {
				Content json.RawMessage `json:"content"`
			}
			if err := json.Unmarshal(item.Message, &message); err != nil {
				continue
			}
			text := textFromContent(message.Content)
			if text == "" {
				continue
			}
			var at time.Time
			if item.Timestamp != "" {
				at, _ = time.Parse(time.RFC3339, item.Timestamp)
			}
			turns = append(turns, userTurn{session: item.SessionID, at: at, text: text, order: order})
			order++
		}
		if err := scanner.Err(); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		if err := f.Close(); err != nil {
			return nil, err
		}
	}
	return turns, nil
}

func jsonlFiles(dir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.EqualFold(filepath.Ext(path), ".jsonl") {
			out = append(out, path)
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}

func textFromContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return cleanText(s)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var b strings.Builder
	for _, block := range blocks {
		if block.Type == "text" && block.Text != "" {
			b.WriteString(block.Text)
			b.WriteString("\n")
		}
	}
	return cleanText(b.String())
}

func cleanText(s string) string {
	return strings.TrimSpace(systemReminderRE.ReplaceAllString(s, ""))
}

func isRepeatCorrection(text string) bool {
	return repeatCorrectionRE.MatchString(text)
}

func words(s string) map[string]bool {
	out := make(map[string]bool)
	for _, w := range wordRE.FindAllString(strings.ToLower(s), -1) {
		if !evalStopWords[w] && !mcpinit.IsGenericWord(w) {
			out[w] = true
		}
	}
	return out
}

// findOriginalRule finds the earlier standing-rule statement a repeat refers
// to. A repeat that cannot be tied to an earlier rule is not a measurement of
// "not saved in time", so it is left out of the count.
func findOriginalRule(turn userTurn, prior []userTurn) (userTurn, bool) {
	tw := words(turn.text)
	if len(tw) < 2 {
		return userTurn{}, false
	}
	best := userTurn{}
	score := 0
	for _, p := range prior {
		if !mcpinit.IsDirectivePrompt(p.text) {
			continue
		}
		pw := words(p.text)
		hits := 0
		for w := range tw {
			if pw[w] {
				hits++
			}
		}
		if hits >= 2 && hits >= score {
			best, score = p, hits
		}
	}
	return best, score >= 2
}

func ruleInGhost(rule string, contents []string) bool {
	rw := words(rule)
	if len(rw) < 2 {
		needle := strings.ToLower(strings.TrimSpace(rule))
		if needle == "" {
			return false
		}
		for _, c := range contents {
			if strings.Contains(strings.ToLower(c), needle) {
				return true
			}
		}
		return false
	}
	for _, c := range contents {
		cw := words(c)
		hits := 0
		for w := range rw {
			if cw[w] {
				hits++
			}
		}
		if hits >= 2 {
			return true
		}
	}
	return false
}

// contentsAsOf reads the rule text a store held at an instant. With a project
// the read is scoped to it (and _global); without one it spans every project,
// which can mistake an unrelated cross-project memory for the rule. A
// historical read gives recorded content for versioned rows; a row with no
// version at that instant is reported only by id (its state was never
// recorded), so its current text is read to answer "was the rule here yet".
// Reading the live text cannot invent a memory: a row deleted before the
// instant has no id in the set, and a versioned row keeps its historical text.
func contentsAsOf(ctx context.Context, store ruleStore, project string, at time.Time) ([]string, error) {
	var set *memory.AsOfSet
	var err error
	if project == "" {
		set, err = store.MemoriesAsOfAll(ctx, at)
	} else {
		set, err = store.MemoriesAsOf(ctx, project, at)
	}
	if err != nil || set == nil {
		return nil, err
	}
	out := make([]string, 0, len(set.Rows)+len(set.Unknown))
	for _, row := range set.Rows {
		out = append(out, row.Content)
	}
	ids := make([]string, 0, len(set.Unknown))
	for _, row := range set.Unknown {
		ids = append(ids, row.ID)
	}
	current, err := store.GetByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, row := range current {
		out = append(out, row.Content)
	}
	return out, nil
}

func analyze(turns []userTurn, store ruleStore, project string, listFlagged bool) (report, error) {
	result := report{UserTurns: len(turns), Project: project, ListFlagged: listFlagged}
	for _, turn := range turns {
		if mcpinit.IsDirectivePrompt(turn.text) {
			result.Flagged++
			result.FlaggedRows = append(result.FlaggedRows, flaggedRow{
				At: turn.at, Session: turn.session, Text: turn.text,
			})
		}
	}
	ordered := append([]userTurn(nil), turns...)
	sort.SliceStable(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if a.at.IsZero() != b.at.IsZero() {
			return !a.at.IsZero()
		}
		if a.at.IsZero() || a.at.Equal(b.at) {
			return a.order < b.order
		}
		return a.at.Before(b.at)
	})
	for i, turn := range ordered {
		if !isRepeatCorrection(turn.text) {
			continue
		}
		rule, ok := findOriginalRule(turn, ordered[:i])
		if !ok {
			result.Unmatched++
			continue
		}
		result.Repeats++
		if turn.at.IsZero() {
			result.Undated++
			continue
		}
		contents, err := contentsAsOf(context.Background(), store, project, turn.at)
		if err != nil {
			return result, err
		}
		if !ruleInGhost(rule.text, contents) {
			result.Missing++
			result.MissingRows = append(result.MissingRows, missingRow{
				At: turn.at, Session: turn.session, Repeat: turn.text, Rule: rule.text,
			})
		}
	}
	return result, nil
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	const max = 300
	runes := []rune(s)
	if len(runes) > max {
		return string(runes[:max]) + "..."
	}
	return s
}

func (r report) writeTo(w io.Writer) error {
	var writeErr error
	out := func(format string, args ...any) {
		if writeErr == nil {
			_, writeErr = fmt.Fprintf(w, format, args...)
		}
	}
	rate := 0.0
	if r.UserTurns > 0 {
		rate = float64(r.Flagged) / float64(r.UserTurns) * 100
	}
	out("User turns: %d\n", r.UserTurns)
	out("Directive-flagged turns: %d\n", r.Flagged)
	out("Flag rate: %.2f%%\n", rate)
	out("  (share of all user turns the lexical detector would nudge; flagged turns)\n")
	out("  (the flagged turns are the sample for judging false positives; -list prints them)\n")
	if r.ListFlagged {
		out("\nFlagged user turns:\n")
		for i, row := range r.FlaggedRows {
			out("  [%d] at %s session %s\n", i+1, row.At.Format(time.RFC3339), row.Session)
			out("      %s\n", oneLine(row.Text))
		}
	}
	out("\nRepeat corrections: %d\n", r.Repeats)
	if r.Project == "" {
		out("Rule presence scope: all projects\n")
		out("  (an unrelated memory in another project sharing two words with the rule can\n")
		out("   make it read as present; -project narrows the read)\n")
	} else {
		out("Rule presence scope: project %s (and _global)\n", r.Project)
	}
	out("  rule absent before repeat: %d\n", r.Missing)
	out("  rule present before repeat: %d\n", r.Repeats-r.Missing-r.Undated)
	if r.Unmatched > 0 {
		out("  repeats with no identifiable earlier rule: %d\n", r.Unmatched)
	}
	if r.Undated > 0 {
		out("  repeats with no timestamp (not checked): %d\n", r.Undated)
	}
	if len(r.MissingRows) > 0 {
		out("\nRepeats with the rule absent before the repeat:\n")
		for i, row := range r.MissingRows {
			out("  [%d] at %s session %s\n", i+1, row.At.Format(time.RFC3339), row.Session)
			out("      repeat: %s\n", oneLine(row.Repeat))
			out("      rule:   %s\n", oneLine(row.Rule))
		}
	}
	return writeErr
}
