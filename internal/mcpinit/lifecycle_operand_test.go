package mcpinit

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/wcatz/ghost/internal/memory"
	"testing"
)

// #839, in the one package that had no call to the renderer.
//
// The lifecycle hook's project argument is the operator's `--project` operand,
// and the session's own clone URL is what a person or an agent supplies about as
// often as a project name — inline credentials included. Both refusals below
// interpolated it with `%q` on the raw value, so a token-shaped operand was
// quoted verbatim into the operator's stderr.
//
// The wrapped error in the lock's case is the STORE's, and the store renders its
// own argument safely, so half of that sentence was already withheld: the leak
// was the operand printed beside it. A test that only looked at the wrapped
// error would have passed.
// plantProjectNamed inserts a project row directly, bypassing the creation
// guards, so a test can hold a store in the shape those guards now prevent.
func plantProjectNamed(t *testing.T, dataHome, id, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dataHome, "ghost"), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := memory.OpenDB(filepath.Join(dataHome, "ghost", "ghost.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() //nolint:errcheck
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES (?, ?, ?)`,
		id, filepath.Join(t.TempDir(), id), name); err != nil {
		t.Fatalf("plant project %q: %v", id, err)
	}
}

func TestALifecycleRefusalNeverQuotesTheProjectOperand(t *testing.T) {
	// Obviously fake, shaped by the detector's own GitHub personal access token
	// rule, and usable as a directory name so it can stand in for a session's
	// clone URL rather than only for a bare name.
	const token = "ghp_" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	// The lock's refusal needs a reference that actually RESOLVES, because an
	// unresolvable one returns (noop, true, nil) and never reaches the sentence.
	// So the token goes in the name of two projects that exist, and the operand is
	// that same token: the ordinary way a credential-shaped project argument
	// collides with something in the store.
	//
	// Planted with SQL rather than through `EnsureProject`, because the creation
	// guard refuses a credential-shaped name outright (#836) and would turn this
	// into a test of that guard. This is the shape the row actually has on the
	// stores this is about: a pre-guard save, a restored snapshot, a hand edit.
	dataHome := isolatedHome(t)
	plantProjectNamed(t, dataHome, "twin-a", token)
	plantProjectNamed(t, dataHome, "twin-b", token)

	for _, tc := range []struct {
		name string
		// run returns everything the caller can see. Both are the exported
		// entry points the hook itself uses.
		run func(t *testing.T) string
	}{
		{
			name: "the lifecycle lock's resolve failure",
			run: func(t *testing.T) string {
				t.Helper()
				_, _, err := AcquireLifecycleLock(token)
				if err == nil {
					t.Fatal("precondition: an ambiguous project operand must be refused")
				}
				return err.Error()
			},
		},
		{
			name: "the lifecycle start stamp's unresolvable project",
			run: func(t *testing.T) string {
				t.Helper()
				// An operand that resolves to nothing, so the stamp refuses. The
				// token is a directory name rather than a recorded project, which
				// is the ordinary miss: a checkout Ghost has never heard of.
				dir := filepath.Join(t.TempDir(), token)
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Skipf("this filesystem will not hold the token in a directory name: %v", err)
				}
				err := TouchLifecycleStart(dir)
				if err == nil {
					t.Fatal("precondition: a project nothing resolves to must be refused")
				}
				return err.Error()
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := tc.run(t)
			if strings.Contains(out, token) {
				t.Errorf("the refusal quoted the credential-shaped operand back to the operator: %s", out)
			}
			// And the operand is still named, because a refusal that withholds
			// everything is a refusal nobody can act on: the whole diagnostic is
			// knowing WHICH project was refused. The placeholder carries the
			// format, which is what tells the operator what to take out of it.
			if !strings.Contains(out, "GitHub personal access token") {
				t.Errorf("the refusal withheld the value without naming the format it found: %s", out)
			}
			if !strings.Contains(out, "<project withheld: it holds a ") {
				t.Errorf("the refusal does not name the operand it withheld: %s", out)
			}
		})
	}
}
