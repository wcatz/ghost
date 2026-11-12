package reflection

import (
	"fmt"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// These pin the two ways a consolidation used to delete a memory an unattended
// apply never reported (#549). Both are silent — no warning, no --allow-drops,
// exit 0 — and both are now closed by the same rule: the drop guard keeps a row
// the corpus cannot show is gone, because resolve and supersede run right after
// reflect and can demote a stale row, while nothing can bring back a deleted one.

// bulletErr is bullet without the t.Fatalf, so a test can assert the refusal
// instead of merely provoking it. A Fatalf inside a helper fails the test that
// called it, which leaves "the guard fired" unpinnable — removing the guard would
// leave every other assertion green, verified by mutation.
func bulletErr(prompt, marker string) (string, error) {
	if n := strings.Count(prompt, marker); n != 1 {
		return "", fmt.Errorf("marker %s appears %d times in the prompt, want exactly 1; a test using it would inspect the wrong line", marker, n)
	}
	_, after, ok := strings.Cut(prompt, marker)
	if !ok {
		return "", fmt.Errorf("the prompt has no %s bullet at all", marker)
	}
	line, _, ok := strings.Cut(after, "\n")
	if !ok {
		return "", fmt.Errorf("the %s bullet is the last line, so it cannot be delimited", marker)
	}
	return line, nil
}

// bullet returns the single prompt line beginning at marker, so a contract can be
// asserted on the bullet that carries it rather than on the prompt as a whole. A
// shared literal across two bullets would pin neither: a change that put the
// wrong wording on one while the other stayed right would pass.
//
// The marker must be UNIQUE, and that is checked rather than assumed.
// BuildReflectionPrompt renders untrusted stored memory above the ops contract,
// so a memory whose content quotes the ops vocabulary would put a second copy of
// the marker in the prompt — and strings.Cut returns the FIRST match, which
// would be that memory's line. Every "must not contain" assertion below would
// then pass while inspecting nothing about the ops bullet, which is the exact
// masking these tests exist to prevent.
func bullet(t *testing.T, prompt, marker string) string {
	t.Helper()
	line, err := bulletErr(prompt, marker)
	if err != nil {
		t.Fatal(err)
	}
	return line
}

// TestPromptTellsTheModelARewriteMayComeBack is the prompt half of the KEEP rule,
// and it is a test because the drift is silent: the guard changed and the prompt
// did not, so on the unattended path the model was told a rewrite or a
// supersession is final when neither is. Every such operation the corpus cannot
// account for then becomes a paraphrase duplicate that every later pass reads,
// which is the cost the rule accepts — accepted, but not intended.
//
// The prompt already carried the equivalent warning for a merge (mergeTail) and
// an obsolete drop (staleTail), so this asserts the two REPLACING operations got
// the same signal rather than asserting a new idea.
func TestPromptTellsTheModelARewriteMayComeBack(t *testing.T) {
	prompt := BuildReflectionPrompt(ReflectionInput{ExistingMemories: []memory.Memory{
		{ID: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA1", Category: "gotcha", Content: "the bastion is reached on port 2222"},
	}})

	for _, want := range []string{
		`"rewrite <id> -> <text>"`,         // the bullet exists at all
		"CARRY the old memory's substance", // the replacement must explain the row
		"puts that row back verbatim",      // and what happens when it does not
		"until a later pass demotes",       // so the model knows it is not permanent either
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt does not tell the model %q about a rewrite that does not account for its row", want)
		}
	}

	// The supersession bullet is the one the hole was reached through, so it must
	// carry the signal too and not only inherit it from the rewrite bullet.
	sup := bullet(t, prompt, `"drop <id> reason: superseded by <id>"`)
	if !strings.Contains(sup, "puts that row back verbatim") {
		t.Errorf("the supersession bullet still reads as final; it needs the same warning as a rewrite")
	}
	// The two spliced clauses must be separated by a real sentence boundary.
	// This one is here because the comment on staleTail claims the full stop
	// matters and nothing else checked it: without it the harness is handed
	// "…is undone by the verbatim re-add State an obsolete drop only when…".
	obsolete := bullet(t, prompt, `"drop <id> reason: obsolete"`)
	if !strings.Contains(obsolete, "verbatim re-add. State an obsolete drop") {
		t.Errorf("the obsolete bullet runs its two spliced clauses together: %q", obsolete)
	}

	// The RULES-section "- Drop stale situational memories" line is the SECOND of
	// the three sites that splice the mode-dependent staleTail (the obsolete
	// bullet above, this one, and #674's "- Repository facts" rule), and it is
	// pinned in the retention direction here for one narrow reason, which is
	// worth stating precisely because most mutations here are already caught
	// elsewhere.
	//
	// What is ALREADY covered without these two assertions, each verified by
	// mutation: rewording staleTail's retention value (the sentence-boundary
	// assertion above reads the same variable through the obsolete bullet);
	// splicing the other mode's tail in (fabrication_test.go rejects
	// "a drop nothing explains is a real deletion" from the retention prompt
	// prompt-wide); a neutral hardcoded literal, or countTail, spliced in (the
	// --allow-drops per-bullet assertion below reads this same line, and a
	// template-line literal renders in both modes).
	//
	// What only these catch: a tail for this line that is CORRECT under
	// --allow-drops and wrong in the retention prompt. Because the allow-drops
	// assertions already pin this bullet's other-mode rendering, a
	// mode-dependent tail written fresh for this line passes them while the
	// retention prompt tells the model something the guard does not do — the
	// prompt would re-add in the obsolete bullet and hedge in the rules
	// section, and every other assertion in the package would stay green.
	stale := bullet(t, prompt, "- Drop stale situational memories")
	if !strings.Contains(stale, "since a drop nothing explains is undone by the verbatim re-add.") {
		t.Errorf("the stale-memories rule does not carry the retention staleTail: %q", stale)
	}
	if strings.Contains(stale, "a real deletion") {
		t.Errorf("the retention prompt's stale-memories rule promises a deletion the apply does not perform: %q", stale)
	}
}
