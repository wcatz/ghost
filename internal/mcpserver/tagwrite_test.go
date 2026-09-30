package mcpserver

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wcatz/ghost/internal/assemble"
)

// TestEveryTagBearingWriteToolRefusesATagThatCouldForgeALine is the write
// boundary for #811, and it is the boundary that is actually reachable.
//
// The first attempt at closing #811 put the refusal on `ghost import`, which is
// the wrong side of the store: `validateTags` checked only count and length, so
// `ghost_memory_save` with tags ["«urgent»"] was accepted, the tag rendered
// harmlessly, and the row then VANISHED from every future `ghost export` because
// the exporter applied the importer's own tag check. A backup that loses a user's
// memory because of a tag is data loss, and the reason it was reachable is that
// the refusal sat on a path an ordinary save never takes.
//
// So the refusal belongs here, on the four tools that write a tag list, and the
// import and export paths carry the value byte for byte. This test drives all four
// over the MCP transport, because a refusal that exists on three of the four
// writers is exactly what the secret guard's own comment warns about: "a guard's
// reach is a set, and a claim about it has to enumerate the set rather than assert
// it".
func TestEveryTagBearingWriteToolRefusesATagThatCouldForgeALine(t *testing.T) {
	// The class, as one table, because the four tools must agree on it: a control
	// character ends the line the tags label is on, a backtick pairs into a code
	// span that swallows the rest of the row, and a data delimiter opens a block
	// of its own mid-metadata. A SPACE is not in it, and neither is a length: a
	// tag is a keyword a reader scans, and "ci timeouts" is a real one.
	refused := map[string][]string{
		"an opening guillemet":         {"a«b"},
		"a closing guillemet":          {"a»b"},
		"a whole data block":           {"«urgent»"},
		"a newline":                    {"a\nb"},
		"a carriage return":            {"a\rb"},
		"a nul":                        {"a\x00b"},
		"a tab":                        {"a\tb"},
		"a backtick":                   {"a`b"},
		"a hostile tag beside ok ones": {"golden", "x«y"},
	}
	accepted := map[string][]string{
		"minted label":        {"golden"},
		"a space":             {"ci timeouts"},
		"a slash and a dot":   {".sandbox/tmp"},
		"non-ascii":           {"日本語"},
		"a quote":             {`a"b`},
		"at the length bound": {strings.Repeat("t", 64)},
	}

	// One call per tool, so the four are enumerated rather than asserted. Each
	// builds its own arguments because the four differ in what they require, and a
	// shared builder would have hidden that.
	//
	// The second return is how many rows the call itself wrote before reaching the
	// tags: a refusal has to leave the store as it found it, and
	// `ghost_memory_update` necessarily writes a row to have one to update.
	writers := []struct {
		name string
		call func(t *testing.T, srv *Server, session *mcp.ClientSession, tags []string) (*mcp.CallToolResult, int)
	}{
		{"ghost_memory_save", func(t *testing.T, _ *Server, s *mcp.ClientSession, tags []string) (*mcp.CallToolResult, int) {
			return callTool(t, s, "ghost_memory_save", map[string]any{
				"project_id": "vproj", "content": "a claim with a tag", "category": "fact", "tags": tags,
			}), 0
		}},
		{"ghost_save_global", func(t *testing.T, _ *Server, s *mcp.ClientSession, tags []string) (*mcp.CallToolResult, int) {
			return callTool(t, s, "ghost_save_global", map[string]any{
				"content": "a cross-project preference with a tag", "category": "preference", "tags": tags,
			}), 0
		}},
		{"ghost_memory_update", func(t *testing.T, _ *Server, s *mcp.ClientSession, tags []string) (*mcp.CallToolResult, int) {
			if res := callTool(t, s, "ghost_memory_save", map[string]any{
				"project_id": "vproj", "content": "a claim to be re-tagged", "category": "fact",
			}); res.IsError {
				t.Fatalf("seed save: %s", resultText(res))
			}
			// `ghost_memory_update` needs the id, and the seed's own answer is where
			// it is. Passing the CONTENT instead — which is what the first version of
			// this test did — is a schema rejection that reads exactly like a
			// refusal the tool did not make, and the whole table failed for that
			// reason.
			id := savedID(t, resultText(callTool(t, s, "ghost_memory_search", map[string]any{
				"query": "a claim to be re-tagged", "project_id": "vproj",
			})))
			return callTool(t, s, "ghost_memory_update", map[string]any{
				"project_id": "vproj", "memory_id": id, "tags": tags,
			}), 1
		}},
		{"ghost_decision_record", func(t *testing.T, _ *Server, s *mcp.ClientSession, tags []string) (*mcp.CallToolResult, int) {
			return callTool(t, s, "ghost_decision_record", map[string]any{
				"project_id": "vproj", "title": "a decision with a tag",
				"decision": "we chose the rune-safe cut", "rationale": "a byte cut stores invalid UTF-8",
				"tags": tags,
			}), 0
		}},
	}

	for _, w := range writers {
		t.Run(w.name, func(t *testing.T) {
			for name, tags := range refused {
				t.Run("refuses/"+name, func(t *testing.T) {
					srv, session := newValiditySession(t)
					res, seeded := w.call(t, srv, session, tags)
					out := resultText(res)
					// The refusal has to name the FIELD, or an agent cannot tell
					// which argument to change.
					if !strings.Contains(out, "tag") {
						t.Errorf("%s accepted the tags %q (answer: %s) — the refusal has to name the field",
							w.name, tags, out)
					}
					// And it has to leave the store as it found it. This is the half
					// that matters: a refusal that still wrote the row would leave the
					// exporter to discover it, which is the shape this whole change
					// removes. Asserted as "unchanged" rather than "zero", because an
					// update necessarily has a row to decline to change.
					if got := countProjectMemories(t, srv, "vproj"); got != seeded {
						t.Errorf("%s refused the tags %q but the project holds %d memory row(s), want the %d it "+
							"held before the call", w.name, tags, got, seeded)
					}
					// And the seeded row must not have been re-tagged, since that is
					// the write an update refusal is actually refusing.
					if seeded > 0 && len(storedTags(t, srv, "vproj")) != 0 {
						t.Errorf("%s refused the tags %q but the row it declined to update now carries %v",
							w.name, tags, storedTags(t, srv, "vproj"))
					}
				})
			}
			for name, tags := range accepted {
				t.Run("accepts/"+name, func(t *testing.T) {
					srv, session := newValiditySession(t)
					res, _ := w.call(t, srv, session, tags)
					if out := resultText(res); strings.Contains(out, "must hold no") {
						t.Errorf("%s refused the tags %q, which a real store holds (answer: %s)", w.name, tags, out)
					}
				})
			}
		})
	}
}

// TestATagRefusalNamesTheTagThroughTheSafeRenderer is the half of the refusal
// message that is easy to get wrong and worse than having no message: the tag is
// caller-supplied text that reaches the answer, and an error that interpolates it
// raw is a second injection on the very surface the first one closes.
//
// So the message carries the tag through `assemble.Token` — bare for a tag a
// stored name plausibly uses, and otherwise the ASCII-only quoted string — and it
// contains NONE of the three characters it refuses, not even in its own prose. The
// first version spelled them out ("outside the «...» delimiters"), which put a data
// delimiter into the very answer the refusal exists to keep clean: a reader, and a
// test, could no longer tell a message MENTIONING a delimiter from one CARRYING
// the caller's. That is the property worth having, so the prose says "delimiter".
func TestATagRefusalNamesTheTagThroughTheSafeRenderer(t *testing.T) {
	_, session := newValiditySession(t)
	out := resultText(callTool(t, session, "ghost_memory_save", map[string]any{
		"project_id": "vproj", "content": "a claim with a tag", "category": "fact",
		"tags": []string{"«urgent»"},
	}))
	for _, forbidden := range []string{"«", "»", "`"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("the refusal carries %q into the answer:\n%s", forbidden, out)
		}
	}
	// Identifiable, and in the SAME form the row would print — which is what
	// "through the safe renderer" means, and what makes the message and the line
	// about the same tag agree.
	if !strings.Contains(out, assemble.Token("«urgent»")) {
		t.Errorf("the refusal does not name the tag in the form the renderer would print it (want %q):\n%s",
			assemble.Token("«urgent»"), out)
	}
	// The POSITION, so a ten-tag list names the one to change without the caller
	// having to diff two lists.
	if !strings.Contains(out, "tag 0") {
		t.Errorf("the refusal does not say which tag is at fault:\n%s", out)
	}
	// And the CLASS, or the caller cannot tell a backtick from a delimiter and so
	// cannot tell which character to change.
	if !strings.Contains(out, "data delimiter") {
		t.Errorf("the refusal does not name the class it refused:\n%s", out)
	}
}

// TestATagLongerThanTheBoundIsCutOnARuneBoundary is here rather than in the tag
// label's package because the CUT is what this writer does, and it was a byte
// slice: `tags[i] = t[:64]` on a CJK tag returned half a rune, and that invalid
// UTF-8 was STORED — not merely printed. It is the same defect #810 was about, one
// function away, inside the function this change rewrites.
//
// The column is unconstrained text, so the store accepts it and the only place it
// can be caught is here; and a test can only see it through the bytes that come
// back out, so it reads the row rather than the answer.
func TestATagLongerThanTheBoundIsCutOnARuneBoundary(t *testing.T) {
	// Twenty-one CJK runes are 63 bytes, so a byte cut at 64 lands INSIDE the
	// twenty-second. The bound is what cuts this tag and nothing else.
	tag := strings.Repeat("日", 22)
	srv, session := newValiditySession(t)
	if res := callTool(t, session, "ghost_memory_save", map[string]any{
		"project_id": "vproj", "content": "a claim with a long tag", "category": "fact",
		"tags": []string{tag},
	}); res.IsError {
		t.Fatalf("save: %s", resultText(res))
	}
	got := firstStoredTag(t, srv, "vproj")
	if !utf8.ValidString(got) {
		t.Errorf("the stored tag is not valid UTF-8: %q", got)
	}
	if got != strings.Repeat("日", 21) {
		t.Errorf("stored tag = %q (%d runes), want the first 21 cut on a rune boundary", got, len([]rune(got)))
	}
	// And the bound still holds, which is the other half: a rune-safe cut that
	// ignored the cap would be a different bug.
	if len(got) > 64 {
		t.Errorf("the stored tag is %d bytes, over the 64-byte bound", len(got))
	}
}

func countProjectMemories(t *testing.T, srv *Server, projectID string) int {
	t.Helper()
	rows, err := srv.store.GetTopMemories(context.Background(), projectID, 100)
	if err != nil {
		t.Fatalf("GetTopMemories(%q): %v", projectID, err)
	}
	return len(rows)
}

func storedTags(t *testing.T, srv *Server, projectID string) []string {
	t.Helper()
	rows, err := srv.store.GetTopMemories(context.Background(), projectID, 100)
	if err != nil {
		t.Fatalf("GetTopMemories(%q): %v", projectID, err)
	}
	var out []string
	for _, r := range rows {
		out = append(out, r.Tags...)
	}
	return out
}

func firstStoredTag(t *testing.T, srv *Server, projectID string) string {
	t.Helper()
	tags := storedTags(t, srv, projectID)
	if len(tags) == 0 {
		t.Fatalf("no memory row with a tag was stored for %q", projectID)
	}
	return tags[0]
}

// savedID pulls the id out of a memory row in an answer, so a test that needs one
// does not re-implement the row's shape. A row prints its id inside backticks, and
// the first version of this parsed "id: ", which a search line does not contain —
// it read the empty result as "no id in the answer" and failed the whole table for
// a reason that had nothing to do with what was being tested.
func savedID(t *testing.T, answer string) string {
	t.Helper()
	open := strings.Index(answer, "`")
	if open < 0 {
		t.Fatalf("the answer carries no id in backticks: %s", answer)
	}
	rest := answer[open+1:]
	if close := strings.IndexByte(rest, '`'); close >= 0 {
		rest = rest[:close]
	}
	id := strings.TrimSpace(rest)
	if id == "" {
		t.Fatalf("the answer carries an empty id: %s", answer)
	}
	return id
}
