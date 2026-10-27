package mcpinit

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/memory"
)

// OverdueProject is one project whose maintenance is overdue: it holds
// consolidatable memories and its last lifecycle run is outside the cooldown
// window (or it has never had one). A currently running lifecycle is not
// overdue.
type OverdueProject struct {
	ID          string
	Name        string
	Count       int
	LastRun     time.Time
	HasLastRun  bool
	Running     bool
	MinInterval time.Duration
}

// OverdueMaintenanceProjects returns the projects that have pending
// consolidation work and whose last lifecycle run is overdue. Projects with
// no consolidatable memories are never overdue, and neither is a project
// whose lifecycle is currently running. An unsafe project id (one that
// cannot be a filename component) is skipped, because its bookkeeping cannot
// be read. A non-positive minInterval means the cooldown is disabled (every
// project with work and no live claim is due). consolidationEnabled gates
// the listing: when false, automatic lifecycle consolidation is not configured
// and no retry is useful, so the result is nil.
func OverdueMaintenanceProjects(ctx context.Context, store *memory.Store, dataDir string, now time.Time, minInterval time.Duration, consolidationEnabled bool) ([]OverdueProject, error) {
	if store == nil {
		return nil, nil
	}
	if dataDir == "" {
		return nil, nil
	}
	if !consolidationEnabled {
		return nil, nil
	}
	noCooldown := minInterval <= 0
	projects, err := store.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	counts, err := store.ConsolidatableCounts(ctx)
	if err != nil {
		return nil, err
	}
	var out []OverdueProject
	for _, p := range projects {
		if p.ID == memory.GlobalProjectID {
			continue
		}
		if !safeProjectIDComponent(p.ID) {
			continue
		}
		n := counts[p.ID]
		if n == 0 {
			continue
		}
		pidPath := filepath.Join(dataDir, "lifecycle-"+p.ID+".pid")
		if isAlive(pidPath) {
			continue
		}
		lastRun, ok := lifecycleStartStamp(dataDir, p.ID)
		if ok {
			if !noCooldown {
				skip, _ := lifecycleCooldownActive(dataDir, p.ID, minInterval, now)
				if skip {
					// Inside the cooldown window, not overdue.
					continue
				}
			}
			out = append(out, OverdueProject{
				ID:          p.ID,
				Name:        p.Name,
				Count:       n,
				LastRun:     lastRun,
				HasLastRun:  true,
				MinInterval: minInterval,
			})
		} else {
			out = append(out, OverdueProject{
				ID:          p.ID,
				Name:        p.Name,
				Count:       n,
				HasLastRun:  false,
				MinInterval: minInterval,
			})
		}
	}
	return out, nil
}

// FormatOverdueMaintenance returns the diagnostic text for overdue projects,
// and whether any were overdue. Empty when none are.
func FormatOverdueMaintenance(overdue []OverdueProject, now time.Time) (string, bool) {
	if len(overdue) == 0 {
		return "", false
	}
	var sb strings.Builder
	interval := overdue[0].MinInterval
	if len(overdue) == 1 {
		fmt.Fprintf(&sb, "**Maintenance overdue:** 1 project has consolidatable memories and has not had lifecycle maintenance within %s; run `ghost lifecycle --project <name>` for it:\n", interval)
	} else {
		fmt.Fprintf(&sb, "**Maintenance overdue:** %d projects have consolidatable memories and have not had lifecycle maintenance within %s; run `ghost lifecycle --project <name>` for each:\n", len(overdue), interval)
	}
	for _, o := range overdue {
		age := "never"
		if o.HasLastRun {
			age = fmt.Sprintf("%s ago", now.Sub(o.LastRun).Round(time.Second))
			if o.LastRun.After(now) {
				age = fmt.Sprintf("in the future (%s)", o.LastRun.Format(time.RFC3339))
			}
		}
		fmt.Fprintf(&sb, "- %s (%s): %d consolidatable memories, last run %s\n",
			assemble.Label(o.Name), assemble.Token(o.ID), o.Count, age)
	}
	return sb.String(), true
}

// ReportOverdueMaintenance writes the overdue line to w when any projects
// are overdue. It is a helper for the CLI status path, which prints `-`/`!`
// lines rather than the health markdown.
func ReportOverdueMaintenance(w io.Writer, overdue []OverdueProject, now time.Time) {
	text, ok := FormatOverdueMaintenance(overdue, now)
	if !ok {
		return
	}
	// Render as the status-style lines: the health markdown header is not
	// appropriate on the CLI, so rewrite the first line.
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	for i, line := range lines {
		if i == 0 {
			line = strings.TrimPrefix(line, "**Maintenance overdue:** ")
			line = strings.TrimSuffix(line, ":")
			_, _ = fmt.Fprintf(w, "  ! maintenance overdue: %s\n", line)
			continue
		}
		// Bullet lines already name the project and the count.
		if strings.HasPrefix(line, "- ") {
			_, _ = fmt.Fprintf(w, "    %s\n", line)
			continue
		}
		_, _ = fmt.Fprintf(w, "%s\n", line)
	}
}
