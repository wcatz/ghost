package memory

// #839: the credential guard, asked where a project argument becomes an answer.
//
// `ResolveProject` returns its refusals to nineteen call sites, and every one of
// them hands the sentence on as a tool answer — so a `%q` in one of those strings
// is a place an agent's own token can come back out of, in the same answer family
// that says Ghost never stores credentials. The precondition is narrow (the
// reference has to be ambiguous) and the value has to be credential-shaped, and
// neither is under the caller's control: pasting a clone URL with inline auth into
// `project_id` is an ordinary mistake and the ambiguity is whatever the database
// happens to hold.
//
// These tests hold the renderer and both ambiguity shapes. The surface sweep that
// holds every handler is `internal/mcpserver`'s, because the claim being made here
// is about the sentence and the claim there is about where it goes.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// credentialToken is obviously fake and shaped like the class an agent produces by
// pasting a remote: the GitHub personal access token rule is `ghp_` plus 36
// characters, which is also what makes it usable as a DIRECTORY NAME — a path is
// how a session's `project_id` is written, and the path-prefix ambiguity shape
// needs a real directory on disk.
const credentialToken = "ghp_" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// TestProjectArgQuotesWhatItMayAndNamesOnlyWhatItMayNot is the renderer itself, and
// both halves of it matter.
//
// The first half is the reason the function exists at all: a refusal that names
// its argument is the diagnostic. `project "foo" not found` is how an agent learns
// its own project is misspelled and `matches multiple projects: foo` is how an
// operator finds the duplicate. So an ordinary value must come back as exactly
// `%q` — byte-identical to the sentence it replaces — and a test that only checked
// the withholding would pass on a renderer that quietly stopped naming anything.
//
// The second half is the guard. It must hold for a value that looks like a
// credential ANYWHERE, not only at the front, because the whole ambiguous-by-path
// shape is a credential buried in the middle of a path.
func TestProjectArgQuotesWhatItMayAndNamesOnlyWhatItMayNot(t *testing.T) {
	for _, tc := range []struct {
		name, field, value, want string
		credential               bool
	}{
		{name: "an ordinary name", field: "project_id", value: "ghost", want: `"ghost"`},
		{name: "a path", field: "project_id", value: "/srv/checkout/ghost", want: `"/srv/checkout/ghost"`},
		{name: "a name with a space", field: "project_id", value: "my project", want: `"my project"`},
		{name: "an empty argument", field: "project_id", value: "", want: `""`},
		{
			name: "a token", field: "project_id", value: credentialToken, credential: true,
			want: `"<project_id withheld: it holds a GitHub personal access token>"`,
		},
		{
			name: "a token buried in a path", field: "project_id",
			value:      "/srv/" + credentialToken + "/sub",
			credential: true,
			want:       `"<project_id withheld: it holds a GitHub personal access token>"`,
		},
		{
			// The field is the boundary's own name for its argument, not the
			// column's: `ghost_resolve` calls it `project`, and an agent that
			// passed `project` has to be told it was the `project`.
			name: "a token under another field name", field: "project", value: credentialToken, credential: true,
			want: `"<project withheld: it holds a GitHub personal access token>"`,
		},
		{
			// Which FORMAT is named is the detector's own precedence, not this
			// function's: a token inside a URL is reported as the token, because
			// that is the more specific finding and the more useful one to remove.
			// The assertion is that a format is named at all and no value is.
			name: "a remote with inline credentials", field: "project_id",
			value:      "https://x-access-token:" + credentialToken + "@github.com/owner/repo",
			credential: true,
			want:       `"<project_id withheld: it holds a GitHub personal access token>"`,
		},
		{
			name: "a remote whose password the URL rule owns", field: "project_id",
			value:      "https://someone:hunter2hunter2hunter2hunter2@github.com/owner/repo",
			credential: true,
			want:       `"<project_id withheld: it holds a URL with inline credentials>"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ProjectArg(tc.field, tc.value)
			if got != tc.want {
				t.Errorf("ProjectArg(%q, %q) = %s, want %s", tc.field, tc.value, got, tc.want)
			}
			if tc.credential && strings.Contains(got, tc.value) {
				t.Errorf("ProjectArg(%q, %q) quoted the value it was meant to withhold: %s", tc.field, tc.value, got)
			}
			if tc.credential && strings.Contains(got, credentialToken) {
				t.Errorf("ProjectArg(%q, %q) leaked part of the credential: %s", tc.field, tc.value, got)
			}
		})
	}
}

// TestResolveProjectRefusalsWithholdACredentialShapedArgument is #839 at the place
// it was filed, for both of the shapes `ResolveProject` can be ambiguous in.
//
// Both fixtures are the real ones, not stand-ins. The name shape plants two
// projects whose `name` IS the token, which is what `basenameCandidates` matches
// on and the first of the two steps that can report ambiguity. The path shape
// plants two projects on ONE directory spelled two ways and resolves from a
// subdirectory of it, which is the tie `pathRankLength` refuses to choose between
// — and the directory is created for real and NAMED after the token, because
// `ResolveProject` runs path candidates through `pathsAgree`, which resolves
// symlinks on both sides, so a path that does not exist is unreachable BY its path
// and the tie would never be reached.
//
// What is asserted is narrow on purpose: the token is not in the sentence, and the
// sentence is still an `ErrAmbiguousProject`. The second half matters as much as
// the first — a fix that returned the guard's own error instead would satisfy the
// leak assertion while silently changing what nineteen callers do with the error,
// and `ghost_memory_save` in particular propagates `ErrAmbiguousProject` rather
// than reporting a miss, because choosing a project for a save is not something a
// caller may do by accident.
func TestResolveProjectRefusalsWithholdACredentialShapedArgument(t *testing.T) {
	ctx := context.Background()

	t.Run("ambiguous by name", func(t *testing.T) {
		s := testStore(t)
		plantAmbiguousTwyName(t, s, credentialToken)

		_, _, err := s.ResolveProject(ctx, credentialToken)
		assertAmbiguityWithheld(t, err, credentialToken)
	})

	t.Run("ambiguous by tied path prefix", func(t *testing.T) {
		s := testStore(t)
		// A directory named after the token, so the subdirectory the session
		// reports is credential-shaped as well as ambiguous. Without the name the
		// test would be proving something weaker — that a credential-free path is
		// ambiguous — and the shape it is here for is the one an agent produces by
		// pasting a clone URL.
		dir := filepath.Join(t.TempDir(), credentialToken)
		input := filepath.Join(dir, "sub")
		if err := os.MkdirAll(input, 0o755); err != nil {
			t.Fatalf("mkdir input: %v", err)
		}
		plantTiedPathPair(t, s, dir)

		_, _, err := s.ResolveProject(ctx, input)
		assertAmbiguityWithheld(t, err, credentialToken)
		if strings.Contains(err.Error(), input) {
			t.Errorf("the refusal quoted the whole argument rather than withholding it: %v", err)
		}
	})

	t.Run("an ordinary ambiguous argument is still named", func(t *testing.T) {
		// The other half of the renderer, at the surface it exists for. Without
		// this the fix would pass by refusing to name ANY argument, which closes
		// the leak and takes the diagnostic with it — and the diagnostic is what
		// tells an operator which two projects to merge.
		s := testStore(t)
		plantAmbiguousTwyName(t, s, "dup")

		_, _, err := s.ResolveProject(ctx, "dup")
		if !errors.Is(err, ErrAmbiguousProject) {
			t.Fatalf("err = %v, want ErrAmbiguousProject", err)
		}
		if !strings.Contains(err.Error(), `"dup" matches multiple projects`) {
			t.Errorf("an ordinary ambiguous argument is no longer named: %v", err)
		}
	})
}

// assertAmbiguityWithheld is the one assertion both shapes share: the sentence is
// an ambiguity refusal, and no part of the credential is in it.
func assertAmbiguityWithheld(t *testing.T, err error, token string) {
	t.Helper()
	if !errors.Is(err, ErrAmbiguousProject) {
		t.Fatalf("err = %v, want ErrAmbiguousProject", err)
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("the refusal echoed the credential back to the caller: %v", err)
	}
}

// plantAmbiguousTwyName plants two projects carrying one name, which is the
// duplicate `ErrAmbiguousProject` exists for.
func plantAmbiguousTwyName(t *testing.T, s *Store, name string) {
	t.Helper()
	for _, id := range []string{"twin-a", "twin-b"} {
		if _, err := s.db.ExecContext(context.Background(),
			`INSERT INTO projects (id, path, name) VALUES (?, ?, ?)`, id, filepath.Join(t.TempDir(), id), name); err != nil {
			t.Fatalf("plant project %q: %v", id, err)
		}
	}
}

// plantTiedPathPair records one directory twice, in the two spellings resolution
// compares as equal but ranks by length — a Windows-style backslash spelling of a
// POSIX path is the same number of characters, so the two tie for the longest
// match and the resolver has to refuse to choose. It is the fixture
// TestResolveProjectRejectsTiedPathPrefixCandidates uses, and it is the same tie.
func plantTiedPathPair(t *testing.T, s *Store, dir string) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(), `
		INSERT INTO projects (id, path, name) VALUES
		('path-one', ? , 'infra'),
		('path-two', ? , 'infra')
	`, dir, strings.ReplaceAll(dir, string(filepath.Separator), `\`)); err != nil {
		t.Fatalf("plant tied path projects: %v", err)
	}
}
