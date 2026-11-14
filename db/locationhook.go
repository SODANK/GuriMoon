package db

// LiveLocationHook answers config.GetMultiLocationsEx for an ID no table
// holds (an NPC, drone or other ball that exists only in a loaded system).
// cfg.evelocations.Get raises on a miss. Nil by default; the game server
// installs it at boot (cmd/gurimoon/livelocations.go).
var LiveLocationHook func(id int64) (name string, x, y, z float64, ok bool)
