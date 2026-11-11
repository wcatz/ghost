package reflection

import "github.com/wcatz/ghost/internal/memory"

// IsFrozen reports whether m is excluded from consolidation.
func IsFrozen(m memory.Memory) bool { return memory.IsFrozen(m) }

// IsConsolidatable reports whether m is eligible for consolidation.
func IsConsolidatable(m memory.Memory) bool { return memory.IsConsolidatable(m) }
