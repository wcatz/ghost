// Package github polls GitHub notifications and scores them by priority.
package github

import (
	"context"
	"database/sql"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"strings"
	"time"

	gh "github.com/google/go-github/v68/github"
)

// Priority levels for notifications.
const (
	P0 = 0 // security_alert, vulnerability
	P1 = 1 // review_requested, ci_activity on own PRs
	P2 = 2 // mention, assign, direct comment
	P3 = 3 // subscribed, team_mention
	P4 = 4 // everything else
)

// Notification is a scored GitHub notification stored in SQLite.
type Notification struct {
	ID            string `json:"id"`
	GitHubID      string `json:"github_id"`
	RepoFullName  string `json:"repo_full_name"`
	SubjectTitle  string `json:"subject_title"`
	SubjectType   string `json:"subject_type"`
	SubjectURL    string `json:"subject_url"`
	Reason        string `json:"reason"`
	Priority      int    `json:"priority"`
	Unread        bool   `json:"unread"`
	UpdatedAt     string `json:"updated_at"`
	CreatedAt     string `json:"created_at"`
	DismissedAt   string `json:"dismissed_at,omitempty"`
}

// Monitor polls GitHub notifications on an interval.
type Monitor struct {
	client       *gh.Client
	db           *sql.DB
	logger       *slog.Logger
	interval     time.Duration
	lastModified time.Time
	onAlert      func(Notification) // called for P0/P1 notifications
}

// NewMonitor creates a GitHub notification monitor.
// token is a GitHub personal access token with notifications scope.
func NewMonitor(token string, db *sql.DB, logger *slog.Logger, interval time.Duration) *Monitor {
	client := gh.NewClient(nil).WithAuthToken(token)
	return &Monitor{
		client:   client,
		db:       db,
		logger:   logger,
		interval: interval,
	}
}

// OnAlert registers a callback for high-priority notifications (P0/P1).
func (m *Monitor) OnAlert(fn func(Notification)) {
	m.onAlert = fn
}

// Run starts the polling loop. Blocks until ctx is cancelled.
func (m *Monitor) Run(ctx context.Context) {
	// Initial poll immediately.
	m.poll(ctx)

	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.poll(ctx)
		}
	}
}

func (m *Monitor) poll(ctx context.Context) {
	opts := &gh.NotificationListOptions{
		All: false, // only unread
	}
	if !m.lastModified.IsZero() {
		opts.Since = m.lastModified
	}

	notifications, resp, err := m.client.Activity.ListNotifications(ctx, opts)
	if err != nil {
		m.logger.Error("github poll", "error", err)
		return
	}

	// 304 Not Modified — nothing new, and it was free.
	if resp.StatusCode == 304 {
		return
	}

	if lm := resp.Header.Get("Last-Modified"); lm != "" {
		if t, err := time.Parse(time.RFC1123, lm); err == nil {
			m.lastModified = t
		}
	}

	if len(notifications) == 0 {
		return
	}

	m.logger.Info("github notifications", "count", len(notifications))

	newAlerts := 0
	for _, n := range notifications {
		notif := toNotification(n)

		inserted, err := m.upsert(ctx, notif)
		if err != nil {
			m.logger.Error("save notification", "error", err, "github_id", notif.GitHubID)
			continue
		}

		if inserted && notif.Priority <= P1 && m.onAlert != nil {
			m.onAlert(notif)
			newAlerts++
		}
	}

	if newAlerts > 0 {
		m.logger.Info("new high-priority alerts", "count", newAlerts)
	}
}

func (m *Monitor) upsert(ctx context.Context, n Notification) (inserted bool, err error) {
	// Check if we already have this notification.
	var existing string
	err = m.db.QueryRowContext(ctx, `SELECT id FROM notifications WHERE github_id = ?`, n.GitHubID).Scan(&existing)
	if err == nil {
		// Already exists — update.
		_, err = m.db.ExecContext(ctx, `
			UPDATE notifications
			SET subject_title = ?, priority = ?, unread = ?, updated_at = ?
			WHERE github_id = ?
		`, n.SubjectTitle, n.Priority, boolToInt(n.Unread), n.UpdatedAt, n.GitHubID)
		return false, err
	}

	// New notification.
	id := randomID()
	_, err = m.db.ExecContext(ctx, `
		INSERT INTO notifications (id, github_id, repo_full_name, subject_title, subject_type, subject_url, reason, priority, unread, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, id, n.GitHubID, n.RepoFullName, n.SubjectTitle, n.SubjectType, n.SubjectURL, n.Reason, n.Priority, boolToInt(n.Unread), n.UpdatedAt)
	return true, err
}

// GetUnread returns unread notifications ordered by priority.
func (m *Monitor) GetUnread(ctx context.Context, limit int) ([]Notification, error) {
	return m.query(ctx, `
		SELECT id, github_id, repo_full_name, subject_title, subject_type, subject_url,
		       reason, priority, unread, updated_at, created_at, dismissed_at
		FROM notifications
		WHERE unread = 1
		ORDER BY priority ASC, updated_at DESC
		LIMIT ?
	`, limit)
}

// GetByPriority returns notifications at or above the given priority level.
func (m *Monitor) GetByPriority(ctx context.Context, maxPriority, limit int) ([]Notification, error) {
	return m.query(ctx, `
		SELECT id, github_id, repo_full_name, subject_title, subject_type, subject_url,
		       reason, priority, unread, updated_at, created_at, dismissed_at
		FROM notifications
		WHERE priority <= ?
		ORDER BY priority ASC, updated_at DESC
		LIMIT ?
	`, maxPriority, limit)
}

// Dismiss marks a notification as read.
func (m *Monitor) Dismiss(ctx context.Context, id string) error {
	_, err := m.db.ExecContext(ctx, `
		UPDATE notifications SET unread = 0, dismissed_at = datetime('now') WHERE id = ?
	`, id)
	return err
}

// Summary returns a count of unread notifications by priority.
func (m *Monitor) Summary(ctx context.Context) (map[int]int, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT priority, COUNT(*) FROM notifications WHERE unread = 1 GROUP BY priority ORDER BY priority
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[int]int)
	for rows.Next() {
		var p, c int
		if err := rows.Scan(&p, &c); err != nil {
			return nil, err
		}
		result[p] = c
	}
	return result, rows.Err()
}

func (m *Monitor) query(ctx context.Context, q string, args ...interface{}) ([]Notification, error) {
	rows, err := m.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var notifications []Notification
	for rows.Next() {
		var n Notification
		var unread int
		var dismissedAt sql.NullString
		if err := rows.Scan(&n.ID, &n.GitHubID, &n.RepoFullName, &n.SubjectTitle,
			&n.SubjectType, &n.SubjectURL, &n.Reason, &n.Priority, &unread,
			&n.UpdatedAt, &n.CreatedAt, &dismissedAt); err != nil {
			return nil, err
		}
		n.Unread = unread == 1
		if dismissedAt.Valid {
			n.DismissedAt = dismissedAt.String
		}
		notifications = append(notifications, n)
	}
	return notifications, rows.Err()
}

func toNotification(n *gh.Notification) Notification {
	notif := Notification{
		GitHubID:     n.GetID(),
		RepoFullName: n.GetRepository().GetFullName(),
		SubjectTitle: n.GetSubject().GetTitle(),
		SubjectType:  n.GetSubject().GetType(),
		SubjectURL:   n.GetSubject().GetURL(),
		Reason:       n.GetReason(),
		Unread:       n.GetUnread(),
		UpdatedAt:    n.GetUpdatedAt().UTC().Format(time.RFC3339),
	}
	notif.Priority = scorePriority(notif.Reason, notif.SubjectType)
	return notif
}

// WebURL converts a GitHub API URL to a web-browsable URL.
// e.g. "https://api.github.com/repos/owner/repo/pulls/123" → "https://github.com/owner/repo/pull/123"
func (n Notification) WebURL() string {
	u := n.SubjectURL
	if u == "" {
		return "https://github.com/" + n.RepoFullName
	}
	// Strip API prefix.
	u = strings.Replace(u, "https://api.github.com/repos/", "https://github.com/", 1)
	// API uses "pulls" for PRs, web uses "pull".
	u = strings.Replace(u, "/pulls/", "/pull/", 1)
	// API uses "commits", web uses "commit".
	u = strings.Replace(u, "/commits/", "/commit/", 1)
	return u
}

func scorePriority(reason, subjectType string) int {
	// P0: security alerts.
	if reason == "security_alert" || strings.Contains(strings.ToLower(subjectType), "vulnerability") {
		return P0
	}

	// P1: review requests, CI on own PRs.
	if reason == "review_requested" || reason == "ci_activity" {
		return P1
	}

	// P2: direct engagement.
	switch reason {
	case "mention", "assign", "comment":
		return P2
	}

	// P3: team-level.
	if reason == "subscribed" || reason == "team_mention" {
		return P3
	}

	// P4: everything else.
	return P4
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func randomID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return hex.EncodeToString([]byte(time.Now().String()))[:32]
	}
	return hex.EncodeToString(b)
}
