// Package telegram provides a Telegram bot interface for Ghost.
// Commands: /status, /notifications, /memory, /remind, /briefing.
// Only responds to whitelisted user IDs.
package telegram

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/wcatz/ghost/internal/briefing"
	gh "github.com/wcatz/ghost/internal/github"
	"github.com/wcatz/ghost/internal/provider"
	"github.com/wcatz/ghost/internal/scheduler"
)

// Bot is the Ghost Telegram bot.
type Bot struct {
	bot            *bot.Bot
	store          provider.MemoryStore
	ghMonitor      *gh.Monitor
	sched          *scheduler.Scheduler
	briefingSources briefing.Sources
	db             *sql.DB
	logger         *slog.Logger
	allowedIDs     map[int64]bool
}

// Config holds Telegram bot configuration.
type Config struct {
	Token      string
	AllowedIDs []int64
}

// New creates and configures the Telegram bot.
func New(cfg Config, store provider.MemoryStore, ghMonitor *gh.Monitor, sched *scheduler.Scheduler, db *sql.DB, logger *slog.Logger) (*Bot, error) {
	tb := &Bot{
		store:      store,
		ghMonitor:  ghMonitor,
		sched:      sched,
		db:         db,
		logger:     logger,
		allowedIDs: make(map[int64]bool, len(cfg.AllowedIDs)),
	}

	for _, id := range cfg.AllowedIDs {
		tb.allowedIDs[id] = true
	}

	b, err := bot.New(cfg.Token,
		bot.WithMiddlewares(tb.authMiddleware),
		bot.WithDefaultHandler(tb.handleDefault),
	)
	if err != nil {
		return nil, fmt.Errorf("create telegram bot: %w", err)
	}
	tb.bot = b

	// Register commands.
	b.RegisterHandler(bot.HandlerTypeMessageText, "/status", bot.MatchTypeCommand, tb.handleStatus)
	b.RegisterHandler(bot.HandlerTypeMessageText, "/notifications", bot.MatchTypeCommand, tb.handleNotifications)
	b.RegisterHandler(bot.HandlerTypeMessageText, "/memory", bot.MatchTypeCommand, tb.handleMemory)
	b.RegisterHandler(bot.HandlerTypeMessageText, "/remind", bot.MatchTypeCommand, tb.handleRemind)
	b.RegisterHandler(bot.HandlerTypeMessageText, "/briefing", bot.MatchTypeCommand, tb.handleBriefing)
	b.RegisterHandler(bot.HandlerTypeMessageText, "/help", bot.MatchTypeCommand, tb.handleHelp)

	return tb, nil
}

// Run starts the bot polling loop. Blocks until ctx is cancelled.
func (tb *Bot) Run(ctx context.Context) {
	// Clear old commands and register Ghost's command menu.
	tb.registerCommands(ctx)
	tb.logger.Info("telegram bot starting")
	tb.bot.Start(ctx)
}

// registerCommands pushes Ghost's command menu to the Telegram API,
// replacing any previously registered commands.
func (tb *Bot) registerCommands(ctx context.Context) {
	// Delete all existing commands first.
	if _, err := tb.bot.DeleteMyCommands(ctx, &bot.DeleteMyCommandsParams{}); err != nil {
		tb.logger.Error("delete old telegram commands", "error", err)
	}

	commands := []models.BotCommand{
		{Command: "status", Description: "System status + notification summary"},
		{Command: "notifications", Description: "List unread GitHub notifications"},
		{Command: "memory", Description: "Search memories: /memory search <project> <query>"},
		{Command: "remind", Description: "Set a reminder: /remind <message>"},
		{Command: "briefing", Description: "Get your morning briefing"},
		{Command: "help", Description: "Show available commands"},
	}

	if _, err := tb.bot.SetMyCommands(ctx, &bot.SetMyCommandsParams{
		Commands: commands,
	}); err != nil {
		tb.logger.Error("set telegram commands", "error", err)
	} else {
		tb.logger.Info("telegram commands registered", "count", len(commands))
	}
}

// SendMessage sends a message to a specific chat. Used for proactive alerts.
func (tb *Bot) SendMessage(ctx context.Context, chatID int64, text string) error {
	_, err := tb.bot.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:    chatID,
		Text:      text,
		ParseMode: models.ParseModeMarkdownV1,
	})
	return err
}

// SendToAll sends a message to all allowed users.
func (tb *Bot) SendToAll(ctx context.Context, text string) {
	for id := range tb.allowedIDs {
		if err := tb.SendMessage(ctx, id, text); err != nil {
			tb.logger.Error("send to user", "error", err, "user_id", id)
		}
	}
}

// --- Middleware ---

func (tb *Bot) authMiddleware(next bot.HandlerFunc) bot.HandlerFunc {
	return func(ctx context.Context, b *bot.Bot, update *models.Update) {
		if update.Message == nil || update.Message.From == nil {
			return
		}
		userID := update.Message.From.ID
		if !tb.allowedIDs[userID] {
			tb.logger.Warn("unauthorized telegram user", "user_id", userID, "username", update.Message.From.Username)
			return
		}
		next(ctx, b, update)
	}
}

// --- Command Handlers ---

func (tb *Bot) handleStatus(ctx context.Context, b *bot.Bot, update *models.Update) {
	var sb strings.Builder
	sb.WriteString("*Ghost Status*\n\n")

	// Notification summary.
	if tb.ghMonitor != nil {
		summary, err := tb.ghMonitor.Summary(ctx)
		if err == nil {
			total := 0
			for _, c := range summary {
				total += c
			}
			sb.WriteString(fmt.Sprintf("📬 Notifications: %d unread\n", total))
			for p := gh.P0; p <= gh.P4; p++ {
				if c, ok := summary[p]; ok && c > 0 {
					sb.WriteString(fmt.Sprintf("  P%d: %d\n", p, c))
				}
			}
		}
	}

	sb.WriteString("\n✅ Ghost is running")
	tb.reply(ctx, b, update, sb.String())
}

func (tb *Bot) handleNotifications(ctx context.Context, b *bot.Bot, update *models.Update) {
	if tb.ghMonitor == nil {
		tb.reply(ctx, b, update, "GitHub monitor not configured.")
		return
	}

	notifs, err := tb.ghMonitor.GetUnread(ctx, 15)
	if err != nil {
		tb.reply(ctx, b, update, "Error fetching notifications: "+err.Error())
		return
	}

	if len(notifs) == 0 {
		tb.reply(ctx, b, update, "No unread notifications. 🎉")
		return
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("*Unread Notifications* (%d)\n\n", len(notifs)))
	for _, n := range notifs {
		emoji := priorityEmoji(n.Priority)
		sb.WriteString(fmt.Sprintf("%s *P%d* `%s`\n  %s\n  _%s_\n\n",
			emoji, n.Priority, n.RepoFullName, n.SubjectTitle, n.Reason))
	}
	tb.reply(ctx, b, update, sb.String())
}

func (tb *Bot) handleMemory(ctx context.Context, b *bot.Bot, update *models.Update) {
	text := update.Message.Text
	// Parse: /memory search <project> <query>
	parts := strings.Fields(text)
	if len(parts) < 3 {
		tb.reply(ctx, b, update, "Usage: `/memory search <project_id> <query>`")
		return
	}

	sub := parts[1]
	if sub != "search" {
		tb.reply(ctx, b, update, "Usage: `/memory search <project_id> <query>`")
		return
	}

	projectID := parts[2]
	query := strings.Join(parts[3:], " ")
	if query == "" {
		tb.reply(ctx, b, update, "Please provide a search query.")
		return
	}

	memories, err := tb.store.SearchFTS(ctx, projectID, query, 10)
	if err != nil {
		tb.reply(ctx, b, update, "Search error: "+err.Error())
		return
	}

	if len(memories) == 0 {
		tb.reply(ctx, b, update, "No matching memories found.")
		return
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("*Memories* (%d results)\n\n", len(memories)))
	for _, m := range memories {
		sb.WriteString(fmt.Sprintf("• \\[%s\\] %.1f — %s\n", m.Category, m.Importance, m.Content))
	}
	tb.reply(ctx, b, update, sb.String())
}

func (tb *Bot) handleRemind(ctx context.Context, b *bot.Bot, update *models.Update) {
	text := update.Message.Text
	parts := strings.SplitN(text, " ", 2)
	if len(parts) < 2 || strings.TrimSpace(parts[1]) == "" {
		tb.reply(ctx, b, update, "Usage: `/remind <message>`\nExample: `/remind tomorrow at 2pm check the deploy`")
		return
	}

	if tb.sched == nil {
		tb.reply(ctx, b, update, "Scheduler not configured.")
		return
	}

	dueAt, err := tb.sched.AddReminder(ctx, parts[1])
	if err != nil {
		tb.reply(ctx, b, update, "Failed to create reminder: "+err.Error())
		return
	}

	tb.reply(ctx, b, update, fmt.Sprintf("⏰ Reminder set for %s:\n%s",
		dueAt.Local().Format("Mon Jan 2 15:04"), parts[1]))
}

func (tb *Bot) handleBriefing(ctx context.Context, b *bot.Bot, update *models.Update) {
	msg := briefing.Generate(ctx, tb.briefingSources)
	tb.reply(ctx, b, update, msg)
}

// SetBriefingSources configures the data sources for on-demand briefings.
func (tb *Bot) SetBriefingSources(src briefing.Sources) {
	tb.briefingSources = src
}

func (tb *Bot) handleHelp(ctx context.Context, b *bot.Bot, update *models.Update) {
	tb.reply(ctx, b, update, `*Ghost Commands*

/status — System status + notification summary
/notifications — List unread GitHub notifications
/memory search <project> <query> — Search memories
/remind <message> — Set a reminder
/briefing — Get your morning briefing
/help — This message`)
}

func (tb *Bot) handleDefault(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	tb.reply(ctx, b, update, "Use /help to see available commands.")
}

// --- Helpers ---

func (tb *Bot) reply(ctx context.Context, b *bot.Bot, update *models.Update, text string) {
	if update.Message == nil {
		return
	}
	_, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:    update.Message.Chat.ID,
		Text:      text,
		ParseMode: models.ParseModeMarkdownV1,
	})
	if err != nil {
		tb.logger.Error("telegram send", "error", err, "chat_id", update.Message.Chat.ID)
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
	case gh.P3:
		return "🔵"
	default:
		return "⚪"
	}
}

// ParseAllowedIDs converts a comma-separated string of user IDs to int64 slice.
func ParseAllowedIDs(s string) []int64 {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	ids := make([]int64, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if id, err := strconv.ParseInt(p, 10, 64); err == nil {
			ids = append(ids, id)
		}
	}
	return ids
}
