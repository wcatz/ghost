package telegram

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/wcatz/ghost/internal/mdv2"
)

type apiSession struct {
	ID          string `json:"id"`
	ProjectPath string `json:"project_path"`
	ProjectName string `json:"project_name"`
	Mode        string `json:"mode"`
	Active      bool   `json:"active"`
	Messages    int    `json:"messages"`
}

// handleSessions lists all active Ghost sessions.
func (tb *Bot) handleSessions(ctx context.Context, b *bot.Bot, update *models.Update) {
	if tb.serverAddr == "" {
		tb.reply(ctx, b, update, "Ghost server not configured\\.")
		return
	}

	tb.sendTyping(ctx, update)

	sessions, err := tb.fetchSessions()
	if err != nil {
		tb.reply(ctx, b, update, "Error fetching sessions: "+mdv2.Esc(err.Error()))
		return
	}

	if len(sessions) == 0 {
		tb.reply(ctx, b, update, "No active sessions\\.")
		return
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "*Active Sessions* \\(%d\\)\n\n", len(sessions))

	var rows [][]models.InlineKeyboardButton
	for _, s := range sessions {
		status := "🟢"
		if !s.Active {
			status = "🔴"
		}
		shortID := s.ID[:8]
		fmt.Fprintf(&sb, "%s `%s`\n  %s \\| %s \\| %d msgs\n\n",
			status, mdv2.Esc(shortID), mdv2.Esc(s.ProjectName), mdv2.Esc(s.Mode), s.Messages)
		rows = append(rows, []models.InlineKeyboardButton{
			{Text: fmt.Sprintf("💬 %s (%s)", s.ProjectName, shortID), CallbackData: "chat:" + s.ID},
		})
	}

	tb.replyWithKeyboard(ctx, b, update, sb.String(), rows)
}

// handleChatCallback handles inline button taps from /sessions.
func (tb *Bot) handleChatCallback(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.CallbackQuery == nil {
		return
	}
	sessionID := strings.TrimPrefix(update.CallbackQuery.Data, "chat:")

	// Acknowledge the button tap.
	_, _ = b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
		CallbackQueryID: update.CallbackQuery.ID,
		Text:            "Send a message to this session",
	})

	// Prompt the user to reply with a message.
	chatID := update.CallbackQuery.Message.Message.Chat.ID
	shortID := sessionID[:8]
	_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:    chatID,
		Text:      fmt.Sprintf("💬 Session `%s` selected.\nReply to this message with your prompt:", shortID),
		ParseMode: models.ParseModeMarkdown,
		ReplyMarkup: &models.ForceReply{
			ForceReply:            true,
			InputFieldPlaceholder: "Type your message...",
			Selective:             true,
		},
	})

	// Store pending chat session so the reply handler can route it.
	tb.mu.Lock()
	tb.pendingChat[chatID] = sessionID
	tb.mu.Unlock()
}

// handleChat sends a message to a specific Ghost session.
func (tb *Bot) handleChat(ctx context.Context, b *bot.Bot, update *models.Update) {
	if tb.serverAddr == "" {
		tb.reply(ctx, b, update, "Ghost server not configured\\.")
		return
	}

	text := update.Message.Text
	parts := strings.SplitN(text, " ", 3)
	if len(parts) < 3 {
		tb.reply(ctx, b, update, "Usage: `/chat <session_id> <message>`")
		return
	}

	sessionID := parts[1]
	message := parts[2]

	// Resolve short session IDs.
	sessions, err := tb.fetchSessions()
	if err != nil {
		tb.reply(ctx, b, update, "Error: "+mdv2.Esc(err.Error()))
		return
	}

	fullID := ""
	for _, s := range sessions {
		if strings.HasPrefix(s.ID, sessionID) {
			fullID = s.ID
			break
		}
	}
	if fullID == "" {
		tb.reply(ctx, b, update, "Session not found\\. Use /sessions to list\\.")
		return
	}

	tb.sendTyping(ctx, update)

	response, err := tb.sendChatMessage(fullID, message)
	if err != nil {
		tb.reply(ctx, b, update, "Error: "+mdv2.Esc(err.Error()))
		return
	}

	if response == "" {
		response = "(no response)"
	}
	// Claude's response is standard Markdown, not MarkdownV2-escaped — use plain text.
	tb.replyText(ctx, b, update, response)
}

// handlePendingChatReply routes text replies to the pending chat session.
func (tb *Bot) handlePendingChatReply(ctx context.Context, b *bot.Bot, update *models.Update) bool {
	chatID := update.Message.Chat.ID
	tb.mu.Lock()
	sessionID, ok := tb.pendingChat[chatID]
	if ok {
		delete(tb.pendingChat, chatID)
	}
	tb.mu.Unlock()
	if !ok {
		return false
	}

	message := update.Message.Text
	if message == "" {
		return false
	}

	tb.sendTyping(ctx, update)

	response, err := tb.sendChatMessage(sessionID, message)
	if err != nil {
		tb.reply(ctx, b, update, "Error: "+mdv2.Esc(err.Error()))
		return true
	}

	if response == "" {
		response = "(no response)"
	}
	// Claude's response is standard Markdown, not MarkdownV2-escaped — use plain text.
	tb.replyText(ctx, b, update, response)
	return true
}

func (tb *Bot) fetchSessions() ([]apiSession, error) {
	url := fmt.Sprintf("http://%s/api/v1/sessions", tb.serverAddr)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("create sessions request: %w", err)
	}
	if tb.serverToken != "" {
		req.Header.Set("Authorization", "Bearer "+tb.serverToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("server returned %d", resp.StatusCode)
	}

	var sessions []apiSession
	if err := json.NewDecoder(resp.Body).Decode(&sessions); err != nil {
		return nil, err
	}
	return sessions, nil
}

// sendChatMessage sends a message to a Ghost session and collects the streamed response.
func (tb *Bot) sendChatMessage(sessionID, message string) (string, error) {
	payload, _ := json.Marshal(map[string]string{"message": message})
	url := fmt.Sprintf("http://%s/api/v1/sessions/%s/send", tb.serverAddr, sessionID)

	req, err := http.NewRequest("POST", url, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("create chat request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if tb.serverToken != "" {
		req.Header.Set("Authorization", "Bearer "+tb.serverToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("server returned %d: %s", resp.StatusCode, body)
	}

	// Parse SSE stream and collect assistant text.
	var response strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	var eventType string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") {
			eventType = strings.TrimPrefix(line, "event: ")
			continue
		}
		if strings.HasPrefix(line, "data: ") && eventType == "text" {
			data := strings.TrimPrefix(line, "data: ")
			var payload struct {
				Text string `json:"text"`
			}
			if json.Unmarshal([]byte(data), &payload) == nil {
				response.WriteString(payload.Text)
			}
		}
	}
	return response.String(), nil
}
