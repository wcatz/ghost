package ai

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var defaultTimeout = 5 * time.Minute

type noDefaultBoundKey struct{}

// WithoutDefaultBound returns ctx marked so harness clients do not apply the default timeout.
func WithoutDefaultBound(ctx context.Context) context.Context {
	return context.WithValue(ctx, noDefaultBoundKey{}, true)
}

// DefaultBoundRemoved reports whether ctx was marked by WithoutDefaultBound.
func DefaultBoundRemoved(ctx context.Context) bool {
	v, _ := ctx.Value(noDefaultBoundKey{}).(bool)
	return v
}

type defaultBoundCause struct{ d time.Duration }

func (c defaultBoundCause) Error() string {
	return fmt.Sprintf("cut at the harness client's default of %v per call, because the caller set no deadline", c.d)
}

func boundHarnessCall(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok || DefaultBoundRemoved(ctx) {
		return ctx, func() {}
	}
	return context.WithTimeoutCause(ctx, defaultTimeout, defaultBoundCause{d: defaultTimeout})
}

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
