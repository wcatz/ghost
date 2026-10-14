package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readCLIDoc reads docs/cli.md once for the assertions that hold the documented
// contract to the parser.
func readCLIDoc(t *testing.T) string {
	t.Helper()
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "cli.md"))
	if err != nil {
		t.Fatalf("read docs/cli.md: %v", err)
	}
	return string(doc)
}

// docRowContaining returns the single table row starting with lead, or "" when
// there is none. A row is one line, which is what a flag table's rows are, and
// anchoring on the row's own opening cells keeps the assertion off the rest of
// the page: --out is documented by four separate commands, and each one is
// claimed here by its lead.
func docRowContaining(t *testing.T, doc, lead string) string {
	t.Helper()
	found := ""
	for _, line := range strings.Split(doc, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), lead) {
			continue
		}
		if found != "" {
			t.Fatalf("docs/cli.md has more than one row beginning %q, so the assertion cannot say which it means", lead)
		}
		found = strings.TrimSpace(line)
	}
	return found
}

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

// TestRepeatedIntervalIsRefusedByObsidian is the same rule for obsidian's second
// destination-ish flag, and it is asked separately because --interval is spelled
// in only this parser.
//
// The silent half of it is the one a reader cannot catch: sync logs the cadence
// it is running at, so `--interval 5m --interval 9m` polls on a schedule the
// operator wrote down twice and did not get. Exit 0 is not the complaint here
// (sync is a poller, not a one-shot writer); the wrong cadence in effect is.
func TestRepeatedIntervalIsRefusedByObsidian(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"separate then separate", []string{"--interval", "5m", "--interval", "9m"}, `--interval was given twice ("5m" and "9m")`},
		{"separate then equals", []string{"--interval", "5m", "--interval=9m"}, `--interval was given twice ("5m" and "9m")`},
		{"equals then separate", []string{"--interval=5m", "--interval", "9m"}, `--interval was given twice ("5m" and "9m")`},
		{"equals then equals", []string{"--interval=5m", "--interval=9m"}, `--interval was given twice ("5m" and "9m")`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, interval, err := parseObsidianFlags(tc.args)
			if err == nil {
				t.Fatalf("parseObsidianFlags(%v) = %q with no error: a cadence named twice is a refusal, not the last one", tc.args, interval)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("parseObsidianFlags(%v) error = %q, want it to contain %q", tc.args, err, tc.want)
			}
		})
	}

	// The control: ONE interval still parses, in both spellings, beside --out
	// and --project. A guard that refused the flag outright would pass the table
	// above and break every documented `ghost obsidian sync --interval 30s`.
	for _, args := range [][]string{{"--interval", "30s"}, {"--interval=30s"}} {
		_, _, interval, err := parseObsidianFlags(args)
		if err != nil || interval != "30s" {
			t.Errorf("parseObsidianFlags(%v) = %q (err %v), want 30s and no error", args, interval, err)
		}
	}
}

// TestADestinationStaysParsableBesideTheOthers is the control for the refusals
// above, in every parser: a single --out still names the destination, beside a
// --project and (for obsidian) an --interval, in both spellings. Without it a
// parser that refused every --out would pass both tables above.
func TestADestinationStaysParsableBesideTheOthers(t *testing.T) {
	for _, p := range []outParser{
		{"backup", backupOut},
		{"export", exportOut},
		{"obsidian", obsidianOut},
	} {
		for _, args := range [][]string{{"--out", "x.db"}, {"--out=x.db"}} {
			got, err := p.parse(args)
			if err != nil || got != "x.db" {
				t.Errorf("%s(%v) = %q (err %v), want x.db and no error", p.name, args, got, err)
			}
		}
	}
	if got, err := exportOut([]string{"--project", "ghost", "--out", "x.jsonl"}); err != nil || got != "x.jsonl" {
		t.Errorf("export with one project and one destination = %q (err %v), want x.jsonl", got, err)
	}
	if out, _, interval, err := parseObsidianFlags([]string{"--out", "/v", "--project=ghost", "--interval=30s"}); err != nil ||
		out != "/v" || interval != "30s" {
		t.Errorf("obsidian with one of each = %q/%q (err %v), want /v/30s", out, interval, err)
	}
}

// TestARepeatedDestinationRefusalWithholdsACredential: the refusal NAMES both
// values, so it reaches the terminal and the log, and docs/invariants.md's rule
// (#839) is that a refusal quoting caller text routes the value through
// memory.RefusalArg rather than interpolating it with %q. A destination path can
// carry a token-shaped string — a signed URL, a path built from an API key — and
// a refusal that echoed one verbatim would print it to the terminal.
//
// Both spellings of the second --out for each parser, because the two arms are
// separate code and the value reaches the sentence from both.
func TestARepeatedDestinationRefusalWithholdsACredential(t *testing.T) {
	const key = "sk-ant-api03-Zz09XxYyWwVvUuTtSsRrQqPpOoNnMmLlKkJj01"
	for _, p := range []outParser{
		{"backup", backupOut},
		{"export", exportOut},
		{"obsidian", obsidianOut},
	} {
		for _, tc := range []struct {
			name string
			args []string
		}{
			{"second value attached", []string{"--out=" + key, "--out=/other"}},
			{"second value separate", []string{"--out=" + key, "--out", "/other"}},
			{"credential in the SECOND value", []string{"--out=/first", "--out=" + key}},
		} {
			t.Run(p.name+"/"+tc.name, func(t *testing.T) {
				_, err := p.parse(tc.args)
				if err == nil {
					t.Fatalf("%s(%v) accepted the same destination twice", p.name, tc.args)
				}
				msg := err.Error()
				if strings.Contains(msg, key) {
					t.Errorf("the refusal quotes the credential the reader passed:\n%s", msg)
				}
				if !strings.Contains(msg, "twice") {
					t.Errorf("the refusal no longer names the duplicate:\n%s", msg)
				}
				if !strings.Contains(msg, "<out withheld") {
					t.Errorf("the refusal does not say which argument was withheld:\n%s", msg)
				}
			})
		}
	}

	// And the interval, whose operand is caller text on the same surface.
	if _, _, _, err := parseObsidianFlags([]string{"--interval=" + key, "--interval=9m"}); err == nil {
		t.Fatal("obsidian accepted the same interval twice")
	} else if strings.Contains(err.Error(), key) {
		t.Errorf("the interval refusal quotes the credential the reader passed:\n%s", err)
	} else if !strings.Contains(err.Error(), "<interval withheld") {
		t.Errorf("the interval refusal does not say which argument was withheld:\n%s", err)
	}
}

// TestTheCLIReferenceStatesARefusedRepeatedDestination holds the documented
// contract to the parser, the way TestTheCLIReferenceStatesReflectRefusesASecondProject
// holds the retired last-one-wins claim: docs/cli.md documented the refusal for
// `ghost context` and said nothing at all for backup, export and obsidian, which
// is what let a caller follow the reference and get exit 0 with the artifact at a
// name nobody chose.
//
// The assertion is on the flag rows themselves, because that is where a reader
// looks for what a flag refuses.
func TestTheCLIReferenceStatesARefusedRepeatedDestination(t *testing.T) {
	doc := readCLIDoc(t)
	for _, tc := range []struct {
		name string
		row  string
	}{
		{"backup --out", "| `--out <path>` | Where to write the snapshot."},
		{"export --out", "| `--out <path>` | Where to write it."},
		{"obsidian export --out", "| `--out <path>` | Vault directory. Defaults to `obsidian.vault_dir`"},
		{"obsidian sync --out", "| `--out <path>` | Vault directory. An EMPTY value is refused, as for `export`, and so is a repeated"},
		{"obsidian sync --interval", "| `--interval <duration>` | Positive Go duration; defaults to `obsidian.interval` or `30s`."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := docRowContaining(t, doc, tc.row)
			if row == "" {
				t.Fatalf("docs/cli.md no longer has the flag row %q, so this test cannot tell whether it documents the refusal", tc.row)
			}
			if !strings.Contains(row, "twice") {
				t.Errorf("docs/cli.md's row does not document the repeated-flag refusal: %s", row)
			}
		})
	}
}
