package tui

import (
	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/compat"
)

// Color palette — cyan/teal theme matching the ghost gopher logo.
var (
	colorPrimary   = compat.AdaptiveColor{Light: lipgloss.Color("#0097A7"), Dark: lipgloss.Color("#00D4AA")}
	colorBright    = compat.AdaptiveColor{Light: lipgloss.Color("#00ACC1"), Dark: lipgloss.Color("#5CE0D6")}
	colorSecondary = compat.AdaptiveColor{Light: lipgloss.Color("#00897B"), Dark: lipgloss.Color("#4DB6AC")}
	colorAccent    = compat.AdaptiveColor{Light: lipgloss.Color("#FF8A65"), Dark: lipgloss.Color("#FFAB91")}
	colorSubtle    = compat.AdaptiveColor{Light: lipgloss.Color("#90A4AE"), Dark: lipgloss.Color("#546E7A")}
	colorText      = compat.AdaptiveColor{Light: lipgloss.Color("#1A1A2E"), Dark: lipgloss.Color("#ECEFF1")}
	colorDim       = compat.AdaptiveColor{Light: lipgloss.Color("#B0BEC5"), Dark: lipgloss.Color("#607D8B")}
	colorError     = compat.AdaptiveColor{Light: lipgloss.Color("#D32F2F"), Dark: lipgloss.Color("#EF5350")}
	colorSuccess   = compat.AdaptiveColor{Light: lipgloss.Color("#2E7D32"), Dark: lipgloss.Color("#66BB6A")}
	colorWarning   = compat.AdaptiveColor{Light: lipgloss.Color("#F57F17"), Dark: lipgloss.Color("#FFD54F")}
	colorGhost     = compat.AdaptiveColor{Light: lipgloss.Color("#00838F"), Dark: lipgloss.Color("#00E5CC")}
)

// Layout styles.
var (
	headerStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorGhost).
			PaddingLeft(1)

	headerDividerStyle = lipgloss.NewStyle().
				Foreground(colorSubtle)

	statusBarStyle = lipgloss.NewStyle().
			Foreground(colorDim).
			PaddingLeft(1).
			PaddingRight(1)

	statusProjectStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(colorBright)

	statusModeStyle = lipgloss.NewStyle().
			Foreground(colorSecondary)

	statusCostStyle = lipgloss.NewStyle().
			Foreground(colorWarning)

	statusTokenStyle = lipgloss.NewStyle().
				Foreground(colorDim)
)

// Message styles.
var (
	userMsgStyle = lipgloss.NewStyle().
			Foreground(colorText).
			PaddingLeft(1).
			BorderLeft(true).
			BorderStyle(lipgloss.NormalBorder()).
			BorderForeground(colorAccent)

	userLabelStyle = lipgloss.NewStyle().
			Foreground(colorAccent).
			Bold(true).
			PaddingLeft(1)

	assistantMsgStyle = lipgloss.NewStyle().
				Foreground(colorText).
				PaddingLeft(1).
				BorderLeft(true).
				BorderStyle(lipgloss.NormalBorder()).
				BorderForeground(colorGhost)

	assistantLabelStyle = lipgloss.NewStyle().
				Foreground(colorGhost).
				Bold(true).
				PaddingLeft(1)

	errorStyle = lipgloss.NewStyle().
			Foreground(colorError).
			Bold(true).
			PaddingLeft(1)
)

// Tool styles.
var (
	toolNameStyle = lipgloss.NewStyle().
			Foreground(colorBright).
			Bold(true)

	toolSpinnerStyle = lipgloss.NewStyle().
				Foreground(colorGhost)

	toolDoneStyle = lipgloss.NewStyle().
			Foreground(colorSuccess).
			Bold(true)

	toolDeniedStyle = lipgloss.NewStyle().
			Foreground(colorError).
			Bold(true)

	toolDurationStyle = lipgloss.NewStyle().
				Foreground(colorDim)

	toolBlockCollapsedStyle = lipgloss.NewStyle().
					Foreground(colorSubtle).
					PaddingLeft(2)

	toolBlockExpandedStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(colorSubtle).
				Padding(0, 1).
				MarginLeft(2)
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
			Foreground(colorGhost).
			Bold(true)
)

// Command palette styles.
var (
	paletteBorderStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(colorGhost).
				Padding(0, 1)

	paletteSelectedStyle = lipgloss.NewStyle().
				Foreground(colorGhost).
				Bold(true)

	paletteItemStyle = lipgloss.NewStyle().
			Foreground(colorText)

	paletteDescStyle = lipgloss.NewStyle().
			Foreground(colorDim)
)

// Diff styles — used for colorizing unified diff output.
var (
	diffAddStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#9ece6a"))

	diffRemoveStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#f7768e"))

	diffHunkStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#7dcfff"))

	diffHeaderStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#7dcfff")).
			Bold(true)
)

// Warning style — used for truncation warnings and similar.
var warningStyle = lipgloss.NewStyle().
	Foreground(colorWarning).
	Bold(true).
	PaddingLeft(1)

// Header component styles.
var (
	headerModeStyle = lipgloss.NewStyle().
			Foreground(colorGhost).
			Bold(true).
			PaddingLeft(1)

	headerModelStyle = lipgloss.NewStyle().
				Foreground(colorSecondary)

	headerGitStyle = lipgloss.NewStyle().
			Foreground(colorDim)

	headerGitBranchStyle = lipgloss.NewStyle().
				Foreground(colorBright)

	headerGhostYoloStyle = lipgloss.NewStyle().
				Foreground(colorError).
				Bold(true)
)

// Context progress bar styles.
var (
	ctxBarFilledGreenStyle  = lipgloss.NewStyle().Foreground(colorSuccess)
	ctxBarFilledYellowStyle = lipgloss.NewStyle().Foreground(colorWarning)
	ctxBarFilledRedStyle    = lipgloss.NewStyle().Foreground(colorError)
	ctxBarEmptyStyle        = lipgloss.NewStyle().Foreground(colorSubtle)
)

// Input area styles.
var (
	inputBorderStyle = lipgloss.NewStyle().
			Border(lipgloss.NormalBorder(), true, false, false, false).
			BorderForeground(colorSubtle)

	inputPromptStyle = lipgloss.NewStyle().
			Foreground(colorGhost).
			Bold(true)
)
