package mdv2

import (
	"strings"
	"testing"
)

func TestEsc(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		// Each of the 18 special characters.
		{"underscore", "_", "\\_"},
		{"asterisk", "*", "\\*"},
		{"open_bracket", "[", "\\["},
		{"close_bracket", "]", "\\]"},
		{"open_paren", "(", "\\("},
		{"close_paren", ")", "\\)"},
		{"tilde", "~", "\\~"},
		{"backtick", "`", "\\`"},
		{"greater_than", ">", "\\>"},
		{"hash", "#", "\\#"},
		{"plus", "+", "\\+"},
		{"minus", "-", "\\-"},
		{"equals", "=", "\\="},
		{"pipe", "|", "\\|"},
		{"open_brace", "{", "\\{"},
		{"close_brace", "}", "\\}"},
		{"dot", ".", "\\."},
		{"exclamation", "!", "\\!"},

		// Plain text and edge cases.
		{"plain_text", "hello world", "hello world"},
		{"empty_string", "", ""},
		{"mixed", "Hello, *world*!", "Hello, \\*world\\*\\!"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Esc(tt.input)
			if got != tt.want {
				t.Errorf("Esc(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestSplit_Short(t *testing.T) {
	chunks := Split("short", 100)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	if chunks[0] != "short" {
		t.Errorf("got %q, want %q", chunks[0], "short")
	}
}

func TestSplit_ExactLimit(t *testing.T) {
	text := "abcde"
	chunks := Split(text, len(text))
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	if chunks[0] != text {
		t.Errorf("got %q, want %q", chunks[0], text)
	}
}

func TestSplit_SplitAtNewline(t *testing.T) {
	// 10 chars + newline + 10 chars = 21 bytes; limit 15 should split at the newline.
	text := "0123456789\nabcdefghij"
	chunks := Split(text, 15)
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(chunks))
	}
	if chunks[0] != "0123456789" {
		t.Errorf("chunk[0] = %q, want %q", chunks[0], "0123456789")
	}
	if chunks[1] != "abcdefghij" {
		t.Errorf("chunk[1] = %q, want %q", chunks[1], "abcdefghij")
	}
}

func TestSplit_NoNewlines(t *testing.T) {
	// 20 chars, no newlines, limit 10 -> hard cut at 10.
	text := "01234567890123456789"
	chunks := Split(text, 10)
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(chunks))
	}
	if chunks[0] != "0123456789" {
		t.Errorf("chunk[0] = %q, want %q", chunks[0], "0123456789")
	}
	if chunks[1] != "0123456789" {
		t.Errorf("chunk[1] = %q, want %q", chunks[1], "0123456789")
	}
}

func TestSplit_MultipleChunks(t *testing.T) {
	// Build a string of 100 chars with newlines every 30 chars, limit 35.
	parts := []string{
		strings.Repeat("a", 30),
		strings.Repeat("b", 30),
		strings.Repeat("c", 30),
	}
	text := strings.Join(parts, "\n") // 30 + 1 + 30 + 1 + 30 = 92
	chunks := Split(text, 35)
	if len(chunks) < 3 {
		t.Fatalf("expected at least 3 chunks, got %d", len(chunks))
	}
	// Reassemble should equal original.
	reassembled := strings.Join(chunks, "\n")
	if reassembled != text {
		t.Errorf("reassembled text doesn't match original")
	}
}

func TestSplit_EmptyString(t *testing.T) {
	chunks := Split("", 100)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	if chunks[0] != "" {
		t.Errorf("got %q, want %q", chunks[0], "")
	}
}
