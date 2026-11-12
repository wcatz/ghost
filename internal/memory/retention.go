package memory

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

const (
	RetentionSession    = "session"
	RetentionProject    = "project"
	RetentionPersistent = "persistent"
)

// SessionTTL is the default lifetime for a session-tier memory.
const SessionTTL = 24 * time.Hour

// DefaultSessionGrace is how long past expiry a session row is left alone by prune.
const DefaultSessionGrace = 7 * 24 * time.Hour

var retentionTiers = []string{RetentionSession, RetentionProject, RetentionPersistent}

var validRetention = map[string]bool{
	RetentionSession:    true,
	RetentionProject:    true,
	RetentionPersistent: true,
}

// RetentionValues returns the supported retention tiers.
func RetentionValues() []string {
	out := make([]string, len(retentionTiers))
	copy(out, retentionTiers)
	return out
}

// IsValidRetention reports whether tier is supported.
func IsValidRetention(tier string) bool { return validRetention[tier] }

// ErrInvalidRetention reports an invalid retention tier.
var ErrInvalidRetention = errors.New("invalid retention tier")

// InvalidRetentionError returns an error describing an invalid retention tier.
func InvalidRetentionError(got string) error {
	return fmt.Errorf("%w %s: must be one of %s — %s (useful for this conversation only), %s (the default: persists until resolved), %s (keep-forever, exempt from consolidation, supersede, resolve and pruning)",
		ErrInvalidRetention, RefusalArg("retention", got), strings.Join(retentionTiers, ", "),
		RetentionSession, RetentionProject, RetentionPersistent)
}

// NormalizeRetention returns tier or the default when empty.
func NormalizeRetention(tier string) (string, error) {
	if tier == "" {
		return RetentionProject, nil
	}
	if !IsValidRetention(tier) {
		return "", InvalidRetentionError(tier)
	}
	return tier, nil
}

// RetentionExempt reports whether a memory is exempt from lifecycle passes and pruning.
func RetentionExempt(m Memory) bool { return m.Retention == RetentionPersistent }

const retentionExemptSQL = "retention <> '" + RetentionPersistent + "'"

const (
	sessionDecayTau   = 7.0
	sessionDecayFloor = 0.5
)

// RetentionDecayFactor returns the retention multiplier applied to decay.
func RetentionDecayFactor(tier string, pinned bool, ageDays float64) float64 {
	if pinned || tier != RetentionSession {
		return 1.0
	}
	return math.Max(sessionDecayFloor, 1.0/(1.0+ageDays/sessionDecayTau))
}

var retentionDecayFactorSQL = fmt.Sprintf(`
    * CASE
        WHEN pinned = 1 THEN 1.0
        WHEN retention = '%s' THEN
            MAX(%s, 1.0 / (1.0 + (julianday('now') - julianday(created_at)) / %s))
        ELSE 1.0
    END
`, RetentionSession,
	strconv.FormatFloat(sessionDecayFloor, 'f', 1, 64),
	strconv.FormatFloat(sessionDecayTau, 'f', 1, 64))
