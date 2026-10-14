package memory

import (
	"context"
	"strings"
	"testing"
)

// The sidecar arrives with the copy being judged, so it is untrusted input, and
// its sha256 is the one manifest field verify ever printed as text (#1187). A
// value carrying ESC [1F moved the cursor back onto the headline and wrote
// "verified" over "refusing"; a newline forged a whole line. A sha256 is a
// digest or it is damage: anything other than 64 lowercase hex digits is a
// manifest that cannot be read.

// craftedSHA256 is the issue's reproduction: the 12 bytes shortHash keeps are
// ESC [1F followed by "verified", padded to the length of a real digest.
const craftedSHA256 = "\x1b[1Fverified" + "0000000000000000000000000000000000000000000000000000"

// controlByte returns the first byte of s that a terminal would act on rather
// than print, and whether there was one.
func controlByte(s string) (byte, bool) {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c == 0x7f {
			return c, true
		}
	}
	return 0, false
}

func TestVerifyRefusesAManifestSHA256ThatIsNotADigest(t *testing.T) {
	for _, tc := range []struct {
		name, sha string
	}{
		{"cursor escape", craftedSHA256},
		{"forged line", "\n\x1b[2Kverified" + strings.Repeat("0", 52)},
		{"uppercase", strings.Repeat("AB", 32)},
		{"short", strings.Repeat("a", 63)},
		{"long", strings.Repeat("a", 65)},
		{"empty", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dest := backedUpFixture(t)
			path := ManifestPath(dest)
			m := readManifestAt(t, dest)
			m.SHA256 = tc.sha
			writeManifestFile(t, path, m)

			rep, err := VerifyBackup(context.Background(), dest)
			if err == nil {
				t.Fatalf("verify accepted a manifest whose sha256 is %q: %+v", tc.sha, rep)
			}
			if rep.HasManifest {
				t.Error("HasManifest is true for a manifest whose sha256 is not a digest")
			}
			if !rep.ManifestPresent || rep.ManifestErr == nil {
				t.Errorf("ManifestPresent=%v ManifestErr=%v, want the present-but-unreadable report", rep.ManifestPresent, rep.ManifestErr)
			}
			msg := err.Error()
			if !strings.Contains(msg, "sha256") || !strings.Contains(msg, path) {
				t.Errorf("error = %q, want it to name the sha256 field and the manifest", msg)
			}
			if c, ok := controlByte(msg); ok {
				t.Errorf("error carries control byte %#x: %q", c, msg)
			}
			for _, c := range rep.Checks {
				if b, ok := controlByte(c.Detail); ok {
					t.Errorf("check %s detail carries control byte %#x: %q", c.Name, b, c.Detail)
				}
			}
		})
	}
}

// TestCheckHashRendersOnlyAHexDigest: checkHash takes any BackupManifest, so it
// does not rely on its caller having read the manifest through
// ReadBackupManifest. A value that is not a digest is a failure, and the detail
// carries it quoted rather than raw.
func TestCheckHashRendersOnlyAHexDigest(t *testing.T) {
	dest := backedUpFixture(t)
	size := fileSize(t, dest)
	m := BackupManifest{Bytes: size, SHA256: craftedSHA256}

	c := checkHash(dest, m, true, size)
	if c.State != VerifyFailed {
		t.Fatalf("sha256 check = %q (%s), want failed", c.State, c.Detail)
	}
	if b, ok := controlByte(c.Detail); ok {
		t.Fatalf("detail carries control byte %#x: %q", b, c.Detail)
	}
	if !strings.Contains(c.Detail, `\x1b`) {
		t.Errorf("detail = %q, want the value shown escaped", c.Detail)
	}
}

// TestVerifyStillAcceptsAGenuineDigest: the shape check refuses nothing a
// backup writes.
func TestVerifyStillAcceptsAGenuineDigest(t *testing.T) {
	dest := backedUpFixture(t)
	rep, err := VerifyBackup(context.Background(), dest)
	if err != nil || len(rep.Problems) > 0 {
		t.Fatalf("verify refused the backup it just wrote: err=%v problems=%v", err, rep.Problems)
	}
	if hash := checkNamed(t, rep, "sha256"); hash.State != VerifyOK {
		t.Fatalf("sha256 check = %q (%s), want ok", hash.State, hash.Detail)
	}
}
