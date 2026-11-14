package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math"
	"sort"
	"sync"
	"time"

	"gurimoon/dogma"
)

// AttrIsOnline is dogma attribute 2 (isOnline). One entity_attributes row per fitted
// module records its online state (valueInt 1 = online); no row means offline.
const AttrIsOnline = 2

// EnsureFittedModuleOnlineState backfills isOnline rows, marking online, for modules
// fitted before online state was persisted. Called once at startup.
func EnsureFittedModuleOnlineState(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO entity_attributes (itemID, attributeID, valueInt, valueFloat)
		SELECT e.itemID, ?, 1, NULL
		FROM entity e
		JOIN invTypes  t ON t.typeID  = e.typeID
		JOIN invGroups g ON g.groupID = t.groupID
		WHERE e.flag BETWEEN 11 AND 34 AND g.categoryID = 7
		  AND NOT EXISTS (SELECT 1 FROM entity_attributes ea
		                  WHERE ea.itemID = e.itemID AND ea.attributeID = ?)`,
		AttrIsOnline, AttrIsOnline)
	if err != nil {
		return fmt.Errorf("EnsureFittedModuleOnlineState: %w", err)
	}
	return nil
}

// bootFittingCandidateQuery selects ships whose online modules may exceed CPU or powergrid.
// It mirrors GetShipCPUPowerLoad and GetShipCPUPGCapacityFitted in SQL; the Go path re-checks
// every row, so a divergence costs only a wasted check. Comparisons are strict. Need skills,
// hull role bonuses and implants are left out: each only lowers load or raises capacity, so
// the candidates stay a superset.
const bootFittingCandidateQuery = `
SELECT L.shipID, s.typeID
FROM (
  SELECT e.locationID AS shipID,
    COALESCE(SUM(CASE WHEN dta.attributeID = 50
                      THEN COALESCE(dta.valueFloat, dta.valueInt)
                           * COALESCE(NULLIF(COALESCE(sw.valueFloat, sw.valueInt), 0), 1)
                      ELSE 0 END), 0) AS cpuLoad,
    COALESCE(SUM(CASE WHEN dta.attributeID = 30
                      THEN COALESCE(dta.valueFloat, dta.valueInt)
                      ELSE 0 END), 0) AS pgLoad
  FROM entity e
  JOIN entity_attributes ea ON ea.itemID = e.itemID
   AND ea.attributeID = ? AND ea.valueInt = 1
  JOIN dgmTypeAttributes dta ON dta.typeID = e.typeID AND dta.attributeID IN (30, 50)
  LEFT JOIN dgmTypeAttributes sw ON sw.typeID = e.typeID AND sw.attributeID = 1081
  JOIN invTypes  t2 ON t2.typeID  = e.typeID
  JOIN invGroups g2 ON g2.groupID = t2.groupID
  WHERE e.flag BETWEEN 11 AND 34 AND g2.categoryID <> 8
  GROUP BY e.locationID
) L
JOIN entity s ON s.itemID = L.shipID
LEFT JOIN dgmTypeAttributes c ON c.typeID = s.typeID AND c.attributeID = 48
LEFT JOIN dgmTypeAttributes p ON p.typeID = s.typeID AND p.attributeID = 11
LEFT JOIN (
  SELECT e.locationID AS shipID,
    EXP(SUM(CASE WHEN a.attributeID = 202
                 THEN LN(COALESCE(NULLIF(a.valueFloat, 0), a.valueInt)) ELSE 0 END)) AS cpuMult,
    EXP(SUM(CASE WHEN a.attributeID = 145
                 THEN LN(COALESCE(NULLIF(a.valueFloat, 0), a.valueInt)) ELSE 0 END)) AS pgMult,
    SUM(CASE WHEN a.attributeID = 549
             THEN COALESCE(NULLIF(a.valueFloat, 0), a.valueInt) ELSE 0 END) AS pgAdd
  FROM entity e
  JOIN entity_attributes ea ON ea.itemID = e.itemID
   AND ea.attributeID = ? AND ea.valueInt = 1
  JOIN invTypes t ON t.typeID = e.typeID
  JOIN dgmTypeAttributes a ON a.typeID = e.typeID AND a.attributeID IN (202, 145, 549)
  WHERE e.flag BETWEEN 11 AND 34 AND t.groupID NOT IN (?, ?)
  GROUP BY e.locationID
) B ON B.shipID = L.shipID
WHERE L.cpuLoad >
        COALESCE(NULLIF(c.valueFloat, 0), c.valueInt, 0)
        * COALESCE(B.cpuMult, 1)
        * (1 + ? * COALESCE((SELECT COALESCE(sa.valueInt, 1)
                             FROM entity se
                             LEFT JOIN entity_attributes sa
                               ON sa.itemID = se.itemID AND sa.attributeID = 280
                             WHERE se.ownerID = s.ownerID AND se.typeID = ?
                               AND se.flag IN (7, 61) LIMIT 1), 0))
   OR L.pgLoad >
        COALESCE(NULLIF(p.valueFloat, 0), p.valueInt, 0)
        * COALESCE(B.pgMult, 1)
        * (1 + ? * COALESCE((SELECT COALESCE(ga.valueInt, 1)
                             FROM entity ge
                             LEFT JOIN entity_attributes ga
                               ON ga.itemID = ge.itemID AND ga.attributeID = 280
                             WHERE ge.ownerID = s.ownerID AND ge.typeID = ?
                               AND ge.flag IN (7, 61) LIMIT 1), 0))
        + COALESCE(B.pgAdd, 0)`

// EnforceBootFittingLoads offlines modules, highest slot flag first, on any ship whose
// persisted online set exceeds its CPU or powergrid. Candidates come from
// bootFittingCandidateQuery and are re-checked here. Returns how many modules went offline.
func EnforceBootFittingLoads(ctx context.Context, db *sql.DB) (int, error) {
	start := time.Now()
	rows, err := db.QueryContext(ctx, bootFittingCandidateQuery,
		AttrIsOnline, AttrIsOnline,
		groupSensorBooster, groupTrackingComputer,
		fittingSkillBonusPerLevel, skillElectronics,
		fittingSkillBonusPerLevel, skillEngineering)
	if err != nil {
		return 0, fmt.Errorf("EnforceBootFittingLoads ships: %w", err)
	}
	type shipRow struct{ shipID, typeID int64 }
	var ships []shipRow
	for rows.Next() {
		var s shipRow
		if err := rows.Scan(&s.shipID, &s.typeID); err != nil {
			rows.Close()
			return 0, fmt.Errorf("EnforceBootFittingLoads scan: %w", err)
		}
		ships = append(ships, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("EnforceBootFittingLoads rows: %w", err)
	}
	log.Printf("[boot] fitting: %d over-budget candidate(s) in %s",
		len(ships), time.Since(start).Round(time.Millisecond))

	offlined := 0
	for _, s := range ships {
		ids, _ := shedOverBudgetModules(ctx, db, s.shipID, s.typeID)
		offlined += len(ids)
	}
	return offlined, nil
}

// shedOverBudgetModules offlines modules on shipID, highest slot flag first, until its
// online set fits its CPU and powergrid, re-reading capacity after each (a co-processor or
// RCU lowers the output). Returns the modules it offlined, nil when the ship already fits.
func shedOverBudgetModules(ctx context.Context, db *sql.DB, shipID, shipTypeID int64) ([]int64, error) {
	cpuCap, pgCap, err := GetShipCPUPGCapacityFitted(ctx, db, shipTypeID, shipID)
	if err != nil {
		return nil, err
	}
	cpuLoad, pgLoad, err := GetShipCPUPowerLoad(ctx, db, shipID)
	if err != nil || (cpuLoad <= cpuCap && pgLoad <= pgCap) {
		return nil, err
	}
	modRows, err := db.QueryContext(ctx, `
		SELECT e.itemID FROM entity e
		JOIN entity_attributes ea ON ea.itemID = e.itemID
		 AND ea.attributeID = ? AND ea.valueInt = 1
		WHERE e.locationID = ? AND e.flag BETWEEN 11 AND 34
		ORDER BY e.flag DESC`, AttrIsOnline, shipID)
	if err != nil {
		return nil, err
	}
	var modIDs []int64
	for modRows.Next() {
		var id int64
		if modRows.Scan(&id) == nil {
			modIDs = append(modIDs, id)
		}
	}
	modRows.Close()
	var offlined []int64
	for _, modID := range modIDs {
		if cpuLoad <= cpuCap && pgLoad <= pgCap {
			break
		}
		if SetModuleOnlineState(ctx, db, modID, false) != nil {
			continue
		}
		offlined = append(offlined, modID)
		cpuCap, pgCap, _ = GetShipCPUPGCapacityFitted(ctx, db, shipTypeID, shipID)
		cpuLoad, pgLoad, _ = GetShipCPUPowerLoad(ctx, db, shipID)
	}
	return offlined, nil
}

// EnforceOwnerFittingLoads runs shedOverBudgetModules on every ship ownerID owns that has an
// online module. Call it after the pilot's implants change: losing a fitting hardwiring
// can leave any of their ships over budget. Returns the offlined modules by ship.
func EnforceOwnerFittingLoads(ctx context.Context, db *sql.DB, ownerID int64) (map[int64][]int64, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT DISTINCT s.itemID, s.typeID
		FROM entity s
		JOIN entity e ON e.locationID = s.itemID AND e.flag BETWEEN 11 AND 34
		JOIN entity_attributes ea ON ea.itemID = e.itemID
		 AND ea.attributeID = ? AND ea.valueInt = 1
		JOIN invTypes  t ON t.typeID  = s.typeID
		JOIN invGroups g ON g.groupID = t.groupID
		WHERE s.ownerID = ? AND g.categoryID = 6`, AttrIsOnline, ownerID)
	if err != nil {
		return nil, fmt.Errorf("EnforceOwnerFittingLoads: %w", err)
	}
	type shipRow struct{ shipID, typeID int64 }
	var ships []shipRow
	for rows.Next() {
		var r shipRow
		if err := rows.Scan(&r.shipID, &r.typeID); err != nil {
			rows.Close()
			return nil, fmt.Errorf("EnforceOwnerFittingLoads scan: %w", err)
		}
		ships = append(ships, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("EnforceOwnerFittingLoads rows: %w", err)
	}
	out := map[int64][]int64{}
	for _, r := range ships {
		ids, err := shedOverBudgetModules(ctx, db, r.shipID, r.typeID)
		if err != nil {
			return out, fmt.Errorf("EnforceOwnerFittingLoads ship %d: %w", r.shipID, err)
		}
		if len(ids) > 0 {
			out[r.shipID] = ids
		}
	}
	return out, nil
}

// SetModuleOnlineState persists a module's online state (attr 2 isOnline).
func SetModuleOnlineState(ctx context.Context, db *sql.DB, itemID int64, online bool) error {
	v := int64(0)
	if online {
		v = 1
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO entity_attributes (itemID, attributeID, valueInt, valueFloat)
		VALUES (?, ?, ?, NULL)
		ON DUPLICATE KEY UPDATE valueInt = VALUES(valueInt), valueFloat = NULL`,
		itemID, AttrIsOnline, v)
	if err != nil {
		return fmt.Errorf("SetModuleOnlineState: %w", err)
	}
	return nil
}

// ClearModuleOnlineState removes the isOnline attribute row, used when a module is
// unfitted (hangar items carry no online state).
func ClearModuleOnlineState(ctx context.Context, db *sql.DB, itemID int64) error {
	_, err := db.ExecContext(ctx,
		`DELETE FROM entity_attributes WHERE itemID = ? AND attributeID = ?`,
		itemID, AttrIsOnline)
	if err != nil {
		return fmt.Errorf("ClearModuleOnlineState: %w", err)
	}
	return nil
}

// GetOnlineModuleIDs returns the set of fitted-module itemIDs on shipID whose
// persisted isOnline state is 1.
func GetOnlineModuleIDs(ctx context.Context, db *sql.DB, shipID int64) (map[int64]bool, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT e.itemID FROM entity e
		JOIN entity_attributes ea ON ea.itemID = e.itemID
		 AND ea.attributeID = ? AND ea.valueInt = 1
		WHERE e.locationID = ? AND e.flag BETWEEN 11 AND 34`,
		AttrIsOnline, shipID)
	if err != nil {
		return nil, fmt.Errorf("GetOnlineModuleIDs: %w", err)
	}
	defer rows.Close()
	out := make(map[int64]bool)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("GetOnlineModuleIDs scan: %w", err)
		}
		out[id] = true
	}
	return out, rows.Err()
}

// GetShipWarpCoreStrength returns the warp scramble points this ship resists: the
// sign-flipped sum of negative warpScrambleStrength (attr 105) on its online modules
// (warp core stabilizers, -1 each). Queried on demand by the warp, dock and jump gates.
func GetShipWarpCoreStrength(ctx context.Context, db *sql.DB, shipID int64) (float64, error) {
	var total sql.NullFloat64
	err := db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(-COALESCE(dta.valueFloat, dta.valueInt)), 0)
		FROM entity e
		JOIN entity_attributes ea ON ea.itemID = e.itemID
		 AND ea.attributeID = ? AND ea.valueInt = 1
		JOIN dgmTypeAttributes dta ON dta.typeID = e.typeID AND dta.attributeID = ?
		WHERE e.locationID = ? AND e.flag BETWEEN 11 AND 34
		  AND COALESCE(dta.valueFloat, dta.valueInt) < 0`,
		AttrIsOnline, dogma.AttrWarpScrambleStrength, shipID).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("GetShipWarpCoreStrength shipID=%d: %w", shipID, err)
	}
	if !total.Valid || total.Float64 < 0 {
		return 0, nil
	}
	return total.Float64, nil
}

// GetShipFittingHPBonuses sums the flat HP bonuses granted by online tank
// modules: shield extenders (effect 21 shieldCapacityBonusOnline → attr 72
// capacityBonus) and armor plates (effect 594 armorExtender → attr 335
// armorHpBonus). Only modules with persisted isOnline=1 contribute.
func GetShipFittingHPBonuses(ctx context.Context, db *sql.DB, shipID int64) (shieldAdd, armorAdd float64, err error) {
	rows, err := db.QueryContext(ctx, `
		SELECT te.effectID,
		       SUM(COALESCE(NULLIF(a.valueFloat, 0), CAST(a.valueInt AS DOUBLE), a.valueFloat))
		FROM entity e
		JOIN entity_attributes ea ON ea.itemID = e.itemID
		 AND ea.attributeID = 2 AND ea.valueInt = 1
		JOIN dgmTypeEffects te ON te.typeID = e.typeID AND te.effectID IN (21, 594)
		JOIN dgmTypeAttributes a ON a.typeID = e.typeID
		 AND a.attributeID = CASE te.effectID WHEN 21 THEN 72 ELSE 335 END
		WHERE e.locationID = ? AND e.flag BETWEEN 11 AND 34
		GROUP BY te.effectID`, shipID)
	if err != nil {
		return 0, 0, fmt.Errorf("GetShipFittingHPBonuses: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var effectID int64
		var total sql.NullFloat64
		if err := rows.Scan(&effectID, &total); err != nil {
			return 0, 0, fmt.Errorf("GetShipFittingHPBonuses scan: %w", err)
		}
		if !total.Valid {
			continue
		}
		switch effectID {
		case 21:
			shieldAdd = total.Float64
		case 594:
			armorAdd = total.Float64
		}
	}
	return shieldAdd, armorAdd, rows.Err()
}

// GetMiningYieldBonus returns the multiplicative mining yield bonus from
// online mining upgrade modules (attr 434 miningAmountBonus, a percentage
// per module, e.g. Mining Laser Upgrade I = 5). Returns 1.0 when none online.
func GetMiningYieldBonus(ctx context.Context, db *sql.DB, shipID int64) (float64, error) {
	mult := 1.0
	if shipID == 0 {
		return mult, nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT COALESCE(NULLIF(a.valueFloat, 0), CAST(a.valueInt AS DOUBLE), a.valueFloat)
		FROM entity e
		JOIN entity_attributes ea ON ea.itemID = e.itemID
		 AND ea.attributeID = 2 AND ea.valueInt = 1
		JOIN dgmTypeAttributes a ON a.typeID = e.typeID AND a.attributeID = 434
		WHERE e.locationID = ? AND e.flag BETWEEN 11 AND 34`, shipID)
	if err != nil {
		return mult, fmt.Errorf("GetMiningYieldBonus: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var bonus sql.NullFloat64
		if err := rows.Scan(&bonus); err != nil {
			return mult, fmt.Errorf("GetMiningYieldBonus scan: %w", err)
		}
		if bonus.Valid && bonus.Float64 != 0 {
			mult *= 1.0 + bonus.Float64/100.0
		}
	}
	return mult, rows.Err()
}

// GetWeaponCombatBonuses computes the damage and cycle-time multipliers for one weapon
// from the character's skills, the ship's online damage mods matching the weapon group,
// and implants. Returns (1, 1) when nothing applies; cycleMult is clamped to [0.25, 4.0].
// See WeaponCombatBonusesFrom.
func GetWeaponCombatBonuses(ctx context.Context, db *sql.DB, charID, shipID, weaponTypeID, weaponGroupID, chargeTypeID int64, isTurret bool) (dmgMult, cycleMult float64, err error) {
	reqs := func(typeID int64) []SkillReq {
		if typeID == 0 {
			return nil
		}
		r, rerr := GetRequiredSkills(ctx, db, typeID)
		if rerr != nil {
			return nil
		}
		return r
	}
	levels := func(skillID int64) int {
		lvl, lerr := GetCharacterSkillLevel(ctx, db, charID, skillID)
		if lerr != nil {
			return 0
		}
		return lvl
	}
	attrs := func(typeID int64) map[int64]float64 {
		a, aerr := TypeAttrs(ctx, db, typeID)
		if aerr != nil {
			return nil
		}
		return a
	}

	var dmgModTypes []int64
	if effs, ok := dogma.WeaponDamageModEffects[weaponGroupID]; ok && shipID != 0 {
		dmgModTypes, err = fittedDamageModTypes(ctx, db, shipID, effs[0])
		if err != nil {
			return 1.0, 1.0, err
		}
	}

	dmgMult, cycleMult = WeaponCombatBonusesFrom(levels, attrs, reqs,
		weaponTypeID, chargeTypeID, isTurret, dmgModTypes)

	// Implants fold after the clamp, like the hull terms: Gunslinger damage lines and the
	// Rapid Firing / Rapid Launch lines.
	if charID != 0 {
		im := GetCharImplantMults(ctx, db, charID)
		if isTurret {
			wpnReq := map[int64]bool{}
			for _, r := range reqs(weaponTypeID) {
				wpnReq[r.SkillID] = true
			}
			dmgMult *= im.TurretDamage(wpnReq)
			cycleMult *= im.TurretCycle
		} else {
			cycleMult *= im.LauncherCycle
		}
	}
	return dmgMult, cycleMult, nil
}

// MissileRangeMults returns the pilot's multipliers on a missile's flight velocity and
// flight time (Missile Projection, Missile Bombardment, implants, hull bonus). Failures
// read 1.0. The skills are named by ID. See docs/fitting-notes.md#missile-range.
func MissileRangeMults(ctx context.Context, db *sql.DB, charID, shipTypeID, launcherGroupID int64) (velocity, flightTime float64) {
	velocity, flightTime = 1.0, 1.0
	if db == nil {
		return
	}
	if charID != 0 {
		skill := func(skillID, attrID int64) float64 {
			lvl, err := GetCharacterSkillLevel(ctx, db, charID, skillID)
			if err != nil || lvl <= 0 {
				return 0
			}
			a, err := TypeAttrs(ctx, db, skillID)
			if err != nil {
				return 0
			}
			return a[attrID] / 100.0 * float64(lvl)
		}
		velocity *= 1.0 + skill(dogma.SkillMissileProjection, dogma.AttrSpeedFactor)
		flightTime *= 1.0 + skill(dogma.SkillMissileBombardment, dogma.AttrMaxFlightTimeBonus)
		im := GetCharImplantMults(ctx, db, charID)
		velocity *= im.MissileVelocity
		flightTime *= im.MissileFlightTime
	}
	if shipTypeID != 0 {
		if hb, err := GetShipHullBonuses(ctx, db, charID, shipTypeID); err == nil && len(hb) > 0 {
			hv, hf := hb.MissileRangeMults(launcherGroupID)
			velocity *= hv
			flightTime *= hf
		}
	}
	return velocity, flightTime
}

// TurretSkillMults holds a pilot's optimal, falloff, tracking and capacitor-use multipliers
// from the turret range skills. Kept out of HullWeaponMults, which the rolebots module
// builds by keyed literal (a new field would read 0 there). Identity is all ones.
type TurretSkillMults struct {
	Optimal, Falloff, Tracking, CapNeed float64
}

// IdentityTurretSkillMults is the unskilled pilot: nothing moves.
func IdentityTurretSkillMults() TurretSkillMults {
	return TurretSkillMults{Optimal: 1, Falloff: 1, Tracking: 1, CapNeed: 1}
}

// TurretSkillMultsFrom is the pure fold behind GetTurretSkillMults: Sharpshooter (optimal),
// Trajectory Analysis (falloff), Motion Prediction (tracking), Controlled Bursts (cap use),
// each a per-level percentage read off the skill. Motion Prediction is read by ID only: its
// attr 244 is a multiplier on a tracking disruptor. levels returns 0 when untrained.
func TurretSkillMultsFrom(levels func(skillID int64) int, attrs func(typeID int64) map[int64]float64) TurretSkillMults {
	out := IdentityTurretSkillMults()
	apply := func(skillID, attrID int64, into *float64) {
		lvl := levels(skillID)
		if lvl <= 0 {
			return
		}
		a := attrs(skillID)
		if a == nil {
			return
		}
		if b := a[attrID]; b != 0 {
			*into *= 1.0 + b/100.0*float64(lvl)
		}
	}
	apply(dogma.SkillSharpshooter, dogma.AttrRangeSkillBonus, &out.Optimal)
	apply(dogma.SkillTrajectoryAnalysis, dogma.AttrFalloffSkillBonus, &out.Falloff)
	apply(dogma.SkillMotionPrediction, dogma.AttrTrackingSpeedMultiplier, &out.Tracking)
	apply(dogma.SkillControlledBursts, dogma.AttrCapNeedSkillBonus, &out.CapNeed)
	return out
}

// GetTurretSkillMults reads the pilot's four turret range skills and implants. Any failure
// degrades to identity.
func GetTurretSkillMults(ctx context.Context, db *sql.DB, charID int64) TurretSkillMults {
	if db == nil || charID == 0 {
		return IdentityTurretSkillMults()
	}
	levels := func(skillID int64) int {
		lvl, err := GetCharacterSkillLevel(ctx, db, charID, skillID)
		if err != nil {
			return 0
		}
		return lvl
	}
	attrs := func(typeID int64) map[int64]float64 {
		a, err := TypeAttrs(ctx, db, typeID)
		if err != nil {
			return nil
		}
		return a
	}
	out := TurretSkillMultsFrom(levels, attrs)
	im := GetCharImplantMults(ctx, db, charID)
	out.Optimal *= im.TurretOptimal
	out.Falloff *= im.TurretFalloff
	out.Tracking *= im.TurretTracking
	out.CapNeed *= im.TurretCapNeed
	return out
}

// fittedDamageModTypes lists the typeIDs of the online damage mods on shipID carrying
// dmgEffectID (gyrostabilizers, heat sinks, field stabilizers, ballistic control systems).
func fittedDamageModTypes(ctx context.Context, db *sql.DB, shipID, dmgEffectID int64) ([]int64, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT e.typeID FROM entity e
		JOIN entity_attributes ea ON ea.itemID = e.itemID
		 AND ea.attributeID = 2 AND ea.valueInt = 1
		JOIN dgmTypeEffects te ON te.typeID = e.typeID AND te.effectID = ?
		WHERE e.locationID = ? AND e.flag BETWEEN 11 AND 34`,
		dmgEffectID, shipID)
	if err != nil {
		return nil, fmt.Errorf("GetWeaponCombatBonuses mods: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var t int64
		if err := rows.Scan(&t); err == nil {
			out = append(out, t)
		}
	}
	return out, rows.Err()
}

// WeaponCombatBonusesFrom is the pure fold behind GetWeaponCombatBonuses. Every read is
// hoisted into levels, attrs and reqs (nil to skip), so batched callers and the rolebots
// module share one copy of the arithmetic. dmgModTypes are the online damage mods already
// matched to the weapon group. The hull cycle bonus is applied after the [0.25, 4.0] clamp.
func WeaponCombatBonusesFrom(
	levels func(skillID int64) int,
	attrs func(typeID int64) map[int64]float64,
	reqs func(typeID int64) []SkillReq,
	weaponTypeID, chargeTypeID int64,
	isTurret bool,
	dmgModTypes []int64,
) (dmgMult, cycleMult float64) {
	dmgMult, cycleMult = 1.0, 1.0

	// ── skill bonuses ─────────────────────────────────────────────────────
	seen := map[int64]bool{}
	var candidates []int64
	addReqs := func(typeID int64) {
		for _, r := range reqs(typeID) {
			if !seen[r.SkillID] {
				seen[r.SkillID] = true
				candidates = append(candidates, r.SkillID)
			}
		}
	}
	addReqs(weaponTypeID)
	addReqs(chargeTypeID)
	// Launcher support skills are in no requirement chain, so they are listed: Warhead
	// Upgrades (+2% damage/level) and Rapid Launch (-3% cycle/level).
	globals := []int64{dogma.SkillMissileLauncherOp, dogma.SkillWarheadUpgrades, dogma.SkillRapidLaunch}
	if isTurret {
		globals = []int64{dogma.SkillGunnery, dogma.SkillRapidFiring, dogma.SkillSurgicalStrike}
	}
	for _, g := range globals {
		if !seen[g] {
			seen[g] = true
			candidates = append(candidates, g)
		}
	}

	for _, skillID := range candidates {
		lvl := levels(skillID)
		if lvl <= 0 {
			continue
		}
		a := attrs(skillID)
		if a == nil {
			continue
		}
		if b := a[dogma.AttrDamageMultiplierBonus]; b != 0 {
			dmgMult *= 1.0 + b/100.0*float64(lvl)
		}
		if b := a[dogma.AttrRofBonus]; b != 0 {
			cycleMult *= 1.0 + b/100.0*float64(lvl)
		}
		if b := a[dogma.AttrTurretSpeedBonus]; b != 0 {
			cycleMult *= 1.0 + b/100.0*float64(lvl)
		}
	}

	// ── fitted damage-mod bonuses ─────────────────────────────────────────
	var dmgMods, spdMods []float64
	for _, t := range dmgModTypes {
		a := attrs(t)
		if a == nil {
			continue
		}
		if m := a[dogma.AttrDamageMultiplier]; m > 0 {
			dmgMods = append(dmgMods, m)
		}
		// BCS: damage bonus is a raw multiplier in attr 213 (1.07/1.10),
		// not damageMultiplier(64) like turret damage mods.
		if m := a[dogma.AttrMissileDamageMultiplierBonus]; m > 0 {
			dmgMods = append(dmgMods, m)
		}
		if m := a[dogma.AttrSpeedMultiplier]; m > 0 {
			spdMods = append(spdMods, m)
		}
	}
	// Stacking penalty, strongest (furthest from 1.0) first.
	sort.Sort(sort.Reverse(sort.Float64Slice(dmgMods)))
	for i, m := range dmgMods {
		dmgMult *= 1.0 + (m-1.0)*StackingPenalty(i)
	}
	sort.Float64s(spdMods)
	for i, m := range spdMods {
		cycleMult *= 1.0 + (m-1.0)*StackingPenalty(i)
	}

	if cycleMult < 0.25 {
		cycleMult = 0.25
	} else if cycleMult > 4.0 {
		cycleMult = 4.0
	}
	return dmgMult, cycleMult
}

// StackingPenalty returns the effectiveness factor for the i-th strongest
// module affecting the same attribute (i is 0-based): exp(-(i/2.67)²).
func StackingPenalty(i int) float64 {
	x := float64(i) / 2.67
	return math.Exp(-x * x)
}

// Resist-modifier effect IDs. Passive resist modules use attrs 984-987; hardeners carry the
// passive variant (994-997, -1%) plus an Active effect that swaps in 984-987 while running;
// damage controls carry direct resonance multipliers (see dcResonanceLayers).
const (
	effectModifyArmorResonance  = 2041
	effectModifyShieldResonance = 2052
	effectArmorHardenerPassive  = 2084
	effectArmorHardenerActive   = 2098
	effectShieldHardenerPassive = 2117
	effectShieldHardenerActive  = 2118
	effectDamageControl         = 2302
)

// Resistance-bonus attribute IDs in em/explosive/kinetic/thermal order,
// matching the destiny.Entity Resists damage-type order.
var (
	resistBonusAttrs        = [4]int64{984, 985, 986, 987}
	passiveResistBonusAttrs = [4]int64{994, 995, 996, 997}
	hullResonanceAttrs      = [4]int64{974, 975, 976, 977}
)

// dcResonanceLayers lists a damage control's resonance attributes by layer (0 shield, 1
// armour, 2 hull) and em/expl/kin/therm. They are multipliers (0.9 = 10% resist), not the
// percentage bonuses of 984-987. See docs/fitting-notes.md#damage-control.
var dcResonanceLayers = [3][4]int64{
	{271, 272, 273, 274}, // shield
	{267, 268, 269, 270}, // armour
	hullResonanceAttrs,   // hull
}

// foldDamageControlResonance keeps the strongest (lowest) damage-control
// multiplier seen so far per layer and damage type. A value outside (0,1) is
// not a resist and is ignored. Pure so the arithmetic is testable without a
// database.
func foldDamageControlResonance(best *[3][4]float64, attrs map[int64]float64) {
	for layer, ids := range dcResonanceLayers {
		for d, attrID := range ids {
			if r := attrs[attrID]; r > 0 && r < 1 && r < best[layer][d] {
				best[layer][d] = r
			}
		}
	}
}

// GetShipResistanceMultipliers returns combined resonance multipliers per layer (0 shield,
// 1 armor, 2 hull) and damage type from online resist modules. Hardeners count at full
// strength only while in activeItemIDs, else -1%. Damage controls: docs/fitting-notes.md#damage-control.
func GetShipResistanceMultipliers(ctx context.Context, db *sql.DB, shipID int64, activeItemIDs map[int64]bool) ([3][4]float64, error) {
	mult := [3][4]float64{
		{1, 1, 1, 1}, {1, 1, 1, 1}, {1, 1, 1, 1},
	}
	if shipID == 0 {
		return mult, nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT e.itemID, e.typeID, te.effectID FROM entity e
		JOIN entity_attributes ea ON ea.itemID = e.itemID
		 AND ea.attributeID = 2 AND ea.valueInt = 1
		JOIN dgmTypeEffects te ON te.typeID = e.typeID
		 AND te.effectID IN (?, ?, ?, ?, ?)
		WHERE e.locationID = ? AND e.flag BETWEEN 11 AND 34`,
		effectModifyArmorResonance, effectModifyShieldResonance,
		effectArmorHardenerActive, effectShieldHardenerActive,
		effectDamageControl, shipID)
	if err != nil {
		return mult, fmt.Errorf("GetShipResistanceMultipliers: %w", err)
	}
	type modRow struct {
		itemID, typeID, effectID int64
	}
	var mods []modRow
	for rows.Next() {
		var m modRow
		if err := rows.Scan(&m.itemID, &m.typeID, &m.effectID); err == nil {
			mods = append(mods, m)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return mult, fmt.Errorf("GetShipResistanceMultipliers rows: %w", err)
	}

	// Percentage bonuses per layer × damage type (negative = resistance).
	var bonuses [3][4][]float64
	dcBest := [3][4]float64{{1, 1, 1, 1}, {1, 1, 1, 1}, {1, 1, 1, 1}}
	for _, m := range mods {
		attrs, aerr := TypeAttrs(ctx, db, m.typeID)
		if aerr != nil {
			continue
		}
		switch m.effectID {
		case effectModifyShieldResonance, effectModifyArmorResonance:
			layer := 0
			if m.effectID == effectModifyArmorResonance {
				layer = 1
			}
			for d, attrID := range resistBonusAttrs {
				if b := attrs[attrID]; b != 0 {
					bonuses[layer][d] = append(bonuses[layer][d], b)
				}
			}
		case effectShieldHardenerActive, effectArmorHardenerActive:
			layer := 0
			if m.effectID == effectArmorHardenerActive {
				layer = 1
			}
			src := passiveResistBonusAttrs
			if activeItemIDs[m.itemID] {
				src = resistBonusAttrs
			}
			for d, attrID := range src {
				if b := attrs[attrID]; b != 0 {
					bonuses[layer][d] = append(bonuses[layer][d], b)
				}
			}
		case effectDamageControl:
			foldDamageControlResonance(&dcBest, attrs)
		}
	}
	// A damage control sits outside the stacking penalty and a second adds nothing, so the
	// fold keeps the strongest per layer and damage type.
	for layer := 0; layer < 3; layer++ {
		for d := 0; d < 4; d++ {
			mult[layer][d] *= dcBest[layer][d]
		}
	}

	for layer := 0; layer < 3; layer++ {
		for d := 0; d < 4; d++ {
			bs := bonuses[layer][d]
			// Strongest (most negative) first for the stacking penalty.
			sort.Float64s(bs)
			for i, b := range bs {
				mult[layer][d] *= 1.0 + b*StackingPenalty(i)/100.0
			}
		}
	}
	return mult, nil
}

// GetTankSkillMultipliers returns the HP and shield-recharge multipliers from tank skills,
// then implants: Shield Management (attr 337), Hull Upgrades (335), Mechanic (327), Shield
// Operation (338), each 5% per level. rechargeMult scales recharge time, so below 1.0 is
// faster. All 1.0 when charID is 0.
func GetTankSkillMultipliers(ctx context.Context, db *sql.DB, charID int64) (shieldMult, armorMult, hullMult, rechargeMult float64, err error) {
	shieldMult, armorMult, hullMult, rechargeMult = 1.0, 1.0, 1.0, 1.0
	if charID == 0 {
		return
	}
	apply := func(skillID, bonusAttrID int64, out *float64) {
		lvl, lerr := GetCharacterSkillLevel(ctx, db, charID, skillID)
		if lerr != nil || lvl <= 0 {
			return
		}
		attrs, aerr := TypeAttrs(ctx, db, skillID)
		if aerr != nil {
			return
		}
		if b := attrs[bonusAttrID]; b != 0 {
			*out *= 1.0 + b/100.0*float64(lvl)
		}
	}
	apply(dogma.SkillShieldManagement, 337, &shieldMult)
	apply(dogma.SkillHullUpgrades, 335, &armorMult)
	apply(dogma.SkillMechanic, 327, &hullMult)
	apply(dogma.SkillShieldOperation, 338, &rechargeMult)
	// Implants: the Slave set and the HP / recharge hardwirings.
	im := GetCharImplantMults(ctx, db, charID)
	shieldMult *= im.ShieldHP
	armorMult *= im.ArmorHP
	hullMult *= im.HullHP
	rechargeMult *= im.ShieldRecharge
	return
}

// Shield Boost Amplifier effect ID: a passive module scaling shield-booster amount via
// shieldBoostMultiplier (548).
const effectShieldBoostAmplifier = 1720

// ShipPassiveBonuses aggregates the ship-wide modifiers from online passive modules.
// Multiplier fields default to 1.0 and Add fields to 0; RechargeMult fields scale recharge
// time, so below 1.0 is faster regen.
type ShipPassiveBonuses struct {
	CPUMult, PGMult, PGAdd              float64
	CapCapMult, CapAdd, CapRechargeMult float64
	ShieldCapMult, ShieldRechargeMult   float64
	StructureMult, CargoMult            float64
	VelocityMult, AgilityMult           float64
	ScanResMult                         float64
	// TargetRangeMult is the signal amplifiers' lock-range multiplier (attr 237 on group 210
	// only: on a sensor damper it is an EWAR debuff). 1 with none.
	TargetRangeMult float64
}

// Passive-bonus attribute IDs read off online fitted modules. Multipliers sit near 1.0
// (1.05 = +5%), percent attrs are signed percentages, flat attrs are raw units (MW, GJ).
const (
	attrCapacitorBonus     = 67  // flat GJ (cap batteries)
	attrSpeedBonus         = 80  // ±% max velocity (overdrives, nanofibers)
	attrShieldRechargeMult = 134 // × shield recharge time (flux coil, PDS)
	attrCapRechargeMult    = 144 // × cap recharge time (rechargers, relays, PDS)
	attrPowerOutputMult    = 145 // × powergrid (RCU, PDS)
	attrShieldCapacityMult = 146 // × shield capacity (PDS, flux coil)
	attrCapCapacityMult    = 147 // × cap capacity (PDS, relays, batteries)
	attrCargoCapacityMult  = 149 // × cargo hold (expanded cargohold, bulkheads)
	attrStructureHPMult    = 150 // × structure HP (bulkheads, cargoholds)
	attrAgilityMult        = 169 // ±% inertia (istabs, nanofibers, plates)
	attrCPUMult            = 202 // × CPU output (co-processors)
	attrPowerIncrease      = 549 // flat MW (aux power core)
	attrScanResolutionMult = 565 // × scan resolution (signal amps; sensor boosters excluded)
	attrMaxTargetRangeMult = 237 // × max targeting range (signal amps, running sensor boosters)
	groupSignalAmplifier   = 210 // passive lock-range / scan-resolution modules
	attrMaxRangeMult       = 243 // × turret optimal (tracking enhancers/computers)
	attrTrackingSpeedMult  = 244 // × turret tracking (tracking enhancers/computers)
	groupSensorBooster     = 212 // active: applies only while running
	groupTrackingComputer  = 213 // active: applies only while running
	groupTrackingEnhancer  = 211 // passive: applies while online
	groupCapBattery        = 61  // only group whose capacitorBonus adds capacity
)

// stackMultipliers folds a list of multiplier-style values (≈1.0 ± x) into
// one factor with the standard stacking penalty, strongest deviation first.
func stackMultipliers(vals []float64) float64 {
	sort.Slice(vals, func(i, j int) bool {
		return math.Abs(vals[i]-1.0) > math.Abs(vals[j]-1.0)
	})
	out := 1.0
	for i, v := range vals {
		out *= 1.0 + (v-1.0)*StackingPenalty(i)
	}
	return out
}

// productMultipliers folds multiplier-style values into one factor with no stacking penalty.
func productMultipliers(vals []float64) float64 {
	out := 1.0
	for _, v := range vals {
		out *= v
	}
	return out
}

// productPercents folds signed-percentage values into one factor with no stacking penalty.
func productPercents(vals []float64) float64 {
	out := 1.0
	for _, v := range vals {
		out *= 1.0 + v/100.0
	}
	return out
}

// GetShipPassiveBonuses computes the combined ShipPassiveBonuses from online fitted
// modules. A multiplier is stack-penalized only when the attribute it lands on carries
// dgmAttributeTypes.stackable = 0: scan resolution (564) and targeting range (76). Capacitor,
// shield, structure, velocity and agility are stackable = 1 and fold as a plain product, like
// the fitting outputs. See docs/fitting-notes.md#stacking-penalty. Sensor boosters and tracking
// computers are excluded: they apply only while running (GetTurretTrackingMults,
// GetActiveSensorBoosterMult).
func GetShipPassiveBonuses(ctx context.Context, db *sql.DB, shipID int64) (*ShipPassiveBonuses, error) {
	b := &ShipPassiveBonuses{
		CPUMult: 1, PGMult: 1, CapCapMult: 1, CapRechargeMult: 1,
		ShieldCapMult: 1, ShieldRechargeMult: 1, StructureMult: 1,
		CargoMult: 1, VelocityMult: 1, AgilityMult: 1, ScanResMult: 1,
		TargetRangeMult: 1,
	}
	if shipID == 0 {
		return b, nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT a.attributeID, t.groupID, COALESCE(NULLIF(a.valueFloat, 0), CAST(a.valueInt AS DOUBLE), a.valueFloat)
		FROM entity e
		JOIN entity_attributes ea ON ea.itemID = e.itemID
		 AND ea.attributeID = 2 AND ea.valueInt = 1
		JOIN invTypes t ON t.typeID = e.typeID
		JOIN dgmTypeAttributes a ON a.typeID = e.typeID
		 AND a.attributeID IN (67, 80, 134, 144, 145, 146, 147, 149, 150, 169, 202, 549, 565, 237)
		WHERE e.locationID = ? AND e.flag BETWEEN 11 AND 34
		  AND t.groupID NOT IN (?, ?)
		  AND (a.attributeID <> 237 OR t.groupID = ?)`,
		shipID, groupSensorBooster, groupTrackingComputer, groupSignalAmplifier)
	if err != nil {
		return b, fmt.Errorf("GetShipPassiveBonuses: %w", err)
	}
	defer rows.Close()
	byAttr := map[int64][]float64{}
	for rows.Next() {
		var attrID, groupID int64
		var val float64
		if err := rows.Scan(&attrID, &groupID, &val); err != nil {
			return b, fmt.Errorf("GetShipPassiveBonuses scan: %w", err)
		}
		// capacitorBonus is also a laser cap-use stat on some energy turrets; only cap batteries
		// add it to capacity.
		if attrID == attrCapacitorBonus && groupID != groupCapBattery {
			continue
		}
		byAttr[attrID] = append(byAttr[attrID], val)
	}
	if err := rows.Err(); err != nil {
		return b, fmt.Errorf("GetShipPassiveBonuses rows: %w", err)
	}

	// Fitting outputs and flat adds: no stacking penalty.
	for _, v := range byAttr[attrCPUMult] {
		b.CPUMult *= v
	}
	for _, v := range byAttr[attrPowerOutputMult] {
		b.PGMult *= v
	}
	for _, v := range byAttr[attrPowerIncrease] {
		b.PGAdd += v
	}
	for _, v := range byAttr[attrCapacitorBonus] {
		b.CapAdd += v
	}
	for _, v := range byAttr[attrCargoCapacityMult] {
		b.CargoMult *= v
	}
	// Targets with stackable = 1: capacitorCapacity (482), rechargeRate (55), shieldCapacity
	// (263), shieldRechargeRate (479), hp (9), maxVelocity (37), agility (70).
	b.CapCapMult = productMultipliers(byAttr[attrCapCapacityMult])
	b.CapRechargeMult = productMultipliers(byAttr[attrCapRechargeMult])
	b.ShieldCapMult = productMultipliers(byAttr[attrShieldCapacityMult])
	b.ShieldRechargeMult = productMultipliers(byAttr[attrShieldRechargeMult])
	b.StructureMult = productMultipliers(byAttr[attrStructureHPMult])
	b.VelocityMult = productPercents(byAttr[attrSpeedBonus])
	b.AgilityMult = productPercents(byAttr[attrAgilityMult])
	// Targets with stackable = 0, penalized: scanResolution (564), maxTargetRange (76).
	b.ScanResMult = stackMultipliers(byAttr[attrScanResolutionMult])
	b.TargetRangeMult = stackMultipliers(byAttr[attrMaxTargetRangeMult])
	return b, nil
}

// Fitting-capacity skills: Engineering +5% powergrid per level, Electronics +5% CPU per
// level. The client applies both to its fitting bars, so the server must match or it
// refuses modules the client shows as fitting.
const (
	skillEngineering          = 3413
	skillElectronics          = 3426
	fittingSkillBonusPerLevel = 0.05
)

// GetShipCPUPGCapacityFitted returns the ship's effective CPU and powergrid
// capacity: hull base scaled by the owner's Electronics/Engineering skills and
// output hardwirings, online co-processors / reactor controls / power
// diagnostics, plus aux-power-core flat MW.
func GetShipCPUPGCapacityFitted(ctx context.Context, db *sql.DB, shipTypeID, shipID int64) (cpuOutput, pgOutput float64, err error) {
	cpuOutput, pgOutput, err = GetShipCPUPGCapacity(ctx, db, shipTypeID)
	if err != nil {
		return
	}
	// Owner skill bonuses.
	var ownerID int64
	if shipID != 0 {
		_ = db.QueryRowContext(ctx, `SELECT ownerID FROM entity WHERE itemID = ?`, shipID).Scan(&ownerID)
	}
	if ownerID != 0 {
		if lvl, serr := GetCharacterSkillLevel(ctx, db, ownerID, skillElectronics); serr == nil {
			cpuOutput *= 1 + fittingSkillBonusPerLevel*float64(lvl)
		}
		if lvl, serr := GetCharacterSkillLevel(ctx, db, ownerID, skillEngineering); serr == nil {
			pgOutput *= 1 + fittingSkillBonusPerLevel*float64(lvl)
		}
		ic, ip := GetCharImplantMults(ctx, db, ownerID).FittingOutput()
		cpuOutput *= ic
		pgOutput *= ip
	}
	pb, perr := GetShipPassiveBonuses(ctx, db, shipID)
	if perr != nil {
		return cpuOutput, pgOutput, nil // base capacity still usable
	}
	return cpuOutput * pb.CPUMult, pgOutput*pb.PGMult + pb.PGAdd, nil
}

// GetShipCargoCapacityFitted returns the ship's effective cargo capacity:
// hull base scaled by online expanded cargoholds / reinforced bulkheads,
// plus the hull's own cargo bonus (industrials +5% per racial industrial
// level) resolved against the ship owner's skills.
func GetShipCargoCapacityFitted(ctx context.Context, db *sql.DB, shipTypeID, shipID int64) (float64, error) {
	base, err := GetShipCargoCapacity(ctx, db, shipTypeID)
	if err != nil {
		return base, err
	}
	// Hull cargo bonus scales with the owner's command skill.
	var ownerID int64
	if shipID != 0 {
		_ = db.QueryRowContext(ctx, `SELECT ownerID FROM entity WHERE itemID = ?`, shipID).Scan(&ownerID)
	}
	if hb, herr := GetShipHullBonuses(ctx, db, ownerID, shipTypeID); herr == nil && len(hb) > 0 {
		base *= hb.StatMults().Cargo
	}
	if ownerID != 0 {
		base *= GetGeneralFreightMult(ctx, db, ownerID)
	}
	pb, perr := GetShipPassiveBonuses(ctx, db, shipID)
	if perr != nil {
		return base, nil
	}
	return base * pb.CargoMult, nil
}

// General Freight (12834) gives every hull +5% cargo per level through attr 614
// cargoCapacityBonus on the skill. See docs/fitting-notes.md#general-freight.
const (
	skillGeneralFreight    = 12834
	attrCargoCapacityBonus = 614
)

var (
	generalFreightMu     sync.Mutex
	generalFreightLoaded bool
	generalFreightPct    float64
)

// generalFreightMult is the cargo multiplier General Freight gives at a
// trained level: 1 + bonusPct·level/100.
func generalFreightMult(bonusPct float64, level int) float64 {
	if level <= 0 || bonusPct == 0 {
		return 1.0
	}
	return 1 + bonusPct*float64(level)/100
}

// GetGeneralFreightMult resolves the owner's General Freight level against
// the skill's attr 614 (read once per process). Any failure reads 1.0.
func GetGeneralFreightMult(ctx context.Context, db *sql.DB, ownerID int64) float64 {
	generalFreightMu.Lock()
	if !generalFreightLoaded {
		var pct sql.NullFloat64
		err := db.QueryRowContext(ctx, `
			SELECT COALESCE(NULLIF(valueFloat, 0), CAST(valueInt AS DOUBLE), valueFloat)
			FROM dgmTypeAttributes WHERE typeID = ? AND attributeID = ?`,
			skillGeneralFreight, attrCargoCapacityBonus).Scan(&pct)
		if err == nil || errors.Is(err, sql.ErrNoRows) {
			generalFreightPct = pct.Float64
			generalFreightLoaded = true
		}
	}
	pct := generalFreightPct
	generalFreightMu.Unlock()
	if pct == 0 {
		return 1.0
	}
	level, err := GetCharacterSkillLevel(ctx, db, ownerID, skillGeneralFreight)
	if err != nil {
		return 1.0
	}
	return generalFreightMult(pct, level)
}

// GetTurretTrackingMults computes the optimal-range and tracking-speed multipliers for
// turrets from tracking enhancers (passive, count while online) and tracking computers
// (count only while in activeItemIDs). Stacking-penalized, strongest first.
func GetTurretTrackingMults(ctx context.Context, db *sql.DB, shipID int64, activeItemIDs map[int64]bool) (rangeMult, trackingMult float64, err error) {
	rangeMult, trackingMult = 1.0, 1.0
	if shipID == 0 {
		return
	}
	rows, qerr := db.QueryContext(ctx, `
		SELECT e.itemID, t.groupID, a.attributeID,
		       COALESCE(NULLIF(a.valueFloat, 0), CAST(a.valueInt AS DOUBLE), a.valueFloat)
		FROM entity e
		JOIN entity_attributes ea ON ea.itemID = e.itemID
		 AND ea.attributeID = 2 AND ea.valueInt = 1
		JOIN invTypes t ON t.typeID = e.typeID AND t.groupID IN (?, ?)
		JOIN dgmTypeAttributes a ON a.typeID = e.typeID AND a.attributeID IN (?, ?)
		WHERE e.locationID = ? AND e.flag BETWEEN 11 AND 34`,
		groupTrackingEnhancer, groupTrackingComputer,
		attrMaxRangeMult, attrTrackingSpeedMult, shipID)
	if qerr != nil {
		return rangeMult, trackingMult, fmt.Errorf("GetTurretTrackingMults: %w", qerr)
	}
	defer rows.Close()
	var ranges, trackings []float64
	for rows.Next() {
		var itemID, groupID, attrID int64
		var val float64
		if err := rows.Scan(&itemID, &groupID, &attrID, &val); err != nil {
			continue
		}
		if groupID == groupTrackingComputer && !activeItemIDs[itemID] {
			continue
		}
		switch attrID {
		case attrMaxRangeMult:
			ranges = append(ranges, val)
		case attrTrackingSpeedMult:
			trackings = append(trackings, val)
		}
	}
	return stackMultipliers(ranges), stackMultipliers(trackings), rows.Err()
}

// GetActiveSensorBoosterRangeMult is GetActiveSensorBoosterMult's twin for lock range:
// the combined maxTargetRangeMultiplier (237) of running sensor boosters.
// See docs/fitting-notes.md#lock-range.
func GetActiveSensorBoosterRangeMult(ctx context.Context, db *sql.DB, shipID int64, activeItemIDs map[int64]bool) (float64, error) {
	if shipID == 0 || len(activeItemIDs) == 0 {
		return 1.0, nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT e.itemID, COALESCE(NULLIF(a.valueFloat, 0), CAST(a.valueInt AS DOUBLE), a.valueFloat)
		FROM entity e
		JOIN invTypes t ON t.typeID = e.typeID AND t.groupID = ?
		JOIN dgmTypeAttributes a ON a.typeID = e.typeID AND a.attributeID = ?
		WHERE e.locationID = ? AND e.flag BETWEEN 11 AND 34`,
		groupSensorBooster, attrMaxTargetRangeMult, shipID)
	if err != nil {
		return 1.0, fmt.Errorf("GetActiveSensorBoosterRangeMult: %w", err)
	}
	defer rows.Close()
	var vals []float64
	for rows.Next() {
		var itemID int64
		var val float64
		if err := rows.Scan(&itemID, &val); err != nil {
			continue
		}
		if activeItemIDs[itemID] && val > 0 {
			vals = append(vals, val)
		}
	}
	return stackMultipliers(vals), rows.Err()
}

// GetActiveSensorBoosterMult returns the combined scanResolutionMultiplier
// from sensor boosters whose active effect is currently running. Passive
// signal-amp bonuses live in ShipPassiveBonuses.ScanResMult instead.
func GetActiveSensorBoosterMult(ctx context.Context, db *sql.DB, shipID int64, activeItemIDs map[int64]bool) (float64, error) {
	if shipID == 0 || len(activeItemIDs) == 0 {
		return 1.0, nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT e.itemID, COALESCE(NULLIF(a.valueFloat, 0), CAST(a.valueInt AS DOUBLE), a.valueFloat)
		FROM entity e
		JOIN invTypes t ON t.typeID = e.typeID AND t.groupID = ?
		JOIN dgmTypeAttributes a ON a.typeID = e.typeID AND a.attributeID = ?
		WHERE e.locationID = ? AND e.flag BETWEEN 11 AND 34`,
		groupSensorBooster, attrScanResolutionMult, shipID)
	if err != nil {
		return 1.0, fmt.Errorf("GetActiveSensorBoosterMult: %w", err)
	}
	defer rows.Close()
	var vals []float64
	for rows.Next() {
		var itemID int64
		var val float64
		if err := rows.Scan(&itemID, &val); err != nil {
			continue
		}
		if activeItemIDs[itemID] {
			vals = append(vals, val)
		}
	}
	return stackMultipliers(vals), rows.Err()
}

// GetCapacitorSkillMultipliers computes the capacitor capacity and recharge
// multipliers from Energy Management (3418, +5%/lvl capacity, attr 1079) and
// Energy Systems Operation (3417, -5%/lvl recharge time, attr 314).
func GetCapacitorSkillMultipliers(ctx context.Context, db *sql.DB, charID int64) (capMult, rechargeMult float64, err error) {
	capMult, rechargeMult = 1.0, 1.0
	if charID == 0 {
		return
	}
	apply := func(skillID, bonusAttrID int64, out *float64) {
		lvl, lerr := GetCharacterSkillLevel(ctx, db, charID, skillID)
		if lerr != nil || lvl <= 0 {
			return
		}
		attrs, aerr := TypeAttrs(ctx, db, skillID)
		if aerr != nil {
			return
		}
		if b := attrs[bonusAttrID]; b != 0 {
			*out *= 1.0 + b/100.0*float64(lvl)
		}
	}
	apply(dogma.SkillEnergyManagement, 1079, &capMult)
	apply(dogma.SkillEnergySystemsOp, 314, &rechargeMult)
	im := GetCharImplantMults(ctx, db, charID)
	capMult *= im.CapCapacity
	rechargeMult *= im.CapRecharge
	return
}

// RemoteRepairBonusesFrom is the pure fold behind GetRemoteRepairBonuses: the cycle and cap
// multipliers a remote repair module gets from its own required skills (Repair Systems,
// attr 312 duration; the emission skills, attr 317 cap need). Never raises the amount.
// cycleMult is clamped to [0.25, 4.0]. See docs/fitting-notes.md#remote-repair.
func RemoteRepairBonusesFrom(
	levels func(skillID int64) int,
	attrs func(typeID int64) map[int64]float64,
	reqSkills []int64,
) (cycleMult, capMult float64) {
	cycleMult, capMult = 1.0, 1.0
	seen := map[int64]bool{}
	for _, skillID := range reqSkills {
		if seen[skillID] {
			continue
		}
		seen[skillID] = true
		lvl := levels(skillID)
		if lvl <= 0 {
			continue
		}
		a := attrs(skillID)
		if a == nil {
			continue
		}
		if b := a[dogma.AttrDurationSkillBonus]; b != 0 {
			cycleMult *= 1.0 + b/100.0*float64(lvl)
		}
		if b := a[dogma.AttrCapNeedSkillBonus]; b != 0 {
			capMult *= 1.0 + b/100.0*float64(lvl)
		}
	}
	if cycleMult < 0.25 {
		cycleMult = 0.25
	} else if cycleMult > 4.0 {
		cycleMult = 4.0
	}
	return cycleMult, capMult
}

// GetRemoteRepairBonuses reads the pilot's levels in the module's required skills and
// folds them. Any failure degrades to (1, 1).
func GetRemoteRepairBonuses(ctx context.Context, db *sql.DB, charID, moduleTypeID int64) (cycleMult, capMult float64) {
	if db == nil || charID == 0 || moduleTypeID == 0 {
		return 1.0, 1.0
	}
	reqs, err := GetRequiredSkills(ctx, db, moduleTypeID)
	if err != nil {
		return 1.0, 1.0
	}
	ids := make([]int64, 0, len(reqs))
	for _, r := range reqs {
		ids = append(ids, r.SkillID)
	}
	levels := func(skillID int64) int {
		lvl, lerr := GetCharacterSkillLevel(ctx, db, charID, skillID)
		if lerr != nil {
			return 0
		}
		return lvl
	}
	attrs := func(typeID int64) map[int64]float64 {
		a, aerr := TypeAttrs(ctx, db, typeID)
		if aerr != nil {
			return nil
		}
		return a
	}
	cycleMult, capMult = RemoteRepairBonusesFrom(levels, attrs, ids)
	// The emission hardwirings cut the cap of the one module kind requiring
	// their skill.
	capMult *= GetCharImplantMults(ctx, db, charID).RemoteRepCap(ids)
	return cycleMult, capMult
}

// GetRepairModuleBonuses returns amount, cycle and cap multipliers for a self-repair module.
// amount: online Shield Boost Amplifiers (548, stacking-penalized) and implants. cycle: required
// skills' durationSkillBonus (312), clamped to [0.25, 4.0]. cap: Shield Compensation (851),
// added by ID because it is not a prerequisite. (1, 1, 1) when nothing applies.
func GetRepairModuleBonuses(ctx context.Context, db *sql.DB, charID, shipID, moduleTypeID int64, isShieldBooster bool) (amountMult, cycleMult, capMult float64, err error) {
	amountMult, cycleMult, capMult = 1.0, 1.0, 1.0

	// ── skill bonuses ─────────────────────────────────────────────────────
	if charID != 0 {
		seen := map[int64]bool{}
		var candidates []int64
		reqs, rerr := GetRequiredSkills(ctx, db, moduleTypeID)
		if rerr == nil {
			for _, r := range reqs {
				if !seen[r.SkillID] {
					seen[r.SkillID] = true
					candidates = append(candidates, r.SkillID)
				}
			}
		}
		if isShieldBooster && !seen[dogma.SkillShieldCompensation] {
			candidates = append(candidates, dogma.SkillShieldCompensation)
		}
		for _, skillID := range candidates {
			lvl, lerr := GetCharacterSkillLevel(ctx, db, charID, skillID)
			if lerr != nil || lvl <= 0 {
				continue
			}
			attrs, aerr := TypeAttrs(ctx, db, skillID)
			if aerr != nil {
				continue
			}
			if b := attrs[dogma.AttrDurationSkillBonus]; b != 0 {
				cycleMult *= 1.0 + b/100.0*float64(lvl)
			}
			if b := attrs[dogma.AttrShieldBoostCapBonus]; b != 0 {
				capMult *= 1.0 + b/100.0*float64(lvl)
			}
		}
	}

	// ── fitted Shield Boost Amplifiers ────────────────────────────────────
	if isShieldBooster && shipID != 0 {
		rows, qerr := db.QueryContext(ctx, `
			SELECT e.typeID FROM entity e
			JOIN entity_attributes ea ON ea.itemID = e.itemID
			 AND ea.attributeID = 2 AND ea.valueInt = 1
			JOIN dgmTypeEffects te ON te.typeID = e.typeID AND te.effectID = ?
			WHERE e.locationID = ? AND e.flag BETWEEN 11 AND 34`,
			effectShieldBoostAmplifier, shipID)
		if qerr != nil {
			return amountMult, cycleMult, capMult, fmt.Errorf("GetRepairModuleBonuses amps: %w", qerr)
		}
		var ampTypes []int64
		for rows.Next() {
			var t int64
			if err := rows.Scan(&t); err == nil {
				ampTypes = append(ampTypes, t)
			}
		}
		rows.Close()
		var bonuses []float64
		for _, t := range ampTypes {
			attrs, aerr := TypeAttrs(ctx, db, t)
			if aerr != nil {
				continue
			}
			if b := attrs[dogma.AttrShieldBoostMultiplier]; b > 0 {
				bonuses = append(bonuses, b)
			}
		}
		// Stacking penalty, strongest first.
		sort.Sort(sort.Reverse(sort.Float64Slice(bonuses)))
		for i, b := range bonuses {
			amountMult *= 1.0 + b*StackingPenalty(i)/100.0
		}
	}

	if cycleMult < 0.25 {
		cycleMult = 0.25
	} else if cycleMult > 4.0 {
		cycleMult = 4.0
	}
	// Implants: the Crystal set and shield boost hardwirings scale a shield booster's amount;
	// the armour repair amount and Repair Systems cycle lines belong to the repairers.
	if charID != 0 {
		im := GetCharImplantMults(ctx, db, charID)
		if isShieldBooster {
			amountMult *= im.ShieldBoost
		} else {
			amountMult *= im.ArmorRepAmount
			cycleMult *= im.RepairCycle
		}
	}
	return amountMult, cycleMult, capMult, nil
}
