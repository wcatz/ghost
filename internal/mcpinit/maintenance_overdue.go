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
	"github.com/wcatz/ghost/internal/reflection"
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
	// UnsafeID marks a project whose id cannot be a filename component. Its
	// lifecycle bookkeeping cannot be written and no run can be started for
	// it, so it is reported separately from the overdue projects: the retry
	// an overdue project is offered cannot help this one.
	UnsafeID bool
}

// OverdueMaintenanceProjects returns the projects that have pending
// consolidation work and whose last lifecycle run is overdue. Projects with
// no consolidatable memories are never overdue, and neither is a project
// whose lifecycle is currently running. A project whose id cannot be a
// filename component cannot have lifecycle bookkeeping written for it at all,
// so it is returned with UnsafeID set and without a last-run figure rather
// than being dropped: dropping it hid the project from both diagnostics. A
// non-positive minInterval means the cooldown is disabled (every project with
// work and no live claim is due). consolidationEnabled gates the listing: when
// false, automatic lifecycle consolidation is not configured and no retry is
// useful, so the result is nil.
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
		n := counts[p.ID]
		if n == 0 {
			continue
		}
		if !safeProjectIDComponent(p.ID) {
			// No filename can be keyed on this id, so the project has no
			// claim or stamp to read and no run can be started for it. It is
			// still work that will never be done, so it is named with the
			// reason rather than dropped.
			out = append(out, OverdueProject{
				ID:          p.ID,
				Name:        p.Name,
				Count:       n,
				MinInterval: minInterval,
				UnsafeID:    true,
			})
			continue
		}
		if n > reflection.MaxConsolidationInput {
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

// splitMaintenanceProjects separates the projects that can be maintained
// unattended from those whose id cannot key lifecycle bookkeeping.
func splitMaintenanceProjects(projects []OverdueProject) (overdue, unsafe []OverdueProject) {
	for _, p := range projects {
		if p.UnsafeID {
			unsafe = append(unsafe, p)
			continue
		}
		overdue = append(overdue, p)
	}
	return overdue, unsafe
}

// overdueDiagnosticLines renders the overdue section as markdown lines,
// without a trailing newline. Empty when no project is overdue.
func overdueDiagnosticLines(overdue []OverdueProject, now time.Time) []string {
	if len(overdue) == 0 {
		return nil
	}
	var header string
	interval := overdue[0].MinInterval
	if interval <= 0 {
		if len(overdue) == 1 {
			header = "**Maintenance overdue:** 1 project has consolidatable memories and is due for lifecycle maintenance (lifecycle cooldown disabled); run `ghost lifecycle --project <name>` for it:"
		} else {
			header = fmt.Sprintf("**Maintenance overdue:** %d projects have consolidatable memories and are due for lifecycle maintenance (lifecycle cooldown disabled); run `ghost lifecycle --project <name>` for each:", len(overdue))
		}
	} else if len(overdue) == 1 {
		header = fmt.Sprintf("**Maintenance overdue:** 1 project has consolidatable memories and has not had lifecycle maintenance within %s; run `ghost lifecycle --project <name>` for it:", interval)
	} else {
		header = fmt.Sprintf("**Maintenance overdue:** %d projects have consolidatable memories and have not had lifecycle maintenance within %s; run `ghost lifecycle --project <name>` for each:", len(overdue), interval)
	}
	lines := []string{header}
	for _, o := range overdue {
		age := "never"
		if o.HasLastRun {
			age = fmt.Sprintf("%s ago", now.Sub(o.LastRun).Round(time.Second))
			if o.LastRun.After(now) {
				age = fmt.Sprintf("in the future (%s)", o.LastRun.Format(time.RFC3339))
			}
		}
		lines = append(lines, fmt.Sprintf("- %s (%s): %d consolidatable memories, last run %s",
			assemble.Label(o.Name), assemble.Token(o.ID), o.Count, age))
	}
	return lines
}

// unmaintainableDiagnosticLines renders the projects whose id is not a safe
// filename component as their own markdown section, without a trailing
// newline. They are not listed as overdue because the retry an overdue
// project is offered cannot help them; what they need is an id a lifecycle
// file can be keyed on.
func unmaintainableDiagnosticLines(unsafe []OverdueProject) []string {
	if len(unsafe) == 0 {
		return nil
	}
	var header string
	if len(unsafe) == 1 {
		header = "**Cannot be maintained unattended:** 1 project has consolidatable memories, but its id is not a safe filename component, so no lifecycle bookkeeping can be keyed on it:"
	} else {
		header = fmt.Sprintf("**Cannot be maintained unattended:** %d projects have consolidatable memories, but their ids are not safe filename components, so no lifecycle bookkeeping can be keyed on them:", len(unsafe))
	}
	lines := []string{header}
	for _, o := range unsafe {
		lines = append(lines, fmt.Sprintf("- %s (%s): %d consolidatable memories",
			assemble.Label(o.Name), assemble.Token(o.ID), o.Count))
	}
	return lines
}

// FormatOverdueMaintenanceSections returns the overdue section and the
// unmaintainable section separately, each ending in a newline and empty when
// it has nothing to report. A caller that caps its output per section needs
// the two apart: one combined line list lets a long overdue section push the
// unmaintainable projects past the cap, which would make the two diagnostics
// disagree about them.
func FormatOverdueMaintenanceSections(projects []OverdueProject, now time.Time) (overdue, unmaintainable string) {
	overdueProjects, unsafeProjects := splitMaintenanceProjects(projects)
	return joinDiagnosticLines(overdueDiagnosticLines(overdueProjects, now)),
		joinDiagnosticLines(unmaintainableDiagnosticLines(unsafeProjects))
}

// joinDiagnosticLines renders one section as text ending in a newline, or ""
// when the section has no lines.
func joinDiagnosticLines(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// FormatOverdueMaintenance returns the diagnostic text for overdue projects
// and for projects that cannot be maintained unattended, and whether either
// section is present. Empty when neither is.
func FormatOverdueMaintenance(projects []OverdueProject, now time.Time) (string, bool) {
	overdue, unmaintainable := FormatOverdueMaintenanceSections(projects, now)
	text := overdue + unmaintainable
	if text == "" {
		return "", false
	}
	return text, true
}

// ReportOverdueMaintenance writes the overdue and unmaintainable lines to w
// when either is present. It is a helper for the CLI status path, which prints
// `-`/`!` lines rather than the health markdown.
func ReportOverdueMaintenance(w io.Writer, projects []OverdueProject, now time.Time) {
	overdue, unsafe := splitMaintenanceProjects(projects)
	reportMaintenanceSection(w, overdueDiagnosticLines(overdue, now), "**Maintenance overdue:** ", "  ! maintenance overdue: ")
	reportMaintenanceSection(w, unmaintainableDiagnosticLines(unsafe), "**Cannot be maintained unattended:** ", "  ! cannot be maintained unattended: ")
}

// reportMaintenanceSection converts one markdown section into the status-style
// lines. The heading becomes a `!` line and its bullets are indented under it.
func reportMaintenanceSection(w io.Writer, lines []string, heading, statusLine string) {
	for i, line := range lines {
		if i == 0 {
			line = strings.TrimPrefix(line, heading)
			line = strings.TrimSuffix(line, ":")
			_, _ = fmt.Fprintf(w, "%s%s\n", statusLine, line)
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
