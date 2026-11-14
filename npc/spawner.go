package npc

import (
	"context"
	"database/sql"
	"gurimoon/db"
	"gurimoon/destiny"
	"log"
	"math"
	"math/rand"
	"sync"
	"time"
)

const (
	// AsteroidBeltGroupID is the celestial groupID for asteroid belts.
	AsteroidBeltGroupID int64 = 9
)

// Spawner is a logical entity anchored to a belt that manages NPC lifecycle.
// Its roster is the flat per-region spawn list: what can appear here and who
// owns it is decided row by row, no templates or pools in between.
type Spawner struct {
	ID          int64
	SystemID    int64
	RegionID    int64 // scope of the officer ledger this belt claims against
	CelestialID int64 // actual celestial entityID (belt)
	AnchorPos   destiny.Vec3

	// Roster is the region's spawn list. A row is eligible in this system
	// when the system security is at or below the row's MaxSec.
	Roster []db.RegionSpawnRow

	ScatterRadius float64
	// SeedCooldown is the minimum gap between waves at this belt, measured
	// seed-to-seed. Ships are never replaced individually: a belt keeps whatever
	// survives until the next wave.
	SeedCooldown time.Duration
	WarpIn       bool

	// CurrentTier is the squad tier the last wave rolled. Read by the log line
	// and the admin panel only.
	CurrentTier string

	mu       sync.Mutex
	liveNPCs map[uint32]bool // entityIDs currently alive
	// officerNPCs is the subset of liveNPCs holding a region officer slot on the
	// in-memory path only (no SpawnerManager or no DB). A persisted belt's slot is
	// held by its npcBeltRats row and settled by SpawnerManager.KillBeltRat.
	officerNPCs map[uint32]bool
	lastSeedAt  time.Time // zero until the first wave is rolled
	seeded      bool
	smgr        *SpawnerManager // back-ref for NPC registration
}

// beltSyncInterval is how often an EMPTY belt re-reads its persisted wave. A
// belt with anything alive on it costs no query at all: the ball census is the
// answer, and only a wipe can change it.
const beltSyncInterval = 15 * time.Second

// Tick reconciles the balls in space with the belt's persisted wave.
func (s *Spawner) Tick(ss *destiny.SolarSystem, mgr *NPCManager, sqldb *sql.DB) {
	s.mu.Lock()

	if len(s.Roster) == 0 {
		s.mu.Unlock()
		return
	}

	// Prune NPCs that are gone from the system.
	for eid := range s.liveNPCs {
		if ss.GetEntity(eid) == nil {
			delete(s.liveNPCs, eid)
		}
	}

	now := time.Now()
	alive := len(s.liveNPCs)

	// Sync on the first tick, then only while the belt is empty: a full belt is
	// already in agreement with its rows.
	due := !s.seeded || (alive == 0 && now.Sub(s.lastSeedAt) >= beltSyncInterval)
	if !due {
		s.mu.Unlock()
		return
	}
	s.seeded = true
	s.lastSeedAt = now
	s.mu.Unlock()

	if s.smgr != nil && s.smgr.sqldb != nil {
		s.syncPersistedWave(ss, mgr, sqldb)
		return
	}
	// In-memory path: no manager or no DB (unit tests, hand-built Spawners).
	s.spawnFullWave(ss, mgr, sqldb)
}

// syncPersistedWave materialises the belt's persisted rats as balls. The wave
// itself is decided by SpawnerManager.EnsureBeltWave, so a half-cleared belt
// comes up half-cleared and a cleared one stays empty for its cooldown.
func (s *Spawner) syncPersistedWave(ss *destiny.SolarSystem, mgr *NPCManager, sqldb *sql.DB) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	wave, rats, err := s.smgr.EnsureBeltWave(ctx, s.CelestialID, s.SystemID, ss.Security)
	if err != nil {
		log.Printf("[spawner] EnsureBeltWave belt=%d system=%d: %v", s.CelestialID, s.SystemID, err)
		return
	}
	s.mu.Lock()
	s.CurrentTier = wave.TierName
	s.mu.Unlock()

	spawned := 0
	for _, rat := range rats {
		if !rat.Alive || rat.EntityID != 0 {
			continue // already dead, or already standing in this system
		}
		row := db.RegionSpawnRow{
			TypeID:        rat.TypeID,
			Role:          rat.Role,
			ShipClass:     rat.ShipClass,
			FactionID:     rat.FactionID,
			CorporationID: rat.CorporationID,
		}
		entity := s.spawnNPCFromRow(ctx, ss, mgr, sqldb, &row,
			rat.ShieldFrac, rat.ArmorFrac, rat.StructureFrac)
		if entity == nil {
			// The row survives: a ball that could not be built is a spawn failure, not
			// a kill, and the officer claim belongs to the row.
			continue
		}
		if rec := mgr.Get(entity.ID); rec != nil {
			rec.mu.Lock()
			rec.CelestialID = s.CelestialID
			rec.SpawnerID = s.ID
			rec.mu.Unlock()
		}
		if err := db.BindBeltRatEntity(ctx, sqldb, rat.BeltRatID, entity.ID); err != nil {
			log.Printf("[spawner] BindBeltRatEntity rat=%d entity=%d: %v", rat.BeltRatID, entity.ID, err)
		}
		s.mu.Lock()
		if s.liveNPCs == nil {
			s.liveNPCs = make(map[uint32]bool)
		}
		s.liveNPCs[entity.ID] = true
		s.mu.Unlock()
		if s.smgr != nil {
			s.smgr.RegisterNPC(entity.ID, s)
		}
		spawned++
	}

	if spawned > 0 {
		log.Printf("[spawner] materialised %d/%d persisted NPCs at belt %d (system=%d, tier=%s, sec=%.2f)",
			spawned, len(rats), s.CelestialID, s.SystemID, wave.TierName, ss.Security)
	}
}

// SaveLiveHealth writes every materialised rat's current HP back to its row.
// Called when a system unloads, so damage a player dealt is not undone by
// everybody leaving.
func (s *Spawner) SaveLiveHealth(ss *destiny.SolarSystem, sqldb *sql.DB) {
	if sqldb == nil || ss == nil {
		return
	}
	s.mu.Lock()
	ids := make([]uint32, 0, len(s.liveNPCs))
	for eid := range s.liveNPCs {
		ids = append(ids, eid)
	}
	s.mu.Unlock()
	if len(ids) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, eid := range ids {
		e := ss.GetEntity(eid)
		if e == nil {
			continue
		}
		rat, _, err := db.GetBeltRatByEntity(ctx, sqldb, eid)
		if err != nil || rat == nil {
			continue
		}
		hp := e.HPSnapshot()
		if err := db.SaveBeltRatHealth(ctx, sqldb, rat.BeltRatID,
			frac(hp.CurShield, hp.MaxShield),
			frac(hp.CurArmor, hp.MaxArmor),
			frac(hp.CurStructure, hp.MaxStructure)); err != nil {
			log.Printf("[spawner] SaveBeltRatHealth rat=%d: %v", rat.BeltRatID, err)
		}
	}
}

// frac reduces a cur/max pair to [0,1], reading a zero max as undamaged rather
// than as destroyed.
func frac(cur, max float64) float64 {
	if max <= 0 {
		return 1
	}
	f := cur / max
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

// spawnFullWave builds a wave: the security-matched spawn class decides how
// many of each hull class, and every slot is a weighted pick among the
// region's eligible rows of that hull class.
func (s *Spawner) spawnFullWave(ss *destiny.SolarSystem, mgr *NPCManager, sqldb *sql.DB) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// A lookup failure is reported as a failure, not as a coverage hole.
	rule, err := db.GetSpawnerRuleForSecurity(ctx, sqldb, ss.Security)
	if err != nil {
		log.Printf("[spawner] GetSpawnerRuleForSecurity sec=%.4f (system=%d): %v — empty wave",
			ss.Security, s.SystemID, err)
		return
	}
	if rule == nil {
		// npcSpawnerRules bands are matched inclusively at both ends, so a band
		// ending short of the next leaves a hole. Print the full security.
		log.Printf("[spawner] no spawner rule covers sec %.4f (system=%d) — empty wave; "+
			"check npcSpawnerRules for a gap between bands", ss.Security, s.SystemID)
		return
	}

	in := WaveInput{
		Roster:   s.Roster,
		Rule:     rule,
		Security: ss.Security,
	}
	if s.smgr != nil {
		in.Tiers = s.smgr.tiers
		in.Cfg = s.smgr.waveCfg
		regionID, systemID := s.RegionID, s.SystemID
		in.ClaimOfficer = func(row db.RegionSpawnRow) bool {
			ok, _ := s.smgr.TryClaimOfficer(regionID, row.TypeID, systemID, row.FactionID)
			return ok
		}
	} else {
		in.Cfg = DefaultWaveConfig()
	}

	out := RollWave(in)
	s.mu.Lock()
	s.CurrentTier = out.Tier.TierName
	s.mu.Unlock()

	if len(out.Rows) == 0 {
		log.Printf("[spawner] belt rolled an empty wave (system=%d, tier=%s) — retry in %s",
			s.SystemID, out.Tier.TierName, s.SeedCooldown)
		return
	}

	spawned := 0
	for _, row := range out.Rows {
		entity := s.spawnNPCFromRow(ctx, ss, mgr, sqldb, &row, 1, 1, 1)
		if entity == nil {
			// The claim was taken before the ball existed. Hand it back with
			// killed=false so a failed spawn does not start the region's cool-down.
			if row.Role == "officer" && s.smgr != nil {
				s.smgr.ReleaseOfficer(s.RegionID, false)
			}
			continue
		}
		if rec := mgr.Get(entity.ID); rec != nil {
			rec.mu.Lock()
			rec.CelestialID = s.CelestialID
			rec.SpawnerID = s.ID
			rec.mu.Unlock()
		}
		s.mu.Lock()
		if s.liveNPCs == nil {
			s.liveNPCs = make(map[uint32]bool)
		}
		s.liveNPCs[entity.ID] = true
		if row.Role == "officer" {
			if s.officerNPCs == nil {
				s.officerNPCs = make(map[uint32]bool)
			}
			s.officerNPCs[entity.ID] = true
		}
		s.mu.Unlock()
		if s.smgr != nil {
			s.smgr.RegisterNPC(entity.ID, s)
		}
		spawned++
	}

	// This log line is the instrument for tuning the tier table.
	log.Printf("[spawner] spawned %d NPCs at belt (system=%d, tier=%s, sec=%.2f→%.2f, "+
		"cmdr=%v, officer=%v%s, anchor=%.0f,%.0f,%.0f)",
		spawned, s.SystemID, out.Tier.TierName, ss.Security, out.EffectiveSec,
		out.HasCommander, out.HasOfficer, officerNote(out),
		s.AnchorPos.X, s.AnchorPos.Y, s.AnchorPos.Z)
}

// officerNote annotates the spawn log when an officer roll came up and was
// turned away, so a quiet region is explained rather than merely quiet.
func officerNote(out WaveOutput) string {
	if out.OfficerDenied == "" {
		return ""
	}
	return " [officer rolled, denied: " + out.OfficerDenied + "]"
}

// spawnNPCFromRow creates one NPC from a region-spawn row. The row's faction
// and corporation own the ball; a missing corp falls back to the faction's
// primary NPC corp, a missing faction to Angel Cartel. The three fractions are
// the persisted rat's remaining health; pass 1,1,1 for a fresh roll.
func (s *Spawner) spawnNPCFromRow(ctx context.Context, ss *destiny.SolarSystem, mgr *NPCManager, sqldb *sql.DB, row *db.RegionSpawnRow, shieldFrac, armorFrac, structureFrac float64) *destiny.Entity {
	pos := scatterAround(s.AnchorPos, s.ScatterRadius)

	stats, err := db.GetNPCStats(ctx, sqldb, row.TypeID)
	if err != nil {
		log.Printf("[spawner] GetNPCStats typeID=%d: %v", row.TypeID, err)
		return nil
	}

	var hpInit *NPCHPInit
	hp, err := db.GetEntityHPByType(ctx, sqldb, row.TypeID)
	if err == nil && hp != nil {
		hpInit = &NPCHPInit{
			ShieldHP:    hp.ShieldHP,
			ArmorHP:     hp.ArmorHP,
			StructureHP: hp.StructureHP,
			ShieldTau:   hp.ShieldTau,
			ShieldRes:   hp.ShieldRes,
			ArmorRes:    hp.ArmorRes,
			HullRes:     hp.HullRes,
		}
	}

	wp, wpErr := db.GetNPCWeaponProfile(ctx, sqldb, row.TypeID)
	if wpErr != nil {
		log.Printf("[spawner] weapon profile error typeID=%d: %v", row.TypeID, wpErr)
	} else if wp == nil {
		log.Printf("[spawner] typeID=%d has no weapon profile (unarmed)", row.TypeID)
	}
	bounty := db.GetNPCBounty(ctx, sqldb, row.TypeID)

	// Ownership rides the row.
	factionID := row.FactionID
	if factionID == 0 {
		factionID = 500011 // Angel Cartel
	}
	corpID := row.CorporationID
	if corpID == 0 {
		corpID = db.GetFactionCorpID(ctx, sqldb, factionID)
	}

	// For warp-in spawners, position at warp origin and set warp-in state.
	var warpDest destiny.Vec3
	useWarp := s.WarpIn
	if useWarp {
		// Place NPC 100-150km from anchor in random direction.
		warpDist := 100000.0 + rand.Float64()*50000.0
		theta := rand.Float64() * 2 * math.Pi
		phi := math.Acos(2*rand.Float64() - 1)
		warpOrigin := destiny.Vec3{
			X: s.AnchorPos.X + warpDist*math.Sin(phi)*math.Cos(theta),
			Y: s.AnchorPos.Y + warpDist*math.Sin(phi)*math.Sin(theta),
			Z: s.AnchorPos.Z + warpDist*math.Cos(phi),
		}
		warpDest = pos // scattered position near anchor
		pos = warpOrigin
	}

	// notify=false: the slim's group/category and warp-in state must be final
	// before players receive the ball. Belt rats are aggressive and may flee.
	entity := mgr.SpawnNPC(ss, row.TypeID, stats.TypeName,
		pos, stats.Mass, stats.Agility, stats.MaxVelocity, stats.Radius,
		StanceAggressive, hpInit, wp, corpID, true, bounty, factionID, false)

	if entity != nil {
		SetGroupCategory(entity, stats.GroupID, stats.CategoryID)

		// Restore persisted damage before NotifySpawn, or a client draws the rat at
		// full and then sees it jump.
		applyHPFractions(entity, shieldFrac, armorFrac, structureFrac)

		// Record the spawn role so the loot path can gate role-restricted
		// drops (e.g. commander-only items) at kill time.
		role := row.Role
		if role == "" {
			role = "normal"
		}
		if rec := mgr.Get(entity.ID); rec != nil {
			rec.mu.Lock()
			rec.SpawnRole = role
			// Aggro floor covers the scatter plus the belt field so the whole belt is
			// one engagement zone. See docs/npc-behaviour.md#hostility-and-threat.
			aggrFloor := s.ScatterRadius + destiny.BeltFieldRadius(ss.Security)
			if aggrFloor < 30000 {
				aggrFloor = 30000
			}
			if rec.AggrRange < aggrFloor {
				rec.AggrRange = aggrFloor
			}
			rec.mu.Unlock()
		}

		if useWarp {
			mgr.SetWarpIn(entity.ID, warpDest)
		}

		mgr.NotifySpawn(entity, ss)
	}

	return entity
}

// applyHPFractions scales a freshly built entity's current HP to a persisted
// rat's remaining health. A fraction at or above 1 (or a layer with no max) is
// left alone, so the common fresh-spawn case costs nothing.
func applyHPFractions(e *destiny.Entity, shieldFrac, armorFrac, structureFrac float64) {
	if shieldFrac >= 1 && armorFrac >= 1 && structureFrac >= 1 {
		return
	}
	scale := func(cur, max, f float64) float64 {
		if max <= 0 || f >= 1 {
			return cur
		}
		if f < 0 {
			f = 0
		}
		return max * f
	}
	hp := e.HPSnapshot()
	e.SetHP(destiny.HPState{
		MaxShield: hp.MaxShield, CurShield: scale(hp.CurShield, hp.MaxShield, shieldFrac),
		MaxArmor: hp.MaxArmor, CurArmor: scale(hp.CurArmor, hp.MaxArmor, armorFrac),
		MaxStructure: hp.MaxStructure, CurStructure: scale(hp.CurStructure, hp.MaxStructure, structureFrac),
		ShieldTau: hp.ShieldTau,
	})
}

// NotifyDeath is called when a managed NPC dies; the tick loop reseeds the
// whole belt once it is empty and SeedCooldown has elapsed. It reports whether
// the dead ball was this belt's region officer and does not settle the ledger:
// the caller holds SpawnerManager.mu, and the ledger must not be taken under it.
func (s *Spawner) NotifyDeath(entityID uint32) (wasOfficer bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.liveNPCs, entityID)
	if s.officerNPCs[entityID] {
		delete(s.officerNPCs, entityID)
		return true
	}
	return false
}

// LiveCount returns number of alive NPCs.
func (s *Spawner) LiveCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.liveNPCs)
}

// OfficerCount returns how many region officer slots this belt is holding.
// UnloadSystem uses it to hand them back when the system is torn down.
func (s *Spawner) OfficerCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.officerNPCs)
}

// Tier returns the squad tier the belt's last wave rolled ("" before the first).
func (s *Spawner) Tier() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.CurrentTier
}

// scatterAround returns a random point within radius of centre.
func scatterAround(centre destiny.Vec3, radius float64) destiny.Vec3 {
	if radius <= 0 {
		return centre
	}
	theta := rand.Float64() * 2 * math.Pi
	phi := math.Acos(2*rand.Float64() - 1)
	r := radius * math.Cbrt(rand.Float64())
	return destiny.Vec3{
		X: centre.X + r*math.Sin(phi)*math.Cos(theta),
		Y: centre.Y + r*math.Sin(phi)*math.Sin(theta),
		Z: centre.Z + r*math.Cos(phi),
	}
}
