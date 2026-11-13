package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLifecycleArgumentRefusalsDoNotEchoTheOperandRaw(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the ghost binary")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "ghost")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	key := lifecycleCredential()
	long := strings.Repeat("proj", 100) // 400 bytes, past ArgEchoMaxBytes
	cases := []struct {
		name    string
		args    []string
		leaked  string
		wantSub string
	}{
		{"second positional, token", []string{"alpha", key}, key, "withheld"},
		{"second positional, oversized", []string{"alpha", long}, long, "bytes"},
		{"unknown flag, token", []string{"--project=" + key}, key, "withheld"},
		{"unknown flag, oversized", []string{"--project=" + long}, long, "bytes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(bin, append([]string{"lifecycle"}, tc.args...)...)
			cmd.Env = []string{"HOME=" + dir, "XDG_CONFIG_HOME=" + dir, "XDG_DATA_HOME=" + dir, "PATH=" + os.Getenv("PATH")}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			err := cmd.Run()
			ee, ok := err.(*exec.ExitError)
			if !ok || ee.ExitCode() != 1 {
				t.Fatalf("exit = %v, want exit status 1", err)
			}
			got := stderr.String()
			if strings.Contains(got, tc.leaked) {
				t.Errorf("refusal repeated the operand:\n%s", got)
			}
			if !strings.Contains(got, tc.wantSub) {
				t.Errorf("refusal did not withhold (%q missing):\n%s", tc.wantSub, got)
			}
		})
	}
	// An ordinary operand is still named.
	if _, _, err := parseLifecycleArgs([]string{"alpha", "beta"}); err == nil || !strings.Contains(err.Error(), `"beta"`) {
		t.Errorf("an ordinary extra argument was not named: %v", err)
	}
}
