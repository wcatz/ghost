package memory

import (
	"context"
	"strings"
	"testing"
)

// TestDeleteAndPinNotFoundNeverEchoTheID: the "not found" a delete, a purge or
// an unscoped pin reports for a missing row renders the caller's memory id
// through RefusalArg, so a token-shaped id and one longer than ArgEchoMaxBytes
// come back as neither, and the error still says not found.
func TestDeleteAndPinNotFoundNeverEchoTheID(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	ids := map[string]string{
		"token-shaped": "ghp_" + strings.Repeat("a", 36),
		"oversized":    strings.Repeat("z", ArgEchoMaxBytes+44),
	}
	for shape, id := range ids {
		calls := map[string]error{
			"Delete":               s.Delete(ctx, id),
			"DeleteWithOptions":    s.DeleteWithOptions(ctx, id, DeleteOptions{ProjectID: testProject}),
			"TogglePin":            s.TogglePin(ctx, id, true),
			"TogglePinInProject":   s.TogglePinInProject(ctx, testProject, id, true),
			"DeletePurge":          func() error { _, err := s.DeletePurge(ctx, id); return err }(),
			"DeletePurgeInProject": func() error { _, err := s.DeletePurgeInProject(ctx, testProject, id); return err }(),
		}
		for name, err := range calls {
			if err == nil {
				t.Errorf("%s %s: a missing row was not refused", shape, name)
				continue
			}
			if !strings.Contains(err.Error(), "not found") {
				t.Errorf("%s %s: err = %v, want a not-found refusal", shape, name, err)
			}
			if strings.Contains(err.Error(), id) {
				t.Errorf("%s %s: the refusal repeats the memory id: %.120q", shape, name, err.Error())
			}
		}
	}
}
