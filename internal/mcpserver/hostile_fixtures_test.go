package mcpserver

import (
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/adversarial"
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
	all := append(append([]adversarial.Payload{}, adversarial.FormatControls()...), adversarial.ZeroWidth()...)
	for _, p := range all {
		p := p
		t.Run(p.Name, func(t *testing.T) {
			_, session, _ := echoLogSession(t)
			res := callTool(t, session, "ghost_memory_save", map[string]any{
				"project_id": "p",
				"content":    "a fact worth keeping",
				"category":   "fact",
				"tags":       []string{p.Text},
			})
			if !res.IsError {
				t.Fatalf("tag write accepted the %s control", p.Name)
			}
			out := resultText(res)
			if !strings.Contains(out, "control character") {
				t.Errorf("tag write did not refuse the %s control: %s", p.Name, out)
			}
		})
	}
}
