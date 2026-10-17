package main

import (
	"context"
	"testing"
	"time"
)

// TestTheInterruptRecordNamesOnlyAStartedPhase pins the two shapes a signal
// leaves and the one shape that records nothing.
//
// The phase the chain never reached must NOT be named in phases_failed: the
// old shape recorded the next phase in line with the bare cause "context
// canceled" — a claim about a process that was never meaningfully started, and
// the phase the signal caught in flight, which did start, is the one that may
// be named. And a chain that finished every enabled phase records nothing at
// all: the signal arrived after the work, so nothing is paused and the record
// must not invent a phase to blame.
func TestTheInterruptRecordNamesOnlyAStartedPhase(t *testing.T) {
	for _, tc := range []struct {
		name       string
		running    string
		next       string
		tail       string
		wantOK     bool
		wantPhases []string
		wantCause  string
	}{
		{
			// The signal landed between phases: it was never reached, so the
			// record names no phase and the cause is the only place it appears.
			name:      "between phases the never-started phase is named by nobody",
			next:      "resolve",
			wantOK:    true,
			wantCause: "interrupted by SIGTERM before resolve",
		},
		{
			// The signal caught a phase in flight: it started, so it is named,
			// and the signal itself is its cause rather than "context canceled".
			name:       "a phase in flight is named and the signal is its cause",
			running:    "resolve",
			wantOK:     true,
			wantPhases: []string{"resolve"},
			wantCause:  "interrupted by SIGTERM while running resolve",
		},
		{
			// What the phase printed before it stopped travels with the cause:
			// it is the only record of what the phase was doing.
			name:       "the caught phase's output travels with the cause",
			running:    "resolve",
			tail:       "some harness output\n",
			wantOK:     true,
			wantPhases: []string{"resolve"},
			wantCause:  "interrupted by SIGTERM while running resolve\nsome harness output",
		},
		{
			// Every enabled phase completed: the signal cut nothing short, so
			// there is nothing to record.
			name:   "every phase completed records nothing",
			wantOK: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			phases, cause, ok := interruptOutcome(tc.running, tc.next, tc.tail)
			if ok != tc.wantOK {
				t.Fatalf("interrupted = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if len(phases) != len(tc.wantPhases) {
				t.Fatalf("phases = %v, want %v", phases, tc.wantPhases)
			}
			for i := range phases {
				if phases[i] != tc.wantPhases[i] {
					t.Errorf("phases[%d] = %q, want %q", i, phases[i], tc.wantPhases[i])
				}
			}
			if cause != tc.wantCause {
				t.Errorf("cause = %q, want %q", cause, tc.wantCause)
			}
		})
	}
}

// TestTheSweepIsAwaitedOnlyForAnInterruptedRun pins the three ways the wait
// after the phase loop ends, and the one way it must not.
//
// The escalation's sweep has to land before the coordinator releases the
// per-project claim and exits, so a signalled run waits for it. A run no signal
// stopped owes nothing — the run that just succeeded must not be delayed by a
// phase's escalation — and a run in which no phase ever launched one has
// nothing to await. The bound is a backstop, so a sweep that never returns must
// not hang the exit.
func TestTheSweepIsAwaitedOnlyForAnInterruptedRun(t *testing.T) {
	closed := func() <-chan time.Time { c := make(chan time.Time); close(c); return c }
	never := make(chan time.Time)
	// within counts the calls, so a branch that must not start the backstop can
	// say so rather than merely returning at the same instant.
	newClock := func(ch <-chan time.Time, calls *int) func(time.Duration) <-chan time.Time {
		return func(time.Duration) <-chan time.Time { *calls++; return ch }
	}

	t.Run("a run no signal stopped does not wait", func(t *testing.T) {
		done := make(chan struct{}) // would block forever if awaited
		calls := 0
		awaitPhaseSweep(context.Background(), done, newClock(never, &calls))
		if calls != 0 {
			t.Errorf("the backstop clock was started %d time(s) for a run no signal stopped", calls)
		}
	})

	t.Run("a run that never launched an escalation does not wait", func(t *testing.T) {
		root, stop := context.WithCancel(context.Background())
		stop()
		calls := 0
		awaitPhaseSweep(root, nil, newClock(never, &calls))
		if calls != 0 {
			t.Errorf("the backstop clock was started %d time(s) with no escalation to await", calls)
		}
	})

	t.Run("an interrupted run returns when the sweep does", func(t *testing.T) {
		root, stop := context.WithCancel(context.Background())
		stop()
		done := make(chan struct{})
		close(done)
		returned := make(chan struct{})
		go func() {
			defer close(returned)
			// never: the bound must not be what ends this wait, so an
			// implementation that ignored the closed sweep would hang here and
			// be caught rather than passing for the wrong reason. (A select
			// evaluates every operand up front, so the clock is consulted
			// either way; what matters is which case commits.)
			awaitPhaseSweep(root, done, func(time.Duration) <-chan time.Time { return never })
		}()
		select {
		case <-returned:
		case <-time.After(5 * time.Second):
			t.Fatal("the wait did not return although the sweep had: it is waiting on the bound instead of the sweep")
		}
	})

	t.Run("an interrupted run gives up at the backstop", func(t *testing.T) {
		root, stop := context.WithCancel(context.Background())
		stop()
		calls := 0
		// The sweep never returns; the bound is what must end the wait, or a
		// wedged escalation would hang the coordinator's exit.
		awaitPhaseSweep(root, make(chan struct{}), newClock(closed(), &calls))
		if calls != 1 {
			t.Errorf("the backstop clock was started %d time(s), want exactly 1", calls)
		}
	})
}

// TestTheInterruptIsAppendedToAnEarlierFailure pins how a signal's outcome
// lands in the marker's two fields when the run has already failed.
//
// The marker carries ONE error string and keeps the FIRST cause, so an
// interrupt that follows an earlier phase's failure must be appended to that
// cause rather than passed in as a second one: otherwise the phase the signal
// caught is named in phases_failed while its own cause — the signal, and the
// output tail that is the only record of what it was doing — is silently
// dropped, and the alert reads as if the earlier phase were the whole story.
//
// The one shape with no phase to name and no failure to append to is the run
// itself, returned as a direct-write cause because FinishLifecycleRun reads an
// empty failure list as success and would clear the record.
func TestTheInterruptIsAppendedToAnEarlierFailure(t *testing.T) {
	for _, tc := range []struct {
		name         string
		failed       []string
		err          string
		running      string
		next         string
		tail         string
		wantFailed   []string
		wantErr      string
		wantRunCause string
	}{
		{
			// The regression: an earlier phase failed, then the signal caught a
			// later phase. Both facts have to survive in the one record.
			name:       "a phase caught in flight after an earlier failure keeps both causes",
			failed:     []string{"reflect"},
			err:        "reflect exploded",
			running:    "resolve",
			tail:       "partial harness output\n",
			wantFailed: []string{"reflect", "resolve"},
			wantErr:    "reflect exploded; interrupted by SIGTERM while running resolve\npartial harness output",
		},
		{
			name:       "a phase caught in flight with nothing failed sets its cause",
			running:    "resolve",
			wantFailed: []string{"resolve"},
			wantErr:    "interrupted by SIGTERM while running resolve",
		},
		{
			name:       "between phases after an earlier failure appends and names nobody",
			failed:     []string{"reflect"},
			err:        "reflect exploded",
			next:       "supersede",
			wantFailed: []string{"reflect"},
			wantErr:    "reflect exploded; interrupted by SIGTERM before supersede",
		},
		{
			name:         "between phases with nothing failed is written directly",
			next:         "supersede",
			wantRunCause: "interrupted by SIGTERM before supersede",
		},
		{
			name:       "a signal that cut nothing short leaves the bookkeeping alone",
			failed:     []string{"reflect"},
			err:        "reflect exploded",
			wantFailed: []string{"reflect"},
			wantErr:    "reflect exploded",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failed, err, runCause := applyInterrupt(tc.failed, tc.err, tc.running, tc.next, tc.tail)
			if len(failed) != len(tc.wantFailed) {
				t.Fatalf("phases_failed = %v, want %v", failed, tc.wantFailed)
			}
			for i := range failed {
				if failed[i] != tc.wantFailed[i] {
					t.Errorf("phases_failed[%d] = %q, want %q", i, failed[i], tc.wantFailed[i])
				}
			}
			if err != tc.wantErr {
				t.Errorf("error = %q, want %q", err, tc.wantErr)
			}
			if runCause != tc.wantRunCause {
				t.Errorf("direct-write cause = %q, want %q", runCause, tc.wantRunCause)
			}
		})
	}
}
