package main

// Bills: the wire behind the wallet's Bills tabs, plus the recurring sweeps that issue them.
//
// billMgr (unbound): GetBillTypes, CharGetBills, CharGetBillsReceivable, GetCorporationBills,
// GetCorporationBillsReceivable, CharPayBill, PayCorporationBill. allianceRegistry bound
// object: GetBills, GetBillsReceivable, PayBill, GetBillBalance (handleAllianceBound).
//
// The creditor follows the bill type; see docs/corp-notes.md#bills. OnBillReceived ignores its
// arguments, so the push is an empty tuple sent to the debtor's pilots.

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

	"gurimoon/corp"
	evedb "gurimoon/db"
	"gurimoon/esp"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
)

// handleBillMgrReal serves the billMgr service from the bills table.
func handleBillMgrReal(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket,
	accountID, charID int64, sqldb *sql.DB, sessReg *esp.SessionRegistry) error {

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	argInt := func(i int) int64 {
		if i < len(cs.ArgTuple.Items) {
			if v, ok := cs.ArgTuple.Items[i].(marshal.PyInt); ok {
				return int64(v.Value)
			}
		}
		return 0
	}
	emptyBills := func() marshal.PyRep {
		r, _ := evedb.GetBillsRowset(ctx, sqldb, 0, true)
		return r
	}

	switch cs.Method {

	case "GetBillTypes":
		// util.Rowset, non-empty: svc.wallet's Run() calls .Index on it at login.
		rowset, err := evedb.GetBillTypesRowset(ctx, sqldb)
		if err != nil {
			log.Printf("[%s] billMgr.GetBillTypes: %v", addr, err)
			return server.SendCallException(c, pkt, accountID, nil)
		}
		log.Printf("[%s] → billMgr.GetBillTypes", addr)
		return server.SendCallRsp(c, pkt, accountID, rowset)

	case "CharGetBills", "CharGetBillsPayable":
		rowset, err := evedb.GetBillsRowset(ctx, sqldb, charID, true)
		if err != nil {
			log.Printf("[%s] billMgr.%s: %v", addr, cs.Method, err)
			rowset = emptyBills()
		}
		log.Printf("[%s] → billMgr.%s charID=%d", addr, cs.Method, charID)
		return server.SendCallRsp(c, pkt, accountID, rowset)

	case "CharGetBillsReceivable":
		rowset, err := evedb.GetBillsRowset(ctx, sqldb, charID, false)
		if err != nil {
			rowset = emptyBills()
		}
		log.Printf("[%s] → billMgr.CharGetBillsReceivable charID=%d", addr, charID)
		return server.SendCallRsp(c, pkt, accountID, rowset)

	case "GetCorporationBills", "CorpGetBills", "CorpGetBillsPayable", "GetBillsPayable", "GetBills":
		corpID, _ := allianceCallerCorp(ctx, sqldb, charID)
		rowset, err := evedb.GetBillsRowset(ctx, sqldb, corpID, true)
		if err != nil {
			rowset = emptyBills()
		}
		log.Printf("[%s] → billMgr.%s corpID=%d", addr, cs.Method, corpID)
		return server.SendCallRsp(c, pkt, accountID, rowset)

	case "GetCorporationBillsReceivable", "CorpGetBillsReceivable", "GetBillsReceivable":
		corpID, _ := allianceCallerCorp(ctx, sqldb, charID)
		rowset, err := evedb.GetBillsRowset(ctx, sqldb, corpID, false)
		if err != nil {
			rowset = emptyBills()
		}
		log.Printf("[%s] → billMgr.%s corpID=%d", addr, cs.Method, corpID)
		return server.SendCallRsp(c, pkt, accountID, rowset)

	case "CharPayBill":
		billID := argInt(0)
		bill, err := evedb.PayBill(ctx, sqldb, billID, charID)
		if err != nil {
			log.Printf("[%s] billMgr.CharPayBill %d: %v", addr, billID, err)
			return corpUserErr(c, pkt, accountID, err.Error())
		}
		announceBillPaid(ctx, sqldb, sessReg, bill)
		// PayBill moves ISK and OnBillReceived carries none, so push the balance or the client's
		// cached wealth keeps the pre-payment figure.
		if bal, berr := evedb.GetCharacterBalance(ctx, sqldb, charID); berr == nil {
			server.PushCashBalance(c, accountID, charID, bal)
		}
		log.Printf("[%s] → billMgr.CharPayBill %d paid by charID=%d", addr, billID, charID)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})

	case "PayCorporationBill":
		// The client shows the Pay button to Accountants; the server enforces
		// it, plus directors and the CEO, who outrank the accountant.
		corpID, _ := allianceCallerCorp(ctx, sqldb, charID)
		roles := callerRoles(ctx, sqldb, charID)
		mayPay := isCorpCEO(ctx, sqldb, corpID, charID) ||
			corp.HasRole(roles, corp.RoleDirector) ||
			corp.HasRole(roles, 256) // corpRoleAccountant
		if !mayPay {
			return corpUserErr(c, pkt, accountID, "Only an Accountant or Director may pay corporation bills.")
		}
		billID := argInt(0)
		// The scope check comes before the payment: PayBill debits the bill's debtor, so another
		// corporation's bill would spend their wallet.
		pending, err := evedb.GetBill(ctx, sqldb, billID)
		if err != nil || pending == nil {
			return corpUserErr(c, pkt, accountID, "No such bill.")
		}
		if pending.DebtorID != corpID {
			return corpUserErr(c, pkt, accountID, "That bill is not addressed to your corporation.")
		}
		bill, err := evedb.PayBill(ctx, sqldb, billID, 0)
		if err != nil {
			log.Printf("[%s] billMgr.PayCorporationBill %d: %v", addr, billID, err)
			return corpUserErr(c, pkt, accountID, err.Error())
		}
		announceBillPaid(ctx, sqldb, sessReg, bill)
		log.Printf("[%s] → billMgr.PayCorporationBill %d paid by corp %d", addr, billID, corpID)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}

	log.Printf("[%s] → billMgr.%s (None)", addr, cs.Method)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// pushBillReceived blinks the debtor's wallet: every online pilot for a corp
// debtor, the one pilot for a character debtor.  Arguments are ignored by the
// client's OnBillReceived handler.
func pushBillReceived(sessReg *esp.SessionRegistry, debtorID int64) {
	if sessReg == nil {
		return
	}
	args := marshal.PyTuple{Items: []marshal.PyRep{}}
	if s := sessReg.FindByCharID(debtorID); s != nil {
		if s.Conn != nil {
			if err := server.SendNotification(s.Conn, s.AccountID, "OnBillReceived", "charid", args); err != nil {
				log.Printf("OnBillReceived to char %d: %v", debtorID, err)
			}
		}
		return
	}
	notifyCorp(sessReg, debtorID, "OnBillReceived", args)
}

// announceBillPaid refreshes both wallets after a payment: the debtor's pilots get
// OnBillReceived (the open bills tab refetches on it), and so do the creditor's, since a paid
// receivable leaves their list.
func announceBillPaid(ctx context.Context, sqldb *sql.DB, sessReg *esp.SessionRegistry, bill *evedb.Bill) {
	if bill == nil {
		return
	}
	pushBillReceived(sessReg, bill.DebtorID)
	pushBillReceived(sessReg, bill.CreditorID)
}

// ── Recurring sweeps ─────────────────────────────────────────────────────────

// sweepRecurringBills issues the periodic non-war bills: the weekly alliance
// maintenance fee (CONCORD's, per member corporation, the client's
// const.allianceMembershipCost) and the 30-day office rent (the station
// owner's).  War bills ride the war sweep, which also enforces them.
func sweepRecurringBills(sqldb *sql.DB, sessReg *esp.SessionRegistry) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	graceTicks := uint64(evedb.BillGraceDays(ctx, sqldb) * 24 * 3600 * 10000000)
	week := uint64(7) * 24 * 3600 * 10000000
	month := uint64(30) * 24 * 3600 * 10000000

	// Alliance maintenance: 2M ISK per member corp per week, to CONCORD.
	due, err := evedb.AlliancesDueForMaintenance(ctx, sqldb)
	if err != nil {
		log.Printf("bill sweep alliances: %v", err)
	}
	for _, d := range due {
		bill, created, err := evedb.CreateBill(ctx, sqldb, evedb.Bill{
			BillTypeID:  evedb.BillTypeAllianceMaintainance,
			DebtorID:    d.CorporationID,
			CreditorID:  evedb.OwnerCONCORDBills,
			Amount:      allianceMembershipCost,
			DueDateTime: int64(server.Win32TimeNow() + graceTicks),
			ExternalID:  d.AllianceID,
			ExternalID2: -1,
		}, week)
		if err != nil || !created {
			continue
		}
		pushBillReceived(sessReg, d.CorporationID)
		log.Printf("alliance %d: maintenance bill %d (%.0f ISK) issued to corp %d",
			d.AllianceID, bill.BillID, allianceMembershipCost, d.CorporationID)
	}

	// Office rent: 30-day lease per crpoffices row, to the station owner, at the OfficeRentQuoteFor
	// amount RentOffice charged up front.
	offices, err := evedb.OfficesDueForRent(ctx, sqldb)
	if err != nil {
		log.Printf("bill sweep offices: %v", err)
	}
	for _, o := range offices {
		if o.StationOwner == 0 || o.StationOwner == o.CorporationID {
			continue // a corp does not bill itself for its own station
		}
		rent := evedb.OfficeRentQuoteFor(ctx, sqldb, o.StationID, o.CorporationID)
		bill, created, err := evedb.CreateBill(ctx, sqldb, evedb.Bill{
			BillTypeID:  evedb.BillTypeRentalBill,
			DebtorID:    o.CorporationID,
			CreditorID:  o.StationOwner,
			Amount:      rent,
			DueDateTime: int64(server.Win32TimeNow() + graceTicks),
			ExternalID:  evedb.TypeOfficeFolder,
			ExternalID2: o.StationID,
		}, month)
		if err != nil || !created {
			continue
		}
		pushBillReceived(sessReg, o.CorporationID)
		log.Printf("office rent bill %d (%.0f ISK): corp %d at station %d, to owner corp %d",
			bill.BillID, rent, o.CorporationID, o.StationID, o.StationOwner)
	}

	// Eviction: a rental bill unpaid past its due date (grace included) repossesses the office;
	// contents are impounded on the station floor and recoverable via the corp-junk path.
	overdue, err := evedb.OverdueUnpaidBills(ctx, sqldb, evedb.BillTypeRentalBill)
	if err != nil {
		log.Printf("bill sweep evictions: %v", err)
		overdue = nil
	}
	for _, b := range overdue {
		// Only office rentals evict: a rental bill's externalID names the folder type (26 office,
		// 28 factory), and a factory bill must not tear an office down.
		if b.ExternalID != evedb.TypeOfficeFolder {
			continue
		}
		if b.ExternalID2 <= 0 {
			_ = evedb.WriteOffBill(ctx, sqldb, b.BillID)
			continue
		}
		officeID, _, contents, err := evedb.ReleaseOffice(ctx, sqldb, b.DebtorID, b.ExternalID2)
		if err != nil {
			log.Printf("evict corp %d station %d: %v", b.DebtorID, b.ExternalID2, err)
			continue
		}
		_ = evedb.WriteOffBill(ctx, sqldb, b.BillID)
		if officeID == 0 {
			continue // office already gone (unrented meanwhile)
		}
		pushOfficeRentalChanged(sessReg, b.DebtorID, b.ExternalID2,
			marshal.PyNone{}, marshal.PyNone{})
		notifyEvictedCorp(ctx, sqldb, b, contents)
		evedb.EmitWorldEvent(evedb.OfficeEvictedEvent{At: time.Now(), CorpID: b.DebtorID, StationID: b.ExternalID2})
		log.Printf("office EVICTED: corp %d at station %d (bill %d unpaid, %d item(s) impounded)",
			b.DebtorID, b.ExternalID2, b.BillID, contents)
	}

	expelUnpaidMaintenance(ctx, sqldb, sessReg)
}

// expelUnpaidMaintenance removes a member corporation whose alliance maintenance bill went
// unpaid past its due date, through the Kick path (removeCorpFromAlliance). Gated by
// server_settings alliance.expelUnpaidMaintenance (default off); only bills due after the
// switch count. The bill is written off either way.
func expelUnpaidMaintenance(ctx context.Context, sqldb *sql.DB, sessReg *esp.SessionRegistry) {
	since, on := evedb.AllianceExpelUnpaidSince(ctx, sqldb)
	if !on {
		return
	}
	overdue, err := evedb.OverdueMaintenanceBills(ctx, sqldb, since)
	if err != nil {
		log.Printf("bill sweep maintenance: %v", err)
		return
	}
	for _, b := range overdue {
		var current int64
		if err := sqldb.QueryRowContext(ctx,
			`SELECT allianceID FROM corporation WHERE corporationID = ?`, b.DebtorID).Scan(&current); err != nil {
			continue
		}
		if current == 0 || current != b.ExternalID {
			_ = evedb.WriteOffBill(ctx, sqldb, b.BillID)
			continue
		}
		left, executor, err := removeCorpFromAlliance(ctx, sqldb, sessReg, b.ExternalID, b.DebtorID)
		if err != nil {
			log.Printf("expel corp %d from alliance %d: %v", b.DebtorID, b.ExternalID, err)
			continue
		}
		_ = evedb.WriteOffBill(ctx, sqldb, b.BillID)
		if left == 0 {
			continue
		}
		notifyExpelledCorp(ctx, sqldb, b)
		log.Printf("alliance %d: corp %d EXPELLED for unpaid maintenance (bill %d, executor now %d)",
			left, b.DebtorID, b.BillID, executor)
	}
}

// notifyExpelledCorp mails the expelled corporation's CEO from CONCORD, which
// issued the bill and keeps the alliance register.
func notifyExpelledCorp(ctx context.Context, sqldb *sql.DB, b evedb.Bill) {
	var ceoID int64
	if err := sqldb.QueryRowContext(ctx,
		`SELECT ceoID FROM corporation WHERE corporationID = ?`, b.DebtorID).Scan(&ceoID); err != nil || ceoID == 0 {
		return
	}
	subject := "Alliance membership revoked"
	body := fmt.Sprintf("This is an automatic message from CONCORD.\n\n"+
		"Your corporation has been removed from %s: its alliance maintenance bill of %.2f ISK "+
		"was not paid by its due date.", ownerDisplayName(ctx, sqldb, b.ExternalID), b.Amount)
	created := int64(server.Win32TimeNow())
	msgID, err := evedb.SendEveMail(ctx, sqldb, ceoID, evedb.OwnerCONCORDBills, subject, body, created)
	if err != nil {
		log.Printf("expulsion mail corp %d: %v", b.DebtorID, err)
		return
	}
	pushMailNotification(ceoID, msgID, evedb.OwnerCONCORDBills, subject, created)
}

// notifyEvictedCorp mails the evicted corporation's CEO from the station
// owner about the repossession.
func notifyEvictedCorp(ctx context.Context, sqldb *sql.DB, b evedb.Bill, contents int64) {
	var ceoID int64
	if err := sqldb.QueryRowContext(ctx,
		`SELECT ceoID FROM corporation WHERE corporationID = ?`, b.DebtorID).Scan(&ceoID); err != nil || ceoID == 0 {
		return
	}
	var stationName string
	_ = sqldb.QueryRowContext(ctx,
		`SELECT stationName FROM staStations WHERE stationID = ?`, b.ExternalID2).Scan(&stationName)
	subject := "Office repossessed"
	body := fmt.Sprintf("Your corporation's office at %s has been repossessed: the rental bill of %.2f ISK "+
		"was not paid by its due date. %d item(s) have been impounded and can be reclaimed at the station "+
		"for a fee.", stationName, b.Amount, contents)
	created := int64(server.Win32TimeNow())
	msgID, err := evedb.SendEveMail(ctx, sqldb, ceoID, b.CreditorID, subject, body, created)
	if err != nil {
		log.Printf("eviction mail corp %d: %v", b.DebtorID, err)
		return
	}
	pushMailNotification(ceoID, msgID, b.CreditorID, subject, created)
}

// allianceMembershipCost is const.allianceMembershipCost from eveConstants: 2,000,000 ISK per
// member corporation per week.
const allianceMembershipCost = 2000000.0
