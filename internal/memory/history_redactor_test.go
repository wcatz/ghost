package memory

import (
	"context"
	"strings"
	"testing"
)

// TestHistoryRedactorIsInstalled is the wiring #664 left for this PR, and the
// test that says whether it is done or not.
//
// #664 added memory_history as the one place Ghost keeps text it holds nowhere
// else — a memory row is overwritten by the next edit and gone by the next
// delete, while its earlier versions sit in this table and are printed by
// `ghost history`. It also left the seam: `redactHistoryContent` as an identity
// function, with a `TODO(#656)` saying this PR installs `secret.Detect` through
// it. Until that is done the seam is worse than absent — the append path's
// comments claim the content is redacted, and it is not.
//
// The threat is narrow and worth stating, because it decides how careful the
// redaction has to be. Every writer now REFUSES a credential, so a credential
// reaches a history row only if it was stored before this guard existed. So
// wiring the seam cannot introduce a new leak; it can only close one that is
// already on disk. That is why the conservative redaction below is the right
// trade rather than a lazy one — see redactHistoryContent.
func TestHistoryRedactorIsInstalled(t *testing.T) {
	if !historyRedactorInstalled() {
		t.Fatal("no redactor is installed, so `ghost history` returns credential " +
			"text verbatim. #664 left this seam as the identity function with a " +
			"TODO for this PR; a TODO is not a control.")
	}
	if historyContentExpr() != historyContentFunc+"(content)" {
		t.Errorf("historyContentExpr() = %q, want the SQL filter — the append "+
			"statement's gate is derived from the redactor, so this reading means "+
			"the filter is installed but not being called", historyContentExpr())
	}
}

// TestHistoryRedactsACredentialAndKeepsEverythingElse is the behaviour, in both
// directions, end to end through a real write.
func TestHistoryRedactsACredentialAndKeepsEverythingElse(t *testing.T) {
	// Assembled rather than written out: GitHub push protection matches the PAT
	// format anywhere in a diff and rejects the push (GH013) before review.
	credential := "ghp_" + strings.Repeat("a1B2c3D4e5F6", 3) + "AbCd"

	t.Run("a credential is redacted out of the history", func(t *testing.T) {
		s := testStore(t)
		// A pre-guard row: the store's own writers would refuse this content, so
		// it is written the only way a leaked row can exist — straight through
		// insertMemory, which is the statement CreateFromCorpus also uses.
		id, err := s.CreateFromCorpus(context.Background(), testProject, Memory{
			Category: "fact", Importance: 0.5, Source: "onboarding",
			Content: "the deploy key is " + credential,
		})
		if err != nil {
			t.Fatalf("CreateFromCorpus: %v", err)
		}
		// Now the user rotates it, which is what produces a history row holding
		// the OLD text — the leak #664's author was reasoning about.
		if err := s.UpdateMemory(context.Background(), testProject, id,
			strPtr("the deploy key was rotated"), nil, nil, nil); err != nil {
			t.Fatalf("UpdateMemory: %v", err)
		}

		entries, err := s.MemoryHistory(context.Background(), id, 0)
		if err != nil {
			t.Fatalf("MemoryHistory: %v", err)
		}
		var leaked int
		for _, e := range entries {
			if strings.Contains(e.Content, credential) {
				leaked++
			}
		}
		if leaked != 0 {
			t.Errorf("%d of %d history entries still hold the credential — the "+
				"history is the longest-lived copy of the text and `ghost history` "+
				"prints it", leaked, len(entries))
		}
		// And the redaction says what it removed, or an operator reading
		// `ghost history` sees text vanish with no explanation.
		found := false
		for _, e := range entries {
			if strings.Contains(e.Content, "redacted") {
				found = true
				if strings.Contains(e.Content, credential) {
					t.Errorf("the redaction notice quotes the value: %q", e.Content)
				}
			}
		}
		if !found {
			t.Error("no entry says it was redacted — a reader cannot tell a " +
				"redaction from data loss")
		}
		// The live row is untouched. The filter is installed on the history
		// append path only, so what the user reads back from the store is the
		// text they wrote — here the rotation, not a redaction notice.
		rows, err := s.GetAll(context.Background(), testProject, 100)
		if err != nil {
			t.Fatalf("GetAll: %v", err)
		}
		live := ""
		for _, r := range rows {
			if r.ID == id {
				live = r.Content
			}
		}
		if live != "the deploy key was rotated" {
			t.Errorf("the live row reads %q, want the text the user wrote — the "+
				"history filter must not reach the memory itself", live)
		}
	})

	t.Run("ordinary content is stored verbatim", func(t *testing.T) {
		s := testStore(t)
		const body = "k3s-mini-1 runs Grafana on port 80 and the relay listens on 2222"
		id, _, _, err := s.Upsert(context.Background(), testProject, "fact", body, "mcp", 0.5, nil)
		if err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		if err := s.UpdateMemory(context.Background(), testProject, id,
			strPtr(body+" — and Postgres is on 5432"), nil, nil, nil); err != nil {
			t.Fatalf("UpdateMemory: %v", err)
		}
		entries, err := s.MemoryHistory(context.Background(), id, 0)
		if err != nil {
			t.Fatalf("MemoryHistory: %v", err)
		}
		for _, e := range entries {
			if !strings.Contains(e.Content, body) {
				t.Errorf("an ordinary memory was altered in the history:\n got %q\nwant it to contain %q", e.Content, body)
			}
		}
	})
}

// preGuardRow reproduces the state a build from before this PR left behind: a
// memory whose content holds a credential, with that text in its history.
//
// Both guards have to be stepped around, and that is the point rather than an
// inconvenience. Every writer now refuses credential-shaped content, and the
// history filter removes it from any row that somehow holds it, so the state these
// tests are about — "a leaked secret survived deletion" — is no longer reachable
// through the public surface. It is still reachable ON DISK, on any database
// written before the guard existed, and that is exactly the population the purge
// exists for.
//
// So both halves go off: CreateFromCorpus for the writer, which is the unguarded
// statement documented as the corpus path, and setHistoryRedactor(nil) for the
// history filter, which is the seam's own API and the same thing a pre-#656 process
// did by never installing one. A test that reached this state through the writers
// would now be testing the guard instead of the purge.
func preGuardRow(t *testing.T, s *Store, m Memory) string {
	t.Helper()
	restore := setHistoryRedactor(nil)
	t.Cleanup(restore)
	m.Tags = nil
	id, err := s.CreateFromCorpus(context.Background(), testProject, m)
	if err != nil {
		t.Fatalf("CreateFromCorpus: %v", err)
	}
	return id
}
