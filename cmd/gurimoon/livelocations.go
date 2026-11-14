package main

// The live half of db.LiveLocationHook: a ball that exists only in a loaded
// solar system (NPC, drone, anything minted from the 700M+ bands) answers
// config.GetMultiLocationsEx with its ball name and position, so a scan
// result the client has no slim for can be named instead of raising.

import (
	"context"
	"database/sql"
	"sync"
	"time"

	evedb "gurimoon/db"
	"gurimoon/esp"
	"gurimoon/tick"
)

func init() {
	moduleBootHooks = append(moduleBootHooks, func(sqldb *sql.DB, _ *esp.SessionRegistry, _ *tick.Manager) {
		names := &typeNameCache{db: sqldb, names: map[int64]string{}}
		evedb.LiveLocationHook = func(id int64) (string, float64, float64, float64, bool) {
			return liveLocation(id, names)
		}
	})
}

// typeNameCache memoises invTypes.typeName for the fallback name of a ball
// with no name of its own (an NPC ball's slim carries only its typeID).
type typeNameCache struct {
	db    *sql.DB
	mu    sync.Mutex
	names map[int64]string
}

func (t *typeNameCache) name(typeID int64) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if n, ok := t.names[typeID]; ok {
		return n
	}
	n := ""
	if t.db != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		n, _ = evedb.GetTypeName(ctx, t.db, typeID)
		cancel()
	}
	t.names[typeID] = n
	return n
}

func liveLocation(id int64, names *typeNameCache) (string, float64, float64, float64, bool) {
	if globalSysMgr == nil || id <= 0 || id > 1<<32-1 {
		return "", 0, 0, 0, false
	}
	for _, ss := range globalSysMgr.All() {
		e := ss.GetEntity(uint32(id))
		if e == nil {
			continue
		}
		name := e.BallName()
		if name == "" {
			name = names.name(e.Slim.TypeID)
		}
		p := e.Pos()
		return name, p.X, p.Y, p.Z, true
	}
	// Balls with no entity (probes, open-site and cyno beacons, anything in the loot or starbase
	// slices) are named here too, because a scan result this call cannot name wedges the scanner window.
	for _, ss := range globalSysMgr.All() {
		bd, slim, ok := ss.SliceBall(uint32(id))
		if !ok {
			continue
		}
		name := bd.Name
		if name == "" {
			name = names.name(slim.TypeID)
		}
		return name, bd.Pos.X, bd.Pos.Y, bd.Pos.Z, true
	}
	return "", 0, 0, 0, false
}
