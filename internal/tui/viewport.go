package tui

import (
	"strings"

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

	// Use cached renders for all but the last message.
	limit := len(cv.messages) - 1
	if limit > len(cv.rendered) {
		limit = len(cv.rendered)
	}
	for i := 0; i < limit; i++ {
		b.WriteString(cv.rendered[i])
		b.WriteString("\n")
	}

	// Render the last message live (it may be streaming).
	if len(cv.messages) > 0 {
		last := cv.messages[len(cv.messages)-1]
		b.WriteString(cv.renderer.render(last))
		b.WriteString("\n")
	}

	cv.viewport.SetContent(b.String())
	if cv.autoScroll {
		cv.viewport.GotoBottom()
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
		Render("ghost")

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
