// Package posmgr implements the "posMgr" service (player-owned starbases). The client reaches it
// three ways: a bare service call (GetControlTowerFuelRequirements, also used docked), a bound
// object util.Moniker('posMgr', session.solarsystemid) for the management window, and an inline
// first call folded into MachoBindObject.
//
// Tower policy (Access, Force Field, Defense and Claim tabs) persists through
// db/starbasepolicy.go; enforcement sits behind starbase.enforceAccess.
// See docs/starbase-pos-investigation.md.
package posmgr

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"time"

	evedb "gurimoon/db"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
	"gurimoon/syncx"
)

// IsMethod reports whether method belongs to posMgr. The bound-call dispatcher (empty service
// name) and the inline-bind path use it; posMgr method names are unambiguous across services.
func IsMethod(method string) bool {
	switch method {
	// AnchorStructure is absent: it needs live system state, so main.go handles it before this package.
	case "GetControlTowerFuelRequirements",
		"SetTowerPassword", "SetShipPassword",
		"GetTowerSentrySettings", "SetTowerSentrySettings",
		"GetTowerClaim", "SetTowerClaim",
		"GetStarbasePermissions", "SetStarbasePermissions",
		"GetMoonForTower", "GetMoonProcessInfoForTower",
		"LinkResourceForTower", "ChangeStructureProvisionType",
		// Lower-case 'f' matches moonmining.py:1011; method names dispatch verbatim.
		"RunMoonProcessCycleforTower":
		return true
	}
	return false
}

// Result computes the reply for a posMgr call; handled is false for unknown methods. charID is
// the caller: settings writes are gated on it and the tower password check remembers it.
func Result(
	ctx context.Context, addr, method string, args []marshal.PyRep,
	charID int64, sqldb *sql.DB,
) (result marshal.PyRep, handled bool, err error) {
	switch method {
	case "GetControlTowerFuelRequirements":
		r, err := fuelRequirements(ctx, sqldb)
		return r, true, err

	case "GetTowerSentrySettings":
		// (standing, status, standingDrop, statusDrop, aggression, war): six values, moonmining.py:287.
		// None for a float is the client's unticked checkbox.
		p, _, err := towerPolicy(ctx, sqldb, argInt(args, 0))
		if err != nil {
			return nil, true, err
		}
		return sentryTuple(p.Sentry), true, nil

	case "GetTowerClaim":
		p, _, err := towerPolicy(ctx, sqldb, argInt(args, 0))
		if err != nil {
			return nil, true, err
		}
		return marshal.PyBool{Value: p.Claim}, true, nil

	case "GetStarbasePermissions":
		// (deployFlags, usageFlagsList), moonmining.py:403. deployFlags is indexed by four string keys;
		// usageFlagsList must be a util.Rowset (.Index('structureID')) whose rows come back edited.
		towerID := argInt(args, 0)
		p, _, err := towerPolicy(ctx, sqldb, towerID)
		if err != nil {
			return nil, true, err
		}
		deployFlags := marshal.PyDict{}
		for _, a := range evedb.StarbaseDeployActions {
			deployFlags.Items = append(deployFlags.Items, marshal.DictEntry{
				Key: marshal.PyString{Value: a}, Value: marshal.PyInt{Value: uint64(p.Deploy[a])},
			})
		}
		var lines []marshal.PyRep
		if towerID != 0 && sqldb != nil {
			rows, err := evedb.ListStructureAccess(ctx, sqldb, towerID)
			if err != nil {
				log.Printf("[%s] posMgr.GetStarbasePermissions %d: %v", addr, towerID, err)
			}
			for _, r := range rows {
				lines = append(lines, marshal.PyList{Items: []marshal.PyRep{
					marshal.PyInt{Value: uint64(r.StructureID)},
					marshal.PyInt{Value: uint64(r.ViewPut)},
					marshal.PyInt{Value: uint64(r.ViewPutTake)},
					marshal.PyInt{Value: uint64(r.Use)},
				}})
			}
		}
		usage := rowset([]string{"structureID", "viewput", "viewputtake", "use"}, lines)
		return marshal.PyTuple{Items: []marshal.PyRep{deployFlags, usage}}, true, nil

	case "SetTowerSentrySettings":
		// (towerID, standing, status, standingDrop, statusDrop, aggression, war), moonmining.py:675.
		// Stored and evaluated by db.SentryShouldEngage.
		towerID := argInt(args, 0)
		p, rec, err := towerPolicy(ctx, sqldb, towerID)
		if err != nil {
			return nil, true, err
		}
		if err := mayConfigure(ctx, sqldb, rec, charID, false); err != nil {
			return nil, true, err
		}
		p.Sentry = evedb.SentrySettings{
			Standing:     argFloatPtr(args, 1),
			Status:       argFloatPtr(args, 2),
			StandingDrop: argBool(args, 3),
			StatusDrop:   argBool(args, 4),
			Aggression:   argBool(args, 5),
			War:          argBool(args, 6),
		}
		if err := evedb.SaveStarbasePolicy(ctx, sqldb, p); err != nil {
			return nil, true, err
		}
		log.Printf("[%s] posMgr.SetTowerSentrySettings tower=%d %+v", addr, towerID, p.Sentry)
		return marshal.PyNone{}, true, nil

	case "SetTowerClaim":
		// (towerID, claim), moonmining.py:736. Stored only; sovereignty follows the majority rule.
		towerID := argInt(args, 0)
		p, rec, err := towerPolicy(ctx, sqldb, towerID)
		if err != nil {
			return nil, true, err
		}
		if err := mayConfigure(ctx, sqldb, rec, charID, false); err != nil {
			return nil, true, err
		}
		p.Claim = argBool(args, 1)
		if err := evedb.SaveStarbasePolicy(ctx, sqldb, p); err != nil {
			return nil, true, err
		}
		log.Printf("[%s] posMgr.SetTowerClaim tower=%d claim=%v", addr, towerID, p.Claim)
		return marshal.PyNone{}, true, nil

	case "SetStarbasePermissions":
		// (towerID, deployFlags, usageFlagsList), moonmining.py:430: the dict and rowset from
		// GetStarbasePermissions, edited in place. The Access tab itself needs the starbase config role.
		towerID := argInt(args, 0)
		p, rec, err := towerPolicy(ctx, sqldb, towerID)
		if err != nil {
			return nil, true, err
		}
		if err := mayConfigure(ctx, sqldb, rec, charID, true); err != nil {
			return nil, true, err
		}
		if len(args) >= 2 {
			if d, ok := args[1].(marshal.PyDict); ok {
				for _, e := range d.Items {
					if k, ok := e.Key.(marshal.PyString); ok {
						if _, known := p.Deploy[k.Value]; known {
							p.Deploy[k.Value] = int(repInt(e.Value))
						}
					}
				}
			}
		}
		if err := evedb.SaveStarbasePolicy(ctx, sqldb, p); err != nil {
			return nil, true, err
		}
		saved := 0
		if len(args) >= 3 {
			if header, lines, ok := server.ParseRowsetArg(args[2]); ok {
				col := map[string]int{}
				for i, h := range header {
					col[h] = i
				}
				get := func(line []marshal.PyRep, name string) int64 {
					if i, ok := col[name]; ok && i < len(line) {
						return repInt(line[i])
					}
					return evedb.StarbaseAccessCorporation
				}
				var rows []evedb.StructureAccess
				for _, line := range lines {
					rows = append(rows, evedb.StructureAccess{
						StructureID: get(line, "structureID"), TowerID: towerID,
						ViewPut: int(get(line, "viewput")), ViewPutTake: int(get(line, "viewputtake")),
						Use: int(get(line, "use")),
					})
				}
				if saved, err = evedb.SaveStructureAccess(ctx, sqldb, towerID, rows); err != nil {
					return nil, true, err
				}
			} else {
				log.Printf("[%s] posMgr.SetStarbasePermissions tower=%d: usage list not a rowset: %#v", addr, towerID, args[2])
			}
		}
		log.Printf("[%s] posMgr.SetStarbasePermissions tower=%d deploy=%v structures=%d", addr, towerID, p.Deploy, saved)
		return marshal.PyNone{}, true, nil

	case "GetMoonForTower":
		// (moonID, [(typeID, quantity), ...]), moonmining.py:750-759. None gives the header "(not attached
		// to moon)" and an empty Moon tab with no client error, so every None exit logs its cause.
		towerID := argInt(args, 0)
		if towerID == 0 || sqldb == nil {
			log.Printf("[%s] posMgr.GetMoonForTower: no tower in args → None", addr)
			return marshal.PyNone{}, true, nil
		}
		rec, ok, err := evedb.GetStarbase(ctx, sqldb, towerID)
		switch {
		case err != nil:
			log.Printf("[%s] posMgr.GetMoonForTower: tower %d lookup: %v → None", addr, towerID, err)
			return marshal.PyNone{}, true, nil
		case !ok:
			// Correct for an unanchored structure; a bug for an anchored one.
			log.Printf("[%s] posMgr.GetMoonForTower: item %d has no starbase row (not anchored?) → None", addr, towerID)
			return marshal.PyNone{}, true, nil
		case rec.MoonID == 0:
			log.Printf("[%s] posMgr.GetMoonForTower: starbase %d has moonID 0 — anchored without a moon → None", addr, towerID)
			return marshal.PyNone{}, true, nil
		}
		res, rerr := evedb.GetMoonResources(ctx, sqldb, rec.MoonID)
		if rerr != nil {
			log.Printf("[%s] posMgr.GetMoonForTower: moon %d: %v → None", addr, rec.MoonID, rerr)
			return marshal.PyNone{}, true, nil
		}
		pairs := make([]marshal.PyRep, 0, len(res))
		for _, r := range res {
			pairs = append(pairs, marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyInt{Value: uint64(r.TypeID)},
				marshal.PyInt{Value: uint64(r.Quantity)},
			}})
		}
		return marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(rec.MoonID)},
			marshal.PyList{Items: pairs},
		}}, true, nil

	case "GetMoonProcessInfoForTower":
		r, err := moonProcessInfo(ctx, addr, sqldb, argInt(args, 0))
		return r, true, err

	case "ChangeStructureProvisionType":
		// (towerID, structureItemID, typeID or None): what a harvester pulls or a silo stores; None
		// clears it.
		structureID := argInt(args, 1)
		typeID := argInt(args, 2) // None is 0, the clear
		if structureID == 0 || sqldb == nil {
			return marshal.PyNone{}, true, nil
		}
		// A harvester accepts only the 20 moon materials (the client offers nothing else); silos also
		// store reaction products.
		if typeID != 0 && !evedb.IsMoonMaterial(typeID) {
			if er, _ := evedb.GetEntityRow(ctx, sqldb, structureID); er != nil && er.GroupID == evedb.GroupMoonMining {
				log.Printf("[%s] posMgr.ChangeStructureProvisionType %d: type %d is not a moon material — refused",
					addr, structureID, typeID)
				return marshal.PyNone{}, true, nil
			}
		}
		if err := evedb.SetStructureProvisionType(ctx, sqldb, structureID, typeID); err != nil {
			log.Printf("[%s] posMgr.ChangeStructureProvisionType %d: %v", addr, structureID, err)
			return marshal.PyNone{}, true, nil
		}
		log.Printf("[%s] posMgr.ChangeStructureProvisionType structure=%d type=%d", addr, structureID, typeID)
		return marshal.PyNone{}, true, nil

	case "LinkResourceForTower":
		// (towerID, connections). Apply sends the whole structureConnections dict, so this replaces the
		// tower's wiring. A falsy answer makes the client reload its Production tab from the server.
		towerID := argInt(args, 0)
		if towerID == 0 || sqldb == nil || len(args) < 2 {
			return marshal.PyNone{}, true, nil
		}
		links, shape := parseStructureLinks(args[1])
		if links == nil {
			// Unrecognised shape: log the raw payload. ProdApply's list comprehension decompiles to a
			// placeholder, so the log is the only specification of the element shape.
			log.Printf("[%s] posMgr.LinkResourceForTower tower=%d UNPARSED payload: %#v",
				addr, towerID, args[1])
			return marshal.PyNone{}, true, nil
		}
		if err := evedb.SetStructureLinks(ctx, sqldb, towerID, links); err != nil {
			log.Printf("[%s] posMgr.LinkResourceForTower %d: %v", addr, towerID, err)
			return marshal.PyNone{}, true, nil
		}
		log.Printf("[%s] posMgr.LinkResourceForTower tower=%d: %d link(s) (%s)",
			addr, towerID, len(links), shape)
		return marshal.PyNone{}, true, nil

	case "SetTowerPassword":
		// 2 args: pwn.EnterTowerPassword (playerOwned.py:110), a pilot presenting the secret, checked
		// and never stored. 4 args: the Force Field tab (moonmining.py:717), the owner setting it.
		towerID := argInt(args, 0)
		if towerID == 0 || sqldb == nil {
			return marshal.PyNone{}, true, nil
		}
		if len(args) < 4 {
			ok, err := evedb.CheckStarbasePassword(ctx, sqldb, towerID, argString(args, 1))
			if err != nil {
				return nil, true, err
			}
			if ok {
				rememberPassword(towerID, charID)
			}
			log.Printf("[%s] posMgr.SetTowerPassword tower=%d char=%d presented password (correct=%v)", addr, towerID, charID, ok)
			return marshal.PyNone{}, true, nil
		}
		p, rec, err := towerPolicy(ctx, sqldb, towerID)
		if err != nil {
			return nil, true, err
		}
		if err := mayConfigure(ctx, sqldb, rec, charID, false); err != nil {
			return nil, true, err
		}
		// False means "keep the old password" (the empty-field dialog, moonmining.py:710); only a string
		// replaces it.
		if pw, isStr := args[1].(marshal.PyString); isStr {
			if err := evedb.SetStarbasePassword(ctx, sqldb, towerID, pw.Value); err != nil {
				return nil, true, err
			}
		}
		p.FFAllowCorp = argBool(args, 2)
		p.FFAllowAlliance = argBool(args, 3)
		if err := evedb.SaveStarbasePolicy(ctx, sqldb, p); err != nil {
			return nil, true, err
		}
		if ForcefieldChanged != nil {
			ForcefieldChanged(towerID)
		}
		log.Printf("[%s] posMgr.SetTowerPassword tower=%d allowCorp=%v allowAlliance=%v (stored)",
			addr, towerID, p.FFAllowCorp, p.FFAllowAlliance)
		return marshal.PyNone{}, true, nil

	case "RunMoonProcessCycleforTower":
		// GM "Run Cycle" (moonmining.py RunProcessCycle): one production pass ignoring cycle timers; the
		// tab redraws from the OnMoonProcessChange it pushes. Owners only, since the button is no gate.
		towerID := argInt(args, 0)
		rec, found, err := evedb.GetStarbase(ctx, sqldb, towerID)
		if err != nil || !found {
			return marshal.PyNone{}, true, nil
		}
		_, corpID, allianceID, err := evedb.GetCharSovContext(ctx, sqldb, charID)
		if err != nil || !evedb.StarbaseManagePermitted(rec, corpID, allianceID) {
			return nil, true, refusal("You cannot run this tower's production.")
		}
		out := "production is not wired"
		if RunProcessCycle != nil {
			out = RunProcessCycle(towerID)
		}
		log.Printf("[%s] posMgr.RunMoonProcessCycleforTower %d by char %d: %s", addr, towerID, charID, out)
		return marshal.PyNone{}, true, nil

	case "SetShipPassword":
		// Stub: accepted and dropped. A falsy return makes the client reload its production tab.
		log.Printf("[%s] posMgr.%s (stub — not persisted)", addr, method)
		return marshal.PyNone{}, true, nil
	}
	return nil, false, nil
}

// RunProcessCycle is set by the server (cmd/gurimoon/moonharvest.go, which owns the sweep) and
// called by Run Cycle: one forced production pass (harvesters, then reactors), returning a
// summary. Nil leaves Run Cycle inert.
var RunProcessCycle func(towerID int64) string

// MoonProcessInfo is GetMoonProcessInfoForTower's answer, also used by the
// OnMoonProcessChange push.
func MoonProcessInfo(ctx context.Context, addr string, sqldb *sql.DB, towerID int64) (marshal.PyRep, error) {
	return moonProcessInfo(ctx, addr, sqldb, towerID)
}

// Handle dispatches a CallReq addressed to the "posMgr" service (bare-service
// or bound-object form).
func Handle(
	c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket,
	accountID, charID int64, sqldb *sql.DB,
) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, handled, err := Result(ctx, addr, cs.Method, cs.ArgTuple.Items, charID, sqldb)
	if !handled {
		server.RecordUnhandled(addr, "posMgr", cs, true)
		log.Printf("[%s] posMgr: unhandled method %q — returning None", addr, cs.Method)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	var ref refusal
	if errors.As(err, &ref) {
		log.Printf("[%s] posMgr.%s refused: %s", addr, cs.Method, string(ref))
		return server.SendCallException(c, pkt, accountID, server.BuildUserError(
			"CustomInfo", map[string]marshal.PyRep{"info": marshal.PyString{Value: string(ref)}}))
	}
	if err != nil {
		log.Printf("[%s] posMgr.%s: %v", addr, cs.Method, err)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	log.Printf("[%s] → posMgr.%s", addr, cs.Method)
	return server.SendCallRsp(c, pkt, accountID, result)
}

// fuelRequirements builds the util.Rowset for GetControlTowerFuelRequirements.
// The client reads columns by name (controlTowerTypeID, purpose, resourceTypeID,
// quantity, factionID, minSecurityLevel) and filters by controlTowerTypeID
// itself.  purpose values outside 1-4 are silently dropped by the UI.
func fuelRequirements(ctx context.Context, sqldb *sql.DB) (marshal.PyRep, error) {
	cols := []string{
		"controlTowerTypeID", "resourceTypeID", "purpose",
		"quantity", "minSecurityLevel", "factionID",
	}
	if sqldb == nil {
		return rowset(cols, nil), nil
	}
	recs, err := evedb.GetControlTowerFuelRequirements(ctx, sqldb)
	if err != nil {
		return rowset(cols, nil), err
	}
	lines := make([]marshal.PyRep, 0, len(recs))
	for _, r := range recs {
		minSec := marshal.PyRep(marshal.PyNone{})
		if r.MinSecurityLevel.Valid {
			minSec = marshal.PyReal{Value: r.MinSecurityLevel.Float64}
		}
		faction := marshal.PyRep(marshal.PyNone{})
		if r.FactionID.Valid {
			faction = marshal.PyInt{Value: uint64(r.FactionID.Int64)}
		}
		lines = append(lines, marshal.PyList{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(r.ControlTowerTypeID)},
			marshal.PyInt{Value: uint64(r.ResourceTypeID)},
			marshal.PyInt{Value: uint64(r.Purpose)},
			marshal.PyInt{Value: uint64(r.Quantity)},
			minSec,
			faction,
		}})
	}
	return rowset(cols, lines), nil
}

// moonProcessInfo builds GetMoonProcessInfoForTower's answer: one six-element tuple per production
// structure, (itemID, active, reaction, connections, demands, supplies). Only groups 416/404/438
// may appear (the tab's sort keyfunc indexes exactly those, moonmining.py:207). `reaction` is
// (itemID, reactionTypeID) for a reactor and (used, capacity) for a silo: branch on groupID.
func moonProcessInfo(ctx context.Context, addr string, sqldb *sql.DB, towerID int64) (marshal.PyRep, error) {
	if towerID == 0 || sqldb == nil {
		return marshal.PyList{}, nil
	}
	structures, err := evedb.ListStarbaseStructuresForTower(ctx, sqldb, towerID)
	if err != nil {
		log.Printf("[%s] posMgr.GetMoonProcessInfoForTower %d: %v", addr, towerID, err)
		return marshal.PyList{}, nil
	}
	links, err := evedb.ListStructureLinks(ctx, sqldb, towerID)
	if err != nil {
		log.Printf("[%s] posMgr.GetMoonProcessInfoForTower links %d: %v", addr, towerID, err)
	}

	out := make([]marshal.PyRep, 0, len(structures))
	for _, s := range structures {
		if !evedb.IsProductionStructureGroup(s.GroupID) {
			continue
		}
		active := marshal.PyRep(marshal.PyInt{Value: 0})
		if s.State == evedb.StarbaseStateOnline {
			active = marshal.PyInt{Value: 1}
		}

		// connections: what feeds this structure, as (sourceItemID, typeID); the client rebuilds
		// structureConnections[(typeID, sourceID)] = itemID.
		conns := []marshal.PyRep{}
		for _, l := range links {
			if l.DestID != s.EntityID {
				continue
			}
			conns = append(conns, marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyInt{Value: uint64(l.SourceID)},
				marshal.PyInt{Value: uint64(l.TypeID)},
			}})
		}

		reaction := marshal.PyRep(marshal.PyNone{})
		demands := marshal.PyRep(marshal.PyNone{})
		supplies := marshal.PyRep(marshal.PyNone{})

		switch s.GroupID {
		case evedb.GroupMoonMining:
			// A harvester supplies its material and demands nothing. The client replaces -1 with
			// min(harvesterQuality, abundance) for display, so both sides compute the real rate.
			if s.ProvisionTypeID != 0 {
				supplies = typeQtyList(s.ProvisionTypeID, -1)
			}
		case evedb.GroupSilo:
			if s.ProvisionTypeID != 0 {
				demands = typeQtyList(s.ProvisionTypeID, -1)
				supplies = typeQtyList(s.ProvisionTypeID, -1)
			}
			// The gauge is (used, capacity) in units of the material (the Silo row prints "%s/%s").
			used, capacity, sErr := evedb.SiloUnits(ctx, sqldb, s.EntityID, s.TypeID, s.ProvisionTypeID)
			if sErr == nil {
				reaction = marshal.PyTuple{Items: []marshal.PyRep{
					marshal.PyInt{Value: uint64(used)}, marshal.PyInt{Value: uint64(capacity)},
				}}
			}
		case evedb.GroupMobileReactor:
			// The reaction is the category-24 item in the reactor, sent as (itemID, reactionTypeID). Demands
			// and supplies stay None: the client derives both from cfg.invreactiontypes.
			if itemID, rTypeID, ok, rErr := evedb.ReactorReaction(ctx, sqldb, s.EntityID); rErr == nil && ok {
				reaction = marshal.PyTuple{Items: []marshal.PyRep{
					marshal.PyInt{Value: uint64(itemID)}, marshal.PyInt{Value: uint64(rTypeID)},
				}}
			}
		}

		out = append(out, marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(s.EntityID)},
			active,
			reaction,
			marshal.PyList{Items: conns},
			demands,
			supplies,
		}})
	}
	return marshal.PyList{Items: out}, nil
}

// typeQtyList builds the [(typeID, quantity)] shape demands/supplies use.
func typeQtyList(typeID int64, qty int64) marshal.PyRep {
	return marshal.PyList{Items: []marshal.PyRep{
		marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(typeID)},
			marshal.PyInt{Value: uint64(qty)},
		}},
	}}
}

// parseStructureLinks reads LinkResourceForTower's connection payload. The element shape is not
// recoverable from the decompile, so it accepts the three plausible encodings of
// structureConnections[(typeID, sourceItemID)] = destItemID and names the one used in the log. It
// returns nil when nothing matched, so the caller dumps the payload instead of clearing wiring.
func parseStructureLinks(rep marshal.PyRep) ([]evedb.StructureLink, string) {
	toInt := func(r marshal.PyRep) (int64, bool) {
		v, ok := r.(marshal.PyInt)
		return int64(v.Value), ok
	}

	// {(typeID, sourceID): destID}
	if d, ok := rep.(marshal.PyDict); ok {
		links := []evedb.StructureLink{}
		for _, e := range d.Items {
			key, kOK := e.Key.(marshal.PyTuple)
			dest, dOK := toInt(e.Value)
			if !kOK || !dOK || len(key.Items) != 2 {
				return nil, ""
			}
			typeID, t1 := toInt(key.Items[0])
			source, t2 := toInt(key.Items[1])
			if !t1 || !t2 {
				return nil, ""
			}
			links = append(links, evedb.StructureLink{TypeID: typeID, SourceID: source, DestID: dest})
		}
		return links, "dict"
	}

	items, ok := repItems(rep)
	if !ok {
		return nil, ""
	}
	links := []evedb.StructureLink{}
	shape := "empty"
	for _, it := range items {
		t, tOK := it.(marshal.PyTuple)
		if !tOK {
			return nil, ""
		}
		switch len(t.Items) {
		case 3: // (typeID, sourceID, destID)
			typeID, a := toInt(t.Items[0])
			source, b := toInt(t.Items[1])
			dest, c := toInt(t.Items[2])
			if !a || !b || !c {
				return nil, ""
			}
			links = append(links, evedb.StructureLink{TypeID: typeID, SourceID: source, DestID: dest})
			shape = "triple"
		case 2: // ((typeID, sourceID), destID)
			key, kOK := t.Items[0].(marshal.PyTuple)
			dest, dOK := toInt(t.Items[1])
			if !kOK || !dOK || len(key.Items) != 2 {
				return nil, ""
			}
			typeID, a := toInt(key.Items[0])
			source, b := toInt(key.Items[1])
			if !a || !b {
				return nil, ""
			}
			links = append(links, evedb.StructureLink{TypeID: typeID, SourceID: source, DestID: dest})
			shape = "nested"
		default:
			return nil, ""
		}
	}
	return links, shape
}

// repItems unpacks a PyList or PyTuple into its elements.
func repItems(rep marshal.PyRep) ([]marshal.PyRep, bool) {
	switch v := rep.(type) {
	case marshal.PyList:
		return v.Items, true
	case marshal.PyTuple:
		return v.Items, true
	case marshal.PyNone:
		return nil, true
	}
	return nil, false
}

// ── tower settings helpers ──────────────────────────────────────────────────

// ForcefieldChanged is set by main and called after a tower's force-field flags change, so the
// ball is re-presented (the Force Field tab reads the flags off the ball, not from an RPC).
var ForcefieldChanged func(towerID int64)

// refusal is an error Handle turns into a CustomInfo UserError the pilot sees.
type refusal string

func (r refusal) Error() string { return string(r) }

// towerPolicy loads a tower and its stored (or default) policy. A towerID
// that is not an anchored tower answers the default policy of an empty
// record, so the window still draws.
func towerPolicy(ctx context.Context, sqldb *sql.DB, towerID int64) (evedb.StarbasePolicy, evedb.StarbaseRecord, error) {
	if sqldb == nil || towerID == 0 {
		return evedb.DefaultStarbasePolicy(evedb.StarbaseRecord{EntityID: towerID}), evedb.StarbaseRecord{}, nil
	}
	rec, ok, err := evedb.GetStarbase(ctx, sqldb, towerID)
	if err != nil {
		return evedb.StarbasePolicy{}, rec, err
	}
	if !ok {
		return evedb.DefaultStarbasePolicy(evedb.StarbaseRecord{EntityID: towerID}), rec, nil
	}
	p, err := evedb.GetStarbasePolicy(ctx, sqldb, rec)
	return p, rec, err
}

// mayConfigure gates settings writes on StarbaseManagePermitted (owner alliance, or owner corp
// for an empire tower), plus the Config Starbase Equipment role where the client demands it (the
// Access tab). It gates editing, so it applies with starbase.enforceAccess off.
func mayConfigure(ctx context.Context, sqldb *sql.DB, rec evedb.StarbaseRecord, charID int64, needRole bool) error {
	if sqldb == nil {
		return nil
	}
	if rec.EntityID == 0 {
		return refusal("That is not an anchored control tower.")
	}
	a, err := evedb.LoadStarbaseActor(ctx, sqldb, charID)
	if err != nil {
		return err
	}
	if !evedb.StarbaseManagePermitted(rec, a.CorpID, a.AllianceID) {
		return refusal("You are not permitted to configure this control tower.")
	}
	if needRole && !evedb.AccessLevelAllows(evedb.StarbaseAccessRole, a, a.CorpID, 0) {
		return refusal("Configuring starbase access requires the Config Starbase Equipment role.")
	}
	return nil
}

// Passwords a pilot has answered correctly since the server started. Memory-only, like the
// client's ship passwords, which clear on logout.
var passwordKeys syncx.Map[[2]int64, struct{}] // [2]int64{towerID, charID} → struct{}

func rememberPassword(towerID, charID int64) {
	if charID != 0 {
		passwordKeys.Store([2]int64{towerID, charID}, struct{}{})
	}
}

// PasswordAccepted reports whether charID has presented towerID's current
// password (EnterTowerPassword) since the server started.
func PasswordAccepted(towerID, charID int64) bool {
	_, ok := passwordKeys.Load([2]int64{towerID, charID})
	return ok
}

func sentryTuple(s evedb.SentrySettings) marshal.PyRep {
	f := func(v *float64) marshal.PyRep {
		if v == nil {
			return marshal.PyNone{}
		}
		return marshal.PyReal{Value: *v}
	}
	return marshal.PyTuple{Items: []marshal.PyRep{
		f(s.Standing), f(s.Status),
		marshal.PyBool{Value: s.StandingDrop}, marshal.PyBool{Value: s.StatusDrop},
		marshal.PyBool{Value: s.Aggression}, marshal.PyBool{Value: s.War},
	}}
}

// repInt reads an int-ish value (PyInt, PyBool, PyReal); 0 otherwise.
func repInt(r marshal.PyRep) int64 {
	switch v := r.(type) {
	case marshal.PyInt:
		return int64(v.Value)
	case marshal.PyBool:
		if v.Value {
			return 1
		}
	case marshal.PyReal:
		return int64(v.Value)
	}
	return 0
}

// argBool reads a checkbox value, which arrives as a bool or an int.
func argBool(args []marshal.PyRep, idx int) bool {
	return idx < len(args) && repInt(args[idx]) != 0
}

// argFloatPtr reads an optional float: None (unticked) → nil.
func argFloatPtr(args []marshal.PyRep, idx int) *float64 {
	if idx >= len(args) {
		return nil
	}
	var f float64
	switch v := args[idx].(type) {
	case marshal.PyReal:
		f = v.Value
	case marshal.PyInt:
		f = float64(int64(v.Value))
	default:
		return nil
	}
	return &f
}

// argInt extracts a PyInt argument (0 when absent or not an int).
func argInt(args []marshal.PyRep, idx int) int64 {
	if idx >= len(args) {
		return 0
	}
	if v, ok := args[idx].(marshal.PyInt); ok {
		return int64(v.Value)
	}
	return 0
}

// argString extracts a PyString argument ("" when absent, None or not a string; a None password
// clears the stored one).
func argString(args []marshal.PyRep, idx int) string {
	if idx >= len(args) {
		return ""
	}
	if v, ok := args[idx].(marshal.PyString); ok {
		return v.Value
	}
	return ""
}

// rowset builds a util.Rowset with util.Row lines, the only flavour that supports .Index(),
// which the client calls on posMgr results.
func rowset(cols []string, lines []marshal.PyRep) marshal.PyRep {
	header := make([]marshal.PyRep, len(cols))
	for i, col := range cols {
		header[i] = marshal.PyString{Value: col}
	}
	headerList := marshal.PyList{Items: header}
	if lines == nil {
		lines = []marshal.PyRep{}
	}
	return marshal.PyObject{
		Type: "util.Rowset",
		Args: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "header"}, Value: headerList},
			{Key: marshal.PyString{Value: "RowClass"}, Value: marshal.PyString{Value: "util.Row", IsType1: true}},
			{Key: marshal.PyString{Value: "lines"}, Value: marshal.PyList{Items: lines}},
			{Key: marshal.PyString{Value: "columns"}, Value: headerList},
		}},
	}
}
