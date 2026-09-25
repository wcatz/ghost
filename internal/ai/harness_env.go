package ai

import (
	"os"
	"strings"
)

// harnessEnv returns the environment a harness child should run with: an
// explicit allowlist, not "everything except a few keys".
//
// The children receive memory text, and memory text routinely contains
// third-party content pulled in from a repository. Until now they inherited
// essentially the whole user environment and stripLLMKeys removed exactly
// three LLM keys — so AWS, GitHub, SOPS, age and kube credentials rode along
// purely because they happened to be set, alongside anything else in the
// parent's environment.
//
// What is kept is what a harness needs to run and nothing else: PATH to find
// its own binaries, HOME where it stores the login this binary is billing
// against, locale so it does not emit mis-encoded output, the XDG locations,
// terminal and timezone, proxies and CA bundles for reaching the network, and
// SSH_AUTH_SOCK for the git it may legitimately perform.
//
// Every GHOST_* variable passes through, because that is how this binary
// configures the harness it is about to call: GHOST_OPENCODE_MODEL,
// GHOST_CLI_*_BINARY, GHOST_CLI_MODEL_* pins, GHOST_SCRATCH_DIR.
//
// GHOST_PASSTHROUGH_ENV names further variables, comma-separated. A
// deny-by-default policy fails closed in ways that are hard to diagnose from
// the outside — the symptom is a harness that stops working for a reason
// only visible in this file — so there has to be a way out that does not
// require patching Ghost.
//
// The three LLM keys are stripped after all of that, regardless of the hatch:
// harness calls are subscription-billed and must never silently become direct
// API calls. That invariant should not depend on remembering to leave a name
// off a list.
func harnessEnv(base []string) []string {
	keep := map[string]bool{
		// Process and identity
		"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "SHELL": true,
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
		// Dropping these is how a child ends up with a different (or no)
		// credential store than the one the user configured.
		"XDG_CONFIG_HOME": true, "XDG_DATA_HOME": true,
		"XDG_CACHE_HOME": true, "XDG_STATE_HOME": true, "XDG_RUNTIME_DIR": true,
		// Network path to the model endpoint
		"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true, "ALL_PROXY": true,
		"http_proxy": true, "https_proxy": true, "no_proxy": true, "all_proxy": true,
		"SSL_CERT_FILE": true, "SSL_CERT_DIR": true, "GIT_SSL_CAINFO": true,
		"NODE_EXTRA_CA_CERTS": true, "REQUESTS_CA_BUNDLE": true, "CURL_CA_BUNDLE": true,
		// The harness may legitimately run git.
		"SSH_AUTH_SOCK": true, "GIT_ASKPASS": true, "SSH_ASKPASS": true,
		// Harness endpoint/model configuration the user may have set. These
		// name where to talk and what to call it — they are not credentials,
		// and the credential they would replace is stripped below anyway.
		"ANTHROPIC_BASE_URL": true, "ANTHROPIC_MODEL": true, "ANTHROPIC_SMALL_FAST_MODEL": true,
		"OPENAI_BASE_URL": true, "OPENAI_API_BASE": true, "OPENAI_MODEL": true,
		"OPENAI_ORG_ID": true, "OPENAI_ORGANIZATION": true,
	}

	// Explicit opt-outs for a setup this list cannot anticipate.
	for _, name := range strings.Split(os.Getenv("GHOST_PASSTHROUGH_ENV"), ",") {
		if n := strings.TrimSpace(name); n != "" {
			keep[n] = true
		}
	}

	out := make([]string, 0, len(base))
	for _, kv := range base {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue // malformed entry; dropping it cannot lose information
		}
		if keep[name] || strings.HasPrefix(name, "GHOST_") {
			out = append(out, kv)
		}
	}
	return stripLLMKeys(out)
}
