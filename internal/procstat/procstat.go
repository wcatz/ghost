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
// obtained the same way on the same machine. Check distinguishes unknown
// access from a proven exit; IsAlive retains the historical fail-open boolean
// contract for callers that do not need tri-state safety.
package procstat

// State is the result of a process-identity probe. Unknown is intentionally
// distinct from Dead: callers that remove files must not treat an inaccessible
// or unsupported probe as proof that a process exited.
type State uint8

const (
	// StateUnknown means the platform could not prove liveness or death.
	StateUnknown State = iota
	// StateAlive means the PID and optional creation token identify a live process.
	StateAlive
	// StateDead means the PID is proven absent or its creation token is stale.
	StateDead
)
