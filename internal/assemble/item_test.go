package assemble

import (
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// The renderer is the only place a caller learns what a row claims about itself.
// Stage 2 keeps a row because its window is open, and a row whose window nobody
// has ever checked is a different thing to act on than one checked last month —
// but if the line renders them identically, the column is being stored for
// nobody. These tests pin the field set the item line carries and the one
// surface that has not moved to it yet (ValidityLabel, called by
// internal/mcpserver's formatMemories).

func stamp(s string) *time.Time {
	at, err := time.Parse(memory.StoredStampLayout, s)
	if err != nil {
		panic("test fixture is not a stored stamp: " + s)
	}
	return &at
}

func confidence(f float64) *float64 { return &f }

// A whole-day boundary renders as a date, because that is what a caller states
// and what a reader acts on; a stamp with a time of day keeps it, because then
// the time was the point. Both halves are the same rule, so they are pinned
// together: a renderer that formatted everything through one layout would pass a
// test that only checked midnight.
func TestItemLineRendersValidityDatesAndTimes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		item  Item
		wants []string
	}{
		{
			name:  "open window",
			item:  Item{ValidityState: validityValid, ValidFrom: stamp("2026-01-15 00:00:00"), ValidUntil: stamp("2026-10-01 00:00:00")},
			wants: []string{"valid from 2026-01-15", "until 2026-10-01"},
		},
		{
			name:  "open window nobody checked",
			item:  Item{ValidityState: validityUnverified, ValidFrom: stamp("2026-01-15 00:00:00"), ValidUntil: stamp("2026-10-01 00:00:00")},
			wants: []string{"valid from 2026-01-15", "until 2026-10-01", "unverified"},
		},
		{
			name:  "checked at a moment of the day",
			item:  Item{ValidityState: validityValid, ValidFrom: stamp("2026-01-15 00:00:00"), VerifiedAt: stamp("2026-09-20 09:30:00")},
			wants: []string{"valid from 2026-01-15", "verified 2026-09-20 09:30:00"},
		},
		{
			name:  "verification with no window",
			item:  Item{ValidityState: validityValid, VerifiedAt: stamp("2026-09-20 00:00:00")},
			wants: []string{"verified 2026-09-20"},
		},
		{
			name:  "an open end only",
			item:  Item{ValidityState: validityUnverified, ValidFrom: stamp("2026-01-15 00:00:00")},
			wants: []string{"valid from 2026-01-15", "unverified"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			line := tc.item.Line()
			for _, want := range tc.wants {
				if !strings.Contains(line, want) {
					t.Errorf("line is missing %q: %s", want, line)
				}
			}
		})
	}
}

// The overwhelmingly common row states no window at all, and its line must not
// grow a word for it: every memory written before the columns existed reads
// unset, so an "unset" marker would put a token in front of the whole corpus.
//
// Every assertion here is negative, so on its own this test passes with the
// renderers removed. It is the guard, not the proof: the positive cases above
// show the fields appear when they are recorded, and this one shows the absence
// is not paid for in a marker on every row that has nothing to say.
func TestItemLineSaysNothingWhenNoClaimIsRecorded(t *testing.T) {
	line := Item{Category: "fact", ID: "A1B2", Content: "a durable fact", Importance: 0.7, ValidityState: validityUnset}.Line()
	for _, unwanted := range []string{"valid", "unverified", "confidence", "agent=", "source_ref="} {
		if strings.Contains(line, unwanted) {
			t.Errorf("line carries %q for a row that claims nothing: %s", unwanted, line)
		}
	}
	if !strings.Contains(line, "«a durable fact»") {
		t.Errorf("line lost its content: %s", line)
	}
}

// Provenance is what makes a claim checkable, and the reference is the part that
// names something to check it against. Both are optional, both are delimited —
// source_ref is caller-supplied text on its way into a tool answer, and an
// imported artifact can put anything in agent — and neither is ever scored.
func TestItemLineRendersProvenanceCompactly(t *testing.T) {
	line := Item{
		Category: "gotcha", ID: "A1B2", Content: "the port moved", Importance: 0.9,
		Agent: "opencode", SourceRef: "docs/runbook.md", Confidence: confidence(0.8),
	}.Line()
	for _, want := range []string{"agent=«opencode»", "source_ref=«docs/runbook.md»", "confidence 0.8"} {
		if !strings.Contains(line, want) {
			t.Errorf("line is missing %q: %s", want, line)
		}
	}
}

// A confidence of 0 is a rating, not an absent one — Provenance.Confidence is a
// pointer for exactly that reason — so a renderer testing the float for
// truthiness would drop the one value that can never be defaulted into.
func TestItemLineRendersAZeroConfidence(t *testing.T) {
	line := Item{Category: "fact", ID: "A1B2", Content: "a doubted claim", Importance: 0.5, Confidence: confidence(0)}.Line()
	if !strings.Contains(line, "confidence 0") {
		t.Errorf("line dropped a recorded 0.0 confidence: %s", line)
	}
}

// ValidityLabel is the second entry point into the same formatter, for the
// surfaces that still render memory.Memory. It takes the stored strings because
// the column is unconstrained text, so a row can hold a layout this build does
// not read — and an unreadable value has to render as no claim rather than as a
// date assembled from whatever parsed.
func TestValidityLabelRendersStoredStampsAndIgnoresUnreadableOnes(t *testing.T) {
	from, until, verified := "2026-01-15 00:00:00", "2026-10-01 00:00:00", "2026-09-20 00:00:00"
	if got := ValidityLabel(validityValid, &from, &until, &verified); got != " valid from 2026-01-15 until 2026-10-01 verified 2026-09-20" {
		t.Errorf("ValidityLabel = %q, want the same words Item.Line renders", got)
	}
	if got := ValidityLabel("", nil, nil, nil); got != "" {
		t.Errorf("ValidityLabel over three NULL stamps = %q, want empty", got)
	}
	if got := ValidityLabel("", &from, nil, nil); got != " valid from 2026-01-15" {
		t.Errorf("ValidityLabel = %q, want the single boundary", got)
	}
	if got := ValidityLabel("", ptr("next tuesday"), nil, nil); got != "" {
		t.Errorf("ValidityLabel over an unreadable stamp = %q, want empty — no readable claim is no claim", got)
	}
	// A date-only stamp is the other layout SQLite's date() produces, and a row
	// may hold either; reading one is not a reason to refuse the other.
	if got := ValidityLabel("", ptr("2026-01-15"), nil, nil); got != " valid from 2026-01-15" {
		t.Errorf("ValidityLabel over a date-only stamp = %q, want the date", got)
	}
}

func ptr(s string) *string { return &s }

// A browsing surface has not run stage 2, so a closed window is about to be
// printed in full. Printing it unmarked is the worst available reading of a dated
// row: the date is right there and nothing says what it means. ValidityStateOf is
// how such a surface gets the same verdict the stages would have reached, and
// expired and not-yet-valid are the two states the label has to carry because
// stage 2 drops them everywhere else.
func TestValidityStateOfMatchesWhatTheStagesWouldDecide(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name               string
		from, to, verified *string
		want               string
	}{
		{"closed window", ptr("2024-01-01 00:00:00"), ptr("2020-06-01 00:00:00"), nil, validityExpired},
		{"window not yet open", ptr("2099-01-01 00:00:00"), ptr("2199-12-31 00:00:00"), nil, validityFuture},
		{"open window nobody checked", ptr("2024-01-01 00:00:00"), ptr("2199-12-31 00:00:00"), nil, validityUnverified},
		{"checked current claim", ptr("2024-01-01 00:00:00"), nil, ptr("2026-01-01 00:00:00"), validityValid},
		{"no claim at all", nil, nil, nil, validityUnset},
		{"expiry is today, not yesterday", nil, ptr("2026-09-27 00:00:00"), nil, validityExpired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ValidityStateOf(tc.from, tc.to, tc.verified, now)
			if got != tc.want {
				t.Fatalf("ValidityStateOf = %q, want %q", got, tc.want)
			}
			// The rule ValidityStateOf's own comment promises to share: the state
			// the pipeline reaches for this row must be the state a browsing
			// surface renders. Nothing else in the package pins that agreement, and
			// a reordering of readValidity's switch would leave both functions
			// compiling and quietly disagreeing about which rows are retired.
			if staged := readValidity(memory.Candidate{Memory: memory.Memory{
				ValidFrom: tc.from, ValidUntil: tc.to, VerifiedAt: tc.verified,
			}}, now).state; staged != got {
				t.Errorf("the pipeline reads this row as %q and the renderer as %q", staged, got)
			}
			label := ValidityLabel(got, tc.from, tc.to, tc.verified)
			switch tc.want {
			case validityExpired:
				if !strings.Contains(label, "expired") {
					t.Errorf("a closed window renders unmarked: %q", label)
				}
			case validityFuture:
				if !strings.Contains(label, "not yet valid") {
					t.Errorf("a window that has not opened renders unmarked: %q", label)
				}
			case validityUnverified:
				if !strings.Contains(label, "unverified") {
					t.Errorf("an unchecked window renders unmarked: %q", label)
				}
			case validityUnset:
				if label != "" {
					t.Errorf("a row that claims nothing renders %q, want nothing", label)
				}
			}
		})
	}
}

// An unreadable stamp reads as no claim on a browsing surface, exactly as stage 2
// reads it as unset — the trace is where it is reported, and a renderer has no
// trace. The alternative is assembling a date out of whatever parsed, which is how
// a broken row becomes a wrong one.
func TestValidityLabelIgnoresUnreadableStampsItCannotInterpret(t *testing.T) {
	now := time.Now().UTC()
	bad := ptr("not a date")
	if got := ValidityStateOf(bad, nil, nil, now); got != validityUnset {
		t.Errorf("ValidityStateOf over an unreadable stamp = %q, want unset", got)
	}
	if got := ValidityLabel(validityUnset, bad, nil, nil); got != "" {
		t.Errorf("ValidityLabel rendered an unreadable stamp as %q, want nothing", got)
	}
	if staged := readValidity(memory.Candidate{Memory: memory.Memory{ValidFrom: bad}}, now); staged.state != validityUnset {
		t.Errorf("the pipeline reads an unreadable stamp as %q, want unset", staged.state)
	}
	// A readable end beside an unreadable start is still an expired row: the end
	// is a claim in its own right, and the unreadable half is not evidence
	// against it. Reading the pair as wholly unset would resurrect a retired claim.
	good, older := ptr("2020-01-01 00:00:00"), bad
	if got := ValidityStateOf(older, good, nil, now); got != validityExpired {
		t.Errorf("ValidityStateOf over an unreadable start and a closed end = %q, want expired", got)
	}
}

// The two entry points must not drift. They render the same row through
// different types, and a divergence here is the one thing that makes the shared
// field set a claim rather than a fact.
func TestItemLineAndValidityLabelAgree(t *testing.T) {
	from, until := "2026-01-15 00:00:00", "2026-10-01 00:00:00"
	item := Item{ValidFrom: stamp(from), ValidUntil: stamp(until), ValidityState: validityUnverified}
	if got, want := item.Line(), ValidityLabel(validityUnverified, &from, &until, nil); !strings.Contains(got, want) {
		t.Errorf("Item.Line renders %s, ValidityLabel renders %q: the two entry points disagree", got, want)
	}
}
