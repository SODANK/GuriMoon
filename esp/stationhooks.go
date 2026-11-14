package esp

// StationOwnerChangedHook is set by the game server and called after a station
// transfer (/outposts). It pushes OnStationOwnerChanged to docked pilots
// (ui/station/base.py reloads the lobby). nil when no server is attached.
var StationOwnerChangedHook func(stationID, ownerID int64)
