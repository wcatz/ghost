package main

import (
	"context"
	"io"
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
		_, _, _, _, err := applyReflection(
			context.Background(), f, "p1",
			projectMemories("the relay listens on 2222", "the deploy token is "+credential),
			nil, "since", false,
			nil)
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
	// The removal is reported, and the report is diagnosable: a discard that
	// leaves no trace is indistinguishable from a proposal the model never
	// emitted. Format, category and length, never the content.
	if !strings.Contains(stderr, "GitHub personal access token") || !strings.Contains(stderr, "not applied") {
		t.Errorf("the removal was not reported with its format:\n%s", stderr)
	}
	// Neither the drop line nor the audit's own warning line may print the value.
	if strings.Contains(stderr, credential) {
		t.Errorf("the reflect report printed the credential:\n%s", stderr)
	}
}

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	var out strings.Builder
	captureOutput(t, &out, &out, fn)
	return out.String()
}

// captureOutput runs fn with both standard streams redirected into w, so a test
// can assert on everything a command reports. The proposal listing, the "Applied"
// summary and the restore hint are stdout; the drop and signature notes are
// stderr, and a guarantee about "the report" has to cover both.
func captureOutput(t *testing.T, stdout, stderr io.Writer, fn func()) {
	t.Helper()
	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe stdout: %v", err)
	}
	rErr, wErr, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe stderr: %v", err)
	}
	prevOut, prevErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = wOut, wErr

	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(stdout, rOut); done <- struct{}{} }()
	go func() { _, _ = io.Copy(stderr, rErr); done <- struct{}{} }()

	fn()

	os.Stdout, os.Stderr = prevOut, prevErr
	_ = wOut.Close()
	_ = wErr.Close()
	<-done
	<-done
	_ = rOut.Close()
	_ = rErr.Close()
}
