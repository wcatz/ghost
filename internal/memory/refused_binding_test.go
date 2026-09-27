package memory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveOrCreateRepoProjectReportsRefusedNameBinding is the write-side
// half of #613: a repository that may not claim the project its name matched
// opens a project of its own instead, and until now the caller could not tell
// that apart from a genuinely new project — while the agent has in fact just
// stopped seeing the context held by the same-named project.
//
// Each case is a refusal that already existed and is still correct; the only
// thing asserted here beyond them is that the refusal is *reported*. The save
// is not lost (the fallback project still records the remote) and the refused
// project is still left alone (it does not record it), so a notice cannot
// become a second way to bind.
func TestResolveOrCreateRepoProjectReportsRefusedNameBinding(t *testing.T) {
	const (
		remote   = "https://github.com/wcatz/infra.git"
		canon    = "github.com/wcatz/infra"
		other    = "https://github.com/someone/infra.git"
		otherCan = "github.com/someone/infra"
	)
	// The recorded checkout a path-mismatch refusal has to disagree with.
	// pathsAgree resolves what it compares, so a directory that does not exist
	// would make that case pass for the wrong reason.
	recorded := existingDir(t, "git", "infra")

	for _, tc := range []struct {
		name     string
		seed     func(t *testing.T, s *Store)
		saving   string
		kind     BindingRefusalKind
		holdIDs  []string
		recorded string
		remote   string
		// advice is what the notice must tell the reader to run, in the order
		// it must appear in: the order is part of the claim, because one of
		// the two orders of the merge and the bind fails.
		advice []string
		// forbidden is what the notice must not suggest, and why each would be
		// wrong for this kind.
		forbidden map[string]string
	}{
		{
			name: "recorded path elsewhere",
			seed: func(t *testing.T, s *Store) {
				if err := s.EnsureProject(context.Background(), "real-infra", recorded, "infra"); err != nil {
					t.Fatalf("EnsureProject: %v", err)
				}
			},
			saving:   existingDir(t, "Downloads", "infra"),
			kind:     RefusedPathMismatch,
			holdIDs:  []string{"real-infra"},
			recorded: recorded,
			// Both commands, in this order. The merge collects what was saved
			// into the project this checkout opened; the bind is what makes it
			// last, because the merge deletes that project along with the
			// repository it recorded — so a merge on its own leaves the next
			// save from this checkout refusing again and splitting once more.
			advice: []string{`ghost project merge "SAVED" "real-infra"`, `ghost project bind "real-infra" "SAVED"`},
		},
		{
			name: "ambiguous name",
			seed: func(t *testing.T, s *Store) {
				for _, id := range []string{"second", "first"} {
					if err := s.EnsureProject(context.Background(), id, "", "infra"); err != nil {
						t.Fatalf("EnsureProject %s: %v", id, err)
					}
				}
			},
			saving:  existingDir(t, "Downloads", "infra"),
			kind:    RefusedAmbiguousName,
			holdIDs: []string{"first", "second"},
			// No bind here, and there must not be one: the project this save
			// used already records the checkout and the repository, so nothing
			// about this save's routing is left to repair. Which candidates are
			// duplicates of each other is the reader's to judge, so the merge
			// is between them and names neither.
			advice:    []string{"ghost project merge <duplicate-id> <survivor-id>"},
			forbidden: map[string]string{"ghost project bind": "the project this save used already records the checkout"},
		},
		{
			name: "already another repository",
			seed: func(t *testing.T, s *Store) {
				if err := s.EnsureProjectWithRepo(context.Background(), "real-infra", "", "infra", other); err != nil {
					t.Fatalf("EnsureProjectWithRepo: %v", err)
				}
			},
			saving: existingDir(t, "Downloads", "infra"),
			kind:   RefusedDifferentRemote,
			// The holder records no path here: a project created by name is the
			// ordinary shape for one that already claims a remote, and a
			// recorded path is not what the conflict turns on.
			holdIDs: []string{"real-infra"},
			remote:  otherCan,
			advice:  []string{},
			// No command at all. These are two repositories: a merge would
			// either hand one project's memories to a project that claims the
			// other repository, or drop the repository this save came from.
			// Nothing here needs repairing either — the project this save used
			// records this checkout and is found by id on the next save — so
			// the answer is which project the reader meant, not how to join them.
			forbidden: map[string]string{
				"ghost project merge": "two different repositories must stay two projects",
				"ghost project bind":  "the project this save used already records this checkout and repository",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			tc.seed(t, s)

			canonical, refused, err := s.ResolveOrCreateRepoProject(
				ctx, tc.saving, "infra", tc.saving, tc.saving, tc.saving, remote,
			)
			if err != nil {
				t.Fatalf("ResolveOrCreateRepoProject: %v", err)
			}

			// The save still has to land somewhere, and the refusal must say
			// where: a caller that cannot name the project it lost cannot
			// recover the context either.
			if canonical != tc.saving {
				t.Errorf("canonical project = %q, want the save's own project %q", canonical, tc.saving)
			}
			if refused == nil {
				t.Fatalf("refused unique-name binding was not reported (canonical %q)", canonical)
			}
			if refused.Kind != tc.kind {
				t.Errorf("refusal kind = %q, want %q", refused.Kind, tc.kind)
			}
			if refused.Name != "infra" {
				t.Errorf("refusal names %q, want the repository name %q", refused.Name, "infra")
			}
			if strings.Join(refused.ProjectIDs, ",") != strings.Join(tc.holdIDs, ",") {
				t.Errorf("refusal candidates = %v, want %v", refused.ProjectIDs, tc.holdIDs)
			}
			if refused.RecordedPath != tc.recorded {
				t.Errorf("refusal recorded path = %q, want %q", refused.RecordedPath, tc.recorded)
			}
			if refused.RecordedRemote != tc.remote {
				t.Errorf("refusal recorded remote = %q, want %q", refused.RecordedRemote, tc.remote)
			}
			if refused.SavedTo != canonical {
				t.Errorf("refusal SavedTo = %q, want the canonical project %q", refused.SavedTo, canonical)
			}

			// The notice is the product of all of this: it has to name what
			// kept the name and where the memory went, or the caller still
			// cannot act on it.
			notice := refused.Notice()
			for _, want := range append([]string{canonical, "infra"}, tc.holdIDs...) {
				if !strings.Contains(notice, want) {
					t.Errorf("notice does not mention %q: %q", want, notice)
				}
			}
			if tc.recorded != "" && !strings.Contains(notice, tc.recorded) {
				t.Errorf("notice does not mention the recorded path %q: %q", tc.recorded, notice)
			}
			if tc.remote != "" && !strings.Contains(notice, tc.remote) {
				t.Errorf("notice does not mention the recorded remote %q: %q", tc.remote, notice)
			}

			// Advice is a claim about what the reader should run, so it is
			// asserted as one: those commands, in that order, and nothing the
			// kind must not name. "SAVED" stands for the project this save went
			// to, which is a filesystem path and so is quoted in the notice.
			rest := notice
			for _, want := range tc.advice {
				at := strings.Index(rest, strings.ReplaceAll(want, "SAVED", canonical))
				if at < 0 {
					t.Errorf("notice does not suggest %q, or not in that order: %q", want, notice)
					continue
				}
				rest = rest[at+len(want):]
			}
			for unwanted, why := range tc.forbidden {
				if strings.Contains(notice, unwanted) {
					t.Errorf("notice suggests %q, which is wrong here because %s: %q", unwanted, why, notice)
				}
			}

			// Reported, not changed: the fallback project carries the
			// repository and the refused ones are still unclaimed.
			var got string
			if err := s.db.QueryRowContext(ctx,
				`SELECT COALESCE(repo_remote, '') FROM projects WHERE id = ?`, canonical).Scan(&got); err != nil {
				t.Fatalf("read fallback project repository: %v", err)
			}
			if got != canon {
				t.Errorf("fallback project recorded repository %q, want %q — the save must not be lost", got, canon)
			}
			for _, id := range tc.holdIDs {
				var held string
				if err := s.db.QueryRowContext(ctx,
					`SELECT COALESCE(repo_remote, '') FROM projects WHERE id = ?`, id).Scan(&held); err != nil {
					t.Fatalf("read refused project %q: %v", id, err)
				}
				if held == canon {
					t.Errorf("refused project %q was bound to %q anyway", id, canon)
				}
			}
		})
	}
}

// TestResolveOrCreateRepoProjectReportsNoRefusalWhenNothingWasRefused is the
// other half: a notice on every save would train its reader to skip it. The
// two shapes that bind nothing and lose nothing must stay silent — a name no
// project records, which is an ordinary first save, and a name whose candidate
// agrees with the directory, which is the ordinary second checkout.
func TestResolveOrCreateRepoProjectReportsNoRefusalWhenNothingWasRefused(t *testing.T) {
	const remote = "https://github.com/wcatz/infra.git"

	t.Run("name matches no project", func(t *testing.T) {
		s := testStore(t)
		ctx := context.Background()
		checkout := existingDir(t, "git", "infra")

		canonical, refused, err := s.ResolveOrCreateRepoProject(
			ctx, checkout, "infra", checkout, checkout, checkout, remote,
		)
		if err != nil {
			t.Fatalf("ResolveOrCreateRepoProject: %v", err)
		}
		if canonical != checkout || refused != nil {
			t.Errorf("first save = (%q, %+v), want a new project at %q and no refusal", canonical, refused, checkout)
		}
	})

	t.Run("candidate recorded path agrees", func(t *testing.T) {
		s := testStore(t)
		ctx := context.Background()
		checkout := existingDir(t, "git", "infra")
		if err := s.EnsureProject(ctx, "real-infra", checkout+string(os.PathSeparator), "infra"); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}

		canonical, refused, err := s.ResolveOrCreateRepoProject(
			ctx, checkout, "infra", checkout, checkout, checkout, remote,
		)
		if err != nil {
			t.Fatalf("ResolveOrCreateRepoProject: %v", err)
		}
		if canonical != "real-infra" {
			t.Errorf("canonical project = %q, want the named project it was allowed to bind", canonical)
		}
		if refused != nil {
			t.Errorf("a bound save reported a refusal: %+v", refused)
		}
	})
}

// TestBindingRefusalNoticeOnNilCaller pins that a caller may append the notice
// without a nil check: ensureProjectFor returns no refusal for every ordinary
// save, and the save message is built for all of them.
//
// It also pins that a refusal with nothing in it renders rather than panics.
// Notice runs on the result of a save that has already been written, so a
// partial refusal from any source is a worse sentence, not a crashed save.
func TestBindingRefusalNoticeOnNilCaller(t *testing.T) {
	var refused *BindingRefusal
	if notice := refused.Notice(); notice != "" {
		t.Errorf("Notice on no refusal = %q, want empty so an ordinary save says nothing extra", notice)
	}
	empty := &BindingRefusal{Kind: RefusedPathMismatch, Name: "infra", SavedTo: "/work/infra"}
	if notice := empty.Notice(); !strings.Contains(notice, "/work/infra") {
		t.Errorf("Notice on a refusal with no candidates = %q, want it to name where the save went", notice)
	}
}

// TestAmbiguousNameRefusalNamesAtMostFiveCandidates bounds the one part of the
// notice that could grow without limit. Same-named projects accumulate exactly
// where a merge accident left them, the notice is returned to the agent that
// made the save and stays in its context, and listing every candidate would
// make the refusal the most expensive line in the result.
func TestAmbiguousNameRefusalNamesAtMostFiveCandidates(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for i := 1; i <= 7; i++ {
		if err := s.EnsureProject(ctx, fmt.Sprintf("copy-%d", i), "", "infra"); err != nil {
			t.Fatalf("EnsureProject copy-%d: %v", i, err)
		}
	}
	saving := existingDir(t, "Downloads", "infra")

	_, refused, err := s.ResolveOrCreateRepoProject(
		ctx, saving, "infra", saving, saving, saving, "https://github.com/wcatz/infra.git",
	)
	if err != nil {
		t.Fatalf("ResolveOrCreateRepoProject: %v", err)
	}
	// The read that finds the candidates is bounded, so the refusal carries
	// the true count beside the few it names: the notice has to be able to say
	// "7 projects" while listing five.
	if refused == nil || refused.CandidateCount != 7 {
		t.Fatalf("refusal = %+v, want a count of all 7 candidates", refused)
	}
	if len(refused.ProjectIDs) != maxNamedCandidates {
		t.Fatalf("refusal named %d candidates, want the cap of %d", len(refused.ProjectIDs), maxNamedCandidates)
	}
	notice := refused.Notice()
	for i := 1; i <= 5; i++ {
		if !strings.Contains(notice, fmt.Sprintf("copy-%d", i)) {
			t.Errorf("notice drops candidate copy-%d: %q", i, notice)
		}
	}
	for _, dropped := range []string{"copy-6", "copy-7"} {
		if strings.Contains(notice, dropped) {
			t.Errorf("notice lists %s past the cap: %q", dropped, notice)
		}
	}
	if !strings.Contains(notice, "2 more") || !strings.Contains(notice, "7 projects") {
		t.Errorf("notice does not report the true count and the remainder: %q", notice)
	}
}

// TestProjectsNamedTxClampsTheLimit pins that a limit which would return
// nothing cannot turn the unique-name decision into a silent duplicate.
// SQLite reads LIMIT 0 as "no rows", so without the clamp the query reports
// that no project records the name, and the caller opens a project of its own
// and binds the repository to it: the same-named project is left behind,
// unbound, with no refusal to report — the one outcome this path exists to
// prevent. A negative limit is no limit at all to SQLite, which is why the
// clamp turns both inputs into the same bound of one.
//
// Two projects share the name so that each input is observable rather than
// merely equal to the fixture: an unclamped -1 returns both rows, an unclamped
// 0 returns none and a count of 0, and a clamped limit of either returns the
// first of them beside the true count of two.
func TestProjectsNamedTxClampsTheLimit(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for _, id := range []string{"second", "first"} {
		if err := s.EnsureProject(ctx, id, "", "infra"); err != nil {
			t.Fatalf("EnsureProject %s: %v", id, err)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	// Rolled back, not committed: this query only reads, and a commit would
	// write nothing either way.
	defer tx.Rollback() //nolint:errcheck

	for _, limit := range []int{0, -1} {
		candidates, matching, err := s.projectsNamedTx(ctx, tx, "infra", limit)
		if err != nil {
			t.Fatalf("projectsNamedTx(limit %d): %v", limit, err)
		}
		if matching != 2 || len(candidates) != 1 || candidates[0].id != "first" {
			t.Errorf("projectsNamedTx(limit %d) = (%d rows, count %d), want one row and the count of both", limit, len(candidates), matching)
		}
	}
}

// existingDir creates a real directory under a fresh temporary root and
// returns its path. Every refusal under test compares directories on disk, so
// a path that does not resolve would make the case pass for the wrong reason.
func existingDir(t *testing.T, parts ...string) string {
	t.Helper()
	dir := filepath.Join(append([]string{t.TempDir()}, parts...)...)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create %s: %v", dir, err)
	}
	// t.TempDir() is spelled /var/folders/... on macOS, which is a symlink to
	// /private/var/folders/...; the refusal compares resolved paths, so the
	// value a test asserts on has to be the physical one.
	resolved, err := canonicalPath(dir)
	if err != nil {
		t.Fatalf("resolve %s: %v", dir, err)
	}
	return resolved
}
