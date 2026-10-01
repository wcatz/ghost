package hostevent

// The AUDIT half of transcript scanning: not "how many saves did this session
// make" but "what did the agent DO with the memories Ghost retrieved".
//
// The whole design is the line this file will not cross, and it is worth stating
// before the code: only the AGENT'S OWN WORDS are evidence. A transcript holds
// two kinds of text — what the agent wrote, and what Ghost handed it — and the
// injected block repeats each retrieved memory's wording VERBATIM. So a scan that
// read a whole file would find every retrieval in use no matter what the agent
// did, and the audit would report "used" at a rate of 100% while measuring
// nothing. Every rule below exists to keep the injected half out:
//
//   - a host's tool RESULT is never read. Claude's tool_result, opencode's
//     state.output and codex's function_call_output are all the same thing under
//     three names: what came back. That is where the memory's wording arrives.
//   - only ASSISTANT-authored regions are visited. A user turn carries the
//     injected block in some hosts, so "read every line" cannot mean "read every
//     line".
//   - a Ghost SAVE's arguments go to the save set, not to prose, because a save
//     restating a memory is a supersession and not a use.
//
// What leaves this file is a set of 64-bit token fingerprints and memory ids —
// never a word, never a query, never a memory's content. It runs synchronously in
// the stop hook, which is why it does no I/O beyond the transcript itself and no
// database at all.

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/wcatz/ghost/internal/audit"
)

// AuditScanFunc streams a transcript and returns what the agent did with what it
// was shown.
//
// A non-nil error means the transcript was only PARTIALLY read, and the returned
// Signals are marked degraded before the error is returned — a caller that
// persisted verdicts from a truncated transcript without that marker would file
// "ignored" as a claim about the session rather than about the text that was
// read. The signals are still returned on an error: the lines that were read are
// real evidence, and discarding them would turn a partial answer into none.
//
// The hasher is a parameter because a token is a function of the word AND the
// per-install key, and the scanner is the first thing that computes one. A
// scanner handed no hasher would produce a Signals with no tokens, which is Empty,
// and the caller would read that as "this transcript held no evidence" rather than
// as "no key" — so the key arrives with the reader and a caller that has none
// stops before it gets here.
type AuditScanFunc func(io.Reader, audit.Hasher) (*audit.Signals, error)

// auditScanners is the audit's format-keyed registry, and it is a SEPARATE map
// from scanners rather than a second field on one entry.
//
// The two scanners want different things from the same transcript, and only one
// of them can be right for a given host: the save-nudge must not read a tool's
// arguments, or prose naming a save tool would block a stop, and this one must
// not skip a tool's output, or every use would be invisible. Bundling them into
// one struct would make every host's shape a compromise of both.
//
// TestScanAuditRegistryCoversEveryScanFormat is what keeps the two in step, and
// it is the reason this map is allowed to exist separately at all: a format
// added to one and not the other would silently disable the audit — or the
// nudge — for that host's users, which is the failure the scanners registry's
// own comment is about one level up.
var auditScanners = map[string]AuditScanFunc{
	FormatClaudeJSONL:        AuditScanClaudeJSONL,
	FormatOpencodeMessages:   AuditScanOpencodeMessages,
	FormatOpencodeV2Messages: AuditScanOpencodeV2Messages,
	FormatCodexRollout:       AuditScanCodexRollout,
}

// ScanAudit runs the audit scanner registered for format.
//
// The shape is Scan's, deliberately: ok is false when nothing is registered, and
// a caller must treat that as fail-open rather than as an error to surface to the
// host. An unregistered format means the audit is off for that host — never that
// the host misbehaved — so returning a signals value there would invite a caller
// to persist a verdict derived from nothing.
func ScanAudit(format string, r io.Reader, h audit.Hasher) (*audit.Signals, bool, error) {
	fn, ok := auditScanners[format]
	if !ok {
		return nil, false, nil
	}
	sig, err := fn(r, h)
	return sig, true, err
}

// finish marks a partial read on the signals and hands both back, so every
// scanner's terminal step is the same two lines rather than five copies of them.
func finish(sig *audit.Signals, err error) (*audit.Signals, error) {
	if err != nil {
		sig.MarkDegraded(fmt.Sprintf("scan transcript: stopped before the end (%v)", err))
	}
	return sig, err
}

// toolArgText renders a tool call's arguments as the text of the agent's own
// words, and deliberately drops the OBJECT KEYS.
//
// A tool call's argument object is mostly schema: `{"file_path": "...",
// "content": "..."}`, where the keys belong to the harness and the values to the
// agent. Keys are short and shared — "content", "command", "path", "description"
// — and every transcript holds them in nearly every call, so feeding them to the
// token arm would put a handful of words in the agent's set that no prose could
// ever have contributed. A memory about a file's "content" would then read as
// used by any session that edited a file. Dropping the keys costs a memory whose
// only distinctive word is a schema word, which is the conservative direction.
//
// Numbers and booleans are dropped for the same reason and a stronger one: they
// are not words, and a memory whose tokens are hex runs is an id, which has its
// own arm.
func toolArgText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		// A host that writes its arguments as something other than JSON is
		// passing the agent's own words through unencoded; take them as they are
		// rather than losing them.
		return string(raw)
	}
	return walkArgText(v)
}

func walkArgText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		parts := make([]string, 0, len(t))
		for _, e := range t {
			if s := walkArgText(e); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, " ")
	case map[string]any:
		parts := make([]string, 0, len(t))
		for _, e := range t {
			if s := walkArgText(e); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, " ")
	}
	return ""
}

// addToolCall files one tool call's arguments under the right set.
//
// The save/tool split is here rather than in each scanner so all four formats
// decide it the same way: a Ghost save's words are the agent DECLARING knowledge
// again, which is the superseded-in-session bucket, and a save that counted as
// usage would leave that bucket permanently empty. isGhostSaveTool already
// resolves every host's naming convention, so a scanner passes the name it found
// and this decides.
func addToolCall(sig *audit.Signals, name string, input json.RawMessage) {
	if text := toolArgText(input); text != "" {
		if isGhostSaveTool(name) {
			sig.AddSaveArgs(text)
		} else {
			sig.AddToolArgs(text)
		}
	}
}

// claudeAuditLine is the assistant-authored shape of one Claude Code transcript
// line, plus the text of a tool_use's arguments.
//
// Content is a list of typed blocks and only two of the types are read: "text",
// which is prose, and "tool_use", whose input is the agent's call. "thinking" is
// deliberately NOT read — a host may redact or encrypt it, and a verdict that
// depended on a field hosts omit by design would be a verdict about a transcript
// shape rather than about a session. The remaining types (tool_result lives under
// a user turn and is never reached here) are the harness's, not the agent's.
type claudeAuditLine struct {
	Type    string `json:"type"`
	Message struct {
		Content []struct {
			Type  string          `json:"type"`
			Name  string          `json:"name"`
			Text  string          `json:"text"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	} `json:"message"`
}

// AuditScanClaudeJSONL streams a Claude Code transcript for what the agent wrote.
//
// Assistant lines only. A "user" line is where the injected block and every tool
// result arrive, so visiting them would make the audit report "used" for every
// retrieval — see the file comment. Unparseable lines are skipped rather than
// fatal, exactly as the save-nudge's scanner skips them: one bad line must not
// cost a whole session's audit.
func AuditScanClaudeJSONL(r io.Reader, h audit.Hasher) (*audit.Signals, error) {
	sig := audit.NewWithHasher(h)
	err := streamJSONL(r, func(line []byte) {
		var l claudeAuditLine
		if err := json.Unmarshal(line, &l); err != nil || l.Type != "assistant" {
			return
		}
		for _, c := range l.Message.Content {
			switch c.Type {
			case "text":
				sig.AddProse(c.Text)
			case "tool_use":
				addToolCall(sig, c.Name, c.Input)
			}
		}
	})
	return finish(sig, err)
}

// opencodeAuditLine is the shape of one opencode-messages JSONL line, including
// the part of a tool call the agent authored.
//
// state.input is the agent's call and state.output is what came back. Reading
// both would make the audit report a use for every memory the agent merely
// fetched, which is the failure mode this whole file is about: opencode's output
// IS the injected block, verbatim, for the search tool.
type opencodeAuditLine struct {
	Info struct {
		Role string `json:"role"`
	} `json:"info"`
	Parts []struct {
		Type  string `json:"type"`
		Tool  string `json:"tool"`
		Text  string `json:"text"`
		State struct {
			Input json.RawMessage `json:"input"`
		} `json:"state"`
	} `json:"parts"`
}

// AuditScanOpencodeMessages streams an opencode-messages transcript for what the
// agent wrote. Assistant messages only, and inside them the text parts and the
// INPUT of each tool part — never the output.
func AuditScanOpencodeMessages(r io.Reader, h audit.Hasher) (*audit.Signals, error) {
	sig := audit.NewWithHasher(h)
	err := streamJSONL(r, func(line []byte) {
		var l opencodeAuditLine
		if err := json.Unmarshal(line, &l); err != nil || l.Info.Role != "assistant" {
			return
		}
		for _, p := range l.Parts {
			switch p.Type {
			case "text":
				sig.AddProse(p.Text)
			case "tool":
				addToolCall(sig, p.Tool, p.State.Input)
			}
		}
	})
	return finish(sig, err)
}

// opencodeV2AuditLine is the shape of one opencode-v2-messages JSONL line.
//
// The Code Mode `execute` entry is why this type is not just the V1 one: the
// outer state.input holds the JavaScript the agent wrote, and the real
// arguments of each call it made live in state.metadata.toolCalls[].input. The
// code text is NOT read, and the reason is specific rather than tidy — code that
// embeds a save's arguments repeats the save's own content, so reading the code
// AND the inner input would file a save as BOTH a use and a supersession, and
// the comparison's precedence would then hand the verdict to whichever it
// happened to test first.
type opencodeV2AuditLine struct {
	Type    string `json:"type"`
	Content []struct {
		Type  string `json:"type"`
		Name  string `json:"name"`
		Text  string `json:"text"`
		State struct {
			Input    json.RawMessage `json:"input"`
			Metadata struct {
				ToolCalls []struct {
					Tool  string          `json:"tool"`
					Input json.RawMessage `json:"input"`
				} `json:"toolCalls"`
			} `json:"metadata"`
		} `json:"state"`
	} `json:"content"`
}

// codeModeTool is the name opencode V2 gives a Code Mode tool entry, whose real
// arguments are its recorded inner calls rather than its own input.
const codeModeTool = "execute"

// AuditScanOpencodeV2Messages streams an opencode-v2-messages transcript for
// what the agent wrote.
func AuditScanOpencodeV2Messages(r io.Reader, h audit.Hasher) (*audit.Signals, error) {
	sig := audit.NewWithHasher(h)
	err := streamJSONL(r, func(line []byte) {
		var l opencodeV2AuditLine
		if err := json.Unmarshal(line, &l); err != nil || l.Type != "assistant" {
			return
		}
		for _, c := range l.Content {
			switch c.Type {
			case "text":
				sig.AddProse(c.Text)
			case "tool":
				if c.Name == codeModeTool {
					for _, inner := range c.State.Metadata.ToolCalls {
						addToolCall(sig, inner.Tool, inner.Input)
					}
					continue
				}
				addToolCall(sig, c.Name, c.State.Input)
			}
		}
	})
	return finish(sig, err)
}

// codexAuditLine is the shape of one codex rollout JSONL record, including the
// arguments of a function call and the command of a local shell call.
//
// Only response_item lines carry model traffic; session_meta, event_msg and
// turn_context are envelope bookkeeping. The arguments arrive as a JSON-ENCODED
// STRING, so they are decoded once more before being read — a second decode that
// is what makes a codex save a save, since the content lives inside that string.
type codexAuditLine struct {
	Type    string `json:"type"`
	Payload struct {
		Type      string  `json:"type"`
		Name      string  `json:"name"`
		Namespace *string `json:"namespace"`
		Arguments string  `json:"arguments"`
		Action    struct {
			Command []string `json:"command"`
		} `json:"action"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Role string `json:"role"`
	} `json:"payload"`
}

// AuditScanCodexRollout streams a codex rollout transcript for what the agent
// wrote.
//
// A function_call_output is NOT read, for the same reason opencode's tool output
// is not: it is what came back, and for a Ghost search it is the injected block.
// A local_shell_call's command IS read, and that is a deliberate asymmetry: the
// command is what the agent decided to run, it is nearly all of a codex session's
// activity, and omitting it would report almost every codex session as using
// nothing — a false negative the operator would read as a finding about their
// memories.
func AuditScanCodexRollout(r io.Reader, h audit.Hasher) (*audit.Signals, error) {
	sig := audit.NewWithHasher(h)
	err := streamJSONL(r, func(line []byte) {
		var l codexAuditLine
		if err := json.Unmarshal(line, &l); err != nil || l.Type != "response_item" {
			return
		}
		switch l.Payload.Type {
		case "message":
			if l.Payload.Role != "assistant" {
				return
			}
			for _, c := range l.Payload.Content {
				if c.Type == "text" {
					sig.AddProse(c.Text)
				}
			}
		case "function_call":
			flat := ""
			if l.Payload.Namespace != nil {
				flat = *l.Payload.Namespace
			}
			addToolCall(sig, flat+l.Payload.Name, codexArguments(l.Payload.Arguments))
		case "local_shell_call":
			if len(l.Payload.Action.Command) > 0 {
				sig.AddToolArgs(strings.Join(l.Payload.Action.Command, " "))
			}
		}
	})
	return finish(sig, err)
}

// codexArguments unwraps codex's JSON-encoded arguments string.
//
// It decodes to the arguments' own JSON when it can, and falls back to the raw
// string when it cannot — the same posture as toolArgText, for the same reason:
// a host that stops encoding its arguments must not cost the session its audit.
func codexArguments(s string) json.RawMessage {
	if s == "" {
		return nil
	}
	if json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	b, err := json.Marshal(s)
	if err != nil {
		return nil
	}
	return b
}
