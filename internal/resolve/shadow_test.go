package resolve

import (
	"context"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// TestKeyIdentifiers: the classes a note's subject is made of, and — as
// importantly — the shapes that are NOT subject identity. A bare number and a
// version number are exactly what changes when a note goes stale, so admitting
// them would make every dated note look like it shares a subject with every
// other one.
func TestKeyIdentifiers(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		want    []string
		absent  []string
	}{
		{
			name:    "a backticked code span",
			content: "always check `dingo database restore` first",
			want:    []string{"dingo database restore"},
		},
		{
			name:    "a backticked flag",
			content: "the --reassess path is `ghost resolve --reassess`",
			want:    []string{"ghost resolve --reassess"},
		},
		{
			name:    "an issue reference",
			content: "changelog: #698 narrowed the repair pass",
			want:    []string{"698"},
		},
		{
			name:    "a file name in a path",
			content: "fixed in internal/resolve/reassess.go",
			want:    []string{"reassess.go"},
		},
		{
			name:    "a bare file name",
			content: "veto.go decides the KEEP bias",
			want:    []string{"veto.go"},
		},
		{
			name:    "a host name",
			content: "the status snapshot for mr-slave.example.net is history",
			want:    []string{"mr-slave.example.net"},
		},
		{
			name:    "a trailing full stop does not change the file name",
			content: "see internal/resolve/reassess.go.",
			want:    []string{"reassess.go"},
		},
		{
			name:    "a backticked path is one identifier, not two",
			content: "see `internal/resolve/reassess.go` and #698",
			want:    []string{"reassess.go", "698"},
			// The full path and its base name are the same file, so a note
			// naming one file must not carry two identifiers for it: the floor
			// counts identifiers, and a file counted twice would satisfy it
			// alone.
			absent: []string{"internal/resolve/reassess.go"},
		},
		{
			name:    "a path is a file name, not a host name",
			content: "edit internal/resolve/reassess.go, not the e2e copy",
			want:    []string{"reassess.go"},
			absent:  []string{"internal/resolve/reassess.go"},
		},
		{
			name:    "a backticked span carrying an issue reference is one identifier",
			content: "changelog: always run `dingo restore #698` on the first spindle",
			want:    []string{"698"},
			// The span and the reference are one thing. Both being identifiers
			// would let a single shared issue reference satisfy a floor of two —
			// the same double count a path used to cause.
			absent: []string{"dingo restore #698"},
		},
		{
			name:    "a bare flag span is still an identifier of its own",
			content: "always pass `--reassess` before a repair",
			want:    []string{"--reassess"},
		},
		{
			name:    "a host is not a file name",
			content: "mr-slave.example.net is the only host with a spare spindle",
			want:    []string{"mr-slave.example.net"},
			absent:  []string{"net"},
		},
		{
			name:    "a dotted directory is still just its base name",
			content: "the scratch writer lives in internal/pkg.io, not in cmd",
			want:    []string{"pkg.io"},
			absent:  []string{"internal/pkg.io"},
		},
		{
			name:    "ordinary prose names nothing",
			content: "Never commit to main directly — feature branches and a PR.",
			want:    nil,
		},
		{
			name:    "a bare number is not subject identity",
			content: "the 2222 relay was abandoned",
			want:    nil,
		},
		{
			name:    "a version is not subject identity",
			content: "shipped in 0.36.0 and 1.2.3",
			want:    nil,
		},
		{
			name:    "a sentence abbreviation is not a host",
			content: "read the docs, e.g. the veto list",
			want:    nil,
		},
		{
			name:    "a hyphenated word is not a host",
			content: "re-bootstrap the dingo-core-mithril-sync unit",
			want:    nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := keyIdentifiers(tc.content)
			for _, want := range tc.want {
				if !got[want] {
					t.Errorf("keyIdentifiers(%q) = %v, want it to carry %q", tc.content, got, want)
				}
			}
			for _, absent := range tc.absent {
				if got[absent] {
					t.Errorf("keyIdentifiers(%q) = %v, must not carry %q", tc.content, got, absent)
				}
			}
			if tc.want == nil && len(got) != 0 {
				t.Errorf("keyIdentifiers(%q) = %v, want nothing", tc.content, got)
			}
		})
	}
}

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

// TestReassessShadowedNoteCanStillComeBackResolved: deferring the veto only
// moves the decision, it does not pre-judge it. A shadowed note the classifier
// calls RESOLVED keeps its resolved_at — the floor here is the classifier, not
// the veto, and a rule that un-hid every note a newer memory touched would be
// the same bug with the sign flipped.
func TestReassessShadowedNoteCanStillComeBackResolved(t *testing.T) {
	changelog := memory.Memory{ID: "a1a1a1a1b2b2c2c2d3d3e3e3f3f3a1a1", Category: "changelog", UpdatedAt: "2026-09-01 00:00:00",
		Content: "changelog 0.36.0: always check `internal/resolve/reassess.go` before a repair; #698 landed"}
	newer := memory.Memory{ID: "e5e5e5e5f6f6a6a6b7b7c7c7d7d7e5e5", Category: "changelog", UpdatedAt: "2026-09-20 00:00:00",
		Content: "#698 is closed: internal/resolve/reassess.go now takes --only, so the 0.36.0 note is history"}
	store := &fakeStore{alreadyResolved: []memory.Memory{changelog}, candidates: []memory.Memory{newer}}
	cls := &fakeClassifier{drop: map[string]bool{changelog.Content: true}}

	res, reKept, err := Reassess(context.Background(), store, cls, "proj", true, Scope{}, nil)
	if err != nil {
		t.Fatalf("Reassess: %v", err)
	}
	if cls.calls != 1 {
		t.Fatalf("classifier calls = %d, want 1", cls.calls)
	}
	if res.StillResolved != 1 || res.Vetoed != 0 {
		t.Errorf("StillResolved = %d Vetoed = %d, want 1 and 0", res.StillResolved, res.Vetoed)
	}
	if len(reKept) != 0 || len(store.cleared) != 0 {
		t.Errorf("reKept = %v cleared = %v, want the note left resolved", reKept, store.cleared)
	}
}

// TestReassessShadowReadsOnlyTheUnresolvedPool: the pool is the same one Run
// draws corrections from, and for the same reason. A newer memory that has
// itself been resolved asserts nothing to the next ordinary pass, and a veto
// that deferred to it would send notes to the classifier on the strength of a
// row nothing will ever act on again. The resolved note here carries no
// imperative of its own, so it is the only note the classifier is asked about —
// which is what makes "the changelog was not deferred" observable.
func TestReassessShadowReadsOnlyTheUnresolvedPool(t *testing.T) {
	changelog := memory.Memory{ID: "a1a1a1a1b2b2c2c2d3d3e3e3f3f3a1a1", Category: "changelog", UpdatedAt: "2026-09-01 00:00:00",
		Content: "changelog: 0.36.0 always check `internal/resolve/reassess.go` before a repair; #698 landed"}
	resolvedNewer := memory.Memory{ID: "e5e5e5e5f6f6a6a6b7b7c7c7d7d7e5e5", Category: "changelog", UpdatedAt: "2026-09-20 00:00:00",
		Content: "#698 now takes --only in internal/resolve/reassess.go; the 0.36.0 entry is history"}
	store := &fakeStore{alreadyResolved: []memory.Memory{changelog, resolvedNewer}}
	cls := &fakeClassifier{}

	res, _, err := Reassess(context.Background(), store, cls, "proj", true, Scope{}, nil)
	if err != nil {
		t.Fatalf("Reassess: %v", err)
	}
	if res.Vetoed != 1 {
		t.Errorf("res.Vetoed = %d, want 1 — a resolved newer note must not defer the veto", res.Vetoed)
	}
	if !eqStrings(cls.askedFor, []string{resolvedNewer.Content}) {
		t.Errorf("the classifier was asked about %v, want only the note that carries no imperative", cls.askedFor)
	}
}

// TestRunVetoIsUnchangedByTheShadowRule: the rule is the repair path's alone.
// On the ordinary pass the same veto reads in the opposite direction — it keeps
// a note out of the resolution — where a false veto costs one noisy memory in
// the ranked surface and a later pass can still bury it. On the repair pass it
// un-hides, which is why only that path pays for a call.
//
// The same pair the reassess tests use, with the older note carrying
// "changelog:" so the ordinary pass considers it at all, and the newer one
// carrying no resolution keyword so the only candidate is the note under test.
func TestRunVetoIsUnchangedByTheShadowRule(t *testing.T) {
	changelog := memory.Memory{ID: "a1a1a1a1b2b2c2c2d3d3e3e3f3f3a1a1", Category: "changelog",
		Content: "changelog: 0.36.0 always check `internal/resolve/reassess.go` before a repair; #698 landed"}
	newer := memory.Memory{ID: "e5e5e5e5f6f6a6a6b7b7c7c7d7d7e5e5", Category: "changelog", UpdatedAt: "2026-09-20 00:00:00",
		Content: "#698 now takes --only in internal/resolve/reassess.go; the 0.36.0 entry is history"}
	store := &fakeStore{candidates: []memory.Memory{changelog, newer}}
	cls := &fakeClassifier{}

	res, confirmed, err := Run(context.Background(), store, cls, "proj", true, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Candidates != 1 {
		t.Fatalf("res.Candidates = %d, want 1 — only the changelog carries a resolution keyword", res.Candidates)
	}
	if res.Vetoed != 1 {
		t.Errorf("res.Vetoed = %d, want 1: the ordinary pass's veto is unchanged", res.Vetoed)
	}
	if cls.calls != 0 || len(confirmed) != 0 || len(store.resolved) != 0 {
		t.Errorf("calls = %d confirmed = %v resolved = %v, want the note settled KEEP for free", cls.calls, confirmed, store.resolved)
	}
}
