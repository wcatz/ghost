package ai

import "os"

// LiveTestsEnabled reports whether the billable live LLM tests may run.
//
// Plain `go test ./...` must never make a real harness call. It used to:
// internal/resolve/live_test.go and internal/supersede/relation_test.go gated
// on cli.Available(), which is fed by DetectSource(), so running the suite
// from inside a harness session detected that session's harness and ran the
// live classifier sets anyway — 48s in resolve and 50s in supersede on an
// ordinary test run.
//
// The gate is strictly GHOST_LIVE_TESTS=1 and there is deliberately no
// auto-detection anywhere in the decision. Two reasons:
//
//   - The question is not "is a harness available" but "was this asked for".
//     Availability says nothing about intent, and a shell that happens to
//     carry CLAUDECODE=1 has stated no intent at all.
//   - A truthy parse ("true", "on") would let a stray line in a shell profile
//     quietly re-enable spend. An explicit "1" is a decision someone had to
//     type.
//
// Selecting WHICH harness to call stays separate: GHOST_TEST_SOURCE, else
// DetectSource(), is consulted only after this returns true. Auto-detection is
// fine for choosing an instrument once you have been asked to play.
func LiveTestsEnabled() bool {
	return os.Getenv("GHOST_LIVE_TESTS") == "1"
}
