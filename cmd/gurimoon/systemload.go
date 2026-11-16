package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strconv"
	"sync"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/logx"
	"gurimoon/marshal"
	"gurimoon/modapi"
	"gurimoon/safego"
	"gurimoon/server"
)

// sysBuildMu guards sysBuilding, one mutex per system being built. The build is serialised per
// system so a second caller waits for the finished system instead of using a half-filled one.
var (
	sysBuildMu  sync.Mutex
	sysBuilding = map[int64]*sync.Mutex{}
)

func sysBuildLock(sysID int64) *sync.Mutex {
	sysBuildMu.Lock()
	defer sysBuildMu.Unlock()
	mu, ok := sysBuilding[sysID]
	if !ok {
		mu = &sync.Mutex{}
		sysBuilding[sysID] = mu
	}
	return mu
}

// ensureSystemLoaded brings sysID to the state a player entering it would find and returns it.
// Idempotent, so it is safe on every undock arrival. cels and solItem are the caller's when it has
// them and are fetched otherwise (the modapi.SpaceHooks preload path). Modules call this and never
// SystemManager.GetOrCreate. See docs/cmd-misc-notes.md#system-loader.
func ensureSystemLoaded(sysMgr *destiny.SystemManager, sysID int64, sqldb *sql.DB, addr string,
	cels []evedb.CelestialEntity, solItem marshal.PyRep) (*destiny.SolarSystem, error) {

	if sysMgr == nil || sysID == 0 {
		return nil, fmt.Errorf("ensureSystemLoaded: no system manager for sysID=%d", sysID)
	}

	// Fast path outside the build lock: a loaded system needs nothing here. A system with belts and no
	// rocks takes the slow path, whose asteroid backfill is the only thing that fills it.
	if ss := sysMgr.Get(sysID); ss != nil && len(ss.CelBalls) > 0 &&
		!(len(ss.AsteroidBalls) == 0 && systemHasBelts(ss)) {
		modapi.FireSystemLoaded(ss)
		return ss, nil
	}

	mu := sysBuildLock(sysID)
	mu.Lock()
	defer mu.Unlock()

	// The preload path supplies neither.
	if len(cels) == 0 || solItem == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		var err error
		if len(cels) == 0 {
			if cels, err = globalCelestialCache.GetCelestials(ctx, sqldb, sysID); err != nil {
				cancel()
				return nil, fmt.Errorf("ensureSystemLoaded: celestials sysID=%d: %w", sysID, err)
			}
		}
		if solItem == nil {
			er, erErr := evedb.GetSolarSystemEntityRow(ctx, sqldb, sysID)
			if erErr != nil {
				log.Printf("[%s] ensureSystemLoaded: GetSolarSystemEntityRow sysID=%d: %v", addr, sysID, erErr)
				solItem = marshal.PyNone{}
			} else if er != nil {
				solItem = buildEntityRow(er)
			} else {
				solItem = marshal.PyNone{}
			}
		}
		cancel()
	}

	ss, isNew := sysMgr.GetOrCreate(sysID)
	// Build when the system is new or has no celestials: created is reported only for the map insert,
	// so a registered but unbuilt shell is filled by the next caller.
	if isNew || len(ss.CelBalls) == 0 {
		// Fetch system security to determine if stations are conquerable.
		if sec, ok := globalCelestialCache.GetSecurity(sysID); ok {
			ss.Security = sec
		} else if sqldb != nil {
			secCtx, secCancel := context.WithTimeout(context.Background(), 5*time.Second)
			sec, secOK, secErr := evedb.GetSystemSecurity(secCtx, sqldb, sysID)
			if secErr == nil && secOK {
				ss.Security = sec
			}
			secCancel()
		}
		// Sovereign faction gates which lowsec ore and faction ice the belts carry. Set before the
		// asteroid build.
		if fid, ok := globalCelestialCache.GetSovFaction(sysID); ok {
			ss.FactionID = fid
		} else if sqldb != nil {
			secCtx, secCancel := context.WithTimeout(context.Background(), 5*time.Second)
			if fid, facErr := evedb.GetSystemSovFactionID(secCtx, sqldb, sysID); facErr == nil {
				ss.FactionID = fid
			}
			secCancel()
		}

		// Load celestials.
		ss.CelBalls = makeCelestialBallDescs(cels)
		ss.CelSlims = makeCelestialSlimItems(cels)
		celDmg := destiny.DamageState{Shield: 0.75, Tau: 100000, Armor: 0.95, Structure: 1.0}
		for _, cel := range cels {
			ss.SetCelDamage(uint32(cel.ItemID), celDmg)
		}
		if solItem != nil {
			ss.SolItem = solItem
		} else {
			ss.SolItem = marshal.PyNone{}
		}
		// Register conquerable station entities in nullsec systems.
		if ss.Security < 0.0 && sqldb != nil {
			registerConquerableStations(ss, cels, sqldb, addr)
		}
		// Seed anchored bubbles at gates, stations and belts before asteroid, billboard and NPC
		// generation so those entities join the right bubbles.
		ss.SeedCelestialBubbles(ss.CelBalls, ss.CelSlims)
		// Rebuild the belts from this system's persisted seed.
		loadAsteroidsIntoSystem(addr, sqldb, ss, sysID)
		// Load any persisted starbases for this system.
		loadStarbasesIntoSystem(addr, sqldb, ss, sysID)
		// Anchored outpost construction platforms.
		loadOutpostPlatformsIntoSystem(addr, sqldb, ss, sysID)
		// Cynosural fields still burning here.
		loadCynoBeaconsIntoSystem(addr, sqldb, ss, sysID)
		// Beacons of spawner-placed open dungeon sites.
		loadOpenSitesIntoSystem(addr, ss, sysID)
		// Persisted loot containers.
		loadLootContainersIntoSystem(addr, sqldb, ss, sysID)
		// Ships their pilots ejected from (eject.go).
		loadAbandonedShipsIntoSystem(addr, sqldb, ss, sysID)
		// Scanner probes still alive here (memory only).
		loadScanProbesIntoSystem(ss, sysID)
		// Mines laid here (mines.go).
		loadMinesIntoSystem(addr, sqldb, ss, sysID)
		// Emplace station sentry guns (low-sec and high-sec only). After
		// SeedCelestialBubbles so the guns join the station's grid.
		spawnStationSentries(ss, cels, sqldb, globalNPCMgr, addr)
		// Spawn billboards near stargates (high-sec and low-sec only).
		ss.GenerateBillboards(ss.CelBalls, ss.CelSlims)
		// Load spawners and do initial spawn at belt locations.
		if globalSpawnerMgr != nil && globalNPCMgr != nil {
			spawnCtx, spawnCancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := globalSpawnerMgr.LoadForSystem(spawnCtx, sqldb, ss); err != nil {
				log.Printf("[%s] registerUndock: spawner load failed sysID=%d: %v", addr, sysID, err)
			} else {
				globalSpawnerMgr.TickSystem(ss, globalNPCMgr, sqldb)
			}
			spawnCancel()
		}
		// Spawn customs officers at stargates in highsec empire systems.
		if globalNPCMgr != nil {
			initCustomsPatrol(ss, sqldb, globalNPCMgr)
		}
		// Respawn persistent mission dungeon NPCs in this system.
		if globalNPCMgr != nil {
			spawnMissionDungeonsForSystem(ss, sqldb, globalNPCMgr, sysID, addr)
		}
		log.Printf("[%s] registerUndock: first player in sysID=%d — loaded %d celestials, %d asteroids, %d starbases, %d billboards",
			addr, sysID, len(ss.CelBalls), len(ss.AsteroidBalls), len(ss.StarbaseBalls), len(ss.BillboardBalls))
	} else if len(ss.AsteroidBalls) == 0 && systemHasBelts(ss) {
		// A loaded system with belts and no rocks gets its field built here. The belt test keeps a
		// beltless system out of this branch, since hydration is a database transaction.
		loadAsteroidsIntoSystem(addr, sqldb, ss, sysID)
		log.Printf("[%s] registerUndock: generated %d asteroids for existing sysID=%d",
			addr, len(ss.AsteroidBalls), sysID)
	}
	// Modules see every system a player enters; the hook is idempotent and fires for new and loaded
	// systems alike.
	modapi.FireSystemLoaded(ss)
	// Push respawned rocks to clients still sitting on the belt grid; everyone
	// else picks them up from SetState on their next warp-in.
	ss.SetOnAsteroidRespawn(func(ball destiny.BallDesc, slim destiny.SlimItem, dmg destiny.DamageState) {
		for _, p := range ss.GetAllPlayersInSystem() {
			if p.Conn == nil || !p.HasBallparkState() {
				continue
			}
			if p.Pos().Sub(ball.Pos).LenSq() > destiny.BubbleRadius*destiny.BubbleRadius {
				continue
			}
			if len(p.ClaimUnknownBalls(ball.EntityID)) == 0 {
				continue
			}
			// Queued: a belt respawns rocks in bursts, and a rock appearing a
			// tick late is invisible to a player where a spent stamp is not.
			p.QueueAddActionFunc("add:"+strconv.FormatUint(uint64(ball.EntityID), 10), ball.EntityID,
				func(stamp uint32) marshal.PyRep {
					return destiny.BuildAddBallsAction(stamp,
						[]destiny.BallDesc{ball}, []destiny.SlimItem{slim},
						map[uint32]destiny.DamageState{ball.EntityID: dmg})
				})
		}
	})
	ss.StartAsteroidRespawnTicker()
	// Bubble crossings update who knows which ball. The tick loop starts after the callback is set.
	ss.SetBubbleTransitionBroadcast(func(sys *destiny.SolarSystem, entity *destiny.Entity, oldBubbleID int) {
		pos := entity.Pos()
		left, joined, tickMerged := sys.TransferBubble(entity.ID, pos)

		// Queued so removals coalesce per (kind, ball): a ball flapping across a boundary collapses to one
		// final state instead of one stamped packet per crossing.
		rmKey := "rm:" + strconv.FormatUint(uint64(entity.ID), 10)
		for _, mate := range left {
			if mate.Conn == nil || mate.Conn.IsClosed() || !mate.KnowsBall(entity.ID) {
				continue
			}
			// Drop a still-queued AddBalls instead of sending an add/remove pair the client rejects.
			// DropPendingAdd, not DropPending, so a queued movement cannot cancel a real removal.
			if mate.DropPendingAdd(entity.ID) {
				mate.ForgetBalls(entity.ID)
				continue
			}
			// QueueRemoveBall keeps the ball known until the packet leaves. Forgetting it first lets the
			// repair and prune paths act on a belief that the client dropped a ball it still draws.
			mate.QueueRemoveBall(rmKey, entity.ID)
		}

		if tickMerged {
			// Bubble merge: cross-notify all players in the merged bubble.
			for _, p := range joined {
				if !p.IsPlayer || p.Conn == nil || p.Conn.IsClosed() {
					continue
				}
				ensureBallsKnown(p, joined...)
			}
		} else {
			// Notify players in the new bubble that this entity arrived.
			for _, mate := range joined {
				if mate.Conn == nil || mate.Conn.IsClosed() {
					continue
				}
				ensureBallsKnown(mate, entity)
			}

			// A player flying sublight into a new bubble also needs that bubble's dynamic contents, or it
			// sees nothing of the new grid until it warps.
			if entity.IsPlayer && entity.Conn != nil && !entity.Conn.IsClosed() {
				ensureBallsKnown(entity, sys.GetBubbleEntitiesExcept(entity.ID)...)
			}
		}

		// The mover also drops the grid it left. The prune runs in the mover's own reconciler, not on this
		// tick goroutine, because the keep-set build scans the system's asteroids and loot.
		if entity.IsPlayer && entity.Conn != nil && !entity.Conn.IsClosed() {
			wakeBallparkReconciler(entity.ID)
		}

		// Debug only: one line per ball per bubble crossing.
		logx.Debugf("[tick] bubble transition: entity %d moved from bubble %d, left=%d joined=%d merged=%v",
			entity.ID, oldBubbleID, len(left), len(joined), tickMerged)
	})
	ss.StartTickLoop()
	// Start shield regen ticker and wire up the broadcast callback.
	ss.SetShieldRegenBroadcast(func(sys *destiny.SolarSystem, entityID uint32, dmgState destiny.DamageState) {
		dmgState.Timestamp = server.Win32TimeNow()

		// Update CelDamage for station entities so new arrivals see correct HP.
		if sys.IsStationEntity(entityID) {
			sys.SetCelDamage(entityID, dmgState)
		}
		// Structures: the same for StarbaseDamage, plus persistence and the
		// tower's reinforcement floor, which re-arms as the shield recovers.
		if e := sys.StructureEntity(entityID); e != nil {
			structureHPChanged(sys, e, dmgState)
		}

		// Broadcast to all players who can see this entity.
		target := sys.GetEntity(entityID)
		if target == nil {
			return
		}
		var players []*destiny.Entity
		if target.BubbleID < 0 {
			players = sys.GetAllPlayersInSystem()
		} else {
			if target.Conn != nil {
				players = append(players, target)
			}
			players = append(players, sys.GetBubbleConnectedPlayersExcept(entityID)...)
		}
		for _, p := range players {
			if p.Conn == nil || !p.KnowsBall(entityID) {
				continue
			}
			if p.IsInWarp() || p.IsUntargetable() {
				continue
			}
			p.SendFXStamped(func(stamp uint32) error {
				return destiny.SendDamageStateChange(p.Conn, globalNodeID, p.AccountID, stamp, entityID, dmgState)
			})
		}

		// Update HUD S/A/S bars if target is a player.
		if target.IsPlayer && target.Conn != nil &&
			!target.IsInWarp() && !target.IsUntargetable() {
			sendPlayerHPUpdate(target)
		}
	})
	ss.StartShieldRegenTicker()

	return ss, nil
}

// gateJumpFlashDelay is how long a stargate jump holds the session change after the JumpOut
// flash, so the client sees the gate fire before its ballpark is torn down. server_settings
// gate.jumpFlashMs, default 2500.
func gateJumpFlashDelay() time.Duration {
	return evedb.CachedSettingDuration("gate.jumpFlashMs", time.Millisecond, 2500, 200, 10000)
}

// prewarmJumpDestination builds sysID in the background while a jumping ship's gate flash
// plays. The jumper's own registerUndock then takes ensureSystemLoaded's fast path, or waits
// on the build lock for this build instead of starting its own.
//
// A system this call created is released again if no player has arrived a minute later
// (a jump abandoned mid-flight); releaseSystem refuses while any player is in it.
func prewarmJumpDestination(sysMgr *destiny.SystemManager, sysID int64, sqldb *sql.DB, addr string) {
	if sysMgr == nil || sysID == 0 || sysMgr.Get(sysID) != nil {
		return
	}
	go func() {
		defer safego.Recover("prewarmJumpDestination")
		started := time.Now()
		ss, err := ensureSystemLoaded(sysMgr, sysID, sqldb, addr, nil, nil)
		if err != nil {
			log.Printf("[%s] jump prewarm sysID=%d: %v", addr, sysID, err)
			return
		}
		log.Printf("[%s] jump prewarm sysID=%d ready in %s", addr, sysID,
			time.Since(started).Round(time.Millisecond))
		time.AfterFunc(time.Minute, func() {
			defer safego.Recover("prewarmJumpDestination.release")
			releaseSystem(sysMgr, ss, sqldb)
		})
	}()
}
