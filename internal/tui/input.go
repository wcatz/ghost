package tui

import (
	"strings"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

const maxHistory = 100

// inputArea wraps a bubbles textarea with input history.
type inputArea struct {
	textarea textarea.Model
	history  []string
	histIdx  int
	width    int
}

func newInputArea() inputArea {
	ta := textarea.New()
	ta.Placeholder = "Type a message..."
	ta.Prompt = "> "
	ta.ShowLineNumbers = false
	ta.CharLimit = 10000
	ta.SetHeight(2)

	// Clean up default styles.
	s := ta.Styles()
	s.Focused.CursorLine = lipgloss.NewStyle()
	s.Focused.Prompt = lipgloss.NewStyle().Foreground(colorGhost).Bold(true)
	s.Blurred.Prompt = lipgloss.NewStyle().Foreground(colorDim)
	ta.SetStyles(s)
	ta.Focus()

	return inputArea{
		textarea: ta,
		histIdx:  -1,
	}
}

func (i *inputArea) setSize(width int) {
	i.width = width
	i.textarea.SetWidth(width - 4)
}

func (i *inputArea) value() string {
	return i.textarea.Value()
}

func (i *inputArea) reset() {
	i.textarea.Reset()
	i.histIdx = -1
}

func (i *inputArea) submit() string {
	val := i.textarea.Value()
	if val == "" {
		return ""
	}
	// Add to history.
	i.history = append(i.history, val)
	if len(i.history) > maxHistory {
		i.history = i.history[1:]
	}
	i.reset()
	return val
}

func (i *inputArea) historyUp() {
	if len(i.history) == 0 {
		return
	}
	if i.histIdx == -1 {
		i.histIdx = len(i.history) - 1
	} else if i.histIdx > 0 {
		i.histIdx--
	}
	i.textarea.SetValue(i.history[i.histIdx])
}

func (i *inputArea) historyDown() {
	if i.histIdx == -1 {
		return
	}
	if i.histIdx < len(i.history)-1 {
		i.histIdx++
		i.textarea.SetValue(i.history[i.histIdx])
	} else {
		i.histIdx = -1
		i.textarea.Reset()
	}
}

func (i *inputArea) focus() {
	i.textarea.Focus()
}

func (i *inputArea) blur() {
	i.textarea.Blur()
}

func (i inputArea) update(msg tea.Msg) (inputArea, tea.Cmd) {
	var cmd tea.Cmd
	i.textarea, cmd = i.textarea.Update(msg)
	return i, cmd
}

// completionHint returns a dim hint line showing matching slash commands.
func (i inputArea) completionHint() string {
	val := i.textarea.Value()
	if !strings.HasPrefix(val, "/") || strings.Contains(val, " ") {
		return ""
	}

	var matches []string
	for _, item := range paletteCommands {
		cmd := strings.Fields(item.command)[0]
		if strings.HasPrefix(cmd, val) && cmd != val {
			matches = append(matches, cmd+" "+item.desc)
		}
		if len(matches) >= 3 {
			break
		}
	}
	if len(matches) == 0 {
		return ""
	}

	hint := strings.Join(matches, "  ·  ")
	return lipgloss.NewStyle().Foreground(colorDim).Render("  " + hint)
}

func (i inputArea) view() string {
	v := inputBorderStyle.Render(i.textarea.View())
	if hint := i.completionHint(); hint != "" {
		v += "\n" + hint
	}
	return v
}
