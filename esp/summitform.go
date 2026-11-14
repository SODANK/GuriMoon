package esp

// Cleaning what the in-game browser posts to the Intergalactic Summit (summit.go).
//
// Read from the 3913 client (ui/shared/browser/form.py, control/singlelineedit.py,
// control/bastard/edit.py):
//   - A <textarea> answers VirtualEdit.SaveHTML(): markup (<b>, <i>, <u>, <font size|color>,
//     <a>) with every <br> already turned into "\r\n".
//   - A text <input> answers its text encoded to cp1252, with < and > pre-escaped as &lt; &gt;.
//   - The form is urlencoded after encoding unicode with xmlcharrefreplace, so a character outside
//     the code page arrives as a numeric &#NNNN; reference.
//
// So: bytes that are not UTF-8 are cp1252, tags are dropped, every entity (named or numeric) is
// decoded, and what is stored is plain UTF-8 text. Rendering escapes it again (igbText).

import (
	"html"
	"regexp"
	"strings"
	"unicode/utf8"
)

// cp1252High is cp1252's 0x80-0x9F, the only range where it differs from Latin-1. Unassigned
// bytes map to U+FFFD.
var cp1252High = [32]rune{
	'€', '�', '‚', 'ƒ', '„', '…', '†', '‡', 'ˆ', '‰', 'Š', '‹', 'Œ', '�', 'Ž', '�',
	'�', '‘', '’', '“', '”', '•', '–', '—', '˜', '™', 'š', '›', 'œ', '�', 'ž', 'Ÿ',
}

// summitDecode reads posted bytes as UTF-8 when they are, cp1252 otherwise.
func summitDecode(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c < 0x80:
			b.WriteByte(c)
		case c < 0xA0:
			b.WriteRune(cp1252High[c-0x80])
		default:
			b.WriteRune(rune(c))
		}
	}
	return b.String()
}

var (
	summitBreakRE = regexp.MustCompile(`(?i)<\s*(br|/p|p|/div)\s*/?\s*>`)
	summitTagRE   = regexp.MustCompile(`<[^>]*>`)
	summitGapRE   = regexp.MustCompile(`\n{3,}`)
)

// summitCleanText is a posted body as plain text: lines kept, trailing blanks and runs of more
// than one empty line dropped, control characters removed.
func summitCleanText(raw string) string {
	s := summitDecode(raw)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = summitBreakRE.ReplaceAllString(s, "\n")
	s = summitTagRE.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7F || r == '�' {
			return -1
		}
		return r
	}, s)
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	s = summitGapRE.ReplaceAllString(strings.Join(lines, "\n"), "\n\n")
	return strings.TrimSpace(s)
}

// summitCleanLine is a posted one-line field (a topic's title) as plain text on one line.
func summitCleanLine(raw string) string {
	return strings.Join(strings.Fields(summitCleanText(raw)), " ")
}

// summitBodyHTML renders a stored body for the page: escaped, with its line breaks.
func summitBodyHTML(body string) string {
	return strings.ReplaceAll(igbText(body), "\n", "<br>")
}
