package memory

import (
	"fmt"

	"github.com/wcatz/ghost/internal/secret"
)

func redactHistoryContent(content string) string {
	f, ok := secret.Detect(content)
	if !ok {
		return content
	}
	return fmt.Sprintf(
		"<redacted: %s (%s). This earlier version of the memory held a value of that "+
			"shape, so its text is not kept. The live memory row is authoritative; "+
			"`ghost history purge` removes these entries.>",
		f.Label, f.Rule)
}

func init() { setHistoryRedactor(redactHistoryContent) }
