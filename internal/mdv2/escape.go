// Package mdv2 provides Telegram MarkdownV2 formatting utilities.
package mdv2

import "strings"

// replacer escapes all 18 MarkdownV2 special characters.
// Allocated once, safe for concurrent use.
var replacer = strings.NewReplacer(
	"_", "\\_", "*", "\\*", "[", "\\[", "]", "\\]",
	"(", "\\(", ")", "\\)", "~", "\\~", "`", "\\`",
	">", "\\>", "#", "\\#", "+", "\\+", "-", "\\-",
	"=", "\\=", "|", "\\|", "{", "\\{", "}", "\\}",
	".", "\\.", "!", "\\!",
)

// Esc escapes special characters for Telegram MarkdownV2.
func Esc(s string) string {
	return replacer.Replace(s)
}

// Split splits text into chunks that fit Telegram's message limit.
// Splits at newline boundaries when possible.
func Split(text string, limit int) []string {
	if len(text) <= limit {
		return []string{text}
	}

	var chunks []string
	for len(text) > 0 {
		if len(text) <= limit {
			chunks = append(chunks, text)
			break
		}

		cut := strings.LastIndex(text[:limit], "\n")
		if cut <= 0 {
			cut = limit
		}

		chunks = append(chunks, text[:cut])
		text = text[cut:]
		if len(text) > 0 && text[0] == '\n' {
			text = text[1:]
		}
	}
	return chunks
}
