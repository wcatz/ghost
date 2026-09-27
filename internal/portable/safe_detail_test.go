package portable

import (
	"strings"
	"testing"
)

// TestSafeDetailScansTheWholeFieldNotThePrintPrefix is the fourth layer of the
// never-print-the-value guarantee, and it is the one that was cut in half.
//
// safeDetail ran the detector on the label, and a memory's label is a 60-rune
// prefix of its first line. A GitHub token is 40 characters, so a memory whose
// value begins just past the cut scans as clean and the report prints the first
// part of a live credential — on a record that is being printed because it was
// SKIPPED or REJECTED for some unrelated reason, so the guard is not even the
// reason the line is on the terminal.
//
// The prefix is still what gets printed. What changed is that the decision reads
// all of the text.
func TestSafeDetailScansTheWholeFieldNotThePrintPrefix(t *testing.T) {
	// Assembled rather than written out: GitHub push protection matches the PAT
	// format anywhere in a diff and rejects the push (GH013) before review.
	credential := "ghp_" + strings.Repeat("a1B2c3D4e5F6", 3) + "AbCd"

	cases := []struct {
		name  string
		field string
		want  string
	}{
		{
			name:  "the value starts inside the print prefix",
			field: "ghp_Ab12Cd34Ef5" + strings.Repeat("9z", 20),
			// The prefix cut lands mid-token, so the label looks clean and the
			// full field does not.
			want: "MEM0001",
		},
		{
			name:  "the value starts just past the print prefix",
			field: "deploy note: " + strings.Repeat("padding ", 8) + credential,
			want:  "MEM0002",
		},
		{
			name:  "the value is on a later line",
			field: "an ordinary first line about the relay\nand the token is " + credential,
			want:  "MEM0003",
		},
		{
			// A long clean memory still gets its preview, which is the whole
			// point of printing a prefix at all.
			name:  "a long clean memory keeps its preview",
			field: "the relay listens on 2222 and the tablet is provisioned " + strings.Repeat("in every zone ", 20),
			want:  contentPrefix("the relay listens on 2222 and the tablet is provisioned " + strings.Repeat("in every zone ", 20)),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := safeDetail(contentPrefix(tc.field), tc.field, tc.want)
			if strings.Contains(got, credential) || strings.Contains(got, "ghp_") {
				t.Fatalf("safeDetail returned something holding the credential: %q", got)
			}
			if tc.want == contentPrefix(tc.field) {
				// The clean case must return the label unchanged.
				if got != tc.want {
					t.Errorf("safeDetail(%q) = %q, want the prefix unchanged", tc.field, got)
				}
				return
			}
			if got != tc.want {
				t.Errorf("safeDetail(%q) = %q, want the id %q — the prefix cuts the value and the label reads clean", tc.field, got, tc.want)
			}
		})
	}
}
