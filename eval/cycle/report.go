package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ReportData is everything the Markdown scorecard renders.
type ReportData struct {
	Date       string
	CorpusPath string
	Project    string
	Total      int
	ScratchDir string

	Supersede   Stage
	Resolve     Stage
	SkipReflect bool
	Reflect     ReflectReport
}

// writeReport renders the dated Markdown report under dir and returns its path.
func writeReport(dir string, d ReportData) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	day := time.Now().Format("2006-01-02")
	path := filepath.Join(dir, day+"-report.md")
	if _, err := os.Stat(path); err == nil {
		path = filepath.Join(dir, fmt.Sprintf("%s-report-%s.md", day, time.Now().Format("150405")))
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# Eval cycle report — %s\n\n", d.Date)
	fmt.Fprintf(&b, "- Project: `%s` (scratch-isolated; production DB untouched)\n", d.Project)
	fmt.Fprintf(&b, "- Corpus: `%s` (%d memories)\n", d.CorpusPath, d.Total)
	fmt.Fprintf(&b, "- Scratch: `%s`\n", d.ScratchDir)
	model := os.Getenv("GHOST_OPENCODE_MODEL")
	if model == "" {
		model = "(opencode default)"
	}
	fmt.Fprintf(&b, "- LLM backend: claude-or-opencode CLI path; opencode model: %s\n\n", model)

	writeStageSection(&b, "Supersede", d.Supersede)
	writeStageSection(&b, "Resolve", d.Resolve)

	b.WriteString("## Reflect (set-level)\n\n")
	if d.SkipReflect {
		b.WriteString("Skipped via `--skip-reflect`.\n\n")
	} else {
		fmt.Fprintf(&b, "| metric | value |\n|---|---|\n| memories before | %d |\n| memories after | %d |\n| dropped-important members | %d |\n\n",
			d.Reflect.Before, d.Reflect.After, d.Reflect.DroppedImportant)
		b.WriteString("| group | status | survivors | detail |\n|---|---|---|---|\n")
		for _, g := range d.Reflect.Groups {
			fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", g.Group, g.Status, g.Survivors, g.Detail)
		}
		if len(d.Reflect.DroppedDistractors) > 0 {
			b.WriteString("\nDropped distractor keys: ")
			b.WriteString(strings.Join(d.Reflect.DroppedDistractors, ", "))
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	b.WriteString("## Misclassifications\n\n")
	wrote := false
	for _, st := range []Stage{d.Supersede, d.Resolve} {
		for _, m := range st.Misses {
			fmt.Fprintf(&b, "- **%s/%s** (%s): %s\n", strings.ToLower(st.Name), m.Kind, m.Key, m.Detail)
			wrote = true
		}
	}
	for _, k := range d.Reflect.DroppedDistractors {
		fmt.Fprintf(&b, "- **reflect/dropped-distractor** (%s)\n", k)
		wrote = true
	}
	if !wrote {
		b.WriteString("None — every annotated expectation was met.\n")
	}

	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func writeStageSection(b *strings.Builder, title string, st Stage) {
	fmt.Fprintf(b, "## %s\n\n", title)
	fmt.Fprintf(b, "| TP | FP | FN | causes | precision | recall |\n")
	b.WriteString("|---|---|---|---|---|---|\n")
	fmt.Fprintf(b, "| %d | %d | %d | %d | %.2f | %.2f |\n\n",
		st.TP, st.FP, st.FN, st.Causes, st.Precision(), st.Recall())
	if len(st.Misses) > 0 {
		b.WriteString("| kind | key | detail |\n|---|---|---|\n")
		for _, m := range st.Misses {
			fmt.Fprintf(b, "| %s | %s | %s |\n", m.Kind, m.Key, m.Detail)
		}
		b.WriteString("\n")
	}
}
