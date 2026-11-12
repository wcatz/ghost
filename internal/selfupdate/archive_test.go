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
	// typeflag is the tar entry type; the zero value is tar.TypeReg, which is
	// what every real archive carries.
	typeflag byte
}

// tarGzWith builds an in-memory .tar.gz, the archive shape every non-Windows
// ghost release ships.
func tarGzWith(t *testing.T, entries ...archiveEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: e.name, Typeflag: e.typeflag, Mode: 0o755, Size: int64(len(e.body))}); err != nil {
			t.Fatalf("tar header %s: %v", e.name, err)
		}
		if len(e.body) > 0 {
			if _, err := tw.Write(e.body); err != nil {
				t.Fatalf("tar body %s: %v", e.name, err)
			}
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

// stampedZipEntry is a zip entry whose header carries a chosen creator system
// and attribute bits. Those two decide how a zip reader reports the entry, so
// a test has to be able to choose them.
type stampedZipEntry struct {
	name           string
	creatorVersion uint16
	externalAttrs  uint32
	body           []byte
}

// zipWithStamped builds an in-memory .zip whose entries carry the creator
// system and the attribute bits the caller chose.
func zipWithStamped(t *testing.T, entries ...stampedZipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		w, err := zw.CreateHeader(&zip.FileHeader{
			Name:           e.name,
			Method:         zip.Store,
			CreatorVersion: e.creatorVersion,
			ExternalAttrs:  e.externalAttrs,
		})
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

func TestExtractBinaryRefusesAnEntryThatIsNotTheBinary(t *testing.T) {
	tests := []struct {
		name    string
		archive []byte
		want    string
	}{
		{
			name:    "a directory named ghost",
			archive: tarGzWith(t, archiveEntry{name: "ghost", typeflag: tar.TypeDir}),
			want:    "directory",
		},
		{
			name:    "a symlink named ghost",
			archive: tarGzWith(t, archiveEntry{name: "ghost", typeflag: tar.TypeSymlink}),
			want:    "symlink",
		},
		{
			name:    "an empty file named ghost",
			archive: tarGzWith(t, archiveEntry{name: "ghost"}),
			want:    "empty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ExtractBinary(tt.archive)
			if err == nil {
				t.Fatalf("ExtractBinary returned %d byte(s) with no error for %s", len(got), tt.name)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q should explain the refusal (%q)", err, tt.want)
			}
		})
	}
}

func TestExtractBinaryRefusesALinkEntryInAZip(t *testing.T) {
	tests := []struct {
		name           string
		creatorVersion uint16
	}{
		{name: "an archive stamped FAT", creatorVersion: 0<<8 | 20},
		{name: "an archive stamped NTFS", creatorVersion: 11<<8 | 20},
		{name: "an archive stamped Unix", creatorVersion: 3<<8 | 20},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			archive := zipWithStamped(t, stampedZipEntry{
				name:           "ghost.exe",
				creatorVersion: tt.creatorVersion,
				externalAttrs:  0o120777 << 16,
				body:           []byte("/etc/passwd\n"),
			})

			got, err := ExtractBinary(archive)
			if err == nil {
				t.Fatalf("ExtractBinary returned %q with no error; a link entry is not the binary", got)
			}
			if !strings.Contains(err.Error(), "link") {
				t.Errorf("error %q should say the entry is a link", err)
			}
			if !strings.Contains(err.Error(), "not a regular file") {
				t.Errorf("error %q should say the entry is not a regular file", err)
			}
		})
	}
}

func TestExtractBinaryAcceptsAZipEntryThatRecordsNoType(t *testing.T) {
	binary := []byte("MZ pretend executable")

	tests := []struct {
		name           string
		creatorVersion uint16
		externalAttrs  uint32
	}{
		{name: "an archive stamped FAT with no attribute bits", creatorVersion: 0<<8 | 20},
		{name: "an archive stamped NTFS with no attribute bits", creatorVersion: 11<<8 | 20},
		{name: "an archive stamped Unix with a regular file's mode", creatorVersion: 3<<8 | 20, externalAttrs: 0o100644 << 16},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			archive := zipWithStamped(t, stampedZipEntry{
				name:           "ghost.exe",
				creatorVersion: tt.creatorVersion,
				externalAttrs:  tt.externalAttrs,
				body:           binary,
			})

			got, err := ExtractBinary(archive)
			if err != nil {
				t.Fatalf("ExtractBinary: %v", err)
			}
			if !bytes.Equal(got, binary) {
				t.Errorf("extracted %q, want %q", got, binary)
			}
		})
	}
}
