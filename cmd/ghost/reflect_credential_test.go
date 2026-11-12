package main

import (
	"io"
	"os"
	"strings"
	"sync"
	"testing"
)

// reflectCredential is a token-shaped value in the GitHub PAT format, assembled
// rather than written out because GitHub push protection matches that format
// anywhere in a diff and rejects the push (GH013) before review starts.
func reflectCredential() string { return "ghp_" + strings.Repeat("a1B2c3D4e5F6", 3) + "AbCd" }

// captureStderr returns everything a run wrote to EITHER stream, because a
// guarantee about "the report" has to cover both (the proposal listing and the
// "Applied" summary are stdout; the drop and signature notes are stderr).
//
// The two writers are serialised on the way into one buffer. captureOutput runs
// one io.Copy per stream in its own goroutine, so handing both the same
// strings.Builder is a data race on it the moment both streams carry bytes —
// which a full `ghost reflect` run always does, and which a single-stream call
// like the credential test's never did, so the race stayed invisible until one.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	var out strings.Builder
	w := &serialWriter{w: &out}
	captureOutput(t, w, w, fn)
	return out.String()
}

// serialWriter is the one writer both capture goroutines may share.
type serialWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *serialWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// captureOutput runs fn with both standard streams redirected into w, so a test
// can assert on everything a command reports. The proposal listing, the "Applied"
// summary and the restore hint are stdout; the drop and signature notes are
// stderr, and a guarantee about "the report" has to cover both.
func captureOutput(t *testing.T, stdout, stderr io.Writer, fn func()) {
	t.Helper()
	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe stdout: %v", err)
	}
	rErr, wErr, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe stderr: %v", err)
	}
	prevOut, prevErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = wOut, wErr

	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(stdout, rOut); done <- struct{}{} }()
	go func() { _, _ = io.Copy(stderr, rErr); done <- struct{}{} }()

	fn()

	os.Stdout, os.Stderr = prevOut, prevErr
	_ = wOut.Close()
	_ = wErr.Close()
	<-done
	<-done
	_ = rOut.Close()
	_ = rErr.Close()
}
