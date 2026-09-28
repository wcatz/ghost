package reflection

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/secret"
)

// The per-id operation contract (#639).
//
// Before this, a consolidation returned a complete rewritten memory set as free
// text. Only a byte-identical re-emission kept a row's identity, so any retyping
// — including a rewrite the model considered an improvement — gave the memory a
// fresh id, and with it a fresh embedding, an empty link graph (both are ON
// DELETE CASCADE) and a reset age. The measured cost across a 865-memory corpus
// was 12 of 13 new ids in one project, 15 of 17 in another. The model also had
// no way to say "this row is fine": it could only re-say it, differently.
//
// So the harness is asked for operations ON the ids it was given:
//
//	keep <id>                                      the row is already right
//	merge <id>,<id>[,<id>] -> <text>                several rows say one thing
//	rewrite <id> -> <text>                          correct one row
//	drop <id> reason: obsolete                      wrong, and nothing replaces it
//	drop <id> reason: superseded by <id>            another id already says it
//
// A keep emits the stored row's own content byte for byte, which is what
// ReplaceNonManual's exact-content reuse needs to update the row in place rather
// than delete and re-insert it. The rest of the corpus is untouched by
// construction, and the model has no vocabulary in which to paraphrase a memory
// it did not explicitly rewrite.
//
// Everything here is strict. An operation Ghost cannot read, an id Ghost did not
// feed the model, an id claimed twice, a supersession whose target the same
// response throws away: each fails the whole response rather than being applied
// on a best guess. A partially-understood operation list is a corpus rewritten as
// if the model had said something else, and the tiered consolidator falls through
// to the deterministic tier instead.

// opResponse is the JSON envelope. Ops is a pointer so an absent "ops" key is
// distinguishable from an empty list: the first is a harness answering in the
// retired free-text shape (or a truncated response), the second is a
// consolidation that claims to have kept nothing, and they fail differently.
type opResponse struct {
	LearnedContext string    `json:"learned_context"`
	Ops            *[]string `json:"ops"`
}

type opKind int

const (
	opKeep opKind = iota
	opMerge
	opRewrite
	opDrop
)

// memOp is one parsed operation, with ids still unresolved: they are checked
// against the input in one pass so the error names the id and the line, rather
// than a nil map lookup deep inside the executor.
type memOp struct {
	kind     opKind
	ids      []string
	target   string // drop: the id said to supersede this one
	obsolete bool   // drop: no target named
	text     string // merge/rewrite: the replacement content
	line     int    // 1-based position in the ops array, for diagnostics
}

// opRefusal is a reader complaint, kept in two renderings because it has two
// audiences with different rights to the same text.
//
// Error() is the one a person reads, and the one the repair prompt quotes back to
// the model that wrote the offending line: it carries that line, clipped, because
// the line is how anyone can see WHICH of the operations was wrong and a model
// re-asked has nothing else to go on. Safe() is the one a log may carry: the line
// is withheld, because an operation's replacement text is model-written prose over
// stored memory — a rewrite can quote a memory nearly verbatim — and a log
// outlives the run that produced it, which is the same reason the three log lines
// that report a proposal go through previewContent instead of printing it.
//
// The reason and the ids stay in both, because they are the diagnostic and an id
// is not content — but "the reason" is not automatically safe, since SIX reasons
// quote a model-supplied fragment rather than describing one. All six are listed
// here so the list can be audited against the code, and every one is routed
// through clipOpText (six call sites, eight invocations — the not-carried-
// forward supersession quotes three fragments on one line):
//
//	parseOpLine:  the unreadable drop tail, and an unknown operation verb
//	executeOps:   an id that is not one of the input, a superseded-by target
//	              that is not, an id claimed twice, and a supersession whose
//	              target this response does not carry forward
//
// clipOpText runs the same value-shape gate previewContent applies, so a
// `drop <id> reason: <a credential>` refusal withholds the credential in the log
// rendering too. What reaches a log is therefore the reason MINUS any fragment
// the gate caught, not the reason unconditionally.
type opRefusal struct {
	full  string
	safe  string
	cause error
}

func (e *opRefusal) Error() string { return e.full }

// Safe is the value-free rendering, for a sink that outlives the run.
func (e *opRefusal) Safe() string { return e.safe }

// Unwrap keeps the cause of an envelope refusal unwrappable, as the %w it
// replaced was.
func (e *opRefusal) Unwrap() error { return e.cause }

// refuseOp refuses one operation line. raw is the line as it arrived; the
// rendered quote is clipped, and the log gets its size instead of its text.
func refuseOp(lineNo int, raw, reason string) error {
	return &opRefusal{
		full: fmt.Sprintf("ops[%d] %q: %s", lineNo, clipOpLine(raw), reason),
		safe: fmt.Sprintf("ops[%d] <withheld: operation line, %d chars>: %s", lineNo, len([]rune(raw)), reason),
	}
}

// refuseAt refuses a response at a known operation without quoting its line — a
// contradiction found only once the whole list is known. Nothing model-written
// is in the reason beyond ids, so the two renderings agree.
func refuseAt(lineNo int, reason string) error {
	msg := fmt.Sprintf("ops[%d]: %s", lineNo, reason)
	return &opRefusal{full: msg, safe: msg}
}

// refuseEnvelope refuses the JSON envelope itself. snippet is the head of the raw
// response, which is model-written prose over stored memory (a learned_context
// paragraph, typically), so it is in the full text only.
func refuseEnvelope(reason, snippet string, cause error) error {
	full := reason
	if snippet != "" {
		full = fmt.Sprintf("%s (starts: %q)", reason, snippet)
	}
	return &opRefusal{full: full, safe: reason, cause: cause}
}

// readerComplaintForLog renders a reader refusal for a sink that outlives the
// run, withholding the operation line it quotes. It falls CLOSED: an error this
// package did not build as an opRefusal is withheld whole and named by type,
// because an unrecognised renderer is exactly the case in which a log line must
// not guess what is safe to print.
func readerComplaintForLog(err error) string {
	var refusal *opRefusal
	if errors.As(err, &refusal) {
		return refusal.Safe()
	}
	return fmt.Sprintf("<withheld: unclassified reader complaint, %T>", err)
}

// safeTierError renders an error a TIERED run logged on a failed tier.
//
// A reader refusal is redacted: Safe() withholds the operation line it quotes,
// which for a merge or a rewrite ends in the model's own replacement prose over
// stored memory. Nothing else is. In particular a HARNESS failure is passed
// through verbatim, and it is NOT value-free: internal/ai builds those from the
// child's own output (harnessFailureOutput falls back to stdout, because
// opencode reports on its JSON stream, and that stream carries `text` events
// holding the model's answer), so up to 1200 bytes of model prose reach this line
// and the append-only lifecycle.log with it. That is pre-existing — the old code
// logged `err` too — and it is the deliberate trade #540 made, because those 357
// undiagnosable "opencode run: exit status 1: " entries were the cost of hiding
// the child's explanation. Fixing it means changing what internal/ai puts in the
// error, which is a separate change with its own callers; what this function
// declines to do is pretend the line is value-free.
func safeTierError(err error) string {
	var refusal *opRefusal
	if errors.As(err, &refusal) {
		return refusal.Safe()
	}
	return err.Error()
}

// clipOpText bounds ONE model-supplied fragment interpolated into a complaint —
// a hallucinated id, a free-form drop reason, a verb. Clipping the line is not
// enough on its own: `drop <id> reason: <a paragraph of prose>` puts that whole
// paragraph in the message, and the message is quoted back into a prompt and
// written to a log. A stored id is 32 characters — every `id` default in the
// schema is `hex(randomblob(16))` — so 60 leaves room for an id plus a label or
// a separator and never truncates a legitimate one.
//
// It also runs the value-shape gate in BOTH cases — see the probe below —
// because bounding the fragment is not the same as making it safe to print. 60
// runes holds a whole short-format token
// (gh[pousr]_ + 20, AKIA… + 16, npm_/hf_ + 30) and a large part of a long one —
// an ed25519 cborHex, a PEM body, a mnemonic, a JWT all run past the clip, so
// without the gate a fragment of the key would sit in the log rather than
// nothing. And a `drop <id> reason: <token>` is refused PRECISELY because the
// tail is free-form, so the refused text is the model's own. This is the same
// gate previewContent applies to the three other in-tier log lines, and it
// belongs in the one function every fragment goes through rather than at each
// call site — see previewContent for why. The detection runs BEFORE the clip, so
// a value that straddles the boundary is judged whole.
func clipOpText(s string) string {
	// Probed in all THREE spellings — as written, lower-cased, upper-cased — and
	// the first two folds are what cover the case-SENSITIVE rules. secret.Detect
	// matches a value shape in the spelling it is given, and those rules split by
	// literal case: `gh[pousr]_` is lower, `AKIA|ASIA|ABIA|ACCA`+16 and
	// `AGE-SECRET-KEY-1…` are upper, so no single fold reaches both and each fold
	// is a no-op on a fragment already in that case.
	//
	// All three are reachable from model text, which is why all three are probed.
	// The parser upper-cases a supersession's target so it coincides with the
	// stored spelling (hex(randomblob) renders upper-case), which HIDES a
	// `gh[pousr]_` token from an as-written-only probe; and the free-form drop tail
	// is taken in the spelling the model wrote, which HIDES a lower-case `AKIA…`
	// from a lower-fold-only probe (folding that one down is a no-op).
	//
	// The as-written probe is not redundant, and this is the case only IT covers:
	// the MIXED-case literals — google-api-key `AIza…`, pypi `pypi-AgEIcHlwaS5vcmc…`,
	// JWT `eyJ…`, PuTTY `PuTTY-User-Key-File-` — match neither fold, so a
	// re-spelled one (`aizasyd-…`) is caught by the original probe alone. Note the
	// consequence for the parser, which is why parseOpLine keeps a raw copy of the
	// verb instead of gating the folded one: lower-casing a token before this
	// function sees it converts a mixed-case literal into a shape no probe
	// recognises, which is the leak this gate exists to close. Named rather than
	// pointed at with a direction, because a positional pointer in a comment like
	// this one goes stale on the next edit above it. Pinned by
	// TestReaderComplaintGatesAMixedCaseLiteral, which fails if either the
	// as-written probe or the raw verb is removed.
	//
	// KNOWN RESIDUAL, stated rather than implied closed: closing the re-spelling
	// case itself would mean lower-casing the rules in internal/secret, which
	// changes what they match everywhere they are called — including the write
	// boundary, where a re-spelled credential must be judged the same way. That is
	// a change to internal/secret with its own callers, not to this probe set.
	//
	// A stored id trips none of the three, for two separate reasons the detector's
	// own constants give: 32 hex characters is under the two bare-hex floors
	// (cardanoKeyMinRun 68, longHexFloor 132) though ABOVE assignedSecretFloor (20) —
	// so "it is short" is not the general answer either — and it carries no provider
	// prefix and no `key: value` assignment, which is what every remaining rule
	// needs.
	if finding, ok := secret.Detect(s); ok {
		return fmt.Sprintf("<withheld: %s, bytes=%d>", finding.Label, len(s))
	}
	if lower := strings.ToLower(s); lower != s {
		if finding, ok := secret.Detect(lower); ok {
			return fmt.Sprintf("<withheld: %s, bytes=%d>", finding.Label, len(s))
		}
	}
	if upper := strings.ToUpper(s); upper != s {
		if finding, ok := secret.Detect(upper); ok {
			return fmt.Sprintf("<withheld: %s, bytes=%d>", finding.Label, len(s))
		}
	}
	const max = 60
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

// parseOpResponse reads the JSON envelope, tolerating a markdown code fence
// (harnesses emit one whether or not they are asked to) and nothing else. The
// operation grammar is deliberately not parsed here — see parseOpLine — so the
// two failure kinds stay separable in their messages.
func parseOpResponse(text string) (opResponse, error) {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "```") {
		if idx := strings.Index(text, "\n"); idx != -1 {
			text = text[idx+1:]
		}
		if idx := strings.LastIndex(text, "```"); idx != -1 {
			text = text[:idx]
		}
		text = strings.TrimSpace(text)
	}

	var resp opResponse
	if err := json.Unmarshal([]byte(text), &resp); err != nil {
		// Unparseable output is an error, not a result. Returning the raw text as
		// learned_context with zero memories read as "the model consolidated
		// everything away" and sent the tiered quality gate looking for a
		// truncation that was really a malformed response.
		snippet := text
		if len(snippet) > 120 {
			snippet = memory.TruncateUTF8(snippet, 120) + "..."
		}
		return opResponse{}, refuseEnvelope(fmt.Sprintf("reflection output is not valid JSON: %v", err), snippet, err)
	}
	if resp.Ops == nil {
		return opResponse{}, refuseEnvelope("reflection output has no \"ops\" array — consolidation is now expressed as operations on the input ids (keep/merge/rewrite/drop), not as a rewritten memory list", "", nil)
	}
	if len(*resp.Ops) == 0 {
		// An empty operation list is not a consolidation, it is a deletion, and
		// the only thing between it and the store is the quality gate — which
		// reads no output count at all below six inputs. Failing here names the
		// cause and keeps the deterministic tier reachable in every case.
		return opResponse{}, refuseEnvelope("reflection output carries no operations: the harness returned an empty \"ops\" array", "", nil)
	}
	return resp, nil
}

// parseOpLine reads one operation. Every deviation is an error: the alternative
// is guessing what an operation the model did not state meant, and a wrong guess
// deletes or rewrites a memory that is still true. A blank line is filtered by
// the caller — it carries no operation, so it is neither a contradiction nor a
// partial apply.
func parseOpLine(lineNo int, raw string) (memOp, error) {
	fail := func(format string, args ...any) error {
		return refuseOp(lineNo, raw, fmt.Sprintf(format, args...))
	}

	s := strings.TrimSpace(raw)
	verb, rest, _ := strings.Cut(s, " ")
	rest = strings.TrimSpace(rest)
	// Fold a COPY for the switch; the raw token is what reaches the unknown-verb
	// refusal below, because that is the one place the model's own words are the
	// complaint's subject. Gating the folded form would lower-case the fragment
	// BEFORE the value-shape check, and the mixed-case literals (AIza…, eyJ…,
	// PuTTY-User-Key-File-) match only as written — so an ops line beginning with
	// a credential-shaped token would have been re-spelled into a shape no probe
	// recognises, and then clipped and logged. The fold is a lookup convenience
	// and must not rewrite what the refusal quotes.
	rawVerb := verb
	verb = strings.ToLower(verb)
	if verb == "" {
		return memOp{}, fail("empty operation")
	}

	switch verb {
	case "keep":
		id, ok := trimIDLabel(rest)
		if !ok {
			return memOp{}, fail("keep needs the id of the memory to carry forward")
		}
		if strings.ContainsAny(id, " \t") {
			return memOp{}, fail("keep takes exactly one id and no text — a kept memory is passed through unchanged, not retyped")
		}
		return memOp{kind: opKeep, ids: []string{id}, line: lineNo}, nil

	case "merge", "rewrite":
		arrow := strings.Index(rest, "->")
		if arrow < 0 {
			return memOp{}, fail("%s needs \"-> <text>\" after the id(s)", verb)
		}
		text := strings.TrimSpace(rest[arrow+2:])
		if text == "" {
			return memOp{}, fail("%s has no replacement text", verb)
		}
		idText := strings.TrimSpace(rest[:arrow])
		if idText == "" {
			return memOp{}, fail("%s needs at least one id before \"->\"", verb)
		}
		parts := strings.Split(idText, ",")
		ids := make([]string, 0, len(parts))
		for _, p := range parts {
			id, ok := trimIDLabel(p)
			if !ok {
				return memOp{}, fail("%s has an empty id in its id list", verb)
			}
			ids = append(ids, id)
		}
		if verb == "merge" && len(ids) < 2 {
			return memOp{}, fail("merge needs at least two ids — correcting one memory is \"rewrite\"")
		}
		if verb == "rewrite" && len(ids) != 1 {
			return memOp{}, fail("rewrite takes exactly one id; folding several together is \"merge\"")
		}
		kind := opMerge
		if verb == "rewrite" {
			kind = opRewrite
		}
		return memOp{kind: kind, ids: ids, text: text, line: lineNo}, nil

	case "drop":
		id, reason, _ := strings.Cut(rest, "reason:")
		id, ok := trimIDLabel(id)
		if !ok {
			return memOp{}, fail("drop needs the id of the memory to drop")
		}
		reason = strings.TrimSpace(reason)
		if strings.ContainsAny(id, " \t") {
			return memOp{}, fail("drop takes exactly one id")
		}
		if reason == "" {
			return memOp{}, fail("drop needs a reason: \"obsolete\", or \"superseded by <id>\" naming the input id that replaces it")
		}
		lower := strings.ToLower(reason)
		if lower == "obsolete" {
			return memOp{kind: opDrop, ids: []string{id}, obsolete: true, line: lineNo}, nil
		}
		target, named := strings.CutPrefix(lower, "superseded by ")
		if !named {
			return memOp{}, fail("unreadable drop reason %q — use \"obsolete\" or \"superseded by <id>\"", clipOpText(reason))
		}
		target, ok = trimIDLabel(target)
		if !ok || strings.ContainsAny(target, " \t") {
			return memOp{}, fail("\"superseded by\" needs the id of the input memory that replaces this one")
		}
		return memOp{kind: opDrop, ids: []string{id}, target: strings.ToUpper(target), line: lineNo}, nil

	default:
		return memOp{}, fail("unknown operation %q — use keep, merge, rewrite or drop", clipOpText(rawVerb))
	}
}

// trimIDLabel reads one id from an operation, accepting the "id:" label the
// prompt prints in front of every memory. A model that copies the line it was
// shown writes "keep id:01AAA", and failing a whole consolidation over a label
// Ghost printed itself would cost a pass over a cosmetic slip — so the label is
// stripped here rather than left to fail the parse. Returns false for an empty id.
func trimIDLabel(s string) (string, bool) {
	id := strings.TrimSpace(s)
	if stripped, ok := strings.CutPrefix(strings.ToLower(id), "id:"); ok {
		id = strings.TrimSpace(stripped)
	}
	return id, id != ""
}

// clipOpLine bounds a rejected line for the diagnostic, by rune so a line of
// stored memory cannot split a UTF-8 character into the error text.
func clipOpLine(s string) string {
	const max = 80
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

// executeOps resolves the parsed operations against the input corpus and
// produces the consolidated result.
//
// The two rules that decide what the model is allowed to change:
//
//   - An id in at most one operation. Two operations over one id contradict each
//     other, and choosing one silently discards the other's intent.
//   - A drop's supersession target must be an id this same response carries
//     forward. A supersession pointing at a row the response also drops is not a
//     supersession: the knowledge leaves the corpus with nothing replacing it.
//
// A merge or rewrite whose text carries an identifier found in none of its
// sources is rejected and its sources are emitted unchanged. The output is not
// repaired, because Ghost cannot know which side of a corrupted identifier
// (2.BeXIAhbj.js vs 2.BeXIAhbq.js) is the true one, and the inputs are still
// there.
func executeOps(resp opResponse, input ReflectionInput, logger *slog.Logger) (ReflectionResult, error) {
	if logger == nil {
		logger = slog.Default()
	}
	byID := make(map[string]memory.Memory, len(input.ExistingMemories))
	for _, m := range input.ExistingMemories {
		byID[memIDKey(m.ID)] = m
	}
	resolve := func(id string) (memory.Memory, bool) {
		m, ok := byID[memIDKey(id)]
		return m, ok
	}

	ops := make([]memOp, 0, len(*resp.Ops))
	claimed := make(map[string]int, len(input.ExistingMemories))
	for i, line := range *resp.Ops {
		if strings.TrimSpace(line) == "" {
			// A blank line carries no operation. It is not a partial apply and
			// not a contradiction, so it is skipped rather than failed.
			continue
		}
		op, err := parseOpLine(i+1, line)
		if err != nil {
			return ReflectionResult{}, err
		}
		for _, id := range op.ids {
			if _, ok := resolve(id); !ok {
				return ReflectionResult{}, refuseOp(i+1, line, fmt.Sprintf("id %q is not one of the memories this run was given", clipOpText(id)))
			}
		}
		if op.target != "" {
			if _, ok := resolve(op.target); !ok {
				return ReflectionResult{}, refuseOp(i+1, line, fmt.Sprintf("superseded-by target %q is not one of the memories this run was given", clipOpText(op.target)))
			}
		}
		for _, id := range op.ids {
			key := memIDKey(id)
			if first, dup := claimed[key]; dup {
				return ReflectionResult{}, refuseOp(i+1, line, fmt.Sprintf("id %q already appears in ops[%d] — each id takes exactly one operation", clipOpText(id), first))
			}
			claimed[key] = i + 1
		}
		ops = append(ops, op)
	}

	// The targets a supersession may name, resolved now that every operation is
	// known: the ids a keep, merge or rewrite carries forward.
	carried := make(map[string]bool)
	for _, op := range ops {
		if op.kind != opDrop {
			for _, id := range op.ids {
				carried[memIDKey(id)] = true
			}
		}
	}
	for _, op := range ops {
		if op.kind == opDrop && op.target != "" && !carried[op.target] {
			return ReflectionResult{}, refuseAt(op.line, fmt.Sprintf("%q is dropped as superseded by %q, but %q is not carried forward in this response — the memory would leave the corpus with nothing replacing it",
				clipOpText(op.ids[0]), clipOpText(op.target), clipOpText(op.target)))
		}
	}

	result := ReflectionResult{LearnedContext: resp.LearnedContext}
	// emitted maps an input id to the text that id contributes to the result, so a
	// supersession can name the witness its claim depends on. Drops are resolved
	// after the loop because a target may appear later in the operation list than
	// the drop that names it.
	emitted := make(map[string]string, len(input.ExistingMemories))
	var dropped []memOp
	for _, op := range ops {
		switch op.kind {
		case opKeep:
			m, _ := resolve(op.ids[0])
			result.Memories = append(result.Memories, verbatimMemory(m))
			emitted[memIDKey(op.ids[0])] = m.Content
			// The one thing that separates a keep from the pass-through below, and
			// the report needs the difference: both emit the stored row, so a
			// reader of the result cannot otherwise tell a row the model looked
			// at from one it never mentioned (#684).
			result.Kept = append(result.Kept, op.ids[0])

		case opMerge, opRewrite:
			sources := make([]memory.Memory, 0, len(op.ids))
			texts := make([]string, 0, len(op.ids))
			for _, id := range op.ids {
				m, _ := resolve(id)
				sources = append(sources, m)
				texts = append(texts, m.Content)
			}
			if unknown := unknownIdentifiers(op.text, texts); len(unknown) > 0 {
				kind := "rewrite"
				if op.kind == opMerge {
					kind = "merge"
				}
				logger.Warn("reflection "+kind+" introduced identifiers absent from its sources; keeping the sources instead",
					"ids", strings.Join(op.ids, ","), "identifiers", strings.Join(unknown, ","),
					"preview", previewContent(op.text))
				for _, m := range sources {
					result.Memories = append(result.Memories, verbatimMemory(m))
					emitted[memIDKey(m.ID)] = m.Content
				}
				// Recorded, not only logged. The sources being in the result is
				// what the refusal means, and on its own it reads as a pass-through:
				// nothing in the result said the model had asked to combine these
				// rows, so a report accounting for every input id could not
				// distinguish this from a memory the model ignored.
				result.Refusals = append(result.Refusals, Refusal{
					Kind:        kind,
					IDs:         op.ids,
					Text:        op.text,
					Identifiers: unknown,
				})
				break
			}
			result.Memories = append(result.Memories, mergedMemory(sources, op.text))
			for _, id := range op.ids {
				emitted[memIDKey(id)] = op.text
			}
			// A rejected operation is no operation: its sources are re-emitted
			// verbatim, so it folds nothing and replaces nothing, and neither list
			// may claim otherwise. The grounding check, not the drop guard, is what
			// keeps a rejected rewrite from replacing a row.
			if op.kind == opRewrite {
				result.Replacements = append(result.Replacements,
					Replacement{ID: op.ids[0], Text: op.text})
			} else {
				result.Merges = append(result.Merges, Merge{IDs: op.ids, Text: op.text})
			}

		case opDrop:
			dropped = append(dropped, op)
			// Every drop is recorded, the obsolete ones included. An obsolete drop
			// names no successor, so nothing else in the result mentions the id it
			// disposed of — and an input id nothing accounts for is the hole this
			// record closes: the report could not show a disposal the model had
			// asked for. The reason travels with it for the same reason, since
			// "obsolete" and "superseded by" are different claims, and the named
			// successor is its own field so a reader can resolve it to the stored
			// id rather than parsing it out of a sentence.
			drop := Drop{ID: op.ids[0], Reason: "obsolete"}
			if !op.obsolete {
				drop.Reason, drop.Successor = "superseded by", op.target
			}
			result.Drops = append(result.Drops, drop)
		}
	}
	for _, op := range dropped {
		if op.obsolete {
			// Nothing to emit and nothing to exempt. An obsolete claim names no
			// successor, so the drop guard still audits the row and decides
			// whether the corpus can show it is gone — the id was named, so it is
			// not passed through below, but the guard may still put it back.
			continue
		}
		// The text the successor actually carries, which is the merge's text when
		// the successor was folded into one. Recording the successor's stored
		// content instead would misreport what the response said it was
		// replacing. This is a record, not a permission: the drop guard audits a
		// disposed row like any other, because a kept stale row is repairable by
		// resolve and supersede and a deleted row is not (#549).
		result.Replacements = append(result.Replacements,
			Replacement{ID: op.ids[0], Text: emitted[op.target]})
	}

	// Everything the response never named is carried through as a keep, verbatim
	// and in input order. This is what makes the prompt's promise true, and it is
	// structural rather than heuristic: before it, an unnamed memory was emitted
	// by nobody and survived only if the drop guard FAILED to recognise a
	// survivor, so an unrelated output sharing 45% of its tokens deleted it with
	// no warning and no --allow-drops. A token guard can only re-add what it
	// flags, and a false positive in the "absorbed" direction is silent data
	// loss; an id the model never mentioned needs no inference at all.
	//
	// Consequence, deliberate: omission is no longer a deletion path for the LLM
	// tier. Naming an id is NECESSARY but not SUFFICIENT: a memory leaves the
	// corpus when its id is named as a merge source, named for a rewrite, or
	// named in a drop with a reason AND a surviving output accounts for it — the
	// drop guard's 45% token containment, with a merge source measured against
	// its own merge — or --allow-drops accepts the deletion. So this comment's
	// record of what was named is not a record of what was removed (#549), and a
	// rewrite whose replacement says nothing of the old row leaves that row in
	// the corpus verbatim.
	//
	// --allow-drops keeps its meaning for what the guard still finds — an
	// explicit drop it cannot corroborate, a merge that lost substance, and the
	// SQLite tier's absorbed duplicates.
	for _, m := range input.ExistingMemories {
		if claimed[memIDKey(m.ID)] == 0 {
			result.Memories = append(result.Memories, verbatimMemory(m))
			emitted[memIDKey(m.ID)] = m.Content
		}
	}
	return result, nil
}

// verbatimMemory is a memory passed through from the input without the model
// touching it. Every field Ghost can carry is the stored one, so the emitted
// row is byte-identical to the row it stands for: ReplaceNonManual's
// exact-content reuse updates it in place and its id, embedding, links and
// age survive. Scope is left unset on purpose — the emitted string scope is only
// a project/global routing hint, and a keep must not restate it.
//
// The category is copied for the same reason it must not be inferred: a
// retyped category sends the row down the rewrite branch of the reuse, which
// restamps created_at and relabels its source (#623).
func verbatimMemory(m memory.Memory) ReflectMemory {
	tags := m.Tags
	if tags == nil {
		tags = []string{}
	}
	return ReflectMemory{
		Category:   m.Category,
		Content:    m.Content,
		Importance: m.Importance,
		Tags:       tags,
	}
}

// mergedMemory derives a merge or rewrite survivor from the rows it folds in.
// Category is the first id's — a merge must not silently recategorize the
// memory that absorbs the others — importance is the strongest of the sources
// rather than the model's estimate, and tags are the union, sorted so the same
// operation list always produces the same row. Scope is inferred from the text
// by the same rule the SQLite tier uses: the model no longer chooses it, and an
// unattended model choosing "global" moves knowledge into every project's
// injected context.
func mergedMemory(sources []memory.Memory, text string) ReflectMemory {
	first := sources[0]
	importance := first.Importance
	tagSet := make(map[string]bool)
	for _, m := range sources {
		if m.Importance > importance {
			importance = m.Importance
		}
		for _, t := range m.Tags {
			tagSet[t] = true
		}
	}
	tags := make([]string, 0, len(tagSet))
	for t := range tagSet {
		tags = append(tags, t)
	}
	sort.Strings(tags)

	return ReflectMemory{
		Category:   first.Category,
		Content:    text,
		Importance: importance,
		Tags:       tags,
		Scope:      inferGlobalScope(first.Category, text),
	}
}
