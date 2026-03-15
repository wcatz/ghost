package mode

// Mode defines a ghost operating mode.
type Mode struct {
	Name           string
	MaxTokens      int
	ThinkingBudget int // 0 = disabled, >0 = extended thinking token budget
	SystemHint     string
}

// Modes is the complete set of operating modes.
var Modes = map[string]Mode{
	"chat": {
		Name:      "chat",
		MaxTokens: 1500,
		SystemHint: "Conversational coding assistance. Brief answers unless asked to elaborate.",
	},
	"code": {
		Name:           "code",
		MaxTokens:      16000,
		ThinkingBudget: 10000,
		SystemHint:     "Write correct, tested code. Show file paths and line numbers. Run tests after changes when tests exist.",
	},
	"debug": {
		Name:           "debug",
		MaxTokens:      16000,
		ThinkingBudget: 10000,
		SystemHint:     "Systematic debugging. Read error messages carefully. Form hypotheses, verify with grep/read, then fix. Show evidence for every conclusion.",
	},
	"review": {
		Name:           "review",
		MaxTokens:      12000,
		ThinkingBudget: 8000,
		SystemHint:     "Code review mode. Analyze diffs for bugs, security issues, performance problems, and style. Be constructive and specific.",
	},
	"plan": {
		Name:           "plan",
		MaxTokens:      16000,
		ThinkingBudget: 10000,
		SystemHint:     "Architecture and planning. Think before coding. Outline the approach, identify risks, list files to change. Do NOT write code unless explicitly asked.",
	},
	"refactor": {
		Name:           "refactor",
		MaxTokens:      16000,
		ThinkingBudget: 10000,
		SystemHint:     "Refactoring mode. Preserve behavior exactly. Make surgical changes. Run tests before and after every change.",
	},
}

// Default returns the default mode name.
func Default() string {
	return "code"
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
