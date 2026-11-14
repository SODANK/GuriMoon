package esp

// Skill queue management pages. The 3913 client has no skill-queue UI, so the queue
// (chrSkillQueue, db/skillqueuedb.go) is managed over HTTP: the character page
// (/character/<id>) shows a read-only card, /skillwindow/<id> edits it, and the in-game browser
// gets Genolution (/genolution/, alias /ig/skills).
//
// The eve.charID header is client-supplied text and here selects whose queue is edited, so a
// spoofed header can rearrange another pilot's queue. Nothing else is reachable through it.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
)

// SkillQueueNotify is set by main (an import cycle blocks a direct call). It pushes an
// AdvanceSkillQueue result to a live connection: OnSkillTrained / OnSkillStartTraining and the
// completion timer.
var SkillQueueNotify func(sess *Session, adv *evedb.SkillQueueAdvance)

// SkillInjectNotify is set by main. It tells a live client about a skillbook the ESP injected:
// OnItemChange for the new skill item and OnAccountChange for the wallet.
var SkillInjectNotify func(sess *Session, itemID int64, newBalance float64)

// espTrainingView is the active training entry formatted for templates.
type espTrainingView struct {
	TypeName    string
	TargetLevel int
	EndsAt      string
	Remaining   string
}

// espQueueRow is one queued entry formatted for templates.
type espQueueRow struct {
	Position    int
	TypeID      int64
	TypeName    string
	TargetLevel int
	EstEnd      string
}

func espFmtTick(t uint64) string {
	return evedb.FiletimeToTime(t).Local().Format("2006-01-02 15:04")
}

func espFmtRemaining(t uint64) string {
	d := time.Until(evedb.FiletimeToTime(t))
	if d <= 0 {
		return "due"
	}
	d = d.Round(time.Minute)
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60
	if days > 0 {
		return fmt.Sprintf("%dd %dh %dm", days, hours, mins)
	}
	if hours > 0 {
		return fmt.Sprintf("%dh %dm", hours, mins)
	}
	return fmt.Sprintf("%dm", mins)
}

// skillQueueViewData loads the active training and queued entries for charID,
// formatted for page rendering.
func skillQueueViewData(ctx context.Context, db *sql.DB, charID int64) (*espTrainingView, []espQueueRow) {
	var training *espTrainingView
	if ti, err := evedb.GetSkillTrainingInfo(ctx, db, charID); err == nil && ti != nil {
		training = &espTrainingView{
			TypeName:    ti.TypeName,
			TargetLevel: ti.TargetLevel,
			EndsAt:      espFmtTick(ti.EndOfTraining),
			Remaining:   espFmtRemaining(ti.EndOfTraining),
		}
	}
	entries, err := evedb.GetSkillQueue(ctx, db, charID)
	if err != nil {
		log.Printf("esp: GetSkillQueue charID=%d: %v", charID, err)
		return training, nil
	}
	if err := evedb.EstimateSkillQueueETAs(ctx, db, charID, entries); err != nil {
		log.Printf("esp: EstimateSkillQueueETAs charID=%d: %v", charID, err)
	}
	rows := make([]espQueueRow, 0, len(entries))
	for _, e := range entries {
		row := espQueueRow{
			Position:    e.Position,
			TypeID:      e.TypeID,
			TypeName:    e.TypeName,
			TargetLevel: e.TargetLevel,
		}
		if e.EstEnd > 0 {
			row.EstEnd = espFmtTick(e.EstEnd)
		}
		rows = append(rows, row)
	}
	return training, rows
}

func skillTypeName(ctx context.Context, db *sql.DB, typeID int64) string {
	var name string
	if err := db.QueryRowContext(ctx,
		`SELECT typeName FROM invTypes WHERE typeID = ?`, typeID).Scan(&name); err != nil {
		return "type " + strconv.FormatInt(typeID, 10)
	}
	return name
}

// applySkillQueueAction runs one queue mutation ("add", "remove", "up", "down", "clear",
// "advance") for charID, advances the queue so an idle character with queued skills trains, and
// notifies the live session. Returns a human-readable result.
func applySkillQueueAction(ctx context.Context, db *sql.DB, sessions *SessionRegistry, charID int64, action string, typeID int64, pos int) string {
	var msgs []string
	switch action {
	case "add":
		lvl, err := evedb.AddSkillToQueue(ctx, db, charID, typeID)
		if err != nil {
			return "Error: " + err.Error()
		}
		msgs = append(msgs, fmt.Sprintf("Queued %s to level %d.", skillTypeName(ctx, db, typeID), lvl))
	case "remove":
		if err := evedb.RemoveSkillQueueEntry(ctx, db, charID, pos); err != nil {
			return "Error: " + err.Error()
		}
		msgs = append(msgs, "Queue entry removed.")
	case "up", "down":
		if err := evedb.MoveSkillQueueEntry(ctx, db, charID, pos, action == "up"); err != nil {
			return "Error: " + err.Error()
		}
		msgs = append(msgs, "Queue reordered.")
	case "clear":
		if err := evedb.ClearSkillQueue(ctx, db, charID); err != nil {
			return "Error: " + err.Error()
		}
		msgs = append(msgs, "Queue cleared.")
	case "normalize":
		moved, err := evedb.NormalizeSkillQueue(ctx, db, charID)
		if err != nil {
			return "Error: " + err.Error()
		}
		if moved == 0 {
			msgs = append(msgs, "Queue order already valid.")
		} else {
			msgs = append(msgs, fmt.Sprintf("Queue repaired — %d entries moved into a trainable order.", moved))
		}
	case "skip":
		// Abort current training (banking partial SP) and advance to the next queued entry.
		if stopped, err := evedb.StopTrainingSkill(ctx, db, charID, 0); err != nil {
			return "Error stopping training: " + err.Error()
		} else if stopped != 0 {
			msgs = append(msgs, "Training aborted (SP banked).")
		}
	case "advance":
		// No mutation; run the advance below.
	default:
		return "Unknown queue action."
	}

	adv, err := evedb.AdvanceSkillQueue(ctx, db, charID)
	if err != nil {
		msgs = append(msgs, "Advance error: "+err.Error())
		return strings.Join(msgs, " ")
	}
	if n := len(adv.Completed); n > 0 {
		msgs = append(msgs, fmt.Sprintf("%d finished training(s) completed.", n))
	}
	if adv.StartedItemID != 0 {
		msgs = append(msgs, "Training started, ends "+espFmtTick(adv.StartedEnd)+".")
	}
	if action == "advance" && len(adv.Completed) == 0 && adv.StartedItemID == 0 {
		msgs = append(msgs, "Nothing due; queue unchanged.")
	}
	if SkillQueueNotify != nil && sessions != nil &&
		(len(adv.Completed) > 0 || adv.StartedItemID != 0) {
		if sess := sessions.FindByCharID(charID); sess != nil {
			SkillQueueNotify(sess, adv)
		}
	}
	return strings.Join(msgs, " ")
}

// applySkillPurchase buys skillbook typeID for charID and injects it at level 0, paying
// Genolution Biotech. Every refusal comes back from the DB layer as an error string with nothing
// charged.
func applySkillPurchase(ctx context.Context, db *sql.DB, sessions *SessionRegistry, charID, typeID int64) string {
	p, err := evedb.PurchaseAndInjectSkill(ctx, db, charID, typeID)
	if err != nil {
		return "Error: " + err.Error()
	}
	if SkillInjectNotify != nil && sessions != nil {
		if sess := sessions.FindByCharID(charID); sess != nil {
			SkillInjectNotify(sess, p.ItemID, p.NewBalance)
		}
	}
	return fmt.Sprintf("Injected %s for %s ISK (market average %s, from %s) — paid to Genolution Biotech. Wallet now %s ISK.",
		p.TypeName, formatISK(p.Price), formatISK(p.Average), p.Source, formatISK(p.NewBalance))
}

// applySkillGrant injects a skill (or every skill) at a fixed level with no ISK and no training
// time (the GM counterpart to the shop). There is no live push: godma rebuilds its skill list on
// login, so the pilot must relog.
func applySkillGrant(ctx context.Context, db *sql.DB, charID int64, action, typeIDStr, levelStr string) string {
	if action == "grantall" {
		n, err := evedb.GiveAllSkills(ctx, db, charID)
		if err != nil {
			return "Error: " + err.Error()
		}
		return fmt.Sprintf("Granted %d skills at level V. Relog required.", n)
	}
	typeID, err := strconv.ParseInt(typeIDStr, 10, 64)
	if err != nil || typeID <= 0 {
		return "Pick a skill to grant."
	}
	level, err := strconv.Atoi(levelStr)
	if err != nil || level < 1 || level > 5 {
		return "Level must be 1-5 — a level 0 injection is what the Buy button does."
	}
	typeName, err := evedb.GiveSkill(ctx, db, charID, typeID, level)
	if err != nil {
		return "Error: " + err.Error()
	}
	return fmt.Sprintf("Granted %s %s. Relog required.", typeName, skillRoman[level])
}

// applyMinMaxQueue writes a fastest-first plan over the injected skills into the queue (every
// trainable level 1, then the level 2s, and so on). replace wipes the queue first, otherwise the
// plan is appended. maxStepHours drops any level slower than the cap (0 = keep all).
func applyMinMaxQueue(ctx context.Context, db *sql.DB, sessions *SessionRegistry, charID int64,
	maxLevel, maxSteps int, maxStepHours float64, replace bool) string {

	if replace {
		if err := evedb.ClearSkillQueue(ctx, db, charID); err != nil {
			return "Error: " + err.Error()
		}
	}
	baseline, err := evedb.ProjectedSkillLevels(ctx, db, charID)
	if err != nil {
		return "Error: " + err.Error()
	}
	plan, err := evedb.PlanMinMaxSkillQueue(ctx, db, charID, baseline, maxLevel, maxSteps, maxStepHours*60)
	if err != nil {
		return "Error: " + err.Error()
	}
	if len(plan) == 0 {
		return "Nothing to queue — no injected skill has a trainable level left under those limits."
	}
	added, err := evedb.AppendSkillQueuePlan(ctx, db, charID, plan)
	if err != nil {
		return "Error: " + err.Error()
	}
	var total float64
	for _, s := range plan {
		total += s.Minutes
	}
	verb := "Appended"
	if replace {
		verb = "Queued"
	}
	msg := fmt.Sprintf("%s %d min-max steps (through level %d, %s of training).",
		verb, added, maxLevel, espFmtMinutes(total))
	adv := applySkillQueueAction(ctx, db, sessions, charID, "advance", 0, 0)
	if adv != "" && adv != "Nothing due; queue unchanged." {
		msg += " " + adv
	}
	return msg
}

// ---- Skill window: /skillwindow/<charID> ----
// Category tiles, per-group skill lists with level pips and time-to-next-level, and a
// drag-and-drop training queue.

var skillRoman = [6]string{"", "I", "II", "III", "IV", "V"}

// skillWinSkill is one catalogue row formatted for the skill window.
type skillWinSkill struct {
	TypeID      int64
	Name        string
	Description string // invTypes.description, shown as tooltip on hover
	Rank        int64
	Known       bool
	Level       int
	SP          int64
	NextLevel   int     // level the next queue add would train (0 = maxed/unknown)
	NextRoman   string  // roman numeral of NextLevel
	TimeToNext  string  // estimated training time for NextLevel
	Pips        [5]bool // level pips: Pips[i] filled when Level > i
	Blocked     string  // unmet prerequisites ("Navigation IV, ..."); "" = trainable

	// Shop fields, only meaningful on a skill the character has not injected.
	Price      string // formatted shop price ("" = not for sale)
	PriceHint  string // tooltip: market average and where it came from
	CanBuy     bool   // prerequisites met and a price exists
	TooPoor    bool   // CanBuy, but the wallet cannot cover it
	BuyBlocked string // unmet prerequisites for injection ("" = none)
	NoMarket   bool   // no sell orders, no trades, no base price
}

// skillWinGroup is one skill group (category tile + its skill list).
type skillWinGroup struct {
	GroupID int64
	Name    string
	Known   int // injected skills in this group
	Total   int
	Skills  []skillWinSkill
}

// skillWinQueueRow is one queue entry for the skill window.
type skillWinQueueRow struct {
	Position    int
	TypeID      int64
	Name        string
	TargetLevel int
	Roman       string
	EstEnd      string
	Remaining   string
}

func espFmtMinutes(mins float64) string {
	if mins < 1 {
		return "< 1m"
	}
	d := time.Duration(mins * float64(time.Minute)).Round(time.Minute)
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	m := int(d.Minutes()) % 60
	if days > 0 {
		return fmt.Sprintf("%dd %dh %dm", days, hours, m)
	}
	if hours > 0 {
		return fmt.Sprintf("%dh %dm", hours, m)
	}
	return fmt.Sprintf("%dm", m)
}

// skillNextTraining returns the level the next queue add would train for a catalogue row and its
// estimated time, honoring the active training and queued levels (AddSkillToQueue's floor logic).
// Level 0 means maxed or not injected.
func skillNextTraining(row evedb.ESPSkillCatalogueRow, stats map[int64]float64,
	training *evedb.SkillTrainingInfo, maxQueued map[int64]int) (int, string) {
	if !row.Known {
		return 0, ""
	}
	floor := row.Level
	if training != nil && training.TypeID == row.TypeID && training.TargetLevel > floor {
		floor = training.TargetLevel
	}
	if q := maxQueued[row.TypeID]; q > floor {
		floor = q
	}
	if floor >= 5 {
		return 0, ""
	}
	next := floor + 1
	spStart := float64(row.SkillPoints)
	if prev := evedb.ESPSkillSPForLevel(row.Rank, next-1); prev > spStart {
		spStart = prev // queued/training intermediate levels assumed done
	}
	spNeeded := evedb.ESPSkillSPForLevel(row.Rank, next) - spStart
	if spNeeded < 1 {
		spNeeded = 1
	}
	return next, espFmtMinutes(spNeeded / evedb.ESPSkillSPPerMinute(stats, row.PrimaryAttrID, row.SecondaryAttrID))
}

// handleSkillWindow serves GET /skillwindow/<charID> and its POST mutations
// (add/remove/clear/advance via applySkillQueueAction, plus "reorder" with a
// comma-separated permutation of queue positions from the drag-and-drop UI).
// POSTs redirect back to the GET (303) so refresh never repeats an action.
func (s *Server) handleSkillWindow(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 2 {
		http.NotFound(w, r)
		return
	}
	charID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || charID <= 0 {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	self := fmt.Sprintf("/skillwindow/%d", charID)

	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		action := r.FormValue("action")
		var msg string
		switch action {
		case "add", "remove", "clear", "advance", "normalize", "skip":
			typeID, _ := strconv.ParseInt(r.FormValue("typeID"), 10, 64)
			pos, _ := strconv.Atoi(r.FormValue("pos"))
			msg = applySkillQueueAction(ctx, s.db, s.sessions, charID, action, typeID, pos)
		case "buy":
			typeID, _ := strconv.ParseInt(r.FormValue("typeID"), 10, 64)
			msg = applySkillPurchase(ctx, s.db, s.sessions, charID, typeID)
		case "grant", "grantall":
			// GM grant: skills are granted here, next to the catalogue they act on.
			msg = applySkillGrant(ctx, s.db, charID, r.FormValue("action"),
				r.FormValue("typeID"), r.FormValue("level"))
		case "minmax":
			maxLevel, err := strconv.Atoi(r.FormValue("maxLevel"))
			if err != nil || maxLevel < 1 || maxLevel > 5 {
				maxLevel = 5
			}
			maxSteps, err := strconv.Atoi(r.FormValue("maxSteps"))
			if err != nil || maxSteps < 0 {
				maxSteps = 0
			}
			stepHours, err := strconv.ParseFloat(r.FormValue("stepHours"), 64)
			if err != nil || stepHours < 0 {
				stepHours = 0
			}
			msg = applyMinMaxQueue(ctx, s.db, s.sessions, charID,
				maxLevel, maxSteps, stepHours, r.FormValue("replace") == "1")
		case "reorder":
			var order []int
			for _, part := range strings.Split(r.FormValue("order"), ",") {
				p, err := strconv.Atoi(strings.TrimSpace(part))
				if err != nil {
					msg = "Bad reorder list."
					break
				}
				order = append(order, p)
			}
			if msg == "" {
				if err := evedb.ReorderSkillQueue(ctx, s.db, charID, order); err != nil {
					msg = "Error: " + err.Error()
				} else {
					msg = applySkillQueueAction(ctx, s.db, s.sessions, charID, "advance", 0, 0)
					if msg == "Nothing due; queue unchanged." {
						msg = "Queue reordered."
					}
				}
			}
		default:
			msg = "Unknown action."
		}
		if r.Header.Get("X-Requested-With") == "XMLHttpRequest" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"msg": msg})
			return
		}
		http.Redirect(w, r, self+"?msg="+url.QueryEscape(msg), http.StatusSeeOther)
		return
	}

	// Lazy-advance so a due training completes on page load.
	if adv, err := evedb.AdvanceSkillQueue(ctx, s.db, charID); err == nil && adv != nil &&
		(len(adv.Completed) > 0 || adv.StartedItemID != 0) && SkillQueueNotify != nil {
		if sess := s.sessions.FindByCharID(charID); sess != nil {
			SkillQueueNotify(sess, adv)
		}
	}

	detail, err := evedb.ESPGetCharacterDetail(ctx, s.db, charID)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "characters"})
		return
	}

	catalogue, err := evedb.ESPGetSkillCatalogue(ctx, s.db, charID)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "characters"})
		return
	}
	stats := evedb.ESPEffectiveLearningStats(ctx, s.db, charID)

	// Queued target levels per type raise the "next add" level shown per skill,
	// matching AddSkillToQueue's floor logic.
	queueEntries, err := evedb.GetSkillQueue(ctx, s.db, charID)
	if err != nil {
		log.Printf("esp: skillwindow GetSkillQueue charID=%d: %v", charID, err)
	}
	_ = evedb.EstimateSkillQueueETAs(ctx, s.db, charID, queueEntries)
	maxQueued := make(map[int64]int)
	for _, e := range queueEntries {
		if e.TargetLevel > maxQueued[e.TypeID] {
			maxQueued[e.TypeID] = e.TargetLevel
		}
	}
	training, _ := evedb.GetSkillTrainingInfo(ctx, s.db, charID)

	// A skill whose required skills the queue never satisfies cannot be queued, so the page shows
	// why instead of offering a "+". Projected levels count: Navigation V behind a queued IV is fine.
	prereqs, perr := evedb.GetSkillPrereqMap(ctx, s.db)
	if perr != nil {
		log.Printf("esp: skillwindow GetSkillPrereqMap: %v", perr)
	}
	// Trained levels are separate from projected ones: buying a skillbook is gated on what is
	// injected, queueing a level on what the queue will have delivered.
	trained := make(map[int64]int)
	projected := make(map[int64]int)
	for _, row := range catalogue {
		if row.Known {
			trained[row.TypeID] = row.Level
			projected[row.TypeID] = row.Level
		}
	}
	if training != nil && training.TargetLevel > projected[training.TypeID] {
		projected[training.TypeID] = training.TargetLevel
	}
	for typeID, lvl := range maxQueued {
		if lvl > projected[typeID] {
			projected[typeID] = lvl
		}
	}
	typeNames := make(map[int64]string, len(catalogue))
	for _, row := range catalogue {
		typeNames[row.TypeID] = row.TypeName
	}
	blockedText := func(typeID int64) string {
		missing := evedb.UnmetSkillReqs(prereqs[typeID], projected, nil)
		if len(missing) == 0 {
			return ""
		}
		parts := make([]string, 0, len(missing))
		for _, r := range missing {
			name := typeNames[r.SkillID]
			if name == "" {
				name = fmt.Sprintf("type %d", r.SkillID)
			}
			parts = append(parts, name+" "+skillRoman[min(max(r.Level, 1), 5)])
		}
		return strings.Join(parts, ", ")
	}

	// Shop prices for the whole catalogue in one pass, since the window lists every published skill.
	prices, perr := evedb.SkillMarketPrices(ctx, s.db)
	if perr != nil {
		log.Printf("esp: skillwindow SkillMarketPrices: %v", perr)
	}

	var groups []*skillWinGroup
	groupIdx := make(map[int64]*skillWinGroup)
	var totalSP, knownCount int64
	for _, row := range catalogue {
		g := groupIdx[row.GroupID]
		if g == nil {
			g = &skillWinGroup{GroupID: row.GroupID, Name: row.GroupName}
			groupIdx[row.GroupID] = g
			groups = append(groups, g)
		}
		sk := skillWinSkill{
			TypeID: row.TypeID, Name: row.TypeName, Description: row.Description,
			Rank: row.Rank, Known: row.Known, Level: row.Level, SP: row.SkillPoints,
		}
		for i := 0; i < 5 && i < row.Level; i++ {
			sk.Pips[i] = true
		}
		g.Total++
		if row.Known {
			g.Known++
			knownCount++
			totalSP += row.SkillPoints
			if next, eta := skillNextTraining(row, stats, training, maxQueued); next > 0 {
				sk.NextLevel = next
				sk.NextRoman = skillRoman[next]
				sk.TimeToNext = eta
				sk.Blocked = blockedText(row.TypeID)
			}
		} else {
			sk.BuyBlocked = evedb.UnmetInjectReqs(prereqs[row.TypeID], trained, typeNames)
			q := prices[row.TypeID]
			switch {
			case sk.BuyBlocked != "":
				// Unbuyable regardless of price: say why, not how much.
			case q.Price <= 0:
				sk.NoMarket = true
			default:
				sk.Price = formatISK(q.Price)
				sk.PriceHint = fmt.Sprintf("market average %s ISK from %s, +%.0f%% markup",
					formatISK(q.Average), q.Source, (evedb.SkillPriceMarkup-1)*100)
				sk.CanBuy = true
				sk.TooPoor = detail.Balance < q.Price
			}
		}
		g.Skills = append(g.Skills, sk)
	}

	var trainingView *espTrainingView
	if training != nil {
		trainingView = &espTrainingView{
			TypeName:    training.TypeName,
			TargetLevel: training.TargetLevel,
			EndsAt:      espFmtTick(training.EndOfTraining),
			Remaining:   espFmtRemaining(training.EndOfTraining),
		}
	}
	queueRows := make([]skillWinQueueRow, 0, len(queueEntries))
	var queueTotal string
	for _, e := range queueEntries {
		row := skillWinQueueRow{
			Position: e.Position, TypeID: e.TypeID, Name: e.TypeName,
			TargetLevel: e.TargetLevel, Roman: skillRoman[min(e.TargetLevel, 5)],
		}
		if e.EstEnd > 0 {
			row.EstEnd = espFmtTick(e.EstEnd)
			row.Remaining = espFmtRemaining(e.EstEnd)
			queueTotal = row.Remaining // last entry's remaining = whole queue
		}
		queueRows = append(queueRows, row)
	}

	s.render(w, "skills.html", map[string]any{
		"Char":       detail,
		"Groups":     groups,
		"Training":   trainingView,
		"TrainRoman": trainingRoman(trainingView),
		"Queue":      queueRows,
		"QueueTotal": queueTotal,
		"TotalSP":    commaInt(totalSP),
		"KnownCount": knownCount,
		"Msg":        r.URL.Query().Get("msg"),
		"Self":       self,
		"Page":       "characters",
	})
}

func trainingRoman(t *espTrainingView) string {
	if t == nil || t.TargetLevel < 1 || t.TargetLevel > 5 {
		return ""
	}
	return skillRoman[t.TargetLevel]
}
