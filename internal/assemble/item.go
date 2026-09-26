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
	Score         float64
	// Source is the row's stored origin. It travels with the item because the
	// shared line labels it, and mcpInstructions tells the agent to trust that
	// label: a renderer that could not see the origin would have to drop the
	// label, and dropping it is a behaviour change on a live surface. The label
	// itself comes from memory.OriginClass, never from this field's literal
	// value.
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
	origin := ""
	if _, label := memory.OriginClass(memory.CanonicalOriginSource(i.Source, i.Content)); label != "" {
		origin = " source=" + label
	}
	return "- [" + i.Category + "] `" + i.ID + "` (" +
		strconv.FormatFloat(i.Importance, 'f', 1, 64) + pin + tags + resolved + scopeLabel(i.Scope) + origin +
		") " + quoteData(i.Content)
}

// scopeLabel renders a memory's scope for a listing, or "" when unscoped.
// Keys are sorted: map iteration order is random in Go, so an unsorted rendering
// would show the same scope in a different order on each read and look like the
// scope itself was changing.
func scopeLabel(scope map[string]string) string {
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
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(scope[k])
	}
	b.WriteString("}")
	return b.String()
}

// quoteData wraps untrusted stored text in «...» data delimiters, first
// rewriting any literal « or » inside it so embedded delimiters cannot
// terminate the data block early and smuggle text back out as instructions.
func quoteData(s string) string {
	return "«" + strings.NewReplacer("«", "<<", "»", ">>").Replace(s) + "»"
}

// itemOf materialises one item from a candidate. The fields are copied, not
// re-read, so an item cannot disagree with the row that produced it.
func itemOf(c memory.Candidate) Item {
	return Item{
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
		Score:      c.Score,
		Source:     c.Source,
		ResolvedAt: parseStampPtr(c.ResolvedAt),
	}
}
