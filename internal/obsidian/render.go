// Package obsidian mirrors Ghost's store into a plain-Markdown folder that
// Obsidian reads natively. Strictly one-way: Ghost → vault.
package obsidian

import (
	"fmt"
	"strings"

	"github.com/wcatz/ghost/internal/memory"
)

const banner = "> [!info] Mirrored from Ghost — edits here are not synced back.\n"

// slug derives a stable-ish, readable filename prefix from the first ~6
// content words: lowercase, alnum-only, dash-joined, max 40 chars.
// Entirely non-ASCII content degrades to "note"; identity is preserved by
// the id8 suffix in the filename.
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

func id8(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func fileName(m memory.Memory) string {
	return slug(m.Content) + "-" + id8(m.ID) + ".md"
}

func date(ts string) string {
	if len(ts) >= 10 {
		return ts[:10]
	}
	return ts
}

// yamlScalar renders s as a single-line YAML scalar. Newlines and tabs are
// flattened to spaces — the ghost_id-first, one-line-per-key invariant that
// prune's hasGhostID scan depends on — and the value is double-quoted with
// escaping whenever plain emission could change the line's YAML meaning. When
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
// Invariant for every renderer: ghost_id must be the first frontmatter key
// and every value must occupy exactly one line — prune's hasGhostID scan
// depends on it. Values are rendered through yamlScalar, which flattens
// newlines and quotes anything that would break YAML. Fixed-vocabulary values
// — category, source, and task/decision status are all CHECK-constrained in
// memory/schema.go — plus type, numerics, bools, and dates take yamlScalar's
// plain path and emit unquoted.
func fm(b *strings.Builder, key, val string) {
	fmt.Fprintf(b, "%s: %s\n", key, yamlScalar(val, false))
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

func renderMemory(m memory.Memory, links []memory.Link, fileFor map[string]string) string {
	var b strings.Builder
	b.WriteString("---\n")
	fm(&b, "ghost_id", m.ID)
	fm(&b, "type", "memory")
	fm(&b, "category", m.Category)
	fm(&b, "importance", strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", m.Importance), "0"), "."))
	fm(&b, "pinned", fmt.Sprintf("%v", m.Pinned))
	fm(&b, "project", m.ProjectID)
	fmTags(&b, m.Tags)
	fm(&b, "created", date(m.CreatedAt))
	fm(&b, "updated", date(m.UpdatedAt))
	fm(&b, "source", m.Source)
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
				fmt.Fprintf(&b, "- %s — %s (%.2f)\n", id8(other), l.Relation, l.Strength)
			}
		}
	}
	return b.String()
}

// fileNameFor derives a filename for a decision or task from its title.
func fileNameFor(title, id string) string { return slug(title) + "-" + id8(id) + ".md" }

func renderDecision(d memory.Decision) string {
	var b strings.Builder
	b.WriteString("---\n")
	fm(&b, "ghost_id", d.ID)
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
