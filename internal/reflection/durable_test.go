package reflection

import (
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// #674: a memory that restates what the repository holds is a consolidation
// candidate. It is stated here as a `drop … reason: obsolete` CANDIDATE, never
// as a licence to remove one — the unattended path's answer to an unexplained
// drop is the verbatim re-add, and the mode-dependent tail that says so is
// spliced into this rule exactly as it is into the others.

func durableInput(allowDrops bool) string {
	return BuildReflectionPrompt(ReflectionInput{
		ProjectName: "ghost",
		ExistingMemories: []memory.Memory{
			{ID: "D20E133860CC4AFE38B485AD5371BA59", Category: "fact", Content: "foo.go contains HandleFoo()"},
		},
		AllowDrops: allowDrops,
	})
}

// TestBuildReflectionPrompt_TreatsRepositoryFactsAsDropCandidates pins the
// prompt half of #674: a purely reconstructable code-location fact is a drop
// candidate, and the durable half of the rule — a rule, a constraint, a reason
// or a consequence the code does not state — is not, whatever paths it names.
// Without the second half the first is a licence to consolidate away every
// architecture note that happens to cite a file.
func TestBuildReflectionPrompt_TreatsRepositoryFactsAsDropCandidates(t *testing.T) {
	prompt := durableInput(false)

	// Every claim is asserted on the RULE BULLET, never on the prompt as a
	// whole: the input memory in this fixture is "foo.go contains HandleFoo()",
	// so a prompt-wide "contains" for the issue's bad example is satisfied by
	// the rendered memory and would pass with the example deleted from the rule.
	// The bullet is where the rule has to live for it to reach the model.
	rule := bullet(t, prompt, "- Repository facts")
	for _, want := range []string{
		// the candidate, with the issue's own bad example
		"foo.go contains HandleFoo()",
		"an obsolete-drop candidate",
		"reading the file settles it",
		// the boundary, with the issue's own good example
		"Production schema changes require explicit approval.",
		// "candidate" is qualified because the next bullet reads "Every category
		// is a candidate" about something else, and a model reading the list
		// literally would have to pick between the two senses.
		"never an obsolete-drop candidate",
		"stays whatever files or paths it mentions",
	} {
		if !strings.Contains(rule, want) {
			t.Errorf("the repository-facts rule is missing %q; got:\n%s", want, rule)
		}
	}

	// The obsolete bullet is where a reader looks for what may be dropped, so
	// the new ground has to be part of its DEFINITION and not only of a rule
	// bullet: "the memory is wrong or no longer true" alone excludes a note
	// that is perfectly true and merely redundant with the code, which is the
	// whole population #674 is about.
	obsolete := bullet(t, prompt, `"drop <id> reason: obsolete"`)
	if !strings.Contains(obsolete, "what the repository already holds") {
		t.Errorf("the obsolete drop bullet does not name the repository-fact ground; got:\n%s", obsolete)
	}
	// obsoleteTail carries the SECOND ground, and fabrication_test.go pins only
	// its first disjunct, so without this the rule would be stated in the Rules
	// block and contradicted by the bullet that defines the operation.
	if !strings.Contains(obsolete, "or when the note records nothing the code does not already say") {
		t.Errorf("the obsolete drop bullet does not allow a repository-fact ground on its tail; got:\n%s", obsolete)
	}
}

// TestBuildReflectionPrompt_RepositoryFactRuleCarriesTheModeTail: the rule is
// the only new one in the Rules block, and it claims something about what a
// drop costs. That claim is two-sided, so it takes the same mode-dependent tail
// every other drop clause takes — a rule asserting "this only proposes" under
// --allow-drops would tell the eval harness a consolidation is cheaper than it
// is.
func TestBuildReflectionPrompt_RepositoryFactRuleCarriesTheModeTail(t *testing.T) {
	retained := durableInput(false)
	dropping := durableInput(true)

	// Every assertion is on the BULLET, never on the prompt as a whole: two
	// other lines already carry staleTail verbatim, so a prompt-wide "contains"
	// here is satisfied by a neighbour and passes on a rule whose own tail was
	// hardcoded to the wrong mode. That is the exact masking the bullet helper
	// exists to prevent, and a neutral literal on this line is the mutation it
	// is here for.
	//
	// The ADJACENCY assertion is the second half: WHERE the tail sits matters
	// as much as that it is there. staleTail prices a DROP, so it belongs on
	// the sentence that proposes one; moved to the end of the bullet it would
	// land on the prohibition and read as justification for "never a
	// candidate", which is not what it says.
	retainedRule := bullet(t, retained, "- Repository facts")
	if !strings.Contains(retainedRule, "reading the file settles it, since a drop nothing explains is undone by the verbatim re-add.") {
		t.Errorf("the retention prompt's repository-fact rule does not price the drop it proposes, on that clause: %q", retainedRule)
	}
	if strings.Contains(retainedRule, "a real deletion") {
		t.Errorf("the retention prompt's repository-fact rule promises a deletion the apply does not perform: %q", retainedRule)
	}

	droppingRule := bullet(t, dropping, "- Repository facts")
	if !strings.Contains(droppingRule, "reading the file settles it, since a drop nothing explains is a real deletion.") {
		t.Errorf("the --allow-drops prompt's repository-fact rule does not price the drop it proposes, on that clause: %q", droppingRule)
	}
	if strings.Contains(droppingRule, "verbatim re-add") {
		t.Errorf("the --allow-drops prompt's repository-fact rule promises a re-add the apply skips: %q", droppingRule)
	}
}
