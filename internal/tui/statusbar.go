package tui

import (
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/wcatz/ghost/internal/ai"
)

// statusBar shows cost, savings, cache rate, context window, and elapsed time.
type statusBar struct {
	width          int
	cost           *ai.CostTracker
	estimateTokens func() int
	maxTokens      func() int
	isProcessing   bool
	requestStart   time.Time
}

func newStatusBar(cost *ai.CostTracker, estimateTokens func() int, maxTokens func() int) statusBar {
	return statusBar{
		cost:           cost,
		estimateTokens: estimateTokens,
		maxTokens:      maxTokens,
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
	var parts []string

	if s.cost != nil {
		cost := s.cost.Cost()
		cacheRate := s.cost.CacheHitRate()
		savings := s.cost.Savings()

		if cost > 0 || cacheRate > 0 {
			parts = append(parts, statusCostStyle.Render(fmt.Sprintf("$%.4f", cost)))
			if savings > 0.0001 {
				parts = append(parts, statusBarStyle.Render(fmt.Sprintf("saved $%.2f", savings)))
			}
			if cacheRate > 0 {
				parts = append(parts, statusBarStyle.Render(fmt.Sprintf("cache:%.0f%%", cacheRate)))
			}
		}
	}

	// Context window progress bar.
	if s.estimateTokens != nil && s.maxTokens != nil {
		used := s.estimateTokens()
		max := s.maxTokens()
		pct := float64(used) / float64(max)
		if pct > 0 {
			bar := renderProgressBar(pct, 10)
			parts = append(parts, bar+statusBarStyle.Render(fmt.Sprintf(" %.0f%%", pct*100)))
		}
	}

	if s.isProcessing {
		elapsed := time.Since(s.requestStart).Round(100 * time.Millisecond)
		parts = append(parts, statusBarStyle.Render(fmt.Sprintf("[%s]", elapsed)))
	}

	if len(parts) == 0 {
		return statusBarStyle.Render("")
	}

	content := strings.Join(parts, statusBarStyle.Render(" · "))
	gap := s.width - lipgloss.Width(content) - 2
	if gap < 1 {
		gap = 1
	}
	padding := strings.Repeat(" ", gap)
	return statusBarStyle.Render(padding + content)
}

// renderProgressBar renders a progress bar of barWidth chars using block characters.
// pct is 0.0–1.0. Color shifts green→yellow→red at 0.50/0.75.
func renderProgressBar(pct float64, barWidth int) string {
	if pct < 0 {
		pct = 0
	}
	if pct > 1 {
		pct = 1
	}
	filled := int(pct * float64(barWidth))

	var filledStyle lipgloss.Style
	switch {
	case pct >= 0.75:
		filledStyle = ctxBarFilledRedStyle
	case pct >= 0.50:
		filledStyle = ctxBarFilledYellowStyle
	default:
		filledStyle = ctxBarFilledGreenStyle
	}

	filledStr := filledStyle.Render(strings.Repeat("█", filled))
	emptyStr := ctxBarEmptyStyle.Render(strings.Repeat("░", barWidth-filled))
	return filledStr + emptyStr
}

// shortModelName extracts a readable model name from a full model ID.
func shortModelName(model string) string {
	switch {
	case strings.Contains(model, "opus-4-6"):
		return "opus-4.6"
	case strings.Contains(model, "sonnet-4-6"):
		return "sonnet-4.6"
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
