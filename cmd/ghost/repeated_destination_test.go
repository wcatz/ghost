package main

import (
	"strings"
	"testing"
)

// outParser is the one question asked of backup and export together: what does
// this parser make of a command line that names a destination twice? They are
// one table because they are the same parser twice, and a rule that holds for
// one of them and not the other is a rule a reader cannot learn.
type outParser struct {
	name  string
	parse func([]string) (string, error)
}

func backupOut(args []string) (string, error) {
	opts, err := parseBackupArgs(args)
	return opts.Out, err
}

func exportOut(args []string) (string, error) {
	opts, err := parseExportArgs(args)
	return opts.Out, err
}

func obsidianOut(args []string) (string, error) {
	out, _, _, err := parseObsidianFlags(args)
	return out, err
}

// TestRepeatedDestinationIsRefusedByEveryParser: a second --out is a refusal in
// every parser that takes one, whatever it holds, and both spellings are asked.
//
// The hazard is not an overwrite — backup and export both refuse an existing
// path — but a destination the reader did not intend, reported as success. All
// three of these commands are written to be chained (`ghost backup --out A &&
// ...`) or built from an inherited argument list, and the last value on the line
// won silently:
//
//	ghost backup --out intended.db --out actual.db       -> backed up actual.db
//	ghost export --out intended.jsonl --out actual.jsonl -> exported to actual.jsonl
//	ghost obsidian export --out A --out B                -> mirrors into B
//
// Occurrences are counted rather than the value tested for emptiness, because
// that test cannot see `--out A --out=`: the second value is empty, so a test
// reads the first as unset and the artifact lands at A while the command line
// named two destinations. The duplicate is therefore asked BEFORE the empty
// check, exactly as every scope parser in the tree orders its two questions.
//
// Both spellings are in the table for each parser, because each is held by two
// arms and a guard on one arm is a guard on half the flag.
func TestRepeatedDestinationIsRefusedByEveryParser(t *testing.T) {
	for _, p := range []outParser{
		{"backup", backupOut},
		{"export", exportOut},
		{"obsidian", obsidianOut},
	} {
		for _, tc := range []struct {
			name string
			args []string
			want string
		}{
			{"separate then separate", []string{"--out", "intended", "--out", "actual"}, `--out was given twice ("intended" and "actual")`},
			{"separate then equals", []string{"--out", "intended", "--out=actual"}, `--out was given twice ("intended" and "actual")`},
			{"equals then separate", []string{"--out=intended", "--out", "actual"}, `--out was given twice ("intended" and "actual")`},
			{"equals then equals", []string{"--out=intended", "--out=actual"}, `--out was given twice ("intended" and "actual")`},
			// The occurrence count is what this row holds: the second value is
			// EMPTY, so a value test would read it as "no destination given"
			// and keep the first — answering a command line that named two.
			{"a destination then an empty one is still twice", []string{"--out", "intended", "--out="}, `--out was given twice ("intended" and "")`},
			{"a destination then an empty separate one is still twice", []string{"--out", "intended", "--out", ""}, `--out was given twice ("intended" and "")`},
			// Two empty values stop at the FIRST one: there is no destination in
			// the command line to be a duplicate of, so the empty value is what
			// the reader needs told about.
			{"two empty ones stop at the first", []string{"--out=", "--out="}, "--out requires a value"},
			{"an empty one then a destination stops at the first", []string{"--out=", "--out", "actual"}, "--out requires a value"},
		} {
			t.Run(p.name+"/"+tc.name, func(t *testing.T) {
				got, err := p.parse(tc.args)
				if err == nil {
					t.Fatalf("%s(%v) = %q with no error: a destination named twice is a refusal, not the last one", p.name, tc.args, got)
				}
				if !strings.Contains(err.Error(), tc.want) {
					t.Errorf("%s(%v) error = %q, want it to contain %q", p.name, tc.args, err, tc.want)
				}
			})
		}
	}
}
