// Package portable reads and writes the portable JSONL artifact that moves
// memories, tasks, decisions and projects between Ghost installations.
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

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/secret"
)

// SchemaVersion is the version of the artifact format this build writes and
// reads.
const SchemaVersion = 3

const minReadableSchemaVersion = 1

var knownFeatures = map[string]bool{
	"retention": true,
	"links":     true,
}

func declaredFeatures() []string {
	out := make([]string, 0, len(knownFeatures))
	for name := range knownFeatures {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

const (
	TypeHeader   = "header"
	TypeProject  = "project"
	TypeMemory   = "memory"
	TypeTask     = "task"
	TypeDecision = "decision"
	TypeLink     = "link"
)

type record struct {
	Type     string                  `json:"type"`
	Project  *memory.PortableProject `json:"project,omitempty"`
	Memory   *memory.PortableMemory  `json:"memory,omitempty"`
	Task     *memory.Task            `json:"task,omitempty"`
	Decision *memory.Decision        `json:"decision,omitempty"`
	Link     *memory.PortableLink    `json:"link,omitempty"`

	SchemaVersion int `json:"schema_version,omitempty"`

	Features []string `json:"features,omitempty"`
}

func headerLine() []byte {
	b, _ := json.Marshal(record{Type: TypeHeader, SchemaVersion: SchemaVersion, Features: declaredFeatures()})
	return b
}

// Export writes the store as an artifact: a header line, then every project,
// memory, link, task and decision.
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

	keptMemories := make([]memory.PortableMemory, 0, len(memories))
	for _, m := range memories {
		reason, secret, scope, scopeChar, orphan, validity, evidence := "", false, false, false, false, false, false
		switch {
		case orphaned(m.ProjectID):
			reason, orphan = orphanReason, true
		default:
			err := memory.CheckImportedMemory(m)
			reason, secret = exportRefusal(err)
			var bound *memory.ScopeBoundError
			var sch *memory.ScopeKeyValueError
			var vr *memory.ValidityRefusalError
			var er *memory.EvidenceRefusalError
			scope = errors.As(err, &bound)
			scopeChar = errors.As(err, &sch)
			validity = errors.As(err, &vr)
			evidence = errors.As(err, &er)
		}
		if reason != "" {
			skipped = append(skipped, SkippedRecord{Type: TypeMemory, ID: m.ID, Reason: reason, Secret: secret, Scope: scope, ScopeChar: scopeChar, Orphan: orphan, Validity: validity, Evidence: evidence})
			continue
		}
		keptMemories = append(keptMemories, m)
	}
	keptTasks := make([]memory.Task, 0, len(tasks))
	for _, t := range tasks {
		reason, secret, orphan := "", false, false
		switch {
		case orphaned(t.ProjectID):
			reason, orphan = orphanReason, true
		default:
			reason, secret = exportRefusal(memory.CheckImportedTask(t))
		}
		if reason != "" {
			skipped = append(skipped, SkippedRecord{Type: TypeTask, ID: t.ID, Reason: reason, Secret: secret, Orphan: orphan})
			continue
		}
		keptTasks = append(keptTasks, t)
	}
	keptDecisions := make([]memory.Decision, 0, len(decisions))
	for _, d := range decisions {
		reason, secret, orphan := "", false, false
		switch {
		case orphaned(d.ProjectID):
			reason, orphan = orphanReason, true
		default:
			reason, secret = exportRefusal(memory.CheckImportedDecision(d))
		}
		if reason != "" {
			skipped = append(skipped, SkippedRecord{Type: TypeDecision, ID: d.ID, Reason: reason, Secret: secret, Orphan: orphan})
			continue
		}
		keptDecisions = append(keptDecisions, d)
	}

	links, err := s.PortableLinks(ctx, ids)
	if err != nil {
		return Stats{}, fmt.Errorf("export links: %w", err)
	}
	carried := make(map[string]bool, len(keptMemories))
	for _, m := range keptMemories {
		carried[m.ID] = true
	}
	keptLinks := make([]memory.PortableLink, 0, len(links))
	linksLeftOut := 0
	for _, l := range links {
		if !carried[l.SourceID] || !carried[l.TargetID] {
			linksLeftOut++
			continue
		}
		if reason, secret := exportRefusal(memory.CheckImportedLink(l)); reason != "" {
			skipped = append(skipped, SkippedRecord{Type: TypeLink, ID: l.SourceID, Detail: linkDetail(&l), Reason: reason, Secret: secret})
			continue
		}
		keptLinks = append(keptLinks, l)
	}

	stats := Stats{
		Projects:  len(keptProjects),
		Memories:  len(keptMemories),
		Links:     len(keptLinks),
		Tasks:     len(keptTasks),
		Decisions: len(keptDecisions),
	}
	if linksLeftOut > 0 {
		stats.LinksLeftOut = linksLeftOut
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
	for i := range keptLinks {
		l := keptLinks[i]
		if err := write(record{Type: TypeLink, Link: &l}); err != nil {
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
type SkippedRecord struct {
	Type      string
	ID        string
	Detail    string
	Reason    string
	Secret    bool
	Scope     bool
	ScopeChar bool
	Orphan    bool
	Validity  bool
	Evidence  bool
}

const orphanReason = "the project it names was left out of this artifact"

// Stats is what an export wrote, and what it deliberately left out.
type Stats struct {
	Projects     int
	Memories     int
	Links        int
	LinksLeftOut int
	Tasks        int
	Decisions    int
	Skipped      []SkippedRecord
}

func exportRefusal(err error) (reason string, secret bool) {
	if err == nil {
		return "", false
	}
	var bound *memory.ScopeBoundError
	if errors.As(err, &bound) {
		return scopeBoundReason(bound), false
	}
	var scopeChar *memory.ScopeKeyValueError
	if errors.As(err, &scopeChar) {
		return scopeCharReason(scopeChar), false
	}
	var refused *memory.SecretContentError
	if errors.As(err, &refused) {
		return fmt.Sprintf("its %s is credential-shaped, and ghost import refuses to store one by design — Ghost never stores a credential value",
			refused.Field), true
	}
	return "ghost import refuses it: " + err.Error(), false
}

func scopeCharReason(e *memory.ScopeKeyValueError) string {
	reason := "ghost import refuses it: " + e.Error()
	if e.Key != "" {
		if _, shaped := secret.Detect(e.Key); shaped {
			reason += " (one scope value; its key is not shown)"
		} else {
			reason += fmt.Sprintf(" (the scope key is %s)", memory.SafeToken(e.Key))
		}
	}
	return reason
}

func scopeBoundReason(e *memory.ScopeBoundError) string {
	reason := "ghost import refuses it: " + e.Error()
	if e.Kind == memory.ScopeValueTooLong {
		if _, shaped := secret.Detect(e.Key); shaped {
			reason += " (one scope value; its key is not shown)"
		} else {
			reason += fmt.Sprintf(" (the scope key is %s)", memory.SafeToken(e.Key))
		}
	}
	return reason
}

func selectProjects(projects []memory.PortableProject, filter string) ([]memory.PortableProject, error) {
	if filter == "" {
		return projects, nil
	}
	for _, p := range projects {
		if p.ID == filter {
			return []memory.PortableProject{p}, nil
		}
	}
	var out []memory.PortableProject
	for _, p := range projects {
		if p.Name == filter {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no project matches %s", memory.RefusalArg("project", filter))
	}
	if len(out) > 1 {
		ids := make([]string, 0, len(out))
		for _, p := range out {
			ids = append(ids, memory.RefusalArg("project_id", p.ID))
		}
		return nil, fmt.Errorf("project filter %s matches multiple projects by name: %s — name a project by its unique id",
			memory.RefusalArg("project", filter), strings.Join(ids, ", "))
	}
	return out, nil
}

const exportListLimit = 1000000

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
