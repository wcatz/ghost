package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
)

// activeTool tracks a tool call in progress.
type activeTool struct {
	id        string
	name      string
	startedAt time.Time
	done      bool
	denied    bool
	duration  time.Duration
	inputPreview string
}

// toolbar manages active tool progress spinners.
type toolbar struct {
	tools   []activeTool
	spinner spinner.Model
}

func newToolbar() toolbar {
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = toolSpinnerStyle
	return toolbar{spinner: s}
}

func (t *toolbar) addTool(id, name string) {
	t.tools = append(t.tools, activeTool{
		id:        id,
		name:      name,
		startedAt: time.Now(),
	})
}

func (t *toolbar) updateInput(id, delta string) {
	for i := range t.tools {
		if t.tools[i].id == id {
			t.tools[i].inputPreview += delta
			// Truncate preview to first 80 chars.
			if len(t.tools[i].inputPreview) > 80 {
				t.tools[i].inputPreview = t.tools[i].inputPreview[:80] + "..."
			}
		}
	}
}

func (t *toolbar) completeTool(id string) {
	for i := range t.tools {
		if t.tools[i].id == id && !t.tools[i].done {
			t.tools[i].done = true
			t.tools[i].duration = time.Since(t.tools[i].startedAt)
		}
	}
}

func (t *toolbar) denyTool(id string) {
	for i := range t.tools {
		if t.tools[i].id == id {
			t.tools[i].done = true
			t.tools[i].denied = true
			t.tools[i].duration = time.Since(t.tools[i].startedAt)
		}
	}
}

func (t *toolbar) clear() {
	t.tools = nil
}

func (t *toolbar) hasActive() bool {
	for _, tool := range t.tools {
		if !tool.done {
			return true
		}
	}
	return false
}

func (t toolbar) update(msg tea.Msg) (toolbar, tea.Cmd) {
	var cmd tea.Cmd
	t.spinner, cmd = t.spinner.Update(msg)
	return t, cmd
}

func (t toolbar) view() string {
	if len(t.tools) == 0 {
		return ""
	}

	var lines []string
	for _, tool := range t.tools {
		var line string
		if tool.denied {
			line = fmt.Sprintf("  %s %s %s",
				toolDeniedStyle.Render("✗"),
				toolNameStyle.Render(tool.name),
				toolDeniedStyle.Render("denied"),
			)
		} else if tool.done {
			line = fmt.Sprintf("  %s %s %s",
				toolDoneStyle.Render("✓"),
				toolNameStyle.Render(tool.name),
				toolDurationStyle.Render(tool.duration.Round(time.Millisecond).String()),
			)
		} else {
			line = fmt.Sprintf("  %s %s",
				t.spinner.View(),
				toolNameStyle.Render(tool.name),
			)
			if tool.inputPreview != "" {
				line += " " + toolDurationStyle.Render(tool.inputPreview)
			}
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}
