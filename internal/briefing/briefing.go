// Package briefing generates daily briefing digests aggregating
// GitHub notifications, calendar events, and pending reminders.
// Output is MarkdownV2-safe for Telegram.
package briefing

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/calendar"
	gh "github.com/wcatz/ghost/internal/github"
	goog "github.com/wcatz/ghost/internal/google"
	"github.com/wcatz/ghost/internal/mdv2"
	"github.com/wcatz/ghost/internal/scheduler"
)

// GoogleProvider is the interface for Google Calendar/Gmail access.
type GoogleProvider interface {
	TodayEvents(ctx context.Context) ([]goog.Event, error)
	UnreadCount(ctx context.Context) (int, error)
	RecentUnread(ctx context.Context, limit int) ([]goog.Email, error)
}

// Sources holds the optional data sources for a briefing.
type Sources struct {
	GitHub    *gh.Monitor
	Calendar  *calendar.Client // legacy CalDAV
	Google    GoogleProvider   // Google Calendar + Gmail
	Scheduler *scheduler.Scheduler
}

// Generate builds a morning briefing message from all available sources.
func Generate(ctx context.Context, src Sources) string {
	var sb strings.Builder
	sb.WriteString("*☀️ Ghost Morning Briefing*\n")
	fmt.Fprintf(&sb, "_%s_\n\n", mdv2.Esc(time.Now().Format("Monday, January 2")))

	if src.GitHub != nil {
		writeGitHub(ctx, &sb, src.GitHub)
	}
	if src.Google != nil {
		writeGoogleCalendar(ctx, &sb, src.Google)
		writeGmail(ctx, &sb, src.Google)
	} else if src.Calendar != nil {
		writeCalendar(ctx, &sb, src.Calendar)
	}
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

	fmt.Fprintf(sb, "*GitHub* — %d unread\n", total)
	for p := gh.P0; p <= gh.P4; p++ {
		if c, ok := summary[p]; ok && c > 0 {
			fmt.Fprintf(sb, "  P%d: %d %s\n", p, c, mdv2.Esc(priorityLabel(p)))
		}
	}

	urgent, _ := mon.GetByPriority(ctx, gh.P2, 5)
	if len(urgent) > 0 {
		sb.WriteString("\n")
		for _, n := range urgent {
			fmt.Fprintf(sb, "  %s `%s` — %s\n", priorityEmoji(n.Priority), mdv2.Esc(n.RepoFullName), mdv2.Esc(n.SubjectTitle))
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

	fmt.Fprintf(sb, "*Calendar* — %d events today\n", len(events))
	for _, e := range events {
		if e.AllDay {
			fmt.Fprintf(sb, "  📅 %s \\(all day\\)\n", mdv2.Esc(e.Summary))
		} else {
			fmt.Fprintf(sb, "  🕐 %s — %s\n",
				mdv2.Esc(e.StartTime.Local().Format("15:04")), mdv2.Esc(e.Summary))
		}
		if e.Location != "" {
			fmt.Fprintf(sb, "     📍 %s\n", mdv2.Esc(e.Location))
		}
	}
	sb.WriteString("\n")
}

func writeReminders(ctx context.Context, sb *strings.Builder, sched *scheduler.Scheduler) {
	reminders, err := sched.ListPending(ctx, 10)
	if err != nil || len(reminders) == 0 {
		return
	}

	fmt.Fprintf(sb, "*Reminders* — %d pending\n", len(reminders))
	for _, r := range reminders {
		dueAt, _ := time.Parse(time.RFC3339, r.DueAt)
		fmt.Fprintf(sb, "  ⏰ %s — %s\n", mdv2.Esc(dueAt.Local().Format("15:04")), mdv2.Esc(r.Message))
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

func writeGoogleCalendar(ctx context.Context, sb *strings.Builder, g GoogleProvider) {
	events, err := g.TodayEvents(ctx)
	if err != nil {
		sb.WriteString("*Calendar* — error fetching\n\n")
		return
	}

	if len(events) == 0 {
		sb.WriteString("*Calendar* — No events today 📅\n\n")
		return
	}

	fmt.Fprintf(sb, "*Calendar* — %d events today\n", len(events))
	for _, e := range events {
		if e.AllDay {
			fmt.Fprintf(sb, "  📅 %s \\(all day\\)\n", mdv2.Esc(e.Summary))
		} else {
			fmt.Fprintf(sb, "  🕐 %s — %s\n",
				mdv2.Esc(e.Start.Local().Format("15:04")), mdv2.Esc(e.Summary))
		}
		if e.Location != "" {
			fmt.Fprintf(sb, "     📍 %s\n", mdv2.Esc(e.Location))
		}
		if e.MeetLink != "" {
			// URL part of a MarkdownV2 link needs only ) and \ escaped — not all 18 chars.
			fmt.Fprintf(sb, "     🔗 [Join Meet](%s)\n", e.MeetLink)
		}
	}
	sb.WriteString("\n")
}

func writeGmail(ctx context.Context, sb *strings.Builder, g GoogleProvider) {
	count, err := g.UnreadCount(ctx)
	if err != nil {
		return // silently skip if Gmail unavailable
	}
	if count == 0 {
		sb.WriteString("*Email* — Inbox zero 📧\n\n")
		return
	}

	fmt.Fprintf(sb, "*Email* — %d unread\n", count)
	emails, err := g.RecentUnread(ctx, 5)
	if err == nil {
		for _, e := range emails {
			fmt.Fprintf(sb, "  📧 %s — %s\n", mdv2.Esc(e.From), mdv2.Esc(e.Subject))
		}
	}
	sb.WriteString("\n")
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

