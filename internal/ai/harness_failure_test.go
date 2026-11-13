package ai

import (
	"context"
	"strings"
	"testing"
)

// TestHarnessFailureOutputFallsBackToStdout is the defect behind issue #540.
func TestHarnessFailureOutputFallsBackToStdout(t *testing.T) {
	if got := harnessFailureOutput(`{"error":"writeFile /x/.gitignore: ENOENT"}`, ""); !strings.Contains(got, "ENOENT") {
		t.Errorf("stdout discarded when stderr empty: %q", got)
	}
}

func TestHarnessFailureOutput(t *testing.T) {
	t.Run("prefers stderr", func(t *testing.T) {
		got := harnessFailureOutput(`{"noise":"large json stream"}`, "permission denied writing config")
		if !strings.Contains(got, "permission denied") || strings.Contains(got, "large json stream") {
			t.Errorf("got %q", got)
		}
	})
	t.Run("bounded and keeps tail", func(t *testing.T) {
		big := strings.Repeat(`{"line":"filler"}`+"\n", 5000)
		got := harnessFailureOutput(big, "")
		if len(got) > harnessFailureOutputMax+64 || !strings.Contains(got, `{"line":"filler"}`) {
			t.Errorf("len=%d", len(got))
		}
	})
	t.Run("empty", func(t *testing.T) {
		if got := harnessFailureOutput("", ""); got != "" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("trims noise", func(t *testing.T) {
		if got := harnessFailureOutput("   \n\n  actual failure line\nmore\n", ""); !strings.HasPrefix(got, "actual failure line") {
			t.Errorf("got %q", got)
		}
	})
	t.Run("guards tokens", func(t *testing.T) {
		for _, token := range []string{"ghp_AbCdEfGhIjKlMnOpQrStUvWxYz1234567890", "AKIAIOSFODNN7EXAMPLE"} {
			if got := harnessFailureOutput(token, ""); got == token {
				t.Errorf("token leaked from stdout: %q", got)
			}
			if got := harnessFailureOutput("", token); got == token {
				t.Errorf("token leaked from stderr: %q", got)
			}
		}
	})
	t.Run("guards token across cut", func(t *testing.T) {
		token := "ghp_AbCdEfGhIjKlMnOpQrStUvWxYz1234567890"
		input := strings.Repeat("=", harnessFailureOutputMax) + token + strings.Repeat("y", 1160)
		for name, args := range map[string][2]string{"stdout": {input, ""}, "stderr": {"", input}} {
			if got := harnessFailureOutput(args[0], args[1]); got == tail(input, harnessFailureOutputMax) {
				t.Errorf("%s: token not withheld: %q", name, got)
			}
		}
	})
}

func TestCLIClientReportsAFailureTheChildPutOnStdout(t *testing.T) {
	bin := fakeClaudeBinary(t, `
printf 'config write failed: ENOENT'
exit 3
`)
	c := &CLIClient{binary: bin}
	_, _, err := c.Reflect(context.Background(), "prompt")
	if err == nil {
		t.Fatal("expected the failing child to surface an error")
	}
	if !strings.Contains(err.Error(), "ENOENT") {
		t.Errorf("the child's own explanation was dropped:\n%s", err.Error())
	}
	if !strings.Contains(err.Error(), "exit status 3") {
		t.Errorf("the exit status is missing too:\n%s", err.Error())
	}
}
