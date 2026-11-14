package esp

// The DED's Sovereignty tab: which alliances took which 0.0 systems, newest first, from sovHistory
// (db/sovhistorydb.go).
//
//	/concord/sovereignty?v=all&alliance=N&page=N
//
// Default view is acquisitions only; v=all adds systems lost to nobody.

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
)

const concordSovPageSize = 50

func sovURL(all bool, alliance int64, page int) string {
	q := []string{}
	if all {
		q = append(q, "v=all")
	}
	if alliance != 0 {
		q = append(q, "alliance="+strconv.FormatInt(alliance, 10))
	}
	if page > 0 {
		q = append(q, "page="+strconv.Itoa(page))
	}
	u := newsHost + "/concord/sovereignty"
	if len(q) > 0 {
		u += "?" + strings.Join(q, "&amp;")
	}
	return u
}

// sovAllianceName is "Name [TICK]", or a fallback for an alliance that no longer exists.
func sovAllianceName(id int64, name, short string) string {
	if name == "" {
		name = "Alliance " + strconv.FormatInt(id, 10)
	}
	out := igbText(name)
	if short != "" {
		out += " <font color=" + cdDim + ">[" + igbText(short) + "]</font>"
	}
	return out
}

// sovAllianceLink narrows the tab to one alliance.
func sovAllianceLink(all bool, id int64, name, short string) string {
	if id == 0 {
		return "<font color=" + cdDim + ">nobody</font>"
	}
	return `<a href="` + sovURL(all, id, 0) + `">` + sovAllianceName(id, name, "") + `</a>` +
		map[bool]string{true: " <font color=" + cdDim + ">[" + igbText(short) + "]</font>", false: ""}[short != ""]
}

func (s *NewsServer) handleConcordSov(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	q := r.URL.Query()
	all := q.Get("v") == "all"
	alliance, _ := strconv.ParseInt(q.Get("alliance"), 10, 64)
	page, _ := strconv.Atoi(q.Get("page"))
	page = max(page, 0)

	var b strings.Builder
	writeConcordHeader(&b, "sov")
	b.WriteString("<br><font size=3 color=" + cdDim + ">SOVEREIGNTY OVER 0.0 SOLAR SYSTEMS</font><br>")
	b.WriteString("<font size=3>An alliance holds a system while its corporations anchor the majority of qualifying " +
		"structures there. A challenger that keeps the majority through the grace period takes the system.</font><br><br>\n")

	if page == 0 && alliance == 0 {
		holders, herr := evedb.TopSovHolders(ctx, s.db, concordSidebarRows)
		gainers, gerr := evedb.TopSovGainers(ctx, s.db, time.Now().Add(-7*24*time.Hour), concordSidebarRows)
		if herr != nil || gerr != nil {
			log.Printf("igb: sov panels: holders=%v gainers=%v", herr, gerr)
		}
		panel := func(title, empty string, rows []evedb.SovHolding) igbBandPanel {
			p := igbBandPanel{Title: title, Empty: empty, Align: []string{"", "", "right"}, Width: []string{"34", "", "60"}}
			for _, h := range rows {
				p.Rows = append(p.Rows, []string{igbCorpLogo(h.ExecutorCorp, 32),
					"<font size=3>" + sovAllianceLink(all, h.AllianceID, h.AllianceName, h.ShortName) + "</font>",
					"<font size=3 color=" + cdHi + "><b>" + igbInt(h.Systems) + "</b></font>"})
			}
			return p
		}
		writePanelBand(&b, siteConcord, []igbBandPanel{
			panel("Largest holders", "No system is held.", holders),
			panel("Most systems taken, last 7 days", "None taken this week.", gainers),
		})
		b.WriteString("<br>")
	}

	b.WriteString("<table width=100% bgcolor=" + cdHeader + "><tr><td><font size=3>&nbsp;")
	b.WriteString(igbFilterCell("Systems acquired", sovURL(false, alliance, 0), !all))
	b.WriteString(" &middot; " + igbFilterCell("All changes", sovURL(true, alliance, 0), all))
	if alliance != 0 {
		b.WriteString(` &nbsp;&nbsp; <a href="` + sovURL(all, 0, 0) + `">every alliance</a>`)
	}
	b.WriteString("</font></td></tr></table>\n")

	f := evedb.SovHistoryFilter{AcquiredOnly: !all, AllianceID: alliance,
		Limit: concordSovPageSize, Offset: page * concordSovPageSize}
	rows, err := evedb.ListSovHistory(ctx, s.db, f)
	if err != nil {
		log.Printf("igb: ListSovHistory: %v", err)
		b.WriteString("<br><font size=3>The registry is not available.</font><br>\n")
		finishSitePage(w, &b, siteConcord, 0)
		return
	}
	if len(rows) == 0 {
		b.WriteString("<br><font size=3>No change of sovereignty is on record.</font><br>\n")
		finishSitePage(w, &b, siteConcord, 0)
		return
	}
	writeSovRows(&b, rows, all)

	total, _ := evedb.CountSovHistory(ctx, s.db, f)
	b.WriteString("<font size=3>")
	if page > 0 {
		b.WriteString(`<a href="` + sovURL(all, alliance, page-1) + `">&laquo; newer</a> &nbsp;`)
	}
	if int64((page+1)*concordSovPageSize) < total {
		b.WriteString(`<a href="` + sovURL(all, alliance, page+1) + `">older &raquo;</a>`)
	}
	b.WriteString("</font><br>\n")
	finishSitePage(w, &b, siteConcord, 0)
}

// writeSovRows is one flat table: a dated bar between days, then per change the new holder's logo,
// what happened, the system and region, and the time. A loss to nobody is written in red.
func writeSovRows(b *strings.Builder, rows []evedb.SovEvent, all bool) {
	b.WriteString("<table width=100%>\n")
	day := ""
	for i, e := range rows {
		if d := igbYC(e.ChangedAt); d != day {
			day = d
			b.WriteString("<tr><td colspan=4 bgcolor=" + cdHeader + "><font size=3 color=" + cdTitle + "><b>&nbsp;" + d +
				"</b></font></td></tr>\n")
		}
		shade := cdRowA
		if i%2 == 1 {
			shade = cdRowB
		}
		td := func(attrs string) string { return "<td valign=top bgcolor=" + shade + attrs + ">" }
		sys := concordSystemLink(e.SolarSystemID, e.SystemName) + " " + concordSec(e.Security)

		var line1, line2, logo string
		if e.NewAllianceID != 0 {
			logo = igbCorpLogo(e.NewExecutorCorp, 32)
			line1 = "<font color=" + cdGreen + "><b>Acquired</b></font> " + sys + " &mdash; " +
				sovAllianceLink(all, e.NewAllianceID, e.NewAllianceName, e.NewShortName)
			if e.OldAllianceID != 0 {
				line2 = "taken from " + sovAllianceLink(all, e.OldAllianceID, e.OldAllianceName, e.OldShortName)
			} else {
				line2 = "previously unclaimed"
			}
			if e.ClaimingCorp != "" {
				line2 += " &middot; leading corporation " + igbCorpLink(e.ClaimingCorpID, e.ClaimingCorp)
			}
		} else {
			line1 = "<font color=" + cdRed + "><b>Lost</b></font> " + sys + " &mdash; " +
				sovAllianceLink(all, e.OldAllianceID, e.OldAllianceName, e.OldShortName)
			line2 = "the system is now unclaimed"
		}
		b.WriteString("<tr>")
		b.WriteString(td(" width=36") + logo + "</td>")
		b.WriteString(td("") + "<font size=3>" + line1 + "</font><br><font size=2 color=" + cdDim + ">" + line2 + "</font></td>")
		b.WriteString(td(" width=120") + "<font size=3>" + igbText(e.RegionName) + "</font></td>")
		b.WriteString(td(" width=50 align=right") + "<font size=3 color=" + cdDim + ">" + e.ChangedAt.UTC().Format("15:04") +
			"</font></td>")
		b.WriteString("</tr>\n")
	}
	b.WriteString("</table>\n")
}
