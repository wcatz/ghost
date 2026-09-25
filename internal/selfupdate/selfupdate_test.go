package selfupdate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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

// useTestClient swaps one of the package's HTTP clients for a client with the
// given deadline, restoring the production client when the test ends. It lets a
// test make the deadline shorter than a test can wait, without touching the
// shipped timeouts.
func useTestClient(t *testing.T, dst **http.Client, timeout time.Duration) {
	t.Helper()
	original := *dst
	t.Cleanup(func() { *dst = original })
	*dst = &http.Client{Timeout: timeout}
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

func TestClientsHaveDeadlines(t *testing.T) {
	// A client with no Timeout waits on a stalled connection for as long as
	// the OS allows, which is how `ghost upgrade` hangs forever on a proxy
	// that accepts and then goes quiet.
	if apiClient.Timeout <= 0 {
		t.Error("apiClient has no deadline: a stalled release API would block ghost upgrade indefinitely")
	}
	if downloadClient.Timeout <= 0 {
		t.Error("downloadClient has no deadline: a stalled download would block ghost upgrade indefinitely")
	}
	if downloadClient.Timeout <= apiClient.Timeout {
		t.Errorf("downloadClient deadline %s must be longer than the API deadline %s: an archive is a transfer, not a metadata lookup",
			downloadClient.Timeout, apiClient.Timeout)
	}
}

func TestFetchReleaseTimesOutOnAStalledAPI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Longer than the test deadline, shorter than a real upgrade: the
		// answer arrives, just too late.
		time.Sleep(300 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"tag_name":"v0.32.0"}`)
	}))
	defer srv.Close()

	useTestClient(t, &apiClient, 20*time.Millisecond)

	start := time.Now()
	if _, err := fetchRelease(srv.URL); err == nil {
		t.Fatal("expected a deadline error when the release API does not answer in time")
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Errorf("fetchRelease returned after %s: nothing stopped it from waiting on a stalled connection", elapsed)
	}
}

func TestDownloadTimesOutOnAStalledServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = io.WriteString(w, "archive bytes")
	}))
	defer srv.Close()

	useTestClient(t, &downloadClient, 20*time.Millisecond)

	start := time.Now()
	if _, err := Download(srv.URL); err == nil {
		t.Fatal("expected a deadline error when the asset server does not answer in time")
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Errorf("Download returned after %s: nothing stopped it from waiting on a stalled connection", elapsed)
	}
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

	body, err := Download(srv.URL)
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

	body, err := Download(srv.URL)
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

	_, err := fetchRelease(srv.URL)
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

	if _, err := fetchRelease(srv.URL); err == nil {
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

	rel, err := fetchRelease(srv.URL)
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
