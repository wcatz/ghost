package assemble

import (
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// TestValidityStateOfAgreesWithTheOneRule is the invariant #583 exists to
// establish, and it is a test about AGREEMENT rather than about any one value.
//
// memory.ValidityState is now the only place the question "may this row be used"
// is decided, and two callers have to reach the same answer: this package's
// stage-2 verdict over a traced candidate, and ValidityStateOf over the stored
// triple that the browsing surfaces read directly. Those were two implementations
// of the rule for most of the branch's life, and they did not merely duplicate
// each other — they DISAGREED. This file's parseStampPtr treated a stamp that
// parses to the zero time as unreadable, while the store's ParseStamp reports it
// as readable, because time.Parse accepts it and the store's own
// TestImportVerification asserts that it should. A row whose valid_until is
// literally 0001-01-01 00:00:00 was therefore dropped by the search assembler and
// still listed as unset by every browsing surface: the same row, two answers,
// which is the defect this change exists to remove rather than a tidier factoring
// of it.
//
// The case is in the table because it caught that, and because it is the general
// shape of the hazard. The columns are unconstrained TEXT, so any value
// time.Parse accepts is a value the rule must have an opinion about, and the zero
// instant is the one a reader is most natural to mistake for "no claim".
func TestValidityStateOfAgreesWithTheOneRule(t *testing.T) {
	zeroStamp := "0001-01-01 00:00:00"
	openStamp := "2030-01-01 00:00:00"
	closedStamp := "2020-01-01 00:00:00"
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name             string
		from, until, ver *string
		want             string
		why              string
	}{
		{name: "nothing stated", want: "unset", why: "the common case, and it must not read as expired"},
		{
			name: "open window", until: &openStamp, ver: &openStamp, want: "valid",
			why: "a window that has not closed and has been verified",
		},
		{
			name: "closed window", until: &closedStamp, ver: &closedStamp, want: "expired",
			why: "a window that has closed, whatever else it says",
		},
		{
			name: "not yet valid", from: &openStamp, until: &openStamp, want: "future",
			why: "a window that has not opened",
		},
		{
			name: "stated but unverified", until: &openStamp, want: "unverified",
			why: "a currency the store can read but nobody has re-checked",
		},
		{
			name: "valid_until is the zero instant", until: &zeroStamp, want: "expired",
			why: "the value time.Parse accepts and a stamp that failed to parse looks identical to; its window " +
				"closed at the zero instant, so it is expired rather than absent",
		},
		{
			name: "valid_from is the zero instant", from: &zeroStamp, until: &openStamp, ver: &openStamp,
			want: "valid",
			why:  "a window that began at the zero instant is open, not future",
		},
		{
			name: "unreadable window", until: strptr("the ides of march"), want: "unset",
			why: "a value no layout reads is no claim, and the trace is where the value itself is reported",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// readValidity is the stage-2 verdict the pipeline itself uses, over a
			// candidate rather than the stored triple. Asserting against it rather
			// than against the rule directly is the point: it is the OTHER caller,
			// and the two agreeing is the property.
			candidate := memory.Candidate{Memory: memory.Memory{
				ValidFrom: tc.from, ValidUntil: tc.until, VerifiedAt: tc.ver,
			}}
			if got := readValidity(candidate, now).state; got != tc.want {
				t.Errorf("readValidity = %q, want %q: %s", got, tc.want, tc.why)
			}
			got := ValidityStateOf(tc.from, tc.until, tc.ver, now)
			if got != tc.want {
				t.Errorf("ValidityStateOf = %q, want %q: %s", got, tc.want, tc.why)
			}
			state, _ := memory.ValidityState(tc.from, tc.until, tc.ver, now)
			if got != state {
				t.Errorf("ValidityStateOf = %q but memory.ValidityState = %q: the search assembler and the "+
					"browsing surfaces reach different answers about the same row, which is the two-copies "+
					"failure this delegation exists to prevent", got, state)
			}

			// The third leg, and the one that was missed twice: the LABEL has to
			// agree with the state. A surface that decided "expired" and then
			// printed no marker has told the reader the row is current, and the
			// marker is the only thing that settles it — a browsing surface has not
			// run stage 2, so it gets no other signal that the claim is retired.
			//
			// So for a row that states ANYTHING readable, the label must be
			// non-empty, and it must carry the state's own word.
			label := ValidityLabel(state, tc.from, tc.until, tc.ver)
			if anyReadable(tc.from, tc.until, tc.ver) {
				if label == "" {
					t.Errorf("ValidityLabel = \"\" for a row whose verdict is %q: the row states a readable "+
						"claim and was judged %s, so the surface must say so — a decided verdict with no "+
						"marker reads as current", state, state)
				}
				if word := stateWord(state); word != "" && !strings.Contains(label, word) {
					t.Errorf("ValidityLabel = %q, want it to carry %q for a row the rule calls %s: the state "+
						"and the label are the same decision rendered twice, and a reader who sees only one "+
						"of them is being told something the other contradicts", label, word, state)
				}
			} else if label != "" {
				t.Errorf("ValidityLabel = %q for a row that states nothing readable, want \"\": there is no "+
					"claim to date, so a label would assert one", label)
			}
		})
	}
}

func strptr(s string) *string { return &s }

// anyReadable reports whether any of the three is a value the store can read, by
// asking the store's own parser rather than re-implementing readability here —
// which is the mistake this whole test is about.
func anyReadable(values ...*string) bool {
	for _, v := range values {
		if v == nil {
			continue
		}
		if _, ok := memory.ParseStamp(*v); ok {
			return true
		}
	}
	return false
}

// stateWord is the word a label carries for a state, or "" for the states whose
// label is the dates alone. It mirrors validityLabel's own switch on purpose: the
// test is checking that the switch still agrees with the STATE it is handed, so
// deriving the expectation from the same table would be circular — these are the
// literal words, written out.
func stateWord(state string) string {
	switch state {
	case "expired":
		return "expired"
	case "future":
		return "not yet valid"
	case "unverified":
		return "unverified"
	default:
		return ""
	}
}
