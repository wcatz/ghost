package ai

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// #751 was the shape of bug these tests exist to catch. The funnel degraded to

const fallbackTempRootEnv = "GHOST_TEST_FALLBACK_TEMP_ROOT"

// pointed at a GHOST_DEV_FORBID_DATA_DIR data dir produces (#723), which is
func brokenScratchRoot(t *testing.T) {
	t.Helper()
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GHOST_SCRATCH_DIR", filepath.Join(blocker, "scratch"))
}

// separately visible — the probe is a child too, and in #751 it was one of the
const childReportLine = `{ printf 'CHILD pwd=%s tmpdir=%s tmp=%s temp=%s\n' "$PWD" "$TMPDIR" "$TMP" "$TEMP" >> "$HARNESS_FALLBACK_REPORT"; }` + "\n"

// of #751, one subtest per backend. It drives each client's own run path with a
func TestEveryHarnessClientChildIsConfinedWhenScratchFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake requires a POSIX shell")
	}

	for _, harness := range []struct {
		name    string
		kind    harnessKind
		minimum int
		script  string
		reflect func(ctx context.Context, bin string) error
	}{
		{
			name:    "claude",
			kind:    harnessClaude,
			minimum: 2,
			script: `if [ "$1" = "--help" ]; then
  printf '%s\n' '--safe-mode' '--restricted' '--strict-mcp-config' '--disable-slash-commands' '--tools' '--disallowedTools' '--setting-sources'
  exit 0
fi
printf '%s' 'KEEP'`,
			reflect: func(ctx context.Context, bin string) error {
				_, _, err := (&CLIClient{binary: bin}).Reflect(ctx, "prompt")
				return err
			},
		},
		{
			name:    "codex",
			kind:    harnessCodex,
			minimum: 2,
			script: `if [ "$1" = "features" ]; then
  printf '%s\n' 'shell_tool stable true' 'unified_exec stable true' 'view_image stable true' 'apps stable true' 'plugins stable true' 'tool_suggest stable true' 'skill_mcp_dependency_install stable true' 'remote_plugin stable true' 'hooks stable true' 'multi_agent stable true'
  exit 0
fi
printf '%s' 'KEEP'`,
			reflect: func(ctx context.Context, bin string) error {
				_, _, err := (&CodexClient{binary: bin}).Reflect(ctx, "prompt")
				return err
			},
		},
		{
			name:    "goose",
			kind:    harnessGoose,
			minimum: 1,
			script:  `printf '%s' 'KEEP'`,
			reflect: func(ctx context.Context, bin string) error {
				_, _, err := (&GooseClient{binary: bin}).Reflect(ctx, "prompt")
				return err
			},
		},
		{
			name:    "opencode",
			kind:    harnessOpencode,
			minimum: 2,
			script: `if [ "$1" = "--version" ]; then
  echo '1.18.32'
  exit 0
fi
printf '%s\n' '{"type":"text","part":{"type":"text","text":"KEEP"}}'`,
			reflect: func(ctx context.Context, bin string) error {
				_, _, err := (&OpenCodeClient{binary: bin}).Reflect(ctx, "prompt")
				return err
			},
		},
	} {
		t.Run(harness.name, func(t *testing.T) {
			root := pinFallbackTempRoot(t)
			brokenScratchRoot(t)
			report := filepath.Join(t.TempDir(), "children")
			home := t.TempDir()
			for key, value := range map[string]string{
				"HOME":                    home,
				"USERPROFILE":             home,
				"XDG_CONFIG_HOME":         filepath.Join(home, "config"),
				"XDG_DATA_HOME":           filepath.Join(home, "data"),
				"HARNESS_FALLBACK_REPORT": report,
				"GHOST_PASSTHROUGH_ENV":   "HARNESS_FALLBACK_REPORT",
			} {
				t.Setenv(key, value)
			}

			bin := filepath.Join(t.TempDir(), harness.name)
			if err := os.WriteFile(bin, []byte("#!/bin/sh\nset -e\n"+childReportLine+harness.script+"\n"), 0o755); err != nil {
				t.Fatalf("write fake %s: %v", harness.name, err)
			}
			if err := harness.reflect(context.Background(), bin); err != nil {
				t.Fatalf("Reflect: %v", err)
			}

			children := readChildReports(t, report)
			if len(children) < harness.minimum {
				t.Fatalf("saw %d child invocation(s), want at least %d — a probe that never ran leaves the probe path untested", len(children), harness.minimum)
			}
			for i, child := range children {
				if child.pwd == "" || resolvedComparable(t, child.pwd) == resolvedComparable(t, workingDir(t)) {
					t.Errorf("child %d ran with pwd %q, want a private invocation dir", i, child.pwd)
				}
				if got := resolvedComparable(t, filepath.Dir(child.pwd)); got != resolvedComparable(t, root) {
					t.Errorf("child %d ran in %q, want a direct child of the fallback root %q", i, child.pwd, root)
				}
				if want := "ghost-" + string(harness.kind) + "-"; !strings.HasPrefix(filepath.Base(child.pwd), want) {
					t.Errorf("child %d ran in %q, want a name beginning %q", i, child.pwd, want)
				}
				for key, got := range map[string]string{
					"TMPDIR": child.tmpdir,
					"TMP":    child.tmp,
					"TEMP":   child.temp,
				} {
					if resolvedComparable(t, got) != resolvedComparable(t, child.pwd) {
						t.Errorf("child %d saw %s=%q, want the invocation dir %q", i, key, got, child.pwd)
					}
				}
				if _, err := os.Stat(child.pwd); !os.IsNotExist(err) {
					t.Errorf("invocation dir %s outlived the call (err=%v)", child.pwd, err)
				}
			}
		})
	}
}

func pinFallbackTempRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv(fallbackTempRootEnv, root)
	for _, key := range tempDirKeys {
		t.Setenv(key, root)
	}
	return root
}

func workingDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func resolvedComparable(t *testing.T, p string) string {
	t.Helper()
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(p))
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", p, err)
	}
	return filepath.Join(parent, filepath.Base(p))
}

type childReport struct {
	pwd    string
	tmpdir string
	tmp    string
	temp   string
}

func readChildReports(t *testing.T, path string) []childReport {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read child report: %v", err)
	}
	var out []childReport
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "CHILD "))
		values := map[string]string{}
		for _, field := range fields {
			key, value, found := strings.Cut(field, "=")
			if !found {
				t.Fatalf("malformed child report line %q", line)
			}
			values[key] = value
		}
		if len(values) != 4 {
			t.Fatalf("malformed child report line %q", line)
		}
		out = append(out, childReport{
			pwd:    values["pwd"],
			tmpdir: values["tmpdir"],
			tmp:    values["tmp"],
			temp:   values["temp"],
		})
	}
	return out
}

// half of #751: with the scratch root unusable, EVERY backend still gets a
func TestHarnessCommandConfinesEveryClientWhenScratchFails(t *testing.T) {
	for _, harness := range []struct {
		name string
		kind harnessKind
	}{
		{name: "claude", kind: harnessClaude},
		{name: "codex", kind: harnessCodex},
		{name: "goose", kind: harnessGoose},
		{name: "opencode", kind: harnessOpencode},
	} {
		t.Run(harness.name, func(t *testing.T) {
			brokenScratchRoot(t)
			root := pinFallbackTempRoot(t)
			env := []string{"PATH=/usr/bin", "TMPDIR=/inherited", "TMP=/inherited", "TEMP=/inherited"}

			cmd, release, err := harnessCommand(context.Background(), "true", nil, env, harness.kind)
			if err != nil {
				t.Fatalf("harnessCommand: %v", err)
			}
			defer release()

			if cmd.Dir == "" {
				t.Fatal("cmd.Dir is empty: the child would run in Ghost's own working directory")
			}
			if got := filepath.Dir(cmd.Dir); got != root {
				t.Errorf("cmd.Dir = %q, want a direct child of the fallback root %q", cmd.Dir, root)
			}
			if want := "ghost-" + string(harness.kind) + "-"; !strings.HasPrefix(filepath.Base(cmd.Dir), want) {
				t.Errorf("cmd.Dir = %q, want a name beginning %q so a leaked directory names its harness", cmd.Dir, want)
			}
			for _, key := range tempDirKeys {
				if got := envValue(cmd.Env, key); got != cmd.Dir {
					t.Errorf("%s = %q, want the fallback dir %q", key, got, cmd.Dir)
				}
				count := 0
				for _, kv := range cmd.Env {
					if k, _, _ := strings.Cut(kv, "="); strings.EqualFold(k, key) {
						count++
					}
				}
				if count != 1 {
					t.Errorf("%s appears %d times, want exactly 1", key, count)
				}
			}
			if _, err := os.Stat(cmd.Dir); err != nil {
				t.Fatalf("fallback dir missing before release: %v", err)
			}
			release()
			if _, err := os.Stat(cmd.Dir); !os.IsNotExist(err) {
				t.Errorf("fallback dir %s survived release (err=%v)", cmd.Dir, err)
			}
			release() // must stay safe to call repeatedly
		})
	}
}

// an error and no command at all. A non-nil command here is the #751 shape one
func TestHarnessCommandFailsRatherThanRunUnconfined(t *testing.T) {
	brokenScratchRoot(t)
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, key := range tempDirKeys {
		t.Setenv(key, blocker)
	}
	if probe, err := os.MkdirTemp("", "block-probe-"); err == nil {
		_ = os.RemoveAll(probe)
		t.Skipf("this platform still resolved a writable temp root (%s) with every temp key blocked", os.TempDir())
	}

	cmd, release, err := harnessCommand(context.Background(), "true", nil, []string{"PATH=/usr/bin"}, harnessClaude)
	if err == nil {
		t.Fatalf("harnessCommand returned no error with no scratch root and no fallback dir (cmd=%v)", cmd)
	}
	if cmd != nil {
		t.Errorf("cmd = %v, want nil: a caller that ignored the error must not be able to run it", cmd)
	}
	if !strings.Contains(err.Error(), "scratch") {
		t.Errorf("error %q does not name the scratch failure that caused it", err)
	}
	release() // safe to call even though nothing was created
}

func TestEveryHarnessCommandCallSiteChecksItsError(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	found := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			assign, isAssign := n.(*ast.AssignStmt)
			if !isAssign || len(assign.Rhs) != 1 {
				return true
			}
			call, isCall := assign.Rhs[0].(*ast.CallExpr)
			if !isCall || !isHarnessCommandCall(call) {
				return true
			}
			found++
			where := fset.Position(call.Pos())
			if len(assign.Lhs) < 3 {
				t.Errorf("%s:%d assigns %d of harnessCommand's 3 results: the error cannot be checked",
					name, where.Line, len(assign.Lhs))
				return true
			}
			// The blank identifier in the error's position: the exact #751 shape,
			if ident, isIdent := assign.Lhs[2].(*ast.Ident); isIdent && ident.Name == "_" {
				t.Errorf("%s:%d discards harnessCommand's error with _: the nil command that comes with it "+
					"would be dereferenced, panicking instead of reporting the failure", name, where.Line)
			}
			return true
		})
	}
	if found == 0 {
		t.Fatal("no harnessCommand call sites found: this test has stopped seeing the code it guards")
	}
}

func isHarnessCommandCall(call *ast.CallExpr) bool {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return fun.Name == "harnessCommand"
	case *ast.SelectorExpr:
		return fun.Sel.Name == "harnessCommand"
	}
	return false
}
