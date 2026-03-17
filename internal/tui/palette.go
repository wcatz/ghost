package tui

import (
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
)

// paletteItem is a command available in the palette.
type paletteItem struct {
	command string
	desc    string
}

var paletteCommands = []paletteItem{
	{"/model", "Switch model (sonnet/haiku/opus)"},
	{"/continue", "Continue from where assistant left off"},
	{"/compact", "Compress conversation history"},
	{"/tokens", "Show token estimates and cache stats"},
	{"/export", "Export conversation as markdown"},
	{"/sessions", "List all sessions with message counts"},
	{"/new", "Start a fresh session"},
	{"/resume", "Resume last session"},
	{"/memory", "List all memories (with IDs)"},
	{"/memory search", "Search memories"},
	{"/memory add", "Add a manual memory"},
	{"/memory delete", "Delete a memory by ID"},
	{"/reflect", "Force memory consolidation"},
	{"/briefing", "Ask Ghost for a status briefing"},
	{"/context", "Show project context"},
	{"/cost", "Show token usage and cost"},
	{"/image", "Send image to Claude"},
	{"/voice", "Toggle voice mode info"},
	{"/health", "Memory count, embeddings, cost"},
	{"/history", "Conversation stats"},
	{"/theme", "Switch glamour theme"},
	{"/remind", "Set a reminder (if scheduler available)"},
	{"/reminders", "List pending reminders"},
	{"/switch", "Switch active project"},
	{"/projects", "List project sessions"},
	{"/clear", "Clear conversation"},
	{"/quit", "Exit ghost"},
}

// commandPalette is a Ctrl+K fuzzy-filterable command list.
type commandPalette struct {
	active   bool
	input    textinput.Model
	filtered []paletteItem
	selected int
	width    int
}

func newCommandPalette() commandPalette {
	ti := textinput.New()
	ti.Placeholder = "Type a command..."
	ti.CharLimit = 100

	return commandPalette{
		input:    ti,
		filtered: paletteCommands,
	}
}

func (p *commandPalette) open() {
	p.active = true
	p.input.Reset()
	p.input.Focus()
	p.selected = 0
	p.filtered = paletteCommands
}

func (p *commandPalette) close() {
	p.active = false
	p.input.Blur()
}

func (p *commandPalette) setWidth(width int) {
	p.width = width
	p.input.SetWidth(width - 10)
}

func (p *commandPalette) filter() {
	query := strings.ToLower(p.input.Value())
	if query == "" {
		p.filtered = paletteCommands
		p.selected = 0
		return
	}

	var results []paletteItem
	for _, item := range paletteCommands {
		if strings.Contains(strings.ToLower(item.command), query) ||
			strings.Contains(strings.ToLower(item.desc), query) {
			results = append(results, item)
		}
	}
	p.filtered = results
	if p.selected >= len(p.filtered) {
		p.selected = 0
	}
}

func (p commandPalette) update(msg tea.Msg) (commandPalette, tea.Cmd) {
	if !p.active {
		return p, nil
	}

	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, keys.Cancel):
			p.close()
			return p, nil
		case msg.String() == "enter":
			if len(p.filtered) > 0 {
				selected := p.filtered[p.selected].command
				p.close()
				parts := strings.Fields(selected)
				return p, func() tea.Msg {
					return commandMsg{
						Command: parts[0],
						Args:    parts[1:],
					}
				}
			}
			return p, nil
		case msg.String() == "up":
			if p.selected > 0 {
				p.selected--
			}
			return p, nil
		case msg.String() == "down":
			if p.selected < len(p.filtered)-1 {
				p.selected++
			}
			return p, nil
		}
	}

	var cmd tea.Cmd
	p.input, cmd = p.input.Update(msg)
	p.filter()
	return p, cmd
}

func (p commandPalette) view() string {
	if !p.active {
		return ""
	}

	var b strings.Builder
	b.WriteString(p.input.View())
	b.WriteString("\n")

	maxVisible := 10
	for i, item := range p.filtered {
		if i >= maxVisible {
			break
		}
		if i == p.selected {
			b.WriteString(paletteSelectedStyle.Render(" > "+item.command) +
				"  " + paletteDescStyle.Render(item.desc) + "\n")
		} else {
			b.WriteString(paletteItemStyle.Render("   "+item.command) +
				"  " + paletteDescStyle.Render(item.desc) + "\n")
		}
	}

	boxWidth := p.width - 6
	if boxWidth < 40 {
		boxWidth = 40
	}
	if boxWidth > 80 {
		boxWidth = 80
	}

	return paletteBorderStyle.Width(boxWidth).Render(b.String())
}
