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
//	s.mu.Lock()  →  presence check  →  record checks  →  INSERT
//
// where "record checks" is the one shared predicate each importer now calls
// (CheckImportedMemory and friends, #813) — it used to be the shape check followed
// by the field checks, and one call is both the reason this test could be simplified
// and the reason it still holds: the exporter calls the same function, so a refusal
// that is not behind the presence check is one `ghost export` cannot honour.
//
// Each arrow was broken at some point:
//
//   - The record checks ahead of the presence check broke idempotence: a record
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
			// COMMENTS ARE BLANKED, and that is a fix rather than a nicety. This
			// test is about where STATEMENTS sit, and a comment naming the shape
			// check is not a call to it: an explanatory paragraph above the
			// presence check ("THEN CheckImportedMemory, ...") read as a shape
			// check positioned before it, and the test failed a file whose order
			// was correct. The offset arithmetic below is only meaningful over
			// code, and blanking keeps the positions of the four statements
			// comparable — which is the whole assertion.
			body := blankLineComments(text[i : i+j])

			lock := strings.Index(body, "s.mu.Lock()")
			presence := strings.Index(body, "SELECT 1 FROM")
			shape := strings.Index(body, "CheckImported")
			begin := strings.Index(body, "beginWrite")
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
				t.Errorf("the record checks are NOT after the presence check: SELECT at %d, CheckImported at %d — "+
					"a record already in the store would be refused rather than skipped, breaking "+
					"re-run-is-always-safe", presence, shape)
			}
			if shape >= insert {
				t.Errorf("the record checks are NOT before the INSERT: CheckImported at %d, INSERT at %d", shape, insert)
			}
			// And the re-check that closes the cross-process race: after
			// beginWrite, before the INSERT. s.mu is a per-Store lock and closes
			// nothing across processes, so the pre-check alone is not atomic —
			// the re-check has to be inside the BEGIN IMMEDIATE transaction.
			recheck := strings.LastIndex(body, "SELECT 1 FROM")
			if begin < 0 {
				t.Fatalf("no beginWrite in %s — the write has to go through a transaction the re-check can share", fn)
			}
			if recheck <= begin {
				t.Errorf("the presence re-check is NOT inside the write transaction: beginWrite at %d, "+
					"re-check at %d — a second process can pass the pre-check and fail the INSERT with a "+
					"UNIQUE constraint error", begin, recheck)
			}
			if recheck >= insert {
				t.Errorf("the presence re-check is NOT before the INSERT: re-check at %d, INSERT at %d", recheck, insert)
			}
		})
	}
}

// TestNoImporterErrorAboveTheRecordCheckNamesTheID is the companion assertion,
// and it is the reason the presence check can sit above the record checks without
// reintroducing the round-2 forgery. The only message between them is the presence
// check's own database error, and it must not name the id: a hostile id reaching
// that message would reach a report line without the record checks ever running.
//
// The record checks are now ONE call to a shared predicate rather than three inline
// checks, and the property is unchanged. It is worth being precise about WHY, since
// the ordering used to be the only thing holding it up: it no longer is. No message
// in CheckImported* interpolates a record's id, so even a check placed above the
// shape check could not carry the payload — the predicate's messages name a FIELD.
// This test still guards the one message that remains above the call, and
// TestAPredicateRefusalNeverCarriesTheIDOrTheValue in internal/portable guards the
// predicate itself.
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
			// Comments blanked, for the reason the sibling test gives: a comment
			// that NAMES a check is not a call to it, and a paragraph explaining
			// the ordering must not be read as the ordering.
			body := blankLineComments(text[i : i+j])

			shape := strings.Index(body, "CheckImported")
			if shape < 0 {
				t.Fatalf("no record check in %s", fn)
			}
			above := body[:shape]
			// Every error message above the record check, and none of them may
			// interpolate the id. The id-presence check's own database error is
			// the one that could have.
			for _, line := range strings.Split(above, "\n") {
				trimmed := strings.TrimSpace(line)
				if !strings.Contains(trimmed, "fmt.Errorf") {
					continue
				}
				if strings.Contains(trimmed, ".ID") {
					t.Errorf("an error message above the record check names the id:\n%s", trimmed)
				}
			}
		})
	}
}

// blankLineComments replaces every `//` comment with spaces, preserving length so
// the offsets a caller computes over the result still index the same bytes.
//
// Only whole-line comments are blanked, and that is deliberate: the assertions
// above look at STATEMENTS on their own lines, so a trailing comment on a
// statement's line cannot move anything. Block comments are not handled, and none
// of the four importers has one inside a function body.
func blankLineComments(src string) string {
	lines := strings.Split(src, "\n")
	for i, line := range lines {
		trimmed := strings.TrimLeft(line, " \t")
		if !strings.HasPrefix(trimmed, "//") {
			continue
		}
		lines[i] = strings.Repeat(" ", len(line))
	}
	return strings.Join(lines, "\n")
}
