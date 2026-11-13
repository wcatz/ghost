package memory

import (
	"strings"
	"testing"
)

// TestTokenGuardPlaceholderAnswersTheGuardAndNothingElse: a token-shaped value
// comes back withheld behind the same placeholder a refusal prints, and an
// ordinary value comes back unwithheld with no placeholder.
func TestTokenGuardPlaceholderAnswersTheGuardAndNothingElse(t *testing.T) {
	placeholder, withheld := TokenGuardPlaceholder("project", secretFixture)
	if !withheld || placeholder == "" {
		t.Fatalf("the guard did not withhold a token-shaped value: %q, %v", placeholder, withheld)
	}
	if strings.Contains(placeholder, secretFixture) {
		t.Errorf("the placeholder echoes the value it withheld: %q", placeholder)
	}
	if want := RefusalArg("project", secretFixture); placeholder != want {
		t.Errorf("placeholder = %q, want the refusal's own %q", placeholder, want)
	}
	if placeholder, withheld := TokenGuardPlaceholder("project", "ordinary-project"); withheld || placeholder != "" {
		t.Errorf("an ordinary value came back %q, %v", placeholder, withheld)
	}
}
