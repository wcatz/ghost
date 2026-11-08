package reflection

import "github.com/wcatz/ghost/internal/memory"

// IsFrozen reports whether reflect will never consolidate m.
// Pinned, persistent, manual, builtin and resolved rows are never
// consolidated; the predicate is memory.IsFrozen so health and status
// disclose the same rows reflect excludes.
func IsFrozen(m memory.Memory) bool { return memory.IsFrozen(m) }

// IsConsolidatable reports whether m is eligible for consolidation.
func IsConsolidatable(m memory.Memory) bool { return memory.IsConsolidatable(m) }
