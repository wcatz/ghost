// Package telegram provides a Telegram bot interface for Ghost.
// Commands: /status, /notifications, /memory, /remind, /briefing, /meetings, /emails.
// Only responds to whitelisted user IDs.
package telegram

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"

	goog "github.com/wcatz/ghost/internal/google"
	"github.com/wcatz/ghost/internal/mdv2"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/wcatz/ghost/internal/briefing"
	gh "github.com/wcatz/ghost/internal/github"
	"github.com/wcatz/ghost/internal/provider"
	"github.com/wcatz/ghost/internal/scheduler"
)

const (
	maxQueryLen    = 200
	maxRemindLen   = 500
	telegramMsgMax = 4096
)

// Bot is the Ghost Telegram bot.
type Bot struct {
	bot             *bot.Bot
	store           provider.MemoryStore
	ghMonitor       *gh.Monitor
	sched           *scheduler.Scheduler
	google          GoogleProvider
	briefingSources briefing.Sources
	db              *sql.DB
	logger          *slog.Logger
	allowedIDs      map[int64]bool
	serverAddr      string // Ghost serve address for API calls
	serverToken     string // Bearer token for Ghost API auth
	token           string // Telegram bot token (for file download URLs)
	stt             STTProvider   // optional voice transcription
	approval        approvalState // pending approval tracking
	mu              sync.Mutex
	pendingChat     map[int64]string // chatID → sessionID for reply routing
}

// GoogleProvider is the interface for Google Calendar/Gmail access.
type GoogleProvider interface {
	TodayEvents(ctx context.Context) ([]goog.Event, error)
	UnreadCount(ctx context.Context) (int, error)
	RecentUnread(ctx context.Context, limit int) ([]goog.Email, error)
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
		token:      cfg.Token,
		allowedIDs:  make(map[int64]bool, len(cfg.AllowedIDs)),
		pendingChat: make(map[int64]string),
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
	b.RegisterHandler(bot.HandlerTypeMessageText, "status", bot.MatchTypeCommand, tb.handleStatus)
	b.RegisterHandler(bot.HandlerTypeMessageText, "notifications", bot.MatchTypeCommand, tb.handleNotifications)
	b.RegisterHandler(bot.HandlerTypeMessageText, "memory", bot.MatchTypeCommand, tb.handleMemory)
	b.RegisterHandler(bot.HandlerTypeMessageText, "remind", bot.MatchTypeCommand, tb.handleRemind)
	b.RegisterHandler(bot.HandlerTypeMessageText, "briefing", bot.MatchTypeCommand, tb.handleBriefing)
	b.RegisterHandler(bot.HandlerTypeMessageText, "meetings", bot.MatchTypeCommand, tb.handleMeetings)
	b.RegisterHandler(bot.HandlerTypeMessageText, "emails", bot.MatchTypeCommand, tb.handleEmails)
	b.RegisterHandler(bot.HandlerTypeMessageText, "help", bot.MatchTypeCommand, tb.handleHelp)
	b.RegisterHandler(bot.HandlerTypeMessageText, "sessions", bot.MatchTypeCommand, tb.handleSessions)
	b.RegisterHandler(bot.HandlerTypeMessageText, "chat", bot.MatchTypeCommand, tb.handleChat)

	// Callback query handlers.
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "approve:", bot.MatchTypePrefix, tb.handleApprovalCallback)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "deny:", bot.MatchTypePrefix, tb.handleApprovalCallback)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "chat:", bot.MatchTypePrefix, tb.handleChatCallback)

	return tb, nil
}

// SetGoogle configures the Google Calendar/Gmail provider.
func (tb *Bot) SetGoogle(g GoogleProvider) {
	tb.google = g
}

// Run starts the bot polling loop. Blocks until ctx is cancelled.
func (tb *Bot) Run(ctx context.Context) {
	tb.registerCommands(ctx)
	tb.logger.Info("telegram bot starting")
	tb.bot.Start(ctx)
}

// registerCommands pushes Ghost's command menu to the Telegram API.
func (tb *Bot) registerCommands(ctx context.Context) {
	if _, err := tb.bot.DeleteMyCommands(ctx, nil); err != nil {
		tb.logger.Warn("delete old telegram commands (non-fatal)", "error", err)
	}

	commands := []models.BotCommand{
		{Command: "status", Description: "System status + notification summary"},
		{Command: "notifications", Description: "List unread GitHub notifications"},
		{Command: "meetings", Description: "Today's calendar with Meet links"},
		{Command: "emails", Description: "Recent unread emails"},
		{Command: "sessions", Description: "List active Ghost sessions"},
		{Command: "chat", Description: "Chat with a session: /chat <id> <msg>"},
		{Command: "memory", Description: "Manage memories: search, add, delete"},
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
	for _, chunk := range mdv2.Split(text, telegramMsgMax) {
		_, err := tb.bot.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:    chatID,
			Text:      chunk,
			ParseMode: models.ParseModeMarkdown,
			LinkPreviewOptions: &models.LinkPreviewOptions{
				IsDisabled: bot.True(),
			},
		})
		if err != nil {
			return err
		}
	}
	return nil
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
		// Check Message-based updates.
		if update.Message != nil && update.Message.From != nil {
			if !tb.allowedIDs[update.Message.From.ID] {
				tb.logger.Warn("unauthorized telegram user", "user_id", update.Message.From.ID, "username", update.Message.From.Username)
				return
			}
			next(ctx, b, update)
			return
		}
		// Check CallbackQuery-based updates.
		if update.CallbackQuery != nil && update.CallbackQuery.From.ID != 0 {
			if !tb.allowedIDs[update.CallbackQuery.From.ID] {
				return
			}
			next(ctx, b, update)
			return
		}
	}
}

// --- Command Handlers ---

func (tb *Bot) handleStatus(ctx context.Context, b *bot.Bot, update *models.Update) {
	tb.sendTyping(ctx, update)

	var sb strings.Builder
	sb.WriteString("*Ghost Status*\n\n")

	if tb.ghMonitor != nil {
		summary, err := tb.ghMonitor.Summary(ctx)
		if err == nil {
			total := 0
			for _, c := range summary {
				total += c
			}
			fmt.Fprintf(&sb, "📬 Notifications: %d unread\n", total)
			for p := gh.P0; p <= gh.P4; p++ {
				if c, ok := summary[p]; ok && c > 0 {
					fmt.Fprintf(&sb, "  P%d: %d\n", p, c)
				}
			}
		}
	}

	sb.WriteString("\n✅ Ghost is running")
	tb.reply(ctx, b, update, sb.String())
}

func (tb *Bot) handleNotifications(ctx context.Context, b *bot.Bot, update *models.Update) {
	if tb.ghMonitor == nil {
		tb.reply(ctx, b, update, "GitHub monitor not configured\\.")
		return
	}

	tb.sendTyping(ctx, update)

	notifs, err := tb.ghMonitor.GetUnread(ctx, 15)
	if err != nil {
		tb.reply(ctx, b, update, "Error fetching notifications: "+mdv2.Esc(err.Error()))
		return
	}

	if len(notifs) == 0 {
		tb.reply(ctx, b, update, "No unread notifications\\. 🎉")
		return
	}

	var sb strings.Builder
	var buttons [][]models.InlineKeyboardButton
	fmt.Fprintf(&sb, "*Unread Notifications* \\(%d\\)\n\n", len(notifs))
	for _, n := range notifs {
		emoji := priorityEmoji(n.Priority)
		fmt.Fprintf(&sb, "%s *P%d* `%s`\n  %s\n  _%s_\n\n",
			emoji, n.Priority, mdv2.Esc(n.RepoFullName), mdv2.Esc(n.SubjectTitle), mdv2.Esc(n.Reason))
		if htmlURL := ghAPIToHTML(n.SubjectURL, n.SubjectType); htmlURL != "" {
			label := fmt.Sprintf("%s %s", emoji, truncate(n.SubjectTitle, 30))
			buttons = append(buttons, []models.InlineKeyboardButton{
				{Text: label, URL: htmlURL},
			})
		}
	}
	tb.replyWithKeyboard(ctx, b, update, sb.String(), buttons)
}

func (tb *Bot) handleMemory(ctx context.Context, b *bot.Bot, update *models.Update) {
	text := update.Message.Text
	parts := strings.Fields(text)
	if len(parts) < 2 {
		tb.replyMemoryUsage(ctx, b, update)
		return
	}

	sub := parts[1]
	switch sub {
	case "search":
		tb.handleMemorySearch(ctx, b, update, parts[2:])
	case "add":
		tb.handleMemoryAdd(ctx, b, update, parts[2:])
	case "delete":
		tb.handleMemoryDelete(ctx, b, update, parts[2:])
	default:
		tb.replyMemoryUsage(ctx, b, update)
	}
}

func (tb *Bot) replyMemoryUsage(ctx context.Context, b *bot.Bot, update *models.Update) {
	tb.reply(ctx, b, update, `*Usage:*
/memory search <project\_id> <query>
/memory add <project\_id> <content>
/memory delete <memory\_id>`)
}

func (tb *Bot) handleMemorySearch(ctx context.Context, b *bot.Bot, update *models.Update, args []string) {
	if len(args) < 2 {
		tb.reply(ctx, b, update, "Usage: `/memory search <project_id> <query>`")
		return
	}

	projectID := args[0]
	query := strings.Join(args[1:], " ")
	if len(query) > maxQueryLen {
		query = query[:maxQueryLen]
	}

	tb.sendTyping(ctx, update)

	memories, err := tb.store.SearchFTS(ctx, projectID, query, 10)
	if err != nil {
		tb.reply(ctx, b, update, "Search error: "+mdv2.Esc(err.Error()))
		return
	}

	if len(memories) == 0 {
		tb.reply(ctx, b, update, "No matching memories found\\.")
		return
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "*Memories* \\(%d results\\)\n\n", len(memories))
	for _, m := range memories {
		fmt.Fprintf(&sb, "• \\[%s\\] %.1f — %s\n", mdv2.Esc(m.Category), m.Importance, mdv2.Esc(m.Content))
	}
	tb.reply(ctx, b, update, sb.String())
}

func (tb *Bot) handleMemoryAdd(ctx context.Context, b *bot.Bot, update *models.Update, args []string) {
	if tb.serverAddr == "" {
		tb.reply(ctx, b, update, "Ghost server not configured\\.")
		return
	}

	if len(args) < 2 {
		tb.reply(ctx, b, update, "Usage: `/memory add <project_id> <content>`")
		return
	}

	projectID := args[0]
	content := strings.Join(args[1:], " ")

	tb.sendTyping(ctx, update)

	id, merged, err := tb.createMemory(projectID, content)
	if err != nil {
		tb.reply(ctx, b, update, "Error creating memory: "+mdv2.Esc(err.Error()))
		return
	}

	action := "Created"
	if merged {
		action = "Merged into existing"
	}
	tb.reply(ctx, b, update, fmt.Sprintf("✅ %s memory `%s`", action, mdv2.Esc(id[:8])))
}

func (tb *Bot) handleMemoryDelete(ctx context.Context, b *bot.Bot, update *models.Update, args []string) {
	if tb.serverAddr == "" {
		tb.reply(ctx, b, update, "Ghost server not configured\\.")
		return
	}

	if len(args) < 1 {
		tb.reply(ctx, b, update, "Usage: `/memory delete <memory_id>`")
		return
	}

	memoryID := args[0]

	tb.sendTyping(ctx, update)

	if err := tb.deleteMemory(memoryID); err != nil {
		tb.reply(ctx, b, update, "Error deleting memory: "+mdv2.Esc(err.Error()))
		return
	}

	tb.reply(ctx, b, update, fmt.Sprintf("🗑 Deleted memory `%s`", mdv2.Esc(memoryID)))
}

func (tb *Bot) handleRemind(ctx context.Context, b *bot.Bot, update *models.Update) {
	text := update.Message.Text
	parts := strings.SplitN(text, " ", 2)
	if len(parts) < 2 || strings.TrimSpace(parts[1]) == "" {
		tb.reply(ctx, b, update, "Usage: `/remind <message>`\nExample: `/remind tomorrow at 2pm check the deploy`")
		return
	}

	if tb.sched == nil {
		tb.reply(ctx, b, update, "Scheduler not configured\\.")
		return
	}

	reminder := parts[1]
	if len(reminder) > maxRemindLen {
		tb.reply(ctx, b, update, fmt.Sprintf("Reminder too long \\(%d chars\\)\\. Max %d\\.", len(reminder), maxRemindLen))
		return
	}

	dueAt, err := tb.sched.AddReminder(ctx, reminder)
	if err != nil {
		tb.reply(ctx, b, update, "Failed to create reminder: "+mdv2.Esc(err.Error()))
		return
	}

	tb.reply(ctx, b, update, fmt.Sprintf("⏰ Reminder set for %s:\n%s",
		mdv2.Esc(dueAt.Local().Format("Mon Jan 2 15:04")), mdv2.Esc(reminder)))
}

func (tb *Bot) handleBriefing(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	chatID := update.Message.Chat.ID
	tb.sendTyping(ctx, update)

	// Send initial message, then edit in-place as sections arrive.
	sent, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:    chatID,
		Text:      "⏳ Loading briefing\\.\\.\\.",
		ParseMode: models.ParseModeMarkdown,
		LinkPreviewOptions: &models.LinkPreviewOptions{
			IsDisabled: bot.True(),
		},
	})
	if err != nil {
		tb.logger.Error("telegram send", "error", err)
		return
	}

	msg := briefing.Generate(ctx, tb.briefingSources)

	_, err = b.EditMessageText(ctx, &bot.EditMessageTextParams{
		ChatID:    chatID,
		MessageID: sent.ID,
		Text:      msg,
		ParseMode: models.ParseModeMarkdown,
		LinkPreviewOptions: &models.LinkPreviewOptions{
			IsDisabled: bot.True(),
		},
	})
	if err != nil {
		tb.logger.Error("telegram edit", "error", err)
	}
}

// SetBriefingSources configures the data sources for on-demand briefings.
func (tb *Bot) SetBriefingSources(src briefing.Sources) {
	tb.briefingSources = src
}

func (tb *Bot) handleMeetings(ctx context.Context, b *bot.Bot, update *models.Update) {
	if tb.google == nil {
		tb.reply(ctx, b, update, "Google Calendar not configured\\.")
		return
	}

	tb.sendTyping(ctx, update)

	events, err := tb.google.TodayEvents(ctx)
	if err != nil {
		tb.reply(ctx, b, update, "Error fetching calendar: "+mdv2.Esc(err.Error()))
		return
	}

	if len(events) == 0 {
		tb.reply(ctx, b, update, "No meetings today\\. 🎉")
		return
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "*Today's Meetings* \\(%d\\)\n\n", len(events))
	for _, e := range events {
		if e.AllDay {
			fmt.Fprintf(&sb, "📅 %s \\(all day\\)\n", mdv2.Esc(e.Summary))
		} else {
			fmt.Fprintf(&sb, "🕐 %s – %s  *%s*\n",
				mdv2.Esc(e.Start.Local().Format("15:04")),
				mdv2.Esc(e.End.Local().Format("15:04")),
				mdv2.Esc(e.Summary))
		}
		if e.Location != "" {
			fmt.Fprintf(&sb, "  📍 %s\n", mdv2.Esc(e.Location))
		}
		if e.MeetLink != "" {
			fmt.Fprintf(&sb, "  🔗 [Join Meet](%s)\n", e.MeetLink)
		}
		sb.WriteString("\n")
	}
	tb.reply(ctx, b, update, sb.String())
}

func (tb *Bot) handleEmails(ctx context.Context, b *bot.Bot, update *models.Update) {
	if tb.google == nil {
		tb.reply(ctx, b, update, "Gmail not configured\\.")
		return
	}

	tb.sendTyping(ctx, update)

	count, err := tb.google.UnreadCount(ctx)
	if err != nil {
		tb.reply(ctx, b, update, "Error fetching emails: "+mdv2.Esc(err.Error()))
		return
	}

	if count == 0 {
		tb.reply(ctx, b, update, "Inbox zero\\! 📭")
		return
	}

	emails, err := tb.google.RecentUnread(ctx, 10)
	if err != nil {
		tb.reply(ctx, b, update, fmt.Sprintf("📬 %d unread \\(error fetching details: %s\\)", count, mdv2.Esc(err.Error())))
		return
	}

	var sb strings.Builder
	var buttons [][]models.InlineKeyboardButton
	fmt.Fprintf(&sb, "*Unread Emails* \\(%d total\\)\n\n", count)
	for _, e := range emails {
		fmt.Fprintf(&sb, "📧 *%s*\n  From: %s\n", mdv2.Esc(e.Subject), mdv2.Esc(e.From))
		if e.Snippet != "" {
			snippet := e.Snippet
			if len(snippet) > 100 {
				snippet = snippet[:100] + "..."
			}
			fmt.Fprintf(&sb, "  _%s_\n", mdv2.Esc(snippet))
		}
		sb.WriteString("\n")
		gmailURL := fmt.Sprintf("https://mail.google.com/mail/u/0/#inbox/%s", e.ID)
		label := truncate(e.Subject, 35)
		if label == "" {
			label = "Open email"
		}
		buttons = append(buttons, []models.InlineKeyboardButton{
			{Text: "📧 " + label, URL: gmailURL},
		})
	}
	tb.replyWithKeyboard(ctx, b, update, sb.String(), buttons)
}

func (tb *Bot) handleHelp(ctx context.Context, b *bot.Bot, update *models.Update) {
	tb.reply(ctx, b, update, `*Ghost Commands*

/status — System status \+ notification summary
/notifications — List unread GitHub notifications
/meetings — Today's calendar with Meet links
/emails — Recent unread emails
/sessions — List active Ghost sessions
/chat — Chat with a session
/memory — Search, add, or delete memories
/remind — Set a reminder
/briefing — Get your morning briefing
/help — This message`)
}

func (tb *Bot) handleDefault(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	// Handle voice messages.
	if update.Message.Voice != nil {
		tb.handleVoice(ctx, b, update)
		return
	}
	// Check if this is a reply to an approval message with instructions.
	if tb.handleInstructionReply(ctx, b, update) {
		return
	}
	// Check if this is a reply to a pending chat session prompt.
	if tb.handlePendingChatReply(ctx, b, update) {
		return
	}
	tb.reply(ctx, b, update, "Use /help to see available commands\\.")
}

// --- Helpers ---

func (tb *Bot) sendTyping(ctx context.Context, update *models.Update) {
	if update.Message == nil {
		return
	}
	_, _ = tb.bot.SendChatAction(ctx, &bot.SendChatActionParams{
		ChatID: update.Message.Chat.ID,
		Action: models.ChatActionTyping,
	})
}

func (tb *Bot) reply(ctx context.Context, b *bot.Bot, update *models.Update, text string) {
	if update.Message == nil {
		return
	}
	chatID := update.Message.Chat.ID
	for _, chunk := range mdv2.Split(text, telegramMsgMax) {
		_, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:    chatID,
			Text:      chunk,
			ParseMode: models.ParseModeMarkdown,
			LinkPreviewOptions: &models.LinkPreviewOptions{
				IsDisabled: bot.True(),
			},
		})
		if err != nil {
			tb.logger.Error("telegram send", "error", err, "chat_id", chatID)
			return
		}
	}
}

// replyText sends plain text with no parse mode — safe for unescaped content such
// as raw Claude responses that contain standard Markdown but are not MarkdownV2-escaped.
func (tb *Bot) replyText(ctx context.Context, b *bot.Bot, update *models.Update, text string) {
	if update.Message == nil {
		return
	}
	chatID := update.Message.Chat.ID
	for _, chunk := range mdv2.Split(text, telegramMsgMax) {
		_, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: chatID,
			Text:   chunk,
			LinkPreviewOptions: &models.LinkPreviewOptions{
				IsDisabled: bot.True(),
			},
		})
		if err != nil {
			tb.logger.Error("telegram send (plain)", "error", err, "chat_id", chatID)
			return
		}
	}
}

// SendAlertToAll sends a formatted P0/P1 GitHub notification alert to all allowed users.
// Includes the priority emoji, repo/title, reason, and an inline button linking to the PR/issue.
func (tb *Bot) SendAlertToAll(ctx context.Context, n gh.Notification) {
	emoji := priorityEmoji(n.Priority)
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s *P%d Alert*\n", emoji, n.Priority)
	fmt.Fprintf(&sb, "`%s`\n", mdv2.Esc(n.RepoFullName))
	fmt.Fprintf(&sb, "%s\n", mdv2.Esc(n.SubjectTitle))
	fmt.Fprintf(&sb, "_%s_", mdv2.Esc(n.Reason))
	text := sb.String()

	var markup *models.InlineKeyboardMarkup
	if htmlURL := ghAPIToHTML(n.SubjectURL, n.SubjectType); htmlURL != "" {
		label := truncate(n.SubjectTitle, 35)
		if label == "" {
			label = "Open"
		}
		markup = &models.InlineKeyboardMarkup{
			InlineKeyboard: [][]models.InlineKeyboardButton{
				{{Text: emoji + " " + label, URL: htmlURL}},
			},
		}
	}

	for id := range tb.allowedIDs {
		params := &bot.SendMessageParams{
			ChatID:    id,
			Text:      text,
			ParseMode: models.ParseModeMarkdown,
			LinkPreviewOptions: &models.LinkPreviewOptions{
				IsDisabled: bot.True(),
			},
		}
		if markup != nil {
			params.ReplyMarkup = markup
		}
		if _, err := tb.bot.SendMessage(ctx, params); err != nil {
			tb.logger.Error("telegram alert", "error", err, "user_id", id)
		}
	}
}

func (tb *Bot) replyWithKeyboard(ctx context.Context, b *bot.Bot, update *models.Update, text string, buttons [][]models.InlineKeyboardButton) {
	if update.Message == nil {
		return
	}
	chatID := update.Message.Chat.ID
	// Telegram allows max 100 inline buttons. Trim if needed.
	if len(buttons) > 20 {
		buttons = buttons[:20]
	}
	var markup *models.InlineKeyboardMarkup
	if len(buttons) > 0 {
		markup = &models.InlineKeyboardMarkup{InlineKeyboard: buttons}
	}
	chunks := mdv2.Split(text, telegramMsgMax)
	for i, chunk := range chunks {
		params := &bot.SendMessageParams{
			ChatID:    chatID,
			Text:      chunk,
			ParseMode: models.ParseModeMarkdown,
			LinkPreviewOptions: &models.LinkPreviewOptions{
				IsDisabled: bot.True(),
			},
		}
		// Attach keyboard to last chunk only.
		if i == len(chunks)-1 && markup != nil {
			params.ReplyMarkup = markup
		}
		if _, err := b.SendMessage(ctx, params); err != nil {
			tb.logger.Error("telegram send", "error", err, "chat_id", chatID)
			return
		}
	}
}

// ghAPIToHTML converts a GitHub API URL to the corresponding HTML URL.
// e.g. https://api.github.com/repos/owner/repo/pulls/123 -> https://github.com/owner/repo/pull/123
func ghAPIToHTML(apiURL, subjectType string) string {
	if apiURL == "" {
		return ""
	}
	s := strings.Replace(apiURL, "https://api.github.com/repos/", "https://github.com/", 1)
	// API uses "pulls" but HTML uses "pull".
	s = strings.Replace(s, "/pulls/", "/pull/", 1)
	// API uses "issues" which is the same in HTML.
	if s == apiURL {
		return "" // couldn't convert
	}
	return s
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
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
