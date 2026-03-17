package tui

import (
	"encoding/json"
	"fmt"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/wcatz/ghost/internal/provider"
)

// approvalDialog shows a non-blocking overlay for tool approval.
type approvalDialog struct {
	active       bool
	request      provider.ApprovalRequest
	toolName     string
	summary      string
	width        int
	confirmAll   bool              // true when showing "are you sure?" for Allow All
	showDenyInput bool             // true when collecting deny reason
	denyInput    textinput.Model
}

func newApprovalDialog() approvalDialog {
	ti := textinput.New()
	ti.Placeholder = "Reason (optional, enter to skip)"
	ti.CharLimit = 200
	return approvalDialog{denyInput: ti}
}

func (a *approvalDialog) show(req provider.ApprovalRequest) {
	a.active = true
	a.request = req
	a.toolName = req.ToolName
	a.summary = extractSummary(req.Input)
	a.confirmAll = false
	a.showDenyInput = false
}

func (a *approvalDialog) respond(approved bool) {
	a.respondWith(provider.ApprovalResponse{Approved: approved})
}

func (a *approvalDialog) respondWith(resp provider.ApprovalResponse) {
	if !a.active {
		return
	}
	a.request.Response <- resp
	a.active = false
	a.showDenyInput = false
}

func (a *approvalDialog) setWidth(width int) {
	a.width = width
	a.denyInput.SetWidth(width - 20)
}

func (a approvalDialog) update(msg tea.Msg) (approvalDialog, tea.Cmd) {
	if !a.active {
		return a, nil
	}

	if msg, ok := msg.(tea.KeyPressMsg); ok {
		// Deny input mode — collecting reason text.
		if a.showDenyInput {
			switch {
			case msg.String() == "enter":
				reason := a.denyInput.Value()
				a.denyInput.Reset()
				a.respondWith(provider.ApprovalResponse{
					Approved:     false,
					Instructions: reason,
				})
			case key.Matches(msg, keys.Cancel):
				a.showDenyInput = false
				a.denyInput.Reset()
				a.denyInput.Blur()
			default:
				var cmd tea.Cmd
				a.denyInput, cmd = a.denyInput.Update(msg)
				return a, cmd
			}
			return a, nil
		}

		if a.confirmAll {
			// Second confirmation for Allow All.
			switch {
			case key.Matches(msg, keys.Approve):
				a.confirmAll = false
				a.respond(true)
				return a, func() tea.Msg {
					return commandMsg{Command: "auto-approve"}
				}
			default:
				// Any other key cancels the confirmation.
				a.confirmAll = false
			}
			return a, nil
		}

		switch {
		case key.Matches(msg, keys.Approve):
			a.respond(true)
		case key.Matches(msg, keys.Deny):
			// Show deny reason input instead of immediately denying.
			a.showDenyInput = true
			a.denyInput.Focus()
			return a, a.denyInput.Focus()
		case key.Matches(msg, keys.Cancel):
			a.respond(false)
		case key.Matches(msg, keys.ApproveAll):
			a.confirmAll = true // require second confirmation
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

	var hint string
	if a.showDenyInput {
		hint = "\nDeny reason:\n" + a.denyInput.View() +
			"\n" + lipgloss.NewStyle().Foreground(colorDim).Render("enter to deny · esc to cancel")
	} else if a.confirmAll {
		hint = fmt.Sprintf("\n%s to auto-approve ALL tools this session?",
			approvalKeyStyle.Render("[y] Confirm"),
		)
	} else {
		hint = fmt.Sprintf("\n%s Allow  %s Deny  %s Allow All",
			approvalKeyStyle.Render("[y]"),
			approvalKeyStyle.Render("[n]"),
			approvalKeyStyle.Render("[a]"),
		)
	}

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

	// Priority order: human-readable description first, then key identifiers.
	// bash: description > command; file_edit/file_write: path; memory: query/content.
	for _, field := range []string{"description", "command", "path", "query", "content", "category", "message"} {
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
