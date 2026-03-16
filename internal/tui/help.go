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
		{"ctrl+k", "command palette"},
		{"ctrl+j / ctrl+h", "next / prev tool"},
		{"space", "expand/collapse tool output"},
		{"alt+up / alt+down", "input history"},
		{"pgup / pgdown", "scroll chat"},
		{"ctrl+space", "push to talk (Phase C)"},
		{"ctrl+c", "quit"},
		{"?", "this help"},
	}

	for _, bind := range bindings {
		k := lipgloss.NewStyle().Foreground(colorAccent).Width(22).Render(bind.key)
		d := lipgloss.NewStyle().Foreground(colorSubtle).Render(bind.desc)
		b.WriteString("  " + k + d + "\n")
	}

	b.WriteString("\n")
	cmdTitle := lipgloss.NewStyle().Bold(true).Foreground(colorGhost).Render("Slash Commands")
	b.WriteString(cmdTitle + "\n\n")

	commands := []struct{ cmd, desc string }{
		{"/memory", "list all memories"},
		{"/memory search <q>", "search memories"},
		{"/memory add", "add a manual memory"},
		{"/cost", "session cost breakdown"},
		{"/context", "show project context"},
		{"/image <path>", "send image to Claude"},
		{"/reflect", "force memory consolidation"},
		{"/clear", "clear conversation"},
		{"/switch <name>", "switch project"},
		{"/projects", "list project sessions"},
		{"/quit", "exit ghost"},
	}

	for _, cmd := range commands {
		c := lipgloss.NewStyle().Foreground(colorAccent).Width(22).Render(cmd.cmd)
		d := lipgloss.NewStyle().Foreground(colorSubtle).Render(cmd.desc)
		b.WriteString("  " + c + d + "\n")
	}

	b.WriteString("\n  " + lipgloss.NewStyle().Foreground(colorDim).Render("press any key to close"))

	boxWidth := width - 10
	if boxWidth > 60 {
		boxWidth = 60
	}
	if boxWidth < 40 {
		boxWidth = 40
	}

	return paletteBorderStyle.Width(boxWidth).Render(b.String())
}
