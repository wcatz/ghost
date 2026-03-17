package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/wcatz/ghost/internal/mdv2"
)

// approvalState tracks which session has a pending approval for instruction replies.
type approvalState struct {
	mu        sync.Mutex
	sessionID string // session with pending approval
	toolName  string
}

// SetServerAddr configures the Ghost server address for API calls.
func (tb *Bot) SetServerAddr(addr string) {
	tb.serverAddr = addr
}

// SetServerToken configures the bearer token for Ghost server API auth.
func (tb *Bot) SetServerToken(token string) {
	tb.serverToken = token
}

// NotifyApproval sends an approval request to all allowed Telegram users
// with Allow / Deny inline buttons. Implements server.ApprovalNotifier.
func (tb *Bot) NotifyApproval(sessionID, projectName, toolName string, input json.RawMessage) {
	// Format the input for display.
	inputStr := formatToolInput(toolName, input)

	var sb strings.Builder
	sb.WriteString("🔐 *Approval Required*\n\n")
	if projectName != "" {
		fmt.Fprintf(&sb, "Project: `%s`\n", mdv2.Esc(projectName))
	}
	fmt.Fprintf(&sb, "Tool: `%s`\n\n", mdv2.Esc(toolName))
	if inputStr != "" {
		fmt.Fprintf(&sb, "```\n%s\n```\n\n", inputStr)
	}
	sb.WriteString("_Reply to this message with text to deny with instructions_")

	text := sb.String()
	keyboard := &models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{
			{
				{Text: "✅ Allow", CallbackData: "approve:" + sessionID},
				{Text: "❌ Deny", CallbackData: "deny:" + sessionID},
			},
		},
	}

	tb.approval.mu.Lock()
	tb.approval.sessionID = sessionID
	tb.approval.toolName = toolName
	tb.approval.mu.Unlock()

	for id := range tb.allowedIDs {
		_, err := tb.bot.SendMessage(context.Background(), &bot.SendMessageParams{
			ChatID:    id,
			Text:      text,
			ParseMode: models.ParseModeMarkdown,
			ReplyMarkup: keyboard,
			LinkPreviewOptions: &models.LinkPreviewOptions{
				IsDisabled: bot.True(),
			},
		})
		if err != nil {
			tb.logger.Error("telegram approval notify", "error", err, "user_id", id)
		}
	}
}

// handleApprovalCallback handles Allow/Deny button presses.
func (tb *Bot) handleApprovalCallback(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.CallbackQuery == nil {
		return
	}

	data := update.CallbackQuery.Data
	var approved bool
	var sessionID string

	if strings.HasPrefix(data, "approve:") {
		approved = true
		sessionID = strings.TrimPrefix(data, "approve:")
	} else if strings.HasPrefix(data, "deny:") {
		approved = false
		sessionID = strings.TrimPrefix(data, "deny:")
	} else {
		return
	}

	// Call Ghost server approve endpoint.
	err := tb.callApproveAPI(sessionID, approved, "")
	if err != nil {
		tb.logger.Error("telegram approval callback", "error", err)
		_, _ = b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
			CallbackQueryID: update.CallbackQuery.ID,
			Text:            "Error: " + err.Error(),
			ShowAlert:       true,
		})
		return
	}

	// Answer callback and edit the message.
	action := "✅ Approved"
	if !approved {
		action = "❌ Denied"
	}

	_, _ = b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
		CallbackQueryID: update.CallbackQuery.ID,
		Text:            action,
	})

	// Edit the approval message to show the result.
	if update.CallbackQuery.Message.Message != nil {
		_, _ = b.EditMessageText(ctx, &bot.EditMessageTextParams{
			ChatID:    update.CallbackQuery.Message.Message.Chat.ID,
			MessageID: update.CallbackQuery.Message.Message.ID,
			Text:      update.CallbackQuery.Message.Message.Text + "\n\n" + action,
			ParseMode: models.ParseModeMarkdown,
		})
	}

	tb.approval.mu.Lock()
	tb.approval.sessionID = ""
	tb.approval.toolName = ""
	tb.approval.mu.Unlock()
}

// handleInstructionReply checks if a message is a reply to an approval prompt
// and sends it as deny-with-instructions.
func (tb *Bot) handleInstructionReply(ctx context.Context, b *bot.Bot, update *models.Update) bool {
	if update.Message == nil || update.Message.ReplyToMessage == nil {
		return false
	}

	// Check if there's a pending approval.
	tb.approval.mu.Lock()
	sessionID := tb.approval.sessionID
	tb.approval.mu.Unlock()

	if sessionID == "" {
		return false
	}

	// Check if the reply is to a message from the bot (approval message).
	if update.Message.ReplyToMessage.From == nil || !update.Message.ReplyToMessage.From.IsBot {
		return false
	}

	instructions := update.Message.Text
	if instructions == "" {
		return false
	}

	err := tb.callApproveAPI(sessionID, false, instructions)
	if err != nil {
		tb.reply(ctx, b, update, "Failed to send instructions: "+mdv2.Esc(err.Error()))
	} else {
		tb.reply(ctx, b, update, fmt.Sprintf("❌ Denied with instructions:\n_%s_", mdv2.Esc(instructions)))
	}

	tb.approval.mu.Lock()
	tb.approval.sessionID = ""
	tb.approval.toolName = ""
	tb.approval.mu.Unlock()

	return true
}

// callApproveAPI calls the Ghost server's approve endpoint.
func (tb *Bot) callApproveAPI(sessionID string, approved bool, instructions string) error {
	if tb.serverAddr == "" {
		return fmt.Errorf("server address not configured")
	}

	payload := map[string]interface{}{
		"approved": approved,
	}
	if instructions != "" {
		payload["instructions"] = instructions
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	url := fmt.Sprintf("http://%s/api/v1/sessions/%s/approve", tb.serverAddr, sessionID)
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create approve request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if tb.serverToken != "" {
		req.Header.Set("Authorization", "Bearer "+tb.serverToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("approve API call: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("approve API returned %d", resp.StatusCode)
	}
	return nil
}

// formatToolInput extracts the most relevant info from tool input for display.
func formatToolInput(toolName string, input json.RawMessage) string {
	var m map[string]interface{}
	if err := json.Unmarshal(input, &m); err != nil {
		return string(input)
	}

	switch toolName {
	case "bash":
		desc, _ := m["description"].(string)
		cmd, _ := m["command"].(string)
		if desc != "" && cmd != "" {
			if len(cmd) > 80 {
				cmd = cmd[:80] + "..."
			}
			return desc + "\n$ " + cmd
		}
		if desc != "" {
			return desc
		}
		if cmd != "" {
			return cmd
		}
	case "file_write":
		if path, ok := m["path"].(string); ok && path != "" {
			return path
		}
	case "file_edit":
		if path, ok := m["path"].(string); ok && path != "" {
			old, _ := m["old_string"].(string)
			if len(old) > 60 {
				old = old[:60] + "..."
			}
			if old != "" {
				return path + "\n- " + old
			}
			return path
		}
	case "memory_save":
		if content, ok := m["content"].(string); ok {
			return content
		}
	case "memory_search":
		if query, ok := m["query"].(string); ok {
			return query
		}
	}

	// Fallback: compact JSON, truncated.
	b, _ := json.Marshal(m)
	s := string(b)
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}
