// Package portable reads and writes the portable JSONL artifact that moves
// memories, tasks, decisions and projects between Ghost installations.
//
// It is a wire format, not a backup: a backup (`memory.Store.Backup`) is a
// consistent copy of the database, restorable with no interpretation, while an
// artifact is inspectable text that a user can read, edit, diff and keep in
// version control. The two answer different questions, so neither is built on
// the other.
//
// The format is JSON Lines: one self-describing JSON object per line, opened by
// a header line carrying the schema version. A line-oriented format survives a
// damaged file — a line that will not parse is rejected on its own, by line
// number, and the records around it still import — which a single JSON document
// cannot, and it lets a user grep the artifact for one memory without a JSON
// tool.
package portable

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/wcatz/ghost/internal/memory"
)

// SchemaVersion is the version of the artifact format this build writes and
// reads.
//
// It is a wire-format version, independent of the database schema version: an
// artifact is a file that outlives the ghost that wrote it, and its meaning is
// the shape of the records, not the tables behind them. It increments when a
// record changes shape, and never for an added optional field, so an artifact
// from a slightly older build still imports.
//
// v2 is the memory record's nested `evidence` list (#673). A list is a change of
// SHAPE rather than one more optional field, so it took a version — and the
// version alone would have made every v1 artifact unreadable, which is why the
// readable range below is explicit rather than "this version only".
//
// Import refuses any version outside the range it reads. A newer one is a file
// whose fields this build cannot interpret, and reading it would insert records
// that mean whatever this build guesses. An older one would normally be a file
// written under rules that no longer hold — so the range is not "everything
// below", it is a list of the versions whose rules still hold. v1 is on it
// because nothing about a v1 record CHANGED: the only difference is a list v1
// files do not carry, and an absent optional field is the documented no-change
// case. A future shape change removes v1 from the range rather than widening it,
// and the test that pins the range is the one that has to be edited to do it.
const SchemaVersion = 2

// minReadableSchemaVersion is the oldest artifact this build reads. See
// SchemaVersion for why it is a range and not everything below.
const minReadableSchemaVersion = 1

// Record type discriminators. Each line carries exactly one, and an import
// refuses a type it does not know: silently skipping records would produce a
// store that is missing data the artifact plainly contains, and the user would
// have no way to know.
const (
	TypeHeader   = "header"
	TypeProject  = "project"
	TypeMemory   = "memory"
	TypeTask     = "task"
	TypeDecision = "decision"
)

// record is one line of the artifact: a type discriminator plus that type's
// payload. The payload is a pointer so an absent one is distinguishable from a
// present-but-empty record — `{"type":"memory"}` is a malformed file, and it has
// to be reported as one rather than importing a memory with no content.
type record struct {
	Type     string                  `json:"type"`
	Project  *memory.PortableProject `json:"project,omitempty"`
	Memory   *memory.PortableMemory  `json:"memory,omitempty"`
	Task     *memory.Task            `json:"task,omitempty"`
	Decision *memory.Decision        `json:"decision,omitempty"`

	// SchemaVersion is on the header line only. It is omitted elsewhere: a
	// payload that repeated it would raise the question of which value governs
	// when the two disagreed.
	SchemaVersion int `json:"schema_version,omitempty"`
}

// headerLine returns the artifact's opening line. It carries the version and
// nothing else — no timestamp, no generator, no count — because a header that
// changed per run would make two exports of an unchanged database differ, and
// the whole point of the deterministic ordering is that a diff of two artifacts
// shows only what changed in the store.
func headerLine() []byte {
	b, _ := json.Marshal(record{Type: TypeHeader, SchemaVersion: SchemaVersion})
	return b
}

// Export writes the store as an artifact: a header line, then every project,
// memory, task and decision.
//
// A projectFilter of "" exports everything; anything else must match a project's
// id or name exactly, and a filter that matches nothing is an error. Silently
// exporting zero records would produce a file that reads like a store with
// nothing in it, and the user who asked for one project would not learn that the
// name was wrong until they tried to import it elsewhere.
//
// Records are written projects first, then memories, then tasks, then decisions.
// Projects come first because a memory names a project, and an artifact a user
// has edited by hand is read in order — a memory before its project would be a
// rejected record in a file that is otherwise fine. Within each kind the order
// is the store's id order, which is what makes the output byte-reproducible.
//
// The export does not write embeddings, and does not leave a partial file behind
// a failure it can foresee: the caller removes the file when Export returns an
// error, so a failed run leaves nothing rather than a half-written artifact that
// looks complete.
func Export(ctx context.Context, s *memory.Store, w io.Writer, projectFilter string) (Stats, error) {
	projects, err := s.PortableProjects(ctx)
	if err != nil {
		return Stats{}, fmt.Errorf("export projects: %w", err)
	}
	selected, err := selectProjects(projects, projectFilter)
	if err != nil {
		return Stats{}, err
	}
	ids := make([]string, 0, len(selected))
	for _, p := range selected {
		ids = append(ids, p.ID)
	}

	memories, err := s.PortableMemories(ctx, ids)
	if err != nil {
		return Stats{}, fmt.Errorf("export memories: %w", err)
	}
	tasks, err := selectTasks(ctx, s, ids)
	if err != nil {
		return Stats{}, err
	}
	decisions, err := selectDecisions(ctx, s, ids)
	if err != nil {
		return Stats{}, err
	}

	// Split every record into what the importer will accept and what it will
	// refuse, BEFORE anything is written, so the counts below describe the
	// artifact rather than the store.
	//
	// The four predicates are the IMPORTER's own, called here exactly as
	// `ImportProject`, `ImportMemory`, `ImportTask` and `ImportDecision` call them
	// (#813). That is the whole of the change and the whole of the guarantee: an
	// export cannot exit 0 with a record its own importer will refuse, because the
	// two ask one function rather than two that have to agree. #796 closed this for
	// the id-shape checks alone and the other refusals — an empty project name, a
	// credential-shaped content — still exported at 0 and failed on a restore, so a
	// backup that looked clean lost rows when it was needed.
	//
	// The project split comes first and its CHILDREN follow it, because a
	// project left out of the artifact takes every record naming it with it: the
	// importer resolves each record's project against the artifact, so a memory
	// under an absent project is rejected as project-not-found. Dropping the
	// project alone would therefore trade one unimportable record for a whole
	// project's worth, while naming the child costs one extra report line.
	skipped := make([]SkippedRecord, 0)
	droppedProjects := make(map[string]bool)
	keptProjects := make([]memory.PortableProject, 0, len(selected))
	for _, p := range selected {
		if reason, secret := exportRefusal(memory.CheckImportedProject(p)); reason != "" {
			skipped = append(skipped, SkippedRecord{Type: TypeProject, ID: p.ID, Reason: reason, Secret: secret})
			droppedProjects[p.ID] = true
			continue
		}
		keptProjects = append(keptProjects, p)
	}
	orphaned := func(projectID string) bool { return droppedProjects[projectID] }
	const orphanReason = "the project it names was left out of this artifact"

	keptMemories := make([]memory.PortableMemory, 0, len(memories))
	for _, m := range memories {
		reason, secret := "", false
		switch {
		case orphaned(m.ProjectID):
			reason = orphanReason
		default:
			reason, secret = exportRefusal(memory.CheckImportedMemory(m))
		}
		if reason != "" {
			skipped = append(skipped, SkippedRecord{Type: TypeMemory, ID: m.ID, Reason: reason, Secret: secret})
			continue
		}
		keptMemories = append(keptMemories, m)
	}
	keptTasks := make([]memory.Task, 0, len(tasks))
	for _, t := range tasks {
		reason, secret := "", false
		switch {
		case orphaned(t.ProjectID):
			reason = orphanReason
		default:
			reason, secret = exportRefusal(memory.CheckImportedTask(t))
		}
		if reason != "" {
			skipped = append(skipped, SkippedRecord{Type: TypeTask, ID: t.ID, Reason: reason, Secret: secret})
			continue
		}
		keptTasks = append(keptTasks, t)
	}
	keptDecisions := make([]memory.Decision, 0, len(decisions))
	for _, d := range decisions {
		reason, secret := "", false
		switch {
		case orphaned(d.ProjectID):
			reason = orphanReason
		default:
			reason, secret = exportRefusal(memory.CheckImportedDecision(d))
		}
		if reason != "" {
			skipped = append(skipped, SkippedRecord{Type: TypeDecision, ID: d.ID, Reason: reason, Secret: secret})
			continue
		}
		keptDecisions = append(keptDecisions, d)
	}

	stats := Stats{
		Projects:  len(keptProjects),
		Memories:  len(keptMemories),
		Tasks:     len(keptTasks),
		Decisions: len(keptDecisions),
	}
	if len(skipped) > 0 {
		stats.Skipped = skipped
	}
	bw := bufio.NewWriter(w)
	write := func(r record) error {
		b, err := json.Marshal(r)
		if err != nil {
			return fmt.Errorf("encode %s record: %w", r.Type, err)
		}
		if _, err := bw.Write(append(b, '\n')); err != nil {
			return fmt.Errorf("write %s record: %w", r.Type, err)
		}
		return nil
	}
	if _, err := bw.Write(append(headerLine(), '\n')); err != nil {
		return Stats{}, fmt.Errorf("write header: %w", err)
	}
	for i := range keptProjects {
		p := keptProjects[i]
		if err := write(record{Type: TypeProject, Project: &p}); err != nil {
			return Stats{}, err
		}
	}
	for i := range keptMemories {
		m := keptMemories[i]
		if err := write(record{Type: TypeMemory, Memory: &m}); err != nil {
			return Stats{}, err
		}
	}
	for i := range keptTasks {
		t := keptTasks[i]
		if err := write(record{Type: TypeTask, Task: &t}); err != nil {
			return Stats{}, err
		}
	}
	for i := range keptDecisions {
		d := keptDecisions[i]
		if err := write(record{Type: TypeDecision, Decision: &d}); err != nil {
			return Stats{}, err
		}
	}
	if err := bw.Flush(); err != nil {
		return Stats{}, fmt.Errorf("flush artifact: %w", err)
	}
	return stats, nil
}

// SkippedRecord is one record an export left out of the artifact, and why.
//
// The exporter applies the IMPORTER's own refusal predicates
// (`memory.CheckImportedProject`, `CheckImportedMemory`, `CheckImportedTask`,
// `CheckImportedDecision`), because a store can already hold a record this build
// refuses to import: written by a pre-#791 `ghost import`, reinstated by
// `RestoreSnapshot`, seeded by another tool, or edited by hand. Exporting one
// produced an artifact that `ghost import` then rejected record by record, so the
// backup was not a backup — and the operator never learned it until they needed
// it. #796 closed that for the id-shape checks; #813 closed it for every refusal
// the importers make about a record, so a new one cannot be missed here.
//
// It calls the store's predicates rather than restating a class, and that is the
// whole argument for the shape: a filter written HERE drifts, and the drift is
// invisible until a restore rejects a file the operator believes is good.
//
// A memory's TAGS are the one field where "export what the importer accepts" is
// right for the wrong reason and wrong for the right one, and the reasoning is on
// `CheckImportedMemory` in `internal/memory/import_check.go` — where the omission
// is deliberate and is stated as an omission rather than left to be found.
//
// Leaving the record OUT is the only honest option for a refusal Ghost cannot
// repair. A different id is a different row: `memory_links`, the recorded history
// and every `ghost history` read are attached to the id this store holds, so
// re-keying one would orphan all of them. So the id is named and the operator
// decides what to do with the row.
//
// ID is the RAW id, and a caller MUST render it through `assemble.Token` before
// printing it — the same obligation `RecordResult.ID` carries. An id carrying a
// newline would otherwise forge a report line on the very report that names it.
type SkippedRecord struct {
	Type   string
	ID     string
	Reason string
	// Secret marks a record the CREDENTIAL guard refused, which is a different
	// kind of news from every other reason here and gets its own advice from the
	// report: the row is refused on import BY DESIGN rather than being a value
	// Ghost happens to judge malformed, and the way out is to edit the field the
	// Reason names. The caller uses it to name that command — see
	// `reportSkippedRecords`.
	//
	// It carries nothing about the credential. The Reason names the field and
	// nothing else, and there is no second copy of the value anywhere in this
	// struct: a report built from a SkippedRecord must not be able to print one.
	Secret bool
}

// Stats is what an export wrote, and what it deliberately left out.
type Stats struct {
	Projects  int
	Memories  int
	Tasks     int
	Decisions int
	// Skipped holds every record left out, in the order the artifact would have
	// written them: projects, then memories, tasks and decisions. It is empty for
	// an ordinary store, and a caller that reports an export must say so when it
	// is not — a count of what was written is not a count of what exists.
	Skipped []SkippedRecord
}

// exportRefusal renders one refusal from a CheckImported* predicate for the
// export report: the reason to print beside the record, and whether it was the
// credential guard. It takes the predicate's OWN error rather than the record,
// so it cannot re-judge anything and cannot disagree with the importer about
// which records are refused — it only decides how to phrase what the predicate
// already decided.
//
// The phrasing is the importer's message, prefixed with who is refusing, because
// the reader is looking at an export and the sentence they need to hear is that
// this artifact is not the whole store and why. Spelling the rules out a second
// time here is what drifted before: a second phrasing is a second thing to keep in
// step, and the version #796 shipped is a documented admission that it had.
//
// A CREDENTIAL refusal is the one case that gets its own sentence, for two
// reasons. It is not a malformed value to be tidied but a design decision — Ghost
// never stores a credential — and it is the only case with a different way out
// (edit the field, don't delete the row). And its message must not be the one
// printed: `SecretContentError` names the field and the detected FORMAT, which is
// a label rather than a value, but the surrounding sentence is advice aimed at a
// caller about to EDIT the artifact, and an operator reading an export report has
// no artifact to edit. So the line says which field, says the refusal is by
// design, and stops. The value is in neither the message nor the struct.
func exportRefusal(err error) (reason string, secret bool) {
	if err == nil {
		return "", false
	}
	var refused *memory.SecretContentError
	if errors.As(err, &refused) {
		return fmt.Sprintf("its %s is credential-shaped, and ghost import refuses to store one by design — Ghost never stores a credential value",
			refused.Field), true
	}
	return "ghost import refuses it: " + err.Error(), false
}

// selectProjects filters an already-read project list by id or exact name.
// Matching is on the id or the name and nothing else — no path, no remote, no
// basename fallback — because an export filter is a choice about what to copy,
// and a filter that resolved like project resolution would select a different
// project on another machine than the one named here.
func selectProjects(projects []memory.PortableProject, filter string) ([]memory.PortableProject, error) {
	if filter == "" {
		return projects, nil
	}
	var out []memory.PortableProject
	for _, p := range projects {
		if p.ID == filter || p.Name == filter {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no project matches %q", filter)
	}
	return out, nil
}

// exportListLimit caps each per-project list read. A list that comes back
// exactly this long may have been cut by the store, and an export that silently
// dropped the tail of a project's tasks would be a backup that lies about its
// own contents — so the limit is reported rather than absorbed.
const exportListLimit = 1000000

// selectTasks reads the tasks of the given projects in id order.
func selectTasks(ctx context.Context, s *memory.Store, projectIDs []string) ([]memory.Task, error) {
	var out []memory.Task
	for _, id := range projectIDs {
		list, err := s.ListTasks(ctx, id, "", exportListLimit)
		if err != nil {
			return nil, fmt.Errorf("export tasks for %s: %w", id, err)
		}
		if len(list) >= exportListLimit {
			return nil, fmt.Errorf("export tasks for %s: list hit the %d limit — refusing to write a partial artifact", id, exportListLimit)
		}
		out = append(out, list...)
	}
	sortTasksByID(out)
	return out, nil
}

// selectDecisions reads the decisions of the given projects in id order.
func selectDecisions(ctx context.Context, s *memory.Store, projectIDs []string) ([]memory.Decision, error) {
	var out []memory.Decision
	for _, id := range projectIDs {
		list, err := s.ListDecisions(ctx, id, "", exportListLimit)
		if err != nil {
			return nil, fmt.Errorf("export decisions for %s: %w", id, err)
		}
		if len(list) >= exportListLimit {
			return nil, fmt.Errorf("export decisions for %s: list hit the %d limit — refusing to write a partial artifact", id, exportListLimit)
		}
		out = append(out, list...)
	}
	sortDecisionsByID(out)
	return out, nil
}
