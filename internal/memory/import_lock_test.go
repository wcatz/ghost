package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestThePresenceCheckIsUnderTheLockAndBeforeTheShapeCheck is a structural test
// for the ordering that two review rounds got wrong in opposite directions, and
// it is structural because the property is about WHERE statements sit rather
// than what they return.
//
// The ordering in all four importers has to be:
//
//	s.mu.Lock()  →  presence check  →  shape check  →  field checks  →  INSERT
//
// and each arrow was broken at some point:
//
//   - The shape check ahead of the presence check broke idempotence: a record
//     already in the store was refused rather than skipped, so a store holding
//     a pre-#791 id made a re-run fail over a row that was not being written.
//     Fixed in 0e5ef92c.
//   - Moving the presence check above the lock to make room for that fix opened
//     a duplicate-insert race: two imports of the same new id both saw it
//     absent, both passed validation, and the second failed the INSERT with a
//     UNIQUE constraint error where it used to return a skip. A check-then-write
//     is only a check-then-write if nothing can change between the check and the
//     write.
//
// A behavioural test for the race would be timing-dependent and flaky, and the
// ordering is a fact about the source, so the source is what is asserted. The
// test reads the file rather than importing a symbol because the property is
// textual: it is the relative position of four statements inside four functions.
func TestThePresenceCheckIsUnderTheLockAndBeforeTheShapeCheck(t *testing.T) {
	path := filepath.Join("portable.go")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	text := string(src)

	for _, fn := range []string{"ImportMemory", "ImportProject", "ImportTask", "ImportDecision"} {
		t.Run(fn, func(t *testing.T) {
			i := strings.Index(text, "func (s *Store) "+fn+"(")
			if i < 0 {
				t.Fatalf("func %s not found", fn)
			}
			// The function body runs to the next top-level closing brace, which
			// is the first "\n}\n" after the signature.
			j := strings.Index(text[i:], "\n}\n")
			if j < 0 {
				t.Fatalf("end of func %s not found", fn)
			}
			body := text[i : i+j]

			lock := strings.Index(body, "s.mu.Lock()")
			presence := strings.Index(body, "SELECT 1 FROM")
			shape := strings.Index(body, "CheckImported")
			insert := strings.Index(body, "INSERT INTO")
			if lock < 0 || presence < 0 || shape < 0 || insert < 0 {
				t.Fatalf("one of the four statements is missing: lock=%d presence=%d shape=%d insert=%d",
					lock, presence, shape, insert)
			}
			if lock >= presence {
				t.Errorf("the presence check is NOT under the mutex: s.mu.Lock() at %d, SELECT at %d — "+
					"two imports of the same new id can both see it absent and the second fails the INSERT "+
					"with a UNIQUE constraint error instead of returning a skip", lock, presence)
			}
			if presence >= shape {
				t.Errorf("the shape check is NOT after the presence check: SELECT at %d, CheckImported at %d — "+
					"a record already in the store would be refused rather than skipped, breaking "+
					"re-run-is-always-safe", presence, shape)
			}
			if shape >= insert {
				t.Errorf("the shape check is NOT before the INSERT: CheckImported at %d, INSERT at %d", shape, insert)
			}
		})
	}
}

// TestNoImporterErrorAboveTheShapeCheckNamesTheID is the companion assertion,
// and it is the reason the presence check can sit above the shape check without
// reintroducing the round-2 forgery. The only message between them is the
// presence check's own database error, and it must not name the id: a hostile id
// reaching that message would reach a report line without the shape check ever
// running.
func TestNoImporterErrorAboveTheShapeCheckNamesTheID(t *testing.T) {
	src, err := os.ReadFile("portable.go")
	if err != nil {
		t.Fatalf("read portable.go: %v", err)
	}
	text := string(src)

	for _, fn := range []string{"ImportMemory", "ImportProject", "ImportTask", "ImportDecision"} {
		t.Run(fn, func(t *testing.T) {
			i := strings.Index(text, "func (s *Store) "+fn+"(")
			if i < 0 {
				t.Fatalf("func %s not found", fn)
			}
			j := strings.Index(text[i:], "\n}\n")
			if j < 0 {
				t.Fatalf("end of func %s not found", fn)
			}
			body := text[i : i+j]

			shape := strings.Index(body, "CheckImported")
			if shape < 0 {
				t.Fatalf("no shape check in %s", fn)
			}
			above := body[:shape]
			// Every error message above the shape check, and none of them may
			// interpolate the id. The id-presence check's own database error is
			// the one that could have.
			for _, line := range strings.Split(above, "\n") {
				trimmed := strings.TrimSpace(line)
				if !strings.Contains(trimmed, "fmt.Errorf") {
					continue
				}
				if strings.Contains(trimmed, ".ID") {
					t.Errorf("an error message above the shape check names the id:\n%s", trimmed)
				}
			}
		})
	}
}
