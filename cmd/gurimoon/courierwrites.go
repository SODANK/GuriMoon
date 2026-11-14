package main

// Player courier contract handlers, dispatched from handleMissionMgr and handleBoundCall (bound
// CreateCourierMission). Client contract (base_missions.py):
//   - Create: GetCourierMissionCreator(stationID).CreateCourierMission(itemlist, dropStationID,
//     description, collateral, assignedTo, tracking, reward, expiryDate) returns (missionID,
//     excludedItemIDs). itemlist is [(itemID, None), ...]; expiryDate is a FILETIME.
//   - Browse: GetAvailableCourierMissions(maxVolume, maxCollateral, regionID, solsysID,
//     stationID), filtered server-side. The wire sends totalReward = reward + collateral and
//     acceptFee = collateral; column 1 shows totalReward - acceptFee, column 6 acceptFee.
//   - GetMyCourierMissions, AcceptCourierMission, CancelMission, GetManifest and
//     DeliverCourierPackage take a missionID.

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

	evedb "gurimoon/db"
	evenet "gurimoon/net"
	"gurimoon/marshal"
	"gurimoon/server"

	"gurimoon/esp"
)

// createCourierMission handles the CreateCourierMission RPC from both the
// inline MachoBindObject path and the bound-object path.
func createCourierMission(ctx context.Context, addr string, c *evenet.Conn, pkt *evenet.PyPacket,
	accountID, charID, stationID int64, cs *server.CallStream, sqldb *sql.DB) (marshal.PyRep, error) {

	if stationID == 0 {
		return nil, fmt.Errorf("must be docked to create a courier contract")
	}

	// Positional args: (itemlist, dropStationID, description, collateral); kwargs: reward,
	// expiryDate (FILETIME), tracking, assignedTo.
	args := cs.ArgTuple
	if len(args.Items) < 4 {
		return nil, fmt.Errorf("CreateCourierMission: expected 4+ args, got %d", len(args.Items))
	}

	itemIDs := parseItemTupleList(args.Items[0])
	dropStationID := repToInt(args.Items[1])
	description := repToStr(args.Items[2])
	collateral := repToFloat(args.Items[3])

	// Read kwargs.
	reward := dictFloat(cs.ArgDict, "reward")
	expiryFileTime := dictInt(cs.ArgDict, "expiryDate")

	// Convert Win32 FILETIME → days from now (clamped 1–30).
	daysToExpire := 7
	if expiryFileTime > 0 {
		expiryUnix := expiryFileTime/10000000 - 11644473600
		days := int((expiryUnix - time.Now().Unix()) / 86400)
		if days < 1 {
			days = 1
		} else if days > 30 {
			days = 30
		}
		daysToExpire = days
	}

	if len(itemIDs) == 0 {
		return nil, fmt.Errorf("no items selected")
	}
	if dropStationID == 0 {
		return nil, fmt.Errorf("no drop station selected")
	}
	if reward < 0 {
		return nil, fmt.Errorf("reward cannot be negative")
	}
	if collateral < 0 {
		return nil, fmt.Errorf("collateral cannot be negative")
	}

	// Sum volumes: full stack per item (the client sends no quantities).
	var totalVolume float64
	for _, itemID := range itemIDs {
		var vol float64
		var qty int64
		sqldb.QueryRowContext(ctx,
			`SELECT COALESCE(t.volume, 0), e.quantity FROM entity e
			 JOIN invTypes t ON t.typeID = e.typeID
			 WHERE e.itemID = ?`, itemID).Scan(&vol, &qty)
		if qty <= 0 {
			qty = 1
		}
		totalVolume += vol * float64(qty)
	}
	if totalVolume > evedb.CourierMaxVolumeM3 {
		return nil, fmt.Errorf("total volume %.0f m³ exceeds maximum %.0f m³",
			totalVolume, evedb.CourierMaxVolumeM3)
	}

	// Detect escrow mode: reward=0 and drop station = pickup station.
	isEscrow := reward == 0 && dropStationID == stationID

	listingFee := evedb.CourierBasicFee
	totalCost := listingFee
	if !isEscrow {
		totalCost += reward // escrow: issuer pays only the listing fee
	}

	// Verify wallet.
	var balance float64
	if err := sqldb.QueryRowContext(ctx,
		`SELECT balance FROM character_ WHERE characterID = ?`,
		charID).Scan(&balance); err != nil {
		return nil, fmt.Errorf("wallet lookup: %w", err)
	}
	if balance < totalCost {
		return nil, fmt.Errorf("insufficient funds (need %.2f, have %.2f)", totalCost, balance)
	}

	// Resolve station geography.
	var pickupSysID, pickupRegionID int64
	sqldb.QueryRowContext(ctx,
		`SELECT COALESCE(solarSystemID,0), COALESCE(regionID,0)
		 FROM staStations WHERE stationID = ?`, stationID).Scan(&pickupSysID, &pickupRegionID)
	var dropSysID, dropRegionID int64
	sqldb.QueryRowContext(ctx,
		`SELECT COALESCE(solarSystemID,0), COALESCE(regionID,0)
		 FROM staStations WHERE stationID = ?`, dropStationID).Scan(&dropSysID, &dropRegionID)

	// Get corp ID.
	corpID, _ := evedb.GetCharCorpID(ctx, sqldb, charID)

	// Transaction: debit issuer, create escrow container, escrow items, insert mission.
	tx, err := sqldb.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Debit wallet.
	newBalance := balance - totalCost
	if _, err := tx.ExecContext(ctx,
		`UPDATE character_ SET balance = ? WHERE characterID = ?`,
		newBalance, charID); err != nil {
		return nil, fmt.Errorf("debit wallet: %w", err)
	}

	// Journal for listing fee + reward escrow.
	journalReason := "Courier contract created — reward + listing fee"
	if isEscrow {
		journalReason = "Escrow listing fee"
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO market_journal
		  (characterID, transDate, refTypeID, ownerID1, ownerID2, argID1,
		   accountID, amount, balance, reason, accountKey)
		VALUES (?, ?, ?, ?, ?, NULL, 0, ?, ?, ?, 1000)`,
		charID, server.Win32TimeNow(), evedb.CourierRefType, charID, charID,
		-totalCost, newBalance,
		evedb.JournalReason(journalReason)); err != nil {
		log.Printf("[db] createCourierMission: best-effort write failed: %v", err)
	}

	// Create escrow container.
	containerID, err := evedb.CreateCourierEscrowContainer(ctx, tx, stationID)
	if err != nil {
		return nil, err
	}

	// Escrow each item (full stack; EscrowCourierItem clamps qty <= 0 to have).
	var excluded []int64
	for _, itemID := range itemIDs {
		if err := evedb.EscrowCourierItem(ctx, tx, charID, stationID, containerID, itemID, 0); err != nil {
			log.Printf("[%s] courier escrow item %d: %v — excluding", addr, itemID, err)
			excluded = append(excluded, itemID)
		}
	}

	// Insert the mission row.
	var missionID int64
	if isEscrow {
		missionID, err = evedb.InsertEscrow(ctx, tx, evedb.EscrowCreateParams{
			IssuerID:            charID,
			IssuerCorpID:        corpID,
			PickupStationID:     stationID,
			Collateral:          collateral,
			ListingFee:          listingFee,
			Volume:              totalVolume,
			Description:         description,
			DaysToExpire:        daysToExpire,
			ContainerID:         containerID,
			IssuerIsBot:         false,
			PickupSolarSystemID: pickupSysID,
			PickupRegionID:      pickupRegionID,
		})
	} else {
		missionID, err = evedb.InsertCourierMission(ctx, tx, evedb.CourierCreateParams{
			IssuerID:            charID,
			IssuerCorpID:        corpID,
			PickupStationID:     stationID,
			DropStationID:       dropStationID,
			Reward:              reward,
			Collateral:          collateral,
			ListingFee:          listingFee,
			Volume:              totalVolume,
			Description:         description,
			DaysToExpire:        daysToExpire,
			ContainerID:         containerID,
			IssuerIsBot:         false,
			PickupSolarSystemID: pickupSysID,
			PickupRegionID:      pickupRegionID,
			DropSolarSystemID:   dropSysID,
			DropRegionID:        dropRegionID,
		})
	}
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	kindLabel := "Courier"
	if isEscrow {
		kindLabel = "Escrow"
	}
	log.Printf("[%s] → Create%sMission charID=%d missionID=%d reward=%.0f collateral=%.0f volume=%.0f",
		addr, kindLabel, charID, missionID, reward, collateral, totalVolume)

	// Push wallet update.
	_ = server.SendNotification(c, accountID, "OnAccountChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyString{Value: "cash"},
		marshal.PyInt{Value: uint64(charID)},
		marshal.PyReal{Value: newBalance},
	}})

	// Client unpacks (missionID, excludedItemIDs).
	excludedReps := make([]marshal.PyRep, len(excluded))
	for i, id := range excluded {
		excludedReps[i] = marshal.PyInt{Value: uint64(id)}
	}
	return marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyInt{Value: uint64(missionID)},
		marshal.PyList{Items: excludedReps},
	}}, nil
}

// handleCourierAccept processes AcceptCourierMission(missionID).
func handleCourierAccept(ctx context.Context, addr string, c *evenet.Conn, pkt *evenet.PyPacket,
	accountID, charID int64, cs *server.CallStream, sqldb *sql.DB, sessReg *esp.SessionRegistry) error {

	if len(cs.ArgTuple.Items) < 1 {
		return server.SendCallException(c, pkt, accountID, server.BuildUserError(
			"CustomError", map[string]marshal.PyRep{
				"error": marshal.PyString{Value: "Missing mission ID."},
			}))
	}
	missionID := repToInt(cs.ArgTuple.Items[0])

	// Check if this is an escrow listing and dispatch accordingly.
	if evedb.IsEscrowListing(ctx, sqldb, missionID) {
		return handleEscrowClaim(ctx, addr, c, pkt, accountID, charID, cs, sqldb, sessReg)
	}

	// Verify caller is docked at the pickup station.
	stationID, err := evedb.GetCharStationID(ctx, sqldb, charID)
	if err != nil || stationID == 0 {
		return server.SendCallException(c, pkt, accountID, server.BuildUserError(
			"CustomError", map[string]marshal.PyRep{
				"error": marshal.PyString{Value: "You must be docked to accept a courier contract."},
			}))
	}

	// Check the contract's pickup station matches.
	contract, err := evedb.GetCourierByID(ctx, sqldb, missionID)
	if err != nil || contract == nil {
		return server.SendCallException(c, pkt, accountID, server.BuildUserError(
			"CustomNotify", map[string]marshal.PyRep{
				"notify": marshal.PyString{Value: "That courier listing is no longer available."},
			}))
	}
	if contract.PickupStationID != stationID {
		return server.SendCallException(c, pkt, accountID, server.BuildUserError(
			"CustomError", map[string]marshal.PyRep{
				"error": marshal.PyString{Value: "You must be docked at the pickup station to accept this contract."},
			}))
	}

	corpID, _ := evedb.GetCharCorpID(ctx, sqldb, charID)

	crateID, err := evedb.AcceptCourierMissionDB(ctx, sqldb, missionID, charID, corpID, false)
	if err != nil {
		log.Printf("[%s] AcceptCourierMission %d: %v", addr, missionID, err)
		return server.SendCallException(c, pkt, accountID, server.BuildUserError(
			"CustomError", map[string]marshal.PyRep{
				"error": marshal.PyString{Value: err.Error()},
			}))
	}

	log.Printf("[%s] → AcceptCourierMission charID=%d missionID=%d crateID=%d",
		addr, charID, missionID, crateID)

	// Push wallet update for collateral debit.
	var newBal float64
	sqldb.QueryRowContext(ctx,
		`SELECT balance FROM character_ WHERE characterID = ?`, charID).Scan(&newBal)
	_ = server.SendNotification(c, accountID, "OnAccountChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyString{Value: "cash"},
		marshal.PyInt{Value: uint64(charID)},
		marshal.PyReal{Value: newBal},
	}})

	// Push OnItemChange for the courier crate so it appears in the hangar
	// without requiring a redock.
	if crateID > 0 {
		er, qErr := evedb.GetEntityRow(ctx, sqldb, crateID)
		if qErr == nil && er != nil {
			_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
				evedb.BuildEntityRow(er),
				marshal.PyDict{Items: []marshal.DictEntry{
					{Key: marshal.PyInt{Value: 3}, Value: marshal.PyInt{Value: 0}}, // ixLocationID: old=0 (new item)
				}},
			}})
		}
	}

	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// courierSealBlocks reports whether an Add/MultiAdd must be refused because it touches a courier
// package: it moves goods out of one (they belong to the issuer until delivery), or the bound
// inventory it drops into is a package.
func courierSealBlocks(sqldb *sql.DB, cs *server.CallStream, itemIDs []int64) bool {
	if sqldb == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if bindID := bindNumFromCallStream(cs); bindID > 0 {
		if _, isPkg := evedb.CourierCrateEscrowID(ctx, sqldb, bindID); isPkg {
			return true
		}
	}
	for _, id := range itemIDs {
		if evedb.CourierSealed(ctx, sqldb, id) {
			return true
		}
	}
	return false
}

// sendNotInMissionPackage refuses with the client's own package message
// ("Can't do that in a mission package").
func sendNotInMissionPackage(c *evenet.Conn, pkt *evenet.PyPacket, accountID int64) error {
	return server.SendCallException(c, pkt, accountID,
		server.BuildUserError("NotInMissionPackage", map[string]marshal.PyRep{}))
}

// handleBreakPlasticWrap answers the package's "Break..." menu entry (menusvc.Break calls
// GetInventoryFromId(pkg).BreakPlasticWrap()). A courier package is not the acceptor's to open.
// The refusal carries a reason because an unhandled call surfaces as an exception after the
// client's confirm dialog.
func handleBreakPlasticWrap(c *evenet.Conn, addr string, pkt *evenet.PyPacket, accountID int64,
	cs *server.CallStream) error {
	log.Printf("[%s] BreakPlasticWrap package=%d: refused", addr, bindNumFromCallStream(cs))
	return corpUserErr(c, pkt, accountID,
		"A courier package can only be opened by its recipient. Deliver it to the drop station to complete the contract.")
}

// handleCourierDeliver processes DeliverCourierPackage(missionID).
func handleCourierDeliver(ctx context.Context, addr string, c *evenet.Conn, pkt *evenet.PyPacket,
	accountID, charID int64, cs *server.CallStream, sqldb *sql.DB, sessReg *esp.SessionRegistry) error {

	if len(cs.ArgTuple.Items) < 1 {
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	missionID := repToInt(cs.ArgTuple.Items[0])

	stationID, err := evedb.GetCharStationID(ctx, sqldb, charID)
	if err != nil || stationID == 0 {
		return server.SendCallException(c, pkt, accountID, server.BuildUserError(
			"CustomError", map[string]marshal.PyRep{
				"error": marshal.PyString{Value: "You must be docked at the drop station to deliver."},
			}))
	}

	// Snapshot the package before delivery deletes it: the client still holds
	// it in the hangar or cargo cache and drops it only on an OnItemChange.
	var pkgRow *evedb.EntityRow
	if pre, _ := evedb.GetCourierByID(ctx, sqldb, missionID); pre != nil && pre.CraterID > 0 {
		pkgRow, _ = evedb.GetEntityRow(ctx, sqldb, pre.CraterID)
	}

	if err := evedb.DeliverCourierMissionDB(ctx, sqldb, missionID, charID, stationID); err != nil {
		log.Printf("[%s] DeliverCourierPackage %d: %v", addr, missionID, err)
		return server.SendCallException(c, pkt, accountID, server.BuildUserError(
			"CustomError", map[string]marshal.PyRep{
				"error": marshal.PyString{Value: err.Error()},
			}))
	}

	log.Printf("[%s] → DeliverCourierPackage charID=%d missionID=%d", addr, charID, missionID)

	// Tell the client the package is gone, or it stays in the hangar as an empty Plastic Wrap until relog.
	if pkgRow != nil {
		_ = server.SendNotification(c, accountID, "OnItemChange", "charid", itemDeletedNotifArgs(pkgRow))
	}

	// Push wallet update to acceptor (caller).
	var newBal float64
	sqldb.QueryRowContext(ctx,
		`SELECT balance FROM character_ WHERE characterID = ?`, charID).Scan(&newBal)
	_ = server.SendNotification(c, accountID, "OnAccountChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyString{Value: "cash"},
		marshal.PyInt{Value: uint64(charID)},
		marshal.PyReal{Value: newBal},
	}})

	// Push wallet update to issuer if online.
	contract, _ := evedb.GetCourierByID(ctx, sqldb, missionID)
	if contract != nil {
		if sess := sessReg.FindByCharID(contract.IssuerID); sess != nil && sess.Conn != nil {
			var issuerBal float64
			sqldb.QueryRowContext(ctx,
				`SELECT balance FROM character_ WHERE characterID = ?`,
				contract.IssuerID).Scan(&issuerBal)
			_ = server.SendNotification(sess.Conn, int64(sess.AccountID), "OnAccountChange", "charid",
				marshal.PyTuple{Items: []marshal.PyRep{
					marshal.PyString{Value: "cash"},
					marshal.PyInt{Value: uint64(contract.IssuerID)},
					marshal.PyReal{Value: issuerBal},
				}})
		}
	}

	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// handleCourierCancel processes CancelMission(missionID).
func handleCourierCancel(ctx context.Context, addr string, c *evenet.Conn, pkt *evenet.PyPacket,
	accountID, charID int64, cs *server.CallStream, sqldb *sql.DB) error {

	if len(cs.ArgTuple.Items) < 1 {
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	missionID := repToInt(cs.ArgTuple.Items[0])

	if err := evedb.CancelCourierMission(ctx, sqldb, missionID, charID); err != nil {
		log.Printf("[%s] CancelMission %d: %v", addr, missionID, err)
		return server.SendCallException(c, pkt, accountID, server.BuildUserError(
			"CustomError", map[string]marshal.PyRep{
				"error": marshal.PyString{Value: err.Error()},
			}))
	}

	log.Printf("[%s] → CancelMission charID=%d missionID=%d", addr, charID, missionID)

	// Push wallet update for reward refund.
	var newBal float64
	sqldb.QueryRowContext(ctx,
		`SELECT balance FROM character_ WHERE characterID = ?`, charID).Scan(&newBal)
	_ = server.SendNotification(c, accountID, "OnAccountChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyString{Value: "cash"},
		marshal.PyInt{Value: uint64(charID)},
		marshal.PyReal{Value: newBal},
	}})

	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// handleCourierManifest processes GetManifest(missionID). The client sends the missionID (or
// escrowID) from the browse row, so resolve through both tables before querying items.
func handleCourierManifest(ctx context.Context, addr string, c *evenet.Conn, pkt *evenet.PyPacket,
	accountID int64, cs *server.CallStream, sqldb *sql.DB) error {

	if len(cs.ArgTuple.Items) < 1 {
		return server.SendCallRsp(c, pkt, accountID, marshal.PyList{})
	}
	listingID := repToInt(cs.ArgTuple.Items[0])
	if listingID == 0 {
		return server.SendCallRsp(c, pkt, accountID, marshal.PyList{})
	}

	// Resolve listingID → containerID. Try escrow first, then courier.
	containerID := int64(0)
	if cid, err := evedb.GetEscrowContainerID(ctx, sqldb, int64(listingID)); err == nil && cid > 0 {
		containerID = cid
	} else if cid, err := evedb.GetCourierContainerID(ctx, sqldb, int64(listingID)); err == nil && cid > 0 {
		containerID = cid
	}
	if containerID == 0 {
		log.Printf("[%s] GetManifest %d: no container found", addr, listingID)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyList{})
	}

	items, err := evedb.GetCourierManifest(ctx, sqldb, containerID)
	if err != nil {
		log.Printf("[%s] GetManifest %d (container %d): %v", addr, listingID, containerID, err)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyList{})
	}

	rows := make([]marshal.PyRep, len(items))
	for i, it := range items {
		rows[i] = marshal.PyObject{
			Type: "util.KeyVal",
			Args: marshal.PyDict{Items: []marshal.DictEntry{
				{Key: marshal.PyString{Value: "itemID"}, Value: marshal.PyInt{Value: uint64(it.ItemID)}},
				{Key: marshal.PyString{Value: "typeID"}, Value: marshal.PyInt{Value: uint64(it.TypeID)}},
				{Key: marshal.PyString{Value: "ownerID"}, Value: marshal.PyInt{Value: uint64(it.OwnerID)}},
				{Key: marshal.PyString{Value: "quantity"}, Value: marshal.PyInt{Value: uint64(it.Quantity)}},
				{Key: marshal.PyString{Value: "groupID"}, Value: marshal.PyInt{Value: uint64(it.GroupID)}},
				{Key: marshal.PyString{Value: "categoryID"}, Value: marshal.PyInt{Value: uint64(it.CategoryID)}},
				{Key: marshal.PyString{Value: "damage"}, Value: marshal.PyInt{Value: 0}},
				{Key: marshal.PyString{Value: "copy"}, Value: marshal.PyInt{Value: uint64(it.Copy)}},
				{Key: marshal.PyString{Value: "materialLevel"}, Value: marshal.PyInt{Value: uint64(it.MaterialLevel)}},
				{Key: marshal.PyString{Value: "productivityLevel"}, Value: marshal.PyInt{Value: uint64(it.ProductivityLevel)}},
				{Key: marshal.PyString{Value: "bpRuns"}, Value: marshal.PyInt{Value: uint64(it.LicensedRuns)}},
			}},
		}
	}

	log.Printf("[%s] → GetManifest listingID=%d containerID=%d (%d items)", addr, listingID, containerID, len(rows))
	return server.SendCallRsp(c, pkt, accountID, marshal.PyList{Items: rows})
}

// buildAvailableCourierMissions builds the browse list from five server-side filter args:
// (maxVolume, maxCollateral, regionID, solsysID, stationID). The wire carries totalReward =
// reward + collateral and acceptFee = collateral (the client shows column 1 as totalReward -
// acceptFee and column 6 as acceptFee).
func buildAvailableCourierMissions(ctx context.Context, addr string, sqldb *sql.DB,
	charID, stationID int64, filterArgs marshal.PyTuple) marshal.PyRep {

	// Extract filter args: (maxVolume, maxCollateral, regionID, solsysID, stationID)
	var maxVolume, maxCollateral float64
	var fRegionID, fSolsysID, fStationID int64
	if len(filterArgs.Items) > 0 {
		maxVolume = repToFloat(filterArgs.Items[0])
	}
	if len(filterArgs.Items) > 1 {
		maxCollateral = repToFloat(filterArgs.Items[1])
	}
	if len(filterArgs.Items) > 2 {
		fRegionID = repToInt(filterArgs.Items[2])
	}
	if len(filterArgs.Items) > 3 {
		fSolsysID = repToInt(filterArgs.Items[3])
	}
	if len(filterArgs.Items) > 4 {
		fStationID = repToInt(filterArgs.Items[4])
	}

	listings, err := evedb.ListAvailableCouriersFiltered(ctx, sqldb,
		maxVolume, maxCollateral, fRegionID, fSolsysID, fStationID)
	if err != nil {
		log.Printf("[%s] GetAvailableCourierMissions: %v", addr, err)
		return marshal.PyList{}
	}

	// Station location cache for the response.
	type stnLoc struct{ solarSystemID, regionID int64 }
	locCache := map[int64]stnLoc{}
	stationLoc := func(sid int64) stnLoc {
		if l, ok := locCache[sid]; ok {
			return l
		}
		var l stnLoc
		if sid > 0 {
			sqldb.QueryRowContext(ctx, `
				SELECT COALESCE(solarSystemID,0), COALESCE(regionID,0)
				FROM staStations WHERE stationID = ?`, sid).Scan(&l.solarSystemID, &l.regionID)
		}
		locCache[sid] = l
		return l
	}

	rows := make([]marshal.PyRep, 0, len(listings))
	for _, l := range listings {
		pickup := stationLoc(l.PickupStationID)
		drop := stationLoc(l.DropStationID)

		expires := time.Unix(l.DateExpires, 0)

		// Wire mapping: totalReward = reward + collateral (gross), acceptFee = collateral.
		grossReward := l.TotalReward + l.Collateral

		rows = append(rows, marshal.PyObject{
			Type: "util.KeyVal",
			Args: marshal.PyDict{Items: []marshal.DictEntry{
				{Key: marshal.PyString{Value: "missionID"}, Value: marshal.PyInt{Value: uint64(l.MissionID)}},
				{Key: marshal.PyString{Value: "issuerID"}, Value: marshal.PyInt{Value: uint64(l.IssuerID)}},
				{Key: marshal.PyString{Value: "acceptorID"}, Value: marshal.PyNone{}},
				{Key: marshal.PyString{Value: "pickupStationID"}, Value: marshal.PyInt{Value: uint64(l.PickupStationID)}},
				{Key: marshal.PyString{Value: "dropStationID"}, Value: marshal.PyInt{Value: uint64(l.DropStationID)}},
				{Key: marshal.PyString{Value: "pickupSolarSystemID"}, Value: marshal.PyInt{Value: uint64(pickup.solarSystemID)}},
				{Key: marshal.PyString{Value: "pickupRegionID"}, Value: marshal.PyInt{Value: uint64(pickup.regionID)}},
				{Key: marshal.PyString{Value: "dropSolarSystemID"}, Value: marshal.PyInt{Value: uint64(drop.solarSystemID)}},
				{Key: marshal.PyString{Value: "dropRegionID"}, Value: marshal.PyInt{Value: uint64(drop.regionID)}},
				{Key: marshal.PyString{Value: "totalReward"}, Value: marshal.PyReal{Value: grossReward}},
				{Key: marshal.PyString{Value: "acceptFee"}, Value: marshal.PyReal{Value: l.Collateral}},
				{Key: marshal.PyString{Value: "collateral"}, Value: marshal.PyReal{Value: l.Collateral}},
				{Key: marshal.PyString{Value: "volume"}, Value: marshal.PyReal{Value: l.Volume}},
				{Key: marshal.PyString{Value: "dateExpires"}, Value: marshal.PyInt{Value: timeToFileTime(expires)}},
				{Key: marshal.PyString{Value: "description"}, Value: marshal.PyString{Value: l.Description}},
				{Key: marshal.PyString{Value: "craterID"}, Value: marshal.PyInt{Value: 0}},
			}},
		})
	}

	log.Printf("[%s] → GetAvailableCourierMissions (%d listings, filters: vol=%.0f collat=%.0f region=%d sys=%d stn=%d)",
		addr, len(rows), maxVolume, maxCollateral, fRegionID, fSolsysID, fStationID)
	return marshal.PyList{Items: rows}
}

// buildAllMyCourierMissions builds the "My Courier Missions" list, including
// both agent couriers and player couriers.
func buildAllMyCourierMissions(ctx context.Context, addr string, sqldb *sql.DB, charID int64) marshal.PyRep {
	// Start with the agent couriers (existing logic).
	agentRows := buildMyCourierMissions(ctx, addr, sqldb, charID)

	// Add player couriers.
	contracts, err := evedb.GetMyCourierContracts(ctx, sqldb, charID)
	if err != nil {
		log.Printf("[%s] GetMyCourierContracts: %v", addr, err)
		return agentRows
	}
	if len(contracts) == 0 {
		return agentRows
	}

	// Station location cache.
	type stnLoc struct{ solarSystemID, regionID int64 }
	locCache := map[int64]stnLoc{}
	stationLoc := func(sid int64) stnLoc {
		if l, ok := locCache[sid]; ok {
			return l
		}
		var l stnLoc
		if sid > 0 {
			sqldb.QueryRowContext(ctx, `
				SELECT COALESCE(solarSystemID,0), COALESCE(regionID,0)
				FROM staStations WHERE stationID = ?`, sid).Scan(&l.solarSystemID, &l.regionID)
		}
		locCache[sid] = l
		return l
	}

	// Merge agent rows into the result list.
	agentList, ok := agentRows.(marshal.PyList)
	if !ok {
		agentList = marshal.PyList{}
	}

	for _, cr := range contracts {
		pickup := stationLoc(cr.PickupStationID)
		drop := stationLoc(cr.DropStationID)

		expires := time.Unix(cr.DateExpires, 0)

		// Same wire mapping as the browse list.
		grossReward := cr.TotalReward + cr.Collateral

		// acceptorID must be None (not 0) when unaccepted: the client tests `acceptorID is None`.
		var acceptorRep marshal.PyRep = marshal.PyNone{}
		if cr.AcceptorID > 0 {
			acceptorRep = marshal.PyInt{Value: uint64(cr.AcceptorID)}
		}

		agentList.Items = append(agentList.Items, marshal.PyObject{
			Type: "util.KeyVal",
			Args: marshal.PyDict{Items: []marshal.DictEntry{
				{Key: marshal.PyString{Value: "missionID"}, Value: marshal.PyInt{Value: uint64(cr.MissionID)}},
				{Key: marshal.PyString{Value: "issuerID"}, Value: marshal.PyInt{Value: uint64(cr.IssuerID)}},
				{Key: marshal.PyString{Value: "acceptorID"}, Value: acceptorRep},
				{Key: marshal.PyString{Value: "pickupStationID"}, Value: marshal.PyInt{Value: uint64(cr.PickupStationID)}},
				{Key: marshal.PyString{Value: "dropStationID"}, Value: marshal.PyInt{Value: uint64(cr.DropStationID)}},
				{Key: marshal.PyString{Value: "pickupSolarSystemID"}, Value: marshal.PyInt{Value: uint64(pickup.solarSystemID)}},
				{Key: marshal.PyString{Value: "pickupRegionID"}, Value: marshal.PyInt{Value: uint64(pickup.regionID)}},
				{Key: marshal.PyString{Value: "dropSolarSystemID"}, Value: marshal.PyInt{Value: uint64(drop.solarSystemID)}},
				{Key: marshal.PyString{Value: "dropRegionID"}, Value: marshal.PyInt{Value: uint64(drop.regionID)}},
				{Key: marshal.PyString{Value: "totalReward"}, Value: marshal.PyReal{Value: grossReward}},
				{Key: marshal.PyString{Value: "acceptFee"}, Value: marshal.PyReal{Value: cr.Collateral}},
				{Key: marshal.PyString{Value: "collateral"}, Value: marshal.PyReal{Value: cr.Collateral}},
				{Key: marshal.PyString{Value: "volume"}, Value: marshal.PyReal{Value: cr.Volume}},
				{Key: marshal.PyString{Value: "dateExpires"}, Value: marshal.PyInt{Value: timeToFileTime(expires)}},
				{Key: marshal.PyString{Value: "description"}, Value: marshal.PyString{Value: cr.Description}},
				{Key: marshal.PyString{Value: "craterID"}, Value: marshal.PyInt{Value: uint64(cr.CraterID)}},
			}},
		})
	}

	return agentList
}

// ── PyRep helpers ───────────────────────────────────────────────────────────

func repToInt(r marshal.PyRep) int64 {
	switch v := r.(type) {
	case marshal.PyInt:
		return int64(v.Value)
	case marshal.PyBool:
		if v.Value {
			return 1
		}
		return 0
	}
	return 0
}

func repToFloat(r marshal.PyRep) float64 {
	switch v := r.(type) {
	case marshal.PyReal:
		return v.Value
	case marshal.PyInt:
		return float64(int64(v.Value))
	}
	return 0
}

func repToStr(r marshal.PyRep) string {
	switch v := r.(type) {
	case marshal.PyString:
		return v.Value
	}
	return ""
}

func parseIntListRep(r marshal.PyRep) []int64 {
	switch v := r.(type) {
	case marshal.PyList:
		out := make([]int64, len(v.Items))
		for i, item := range v.Items {
			out[i] = repToInt(item)
		}
		return out
	case marshal.PyTuple:
		out := make([]int64, len(v.Items))
		for i, item := range v.Items {
			out[i] = repToInt(item)
		}
		return out
	}
	return nil
}

// parseItemTupleList extracts itemIDs from the client's item list [(itemID, None), ...]; the
// first element of each 2-tuple is the itemID.
func parseItemTupleList(r marshal.PyRep) []int64 {
	items := listOrTupleItems(r)
	if items == nil {
		return nil
	}
	out := make([]int64, 0, len(items))
	for _, entry := range items {
		switch t := entry.(type) {
		case marshal.PyTuple:
			if len(t.Items) > 0 {
				out = append(out, repToInt(t.Items[0]))
			}
		case marshal.PyList:
			if len(t.Items) > 0 {
				out = append(out, repToInt(t.Items[0]))
			}
		default:
			// Bare int: tolerate flat lists.
			out = append(out, repToInt(entry))
		}
	}
	return out
}

func listOrTupleItems(r marshal.PyRep) []marshal.PyRep {
	switch v := r.(type) {
	case marshal.PyList:
		return v.Items
	case marshal.PyTuple:
		return v.Items
	}
	return nil
}

// dictFloat reads a float from a PyDict by key name.
func dictFloat(d *marshal.PyDict, key string) float64 {
	if d == nil {
		return 0
	}
	for _, e := range d.Items {
		if s, ok := e.Key.(marshal.PyString); ok && s.Value == key {
			return repToFloat(e.Value)
		}
	}
	return 0
}

// dictInt reads an int from a PyDict by key name.
func dictInt(d *marshal.PyDict, key string) int64 {
	if d == nil {
		return 0
	}
	for _, e := range d.Items {
		if s, ok := e.Key.(marshal.PyString); ok && s.Value == key {
			return repToInt(e.Value)
		}
	}
	return 0
}
