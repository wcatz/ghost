package mode

// Mode defines a ghost operating mode.
type Mode struct {
	Name           string
	MaxTokens      int
	ThinkingBudget int  // -1 = adaptive (Claude auto-scales), 0 = disabled, >0 = fixed budget
	UseQualityModel bool // if true, use Opus for this mode
	SystemHint     string
}

// Modes is the complete set of operating modes.
var Modes = map[string]Mode{
	"chat": {
		Name:           "chat",
		MaxTokens:      8192,
		ThinkingBudget: 0, // disabled — API 2023-06-01 requires budget_tokens field
		UseQualityModel: false,
		SystemHint:     "Conversational assistant. Brief answers unless asked to elaborate. Save important facts to memory.",
	},
	"code": {
		Name:           "code",
		MaxTokens:      16384,
		ThinkingBudget: -1, // adaptive thinking
		UseQualityModel: true,
		SystemHint:     "Engineering mode. Write production-ready code with tests. Use extended thinking for architecture decisions. Always run tests and linters before declaring completion.",
	},
	"debug": {
		Name:           "debug",
		MaxTokens:      16384,
		ThinkingBudget: -1, // adaptive thinking
		UseQualityModel: true,
		SystemHint:     "Debugging mode. Systematically diagnose issues: reproduce the problem, examine logs/errors, trace execution flow, test hypotheses. Use extended thinking for complex root cause analysis.",
	},
	"review": {
		Name:           "review",
		MaxTokens:      16384,
		ThinkingBudget: -1, // adaptive thinking
		UseQualityModel: true,
		SystemHint:     "Code review mode. Analyze code for correctness, performance, security, maintainability, and adherence to project conventions. Provide specific, actionable feedback with examples.",
	},
	"refactor": {
		Name:           "refactor",
		MaxTokens:      16384,
		ThinkingBudget: -1, // adaptive thinking
		UseQualityModel: true,
		SystemHint:     "Refactoring mode. Improve code structure while preserving behavior. Plan changes carefully, maintain test coverage, make incremental improvements. Always verify tests pass after each change.",
	},
	"plan": {
		Name:           "plan",
		MaxTokens:      8192,
		ThinkingBudget: -1, // adaptive thinking
		UseQualityModel: true,
		SystemHint:     "Planning mode. Break down complex tasks into actionable steps. Consider dependencies, risks, and tradeoffs. Create clear implementation plans with milestones.",
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
