package tui

import (
	"fmt"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
)

// activeTool tracks a single tool call in progress.
type activeTool struct {
	id           string
	name         string
	startedAt    time.Time
	inputPreview string
}

// toolbar shows ONLY the currently-active tool as a 1-2 line fixed area.
// Completed tools are rendered inline in the chat viewport instead.
type toolbar struct {
	current *activeTool   // nil when idle
	spinner spinner.Model
	thinking      bool      // true while extended thinking is active
	thinkingStart time.Time // when thinking started
	thinkingTokens int      // estimated thinking tokens (len/4 approx)
}

func newToolbar() toolbar {
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = toolSpinnerStyle
	return toolbar{
		spinner: s,
	}
}

// startTool begins tracking a new active tool.
func (t *toolbar) startTool(id, name string) {
	t.current = &activeTool{
		id:        id,
		name:      name,
		startedAt: time.Now(),
	}
}

// updateInput appends input delta to the active tool's preview.
func (t *toolbar) updateInput(id, delta string) {
	if t.current != nil && t.current.id == id {
		t.current.inputPreview += delta
		if len(t.current.inputPreview) > 80 {
			t.current.inputPreview = t.current.inputPreview[:80] + "..."
		}
	}
}

// completeTool finishes the active tool and returns its info for inline rendering.
// Returns (name, duration, ok). ok is false if the tool wasn't active.
func (t *toolbar) completeTool(id string) (string, time.Duration, bool) {
	if t.current == nil || t.current.id != id {
		return "", 0, false
	}
	name := t.current.name
	dur := time.Since(t.current.startedAt)
	t.current = nil
	return name, dur, true
}

func (t *toolbar) setThinking(active bool) {
	if active && !t.thinking {
		t.thinkingStart = time.Now()
		t.thinkingTokens = 0
	}
	t.thinking = active
}

// addThinkingTokens accumulates estimated thinking token count.
func (t *toolbar) addThinkingTokens(n int) {
	t.thinkingTokens += n
}

// clear resets the toolbar to idle state.
func (t *toolbar) clear() {
	t.current = nil
	t.thinking = false
}

// hasActive returns true if a tool is running or thinking is active.
func (t *toolbar) hasActive() bool {
	return t.current != nil || t.thinking
}

// height returns the number of lines the toolbar will occupy.
func (t *toolbar) height() int {
	if t.current != nil {
		return 1
	}
	if t.thinking {
		return 1
	}
	return 0
}

func (t toolbar) update(msg tea.Msg) (toolbar, tea.Cmd) {
	var cmd tea.Cmd
	t.spinner, cmd = t.spinner.Update(msg)
	return t, cmd
}

// view renders the toolbar: 0 lines when idle, 1 line when active.
func (t toolbar) view() string {
	if t.thinking && t.current == nil {
		elapsed := time.Since(t.thinkingStart).Seconds()
		tokens := formatTokens(t.thinkingTokens)
		return fmt.Sprintf("   %s %s %s",
			t.spinner.View(),
			toolNameStyle.Render("thinking..."),
			toolDurationStyle.Render(fmt.Sprintf("%.1fs (%s tokens)", elapsed, tokens)),
		)
	}
	if t.current != nil {
		line := fmt.Sprintf("   %s %s",
			t.spinner.View(),
			toolNameStyle.Render(t.current.name),
		)
		if t.current.inputPreview != "" {
			preview := t.current.inputPreview
			if len(preview) > 60 {
				preview = preview[:60] + "..."
			}
			line += " " + toolDurationStyle.Render(preview)
		}
		return line
	}
	return ""
}
