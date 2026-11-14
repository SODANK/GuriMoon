package lsc

import (
	"regexp"
	"strings"
)

// Chat link style reset. The client's label re-parses a <url> tag when the word holding it
// wraps, so the link's orange bold leaks past </url>. After each </url> the server writes the
// colour and bold the text had before the link. See docs/server-notes.md#lsc-link-style-reset.

// chatTag matches one markup tag the way the client's label splits them.
var chatTag = regexp.MustCompile(`<([^<>]*)>`)

// chatColorValue is a colour the label accepts: an 0xAARRGGBB literal or a named colour.
var chatColorValue = regexp.MustCompile(`^(?:0x[0-9a-fA-F]{8}|[A-Za-z]+)$`)

// chatRoleColor is lscchannel.GetColor: the first role bit in this order picks the colour of
// the sender's text, else the default grey.
func chatRoleColor(role int64) string {
	for _, rc := range []struct {
		bit   int64
		color string
	}{
		{8192, "0xff0099ff"},   // ROLE_QA
		{16, "0xffee6666"},     // ROLE_GMH
		{8, "0xffffff20"},      // ROLE_GML
		{2048, "0xff00ff00"},   // ROLE_CENTURION
		{262144, "0xff00ffcc"}, // ROLE_LEGIONEER
		{32, "0xffee6666"},     // ROLE_ADMIN
	} {
		if role&rc.bit == rc.bit {
			return rc.color
		}
	}
	return "0xffe0e0e0"
}

// resetChatLinkStyle appends <color=C> and <b> or </b> after every </url> in message, where
// C and bold are the state just before the matching link opened. role is the sender's role,
// which sets the starting colour. A message that starts with "/" other than "/emote" is drawn
// in the client's command cyan. A message with no </url> is returned unchanged.
func resetChatLinkStyle(message string, role int64) string {
	if !strings.Contains(strings.ToLower(message), "</url>") {
		return message
	}
	type style struct {
		color string
		bold  bool
	}
	cur := style{color: chatRoleColor(role)}
	if strings.HasPrefix(message, "/") && !strings.HasPrefix(message, "/emote") && message != "/" {
		cur.color = "0xff00dddd"
	}
	saved := cur
	inLink := false

	var b strings.Builder
	last := 0
	for _, loc := range chatTag.FindAllStringSubmatchIndex(message, -1) {
		inner := strings.TrimSpace(message[loc[2]:loc[3]])
		tag := strings.ToLower(inner)
		b.WriteString(message[last:loc[1]])
		last = loc[1]
		switch {
		case tag == "/url":
			if !inLink {
				continue
			}
			inLink = false
			cur = saved
			b.WriteString("<color=" + cur.color + ">")
			if cur.bold {
				b.WriteString("<b>")
			} else {
				b.WriteString("</b>")
			}
		case strings.HasPrefix(tag, "url"):
			if !inLink {
				saved = cur
			}
			inLink = true
		case inLink:
			// Tags inside a link change only the link text; the label restores the
			// pre-link state at </url>.
		case tag == "b":
			cur.bold = true
		case tag == "/b":
			cur.bold = false
		case strings.HasPrefix(tag, "color="):
			if v := strings.TrimSpace(inner[len("color="):]); chatColorValue.MatchString(v) {
				cur.color = v
			}
		}
	}
	b.WriteString(message[last:])
	return b.String()
}
