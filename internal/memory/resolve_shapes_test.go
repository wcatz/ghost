package memory

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestIsPathShapedRecognizesDriveRelativePath pins the separatorless Windows
// shape. "C:repo" is a valid drive-relative path — relative to that drive's
// current directory — and it contains no separator at all, so the shared
// predicate classified it as a project name. IsPathShaped is the one definition
// of "the caller is reporting a location", used by both store resolution and
// mcpserver's repository detection: a writer that read it as a path and a reader
// that read it as a name is exactly the disagreement this predicate exists to
// prevent, and the reviewer's case is the one that slips through a separator
// check.
func TestIsPathShapedRecognizesDriveRelativePath(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		want  bool
	}{
		{"drive-relative path", `C:repo`, true},
		{"drive-relative with a separator", `C:\repo`, true},
		{"drive-relative with a forward slash", `C:/repo`, true},
		{"absolute unix path", "/home/me/ghost", true},
		{"bare project name", "ghost", false},
		{"project id", "a7293a04b38a", false},
		{"name containing a dot", "ghost.exe", false},
		{"lone colon is not enough", ":repo", false},
		{"a multi-character prefix is not a drive", "edge:cache", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsPathShaped(tc.input); got != tc.want {
				t.Errorf("IsPathShaped(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

// TestResolveProjectMatchesRelativeSessionDirectory is the integration half: a
// caller that reports a relative directory, from a repository with no detectable
// remote, must still reach the project recorded at the matching absolute path.
// EvalSymlinks preserves a relative result as relative, so comparing
// "root/project/sub" against a stored "/cwd/root/project" failed, and the
// textual prefilter omitted the stored row entirely — the project was
// unreachable rather than merely mis-compared.
func TestResolveProjectMatchesRelativeSessionDirectory(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	project := filepath.Join(root, "project")
	if err := os.MkdirAll(filepath.Join(project, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}

	s := testStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "relproj", project, "relproj"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	// Stand where the relative path is rooted, the way a shell invoked inside
	// the project would be. Not parallel: t.Chdir changes process state.
	t.Chdir(root)

	gotID, gotName, err := s.ResolveProject(ctx, filepath.Join("project", "sub"))
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if gotID != "relproj" {
		t.Errorf("resolved id = %q, want relproj — a relative session directory must be made absolute before it is compared", gotID)
	}
	if gotName != "relproj" {
		t.Errorf("resolved name = %q, want relproj", gotName)
	}
}
