package reflection

import "testing"

// TestLooksLikeSecretIsAScopeDecision pins the contract of the one function
// left in secrets.go, and it is a contract about the SCOPE of a memory, not
// about whether it holds a credential value.
//
// A global memory is replayed into every project on every save, so a
// project-local secret must not be widened into global context. That is a
// different question from "is this a credential value", which is
// internal/secret's and is enforced at the store layer — a memory saying
// "rotate the deployment password every quarter" is ordinary operational
// knowledge, and refusing to save it (or to keep it project-scoped) is the
// keyword-heuristic failure this whole arrangement exists to avoid.
//
// The function is also the reason the credential drop is not here: a
// credential-shaped memory that a tier proposes is dropped at the write
// boundary (cmd/ghost/promotion.go), after the drop guard's audit, because
// that ordering is what stops it being re-added or silently deleted. Nothing
// in this file writes, so nothing in this file can be the mitigation for
// leaving a write path unguarded.
func TestLooksLikeSecretIsAScopeDecision(t *testing.T) {
	cases := []struct {
		name  string
		text  string
		flags bool
	}{
		// Prose about credentials is ordinary knowledge, and blocking it from
		// global scope would refuse a memory store's most common content.
		{name: "prose about rotating a password", text: "Rotate the deployment password every quarter", flags: true},
		{name: "prose about where a token lives", text: "the access token is in the runner env", flags: true},
		{name: "prose about key material", text: "the private key is read from disk at boot", flags: true},
		{name: "a credential rotation schedule", text: "password rotation is automated by the vault", flags: true},

		// Assignment syntax, which is what actually looks like a value being
		// carried between projects. The separator normalization is the reason
		// "api-key:" and "api key" reach the same verdict.
		{name: "an assignment with an equals sign", text: "TOKEN=<the value from the vault>", flags: true},
		{name: "an assignment with a colon", text: "CLIENT_SECRET: from the secrets manager", flags: true},
		{name: "a hyphenated assignment key", text: "api-key is rotated weekly", flags: true},
		{name: "an underscore assignment key", text: "api_key is rotated weekly", flags: true},

		// A word that merely contains a credential word is not one, for the
		// patterns that carry a trailing delimiter. Without that, "tokenizer"
		// would block a global memory about the wrong thing entirely.
		{name: "a tokenizer is not a token", text: "the tokenizer keeps a 30k vocabulary", flags: false},
		// A KNOWN over-match, recorded rather than asserted away. The prefix
		// test is a plain Contains, so `credential_` matches inside a longer
		// hyphenated word and this flags as scope-bearing when it is not. It is
		// pre-existing and unchanged by this PR, and the consequence is bounded:
		// the memory stays project-scoped instead of global, which costs a
		// memory its cross-project reach rather than refusing the save or
		// storing a value. Fixing it properly means comparing on word
		// boundaries rather than substrings, which is a change to a function
		// this PR only documents.
		{name: "KNOWN OVER-MATCH: credential-less", text: "the credential-less helper has no config", flags: true},
		{name: "unrelated operational prose", text: "the relay listens on 2222", flags: false},
		{name: "an ordinary global preference", text: "tabs, not spaces, in every repository we touch", flags: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := looksLikeSecret(lowerAll(tc.text))
			if got != tc.flags {
				t.Errorf("looksLikeSecret(%q) = %v, want %v — this is a scope decision about replaying into every project, not a detection of a credential value",
					tc.text, got, tc.flags)
			}
		})
	}
}

// TestLooksLikeSecretSeesNoValue pins that the function never inspects a
// value's shape: two texts differing only in the value must reach the same
// verdict, because the question is what the memory is ABOUT, not what it
// carries. internal/secret is what answers the second question.
func TestLooksLikeSecretSeesNoValue(t *testing.T) {
	const head = "the deploy token is "
	shapes := []string{
		"a random string",
		"in the runner env",
		"rotated quarterly",
	}
	for _, shape := range shapes {
		if looksLikeSecret(lowerAll(head+shape)) != true {
			t.Errorf("looksLikeSecret(%q) = false, want true — the key names a credential either way", head+shape)
		}
	}
}

func lowerAll(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}
