package obsidian

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/wcatz/ghost/internal/memory"
)

const globalProjectID = "_global"

// exportListLimit caps each per-project list query. A list that comes back
// exactly this long may have been truncated by the store — see truncated.
const exportListLimit = 100000

// Exporter mirrors the store into a vault directory.
type Exporter struct {
	Store  *memory.Store
	Logger *slog.Logger

	// listLimit overrides exportListLimit when > 0 (tests only).
	listLimit int
}

// truncated reports whether a list of n rows fetched with the given limit
// may have been silently cut off by the store. A truncated list would leave
// entities out of the keep-set, and prune would then delete their notes —
// so callers must treat the project as unsafe to prune.
func truncated(n, limit int) bool { return n >= limit }

// Export performs one full deterministic mirror pass. projectFilter of ""
// mirrors everything; otherwise that project (by ID or name) plus _global.
func (e *Exporter) Export(ctx context.Context, vaultDir, projectFilter string) error {
	if err := ensureVault(vaultDir); err != nil {
		return err
	}
	limit := e.listLimit
	if limit <= 0 {
		limit = exportListLimit
	}
	projects, err := e.Store.ListProjects(ctx)
	if err != nil {
		return fmt.Errorf("list projects: %w", err)
	}
	// Folder assignment is computed over ALL projects before filtering so a
	// --project run places notes in exactly the same folder as an unfiltered
	// run, regardless of case-collision disambiguation.
	folders := folderNames(projects)
	selected := projects[:0:0]
	matched := false
	for _, p := range projects {
		switch {
		case projectFilter == "":
			selected = append(selected, p)
		case p.ID == projectFilter || p.Name == projectFilter:
			selected = append(selected, p)
			matched = true
		case p.ID == globalProjectID:
			selected = append(selected, p) // globals ride along with any filter
		}
	}
	if projectFilter != "" && !matched {
		return fmt.Errorf("no project matches %q", projectFilter)
	}

	// Pass 1: load all memories, build the global id → filename map for wikilinks.
	type projData struct {
		p        memory.Project
		folder   string
		memories []memory.Memory
		trunc    bool // some list hit the limit — pruning this folder is unsafe
	}
	var data []projData
	fileFor := make(map[string]string)
	for _, p := range selected {
		mems, err := e.Store.GetAll(ctx, p.ID, limit)
		if err != nil {
			return fmt.Errorf("load memories for %s: %w", p.ID, err)
		}
		trunc := truncated(len(mems), limit)
		if trunc {
			e.Logger.Warn("obsidian export: list truncated at limit", "project", p.ID, "kind", "memories", "limit", limit)
		}
		for _, m := range mems {
			fileFor[m.ID] = fileName(m)
		}
		data = append(data, projData{p: p, folder: folders[p.ID], memories: mems, trunc: trunc})
	}

	// Pass 2: render + diff-write + collect keep-set, then prune. keep maps
	// each entity's ghost_id to its canonical basename this pass: content
	// edits rewrite a memory in place (same ID, new slug), so prune must
	// drop old-slug files even though their ghost_id is still live.
	keep := make(map[string]string)
	var subtrees []string
	written, skipped := 0, 0
	for _, d := range data {
		for _, m := range d.memories {
			links, err := e.Store.GetLinks(ctx, m.ID)
			if err != nil {
				return fmt.Errorf("links for %s: %w", m.ID, err)
			}
			keep[m.ID] = fileFor[m.ID]
			w, err := writeIfChanged(filepath.Join(vaultDir, d.folder, "Memories", fileFor[m.ID]), renderMemory(m, links, fileFor))
			if err != nil {
				return err
			}
			count(&written, &skipped, w)
		}
		decisions, err := e.Store.ListDecisions(ctx, d.p.ID, "", limit)
		if err != nil {
			return fmt.Errorf("decisions for %s: %w", d.p.ID, err)
		}
		if truncated(len(decisions), limit) {
			e.Logger.Warn("obsidian export: list truncated at limit", "project", d.p.ID, "kind", "decisions", "limit", limit)
			d.trunc = true
		}
		for _, dec := range decisions {
			name := fileNameFor(dec.Title, dec.ID)
			keep[dec.ID] = name
			w, err := writeIfChanged(filepath.Join(vaultDir, d.folder, "Decisions", name), renderDecision(dec))
			if err != nil {
				return err
			}
			count(&written, &skipped, w)
		}
		tasks, err := e.Store.ListTasks(ctx, d.p.ID, "", limit)
		if err != nil {
			return fmt.Errorf("tasks for %s: %w", d.p.ID, err)
		}
		if truncated(len(tasks), limit) {
			e.Logger.Warn("obsidian export: list truncated at limit", "project", d.p.ID, "kind", "tasks", "limit", limit)
			d.trunc = true
		}
		for _, tk := range tasks {
			name := fileNameFor(tk.Title, tk.ID)
			keep[tk.ID] = name
			w, err := writeIfChanged(filepath.Join(vaultDir, d.folder, "Tasks", name), renderTask(tk))
			if err != nil {
				return err
			}
			count(&written, &skipped, w)
		}
		// A truncated list means the keep-set is incomplete for this project:
		// pruning would delete notes for entities we simply didn't see. Skip
		// the folder — stale extras beat silent deletions.
		if !d.trunc {
			subtrees = append(subtrees, d.folder)
		}
	}
	if err := prune(vaultDir, subtrees, keep); err != nil {
		return err
	}
	e.Logger.Info("obsidian export complete", "projects", len(data), "written", written, "unchanged", skipped)
	return nil
}

func count(written, skipped *int, wrote bool) {
	if wrote {
		*written++
	} else {
		*skipped++
	}
}

// folderName maps a project to its vault folder. _global gets "Global";
// otherwise the sanitized project name (fallback: ID).
func folderName(p memory.Project) string {
	if p.ID == globalProjectID {
		return "Global"
	}
	name := p.Name
	if name == "" {
		name = p.ID
	}
	var b []rune
	for _, r := range name {
		switch r {
		case '/', '\\', ':', 0:
			b = append(b, '-')
		default:
			b = append(b, r)
		}
	}
	return string(b)
}

// folderNames maps each project ID to a distinct vault folder. Folders that
// collide case-insensitively (APFS/NTFS would silently merge "Foo" and
// "foo") are disambiguated: the first keeps its plain name, later collisions
// get "-" + the first 8 chars of the project ID appended. Input order is
// deterministic (ListProjects sorts by name), so folder assignment is too.
//
// Containment: a computed folder of ".", "..", "", or anything failing
// filepath.IsLocal is replaced with "project-" + id8 — the write path must
// stay under the vault root no matter what the projects table holds, just
// like prune's subtree guard on the delete path.
//
// id8 collisions are astronomically unlikely (hex UUIDs); a collision would
// merge folders/files in the vault without any data loss in the store.
func folderNames(projects []memory.Project) map[string]string {
	folders := make(map[string]string, len(projects))
	seen := make(map[string]bool, len(projects))
	for _, p := range projects {
		f := folderName(p)
		if seen[strings.ToLower(f)] {
			f += "-" + id8(p.ID)
		}
		if f == "." || f == ".." || f == "" || !filepath.IsLocal(f) {
			f = "project-" + id8(p.ID)
		}
		seen[strings.ToLower(f)] = true
		folders[p.ID] = f
	}
	return folders
}
