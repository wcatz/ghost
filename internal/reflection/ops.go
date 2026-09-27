package reflection

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/wcatz/ghost/internal/memory"
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
		return opResponse{}, fmt.Errorf("reflection output is not valid JSON: %w (starts: %q)", err, snippet)
	}
	if resp.Ops == nil {
		return opResponse{}, fmt.Errorf("reflection output has no \"ops\" array — consolidation is now expressed as operations on the input ids (keep/merge/rewrite/drop), not as a rewritten memory list")
	}
	if len(*resp.Ops) == 0 {
		// An empty operation list is not a consolidation, it is a deletion, and
		// the only thing between it and the store is the quality gate — which
		// reads no output count at all below six inputs. Failing here names the
		// cause and keeps the deterministic tier reachable in every case.
		return opResponse{}, fmt.Errorf("reflection output carries no operations: the harness returned an empty \"ops\" array")
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
		return fmt.Errorf("ops[%d] %q: %s", lineNo, clipOpLine(raw), fmt.Sprintf(format, args...))
	}

	s := strings.TrimSpace(raw)
	verb, rest, _ := strings.Cut(s, " ")
	rest = strings.TrimSpace(rest)
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
			return memOp{}, fail("unreadable drop reason %q — use \"obsolete\" or \"superseded by <id>\"", reason)
		}
		target, ok = trimIDLabel(target)
		if !ok || strings.ContainsAny(target, " \t") {
			return memOp{}, fail("\"superseded by\" needs the id of the input memory that replaces this one")
		}
		return memOp{kind: opDrop, ids: []string{id}, target: strings.ToUpper(target), line: lineNo}, nil

	default:
		return memOp{}, fail("unknown operation %q — use keep, merge, rewrite or drop", verb)
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
				return ReflectionResult{}, fmt.Errorf("ops[%d] %q: id %q is not one of the memories this run was given", i+1, clipOpLine(line), id)
			}
		}
		if op.target != "" {
			if _, ok := resolve(op.target); !ok {
				return ReflectionResult{}, fmt.Errorf("ops[%d] %q: superseded-by target %q is not one of the memories this run was given", i+1, clipOpLine(line), op.target)
			}
		}
		for _, id := range op.ids {
			key := memIDKey(id)
			if first, dup := claimed[key]; dup {
				return ReflectionResult{}, fmt.Errorf("ops[%d] %q: id %q already appears in ops[%d] — each id takes exactly one operation", i+1, clipOpLine(line), id, first)
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
			return ReflectionResult{}, fmt.Errorf("ops[%d]: %q is dropped as superseded by %q, but %q is not carried forward in this response — the memory would leave the corpus with nothing replacing it",
				op.line, op.ids[0], op.target, op.target)
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
				result.MergedIDs = append(result.MergedIDs, op.ids...)
			}

		case opDrop:
			dropped = append(dropped, op)
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
		// The witness is the text the successor actually carries, which is the
		// merge's text when the successor was folded into one. Recording the
		// successor's stored content instead would let the guard honour a claim
		// about text the result does not hold.
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
	// tier. A memory leaves the corpus when its id is named as a merge source,
	// named for a rewrite, or named in a drop with a reason. --allow-drops keeps
	// its meaning for what the guard still finds — an explicit drop it cannot
	// corroborate, a merge that lost substance, and the SQLite tier's absorbed
	// duplicates.
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
