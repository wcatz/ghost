package memory

import "testing"

// TestContradictoryInstruction pins the guard that stops FoldOnly from folding
// a near-match that says the opposite thing. The Jaccard bar alone cannot see
// this: negation is one token, so "always deploy staging" and "never deploy
// staging" score 0.5 — exactly the merge threshold.
func TestContradictoryInstruction(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		want bool
	}{
		{
			name: "flipped polarity on the same subject",
			a:    "always deploy staging before merging the release branch",
			b:    "never deploy staging before merging the release branch",
			want: true,
		},
		{
			name: "not versus never",
			a:    "run the migrate step before the deploy",
			b:    "never run the migrate step before the deploy",
			want: true,
		},
		{
			name: "different numeric value",
			a:    "the health check endpoint listens on port 80",
			b:    "the health check endpoint listens on port 81",
			want: true,
		},
		{
			name: "a genuine restatement is not a contradiction",
			a:    "always deploy staging before merging the release branch",
			b:    "always deploy staging before merging a release branch",
			want: false,
		},
		{
			name: "unrelated text with no shared numbers",
			a:    "prefer squash merges on the release branch",
			b:    "the changelog is generated from the commit log",
			want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := contradictoryInstruction(tokenizeContent(c.a), tokenizeContent(c.b))
			if got != c.want {
				t.Errorf("contradictoryInstruction(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
			}
		})
	}
}
