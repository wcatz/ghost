package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/followup"
	"github.com/wcatz/ghost/internal/scratch"
	"github.com/wcatz/ghost/internal/supersede"
)

func withdrawnTargets(withdrawn []supersede.WithdrawnEdge) []supersede.ProjectTargets {
	rows := make([]supersede.RepairTarget, 0, len(withdrawn))
	for _, w := range withdrawn {
		if w.OlderID == "" {
			continue
		}
		rows = append(rows, supersede.RepairTarget{ID: w.OlderID, ProjectID: w.TargetProjectID})
	}
	return supersede.GroupByProject(rows)
}

func printReassessFollowup(projectName string, targets []string, writtenBy string) string {
	path, err := writeReassessTargets(projectName, targets, writtenBy)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: write the follow-up id file: %v\n", err)
	}
	return supersedeReassessFollowup(projectName, targets, path)
}

func resolveFollowupCommand(projectName string, ids []string) (string, []string, []string) {
	return followup.ResolveCommand(projectName, ids)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func supersedeReassessFollowup(projectName string, ids []string, path string) string {
	if len(ids) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nFollow-up: these are the targets of the edges this run withdrew, and the\n" +
		"resolutions those edges justified. One that no live 'supersedes' edge still\n" +
		"holds can now be cleared; one another edge still holds is reported as still\n" +
		"asserted rather than cleared. Nothing outside this list is judged:\n")
	cmd, viaFileOnly, unnameable := resolveFollowupCommand(projectName, ids)
	if cmd != "" {
		fmt.Fprintf(&b, "  %s\n", cmd)
	}
	fileHoldsSomething := path != "" && !containsLineBreak(path) && len(unnameable) < len(ids)
	if fileHoldsSomething {
		fmt.Fprintf(&b, "  (the same ids are in %s, for `ghost resolve --project %s --reassess --only-file %s --apply`)\n",
			assemble.Label(path), shellQuote(projectName), shellQuote(path))
	}
	viaFileText, unnameableText := followup.RenderUncarriedIDs(viaFileOnly, unnameable)
	switch {
	case len(viaFileOnly) == 0 && len(unnameable) == 0:
	case len(viaFileOnly) == len(ids):
		if fileHoldsSomething {
			fmt.Fprintf(&b, "  (no --only command can name them — every id holds a comma, which --only splits on —\n"+
				"   so the file above is the only way to run this repair. Do NOT fall back on the same command\n"+
				"   without --only: that re-judges every resolved memory in the project)\n")
		} else {
			fmt.Fprintf(&b, "  (no --only command can name them and no id file can be named here, so they are\n"+
				"   named here: put each on its own line in a file and use --only-file. Do NOT fall back on the\n"+
				"   same command without --only: that re-judges every resolved memory in the project)\n")
			b.WriteString(viaFileText)
		}
	case len(viaFileOnly) > 0 && fileHoldsSomething:
		fmt.Fprintf(&b, "  (%d id(s) hold a comma, which --only cannot carry, so the command above leaves them out;\n"+
			"   they are in the file, and the file is the only way to name them)\n", len(viaFileOnly))
	case len(viaFileOnly) > 0:
		fmt.Fprintf(&b, "  (%d id(s) hold a comma and no id file can be named here, so they are named here;\n"+
			"   no --only command can carry them — put each on its own line in a file and use --only-file)\n", len(viaFileOnly))
		b.WriteString(viaFileText)
	}
	if len(unnameable) > 0 {
		fmt.Fprintf(&b, "  (%d id(s) hold a newline, which no --only or --only-file form can carry. No surface can\n"+
			"   name them, so these stay resolved until the row is rewritten — delete and re-save the memory,\n"+
			"   or re-import it under an id without a newline)\n", len(unnameable))
		b.WriteString(unnameableText)
	}
	return b.String()
}

func containsLineBreak(id string) bool { return strings.ContainsAny(id, "\n\r") }

func writeReassessTargets(projectName string, ids []string, writtenBy string) (string, error) {
	if len(ids) == 0 {
		return "", fmt.Errorf("no withdrawn targets to write")
	}
	root, err := scratch.Root()
	if err != nil {
		return "", err
	}
	head, viaFileOnly, unnameable := resolveFollowupCommand(projectName, ids)
	if len(unnameable) == len(ids) {
		return "", fmt.Errorf("no id in this set is nameable through --only-file: all %d hold a newline", len(ids))
	}
	var b strings.Builder
	switch {
	case head != "":
		fmt.Fprintf(&b, "# %s\n", head)
	default:
		fmt.Fprintf(&b, "# No --only command can name some of these ids: %d hold a comma, which --only\n"+
			"# splits on, and %d hold a newline, which no form can carry. The ids this file CAN name\n"+
			"# are below; run `ghost resolve <project> --reassess --only-file <this file> --apply`.\n",
			len(viaFileOnly), len(unnameable))
	}
	if len(viaFileOnly) > 0 {
		fmt.Fprintf(&b, "# (%d id(s) below hold a comma and are not in that command; --only cannot carry them,\n"+
			"#  which is what this file is for)\n", len(viaFileOnly))
	}
	fmt.Fprintf(&b, "# Written by `%s`: the targets of the edges it\n", writtenBy)
	b.WriteString("# withdrew, one id per line. Use with `ghost resolve <project> --reassess --only-file`.\n")
	for _, id := range ids {
		if containsLineBreak(id) {
			continue
		}
		b.WriteString(id + "\n")
	}
	if len(unnameable) > 0 {
		fmt.Fprintf(&b, "# (%d id(s) hold a newline, which no surface can carry, and are NOT below;\n"+
			"#  they are named in the command's own output and must be cleared by rewriting the row)\n", len(unnameable))
	}
	body := b.String()

	for attempt := 0; attempt < reassessTargetsAttempts; attempt++ {
		name, err := reassessTargetsName(projectName)
		if err != nil {
			return "", err
		}
		path := filepath.Join(root, name)
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("write %s: %w", path, err)
		}
		if _, err := f.WriteString(body); err != nil {
			_ = f.Close()
			_ = os.Remove(path)
			return "", fmt.Errorf("write %s: %w", path, err)
		}
		if err := f.Close(); err != nil {
			_ = os.Remove(path)
			return "", fmt.Errorf("write %s: %w", path, err)
		}
		return path, nil
	}
	return "", fmt.Errorf("no free id file name under %s after %d attempts", root, reassessTargetsAttempts)
}

const reassessTargetsAttempts = 3

var defaultReassessTargetsName = scratch.ReassessTargetsName

var reassessTargetsName = defaultReassessTargetsName
