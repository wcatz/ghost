package mcpinit

import (
	"os"
	"strings"
	"testing"
)

// TestThePermissionsDocPlacesTheStatusRunOnTheReadOnlySide holds
// docs/configuration.md's permissions section to what the status run actually
// does. The section is prose next to the code and no build step reads either,
// so it drifted: it listed `ghost mcp status` among the read-write commands,
// and a status run opens the store through memory.OpenReadDB — which tightens
// no mode. A reader who ran the diagnostic to find out why the store was
// world-readable was told, wrongly, that the diagnostic had fixed it.
//
// Both directions are checked, because a "must not contain" alone would pass on
// a section that had simply dropped the sentence, and because the read-write
// bullet's other claim — that `ghost maintenance status` DOES tighten — is the
// half that tells the two commands apart.
func TestThePermissionsDocPlacesTheStatusRunOnTheReadOnlySide(t *testing.T) {
	section := permissionsSection(t)

	readOnly := docBullet(t, section, "**Read-only opens change nothing.**")
	if !strings.Contains(readOnly, "ghost mcp status") {
		t.Error("docs/configuration.md's read-only bullet does not name `ghost mcp status`, so a reader cannot tell that the diagnostic tightens no mode")
	}

	readWrite := docBullet(t, section, "**Read-write opens tighten**")
	if strings.Contains(readWrite, "ghost mcp status") {
		t.Error("docs/configuration.md's read-write bullet still names `ghost mcp status`: the status run opens the store read-only and tightens no mode")
	}
	if !strings.Contains(readWrite, "ghost maintenance status") {
		t.Error("docs/configuration.md's read-write bullet does not name `ghost maintenance status`, the report command that does tighten the modes")
	}
}

// permissionsSection returns the "Data directory permissions" section of
// docs/configuration.md, so a change to the document around it cannot
// redirect the checks above at another section's text.
func permissionsSection(t *testing.T) string {
	t.Helper()
	const path = "../../docs/configuration.md"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	const heading = "### Data directory permissions"
	doc := string(raw)
	start := strings.Index(doc, heading)
	if start < 0 {
		t.Fatalf("%s has no %q section", path, heading)
	}
	section := doc[start+len(heading):]
	if end := strings.Index(section, "\n#"); end >= 0 {
		section = section[:end]
	}
	return section
}

// docBullet returns the bullet that opens with marker, and fails the test when
// there is none: a section whose bullet was reworded must fail loudly rather
// than pass on an empty string.
func docBullet(t *testing.T, section, marker string) string {
	t.Helper()
	for _, line := range strings.Split(section, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "- "+marker) {
			return line
		}
	}
	t.Fatalf("no bullet opening with %q in the permissions section", marker)
	return ""
}
