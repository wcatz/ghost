package ai

import (
	"context"
	"errors"
)

// This file is the part of the harness capability-probe single-flight that
// BOTH probes share. `codexFeaturesFor` and `claudeCapabilitiesFor` each keep
// their own `sync.Map` cache and their own `singleflight.Group` — separate
// probes, with separate keys, separate retention and separate failure
// semantics — but the deduplication around them is one decision, and the two
// ways singleflight differs from the caller's own context are the same in both.

// probeContextError marks a probe that failed because the CONTEXT which ran it
// is what killed it, as distinct from a probe that failed on its own terms.
//
// The distinction has to be made by the goroutine that ran the probe, and it
// cannot be made by the follower afterwards: `exec.CommandContext` kills the
// child and `Wait` reports the KILL ("signal: killed"), never the context
// error, so the error a follower receives is textually identical whether the
// probe hit a real failure or the leader's context died. Those two demand
// opposite responses — one is this caller's answer, the other is a leader's
// accident that a live caller must not inherit — and guessing between them
// would either refuse an unrelated turn or re-probe a genuinely broken binary.
//
// The text is the wrapped error's, so the only thing that changes is the type.
// No caller's rendered error changes because of this.
type probeContextError struct{ err error }

func (e probeContextError) Error() string { return e.err.Error() }

func (e probeContextError) Unwrap() error { return e.err }

// isSharedProbeCancellation reports whether err is the marker's error AND this
// caller's own context is still live. Both halves are required.
//
// The marker is necessary: the leader's probe fails the same way as a real
// failure, and a follower cannot tell them apart. The live-context half is
// necessary too, because the LEADER sees the marker as well — and a leader
// whose context has died must report that rather than start a fresh attempt
// under a context that is already dead. That asymmetry is the whole reason a
// dead caller cannot be handled by re-probing: its answer is unusable either
// way, so it gets its own `ctx.Err()` from the select in each probe.
func isSharedProbeCancellation(err error, ctx context.Context) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}
	var marked probeContextError
	return errors.As(err, &marked)
}
