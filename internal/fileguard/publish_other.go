//go:build !windows && !linux

package fileguard

import "errors"

func publishNoReplace(string, string) error {
	return errors.New("atomic no-replace publication is unavailable on this platform")
}
