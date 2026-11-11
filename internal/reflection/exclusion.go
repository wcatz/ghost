package reflection

import "github.com/wcatz/ghost/internal/memory"

// IsFrozen reports whether reflect will never consolidate m.
// Pinned, persistent, manual, builtin and resolved rows are never
func IsFrozen(m memory.Memory) bool { return memory.IsFrozen(m) }

// IsConsolidatable reports whether m is eligible for consolidation.
func IsConsolidatable(m memory.Memory) bool { return memory.IsConsolidatable(m) }
