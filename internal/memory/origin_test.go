package memory

import (
	"testing"
)

// TestCanonicalOriginSourceForProjectScopesTheSeedRewriteToTheGlobalProject
// pins both halves of the compatibility rewrite's precondition. The v15
// migration that eventually makes this unnecessary needs both the global
// project and the frozen content, so a render of a row still in the legacy
// shape has to reach the same conclusion — and must not reach it for a project
// row.
//
// The project half is the one that matters. Relabelling a user's own project
// memory as builtin both misattributes it and strips the absence-of-a-tag that
// every surface uses as the marker for direct user material, which is a
// correctness loss rather than a cosmetic one.
func TestCanonicalOriginSourceForProjectScopesTheSeedRewriteToTheGlobalProject(t *testing.T) {
	cases := []struct {
		name      string
		projectID string
		source    string
		content   string
		want      string
	}{
		{
			name:      "the shipped seed in the global project is a builtin",
			projectID: GlobalProjectID,
			source:    "manual",
			content:   builtinSeedContent,
			want:      "builtin",
		},
		{
			name:      "the same words in a project are the user's own",
			projectID: "abc123",
			source:    "manual",
			content:   builtinSeedContent,
			want:      "manual",
		},
		{
			name:      "an unstated project is not the reserved one",
			projectID: "",
			source:    "manual",
			content:   builtinSeedContent,
			want:      "manual",
		},
		{
			name:      "another global preference keeps its source",
			projectID: GlobalProjectID,
			source:    "manual",
			content:   "a user's own preference",
			want:      "manual",
		},
		{
			name:      "an already-relabelled global seed is unchanged",
			projectID: GlobalProjectID,
			source:    "builtin",
			content:   builtinSeedContent,
			want:      "builtin",
		},
		{
			name:      "a non-manual source is never rewritten",
			projectID: GlobalProjectID,
			source:    "reflection",
			content:   builtinSeedContent,
			want:      "reflection",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CanonicalOriginSourceForProject(tc.projectID, tc.source, tc.content); got != tc.want {
				t.Errorf("CanonicalOriginSourceForProject(%q, %q, seed?%v) = %q, want %q",
					tc.projectID, tc.source, tc.content == builtinSeedContent, got, tc.want)
			}
		})
	}
}
