package mcpserver

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// The write-side half of the writer contract (#575). Schema v10 added
// valid_from, valid_until, verified_at, confidence, source_ref and session_id,
// and until now no production path could fill any of them: every row in a live
// store read NULL, and the columns the schema promised were inert weight in
// every SELECT. This file is the one place a tool turns caller arguments into
// those values, so the three writer tools cannot disagree about what a date
// means, what gets stored, or which combinations are impossible.
//
// The rules it enforces, and why each is here rather than left to the store:
//
//   - Nothing is invented. An omitted argument is not a zero value, a default
//     or a guess; it is absent, and absent is stored as NULL. A writer that
//     filled an empty field would make "nobody said" indistinguishable from
//     "said to be nothing", which is the one distinction these columns exist
//     to preserve.
//   - A claim that contradicts itself is refused at the boundary rather than
//     stored and reinterpreted later. valid_until before valid_from is not a
//     window; stage 2 would read it as expired and withhold the row from every
//     search, with no error anywhere to explain why.
//   - A value nobody can read is refused at write time. SQLite holds these
//     columns as unconstrained text and stage 2 reports an unreadable one as
//     validity_unparseable — the right treatment for a row that arrived through
//     an import or a hand edit, and the wrong way to accept a new one when the
//     caller is present to correct it.

// validityArgs is the validity and provenance tail every writer tool shares:
// ghost_memory_save, ghost_save_global and ghost_memory_update each embed it
// rather than repeating six fields and six descriptions, so the argument a
// caller reads in one tool's schema is worded identically in the other two and
// cannot drift into meaning two things.
type validityArgs struct {
	ValidFrom  any    `json:"valid_from,omitempty" jsonschema:"When this memory became true. RFC 3339 (2026-10-01T09:00:00Z) or a date (2026-10-01). ghost_memory_search stops returning the memory until then. Omit for a memory that is true as far as anyone knows."`
	ValidUntil any    `json:"valid_until,omitempty" jsonschema:"When this memory stops being true. Same formats as valid_from. ghost_memory_search then stops returning it rather than showing a stale claim — use it for a policy, an endpoint, or a migration with a real expiry. The surfaces that run the context pipeline do the same: ghost_project_context, the ghost://memories/global resource and the session-start block withhold it too, and a project context whose memories have ALL retired says they were withheld rather than that nothing was ever saved — including when the block is not empty, since the block also carries the _global rows and a block of those alone would otherwise read as the project's. The two browsing surfaces (ghost_memories_list and ghost_search_all) still show the memory, marked 'expired', because they browse rather than filter. A stamp can be replaced but not removed. Omit for durable knowledge."`
	VerifiedAt any    `json:"verified_at,omitempty" jsonschema:"When someone last checked this claim. Same formats as valid_from. Omit if nobody has re-checked it."`
	Verified   bool   `json:"verified,omitempty" jsonschema:"Set true to record that you checked this claim just now, instead of passing verified_at."`
	Confidence any    `json:"confidence,omitempty" jsonschema:"How much to trust this, from 0 to 1 (e.g. 0.8). Omit when you have no view — a recorded number is a claim about trust, and stage 4 does not score on it yet."`
	SourceRef  string `json:"source_ref,omitempty" jsonschema:"Where this came from: a file path, a commit, a URL (e.g. 'docs/runbook.md', 'abc123', 'https://example.com/spec'). Omit when there is no reference."`
}

// acceptedStampForms is the caller's side of the same contract, in the order the
// error message lists them. The whole-day form is memory.DateStampLayout because
// it is the same string the store's readers accept.
var acceptedStampForms = []string{time.RFC3339, memory.DateStampLayout}

// parseStampArg normalizes one caller-supplied stamp into the stored layout.
//
// endOfDay says whether a bare date is the end of a window rather than its
// start, because the two mean different instants and only one of them is the
// plain reading. A date as a window's START is midnight and needs no adjustment:
// a claim that begins on the 1st begins then. As an END it does, because
// memory.ValidityState's expiry test is a strict "before now" — midnight would
// retire the row from the first instant of the day the caller said it was true
// through, and "valid until 2026-12-31" plainly means the whole of the 31st. The
// stored value
// is therefore the last second of that day, which is the finest a
// second-resolution text column can express: the claim stops one second before
// the day is over rather than a day before it began. A full RFC 3339 stamp is
// taken at the instant the caller wrote, because there they did say which one,
//
// Either way the stored value round-trips: assemble.stampText collapses a
// boundary to a date only when the instant is the one that date form stands for
// on that boundary — on a window's end that is this 23:59:59, and on a start or a
// verification it is midnight, so a 23:59:59 the caller chose there prints in
// full.
//
// nil and an empty string are the same request — "no claim" — and become a nil
// pointer, so a caller that sends "" does not write a claim with no readable
// value. Anything else must parse: the layouts are tried in the order they are
// documented, and a value matching none is an error naming the field, the
// accepted formats and the value itself, because a rejected date is something
// the caller can fix and an unreadable stored one is not.
func parseStampArg(field string, raw any, endOfDay bool) (*string, error) {
	if raw == nil {
		return nil, nil
	}
	s, ok := raw.(string)
	if !ok {
		return nil, fmt.Errorf("%s must be a string timestamp, got %v (%T)", field, raw, raw)
	}
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	for _, layout := range acceptedStampForms {
		if t, err := time.Parse(layout, s); err == nil {
			stored := t.UTC()
			if endOfDay && layout == memory.DateStampLayout {
				stored = stored.Add(24*time.Hour - time.Second)
			}
			out := stored.Format(memory.StoredStampLayout)
			return &out, nil
		}
	}
	return nil, fmt.Errorf("%s must be RFC 3339 (2026-10-01T09:00:00Z) or a date (2026-10-01), got %q", field, s)
}

// parseStampFields normalizes the validity triple from one writer's arguments.
//
// verified is checked against verified_at here rather than silently preferring
// one: the two disagree by construction, since one means "now" and the other
// means a moment the caller chose, and picking either would discard the other
// without a word. verified:false is not a conflict — it is the absence of the
// shortcut, and it states nothing.
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

	// The window is checked on the instants, after normalization, so a date-only
	// boundary compares the same as a timestamped one. This pass judges the
	// arguments alone; memory.CheckWindowOrder is the one rule, and the store
	// re-runs it inside its own transaction against the row as it is at that
	// moment, which is the check that has to hold. Judging the caller's half
	// against a value read before the write would leave two concurrent edits able
	// to each pass against a snapshot the other has already invalidated, and would
	// refuse a verification-only edit on a row whose stored window this caller
	// never touched and may not be able to restate.
	if err := memory.CheckWindowOrder(v, memory.Validity{}); err != nil {
		return memory.Validity{}, err
	}
	return v, nil
}

// parseConfidence normalizes the confidence argument to the stored *float64, or
// nil when the caller stated none.
//
// The range is checked rather than clamped. Provenance.Confidence is a pointer
// precisely so 0.0 is a real rating and nil means "no rating", and a value
// outside [0,1] is not a low belief — it is a broken scale, and clamping it
// into range would invent a rating the caller never gave.
//
// This is the tool boundary's contract, not the column's: memories.confidence is
// a bare REAL with no CHECK, and an imported portable artifact can still carry
// anything (the value-shape detector, #656, is on main; nothing downstream
// corrects such a value either — stage 4's multiplier is pinned at 1.0 — so the
// renderer shows what the row says, and the one place a caller can state a
// rating by hand is the one place that refuses a broken one.
func parseConfidence(raw any) (*float64, error) {
	if raw == nil {
		return nil, nil
	}
	// toFloat64 accepts the stringified forms some clients send, and returns nil
	// for a nil argument, so a client that sent nothing is not an error here.
	f, err := toFloat64(raw, "confidence")
	if err != nil {
		return nil, err
	}
	if f == nil {
		return nil, nil
	}
	// NaN and ±Inf are rejected here rather than relied on not to arrive.
	// toFloat64 checks the two shapes that can carry them as text (a JSON number
	// and a string), while a native float64 reaching this handler is unchecked by
	// it — and a NaN passes every `f < 0 || f > 1` comparison, so it would be
	// stored and then rendered as "confidence NaN".
	if math.IsNaN(*f) || math.IsInf(*f, 0) {
		return nil, fmt.Errorf("confidence must be a finite number between 0 and 1, got %v", *f)
	}
	if *f < 0 || *f > 1 {
		return nil, fmt.Errorf("confidence must be between 0 and 1, got %v", *f)
	}
	return f, nil
}

// writeFields is the resolved tail of a writer's arguments: the validity claim
// and the trust rating, validated together so a caller learns about a
// contradictory window before the store opens a transaction.
type writeFields struct {
	Validity   memory.Validity
	Confidence *float64
	SourceRef  string
}

// resolveWriteFields validates the validity and provenance tail of one writer's
// arguments.
//
// source_ref is passed through as sent: it is stored text with no shape to impose,
// and both guards on it belong to the store, which is the layer the renderer
// depends on. memory.MaxSourceRefLen refuses an over-long one, and
// secret.Detect a credential-shaped one, on the four writers Ghost's own callers
// reach — UpsertWithOptions and UpdateMemoryWithOptions (the writer tools),
// Create (the bench seeders) and ImportMemory (`ghost import`) — because a
// reference is a path, a commit, a URL or a ticket, and a megabyte of it is
// echoed into every answer that touches the row. THREE writers deliberately do not
// reach them: RestoreSnapshot (SQL, from the snapshot table), CreateFromCorpus
// (insertMemory directly) and ReplaceNonManual, which since this PR inherits the
// replaced row's reference onto the row a rewrite becomes — a copy of a value this
// database already bounded rather than new caller text, and filtering there
// deletes the stored row. assemble.SourceRefLabel bounds what a listing PRINTS for
// exactly those three. The
// same shape is the reason memory.MaxAgentLen and assemble.AgentLabel exist for
// the agent printed beside it, though on this path the column can only ever
// hold one of four harness tokens. The tool boundary adds nothing, so there is
// one cap and one message rather than two that could disagree.
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

// isZero reports whether the caller stated nothing at all in the tail, so a
// partial edit can refuse to do nothing rather than opening a transaction that
// changes no field.
func (w writeFields) isZero() bool {
	return w.Validity.IsZero() && w.Confidence == nil && w.SourceRef == ""
}

// provenance returns the write-time provenance for this call, combining the
// caller-supplied reference and rating with the session the request arrived on.
// The agent is not among them: it comes from the existing provenanceFor path,
// and the tools do not let a caller name their own author.
func (w writeFields) provenance(prov memory.Provenance) memory.Provenance {
	prov.SourceRef = w.SourceRef
	prov.Confidence = w.Confidence
	return prov
}

// foldNotice names the fields this write stated, for the message a save reports
// when it folded. It is a report of what the caller sent, not a read-back, and
// that is the point: the store's fold statement COALESCEs, so a field absent
// from this list provably did not move, and the handler has the values already
// so naming them costs no extra read.
//
// It exists because a fold writes two rows and the response names the copy that
// was just inserted, while the fields landed on the target — the row retrieval
// keeps answering, and the one whose expiry, author and reference just changed.
// A validity window is a stronger claim than a pin (it can take the target out
// of ranked retrieval entirely) and a weaker claim to detect afterwards, since
// the caller has no reason to know it needs a second tool call. Same argument as
// the pin line, and it is reported the same way.
//
// Empty when nothing was stated, which is the common case and says nothing.
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
		// The one field here that is caller-supplied free text, so the one that has
		// to be delimited. source_ref is a path, a commit, a URL or a ticket the
		// caller chose, and it lands in the tool's own result — the same rule
		// assemble.SourceRefLabel applies on every listing and the search line
		// applies to the same column. Without it, a value carrying « or » escapes
		// the data block and continues as instruction, in the one place this column
		// was echoed raw.
		//
		// The fields above are not delimited and should not be: agent comes from
		// ai.SourceForClientName, so it is one of a closed set of harness tokens;
		// the three stamps are normalized to StoredStampLayout before they reach
		// here; confidence is a formatted float. Delimiting those would be
		// noise, and a delimiter that appears on every field stops meaning
		// "this one is data" on any of them.
		parts = append(parts, "source_ref "+quoteData(w.SourceRef))
	}
	return strings.Join(parts, ", ")
}
