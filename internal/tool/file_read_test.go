package tool

import (
	"path/filepath"
	"testing"
)

func TestSafePath(t *testing.T) {
	project := "/home/user/project"

	tests := []struct {
		name    string
		path    string
		wantErr bool
		want    string
	}{
		{
			name:    "relative path within project",
			path:    "src/main.go",
			wantErr: false,
			want:    filepath.Join(project, "src/main.go"),
		},
		{
			name:    "absolute path within project",
			path:    "/home/user/project/src/main.go",
			wantErr: false,
			want:    "/home/user/project/src/main.go",
		},
		{
			name:    "dotdot escapes project",
			path:    "../../etc/passwd",
			wantErr: true,
		},
		{
			name:    "dotdot resolves within project",
			path:    "src/../lib/util.go",
			wantErr: false,
			want:    filepath.Join(project, "lib/util.go"),
		},
		{
			name:    "exact project root",
			path:    "/home/user/project",
			wantErr: false,
			want:    project,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := safePath(project, tt.path)
			if tt.wantErr {
				if err == nil {
					t.Errorf("safePath(%q, %q) expected error, got %q", project, tt.path, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("safePath(%q, %q) unexpected error: %v", project, tt.path, err)
			}
			if got != tt.want {
				t.Errorf("safePath(%q, %q) = %q, want %q", project, tt.path, got, tt.want)
			}
		})
	}
}

func TestResolvePath(t *testing.T) {
	project := "/home/user/project"

	tests := []struct {
		name string
		path string
		want string
	}{
		{
			name: "relative path gets joined",
			path: "src/main.go",
			want: filepath.Join(project, "src/main.go"),
		},
		{
			name: "absolute path stays absolute",
			path: "/tmp/file.txt",
			want: "/tmp/file.txt",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolvePath(project, tt.path)
			if got != tt.want {
				t.Errorf("resolvePath(%q, %q) = %q, want %q", project, tt.path, got, tt.want)
			}
		})
	}
}
