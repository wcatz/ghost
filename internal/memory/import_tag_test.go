package memory

import (
	"context"
	"strings"
	"testing"
)

// TestImportMemoryCarriesATagByteForByteWhateverItHolds is the import half of the
// #811 correction, and it is the assertion that keeps a backup a backup.
//
// `ImportMemory` used to refuse a tag holding a control character, a backtick or a
// data delimiter, and `internal/portable`'s exporter applied the same check so
// export→import would stay a round trip. That is a coherent rule with an incoherent
// consequence: the memory is dropped from the ARTIFACT, so the user's backup loses
// the row, on the strength of a label. Nothing on the MCP write path refused those
// tags for the whole life of the feature, so any store written through it could
// hold one, and the operator discovers the gap on the day they need the file.
//
// So an import carries the value and says nothing about its shape. This test is the
// opposite of the one it replaces: every hostile shape is ACCEPTED, and stored
// verbatim, because a restore that rewrites a label is a restore that does not
// restore. The render-time neutralisation in `assemble.TagsLabel` is what makes the
// stored value safe on the line an agent reads, and the class is still refused where
// it is reachable — on the four MCP writers, in `validateTags`.
func TestImportMemoryCarriesATagByteForByteWhateverItHolds(t *testing.T) {
	carried := map[string][]string{
		"an opening guillemet":                  {"a«b"},
		"a closing guillemet":                   {"a»b"},
		"a whole data block":                    {"«urgent»"},
		"a newline":                             {"a\nb"},
		"a carriage return":                     {"a\rb"},
		"a nul":                                 {"a\x00b"},
		"a tab":                                 {"a\tb"},
		"a backtick":                            {"a`b"},
		"an ordinary tag beside a bad one":      {"golden", "x«y"},
		"a space, which was never in the class": {"ci timeouts"},
		"an over-long tag, which is trimmed not refused": {strings.Repeat("t", 200)},
	}
	for name, tags := range carried {
		t.Run(name, func(t *testing.T) {
			s := portableTestStore(t)
			ctx := context.Background()
			if err := s.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
				t.Fatalf("EnsureProject: %v", err)
			}
			if _, _, _, err := s.ImportMemory(ctx, PortableMemory{
				ID: "M1", ProjectID: "p1", Category: "fact", Content: "a claim an artifact carried",
				Source: "mcp", Tags: tags,
			}, ImportOptions{Apply: true, TrustProvenance: true}); err != nil {
				t.Fatalf("ImportMemory refused the tags %q, so an artifact a store already holds would not "+
					"restore: %v", tags, err)
			}
			got, err := s.GetByIDs(ctx, []string{"M1"})
			if err != nil || len(got) != 1 {
				t.Fatalf("GetByIDs: err=%v rows=%d", err, len(got))
			}
			if len(got[0].Tags) != len(tags) {
				t.Fatalf("the row came back with tags %q, want %q", got[0].Tags, tags)
			}
			// Byte for byte, element by element: an importer that trimmed a tag
			// would keep the COUNT and change the value, which is a silent rewrite
			// of the user's row. Nothing bounds a tag on this path, so the
			// over-long one arrives whole.
			for i := range tags {
				if got[0].Tags[i] != tags[i] {
					t.Errorf("tag %d came back as %q (%d bytes), want %q (%d bytes) byte for byte",
						i, got[0].Tags[i], len(got[0].Tags[i]), tags[i], len(tags[i]))
				}
			}
		})
	}
}

// TestImportMemoryStillRefusesASecretInATag is the guard that is NOT about shape,
// and it is here so the removal above cannot be read as "tags are unvalidated".
//
// A credential in a tag is a different question from a delimiter in one, and it is
// the one that matters operationally: a tag is carried into every listing, every
// search row and the next reflect prompt. The refusal names the field and never
// echoes the value, and it runs in the same window as the other field checks, so a
// dry run classifies the record exactly as the apply run it previews.
func TestImportMemoryStillRefusesASecretInATag(t *testing.T) {
	s := portableTestStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	const token = "ghp_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789"
	rec := PortableMemory{
		ID: "M1", ProjectID: "p1", Category: "fact", Content: "a claim an artifact carried",
		Source: "mcp", Tags: []string{"golden", "deploy-token-" + token},
	}
	for _, apply := range []bool{false, true} {
		_, _, _, err := s.ImportMemory(ctx, rec, ImportOptions{Apply: apply, TrustProvenance: true})
		if err == nil {
			t.Errorf("apply=%v: a credential in a tag was accepted", apply)
			continue
		}
		if !strings.Contains(err.Error(), "tags") {
			t.Errorf("apply=%v: the refusal does not name the field: %v", apply, err)
		}
		if strings.Contains(err.Error(), token) {
			t.Errorf("apply=%v: the refusal echoed the credential: %v", apply, err)
		}
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memories WHERE project_id = 'p1'`).Scan(&n); err != nil {
		t.Fatalf("count memories: %v", err)
	}
	if n != 0 {
		t.Errorf("a record with a credential in a tag was written anyway: %d rows", n)
	}
}
