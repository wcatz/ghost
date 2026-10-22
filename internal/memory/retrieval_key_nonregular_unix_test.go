//go:build !windows

package memory

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// The retrieval key lives in the data dir, which every local process can
// write. A FIFO at its path blocked the open until a writer appeared, which
// wedged the stop hook's audit scan and the coordinator's audit phase, and a
// symlink was followed to whatever it named. Each call runs under a
// three-second deadline so a regression fails instead of hanging.

func keyDeadline(t *testing.T, fifo string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
		return
	case <-time.After(3 * time.Second):
	}
	for {
		if w, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = w.Close()
		}
		if r, err := os.OpenFile(fifo, os.O_RDONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = r.Close()
		}
		select {
		case <-done:
			t.Fatal("the key read blocked for more than 3s on a path that is not a regular file")
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func keyPathFor(t *testing.T) (dir, path string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "ghost")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	useQueryKeyDir(t, dir)
	return dir, filepath.Join(dir, queryKeyFileName)
}

func TestRetrievalKeyReadersRefuseFIFO(t *testing.T) {
	_, path := keyPathFor(t)
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	reads := map[string]func() ([]byte, error){
		"ReadRetrievalKey":      ReadRetrievalKey,
		"loadOrCreateQueryKey":  loadOrCreateQueryKey,
		"readPublishedQueryKey": func() ([]byte, error) { return readPublishedQueryKeyOnce(path) },
	}
	for name, read := range reads {
		t.Run(name, func(t *testing.T) {
			var key []byte
			var err error
			keyDeadline(t, path, func() { key, err = read() })
			if err == nil || len(key) != 0 {
				t.Errorf("a FIFO at the key path was accepted: key %d bytes, err %v", len(key), err)
			}
		})
	}
}

func TestRetrievalKeyReadersRefuseSymlink(t *testing.T) {
	dir, path := keyPathFor(t)
	target := filepath.Join(dir, "elsewhere")
	good := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if err := os.WriteFile(target, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	reads := map[string]func() ([]byte, error){
		"ReadRetrievalKey":     ReadRetrievalKey,
		"loadOrCreateQueryKey": loadOrCreateQueryKey,
	}
	for name, read := range reads {
		t.Run(name, func(t *testing.T) {
			key, err := read()
			if err == nil || len(key) != 0 {
				t.Errorf("a symlink at the key path was followed: key %d bytes, err %v", len(key), err)
			}
		})
	}
}
