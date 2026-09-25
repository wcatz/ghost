//go:build !windows && !linux

package fileguard

import "context"

func nativeOpenProbe(context.Context, string) (inUse, known bool, err error) {
	return false, false, nil
}
