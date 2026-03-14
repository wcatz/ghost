package reflection

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

)

// ExtractMemories uses a Haiku call to identify project facts and patterns
// from a chat exchange, then saves them as memories with source='chat'.
// Safe to call from a goroutine.
func ExtractMemories(ctx context.Context, client reflector, store memoryStore, logger *slog.Logger, projectID, userMsg, assistantResponse string) {
	if client == nil || store == nil {
		return
	}
	if userMsg == "" && assistantResponse == "" {
		return
	}
	// Skip trivial exchanges.
	if len(userMsg) < 20 && len(assistantResponse) < 100 {
		return
	}

	extractCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	prompt := buildExtractionPrompt(userMsg, assistantResponse)
	responseText, _, err := client.Reflect(extractCtx, prompt)
	if err != nil {
		logger.Error("memory extraction failed", "error", err, "project_id", projectID)
		return
	}

	memories := parseExtractionResponse(responseText)
	if len(memories) == 0 {
		return
	}

	saved, merged := 0, 0
	for _, m := range memories {
		if m.Content == "" {
			continue
		}
		if m.Importance < 0 {
			m.Importance = 0
		}
		if m.Importance > 1 {
			m.Importance = 1
		}
		if m.Tags == nil {
			m.Tags = []string{}
		}
		if m.Category == "" {
			m.Category = "fact"
		}

		_, wasMerged, err := store.Upsert(extractCtx, projectID, m.Category, m.Content, "chat", m.Importance, m.Tags)
		if err != nil {
			logger.Error("save chat memory", "error", err, "project_id", projectID)
			continue
		}
		if wasMerged {
			merged++
		} else {
			saved++
		}
	}

	if saved > 0 || merged > 0 {
		logger.Info("chat memories processed", "project_id", projectID, "new", saved, "merged", merged)
	}
}

func buildExtractionPrompt(userMsg, assistantResponse string) string {
	var sb strings.Builder
	sb.WriteString(ExtractionPrompt)
	sb.WriteString("\n\n---\n\n")
	sb.WriteString(fmt.Sprintf("USER: %s\n\n", userMsg))
	sb.WriteString(fmt.Sprintf("ASSISTANT: %s", assistantResponse))
	return sb.String()
}

func parseExtractionResponse(text string) []ReflectMemory {
	text = strings.TrimSpace(text)

	// Strip markdown code fences if present.
	if strings.HasPrefix(text, "```") {
		if idx := strings.Index(text, "\n"); idx != -1 {
			text = text[idx+1:]
		}
		if idx := strings.LastIndex(text, "```"); idx != -1 {
			text = text[:idx]
		}
		text = strings.TrimSpace(text)
	}

	var memories []ReflectMemory
	if err := json.Unmarshal([]byte(text), &memories); err != nil {
		return nil
	}
	return memories
}
