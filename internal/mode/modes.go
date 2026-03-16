package mode

// Mode defines a ghost operating mode.
type Mode struct {
	Name           string
	MaxTokens      int
	ThinkingBudget int // -1 = adaptive (Claude auto-scales), 0 = disabled, >0 = fixed budget
	SystemHint     string
}

// Modes is the complete set of operating modes.
var Modes = map[string]Mode{
	"chat": {
		Name:           "chat",
		MaxTokens:      8192,
		ThinkingBudget: 0, // disabled — API 2023-06-01 requires budget_tokens field
		SystemHint:     "Conversational assistant. Brief answers unless asked to elaborate. Save important facts to memory.",
	},
}

// Default returns the default mode name.
func Default() string {
	return "chat"
}

// Get returns a mode by name, falling back to the default.
func Get(name string) Mode {
	if m, ok := Modes[name]; ok {
		return m
	}
	return Modes[Default()]
}

// Names returns all available mode names.
func Names() []string {
	names := make([]string, 0, len(Modes))
	for name := range Modes {
		names = append(names, name)
	}
	return names
}
