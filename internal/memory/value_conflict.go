package memory

import (
	"strings"
	"unicode"
)

// statesDifferentValue reports whether two similar texts state DIFFERENT values
// of one fact, which makes the newer one a replacement rather than a second
// wording, and so not something a save may fold.
func statesDifferentValue(a, b string) bool {
	va, vb := valueTokens(a), valueTokens(b)
	return lacksValue(va, vb) && lacksValue(vb, va)
}

func lacksValue(from, other map[string]bool) bool {
	for t := range from {
		if other[t] {
			continue
		}
		if core, ok := numericCore(t); ok && other[core] {
			continue
		}
		if isBareNumber(t) && hasUnitOf(other, t) {
			continue
		}
		return true
	}
	return false
}

func hasUnitOf(set map[string]bool, n string) bool {
	for t := range set {
		if core, ok := numericCore(t); ok && core == n {
			return true
		}
	}
	return false
}

func valueTokens(s string) map[string]bool {
	out := map[string]bool{}
	for _, f := range valueFields(s) {
		if f.value {
			out[f.word] = true
		}
	}
	return out
}

type valueField struct {
	word          string
	value, shaped bool
	sentence      int
}

func valueFields(s string) []valueField {
	var out []valueField
	segments := strings.Split(s, "`")
	sentenceStart, lineStart := true, true
	sentence := -1
	for si, seg := range segments {
		code := si%2 == 1 && si < len(segments)-1
		for li, line := range strings.Split(seg, "\n") {
			if li > 0 {
				sentenceStart, lineStart = true, true
			}
			for _, raw := range strings.Fields(line) {
				if lineStart && !code && isListMarker(raw) {
					continue
				}
				lineStart = false
				tok := strings.Trim(raw, "\"'«»“”‘’()[]{}<>.,;:!?*")
				tok = groupedDigits(strings.TrimRight(tok, "/"))
				starts := sentenceStart
				if starts || sentence < 0 {
					sentence++
				}
				closed := strings.TrimRight(raw, "\"'»”’)]}*")
				sentenceStart = closed != "" && strings.ContainsAny(closed[len(closed)-1:], ".!?:")
				if tok == "" {
					continue
				}
				shaped := isValueToken(tok) || (!starts && isName(tok))
				out = append(out, valueField{word: strings.ToLower(tok), value: code || shaped, shaped: shaped, sentence: sentence})
			}
		}
	}
	return out
}

func isListMarker(f string) bool {
	switch f {
	case "-", "*", "+":
		return true
	}
	if strings.Trim(f, "#") == "" {
		return true
	}
	if len(f) < 2 || (f[len(f)-1] != '.' && f[len(f)-1] != ')') {
		return false
	}
	for _, r := range f[:len(f)-1] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func groupedDigits(t string) string {
	if !strings.Contains(t, ",") {
		return t
	}
	isDigit := func(i int) bool { return i >= 0 && i < len(t) && t[i] >= '0' && t[i] <= '9' }
	var b strings.Builder
	for i := 0; i < len(t); i++ {
		if t[i] == ',' && isDigit(i-1) && isDigit(i+1) && isDigit(i+2) && isDigit(i+3) && !isDigit(i+4) {
			continue
		}
		b.WriteByte(t[i])
	}
	return b.String()
}

func isName(tok string) bool {
	for _, r := range tok {
		return unicode.IsUpper(r)
	}
	return false
}

func isValueToken(tok string) bool {
	alnum := 0
	inner := false
	upperAfterFirst := false
	for i, r := range tok {
		switch {
		case unicode.IsDigit(r):
			return true
		case unicode.IsLetter(r):
			alnum++
			if i > 0 && unicode.IsUpper(r) {
				upperAfterFirst = true
			}
		case strings.ContainsRune("./_=:@#\\", r):
			inner = true
		}
	}
	return upperAfterFirst || (inner && alnum >= 3)
}

func numericCore(t string) (string, bool) {
	i := 0
	for i < len(t) && (t[i] >= '0' && t[i] <= '9' || (i > 0 && (t[i] == '.' || t[i] == ',') && i+1 < len(t) && t[i+1] >= '0' && t[i+1] <= '9')) {
		i++
	}
	if i == 0 || i == len(t) {
		return "", false
	}
	suffix := t[i:]
	if len(suffix) > 3 {
		return "", false
	}
	for _, r := range suffix {
		if !unicode.IsLetter(r) && r != '%' {
			return "", false
		}
	}
	return t[:i], true
}

func isBareNumber(t string) bool {
	if t == "" {
		return false
	}
	for _, r := range t {
		if (r < '0' || r > '9') && r != '.' && r != ',' {
			return false
		}
	}
	return t[0] >= '0' && t[0] <= '9'
}
