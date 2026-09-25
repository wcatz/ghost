package ai

import (
	"context"
	"strings"
	"testing"
)

func namesOf(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			out[k] = v
		}
	}
	return out
}

// TestHarnessEnvDropsCredentialsNotOurs: children receive memory text, and
// memory text routinely contains third-party content read out of a repository.
// They used to inherit almost everything and strip only three LLM keys, so
// AWS, GitHub, SOPS, age and kube credentials rode along merely for being set.
func TestHarnessEnvDropsCredentialsNotOurs(t *testing.T) {
	base := []string{
		"PATH=/usr/bin", "HOME=/home/u",
		"AWS_SECRET_ACCESS_KEY=AKIAsecret",
		"AWS_ACCESS_KEY_ID=AKIAid",
		"GITHUB_TOKEN=ghp_secret",
		"SOPS_AGE_KEY_FILE=/home/u/.config/sops/age/keys.txt",
		"AGE_KEY=SECRETAGEKEY",
		"KUBECONFIG=/home/u/.kube/config",
		"VAULT_TOKEN=hvs.secret",
		"DATABASE_URL=postgres://user:pass@host/db",
		"SSH_AUTH_SOCK=/tmp/ssh-agent",
		"GIT_ASKPASS=/tmp/askpass",
		"SSH_ASKPASS=/tmp/ssh-askpass",
		"GHOST_DEBUG=1",
		"GHOST_LOG_FILE=/tmp/ghost.log",
	}
	got := namesOf(harnessEnv(base, harnessClaude))

	for _, gone := range []string{
		"AWS_SECRET_ACCESS_KEY", "AWS_ACCESS_KEY_ID", "GITHUB_TOKEN",
		"SOPS_AGE_KEY_FILE", "AGE_KEY", "KUBECONFIG", "VAULT_TOKEN", "DATABASE_URL",
		"SSH_AUTH_SOCK", "GIT_ASKPASS", "SSH_ASKPASS", "GHOST_DEBUG", "GHOST_LOG_FILE",
	} {
		if v, present := got[gone]; present {
			t.Errorf("%s passed to the harness child (value %q) — it has no business with it", gone, v)
		}
	}
	for _, keep := range []string{"PATH", "HOME"} {
		if _, present := got[keep]; !present {
			t.Errorf("%s was dropped — a child without it cannot run at all", keep)
		}
	}
}

// TestHarnessEnvKeepsWhatAHarnessNeeds is the other half: an allowlist that is
// too tight breaks every harness invocation in ways that only show up against
// a real binary, so the required set is asserted explicitly.
func TestHarnessEnvKeepsWhatAHarnessNeeds(t *testing.T) {
	base := []string{
		"PATH=/usr/bin", "HOME=/home/u", "USER=u", "LOGNAME=u", "SHELL=/bin/sh",
		"TMPDIR=/tmp/ghost", "LANG=C.UTF-8", "TZ=UTC",
		"XDG_CONFIG_HOME=/home/u/.config", "XDG_DATA_HOME=/home/u/.local/share",
		"HTTP_PROXY=http://proxy:3128", "SSL_CERT_FILE=/etc/ssl/certs/ca.pem",
		"TERM=xterm-256color",
		"GHOST_OPENCODE_MODEL=opencode/big-pickle",
		"GHOST_CLI_CLAUDE_BINARY=/usr/local/bin/claude",
		"GHOST_SCRATCH_DIR=/tmp/scratch",
	}
	got := namesOf(harnessEnv(base, harnessClaude))

	for k, want := range map[string]string{
		"PATH": "/usr/bin", "HOME": "/home/u", "TMPDIR": "/tmp/ghost",
		"LANG": "C.UTF-8", "XDG_CONFIG_HOME": "/home/u/.config",
		"HTTP_PROXY": "http://proxy:3128", "SSL_CERT_FILE": "/etc/ssl/certs/ca.pem",
		"GHOST_OPENCODE_MODEL":    "opencode/big-pickle",
		"GHOST_CLI_CLAUDE_BINARY": "/usr/local/bin/claude",
		"GHOST_SCRATCH_DIR":       "/tmp/scratch",
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
}

// TestHarnessEnvPassthroughEscapeHatch: a deny-by-default policy fails closed
// in ways that are invisible from outside — the symptom is a harness that
// stops working for a reason readable only inside this file. There has to be a
// way out that does not require patching Ghost.
func TestHarnessEnvPassthroughEscapeHatch(t *testing.T) {
	t.Setenv("GHOST_PASSTHROUGH_ENV", "MY_SITE_VAR, ANOTHER_ONE")

	got := namesOf(harnessEnv([]string{
		"PATH=/usr/bin", "MY_SITE_VAR=yes", "ANOTHER_ONE=2", "STILL_DROPPED=no",
	}, harnessClaude))

	if got["MY_SITE_VAR"] != "yes" || got["ANOTHER_ONE"] != "2" {
		t.Errorf("opted-in variables not passed through: %v", got)
	}
	if _, present := got["STILL_DROPPED"]; present {
		t.Errorf("a variable nobody opted into was passed: %v", got)
	}
}

// TestHarnessEnvNeverPassesLLMKeysEvenWhenOptedIn: harness calls are
// subscription-billed and must never silently become direct API calls. That
// invariant should not depend on remembering to leave a name off a list, so
// the strip runs after the allowlist and after the escape hatch.
func TestHarnessEnvNeverPassesLLMKeysEvenWhenOptedIn(t *testing.T) {
	t.Setenv("GHOST_PASSTHROUGH_ENV", "ANTHROPIC_API_KEY,OPENAI_API_KEY,GOOSE_PROVIDER__API_KEY")
	t.Setenv("ANTHROPIC_API_KEY", "sk-should-never-pass")
	t.Setenv("OPENAI_API_KEY", "sk-openai-should-never-pass")
	t.Setenv("GOOSE_PROVIDER__API_KEY", "goose-should-never-pass")

	got := namesOf(harnessEnv([]string{
		"PATH=/usr/bin",
		"ANTHROPIC_API_KEY=sk-should-never-pass",
		"OPENAI_API_KEY=sk-openai-should-never-pass",
		"GOOSE_PROVIDER__API_KEY=goose-should-never-pass",
	}, harnessClaude))

	for _, k := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GOOSE_PROVIDER__API_KEY"} {
		if v, present := got[k]; present {
			t.Errorf("%s reached the child despite being opted in (value %q)", k, v)
		}
	}
}

// TestCLIClient_ChildDoesNotInheritSecrets is the focused Claude wiring
// regression. The table-driven policy tests in harness_policy_test.go cover
// the same spawn funnel for all four clients.
func TestCLIClient_ChildDoesNotInheritSecrets(t *testing.T) {
	bin := fakeClaudeBinary(t, `
for v in AWS_SECRET_ACCESS_KEY GITHUB_TOKEN SOPS_AGE_KEY_FILE KUBECONFIG; do
  eval "val=\$$v"
  if [ -n "$val" ]; then echo "LEAKED $v=$val" >&2; exit 1; fi
done
if [ -z "$PATH" ]; then echo "PATH missing from child" >&2; exit 1; fi
if [ -z "$HOME" ]; then echo "HOME missing from child" >&2; exit 1; fi
if [ -z "$GHOST_SCRATCH_DIR" ]; then echo "GHOST_SCRATCH_DIR not passed to child" >&2; exit 1; fi
printf '%s' '{"memories":[]}'
`)
	t.Setenv("AWS_SECRET_ACCESS_KEY", "AKIAsecret")
	t.Setenv("GITHUB_TOKEN", "ghp_secret")
	t.Setenv("SOPS_AGE_KEY_FILE", "/home/u/.config/sops/age/keys.txt")
	t.Setenv("KUBECONFIG", "/home/u/.kube/config")

	c := &CLIClient{binary: bin}
	text, _, err := c.Reflect(context.Background(), "prompt")
	if err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	if text != `{"memories":[]}` {
		t.Errorf("unexpected stdout: %q", text)
	}
}

// TestHarnessEnvSurvivesAnEmptyParent keeps the function total: a parent with
// no environment produces an empty child environment, while any PATH present
// in the parent is never silently dropped.
func TestHarnessEnvSurvivesAnEmptyParent(t *testing.T) {
	got := namesOf(harnessEnv(nil, harnessClaude))
	if len(got) != 0 {
		t.Errorf("with no parent env the child should get nothing, got %v", got)
	}
	// The real guarantee: if PATH exists it is kept, never silently dropped.
	got = namesOf(harnessEnv([]string{"PATH=/bin"}, harnessClaude))
	if got["PATH"] != "/bin" {
		t.Errorf("PATH = %q, want /bin", got["PATH"])
	}
}

// lookupFold finds a variable the way Windows treats them — by name, not by
// the exact spelling the parent happened to use.
func lookupFold(env []string, want string) (string, bool) {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && strings.EqualFold(k, want) {
			return v, true
		}
	}
	return "", false
}

// TestHarnessEnvAcceptsWindowsVariableSpelling: Windows spells these Path and
// Home, and os.Environ() hands back exactly that case. A case-sensitive map
// lookup dropped them, so a harness child on Windows started without a PATH
// and could not find its own binaries — the allowlist failing in the one
// place deny-by-default is hardest to diagnose.
//
// isTempDirKey already normalizes with EqualFold for the same reason.
func TestHarnessEnvAcceptsWindowsVariableSpelling(t *testing.T) {
	got := harnessEnv([]string{
		"Path=C:\\Windows\\system32", // Windows' own spelling of PATH
		"Home=C:\\Users\\u",
		"Lang=en_US.UTF-8",
		"Ghost_OpenCode_Model=opencode/big-pickle", // explicit Ghost name, case-insensitive
		"AWS_SECRET_ACCESS_KEY=AKIAsecret",         // still must not pass
	}, harnessClaude)

	for name, want := range map[string]string{
		"Path":                 "C:\\Windows\\system32",
		"Home":                 "C:\\Users\\u",
		"Lang":                 "en_US.UTF-8",
		"Ghost_OpenCode_Model": "opencode/big-pickle",
	} {
		if v, ok := lookupFold(got, name); !ok {
			t.Errorf("%s was dropped — a Windows child would be missing it", name)
		} else if v != want {
			t.Errorf("%s = %q, want %q", name, v, want)
		}
	}
	if _, ok := lookupFold(got, "AWS_SECRET_ACCESS_KEY"); ok {
		t.Error("AWS_SECRET_ACCESS_KEY passed despite the case-insensitive lookup")
	}
}
