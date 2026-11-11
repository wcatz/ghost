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

type memOp struct {
	kind     opKind
	ids      []string
	target   string
	obsolete bool
	text     string
	line     int
}

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

func refuseOp(lineNo int, raw, reason string) error {
	return &opRefusal{
		full: fmt.Sprintf("ops[%d] %q: %s", lineNo, clipOpLine(raw), reason),
		safe: fmt.Sprintf("ops[%d] <withheld: operation line, %d chars>: %s", lineNo, len([]rune(raw)), reason),
	}
}

func refuseAt(lineNo int, reason string) error {
	msg := fmt.Sprintf("ops[%d]: %s", lineNo, reason)
	return &opRefusal{full: msg, safe: msg}
}

func refuseEnvelope(reason, snippet string, cause error) error {
	full := reason
	if snippet != "" {
		full = fmt.Sprintf("%s (starts: %q)", reason, snippet)
	}
	return &opRefusal{full: full, safe: reason, cause: cause}
}

func readerComplaintForLog(err error) string {
	var refusal *opRefusal
	if errors.As(err, &refusal) {
		return refusal.Safe()
	}
	return fmt.Sprintf("<withheld: unclassified reader complaint, %T>", err)
}

func safeTierError(err error) string {
	var refusal *opRefusal
	if errors.As(err, &refusal) {
		return refusal.Safe()
	}
	return err.Error()
}

func clipOpText(s string) string {
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
		return opResponse{}, refuseEnvelope("reflection output carries no operations: the harness returned an empty \"ops\" array", "", nil)
	}
	return resp, nil
}

func parseOpLine(lineNo int, raw string) (memOp, error) {
	fail := func(format string, args ...any) error {
		return refuseOp(lineNo, raw, fmt.Sprintf(format, args...))
	}

	s := strings.TrimSpace(raw)
	verb, rest, _ := strings.Cut(s, " ")
	rest = strings.TrimSpace(rest)
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

func trimIDLabel(s string) (string, bool) {
	id := strings.TrimSpace(s)
	if stripped, ok := strings.CutPrefix(strings.ToLower(id), "id:"); ok {
		id = strings.TrimSpace(stripped)
	}
	return id, id != ""
}

func clipOpLine(s string) string {
	const max = 80
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

func refusalIDs(ids []string) string {
	rendered := make([]string, 0, len(ids))
	for _, id := range ids {
		rendered = append(rendered, memory.RefusalArg("memory_id", id))
	}
	return strings.Join(rendered, ",")
}

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
			continue
		}
		op, err := parseOpLine(i+1, line)
		if err != nil {
			return ReflectionResult{}, err
		}
		if op.kind == opKeep {
			if _, ok := resolve(op.ids[0]); !ok {
				logger.Warn("reflection keep names an id this run was not given; skipping it",
					"op", i+1, "id", clipOpText(op.ids[0]))
				continue
			}
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
	emitted := make(map[string]string, len(input.ExistingMemories))
	var dropped []memOp
	for _, op := range ops {
		switch op.kind {
		case opKeep:
			m, _ := resolve(op.ids[0])
			result.Memories = append(result.Memories, verbatimMemory(m))
			emitted[memIDKey(op.ids[0])] = m.Content
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
					"ids", refusalIDs(op.ids), "identifiers", strings.Join(unknown, ","),
					"preview", previewContent(op.text))
				for _, m := range sources {
					result.Memories = append(result.Memories, verbatimMemory(m))
					emitted[memIDKey(m.ID)] = m.Content
				}
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
			if op.kind == opRewrite {
				result.Replacements = append(result.Replacements,
					Replacement{ID: op.ids[0], Text: op.text})
			} else {
				result.Merges = append(result.Merges, Merge{IDs: op.ids, Text: op.text})
			}

		case opDrop:
			dropped = append(dropped, op)
			drop := Drop{ID: op.ids[0], Reason: "obsolete"}
			if !op.obsolete {
				drop.Reason, drop.Successor = "superseded by", op.target
			}
			result.Drops = append(result.Drops, drop)
		}
	}
	for _, op := range dropped {
		if op.obsolete {
			continue
		}
		result.Replacements = append(result.Replacements,
			Replacement{ID: op.ids[0], Text: emitted[op.target]})
	}

	for _, m := range input.ExistingMemories {
		if claimed[memIDKey(m.ID)] == 0 {
			result.Memories = append(result.Memories, verbatimMemory(m))
			emitted[memIDKey(m.ID)] = m.Content
		}
	}
	return result, nil
}

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
		RestatesID: m.ID,
	}
}

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
