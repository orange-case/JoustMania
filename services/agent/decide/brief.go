package decide

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxOperatorBriefRunes is the longest operator brief a request carries (#2687,
// UC-040 BR-044): the flag service's free-string bound (registry MaxFreeString).
const MaxOperatorBriefRunes = 512

// NormalizeOperatorBrief cleans a decision.operator_brief value for the prompt
// (decide-contract.md §3, UC-040 BR-044): trimmed, CRLF / CR as a line break,
// every other control character a space, '<' and '>' as '‹' and '›' (so the text
// can neither close the <operator_brief> block nor open another one), and at most
// MaxOperatorBriefRunes runes. A blank value is "" (no brief). It never adds
// anything: the brief steers which offered option the model prefers, nothing else.
func NormalizeOperatorBrief(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ToValidUTF8(s, "")
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteRune(r)
		case r == '\r', unicode.IsControl(r):
			b.WriteByte(' ')
		case r == '<':
			b.WriteRune('‹')
		case r == '>':
			b.WriteRune('›')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	if utf8.RuneCountInString(out) > MaxOperatorBriefRunes {
		out = strings.TrimSpace(string([]rune(out)[:MaxOperatorBriefRunes]))
	}
	return out
}
