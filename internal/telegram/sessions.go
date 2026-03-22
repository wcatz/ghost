package telegram

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/wcatz/ghost/internal/mdv2"
	"github.com/wcatz/ghost/internal/mode"
)

// httpClient is used for short API calls to the Ghost server.
var httpClient = &http.Client{Timeout: 30 * time.Second}

// sseClient is used for SSE streams which can run for minutes during tool loops.
var sseClient = &http.Client{Timeout: 10 * time.Minute}

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
		sid := shortID(s.ID)
		fmt.Fprintf(&sb, "%s `%s`\n  %s \\| %s \\| %d msgs\n\n",
			status, mdv2.Esc(sid), mdv2.Esc(s.ProjectName), mdv2.Esc(s.Mode), s.Messages)
		rows = append(rows, []models.InlineKeyboardButton{
			{Text: fmt.Sprintf("💬 %s (%s)", s.ProjectName, sid), CallbackData: "chat:" + s.ID},
			{Text: "📌 Set active", CallbackData: "use:" + s.ID},
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
	if update.CallbackQuery.Message.Message == nil {
		return
	}
	chatID := update.CallbackQuery.Message.Message.Chat.ID
	shortID := shortID(sessionID)
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

// handleChat sends a message to a specific Ghost session with rich streaming.
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

	var fullID, projectName string
	for _, s := range sessions {
		if strings.HasPrefix(s.ID, sessionID) {
			fullID = s.ID
			projectName = s.ProjectName
			break
		}
	}
	if fullID == "" {
		tb.reply(ctx, b, update, "Session not found\\. Use /sessions to list\\.")
		return
	}

	chatID := update.Message.Chat.ID
	tb.streamToSession(ctx, b, chatID, fullID, projectName, message)
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

	// Look up project name for the session.
	projectName := shortID(sessionID)
	if sessions, err := tb.fetchSessions(); err == nil {
		for _, s := range sessions {
			if s.ID == sessionID {
				projectName = s.ProjectName
				break
			}
		}
	}

	tb.streamToSession(ctx, b, chatID, sessionID, projectName, message)
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
	resp, err := httpClient.Do(req)
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

// streamChatMessage opens an SSE connection to a Ghost session and returns a channel
// of parsed stream events. The channel is closed when the stream ends.
func (tb *Bot) streamChatMessage(ctx context.Context, sessionID, message string) (<-chan streamEvent, error) {
	payload, _ := json.Marshal(map[string]string{"message": message})
	url := fmt.Sprintf("http://%s/api/v1/sessions/%s/send", tb.serverAddr, sessionID)

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("create chat request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if tb.serverToken != "" {
		req.Header.Set("Authorization", "Bearer "+tb.serverToken)
	}
	resp, err := sseClient.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("server returned %d: %s", resp.StatusCode, body)
	}

	ch := make(chan streamEvent, 64)
	go func() {
		defer close(ch)
		defer func() { _ = resp.Body.Close() }()

		scanner := bufio.NewScanner(resp.Body)
		// Allow up to 1MB per SSE line (tool outputs can be large).
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		var eventType string

		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "event: ") {
				eventType = strings.TrimPrefix(line, "event: ")
				continue
			}
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := strings.TrimPrefix(line, "data: ")

			// Skip events we don't display (tool_input_delta, tool_use_end).
			switch eventType {
			case "text", "thinking", "tool_use_start", "tool_result", "tool_diff", "done", "error":
				evt := parseStreamData(eventType, data)
				select {
				case ch <- evt:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	return ch, nil
}

// createMemory POSTs a new memory to the Ghost server API.
func (tb *Bot) createMemory(projectID, content string) (id string, merged bool, err error) {
	payload, _ := json.Marshal(map[string]interface{}{
		"project_id": projectID,
		"category":   "fact",
		"content":    content,
		"source":     "telegram",
		"importance": 0.7,
	})
	url := fmt.Sprintf("http://%s/api/v1/memories/", tb.serverAddr)

	req, err := http.NewRequest("POST", url, bytes.NewReader(payload))
	if err != nil {
		return "", false, fmt.Errorf("create memory request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if tb.serverToken != "" {
		req.Header.Set("Authorization", "Bearer "+tb.serverToken)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", false, fmt.Errorf("server returned %d: %s", resp.StatusCode, body)
	}

	var result struct {
		ID     string `json:"id"`
		Merged bool   `json:"merged"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", false, err
	}
	return result.ID, result.Merged, nil
}

// resolveSessionID resolves a short session ID prefix to a full ID.
func (tb *Bot) resolveSessionID(prefix string) (string, error) {
	sessions, err := tb.fetchSessions()
	if err != nil {
		return "", err
	}
	for _, s := range sessions {
		if strings.HasPrefix(s.ID, prefix) {
			return s.ID, nil
		}
	}
	return "", fmt.Errorf("session not found")
}

// setSessionMode calls the server API to change a session's mode.
func (tb *Bot) setSessionMode(sessionID, modeName string) (string, error) {
	payload, _ := json.Marshal(map[string]string{"mode": modeName})
	url := fmt.Sprintf("http://%s/api/v1/sessions/%s/mode", tb.serverAddr, sessionID)

	req, err := http.NewRequest("POST", url, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("create mode request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if tb.serverToken != "" {
		req.Header.Set("Authorization", "Bearer "+tb.serverToken)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("server returned %d: %s", resp.StatusCode, body)
	}

	var result struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	return result.Mode, nil
}

// handleMode lists available modes or switches a session's mode.
func (tb *Bot) handleMode(ctx context.Context, b *bot.Bot, update *models.Update) {
	if tb.serverAddr == "" {
		tb.reply(ctx, b, update, "Ghost server not configured\\.")
		return
	}

	text := update.Message.Text
	parts := strings.Fields(text)

	// /mode — list available modes
	if len(parts) == 1 {
		tb.sendTyping(ctx, update)

		// Show available modes with current session modes.
		sessions, _ := tb.fetchSessions()
		var sb strings.Builder
		sb.WriteString("*Available Modes*\n\n")
		names := make([]string, 0, len(mode.Modes))
		for name := range mode.Modes {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			m := mode.Modes[name]
			model := "Sonnet"
			if m.UseQualityModel {
				model = "Opus"
			}
			fmt.Fprintf(&sb, "• `%s` — %s\n", mdv2.Esc(name), mdv2.Esc(model))
		}
		if len(sessions) > 0 {
			sb.WriteString("\n*Session Modes*\n\n")
			for _, s := range sessions {
				sid := shortID(s.ID)
				fmt.Fprintf(&sb, "• `%s` %s → `%s`\n",
					mdv2.Esc(sid), mdv2.Esc(s.ProjectName), mdv2.Esc(s.Mode))
			}
		}
		sb.WriteString("\nSwitch: `/mode <session_id> <mode_name>`")
		tb.reply(ctx, b, update, sb.String())
		return
	}

	// /mode <session_id> <mode_name>
	if len(parts) < 3 {
		tb.reply(ctx, b, update, "Usage: `/mode <session_id> <mode_name>`")
		return
	}

	sessionPrefix := parts[1]
	modeName := parts[2]

	tb.sendTyping(ctx, update)

	fullID, err := tb.resolveSessionID(sessionPrefix)
	if err != nil {
		tb.reply(ctx, b, update, "Session not found\\. Use /sessions to list\\.")
		return
	}

	newMode, err := tb.setSessionMode(fullID, modeName)
	if err != nil {
		tb.reply(ctx, b, update, "Error: "+mdv2.Esc(err.Error()))
		return
	}

	tb.reply(ctx, b, update, fmt.Sprintf("✅ Mode set to `%s`", mdv2.Esc(newMode)))
}

// handleCost shows the cumulative cost for a session.
func (tb *Bot) handleCost(ctx context.Context, b *bot.Bot, update *models.Update) {
	if tb.serverAddr == "" {
		tb.reply(ctx, b, update, "Ghost server not configured\\.")
		return
	}

	text := update.Message.Text
	parts := strings.Fields(text)

	// /cost — show costs for all sessions
	if len(parts) == 1 {
		tb.sendTyping(ctx, update)

		sessions, err := tb.fetchSessions()
		if err != nil {
			tb.reply(ctx, b, update, "Error: "+mdv2.Esc(err.Error()))
			return
		}
		if len(sessions) == 0 {
			tb.reply(ctx, b, update, "No active sessions\\.")
			return
		}

		var sb strings.Builder
		sb.WriteString("*Session Costs*\n\n")
		tb.mu.Lock()
		for _, s := range sessions {
			sid := shortID(s.ID)
			cost, ok := tb.sessionCosts[s.ID]
			if !ok {
				cost = "no data yet"
			}
			fmt.Fprintf(&sb, "• `%s` %s — %s\n",
				mdv2.Esc(sid), mdv2.Esc(s.ProjectName), mdv2.Esc(cost))
		}
		tb.mu.Unlock()
		sb.WriteString("\n_Costs update after each chat message\\._")
		tb.reply(ctx, b, update, sb.String())
		return
	}

	// /cost <session_id>
	sessionPrefix := parts[1]

	tb.sendTyping(ctx, update)

	fullID, err := tb.resolveSessionID(sessionPrefix)
	if err != nil {
		tb.reply(ctx, b, update, "Session not found\\. Use /sessions to list\\.")
		return
	}

	tb.mu.Lock()
	cost, ok := tb.sessionCosts[fullID]
	tb.mu.Unlock()

	if !ok {
		tb.reply(ctx, b, update, "No cost data yet\\. Send a /chat message first\\.")
		return
	}

	sid := shortID(fullID)
	tb.reply(ctx, b, update, fmt.Sprintf("💰 Session `%s`: %s", mdv2.Esc(sid), mdv2.Esc(cost)))
}

// deleteMemory sends a DELETE request to remove a memory by ID.
func (tb *Bot) deleteMemory(memoryID string) error {
	url := fmt.Sprintf("http://%s/api/v1/memories/%s", tb.serverAddr, memoryID)

	req, err := http.NewRequest("DELETE", url, nil)
	if err != nil {
		return fmt.Errorf("create delete request: %w", err)
	}
	if tb.serverToken != "" {
		req.Header.Set("Authorization", "Bearer "+tb.serverToken)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("server returned %d: %s", resp.StatusCode, body)
	}
	return nil
}
