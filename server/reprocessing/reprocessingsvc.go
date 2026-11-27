package reprocessing

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"math"
	"time"

	evedb "gurimoon/db"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
)

const (
	skillRefining           int64 = 3385
	skillRefineryEfficiency int64 = 3389
)

// Handle dispatches a CallReq addressed to the "reprocessingSvc" service.
func Handle(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID, stationID int64, sqldb *sql.DB) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	switch cs.Method {
	case "GetReprocessingInfo":
		return handleGetReprocessingInfo(ctx, c, addr, pkt, accountID, charID, stationID, sqldb)
	case "GetQuote":
		return handleGetQuote(ctx, c, addr, cs, pkt, accountID, charID, stationID, sqldb)
	case "GetQuotes":
		return handleGetQuotes(ctx, c, addr, cs, pkt, accountID, charID, stationID, sqldb)
	case "Reprocess":
		return handleReprocess(ctx, c, addr, cs, pkt, accountID, charID, stationID, sqldb)
	case "GetOptionsForItemTypes":
		return handleGetOptionsForItemTypes(ctx, c, addr, cs, pkt, accountID, stationID, sqldb)
	default:
		server.RecordUnhandled(addr, "reprocessingSvc", cs, true)
		log.Printf("[%s] reprocessingSvc: unhandled method %q — returning None", addr, cs.Method)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
}

// reprocessingParams computes station efficiency, tax, and combined yield for a character.
type reprocessingParams struct {
	StationEff    float64
	Tax           float64
	Standing      float64
	CombinedYield float64 // display value: station + generic skill term, capped
	SkillTerm     float64 // 0.375*(1+0.02*Refining)*(1+0.04*RefineryEff), uncapped
}

func getReprocessingParams(ctx context.Context, sqldb *sql.DB, charID, stationID int64) (*reprocessingParams, error) {
	sri, err := evedb.GetStationReprocessingInfo(ctx, sqldb, stationID)
	if err != nil {
		return nil, err
	}

	// Effective standing of the station-owner corp toward the character:
	// chrNPCStandings folded with the social-skill bonus. The client's welcome text
	// prints this value.
	factionID, _ := evedb.GetAgentCorpFactionID(ctx, sqldb, sri.CorporationID)
	corpStanding := evedb.GetNPCStandingToChar(ctx, sqldb, charID, sri.CorporationID)
	facStanding := evedb.GetNPCStandingToChar(ctx, sqldb, charID, factionID)
	bonus := evedb.StandingSkillBonus(ctx, sqldb, charID, corpStanding, facStanding, factionID)
	standing := evedb.ApplyStandingBonus(corpStanding, bonus)

	// Dials: reprocessing.taxBase less reprocessing.taxPerStanding per point of
	// standing; reprocessing.skillYieldBase is the skill term's base before Refining / Refinery Efficiency.
	tax := evedb.CachedSettingFloat("reprocessing.taxBase", 0.05, 0, 1) -
		evedb.CachedSettingFloat("reprocessing.taxPerStanding", 0.0075, 0, 1)*standing
	if tax < 0 {
		tax = 0
	}
	// A player owner sets its own take (Station Details tab), scaled by its
	// Reprocessing cost modifier for this pilot. See docs/station-management.md#enforcement.
	if npc, _ := evedb.IsNPCCorp(ctx, sqldb, sri.CorporationID); !npc && sri.CorporationID != 0 {
		_, corpID, allianceID, _ := evedb.GetCharSovContext(ctx, sqldb, charID)
		standing = evedb.OwnerStandingToward(ctx, sqldb, sri.CorporationID, charID, corpID, allianceID)
		tax = sri.StationTake * evedb.StationServiceFeeFactor(ctx, sqldb, stationID, charID, evedb.StationServiceReprocessing)
		tax = math.Max(0, math.Min(1, tax))
	}

	refining, err := evedb.GetCharacterSkillLevel(ctx, sqldb, charID, skillRefining)
	if err != nil {
		refining = 0
	}
	refineryEff, err := evedb.GetCharacterSkillLevel(ctx, sqldb, charID, skillRefineryEfficiency)
	if err != nil {
		refineryEff = 0
	}

	skillTerm := evedb.CachedSettingFloat("reprocessing.skillYieldBase", 0.375, 0, 1) *
		(1 + float64(refining)*0.02) * (1 + float64(refineryEff)*0.04)
	combinedYield := sri.Efficiency + skillTerm
	if combinedYield > 1.0 {
		combinedYield = 1.0
	}

	return &reprocessingParams{
		StationEff:    sri.Efficiency,
		Tax:           tax,
		Standing:      standing,
		CombinedYield: combinedYield,
		SkillTerm:     skillTerm,
	}, nil
}

func handleGetReprocessingInfo(ctx context.Context, c *evenet.Conn, addr string, pkt *evenet.PyPacket, accountID, charID, stationID int64, sqldb *sql.DB) error {
	params, err := getReprocessingParams(ctx, sqldb, charID, stationID)
	if err != nil {
		log.Printf("[%s] reprocessingSvc.GetReprocessingInfo: %v", addr, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}

	// The client reads this with dict subscripts (rinfo['yield'], rinfo['wetake'][0],
	// rinfo['wetake'][1]); util.KeyVal has no __getitem__, so it must be a plain dict.
	// wetake is (station take fraction, effective standing).
	result := marshal.PyDict{Items: []marshal.DictEntry{
		{Key: marshal.PyString{Value: "standing"}, Value: marshal.PyReal{Value: params.Standing}},
		{Key: marshal.PyString{Value: "tax"}, Value: marshal.PyReal{Value: params.Tax}},
		{Key: marshal.PyString{Value: "yield"}, Value: marshal.PyReal{Value: params.StationEff}},
		{Key: marshal.PyString{Value: "combinedyield"}, Value: marshal.PyReal{Value: params.CombinedYield}},
		{Key: marshal.PyString{Value: "wetake"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyReal{Value: params.Tax},
			marshal.PyReal{Value: params.Standing},
		}}},
	}}

	log.Printf("[%s] → reprocessingSvc.GetReprocessingInfo stationEff=%.3f tax=%.3f yield=%.3f",
		addr, params.StationEff, params.Tax, params.CombinedYield)
	return server.SendCallRsp(c, pkt, accountID, result)
}

// quoteLine represents one output material from reprocessing.
type quoteLine struct {
	typeID       int64
	unrecovable  int64
	stationTake  int64
	playerAmount int64
}

func calculateQuote(ctx context.Context, sqldb *sql.DB, charID, stationID int64, item *evedb.HangarItemInfo) ([]quoteLine, int64, error) {
	// Assembled items (ships, fitted modules, containers) must be repackaged first:
	// reprocessing a singleton would orphan its children.
	if item.Singleton != 0 {
		return nil, 0, fmt.Errorf("assembled itemID %d cannot be reprocessed — repackage it first", item.ItemID)
	}

	portionSize, err := evedb.GetTypePortionSize(ctx, sqldb, item.TypeID)
	if err != nil {
		return nil, 0, err
	}
	if portionSize <= 0 {
		portionSize = 1
	}

	mats, err := evedb.GetTypeMaterials(ctx, sqldb, item.TypeID)
	if err != nil {
		return nil, 0, err
	}
	if len(mats) == 0 {
		return nil, 0, fmt.Errorf("no materials for typeID %d", item.TypeID)
	}

	params, err := getReprocessingParams(ctx, sqldb, charID, stationID)
	if err != nil {
		return nil, 0, err
	}

	// The ore-specific skill bonus is folded into the skill term before the 100% cap;
	// applying it after the cap yields more than 100% recovery.
	oreSkillID, _ := evedb.GetOreProcessingSkill(ctx, sqldb, item.GroupID)
	oreSkillLevel, _ := evedb.GetCharacterSkillLevel(ctx, sqldb, charID, oreSkillID)
	oreBonus := 1 + float64(oreSkillLevel)*0.05
	effYield := params.StationEff + params.SkillTerm*oreBonus
	if effYield > 1.0 {
		effYield = 1.0
	}

	runs := item.Quantity / portionSize
	leftOvers := item.Quantity % portionSize

	var lines []quoteLine
	for _, mat := range mats {
		// Total material output = runs * baseMat * effectiveYield
		totalYield := float64(runs) * float64(mat.Quantity) * effYield
		totalInt := int64(math.Floor(totalYield))

		// Station tax portion.
		stationTake := int64(math.Floor(float64(totalInt) * params.Tax))

		// Unrecoverable = base - yielded
		baseTotal := runs * mat.Quantity
		unrecoverable := baseTotal - totalInt

		playerAmt := totalInt - stationTake
		if playerAmt < 0 {
			playerAmt = 0
		}

		lines = append(lines, quoteLine{
			typeID:       mat.MaterialTypeID,
			unrecovable:  unrecoverable,
			stationTake:  stationTake,
			playerAmount: playerAmt,
		})
	}

	return lines, leftOvers, nil
}

// buildQuoteKeyVal computes the quote for one hangar item and builds the
// util.KeyVal the client expects (recoverables list with typeID/client/
// station/unrecoverable per line).  Returns nil if the item can't be quoted.
func buildQuoteKeyVal(ctx context.Context, sqldb *sql.DB, charID, stationID, itemID int64) marshal.PyRep {
	item, err := evedb.GetHangarItem(ctx, sqldb, itemID, charID, stationID)
	if err != nil || item == nil {
		return nil
	}

	lines, leftOvers, err := calculateQuote(ctx, sqldb, charID, stationID, item)
	if err != nil {
		return nil
	}

	params, _ := getReprocessingParams(ctx, sqldb, charID, stationID)
	standing := 0.0
	if params != nil {
		standing = params.Standing
	}

	// Each line must be a util.KeyVal for attribute access
	// (client does line.typeID, line.client, etc.).
	pyLines := make([]marshal.PyRep, 0, len(lines))
	for _, l := range lines {
		pyLines = append(pyLines, marshal.PyObject{
			Type: "util.KeyVal",
			Args: marshal.PyDict{Items: []marshal.DictEntry{
				{Key: marshal.PyString{Value: "typeID"}, Value: marshal.PyInt{Value: uint64(l.typeID)}},
				{Key: marshal.PyString{Value: "unrecoverable"}, Value: marshal.PyInt{Value: uint64(l.unrecovable)}},
				{Key: marshal.PyString{Value: "station"}, Value: marshal.PyInt{Value: uint64(l.stationTake)}},
				{Key: marshal.PyString{Value: "client"}, Value: marshal.PyInt{Value: uint64(l.playerAmount)}},
			}},
		})
	}

	portionSize, _ := evedb.GetTypePortionSize(ctx, sqldb, item.TypeID)
	if portionSize <= 0 {
		portionSize = 1
	}
	qtyToProcess := (item.Quantity / portionSize) * portionSize

	return marshal.PyObject{
		Type: "util.KeyVal",
		Args: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "recoverables"}, Value: marshal.PyList{Items: pyLines}},
			{Key: marshal.PyString{Value: "leftOvers"}, Value: marshal.PyInt{Value: uint64(leftOvers)}},
			{Key: marshal.PyString{Value: "quantityToProcess"}, Value: marshal.PyInt{Value: uint64(qtyToProcess)}},
			{Key: marshal.PyString{Value: "playerStanding"}, Value: marshal.PyReal{Value: standing}},
		}},
	}
}

func handleGetQuote(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID, stationID int64, sqldb *sql.DB) error {
	itemID, err := pickInt(cs.ArgTuple.Items, 0, "GetQuote")
	if err != nil {
		log.Printf("[%s] reprocessingSvc.GetQuote: %v", addr, err)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}

	// A falsy return is the client's "contains no materials" signal
	// (base_reprocessingplant.py re-raises any non-UserError exception).
	quote := buildQuoteKeyVal(ctx, sqldb, charID, stationID, itemID)
	if quote == nil {
		log.Printf("[%s] reprocessingSvc.GetQuote itemID=%d: no quote", addr, itemID)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}

	log.Printf("[%s] → reprocessingSvc.GetQuote itemID=%d", addr, itemID)
	return server.SendCallRsp(c, pkt, accountID, quote)
}

// handleGetQuotes serves the ReprocessingDialog: GetQuotes takes a list of itemIDs
// and expects {itemID: quote}. Items that cannot be quoted are omitted; the client
// checks has_key per item.
func handleGetQuotes(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID, stationID int64, sqldb *sql.DB) error {
	itemIDs, err := pickIntList(cs.ArgTuple.Items, 0, "GetQuotes")
	if err != nil {
		log.Printf("[%s] reprocessingSvc.GetQuotes: %v", addr, err)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyDict{})
	}

	result := marshal.PyDict{}
	for _, itemID := range itemIDs {
		quote := buildQuoteKeyVal(ctx, sqldb, charID, stationID, itemID)
		if quote == nil {
			continue
		}
		result.Items = append(result.Items, marshal.DictEntry{
			Key:   marshal.PyInt{Value: uint64(itemID)},
			Value: quote,
		})
	}

	log.Printf("[%s] → reprocessingSvc.GetQuotes items=%d quoted=%d", addr, len(itemIDs), len(result.Items))
	return server.SendCallRsp(c, pkt, accountID, result)
}

func handleReprocess(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID, stationID int64, sqldb *sql.DB) error {
	// Client sends either a single itemID or a list of itemIDs (from bulk reprocess).
	itemIDs, err := pickIntList(cs.ArgTuple.Items, 0, "Reprocess")
	if err != nil {
		log.Printf("[%s] reprocessingSvc.Reprocess: %v", addr, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}

	for _, itemID := range itemIDs {
		reprocessOneItem(ctx, c, addr, accountID, charID, stationID, itemID, sqldb)
	}

	log.Printf("[%s] → reprocessingSvc.Reprocess items=%d", addr, len(itemIDs))
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

func reprocessOneItem(ctx context.Context, c *evenet.Conn, addr string, accountID, charID, stationID, itemID int64, sqldb *sql.DB) {
	item, err := evedb.GetHangarItem(ctx, sqldb, itemID, charID, stationID)
	if err != nil || item == nil {
		log.Printf("[%s] reprocessingSvc.Reprocess itemID=%d: item not found or error: %v", addr, itemID, err)
		return
	}

	lines, _, err := calculateQuote(ctx, sqldb, charID, stationID, item)
	if err != nil {
		log.Printf("[%s] reprocessingSvc.Reprocess itemID=%d: %v", addr, itemID, err)
		return
	}

	portionSize, _ := evedb.GetTypePortionSize(ctx, sqldb, item.TypeID)
	if portionSize <= 0 {
		portionSize = 1
	}
	qtyToProcess := (item.Quantity / portionSize) * portionSize

	// Snapshot the source row before the transaction so its deletion can be
	// announced afterwards.
	srcER, _ := evedb.GetEntityRow(ctx, sqldb, itemID)

	// Destroy the source and create outputs in one transaction so a failure cannot
	// consume the ore without producing minerals.
	tx, err := sqldb.BeginTx(ctx, nil)
	if err != nil {
		log.Printf("[%s] reprocessingSvc.Reprocess begin tx: %v", addr, err)
		return
	}
	defer tx.Rollback()

	type qtyChange struct {
		itemID int64
		oldQty int64
	}
	var mergedStacks []qtyChange
	var createdIDs []int64
	srcDeleted := item.Quantity-qtyToProcess <= 0

	if srcDeleted {
		if err := evedb.DeleteEntity(ctx, tx, itemID); err != nil {
			log.Printf("[%s] reprocessingSvc.Reprocess delete itemID=%d: %v", addr, itemID, err)
			return
		}
	} else {
		if err := evedb.ReduceEntityQuantity(ctx, tx, itemID, qtyToProcess); err != nil {
			log.Printf("[%s] reprocessingSvc.Reprocess reduce itemID=%d: %v", addr, itemID, err)
			return
		}
	}

	// A corporation's ore refines into the same division of its office, still the
	// corporation's; the character's own into its station hangar.
	outOwner, outLoc, outFlag := evedb.ReprocessOutputSlot(item, charID, stationID)
	for _, l := range lines {
		if l.playerAmount <= 0 {
			continue
		}
		existingID, existingQty, err := evedb.FindStackAt(ctx, tx, l.typeID, outOwner, outLoc, outFlag)
		if err != nil {
			log.Printf("[%s] reprocessingSvc.Reprocess find stack typeID=%d: %v", addr, l.typeID, err)
			return
		}
		if existingID != 0 {
			if err := evedb.UpdateCargoItemQty(ctx, tx, existingID, existingQty+l.playerAmount); err != nil {
				log.Printf("[%s] reprocessingSvc.Reprocess update stack itemID=%d: %v", addr, existingID, err)
				return
			}
			mergedStacks = append(mergedStacks, qtyChange{itemID: existingID, oldQty: existingQty})
		} else {
			newID, err := evedb.CreateStackAt(ctx, tx, l.typeID, outOwner, outLoc, outFlag, l.playerAmount)
			if err != nil {
				log.Printf("[%s] reprocessingSvc.Reprocess create typeID=%d: %v", addr, l.typeID, err)
				return
			}
			createdIDs = append(createdIDs, newID)
		}
	}

	if err := tx.Commit(); err != nil {
		log.Printf("[%s] reprocessingSvc.Reprocess commit itemID=%d: %v", addr, itemID, err)
		return
	}

	// Notify after commit only, so a rolled-back transaction stays invisible.
	if srcDeleted {
		if srcER != nil {
			sendItemRemoved(c, accountID, srcER)
		}
	} else if er, erErr := evedb.GetEntityRow(ctx, sqldb, itemID); erErr == nil && er != nil {
		sendItemQtyChanged(c, accountID, er, item.Quantity)
	}
	for _, m := range mergedStacks {
		if er, erErr := evedb.GetEntityRow(ctx, sqldb, m.itemID); erErr == nil && er != nil {
			sendItemQtyChanged(c, accountID, er, m.oldQty)
		}
	}
	for _, id := range createdIDs {
		if er, erErr := evedb.GetEntityRow(ctx, sqldb, id); erErr == nil && er != nil {
			sendItemCreated(c, accountID, er)
		}
	}

	log.Printf("[%s]   reprocessed itemID=%d qtyProcessed=%d materials=%d",
		addr, itemID, qtyToProcess, len(lines))
}

func handleGetOptionsForItemTypes(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, stationID int64, sqldb *sql.DB) error {
	result := marshal.PyDict{}
	if len(cs.ArgTuple.Items) >= 1 {
		if itemTypesDict, ok := cs.ArgTuple.Items[0].(marshal.PyDict); ok {
			result = OptionsForItemTypes(ctx, sqldb, itemTypesDict)
		}
	}
	log.Printf("[%s] → reprocessingSvc.GetOptionsForItemTypes → %d types", addr, len(result.Items))
	return server.SendCallRsp(c, pkt, accountID, result)
}

// OptionsForItemTypes builds the {typeID: KeyVal{isRecyclable, isRefinable}} dict
// for GetOptionsForItemTypes; the inline bind path in main.go shares it. The client
// sends {typeID: 0, ...} and needs every requested typeID back (a missing key is a KeyError).
func OptionsForItemTypes(ctx context.Context, sqldb *sql.DB, itemTypesDict marshal.PyDict) marshal.PyDict {
	result := marshal.PyDict{}
	for _, entry := range itemTypesDict.Items {
		var typeID int64
		switch k := entry.Key.(type) {
		case marshal.PyInt:
			typeID = int64(k.Value)
		default:
			continue
		}
		var isRecyclable marshal.PyRep = marshal.PyBool{Value: false}
		var isRefinable marshal.PyRep = marshal.PyBool{Value: false}
		mats, err := evedb.GetTypeMaterials(ctx, sqldb, typeID)
		if err == nil && len(mats) > 0 {
			isRecyclable = marshal.PyBool{Value: true}
			isRefinable = marshal.PyBool{Value: true}
		}
		result.Items = append(result.Items, marshal.DictEntry{
			Key: marshal.PyInt{Value: uint64(typeID)},
			Value: marshal.PyObject{
				Type: "util.KeyVal",
				Args: marshal.PyDict{Items: []marshal.DictEntry{
					{Key: marshal.PyString{Value: "isRecyclable"}, Value: isRecyclable},
					{Key: marshal.PyString{Value: "isRefinable"}, Value: isRefinable},
				}},
			},
		})
	}
	return result
}

func sendItemCreated(c *evenet.Conn, accountID int64, er *evedb.EntityRow) {
	args := marshal.PyTuple{Items: []marshal.PyRep{
		evedb.BuildEntityRow(er),
		marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyInt{Value: 3}, Value: marshal.PyInt{Value: 0}}, // ixLocationID changed
		}},
	}}
	_ = server.SendNotification(c, accountID, "OnItemChange", "charid", args)
}

func sendItemQtyChanged(c *evenet.Conn, accountID int64, er *evedb.EntityRow, oldQty int64) {
	args := marshal.PyTuple{Items: []marshal.PyRep{
		evedb.BuildEntityRow(er),
		marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyInt{Value: 7}, Value: marshal.PyInt{Value: uint64(oldQty)}}, // ixQuantity
		}},
	}}
	_ = server.SendNotification(c, accountID, "OnItemChange", "charid", args)
}

// sendItemRemoved announces a consumed item. The client drops a stack only when
// the row is in a junk location (cfg.IsJunkLocation) or has quantity 0, so it is
// re-parented to the junkyard with quantity 0 and the old location and quantity in
// the change dict (as itemDeletedNotifArgs in main.go does).
func sendItemRemoved(c *evenet.Conn, accountID int64, er *evedb.EntityRow) {
	gone := *er
	gone.LocationID = 6 // junkyard
	gone.Quantity = 0
	args := marshal.PyTuple{Items: []marshal.PyRep{
		evedb.BuildEntityRow(&gone),
		marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyInt{Value: 3}, Value: marshal.PyInt{Value: uint64(er.LocationID)}}, // ixLocationID
			{Key: marshal.PyInt{Value: 7}, Value: marshal.PyInt{Value: uint64(er.Quantity)}},   // ixQuantity
		}},
	}}
	_ = server.SendNotification(c, accountID, "OnItemChange", "charid", args)
}

// pickIntList extracts a list of int64s from arg[idx]. Accepts a single int or a PyList/PyTuple of ints.
func pickIntList(args []marshal.PyRep, idx int, name string) ([]int64, error) {
	if idx >= len(args) {
		return nil, fmt.Errorf("%s arg missing at index %d", name, idx)
	}
	switch v := args[idx].(type) {
	case marshal.PyInt:
		return []int64{int64(v.Value)}, nil
	case marshal.PyList:
		return extractInts(v.Items, name)
	case marshal.PyTuple:
		return extractInts(v.Items, name)
	}
	return nil, fmt.Errorf("%s arg[%d] is %T, want int or list", name, idx, args[idx])
}

func extractInts(items []marshal.PyRep, name string) ([]int64, error) {
	var ids []int64
	for _, item := range items {
		switch v := item.(type) {
		case marshal.PyInt:
			ids = append(ids, int64(v.Value))
		default:
			return nil, fmt.Errorf("%s list element is %T, want PyInt", name, item)
		}
	}
	return ids, nil
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
