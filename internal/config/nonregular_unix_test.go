//go:build !windows

package config

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// loadDeadline bounds every Load here that could block on a FIFO, so a
// regression is a failure rather than a stuck run.
const loadDeadline = 5 * time.Second

// loadResult is what a Load run in a goroutine hands back.
type loadResult struct {
	cfg *Config
	err error
}

// loadWithin runs Load and waits up to loadDeadline for it. On a timeout it
// opens the FIFO for writing and closes it, which releases a reader blocked in
// open, waits for the stuck Load to finish, and fails the test.
func loadWithin(t *testing.T, fifo string) loadResult {
	t.Helper()
	done := make(chan loadResult, 1)
	go func() {
		cfg, err := Load()
		done <- loadResult{cfg, err}
	}()
	select {
	case res := <-done:
		return res
	case <-time.After(loadDeadline):
		if w, err := os.OpenFile(fifo, os.O_WRONLY, 0); err == nil {
			_ = w.Close()
		}
		<-done
		t.Fatalf("Load did not return within %s with a FIFO at %s; it opened the FIFO and blocked for a writer", loadDeadline, fifo)
		return loadResult{}
	}
}

// TestLoad_FIFOAtUserConfigPathIsSkipped plants a FIFO where the user config
// layer lives. Opening a FIFO for reading blocks until a writer appears, which
// stopped every command that loads config. The layer is skipped with the same
// warning an unreadable file gets, and Load returns the defaults.
func TestLoad_FIFOAtUserConfigPathIsSkipped(t *testing.T) {
	isolateConfig(t)
	warnings := captureConfigWarnings(t)

	dir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ghost")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "config.yaml")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	res := loadWithin(t, fifo)
	if res.err != nil {
		t.Fatalf("Load with a FIFO at the user config path = %v, want nil (warned and skipped)", res.err)
	}
	if res.cfg.Embedding.Model != DefaultEmbeddingModel {
		t.Errorf("embedding.model = %q, want the default %q: the FIFO layer must contribute nothing",
			res.cfg.Embedding.Model, DefaultEmbeddingModel)
	}
	if got := warnings.String(); !strings.Contains(got, fifo) || !strings.Contains(got, "skipping") {
		t.Errorf("warning %q must name the skipped FIFO %q", got, fifo)
	}
}

// TestLoad_SymlinkToRegularConfigLoads is the control for the FIFO case: a
// symlinked config.yaml is a normal dotfiles setup, and the regular-file check
// is made on what the link resolves to, so the layer still loads.
func TestLoad_SymlinkToRegularConfigLoads(t *testing.T) {
	isolateConfig(t)
	warnings := captureConfigWarnings(t)

	target := filepath.Join(t.TempDir(), "real-config.yaml")
	if err := os.WriteFile(target, []byte("embedding:\n  model: linked-embed-model\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ghost")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "config.yaml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	res := loadWithin(t, link)
	if res.err != nil {
		t.Fatalf("Load with a symlinked config = %v", res.err)
	}
	if res.cfg.Embedding.Model != "linked-embed-model" {
		t.Errorf("embedding.model = %q, want %q from the symlinked file", res.cfg.Embedding.Model, "linked-embed-model")
	}
	if got := warnings.String(); strings.Contains(got, link) {
		t.Errorf("a symlink to a regular file was warned about: %q", got)
	}
}
