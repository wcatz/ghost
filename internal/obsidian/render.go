// Package obsidian mirrors Ghost's store into a plain-Markdown folder that
// Obsidian reads natively. Strictly one-way: Ghost → vault.
package obsidian

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/wcatz/ghost/internal/memory"
)

const banner = "> [!info] Mirrored from Ghost — edits here are not synced back.\n"

// slug derives a stable-ish, readable filename prefix from the first ~6
// content words: lowercase, alnum-only, dash-joined, max 40 chars.
// Entirely non-ASCII content degrades to "note"; identity is preserved by
// the id token in the filename.
func slug(content string) string {
	var words []string
	for _, w := range strings.Fields(strings.ToLower(content)) {
		var b strings.Builder
		for _, r := range w {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
				b.WriteRune(r)
			}
		}
		if b.Len() > 0 {
			words = append(words, b.String())
		}
		if len(words) == 6 {
			break
		}
	}
	s := strings.Join(words, "-")
	if len(s) > 40 {
		s = strings.TrimRight(s[:40], "-")
	}
	if s == "" {
		return "note"
	}
	return s
}

// aliasLabel derives a short, readable display name from text: the first line,
// whitespace-collapsed, capped at 60 runes. Obsidian shows it in the graph and
// Quick Switcher instead of the id-suffixed filename. Empty when text has no
// usable content, in which case the caller omits the aliases key entirely.
func aliasLabel(text string) string {
	line := text
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	line = strings.Join(strings.Fields(line), " ")
	const max = 60
	if r := []rune(line); len(r) > max {
		line = strings.TrimRight(string(r[:max]), " ")
	}
	return line
}

// idTokenWidth is how many bytes of an id name a note. It is also the width
// adversarial.AssertLocalName's own bound leaves room for, which is what keeps
// the fixture and this function from drifting apart silently.
const idTokenWidth = 8

// idToken is the filename fragment that identifies a record: the first
// idTokenWidth bytes of its id when those name a single path component, and
// otherwise a hash of the whole id.
//
// Every id ghost mints is hex, so the plain path is the only one a real record
// takes and its note keeps the name it already had. The hashed path is for ids
// that arrived from somewhere else: a portable artifact carries its own ids and
// Store.ImportMemory writes them verbatim, so an id can hold a separator, a NUL
// or a backslash. A separator in that fragment is not cosmetic — the note lands
// outside the Memories/ subtree prune watches and is then deleted by it, because
// the keep-set holds the un-cleaned name and never matches the file on disk; a
// NUL makes the write fail with EINVAL, which fails the whole export on every
// run and every retry. Hashing keeps the fragment one path element, and unlike
// replacing the offending bytes it keeps two different ids apart, so two hostile
// ids cannot collapse onto one filename.
//
// "Names a single path component" is deliberately permissive — a dash, a dot or
// a space in the prefix is left exactly as it was — so the only ids that take
// the hashed path are the ones that could have escaped or could not be created.
// A tab, a newline or a carriage return is the one addition to that set: such a
// name is a legal path component, but it is invisible to `ls`, breaks a glob in
// any shell that does not quote its output, and cannot be typed by a user who
// wants to open the note. A name nobody can name is not a name.
func idToken(id string) string {
	head := id
	if len(head) > idTokenWidth {
		head = head[:idTokenWidth]
	}
	if head != "" && head != "." && head != ".." &&
		!strings.ContainsAny(head, "/\\\t\n\r") && !strings.ContainsRune(head, 0) {
		return head
	}
	sum := sha256.Sum256([]byte(id))
	return "x" + hex.EncodeToString(sum[:])[:idTokenWidth-1]
}

func fileName(m memory.Memory) string {
	return slug(m.Content) + "-" + idToken(m.ID) + ".md"
}

func date(ts string) string {
	if len(ts) >= 10 {
		return ts[:10]
	}
	return ts
}

// yamlScalar renders s as a single-line YAML scalar. Newlines and tabs are
// flattened to spaces — the one-line-per-key rule that hasGhostID's scan depends
// on — and the value is double-quoted with escaping whenever plain emission could
// change the line's YAML meaning. The flattening is lossy and always will be, so
// nothing may be keyed on reading an id back out of a note: see keepSet. When
// flow is true the value is a flow-sequence item (a tag), so the structural
// characters , [ ] { } force quoting anywhere in the string, not just at the
// start. Well-formed values (hex ids, plain project names and tags) take the
// plain path and render byte-identically to an unescaped emission.
func yamlScalar(s string, flow bool) string {
	s = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ", "\t", " ").Replace(s)
	if needsYAMLQuote(s, flow) {
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
	}
	return s
}

// needsYAMLQuote reports whether s must be double-quoted to survive as a
// single-line YAML scalar (see yamlScalar). s is assumed already flattened.
func needsYAMLQuote(s string, flow bool) bool {
	if s == "" || strings.TrimSpace(s) != s { // empty, or leading/trailing space
		return true
	}
	switch s[0] { // leading YAML indicator characters
	case '!', '&', '*', '?', '|', '>', '%', '@', '`', '"', '\'', '#', ',', '[', ']', '{', '}', '-', ':', ' ':
		return true
	}
	if strings.HasSuffix(s, ":") || strings.Contains(s, ": ") || strings.Contains(s, " #") || strings.Contains(s, `"`) {
		return true
	}
	if flow {
		if strings.ContainsAny(s, ",[]{}") {
			return true
		}
		// Tags are always strings; quote reserved scalars so a tag like "no" or
		// "on" stays a string rather than coercing to a bool/null. Scalar fields
		// are not checked here: pinned/importance/dates are trusted YAML
		// literals emitted by their renderers and must stay bare.
		switch strings.ToLower(s) {
		case "~", "null", "true", "false", "yes", "no", "on", "off":
			return true
		}
	}
	return false
}

// fm writes one frontmatter line.
//
// Invariant for every renderer: every value must occupy exactly one line, and
// ghost_id must be the first frontmatter key.
//
// The one-line rule is what hasGhostID relies on: it scans to the first `---`
// and takes the one `ghost_id:` line it finds, so a value carrying a newline
// would either split the block or push a second key out of it. Putting ghost_id
// first is the convention a reader sees, not something the scan needs. Values
// are rendered through yamlScalar, which flattens newlines and quotes anything
// that would break YAML. Fixed-vocabulary values — category, source, and
// task/decision status are all CHECK-constrained in memory/schema.go — plus
// type, numerics, bools, and dates take yamlScalar's plain path and emit
// unquoted.
func fm(b *strings.Builder, key, val string) {
	fmt.Fprintf(b, "%s: %s\n", key, yamlScalar(val, false))
}

// fmAlias writes the aliases flow list when label is non-empty. The single-
// line flow form preserves the one-line-per-key frontmatter invariant, and the
// label is rendered through yamlScalar (flow=true) so a colon- or bracket-
// bearing preview stays a valid quoted scalar.
func fmAlias(b *strings.Builder, label string) {
	if label == "" {
		return
	}
	fmt.Fprintf(b, "aliases: [%s]\n", yamlScalar(label, true))
}

// fmTags writes the tags flow list. Each tag is rendered as a flow-sequence
// item via yamlScalar (flow=true), which quotes any tag containing flow-
// structural characters or a YAML indicator so the composed [a, b] list always
// parses — '#urgent' and 'status: open' survive intact rather than corrupting
// the note's whole frontmatter.
func fmTags(b *strings.Builder, tags []string) {
	items := make([]string, 0, len(tags))
	for _, tag := range tags {
		items = append(items, yamlScalar(tag, true))
	}
	fmt.Fprintf(b, "tags: [%s]\n", strings.Join(items, ", "))
}

// edgeRelations is the fixed alphabetical emission order of the five typed
// frontmatter edge keys. directed=true means the field is written only on
// the link's source endpoint; directed=false (related, elaborates) writes on
// both endpoints, matching ## Related's both-endpoint view.
var edgeRelations = []struct {
	name     string
	directed bool
}{
	{name: "causes", directed: true},
	{name: "contradicts", directed: true},
	{name: "elaborates", directed: false},
	{name: "related", directed: false},
	{name: "supersedes", directed: true},
}

// fmEdges appends typed edge fields for m after the existing frontmatter
// keys. Targets missing from fileFor are skipped (no dead wikilinks under
// --project); a relation with zero remaining targets omits its key. Filenames
// are sorted so re-exports stay byte-identical regardless of GetLinks order.
func fmEdges(b *strings.Builder, m memory.Memory, links []memory.Link, fileFor map[string]string) {
	byRel := make(map[string][]string)
	seen := make(map[string]map[string]bool)
	for _, l := range links {
		var directed bool
		var known bool
		for _, er := range edgeRelations {
			if er.name == l.Relation {
				directed, known = er.directed, true
				break
			}
		}
		if !known {
			continue
		}
		other := l.TargetID
		if directed {
			if l.SourceID != m.ID {
				continue
			}
		} else if other == m.ID {
			other = l.SourceID
		}
		f, ok := fileFor[other]
		if !ok {
			continue
		}
		if seen[l.Relation] == nil {
			seen[l.Relation] = make(map[string]bool)
		}
		if seen[l.Relation][f] {
			continue
		}
		seen[l.Relation][f] = true
		byRel[l.Relation] = append(byRel[l.Relation], f)
	}
	for _, er := range edgeRelations {
		targets := byRel[er.name]
		if len(targets) == 0 {
			continue
		}
		sort.Strings(targets)
		items := make([]string, 0, len(targets))
		for _, f := range targets {
			items = append(items, yamlScalar("[["+strings.TrimSuffix(f, ".md")+"]]", true))
		}
		fmt.Fprintf(b, "%s: [%s]\n", er.name, strings.Join(items, ", "))
	}
}

func renderMemory(m memory.Memory, links []memory.Link, fileFor map[string]string) string {
	var b strings.Builder
	b.WriteString("---\n")
	fm(&b, "ghost_id", m.ID)
	fmAlias(&b, aliasLabel(m.Content))
	fm(&b, "type", "memory")
	fm(&b, "category", m.Category)
	fm(&b, "importance", strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", m.Importance), "0"), "."))
	fm(&b, "pinned", fmt.Sprintf("%v", m.Pinned))
	fm(&b, "project", m.ProjectID)
	fmTags(&b, m.Tags)
	fm(&b, "created", date(m.CreatedAt))
	fm(&b, "updated", date(m.UpdatedAt))
	fm(&b, "source", m.Source)
	fmEdges(&b, m, links, fileFor)
	b.WriteString("---\n")
	b.WriteString(banner)
	b.WriteString("\n")
	b.WriteString(strings.TrimRight(m.Content, "\n"))
	b.WriteString("\n")
	if len(links) > 0 {
		b.WriteString("\n## Related\n")
		for _, l := range links {
			other := l.TargetID
			if other == m.ID {
				other = l.SourceID
			}
			if f, ok := fileFor[other]; ok {
				fmt.Fprintf(&b, "- [[%s]] — %s (%.2f)\n", strings.TrimSuffix(f, ".md"), l.Relation, l.Strength)
			} else {
				fmt.Fprintf(&b, "- %s — %s (%.2f)\n", idToken(other), l.Relation, l.Strength)
			}
		}
	}
	return b.String()
}

// fileNameFor derives a filename for a decision or task from its title.
func fileNameFor(title, id string) string { return slug(title) + "-" + idToken(id) + ".md" }

func renderDecision(d memory.Decision) string {
	var b strings.Builder
	b.WriteString("---\n")
	fm(&b, "ghost_id", d.ID)
	fmAlias(&b, aliasLabel(d.Title))
	fm(&b, "type", "decision")
	fm(&b, "status", d.Status)
	fm(&b, "project", d.ProjectID)
	fmTags(&b, d.Tags)
	fm(&b, "created", date(d.CreatedAt))
	fm(&b, "updated", date(d.UpdatedAt))
	b.WriteString("---\n")
	b.WriteString(banner)
	fmt.Fprintf(&b, "\n# %s\n\n%s\n", d.Title, strings.TrimRight(d.Decision, "\n"))
	if d.Rationale != "" {
		fmt.Fprintf(&b, "\n## Rationale\n\n%s\n", strings.TrimRight(d.Rationale, "\n"))
	}
	if len(d.Alternatives) > 0 {
		b.WriteString("\n## Alternatives\n\n")
		for _, a := range d.Alternatives {
			fmt.Fprintf(&b, "- %s\n", a)
		}
	}
	return b.String()
}

func renderTask(t memory.Task) string {
	var b strings.Builder
	b.WriteString("---\n")
	fm(&b, "ghost_id", t.ID)
	fmAlias(&b, aliasLabel(t.Title))
	fm(&b, "type", "task")
	fm(&b, "status", t.Status)
	fm(&b, "priority", fmt.Sprintf("%d", t.Priority))
	fm(&b, "project", t.ProjectID)
	fm(&b, "created", date(t.CreatedAt))
	fm(&b, "updated", date(t.UpdatedAt))
	b.WriteString("---\n")
	b.WriteString(banner)
	fmt.Fprintf(&b, "\n# %s\n", t.Title)
	if t.Description != "" {
		fmt.Fprintf(&b, "\n%s\n", strings.TrimRight(t.Description, "\n"))
	}
	if t.Notes != "" {
		fmt.Fprintf(&b, "\n## Notes\n\n%s\n", strings.TrimRight(t.Notes, "\n"))
	}
	return b.String()
}

// noteRecord is one record headed for a note directory: its id and the name
// fileName or fileNameFor builds for it.
type noteRecord struct{ id, name string }

// noteNames assigns every record bound for ONE directory a note name of its own.
//
// A built name is the title slug plus idToken, the first idTokenWidth bytes of
// the id, so two records with one title whose ids agree on those bytes are built
// the same name — imported ids reach that easily — and the second write used to
// overwrite the first while Export reported success. Names are compared case-
// folded, because a vault on a case-insensitive filesystem merges names that
// differ only in case just as surely.
//
// The rule keeps every name that was ever unique exactly as it was: a record
// whose built name nobody else in the directory has keeps it, so this changes no
// note in a vault without a collision. Within a colliding group the SMALLEST id
// (bytewise) keeps the built name and every other member is named
// <stem>-<8 hex of sha256(id)>.md. The winner depends on the set of ids alone,
// never on the order the store lists them in, so two passes over the same store
// cannot swap names and churn the vault. Retention is unchanged: the caller puts
// these names in the keep-set, so when the record holding the plain name goes,
// the survivor inherits it and its old note is pruned as stale like any other.
//
// A suffixed name could still meet another record's built name, because an
// imported id can be chosen to make it. Such a name is replaced by the stem plus
// the WHOLE 64-hex hash: that cannot meet a built name (a built stem is at most
// 40 slug bytes + "-" + idTokenWidth bytes, shorter than the hash alone) nor
// another full-hash name (two of them agree only if the hashes, so the ids, do).
// The suffix is hex and the stem a name idToken already made safe, so the result
// stays one path component, well under any filesystem's name limit.
func noteNames(recs []noteRecord) map[string]string {
	out := make(map[string]string, len(recs))
	groups := make(map[string][]noteRecord, len(recs))
	for _, r := range recs {
		k := strings.ToLower(r.name)
		groups[k] = append(groups[k], r)
	}
	claimed := make(map[string]bool, len(recs))
	var losers []noteRecord
	for k, g := range groups {
		sort.Slice(g, func(i, j int) bool { return g[i].id < g[j].id })
		out[g[0].id] = g[0].name
		claimed[k] = true
		losers = append(losers, g[1:]...)
	}
	sort.Slice(losers, func(i, j int) bool { return losers[i].id < losers[j].id })
	for _, r := range losers {
		stem := strings.TrimSuffix(r.name, ".md")
		sum := sha256.Sum256([]byte(r.id))
		h := hex.EncodeToString(sum[:])
		name := stem + "-" + h[:8] + ".md"
		if claimed[strings.ToLower(name)] {
			name = stem + "-" + h + ".md"
		}
		claimed[strings.ToLower(name)] = true
		out[r.id] = name
	}
	return out
}
