package memory

import (
	"fmt"
	"strings"
	"time"
	"unicode"
)

// This file is the WHOLE of what `ghost import` refuses a record for, in one
// place, as four functions that touch nothing: `CheckImportedProject`,
// `CheckImportedMemory`, `CheckImportedTask` and `CheckImportedDecision`. Each
// `Import*` below portable.go calls exactly one of them, and `internal/portable`
// calls the same one to decide what an EXPORT may write.
//
// Why the predicates exist, and why they are one per record type rather than
// four call sites each:
//
//	ghost export can exit 0 with an artifact that ghost import then partly refuses.
//
// #796 closed the first version of that hole — the id-shape checks — and left the
// rest of it open. The exporter called `CheckImportedID` and the two project shape
// checks because those happened to be the importer's own exported functions; every
// OTHER refusal the importers make was written inline inside them, so the exporter
// had no way to learn about it. A project row with `name = ''` (valid SQL, and
// reachable from a pre-guard write, a hand edit, a restore) exported at exit 0 and
// was refused on restore — and the child memory it took with it was refused as
// "project not found", so one bad row cost a whole project's worth. A memory whose
// content was credential-shaped exported at exit 0 and was refused on restore, by
// design, with no way to learn that in advance. A backup that looks clean and loses
// rows on restore is the worst failure this pair of commands has, because the loss
// is discovered at exactly the moment the artifact was needed.
//
// A second rule, a second spelling, is what let the two drift in the first place.
// So the rule is written ONCE, as a pure function of the record, and both sides
// call it. Adding an import refusal now means adding it here, and there is no
// second place it can be forgotten from: `internal/portable` cannot see inside
// `ImportProject`, and this file cannot reach the database.
//
// Two properties of the shape of the answer, both of which the tests pin:
//
//   - A predicate returns the SAME error the importer returns, so a refusal reads
//     identically whether it was reached by importing or by exporting. The messages
//     name the FIELD and never the record's id. They used to interpolate the id
//     (`task %s: title is required`), which was safe only because the shape check
//     ran first (see CheckImportedID) — a rule that held by ordering rather than by
//     construction. Not interpolating it makes the property unconditional, and it
//     costs the reader nothing: both reports print the id beside the message
//     anyway (`labelOrID` on the import side, `assemble.Token` on the export side),
//     so the id appeared twice in an import refusal and now appears once.
//
//   - NEITHER PROJECT SHAPE MESSAGE SPELLS OUT A CHARACTER IT REFUSES. They used
//     to say "backtick or «»", and the prose put a data delimiter into the very
//     sentence that exists to keep one out of a report — so a reader, and a test,
//     could no longer tell a message MENTIONING a delimiter from one CARRYING the
//     caller's. It is the same rule `mcpserver.validateTags` follows for a tag,
//     and it is why the prose here says "backtick or data delimiter". This matters
//     more since #824: the write boundary quotes the refused value beside THIS
//     message through `assemble.Token` so the caller can see what to change, and a
//     sentence that already held a « would make the quoted value unreadable as
//     data — exactly the ambiguity the quoting exists to remove.
//
//     `CheckImportedID` is the one message here that still spells its characters
//     out, and it is not the same case: no write boundary quotes a refused RECORD
//     id beside this sentence, because the record id is a value the artifact's
//     author already chose and is about to be told to change. Named here so a
//     reader who greps for the class finds the exception rather than assuming the
//     property is file-wide.
//
//   - NEITHER PROJECT SHAPE MESSAGE SAYS ITS VALUE IS NEVER SHOWN, because #824
//     made that false. Both used to close "the offending value is not shown,
//     because it is the value being refused", and the write boundary now names the
//     value immediately after that sentence through `assemble.Token` — so a message
//     that kept it would tell a caller not to look for a value printed right
//     beside the words. They say instead that THIS SENTENCE does not repeat it,
//     and why: the same text is the export report's, where quoting the caller's
//     text is noise and a hazard. True on both surfaces, which is what a message
//     two surfaces share has to be.
//
//   - A predicate checks a RECORD, never a STORE. Everything that depends on what
//     the destination holds — the project a record names, a project this store
//     already records the same checkout for, a task's blocker, an id that still has
//     recorded history — is deliberately absent, and the comment on each says why.
//     An exporter that guessed at any of them would be inventing facts about a
//     store it has never opened. The one of those checks the export DOES have a
//     counterpart for is the missing project, and the counterpart is the orphan
//     rule in `internal/portable.Export`: a record whose project is left out of the
//     artifact goes with it, because the importer resolves a record's project
//     against the artifact.

// CheckImportedProject reports why `ghost import` would refuse this project, or
// nil when it would accept it. It is the only place a project record is judged.
//
// The order is the importer's, and it is load-bearing in both directions. The
// id's SHAPE comes first and above every message that could name the id: a
// project's id is printed inside backticks and outside the «...» data delimiters
// on every listing, so a newline in it forges a line — and a message that
// interpolated the id would carry the payload into the very report that names it.
// Then the required fields, path before name, and then the credential guard.
//
// Placement AFTER the importer's id-presence check matters MORE for a project than
// for the other three records, because a project refusal CASCADES: a failed project
// step never records its id, so every memory, task and decision naming that project
// is then rejected for a project "not found". A check that rejected a project the
// store legitimately holds would therefore take its whole contents with it.
func CheckImportedProject(p PortableProject) error {
	if p.ID == "" {
		return fmt.Errorf("project id is required")
	}
	// Three fields and three rules, because a project is printed in three places
	// with three different shapes: its id inside backticks in a listing, its name
	// as a bare label that is also the session-start block's own `## Ghost
	// context:` heading, and its path inside backticks for a human to copy. The
	// name and the path are the ones a space belongs in — `ensureProjectFor`
	// stores a caller's `project_id` argument as BOTH the id and the name, and
	// that argument is routinely a filesystem path — so their rule refuses the
	// line-forging characters and nothing else. See CheckImportedProjectID.
	if err := CheckImportedProjectID(p.ID); err != nil {
		return err
	}
	if err := CheckImportedProjectText("name", p.Name); err != nil {
		return err
	}
	if err := CheckImportedProjectText("path", p.Path); err != nil {
		return err
	}
	// And only now the two required-field checks. Their order among themselves is
	// the importer's: path first.
	if p.Path == "" {
		return fmt.Errorf("path is required")
	}
	if p.Name == "" {
		return fmt.Errorf("name is required")
	}
	// The credential guard, last, and it is here rather than in the importer for
	// the reason every check in this file is: an exporter writing a project whose
	// name or path holds a credential is writing a record that will be refused on
	// import, and refusing it at export is the only place the operator learns
	// before they need the backup. Both fields are replayed into every later
	// session's digest and returned by ghost_project_list. repo_remote is guarded
	// too: it is replayed into every session's digest and written into every
	// export, so a token in it travels exactly as one in the name would.
	// NormalizeRepoRemote strips the userinfo (on every branch, scheme-less
	// spellings included), the query string and the fragment before a write, so
	// those positions never reach the column from a current build. This guard is
	// the backstop for ANY token position, including the path, which
	// normalisation cannot recognise and leaves alone. The store's creating
	// INSERT arms ask it; the refresh and bind routes do not, so import and
	// export are where it always holds. It is also what covers a row an older
	// build stored: no migration rewrites such a row in place, but it is refused
	// here on import and, because `ghost export` asks this same predicate, named
	// and left out of the artifact. The value judged is the one the record
	// carries, un-normalised, so a legacy `?token=…` is seen (an artifact whose
	// remote carries a token is refused rather than silently stripped).
	return rejectSecretFields(
		secretField{"name", p.Name},
		secretField{"path", p.Path},
		secretField{"repo_remote", p.RepoRemote},
	)
}

// ValidityRefusalError marks a memory refused for its validity triple or its
// confidence: an unreadable stamp, a window that does not end after it starts,
// or a rating outside 0 to 1. Error is the save tool's own sentence, unchanged.
// It is typed so `ghost export` can tell this refusal from one about the id: the
// id is fine, and the repair is an edit of the row in place rather than a
// delete and re-save, which would lose its links and history.
type ValidityRefusalError struct{ Err error }

func (e *ValidityRefusalError) Error() string { return e.Err.Error() }
func (e *ValidityRefusalError) Unwrap() error { return e.Err }

// ImportedValidity reads a portable memory's validity triple as the save tool
// reads it — NormalizeImportedStamp per field, with the end-of-day rule on
// valid_until alone — and refuses a window that does not end after it starts,
// judged on the normalized instants so an RFC 3339 pair and a same-day date pair
// are judged correctly. CheckImportedMemory calls it for the refusal and
// ImportMemory for the values it binds, so the two cannot read a stamp
// differently.
func ImportedValidity(m PortableMemory) (Validity, error) {
	var v Validity
	var err error
	if v.ValidFrom, err = NormalizeImportedStamp("valid_from", m.ValidFrom, false); err != nil {
		return Validity{}, err
	}
	if v.ValidUntil, err = NormalizeImportedStamp("valid_until", m.ValidUntil, true); err != nil {
		return Validity{}, err
	}
	if v.VerifiedAt, err = NormalizeImportedStamp("verified_at", m.VerifiedAt, false); err != nil {
		return Validity{}, err
	}
	if err := CheckWindowOrder(v, Validity{}); err != nil {
		return Validity{}, err
	}
	return v, nil
}

// CheckImportedMemory reports why `ghost import` would refuse this memory, or nil
// when it would accept it. It is the only place a memory record is judged.
//
// Every check an MCP save makes is here, because an import is another way to reach
// the same table and must not be the way around its rules — plus the two the
// artifact adds (the id's shape, and the evidence records' own text).
//
// What is NOT here, and each omission is a fact about a STORE rather than about
// this record:
//
//   - ClampContent, and the [0,1] clamp on a stated importance. Both are
//     TRANSFORMATIONS, not refusals — the import applies them and reports them, so
//     the operator can see that the stored text is shorter than the artifact's. A
//     predicate that refused an over-long content would turn a documented,
//     reported cut into a lost row.
//
//   - The provenance downgrade. It depends on `ImportOptions.TrustProvenance`, and
//     it is a rewrite the report states, not a refusal. The retention downgrade
//     rides the same flag, for the same reason, and is also a rewrite: what IS
//     checked here is the tier's VALUE SET, exactly as category's and source's
//     are — a fourth tier is a bad value in a column, and an absent one is a
//     record that states nothing rather than a bad value.
//
//   - The recorded-history check. See ImportMemory for why it must run against the
//     DESTINATION and after the presence check; screening on it here would be
//     actively wrong, since every write appends a history row and so every memory
//     this store exports has one.
//
//   - A tag's SHAPE. This is the omission most likely to look like one, so it is
//     stated rather than left to be found: a control character, a backtick or a
//     data delimiter in a tag is accepted here and carried byte for byte.
//
//     The reason is that every OTHER check in this function is a fact about a
//     RECORD, while a tag's characters are a fact about a RENDERING — and
//     `ghost export` calls this predicate to decide what may be left out of a
//     backup. So a shape check here would make a memory with a hostile tag
//     UNBACKED UP, on the day the operator needs the file, for a label the user
//     cannot even see (#811). Nothing on the MCP write path refused such a tag for
//     the whole life of the feature, so any store written through it can hold one.
//
//     Safety is the renderer's, on every surface a tag reaches: `assemble.TagsLabel`
//     on a memory row, and `internal/reflection`'s `tagSubstitution` in the
//     consolidation prompt, which needs the LARGER class because its list is neither
//     JSON nor delimited. The class is refused where a NEW tag can arrive —
//     `validateTags` in `internal/mcpserver`, on all four tools that write a tag list,
//     which is the only boundary an ordinary save passes through.
func CheckImportedMemory(m PortableMemory) error {
	// The id's SHAPE, and above every message that could name the id. The id is
	// the one field of this record that reaches a rendered line OUTSIDE the
	// «...» data delimiters — `Item.Line` and `formatMemories` print it inside
	// backticks ahead of the content, so a newline in it forges a second line
	// reading as Ghost's own memory row (#791). Refusing before the id reaches a
	// format verb is what keeps the refusal itself from carrying the payload.
	if m.ID == "" {
		return fmt.Errorf("memory id is required")
	}
	if err := CheckImportedID(m.ID); err != nil {
		return err
	}
	// Then the field checks, in the importer's order. They are validation rather
	// than precondition — none of them is about whether this record may be written
	// at all.
	if m.ProjectID == "" {
		return fmt.Errorf("project_id is required")
	}
	if m.Content == "" {
		return fmt.Errorf("content is required")
	}
	// The two value sets below are RENDERED through RefusalArg rather than %q, and
	// the reason is the position they run in: this predicate judges a record
	// BEFORE the secret guard further down, and its sentences are PRINTED — the
	// import report carries every rejected record's error and the CLI prints each
	// one — so a hand-edited artifact can put a credential in any column and a
	// sentence that quotes it would carry the secret to the terminal. RefusalArg
	// is byte-identical to %q for every value the guard does not recognise, so an
	// ordinary typo is still named; it withholds the two it must — a
	// credential-shaped value by its format and an oversized one by its length.
	if !IsValidCategory(m.Category) {
		return fmt.Errorf("invalid category %s — must be one of: architecture, decision, pattern, convention, gotcha, dependency, preference, fact", RefusalArg("category", m.Category))
	}
	// The source is checked against the schema's own value set, not the secret
	// guard: an invalid source is a bad value in a column.
	if !IsValidSource(m.Source) {
		return fmt.Errorf("invalid source %s — must be one of: reflection, chat, manual, tool, mcp, onboarding, decision_log, builtin", RefusalArg("source", m.Source))
	}
	// And the tier, on the same terms and with the same single refusal the MCP
	// save tool, the CLI and UpsertWithOptions use — so a caller who typed a
	// fourth tier into the file learns which three words the column accepts
	// instead of failing the INSERT with SQLite's CHECK wording. The empty
	// string is NOT a tier but is not a refusal either: it is a record that
	// states nothing, which is what every v1 artifact does, and NormalizeRetention
	// resolves it to the column's own default.
	if m.Retention != "" && !IsValidRetention(m.Retention) {
		return InvalidRetentionError(m.Retention)
	}
	// The validity triple and the rating, by the save tool's own rules, so an
	// artifact is not the way around them: a stamp no accepted form reads, a
	// window that ends before it starts, and a confidence outside 0 to 1 are each
	// refused with the save tool's sentence. Every reader treats an unreadable
	// stamp as no claim, so storing one would serve an expired fact as live.
	// The messages quote a value only through RefusalArg, which is why they may
	// run ahead of the token guard below.
	if _, err := ImportedValidity(m); err != nil {
		return &ValidityRefusalError{Err: err}
	}
	if m.Confidence != nil {
		if err := CheckConfidence(*m.Confidence); err != nil {
			return &ValidityRefusalError{Err: err}
		}
	}
	// The secret guard, and the window it runs in is the importer's: after the
	// presence check, so a record already in the store stays a skip, and before the
	// apply=false early return, so a dry run classifies a record exactly as the
	// apply run it previews. An artifact is untrusted input arriving from a file,
	// which is why it is guarded at all despite the same idempotence argument
	// applying to Create.
	// agent and session_id join source_ref here, and the reason is the arrival
	// record the import appends: an import writes the artifact's own agent and
	// session onto a memory_provenance row, so an unguarded value here would be
	// stored twice. On this route all three are the FILE's content rather than the
	// harness's identity, which is the condition secret_guard.go already names for
	// guarding them.
	if err := rejectSecretFields(
		secretField{"content", m.Content},
		secretField{"source_ref", m.SourceRef},
		secretField{"agent", m.Agent},
		secretField{"session_id", m.SessionID},
		secretField{"created_at", m.CreatedAt},
		secretField{"updated_at", m.UpdatedAt},
	); err != nil {
		return err
	}
	if err := checkImportedTimestamp("created_at", m.CreatedAt, timestampLayouts("created_at")); err != nil {
		return err
	}
	if err := checkImportedTimestamp("updated_at", m.UpdatedAt, timestampLayouts("updated_at")); err != nil {
		return err
	}
	// last_accessed is a timestamp field that reaches the store from the artifact.
	// Guard it against credentials and validate its format.
	if m.LastAccessed != nil {
		if err := rejectSecretFields(secretField{"last_accessed", *m.LastAccessed}); err != nil {
			return err
		}
		if err := checkImportedTimestamp("last_accessed", *m.LastAccessed, timestampLayouts("last_accessed")); err != nil {
			return err
		}
	}
	// resolved_at is written by the store with datetime('now'), so it uses
	// StoredStampLayout. It is a pointer because an absent value means "not
	// resolved" and a stated value is a real claim. Guard it against credentials
	// and validate its format.
	if m.ResolvedAt != nil {
		if err := rejectSecretFields(secretField{"resolved_at", *m.ResolvedAt}); err != nil {
			return err
		}
		if err := checkImportedTimestamp("resolved_at", *m.ResolvedAt, timestampLayouts("resolved_at")); err != nil {
			return err
		}
	}
	// And the lengths, for the same reason the writers apply them: a reference and
	// a harness name are both printed as labelled fields on every listing, so an
	// artifact is a way to plant a value that reaches every answer touching the row.
	// Refused rather than clamped — a truncated path is a different path.
	if _, err := boundedSourceRef(m.SourceRef); err != nil {
		return err
	}
	if _, err := boundedAgent(m.Agent); err != nil {
		return err
	}
	// The tags too. An artifact's tags column is untrusted input from a file and
	// was being written raw; the record it lands in is an ordinary memory, so it is
	// assembled into every search row and quoted into the next reflect prompt like
	// any other.
	if err := rejectSecretList("tags", m.Tags); err != nil {
		return err
	}
	// The scope, on the terms of the save: an artifact's scope column is the
	// FILE's content, printed outside the data delimiters on every listing, so a
	// token in a key or value is refused and the count and lengths are bounded.
	// Export asks this predicate too, so a stored row that predates the bound is
	// named in the export report rather than carried into an artifact the importer
	// would refuse.
	if err := checkScope(m.Scope); err != nil {
		return err
	}
	// The evidence records' own text, for the same reason. An artifact's evidence
	// rows are the FILE's content, exactly as `source_ref` above is, and this is
	// the one route where that is true: a harness's own identity is not a secret,
	// but a hand-edited or hostile artifact can put anything in a nested field. The
	// memory-level guard cannot see these, because the memory row does not hold
	// them — so without this the table becomes the one place a credential survives,
	// in an append-only store the next `ghost export` re-emits and only the purge's
	// explicit DELETE reaches. The field is named per record, never the value.
	//
	// An evidence id is the row's primary key in `memory_provenance`, so it is
	// judged here too: a whitespace-only one, one carrying a control or line-break character,
	// one past MaxImportedIDLen, or one an earlier entry of the SAME memory already
	// used. The insert would mint a fresh id for a repeat (importEvidenceTx keeps an
	// id only while no row holds it), so the repeat is not refused to protect the
	// key: two records of one memory naming one id is a hand edit whose intent the
	// importer cannot tell, and saying so beats silently renaming one of them. An id
	// held by a DIFFERENT memory is minted instead, because nothing about this
	// record is ambiguous. Each refusal names the entry by position and never the
	// id, which is the value being refused.
	seenEvidenceIDs := make(map[string]struct{}, len(m.Evidence))
	for i, e := range m.Evidence {
		if !IsValidEvidenceKind(e.Kind) {
			return fmt.Errorf("evidence record %d has invalid kind %s — must be one of: observed, imported, verified, legacy", i, RefusalArg("kind", e.Kind))
		}
		if err := rejectSecretFields(
			secretField{fmt.Sprintf("evidence[%d].agent", i), e.Agent},
			secretField{fmt.Sprintf("evidence[%d].session_id", i), e.SessionID},
			secretField{fmt.Sprintf("evidence[%d].source_ref", i), e.SourceRef},
		); err != nil {
			return err
		}
		// An empty id is not refused: the importer mints one for it, so it is also
		// left out of the repeat check. A whitespace-only id is not empty to that
		// rule, would be stored raw, and is refused.
		if e.ID != "" && strings.TrimSpace(e.ID) == "" {
			return fmt.Errorf("evidence[%d].id must not be whitespace only — leave it empty and one is minted", i)
		}
		if len(e.ID) > MaxImportedIDLen {
			return fmt.Errorf("evidence[%d].id must be at most %d bytes, got %d — it names a row, not a document", i, MaxImportedIDLen, len(e.ID))
		}
		if unprintableInIdentifier(e.ID, false) != "" {
			return fmt.Errorf("evidence[%d].id must hold no control character, line separator, backtick or «» — the offending id is not shown, because it is the value being refused", i)
		}
		if e.ID == "" {
			continue
		}
		if _, dup := seenEvidenceIDs[e.ID]; dup {
			return fmt.Errorf("evidence[%d].id repeats the id of an earlier evidence record of this memory, which is ambiguous — give each evidence record its own id, or leave them empty and one is minted for each", i)
		}
		seenEvidenceIDs[e.ID] = struct{}{}
	}
	return nil
}

// CheckImportedTask reports why `ghost import` would refuse this task, or nil when
// it would accept it. It is the only place a task record is judged.
//
// The project this task names and the blocker it points at are NOT checked here.
// Both are facts about the destination store, and the portable importer resolves
// them before it ever gets here: a pointer to a task the artifact does not contain
// has its pointer dropped (orderTasks), so a task whose blocker was left out of the
// artifact imports as a task that is simply not blocked — which is the honest
// state, since the blocker does not exist to be blocked by.
func CheckImportedTask(t Task) error {
	if t.ID == "" {
		return fmt.Errorf("task id is required")
	}
	// The id's SHAPE, above every message that could name the id — see
	// CheckImportedMemory for why, and CheckImportedID for the rule.
	if err := CheckImportedID(t.ID); err != nil {
		return err
	}
	if t.ProjectID == "" {
		return fmt.Errorf("project_id is required")
	}
	if t.Title == "" {
		return fmt.Errorf("title is required")
	}
	if !validTaskStatuses[t.Status] {
		return fmt.Errorf("invalid status %s — must be one of: pending, active, done, blocked", RefusalArg("status", t.Status))
	}
	if t.Priority < 0 || t.Priority > 4 {
		return fmt.Errorf("invalid priority %d — must be between 0 and 4", t.Priority)
	}
	// branch: guard against credentials first (so a token-bearing oversize
	// branch is refused by the token guard), then bound its size like other
	// task text.
	if err := rejectSecretFields(secretField{"branch", t.Branch}); err != nil {
		return err
	}
	if len(t.Branch) > MaxBranchLen {
		return fmt.Errorf("branch must be at most %d bytes, got %d — it is stored alongside the task's text and reaches the same renderers", MaxBranchLen, len(t.Branch))
	}
	// completed_at, created_at, and updated_at are otherwise unvalidated text
	// that a normal save refuses, and an artifact carries them. Same window as
	// the memory's: after the presence check, before apply=false.
	if err := rejectSecretFields(
		secretField{"title", t.Title},
		secretField{"description", t.Description},
		secretField{"notes", t.Notes},
		secretField{"completed_at", t.CompletedAt},
		secretField{"created_at", t.CreatedAt},
		secretField{"updated_at", t.UpdatedAt},
	); err != nil {
		return err
	}
	if err := checkImportedTimestamp("completed_at", t.CompletedAt, timestampLayouts("completed_at")); err != nil {
		return err
	}
	if err := checkImportedTimestamp("created_at", t.CreatedAt, timestampLayouts("created_at")); err != nil {
		return err
	}
	if err := checkImportedTimestamp("updated_at", t.UpdatedAt, timestampLayouts("updated_at")); err != nil {
		return err
	}
	return nil
}

// CheckImportedDecision reports why `ghost import` would refuse this decision, or
// nil when it would accept it. It is the only place a decision record is judged.
//
// As with a task, the project and the superseding decision are facts about the
// destination store rather than about this record, and the portable importer
// resolves them first (orderDecisions drops a pointer to a decision the artifact
// does not contain).
//
// Its tags are judged for CREDENTIALS and for nothing else, and the asymmetry with
// a memory's tags is deliberate and settled: `CheckImportedMemory` guards them the
// same way (below), while neither of them judges a tag's SHAPE (#822). A tag's
// characters are a fact about a RENDERING, and `ghost export` calls this predicate
// to decide what may be left out of a backup — so a shape check here would put a
// decision with a hostile label outside every export on the day the operator needs
// the file. A credential is the other question: the artifact's tags column is
// untrusted input from a file, it was being written raw, and a token in it is
// re-emitted by the next `ghost export` into the file most likely to be pasted into
// a support channel. The refusal therefore names the field and never the value, and
// it makes such a decision a `!` line rather than a silent loss.
func CheckImportedDecision(d Decision) error {
	if d.ID == "" {
		return fmt.Errorf("decision id is required")
	}
	if err := CheckImportedID(d.ID); err != nil {
		return err
	}
	// A decision id is also the tail of its companion's `decision:<id>` link, and
	// the lookup splits a source_ref on decisionRefSeparator. RecordDecision mints
	// hex ids, but an import writes this one verbatim, so an id holding the
	// separator would split its own link into tokens that name no decision: the
	// companion is never found and a supersede answers clean while the reversed
	// decision keeps ranking. The other characters a LIKE treats specially (% and
	// _) only widen the lookup's candidate set, which the exact token check then
	// narrows, so the separator is the one character that breaks a link.
	if strings.Contains(d.ID, decisionRefSeparator) {
		return fmt.Errorf("decision id must not contain %q — it separates the decision links a memory carries, so an id holding it names no decision. Give the decision a new id in the artifact", decisionRefSeparator)
	}
	if d.ProjectID == "" {
		return fmt.Errorf("project_id is required")
	}
	if d.Title == "" {
		return fmt.Errorf("title is required")
	}
	if d.Decision == "" {
		return fmt.Errorf("decision is required")
	}
	if d.Rationale == "" {
		return fmt.Errorf("rationale is required")
	}
	if !validDecisionStatuses[d.Status] {
		return fmt.Errorf("invalid status %s — must be one of: active, superseded, revisit", RefusalArg("status", d.Status))
	}
	// Same window as the memory's. alternatives is a list because it is one:
	// ghost_decisions_list renders it back to the agent, so an entry is stored and
	// replayed exactly like the rationale beside it.
	if err := rejectSecretFields(
		secretField{"title", d.Title},
		secretField{"decision", d.Decision},
		secretField{"rationale", d.Rationale},
		secretField{"created_at", d.CreatedAt},
		secretField{"updated_at", d.UpdatedAt},
	); err != nil {
		return err
	}
	if err := checkImportedTimestamp("created_at", d.CreatedAt, timestampLayouts("created_at")); err != nil {
		return err
	}
	if err := checkImportedTimestamp("updated_at", d.UpdatedAt, timestampLayouts("updated_at")); err != nil {
		return err
	}
	if err := rejectSecretList("alternatives", d.Alternatives); err != nil {
		return err
	}
	// And the tags, in the same order RecordDecision checks them and with the same
	// predicate CheckImportedMemory uses, so a decision and a memory cannot be
	// guarded on one route and not the other. This was the gap #835 closed: the
	// write path has refused a credential-shaped tag since #656 and the artifact's
	// tags column was written raw into the same table, so a token arriving in a
	// file was stored verbatim and re-emitted by every export afterwards.
	//
	// Not a SHAPE check, on the reasoning above and in CheckImportedMemory.
	return rejectSecretList("tags", d.Tags)
}

// createdProject assembles the project record a WRITE is about to store (repoRemote
// is the already-normalised remote, which the creation routes hold), so
// `CheckImportedProject` judges the stored shape rather than the caller's spelling
// of it (#824).
//
// The one substitution is the store's own: `ensureProjectLocked` normalizes an
// empty path to the id, because `projects.path` is UNIQUE and MCP callers pass
// path="" because they name a project rather than describe a checkout. Judging the
// empty path instead would refuse every project an ordinary save creates, over a
// field the store had already decided what to put in it.
//
// It is a function rather than three lines at each site because the two project
// creation routes — `ensureProjectLocked` and `ResolveOrCreateRepoProject` —
// build the same record and must be judged by the same call, and a second
// hand-assembled copy is exactly the drift this file exists to prevent.
func createdProject(id, path, name, repoRemote string) PortableProject {
	if path == "" {
		path = id
	}
	return PortableProject{ID: id, Name: name, Path: path, RepoRemote: repoRemote}
}

// MaxImportedIDLen is the byte cap on a memory id a portable artifact may carry.
//
// The value bounds a KEY, not prose, and it is deliberately generous: the id
// column mints `hex(randomblob(16))` — 32 characters — and a store can legitimately
// hold others. `internal/bench` seeds `bench:<project>:<key>`, a restored snapshot
// reinstates whatever it recorded, and an operator restoring a store written by
// another tool has ids this build never minted. Refusing those would make
// `ghost import` refuse the stores it exists to restore, which is a worse failure
// than the one the bound prevents.
//
// 128 bytes is four times the minted id and comfortably wider than the widest id
// any writer here produces, while still refusing the class the bound is for: a
// payload wearing an id's clothes, echoed into every listing that touches the row.
const MaxImportedIDLen = 128

// MaxBranchLen is the byte cap on a task's branch field.
//
// A branch name is a git reference name, not free-form prose, but the import
// accepts it from an artifact file and must bound it for the same reason the
// other task text fields are bounded: the row-rendering read tools assume a
// row is bounded by construction. The cap matches MaxContentLen so a branch
// cannot be used to sidestep the content cap.
const MaxBranchLen = MaxContentLen

// checkImportedTimestamp validates that a timestamp field from a portable
// artifact is either empty or a valid timestamp in one of the formats the
// store's writer for that column emits. It returns an error naming the field
// if the value is non-empty and invalid.
func checkImportedTimestamp(field, value string, layouts []string) error {
	if value == "" {
		return nil
	}
	for _, layout := range layouts {
		if _, err := time.Parse(layout, value); err == nil {
			return nil
		}
	}
	return fmt.Errorf("%s must be a valid timestamp or empty", field)
}

// timestampLayouts returns the accepted layouts for a given timestamp field.
func timestampLayouts(field string) []string {
	switch field {
	case "last_accessed":
		// Store.Touch writes RFC3339; every other stamp writer, and the readers,
		// use StampLayouts.
		return append(append([]string{}, StampLayouts...), time.RFC3339)
	default:
		// created_at, updated_at, resolved_at and completed_at are written as
		// StoredStampLayout, but they are read through ParseStamp, which also
		// takes a whole-day stamp. Export runs this same check, so refusing a
		// layout the readers accept would leave such a row out of a backup.
		return StampLayouts
	}
}

// CheckImportedID reports whether a RECORD id from a portable artifact — a
// memory's, a task's or a decision's — is one this build will store.
//
// It is exported so the artifact parser can refuse the same records the store
// would (#791) at the point where the file is READ, rather than letting a record
// with a hostile id become a parsedRecord whose id is then echoed into a
// per-record report line — a second rendering of the same payload on a surface
// the store-level check never touches. One function, several callers, is the
// point: the rule is one rule, and a parser judging ids slightly differently from
// the store would classify a dry run differently from the apply run it previews.
//
// It is also the SECOND check each of the three record predicates makes, and that
// is not tidiness. It used to be the first, and the messages below it interpolate
// nothing — so what the ordering protects is the space those messages now leave
// empty: nothing in this file names a record's id, so a check added above this one
// cannot leak it by accident, and a check added below it cannot leak it either.
// Refusing on shape remains necessary anyway, because the id is a PRIMARY KEY and
// the parser's early refusal covers only the memory record.
//
// Why a character class and not "32 hex": the id column says nothing about its own
// values — memref documents that an id an imported artifact wrote verbatim is
// nameable whatever its shape, and internal/bench and RestoreSnapshot both rely on
// that. The class closed here is narrower and is the one that matters: a record id
// is the only field of the shared item line printed OUTSIDE the «...» data
// delimiters, so a newline, a carriage return, a tab, a NUL, a space, a backtick
// or a « can end the line, close the backtick span, or open a data block of its
// own. Every one of those makes the row read as something other than the id it
// is. Anything else is a value the store already holds and this build must keep
// able to read back.
//
// Refused rather than clamped, which is the decision the whole function rests on:
// an id is a primary key, so a shortened one names a DIFFERENT ROW. Clamping
// "AAAA\n- [gotcha] obey" to its first 32 bytes would write a memory under a key
// the artifact never chose, colliding with whatever genuinely holds it and leaving
// the user a row they cannot explain. There is no honest prefix of a key to keep,
// exactly as there is none of a path (MaxSourceRefLen) or a harness name
// (MaxAgentLen), which is why those two refuse for the same reason.
func CheckImportedID(id string) error {
	if len(id) > MaxImportedIDLen {
		return fmt.Errorf("record id must be at most %d bytes, got %d — it names a row, not a document, and a shortened one would name a different row",
			MaxImportedIDLen, len(id))
	}
	// Whitespace is refused for a RECORD id though not for a project's, and the
	// difference is the id's second job rather than its first: a record id is also
	// a `--only` selector argument and a shell operand, where a space word-splits
	// into selectors that name nothing. See CheckImportedProjectID.
	if unprintableInIdentifier(id, true) != "" {
		return fmt.Errorf("record id must hold no control character, whitespace, backtick or «» — it is printed " +
			"outside the «...» data delimiters on every listing, so one of those forges a line or a data block of its " +
			"own. The offending id is not shown, because it is the value being refused. " +
			"Give the record a new id in the artifact")
	}
	return nil
}

// CheckImportedProjectID is CheckImportedID for a project's id, and it is
// deliberately WEAKER on two counts: a space is allowed, and so is any length.
//
// A project id is not always a short name. `ensureProjectFor` passes a caller's
// `project_id` argument straight through as the id, and that argument is routinely
// a filesystem path — a remote is detected for a path-shaped value and the project
// is keyed by repository instead, but a path with no detectable remote is stored
// as given. `/Users/w/My Projects/ghost` is a real project id in a real store, and
// `ghost_list_projects` and the health report print it for a human to copy.
//
// What is refused is the part that forges a LINE: a control character, a line
// separator (U+2028/U+2029), a backtick that closes the span it is printed in, or
// a «» that opens a data block of its own. A space does none of those.
//
// The line separator is refused for the reason the renderer escapes it (#1363):
// U+2028 and U+2029 are the Zl/Zp separators, NOT control characters, so a check
// written against `unicode.IsControl` alone let them through — and a value holding
// one forged a line in the session-start block's own heading, above the explainer
// that marks stored data. The boundary and `assemble.Label` now name the same
// line-ending class.
//
// There is deliberately NO LENGTH BOUND, and the first version of this function
// had MaxImportedIDLen here, which was wrong in a way that reached much further
// than one refused record. A deep checkout or a long macOS/Windows username makes
// a path-shaped id exceed 128 bytes easily, and `ghost export` writes that id into
// the artifact — so `ghost import` would refuse a file `ghost export` had just
// written. Worse, the refusal cascades: a project step that fails never records
// its id, so `checkFor` then rejects EVERY memory, task and decision naming that
// project, and a fresh-store restore of that project imports nothing while
// reporting a reason that names neither the length nor the path. Length is not the
// threat class for an id rendered by a line-safe renderer; the characters that end
// a line are. See CheckImportedProjectText, which has never had a bound.
func CheckImportedProjectID(id string) error {
	if unprintableInIdentifier(id, false) != "" {
		return fmt.Errorf("project id must hold no control character, line separator, backtick or data delimiter — it is printed inside backticks " +
			"and outside the data delimiters, so one of those ends the line or the span. A space is fine, and so " +
			"is any length: a project id is often a filesystem path, and a deep checkout is a longer one. This " +
			"sentence does not repeat the offending id, because it is also the export report's, where the caller's " +
			"text is noise and a hazard; a write boundary that names the value it refused does so beside this " +
			"sentence, through the safe renderer")
	}
	return nil
}

// CheckImportedProjectText is the same rule for a project's name and path: no
// control character, no line separator (U+2028/U+2029 — the Zl/Zp separators,
// which `unicode.IsControl` does not cover, #1363), no backtick, no «». A space
// is fine, for the reason CheckImportedProjectID gives — a project name is
// normally full of them.
//
// field names the column so the refusal says which value to fix, and no length
// bound is applied because length is not the threat class here: a long name or
// path is still one line, and the renderer keeps it that way whatever it holds.
func CheckImportedProjectText(field, value string) error {
	if unprintableInIdentifier(value, false) != "" {
		return fmt.Errorf("project %s must hold no control character, line separator, backtick or data delimiter — it is printed as a label on "+
			"every listing and in the session-start block's own heading, and one of those ends the line. A space is "+
			"fine. This sentence does not repeat the offending value, because it is also the export report's, where "+
			"the caller's text is noise and a hazard; a write boundary that names the value it refused does so "+
			"beside this sentence, through the safe renderer", field)
	}
	return nil
}

// unprintableInIdentifier returns "" when s holds nothing that can end a rendered
// line, a backtick span or a «...» data block, and the reason otherwise.
//
// It is the one place that class is written down, because three exported checks
// depend on it and a second copy would be a second rule. spaces is a
// parameter rather than a constant because the answer genuinely differs by what
// the value is FOR: a record id is a `--only` selector and a shell operand, where
// a space word-splits, and a project id is often a path, where it does not.
//
// The line-ending class is TWO tests, and it has to be: `unicode.IsControl` is Cc
// (C0, DEL and C1) and does NOT cover the Zl/Zp separators U+2028 and U+2029,
// which end a line for every reader that honours Unicode (#1363). `unicode.IsSpace`
// covers them only in the `spaces` branch, so it cannot stand in for the control
// test on a project's id, name or path — that is the branch that let them through.
func unprintableInIdentifier(s string, spaces bool) string {
	for _, r := range s {
		switch {
		case unicode.IsControl(r):
			return "control character"
		case unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r):
			return "line separator"
		case spaces && unicode.IsSpace(r):
			return "whitespace"
		case r == '`':
			return "backtick"
		case r == '«' || r == '»':
			return "guillemet"
		}
	}
	return ""
}
