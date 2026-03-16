package mode

import (
	"testing"
)

func TestDefault(t *testing.T) {
	d := Default()
	if d != "chat" {
		t.Errorf("Default() = %q, want %q", d, "chat")
	}
}

func TestGet_KnownMode(t *testing.T) {
	for name, expected := range Modes {
		t.Run(name, func(t *testing.T) {
			got := Get(name)
			if got.Name != expected.Name {
				t.Errorf("Get(%q).Name = %q, want %q", name, got.Name, expected.Name)
			}
			if got.MaxTokens != expected.MaxTokens {
				t.Errorf("Get(%q).MaxTokens = %d, want %d", name, got.MaxTokens, expected.MaxTokens)
			}
			if got.ThinkingBudget != expected.ThinkingBudget {
				t.Errorf("Get(%q).ThinkingBudget = %d, want %d", name, got.ThinkingBudget, expected.ThinkingBudget)
			}
		})
	}
}

func TestGet_FallbackToDefault(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"unknown mode", "nonexistent"},
		{"empty string", ""},
		{"random string", "xyz123"},
	}

	defaultMode := Modes[Default()]

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Get(tc.input)
			if got.Name != defaultMode.Name {
				t.Errorf("Get(%q).Name = %q, want default %q", tc.input, got.Name, defaultMode.Name)
			}
			if got.MaxTokens != defaultMode.MaxTokens {
				t.Errorf("Get(%q).MaxTokens = %d, want %d", tc.input, got.MaxTokens, defaultMode.MaxTokens)
			}
		})
	}
}

func TestNames(t *testing.T) {
	names := Names()
	if len(names) == 0 {
		t.Fatal("Names() returned empty slice")
	}
	if len(names) != len(Modes) {
		t.Errorf("Names() returned %d names, but Modes has %d entries", len(names), len(Modes))
	}

	// Verify each name maps to a valid mode.
	nameSet := make(map[string]bool)
	for _, n := range names {
		nameSet[n] = true
		if _, ok := Modes[n]; !ok {
			t.Errorf("Names() returned %q which is not in Modes", n)
		}
	}

	// Verify no duplicates.
	if len(nameSet) != len(names) {
		t.Errorf("Names() contains duplicates: %v", names)
	}
}

func TestChatMode_Properties(t *testing.T) {
	chat := Get("chat")
	if chat.Name != "chat" {
		t.Errorf("chat mode name = %q, want %q", chat.Name, "chat")
	}
	if chat.MaxTokens <= 0 {
		t.Errorf("chat mode MaxTokens = %d, want > 0", chat.MaxTokens)
	}
	if chat.SystemHint == "" {
		t.Error("chat mode SystemHint is empty")
	}
}
