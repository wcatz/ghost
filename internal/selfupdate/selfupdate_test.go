package selfupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
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

	_, err = FindChecksumAsset(&Release{TagName: "v1.2.3"})
	if err == nil {
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

	t.Run("goreleaser binary-mode asterisk prefix is tolerated", func(t *testing.T) {
		manifest := hexSum + "  *" + assetName + "\n"
		if err := VerifyChecksum(data, manifest, assetName); err != nil {
			t.Errorf("VerifyChecksum with '*' prefix: %v", err)
		}
	})
}
