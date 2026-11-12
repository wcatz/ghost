package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
)

type Demo struct {
	Source    string     `json:"source"`
	Context   string     `json:"context"`
	Questions []string   `json:"questions"`
	Answers   [][]string `json:"answers"`
	QAPairIDs []string   `json:"qa_pair_ids"`
}

var factLine = regexp.MustCompile(`(?m)^\d+\.[ \t]+([^\r\n]+)`)

func splitFacts(text string) []string {
	matches := factLine.FindAllStringSubmatch(text, -1)
	facts := make([]string, len(matches))
	for i, m := range matches {
		facts[i] = m[1]
	}
	return facts
}

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
