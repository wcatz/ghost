// Package procstat answers process-identity questions shared by packages that
// must not trust a bare PID. A PID number identifies a process instance only
// until it exits, after which the OS may hand the same number to an unrelated
package procstat
