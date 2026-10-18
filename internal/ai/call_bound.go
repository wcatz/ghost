package ai

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// defaultTimeout bounds a harness call when the caller's context carries no
// deadline and has not removed the bound on purpose (WithoutDefaultBound) —
// otherwise a stalled subprocess (auth prompt, network stall, hung MCP init
// inside it) blocks the classify/reflect loop indefinitely, since
// resolve/supersede classify candidates one at a time. It is a variable only so
// a test can shorten it; nothing else assigns it.
var defaultTimeout = 5 * time.Minute

// noDefaultBoundKey marks a context whose caller removed the bound on purpose.
type noDefaultBoundKey struct{}

// WithoutDefaultBound returns ctx marked so that a harness client does NOT
// apply defaultTimeout to it. A client cannot tell "the caller wants no bound"
// from "the caller forgot a deadline" by looking at ctx.Deadline alone, so the
// first is said out loud and the second keeps the default. The marker never
// lifts a deadline the context does carry: a caller's own deadline always
// wins. The one caller is the consolidation path for an explicit
// reflection.consolidation_timeout_minutes: 0 (cmd/ghost consolidationContext).
func WithoutDefaultBound(ctx context.Context) context.Context {
	return context.WithValue(ctx, noDefaultBoundKey{}, true)
}

// DefaultBoundRemoved reports whether ctx was marked by WithoutDefaultBound.
func DefaultBoundRemoved(ctx context.Context) bool {
	v, _ := ctx.Value(noDefaultBoundKey{}).(bool)
	return v
}

// defaultBoundCause is the cancellation cause of a call the client's own
// default cut, so the error can name the default: the harness itself only
// reports a bare SIGTERM with nothing on its output.
type defaultBoundCause struct{ d time.Duration }

func (c defaultBoundCause) Error() string {
	return fmt.Sprintf("cut at the harness client's default of %v per call, because the caller set no deadline", c.d)
}

// boundHarnessCall is the per-call bound every harness client's run applies,
// in this order: a deadline on ctx is kept as it is; a ctx marked
// WithoutDefaultBound gets no timer; any other ctx gets defaultTimeout. The
// capability and version checks inside a call keep their own 10s bounds either
// way, because they derive them from the returned context.
func boundHarnessCall(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok || DefaultBoundRemoved(ctx) {
		return ctx, func() {}
	}
	return context.WithTimeoutCause(ctx, defaultTimeout, defaultBoundCause{d: defaultTimeout})
}

// explainDefaultBound adds the default's name to err when the call failed
// because boundHarnessCall's default fired. A cut at the caller's own deadline,
// or a caller that was cancelled, is left as it was: context.Cause reports the
// first cancellation, so only the default's own expiry carries its cause.
func explainDefaultBound(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	var cause defaultBoundCause
	if errors.As(context.Cause(ctx), &cause) {
		return fmt.Errorf("%w; %v", err, cause)
	}
	return err
}
