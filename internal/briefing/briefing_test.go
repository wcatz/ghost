package briefing

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	goog "github.com/wcatz/ghost/internal/google"
	"github.com/wcatz/ghost/internal/scheduler"

	_ "modernc.org/sqlite"
)

// --- Mock Google Provider ---

type mockGoogle struct {
	events      []goog.Event
	eventsErr   error
	unreadCount int
	unreadErr   error
	emails      []goog.Email
	emailsErr   error
}

func (m *mockGoogle) TodayEvents(_ context.Context) ([]goog.Event, error) {
	return m.events, m.eventsErr
}

func (m *mockGoogle) UnreadCount(_ context.Context) (int, error) {
	return m.unreadCount, m.unreadErr
}

func (m *mockGoogle) RecentUnread(_ context.Context, _ int) ([]goog.Email, error) {
	return m.emails, m.emailsErr
}

// testSchedulerDB creates an in-memory SQLite database with scheduler tables.
func testSchedulerDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:?_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS scheduled_jobs (
			id TEXT PRIMARY KEY, name TEXT NOT NULL, schedule TEXT NOT NULL,
			payload TEXT DEFAULT '{}', enabled INTEGER NOT NULL DEFAULT 1,
			last_run TEXT, next_run TEXT, created_at TEXT NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE IF NOT EXISTS reminders (
			id TEXT PRIMARY KEY, message TEXT NOT NULL, due_at TEXT NOT NULL,
			fired INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL DEFAULT (datetime('now'))
		);
		CREATE INDEX IF NOT EXISTS idx_reminders_due ON reminders(fired, due_at);
	`)
	if err != nil {
		t.Fatalf("create tables: %v", err)
	}
	return db
}

func testScheduler(t *testing.T) *scheduler.Scheduler {
	t.Helper()
	db := testSchedulerDB(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s, err := scheduler.New(db, logger)
	if err != nil {
		t.Fatalf("New scheduler: %v", err)
	}
	return s
}

// --- Tests ---

func TestGenerate_AllNilSources(t *testing.T) {
	result := Generate(context.Background(), Sources{})
	if !strings.Contains(result, "Morning Briefing") {
		t.Errorf("expected header in briefing, got: %s", result)
	}
	if strings.Contains(result, "GitHub") {
		t.Error("should not contain GitHub section when source is nil")
	}
	if strings.Contains(result, "Calendar") {
		t.Error("should not contain Calendar section when source is nil")
	}
}

func TestGenerate_GoogleCalendarEmpty(t *testing.T) {
	g := &mockGoogle{events: nil}
	result := Generate(context.Background(), Sources{Google: g})
	if !strings.Contains(result, "No events today") {
		t.Errorf("expected 'No events today', got: %s", result)
	}
}

func TestGenerate_GoogleCalendarWithEvents(t *testing.T) {
	now := time.Now()
	g := &mockGoogle{
		events: []goog.Event{
			{Summary: "Team Standup", Start: now, End: now.Add(30 * time.Minute)},
			{Summary: "All Hands", Start: now.Add(2 * time.Hour), End: now.Add(3 * time.Hour), AllDay: true},
		},
	}

	result := Generate(context.Background(), Sources{Google: g})
	if !strings.Contains(result, "2 events today") {
		t.Errorf("expected '2 events today', got: %s", result)
	}
	if !strings.Contains(result, "Team Standup") {
		t.Errorf("expected 'Team Standup' in result, got: %s", result)
	}
	if !strings.Contains(result, "all day") {
		t.Errorf("expected 'all day' for all-day event, got: %s", result)
	}
}

func TestGenerate_GoogleCalendarError(t *testing.T) {
	g := &mockGoogle{eventsErr: fmt.Errorf("oauth expired")}
	result := Generate(context.Background(), Sources{Google: g})
	if !strings.Contains(result, "error fetching") {
		t.Errorf("expected 'error fetching', got: %s", result)
	}
}

func TestGenerate_GoogleCalendarWithLocation(t *testing.T) {
	now := time.Now()
	g := &mockGoogle{
		events: []goog.Event{
			{Summary: "Office Meeting", Start: now, End: now.Add(1 * time.Hour), Location: "Room 42"},
		},
	}
	result := Generate(context.Background(), Sources{Google: g})
	if !strings.Contains(result, "Room 42") {
		t.Errorf("expected location in result, got: %s", result)
	}
}

func TestGenerate_GoogleCalendarWithMeetLink(t *testing.T) {
	now := time.Now()
	g := &mockGoogle{
		events: []goog.Event{
			{Summary: "Remote Sync", Start: now, End: now.Add(1 * time.Hour), MeetLink: "https://meet.google.com/abc-defg-hij"},
		},
	}
	result := Generate(context.Background(), Sources{Google: g})
	if !strings.Contains(result, "Join Meet") {
		t.Errorf("expected Meet link in result, got: %s", result)
	}
}

func TestGenerate_GmailInboxZero(t *testing.T) {
	g := &mockGoogle{unreadCount: 0}
	result := Generate(context.Background(), Sources{Google: g})
	if !strings.Contains(result, "Inbox zero") {
		t.Errorf("expected 'Inbox zero', got: %s", result)
	}
}

func TestGenerate_GmailWithUnread(t *testing.T) {
	g := &mockGoogle{
		unreadCount: 3,
		emails: []goog.Email{
			{From: "alice@test.com", Subject: "PR review needed"},
			{From: "bob@test.com", Subject: "Deploy notification"},
		},
	}
	result := Generate(context.Background(), Sources{Google: g})
	if !strings.Contains(result, "3 unread") {
		t.Errorf("expected '3 unread', got: %s", result)
	}
	if !strings.Contains(result, "PR review needed") {
		t.Errorf("expected email subject, got: %s", result)
	}
}

func TestGenerate_GmailError(t *testing.T) {
	g := &mockGoogle{unreadErr: fmt.Errorf("token expired")}
	result := Generate(context.Background(), Sources{Google: g})
	// Gmail errors are silently skipped.
	if strings.Contains(result, "error") {
		t.Errorf("gmail error should be silent, got: %s", result)
	}
}

func TestGenerate_WithReminders(t *testing.T) {
	sched := testScheduler(t)
	ctx := context.Background()

	// Add a pending reminder.
	_, err := sched.AddReminder(ctx, "deploy frontend in 2 hours")
	if err != nil {
		t.Fatalf("AddReminder: %v", err)
	}

	result := Generate(ctx, Sources{Scheduler: sched})
	if !strings.Contains(result, "Reminders") {
		t.Errorf("expected Reminders section, got: %s", result)
	}
	if !strings.Contains(result, "1 pending") {
		t.Errorf("expected '1 pending', got: %s", result)
	}
}

func TestGenerate_WithNoReminders(t *testing.T) {
	sched := testScheduler(t)
	result := Generate(context.Background(), Sources{Scheduler: sched})
	// No reminders should not produce a Reminders section.
	if strings.Contains(result, "Reminders") {
		t.Errorf("should not contain Reminders when none exist, got: %s", result)
	}
}

func TestPriorityLabel(t *testing.T) {
	tests := []struct {
		priority int
		want     string
	}{
		{0, "(security)"},
		{1, "(review/CI)"},
		{2, "(mention/assign)"},
		{3, "(subscribed)"},
		{4, ""},
		{99, ""},
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("P%d", tc.priority), func(t *testing.T) {
			got := priorityLabel(tc.priority)
			if got != tc.want {
				t.Errorf("priorityLabel(%d) = %q, want %q", tc.priority, got, tc.want)
			}
		})
	}
}

func TestPriorityEmoji(t *testing.T) {
	tests := []struct {
		priority int
		want     string
	}{
		{0, "\U0001f534"},
		{1, "\U0001f7e0"},
		{2, "\U0001f7e1"},
		{3, "\u26aa"},
		{4, "\u26aa"},
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("P%d", tc.priority), func(t *testing.T) {
			got := priorityEmoji(tc.priority)
			if got != tc.want {
				t.Errorf("priorityEmoji(%d) = %q, want %q", tc.priority, got, tc.want)
			}
		})
	}
}

func TestGenerate_CombinedSources(t *testing.T) {
	now := time.Now()
	sched := testScheduler(t)
	ctx := context.Background()

	sched.AddReminder(ctx, "check CI in 1 hour")

	g := &mockGoogle{
		events: []goog.Event{
			{Summary: "Planning", Start: now, End: now.Add(1 * time.Hour)},
		},
		unreadCount: 2,
		emails: []goog.Email{
			{From: "team@co.com", Subject: "Weekly update"},
		},
	}

	result := Generate(ctx, Sources{
		Google:    g,
		Scheduler: sched,
	})

	if !strings.Contains(result, "Morning Briefing") {
		t.Error("missing header")
	}
	if !strings.Contains(result, "Calendar") {
		t.Error("missing Calendar section")
	}
	if !strings.Contains(result, "Email") {
		t.Error("missing Email section")
	}
	if !strings.Contains(result, "Reminders") {
		t.Error("missing Reminders section")
	}
}
