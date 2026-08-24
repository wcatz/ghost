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

// scanners is the format-keyed registry. Entries land before their adapter
// does (spec §4): an adapter that invokes the contract successfully but hits
// an unregistered format would silently disable reflection/resolve/supersede
// for that host's users.
var scanners = map[string]ScanFunc{
	FormatClaudeJSONL:      ScanClaudeJSONL,
	FormatOpencodeMessages: ScanOpencodeMessages,
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

// streamJSONL visits each line of a newline-delimited JSON transcript. The
// 4 MiB line buffer matches the historical stop-hook scanner: transcript
// lines carry full tool results and can be huge.
func streamJSONL(r io.Reader, visit func(line []byte)) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		visit(sc.Bytes())
	}
}

// ScanClaudeJSONL streams a Claude Code transcript and counts assistant
// tool_use blocks, plus how many were Ghost save tools. Unparseable lines are
// skipped; a scanner error mid-file yields the counts seen so far — worst case
// the nudge fires once and the second stop passes through the stop_hook_active
// guard.
func ScanClaudeJSONL(r io.Reader) ScanResult {
	var res ScanResult
	streamJSONL(r, func(line []byte) {
		var l claudeJSONLLine
		if err := json.Unmarshal(line, &l); err != nil {
			return
		}
		if l.Type != "assistant" {
			return
		}
		for _, c := range l.Message.Content {
			if c.Type == "tool_use" {
				res.ToolCalls++
				if ghostSaveTools[c.Name] {
					res.GhostSaves++
				}
			}
		}
	})
	return res
}

// opencodeMessagesLine is the minimal shape needed to spot tool-call parts in
// one opencode-messages JSONL line: a serialized `{info, parts}` pair exactly
// as returned by opencode's session.messages API, so the adapter materializes
// its temp JSONL by writing SDK objects verbatim (spec §2.1).
type opencodeMessagesLine struct {
	Info struct {
		Role string `json:"role"`
	} `json:"info"`
	Parts []struct {
		Type string `json:"type"`
		Tool string `json:"tool"`
	} `json:"parts"`
}

// ghostSaveToolsOpencode mirrors ghostSaveTools under opencode's MCP naming
// convention: tools register as `<server>_<tool>` with non-alphanumeric
// characters folded to '_' (opencode docs, "Names and permissions") — not
// Claude Code's `mcp__<server>__<tool>`.
var ghostSaveToolsOpencode = map[string]bool{
	"ghost_ghost_memory_save": true,
	"ghost_save_global":       true,
}

// ScanOpencodeMessages streams an opencode-messages transcript and counts
// assistant tool-call parts, plus how many were Ghost save tools. Only parts
// typed "tool" count — prose mentions never do — and only assistant messages
// are visited. Unparseable lines are skipped; errors mid-file yield the counts
// seen so far, same fail-open posture as Claude.
func ScanOpencodeMessages(r io.Reader) ScanResult {
	var res ScanResult
	streamJSONL(r, func(line []byte) {
		var l opencodeMessagesLine
		if err := json.Unmarshal(line, &l); err != nil {
			return
		}
		if l.Info.Role != "assistant" {
			return
		}
		for _, p := range l.Parts {
			if p.Type == "tool" {
				res.ToolCalls++
				if ghostSaveToolsOpencode[p.Tool] {
					res.GhostSaves++
				}
			}
		}
	})
	return res
}
