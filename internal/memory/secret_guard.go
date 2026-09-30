package memory

import (
	"errors"
	"fmt"

	"github.com/wcatz/ghost/internal/secret"
)

// ErrSecretContent is the sentinel every credential refusal unwraps to, so a
// caller can tell "Ghost will not store this" from "the database was
// unreachable" without matching on the message.
var ErrSecretContent = errors.New("refusing to store credential-shaped content")

// SecretContentError is the refusal a write path returns for a value
// internal/secret recognised. It names the field and the format and nothing
// else: the message reaches the log file, the saving agent's context, and — for
// reflection — a prompt sent to a third-party model, so quoting the value would
// relocate the secret rather than contain it.
type SecretContentError struct {
	// Field is the caller's name for the argument that was refused, e.g.
	// "content" or "rationale". It is what makes the refusal actionable: an
	// agent that put a credential in the rationale of a decision needs to be
	// told it was the rationale.
	Field string
	// Format is secret.Finding's human-readable label, e.g. "AWS access key
	// ID". It tells the saving agent which value to remove without showing it
	// the value.
	Format string
}

func (e *SecretContentError) Error() string {
	return fmt.Sprintf("%s in %s: %s. Ghost never stores credential values — record where "+
		"the value lives and how to read it, never the value itself",
		ErrSecretContent.Error(), e.Field, e.Format)
}

func (e *SecretContentError) Unwrap() error { return ErrSecretContent }

// rejectSecret returns a *SecretContentError when text holds a credential. It
// is the store's single seam for the guard, called before any statement on
// every path that writes caller-supplied text: refusing first means a caller
// that retries cannot race a partial write, and it means the refusal is the
// same error whatever the store was doing when the value arrived.
func rejectSecret(field, text string) error {
	finding, ok := secret.Detect(text)
	if !ok {
		return nil
	}
	return &SecretContentError{Field: field, Format: finding.Label}
}

// RejectSecret is `rejectSecret` under the name a boundary can call, and it is
// exported for exactly one caller: `mcpserver.ensureProjectFor`, which NAMES the
// `project_id` it refused so an agent can fix it.
//
// Naming is a promise the value has to be able to keep, and a credential cannot.
// A *SecretContentError quotes nothing precisely because the sentence it produces
// reaches the log file, the saving agent's context, and — for reflection — a
// prompt sent to a third-party model; a boundary that appended the value would
// relocate the secret rather than contain it. So that boundary asks this FIRST
// and returns the guard's own refusal untouched, and the shape rule it would
// otherwise have quoted a value into is then free to name what is left.
//
// Asking first is also what makes the guarantee hold for a value that is BOTH.
// `CheckImportedProject` judges shape before credentials, so a hostile id that
// also holds a token comes back as a shape error with the credential hiding
// behind it; a caller that branched on the returned error could not see it, and
// would print the secret. Here there is nothing to branch on and nothing to
// order: one question, one answer.
//
// The field is the boundary's own name for its argument rather than the column's,
// because the boundary knows what the caller called it — an agent that passed
// `project_id` has to be told it was the `project_id` — while the store's own
// refusals name the artifact's field, which is where its sentences are read.
//
// It is the same `rejectSecret` every writer calls, not a second detection pass:
// one implementation of the detector, so a boundary cannot reach a different
// verdict from one a writer would.
func RejectSecret(field, text string) error {
	return rejectSecret(field, text)
}

// secretField is one caller-supplied text to check, named the way the caller
// named the argument so the refusal says which one to fix.
type secretField struct{ name, text string }

// rejectSecretFields checks the named fields in order and returns the first
// refusal. It is the form every multi-field writer uses, so the field in the
// error is one the caller recognises.
func rejectSecretFields(fields ...secretField) error {
	for _, f := range fields {
		if err := rejectSecret(f.name, f.text); err != nil {
			return err
		}
	}
	return nil
}

// rejectSecretList checks every entry of a list-valued field and names the
// offending one by index. decisions.alternatives is the reason this exists: it
// is rendered back to the agent by ghost_decisions_list, so an entry is stored
// and replayed exactly like the rationale beside it — the only difference is
// that it is a list, and "alternatives" alone would not say which entry to fix.
func rejectSecretList(field string, values []string) error {
	for i, v := range values {
		if err := rejectSecret(fmt.Sprintf("%s[%d]", field, i), v); err != nil {
			return err
		}
	}
	return nil
}

// The guard's reach, and its deliberate limits.
//
// Reached: every write of text a tool, an import, or a model hands to a store
// function the agent can reach — UpsertWithOptions (and so Upsert,
// UpsertWithProvenance, the MCP save tools, the Claude first-contact import,
// and reflection's candidate writes), Create, UpdateMemory, RecordDecision, the
// three task writers, UpdateLearnedContext, and the three portable importers
// ImportMemory/ImportTask/ImportDecision. The importers matter more than their
// position in this list suggests: they write with raw INSERTs rather than
// through Upsert, and their input is a JSONL artifact that arrived from
// somewhere.
//
// Not reached, on purpose:
//
//   - Store.RestoreSnapshot and SeedGlobalMemories. These write byte-exact data
//     into a restored store: a snapshot file, and Ghost's own builtin seeds. A
//     restore that silently dropped rows would be worse than the leak it
//     prevents — a restore is a user's own database coming back — and they are
//     outside the MaxContentLen contract in content.go for the same reason.
//   - Store.ReplaceNonManual, which writes the reflect tier's memories with its
//     own statements and is likewise exported on provider.MemoryStore. Two
//     reasons, and the second is the one that decided it.
//
//     It runs inside ApplyReflection's transaction, so a refusal would roll back
//     an entire consolidation — every merge and every preserved row — over one
//     contaminated memory. The mitigation is `dropCredentialProposals` in
//     cmd/ghost/promotion.go, which sits at the write boundary and therefore
//     after the drop guard's audit — see the comment there for why that ordering
//     is the point rather than an accident.
//
//     Filtering here instead would be worse than either. ReplaceNonManual
//     deletes every replaceable row the snapshot does not account for, so
//     dropping a credential-shaped memory from the emitted set DELETES the
//     stored row: a database written before this guard existed would have that
//     memory removed by an unattended nightly job, with no report and no way to
//     recover it. That is the same reason the reflection drop guard takes an
//     explicit --allow-drops before deleting anything (#549), and it is why
//     clearing a credential out of an existing database is the separate,
//     report-first job described below rather than a side effect of
//     consolidation.
//   - A project's NAME, PATH and repo_remote on the MCP save path, and the
//     agent and session_id beside a saved memory. `source_ref` is NOT in this
//     list any more — it became a caller-supplied argument on the three writer
//     tools and is guarded on four of the seven writers that reach it. The
//     paragraph below names the three that do not, because a lead-in that
//     contradicts its own detail is the failure this whole comment is written
//     against.
//
//     The project name and path are the real gap and they are structural: a save
//     calls EnsureProjectWithRepo BEFORE the guard, so covering them means
//     deciding whether a project may be created before its name is known to be
//     safe — a behaviour change on every save, for a field an operator sets
//     once. Not taken here.
//
//     The tags were on this list too, on the claim that they are not returned by
//     search and not quoted into a harness prompt. That claim was false and I
//     should have checked it rather than asserted it: assemble.Item.Line
//     marshals the tag list into the row that ghost_memory_search returns, and
//     BuildReflectionPrompt writes `, tags:[…]` into the prompt sent to a CLI
//     harness talking to a third-party model. validateTags allows ten tags of 64
//     characters, so a 40-character token fits in one.
//
//     Guarding them took two rounds and the second one is the useful part: the
//     first guarded Create and UpsertWithOptions and then said "they are guarded
//     now", which read as the general claim and was true of two of the four
//     tag-bearing writers. A guard's reach is a set, and a claim about it has to
//     enumerate the set rather than assert it — so the writers that take a tag
//     list are: Create, UpsertWithOptions, UpdateMemory, RecordDecision (which
//     marshals tags into BOTH the decisions row and a companion memory row, and
//     the companion is an ordinary memory), and the portable importers, whose
//     tags column came straight out of an artifact file. All five, each before
//     its lock, and TestEveryTagBearingWriterIsGuarded is the table.
//
//     What is left unguarded is agent and session_id as the HARNESS states
//     them, and a project's name and path. `source_ref` is not in that list any
//     more: it became a caller-supplied argument on the three writer tools, so
//     the harness route now carries it, and it is checked on four of the seven
//     writers that reach the column — Create, UpsertWithOptions,
//     UpdateMemoryWithOptions and ImportMemory — beside the portable import it was
//     already guarded on. The other three are named two paragraphs down.
//
//     THREE writers do not reach it, and all three are named rather than left as
//     an invisible remainder. RestoreSnapshot (SQL, from the snapshot table) and
//     CreateFromCorpus (insertMemory directly) are the byte-exact exclusions
//     MaxContentLen draws. ReplaceNonManual is the third, and it arrived with
//     #677: a rewrite or a merge now inherits the replaced row's validity and
//     provenance onto the row that becomes, so it writes source_ref. It is exempt
//     for the reason given above rather than by omission — filtering there
//     DELETES the stored row, so refusing an emission whose inherited reference is
//     credential-shaped would silently drop a memory from a pre-guard database,
//     and the value is a copy of one this database already guarded at its own
//     write rather than new caller text. That is coverage by transitivity, and it
//     is only true while the source row was itself written through a guard: a
//     store predating it, or a hand-edited row, breaks the chain — the same
//     residual the two byte-exact writers have, and the reason
//     assemble.SourceRefLabel bounds what it PRINTS for all three. That is a
//     length bound, not this guard: an untrusted `source_ref` is still checked
//     wherever a writer can see it. agent and session_id as the harness states
//     them are the real gap and it is structural: a harness is chosen by Ghost,
//     not by the caller, so guarding it is the wrong layer — see above.
//
//     A SECOND COPY is the other way a field is unguarded while its writer is
//     guarded, and it is the one #673 opened: since memory_provenance exists, a
//     provenance value lands TWICE — once on memories, once on an append-only
//     evidence row — and the table is the worse of the two, because it is never
//     overwritten, the next `ghost export` re-emits it, and only the purge's
//     explicit DELETE reaches it. So guarding a writer's memories column says
//     nothing about the evidence rows the same write creates, and the rule is
//     the import's, already stated there: guard a field at EVERY copy the write
//     creates, or one of them leaks.
//
//     Both routes are covered, for different reasons and each with a witness.
//     ImportMemory checks the memory's three provenance fields AND each carried
//     record's own agent, session_id and source_ref, because there the copy is
//     the FILE's content and the memory-level guard cannot see it at all — see
//     TestImportedEvidenceIsGuardedLikeTheMemoryRow. The harness path cannot put
//     a credential in an evidence row that its guard did not already refuse,
//     because every appendEvidenceTx there writes the SAME Provenance value, in
//     the SAME transaction, after the same rejectSecret: insertMemory behind
//     Create's check, and all three of UpsertWithOptions' appends — the
//     fold-into-existing one above the two fresh-insert ones — behind the single
//     check at the top of that function. That is coverage BY CONSTRUCTION rather
//     than by a second call, so it needs a witness instead of a redundant guard:
//     TestObservedEvidenceCarriesNoUnguardedProvenance is that witness, and it is
//     the assertion that fails the day a writer appends evidence from a Provenance
//     its guard never saw.
//   - Content already in the database. This guard reads what a caller is
//     trying to write; it does not sweep rows a previous version stored. Doing
//     that is a separate, report-first job — a detection pass over existing
//     rows has to be able to tell an operator what it found without changing
//     their memory store under them. Note that the portable importers are NOT
//     in this category: an artifact is untrusted input arriving now, not a row
//     that was already here.
//
// The consequence of the first limit is stated plainly: a caller that reaches
// RestoreSnapshot or ReplaceNonManual directly bypasses the guard. Both are raw
// "write exactly this" primitives on the provider.MemoryStore interface, both
// have exactly one production caller, and each of those filters upstream — a
// snapshot file, and the reflection tier. So the exposure is a future caller
// reading the interface rather than a hole in a shipped path. If that changes,
// the guard moves with it.
