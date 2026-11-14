package main

// Station management: the lobby's Station Management window for a player-owned
// station. Contract: docs/station-management.md; storage and the access rule:
// db/stationmgmtdb.go. Every object the client edits and sends back is served
// as a util.KeyVal (lists of them where the client iterates), since a KeyVal
// returns as a plain dict and an edited util.Row has no confirmed shape.
// See docs/station-management.md#shape-decision-keyval-not-row.

import (
	"context"
	"database/sql"
	"log"
	"time"

	"gurimoon/corp"
	evedb "gurimoon/db"
	"gurimoon/esp"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
	"gurimoon/server/mapsvc"
)

// stationMgmtMethods is the window's bound surface. Unique names across the
// client's bound objects, so dispatch by name is safe (as corpStationMgrMethods).
var stationMgmtMethods = map[string]bool{
	"GetStationServiceIdentifiers":             true,
	"GetStationServiceAccessRule":              true,
	"GetStationManagementServiceCostModifiers": true,
	"GetStationDetails":                        true,
	"GetRentableItems":                         true,
	"UpdateStationManagementSettings":          true,
	"GetOwnerIDsOfClonesAtStation":             true,
	"RevokeCloneContractsAtStation":            true,
}

// rentalPeriodDays is the office rent cycle the bill sweep runs.
const rentalPeriodDays = 30

func handleStationManagement(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket,
	accountID, charID int64, stationID *int64, sqldb *sql.DB) error {
	if sqldb == nil || charID == 0 || stationID == nil || *stationID == 0 {
		return corpUserErr(c, pkt, accountID, "You have to be docked at a station your corporation owns to manage it.")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st := *stationID

	corpID := boundCharCorpID(charID, sqldb)
	if ok, reason := evedb.StationManageableBy(ctx, sqldb, st, corpID); !ok {
		log.Printf("[%s] corpStationMgr.%s station=%d corp=%d refused: %s", addr, cs.Method, st, corpID, reason)
		return corpUserErr(c, pkt, accountID, reason)
	}
	// The client's own gate, repeated: Station Manager, Director or CEO.
	roles := callerRoles(ctx, sqldb, charID)
	if !isCorpCEO(ctx, sqldb, corpID, charID) && !corp.HasRole(roles, corp.RoleDirector) &&
		!corp.HasRole(roles, corp.RoleStationManager) {
		return corpUserErr(c, pkt, accountID, "You do not have the role Station Manager, so you can't do station management.")
	}

	switch cs.Method {
	case "GetStationServiceIdentifiers":
		svcs, err := evedb.ListStationServices(ctx, sqldb)
		if err != nil {
			log.Printf("[%s] GetStationServiceIdentifiers: %v", addr, err)
		}
		out := make([]marshal.PyRep, 0, len(svcs))
		for _, s := range svcs {
			out = append(out, kv("serviceID", pyI(s.ServiceID), "serviceName", marshal.PyString{Value: s.Name}))
		}
		return server.SendCallRsp(c, pkt, accountID, marshal.PyList{Items: out})

	case "GetStationServiceAccessRule":
		serviceID := int64(0)
		if len(cs.ArgTuple.Items) >= 2 {
			if v, ok := cs.ArgTuple.Items[1].(marshal.PyInt); ok {
				serviceID = int64(v.Value)
			}
		}
		r, err := evedb.GetServiceAccessRule(ctx, sqldb, st, serviceID)
		if err != nil {
			log.Printf("[%s] GetStationServiceAccessRule %d/%d: %v", addr, st, serviceID, err)
		}
		return server.SendCallRsp(c, pkt, accountID, accessRuleKV(r))

	case "GetStationManagementServiceCostModifiers":
		mods, err := evedb.ListServiceCostModifiers(ctx, sqldb, st)
		if err != nil {
			log.Printf("[%s] GetStationManagementServiceCostModifiers %d: %v", addr, st, err)
		}
		out := make([]marshal.PyRep, 0, len(mods))
		for _, m := range mods {
			out = append(out, kv("serviceID", pyI(m.ServiceID),
				"discountPerGoodStandingPoint", marshal.PyReal{Value: m.DiscountPerGoodStandingPoint},
				"surchargePerBadStandingPoint", marshal.PyReal{Value: m.SurchargePerBadStandingPoint}))
		}
		return server.SendCallRsp(c, pkt, accountID, marshal.PyList{Items: out})

	case "GetStationDetails":
		d, err := evedb.GetStationDetails(ctx, sqldb, st)
		if err != nil {
			log.Printf("[%s] GetStationDetails %d: %v", addr, st, err)
			return corpUserErr(c, pkt, accountID, "The station's details could not be read.")
		}
		return server.SendCallRsp(c, pkt, accountID, kv(
			"stationID", pyI(d.StationID),
			"stationName", marshal.PyString{Value: d.Name},
			"description", marshal.PyString{Value: d.Description},
			"security", marshal.PyReal{Value: d.Security},
			"dockingCostPerVolume", marshal.PyReal{Value: d.DockingCostPerVolume},
			"officeRentalCost", pyI(d.OfficeRentalCost),
			"maxShipVolumeDockable", pyI(d.MaxShipVolumeDockable),
			"reprocessingStationsTake", marshal.PyReal{Value: d.ReprocessingStationsTake},
			"reprocessingHangarFlag", pyI(d.ReprocessingHangarFlag),
			"factorySlotRentalCost", pyI(0),
			"researchFacilitySlotRentalCost", pyI(0),
		))

	case "GetRentableItems":
		// Offices only (typeID 26), one row per slot whether rented or free; factory and
		// research slots have no per-slot rental here. number is the slot (flag 70 + n - 1).
		offices, err := evedb.ListStationOffices(ctx, sqldb, st)
		if err != nil {
			log.Printf("[%s] GetRentableItems %d: %v", addr, st, err)
		}
		public, err := evedb.OfficeSlotsPublic(ctx, sqldb, st)
		if err != nil {
			log.Printf("[%s] GetRentableItems %d slots: %v", addr, st, err)
		}
		bySlot := map[int64]evedb.StationOffice{}
		var unplaced []evedb.StationOffice
		capacity := evedb.StationOfficeCapacity(ctx, sqldb, st)
		for _, o := range offices {
			n := o.Flag - evedb.FlagOfficeSlotFirst + 1
			if _, taken := bySlot[n]; n < 1 || n > capacity || taken {
				unplaced = append(unplaced, o)
				continue
			}
			bySlot[n] = o
		}
		// An office whose flag is off the slot range still shows, in the first free number.
		for n := int64(1); n <= capacity && len(unplaced) > 0; n++ {
			if _, taken := bySlot[n]; !taken {
				bySlot[n], unplaced = unplaced[0], unplaced[1:]
			}
		}
		out := make([]marshal.PyRep, 0, capacity)
		const filetimePerDay = 864000000000
		for n := int64(1); n <= capacity; n++ {
			pub, set := public[n]
			if !set {
				pub = true
			}
			var item, renter, start, due, period, cost marshal.PyRep = marshal.PyNone{}, marshal.PyNone{},
				marshal.PyNone{}, marshal.PyNone{}, marshal.PyNone{}, marshal.PyNone{}
			if o, rented := bySlot[n]; rented {
				item, renter = pyI(o.OfficeID), pyI(o.CorpID)
				period, cost = pyI(rentalPeriodDays), marshal.PyReal{Value: o.PeriodISK}
				if o.LastPaid > 0 {
					start = pyI(o.LastPaid)
					due = pyI(o.LastPaid + rentalPeriodDays*filetimePerDay)
				}
			}
			out = append(out, kv(
				"typeID", pyI(evedb.TypeOfficeFolder),
				"number", pyI(n),
				"flag", pyI(evedb.OfficeSlotFlag(n)),
				"itemID", item,
				"rentedToID", renter,
				"publiclyAvailable", marshal.PyBool{Value: pub},
				"startDate", start,
				"rentPeriodInDays", period,
				"periodCost", cost,
				"balanceDueDate", due,
			))
		}
		return server.SendCallRsp(c, pkt, accountID, marshal.PyList{Items: out})

	case "UpdateStationManagementSettings":
		return handleUpdateStationManagement(ctx, c, addr, cs, pkt, accountID, st, sqldb)

	case "GetOwnerIDsOfClonesAtStation":
		// A clone contract is a pilot's home station (character_.homeStationID). corpID -1 lists
		// one row per corporation (.corporationID), any other corpID its pilots (.characterID).
		// See docs/station-management.md#clone-contracts.
		viewCorp := int64(-1)
		if len(cs.ArgTuple.Items) >= 1 {
			viewCorp = int64(repFloat(cs.ArgTuple.Items[0]))
		}
		var ids []int64
		var err error
		field := "corporationID"
		if viewCorp == -1 {
			ids, err = evedb.CloneContractCorps(ctx, sqldb, st)
		} else {
			field = "characterID"
			ids, err = evedb.CloneContractChars(ctx, sqldb, st, viewCorp)
		}
		if err != nil {
			log.Printf("[%s] GetOwnerIDsOfClonesAtStation %d/%d: %v", addr, st, viewCorp, err)
		}
		out := make([]marshal.PyRep, 0, len(ids))
		for _, id := range ids {
			out = append(out, kv(field, pyI(id)))
		}
		return server.SendCallRsp(c, pkt, accountID, marshal.PyList{Items: out})

	case "RevokeCloneContractsAtStation":
		// (corpID, charID): charID -1 revokes every contract the corporation holds here.
		if len(cs.ArgTuple.Items) < 2 {
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		revokeCorp := int64(repFloat(cs.ArgTuple.Items[0]))
		revokeChar := int64(repFloat(cs.ArgTuple.Items[1]))
		moved, err := evedb.RevokeCloneContracts(ctx, sqldb, st, revokeCorp, revokeChar)
		if err != nil {
			log.Printf("[%s] RevokeCloneContractsAtStation %d corp=%d char=%d: %v", addr, st, revokeCorp, revokeChar, err)
			return refuseErr(c, pkt, accountID, err)
		}
		log.Printf("[%s] RevokeCloneContractsAtStation %d corp=%d char=%d: %d contract(s) moved to the corp HQ",
			addr, st, revokeCorp, revokeChar, len(moved))
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// handleUpdateStationManagement is OK / Apply:
// (rules{serviceID: rule}, costMods[row], rentables[row], name, description,
// dockingCostPerVolume, factorySlotRentalCost, officeRentalCost,
// researchFacilitySlotRentalCost, reprocessingStationsTake, reprocessingHangarFlag).
func handleUpdateStationManagement(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream,
	pkt *evenet.PyPacket, accountID, stationID int64, sqldb *sql.DB) error {
	a := cs.ArgTuple.Items
	if len(a) < 11 {
		log.Printf("[%s] UpdateStationManagementSettings: %d args, want 11", addr, len(a))
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	u := evedb.StationManagementUpdate{
		Name:                     repString(a[3]),
		Description:              repString(a[4]),
		DockingCostPerVolume:     repFloat(a[5]),
		OfficeRentalCost:         int64(repFloat(a[7])),
		ReprocessingStationsTake: repFloat(a[9]),
		ReprocessingHangarFlag:   int64(repFloat(a[10])),
	}
	if d, ok := a[0].(marshal.PyDict); ok {
		for _, e := range d.Items {
			f := kvFields(e.Value)
			if f == nil {
				continue
			}
			u.Rules = append(u.Rules, evedb.ServiceAccessRule{
				ServiceID:           int64(repFloat(e.Key)),
				MinimumStanding:     repFloat(f["minimumStanding"]),
				MinimumCharSecurity: repFloat(f["minimumCharSecurity"]),
				MaximumCharSecurity: repFloat(f["maximumCharSecurity"]),
				MinimumCorpSecurity: repFloat(f["minimumCorpSecurity"]),
				MaximumCorpSecurity: repFloat(f["maximumCorpSecurity"]),
			})
		}
	}
	for _, row := range repList(a[1]) {
		if f := kvFields(row); f != nil {
			u.Modifiers = append(u.Modifiers, evedb.ServiceCostModifier{
				ServiceID:                    int64(repFloat(f["serviceID"])),
				DiscountPerGoodStandingPoint: repFloat(f["discountPerGoodStandingPoint"]),
				SurchargePerBadStandingPoint: repFloat(f["surchargePerBadStandingPoint"]),
			})
		}
	}
	for _, row := range repList(a[2]) {
		f := kvFields(row)
		if f == nil || int64(repFloat(f["typeID"])) != evedb.TypeOfficeFolder {
			continue // factory folders have no per-slot rental here
		}
		u.OfficeSlots = append(u.OfficeSlots, evedb.OfficeSlotPublic{
			Number: int64(repFloat(f["number"])),
			Public: repFloat(f["publiclyAvailable"]) != 0,
		})
	}

	renamed, redescribed, err := evedb.SaveStationManagement(ctx, sqldb, stationID, u)
	if err != nil {
		log.Printf("[%s] UpdateStationManagementSettings %d: %v", addr, stationID, err)
		return corpUserErr(c, pkt, accountID, "The station settings could not be saved.")
	}
	if renamed || redescribed {
		// A rename changes the celestial row; a new description can move the station
		// onto its own cloned operation, which map.GetStationExtraInfo's cache must see.
		if renamed && globalCelestialCache != nil {
			if d, derr := evedb.GetStationDetails(ctx, sqldb, stationID); derr == nil {
				globalCelestialCache.InvalidateSystem(d.SolarSystemID)
			}
		}
		mapsvc.InvalidateStationExtraInfo()
		if esp.StationServicesInvalidator != nil {
			esp.StationServicesInvalidator()
		}
	}
	log.Printf("[%s] UpdateStationManagementSettings station=%d rules=%d costMods=%d renamed=%v redescribed=%v",
		addr, stationID, len(u.Rules), len(u.Modifiers), renamed, redescribed)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// stationServiceStandingCheck is DoStandingCheckForStationService: "" to
// allow, else the refusal text (evedb.StationServiceRefusal).
func stationServiceStandingCheck(ctx context.Context, sqldb *sql.DB, stationID, charID, serviceID int64) string {
	return evedb.StationServiceRefusal(ctx, sqldb, stationID, charID, serviceID)
}

// ── small rep helpers ────────────────────────────────────────────────────────

func pyI(v int64) marshal.PyRep { return marshal.PyInt{Value: uint64(v)} }

// kv builds a util.KeyVal from name/value pairs.
func kv(pairs ...any) marshal.PyRep {
	items := make([]marshal.DictEntry, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		items = append(items, marshal.DictEntry{
			Key: marshal.PyString{Value: pairs[i].(string)}, Value: pairs[i+1].(marshal.PyRep)})
	}
	return marshal.PyObject{Type: "util.KeyVal", Args: marshal.PyDict{Items: items}}
}

func accessRuleKV(r evedb.ServiceAccessRule) marshal.PyRep {
	return kv("serviceID", pyI(r.ServiceID),
		"minimumStanding", marshal.PyReal{Value: r.MinimumStanding},
		"minimumCharSecurity", marshal.PyReal{Value: r.MinimumCharSecurity},
		"maximumCharSecurity", marshal.PyReal{Value: r.MaximumCharSecurity},
		"minimumCorpSecurity", marshal.PyReal{Value: r.MinimumCorpSecurity},
		"maximumCorpSecurity", marshal.PyReal{Value: r.MaximumCorpSecurity})
}

// kvFields reads a util.KeyVal (or bare dict) the client sent back.
func kvFields(rep marshal.PyRep) map[string]marshal.PyRep {
	var d marshal.PyDict
	switch v := rep.(type) {
	case marshal.PyObject:
		dd, ok := v.Args.(marshal.PyDict)
		if !ok {
			return nil
		}
		d = dd
	case marshal.PyDict:
		d = v
	default:
		return nil
	}
	out := make(map[string]marshal.PyRep, len(d.Items))
	for _, e := range d.Items {
		if k, ok := e.Key.(marshal.PyString); ok {
			out[k.Value] = e.Value
		}
	}
	return out
}

func repFloat(r marshal.PyRep) float64 {
	switch v := r.(type) {
	case marshal.PyReal:
		return v.Value
	case marshal.PyInt:
		return float64(int64(v.Value))
	case marshal.PyBool:
		if v.Value {
			return 1
		}
	}
	return 0
}

func repString(r marshal.PyRep) string {
	if s, ok := r.(marshal.PyString); ok {
		return s.Value
	}
	return ""
}

func repList(r marshal.PyRep) []marshal.PyRep {
	switch v := r.(type) {
	case marshal.PyList:
		return v.Items
	case marshal.PyTuple:
		return v.Items
	}
	return nil
}
