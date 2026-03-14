package tui

import "github.com/charmbracelet/lipgloss"

// Color palette — adapts to terminal theme via lipgloss.AdaptiveColor.
var (
	colorPrimary   = lipgloss.AdaptiveColor{Light: "#7D56F4", Dark: "#AD8CFF"}
	colorSecondary = lipgloss.AdaptiveColor{Light: "#04B575", Dark: "#3EE8B5"}
	colorAccent    = lipgloss.AdaptiveColor{Light: "#FF6F61", Dark: "#FF9A8C"}
	colorSubtle    = lipgloss.AdaptiveColor{Light: "#9B9B9B", Dark: "#5C5C5C"}
	colorText      = lipgloss.AdaptiveColor{Light: "#1A1A2E", Dark: "#FFFDF5"}
	colorDim       = lipgloss.AdaptiveColor{Light: "#A49FA5", Dark: "#777777"}
	colorError     = lipgloss.AdaptiveColor{Light: "#CC0000", Dark: "#FF5555"}
	colorSuccess   = lipgloss.AdaptiveColor{Light: "#04B575", Dark: "#3EE8B5"}
	colorWarning   = lipgloss.AdaptiveColor{Light: "#E5A100", Dark: "#FFCC00"}
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
