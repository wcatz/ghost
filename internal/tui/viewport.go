package tui

import (
	"strings"
	"time"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// chatViewport wraps a bubbles viewport with message management.
type chatViewport struct {
	viewport   viewport.Model
	messages   []chatMessage
	rendered   []string // cached rendered output per message
	renderer   *messageRenderer
	autoScroll bool
	width      int
	height     int

	// lineMap maps viewport line numbers to message indices for click handling.
	// Rebuilt on every rebuildContent call.
	lineMap []int
}

func newChatViewport(width, height int) chatViewport {
	vp := viewport.New(
		viewport.WithWidth(width),
		viewport.WithHeight(height),
	)
	vp.MouseWheelEnabled = true

	return chatViewport{
		viewport:   vp,
		renderer:   newMessageRenderer(width),
		autoScroll: true,
		width:      width,
		height:     height,
	}
}

func (cv *chatViewport) setSize(width, height int) {
	cv.width = width
	cv.height = height
	cv.viewport.SetWidth(width)
	cv.viewport.SetHeight(height)
	cv.renderer.setWidth(width)
	// Invalidate all caches on resize.
	cv.rendered = nil
	cv.rerenderAll()
}

// addMessage appends a new message and renders it.
func (cv *chatViewport) addMessage(msg chatMessage) {
	// If the last message was streaming, finalize its cache.
	if len(cv.messages) > 0 && len(cv.rendered) == len(cv.messages)-1 {
		last := cv.messages[len(cv.messages)-1]
		cv.rendered = append(cv.rendered, cv.renderer.render(last))
	}
	cv.messages = append(cv.messages, msg)
	cv.rendered = append(cv.rendered, cv.renderer.render(msg))
	cv.rebuildContent()
}

// addToolBlock appends a completed tool result as an inline block in the chat.
func (cv *chatViewport) addToolBlock(toolID, toolName string, duration time.Duration, output string, isError, denied bool) {
	msg := chatMessage{
		kind:         msgToolBlock,
		toolID:       toolID,
		toolName:     toolName,
		toolDuration: duration,
		toolOutput:   output,
		toolIsError:  isError,
		toolDenied:   denied,
		toolExpanded: false,
	}
	cv.addMessage(msg)
}

// addThinkingBlock appends a completed thinking block as an inline block in the chat.
func (cv *chatViewport) addThinkingBlock(text string) {
	if text == "" {
		return
	}
	msg := chatMessage{
		kind:             msgThinkingBlock,
		thinkingText:     text,
		thinkingExpanded: false,
	}
	cv.addMessage(msg)
}

// toggleBlockAtLine toggles expand/collapse for a tool or thinking block
// at the given viewport line number (relative to viewport top). Returns true if toggled.
func (cv *chatViewport) toggleBlockAtLine(viewportLine int) bool {
	// Convert viewport-relative line to absolute content line.
	absLine := cv.viewport.YOffset() + viewportLine
	if absLine < 0 || absLine >= len(cv.lineMap) {
		return false
	}
	msgIdx := cv.lineMap[absLine]
	if msgIdx < 0 || msgIdx >= len(cv.messages) {
		return false
	}
	return cv.toggleBlock(msgIdx)
}

// toggleBlock toggles expand/collapse on a tool or thinking block by message index.
func (cv *chatViewport) toggleBlock(msgIdx int) bool {
	if msgIdx < 0 || msgIdx >= len(cv.messages) {
		return false
	}
	msg := &cv.messages[msgIdx]
	switch msg.kind {
	case msgToolBlock:
		msg.toolExpanded = !msg.toolExpanded
	case msgThinkingBlock:
		msg.thinkingExpanded = !msg.thinkingExpanded
	default:
		return false
	}
	// Re-render this specific message and update the cache.
	if msgIdx < len(cv.rendered) {
		cv.rendered[msgIdx] = cv.renderer.render(*msg)
	}
	cv.rebuildContent()
	return true
}

// appendToLastAssistant appends a text delta to the last assistant message.
// Only re-renders the last message (not the entire history).
func (cv *chatViewport) appendToLastAssistant(delta string) {
	if len(cv.messages) == 0 || cv.messages[len(cv.messages)-1].kind != msgAssistant {
		cv.messages = append(cv.messages, chatMessage{kind: msgAssistant})
	}
	last := &cv.messages[len(cv.messages)-1]
	last.raw += delta
	cv.rebuildContent()
}

// startNewAssistantMessage begins a fresh assistant message.
func (cv *chatViewport) startNewAssistantMessage() {
	// Cache the previous last message if needed.
	if len(cv.messages) > 0 && len(cv.rendered) < len(cv.messages) {
		last := cv.messages[len(cv.messages)-1]
		cv.rendered = append(cv.rendered, cv.renderer.render(last))
	}
	cv.messages = append(cv.messages, chatMessage{kind: msgAssistant})
}

// rebuildContent assembles the viewport from cached renders + live last message.
func (cv *chatViewport) rebuildContent() {
	var b strings.Builder
	cv.lineMap = nil

	// Use cached renders for all but the last message.
	limit := len(cv.messages) - 1
	if limit > len(cv.rendered) {
		limit = len(cv.rendered)
	}
	for i := 0; i < limit; i++ {
		text := cv.rendered[i]
		cv.appendToLineMap(&b, text, i)
		b.WriteString("\n")
		// Account for the trailing newline in lineMap.
		cv.lineMap = append(cv.lineMap, i)
	}

	// Render the last message live (it may be streaming).
	if len(cv.messages) > 0 {
		lastIdx := len(cv.messages) - 1
		last := cv.messages[lastIdx]
		text := cv.renderer.render(last)
		cv.appendToLineMap(&b, text, lastIdx)
		b.WriteString("\n")
		cv.lineMap = append(cv.lineMap, lastIdx)
	}

	cv.viewport.SetContent(b.String())
	if cv.autoScroll {
		cv.viewport.GotoBottom()
	}
}

// appendToLineMap writes text to the builder and records line->messageIndex mappings.
func (cv *chatViewport) appendToLineMap(b *strings.Builder, text string, msgIdx int) {
	lines := strings.Split(text, "\n")
	for _, line := range lines {
		cv.lineMap = append(cv.lineMap, msgIdx)
		b.WriteString(line)
		b.WriteString("\n")
	}
}

// rerenderAll re-renders everything (used after resize).
func (cv *chatViewport) rerenderAll() {
	cv.rendered = make([]string, len(cv.messages))
	for i, msg := range cv.messages {
		cv.rendered[i] = cv.renderer.render(msg)
	}
	cv.rebuildContent()
}

func (cv *chatViewport) clear() {
	cv.messages = nil
	cv.rendered = nil
	cv.lineMap = nil
	cv.viewport.SetContent("")
}

func (cv chatViewport) update(msg tea.Msg) (chatViewport, tea.Cmd) {
	var cmd tea.Cmd
	cv.viewport, cmd = cv.viewport.Update(msg)

	// If user scrolled up, disable auto-scroll. Re-enable at bottom.
	cv.autoScroll = cv.viewport.AtBottom()

	return cv, cmd
}

// welcomeView renders a centered welcome screen when chat is empty.
func (cv chatViewport) welcomeView() string {
	title := lipgloss.NewStyle().
		Foreground(colorGhost).
		Bold(true).
		Render("Ghost")

	tagline := lipgloss.NewStyle().
		Foreground(colorDim).
		Italic(true).
		Render("your personal memory daemon")

	tips := lipgloss.NewStyle().
		Foreground(colorSubtle).
		Render("enter send · shift+enter newline · ctrl+k commands · ? help")

	content := lipgloss.JoinVertical(lipgloss.Center,
		title,
		"",
		tagline,
		"",
		tips,
	)

	return lipgloss.Place(cv.width, cv.height,
		lipgloss.Center, lipgloss.Center,
		content,
	)
}

func (cv chatViewport) view() string {
	if len(cv.messages) == 0 {
		return cv.welcomeView()
	}
	return cv.viewport.View()
}
