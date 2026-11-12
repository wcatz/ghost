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

const exportListLimit = 100000

// Exporter mirrors the store into a vault directory.
type Exporter struct {
	Store  *memory.Store
	Logger *slog.Logger

	listLimit int
}

func truncated(n, limit int) bool { return n >= limit }

// Export performs one mirror pass.
func (e *Exporter) Export(ctx context.Context, vaultDir, projectFilter string) error {
	limit := e.listLimit
	if limit <= 0 {
		limit = exportListLimit
	}
	projects, err := e.Store.ListProjects(ctx)
	if err != nil {
		return fmt.Errorf("list projects: %w", err)
	}
	selected, err := selectProjects(projects, projectFilter)
	if err != nil {
		return err
	}
	if err := ensureVault(vaultDir); err != nil {
		return err
	}
	folders := folderNames(projects)

	type projData struct {
		p        memory.Project
		folder   string
		memories []memory.Memory
		trunc    bool
	}
	var data []projData
	fileFor := make(map[string]string)
	for _, p := range selected {
		mems, err := e.Store.GetAll(ctx, p.ID, limit)
		if err != nil {
			return fmt.Errorf("load memories for %s: %w", memory.RefusalArg("project_id", p.ID), err)
		}
		trunc := truncated(len(mems), limit)
		if trunc {
			e.Logger.Warn("obsidian export: list truncated at limit", "project", memory.RefusalArg("project_id", p.ID), "kind", "memories", "limit", limit)
		}
		recs := make([]noteRecord, 0, len(mems))
		for _, m := range mems {
			recs = append(recs, noteRecord{id: m.ID, name: fileName(m)})
		}
		for id, name := range noteNames(recs) {
			fileFor[id] = name
		}
		data = append(data, projData{p: p, folder: folders[p.ID], memories: mems, trunc: trunc})
	}

	keep := make(keepSet)
	var subtrees []string
	written, skipped := 0, 0
	for _, d := range data {
		for _, m := range d.memories {
			links, err := e.Store.GetLinks(ctx, m.ID)
			if err != nil {
				return fmt.Errorf("links for %s: %w", memory.RefusalArg("memory_id", m.ID), err)
			}
			path := filepath.Join(vaultDir, d.folder, "Memories", fileFor[m.ID])
			keep[keepKey(vaultDir, path)] = struct{}{}
			w, err := writeIfChanged(path, renderMemory(m, links, fileFor))
			if err != nil {
				return err
			}
			count(&written, &skipped, w)
		}
		decisions, err := e.Store.ListDecisions(ctx, d.p.ID, "", limit)
		if err != nil {
			return fmt.Errorf("decisions for %s: %w", memory.RefusalArg("project_id", d.p.ID), err)
		}
		if truncated(len(decisions), limit) {
			e.Logger.Warn("obsidian export: list truncated at limit", "project", memory.RefusalArg("project_id", d.p.ID), "kind", "decisions", "limit", limit)
			d.trunc = true
		}
		decRecs := make([]noteRecord, 0, len(decisions))
		for _, dec := range decisions {
			decRecs = append(decRecs, noteRecord{id: dec.ID, name: fileNameFor(dec.Title, dec.ID)})
		}
		decNames := noteNames(decRecs)
		for _, dec := range decisions {
			path := filepath.Join(vaultDir, d.folder, "Decisions", decNames[dec.ID])
			keep[keepKey(vaultDir, path)] = struct{}{}
			w, err := writeIfChanged(path, renderDecision(dec))
			if err != nil {
				return err
			}
			count(&written, &skipped, w)
		}
		tasks, err := e.Store.ListTasks(ctx, d.p.ID, "", limit)
		if err != nil {
			return fmt.Errorf("tasks for %s: %w", memory.RefusalArg("project_id", d.p.ID), err)
		}
		if truncated(len(tasks), limit) {
			e.Logger.Warn("obsidian export: list truncated at limit", "project", memory.RefusalArg("project_id", d.p.ID), "kind", "tasks", "limit", limit)
			d.trunc = true
		}
		taskRecs := make([]noteRecord, 0, len(tasks))
		for _, tk := range tasks {
			taskRecs = append(taskRecs, noteRecord{id: tk.ID, name: fileNameFor(tk.Title, tk.ID)})
		}
		taskNames := noteNames(taskRecs)
		for _, tk := range tasks {
			path := filepath.Join(vaultDir, d.folder, "Tasks", taskNames[tk.ID])
			keep[keepKey(vaultDir, path)] = struct{}{}
			w, err := writeIfChanged(path, renderTask(tk))
			if err != nil {
				return err
			}
			count(&written, &skipped, w)
		}
		if !d.trunc {
			subtrees = append(subtrees, d.folder)
		}
	}
	var knownFolders []string
	if projectFilter == "" {
		knownFolders = make([]string, 0, len(folders))
		for _, f := range folders {
			knownFolders = append(knownFolders, f)
		}
	}
	if err := prune(vaultDir, subtrees, keep, knownFolders); err != nil {
		return err
	}
	e.Logger.Info("obsidian export complete", "projects", len(data), "written", written, "unchanged", skipped)
	return nil
}

func selectProjects(projects []memory.Project, filter string) ([]memory.Project, error) {
	if filter == "" {
		return projects, nil
	}
	var match []memory.Project
	for _, p := range projects {
		if p.ID == filter {
			match = []memory.Project{p}
			break
		}
	}
	if match == nil {
		for _, p := range projects {
			if p.Name == filter {
				match = append(match, p)
			}
		}
	}
	if len(match) == 0 {
		return nil, fmt.Errorf("no project matches %s", memory.RefusalArg("project", filter))
	}
	if len(match) > 1 {
		ids := make([]string, 0, len(match))
		for _, p := range match {
			ids = append(ids, memory.RefusalArg("project_id", p.ID))
		}
		return nil, fmt.Errorf("project filter %s matches multiple projects by name: %s — name a project by its unique id",
			memory.RefusalArg("project", filter), strings.Join(ids, ", "))
	}
	selected := projects[:0:0]
	for _, p := range projects {
		if p.ID == match[0].ID || p.ID == globalProjectID {
			selected = append(selected, p)
		}
	}
	return selected, nil
}

func count(written, skipped *int, wrote bool) {
	if wrote {
		*written++
	} else {
		*skipped++
	}
}

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

const maxFolderName = 120

func folderNames(projects []memory.Project) map[string]string {
	folders := make(map[string]string, len(projects))
	seen := make(map[string]bool, len(projects))
	for _, p := range projects {
		f := folderName(p)
		if seen[strings.ToLower(f)] {
			f += "-" + idToken(p.ID)
		}
		if f == "." || f == ".." || f == "" || !filepath.IsLocal(f) || len(f) > maxFolderName {
			f = "project-" + idToken(p.ID)
		}
		seen[strings.ToLower(f)] = true
		folders[p.ID] = f
	}
	return folders
}
