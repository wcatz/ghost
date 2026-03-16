package tui

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
)

// activeTool tracks a tool call in progress.
type activeTool struct {
	id           string
	name         string
	startedAt    time.Time
	done         bool
	denied       bool
	duration     time.Duration
	inputPreview string
	output       string // tool result output
	isError      bool   // whether tool execution failed
	expanded     bool   // whether output is visible
}

// toolbar manages active tool progress spinners and thinking state.
type toolbar struct {
	tools           []activeTool
	spinner         spinner.Model
	thinking        bool   // true while extended thinking is active
	thinkingText    string // accumulated thinking content
	thinkingExpanded bool   // whether thinking is visible
	selectedIndex   int    // -1 = thinking, 0+ = tool index (-2 = none)
}

func newToolbar() toolbar {
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = toolSpinnerStyle
	return toolbar{
		spinner:       s,
		selectedIndex: -2, // nothing selected initially
	}
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

func (t *toolbar) setThinking(active bool) {
	t.thinking = active
	if !active {
		t.thinkingText = ""
	}
}

func (t *toolbar) appendThinking(text string) {
	t.thinkingText += text
}

func (t *toolbar) setToolOutput(id, output string, isError bool) {
	for i := range t.tools {
		if t.tools[i].id == id {
			t.tools[i].output = output
			t.tools[i].isError = isError
			return
		}
	}
}

func (t *toolbar) clear() {
	t.tools = nil
	t.thinking = false
	t.thinkingText = ""
	t.thinkingExpanded = false
	t.selectedIndex = -2
}

func (t *toolbar) toggleSelected() {
	if t.selectedIndex == -1 && t.thinking {
		t.thinkingExpanded = !t.thinkingExpanded
	} else if t.selectedIndex >= 0 && t.selectedIndex < len(t.tools) {
		t.tools[t.selectedIndex].expanded = !t.tools[t.selectedIndex].expanded
	}
}

func (t *toolbar) selectNext() {
	maxIdx := len(t.tools) - 1
	if t.thinking {
		// Can select thinking (-1) or tools (0..maxIdx)
		if t.selectedIndex < maxIdx {
			t.selectedIndex++
		}
	} else {
		// Only tools
		if t.selectedIndex < maxIdx {
			t.selectedIndex++
		}
	}
}

func (t *toolbar) selectPrev() {
	minIdx := -2
	if t.thinking {
		minIdx = -1
	} else if len(t.tools) > 0 {
		minIdx = 0
	}
	if t.selectedIndex > minIdx {
		t.selectedIndex--
	}
}

func (t *toolbar) hasActive() bool {
	if t.thinking {
		return true
	}
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
	if len(t.tools) == 0 && !t.thinking {
		return ""
	}

	var lines []string
	itemIdx := -1

	// Thinking block
	if t.thinking {
		itemIdx = -1
		selected := t.selectedIndex == itemIdx
		arrow := "▶"
		if t.thinkingExpanded {
			arrow = "▼"
		}
		prefix := "  "
		if selected {
			prefix = "> "
		}
		line := fmt.Sprintf("%s%s %s %s",
			prefix,
			arrow,
			t.spinner.View(),
			toolNameStyle.Render("thinking..."),
		)
		lines = append(lines, line)

		// Show thinking content if expanded
		if t.thinkingExpanded && t.thinkingText != "" {
			contentLines := strings.Split(strings.TrimSpace(t.thinkingText), "\n")
			for _, cLine := range contentLines {
				if len(cLine) > 100 {
					cLine = cLine[:100] + "..."
				}
				lines = append(lines, "    "+toolDurationStyle.Render(cLine))
			}
		}
	}

	// Tool blocks
	for i, tool := range t.tools {
		itemIdx = i
		selected := t.selectedIndex == itemIdx
		arrow := "▶"
		if tool.expanded {
			arrow = "▼"
		}
		prefix := "  "
		if selected {
			prefix = "> "
		}

		var line string
		if tool.denied {
			line = fmt.Sprintf("%s%s %s %s",
				prefix,
				arrow,
				toolDeniedStyle.Render("✗ "+tool.name),
				toolDeniedStyle.Render("denied"),
			)
		} else if tool.done {
			line = fmt.Sprintf("%s%s %s %s",
				prefix,
				arrow,
				toolDoneStyle.Render("✓ "+tool.name),
				toolDurationStyle.Render(tool.duration.Round(time.Millisecond).String()),
			)
		} else {
			line = fmt.Sprintf("%s%s %s %s",
				prefix,
				arrow,
				t.spinner.View(),
				toolNameStyle.Render(tool.name),
			)
			if tool.inputPreview != "" {
				line += " " + toolDurationStyle.Render(tool.inputPreview)
			}
		}
		lines = append(lines, line)

		// Show tool output if expanded and available
		if tool.expanded && tool.output != "" {
			outputStyle := toolDurationStyle
			if tool.isError {
				outputStyle = toolDeniedStyle
			}
			contentLines := strings.Split(strings.TrimSpace(tool.output), "\n")
			maxLines := 10
			for i, cLine := range contentLines {
				if i >= maxLines {
					lines = append(lines, "    "+toolDurationStyle.Render(fmt.Sprintf("... (%d more lines)", len(contentLines)-maxLines)))
					break
				}
				if len(cLine) > 100 {
					cLine = cLine[:100] + "..."
				}
				lines = append(lines, "    "+outputStyle.Render(cLine))
			}
		}
	}
	return strings.Join(lines, "\n")
}
