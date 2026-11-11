package ai

import (
	"os"
	"strings"
)

type harnessKind string

const (
	harnessClaude   harnessKind = "claude"
	harnessCodex    harnessKind = "codex"
	harnessGoose    harnessKind = "goose"
	harnessOpencode harnessKind = "opencode"
)

func harnessEnv(base []string, kind harnessKind) []string {
	keep := map[string]bool{

		"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "SHELL": true,
		"USERPROFILE": true, "HOMEDRIVE": true, "HOMEPATH": true,
		"USERNAME": true, "USERDOMAIN": true,
		"APPDATA": true, "LOCALAPPDATA": true, "PROGRAMDATA": true,
		"ALLUSERSPROFILE": true, "PROGRAMFILES": true, "PROGRAMFILES(X86)": true,
		"SYSTEMROOT": true, "WINDIR": true, "PATHEXT": true, "COMSPEC": true,
		"PROCESSOR_ARCHITECTURE": true, "PROCESSOR_ARCHITEW6432": true,
		"NUMBER_OF_PROCESSORS": true, "OS": true,

		"TMPDIR": true, "TMP": true, "TEMP": true,

		"LANG": true, "LANGUAGE": true, "TZ": true,
		"LC_ALL": true, "LC_CTYPE": true, "LC_COLLATE": true,
		"LC_MESSAGES": true, "LC_MONETARY": true, "LC_NUMERIC": true, "LC_TIME": true,

		"TERM": true, "COLORTERM": true, "NO_COLOR": true,

		"XDG_CONFIG_HOME": true, "XDG_DATA_HOME": true,
		"XDG_CACHE_HOME": true, "XDG_STATE_HOME": true, "XDG_RUNTIME_DIR": true,

		"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true, "ALL_PROXY": true,
		"http_proxy": true, "https_proxy": true, "no_proxy": true, "all_proxy": true,
		"SSL_CERT_FILE": true, "SSL_CERT_DIR": true, "GIT_SSL_CAINFO": true,
		"NODE_EXTRA_CA_CERTS": true, "REQUESTS_CA_BUNDLE": true, "CURL_CA_BUNDLE": true,

		"ANTHROPIC_BASE_URL": true, "ANTHROPIC_MODEL": true, "ANTHROPIC_SMALL_FAST_MODEL": true,
		"OPENAI_BASE_URL": true, "OPENAI_API_BASE": true, "OPENAI_MODEL": true,
		"OPENAI_ORG_ID": true, "OPENAI_ORGANIZATION": true,
	}

	for _, name := range []string{
		"GHOST_CLI_CLAUDE_BINARY", "GHOST_CLI_OPENCODE_BINARY",
		"GHOST_CLI_CODEX_BINARY", "GHOST_CLI_GOOSE_BINARY",
		"GHOST_CLI_MODEL_REFLECT", "GHOST_CLI_MODEL_RESOLVE", "GHOST_CLI_MODEL_SUPERSEDE",
		"GHOST_EMBEDDING_ENABLED", "GHOST_EMBEDDING_MODEL", "GHOST_EMBEDDING_DIMENSIONS", "GHOST_EMBEDDING_OLLAMA_URL",
		"GHOST_LINKING_ENABLED", "GHOST_LINKING_THRESHOLD", "GHOST_LINKING_DEMOTION_THRESHOLD",
		"GHOST_INJECTION_BEHAVIOR_FLOOR", "GHOST_INJECTION_BEHAVIOR_CATEGORIES",
		"GHOST_INJECTION_CATEGORY_WEIGHTS", "GHOST_INJECTION_CATEGORY_CAPS",
		"GHOST_OBSIDIAN_VAULT_DIR", "GHOST_OBSIDIAN_INTERVAL", "GHOST_OBSIDIAN_AUTO_SYNC",
		"GHOST_REFLECTION_AUTO_REFLECT", "GHOST_REFLECTION_AUTO_RESOLVE", "GHOST_REFLECTION_AUTO_SUPERSEDE",
		"GHOST_REFLECTION_LIFECYCLE_TIMEOUT_MINUTES", "GHOST_REFLECTION_CONSOLIDATION_TIMEOUT_MINUTES",
		"GHOST_LIFECYCLE_MIN_INTERVAL",
		"GHOST_OLLAMA_URL", "GHOST_ROUTING_DEFAULT_PROJECT", "GHOST_SEARCH_MIN_SIMILARITY",
		"GHOST_SCRATCH_DIR", "GHOST_SCRATCH_MAX_BYTES", "GHOST_OPENCODE_MODEL",
	} {
		keep[name] = true
	}

	for _, name := range map[harnessKind][]string{
		harnessClaude: {
			"CLAUDE_CONFIG_DIR",
		},
		harnessCodex: {
			"CODEX_HOME",
		},
		harnessGoose: {
			"GOOSE_PATH_ROOT", "GOOSE_PROVIDER", "GOOSE_MODEL", "GOOSE_PROVIDER__TYPE",
			"GOOSE_PROVIDER__HOST", "GOOSE_TEMPERATURE", "GOOSE_MAX_TOKENS", "GOOSE_MAX_TURNS",
			"GOOSE_DISABLE_SESSION_NAMING", "GOOSE_MAX_TOOL_REPETITIONS", "GOOSE_MAX_BACKGROUND_TASKS",
			"GOOSE_SUBAGENT_MAX_TURNS", "GOOSE_SEARCH_PATHS", "GOOSE_SHELL", "GOOSE_NO_CODE_TRUNCATION",
			"GOOSE_CLI_MIN_PRIORITY", "GOOSE_SHOW_FULL_OUTPUT", "GOOSE_MAX_TOOL_RESPONSE_SIZE",
			"GOOSE_TELEMETRY_ENABLED",
		},
		harnessOpencode: {
			"OPENCODE_API_KEY",
		},
	}[kind] {
		keep[name] = true
	}

	for _, name := range strings.Split(os.Getenv("GHOST_PASSTHROUGH_ENV"), ",") {
		if n := strings.TrimSpace(name); n != "" {
			keep[strings.ToUpper(n)] = true
		}
	}

	out := make([]string, 0, len(base))
	for _, kv := range base {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}

		if keep[strings.ToUpper(name)] {
			out = append(out, kv)
		}
	}
	return stripLLMKeys(out)
}
