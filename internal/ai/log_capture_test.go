package ai

import (
	"bytes"
	"log/slog"
	"sync"
	"testing"
)

// race #853's stack shows: bytes.(*Buffer).grow from the abandoned probe against

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *lockedBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

func (b *lockedBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}

func captureProcessLogs(t *testing.T) *lockedBuffer {
	t.Helper()
	var logs lockedBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))

	markCaptureInstalled(t)
	t.Cleanup(func() {
		settleAbandonedProbes(t)
		slog.SetDefault(prev)
	})
	return &logs
}
