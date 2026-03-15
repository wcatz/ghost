package tui

import (
	"encoding/json"
	"fmt"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/wcatz/ghost/internal/provider"
)

// approvalDialog shows a non-blocking overlay for tool approval.
type approvalDialog struct {
	active   bool
	request  provider.ApprovalRequest
	toolName string
	summary  string
	width    int
}

func newApprovalDialog() approvalDialog {
	return approvalDialog{}
}

func (a *approvalDialog) show(req provider.ApprovalRequest) {
	a.active = true
	a.request = req
	a.toolName = req.ToolName
	a.summary = extractSummary(req.Input)
}

func (a *approvalDialog) respond(approved bool) {
	if !a.active {
		return
	}
	a.request.Response <- approved
	a.active = false
}

func (a *approvalDialog) setWidth(width int) {
	a.width = width
}

func (a approvalDialog) update(msg tea.Msg) (approvalDialog, tea.Cmd) {
	if !a.active {
		return a, nil
	}

	if msg, ok := msg.(tea.KeyPressMsg); ok {
		switch {
		case key.Matches(msg, keys.Approve):
			a.respond(true)
		case key.Matches(msg, keys.Deny), key.Matches(msg, keys.Cancel):
			a.respond(false)
		case key.Matches(msg, keys.ApproveAll):
			a.respond(true)
			// Return a command to set auto-approve.
			return a, func() tea.Msg {
				return commandMsg{Command: "auto-approve"}
			}
		}
	}

	return a, nil
}

func (a approvalDialog) view() string {
	if !a.active {
		return ""
	}

	title := approvalTitleStyle.Render("Tool Approval Required")
	tool := toolNameStyle.Render(a.toolName)

	var content string
	if a.summary != "" {
		content = fmt.Sprintf("%s\n\n%s\n%s", title, tool, a.summary)
	} else {
		content = fmt.Sprintf("%s\n\n%s", title, tool)
	}

	hint := fmt.Sprintf("\n%s Allow  %s Deny  %s Allow All",
		approvalKeyStyle.Render("[y]"),
		approvalKeyStyle.Render("[n]"),
		approvalKeyStyle.Render("[a]"),
	)

	boxWidth := a.width - 10
	if boxWidth < 40 {
		boxWidth = 40
	}
	if boxWidth > 70 {
		boxWidth = 70
	}

	box := approvalBorderStyle.
		Width(boxWidth).
		Render(content + hint)

	// Center the dialog.
	return lipgloss.Place(a.width, 0,
		lipgloss.Center, lipgloss.Center,
		box,
	)
}

// extractSummary pulls the most useful field from tool input JSON.
func extractSummary(input json.RawMessage) string {
	var m map[string]interface{}
	if err := json.Unmarshal(input, &m); err != nil {
		return ""
	}

	// Try common fields in priority order.
	for _, field := range []string{"command", "path", "pattern", "query", "content"} {
		if v, ok := m[field]; ok {
			s := fmt.Sprintf("%v", v)
			if len(s) > 120 {
				s = s[:120] + "..."
			}
			return strings.TrimSpace(s)
		}
	}
	return ""
}
