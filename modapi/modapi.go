// Package modapi is the compile-time module registry. A module is a package under
// modules/<name>/ that registers itself from init(); enabled modules are compiled in
// through blank imports in modules/modules.go, which rebuild.ps1 generates.
//
// Lifecycle: each module calls Register from init(). main() builds a Context once the
// DB, config, tick manager and registries exist, then calls InitAll, where modules
// create tables, register tick callbacks and add ESP pages via esp.ModuleHandlers.
// Core code calls the Fire* hooks, and a module opts in by implementing the matching
// optional interface.
package modapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"

	"gurimoon/destiny"
	"gurimoon/esp"
	"gurimoon/npc"
	"gurimoon/server/lsc"
	"gurimoon/server/station"
	"gurimoon/tick"
)

// Module is the interface every module must implement.
type Module interface {
	// Name is the module's stable identifier. It selects the module's
	// config block: config.json "modules": {"<name>": {...}}.
	Name() string
	// Init is called once at startup with the shared Context. Create
	// tables, register tick callbacks, and wire ESP pages here. Returning
	// an error aborts server startup.
	Init(ctx *Context) error
}

// Context is what a module gets to work with. All fields are shared,
// process-wide singletons owned by main().
type Context struct {
	DB       *sql.DB
	Config   json.RawMessage // this module's block from config.json "modules", nil if absent
	NodeID   uint64
	Tick     *tick.Manager
	LSC      *lsc.Registry            // chat channels (JoinBot/LeaveBot for synthetic members)
	Stations *station.StationRegistry // station guest lists (JoinBot/LeaveBot likewise)
	Systems  *destiny.SystemManager
	NPC      *npc.NPCManager // NPC AI/damage/death entry points (NotifyDamage, Kill, IsNPC)
	// Spawner owns belt population. EnsureBeltWave answers what is at a belt, including in
	// a system no client has loaded, where the balls do not exist but the rats do.
	Spawner  *npc.SpawnerManager
	Sessions *esp.SessionRegistry // live player sessions (FindByCharID; bots are not in it)
}

// SystemLoadedHook is implemented by modules that want a callback when a solar system
// becomes live. It fires on every player system entry, so it must be idempotent.
type SystemLoadedHook interface {
	OnSystemLoaded(ss *destiny.SolarSystem)
}

// CharacterSelectGate is implemented by modules that can veto a character
// selection. Returning a non-nil error refuses the login; the error text is
// shown to the player.
type CharacterSelectGate interface {
	CharacterSelectGate(ctx context.Context, db *sql.DB, charID int64) error
}

// EntityRemovedHook is implemented by modules that react when a player entity is
// destroyed and removed from a solar system. killerID is the attacker's entity ID, or
// 0 when there is none.
type EntityRemovedHook interface {
	OnEntityRemoved(ss *destiny.SolarSystem, entityID uint32, killerID uint32)
}

// SystemUnloadingHook is implemented by modules that clean up (for example
// de-embodying bots) when a solar system is torn down because no real players remain.
type SystemUnloadingHook interface {
	OnSystemUnloading(solarSystemID int64)
}

var registered []Module

// Register adds a module to the registry. Call from the module package's
// init(). Duplicate names are a startup error caught by InitAll.
func Register(m Module) {
	registered = append(registered, m)
}

// All returns the registered modules in registration order.
func All() []Module {
	return registered
}

// InitAll initialises every registered module. moduleConfigs is the raw
// "modules" map from config.json (may be nil). The passed Context is
// copied per-module with that module's config block filled in.
func InitAll(base Context, moduleConfigs map[string]json.RawMessage) error {
	seen := map[string]bool{}
	for _, m := range registered {
		name := m.Name()
		if seen[name] {
			log.Fatalf("module: duplicate module name %q", name)
		}
		seen[name] = true
		ctx := base
		ctx.Config = moduleConfigs[name]
		if err := m.Init(&ctx); err != nil {
			return err
		}
		log.Printf("module: %s initialised", name)
	}
	return nil
}

// FireSystemLoaded notifies every SystemLoadedHook module that ss is live.
func FireSystemLoaded(ss *destiny.SolarSystem) {
	for _, m := range registered {
		if h, ok := m.(SystemLoadedHook); ok {
			h.OnSystemLoaded(ss)
		}
	}
}

// FireCharacterSelectGate asks every CharacterSelectGate module whether
// charID may be played. The first refusal wins.
func FireCharacterSelectGate(ctx context.Context, db *sql.DB, charID int64) error {
	for _, m := range registered {
		if g, ok := m.(CharacterSelectGate); ok {
			if err := g.CharacterSelectGate(ctx, db, charID); err != nil {
				return err
			}
		}
	}
	return nil
}

// FireEntityRemoved notifies every EntityRemovedHook module that a player
// entity was destroyed and removed from the given solar system.
func FireEntityRemoved(ss *destiny.SolarSystem, entityID uint32, killerID uint32) {
	for _, m := range registered {
		if h, ok := m.(EntityRemovedHook); ok {
			h.OnEntityRemoved(ss, entityID, killerID)
		}
	}
}

// FireSystemUnloading notifies every SystemUnloadingHook module that the
// given solar system is about to be torn down (no real players remain).
func FireSystemUnloading(solarSystemID int64) {
	for _, m := range registered {
		if h, ok := m.(SystemUnloadingHook); ok {
			h.OnSystemUnloading(solarSystemID)
		}
	}
}
