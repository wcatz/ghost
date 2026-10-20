package mcpserver

import (
	"context"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/adversarial"
	"github.com/wcatz/ghost/internal/memory"
)

// TestSharedTokenShapedProjectArgumentsAreWithheldByResolveResults runs the
// shared token-shaped set through the two resolve tools that used to echo the
// raw project argument (#1637). The tools are the boundary; the shared set is
// what makes the next class reach them too.
func TestSharedTokenShapedProjectArgumentsAreWithheldByResolveResults(t *testing.T) {
	for _, p := range adversarial.TokenShaped() {
		t.Run("resolve_mark/"+p.Name, func(t *testing.T) {
			_, session, _ := echoLogSession(t)
			out := callToolErr(t, session, "ghost_resolve_mark", map[string]any{
				"project_id": p.Text,
				"memory_ids": []string{"AABBCCDD"},
			})
			assertSharedValueWithheld(t, "ghost_resolve_mark", out, p)
		})
		t.Run("resolve/"+p.Name, func(t *testing.T) {
			_, session, _ := echoLogSession(t)
			out := callToolErr(t, session, "ghost_resolve", map[string]any{"project": p.Text})
			assertSharedValueWithheld(t, "ghost_resolve", out, p)
		})
	}
}

func assertSharedValueWithheld(t *testing.T, surface, out string, p adversarial.Payload) {
	t.Helper()
	if strings.Contains(out, p.Text) {
		t.Errorf("%s echoed the %s value raw:\n%s", surface, p.Name, out)
	}
	if len(out) > 2048 {
		t.Errorf("%s refusal is %d bytes, want a bounded sentence:\n%s", surface, len(out), out)
	}
}

// TestSharedFormatControlsAreRefusedByTheTagWriteBoundary runs the shared
// control set through the one write path that refuses a hostile tag (#1648's
// tag class, #1651's bidi sibling). The write path and the render path are
// different boundaries, so the shared set is run at both.
func TestSharedFormatControlsAreRefusedByTheTagWriteBoundary(t *testing.T) {
	for _, p := range adversarial.FormatControls() {
		t.Run(p.Name, func(t *testing.T) {
			_, session, _ := echoLogSession(t)
			out := callToolErr(t, session, "ghost_memory_save", map[string]any{
				"project_id": "p",
				"content":    "a fact worth keeping",
				"category":   "fact",
				"tags":       []string{p.Text},
			})
			if !strings.Contains(out, "control character") {
				t.Errorf("tag write did not refuse the %s control: %s", p.Name, out)
			}
		})
	}
}

// TestSharedLineSeparatorsAreRefusedOrRenderedSafelyAtTheTagBoundary: a line
// separator is not a control character, so the write path may accept it, but
// the rendered row must still be one physical line.
func TestSharedLineSeparatorsAreRefusedOrRenderedSafelyAtTheTagBoundary(t *testing.T) {
	for _, p := range adversarial.LineSeparators() {
		t.Run(p.Name, func(t *testing.T) {
			_, session := newCapSession(t)
			res := callTool(t, session, "ghost_memory_save", map[string]any{
				"project_id": "test-project",
				"content":    "a fact worth keeping",
				"category":   "fact",
				"tags":       []string{p.Text},
			})
			if res.IsError {
				if !strings.Contains(resultText(res), "control character") {
					t.Fatalf("tag write refused %s for an unexpected reason: %s", p.Name, resultText(res))
				}
				return
			}
			out := resultText(callTool(t, session, "ghost_memories_list", map[string]any{"project_id": "test-project"}))
			if strings.ContainsAny(out, "\u2028\u2029") {
				t.Errorf("listing printed the %s separator raw:\n%q", p.Name, out)
			}
		})
	}
}

// TestSharedOversizedTagsAreTrimmedAtTheTagWriteBoundary covers the oversized
// field class at the write boundary: the tag writer trims a tag past its display
// bound rather than storing the whole value, which is the documented direction
// for a label.
func TestSharedOversizedTagsAreTrimmedAtTheTagWriteBoundary(t *testing.T) {
	for _, p := range adversarial.Oversized() {
		if p.Name != "oversized-tag" {
			continue
		}
		t.Run(p.Name, func(t *testing.T) {
			srv, session := newCapSession(t)
			res := callTool(t, session, "ghost_memory_save", map[string]any{
				"project_id": "test-project",
				"content":    "a fact worth keeping",
				"category":   "fact",
				"tags":       []string{p.Text},
			})
			if res.IsError {
				t.Fatalf("oversized tag was refused rather than trimmed: %s", resultText(res))
			}
			id, ok := extractID(resultText(res))
			if !ok {
				t.Fatalf("save response carries no id: %s", resultText(res))
			}
			mems, err := srv.store.GetByIDs(context.Background(), []string{id})
			if err != nil || len(mems) != 1 {
				t.Fatalf("GetByIDs(%s): err=%v n=%d", id, err, len(mems))
			}
			if len(mems[0].Tags) != 1 || len(mems[0].Tags[0]) > tagMaxLen {
				t.Errorf("stored tag is %q, want at most %d bytes", mems[0].Tags, tagMaxLen)
			}
		})
	}
}

// TestSharedFormatControlsReachTheListingFromAStoredRow is the render boundary
// for a tag the write path never saw: a restored row or an artifact can carry
// one, and the listing must print it safely.
func TestSharedFormatControlsReachTheListingFromAStoredRow(t *testing.T) {
	srv, session := newCapSession(t)
	ctx := context.Background()
	for _, p := range adversarial.AllControls() {
		if _, err := srv.store.Create(ctx, "abc123", memory.Memory{
			Content:    "stored fact " + p.Name,
			Category:   "fact",
			Importance: 0.5,
			Source:     "mcp",
			Tags:       []string{p.Text},
		}); err != nil {
			t.Fatalf("Create(%s): %v", p.Name, err)
		}
	}
	out := resultText(callTool(t, session, "ghost_memories_list", map[string]any{"project_id": "test-project"}))
	for _, p := range adversarial.FormatControls() {
		if strings.Contains(out, p.Text) {
			t.Errorf("listing printed the %s control raw:\n%q", p.Name, out)
		}
	}
	for _, p := range adversarial.LineSeparators() {
		if strings.ContainsAny(out, "\u2028\u2029") {
			t.Errorf("listing printed the %s separator raw:\n%q", p.Name, out)
		}
	}
}
