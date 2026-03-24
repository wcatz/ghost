package reflection

import (
	"fmt"
	"strings"

	"github.com/wcatz/ghost/internal/memory"
)

// ReflectionInput holds all data fed into the reflection prompt.
type ReflectionInput struct {
	RecentExchanges  [][2]string     // [user, assistant] pairs
	ExistingMemories []memory.Memory // all memories (up to 200)
	CurrentContext   string          // learned_context from ghost_state
	LastCommits      []string        // recent commit messages
	ProjectLanguage  string
	ProjectName      string
}

// ReflectionResult holds the parsed output from a reflection call.
type ReflectionResult struct {
	LearnedContext string            `json:"learned_context"`
	Memories       []ReflectMemory   `json:"memories"`
}

// ReflectMemory is a discrete memory extracted during reflection.
type ReflectMemory struct {
	Category   string   `json:"category"`
	Content    string   `json:"content"`
	Importance float32  `json:"importance"`
	Tags       []string `json:"tags"`
	Scope      string   `json:"scope,omitempty"` // "project" (default) or "global"
}

// BuildReflectionPrompt assembles the reflection prompt from project history.
func BuildReflectionPrompt(input ReflectionInput) string {
	var sb strings.Builder

	// Recent code exchanges.
	if len(input.RecentExchanges) > 0 {
		sb.WriteString(fmt.Sprintf("## Recent Exchanges (last %d)\n", len(input.RecentExchanges)))
		for _, e := range input.RecentExchanges {
			userMsg := e[0]
			if len(userMsg) > 500 {
				userMsg = userMsg[:500] + "..."
			}
			assistantMsg := e[1]
			if len(assistantMsg) > 500 {
				assistantMsg = assistantMsg[:500] + "..."
			}
			sb.WriteString(fmt.Sprintf("- User: %q -> Ghost: %q\n", userMsg, assistantMsg))
		}
	}

	// Recent git activity.
	if len(input.LastCommits) > 0 {
		sb.WriteString(fmt.Sprintf("\n## Recent Git Activity (%d commits)\n", len(input.LastCommits)))
		for _, c := range input.LastCommits {
			sb.WriteString(fmt.Sprintf("- %s\n", c))
		}
	}

	// Project info.
	sb.WriteString(fmt.Sprintf("\n## Project\n- Name: %s\n- Language: %s\n", input.ProjectName, input.ProjectLanguage))

	// Current learned context.
	sb.WriteString("\n## Current Learned Context\n")
	if input.CurrentContext != "" {
		sb.WriteString(input.CurrentContext)
	} else {
		sb.WriteString("None yet — this is the first reflection.")
	}

	// Existing memories for consolidation.
	if len(input.ExistingMemories) > 0 {
		sb.WriteString(fmt.Sprintf("\n\n## Existing Memories (%d total) — CONSOLIDATE THESE\n", len(input.ExistingMemories)))
		sb.WriteString("Review each memory. Merge duplicates, combine similar items into one stronger memory, drop stale/irrelevant ones, and keep confirmed facts.\n")
		for _, m := range input.ExistingMemories {
			line := fmt.Sprintf("- [%s] (imp:%.1f, src:%s", m.Category, m.Importance, m.Source)
			if m.AccessCount > 0 {
				line += fmt.Sprintf(", used:%d", m.AccessCount)
			}
			line += fmt.Sprintf(") %s\n", m.Content)
			sb.WriteString(line)
		}
	}

	sb.WriteString(`

Produce a JSON object with two fields:
1. "learned_context": A concise paragraph (max 200 words) describing this project's architecture, the developer's patterns, and key technical decisions.
2. "memories": The COMPLETE consolidated memory set. This REPLACES all existing non-manual memories. Rules:
   - Merge duplicates into one stronger memory (higher importance)
   - Keep identity facts (architecture, conventions) — never drop these
   - Drop stale situational memories (old gotchas that were fixed)
   - Each memory: "category" (architecture/decision/pattern/convention/gotcha/dependency/preference/fact), "content" (1-2 sentences), "importance" (0.0-1.0), "tags" (1-3 keywords), "scope" ("project" or "global")
   - Aim for 10-25 high-quality memories, not 50 repetitive ones
   - Scope: most memories are "project". Mark as "global" ONLY if the knowledge applies across ALL repositories — examples: user preferences, cross-repo workflows (deploying from one repo to another), personal tooling choices, SSH hosts, infrastructure topology. Project-specific architecture, patterns, or conventions are always "project".

Return ONLY the JSON object, no other text.`)

	return sb.String()
}

// ExtractionPrompt is the system prompt for per-exchange memory extraction.
const ExtractionPrompt = `You analyze a conversation between a developer and their personal assistant (Ghost). Extract project-specific facts, patterns, or decisions worth remembering for future sessions.

Extract:
- Architecture: how the codebase is organized, key abstractions
- Decisions: why something was done a certain way, what alternatives were rejected
- Patterns: recurring code patterns, naming conventions
- Conventions: formatting, testing, commit message style
- Gotchas: bugs found, tricky behavior, edge cases discovered
- Dependencies: key libraries, versions, integration notes
- Preferences: developer's preferred approaches or tools

Do NOT extract:
- Transient conversation details ("the user asked about X")
- Generic programming facts ("Go uses goroutines for concurrency")
- Anything that's just restating what the code does without insight
- Secrets, API keys, passwords, or personal data

Importance scale:
- 0.9-1.0: Architecture decisions, critical gotchas, project identity
- 0.7-0.8: Useful patterns, conventions, dependencies
- 0.5-0.6: Minor facts, preferences

Return ONLY a JSON array. Each item:
{"category": "architecture|decision|pattern|convention|gotcha|dependency|preference|fact", "content": "specific observation", "importance": 0.0-1.0, "tags": ["tag1"]}

Return [] if nothing worth remembering.`
