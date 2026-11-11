package ai

import (
	"context"
	"errors"
)

type probeContextError struct{ err error }

func (e probeContextError) Error() string { return e.err.Error() }

func (e probeContextError) Unwrap() error { return e.err }

func isSharedProbeCancellation(err error, ctx context.Context) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}
	var marked probeContextError
	return errors.As(err, &marked)
}
