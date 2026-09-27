package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"strings"
	"testing"
)

type archiveEntry struct {
	name string
	body []byte
}

// tarGzWith builds an in-memory .tar.gz, the archive shape every non-Windows
// ghost release ships.
func tarGzWith(t *testing.T, entries ...archiveEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: e.name, Mode: 0o755, Size: int64(len(e.body))}); err != nil {
			t.Fatalf("tar header %s: %v", e.name, err)
		}
		if _, err := tw.Write(e.body); err != nil {
			t.Fatalf("tar body %s: %v", e.name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// zipWith builds an in-memory .zip, the archive shape every Windows ghost
// release ships (goreleaser's format_overrides).
func zipWith(t *testing.T, entries ...archiveEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		w, err := zw.Create(e.name)
		if err != nil {
			t.Fatalf("zip entry %s: %v", e.name, err)
		}
		if _, err := w.Write(e.body); err != nil {
			t.Fatalf("zip body %s: %v", e.name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

// TestExtractBinaryReadsBothReleaseArchiveShapes is the Windows half of
// #558: ghost_0.33.0_windows_amd64.zip is a zip, and handing it to a gzip
// reader failed every Windows upgrade at "gzip: invalid header" — the command
// that exists to repair a machine could not run on one of the platforms ghost
// ships.
func TestExtractBinaryReadsBothReleaseArchiveShapes(t *testing.T) {
	binary := []byte("MZ pretend executable")

	tests := []struct {
		name    string
		archive []byte
	}{
		{
			name:    "the tar.gz every unix release ships",
			archive: tarGzWith(t, archiveEntry{name: "LICENSE", body: []byte("MIT")}, archiveEntry{name: "ghost", body: binary}),
		},
		{
			name:    "the zip every windows release ships",
			archive: zipWith(t, archiveEntry{name: "README.md", body: []byte("# ghost")}, archiveEntry{name: "ghost.exe", body: binary}),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ExtractBinary(tt.archive)
			if err != nil {
				t.Fatalf("ExtractBinary: %v", err)
			}
			if !bytes.Equal(got, binary) {
				t.Errorf("extracted %q, want %q", got, binary)
			}
		})
	}
}

// TestExtractBinaryRefusesAnArchiveWithoutTheBinary keeps a wrong-but-valid
// archive from producing an install. Every ghost archive also carries LICENSE
// and README.md, so the binary has to be found, not assumed.
func TestExtractBinaryRefusesAnArchiveWithoutTheBinary(t *testing.T) {
	tests := []struct {
		name    string
		archive []byte
	}{
		{name: "tar.gz with only documentation", archive: tarGzWith(t, archiveEntry{name: "LICENSE", body: []byte("MIT")})},
		{name: "zip with only documentation", archive: zipWith(t, archiveEntry{name: "LICENSE", body: []byte("MIT")})},
		{
			// A nested entry is a different file: only the archive root is
			// where goreleaser puts the binary, and an entry reached by
			// traversal is not one ghost published.
			name:    "zip with the binary nested in a directory",
			archive: zipWith(t, archiveEntry{name: "ghost_1.2.3_windows_amd64/ghost.exe", body: []byte("MZ")}),
		},
		{name: "not an archive at all", archive: []byte("plain text, not an archive")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, err := ExtractBinary(tt.archive); err == nil {
				t.Fatalf("ExtractBinary returned %q with no error; an archive without the ghost binary must refuse", got)
			}
		})
	}
}

// TestExtractBinaryRefusesAnEntryOverItsCap closes the size gap the transfer
// cap left open. A 200 MiB download cap bounds what arrives; nothing bounded
// what came out of the decompressor, so a small hostile archive could expand
// until the process died.
func TestExtractBinaryRefusesAnEntryOverItsCap(t *testing.T) {
	original := binaryCap
	binaryCap = 1024
	t.Cleanup(func() { binaryCap = original })

	oversized := bytes.Repeat([]byte("a"), int(binaryCap)+1)

	tests := []struct {
		name    string
		archive []byte
	}{
		{name: "tar.gz entry", archive: tarGzWith(t, archiveEntry{name: "ghost", body: oversized})},
		{name: "zip entry", archive: zipWith(t, archiveEntry{name: "ghost.exe", body: oversized})},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ExtractBinary(tt.archive)
			if err == nil {
				t.Fatal("expected an error when the binary inside the archive exceeds its cap")
			}
			if !strings.Contains(err.Error(), "cap") {
				t.Errorf("error should name the cap, got: %v", err)
			}
		})
	}
}

// TestExtractBinaryCapsTheEntriesItSkips is the half of the cap that a limit
// on the returned bytes does not cover. archive/tar reads an entry in full to
// reach the next header — a gzip stream cannot seek — so every entry ahead of
// the binary is inflated before the loop gets there, and those are real entries
// (LICENSE, README.md) rather than something only an attacker would add.
func TestExtractBinaryCapsTheEntriesItSkips(t *testing.T) {
	original := binaryCap
	binaryCap = 4096
	t.Cleanup(func() { binaryCap = original })

	archive := tarGzWith(t,
		// A small, entirely plausible documentation entry that inflates well
		// past the cap, ahead of a binary small enough to be the only thing
		// read.
		archiveEntry{name: "README.md", body: bytes.Repeat([]byte("a"), 64<<10)},
		archiveEntry{name: "ghost", body: []byte("binary")},
	)

	_, err := ExtractBinary(archive)
	if err == nil {
		t.Fatal("expected an error: the archive inflates past the cap in an entry the extraction never returns")
	}
	if !strings.Contains(err.Error(), "cap") {
		t.Errorf("error should name the cap, got: %v", err)
	}
}

// TestExtractBinaryAcceptsAnEntryExactlyAtItsCap is the other half of the
// boundary: "over the cap" must not be implemented as "big". It is expressed on
// the zip path, where the single entry it opens is the whole of what the
// archive inflates; a .tar.gz spends part of the cap on headers and block
// padding, which is why its limit is on everything the archive inflates rather
// than on one entry.
func TestExtractBinaryAcceptsAnEntryExactlyAtItsCap(t *testing.T) {
	original := binaryCap
	binaryCap = 1024
	t.Cleanup(func() { binaryCap = original })

	atCap := bytes.Repeat([]byte("a"), int(binaryCap))
	got, err := ExtractBinary(zipWith(t, archiveEntry{name: "ghost.exe", body: atCap}))
	if err != nil {
		t.Fatalf("ExtractBinary at the cap: %v", err)
	}
	if len(got) != int(binaryCap) {
		t.Errorf("extracted %d bytes, want %d", len(got), binaryCap)
	}
}
