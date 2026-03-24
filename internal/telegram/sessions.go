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
		if err := scanner.Err(); err != nil {
			select {
			case ch <- streamEvent{Type: "error", Text: "stream read error: " + err.Error()}:
			case <-ctx.Done():
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

	var sessionPrefix, modeName string
	if len(parts) == 2 {
		// /mode <mode_name> — use active session
		tb.mu.Lock()
		sid := tb.activeSession[update.Message.Chat.ID]
		tb.mu.Unlock()
		if sid == "" {
			tb.reply(ctx, b, update, "Usage: `/mode <session_id> <mode_name>`\nOr set active session with /use first\\.")
			return
		}
		sessionPrefix = shortID(sid)
		modeName = parts[1]
	} else if len(parts) >= 3 {
		sessionPrefix = parts[1]
		modeName = parts[2]
	} else {
		tb.reply(ctx, b, update, "Usage: `/mode <session_id> <mode_name>`")
		return
	}

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

// --- API helpers for session lifecycle ---

type apiProject struct {
	ID   string `json:"id"`
	Path string `json:"path"`
	Name string `json:"name"`
}

type historyMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// fetchProjects returns all known projects from the Ghost server.
func (tb *Bot) fetchProjects() ([]apiProject, error) {
	url := fmt.Sprintf("http://%s/api/v1/projects", tb.serverAddr)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
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
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("server returned %d: %s", resp.StatusCode, body)
	}
	var projects []apiProject
	if err := json.NewDecoder(resp.Body).Decode(&projects); err != nil {
		return nil, fmt.Errorf("decode projects: %w", err)
	}
	return projects, nil
}

// createSession creates a new session on the Ghost server.
func (tb *Bot) createSession(path string) (*apiSession, error) {
	payload, _ := json.Marshal(map[string]string{"path": path})
	url := fmt.Sprintf("http://%s/api/v1/sessions/", tb.serverAddr)
	req, err := http.NewRequest("POST", url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if tb.serverToken != "" {
		req.Header.Set("Authorization", "Bearer "+tb.serverToken)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("server returned %d: %s", resp.StatusCode, body)
	}
	var session apiSession
	if err := json.NewDecoder(resp.Body).Decode(&session); err != nil {
		return nil, fmt.Errorf("decode session: %w", err)
	}
	return &session, nil
}

// deleteSession stops a session on the Ghost server.
func (tb *Bot) deleteSession(sessionID string) error {
	url := fmt.Sprintf("http://%s/api/v1/sessions/%s", tb.serverAddr, sessionID)
	req, err := http.NewRequest("DELETE", url, nil)
	if err != nil {
		return err
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

// setAutoApprove enables or disables auto-approve for a session.
func (tb *Bot) setAutoApprove(sessionID string, enabled bool) error {
	payload, _ := json.Marshal(map[string]bool{"enabled": enabled})
	url := fmt.Sprintf("http://%s/api/v1/sessions/%s/auto-approve", tb.serverAddr, sessionID)
	req, err := http.NewRequest("POST", url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
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

// fetchHistory returns the conversation history for a session.
func (tb *Bot) fetchHistory(sessionID string) ([]historyMsg, error) {
	url := fmt.Sprintf("http://%s/api/v1/sessions/%s/history", tb.serverAddr, sessionID)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
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
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("server returned %d: %s", resp.StatusCode, body)
	}
	var msgs []historyMsg
	if err := json.NewDecoder(resp.Body).Decode(&msgs); err != nil {
		return nil, fmt.Errorf("decode history: %w", err)
	}
	return msgs, nil
}

// --- Session lifecycle handlers ---

// handleNew creates a new Ghost session.
func (tb *Bot) handleNew(ctx context.Context, b *bot.Bot, update *models.Update) {
	if tb.serverAddr == "" {
		tb.reply(ctx, b, update, "Ghost server not configured\\.")
		return
	}

	parts := strings.Fields(update.Message.Text)

	// /new — show project picker
	if len(parts) == 1 {
		tb.sendTyping(ctx, update)
		projects, err := tb.fetchProjects()
		if err != nil {
			tb.reply(ctx, b, update, "Error: "+mdv2.Esc(err.Error()))
			return
		}
		if len(projects) == 0 {
			tb.reply(ctx, b, update, "No projects found\\. Ghost needs at least one project with memories\\.")
			return
		}

		var sb strings.Builder
		sb.WriteString("*Start a new session*\n\n")
		var buttons [][]models.InlineKeyboardButton
		for _, p := range projects {
			fmt.Fprintf(&sb, "• `%s` — %s\n", mdv2.Esc(p.Name), mdv2.Esc(p.Path))
			buttons = append(buttons, []models.InlineKeyboardButton{
				{Text: "▶ " + p.Name, CallbackData: "new:" + p.Path},
			})
		}
		tb.replyWithKeyboard(ctx, b, update, sb.String(), buttons)
		return
	}

	// /new <path> — create directly
	path := parts[1]
	tb.sendTyping(ctx, update)
	session, err := tb.createSession(path)
	if err != nil {
		tb.reply(ctx, b, update, "Error: "+mdv2.Esc(err.Error()))
		return
	}

	chatID := update.Message.Chat.ID
	tb.mu.Lock()
	tb.activeSession[chatID] = session.ID
	tb.activeName[chatID] = session.ProjectName
	tb.mu.Unlock()

	tb.sendWithKeyboard(ctx, b, chatID,
		fmt.Sprintf("✅ Session started: *%s* \\(`%s`\\)\nSet as active — free\\-text messages go here\\.",
			mdv2.Esc(session.ProjectName), mdv2.Esc(shortID(session.ID))))
}

// handleNewCallback handles the "new:" inline button to create a session.
func (tb *Bot) handleNewCallback(ctx context.Context, b *bot.Bot, update *models.Update) {
	cb := update.CallbackQuery
	if cb == nil || cb.Message.Message == nil {
		return
	}
	path := strings.TrimPrefix(cb.Data, "new:")

	session, err := tb.createSession(path)
	if err != nil {
		_, _ = b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
			CallbackQueryID: cb.ID,
			Text:            "Error: " + err.Error(),
			ShowAlert:       true,
		})
		return
	}

	chatID := cb.Message.Message.Chat.ID
	tb.mu.Lock()
	tb.activeSession[chatID] = session.ID
	tb.activeName[chatID] = session.ProjectName
	tb.mu.Unlock()

	_, _ = b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
		CallbackQueryID: cb.ID,
		Text:            "Session started: " + session.ProjectName,
	})

	tb.sendWithKeyboard(ctx, b, chatID,
		fmt.Sprintf("✅ Session started: *%s* \\(`%s`\\)\nSet as active — free\\-text messages go here\\.",
			mdv2.Esc(session.ProjectName), mdv2.Esc(shortID(session.ID))))
}

// handleStop stops a Ghost session.
func (tb *Bot) handleStop(ctx context.Context, b *bot.Bot, update *models.Update) {
	if tb.serverAddr == "" {
		tb.reply(ctx, b, update, "Ghost server not configured\\.")
		return
	}

	parts := strings.Fields(update.Message.Text)
	chatID := update.Message.Chat.ID

	var fullID string
	if len(parts) >= 2 {
		// /stop <id>
		var err error
		fullID, err = tb.resolveSessionID(parts[1])
		if err != nil {
			tb.reply(ctx, b, update, "Session not found\\. Use /sessions to list\\.")
			return
		}
	} else {
		// /stop — use active session
		tb.mu.Lock()
		fullID = tb.activeSession[chatID]
		tb.mu.Unlock()
		if fullID == "" {
			tb.reply(ctx, b, update, "No active session\\. Use `/stop <id>` or set active with /use\\.")
			return
		}
	}

	tb.sendTyping(ctx, update)
	if err := tb.deleteSession(fullID); err != nil {
		tb.reply(ctx, b, update, "Error: "+mdv2.Esc(err.Error()))
		return
	}

	// Clean up local state.
	tb.mu.Lock()
	if tb.activeSession[chatID] == fullID {
		delete(tb.activeSession, chatID)
		delete(tb.activeName, chatID)
	}
	delete(tb.sessionCosts, fullID)
	delete(tb.autoApprove, fullID)
	tb.mu.Unlock()

	tb.sendWithKeyboard(ctx, b, chatID,
		fmt.Sprintf("✅ Session `%s` stopped\\.", mdv2.Esc(shortID(fullID))))
}

// handleYolo toggles auto-approve for a session.
func (tb *Bot) handleYolo(ctx context.Context, b *bot.Bot, update *models.Update) {
	if tb.serverAddr == "" {
		tb.reply(ctx, b, update, "Ghost server not configured\\.")
		return
	}

	parts := strings.Fields(update.Message.Text)
	chatID := update.Message.Chat.ID

	var fullID string
	if len(parts) >= 2 {
		var err error
		fullID, err = tb.resolveSessionID(parts[1])
		if err != nil {
			tb.reply(ctx, b, update, "Session not found\\. Use /sessions to list\\.")
			return
		}
	} else {
		tb.mu.Lock()
		fullID = tb.activeSession[chatID]
		tb.mu.Unlock()
		if fullID == "" {
			tb.reply(ctx, b, update, "No active session\\. Use `/yolo <id>` or set active with /use\\.")
			return
		}
	}

	tb.mu.Lock()
	current := tb.autoApprove[fullID]
	tb.mu.Unlock()
	newState := !current

	tb.sendTyping(ctx, update)
	if err := tb.setAutoApprove(fullID, newState); err != nil {
		tb.reply(ctx, b, update, "Error: "+mdv2.Esc(err.Error()))
		return
	}

	tb.mu.Lock()
	tb.autoApprove[fullID] = newState
	tb.mu.Unlock()

	if newState {
		tb.reply(ctx, b, update, "🔓 Auto\\-approve *enabled* — all tools run without confirmation\\.")
	} else {
		tb.reply(ctx, b, update, "🔒 Auto\\-approve *disabled* — tools require approval\\.")
	}
}

// handleHistory shows conversation history for a session.
func (tb *Bot) handleHistory(ctx context.Context, b *bot.Bot, update *models.Update) {
	if tb.serverAddr == "" {
		tb.reply(ctx, b, update, "Ghost server not configured\\.")
		return
	}

	parts := strings.Fields(update.Message.Text)
	chatID := update.Message.Chat.ID

	var fullID string
	if len(parts) >= 2 {
		var err error
		fullID, err = tb.resolveSessionID(parts[1])
		if err != nil {
			tb.reply(ctx, b, update, "Session not found\\. Use /sessions to list\\.")
			return
		}
	} else {
		tb.mu.Lock()
		fullID = tb.activeSession[chatID]
		tb.mu.Unlock()
		if fullID == "" {
			tb.reply(ctx, b, update, "No active session\\. Use `/history <id>` or set active with /use\\.")
			return
		}
	}

	tb.sendTyping(ctx, update)
	msgs, err := tb.fetchHistory(fullID)
	if err != nil {
		tb.reply(ctx, b, update, "Error: "+mdv2.Esc(err.Error()))
		return
	}

	if len(msgs) == 0 {
		tb.reply(ctx, b, update, "No messages yet in this session\\.")
		return
	}

	// Show last 10 messages.
	start := 0
	if len(msgs) > 10 {
		start = len(msgs) - 10
	}

	var sb strings.Builder
	for _, m := range msgs[start:] {
		icon := "👤"
		if m.Role == "assistant" {
			icon = "🤖"
		}
		content := m.Content
		if len(content) > 300 {
			content = content[:300] + "..."
		}
		fmt.Fprintf(&sb, "%s %s\n\n", icon, content)
	}

	// Send as plain text — content may contain arbitrary characters.
	text := sb.String()
	if len(text) > telegramMsgMax {
		text = text[:telegramMsgMax-3] + "..."
	}
	_, err = b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: chatID,
		Text:   text,
	})
	if err != nil {
		tb.logger.Error("send history", "error", err, "chat_id", chatID)
	}
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
