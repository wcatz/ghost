package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
)

// reflectCredential is a token-shaped value in the GitHub PAT format, assembled
// rather than written out because GitHub push protection matches that format
// anywhere in a diff and rejects the push (GH013) before review starts.
func reflectCredential() string { return "ghp_" + strings.Repeat("a1B2c3D4e5F6", 3) + "AbCd" }

// TestCredentialIsDroppedAfterTheGuardAuditNotBefore pins the ordering of the
// credential drop against the drop guard, which is the whole reason it lives
// here and not inside the LLM tier.
//
// The audit asks which INPUTS the consolidation output failed to account for,
// and executeOps emits every unclaimed input verbatim — so a memory the tier
// carried forward is byte-identical to the output that carried it. Filter the
// credential memory before the audit and the audit reads that input as
// unaccounted for, and both outcomes are wrong:
//
//   - RetainGuardedDrops re-adds it verbatim, so the credential is written back
//     and the drop is a no-op — after the audit has already printed its content
//     to stderr, which is the report leak the store's refusal path avoids; or
//   - some other output happens to cover 45% of its tokens, the audit stays
//     quiet, and ReplaceNonManual deletes the stored row with no --allow-drops —
//     the one deletion path the drop guard exists to close.
//
// Run after the audit, the credential memory is still accounted for by the
// output that carried it, so it is neither re-added nor deleted, and it simply
// never reaches the set that is written.
func TestCredentialIsDroppedAfterTheGuardAuditNotBefore(t *testing.T) {
	credential := reflectCredential()
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
	for _, m := range f.projectMems {
		if strings.Contains(m.Content, credential) {
			t.Errorf("a credential reached the store write: %q", m.Content)
		}
	}
	// The clean memory is untouched, so the drop is a removal and not a refusal
	// of the whole round.
	if len(f.projectMems) != 1 || !strings.Contains(f.projectMems[0].Content, "2222") {
		t.Errorf("the surviving project memories are not the one clean memory: %+v", f.projectMems)
	}
	// The report must not print the value either — the audit's own warning line
	// is the path that would.
	if strings.Contains(stderr, credential) {
		t.Errorf("the reflect report printed the credential:\n%s", stderr)
	}
}

// captureStderr runs fn with os.Stderr redirected, so a test can assert on what
// a command prints without the process's own output carrying the value.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	prev := os.Stderr
	os.Stderr = w
	var buf bytes.Buffer
	done := make(chan struct{})
	go func() { _, _ = buf.ReadFrom(r); close(done) }()
	fn()
	os.Stderr = prev
	_ = w.Close()
	<-done
	_ = r.Close()
	return buf.String()
}
