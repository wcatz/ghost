package ai

import (
	"os"
	"strings"
)

// harnessKind identifies the backend whose authentication and configuration
// roots are allowed into a child environment. Keeping this explicit prevents
// a credential intended for one harness from being copied into another one.
type harnessKind string

const (
	harnessClaude   harnessKind = "claude"
	harnessCodex    harnessKind = "codex"
	harnessGoose    harnessKind = "goose"
	harnessOpencode harnessKind = "opencode"
)

// harnessEnv returns the environment a harness child should run with: an
// explicit allowlist, not "everything except a few keys". The backend is part
// of the policy because authentication/config roots are not interchangeable.
//
// The children receive memory text, and memory text routinely contains
// third-party content pulled in from a repository. Until now they inherited
// essentially the whole user environment and stripLLMKeys removed exactly
// three LLM keys — so AWS, GitHub, SOPS, age and kube credentials rode along
// purely because they happened to be set, alongside anything else in the
// parent's environment.
//
// What is kept is what a harness needs to run and nothing else: PATH to find
// its own binaries, HOME/USERPROFILE and the platform config roots, locale so
// it does not emit mis-encoded output, the XDG locations, terminal and
// timezone, and proxies/CA bundles for reaching the network. Backend-specific
// entries below preserve the documented auth/config path for that backend
// without widening the common policy.
//
// GHOST_* is also an explicit list. The old prefix wildcard would let a
// legacy GHOST_API_KEY, GHOST_DATABASE_URL, or a future secret-looking name
// bypass the policy. GHOST_PASSTHROUGH_ENV names further variables,
// comma-separated, as an explicit escape hatch. The hatch is intentionally
// security-sensitive: opting a credential in re-exposes it to the selected
// harness. The three LLM keys are stripped after all of that, regardless of
// the hatch, so the subscription-billing invariant does not depend on
// remembering to leave a name off a list.
func harnessEnv(base []string, kind harnessKind) []string {
	keep := map[string]bool{
		// Process, identity, and platform home/config variables.
		"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "SHELL": true,
		"USERPROFILE": true, "HOMEDRIVE": true, "HOMEPATH": true,
		"USERNAME": true, "USERDOMAIN": true,
		"APPDATA": true, "LOCALAPPDATA": true, "PROGRAMDATA": true,
		"ALLUSERSPROFILE": true, "PROGRAMFILES": true, "PROGRAMFILES(X86)": true,
		"SYSTEMROOT": true, "WINDIR": true, "PATHEXT": true, "COMSPEC": true,
		"PROCESSOR_ARCHITECTURE": true, "PROCESSOR_ARCHITEW6432": true,
		"NUMBER_OF_PROCESSORS": true, "OS": true,
		// Where the harness is told to put its own working files. Without
		// TMPDIR a child falls back to /tmp and Ghost's scratch budget stops
		// describing where its children actually wrote.
		"TMPDIR": true, "TMP": true, "TEMP": true,
		// Locale — output encoding, and a harness that reads a non-UTF-8
		// locale can mangle the very memory text it was given.
		"LANG": true, "LANGUAGE": true, "TZ": true,
		"LC_ALL": true, "LC_CTYPE": true, "LC_COLLATE": true,
		"LC_MESSAGES": true, "LC_MONETARY": true, "LC_NUMERIC": true, "LC_TIME": true,
		// Terminal
		"TERM": true, "COLORTERM": true, "NO_COLOR": true,
		// XDG: the login and config the harness authenticates from live here.
		// OpenCode replaces these with an invocation-owned isolated tree before
		// it starts; the other backends use them for their own auth/config.
		"XDG_CONFIG_HOME": true, "XDG_DATA_HOME": true,
		"XDG_CACHE_HOME": true, "XDG_STATE_HOME": true, "XDG_RUNTIME_DIR": true,
		// Network path to the model endpoint
		"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true, "ALL_PROXY": true,
		"http_proxy": true, "https_proxy": true, "no_proxy": true, "all_proxy": true,
		"SSL_CERT_FILE": true, "SSL_CERT_DIR": true, "GIT_SSL_CAINFO": true,
		"NODE_EXTRA_CA_CERTS": true, "REQUESTS_CA_BUNDLE": true, "CURL_CA_BUNDLE": true,
		// Git/SSH agent variables are intentionally omitted: no harness
		// invocation executes a tool (the others disable their tool surfaces;
		// OpenCode V2 advertises its tools but its non-interactive run declines
		// every call), so an agent credential has no legitimate consumer in the
		// child — and leaving it out keeps it unreachable should that ever change.
		// Harness endpoint/model configuration the user may have set. These
		// name where to talk and what to call it — they are not credentials.
		"ANTHROPIC_BASE_URL": true, "ANTHROPIC_MODEL": true, "ANTHROPIC_SMALL_FAST_MODEL": true,
		"OPENAI_BASE_URL": true, "OPENAI_API_BASE": true, "OPENAI_MODEL": true,
		"OPENAI_ORG_ID": true, "OPENAI_ORGANIZATION": true,
	}

	// Ghost's public configuration surface. Keep this list explicit: a new
	// GHOST_* variable is not a credential until it has been reviewed and added
	// here. Runtime-only test and diagnostic switches intentionally stay out.
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

	// Preserve only the auth/config variables documented for this backend.
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

	// Explicit opt-outs for a setup this list cannot anticipate. Stored
	// uppercased for the same reason as the lookup below: a user writing
	// "MyVar" against a parent that spells it "myvar" is still opting it in.
	for _, name := range strings.Split(os.Getenv("GHOST_PASSTHROUGH_ENV"), ",") {
		if n := strings.TrimSpace(name); n != "" {
			keep[strings.ToUpper(n)] = true
		}
	}

	out := make([]string, 0, len(base))
	for _, kv := range base {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue // malformed entry; dropping it cannot lose information
		}
		// Case-insensitive: Windows spells these Path and Home, and
		// os.Environ() returns exactly that. A case-sensitive lookup dropped
		// PATH there, leaving the child without a way to find its own
		// binaries. isTempDirKey normalizes with EqualFold for the same
		// reason; ToUpper is used here so one normalized value serves the
		// common, Ghost, backend, and passthrough lookups.
		if keep[strings.ToUpper(name)] {
			out = append(out, kv)
		}
	}
	return stripLLMKeys(out)
}
