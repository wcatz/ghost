package mcpinit

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"slices"
	"time"

	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/memory"
)

const (
	sessionDisplayBytes = memory.SessionProjectDisplayBytes
	globalsDisplayBytes = memory.SessionGlobalDisplayBytes
)

const sessionStartByteCap = 9000

const sessionHostLimit = 10000

// A compact handoff is working state, not the block itself: it gets at most a
// third of the session-start cap so the ranked block still has room even when
// the handoff is long. The two together fit the host's output limit.
const handoffByteCap = sessionStartByteCap / 3

const (
	sessionLearnedBytes = 1000
	sessionTitleBytes   = 120
)

type sessionFrame struct {
	project          string
	learned          string
	tasks            [][4]string
	decisions        [][4]string
	interactionCount int
	covered          func(memory.Candidate) bool
	// budgetReserve is the byte length of the handoff a compact session-start
	// puts ahead of this block. The block is fitted to the same cap minus the
	// handoff, so the two together still fit the host's output limit.
	budgetReserve int
}

func sessionGlobalReserve() map[string]int {
	return map[string]int{memory.GlobalProjectID: 1}
}

func sessionGlobalPinnedReserve() map[string]int {
	return map[string]int{memory.GlobalProjectID: globalsCap}
}

// sessionRuleReserve and globalsRuleReserve are the slots standing rules own
// inside assembly selection, so a descriptive fact cannot take a rule's slot.
// The rest of each cap stays open to facts, which keeps the block bounded.
const (
	sessionRuleReserve = sessionMemoriesCap * 2 / 3
	globalsRuleReserve = globalsCap * 3 / 4
)

func sessionRuleBehaviorCategories() []string {
	return []string{"gotcha", "convention", "preference"}
}

// The session-start slice keeps its own rule categories rather than
// injection.behavior_categories: the reserve is for standing rules, and
// decisions are rendered in their own group.
//
// sessionProjectFloor honours injection.behavior_floor: 0 turns the bias off,
// any other value is raised to the rule reserve and held under the slice cap.
// injection.category_caps still limits each category inside that floor pass.
func sessionProjectFloor(cfgFloor int) int {
	if cfgFloor <= 0 {
		return 0
	}
	return min(max(cfgFloor, sessionRuleReserve), sessionMemoriesCap)
}

// sessionCategoryCaps keeps injection.category_caps for every category except
// the standing-rule ones. The reserve exists so rules are not crowded out, and
// the default cap of 4 on gotcha would cut it; the caps still bound a
// configured non-rule behavior category such as decision.
func sessionCategoryCaps(caps map[string]int) map[string]int {
	if len(caps) == 0 {
		return nil
	}
	rule := map[string]bool{}
	for _, c := range sessionRuleBehaviorCategories() {
		rule[c] = true
	}
	out := make(map[string]int, len(caps))
	for c, n := range caps {
		if !rule[c] {
			out[c] = n
		}
	}
	return out
}

func sessionPassiveBudget(cfg *config.Config, projectID string) assemble.Budget {
	inj := cfg.Injection

	slices := make([]assemble.Slice, 0, 2)

	if projectID != "" {
		floor := sessionProjectFloor(inj.BehaviorFloor)
		slices = append(slices, assemble.Slice{
			Bucket: projectID, MaxItems: sessionMemoriesCap,
			OverFetch: sessionMemoriesCap * 3,
			Order:     memory.OrderDecay, TwoPass: floor > 0,
			BehaviorFloor: floor, BehaviorCategories: sessionRuleBehaviorCategories(),
			CategoryWeights: inj.CategoryWeights, CategoryCaps: sessionCategoryCaps(inj.CategoryCaps),
			DemotionThreshold:     cfg.Linking.DemotionThreshold,
			DemoteOnlyWhenOverCap: true,
		})
	}
	slices = append(slices, assemble.Slice{
		Bucket: memory.GlobalProjectID, MaxItems: globalsCap,
		OverFetch: globalsCap * 2,
		Order:     memory.OrderPinnedImportanceUpdated, TwoPass: true,
		BehaviorFloor: globalsRuleReserve, BehaviorCategories: sessionRuleBehaviorCategories(),
		DemotionThreshold: globalsDemotionThreshold,
		DropDemotedLosers: true,
	})
	return assemble.Budget{
		Slices:                 slices,
		ReserveByProject:       sessionGlobalReserve(),
		ReservePinnedByProject: sessionGlobalPinnedReserve(),
	}
}

type sessionTally struct {
	project      assemble.BucketTally
	globals      assemble.BucketTally
	emptyNote    string
	asOfWithheld int
}

func sessionRowsFrom(items []assemble.Item) (memories, globals []sessionMemory) {
	for _, it := range items {
		row := sessionMemory{
			ID:              it.ID,
			Category:        it.Category,
			Content:         it.Content,
			Tags:            it.Tags,
			Importance:      it.Importance,
			Pinned:          it.Pinned,
			CreatedAt:       it.CreatedAt,
			Scope:           it.Scope,
			ProjectID:       it.ProjectID,
			Source:          it.Source,
			ResolvedAt:      it.ResolvedAt,
			ValidFrom:       it.ValidFrom,
			ValidUntil:      it.ValidUntil,
			VerifiedAt:      it.VerifiedAt,
			ValidityState:   it.ValidityState,
			Confidence:      it.Confidence,
			Agent:           it.Agent,
			SourceRef:       it.SourceRef,
			ConflictsWith:   it.ConflictsWith,
			SupersededBy:    it.SupersededBy,
			ContradictedBy:  it.ContradictedBy,
			MaybeReplacedBy: it.MaybeReplacedBy,
		}
		if it.ProjectID == memory.GlobalProjectID {
			row.Content = truncateUTF8(row.Content, globalsDisplayBytes)
			globals = append(globals, row)
			continue
		}
		row.Content = truncateUTF8(row.Content, sessionDisplayBytes)
		memories = append(memories, row)
	}
	memories = sessionRulesFirst(memories)
	globals = sessionRulesFirst(globals)
	return memories, globals
}

func isSessionRuleCategoryName(cat string) bool {
	return slices.Contains(sessionRuleBehaviorCategories(), cat)
}

func isSessionRule(m sessionMemory) bool {
	return m.Pinned || isSessionRuleCategoryName(m.Category)
}

func isSessionRuleItem(it assemble.Item) bool {
	return it.Pinned || isSessionRuleCategoryName(it.Category)
}

// sessionRulesFirst is a stable partition: pinned rows, then unpinned rows of a
// rule category, then facts, each group keeping its rank order.
func sessionRulesFirst(rows []sessionMemory) []sessionMemory {
	var pinned, rules, facts []sessionMemory
	for _, m := range rows {
		switch {
		case m.Pinned:
			pinned = append(pinned, m)
		case isSessionRule(m):
			rules = append(rules, m)
		default:
			facts = append(facts, m)
		}
	}
	out := make([]sessionMemory, 0, len(rows))
	out = append(out, pinned...)
	out = append(out, rules...)
	out = append(out, facts...)
	return out
}

func foldCount(t assemble.BucketTally, n, excluded int, capped bool, overFetch int) assemble.BucketTally {
	if capped {
		return t.CountedAtLeast(n, excluded, overFetch)
	}
	return t.CountedAgainst(n, excluded, overFetch)
}

func loadSessionPassive(ctx context.Context, store *memory.Store, cfg *config.Config, projectID string, now time.Time, record assemble.RecordSink, sessionID string, frames ...sessionFrame) (memories, globals []sessionMemory, tally sessionTally) {
	var frame sessionFrame
	if len(frames) > 0 {
		frame = frames[0]
	}
	budget := sessionPassiveBudget(cfg, projectID)

	type eligible struct {
		n, excluded int
		capped      bool
	}
	counts := make(map[string]eligible, len(budget.Slices))
	for _, sl := range budget.Slices {
		n, excluded, capped, err := store.PassiveEligibleCount(ctx, memory.SlicePolicy{Bucket: sl.Bucket, IncludeGlobal: sl.IncludeGlobal}, now, cfg.Injection.SessionScope)
		if err != nil {
			slog.Warn("ghost: session-start eligible count failed, so the header counts the retrieval window", "bucket", sl.Bucket, "error", err)
			continue
		}
		counts[sl.Bucket] = eligible{n, excluded, capped}
	}
	tallyFor := func(trace *assemble.Trace, nProject, nGlobals int) sessionTally {
		t := sessionTally{
			project: assemble.CountsFor(trace, projectID, nProject),
			globals: assemble.CountsFor(trace, memory.GlobalProjectID, nGlobals),
		}
		for _, sl := range budget.Slices {
			c, ok := counts[sl.Bucket]
			if !ok {
				continue
			}
			switch sl.Bucket {
			case projectID:
				t.project = foldCount(t.project, c.n, c.excluded, c.capped, sl.OverFetch+windowExtra(trace, sl.Bucket))
			case memory.GlobalProjectID:
				t.globals = foldCount(t.globals, c.n, c.excluded, c.capped, sl.OverFetch+windowExtra(trace, sl.Bucket))
			}
		}
		return t
	}
	budget.MaxBytes = sessionStartByteCap - frame.budgetReserve
	budget.KeepPinned = true
	budget.ReserveByProject = sessionGlobalReserve()
	budget.ReservePinnedByProject = sessionGlobalPinnedReserve()
	budget.FramingCeiling = sessionHostLimit
	budget.Measure = func(items []assemble.Item, trace *assemble.Trace) int {
		mem, glob := sessionRowsFrom(items)
		return len(formatSessionContext(projectID, frame.project, nil, mem, frame.learned, frame.tasks, frame.decisions, frame.interactionCount, glob, tallyFor(trace, len(mem), len(glob))))
	}
	res, err := assemble.Run(ctx, store, assemble.Request{
		ProjectID: projectID,
		Query:     "",
		Source:    assemble.SourceSessionStart,
		Condition: assemble.CondHybrid,
		Now:       now,
		Scope:     cfg.Injection.SessionScope,
		Budget:    budget,
		Record:    record,
		Covered:   frame.covered,
		SessionID: sessionID,
		Logger:    sessionLog(),
	})
	if err != nil {
		slog.Warn("ghost: session-start assembly failed, so the block has no memory rows", "error", err)
		return nil, nil, sessionTally{}
	}
	memories, globals = sessionRowsFrom(res.Items)
	tally = tallyFor(res.Trace, len(memories), len(globals))
	if tally.globals.Total() == 0 && tally.project.WithheldNote() != "" {
		tally.emptyNote = assemble.EmptyNote(res)
	}
	return memories, globals, tally
}

func sessionStore(db *sql.DB) *memory.Store {
	return memory.NewStoreWithRead(db, db, sessionLog())
}

func sessionLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelWarn,
	}))
}

// openWritableStore is the one rw-store opener for hook writes: a missing
// database is never created by a write meant to describe it, and the data
// directory's permissions are tightened on both sides of the open. The record
// sink and the handoff writer both use it rather than repeating that shape.
func openWritableStore(dbPath string) (*memory.Store, func(), error) {
	if _, err := os.Stat(dbPath); err != nil {
		return nil, func() {}, err
	}
	memory.TightenPermissions(dbPath)
	db, err := sql.Open("sqlite", rwDSN(dbPath))
	if err != nil {
		return nil, func() {}, err
	}
	return sessionStore(db), func() {
		_ = db.Close()
		memory.TightenPermissions(dbPath)
	}, nil
}

func sessionRecordSink(dbPath string) (assemble.RecordSink, func()) {
	s, closeStore, _ := openWritableStore(dbPath)
	if s == nil {
		return nil, closeStore
	}
	return s, closeStore
}

func windowExtra(trace *assemble.Trace, bucket string) int {
	if trace == nil {
		return 0
	}
	return trace.WindowExtra[bucket]
}
