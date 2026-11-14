package esp

// The dossier header: the top panel of /concord/capsuleer and /concord/corporation.
//
// One flat table. The client lays out rowspan (components.py walks every row a cell spans), so
// the logo, the identity fields and the corporation's leaderboard each span the full height while
// the stat grid runs row by row beside them. A nested table cannot be used: see
// docs/galnet-igb.md#browser-limits. The grid shows destroyed against lost in hulls and ISK with
// an efficiency for each, for all time, 90 days and 7 days, then two bars (danger, solo share).

import (
	"strconv"
	"strings"

	evedb "gurimoon/db"
)

// igbDossierStats is the three windows the grid reports. Lifetime carries the archive; the two
// windows cannot (KillActorStats: the rollup has no time axis). A nil window renders as dashes.
type igbDossierStats struct {
	Lifetime, Days90, Days7 *evedb.KillActorStats
}

// igbKVPair is one identity field: a label (upper case, no dots, since it is drawn at size 2)
// and its already-escaped value.
type igbKVPair struct{ Label, Value string }

const (
	igbDossierLogoW   = 136
	igbDossierMiddleW = 190
)

// igbWriteDossierHeader writes the whole header table. middle, when not nil, writes one <td>
// given the rowspan it must carry (the corporation's Most active cell); the pilot page has none.
func igbWriteDossierHeader(b *strings.Builder, logo string, kv []igbKVPair,
	middle func(b *strings.Builder, rowspan int), st igbDossierStats) {

	// The grid always has its header, ships, ISK and both window rows, so it
	// sets the height and every side cell spans exactly that many rows.
	grid := igbStatGrid(st)
	rs := strconv.Itoa(len(grid))

	b.WriteString("<table width=100%>\n")
	for r, row := range grid {
		b.WriteString("<tr>")
		if r == 0 {
			b.WriteString("<td rowspan=" + rs + " width=" + strconv.Itoa(igbDossierLogoW) +
				" valign=top bgcolor=" + cdPanel + ">" + logo + "</td>")

			// Identity: one cell spanning the whole header, each field a small dim label over its value,
			// stacked so a long headquarters name wraps inside its own cell.
			b.WriteString("<td rowspan=" + rs + " valign=top bgcolor=" + cdPanel + ">")
			for _, f := range kv {
				v := f.Value
				if v == "" {
					v = "&mdash;"
				}
				b.WriteString("<font size=2 color=" + cdDim + "><b>" + f.Label + "</b></font><br>" +
					"<font size=3>" + v + "</font><br>")
			}
			b.WriteString("</td>")

			if middle != nil {
				middle(b, len(grid))
			}
		}
		b.WriteString(row)
		b.WriteString("</tr>\n")
	}
	b.WriteString("</table>\n")
}

// igbStatGrid returns the grid's rows, each a run of <td>s spanning four columns. Numeric columns
// have fixed widths so the grid holds its shape beside a long headquarters name.
func igbStatGrid(st igbDossierStats) []string {
	life := st.Lifetime
	if life == nil {
		life = &evedb.KillActorStats{}
	}
	var rows []string

	// Header. Size-2 type drops "." and ":", so no abbreviation carries a dot.
	head := func(s, align string) string {
		return "<td" + align + " bgcolor=" + cdHeader + "><font size=2 color=" + cdDim + "><b>" + s + "</b></font></td>"
	}
	rows = append(rows, head("ALL TIME", " width=64")+head("DESTROYED", " width=92 align=right")+
		head("LOST", " width=92 align=right")+head("EFF %", " width=56 align=right"))

	cell := func(shade, align, inner string) string {
		return "<td" + align + " bgcolor=" + shade + ">" + inner + "</td>"
	}
	label := func(s string) string { return "<font size=3 color=" + cdDim + ">" + s + "</font>" }
	smallLabel := func(s string) string { return "<font size=2 color=" + cdDim + "><b>" + s + "</b></font>" }

	// Ships and ISK, all time, at headline size.
	rows = append(rows,
		cell(cdRowA, "", label("Ships"))+
			cell(cdRowA, " align=right", "<font size=5 color="+cdGreen+"><b>"+igbInt(life.Kills)+"</b></font>")+
			cell(cdRowA, " align=right", "<font size=5 color="+cdRed+"><b>"+igbInt(life.Losses)+"</b></font>")+
			cell(cdRowA, " align=right", igbEffFigure(life.ShipEfficiency(), life.Kills+life.Losses > 0, 4)),
		cell(cdRowB, "", label("ISK"))+
			cell(cdRowB, " align=right", "<font size=4 color="+cdGreen+">"+igbISKShort(life.ISKDestroyed)+"</font>")+
			cell(cdRowB, " align=right", "<font size=4 color="+cdRed+">"+igbISKShort(life.ISKLost)+"</font>")+
			cell(cdRowB, " align=right", igbEffFigure(life.Efficiency(), life.ISKDestroyed+life.ISKLost > 0, 4)),
	)

	// The windows: hulls and ISK in one cell each, efficiency by ISK.
	window := func(shade, name string, w *evedb.KillActorStats) string {
		if w == nil || w.Kills+w.Losses == 0 {
			return cell(shade, "", smallLabel(name)) +
				cell(shade, " align=right", "<font size=3 color="+cdDim+">&mdash;</font>") +
				cell(shade, " align=right", "<font size=3 color="+cdDim+">&mdash;</font>") +
				cell(shade, " align=right", "<font size=3 color="+cdDim+">&mdash;</font>")
		}
		side := func(n int64, isk float64, color string) string {
			return "<font size=3 color=" + color + "><b>" + igbInt(n) + "</b> &middot; " + igbISKShort(isk) + "</font>"
		}
		return cell(shade, "", smallLabel(name)) +
			cell(shade, " align=right", side(w.Kills, w.ISKDestroyed, cdGreen)) +
			cell(shade, " align=right", side(w.Losses, w.ISKLost, cdRed)) +
			cell(shade, " align=right", igbEffFigure(w.Efficiency(), w.ISKDestroyed+w.ISKLost > 0, 3))
	}
	rows = append(rows, window(cdRowA, "90 DAYS", st.Days90), window(cdRowB, "7 DAYS", st.Days7))

	wide := func(inner string) string {
		return "<td colspan=4 bgcolor=" + cdPanel + ">" + inner + "</td>"
	}

	// Violent / Peaceful: the share of hulls on this record that were
	// somebody else's. Red is the danger, so the filled part is red here.
	if life.Kills+life.Losses > 0 {
		d := life.ShipEfficiency()
		rows = append(rows, wide(
			"<font size=3 color="+cdRed+"><b>Violent "+strconv.FormatFloat(d, 'f', 0, 64)+"%</b></font> "+
				"<font size=3>"+igbSplitBar(d/100, 26, cdRed, cdGreen)+"</font> "+
				"<font size=3 color="+cdGreen+"><b>Peaceful</b></font>"))
	}

	// Solo share and average gang: who this record was built with.
	if life.Kills > 0 {
		solo := float64(life.SoloKills) / float64(life.Kills) * 100
		line := "<font size=3 color=" + cdDim + ">Solo</font> <font size=3 color=" + cdHi + "><b>" +
			strconv.FormatFloat(solo, 'f', 0, 64) + "%</b></font> " +
			"<font size=3>" + igbBar(solo/100, 14, cdHi) + "</font>"
		if g := life.AvgGang(); g > 0 {
			line += " <font size=3 color=" + cdDim + ">avg gang</font> <font size=3 color=" + cdHi + "><b>" +
				strconv.FormatFloat(g, 'f', 1, 64) + "</b></font>"
		}
		line += " <font size=3 color=" + cdDim + ">decisive strikes</font> <font size=3 color=" + cdHi + "><b>" +
			igbInt(life.FinalBlows) + "</b></font>"
		rows = append(rows, wide(line))
	}

	// Recency, and the archive disclaimer: the totals above span the retention
	// boundary and the mails behind the archived part no longer exist, so the
	// tables further down cannot add up to them.
	var tail []string
	if !life.LastKill.IsZero() {
		tail = append(tail, "Last incident "+igbRelTime(life.LastKill))
	}
	if !life.LastLoss.IsZero() {
		tail = append(tail, "last loss "+igbRelTime(life.LastLoss))
	}
	if life.HasArchive() {
		tail = append(tail, "includes "+igbInt(life.Archived.Kills)+" archived incident(s) and "+
			igbInt(life.Archived.Losses)+" loss(es) whose reports have aged out")
	}
	if len(tail) > 0 {
		rows = append(rows, wide("<font size=2 color="+cdDim+">"+strings.Join(tail, " &nbsp;&middot;&nbsp; ")+"</font>"))
	}
	return rows
}

// igbEffFigure renders an efficiency percentage, green at or above half and
// red below it. ok false (an empty record) renders a dash: 0/0 is not 0%.
func igbEffFigure(pct float64, ok bool, size int) string {
	sz := strconv.Itoa(size)
	if !ok {
		return "<font size=" + sz + " color=" + cdDim + ">&mdash;</font>"
	}
	c := cdGreen
	if pct < 50 {
		c = cdRed
	}
	return "<font size=" + sz + " color=" + c + "><b>" + strconv.FormatFloat(pct, 'f', 1, 64) + "</b></font>"
}

// igbSplitBar is igbBar with a coloured remainder instead of a grey one: a
// proportion between two named ends rather than progress toward a total.
func igbSplitBar(frac float64, width int, left, right string) string {
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	filled := int(float64(width)*frac + 0.5)
	if filled > width {
		filled = width
	}
	return "<font color=" + left + "><b>" + strings.Repeat("|", filled) + "</b></font>" +
		"<font color=" + right + "><b>" + strings.Repeat("|", width-filled) + "</b></font>"
}
