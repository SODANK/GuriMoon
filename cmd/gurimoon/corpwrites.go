package main

// corpRegistry bound-object write methods: corporation edits, titles, member roles, votes,
// shares, item lockdown and application offers, dispatched via handleCorpRegistryWrite.
//
// Client contract (bco_*.py): every write's return value is discarded (except
// UpdateCorporationAbilities, which must be non-None, and ResignFromCEO's two-phase protocol);
// the UI refreshes from the On*Changed notifications. See docs/corp-notes.md#change-dicts.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"reflect"
	"time"

	"gurimoon/corp"
	evedb "gurimoon/db"
	"gurimoon/esp"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
	"gurimoon/server/slash"
	"gurimoon/tick"
)

// corpWriteMethods lists the methods handleCorpRegistryWrite owns.
var corpWriteMethods = map[string]bool{
	"UpdateCorporation": true, "UpdateCorporationAbilities": true,
	"UpdateLogo": true, "GetSuggestedTickerNames": true,
	"UpdateDivisionNames": true, "PayoutDividend": true,
	"ResignFromCEO": true, "KickOutMember": true,
	"UpdateTitle": true, "UpdateTitles": true, "DeleteTitle": true,
	"UpdateMember": true, "UpdateMembers": true, "ExecuteActions": true,
	"InsertVoteCase": true, "InsertVote": true,
	"MoveCompanyShares": true, "MovePrivateShares": true,
	"GetLockedItemsByLocation": true, "UpdateSanctionedAction": true,
	"UpdateApplicationOffer": true,
}

func init() {
	// A CEO row written before JoinCorporation and SetCorpCEO granted the grantable masks holds none.
	moduleBootHooks = append(moduleBootHooks, func(sqldb *sql.DB, _ *esp.SessionRegistry, _ *tick.Manager) {
		if sqldb == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := evedb.RepairCEOGrantableRoles(ctx, sqldb); err != nil {
			log.Printf("%v", err)
		}
	})
}

// ─── small helpers ─────────────────────────────────────────────────────────

func corpUserErr(c *evenet.Conn, pkt *evenet.PyPacket, accountID int64, msg string) error {
	return server.SendCallException(c, pkt, accountID, server.BuildUserError(
		"CustomError", map[string]marshal.PyRep{"error": marshal.PyString{Value: msg}}))
}

// pyOptInt reads an int argument that may be None (the client's
// "don't change" sentinel).
func pyOptInt(rep marshal.PyRep) *int64 {
	if v, ok := rep.(marshal.PyInt); ok {
		n := int64(v.Value)
		return &n
	}
	return nil
}

// pyStr reads a string argument. A free-text field arrives as a PyBuffer (the edit dialogs
// encode to cp1252 and the client marshals that as opBuffer), so both forms are accepted.
func pyStr(rep marshal.PyRep) (string, bool) {
	switch s := rep.(type) {
	case marshal.PyString:
		return s.Value, true
	case marshal.PyBuffer:
		return string(s.Data), true
	}
	return "", false
}

// parseRowsetArg decodes a client-marshalled util.Rowset instance; see
// server.ParseRowsetArg, which posMgr shares.
func parseRowsetArg(rep marshal.PyRep) (header []string, lines [][]marshal.PyRep, ok bool) {
	return server.ParseRowsetArg(rep)
}

// chg builds one change-dict entry: column → (old, new).
func chg(col string, oldV, newV marshal.PyRep) marshal.DictEntry {
	return marshal.DictEntry{Key: marshal.PyString{Value: col},
		Value: marshal.PyTuple{Items: []marshal.PyRep{oldV, newV}}}
}

// appRemoveNotification builds an OnCorporationApplicationChanged notification whose change
// dict signals REMOVE (all new values None; bco_base.GetAddRemoveFromChange drops the row). Arg
// order is (applicantID, corporationID, change), per bco_applications.py.
func appRemoveNotification(corpID, charID, oldStatus int64) marshal.PyTuple {
	n := marshal.PyNone{}
	return marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyInt{Value: uint64(charID)},
		marshal.PyInt{Value: uint64(corpID)},
		marshal.PyDict{Items: []marshal.DictEntry{
			chg("corporationID", marshal.PyInt{Value: uint64(corpID)}, n),
			chg("characterID", marshal.PyInt{Value: uint64(charID)}, n),
			chg("applicationText", marshal.PyString{Value: ""}, n),
			chg("roles", marshal.PyInt{Value: 0}, n),
			chg("grantableRoles", marshal.PyInt{Value: 0}, n),
			chg("status", marshal.PyInt{Value: uint64(oldStatus)}, n),
			chg("applicationDateTime", marshal.PyInt{Value: 0}, n),
			chg("deleted", marshal.PyInt{Value: 0}, n),
			chg("lastCorpUpdaterID", marshal.PyInt{Value: 0}, n),
		}},
	}}
}

// espChangeRep converts one value out of an ESP panel edit (esp.CorpFieldChange) into the PyRep
// the client's change dict expects. Anything unrecognised becomes None: a wrong type is read by
// godma as a real value and sticks until relog.
func espChangeRep(v any) marshal.PyRep {
	switch t := v.(type) {
	case nil:
		return marshal.PyNone{}
	case string:
		return marshal.PyString{Value: t}
	case int64:
		return marshal.PyInt{Value: uint64(t)}
	case int:
		return marshal.PyInt{Value: uint64(t)}
	case float64:
		return marshal.PyReal{Value: t}
	case bool:
		return marshal.PyBool{Value: t}
	}
	return marshal.PyNone{}
}

// lockVoteRefusal checks a lockdown/unlock vote's items when it is proposed, so a vote that
// could never be carried out is refused up front. Only options carrying an itemID are checked.
func lockVoteRefusal(ctx context.Context, sqldb *sql.DB, corpID, voteType int64, options []evedb.VoteCaseOption) string {
	if voteType != evedb.VoteItemLockdown && voteType != evedb.VoteItemUnlock {
		return ""
	}
	for _, o := range options {
		if !o.Parameter.Valid || o.Parameter.Int64 == 0 {
			continue
		}
		if voteType == evedb.VoteItemUnlock {
			if _, ok, err := evedb.GetLockedItem(ctx, sqldb, corpID, o.Parameter.Int64); err == nil && !ok {
				return evedb.LockMsgItemNotLocked
			}
			continue
		}
		station := int64(0)
		if o.Parameter2.Valid {
			station = o.Parameter2.Int64
		}
		if r, err := evedb.CheckLockable(ctx, sqldb, corpID, o.Parameter.Int64, station); err == nil && r != "" {
			return r
		}
	}
	return ""
}

// pushLockedItemChange tells every online member of the corporation that a lock was added or
// removed: bco_itemLocking caches the lock list per station for the session. The change dict
// carries every LockedItemCols column (the add branch builds its row from the header); all-None
// new values are the remove form.
func pushLockedItemChange(sessReg *esp.SessionRegistry, li evedb.LockedItem, added bool) {
	vals := map[string]int64{"itemID": li.ItemID, "typeID": li.TypeID, "ownerID": li.OwnerID}
	var entries []marshal.DictEntry
	for _, col := range evedb.LockedItemCols {
		v := marshal.PyRep(marshal.PyInt{Value: uint64(vals[col])})
		if added {
			entries = append(entries, chg(col, marshal.PyNone{}, v))
		} else {
			entries = append(entries, chg(col, v, marshal.PyNone{}))
		}
	}
	notifyCorp(sessReg, li.CorpID, "OnLockedItemChange", marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyInt{Value: uint64(li.ItemID)},
		marshal.PyInt{Value: uint64(li.OwnerID)},
		marshal.PyInt{Value: uint64(li.StationID)},
		marshal.PyDict{Items: entries},
	}})
}

func notifyCorp(sessReg *esp.SessionRegistry, corpID int64, eventType string, args marshal.PyRep) {
	if sessReg == nil {
		return
	}
	for _, s := range sessReg.FindByCorpID(corpID) {
		if s.Conn == nil {
			continue
		}
		if err := server.SendNotification(s.Conn, s.AccountID, eventType, "corpid", args); err != nil {
			log.Printf("[%s] notifyCorp %s: %v", s.Addr, eventType, err)
		}
	}
}

// onCorporationChanged pushes the corp-row change dict every corp UI keys off.
func onCorporationChanged(sessReg *esp.SessionRegistry, corpID int64, entries []marshal.DictEntry) {
	notifyCorp(sessReg, corpID, "OnCorporationChanged", marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyInt{Value: uint64(corpID)},
		marshal.PyDict{Items: entries},
	}})
}

// pushMemberRoleSession sends target the session change that carries a new role mask: corprole
// and the rolesAt* keys are all the client knows of what it may do, and sessions.py recomputes
// corprole on every location change. old may be nil; the change is skipped when nothing moved,
// except when force is set (a self-edit arrives inside PerformSessionChange).
func pushMemberRoleSession(ctx context.Context, sqldb *sql.DB, target int64, old *evedb.CorpMemberInfo, force bool) {
	conn, accountID := charConnLookup(target)
	if conn == nil {
		return
	}
	fresh, err := evedb.GetCorporationMemberInfo(ctx, sqldb, target)
	if err != nil || fresh == nil {
		return
	}
	var was evedb.CorpMemberInfo
	if old != nil {
		was = *old
	}
	if !force && was == *fresh {
		return
	}
	pair := func(o, n uint64) marshal.PyRep {
		return marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: o}, marshal.PyInt{Value: n},
		}}
	}
	// baseID is a role-bearing session value like hqID, None when unset (docs/corp-notes.md#hq-session).
	idRep := func(id int64) marshal.PyRep {
		if id == 0 {
			return marshal.PyNone{}
		}
		return marshal.PyInt{Value: uint64(id)}
	}
	if err := server.SendSessionChange(conn, uint64(accountID), []marshal.DictEntry{
		{Key: marshal.PyString{Value: "corprole"}, Value: pair(was.Corprole, fresh.Corprole)},
		{Key: marshal.PyString{Value: "rolesAtAll"}, Value: pair(was.RolesAtAll, fresh.RolesAtAll)},
		{Key: marshal.PyString{Value: "rolesAtHQ"}, Value: pair(was.RolesAtHQ, fresh.RolesAtHQ)},
		{Key: marshal.PyString{Value: "rolesAtBase"}, Value: pair(was.RolesAtBase, fresh.RolesAtBase)},
		{Key: marshal.PyString{Value: "rolesAtOther"}, Value: pair(was.RolesAtOther, fresh.RolesAtOther)},
		{Key: marshal.PyString{Value: "baseID"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			idRep(was.BaseID), idRep(fresh.BaseID),
		}}},
	}); err != nil {
		log.Printf("pushMemberRoleSession %d: %v", target, err)
	}
}

// pushTitleRoleSessions re-pushes roles to the members snapshotted before a title edit; a title
// is granted in bulk, so an edit changes what many pilots may do.
func pushTitleRoleSessions(ctx context.Context, sqldb *sql.DB, before map[int64]evedb.CorpMemberInfo) {
	for charID, old := range before {
		prev := old
		pushMemberRoleSession(ctx, sqldb, charID, &prev, false)
	}
}

// onlineTitleHolders snapshots the effective roles of every online member of
// corpID whose titleMask includes titleID, so a title edit can be diffed
// against what those members held before it.
func onlineTitleHolders(ctx context.Context, sqldb *sql.DB, corpID, titleID int64) map[int64]evedb.CorpMemberInfo {
	rows, err := sqldb.QueryContext(ctx, `
		SELECT c.characterID
		FROM character_ c
		JOIN chrCorporationRoles r ON r.characterID = c.characterID
		WHERE c.corporationID = ? AND (r.titleMask & ?) <> 0`, corpID, titleID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := map[int64]evedb.CorpMemberInfo{}
	for rows.Next() {
		var charID int64
		if err := rows.Scan(&charID); err != nil {
			return out
		}
		if conn, _ := charConnLookup(charID); conn == nil {
			continue
		}
		if info, err := evedb.GetCorporationMemberInfo(ctx, sqldb, charID); err == nil && info != nil {
			out[charID] = *info
		}
	}
	return out
}

// callerRoles fetches the caller's role mask.
func callerRoles(ctx context.Context, sqldb *sql.DB, charID int64) uint64 {
	info, err := evedb.GetCorporationMemberInfo(ctx, sqldb, charID)
	if err != nil {
		return 0
	}
	return info.Corprole
}

// isCorpCEO reports whether charID is the CEO of corpID.
func isCorpCEO(ctx context.Context, sqldb *sql.DB, corpID, charID int64) bool {
	var ceoID int64
	if err := sqldb.QueryRowContext(ctx,
		`SELECT ceoID FROM corporation WHERE corporationID = ?`, corpID).Scan(&ceoID); err != nil {
		return false
	}
	return ceoID == charID
}

// mayRenameItem is invbroker.SetLabel's ownership rule, the server half of menusvc
// checkCanRename: the pilot's own item, or one their corporation owns when they hold Equipment
// Config, Director or are the CEO. A POS structure is always corporation property.
func mayRenameItem(ctx context.Context, sqldb *sql.DB, charID int64, er *evedb.EntityRow) bool {
	if er == nil {
		return false
	}
	if er.OwnerID == charID {
		return true
	}
	corpID := boundCharCorpID(charID, sqldb)
	if corpID == 0 || er.OwnerID != corpID {
		return false
	}
	roles := callerRoles(ctx, sqldb, charID)
	return corp.HasRole(roles, corp.RoleEquipmentConfig) || corp.HasRole(roles, corp.RoleDirector) ||
		isCorpCEO(ctx, sqldb, corpID, charID)
}

func isDirectorOrCEO(ctx context.Context, sqldb *sql.DB, corpID, charID int64) bool {
	return isCorpCEO(ctx, sqldb, corpID, charID) ||
		corp.HasRole(callerRoles(ctx, sqldb, charID), corp.RoleDirector)
}

// kickToNPCCorp moves a character out of corpID into their fallback NPC corp,
// sends the session change if they are online, and pushes the memberCount
// change to the corp.  Returns the NPC corp joined.
func kickToNPCCorp(ctx context.Context, sqldb *sql.DB, sessReg *esp.SessionRegistry, targetID, corpID int64, targetConn *evenet.Conn) int64 {
	npcCorp := evedb.PreviousNPCCorp(ctx, sqldb, targetID)
	roles, err := evedb.JoinCorporation(ctx, sqldb, targetID, npcCorp, corpID)
	if err != nil {
		log.Printf("kickToNPCCorp charID=%d: %v", targetID, err)
		return 0
	}
	if targetConn != nil {
		_ = slash.SendCorpSessionChange(targetConn, corpID, npcCorp, roles)
		_ = slash.SendCorpAllianceSessionChange(ctx, sqldb, targetConn, corpID, npcCorp)
	}
	var mc int64
	_ = sqldb.QueryRowContext(ctx,
		`SELECT memberCount FROM corporation WHERE corporationID = ?`, corpID).Scan(&mc)
	onCorporationChanged(sessReg, corpID, []marshal.DictEntry{
		chg("memberCount", marshal.PyInt{Value: uint64(mc + 1)}, marshal.PyInt{Value: uint64(mc)}),
	})
	return npcCorp
}

// ─── dispatcher ────────────────────────────────────────────────────────────

func handleCorpRegistryWrite(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID int64, sqldb *sql.DB, sessReg *esp.SessionRegistry) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	corpID := boundCharCorpID(charID, sqldb)
	if corpID == 0 || sqldb == nil {
		return corpUserErr(c, pkt, accountID, "You are not in a corporation.")
	}
	arg := func(i int) marshal.PyRep {
		if i < len(cs.ArgTuple.Items) {
			return cs.ArgTuple.Items[i]
		}
		return nil
	}
	none := marshal.PyNone{}
	done := func() error { return server.SendCallRsp(c, pkt, accountID, none) }

	switch cs.Method {

	case "UpdateCorporation":
		// (description, url, taxRate), Director only; the UI refresh rides on OnCorporationChanged.
		if !isDirectorOrCEO(ctx, sqldb, corpID, charID) {
			return corpUserErr(c, pkt, accountID, "Only a Director may edit corporation details.")
		}
		desc, _ := pyStr(arg(0))
		url, _ := pyStr(arg(1))
		var taxRate float64
		switch v := arg(2).(type) {
		case marshal.PyReal:
			taxRate = v.Value
		case marshal.PyInt:
			taxRate = float64(int64(v.Value))
		}
		if taxRate < 0 || taxRate > 1 {
			return corpUserErr(c, pkt, accountID, "Invalid tax rate.")
		}
		oldDesc, oldURL, oldTax, err := evedb.UpdateCorporationDetails(ctx, sqldb, corpID, desc, url, taxRate)
		if err != nil {
			log.Printf("[%s] corpRegistry.UpdateCorporation: %v", addr, err)
			return corpUserErr(c, pkt, accountID, "Update failed.")
		}
		onCorporationChanged(sessReg, corpID, []marshal.DictEntry{
			chg("description", marshal.PyString{Value: oldDesc}, marshal.PyString{Value: desc}),
			chg("url", marshal.PyString{Value: oldURL}, marshal.PyString{Value: url}),
			chg("taxRate", marshal.PyReal{Value: oldTax}, marshal.PyReal{Value: taxRate}),
		})
		log.Printf("[%s] → corpRegistry.UpdateCorporation corpID=%d tax=%.3f", addr, corpID, taxRate)
		return done()

	case "UpdateCorporationAbilities":
		// () recompute memberLimit/allowedMemberRaceIDs from the CEO's leadership skills. Must return
		// non-None: the client aborts its refresh on None.
		if !isDirectorOrCEO(ctx, sqldb, corpID, charID) {
			return corpUserErr(c, pkt, accountID, "Only a Director may update corporation abilities.")
		}
		oldLimit, memberLimit, oldRaces, allowedRaces, err := evedb.RecomputeCorpAbilities(ctx, sqldb, corpID)
		if err != nil {
			log.Printf("[%s] corpRegistry.UpdateCorporationAbilities: %v", addr, err)
			return corpUserErr(c, pkt, accountID, "Update failed.")
		}
		onCorporationChanged(sessReg, corpID, []marshal.DictEntry{
			chg("memberLimit", marshal.PyInt{Value: uint64(oldLimit)}, marshal.PyInt{Value: uint64(memberLimit)}),
			chg("allowedMemberRaceIDs", marshal.PyInt{Value: uint64(oldRaces)}, marshal.PyInt{Value: uint64(allowedRaces)}),
		})
		log.Printf("[%s] → corpRegistry.UpdateCorporationAbilities corpID=%d limit=%d races=%d", addr, corpID, memberLimit, allowedRaces)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyInt{Value: 1})

	case "UpdateLogo":
		// (shape1..3, color1..3, typeface=None), Director only. The logo change fee
		// (const.corpLogoChangeCost) is not billed.
		if !isDirectorOrCEO(ctx, sqldb, corpID, charID) {
			return corpUserErr(c, pkt, accountID, "Only a Director may change the corporation logo.")
		}
		var vals [6]sql.NullInt64
		for i := 0; i < 6; i++ {
			if v := pyOptInt(arg(i)); v != nil {
				vals[i] = sql.NullInt64{Int64: *v, Valid: true}
			}
		}
		old, err := evedb.UpdateCorpLogo(ctx, sqldb, corpID, vals)
		if err != nil {
			log.Printf("[%s] corpRegistry.UpdateLogo: %v", addr, err)
			return corpUserErr(c, pkt, accountID, "Logo update failed.")
		}
		cols := []string{"shape1", "shape2", "shape3", "color1", "color2", "color3"}
		entries := make([]marshal.DictEntry, 0, 6)
		nrep := func(v sql.NullInt64) marshal.PyRep {
			if !v.Valid {
				return marshal.PyNone{}
			}
			return marshal.PyInt{Value: uint64(v.Int64)}
		}
		for i, col := range cols {
			entries = append(entries, chg(col, nrep(old[i]), nrep(vals[i])))
		}
		onCorporationChanged(sessReg, corpID, entries)
		log.Printf("[%s] → corpRegistry.UpdateLogo corpID=%d", addr, corpID)
		return done()

	case "GetSuggestedTickerNames":
		// (corporationName) → rowset of candidate tickers.
		name, _ := pyStr(arg(0))
		var lines []marshal.PyRep
		for _, t := range evedb.SuggestedTickers(ctx, sqldb, name) {
			lines = append(lines, marshal.PyList{Items: []marshal.PyRep{marshal.PyString{Value: t}}})
		}
		log.Printf("[%s] → corpRegistry.GetSuggestedTickerNames %q (%d)", addr, name, len(lines))
		return server.SendCallRsp(c, pkt, accountID, marshal.PyObject{
			Type: "util.Rowset",
			Args: marshal.PyDict{Items: []marshal.DictEntry{
				{Key: marshal.PyString{Value: "header"}, Value: marshal.PyList{Items: []marshal.PyRep{marshal.PyString{Value: "tickerName"}}}},
				{Key: marshal.PyString{Value: "RowClass"}, Value: marshal.PyString{Value: "util.Row", IsType1: true}},
				{Key: marshal.PyString{Value: "lines"}, Value: marshal.PyList{Items: lines}},
			}},
		})

	case "UpdateDivisionNames":
		// (div1..div7), CEO only. Validation errors would need CorpDiv* notify keys; only length is checked.
		if !isCorpCEO(ctx, sqldb, corpID, charID) {
			return corpUserErr(c, pkt, accountID, "Only the CEO can edit division names.")
		}
		var div [7]string
		for i := 0; i < 7; i++ {
			div[i], _ = pyStr(arg(i))
			if len(div[i]) > 50 {
				div[i] = div[i][:50]
			}
		}
		old, err := evedb.UpdateCorpDivisions(ctx, sqldb, corpID, div)
		if err != nil {
			log.Printf("[%s] corpRegistry.UpdateDivisionNames: %v", addr, err)
			return corpUserErr(c, pkt, accountID, "Division rename failed.")
		}
		entries := make([]marshal.DictEntry, 0, 7)
		for i := 0; i < 7; i++ {
			entries = append(entries, chg(fmt.Sprintf("division%d", i+1),
				marshal.PyString{Value: old[i]}, marshal.PyString{Value: div[i]}))
		}
		onCorporationChanged(sessReg, corpID, entries)
		log.Printf("[%s] → corpRegistry.UpdateDivisionNames corpID=%d", addr, corpID)
		return done()

	case "PayoutDividend":
		// (payShareholders, payoutAmount), CEO only. 1 splits by shareholding, treasury excluded; 0
		// splits equally among members. One transaction (evedb.PayDividend), journalled as refType 38.
		if !isCorpCEO(ctx, sqldb, corpID, charID) {
			return corpUserErr(c, pkt, accountID, "Only the CEO can pay out dividends.")
		}
		payShareholders := pyOptInt(arg(0))
		amountP := pyOptInt(arg(1))
		if amountP == nil || *amountP <= 0 {
			return corpUserErr(c, pkt, accountID, "Invalid dividend amount.")
		}
		amount := float64(*amountP)
		var payouts []evedb.DividendPayout
		if payShareholders != nil && *payShareholders != 0 {
			holders, err := evedb.BeneficialShareHolders(ctx, sqldb, corpID)
			if err != nil {
				return corpUserErr(c, pkt, accountID, "Shareholder lookup failed.")
			}
			payouts = evedb.SplitDividendByShares(amount, corpID, holders)
		} else {
			ids, err := evedb.GetCorpMemberIDs(ctx, sqldb, corpID)
			if err != nil {
				return corpUserErr(c, pkt, accountID, "Member lookup failed.")
			}
			payouts = evedb.SplitDividendEqually(amount, ids)
		}
		if len(payouts) == 0 {
			return corpUserErr(c, pkt, accountID, "There is nobody to pay, or the amount is too small to divide.")
		}
		total, paid, corpBal, err := evedb.PayDividend(ctx, sqldb, corpID, payouts, "Dividend")
		if errors.Is(err, evedb.ErrInsufficientFunds) {
			return corpUserErr(c, pkt, accountID, "The corporation cannot afford that dividend.")
		}
		if err != nil {
			log.Printf("[%s] corpRegistry.PayoutDividend: %v", addr, err)
			return corpUserErr(c, pkt, accountID, "Dividend payout failed.")
		}
		for _, p := range paid {
			if p.IsChar {
				server.NotifyCharacter(sessReg, p.PayeeID, "OnAccountChange", "clientID", marshal.PyTuple{Items: []marshal.PyRep{
					marshal.PyString{Value: "cash"},
					marshal.PyInt{Value: uint64(p.PayeeID)},
					marshal.PyReal{Value: p.Balance},
				}})
			}
		}
		// The CEO's corp wallet view.
		_ = server.SendNotification(c, accountID, "OnAccountChange", "clientID", marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyString{Value: "cash"},
			marshal.PyInt{Value: uint64(corpID)},
			marshal.PyReal{Value: corpBal},
		}})
		log.Printf("[%s] → corpRegistry.PayoutDividend corpID=%d asked=%.0f paid=%.0f payees=%d",
			addr, corpID, amount, total, len(paid))
		return done()

	case "ResignFromCEO":
		// (newCeoID | None), two-phase: with no successor and other members present, return candidate
		// charIDs and the client re-calls with the pick. Sole member and no successor closes the corp.
		if !isCorpCEO(ctx, sqldb, corpID, charID) {
			return corpUserErr(c, pkt, accountID, "You are not the CEO.")
		}
		newCeoID := pyOptInt(arg(0))
		members, err := evedb.GetCorpMemberIDs(ctx, sqldb, corpID)
		if err != nil {
			return corpUserErr(c, pkt, accountID, "Member lookup failed.")
		}
		if newCeoID == nil {
			if len(members) <= 1 {
				// Destroy the corporation.
				npcCorp := kickToNPCCorp(ctx, sqldb, sessReg, charID, corpID, c)
				if npcCorp == 0 {
					return corpUserErr(c, pkt, accountID, "Could not close the corporation.")
				}
				_ = evedb.MarkCorpDeleted(ctx, sqldb, corpID)
				// A closed corporation holds nothing: its stakes elsewhere return to the issuers' treasuries.
				if issuers, serr := evedb.ReturnSharesToIssuers(ctx, sqldb, corpID); serr != nil {
					log.Printf("[%s] ResignFromCEO close corp=%d shares: %v", addr, corpID, serr)
				} else {
					for _, id := range issuers {
						retallyShareVotes(ctx, sqldb, sessReg, id)
					}
				}
				log.Printf("[%s] → corpRegistry.ResignFromCEO corpID=%d closed, charID=%d → %d", addr, corpID, charID, npcCorp)
				return done()
			}
			items := make([]marshal.PyRep, 0, len(members)-1)
			for _, id := range members {
				if id != charID {
					items = append(items, marshal.PyInt{Value: uint64(id)})
				}
			}
			log.Printf("[%s] → corpRegistry.ResignFromCEO corpID=%d (%d candidates)", addr, corpID, len(items))
			return server.SendCallRsp(c, pkt, accountID, marshal.PyList{Items: items})
		}
		// Transfer to the chosen successor.
		found := false
		for _, id := range members {
			if id == *newCeoID {
				found = true
				break
			}
		}
		if !found {
			return clientMsgErr(c, pkt, accountID, "CrpCharNotInThisCorp", nil)
		}
		oldCeo, err := evedb.SetCorpCEO(ctx, sqldb, corpID, *newCeoID)
		if err != nil {
			log.Printf("[%s] corpRegistry.ResignFromCEO: %v", addr, err)
			return corpUserErr(c, pkt, accountID, "CEO transfer failed.")
		}
		// Demote the resigning CEO to a plain member.
		_, _ = sqldb.ExecContext(ctx, `
			REPLACE INTO chrCorporationRoles (characterID, corprole, rolesAtAll, rolesAtBase, rolesAtHQ, rolesAtOther)
			VALUES (?, 0, 0, 0, 0, 0)`, charID)
		_ = slash.SendCorpSessionChange(c, corpID, corpID, 0)
		onCorporationChanged(sessReg, corpID, []marshal.DictEntry{
			chg("ceoID", marshal.PyInt{Value: uint64(oldCeo)}, marshal.PyInt{Value: uint64(*newCeoID)}),
		})
		// The CEO votes the treasury and this corp's stakes elsewhere, so a new CEO moves every open tally.
		retallyAfterCEOChange(ctx, sqldb, sessReg, corpID)
		log.Printf("[%s] → corpRegistry.ResignFromCEO corpID=%d ceo %d → %d", addr, corpID, oldCeo, *newCeoID)
		return done()

	case "KickOutMember":
		// (charID): self-quit or Director expelling a member.
		targetP := pyOptInt(arg(0))
		if targetP == nil {
			return corpUserErr(c, pkt, accountID, "No member specified.")
		}
		target := *targetP
		if isCorpCEO(ctx, sqldb, corpID, target) {
			return corpUserErr(c, pkt, accountID, "The CEO must resign before leaving the corporation.")
		}
		if target != charID && !isDirectorOrCEO(ctx, sqldb, corpID, charID) {
			return corpUserErr(c, pkt, accountID, "Only a Director may expel members.")
		}
		var targetConn *evenet.Conn
		if target == charID {
			targetConn = c
		} else if conn, _ := charConnLookup(target); conn != nil {
			targetConn = conn
		}
		if kickToNPCCorp(ctx, sqldb, sessReg, target, corpID, targetConn) == 0 {
			return corpUserErr(c, pkt, accountID, "Could not remove the member.")
		}
		log.Printf("[%s] → corpRegistry.KickOutMember corpID=%d target=%d", addr, corpID, target)
		return done()

	case "UpdateTitle", "UpdateTitles":
		// Director only. UpdateTitle carries the 10 fields positionally, UpdateTitles a rowset of such
		// lines. One OnTitleChanged push per row keeps the client's progress meter in sync.
		if !isDirectorOrCEO(ctx, sqldb, corpID, charID) {
			return corpUserErr(c, pkt, accountID, "Only a Director may edit titles.")
		}
		var rows [][]marshal.PyRep
		if cs.Method == "UpdateTitle" {
			rows = [][]marshal.PyRep{cs.ArgTuple.Items}
		} else {
			_, lines, ok := parseRowsetArg(arg(0))
			if !ok {
				log.Printf("[%s] corpRegistry.UpdateTitles: unparseable rowset arg %T", addr, arg(0))
				return corpUserErr(c, pkt, accountID, "Malformed title update.")
			}
			rows = lines
		}
		if err := evedb.EnsureDefaultTitles(ctx, sqldb, corpID); err != nil {
			log.Printf("[%s] EnsureDefaultTitles: %v", addr, err)
		}
		titleCols := []string{"titleID", "titleName", "roles", "grantableRoles",
			"rolesAtHQ", "grantableRolesAtHQ", "rolesAtBase", "grantableRolesAtBase",
			"rolesAtOther", "grantableRolesAtOther"}
		for _, row := range rows {
			if len(row) < 10 {
				continue
			}
			idP := pyOptInt(row[0])
			name, _ := pyStr(row[1])
			if idP == nil {
				continue
			}
			t := evedb.TitleRow{TitleID: *idP, TitleName: name}
			for i := 0; i < 8; i++ {
				if v := pyOptInt(row[2+i]); v != nil {
					// The Director bit can never live in a title.
					t.Masks[i] = uint64(*v) &^ corp.RoleDirector
				}
			}
			old, _ := evedb.GetTitleRow(ctx, sqldb, corpID, t.TitleID)
			holders := onlineTitleHolders(ctx, sqldb, corpID, t.TitleID)
			if err := evedb.UpsertTitle(ctx, sqldb, corpID, t); err != nil {
				log.Printf("[%s] UpsertTitle %d: %v", addr, t.TitleID, err)
				continue
			}
			pushTitleRoleSessions(ctx, sqldb, holders)
			oldName := ""
			var oldMasks [8]uint64
			if old != nil {
				oldName, oldMasks = old.TitleName, old.Masks
			}
			entries := []marshal.DictEntry{
				chg(titleCols[1], marshal.PyString{Value: oldName}, marshal.PyString{Value: t.TitleName}),
			}
			for i := 0; i < 8; i++ {
				entries = append(entries, chg(titleCols[2+i],
					marshal.PyInt{Value: oldMasks[i]}, marshal.PyInt{Value: t.Masks[i]}))
			}
			notifyCorp(sessReg, corpID, "OnTitleChanged", marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyInt{Value: uint64(corpID)},
				marshal.PyInt{Value: uint64(t.TitleID)},
				marshal.PyDict{Items: entries},
			}})
		}
		log.Printf("[%s] → corpRegistry.%s corpID=%d (%d rows)", addr, cs.Method, corpID, len(rows))
		return done()

	case "DeleteTitle":
		if !isDirectorOrCEO(ctx, sqldb, corpID, charID) {
			return corpUserErr(c, pkt, accountID, "Only a Director may edit titles.")
		}
		if idP := pyOptInt(arg(0)); idP != nil {
			holders := onlineTitleHolders(ctx, sqldb, corpID, *idP)
			_ = evedb.DeleteTitleRow(ctx, sqldb, corpID, *idP)
			// Deleting a title takes back what it granted, so its holders need the new mask.
			pushTitleRoleSessions(ctx, sqldb, holders)
			log.Printf("[%s] → corpRegistry.DeleteTitle corpID=%d titleID=%d", addr, corpID, *idP)
		}
		return done()

	case "UpdateMember":
		// 14 positional args; None = leave unchanged.
		return applyMemberUpdate(ctx, c, addr, pkt, accountID, charID, corpID, sqldb, sessReg, cs.ArgTuple.Items)

	case "UpdateMembers":
		// One rowset of UpdateMember lines (a client bug can send self rows twice; the second write is a no-op).
		_, lines, ok := parseRowsetArg(arg(0))
		if !ok {
			log.Printf("[%s] corpRegistry.UpdateMembers: unparseable rowset arg %T", addr, arg(0))
			return corpUserErr(c, pkt, accountID, "Malformed member update.")
		}
		failed, firstErr := 0, error(nil)
		for _, row := range lines {
			if err := applyMemberUpdateRow(ctx, c, addr, charID, corpID, sqldb, row); err != nil {
				log.Printf("[%s] UpdateMembers row: %v", addr, err)
				failed++
				if firstErr == nil {
					firstErr = err
				}
			}
		}
		log.Printf("[%s] → corpRegistry.UpdateMembers corpID=%d (%d rows, %d refused)", addr, corpID, len(lines), failed)
		if firstErr != nil {
			return corpUserErr(c, pkt, accountID, memberUpdateRefusal(failed, len(lines), firstErr))
		}
		return done()

	case "ExecuteActions":
		// (targetIDs, actions=[[verb, property, value], ...]): the member search's mass edit. Each
		// target becomes one UpdateMember line, so every permission rule applies. The raw call is
		// logged until the shape is confirmed live.
		log.Printf("[%s] corpRegistry.ExecuteActions charID=%d targets=%+v actions=%+v", addr, charID, arg(0), arg(1))
		targets, okT := pySeq(arg(0))
		actions, okA := pySeq(arg(1))
		if !okT || !okA {
			return corpUserErr(c, pkt, accountID, "Malformed member action.")
		}
		failed, firstErr := 0, error(nil)
		for _, t := range targets {
			tp := pyOptInt(t)
			if tp == nil {
				continue
			}
			row, err := memberActionRow(ctx, sqldb, *tp, actions)
			if err == nil && row != nil {
				err = applyMemberUpdateRow(ctx, c, addr, charID, corpID, sqldb, row)
			}
			if err != nil {
				log.Printf("[%s] ExecuteActions target %d: %v", addr, *tp, err)
				failed++
				if firstErr == nil {
					firstErr = err
				}
			}
		}
		if firstErr != nil {
			return corpUserErr(c, pkt, accountID, memberUpdateRefusal(failed, len(targets), firstErr))
		}
		return done()

	case "InsertVoteCase":
		// (voteCaseText, description, corporationID, voteType, options, startDateTime, endDateTime).
		// Proposing is a Director's act in their own corporation; the client only hides the button.
		if !isDirectorOrCEO(ctx, sqldb, corpID, charID) {
			return corpUserErr(c, pkt, accountID, "Only directors can propose votes.")
		}
		text, _ := pyStr(arg(0))
		desc, _ := pyStr(arg(1))
		voteTypeP := pyOptInt(arg(3))
		startP := pyOptInt(arg(5))
		endP := pyOptInt(arg(6))
		if voteTypeP == nil || startP == nil || endP == nil || *endP <= *startP ||
			*voteTypeP < evedb.VoteCEO || *voteTypeP > evedb.VoteItemUnlock {
			return corpUserErr(c, pkt, accountID, "Malformed vote.")
		}
		if target := pyOptInt(arg(2)); target != nil && *target != corpID {
			return corpUserErr(c, pkt, accountID, "Votes can only be proposed in your own corporation.")
		}
		_, optLines, ok := parseRowsetArg(arg(4))
		if !ok || len(optLines) == 0 {
			return corpUserErr(c, pkt, accountID, "A vote needs options.")
		}
		var options []evedb.VoteCaseOption
		for _, ln := range optLines {
			var o evedb.VoteCaseOption
			if len(ln) >= 1 {
				o.Text, _ = pyStr(ln[0])
			}
			set := func(dst *sql.NullInt64, rep marshal.PyRep) {
				if v := pyOptInt(rep); v != nil {
					*dst = sql.NullInt64{Int64: *v, Valid: true}
				}
			}
			if len(ln) >= 2 {
				set(&o.Parameter, ln[1])
			}
			if len(ln) >= 3 {
				set(&o.Parameter1, ln[2])
			}
			if len(ln) >= 4 {
				set(&o.Parameter2, ln[3])
			}
			options = append(options, o)
		}
		if refusal := lockVoteRefusal(ctx, sqldb, corpID, *voteTypeP, options); refusal != "" {
			return clientMsgErr(c, pkt, accountID, refusal, nil)
		}
		caseID, err := evedb.InsertVoteCaseRow(ctx, sqldb, corpID, text, desc, *voteTypeP, *startP, *endP, options)
		if err != nil {
			log.Printf("[%s] InsertVoteCase: %v", addr, err)
			return corpUserErr(c, pkt, accountID, "Vote creation failed.")
		}
		// Add-form change: every vote-case column as (None, new).
		notifyCorp(sessReg, corpID, "OnCorporationVoteCaseChanged", marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(corpID)},
			marshal.PyInt{Value: uint64(caseID)},
			marshal.PyDict{Items: []marshal.DictEntry{
				chg("voteCaseID", none, marshal.PyInt{Value: uint64(caseID)}),
				chg("corporationID", none, marshal.PyInt{Value: uint64(corpID)}),
				chg("voteCaseText", none, marshal.PyString{Value: text}),
				chg("description", none, marshal.PyString{Value: desc}),
				chg("startDateTime", none, marshal.PyInt{Value: uint64(*startP)}),
				chg("endDateTime", none, marshal.PyInt{Value: uint64(*endP)}),
				chg("voteType", none, marshal.PyInt{Value: uint64(*voteTypeP)}),
			}},
		}})
		log.Printf("[%s] → corpRegistry.InsertVoteCase corpID=%d caseID=%d type=%d (%d options)",
			addr, corpID, caseID, *voteTypeP, len(options))
		return done()

	case "InsertVote":
		// (corporationID, voteCaseID, optionID). The case may belong to a corporation the voter only
		// holds shares in, so everything keys off the case; retallyShareVotes recomputes the tally.
		caseP := pyOptInt(arg(1))
		optP := pyOptInt(arg(2))
		if caseP == nil || optP == nil {
			return corpUserErr(c, pkt, accountID, "Malformed vote.")
		}
		w, werr := evedb.GetVoteCaseWindow(ctx, sqldb, *caseP)
		if werr != nil {
			return corpUserErr(c, pkt, accountID, "Unknown vote.")
		}
		if argCorp := pyOptInt(arg(0)); argCorp != nil && *argCorp != w.CorpID {
			return corpUserErr(c, pkt, accountID, "Unknown vote.")
		}
		if now := int64(server.Win32TimeNow()); now >= w.End {
			return clientMsgErr(c, pkt, accountID, "CorpVoteCaseClosed", nil)
		} else if now < w.Start {
			return corpUserErr(c, pkt, accountID, "That vote is not open yet.")
		}
		if !evedb.VoteOptionExists(ctx, sqldb, *caseP, *optP) {
			return corpUserErr(c, pkt, accountID, "Unknown vote option.")
		}
		if !evedb.CanCastVote(ctx, sqldb, charID, w.CorpID) {
			return corpUserErr(c, pkt, accountID, "Only shareholders and the CEO can vote.")
		}
		if err := evedb.InsertVoteRow(ctx, sqldb, *caseP, charID, *optP); err != nil {
			if errors.Is(err, evedb.ErrAlreadyVoted) {
				return clientMsgErr(c, pkt, accountID, "CrpAlreadyVoted", nil)
			}
			log.Printf("[%s] InsertVote: %v", addr, err)
			return corpUserErr(c, pkt, accountID, "Vote failed.")
		}
		voteChanged := marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(w.CorpID)},
			marshal.PyInt{Value: uint64(*caseP)},
			marshal.PyInt{Value: uint64(charID)},
			marshal.PyDict{Items: []marshal.DictEntry{
				chg("characterID", none, marshal.PyInt{Value: uint64(charID)}),
				chg("optionID", none, marshal.PyInt{Value: uint64(*optP)}),
			}},
		}}
		notifyCorp(sessReg, w.CorpID, "OnCorporationVoteChanged", voteChanged)
		if corpID != w.CorpID {
			server.NotifyCharacter(sessReg, charID, "OnCorporationVoteChanged", "clientID", voteChanged)
		}
		retallyShareVotes(ctx, sqldb, sessReg, w.CorpID)
		log.Printf("[%s] → corpRegistry.InsertVote corp=%d case=%d char=%d option=%d weight=%d",
			addr, w.CorpID, *caseP, charID, *optP, evedb.ShareVoteWeight(ctx, sqldb, charID, w.CorpID))
		return done()

	case "MoveCompanyShares", "MovePrivateShares":
		// (corporationID, toShareholderID, numberOfShares), the voucher's Give dialog. Company moves draw
		// on the caller's corporation, private moves on the caller; the recipient is any pilot or player corp.
		stockCorpP := pyOptInt(arg(0))
		toP := pyOptInt(arg(1))
		qtyP := pyOptInt(arg(2))
		if stockCorpP == nil || toP == nil || qtyP == nil || *qtyP <= 0 {
			return corpUserErr(c, pkt, accountID, "Malformed share transfer.")
		}
		stockCorp, toHolder := *stockCorpP, *toP
		fromHolder := charID
		if cs.Method == "MoveCompanyShares" {
			if !isDirectorOrCEO(ctx, sqldb, corpID, charID) &&
				!corp.HasRole(callerRoles(ctx, sqldb, charID), corp.RoleAccountant) {
				return corpUserErr(c, pkt, accountID, "You need the Accountant role to move corporation shares.")
			}
			fromHolder = corpID
		}
		if toHolder == fromHolder {
			return corpUserErr(c, pkt, accountID, "Those shares are already there.")
		}
		if evedb.ShareOwnerKind(ctx, sqldb, toHolder) == "" {
			return corpUserErr(c, pkt, accountID, "Shares can only be given to a pilot or a player corporation.")
		}
		move, err := evedb.MoveShares(ctx, sqldb, stockCorp, fromHolder, toHolder, *qtyP)
		if err != nil {
			if errors.Is(err, evedb.ErrNotEnoughShares) {
				return corpUserErr(c, pkt, accountID, fmt.Sprintf(
					"Only %d shares of that corporation are held there.", move.FromOld))
			}
			log.Printf("[%s] corpRegistry.%s: %v", addr, cs.Method, err)
			return corpUserErr(c, pkt, accountID, "Share transfer failed.")
		}
		announceShareChange(ctx, sqldb, sessReg, stockCorp, move.Deltas(fromHolder, toHolder), 0, 0)
		if esp.ShareMovedHook != nil {
			esp.ShareMovedHook(stockCorp, fromHolder, toHolder, *qtyP)
		}
		log.Printf("[%s] → corpRegistry.%s stock=%d %d→%d qty=%d (sender %d→%d, recipient %d→%d)",
			addr, cs.Method, stockCorp, fromHolder, toHolder, *qtyP, move.FromOld, move.FromNew, move.ToOld, move.ToNew)
		return done()

	case "GetLockedItemsByLocation":
		locP := pyOptInt(arg(0))
		if locP == nil {
			return corpUserErr(c, pkt, accountID, "No location given.")
		}
		log.Printf("[%s] → corpRegistry.GetLockedItemsByLocation corpID=%d loc=%d", addr, corpID, *locP)
		return server.SendCallRsp(c, pkt, accountID,
			evedb.GetLockedItemsAtLocation(ctx, sqldb, corpID, *locP))

	case "UpdateSanctionedAction":
		// (voteCaseID, inEffect, reason): the active CEO implements a passed vote (wars, shares, kick, lockdown).
		if !isCorpCEO(ctx, sqldb, corpID, charID) {
			return corpUserErr(c, pkt, accountID, "Only the active CEO can implement sanctioned actions.")
		}
		caseP := pyOptInt(arg(0))
		if caseP == nil {
			return corpUserErr(c, pkt, accountID, "No vote case given.")
		}
		caseCorp, voteType, acted, err := evedb.GetVoteCaseInfo(ctx, sqldb, *caseP)
		if err != nil || caseCorp != corpID {
			return corpUserErr(c, pkt, accountID, "Unknown vote case.")
		}
		if acted {
			return corpUserErr(c, pkt, accountID, "That action has already been implemented.")
		}
		// A result exists only once voting has closed; until then the tally moves with every ballot.
		if w, werr := evedb.GetVoteCaseWindow(ctx, sqldb, *caseP); werr != nil ||
			w.End > int64(server.Win32TimeNow()) {
			return corpUserErr(c, pkt, accountID, "Voting on that case has not closed yet.")
		}
		optID, param, p1, p2, ok := evedb.WinningOption(ctx, sqldb, *caseP)
		if !ok || !param.Valid || param.Int64 == 0 {
			return corpUserErr(c, pkt, accountID, "The vote did not pass.")
		}
		_ = p1
		switch voteType {
		case evedb.VoteWar:
			// The winning option's parameter is the target ownerID. This is the only way a lone corporation
			// declares war; declareWar refuses an allied corporation, so a stale war vote dies here.
			war, werr := declareWar(ctx, sqldb, sessReg, corpID, corpID, param.Int64, false)
			if werr != nil {
				return refuseErr(c, pkt, accountID, werr)
			}
			log.Printf("[%s] → vote-sanctioned war: corp=%d against=%d war=%d", addr, corpID, param.Int64, war.WarID)
		case evedb.VoteShares:
			// The new shares land in the treasury, where management can
			// hand them out through the corp wallet's voucher.
			issue, ierr := evedb.IssueCorpShares(ctx, sqldb, corpID, param.Int64)
			if ierr != nil {
				log.Printf("[%s] vote-sanctioned share issue corp=%d: %v", addr, corpID, ierr)
				return corpUserErr(c, pkt, accountID, "Share creation failed.")
			}
			announceShareChange(ctx, sqldb, sessReg, corpID,
				[]evedb.ShareDelta{{HolderID: corpID, Old: issue.OldTreasury, New: issue.NewTreasury}},
				issue.OldTotal, issue.NewTotal)
		case evedb.VoteKickMember:
			target := param.Int64
			if isCorpCEO(ctx, sqldb, corpID, target) {
				return corpUserErr(c, pkt, accountID, "The CEO cannot be expelled.")
			}
			conn, _ := charConnLookup(target)
			if kickToNPCCorp(ctx, sqldb, sessReg, target, corpID, conn) == 0 {
				return corpUserErr(c, pkt, accountID, "Expulsion failed.")
			}
		case evedb.VoteItemLockdown:
			// The option is [title, itemID, typeID, stationID]; the item is re-checked because thirty days
			// can pass between the vote and its implementation.
			loc := int64(0)
			if p2.Valid {
				loc = p2.Int64
			}
			li, refusal, err := evedb.LockItem(ctx, sqldb, corpID, param.Int64, loc)
			if err != nil {
				log.Printf("[%s] vote-sanctioned lockdown corp=%d item=%d: %v", addr, corpID, param.Int64, err)
				return corpUserErr(c, pkt, accountID, "Lockdown failed.")
			}
			if refusal != "" {
				return clientMsgErr(c, pkt, accountID, refusal, nil)
			}
			pushLockedItemChange(sessReg, li, true)
		case evedb.VoteItemUnlock:
			li, refusal, err := evedb.UnlockItem(ctx, sqldb, corpID, param.Int64)
			if err != nil {
				log.Printf("[%s] vote-sanctioned unlock corp=%d item=%d: %v", addr, corpID, param.Int64, err)
				return corpUserErr(c, pkt, accountID, "Unlock failed.")
			}
			if refusal != "" {
				return clientMsgErr(c, pkt, accountID, refusal, nil)
			}
			pushLockedItemChange(sessReg, li, false)
		default:
			return corpUserErr(c, pkt, accountID, "That sanctioned action type is not supported on this server yet.")
		}
		now := int64(server.Win32TimeNow())
		_ = evedb.MarkSanctionedActionDone(ctx, sqldb, *caseP, now)
		notifyCorp(sessReg, corpID, "OnSanctionedActionChanged", marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(corpID)},
			marshal.PyInt{Value: uint64(*caseP)},
			marshal.PyDict{Items: []marshal.DictEntry{
				chg("inEffect", marshal.PyInt{Value: 0}, marshal.PyInt{Value: 1}),
				chg("actedUpon", marshal.PyInt{Value: 0}, marshal.PyInt{Value: 1}),
				chg("timeActedUpon", marshal.PyInt{Value: 0}, marshal.PyInt{Value: uint64(now)}),
			}},
		}})
		log.Printf("[%s] → corpRegistry.UpdateSanctionedAction case=%d type=%d option=%d", addr, *caseP, voteType, optID)
		return done()

	case "UpdateApplicationOffer":
		// (characterID, applicationText, status, applicationDateTime): the corp side accepting,
		// rejecting or renegotiating. The join happens later, when the applicant accepts.
		if !isDirectorOrCEO(ctx, sqldb, corpID, charID) &&
			!corp.HasRole(callerRoles(ctx, sqldb, charID), corp.RolePersonnelManager) {
			return corpUserErr(c, pkt, accountID, "You need the Personnel Manager role to handle applications.")
		}
		applicantP := pyOptInt(arg(0))
		text, _ := pyStr(arg(1))
		statusP := pyOptInt(arg(2))
		if applicantP == nil || statusP == nil {
			return corpUserErr(c, pkt, accountID, "Malformed application update.")
		}
		oldStatus := evedb.GetApplicationStatus(ctx, sqldb, *applicantP, corpID)
		// The member limit is enforced at the offer and again at the applicant's accept (the corp may
		// fill meanwhile). Members over the limit are untouched (evedb.CorpHasOpening).
		if *statusP == evedb.AppAcceptedByCorporation {
			if ok, used, limit, err := evedb.CorpHasOpening(ctx, sqldb, corpID); err == nil && !ok {
				return clientMsgErr(c, pkt, accountID, "CorporationMembershipFull", map[string]marshal.PyRep{
					"limit": marshal.PyInt{Value: uint64(limit)},
					"count": marshal.PyInt{Value: uint64(used)},
				})
			}
		}
		if err := evedb.UpdateApplication(ctx, sqldb, *applicantP, corpID, charID, *statusP, text); err != nil {
			log.Printf("[%s] UpdateApplicationOffer: %v", addr, err)
			return corpUserErr(c, pkt, accountID, "Application update failed.")
		}
		if evedb.IsTerminalAppStatus(*statusP) {
			// Terminal state: soft-delete and send REMOVE so the client drops the cached row.
			_ = evedb.SoftDeleteApplication(ctx, sqldb, *applicantP, corpID)
			rem := appRemoveNotification(corpID, *applicantP, oldStatus)
			notifyCorp(sessReg, corpID, "OnCorporationApplicationChanged", rem)
			server.NotifyCharacter(sessReg, *applicantP, "OnCorporationApplicationChanged", "charid", rem)
		} else {
			// Non-terminal (e.g. AcceptedByCorporation): status UPDATE. Arg order per bco_applications.py.
			appChange := marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyInt{Value: uint64(*applicantP)},
				marshal.PyInt{Value: uint64(corpID)},
				marshal.PyDict{Items: []marshal.DictEntry{
					chg("status", marshal.PyInt{Value: uint64(oldStatus)}, marshal.PyInt{Value: uint64(*statusP)}),
					chg("lastCorpUpdaterID", none, marshal.PyInt{Value: uint64(charID)}),
				}},
			}}
			notifyCorp(sessReg, corpID, "OnCorporationApplicationChanged", appChange)
			server.NotifyCharacter(sessReg, *applicantP, "OnCorporationApplicationChanged", "charid", appChange)
		}
		log.Printf("[%s] → corpRegistry.UpdateApplicationOffer corpID=%d applicant=%d status=%d",
			addr, corpID, *applicantP, *statusP)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyInt{Value: 1})
	}

	return corpUserErr(c, pkt, accountID, "Unhandled corporation method.")
}

// applyMemberUpdate handles a full UpdateMember argument vector (also used
// per-row by UpdateMembers via applyMemberUpdateRow).  A self-edit answers
// with the session-change notification PerformSessionChange waits for.
func applyMemberUpdate(ctx context.Context, c *evenet.Conn, addr string, pkt *evenet.PyPacket, accountID, callerID, corpID int64, sqldb *sql.DB, sessReg *esp.SessionRegistry, args []marshal.PyRep) error {
	if err := applyMemberUpdateRow(ctx, c, addr, callerID, corpID, sqldb, args); err != nil {
		log.Printf("[%s] corpRegistry.UpdateMember: %v", addr, err)
		return corpUserErr(c, pkt, accountID, err.Error())
	}
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// restrictToHeldMasks refuses a member update that would add a role bit the character does not
// already hold. Every mask is checked, the title mask included, since a title is a role bundle.
func restrictToHeldMasks(ctx context.Context, sqldb *sql.DB, charID int64, u *evedb.CorpMemberUpdate) error {
	var held [9]uint64
	err := sqldb.QueryRowContext(ctx, `
		SELECT COALESCE(corprole, 0), COALESCE(grantableRoles, 0),
		       COALESCE(rolesAtHQ, 0), COALESCE(grantableRolesAtHQ, 0),
		       COALESCE(rolesAtBase, 0), COALESCE(grantableRolesAtBase, 0),
		       COALESCE(rolesAtOther, 0), COALESCE(grantableRolesAtOther, 0),
		       COALESCE(titleMask, 0)
		FROM chrCorporationRoles WHERE characterID = ?`, charID,
	).Scan(&held[0], &held[1], &held[2], &held[3], &held[4], &held[5], &held[6], &held[7], &held[8])
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("could not read your current roles")
	}
	// sql.ErrNoRows leaves held all-zero: a member with no role row holds nothing and may set nothing.
	fields := [9]*int64{
		u.Roles, u.GrantableRoles,
		u.RolesAtHQ, u.GrantableRolesAtHQ,
		u.RolesAtBase, u.GrantableRolesAtBase,
		u.RolesAtOther, u.GrantableRolesAtOther,
		u.TitleMask,
	}
	for i, f := range fields {
		if f == nil {
			continue
		}
		if uint64(*f)&^held[i] != 0 {
			return fmt.Errorf("you cannot grant yourself roles you do not hold")
		}
	}
	return nil
}

// memberMasks is a member's stored role row: held and grantable roles in the order corporation-wide
// (roles), at HQ, at Base, at Other.
type memberMasks struct {
	held, grant [4]uint64
	title       uint64
	baseID      int64
}

// grants reports whether the member holds any role as grantable.
func (mm memberMasks) grants() bool {
	return mm.grant != [4]uint64{}
}

func readMemberMasks(ctx context.Context, sqldb *sql.DB, charID int64) (memberMasks, error) {
	var mm memberMasks
	err := sqldb.QueryRowContext(ctx, `
		SELECT COALESCE(corprole, 0), COALESCE(rolesAtHQ, 0), COALESCE(rolesAtBase, 0), COALESCE(rolesAtOther, 0),
		       COALESCE(grantableRoles, 0), COALESCE(grantableRolesAtHQ, 0),
		       COALESCE(grantableRolesAtBase, 0), COALESCE(grantableRolesAtOther, 0),
		       COALESCE(titleMask, 0), COALESCE(baseID, 0)
		  FROM chrCorporationRoles WHERE characterID = ?`, charID,
	).Scan(&mm.held[0], &mm.held[1], &mm.held[2], &mm.held[3],
		&mm.grant[0], &mm.grant[1], &mm.grant[2], &mm.grant[3], &mm.title, &mm.baseID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return mm, fmt.Errorf("could not read the member's roles")
	}
	return mm, nil
}

// restrictToGrantable refuses a member update by a grantor who is neither CEO nor Director unless
// every role it adds or removes is one the grantor holds as grantable for that column. Roles at
// Base are grantable only between members sharing a Base. Grantable masks, titles and the Base
// stay a Director's to change. The Director role is never grantable. Pure.
func restrictToGrantable(grantor, grantee memberMasks, u *evedb.CorpMemberUpdate) error {
	grant := grantor.grant
	if grantor.baseID == 0 || grantor.baseID != grantee.baseID {
		grant[2] = 0
	}
	for i := range grant {
		grant[i] &^= corp.RoleDirector
	}
	held := [4]*int64{u.Roles, u.RolesAtHQ, u.RolesAtBase, u.RolesAtOther}
	for i, f := range held {
		if f == nil {
			continue
		}
		if (uint64(*f)^grantee.held[i])&^grant[i] != 0 {
			return fmt.Errorf("you may only grant or revoke roles you hold as grantable")
		}
	}
	grantable := [4]*int64{u.GrantableRoles, u.GrantableRolesAtHQ, u.GrantableRolesAtBase, u.GrantableRolesAtOther}
	for i, f := range grantable {
		if f != nil && uint64(*f) != grantee.grant[i] {
			return fmt.Errorf("only a Director may change which roles a member can grant")
		}
	}
	if u.TitleMask != nil && uint64(*u.TitleMask) != grantee.title {
		return fmt.Errorf("only a Director may change a member's titles")
	}
	if u.BaseID != nil && *u.BaseID != grantee.baseID {
		return fmt.Errorf("only a Director may change a member's base")
	}
	return nil
}

// applyMemberUpdateRow validates and persists one member-update line.
func applyMemberUpdateRow(ctx context.Context, c *evenet.Conn, addr string, callerID, corpID int64, sqldb *sql.DB, row []marshal.PyRep) error {
	if len(row) < 1 {
		return fmt.Errorf("empty member update")
	}
	at := func(i int) marshal.PyRep {
		if i < len(row) {
			return row[i]
		}
		return nil
	}
	targetP := pyOptInt(at(0))
	if targetP == nil {
		return fmt.Errorf("no member specified")
	}
	target := *targetP
	// Membership check: the target must be in the caller's corp.
	var targetCorp int64
	if err := sqldb.QueryRowContext(ctx,
		`SELECT corporationID FROM character_ WHERE characterID = ?`, target).Scan(&targetCorp); err != nil || targetCorp != corpID {
		return fmt.Errorf("that character is not in your corporation")
	}
	isDirector := isDirectorOrCEO(ctx, sqldb, corpID, callerID)
	// A member who is neither CEO nor Director may edit another member only through the roles it
	// holds as grantable (restrictToGrantable), and never the CEO's or a Director's row.
	var grantor, grantee memberMasks
	if target != callerID && !isDirector {
		var err error
		if grantor, err = readMemberMasks(ctx, sqldb, callerID); err != nil {
			return err
		}
		if !grantor.grants() {
			return fmt.Errorf("only a Director, or a member holding grantable roles, may edit other members")
		}
		if grantee, err = readMemberMasks(ctx, sqldb, target); err != nil {
			return err
		}
		if isCorpCEO(ctx, sqldb, corpID, target) || grantee.held[0]&corp.RoleDirector != 0 {
			return fmt.Errorf("only the CEO may edit the roles of the CEO or a Director")
		}
	}
	u := &evedb.CorpMemberUpdate{}
	if s, ok := pyStr(at(1)); ok && isDirector {
		u.Title = &s
	}
	u.DivisionID = pyOptInt(at(2))
	u.SquadronID = pyOptInt(at(3))
	u.Roles = pyOptInt(at(4))
	u.GrantableRoles = pyOptInt(at(5))
	u.RolesAtHQ = pyOptInt(at(6))
	u.GrantableRolesAtHQ = pyOptInt(at(7))
	u.RolesAtBase = pyOptInt(at(8))
	u.GrantableRolesAtBase = pyOptInt(at(9))
	u.RolesAtOther = pyOptInt(at(10))
	u.GrantableRolesAtOther = pyOptInt(at(11))
	u.BaseID = pyOptInt(at(12))
	u.TitleMask = pyOptInt(at(13))
	// A Base is one of the corporation's offices or its HQ. 0 is "no Base"; the client sends None
	// for an unset combo, which pyOptInt reads as no change, so ReleaseOffice is what clears one.
	if u.BaseID != nil && *u.BaseID != 0 && !evedb.IsCorpBaseStation(ctx, sqldb, corpID, *u.BaseID) {
		return fmt.Errorf("a member's base must be one of the corporation's offices")
	}
	// Granting or revoking the Director role is CEO-only; self-demotion (roles=0 from the
	// remove-all menu) is always allowed. A line that leaves the bit as it is (a Director's
	// hangar roles edited) is no Director change.
	if u.Roles != nil && !isCorpCEO(ctx, sqldb, corpID, callerID) {
		var held uint64
		_ = sqldb.QueryRowContext(ctx,
			`SELECT COALESCE(corprole, 0) FROM chrCorporationRoles WHERE characterID = ?`, target).Scan(&held)
		next := uint64(*u.Roles)
		if (next^held)&corp.RoleDirector != 0 && !(target == callerID && next&corp.RoleDirector == 0) {
			return fmt.Errorf("only the CEO may grant or revoke the Director role")
		}
	}
	// A non-Director edits itself only downward, and another member only within its grantable
	// roles. The client's gate (corputil.CanEditRole) is client-side, so a hand-built UpdateMember
	// would otherwise grant roles.
	if !isDirector {
		var err error
		if target == callerID {
			err = restrictToHeldMasks(ctx, sqldb, target, u)
		} else {
			err = restrictToGrantable(grantor, grantee, u)
		}
		if err != nil {
			return err
		}
	}
	oldInfo, _ := evedb.GetCorporationMemberInfo(ctx, sqldb, target)
	oldLine, _ := evedb.GetCorpMemberLine(ctx, sqldb, corpID, target)
	if err := evedb.UpdateCorpMemberRow(ctx, sqldb, target, u); err != nil {
		return err
	}
	// The member list and role window redraw from their cache; without this the edit looks
	// undone until the window is reopened.
	notifyMemberRowChanged(ctx, sqldb, corpID, target, oldLine)
	// The target hears about their own new mask whoever made the edit; a self-edit is forced
	// because it arrives inside the client's PerformSessionChange.
	pushMemberRoleSession(ctx, sqldb, target, oldInfo, target == callerID && u.Roles != nil)
	if h := esp.MemberBaseChangedHook; h != nil && u.BaseID != nil {
		var oldBase int64
		if oldInfo != nil {
			oldBase = oldInfo.BaseID
		}
		if oldBase != *u.BaseID {
			h(corpID, target, callerID, oldBase, *u.BaseID)
		}
	}
	log.Printf("[%s] → corpRegistry member update target=%d by=%d", addr, target, callerID)
	return nil
}

// memberUpdateRefusal is the dialog text for a batch of member lines some of which were refused.
func memberUpdateRefusal(failed, total int, first error) string {
	if total <= 1 {
		return first.Error()
	}
	return fmt.Sprintf("%d of %d member updates were refused: %v", failed, total, first)
}

// notifyMemberRowChanged pushes OnCorporationMemberChanged(charID, {col: (old, new)}) to the
// corporation for every member column that differs from before (the line read before the
// write; nil sends every column). There is no corporationID entry, so the client reads it as an
// edit of a member it already lists, not a join or a leave (corpmembership.go).
func notifyMemberRowChanged(ctx context.Context, sqldb *sql.DB, corpID, charID int64, before []marshal.PyRep) {
	if globalSessReg == nil || sqldb == nil {
		return
	}
	after, err := evedb.GetCorpMemberLine(ctx, sqldb, corpID, charID)
	if err != nil || after == nil {
		return
	}
	var entries []marshal.DictEntry
	for i, col := range evedb.CorpMemberColumns() {
		if col == "characterID" || col == "corporationID" || i >= len(after) {
			continue
		}
		var oldV, newV marshal.PyRep = marshal.PyNone{}, marshal.PyNone{}
		if after[i] != nil {
			newV = after[i]
		}
		if i < len(before) && before[i] != nil {
			oldV = before[i]
		}
		if before != nil && reflect.DeepEqual(oldV, newV) {
			continue
		}
		entries = append(entries, chg(col, oldV, newV))
	}
	if len(entries) == 0 {
		return
	}
	notifyCorp(globalSessReg, corpID, "OnCorporationMemberChanged", marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyInt{Value: uint64(charID)}, marshal.PyDict{Items: entries},
	}})
}

// pySeq reads a list or tuple argument.
func pySeq(rep marshal.PyRep) ([]marshal.PyRep, bool) {
	switch v := rep.(type) {
	case marshal.PyList:
		return v.Items, true
	case marshal.PyTuple:
		return v.Items, true
	}
	return nil, false
}

// The member search's mass-edit verbs (eveconstants CTV_*). GIVE and COMMS are not member edits.
const (
	ctvAdd    = 1
	ctvRemove = 2
	ctvSet    = 3
)

// memberActionCols maps an ExecuteActions property to its UpdateMember argument slot.
var memberActionCols = map[string]int{
	"roles": 4, "grantableRoles": 5,
	"rolesAtHQ": 6, "grantableRolesAtHQ": 7,
	"rolesAtBase": 8, "grantableRolesAtBase": 9,
	"rolesAtOther": 10, "grantableRolesAtOther": 11,
	"baseID": 12, "titleMask": 13,
}

// memberActionRow turns ExecuteActions' verbs for one member into an UpdateMember line over the
// member's stored masks; nil when no action touches a member field.
func memberActionRow(ctx context.Context, sqldb *sql.DB, target int64, actions []marshal.PyRep) ([]marshal.PyRep, error) {
	var cur [14]uint64
	err := sqldb.QueryRowContext(ctx, `
		SELECT COALESCE(corprole, 0), COALESCE(grantableRoles, 0),
		       COALESCE(rolesAtHQ, 0), COALESCE(grantableRolesAtHQ, 0),
		       COALESCE(rolesAtBase, 0), COALESCE(grantableRolesAtBase, 0),
		       COALESCE(rolesAtOther, 0), COALESCE(grantableRolesAtOther, 0),
		       COALESCE(baseID, 0), COALESCE(titleMask, 0)
		  FROM chrCorporationRoles WHERE characterID = ?`, target,
	).Scan(&cur[4], &cur[5], &cur[6], &cur[7], &cur[8], &cur[9], &cur[10], &cur[11], &cur[12], &cur[13])
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("could not read the member's roles")
	}
	row := make([]marshal.PyRep, 14)
	row[0] = marshal.PyInt{Value: uint64(target)}
	touched := false
	for _, a := range actions {
		parts, ok := pySeq(a)
		if !ok || len(parts) < 3 {
			continue
		}
		verb := pyOptInt(parts[0])
		prop, okP := pyStr(parts[1])
		if verb == nil || !okP {
			continue
		}
		if prop == "title" && *verb == ctvSet {
			if _, ok := pyStr(parts[2]); ok {
				row[1], touched = parts[2], true
			}
			continue
		}
		slot, known := memberActionCols[prop]
		val := pyOptInt(parts[2])
		if !known || val == nil {
			continue
		}
		v := uint64(*val)
		switch *verb {
		case ctvAdd:
			cur[slot] |= v
		case ctvRemove:
			cur[slot] &^= v
		case ctvSet:
			cur[slot] = v
		default:
			continue
		}
		row[slot], touched = marshal.PyInt{Value: cur[slot]}, true
	}
	if !touched {
		return nil, nil
	}
	return row, nil
}
