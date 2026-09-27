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
// replace would run. It would not whenever the project set the replace works
// from was empty — ApplyReflection skips ReplaceNonManual unless there is one,
// so a surviving global candidate under --promote-globals was enough — and the
// stored row survived while the log said it was deleted.
//
// A number is not available to print here and the note does not try: `dropped`
// counts proposals removed from the emitted set, and the count of rows the
// replace deleted is different in both directions — a fresh merge carried
// nothing, and a manual, builtin, pinned or resolved row is not in the replace's
// candidate set at all. So the note states the mechanism and where to look.
func TestCredentialRemovalIsReportedOnlyAfterAReplace(t *testing.T) {
	credential := "ghp_" + strings.Repeat("a1B2c3D4e5F6", 3) + "AbCd"

	t.Run("a surviving project proposal means the replace ran, so the note fires", func(t *testing.T) {
		f := &fakeReflectionApplier{}
		stderr := captureStderr(t, func() {
			_, _, _, _, _, applied, err := applyReflection(
				context.Background(), f, "p1",
				projectMemories("the relay listens on 2222", "the deploy token is "+credential),
				nil, "since", false,
				nil)
			if err != nil {
				t.Fatalf("applyReflection: %v", err)
			}
			if !applied {
				t.Error("applied = false, want true — a project proposal survived, so a replace ran")
			}
		})
		if f.calls != 1 {
			t.Fatalf("ApplyReflection called %d times, want 1", f.calls)
		}
		if !strings.Contains(stderr, "project replace has just removed") {
			t.Errorf("the replace ran but the removal was not reported:\n%s", stderr)
		}
		// It must not assert a count of deleted rows: it cannot know one.
		if strings.Contains(stderr, "were REMOVED by the project replace") {
			t.Errorf("the note claims a count of deleted rows, which is not knowable here:\n%s", stderr)
		}
		if strings.Contains(stderr, credential) {
			t.Errorf("the report printed the credential:\n%s", stderr)
		}
	})

	t.Run("the drop emptying the set means no replace, so no removal is claimed", func(t *testing.T) {
		f := &fakeReflectionApplier{}
		stderr := captureStderr(t, func() {
			_, _, _, _, _, applied, err := applyReflection(
				context.Background(), f, "p1",
				projectMemories("the deploy token is "+credential),
				nil, "since", false,
				nil)
			if err != nil {
				t.Fatalf("applyReflection: %v", err)
			}
			if applied {
				t.Error("applied = true, want false — the drop left nothing to write")
			}
		})
		if f.calls != 0 {
			t.Errorf("ApplyReflection called %d times, want 0 — the drop left nothing to write", f.calls)
		}
		if strings.Contains(stderr, "project replace has just removed") {
			t.Errorf("a removal was claimed with no replace to have done it — the stored row survives and the report must not say otherwise:\n%s", stderr)
		}
		if !strings.Contains(stderr, "not applied") {
			t.Errorf("the drop was not reported at all:\n%s", stderr)
		}
		if strings.Contains(stderr, credential) {
			t.Errorf("the report printed the credential:\n%s", stderr)
		}
	})

	t.Run("a surviving global with an empty project set means no replace either", func(t *testing.T) {
		// The shape that made a `dropped > 0` gate insufficient: the project set
		// the replace works from is empty, so ReplaceNonManual never runs, while
		// a global candidate is still promoted and written to _global.
		f := &fakeReflectionApplier{resultPromoted: 1}
		stderr := captureStderr(t, func() {
			_, _, _, _, _, _, err := applyReflection(
				context.Background(), f, "p1",
				projectMemories("the deploy token is "+credential),
				globals("tabs, not spaces, in every repository we touch"),
				"since", true,
				nil)
			if err != nil {
				t.Fatalf("applyReflection: %v", err)
			}
		})
		if strings.Contains(stderr, "project replace has just removed") {
			t.Errorf("a project-removal was claimed although no project replace ran — the credential's stored row survives:\n%s", stderr)
		}
	})

	t.Run("a failed apply claims no removal", func(t *testing.T) {
		f := &fakeReflectionApplier{err: context.DeadlineExceeded}
		stderr := captureStderr(t, func() {
			_, _, _, _, _, applied, err := applyReflection(
				context.Background(), f, "p1",
				projectMemories("the relay listens on 2222", "the deploy token is "+credential),
				nil, "since", false,
				nil)
			if err == nil {
				t.Fatal("applyReflection: want the store's error")
			}
			if applied {
				t.Error("applied = true, want false — the apply failed")
			}
		})
		if strings.Contains(stderr, "project replace has just removed") {
			t.Errorf("a removal was claimed although the apply failed, so nothing was replaced:\n%s", stderr)
		}
	})
}
