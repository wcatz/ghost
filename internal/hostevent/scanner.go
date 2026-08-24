package hostevent

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
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

// ScanFunc streams a transcript and returns the counts observed. A non-nil
// error means the transcript was only partially read (I/O failure, scanner
// limit exceeded) — callers must not act on partial counts: a save recorded
// after the cut would be missed and a stop could be wrongly blocked.
type ScanFunc func(io.Reader) (ScanResult, error)

// scanners is the format-keyed registry. Entries land before their adapter
// does (spec §4): an adapter that invokes the contract successfully but hits
// an unregistered format would silently disable reflection/resolve/supersede
// for that host's users.
var scanners = map[string]ScanFunc{
	FormatClaudeJSONL:      ScanClaudeJSONL,
	FormatOpencodeMessages: ScanOpencodeMessages,
	FormatCodexRollout:     ScanCodexRollout,
}

// Scan runs the scanner registered for format. ok is false when no scanner is
// registered — callers must treat that as a fail-open outcome, never an error
// surfaced to the host. err != nil means the scan stopped early; treat the
// counts as incomplete and fail open.
func Scan(format string, r io.Reader) (ScanResult, bool, error) {
	fn, ok := scanners[format]
	if !ok {
		return ScanResult{}, false, nil
	}
	res, err := fn(r)
	return res, true, err
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

// streamJSONL visits each line of a newline-delimited JSON transcript and
// returns the terminal scanner error, if any (I/O read failure, or the line
// buffer limit being exceeded). The buffer matches the historical stop-hook
// scanner: transcript lines carry full tool results and can be huge.
func streamJSONL(r io.Reader, visit func(line []byte)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		visit(sc.Bytes())
	}
	return sc.Err()
}

// ScanClaudeJSONL streams a Claude Code transcript and counts assistant
// tool_use blocks, plus how many were Ghost save tools. Unparseable lines are
// skipped; a read error or over-long line aborts the scan and is returned —
// the counts are partial and the caller must fail open rather than nudge.
func ScanClaudeJSONL(r io.Reader) (ScanResult, error) {
	var res ScanResult
	err := streamJSONL(r, func(line []byte) {
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
	return res, err
}

// codexRolloutLine is the minimal shape of one codex rollout JSONL record
// (openai/codex, codex-rs/rollout): a timestamped envelope whose type selects
// the payload — only "response_item" lines carry model traffic; everything
// else (session_meta, event_msg, turn_context, …) is skipped.
type codexRolloutLine struct {
	Type    string `json:"type"`
	Payload struct {
		Type      string  `json:"type"`
		Name      string  `json:"name"`
		Namespace *string `json:"namespace"`
	} `json:"payload"`
}

// ScanCodexRollout streams a codex rollout transcript and counts tool calls
// (function_call items plus local_shell_call items — codex's shell is not a
// function call), plus how many were Ghost save tools. Ghost save detection
// is separator-agnostic: codex flattens namespaced tools as namespace+name
// with no separator (flat_tool_name in codex-rs/core/src/tools), and MCP
// namespaces take the mcp__<server> form — so "mcp__ghost"+"ghost_memory_save"
// and a legacy flat "ghost_memory_save" both match on substring, while a
// different server's identically-named tool cannot (its namespace lacks
// "ghost"). Unparseable lines are skipped; errors mid-file are returned with
// partial counts, same fail-open posture as the other scanners.
func ScanCodexRollout(r io.Reader) (ScanResult, error) {
	var res ScanResult
	err := streamJSONL(r, func(line []byte) {
		var l codexRolloutLine
		if err := json.Unmarshal(line, &l); err != nil {
			return
		}
		if l.Type != "response_item" {
			return
		}
		switch l.Payload.Type {
		case "function_call":
			res.ToolCalls++
			flat := ""
			if l.Payload.Namespace != nil {
				flat = *l.Payload.Namespace
			}
			flat += l.Payload.Name
			if strings.Contains(flat, "ghost_memory_save") || strings.Contains(flat, "ghost_save_global") {
				res.GhostSaves++
			}
		case "local_shell_call":
			res.ToolCalls++
		}
	})
	return res, err
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
// are visited. Unparseable lines are skipped; a read error or over-long line
// aborts the scan and is returned, same fail-open posture as Claude.
func ScanOpencodeMessages(r io.Reader) (ScanResult, error) {
	var res ScanResult
	err := streamJSONL(r, func(line []byte) {
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
	return res, err
}
