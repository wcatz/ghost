package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseFloors(t *testing.T) {
	tests := []struct {
		name    string
		spec    string
		want    map[string]float64
		wantErr bool
	}{
		{
			name: "empty spec yields no floors",
			spec: "",
			want: map[string]float64{},
		},
		{
			name: "single floor",
			spec: "supersede_precision=0.60",
			want: map[string]float64{"supersede_precision": 0.60},
		},
		{
			name: "multiple floors",
			spec: "supersede_precision=0.60,supersede_recall=0.45,resolve_precision=0.85,resolve_recall=0.30",
			want: map[string]float64{
				"supersede_precision": 0.60,
				"supersede_recall":    0.45,
				"resolve_precision":   0.85,
				"resolve_recall":      0.30,
			},
		},
		{
			name:    "unknown key",
			spec:    "r5=0.74",
			wantErr: true,
		},
		{
			name:    "missing value",
			spec:    "resolve_precision=",
			wantErr: true,
		},
		{
			name:    "non-numeric value",
			spec:    "resolve_precision=high",
			wantErr: true,
		},
		{
			name:    "missing equals",
			spec:    "resolve_precision",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseFloors(tt.spec)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseFloors(%q) = %v, nil; want error", tt.spec, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseFloors(%q): unexpected error: %v", tt.spec, err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("parseFloors(%q) = %v, want %v", tt.spec, got, tt.want)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("parseFloors(%q)[%s] = %v, want %v", tt.spec, k, got[k], v)
				}
			}
		})
	}
}

func TestCheckFloors(t *testing.T) {
	metrics := func() map[string]float64 {
		return map[string]float64{
			"supersede_precision": 0.71,
			"supersede_recall":    0.62,
			"resolve_precision":   1.00,
			"resolve_recall":      0.50,
		}
	}

	t.Run("empty floors is never a violation", func(t *testing.T) {
		if got := checkFloors(metrics(), map[string]float64{}); len(got) != 0 {
			t.Fatalf("checkFloors with empty floors = %v, want none", got)
		}
	})

	t.Run("floors below observed pass", func(t *testing.T) {
		floors := map[string]float64{
			"supersede_precision": 0.60,
			"supersede_recall":    0.45,
			"resolve_precision":   0.85,
			"resolve_recall":      0.30,
		}
		if got := checkFloors(metrics(), floors); len(got) != 0 {
			t.Fatalf("checkFloors = %v, want none", got)
		}
	})

	t.Run("violation reports metric and values", func(t *testing.T) {
		got := checkFloors(metrics(), map[string]float64{"resolve_recall": 0.80})
		if len(got) != 1 {
			t.Fatalf("checkFloors = %v, want exactly one violation", got)
		}
	})
	t.Run("exact equality is not a violation", func(t *testing.T) {
		got := checkFloors(metrics(), map[string]float64{"supersede_recall": 0.62})
		if len(got) != 0 {
			t.Fatalf("checkFloors at equality = %v, want none", got)
		}
	})
}

func TestSeedOpencodeAuth(t *testing.T) {
	t.Run("copies auth file into scratch data dir with tight perms", func(t *testing.T) {
		scratch := t.TempDir()
		src := filepath.Join(t.TempDir(), "auth.json")
		if err := os.WriteFile(src, []byte(`{"opencode":{"key":"secret"}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := seedOpencodeAuth(scratch, src); err != nil {
			t.Fatalf("seedOpencodeAuth: %v", err)
		}
		dst := filepath.Join(scratch, "data", "opencode", "auth.json")
		b, err := os.ReadFile(dst)
		if err != nil {
			t.Fatalf("seeded file missing: %v", err)
		}
		if string(b) != `{"opencode":{"key":"secret"}}` {
			t.Fatalf("seeded content = %q", b)
		}
		info, err := os.Stat(dst)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("seeded perms = %o, want 600", perm)
		}
	})

	t.Run("empty path is a no-op for local runs", func(t *testing.T) {
		scratch := t.TempDir()
		if err := seedOpencodeAuth(scratch, ""); err != nil {
			t.Fatalf("seedOpencodeAuth with empty path: %v", err)
		}
		if _, err := os.Stat(filepath.Join(scratch, "data")); !os.IsNotExist(err) {
			t.Fatalf("scratch data dir should not be created by a no-op seed: %v", err)
		}
	})

	t.Run("missing source file errors", func(t *testing.T) {
		scratch := t.TempDir()
		if err := seedOpencodeAuth(scratch, filepath.Join(scratch, "nope.json")); err == nil {
			t.Fatal("seedOpencodeAuth with missing source should error")
		}
	})
}
