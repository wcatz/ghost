package main

// The consolidation deadline is the one on the consolidation context, not the
// maintenance context it derives from. reflection.consolidation_timeout_minutes
// bounds exactly one consolidation call, and a bare mechanical tier returns its
// result without ever consulting the context, so a run that reads the parent's
// error applies a pass that outlived its own bound and reports success.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/reflection"
)

// TestReflectRefusesAConsolidationPastItsDeadline drives the real SQLite tier,
// which ignores its context, under a bound that is already in the past. The
// result must become an error rather than a pass the apply step can use.
func TestReflectRefusesAConsolidationPastItsDeadline(t *testing.T) {
	_, err := consolidateBounded(context.Background(), reflection.NewSQLiteConsolidator(),
		reflection.ReflectionInput{CurrentContext: "ctx"}, time.Nanosecond)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want the expired consolidation deadline", err)
	}
}

// TestReflectAppliesAConsolidationInsideItsDeadline is the control: a pass that
// finishes within its bound still returns its result.
func TestReflectAppliesAConsolidationInsideItsDeadline(t *testing.T) {
	result, err := consolidateBounded(context.Background(), reflection.NewSQLiteConsolidator(),
		reflection.ReflectionInput{CurrentContext: "ctx"}, time.Minute)

	if err != nil {
		t.Fatalf("error = %v, want the result", err)
	}
	if result.LearnedContext != "ctx" {
		t.Errorf("learned context = %q, want the input's %q", result.LearnedContext, "ctx")
	}
}

// TestReflectRefusesACancelledConsolidation keeps the operator-stop guard: the
// consolidation context derives from the maintenance context, so a cancelled
// run is refused even though the mechanical tier never looks at it.
func TestReflectRefusesACancelledConsolidation(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := consolidateBounded(parent, reflection.NewSQLiteConsolidator(),
		reflection.ReflectionInput{CurrentContext: "ctx"}, time.Minute)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want the cancellation", err)
	}
}
