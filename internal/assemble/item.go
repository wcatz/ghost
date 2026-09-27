package assemble

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// Item is the shared output of assembly: what both surfaces render, what an
// explanation describes, and what a context metric scores. One type for all
// three is what stops a renderer and a metric from disagreeing about what a
// memory is.
//
// The four timestamps are parsed output values. A validity value that cannot be
// parsed is nil here, so nil means "no readable claim" rather than a validity
// assertion.
type Item struct {
	ID, Category, Content string
	Tags                  []string
	Importance            float64
	Pinned                bool
	CreatedAt             time.Time
	Bytes                 int
	Bucket                string
	ProjectID             string
	Scope                 map[string]string
	ResolvedAt            *time.Time
	ValidFrom, ValidUntil *time.Time
	VerifiedAt            *time.Time
	// ValidityState is valid, future, expired, unverified or unset.
	ValidityState string
	Confidence    *float64
	Agent         string
	// SourceRef is the reference the writer recorded: a file, a commit, a URL.
	// It travels with the item for the same reason Agent does — it is what makes
	// a claim checkable, and an item that could not show it would make writing
	// it pointless.
	SourceRef string
	Score     float64
	// Source is the row's stored origin. It travels with the item because the
	// shared line labels it, and mcpInstructions tells the agent to trust that
	// label: a renderer that could not see the origin would have to drop the
	// label, and dropping it is a behaviour change on a live surface. The label
	// itself comes from memory.OriginClass, never from this field's literal
	// value, and the compatibility correction is scoped by ProjectID above.
	Source string
}

// Line renders the shared item prefix: the one line shape both surfaces emit for
// a memory, so a reader comparing search output with an injected block sees the
// same fields in the same place. Each surface keeps its own framing around it.
//
// The line carries no trailing newline; the caller joins lines.
func (i Item) Line() string {
	pin := ""
	if i.Pinned {
		pin = " [pinned]"
	}
	tags := ""
	if len(i.Tags) > 0 {
		if b, err := json.Marshal(i.Tags); err == nil {
			tags = " tags:" + string(b)
		}
	}
	resolved := ""
	if i.ResolvedAt != nil {
		resolved = " [resolved]"
	}
	// The legacy-seed correction is scoped to the global project, because that
	// is the only row it is about: a project row holding the shipped sentence is
	// the user's own material, and labelling it builtin would misattribute it and
	// remove the "no agent recorded" marker that says so. The item carries its
	// own project id precisely so this renderer does not have to guess.
	origin := ""
	if _, label := memory.OriginClass(memory.CanonicalOriginSourceForProject(i.ProjectID, i.Source, i.Content)); label != "" {
		origin = " source=" + label
	}
	return "- [" + i.Category + "] `" + i.ID + "` (" +
		strconv.FormatFloat(i.Importance, 'f', 1, 64) + pin + tags + resolved + ScopeLabel(i.Scope) +
		validityLabel(i.ValidityState, i.ValidFrom, i.ValidUntil, i.VerifiedAt) +
		ConfidenceLabel(i.Confidence) + AgentLabel(i.Agent) + SourceRefLabel(i.SourceRef) + origin +
		") " + quoteData(i.Content)
}

// AgentLabel renders the writing harness, or "" when the row records none.
//
// The value is delimited like every other stored text in an answer, which looks
// redundant against the closed vocabulary ai.SourceForClientName and
// ai.DetectSource produce (claude-code, opencode, codex, goose) — and is not. A
// portable artifact carries an `agent` field written by whoever exported it, and
// the same renderer serves imported rows, so "the writer only ever sets one of
// four tokens" is true of the MCP path rather than of the column.
func AgentLabel(agent string) string {
	if agent == "" {
		return ""
	}
	return " agent=" + quoteData(agent)
}

// SourceRefLabel renders the reference a claim was read from, or "" when the row
// records none. Free text on its way into a tool answer, so it is delimited: a
// path or URL carrying « or » would otherwise close the data block early and let
// its own tail read as instruction.
func SourceRefLabel(ref string) string {
	if ref == "" {
		return ""
	}
	return " source_ref=" + quoteData(ref)
}

// ConfidenceLabel renders a recorded belief, or "" when the row records none.
//
// It is a label and not a score. Stage 4's multiplier is pinned at 1.0, so
// nothing ranks on this number and a row with confidence 0.2 is not demoted — a
// reader who believes otherwise is being told something the pipeline does not do,
// which is the worse of the two errors available here.
func ConfidenceLabel(confidence *float64) string {
	if confidence == nil {
		return ""
	}
	return " confidence " + strconv.FormatFloat(*confidence, 'g', -1, 64)
}

// stampText renders one validity boundary, dropping the time of day only when it
// is midnight UTC — into memory.DateStampLayout, the same whole-day form a reader
// accepts. A validity window is almost always stated in days and "valid until
// 2026-10-01" is what the caller meant; anything with a time of day keeps it, since
// then the time was the point. The two are the same instant, so nothing is lost: a
// caller who wrote 2026-01-15T00:00:00Z reads back 2026-01-15, and one who wrote
// 2026-01-15T09:00:00Z reads back the hour.
func stampText(t *time.Time) string {
	if t.Hour() == 0 && t.Minute() == 0 && t.Second() == 0 {
		return t.Format(memory.DateStampLayout)
	}
	return t.Format(memory.StoredStampLayout)
}

// ScopeLabel renders a memory's scope for a listing, or "" when unscoped.
// Keys are sorted: map iteration order is random in Go, so an unsorted rendering
// would show the same scope in a different order on each read and look like the
// scope itself was changing.
//
// It is exported because the label is what makes scope legible, and a surface
// that re-derived it would be free to spell it differently: the search line and
// the session-start block would then show the same scope in two forms, and
// neither reader could be sure the two are the same claim. The value carries a
// leading space, so a caller places it wherever its own line puts an item's
// attributes.
func ScopeLabel(scope map[string]string) string {
	if len(scope) == 0 {
		return ""
	}
	keys := make([]string, 0, len(scope))
	for k := range scope {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString(" scope{")
	for i, k := range keys {
		if i > 0 {
			b.WriteString(" ")
		}
		b.WriteString(scopeToken(k))
		b.WriteString("=")
		b.WriteString(scopeToken(scope[k]))
	}
	b.WriteString("}")
	return b.String()
}

// scopeToken renders one scope key or value. The label is printed OUTSIDE the
// «...» data delimiters, and a scope is text Ghost did not author (a save
// argument, an imported artifact), so a value is written bare only when every
// character is one a scope name plausibly uses. Anything else is written as an
// ASCII-only Go quoted string: a newline cannot start a line of its own, a `}`
// cannot close the label early, and a «, » or other non-ASCII rune cannot open
// a data block of its own.
func scopeToken(s string) string {
	if s == "" {
		return `""`
	}
	for _, r := range s {
		if !isScopeNameRune(r) {
			return strconv.QuoteToASCII(s)
		}
	}
	return s
}

func isScopeNameRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	}
	return strings.ContainsRune("._-:/@+", r)
}

// quoteData wraps untrusted stored text in «...» data delimiters, first
// rewriting any literal « or » inside it so embedded delimiters cannot
// terminate the data block early and smuggle text back out as instructions.
func quoteData(s string) string {
	return "«" + strings.NewReplacer("«", "<<", "»", ">>").Replace(s) + "»"
}

// itemOf materialises one item from a candidate. The fields are copied, not
// re-read, so an item cannot disagree with the row that produced it.
//
// ResolvedAt is non-nil exactly when the column is set, and its value is the
// parsed stamp or the zero time when the text is unreadable. That asymmetry is
// deliberate: the marker and every leak predicate ask "was this row resolved",
// which is a fact about the column, and SQLite wrote that column — so a value
// this build cannot parse still means a resolved row. Reading it as live would
// be the opposite claim, on the strength of a formatting problem.
func itemOf(c memory.Candidate) Item {
	it := Item{
		ID:         c.ID,
		Category:   c.Category,
		Content:    c.Content,
		Tags:       c.Tags,
		Importance: float64(c.Importance),
		Pinned:     c.Pinned,
		CreatedAt:  parseStamp(c.CreatedAt),
		Bytes:      len(c.Content),
		Bucket:     c.ProjectID,
		ProjectID:  c.ProjectID,
		Scope:      c.Scope,
		Confidence: c.Confidence,
		Agent:      c.Agent,
		SourceRef:  c.SourceRef,
		Score:      c.Score,
		Source:     c.Source,
	}
	if c.ResolvedAt != nil {
		resolved := parseStamp(*c.ResolvedAt)
		it.ResolvedAt = &resolved
	}
	return it
}
