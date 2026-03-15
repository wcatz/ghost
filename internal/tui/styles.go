package tui

import (
	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/compat"
)

// Color palette — adapts to terminal theme via compat.AdaptiveColor.
var (
	colorPrimary   = compat.AdaptiveColor{Light: lipgloss.Color("#7D56F4"), Dark: lipgloss.Color("#AD8CFF")}
	colorSecondary = compat.AdaptiveColor{Light: lipgloss.Color("#04B575"), Dark: lipgloss.Color("#3EE8B5")}
	colorAccent    = compat.AdaptiveColor{Light: lipgloss.Color("#FF6F61"), Dark: lipgloss.Color("#FF9A8C")}
	colorSubtle    = compat.AdaptiveColor{Light: lipgloss.Color("#9B9B9B"), Dark: lipgloss.Color("#5C5C5C")}
	colorText      = compat.AdaptiveColor{Light: lipgloss.Color("#1A1A2E"), Dark: lipgloss.Color("#FFFDF5")}
	colorDim       = compat.AdaptiveColor{Light: lipgloss.Color("#A49FA5"), Dark: lipgloss.Color("#777777")}
	colorError     = compat.AdaptiveColor{Light: lipgloss.Color("#CC0000"), Dark: lipgloss.Color("#FF5555")}
	colorSuccess   = compat.AdaptiveColor{Light: lipgloss.Color("#04B575"), Dark: lipgloss.Color("#3EE8B5")}
	colorWarning   = compat.AdaptiveColor{Light: lipgloss.Color("#E5A100"), Dark: lipgloss.Color("#FFCC00")}
)

// Layout styles.
var (
	headerStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorPrimary).
			PaddingLeft(1)

	statusBarStyle = lipgloss.NewStyle().
			Foreground(colorDim).
			PaddingLeft(1).
			PaddingRight(1)

	statusProjectStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(colorSecondary)

	statusModeStyle = lipgloss.NewStyle().
			Foreground(colorPrimary)

	statusCostStyle = lipgloss.NewStyle().
			Foreground(colorWarning)
)

// Message styles.
var (
	userMsgStyle = lipgloss.NewStyle().
			Foreground(colorSecondary).
			Bold(true).
			PaddingLeft(1)

	userLabelStyle = lipgloss.NewStyle().
			Foreground(colorSecondary).
			Bold(true)

	assistantLabelStyle = lipgloss.NewStyle().
				Foreground(colorPrimary).
				Bold(true)

	errorStyle = lipgloss.NewStyle().
			Foreground(colorError).
			Bold(true).
			PaddingLeft(1)
)

// Tool styles.
var (
	toolNameStyle = lipgloss.NewStyle().
			Foreground(colorDim).
			Bold(true)

	toolSpinnerStyle = lipgloss.NewStyle().
				Foreground(colorPrimary)

	toolDoneStyle = lipgloss.NewStyle().
			Foreground(colorSuccess)

	toolDeniedStyle = lipgloss.NewStyle().
			Foreground(colorError)

	toolDurationStyle = lipgloss.NewStyle().
				Foreground(colorSubtle)
)

// Approval dialog styles.
var (
	approvalBorderStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(colorWarning).
				Padding(1, 2)

	approvalTitleStyle = lipgloss.NewStyle().
				Foreground(colorWarning).
				Bold(true)

	approvalKeyStyle = lipgloss.NewStyle().
			Foreground(colorPrimary).
			Bold(true)
)

// Command palette styles.
var (
	paletteBorderStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(colorPrimary).
				Padding(0, 1)

	paletteSelectedStyle = lipgloss.NewStyle().
				Foreground(colorPrimary).
				Bold(true)

	paletteItemStyle = lipgloss.NewStyle().
			Foreground(colorText)

	paletteDescStyle = lipgloss.NewStyle().
			Foreground(colorDim)
)

// Input area styles.
var (
	inputBorderStyle = lipgloss.NewStyle().
			Border(lipgloss.NormalBorder(), true, false, false, false).
			BorderForeground(colorSubtle)

	inputPromptStyle = lipgloss.NewStyle().
			Foreground(colorPrimary).
			Bold(true)
)
