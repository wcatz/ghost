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
	SidecarHeader = "# ghost-audit-signals v1"

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
// passes its path, and what crosses the gap is ids and hashes — which is also
// why the file is safe to leave on disk for a while.
type Signals struct {
	ids   []string
	prose []string
	saves []string
	// negated holds one entry per agent-authored sentence that carried a denial
	// cue, so a contradiction can be attributed to the sentence it happened in
	// rather than to the whole transcript.
	negated  []negSegment
	degraded string
}

// negSegment is ONE cued sentence: the fingerprints of its words and the ids it
// names.
//
// Both, and in one entry, because the two arms of the negation rule ask the same
// question of the same sentence — "did the agent deny this memory here" — by
// wording for a memory whose text it repeated and by name for one it referred to.
// Splitting them into two lists would let a cue in one sentence satisfy the id
// arm for a memory named in another, which is the same leak the segment split
// exists to close.
type negSegment struct {
	fps []string
	ids []string
}

// idsNamed reports whether this sentence named the given memory, upper-cased on
// both sides for the reason AddID gives.
func (n negSegment) idsNamed(id string) bool {
	if id == "" {
		return false
	}
	upper := strings.ToUpper(id)
	for _, ex := range n.ids {
		if ex == upper {
			return true
		}
	}
	return false
}

// AddID records a memory id the agent named, upper-cased and de-duplicated: the
// comparison upper-cases both sides, because a hex id has exactly one
// case-insensitive spelling and a host that lower-cases one named the same memory.
func (s *Signals) AddID(id string) {
	upper := strings.ToUpper(id)
	for _, ex := range s.ids {
		if ex == upper {
			return
		}
	}
	s.ids = append(s.ids, upper)
}

// AddProse records the agent's own words: its prose, and the arguments of the
// tool calls it made. Ids are lifted out separately, so the token arm can never be
// satisfied by an id (an id is evidence on its own arm, and hashing it as a word
// would let a memory "match itself" without the agent saying anything).
func (s *Signals) AddProse(text string) {
	s.addFingerprints(&s.prose, DistinctTokens(text))
	for _, id := range memoryIDs(text) {
		s.AddID(id)
	}
	for _, seg := range segments(text) {
		if HasNegationCue(seg) {
			s.negated = append(s.negated, negSegment{
				fps: DistinctTokens(seg),
				ids: memoryIDs(seg),
			})
		}
	}
}

// AddSaveArgs records the words of a Ghost SAVE's arguments.
//
// Kept out of prose on purpose. A save that restates a memory is the agent
// declaring the same knowledge again, which is the superseded-in-session bucket and
// not a use — and if save text counted as usage, that bucket could never be
// non-empty.
func (s *Signals) AddSaveArgs(text string) {
	s.addFingerprints(&s.saves, DistinctTokens(text))
	for _, id := range memoryIDs(text) {
		s.AddID(id)
	}
}

// AddToolArgs records a non-save tool call's arguments, which are the agent's own
// words like any other.
func (s *Signals) AddToolArgs(text string) { s.AddProse(text) }

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
func (s *Signals) Empty() bool {
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

func (s *Signals) addFingerprints(dst *[]string, fps []string) {
	seen := make(map[string]bool, len(*dst)+len(fps))
	for _, f := range *dst {
		seen[f] = true
	}
	for _, f := range fps {
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
func WriteSidecar(dir string, s *Signals) (string, error) {
	f, err := os.CreateTemp(dir, "ghost-audit-*.signals")
	if err != nil {
		return "", fmt.Errorf("audit: create sidecar: %w", err)
	}
	path := f.Name()
	var buf bytes.Buffer
	buf.WriteString(SidecarHeader)
	buf.WriteByte('\n')
	for _, id := range s.ids {
		buf.WriteString("id " + id + "\n")
	}
	for _, fp := range s.prose {
		buf.WriteString("prose " + fp + "\n")
	}
	for _, fp := range s.saves {
		buf.WriteString("save " + fp + "\n")
	}
	for _, seg := range s.negated {
		buf.WriteString("neg " + seg.field() + "\n")
	}
	if s.degraded != "" {
		buf.WriteString("degraded " + strconv.QuoteToASCII(s.degraded) + "\n")
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

// ReadSidecar reads a Signals back, refusing any file that is not one this
// version wrote.
//
// Strict on purpose: the path arrives on a command line from a hook, so a
// truncated, edited or foreign file must fail rather than be read as "the agent
// used nothing" — which is the one reading that would make the report confidently
// wrong. A field it cannot parse is an error, not a default.
func ReadSidecar(path string) (*Signals, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("audit: read sidecar: %w", err)
	}
	rest, ok := bytes.CutPrefix(raw, []byte(SidecarHeader+"\n"))
	if !ok {
		return nil, errors.New("audit: sidecar header is not " + SidecarHeader)
	}
	s := &Signals{}
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
			s.AddID(string(value))
		case "prose":
			fp, ok := unhex64(string(value))
			if !ok {
				return nil, fmt.Errorf("audit: sidecar line %d: %q is not a token fingerprint", i+1, value)
			}
			s.prose = append(s.prose, fp)
		case "save":
			fp, ok := unhex64(string(value))
			if !ok {
				return nil, fmt.Errorf("audit: sidecar line %d: %q is not a token fingerprint", i+1, value)
			}
			s.saves = append(s.saves, fp)
		case "neg":
			seg, err := parseNegSegment(string(value))
			if err != nil {
				return nil, fmt.Errorf("audit: sidecar line %d: %w", i+1, err)
			}
			s.negated = append(s.negated, seg)
		case "degraded":
			unquoted, err := strconv.Unquote(string(value))
			if err != nil {
				return nil, fmt.Errorf("audit: sidecar line %d: degraded reason is not quoted: %w", i+1, err)
			}
			s.degraded = unquoted
		default:
			return nil, fmt.Errorf("audit: sidecar line %d has unknown field %q", i+1, kind)
		}
	}
	return s, nil
}

// field renders one cued sentence for the sidecar: its fingerprints and the ids
// it named, in one space-separated run.
//
// Order is the segment's own, and both halves are de-duplicated the way prose and
// saves are, because the reader re-adds them through addFingerprints and the
// no-duplicates invariant has to hold on BOTH paths — a file written by one build
// and read by another must not be distinguishable from one written and read by
// the same.
func (n negSegment) field() string {
	parts := make([]string, 0, len(n.fps)+len(n.ids))
	parts = append(parts, n.fps...)
	parts = append(parts, n.ids...)
	return strings.Join(parts, " ")
}

// parseNegSegment reads one line's field run back.
//
// The two kinds are told apart by SHAPE rather than by position or a prefix,
// which is what makes the line self-describing: a fingerprint is sixteen
// lower-case hex characters and an id is thirty-two upper-case ones, so the two
// vocabularies do not overlap and a field that is neither is refused rather than
// guessed at. A prefix would have worked too, but it would have made the sidecar
// format carry a third thing to keep in step with the two it already has.
func parseNegSegment(s string) (negSegment, error) {
	var seg negSegment
	if s == "" {
		return seg, nil
	}
	for _, f := range strings.Split(s, " ") {
		switch {
		case isFingerprintField(f):
			seg.fps = append(seg.fps, f)
		case isIDField(f):
			seg.ids = append(seg.ids, f)
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

// SweepSidecars removes sidecars in dir older than maxAge, and reports how many
// it took.
//
// Scoped twice — by NAME and by header — because the directory is the OS temp
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
		if !strings.HasPrefix(name, "ghost-audit-") || !strings.HasSuffix(name, ".signals") {
			continue
		}
		path := filepath.Join(dir, name)
		info, err := ent.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil || !bytes.HasPrefix(raw, []byte(SidecarHeader)) {
			continue
		}
		if err := os.Remove(path); err == nil {
			removed++
		}
	}
	return removed, nil
}
