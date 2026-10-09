package bench

// This file is the labelled offline session `ghost bench --audit` scores the
// retrieval audit against: forty memories, a scripted session of turns written in
// order, and for each turn the labels the corpus author wrote by hand — which
// memories it CITES (the full id is in the text), RESTATES (its words repeat the
// memory within this one turn), DENIES (a denial cue bound to the memory's wording
// or id) or SAVES (a save's arguments restate it).
//
// The labels are the corpus's and nothing the product computed. What the product
// is asked is only audit.Run's verdict, and the report compares that verdict with
// what the labels say it should be. Nothing here calls into internal/audit.
//
// The forty memory sentences and the narrative turns are the audit package's own
// realistic-session fixtures (run_session_test.go), transcribed verbatim, so the
// first golden is comparable with the figures docs/architecture.md carried before
// the bench existed. A test of that package cannot be imported from here, so they
// are copied; nothing compares the two copies.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// AuditSession is the session id every call and every scan in the bench carries.
const AuditSession = "bench-session"

// AuditKind is how a turn's text reaches the scanner: the three Add* entry points
// the stop hook uses.
type AuditKind string

const (
	AuditProse    AuditKind = "prose"
	AuditToolArgs AuditKind = "tool_args"
	AuditSaveArgs AuditKind = "save_args"
	// AuditToolCall is a whole tool call as the host wrote it: Tool is its name and
	// Text is its JSON arguments, so the bench drives the same routing the scanners
	// do (which words are prose, which are a save's, which ids are citations)
	// instead of choosing the entry point for the product.
	AuditToolCall AuditKind = "tool_call"
)

// AuditMemory is one stored memory: its id, its text, and the domain it is about
// relative to the scripted session ("dev" is the code the session edits, "ops" is
// something else).
type AuditMemory struct {
	ID      string
	Content string
	Domain  string
}

// AuditTurn is one item of the scripted session. Minute is its offset from turn 0;
// several items may share a minute, in list order. The label slices name memory
// ids.
type AuditTurn struct {
	Minute int
	Kind   AuditKind
	// Tool names the tool of an AuditToolCall turn, and is empty for every other kind.
	Tool     string
	Text     string
	Cites    []string
	Restates []string
	Denies   []string
	Saves    []string
	// Edits names a memory whose full id the turn holds as the argument of an EDIT
	// of that same memory. It is not a citation: the agent was rewriting the memory,
	// not relying on it, so no verdict follows from the label.
	Edits []string
}

// AuditCall is one retrieval the session made: the turn it is placed before (every
// turn at or after Minute is later than it), its source, and the domains it kept.
type AuditCall struct {
	Name    string
	Source  string
	Before  int
	Domains []string
}

// AuditCorpus is the whole fixture.
type AuditCorpus struct {
	Memories []AuditMemory
	Turns    []AuditTurn
	Calls    []AuditCall
}

// auditTurnCount is how many narrative minutes the session has.
const auditTurnCount = 40

// auditID is a deterministic 32 upper-hex id: the first sixteen bytes of the
// SHA-256 of a fixed name.
func auditID(name string) string {
	sum := sha256.Sum256([]byte("ghost bench --audit/" + name))
	return strings.ToUpper(hex.EncodeToString(sum[:16]))
}

func auditDevMemories() []string {
	return []string{
		"The retrieval record sink never fails the search when the write is refused",
		"A schema migration needs one frozen step per version and the schema string together",
		"The assembler stage order decides which verdicts reach the retrieval record",
		"The stop hook scans the session text and writes a sidecar of fingerprints",
		"Verdicts column holds a json array of kept and dropped rows per call",
		"Session id comes from the hook payload and the server environment",
		"The store refuses a schema newer than this build understands",
		"Record sink budget is a quarter of a second so the search is answered first",
		"Reflection consolidates memories and the resolve pass classifies them in batches",
		"The race detector run covers the memory package before every release",
		"Handler wiring passes the project id and session id down to the assembler",
		"Return errors wrapped with the operation name and the session id",
		"Context cancellation stops the write before the store lock is taken",
		"The package comment names the stage and the sink for the record",
		"Fingerprints are keyed hashes so a sidecar cannot be read as plain text",
		"An audit verdict is filed against the call that kept the memory",
		"Prose and tool arguments both feed the token arm of the comparison",
		"Tests build the real store because the write seam is the contract",
		"Assembler decisions record why a row was dropped at each stage",
		"Session scoped reads keep one session's calls apart from another's",
	}
}

func auditOpsMemories() []string {
	return []string{
		"Stake pool relays announce on port 3001 behind the tailscale mesh",
		"Grafana dashboards are generated from yaml with one panel per exporter",
		"KES keys rotate every ninety two days before the operational certificate expires",
		"Helmfile diff must run before apply on the production cluster",
		"The ogmios chart pins the node socket volume read only",
		"Prometheus scrape interval for cardano nodes is fifteen seconds",
		"SOPS age recipients live in the repository creation rules file",
		"Alertmanager routes block production alerts to the on call receiver",
		"The k3s agent joins with the tailscale auth key flag",
		"Mithril snapshots restore faster than syncing from genesis",
		"Cncli leaderlog needs the vrf signing key for epoch schedules",
		"Dingo block producer needs the opcert counter incremented on rotation",
		"Preprod faucet limits requests to one thousand test ada daily",
		"The relay topology file lists hot peers and local roots separately",
		"ARC runner scale sets use ephemeral pods labelled per repository",
		"Backups upload encrypted tarballs to object storage nightly",
		"Chrony keeps the block producer clock within ten milliseconds",
		"Ansible roles pin the docker compose version per host group",
		"Terraform state lives in a remote backend with locking enabled",
		"Cardano node release notes list the protocol parameter changes",
	}
}

// auditSpread is a memory whose distinctive words are spread over three turns,
// three in each, and never all in one. No single turn restates it, so the labels
// expect it ignored; a comparison that pools the whole session's words reads the
// union and calls it used. Each memory has at least ten distinctive words, so three
// in one turn is under a half of it.
type auditSpread struct {
	content string
	turns   [3]string
}

func auditSpreadMemories() []auditSpread {
	return []auditSpread{
		{"Kestrel gateway rotates bearer tokens nightly because upstream proxies cache stale certificates aggressively",
			[3]string{"Looked at the kestrel gateway rotates entry", "Then bearer tokens nightly in the config", "Upstream proxies cache was the last item"}},
		{"Quartz scheduler persists cron triggers inside postgres advisory locks preventing duplicate firing during failover events",
			[3]string{"The quartz scheduler persists setting", "Cron triggers inside the job table", "Postgres advisory locks came next"}},
		{"Mosaic thumbnails render lazily through worker threads whenever browsers report constrained bandwidth",
			[3]string{"Mosaic thumbnails render in the gallery", "Lazily worker threads for the grid", "Browsers report constrained values"}},
		{"Lantern feature flags default dark until product owners sign rollout checklists weekly",
			[3]string{"Lantern feature flags in the settings page", "Default dark product colours", "Owners sign rollout notes"}},
	}
}

func auditNarrative() []string {
	return []string{
		"I will read the assembler first and then change how the retrieval record is written by the sink",
		"The failing test shows the verdicts column holds stale rows, so the reader needs a session predicate",
		"Now the migration step: the schema string and one frozen step per version must change together",
		"Running the memory package tests with the race detector before touching the stop hook",
		"The hook payload carries the session id, and the server process carries it in its environment",
	}
}

// auditBody is the Edit/Write-shaped tool-call argument the realistic session
// carries on every turn: a whole file, which is what makes the session thousands of
// words long.
func auditBody(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "// Package handler%d wires the retrieval record sink into the assembler stage %d.\n", i, i)
		fmt.Fprintf(&b, "func (s *Store) writeVerdicts%d(ctx context.Context, rec RetrievalRecord) error {\n", i)
		fmt.Fprintf(&b, "\t// the schema migration guards the verdicts column so the sink never fails the search %d\n", i)
		fmt.Fprintf(&b, "\tif err := s.sink.RecordRetrieval(ctx, rec); err != nil { return fmt.Errorf(\"record session %d: %%w\", err) }\n", i)
		fmt.Fprintf(&b, "\treturn s.assembler.Stage(ctx, rec.ProjectID, rec.SessionID, rec.Verdicts)\n}\n")
	}
	return b.String()
}

// auditGiant is a tool argument of n distinct made-up words, followed by the given
// sentences: the shape of a generated file or a long agent instruction. The made-up
// words are letters only, four or more of them, so each is a token of its own.
func auditGiant(n int, sentences ...string) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString("zqxv")
		for v := i + 1; v > 0; v /= 26 {
			b.WriteByte(byte('a' + v%26))
		}
		b.WriteByte(' ')
	}
	for _, sent := range sentences {
		b.WriteString(sent)
		b.WriteByte(' ')
	}
	return b.String()
}

// NewAuditCorpus builds the fixture. Deterministic: every id is a hash of a fixed
// name and nothing reads a clock.
func NewAuditCorpus() AuditCorpus {
	var c AuditCorpus
	dev := make([]string, 0, 20)
	for i, content := range auditDevMemories() {
		id := auditID(fmt.Sprintf("dev/%02d", i))
		dev = append(dev, id)
		c.Memories = append(c.Memories, AuditMemory{ID: id, Content: content, Domain: "dev"})
	}
	ops := make([]string, 0, 20)
	for i, content := range auditOpsMemories() {
		id := auditID(fmt.Sprintf("ops/%02d", i))
		ops = append(ops, id)
		c.Memories = append(c.Memories, AuditMemory{ID: id, Content: content, Domain: "ops"})
	}

	// Memories whose words are spread over turns (see auditSpread), and the turns that
	// spread them, in the second half so the middle call follows them too.
	for i, sp := range auditSpreadMemories() {
		id := auditID(fmt.Sprintf("spread/%02d", i))
		c.Memories = append(c.Memories, AuditMemory{ID: id, Content: sp.content, Domain: "spread"})
		for j, text := range sp.turns {
			c.Turns = append(c.Turns, AuditTurn{Minute: 21 + 4*j + i%2, Kind: AuditProse, Text: text})
		}
	}

	// The narrative base: a prose sentence and a file body in every one of forty
	// minutes. None of it is labelled, so a memory the audit calls used because of it
	// is a false positive by the corpus's own account — which is the point of the
	// same-domain figure.
	narrative := auditNarrative()
	body := auditBody(4)
	for m := 0; m < auditTurnCount; m++ {
		c.Turns = append(c.Turns,
			AuditTurn{Minute: m, Kind: AuditProse, Text: narrative[m%len(narrative)]},
			AuditTurn{Minute: m, Kind: AuditToolArgs, Text: body})
	}

	add := func(t AuditTurn) { c.Turns = append(c.Turns, t) }

	// Twenty turns that restate nothing: plain working narration over words no
	// memory holds, one every two minutes.
	filler := []string{
		"Checking the formatting of the changelog entry before the commit",
		"Renaming a local variable so the loop reads better",
		"The linter wants a blank line above the comment",
		"Reordering the imports alphabetically",
		"Scrolling the diff to confirm nothing unrelated moved",
		"Adding a trailing newline to the generated file",
		"Splitting the long function into two shorter ones",
		"Using a table for the cases instead of five copies",
		"Deleting the unused constant the compiler reported",
		"Confirming the benchmark numbers are unchanged",
	}
	for i := 0; i < 20; i++ {
		add(AuditTurn{Minute: 2*i + 1, Kind: AuditProse, Text: filler[i%len(filler)]})
	}

	// Three turns citing an id: a dev memory early, an ops memory in the first half,
	// a dev memory late (so it reaches the middle call as well).
	//
	// The memories the labelled turns name are all ones the unlabelled base session
	// does NOT already match on words (measured: ten of the twenty dev memories are
	// matched by the base alone), so every positive label is a pair the audit has to
	// find, and none of them is found by the same-domain overlap the bench exists to
	// expose.
	add(AuditTurn{Minute: 5, Kind: AuditProse, Cites: []string{dev[10]},
		Text: "Following memory " + dev[10] + " when wiring the handler"})
	add(AuditTurn{Minute: 12, Kind: AuditProse, Cites: []string{ops[3]},
		Text: "Per memory " + strings.ToLower(ops[3]) + " the diff runs before the apply"})
	add(AuditTurn{Minute: 28, Kind: AuditToolArgs, Cites: []string{dev[15]},
		Text: "ghost_memory_search --note " + dev[15]})

	// Four turns that genuinely restate one dev memory each.
	add(AuditTurn{Minute: 3, Kind: AuditProse, Restates: []string{dev[16]},
		Text: "Prose and tool arguments both feed the token arm, so a pasted file body counts as the agent's own words"})
	add(AuditTurn{Minute: 10, Kind: AuditProse, Restates: []string{dev[14]},
		Text: "Fingerprints are keyed hashes, so the sidecar cannot be read back as plain text"})
	add(AuditTurn{Minute: 24, Kind: AuditProse, Restates: []string{dev[12]},
		Text: "Context cancellation stops the write before the store lock is taken, so check that first"})
	add(AuditTurn{Minute: 30, Kind: AuditProse, Restates: []string{dev[6]},
		Text: "The store refuses a schema newer than this build understands, which the new test covers"})

	// Two turns that deny one memory each: one by repeating its wording after the cue,
	// one by naming its id beside the cue.
	add(AuditTurn{Minute: 8, Kind: AuditProse, Denies: []string{dev[8]},
		Text: "Ignore this: reflection consolidates memories and the resolve pass classifies them in batches"})
	add(AuditTurn{Minute: 26, Kind: AuditProse, Denies: []string{dev[11]},
		Text: "Disregard memory " + dev[11] + " here"})

	// One save that restates a memory.
	add(AuditTurn{Minute: 32, Kind: AuditSaveArgs, Saves: []string{dev[19]},
		Text: "Session scoped reads keep one session's calls apart from another's"})

	// The false-positive classes the issue measured on real work, one hand-written case
	// each, kept in their own domain and their own call so the figures above keep the
	// denominators they had. The vocabulary is disjoint from the dev, ops and spread
	// memories, except where a case is ABOUT the session's own generic words, and its
	// turns come after the forty narrative minutes.
	//
	// Every case but the two guards is a memory no turn restates, cites or saves, so
	// the labels expect it ignored; the audit saying used is a false positive by the
	// corpus's own account. The guards are restatements the audit has to keep finding,
	// and one of them is a known cost, labelled as one.
	fpMem := func(name, content string) string {
		id := auditID("fp/" + name)
		c.Memories = append(c.Memories, AuditMemory{ID: id, Content: content, Domain: "fp"})
		return id
	}
	minute := auditTurnCount + 1

	// Generic project words: the session's own vocabulary is in every turn (the file
	// bodies name the record, the sink, the stage, the session and the verdicts each
	// minute), so a memory built from it is matched by any turn at all. The second is
	// the short form: every one of its four words is generic, so the floor of three is
	// the whole memory.
	fpMem("generic/long", "Session verdicts column rows keep the retrieval record stage")
	fpMem("generic/short", "Session record sink verdicts")

	// Code the agent wrote: forty consecutive edits of one file, each carrying the
	// same words in a comment and a log line, which is a third of the session's turns.
	// The memory is about that file's subject, and each edit repeats most of it. Twenty
	// edits (a fifth of the turns) is under the generic-word line and does not flip it.
	fpMem("code", "Ledger sweeper deletes orphaned snapshots before compaction starts")
	for i := 0; i < 40; i++ {
		add(AuditTurn{Minute: minute, Kind: AuditToolArgs, Text: fmt.Sprintf(
			"func (l *Ledger) pass%d(ctx context.Context) error {\n"+
				"\t// the ledger sweeper deletes orphaned snapshots before compaction starts\n"+
				"\tlog.Printf(\"ledger sweeper deletes orphaned snapshots before compaction starts, pass %d\")\n"+
				"\treturn l.next(ctx)\n}\n", i, i)})
		minute++
	}

	// One giant tool-argument turn: a generated file of two hundred distinct words that
	// happens to hold the words of two memories whole. A turn that size matches
	// whatever it is asked about.
	fpMem("giant/a", "Quorum arbiter rebalances shard leases whenever replicas lag beyond threshold")
	fpMem("giant/b", "Mirror cutover drains pending journals once standby promotes cleanly")
	add(AuditTurn{Minute: minute, Kind: AuditToolArgs, Text: auditGiant(200,
		"quorum arbiter rebalances shard leases whenever replicas lag beyond threshold",
		"mirror cutover drains pending journals once standby promotes cleanly")})
	minute++

	// An id inside an update of the same memory: the agent rewrote the memory and the
	// call carries its id as the argument that says which one. That is an edit and not
	// a citation. The rewrite shares two words with it, which no arm counts.
	editID := fpMem("edit", "Compactor watermark advances only after manifests are fsynced")
	add(AuditTurn{Minute: minute, Kind: AuditToolCall, Tool: "mcp__ghost__ghost_memory_update", Edits: []string{editID},
		Text: `{"memory_id":"` + editID + `","content":"Compactor watermark note reworded for clarity"}`})
	minute++

	// Guards. A memory of three distinctive words that one turn repeats whole is the
	// floor's own case and stays used; so does a long one restated in one turn.
	shortID := fpMem("true/short", "Basalt lantern cipher")
	add(AuditTurn{Minute: minute, Kind: AuditProse, Restates: []string{shortID},
		Text: "Rotating the basalt lantern cipher before the handover"})
	minute++
	longID := fpMem("true/long", "Orchard tether syncs bundles through relay brokers nightly")
	add(AuditTurn{Minute: minute, Kind: AuditProse, Restates: []string{longID},
		Text: "The orchard tether syncs bundles through the relay brokers nightly, so the handover waits"})
	minute++

	// The known cost of skipping a giant turn: a real restatement that shares its turn
	// with a giant file write is skipped with it. Labelled a use, because it is one, so
	// the report shows the miss instead of hiding it.
	costID := fpMem("cost", "Pylon rotor torque resets whenever gearbox oil warms")
	add(AuditTurn{Minute: minute, Kind: AuditProse, Restates: []string{costID},
		Text: "The pylon rotor torque resets whenever gearbox oil warms, which the next file relies on"})
	add(AuditTurn{Minute: minute, Kind: AuditToolArgs, Text: auditGiant(200)})

	c.Calls = []AuditCall{
		{Name: "start", Source: "session_start", Before: 0, Domains: []string{"dev", "ops", "spread"}},
		{Name: "middle", Source: "search", Before: auditTurnCount / 2, Domains: []string{"dev", "spread"}},
		{Name: "end", Source: "search", Before: auditTurnCount, Domains: []string{"dev", "spread"}},
		{Name: "fp", Source: "search", Before: 0, Domains: []string{"fp"}},
	}
	return c
}

// Expected is the class the labels say each memory a call kept should be judged,
// keyed by id. Only turns at or after the call count, and the precedence is the
// comparison's own (contradicted, identifier, token, save, ignored): a memory no
// later turn mentions is ignored.
func (c AuditCorpus) Expected(call AuditCall) map[string]AuditClass {
	var cited, restated, denied, saved = map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, t := range c.Turns {
		if t.Minute < call.Before {
			continue
		}
		for _, id := range t.Cites {
			cited[id] = true
		}
		for _, id := range t.Restates {
			restated[id] = true
		}
		for _, id := range t.Denies {
			denied[id] = true
		}
		for _, id := range t.Saves {
			saved[id] = true
		}
	}
	out := map[string]AuditClass{}
	for _, id := range c.Kept(call) {
		switch {
		case denied[id]:
			out[id] = ClassContradicted
		case cited[id]:
			out[id] = ClassUsedIdentifier
		case restated[id]:
			out[id] = ClassUsedToken
		case saved[id]:
			out[id] = ClassSuperseded
		default:
			out[id] = ClassIgnored
		}
	}
	return out
}

// Kept is the ids a call kept, in corpus order.
func (c AuditCorpus) Kept(call AuditCall) []string {
	var ids []string
	for _, m := range c.Memories {
		for _, d := range call.Domains {
			if m.Domain == d {
				ids = append(ids, m.ID)
				break
			}
		}
	}
	return ids
}
