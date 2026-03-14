package tui

import (
	"fmt"

	"github.com/charmbracelet/lipgloss"
	"github.com/wcatz/ghost/internal/ai"
)

// statusBar shows project, mode, token counts, and cost.
type statusBar struct {
	projectName string
	modeName    string
	width       int

	// Cumulative session totals.
	totalInput       int
	totalOutput      int
	totalCacheCreate int
	totalCacheRead   int
	totalCostUSD     float64

	isProcessing bool
}

func newStatusBar(projectName, modeName string) statusBar {
	return statusBar{
		projectName: projectName,
		modeName:    modeName,
	}
}

func (s *statusBar) setSize(width int) {
	s.width = width
}

func (s *statusBar) updateUsage(usage *ai.TokenUsage) {
	if usage == nil {
		return
	}
	s.totalInput += usage.InputTokens
	s.totalOutput += usage.OutputTokens
	s.totalCacheCreate += usage.CacheCreationInputTokens
	s.totalCacheRead += usage.CacheReadInputTokens
	s.totalCostUSD += estimateCost(usage)
}

func (s statusBar) view() string {
	left := statusProjectStyle.Render(s.projectName) +
		statusBarStyle.Render("/") +
		statusModeStyle.Render(s.modeName)

	var right string
	if s.totalInput > 0 || s.totalOutput > 0 {
		tokens := fmt.Sprintf("in:%s out:%s",
			formatTokens(s.totalInput), formatTokens(s.totalOutput))
		if s.totalCacheRead > 0 {
			tokens += fmt.Sprintf(" cached:%s", formatTokens(s.totalCacheRead))
		}
		right = statusBarStyle.Render(tokens)
		if s.totalCostUSD > 0 {
			right += " " + statusCostStyle.Render(fmt.Sprintf("$%.4f", s.totalCostUSD))
		}
	}

	if s.isProcessing {
		right += statusBarStyle.Render(" thinking...")
	}

	gap := s.width - lipgloss.Width(left) - lipgloss.Width(right) - 2
	if gap < 1 {
		gap = 1
	}

	padding := ""
	for i := 0; i < gap; i++ {
		padding += " "
	}

	return statusBarStyle.Render(left + padding + right)
}

// estimateCost estimates USD cost based on Claude Sonnet 4 pricing.
// Input: $3/MTok, Output: $15/MTok, Cache write: $3.75/MTok, Cache read: $0.30/MTok.
func estimateCost(u *ai.TokenUsage) float64 {
	if u == nil {
		return 0
	}
	input := float64(u.InputTokens) * 3.0 / 1_000_000
	output := float64(u.OutputTokens) * 15.0 / 1_000_000
	cacheWrite := float64(u.CacheCreationInputTokens) * 3.75 / 1_000_000
	cacheRead := float64(u.CacheReadInputTokens) * 0.30 / 1_000_000
	return input + output + cacheWrite + cacheRead
}

// formatTokens formats a token count as human-readable (e.g., 1234 → "1.2k").
func formatTokens(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	if n < 10000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprintf("%dk", n/1000)
}
