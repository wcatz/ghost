package hostevent

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/audit"
)

// The audit reads a transcript for what the AGENT DID, and the whole design is
// the line between the agent's own words and the text Ghost handed it. So the
// fixtures below put the memory's wording in both places and ask which one the
// scan counted — a scan that read the injected block would report every
// retrieval as used, which is the one result that makes the audit worthless.
const auditMemoryID = "D20E133860CC4AFE38B485AD5371BA59"

// injectedText is what the agent was SHOWN: the memory's wording arriving as a
// tool result. It must not make the memory count as used.
const injectedText = "the opencode plugin materializes its transcript under mkdtemp and rm-rfs the directory on hook close"

// agentText is what the agent SAID, in its own words, and it is what does count.
const agentText = "as I read it, the opencode plugin materializes its transcript under mkdtemp, so the sidecar has to be written synchronously"

// auditTestKey is the per-install key this package's audit tests sign with. It is
// a literal because it is not a secret and because a fixture that had to provision
// a real key could only assert the same thing more slowly; what matters is that
// both the scan and the comparison in the helpers below use this one, and that a
// test which needs a DIFFERENT one builds it explicitly.
var auditTestKey = []byte("hostevent-audit-test-key-not-secret")

// Stamps for the fixtures below, which are all STAMPED: a line a real host wrote
// carries a timestamp, and since #932 a line with no usable stamp lands in an
// unplaced turn that is carried but can never clear the token bar. These are one
// instant written the two ways hosts write it — RFC 3339 for claude and codex,
// Unix milliseconds for opencode.
//
// The NEGATIVES are stamped for exactly the same reason as the positives: a
// fixture that cannot be placed would pass for the wrong reason, so what decides
// a verdict here is which region the scanner read, and never whether the lines
// could be placed at all. The same limit applies to the two tests whose subject
// is not placement — the partial read and the keyless scan — where the assertion
// is about the read or the key and no verdict is reached.
const (
	auditStampRFC3339 = "2026-08-24T09:00:00.000Z"
	// auditStampMS is auditStampRFC3339 in Unix milliseconds.
	auditStampMS = "1787562000000"
)

// auditTestHasher is auditTestKey's hasher.
func auditTestHasher(t *testing.T) audit.Hasher {
	t.Helper()
	h, err := audit.NewHasher(auditTestKey)
	if err != nil {
		t.Fatalf("NewHasher(auditTestKey): %v", err)
	}
	return h
}

// matchesFixture reports whether the signals judge the fixture memory as used.
func usedBySignals(t *testing.T, sig *audit.Signals) bool {
	t.Helper()
	v, ok := audit.CompareAgainst(sig, audit.Judged{MemoryID: auditMemoryID, Content: injectedText})
	if !ok {
		t.Fatal("the fixture memory could not be judged at all")
	}
	return v.Outcome == audit.OutcomeUsed
}

// auditUnnamedID names a memory no fixture here ever writes, so a verdict on it
// cannot come from the identifier arm.
const auditUnnamedID = "0123456789abcdef0123456789abcdef"

// tokenArmUsedBySignals judges the SAME memory under an id the fixture never
// names, which is the only way these fixtures can pin the TOKEN arm: precedence
// answers used/identifier for a named memory even if nothing was placed, so a
// stamp the scanner stopped writing would leave that assertion green.
func tokenArmUsedBySignals(t *testing.T, sig *audit.Signals) bool {
	t.Helper()
	v, ok := audit.CompareAgainst(sig, audit.Judged{MemoryID: auditUnnamedID, Content: injectedText})
	if !ok {
		t.Fatal("the fixture memory could not be judged at all")
	}
	return v.Outcome == audit.OutcomeUsed
}

func scanAudit(t *testing.T, format, transcript string) *audit.Signals {
	t.Helper()
	f := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(f, []byte(transcript), 0o600); err != nil {
		t.Fatalf("write the fixture: %v", err)
	}
	r, err := os.Open(f)
	if err != nil {
		t.Fatalf("open the fixture: %v", err)
	}
	defer r.Close() //nolint:errcheck
	sig, ok, err := ScanAudit(format, r, auditTestHasher(t))
	if err != nil {
		t.Fatalf("ScanAudit: %v", err)
	}
	if !ok {
		t.Fatalf("no audit scanner registered for %q", format)
	}
	return sig
}

func TestScanAuditClaudeJSONL(t *testing.T) {
	// The agent's own text, a Bash call, the memory's wording arriving as the
	// tool result, and the id named in a later message. Stamped, so that what
	// places the prose is the timestamp the scanner read (wcatz/ghost#932).
	transcript := strings.Join([]string{
		`{"type":"user","message":{"content":[{"type":"tool_result","content":"` + injectedText + ` (id ` + auditMemoryID + `)"}]}}`,
		`{"type":"assistant","timestamp":"` + auditStampRFC3339 + `","message":{"content":[{"type":"text","text":"` + agentText + `"}]}}`,
		`{"type":"assistant","timestamp":"` + auditStampRFC3339 + `","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"ls internal/memory"}}]}}`,
		`{"type":"assistant","timestamp":"` + auditStampRFC3339 + `","message":{"content":[{"type":"text","text":"applying what ` + auditMemoryID + ` records"}]}}`,
		"",
	}, "\n")

	sig := scanAudit(t, FormatClaudeJSONL, transcript)
	if !sig.HasID(auditMemoryID) {
		t.Error("HasID = false for an id the agent named in its own message")
	}
	if !usedBySignals(t, sig) {
		t.Error("the agent's restatement of the memory did not count as a use")
	}
	// The same memory under an id the fixture never names: only the token arm
	// can answer this, and only if the two assistant text lines landed in a turn
	// the bar can be asked about.
	if !tokenArmUsedBySignals(t, sig) {
		t.Error("the token arm did not judge the stamped prose this scanner read")
	}
}

// TestScanAuditDoesNotReadWhatGhostInjected is the load-bearing negative: the
// injected block carries the memory's exact wording, and reading it would make
// every retrieval look used no matter what the agent did.
func TestScanAuditDoesNotReadWhatGhostInjected(t *testing.T) {
	transcript := strings.Join([]string{
		`{"type":"user","message":{"content":[{"type":"tool_result","content":"` + injectedText + ` (id ` + auditMemoryID + `)"}]}}`,
		`{"type":"assistant","timestamp":"` + auditStampRFC3339 + `","message":{"content":[{"type":"text","text":"moving on to something else entirely"}]}}`,
		"",
	}, "\n")

	sig := scanAudit(t, FormatClaudeJSONL, transcript)
	if usedBySignals(t, sig) {
		t.Error("the injected block counted as the agent's use of the memory")
	}
	if sig.HasID(auditMemoryID) {
		t.Error("an id inside the injected block counted as the agent naming it")
	}
}

// TestScanAuditProseIsNotASave: a save is what SUPERSEDES a memory in-session, so
// reading a save's own content as usage would leave that bucket permanently
// empty — the two rules are distinguished by where the text came from.
func TestScanAuditProseIsNotASave(t *testing.T) {
	// Stamped, as every line a real host writes is: an unstamped line cannot be
	// placed after a call, so it would carry no turn and the token arm would have
	// nothing to judge (wcatz/ghost#932).
	transcript := strings.Join([]string{
		`{"type":"assistant","timestamp":"` + auditStampRFC3339 + `","message":{"content":[{"type":"text","text":"` + agentText + `"}]}}`,
		"",
	}, "\n")
	sig := scanAudit(t, FormatClaudeJSONL, transcript)
	if v, ok := audit.CompareAgainst(sig, audit.Judged{MemoryID: auditMemoryID, Content: injectedText}); !ok || v.Outcome != audit.OutcomeUsed {
		t.Errorf("outcome = %+v, want used: prose IS usage", v)
	}

	saveTranscript := strings.Join([]string{
		`{"type":"assistant","timestamp":"` + auditStampRFC3339 + `","message":{"content":[{"type":"tool_use","name":"mcp__ghost__ghost_memory_save","input":{"content":"` + injectedText + `"}}]}}`,
		"",
	}, "\n")
	sig = scanAudit(t, FormatClaudeJSONL, saveTranscript)
	v, ok := audit.CompareAgainst(sig, audit.Judged{MemoryID: auditMemoryID, Content: injectedText})
	if !ok {
		t.Fatal("the save could not be judged")
	}
	if v.Outcome != audit.OutcomeSuperseded {
		t.Errorf("outcome = %+v, want %q: a save restating the memory is not a use", v, audit.OutcomeSuperseded)
	}
}

func TestScanAuditOpencodeMessages(t *testing.T) {
	// opencode V1: an assistant message's tool part carries input AND output, and
	// the output is where the memory's wording comes back. Only the input is the
	// agent's own.
	// The id appears in the tool's OUTPUT, where the agent did not write it, and
	// again in a message the agent DID write. Only the second is the agent naming
	// the memory — which is what TestScanAuditOpencodeMessagesIgnoresToolOutput
	// rules out for the first.
	// Stamped, as every line a real host writes is (wcatz/ghost#932).
	transcript := strings.Join([]string{
		`{"info":{"id":"m1","role":"user","time":{"created":` + auditStampMS + `}},"parts":[{"type":"text","text":"` + injectedText + `"}]}`,
		`{"info":{"id":"m2","role":"assistant","time":{"created":` + auditStampMS + `}},"parts":[{"type":"tool","tool":"ghost_ghost_memory_search","state":{"status":"completed","input":{"query":"transcript"},"output":"` + injectedText + ` (id ` + auditMemoryID + `)"}}]}`,
		`{"info":{"id":"m3","role":"assistant","time":{"created":` + auditStampMS + `}},"parts":[{"type":"text","text":"` + agentText + `"}]}`,
		`{"info":{"id":"m4","role":"assistant","time":{"created":` + auditStampMS + `}},"parts":[{"type":"text","text":"applying what ` + auditMemoryID + ` records"}]}`,
		"",
	}, "\n")

	sig := scanAudit(t, FormatOpencodeMessages, transcript)
	if !sig.HasID(auditMemoryID) {
		t.Error("HasID = false for an id the agent named")
	}
	if !usedBySignals(t, sig) {
		t.Error("the agent's restatement did not count as a use")
	}
	// Same memory, unnamed id: the token arm alone answers this, and only if the
	// scanner stamped the prose it read (wcatz/ghost#932).
	if !tokenArmUsedBySignals(t, sig) {
		t.Error("the token arm did not judge the stamped prose this scanner read")
	}
}

func TestScanAuditOpencodeMessagesIgnoresToolOutput(t *testing.T) {
	transcript := strings.Join([]string{
		`{"info":{"id":"m1","role":"assistant","time":{"created":` + auditStampMS + `}},"parts":[{"type":"tool","tool":"ghost_ghost_memory_search","state":{"status":"completed","input":{"query":"transcript"},"output":"` + agentText + `"}}]}`,
		"",
	}, "\n")
	if usedBySignals(t, scanAudit(t, FormatOpencodeMessages, transcript)) {
		t.Error("a tool's OUTPUT counted as the agent's own words")
	}
}

func TestScanAuditOpencodeV2Messages(t *testing.T) {
	// V2 Code Mode: the `execute` entry holds the inner call's metadata, and the
	// inner call's input is the save's content. Reading the outer code alone
	// would call a save a tool argument, which is a use.
	transcript := strings.Join([]string{
		`{"type":"assistant","time":{"created":` + auditStampMS + `},"content":[{"type":"tool","name":"execute","state":{"status":"completed","input":{"code":"return 1"},"metadata":{"toolCalls":[]}}}]}`,
		`{"type":"assistant","time":{"created":` + auditStampMS + `},"content":[{"type":"tool","name":"execute","state":{"status":"completed","input":{"code":"await tools.ghost.ghost_memory_save({})"},"metadata":{"toolCalls":[{"tool":"ghost.ghost_memory_save","status":"completed","input":{"content":"` + injectedText + `"}}]}}}]}`,
		`{"type":"assistant","time":{"created":` + auditStampMS + `},"content":[{"type":"text","text":"` + agentText + `"}]}`,
		"",
	}, "\n")

	sig := scanAudit(t, FormatOpencodeV2Messages, transcript)
	v, ok := audit.CompareAgainst(sig, audit.Judged{MemoryID: auditMemoryID, Content: injectedText})
	if !ok {
		t.Fatal("the fixture could not be judged")
	}
	// Used, because the agent also said it in prose — and that is the point of
	// this fixture: the save inside `execute` must not have decided the bucket on
	// its own, because it is present in the same transcript.
	if v.Outcome != audit.OutcomeUsed {
		t.Errorf("outcome = %+v, want used from the prose", v)
	}
}

func TestScanAuditOpencodeV2SaveInCodeModeIsASave(t *testing.T) {
	transcript := strings.Join([]string{
		`{"type":"assistant","time":{"created":` + auditStampMS + `},"content":[{"type":"tool","name":"execute","state":{"status":"completed","input":{"code":"await tools.ghost.ghost_memory_save({})"},"metadata":{"toolCalls":[{"tool":"ghost.ghost_memory_save","status":"completed","input":{"content":"` + injectedText + `"}}]}}}]}`,
		"",
	}, "\n")
	sig := scanAudit(t, FormatOpencodeV2Messages, transcript)
	v, ok := audit.CompareAgainst(sig, audit.Judged{MemoryID: auditMemoryID, Content: injectedText})
	if !ok {
		t.Fatal("the fixture could not be judged")
	}
	if v.Outcome != audit.OutcomeSuperseded {
		t.Errorf("outcome = %+v, want %q", v, audit.OutcomeSuperseded)
	}
}

func TestScanAuditCodexRollout(t *testing.T) {
	transcript := strings.Join([]string{
		`{"timestamp":"2026-08-24T08:59:00.000Z","type":"session_meta","payload":{"id":"s"}}`,
		`{"timestamp":"` + auditStampRFC3339 + `","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"text","text":"` + agentText + `"}]}}`,
		`{"timestamp":"` + auditStampRFC3339 + `","type":"response_item","payload":{"type":"function_call","name":"ghost_memory_save","namespace":"mcp__ghost","arguments":"{\"content\":\"` + injectedText + `\"}"` + `}}`,
		`{"timestamp":"` + auditStampRFC3339 + `","type":"response_item","payload":{"type":"function_call_output","output":"` + agentText + `"}}`,
		"",
	}, "\n")

	sig := scanAudit(t, FormatCodexRollout, transcript)
	v, ok := audit.CompareAgainst(sig, audit.Judged{MemoryID: auditMemoryID, Content: injectedText})
	if !ok {
		t.Fatal("the fixture could not be judged")
	}
	if v.Outcome != audit.OutcomeUsed {
		t.Errorf("outcome = %+v, want used: the assistant message is the agent's own, and a function_call_output is not", v)
	}
}

func TestScanAuditCodexSaveArgumentsAreASave(t *testing.T) {
	transcript := strings.Join([]string{
		`{"timestamp":"` + auditStampRFC3339 + `","type":"response_item","payload":{"type":"function_call","name":"ghost_memory_save","namespace":"mcp__ghost","arguments":"{\"content\":\"` + injectedText + `\"}"` + `}}`,
		"",
	}, "\n")
	sig := scanAudit(t, FormatCodexRollout, transcript)
	v, ok := audit.CompareAgainst(sig, audit.Judged{MemoryID: auditMemoryID, Content: injectedText})
	if !ok {
		t.Fatal("the fixture could not be judged")
	}
	if v.Outcome != audit.OutcomeSuperseded {
		t.Errorf("outcome = %+v, want %q", v, audit.OutcomeSuperseded)
	}
}

// TestScanAuditCodexIgnoresFunctionCallOutput is the negative half of the codex
// contract, and it needs its own fixture: the rollout test above cannot pin it,
// because the assistant message in that transcript already supplies the evidence
// — a function_call_output could be read as prose and the verdict would not move.
// Here the ONLY agent-authored text carrying the memory's wording is the output,
// so reading it would flip the verdict.
func TestScanAuditCodexIgnoresFunctionCallOutput(t *testing.T) {
	transcript := strings.Join([]string{
		`{"timestamp":"` + auditStampRFC3339 + `","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"text","text":"searching for the sweep"}]}}`,
		`{"timestamp":"` + auditStampRFC3339 + `","type":"response_item","payload":{"type":"function_call_output","output":"` + agentText + `"}}`,
		"",
	}, "\n")
	if usedBySignals(t, scanAudit(t, FormatCodexRollout, transcript)) {
		t.Error("a function_call_output counted as the agent's own words")
	}
}

// TestScanAuditSkipsUnparseableLines keeps the same posture the save-nudge's
// scanner has: a line Ghost cannot read is skipped, never fatal, because a
// whole session's audit must not be lost to one bad line.
func TestScanAuditSkipsUnparseableLines(t *testing.T) {
	transcript := "garbage not json\n" + `{"type":"assistant","timestamp":"` + auditStampRFC3339 + `","message":{"content":[{"type":"text","text":"` + agentText + `"}]}}` + "\n{{{{\n"
	if !usedBySignals(t, scanAudit(t, FormatClaudeJSONL, transcript)) {
		t.Error("a transcript with unparseable lines lost the agent's own text")
	}
}

// TestScanAuditMarksAPartialReadDegraded: a truncated transcript would otherwise
// produce a confident report saying the agent never used anything.
func TestScanAuditMarksAPartialReadDegraded(t *testing.T) {
	full := `{"type":"assistant","message":{"content":[{"type":"text","text":"` + agentText + `"}]}}`
	// One whole line, then a partial one with no terminator, then a read error.
	data := full + "\n" + `{"type":"assistant","mess`
	r := &errReader{data: []byte(data)}
	sig, ok, err := ScanAudit(FormatClaudeJSONL, r, auditTestHasher(t))
	if !ok {
		t.Fatal("no audit scanner registered for claude-jsonl")
	}
	if err == nil {
		t.Fatal("a partial read returned no error")
	}
	if _, degraded := sig.Degraded(); !degraded {
		t.Error("a partial read was not marked degraded; the report would claim the agent used nothing")
	}
}

func TestScanAuditRegistryCoversEveryScanFormat(t *testing.T) {
	// The save-nudge's registry and the audit's are separate, so a format added
	// to one and not the other would silently disable the audit for that host's
	// users — the failure the scanner registry comment is about, one level up.
	for format := range scanners {
		if _, ok := auditScanners[format]; !ok {
			t.Errorf("format %q has a save-nudge scanner but no audit scanner", format)
		}
	}
	for format := range auditScanners {
		if _, ok := scanners[format]; !ok {
			t.Errorf("format %q has an audit scanner but no save-nudge scanner", format)
		}
	}
}

func TestScanAuditUnknownFormat(t *testing.T) {
	if _, ok, err := ScanAudit("", strings.NewReader(""), auditTestHasher(t)); ok || err != nil {
		t.Error(`ScanAudit("") must stay unregistered so callers fail open`)
	}
}

// TestAScanWithNoKeyProducesNoTokens: the hasher is a parameter of every scanner,
// and this is why that is not an optional convenience.
//
// A keyless scan is Empty. That is load-bearing rather than incidental: the hook's
// precondition for writing a sidecar is Empty, and a scan that recorded tokens
// without a key would hand the detached child a file of unkeyed hashes — the
// exact thing the keyed token is for. It is better for the scan to report that it
// found nothing than for it to report something it cannot protect.
func TestAScanWithNoKeyProducesNoTokens(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	// agentText, in a real transcript line, so the fixture exercises the scanner
	// rather than the registry.
	if err := os.WriteFile(path, []byte(
		`{"type":"assistant","message":{"content":[{"type":"text","text":`+
			strconv.Quote(agentText)+`}]}}`+"\n"), 0o600); err != nil {
		t.Fatalf("write the fixture: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open the fixture: %v", err)
	}
	defer f.Close() //nolint:errcheck

	sig, ok, err := ScanAudit(FormatClaudeJSONL, f, audit.Hasher{})
	if err != nil {
		t.Fatalf("ScanAudit with no key: %v", err)
	}
	if !ok {
		t.Fatal("the format is registered, so ok must be true")
	}
	if !sig.Empty() {
		t.Error("a keyless scan reported evidence; it must produce nothing rather than " +
			"tokens nobody can verify")
	}
}

// TestEveryAuditScannerStampsWhenTheAgentWrote: a call is judged only against text written
// after it, so each scanner has to put an instant on what it reads. The fixture has the
// memory's wording said once at 09:00 and unrelated text at 10:00; a cutoff between them
// must leave the memory unused, and a cutoff before both must leave it used. A line with no
// timestamp is carried but never counts for any cutoff, and the scan says so.
func TestEveryAuditScannerStampsWhenTheAgentWrote(t *testing.T) {
	early := time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC)
	late := early.Add(time.Hour)
	iso := func(t time.Time) string { return t.Format(time.RFC3339Nano) }
	ms := func(t time.Time) string { return strconv.FormatInt(t.UnixMilli(), 10) }
	const other = "an unrelated remark about the weather"

	cases := map[string]struct {
		format     string
		transcript func(wording, other, unplaced string) string
	}{
		"claude": {FormatClaudeJSONL, func(w, o, u string) string {
			return strings.Join([]string{
				`{"type":"assistant","timestamp":"` + iso(early) + `","message":{"content":[{"type":"text","text":"` + w + `"}]}}`,
				`{"type":"assistant","timestamp":"` + iso(late) + `","message":{"content":[{"type":"text","text":"` + o + `"}]}}`,
				`{"type":"assistant","message":{"content":[{"type":"text","text":"` + u + `"}]}}`, ""}, "\n")
		}},
		"codex": {FormatCodexRollout, func(w, o, u string) string {
			return strings.Join([]string{
				`{"timestamp":"` + iso(early) + `","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"text","text":"` + w + `"}]}}`,
				`{"timestamp":"` + iso(late) + `","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"text","text":"` + o + `"}]}}`,
				`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"text","text":"` + u + `"}]}}`, ""}, "\n")
		}},
		"opencode": {FormatOpencodeMessages, func(w, o, u string) string {
			return strings.Join([]string{
				`{"info":{"role":"assistant","time":{"created":` + ms(early) + `}},"parts":[{"type":"text","text":"` + w + `"}]}`,
				`{"info":{"role":"assistant","time":{"created":` + ms(early) + `}},"parts":[{"type":"text","time":{"start":` + ms(late) + `},"text":"` + o + `"}]}`,
				`{"info":{"role":"assistant"},"parts":[{"type":"text","text":"` + u + `"}]}`, ""}, "\n")
		}},
		"opencode-v2": {FormatOpencodeV2Messages, func(w, o, u string) string {
			return strings.Join([]string{
				`{"type":"assistant","time":{"created":` + ms(early) + `},"content":[{"type":"text","text":"` + w + `"}]}`,
				`{"type":"assistant","time":{"created":` + ms(early) + `},"content":[{"type":"text","time":{"created":` + ms(late) + `},"text":"` + o + `"}]}`,
				`{"type":"assistant","content":[{"type":"text","text":"` + u + `"}]}`, ""}, "\n")
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			sig := scanAudit(t, tc.format, tc.transcript(agentText, other, "unplaced words about the opencode plugin materializes mkdtemp sidecar"))
			if !usedBySignals(t, sig.Since(early.Add(-time.Minute))) {
				t.Error("a cutoff before the wording did not leave it used: the scanner stamped no instant at all")
			}
			if usedBySignals(t, sig.Since(early.Add(30*time.Minute))) {
				t.Error("the wording was written before the cutoff and still counted as used")
			}
			if sig.Unplaced() == 0 {
				t.Error("the line with no timestamp was not counted as unplaced")
			}
			if reason, ok := sig.Degraded(); !ok || !strings.Contains(reason, "no timestamp") {
				t.Errorf("degraded = %q, %v; an unplaced line must be named on the scan", reason, ok)
			}
		})
		// Two assistant lines at two instants are two turns, and the token arm is
		// judged within one of them (wcatz/ghost#932). Each line below carries two
		// of the fixture memory's eight distinctive words: the union of the two
		// clears the bar and neither line does, so a scanner that stamped one
		// instant for both — or none — would report used here. The control says the
		// same four words in ONE line must still be used, so the split costs
		// nothing the rule does not intend to cost.
		t.Run(name+"/turns", func(t *testing.T) {
			split := scanAudit(t, tc.format, tc.transcript("the opencode plugin", "materializes its transcript", ""))
			if usedBySignals(t, split) {
				t.Error("two lines of two memory words each judged used: the arm read their union, not either turn")
			}
			whole := scanAudit(t, tc.format, tc.transcript("opencode plugin materializes its transcript", other, ""))
			if !usedBySignals(t, whole) {
				t.Error("the same four memory words in one line judged ignored: one turn must clear the bar")
			}
		})
	}
}

// TestScanAuditAnUpdateOfAMemoryDoesNotCiteIt: an update's `memory_id` says which
// memory is being rewritten, so the id is the edit's target and not the agent relying
// on it. Its words are read as a restatement, like a save's. Any other tool call that
// carries the id is still a citation.
func TestScanAuditAnUpdateOfAMemoryDoesNotCiteIt(t *testing.T) {
	line := func(tool string) string {
		return `{"type":"assistant","timestamp":"` + auditStampRFC3339 + `","message":{"content":[{"type":"tool_use","name":"` + tool +
			`","input":{"memory_id":"` + auditMemoryID + `","content":"` + injectedText + `"}}]}}` + "\n"
	}
	for _, name := range []string{"mcp__ghost__ghost_memory_update", "ghost_memory_update", "ghost_ghost_memory_update", "ghost.ghost_memory_update"} {
		sig := scanAudit(t, FormatClaudeJSONL, line(name))
		if sig.HasID(auditMemoryID) {
			t.Errorf("%s: the id of the memory being edited was recorded as a citation", name)
		}
		v, ok := audit.CompareAgainst(sig, audit.Judged{MemoryID: auditMemoryID, Content: injectedText})
		if !ok || v.Outcome != audit.OutcomeSuperseded {
			t.Errorf("%s: verdict %+v, want %q: a rewrite restating the memory is a restatement", name, v, audit.OutcomeSuperseded)
		}
	}
	// The control, so the cases above are the routing's doing: the same arguments
	// under a tool that is not a Ghost write are the agent naming the memory.
	sig := scanAudit(t, FormatClaudeJSONL, line("mcp__ghost__ghost_memory_pin"))
	if !sig.HasID(auditMemoryID) {
		t.Error("an id in another tool's arguments stopped being a citation")
	}
	// And another server's tool of the same bare name is not Ghost's.
	sig = scanAudit(t, FormatClaudeJSONL, line("mcp__other__ghost_memory_update"))
	if !sig.HasID(auditMemoryID) {
		t.Error("another server's ghost_memory_update was treated as Ghost's")
	}
}
