package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestImportRefusesAnUnreadableArtifactBeforeOpeningTheStore pins the order of
// the two opens: the artifact is judged first, so a file this build refuses as a
// whole leaves no config file and no database behind. Opening the store first
// would have bootstrap create the config, migrate the database and seed the
// builtin rows before the artifact was even read.
func TestImportRefusesAnUnreadableArtifactBeforeOpeningTheStore(t *testing.T) {
	const header = `{"type":"header","schema_version":1}` + "\n"
	cases := []struct {
		name string
		body string // empty means the path is not created
		want string
	}{
		{
			name: "unknown record type",
			body: header + `{"type":"bogus"}` + "\n",
			want: "unknown record type",
		},
		{
			name: "payload does not match the type",
			body: header + `{"type":"memory","task":{"id":"t1","project_id":"p1","title":"t","status":"pending","priority":2}}` + "\n",
			want: `"memory" payload`,
		},
		{
			name: "missing artifact",
			want: "no artifact at",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dataHome := filepath.Join(root, "data")
			configHome := filepath.Join(root, "config")
			t.Setenv("XDG_DATA_HOME", dataHome)
			t.Setenv("XDG_CONFIG_HOME", configHome)

			path := filepath.Join(root, "artifact.jsonl")
			if tc.body != "" {
				if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
					t.Fatalf("write artifact: %v", err)
				}
			}

			artifact, store, err := openImportRun(path, true)
			if err == nil {
				if store != nil {
					_ = store.Close()
				}
				t.Fatalf("openImportRun accepted %s; a refused artifact must be reported", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to contain %q", err, tc.want)
			}
			if artifact != nil || store != nil {
				t.Errorf("a refused artifact handed back an artifact or store")
			}
			for _, path := range []string{
				filepath.Join(dataHome, "ghost", "ghost.db"),
				filepath.Join(configHome, "ghost", "config.yaml"),
			} {
				if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
					t.Errorf("a refused artifact left %s behind (stat error %v)", path, statErr)
				}
			}
		})
	}
}

// TestImportPreflightKeepsPerLineDamageForTheReport: only a whole-file refusal
// happens before the store is opened. A line that is not JSON is one damaged
// record among others, and the run still opens the store so it can report the
// rejected line and import the rest.
func TestImportPreflightKeepsPerLineDamageForTheReport(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	path := filepath.Join(root, "artifact.jsonl")
	body := `{"type":"header","schema_version":1}` + "\n" + "not json\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write artifact: %v", err)
	}

	f, store, err := openImportRun(path, true)
	if err != nil {
		t.Fatalf("a per-line refusal must reach the report, not stop the run before the store: %v", err)
	}
	if f == nil {
		t.Fatal("a per-line refusal did not hand back the parsed artifact")
	}
	defer store.Close() //nolint:errcheck
}

// TestImportPreflightOpensTheStoreForAReadableArtifact is the control: a file
// the preflight accepts still reaches bootstrap, so the refusal above is about
// the artifact rather than about never opening a store.
func TestImportPreflightOpensTheStoreForAReadableArtifact(t *testing.T) {
	root := t.TempDir()
	dataHome := filepath.Join(root, "data")
	configHome := filepath.Join(root, "config")
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv("XDG_CONFIG_HOME", configHome)
	path := filepath.Join(root, "artifact.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"header","schema_version":1}`+"\n"), 0o600); err != nil {
		t.Fatalf("write artifact: %v", err)
	}

	f, store, err := openImportRun(path, true)
	if err != nil {
		t.Fatalf("openImportRun on a readable artifact: %v", err)
	}
	if f == nil {
		t.Fatal("a readable artifact did not hand back the parsed artifact")
	}
	defer store.Close() //nolint:errcheck

	for _, path := range []string{
		filepath.Join(dataHome, "ghost", "ghost.db"),
		filepath.Join(configHome, "ghost", "config.yaml"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("a readable artifact did not create %s: %v", path, err)
		}
	}
}

// TestRunImportBinaryRefusesBeforeOpeningTheStore builds this package's own
// program and runs `ghost import <file> --apply` against an empty data and
// config directory, so it exercises runImport itself rather than the helpers it
// calls. A refused artifact (an unknown record type, or a path that does not
// exist) must exit 1 and leave neither ghost.db nor config.yaml behind; if
// runImport opened the store before reading the artifact, bootstrap would
// create both and the test would fail. The readable artifact is the control:
// the same run exits 0 and does create them.
func TestRunImportBinaryRefusesBeforeOpeningTheStore(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "ghost-under-test")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	const header = `{"type":"header","schema_version":1}` + "\n"
	cases := []struct {
		name     string
		body     string // empty means the path is not created
		wantExit int
		wantErr  string
		wantDB   bool
	}{
		{name: "unknown record type", body: header + `{"type":"bogus"}` + "\n", wantExit: 1, wantErr: "unknown record type"},
		{name: "missing artifact", wantExit: 1, wantErr: "no artifact at"},
		{name: "readable artifact", body: header, wantExit: 0, wantDB: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dataHome := filepath.Join(root, "data")
			configHome := filepath.Join(root, "config")
			home := filepath.Join(root, "home")
			path := filepath.Join(root, "artifact.jsonl")
			if tc.body != "" {
				if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
					t.Fatalf("write artifact: %v", err)
				}
			}

			cmd := exec.Command(bin, "import", path, "--apply")
			cmd.Env = []string{
				"PATH=" + os.Getenv("PATH"),
				"HOME=" + home,
				"XDG_DATA_HOME=" + dataHome,
				"XDG_CONFIG_HOME=" + configHome,
			}
			out, err := cmd.CombinedOutput()
			exit := 0
			if err != nil {
				var ee *exec.ExitError
				if !errors.As(err, &ee) {
					t.Fatalf("run ghost import: %v", err)
				}
				exit = ee.ExitCode()
			}
			if exit != tc.wantExit {
				t.Errorf("exit = %d, want %d\n%s", exit, tc.wantExit, out)
			}
			if !strings.Contains(string(out), tc.wantErr) {
				t.Errorf("output does not contain %q:\n%s", tc.wantErr, out)
			}
			for _, p := range []string{
				filepath.Join(dataHome, "ghost", "ghost.db"),
				filepath.Join(configHome, "ghost", "config.yaml"),
			} {
				_, statErr := os.Stat(p)
				if tc.wantDB && statErr != nil {
					t.Errorf("a readable artifact did not create %s: %v", p, statErr)
				}
				if !tc.wantDB && !os.IsNotExist(statErr) {
					t.Errorf("a refused artifact left %s behind (stat error %v)", p, statErr)
				}
			}
		})
	}
}
