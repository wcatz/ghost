package tui

import (
	"strings"
	"sync"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/wcatz/ghost/assets"
)

// ghostBanner is the large ASCII art for the welcome screen.
var ghostBanner = strings.Join([]string{
	`  ██████╗ ██╗  ██╗ ██████╗ ███████╗████████╗`,
	` ██╔════╝ ██║  ██║██╔═══██╗██╔════╝╚══██╔══╝`,
	` ██║  ███╗███████║██║   ██║███████╗   ██║   `,
	` ██║   ██║██╔══██║██║   ██║╚════██║   ██║   `,
	` ╚██████╔╝██║  ██║╚██████╔╝███████║   ██║   `,
	`  ╚═════╝ ╚═╝  ╚═╝ ╚═════╝ ╚══════╝   ╚═╝   `,
}, "\n")

// ghostShadow is a small dim ghost watermark at the top of chat.
var ghostShadow = strings.Join([]string{
	`    ▄█████▄`,
	`   █████████`,
	`   ██ █ █ ██`,
	`   █████████`,
	`   █████████`,
	`    █ █ █ █`,
}, "\n")

// chatViewport wraps a bubbles viewport with message management.
type chatViewport struct {
	viewport   viewport.Model
	messages   []chatMessage
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

	// Ghost shadow watermark at the top of chat history.
	shadow := lipgloss.NewStyle().Foreground(colorSubtle).Render(ghostShadow)
	b.WriteString(lipgloss.PlaceHorizontal(cv.width, lipgloss.Center, shadow))
	b.WriteString("\n\n")

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

// cachedLogo caches the half-block logo render (computed once).
var (
	cachedLogo     string
	cachedLogoOnce sync.Once
)

func getLogoHalfBlock() string {
	cachedLogoOnce.Do(func() {
		cachedLogo = renderImageHalfBlock(assets.LogoPNG, 24)
	})
	return cachedLogo
}

// welcomeView renders a centered welcome screen when chat is empty.
func (cv chatViewport) welcomeView() string {
	// Try rendering the actual logo as half-block characters.
	logo := getLogoHalfBlock()

	var banner string
	if logo != "" {
		banner = logo
	} else {
		// Fallback to ASCII banner.
		banner = lipgloss.NewStyle().
			Foreground(colorGhost).
			Bold(true).
			Render(ghostBanner)
	}

	tagline := lipgloss.NewStyle().
		Foreground(colorDim).
		Italic(true).
		Render("memory-first coding agent")

	tips := lipgloss.NewStyle().
		Foreground(colorSubtle).
		Render("enter send · shift+enter newline · ctrl+k commands")

	content := lipgloss.JoinVertical(lipgloss.Center,
		banner,
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
