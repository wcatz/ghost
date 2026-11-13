package memory

import (
	"context"
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
			// Rendered through shellQuote below, the way the notice quotes a
			// command argument: what this case is about is WHICH commands, in
			// WHICH order, over WHICH ids. How an awkward id is spelled is
			// TestBindingRefusalNoticeShellQuotesItsCommands' claim.
			advice: []string{`ghost project merge SAVED real-infra`, `ghost project bind real-infra SAVED`},
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
			// to, which is a filesystem path, so it is substituted already
			// shell-quoted — the quoting itself has its own test.
			//
			// The order is checked by position rather than by scanning a window
			// and resuming past what matched: a path is longer than the
			// placeholder, so any offset taken from the template rather than
			// from the text that matched leaves the next search starting
			// inside the match it just consumed, and "in that order" quietly
			// stops being the thing being tested.
			previous := -1
			for _, want := range tc.advice {
				at := strings.Index(notice, strings.ReplaceAll(want, "SAVED", shellQuote(canonical)))
				if at < 0 {
					t.Errorf("notice does not suggest %q: %q", want, notice)
					continue
				}
				if at < previous {
					t.Errorf("notice suggests %q before the command before it, so the order it states is not the order it prints: %q", want, notice)
				}
				previous = at
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
