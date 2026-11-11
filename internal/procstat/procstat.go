// Package procstat answers process-identity questions shared by packages that must not trust a bare PID.
// It records and checks process creation tokens so a recycled PID is not mistaken for a live owner.
package procstat
