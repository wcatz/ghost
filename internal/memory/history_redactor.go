package memory

import (
	"fmt"

	"github.com/wcatz/ghost/internal/secret"
)

// This is the wiring `internal/memory/history.go` was left for: #664 built
// memory_history, and left `redactHistoryContent` as the identity function with a
// `TODO(#656)` for the change that would install a detector through it. #656 has
// landed — the write-path value guard covers every write path — and this file
// fills that seam: init() below installs redactHistoryContent through
// setHistoryRedactor, so every history row's content is passed through
// `secret.Detect` before it is stored, which is what the append path's comments
// claimed while it was still untrue.

// redactHistoryContent is the filter a history row's content column is written
// through. It REPLACES rather than refuses, which is the seam's requirement and
// not a detail: a refusal would fail the user's own write over something only
// the history can see, and the text being filtered is a row that already exists.
//
// Why whole-content and not a span. A span-precise redactor would need the
// detector to report where each credential is, which means a capture-group per
// rule across the whole rules table plus offsets from all three post-table passes
// — and `secret.Finding` carries a rule name and a label, deliberately, because
// its other consumer is a message that must not quote the value. So the honest
// options today are "replace the content" and "keep the credential", and only one
// of those is a redaction.
//
// The trade, stated so a future author does not have to guess it: a FALSE POSITIVE
// now costs one history entry's text, where before the guard landed it cost
// nothing. That is the right side to err on — the live row still holds the
// current text, and the entry still records that a change happened, when, and by
// whom — but it is a real cost and not a free win.
//
// Why this is defence in depth rather than a live control. Every writer refuses a
// credential on the way in, so a credential can only be in a history row if it was
// stored before that guard existed. Wiring the seam cannot introduce a new leak; it
// can only close one already on disk. That is what makes the coarse redaction
// above acceptable: on a store with no pre-guard rows this filter never fires.
func redactHistoryContent(content string) string {
	f, ok := secret.Detect(content)
	if !ok {
		return content
	}
	// The rule and label are code constants, never caller text, and the value is
	// not quoted — this string is printed by `ghost history` and read by a person
	// deciding whether to purge, so it has to say what was removed without
	// reproducing it.
	return fmt.Sprintf(
		"<redacted: %s (%s). This earlier version of the memory held a value of that "+
			"shape, so its text is not kept. The live memory row is authoritative; "+
			"`ghost history purge` removes these entries.>",
		f.Label, f.Rule)
}

// The cost, because it is not free and the next author will assume it is: the
// filter runs on EVERY appended row, whether or not anything is redacted, because
// the only sound way to know whether there is a credential is to look. Measured at
// ~1.1 ms for a 2 KB memory and ~3.6 ms at the 8 KB content cap, per row, inside
// the write transaction — a batched append pays it once per id. Skipping the
// detector on rows that "look clean" is not available: a prefilter with a false
// negative is a silent leak — the same lesson the detector's own keyword
// prefilter learned while #656 was being built.
//
// Installing here rather than in history.go keeps the two apart on purpose: this is
// the consumer half of #664's seam and that file is the mechanism half, and a
// conflict between them on the next rebase should be a decision rather than a
// merge. init() ordering does not matter — history.go registers the SQL function
// in its own init and the callback reads historyRedactor.filter at CALL time, so a
// filter installed after it still applies.
func init() { setHistoryRedactor(redactHistoryContent) }
