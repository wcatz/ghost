package mcpserver

import (
	"fmt"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

type validityArgs struct {
	ValidFrom  any    `json:"valid_from,omitempty" jsonschema:"When this memory became true. RFC 3339 (2026-10-01T09:00:00Z) or a date (2026-10-01). ghost_memory_search stops returning the memory until then. Omit for a memory that is true as far as anyone knows."`
	ValidUntil any    `json:"valid_until,omitempty" jsonschema:"When this memory stops being true. Same formats as valid_from. ghost_memory_search then stops returning it rather than showing a stale claim — use it for a policy, an endpoint, or a migration with a real expiry. The surfaces that run the context pipeline do the same: ghost_project_context, the ghost://memories/global resource and the session-start block withhold it too, and a project context whose memories have ALL retired says they were withheld rather than that nothing was ever saved — including when the block is not empty, since the block also carries the _global rows and a block of those alone would otherwise read as the project's. The two browsing surfaces (ghost_memories_list and ghost_search_all) still show the memory, marked 'expired', because they browse rather than filter. A stamp can be replaced but not removed. Omit for durable knowledge."`
	VerifiedAt any    `json:"verified_at,omitempty" jsonschema:"When someone last checked this claim. Same formats as valid_from. Omit if nobody has re-checked it."`
	Verified   bool   `json:"verified,omitempty" jsonschema:"Set true to record that you checked this claim just now, instead of passing verified_at."`
	Confidence any    `json:"confidence,omitempty" jsonschema:"How much to trust this, from 0 to 1 (e.g. 0.8). Omit when you have no view — a recorded number is a claim about trust, and stage 4 does not score on it yet."`
	SourceRef  string `json:"source_ref,omitempty" jsonschema:"Where this came from: a file path, a commit, a URL (e.g. 'docs/runbook.md', 'abc123', 'https://example.com/spec'). Omit when there is no reference."`
}

func parseStampArg(field string, raw any, endOfDay bool) (*string, error) {
	if raw == nil {
		return nil, nil
	}
	s, ok := raw.(string)
	if !ok {
		return nil, fmt.Errorf("%s must be a string timestamp, got %T", field, raw)
	}
	return memory.NormalizeStampArg(field, s, endOfDay)
}

func parseStampFields(args validityArgs) (memory.Validity, error) {
	var v memory.Validity
	var err error
	if v.ValidFrom, err = parseStampArg("valid_from", args.ValidFrom, false); err != nil {
		return memory.Validity{}, err
	}
	if v.ValidUntil, err = parseStampArg("valid_until", args.ValidUntil, true); err != nil {
		return memory.Validity{}, err
	}
	if v.VerifiedAt, err = parseStampArg("verified_at", args.VerifiedAt, false); err != nil {
		return memory.Validity{}, err
	}

	if args.Verified && v.VerifiedAt != nil {
		return memory.Validity{}, fmt.Errorf("pass verified or verified_at, not both — one means \"checked just now\" and the other names the moment")
	}
	if args.Verified {
		now := time.Now().UTC().Format(memory.StoredStampLayout)
		v.VerifiedAt = &now
	}

	if err := memory.CheckWindowOrder(v, memory.Validity{}); err != nil {
		return memory.Validity{}, err
	}
	return v, nil
}

func parseConfidence(raw any) (*float64, error) {
	if raw == nil {
		return nil, nil
	}
	f, err := toFloat64(raw, "confidence")
	if err != nil {
		return nil, err
	}
	if f == nil {
		return nil, nil
	}
	if err := memory.CheckConfidence(*f); err != nil {
		return nil, err
	}
	return f, nil
}

type writeFields struct {
	Validity   memory.Validity
	Confidence *float64
	SourceRef  string
}

func resolveWriteFields(args validityArgs) (writeFields, error) {
	var out writeFields
	var err error
	if out.Validity, err = parseStampFields(args); err != nil {
		return writeFields{}, err
	}
	if out.Confidence, err = parseConfidence(args.Confidence); err != nil {
		return writeFields{}, err
	}
	out.SourceRef = args.SourceRef
	return out, nil
}

func (w writeFields) isZero() bool {
	return w.Validity.IsZero() && w.Confidence == nil && w.SourceRef == ""
}

func (w writeFields) provenance(prov memory.Provenance) memory.Provenance {
	prov.SourceRef = w.SourceRef
	prov.Confidence = w.Confidence
	return prov
}

func (w writeFields) foldNotice(prov memory.Provenance) string {
	var parts []string
	for _, f := range []struct {
		name  string
		value *string
	}{
		{"valid_from", w.Validity.ValidFrom},
		{"valid_until", w.Validity.ValidUntil},
		{"verified_at", w.Validity.VerifiedAt},
	} {
		if f.value != nil {
			parts = append(parts, f.name+" "+*f.value)
		}
	}
	if w.Confidence != nil {
		parts = append(parts, fmt.Sprintf("confidence %g", *w.Confidence))
	}
	if prov.Agent != "" {
		parts = append(parts, "agent "+prov.Agent)
	}
	if w.SourceRef != "" {
		parts = append(parts, "source_ref "+quoteData(w.SourceRef))
	}
	return strings.Join(parts, ", ")
}
