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
	// ValidityState is valid, future, expired, unverified or unset — and empty,
	// which is what stage 2 records for a row on a historical (as_of) request:
	// the window is the live row's and no verdict can be drawn from it, so the
	// bounds render unjudged (see ValidityLabel) rather than labelled with a
	// clock's answer about bounds the row did not hold then.
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
	// ConflictsWith lists the ids of the rows this one directly contradicts that
	// stage 5 withheld and did not render. It is set in one place, after the last
	// stage that can remove a row (markConflicts), so it names only rows that are
	// out of the answer by construction — the reader cannot see the partner, and
	// that is the point of the marker. The list is held whole; ConflictsLabel
	// bounds what the line renders of it.
	ConflictsWith []string
	// ContradictedBy lists the ids of the rows that directly contradict this
	// PINNED row, are newer than it, and are not the ones a pin lets win: a
	// withheld row (the pin outranks it, so it never reaches the reader) or
	// another pinned row that stayed in the answer. A pin guarantees delivery and
	// never says the claim is still true, so this marks the row as disputed by
	// newer evidence without withholding it. It is set in one place
	// (markConflicts) and names only rows a live, scope-compatible `contradicts`
	// edge joins to this one. The list is held whole; ContradictedByLabel bounds
	// what the line renders of it.
	ContradictedBy []string
	// SupersededBy lists the ids of the rows in the SAME rendered answer that
	// replaced this one through a live `supersedes` edge. It is set on a passive
	// read and a query read alike (markSuperseded), pinned row or not: a
	// live edge switches the lexical guess (MaybeReplacedBy) off for its pair, and
	// a convention or a preference is never resolved, so without it the edge would
	// leave the row with less on its line than the guess it replaced. On a
	// passive read a pinned row is also ordered behind its superseder; every
	// other superseded row is sunk by the retriever's demotion, and a keep-forever
	// one is marked but never moved. A superseder that is not in the answer is not named. A
	// non-empty list renders SupersededMarker and `superseded_by=`.
	SupersededBy []string
	// MaybeReplacedBy lists the ids of the rows in the SAME rendered answer that
	// look like this row's replacement: newer, the same project and scope, a
	// similar text that states a different value, and no explicit `supersedes`,
	// `contradicts` or resolve relating the two (memory.Candidate.MaybeReplacedBy,
	// the retriever's lexical guess). It is set in one place (markMaybeReplaced),
	// after the last stage that can remove a row, and names only rows still on the
	// page. A non-empty list renders MaybeReplacedMarker and the ids; the row is
	// marked, never withheld, because the guess is lexical and can be wrong.
	MaybeReplacedBy []string
}

// MaybeReplacedMarker is the bracketed field a line carries when a newer row on
// the same page looks like its replacement. It is worded as what it is, a guess
// read off the text: Ghost has not related the two rows, and the reader decides
// which one is current. It sits at a FIELD of the prefix, directly after
// `[resolved]` (or the tags, the pin or the importance when those are absent),
// outside the «...» data. Its words alone are not proof of it: a tag is stored
// text printed in the same prefix, so a tag can hold the same words. A reader
// sees such a tag JSON-quoted inside `tags:[…]`, and a check that wants the
// marker must find it at this field (eval/storyline's carriesReplacedMarker
// does), not anywhere before the first «.
const MaybeReplacedMarker = "[maybe replaced by a newer note]"

// SupersededMarker is the bracketed field a line carries when a row on the same
// page replaced it through a live `supersedes` edge (Item.SupersededBy). Unlike
// MaybeReplacedMarker it is not a guess: Ghost related the two rows. It sits at
// the same field, so the explicit relation never shows less than the lexical
// guess it switches off, and the same caution applies: a tag can hold its words,
// so a check must find it at its field.
const SupersededMarker = "[superseded by a newer note]"

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
	// The explicit relation's marker comes first: it is a fact Ghost recorded,
	// the lexical guess beside it is about another row.
	if len(i.SupersededBy) > 0 {
		resolved += " " + SupersededMarker
	}
	if len(i.MaybeReplacedBy) > 0 {
		resolved += " " + MaybeReplacedMarker
	}
	// The legacy-seed correction is scoped to the global project, because that
	// is the only row it is about: a project row holding the shipped sentence is
	// the user's own material, and labelling it builtin would misattribute it and
	// remove the "no agent recorded" marker that says so. The item carries its
	// own project id precisely so this renderer does not have to guess.
	origin := ""
	if _, label := memory.OriginClass(memory.CanonicalOriginSourceForProject(i.ProjectID, i.Source, i.Content)); label != "" {
		origin = SourceLabel(label)
	}
	// A field on the same physical line, never a line of its own: a memory is one
	// line, and a marker on a second one would read as a second memory.
	conflicts := ConflictsLabel(i.ConflictsWith)
	return "- [" + i.Category + "] `" + Token(i.ID) + "` (" +
		strconv.FormatFloat(i.Importance, 'f', 1, 64) + pin + tags + resolved + ScopeLabel(i.Scope) +
		validityLabel(i.ValidityState, i.ValidFrom, i.ValidUntil, i.VerifiedAt) +
		ConfidenceLabel(i.Confidence) + AgentLabel(i.Agent) + SourceRefLabel(i.SourceRef) + origin + conflicts + ContradictedByLabel(i.ContradictedBy) +
		SupersededByLabel(i.SupersededBy) + MaybeReplacedByLabel(i.MaybeReplacedBy) +
		") " + Data(i.Content)
}

// maxRenderedConflictPartners bounds how many withheld ids one kept line names
// in `conflicts_with=`. A dense contradiction component can name dozens, and
// each id costs ~35 bytes on one physical line of a response with a 16000-byte
// ceiling, so an unbounded list lets one line spend the budget the response-fit
// pass needs for the admitted rows below it. Naming the first
// maxRenderedConflictPartners (in the rank order markConflicts builds them, so
// the cut is stable across reads) and counting the rest costs a bounded number
// of bytes and still tells the reader the block was thinned. It is the same
// shape as memory.ClampAttribution, which bounds the counterpart ids one row
// may name in the explain projection.
const maxRenderedConflictPartners = 8

// ConflictsLabel renders the rows a memory contradicts, as they are rendered on
// their own lines (Token), or "" when it contradicts none in this answer. At
// most maxRenderedConflictPartners are named; any remainder is reported as a
// trailing count, so a large component cannot spend the response budget on one
// line. Item.ConflictsWith keeps the full list — only the rendering is bounded.
func ConflictsLabel(ids []string) string {
	return boundedIDLabel(" conflicts_with=", ids)
}

// ContradictedByLabel renders the newer rows that contradict a pinned row, with
// the same bound as ConflictsLabel, or "" when none does.
func ContradictedByLabel(ids []string) string {
	return boundedIDLabel(" contradicted_by=", ids)
}

// boundedIDLabel renders ` <name>=` followed by at most
// maxRenderedConflictPartners ids as they appear on their own lines, and a
// trailing `(+N more)` for the rest.
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
// through Token like every other identifier on the line and cannot carry a line
// break or a delimiter (#911). The known vocabulary (mcp, reflection, builtin,
// import:…) is all bare-token characters and renders unchanged. Callers pass the
// label memory.OriginClass returned, and "" renders as nothing.
func SourceLabel(label string) string {
	if label == "" {
		return ""
	}
	return " source=" + Token(label)
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
	return " agent=" + Data(agent)
}

// ProjectLabel renders the bucket a row belongs to, as ` project=<id>`, for a
// listing that fuses rows from several projects. The id goes through Token like
// every identifier printed outside the data delimiters. It is bounded for the
// reason AgentLabel is: an imported project id may be as long as the path it
// was keyed on (memory.CheckImportedProjectID permits that so a long path
// round-trips), and a response budget that always keeps its first row whole
// would otherwise return an answer of whatever size that id has.
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
	return " source_ref=" + Data(ref)
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

	// The display bound, for the reason AgentLabel and TagsLabel carry one: the
	// writers refuse a scope past memory.MaxScopeKeys / MaxScopeKeyLen /
	// MaxScopeValueLen, but a row written before that bound, an imported
	// snapshot, and the writers that deliberately skip it (RestoreSnapshot,
	// CreateFromCorpus, ReplaceNonManual) can hold any size, and this label is
	// printed on every listing. The bounds are the writer's own constants rather
	// than copies, so the renderer can never be tighter than what a writer
	// accepted whole — a display bound quietly becoming a data change.
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

// scopeTruncationMarker is what an over-long scope key or value prints as its last
// characters, the same shape as the agent, source_ref and tag markers.
const scopeTruncationMarker = "…[scope truncated]"

// boundedScopePart renders one scope key or value through Token after cutting it
// to max bytes on a rune boundary. The cut comes BEFORE Token so the rendered size
// is bounded by the constant rather than by how much a quoted escape expands, and
// the marker is appended outside the token so it is never itself quoted.
func boundedScopePart(s string, max int) string {
	if len(s) <= max {
		return Token(s)
	}
	return Token(clampBytes(s, max)) + scopeTruncationMarker
}

// Token renders one stored value that a line prints OUTSIDE the «...» data
// delimiters: a scope key or value, or a memory id. Both are text Ghost did not
// author — a save argument, an imported artifact's verbatim value — and both are
// printed on a line an agent reads as Ghost's own, so neither may be able to
// start a line, close the construct it sits in, or open a data block of its own.
//
// The rule itself is memory.SafeToken and this is a delegation, not a copy. It
// MOVED there rather than being duplicated because the audit's usefulness line
// renders a session id into a prompt from internal/memory, and the dependency runs
// assemble -> memory: a reader in internal/memory cannot reach a function in the
// package that imports it. That is the same arrangement the stamp parsers already
// use (memory.ParseStamp, memory.StampLayouts, memory.ValidityState), and for the
// same reason — two implementations of one rule are two rules, and the copy that
// is not tested is the one that ships the bug. It is kept as a named function
// because a second renderer printing the same fields (mcpserver's formatMemories)
// reads better against this name than against memory.SafeToken.
func Token(s string) string {
	return memory.SafeToken(s)
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
// construct around it. Four things are printed around a project name and path, so
// four classes are delimiters here that are not delimiters for an id: any rune
// that ends a line for a reader, which is the package's own isLineBreak set
// (LF, VT, FF, CR, FS/GS/RS, NEL, LS, PS) plus the rest of the C0 and C1 control
// classes, so the escape test and Data's fold share ONE notion of what makes a
// line; «», which opens a data block; and the backtick and the double quote,
// which close the backtick span a path is printed in and the `"…"` the
// session-start block tells the reader to pass to every tool.
//
// The line-ending class is taken WHOLE rather than approximated by `r < 0x20`:
// that test is C0 and DEL only, so NEL (U+0085) — the only C1 control in
// isLineBreak — and the Zl/Zp separators LS (U+2028) and PS (U+2029), which are
// not control characters at all, went through raw and a project name holding one
// forged a line in the session-start block's own heading (#1363). isLineBreak is
// the shared set the content fold already uses; asking `unicode.IsControl`
// alongside it covers the C0/C1 classes without a second hand-written list.
//
// Beside those four classes it also escapes the invisible FORMAT characters
// (category Cf), which end no line and close no construct but hide a spelling
// inside one: a name holding U+200B is a different name from the one a reader
// sees, so the two must not print alike (#1663).
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
		case isLineEndingOrControl(r):
			return true
		case isInvisibleFormat(r):
			return true
		case r == '`', r == '"', r == '«', r == '»':
			return true
		}
	}
	return false
}

// isLineEndingOrControl is the line-ending class Label escapes (#1363): every rune
// isLineBreak reports, plus the rest of the C0 and C1 control classes. It is a
// named predicate because TagsLabel escapes the same class (#1488), and two
// spellings of "what ends a line" is how one of them came to miss NEL.
func isLineEndingOrControl(r rune) bool {
	return unicode.IsControl(r) || isLineBreak(r)
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
	// The same set Data folds (isLineBreak), so a preview and a memory line agree
	// on what ends a line (#911): U+2028, U+0085, VT, FF and FS/GS/RS too.
	if i := strings.IndexFunc(s, isLineBreak); i >= 0 {
		s = s[:i]
	}
	r := []rune(s)
	if len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

// isTokenRune is the set Token writes bare. It is memory.SafeToken's own set,
// reached through its unexported name here because three renderers in this package
// consult it on a rune at a time rather than on a whole string — Label's escape
// test and the two pipeline decisions about what an id may be shown as.
//
// It is the scope-name set the label has always used, widened by nothing: the id
// column's own values are 32 hex characters, and the other ids a real store holds
// (a bench corpus id, a restored snapshot's) are words with separators. Nothing
// else needs to be bare to be legible, and every character outside this set is
// exactly the class that can break a line, a record, a data block, or a reader's
// view of one.
//
// It must not drift from memory.SafeToken's set: Token delegates there, so a
// divergence would mean this package's rune-at-a-time checks decide on a
// different rule than the whole-string renderer they sit beside.
// TestTheSharedTokenRuneSetIsOneSet holds them together.
func isTokenRune(r rune) bool {
	return memory.IsSafeTokenRune(r)
}

// Data wraps untrusted stored text in «...» data delimiters, first rewriting
// any literal « or » inside it so embedded delimiters cannot terminate the
// data block early and smuggle text back out as instructions.
//
// It is exported for the same reason Token is: a second renderer printing the
// same field must reach the SAME function, and the single-record CLI dumps
// (#802) are that second renderer. `Item.Line`, the session-start block and the
// MCP listings each had their own copy of this rule before, and a copy is how
// a rule drifts — the copy that is not tested is the one that ships the bug.
// The delimiters are the reader's contract, not the caller's: text between «
// and » is DATA. The delimiters are not the only defence: a reader that goes
// line by line never tracks them, so every line break in the text is also folded
// to LineBreakEscape (#911) and a memory is always ONE physical line, whatever
// it holds.
//
// It is the ONE delimiter renderer in this package, and it reaches
// `neutralizeDelimiters` rather than building its own substitution: that is the
// same rule the tag label (#811) needs, and two spellings of a substitution a
// reader parses visually is two things to keep in step.
//
// Every other control rune is printed as a visible ASCII escape (#1489): ESC
// and the C1 controls, CSI among them, start terminal sequences that recolour,
// move the cursor, clear the screen or retitle the window, and a memory is
// printed to a terminal by the CLI. The invisible FORMAT characters (category
// Cf) are included too: the bidi controls (U+200E, U+200F, U+202A–U+202E,
// U+2066–U+2069) reorder how the rest of a line displays, including the closing
// delimiter, and the rest of the class (U+200B, U+200C, U+200D, U+2060, U+FEFF
// among them) hides a spelling inside a line, so two values that differ in
// bytes would print identically. The form is the one Label uses, the strconv
// escape (backslash x1b, a, x7f, u009b, u202e), so the two renderers agree.
// TAB is the one exception: stored prose holds tabs, and a tab only advances
// to the next stop, so it stays as written.
func Data(s string) string {
	return "«" + escapeControls(foldLineBreaks(neutralizeDelimiters(s))) + "»"
}

// escapeControls replaces each control rune that is not a TAB with its
// strconv ASCII escape. It runs after foldLineBreaks, so no line break is left
// to it; text with no such control is returned unchanged, which keeps every
// golden byte-identical.
func escapeControls(s string) string {
	if !strings.ContainsFunc(s, isEscapedControl) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	// Bytes are copied as they came, like foldLineBreaks, so an invalid byte
	// is never rewritten to U+FFFD by this pass.
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

// isEscapedControl is the set escapeControls prints as an escape: the control
// runes Label escapes, less TAB, plus the invisible FORMAT characters
// (category Cf) that unicode.IsControl does not cover — U+200E, U+200F,
// U+202A–U+202E and U+2066–U+2069 among them, which reorder how the rest of a
// line displays, and U+200B, U+200C, U+200D, U+2060 and U+FEFF among the rest,
// which hide a spelling inside a line without reordering anything.
func isEscapedControl(r rune) bool {
	return (r != '\t' && unicode.IsControl(r)) || isInvisibleFormat(r)
}

// isInvisibleFormat reports whether r is a Unicode FORMAT character — category
// Cf — and so carries no width of its own and prints as nothing a reader can
// see.
//
// It is the WHOLE category rather than the bidirectional subset the renderers
// escaped first (#1649, #1651). The bidi controls reorder the rest of a line,
// which is why they were caught first; the rest of Cf hides a spelling INSIDE
// one — U+200B ZERO WIDTH SPACE, U+200C and U+200D (the non-joiner and the
// joiner), U+2060 WORD JOINER and U+FEFF ZERO WIDTH NO-BREAK SPACE among them
// — so a tag of `gh` + U+200B + `ost` printed exactly as `ghost` while being a
// different string, and two values that differ in bytes printed identically.
//
// It is a category test rather than a hand-written list because a list holds
// only what somebody thought of, and this rule is about the class rather than
// about its members: a FORMAT character is an instruction to the renderer, not
// text a reader can see. The stored value is never changed; only the rendering
// escapes it.
//
// The lower bound is a fast path and not a second rule: the lowest FORMAT
// character is SOFT HYPHEN (U+00AD), and every code point below it is assigned
// and cannot become one.
func isInvisibleFormat(r rune) bool {
	if r < 0x00AD {
		return false
	}
	return unicode.In(r, unicode.Cf)
}

// LineBreakEscape is what a stored line break is printed as: every memory
// occupies exactly ONE physical line on every surface (#911), the way Token
// already guarantees for an identifier, so a stored newline followed by
// "- [decision] …" cannot start a line shaped like a memory line for a reader
// that goes line by line rather than tracking «...».
//
// It is a glyph rather than a backslash escape for two reasons. A literal `\n`
// is ambiguous with stored text that really contains a backslash and an n, and
// the backslash is also what Token and Label emit for their own escapes, so a
// reader could not tell which renderer spoke. And ⏎ (U+23CE RETURN SYMBOL) is
// none of the characters a line is built from — not « or », a backtick, a quote,
// a bracket, a parenthesis, a hyphen or whitespace — and not a line break to any
// reader, so it can neither close a construct nor start a line. Text that
// genuinely contains ⏎ is indistinguishable from a folded break; that is the
// price of a one-way display fold, and the stored value is never touched.
const LineBreakEscape = "⏎"

// foldLineBreaks replaces every line break a terminal or a line-splitting
// reader honours with LineBreakEscape: LF, VT, FF, CR, FS, GS, RS (the last
// three split lines in Python's str.splitlines), NEL (U+0085), LS (U+2028) and
// PS (U+2029). A CRLF pair is ONE break, so it folds to one escape. Text with
// no line break is returned unchanged, which is what keeps every golden
// byte-identical.
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
//
// It is EXPORTED, and it is the one implementation, because the label was written
// twice — here and in `mcpserver.formatMemories` — and two implementations of one
// rule are two rules, with the untested one shipping the bug. `formatMemories`
// renders the same field for the surfaces that have not moved to the assembler
// yet, and it now calls this.
//
// WHY the label is a JSON array and not a delimited one, which is the decision
// that makes the escaping below necessary: the line already delimits the one
// field that is free prose (the content, through Data), and tags are short
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
// ONE function that does it, and the one `Data` already used, because a
// second copy of a substitution a reader parses visually is a second thing to keep
// in step. `<<` is the same spelling the content uses, so a reader who has met one
// knows the other. A backtick is neutralised too, and differently; see
// `TagBacktickEscape`, which is where the two rules diverge and why.
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
//
// WHAT THE LABEL BOUNDS is a decision this function did not have to make before,
// and it is the same one AgentLabel and SourceRefLabel make: the value printed here
// is not necessarily one a writer capped (#821). `boundTags` cuts each tag at
// MaxRenderedTagLen and marks the cut — before the marshal, so the marker sits
// inside the JSON string like every other character of it, and after the
// substitution, so a shortened tag is still neutralised on the way out. The two
// rules are independent and a value that trips both must satisfy both.
func TagsLabel(tags []string) string {
	if len(tags) == 0 {
		return ""
	}
	b, err := json.Marshal(boundTags(tags))
	if err != nil {
		// Unreachable: tags is a []string, which json.Marshal always encodes. A
		// dropped label is the answer if it ever were not, because a raw
		// concatenation is the one rendering that cannot be escaped at all.
		return ""
	}
	return " tags:" + neutralizeTagLabel(string(b))
}

// MaxRenderedTagLen bounds what a listing prints of ONE tag. It is a DISPLAY
// bound, not a claim about the column, for the reason AgentLabel and
// SourceRefLabel carry one: the renderer is the one layer that cannot assume its
// input came from a writer that enforces a cap.
//
// For a tag the write-side cap is `validateTags`' tagMaxLen, and it reaches only
// the four MCP tools that take a tag list. Three writers deliberately do not reach
// it — `RestoreSnapshot` writes the column in SQL from the snapshot table,
// `CreateFromCorpus` reaches `insertMemory` directly, and `ReplaceNonManual`
// inherits the rewritten row's union of tags — and a restored artifact writes a
// tag verbatim. So an over-long tag is reachable without any of Ghost's tools
// writing one, which is exactly the class the two labels beside this one were
// added for.
//
// The value is 64, the number `mcpserver.tagMaxLen` cuts at, and the direction of
// that agreement is what matters: this bound must never be TIGHTER than the
// writer's, or a tag the writer accepted whole and stored whole would print
// shortened — a display bound quietly becoming a data change, which is the one
// thing a renderer must not do. It is allowed to be looser, because a looser print
// bound costs nothing. The writer's constant is unexported in a package that
// IMPORTS this one, so the agreement cannot be a compile-time fact here and has to
// be held by a test that can see both: `mcpserver`'s
// TestTheRendererNeverShortensATagThisWriterAccepts drives a tag of exactly
// tagMaxLen through the real tool and then through this renderer. This package's own
// test holds the value at the boundary from the other side.
const MaxRenderedTagLen = 64

// tagTruncationMarker is what an over-long tag prints as its last characters. It
// is the same shape AgentLabel and SourceRefLabel use, deliberately: a reader who
// has met one truncation marker on this line knows what the others mean, and the
// alternative — a bare cut — presents a shortened label as the whole one, which is
// a claim about the row that is not true.
const tagTruncationMarker = "…[tag truncated]"

// boundTags returns tags with every over-long entry cut to MaxRenderedTagLen and
// marked, which is the whole of the display bound for a tag list.
//
// It allocates only when it has to, because this runs on every row of every
// listing and a listing whose tags are all short — which is every listing a real
// store produces — must cost nothing. A list is bounded TAG BY TAG rather than as
// one value because a tag is a label: bounding the list as a whole would have to
// drop or merge entries, and dropping a tag is worse than showing part of one,
// exactly as a shortened tag is a different label rather than a different row.
//
// It never touches the stored value, and it cannot: it hands back a list to
// marshal, and the row keeps whatever the writer put in the column.
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
		// clampBytes, not a raw slice, for the reason SourceRefLabel uses it: this
		// bound handles values no writer vouched for, so its input is exactly the
		// untrusted non-ASCII text a writer would have refused, and a cut through a
		// multi-byte rune would put an invalid byte inside the JSON array.
		bounded[i] = clampBytes(t, MaxRenderedTagLen) + tagTruncationMarker
	}
	if bounded == nil {
		return tags
	}
	return bounded
}

// TagBacktickEscape is what a backtick in a tag prints as.
//
// It is exported for mcpserver's tag refusal, which names a refused tag with the
// same escape rather than a second copy of it.
//
// A JSON ESCAPE rather than a substitution like the guillemet's `<<`, and the
// reason is that `<<` already means something to a reader of a Ghost line: it is
// what `Data` writes for a literal `«`, so a reader who sees `<<` in a tag
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
const TagBacktickEscape = "\\u0060"

// neutralizeTagLabel is the label's own neutralisation, and it is separate from
// `neutralizeDelimiters` rather than an extension of it.
//
// `neutralizeDelimiters` is shared with `Data`, where a backtick is NOT a
// threat — the content sits inside «...», so a backtick in it cannot close anything
// Ghost printed. Widening that function would change how every content block in
// every answer renders, on no evidence that a content backtick is unsafe. So the
// label has its own, and this is the one place the two rules differ.
//
// It also writes every rune of Label's line-ending class (isLineEndingOrControl)
// and every invisible FORMAT character (isInvisibleFormat) as a JSON `\uXXXX`
// escape (#1488, #1651, #1663). `json.Marshal` escapes C0 and U+2028/U+2029 but
// leaves NEL (U+0085) and the rest of C1 raw, and an imported tag is stored
// verbatim, so a NEL in one forged a line in the session-start block. The bidi
// controls (U+200E, U+200F, U+202A–U+202E, U+2066–U+2069) are category Cf, so
// neither json.Marshal nor unicode.IsControl covered them; neither did the rest
// of the category, so a tag of `gh` + U+200B + `ost` printed as `ghost`.
//
// The escape is a JSON one, so it has to be one a JSON reader accepts: a rune
// outside the Basic Multilingual Plane is written as the SURROGATE PAIR JSON
// requires, because a four-digit `\u` cannot hold a code point above U+FFFF and
// JSON has no `\U`. Every rune of the class is otherwise in the BMP, where the
// four-digit escape is exact, and an escape inside a JSON string decodes back to
// the same rune: the array stays valid and a reader decoding it gets the stored
// tag unchanged. Outside a JSON string the marshalled array holds only ASCII
// punctuation, so no escape lands there.
func neutralizeTagLabel(s string) string {
	s = strings.ReplaceAll(neutralizeDelimiters(s), "`", TagBacktickEscape)
	if !strings.ContainsFunc(s, func(r rune) bool {
		return isLineEndingOrControl(r) || isInvisibleFormat(r)
	}) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for _, r := range s {
		if isLineEndingOrControl(r) || isInvisibleFormat(r) {
			writeJSONEscape(&b, r)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// writeJSONEscape writes r as a JSON string escape: the four-digit `\uXXXX`
// for a rune in the Basic Multilingual Plane, and the surrogate pair JSON
// requires for one above it.
func writeJSONEscape(b *strings.Builder, r rune) {
	if r > 0xFFFF {
		hi, lo := utf16.EncodeRune(r)
		fmt.Fprintf(b, "\\u%04x\\u%04x", hi, lo)
		return
	}
	fmt.Fprintf(b, "\\u%04x", r)
}

// neutralizeDelimiters rewrites the « and » that open and close a data block into
// the fixed `<<` and `>>` a reader cannot mistake for one.
//
// It is the ONE function that does it, and the fact that the tag list needed it
// (#811) is the argument: `Data` has done this for the content since the
// delimiters existed, and a field that renders its own copy of the substitution is
// a field whose copy will drift.
func neutralizeDelimiters(s string) string {
	return delimiterReplacer.Replace(s)
}

// delimiterReplacer is the substitution itself, hoisted out of Data for the
// same reason the three copies of the preview cut were hoisted into PreviewLine: a
// strings.Replacer is safe for concurrent use, and a rule that must never drift is
// the kind that must not be copyable. It is a package var rather than a per-call
// NewReplacer so it is built once instead of once per tag on every line of every
// listing.
var delimiterReplacer = strings.NewReplacer("«", "<<", "»", ">>")

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
