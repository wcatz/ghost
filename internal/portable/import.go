package portable

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode"

	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/secret"
)

// ImportOptions is one import run's settings. It is an alias rather than a
// second struct because the store owns these semantics — it is the store that
// downgrades provenance and that decides what a dry run may check — and a
// separate type here would be a second place for the two to drift.
type ImportOptions = memory.ImportOptions

// Action is what an import did, or would do, with one record.
type Action string

const (
	// ActionCreate: the record was inserted, or would be on an apply run.
	ActionCreate Action = "create"
	// ActionSkip: the record's id is already in the store, so it was left
	// alone. An import never overwrites: the artifact is a copy to be restored
	// from, and whatever is already there is the newer of the two.
	ActionSkip Action = "skip"
	// ActionReject: the record could not be imported and was not written.
	// The reason is in Error.
	ActionReject Action = "reject"
)

// RecordResult is the outcome for one record of an import, reported to the
// caller's onRecord so the CLI can print it. Line is the artifact line number
// (1-based, counting the header), so a rejection can be found and fixed in the
// file rather than hunted for.
type RecordResult struct {
	Type   string
	ID     string
	Line   int
	Action Action
	// Detail is a short human description of the record — a memory's content
	// prefix, a task or decision title, a project's name — so a report line
	// identifies the record without the reader having to look up its id.
	Detail string
	// Clamped names the fields whose text a cap cut on the way in, in the
	// record's own field order — `content` for a memory, and for a task or a
	// decision any of title, description, notes, decision, rationale and
	// alternatives. Empty is the ordinary case, and a cut the reader cannot see
	// in the artifact afterwards is the reason the names are here rather than a
	// flag: ClampContent's marker is in a stored body, but a cut title and the
	// alternatives dropped past the twentieth leave nothing behind at all. It is
	// set for a create, including a dry run's, because the cut is what the user
	// needs to know about before applying (#1232).
	Clamped []string
	// Downgraded reports that the memory's own source, pin state and retention
	// tier were replaced, because the run was not told to trust the artifact's
	// provenance (see memory.ImportOptions.TrustProvenance). It is set for a dry
	// run's create too, for the same reason Clamped is: the rewrite is the thing
	// to read the preview for.
	Downgraded bool
	// DroppedRef is the id a task's blocked_by or a decision's superseded_by
	// named when the pointer was dropped: its target was rejected by this run,
	// is in neither the artifact nor the store, lands in another project than
	// the record, or the pointer is part of a cycle (DropReason says which). A
	// target the store already holds, or this run lands, in the record's own
	// project is never a drop. The record itself is still imported, without the
	// pointer; this is how the report says what it lost. Set for a create, a dry
	// run's included, so the preview shows the drop the apply will make.
	DroppedRef string
	// DropReason says why DroppedRef was dropped. Its zero value is
	// DropRejected, the reason the field was first added for.
	DropReason DropReason
	Error      error
}

// DropReason is why an import dropped a task's blocked_by or a decision's
// superseded_by. Each is a pointer that could not be honoured without refusing
// the record holding it, so the record lands without the pointer and the report
// says which of these it was.
type DropReason int

const (
	// DropRejected: the record the pointer names is in the artifact, but this
	// run rejected it.
	DropRejected DropReason = iota
	// DropAbsent: the record the pointer names is neither in the artifact nor
	// already in the store, or it is, but in another project than the record
	// holding the pointer, which leaves nothing that pointer can follow.
	DropAbsent
	// DropCycle: the pointer is part of a cycle of pointers, which no order of
	// writes can satisfy.
	DropCycle
)

// ImportReport summarises one import run, counted per record type.
type ImportReport struct {
	// Applied reports whether anything was written. A dry run reports false and
	// still fills Created with what it would have created.
	Applied bool
	Created map[string]int
	Skipped map[string]int
	// Rejected is the total across types; Errors holds the detail.
	Rejected int
	Errors   []error
}

func (r *ImportReport) count(m map[string]int, kind string) {
	if m == nil {
		return
	}
	m[kind]++
}

// Import reads an artifact and, when apply is true, inserts the records the
// store does not already have.
//
// apply=false is the default everywhere it is offered. It is not a separate
// code path: every record goes through the same store method with apply=false,
// so a dry run validates exactly what an apply run would and cannot disagree
// with it about which records would be created, skipped or rejected.
//
// A record whose id is already present is skipped, never updated. The artifact
// is the older of the two copies by construction — it was exported before
// whatever the store has done since — so overwriting would be restoring stale
// data over live data, and the command a user runs to be safe would be the one
// thing that loses their work. Re-running an import is therefore always safe,
// which is what makes it the repair after a rejected record.
//
// A record that cannot be imported is rejected and the run continues: one
// hand-edited line must not abandon the rest of a file that may hold ten
// thousand good records. The rejections are counted and reported, and the
// caller is expected to exit non-zero, so a partial import cannot be mistaken
// for a complete one.
//
// A file that is not a readable artifact at all — no header, an unknown schema
// version, a second header, an unknown record type, a line whose payload is not
// exactly the one its type names — is refused before any record is written.
// Those are not per-record problems: a missing header means nobody checked the
// version, and applying the rest of a file whose format is unknown would be
// importing data whose meaning this build is guessing at. A line holding a
// second payload is one of them for the same reason: which of the two the line
// meant is a guess, and importing the one its type names drops the other.
//
// A line that is not JSON is a different thing, and is treated as one. A file
// can hold an unparseable line because it was truncated, merged badly, or
// hand-edited, and the records around it are still good — so the line is
// rejected on its own, counted, and named by line number, and the rest of the
// file imports. That is what the line-oriented format buys: the readable prefix
// of a damaged artifact is still worth having, and the rejected line is reported
// rather than swallowed.
func Import(ctx context.Context, s *memory.Store, r io.Reader, opts ImportOptions, onRecord func(RecordResult)) (ImportReport, error) {
	report := ImportReport{
		Applied: opts.Apply,
		Created: map[string]int{},
		Skipped: map[string]int{},
	}

	recs, unreadable, err := readRecords(r)
	if err != nil {
		return report, err
	}

	// A dry run has to project the records onto the store in the same order an
	// apply run would, because "would this be created" depends on what earlier
	// records create — a project, most of all. Projecting the whole set first
	// and then replaying it means one ordering, used by both modes.
	plan, err := planRecords(recs, s, ctx, opts)
	if err != nil {
		return report, err
	}
	// The unreadable lines are reported first, before any record is applied, so
	// the reader sees every one of them at the top rather than after the run
	// that skipped them.
	for _, bad := range unreadable {
		report.Rejected++
		report.Errors = append(report.Errors, bad.err)
		if onRecord != nil {
			onRecord(bad.result)
		}
	}
	// The ids this run creates, which is what lets a dry run answer a repeated id
	// the way the apply run it previews does. See createdIDs.
	created := createdIDs{}
	for _, step := range plan {
		step.run(ctx, s, &report, onRecord, created)
	}
	return report, nil
}

// unreadable is one line the parser refused: either it is not JSON, or it is a
// record whose memory id is a shape this build will not store (#791). Both are
// per-line refusals for the same reason and with the same consequence — the rest
// of the file still imports — and neither names the offending id, because a line
// that forges a rendering must not be reprinted by the report that names it.
type unreadable struct {
	line   int
	result RecordResult
	err    error
}

// readRecords parses an artifact into its records, refusing a file that is not a
// readable one and collecting the individual lines it could not parse.
//
// The two outcomes are deliberately different. A file-level problem — no header,
// an unknown schema version — is returned as an error that stops the run before
// anything is written. A per-LINE problem is collected and returned alongside the
// records: the rest of the file is still readable, and Import reports the line and
// carries on. There are two of them today — a line that is not JSON, and a
// memory record whose id `memory.CheckImportedID` refuses — and they share the
// treatment because they share the property that makes it right: one damaged
// record must not abandon a file that may hold ten thousand good ones.
func readRecords(r io.Reader) ([]parsedRecord, []unreadable, error) {
	// The artifact does not bound a record's length, so the reader must not put
	// a fixed ceiling on a line. `content` is capped at MaxContentLen, but a
	// memory's `evidence` list has no bound — one row is appended every time the
	// same fact is saved again (see Store.Upsert's fold branch) — so an
	// exporter's own output can pass any ceiling derived from the content cap
	// (#1084). A fixed cap refused the exporter's own artifact as a whole file,
	// before any record was read, so the one bloated record left every other
	// record in the backup unimportable. bufio.Reader grows its buffer to fit the
	// line instead: what the exporter can write, the importer reads.
	reader := bufio.NewReaderSize(r, 64*1024)

	var (
		recs    []parsedRecord
		unread  []unreadable
		header  bool
		lineNum int
	)
	for {
		raw, readErr := reader.ReadBytes('\n')
		// A read that did not end on the delimiter is a partial line: on any
		// error other than EOF there is no line to judge, so the stream error is
		// returned and the read stops. EOF with no bytes left is the end, while
		// EOF with a final unterminated line still yields that line below — the
		// same line bufio.ScanLines delivered.
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, nil, fmt.Errorf("read artifact: %w", readErr)
		}
		if len(raw) == 0 {
			break
		}
		lineNum++
		// TrimRight rather than one delimiter: ReadBytes yields the line ending,
		// and "\r\n" is the Windows spelling of the same ending ScanLines
		// dropped a "\r" from.
		line := bytes.TrimRight(raw, "\r\n")
		if len(strings.TrimSpace(string(line))) == 0 {
			// A blank line is skipped rather than rejected: a file that has
			// been through an editor may have them, and there is nothing to
			// import in one. Line numbers still count them, so an error names
			// the line a user sees in their editor.
			continue
		}
		var rec record
		if err := json.Unmarshal(line, &rec); err != nil {
			// Not JSON. The line is rejected on its own and the rest of the file
			// carries on, which is the whole reason the format is line-oriented:
			// a truncated or hand-mangled artifact still yields every record
			// before and after the damage. Before the header it is a different
			// matter — there is no version to trust, so nothing is imported.
			if !header {
				return nil, nil, fmt.Errorf("line %d: %w, and the line is not a record: %v", lineNum, errNoHeader, err)
			}
			unread = append(unread, unreadable{
				line: lineNum,
				result: RecordResult{
					Line:   lineNum,
					Action: ActionReject,
					Detail: "unreadable line",
					Error:  err,
				},
				err: fmt.Errorf("line %d: not a valid ghost artifact record: %w", lineNum, err),
			})
			continue
		}
		// A repeated top-level key, refused as a file before the line is judged
		// any other way. See repeatedKey.
		if key := repeatedKey(line); key != "" {
			return nil, nil, fmt.Errorf("line %d: the record repeats the %s key — a line holds each key once, and only the last copy would be read, so the file is refused rather than imported with the earlier copy dropped", lineNum, memory.RefusalArg("key", key))
		}
		if !header {
			if rec.Type != TypeHeader {
				return nil, nil, fmt.Errorf("line %d: %w — the first line must be %q, so the schema version is checked before any record is read", lineNum, errNoHeader, TypeHeader)
			}
			if err := checkSchemaVersion(rec.SchemaVersion); err != nil {
				return nil, nil, fmt.Errorf("line %d: %w", lineNum, err)
			}
			if err := checkHeaderFeatures(rec); err != nil {
				return nil, nil, fmt.Errorf("line %d: %w", lineNum, err)
			}
			if extra := otherPayloads(rec, TypeHeader); extra != "" {
				return nil, nil, fmt.Errorf("line %d: the %q line also carries %s — a header holds the schema version and its declared features", lineNum, TypeHeader, extra)
			}
			header = true
			continue
		}
		if rec.Type == TypeHeader {
			return nil, nil, fmt.Errorf("line %d: a second %q line — an artifact has exactly one header", lineNum, TypeHeader)
		}
		// The memory id's SHAPE, refused here rather than at the store
		// (#791). The store refuses it too — `ImportMemory` is the write
		// boundary and has to hold for any caller — but refusing at PARSE time
		// is what keeps the value out of this package's own report: every
		// rejected record's id is echoed into a per-record line and into
		// `labelOrID`, and a stored id is printed inside backticks and OUTSIDE
		// the «...» data delimiters on every assembled surface, so an id holding
		// a newline forges a line that reads as Ghost's own memory row. A record
		// refused here never becomes a parsedRecord, so there is no id left to
		// print.
		//
		// It is the same treatment an unparseable line gets, for the same
		// reason: one damaged line must not abandon a file that may hold ten
		// thousand good records, and the rejection is counted and named by line
		// number so a partial import cannot be mistaken for a complete one.
		// `memory.CheckImportedID` is the store's own function rather than a
		// second copy, because a parser judging ids slightly differently from
		// the store would classify a dry run differently from the apply run it
		// previews — the one property this package is built around.
		//
		// Only a memory line carrying nothing else is judged here. A line of
		// another type that also carries a memory, or a memory line with a second
		// payload, is a malformed file whatever its memory id says, and refusing
		// it by line number here would let the rest of the file import around it;
		// requirePayload refuses it as a file instead, naming kinds and no id.
		if rec.Memory != nil && rec.Type == TypeMemory && otherPayloads(rec, TypeMemory) == "" {
			if err := memory.CheckImportedID(rec.Memory.ID); err != nil {
				unread = append(unread, unreadable{
					line: lineNum,
					result: RecordResult{
						Type:   TypeMemory,
						Line:   lineNum,
						Action: ActionReject,
						Detail: "memory id this build will not store",
						Error:  err,
					},
					err: fmt.Errorf("line %d: %s: %w", lineNum, TypeMemory, err),
				})
				continue
			}
		}
		// The same for an EDGE's endpoints, and for the same reason: the memory
		// that makes the claim is the edge's id in the report, and the memory it
		// is about is printed beside it. Both are memory ids, and both are
		// refused by the same function the store refuses them with.
		if rec.Link != nil && rec.Type == TypeLink && otherPayloads(rec, TypeLink) == "" {
			if err := memory.CheckImportedID(rec.Link.SourceID); err != nil {
				unread = append(unread, unreadable{
					line: lineNum,
					result: RecordResult{
						Type:   TypeLink,
						Line:   lineNum,
						Action: ActionReject,
						Detail: "edge id this build will not store",
						Error:  err,
					},
					err: fmt.Errorf("line %d: %s: %w", lineNum, TypeLink, err),
				})
				continue
			}
			if err := memory.CheckImportedID(rec.Link.TargetID); err != nil {
				unread = append(unread, unreadable{
					line: lineNum,
					result: RecordResult{
						Type:   TypeLink,
						Line:   lineNum,
						Action: ActionReject,
						Detail: "edge id this build will not store",
						Error:  err,
					},
					err: fmt.Errorf("line %d: %s: %w", lineNum, TypeLink, err),
				})
				continue
			}
		}
		recs = append(recs, parsedRecord{rec: rec, line: lineNum})
	}
	if !header {
		if lineNum == 0 {
			return nil, nil, fmt.Errorf("%w — there is nothing to import", errNoHeader)
		}
		return nil, nil, fmt.Errorf("%w — the first line must be %q", errNoHeader, TypeHeader)
	}
	return recs, unread, nil
}

// errNoHeader marks a file that is not an artifact at all, as opposed to one
// carrying a record this build cannot use. The two are reported differently
// because the second is fixable in the file and the first means the user picked
// the wrong file.
var errNoHeader = errors.New("not a ghost artifact (no schema-version header)")

// checkHeaderFeatures refuses an artifact whose header declares a feature this
// build does not understand. A newer artifact's fields mean
// whatever this build guesses, and importing it would store a weaker row while
// reporting that provenance was kept. A header that declares nothing is accepted,
// so an artifact written before this mechanism existed still imports.
func checkHeaderFeatures(rec record) error {
	for _, f := range rec.Features {
		if !knownFeatures[f] {
			return fmt.Errorf("header declares feature %s that this ghost build does not understand", memory.RefusalArg("feature", f))
		}
	}
	return nil
}

// checkSchemaVersion refuses any version outside the range this build reads. A
// newer artifact's fields mean whatever this build guesses; an older one is
// refused unless its rules still hold, which is why the range is named on both
// sides rather than being "everything below SchemaVersion". Both are named in
// the error so the user knows whether to upgrade ghost or re-export.
func checkSchemaVersion(version int) error {
	if version >= minReadableSchemaVersion && version <= SchemaVersion {
		return nil
	}
	if version > SchemaVersion {
		return fmt.Errorf("schema version %d is newer than this ghost build reads (v%d) — upgrade ghost, or re-export with the version that wrote it", version, SchemaVersion)
	}
	return fmt.Errorf("schema version %d is older than this ghost build reads (v%d and later) — re-export the artifact with this build", version, minReadableSchemaVersion)
}

// parsedRecord is one record plus the artifact line it came from.
type parsedRecord struct {
	rec  record
	line int
	// droppedRef and dropReason are set by orderTasks / orderDecisions when
	// ordering cleared the record's pointer, so the step that writes it can
	// report the drop — or restore the pointer if the store already holds its
	// target.
	droppedRef string
	dropReason DropReason
}

// id returns the record's id, for the per-record report. A record with no
// payload has no id, and the empty string is what the report prints alongside
// the line number that identifies it.
//
// An edge of the graph has no id of its own — the table's key is the triple
// (source, target, relation) — so the report names it by the memory that makes
// the claim, which is the half of it a reader needs to follow. The other half
// is in the record's detail.
func (p parsedRecord) id() string {
	switch {
	case p.rec.Project != nil:
		return p.rec.Project.ID
	case p.rec.Memory != nil:
		return p.rec.Memory.ID
	case p.rec.Task != nil:
		return p.rec.Task.ID
	case p.rec.Decision != nil:
		return p.rec.Decision.ID
	case p.rec.Link != nil:
		return p.rec.Link.SourceID
	}
	return ""
}

// step is one record's planned import. Planning separates "what will happen"
// from "do it", which is what lets a dry run and an apply run share every
// decision: the plan is built the same way for both, and apply is the only
// difference in the run step.
type step struct {
	rec     parsedRecord
	kind    string
	key     string
	detail  string
	check   *projectCheck
	opts    ImportOptions
	runFunc func(ctx context.Context, s *memory.Store, opts ImportOptions) outcome
}

// stepKey is the identity the run's created-ids set knows this step by. It is
// the record's id for the four kinds that have one, and the edge's own key for
// a link: the table's primary key is the triple (source, target, relation), so
// two edges between the same pair of memories are two edges when their
// relations differ, and a set keyed on the source alone would tell them apart
// wrongly.
func (st step) stepKey() string {
	if st.key != "" {
		return st.key
	}
	return st.rec.id()
}

// outcome is what running one step produced. A struct rather than a tuple
// because a step's result has grown past what a return line can carry legibly:
// what happened, which fields a cap cut, and whether the memory's provenance
// was downgraded on the way in.
type outcome struct {
	action     Action
	clamped    []string
	downgraded bool
	droppedRef string
	dropReason DropReason
	err        error
}

// landedElsewhere is the project planRecords records for a skipped task or
// decision whose stored row is not in the project the artifact's record is
// imported under. A project id holds no control character
// (memory.CheckImportedProjectID), so this never equals the project of the
// record that points at it.
const landedElsewhere = "\x00elsewhere"

// planRecords turns the parsed records into an ordered list of steps, in the
// order an apply run must write them: projects first, then memories, then tasks,
// then decisions.
//
// The order is not cosmetic. A memory names a project, and the store refuses a
// record whose project is absent — so an artifact whose project line came after
// its memories would reject every one of them, and a file reordered by a text
// editor or a merge would import as a partial store. Within each kind the
// artifact's own order is kept, which is already id order.
//
// The plan also resolves the self-references: a task's blocked_by and a
// decision's superseded_by can only be honoured if the row they point at is
// written first, so those are ordered topologically. A reference to a record the
// artifact does not contain, or a cycle, has its pointer dropped rather than
// failing the import — an artifact that cannot be read in full is still worth
// importing as far as it goes. A reference to a record the artifact contains but
// this run rejects, or that lands in another project than the dependent, is
// dropped the same way, as the steps run. Every such drop is named on the
// dependent record's report line (RecordResult.DroppedRef, with DropReason
// saying why), and a pointer whose target the store already held before the run,
// in the record's own project, is kept rather than dropped, because it resolves.
//
// Each child step is given the set of project ids its project will exist under
// by the time it runs: what the store already holds plus what this artifact's
// own project records have created by then. That set is what lets a dry run
// classify a child record exactly as the apply run it previews will, even though
// the dry run has created nothing.
func planRecords(recs []parsedRecord, s *memory.Store, ctx context.Context, opts ImportOptions) ([]step, error) {
	var (
		projects  []parsedRecord
		memories  []parsedRecord
		links     []parsedRecord
		tasks     []parsedRecord
		decisions []parsedRecord
	)
	for _, p := range recs {
		switch p.rec.Type {
		case TypeProject:
			projects = append(projects, p)
		case TypeMemory:
			memories = append(memories, p)
		case TypeLink:
			links = append(links, p)
		case TypeTask:
			tasks = append(tasks, p)
		case TypeDecision:
			decisions = append(decisions, p)
		default:
			// Refused rather than skipped. A record type this build does not
			// know is a file from a newer build or a different format, and
			// dropping it would import a store that is missing data the file
			// plainly contains.
			return nil, fmt.Errorf("line %d: unknown record type %s — this ghost build reads %q, %q, %q, %q and %q records",
				p.line, memory.RefusalArg("type", p.rec.Type), TypeProject, TypeMemory, TypeTask, TypeDecision, TypeLink)
		}
		if err := requirePayload(p); err != nil {
			return nil, err
		}
	}

	// What the store already holds, plus what this artifact adds as its project
	// records are applied. The two are the same set by the time the children
	// run, which is the point: the plan is built once and used by both modes.
	known, err := liveProjectIDs(ctx, s)
	if err != nil {
		return nil, err
	}
	// Which project id each of the artifact's projects will live under here. An
	// id the store already holds maps to itself; one that only collides on
	// checkout or repository maps to the project that holds it; one that is
	// genuinely new maps to itself and is created.
	local, err := resolveProjectMapping(ctx, s, projects)
	if err != nil {
		return nil, err
	}
	// A child record's project is looked up through the mapping, so a record that
	// follows its project onto a project this store already had still finds it.
	// An id with no mapping is one the artifact does not carry a project record
	// for, and is passed through so the check below rejects it by name.
	under := func(projectID string) string {
		if mapped, ok := local[projectID]; ok {
			return mapped
		}
		return projectID
	}
	checkFor := func(projectID string) *projectCheck {
		return &projectCheck{projectID: under(projectID), available: func(id string) bool { return known[id] }}
	}

	steps := make([]step, 0, len(recs))
	for _, p := range projects {
		project := p.rec.Project
		adopted := local[project.ID]
		detail := project.Name
		if adopted != project.ID {
			// The artifact's project id is unknown here, but the checkout or the
			// repository is not: this store already has that project, and its id
			// is the one the records have to land under. Saying so in the report
			// line is the difference between "already imported" and "attached to
			// the project you already had".
			detail = fmt.Sprintf("%s → %s (this store already records that checkout or repository)",
				project.Name, adopted)
			if !known[adopted] {
				// Not in the store: an earlier project of this artifact
				// claimed the checkout or repository. known is still the
				// store's own set here, as no step has run yet.
				detail = fmt.Sprintf("%s → %s (an earlier project in this artifact records that checkout or repository)",
					project.Name, adopted)
			}
		}
		adoptedElsewhere := adopted != project.ID
		steps = append(steps, step{rec: p, kind: TypeProject, detail: safeDetail(detail, detail, project.ID), opts: opts,
			runFunc: func(ctx context.Context, s *memory.Store, opts ImportOptions) outcome {
				if adoptedElsewhere {
					// Nothing to write, and nothing that could be: the id belongs
					// to a project this store already has.
					return outcome{action: ActionSkip}
				}
				created, err := s.ImportProject(ctx, *project, opts.Apply)
				if err != nil {
					return outcome{action: ActionReject, err: err}
				}
				// A project this artifact adds is available to the children
				// below it whether or not it was actually written, which is
				// what makes a dry run's classification match the apply run's.
				known[project.ID] = true
				return outcome{action: actionFor(created)}
			}})
	}
	// The memories this run lands, and the edges that join them.
	//
	// landedMemories holds every memory id that is in the store once the memory
	// steps have run: one this run created, and one the store already held (a
	// skip). It is the answer to "is this endpoint there?", and it is answered
	// the same way in a dry run and in the apply it previews — the dry run's
	// memory step reports the create it would make, so the edge below is
	// classified the same way in both modes.
	landedMemories := map[string]bool{}
	for _, p := range memories {
		m := p.rec.Memory
		m.ProjectID = under(m.ProjectID)
		steps = append(steps, step{rec: p, kind: TypeMemory, detail: safeDetail(assemble.PreviewLine(m.Content, contentPreviewMax), m.Content, m.ID), check: checkFor(m.ProjectID), opts: opts,
			runFunc: func(ctx context.Context, s *memory.Store, opts ImportOptions) outcome {
				created, clamped, downgraded, err := s.ImportMemory(ctx, *m, opts)
				if err != nil {
					return outcome{action: ActionReject, err: err}
				}
				// The memory is there now — written by this run, or already in the
				// store — so an edge naming it can be written below.
				landedMemories[m.ID] = true
				return outcome{action: actionFor(created), clamped: contentClamped(clamped), downgraded: downgraded}
			}})
	}
	// The edges of the graph, after the memories they join.
	//
	// An edge is written only when BOTH its endpoints are memories this run
	// lands. An edge naming a memory the artifact does not carry — or one the
	// run refused — is refused rather than written against whatever the store
	// happens to hold under that id: the artifact is a copy of a graph whose
	// endpoints it holds, and an edge against a memory nobody landed is a
	// relationship the file does not describe. The refusal is per record, so the
	// rest of the file still imports — and the run says so, which is what keeps a
	// partial import from reading as a complete one.
	for _, p := range links {
		l := *p.rec.Link
		steps = append(steps, step{rec: p, kind: TypeLink, key: linkKey(&l), detail: linkDetail(&l), opts: opts,
			runFunc: func(ctx context.Context, s *memory.Store, opts ImportOptions) outcome {
				if !landedMemories[l.SourceID] {
					return outcome{action: ActionReject, err: fmt.Errorf(
						"the edge's source %s is not a memory this import lands — an edge joins two memories the artifact holds, and this one names a memory that was refused or is not in the artifact",
						memory.RefusalArg("source_id", l.SourceID))}
				}
				if !landedMemories[l.TargetID] {
					return outcome{action: ActionReject, err: fmt.Errorf(
						"the edge's target %s is not a memory this import lands — an edge joins two memories the artifact holds, and this one names a memory that was refused or is not in the artifact",
						memory.RefusalArg("target_id", l.TargetID))}
				}
				created, err := s.ImportLink(ctx, l, opts.Apply)
				if err != nil {
					return outcome{action: ActionReject, err: err}
				}
				return outcome{action: actionFor(created)}
			}})
	}
	// Which of the artifact's tasks and decisions have landed — been created, or
	// found already present — by the time a later step runs, and the project each
	// one landed in. orderTasks and orderDecisions leave a pointer only when it
	// names a record of the artifact that is ordered before the one holding it,
	// so a target that has not landed is one this run rejected. Such a pointer
	// can never resolve, exactly like one naming a record the artifact does not
	// contain, and is dropped the same way rather than taking its record down
	// with it.
	//
	// A target that landed in a project other than the dependent's is dropped
	// too, and reported like one that exists nowhere (DropAbsent), for the
	// reason HasTask gives: the store's readers follow these pointers only within
	// one project, and deleting the other project would rewrite the dependent.
	// A created record lands in the project it is imported under, after mapping.
	// A skipped one is the row the store already holds, which can live in
	// another project than the artifact names, so its project is read back:
	// landedElsewhere stands for any project but the one it was imported under.
	// The restore below then keeps the pointer whenever that stored row is in
	// the dependent's own project after all.
	//
	// The set is filled as steps run, in both modes, and it is the dry run's
	// stand-in for the store's own pre-write check of the pointer, which only an
	// apply can make (the target of a dry run is never written). It is what keeps
	// the preview and the apply agreeing about the dependent record (#1147).
	landedTasks := map[string]string{}
	landedDecisions := map[string]string{}
	// Which of them this run created. A dropped pointer is restored when the
	// store already holds its target in the project the record is imported
	// under, after mapping (#1153); one in another project stays dropped, like a
	// target that exists nowhere. And only a target the store held BEFORE the
	// run: one this run wrote is there in an apply and not in the dry
	// run that previews it, so counting it would make the two disagree about a
	// cycle's pointers.
	createdTasks := map[string]bool{}
	createdDecisions := map[string]bool{}
	for _, p := range orderTasks(tasks) {
		t := p.rec.Task
		t.ProjectID = under(t.ProjectID)
		steps = append(steps, step{rec: p, kind: TypeTask, detail: safeDetail(t.Title, t.Title, t.ID), check: checkFor(t.ProjectID), opts: opts,
			runFunc: func(ctx context.Context, s *memory.Store, opts ImportOptions) outcome {
				task := *t
				dropped, reason := p.droppedRef, p.dropReason
				if task.BlockedBy != "" {
					if landed, ok := landedTasks[task.BlockedBy]; !ok {
						dropped, reason, task.BlockedBy = task.BlockedBy, DropRejected, ""
					} else if landed != task.ProjectID {
						dropped, reason, task.BlockedBy = task.BlockedBy, DropAbsent, ""
					}
				}
				if dropped != "" {
					held, err := s.HasTask(ctx, task.ProjectID, dropped)
					if err != nil {
						return outcome{action: ActionReject, err: fmt.Errorf("look up the task blocked_by names, %s: %w", memory.RefusalArg("blocked_by", dropped), err)}
					}
					if held && !createdTasks[dropped] {
						task.BlockedBy, dropped = dropped, ""
					}
				}
				created, clamped, err := s.ImportTask(ctx, task, opts.Apply)
				if err != nil {
					return outcome{action: ActionReject, err: err}
				}
				landed := task.ProjectID
				if created {
					createdTasks[task.ID] = true
				} else {
					dropped = ""
					held, err := s.HasTask(ctx, task.ProjectID, task.ID)
					if err != nil {
						return outcome{action: ActionReject, err: fmt.Errorf("look up the project of task %s: %w", memory.RefusalArg("task_id", task.ID), err)}
					}
					if !held {
						landed = landedElsewhere
					}
				}
				landedTasks[task.ID] = landed
				return outcome{action: actionFor(created), clamped: clamped, droppedRef: dropped, dropReason: reason}
			}})
	}
	for _, p := range orderDecisions(decisions) {
		d := p.rec.Decision
		d.ProjectID = under(d.ProjectID)
		steps = append(steps, step{rec: p, kind: TypeDecision, detail: safeDetail(d.Title, d.Title, d.ID), check: checkFor(d.ProjectID), opts: opts,
			runFunc: func(ctx context.Context, s *memory.Store, opts ImportOptions) outcome {
				decision := *d
				dropped, reason := p.droppedRef, p.dropReason
				if decision.SupersededBy != "" {
					if landed, ok := landedDecisions[decision.SupersededBy]; !ok {
						dropped, reason, decision.SupersededBy = decision.SupersededBy, DropRejected, ""
					} else if landed != decision.ProjectID {
						dropped, reason, decision.SupersededBy = decision.SupersededBy, DropAbsent, ""
					}
				}
				if dropped != "" {
					held, err := s.HasDecision(ctx, decision.ProjectID, dropped)
					if err != nil {
						return outcome{action: ActionReject, err: fmt.Errorf("look up the decision superseded_by names, %s: %w", memory.RefusalArg("superseded_by", dropped), err)}
					}
					if held && !createdDecisions[dropped] {
						decision.SupersededBy, dropped = dropped, ""
					}
				}
				created, clamped, err := s.ImportDecision(ctx, decision, opts.Apply)
				if err != nil {
					return outcome{action: ActionReject, err: err}
				}
				landed := decision.ProjectID
				if created {
					createdDecisions[decision.ID] = true
				} else {
					dropped = ""
					held, err := s.HasDecision(ctx, decision.ProjectID, decision.ID)
					if err != nil {
						return outcome{action: ActionReject, err: fmt.Errorf("look up the project of decision %s: %w", memory.RefusalArg("decision_id", decision.ID), err)}
					}
					if !held {
						landed = landedElsewhere
					}
				}
				landedDecisions[decision.ID] = landed
				return outcome{action: actionFor(created), clamped: clamped, droppedRef: dropped, dropReason: reason}
			}})
	}
	return steps, nil
}

// resolveProjectMapping decides, for every project record in the artifact, which
// project id its records will live under in this store.
//
// The problem it solves is the ordinary one: project ids are per-install, so an
// artifact exported on a laptop names a project the destination has never seen,
// while the destination very often has its own project for the same checkout or
// the same repository. Inserting the artifact's project would collide on
// `projects.path` or on the partial UNIQUE index over `repo_remote` — and because
// the children name the artifact's id, a single collision would take every
// memory, task and decision with it. So the id is mapped onto the project that
// already holds the checkout, and the import attaches rather than fails.
//
// The mapping is decided before any record runs, which is what lets a dry run
// and an apply run agree: both read the same map, and the dry run's "skip" for
// an adopted project is exactly what the apply run does.
//
// A project this artifact creates is held to the same rule as one the store
// already holds. Two projects of one artifact can name the same path, or
// remotes that normalize to one repository (an older release stored remotes
// raw, and a hand-merged pair of exports repeats a checkout). The apply inserts
// the first and its collision check then refuses the second, while the dry run,
// reading an unchanged store, would promise both. So the later project is
// mapped onto the earlier one, as it would be onto a store project with that
// checkout. Only a project the store would accept claims its checkout: one it
// refuses writes nothing, so the next project is judged on its own record,
// which is the rule createdIDs follows for a repeated id.
func resolveProjectMapping(ctx context.Context, s *memory.Store, projects []parsedRecord) (map[string]string, error) {
	mapping := make(map[string]string, len(projects))
	live, err := s.PortableProjects(ctx)
	if err != nil {
		return nil, fmt.Errorf("read projects for import: %w", err)
	}
	byID := make(map[string]bool, len(live))
	for _, p := range live {
		byID[p.ID] = true
	}
	// The checkouts and repositories this run's own projects will create, keyed
	// the way projectCollision compares them: the path as given, the remote
	// normalized.
	claimedPath := map[string]string{}
	claimedRemote := map[string]string{}
	// The ids whose mapping is final. A repeated id is decided by its first
	// record the store would accept: the apply skips every record after that
	// one, so a later record's path must not remap the id its children
	// follow. A refused record settles nothing, and the next record with the
	// id is judged on its own, as the apply judges it.
	settled := map[string]bool{}
	for _, rec := range projects {
		p := *rec.rec.Project
		if settled[p.ID] {
			continue
		}
		// The very same project, or nothing that collides: keep the artifact's
		// own id.
		if byID[p.ID] {
			mapping[p.ID] = p.ID
			settled[p.ID] = true
			continue
		}
		// `_global` needs no special case: ProjectForCheckout never returns it
		// (it holds every project's memories, not one checkout), so a lookup can
		// only turn up some other project that happens to record the same
		// checkout — and the store's own collision check then refuses the
		// artifact's project by name rather than letting the records attach
		// somewhere they do not belong. That is the safe outcome, and it needs no
		// branch here to reach it.
		found, _, err := s.ProjectForCheckout(ctx, p.Path, p.RepoRemote)
		if err != nil {
			return nil, err
		}
		if found == "" && p.Path != "" {
			found = claimedPath[p.Path]
		}
		remote := memory.NormalizeRepoRemote(p.RepoRemote)
		if found == "" && remote != "" {
			found = claimedRemote[remote]
		}
		if found != "" {
			mapping[p.ID] = found
			settled[p.ID] = true
			continue
		}
		mapping[p.ID] = p.ID
		// The store's own verdict, asked without writing: the same call the
		// project's step makes, so a project that claims a checkout here is
		// one both modes create.
		if created, err := s.ImportProject(ctx, p, false); err == nil && created {
			settled[p.ID] = true
			if p.Path != "" {
				claimedPath[p.Path] = p.ID
			}
			if remote != "" {
				claimedRemote[remote] = p.ID
			}
		}
	}
	return mapping, nil
}

// projectCheck is the plan-level guard on a child record's project: the record
// is refused when neither the store nor this run's earlier project records can
// supply it.
//
// The store makes the same check itself immediately before a write, so an apply
// run cannot slip past it. This one is what a dry run has instead, and it reads
// the same set the apply run will — which is why the two agree.
type projectCheck struct {
	projectID string
	available func(string) bool
}

// liveProjectIDs is the set of project ids the store holds now.
func liveProjectIDs(ctx context.Context, s *memory.Store) (map[string]bool, error) {
	projects, err := s.PortableProjects(ctx)
	if err != nil {
		return nil, fmt.Errorf("read projects for import: %w", err)
	}
	out := make(map[string]bool, len(projects))
	for _, p := range projects {
		out[p.ID] = true
	}
	return out, nil
}

// linkKey is the identity of one edge of the graph: the three columns the
// table's primary key is made of. Two edges between the same pair of memories
// are two edges when their relations differ, so a key on the source alone would
// tell them apart wrongly — and the run's created-ids set has to tell them
// apart, because a repeated edge in one artifact is a skip in the dry run
// exactly as it is on apply.
func linkKey(l *memory.PortableLink) string {
	return l.SourceID + "\x00" + l.TargetID + "\x00" + l.Relation
}

// linkDetail is the label a report line shows for an edge: the relation and the
// memory it is about. The memory that MAKES the claim is the record's own id,
// so the report prints it beside the detail.
//
// The target goes through the shared id renderer, so an id carrying a newline
// cannot forge a line of the report that names it.
func linkDetail(l *memory.PortableLink) string {
	return l.Relation + " " + assemble.Token(l.TargetID)
}

// requirePayload rejects a record whose type claims a payload it does not carry.
// `{"type":"memory"}` is not a memory with no fields — it is a malformed file,
// and importing it would write an empty row or fail the write with an error
// that does not name the line.
func requirePayload(p parsedRecord) error {
	missing := ""
	switch p.rec.Type {
	case TypeProject:
		if p.rec.Project == nil {
			missing = TypeProject
		}
	case TypeMemory:
		if p.rec.Memory == nil {
			missing = TypeMemory
		}
	case TypeTask:
		if p.rec.Task == nil {
			missing = TypeTask
		}
	case TypeDecision:
		if p.rec.Decision == nil {
			missing = TypeDecision
		}
	case TypeLink:
		if p.rec.Link == nil {
			missing = TypeLink
		}
	}
	if missing != "" {
		return fmt.Errorf("line %d: %s record carries no %q payload", p.line, memory.RefusalArg("type", p.rec.Type), missing)
	}
	if extra := otherPayloads(p.rec, p.rec.Type); extra != "" {
		return fmt.Errorf("line %d: %s record also carries %s — a line holds the one payload its type names", p.line, memory.RefusalArg("type", p.rec.Type), extra)
	}
	return nil
}

// otherPayloads names, quoted, every payload the record carries besides the one
// its type names. A line holding a second payload is a malformed file just as a
// line holding none is: importing the payload the type names would drop the
// other one, and the run would report a clean import of a file whose data it
// left behind. Only the kinds are named, never the payload's content.
func otherPayloads(rec record, own string) string {
	var extra []string
	for _, k := range []struct {
		kind    string
		present bool
	}{
		{TypeProject, rec.Project != nil},
		{TypeMemory, rec.Memory != nil},
		{TypeTask, rec.Task != nil},
		{TypeDecision, rec.Decision != nil},
		{TypeLink, rec.Link != nil},
	} {
		if k.present && k.kind != own {
			extra = append(extra, fmt.Sprintf("%q", k.kind))
		}
	}
	if len(extra) == 0 {
		return ""
	}
	return strings.Join(extra, ", ") + " payload"
}

// repeatedKey returns the second spelling of the first top-level key the line's
// object repeats, or "" when every key appears once. encoding/json keeps the
// last value of a repeated key, so a line holding two "memory" objects decodes
// to one record and the first memory is gone before the importer sees it; the
// payload-mismatch check cannot catch that, because the decoded record holds a
// single payload. The exporter never repeats a key, so a line that does was
// merged or hand-edited, and which copy it meant is a guess.
//
// Every top-level key is checked, not only the payload keys: a repeated "type"
// decides which payload the line must carry and a repeated "schema_version"
// decides which version governs, and a key this build does not read today may
// be one a later schema does. Keys are compared the way the decoder matches a
// key to a field — case-insensitively — because "memory" and "Memory" fill the
// same field (see foldRune). A nested object's keys are not checked: the record is the unit
// this function judges.
//
// The line has already been decoded, so it is valid JSON; a line whose top
// level is not an object has no keys to repeat. The pass costs a second scan of
// the line: each key is read as a token and each value is skipped by decoding
// it into one reused json.RawMessage.
func repeatedKey(line []byte) string {
	dec := json.NewDecoder(bytes.NewReader(line))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return ""
	}
	seen := map[string]bool{}
	var skip json.RawMessage
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return ""
		}
		key, ok := tok.(string)
		if !ok {
			return ""
		}
		folded := strings.Map(foldRune, key)
		if seen[folded] {
			return key
		}
		seen[folded] = true
		if err := dec.Decode(&skip); err != nil {
			return ""
		}
	}
	return ""
}

// foldRune maps a rune to the smallest member of its simple case-folding orbit,
// which is how encoding/json folds a key before matching it to a field (its own
// foldRune, applied to every rune). Folding the same way is what makes "taſk",
// with a long s, the same key as "task" here, as it is to the decoder.
func foldRune(r rune) rune {
	for {
		r2 := unicode.SimpleFold(r)
		if r2 <= r {
			return r2
		}
		r = r2
	}
}

// actionFor turns a created flag into the action the report shows. A record
// that was not created was already present: the store reports no error, and
// that is a skip.
func actionFor(created bool) Action {
	if created {
		return ActionCreate
	}
	return ActionSkip
}

// contentClamped turns ImportMemory's cut flag into the field names
// RecordResult carries, so one report line speaks for a memory and a task alike.
//
// A memory has exactly one clampable field, so this is a translation rather than a
// rule: it is here because the store answers the question its own way — a
// boolean, since it has one field to answer about — and the report has to name
// the field for every record type, not just the one whose importer could spare
// the names. A memory that was cut names `content`; one that was not names none.
func contentClamped(cut bool) []string {
	if cut {
		return []string{"content"}
	}
	return nil
}

// createdIDs is the set of record ids this run has already created, per record
// kind. It is the half of the store's presence check that a dry run cannot read:
// that check answers from the database, and an apply run has committed the first
// record by the time a repeated id asks, while a dry run has written nothing and
// so answers "absent" for every occurrence. The plan keeps the other half — an id
// an earlier step created is present for every step after it — which is what makes
// the preview's skip the same skip the apply run makes.
//
// It records what the run actually CREATED rather than what the artifact repeats,
// and the difference is the point: a repeated id whose first record was refused
// wrote nothing under it, so the second is judged on its own record, exactly as
// the apply run judges it.
type createdIDs map[string]map[string]bool

func (c createdIDs) has(kind, id string) bool {
	return c[kind] != nil && c[kind][id]
}

func (c createdIDs) add(kind, id string) {
	if c[kind] == nil {
		c[kind] = make(map[string]bool)
	}
	c[kind][id] = true
}

// run performs one step and records the outcome.
//
// created is the run's set of ids already created. It is read after the project
// check and before the store is asked, which is the order the apply run answers
// in: the project check is this function's own and runs ahead of the store in
// both modes, so a repeated id naming a missing project is refused in both; and
// the store's presence check runs ahead of its record checks, so a repeated id
// whose second record is invalid is a skip in the apply run and has to be one
// here before the store's validation can refuse it.
func (st step) run(ctx context.Context, s *memory.Store, report *ImportReport, onRecord func(RecordResult), created createdIDs) {
	var got outcome
	key := st.stepKey()
	switch {
	case st.check != nil && !st.check.available(st.check.projectID):
		// The project is available neither in the store nor from this run's
		// earlier records, so there is nothing to attach the record to. Refused
		// here rather than left to the store's own pre-write check, so a dry run
		// reaches the same verdict an apply run would. The reason alone: the
		// report line names the record and the line number around it.
		got = outcome{action: ActionReject, err: fmt.Errorf(
			"project %s not found — the artifact carries no record for it and this store does not have it",
			memory.RefusalArg("project_id", st.check.projectID))}
	case created.has(st.kind, key):
		// An earlier step in this run wrote this id, so the store's presence
		// check answers "present" — in the apply run it already has. Reported
		// without asking the store again: the answer is the one the apply run
		// got, and asking a second time is a second code path to drift.
		got = outcome{action: ActionSkip}
	default:
		got = st.runFunc(ctx, s, st.opts)
	}
	// A created id is present for every step after this one, so the run records
	// it whether or not anything was written: a dry run's create is a promise to
	// write, and the next record with the same id has to read as a skip.
	if got.action == ActionCreate {
		created.add(st.kind, key)
	}
	result := RecordResult{
		Type:       st.kind,
		ID:         st.rec.id(),
		Line:       st.rec.line,
		Action:     got.action,
		Detail:     st.detail,
		Clamped:    got.clamped,
		Downgraded: got.downgraded,
		DroppedRef: got.droppedRef,
		DropReason: got.dropReason,
		Error:      got.err,
	}
	action, err := got.action, got.err
	switch action {
	case ActionCreate:
		report.count(report.Created, st.kind)
	case ActionSkip:
		report.count(report.Skipped, st.kind)
	case ActionReject:
		report.Rejected++
		report.Errors = append(report.Errors, fmt.Errorf("line %d: %s %s: %w", st.rec.line, st.kind, labelOrID(result, err), err))
	}
	if onRecord != nil {
		onRecord(result)
	}
}

// safeDetail is the label a report line shows for a record, with the one
// substitution that matters: where that label would carry a credential, the
// record's id stands in for it.
//
// Every rejection path is a printing path, and the report is the only place the
// artifact's own words are echoed back — the store refuses to write them, so
// without this the value would reach the terminal through the very report that
// announced the refusal. It is applied where the detail is built rather than
// where it is printed, because a record can be rejected for a reason that has
// nothing to do with the credential (a missing status, an unknown project) and
// still carry one, and because RecordResult.Detail is a public field with
// consumers in other packages — the per-record line in `ghost import` is one.
//
// The check runs on the WHOLE of the field the label is cut from, not on the
// label. A 60-rune prefix is a cut, and a credential is longer than a cut is
// likely to be: "the deploy token is ghp_Ab12Cd34Ef5…" scans as clean and prints
// the first half of a live token, on a record that is skipped or rejected for
// some unrelated reason — a missing task status, an unknown project — so the
// refusal the guard raised is not even the reason the line is being printed.
//
// What is still narrow is the OUTPUT, not the input. A memory whose first line
// is clean and whose second holds a credential prints a clean prefix, because
// the report identifies the line and the error says what to remove from it. The
// difference is that the decision is made on all of the text, so a value the
// prefix happens to cut is caught, while a value elsewhere in the record does not
// cost the operator their preview.
func safeDetail(label, full, id string) string {
	if _, ok := secret.Detect(full); ok {
		return id
	}
	return label
}

// labelOrID names a record for an error line: the detail where there is one, the
// id where there is not.
//
// err matters as much as the result. A credential refusal prints the id and
// nothing else, whatever the detail happens to hold — safeDetail already reduces
// it to the id, and this is the second line for a caller that sets Detail
// directly or a kind added later that safeDetail does not know about. The format
// still reaches the reader through the wrapped error, which is the part they need
// in order to fix the artifact.
//
// The id goes through assemble.Token on every path, including the credential one.
// That is not a contradiction: a credential refusal reducing the line to the id is
// about not reprinting the CONTENT, and the id is a key this report prints inside
// no quoting at all — so a store holding one that carries a newline, a store
// written before the shape check existed, would forge a report line here (#791).
// A well-formed id is written bare and this is invisible; a hostile one is
// rendered inert on the very line that is naming it.
func labelOrID(r RecordResult, err error) string {
	var refused *memory.SecretContentError
	if errors.As(err, &refused) {
		if r.ID != "" {
			return assemble.Token(r.ID)
		}
		return "(no id)"
	}
	switch {
	case r.Detail != "" && r.ID != "":
		return fmt.Sprintf("%q (%s)", r.Detail, assemble.Token(r.ID))
	case r.ID != "":
		return assemble.Token(r.ID)
	default:
		return "(no id)"
	}
}

// contentPreviewMax is the cap on the content preview a report line carries. It
// is 60 rather than the 70 the MCP resolve reports use because an import report
// line is narrower — it shares its line with an action, a type, a line number and
// an id — and a preview that wraps is a preview nobody reads.
const contentPreviewMax = 60

// orderTasks returns the task records in an order where a task's blocker comes
// before it.
//
// blocked_by is a self-reference, so a task whose blocker is written later in
// the file cannot be inserted first — the store refuses it, and the artifact
// would reject records it has every reason to accept. Order is therefore
// topological over the references the artifact can satisfy.
//
// A pointer to a task the artifact does not contain, and a cycle, have their
// pointer dropped: both are references that can never resolve, and dropping one
// field is a better outcome than refusing records over it. The record carries
// what was cleared and why (droppedRef, dropReason), so its report line names
// the drop; and planRecords puts a cleared pointer back when the store already
// holds its target, since a target outside the artifact may still be one the
// destination has. A blocker the artifact contains but the run rejects is the
// same case discovered later, when the steps run, and planRecords drops that
// pointer too.
func orderTasks(recs []parsedRecord) []parsedRecord {
	present := make(map[string]bool, len(recs))
	for _, r := range recs {
		present[r.rec.Task.ID] = true
	}
	cleaned := make([]parsedRecord, 0, len(recs))
	for _, r := range recs {
		t := *r.rec.Task
		cp := r
		if t.BlockedBy != "" && !present[t.BlockedBy] {
			cp.droppedRef, cp.dropReason = t.BlockedBy, DropAbsent
			t.BlockedBy = ""
		}
		cp.rec.Task = &t
		cleaned = append(cleaned, cp)
	}
	return topoOrder(cleaned,
		func(r parsedRecord) string { return r.rec.Task.ID },
		func(r parsedRecord) string { return r.rec.Task.BlockedBy },
		func(r parsedRecord) parsedRecord {
			t := *r.rec.Task
			r.droppedRef, r.dropReason = t.BlockedBy, DropCycle
			t.BlockedBy = ""
			r.rec.Task = &t
			return r
		})
}

// orderDecisions is orderTasks for decisions' superseded_by.
func orderDecisions(recs []parsedRecord) []parsedRecord {
	present := make(map[string]bool, len(recs))
	for _, r := range recs {
		present[r.rec.Decision.ID] = true
	}
	cleaned := make([]parsedRecord, 0, len(recs))
	for _, r := range recs {
		d := *r.rec.Decision
		cp := r
		if d.SupersededBy != "" && !present[d.SupersededBy] {
			cp.droppedRef, cp.dropReason = d.SupersededBy, DropAbsent
			d.SupersededBy = ""
		}
		cp.rec.Decision = &d
		cleaned = append(cleaned, cp)
	}
	return topoOrder(cleaned,
		func(r parsedRecord) string { return r.rec.Decision.ID },
		func(r parsedRecord) string { return r.rec.Decision.SupersededBy },
		func(r parsedRecord) parsedRecord {
			d := *r.rec.Decision
			r.droppedRef, r.dropReason = d.SupersededBy, DropCycle
			d.SupersededBy = ""
			r.rec.Decision = &d
			return r
		})
}

// topoOrder orders records so that a record's referent comes before it, keeping
// the input order among records that do not constrain each other.
//
// A cycle — two tasks each blocked by the other, which a hand-edited file can
// easily contain — has no valid order, so the references of the cycle's members
// are dropped and ordering continues; a record that only points into the cycle
// keeps its pointer and follows its target. Losing a pointer is the better
// outcome: the rows themselves are good, and a pointer within a cycle describes
// no ordering anyone could have intended.
func topoOrder(recs []parsedRecord, idOf, refOf func(parsedRecord) string, clearRef func(parsedRecord) parsedRecord) []parsedRecord {
	// The passes work on indices into a private copy, with each record's id and
	// pointer read once. A chain hanging off a cycle takes one pass per link
	// once the cycle is broken, so a pass has to cost no more than a walk over
	// the indices still pending: copying the records themselves, or asking the
	// pending slice whether it holds an id, made the whole ordering cubic.
	//
	// Ids are numbered once, so a pass compares integers rather than hashing
	// strings. Every record is either emitted or pending, so "the pointer names
	// a pending record" is "it names an id of recs that has not been emitted";
	// a pointer to an id outside recs (ref -1) constrains nothing.
	work := make([]parsedRecord, len(recs))
	copy(work, recs)
	number := make(map[string]int, len(work))
	idNum := make([]int, len(work))
	for i, r := range work {
		id := idOf(r)
		n, ok := number[id]
		if !ok {
			n = len(number)
			number[id] = n
		}
		idNum[i] = n
	}
	refNum := make([]int, len(work))
	for i, r := range work {
		refNum[i] = -1
		if ref := refOf(r); ref != "" {
			if n, ok := number[ref]; ok {
				refNum[i] = n
			}
		}
	}
	emitted := make([]bool, len(number))
	pending := make([]int, len(work))
	for i := range pending {
		pending[i] = i
	}
	out := make([]parsedRecord, 0, len(work))
	for len(pending) > 0 {
		deferred := pending[:0:0]
		progressed := false
		for _, i := range pending {
			if ref := refNum[i]; ref != -1 && !emitted[ref] {
				deferred = append(deferred, i)
				continue
			}
			out = append(out, work[i])
			emitted[idNum[i]] = true
			progressed = true
		}
		if !progressed {
			// A cycle: nothing in the remainder can be emitted this pass, and no
			// order will ever satisfy it. Drop the references of the records ON
			// a cycle and go round again. Dropping is the better outcome than
			// refusing: the rows themselves are good, and a blocked_by pointer
			// inside a cycle describes no ordering anyone could have intended.
			// Reporting the first record as rejected would lose a task over a
			// pointer that carries no information.
			//
			// Only the cycle's own members lose their pointer. A record that
			// merely points INTO a cycle is not part of it: once the cycle's
			// members are written, its target exists, so the next pass emits it
			// after them with its pointer intact — and the report's "part of a
			// cycle" is true of every record it is said of (#1153).
			//
			// Membership is decided for every record before any pointer is
			// cleared: clearing as the scan goes would break the cycle at its
			// first member and leave the rest of it looking acyclic.
			stalled := make([]parsedRecord, len(deferred))
			for k, i := range deferred {
				stalled[k] = work[i]
			}
			members := cycleMembers(stalled, idOf, refOf)
			cleared := false
			for k, i := range deferred {
				if members[k] {
					work[i] = clearRef(work[i])
					refNum[i] = -1
					cleared = true
				}
			}
			if !cleared {
				// Unreachable with one pointer per record — a remainder that
				// cannot progress always holds a cycle — but the loop must end
				// whatever the input, so the old rule stands behind it.
				for _, i := range deferred {
					out = append(out, clearRef(work[i]))
				}
				break
			}
		}
		pending = deferred
	}
	return out
}

// cycleMembers reports, for each record, whether it lies on a cycle of
// pointers through recs. Each record has at most one pointer, so the records
// form a graph in which every node has at most one successor (the first record
// carrying the id its pointer names), and every walk is a single path.
//
// It runs in time linear in len(recs): each record is walked once. A walk
// stops at a record with no successor or one an earlier walk has finished
// (neither can lead back into this walk), or at one this walk has already
// visited — and then exactly the records from that one to the end of the path
// are the cycle. Walking every record's path afresh, as this once did, made an
// import of a long chain ending in a cycle cubic in the chain's length.
func cycleMembers(recs []parsedRecord, idOf, refOf func(parsedRecord) string) []bool {
	index := make(map[string]int, len(recs))
	for i, r := range recs {
		if _, seen := index[idOf(r)]; !seen {
			index[idOf(r)] = i
		}
	}
	next := make([]int, len(recs))
	for i, r := range recs {
		next[i] = -1
		if ref := refOf(r); ref != "" {
			if j, ok := index[ref]; ok {
				next[i] = j
			}
		}
	}
	const (
		unvisited = iota
		onPath
		done
	)
	state := make([]int, len(recs))
	members := make([]bool, len(recs))
	pos := make([]int, len(recs)) // a record's position in the current path
	var path []int
	for start := range recs {
		if state[start] != unvisited {
			continue
		}
		path = path[:0]
		cur := start
		for cur != -1 && state[cur] == unvisited {
			state[cur] = onPath
			pos[cur] = len(path)
			path = append(path, cur)
			cur = next[cur]
		}
		if cur != -1 && state[cur] == onPath {
			for _, i := range path[pos[cur]:] {
				members[i] = true
			}
		}
		for _, i := range path {
			state[i] = done
		}
	}
	return members
}

// sortTasksByID puts tasks in id order, for the deterministic export.
func sortTasksByID(t []memory.Task) {
	sort.SliceStable(t, func(i, j int) bool { return t[i].ID < t[j].ID })
}

// sortDecisionsByID puts decisions in id order, for the deterministic export.
func sortDecisionsByID(d []memory.Decision) {
	sort.SliceStable(d, func(i, j int) bool { return d[i].ID < d[j].ID })
}
