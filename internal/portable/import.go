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
type ImportOptions = memory.ImportOptions

// Action is what an import did, or would do, with one record.
type Action string

const (
	ActionCreate Action = "create"
	ActionSkip   Action = "skip"
	ActionReject Action = "reject"
)

// RecordResult is the outcome for one record of an import, reported to the
// caller's onRecord so the CLI can print it. Line is the artifact line number
type RecordResult struct {
	Type       string
	ID         string
	Line       int
	Action     Action
	Detail     string
	Clamped    []string
	Downgraded bool
	DroppedRef string
	DropReason DropReason
	Error      error
}

// DropReason is why an import dropped a task's blocked_by or a decision's
// superseded_by. Each is a pointer that could not be honoured without refusing
type DropReason int

const (
	DropRejected DropReason = iota
	DropAbsent
	DropCycle
)

// ImportReport summarises one import run, counted per record type.
type ImportReport struct {
	Applied  bool
	Created  map[string]int
	Skipped  map[string]int
	Rejected int
	Errors   []error
}

func (r *ImportReport) count(m map[string]int, kind string) {
	if m == nil {
		return
	}
	m[kind]++
}

// Artifact is one parsed artifact: the records an import will apply and the
// lines it collected as per-record refusals. Parse returns it, and its Import
type Artifact struct {
	recs       []parsedRecord
	unreadable []unreadable
}

// Parse reads an artifact and refuses one this build cannot read as a whole,
// without a store. It is the file-level half of Import — the header, every
func Parse(r io.Reader) (*Artifact, error) {
	recs, unreadable, err := readRecords(r)
	if err != nil {
		return nil, err
	}
	if err := checkRecordTypes(recs); err != nil {
		return nil, err
	}
	return &Artifact{recs: recs, unreadable: unreadable}, nil
}

func checkRecordTypes(recs []parsedRecord) error {
	for _, p := range recs {
		switch p.rec.Type {
		case TypeProject, TypeMemory, TypeTask, TypeDecision, TypeLink:
		default:
			return fmt.Errorf("line %d: unknown record type %s — this ghost build reads %q, %q, %q, %q and %q records",
				p.line, memory.RefusalArg("type", p.rec.Type), TypeProject, TypeMemory, TypeTask, TypeDecision, TypeLink)
		}
		if err := requirePayload(p); err != nil {
			return err
		}
	}
	return nil
}

// Import reads an artifact and, when apply is true, inserts the records the
// store does not already have.
func Import(ctx context.Context, s *memory.Store, r io.Reader, opts ImportOptions, onRecord func(RecordResult)) (ImportReport, error) {
	report := newImportReport(opts)
	a, err := Parse(r)
	if err != nil {
		return report, err
	}
	return a.Import(ctx, s, opts, onRecord)
}

func newImportReport(opts ImportOptions) ImportReport {
	return ImportReport{
		Applied: opts.Apply,
		Created: map[string]int{},
		Skipped: map[string]int{},
	}
}

// Import applies an artifact Parse has already read and judged. It is the run
// half of Import, split out so a caller that had to refuse the file before
func (a *Artifact) Import(ctx context.Context, s *memory.Store, opts ImportOptions, onRecord func(RecordResult)) (ImportReport, error) {
	report := newImportReport(opts)

	plan, err := planRecords(a.recs, s, ctx, opts)
	if err != nil {
		return report, err
	}
	for _, bad := range a.unreadable {
		report.Rejected++
		report.Errors = append(report.Errors, bad.err)
		if onRecord != nil {
			onRecord(bad.result)
		}
	}
	created := createdIDs{}
	for _, step := range plan {
		step.run(ctx, s, &report, onRecord, created)
	}
	return report, nil
}

type unreadable struct {
	line   int
	result RecordResult
	err    error
}

func readRecords(r io.Reader) ([]parsedRecord, []unreadable, error) {
	reader := bufio.NewReaderSize(r, 64*1024)

	var (
		recs    []parsedRecord
		unread  []unreadable
		header  bool
		lineNum int
	)
	for {
		raw, readErr := reader.ReadBytes('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, nil, fmt.Errorf("read artifact: %w", readErr)
		}
		if len(raw) == 0 {
			break
		}
		lineNum++
		line := bytes.TrimRight(raw, "\r\n")
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var rec record
		if err := json.Unmarshal(line, &rec); err != nil {
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

var errNoHeader = errors.New("not a ghost artifact (no schema-version header)")

func checkHeaderFeatures(rec record) error {
	for _, f := range rec.Features {
		if !knownFeatures[f] {
			return fmt.Errorf("header declares feature %s that this ghost build does not understand", memory.RefusalArg("feature", f))
		}
	}
	return nil
}

func checkSchemaVersion(version int) error {
	if version >= minReadableSchemaVersion && version <= SchemaVersion {
		return nil
	}
	if version > SchemaVersion {
		return fmt.Errorf("schema version %d is newer than this ghost build reads (v%d) — upgrade ghost, or re-export with the version that wrote it", version, SchemaVersion)
	}
	return fmt.Errorf("schema version %d is older than this ghost build reads (v%d and later) — re-export the artifact with this build", version, minReadableSchemaVersion)
}

type parsedRecord struct {
	rec        record
	line       int
	droppedRef string
	dropReason DropReason
}

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

type step struct {
	rec     parsedRecord
	kind    string
	key     string
	detail  string
	check   *projectCheck
	opts    ImportOptions
	runFunc func(ctx context.Context, s *memory.Store, opts ImportOptions) outcome
}

func (st step) stepKey() string {
	if st.key != "" {
		return st.key
	}
	return st.rec.id()
}

type outcome struct {
	action     Action
	clamped    []string
	downgraded bool
	droppedRef string
	dropReason DropReason
	err        error
}

const landedElsewhere = "\x00elsewhere"

func planRecords(recs []parsedRecord, s *memory.Store, ctx context.Context, opts ImportOptions) ([]step, error) {
	var (
		projects  []parsedRecord
		memories  []parsedRecord
		links     []parsedRecord
		tasks     []parsedRecord
		decisions []parsedRecord
	)
	if err := checkRecordTypes(recs); err != nil {
		return nil, err
	}
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
		}
	}

	known, err := liveProjectIDs(ctx, s)
	if err != nil {
		return nil, err
	}
	local, err := resolveProjectMapping(ctx, s, projects)
	if err != nil {
		return nil, err
	}
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
			detail = fmt.Sprintf("%s → %s (this store already records that checkout or repository)",
				project.Name, adopted)
			if !known[adopted] {
				detail = fmt.Sprintf("%s → %s (an earlier project in this artifact records that checkout or repository)",
					project.Name, adopted)
			}
		}
		adoptedElsewhere := adopted != project.ID
		steps = append(steps, step{rec: p, kind: TypeProject, detail: safeDetail(detail, detail, project.ID), opts: opts,
			runFunc: func(ctx context.Context, s *memory.Store, opts ImportOptions) outcome {
				if adoptedElsewhere {
					return outcome{action: ActionSkip}
				}
				created, err := s.ImportProject(ctx, *project, opts.Apply)
				if err != nil {
					return outcome{action: ActionReject, err: err}
				}
				known[project.ID] = true
				return outcome{action: actionFor(created)}
			}})
	}
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
				landedMemories[m.ID] = true
				return outcome{action: actionFor(created), clamped: contentClamped(clamped), downgraded: downgraded}
			}})
	}
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
	landedTasks := map[string]string{}
	landedDecisions := map[string]string{}
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
	claimedPath := map[string]string{}
	claimedRemote := map[string]string{}
	settled := map[string]bool{}
	for _, rec := range projects {
		p := *rec.rec.Project
		if settled[p.ID] {
			continue
		}
		if byID[p.ID] {
			mapping[p.ID] = p.ID
			settled[p.ID] = true
			continue
		}
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

type projectCheck struct {
	projectID string
	available func(string) bool
}

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

func linkKey(l *memory.PortableLink) string {
	return l.SourceID + "\x00" + l.TargetID + "\x00" + l.Relation
}

func linkDetail(l *memory.PortableLink) string {
	return l.Relation + " " + assemble.Token(l.TargetID)
}

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

func foldRune(r rune) rune {
	for {
		r2 := unicode.SimpleFold(r)
		if r2 <= r {
			return r2
		}
		r = r2
	}
}

func actionFor(created bool) Action {
	if created {
		return ActionCreate
	}
	return ActionSkip
}

func contentClamped(cut bool) []string {
	if cut {
		return []string{"content"}
	}
	return nil
}

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

func (st step) run(ctx context.Context, s *memory.Store, report *ImportReport, onRecord func(RecordResult), created createdIDs) {
	var got outcome
	key := st.stepKey()
	switch {
	case st.check != nil && !st.check.available(st.check.projectID):
		got = outcome{action: ActionReject, err: fmt.Errorf(
			"project %s not found — the artifact carries no record for it and this store does not have it",
			memory.RefusalArg("project_id", st.check.projectID))}
	case created.has(st.kind, key):
		got = outcome{action: ActionSkip}
	default:
		got = st.runFunc(ctx, s, st.opts)
	}
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

func safeDetail(label, full, id string) string {
	if _, ok := secret.Detect(full); ok {
		return id
	}
	return label
}

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

const contentPreviewMax = 60

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

func topoOrder(recs []parsedRecord, idOf, refOf func(parsedRecord) string, clearRef func(parsedRecord) parsedRecord) []parsedRecord {
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

func sortTasksByID(t []memory.Task) {
	sort.SliceStable(t, func(i, j int) bool { return t[i].ID < t[j].ID })
}

func sortDecisionsByID(d []memory.Decision) {
	sort.SliceStable(d, func(i, j int) bool { return d[i].ID < d[j].ID })
}
