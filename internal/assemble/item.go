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
	// Tokens is a token ESTIMATE for this item's content: Bytes/4, rounded up,
	// so a short memory is never reported as free. It is derived from Bytes at
	// materialisation and again after any presentation clamp, so the two cannot
	// disagree. Bytes remain the budget unit — there is no tokenizer in this
	// package — and a caller that budgets in tokens reads this field.
	Tokens int
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
		tags = TagsLabel(i.Tags)
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
	return "- [" + i.Category + "] `" + Token(i.ID) + "` (" +
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
	// The display bound, for the same reason SourceRefLabel has one: the renderer
	// cannot assume its input came from a writer that enforces the cap, and this
	// label is printed beside the reference on every listing. Cut on a rune —
	// the input is exactly the untrusted text a writer would have refused.
	if len(agent) > MaxRenderedAgentLen {
		agent = clampBytes(agent, MaxRenderedAgentLen) + "…[agent truncated]"
	}
	return " agent=" + quoteData(agent)
}

// MaxRenderedAgentLen is what a listing prints of an agent. It is a DISPLAY
// bound, not a claim about the column: memory.MaxAgentLen refuses a longer one on
// the writers that reach it, and this covers the three that deliberately do not —
// RestoreSnapshot, CreateFromCorpus, and ReplaceNonManual, which inherits the
// replaced row's agent onto the row a rewrite becomes.
const MaxRenderedAgentLen = 128

// MaxRenderedSourceRefLen bounds what a listing prints of a reference. It is a
// DISPLAY bound, not a claim about what the column holds: every writer this build
// controls refuses a longer one at write time (memory.MaxSourceRefLen), and this
// covers what they cannot — a store written before that cap, a snapshot table
// edited by hand, a row restored from one. It is here rather than only at the
// writers because the renderer is the one place that cannot assume its input came
// from a writer that enforces the cap — `RestoreSnapshot`, which writes the column
// in SQL from the snapshot table, `CreateFromCorpus`, which reaches insertMemory
// directly, and `ReplaceNonManual`, which inherits the replaced row's reference
// onto the row a rewrite becomes, are the three writers that deliberately do not.
//
// The value is truncated, not refused, because at this point the row already holds
// whatever it holds and refusing to show it would be worse than showing part of it.
const MaxRenderedSourceRefLen = 512

// SourceRefLabel renders the reference a claim was read from, or "" when the row
// records none. Free text on its way into a tool answer, so it is delimited: a
// path or URL carrying « or » would otherwise close the data block early and let
// its own tail read as instruction.
func SourceRefLabel(ref string) string {
	if ref == "" {
		return ""
	}
	if len(ref) > MaxRenderedSourceRefLen {
		// clampBytes, not a raw slice: this bound is the one handling values no
		// writer vouched for, so its input is exactly the untrusted non-ASCII text
		// a writer would have refused, and a cut through a multi-byte rune would
		// put an invalid byte inside the data block.
		ref = clampBytes(ref, MaxRenderedSourceRefLen) + "…[reference truncated]"
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

// stampText renders one validity boundary as a whole day when the instant is the
// boundary that form stands for, and in full otherwise — into
// memory.DateStampLayout, the same form a reader accepts.
//
// Which instant the date stands for depends on which boundary this is, and isEnd
// is what says so:
//
//   - As a window's START or a verification, a date means midnight — which is
//     where every writer puts a bare one — so midnight prints as the date. It is
//     the same instant, and 23:59:59 is a time the caller chose, so it prints in
//     full: collapsing it would read as though the claim began at midnight, and
//     re-saving the boundary the line showed would move the row a day earlier.
//   - As a window's END, a date means the last second of that day, because that is
//     where a writer puts a bare one (see internal/mcpserver's parseStampArg
//     endOfDay parameter) and because "valid until 2026-10-01" means the 1st. So
//     23:59:59 prints as the date, and midnight — which the same date form does
//     NOT stand for on this boundary — prints in full.
//
// The last pair is the asymmetry that matters, and it is why both directions are
// conditional. A midnight `valid_until` is reachable without any of Ghost's tools:
// memory.StampLayouts accepts a bare date, and a portable artifact, a restored
// snapshot, a hand edit or a SQLite date() leaves one. Printing it as "until
// 2026-10-01" would be a claim about the whole day while stage 2 retires the row at
// midnight of it, and an agent re-saving what it was shown would extend the claim
// by a day. Printing the instant keeps the line and the filter saying the same
// thing, which is the only reason to render one from the other.
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

// tokenEstimate converts content bytes into a token ESTIMATE at the conventional
// four-bytes-per-token ratio, rounded UP: a caller budgeting in tokens pays for
// the fraction too, and rounding down would report a short memory as free. The
// value is a ratio applied to bytes, not a tokenizer's output, so every field
// carrying it is named or documented as an estimate.
func tokenEstimate(bytes int) int {
	if bytes <= 0 {
		return 0
	}
	return (bytes + tokenBytesPerToken - 1) / tokenBytesPerToken
}

// tokenBytesPerToken is the ratio behind Item.Tokens and Result.Tokens. It is a
// named constant because the estimate is reported to callers: a reader that had
// to guess the ratio would treat the number as a measurement.
const tokenBytesPerToken = 4

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
		b.WriteString(Token(k))
		b.WriteString("=")
		b.WriteString(Token(scope[k]))
	}
	b.WriteString("}")
	return b.String()
}

// Token renders one stored value that a line prints OUTSIDE the «...» data
// delimiters: a scope key or value, or a memory id. Both are text Ghost did not
// author — a save argument, an imported artifact's verbatim value — and both are
// printed on a line an agent reads as Ghost's own, so neither may be able to
// start a line, close the construct it sits in, or open a data block of its own.
//
// A value is written bare only when every character is one a stored name
// plausibly uses. Anything else is written as an ASCII-only Go quoted string: a
// newline cannot start a line of its own, a `}` or a backtick cannot close the
// label or the id span early, and a «, » or other non-ASCII rune cannot open a
// data block of its own.
//
// The bare case is the one every real row takes — the ids Ghost mints are 32 hex
// characters and a scope name is a word — so this is invisible on every honest
// listing and costs nothing. It is exported because a second renderer printing
// the same fields (mcpserver's formatMemories) must reach the SAME function: two
// implementations of one rule are two rules, and the one that is not tested here
// is the one that ships the bug.
func Token(s string) string {
	if s == "" {
		return `""`
	}
	for _, r := range s {
		if !isTokenRune(r) {
			return strconv.QuoteToASCII(s)
		}
	}
	return s
}

// Label renders one stored value that must occupy a single line of output and is
// read as a LABEL rather than as a key — a project's name, a project's path.
//
// It exists because Token is the wrong renderer for those two, and using it would
// have been a visible regression rather than a safe default: Token writes a space
// as a quoted string, so every project named "my project" or living at
// "/Users/w/My Projects/ghost" would print as `"my project"` on every listing and
// in the session-start block's own heading. A space is not what makes a line, and
// a name is not what gets used as a `--only` selector.
//
// So Label keeps ordinary text exactly as written — spaces, slashes, dots, every
// word a name is made of — and neutralises only what could end the line or open a
// construct around it. Three things are printed around a project name and path, so
// three characters are delimiters here that are not delimiters for an id: a
// control character, which ends the line; «», which opens a data block; and the
// backtick and the double quote, which close the backtick span a path is printed
// in and the `"…"` the session-start block tells the reader to pass to every tool.
//
// The escaping is `strconv.QuoteToASCII` — the same call Token makes, so the two
// renderers cannot disagree about what a newline looks like — with the surrounding
// quotes dropped and the backtick fixed up, because a backtick is printable and is
// not a delimiter in Go, so quoting alone would hand back the one character this
// exists to neutralise.
func Label(s string) string {
	if !labelNeedsEscaping(s) {
		return s
	}
	q := strconv.QuoteToASCII(s)
	q = strings.ReplaceAll(q, "`", "\\`")
	// The quotes are the only thing dropped: a value that needed escaping always
	// comes back with both, so the slice cannot run off the ends.
	return q[1 : len(q)-1]
}

// labelNeedsEscaping reports whether s holds anything Label would have to escape.
func labelNeedsEscaping(s string) bool {
	for _, r := range s {
		switch {
		case r < 0x20 || r == 0x7f:
			return true
		case r == '`', r == '"', r == '«', r == '»':
			return true
		}
	}
	return false
}

// PreviewLine returns the first line of stored text, capped at max runes with an
// ellipsis, for the places that name a record by a glimpse of its content rather
// than by printing all of it.
//
// It exists as ONE function because there were three copies of it, and the copies
// had already drifted: two cut at '\n' and one did too, and then the answer to
// "which byte ends a line" changed and only two of the three were updated. A
// helper whose correctness is "a line is one line" is exactly the kind that must
// not be copyable, so the logic lives here and the three call sites call it.
//
// Cut at the first of EITHER byte, which is the whole reason this is a function
// rather than a one-liner at each site. A preview is rendered raw — that is what
// makes it a preview — so any line-breaking character a memory's content holds
// would otherwise be rendered too. IndexByte('\n') alone left a lone carriage
// return, and a lone CR is enough: a terminal reads it as "return to column 0 and
// overwrite", so content of `legitimate claim\roverwrite this` previewed as
// `overwrite this` and the honest prefix was gone. Several renderers also split on
// CR as readily as on LF, so this was never terminal-specific. Cutting at
// whichever comes first drops the CR of a CRLF pair too, since s[:i] ends
// immediately before it (#791).
func PreviewLine(s string, max int) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	r := []rune(s)
	if len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

// isTokenRune is the set Token writes bare. It is the scope-name set the label
// has always used, widened by nothing: the id column's own values are 32 hex
// characters, and the other ids a real store holds (a bench corpus id, a restored
// snapshot's) are words with separators. Nothing else needs to be bare to be
// legible, and every character outside this set is exactly the class that can
// break a line or a data block.
func isTokenRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	}
	return strings.ContainsRune("._-:/@+", r)
}

// TagsLabel renders a row's tag list as the ` tags:[…]` label a memory line
// carries, or "" when the row records no tag.
//
// It is EXPORTED, and it is the one implementation, because the label was written
// twice — here and in `mcpserver.formatMemories` — and two implementations of one
// rule are two rules, with the untested one shipping the bug. `formatMemories`
// renders the same field for the surfaces that have not moved to the assembler
// yet, and it now calls this.
//
// WHY the label is a JSON array and not a delimited one, which is the decision
// that makes the escaping below necessary: the line already delimits the one
// field that is free prose (the content, through quoteData), and tags are short
// keyword labels a reader scans. A second «...» pair per row would be a second
// thing to explain on every listing, so the label is the JSON array, and the array
// is what makes it safe — with one gap that had to be closed rather than assumed.
//
// THAT GAP is #811. `json.Marshal` escapes a newline, a quote and a backslash, and
// HTML-escapes `<`, `>` and `&`, so a tag cannot forge a line or break out of its
// own JSON string. It does NOT escape « or », and the label is printed OUTSIDE the
// «...» data delimiters, on the same line as the content. So a tag holding a «
// opened a data block of its own mid-metadata: the reader met « twice on the row
// before reaching the content, and the span between them read as data rather than
// as the tag list. It cannot CLOSE a block, so nothing was smuggled OUT as
// instruction — which is why this is a lower-severity sibling of the id defect
// (#791) rather than a copy of it — but the delimiter contract in docs/mcp.md is
// one contract, and a field that can open a block breaks it.
//
// So a « becomes `<<` and a » becomes `>>`, through `neutralizeDelimiters` — the
// ONE function that does it, and the one `quoteData` already used, because a
// second copy of a substitution a reader parses visually is a second thing to keep
// in step. `<<` is the same spelling the content uses, so a reader who has met one
// knows the other. A backtick is neutralised too, and differently; see
// `tagBacktickEscape`, which is where the two rules diverge and why.
//
// THE ORDER IS THE INTERESTING PART, and it took one reversal to get right.
// Substituting BEFORE the marshal was the first attempt and it is wrong: the
// substitution introduces ASCII `<` and `>`, which `json.Marshal` HTML-escapes in
// turn, so a tag of `a«b` came out as `["a\u003c\u003cb"]` — twenty-two characters
// of escape for a two-character substitution, and a reader scanning a tag list sees
// noise. Substituting AFTER the marshal cannot do that, and cannot break the JSON
// either: « and » are not JSON metacharacters, so rewriting one cannot unbalance
// the string it sits in. And the ordinary cases come out byte-identical, which is
// the point of doing it this way rather than routing the list through `Token` or
// `Label` — those would quote every tag holding a space, which is most of them,
// and `tags:["golden","pinned"]` is what every existing store, golden and test
// already asserts on.
func TagsLabel(tags []string) string {
	if len(tags) == 0 {
		return ""
	}
	b, err := json.Marshal(tags)
	if err != nil {
		// Unreachable: tags is a []string, which json.Marshal always encodes. A
		// dropped label is the answer if it ever were not, because a raw
		// concatenation is the one rendering that cannot be escaped at all.
		return ""
	}
	return " tags:" + neutralizeTagLabel(string(b))
}

// tagBacktickEscape is what a backtick in a tag prints as.
//
// A JSON ESCAPE rather than a substitution like the guillemet's `<<`, and the
// reason is that `<<` already means something to a reader of a Ghost line: it is
// what `quoteData` writes for a literal `«`, so a reader who sees `<<` in a tag
// list knows it came from a delimiter. There is no such convention for a backtick
// in a tag, so the honest form is the one the surrounding JSON already uses for a
// character it must not print literally — and it stays VALID JSON, so a reader
// decoding the array gets the backtick back rather than losing it.
//
// It matters because `json.Marshal` does not escape a backtick, and a lone one in
// a listing row opens a markdown code span that swallows the rest of the line. The
// MCP write path refuses a backtick in a tag (mcpserver.validateTags), but a store
// can already hold one — a pre-guard save, a restored snapshot, a hand edit — and
// the renderer is the layer that covers those. Same reason the guillemets are
// handled here and not refused at import: refusing a stored value on the way OUT of
// a backup costs the user the whole memory, which is a far worse outcome than
// printing one backtick as an escape.
const tagBacktickEscape = "\\u0060"

// neutralizeTagLabel is the label's own neutralisation, and it is separate from
// `neutralizeDelimiters` rather than an extension of it.
//
// `neutralizeDelimiters` is shared with `quoteData`, where a backtick is NOT a
// threat — the content sits inside «...», so a backtick in it cannot close anything
// Ghost printed. Widening that function would change how every content block in
// every answer renders, on no evidence that a content backtick is unsafe. So the
// label has its own, and this is the one place the two rules differ.
func neutralizeTagLabel(s string) string {
	return strings.ReplaceAll(neutralizeDelimiters(s), "`", tagBacktickEscape)
}

// neutralizeDelimiters rewrites the « and » that open and close a data block into
// the fixed `<<` and `>>` a reader cannot mistake for one.
//
// It is the ONE function that does it, and the fact that the tag list needed it
// (#811) is the argument: `quoteData` has done this for the content since the
// delimiters existed, and a field that renders its own copy of the substitution is
// a field whose copy will drift.
func neutralizeDelimiters(s string) string {
	return delimiterReplacer.Replace(s)
}

// delimiterReplacer is the substitution itself, hoisted out of quoteData for the
// same reason the three copies of the preview cut were hoisted into PreviewLine: a
// strings.Replacer is safe for concurrent use, and a rule that must never drift is
// the kind that must not be copyable. It is a package var rather than a per-call
// NewReplacer so it is built once instead of once per tag on every line of every
// listing.
var delimiterReplacer = strings.NewReplacer("«", "<<", "»", ">>")

// quoteData wraps untrusted stored text in «...» data delimiters, first
// rewriting any literal « or » inside it so embedded delimiters cannot
// terminate the data block early and smuggle text back out as instructions.
func quoteData(s string) string {
	return "«" + neutralizeDelimiters(s) + "»"
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
		Tokens:     tokenEstimate(len(c.Content)),
	}
	if c.ResolvedAt != nil {
		resolved := parseStamp(*c.ResolvedAt)
		it.ResolvedAt = &resolved
	}
	return it
}
