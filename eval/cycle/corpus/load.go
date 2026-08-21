// Package corpus loads and validates the annotated eval-cycle memory corpus.
// Annotations ride alongside normal save fields but are harness-only: they
// tell the evalcycle runner what supersede/resolve/reflect SHOULD do so the
// stages can be graded, and never reach the save path.
package corpus

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Entry is one corpus memory plus optional grading annotations.
type Entry struct {
	Key                  string   `json:"key"`
	Category             string   `json:"category"`
	Content              string   `json:"content"`
	Importance           float32  `json:"importance"`
	Tags                 []string `json:"tags,omitempty"`
	ExpectedSupersededBy string   `json:"expected_superseded_by,omitempty"` // on the OLDER member; value is the newer key
	ExpectedResolved     bool     `json:"expected_resolved,omitempty"`
	MergeGroup           string   `json:"merge_group,omitempty"`
}

// TagsOrEmpty returns e.Tags, never nil — the MCP save tool requires a JSON
// array for tags.
func (e Entry) TagsOrEmpty() []string {
	if e.Tags == nil {
		return []string{}
	}
	return e.Tags
}

var validCategories = map[string]bool{
	"architecture": true, "decision": true, "pattern": true, "convention": true,
	"gotcha": true, "dependency": true, "preference": true, "fact": true,
}

// Load reads entries from JSONL, skipping blank and '#' comment lines.
// Duplicate keys are rejected: keys are the grading identity.
func Load(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck
	var out []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	line := 0
	seen := map[string]bool{}
	for sc.Scan() {
		line++
		b := strings.TrimSpace(sc.Text())
		if b == "" || strings.HasPrefix(b, "#") {
			continue
		}
		var e Entry
		if err := json.Unmarshal([]byte(b), &e); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		if seen[e.Key] {
			return nil, fmt.Errorf("line %d: duplicate key %q", line, e.Key)
		}
		seen[e.Key] = true
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// Validate checks cross-entry integrity so a malformed corpus fails loudly
// before any LLM spend rather than grading garbage silently.
func Validate(entries []Entry) error {
	keys := make(map[string]bool, len(entries))
	for i, e := range entries {
		if e.Key == "" || e.Content == "" {
			return fmt.Errorf("entry %d: key and content are required", i)
		}
		if !validCategories[e.Category] {
			return fmt.Errorf("%s: invalid category %q", e.Key, e.Category)
		}
		keys[e.Key] = true
	}
	for _, e := range entries {
		if e.ExpectedSupersededBy != "" && !keys[e.ExpectedSupersededBy] {
			return fmt.Errorf("%s: expected_superseded_by %q not in corpus", e.Key, e.ExpectedSupersededBy)
		}
	}
	groups := map[string]int{}
	for _, e := range entries {
		if e.MergeGroup != "" {
			groups[e.MergeGroup]++
		}
	}
	for g, n := range groups {
		if n < 2 {
			return fmt.Errorf("merge group %q has %d members; need >=2", g, n)
		}
	}
	return nil
}
