package tui

import (
	"encoding/base64"
	"fmt"
	"os"
	"strings"
)

// copyOSC52 copies text to the system clipboard using the OSC 52 escape sequence.
// This works in most modern terminals (iTerm2, WezTerm, kitty, foot, tmux with
// set-clipboard on, etc.) without requiring xclip/pbcopy.
func copyOSC52(text string) {
	encoded := base64.StdEncoding.EncodeToString([]byte(text))
	// OSC 52 ; c ; <base64> ST
	fmt.Fprintf(os.Stdout, "\033]52;c;%s\a", encoded)
}

// extractLastCodeBlock finds the last fenced code block in the chat messages.
// Returns the code content (without the ``` fences) or empty string if none found.
func extractLastCodeBlock(messages []chatMessage) string {
	// Search messages in reverse order.
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.kind != msgAssistant {
			continue
		}
		blocks := extractCodeBlocks(msg.raw)
		if len(blocks) > 0 {
			return blocks[len(blocks)-1]
		}
	}
	return ""
}

// extractCodeBlocks extracts all fenced code blocks from markdown text.
// Returns the content of each block (without the ``` fences).
func extractCodeBlocks(text string) []string {
	var blocks []string
	lines := strings.Split(text, "\n")
	inBlock := false
	var current strings.Builder

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			if inBlock {
				// End of code block.
				blocks = append(blocks, strings.TrimRight(current.String(), "\n"))
				current.Reset()
				inBlock = false
			} else {
				// Start of code block.
				inBlock = true
			}
			continue
		}
		if inBlock {
			current.WriteString(line)
			current.WriteString("\n")
		}
	}

	return blocks
}
