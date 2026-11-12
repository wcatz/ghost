package mcpinit

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
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
}

func sessionGlobalReserve() map[string]int {
	return map[string]int{memory.GlobalProjectID: 1}
}

func sessionGlobalPinnedReserve() map[string]int {
	return map[string]int{memory.GlobalProjectID: globalsCap}
}

func sessionPassiveBudget(cfg *config.Config, projectID string) assemble.Budget {
	inj := cfg.Injection

	slices := make([]assemble.Slice, 0, 2)

	if projectID != "" {
		slices = append(slices, assemble.Slice{
			Bucket: projectID, MaxItems: sessionMemoriesCap,
			OverFetch: sessionMemoriesCap * 3,
			Order:     memory.OrderDecay, TwoPass: inj.BehaviorFloor > 0,
			BehaviorFloor: inj.BehaviorFloor, BehaviorCategories: inj.BehaviorCategories,
			CategoryWeights: inj.CategoryWeights, CategoryCaps: inj.CategoryCaps,
			DemotionThreshold:     cfg.Linking.DemotionThreshold,
			DemoteOnlyWhenOverCap: true,
		})
	}
	slices = append(slices, assemble.Slice{
		Bucket: memory.GlobalProjectID, MaxItems: globalsCap,
		OverFetch:         globalsCap * 2,
		Order:             memory.OrderPinnedImportanceUpdated,
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
	return memories, globals
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
	budget.MaxBytes = sessionStartByteCap
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

func sessionRecordSink(dbPath string) (assemble.RecordSink, func()) {
	if _, err := os.Stat(dbPath); err != nil {
		return nil, func() {}
	}
	memory.TightenPermissions(dbPath)
	db, err := sql.Open("sqlite", rwDSN(dbPath))
	if err != nil {
		return nil, func() {}
	}
	return sessionStore(db), func() {
		_ = db.Close()
		memory.TightenPermissions(dbPath)
	}
}

func windowExtra(trace *assemble.Trace, bucket string) int {
	if trace == nil {
		return 0
	}
	return trace.WindowExtra[bucket]
}
