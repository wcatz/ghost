// Package scheduler provides cron-based job scheduling and one-shot reminders.
// Jobs are persisted in SQLite so they survive restarts.
package scheduler

import (
	"context"
	"database/sql"
	"encoding/hex"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/go-co-op/gocron/v2"
	"github.com/olebedev/when"
	"github.com/olebedev/when/rules/common"
	"github.com/olebedev/when/rules/en"
)

// AlertFunc is called when a reminder fires or a job wants to notify.
type AlertFunc func(message string)

// Scheduler manages cron jobs and one-shot reminders.
type Scheduler struct {
	cron    gocron.Scheduler
	db      *sql.DB
	logger  *slog.Logger
	onAlert AlertFunc
	parser  *when.Parser
	mu      sync.Mutex
}

// New creates a scheduler backed by SQLite.
func New(db *sql.DB, logger *slog.Logger) (*Scheduler, error) {
	cron, err := gocron.NewScheduler()
	if err != nil {
		return nil, fmt.Errorf("create scheduler: %w", err)
	}

	parser := when.New(nil)
	parser.Add(en.All...)
	parser.Add(common.All...)

	return &Scheduler{
		cron:   cron,
		db:     db,
		logger: logger,
		parser: parser,
	}, nil
}

// OnAlert registers a callback for reminder notifications.
// Must be called before Start.
func (s *Scheduler) OnAlert(fn AlertFunc) {
	s.onAlert = fn
}

// Start begins the scheduler and reminder check loop.
func (s *Scheduler) Start(ctx context.Context) {
	s.cron.Start()
	s.logger.Info("scheduler started")

	// Check reminders every 30 seconds.
	go s.reminderLoop(ctx)

	<-ctx.Done()
	if err := s.cron.Shutdown(); err != nil {
		s.logger.Error("scheduler shutdown", "error", err)
	}
}

// AddReminder parses a natural language time and creates a reminder.
// Returns the parsed due time. If parsing fails, uses a fallback of 1 hour from now.
func (s *Scheduler) AddReminder(ctx context.Context, text string) (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	dueAt := time.Now().Add(1 * time.Hour) // fallback

	result, err := s.parser.Parse(text, time.Now())
	if err == nil && result != nil {
		dueAt = result.Time
	}

	id, err := randomID()
	if err != nil {
		return time.Time{}, fmt.Errorf("generate id: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO reminders (id, message, due_at) VALUES (?, ?, ?)
	`, id, text, dueAt.UTC().Format(time.RFC3339))
	if err != nil {
		return time.Time{}, fmt.Errorf("save reminder: %w", err)
	}

	s.logger.Info("reminder created", "id", id, "due_at", dueAt, "message", text)
	return dueAt, nil
}

// ListPending returns unfired reminders ordered by due date.
func (s *Scheduler) ListPending(ctx context.Context, limit int) ([]Reminder, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, message, due_at, fired, created_at
		FROM reminders
		WHERE fired = 0
		ORDER BY due_at ASC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var reminders []Reminder
	for rows.Next() {
		var r Reminder
		var fired int
		if err := rows.Scan(&r.ID, &r.Message, &r.DueAt, &fired, &r.CreatedAt); err != nil {
			return nil, err
		}
		r.Fired = fired == 1
		reminders = append(reminders, r)
	}
	return reminders, rows.Err()
}

// AddCronJob registers a recurring job.
func (s *Scheduler) AddCronJob(ctx context.Context, name, cronExpr string, payload map[string]interface{}, fn func()) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	payloadJSON, _ := json.Marshal(payload)
	id, err := randomID()
	if err != nil {
		return fmt.Errorf("generate id: %w", err)
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO scheduled_jobs (id, name, schedule, payload) VALUES (?, ?, ?, ?)
	`, id, name, cronExpr, string(payloadJSON))
	if err != nil {
		return fmt.Errorf("save job: %w", err)
	}

	_, err = s.cron.NewJob(
		gocron.CronJob(cronExpr, false),
		gocron.NewTask(fn),
		gocron.WithName(name),
	)
	if err != nil {
		// Clean up orphaned DB record on schedule failure.
		s.db.ExecContext(ctx, `DELETE FROM scheduled_jobs WHERE id = ?`, id)
		return fmt.Errorf("schedule job: %w", err)
	}

	s.logger.Info("cron job added", "name", name, "schedule", cronExpr)
	return nil
}

// Reminder is a one-shot reminder.
type Reminder struct {
	ID        string `json:"id"`
	Message   string `json:"message"`
	DueAt     string `json:"due_at"`
	Fired     bool   `json:"fired"`
	CreatedAt string `json:"created_at"`
}

func (s *Scheduler) reminderLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.fireReminders(ctx)
		}
	}
}

func (s *Scheduler) fireReminders(ctx context.Context) {
	now := time.Now().UTC().Format(time.RFC3339)

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, message FROM reminders WHERE fired = 0 AND due_at <= ?
	`, now)
	if err != nil {
		s.logger.Error("check reminders", "error", err)
		return
	}
	defer rows.Close()

	var fired []string
	for rows.Next() {
		var id, message string
		if err := rows.Scan(&id, &message); err != nil {
			continue
		}

		if s.onAlert != nil {
			s.onAlert(fmt.Sprintf("⏰ Reminder: %s", message))
		}
		fired = append(fired, id)
	}

	for _, id := range fired {
		if _, err := s.db.ExecContext(ctx, `UPDATE reminders SET fired = 1 WHERE id = ?`, id); err != nil {
			s.logger.Error("mark reminder fired", "error", err, "id", id)
		}
	}
}

func randomID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("crypto/rand: %w", err)
	}
	return hex.EncodeToString(b), nil
}
