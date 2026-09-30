package config

import (
	"os"
	"strings"
	"testing"
)

// The documentation of `reflection.supersede_consensus`, held to what the key
// actually does. Two independent reasons it needs holding rather than being
// written once and trusted:
//
//   - The key's cost lives in THREE files — the struct comment somebody reading
//     the type, the YAML comment somebody copying the example, and the
//     configuration page somebody deciding whether to enable the phase — and each
//     was found naming the CALL count and not the wall time. An operator learns
//     the cost in milliseconds and not in minutes.
//   - The rubric half of #779 (#798) left a forward reference to this gate,
//     because the gate is the second half of the same issue and the sentence had
//     to stand on its own in a PR that ships no gate. A forward reference is the
//     right way to write that, and it is also a promise: a reader of the merged
//     docs who lands on it and finds no gate has been told a remedy exists and
//     cannot find it.
//
// So this file is the completeness check for both, in the package that already
// owns `docs/configuration.md`'s env-override table.

// readDoc reads one documentation file for a completeness check, and names the
// file it could not read rather than only that a read failed.
func readDoc(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

// TestTheConsensusKeyDocumentsItsOwnTimeoutCost: a gated phase asks the
// classifier N times what an ungated one asks once, and the phase is bounded by
// `lifecycle_timeout_minutes` — which this change deliberately does NOT scale,
// because that bound catches a hung phase and multiplying it by the work factor
// makes it N times longer to notice a model that never answers. So the
// consequence lands on the operator: a gated phase multiplies its wall time
// against an unchanged deadline, and an expired one can SIGTERM inside the
// apply block, whose per-pair writes are separate transactions, leaving a
// partial write, no report at all, and a `lifecycle-last-failure` marker the next
// session-start turns into an alert.
//
// The required phrases are chosen so a paraphrase that drops the deadline, the
// scale, or the FAILURE MODE fails: a warning that omits what going wrong looks
// like is the thing this exists to prevent, and "partial write" is that clause.
//
// The counts are not derived from `supersede.MinConsensus` on purpose — this
// package does not import internal/supersede, and adding that dependency to
// assert one integer is a worse trade than requiring the prose to name the
// boundary in words.
func TestTheConsensusKeyDocumentsItsOwnTimeoutCost(t *testing.T) {
	for _, tc := range []struct {
		path string
		// want are the phrases that must appear.
		want []string
	}{
		{
			path: "config.example.yaml",
			want: []string{
				"WALL TIME SCALES WITH N AND ITS",
				"DEADLINE DOES NOT",
				"hang",
				"lifecycle_timeout_minutes",
				"partial write",
			},
		},
		{
			path: "../../docs/configuration.md",
			want: []string{
				"wall time scales with N and its deadline does not",
				"hang detector",
				"lifecycle_timeout_minutes",
				"partial write",
			},
		},
		{
			path: "../../docs/cli.md",
			want: []string{
				"wall time scales with N and its deadline does not",
				"hang detector",
				"lifecycle_timeout_minutes",
				"partial write",
			},
		},
	} {
		t.Run(tc.path, func(t *testing.T) {
			text := readDoc(t, tc.path)
			for _, want := range tc.want {
				if !strings.Contains(text, want) {
					t.Errorf("%s does not carry %q about reflection.supersede_consensus's timeout cost: a reader deciding whether to enable the phase learns the call count and not the wall time, the deadline, or the partial write an expired one leaves", tc.path, want)
				}
			}
		})
	}
}

// TestTheGateIsNamedWhereTheRubricPromisedIt: the rubric half wrote "which is
// what `supersede_consensus` is for" and a placeholder saying the gate was not
// part of that PR. The placeholder must be GONE, the sentence must name the key
// rather than the issue, and the key must be documented on the same page — or
// the reference resolves to nothing.
//
// This is a fact about two files agreeing, which is why it is a doc test and not
// a unit test: either file alone is well-formed.
func TestTheGateIsNamedWhereTheRubricPromisedIt(t *testing.T) {
	text := readDoc(t, "../../docs/configuration.md")
	if strings.Contains(text, "is not in this PR") {
		t.Error("docs/configuration.md still carries the rubric half's placeholder (\"is not in this PR\"): the gate ships in this release, so the sentence must name it")
	}
	if !strings.Contains(text, "which is what `supersede_consensus` is for") {
		t.Error("docs/configuration.md does not say the unstable-classifier finding is what `supersede_consensus` is for, so the forward reference the rubric half left is unresolved")
	}
	if !strings.Contains(text, "### `supersede_consensus`") {
		t.Error("docs/configuration.md names `supersede_consensus` but does not document it under its own heading, so the forward reference points at nothing")
	}
	// And the CLI reference, which is where a reader of the gate section came
	// from, links the rubric's sentence to the gate that answers it.
	cli := readDoc(t, "../../docs/cli.md")
	if !strings.Contains(cli, "### Gate the `--apply` on agreement between passes") {
		t.Error("docs/cli.md has no gate section for the rubric's forward reference to point at")
	}
	if !strings.Contains(cli, "which is what the [gate below](#gate-the---apply-on-agreement-between-passes) answers") {
		t.Error("docs/cli.md does not link the rubric's unstable-classifier sentence to the gate section that answers it")
	}
}
