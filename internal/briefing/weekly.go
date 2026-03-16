package briefing

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/mdv2"
)

// WeeklyDigestSources holds data needed for a weekly digest.
type WeeklyDigestSources struct {
	Sources         // embed daily briefing sources
	RepoPath string // git repo path for commit analysis
}

// GenerateWeekly builds a weekly status digest for a project.
func GenerateWeekly(ctx context.Context, src WeeklyDigestSources, days int) string {
	if days <= 0 {
		days = 7
	}

	var sb strings.Builder
	sb.WriteString("*📊 Ghost Weekly Digest*\n")
	since := time.Now().AddDate(0, 0, -days)
	fmt.Fprintf(&sb, "_%s — %s_\n\n",
		mdv2.Esc(since.Format("Jan 2")),
		mdv2.Esc(time.Now().Format("Jan 2, 2006")))

	// Git activity.
	if src.RepoPath != "" {
		writeGitActivity(ctx, &sb, src.RepoPath, days)
	}

	// GitHub notifications summary.
	if src.GitHub != nil {
		writeGitHub(ctx, &sb, src.GitHub)
	}

	// Reminders summary.
	if src.Scheduler != nil {
		writeReminders(ctx, &sb, src.Scheduler)
	}

	if sb.Len() < 60 {
		sb.WriteString("_No notable activity this week\\._\n")
	}

	return sb.String()
}

// writeGitActivity adds recent git commit summary to the digest.
func writeGitActivity(ctx context.Context, sb *strings.Builder, repoPath string, days int) {
	sinceDate := time.Now().AddDate(0, 0, -days).Format("2006-01-02")

	// Get commit count and summary.
	cmd := exec.CommandContext(ctx, "git", "log",
		"--oneline",
		"--since="+sinceDate,
	)
	cmd.Dir = repoPath
	out, err := cmd.Output()
	if err != nil {
		return
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 0 || (len(lines) == 1 && lines[0] == "") {
		return
	}

	sb.WriteString(fmt.Sprintf("*🔀 Git Activity* \\(%d commits\\)\n", len(lines)))

	// Count by conventional commit prefix.
	counts := map[string]int{}
	for _, line := range lines {
		parts := strings.SplitN(line, " ", 2)
		if len(parts) < 2 {
			continue
		}
		msg := parts[1]
		prefix := "other"
		for _, p := range []string{"feat", "fix", "refactor", "chore", "docs", "test", "perf"} {
			if strings.HasPrefix(msg, p+"(") || strings.HasPrefix(msg, p+":") {
				prefix = p
				break
			}
		}
		counts[prefix]++
	}

	var breakdown []string
	for _, p := range []string{"feat", "fix", "refactor", "chore", "docs", "test", "perf", "other"} {
		if c, ok := counts[p]; ok {
			breakdown = append(breakdown, fmt.Sprintf("%s: %d", p, c))
		}
	}
	if len(breakdown) > 0 {
		sb.WriteString(fmt.Sprintf("  %s\n", mdv2.Esc(strings.Join(breakdown, " | "))))
	}

	// Show last 5 commits.
	show := lines
	if len(show) > 5 {
		show = show[:5]
	}
	for _, line := range show {
		sb.WriteString(fmt.Sprintf("  • %s\n", mdv2.Esc(line)))
	}
	sb.WriteString("\n")
}
