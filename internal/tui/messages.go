package tui

import (
	"fmt"
	"strings"
	"time"

	"charm.land/glamour/v2"
	"charm.land/lipgloss/v2"
	"github.com/wcatz/ghost/assets"
)

// messageType distinguishes message sources.
type messageType int

const (
	msgUser messageType = iota
	msgAssistant
	msgError
	msgWarning       // yellow warning (e.g., truncation)
	msgToolBlock     // completed tool result (inline in viewport)
	msgThinkingBlock // completed thinking block (inline in viewport)
)

// chatMessage is a rendered message in the viewport.
type chatMessage struct {
	kind     messageType
	raw      string // raw text (for assistant: accumulated markdown)
	rendered string // rendered output (unused currently — renderer handles it)

	// Tool block fields (only for msgToolBlock).
	toolID       string
	toolName     string
	toolDuration time.Duration
	toolOutput   string
	toolIsError  bool
	toolDenied   bool
	toolExpanded bool

	// Thinking block fields (only for msgThinkingBlock).
	thinkingText     string
	thinkingExpanded bool
}

// toolBlockLine returns the line index offset for the expand/collapse triangle.
// Used by the viewport to map mouse clicks to tool blocks.
type toolBlockLine struct {
	messageIndex int
}

// messageRenderer handles glamour markdown rendering.
type messageRenderer struct {
	renderer *glamour.TermRenderer
	width    int
}

func newMessageRenderer(width int) *messageRenderer {
	r, err := glamour.NewTermRenderer(
		glamour.WithStylesFromJSONBytes(assets.GhostBlueStyle),
		glamour.WithWordWrap(width-4),
	)
	if err != nil {
		// Fallback: no markdown rendering.
		return &messageRenderer{width: width}
	}
	return &messageRenderer{renderer: r, width: width}
}

// setTheme switches the glamour theme at runtime.
// Supported: "ghost-blue" (default), "dark", "light", "notty", "auto".
func (mr *messageRenderer) setTheme(name string) {
	var opts []glamour.TermRendererOption
	switch name {
	case "ghost-blue", "ghost", "":
		opts = append(opts, glamour.WithStylesFromJSONBytes(assets.GhostBlueStyle))
	case "dark":
		opts = append(opts, glamour.WithStandardStyle("dark"))
	case "light":
		opts = append(opts, glamour.WithStandardStyle("light"))
	case "notty":
		opts = append(opts, glamour.WithStandardStyle("notty"))
	case "auto":
		opts = append(opts, glamour.WithEnvironmentConfig())
	default:
		return // unknown theme, keep current
	}
	opts = append(opts, glamour.WithWordWrap(mr.width-4))
	r, err := glamour.NewTermRenderer(opts...)
	if err == nil {
		mr.renderer = r
	}
}

func (mr *messageRenderer) setWidth(width int) {
	mr.width = width
	// Re-create renderer with new width.
	r, err := glamour.NewTermRenderer(
		glamour.WithStylesFromJSONBytes(assets.GhostBlueStyle),
		glamour.WithWordWrap(width-4),
	)
	if err == nil {
		mr.renderer = r
	}
}

func (mr *messageRenderer) renderUser(text string) string {
	return userLabelStyle.Render("you") + "\n" +
		userMsgStyle.Render(text) + "\n"
}

func (mr *messageRenderer) renderAssistant(text string) string {
	if mr.renderer == nil || text == "" {
		return assistantLabelStyle.Render("ghost") + "\n" +
			text + "\n"
	}

	rendered, err := mr.renderer.Render(text)
	if err != nil {
		return assistantLabelStyle.Render("ghost") + "\n" +
			text + "\n"
	}

	return assistantLabelStyle.Render("ghost") + "\n" +
		strings.TrimRight(rendered, "\n") + "\n"
}

func (mr *messageRenderer) renderError(text string) string {
	return errorStyle.Render(fmt.Sprintf("error: %s", text)) + "\n"
}

// isDiffOutput returns true if the text looks like a unified diff.
func isDiffOutput(text string) bool {
	lines := strings.SplitN(text, "\n", 5)
	for _, line := range lines {
		if strings.HasPrefix(line, "diff --git") ||
			strings.HasPrefix(line, "--- ") ||
			strings.HasPrefix(line, "+++ ") ||
			strings.HasPrefix(line, "@@ ") {
			return true
		}
	}
	return false
}

// colorizeDiffLine applies diff-aware styling to a single line.
func colorizeDiffLine(line string) string {
	switch {
	case strings.HasPrefix(line, "diff --git"):
		return diffHeaderStyle.Render(line)
	case strings.HasPrefix(line, "@@"):
		return diffHunkStyle.Render(line)
	case strings.HasPrefix(line, "+"):
		return diffAddStyle.Render(line)
	case strings.HasPrefix(line, "-"):
		return diffRemoveStyle.Render(line)
	default:
		return toolDurationStyle.Render(line)
	}
}

func (mr *messageRenderer) renderToolBlock(msg chatMessage) string {
	arrow := "▶"
	if msg.toolExpanded {
		arrow = "▼"
	}

	var line string
	if msg.toolDenied {
		line = fmt.Sprintf("  %s %s %s",
			arrow,
			toolDeniedStyle.Render("✗ "+msg.toolName),
			toolDeniedStyle.Render("denied"),
		)
	} else {
		durStr := msg.toolDuration.Round(time.Millisecond).String()
		line = fmt.Sprintf("  %s %s %s",
			arrow,
			toolDoneStyle.Render("✓ "+msg.toolName),
			toolDurationStyle.Render(durStr),
		)
	}

	if !msg.toolExpanded || msg.toolOutput == "" {
		return line
	}

	// Render expanded output.
	outputStyle := toolDurationStyle
	if msg.toolIsError {
		outputStyle = toolDeniedStyle
	}

	diff := isDiffOutput(msg.toolOutput)

	var b strings.Builder
	b.WriteString(line)

	contentLines := strings.Split(strings.TrimSpace(msg.toolOutput), "\n")
	maxLines := 15
	for i, cLine := range contentLines {
		if i >= maxLines {
			b.WriteString("\n    " + toolDurationStyle.Render(
				fmt.Sprintf("... (%d more lines)", len(contentLines)-maxLines)))
			break
		}
		// Truncate very long lines to fit width.
		maxWidth := mr.width - 6
		if maxWidth < 40 {
			maxWidth = 40
		}
		if len(cLine) > maxWidth {
			cLine = cLine[:maxWidth] + "..."
		}
		if diff {
			b.WriteString("\n    " + colorizeDiffLine(cLine))
		} else {
			b.WriteString("\n    " + outputStyle.Render(cLine))
		}
	}

	return b.String()
}

func (mr *messageRenderer) renderThinkingBlock(msg chatMessage) string {
	arrow := "▶"
	if msg.thinkingExpanded {
		arrow = "▼"
	}

	thinkingLabel := lipgloss.NewStyle().
		Foreground(colorDim).
		Italic(true).
		Render("thinking")

	line := fmt.Sprintf("  %s %s", arrow, thinkingLabel)

	if !msg.thinkingExpanded || msg.thinkingText == "" {
		return line
	}

	var b strings.Builder
	b.WriteString(line)

	contentLines := strings.Split(strings.TrimSpace(msg.thinkingText), "\n")
	maxLines := 20
	for i, cLine := range contentLines {
		if i >= maxLines {
			b.WriteString("\n    " + toolDurationStyle.Render(
				fmt.Sprintf("... (%d more lines)", len(contentLines)-maxLines)))
			break
		}
		maxWidth := mr.width - 6
		if maxWidth < 40 {
			maxWidth = 40
		}
		if len(cLine) > maxWidth {
			cLine = cLine[:maxWidth] + "..."
		}
		b.WriteString("\n    " + toolDurationStyle.Render(cLine))
	}

	return b.String()
}

func (mr *messageRenderer) renderWarning(text string) string {
	return warningStyle.Render(text) + "\n"
}

func (mr *messageRenderer) render(msg chatMessage) string {
	switch msg.kind {
	case msgUser:
		return mr.renderUser(msg.raw)
	case msgAssistant:
		return mr.renderAssistant(msg.raw)
	case msgError:
		return mr.renderError(msg.raw)
	case msgWarning:
		return mr.renderWarning(msg.raw)
	case msgToolBlock:
		return mr.renderToolBlock(msg)
	case msgThinkingBlock:
		return mr.renderThinkingBlock(msg)
	default:
		return msg.raw
	}
}
