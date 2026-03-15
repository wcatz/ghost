package telegram

import (
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
	for _, s := range sessions {
		status := "🟢"
		if !s.Active {
			status = "🔴"
		}
		fmt.Fprintf(&sb, "%s `%s`\n  %s \\| %s \\| %d msgs\n\n",
			status, mdv2.Esc(s.ID[:8]), mdv2.Esc(s.ProjectName), mdv2.Esc(s.Mode), s.Messages)
	}
	sb.WriteString("Use `/chat <session_id> <message>` to interact")
	tb.reply(ctx, b, update, sb.String())
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

	// Send message via API. This returns an SSE stream — we just report success.
	err = tb.sendChatMessage(fullID, message)
	if err != nil {
		tb.reply(ctx, b, update, "Error: "+mdv2.Esc(err.Error()))
		return
	}

	tb.reply(ctx, b, update, fmt.Sprintf("📤 Sent to `%s`:\n_%s_",
		mdv2.Esc(fullID[:8]), mdv2.Esc(message)))
}

func (tb *Bot) fetchSessions() ([]apiSession, error) {
	url := fmt.Sprintf("http://%s/api/v1/sessions", tb.serverAddr)
	resp, err := http.Get(url) //nolint:gosec
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

func (tb *Bot) sendChatMessage(sessionID, message string) error {
	payload, _ := json.Marshal(map[string]string{"message": message})
	url := fmt.Sprintf("http://%s/api/v1/sessions/%s/send", tb.serverAddr, sessionID)

	resp, err := http.Post(url, "application/json", bytes.NewReader(payload)) //nolint:gosec
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	// The send endpoint returns SSE — just drain it.
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("server returned %d: %s", resp.StatusCode, body)
	}
	// Drain SSE stream so the request completes.
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}
