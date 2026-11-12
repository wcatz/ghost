// Package hostevent implements the normalized host-event contract.
package hostevent

import (
	"encoding/json"
	"strings"
)

const (
	FormatNone               = "none"
	FormatClaudeJSONL        = "claude-jsonl"
	FormatOpencodeMessages   = "opencode-messages"
	FormatOpencodeV2Messages = "opencode-v2-messages"
	FormatCodexRollout       = "codex-rollout"
)

// ContractVersion is the current envelope version.
const ContractVersion = 1

// Event is a canonical lifecycle event name.
type Event string

const (
	EventSessionStart  Event = "session-start"
	EventStop          Event = "stop"
	EventSessionEnd    Event = "session-end"
	EventMessageSubmit Event = "message-submit"
	EventEdit          Event = "edit"
)

// Source identifies the host adapter that produced the event.
type Source string

const (
	SourceClaudeCode Source = "claude-code"
	SourceCodex      Source = "codex"
	SourceGoose      Source = "goose"
	SourceOpencode   Source = "opencode"
)

// Capability reports what output protocols a host honors.
type Capability struct {
	BlockStop     bool
	InjectContext bool
	WorkingMoment bool
}

var capabilityMatrix = map[Source]Capability{
	SourceClaudeCode: {BlockStop: true, InjectContext: true, WorkingMoment: true},
	SourceCodex:      {BlockStop: true, InjectContext: true},
	SourceGoose:      {BlockStop: true, InjectContext: false},
	SourceOpencode:   {BlockStop: false, InjectContext: false},
}

// CapabilityFor returns the capabilities for source.
func CapabilityFor(source Source) (Capability, bool) {
	cap, ok := capabilityMatrix[source]
	return cap, ok
}

// Envelope is the contract envelope inside a payload.
type Envelope struct {
	Version          int    `json:"version"`
	Source           string `json:"source"`
	TranscriptFormat string `json:"transcript_format,omitempty"`
}

// Payload is the normalized host-event payload.
type Payload struct {
	Contract       *Envelope       `json:"contract"`
	HookEventName  string          `json:"hook_event_name"`
	SessionID      string          `json:"session_id"`
	TranscriptPath string          `json:"transcript_path"`
	CWD            string          `json:"cwd"`
	StopHookActive bool            `json:"stop_hook_active"`
	Raw            json.RawMessage `json:"-"`
}

// HostSource returns the envelope source.
func (p Payload) HostSource() Source { return Source(p.Contract.Source) }

// Event returns the payload's normalized event.
func (p Payload) Event() Event { return NormalizeEvent(p.HookEventName) }

// NormalizeEvent maps accepted event spellings to the canonical form.
func NormalizeEvent(name string) Event {
	key := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(name), "-", ""))
	switch key {
	case "sessionstart":
		return EventSessionStart
	case "stop":
		return EventStop
	case "sessionend":
		return EventSessionEnd
	case "messagesubmit", "userpromptsubmit":
		return EventMessageSubmit
	case "edit", "posttooluse":
		return EventEdit
	default:
		return ""
	}
}
