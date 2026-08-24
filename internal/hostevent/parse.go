package hostevent

import (
	"encoding/json"
	"fmt"
)

// Parse builds a Payload from raw stdin bytes. The contract is mandatory:
// sourceArg and eventArg must be non-empty and known, and hook_event_name must
// normalize to eventArg. Any mismatch, unknown value, or unparseable JSON is
// an error — callers fail open.
//
// Envelope completion: hosts in the shared dialect (Claude Code, codex, goose)
// send no contract object — their payloads pass through verbatim and the
// authoritative argv values complete the envelope. An EXPLICIT contract object
// must agree with argv strictly; there is no fallback parsing of unversioned
// payloads (the contract has no legacy mode), and --source remains required.
//
// Host-specific extras at the top level (model, turn_id, permission_mode,
// reason, source, agent_id, …) are ignored by this struct but preserved in
// Raw for handlers that need them (e.g. the Claude session-start gate reads
// agent_id from Raw).
//
// Goose field aliasing: goose follows the Open Plugins hooks spec, and its
// published payload shape does NOT reuse the dialect's field names — it sends
// `event` where the dialect uses hook_event_name and `working_dir` where the
// dialect uses cwd. For --source goose only, each native field is used as a
// fallback when its dialect counterpart is absent or empty, so
// `<ghost> hook <event> --source goose` parses a native goose payload
// directly — no shell shim between goose and ghost. Dialect fields always win
// when present, argv still must agree with the effective event name, and Raw
// keeps the original bytes verbatim either way.
func Parse(data []byte, eventArg, sourceArg string) (Payload, error) {
	if sourceArg == "" {
		return Payload{}, fmt.Errorf("missing --source (the contract has no legacy mode; re-run `ghost mcp init` to migrate hook wiring)")
	}
	wantEvent := NormalizeEvent(eventArg)
	if wantEvent == "" {
		return Payload{}, fmt.Errorf("unknown event %q", eventArg)
	}
	wantSource := Source(sourceArg)
	if _, ok := CapabilityFor(wantSource); !ok {
		return Payload{}, fmt.Errorf("unknown --source %q", sourceArg)
	}

	var p Payload
	p.Raw = append(p.Raw, data...)
	if err := json.Unmarshal(data, &p); err != nil {
		return Payload{}, fmt.Errorf("parse payload: %w", err)
	}
	applyGooseAliases(&p, data, wantSource)

	// Absent (or null) contract object → argv completes the envelope. An
	// explicit contract must validate strictly below; `"contract": {}` is
	// explicit-but-invalid and is rejected, never silently completed.
	if p.Contract == nil {
		p.Contract = &Envelope{
			Version:          ContractVersion,
			Source:           sourceArg,
			TranscriptFormat: defaultFormatFor(wantSource),
		}
	}

	if p.Contract.Version != ContractVersion {
		return Payload{}, fmt.Errorf("contract.version %d unsupported (want %d)", p.Contract.Version, ContractVersion)
	}
	if Source(p.Contract.Source) != wantSource {
		return Payload{}, fmt.Errorf("contract.source %q disagrees with --source %q", p.Contract.Source, sourceArg)
	}
	gotEvent := p.Event()
	if gotEvent == "" {
		return Payload{}, fmt.Errorf("payload hook_event_name %q is not a v1 event", p.HookEventName)
	}
	if gotEvent != wantEvent {
		return Payload{}, fmt.Errorf("payload event %q disagrees with argv event %q", p.HookEventName, eventArg)
	}
	switch p.Contract.TranscriptFormat {
	case "", FormatNone, FormatClaudeJSONL, FormatOpencodeMessages, FormatCodexRollout:
		if p.Contract.TranscriptFormat == "" {
			p.Contract.TranscriptFormat = FormatNone
		}
	default:
		return Payload{}, fmt.Errorf("unknown transcript_format %q", p.Contract.TranscriptFormat)
	}
	return p, nil
}

// defaultFormatFor names the native transcript format of hosts in the shared
// dialect, used only when argv completes an absent envelope. Sources without a
// v1 scanner mapping start at "none" until their adapter lands.
func defaultFormatFor(source Source) string {
	if source == SourceClaudeCode {
		return FormatClaudeJSONL
	}
	return FormatNone
}

// gooseNativeFields mirrors the native payload fields goose actually sends,
// per the Open Plugins hooks spec: `event` and `working_dir` instead of the
// dialect's hook_event_name and cwd. It is a separate aux struct — not extra
// Payload fields — because Payload already exposes an Event() method (a field
// of that name cannot coexist) and because non-goose payloads must never have
// these names populated on the contract struct.
type gooseNativeFields struct {
	Event      string `json:"event"`
	WorkingDir string `json:"working_dir"`
}

// applyGooseAliases backfills goose's native field names onto the dialect
// fields, but only for --source goose and only where the dialect field is
// absent or empty — a present hook_event_name/cwd always wins over its alias,
// and other sources parse untouched. The payload's own event name is still
// validated against argv afterwards, so an aliased disagreement is rejected
// exactly like a native-dialect one.
func applyGooseAliases(p *Payload, data []byte, source Source) {
	if source != SourceGoose || (p.HookEventName != "" && p.CWD != "") {
		return
	}
	var native gooseNativeFields
	if err := json.Unmarshal(data, &native); err != nil {
		return // malformed JSON already failed the Payload unmarshal above
	}
	if p.HookEventName == "" {
		p.HookEventName = native.Event
	}
	if p.CWD == "" {
		p.CWD = native.WorkingDir
	}
}
