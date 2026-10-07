package bench

// This file measures the PASSIVE context surfaces: the session-start block (and
// `ghost context`, which is the same render), ghost_project_context, and the
// project-context resource the recall_project prompt shares.
//
// Everything they render comes out of assemble.Run in passive mode. The two
// entry points differ — session start selects through mcpinit's
// sessionPassiveBudget (a project slice plus a `_global` slice, each with its own
// policy), project context through mcpserver's projectContextBudget (one union
// bucket at the caller's limit) — so each is measured separately and each is
// called through the function production calls, at the production budget, with a
// fixed clock. Nothing in this file selects, orders, filters or renders a row; it
// reads rendered text and compares it with the corpus's own grades.
//
// The measurement is of the RENDERED block, by id, and not of assemble.Result: the
// session-start header counts rows and the surface prints what the renderer chose,
// so a figure read off the Result would be a statement about a value the caller
// never received. A row is "rendered" when its backticked id is on a line of the
// shape Item.Line prints.
//
// The honesty check is the one figure that can fail on a correct corpus: it
// compares the counts the session-start header SAYS with what the block holds. It
// is reported rather than asserted, because the header's "of M total" is a second
// COUNT of the store rather than the window's own, so withheld rows are counted as
// ranked out (#897, PR #912); on a tree where that is fixed the check passes, and
// the golden test pins whichever state the tree is in.

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/mcpinit"
	"github.com/wcatz/ghost/internal/mcpserver"
	"github.com/wcatz/ghost/internal/memory"
)

// PassiveInstant is the clock `ghost bench --passive` reads the store at. It is the
// context bench's clock: one fixed date, years from every window in the corpus.
func PassiveInstant() time.Time { return ContextInstant() }

// The caps each surface is measured against, stated here only to compute the
// budget-use figure. They are NOT what produces the blocks — the production
// budgets do — and TestPassiveCapsAreTheOnesTheSurfacesUse fails if these drift
// from them, which a copied constant would otherwise do silently.
const (
	passiveSessionProjectCap = 15 // mcpinit sessionMemoriesCap
	passiveSessionGlobalCap  = 8  // mcpinit globalsCap
	passiveToolLimit         = 20 // the tool's default `limit`
	passiveResourceCap       = 20 // mcpserver projectContextMemoriesCap
	passiveResourceGlobalCap = 15 // mcpserver projectContextGlobalsCap
)

// PassiveSurfaceSpec is one measured surface: what it is called, whether its read
// carries the production scope, the caps its budget states, and how to read it.
type PassiveSurfaceSpec struct {
	Name string
	// Scoped is whether the read is asked for PassiveScope. It decides how the
	// corpus's out-of-scope rows are graded for this surface.
	Scoped bool
	// ProjectCap and GlobalCap are the row caps the surface's budget states; 0 for
	// a cap that does not exist as a separate number (the tool's one union cap is
	// ProjectCap, with GlobalCap 0 and Union true).
	ProjectCap, GlobalCap int
	Union                 bool
	// PinOptional grades the pinned row optional: the union read (OrderDecay with no
	// two-pass, behaviour floor or pinned-first) does not promise a pin a slot, and
	// whether it should is a separate question the bench does not answer.
	PinOptional bool
	// Header is whether the block prints "N shown of M total" counts for the
	// honesty check to read.
	Header bool
	// Read returns the block for one project.
	Read func(ctx context.Context, env *PassiveEnv, project string) (string, error)
}

// PassiveEnv is the store the surfaces read: an on-disk database (the session-start
// path opens its own read-only handle on a path, and refuses :memory:), the store
// on a writable handle for the project-context path, and the instant.
type PassiveEnv struct {
	Path  string
	Store *memory.Store
	DB    *sql.DB
	At    time.Time
	Cfg   *config.Config
	// ScopedCfg is Cfg with the production session scope set.
	ScopedCfg *config.Config
}

// PassiveSurfaces is the measured set, in report order.
func PassiveSurfaces() []PassiveSurfaceSpec {
	return []PassiveSurfaceSpec{
		{
			Name: "session start / ghost context", ProjectCap: passiveSessionProjectCap, GlobalCap: passiveSessionGlobalCap, Header: true,
			Read: func(_ context.Context, e *PassiveEnv, p string) (string, error) {
				return mcpinit.SessionBlockAt(e.Path, p, e.Cfg, e.At), nil
			},
		},
		{
			Name: "session start, scope env=production", Scoped: true, ProjectCap: passiveSessionProjectCap, GlobalCap: passiveSessionGlobalCap, Header: true,
			Read: func(_ context.Context, e *PassiveEnv, p string) (string, error) {
				return mcpinit.SessionBlockAt(e.Path, p, e.ScopedCfg, e.At), nil
			},
		},
		{
			Name: "ghost_project_context (limit 20)", PinOptional: true, ProjectCap: passiveToolLimit, Union: true,
			Read: func(ctx context.Context, e *PassiveEnv, p string) (string, error) {
				return mcpserver.ProjectContextAt(ctx, e.Store, p, passiveToolLimit, e.At)
			},
		},
		{
			Name: "project resource / recall_project", PinOptional: true, ProjectCap: passiveResourceCap, GlobalCap: passiveResourceGlobalCap,
			Read: func(ctx context.Context, e *PassiveEnv, p string) (string, error) {
				return mcpserver.ProjectResourceAt(ctx, e.Store, p, e.At)
			},
		},
	}
}

// passiveConfig is the configuration the session-start surfaces are read under:
// the compiled defaults, with the two knobs the passive budget reads stated
// explicitly so a GHOST_* variable on the machine cannot move the report.
func passiveConfig(scope map[string]string) *config.Config {
	cfg := config.FallbackConfig()
	cfg.Injection = config.DefaultInjectionConfig()
	cfg.Injection.SessionScope = scope
	cfg.Linking.DemotionThreshold = 0.90
	return cfg
}

// OpenPassiveEnv seeds a corpus into a fresh on-disk store under dir and returns
// the environment the surfaces read. The caller closes it.
func OpenPassiveEnv(ctx context.Context, dir string, c PassiveCorpus, blind PassiveBlind) (*PassiveEnv, func(), error) {
	path := filepath.Join(dir, "passive.db")
	db, err := memory.OpenDB(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open passive store: %w", err)
	}
	store := memory.NewStore(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	at, err := SeedPassive(ctx, store, db, c, PassiveInstant(), blind)
	if err != nil {
		_ = store.Close()
		return nil, nil, err
	}
	env := &PassiveEnv{
		Path: path, Store: store, DB: db, At: at,
		Cfg: passiveConfig(nil), ScopedCfg: passiveConfig(PassiveScope),
	}
	return env, func() { _ = store.Close() }, nil
}

// RunPassiveTemp is RunPassive over a store in a temporary directory, which it
// removes. It is what `ghost bench --passive` runs.
func RunPassiveTemp(ctx context.Context, blind PassiveBlind) (PassiveReport, error) {
	dir, err := os.MkdirTemp("", "ghost-bench-passive-")
	if err != nil {
		return PassiveReport{}, err
	}
	defer os.RemoveAll(dir) //nolint:errcheck
	c, err := NewPassiveCorpus()
	if err != nil {
		return PassiveReport{}, err
	}
	env, closeEnv, err := OpenPassiveEnv(ctx, dir, c, blind)
	if err != nil {
		return PassiveReport{}, err
	}
	defer closeEnv()
	return RunPassive(ctx, env, c, PassiveSurfaces())
}

// HeaderFinding is one disagreement between what a session-start header says and
// what the block holds.
type HeaderFinding struct {
	Project string
	Bucket  string // "project" or "global"
	Header  string
	Problem string
}

// PassiveSurfaceReport is one surface's figures, pooled over every project read.
type PassiveSurfaceReport struct {
	Name   string
	Scoped bool
	Header bool
	// Blocks is how many project reads were measured.
	Blocks int
	// Leaked: withheld rows rendered / withheld rows within the read's reach. Must
	// be 0.
	Leaked Ratio
	// Recall: expected rows rendered / expected rows within the read's reach.
	Recall Ratio
	// Contamination: rows of ANOTHER project rendered / rows rendered.
	Contamination Ratio
	// GlobalShare: `_global` rows rendered / rows rendered.
	GlobalShare Ratio
	// BudgetUse: rows rendered / the row caps the surface's budget states.
	BudgetUse Ratio
	// Cut: rows a read could legitimately have admitted and did not / rows it could.
	Cut Ratio
	// Duplicates: rendered rows that restate or are replaced by another rendered
	// row / rows rendered.
	Duplicates Ratio
	// HeadersChecked and HeadersWrong count the count-lines the honesty check read.
	HeadersChecked int
	Findings       []HeaderFinding
	// MeanBytes and MaxBytes are the rendered block's size; tokens are bytes/4,
	// the estimate the assembler's own Item.Tokens uses.
	MeanBytes int
	MaxBytes  int
	// LeakedIDs names every withheld row that was rendered, for the reader.
	LeakedIDs []string
	// MissedIDs names every expected row a read could have shown and did not.
	MissedIDs []string
}

// PassiveReport is the whole measurement.
type PassiveReport struct {
	Rows     int
	Projects int
	Surfaces []PassiveSurfaceReport
}

var passiveLineRE = regexp.MustCompile("(?m)^- \\[[a-z]+\\] `([^`]+)`")

// renderedIDs returns the ids of the memory lines in a block, in order.
func renderedIDs(block string) []string {
	var ids []string
	for _, m := range passiveLineRE.FindAllStringSubmatch(block, -1) {
		ids = append(ids, m[1])
	}
	return ids
}

// RunPassive reads every surface for every project of the corpus and pools the
// figures per surface.
func RunPassive(ctx context.Context, env *PassiveEnv, c PassiveCorpus, specs []PassiveSurfaceSpec) (PassiveReport, error) {
	byID := c.ByID()
	rep := PassiveReport{Rows: len(c.Rows), Projects: len(PassiveProjects)}
	for _, spec := range specs {
		sr := PassiveSurfaceReport{Name: spec.Name, Scoped: spec.Scoped, Header: spec.Header}
		totalBytes := 0
		for _, project := range PassiveProjects {
			block, err := spec.Read(ctx, env, project)
			if err != nil {
				return PassiveReport{}, fmt.Errorf("%s: read %s: %w", spec.Name, project, err)
			}
			sr.Blocks++
			measurePassiveBlock(&sr, spec, c, byID, project, block)
			totalBytes += len(block)
			if len(block) > sr.MaxBytes {
				sr.MaxBytes = len(block)
			}
		}
		sr.MeanBytes = totalBytes / max(sr.Blocks, 1)
		sr.LeakedIDs = sortedUnique(sr.LeakedIDs)
		sr.MissedIDs = sortedUnique(sr.MissedIDs)
		rep.Surfaces = append(rep.Surfaces, sr)
	}
	return rep, nil
}

// sortedUnique is the ids once each, in order: a `_global` row that leaks into
// four blocks is one leaking row, named once, and the ratio carries the count.
func sortedUnique(ids []string) []string {
	sort.Strings(ids)
	out := ids[:0]
	for i, id := range ids {
		if i == 0 || id != ids[i-1] {
			out = append(out, id)
		}
	}
	return out
}

// measurePassiveBlock folds one block into the surface's figures.
func measurePassiveBlock(sr *PassiveSurfaceReport, spec PassiveSurfaceSpec, c PassiveCorpus, byID map[string]PassiveRow, project, block string) {
	replaced := c.replacements()
	ids := renderedIDs(block)
	rendered := make(map[string]bool, len(ids))
	for _, id := range ids {
		rendered[id] = true
	}

	// The read's reach: this project's rows and `_global`'s, which is everything a
	// block read for this project may legitimately hold. Another project's row is
	// in nobody's reach, which is what makes it contamination.
	var withheld, expected, withheldShown, expectedShown, eligible, eligibleShown int
	for _, r := range c.Rows {
		if r.Project != project && r.Project != PassiveGlobal {
			continue
		}
		shown := rendered[r.ID()]
		switch r.gradeOn(spec) {
		case GradeWithheld:
			withheld++
			if shown {
				withheldShown++
				sr.LeakedIDs = append(sr.LeakedIDs, r.ID())
			}
		case GradeExpected:
			expected++
			if shown {
				expectedShown++
			} else {
				sr.MissedIDs = append(sr.MissedIDs, r.ID())
			}
		}
		if r.eligible(spec.Scoped) {
			eligible++
			if shown {
				eligibleShown++
			}
		}
	}
	sr.Leaked = sr.Leaked.add(withheldShown, withheld)
	sr.Recall = sr.Recall.add(expectedShown, expected)
	sr.Cut = sr.Cut.add(eligible-eligibleShown, eligible)

	foreign, global, dup := 0, 0, 0
	for _, id := range ids {
		r, ok := byID[id]
		switch {
		case !ok:
			// A rendered line the corpus does not know is a row from outside the
			// fixture, which can only be contamination.
			foreign++
		case r.Project == PassiveGlobal:
			global++
		case r.Project != project:
			foreign++
		}
		if ok && redundantBeside(r, rendered, replaced) {
			dup++
		}
	}
	sr.Contamination = sr.Contamination.add(foreign, len(ids))
	sr.GlobalShare = sr.GlobalShare.add(global, len(ids))
	sr.Duplicates = sr.Duplicates.add(dup, len(ids))
	sr.BudgetUse = sr.BudgetUse.add(len(ids), passiveCapOf(spec))

	if spec.Header {
		checkSessionHeaders(sr, spec, c, project, block)
	}
}

// redundantBeside reports whether the row restates, or is replaced by, a row that
// is also in the block. The pairs are the corpus's own edges: replacedBy maps an
// older row's id to the ids of the rows whose `supersedes` edge names it.
func redundantBeside(r PassiveRow, rendered map[string]bool, replacedBy map[string][]string) bool {
	switch r.Kind {
	case KindDuplicate:
		return rendered[corpusID(r.Project, r.DuplicateOf)]
	case KindSuperseded:
		for _, newer := range replacedBy[r.ID()] {
			if rendered[newer] {
				return true
			}
		}
	}
	return false
}

// replacements inverts the corpus's supersedes edges.
func (c PassiveCorpus) replacements() map[string][]string {
	m := map[string][]string{}
	for _, r := range c.Rows {
		if r.Supersedes != "" {
			old := corpusID(r.Project, r.Supersedes)
			m[old] = append(m[old], r.ID())
		}
	}
	return m
}

func passiveCapOf(spec PassiveSurfaceSpec) int { return spec.ProjectCap + spec.GlobalCap }

// The three shapes of session-start count line. The first is what an unfixed tree
// prints, the other two what a tree with #897 prints; the parser reads any of
// them, so the same check runs on both.
var (
	hdrShownRE     = regexp.MustCompile(`(\d+) shown`)
	hdrTotalRE     = regexp.MustCompile(`of (\d+) total`)
	hdrNotShownRE  = regexp.MustCompile(`(\d+) not shown`)
	hdrRankedOutRE = regexp.MustCompile(`(\d+) ranked out`)
	hdrWithheldRE  = regexp.MustCompile(`(\d+) withheld`)
)

const sessionGlobalMarker = "**Global (applies to all projects):**"

// checkSessionHeaders is the honesty check. For each bucket it derives the truth
// from the corpus and the block — rows rendered, rows a stage withheld, rows the
// ranking cut — and compares it with what the header says.
//
// The truth is the fixture's own: a bucket's eligible rows are the ones its kind
// says are in play, so the check never asks the product what it withheld. A header
// is honest when its shown count is the number of lines rendered under it, and
// when whatever it calls "not shown" is exactly the rows the ranking cut, with the
// rows a stage withheld either named as withheld or not counted at all.
func checkSessionHeaders(sr *PassiveSurfaceReport, spec PassiveSurfaceSpec, c PassiveCorpus, project, block string) {
	projectPart, globalPart := block, ""
	if i := strings.Index(block, sessionGlobalMarker); i >= 0 {
		projectPart, globalPart = block[:i], block[i:]
	}
	for _, bucket := range []struct {
		name, owner, text string
	}{
		{"project", project, projectPart},
		{"global", PassiveGlobal, globalPart},
	} {
		var header string
		for _, line := range strings.Split(bucket.text, "\n") {
			if hdrShownRE.MatchString(line) && (strings.HasPrefix(line, "**Memories (") || strings.HasPrefix(line, "(")) {
				header = line
				break
			}
		}
		shown := len(renderedIDs(bucket.text))
		// demotable is the rows the `_global` policy may DROP rather than leave to
		// the cap (DropDemotedLosers): a superseded or duplicate loser removed by a
		// stage is a withheld row to the product and a cut row to this fixture, and
		// either attribution is honest, so the header may name between none and all
		// of them as withheld. Only the global bucket drops; the project bucket
		// reorders them and leaves the drop to the cap.
		var eligible, withheld, demotable int
		for _, r := range c.Rows {
			if r.Project != bucket.owner {
				continue
			}
			if r.Resolved {
				continue // the window never held it, so no count is about it
			}
			if r.Kind == KindScoped && spec.Scoped {
				continue // excluded by the scope clause in the fetch, so never in the window
			}
			if r.withheldByAStage() {
				withheld++
			} else {
				eligible++
				if bucket.name == "global" && (r.Kind == KindSuperseded || r.Kind == KindDuplicate) {
					demotable++
				}
			}
		}
		cut := eligible - shown

		if header == "" {
			// No count line. That is honest only when nothing was cut or withheld;
			// otherwise the block dropped rows without saying so.
			if cut > 0 || withheld > 0 {
				sr.HeadersChecked++
				sr.Findings = append(sr.Findings, HeaderFinding{project, bucket.name, "(none)",
					fmt.Sprintf("no count line, but %d rows were cut and %d withheld", cut, withheld)})
			}
			continue
		}
		sr.HeadersChecked++
		var problems []string
		if n := atoi(hdrShownRE, header); n != shown {
			problems = append(problems, fmt.Sprintf("says %d shown, block renders %d", n, shown))
		}
		cutWant := cut
		if hdrWithheldRE.MatchString(header) {
			n := atoi(hdrWithheldRE, header)
			if n < withheld || n > withheld+demotable {
				problems = append(problems, fmt.Sprintf("says %d withheld, corpus withheld %d (and up to %d more may be policy drops)", n, withheld, demotable))
			} else {
				cutWant = cut - (n - withheld) // rows moved to withheld are not also cut
			}
		} else if withheld > 0 {
			problems = append(problems, fmt.Sprintf("%d rows were withheld by a stage and the header does not name them", withheld))
		}
		switch {
		case hdrRankedOutRE.MatchString(header):
			if n := atoi(hdrRankedOutRE, header); n != cutWant {
				problems = append(problems, fmt.Sprintf("says %d ranked out, ranking cut %d", n, cutWant))
			}
		case hdrNotShownRE.MatchString(header):
			if n := atoi(hdrNotShownRE, header); n != cut {
				problems = append(problems, fmt.Sprintf("says %d not shown (ranked), ranking cut %d and a stage withheld %d", n, cut, withheld))
			}
		}
		if hdrTotalRE.MatchString(header) {
			if n := atoi(hdrTotalRE, header); n != shown+cut+withheld {
				problems = append(problems, fmt.Sprintf("says %d total, shown+cut+withheld is %d", n, shown+cut+withheld))
			}
		}
		if len(problems) > 0 {
			sr.Findings = append(sr.Findings, HeaderFinding{project, bucket.name, strings.TrimSpace(header), strings.Join(problems, "; ")})
		}
	}
}

func atoi(re *regexp.Regexp, s string) int {
	m := re.FindStringSubmatch(s)
	if m == nil {
		return -1
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return -1
	}
	return n
}

// FormatPassive renders the report in the context bench's style: a header naming
// what was measured and against what, then one block of figures per surface, each
// ratio with its denominator.
func FormatPassive(rep PassiveReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "passive context: %d rows, %d projects + _global, clock %s\n",
		rep.Rows, rep.Projects, PassiveInstant().Format(time.RFC3339))
	b.WriteString("  the blocks session start, `ghost context` and ghost_project_context hand a model,\n")
	b.WriteString("  read through the production entry points at their production budgets\n\n")
	for _, s := range rep.Surfaces {
		fmt.Fprintf(&b, "%s (%d blocks)\n", s.Name, s.Blocks)
		fmt.Fprintf(&b, "  withheld leakage       %s   (must be 0)\n", s.Leaked)
		fmt.Fprintf(&b, "  expected-row recall    %s\n", s.Recall)
		fmt.Fprintf(&b, "  cross-project          %s\n", s.Contamination)
		fmt.Fprintf(&b, "  _global share          %s\n", s.GlobalShare)
		fmt.Fprintf(&b, "  budget use             %s   rows rendered / row caps\n", s.BudgetUse)
		fmt.Fprintf(&b, "  budget cut             %s   admissible rows left out\n", s.Cut)
		fmt.Fprintf(&b, "  duplicate rate         %s   rendered beside the row they restate or replace\n", s.Duplicates)
		fmt.Fprintf(&b, "  block size             mean %d B / %d tok, max %d B / %d tok\n",
			s.MeanBytes, (s.MeanBytes+3)/4, s.MaxBytes, (s.MaxBytes+3)/4)
		if s.Header {
			if len(s.Findings) == 0 {
				fmt.Fprintf(&b, "  header honesty         PASS   (%d count lines agree with the rows rendered)\n", s.HeadersChecked)
			} else {
				fmt.Fprintf(&b, "  header honesty         FAIL   (%d of %d count lines disagree with the rows rendered)\n",
					len(s.Findings), s.HeadersChecked)
				for _, f := range s.Findings {
					fmt.Fprintf(&b, "    %s/%s: %s\n      header: %s\n", f.Project, f.Bucket, f.Problem, f.Header)
				}
			}
		} else {
			b.WriteString("  header honesty         n/a    (this surface prints no count line)\n")
		}
		if len(s.LeakedIDs) > 0 {
			fmt.Fprintf(&b, "  LEAKED: %s\n", strings.Join(s.LeakedIDs, ", "))
		}
		if len(s.MissedIDs) > 0 {
			fmt.Fprintf(&b, "  missed expected rows: %d (%s)\n", len(s.MissedIDs), strings.Join(s.MissedIDs, ", "))
		}
		b.WriteString("\n")
	}
	return b.String()
}
