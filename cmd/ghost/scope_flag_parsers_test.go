package main

import (
	"errors"
	"strings"
	"testing"
)

// The scope-flag contract, one table for every parser that takes a project.
// #876/#876-follow-up: an empty value is a REFUSAL rather than the parser's
// default scope, and naming the project twice is a refusal too — in either
// order, and whichever spelling each parser offers. Each parser below is asked
// the same six questions, so a parser that answers one of them differently is
// visible as a row rather than as a reader's surprise on the command line.
//
// Every refusal carries a substring rather than a whole message, because the
// wording is the parsers' own except for the two shared sentences ("--project
// requires a value" for an empty value, "exactly one project" for a repeat),
// and the shared sentences are what is being pinned.
type scopeCase struct {
	name string
	args []string
	// want is the substring the error must contain. Empty means the parse must
	// succeed and return wantProject.
	want string
	// wantProject is the scope a successful parse must return.
	wantProject string
}

func runScopeCases(t *testing.T, parser string, project func([]string) (string, error), cases []scopeCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(parser+"/"+tc.name, func(t *testing.T) {
			got, err := project(tc.args)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("%s(%v) = %v, want no error", parser, tc.args, err)
				}
				if got != tc.wantProject {
					t.Errorf("%s(%v) project = %q, want %q", parser, tc.args, got, tc.wantProject)
				}
				return
			}
			if err == nil {
				t.Fatalf("%s(%v) = %q with no error, want a refusal naming %q", parser, tc.args, got, tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%s(%v) error = %q, want it to contain %q", parser, tc.args, err, tc.want)
			}
		})
	}
}

func reflectProject(args []string) (string, error) {
	p, err := parseReflectArgs(args)
	return p.project, err
}

func supersedeProject(args []string) (string, error) {
	project, _, _, _, _, _, _, _, err := parseSupersedeArgs(args)
	return project, err
}

func resolveProject(args []string) (string, error) {
	p, err := parseResolveArgs(args)
	return p.project, err
}

func lifecycleProject(args []string) (string, error) {
	project, _, _, err := parseLifecycleArgs(args)
	return project, err
}

func exportProject(args []string) (string, error) {
	opts, err := parseExportArgs(args)
	return opts.Project, err
}

func historyCompactProject(args []string) (string, error) {
	opts, err := parseHistoryCompactArgs(args)
	return opts.Project, err
}

func obsidianProject(args []string) (string, error) {
	_, project, _, err := parseObsidianFlags(args)
	return project, err
}

// projectDeleteProject asks parseProjectDeleteArgs for the name only. The
// no-project case answers usage rather than an error (that is the historical
// behaviour: `ghost project delete` prints the usage block), so it is reported
// here as an error whose text the case asserts.
func projectDeleteProject(args []string) (string, error) {
	name, _, showUsage, err := parseProjectDeleteArgs(args)
	if err != nil {
		return "", err
	}
	if showUsage {
		return "", errors.New(projectDeleteUsage)
	}
	return name, nil
}

// TestScopeFlagParsersRefuseEmptyAndRepeated covers every parser that takes a
// project with one table: a single valid value still works, an empty value is
// refused in each spelling the parser offers, and a second occurrence is
// refused in every order — flag then flag, flag then operand, operand then flag,
// and either spelling paired with either. The mixed orders are the ones that
// used to slip through: a value test on the project cannot see a second
// occurrence that is empty, and a positional that assigns without bookkeeping
// is invisible to a guard that only the flag arms consult, so `ghost reflect
// --project a b` acted on b while `ghost reflect b --project a` was refused.
func TestScopeFlagParsersRefuseEmptyAndRepeated(t *testing.T) {
	t.Run("reflect", func(t *testing.T) {
		runScopeCases(t, "reflect", reflectProject, []scopeCase{
			{name: "a single positional", args: []string{"alpha"}, wantProject: "alpha"},
			{name: "a single separate --project", args: []string{"--project", "alpha"}, wantProject: "alpha"},
			{name: "a single attached --project", args: []string{"--project=alpha"}, wantProject: "alpha"},
			// A dash-leading name is a NAME, not a flag: --project takes the next
			// token verbatim, so it is never empty and never a second occurrence.
			{name: "a dash-leading name", args: []string{"--project", "-alpha"}, wantProject: "-alpha"},
			{name: "an empty separate value", args: []string{"--project", ""}, want: "--project requires a value"},
			{name: "an empty attached value", args: []string{"--project="}, want: "--project requires a value"},
			{name: "an empty positional", args: []string{""}, want: "--project requires a value"},
			{name: "two positionals", args: []string{"alpha", "beta"}, want: "expected exactly one project"},
			{name: "positional then separate flag", args: []string{"alpha", "--project", "beta"}, want: "expected exactly one project"},
			{name: "positional then attached flag", args: []string{"alpha", "--project=beta"}, want: "expected exactly one project"},
			{name: "separate flag then positional", args: []string{"--project", "alpha", "beta"}, want: "expected exactly one project"},
			{name: "attached flag then positional", args: []string{"--project=alpha", "beta"}, want: "expected exactly one project"},
			{name: "separate then separate", args: []string{"--project", "alpha", "--project", "beta"}, want: "expected exactly one project"},
			{name: "separate then attached", args: []string{"--project", "alpha", "--project=beta"}, want: "expected exactly one project"},
			{name: "attached then separate", args: []string{"--project=alpha", "--project", "beta"}, want: "expected exactly one project"},
			{name: "attached then attached", args: []string{"--project=alpha", "--project=beta"}, want: "expected exactly one project"},
			// Two empties stop at the FIRST one: there is no scope in the command
			// line to be a duplicate of, so the empty value is what the reader
			// needs to be told about.
			{name: "two empty values stop at the first", args: []string{"--project", "", "--project", ""}, want: "--project requires a value"},
			{name: "a named scope then an empty one is still twice", args: []string{"--project", "alpha", "--project", ""}, want: "expected exactly one project"},
			{name: "a named scope then an empty positional is still twice", args: []string{"--project", "alpha", ""}, want: "expected exactly one project"},
		})
	})

	t.Run("supersede", func(t *testing.T) {
		runScopeCases(t, "supersede", supersedeProject, []scopeCase{
			{name: "a single positional", args: []string{"alpha"}, wantProject: "alpha"},
			{name: "a single separate --project", args: []string{"--project", "alpha"}, wantProject: "alpha"},
			{name: "a single attached --project", args: []string{"--project=alpha"}, wantProject: "alpha"},
			{name: "a dash-leading name", args: []string{"--project", "-alpha"}, wantProject: "-alpha"},
			{name: "an empty separate value", args: []string{"--project", ""}, want: "--project requires a value"},
			{name: "an empty attached value", args: []string{"--project="}, want: "--project requires a value"},
			{name: "an empty positional", args: []string{""}, want: "--project requires a value"},
			{name: "two positionals", args: []string{"alpha", "beta"}, want: "expected exactly one project"},
			{name: "positional then separate flag", args: []string{"alpha", "--project", "beta"}, want: "expected exactly one project"},
			{name: "positional then attached flag", args: []string{"alpha", "--project=beta"}, want: "expected exactly one project"},
			{name: "separate flag then positional", args: []string{"--project", "alpha", "beta"}, want: "expected exactly one project"},
			{name: "attached flag then positional", args: []string{"--project=alpha", "beta"}, want: "expected exactly one project"},
			{name: "separate then separate", args: []string{"--project", "alpha", "--project", "beta"}, want: "expected exactly one project"},
			{name: "separate then attached", args: []string{"--project", "alpha", "--project=beta"}, want: "expected exactly one project"},
			{name: "attached then separate", args: []string{"--project=alpha", "--project", "beta"}, want: "expected exactly one project"},
			{name: "attached then attached", args: []string{"--project=alpha", "--project=beta"}, want: "expected exactly one project"},
			{name: "two empty values stop at the first", args: []string{"--project", "", "--project", ""}, want: "--project requires a value"},
			{name: "a named scope then an empty one is still twice", args: []string{"--project", "alpha", "--project", ""}, want: "expected exactly one project"},
		})
	})

	t.Run("resolve", func(t *testing.T) {
		runScopeCases(t, "resolve", resolveProject, []scopeCase{
			{name: "a single positional", args: []string{"alpha"}, wantProject: "alpha"},
			{name: "a single separate --project", args: []string{"--project", "alpha"}, wantProject: "alpha"},
			{name: "a single attached --project", args: []string{"--project=alpha"}, wantProject: "alpha"},
			{name: "a dash-leading name", args: []string{"--project", "-alpha"}, wantProject: "-alpha"},
			{name: "an empty separate value", args: []string{"--project", ""}, want: "--project requires a value"},
			{name: "an empty attached value", args: []string{"--project="}, want: "--project requires a value"},
			{name: "an empty positional", args: []string{""}, want: "--project requires a value"},
			{name: "two positionals", args: []string{"alpha", "beta"}, want: "expected exactly one project"},
			// The mixed orders, because the positional is the spelling a guard
			// written only for the flag arms cannot see: with a value test here
			// instead of the occurrence count, `ghost resolve "" beta` would run
			// on beta in a command line that named the scope twice.
			{name: "an empty positional then a project", args: []string{"", "beta"}, want: "expected exactly one project"},
			{name: "positional then separate flag", args: []string{"alpha", "--project", "beta"}, want: "expected exactly one project"},
			{name: "positional then attached flag", args: []string{"alpha", "--project=beta"}, want: "expected exactly one project"},
			{name: "separate flag then positional", args: []string{"--project", "alpha", "beta"}, want: "expected exactly one project"},
			{name: "attached flag then positional", args: []string{"--project=alpha", "beta"}, want: "expected exactly one project"},
			{name: "an empty positional then a separate flag", args: []string{"", "--project", "beta"}, want: "expected exactly one project"},
			{name: "an empty positional then an attached flag", args: []string{"", "--project=beta"}, want: "expected exactly one project"},
			{name: "separate then separate", args: []string{"--project", "alpha", "--project", "beta"}, want: "expected exactly one project"},
			{name: "separate then attached", args: []string{"--project", "alpha", "--project=beta"}, want: "expected exactly one project"},
			{name: "attached then separate", args: []string{"--project=alpha", "--project", "beta"}, want: "expected exactly one project"},
			{name: "attached then attached", args: []string{"--project=alpha", "--project=beta"}, want: "expected exactly one project"},
			{name: "two empty values stop at the first", args: []string{"--project", "", "--project", ""}, want: "--project requires a value"},
			{name: "a named scope then an empty one is still twice", args: []string{"--project", "alpha", "--project", ""}, want: "expected exactly one project"},
			{name: "a named scope then an empty positional is still twice", args: []string{"--project", "alpha", ""}, want: "expected exactly one project"},
		})
	})

	t.Run("export", func(t *testing.T) {
		runScopeCases(t, "export", exportProject, []scopeCase{
			{name: "a single separate --project", args: []string{"--project", "alpha"}, wantProject: "alpha"},
			{name: "a single attached --project", args: []string{"--project=alpha"}, wantProject: "alpha"},
			{name: "an empty separate value", args: []string{"--project", ""}, want: "--project requires a value"},
			{name: "an empty attached value", args: []string{"--project="}, want: "--project requires a value"},
			{name: "separate then separate", args: []string{"--project", "alpha", "--project", "beta"}, want: "expected exactly one project"},
			{name: "separate then attached", args: []string{"--project", "alpha", "--project=beta"}, want: "expected exactly one project"},
			{name: "attached then separate", args: []string{"--project=alpha", "--project", "beta"}, want: "expected exactly one project"},
			{name: "attached then attached", args: []string{"--project=alpha", "--project=beta"}, want: "expected exactly one project"},
			{name: "two empty values stop at the first", args: []string{"--project", "", "--project", ""}, want: "--project requires a value"},
			{name: "a named scope then an empty one is still twice", args: []string{"--project", "alpha", "--project", ""}, want: "expected exactly one project"},
		})
	})

	// lifecycle has no `--project=` spelling, so the attached cases do not exist
	// to ask; it takes the next token verbatim and a dash-leading name is a name.
	t.Run("lifecycle", func(t *testing.T) {
		runScopeCases(t, "lifecycle", lifecycleProject, []scopeCase{
			{name: "a single positional", args: []string{"alpha"}, wantProject: "alpha"},
			{name: "a single separate --project", args: []string{"--project", "alpha"}, wantProject: "alpha"},
			{name: "a dash-leading name", args: []string{"--project", "-alpha"}, wantProject: "-alpha"},
			{name: "an empty separate value", args: []string{"--project", ""}, want: "--project requires a value"},
			// An empty operand is refused by the same wording the flag uses; the
			// tail check is what refuses a positional that is empty where no
			// --project was typed at all.
			{name: "an empty positional", args: []string{""}, want: "--project is required"},
			{name: "two positionals", args: []string{"alpha", "beta"}, want: "extra argument"},
			{name: "positional then separate flag", args: []string{"alpha", "--project", "beta"}, want: "more than once"},
			{name: "separate flag then positional", args: []string{"--project", "alpha", "beta"}, want: "extra argument"},
			{name: "an empty positional after a named scope is still twice", args: []string{"--project", "alpha", ""}, want: "extra argument"},
			{name: "separate then separate", args: []string{"--project", "alpha", "--project", "beta"}, want: "more than once"},
			{name: "a named scope then an empty one is still twice", args: []string{"--project", "alpha", "--project", ""}, want: "more than once"},
		})
	})

	t.Run("history compact", func(t *testing.T) {
		runScopeCases(t, "history compact", historyCompactProject, []scopeCase{
			{name: "a single separate --project", args: []string{"--project", "alpha"}, wantProject: "alpha"},
			{name: "a single attached --project", args: []string{"--project=alpha"}, wantProject: "alpha"},
			{name: "an empty separate value", args: []string{"--project", ""}, want: "needs a value"},
			{name: "an empty attached value", args: []string{"--project="}, want: "needs a value"},
			{name: "separate then separate", args: []string{"--project", "alpha", "--project", "beta"}, want: "more than once"},
			{name: "separate then attached", args: []string{"--project", "alpha", "--project=beta"}, want: "more than once"},
			{name: "attached then separate", args: []string{"--project=alpha", "--project", "beta"}, want: "more than once"},
			{name: "attached then attached", args: []string{"--project=alpha", "--project=beta"}, want: "more than once"},
			{name: "two empty values stop at the first", args: []string{"--project", "", "--project", ""}, want: "needs a value"},
			{name: "a named scope then an empty one is still twice", args: []string{"--project", "alpha", "--project", ""}, want: "more than once"},
			{name: "a positional is not a project here", args: []string{"alpha"}, want: "history compact takes flags"},
		})
	})

	// obsidian takes only the two flag spellings — a bare word is an unknown
	// flag there, not a positional project — so no positional case exists, and
	// the mirror's blast radius is what the refusals are for: an empty
	// --project is not a narrower mirror, it is EVERY project written into the
	// vault, which is the opposite of what a script with an unset variable meant.
	t.Run("obsidian", func(t *testing.T) {
		runScopeCases(t, "obsidian", obsidianProject, []scopeCase{
			{name: "a single separate --project", args: []string{"--project", "alpha"}, wantProject: "alpha"},
			{name: "a single attached --project", args: []string{"--project=alpha"}, wantProject: "alpha"},
			// --project takes the next token verbatim, so a dash-leading name is a
			// name and never a second occurrence.
			{name: "a dash-leading name", args: []string{"--project", "-alpha"}, wantProject: "-alpha"},
			{name: "an empty separate value", args: []string{"--project", ""}, want: "--project requires a value"},
			{name: "an empty attached value", args: []string{"--project="}, want: "--project requires a value"},
			{name: "separate then separate", args: []string{"--project", "alpha", "--project", "beta"}, want: "expected exactly one project"},
			{name: "separate then attached", args: []string{"--project", "alpha", "--project=beta"}, want: "expected exactly one project"},
			{name: "attached then separate", args: []string{"--project=alpha", "--project", "beta"}, want: "expected exactly one project"},
			{name: "attached then attached", args: []string{"--project=alpha", "--project=beta"}, want: "expected exactly one project"},
			{name: "two empty values stop at the first", args: []string{"--project", "", "--project", ""}, want: "--project requires a value"},
			{name: "a named scope then an empty one is still twice", args: []string{"--project", "alpha", "--project", ""}, want: "expected exactly one project"},
			{name: "a named scope then an empty attached one is still twice", args: []string{"--project=alpha", "--project="}, want: "expected exactly one project"},
			// A --project with no argument at all was already refused before
			// #876, and it shares one arm with --out and --interval, so its
			// wording stays this parser's own ("flag --project needs a value"):
			// the rule under test is the empty VALUE and the repeat, not this.
			{name: "a --project with no argument", args: []string{"--project"}, want: "flag --project needs a value"},
			{name: "a positional is not a project here", args: []string{"alpha"}, want: "unknown or malformed flag"},
		})
	})

	// project delete takes the project positionally and has no --project flag.
	t.Run("project delete", func(t *testing.T) {
		runScopeCases(t, "project delete", projectDeleteProject, []scopeCase{
			{name: "a single positional", args: []string{"alpha"}, wantProject: "alpha"},
			{name: "an empty positional is refused", args: []string{""}, want: "must not be empty"},
			{name: "two positionals", args: []string{"alpha", "beta"}, want: "exactly one project"},
			// An empty first operand stops the parse where it is, as in every
			// other parser here: there is no scope in the command line to be a
			// duplicate of, so the empty value is what the reader needs told.
			{name: "an empty first positional stops the parse", args: []string{"", "beta"}, want: "must not be empty"},
			{name: "no project at all", args: nil, want: "Usage: ghost project delete"},
		})
	})
}
