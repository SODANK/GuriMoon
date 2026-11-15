package main

// Wars: the wire behind the Wars tab, the war-target overview colouring, and the CONCORD
// exemption that makes a war shot legal.
//
// Routing: the warRegistry bound object (GetWars, GetCostOfStartingAWarAgainst; ownerID is a
// call argument); the alliance bound object (DeclareWarAgainst, RetractWar, ChangeMutualWarFlag
// for an allied pilot); corpRegistry (RetractWar, ChangeMutualWarFlag for a lone corp, which
// declares only through a voteWar vote). See docs/corp-notes.md#wars.

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sync"
	"time"

	evedb "gurimoon/db"
	"gurimoon/esp"
	"gurimoon/logx"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
	"gurimoon/server/infocache"
	"gurimoon/syncx"
)

// corpWarMethods are the war calls that may arrive on the corpRegistry bound
// object.  The alliance bind check runs first in handleBoundCall, so these
// only fire for a corp acting alone.
var corpWarMethods = map[string]bool{
	"RetractWar": true, "ChangeMutualWarFlag": true, "DeclareWarAgainst": true,
}

// ── Reads ────────────────────────────────────────────────────────────────────

// handleGetWars answers warRegistry.GetWars(ownerID) with a util.IndexRowset: a plain dict has
// no .header, and the war service reads len(rowset.header) when a push arrives.
func handleGetWars(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var ownerID int64
	if len(cs.ArgTuple.Items) >= 1 {
		if v, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
			ownerID = int64(v.Value)
		}
	}
	rowset, err := cachedWarsRowset(ctx, sqldb, ownerID)
	if err != nil {
		log.Printf("[%s] warRegistry.GetWars(%d): %v", addr, ownerID, err)
		rowset, _ = evedb.GetWarsRowset(ctx, sqldb, 0) // empty but correctly shaped
	}
	logx.Debugf("[%s] → warRegistry.GetWars ownerID=%d", addr, ownerID)
	return server.SendCallRsp(c, pkt, accountID, rowset)
}

// warsMemoMethod keys GetWars replies in infocache.Shared.
const warsMemoMethod = "warRegistry.GetWars"

// cachedWarsRowset is GetWarsRowset through the shared reply memo. The war service takes no
// lock on a miss, so an undock fires one GetWars per ball on grid; single-flight turns them into
// one query. pushWarChanged drops both owners' entries; the TTL covers the ESP paths that
// finish wars without a push.
func cachedWarsRowset(ctx context.Context, sqldb *sql.DB, ownerID int64) (marshal.PyRep, error) {
	return infocache.Shared.Get(warsMemoMethod, ownerID, infocache.TTLIdentity, func() (marshal.PyRep, error) {
		return evedb.GetWarsRowset(ctx, sqldb, ownerID)
	})
}

// handleGetWarCost answers GetCostOfStartingAWarAgainst(ownerID). Always a number: util.FmtISK
// evaluates math.fmod(cost, 1.0) before its None guard, so None kills both Declare War dialogs.
func handleGetWarCost(c *evenet.Conn, addr string, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cost := evedb.WarDeclarationCost(ctx, sqldb)
	log.Printf("[%s] → warRegistry.GetCostOfStartingAWarAgainst = %.0f", addr, cost)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyReal{Value: cost})
}

// ── The declaration itself ───────────────────────────────────────────────────

// declareWar runs the whole declaration: validity, the CONCORD fee, the row, the OnWarChanged
// ADD and the CONCORD letters. declarerOwnerID is the corp or alliance; feeCorpID is whose
// wallet pays; backdate skips the 24h arming delay (GM only). Refusals are evedb.MsgErr values.
func declareWar(ctx context.Context, sqldb *sql.DB, sessReg *esp.SessionRegistry,
	declarerOwnerID, feeCorpID, againstID int64, backdate bool) (*evedb.War, error) {

	if declarerOwnerID == againstID {
		return nil, fmt.Errorf("you cannot declare war on yourself")
	}
	// A corporation inside an alliance does not go to war as itself; every declaring path shares
	// this gate. See docs/corp-notes.md#wars.
	if kind, err := evedb.ClassifyWarOwner(ctx, sqldb, declarerOwnerID); err != nil {
		return nil, err
	} else if kind == evedb.WarOwnerCorp {
		var declarerAlliance int64
		if err := sqldb.QueryRowContext(ctx,
			`SELECT allianceID FROM corporation WHERE corporationID = ?`, declarerOwnerID,
		).Scan(&declarerAlliance); err == nil && declarerAlliance > 0 {
			return nil, evedb.MsgErr("CrpAccessDenied", map[string]marshal.PyRep{
				"reason": marshal.PyString{Value: "your corporation is in an alliance, so wars are declared by the directors of its executor corporation"},
			}, "the corporation is in an alliance — wars are declared by the alliance")
		}
	}
	targetKind, err := evedb.ClassifyWarOwner(ctx, sqldb, againstID)
	if err != nil {
		return nil, err
	}
	if targetKind == evedb.WarOwnerNone {
		return nil, evedb.MsgErr("CrpCantDeclareWarOnNPCCorp", nil,
			"war can only be declared against a player corporation or alliance")
	}
	// A corp inside an alliance is not a valid target on its own; the war belongs at the alliance level.
	if targetKind == evedb.WarOwnerCorp {
		var targetAlliance int64
		if err := sqldb.QueryRowContext(ctx,
			`SELECT allianceID FROM corporation WHERE corporationID = ?`, againstID,
		).Scan(&targetAlliance); err == nil && targetAlliance > 0 {
			return nil, evedb.MsgErr("CrpCanNotSanctionWarAgainstOwner", nil,
				"that corporation is in an alliance — declare war on the alliance instead")
		}
	}
	if existing, err := evedb.FindActiveWar(ctx, sqldb, declarerOwnerID, againstID); err != nil {
		return nil, err
	} else if existing != nil {
		return nil, evedb.MsgErr("AlreadyAtWarWithThem", nil, "you are already at war with them")
	}
	if n, err := evedb.CountActiveDeclaredWars(ctx, sqldb, declarerOwnerID); err != nil {
		return nil, err
	} else if max := evedb.WarMaxDeclarations(ctx, sqldb); n >= max {
		return nil, evedb.MsgErr("CrpTooManyWarDeclarations", nil,
			fmt.Sprintf("you already have %d war declarations outstanding", n))
	}

	// The fee is paid to CONCORD before anything is written: debit the payer, credit CONCORD, one
	// refType-49 journal row in each wallet.
	cost := evedb.WarDeclarationCost(ctx, sqldb)
	if err := payWarFee(ctx, sqldb, feeCorpID, cost,
		fmt.Sprintf("War declaration against %s", ownerDisplayName(ctx, sqldb, againstID))); err != nil {
		return nil, err
	}

	war, err := evedb.InsertWar(ctx, sqldb, declarerOwnerID, againstID, backdate)
	if err != nil {
		// The fee is already gone: refund it on a transient DB error.
		_ = refundWarFee(ctx, sqldb, feeCorpID, cost)
		return nil, err
	}

	pushWarChanged(ctx, sqldb, sessReg, *war, evedb.WarChangeReps(*war, true, false))
	sendWarDeclarationMail(ctx, sqldb, *war, backdate)
	log.Printf("war %d declared: %d → %d (fee %.0f from corp %d to CONCORD)",
		war.WarID, declarerOwnerID, againstID, cost, feeCorpID)
	return war, nil
}

// payWarFee moves the declaration fee from the paying corporation to CONCORD.
func payWarFee(ctx context.Context, sqldb *sql.DB, corpID int64, amount float64, reason string) error {
	balance, err := evedb.GetCorpBalance(ctx, sqldb, corpID)
	if err != nil {
		return err
	}
	if balance < amount {
		return fmt.Errorf("your corporation needs %.0f ISK to declare this war and has %.2f", amount, balance)
	}
	if err := evedb.CreditCorpWallet(ctx, sqldb, corpID, -amount); err != nil {
		return err
	}
	if err := evedb.CreditCorpWallet(ctx, sqldb, evedb.OwnerCONCORDBills, amount); err != nil {
		return err
	}
	newBalance := balance - amount
	concordBalance, _ := evedb.GetCorpBalance(ctx, sqldb, evedb.OwnerCONCORDBills)
	_ = evedb.InsertJournalEntry(ctx, sqldb, corpID, evedb.RefWarFee,
		corpID, evedb.OwnerCONCORDBills, -amount, newBalance, reason)
	_ = evedb.InsertJournalEntry(ctx, sqldb, evedb.OwnerCONCORDBills, evedb.RefWarFee,
		corpID, evedb.OwnerCONCORDBills, amount, concordBalance, reason)
	return nil
}

func refundWarFee(ctx context.Context, sqldb *sql.DB, corpID int64, amount float64) error {
	if err := evedb.CreditCorpWallet(ctx, sqldb, corpID, amount); err != nil {
		return err
	}
	_ = evedb.CreditCorpWallet(ctx, sqldb, evedb.OwnerCONCORDBills, -amount)
	balance, _ := evedb.GetCorpBalance(ctx, sqldb, corpID)
	return evedb.InsertJournalEntry(ctx, sqldb, corpID, evedb.RefWarFee,
		evedb.OwnerCONCORDBills, corpID, amount, balance, "War declaration fee refund")
}

// ── Pushes ───────────────────────────────────────────────────────────────────

// notifyWarOwner pushes an event to every online pilot of a war party: corpID direct,
// allianceID fanned out per member corp.
func notifyWarOwner(ctx context.Context, sqldb *sql.DB, sessReg *esp.SessionRegistry, ownerID int64, eventType string, args marshal.PyRep) {
	if sessReg == nil || ownerID == 0 {
		return
	}
	kind, err := evedb.ClassifyWarOwner(ctx, sqldb, ownerID)
	if err != nil {
		return
	}
	switch kind {
	case evedb.WarOwnerAlliance:
		notifyAlliance(ctx, sqldb, sessReg, ownerID, eventType, args)
	case evedb.WarOwnerCorp:
		notifyCorp(sessReg, ownerID, eventType, args)
	}
}

// pushWarChanged sends OnWarChanged(warID, declaredByID, againstID, change) to both sides.
// changes maps column to (old, new). The change dict is shaped for the strict corp-UI reader;
// see docs/corp-notes.md#change-dicts.
func pushWarChanged(ctx context.Context, sqldb *sql.DB, sessReg *esp.SessionRegistry,
	w evedb.War, changes map[string][2]marshal.PyRep) {

	// Every in-game war write funnels through here, after its commit.
	infocache.Shared.InvalidateID(w.DeclaredByID)
	infocache.Shared.InvalidateID(w.AgainstID)

	entries := make([]marshal.DictEntry, 0, len(changes))
	for _, col := range evedb.WarColumns() {
		pair, ok := changes[col]
		if !ok {
			continue
		}
		entries = append(entries, chg(col, pair[0], pair[1]))
	}
	args := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyInt{Value: uint64(w.WarID)},
		marshal.PyInt{Value: uint64(w.DeclaredByID)},
		marshal.PyInt{Value: uint64(w.AgainstID)},
		marshal.PyDict{Items: entries},
	}}
	notifyWarOwner(ctx, sqldb, sessReg, w.DeclaredByID, "OnWarChanged", args)
	notifyWarOwner(ctx, sqldb, sessReg, w.AgainstID, "OnWarChanged", args)
}

// warUpdateChange builds an UPDATE change dict: the anchor columns travel as (value, value)
// because the strict reader raises on a dict whose every entry is (None, None), plus the
// changed columns.
func warUpdateChange(w evedb.War, changed map[string][2]marshal.PyRep) map[string][2]marshal.PyRep {
	out := map[string][2]marshal.PyRep{
		"warID": {marshal.PyInt{Value: uint64(w.WarID)}, marshal.PyInt{Value: uint64(w.WarID)}},
	}
	for col, pair := range changed {
		out[col] = pair
	}
	return out
}

// ── Mail ─────────────────────────────────────────────────────────────────────

// ownerDisplayName resolves a war party's name for mail bodies.
func ownerDisplayName(ctx context.Context, sqldb *sql.DB, ownerID int64) string {
	var name string
	if err := sqldb.QueryRowContext(ctx,
		`SELECT COALESCE(
		   (SELECT allianceName FROM alliances WHERE allianceID = ?),
		   (SELECT corporationName FROM corporation WHERE corporationID = ?),
		   '')`, ownerID, ownerID).Scan(&name); err == nil && name != "" {
		return name
	}
	return fmt.Sprintf("owner %d", ownerID)
}

// warOwnerPilots lists every player character of a war party: the corp's pilots, or every
// pilot of every member corp of the alliance.
func warOwnerPilots(ctx context.Context, sqldb *sql.DB, ownerID int64) []int64 {
	rows, err := sqldb.QueryContext(ctx, `
		SELECT ch.characterID
		  FROM character_ ch
		  JOIN corporation c ON c.corporationID = ch.corporationID
		 WHERE ch.accountID > 0
		   AND (c.corporationID = ? OR c.allianceID = ?)`, ownerID, ownerID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err == nil {
			out = append(out, id)
		}
	}
	return out
}

// sendWarMailToBothSides mails every member of both war parties as CONCORD (1000125), which
// is a resolvable owner.
func sendWarMailToBothSides(ctx context.Context, sqldb *sql.DB, w evedb.War, subject, body string) {
	created := int64(server.Win32TimeNow())
	seen := make(map[int64]bool)
	for _, ownerID := range []int64{w.DeclaredByID, w.AgainstID} {
		for _, charID := range warOwnerPilots(ctx, sqldb, ownerID) {
			if seen[charID] {
				continue
			}
			seen[charID] = true
			msgID, err := evedb.SendEveMail(ctx, sqldb, charID, evedb.OwnerCONCORD, subject, body, created)
			if err != nil {
				log.Printf("war mail to %d: %v", charID, err)
				continue
			}
			pushMailNotification(charID, msgID, evedb.OwnerCONCORD, subject, created)
		}
	}
}

func sendWarDeclarationMail(ctx context.Context, sqldb *sql.DB, w evedb.War, immediate bool) {
	declarer := ownerDisplayName(ctx, sqldb, w.DeclaredByID)
	against := ownerDisplayName(ctx, sqldb, w.AgainstID)
	when := "in 24 hours, as provided by CONCORD regulations"
	if immediate {
		when = "immediately, by special CONCORD dispensation"
	}
	subject := "War Declared"
	body := fmt.Sprintf(
		"This is an automatic message from CONCORD.\n\n"+
			"%s has declared war on %s.  The war declaration fee has been paid and "+
			"registered with CONCORD.  Hostilities become lawful %s, after which "+
			"members of either party may engage the other anywhere without CONCORD "+
			"intervention.\n\n"+
			"The war will remain lawful for as long as the aggressor continues to pay "+
			"CONCORD's weekly war fee.",
		declarer, against, when)
	sendWarMailToBothSides(ctx, sqldb, w, subject, body)
}

func sendWarEndedMail(ctx context.Context, sqldb *sql.DB, w evedb.War, reason string) {
	declarer := ownerDisplayName(ctx, sqldb, w.DeclaredByID)
	against := ownerDisplayName(ctx, sqldb, w.AgainstID)
	subject := "War Concluded"
	body := fmt.Sprintf(
		"This is an automatic message from CONCORD.\n\n"+
			"The war of %s against %s has concluded: %s.  Hostilities between the "+
			"parties are no longer sanctioned, and CONCORD protection resumes under "+
			"standard empire law.",
		declarer, against, reason)
	sendWarMailToBothSides(ctx, sqldb, w, subject, body)
}

// ── Permission helpers ───────────────────────────────────────────────────────

// warPartyInCharge reports whether the caller may act for a war party: director/CEO of the
// corp itself, or of the alliance's executor corp (the client's IsInCharge, corp_ui_wars.py).
func warPartyInCharge(ctx context.Context, sqldb *sql.DB, ownerID, charID int64) bool {
	kind, err := evedb.ClassifyWarOwner(ctx, sqldb, ownerID)
	if err != nil {
		return false
	}
	switch kind {
	case evedb.WarOwnerCorp:
		corpID, _ := allianceCallerCorp(ctx, sqldb, charID)
		return corpID == ownerID && isDirectorOrCEO(ctx, sqldb, ownerID, charID)
	case evedb.WarOwnerAlliance:
		return isAllianceExecutorDirector(ctx, sqldb, ownerID, charID)
	}
	return false
}

// ── Retract / mutual (shared by corp and alliance paths) ─────────────────────

// retractWarFor retracts a war on behalf of ownerID. The wire argument is a warID (the client
// names the parameter againstID). Either party may retract: the declarer calls it a retraction,
// the defender a surrender, and the state change is the same.
func retractWarFor(ctx context.Context, sqldb *sql.DB, sessReg *esp.SessionRegistry, ownerID, warID int64) error {
	w, err := evedb.GetWar(ctx, sqldb, warID)
	if err != nil {
		return err
	}
	if w == nil {
		return fmt.Errorf("no such war") // NoSuchWar
	}
	if w.DeclaredByID != ownerID && w.AgainstID != ownerID {
		return fmt.Errorf("your side is not a party to that war")
	}
	if err := evedb.RetractWar(ctx, sqldb, warID); err != nil {
		return err
	}
	now := server.Win32TimeNow()
	updated := *w
	updated.Retracted = sql.NullInt64{Int64: int64(now), Valid: true}
	pushWarChanged(ctx, sqldb, sessReg, updated, warUpdateChange(updated, map[string][2]marshal.PyRep{
		"retracted": {marshal.PyNone{}, marshal.PyInt{Value: now}},
	}))
	log.Printf("war %d retracted by owner %d (cools off in 24h)", warID, ownerID)
	return nil
}

// setWarMutualFor flips the mutual flag on behalf of ownerID.
func setWarMutualFor(ctx context.Context, sqldb *sql.DB, sessReg *esp.SessionRegistry, ownerID, warID int64, mutual bool) error {
	w, err := evedb.GetWar(ctx, sqldb, warID)
	if err != nil {
		return err
	}
	if w == nil {
		return fmt.Errorf("no such war")
	}
	if w.DeclaredByID != ownerID && w.AgainstID != ownerID {
		return fmt.Errorf("your side is not a party to that war")
	}
	if err := evedb.SetWarMutual(ctx, sqldb, warID, mutual); err != nil {
		return err
	}
	oldM, newM := uint64(w.Mutual), uint64(0)
	if mutual {
		newM = 1
	}
	updated := *w
	updated.Mutual = int64(newM)
	pushWarChanged(ctx, sqldb, sessReg, updated, warUpdateChange(updated, map[string][2]marshal.PyRep{
		"mutual": {marshal.PyInt{Value: oldM}, marshal.PyInt{Value: newM}},
	}))
	log.Printf("war %d mutual=%v by owner %d", warID, mutual, ownerID)
	return nil
}

// announceWarAdoptions pushes the wars a joining corporation handed to its alliance
// (evedb.AdoptCorpWars). A retargeted war goes out as a REMOVE of the corp-level row and an ADD
// of the alliance-level one, since an UPDATE dict cannot move a war between two parties'
// caches. A merged war finishes with no letter.
func announceWarAdoptions(ctx context.Context, sqldb *sql.DB, sessReg *esp.SessionRegistry, adopted []evedb.WarAdoption) {
	if len(adopted) == 0 {
		return
	}
	now := server.Win32TimeNow()
	for _, a := range adopted {
		if a.New == nil {
			done := a.Old
			done.TimeFinished = sql.NullInt64{Int64: int64(now), Valid: true}
			pushWarChanged(ctx, sqldb, sessReg, done, warUpdateChange(done, map[string][2]marshal.PyRep{
				"timeFinished": {marshal.PyNone{}, marshal.PyInt{Value: now}},
			}))
			log.Printf("war %d merged into the declarer's war on the alliance", a.Old.WarID)
			continue
		}
		pushWarChanged(ctx, sqldb, sessReg, a.Old, evedb.WarChangeReps(a.Old, false, true))
		pushWarChanged(ctx, sqldb, sessReg, *a.New, evedb.WarChangeReps(*a.New, true, false))
		log.Printf("war %d adopted: %d → %d is now against alliance %d",
			a.Old.WarID, a.Old.DeclaredByID, a.Old.AgainstID, a.New.AgainstID)
	}
	invalidateWarCache()
}

// finishWarNow ends a war (sweep, unpaid bill, or GM), pushes the update and
// mails both sides.
func finishWarNow(ctx context.Context, sqldb *sql.DB, sessReg *esp.SessionRegistry, w evedb.War, reason string) {
	finished, err := evedb.FinishWar(ctx, sqldb, w.WarID)
	if err != nil || !finished {
		return
	}
	now := server.Win32TimeNow()
	updated := w
	updated.TimeFinished = sql.NullInt64{Int64: int64(now), Valid: true}
	pushWarChanged(ctx, sqldb, sessReg, updated, warUpdateChange(updated, map[string][2]marshal.PyRep{
		"timeFinished": {marshal.PyNone{}, marshal.PyInt{Value: now}},
	}))
	sendWarEndedMail(ctx, sqldb, updated, reason)
	log.Printf("war %d finished: %s", w.WarID, reason)
}

// handleCorpWarCall serves RetractWar / ChangeMutualWarFlag / DeclareWarAgainst on the
// corpRegistry bound object for a corp with no alliance. The client sends only the first two
// here and declares by vote.
func handleCorpWarCall(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket,
	accountID, charID int64, sqldb *sql.DB, sessReg *esp.SessionRegistry) error {

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	corpID, callerAlliance := allianceCallerCorp(ctx, sqldb, charID)
	if corpID == 0 {
		return corpUserErr(c, pkt, accountID, "No corporation.")
	}
	if callerAlliance != 0 {
		// The client raises YouAreInAnAllianceSoCallOnThatService before
		// sending; a hand-crafted call gets the same answer.
		return corpUserErr(c, pkt, accountID, "Your corporation is in an alliance — act through the alliance.")
	}
	if !isDirectorOrCEO(ctx, sqldb, corpID, charID) {
		return corpUserErr(c, pkt, accountID, "Only a Director may manage wars.")
	}
	argInt := func(i int) int64 {
		if i < len(cs.ArgTuple.Items) {
			if v, ok := cs.ArgTuple.Items[i].(marshal.PyInt); ok {
				return int64(v.Value)
			}
		}
		return 0
	}

	switch cs.Method {
	case "DeclareWarAgainst":
		war, err := declareWar(ctx, sqldb, sessReg, corpID, corpID, argInt(0), false)
		if err != nil {
			return refuseErr(c, pkt, accountID, err)
		}
		log.Printf("[%s] → corpRegistry.DeclareWarAgainst corp=%d against=%d war=%d", addr, corpID, argInt(0), war.WarID)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	case "RetractWar":
		if err := retractWarFor(ctx, sqldb, sessReg, corpID, argInt(0)); err != nil {
			return corpUserErr(c, pkt, accountID, err.Error())
		}
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	case "ChangeMutualWarFlag":
		if err := setWarMutualFor(ctx, sqldb, sessReg, corpID, argInt(0), argInt(1) != 0); err != nil {
			return corpUserErr(c, pkt, accountID, err.Error())
		}
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// ── CONCORD exemption cache ──────────────────────────────────────────────────

// The damage paths ask "is this shot a war shot" on every hit, so the answer
// is cached for a minute per unordered pair of sides, each side its corp and
// alliance. A war arming or cooling mid-minute is off by at most that.
// The key carries both IDs of each side: a war declared on one corporation
// inside an alliance holds for that corp only, so two corps of one alliance
// can get different answers against the same enemy.
type warSide struct{ corp, alliance int64 }

type warPairKey struct{ a, b warSide }

func (s warSide) less(o warSide) bool {
	if s.corp != o.corp {
		return s.corp < o.corp
	}
	return s.alliance < o.alliance
}

var warHostileCache syncx.Map[warPairKey, warHostileEntry] // warPairKey → warHostileEntry

type warHostileEntry struct {
	hostile bool
	expires time.Time
}

// WarShotIsLegal reports whether attacker and victim are on opposite sides of a war the client
// currently paints as hostile, the exemption maybeTriggerConcordResponse consults. The client
// keys its check on allianceID or corpID, so both owners of each pilot are tried.
func WarShotIsLegal(sqldb *sql.DB, attackerCorp, attackerAlliance, victimCorp, victimAlliance int64) bool {
	if sqldb == nil {
		return false
	}
	a := warSide{attackerCorp, attackerAlliance}
	b := warSide{victimCorp, victimAlliance}
	if a == (warSide{}) || b == (warSide{}) {
		return false
	}
	key := warPairKey{a, b}
	if b.less(a) {
		key = warPairKey{b, a}
	}
	if v, ok := warHostileCache.Load(key); ok {
		if e := v; time.Now().Before(e.expires) {
			return e.hostile
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	hostile, err := evedb.HostileWarBetween(ctx, sqldb,
		[]int64{attackerCorp, attackerAlliance}, []int64{victimCorp, victimAlliance})
	if err != nil {
		return false
	}
	warHostileCache.Store(key, warHostileEntry{hostile: hostile, expires: time.Now().Add(time.Minute)})
	return hostile
}

// invalidateWarCache drops the pair cache after any war state change.
func invalidateWarCache() {
	warHostileCache.Range(func(k warPairKey, _ warHostileEntry) bool {
		warHostileCache.Delete(k)
		return true
	})
}

// ── Sweep ────────────────────────────────────────────────────────────────────

var warSweepMu sync.Mutex

// sweepWars runs the war lifecycle: finish retracted wars whose cool-down is
// up, issue the weekly CONCORD continuation bill, and end wars whose bill went
// unpaid past its due date.
func sweepWars(sqldb *sql.DB, sessReg *esp.SessionRegistry) {
	warSweepMu.Lock()
	defer warSweepMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// 1. Retraction cool-downs.
	if expired, err := evedb.ExpiredRetractedWars(ctx, sqldb); err == nil {
		for _, w := range expired {
			finishWarNow(ctx, sqldb, sessReg, w, "the war was retracted and the cool-down period has elapsed")
		}
	} else {
		log.Printf("war sweep retractions: %v", err)
	}

	// 2. Weekly continuation bills, payable to CONCORD.
	due, err := evedb.WarsDueForBilling(ctx, sqldb)
	if err != nil {
		log.Printf("war sweep billing: %v", err)
		due = nil
	}
	for _, w := range due {
		issueWarBill(ctx, sqldb, sessReg, w)
	}

	// 3. Unpaid war bills past due: the aggressor stopped paying, CONCORD
	// stops sanctioning.
	overdue, err := evedb.OverdueUnpaidBills(ctx, sqldb, evedb.BillTypeWarBill)
	if err != nil {
		log.Printf("war sweep overdue: %v", err)
		return
	}
	for _, b := range overdue {
		if b.ExternalID2 <= 0 {
			_ = evedb.WriteOffBill(ctx, sqldb, b.BillID)
			continue
		}
		w, err := evedb.GetWar(ctx, sqldb, b.ExternalID2)
		if err != nil || w == nil || w.TimeFinished.Valid {
			_ = evedb.WriteOffBill(ctx, sqldb, b.BillID)
			continue
		}
		_ = evedb.WriteOffBill(ctx, sqldb, b.BillID)
		finishWarNow(ctx, sqldb, sessReg, *w,
			"the aggressor failed to pay CONCORD's weekly war fee")
	}
	invalidateWarCache()
}

// issueWarBill creates one weekly continuation bill: debtor is the declarer's paying corp,
// creditor CONCORD, externalID the enemy (the client renders "war against <owner>"),
// externalID2 the warID (server-side linkage; the client ignores it for war bills).
func issueWarBill(ctx context.Context, sqldb *sql.DB, sessReg *esp.SessionRegistry, w evedb.War) {
	payer := w.DeclaredByID
	if kind, _ := evedb.ClassifyWarOwner(ctx, sqldb, w.DeclaredByID); kind == evedb.WarOwnerAlliance {
		var executor int64
		if err := sqldb.QueryRowContext(ctx,
			`SELECT executorCorpID FROM alliances WHERE allianceID = ?`, w.DeclaredByID,
		).Scan(&executor); err != nil || executor == 0 {
			log.Printf("war %d: alliance %d has no executor to bill — ending the war", w.WarID, w.DeclaredByID)
			finishWarNow(ctx, sqldb, sessReg, w, "the aggressor alliance has no executor corporation to sustain it")
			return
		}
		payer = executor
	}
	amount := evedb.WarWeeklyBillCost(ctx, sqldb)
	graceTicks := uint64(evedb.BillGraceDays(ctx, sqldb) * 24 * 3600 * 10000000)
	week := evedb.WarBillPeriodTicks
	bill, created, err := evedb.CreateBill(ctx, sqldb, evedb.Bill{
		BillTypeID:  evedb.BillTypeWarBill,
		DebtorID:    payer,
		CreditorID:  evedb.OwnerCONCORDBills,
		Amount:      amount,
		DueDateTime: int64(server.Win32TimeNow() + graceTicks),
		Interest:    0,
		ExternalID:  w.AgainstID,
		ExternalID2: w.WarID,
	}, week)
	if err != nil {
		log.Printf("war %d bill: %v", w.WarID, err)
		return
	}
	if !created {
		return
	}
	_ = evedb.StampWarBilled(ctx, sqldb, w.WarID)
	pushBillReceived(sessReg, payer)
	// The letter mirrors the client's own BillMgrWarBillMessage wording.
	against := ownerDisplayName(ctx, sqldb, w.AgainstID)
	subject := "Bill issued"
	body := fmt.Sprintf(
		"This is an automatic message. A bill of %.2f ISK, due %s owed by you to CONCORD "+
			"was issued %s. This bill is for continuing the war against %s.",
		amount, fmtWin32Ticks(uint64(bill.DueDateTime)), fmtWin32Ticks(server.Win32TimeNow()), against)
	created64 := int64(server.Win32TimeNow())
	for _, charID := range corpDirectorsAndCEO(ctx, sqldb, payer) {
		if msgID, err := evedb.SendEveMail(ctx, sqldb, charID, evedb.OwnerCONCORD, subject, body, created64); err == nil {
			pushMailNotification(charID, msgID, evedb.OwnerCONCORD, subject, created64)
		}
	}
	log.Printf("war %d: weekly bill %d (%.0f ISK) issued to corp %d, due in %.0f day(s)",
		w.WarID, bill.BillID, amount, payer, evedb.BillGraceDays(ctx, sqldb))
}

// corpDirectorsAndCEO lists the pilots who can pay a corp bill, so the letter lands with
// someone who can act on it.
func corpDirectorsAndCEO(ctx context.Context, sqldb *sql.DB, corpID int64) []int64 {
	rows, err := sqldb.QueryContext(ctx, `
		SELECT DISTINCT ch.characterID
		  FROM character_ ch
		  JOIN corporation c ON c.corporationID = ch.corporationID
		  LEFT JOIN chrCorporationRoles r ON r.characterID = ch.characterID
		 WHERE ch.corporationID = ? AND ch.accountID > 0
		   AND (c.ceoID = ch.characterID OR (COALESCE(r.corprole, 0) & 1) = 1)`, corpID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err == nil {
			out = append(out, id)
		}
	}
	return out
}

// fmtWin32Ticks renders a FILETIME as a human date for mail bodies.
func fmtWin32Ticks(ticks uint64) string {
	const win32Epoch = 116444736000000000 // 1970-01-01 in FILETIME ticks
	if ticks < win32Epoch {
		return "unknown"
	}
	return time.Unix(int64((ticks-win32Epoch)/10000000), 0).UTC().Format("2006.01.02 15:04")
}
