package tui

import (
	"fmt"
	"strings"

	"charm.land/glamour/v2"
	"github.com/wcatz/ghost/assets"
)

// messageType distinguishes message sources.
type messageType int

const (
	msgUser messageType = iota
	msgAssistant
	msgError
)

// chatMessage is a rendered message in the viewport.
type chatMessage struct {
	kind     messageType
	raw      string // raw text (for assistant: accumulated markdown)
	rendered string // rendered output
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

func (mr *messageRenderer) render(msg chatMessage) string {
	switch msg.kind {
	case msgUser:
		return mr.renderUser(msg.raw)
	case msgAssistant:
		return mr.renderAssistant(msg.raw)
	case msgError:
		return mr.renderError(msg.raw)
	default:
		return msg.raw
	}
}
