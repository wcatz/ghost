package tui

import (
	"bytes"
	"os/exec"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/wcatz/ghost/internal/ai"
	"github.com/wcatz/ghost/internal/provider"
)

// gitInfo holds the current git state for the project directory.
type gitInfo struct {
	branch string
	dirty  bool
	ahead  int
	behind int
	err    error
}

// gitInfoMsg carries a fetched gitInfo back to the Update loop.
type gitInfoMsg gitInfo

// fetchGitInfo returns a tea.Cmd that runs git queries non-blocking.
func fetchGitInfo(projectPath string) tea.Cmd {
	return func() tea.Msg {
		branch, err := runGit(projectPath, "rev-parse", "--abbrev-ref", "HEAD")
		if err != nil {
			return gitInfoMsg{err: err}
		}
		info := gitInfo{branch: branch}

		// Parse ahead/behind and dirty from `git status --porcelain=v1 -b`.
		status, err := runGit(projectPath, "status", "--porcelain=v1", "-b")
		if err != nil {
			return gitInfoMsg{branch: branch}
		}
		for _, line := range strings.Split(status, "\n") {
			if strings.HasPrefix(line, "## ") {
				// e.g. "## main...origin/main [ahead 1, behind 2]"
				if i := strings.Index(line, "["); i != -1 {
					counts := line[i+1 : strings.Index(line, "]")]
					for _, part := range strings.Split(counts, ", ") {
						part = strings.TrimSpace(part)
						if strings.HasPrefix(part, "ahead ") {
							info.ahead, _ = strconv.Atoi(strings.TrimPrefix(part, "ahead "))
						} else if strings.HasPrefix(part, "behind ") {
							info.behind, _ = strconv.Atoi(strings.TrimPrefix(part, "behind "))
						}
					}
				}
			} else if len(line) >= 2 && line[0] != '?' {
				info.dirty = true
			}
		}
		return gitInfoMsg(info)
	}
}

func runGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.String()), nil
}

// completedToolInfo holds metadata for a tool that finished execution,
// pending its tool_result event to be rendered inline in the viewport.
type completedToolInfo struct {
	name     string
	duration time.Duration
}

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

// imagePasteMsg wraps a base64 image pasted into the TUI.
type imagePasteMsg struct {
	mediaType string
	data      string
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
