//go:build !windows

package mcpinit

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/embedding"
)

// The log and the Ollama down-marker live in the data dir, which every local
// process can write, so an entry wearing their names is as untrusted as its
// bytes: a FIFO blocked the open until a peer appeared, and a symlink was
// followed to whatever it named. Each case runs under the three-second
// deadline of withMomentMarkerDeadline so a regression fails rather than hangs.

func TestOpenLogForAppendRefusesFIFO(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lifecycle.log")
	mkfifoAt(t, path)

	var f *os.File
	var err error
	withMomentMarkerDeadline(t, []string{path}, func() {
		f, err = openLogForAppend(path)
	})
	if f != nil {
		_ = f.Close()
	}
	if err == nil {
		t.Fatal("openLogForAppend on a FIFO returned no error")
	}
}

func TestOpenLogForAppendRefusesSymlinkToAFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere.txt")
	if err := os.WriteFile(target, []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "obsidian-sync.log")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}

	f, err := openLogForAppend(path)
	if f != nil {
		_, _ = f.WriteString("through the link\n")
		_ = f.Close()
	}
	if err == nil {
		t.Error("openLogForAppend followed a symlink at the log path")
	}
	if b, rerr := os.ReadFile(target); rerr != nil || string(b) != "keep\n" {
		t.Errorf("the link target was written through: %q (err %v)", b, rerr)
	}
}

func TestReportOllamaDownDurationIgnoresFIFO(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, embedding.OllamaDownMarkerFilename)
	mkfifoAt(t, path)

	var out bytes.Buffer
	withMomentMarkerDeadline(t, []string{path}, func() {
		reportOllamaDownDuration(&out, dir)
	})
	if out.Len() != 0 {
		t.Errorf("a FIFO marker produced output: %q", out.String())
	}
}

func TestReportOllamaDownDurationIgnoresSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real")
	if err := os.WriteFile(target, []byte("2020-01-01T00:00:00Z\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, embedding.OllamaDownMarkerFilename)); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	reportOllamaDownDuration(&out, dir)
	if out.Len() != 0 {
		t.Errorf("a symlinked marker was followed: %q", out.String())
	}
}

// GHOST_LOG_FILE names a path the operator chose, so a symlink there is
// followed, while a special file such as a FIFO is still refused.
func TestOperatorLogFollowsSymlinkButRefusesFIFO(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.log")
	if err := os.WriteFile(target, []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "linked.log")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	for name, open := range map[string]func(string) (interface {
		Write([]byte) (int, error)
		Close() error
	}, error){
		"OpenRotatingLog": func(p string) (interface {
			Write([]byte) (int, error)
			Close() error
		}, error) {
			return OpenRotatingLog(p)
		},
		"OpenLogForAppend": func(p string) (interface {
			Write([]byte) (int, error)
			Close() error
		}, error) {
			return OpenLogForAppend(p)
		},
	} {
		t.Run(name+"/symlink", func(t *testing.T) {
			if err := os.WriteFile(target, []byte("keep\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			w, err := open(link)
			if err != nil {
				t.Fatalf("a symlinked operator log was refused: %v", err)
			}
			_, _ = w.Write([]byte("more\n"))
			_ = w.Close()
			if b, _ := os.ReadFile(target); string(b) != "keep\nmore\n" {
				t.Errorf("target holds %q, want the write to land through the link", b)
			}
		})
		t.Run(name+"/fifo", func(t *testing.T) {
			fifo := filepath.Join(t.TempDir(), "ghost.log")
			mkfifoAt(t, fifo)
			var err error
			withMomentMarkerDeadline(t, []string{fifo}, func() {
				w, e := open(fifo)
				if e == nil {
					_ = w.Close()
				}
				err = e
			})
			if err == nil {
				t.Error("a FIFO at the operator log path was accepted")
			}
		})
	}
}

func TestReportOllamaDownDurationIgnoresOversizedMarker(t *testing.T) {
	dir := t.TempDir()
	body := "2020-01-01T00:00:00Z" + strings.Repeat(" ", 8192)
	if err := os.WriteFile(filepath.Join(dir, embedding.OllamaDownMarkerFilename), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	reportOllamaDownDuration(&out, dir)
	if out.Len() != 0 {
		t.Errorf("an oversized marker was parsed from its first bytes: %q", out.String())
	}
}
