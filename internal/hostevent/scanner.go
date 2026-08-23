package hostevent

import (
	"bufio"
	"encoding/json"
	"io"
)

// Transcript formats known to the contract. The scanner registry is keyed by
// these values; ghost selects a scanner by format, never by source.
const (
	FormatNone             = "none"
	FormatClaudeJSONL      = "claude-jsonl"
	FormatOpencodeMessages = "opencode-messages"
	FormatCodexRollout     = "codex-rollout"
)

// ScanResult counts assistant tool_use blocks seen in a transcript and how
// many of them were Ghost save tools — the save-nudge's evidence.
type ScanResult struct {
	ToolCalls  int
	GhostSaves int
}

// ScanFunc streams a transcript and returns the counts observed before any
// error. Unparseable lines are skipped; a mid-file read error yields the
// counts seen so far.
type ScanFunc func(io.Reader) ScanResult

// scanners is the format-keyed registry. v1 ships exactly one entry; Phase 1
// adds opencode-messages before its adapter lands (spec §4).
var scanners = map[string]ScanFunc{
	FormatClaudeJSONL: ScanClaudeJSONL,
}

// Scan runs the scanner registered for format. ok is false when no scanner is
// registered — callers must treat that as a fail-open outcome, never an error
// surfaced to the host.
func Scan(format string, r io.Reader) (ScanResult, bool) {
	fn, ok := scanners[format]
	if !ok {
		return ScanResult{}, false
	}
	return fn(r), true
}

// claudeJSONLLine is the minimal shape needed to spot tool_use entries in a
// Claude Code transcript JSONL line. Everything else in the line is ignored.
type claudeJSONLLine struct {
	Type    string `json:"type"`
	Message struct {
		Content []struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"content"`
	} `json:"message"`
}

// ghostSaveTools are the tool names whose presence in a transcript proves the
// session saved knowledge to Ghost.
var ghostSaveTools = map[string]bool{
	"mcp__ghost__ghost_memory_save": true,
	"mcp__ghost__ghost_save_global": true,
}

// ScanClaudeJSONL streams a Claude Code transcript and counts assistant
// tool_use blocks, plus how many were Ghost save tools. Unparseable lines are
// skipped; a scanner error mid-file yields the counts seen so far — worst case
// the nudge fires once and the second stop passes through the stop_hook_active
// guard. The 4 MiB line buffer matches the historical stop-hook scanner:
// transcript lines carry full tool results and can be huge.
func ScanClaudeJSONL(r io.Reader) ScanResult {
	var res ScanResult
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var line claudeJSONLLine
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			continue
		}
		if line.Type != "assistant" {
			continue
		}
		for _, c := range line.Message.Content {
			if c.Type == "tool_use" {
				res.ToolCalls++
				if ghostSaveTools[c.Name] {
					res.GhostSaves++
				}
			}
		}
	}
	return res
}
