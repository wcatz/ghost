package memory

import (
	"testing"
)

func TestHasFTSTerm(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected bool
	}{
		{"whitespace only", "   ", false},
		{"punctuation only", "***", false},
		{"question and exclamation", "?!", false},
		{"tab and newline", "\t\n", false},
		{"non-Latin cyrillic", "привет", true},
		{"non-Latin japanese", "こんにちは", true},
		{"non-Latin arabic", "مرحبا", true},
		{"ordinary single word", "hello", true},
		{"ordinary multi-word", "hello world", true},
		{"mixed with punctuation", "hello world!", true},
		{"with asterisk prefix", "sqlite*", true},
		{"with numbers", "port 8080", true},
		{"camelCase", "sanitizeFTSN", true},
		{"snake_case", "ghost_windows_arm64", true},
		{"12-word query", "one two three four five six seven eight nine ten eleven twelve", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := HasFTSTerm(tc.input)
			if result != tc.expected {
				t.Errorf("HasFTSTerm(%q) = %v, want %v", tc.input, result, tc.expected)
			}
		})
	}
}
