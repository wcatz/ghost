package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/project"
	"github.com/wcatz/ghost/internal/provider"
)

// onboardProject extracts seed memories from a new project's context.
// Called in a background goroutine when CountMemories returns 0.
// Uses the fast model (Haiku) to analyze README, CLAUDE.md, file tree,
// and manifest files, producing 5-10 foundational memories.
func onboardProject(ctx context.Context, client provider.LLMProvider, store provider.MemoryStore, projCtx *project.Context, logger *slog.Logger) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// Build context string from available project signals.
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Project: %s\n", projCtx.Name))
	sb.WriteString(fmt.Sprintf("Language: %s\n", projCtx.Language))
	sb.WriteString(fmt.Sprintf("Path: %s\n", projCtx.Path))

	if projCtx.GitBranch != "" {
		sb.WriteString(fmt.Sprintf("Branch: %s\n", projCtx.GitBranch))
	}
	if projCtx.TestCommand != "" {
		sb.WriteString(fmt.Sprintf("Test command: %s\n", projCtx.TestCommand))
	}
	if projCtx.LintCommand != "" {
		sb.WriteString(fmt.Sprintf("Lint command: %s\n", projCtx.LintCommand))
	}

	if projCtx.ReadmeSummary != "" {
		sb.WriteString(fmt.Sprintf("\n## README\n%s\n", projCtx.ReadmeSummary))
	}
	if projCtx.ClaudeMD != "" {
		claudeMD := projCtx.ClaudeMD
		if len(claudeMD) > 3000 {
			claudeMD = claudeMD[:3000]
		}
		sb.WriteString(fmt.Sprintf("\n## CLAUDE.md\n%s\n", claudeMD))
	}
	if projCtx.FileTree != "" {
		sb.WriteString(fmt.Sprintf("\n## File tree\n%s\n", projCtx.FileTree))
	}

	if sb.Len() < 50 {
		logger.Debug("onboarding skipped: insufficient project context", "project", projCtx.Name)
		return
	}

	prompt := fmt.Sprintf(`Analyze this project and extract 5-10 important memories.
For each memory, output one line in this exact format:
CATEGORY: content

Categories must be one of: architecture, convention, pattern, dependency, fact

Focus on:
- What this project does (architecture)
- Key technologies and dependencies (dependency)
- Project conventions visible from the file structure (convention)
- Notable patterns in the code organization (pattern)
- Important facts about build, test, or deploy (fact)

Do NOT include speculative or generic observations. Only extract what is clearly evident from the provided context.

---
%s`, sb.String())

	response, _, err := client.Reflect(ctx, prompt)
	if err != nil {
		logger.Warn("onboarding failed", "project", projCtx.Name, "error", err)
		return
	}

	// Parse response lines.
	saved := 0
	for _, line := range strings.Split(response, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.Contains(line, ":") {
			continue
		}

		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}

		category := strings.TrimSpace(strings.ToLower(parts[0]))
		content := strings.TrimSpace(parts[1])

		// Validate category.
		validCategories := map[string]bool{
			"architecture": true, "convention": true, "pattern": true,
			"dependency": true, "fact": true,
		}
		if !validCategories[category] {
			continue
		}
		if len(content) < 10 {
			continue
		}

		_, _, err := store.Upsert(ctx, projCtx.ID, category, content, "onboarding", 0.8, []string{"auto"})
		if err != nil {
			logger.Warn("onboarding save failed", "error", err)
			continue
		}
		saved++
	}

	if saved > 0 {
		logger.Info("project onboarded", "project", projCtx.Name, "memories", saved)
	}
}
