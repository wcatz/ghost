package tui

import (
	"strings"

	"charm.land/lipgloss/v2"
)

// renderHelp builds the help overlay showing all keybindings and commands.
func renderHelp(width int) string {
	var b strings.Builder

	title := lipgloss.NewStyle().Bold(true).Foreground(colorGhost).Render("Ghost Keybindings")
	b.WriteString(title + "\n\n")

	bindings := []struct{ key, desc string }{
		{"enter", "send message"},
		{"shift+enter", "new line"},
		{"esc", "interrupt current request"},
		{"ctrl+c", "interrupt or quit"},
		{"ctrl+d", "force quit"},
		{"ctrl+k", "command palette"},
		{"ctrl+y", "copy last code block (OSC 52)"},
		{"ctrl+j / ctrl+h", "next / prev tool"},
		{"space", "expand/collapse tool output"},
		{"alt+up / alt+down", "input history"},
		{"pgup / pgdown", "scroll chat"},
		{"ctrl+space", "push to talk"},
		{"?", "this help"},
	}

	for _, bind := range bindings {
		k := lipgloss.NewStyle().Foreground(colorAccent).Width(24).Render(bind.key)
		d := lipgloss.NewStyle().Foreground(colorSubtle).Render(bind.desc)
		b.WriteString("  " + k + d + "\n")
	}

	b.WriteString("\n")
	cmdTitle := lipgloss.NewStyle().Bold(true).Foreground(colorGhost).Render("Slash Commands")
	b.WriteString(cmdTitle + "\n\n")

	commands := []struct{ cmd, desc string }{
		{"/model <name>", "switch model (sonnet/haiku/opus)"},
		{"/continue", "continue from where left off"},
		{"/compact", "compress conversation history"},
		{"/tokens", "token estimates + cache stats"},
		{"/export", "export conversation as markdown"},
		{"/sessions", "list sessions with counts"},
		{"/new", "start fresh session"},
		{"/resume", "resume last session"},
		{"/memory", "list all memories"},
		{"/memory search <q>", "search memories"},
		{"/memory add", "add a manual memory"},
		{"/cost", "session cost breakdown"},
		{"/context", "show project context"},
		{"/image <path>", "send image to Claude"},
		{"/reflect", "force memory consolidation"},
		{"/briefing", "ask Ghost for a briefing"},
		{"/voice", "voice mode info"},
		{"/health", "memory, embeddings, cost"},
		{"/history", "conversation stats"},
		{"/theme <name>", "switch glamour theme"},
		{"/remind <t> <msg>", "set a reminder"},
		{"/reminders", "list pending reminders"},
		{"/switch <name>", "switch project"},
		{"/projects", "list project sessions"},
		{"/clear", "clear conversation"},
		{"/quit", "exit ghost"},
	}

	for _, cmd := range commands {
		c := lipgloss.NewStyle().Foreground(colorAccent).Width(22).Render(cmd.cmd)
		d := lipgloss.NewStyle().Foreground(colorSubtle).Render(cmd.desc)
		b.WriteString("  " + c + d + "\n")
	}

	b.WriteString("\n  " + lipgloss.NewStyle().Foreground(colorDim).Render("press any key to close"))

	boxWidth := width - 10
	if boxWidth > 65 {
		boxWidth = 65
	}
	if boxWidth < 40 {
		boxWidth = 40
	}

	return paletteBorderStyle.Width(boxWidth).Render(b.String())
}
