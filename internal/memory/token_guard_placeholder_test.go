package memory

import (
	"strings"
	"testing"
)

// TestTokenGuardPlaceholderAnswersTheGuardAndNothingElse is the helper a report
// line uses when it needs BOTH of a stored id's properties: the guard's
// withholding (a credential-shaped value must not be printed whole) and the line
// renderer's escaping (any other value must not end the line or open a data
// block). The helper is the guard's question in the shape that question is asked
// in — the placeholder, and whether it withheld — so the placeholder has one
// spelling wherever it appears.
//
// The ordinary case is the one a guard that withheld everything would break: a
// helper that returned a placeholder for every value would keep the token out
// AND stop naming the project, which is the diagnostic the line is for.
func TestTokenGuardPlaceholderAnswersTheGuardAndNothingElse(t *testing.T) {
	t.Run("a credential-shaped value comes back withheld", func(t *testing.T) {
		placeholder, withheld := TokenGuardPlaceholder("project", secretFixture)
		if !withheld {
			t.Fatal("the guard did not withhold a credential-shaped value")
		}
		if placeholder == "" {
			t.Fatal("the helper withheld the value and returned no placeholder")
		}
		if strings.Contains(placeholder, secretFixture) {
			t.Errorf("the placeholder echoes the value it withheld: %q", placeholder)
		}
		if !strings.Contains(placeholder, "withheld") {
			t.Errorf("the placeholder does not say what happened: %q", placeholder)
		}
	})

	t.Run("an ordinary value comes back unwithheld", func(t *testing.T) {
		placeholder, withheld := TokenGuardPlaceholder("project", "ordinary-project")
		if withheld {
			t.Error("the guard withheld a value it does not recognise")
		}
		if placeholder != "" {
			t.Errorf("the helper returned %q for a value it did not withhold", placeholder)
		}
	})

	t.Run("the placeholder is the one a refusal prints", func(t *testing.T) {
		// The same value, the same spelling: a report line that withheld the
		// value behind a DIFFERENT placeholder than the sentence beside it
		// would tell the reader two different things about one id.
		placeholder, withheld := TokenGuardPlaceholder("project", secretFixture)
		if !withheld {
			t.Fatal("the guard did not withhold a credential-shaped value")
		}
		if want := RefusalArg("project", secretFixture); placeholder != want {
			t.Errorf("placeholder = %q, want the refusal's own %q", placeholder, want)
		}
	})
}
