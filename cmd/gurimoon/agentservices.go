package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"

	evedb "gurimoon/db"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
	"gurimoon/syncx"
	"gurimoon/tick"
)

// ── Agent services: locator, security status repair, stubs ──────────────────
//
// agents.py wraps every non-initial conversation body in a FORM that posts back through
// DoAction, so agentSays can carry an <input name=actionID>; htmlwriter.Convert() passes
// non-numeric input through as a string. That string is the locator's input channel.
// See docs/agent-services.md#what-the-client-actually-renders-the-two-surfaces.

// Service action IDs. The 1-9 band is missions, 20-21 research, 1000+/2000+ offers, 100000+
// research fields; this band is clear of all of them.
const (
	agentActionLocateStart   = 30
	agentActionLocateConfirm = 31
	agentActionSecQuote      = 40
	agentActionSecConfirm    = 41
	agentActionGiftStub      = 50
	agentActionRumorStub     = 51
)

// Journal refTypes shipped in market_reftypes; the client's journal silently drops any refType
// not in GetRefTypes.
const (
	refTypeAgentLocationServices = 27 // "You paid agent to locate somebody"
	refTypeAgentSecurityServices = 29 // "You paid agent to clean your rep"
)

// locatorMailDelay is config.json agents.locatorDelayMinutes, set at boot.
var locatorMailDelay = 3 * time.Minute

// locatePendingState tracks the two-step locator conversation per character: stage 1 = the
// agent asked for a name (the next string DoAction is the answer), stage 2 = a target is
// resolved and priced, awaiting the pay click. Opening the flow with another agent replaces it.
type locatePendingState struct {
	agentID    int64
	stage      int
	targetID   int64
	targetName string
	fee        int64
	expires    time.Time
}

var locatePending syncx.Map[int64, *locatePendingState] // charID → *locatePendingState

func getLocatePending(charID, agentID int64) *locatePendingState {
	v, ok := locatePending.Load(charID)
	if !ok {
		return nil
	}
	st := v
	if st.agentID != agentID || time.Now().After(st.expires) {
		locatePending.Delete(charID)
		return nil
	}
	return st
}

// serviceText loads an authored dialogue line for a division scope and
// fills its tokens (division line → global line → shipped default).
func serviceText(ctx context.Context, sqldb *sql.DB, divisionID int64, key string, tokens map[string]string) string {
	return evedb.SubstituteServiceText(evedb.GetAgentText(ctx, sqldb, divisionID, key), tokens)
}

// locateNameForm is the name-entry HTML appended after the authored prompt text. The input
// must be named actionID: agents.py DoAction accepts only that kwarg, and the enclosing FORM
// serializes every named non-submit input into the localsvc call.
func locateNameForm(ctx context.Context, sqldb *sql.DB, divisionID int64) string {
	submit := evedb.GetAgentText(ctx, sqldb, divisionID, "locator.submit")
	return fmt.Sprintf(`<br><br><input type="text" name="actionID" size="40"> <input type="submit" value="%s">`, submit)
}

// agentServiceButtons appends the service choices to the opening conversation
// menu. Captions are authorable text keys; sec repair only advertises itself
// to a pilot whose record is actually negative (the info window still lists
// the service either way).
func agentServiceButtons(ctx context.Context, sqldb *sql.DB, agent *evedb.AgentInfo, charID int64, buttons []marshal.PyRep) []marshal.PyRep {
	services, err := evedb.AgentServiceSet(ctx, sqldb, agent)
	if err != nil {
		log.Printf("agentServiceButtons agentID=%d: %v", agent.AgentID, err)
		return buttons
	}
	if services[evedb.ServiceLocator] {
		buttons = append(buttons, agentButton(
			evedb.GetAgentText(ctx, sqldb, agent.DivisionID, "locator.button"), agentActionLocateStart))
	}
	if services[evedb.ServiceSecStatus] {
		if sec, err := evedb.GetCharSecurityRating(ctx, sqldb, charID); err == nil && sec < -0.0001 {
			buttons = append(buttons, agentButton(
				evedb.GetAgentText(ctx, sqldb, agent.DivisionID, "sec.button"), agentActionSecQuote))
		}
	}
	if services[evedb.ServiceGifts] {
		buttons = append(buttons, agentButton(
			evedb.GetAgentText(ctx, sqldb, agent.DivisionID, "gifts.button"), agentActionGiftStub))
	}
	if services[evedb.ServiceRumors] {
		buttons = append(buttons, agentButton(
			evedb.GetAgentText(ctx, sqldb, agent.DivisionID, "rumors.button"), agentActionRumorStub))
	}
	return buttons
}

// shellBtn is agentButton with an authorable caption: the conversation
// shell's player-choice captions (Request Mission, Quit, ...) resolve through
// the dialogue table per division, like every other agent line.
func shellBtn(ctx context.Context, sqldb *sql.DB, agent *evedb.AgentInfo, key string, actionID int) marshal.PyRep {
	div := int64(0)
	if agent != nil {
		div = agent.DivisionID
	}
	return agentButton(evedb.GetAgentText(ctx, sqldb, div, key), actionID)
}

// isAgentServiceAction reports whether handleAgentDoAction should route to
// handleAgentServiceAction: any numbered service action, or a typed string
// while a locator conversation is waiting for a name.
func isAgentServiceAction(actionID int, actionText string) bool {
	switch actionID {
	case agentActionLocateStart, agentActionLocateConfirm,
		agentActionSecQuote, agentActionSecConfirm,
		agentActionGiftStub, agentActionRumorStub:
		return true
	}
	return actionText != ""
}

// handleAgentServiceAction runs the service side of an agent conversation.
// Callers have already passed the CanUseAgent standing gate.
func handleAgentServiceAction(ctx context.Context, c *evenet.Conn, accountID int64, addr string,
	agent *evedb.AgentInfo, charID int64, actionID int, actionText string, lp int, sqldb *sql.DB) marshal.PyRep {

	services, err := evedb.AgentServiceSet(ctx, sqldb, agent)
	if err != nil {
		log.Printf("[%s] service action: AgentServiceSet agentID=%d: %v", addr, agent.AgentID, err)
		services = map[string]bool{}
	}
	agentID := agent.AgentID
	cancelBtn := func() marshal.PyRep {
		return agentButton(evedb.GetAgentText(ctx, sqldb, agent.DivisionID, "locator.cancel"), agentActionQuit)
	}
	secCancelBtn := func() marshal.PyRep {
		return agentButton(evedb.GetAgentText(ctx, sqldb, agent.DivisionID, "sec.cancel"), agentActionQuit)
	}
	xtra := agentXtraInfo(agentID, 0, 0, lp)
	noSuchService := func() marshal.PyRep {
		locatePending.Delete(charID)
		return agentConversation("I offer no such service. Perhaps you have me confused with a colleague.",
			[]marshal.PyRep{shellBtn(ctx, sqldb, agent, "shell.btn.quit", agentActionQuit)}, xtra)
	}

	// A typed string is only meaningful while a locator prompt is open.
	if actionText != "" {
		st := getLocatePending(charID, agentID)
		if st == nil || !services[evedb.ServiceLocator] {
			return agentConversation("I am not sure what you mean by that.",
				[]marshal.PyRep{shellBtn(ctx, sqldb, agent, "shell.btn.quit", agentActionQuit)}, xtra)
		}
		return locatorResolveName(ctx, sqldb, agent, charID, actionText, st, lp)
	}

	switch actionID {
	case agentActionLocateStart:
		if !services[evedb.ServiceLocator] {
			return noSuchService()
		}
		if evedb.HasPendingLocatorJob(ctx, sqldb, charID, agentID) {
			return agentConversation(serviceText(ctx, sqldb, agent.DivisionID, "locator.busy", nil),
				[]marshal.PyRep{cancelBtn()}, xtra)
		}
		locatePending.Store(charID, &locatePendingState{
			agentID: agentID, stage: 1, expires: time.Now().Add(10 * time.Minute),
		})
		text := serviceText(ctx, sqldb, agent.DivisionID, "locator.prompt", map[string]string{
			"range": evedb.LocatorRangeName(agent.Level),
			"pilot": evedb.GetCharacterName(ctx, sqldb, charID),
		}) + locateNameForm(ctx, sqldb, agent.DivisionID)
		return agentConversation(text, []marshal.PyRep{cancelBtn()}, xtra)

	case agentActionLocateConfirm:
		if !services[evedb.ServiceLocator] {
			return noSuchService()
		}
		st := getLocatePending(charID, agentID)
		if st == nil || st.stage != 2 {
			return agentConversation("We had not agreed on a search. Let us start over.",
				[]marshal.PyRep{agentButton(evedb.GetAgentText(ctx, sqldb, agent.DivisionID, "locator.button"), agentActionLocateStart), cancelBtn()}, xtra)
		}
		bal, err := evedb.GetCharacterBalance(ctx, sqldb, charID)
		if err != nil || bal < float64(st.fee) {
			return agentConversation(serviceText(ctx, sqldb, agent.DivisionID, "locator.poor",
				map[string]string{"fee": commaGroups(st.fee)}),
				[]marshal.PyRep{cancelBtn()}, xtra)
		}
		if err := evedb.GiveISK(ctx, sqldb, charID, -float64(st.fee)); err != nil {
			log.Printf("[%s] locator: debit charID=%d fee=%d: %v", addr, charID, st.fee, err)
			return agentConversation("Something went wrong with the transfer. No charge was made.",
				[]marshal.PyRep{cancelBtn()}, xtra)
		}
		if newBal, err := evedb.GetCharacterBalance(ctx, sqldb, charID); err == nil {
			_ = evedb.InsertJournalEntry(ctx, sqldb, charID, refTypeAgentLocationServices,
				charID, agentID, -float64(st.fee), newBal, "Location of "+st.targetName)
			pushAccountChange(c, accountID, charID, newBal)
		}
		if err := evedb.CreateLocatorJob(ctx, sqldb, charID, agentID, st.targetID, st.fee, locatorMailDelay); err != nil {
			log.Printf("[%s] locator: CreateLocatorJob charID=%d target=%d: %v", addr, charID, st.targetID, err)
		}
		locatePending.Delete(charID)
		log.Printf("[%s] → locator: charID=%d paid %d ISK to agent %d for target %d (due in %s)",
			addr, charID, st.fee, agentID, st.targetID, locatorMailDelay)
		return agentConversation(serviceText(ctx, sqldb, agent.DivisionID, "locator.paid", map[string]string{
			"delay":  fmt.Sprintf("%d", int(locatorMailDelay.Minutes())),
			"target": st.targetName,
		}), []marshal.PyRep{shellBtn(ctx, sqldb, agent, "shell.btn.quit", agentActionQuit)}, xtra)

	case agentActionSecQuote, agentActionSecConfirm:
		if !services[evedb.ServiceSecStatus] {
			return noSuchService()
		}
		sec, err := evedb.GetCharSecurityRating(ctx, sqldb, charID)
		if err != nil {
			log.Printf("[%s] sec repair: GetCharSecurityRating charID=%d: %v", addr, charID, err)
			return noSuchService()
		}
		per01 := evedb.GetSecRepairPer01(ctx, sqldb)
		cost := evedb.SecRepairQuote(sec, per01)
		if cost == 0 {
			return agentConversation(serviceText(ctx, sqldb, agent.DivisionID, "sec.clean", map[string]string{
				"pilot": evedb.GetCharacterName(ctx, sqldb, charID),
			}), []marshal.PyRep{secCancelBtn()}, xtra)
		}
		tokens := map[string]string{
			"sec":  fmt.Sprintf("%.1f", sec),
			"cost": commaGroups(cost),
		}
		if actionID == agentActionSecQuote {
			return agentConversation(serviceText(ctx, sqldb, agent.DivisionID, "sec.quote", tokens),
				[]marshal.PyRep{
					agentButton(serviceText(ctx, sqldb, agent.DivisionID, "sec.pay", tokens), agentActionSecConfirm),
					secCancelBtn(),
				}, xtra)
		}
		// Confirm: the quote is recomputed from the live rating above, so a
		// kill between quote and click reprices rather than undercharges.
		bal, err := evedb.GetCharacterBalance(ctx, sqldb, charID)
		if err != nil || bal < float64(cost) {
			return agentConversation(serviceText(ctx, sqldb, agent.DivisionID, "sec.poor", tokens),
				[]marshal.PyRep{secCancelBtn()}, xtra)
		}
		repaired, err := evedb.RepairSecurityStatus(ctx, sqldb, charID)
		if err != nil || !repaired {
			log.Printf("[%s] sec repair: RepairSecurityStatus charID=%d repaired=%v: %v", addr, charID, repaired, err)
			return agentConversation("The paperwork did not go through. No charge was made.",
				[]marshal.PyRep{secCancelBtn()}, xtra)
		}
		// Invalidate every cache of the row (standing2 memoises it for 60 s). The entity is nil: a docked
		// pilot has no ball. No standing transaction is written, since no EventStanding* fits a repair.
		publishSecStatus(nil, charID, 0)

		if err := evedb.GiveISK(ctx, sqldb, charID, -float64(cost)); err != nil {
			log.Printf("[%s] sec repair: debit charID=%d cost=%d: %v", addr, charID, cost, err)
		}
		if newBal, err := evedb.GetCharacterBalance(ctx, sqldb, charID); err == nil {
			_ = evedb.InsertJournalEntry(ctx, sqldb, charID, refTypeAgentSecurityServices,
				charID, agentID, -float64(cost), newBal, "Security status repair")
			pushAccountChange(c, accountID, charID, newBal)
		}
		log.Printf("[%s] → sec repair: charID=%d paid %d ISK to agent %d (was %.2f, now 0.0)",
			addr, charID, cost, agentID, sec)
		return agentConversation(serviceText(ctx, sqldb, agent.DivisionID, "sec.paid", tokens),
			[]marshal.PyRep{shellBtn(ctx, sqldb, agent, "shell.btn.quit", agentActionQuit)}, xtra)

	case agentActionGiftStub:
		if !services[evedb.ServiceGifts] {
			return noSuchService()
		}
		return agentConversation(serviceText(ctx, sqldb, agent.DivisionID, "gifts.stub", nil),
			[]marshal.PyRep{shellBtn(ctx, sqldb, agent, "shell.btn.quit", agentActionQuit)}, xtra)

	case agentActionRumorStub:
		if !services[evedb.ServiceRumors] {
			return noSuchService()
		}
		return agentConversation(serviceText(ctx, sqldb, agent.DivisionID, "rumors.stub", nil),
			[]marshal.PyRep{shellBtn(ctx, sqldb, agent, "shell.btn.quit", agentActionQuit)}, xtra)
	}
	return noSuchService()
}

// locatorResolveName answers the typed name: not found, self, out of range,
// or a fee quote that moves the pending state to stage 2.
func locatorResolveName(ctx context.Context, sqldb *sql.DB, agent *evedb.AgentInfo,
	charID int64, name string, st *locatePendingState, lp int) marshal.PyRep {

	xtra := agentXtraInfo(agent.AgentID, 0, 0, lp)
	cancel := agentButton(evedb.GetAgentText(ctx, sqldb, agent.DivisionID, "locator.cancel"), agentActionQuit)
	retry := func(key string, tokens map[string]string) marshal.PyRep {
		// Stay on stage 1 with the form re-rendered so another name can be
		// tried without restarting the flow.
		return agentConversation(serviceText(ctx, sqldb, agent.DivisionID, key, tokens)+locateNameForm(ctx, sqldb, agent.DivisionID),
			[]marshal.PyRep{cancel}, xtra)
	}

	name = strings.TrimSpace(name)
	rangeName := evedb.LocatorRangeName(agent.Level)
	targetID, realName, err := evedb.FindPlayerCharByName(ctx, sqldb, name)
	if err != nil || targetID == 0 {
		// The helper wraps sql.ErrNoRows for a miss; either way the agent
		// has no record of that pilot.
		return retry("locator.notfound", map[string]string{"target": name, "range": rangeName})
	}
	if targetID == charID {
		return retry("locator.self", map[string]string{
			"pilot": realName, "target": realName, "range": rangeName,
		})
	}

	if !locatorTargetInRange(ctx, sqldb, agent, targetID) {
		return retry("locator.outofrange", map[string]string{"target": realName, "range": rangeName})
	}

	fee := evedb.LocatorFee(agent.Level, evedb.GetLocatorFeeBase(ctx, sqldb))
	st.stage = 2
	st.targetID = targetID
	st.targetName = realName
	st.fee = fee
	st.expires = time.Now().Add(10 * time.Minute)
	locatePending.Store(charID, st)

	tokens := map[string]string{
		"target": realName,
		"fee":    commaGroups(fee),
		"delay":  fmt.Sprintf("%d", int(locatorMailDelay.Minutes())),
		"range":  rangeName,
	}
	return agentConversation(serviceText(ctx, sqldb, agent.DivisionID, "locator.quote", tokens),
		[]marshal.PyRep{
			agentButton(serviceText(ctx, sqldb, agent.DivisionID, "locator.pay", tokens), agentActionLocateConfirm),
			cancel,
		}, xtra)
}

// locatorTargetLocation resolves where the target is right now: the session
// registry when online, the character record's last docked/seen location
// otherwise.
func locatorTargetLocation(ctx context.Context, sqldb *sql.DB, targetID int64) (stationID, systemID int64) {
	if globalSessReg != nil {
		if s := globalSessReg.FindByCharID(targetID); s != nil {
			return s.StationID, s.SolarSystemID
		}
	}
	stationID, systemID, err := evedb.CharLastKnownLocation(ctx, sqldb, targetID)
	if err != nil {
		log.Printf("locator: CharLastKnownLocation target=%d: %v", targetID, err)
	}
	return stationID, systemID
}

// locatorTargetInRange applies the level range: 1 = the agent's solar
// system, 2 = constellation, 3 = region, 4+ = anywhere.
func locatorTargetInRange(ctx context.Context, sqldb *sql.DB, agent *evedb.AgentInfo, targetID int64) bool {
	if agent.Level >= 4 {
		return true
	}
	_, targetSys := locatorTargetLocation(ctx, sqldb, targetID)
	if targetSys == 0 {
		return false
	}
	if agent.SolarSystemID == targetSys {
		return true
	}
	if agent.Level <= 1 {
		return false
	}
	aCon, aReg, err := evedb.SystemConstellationRegion(ctx, sqldb, agent.SolarSystemID)
	if err != nil {
		return false
	}
	tCon, tReg, err := evedb.SystemConstellationRegion(ctx, sqldb, targetSys)
	if err != nil {
		return false
	}
	if agent.Level == 2 {
		return aCon == tCon
	}
	return aReg == tReg // level 3
}

// pushAccountChange blinks the wallet on the requesting client.
func pushAccountChange(c *evenet.Conn, accountID, charID int64, balance float64) {
	if c == nil {
		return
	}
	_ = server.SendNotification(c, accountID, "OnAccountChange", "charid",
		marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyString{Value: "cash"},
			marshal.PyInt{Value: uint64(charID)},
			marshal.PyReal{Value: balance},
		}})
}

// ── Locator delivery sweep ───────────────────────────────────────────────────

// locatorSweepTicks: the tick base is 1s; the mail delay is minutes, so a
// 15s sweep keeps delivery within a quarter-minute of due without load.
const locatorSweepTicks = 15

// startLocatorSweep delivers due locator jobs as EVE-mail from the agent. Jobs persist in
// agtLocatorJobs, so a paid search survives a restart.
func startLocatorSweep(tickMgr *tick.Manager) {
	tickMgr.RegisterPeriodic("locator-sweep", locatorSweepTicks, func(uint64) bool {
		if globalDB == nil {
			return true
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		jobs, err := evedb.DueLocatorJobs(ctx, globalDB)
		if err != nil {
			log.Printf("[locator-sweep] DueLocatorJobs: %v", err)
			return true
		}
		for _, j := range jobs {
			deliverLocatorJob(ctx, globalDB, j)
		}
		return true
	})
}

// deliverLocatorJob composes and sends the "found them" mail. The target's location is
// resolved at delivery time, not at payment time.
func deliverLocatorJob(ctx context.Context, sqldb *sql.DB, j evedb.LocatorJob) {
	// The mail speaks with the agent's division voice; a deleted agent falls
	// back to the global lines.
	agentDiv := int64(0)
	if a, err := evedb.GetAgentInfo(ctx, sqldb, j.AgentID); err == nil {
		agentDiv = a.DivisionID
	}
	targetName := evedb.GetCharacterName(ctx, sqldb, j.TargetID)
	if targetName == "" {
		targetName = fmt.Sprintf("pilot %d", j.TargetID)
	}
	stationID, systemID := locatorTargetLocation(ctx, sqldb, j.TargetID)

	sysName := ""
	if systemID != 0 {
		sysName = evedb.GetSolarSystemName(ctx, sqldb, systemID)
	}
	sysLink := sysName
	if systemID != 0 && sysName != "" {
		sysLink = systemLink(systemID, sysName)
	}

	var body string
	if stationID != 0 {
		stnName := evedb.GetStationName(ctx, sqldb, stationID)
		stnLink := "<b>" + stnName + "</b>"
		if bits, err := evedb.GetStationItemBits(ctx, sqldb, stationID); err == nil {
			stnLink = fmt.Sprintf(`<a href="showinfo:%d//%d">%s</a>`, bits.StationTypeID, stationID, stnName)
		}
		body = serviceText(ctx, sqldb, agentDiv, "locator.mail.docked", map[string]string{
			"target": targetName, "station": stnLink, "system": sysLink,
		})
	} else {
		if sysLink == "" {
			sysLink = "an unknown system — my sources lost the trail"
		}
		body = serviceText(ctx, sqldb, agentDiv, "locator.mail.inspace", map[string]string{
			"target": targetName, "system": sysLink,
		})
	}

	subject := "Location of " + targetName
	created := int64(timeToFileTime(time.Now()))
	msgID, err := evedb.SendEveMail(ctx, sqldb, j.CharID, j.AgentID, subject, body, created)
	if err != nil {
		log.Printf("[locator-sweep] mail job=%d charID=%d: %v", j.JobID, j.CharID, err)
		return
	}
	if err := evedb.MarkLocatorJobDelivered(ctx, sqldb, j.JobID); err != nil {
		log.Printf("[locator-sweep] mark delivered job=%d: %v", j.JobID, err)
	}
	pushMailNotification(j.CharID, msgID, j.AgentID, subject, created)
	log.Printf("[locator-sweep] job=%d delivered: %s located for charID=%d (station=%d system=%d)",
		j.JobID, targetName, j.CharID, stationID, systemID)
}

// ── Info window sections ─────────────────────────────────────────────────────

// buildAgentServiceSections renders the Agent Info tab's service blocks, the (header,
// [(label, text)...]) sections infosvc.py draws verbatim, plus this character's succeeded and
// failed counts with this agent from characterMissions.
func buildAgentServiceSections(ctx context.Context, sqldb *sql.DB, agent *evedb.AgentInfo, charID int64) []marshal.PyRep {
	row := func(label, text string) marshal.PyRep {
		return marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyString{Value: label},
			marshal.PyString{Value: text},
		}}
	}
	section := func(header string, rows ...marshal.PyRep) marshal.PyRep {
		return marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyString{Value: header},
			marshal.PyList{Items: rows},
		}}
	}

	var out []marshal.PyRep

	if charID != 0 {
		stats, statsErr := evedb.GetAgentMissionStats(ctx, sqldb, charID, agent.AgentID)
		onCD, availableAt, _ := evedb.AgentMissionCooldown(ctx, sqldb, charID, agent.AgentID)
		// The section renders whenever there is history, and always while the cap is hit, since
		// "Available Again" is the line the pilot looks for.
		if (statsErr == nil && (stats.Succeeded > 0 || stats.Failed > 0 || stats.Declined > 0)) || onCD {
			var rows []marshal.PyRep
			if onCD {
				rows = append(rows, row("Available Again", availableAt.Format("2006.01.02 15:04:05")))
			}
			rows = append(rows,
				row("Missions Succeeded", fmt.Sprintf("%d", stats.Succeeded)),
				row("Missions Failed", fmt.Sprintf("%d", stats.Failed)))
			if stats.Declined > 0 {
				rows = append(rows, row("Missions Declined", fmt.Sprintf("%d", stats.Declined)))
			}
			out = append(out, section("Missions", rows...))
		}
	}

	services, err := evedb.AgentServiceSet(ctx, sqldb, agent)
	if err != nil {
		return out
	}

	if services[evedb.ServiceLocator] {
		fee := evedb.LocatorFee(agent.Level, evedb.GetLocatorFeeBase(ctx, sqldb))
		out = append(out, section("Locator Services",
			row("Search Range", evedb.LocatorRangeName(agent.Level)),
			row("Fee", commaGroups(fee)+" ISK"),
			row("Results By", fmt.Sprintf("EVE-mail, within %d minutes", int(locatorMailDelay.Minutes()))),
		))
	}
	if services[evedb.ServiceSecStatus] {
		per01 := evedb.GetSecRepairPer01(ctx, sqldb)
		rows := []marshal.PyRep{
			row("Repairs Up To", "0.0 (a clean record is not improved)"),
			row("Cost", commaGroups(int64(per01))+" ISK per 0.1 below zero"),
		}
		if charID != 0 {
			if sec, err := evedb.GetCharSecurityRating(ctx, sqldb, charID); err == nil && sec < -0.0001 {
				rows = append(rows, row("Your Quote",
					commaGroups(evedb.SecRepairQuote(sec, per01))+" ISK"))
			}
		}
		out = append(out, section("Security Status Services", rows...))
	}
	if services[evedb.ServiceGifts] {
		out = append(out, section("Donations/Gifts/Bribes",
			row(`Accepts "Gifts"`, "Yes")))
	}
	if services[evedb.ServiceRumors] {
		out = append(out, section("Rumour Services",
			row("Provides Site Information", "Yes")))
	}
	return out
}
