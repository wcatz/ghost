package main

import (
	"os"
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
