package audit

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// SidecarHeader marks a file this package wrote. Anything else at that
	// offset is either a stale version or somebody else's file, so ReadSidecar
	// refuses it rather than interpreting whatever bytes it happens to find.
	//
	// v2 because the `neg` line grew a third field kind (#854): a cued sentence
	// now carries which of its words a cue is BOUND to, not only which words it
	// contains. A v1 line parses — its shapes are unchanged — and reads back with
	// nothing bound to anything, so every contradiction in that one turn's file
	// would silently go unfound; refusing the version says so out loud instead,
	// and costs that turn's audit, which is what a sidecar this build cannot read
	// is already worth.
	//
	// v3 because the sidecar now names the SESSION it was scanned from (#648): a run
	// judges only the calls that session made, so a file that does not say which
	// session it is would be judged against every call in the project, which is the
	// defect v3 closes. A v2 file parses and would carry no session, so the audit
	// would silently judge nothing from it; refusing the version says so instead.
	//
	// v4 because every signal now carries WHEN it was written (#648): a call is judged
	// only against the text written after it, so a fingerprint with no instant cannot be
	// placed on either side of a call. A v3 file parses and would carry no instants, so
	// every one of its signals would be unplaceable and the audit would judge nothing
	// from it; refusing the version says so instead, and costs that turn's audit.
	SidecarHeader = "# ghost-audit-signals v4"
	sidecarV3     = "# ghost-audit-signals v3"
	sidecarV2     = "# ghost-audit-signals v2"
	sidecarV1     = "# ghost-audit-signals v1"

	// sidecarHeaderPrefix is the part of the header every version shares, which is what
	// the sweep tests: a file of ANY version this package wrote is this package's to
	// delete once it is stale, and only the current one is ever readable.
	sidecarHeaderPrefix = "# ghost-audit-signals v"

	// sidecarStaleAfter is how long an unclaimed sidecar survives before
	// SweepSidecars reaps it. The detached lifecycle child deletes its own; this
	// is for the one that crashed, was killed, or ran against a store it could
	// not open, and without it every such turn would leave a file behind.
	sidecarStaleAfter = 24 * time.Hour
)

// Signals is what one transcript revealed about what the agent DID, with every
// word already reduced to a fingerprint.
//
// It exists as a type because of WHERE it travels. The stop hook reads the
// transcript synchronously and must not touch the database; the comparison has to
// happen in the detached lifecycle child, by which time an adapter-materialized
// transcript has been swept. So the hook writes these signals to a sidecar and
// passes its path, and what crosses the gap is ids and keyed hashes — which is
// also why the file is safe to leave on disk for a while, and why it is keyed
// rather than merely hashed (see hasher.go).
//
// The zero Signals has no key and is therefore Empty: it produced nothing,
// rather than something anybody holding a word list could reverse. That is what
// makes New's error safe to degrade on, and it is why there is no way to build
// one that fingerprints without a key.
type Signals struct {
	// h keys every token this Signals records and every token it later computes
	// for a memory. It is the same field on both sides of the comparison, which
	// is what makes the two agree: a hook and the detached child that read the
	// same sidecar with different keys would find nothing in common and file
	// every memory as ignored.
	h     Hasher
	ids   []string
	prose []string
	saves []string
	// idsAt, proseAt and savesAt run parallel to the lists above: the LATEST instant
	// (Unix milliseconds) the same entry was written. Latest rather than every one,
	// because a call asks "does this text appear at or after me", and an entry appears
	// at or after a cutoff exactly when its latest occurrence does. 0 means the scanner
	// could not place the text, and such an entry is never evidence for any call.
	idsAt   []int64
	proseAt []int64
	savesAt []int64
	// at is the instant (Unix milliseconds) the scanner is currently reading, set with
	// SetAt before each line and stamped on everything added until it changes. 0 until
	// set, which places nothing.
	at int64
	// negated holds one entry per agent-authored sentence that carried a denial
	// cue, so a contradiction can be attributed to the sentence it happened in
	// rather than to the whole transcript.
	negated  []negSegment
	degraded string
	// session is the host's id for the session this text was written in, and "" when
	// the scan could not name one. It travels with the signals because a verdict is a
	// claim about ONE session's text: Run judges only the calls recorded under this id,
	// and an empty one judges none (see Run).
	session string
}

// SetSessionID names the session these signals were scanned from. Set once by the
// scanner's caller, from the hook payload's own session id; never constructed.
func (s *Signals) SetSessionID(id string) { s.session = id }

// SessionID is the session these signals were scanned from, or "".
func (s *Signals) SessionID() string { return s.session }

// SetAt names the instant of the text the scanner is about to add: every Add* call
// until the next SetAt stamps it. The zero time (a line with no usable timestamp)
// clears it, and what is added then is unplaced: it is carried, so the scan is not
// silently smaller, and it is never evidence for any call, which is the direction
// that cannot produce a false `used`.
func (s *Signals) SetAt(t time.Time) {
	if t.IsZero() || t.UnixMilli() <= 0 {
		s.at = 0
		return
	}
	s.at = t.UnixMilli()
}

// Unplaced counts the entries no instant could be put on. A scanner reports them as a
// degradation, so a verdict filed from a partly unplaced scan says so.
func (s *Signals) Unplaced() int {
	n := 0
	for _, at := range s.idsAt {
		if at <= 0 {
			n++
		}
	}
	for _, at := range s.proseAt {
		if at <= 0 {
			n++
		}
	}
	for _, at := range s.savesAt {
		if at <= 0 {
			n++
		}
	}
	for _, seg := range s.negated {
		if seg.at <= 0 {
			n++
		}
	}
	return n
}

// Ordered reports whether anything in these signals has an instant. A scan with none
// cannot be compared against any call, and judging it would file every memory as
// `ignored` on the strength of text that was never placed.
func (s *Signals) Ordered() bool {
	for _, at := range s.idsAt {
		if at > 0 {
			return true
		}
	}
	for _, at := range s.proseAt {
		if at > 0 {
			return true
		}
	}
	for _, at := range s.savesAt {
		if at > 0 {
			return true
		}
	}
	for _, seg := range s.negated {
		if seg.at > 0 {
			return true
		}
	}
	return false
}

// Since returns the signals written at or after cutoff, which is what a call recorded
// at cutoff can have been used in: text written before a retrieval cannot be a use of
// what the retrieval returned. Entries with no instant are left out, so anything the
// scanner could not place can never produce a `used` (it may leave a memory `ignored` on
// a scan the caller has marked degraded). A zero cutoff
// returns an empty view for the same reason: a call with no instant has no "after".
func (s *Signals) Since(cutoff time.Time) *Signals {
	out := &Signals{h: s.h, degraded: s.degraded, session: s.session}
	if cutoff.IsZero() {
		return out
	}
	from := cutoff.UnixMilli()
	keep := func(vals []string, ats []int64, dst *[]string, dstAt *[]int64) {
		for i, v := range vals {
			if i < len(ats) && ats[i] > 0 && ats[i] >= from {
				*dst = append(*dst, v)
				*dstAt = append(*dstAt, ats[i])
			}
		}
	}
	keep(s.ids, s.idsAt, &out.ids, &out.idsAt)
	keep(s.prose, s.proseAt, &out.prose, &out.proseAt)
	keep(s.saves, s.savesAt, &out.saves, &out.savesAt)
	for _, seg := range s.negated {
		if seg.at > 0 && seg.at >= from {
			out.negated = append(out.negated, seg)
		}
	}
	return out
}

// addAt appends the values dst does not hold, stamping them with the scanner's current
// instant, and moves the instant of one it already holds forward when this occurrence
// is later. The lists and their instants stay the same length by construction.
func (s *Signals) addAt(dst *[]string, dstAt *[]int64, vals []string) {
	index := make(map[string]int, len(*dst)+len(vals))
	for i, f := range *dst {
		index[f] = i
	}
	for _, f := range vals {
		if i, ok := index[f]; ok {
			if s.at > (*dstAt)[i] {
				(*dstAt)[i] = s.at
			}
			continue
		}
		index[f] = len(*dst)
		*dst = append(*dst, f)
		*dstAt = append(*dstAt, s.at)
	}
}

// New returns an empty Signals keyed by the per-install key.
//
// It is the only constructor a caller should use, and it is fallible for the
// reason the whole hasher exists: a key that is absent or too short is refused
// rather than defaulted to something unkeyed. Callers degrade by writing no
// sidecar at all, which costs one turn's audit — the honest cost — rather than
// writing a file whose tokens are reproducible from an English word list.
//
// It takes the key BYTES rather than opening anything, because the stop hook
// runs synchronously and must not open a database; memory.ReadRetrievalKey is
// the resolver, and it never creates a key.
func New(key []byte) (*Signals, error) {
	h, err := NewHasher(key)
	if err != nil {
		return nil, err
	}
	return &Signals{h: h}, nil
}

// NewWithHasher returns an empty Signals over an already-built hasher, for the
// caller that resolved the key once and will hand the same hasher to several
// scans.
func NewWithHasher(h Hasher) *Signals { return &Signals{h: h} }

// Hasher returns the hasher these signals are keyed by, so a reader can tell
// whether a Signals it was given can fingerprint a memory at all.
func (s *Signals) Hasher() Hasher { return s.h }

// negSegment is ONE cued sentence: the fingerprints of its words, and the ones a
// cue is bound to.
//
// All of it in one entry, because the two arms of the negation rule ask the same
// question of the same sentence — "did the agent deny this memory here" — by
// wording for a memory whose text it repeated and by name for one it referred to.
// Splitting them into two lists would let a cue in one sentence satisfy the id
// arm for a memory named in another, which is the same leak the segment split
// exists to close.
//
// What an id or a fingerprint is recorded FOR is the binding and never mere
// presence, which is the whole of #854: an id a sentence names beside a cue it is
// not about ("Per <id>, I'll ignore the formatting") is not evidence that the
// memory was denied, and recording it as though it were is how an agent's
// agreement became the audit's loudest finding. So cueFps and cueIDs are what the
// cue is bound to (boundPositions, and boundToCue with cueGap over cueFillers,
// in tokens.go) and fps is the
// sentence's own vocabulary, which the token bar is measured against.
type negSegment struct {
	// at is when the sentence was written, Unix milliseconds, 0 when unplaced.
	at     int64
	fps    []string
	cueFps []string
	cueIDs []string
}

// denies reports whether a cue in this sentence denies the memory named id, which
// is the id arm's whole requirement: the id beside the cue, upper-cased on both
// sides for the reason AddID gives.
//
// The empty id answers false rather than ranging over the list, because a memory
// with no id has nothing to be denied by name — and because contradicts is called
// with "" by the fingerprint-arm tests, where an empty comparison would otherwise
// match an empty entry.
func (n negSegment) denies(id string) bool {
	if id == "" {
		return false
	}
	upper := strings.ToUpper(id)
	for _, ex := range n.cueIDs {
		if ex == upper {
			return true
		}
	}
	return false
}

// deniesByWording reports whether a cue in this sentence denies the memory by
// repeating its wording: the token arm's own bar, AND at least one of the words
// that clear it to be one the cue is bound to.
//
// Both conditions, and the bar is not replaced by the binding — it is a second
// requirement on top of the SAME clearsTokenBar the `used` arm uses, so this arm
// got stricter and never looser. Without the second condition a sentence could
// deny something else and quote the memory verbatim ("ignore the formatter,
// <the memory's wording>"), which is a claim about this memory the agent never
// made; without the first, a cue would be enough on its own, which is the bar a
// second, weaker threshold for negation was and must not become again.
func (n negSegment) deniesByWording(toks []string) bool {
	if !clearsTokenBar(sharedTokens(n.fps, toks), len(toks)) {
		return false
	}
	return sharedTokens(n.cueFps, toks) > 0
}

// AddID records a memory id the agent named, upper-cased and de-duplicated: the
// comparison upper-cases both sides, because a hex id has exactly one
// case-insensitive spelling and a host that lower-cases one named the same memory.
func (s *Signals) AddID(id string) {
	s.addAt(&s.ids, &s.idsAt, []string{strings.ToUpper(id)})
}

// addWords records text as the agent's own words without drawing any negation
// arm from it. Ids are lifted out separately and reported on their own arm; a
// token is computed over the words as written, id-shaped run included, because
// the token arm's floor of three is what stops a lone id from standing in for a
// memory's own wording — TestAddProseKeepsIDsOutOfTheTokenSet pins that.
func (s *Signals) addWords(text string) {
	s.addAt(&s.prose, &s.proseAt, s.h.DistinctTokens(text))
	for _, id := range memoryIDs(text) {
		s.AddID(id)
	}
}

// AddProse records the agent's own words: its prose, and the arguments of the
// tool calls it made. Ids are lifted out separately, so the token arm can never be
// satisfied by an id (an id is evidence on its own arm, and hashing it as a word
// would let a memory "match itself" without the agent saying anything).
//
// Negation segments are extracted ONLY from prose (the agent's own narrative),
// never from tool-call arguments, because tool argument strings routinely contain
// negative-sounding values ("return false") that are not denials of the memory.
func (s *Signals) AddProse(text string) {
	s.addWords(text)
	for _, seg := range segments(text) {
		words, clauses := splitClauses(seg)
		cues := cueSpans(words, clauses)
		if len(cues) == 0 {
			continue
		}
		// The cue is what makes this a negation SEGMENT; what the cue is bound to
		// is what the arms get to compare a memory against, so both are taken here
		// rather than in the comparison — the hook knows the sentence, and the
		// detached child never sees it again.
		//
		// Two bindings, and they are not the same rule: an id is bound by counting
		// the words between it and the cue — up to one arbitrary word, or up to
		// three drawn from the closed set (cueFillers), so "ignore the advice in
		// <id>" binds while "Per <id>, I now ignore the formatting" does not —
		// and by requiring that no CLAUSE BOUNDARY stands between them, so "The
		// build is stale, the memory <id> applies" does not bind either, because an
		// id is a name the words around it are free to introduce and walk away from.
		// The fingerprints are bound by skipping to the nearest word that could BE a
		// token, across punctuation and all, because the denial-then-restatement
		// shape puts a colon between the cue and the memory's own words and skipping
		// it is what makes that a denial rather than a miss.
		var boundWords []string
		for _, i := range boundPositions(words, cues) {
			boundWords = append(boundWords, words[i])
		}
		var boundIDs []string
		for i, w := range words {
			if boundToCue(i, words, clauses, cues) {
				if id, ok := memoryIDWord(w); ok {
					boundIDs = append(boundIDs, id)
				}
			}
		}
		n := negSegment{
			at:     s.at,
			fps:    s.h.distinctTokens(words),
			cueFps: s.h.distinctTokens(boundWords),
		}
		addUnseen(&n.cueIDs, boundIDs)
		s.negated = append(s.negated, n)
	}
}

// AddSaveArgs records the words of a Ghost SAVE's arguments.
//
// Kept out of prose on purpose. A save that restates a memory is the agent
// declaring the same knowledge again, which is the superseded-in-session bucket and
// not a use — and if save text counted as usage, that bucket could never be
// non-empty.
func (s *Signals) AddSaveArgs(text string) {
	s.addAt(&s.saves, &s.savesAt, s.h.DistinctTokens(text))
	for _, id := range memoryIDs(text) {
		s.AddID(id)
	}
}

// AddToolArgs records a non-save tool call's arguments.
//
// Tool-call arguments MUST NOT feed the negation arm, because values like
// "return false" or "cache lockfile directory" routinely contain negative-
// sounding words that are not denials of a memory. Only the agent's prose
// sentences may contribute negation segments.
func (s *Signals) AddToolArgs(text string) {
	s.addWords(text)
}

// MarkDegraded records why the scan stopped early. Its argument is a reason from
// the fail-open vocabulary the hook already prints, never a transcript phrase.
func (s *Signals) MarkDegraded(reason string) {
	if reason == "" {
		return
	}
	s.degraded = reason
}

// Degraded reports whether the scan was partial and, if so, why.
func (s *Signals) Degraded() (string, bool) {
	if s.degraded != "" {
		return s.degraded, true
	}
	return "", false
}

// Empty reports whether the scan found nothing the comparison could use.
//
// It is the hook's precondition for writing a sidecar at all, and it exists
// because the alternative is the one reading that makes the report confidently
// wrong: an empty set of signals judges every kept memory as "ignored", so a
// transcript this build has no scanner for, or one whose assistant authored
// nothing, would produce a durable claim that the agent used nothing — a claim
// about a transcript that was never read rather than about a session in which
// nothing was used.
//
// A degradation reason does not make an empty scan worth comparing. It is a
// caveat ON a comparison, and there is nothing here to compare.
//
// A Signals with no KEY is Empty even when it collected ids, which is the whole
// point of the check and not an oversight in it. Ids alone cannot carry a
// comparison: without a token hasher the memory side has no words to meet them
// on, so the only verdict such a set could produce is "ignored" for every kept
// memory — a claim of total silence from a caller that never read a word. So a
// keyless scan reports itself as having found nothing, and the hook writes no
// sidecar.
func (s *Signals) Empty() bool {
	if !s.h.HasKey() {
		return true
	}
	return len(s.ids) == 0 && len(s.prose) == 0 && len(s.saves) == 0
}

// HasID reports whether the agent named this memory.
func (s *Signals) HasID(id string) bool {
	upper := strings.ToUpper(id)
	for _, ex := range s.ids {
		if ex == upper {
			return true
		}
	}
	return false
}

// addUnseen appends the values dst does not already hold, in order.
//
// The invariant every list of tokens and ids in this type keeps, and it is a
// duplicate per OCCURRENCE that would break it: a sentence that names an id twice
// while a cue sits between them would otherwise carry it twice into the sidecar
// and into a count.
func addUnseen(dst *[]string, vals []string) {
	seen := make(map[string]bool, len(*dst)+len(vals))
	for _, f := range *dst {
		seen[f] = true
	}
	for _, f := range vals {
		if !seen[f] {
			seen[f] = true
			*dst = append(*dst, f)
		}
	}
}

// WriteSidecar writes s to a fresh file in dir and returns its path.
//
// A CreateTemp name rather than a composed one: the path is handed to a detached
// child on a command line, and a name built from a project id would be a name
// another project could collide with or a caller could predict.
//
// It REFUSES an unkeyed Signals, and the refusal is the load-bearing part of this
// function rather than a guard against a caller mistake. An unkeyed write is not a
// degraded sidecar, it is a sidecar that undoes the design: a file of unkeyed
// 64-bit hashes is a file anybody with a word list can read as plain text, and it
// would be written silently by any caller that forgot a key. So the one way to
// get an unkeyed file onto disk is to not be able to build one.
func WriteSidecar(dir string, s *Signals) (string, error) {
	if !s.h.HasKey() {
		return "", errNoKey
	}
	f, err := os.CreateTemp(dir, "ghost-audit-*.signals")
	if err != nil {
		return "", fmt.Errorf("audit: create sidecar: %w", err)
	}
	path := f.Name()
	var buf bytes.Buffer
	buf.WriteString(SidecarHeader)
	buf.WriteByte('\n')
	for i, id := range s.ids {
		buf.WriteString("id " + id + " " + atField(s.idsAt, i) + "\n")
	}
	for i, fp := range s.prose {
		buf.WriteString("prose " + fp + " " + atField(s.proseAt, i) + "\n")
	}
	for i, fp := range s.saves {
		buf.WriteString("save " + fp + " " + atField(s.savesAt, i) + "\n")
	}
	for _, seg := range s.negated {
		line := "neg " + strconv.FormatInt(seg.at, 10)
		if f := seg.field(); f != "" {
			line += " " + f
		}
		buf.WriteString(line + "\n")
	}
	if s.degraded != "" {
		buf.WriteString("degraded " + strconv.QuoteToASCII(s.degraded) + "\n")
	}
	if s.session != "" {
		buf.WriteString("session " + strconv.QuoteToASCII(s.session) + "\n")
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("audit: write sidecar: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("audit: close sidecar: %w", err)
	}
	return path, nil
}

// ReadSidecar reads a Signals back under the given hasher, refusing any file
// that is not one this version wrote.
//
// The hasher is a parameter and not something the file carries, because a key
// written into the sidecar would be a key beside the hashes it protects. The
// caller therefore has to have resolved the SAME per-install key the writer used,
// and a caller that cannot has nothing to compare with — which is the correct
// outcome, and a skipped audit rather than a wrong one.
//
// Strict on purpose: the path arrives on a command line from a hook, so a
// truncated, edited or foreign file must fail rather than be read as "the agent
// used nothing" — which is the one reading that would make the report confidently
// wrong. A field it cannot parse is an error, not a default.
func ReadSidecar(path string, h Hasher) (*Signals, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("audit: read sidecar: %w", err)
	}
	rest, ok := bytes.CutPrefix(raw, []byte(SidecarHeader+"\n"))
	if !ok {
		// The versions, not the bytes: the path arrived on a command line and the
		// file beside it is not necessarily one this package wrote, so the error
		// names the formats rather than echoing whatever the file's first line
		// says.
		return nil, fmt.Errorf("audit: sidecar header is not %s (%s, %s and %s are previous formats this build cannot read)",
			SidecarHeader, sidecarV3, sidecarV2, sidecarV1)
	}
	s := &Signals{h: h}
	for i, line := range bytes.Split(rest, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		kind, value, ok := bytes.Cut(line, []byte(" "))
		if !ok {
			return nil, fmt.Errorf("audit: sidecar line %d is not a field", i+1)
		}
		switch string(kind) {
		case "id":
			id, at, err := cutAt(string(value))
			if err != nil {
				return nil, fmt.Errorf("audit: sidecar line %d: %w", i+1, err)
			}
			s.ids = append(s.ids, strings.ToUpper(id))
			s.idsAt = append(s.idsAt, at)
		case "prose", "save":
			text, at, err := cutAt(string(value))
			if err != nil {
				return nil, fmt.Errorf("audit: sidecar line %d: %w", i+1, err)
			}
			fp, ok := unhex64(text)
			if !ok {
				return nil, fmt.Errorf("audit: sidecar line %d: %q is not a token fingerprint", i+1, text)
			}
			if string(kind) == "prose" {
				s.prose = append(s.prose, fp)
				s.proseAt = append(s.proseAt, at)
			} else {
				s.saves = append(s.saves, fp)
				s.savesAt = append(s.savesAt, at)
			}
		case "neg":
			head, rest, _ := strings.Cut(string(value), " ")
			at, err := strconv.ParseInt(head, 10, 64)
			if err != nil || at < 0 {
				return nil, fmt.Errorf("audit: sidecar line %d: %q is not an instant", i+1, head)
			}
			seg, err := parseNegSegment(rest)
			if err != nil {
				return nil, fmt.Errorf("audit: sidecar line %d: %w", i+1, err)
			}
			seg.at = at
			s.negated = append(s.negated, seg)
		case "degraded":
			unquoted, err := strconv.Unquote(string(value))
			if err != nil {
				return nil, fmt.Errorf("audit: sidecar line %d: degraded reason is not quoted: %w", i+1, err)
			}
			s.degraded = unquoted
		case "session":
			unquoted, err := strconv.Unquote(string(value))
			if err != nil {
				return nil, fmt.Errorf("audit: sidecar line %d: session id is not quoted: %w", i+1, err)
			}
			s.session = unquoted
		default:
			return nil, fmt.Errorf("audit: sidecar line %d has unknown field %q", i+1, kind)
		}
	}
	return s, nil
}

// atField renders entry i's instant for the sidecar, 0 when the list has none.
func atField(ats []int64, i int) string {
	if i < len(ats) {
		return strconv.FormatInt(ats[i], 10)
	}
	return "0"
}

// cutAt splits "<value> <unix ms>" and refuses a line that has no instant, because a
// field that cannot be placed is not one to guess at.
func cutAt(v string) (string, int64, error) {
	text, raw, ok := strings.Cut(v, " ")
	if !ok {
		return "", 0, fmt.Errorf("%q carries no instant", text)
	}
	at, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || at < 0 {
		return "", 0, fmt.Errorf("%q is not an instant", raw)
	}
	return text, at, nil
}

// cueMark prefixes a fingerprint a cue is bound to on the `neg` line.
//
// The one field kind shape cannot tell apart, and the reason there is a mark
// rather than a third vocabulary: a fingerprint and a CUE-BOUND fingerprint are
// the same sixteen characters, and the difference between them is the difference
// between "in this sentence" and "beside a cue in it" — which is what stops an
// agent quoting a memory while denying something else from being filed as having
// denied the memory (#854). Not a hex character, so an unmarked field can never be
// one by accident, and a marked field that is not a fingerprint is refused.
const cueMark = "~"

// field renders one cued sentence for the sidecar: its fingerprints, the ones a
// cue is bound to, and the ids a cue is bound to, in one space-separated run.
//
// Order is the segment's own, and each half is de-duplicated the way prose and
// saves are, because the reader re-adds them through addUnseen and the
// no-duplicates invariant has to hold on BOTH paths — a file written by one build
// and read by another must not be distinguishable from one written and read by
// the same.
func (n negSegment) field() string {
	parts := make([]string, 0, len(n.fps)+len(n.cueFps)+len(n.cueIDs))
	parts = append(parts, n.fps...)
	for _, fp := range n.cueFps {
		parts = append(parts, cueMark+fp)
	}
	parts = append(parts, n.cueIDs...)
	return strings.Join(parts, " ")
}

// parseNegSegment reads one line's field run back.
//
// Two of the three kinds are told apart by SHAPE rather than by position or a
// prefix, which is what makes the line self-describing: a fingerprint is sixteen
// lower-case hex characters and an id is thirty-two upper-case ones, so the two
// vocabularies do not overlap and a field that is neither is refused rather than
// guessed at. The third — a cue-bound fingerprint — has the shape of the first, so
// it is marked (cueMark) instead; a prefix for all three would have carried the
// same information and made the two existing rules harder to read than they are.
func parseNegSegment(s string) (negSegment, error) {
	var seg negSegment
	if s == "" {
		return seg, nil
	}
	for _, f := range strings.Split(s, " ") {
		if bound, marked := strings.CutPrefix(f, cueMark); marked {
			if !isFingerprintField(bound) {
				return negSegment{}, fmt.Errorf("%q is a %s-marked field over %q, which is not a token fingerprint", f, cueMark, bound)
			}
			seg.cueFps = append(seg.cueFps, bound)
			continue
		}
		switch {
		case isFingerprintField(f):
			seg.fps = append(seg.fps, f)
		case isIDField(f):
			seg.cueIDs = append(seg.cueIDs, f)
		default:
			return negSegment{}, fmt.Errorf("%q is neither a token fingerprint nor a memory id", f)
		}
	}
	return seg, nil
}

// isFingerprintField reports whether f is one 64-bit fingerprint: sixteen
// lower-case hex characters, which is what Fingerprint and unhex64 agree on.
func isFingerprintField(f string) bool {
	if len(f) != 16 || f != strings.ToLower(f) {
		return false
	}
	_, ok := unhex64(f)
	return ok
}

// isIDField reports whether f is one memory id: thirty-two UPPER-case hex
// characters, which is how AddID stores every id and therefore what a line this
// package wrote holds.
func isIDField(f string) bool {
	return len(f) == idLen && f == strings.ToUpper(f) && isHex(f)
}

// SweepStale removes the sidecars in dir that are older than this package's own
// age, and reports how many it took.
//
// The age belongs here and not at the call site, because it is part of the
// sidecar contract rather than a caller's housekeeping: the window has to be
// long enough that a slow child still finds its file, and only this package
// knows how long the child can plausibly take. SweepSidecars stays exported for
// the tests that need an exact age.
func SweepStale(dir string) (int, error) {
	return SweepSidecars(dir, sidecarStaleAfter)
}

// IsSidecarName reports whether a base name is one this package writes.
//
// The one predicate for the naming rule, shared by the sweep and by the child's
// decision to DELETE the file it was handed. Both need it, and they must agree:
// a sweep that is narrower than the delete leaves files behind, and a sweep that
// is wider removes another process's, and neither failure is visible in a test
// that only exercises one of them.
func IsSidecarName(name string) bool {
	return strings.HasPrefix(name, "ghost-audit-") && strings.HasSuffix(name, ".signals")
}

// IsSidecarPath reports whether path's base name is a sidecar this package
// writes, whatever the directory it sits in.
//
// It is deliberately about the NAME and not the content. A caller asking "may I
// delete this file" has to be able to ask before reading it, and the content test
// belongs to whoever has read it — see ReadSidecar, which refuses any header this
// package did not write.
func IsSidecarPath(path string) bool {
	return IsSidecarName(filepath.Base(path))
}

// SweepSidecars removes sidecars in dir older than maxAge, and reports how many
// it took.
//
// Scoped twice — by NAME and by header, of any version — because the directory is the OS temp
// dir, shared with every other process on the machine, and a sweep that removed
// anything matching a glob there would be a bug waiting for a collision. A file
// that merely shares the prefix is left alone.
func SweepSidecars(dir string, maxAge time.Duration) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("audit: sweep sidecars: %w", err)
	}
	cutoff := time.Now().Add(-maxAge)
	removed := 0
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		name := ent.Name()
		if !IsSidecarName(name) {
			continue
		}
		path := filepath.Join(dir, name)
		info, err := ent.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		raw, err := os.ReadFile(path)
		// ANY version's header, not only the current one: a file an older build wrote can
		// never be read again (ReadSidecar refuses it by name), so a sweep that matched
		// only the current header left every one of them behind for good.
		if err != nil || !bytes.HasPrefix(raw, []byte(sidecarHeaderPrefix)) {
			continue
		}
		if err := os.Remove(path); err == nil {
			removed++
		}
	}
	return removed, nil
}
