package main

import (
	"context"
	"strings"
	"testing"
)

// TestCredentialRemovalIsReportedOnlyAfterAReplace pins that the removal claim
// is a statement about a write that happened, not a prediction about one.
//
// The per-drop note used to say "any stored memory it carried forward is
// REMOVED from the project", unconditionally, before anything knew whether a
// replace would run. It would not, whenever the drop empties the project list —
// applyReflection returns early on two empty lists, and ApplyReflection skips
// ReplaceNonManual unless there is a project set. A project whose only proposal
// is the credential is exactly the pre-guard single-row case, so the one
// situation an operator most needs an accurate report about is the one where the
// claim was least true: nothing is written, the stored row survives, and the log
// says it was deleted.
//
// A false removal claim is worse than no claim. It closes the incident in the
// operator's head while the value sits in the database.
func TestCredentialRemovalIsReportedOnlyAfterAReplace(t *testing.T) {
	credential := "ghp_" + strings.Repeat("a1B2c3D4e5F6", 3) + "AbCd"
	const removal = "were REMOVED by the project replace"

	t.Run("a surviving proposal means a replace ran, so the removal is real", func(t *testing.T) {
		f := &fakeReflectionApplier{}
		stderr := captureStderr(t, func() {
			_, _, _, err := applyReflection(
				context.Background(), f, "p1",
				projectMemories("the relay listens on 2222", "the deploy token is "+credential),
				nil, "since", false,
			)
			if err != nil {
				t.Fatalf("applyReflection: %v", err)
			}
		})
		if f.calls != 1 {
			t.Fatalf("ApplyReflection called %d times, want 1", f.calls)
		}
		if !strings.Contains(stderr, removal) {
			t.Errorf("the replace ran but the removal was not reported:\n%s", stderr)
		}
		if strings.Contains(stderr, credential) {
			t.Errorf("the report printed the credential:\n%s", stderr)
		}
	})

	t.Run("the drop emptying the set means no replace, so no removal is claimed", func(t *testing.T) {
		f := &fakeReflectionApplier{}
		stderr := captureStderr(t, func() {
			_, _, _, err := applyReflection(
				context.Background(), f, "p1",
				projectMemories("the deploy token is "+credential),
				nil, "since", false,
			)
			if err != nil {
				t.Fatalf("applyReflection: %v", err)
			}
		})
		if f.calls != 0 {
			t.Errorf("ApplyReflection called %d times, want 0 — the drop left nothing to write", f.calls)
		}
		if strings.Contains(stderr, removal) {
			t.Errorf("a removal was claimed with no replace to have done it — the stored row survives and the report must not say otherwise:\n%s", stderr)
		}
		// The drop itself is still reported, because the proposal was still not
		// applied and an operator still needs to know that.
		if !strings.Contains(stderr, "not applied") {
			t.Errorf("the drop was not reported at all:\n%s", stderr)
		}
		if strings.Contains(stderr, credential) {
			t.Errorf("the report printed the credential:\n%s", stderr)
		}
	})

	t.Run("a failed apply claims no removal", func(t *testing.T) {
		f := &fakeReflectionApplier{err: context.DeadlineExceeded}
		stderr := captureStderr(t, func() {
			_, _, _, err := applyReflection(
				context.Background(), f, "p1",
				projectMemories("the relay listens on 2222", "the deploy token is "+credential),
				nil, "since", false,
			)
			if err == nil {
				t.Fatal("applyReflection: want the store's error")
			}
		})
		if strings.Contains(stderr, removal) {
			t.Errorf("a removal was claimed although the apply failed, so nothing was replaced:\n%s", stderr)
		}
	})
}
