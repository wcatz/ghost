package hostevent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

var maxPayloadBytes = 1 << 20

var errPayloadTooLarge = errors.New("hook payload exceeds the size ceiling")

// ReadPayload reads one hook payload from r with a size bound.
func ReadPayload(r io.Reader) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, int64(maxPayloadBytes)+1))
}

// Parse builds a Payload from raw bytes and validates it against the contract.
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

	if len(data) > maxPayloadBytes {
		return Payload{}, fmt.Errorf("%w (%d bytes, ceiling %d)", errPayloadTooLarge, len(data), maxPayloadBytes)
	}

	var p Payload
	p.Raw = append(p.Raw, data...)
	if err := json.Unmarshal(data, &p); err != nil {
		return Payload{}, fmt.Errorf("parse payload: %w", err)
	}
	applyGooseAliases(&p, data, wantSource)

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
	case "", FormatNone, FormatClaudeJSONL, FormatOpencodeMessages, FormatOpencodeV2Messages, FormatCodexRollout:
		if p.Contract.TranscriptFormat == "" {
			p.Contract.TranscriptFormat = FormatNone
		}
	default:
		return Payload{}, fmt.Errorf("unknown transcript_format %q", p.Contract.TranscriptFormat)
	}
	return p, nil
}

func defaultFormatFor(source Source) string {
	switch source {
	case SourceClaudeCode:
		return FormatClaudeJSONL
	case SourceCodex:
		return FormatCodexRollout
	default:
		return FormatNone
	}
}

type gooseNativeFields struct {
	Event      string `json:"event"`
	WorkingDir string `json:"working_dir"`
}

func applyGooseAliases(p *Payload, data []byte, source Source) {
	if source != SourceGoose || (p.HookEventName != "" && p.CWD != "") {
		return
	}
	var native gooseNativeFields
	if err := json.Unmarshal(data, &native); err != nil {
		return
	}
	if p.HookEventName == "" {
		p.HookEventName = native.Event
	}
	if p.CWD == "" {
		p.CWD = native.WorkingDir
	}
}
