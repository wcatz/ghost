// dataset.go loads MemoryAgentBench Conflict_Resolution demos (converted to
// JSONL by convert.py) and splits each demo's numbered fact list into
// ordered fact sentences.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
)

// Demo is one row of the Conflict_Resolution split, as written by convert.py:
// a flat numbered fact list plus the single-hop questions probing it.
type Demo struct {
	Source    string     `json:"source"`
	Context   string     `json:"context"`
	Questions []string   `json:"questions"`
	Answers   [][]string `json:"answers"`
	QAPairIDs []string   `json:"qa_pair_ids"`
}

// factLine matches one numbered fact line, e.g. "16. Chanel was founded by
// Coco Chanel." — the captured group is the fact sentence without its
// leading index.
var factLine = regexp.MustCompile(`(?m)^\d+\.[ \t]+([^\r\n]+)`)

// splitFacts splits a demo's context into its ordered fact sentences. List
// order is temporal order: MemoryAgentBench encodes a fact update/contradiction
// as a later line restating an earlier subject+relation with a different
// object, so fact N is understood to have been "stated after" fact N-1.
func splitFacts(text string) []string {
	matches := factLine.FindAllStringSubmatch(text, -1)
	facts := make([]string, len(matches))
	for i, m := range matches {
		facts[i] = m[1]
	}
	return facts
}

// loadDemos reads convert.py's output: one JSON Demo per line.
func loadDemos(path string) ([]Demo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck

	var out []Demo
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<21)
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var d Demo
		if err := json.Unmarshal(line, &d); err != nil {
			return nil, fmt.Errorf("decode demo: %w", err)
		}
		if len(d.Questions) != len(d.Answers) || len(d.Questions) != len(d.QAPairIDs) {
			return nil, fmt.Errorf("demo %s: %d questions, %d answer sets, %d qa_pair_ids (must match)",
				d.Source, len(d.Questions), len(d.Answers), len(d.QAPairIDs))
		}
		out = append(out, d)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
