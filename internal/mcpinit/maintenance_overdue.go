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

// OverdueProject describes a project with overdue maintenance.
type OverdueProject struct {
	ID          string
	Name        string
	Count       int
	LastRun     time.Time
	HasLastRun  bool
	Running     bool
	MinInterval time.Duration
	UnsafeID    bool
}

// OverdueMaintenanceProjects returns projects whose maintenance is overdue.
func OverdueMaintenanceProjects(ctx context.Context, store *memory.Store, dataDir string, now time.Time, minInterval time.Duration, clearsConsolidatableSet bool) ([]OverdueProject, error) {
	if store == nil {
		return nil, nil
	}
	if dataDir == "" {
		return nil, nil
	}
	if !clearsConsolidatableSet {
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

func maintenanceProjectLabel(name string) string {
	if placeholder, withheld := memory.TokenGuardPlaceholder("project", name); withheld {
		return placeholder
	}
	return assemble.Label(name)
}

func maintenanceProjectID(id string) string {
	if placeholder, withheld := memory.TokenGuardPlaceholder("project", id); withheld {
		return placeholder
	}
	return assemble.Token(id)
}

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
			maintenanceProjectLabel(o.Name), maintenanceProjectID(o.ID), o.Count, age))
	}
	return lines
}

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
			maintenanceProjectLabel(o.Name), maintenanceProjectID(o.ID), o.Count))
	}
	return lines
}

// FormatOverdueMaintenanceSections returns overdue and unmaintainable maintenance sections.
func FormatOverdueMaintenanceSections(projects []OverdueProject, now time.Time) (overdue, unmaintainable string) {
	overdueProjects, unsafeProjects := splitMaintenanceProjects(projects)
	return joinDiagnosticLines(overdueDiagnosticLines(overdueProjects, now)),
		joinDiagnosticLines(unmaintainableDiagnosticLines(unsafeProjects))
}

func joinDiagnosticLines(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// FormatOverdueMaintenance returns diagnostic text for overdue and unmaintainable projects.
func FormatOverdueMaintenance(projects []OverdueProject, now time.Time) (string, bool) {
	overdue, unmaintainable := FormatOverdueMaintenanceSections(projects, now)
	text := overdue + unmaintainable
	if text == "" {
		return "", false
	}
	return text, true
}

// ReportOverdueMaintenance writes overdue and unmaintainable maintenance lines.
func ReportOverdueMaintenance(w io.Writer, projects []OverdueProject, now time.Time) {
	overdue, unsafe := splitMaintenanceProjects(projects)
	reportMaintenanceSection(w, overdueDiagnosticLines(overdue, now), "**Maintenance overdue:** ", "  ! maintenance overdue: ")
	reportMaintenanceSection(w, unmaintainableDiagnosticLines(unsafe), "**Cannot be maintained unattended:** ", "  ! cannot be maintained unattended: ")
}

func reportMaintenanceSection(w io.Writer, lines []string, heading, statusLine string) {
	for i, line := range lines {
		if i == 0 {
			line = strings.TrimPrefix(line, heading)
			line = strings.TrimSuffix(line, ":")
			_, _ = fmt.Fprintf(w, "%s%s\n", statusLine, line)
			continue
		}
		if strings.HasPrefix(line, "- ") {
			_, _ = fmt.Fprintf(w, "    %s\n", line)
			continue
		}
		_, _ = fmt.Fprintf(w, "%s\n", line)
	}
}
