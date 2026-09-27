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

// TestUnknownIdentifiersComparesAgainstEverySource: a merge draws on every id
// it names, so an identifier in ANY of them grounds it. Gating on the first id
// alone would reject every merge that carried a specific across from a sibling.
func TestUnknownIdentifiersComparesAgainstEverySource(t *testing.T) {
	sources := []string{"the bastion is on port 22", "production is fsn1.example.net"}
	if unknown := unknownIdentifiers("the bastion on 22 fronts fsn1.example.net", sources); len(unknown) != 0 {
		t.Errorf("grounded text reported unknowns: %v", unknown)
	}
	if unknown := unknownIdentifiers("the bastion on 22 fronts fsn1.example.org", sources); len(unknown) != 1 ||
		unknown[0] != "fsn1.example.org" {
		t.Errorf("unknowns = %v, want [fsn1.example.org]", unknown)
	}
}

// TestUnknownIdentifiersIsCaseInsensitive: a host or path restated with
// different capitalisation is the same identifier, not a new one.
func TestUnknownIdentifiersIsCaseInsensitive(t *testing.T) {
	sources := []string{"Production runs in region FSN1 behind Cloudflare"}
	if unknown := unknownIdentifiers("production runs in region fsn1 behind cloudflare", sources); len(unknown) != 0 {
		t.Errorf("case-only difference reported as unknown: %v", unknown)
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

// TestGroundingAcceptsAReorderedFaithfulMerge: grounding is not a diff. A merge
// that says the same identifiers in a new order is the entire point of merging.
func TestGroundingAcceptsAReorderedFaithfulMerge(t *testing.T) {
	sources := []string{
		"SSH to the Hetzner bastion goes through port 2222, not 22",
		"Production runs in region fsn1 behind Cloudflare",
	}
	text := "Production in region fsn1 is reached by SSH on port 2222, not 22, via the Hetzner bastion"
	if unknown := unknownIdentifiers(text, sources); len(unknown) != 0 {
		t.Errorf("a faithful reordering was rejected: %v", unknown)
	}
}
