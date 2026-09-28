package selfupdate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFindChecksumAsset(t *testing.T) {
	rel := &Release{
		TagName: "v1.2.3",
		Assets: []Asset{
			{Name: "ghost_1.2.3_linux_amd64.tar.gz", BrowserDownloadURL: "https://example.com/a"},
			{Name: "checksums.txt", BrowserDownloadURL: "https://example.com/checksums.txt"},
		},
	}

	asset, err := FindChecksumAsset(rel)
	if err != nil {
		t.Fatalf("FindChecksumAsset: %v", err)
	}
	if asset.Name != "checksums.txt" {
		t.Errorf("asset.Name = %q, want checksums.txt", asset.Name)
	}

	if _, err := FindChecksumAsset(&Release{TagName: "v1.2.3"}); err == nil {
		t.Fatal("expected error when checksums.txt is missing from the release")
	}
}

func TestVerifyChecksum(t *testing.T) {
	data := []byte("pretend this is a release archive")
	sum := sha256.Sum256(data)
	hexSum := hex.EncodeToString(sum[:])
	assetName := "ghost_1.2.3_linux_amd64.tar.gz"

	t.Run("matching checksum passes", func(t *testing.T) {
		manifest := hexSum + "  " + assetName + "\n" +
			"deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef  ghost_1.2.3_darwin_arm64.tar.gz\n"
		if err := VerifyChecksum(data, manifest, assetName); err != nil {
			t.Errorf("VerifyChecksum: %v", err)
		}
	})

	t.Run("tampered archive is rejected", func(t *testing.T) {
		manifest := hexSum + "  " + assetName + "\n"
		tampered := append([]byte(nil), data...)
		tampered[0] ^= 0xFF
		err := VerifyChecksum(tampered, manifest, assetName)
		if err == nil {
			t.Fatal("expected checksum mismatch error for tampered archive")
		}
		if !strings.Contains(err.Error(), "mismatch") {
			t.Errorf("error should mention mismatch, got: %v", err)
		}
	})

	t.Run("missing manifest entry is rejected", func(t *testing.T) {
		manifest := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef  some_other_file.tar.gz\n"
		if err := VerifyChecksum(data, manifest, assetName); err == nil {
			t.Fatal("expected error when the asset has no checksum entry")
		}
	})

	t.Run("uppercase manifest digest still matches", func(t *testing.T) {
		manifest := strings.ToUpper(hexSum) + "  " + assetName + "\n"
		if err := VerifyChecksum(data, manifest, assetName); err != nil {
			t.Errorf("VerifyChecksum with uppercase digest: %v", err)
		}
	})

	t.Run("goreleaser binary-mode asterisk prefix is tolerated", func(t *testing.T) {
		manifest := hexSum + "  *" + assetName + "\n"
		if err := VerifyChecksum(data, manifest, assetName); err != nil {
			t.Errorf("VerifyChecksum with '*' prefix: %v", err)
		}
	})
}

// useTestDeadline shortens one of the package's request deadlines, restoring
// the production value when the test ends. It lets a test make the deadline
// shorter than a test can wait, without touching the shipped bounds.
func useTestDeadline(t *testing.T, dst *time.Duration, timeout time.Duration) {
	t.Helper()
	original := *dst
	t.Cleanup(func() { *dst = original })
	*dst = timeout
}

// writeChunked streams body to w in 32 KiB chunks, stopping early if the client
// hangs up. A capped client stops reading, so the handler must not treat the
// resulting write error as a failure.
func writeChunked(w http.ResponseWriter, body []byte) {
	const chunkSize = 32 << 10
	for len(body) > 0 {
		n := min(chunkSize, len(body))
		if _, err := w.Write(body[:n]); err != nil {
			return
		}
		body = body[n:]
	}
}

func TestRequestDeadlinesArePositiveAndOrdered(t *testing.T) {
	// A request with no deadline waits on a stalled connection for as long as
	// the OS allows, which is how `ghost upgrade` hangs forever on a proxy
	// that accepts and then goes quiet.
	if apiTimeout <= 0 {
		t.Error("the release lookup has no deadline: a stalled release API would block ghost upgrade indefinitely")
	}
	if downloadTimeout <= 0 {
		t.Error("the asset download has no deadline: a stalled download would block ghost upgrade indefinitely")
	}
	if downloadTimeout <= apiTimeout {
		t.Errorf("download deadline %s must be longer than the API deadline %s: an archive is a transfer, not a metadata lookup",
			downloadTimeout, apiTimeout)
	}
}

// stallAfter is how long a stalled-server handler waits before answering,
// unless the test unblocks it first. A request with a deadline returns long
// before that, so the assertion is about the deadline, not about how fast the
// machine is.
const stallAfter = 2 * time.Second

// stalledServer serves one request that does not answer for stallAfter, and
// returns a func that releases the handler and closes the server. Releasing it
// before Close keeps teardown instant: srv.Close waits for the handler, and the
// handler is what is being stalled.
func stalledServer(t *testing.T, body string) (url string, release func()) {
	t.Helper()
	releaseHandler := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-releaseHandler:
		case <-time.After(stallAfter):
		}
		_, _ = io.WriteString(w, body)
	}))
	return srv.URL, func() {
		close(releaseHandler)
		srv.Close()
	}
}

func TestFetchReleaseTimesOutOnAStalledAPI(t *testing.T) {
	url, release := stalledServer(t, `{"tag_name":"v0.32.0"}`)
	defer release()

	useTestDeadline(t, &apiTimeout, 20*time.Millisecond)

	// An unbounded request would return the JSON after stallAfter with no
	// error at all, so the error is the whole assertion: no wall-clock
	// threshold to flake on a loaded runner.
	_, err := fetchRelease(context.Background(), url)
	if err == nil {
		t.Fatal("expected a deadline error when the release API does not answer in time")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("want a deadline error, got: %v", err)
	}
}

func TestDownloadTimesOutOnAStalledServer(t *testing.T) {
	url, release := stalledServer(t, "archive bytes")
	defer release()

	useTestDeadline(t, &downloadTimeout, 20*time.Millisecond)

	_, err := Download(context.Background(), url)
	if err == nil {
		t.Fatal("expected a deadline error when the asset server does not answer in time")
	}
	// The deadline is carried on the request's context, so a timeout has to
	// arrive AS a deadline. Asserting only err != nil would pass for an
	// archive-cap refusal or a truncated read — two failures that mean the
	// transfer ended for some other reason and the bound did not do its job.
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("want a deadline error, got: %v", err)
	}
}

// TestRequestsStopWhenTheCallersContextIsCancelled is the other half of the
// bound: the deadlines above are this package's own, and a caller that knows
// better — the user pressed Ctrl-C, the machine is going to sleep — has to be
// able to end a request that is already in flight. Only a context threaded
// into the request can do that, so this fails if the deadline only ever lived
// on a client.
func TestRequestsStopWhenTheCallersContextIsCancelled(t *testing.T) {
	url, release := stalledServer(t, `{"tag_name":"v0.32.0"}`)
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	t.Run("release lookup", func(t *testing.T) {
		if _, err := fetchRelease(ctx, url); err == nil {
			t.Fatal("expected an error once the caller's context was cancelled")
		}
	})
	t.Run("asset download", func(t *testing.T) {
		body, err := Download(ctx, url)
		if err == nil {
			_ = body.Close() //nolint:errcheck
			t.Fatal("expected an error once the caller's context was cancelled")
		}
	})
}

func TestReadArchiveRefusesMoreThanItsCap(t *testing.T) {
	original := archiveCap
	archiveCap = 1024
	t.Cleanup(func() { archiveCap = original })

	t.Run("one byte over the cap is an error", func(t *testing.T) {
		// A truncated buffer is worse than a failure: it would be verified
		// against a digest that never matches, or read as a whole archive.
		_, err := ReadArchive(bytes.NewReader(bytes.Repeat([]byte("a"), 1025)))
		if err == nil {
			t.Fatal("expected an error when the archive is one byte over its cap")
		}
		if !strings.Contains(err.Error(), "cap") {
			t.Errorf("error should name the cap, got: %v", err)
		}
	})

	t.Run("exactly at the cap is accepted", func(t *testing.T) {
		got, err := ReadArchive(bytes.NewReader(bytes.Repeat([]byte("a"), 1024)))
		if err != nil {
			t.Fatalf("ReadArchive at the cap: %v", err)
		}
		if len(got) != 1024 {
			t.Errorf("read %d bytes, want 1024", len(got))
		}
	})
}

func TestDownloadedArchiveOverItsCapIsRefused(t *testing.T) {
	original := archiveCap
	archiveCap = 1024
	t.Cleanup(func() { archiveCap = original })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeChunked(w, bytes.Repeat([]byte("a"), 4096))
	}))
	defer srv.Close()

	body, err := Download(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	defer body.Close() //nolint:errcheck

	if _, err := ReadArchive(body); err == nil {
		t.Fatal("expected an error when the downloaded archive exceeds its cap")
	}
}

func TestDownloadedChecksumsOverTheirCapAreRefused(t *testing.T) {
	// Served at the real cap plus one byte: the production cap is what is
	// under test here, not a shrunken stand-in.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeChunked(w, bytes.Repeat([]byte("a"), int(maxChecksumBytes)+1))
	}))
	defer srv.Close()

	body, err := Download(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	defer body.Close() //nolint:errcheck

	_, err = ReadChecksums(body)
	if err == nil {
		t.Fatal("expected an error when checksums.txt exceeds its cap")
	}
	if !strings.Contains(err.Error(), "cap") {
		t.Errorf("error should name the cap, got: %v", err)
	}
}

func TestFetchReleaseRefusesAnOversizedBody(t *testing.T) {
	// Valid JSON, so an unbounded reader would decode it happily: only the cap
	// can turn this into an error.
	payload := []byte(`{"tag_name":"v0.32.0","assets":[],"pad":"` + strings.Repeat("a", int(maxReleaseJSONBytes)) + `"}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		writeChunked(w, payload)
	}))
	defer srv.Close()

	_, err := fetchRelease(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("expected an error when the release metadata exceeds its cap")
	}
	if !strings.Contains(err.Error(), "cap") {
		t.Errorf("error should name the cap, got: %v", err)
	}
}

func TestFetchReleaseRejectsANonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	if _, err := fetchRelease(context.Background(), srv.URL); err == nil {
		t.Fatal("expected an error for a 404 release API response")
	}
}

func TestFetchReleaseReturnsTheRelease(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept"); got != "application/vnd.github+json" {
			t.Errorf("Accept header = %q, want application/vnd.github+json", got)
		}
		_, _ = io.WriteString(w, `{"tag_name":"v0.33.0","assets":[{"name":"checksums.txt","browser_download_url":"https://example.com/c"}]}`)
	}))
	defer srv.Close()

	rel, err := fetchRelease(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("fetchRelease: %v", err)
	}
	if rel.TagName != "v0.33.0" {
		t.Errorf("TagName = %q, want v0.33.0", rel.TagName)
	}
	asset, err := FindChecksumAsset(rel)
	if err != nil {
		t.Fatalf("FindChecksumAsset: %v", err)
	}
	if asset.BrowserDownloadURL != "https://example.com/c" {
		t.Errorf("BrowserDownloadURL = %q, want https://example.com/c", asset.BrowserDownloadURL)
	}
}
