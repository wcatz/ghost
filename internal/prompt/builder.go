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

CAPABILITIES:
- Save and search project memories
- Recall architectural decisions, conventions, gotchas, and patterns
- Track project context across conversations

RULES:
- If unsure, ask. Do not fabricate information.
- When you learn something important, use memory_save to remember it.
- Be helpful and direct. Provide context from memory when relevant.

RESPONSE STYLE:
- Be direct. Lead with the answer.
- Reference remembered context when it's relevant.
- Brief answers unless asked to elaborate.`

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
		for _, m := range memories {
			pin := ""
			if m.Pinned {
				pin = " [pinned]"
			}
			block3.WriteString(fmt.Sprintf("- [%s] %s (imp: %.1f%s)\n", m.Category, m.Content, m.Importance, pin))
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
