package memory

import (
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"
)

// CheckImportedProject validates a project record for import.
func CheckImportedProject(p PortableProject) error {
	if p.ID == "" {
		return fmt.Errorf("project id is required")
	}
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
	if p.Path == "" {
		return fmt.Errorf("path is required")
	}
	if p.Name == "" {
		return fmt.Errorf("name is required")
	}
	// export, so a token in it travels exactly as one in the name would.
	// export are where it always holds. It is also what covers a row an older
	return rejectSecretFields(
		secretField{"name", p.Name},
		secretField{"path", p.Path},
		secretField{"repo_remote", p.RepoRemote},
	)
}

// ValidityRefusalError reports a refused validity or confidence value.
type ValidityRefusalError struct{ Err error }

func (e *ValidityRefusalError) Error() string { return e.Err.Error() }
func (e *ValidityRefusalError) Unwrap() error { return e.Err }

// ImportedValidity parses and validates imported memory validity fields.
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

// EvidenceRefusalError reports a refused evidence record.
type EvidenceRefusalError struct{ Err error }

func (e *EvidenceRefusalError) Error() string { return e.Err.Error() }
func (e *EvidenceRefusalError) Unwrap() error { return e.Err }

// ImportedEvidence parses and validates an imported evidence record.
func ImportedEvidence(index int, e PortableEvidence) (PortableEvidence, error) {
	observed, err := NormalizeImportedStamp(fmt.Sprintf("evidence[%d].observed_at", index), e.ObservedAt, false)
	if err != nil {
		return PortableEvidence{}, err
	}
	verified, err := NormalizeImportedStamp(fmt.Sprintf("evidence[%d].verified_at", index), e.VerifiedAt, false)
	if err != nil {
		return PortableEvidence{}, err
	}
	if e.Confidence != nil {
		if err := CheckConfidence(*e.Confidence); err != nil {
			return PortableEvidence{}, fmt.Errorf("evidence[%d].%w", index, err)
		}
	}
	e.ObservedAt, e.VerifiedAt = observed, verified
	return e, nil
}

// CheckImportedMemory validates a memory record for import.
func CheckImportedMemory(m PortableMemory) error {
	if m.ID == "" {
		return fmt.Errorf("memory id is required")
	}
	if err := CheckImportedID(m.ID); err != nil {
		return err
	}
	if m.ProjectID == "" {
		return fmt.Errorf("project_id is required")
	}
	if m.Content == "" {
		return fmt.Errorf("content is required")
	}
	if !IsValidCategory(m.Category) {
		return fmt.Errorf("invalid category %s — must be one of: architecture, decision, pattern, convention, gotcha, dependency, preference, fact", RefusalArg("category", m.Category))
	}
	if !IsValidSource(m.Source) {
		return fmt.Errorf("invalid source %s — must be one of: reflection, chat, manual, tool, mcp, onboarding, decision_log, builtin", RefusalArg("source", m.Source))
	}
	if m.Retention != "" && !IsValidRetention(m.Retention) {
		return InvalidRetentionError(m.Retention)
	}
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
	if _, err := ImportedValidity(m); err != nil {
		return &ValidityRefusalError{Err: err}
	}
	if m.Confidence != nil {
		if err := CheckConfidence(*m.Confidence); err != nil {
			return &ValidityRefusalError{Err: err}
		}
	}
	if err := checkImportedTimestamp("created_at", m.CreatedAt, timestampLayouts("created_at")); err != nil {
		return err
	}
	if err := checkImportedTimestamp("updated_at", m.UpdatedAt, timestampLayouts("updated_at")); err != nil {
		return err
	}
	if m.LastAccessed != nil {
		if err := rejectSecretFields(secretField{"last_accessed", *m.LastAccessed}); err != nil {
			return err
		}
		if err := checkImportedTimestamp("last_accessed", *m.LastAccessed, timestampLayouts("last_accessed")); err != nil {
			return err
		}
	}
	if m.ResolvedAt != nil {
		if err := rejectSecretFields(secretField{"resolved_at", *m.ResolvedAt}); err != nil {
			return err
		}
		if err := checkImportedTimestamp("resolved_at", *m.ResolvedAt, timestampLayouts("resolved_at")); err != nil {
			return err
		}
	}
	if _, err := boundedSourceRef(m.SourceRef); err != nil {
		return err
	}
	if _, err := boundedAgent(m.Agent); err != nil {
		return err
	}
	if err := rejectSecretList("tags", m.Tags); err != nil {
		return err
	}
	if err := checkScope(m.Scope); err != nil {
		return err
	}
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
		if _, err := ImportedEvidence(i, e); err != nil {
			return &EvidenceRefusalError{Err: err}
		}
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

// CheckImportedTask validates a task record for import.
func CheckImportedTask(t Task) error {
	if t.ID == "" {
		return fmt.Errorf("task id is required")
	}
	if err := CheckImportedID(t.ID); err != nil {
		return err
	}
	if t.ProjectID == "" {
		return fmt.Errorf("project_id is required")
	}
	if strings.TrimSpace(t.Title) == "" {
		return fmt.Errorf("title is required")
	}
	if !validTaskStatuses[t.Status] {
		return fmt.Errorf("invalid status %s — must be one of: pending, active, done, blocked", RefusalArg("status", t.Status))
	}
	if t.Priority < 0 || t.Priority > 4 {
		return fmt.Errorf("invalid priority %d — must be between 0 and 4", t.Priority)
	}
	if err := rejectSecretFields(secretField{"branch", t.Branch}); err != nil {
		return err
	}
	if len(t.Branch) > MaxBranchLen {
		return fmt.Errorf("branch must be at most %d bytes, got %d — it is stored alongside the task's text and reaches the same renderers", MaxBranchLen, len(t.Branch))
	}
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

// CheckImportedDecision validates a decision record for import.
func CheckImportedDecision(d Decision) error {
	if d.ID == "" {
		return fmt.Errorf("decision id is required")
	}
	if err := CheckImportedID(d.ID); err != nil {
		return err
	}
	if strings.Contains(d.ID, decisionRefSeparator) {
		return fmt.Errorf("decision id must not contain %q — it separates the decision links a memory carries, so an id holding it names no decision. Give the decision a new id in the artifact", decisionRefSeparator)
	}
	if d.ProjectID == "" {
		return fmt.Errorf("project_id is required")
	}
	if strings.TrimSpace(d.Title) == "" {
		return fmt.Errorf("title is required")
	}
	if strings.TrimSpace(d.Decision) == "" {
		return fmt.Errorf("decision is required")
	}
	if strings.TrimSpace(d.Rationale) == "" {
		return fmt.Errorf("rationale is required")
	}
	if !validDecisionStatuses[d.Status] {
		return fmt.Errorf("invalid status %s — must be one of: active, superseded, revisit", RefusalArg("status", d.Status))
	}
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
	return rejectSecretList("tags", d.Tags)
}

// CheckImportedLink validates a link record for import.
func CheckImportedLink(l PortableLink) error {
	if l.SourceID == "" {
		return fmt.Errorf("link source_id is required")
	}
	if err := CheckImportedID(l.SourceID); err != nil {
		return fmt.Errorf("link source_id: %w", err)
	}
	if l.TargetID == "" {
		return fmt.Errorf("link target_id is required")
	}
	if err := CheckImportedID(l.TargetID); err != nil {
		return fmt.Errorf("link target_id: %w", err)
	}
	if l.SourceID == l.TargetID {
		return fmt.Errorf("link source_id and target_id are the same memory — an edge needs two ends")
	}
	if !carriedLinkRelation(l.Relation) {
		return fmt.Errorf("invalid link relation %s — the artifact carries %s; the other relations are derived, and a worker rebuilds them after an import",
			RefusalArg("relation", l.Relation), strings.Join(CarriedLinkRelations(), " and "))
	}
	if !carriedLinkSource(l.Source) {
		return fmt.Errorf("invalid link source %s — must be one of: auto, llm, manual", RefusalArg("source", l.Source))
	}
	if math.IsNaN(l.Strength) || math.IsInf(l.Strength, 0) {
		return fmt.Errorf("link strength must be a finite number between 0 and 1, got %v", l.Strength)
	}
	if l.Strength < 0 || l.Strength > 1 {
		return fmt.Errorf("link strength must be between 0 and 1, got %v", l.Strength)
	}
	if err := checkImportedTimestamp("created_at", l.CreatedAt, timestampLayouts("created_at")); err != nil {
		return err
	}
	return nil
}

func createdProject(id, path, name, repoRemote string) PortableProject {
	if path == "" {
		path = id
	}
	return PortableProject{ID: id, Name: name, Path: path, RepoRemote: repoRemote}
}

// MaxImportedIDLen bounds imported record identifiers.
const MaxImportedIDLen = 128

// MaxBranchLen bounds an imported task branch.
const MaxBranchLen = MaxContentLen

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

func timestampLayouts(field string) []string {
	switch field {
	case "last_accessed":
		return append(append([]string{}, StampLayouts...), time.RFC3339)
	default:
		return StampLayouts
	}
}

// CheckImportedID validates an imported record identifier.
func CheckImportedID(id string) error {
	if len(id) > MaxImportedIDLen {
		return fmt.Errorf("record id must be at most %d bytes, got %d — it names a row, not a document, and a shortened one would name a different row",
			MaxImportedIDLen, len(id))
	}
	if unprintableInIdentifier(id, true) != "" {
		return fmt.Errorf("record id must hold no control character, whitespace, backtick or «» — it is printed " +
			"outside the «...» data delimiters on every listing, so one of those forges a line or a data block of its " +
			"own. The offending id is not shown, because it is the value being refused. " +
			"Give the record a new id in the artifact")
	}
	return nil
}

// line-ending class.
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

// CheckImportedProjectText validates imported project text fields.
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

// line, a backtick span or a «...» data block, and the reason otherwise.
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
