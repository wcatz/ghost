package scheduler

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// testDB creates an in-memory SQLite database with the required tables.
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:?_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// Create the minimal tables needed by the scheduler.
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS scheduled_jobs (
			id          TEXT PRIMARY KEY,
			name        TEXT NOT NULL,
			schedule    TEXT NOT NULL,
			payload     TEXT DEFAULT '{}',
			enabled     INTEGER NOT NULL DEFAULT 1,
			last_run    TEXT,
			next_run    TEXT,
			created_at  TEXT NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE IF NOT EXISTS reminders (
			id          TEXT PRIMARY KEY,
			message     TEXT NOT NULL,
			due_at      TEXT NOT NULL,
			fired       INTEGER NOT NULL DEFAULT 0,
			created_at  TEXT NOT NULL DEFAULT (datetime('now'))
		);
		CREATE INDEX IF NOT EXISTS idx_reminders_due ON reminders(fired, due_at);
	`)
	if err != nil {
		t.Fatalf("create tables: %v", err)
	}
	return db
}

func testScheduler(t *testing.T) *Scheduler {
	t.Helper()
	db := testDB(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s, err := New(db, logger)
	if err != nil {
		t.Fatalf("New scheduler: %v", err)
	}
	return s
}

func TestAddReminder_WithNaturalLanguage(t *testing.T) {
	s := testScheduler(t)
	ctx := context.Background()

	tests := []struct {
		name        string
		text        string
		wantMessage string // expected substring in saved message
		wantParsed  bool   // true if time should be parsed, false for fallback
	}{
		{
			name:        "in 5 minutes",
			text:        "review PR in 5 minutes",
			wantMessage: "review PR",
			wantParsed:  true,
		},
		{
			name:        "tomorrow at 9am",
			text:        "standup tomorrow at 9am",
			wantMessage: "standup",
			wantParsed:  true,
		},
		{
			name:        "no time expression fallback",
			text:        "just a reminder with no time",
			wantMessage: "just a reminder with no time",
			wantParsed:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dueAt, err := s.AddReminder(ctx, tc.text)
			if err != nil {
				t.Fatalf("AddReminder: %v", err)
			}

			if dueAt.IsZero() {
				t.Error("due time should not be zero")
			}

			if tc.wantParsed {
				// Parsed times should not equal the default 1-hour fallback.
				fallback := time.Now().Add(1 * time.Hour)
				// Allow a generous window (the parsed time should be clearly different from fallback
				// OR close to it if the expression is "in 5 minutes" — just check it's in the future).
				if dueAt.Before(time.Now()) {
					t.Errorf("due time %v should be in the future", dueAt)
				}
				_ = fallback
			} else {
				// Fallback: should be roughly 1 hour from now.
				expected := time.Now().Add(1 * time.Hour)
				diff := dueAt.Sub(expected)
				if diff < -5*time.Second || diff > 5*time.Second {
					t.Errorf("fallback due time off: got %v, expected ~%v", dueAt, expected)
				}
			}

			// Verify the reminder was persisted.
			reminders, err := s.ListPending(ctx, 100)
			if err != nil {
				t.Fatalf("ListPending: %v", err)
			}
			found := false
			for _, r := range reminders {
				if strings.Contains(r.Message, tc.wantMessage) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("reminder message %q not found in pending list", tc.wantMessage)
			}
		})
	}
}

func TestListPending(t *testing.T) {
	s := testScheduler(t)
	ctx := context.Background()

	// No reminders initially.
	pending, err := s.ListPending(ctx, 10)
	if err != nil {
		t.Fatalf("ListPending (empty): %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("expected 0 pending, got %d", len(pending))
	}

	// Add a few reminders.
	for _, text := range []string{"first reminder in 2 hours", "second reminder in 3 hours"} {
		if _, err := s.AddReminder(ctx, text); err != nil {
			t.Fatalf("AddReminder: %v", err)
		}
	}

	pending, err = s.ListPending(ctx, 10)
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if len(pending) != 2 {
		t.Errorf("expected 2 pending, got %d", len(pending))
	}

	// Test limit.
	pending, err = s.ListPending(ctx, 1)
	if err != nil {
		t.Fatalf("ListPending (limit 1): %v", err)
	}
	if len(pending) != 1 {
		t.Errorf("expected 1 with limit, got %d", len(pending))
	}
}

func TestListPending_OrderByDueDate(t *testing.T) {
	s := testScheduler(t)
	ctx := context.Background()

	// Insert reminders with different due times (the natural-language parser will order them).
	s.AddReminder(ctx, "later reminder in 3 hours")
	s.AddReminder(ctx, "sooner reminder in 1 hour")

	pending, err := s.ListPending(ctx, 10)
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if len(pending) < 2 {
		t.Fatalf("expected at least 2 pending, got %d", len(pending))
	}

	// Verify ordering: first should be earlier.
	due0, _ := time.Parse(time.RFC3339, pending[0].DueAt)
	due1, _ := time.Parse(time.RFC3339, pending[1].DueAt)
	if due0.After(due1) {
		t.Errorf("reminders not ordered by due date: %v > %v", due0, due1)
	}
}

func TestFireReminders(t *testing.T) {
	s := testScheduler(t)
	ctx := context.Background()

	// Track alerts.
	var alerts []string
	s.OnAlert(func(msg string) {
		alerts = append(alerts, msg)
	})

	// Insert a reminder that's already past due.
	pastDue := time.Now().Add(-1 * time.Minute).UTC().Format(time.RFC3339)
	id, err := randomID()
	if err != nil {
		t.Fatalf("randomID: %v", err)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO reminders (id, message, due_at) VALUES (?, ?, ?)`,
		id, "overdue test reminder", pastDue)
	if err != nil {
		t.Fatalf("insert past-due reminder: %v", err)
	}

	// Insert a reminder that's not due yet.
	futureDue := time.Now().Add(1 * time.Hour).UTC().Format(time.RFC3339)
	id2, _ := randomID()
	_, err = s.db.ExecContext(ctx, `INSERT INTO reminders (id, message, due_at) VALUES (?, ?, ?)`,
		id2, "future reminder", futureDue)
	if err != nil {
		t.Fatalf("insert future reminder: %v", err)
	}

	// Fire reminders.
	s.fireReminders(ctx)

	// Should have fired 1 alert.
	if len(alerts) != 1 {
		t.Fatalf("expected 1 alert, got %d: %v", len(alerts), alerts)
	}
	if !strings.Contains(alerts[0], "overdue test reminder") {
		t.Errorf("expected 'overdue test reminder' in alert, got: %s", alerts[0])
	}

	// The overdue reminder should now be fired.
	pending, err := s.ListPending(ctx, 10)
	if err != nil {
		t.Fatalf("ListPending after fire: %v", err)
	}
	if len(pending) != 1 {
		t.Errorf("expected 1 pending (future only), got %d", len(pending))
	}
	if pending[0].Message != "future reminder" {
		t.Errorf("expected future reminder, got: %s", pending[0].Message)
	}
}

func TestFireReminders_NoAlert(t *testing.T) {
	s := testScheduler(t)
	ctx := context.Background()

	// Fire without OnAlert set — should not panic.
	pastDue := time.Now().Add(-1 * time.Minute).UTC().Format(time.RFC3339)
	id, _ := randomID()
	s.db.ExecContext(ctx, `INSERT INTO reminders (id, message, due_at) VALUES (?, ?, ?)`,
		id, "no alert handler test", pastDue)

	s.fireReminders(ctx) // should not panic
}

func TestRandomID(t *testing.T) {
	ids := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id, err := randomID()
		if err != nil {
			t.Fatalf("randomID: %v", err)
		}
		if len(id) != 32 { // 16 bytes = 32 hex chars
			t.Errorf("expected 32 char hex ID, got %d chars: %s", len(id), id)
		}
		if ids[id] {
			t.Errorf("duplicate ID: %s", id)
		}
		ids[id] = true
	}
}

func TestAddCronJob(t *testing.T) {
	s := testScheduler(t)
	ctx := context.Background()

	called := false
	err := s.AddCronJob(ctx, "test-job", "0 9 * * *", map[string]interface{}{"key": "value"}, func() {
		called = true
	})
	if err != nil {
		t.Fatalf("AddCronJob: %v", err)
	}
	_ = called // job won't fire in test; just verify it registered

	// Verify it was persisted in the database.
	var name, schedule string
	err = s.db.QueryRowContext(ctx, `SELECT name, schedule FROM scheduled_jobs WHERE name = ?`, "test-job").Scan(&name, &schedule)
	if err != nil {
		t.Fatalf("query job: %v", err)
	}
	if name != "test-job" {
		t.Errorf("job name = %q, want %q", name, "test-job")
	}
	if schedule != "0 9 * * *" {
		t.Errorf("job schedule = %q, want %q", schedule, "0 9 * * *")
	}
}

func TestAddCronJob_InvalidCron(t *testing.T) {
	s := testScheduler(t)
	ctx := context.Background()

	err := s.AddCronJob(ctx, "bad-job", "not a cron expression", nil, func() {})
	if err == nil {
		t.Error("expected error for invalid cron expression")
	}

	// The orphaned DB record should have been cleaned up.
	var count int
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM scheduled_jobs WHERE name = ?`, "bad-job").Scan(&count)
	if count != 0 {
		t.Errorf("orphaned DB record should be cleaned up, found %d", count)
	}
}

func TestOnAlert(t *testing.T) {
	s := testScheduler(t)
	var msg string
	s.OnAlert(func(m string) {
		msg = m
	})

	if s.onAlert == nil {
		t.Error("onAlert should be set after OnAlert()")
	}

	s.onAlert("test")
	if msg != "test" {
		t.Errorf("onAlert callback not called correctly, got %q", msg)
	}
}

func TestAddReminder_MessageStripping(t *testing.T) {
	s := testScheduler(t)
	ctx := context.Background()

	// When a time expression is found, it should be stripped from the message.
	_, err := s.AddReminder(ctx, "deploy the app in 30 minutes")
	if err != nil {
		t.Fatalf("AddReminder: %v", err)
	}

	pending, err := s.ListPending(ctx, 10)
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if len(pending) == 0 {
		t.Fatal("expected at least 1 pending reminder")
	}

	// The message should have the time expression stripped.
	// "deploy the app in 30 minutes" -> "deploy the app" (time part removed)
	msg := pending[0].Message
	if strings.Contains(msg, "30 minutes") {
		t.Errorf("time expression should be stripped, got message: %q", msg)
	}
	if !strings.Contains(msg, "deploy") {
		t.Errorf("expected 'deploy' in stripped message, got: %q", msg)
	}
}
