package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// digestOf is the "sha256:<hex>" form GitHub's releases API reports for every
// uploaded asset.
func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TestVerifyAssetDigestAcceptsTheReportedDigest(t *testing.T) {
	data := []byte("pretend this is a release archive")

	tests := []struct {
		name   string
		digest string
	}{
		{name: "the digest GitHub reports", digest: digestOf(data)},
		// Hex is compared case-insensitively — and so is the algorithm name,
		// since the same digest written in either case is the same digest —
		// so a differently-cased field cannot fail an otherwise-matching
		// install.
		{name: "the whole field in uppercase", digest: strings.ToUpper(digestOf(data))},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			asset := &Asset{Name: "ghost_1.2.3_linux_amd64.tar.gz", Digest: tt.digest}
			if err := VerifyAssetDigest(data, asset); err != nil {
				t.Errorf("VerifyAssetDigest: %v", err)
			}
		})
	}
}

// TestVerifyAssetDigestRefuses covers every shape the upgrade path has to
// refuse. A missing digest is the dangerous one: it means this response cannot
// vouch for the asset, and the alternative is an archive nobody authenticated.
func TestVerifyAssetDigestRefuses(t *testing.T) {
	data := []byte("pretend this is a release archive")
	other := []byte("a different archive entirely")
	otherSum := sha256.Sum256(other)
	notHex := strings.Repeat("z", 64)
	dataSum := sha256.Sum256(data)

	tests := []struct {
		name   string
		digest string
		want   string
	}{
		{
			name:   "a digest for different bytes",
			digest: "sha256:" + hex.EncodeToString(otherSum[:]),
			want:   "mismatch",
		},
		{
			name:   "no digest at all",
			digest: "",
			want:   "no digest",
		},
		{
			// An algorithm this binary does not implement is not a pass.
			name:   "an algorithm it cannot check",
			digest: "sha512:" + strings.Repeat("a", 128),
			want:   "unsupported",
		},
		{
			name:   "a truncated digest",
			digest: "sha256:abcd",
			want:   "malformed",
		},
		{
			name:   "a digest that is not hex",
			digest: "sha256:" + notHex,
			want:   "malformed",
		},
		{
			// GitHub always prefixes the algorithm. A bare hex digest is not
			// the field's shape, so guessing which algorithm was meant would
			// be worse than refusing.
			name:   "a bare hex digest with no algorithm",
			digest: hex.EncodeToString(dataSum[:]),
			want:   "unsupported",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			asset := &Asset{Name: "ghost_1.2.3_linux_amd64.tar.gz", Digest: tt.digest}
			err := VerifyAssetDigest(data, asset)
			if err == nil {
				t.Fatalf("VerifyAssetDigest accepted digest %q; an unauthenticated or unusable digest must refuse", tt.digest)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q should explain the refusal (%q)", err, tt.want)
			}
		})
	}
}

// TestVerifyAssetDigestNamesTheAssetAndBothDigests keeps the refusal
// actionable: the user has to be able to tell which asset failed and what the
// two values were, or the message is just a failed upgrade.
func TestVerifyAssetDigestNamesTheAssetAndBothDigests(t *testing.T) {
	asset := &Asset{Name: "ghost_1.2.3_linux_amd64.tar.gz", Digest: "sha256:" + strings.Repeat("0", 64)}
	err := VerifyAssetDigest([]byte("archive"), asset)
	if err == nil {
		t.Fatal("expected a refusal for a mismatched digest")
	}
	for _, want := range []string{asset.Name, asset.Digest} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should name %q", err, want)
		}
	}
}

// TestFetchReleaseReadsTheAssetDigest proves the digest is decoded from the
// release metadata at all. A struct field that is never populated makes every
// verification above refuse, and a rename of the JSON key does the same.
func TestFetchReleaseReadsTheAssetDigest(t *testing.T) {
	want := digestOf([]byte("asset"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tag_name":"v0.33.0","assets":[{"name":"ghost_0.33.0_linux_amd64.tar.gz","browser_download_url":"https://example.com/a","digest":"` + want + `"}]}`))
	}))
	defer srv.Close()

	rel, err := fetchRelease(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("fetchRelease: %v", err)
	}
	if len(rel.Assets) != 1 {
		t.Fatalf("got %d assets, want 1", len(rel.Assets))
	}
	if got := rel.Assets[0].Digest; got != want {
		t.Errorf("asset digest = %q, want %q — the releases API digest is not reaching the verifier", got, want)
	}
}
