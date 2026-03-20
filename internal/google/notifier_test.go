package google

import (
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

func TestFormatAlert_Basic(t *testing.T) {
	n := NewMeetingNotifier(nil, nil, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	ev := Event{
		Summary: "Team Standup",
		Start:   time.Now().Add(10 * time.Minute),
		End:     time.Now().Add(40 * time.Minute),
	}

	msg := n.formatAlert(ev, 10*time.Minute)

	if !strings.Contains(msg, "Meeting in 10 min") {
		t.Errorf("expected '10 min', got: %s", msg)
	}
	if !strings.Contains(msg, "Team Standup") {
		t.Errorf("expected event title, got: %s", msg)
	}
}

func TestFormatAlert_WithLocation(t *testing.T) {
	n := NewMeetingNotifier(nil, nil, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	ev := Event{
		Summary:  "Board Room",
		Start:    time.Now().Add(5 * time.Minute),
		End:      time.Now().Add(35 * time.Minute),
		Location: "Building A, Room 3",
	}

	msg := n.formatAlert(ev, 5*time.Minute)

	if !strings.Contains(msg, "Building A") {
		t.Errorf("expected location, got: %s", msg)
	}
	if !strings.Contains(msg, "📍") {
		t.Errorf("expected location emoji, got: %s", msg)
	}
}

func TestFormatAlert_WithMeetLink(t *testing.T) {
	n := NewMeetingNotifier(nil, nil, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	ev := Event{
		Summary:  "Remote Sync",
		Start:    time.Now().Add(5 * time.Minute),
		End:      time.Now().Add(35 * time.Minute),
		MeetLink: "https://meet.google.com/abc-defg-hij",
	}

	msg := n.formatAlert(ev, 5*time.Minute)

	if !strings.Contains(msg, "Join Meet") {
		t.Errorf("expected Meet link text, got: %s", msg)
	}
	if !strings.Contains(msg, "abc-defg-hij") {
		t.Errorf("expected Meet URL, got: %s", msg)
	}
}

func TestFormatAlert_WithHtmlLink(t *testing.T) {
	n := NewMeetingNotifier(nil, nil, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	ev := Event{
		Summary:  "Planning",
		Start:    time.Now().Add(5 * time.Minute),
		End:      time.Now().Add(35 * time.Minute),
		HtmlLink: "https://calendar.google.com/event/abc",
	}

	msg := n.formatAlert(ev, 5*time.Minute)

	// Title should be a link when HtmlLink is present.
	if !strings.Contains(msg, "[Planning](https://calendar.google.com/event/abc)") {
		t.Errorf("expected linked title, got: %s", msg)
	}
}

func TestFormatAlert_SubMinute(t *testing.T) {
	n := NewMeetingNotifier(nil, nil, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	ev := Event{
		Summary: "Now!",
		Start:   time.Now().Add(30 * time.Second),
	}

	msg := n.formatAlert(ev, 30*time.Second)

	// Should clamp to 1 minute.
	if !strings.Contains(msg, "1 min") {
		t.Errorf("expected '1 min' for sub-minute, got: %s", msg)
	}
}

func TestFormatAlert_NoEndTime(t *testing.T) {
	n := NewMeetingNotifier(nil, nil, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	ev := Event{
		Summary: "Quick Sync",
		Start:   time.Now().Add(5 * time.Minute),
		// End is zero value
	}

	msg := n.formatAlert(ev, 5*time.Minute)

	// Should not contain the " – " separator for end time.
	if strings.Contains(msg, " – ") {
		t.Errorf("should not show end time when zero, got: %s", msg)
	}
}

// --- NewMeetingNotifier ---

func TestNewMeetingNotifier(t *testing.T) {
	var alerts []string
	n := NewMeetingNotifier(nil, func(msg string) {
		alerts = append(alerts, msg)
	}, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	if n == nil {
		t.Fatal("expected non-nil notifier")
	}
	if len(n.leadTimes) != 2 {
		t.Errorf("expected 2 lead times, got %d", len(n.leadTimes))
	}
	if n.leadTimes[0] != 10*time.Minute {
		t.Errorf("first lead time = %v, want 10m", n.leadTimes[0])
	}
	if n.leadTimes[1] != 5*time.Minute {
		t.Errorf("second lead time = %v, want 5m", n.leadTimes[1])
	}
	if n.notified == nil {
		t.Error("notified map should be initialized")
	}
}

// --- cleanNotified ---

func TestCleanNotified_UnderLimit(t *testing.T) {
	n := NewMeetingNotifier(nil, nil, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	// Add some entries under the 200 limit.
	for i := 0; i < 50; i++ {
		n.notified[strings.Repeat("x", i+1)] = true
	}

	n.cleanNotified()

	// Should not clear — under 200.
	if len(n.notified) != 50 {
		t.Errorf("expected 50 entries to remain, got %d", len(n.notified))
	}
}

func TestCleanNotified_OverLimit(t *testing.T) {
	n := NewMeetingNotifier(nil, nil, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	// Fill past 200.
	for i := 0; i < 201; i++ {
		n.notified[strings.Repeat("x", i+1)] = true
	}

	n.cleanNotified()

	// Should be reset.
	if len(n.notified) != 0 {
		t.Errorf("expected empty map after cleanup, got %d", len(n.notified))
	}
}
