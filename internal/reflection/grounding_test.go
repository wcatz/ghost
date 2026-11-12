package reflection

import (
	"strings"
	"testing"
)

// TestIdentifiers covers the shapes a merge or rewrite is allowed to invent
// nothing of. The classes are the ones #639 measured being corrupted while the
// model was only copying: a hashed filename, a version, a path, a host, a
// number. A word is deliberately not a class here — see
// TestUnknownIdentifiersLeavesOrdinaryProseAlone.
func TestIdentifiers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		text  string
		want  string
		avoid string
	}{
		{name: "short commit hash", text: "graph expansion removed in 0a1f004", want: "0a1f004"},
		{name: "hash inside a longer token", text: "shipped on 0a1f004-remove-sampling", want: "0a1f004"},
		{name: "memory id", text: "superseded by 60ECB9CC", want: "60ecb9cc"},
		{name: "hashed filename", text: "the mesh bundle 2.BeXIAhbj.js is stale", want: "2.bexiahbj.js"},
		{name: "absolute path", text: "the key lives under /keys/relay3", want: "/keys/relay3"},
		{name: "repo path", text: "the parser lives in internal/reflection/prompt.go", want: "internal/reflection/prompt.go"},
		{name: "dotted version", text: "gouroboros 0.204.3 has the bug", want: "0.204.3"},
		{name: "v-prefixed version", text: "v1.2.3 is pinned", want: "1.2.3"},
		{name: "hostname", text: "the leader on node-3.example.com answers", want: "node-3.example.com"},
		{name: "region hostname", text: "production is fsn1.example.net", want: "fsn1.example.net"},
		{name: "port number", text: "the bastion uses 2222, not 22", want: "2222"},
		{name: "major version number", text: "moved to Node 18", want: "18"},
		// A dotted word that is not a host and not a file: the prose uses
		// "and/or", "i.e." and "e.g." constantly, and flagging those would
		// reject honest merges.
		{name: "and-or is not a path", text: "accepts json and/or yaml", avoid: "and/or"},
		{name: "abbreviation is not a host", text: "i.e. the same file, e.g. the lock", avoid: "i.e,e.g"},
		// A trailing full stop is sentence punctuation, not part of the
		// identifier: a source that ends its sentence with a path grounds an
		// output that ends one too, and a merge is not rejected over it.
		{name: "trailing full stop is not part of the path", text: "the key lives under /keys/relay3.", want: "/keys/relay3", avoid: "/keys/relay3."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := identifiers(tc.text)
			if tc.want != "" && !got[strings.ToLower(tc.want)] {
				t.Errorf("identifiers(%q) = %v, want it to contain %q", tc.text, got, tc.want)
			}
			for _, unwanted := range strings.Split(tc.avoid, ",") {
				if unwanted == "" {
					continue
				}
				if got[strings.ToLower(unwanted)] {
					t.Errorf("identifiers(%q) flagged %q: %v", tc.text, unwanted, got)
				}
			}
		})
	}
}

// TestGroundingRejectsCorruptedIdentifier is the recorded corruption from
// #639's benchmark: the model was copying, and turned 2.BeXIAhbj.js into
// 2.BeXIAhbq.js. The output is rejected whole, not repaired — Ghost cannot know
// which of the two is right, and the originals are still there.
func TestGroundingRejectsCorruptedIdentifier(t *testing.T) {
	sources := []string{"the mesh bundle 2.BeXIAhbj.js is served first"}
	unknown := unknownIdentifiers("the mesh bundle 2.BeXIAhbq.js is served first", sources)
	if len(unknown) != 1 {
		t.Fatalf("unknowns = %v, want the corrupted filename", unknown)
	}
}
