//go:build e2e

package e2e

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSandboxCannotReachAClientOutsideIt is #1759. The sandbox's PATH used to
// carry the host's own directories, so an MCP client the host happened to
// have installed was reachable from inside the sandbox: `ghost mcp init`
// found it, drove it, and reported success where the answer should have been
// "no client on PATH" — and the suite ran a real client where the end-to-end
// rules say the harnesses are fakes.
//
// The client here is planted OUTSIDE the sandbox, in the directory the suite
// builds the binary under test in, which is where a developer's own client
// sits too. It is the suite's own fake harness, so a client the sandbox
// reached would answer `mcp init` exactly as a real one does: the run would
// succeed, and the suite would be driving a program it does not control.
func TestSandboxCannotReachAClientOutsideIt(t *testing.T) {
	s := newSandbox(t)

	outside := filepath.Dir(ghostBin)
	planted := filepath.Join(outside, "claude")
	body := strings.NewReplacer("@NAME@", "claude", "@DIR@", outside).Replace(harnessScript)
	if err := os.WriteFile(planted, []byte(body), 0o755); err != nil {
		if errors.Is(err, fs.ErrPermission) {
			// GHOST_E2E_BIN points at an installed build, and the directory
			// that holds it is the host's and may not be writable. The
			// invariant test still holds there.
			t.Skipf("cannot plant a client beside the binary under test: %v", err)
		}
		t.Fatalf("plant the client outside the sandbox: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Remove(planted)
		_ = os.Remove(filepath.Join(outside, "claude.argv"))
		_ = os.Remove(filepath.Join(outside, "claude.mcp.ghost"))
	})
	_ = os.Remove(filepath.Join(outside, "claude.argv"))

	bare := bareSandbox(s)
	r := bare.mustFail("mcp", "init")
	mustMatch(t, "no client found", r.stderr, "(?i)no supported MCP client|install one of|--client")
	if _, err := os.Stat(filepath.Join(outside, "claude.argv")); err == nil {
		t.Fatal("the sandboxed program ran the client that lives outside the sandbox")
	}
}

// TestSandboxPATHNamesOnlySuiteOwnedDirs pins the rule that keeps #1759 out.
// Every directory on the sandbox's PATH is one this suite created, so a
// program the host installed elsewhere — an MCP client above all — cannot be
// found from inside the sandbox, and the fake harnesses are the only client
// names a sandboxed program can resolve.
func TestSandboxPATHNamesOnlySuiteOwnedDirs(t *testing.T) {
	s := newSandbox(t)
	pathValue := sandboxPath(s.env)
	if pathValue == "" {
		t.Fatal("the sandbox environment carries no PATH")
	}
	for _, dir := range pathDirs(pathValue) {
		if !withinDir(s.root, dir) {
			t.Fatalf("the sandbox PATH names %q, which is outside the sandbox root %q: a client installed there is reachable from inside the sandbox",
				dir, s.root)
		}
	}
	for _, name := range harnessBinaries {
		if p, ok := lookPathIn(pathValue, name); ok && !withinDir(s.root, p) {
			t.Fatalf("the client %q resolves to %q, which is outside the sandbox root %q", name, p, s.root)
		}
	}
	// Closed is not the same as empty: Ghost resolves git by name for
	// repository identity, so a sandbox that could not run it would answer
	// "no repository" for every path.
	if _, err := exec.LookPath("git"); err == nil {
		if _, ok := lookPathIn(pathValue, "git"); !ok {
			t.Fatal("the sandbox PATH does not carry git, which this host has: repository identity is unreachable from inside the sandbox")
		}
	}
}

// TestSandboxFakeHarnessRunsUnderTheClosedPATH: the fake harness scripts call
// POSIX utilities by name, and the sandbox's PATH is closed, so each one has
// to be reachable through the tools directory. A utility the list forgot
// would break a fake halfway through a test with a diagnostic about the
// harness rather than about the thing under test.
//
// Both of the fake's shapes are asked for, because they use different
// utilities: the reflect answer greps the prompt and counts ids with sed,
// and the diagnostic for a prompt the fake cannot read trims that prompt with
// head and tr.
func TestSandboxFakeHarnessRunsUnderTheClosedPATH(t *testing.T) {
	s := newSandbox(t)
	fake := filepath.Join(s.bin, "claude")
	ask := func(stdin string) (string, error) {
		cmd := exec.Command(fake)
		cmd.Env = s.env
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	answer, err := ask("learned_context\n\n1.\n- id:m1\n")
	if err != nil {
		t.Fatalf("the fake harness could not answer under the sandbox PATH: %v\n%s", err, answer)
	}
	if !strings.Contains(answer, "fake consolidation") {
		t.Fatalf("the fake harness did not answer the prompt:\n%s", answer)
	}

	diagnostic, err := ask("nothing the fake recognises\n")
	if err == nil {
		t.Fatalf("the fake harness accepted a prompt it cannot read:\n%s", diagnostic)
	}
	if !strings.Contains(diagnostic, "unrecognised prompt") {
		t.Fatalf("the fake harness did not report the prompt it cannot read:\n%s", diagnostic)
	}
	// The diagnostic quotes the prompt back, trimmed by head and translated by
	// tr, so the quote is where those two are pinned.
	if !strings.Contains(diagnostic, "nothing the fake recognises") {
		t.Fatalf("the fake harness did not quote the prompt it cannot read:\n%s", diagnostic)
	}
}

// sandboxPath returns the PATH a sandbox hands to the programs it runs.
func sandboxPath(env []string) string {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			return v
		}
	}
	return ""
}

// pathDirs splits a PATH value into the directories it names.
func pathDirs(pathValue string) []string {
	return strings.Split(pathValue, string(os.PathListSeparator))
}

// withinDir reports whether path is dir itself or lives under it.
func withinDir(dir, path string) bool {
	if path == dir {
		return true
	}
	return strings.HasPrefix(path, dir+string(os.PathSeparator))
}

// lookPathIn resolves name against a PATH value the way exec.LookPath does:
// the first directory holding an executable file by that name.
func lookPathIn(pathValue, name string) (string, bool) {
	for _, dir := range pathDirs(pathValue) {
		if dir == "" {
			continue
		}
		p := filepath.Join(dir, name)
		info, err := os.Stat(p)
		if err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
			continue
		}
		return p, true
	}
	return "", false
}
