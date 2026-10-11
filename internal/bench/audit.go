package bench

// This file measures the RETRIEVAL AUDIT: audit.Run's verdicts against a labelled
// corpus. Two corpora are scored and printed beside each other — the synthetic
// session in audit_corpus.go and the hand-labelled real-work set in
// audit_corpus_real.go — because a judge change that moves one and not the other
// is the thing the second set exists to show.
//
// Everything is offline and judge-free. A scratch in-memory store is seeded from Go
// literals; a scripted session is built straight into audit.Signals over a hasher
// with a FIXED key written in this file (never a key file, never the data
// directory); each call is recorded through the production sink and judged by
// audit.Run, which is the thing under test. Nothing in this file compares a memory
// with a turn — the only comparison is the verdict against the corpus's label, and
// that is a lookup.
//
// Nothing in the report is a wall-clock value or a path. A call's position is set
// by placing the turns around the instant it was recorded (the store stamps
// recorded_at with its own clock), so the figures are a function of the ORDER of
// the turns and not of when the run happened.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/audit"
	"github.com/wcatz/ghost/internal/hostevent"
	"github.com/wcatz/ghost/internal/memory"
)

// auditBenchKey is the fixed hasher key. It exists to make fingerprints a function
// of the text alone, so the report is the same on every machine.
var auditBenchKey = []byte("ghost-bench-audit-fixed-key-0001")

// auditProject is the project the scratch store files the corpus under.
const auditProject = "bench-audit"

// AuditClass is one of the five things a (call, memory) pair can be: the closed
// vocabulary the confusion table's rows and columns are drawn from.
type AuditClass string

const (
	ClassIgnored        AuditClass = "ignored"
	ClassUsedIdentifier AuditClass = "used/identifier"
	ClassUsedToken      AuditClass = "used/token"
	ClassSuperseded     AuditClass = "superseded_in_session"
	ClassContradicted   AuditClass = "contradicted"
)

// AuditClasses is the fixed report order.
var AuditClasses = []AuditClass{ClassUsedIdentifier, ClassUsedToken, ClassSuperseded, ClassContradicted, ClassIgnored}

// classOf is a verdict's class.
func classOf(v audit.Verdict) AuditClass {
	switch v.Outcome {
	case audit.OutcomeUsed:
		if v.Signal == audit.SignalIdentifier {
			return ClassUsedIdentifier
		}
		return ClassUsedToken
	case audit.OutcomeSuperseded:
		return ClassSuperseded
	case audit.OutcomeContradicted:
		return ClassContradicted
	}
	return ClassIgnored
}

// AuditPair is one (call, memory) pair: what the labels say, and what the audit
// said.
type AuditPair struct {
	MemoryID string
	Domain   string
	Expected AuditClass
	Got      AuditClass
}

// AuditClassScore is one class's figures. Precision is the pairs the audit put in
// the class that the labels agree with, over the pairs it put there; recall is the
// same agreement over the pairs the labels put there.
type AuditClassScore struct {
	Expected, Got int
	Precision     Ratio
	Recall        Ratio
}

// AuditScore is a set of pairs scored.
type AuditScore struct {
	Class map[AuditClass]AuditClassScore
	// Used pools the two used arms: the audit said used (either arm) and the labels
	// say used (either arm).
	Used AuditClassScore
	// Confusion[expected][got] counts pairs.
	Confusion map[AuditClass]map[AuditClass]int
}

// ScoreAudit scores pairs against their labels. It is a pure function of the
// pairs, so a test can hand it any list of verdicts and see what it reports.
func ScoreAudit(pairs []AuditPair) AuditScore {
	s := AuditScore{Class: map[AuditClass]AuditClassScore{}, Confusion: map[AuditClass]map[AuditClass]int{}}
	for _, e := range AuditClasses {
		s.Confusion[e] = map[AuditClass]int{}
	}
	isUsed := func(c AuditClass) bool { return c == ClassUsedIdentifier || c == ClassUsedToken }
	var usedHit int
	for _, p := range pairs {
		s.Confusion[p.Expected][p.Got]++
		if isUsed(p.Expected) {
			s.Used.Expected++
		}
		if isUsed(p.Got) {
			s.Used.Got++
			if isUsed(p.Expected) {
				usedHit++
			}
		}
	}
	s.Used.Precision = Ratio{usedHit, s.Used.Got}
	s.Used.Recall = Ratio{usedHit, s.Used.Expected}
	for _, cl := range AuditClasses {
		hit := s.Confusion[cl][cl]
		var exp, got int
		for _, other := range AuditClasses {
			exp += s.Confusion[cl][other]
			got += s.Confusion[other][cl]
		}
		s.Class[cl] = AuditClassScore{Expected: exp, Got: got, Precision: Ratio{hit, got}, Recall: Ratio{hit, exp}}
	}
	return s
}

// AuditScenario is one call, judged.
type AuditScenario struct {
	Name   string
	Source string
	Before int
	Kept   int
	// Dev is how many of the kept memories are in the session's own domain.
	Dev   int
	Pairs []AuditPair
	Score AuditScore
}

// AuditReport is the whole measurement.
type AuditReport struct {
	// Label names the corpus it measured. The bench scores two of them and prints
	// them beside each other, so a figure has to say which set it came from.
	Label     string
	Memories  int
	Turns     int
	Scenarios []AuditScenario
}

// scenario returns the named scenario, or the zero value.
func (r AuditReport) scenario(name string) AuditScenario {
	for _, s := range r.Scenarios {
		if s.Name == name {
			return s
		}
	}
	return AuditScenario{}
}

// startScenario returns the scenario named "start", and whether the report has one.
// The labelled offline corpus names its session-start call that, and every labelled
// turn follows it, so the corpus's own labels are all in its pairs; a report with no
// scenario of that name (the real-work corpus) has its figures read over every call
// instead (see Headlines). The name is the mechanism, not the call's position.
func (r AuditReport) startScenario() (AuditScenario, bool) {
	for _, s := range r.Scenarios {
		if s.Name == "start" {
			return s, true
		}
	}
	return AuditScenario{}, false
}

// AuditHeadlines are the figures the report leads with.
type AuditHeadlines struct {
	// SameDomainUsed is how many of the session's own-domain memories a session-start
	// call kept and the audit judged used.
	SameDomainUsed, SameDomainTotal int
	// UnlabelledUsed is the part of that figure the labels do not account for:
	// own-domain memories no turn cites, restates, denies or saves, judged used.
	// It is the figure the audit's own tests logged before the bench existed.
	UnlabelledUsed, UnlabelledTotal int
	// SpreadUsed is how many memories whose words are spread over three turns, and
	// whole in none, the session-start call judged used. The labels expect none.
	SpreadUsed, SpreadTotal int
	// CitesCaught is the cited ids the audit filed as used on the identifier arm,
	// over every pair the labels expect there, in the session-start call alone: it
	// is the one call every labelled turn follows, so the denominators are the
	// corpus's own cites and restatements and one source is never pooled with another.
	CitesCaught, CitesTotal int
	// RestatementsCaught is the restatements the audit filed as used on the token
	// arm, over every pair the labels expect there, in the session-start call alone.
	RestatementsCaught, RestatementsTotal int
	// FalseUsed is the pairs the labels expect ignored that the audit judged used,
	// over the same population the two catch lines read: the corpus's own
	// false-positive figure, which is the number a judge change moves.
	FalseUsed, FalseTotal int
	// TokenPrecision is the token arm's precision over that population: the pairs
	// it filed used-by-wording that the labels agree with, over every pair it
	// filed there.
	TokenPrecision Ratio
	// SessionStart reports whether the report has a scenario named "start". The
	// labelled offline session does, so every labelled turn follows it and its
	// pairs carry the whole corpus's labels. The real-work corpus has none, so its
	// catch lines are read over all of its calls, and the two domain lines below
	// have no population to be a fraction of and are not printed.
	SessionStart bool
}

// Headlines reads the figures the report leads with. The two catch lines are read
// off the session-start call when the corpus has one — the only call every labelled
// turn follows, so its pairs carry the whole corpus's labels — and over every call
// when it does not, which is the only population such a corpus has.
func (r AuditReport) Headlines() AuditHeadlines {
	var h AuditHeadlines
	start, ok := r.startScenario()
	var pairs []AuditPair
	if ok {
		h.SessionStart = true
		pairs = start.Pairs
	} else {
		for _, sc := range r.Scenarios {
			pairs = append(pairs, sc.Pairs...)
		}
	}
	var tokenHit, tokenGot int
	for _, p := range pairs {
		switch p.Expected {
		case ClassUsedIdentifier:
			h.CitesTotal++
			if p.Got == p.Expected {
				h.CitesCaught++
			}
		case ClassUsedToken:
			h.RestatementsTotal++
			if p.Got == p.Expected {
				h.RestatementsCaught++
			}
		}
		if p.Got == ClassUsedToken {
			tokenGot++
			if p.Expected == ClassUsedToken {
				tokenHit++
			}
		}
		if p.Expected == ClassIgnored {
			h.FalseTotal++
			if p.Got == ClassUsedIdentifier || p.Got == ClassUsedToken {
				h.FalseUsed++
			}
		}
		if !h.SessionStart {
			continue
		}
		if p.Domain == "spread" {
			h.SpreadTotal++
			if p.Got == ClassUsedIdentifier || p.Got == ClassUsedToken {
				h.SpreadUsed++
			}
		}
		if p.Domain != "dev" {
			continue
		}
		h.SameDomainTotal++
		used := p.Got == ClassUsedIdentifier || p.Got == ClassUsedToken
		if used {
			h.SameDomainUsed++
		}
		if p.Expected == ClassIgnored {
			h.UnlabelledTotal++
			if used {
				h.UnlabelledUsed++
			}
		}
	}
	h.TokenPrecision = Ratio{tokenHit, tokenGot}
	return h
}

// RunAuditTemp runs every call of the labelled offline session, each against its
// own scratch store.
func RunAuditTemp(ctx context.Context) (AuditReport, error) {
	return runAuditCorpus(ctx, NewAuditCorpus(), auditLabelSynthetic)
}

// RunRealAuditTemp runs every call of the hand-labelled real-work corpus, each
// against its own scratch store.
func RunRealAuditTemp(ctx context.Context) (AuditReport, error) {
	return runAuditCorpus(ctx, NewRealAuditCorpus(), auditLabelReal)
}

// The two corpora the audit bench scores, named in the report header so a figure
// printed beside the other set's can be told apart. Nothing in the measurement
// reads them.
const (
	auditLabelSynthetic = "labelled offline session"
	auditLabelReal      = "hand-labelled real sessions"
)

// runAuditCorpus runs every call of the corpus, each against its own scratch
// store. The corpus is a parameter because the judge is measured on two of them:
// the synthetic session the audit package's own fixtures build, and a set labelled
// by hand from real working sessions. A change that moves one and not the other is
// exactly what the second set exists to show, so neither report is reachable
// except through this one function.
func runAuditCorpus(ctx context.Context, c AuditCorpus, label string) (AuditReport, error) {
	rep := AuditReport{Label: label, Memories: len(c.Memories)}
	rep.Turns = len(c.Turns)
	for _, call := range c.Calls {
		sc, err := runAuditCall(ctx, c, call)
		if err != nil {
			return AuditReport{}, fmt.Errorf("audit bench: %s: %w", call.Name, err)
		}
		rep.Scenarios = append(rep.Scenarios, sc)
	}
	return rep, nil
}

func runAuditCall(ctx context.Context, c AuditCorpus, call AuditCall) (AuditScenario, error) {
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		return AuditScenario{}, err
	}
	store := memory.NewStore(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer store.Close() //nolint:errcheck

	if err := store.EnsureProject(ctx, auditProject, "bench/audit", auditProject); err != nil {
		return AuditScenario{}, err
	}
	byID := map[string]AuditMemory{}
	for _, m := range c.Memories {
		byID[m.ID] = m
		if _, err := store.CreateWithID(ctx, auditProject, m.ID, memory.Memory{Content: m.Content, Category: "fact", Source: "manual"}); err != nil {
			return AuditScenario{}, fmt.Errorf("seed %s: %w", m.ID, err)
		}
	}

	kept := c.Kept(call)
	verdicts := make([]memory.RowVerdict, 0, len(kept))
	for _, id := range kept {
		verdicts = append(verdicts, memory.RowVerdict{ID: id, Kept: true, Stage: "fit", Reason: "fit_response"})
	}
	if err := store.RecordRetrieval(ctx, memory.RetrievalRecord{
		ProjectID: auditProject, SessionID: AuditSession, Source: call.Source, Outcome: "answerable", Verdicts: verdicts,
	}); err != nil {
		return AuditScenario{}, fmt.Errorf("record the call: %w", err)
	}

	h, err := audit.NewHasher(auditBenchKey)
	if err != nil {
		return AuditScenario{}, err
	}
	s := audit.NewWithHasher(h)
	s.SetSessionID(AuditSession)
	// Taken AFTER the call was recorded, so the store's stamp (to the second, and the
	// call is taken at the end of that second) is never later than this instant. A
	// turn is placed half a minute past its minute's offset: one at or after the
	// call is clearly after it, one before is clearly before.
	now := time.Now()
	for _, t := range c.Turns {
		s.SetAt(now.Add(time.Duration(t.Minute-call.Before)*time.Minute + 30*time.Second))
		switch t.Kind {
		case AuditProse:
			s.AddProse(t.Text)
		case AuditToolArgs:
			s.AddToolArgs(t.Text)
		case AuditSaveArgs:
			s.AddSaveArgs(t.Text)
		case AuditToolCall:
			hostevent.AddAuditToolCall(s, t.Tool, json.RawMessage(t.Text))
		default:
			return AuditScenario{}, fmt.Errorf("unknown turn kind %q", t.Kind)
		}
	}

	// The signals reach audit.Run the way the stop hook's child receives them: written
	// to a sidecar and read back under the same key, so the file format is on the path
	// this bench measures. The file lives in a scratch directory removed on return.
	dir, err := os.MkdirTemp("", "ghost-bench-audit-")
	if err != nil {
		return AuditScenario{}, err
	}
	defer os.RemoveAll(dir) //nolint:errcheck
	path, err := audit.WriteSidecar(dir, s)
	if err != nil {
		return AuditScenario{}, fmt.Errorf("write the sidecar: %w", err)
	}
	s, err = audit.ReadSidecar(path, h)
	if err != nil {
		return AuditScenario{}, fmt.Errorf("read the sidecar back: %w", err)
	}

	sum, err := audit.Run(ctx, store, auditProject, s)
	if err != nil {
		return AuditScenario{}, err
	}
	if sum.Verdicts != len(kept) {
		return AuditScenario{}, fmt.Errorf("judged %d of %d kept memories (unreadable %d, unfiled %d, no order %v)", sum.Verdicts, len(kept), sum.Unreadable, sum.Unfiled, sum.NoOrder)
	}
	got := map[string]AuditClass{}
	for _, v := range sum.VerdictList {
		got[v.MemoryID] = classOf(v)
	}

	want := c.Expected(call)
	sc := AuditScenario{Name: call.Name, Source: call.Source, Before: call.Before, Kept: len(kept)}
	for _, id := range kept {
		m := byID[id]
		if m.Domain == "dev" {
			sc.Dev++
		}
		sc.Pairs = append(sc.Pairs, AuditPair{MemoryID: id, Domain: m.Domain, Expected: want[id], Got: got[id]})
	}
	sc.Score = ScoreAudit(sc.Pairs)
	return sc, nil
}

// FormatAudit renders the report. Sorted by construction (fixed class and scenario
// order), with no time, path or map-order dependence.
func FormatAudit(rep AuditReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "retrieval audit (%s): %d memories, %d scripted turn items, %d calls\n", rep.Label, rep.Memories, rep.Turns, len(rep.Scenarios))
	b.WriteString("  audit.Run over a labelled corpus, scored against that corpus's own labels;\n")
	b.WriteString("  no model, no network, no real store. A label is a memory the turn cites, restates, denies or saves\n\n")

	h := rep.Headlines()
	// The two domain lines are a property of the synthetic session's own domains,
	// so a corpus that has none prints nothing for them rather than a zero over
	// an empty population.
	if h.SessionStart {
		fmt.Fprintf(&b, "same-domain used at session start: %d of %d\n", h.SameDomainUsed, h.SameDomainTotal)
		fmt.Fprintf(&b, "  of which no turn labels them: %d of %d\n", h.UnlabelledUsed, h.UnlabelledTotal)
	}
	fmt.Fprintf(&b, "cited ids caught: %d of %d\n", h.CitesCaught, h.CitesTotal)
	fmt.Fprintf(&b, "restatements caught: %d of %d\n", h.RestatementsCaught, h.RestatementsTotal)
	fmt.Fprintf(&b, "false \"used\" (labels expect ignored): %d of %d\n", h.FalseUsed, h.FalseTotal)
	fmt.Fprintf(&b, "token-arm precision: %s\n", h.TokenPrecision)
	if h.SessionStart {
		fmt.Fprintf(&b, "spread across turns, judged used: %d of %d\n", h.SpreadUsed, h.SpreadTotal)
	}

	for _, sc := range rep.Scenarios {
		fmt.Fprintf(&b, "\nscenario %s: the call is placed before turn %d, source %s, keeps %d memories (%d same-domain)\n",
			sc.Name, sc.Before, sc.Source, sc.Kept, sc.Dev)
		fmt.Fprintf(&b, "  %-22s %8s %5s  %-16s %-16s\n", "outcome", "expected", "got", "precision", "recall")
		row := func(name string, s AuditClassScore) {
			fmt.Fprintf(&b, "  %-22s %8d %5d  %-16s %-16s\n", name, s.Expected, s.Got, s.Precision, s.Recall)
		}
		for _, cl := range AuditClasses {
			row(string(cl), sc.Score.Class[cl])
			if cl == ClassUsedToken {
				row("used (either arm)", sc.Score.Used)
			}
		}
		b.WriteString("  confusion (rows: expected, columns: got)\n")
		fmt.Fprintf(&b, "  %-22s", "")
		for _, cl := range AuditClasses {
			fmt.Fprintf(&b, " %9s", shortClass(cl))
		}
		b.WriteString("\n")
		for _, e := range AuditClasses {
			fmt.Fprintf(&b, "  %-22s", e)
			for _, g := range AuditClasses {
				fmt.Fprintf(&b, " %9d", sc.Score.Confusion[e][g])
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

func shortClass(c AuditClass) string {
	switch c {
	case ClassUsedIdentifier:
		return "used/id"
	case ClassUsedToken:
		return "used/tok"
	case ClassSuperseded:
		return "supersed"
	case ClassContradicted:
		return "contrad"
	}
	return "ignored"
}
