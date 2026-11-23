package mcpinit

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/hostevent"
	"github.com/wcatz/ghost/internal/memory"
)

// handoffScan is the working state a pre-compact hook reads out of the raw
// transcript before it is condensed. It is deliberately small: a handoff is the
// task in flight, the questions still open, the files in play and a pointer back
// to the history it came from — not a second copy of the conversation.
type handoffScan struct {
	Task      string
	Questions []string
	Files     []string
	Position  int
}

const (
	maxHandoffLineBytes   = 64 << 10
	maxHandoffTaskBytes   = 600
	maxHandoffQuestions   = 5
	maxHandoffFiles       = 10
	maxHandoffQuestionRun = 300
)

// scanHandoffTranscript reads one transcript through the format the envelope
// declares. A malformed line is skipped — one damaged line must not cost the
// rest of the session's state — and so is a line past the bound: it is counted
// but never held in memory, so the hook stays bounded and fail-open while the
// position still reflects the true end of the history.
func scanHandoffTranscript(format string, r io.Reader) handoffScan {
	var out handoffScan
	if r == nil {
		return out
	}
	br := bufio.NewReaderSize(r, 4096)
	seen := make(map[string]bool)
	var open []openQuestion
	for {
		line, oversized, ok := readHandoffLine(br)
		if !ok {
			break
		}
		out.Position++
		if oversized {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal(line, &obj); err != nil {
			continue
		}
		if !handoffIsUser(obj) {
			collectHandoffFiles(obj, &out, seen)
			// A later assistant turn is where an open question gets settled.
			// Only text that carries every distinctive term of a question is
			// read as its answer, so a passing mention does not close it.
			if !handoffHasToolBlock(obj) {
				open = dropAnsweredQuestions(open, handoffText(obj))
			}
			continue
		}
		// A tool result or tool use arrives as a user line but is command or
		// file output, not something the user said.
		if handoffHasToolBlock(obj) {
			continue
		}
		text := handoffText(obj)
		if text == "" {
			continue
		}
		// The guard runs on the whole value before it is cut, so a credential
		// the cut would split is still seen. A flagged task is withheld, not
		// replaced by an earlier one: that would be stale state.
		if handoffHoldsSecret("task", text) {
			out.Task = ""
		} else {
			out.Task = truncateUTF8(text, maxHandoffTaskBytes)
		}
		for _, q := range handoffQuestions(text) {
			open = rememberQuestion(open, q)
		}
	}
	for _, q := range open {
		out.Questions = append(out.Questions, q.text)
	}
	return out
}

// openQuestion is one still-open question the handoff may carry. key folds case
// and punctuation so a re-asked question supersedes its earlier wording rather
// than appearing twice; tokens are the distinctive words an answer must carry.
type openQuestion struct {
	text   string
	key    string
	tokens []string
}

// rememberQuestion keeps the latest maxHandoffQuestions unique open questions.
// A question asked again moves to the back with its newest wording, so the
// window holds what the session is still working on rather than what it asked
// first.
func rememberQuestion(open []openQuestion, text string) []openQuestion {
	q := openQuestion{text: text, key: handoffQuestionKey(text), tokens: handoffDistinctiveTokens(text)}
	for i, existing := range open {
		if existing.key == q.key {
			open = append(open[:i], open[i+1:]...)
			break
		}
	}
	open = append(open, q)
	if len(open) > maxHandoffQuestions {
		open = open[len(open)-maxHandoffQuestions:]
	}
	return open
}

// maxHandoffAnswerBytes bounds the part of one turn scanned for answers.
const maxHandoffAnswerBytes = 8 << 10

// handoffWords splits text into lowercase a-z0-9 words.
func handoffWords(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	})
}

// handoffQuestionKey folds a question to the identity a repeat is judged by.
func handoffQuestionKey(text string) string {
	return strings.Join(handoffWords(text), " ")
}

// handoffQuestionStopWords are the words every question carries, so their
// presence in an answer would prove nothing.
var handoffQuestionStopWords = map[string]bool{
	"what": true, "when": true, "where": true, "which": true, "whose": true,
	"why": true, "how": true, "should": true, "could": true, "would": true,
	"does": true, "did": true, "this": true, "that": true, "these": true,
	"those": true, "there": true, "their": true, "about": true, "with": true,
	"from": true, "have": true, "will": true, "your": true, "ours": true,
	"need": true, "want": true, "into": true,
}

// handoffDistinctiveTokens is the set of words an answer has to carry before a
// question is treated as settled. A question with fewer than two is left open:
// "what port?" has no term an answer could not mention in passing.
func handoffDistinctiveTokens(text string) []string {
	var out []string
	seen := make(map[string]bool)
	for _, w := range handoffWords(text) {
		if len(w) < 4 || handoffQuestionStopWords[w] || seen[w] {
			continue
		}
		seen[w] = true
		out = append(out, w)
	}
	return out
}

// dropAnsweredQuestions removes every open question whose distinctive tokens all
// appear in a later turn. Matching is word-prefix based so "fail" and "fails"
// agree, while "port" does not match "support".
func dropAnsweredQuestions(open []openQuestion, text string) []openQuestion {
	if text == "" || len(open) == 0 {
		return open
	}
	if len(text) > maxHandoffAnswerBytes {
		text = text[:maxHandoffAnswerBytes]
	}
	words := handoffWords(text)
	kept := open[:0]
	for _, q := range open {
		if len(q.tokens) >= 2 && allTokensPresent(q.tokens, words) {
			continue
		}
		kept = append(kept, q)
	}
	return kept
}

func allTokensPresent(tokens, words []string) bool {
	for _, token := range tokens {
		present := false
		for _, w := range words {
			if strings.HasPrefix(w, token) {
				present = true
				break
			}
		}
		if !present {
			return false
		}
	}
	return true
}

// readHandoffLine returns the next line without its newline. A line longer
// than maxHandoffLineBytes is consumed through its newline and reported as
// oversized with no content. ok is false at the end of the input.
func readHandoffLine(br *bufio.Reader) (line []byte, oversized, ok bool) {
	var buf []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if !oversized {
			if len(buf)+len(chunk) > maxHandoffLineBytes+1 {
				oversized = true
				buf = nil
			} else {
				buf = append(buf, chunk...)
			}
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil && len(chunk) == 0 && len(buf) == 0 && !oversized {
			return nil, false, false
		}
		return []byte(strings.TrimRight(string(buf), "\r\n")), oversized, true
	}
}

// handoffHasToolBlock reports whether a line carries a tool_use or tool_result
// content block.
func handoffHasToolBlock(obj map[string]any) bool {
	msg, ok := obj["message"].(map[string]any)
	if !ok {
		msg = obj
	}
	blocks, ok := msg["content"].([]any)
	if !ok {
		return false
	}
	for _, b := range blocks {
		m, ok := b.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := m["type"].(string); t == "tool_result" || t == "tool_use" {
			return true
		}
	}
	return false
}

func handoffIsUser(obj map[string]any) bool {
	if v, ok := obj["type"].(string); ok && strings.EqualFold(v, "user") {
		return true
	}
	if v, ok := obj["role"].(string); ok && strings.EqualFold(v, "user") {
		return true
	}
	if msg, ok := obj["message"].(map[string]any); ok {
		if v, ok := msg["role"].(string); ok && strings.EqualFold(v, "user") {
			return true
		}
	}
	if info, ok := obj["info"].(map[string]any); ok {
		if v, ok := info["role"].(string); ok && strings.EqualFold(v, "user") {
			return true
		}
	}
	return false
}

// handoffText collects the prose of one transcript line. Those strings are
// untrusted data from the host's own history; the formatter renders them before
// they reach a prompt.
func handoffText(obj map[string]any) string {
	var parts []string
	var walk func(any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, val := range t {
				if k == "text" || k == "prompt" {
					if s, ok := val.(string); ok {
						parts = append(parts, s)
						continue
					}
				}
				if k == "content" || k == "message" || k == "parts" {
					walk(val)
					continue
				}
			}
		case []any:
			for _, elem := range t {
				walk(elem)
			}
		case string:
			parts = append(parts, t)
		}
	}
	walk(obj)
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func handoffQuestions(text string) []string {
	var out []string
	for _, line := range strings.FieldsFunc(text, func(r rune) bool { return r == '\n' || r == '\r' }) {
		if !strings.Contains(line, "?") {
			continue
		}
		line = strings.TrimSpace(line)
		if handoffHoldsSecret("open question", line) {
			continue
		}
		out = append(out, truncateUTF8(line, maxHandoffQuestionRun))
	}
	return out
}

// handoffHoldsSecret asks the token guard whether a value copied out of the
// transcript is credential-shaped. A handoff is stored and replayed into later
// sessions, so such a value is dropped rather than stored.
func handoffHoldsSecret(field, text string) bool {
	return memory.RejectSecret(field, text) != nil
}

// collectHandoffFiles takes paths only from structured tool-call fields, never
// from prose that happens to mention a path-shaped string.
func collectHandoffFiles(obj map[string]any, out *handoffScan, seen map[string]bool) {
	var walk func(any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, val := range t {
				switch k {
				case "file_path", "filepath", "filename", "path":
					if s, ok := val.(string); ok && strings.ContainsAny(s, `/\`) && len(s) < 500 {
						key := "file:" + s
						if !seen[key] && len(out.Files) < maxHandoffFiles {
							seen[key] = true
							out.Files = append(out.Files, s)
						}
					}
				default:
					walk(val)
				}
			}
		case []any:
			for _, elem := range t {
				walk(elem)
			}
		}
	}
	walk(obj)
}

// formatHandoff renders the handoff block. Ghost owns every label and line;
// only the values it read from the transcript are rendered as data or tokens.
func formatHandoff(scan handoffScan, sessionID, transcriptPath string) string {
	var b strings.Builder
	fmt.Fprintln(&b, "**Handoff from the session before compaction:**")
	if scan.Task != "" {
		fmt.Fprintf(&b, "- Current task: %s\n", assemble.Data(scan.Task))
	}
	if len(scan.Questions) > 0 {
		qs := make([]string, 0, len(scan.Questions))
		for _, q := range scan.Questions {
			qs = append(qs, assemble.Data(q))
		}
		fmt.Fprintf(&b, "- Open questions: %s\n", strings.Join(qs, "; "))
	}
	if len(scan.Files) > 0 {
		fs := make([]string, 0, len(scan.Files))
		for _, f := range scan.Files {
			fs = append(fs, assemble.Token(f))
		}
		fmt.Fprintf(&b, "- Files in play: %s\n", strings.Join(fs, ", "))
	}
	fmt.Fprintf(&b, "- Raw history: session `%s`, transcript position %d of %s\n",
		assemble.Token(sessionID), scan.Position, assemble.Data(transcriptPath))
	return b.String()
}

// runPreCompact is the pre-compact hook. It saves durable facts the same way a
// Stop does, then records a short handoff as a session-retention row. Every
// failure is fail-open: the host proceeds, and only a diagnostic on stderr
// records what Ghost could not do.
func runPreCompact(p hostevent.Payload, stderr io.Writer) {
	spawnLifecycleIfConfigured(p.CWD, string(p.HostSource()), stderr)

	dataDir, err := config.DataDir()
	if err != nil {
		logFailOpen(stderr, "resolve data directory", err)
		return
	}
	dbPath := filepath.Join(dataDir, "ghost.db")
	if _, err := os.Stat(dbPath); err != nil {
		return
	}
	read, err := memory.OpenReadDB(dbPath)
	if err != nil {
		logFailOpen(stderr, "open store", err)
		return
	}
	defer func() { _ = read.Close() }()
	store := sessionStore(read)
	projectID, _ := resolveSessionProject(context.Background(), store, p.CWD)
	if projectID == "" {
		return
	}

	scan := handoffScan{}
	if p.TranscriptPath != "" {
		f, err := os.Open(p.TranscriptPath)
		if err != nil {
			logFailOpen(stderr, "open transcript", err)
			return
		}
		format := ""
		if p.Contract != nil {
			format = p.Contract.TranscriptFormat
		}
		scan = scanHandoffTranscript(format, f)
		_ = f.Close()
	}
	content := formatHandoff(scan, p.SessionID, p.TranscriptPath)

	writeStore, closeWrite, writeErr := openWritableStore(dbPath)
	if writeStore == nil {
		logFailOpen(stderr, "open writable store", writeErr)
		return
	}
	defer closeWrite()
	if err := writeStore.SaveHandoff(context.Background(), memory.SessionHandoff{
		ProjectID:          projectID,
		SessionID:          p.SessionID,
		TranscriptPath:     p.TranscriptPath,
		TranscriptPosition: scan.Position,
		Content:            content,
	}); err != nil {
		logFailOpen(stderr, "save handoff", err)
	}
}

// boundedHandoff keeps a stored handoff from claiming the whole host limit.
// It cuts at the last whole line so a render built from «...» data blocks never
// ends mid-delimiter: an unclosed « would swallow part of the ranked block.
func boundedHandoff(handoff string, max int) string {
	if len(handoff) <= max {
		return handoff
	}
	cut := memory.TruncateUTF8(handoff, max)
	if i := strings.LastIndex(cut, "\n"); i > 0 {
		return cut[:i]
	}
	return cut
}

// latestHandoffFor reads the handoff a compacting session should be given, if
// any: only the handoff this exact session left. A compaction keeps the session
// id, so there is no fallback to another session's handoff, and two sessions on
// one project never hand each other's in-flight task. An unreadable handoff is
// no handoff: it must never stop the session from opening with the ordinary
// block.
func latestHandoffFor(projectID, sessionID string, now time.Time) string {
	if projectID == "" {
		return ""
	}
	dataDir, err := config.DataDirPath()
	if err != nil {
		return ""
	}
	db, err := memory.OpenReadDB(filepath.Join(dataDir, "ghost.db"))
	if err != nil {
		return ""
	}
	defer func() { _ = db.Close() }()
	store := sessionStore(db)
	h, ok, err := store.HandoffForSession(context.Background(), projectID, sessionID, now)
	if err != nil || !ok {
		return ""
	}
	return h.Content
}
