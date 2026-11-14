package insurance

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"math"
	"time"

	"gurimoon/corp"
	evedb "gurimoon/db"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
)

// 12 weeks in Win32 FILETIME units (100-ns ticks).
const insuranceDuration uint64 = 12 * 7 * 24 * 3600 * 10_000_000

// insuranceLadder is base_insurance.GetQuoteForShip's six quotes: a premium of 5% to
// 30% of invTypes.basePrice in 5-point steps, buying a payout of 50% to 100%. The stored
// contract fraction is the payout: the window labels it with GetInsuranceName(fraction)
// against keys '0.5'..'1.0' and Show Info prints fraction * 100. The values must be the
// exact doubles Python renders as 0.5, so they are never built by adding 0.1.
var insuranceLadder = []struct{ Payout, Premium float64 }{
	{0.5, 0.05},
	{0.6, 0.10},
	{0.7, 0.15},
	{0.8, 0.20},
	{0.9, 0.25},
	{1.0, 0.30},
}

// payoutLabels maps payout fraction to a human-readable coverage label.
var payoutLabels = map[float64]string{
	0.50: "Basic",
	0.60: "Standard",
	0.70: "Bronze",
	0.80: "Silver",
	0.90: "Gold",
	1.00: "Platinum",
}

// PayoutLabel returns the coverage label for a payout fraction.
func PayoutLabel(payout float64) string {
	if label, ok := payoutLabels[payout]; ok {
		return label
	}
	return "Standard"
}

// matchQuote resolves the ISK amount the client picked back to a rung of the ladder,
// returning its payout fraction and the premium charged. The client sends quote.amount,
// never the fraction. GetQuoteForShip accumulates 5% steps, which drifts from a multiply,
// so the match tolerance is 2% of one rung rather than exact.
func matchQuote(basePrice, amount float64) (payout, premium float64, ok bool) {
	best, bestDiff := -1, math.Inf(1)
	for i, q := range insuranceLadder {
		if d := math.Abs(basePrice*q.Premium - amount); d < bestDiff {
			best, bestDiff = i, d
		}
	}
	if best < 0 || bestDiff > basePrice*0.001 {
		return 0, 0, false
	}
	q := insuranceLadder[best]
	return q.Payout, basePrice * q.Premium, true
}

// insureFailed raises the client's own InsureShipFailed UserError. base_insurance.Insure
// catches that key and refreshes the window before re-raising. The dict must carry
// reason (a missing key is a KeyError in the client), and a bare exception is None on
// the wire, which ExceptionWrapper cannot raise.
func insureFailed(c *evenet.Conn, pkt *evenet.PyPacket, accountID int64, reason string) error {
	return server.SendCallException(c, pkt, accountID,
		server.BuildUserError("InsureShipFailed", map[string]marshal.PyRep{
			"reason": marshal.PyString{Value: reason},
		}))
}

// Handle dispatches a CallReq addressed to the "insuranceSvc" service.
func Handle(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID, stationID int64, sqldb *sql.DB) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	switch cs.Method {
	case "GetContractForShip":
		return handleGetContractForShip(ctx, c, addr, cs, pkt, accountID, sqldb)
	case "GetInsurancePrice":
		return handleGetInsurancePrice(ctx, c, addr, cs, pkt, accountID, sqldb)
	case "InsureShip":
		return handleInsureShip(ctx, c, addr, cs, pkt, accountID, charID, sqldb)
	case "UnInsureShip":
		return handleUnInsureShip(ctx, c, addr, cs, pkt, accountID, sqldb)
	case "GetInsuredItems", "GetContracts":
		return handleGetInsuredItems(ctx, c, addr, cs, pkt, accountID, charID, sqldb)
	default:
		server.RecordUnhandled(addr, "insuranceSvc", cs, true)
		log.Printf("[%s] insuranceSvc: unhandled method %q — returning None", addr, cs.Method)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
}

func handleGetInsurancePrice(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	typeID, err := pickInt(cs.ArgTuple.Items, 0, "GetInsurancePrice")
	if err != nil {
		log.Printf("[%s] insuranceSvc.GetInsurancePrice: %v", addr, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	price, err := evedb.GetInsurancePrice(ctx, sqldb, typeID)
	if err != nil {
		log.Printf("[%s] insuranceSvc.GetInsurancePrice typeID=%d: %v", addr, typeID, err)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	log.Printf("[%s] → insuranceSvc.GetInsurancePrice typeID=%d price=%.2f", addr, typeID, price)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyReal{Value: price})
}

func handleGetContractForShip(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	shipID, err := pickInt(cs.ArgTuple.Items, 0, "GetContractForShip")
	if err != nil {
		log.Printf("[%s] insuranceSvc.GetContractForShip: %v", addr, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	ic, err := evedb.GetContractForShip(ctx, sqldb, shipID)
	if err != nil {
		log.Printf("[%s] insuranceSvc.GetContractForShip shipID=%d: %v", addr, shipID, err)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	if ic == nil {
		log.Printf("[%s] → insuranceSvc.GetContractForShip shipID=%d — no contract", addr, shipID)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	log.Printf("[%s] → insuranceSvc.GetContractForShip shipID=%d contractID=%d", addr, shipID, ic.ContractID)
	return server.SendCallRsp(c, pkt, accountID, contractToRow(ic))
}

// handleInsureShip implements base_insurance.Insure's InsureShip(itemID, quote.amount,
// isCorpItem). The second argument is the ISK premium of the clicked quote, not a
// fraction; matchQuote maps it back to a rung.
func handleInsureShip(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID int64, sqldb *sql.DB) error {
	args := cs.ArgTuple.Items
	shipID, err := pickInt(args, 0, "InsureShip")
	if err != nil {
		log.Printf("[%s] insuranceSvc.InsureShip: %v", addr, err)
		return insureFailed(c, pkt, accountID, "the request was malformed")
	}
	amount, err := pickFloat(args, 1, "InsureShip")
	if err != nil {
		log.Printf("[%s] insuranceSvc.InsureShip: %v", addr, err)
		return insureFailed(c, pkt, accountID, "the request was malformed")
	}
	isCorpItem := pickBool(args, 2)

	// Verify ship ownership. A corp item is paid from the corp wallet, so the payer is
	// resolved from the ship's owner, never from the client's flag alone.
	typeID, ownerID, err := evedb.GetShipEntity(ctx, sqldb, shipID)
	if err != nil {
		log.Printf("[%s] insuranceSvc.InsureShip shipID=%d: %v", addr, shipID, err)
		return insureFailed(c, pkt, accountID, "that ship could not be found")
	}
	payerID := charID
	if isCorpItem || ownerID != charID {
		corpID, err := evedb.GetCharCorpID(ctx, sqldb, charID)
		if err != nil || ownerID != corpID {
			log.Printf("[%s] insuranceSvc.InsureShip shipID=%d owner=%d is neither char=%d nor their corp", addr, shipID, ownerID, charID)
			return insureFailed(c, pkt, accountID, "you do not own that ship")
		}
		// Spending corp ISK needs an accountant, director or CEO; the client gates no button
		// and shows the corp's ships to junior accountants too.
		if !mayInsureForCorp(ctx, sqldb, corpID, charID) {
			log.Printf("[%s] insuranceSvc.InsureShip corpID=%d charID=%d lacks accountant role", addr, corpID, charID)
			return insureFailed(c, pkt, accountID, "you are not entitled to spend corporation funds")
		}
		payerID = corpID
	}

	// One contract per ship: the UNIQUE key on shipID would otherwise replace the running
	// one after the wallet was debited.
	if existing, err := evedb.GetContractForShip(ctx, sqldb, shipID); err == nil && existing != nil {
		log.Printf("[%s] insuranceSvc.InsureShip shipID=%d already insured (contract %d)", addr, shipID, existing.ContractID)
		return insureFailed(c, pkt, accountID, "that ship is already insured")
	}

	// Resolve the quote. basePrice is the number the client quoted from (cfg.invtypes).
	basePrice, err := evedb.GetInsurancePrice(ctx, sqldb, typeID)
	if err != nil || basePrice <= 0 {
		log.Printf("[%s] insuranceSvc.InsureShip price lookup typeID=%d: %v", addr, typeID, err)
		return insureFailed(c, pkt, accountID, "that ship type has no insurable value")
	}
	payout, premium, ok := matchQuote(basePrice, amount)
	if !ok {
		log.Printf("[%s] insuranceSvc.InsureShip amount %.2f matches no quote for typeID=%d (base %.2f)", addr, amount, typeID, basePrice)
		return insureFailed(c, pkt, accountID, "the quote offered is no longer valid")
	}

	// Deduct ISK from whichever wallet owns the hull.
	bal, err := payerBalance(ctx, sqldb, payerID, payerID != charID)
	if err != nil {
		log.Printf("[%s] insuranceSvc.InsureShip balance check: %v", addr, err)
		return insureFailed(c, pkt, accountID, "your account balance could not be read")
	}
	if bal < premium {
		log.Printf("[%s] insuranceSvc.InsureShip insufficient ISK: have=%.2f need=%.2f", addr, bal, premium)
		return insureFailed(c, pkt, accountID, "there are not enough funds to pay the premium")
	}
	if err := debitPayer(ctx, sqldb, payerID, payerID != charID, premium); err != nil {
		log.Printf("[%s] insuranceSvc.InsureShip deduct ISK: %v", addr, err)
		return insureFailed(c, pkt, accountID, "the premium could not be charged")
	}

	// Journal entry for insurance premium.
	newBal := bal - premium
	_ = evedb.InsertJournalEntry(ctx, sqldb, payerID, 19, payerID, evedb.OwnerPendInsurance, -premium, newBal, "")

	// Create the 12-week contract. ownerID is the wallet that paid: Show Info tests
	// contract.ownerID against both charid and corpid.
	now := uint64(time.Now().UnixNano()/100) + 116444736000000000
	endDate := now + insuranceDuration
	contractID, err := evedb.InsureShip(ctx, sqldb, shipID, payerID, payout, int64(now), int64(endDate))
	if err != nil {
		log.Printf("[%s] insuranceSvc.InsureShip db insert: %v", addr, err)
		// Refund: the premium is already gone and no contract exists.
		_ = debitPayer(ctx, sqldb, payerID, payerID != charID, -premium)
		return insureFailed(c, pkt, accountID, "the contract could not be written")
	}

	// Notify wallet.
	notifyArgs := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyString{Value: "cash"},
		marshal.PyInt{Value: uint64(payerID)},
		marshal.PyReal{Value: newBal},
	}}
	_ = server.SendNotification(c, accountID, "OnAccountChange", "charid", notifyArgs)

	// Send insurance confirmation mail.
	shipName, _ := evedb.GetTypeName(ctx, sqldb, typeID)
	if shipName == "" {
		shipName = fmt.Sprintf("Ship (type %d)", typeID)
	}
	payoutAmount := basePrice * payout
	label := PayoutLabel(payout)
	mailBody := fmt.Sprintf(
		"Dear Pilot,\n\nWe have issued a %s insurance contract for your %s.\n\nPremium paid: %.2f ISK\nCoverage payout: %.2f ISK\nExpiry: 12 weeks from date of purchase.\n\nFly safe.\n\n- Pend Insurance",
		label, shipName, premium, payoutAmount)
	_ = evedb.SendInsuranceMail(ctx, sqldb, charID, "Insurance Confirmation", mailBody, int64(now))

	log.Printf("[%s] → insuranceSvc.InsureShip shipID=%d contractID=%d payout=%.1f premium=%.2f payer=%d", addr, shipID, contractID, payout, premium, payerID)

	ic := &evedb.InsuranceContract{
		ContractID: contractID,
		ShipID:     shipID,
		OwnerID:    payerID,
		Fraction:   payout,
		StartDate:  int64(now),
		EndDate:    int64(endDate),
	}
	return server.SendCallRsp(c, pkt, accountID, contractToRow(ic))
}

// mayInsureForCorp gates spending the corp wallet on a corp hull.
func mayInsureForCorp(ctx context.Context, sqldb *sql.DB, corpID, charID int64) bool {
	var ceoID int64
	if err := sqldb.QueryRowContext(ctx,
		`SELECT ceoID FROM corporation WHERE corporationID = ?`, corpID).Scan(&ceoID); err == nil && ceoID == charID {
		return true
	}
	info, err := evedb.GetCorporationMemberInfo(ctx, sqldb, charID)
	if err != nil {
		return false
	}
	return corp.HasRole(info.Corprole, corp.RoleDirector) ||
		corp.HasRole(info.Corprole, corp.RoleAccountant)
}

func payerBalance(ctx context.Context, sqldb *sql.DB, payerID int64, isCorp bool) (float64, error) {
	if isCorp {
		return evedb.GetCorpBalance(ctx, sqldb, payerID)
	}
	return evedb.GetCharacterBalance(ctx, sqldb, payerID)
}

func debitPayer(ctx context.Context, sqldb *sql.DB, payerID int64, isCorp bool, amount float64) error {
	if isCorp {
		return evedb.CreditCorpWallet(ctx, sqldb, payerID, -amount)
	}
	return evedb.GiveISK(ctx, sqldb, payerID, -amount)
}

func handleUnInsureShip(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	shipID, err := pickInt(cs.ArgTuple.Items, 0, "UnInsureShip")
	if err != nil {
		log.Printf("[%s] insuranceSvc.UnInsureShip: %v", addr, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	if err := evedb.UnInsureShip(ctx, sqldb, shipID); err != nil {
		log.Printf("[%s] insuranceSvc.UnInsureShip shipID=%d: %v", addr, shipID, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	log.Printf("[%s] → insuranceSvc.UnInsureShip shipID=%d", addr, shipID)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// handleGetInsuredItems serves base_insurance.GetContracts, called bare for the pilot's
// hulls and as GetContracts(1) for the corp's when the caller holds an accountant role.
func handleGetInsuredItems(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID int64, sqldb *sql.DB) error {
	ownerID := charID
	if pickBool(cs.ArgTuple.Items, 0) {
		corpID, err := evedb.GetCharCorpID(ctx, sqldb, charID)
		if err != nil || !mayInsureForCorp(ctx, sqldb, corpID, charID) {
			// Not entitled to the corp list: an empty rowset, since GetContracts iterates the
			// result unconditionally.
			return server.SendCallRsp(c, pkt, accountID, evedb.EmptyInsuredItemsRowset())
		}
		ownerID = corpID
	}
	result, err := evedb.GetInsuredItems(ctx, sqldb, ownerID)
	if err != nil {
		log.Printf("[%s] insuranceSvc.GetInsuredItems ownerID=%d: %v", addr, ownerID, err)
		return server.SendCallRsp(c, pkt, accountID, evedb.EmptyInsuredItemsRowset())
	}
	log.Printf("[%s] → insuranceSvc.GetInsuredItems ownerID=%d", addr, ownerID)
	return server.SendCallRsp(c, pkt, accountID, result)
}

// contractToRow converts an InsuranceContract to a util.Row PyObject.
func contractToRow(ic *evedb.InsuranceContract) marshal.PyRep {
	return marshal.PyObject{
		Type: "util.Row",
		Args: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "header"}, Value: marshal.PyList{Items: []marshal.PyRep{
				marshal.PyString{Value: "contractID"},
				marshal.PyString{Value: "shipID"},
				marshal.PyString{Value: "ownerID"},
				marshal.PyString{Value: "fraction"},
				marshal.PyString{Value: "startDate"},
				marshal.PyString{Value: "endDate"},
			}}},
			{Key: marshal.PyString{Value: "line"}, Value: marshal.PyList{Items: []marshal.PyRep{
				marshal.PyInt{Value: uint64(ic.ContractID)},
				marshal.PyInt{Value: uint64(ic.ShipID)},
				marshal.PyInt{Value: uint64(ic.OwnerID)},
				marshal.PyReal{Value: ic.Fraction},
				marshal.PyInt{Value: uint64(ic.StartDate)},
				marshal.PyInt{Value: uint64(ic.EndDate)},
			}}},
		}},
	}
}

func pickInt(args []marshal.PyRep, idx int, name string) (int64, error) {
	if idx >= len(args) {
		return 0, fmt.Errorf("%s arg missing at index %d", name, idx)
	}
	switch v := args[idx].(type) {
	case marshal.PyInt:
		return int64(v.Value), nil
	case marshal.PyReal:
		return int64(v.Value), nil
	}
	return 0, fmt.Errorf("%s arg[%d] is %T, want PyInt", name, idx, args[idx])
}

func pickFloat(args []marshal.PyRep, idx int, name string) (float64, error) {
	if idx >= len(args) {
		return 0, fmt.Errorf("%s arg missing at index %d", name, idx)
	}
	switch v := args[idx].(type) {
	case marshal.PyReal:
		return v.Value, nil
	case marshal.PyInt:
		return float64(v.Value), nil
	}
	return 0, fmt.Errorf("%s arg[%d] is %T, want PyReal", name, idx, args[idx])
}

// pickBool reads an optional flag argument. The client sends 0/1 ints or bools by call
// site and omits it on the bare form, so a missing argument is false.
func pickBool(args []marshal.PyRep, idx int) bool {
	if idx >= len(args) {
		return false
	}
	switch v := args[idx].(type) {
	case marshal.PyBool:
		return v.Value
	case marshal.PyInt:
		return v.Value != 0
	case marshal.PyReal:
		return v.Value != 0
	}
	return false
}
