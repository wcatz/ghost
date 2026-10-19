package main

// The lifecycle cooldown stamp (lifecycle-<id>.last) has placement rules in
// docs/invariants.md: the run start is stamped only after every precondition
// that can exit non-zero, with one exception (a config file that cannot be
// loaded writes the stamp and a failure marker before exiting 1, so the hook
// does not respawn every turn), and a run turned away by another live run
// writes no stamp. runLifecycle cannot be called from a unit test (it exits the process),
// so these tests drive the BUILT binary. The binary is built with go build into
// the test's temp dir — never a re-exec of the test binary, which under go test
// is the whole suite — and runs in an environment built from an allowlist with
// HOME, XDG_* and TMPDIR all inside the temp dir.

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/mcpinit"
	"github.com/wcatz/ghost/internal/memory"
)

const lifecycleStampProject = "stampproj"

// lifecycleStampSandbox is one temp tree holding the built binary, the data and
// config homes, and an empty bin dir that stands in for PATH so no LLM harness
// can be found.
type lifecycleStampSandbox struct {
	bin, dataDir, configDir string
	env                     []string
}

// newLifecycleStampSandbox builds ghost and creates a store holding one project.
// The project must exist: TouchLifecycleStart writes nothing for an identifier
// that resolves to nothing, so without it every "no stamp" assertion would pass
// vacuously.
func newLifecycleStampSandbox(t *testing.T) *lifecycleStampSandbox {
	t.Helper()
	if testing.Short() {
		t.Skip("builds the ghost binary")
	}
	root := t.TempDir()
	sb := &lifecycleStampSandbox{
		bin:       filepath.Join(root, "ghost"),
		dataDir:   filepath.Join(root, "data", "ghost"),
		configDir: filepath.Join(root, "config", "ghost"),
	}
	for _, d := range []string{sb.dataDir, sb.configDir, filepath.Join(root, "emptybin"), filepath.Join(root, "tmp"), filepath.Join(root, "cache"), filepath.Join(root, "state")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	build := exec.Command("go", "build", "-o", sb.bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	sb.env = []string{
		"HOME=" + root,
		"XDG_DATA_HOME=" + filepath.Join(root, "data"),
		"XDG_CONFIG_HOME=" + filepath.Join(root, "config"),
		"XDG_CACHE_HOME=" + filepath.Join(root, "cache"),
		"XDG_STATE_HOME=" + filepath.Join(root, "state"),
		"TMPDIR=" + filepath.Join(root, "tmp"),
		"PATH=" + filepath.Join(root, "emptybin"),
		"GHOST_EMBEDDING_ENABLED=false",
	}

	db, err := memory.OpenDB(filepath.Join(sb.dataDir, "ghost.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close() //nolint:errcheck
	if err := memory.NewStore(db, nil).EnsureProject(context.Background(), lifecycleStampProject, "", lifecycleStampProject); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	return sb
}

func (sb *lifecycleStampSandbox) stamp() string {
	return filepath.Join(sb.dataDir, "lifecycle-"+lifecycleStampProject+".last")
}

// run executes `ghost lifecycle --project <p>` and returns its exit code and stderr.
func (sb *lifecycleStampSandbox) run(t *testing.T) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, sb.bin, "lifecycle", "--project", lifecycleStampProject)
	cmd.Env = sb.env
	cmd.Dir = filepath.Dir(sb.bin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return 0, stderr.String()
	}
	ee, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("running lifecycle: %v\n%s", err, stderr.String())
	}
	return ee.ExitCode(), stderr.String()
}

// TestLifecycleBadConfigWritesTheStamp pins the one exception to "no stamp on a
// failed precondition": a config file that cannot be loaded exits 1, but only
// after recording the failure and stamping the start, so the stop hook's
// cooldown holds instead of respawning the run every turn.
func TestLifecycleBadConfigWritesTheStamp(t *testing.T) {
	sb := newLifecycleStampSandbox(t)
	// A config file that does not parse: config.Load returns an error, and
	// runLifecycle exits 1 on it.
	if err := os.WriteFile(filepath.Join(sb.configDir, "config.yaml"), []byte("reflection: [unclosed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stderr := sb.run(t)
	if code == 0 {
		t.Fatalf("exit = 0, want non-zero for a config that does not parse\n%s", stderr)
	}
	if !strings.Contains(stderr, "load config") {
		t.Fatalf("the run did not exit on the config load (the precondition under test):\n%s", stderr)
	}
	if st, err := os.Stat(sb.stamp()); err != nil || !st.Mode().IsRegular() {
		t.Fatalf("a run that exited on a bad config left no cooldown stamp (stat err = %v)\n%s", err, stderr)
	}
}

func TestLifecycleLockRefusedWritesNoStamp(t *testing.T) {
	for _, tc := range []struct {
		name     string
		priorAge time.Duration // 0: no stamp exists before the run
	}{
		{"no prior stamp", 0},
		{"prior stamp is left untouched", 3 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sb := newLifecycleStampSandbox(t)
			const priorBody = "2000-01-01T00:00:00Z\n"
			var priorMod time.Time
			if tc.priorAge > 0 {
				if err := os.WriteFile(sb.stamp(), []byte(priorBody), 0o600); err != nil {
					t.Fatal(err)
				}
				priorMod = time.Now().Add(-tc.priorAge).Truncate(time.Second)
				if err := os.Chtimes(sb.stamp(), priorMod, priorMod); err != nil {
					t.Fatal(err)
				}
			}

			// Take the per-project claim the way the product does, against the
			// same data dir. This process stays alive, so the binary sees a live
			// holder and is turned away.
			t.Setenv("XDG_DATA_HOME", filepath.Dir(sb.dataDir))
			release, ok, err := mcpinit.AcquireLifecycleLock(lifecycleStampProject)
			if err != nil || !ok {
				t.Fatalf("AcquireLifecycleLock = ok %v, err %v; want the claim", ok, err)
			}
			defer release()

			code, stderr := sb.run(t)
			if !strings.Contains(stderr, "another lifecycle is already running") {
				t.Fatalf("the run was not turned away by the held claim (exit %d):\n%s", code, stderr)
			}

			st, statErr := os.Stat(sb.stamp())
			if tc.priorAge == 0 {
				if !os.IsNotExist(statErr) {
					t.Fatalf("a refused run wrote a cooldown stamp (stat err = %v)", statErr)
				}
				return
			}
			if statErr != nil {
				t.Fatalf("the prior stamp vanished: %v", statErr)
			}
			if !st.ModTime().Equal(priorMod) {
				t.Errorf("a refused run moved the stamp mtime %s -> %s", priorMod, st.ModTime())
			}
			got, err := os.ReadFile(sb.stamp())
			if err != nil || string(got) != priorBody {
				t.Errorf("a refused run changed the stamp contents: %q (err %v)", got, err)
			}
		})
	}
}

// TestLifecycleGoodRunWritesStamp is the control: with the same sandbox, a run
// that gets past every precondition does stamp, so the two tests above fail for
// the reason they claim rather than because the stamp can never be written here.
func TestLifecycleGoodRunWritesStamp(t *testing.T) {
	sb := newLifecycleStampSandbox(t)
	if _, err := os.Stat(sb.stamp()); !os.IsNotExist(err) {
		t.Fatalf("stamp present before the run (stat err = %v)", err)
	}
	code, stderr := sb.run(t)
	// The phases may fail with no harness on PATH; only the stamp is asserted.
	if strings.Contains(stderr, "another lifecycle is already running") || strings.Contains(stderr, "load config") {
		t.Fatalf("the run did not get past its preconditions (exit %d):\n%s", code, stderr)
	}
	if st, err := os.Stat(sb.stamp()); err != nil || !st.Mode().IsRegular() {
		t.Fatalf("a run that passed its preconditions left no cooldown stamp (stat err = %v)\n%s", err, stderr)
	}
}
