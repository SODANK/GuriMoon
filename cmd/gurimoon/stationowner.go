package main

// OnStationOwnerChanged(ownerID) tells a docked pilot the station changed hands.
// remote/connectionService.py takes it and ui/station/base.py reloads the lobby, where the owner's
// name, the STATION MGMT. button and the service access rules are read. The ball push reaches
// only pilots in space.

import (
	"context"
	"database/sql"
	"time"

	"gurimoon/esp"
	"gurimoon/marshal"
	"gurimoon/server"
	"gurimoon/server/infocache"
	"gurimoon/server/mapsvc"
	"gurimoon/tick"
)

// init registers a boot hook that sets esp.StationOwnerChangedHook, which the esp package calls
// when a station's owner changes.
func init() {
	moduleBootHooks = append(moduleBootHooks, func(sqldb *sql.DB, sessReg *esp.SessionRegistry, _ *tick.Manager) {
		esp.StationOwnerChangedHook = func(stationID, ownerID int64) {
			var systemID int64
			if sqldb != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = sqldb.QueryRowContext(ctx,
					`SELECT solarSystemID FROM staStations WHERE stationID = ?`, stationID).Scan(&systemID)
				cancel()
			}
			refreshStationOwnerCaches(systemID)
			pushStationOwnerChanged(sessReg, stationID, ownerID)
		}
	})
}

// refreshStationOwnerCaches drops every cache that answers a station's owner
// from a snapshot: the system's celestial rows (staStations.corporationID),
// map.GetStationExtraInfo, the owner-name bulk and the identity memo. Call it
// after any staStations.corporationID change; systemID 0 skips the celestials.
func refreshStationOwnerCaches(systemID int64) {
	if systemID != 0 && globalCelestialCache != nil {
		globalCelestialCache.InvalidateSystem(systemID)
	}
	mapsvc.InvalidateStationExtraInfo()
	if globalCacheStore != nil {
		globalCacheStore.InvalidateOwnerNames()
		infocache.Shared.Clear()
	}
}

// pushStationOwnerChanged tells every pilot docked at stationID who owns it
// now. Scope "stationid": the client routes it by the station it is docked
// at, and the registry's StationID is kept current by updateSessionLocation.
func pushStationOwnerChanged(sessReg *esp.SessionRegistry, stationID, ownerID int64) {
	if sessReg == nil {
		return
	}
	server.NotifyStation(sessReg, stationID, "OnStationOwnerChanged", "stationid",
		marshal.PyTuple{Items: []marshal.PyRep{marshal.PyInt{Value: uint64(ownerID)}}})
}
