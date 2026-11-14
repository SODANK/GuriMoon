package esp

// Wars & Bills admin page.
//
// The page is a window onto the same machinery the wire uses: declaring from
// here runs the identical fee/mail/push path as an in-game declaration (via
// WarDeclareHook, wired to declareWar in main.go), so a GM war is
// indistinguishable from a player one.  The one GM-only lever is "hostile
// immediately": the 24-hour arming delay is client arithmetic on timeDeclared,
// and backdating that stamp is the only way to skip it.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
)

// War hooks, set by the game server so panel actions run the full RPC path (fee to CONCORD,
// OnWarChanged pushes, CONCORD mail), not just a DB write.
var (
	WarDeclareHook   func(declaredBy, against int64, backdate bool) error
	WarRetractHook   func(warID int64) error
	WarMutualHook    func(warID int64, mutual bool) error
	WarFinishHook    func(warID int64, reason string) error
	BillPayHook      func(billID int64) error
	BillWriteOffHook func(billID int64) error
)

// handleWars serves the wars + bills page.
func (s *Server) handleWars(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	if r.Method == http.MethodPost {
		s.handleWarsAction(w, r, ctx)
		return
	}

	wars, err := evedb.ListWars(ctx, s.db)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "wars"})
		return
	}
	bills, _ := evedb.ListOpenBills(ctx, s.db)
	corps, _ := evedb.ESPListCorps(ctx, s.db, "", 0, "player", corpPickerCap, 0)
	alliances, _ := evedb.ESPListAlliancesFull(ctx, s.db)

	var active int
	for _, wv := range wars {
		if wv.Active {
			active++
		}
	}

	s.render(w, "wars.html", map[string]any{
		"Wars":       wars,
		"ActiveWars": active,
		"Bills":      bills,
		"Corps":      corps,
		"Alliances":  alliances,
		"DeclCost":   evedb.WarDeclarationCost(ctx, s.db),
		"WeeklyCost": evedb.WarWeeklyBillCost(ctx, s.db),
		"MaxDecl":    evedb.WarMaxDeclarations(ctx, s.db),
		"GraceDays":  evedb.BillGraceDays(ctx, s.db),
		"OfficeRent": evedb.OfficeRentCost(ctx, s.db),
		"HooksLive":  WarDeclareHook != nil,
		"Msg":        r.URL.Query().Get("msg"),
		"Page":       "wars",
	})
}

func (s *Server) handleWarsAction(w http.ResponseWriter, r *http.Request, ctx context.Context) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/wars?msg="+url.QueryEscape("Bad form: "+err.Error()), http.StatusSeeOther)
		return
	}
	var result string
	hooksDown := "The game server has not wired the war hooks — action unavailable."

	switch r.FormValue("action") {

	case "declare":
		declaredBy := espFormInt(r, "declaredByID")
		against := espFormInt(r, "againstID")
		backdate := r.FormValue("backdate") != ""
		if WarDeclareHook == nil {
			result = hooksDown
			break
		}
		if err := WarDeclareHook(declaredBy, against, backdate); err != nil {
			result = "Error: " + err.Error()
			break
		}
		if backdate {
			result = "War declared and backdated — hostile immediately. The fee went to CONCORD and both sides were mailed."
		} else {
			result = "War declared. Hostilities become lawful in 24 hours (the delay is client arithmetic on timeDeclared). The fee went to CONCORD and both sides were mailed."
		}

	case "retract":
		if WarRetractHook == nil {
			result = hooksDown
			break
		}
		if err := WarRetractHook(espFormInt(r, "warID")); err != nil {
			result = "Error: " + err.Error()
			break
		}
		result = "War retracted. It stays hostile for 24 more hours (the client's own rule), then the sweep concludes it."

	case "mutual":
		if WarMutualHook == nil {
			result = hooksDown
			break
		}
		mutual := r.FormValue("mutual") == "1"
		if err := WarMutualHook(espFormInt(r, "warID"), mutual); err != nil {
			result = "Error: " + err.Error()
			break
		}
		if mutual {
			result = "War is now mutual — both parties want it, so CONCORD stops billing the aggressor."
		} else {
			result = "War is no longer mutual; weekly billing resumes."
		}

	case "finish":
		if WarFinishHook == nil {
			result = hooksDown
			break
		}
		if err := WarFinishHook(espFormInt(r, "warID"), "concluded by GM intervention"); err != nil {
			result = "Error: " + err.Error()
			break
		}
		result = "War concluded; both sides were mailed."

	case "paybill":
		if BillPayHook == nil {
			result = hooksDown
			break
		}
		if err := BillPayHook(espFormInt(r, "billID")); err != nil {
			result = "Error: " + err.Error()
			break
		}
		result = "Bill paid: the debtor was debited and the creditor credited, with a journal row in each wallet."

	case "writeoff":
		if BillWriteOffHook == nil {
			result = hooksDown
			break
		}
		if err := BillWriteOffHook(espFormInt(r, "billID")); err != nil {
			result = "Error: " + err.Error()
			break
		}
		result = "Bill written off — the claim is voided, no money moved."

	case "warsettings":
		decl, _ := strconv.ParseFloat(strings.TrimSpace(r.FormValue("declCost")), 64)
		weekly, _ := strconv.ParseFloat(strings.TrimSpace(r.FormValue("weeklyCost")), 64)
		maxDecl := espFormInt(r, "maxDecl")
		if err := evedb.SetWarCostSettings(ctx, s.db, decl, weekly, maxDecl); err != nil {
			result = "Error: " + err.Error()
			break
		}
		result = fmt.Sprintf("War costs saved: %.0f ISK to declare, %.0f ISK per week, %d concurrent declarations.", decl, weekly, maxDecl)

	case "billsettings":
		grace, _ := strconv.ParseFloat(strings.TrimSpace(r.FormValue("graceDays")), 64)
		rent, _ := strconv.ParseFloat(strings.TrimSpace(r.FormValue("officeRent")), 64)
		if err := evedb.SetBillSettings(ctx, s.db, grace, rent); err != nil {
			result = "Error: " + err.Error()
			break
		}
		result = fmt.Sprintf("Bill settings saved: %.4g day(s) grace, %.0f ISK office rent per 30 days.", grace, rent)

	default:
		result = "Unknown action."
	}

	http.Redirect(w, r, "/wars?msg="+url.QueryEscape(result), http.StatusSeeOther)
}
