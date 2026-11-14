package esp

// Shared pieces of CONCORD's Capsuleer Incident Registry (concord.go): icons, links, the
// security tint and the incident report's fitting list. Images use the client's pseudo-URL
// schemes (typeicon:, portrait:, corplogo:); see docs/galnet-igb.md#pseudo-url-images.

import (
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
)

const igbKBPageSize = 25

// igbFitSideWidth is the fixed width of the column beside the fitting list. The name column
// takes the rest of the window, so it also sets how far a quantity sits from its item.
// igbFitImageWidth is the part of it that holds a card's images: two 64 px images plus the cell margin.
const (
	igbFitSideWidth  = 340
	igbFitImageWidth = 136
)

// igbFitCard is one block of the column beside the fitting list: an image cell and a text cell
// on the shade given, spanning the rows igbFitSpans hands it.
type igbFitCard struct {
	Images, Text, Shade string
}

// igbFitSide is the column beside the fitting list: a heading over the cards, which share out
// the rows below it. See docs/esp-2-notes.md#incident-fitting-list.
type igbFitSide struct {
	Heading string
	Cards   []igbFitCard
	// Span is the rows each card takes. 0 shares every row below the heading out between the cards
	// (igbFitSpans). A positive Span packs the cards at the top and leaves the rest of the column empty.
	Span int
}

// igbSecColor tints a security status the way the client's own overview does:
// green in empire, amber in lowsec, red once CONCORD stops coming.
func igbSecColor(sec float64) string {
	switch {
	case sec >= 0.5:
		return cdGreen
	case sec > 0.0:
		return cdHi
	default:
		return cdRed
	}
}

// igbShipIcon renders an item/ship icon from the client's own atlas.
func igbShipIcon(typeID int64, size int) string {
	if typeID == 0 {
		return ""
	}
	return `<img src="typeicon:` + strconv.FormatInt(typeID, 10) + `" size=` + strconv.Itoa(size) + `>`
}

// igbPortrait renders a pilot's face from the client's render cache. A character the client
// never drew gets the client's placeholder.
func igbPortrait(charID int64, size int) string {
	if charID == 0 {
		return ""
	}
	return `<img src="portrait:` + strconv.FormatInt(charID, 10) + `" size=` + strconv.Itoa(size) + `>`
}

// igbPortraitScaled draws a pilot's face at px from the 64 px render. size picks the client's
// cached file, and only 32 and 64 have a placeholder; see docs/galnet-igb.md#pseudo-url-images.
func igbPortraitScaled(charID int64, px int) string {
	if charID == 0 {
		return ""
	}
	return `<img src="portrait:` + strconv.FormatInt(charID, 10) + `" size=64 width=` + strconv.Itoa(px) +
		` height=` + strconv.Itoa(px) + `>`
}

// igbRelTime renders a kill's age as "4h 12m ago".
func igbRelTime(t time.Time) string {
	d := time.Since(t)
	if d < time.Minute {
		return "just now"
	}
	return igbUptime(d) + " ago"
}

// igbKBLink is the registry front page with the given filters; see
// concordFilter for the full set.
func igbKBLink(kind string, mine, npc bool, sys int64, page int) string {
	return concordFilter{Kind: kind, Mine: mine, NPC: npc, Sys: sys, Page: page}.url()
}

// igbFilterCell renders one filter as a link, or as highlighted text when it is the active one
// (there is no CSS to mark a selection).
func igbFilterCell(label, href string, active bool) string {
	if active {
		return `<font color=` + cdTitle + `><b>` + label + `</b></font>`
	}
	return `<a href="` + href + `">` + label + `</a>`
}

// igbKillLink anchors inner to one kill report, with the row's summary as the tooltip. EVE-HTML
// has no row-level link, so every text piece in a row is its own anchor to the same mail.
func igbKillLink(k evedb.KillmailRow, inner string) string {
	victim := k.VictimName
	if victim == "" {
		victim = "Unknown pilot"
	}
	return `<a href="` + concordIncidentURL(k.KillID) +
		`" alt="` + htmlAttrEscape(victim+" lost a "+k.ShipTypeName+" in "+k.SystemName) +
		`">` + inner + `</a>`
}

// igbTopValueWindows is the ladder the front page's ranked panel walks until it fills. The
// heading names the window it settled on.
var igbTopValueWindows = []struct {
	Label string
	Dur   time.Duration
}{
	// Whole phrases, not durations to prefix: a shared prefix would read "last all time".
	{"last 24 hours", 24 * time.Hour},
	{"last 7 days", 7 * 24 * time.Hour},
	{"last 30 days", 30 * 24 * time.Hour},
	{"all time", 0}, // 0 = no Since predicate
}

const igbTopValueCount = 5

// igbWriteFitList renders every item on the mail as one list in fitting order (high, mid, low,
// rig, drones, cargo), green if it dropped and red if it burned, with the slot band as a
// subheading. side, when not nil, is the column at the right (the incident's parties). See
// docs/esp-2-notes.md#incident-fitting-list.
func igbWriteFitList(b *strings.Builder, km *evedb.KillmailDetail, side *igbFitSide) {
	bands := km.FitBands()
	if len(bands) == 0 {
		return
	}

	rows := igbFitRows(km)
	var spans []int
	if side != nil {
		if side.Span > 0 {
			spans = make([]int, len(side.Cards))
			for i := range spans {
				spans[i] = side.Span
			}
		} else {
			spans = igbFitSpans(rows-1, len(side.Cards))
		}
	}

	// endRow closes a row, first opening the card that starts on it.
	row, card, cardRow := 0, 0, 1
	endRow := func() {
		if card < len(spans) && row == cardRow {
			c, span := side.Cards[card], strconv.Itoa(spans[card])
			td := " valign=top rowspan=" + span + " bgcolor=" + c.Shade + ">"
			b.WriteString("<td width=" + strconv.Itoa(igbFitImageWidth) + td + c.Images + "</td>")
			b.WriteString("<td width=" + strconv.Itoa(igbFitSideWidth-igbFitImageWidth) + td + c.Text + "</td>")
			cardRow += spans[card]
			card++
		}
		row++
		b.WriteString("</tr>\n")
	}

	b.WriteString("<table width=100%>\n<tr><td colspan=5>")
	b.WriteString("<font size=4><b><font color=" + cdTitle + ">Fitting</font></b></font> ")
	b.WriteString("<font size=2><font color=" + cdGreen + "><b>|</b></font> <font color=" + cdDim +
		">dropped</font> &nbsp; <font color=" + cdRed + "><b>|</b></font> <font color=" + cdDim +
		">destroyed</font></font></td>")
	if side != nil {
		b.WriteString("<td width=10 rowspan=" + strconv.Itoa(rows) + "></td><td colspan=2 bgcolor=" + cdPanel + ">" +
			side.Heading + "</td>")
	}
	endRow()

	for _, band := range bands {
		// The band heading carries what dropped out of the band; the head row has whole-ship totals only.
		b.WriteString(`<tr><td colspan=4 bgcolor=` + cdPanel + `><font size=2 color=` +
			cdHi + `><b>` + band.Label + `</b></font></td>`)
		b.WriteString(`<td align=right bgcolor=` + cdPanel + `><font size=2 color=` + cdGreen +
			`>` + igbISK(band.DroppedValue) + `</font> <font size=2 color=` + cdRed + `>` +
			igbISK(band.DestroyedValue) + `</font></td>`)
		endRow()

		// Restart the zebra inside each band.
		for row, it := range band.Items {
			shade := cdRowA
			if row%2 == 1 {
				shade = cdRowB
			}
			td := "<td bgcolor=" + shade + ">"

			// Colour carries dropped vs destroyed, and the leading pipe repeats it in shape because the
			// client's palette is dim on a small font.
			color, mark := cdRed, "|"
			if it.Dropped {
				color = cdGreen
			}

			qty := ""
			if it.Quantity > 1 {
				// Literal "x", not &times;: the renderer is only known to handle the entities the news and
				// skill pages use (nbsp, mdash, quot, amp, lt, gt, laquo, raquo).
				qty = "<font color=" + cdDim + ">x" + igbInt(it.Quantity) + "</font>"
			}

			b.WriteString("<tr>")
			b.WriteString("<td width=8 bgcolor=" + shade + "><font size=2 color=" + color + "><b>" +
				mark + "</b></font></td>")
			b.WriteString("<td width=28 bgcolor=" + shade + ">" + igbShipIcon(it.TypeID, 24) + "</td>")
			b.WriteString(td + "<font size=2 color=" + color + ">" + htmlTextEscape(it.TypeName) +
				"</font></td>")
			b.WriteString("<td width=50 align=right bgcolor=" + shade + "><font size=2>" + qty + "</font></td>")
			b.WriteString("<td width=100 align=right bgcolor=" + shade + "><font size=2 color=" + color +
				">" + igbISK(it.TotalValue) + "</font></td>")
			endRow()
		}
	}
	b.WriteString("</table><br>\n")
}

// igbFitRows is how many table rows the fitting list draws: the heading, then each band and
// its items.
func igbFitRows(km *evedb.KillmailDetail) int {
	rows := 1
	for _, band := range km.FitBands() {
		rows += 1 + len(band.Items)
	}
	return rows
}

// igbFitSpans shares rows out between n cards, the first cards taking the remainder. The caller
// keeps n at or below rows, since a card needs a row to start on.
func igbFitSpans(rows, n int) []int {
	spans := make([]int, n)
	for i := range spans {
		spans[i] = rows / n
		if i < rows%n {
			spans[i]++
		}
	}
	return spans
}
