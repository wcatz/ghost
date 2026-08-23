// Package hostevent implements the normalized host-event contract (version 1):
// a thin, versioned envelope over the de-facto cross-host hook dialect
// (Claude Code / codex / goose share the same stdin JSON fields and block
// protocol). See docs/superpowers/specs/2026-08-23-normalized-host-contract-design.md.
//
// Design invariants:
//
//   - Fail-open is absolute. Every parse or validation failure returns an
//     error; callers log one line to stderr, print nothing to stdout, and
//     exit 0 — "allow the stop" is the only failure response ghost emits.
//   - Host-specific payload extras (model, turn_id, permission_mode, reason,
//     agent_id, …) are tolerated and ignored; unknown fields never reject a
//     payload.
//   - Transcript scanning is selected by format, never by source.
package hostevent

import (
	"encoding/json"
	"strings"
)

// ContractVersion is the current envelope version. Adapters must send
// contract.version == ContractVersion; anything else fails open.
const ContractVersion = 1

// Event is a canonical lifecycle event name (argv form).
type Event string

// The v1 event set. argv uses the lowercase kebab forms; payloads may carry
// the hosts' camelCase names ("SessionStart", "Stop", "SessionEnd") —
// NormalizeEvent maps both spellings onto these canonical values.
const (
	EventSessionStart Event = "session-start"
	EventStop         Event = "stop"
	EventSessionEnd   Event = "session-end"
)

// Source identifies the host adapter that produced the event.
type Source string

// Known sources. The list doubles as the capability matrix key set.
const (
	SourceClaudeCode Source = "claude-code"
	SourceCodex      Source = "codex"
	SourceGoose      Source = "goose"
	SourceOpencode   Source = "opencode"
)

// Capability reports what output protocols a host honors, per its published
// documentation (spec §2.1 matrix).
type Capability struct {
	BlockStop     bool
	InjectContext bool
}

// capabilityMatrix mirrors the spec's v1 source-capability table. codex blocks
// Stop via {"decision":"block"} and injects via SessionStart additionalContext;
// goose blocks Stop subject to a host-side consecutive-block cap we never rely
// on; opencode plugins have no stop-blocking or injection surface.
var capabilityMatrix = map[Source]Capability{
	SourceClaudeCode: {BlockStop: true, InjectContext: true},
	SourceCodex:      {BlockStop: true, InjectContext: true},
	SourceGoose:      {BlockStop: true, InjectContext: false},
	SourceOpencode:   {BlockStop: false, InjectContext: false},
}

// CapabilityFor returns the documented capabilities for source. ok is false
// for unknown sources; callers must fail open on !ok.
func CapabilityFor(source Source) (Capability, bool) {
	cap, ok := capabilityMatrix[source]
	return cap, ok
}

// Envelope is ghost's namespaced extension object inside the payload. It is
// nested under the "contract" key rather than merged into top-level fields
// because hosts already use top-level "source" for their own semantics (the
// session start reason: startup|resume|clear|compact); a collision there
// would break verbatim passthrough of native payloads.
type Envelope struct {
	Version          int    `json:"version"`
	Source           string `json:"source"`
	TranscriptFormat string `json:"transcript_format,omitempty"`
}

// Payload is contract v1: the envelope plus the shared dialect fields every
// conforming host already sends. Field names deliberately match the hosts'
// common input schema so native payloads parse without translation.
type Payload struct {
	Contract       Envelope        `json:"contract"`
	HookEventName  string          `json:"hook_event_name"`
	SessionID      string          `json:"session_id"`
	TranscriptPath string          `json:"transcript_path"`
	CWD            string          `json:"cwd"`
	StopHookActive bool            `json:"stop_hook_active"`
	Raw            json.RawMessage `json:"-"`
}

// HostSource returns the envelope's host id.
func (p Payload) HostSource() Source { return Source(p.Contract.Source) }

// Event returns the payload's normalized canonical event.
func (p Payload) Event() Event { return NormalizeEvent(p.HookEventName) }

// NormalizeEvent maps accepted spellings of a v1 event ("stop", "Stop",
// "session-start", "SessionStart", "sessionend", "SessionEnd") onto the
// canonical Event form, or "" for unrecognized names.
func NormalizeEvent(name string) Event {
	key := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(name), "-", ""))
	switch key {
	case "sessionstart":
		return EventSessionStart
	case "stop":
		return EventStop
	case "sessionend":
		return EventSessionEnd
	default:
		return ""
	}
}
