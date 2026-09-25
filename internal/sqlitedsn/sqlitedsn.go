// Package sqlitedsn centralizes SQLite URI construction shared by read-only
// Ghost paths.
package sqlitedsn

import (
	"fmt"
	"net/url"
)

// ReadOnlyURI returns a modernc.org/sqlite read-only URI for path. The timeout
// is explicit because read-only hooks intentionally differ from store opens.
func ReadOnlyURI(path string, busyTimeout int) string {
	u := url.URL{
		Scheme:   "file",
		Opaque:   (&url.URL{Path: path}).EscapedPath(),
		RawQuery: fmt.Sprintf("mode=ro&_pragma=busy_timeout(%d)", busyTimeout),
	}
	return u.String()
}
