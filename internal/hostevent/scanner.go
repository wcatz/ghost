package hostevent

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
)

// Transcript formats known to the contract. The audit scanner registry is
// keyed by these values; ghost selects a scanner by format, never by source.
const (
	FormatNone               = "none"
	FormatClaudeJSONL        = "claude-jsonl"
	FormatOpencodeMessages   = "opencode-messages"
	FormatOpencodeV2Messages = "opencode-v2-messages"
	FormatCodexRollout       = "codex-rollout"
)

// ghostSaveToolNames are Ghost's save tools as the MCP server names them.
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
// server-aware — the same tool name under another server never counts.
func isGhostSaveTool(name string) bool { return isGhostTool(name, ghostSaveToolNames) }

// ghostUpdateToolNames are the tools that rewrite a memory already stored, whose
// arguments are the memory's own id and new text. They are kept apart from
// ghostSaveToolNames because an edit of an existing row is not a new save. The
// tools that act on a memory by id without carrying its text (pin, promote,
// flag, delete, resolve_mark) are deliberately not here: naming a memory to act
// on it is a deliberate reference, which is what a citation is.
var ghostUpdateToolNames = map[string]bool{
	"ghost_memory_update": true,
}

// isGhostUpdateTool is isGhostSaveTool for the tools that edit a stored memory.
func isGhostUpdateTool(name string) bool { return isGhostTool(name, ghostUpdateToolNames) }

// isGhostTool reports whether name is one of names under the ghost server, however
// the host spells the qualifier.
func isGhostTool(name string, names map[string]bool) bool {
	if names[name] {
		return true
	}
	for _, q := range ghostServerQualifiers {
		if rest, ok := strings.CutPrefix(name, q); ok && names[rest] {
			return true
		}
	}
	return false
}

// maxTranscriptLine bounds the memory one transcript line's CONTENT may take. A
// var so tests can lower it — which also means the tests that lower it must not
// run in parallel with each other, since the value is process-wide and the
// override is not scoped to a goroutine.
var maxTranscriptLine = 64 << 20

// errTranscriptLineTooLong reports a line past maxTranscriptLine.
var errTranscriptLineTooLong = errors.New("transcript line exceeds the memory ceiling")

// transcriptContentLen is the length of line with its line terminator excluded.
// The ceiling is about how much CONTENT one line may hold, and the bytes that
// end it are not content: charging them made a line of exactly maxTranscriptLine
// fail, one byte over for "\n" and two for the "\r\n" a Windows host writes.
// A chunk that does not end in a terminator is mid-line, so its length is its
// content and is reported unchanged.
func transcriptContentLen(line []byte) int {
	n := len(line)
	if n > 0 && line[n-1] == '\n' {
		n--
		if n > 0 && line[n-1] == '\r' {
			n--
		}
	}
	return n
}

// streamJSONL visits each line of a newline-delimited JSON transcript and
// returns the terminal read error, if any (an I/O failure, or a line past
// maxTranscriptLine). Lines carry full tool results: an opencode message holds
// every tool call and output of an agentic turn, so real lines pass the old
// 4 MiB bufio.Scanner cap and aborted the scan (#632). The ceiling only bounds
// memory, and it bounds the content of a line rather than the bytes read for it
// (see transcriptContentLen). The visited slice is valid until the next line is
// read; a trailing "\r" is dropped and a final line without a newline is
// visited, as with bufio.ScanLines. A partial line cut off by a read error is
// not visited.
func streamJSONL(r io.Reader, visit func(line []byte)) error {
	br := bufio.NewReaderSize(r, 64*1024)
	var line []byte
	for {
		line = line[:0]
		for {
			chunk, err := br.ReadSlice('\n')
			line = append(line, chunk...)
			if transcriptContentLen(line) > maxTranscriptLine {
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
