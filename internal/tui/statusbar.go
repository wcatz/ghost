package tui

import (
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/wcatz/ghost/internal/ai"
)

// statusBar shows project, model, token counts, cost, and elapsed time.
type statusBar struct {
	projectName string
	modeName    string
	modelName   string
	width       int

	cost         *ai.CostTracker
	isProcessing bool
	requestStart time.Time
}

func newStatusBar(projectName, modelName string, cost *ai.CostTracker) statusBar {
	return statusBar{
		projectName: projectName,
		modelName:   modelName,
		cost:        cost,
	}
}

func (s *statusBar) setSize(width int) {
	s.width = width
}

func (s *statusBar) startProcessing() {
	s.isProcessing = true
	s.requestStart = time.Now()
}

func (s *statusBar) stopProcessing() {
	s.isProcessing = false
}

func (s statusBar) view() string {
	left := statusProjectStyle.Render(s.projectName)
	if s.modelName != "" {
		left += statusBarStyle.Render(" ") + statusModeStyle.Render(s.modelName)
	}

	var right string
	if s.cost != nil {
		cost := s.cost.Cost()
		cacheRate := s.cost.CacheHitRate()
		savings := s.cost.Savings()

		if cost > 0 || cacheRate > 0 {
			right = statusCostStyle.Render(fmt.Sprintf("$%.4f", cost))
			if savings > 0.0001 {
				right += statusBarStyle.Render(fmt.Sprintf(" (saved $%.2f)", savings))
			}
			if cacheRate > 0 {
				right += statusBarStyle.Render(fmt.Sprintf(" cache:%.0f%%", cacheRate))
			}
		}
	}

	if s.isProcessing {
		elapsed := time.Since(s.requestStart).Round(100 * time.Millisecond)
		right += statusBarStyle.Render(fmt.Sprintf(" [%s]", elapsed))
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

// shortModelName extracts a readable model name from a full model ID.
// e.g. "claude-sonnet-4-5-20250929" → "sonnet-4.5"
func shortModelName(model string) string {
	switch {
	case strings.Contains(model, "opus-4-6"):
		return "opus-4.6"
	case strings.Contains(model, "sonnet-4-5"):
		return "sonnet-4.5"
	case strings.Contains(model, "haiku-4-5"):
		return "haiku-4.5"
	case model != "":
		return model
	default:
		return ""
	}
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
