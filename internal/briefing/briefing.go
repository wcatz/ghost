// Package briefing generates daily briefing digests aggregating
// GitHub notifications, calendar events, and pending reminders.
package briefing

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/calendar"
	gh "github.com/wcatz/ghost/internal/github"
	"github.com/wcatz/ghost/internal/scheduler"
)

// Sources holds the optional data sources for a briefing.
type Sources struct {
	GitHub    *gh.Monitor
	Calendar  *calendar.Client
	Scheduler *scheduler.Scheduler
}

// Generate builds a morning briefing message from all available sources.
func Generate(ctx context.Context, src Sources) string {
	var sb strings.Builder
	sb.WriteString("*☀️ Ghost Morning Briefing*\n")
	sb.WriteString(fmt.Sprintf("_%s_\n\n", time.Now().Format("Monday, January 2")))

	// GitHub notifications.
	if src.GitHub != nil {
		writeGitHub(ctx, &sb, src.GitHub)
	}

	// Calendar events.
	if src.Calendar != nil {
		writeCalendar(ctx, &sb, src.Calendar)
	}

	// Pending reminders.
	if src.Scheduler != nil {
		writeReminders(ctx, &sb, src.Scheduler)
	}

	return sb.String()
}

func writeGitHub(ctx context.Context, sb *strings.Builder, mon *gh.Monitor) {
	summary, err := mon.Summary(ctx)
	if err != nil {
		sb.WriteString("*GitHub* — error fetching\n\n")
		return
	}

	total := 0
	for _, c := range summary {
		total += c
	}

	if total == 0 {
		sb.WriteString("*GitHub* — All clear ✅\n\n")
		return
	}

	sb.WriteString(fmt.Sprintf("*GitHub* — %d unread\n", total))
	for p := gh.P0; p <= gh.P4; p++ {
		if c, ok := summary[p]; ok && c > 0 {
			sb.WriteString(fmt.Sprintf("  P%d: %d %s\n", p, c, priorityLabel(p)))
		}
	}

	// Show P0-P2 details.
	urgent, _ := mon.GetByPriority(ctx, gh.P2, 5)
	if len(urgent) > 0 {
		sb.WriteString("\n")
		for _, n := range urgent {
			sb.WriteString(fmt.Sprintf("  %s `%s` — %s\n", priorityEmoji(n.Priority), n.RepoFullName, n.SubjectTitle))
		}
	}
	sb.WriteString("\n")
}

func writeCalendar(ctx context.Context, sb *strings.Builder, cal *calendar.Client) {
	events, err := cal.TodayEvents(ctx)
	if err != nil {
		sb.WriteString("*Calendar* — error fetching\n\n")
		return
	}

	if len(events) == 0 {
		sb.WriteString("*Calendar* — No events today 📅\n\n")
		return
	}

	sb.WriteString(fmt.Sprintf("*Calendar* — %d events today\n", len(events)))
	for _, e := range events {
		if e.AllDay {
			sb.WriteString(fmt.Sprintf("  📅 %s (all day)\n", e.Summary))
		} else {
			sb.WriteString(fmt.Sprintf("  🕐 %s — %s\n",
				e.StartTime.Local().Format("15:04"), e.Summary))
		}
		if e.Location != "" {
			sb.WriteString(fmt.Sprintf("     📍 %s\n", e.Location))
		}
	}
	sb.WriteString("\n")
}

func writeReminders(ctx context.Context, sb *strings.Builder, sched *scheduler.Scheduler) {
	reminders, err := sched.ListPending(ctx, 10)
	if err != nil || len(reminders) == 0 {
		return
	}

	sb.WriteString(fmt.Sprintf("*Reminders* — %d pending\n", len(reminders)))
	for _, r := range reminders {
		dueAt, _ := time.Parse(time.RFC3339, r.DueAt)
		sb.WriteString(fmt.Sprintf("  ⏰ %s — %s\n", dueAt.Local().Format("15:04"), r.Message))
	}
	sb.WriteString("\n")
}

func priorityLabel(p int) string {
	switch p {
	case gh.P0:
		return "(security)"
	case gh.P1:
		return "(review/CI)"
	case gh.P2:
		return "(mention/assign)"
	case gh.P3:
		return "(subscribed)"
	default:
		return ""
	}
}

func priorityEmoji(p int) string {
	switch p {
	case gh.P0:
		return "🔴"
	case gh.P1:
		return "🟠"
	case gh.P2:
		return "🟡"
	default:
		return "⚪"
	}
}
