package hostevent

import (
	"encoding/json"
	"fmt"
)

// Parse builds a Payload from raw stdin bytes. The contract is mandatory:
// sourceArg and eventArg must be non-empty, the payload must carry a contract
// envelope whose version and source match ContractVersion and sourceArg, and
// hook_event_name must normalize to eventArg. Any mismatch, unknown value, or
// unparseable JSON is an error — callers fail open.
//
// Host-specific extras at the top level (model, turn_id, permission_mode,
// reason, source, agent_id, …) are ignored by this struct but preserved in
// Raw for handlers that need them (e.g. the Claude session-start gate reads
// agent_id from Raw).
func Parse(data []byte, eventArg, sourceArg string) (Payload, error) {
	if sourceArg == "" {
		return Payload{}, fmt.Errorf("missing --source (the contract has no legacy mode; re-run `ghost mcp init` to migrate hook wiring)")
	}
	wantEvent := NormalizeEvent(eventArg)
	if wantEvent == "" {
		return Payload{}, fmt.Errorf("unknown event %q", eventArg)
	}

	var p Payload
	p.Raw = append(p.Raw, data...)
	if err := json.Unmarshal(data, &p); err != nil {
		return Payload{}, fmt.Errorf("parse payload: %w", err)
	}

	wantSource := Source(sourceArg)
	if _, ok := CapabilityFor(wantSource); !ok {
		return Payload{}, fmt.Errorf("unknown --source %q", sourceArg)
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
