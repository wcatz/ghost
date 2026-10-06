package memory

// #648 slice 2: an agent's own negative evidence about ONE memory.
//
// A flag is a CLAIM somebody made, not a verdict the store reached, and every
// design decision below follows from that difference. It is APPEND-ONLY (a
// later agent cannot retract an earlier agent's objection by flagging the same
// row again), ATTRIBUTED (which agent and which session said it), and STAMPED
// with the hash of the content it was about — the same rule
// retrieval_audit.content_hash follows (#879, #884), for the same reason:
// Ghost rewrites content under a stable id, and a flag on text that is gone is
// a claim about words nobody can read.
//
// What it must NOT do is act. Nothing here resolves, deletes, demotes or
// re-ranks — the flag reaches resolve and reflect as one COUNT beside the
// memory, and the classifier decides. That is why the reason text never leaves
// the store: it is free text an agent wrote, it would land in a prompt sent to
// a third-party model the moment it was rendered, and its whole value is being
// read by a human in the table later. Only counts and ids cross this seam.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// The two kinds. A CLOSED vocabulary rather than free text is the point: "wrong"
// and "stale" are the two things an agent can say, and a third spelling would be
// a claim no reader knows how to weigh. The table's CHECK mirrors these, so a
// raw INSERT (an import, a future writer) cannot file a claim outside the
// vocabulary either.
const (
	FlagKindWrong = "wrong"
	FlagKindStale = "stale"
)

// FlagReasonMax bounds the reason in RUNES, not bytes.
//
// It counts runes because the bound is about how much TEXT a human will read
// back, and a byte bound would refuse an accented reason the contract allows
// while admitting a shorter one made of wider characters. At the bound is
// accepted; past it is REFUSED rather than clamped, because a clamped reason is
// a truncated claim that still reads as the agent's own words — the operator
// would be reading an objection nobody actually made in that form.
const FlagReasonMax = 500

// The four refusals, as sentinels rather than one generic error: the caller has
// a different fix available for each. An unknown id means the agent guessed;
// another project means it guessed about ownership; a bad kind means the tool's
// contract was ignored; a bad reason means the text has to change. A single
// error would make all four unactionable.
var (
	// ErrFlagKind means the kind is not one of FlagKindWrong/FlagKindStale.
	ErrFlagKind = errors.New("flag a memory: kind must be \"wrong\" or \"stale\"")
	// ErrFlagReason means the reason is empty, whitespace only, or past
	// FlagReasonMax RUNES.
	ErrFlagReason = fmt.Errorf("flag a memory: reason is required and at most %d characters", FlagReasonMax)
	// ErrFlagNoMemory means no memory carries that id.
	ErrFlagNoMemory = errors.New("flag a memory: no memory with that id")
	// ErrFlagWrongProject means the memory exists but belongs to a project the
	// request did not name — the ownership check, and the reason this is a
	// distinct refusal rather than "no memory": the two have different fixes,
	// and collapsing them would tell an agent its id was wrong when the id is
	// fine and its project is not.
	ErrFlagWrongProject = errors.New("flag a memory: the memory belongs to another project")
)

// FlagMemoryRequest is one agent's flag on one memory.
//
// Agent and SessionID are ATTRIBUTION, not identity: an empty value is stored
// as the empty string rather than refused, because a writer with no transport to ask (the CLI)
// is still entitled to record its objection, and refusing it would push the
// caller to invent a name.
type FlagMemoryRequest struct {
	ProjectID string
	MemoryID  string
	Kind      string
	Reason    string
	Agent     string
	SessionID string
}

// FlagMemory appends one flag to a memory.
//
// Validation runs BEFORE the lock for the reason secret_guard gives: a refusal
// that needs no database state must be the same error whatever the store was
// doing when the value arrived, and it must leave nothing behind — a caller
// that retries cannot race a partial write. The kind, then the reason (its
// length and its credentials), then the memory itself.
//
// The lookup and the insert are in ONE transaction, because project ownership
// is a check-then-write: reading the memory's project on the pool and inserting
// afterwards would let a concurrent move hand the flag to the wrong project.
// beginWrite is what makes that atomic here and what refuses the write on a
// store a newer Ghost owns (#746) — this table's vocabulary of kinds is this
// build's.
//
// It is a plain INSERT, never an upsert: two flags on one memory are two
// claims with two reasons, and a second call must not overwrite the first.
// Duplicate rows are the FEATURE, not a mistake to deduplicate, which is also
// why the tool's idempotent hint is false.
//
// The content hash is read inside the same transaction and stamped from the
// content as it stands NOW, so the flag is bound to the text the agent was
// reading when it filed the objection. UsefulnessByMemory then keeps the row
// only while the stored content still hashes to it (#879's rule, applied here):
// a rewrite withdraws the flag, a retag leaves it standing.
//
// It returns an error and nothing else on purpose: the count a caller wants is
// evidence the reader computes, and a count returned by the writer would be a
// second source of the same figure, free to disagree with it.
func (s *Store) FlagMemory(ctx context.Context, req FlagMemoryRequest) error {
	if req.Kind != FlagKindWrong && req.Kind != FlagKindStale {
		return fmt.Errorf("%w: got %q", ErrFlagKind, req.Kind)
	}
	if strings.TrimSpace(req.Reason) == "" {
		return fmt.Errorf("%w: a flag with no reason is an unexplained objection, and the reason is what an "+
			"operator reads back later", ErrFlagReason)
	}
	if utf8.RuneCountInString(req.Reason) > FlagReasonMax {
		return fmt.Errorf("%w: reason is %d characters, the bound is %d", ErrFlagReason,
			utf8.RuneCountInString(req.Reason), FlagReasonMax)
	}
	// The same guard every writer of caller-supplied text calls, and it refuses
	// BEFORE any statement for the reason secret_guard states. The reason is
	// stored verbatim in a column an agent's own words live in, so it is exactly
	// as good a place to put a credential as memories.content is — and the
	// refusal must not quote the value back, since this error reaches the
	// caller's context.
	if err := rejectSecret("reason", req.Reason); err != nil {
		return err
	}
	if strings.TrimSpace(req.ProjectID) == "" {
		return fmt.Errorf("%w: a flag with no project to attribute it to would be counted for every project",
			ErrFlagWrongProject)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, _, err := s.beginWrite(ctx, "flag-memory")
	if err != nil {
		return fmt.Errorf("flag memory: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	// Looked up by id ALONE, and the project compared afterwards: a lookup
	// scoped to the project could not tell "unknown id" from "belongs to
	// someone else", and those are two different guesses for the caller to fix.
	var owner, content string
	err = tx.QueryRowContext(ctx,
		`SELECT project_id, content FROM memories WHERE id = ?`, req.MemoryID,
	).Scan(&owner, &content)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrFlagNoMemory, req.MemoryID)
	}
	if err != nil {
		return fmt.Errorf("flag memory: read the memory: %w", err)
	}
	if owner != req.ProjectID {
		return fmt.Errorf("%w: %s", ErrFlagWrongProject, req.MemoryID)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO memory_flags (project_id, memory_id, kind, reason, content_hash, agent, session_id)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		req.ProjectID, req.MemoryID, req.Kind, req.Reason, ContentHash(content), req.Agent, req.SessionID,
	); err != nil {
		return fmt.Errorf("flag memory: %w", err)
	}
	return tx.Commit()
}
