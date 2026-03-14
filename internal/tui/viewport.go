package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

// chatViewport wraps a bubbles viewport with message management.
type chatViewport struct {
	viewport     viewport.Model
	messages     []chatMessage
	renderer     *messageRenderer
	autoScroll   bool
	width        int
	height       int
}

func newChatViewport(width, height int) chatViewport {
	vp := viewport.New(width, height)
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
	cv.viewport.Width = width
	cv.viewport.Height = height
	cv.renderer.setWidth(width)
	cv.rerender()
}

// addMessage appends a new message and re-renders.
func (cv *chatViewport) addMessage(msg chatMessage) {
	cv.messages = append(cv.messages, msg)
	cv.rerender()
}

// updateLastAssistant updates the last assistant message's raw text
// (used during streaming to accumulate deltas).
func (cv *chatViewport) updateLastAssistant(text string) {
	if len(cv.messages) == 0 {
		cv.messages = append(cv.messages, chatMessage{kind: msgAssistant})
	}
	last := &cv.messages[len(cv.messages)-1]
	if last.kind != msgAssistant {
		cv.messages = append(cv.messages, chatMessage{kind: msgAssistant})
		last = &cv.messages[len(cv.messages)-1]
	}
	last.raw = text
	cv.rerender()
}

// appendToLastAssistant appends a text delta to the last assistant message.
func (cv *chatViewport) appendToLastAssistant(delta string) {
	if len(cv.messages) == 0 || cv.messages[len(cv.messages)-1].kind != msgAssistant {
		cv.messages = append(cv.messages, chatMessage{kind: msgAssistant})
	}
	last := &cv.messages[len(cv.messages)-1]
	last.raw += delta
	cv.rerender()
}

// startNewAssistantMessage begins a fresh assistant message.
func (cv *chatViewport) startNewAssistantMessage() {
	cv.messages = append(cv.messages, chatMessage{kind: msgAssistant})
}

func (cv *chatViewport) rerender() {
	var b strings.Builder
	for _, msg := range cv.messages {
		b.WriteString(cv.renderer.render(msg))
		b.WriteString("\n")
	}
	cv.viewport.SetContent(b.String())
	if cv.autoScroll {
		cv.viewport.GotoBottom()
	}
}

func (cv *chatViewport) clear() {
	cv.messages = nil
	cv.viewport.SetContent("")
}

func (cv chatViewport) update(msg tea.Msg) (chatViewport, tea.Cmd) {
	var cmd tea.Cmd
	cv.viewport, cmd = cv.viewport.Update(msg)

	// If user scrolled up, disable auto-scroll. Re-enable at bottom.
	cv.autoScroll = cv.viewport.AtBottom()

	return cv, cmd
}

func (cv chatViewport) view() string {
	return cv.viewport.View()
}
