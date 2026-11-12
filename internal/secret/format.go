package secret

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// IsFormatRune reports whether r is a Unicode FORMAT character — category Cf —
func IsFormatRune(r rune) bool {
	if r < 0x00AD {
		return false
	}
	return unicode.In(r, unicode.Cf)
}

func isInvisibleForDetection(r rune) bool {
	if IsFormatRune(r) {
		return true
	}
	if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) {
		return true
	}
	if unicode.Is(unicode.Cc, r) && r != '\t' && r != '\n' && r != '\r' {
		return true
	}
	switch r {
	case 0x115F, 0x1160, 0x3164, 0xFFA0, 0xFFFC, 0x2800:
		return true
	}
	return false
}

func withoutInvisibleForDetection(text string) (string, bool) {
	first := -1
	for i, r := range text {
		if isInvisibleForDetection(r) {
			first = i
			break
		}
	}
	if first < 0 {
		return text, false
	}
	var b strings.Builder
	b.Grow(len(text))
	b.WriteString(text[:first])
	rest := text[first:]
	for len(rest) > 0 {
		r, size := utf8.DecodeRuneInString(rest)
		if isInvisibleForDetection(r) {
			rest = rest[size:]
			continue
		}
		if r == utf8.RuneError && size == 1 {
			b.WriteByte(rest[0])
		} else {
			b.WriteString(rest[:size])
		}
		rest = rest[size:]
	}
	return b.String(), true
}
