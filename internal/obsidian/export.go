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

// Exporter mirrors the store into a vault directory.
type Exporter struct {
	Store  *memory.Store
	Logger *slog.Logger
}

// Export performs one full deterministic mirror pass. projectFilter of ""
// mirrors everything; otherwise that project (by ID or name) plus _global.
func (e *Exporter) Export(ctx context.Context, vaultDir, projectFilter string) error {
	if err := ensureVault(vaultDir); err != nil {
		return err
	}
	projects, err := e.Store.ListProjects(ctx)
	if err != nil {
		return fmt.Errorf("list projects: %w", err)
	}
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
	folders := folderNames(selected)

	// Pass 1: load all memories, build the global id → filename map for wikilinks.
	type projData struct {
		p        memory.Project
		folder   string
		memories []memory.Memory
	}
	var data []projData
	fileFor := make(map[string]string)
	for _, p := range selected {
		mems, err := e.Store.GetAll(ctx, p.ID, 100000)
		if err != nil {
			return fmt.Errorf("load memories for %s: %w", p.ID, err)
		}
		for _, m := range mems {
			fileFor[m.ID] = fileName(m)
		}
		data = append(data, projData{p: p, folder: folders[p.ID], memories: mems})
	}

	// Pass 2: render + diff-write + collect keep-set, then prune.
	keep := make(map[string]bool)
	var subtrees []string
	written, skipped := 0, 0
	for _, d := range data {
		subtrees = append(subtrees, d.folder)
		for _, m := range d.memories {
			links, err := e.Store.GetLinks(ctx, m.ID)
			if err != nil {
				return fmt.Errorf("links for %s: %w", m.ID, err)
			}
			keep[m.ID] = true
			w, err := writeIfChanged(filepath.Join(vaultDir, d.folder, "Memories", fileFor[m.ID]), renderMemory(m, links, fileFor))
			if err != nil {
				return err
			}
			count(&written, &skipped, w)
		}
		decisions, err := e.Store.ListDecisions(ctx, d.p.ID, "", 100000)
		if err != nil {
			return fmt.Errorf("decisions for %s: %w", d.p.ID, err)
		}
		for _, dec := range decisions {
			keep[dec.ID] = true
			w, err := writeIfChanged(filepath.Join(vaultDir, d.folder, "Decisions", fileNameFor(dec.Title, dec.ID)), renderDecision(dec))
			if err != nil {
				return err
			}
			count(&written, &skipped, w)
		}
		tasks, err := e.Store.ListTasks(ctx, d.p.ID, "", 100000)
		if err != nil {
			return fmt.Errorf("tasks for %s: %w", d.p.ID, err)
		}
		for _, tk := range tasks {
			keep[tk.ID] = true
			w, err := writeIfChanged(filepath.Join(vaultDir, d.folder, "Tasks", fileNameFor(tk.Title, tk.ID)), renderTask(tk))
			if err != nil {
				return err
			}
			count(&written, &skipped, w)
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
		switch {
		case r == '/' || r == '\\' || r == ':' || r == 0:
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
