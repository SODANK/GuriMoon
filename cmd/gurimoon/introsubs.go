package main

import (
	"os"
	"path/filepath"
	"strings"

	"gurimoon/marshal"
)

// introSubtitle is one subtitle label and its spoken line.
type introSubtitle struct {
	Label string
	Text  string
}

// parseIntroSubtitles reads the SUBTITLE blocks of the client's res/introMovie.txt. The client
// ships no translation table for the intro, so the spoken line is the "# text" comment above the
// timing line (a bare text line is accepted too, as G_EI_15_04 has). A block with no label or
// no text is skipped.
func parseIntroSubtitles(data string) []introSubtitle {
	var out []introSubtitle
	var text, label string
	inBlock := false
	flush := func() {
		if inBlock && label != "" && text != "" {
			out = append(out, introSubtitle{Label: label, Text: text})
		}
		inBlock, text, label = false, "", ""
	}
	for _, raw := range strings.Split(data, "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case line == "":
			continue
		case line == "SUBTITLE":
			flush()
			inBlock = true
		case !inBlock:
			continue
		case strings.HasPrefix(line, "#"):
			if text == "" {
				text = strings.TrimSpace(strings.TrimPrefix(line, "#"))
			}
		case len(line) >= 2 && line[0] == '"' && line[len(line)-1] == '"':
			label = line[1 : len(line)-1]
			flush()
		case isIntroTimingLine(line):
			continue
		case strings.ToUpper(line) == line && !strings.ContainsAny(line, " ,.") && text == "":
			// Another block keyword (SEGMENT, AUDIO ...) ended this block without a label.
			flush()
		default:
			if text == "" {
				text = line
			}
		}
	}
	flush()
	return out
}

// isIntroTimingLine reports whether line looks like "0:3.72 - 0:0.48".
func isIntroTimingLine(line string) bool {
	return len(line) > 0 && line[0] >= '0' && line[0] <= '9' && strings.Contains(line, " - ")
}

// loadIntroSubtitleRows returns the (textLabel, text) rows for GetTextsForGroup, read from
// <clientPath>/res/introMovie.txt. An unset path or a missing file gives no rows, and the
// intro plays without subtitles.
func loadIntroSubtitleRows(clientPath string) []marshal.PyRep {
	if clientPath == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(clientPath, "res", "introMovie.txt"))
	if err != nil {
		return nil
	}
	subs := parseIntroSubtitles(string(data))
	rows := make([]marshal.PyRep, 0, len(subs))
	for _, s := range subs {
		rows = append(rows, marshal.PyList{Items: []marshal.PyRep{
			marshal.PyString{Value: s.Label}, marshal.PyString{Value: s.Text},
		}})
	}
	return rows
}
