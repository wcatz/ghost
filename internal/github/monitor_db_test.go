package github

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// testDB creates an in-memory SQLite with the notifications table.
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:?_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS notifications (
			id              TEXT PRIMARY KEY,
			github_id       TEXT NOT NULL UNIQUE,
			repo_full_name  TEXT NOT NULL,
			subject_title   TEXT NOT NULL,
			subject_type    TEXT NOT NULL,
			subject_url     TEXT DEFAULT '',
			reason          TEXT NOT NULL,
			priority        INTEGER NOT NULL DEFAULT 4,
			unread          INTEGER NOT NULL DEFAULT 1,
			updated_at      TEXT NOT NULL,
			created_at      TEXT NOT NULL DEFAULT (datetime('now')),
			dismissed_at    TEXT
		);
		CREATE INDEX IF NOT EXISTS idx_notif_priority ON notifications(priority, updated_at DESC);
		CREATE INDEX IF NOT EXISTS idx_notif_unread ON notifications(unread, priority);
	`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	return db
}

func testMonitor(t *testing.T) *Monitor {
	t.Helper()
	db := testDB(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return &Monitor{
		db:     db,
		logger: logger,
	}
}

// --- WebURL ---

func TestWebURL(t *testing.T) {
	tests := []struct {
		name         string
		subjectURL   string
		repoFullName string
		want         string
	}{
		{
			name:         "pull request",
			subjectURL:   "https://api.github.com/repos/owner/repo/pulls/123",
			repoFullName: "owner/repo",
			want:         "https://github.com/owner/repo/pull/123",
		},
		{
			name:         "issue",
			subjectURL:   "https://api.github.com/repos/owner/repo/issues/456",
			repoFullName: "owner/repo",
			want:         "https://github.com/owner/repo/issues/456",
		},
		{
			name:         "commit",
			subjectURL:   "https://api.github.com/repos/owner/repo/commits/abc123",
			repoFullName: "owner/repo",
			want:         "https://github.com/owner/repo/commit/abc123",
		},
		{
			name:         "empty URL falls back to repo",
			subjectURL:   "",
			repoFullName: "owner/repo",
			want:         "https://github.com/owner/repo",
		},
		{
			name:         "non-API URL passes through",
			subjectURL:   "https://example.com/some/path",
			repoFullName: "owner/repo",
			want:         "https://example.com/some/path",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := Notification{SubjectURL: tt.subjectURL, RepoFullName: tt.repoFullName}
			got := n.WebURL()
			if got != tt.want {
				t.Errorf("WebURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

// --- Upsert ---

func TestUpsert_Insert(t *testing.T) {
	m := testMonitor(t)
	ctx := context.Background()

	n := Notification{
		GitHubID:     "gh-001",
		RepoFullName: "wcatz/ghost",
		SubjectTitle: "Fix memory leak",
		SubjectType:  "PullRequest",
		SubjectURL:   "https://api.github.com/repos/wcatz/ghost/pulls/42",
		Reason:       "review_requested",
		Priority:     P1,
		Unread:       true,
		UpdatedAt:    time.Now().UTC().Format(time.RFC3339),
	}

	inserted, err := m.upsert(ctx, n)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if !inserted {
		t.Error("expected inserted=true for new notification")
	}

	// Verify it was stored.
	var title string
	err = m.db.QueryRowContext(ctx, `SELECT subject_title FROM notifications WHERE github_id = ?`, "gh-001").Scan(&title)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if title != "Fix memory leak" {
		t.Errorf("title = %q, want %q", title, "Fix memory leak")
	}
}

func TestUpsert_Update(t *testing.T) {
	m := testMonitor(t)
	ctx := context.Background()

	n := Notification{
		GitHubID:     "gh-002",
		RepoFullName: "wcatz/ghost",
		SubjectTitle: "Original title",
		SubjectType:  "Issue",
		Reason:       "mention",
		Priority:     P2,
		Unread:       true,
		UpdatedAt:    time.Now().UTC().Format(time.RFC3339),
	}

	// First insert.
	inserted, err := m.upsert(ctx, n)
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if !inserted {
		t.Error("first upsert should be an insert")
	}

	// Second upsert with updated title.
	n.SubjectTitle = "Updated title"
	n.Priority = P3

	inserted, err = m.upsert(ctx, n)
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if inserted {
		t.Error("second upsert should be an update, not an insert")
	}

	// Verify update.
	var title string
	var priority int
	err = m.db.QueryRowContext(ctx,
		`SELECT subject_title, priority FROM notifications WHERE github_id = ?`, "gh-002").
		Scan(&title, &priority)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if title != "Updated title" {
		t.Errorf("title = %q, want %q", title, "Updated title")
	}
	if priority != P3 {
		t.Errorf("priority = %d, want %d", priority, P3)
	}
}

// --- GetUnread ---

func TestGetUnread(t *testing.T) {
	m := testMonitor(t)
	ctx := context.Background()

	// Insert notifications with different priorities.
	now := time.Now().UTC().Format(time.RFC3339)
	for _, n := range []Notification{
		{GitHubID: "gh-10", RepoFullName: "wcatz/ghost", SubjectTitle: "Security alert", SubjectType: "Issue", Reason: "security_alert", Priority: P0, Unread: true, UpdatedAt: now},
		{GitHubID: "gh-11", RepoFullName: "wcatz/ghost", SubjectTitle: "Review PR", SubjectType: "PullRequest", Reason: "review_requested", Priority: P1, Unread: true, UpdatedAt: now},
		{GitHubID: "gh-12", RepoFullName: "wcatz/ghost", SubjectTitle: "Mentioned", SubjectType: "Issue", Reason: "mention", Priority: P2, Unread: true, UpdatedAt: now},
		{GitHubID: "gh-13", RepoFullName: "wcatz/ghost", SubjectTitle: "Read item", SubjectType: "Issue", Reason: "subscribed", Priority: P3, Unread: false, UpdatedAt: now},
	} {
		if _, err := m.upsert(ctx, n); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}

	unread, err := m.GetUnread(ctx, 10)
	if err != nil {
		t.Fatalf("GetUnread: %v", err)
	}

	// Should only include unread items (3 of 4).
	if len(unread) != 3 {
		t.Fatalf("expected 3 unread, got %d", len(unread))
	}

	// Should be ordered by priority ASC.
	if unread[0].Priority != P0 {
		t.Errorf("first unread should be P0, got P%d", unread[0].Priority)
	}
	if unread[1].Priority != P1 {
		t.Errorf("second unread should be P1, got P%d", unread[1].Priority)
	}
	if unread[2].Priority != P2 {
		t.Errorf("third unread should be P2, got P%d", unread[2].Priority)
	}
}

func TestGetUnread_Limit(t *testing.T) {
	m := testMonitor(t)
	ctx := context.Background()

	now := time.Now().UTC().Format(time.RFC3339)
	for i := 0; i < 5; i++ {
		n := Notification{
			GitHubID:     "gh-limit-" + string(rune('a'+i)),
			RepoFullName: "wcatz/ghost",
			SubjectTitle: "Notification",
			SubjectType:  "Issue",
			Reason:       "mention",
			Priority:     P2,
			Unread:       true,
			UpdatedAt:    now,
		}
		m.upsert(ctx, n)
	}

	unread, err := m.GetUnread(ctx, 2)
	if err != nil {
		t.Fatalf("GetUnread: %v", err)
	}
	if len(unread) != 2 {
		t.Errorf("expected 2 with limit, got %d", len(unread))
	}
}

// --- GetByPriority ---

func TestGetByPriority(t *testing.T) {
	m := testMonitor(t)
	ctx := context.Background()

	now := time.Now().UTC().Format(time.RFC3339)
	for _, n := range []Notification{
		{GitHubID: "gh-20", RepoFullName: "r", SubjectTitle: "P0 alert", SubjectType: "Issue", Reason: "security_alert", Priority: P0, Unread: true, UpdatedAt: now},
		{GitHubID: "gh-21", RepoFullName: "r", SubjectTitle: "P1 review", SubjectType: "PR", Reason: "review_requested", Priority: P1, Unread: true, UpdatedAt: now},
		{GitHubID: "gh-22", RepoFullName: "r", SubjectTitle: "P2 mention", SubjectType: "Issue", Reason: "mention", Priority: P2, Unread: true, UpdatedAt: now},
		{GitHubID: "gh-23", RepoFullName: "r", SubjectTitle: "P3 sub", SubjectType: "Issue", Reason: "subscribed", Priority: P3, Unread: true, UpdatedAt: now},
		{GitHubID: "gh-24", RepoFullName: "r", SubjectTitle: "P4 other", SubjectType: "Issue", Reason: "state_change", Priority: P4, Unread: true, UpdatedAt: now},
	} {
		m.upsert(ctx, n)
	}

	// Get P0+P1 only.
	results, err := m.GetByPriority(ctx, P1, 10)
	if err != nil {
		t.Fatalf("GetByPriority: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 (P0+P1), got %d", len(results))
	}

	// Get P0 through P2.
	results, err = m.GetByPriority(ctx, P2, 10)
	if err != nil {
		t.Fatalf("GetByPriority: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 (P0+P1+P2), got %d", len(results))
	}
}

// --- Dismiss ---

func TestDismiss(t *testing.T) {
	m := testMonitor(t)
	ctx := context.Background()

	n := Notification{
		GitHubID:     "gh-30",
		RepoFullName: "wcatz/ghost",
		SubjectTitle: "Dismiss me",
		SubjectType:  "Issue",
		Reason:       "mention",
		Priority:     P2,
		Unread:       true,
		UpdatedAt:    time.Now().UTC().Format(time.RFC3339),
	}
	m.upsert(ctx, n)

	// Get the internal ID.
	var id string
	m.db.QueryRowContext(ctx, `SELECT id FROM notifications WHERE github_id = ?`, "gh-30").Scan(&id)

	// Dismiss it.
	if err := m.Dismiss(ctx, id); err != nil {
		t.Fatalf("Dismiss: %v", err)
	}

	// Should no longer appear in unread.
	unread, err := m.GetUnread(ctx, 10)
	if err != nil {
		t.Fatalf("GetUnread: %v", err)
	}
	if len(unread) != 0 {
		t.Errorf("expected 0 unread after dismiss, got %d", len(unread))
	}

	// dismissed_at should be set.
	var dismissedAt sql.NullString
	m.db.QueryRowContext(ctx, `SELECT dismissed_at FROM notifications WHERE id = ?`, id).Scan(&dismissedAt)
	if !dismissedAt.Valid {
		t.Error("dismissed_at should be set after dismiss")
	}
}

// --- Summary ---

func TestSummary(t *testing.T) {
	m := testMonitor(t)
	ctx := context.Background()

	now := time.Now().UTC().Format(time.RFC3339)
	for _, n := range []Notification{
		{GitHubID: "gh-40", RepoFullName: "r", SubjectTitle: "a", SubjectType: "I", Reason: "security_alert", Priority: P0, Unread: true, UpdatedAt: now},
		{GitHubID: "gh-41", RepoFullName: "r", SubjectTitle: "b", SubjectType: "I", Reason: "review_requested", Priority: P1, Unread: true, UpdatedAt: now},
		{GitHubID: "gh-42", RepoFullName: "r", SubjectTitle: "c", SubjectType: "I", Reason: "review_requested", Priority: P1, Unread: true, UpdatedAt: now},
		{GitHubID: "gh-43", RepoFullName: "r", SubjectTitle: "d", SubjectType: "I", Reason: "mention", Priority: P2, Unread: false, UpdatedAt: now}, // read
	} {
		m.upsert(ctx, n)
	}

	summary, err := m.Summary(ctx)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}

	if summary[P0] != 1 {
		t.Errorf("P0 count = %d, want 1", summary[P0])
	}
	if summary[P1] != 2 {
		t.Errorf("P1 count = %d, want 2", summary[P1])
	}
	if _, ok := summary[P2]; ok {
		t.Error("P2 should not appear in summary (read notification)")
	}
}

func TestSummary_Empty(t *testing.T) {
	m := testMonitor(t)
	summary, err := m.Summary(context.Background())
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if len(summary) != 0 {
		t.Errorf("expected empty summary, got %v", summary)
	}
}

// --- randomID ---

func TestRandomID(t *testing.T) {
	ids := make(map[string]bool)
	for i := 0; i < 50; i++ {
		id := randomID()
		if len(id) != 32 {
			t.Errorf("expected 32 char hex ID, got %d: %s", len(id), id)
		}
		if ids[id] {
			t.Errorf("duplicate ID: %s", id)
		}
		ids[id] = true
	}
}

// --- toNotification ---

func TestToNotification(t *testing.T) {
	now := time.Now()
	ghID := "12345"
	repoName := "wcatz/ghost"
	title := "Fix: memory leak in reflection"
	subjectType := "PullRequest"
	subjectURL := "https://api.github.com/repos/wcatz/ghost/pulls/42"
	reason := "review_requested"
	unread := true

	// Build a go-github Notification struct.
	ghNotif := buildGHNotification(ghID, repoName, title, subjectType, subjectURL, reason, unread, now)

	n := toNotification(ghNotif)

	if n.GitHubID != ghID {
		t.Errorf("GitHubID = %q, want %q", n.GitHubID, ghID)
	}
	if n.RepoFullName != repoName {
		t.Errorf("RepoFullName = %q, want %q", n.RepoFullName, repoName)
	}
	if n.SubjectTitle != title {
		t.Errorf("SubjectTitle = %q, want %q", n.SubjectTitle, title)
	}
	if n.SubjectType != subjectType {
		t.Errorf("SubjectType = %q, want %q", n.SubjectType, subjectType)
	}
	if n.SubjectURL != subjectURL {
		t.Errorf("SubjectURL = %q, want %q", n.SubjectURL, subjectURL)
	}
	if n.Reason != reason {
		t.Errorf("Reason = %q, want %q", n.Reason, reason)
	}
	if n.Unread != unread {
		t.Errorf("Unread = %v, want %v", n.Unread, unread)
	}
	if n.Priority != P1 {
		t.Errorf("Priority = %d, want %d (review_requested=P1)", n.Priority, P1)
	}
}

// --- OnAlert ---

func TestOnAlert(t *testing.T) {
	m := testMonitor(t)
	var called bool
	m.OnAlert(func(_ Notification) {
		called = true
	})
	if m.onAlert == nil {
		t.Error("onAlert should be set")
	}
	m.onAlert(Notification{})
	if !called {
		t.Error("onAlert callback not called")
	}
}
