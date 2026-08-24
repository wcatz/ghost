package hostevent

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNormalizeEvent(t *testing.T) {
	cases := map[string]Event{
		"stop":          EventStop,
		"Stop":          EventStop,
		"session-start": EventSessionStart,
		"SessionStart":  EventSessionStart,
		"Session-Start": EventSessionStart,
		"sessionend":    EventSessionEnd,
		"SessionEnd":    EventSessionEnd,
		"":              "",
		"turn-end":      "",
	}
	for in, want := range cases {
		if got := NormalizeEvent(in); got != want {
			t.Errorf("NormalizeEvent(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCapabilityFor(t *testing.T) {
	cases := []struct {
		source Source
		ok     bool
		block  bool
		inject bool
	}{
		{SourceClaudeCode, true, true, true},
		{SourceCodex, true, true, true},
		{SourceGoose, true, true, false},
		{SourceOpencode, true, false, false},
		{"gemini-cli", false, false, false},
		{"", false, false, false},
	}
	for _, c := range cases {
		got, ok := CapabilityFor(c.source)
		if ok != c.ok || got.BlockStop != c.block || got.InjectContext != c.inject {
			t.Errorf("CapabilityFor(%q) = %+v,%v want block=%v inject=%v ok=%v", c.source, got, ok, c.block, c.inject, c.ok)
		}
	}
}

// claudeStopPayload is the exact shape Claude Code sends on Stop: shared
// dialect fields plus hook_event_name, and NO contract envelope.
const claudeStopPayload = `{"hook_event_name":"Stop","session_id":"s1","transcript_path":"/t/x.jsonl","cwd":"/repo","stop_hook_active":false,"source":"startup"}`

// codexStopPayload mirrors codex's documented Stop input with an EXPLICIT
// agreeing envelope, plus codex-specific extras that must be tolerated and
// ignored.
const codexStopPayload = `{"contract":{"version":1,"source":"codex","transcript_format":"codex-rollout"},"hook_event_name":"Stop","session_id":"thr_123","transcript_path":"/w/.codex/rollout.jsonl","cwd":"/w","stop_hook_active":false,"turn_id":"t9","model":"gpt-5.6","permission_mode":"default","last_assistant_message":null}`

// gooseStopPayload is an Open Plugins Stop payload carrying an explicit
// agreeing envelope (what a Phase 2 adapter shim would emit).
const gooseStopPayload = `{"contract":{"version":1,"source":"goose"},"hook_event_name":"Stop","session_id":"g1","transcript_path":"","cwd":"/g","stop_hook_active":true}`

func TestParseClaudeCode(t *testing.T) {
	// Envelope completion: a native Claude Code payload has no contract
	// object — argv completes it, dialect fields pass through untouched.
	p, err := Parse([]byte(claudeStopPayload), "stop", "claude-code")
	if err != nil {
		t.Fatalf("parse native payload: %v", err)
	}
	if p.HostSource() != SourceClaudeCode {
		t.Errorf("source = %q, want claude-code", p.HostSource())
	}
	if p.Contract.Version != ContractVersion || p.Contract.TranscriptFormat != FormatClaudeJSONL {
		t.Errorf("completed envelope wrong: %+v", p.Contract)
	}
	if p.TranscriptPath != "/t/x.jsonl" || p.CWD != "/repo" || p.StopHookActive {
		t.Errorf("fields wrong: %+v", p)
	}
	if p.Event() != EventStop {
		t.Errorf("event = %q, want stop", p.Event())
	}

	// An explicit agreeing envelope is equally valid (adapters may send one).
	wrapped := contractWrap(claudeStopPayload, "Stop", "claude-code", FormatClaudeJSONL)
	if _, err := Parse([]byte(wrapped), "stop", "claude-code"); err != nil {
		t.Errorf("explicit agreeing envelope should parse, got %v", err)
	}

	// Completion is argv-authoritative, not legacy parsing: an explicit
	// envelope that disagrees is still rejected, never repaired.
	bad := strings.Replace(wrapped, `"version":1`, `"version":2`, 1)
	if _, err := Parse([]byte(bad), "stop", "claude-code"); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Errorf("explicit version=2 should fail, got %v", err)
	}
	bad = strings.Replace(wrapped, `"source":"claude-code"`, `"source":"goose"`, 1)
	if _, err := Parse([]byte(bad), "stop", "claude-code"); err == nil || !strings.Contains(err.Error(), "disagrees") {
		t.Errorf("explicit disagreeing source should fail, got %v", err)
	}

	// Missing --source is rejected outright (the CLI fails open with guidance).
	if _, err := Parse([]byte(claudeStopPayload), "stop", ""); err == nil || !strings.Contains(err.Error(), "missing --source") {
		t.Errorf("empty source should error, got %v", err)
	}

	// Unknown argv event is rejected before parsing even matters.
	if _, err := Parse([]byte(wrapped), "turn-end", "claude-code"); err == nil || !strings.Contains(err.Error(), "unknown event") {
		t.Errorf("unknown argv event should error, got %v", err)
	}
}

// contractWrap merges the envelope into a native host payload string.
func contractWrap(inner, eventName, source, format string) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(inner), &m); err != nil {
		panic(err)
	}
	m["hook_event_name"] = eventName
	m["contract"] = map[string]any{"version": ContractVersion, "source": source, "transcript_format": format}
	b, _ := json.Marshal(m)
	return string(b)
}

func TestParseStrict(t *testing.T) {
	t.Run("codex native payload completed from argv", func(t *testing.T) {
		native := `{"hook_event_name":"Stop","session_id":"thr_9","transcript_path":"/w/rollout.jsonl","cwd":"/w","turn_id":"t1"}`
		p, err := Parse([]byte(native), "stop", "codex")
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if p.HostSource() != SourceCodex || p.Event() != EventStop {
			t.Errorf("routing wrong: %+v", p)
		}
		// codex has no v1 scanner mapping yet, so completion starts at none.
		if p.Contract.Version != ContractVersion || p.Contract.TranscriptFormat != FormatNone {
			t.Errorf("completed envelope wrong: %+v", p.Contract)
		}
	})

	t.Run("codex passthrough with unknown extras tolerated", func(t *testing.T) {
		p, err := Parse([]byte(codexStopPayload), "stop", "codex")
		if err != nil {
			t.Fatalf("strict parse: %v", err)
		}
		if p.HostSource() != SourceCodex || p.Event() != EventStop {
			t.Errorf("wrong routing: source=%q event=%q", p.HostSource(), p.Event())
		}
		if p.SessionID != "thr_123" || p.CWD != "/w" || p.StopHookActive {
			t.Errorf("dialect fields wrong: %+v", p)
		}
		if p.Contract.TranscriptFormat != FormatCodexRollout {
			t.Errorf("transcript format = %q, want codex-rollout", p.Contract.TranscriptFormat)
		}
	})

	t.Run("goose payload case-insensitive event name", func(t *testing.T) {
		p, err := Parse([]byte(gooseStopPayload), "stop", "goose")
		if err != nil {
			t.Fatalf("strict parse: %v", err)
		}
		if !p.StopHookActive || p.HostSource() != SourceGoose {
			t.Errorf("payload wrong: %+v", p)
		}
	})

	t.Run("missing transcript_format defaults to none", func(t *testing.T) {
		p, err := Parse([]byte(gooseStopPayload), "stop", "goose")
		if err != nil {
			t.Fatalf("strict parse: %v", err)
		}
		if p.Contract.TranscriptFormat != FormatNone {
			t.Errorf("format = %q, want none", p.Contract.TranscriptFormat)
		}
	})

	t.Run("explicit empty contract object is rejected, never completed", func(t *testing.T) {
		if _, err := Parse([]byte(`{"contract":{},"hook_event_name":"Stop"}`), "stop", "codex"); err == nil || !strings.Contains(err.Error(), "unsupported") {
			t.Errorf(`"contract":{} should fail strict validation, got %v`, err)
		}
	})

	t.Run("null contract object completes like an absent one", func(t *testing.T) {
		p, err := Parse([]byte(`{"contract":null,"hook_event_name":"Stop","session_id":"s","cwd":"/w"}`), "stop", "codex")
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if p.Contract.Version != ContractVersion || p.Contract.TranscriptFormat != FormatNone {
			t.Errorf("completed envelope wrong: %+v", p.Contract)
		}
	})

	t.Run("failures", func(t *testing.T) {
		cases := []struct {
			name       string
			payload    string
			event, src string
			wantErr    string
		}{
			{"unknown argv source", codexStopPayload, "stop", "gemini", "unknown --source"},
			{"version mismatch", `{"contract":{"version":2,"source":"codex"}}`, "stop", "codex", "unsupported"},
			{"source disagreement", strings.Replace(codexStopPayload, `"source":"codex"`, `"source":"goose"`, 1), "stop", "codex", "disagrees"},
			{"event disagreement", strings.Replace(codexStopPayload, `"hook_event_name":"Stop"`, `"hook_event_name":"SessionEnd"`, 1), "stop", "codex", "disagrees"},
			{"unparseable strict payload", "{not json", "stop", "codex", "parse payload"},
			{"bad transcript format", `{"contract":{"version":1,"source":"codex","transcript_format":"pdf"},"hook_event_name":"Stop"}`, "stop", "codex", "unknown transcript_format"},
			{"non-v1 hook event name", `{"contract":{"version":1,"source":"codex"},"hook_event_name":"TeammateIdle"}`, "stop", "codex", "not a v1 event"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if _, err := Parse([]byte(tc.payload), tc.event, tc.src); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("Parse error = %v, want containing %q", err, tc.wantErr)
				}
			})
		}
	})
}

// gooseNativeStopPayload is goose's published Open Plugins Stop shape:
// native `event`/`working_dir` field names, no hook_event_name/cwd, and NO
// contract envelope — exactly what `hooks/hooks.json` commands receive on
// stdin.
const gooseNativeStopPayload = `{"event":"Stop","session_id":"g1","working_dir":"/g/proj"}`

// TestParseGooseNativeAliases pins Phase 2's in-core field aliasing: a native
// goose payload (no envelope, no dialect fields) parses for --source goose
// via envelope completion plus event→hook_event_name / working_dir→cwd
// fallbacks, while every other source keeps strict dialect-only parsing.
func TestParseGooseNativeAliases(t *testing.T) {
	t.Run("native payload without envelope parses via completion + aliasing", func(t *testing.T) {
		p, err := Parse([]byte(gooseNativeStopPayload), "stop", "goose")
		if err != nil {
			t.Fatalf("parse native goose payload: %v", err)
		}
		if p.HostSource() != SourceGoose || p.Event() != EventStop {
			t.Errorf("routing wrong: source=%q event=%q", p.HostSource(), p.Event())
		}
		if p.CWD != "/g/proj" || p.SessionID != "g1" {
			t.Errorf("aliased fields wrong: %+v", p)
		}
		if p.Contract == nil || p.Contract.Version != ContractVersion || p.Contract.TranscriptFormat != FormatNone {
			t.Errorf("completed envelope wrong: %+v", p.Contract)
		}
		if !strings.Contains(string(p.Raw), `"working_dir":"/g/proj"`) {
			t.Errorf("Raw passthrough must keep native bytes verbatim: %s", p.Raw)
		}
	})

	t.Run("dialect fields win over aliases when present", func(t *testing.T) {
		p, err := Parse([]byte(`{"event":"SessionEnd","hook_event_name":"Stop","working_dir":"/native","cwd":"/dialect"}`), "stop", "goose")
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if p.Event() != EventStop || p.CWD != "/dialect" {
			t.Errorf("alias fallback must not override present dialect fields: %+v", p)
		}
	})

	t.Run("aliased event disagreement with argv still rejected", func(t *testing.T) {
		_, err := Parse([]byte(`{"event":"SessionEnd","working_dir":"/g"}`), "stop", "goose")
		if err == nil || !strings.Contains(err.Error(), "disagrees") {
			t.Errorf("aliased SessionEnd vs argv stop should fail, got %v", err)
		}
	})

	t.Run("non-goose sources never alias", func(t *testing.T) {
		if _, err := Parse([]byte(`{"event":"Stop","working_dir":"/w"}`), "stop", "codex"); err == nil || !strings.Contains(err.Error(), "not a v1 event") {
			t.Errorf("native goose fields must not satisfy codex parsing, got %v", err)
		}
	})

	t.Run("explicit envelope still wins over aliases", func(t *testing.T) {
		wrapped := contractWrap(gooseNativeStopPayload, "Stop", "goose", FormatNone)
		p, err := Parse([]byte(wrapped), "stop", "goose")
		if err != nil {
			t.Fatalf("explicit agreeing envelope + aliases should parse, got %v", err)
		}
		if p.HostSource() != SourceGoose || p.CWD != "/g/proj" {
			t.Errorf("explicit-envelope payload wrong: %+v", p)
		}

		bad := strings.Replace(wrapped, `"source":"goose"`, `"source":"opencode"`, 1)
		if _, err := Parse([]byte(bad), "stop", "goose"); err == nil || !strings.Contains(err.Error(), "disagrees") {
			t.Errorf("explicit disagreeing envelope should fail even with aliased fields, got %v", err)
		}
	})
}
