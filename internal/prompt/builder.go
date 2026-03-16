package prompt

import (
	"context"
	"fmt"
	"strings"

	"github.com/wcatz/ghost/internal/ai"
	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/mode"
	"github.com/wcatz/ghost/internal/project"
)

const staticPersonality = `You are Ghost, a memory-first personal assistant. You remember project context, decisions, patterns, and preferences across sessions.

<tools>
You have 6 tools available. Use them — do not guess when you can look.

1. file_read — Read a file's contents with line numbers. Use offset/limit for large files.
2. grep — Search file contents using regex patterns. Filter by glob, set context lines.
3. glob — Find files matching a glob pattern. Returns paths sorted by modification time.
4. git — Run git commands. Read ops auto-approved. Write ops need confirmation. Destructive ops blocked.
5. memory_save — Save a memory (architecture, decision, pattern, convention, gotcha, dependency, preference, fact). Use when you learn something important.
6. memory_search — Search project memories by keyword. Filter by category.
</tools>

<rules>
- If unsure, verify. Use file_read, grep, or glob before answering questions about code.
- Do not fabricate file paths, function names, or API signatures. Read the source first.
- When you learn something important and non-sensitive, use memory_save to persist it.
- Never persist secrets (passwords, API keys, tokens, private keys) unless explicitly asked.
- If you do not know something and cannot verify it with your tools, say "I don't know."
</rules>

<response-style>
- Be direct. Lead with the answer.
- Reference remembered context when relevant.
- Brief answers unless asked to elaborate.
- When citing code, include the file path and line number.
</response-style>`

// memoryQuerier is the subset of provider.MemoryStore that Builder needs.
type memoryQuerier interface {
	GetTopMemories(ctx context.Context, projectID string, limit int) ([]memory.Memory, error)
	GetLearnedContext(ctx context.Context, projectID string) (string, error)
}

// Builder constructs system prompts with 3-block caching.
type Builder struct {
	store memoryQuerier
}

// NewBuilder creates a prompt builder.
func NewBuilder(store memoryQuerier) *Builder {
	return &Builder{store: store}
}

// BuildSystemBlocks constructs the 3-block system prompt.
// Block 1: static personality (cached)
// Block 2: project context + mode (cached when stable)
// Block 3: memories + recent git (dynamic per-request)
func (b *Builder) BuildSystemBlocks(ctx context.Context, projCtx *project.Context, currentMode mode.Mode) []ai.SystemBlock {
	blocks := make([]ai.SystemBlock, 0, 3)

	// Block 1: Static personality — cached across all requests.
	blocks = append(blocks, ai.CachedBlock(staticPersonality))

	// Block 2: Project context + mode — cached when stable.
	var block2 strings.Builder
	block2.WriteString(fmt.Sprintf("## Project: %s\n", projCtx.Name))
	block2.WriteString(fmt.Sprintf("Path: %s\n", projCtx.Path))
	block2.WriteString(fmt.Sprintf("Language: %s\n", projCtx.Language))
	if projCtx.GitBranch != "" {
		block2.WriteString(fmt.Sprintf("Git branch: %s\n", projCtx.GitBranch))
	}
	if projCtx.GitStatus != "" {
		block2.WriteString(fmt.Sprintf("Git status: %s\n", projCtx.GitStatus))
	}
	if len(projCtx.LastCommits) > 0 {
		block2.WriteString(fmt.Sprintf("Last commit: %s\n", projCtx.LastCommits[0]))
	}
	if projCtx.TestCommand != "" {
		block2.WriteString(fmt.Sprintf("Test command: %s\n", projCtx.TestCommand))
	}

	if projCtx.ReadmeSummary != "" {
		block2.WriteString("\n## README\n")
		block2.WriteString(projCtx.ReadmeSummary)
		block2.WriteString("\n")
	}

	if projCtx.FileTree != "" {
		block2.WriteString("\n## File tree\n")
		block2.WriteString(projCtx.FileTree)
		block2.WriteString("\n")
	}

	block2.WriteString(fmt.Sprintf("\n## Mode: %s (max %d tokens)\n", currentMode.Name, currentMode.MaxTokens))
	block2.WriteString(currentMode.SystemHint)

	if projCtx.ClaudeMD != "" {
		block2.WriteString("\n\n## Project instructions (CLAUDE.md)\n")
		claudeMD := projCtx.ClaudeMD
		if len(claudeMD) > 2000 {
			claudeMD = claudeMD[:2000] + "\n... (truncated)"
		}
		block2.WriteString(claudeMD)
	}

	blocks = append(blocks, ai.CachedBlock(block2.String()))

	// Block 3: Memories + learned context — dynamic per request.
	var block3 strings.Builder

	memories, err := b.store.GetTopMemories(ctx, projCtx.ID, 20)
	if err == nil && len(memories) > 0 {
		block3.WriteString("## Ghost memories\n")

		// Group memories by category for readability.
		// Order: identity-tier (no decay) → behavioral-tier (45d) → situational-tier (30d)
		categoryOrder := []string{
			"preference", "convention", "fact",
			"architecture", "pattern",
			"decision", "gotcha", "dependency",
		}
		buckets := make(map[string][]memory.Memory, len(categoryOrder))
		for i := range memories {
			buckets[memories[i].Category] = append(buckets[memories[i].Category], memories[i])
		}
		for _, cat := range categoryOrder {
			mems := buckets[cat]
			if len(mems) == 0 {
				continue
			}
			block3.WriteString(fmt.Sprintf("\n### %s\n", cat))
			for _, m := range mems {
				pin := ""
				if m.Pinned {
					pin = " [pinned]"
				}
				block3.WriteString(fmt.Sprintf("- %s (imp: %.1f%s)\n", m.Content, m.Importance, pin))
			}
		}
	}

	learnedCtx, err := b.store.GetLearnedContext(ctx, projCtx.ID)
	if err == nil && learnedCtx != "" {
		block3.WriteString("\n## Learned context\n")
		block3.WriteString(learnedCtx)
	}

	if len(projCtx.LastCommits) > 1 {
		block3.WriteString("\n\n## Recent git activity\n")
		for _, c := range projCtx.LastCommits {
			block3.WriteString(fmt.Sprintf("- %s\n", c))
		}
	}

	if block3.Len() > 0 {
		blocks = append(blocks, ai.PlainBlock(block3.String()))
	}

	return blocks
}
