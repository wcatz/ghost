package portable

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

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
	// Clamped reports that content was cut at MaxContentLen on the way in. It
	// is set for a create, including a dry run's, because the cut is what the
	// user needs to know about before applying.
	Clamped bool
	// Downgraded reports that the memory's own source and pin state were
	// replaced, because the run was not told to trust the artifact's provenance
	// (see memory.ImportOptions.TrustProvenance). It is set for a dry run's
	// create too, for the same reason Clamped is: the rewrite is the thing to
	// read the preview for.
	Downgraded bool
	Error      error
}

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
// version, a second header, an unknown record type, a record with no payload —
// is refused before any record is written. Those are not per-record problems: a
// missing header means nobody checked the version, and applying the rest of a
// file whose format is unknown would be importing data whose meaning this build
// is guessing at.
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
	for _, step := range plan {
		step.run(ctx, s, &report, onRecord)
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
	scanner := bufio.NewScanner(r)
	// A memory is capped at MaxContentLen bytes, so a line can be larger than
	// bufio's 64 KiB default. The cap here is 1 MiB per line: a single record
	// cannot legitimately exceed the content cap by much, and a larger line
	// means a file that is not this format.
	const maxLine = 1 << 20
	scanner.Buffer(make([]byte, 0, 64*1024), maxLine)

	var (
		recs    []parsedRecord
		unread  []unreadable
		header  bool
		lineNum int
	)
	for scanner.Scan() {
		lineNum++
		line := scanner.Bytes()
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
		if !header {
			if rec.Type != TypeHeader {
				return nil, nil, fmt.Errorf("line %d: %w — the first line must be %q, so the schema version is checked before any record is read", lineNum, errNoHeader, TypeHeader)
			}
			if err := checkSchemaVersion(rec.SchemaVersion); err != nil {
				return nil, nil, fmt.Errorf("line %d: %w", lineNum, err)
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
		if rec.Memory != nil {
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
		recs = append(recs, parsedRecord{rec: rec, line: lineNum})
	}
	if err := scanner.Err(); err != nil {
		if err == bufio.ErrTooLong {
			return nil, nil, fmt.Errorf("a line exceeds the %d-byte limit — this does not look like a ghost artifact", maxLine)
		}
		return nil, nil, fmt.Errorf("read artifact: %w", err)
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
}

// id returns the record's id, for the per-record report. A record with no
// payload has no id, and the empty string is what the report prints alongside
// the line number that identifies it.
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
	detail  string
	check   *projectCheck
	opts    ImportOptions
	runFunc func(ctx context.Context, s *memory.Store, opts ImportOptions) outcome
}

// outcome is what running one step produced. A struct rather than a tuple
// because a step's result has grown past what a return line can carry legibly:
// what happened, whether the content was cut, and whether the memory's
// provenance was downgraded on the way in.
type outcome struct {
	action     Action
	clamped    bool
	downgraded bool
	err        error
}

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
// importing as far as it goes, and the report says what was dropped.
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
		tasks     []parsedRecord
		decisions []parsedRecord
	)
	for _, p := range recs {
		switch p.rec.Type {
		case TypeProject:
			projects = append(projects, p)
		case TypeMemory:
			memories = append(memories, p)
		case TypeTask:
			tasks = append(tasks, p)
		case TypeDecision:
			decisions = append(decisions, p)
		default:
			// Refused rather than skipped. A record type this build does not
			// know is a file from a newer build or a different format, and
			// dropping it would import a store that is missing data the file
			// plainly contains.
			return nil, fmt.Errorf("line %d: unknown record type %q — this ghost build reads %q, %q, %q and %q records",
				p.line, p.rec.Type, TypeProject, TypeMemory, TypeTask, TypeDecision)
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
	for _, p := range memories {
		m := p.rec.Memory
		m.ProjectID = under(m.ProjectID)
		steps = append(steps, step{rec: p, kind: TypeMemory, detail: safeDetail(contentPrefix(m.Content), m.Content, m.ID), check: checkFor(m.ProjectID), opts: opts,
			runFunc: func(ctx context.Context, s *memory.Store, opts ImportOptions) outcome {
				created, clamped, downgraded, err := s.ImportMemory(ctx, *m, opts)
				if err != nil {
					return outcome{action: ActionReject, err: err}
				}
				return outcome{action: actionFor(created), clamped: clamped, downgraded: downgraded}
			}})
	}
	for _, p := range orderTasks(tasks) {
		t := p.rec.Task
		t.ProjectID = under(t.ProjectID)
		steps = append(steps, step{rec: p, kind: TypeTask, detail: safeDetail(t.Title, t.Title, t.ID), check: checkFor(t.ProjectID), opts: opts,
			runFunc: func(ctx context.Context, s *memory.Store, opts ImportOptions) outcome {
				created, err := s.ImportTask(ctx, *t, opts.Apply)
				if err != nil {
					return outcome{action: ActionReject, err: err}
				}
				return outcome{action: actionFor(created)}
			}})
	}
	for _, p := range orderDecisions(decisions) {
		d := p.rec.Decision
		d.ProjectID = under(d.ProjectID)
		steps = append(steps, step{rec: p, kind: TypeDecision, detail: safeDetail(d.Title, d.Title, d.ID), check: checkFor(d.ProjectID), opts: opts,
			runFunc: func(ctx context.Context, s *memory.Store, opts ImportOptions) outcome {
				created, err := s.ImportDecision(ctx, *d, opts.Apply)
				if err != nil {
					return outcome{action: ActionReject, err: err}
				}
				return outcome{action: actionFor(created)}
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
	for _, rec := range projects {
		p := *rec.rec.Project
		// The very same project, or nothing that collides: keep the artifact's
		// own id.
		if byID[p.ID] {
			mapping[p.ID] = p.ID
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
		if found != "" {
			mapping[p.ID] = found
			continue
		}
		mapping[p.ID] = p.ID
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
	}
	if missing != "" {
		return fmt.Errorf("line %d: %q record carries no %q payload", p.line, p.rec.Type, missing)
	}
	return nil
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

// run performs one step and records the outcome.
func (st step) run(ctx context.Context, s *memory.Store, report *ImportReport, onRecord func(RecordResult)) {
	var got outcome
	if st.check != nil && !st.check.available(st.check.projectID) {
		// The project is available neither in the store nor from this run's
		// earlier records, so there is nothing to attach the record to. Refused
		// here rather than left to the store's own pre-write check, so a dry run
		// reaches the same verdict an apply run would. The reason alone: the
		// report line names the record and the line number around it.
		got = outcome{action: ActionReject, err: fmt.Errorf(
			"project %q not found — the artifact carries no record for it and this store does not have it",
			st.check.projectID)}
	} else {
		got = st.runFunc(ctx, s, st.opts)
	}
	result := RecordResult{
		Type:       st.kind,
		ID:         st.rec.id(),
		Line:       st.rec.line,
		Action:     got.action,
		Detail:     st.detail,
		Clamped:    got.clamped,
		Downgraded: got.downgraded,
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

// contentPrefix is the first line of a memory's content, capped, for a report
// line. firstLine keeps the report readable without inventing a summary of the
// memory.
func contentPrefix(content string) string {
	const max = 60
	line := content
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	r := []rune(line)
	if len(r) > max {
		return string(r[:max]) + "…"
	}
	return line
}

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
// field is a better outcome than refusing records over it. The store reports a
// dropped reference as a skip-free create, so what was lost shows up as a task
// that is simply not blocked — the honest state, since the blocker does not
// exist to be blocked by.
func orderTasks(recs []parsedRecord) []parsedRecord {
	present := make(map[string]bool, len(recs))
	for _, r := range recs {
		present[r.rec.Task.ID] = true
	}
	cleaned := make([]parsedRecord, 0, len(recs))
	for _, r := range recs {
		t := *r.rec.Task
		if t.BlockedBy != "" && !present[t.BlockedBy] {
			t.BlockedBy = ""
		}
		cp := r
		cp.rec.Task = &t
		cleaned = append(cleaned, cp)
	}
	return topoOrder(cleaned,
		func(r parsedRecord) string { return r.rec.Task.ID },
		func(r parsedRecord) string { return r.rec.Task.BlockedBy },
		func(r parsedRecord) parsedRecord {
			t := *r.rec.Task
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
		if d.SupersededBy != "" && !present[d.SupersededBy] {
			d.SupersededBy = ""
		}
		cp := r
		cp.rec.Decision = &d
		cleaned = append(cleaned, cp)
	}
	return topoOrder(cleaned,
		func(r parsedRecord) string { return r.rec.Decision.ID },
		func(r parsedRecord) string { return r.rec.Decision.SupersededBy },
		func(r parsedRecord) parsedRecord {
			d := *r.rec.Decision
			d.SupersededBy = ""
			r.rec.Decision = &d
			return r
		})
}

// topoOrder orders records so that a record's referent comes before it, keeping
// the input order among records that do not constrain each other.
//
// A cycle — two tasks each blocked by the other, which a hand-edited file can
// easily contain — has no valid order, so the references inside it are dropped
// and the records appended in input order. Losing a pointer is the better
// outcome: the rows themselves are good, and a pointer within a cycle describes
// no ordering anyone could have intended.
func topoOrder(recs []parsedRecord, idOf, refOf func(parsedRecord) string, clearRef func(parsedRecord) parsedRecord) []parsedRecord {
	pending := make([]parsedRecord, len(recs))
	copy(pending, recs)
	out := make([]parsedRecord, 0, len(recs))
	emitted := make(map[string]bool, len(recs))
	for len(pending) > 0 {
		var deferred []parsedRecord
		progressed := false
		for _, r := range pending {
			ref := refOf(r)
			if ref != "" && !emitted[ref] && containsID(pending, ref, idOf) {
				deferred = append(deferred, r)
				continue
			}
			out = append(out, r)
			emitted[idOf(r)] = true
			progressed = true
		}
		if !progressed {
			// A cycle: nothing in the remainder can be emitted this pass, and no
			// order will ever satisfy it. Drop the references among the
			// remaining records and append them in input order. Dropping is the
			// better outcome than refusing: the rows themselves are good, and a
			// blocked_by pointer inside a cycle describes no ordering anyone
			// could have intended. Reporting the first record as rejected would
			// lose a task over a pointer that carries no information.
			for _, r := range deferred {
				r = clearRef(r)
				out = append(out, r)
			}
			break
		}
		pending = deferred
	}
	return out
}

func containsID(recs []parsedRecord, id string, idOf func(parsedRecord) string) bool {
	for _, r := range recs {
		if idOf(r) == id {
			return true
		}
	}
	return false
}

// sortTasksByID puts tasks in id order, for the deterministic export.
func sortTasksByID(t []memory.Task) {
	sort.SliceStable(t, func(i, j int) bool { return t[i].ID < t[j].ID })
}

// sortDecisionsByID puts decisions in id order, for the deterministic export.
func sortDecisionsByID(d []memory.Decision) {
	sort.SliceStable(d, func(i, j int) bool { return d[i].ID < d[j].ID })
}
