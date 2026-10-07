package reflection

import (
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// TestTheReflectionPromptCarriesAStoredNewlineUnchanged: the consolidation
// prompt asks the model to rewrite and merge memory content and the output is
// stored, so a stored line break must reach the model as a real one inside its
// «...» block. Folding it to a visible escape (assemble.Data, #911) would have the
// model copy the escape into merged memories. Do not "fix" this.
func TestTheReflectionPromptCarriesAStoredNewlineUnchanged(t *testing.T) {
	prompt := BuildReflectionPrompt(ReflectionInput{ExistingMemories: []memory.Memory{
		mem("fact", "line one\nline two"),
	}})
	if !strings.Contains(prompt, "«line one\nline two»") {
		t.Errorf("the stored newline did not reach the prompt unchanged:\n%s", prompt)
	}
	if strings.Contains(prompt, "⏎") {
		t.Errorf("the prompt carries a fold escape the model could write back:\n%s", prompt)
	}
}
