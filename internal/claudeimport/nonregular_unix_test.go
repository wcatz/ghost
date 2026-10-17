//go:build !windows

package claudeimport

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// Fixtures for a memory directory entry named *.md that is not a regular file.
//
// The directory is written by a session Ghost did not start, so an entry's
// type is as untrusted as its text. Opening a FIFO for reading blocks until a
// writer appears, which wedged the import (and with it ghost_project_context's
// first contact and every ghost mcp init run), and following a symlink stored
// the bytes of a file outside the directory as a memory. Each entry of that
// kind is now refused unopened and named in the failed list.

// importDeadline bounds every call here that could block on a FIFO, so a
// regression is a failure rather than a stuck run.
const importDeadline = 5 * time.Second

// plantMemoryDir points HOME at a temporary directory and creates the memory
// directory ClaudeMemoryDir resolves for a temporary checkout, so the tests
// drive the exported Import exactly as its callers do.
func plantMemoryDir(t *testing.T) (root, projPath, memDir string) {
	t.Helper()
	root = t.TempDir()
	home := filepath.Join(root, "home")
	t.Setenv("HOME", home)
	projPath = filepath.Join(root, "checkout")
	planted := filepath.Join(home, ".claude", "projects", EncodeProjectPath(projPath), "memory")
	if err := os.MkdirAll(planted, 0o700); err != nil {
		t.Fatal(err)
	}
	memDir = ClaudeMemoryDir(projPath)
	if memDir != planted {
		t.Fatalf("ClaudeMemoryDir(%q) = %q, want %q", projPath, memDir, planted)
	}
	return root, projPath, memDir
}

func mkfifo(t *testing.T, path string) {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("mkfifo %s: %v", path, err)
	}
}

// withDeadline runs fn and fails the test if it has not returned within
// importDeadline. On a timeout each FIFO in unblock is opened for writing and
// closed, which hands a blocked reader EOF, so a regression does not leave a
// wedged goroutine behind for the rest of the package run.
func withDeadline(t *testing.T, unblock []string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(importDeadline):
		for _, p := range unblock {
			if w, err := os.OpenFile(p, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
				_ = w.Close()
			}
		}
		<-done
		t.Fatalf("the call was still blocked after %s: a non-regular entry was opened", importDeadline)
	}
}

const regularMemory = "A regular memory file that the import must still store exactly as before."

// importRun is what one Import call under the deadline returned, with the
// store it wrote to and the log it emitted.
type importRun struct {
	imported                    int
	refused, failed, notRegular []string
	store                       *memory.Store
	log                         string
}

func runImport(t *testing.T, projectID, projPath string, unblock []string) importRun {
	t.Helper()
	r := importRun{store: storeFor(t, projectID)}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	var importErr error
	withDeadline(t, unblock, func() {
		r.imported, r.refused, r.failed, r.notRegular, importErr = Import(context.Background(), r.store, projectID, projPath, logger)
	})
	if importErr != nil {
		t.Fatalf("Import: %v", importErr)
	}
	r.log = buf.String()
	return r
}

// assertRefusedUnopened checks that exactly the named entry was refused by
// the import loop's type check, before ParseMemoryFile was reached, and that
// the regular file beside it still imported. ParseMemoryFile refuses the same
// entries itself, but its refusal lands in failed, so the notRegular list and
// the loop's own log line are what show the loop check ran.
func assertRefusedUnopened(t *testing.T, r importRun, name string) {
	t.Helper()
	if !slices.Equal(r.notRegular, []string{name}) {
		t.Errorf("notRegular = %q, want exactly [%s]", r.notRegular, name)
	}
	if !strings.Contains(r.log, `msg="claude import: not a regular file" file=`+name) {
		t.Errorf("%s was not refused by the import loop's type check; log:\n%s", name, r.log)
	}
	if len(r.failed) != 0 {
		t.Errorf("failed = %q, want none: a non-regular entry is not a write a retry can fix", r.failed)
	}
	if len(r.refused) != 0 {
		t.Errorf("refused = %q, want none", r.refused)
	}
	if r.imported != 1 {
		t.Errorf("imported = %d, want 1 (the regular file)", r.imported)
	}
}

func TestImportRefusesAFIFONamedLikeAMemory(t *testing.T) {
	_, projPath, memDir := plantMemoryDir(t)
	writeTempFile(t, memDir, "good.md", regularMemory)
	fifo := filepath.Join(memDir, "x.md")
	mkfifo(t, fifo)

	assertRefusedUnopened(t, runImport(t, "p-fifo", projPath, []string{fifo}), "x.md")
}

func TestImportRefusesASymlinkToAnOutsideFile(t *testing.T) {
	root, projPath, memDir := plantMemoryDir(t)
	writeTempFile(t, memDir, "good.md", regularMemory)
	const outsideText = "text from a file outside the claude memory directory, read via a link"
	outside := filepath.Join(root, "outside.txt")
	if err := os.WriteFile(outside, []byte(outsideText), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(memDir, "a_link.md")); err != nil {
		t.Fatal(err)
	}

	r := runImport(t, "p-link", projPath, nil)
	assertRefusedUnopened(t, r, "a_link.md")
	rows, err := r.store.GetAll(context.Background(), "p-link", 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range rows {
		if strings.Contains(m.Content, "outside the claude memory directory") {
			t.Errorf("the link's target was stored as a memory: %q", m.Content)
		}
	}
}

func TestImportRefusesASymlinkToAFIFO(t *testing.T) {
	root, projPath, memDir := plantMemoryDir(t)
	writeTempFile(t, memDir, "good.md", regularMemory)
	fifo := filepath.Join(root, "pipe")
	mkfifo(t, fifo)
	if err := os.Symlink(fifo, filepath.Join(memDir, "p_link.md")); err != nil {
		t.Fatal(err)
	}

	assertRefusedUnopened(t, runImport(t, "p-linkfifo", projPath, []string{fifo}), "p_link.md")
}

// TestImportStaysSilentOnANonRegularMemoryIndex pins that MEMORY.md, which the
// import never stores, is skipped silently whatever its type. Before the type
// check a symlinked MEMORY.md was followed and then skipped by name, so a
// dotfiles setup that links it must not start reporting it on every init.
func TestImportStaysSilentOnANonRegularMemoryIndex(t *testing.T) {
	for _, tc := range []struct {
		name  string
		plant func(t *testing.T, root, memDir string) (unblock []string)
	}{
		{"symlink", func(t *testing.T, root, memDir string) []string {
			target := filepath.Join(root, "index.md")
			if err := os.WriteFile(target, []byte("# Index\n- [good.md](good.md)\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(memDir, "MEMORY.md")); err != nil {
				t.Fatal(err)
			}
			return nil
		}},
		{"fifo", func(t *testing.T, _, memDir string) []string {
			fifo := filepath.Join(memDir, "MEMORY.md")
			mkfifo(t, fifo)
			return []string{fifo}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, projPath, memDir := plantMemoryDir(t)
			writeTempFile(t, memDir, "good.md", regularMemory)
			unblock := tc.plant(t, root, memDir)

			r := runImport(t, "p-index-"+tc.name, projPath, unblock)
			if r.imported != 1 || len(r.refused) != 0 || len(r.failed) != 0 || len(r.notRegular) != 0 {
				t.Errorf("imported=%d refused=%q failed=%q notRegular=%q, want 1 and nothing reported",
					r.imported, r.refused, r.failed, r.notRegular)
			}
			if strings.Contains(r.log, "MEMORY.md") {
				t.Errorf("MEMORY.md was logged:\n%s", r.log)
			}
		})
	}
}

// TestImportStillImportsARegularMemoryFile is the control: a regular file and
// a subdirectory named *.md behave exactly as before — the file is stored and
// the directory is skipped silently, not reported.
func TestImportStillImportsARegularMemoryFile(t *testing.T) {
	_, projPath, memDir := plantMemoryDir(t)
	writeTempFile(t, memDir, "good.md", regularMemory)
	if err := os.Mkdir(filepath.Join(memDir, "nested.md"), 0o700); err != nil {
		t.Fatal(err)
	}

	r := runImport(t, "p-control", projPath, nil)
	if r.imported != 1 || len(r.refused) != 0 || len(r.failed) != 0 || len(r.notRegular) != 0 {
		t.Fatalf("imported=%d refused=%q failed=%q notRegular=%q, want 1 and none", r.imported, r.refused, r.failed, r.notRegular)
	}
	rows, err := r.store.GetAll(context.Background(), "p-control", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Content != regularMemory {
		t.Errorf("stored rows = %+v, want the one regular file's content", rows)
	}
}

// TestParseMemoryFileRefusesANonRegularPath covers the exported reader on its
// own, for a caller that hands it a path without going through the import
// loop's type check.
func TestParseMemoryFileRefusesANonRegularPath(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "outside.txt")
	if err := os.WriteFile(outside, []byte("text from a file outside the memory directory, read via a link"), 0o600); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(root, "x.md")
	mkfifo(t, fifo)
	link := filepath.Join(root, "a_link.md")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	fifoTarget := filepath.Join(root, "pipe")
	mkfifo(t, fifoTarget)
	fifoLink := filepath.Join(root, "p_link.md")
	if err := os.Symlink(fifoTarget, fifoLink); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, path, unblock string
	}{
		{"fifo", fifo, fifo},
		{"symlink_to_an_outside_file", link, ""},
		{"symlink_to_a_fifo", fifoLink, fifoTarget},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var (
				content string
				err     error
			)
			var unblock []string
			if tc.unblock != "" {
				unblock = []string{tc.unblock}
			}
			withDeadline(t, unblock, func() {
				content, _, _, _, err = ParseMemoryFile(tc.path)
			})
			if err == nil {
				t.Errorf("ParseMemoryFile(%s) = %q, nil; want a refusal", tc.name, content)
			}
			if content != "" {
				t.Errorf("ParseMemoryFile(%s) returned content %q", tc.name, content)
			}
		})
	}
}

// TestOpenMemoryFileLayers pins each layer of openMemoryFile on its own,
// since each of the others would still refuse what it refuses. The Lstat is
// the classification and names what it found; the open flags are the
// enforcement for an entry swapped in after the Lstat, which no test can time,
// so they are exercised directly as manifest_nofollow_unix_test.go does.
func TestOpenMemoryFileLayers(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.md")
	if err := os.WriteFile(target, []byte(regularMemory), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link.md")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(root, "pipe.md")
	mkfifo(t, fifo)

	t.Run("the_lstat_refuses_a_link_by_type", func(t *testing.T) {
		// Without the Lstat, O_NOFOLLOW would still refuse the link, but
		// with ELOOP; the Lstat is what reports it as a link.
		f, err := openMemoryFile(link)
		if err == nil {
			_ = f.Close()
			t.Fatal("openMemoryFile followed a link")
		}
		if errors.Is(err, syscall.ELOOP) || !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("err = %v, want the Lstat's not-a-regular-file refusal", err)
		}
	})

	t.Run("the_flags_refuse_a_link_without_the_lstat", func(t *testing.T) {
		f, err := os.OpenFile(link, memoryFileOpenFlags, 0)
		if err == nil {
			_ = f.Close()
			t.Fatal("memoryFileOpenFlags followed a link")
		}
		if !errors.Is(err, syscall.ELOOP) {
			t.Errorf("err = %v, want ELOOP", err)
		}
	})

	t.Run("the_flags_open_a_fifo_without_blocking", func(t *testing.T) {
		var (
			f   *os.File
			err error
		)
		withDeadline(t, []string{fifo}, func() {
			f, err = os.OpenFile(fifo, memoryFileOpenFlags, 0)
		})
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer f.Close() //nolint:errcheck
		info, err := f.Stat()
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().IsRegular() {
			t.Error("the fstat reports a FIFO as a regular file, so it could not refuse one")
		}
	})
}
