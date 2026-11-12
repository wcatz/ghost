package mcpinit

import (
	"encoding/json"
	"strings"

	"github.com/wcatz/ghost/internal/assemble"
)

func shownOnly(projectShown, globalsShown int) sessionTally {
	return sessionTally{
		project: assemble.BucketTally{Shown: projectShown},
		globals: assemble.BucketTally{Shown: globalsShown},
	}
}

func (s *settingsFile) hasHook(event, cmdSubstr string) bool {
	hooksRaw, ok := s.raw["hooks"]
	if !ok {
		return false
	}

	var hooks map[string][]hookEntry
	if err := json.Unmarshal(hooksRaw, &hooks); err != nil {
		return false
	}

	for _, entry := range hooks[event] {
		for _, h := range entry.Hooks {
			if strings.Contains(h.Command, cmdSubstr) {
				return true
			}
		}
	}
	return false
}
