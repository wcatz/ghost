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
	ValidUntil any    `json:"valid_until,omitempty" jsonschema:"When this memory stops being true. Same formats as valid_from. ghost_memory_search then stops returning it rather than showing a stale claim — use it for a policy, an endpoint, or a migration with a real expiry. The browsing surfaces (ghost_memories_list, ghost_search_all, ghost_project_context) still show the memory, marked 'expired', because they do not filter on this yet; the session-start block shows it with no marker at all, because it renders its own rows. A stamp can be replaced but not removed. Omit for durable knowledge."`
	VerifiedAt any    `json:"verified_at,omitempty" jsonschema:"When someone last checked this claim. Same formats as valid_from. Omit if nobody has re-checked it."`
	Verified   bool   `json:"verified,omitempty" jsonschema:"Set true to record that you checked this claim just now, instead of passing verified_at."`
	Confidence any    `json:"confidence,omitempty" jsonschema:"How much to trust this, from 0 to 1 (e.g. 0.8). Omit when you have no view — a recorded number is a claim about trust, and stage 4 does not score on it yet."`
	SourceRef  string `json:"source_ref,omitempty" jsonschema:"Where this came from: a file path, a commit, a URL (e.g. 'docs/runbook.md', 'abc123', 'https://example.com/spec'). Omit when there is no reference."`
}

// acceptedStampForms is the caller's side of the same contract, in the order the
// error message lists them.
var acceptedStampForms = []string{time.RFC3339, "2006-01-02"}

// parseStampArg normalizes one caller-supplied stamp into the stored layout.
//
// nil and an empty string are the same request — "no claim" — and become a nil
// pointer, so a caller that sends "" does not write a claim with no readable
// value. Anything else must parse: the layouts are tried in the order they are
// documented, and a value matching none is an error naming the field, the
// accepted formats and the value itself, because a rejected date is something
// the caller can fix and an unreadable stored one is not.
func parseStampArg(field string, raw any) (*string, error) {
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
			stored := t.UTC().Format(memory.StoredStampLayout)
			return &stored, nil
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
	if v.ValidFrom, err = parseStampArg("valid_from", args.ValidFrom); err != nil {
		return memory.Validity{}, err
	}
	if v.ValidUntil, err = parseStampArg("valid_until", args.ValidUntil); err != nil {
		return memory.Validity{}, err
	}
	if v.VerifiedAt, err = parseStampArg("verified_at", args.VerifiedAt); err != nil {
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
	// boundary compares the same as a timestamped one.
	if err := checkWindowOrder(v, memory.Validity{}); err != nil {
		return memory.Validity{}, err
	}
	return v, nil
}

// readStamp reads a stored validity stamp over every layout the store's readers
// accept, reporting whether it was readable at all. It mirrors
// assemble.parseStamp over the same memory.StampLayouts, and exists here so a
// writer judging a value it did not store reads it exactly as a reader will: one
// list of layouts, one owner, no writer-only dialect.
func readStamp(s string) (time.Time, bool) {
	for _, layout := range memory.StampLayouts {
		if at, err := time.Parse(layout, s); err == nil {
			return at, true
		}
	}
	return time.Time{}, false
}

// checkWindowOrder refuses a window that does not end after it starts.
//
// stored is what the row already holds, which matters on the update path: a save
// carries both boundaries or neither, but an edit that restates one boundary
// against a row holding the other can create a contradiction the arguments alone
// do not show. A row saved with valid_from 2026-01-01 and then updated with
// valid_until 2020-01-01 has a window that ends before it starts, and stage 2
// reads `until` first and drops the row as expired — a claim that silently
// disappears from every search with no error anywhere. The rule is applied to
// the effective pair, so it is the same rule whatever produced each half.
//
// Only a contradiction is refused. A window that has already closed, or one that
// has not opened, is a legitimate thing to record: it is what a caller writes to
// say a claim is over, or not yet in force.
func checkWindowOrder(v, stored memory.Validity) error {
	from, until := v.ValidFrom, v.ValidUntil
	if from == nil {
		from = stored.ValidFrom
	}
	if until == nil {
		until = stored.ValidUntil
	}
	if from == nil || until == nil {
		return nil
	}
	fromAt, fromReadable := readStamp(*from)
	untilAt, untilReadable := readStamp(*until)
	if !fromReadable || !untilReadable {
		// Both halves came through parseStampArg, which accepts nothing
		// unparseable, so a value that fails here is one this build stored — an
		// imported artifact, a hand edit, or the whole-day form a reader accepts
		// and a writer never produces. Read it the way the reader does before
		// giving up: a check that only understood the shape this build writes
		// would wave through a contradiction on every row stored as a bare date.
		//
		// A value no layout reads is genuinely not a claim, and stage 2 treats it
		// as unset — so there is nothing here to contradict, and refusing would
		// make an unrelated edit fail on a row Ghost cannot interpret.
		return nil
	}
	if !untilAt.After(fromAt) {
		return fmt.Errorf("valid_until %s is not after valid_from %s — a window must end after it starts, or a claim that is true for no time at all", *until, *from)
	}
	return nil
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
// anything (the value-shape detector that would catch it, #656, is not on main).
// Nothing downstream corrects such a value either — stage 4's multiplier is
// pinned at 1.0 — so the renderer shows what the row says, and the one place a
// caller can state a rating by hand is the one place that refuses a broken one.
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

// MaxSourceRefLen is the byte cap on a source reference. A reference is a path, a
// commit, a URL or a ticket — a few hundred bytes at the outside — and the
// field is caller-supplied text that the renderer prints on every listing, so an
// unbounded one is a megabyte echoed into every answer that touches the row.
// memory.MaxContentLen guards the content path for the same reason; this is the
// same guard at the size this field is actually used at.
//
// A value over the cap is refused rather than truncated. Clamping is right for
// content, where the tail is prose and a marker in its place is honest; a
// truncated path or URL is a different reference, and a wrong one that looks
// right is worse than an error the caller can see.
const MaxSourceRefLen = 512

// resolveWriteFields validates the validity and provenance tail of one writer's
// arguments.
//
// source_ref is checked for length and otherwise passed through as sent: it is
// stored text with no shape to impose, and the renderer delimits it the way it
// delimits content so a value carrying its own delimiter cannot escape the field.
// It is not run through a value-shape check here, because the store does that:
// every write path consults secret.Detect over the fields a caller supplies, and
// source_ref is one of them on all three (#656). The two checks sit where each
// can be right — the bound is an argument-shape rule, and the shape test needs the
// detector and the row.
func resolveWriteFields(args validityArgs) (writeFields, error) {
	var out writeFields
	var err error
	if out.Validity, err = parseStampFields(args); err != nil {
		return writeFields{}, err
	}
	if out.Confidence, err = parseConfidence(args.Confidence); err != nil {
		return writeFields{}, err
	}
	if len(args.SourceRef) > MaxSourceRefLen {
		return writeFields{}, fmt.Errorf("source_ref must be at most %d bytes, got %d — it is a file path, commit or URL, not a document", MaxSourceRefLen, len(args.SourceRef))
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
