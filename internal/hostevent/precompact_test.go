package hostevent

import "testing"

// TestNormalizePreCompactSpelling pins the host spellings of the pre-compact
// lifecycle event. Claude Code writes "PreCompact"; other hosts spell the same
// boundary with an underscore or with no separator at all. All of them must
// normalize to one canonical event so the hook can be wired once per host.
func TestNormalizePreCompactSpelling(t *testing.T) {
	for _, name := range []string{
		"PreCompact", "pre-compact", "PRECOMPACT", " pre-compact ",
		"pre_compact", "precompact", "Pre-Compact",
	} {
		if got := NormalizeEvent(name); got != EventPreCompact {
			t.Errorf("NormalizeEvent(%q) = %q, want %q", name, got, EventPreCompact)
		}
	}
}

// TestUnderscoreAndSpaceSeparatorsAreOnlyForPreCompact pins that the wider
// separators belong to the pre-compact spellings alone: every other event keeps
// the hyphen-only normalisation it had before.
func TestUnderscoreAndSpaceSeparatorsAreOnlyForPreCompact(t *testing.T) {
	for _, name := range []string{"session_start", "session start", "user prompt submit", "user_prompt_submit", "post_tool_use", "session_end"} {
		if got := NormalizeEvent(name); got != "" {
			t.Errorf("NormalizeEvent(%q) = %q, want it unrecognised", name, got)
		}
	}
}

// TestPreCompactIsAParseableEvent pins that a payload naming the event and an
// argv naming the same event agree, the way every other lifecycle event does.
func TestPreCompactIsAParseableEvent(t *testing.T) {
	payload := `{"hook_event_name":"PreCompact","session_id":"s1","cwd":"/repo","transcript_path":"/t/x.jsonl"}`
	p, err := Parse([]byte(payload), "pre-compact", string(SourceClaudeCode))
	if err != nil {
		t.Fatalf("parse pre-compact: %v", err)
	}
	if p.Event() != EventPreCompact {
		t.Errorf("event = %q, want %q", p.Event(), EventPreCompact)
	}
	if p.SessionID != "s1" || p.TranscriptPath != "/t/x.jsonl" {
		t.Errorf("fields wrong: %+v", p)
	}
}

// TestPreCompactCapability pins that only hosts whose adapters can wire the
// event accept it: a host without the capability is silently ignored by the
// dispatch rather than running a handoff it cannot deliver.
func TestPreCompactCapability(t *testing.T) {
	claude, ok := CapabilityFor(SourceClaudeCode)
	if !ok || !claude.PreCompact {
		t.Errorf("claude-code capability = %+v,%v, want PreCompact", claude, ok)
	}
	for _, source := range []Source{SourceCodex, SourceGoose, SourceOpencode} {
		cap, ok := CapabilityFor(source)
		if !ok || cap.PreCompact {
			t.Errorf("%s capability = %+v,%v, want no PreCompact", source, cap, ok)
		}
	}
}
