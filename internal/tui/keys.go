package tui

import "charm.land/bubbles/v2/key"

// keyMap defines all key bindings for the TUI.
type keyMap struct {
	Send        key.Binding
	NewLine     key.Binding
	Quit        key.Binding
	ForceQuit   key.Binding
	Palette     key.Binding
	ScrollUp    key.Binding
	ScrollDown  key.Binding
	PageUp      key.Binding
	PageDown    key.Binding
	Home        key.Binding
	End         key.Binding
	HistoryUp    key.Binding
	HistoryDown  key.Binding
	Approve      key.Binding
	Deny         key.Binding
	ApproveAll   key.Binding
	Cancel       key.Binding
	PushToTalk   key.Binding // Phase C — reserved
	ToolNext     key.Binding
	ToolPrev     key.Binding
	ToolToggle   key.Binding
}

var keys = keyMap{
	Send: key.NewBinding(
		key.WithKeys("enter"),
		key.WithHelp("enter", "send message"),
	),
	NewLine: key.NewBinding(
		key.WithKeys("shift+enter", "alt+enter"),
		key.WithHelp("shift+enter", "new line"),
	),
	Quit: key.NewBinding(
		key.WithKeys("ctrl+c"),
		key.WithHelp("ctrl+c", "quit"),
	),
	ForceQuit: key.NewBinding(
		key.WithKeys("ctrl+d"),
		key.WithHelp("ctrl+d", "force quit"),
	),
	Palette: key.NewBinding(
		key.WithKeys("ctrl+k"),
		key.WithHelp("ctrl+k", "command palette"),
	),
	ScrollUp: key.NewBinding(
		key.WithKeys("up"),
		key.WithHelp("up", "scroll up"),
	),
	ScrollDown: key.NewBinding(
		key.WithKeys("down"),
		key.WithHelp("down", "scroll down"),
	),
	PageUp: key.NewBinding(
		key.WithKeys("pgup"),
		key.WithHelp("pgup", "page up"),
	),
	PageDown: key.NewBinding(
		key.WithKeys("pgdown"),
		key.WithHelp("pgdown", "page down"),
	),
	Home: key.NewBinding(
		key.WithKeys("home"),
		key.WithHelp("home", "scroll to top"),
	),
	End: key.NewBinding(
		key.WithKeys("end"),
		key.WithHelp("end", "scroll to bottom"),
	),
	HistoryUp: key.NewBinding(
		key.WithKeys("alt+up"),
		key.WithHelp("alt+up", "previous input"),
	),
	HistoryDown: key.NewBinding(
		key.WithKeys("alt+down"),
		key.WithHelp("alt+down", "next input"),
	),
	Approve: key.NewBinding(
		key.WithKeys("y"),
		key.WithHelp("y", "approve"),
	),
	Deny: key.NewBinding(
		key.WithKeys("n"),
		key.WithHelp("n", "deny"),
	),
	ApproveAll: key.NewBinding(
		key.WithKeys("a"),
		key.WithHelp("a", "approve all"),
	),
	Cancel: key.NewBinding(
		key.WithKeys("esc"),
		key.WithHelp("esc", "cancel/close"),
	),
	PushToTalk: key.NewBinding(
		key.WithKeys("ctrl+space"),
		key.WithHelp("ctrl+space", "push to talk"),
	),
	ToolNext: key.NewBinding(
		key.WithKeys("ctrl+j"),
		key.WithHelp("ctrl+j", "next tool/thinking"),
	),
	ToolPrev: key.NewBinding(
		key.WithKeys("ctrl+h"),
		key.WithHelp("ctrl+h", "prev tool/thinking"),
	),
	ToolToggle: key.NewBinding(
		key.WithKeys("space"),
		key.WithHelp("space", "expand/collapse tool"),
	),
}
