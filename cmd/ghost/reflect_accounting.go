package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/reflection"
)

func tokenList(ids []string) string {
	rendered := make([]string, 0, len(ids))
	for _, id := range ids {
		rendered = append(rendered, assemble.Token(id))
	}
	return strings.Join(rendered, ", ")
}

type reflectRun struct {
	input      reflection.ReflectionInput
	result     reflection.ReflectionResult
	guarded    []reflection.DroppedGuarded
	allowDrops bool
	full       bool
}

func reportInputAccounting(w io.Writer, run reflectRun) {
	limit := reflectTextLimit(run.full, reflectProposalLimit)
	acc := reflection.AccountInputs(run.input, run.result, run.guarded)

	_, _ = fmt.Fprintf(w, "Inputs (%d) accounted for; every count below is ids, so they add up to it:\n", acc.Inputs)

	merged, refused, overflowed, scopeRefused := 0, 0, 0, 0
	for _, m := range acc.Merges {
		merged += len(m.IDs)
	}
	for _, r := range acc.Refusals {
		refused += len(r.IDs)
	}
	for _, r := range acc.LinkOverflows {
		overflowed += len(r.IDs) + len(r.Carriers)
	}
	for _, r := range acc.ScopeRefusals {
		scopeRefused += len(r.IDs)
	}

	_, _ = fmt.Fprintf(w, "Merges (%d):\n", merged)
	for _, m := range acc.Merges {
		line := fmt.Sprintf("  new <- %s   (%d B from %d B)", tokenList(m.IDs), len(m.Text), m.SourceBytes)
		if !m.In {
			line += "; the merged text is not in this result"
		}
		_, _ = fmt.Fprintln(w, line+guardClause(m.Guarded, run.allowDrops, "source"))
	}

	_, _ = fmt.Fprintf(w, "Refused by the grounding check (%d):\n", refused)
	for _, r := range acc.Refusals {
		_, _ = fmt.Fprintf(w, "  %s <- the %s introduced identifiers no source carries: %s; the sources are kept unchanged\n",
			tokenList(r.IDs), r.Kind, displayClaim(strings.Join(r.Identifiers, ", "), 0))
	}

	_, _ = fmt.Fprintf(w, "Refused because the merge folds more decision links than one source_ref can hold (%d):\n", overflowed)
	for _, r := range acc.LinkOverflows {
		line := fmt.Sprintf("  %s <- the merge folds %d decision links, whose joined source_ref would exceed the %d-byte bound; the sources are kept unchanged",
			tokenList(r.IDs), r.Links, r.Bound)
		if len(r.Carriers) > 0 {
			line += fmt.Sprintf(", and so is %s, which already carries the merge's text", tokenList(r.Carriers))
		}
		_, _ = fmt.Fprintln(w, line)
	}

	_, _ = fmt.Fprintf(w, "Refused because the sources' scopes disagree (%d):\n", scopeRefused)
	for _, r := range acc.ScopeRefusals {
		_, _ = fmt.Fprintf(w, "  %s <- the merge is refused because its sources' scopes disagree (%s); the sources are kept unchanged\n",
			tokenList(r.IDs), scopeList(r.Scopes))
	}

	_, _ = fmt.Fprintf(w, "Rewrites (%d):\n", len(acc.Rewrites))
	for _, rw := range acc.Rewrites {
		line := fmt.Sprintf("  %s -> %s", assemble.Token(rw.ID), displayClaim(rw.Text, limit))
		if !rw.In {
			line += "; the replacement is not in this result"
		}
		_, _ = fmt.Fprintln(w, line+guardClause(boolCount(rw.Guarded), run.allowDrops, "row"))
	}

	_, _ = fmt.Fprintf(w, "Dropped (%d, each audited by the drop guard):\n", len(acc.Drops))
	for _, d := range acc.Drops {
		outcome := "a surviving output carries it"
		if d.Guarded {
			outcome = "nothing in the result carries it"
		}
		reason := d.Reason
		if d.Successor != "" {
			reason += " " + assemble.Token(d.Successor)
		}
		_, _ = fmt.Fprintf(w, "  %s reason: %s — %s%s\n", assemble.Token(d.ID), reason, outcome, guardClause(boolCount(d.Guarded), run.allowDrops, "row"))
	}

	deleted := fmt.Sprintf("Deleted (%d)", len(acc.Deleted))
	if len(acc.Deleted) > 0 {
		deleted += " — rows this round removes from the project:"
	} else {
		deleted += ":"
	}
	_, _ = fmt.Fprintln(w, deleted)
	for _, d := range acc.Deleted {
		_, _ = fmt.Fprintf(w, "  %s  %s\n", assemble.Token(d.ID), d.Reason)
	}

	_, _ = fmt.Fprintf(w, "Kept verbatim: %d    Passed through (not named): %d\n", len(acc.Kept), len(acc.Passed))
}

func scopeList(scopes []map[string]string) string {
	seen := make(map[string]bool, len(scopes))
	out := make([]string, 0, len(scopes))
	for _, sc := range scopes {
		text := "unscoped"
		if len(sc) > 0 {
			b, err := json.Marshal(sc)
			if err != nil {
				continue
			}
			text = displayClaim(string(b), 0)
		}
		if seen[text] {
			continue
		}
		seen[text] = true
		out = append(out, text)
	}
	return strings.Join(out, " vs ")
}

func guardClause(guarded int, allowDrops bool, subject string) string {
	if guarded <= 0 {
		return ""
	}
	plural, verb := "s", "have"
	if guarded == 1 {
		plural, verb = "", "has"
	}
	if allowDrops {
		return fmt.Sprintf("; %d %s%s %s no surviving output, and --allow-drops accepts the deletion", guarded, subject, plural, verb)
	}
	return fmt.Sprintf("; the drop guard re-added %d %s%s verbatim", guarded, subject, plural)
}

func boolCount(guarded bool) int {
	if guarded {
		return 1
	}
	return 0
}
