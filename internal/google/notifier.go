package google

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/wcatz/ghost/internal/mdv2"
)

// AlertFunc is called when a meeting notification should be sent.
type AlertFunc func(message string)

// MeetingNotifier checks for upcoming meetings and sends alerts
// at configurable lead times (default: 10min and 5min before).
type MeetingNotifier struct {
	client    *Client
	onAlert   AlertFunc
	logger    *slog.Logger
	leadTimes []time.Duration

	mu       sync.Mutex
	notified map[string]bool // "summary|startTime|leadMinutes" -> true
}

// NewMeetingNotifier creates a notifier that alerts before meetings.
func NewMeetingNotifier(client *Client, onAlert AlertFunc, logger *slog.Logger) *MeetingNotifier {
	return &MeetingNotifier{
		client:  client,
		onAlert: onAlert,
		logger:  logger,
		leadTimes: []time.Duration{
			10 * time.Minute,
			5 * time.Minute,
		},
		notified: make(map[string]bool),
	}
}

// Run starts the notifier loop. Checks every 60 seconds.
// Blocks until ctx is cancelled.
func (n *MeetingNotifier) Run(ctx context.Context) {
	n.logger.Info("meeting notifier started")
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	// Check immediately on start.
	n.check(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n.check(ctx)
		}
	}
}

func (n *MeetingNotifier) check(ctx context.Context) {
	// Look ahead 15 minutes.
	events, err := n.client.UpcomingEvents(ctx, 15)
	if err != nil {
		n.logger.Warn("meeting notifier check failed", "error", err)
		return
	}

	now := time.Now()

	for _, ev := range events {
		if ev.AllDay {
			continue
		}
		until := ev.Start.Sub(now)

		for _, lead := range n.leadTimes {
			if until > lead || until < 0 {
				continue
			}

			key := fmt.Sprintf("%s|%s|%d", ev.Summary, ev.Start.Format(time.RFC3339), int(lead.Minutes()))

			n.mu.Lock()
			already := n.notified[key]
			if !already {
				n.notified[key] = true
			}
			n.mu.Unlock()

			if already {
				continue
			}

			msg := n.formatAlert(ev, until)
			n.logger.Info("meeting alert", "summary", ev.Summary, "in", until.Round(time.Minute))
			if n.onAlert != nil {
				n.onAlert(msg)
			}
		}
	}

	// Clean old entries (older than 1 hour).
	n.cleanNotified()
}

func (n *MeetingNotifier) formatAlert(ev Event, until time.Duration) string {
	minutes := int(until.Minutes())
	if minutes < 1 {
		minutes = 1
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "📅 *Meeting in %d min*\n", minutes)
	title := mdv2.Esc(ev.Summary)
	if ev.HtmlLink != "" {
		title = fmt.Sprintf("[%s](%s)", mdv2.Esc(ev.Summary), ev.HtmlLink)
	}
	fmt.Fprintf(&sb, "  %s\n", title)
	fmt.Fprintf(&sb, "  🕐 %s", mdv2.Esc(ev.Start.Local().Format("15:04")))
	if !ev.End.IsZero() {
		fmt.Fprintf(&sb, " – %s", mdv2.Esc(ev.End.Local().Format("15:04")))
	}
	sb.WriteString("\n")
	if ev.Location != "" {
		fmt.Fprintf(&sb, "  📍 %s\n", mdv2.Esc(ev.Location))
	}
	if ev.MeetLink != "" {
		fmt.Fprintf(&sb, "  🔗 [Join Meet](%s)\n", mdv2.Esc(ev.MeetLink))
	}
	return sb.String()
}


func (n *MeetingNotifier) cleanNotified() {
	n.mu.Lock()
	defer n.mu.Unlock()

	// Simple cleanup: if map gets large, reset it.
	// Events from >1hr ago won't match again anyway.
	if len(n.notified) > 200 {
		n.notified = make(map[string]bool)
	}
}
