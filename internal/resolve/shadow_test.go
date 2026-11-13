package resolve

import (
	"context"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// TestReassessVetoYieldsToANewerNoteOnTheSameIdentifiers is the shape #698
// measured: a changelog that says "always check X" reads as a standing rule, so
// the veto un-hides it — into every session's injection, where it is a
// completed note about a closed change. A newer unresolved memory naming the
// same PR and the same file is the evidence that it is dated, so the note goes
// to the classifier instead of being settled for free.
//
// The direction matters as much as the rule: the newer note has to be newer.
// An older memory sharing the identifiers says the note was written after it,
// which is the opposite of superseded.
func TestReassessVetoYieldsToANewerNoteOnTheSameIdentifiers(t *testing.T) {
	for _, tc := range []struct {
		name       string
		changelog  memory.Memory
		newer      []memory.Memory
		wantShadow bool
	}{
		{
			name: "a changelog carrying always, with a newer note on the same PR and file",
			changelog: memory.Memory{ID: "a1a1a1a1b2b2c2c2d3d3e3e3f3f3a1a1", Category: "changelog", UpdatedAt: "2026-09-01 00:00:00",
				Content: "changelog 0.36.0: always check `internal/resolve/reassess.go` before a repair; #698 landed"},
			newer: []memory.Memory{{ID: "e5e5e5e5f6f6a6a6b7b7c7c7d7d7e5e5", Category: "changelog", UpdatedAt: "2026-09-20 00:00:00",
				Content: "#698 is closed: internal/resolve/reassess.go now takes --only, so the 0.36.0 note is history"}},
			wantShadow: true,
		},
		{
			name: "a genuine standing rule, with nothing newer about it",
			changelog: memory.Memory{ID: "a1a1a1a1b2b2c2c2d3d3e3e3f3f3a1a1", Category: "gotcha", UpdatedAt: "2026-09-01 00:00:00",
				Content: "Never commit to main directly — feature branches and a PR, always."},
			newer: []memory.Memory{
				{ID: "e5e5e5e5f6f6a6a6b7b7c7c7d7d7e5e5", Category: "changelog", UpdatedAt: "2026-09-20 00:00:00",
					Content: "changelog: #700 renamed the release workflow"},
			},
			wantShadow: false,
		},
		{
			name: "one shared identifier is not enough",
			changelog: memory.Memory{ID: "a1a1a1a1b2b2c2c2d3d3e3e3f3f3a1a1", Category: "changelog", UpdatedAt: "2026-09-01 00:00:00",
				Content: "changelog: always check #698 before touching the veto"},
			newer: []memory.Memory{{ID: "e5e5e5e5f6f6a6a6b7b7c7c7d7d7e5e5", Category: "changelog", UpdatedAt: "2026-09-20 00:00:00",
				Content: "#698 is closed; the veto now also defers to a newer note. See internal/resolve/reassess.go"}},
			wantShadow: false,
		},
		{
			name: "the same identifiers in an OLDER note are the wrong direction",
			changelog: memory.Memory{ID: "a1a1a1a1b2b2c2c2d3d3e3e3f3f3a1a1", Category: "changelog", UpdatedAt: "2026-09-20 00:00:00",
				Content: "changelog 0.36.0: always check `internal/resolve/reassess.go` before a repair; #698 landed"},
			newer: []memory.Memory{{ID: "e5e5e5e5f6f6a6a6b7b7c7c7d7d7e5e5", Category: "changelog", UpdatedAt: "2026-09-01 00:00:00",
				Content: "#698 is closed: internal/resolve/reassess.go now takes --only, so the 0.36.0 note is history"}},
			wantShadow: false,
		},
		{
			name: "a rule with no identifiers of its own can never be shadowed",
			changelog: memory.Memory{ID: "a1a1a1a1b2b2c2c2d3d3e3e3f3f3a1a1", Category: "gotcha", UpdatedAt: "2026-09-01 00:00:00",
				Content: "always run the full test suite before committing"},
			newer: []memory.Memory{{ID: "e5e5e5e5f6f6a6a6b7b7c7c7d7d7e5e5", Category: "changelog", UpdatedAt: "2026-09-20 00:00:00",
				Content: "#698 and internal/resolve/reassess.go are both history now"}},
			wantShadow: false,
		},
		{
			name: "one shared file is a coincidence, not a subject",
			changelog: memory.Memory{ID: "a1a1a1a1b2b2c2c2d3d3e3e3f3f3a1a1", Category: "gotcha", UpdatedAt: "2026-09-01 00:00:00",
				Content: "NEVER edit internal/resolve/reassess.go while a repair is running"},
			newer: []memory.Memory{{ID: "e5e5e5e5f6f6a6a6b7b7c7c7d7d7e5e5", Category: "changelog", UpdatedAt: "2026-09-20 00:00:00",
				Content: "internal/resolve/reassess.go got a new comment; see also internal/resolve/scope.go"}},
			wantShadow: false,
		},
		{
			name: "one shared issue reference is a coincidence too",
			changelog: memory.Memory{ID: "a1a1a1a1b2b2c2c2d3d3e3e3f3f3a1a1", Category: "changelog", UpdatedAt: "2026-09-01 00:00:00",
				Content: "changelog: always run `dingo restore #698` on the first spindle"},
			newer: []memory.Memory{{ID: "e5e5e5e5f6f6a6a6b7b7c7c7d7d7e5e5", Category: "changelog", UpdatedAt: "2026-09-20 00:00:00",
				Content: "#698 is done: the `dingo restore #698` note is history, and two spindles is one too many"}},
			wantShadow: false,
		},
		{
			name: "an open marker yields the same way an imperative does",
			changelog: memory.Memory{ID: "a1a1a1a1b2b2c2c2d3d3e3e3f3f3a1a1", Category: "changelog", UpdatedAt: "2026-09-01 00:00:00",
				Content: "still open: the #698 follow-up in internal/resolve/reassess.go was never finished"},
			newer: []memory.Memory{{ID: "e5e5e5e5f6f6a6a6b7b7c7c7d7d7e5e5", Category: "changelog", UpdatedAt: "2026-09-20 00:00:00",
				Content: "#698 is closed and internal/resolve/reassess.go ships --only today"}},
			wantShadow: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{alreadyResolved: []memory.Memory{tc.changelog}, candidates: tc.newer}
			// The classifier KEEPs it, so a shadowed note is observably
			// classified: an un-hiding verdict either way, and the count of
			// Vetoed is what says which path produced it.
			cls := &fakeClassifier{}

			res, reKept, err := Reassess(context.Background(), store, cls, "proj", true, Scope{}, nil)
			if err != nil {
				t.Fatalf("Reassess: %v", err)
			}
			if tc.wantShadow {
				if cls.calls != 1 {
					t.Errorf("classifier calls = %d, want 1 — a shadowed note must be asked about, not settled free", cls.calls)
				}
				if res.Vetoed != 0 {
					t.Errorf("res.Vetoed = %d, want 0: the veto did not settle this note by itself", res.Vetoed)
				}
			} else {
				if cls.calls != 0 {
					t.Errorf("classifier calls = %d, want 0 — the veto stands and the note costs nothing", cls.calls)
				}
				if res.Vetoed != 1 {
					t.Errorf("res.Vetoed = %d, want 1: the veto settles this note on its own", res.Vetoed)
				}
			}
			if len(reKept) != 1 || reKept[0].ID != tc.changelog.ID {
				t.Errorf("reKept = %v, want the one note (both paths agree it is no longer resolved evidence)", reKept)
			}
		})
	}
}
