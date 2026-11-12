package assemble

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/wcatz/ghost/internal/memory"
)

// Item is the shared output of assembly: what both surfaces render, what an
// explanation describes, and what a context metric scores. One type for all
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
	ValidityState         string
	Confidence            *float64
	Agent                 string
	SourceRef             string
	Score                 float64
	Tokens                int
	Source                string
	ConflictsWith         []string
	ContradictedBy        []string
	SupersededBy          []string
	MaybeReplacedBy       []string
}

// MaybeReplacedMarker is the bracketed field a line carries when a newer row on
// the same page looks like its replacement. It is worded as what it is, a guess
const MaybeReplacedMarker = "[maybe replaced by a newer note]"

// SupersededMarker is the bracketed field a line carries when a row on the same
// page replaced it through a live `supersedes` edge (Item.SupersededBy). Unlike
const SupersededMarker = "[superseded by a newer note]"

// Line renders the shared item prefix: the one line shape both surfaces emit for
// a memory, so a reader comparing search output with an injected block sees the
func (i Item) Line() string {
	pin := ""
	if i.Pinned {
		pin = " [pinned]"
	}
	tags := ""
	if len(i.Tags) > 0 {
		tags = TagsLabel(i.Tags)
	}
	resolved := ""
	if i.ResolvedAt != nil {
		resolved = " [resolved]"
	}
	if len(i.SupersededBy) > 0 {
		resolved += " " + SupersededMarker
	}
	if len(i.MaybeReplacedBy) > 0 {
		resolved += " " + MaybeReplacedMarker
	}
	origin := ""
	if _, label := memory.OriginClass(memory.CanonicalOriginSourceForProject(i.ProjectID, i.Source, i.Content)); label != "" {
		origin = SourceLabel(label)
	}
	conflicts := ConflictsLabel(i.ConflictsWith)
	return "- [" + i.Category + "] `" + Token(i.ID) + "` (" +
		strconv.FormatFloat(i.Importance, 'f', 1, 64) + pin + tags + resolved + ScopeLabel(i.Scope) +
		validityLabel(i.ValidityState, i.ValidFrom, i.ValidUntil, i.VerifiedAt) +
		ConfidenceLabel(i.Confidence) + AgentLabel(i.Agent) + SourceRefLabel(i.SourceRef) + origin + conflicts + ContradictedByLabel(i.ContradictedBy) +
		SupersededByLabel(i.SupersededBy) + MaybeReplacedByLabel(i.MaybeReplacedBy) +
		") " + Data(i.Content)
}

const maxRenderedConflictPartners = 8

// ConflictsLabel renders the rows a memory contradicts, as they are rendered on
// their own lines (Token), or "" when it contradicts none in this answer. At
func ConflictsLabel(ids []string) string {
	return boundedIDLabel(" conflicts_with=", ids)
}

// ContradictedByLabel renders the newer rows that contradict a pinned row, with
// the same bound as ConflictsLabel, or "" when none does.
func ContradictedByLabel(ids []string) string {
	return boundedIDLabel(" contradicted_by=", ids)
}

func boundedIDLabel(name string, ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	shown, more := ids, 0
	if len(shown) > maxRenderedConflictPartners {
		more = len(shown) - maxRenderedConflictPartners
		shown = shown[:maxRenderedConflictPartners]
	}
	toks := make([]string, len(shown))
	for i, id := range shown {
		toks[i] = "`" + Token(id) + "`"
	}
	label := name + strings.Join(toks, ",")
	if more > 0 {
		label += " (+" + strconv.Itoa(more) + " more)"
	}
	return label
}

// MaybeReplacedByLabel renders the rows that look like a memory's replacement,
// with the same bound as ConflictsLabel, or "" when none is in this answer.
func MaybeReplacedByLabel(ids []string) string {
	return boundedIDLabel(" maybe_replaced_by=", ids)
}

// SupersededByLabel renders the rows that replaced a memory, as they are
// rendered on their own lines (Token), or "" when none is in this answer.
func SupersededByLabel(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	toks := make([]string, len(ids))
	for i, id := range ids {
		toks[i] = "`" + Token(id) + "`"
	}
	return " superseded_by=" + strings.Join(toks, ",")
}

// SourceLabel renders a row's origin label as ` source=<label>`. The label is
// stored text — an unrecognised value is printed rather than refused — so it goes
func SourceLabel(label string) string {
	if label == "" {
		return ""
	}
	return " source=" + Token(label)
}

// AgentLabel renders the writing harness, or "" when the row records none.
func AgentLabel(agent string) string {
	if agent == "" {
		return ""
	}
	if len(agent) > MaxRenderedAgentLen {
		agent = clampBytes(agent, MaxRenderedAgentLen) + "…[agent truncated]"
	}
	return " agent=" + Data(agent)
}

// ProjectLabel renders the bucket a row belongs to, as ` project=<id>`, for a
// listing that fuses rows from several projects. The id goes through Token like
func ProjectLabel(projectID string) string {
	if len(projectID) > MaxRenderedProjectIDLen {
		return " project=" + Token(clampBytes(projectID, MaxRenderedProjectIDLen)) + "…[project id truncated]"
	}
	return " project=" + Token(projectID)
}

// MaxRenderedProjectIDLen is what a listing prints of a project id. A DISPLAY
// bound, not a claim about the column.
const MaxRenderedProjectIDLen = 256

// MaxRenderedAgentLen is what a listing prints of an agent. It is a DISPLAY
// bound, not a claim about the column: memory.MaxAgentLen refuses a longer one on
const MaxRenderedAgentLen = 128

// MaxRenderedSourceRefLen bounds what a listing prints of a reference. It is a
// DISPLAY bound, not a claim about what the column holds: every writer this build
const MaxRenderedSourceRefLen = 512

// SourceRefLabel renders the reference a claim was read from, or "" when the row
// records none. Free text on its way into a tool answer, so it is delimited: a
func SourceRefLabel(ref string) string {
	if ref == "" {
		return ""
	}
	if len(ref) > MaxRenderedSourceRefLen {
		ref = clampBytes(ref, MaxRenderedSourceRefLen) + "…[reference truncated]"
	}
	return " source_ref=" + Data(ref)
}

// ConfidenceLabel renders a recorded belief, or "" when the row records none.
func ConfidenceLabel(confidence *float64) string {
	if confidence == nil {
		return ""
	}
	return " confidence " + strconv.FormatFloat(*confidence, 'g', -1, 64)
}

func stampText(t *time.Time, isEnd bool) string {
	hour, minute, second := t.Hour(), t.Minute(), t.Second()
	isBoundary := hour == 0 && minute == 0 && second == 0
	if isEnd {
		isBoundary = hour == 23 && minute == 59 && second == 59
	}
	if isBoundary {
		return t.Format(memory.DateStampLayout)
	}
	return t.Format(memory.StoredStampLayout)
}

func tokenEstimate(bytes int) int {
	if bytes <= 0 {
		return 0
	}
	return (bytes + tokenBytesPerToken - 1) / tokenBytesPerToken
}

const tokenBytesPerToken = 4

// ScopeLabel renders a memory's scope for a listing, or "" when unscoped.
// Keys are sorted: map iteration order is random in Go, so an unsorted rendering
func ScopeLabel(scope map[string]string) string {
	if len(scope) == 0 {
		return ""
	}
	keys := make([]string, 0, len(scope))
	for k := range scope {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	extra := 0
	if len(keys) > memory.MaxScopeKeys {
		extra = len(keys) - memory.MaxScopeKeys
		keys = keys[:memory.MaxScopeKeys]
	}

	var b strings.Builder
	b.WriteString(" scope{")
	for i, k := range keys {
		if i > 0 {
			b.WriteString(" ")
		}
		b.WriteString(boundedScopePart(k, memory.MaxScopeKeyLen))
		b.WriteString("=")
		b.WriteString(boundedScopePart(scope[k], memory.MaxScopeValueLen))
	}
	if extra > 0 {
		fmt.Fprintf(&b, " …[%d more scope keys]", extra)
	}
	b.WriteString("}")
	return b.String()
}

const scopeTruncationMarker = "…[scope truncated]"

func boundedScopePart(s string, max int) string {
	if len(s) <= max {
		return Token(s)
	}
	return Token(clampBytes(s, max)) + scopeTruncationMarker
}

// Token renders one stored value that a line prints OUTSIDE the «...» data
// delimiters: a scope key or value, or a memory id. Both are text Ghost did not
func Token(s string) string {
	return memory.SafeToken(s)
}

// Label renders one stored value that must occupy a single line of output and is
// read as a LABEL rather than as a key — a project's name, a project's path.
func Label(s string) string {
	if !labelNeedsEscaping(s) {
		return s
	}
	q := strconv.QuoteToASCII(s)
	q = strings.ReplaceAll(q, "`", "\\`")
	return q[1 : len(q)-1]
}

func labelNeedsEscaping(s string) bool {
	for _, r := range s {
		switch {
		case isLineEndingOrControl(r):
			return true
		case memory.IsFormatRune(r):
			return true
		case r == '`', r == '"', r == '«', r == '»':
			return true
		}
	}
	return false
}

func isLineEndingOrControl(r rune) bool {
	return unicode.IsControl(r) || isLineBreak(r)
}

// PreviewLine returns the first line of stored text, capped at max runes with an
// ellipsis, for the places that name a record by a glimpse of its content rather
func PreviewLine(s string, max int) string {
	if i := strings.IndexFunc(s, isLineBreak); i >= 0 {
		s = s[:i]
	}

	if max < 0 {
		return ""
	}

	var b strings.Builder
	if max > 0 {
		b.Grow(max * 6) // worst case: each rune becomes a 6-rune escape like \u202e
	}
	renderedRunes := 0
	cut := false
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && n == 1 {
			if renderedRunes+1 > max {
				cut = true
				break
			}
			b.WriteString("\uFFFD")
			renderedRunes++
			i++
			continue
		}

		var rendered string
		if isEscapedControl(r) {
			q := strconv.QuoteRuneToASCII(r)
			rendered = q[1 : len(q)-1] // strip surrounding quotes
		} else {
			rendered = string(r)
		}

		rr := len([]rune(rendered))
		if renderedRunes+rr > max {
			cut = true
			break
		}

		b.WriteString(rendered)
		renderedRunes += rr
		i += n
	}

	if cut {
		b.WriteString("…")
	}
	return b.String()
}

func isTokenRune(r rune) bool {
	return memory.IsSafeTokenRune(r)
}

// Data wraps untrusted stored text in «...» data delimiters, first rewriting
// any literal « or » inside it so embedded delimiters cannot terminate the
func Data(s string) string {
	return "«" + escapeControls(foldLineBreaks(neutralizeDelimiters(s))) + "»"
}

func escapeControls(s string) string {
	if !strings.ContainsFunc(s, isEscapedControl) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		if r != utf8.RuneError && isEscapedControl(r) {
			q := strconv.QuoteRuneToASCII(r)
			b.WriteString(q[1 : len(q)-1])
		} else {
			b.WriteString(s[i : i+n])
		}
		i += n
	}
	return b.String()
}

func isEscapedControl(r rune) bool {
	return (r != '\t' && unicode.IsControl(r)) || memory.IsFormatRune(r)
}

// LineBreakEscape is what a stored line break is printed as: every memory
// occupies exactly ONE physical line on every surface (#911), the way Token
const LineBreakEscape = "⏎"

func foldLineBreaks(s string) string {
	if !strings.ContainsFunc(s, isLineBreak) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] == '\r' && i+1 < len(s) && s[i+1] == '\n' {
			b.WriteString(LineBreakEscape)
			i += 2
			continue
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		if r != utf8.RuneError && isLineBreak(r) {
			b.WriteString(LineBreakEscape)
		} else {
			b.WriteString(s[i : i+n])
		}
		i += n
	}
	return b.String()
}

// FoldLineBreaks is foldLineBreaks for a caller outside the package that must
// keep a stored value on one physical line by the same set Data folds.
func FoldLineBreaks(s string) string { return foldLineBreaks(s) }

func isLineBreak(r rune) bool {
	switch r {
	case '\n', '\v', '\f', '\r', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
		return true
	}
	return false
}

// TagsLabel renders a row's tag list as the ` tags:[…]` label a memory line
// carries, or "" when the row records no tag.
func TagsLabel(tags []string) string {
	if len(tags) == 0 {
		return ""
	}
	b, err := json.Marshal(boundTags(tags))
	if err != nil {
		return ""
	}
	return " tags:" + neutralizeTagLabel(string(b))
}

// MaxRenderedTagLen bounds what a listing prints of ONE tag. It is a DISPLAY
// bound, not a claim about the column, for the reason AgentLabel and
const MaxRenderedTagLen = 64

const tagTruncationMarker = "…[tag truncated]"

func boundTags(tags []string) []string {
	var bounded []string
	for i, t := range tags {
		if len(t) <= MaxRenderedTagLen {
			continue
		}
		if bounded == nil {
			bounded = make([]string, len(tags))
			copy(bounded, tags)
		}
		bounded[i] = clampBytes(t, MaxRenderedTagLen) + tagTruncationMarker
	}
	if bounded == nil {
		return tags
	}
	return bounded
}

// TagBacktickEscape is what a backtick in a tag prints as.
const TagBacktickEscape = "\\u0060"

func neutralizeTagLabel(s string) string {
	s = strings.ReplaceAll(neutralizeDelimiters(s), "`", TagBacktickEscape)
	if !strings.ContainsFunc(s, func(r rune) bool {
		return isLineEndingOrControl(r) || memory.IsFormatRune(r)
	}) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for _, r := range s {
		if isLineEndingOrControl(r) || memory.IsFormatRune(r) {
			writeJSONEscape(&b, r)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func writeJSONEscape(b *strings.Builder, r rune) {
	if r > 0xFFFF {
		hi, lo := utf16.EncodeRune(r)
		fmt.Fprintf(b, "\\u%04x\\u%04x", hi, lo)
		return
	}
	fmt.Fprintf(b, "\\u%04x", r)
}

func neutralizeDelimiters(s string) string {
	return delimiterReplacer.Replace(s)
}

var delimiterReplacer = strings.NewReplacer("«", "<<", "»", ">>")

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
		Tokens:     tokenEstimate(len(c.Content)),
	}
	if c.ResolvedAt != nil {
		resolved := parseStamp(*c.ResolvedAt)
		it.ResolvedAt = &resolved
	}
	return it
}
