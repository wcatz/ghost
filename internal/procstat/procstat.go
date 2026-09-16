// Package procstat answers process-identity questions shared by packages that
// must not trust a bare PID. A PID number identifies a process instance only
// until it exits, after which the OS may hand the same number to an unrelated
// process; Ghost's .pid files (internal/mcpinit) and scratch owner markers
// (internal/scratch) therefore record a process creation-time token alongside
// the PID and re-derive it at read time, so a recycled PID is detected instead
// of mistaken for a live owner.
//
// The token is opaque: it is never interpreted as wall-clock time or compared
// across platforms or machines, only for exact equality against a token
// obtained the same way on the same machine. Both functions fail open to
// PID-only semantics when the platform cannot supply a token (unsupported OS,
// unreadable proc entry, permission denied): a transient token-read failure
// never reports a live process as dead.
package procstat
