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

// fm writes one frontmatter line.
func fm(b *strings.Builder, key, val string) {
	fmt.Fprintf(b, "%s: %s\n", key, val)
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
	fm(&b, "tags", "["+strings.Join(m.Tags, ", ")+"]")
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
	fm(&b, "tags", "["+strings.Join(d.Tags, ", ")+"]")
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
