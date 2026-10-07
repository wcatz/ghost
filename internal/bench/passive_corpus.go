package bench

// The passive corpus: a synthetic store that can tell one version of the passive
// context surfaces from another.
//
// `ghost bench --context` cannot. It measures the block ghost_memory_search
// returns, over a graded corpus that holds no resolved row, no expired row, no
// `_global` row and one project, so every filter the passive surfaces apply is a
// no-op on it and its contamination figure is 0.000 whatever the code does. The
// passive surfaces — the session-start block, `ghost context`, and
// ghost_project_context — are a different request shape (no query, a bucket per
// project, a policy per bucket) and a different population (everything a project
// has accumulated, most of it not meant to be shown). This corpus is that
// population, built so each thing a passive block must do has a row that tests it:
//
//   - rows that must be SHOWN (the live, high-importance ones, and a pinned row
//     whose importance and age would otherwise bury it);
//   - rows that must NEVER be shown: resolved, expired (valid_until in 2020) and
//     not yet valid (valid_from in 2099);
//   - rows whose only fault is their scope, which a surface withholds only when it
//     is asked for a scope;
//   - rows the ranking demotes rather than withholds: a superseded row beside the
//     row that replaced it, and a near-duplicate beside its original;
//   - enough low-importance live filler that every budget cuts, because a block
//     that never has to choose says nothing about how it chooses.
//
// Everything is a function of the constants in this file and the clock: ids come
// from corpusID, ages from corpusStamp, and the windows are years from the clock
// on both sides, so the report cannot move because a calendar did.

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// PassiveKind names what a corpus row is FOR. It is the row's role in the fixture,
// not a property the product reads: the product sees columns, and the kind is how
// the fixture remembers which column it wrote and why.
type PassiveKind string

const (
	// KindLive is a current, high-importance row: expected in every block that has
	// room for it, and the corpus keeps each bucket's expected set under that
	// bucket's cap so "has room" is true.
	KindLive PassiveKind = "live"
	// KindPinned is a pinned row with low importance and an old created_at. It is
	// expected, and it is the row a ranking that ignored the pin would bury under
	// the filler.
	KindPinned PassiveKind = "pinned"
	// KindFiller is live, valid and unremarkable: low importance, so it is what a
	// budget cut removes. It is optional — showing it is not wrong, only
	// unnecessary.
	KindFiller PassiveKind = "filler"
	// KindResolved is a row `ghost resolve` withdrew (resolved_at set). Must not
	// appear anywhere.
	KindResolved PassiveKind = "resolved"
	// KindExpired is a row whose valid_until is in 2020. Must not appear.
	KindExpired PassiveKind = "expired"
	// KindFuture is a row whose valid_from is in 2099. Must not appear.
	KindFuture PassiveKind = "future"
	// KindScoped is a live row scoped to the staging environment. It must not
	// appear in a block that was asked for the production scope, and it is
	// optional in one that was asked for none: an unscoped request matches every
	// row, which is the documented rule rather than a leak.
	KindScoped PassiveKind = "out_of_scope"
	// KindSuperseded is an old row with a `supersedes` edge from a newer live row.
	// Optional: the project bucket REORDERS it behind the row that replaced it and
	// leaves the drop to the cap, so seeing it in a block with room is the
	// documented behaviour. The duplicate rate is what measures it.
	KindSuperseded PassiveKind = "superseded"
	// KindDuplicate restates a live row and carries a `duplicate` edge to it.
	// Optional for the same reason, and measured by the same rate.
	KindDuplicate PassiveKind = "duplicate"
)

// PassiveGrade is what a block is allowed to do with a row.
type PassiveGrade string

const (
	// GradeExpected: the row belongs in the block, and a block without it has lost
	// something it was asked for.
	GradeExpected PassiveGrade = "expected"
	// GradeWithheld: the row must not be rendered. Rendering it is a leak, and the
	// leak figure is a count of these.
	GradeWithheld PassiveGrade = "withheld"
	// GradeOptional: neither. Showing it or cutting it is a judgement the report
	// does not make.
	GradeOptional PassiveGrade = "optional"
)

// PassiveRow is one corpus row.
type PassiveRow struct {
	Project  string
	Key      string
	Kind     PassiveKind
	Category string
	Content  string
	// Importance is the row's stored importance, 0..1.
	Importance float32
	// AgeDays is how long before the corpus instant the row was created.
	AgeDays int
	Pinned  bool
	// Resolved marks a row withdrawn by `ghost resolve`.
	Resolved bool
	// ValidFrom and ValidUntil are in memory.StoredStampLayout, because the layout
	// is the whole reason a window is read at all: an RFC 3339 value is unreadable
	// and an unreadable stamp is kept as unset, so a fixture of them would show no
	// leak for the wrong reason. NewPassiveCorpus asserts every one parses.
	ValidFrom, ValidUntil string
	Scope                 map[string]string
	// Supersedes is the key of the older row in this project this row replaced.
	Supersedes string
	// DuplicateOf is the key of the row this one restates.
	DuplicateOf string
}

// ID is the id the row is stored under.
func (r PassiveRow) ID() string { return corpusID(r.Project, r.Key) }

// Grade is what a block read under the given scope may do with the row.
//
// scoped says whether the read carried the production scope. It is a parameter and
// not a property of the row because the same row is a leak on one surface and
// correct on another: the session-start block honours `injection.session_scope`
// (empty by default), and ghost_project_context carries no scope at all.
func (r PassiveRow) Grade(scoped bool) PassiveGrade {
	switch r.Kind {
	case KindResolved, KindExpired, KindFuture:
		return GradeWithheld
	case KindScoped:
		if scoped {
			return GradeWithheld
		}
		return GradeOptional
	case KindLive, KindPinned:
		return GradeExpected
	default:
		return GradeOptional
	}
}

// eligible reports whether a read at the corpus instant could legitimately admit
// the row: it is neither resolved, outside its window, nor out of the read's
// scope. It is the fixture's own statement of which rows are in play, derived from
// the row's kind — never from the product's verdicts — so the report's ground truth
// cannot be the thing under test.
func (r PassiveRow) eligible(scoped bool) bool {
	return r.Grade(scoped) != GradeWithheld
}

// withheldByAStage reports whether the row is unresolved and was withheld by a
// stage (validity, or scope when the read carries one) rather than by the SQL's
// own resolved_at predicate. It is the population a header's "withheld" count is
// about, as opposed to a resolved row, which the window never contained.
func (r PassiveRow) withheldByAStage(scoped bool) bool {
	return !r.Resolved && r.Grade(scoped) == GradeWithheld
}

// PassiveProjects are the projects the corpus holds, besides `_global`. Three,
// because contamination is a statement about OTHER projects' rows and needs more
// than one other to be a population.
//
// The fourth, `delta`, is small on purpose: everything it holds fits under every
// cap, so a block read for it never has to choose. That is the one place a
// near-duplicate or a superseded row is SHOWN beside the row it restates (the
// project bucket reorders them and leaves the drop to the cap), and so the one
// place the duplicate rate can be non-zero. It is also the project a cap-
// shaped bug cannot hide in.
var PassiveProjects = []string{"alpha", "beta", "gamma", "delta"}

// PassiveGlobal is the cross-project bucket's id.
const PassiveGlobal = memory.GlobalProjectID

// The window stamps. Years from any clock the bench uses, on both sides, for the
// reason ContextInstant gives: a boundary near the clock is a published table one
// corpus edit from changing for a reason that is not retrieval.
const (
	passiveLongAgo = "2019-01-01 00:00:00"
	passiveClosed  = "2020-01-01 00:00:00"
	passiveOpened  = "2025-01-01 00:00:00"
	passiveOpens   = "2099-01-01 00:00:00"
)

// PassiveScope is the scope a scoped read is asked for, and the staging scope the
// out-of-scope rows carry. They name one key with two values on purpose: a row
// that states a DIFFERENT value for a key the request states is the contradiction
// the scope rule withholds, whereas a row that states no scope at all is general
// knowledge and always matches.
var (
	PassiveScope        = map[string]string{"env": "production"}
	passiveStagingScope = map[string]string{"env": "staging"}
)

// PassiveCorpus is the whole fixture.
type PassiveCorpus struct {
	Rows []PassiveRow
}

// NewPassiveCorpus builds the corpus. It is deterministic and takes no input; the
// error is for a fixture that contradicts itself (an unreadable stamp, a
// duplicate key, an edge naming a row that does not exist), which is a bug in
// this file and is reported rather than seeded.
func NewPassiveCorpus() (PassiveCorpus, error) {
	var c PassiveCorpus
	for _, p := range PassiveProjects {
		c.Rows = append(c.Rows, projectRows(p)...)
	}
	c.Rows = append(c.Rows, globalRows()...)
	if err := c.validate(); err != nil {
		return PassiveCorpus{}, err
	}
	return c, nil
}

// projectThemes give each project its own vocabulary, so a row that shows up in
// the wrong block is recognisably another project's and not merely another row.
var projectThemes = map[string][]string{
	"alpha": {"ingest queue", "schema registry", "retry budget", "batch window", "dead letter topic",
		"offset commit", "partition count", "backpressure", "replay tooling", "consumer lag"},
	"beta": {"invoice rounding", "tax table", "ledger close", "refund flow", "dunning schedule",
		"proration rule", "currency pin", "audit export", "settlement cutoff", "credit note"},
	"gamma": {"token lifetime", "key rotation", "session cookie", "rate limiter", "audit trail",
		"scope grammar", "consent screen", "revocation list", "jwks cache", "login throttle"},
	"delta": {"cache warmup", "feature flag", "release train", "log sampling", "health probe",
		"config reload", "canary split", "rollback window", "build matrix", "pager rota"},
}

// projectShape is how many rows of each kind a project holds. The three large
// projects share one shape; delta's is chosen so that its eligible rows fit under
// every cap.
type projectShape struct {
	live, supers, dups, filler, resolved, expired, future, staged int
	// replaceFrom is the live row that replaces old-00; the next supers-1 live
	// rows replace the following olds. dupFrom is the first live row duplicated.
	replaceFrom, dupFrom int
}

func shapeOf(p string) projectShape {
	if p == "delta" {
		return projectShape{live: 5, supers: 2, dups: 2, filler: 0, resolved: 1, expired: 1, future: 1, staged: 1, replaceFrom: 0, dupFrom: 2}
	}
	return projectShape{live: 10, supers: 3, dups: 3, filler: 30, resolved: 3, expired: 3, future: 2, staged: 2, replaceFrom: 5, dupFrom: 0}
}

// categories rotate over the behavioural ones first, because those are the
// categories the session-start policy's behaviour floor reserves slots for, and a
// corpus with one category would leave that reservation unexercised.
var passiveCategories = []string{"gotcha", "convention", "decision", "preference", "architecture", "pattern", "dependency", "fact"}

func projectRows(p string) []PassiveRow {
	themes := projectThemes[p]
	sh := shapeOf(p)
	var rows []PassiveRow
	cat := func(i int) string { return passiveCategories[i%len(passiveCategories)] }

	// Ten live rows, importance 0.95 down to 0.50, newest first. Two of them carry
	// the production scope and two an open window that contains the clock, so a
	// block under the production scope still has them and the validity reader sees
	// `valid` as well as `unset`.
	for i := 0; i < sh.live; i++ {
		r := PassiveRow{
			Project: p, Key: fmt.Sprintf("live-%02d", i), Kind: KindLive, Category: cat(i),
			Content:    fmt.Sprintf("%s: the %s rule is settled and current (live %02d)", p, themes[i], i),
			Importance: 0.95 - 0.05*float32(i), AgeDays: 2 + i,
		}
		switch i {
		case 1, 2:
			r.Scope = PassiveScope
		case 3, 4:
			r.ValidFrom = passiveOpened
		}
		rows = append(rows, r)
	}

	// The pinned row: low importance, old. A pin is the one signal the caller gave
	// the ranking directly, so the block must carry it.
	rows = append(rows, PassiveRow{
		Project: p, Key: "pinned-00", Kind: KindPinned, Category: "convention",
		Content:    fmt.Sprintf("%s: never ship without the %s checklist (pinned)", p, themes[0]),
		Importance: 0.30, AgeDays: 400, Pinned: true,
	})

	// Superseded rows, each replaced by a live row, and near-duplicates of live
	// rows (see projectShape for which).
	// The old and the duplicate carry high importance on purpose: a ranking that
	// forgot the edge would put them beside the rows they restate.
	for i := 0; i < sh.supers; i++ {
		rows = append(rows, PassiveRow{
			Project: p, Key: fmt.Sprintf("old-%02d", i), Kind: KindSuperseded, Category: cat(i + 5),
			Content:    fmt.Sprintf("%s: the %s rule as it stood before the change (superseded %02d)", p, themes[sh.replaceFrom+i], i),
			Importance: 0.90, AgeDays: 60 + i,
		})
	}
	for i := 0; i < sh.dups; i++ {
		of := sh.dupFrom + i
		rows = append(rows, PassiveRow{
			Project: p, Key: fmt.Sprintf("dup-%02d", i), Kind: KindDuplicate, Category: cat(of),
			Content:    fmt.Sprintf("%s: the %s rule is settled and current (live %02d), restated", p, themes[of], of),
			Importance: 0.80, AgeDays: 20 + i, DuplicateOf: fmt.Sprintf("live-%02d", of),
		})
	}

	// Thirty live, valid, unremarkable rows: the budget cut.
	for i := 0; i < sh.filler; i++ {
		rows = append(rows, PassiveRow{
			Project: p, Key: fmt.Sprintf("filler-%02d", i), Kind: KindFiller, Category: cat(i + 3),
			Content:    fmt.Sprintf("%s: minor note %02d about the %s", p, i, themes[i%len(themes)]),
			Importance: 0.10 + 0.005*float32(i), AgeDays: 30 + 5*i,
		})
	}

	// What must not appear. All of it carries the HIGHEST importance and the NEWEST
	// created_at in the project, so a filter that stopped working would put these
	// rows at the top of the block rather than somewhere a cap would hide them.
	for i := 0; i < sh.resolved; i++ {
		rows = append(rows, PassiveRow{
			Project: p, Key: fmt.Sprintf("resolved-%02d", i), Kind: KindResolved, Category: "decision",
			Content:    fmt.Sprintf("%s: the %s question was withdrawn and must not be shown (resolved %02d)", p, themes[i], i),
			Importance: 0.99, AgeDays: 1, Resolved: true,
		})
	}
	for i := 0; i < sh.expired; i++ {
		rows = append(rows, PassiveRow{
			Project: p, Key: fmt.Sprintf("expired-%02d", i), Kind: KindExpired, Category: "fact",
			Content:    fmt.Sprintf("%s: the %s claim stopped being true in 2020 (expired %02d)", p, themes[i], i),
			Importance: 0.99, AgeDays: 1, ValidFrom: passiveLongAgo, ValidUntil: passiveClosed,
		})
	}
	for i := 0; i < sh.future; i++ {
		rows = append(rows, PassiveRow{
			Project: p, Key: fmt.Sprintf("future-%02d", i), Kind: KindFuture, Category: "fact",
			Content:    fmt.Sprintf("%s: the %s claim does not hold until 2099 (future %02d)", p, themes[i], i),
			Importance: 0.98, AgeDays: 1, ValidFrom: passiveOpens,
		})
	}
	for i := 0; i < sh.staged; i++ {
		rows = append(rows, PassiveRow{
			Project: p, Key: fmt.Sprintf("staging-%02d", i), Kind: KindScoped, Category: "gotcha",
			Content:    fmt.Sprintf("%s: the %s override applies to staging only (out of scope %02d)", p, themes[i], i),
			Importance: 0.97, AgeDays: 1, Scope: passiveStagingScope,
		})
	}

	// The edges, named by key and resolved by validate. The supersedes edge points
	// from the NEWER row to the older.
	for i := 0; i < sh.supers; i++ {
		for j := range rows {
			if rows[j].Key == fmt.Sprintf("live-%02d", sh.replaceFrom+i) {
				rows[j].Supersedes = fmt.Sprintf("old-%02d", i)
			}
		}
	}
	return rows
}

func globalRows() []PassiveRow {
	g := PassiveGlobal
	var rows []PassiveRow
	prefs := []string{"commit messages stay short and factual", "every change gets a test that fails without it",
		"no secrets in a repository", "prefer a surgical edit to a rewrite", "state the scope before editing",
		"never force-push a shared branch"}
	for i, text := range prefs {
		rows = append(rows, PassiveRow{
			Project: g, Key: fmt.Sprintf("live-%02d", i), Kind: KindLive, Category: passiveCategories[(i+1)%len(passiveCategories)],
			Content:    "all projects: " + text + fmt.Sprintf(" (global %02d)", i),
			Importance: 0.95 - 0.05*float32(i), AgeDays: 5 + i,
		})
	}
	rows = append(rows, PassiveRow{
		Project: g, Key: "pinned-00", Kind: KindPinned, Category: "preference",
		Content:    "all projects: ask before deleting anything (global pinned)",
		Importance: 0.30, AgeDays: 400, Pinned: true,
	})
	rows = append(rows, PassiveRow{
		Project: g, Key: "old-00", Kind: KindSuperseded, Category: "preference",
		Content:    "all projects: commit messages may run long (global, superseded)",
		Importance: 0.90, AgeDays: 90,
	})
	rows = append(rows, PassiveRow{
		Project: g, Key: "dup-00", Kind: KindDuplicate, Category: "preference",
		Content:    "all projects: " + prefs[0] + " (global 00), restated",
		Importance: 0.80, AgeDays: 30, DuplicateOf: "live-00",
	})
	for i := 0; i < 12; i++ {
		rows = append(rows, PassiveRow{
			Project: g, Key: fmt.Sprintf("filler-%02d", i), Kind: KindFiller, Category: "fact",
			Content:    fmt.Sprintf("all projects: minor cross-project note %02d", i),
			Importance: 0.10 + 0.01*float32(i), AgeDays: 40 + 7*i,
		})
	}
	for i := 0; i < 2; i++ {
		rows = append(rows, PassiveRow{
			Project: g, Key: fmt.Sprintf("resolved-%02d", i), Kind: KindResolved, Category: "preference",
			Content:    fmt.Sprintf("all projects: a withdrawn cross-project preference (global resolved %02d)", i),
			Importance: 0.99, AgeDays: 1, Resolved: true,
		})
		rows = append(rows, PassiveRow{
			Project: g, Key: fmt.Sprintf("expired-%02d", i), Kind: KindExpired, Category: "fact",
			Content:    fmt.Sprintf("all projects: a cross-project claim that lapsed in 2020 (global expired %02d)", i),
			Importance: 0.99, AgeDays: 1, ValidFrom: passiveLongAgo, ValidUntil: passiveClosed,
		})
	}
	rows = append(rows, PassiveRow{
		Project: g, Key: "future-00", Kind: KindFuture, Category: "fact",
		Content:    "all projects: a cross-project claim that begins in 2099 (global future)",
		Importance: 0.98, AgeDays: 1, ValidFrom: passiveOpens,
	})
	rows = append(rows, PassiveRow{
		Project: g, Key: "staging-00", Kind: KindScoped, Category: "gotcha",
		Content:    "all projects: a cross-project override for staging only (global out of scope)",
		Importance: 0.97, AgeDays: 1, Scope: passiveStagingScope,
	})
	for i := range rows {
		if rows[i].Key == "live-04" {
			rows[i].Supersedes = "old-00"
		}
	}
	return rows
}

// validate refuses a fixture that contradicts itself. Every stamp must be one
// memory.ParseStamp reads — the failure it prevents is silent and looks like a
// pass — every key must be unique within its project, and every edge must name a
// row that exists.
func (c PassiveCorpus) validate() error {
	seen := make(map[string]PassiveRow, len(c.Rows))
	for _, r := range c.Rows {
		if r.Key == "" || r.Project == "" {
			return fmt.Errorf("passive corpus row with an empty key or project: %+v", r)
		}
		if _, dup := seen[r.ID()]; dup {
			return fmt.Errorf("passive corpus: duplicate row %s", r.ID())
		}
		seen[r.ID()] = r
		for _, stamp := range []string{r.ValidFrom, r.ValidUntil} {
			if stamp == "" {
				continue
			}
			if _, ok := memory.ParseStamp(stamp); !ok {
				return fmt.Errorf("passive corpus row %s: stamp %q is not one memory.ParseStamp reads, so the row would be kept as unset and the corpus would measure nothing", r.ID(), stamp)
			}
		}
	}
	for _, r := range c.Rows {
		for _, ref := range []string{r.Supersedes, r.DuplicateOf} {
			if ref == "" {
				continue
			}
			if _, ok := seen[corpusID(r.Project, ref)]; !ok {
				return fmt.Errorf("passive corpus row %s names %q, which is not a row of project %s", r.ID(), ref, r.Project)
			}
		}
	}
	return nil
}

// ByID indexes the corpus by stored id.
func (c PassiveCorpus) ByID() map[string]PassiveRow {
	m := make(map[string]PassiveRow, len(c.Rows))
	for _, r := range c.Rows {
		m[r.ID()] = r
	}
	return m
}

// PassiveBlind names a mutation of the SEEDED store that makes one product filter
// unable to see what it filters. It exists for the mutation check: the report is
// graded against the corpus as designed, and a store seeded under a blindness is
// the store a product with that filter disabled would behave as if it held.
type PassiveBlind string

const (
	// BlindNone is the corpus as designed.
	BlindNone PassiveBlind = ""
	// BlindValidity seeds every row with no validity window, so the validity
	// stage reads each as unset and keeps it — the observable behaviour of a
	// pipeline whose validity filter was removed.
	BlindValidity PassiveBlind = "validity"
	// BlindResolved seeds every row with resolved_at NULL, the observable
	// behaviour of a window whose resolved predicate was removed.
	BlindResolved PassiveBlind = "resolved"
)

// SeedPassive writes the corpus into a store built on db, stamped at the given
// instant, and returns that instant truncated to the second (see SeedAt, which
// returns it for the same reason).
func SeedPassive(ctx context.Context, store *memory.Store, db *sql.DB, c PassiveCorpus, at time.Time, blind PassiveBlind) (time.Time, error) {
	stamp := newCorpusStampAt(at)
	projects := append(append([]string{}, PassiveProjects...), PassiveGlobal)
	for _, p := range projects {
		if p == PassiveGlobal {
			// `_global` is created by the schema; EnsureProject is idempotent and
			// states it anyway, so a store that did not would fail here and not
			// later as a missing bucket.
			if err := store.EnsureProject(ctx, p, p, "global"); err != nil {
				return stamp.instant(), fmt.Errorf("ensure project %s: %w", p, err)
			}
			continue
		}
		if err := store.EnsureProject(ctx, p, "/bench/passive/"+p, p); err != nil {
			return stamp.instant(), fmt.Errorf("ensure project %s: %w", p, err)
		}
	}
	for _, r := range c.Rows {
		m := memory.Memory{
			Category: r.Category, Content: r.Content, Importance: r.Importance,
			Source: "mcp", Scope: r.Scope,
		}
		if blind != BlindValidity {
			if r.ValidFrom != "" {
				v := r.ValidFrom
				m.ValidFrom = &v
			}
			if r.ValidUntil != "" {
				v := r.ValidUntil
				m.ValidUntil = &v
			}
		}
		id, err := store.CreateWithIDFromCorpus(ctx, r.Project, r.ID(), m)
		if err != nil {
			return stamp.instant(), fmt.Errorf("create %s: %w", r.ID(), err)
		}
		if err := stamp.apply(ctx, db, id, r.AgeDays); err != nil {
			return stamp.instant(), fmt.Errorf("stamp %s: %w", r.ID(), err)
		}
		// Pinned and resolved are not fields the corpus writer takes — a pin and a
		// withdrawal are acts on an existing row — so they are written the way the
		// product writes them, as a column on the row that already exists.
		if r.Pinned {
			if _, err := db.ExecContext(ctx, `UPDATE memories SET pinned = 1 WHERE id = ?`, id); err != nil {
				return stamp.instant(), fmt.Errorf("pin %s: %w", r.ID(), err)
			}
		}
		if r.Resolved && blind != BlindResolved {
			if _, err := db.ExecContext(ctx, `UPDATE memories SET resolved_at = created_at WHERE id = ?`, id); err != nil {
				return stamp.instant(), fmt.Errorf("resolve %s: %w", r.ID(), err)
			}
		}
	}
	// The edges, after every row exists. A supersedes edge points from the newer
	// row to the older, and a duplicate edge from the copy to the original, which
	// is the direction the demotion's rank rule expects the loser to be the
	// lower-ranked end of.
	for _, r := range c.Rows {
		if r.Supersedes != "" {
			if err := store.CreateLink(ctx, r.ID(), corpusID(r.Project, r.Supersedes), "supersedes", 1.0, "llm"); err != nil {
				return stamp.instant(), fmt.Errorf("link %s supersedes %s: %w", r.ID(), r.Supersedes, err)
			}
		}
		if r.DuplicateOf != "" {
			if err := store.CreateLink(ctx, r.ID(), corpusID(r.Project, r.DuplicateOf), "duplicate", 1.0, "llm"); err != nil {
				return stamp.instant(), fmt.Errorf("link %s duplicates %s: %w", r.ID(), r.DuplicateOf, err)
			}
		}
	}
	return stamp.instant(), nil
}
