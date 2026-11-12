// Package followup renders repair commands for resolve and supersede operations.
package followup

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/wcatz/ghost/internal/assemble"
)

var bareShellWord = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._+-]*$`)

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ReassessCommand renders the project-wide supersede repair command.
func ReassessCommand(projectName string, consensus int) string {
	return fmt.Sprintf("ghost supersede %s --reassess --consensus %d --apply",
		projectArg(projectName), consensus)
}

// WithdrawCommand renders a command to withdraw one named edge.
// nameable is false when the ids cannot be passed as --withdraw operands.
func WithdrawCommand(projectName, sourceID, targetID string) (cmd string, nameable bool) {
	if !withdrawOperandNameable(sourceID) || !withdrawOperandNameable(targetID) {
		return "", false
	}
	return fmt.Sprintf("ghost supersede %s --withdraw %s %s --apply",
		projectArg(projectName), shellQuote(sourceID), shellQuote(targetID)), true
}

func withdrawOperandNameable(id string) bool {
	return id != "" && !strings.HasPrefix(id, "-")
}

func projectArg(projectName string) string {
	if bareShellWord.MatchString(projectName) {
		return projectName
	}
	return "--project " + shellQuote(projectName)
}

// ResolveCommand renders a scoped resolve repair command for the given ids.
// It returns ids that require --only-file and ids that cannot be named on the command line.
func ResolveCommand(projectName string, ids []string) (cmd string, viaFile, unnameable []string) {
	project := projectArg(projectName)

	quoted := make([]string, 0, len(ids))
	for _, id := range ids {
		switch {
		case strings.ContainsAny(id, "\n\r"):
			unnameable = append(unnameable, id)
		case strings.Contains(id, ","):
			viaFile = append(viaFile, id)
		default:
			quoted = append(quoted, shellQuote(id))
		}
	}
	if len(quoted) == 0 {
		return "", viaFile, unnameable
	}
	return fmt.Sprintf("ghost resolve %s --reassess --only %s --apply", project, strings.Join(quoted, ",")), viaFile, unnameable
}

// RenderUncarriedIDs renders the uncarried id buckets as indented lines.
func RenderUncarriedIDs(viaFile, unnameable []string) (viaFileText, unnameableText string) {
	return uncarriedIDLines(viaFile), uncarriedIDLines(unnameable)
}

func uncarriedIDLines(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	var b strings.Builder
	for _, id := range ids {
		fmt.Fprintf(&b, "    %s\n", assemble.Token(id))
	}
	return b.String()
}
