package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/wcatz/ghost/internal/mdv2"
)

// streamEvent represents a parsed SSE event from the Ghost server.
type streamEvent struct {
	Type     string            // text, thinking, tool_start, tool_result, tool_diff, done, error
	Text     string            // text content / error message
	ToolName string            // tool name (for tool events)
	ToolID   string            // tool ID (for correlation)
	Duration string            // tool execution duration (e.g. "320ms")
	IsError  bool              // tool result was an error
	Cost     string            // session cost (on done)
	Diff     map[string]string // file diff metadata (on tool_diff)
}

// toolLogEntry tracks a tool's progress in the status message.
type toolLogEntry struct {
	Name     string
	Status   string // "running", "done", "error", "approval"
	Duration string // e.g. "0.3s"
}

// streamState accumulates streaming data for a single chat interaction.
type streamState struct {
	project       string
	tools         []toolLogEntry
	thinkingLen   int
	thinkingText  strings.Builder
	responseText  strings.Builder
	diffs         []map[string]string
	cost          string
	started       time.Time
	lastStatus    string // last rendered status message (skip no-op edits)
	statusMsgID   int
	errText       string
	tickInterval  time.Duration
}

const (
	maxToolLogEntries = 10
	maxStatusLen      = 3500 // leave room for MarkdownV2 overhead
	defaultTick       = 2 * time.Second
	backoffTick       = 5 * time.Second
)

// streamToSession sends a message to a Ghost session and displays real-time progress.
func (tb *Bot) streamToSession(ctx context.Context, b *bot.Bot, chatID int64, sessionID, projectName, message string) {
	// Send initial status message.
	st := &streamState{
		project:      projectName,
		started:      time.Now(),
		tickInterval: defaultTick,
	}

	statusMsg, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:    chatID,
		Text:      fmt.Sprintf("⚡ Sending to %s\\.\\.\\.", mdv2.Esc(projectName)),
		ParseMode: models.ParseModeMarkdown,
		LinkPreviewOptions: &models.LinkPreviewOptions{
			IsDisabled: bot.True(),
		},
	})
	if err != nil {
		tb.logger.Error("telegram stream status", "error", err)
		return
	}
	st.statusMsgID = statusMsg.ID

	// Open SSE stream.
	events, err := tb.streamChatMessage(ctx, sessionID, message)
	if err != nil {
		tb.editStatus(ctx, b, chatID, st, fmt.Sprintf("❌ Error: %s", mdv2.Esc(err.Error())))
		return
	}

	// Event loop with periodic status edits.
	ticker := time.NewTicker(st.tickInterval)
	defer ticker.Stop()
	dirty := false

	for {
		select {
		case evt, ok := <-events:
			if !ok {
				// Channel closed — stream complete.
				goto done
			}
			dirty = tb.handleStreamEvt(st, evt) || dirty

			// On significant events, trigger an immediate status update.
			switch evt.Type {
			case "tool_start", "tool_result", "error", "done":
				if dirty {
					tb.editStatus(ctx, b, chatID, st, formatStatusMessage(st))
					dirty = false
					ticker.Reset(st.tickInterval)
				}
			}

			if evt.Type == "done" || evt.Type == "error" {
				goto done
			}

		case <-ticker.C:
			if dirty {
				tb.editStatus(ctx, b, chatID, st, formatStatusMessage(st))
				dirty = false
			}

		case <-ctx.Done():
			goto done
		}
	}

done:
	// Delete the status message.
	_, _ = b.DeleteMessage(ctx, &bot.DeleteMessageParams{
		ChatID:    chatID,
		MessageID: st.statusMsgID,
	})

	// Send error if stream failed.
	if st.errText != "" {
		_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: chatID,
			Text:   "❌ " + st.errText,
		})
		return
	}

	// Send the final response.
	response := st.responseText.String()
	if response == "" {
		response = "(no response)"
	}

	// Build inline buttons for post-response actions.
	var buttons [][]models.InlineKeyboardButton
	if st.thinkingLen > 0 {
		buttons = append(buttons, []models.InlineKeyboardButton{
			{Text: fmt.Sprintf("💭 Show thinking (%d chars)", st.thinkingLen), CallbackData: "thinking:" + sessionID},
		})
	}
	if len(st.diffs) > 0 {
		buttons = append(buttons, []models.InlineKeyboardButton{
			{Text: fmt.Sprintf("📝 Show diffs (%d)", len(st.diffs)), CallbackData: "diff:" + sessionID},
		})
	}

	// Store thinking/diff for callback retrieval.
	if st.thinkingLen > 0 || len(st.diffs) > 0 {
		tb.mu.Lock()
		if st.thinkingLen > 0 {
			tb.lastThinking[chatID] = st.thinkingText.String()
		}
		if len(st.diffs) > 0 {
			tb.lastDiffs[chatID] = st.diffs
		}
		tb.mu.Unlock()
	}

	// Store cost.
	if st.cost != "" {
		tb.mu.Lock()
		tb.sessionCosts[sessionID] = st.cost
		tb.mu.Unlock()
	}

	// Send response — plain text (Claude's markdown is not MarkdownV2).
	chunks := mdv2.Split(response, telegramMsgMax)
	for i, chunk := range chunks {
		params := &bot.SendMessageParams{
			ChatID: chatID,
			Text:   chunk,
			LinkPreviewOptions: &models.LinkPreviewOptions{
				IsDisabled: bot.True(),
			},
		}
		// Attach buttons to last chunk.
		if i == len(chunks)-1 && len(buttons) > 0 {
			params.ReplyMarkup = &models.InlineKeyboardMarkup{InlineKeyboard: buttons}
		}
		if _, err := b.SendMessage(ctx, params); err != nil {
			tb.logger.Error("telegram stream response", "error", err, "chat_id", chatID)
			return
		}
	}
}

// handleStreamEvt processes a single SSE event and updates stream state. Returns true if state changed.
func (tb *Bot) handleStreamEvt(st *streamState, evt streamEvent) bool {
	switch evt.Type {
	case "text":
		st.responseText.WriteString(evt.Text)
		return true
	case "thinking":
		st.thinkingText.WriteString(evt.Text)
		st.thinkingLen = st.thinkingText.Len()
		return true
	case "tool_start":
		st.tools = append(st.tools, toolLogEntry{
			Name:   evt.ToolName,
			Status: "running",
		})
		// Trim oldest entries.
		if len(st.tools) > maxToolLogEntries {
			st.tools = st.tools[len(st.tools)-maxToolLogEntries:]
		}
		return true
	case "tool_result":
		// Update the matching tool entry.
		for i := len(st.tools) - 1; i >= 0; i-- {
			if st.tools[i].Name == evt.ToolName && st.tools[i].Status == "running" {
				if evt.IsError {
					st.tools[i].Status = "error"
				} else {
					st.tools[i].Status = "done"
				}
				st.tools[i].Duration = evt.Duration
				break
			}
		}
		return true
	case "tool_diff":
		st.diffs = append(st.diffs, evt.Diff)
		return true
	case "done":
		st.cost = evt.Cost
		return true
	case "error":
		st.errText = evt.Text
		return true
	}
	return false
}

// editStatus edits the status message, handling rate limit backoff.
func (tb *Bot) editStatus(ctx context.Context, b *bot.Bot, chatID int64, st *streamState, text string) {
	if text == st.lastStatus {
		return // no-op
	}
	st.lastStatus = text

	_, err := b.EditMessageText(ctx, &bot.EditMessageTextParams{
		ChatID:    chatID,
		MessageID: st.statusMsgID,
		Text:      text,
		ParseMode: models.ParseModeMarkdown,
		LinkPreviewOptions: &models.LinkPreviewOptions{
			IsDisabled: bot.True(),
		},
	})
	if err != nil {
		// Check for rate limit (429) in error string.
		if strings.Contains(err.Error(), "429") || strings.Contains(err.Error(), "Too Many Requests") {
			st.tickInterval = backoffTick
		}
	}
}

// formatStatusMessage builds the MarkdownV2 status display.
func formatStatusMessage(st *streamState) string {
	var sb strings.Builder
	elapsed := time.Since(st.started).Truncate(time.Second)

	fmt.Fprintf(&sb, "⚙ *Working on %s\\.\\.\\.*\n", mdv2.Esc(st.project))

	if len(st.tools) > 0 {
		sb.WriteString("\n")
		for _, t := range st.tools {
			var icon, suffix string
			switch t.Status {
			case "running":
				icon = "🔧"
				suffix = "⏳"
			case "done":
				icon = "🔧"
				suffix = "✓"
				if t.Duration != "" {
					suffix = "✓ " + mdv2.Esc(t.Duration)
				}
			case "error":
				icon = "❌"
				suffix = "failed"
			case "approval":
				icon = "🔐"
				suffix = "awaiting approval"
			}
			fmt.Fprintf(&sb, "%s %s %s\n", icon, mdv2.Esc(t.Name), suffix)
		}
	}

	if st.thinkingLen > 0 {
		fmt.Fprintf(&sb, "\n💭 Thinking\\.\\.\\. \\(%s chars\\)\n", mdv2.Esc(fmt.Sprintf("%d", st.thinkingLen)))
	}

	fmt.Fprintf(&sb, "\n⏱ %s", mdv2.Esc(elapsed.String()))

	result := sb.String()
	if len(result) > maxStatusLen {
		result = result[:maxStatusLen]
	}
	return result
}

// handleThinkingCallback sends the stored thinking text for a chat.
func (tb *Bot) handleThinkingCallback(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.CallbackQuery == nil {
		return
	}

	_, _ = b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
		CallbackQueryID: update.CallbackQuery.ID,
		Text:            "Sending thinking...",
	})

	if update.CallbackQuery.Message.Message == nil {
		return
	}
	chatID := update.CallbackQuery.Message.Message.Chat.ID

	tb.mu.Lock()
	thinking := tb.lastThinking[chatID]
	tb.mu.Unlock()

	if thinking == "" {
		_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: chatID,
			Text:   "No thinking data available.",
		})
		return
	}

	// Send thinking as plain text, chunked.
	for _, chunk := range mdv2.Split(thinking, telegramMsgMax) {
		_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: chatID,
			Text:   chunk,
		})
	}
}

// handleDiffCallback sends the stored diffs for a chat.
func (tb *Bot) handleDiffCallback(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.CallbackQuery == nil {
		return
	}

	_, _ = b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
		CallbackQueryID: update.CallbackQuery.ID,
		Text:            "Sending diffs...",
	})

	if update.CallbackQuery.Message.Message == nil {
		return
	}
	chatID := update.CallbackQuery.Message.Message.Chat.ID

	tb.mu.Lock()
	diffs := tb.lastDiffs[chatID]
	tb.mu.Unlock()

	if len(diffs) == 0 {
		_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: chatID,
			Text:   "No diff data available.",
		})
		return
	}

	for _, d := range diffs {
		name := d["name"]
		path := d["path"]
		diff := d["diff"]

		header := name
		if path != "" {
			header = path
		}

		text := fmt.Sprintf("📝 %s\n```\n%s\n```", header, diff)
		for _, chunk := range mdv2.Split(text, telegramMsgMax) {
			_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID:    chatID,
				Text:      chunk,
				ParseMode: models.ParseModeMarkdown,
			})
		}
	}
}

// handleUseCallback handles inline "Set active" button from /sessions.
func (tb *Bot) handleUseCallback(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.CallbackQuery == nil {
		return
	}
	sessionID := strings.TrimPrefix(update.CallbackQuery.Data, "use:")

	if update.CallbackQuery.Message.Message == nil {
		return
	}
	chatID := update.CallbackQuery.Message.Message.Chat.ID

	// Resolve session info.
	sessions, err := tb.fetchSessions()
	if err != nil {
		_, _ = b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
			CallbackQueryID: update.CallbackQuery.ID,
			Text:            "Error: " + err.Error(),
			ShowAlert:       true,
		})
		return
	}

	var projectName string
	for _, s := range sessions {
		if s.ID == sessionID {
			projectName = s.ProjectName
			break
		}
	}
	if projectName == "" {
		projectName = sessionID[:8]
	}

	tb.mu.Lock()
	tb.activeSession[chatID] = sessionID
	tb.activeName[chatID] = projectName
	tb.mu.Unlock()

	_, _ = b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
		CallbackQueryID: update.CallbackQuery.ID,
		Text:            "✅ Active: " + projectName,
	})

	// Send confirmation with keyboard.
	tb.sendWithKeyboard(ctx, b, chatID,
		fmt.Sprintf("✅ Active session: *%s*\nFree\\-text messages will be sent to this session\\.", mdv2.Esc(projectName)))
}

// parseStreamEvents parses JSON data from an SSE event into a streamEvent.
func parseStreamData(eventType, data string) streamEvent {
	evt := streamEvent{Type: eventType}

	switch eventType {
	case "text", "thinking":
		var payload struct {
			Text string `json:"text"`
		}
		if json.Unmarshal([]byte(data), &payload) == nil {
			evt.Text = payload.Text
		}

	case "tool_use_start":
		var payload struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if json.Unmarshal([]byte(data), &payload) == nil {
			evt.ToolID = payload.ID
			evt.ToolName = payload.Name
			evt.Type = "tool_start"
		}

	case "tool_result":
		var payload struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			Output   string `json:"output"`
			IsError  bool   `json:"is_error"`
			Duration string `json:"duration"`
		}
		if json.Unmarshal([]byte(data), &payload) == nil {
			evt.ToolID = payload.ID
			evt.ToolName = payload.Name
			evt.IsError = payload.IsError
			evt.Duration = payload.Duration
			evt.Text = payload.Output
		}

	case "tool_diff":
		var payload map[string]string
		if json.Unmarshal([]byte(data), &payload) == nil {
			evt.ToolName = payload["name"]
			evt.ToolID = payload["id"]
			evt.Diff = payload
		}

	case "done":
		var payload struct {
			SessionCost string `json:"session_cost"`
		}
		if json.Unmarshal([]byte(data), &payload) == nil {
			evt.Cost = payload.SessionCost
		}

	case "error":
		var payload struct {
			Error string `json:"error"`
		}
		if json.Unmarshal([]byte(data), &payload) == nil {
			evt.Text = payload.Error
		}
	}

	return evt
}
