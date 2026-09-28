package memory

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Retention tiers: how long a memory is wanted, and what may be done to it.
//
// The three values are a lifecycle, not a size. `session` is a fact that is only
// true for the conversation that produced it and is expected to be swept up
// later; `project` is what Ghost has always stored — it persists until somebody
// resolves or deletes it; `persistent` is a row the user has declared
// off-limits, and it is the ONLY tier a default may not take back.
const (
	// RetentionSession is useful for the current conversation. Ghost derives an
	// expiry for it on save (expires_at) and `ghost prune` is the one command
	// that removes an expired one, never automatically and never without a grace
	// period and an explicit --apply.
	RetentionSession = "session"
	// RetentionProject is the default, and the tier every row that predates
	// schema v19 has after it is migrated. It expires only when somebody
	// resolves or deletes it.
	RetentionProject = "project"
	// RetentionPersistent is a user-declared keep-forever. It is exempt from
	// consolidation (ReplaceNonManual), from the resolve and supersede passes,
	// and from pruning — and from the ranking demotions those passes' output
	// would otherwise cause, because a protection the memory loses one pass
	// later is not a protection.
	RetentionPersistent = "persistent"
)

// SessionTTL is how long a session-tier memory is wanted from the moment it is
// saved. It is short because the tier is a claim about a conversation, and the
// corpus that accumulates from treating every ephemeral fact as durable is the
// growth #542 documents. It is NOT how long a session memory survives: prune
// additionally waits out a grace period (see DefaultSessionGrace), so a row
// stays in the store for at least that long after it expires even if nothing
// ever reads it again.
const SessionTTL = 24 * time.Hour

// DefaultSessionGrace is how long past its expiry an untouched session row is
// left alone by `ghost prune`. It is the safety margin between "Ghost no longer
// wants this" and "Ghost deletes this": a memory that is still being read keeps
// its last activity moving, and one nobody has touched for a week is a fact that
// turned out not to matter.
//
// The activity it is measured from is COALESCE(last_accessed, updated_at,
// created_at). last_accessed is what the name says and is preferred when it
// exists; no production surface records it today (Store.Touch has no caller), so
// in practice this is the row's last WRITE, and the grace is "a week since
// anything changed this row". That is the weaker of the two readings, so the
// default is a week rather than a day.
const DefaultSessionGrace = 7 * 24 * time.Hour

// The Go-side source of truth for the memories retention CHECK constraint, in
// the same role validCategories plays for the category CHECK and for the same
// reason: a writer that accepts a caller-supplied tier validates it against this
// set so the refusal names the three values in the caller's own words, instead
// of failing the whole INSERT with SQLite's own wording and taking the
// transaction with it.
//
// Ordered from the shortest life to the longest, so every surface that lists the
// vocabulary reads them in the order a reader should think about them.
var retentionTiers = []string{RetentionSession, RetentionProject, RetentionPersistent}

var validRetention = map[string]bool{
	RetentionSession:    true,
	RetentionProject:    true,
	RetentionPersistent: true,
}

// RetentionValues returns the three tiers, shortest life first. It is a copy:
// every surface that quotes the vocabulary (the MCP save tool's refusal, the CLI
// usage, the docs) gets its own slice, so a caller that sorts or edits it cannot
// rewrite the vocabulary the next caller reads.
func RetentionValues() []string {
	out := make([]string, len(retentionTiers))
	copy(out, retentionTiers)
	return out
}

// IsValidRetention reports whether tier is one of the three the schema CHECK
// accepts. The empty string is NOT a tier: it is what a caller who said nothing
// sends, and it is resolved to RetentionProject by NormalizeRetention.
func IsValidRetention(tier string) bool { return validRetention[tier] }

// ErrInvalidRetention is the sentinel every tier refusal wraps, so a surface
// that has to tell a caller's typo from a store that would not open can ask
// rather than matching on the message.
var ErrInvalidRetention = errors.New("invalid retention tier")

// InvalidRetentionError is the one refusal, built once so the MCP save tool, the
// CLI and the store cannot word it three ways. It names the value that was
// refused and the three that were available: a caller who typed "forever" learns
// which word to type, rather than that something was wrong.
func InvalidRetentionError(got string) error {
	return fmt.Errorf("%w %q: must be one of %s — %s (useful for this conversation only), %s (the default: persists until resolved), %s (keep-forever, exempt from consolidation, supersede, resolve and pruning)",
		ErrInvalidRetention, got, strings.Join(retentionTiers, ", "),
		RetentionSession, RetentionProject, RetentionPersistent)
}

// NormalizeRetention resolves what a caller sent to a tier: the empty string is
// the default, and anything that is not a tier is refused. It is the one place
// the default is applied, so a writer that forgets to call it fails its column's
// CHECK rather than storing a fourth, readerless value.
func NormalizeRetention(tier string) (string, error) {
	if tier == "" {
		return RetentionProject, nil
	}
	if !IsValidRetention(tier) {
		return "", InvalidRetentionError(tier)
	}
	return tier, nil
}

// RetentionExempt reports whether a memory's tier puts it beyond every
// lifecycle pass: consolidation, supersede, resolve, and pruning.
//
// It exists as one predicate because the exemption is one decision with four
// consequences, and four predicates is four chances for a new pass to read the
// column its own way. Note what it does NOT say: a persistent memory is still
// searchable, still injected, and still deletable by ghost_memory_delete or
// `ghost project delete` — the user asked for it to be kept from the automated
// passes, not from the user's own command.
func RetentionExempt(m Memory) bool { return m.Retention == RetentionPersistent }

// retentionExemptSQL is the same exemption as a statement fragment, for the
// queries that decide membership in SQL. It is written as an inequality rather
// than an equality with the other two tiers so that a row with an unexpected
// value — a database an older build wrote, a hand-edited one — is treated as
// NOT exempt. The safe direction for every one of these statements is the
// crowded corpus, not the protected one.
const retentionExemptSQL = "retention <> '" + RetentionPersistent + "'"

// The bounded ranking decay for session rows.
//
// The problem it solves: a session memory is by definition recent, and recency
// is the strongest signal in the composite score, so a corpus full of
// conversation-scoped facts outranks the durable knowledge that actually
// answers questions. A session row therefore decays, and it decays to a FLOOR
// rather than to nothing — a session memory stays findable for the life of the
// corpus, it just never outranks a durable memory on recency alone.
const (
	// sessionDecayTau is the decay constant in days: a session row is worth half
	// a durable row at seven days old, and the floor is reached at seven.
	sessionDecayTau = 7.0
	// sessionDecayFloor is the bound. 0.5 is the whole guarantee — a session
	// memory's tier multiplier is never better than half — and it is well above
	// the category floors (0.15/0.3) so a session row of a never-decaying
	// category is still demoted relative to its durable twin, which is the point.
	sessionDecayFloor = 0.5
)

// RetentionDecayFactor is the tier's own multiplier on a memory's decay, applied
// inside DecayFactor and reported by explain as a named signal of its own.
//
// It is 1.0 for every tier but session, which is what makes a durable memory's
// score exactly what it was before tiers existed — the default has to cost
// nothing, or adopting Ghost would silently re-rank every existing corpus.
func RetentionDecayFactor(tier string, ageDays float64) float64 {
	if tier != RetentionSession {
		return 1.0
	}
	return math.Max(sessionDecayFloor, 1.0/(1.0+ageDays/sessionDecayTau))
}

// retentionDecayFactorSQL is RetentionDecayFactor as SQL, for the ranking
// expression GetTopMemories and the session-start hook share. It multiplies the
// category CASE rather than replacing it: a session row of a decaying category
// decays by both, and DecayFactor is the Go mirror of the two together (there is
// a parity test over the pair).
//
// A PINNED row is exempt here for the same reason the category CASE exempts it
// and DecayFactor returns 1.0 immediately for one: a pin is a full decay
// exemption, and a tier formula that decayed the very row the category formula
// next to it exempted would split one score against itself. That is not a
// theoretical divergence: the session-start pre-window ranks in SQL only and
// cuts before the Go re-score, so a pinned session row the tier decayed would be
// dropped from the block while every other path treated it as brand new. The
// branch sits first for the same reason it sits first in the category CASE: the
// pin is the strongest claim, so it wins before the tier is consulted.
//
// The numbers are formatted from the Go constants rather than written out, so
// the bound that is documented above and the bound SQLite evaluates cannot
// disagree — which is the one way this pair of formulas could rot silently,
// since a reader comparing the two would be comparing prose with a query.
// 'f' with one place is exact for both values and never produces exponent
// notation, which SQL would not parse.
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
