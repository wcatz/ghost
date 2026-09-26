package hostevent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// Transcript formats known to the contract. The scanner registry is keyed by
// these values; ghost selects a scanner by format, never by source.
const (
	FormatNone               = "none"
	FormatClaudeJSONL        = "claude-jsonl"
	FormatOpencodeMessages   = "opencode-messages"
	FormatOpencodeV2Messages = "opencode-v2-messages"
	FormatCodexRollout       = "codex-rollout"
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
	FormatClaudeJSONL:        ScanClaudeJSONL,
	FormatOpencodeMessages:   ScanOpencodeMessages,
	FormatOpencodeV2Messages: ScanOpencodeV2Messages,
	FormatCodexRollout:       ScanCodexRollout,
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

// ghostSaveToolNames are Ghost's save tools as the MCP server names them.
// Their presence in a transcript proves the session saved knowledge to Ghost.
var ghostSaveToolNames = map[string]bool{
	"ghost_memory_save": true,
	"ghost_save_global": true,
}

// ghostServerQualifiers are the ways MCP hosts qualify a tool with its server
// (here, "ghost"), in the order they must be tried — "mcp__ghost__" before
// codex's separator-less "mcp__ghost":
//   - Claude Code: mcp__<server>__<tool>
//   - opencode (V1, and V2 with codemode off): <server>_<tool>
//   - opencode V2 Code Mode: <server>.<tool>
//   - codex: namespace "mcp__<server>" concatenated with the name
var ghostServerQualifiers = []string{"mcp__ghost__", "ghost_", "ghost.", "mcp__ghost"}

// isGhostSaveTool reports whether a transcript's tool name is one of Ghost's
// save tools, however the host spells it: bare, or qualified with the ghost
// server under any host's convention. Matching is harness-agnostic (#505) but
// server-aware — the same tool name under another server never counts, or a
// stop would skip the nudge when nothing was saved to Ghost.
func isGhostSaveTool(name string) bool {
	if ghostSaveToolNames[name] {
		return true
	}
	for _, q := range ghostServerQualifiers {
		if rest, ok := strings.CutPrefix(name, q); ok && ghostSaveToolNames[rest] {
			return true
		}
	}
	return false
}

// maxTranscriptLine bounds the memory one transcript line may take. A var so
// tests can lower it.
var maxTranscriptLine = 64 << 20

// errTranscriptLineTooLong reports a line past maxTranscriptLine.
var errTranscriptLineTooLong = errors.New("transcript line exceeds the memory ceiling")

// streamJSONL visits each line of a newline-delimited JSON transcript and
// returns the terminal read error, if any (an I/O failure, or a line past
// maxTranscriptLine). Lines carry full tool results: an opencode message holds
// every tool call and output of an agentic turn, so real lines pass the old
// 4 MiB bufio.Scanner cap and aborted the scan (#632). The ceiling only bounds
// memory. The visited slice is valid until the next line is read; a trailing
// "\r" is dropped and a final line without a newline is visited, as with
// bufio.ScanLines. A partial line cut off by a read error is not visited.
func streamJSONL(r io.Reader, visit func(line []byte)) error {
	br := bufio.NewReaderSize(r, 64*1024)
	var line []byte
	for {
		line = line[:0]
		for {
			chunk, err := br.ReadSlice('\n')
			line = append(line, chunk...)
			if len(line) > maxTranscriptLine {
				return errTranscriptLineTooLong
			}
			if err == bufio.ErrBufferFull {
				continue
			}
			if err == io.EOF {
				if len(line) > 0 {
					visit(bytes.TrimSuffix(line, []byte("\r")))
				}
				return nil
			}
			if err != nil {
				return err
			}
			break
		}
		line = bytes.TrimSuffix(line, []byte("\n"))
		visit(bytes.TrimSuffix(line, []byte("\r")))
	}
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
				if isGhostSaveTool(c.Name) {
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
// function call), plus how many were Ghost save tools per
// isGhostSaveTool (namespace and name are concatenated, as codex flattens them). Unparseable lines are skipped; errors mid-file are
// returned with partial counts, same fail-open posture as the other scanners.
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
			if isGhostSaveTool(flat) {
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
				if isGhostSaveTool(p.Tool) {
					res.GhostSaves++
				}
			}
		}
	})
	return res, err
}

// opencodeV2MessageLine is the minimal shape needed to spot tool calls in one
// opencode-v2-messages JSONL line: a V2 session.context message serialized
// verbatim by the adapter. Assistant messages carry their tool calls as
// content entries typed "tool".
type opencodeV2MessageLine struct {
	Type    string `json:"type"`
	Content []struct {
		Type  string `json:"type"`
		Name  string `json:"name"`
		State struct {
			Metadata struct {
				ToolCalls []struct {
					Tool string `json:"tool"`
				} `json:"toolCalls"`
			} `json:"metadata"`
		} `json:"state"`
	} `json:"content"`
}

// ScanOpencodeV2Messages streams an opencode-v2-messages transcript and counts
// assistant tool entries, plus how many were Ghost saves. A Code Mode
// `execute` entry counts once as a tool call; its saves come only from the
// recorded inner calls (metadata.toolCalls), never from the code text, so
// code or prose that merely names the tool does not count. Unparseable lines
// are skipped; read errors abort the scan and are returned.
func ScanOpencodeV2Messages(r io.Reader) (ScanResult, error) {
	var res ScanResult
	err := streamJSONL(r, func(line []byte) {
		var l opencodeV2MessageLine
		if err := json.Unmarshal(line, &l); err != nil {
			return
		}
		if l.Type != "assistant" {
			return
		}
		for _, c := range l.Content {
			if c.Type != "tool" {
				continue
			}
			res.ToolCalls++
			if isGhostSaveTool(c.Name) {
				res.GhostSaves++
				continue
			}
			for _, inner := range c.State.Metadata.ToolCalls {
				if isGhostSaveTool(inner.Tool) {
					res.GhostSaves++
				}
			}
		}
	})
	return res, err
}
