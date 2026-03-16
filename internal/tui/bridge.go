package tui

import (
	tea "charm.land/bubbletea/v2"
	"github.com/wcatz/ghost/internal/ai"
	"github.com/wcatz/ghost/internal/provider"
)

// streamEventMsg wraps an ai.StreamEvent as a bubbletea message.
type streamEventMsg ai.StreamEvent

// streamDoneMsg signals the event channel has closed.
type streamDoneMsg struct{}

// approvalRequestMsg wraps a provider.ApprovalRequest as a bubbletea message.
type approvalRequestMsg provider.ApprovalRequest

// commandMsg carries a slash command to execute.
type commandMsg struct {
	Command string
	Args    []string
}

// voiceResultMsg carries the result of a push-to-talk cycle.
type voiceResultMsg struct {
	transcript string
	response   string
	err        error
}

// errorMsg carries an error to display.
type errorMsg struct{ err error }

func (e errorMsg) Error() string { return e.err.Error() }

// waitForStreamEvent returns a tea.Cmd that reads one event from the channel.
// When the channel closes, it returns streamDoneMsg.
func waitForStreamEvent(ch <-chan ai.StreamEvent) tea.Cmd {
	return func() tea.Msg {
		evt, ok := <-ch
		if !ok {
			return streamDoneMsg{}
		}
		return streamEventMsg(evt)
	}
}

// waitForApproval returns a tea.Cmd that reads one approval request from the channel.
func waitForApproval(ch <-chan provider.ApprovalRequest) tea.Cmd {
	return func() tea.Msg {
		req, ok := <-ch
		if !ok {
			return nil
		}
		return approvalRequestMsg(req)
	}
}
