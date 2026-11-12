package supersede

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/memref"
)

// WithdrawStore is the store operations needed for targeted withdrawal.
type WithdrawStore interface {
	MemoryIDsByIDPrefix(ctx context.Context, projectID, prefix string) ([]string, error)
	MemoryIDsByIDPrefixAnyProject(ctx context.Context, prefix string) ([]string, error)
	LinksInto(ctx context.Context, projectID, memoryID, relation string) ([]memory.Link, error)
	GetByIDs(ctx context.Context, ids []string) ([]memory.Memory, error)
	InvalidateLink(ctx context.Context, sourceID, targetID, relation string) (int64, error)
}

// WithdrawPair names one edge to withdraw.
type WithdrawPair struct {
	Source   string
	Target   string
	Relation string
}

// WithdrawnLink describes one resolved withdrawal.
type WithdrawnLink struct {
	SourceID         string
	TargetID         string
	Relation         string
	TargetText       string
	LinkSource       string
	Strength         float32
	Withdrawn        bool
	TargetProjectID  string
	WithdrawalFailed bool
	NotAttempted     bool
}

// ProjectTargets groups repairable target ids by project.
type ProjectTargets struct {
	ProjectID string
	Targets   []string
}

// RepairableTargets returns deduplicated repairable supersedes targets grouped by project.
func RepairableTargets(links []WithdrawnLink) []ProjectTargets {
	rows := make([]RepairTarget, 0, len(links))
	seen := make(map[string]bool, len(links))
	for _, l := range links {
		if l.Relation != "" && l.Relation != string(RelationSupersedes) {
			continue
		}
		if l.TargetID == "" || seen[l.TargetID] || l.NotAttempted || l.WithdrawalFailed {
			continue
		}
		seen[l.TargetID] = true
		rows = append(rows, RepairTarget{ID: l.TargetID, ProjectID: l.TargetProjectID})
	}
	return GroupByProject(rows)
}

// RepairTarget identifies one repairable target and its project.
type RepairTarget struct {
	ID        string
	ProjectID string
}

// GroupByProject groups repair targets by project in first-seen order.
func GroupByProject(rows []RepairTarget) []ProjectTargets {
	var out []ProjectTargets
	index := make(map[string]int, len(rows))
	seen := make(map[string]bool, len(rows))
	for _, r := range rows {
		if r.ID == "" || seen[r.ID] {
			continue
		}
		seen[r.ID] = true
		i, ok := index[r.ProjectID]
		if !ok {
			i = len(out)
			index[r.ProjectID] = i
			out = append(out, ProjectTargets{ProjectID: r.ProjectID})
		}
		out[i].Targets = append(out[i].Targets, r.ID)
	}
	return out
}

// WithdrawResult summarizes a withdrawal request.
type WithdrawResult struct {
	Resolved  int
	Withdrawn int
	Links     []WithdrawnLink
}

// Withdraw resolves and withdraws the named edges.
func Withdraw(ctx context.Context, store WithdrawStore, projectID string, pairs []WithdrawPair, apply bool, logger *slog.Logger) (WithdrawResult, error) {
	var res WithdrawResult
	if len(pairs) == 0 {
		return res, fmt.Errorf("no pair to withdraw: name an edge as --withdraw <source-id> <target-id>")
	}

	var problems []string
	links := make([]WithdrawnLink, 0, len(pairs))
	for _, pair := range pairs {
		link, err := resolvePair(ctx, store, projectID, pair)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		links = append(links, link)
	}
	if len(problems) > 0 {
		return res, fmt.Errorf("nothing withdrawn: %s", strings.Join(problems, "; "))
	}
	res.Resolved = len(links)
	res.Links = links
	if err := attachTargetText(ctx, store, res.Links); err != nil {
		return WithdrawResult{}, err
	}

	if !apply {
		return res, nil
	}
	for i := range res.Links {
		n, err := store.InvalidateLink(ctx, res.Links[i].SourceID, res.Links[i].TargetID, res.Links[i].Relation)
		if err != nil {
			res.Links[i].WithdrawalFailed = true
			for j := i + 1; j < len(res.Links); j++ {
				res.Links[j].NotAttempted = true
			}
			return res, fmt.Errorf("withdraw %s link %s→%s: %w", res.Links[i].Relation, memory.RefusalArg("memory_id", res.Links[i].SourceID), memory.RefusalArg("memory_id", res.Links[i].TargetID), err)
		}
		if n == 0 {
			continue
		}
		res.Links[i].Withdrawn = true
		res.Withdrawn++
		if logger != nil {
			logger.Info("supersede withdrew a named edge",
				"source", memory.RefusalArg("memory_id", res.Links[i].SourceID), "target", memory.RefusalArg("memory_id", res.Links[i].TargetID),
				"relation", res.Links[i].Relation,
				"link_source", memory.RefusalArg("memory_id", res.Links[i].LinkSource), "withdrawn", res.Withdrawn)
		}
	}
	return res, nil
}

func attachTargetText(ctx context.Context, store WithdrawStore, links []WithdrawnLink) error {
	ids := make([]string, 0, len(links))
	seen := make(map[string]bool, len(links))
	for _, l := range links {
		if l.TargetID == "" || seen[l.TargetID] {
			continue
		}
		seen[l.TargetID] = true
		ids = append(ids, l.TargetID)
	}
	if len(ids) == 0 {
		return nil
	}
	mems, err := store.GetByIDs(ctx, ids)
	if err != nil {
		return fmt.Errorf("load withdrawn targets: %w", err)
	}
	type loaded struct {
		content string
		project string
	}
	byID := make(map[string]loaded, len(mems))
	for _, m := range mems {
		byID[m.ID] = loaded{content: m.Content, project: m.ProjectID}
	}
	for i := range links {
		links[i].TargetText = byID[links[i].TargetID].content
		links[i].TargetProjectID = byID[links[i].TargetID].project
	}
	return nil
}

func resolvePair(ctx context.Context, store WithdrawStore, projectID string, pair WithdrawPair) (WithdrawnLink, error) {
	var link WithdrawnLink
	if pair.Source == "" || pair.Target == "" {
		empty := "source"
		if pair.Target == "" {
			empty = "target"
		}
		return link, fmt.Errorf("the %s ref is empty in the pair (%q, %q)", empty, pair.Source, pair.Target)
	}
	targetID, err := resolveRef(ctx, store, projectID, "target", pair.Target)
	if err != nil {
		return link, err
	}
	into, err := store.LinksInto(ctx, projectID, targetID, "") // fetch all relations for auto/inspection
	if err != nil {
		return link, err
	}
	sourceID, err := resolveSource(ctx, store, projectID, into, pair.Source)
	if err != nil {
		return link, err
	}
	if sourceID == targetID {
		return link, fmt.Errorf("%s is linked to itself: both refs are memory %s", short(targetID), memory.RefusalArg("memory_id", targetID))
	}
	for _, want := range []string{string(RelationSupersedes), string(RelationCauses)} {
		if pair.Relation != "" && want != pair.Relation {
			continue
		}
		for _, l := range into {
			if l.SourceID == sourceID && l.Relation == want {
				return WithdrawnLink{
					SourceID:   l.SourceID,
					TargetID:   l.TargetID,
					Relation:   want,
					LinkSource: l.Source,
					Strength:   l.Strength,
				}, nil
			}
		}
	}
	return link, fmt.Errorf("no live %s link %s → %s in project %s%s",
		relationLabel(pair.Relation), short(sourceID), short(targetID), projectID, intoSuffix(into))
}

func relationLabel(relation string) string {
	if relation == "" {
		return "supersedes or causes"
	}
	return relation
}

func resolveSource(ctx context.Context, store WithdrawStore, projectID string, into []memory.Link, ref string) (string, error) {
	id, scopedErr := resolveRef(ctx, store, projectID, "source", ref)
	if scopedErr == nil {
		return id, nil
	}
	if !errors.Is(scopedErr, memref.ErrNoMatch) {
		return "", scopedErr
	}
	ids := make([]string, 0, len(into))
	seen := make(map[string]bool, len(into))
	for _, l := range into {
		if l.SourceID == "" || seen[l.SourceID] {
			continue
		}
		seen[l.SourceID] = true
		ids = append(ids, l.SourceID)
	}
	if id, err := memref.ResolveIn(ids, "source", ref); err == nil {
		return id, nil
	}
	return "", scopedErr
}

func resolveRef(ctx context.Context, store WithdrawStore, projectID, which, ref string) (string, error) {
	if projectID == memory.GlobalProjectID {
		ids, err := store.MemoryIDsByIDPrefixAnyProject(ctx, ref)
		if err != nil {
			return "", err
		}
		return memref.ResolveIn(ids, which, ref)
	}
	return memref.Resolve(ctx, store, projectID, which, ref)
}

func intoSuffix(links []memory.Link) string {
	if len(links) == 0 {
		return " (no memory in this project links it)"
	}
	parts := make([]string, 0, len(links))
	seen := make(map[string]bool, len(links))
	for _, l := range links {
		if seen[l.SourceID] {
			continue
		}
		seen[l.SourceID] = true
		parts = append(parts, short(l.SourceID))
	}
	suffix := " — withdraw that pair as well, or note that the other edge still buries it"
	if !hasSupersedes(links) {
		suffix = " — withdraw that pair as well; a 'causes' edge does not bury its target, " +
			"so nothing else here is holding it down"
	}
	return " (still " + relationWords(links) + " by " + strings.Join(parts, ", ") + suffix + ")"
}

func hasSupersedes(links []memory.Link) bool {
	for _, l := range links {
		if l.Relation == string(RelationSupersedes) {
			return true
		}
	}
	return false
}

func relationWords(links []memory.Link) string {
	hasSup, hasCauses := false, false
	for _, l := range links {
		switch l.Relation {
		case string(RelationSupersedes):
			hasSup = true
		case string(RelationCauses):
			hasCauses = true
		}
	}
	switch {
	case hasSup && hasCauses:
		return "linked (superseded or caused)"
	case hasCauses:
		return "caused"
	default:
		return "superseded"
	}
}

func short(id string) string {
	return memref.Short(id)
}
