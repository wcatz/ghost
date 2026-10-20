package resolve

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/adversarial"
	"github.com/wcatz/ghost/internal/memory"
)

// TestSharedTokenShapedStoredIDsAreWithheldByTheVetoLog runs the shared
// token-shaped set through the resolve veto log line, which is a stored id on a
// line that reaches lifecycle.log (#1641).
func TestSharedTokenShapedStoredIDsAreWithheldByTheVetoLog(t *testing.T) {
	for _, p := range adversarial.TokenShaped() {
		t.Run(p.Name, func(t *testing.T) {
			log := &replyLog{}
			rule := memory.Memory{ID: p.Text, Category: "gotcha",
				Content: "NEVER run `dingo database restore` with source and target on the same spindle. Fixed in the 0.31 release notes."}
			store := &fakeStore{candidates: []memory.Memory{rule}}
			cls := &fakeClassifier{drop: map[string]bool{rule.Content: true}}

			if _, _, err := Run(context.Background(), store, cls, "proj", true, slog.New(log)); err != nil {
				t.Fatalf("Run: %v", err)
			}
			rec := log.only(t, "resolve veto kept memory")
			got := rec.attrs["id"]
			if strings.Contains(got, p.Text) {
				t.Errorf("the veto line logged id=%s, which repeats the stored id", got)
			}
			if !strings.Contains(got, "withheld") {
				t.Errorf("the veto line logged id=%s, want the guard's placeholder", got)
			}
		})
	}
}
