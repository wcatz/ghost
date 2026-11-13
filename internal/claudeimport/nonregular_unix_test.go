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

// A memory directory entry named *.md that is not a regular file is as
// untrusted as its text: opening a FIFO blocks until a writer appears (which
// wedged the import), and following a symlink stored a file from outside the
// directory. Each such entry is refused unopened and named in notRegular.

// importDeadline bounds every call that could block on a FIFO, so a regression
// is a failure rather than a stuck run.
const importDeadline = 5 * time.Second

// plantMemoryDir points HOME at a temporary directory and creates the memory
// directory ClaudeMemoryDir resolves for a temporary checkout.
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
	if memDir = ClaudeMemoryDir(projPath); memDir != planted {
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

// withDeadline runs fn and fails if it outlives importDeadline. On a timeout
// each FIFO in unblock is opened for writing and closed so the blocked reader
// gets EOF instead of leaking a goroutine.
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

// importRun is what one Import call under the deadline returned.
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

// nothingReported fails unless exactly the regular file imported and no list
// carries an entry.
func (r importRun) nothingReported(t *testing.T) {
	t.Helper()
	if r.imported != 1 || len(r.refused)+len(r.failed)+len(r.notRegular) != 0 {
		t.Errorf("imported=%d refused=%q failed=%q notRegular=%q, want 1 and nothing reported",
			r.imported, r.refused, r.failed, r.notRegular)
	}
}

func TestImportRefusesNonRegularMemoryEntries(t *testing.T) {
	for _, tc := range []struct {
		name, entry string
		plant       func(t *testing.T, root, path string) (unblock []string)
	}{
		{"fifo", "x.md", func(t *testing.T, _, path string) []string {
			mkfifo(t, path)
			return []string{path}
		}},
		{"symlink_to_an_outside_file", "a_link.md", func(t *testing.T, root, path string) []string {
			outside := filepath.Join(root, "outside.txt")
			if err := os.WriteFile(outside, []byte("text from a file outside the claude memory directory, read via a link"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, path); err != nil {
				t.Fatal(err)
			}
			return nil
		}},
		{"symlink_to_a_fifo", "p_link.md", func(t *testing.T, root, path string) []string {
			fifo := filepath.Join(root, "pipe")
			mkfifo(t, fifo)
			if err := os.Symlink(fifo, path); err != nil {
				t.Fatal(err)
			}
			return []string{fifo}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, projPath, memDir := plantMemoryDir(t)
			writeTempFile(t, memDir, "good.md", regularMemory)
			unblock := tc.plant(t, root, filepath.Join(memDir, tc.entry))

			r := runImport(t, "p-"+tc.name, projPath, unblock)
			// Only the loop's own type check logs this line; ParseMemoryFile's
			// refusal would land in failed instead.
			if !slices.Equal(r.notRegular, []string{tc.entry}) ||
				!strings.Contains(r.log, `msg="claude import: not a regular file" file=`+tc.entry) {
				t.Errorf("notRegular = %q, want [%s] refused by the loop's type check; log:\n%s", r.notRegular, tc.entry, r.log)
			}
			if len(r.failed)+len(r.refused) != 0 || r.imported != 1 {
				t.Errorf("imported=%d failed=%q refused=%q, want 1 and none", r.imported, r.failed, r.refused)
			}
			rows, err := r.store.GetAll(context.Background(), "p-"+tc.name, 100)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range rows {
				if strings.Contains(m.Content, "outside the claude memory directory") {
					t.Errorf("the link's target was stored as a memory: %q", m.Content)
				}
			}
		})
	}
}

// MEMORY.md is never stored and is skipped silently whatever its type, so a
// dotfiles setup that links it must not be reported on every init.
func TestImportStaysSilentOnANonRegularMemoryIndex(t *testing.T) {
	for _, kind := range []string{"symlink", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			root, projPath, memDir := plantMemoryDir(t)
			writeTempFile(t, memDir, "good.md", regularMemory)
			index := filepath.Join(memDir, "MEMORY.md")
			var unblock []string
			if kind == "fifo" {
				mkfifo(t, index)
				unblock = []string{index}
			} else {
				target := filepath.Join(root, "index.md")
				if err := os.WriteFile(target, []byte("# Index\n- [good.md](good.md)\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, index); err != nil {
					t.Fatal(err)
				}
			}
			r := runImport(t, "p-index-"+kind, projPath, unblock)
			r.nothingReported(t)
			if strings.Contains(r.log, "MEMORY.md") {
				t.Errorf("MEMORY.md was logged:\n%s", r.log)
			}
		})
	}
}

// The control: a regular file is stored and a subdirectory named *.md is
// skipped silently.
func TestImportStillImportsARegularMemoryFile(t *testing.T) {
	_, projPath, memDir := plantMemoryDir(t)
	writeTempFile(t, memDir, "good.md", regularMemory)
	if err := os.Mkdir(filepath.Join(memDir, "nested.md"), 0o700); err != nil {
		t.Fatal(err)
	}
	r := runImport(t, "p-control", projPath, nil)
	r.nothingReported(t)
	rows, err := r.store.GetAll(context.Background(), "p-control", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Content != regularMemory {
		t.Errorf("stored rows = %+v, want the one regular file's content", rows)
	}
}

// TestOpenMemoryFileLayers pins each layer of openMemoryFile on its own, since
// each of the others would still refuse what it refuses. The Lstat names what
// it found; the open flags enforce for an entry swapped in after the Lstat,
// which no test can time, so they are exercised directly.
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
		if info, err := f.Stat(); err != nil || info.Mode().IsRegular() {
			t.Errorf("fstat = %v (err %v), want a non-regular mode so a FIFO can be refused", info, err)
		}
	})
}
