package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"gurimoon/rates"
	"gurimoon/syncx"
)

// ShipStats holds all per-ship data needed to initialise a destiny.Manager.
type ShipStats struct {
	ShipID         int64
	TypeID         int64
	GroupID        int64
	CategoryID     int64
	CorpID         int64
	AllianceID     int64
	CharName       string
	ShipName       string  // entity.itemName, the custom ship name (ball encoding, evelocations)
	SecurityStatus float64 // character_.securityRating
	Bounty         float64 // character_.bounty

	// From invTypes
	Mass   float64
	Radius float64

	// From dgmTypeAttributes
	MaxVelocity float64 // attributeID 37
	Agility     float64 // attributeID 70

	// From staStations (the station the character last docked at)
	StationX      float64
	StationY      float64
	StationZ      float64
	SolarSystemID int64

	// StationRadius is the station ball radius from mapDenormalize (0 with no row), the one
	// GetSystemCelestials builds the ball from. Read by the undock log and StationStandoffPoint,
	// not by the exit point. See docs/undock-geometry.md#exit-point.
	StationRadius float64

	// From staStationTypes (undock offset from station centre)
	DockEntryX       float64
	DockEntryY       float64
	DockEntryZ       float64
	DockOrientationX float64
	DockOrientationY float64
	DockOrientationZ float64
}

// GetShipStats fetches the ship statistics for shipID plus character data for charID and
// station position for stationID. Missing attributes fall back to defaults (250 m/s,
// agility 3.0).
func GetShipStats(ctx context.Context, db *sql.DB, shipID, charID, stationID int64) (*ShipStats, error) {
	s := &ShipStats{
		ShipID:      shipID,
		MaxVelocity: 250.0,
		Agility:     3.0,
		Mass:        1_000_000.0,
		Radius:      150.0,
	}

	// ── ship typeID, mass, radius ──────────────────────────────────────────
	var typeID, groupID, categoryID int64
	var mass, radius float64
	var shipName string
	err := db.QueryRowContext(ctx, `
		SELECT e.typeID, t.mass, t.radius, t.groupID, COALESCE(ig.categoryID, 0),
		       COALESCE(e.itemName, '')
		  FROM entity e
		  JOIN invTypes t ON t.typeID = e.typeID
		  LEFT JOIN invGroups ig ON ig.groupID = t.groupID
		 WHERE e.itemID = ?`, shipID,
	).Scan(&typeID, &mass, &radius, &groupID, &categoryID, &shipName)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("GetShipStats ship row: %w", err)
	}
	if err == nil {
		s.TypeID = typeID
		s.GroupID = groupID
		s.CategoryID = categoryID
		s.ShipName = shipName
		if mass > 0 {
			s.Mass = mass
		}
		if radius > 0 {
			s.Radius = radius
		}
	}

	// ── ship attributes from dgmTypeAttributes ─────────────────────────────
	if typeID > 0 {
		rows, err := db.QueryContext(ctx, `
			SELECT attributeID, COALESCE(NULLIF(valueFloat, 0), CAST(valueInt AS DECIMAL(20,6)), valueFloat)
			  FROM dgmTypeAttributes
			 WHERE typeID = ? AND attributeID IN (37, 70)`, typeID)
		if err != nil {
			return nil, fmt.Errorf("GetShipStats attributes: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var attrID int64
			var val float64
			if err := rows.Scan(&attrID, &val); err != nil {
				return nil, fmt.Errorf("GetShipStats attributes scan: %w", err)
			}
			switch attrID {
			case 37:
				s.MaxVelocity = val
			case 70:
				s.Agility = val
			}
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("GetShipStats attributes rows: %w", err)
		}
	}

	// ── apply navigation skill bonuses to velocity/agility ───────────────
	if charID > 0 && s.MaxVelocity > 0 {
		// Navigation (3449): +5% maxVelocity per level
		if navLvl, err := GetCharacterSkillLevel(ctx, db, charID, 3449); err == nil && navLvl > 0 {
			s.MaxVelocity *= 1.0 + float64(navLvl)*0.05
		}
		// Spaceship Command (3327): -2% inertia per level (agilityBonus attr 151 = -2, not a
		// velocity bonus).
		if scLvl, err := GetCharacterSkillLevel(ctx, db, charID, 3327); err == nil && scLvl > 0 {
			s.Agility *= 1.0 - float64(scLvl)*0.02
		}
		// Evasive Maneuvering (3453): -5% inertia per level
		if emLvl, err := GetCharacterSkillLevel(ctx, db, charID, 3453); err == nil && emLvl > 0 {
			s.Agility *= 1.0 - float64(emLvl)*0.05
		}
		// Implants: Snake set and the velocity / agility hardwirings; computeShipDerivedAttrs
		// folds the same terms.
		im := GetCharImplantMults(ctx, db, charID)
		s.MaxVelocity *= im.Velocity
		s.Agility *= im.Agility
	}

	// ── fitted hull mods: overdrives/nanofibers (velocity), istabs (agility) ──
	if s.MaxVelocity > 0 {
		if pb, err := GetShipPassiveBonuses(ctx, db, shipID); err == nil {
			s.MaxVelocity *= pb.VelocityMult
			s.Agility *= pb.AgilityMult
		}
	}

	// ── hull velocity bonus (industrials, Stabber...) per command-skill level ──
	if s.MaxVelocity > 0 && typeID > 0 && charID > 0 {
		if hb, err := GetShipHullBonuses(ctx, db, charID, typeID); err == nil && len(hb) > 0 {
			s.MaxVelocity *= hb.StatMults().Velocity
		}
	}

	// ── character name and corporation ────────────────────────────────────
	if charID > 0 {
		var corpID, charSysID int64
		var charName string
		var secRating, bounty float64
		err := db.QueryRowContext(ctx,
			`SELECT corporationID, characterName, COALESCE(solarSystemID, 0),
			        COALESCE(securityRating, 0), COALESCE(bounty, 0)
			   FROM character_ WHERE characterID = ?`, charID,
		).Scan(&corpID, &charName, &charSysID, &secRating, &bounty)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("GetShipStats character: %w", err)
		}
		if err == nil {
			s.CorpID = corpID
			s.CharName = charName
			s.SecurityStatus = secRating
			s.Bounty = bounty
			// In space (stationID 0) the station query is skipped, so take solarSystemID from
			// character_ for the destiny manager.
			if s.SolarSystemID == 0 {
				s.SolarSystemID = charSysID
			}
			// Look up alliance from corporation table.
			if corpID > 0 {
				var allianceID sql.NullInt64
				_ = db.QueryRowContext(ctx,
					`SELECT allianceID FROM corporation WHERE corporationID = ?`, corpID,
				).Scan(&allianceID)
				if allianceID.Valid {
					s.AllianceID = allianceID.Int64
				}
			}
		}
	}

	// ── station position + undock offset from stationType ────────────────
	if stationID > 0 {
		var x, y, z float64
		var sysID int64
		var dex, dey, dez, dox, doy, doz float64
		var stationRadius float64
		err := db.QueryRowContext(ctx, `
			SELECT s.x, s.y, s.z, s.solarSystemID,
			       COALESCE(t.dockEntryX, 0), COALESCE(t.dockEntryY, 0), COALESCE(t.dockEntryZ, 0),
			       COALESCE(t.dockOrientationX, 0), COALESCE(t.dockOrientationY, 0), COALESCE(t.dockOrientationZ, 0),
			       COALESCE(d.radius, 0)
			  FROM staStations s
			  LEFT JOIN staStationTypes t ON s.stationTypeID = t.stationTypeID
			  LEFT JOIN mapDenormalize d ON d.itemID = s.stationID
			 WHERE s.stationID = ?`, stationID,
		).Scan(&x, &y, &z, &sysID, &dex, &dey, &dez, &dox, &doy, &doz, &stationRadius)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("GetShipStats station: %w", err)
		}
		if err == nil {
			s.StationX = x
			s.StationY = y
			s.StationZ = z
			s.SolarSystemID = sysID
			s.DockEntryX = dex
			s.DockEntryY = dey
			s.DockEntryZ = dez
			s.DockOrientationX = dox
			s.DockOrientationY = doy
			s.DockOrientationZ = doz
			s.StationRadius = stationRadius
		}
	}

	return s, nil
}

// GetEntityPosition returns the persisted x, y, z of an entity. A login in space reads
// the ship's last position from here.
func GetEntityPosition(ctx context.Context, db *sql.DB, entityID int64) (x, y, z float64, ok bool, err error) {
	err = db.QueryRowContext(ctx,
		`SELECT COALESCE(x, 0), COALESCE(y, 0), COALESCE(z, 0) FROM entity WHERE itemID = ?`,
		entityID,
	).Scan(&x, &y, &z)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, 0, false, fmt.Errorf("GetEntityPosition entityID=%d: %w", entityID, err)
	}
	return x, y, z, true, nil
}

// NPCStats holds invTypes data needed to spawn an NPC ship in space.
type NPCStats struct {
	TypeName    string
	GroupID     int64
	CategoryID  int64
	Mass        float64
	Radius      float64
	MaxVelocity float64
	Agility     float64
}

// GetNPCStats fetches ship type data for an NPC spawn from invTypes,
// invGroups, and dgmTypeAttributes.
func GetNPCStats(ctx context.Context, db *sql.DB, typeID int64) (*NPCStats, error) {
	s := &NPCStats{
		MaxVelocity: 0.0,
		Agility:     3.0,
		Mass:        1_000_000.0,
		Radius:      150.0,
	}

	err := db.QueryRowContext(ctx, `
		SELECT t.typeName, t.groupID, COALESCE(ig.categoryID, 0),
		       COALESCE(t.mass, 0), COALESCE(t.radius, 0)
		  FROM invTypes t
		  LEFT JOIN invGroups ig ON ig.groupID = t.groupID
		 WHERE t.typeID = ?`, typeID,
	).Scan(&s.TypeName, &s.GroupID, &s.CategoryID, &s.Mass, &s.Radius)
	if err != nil {
		return nil, fmt.Errorf("GetNPCStats typeID=%d: %w", typeID, err)
	}
	if s.Mass <= 0 {
		s.Mass = 1_000_000.0
	}
	if s.Radius <= 0 {
		s.Radius = 150.0
	}

	// Fetch maxVelocity (37), agility (70) and entityCruiseSpeed (508) from
	// dgmTypeAttributes.
	rows, err := db.QueryContext(ctx, `
		SELECT attributeID, COALESCE(NULLIF(valueFloat, 0), CAST(valueInt AS DECIMAL(20,6)), valueFloat)
		  FROM dgmTypeAttributes
		 WHERE typeID = ? AND attributeID IN (37, 70, 508)`, typeID)
	if err != nil {
		return nil, fmt.Errorf("GetNPCStats attributes typeID=%d: %w", typeID, err)
	}
	defer rows.Close()
	var cruiseSpeed float64
	for rows.Next() {
		var attrID int64
		var val float64
		if err := rows.Scan(&attrID, &val); err != nil {
			return nil, fmt.Errorf("GetNPCStats attributes scan: %w", err)
		}
		switch attrID {
		case 37:
			if val > 0 {
				s.MaxVelocity = val
			}
		case 70:
			if val > 0 {
				s.Agility = val
			}
		case 508:
			if val > 0 {
				cruiseSpeed = val
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("GetNPCStats attributes rows: %w", err)
	}

	// NPC entities fly at entityCruiseSpeed; attr 37 on entity types is the burst/chase stat
	// (1280 against cruise 160 on a Loyal Hellhound), which makes rats orbit far too fast.
	if cruiseSpeed > 0 {
		s.MaxVelocity = cruiseSpeed
	}

	return s, nil
}

// GetFactionCorpID returns the primary NPC corporation for a factionID by
// querying crpNPCCorporations. Returns 0 if not found.
func GetFactionCorpID(ctx context.Context, db *sql.DB, factionID int64) int64 {
	var corpID int64
	_ = db.QueryRowContext(ctx,
		`SELECT corporationID FROM crpNPCCorporations WHERE factionID = ? LIMIT 1`,
		factionID).Scan(&corpID)
	return corpID
}

// UpdateEntityPosition updates the x, y, z of an entity row, persisting the ship position
// after warp or gate jump.
func UpdateEntityPosition(ctx context.Context, db *sql.DB, entityID int64, x, y, z float64) error {
	_, err := db.ExecContext(ctx,
		`UPDATE entity SET x=?, y=?, z=? WHERE itemID=?`, x, y, z, entityID)
	if err != nil {
		return fmt.Errorf("UpdateEntityPosition entityID=%d: %w", entityID, err)
	}
	return nil
}

// UpdateCharacterLocation updates solarSystemID, constellationID and regionID on the
// character_ row after a stargate jump.
func UpdateCharacterLocation(ctx context.Context, db *sql.DB, characterID, solarSystemID, constellationID, regionID int64) error {
	_, err := db.ExecContext(ctx,
		`UPDATE character_ SET solarSystemID=?, constellationID=?, regionID=? WHERE characterID=?`,
		solarSystemID, constellationID, regionID, characterID)
	if err != nil {
		return fmt.Errorf("UpdateCharacterLocation charID=%d: %w", characterID, err)
	}
	// Feed the star map's systems-visited overlay. The location write has already succeeded,
	// so a failure here is reported without undoing it.
	if err := RecordSolarSystemVisit(ctx, db, characterID, solarSystemID,
		uint64((time.Now().Unix()+11644473600)*10000000)); err != nil {
		return fmt.Errorf("UpdateCharacterLocation visit charID=%d: %w", characterID, err)
	}
	return nil
}

// GetSolarSystemLocation returns the constellation and region IDs for a solar system, for
// session-change notifications that need all three location tiers.
func GetSolarSystemLocation(ctx context.Context, db *sql.DB, solarSystemID int64) (constellationID, regionID int64, err error) {
	err = db.QueryRowContext(ctx,
		`SELECT constellationID, regionID FROM mapSolarSystems WHERE solarSystemID=?`, solarSystemID,
	).Scan(&constellationID, &regionID)
	if errors.Is(err, sql.ErrNoRows) {
		err = fmt.Errorf("GetSolarSystemLocation: sysID %d not found", solarSystemID)
	} else if err != nil {
		err = fmt.Errorf("GetSolarSystemLocation: %w", err)
	}
	return
}

// CelestialEntity holds data for one entry from mapDenormalize (stars,
// planets, moons, asteroid belts, stargates, stations, etc.).
type CelestialEntity struct {
	ItemID     int64
	TypeID     int64
	GroupID    int64
	CategoryID int64
	X          float64
	Y          float64
	Z          float64
	Radius     float64
	Name       string
	// OrbitID is the parent body this entity orbits (planet for moons,
	// star for planets). Zero when mapDenormalize has no orbit parent.
	OrbitID int64
	// OwnerID is the corporationID that owns this entity (stations only).
	// Zero for non-station celestials.
	OwnerID int64
	// Stargate jump destination (groupID==10 only; both zero otherwise).
	DestStargateID    int64
	DestSolarSystemID int64
}

// GetSystemCelestials returns all celestial entities for a solar system,
// sourced from the mapDenormalize table.
func GetSystemCelestials(ctx context.Context, db *sql.DB, solarSystemID int64) ([]CelestialEntity, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT d.itemID, d.typeID, d.groupID,
		       COALESCE(ig.categoryID, 0),
		       COALESCE(d.x, 0), COALESCE(d.y, 0), COALESCE(d.z, 0),
		       COALESCE(d.radius, t.radius, 1), COALESCE(d.itemName, ''),
		       COALESCE(d.orbitID, 0),
		       COALESCE(
		           (SELECT c.corporationID FROM corporation c WHERE c.corporationID = ss.corporationID),
		           0),
		       COALESCE(mj.celestialID, 0),
		       COALESCE(dest.solarSystemID, 0)
		  FROM mapDenormalize d
		  LEFT JOIN invTypes t ON t.typeID = d.typeID
		  LEFT JOIN invGroups ig ON ig.groupID = d.groupID
		  LEFT JOIN staStations ss ON ss.stationID = d.itemID
		  LEFT JOIN mapJumps mj ON mj.stargateID = d.itemID
		  LEFT JOIN mapDenormalize dest ON dest.itemID = mj.celestialID
		 WHERE d.solarSystemID = ?`, solarSystemID)
	if err != nil {
		return nil, fmt.Errorf("GetSystemCelestials: %w", err)
	}
	defer rows.Close()
	var out []CelestialEntity
	for rows.Next() {
		var e CelestialEntity
		if err := rows.Scan(&e.ItemID, &e.TypeID, &e.GroupID,
			&e.CategoryID,
			&e.X, &e.Y, &e.Z, &e.Radius, &e.Name,
			&e.OrbitID,
			&e.OwnerID,
			&e.DestStargateID, &e.DestSolarSystemID); err != nil {
			return nil, fmt.Errorf("GetSystemCelestials scan: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetRegionCelestials loads the celestials of every system in a region, grouped by
// solarSystemID, with GetSystemCelestials' join; CelestialCache bulk-loads a region from it.
// The region and constellation rows in mapDenormalize carry a NULL solarSystemID and are
// excluded, since scanning one into an int64 fails the whole load.
func GetRegionCelestials(ctx context.Context, db *sql.DB, regionID int64) (map[int64][]CelestialEntity, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT d.solarSystemID, d.itemID, d.typeID, d.groupID,
		       COALESCE(ig.categoryID, 0),
		       COALESCE(d.x, 0), COALESCE(d.y, 0), COALESCE(d.z, 0),
		       COALESCE(d.radius, t.radius, 1), COALESCE(d.itemName, ''),
		       COALESCE(d.orbitID, 0),
		       COALESCE(
		           (SELECT c.corporationID FROM corporation c WHERE c.corporationID = ss.corporationID),
		           0),
		       COALESCE(mj.celestialID, 0),
		       COALESCE(dest.solarSystemID, 0)
		  FROM mapDenormalize d
		  LEFT JOIN invTypes t ON t.typeID = d.typeID
		  LEFT JOIN invGroups ig ON ig.groupID = d.groupID
		  LEFT JOIN staStations ss ON ss.stationID = d.itemID
		  LEFT JOIN mapJumps mj ON mj.stargateID = d.itemID
		  LEFT JOIN mapDenormalize dest ON dest.itemID = mj.celestialID
		 WHERE d.regionID = ? AND d.solarSystemID IS NOT NULL`, regionID)
	if err != nil {
		return nil, fmt.Errorf("GetRegionCelestials regionID=%d: %w", regionID, err)
	}
	defer rows.Close()
	out := make(map[int64][]CelestialEntity)
	for rows.Next() {
		var sysID int64
		var e CelestialEntity
		if err := rows.Scan(&sysID, &e.ItemID, &e.TypeID, &e.GroupID,
			&e.CategoryID,
			&e.X, &e.Y, &e.Z, &e.Radius, &e.Name,
			&e.OrbitID,
			&e.OwnerID,
			&e.DestStargateID, &e.DestSolarSystemID); err != nil {
			return nil, fmt.Errorf("GetRegionCelestials scan: %w", err)
		}
		out[sysID] = append(out[sysID], e)
	}
	return out, rows.Err()
}

// GetAllSystemSecurities loads every solarSystemID→security pair from mapSolarSystems in
// one query, to preload security lookups at boot.
func GetAllSystemSecurities(ctx context.Context, db *sql.DB) (map[int64]float64, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT solarSystemID, security FROM mapSolarSystems`)
	if err != nil {
		return nil, fmt.Errorf("GetAllSystemSecurities: %w", err)
	}
	defer rows.Close()
	out := make(map[int64]float64, 8192)
	for rows.Next() {
		var sysID int64
		var sec float64
		if err := rows.Scan(&sysID, &sec); err != nil {
			return nil, fmt.Errorf("GetAllSystemSecurities scan: %w", err)
		}
		out[sysID] = sec
	}
	return out, rows.Err()
}

// SystemPos is a solar system's position in metres. Plain floats because db does not
// import destiny; callers build the vector.
type SystemPos struct{ X, Y, Z float64 }

// GetAllSystemPositions loads every solar system's galactic coordinates at boot. A capital
// jump range check is a distance between two systems on every activation, so it reads memory.
func GetAllSystemPositions(ctx context.Context, db *sql.DB) (map[int64]SystemPos, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT solarSystemID, x, y, z FROM mapSolarSystems`)
	if err != nil {
		return nil, fmt.Errorf("GetAllSystemPositions: %w", err)
	}
	defer rows.Close()
	out := make(map[int64]SystemPos, 8192)
	for rows.Next() {
		var sysID int64
		var p SystemPos
		if err := rows.Scan(&sysID, &p.X, &p.Y, &p.Z); err != nil {
			return nil, fmt.Errorf("GetAllSystemPositions scan: %w", err)
		}
		out[sysID] = p
	}
	return out, rows.Err()
}

// GetAllSystemSovFactions loads every solarSystemID→factionID pair, with the
// constellation→region COALESCE fallback in the SQL, to preload sovereign factions at boot.
func GetAllSystemSovFactions(ctx context.Context, db *sql.DB) (map[int64]int64, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT s.solarSystemID,
		       COALESCE(s.factionID, c.factionID, r.factionID, 0)
		  FROM mapSolarSystems s
		  LEFT JOIN mapConstellations c ON c.constellationID = s.constellationID
		  LEFT JOIN mapRegions r ON r.regionID = s.regionID`)
	if err != nil {
		return nil, fmt.Errorf("GetAllSystemSovFactions: %w", err)
	}
	defer rows.Close()
	out := make(map[int64]int64, 8192)
	for rows.Next() {
		var sysID, factionID int64
		if err := rows.Scan(&sysID, &factionID); err != nil {
			return nil, fmt.Errorf("GetAllSystemSovFactions scan: %w", err)
		}
		out[sysID] = factionID
	}
	return out, rows.Err()
}

// WeaponDamageProfile holds the per-type damage values for a weapon cycle.
type WeaponDamageProfile struct {
	Em         float64
	Explosive  float64
	Kinetic    float64
	Thermal    float64
	Multiplier float64 // weapon damage multiplier (attr 64)
}

// Total returns the sum of all four damage types after applying the multiplier.
func (w WeaponDamageProfile) Total() float64 {
	return (w.Em + w.Explosive + w.Kinetic + w.Thermal) * w.Multiplier
}

// GetWeaponDamageProfile returns the damage profile for a weapon module and
// its loaded charge. Turret damage = charge(em+expl+kin+therm) * weapon(damageMultiplier).
// For weapons with no charge, the damage attributes are read from the weapon itself.
func GetWeaponDamageProfile(ctx context.Context, db *sql.DB, weaponTypeID, chargeTypeID int64) (*WeaponDamageProfile, error) {
	p := &WeaponDamageProfile{Multiplier: 1.0}

	// Get weapon's damage multiplier (attr 64).
	weaponAttrs, err := GetTypeAttributes(ctx, db, weaponTypeID)
	if err != nil {
		return nil, fmt.Errorf("GetWeaponDamageProfile weapon attrs: %w", err)
	}
	if m, ok := weaponAttrs[64]; ok && m > 0 {
		p.Multiplier = m
	}

	// Get damage values from the charge (ammo) if loaded, otherwise from weapon itself.
	srcTypeID := chargeTypeID
	if srcTypeID == 0 {
		srcTypeID = weaponTypeID
	}
	dmgAttrs, err := GetTypeAttributes(ctx, db, srcTypeID)
	if err != nil {
		return nil, fmt.Errorf("GetWeaponDamageProfile charge attrs: %w", err)
	}
	p.Em = dmgAttrs[114]        // emDamage
	p.Explosive = dmgAttrs[116] // explosiveDamage
	p.Kinetic = dmgAttrs[117]   // kineticDamage
	p.Thermal = dmgAttrs[118]   // thermalDamage

	return p, nil
}

// EntityHP holds the HP and resistance values for an entity type.
type EntityHP struct {
	ShieldHP    float64
	ArmorHP     float64
	StructureHP float64
	ShieldTau   float64 // recharge time in seconds
	// Resistances are [em, explosive, kinetic, thermal] resonances, 0.0 (immune) to 1.0 (no resist).
	ShieldRes [4]float64
	ArmorRes  [4]float64
	HullRes   [4]float64
}

// GetEntityHPByType returns the HP and resistance stats for a given typeID
// from dgmTypeAttributes.
func GetEntityHPByType(ctx context.Context, db *sql.DB, typeID int64) (*EntityHP, error) {
	attrs, err := GetTypeAttributes(ctx, db, typeID)
	if err != nil {
		return nil, fmt.Errorf("GetEntityHPByType: %w", err)
	}

	hp := &EntityHP{
		ShieldHP:    attrs[263],          // shieldCapacity
		ArmorHP:     attrs[265],          // armorHP
		StructureHP: attrs[9],            // hp (structure)
		ShieldTau:   attrs[479] / 1000.0, // shieldRechargeRate ms → seconds
	}
	// Default reasonable values if DB has none.
	if hp.StructureHP <= 0 {
		hp.StructureHP = 100
	}
	if hp.ShieldTau <= 0 {
		hp.ShieldTau = 100000
	}

	// Shield resonances (default 1.0 = no resist).
	hp.ShieldRes = [4]float64{
		attrOrDefault(attrs, 271, 1.0), // shieldEmResonance
		attrOrDefault(attrs, 272, 1.0), // shieldExplResonance
		attrOrDefault(attrs, 273, 1.0), // shieldKinResonance
		attrOrDefault(attrs, 274, 1.0), // shieldThermResonance
	}
	hp.ArmorRes = [4]float64{
		attrOrDefault(attrs, 267, 1.0), // armorEmResonance
		attrOrDefault(attrs, 268, 1.0), // armorExplResonance
		attrOrDefault(attrs, 269, 1.0), // armorKinResonance
		attrOrDefault(attrs, 270, 1.0), // armorThermResonance
	}
	hp.HullRes = [4]float64{
		attrOrDefault(attrs, 113, 1.0), // hullEmResonance
		attrOrDefault(attrs, 111, 1.0), // hullExplResonance
		attrOrDefault(attrs, 109, 1.0), // hullKinResonance
		attrOrDefault(attrs, 110, 1.0), // hullThermResonance
	}
	return hp, nil
}

func attrOrDefault(attrs map[int64]float64, attrID int64, def float64) float64 {
	if v, ok := attrs[attrID]; ok && v > 0 {
		return v
	}
	return def
}

// MissileFlightParams holds the velocity, max flight time and explosion
// cloud parameters for a missile type.
type MissileFlightParams struct {
	Velocity      float64 // m/s
	MaxFlightTime float64 // seconds
	AoeCloudSize  float64 // explosion cloud signature in m (attr 654, 0 = none)
	AoeVelocity   float64 // explosion cloud velocity in m/s (attr 653)
}

// GetMissileFlightParams returns the missile flight velocity (attr 37), max
// flight time derived from explosionDelay (attr 281), and the explosion
// cloud size/velocity (attrs 654/653) for the given missile/charge typeID.
func GetMissileFlightParams(ctx context.Context, db *sql.DB, missileTypeID int64) (*MissileFlightParams, error) {
	attrs, err := GetTypeAttributes(ctx, db, missileTypeID)
	if err != nil {
		return nil, fmt.Errorf("GetMissileFlightParams typeID=%d: %w", missileTypeID, err)
	}
	p := &MissileFlightParams{
		Velocity:      attrs[37],           // maxVelocity
		MaxFlightTime: attrs[281] / 1000.0, // explosionDelay ms → seconds
		AoeCloudSize:  attrs[654],
		AoeVelocity:   attrs[653],
	}
	if p.Velocity <= 0 {
		p.Velocity = 3000 // default 3 km/s
	}
	if p.MaxFlightTime <= 0 {
		p.MaxFlightTime = 15 // default 15s
	}
	return p, nil
}

// EffectiveRange is the distance past which this NPC's weapon cannot reach its target: the
// missile's own range (velocity × flight time) for a launcher, entityAttackRange (attr 247)
// for a turret. It replaces attr 247 on a missile NPC instead of taking the max, or a rat opens
// fire from beyond where its missiles expire. MissileRange 0 keeps attr 247.
func (p *NPCWeaponProfile) EffectiveRange() float64 {
	if p == nil {
		return 0
	}
	if p.MissileTypeID != 0 && p.MissileRange > 0 {
		return p.MissileRange
	}
	return p.AttackRange
}

// GetTypeGroupCategory returns the groupID and categoryID for a given typeID.
func GetTypeGroupCategory(ctx context.Context, db *sql.DB, typeID int64) (groupID, categoryID int64, err error) {
	err = db.QueryRowContext(ctx, `
		SELECT t.groupID, COALESCE(g.categoryID, 0)
		  FROM invTypes t
		  JOIN invGroups g ON g.groupID = t.groupID
		 WHERE t.typeID = ?`, typeID,
	).Scan(&groupID, &categoryID)
	if err != nil {
		err = fmt.Errorf("GetTypeGroupCategory typeID=%d: %w", typeID, err)
	}
	return
}

// GetTypeRaceID returns invTypes.raceID for a typeID (1 Caldari, 2 Minmatar,
// 4 Amarr, 8 Gallente). Returns 0 when the column is NULL or the type is
// unknown, so callers can fall back to another race source.
func GetTypeRaceID(ctx context.Context, db *sql.DB, typeID int64) int64 {
	var race sql.NullInt64
	_ = db.QueryRowContext(ctx,
		`SELECT raceID FROM invTypes WHERE typeID = ?`, typeID).Scan(&race)
	if race.Valid {
		return race.Int64
	}
	return 0
}

// NPCWeaponProfile holds all combat-relevant attributes for an NPC typeID.
type NPCWeaponProfile struct {
	Em, Explosive, Kinetic, Thermal float64 // base damage per cycle
	Multiplier                      float64 // attr 64, weapon damage multiplier
	OptimalRange                    float64 // attr 54 maxRange (metres)
	Falloff                         float64 // attr 158 (metres)
	CycleTimeMs                     int64   // attr 51 speed / 73 duration (ms)
	TrackingSpeed                   float64 // attr 160
	MissileTypeID                   int64   // attr 507 entityMissileTypeID (0 = turret NPC)
	FlyRange                        float64 // attr 416 entityFlyRange (preferred orbit range)
	AttackRange                     float64 // attr 247 entityAttackRange (opens fire within this)
	ChaseMaxDist                    float64 // attr 665 entityChaseMaxDistance (disengage distance)
	AttackDelayMinMs                int64   // attr 475 entityAttackDelayMin (first-shot delay)
	AttackDelayMaxMs                int64   // attr 476 entityAttackDelayMax
	SigRadius                       float64 // attr 552
	WeaponSigRadius                 float64 // attr 620 optimalSigRadius (turret sig resolution for tracking)
	TurretTypeID                    int64   // derived turret typeID for visual hardpoints
	TurretOverridden                bool    // true when TurretTypeID came from attr 10001 override
	TurretSlots                     int     // number of turret hardpoints (attr 102, default 2)
	GroupID                         int64   // invTypes.groupID for hull size classification
	MissileRange                    float64 // pre-computed missile max range (velocity * flightTime)
}

// GetNPCWeaponProfile returns the combat attributes for an NPC typeID from
// dgmTypeAttributes. Returns nil (no error) if the NPC has no damage attributes.
func GetNPCWeaponProfile(ctx context.Context, db *sql.DB, typeID int64) (*NPCWeaponProfile, error) {
	attrs, err := GetTypeAttributes(ctx, db, typeID)
	if err != nil {
		return nil, fmt.Errorf("GetNPCWeaponProfile typeID=%d: %w", typeID, err)
	}

	// Fetch groupID for hull size classification (turret size selection).
	var groupID int64
	_ = db.QueryRowContext(ctx, `SELECT groupID FROM invTypes WHERE typeID = ?`, typeID).Scan(&groupID)

	p := &NPCWeaponProfile{
		Em:               attrs[114],
		Explosive:        attrs[116],
		Kinetic:          attrs[117],
		Thermal:          attrs[118],
		Multiplier:       attrs[64],
		OptimalRange:     attrs[54],
		Falloff:          attrs[158],
		TrackingSpeed:    attrs[160],
		MissileTypeID:    int64(attrs[507]),
		FlyRange:         attrs[416], // entityFlyRange
		AttackRange:      attrs[247], // entityAttackRange
		ChaseMaxDist:     attrs[665], // entityChaseMaxDistance
		AttackDelayMinMs: int64(attrs[475]),
		AttackDelayMaxMs: int64(attrs[476]),
		SigRadius:        attrs[552],
		WeaponSigRadius:  attrs[620], // optimalSigRadius, defaulted below when zero
	}

	// Cycle time: prefer attr 51 (speed), fall back to attr 73 (duration).
	if v := attrs[51]; v > 0 {
		p.CycleTimeMs = int64(v)
	} else if v := attrs[73]; v > 0 {
		p.CycleTimeMs = int64(v)
	}

	// Defaults.
	if p.Multiplier <= 0 {
		p.Multiplier = 1.0
	}
	if p.CycleTimeMs <= 0 {
		p.CycleTimeMs = 3000 // 3 seconds default
	}
	if p.OptimalRange <= 0 {
		p.OptimalRange = 10000 // 10km default
	}
	if p.FlyRange <= 0 {
		p.FlyRange = p.OptimalRange * 0.8
	}
	// A floor, not a default: some types carry entityAttackRange below their own optimal
	// (Mercenary Rookie 11679: 2500 against 5000), which locks but never fires.
	if minAttack := p.OptimalRange + p.Falloff*2; p.AttackRange < minAttack {
		p.AttackRange = minAttack
	}
	if p.ChaseMaxDist <= 0 {
		p.ChaseMaxDist = 150000 // 150km default
	}
	if p.SigRadius <= 0 {
		p.SigRadius = 100
	}
	// Weapon signature resolution: attr 620, else by hull size (40/125/400 m). The NPC's own
	// signature (attr 552) would give small NPCs a tracking bonus against larger ships.
	if p.WeaponSigRadius <= 0 {
		switch npcTurretSize(groupID) {
		case 2:
			p.WeaponSigRadius = 400 // large turrets
		case 1:
			p.WeaponSigRadius = 125 // medium turrets
		default:
			p.WeaponSigRadius = 40 // small turrets
		}
	}

	// Missile NPCs often carry damage on the missile type: fall back to its damage attributes
	// scaled by the entity's missileDamageMultiplier (attr 212).
	if p.MissileTypeID != 0 && p.Em+p.Explosive+p.Kinetic+p.Thermal <= 0 {
		if mAttrs, mErr := GetTypeAttributes(ctx, db, p.MissileTypeID); mErr == nil {
			p.Em = mAttrs[114]
			p.Explosive = mAttrs[116]
			p.Kinetic = mAttrs[117]
			p.Thermal = mAttrs[118]
			if mult := attrs[212]; mult > 0 {
				p.Multiplier *= mult
			}
		}
	}

	// If NPC has no damage at all, return nil (unarmed).
	if p.Em+p.Explosive+p.Kinetic+p.Thermal <= 0 {
		return nil, nil
	}

	p.GroupID = groupID

	// Pre-compute missile max range (velocity × flight time) so combat does not query per cycle.
	if p.MissileTypeID != 0 {
		if fp, err := GetMissileFlightParams(ctx, db, p.MissileTypeID); err == nil {
			p.MissileRange = fp.Velocity * fp.MaxFlightTime
		}
	}

	// Turret hardpoint count from attr 102 (turretSlotsLeft).
	if v := int(attrs[102]); v > 0 {
		p.TurretSlots = v
	} else {
		p.TurretSlots = 2
	}

	// Non-missile NPCs derive the turret typeID from damage profile and hull group so the client
	// loads the right model; attr 10001 overrides (mining lasers, capital turrets).
	if p.MissileTypeID == 0 {
		if override := int64(attrs[10001]); override != 0 {
			p.TurretTypeID = override
			p.TurretOverridden = true
		} else {
			p.TurretTypeID = npcTurretTypeFromDamage(p.Em, p.Explosive, p.Kinetic, p.Thermal, groupID)
		}
	}

	return p, nil
}

// NPCEwarModule is one electronic-warfare capability an NPC type carries, read off
// dgmTypeAttributes: a per-cycle Chance, a DurationMs, a MaxRange and, for attribute
// debuffs, a Multiplier on the victim's stat.
type NPCEwarModule struct {
	Kind       string  // "scram", "web", "jam", "capDrain", "sensorDamp", "trackingDisrupt", "targetPaint"
	Chance     float64 // per-cycle probability, 0-1
	DurationMs int64   // how long the debuff holds
	MaxRange   float64 // metres
	Falloff    float64 // metres beyond MaxRange where chance decays to zero
	Multiplier float64 // stat multiplier for damp/TD/paint, speedFactor percent for web
	Amount     float64 // GJ drained per cycle for capDrain
	Strength   float64 // warp scramble points for scram (attr 105), 0 = not applicable
}

// NPCEwarProfile is the full electronic-warfare loadout of an NPC type.
type NPCEwarProfile struct {
	Modules []NPCEwarModule
}

// ewarAttrSet describes where one EWAR kind's attributes live. Laid out as
// data because the six kinds differ only in attribute IDs.
type ewarAttrSet struct {
	kind       string
	chance     int64
	duration   int64
	maxRange   int64
	falloff    int64
	multiplier int64
	amount     int64
	strength   int64
}

var npcEwarAttrSets = []ewarAttrSet{
	// entityWarpScrambleChance has no duration (the debuff holds while the lock does) but has a
	// range, attr 103 warpScrambleRange, unrelated to weapon range (a Kador Surveillance Sergeant
	// shoots to 250 km and scrambles to 20 km). Deriving it from the weapon scrambles from too far.
	{kind: "scram", chance: 504, maxRange: 103, strength: 105},
	// modifyTargetSpeed* is the entity stasis web. Its multiplier is attr 20 speedFactor, usually
	// a negative percent (-75 = x0.25), but some types (the Serpentis frigates) author a fractional
	// positive that is the multiplier itself; the apply side handles both.
	{kind: "web", chance: 512, duration: 513, maxRange: 514, multiplier: 20},
	{kind: "jam", chance: 930, duration: 929, maxRange: 936, falloff: 953},
	{kind: "capDrain", chance: 931, duration: 942, maxRange: 937, falloff: 952, amount: 946},
	{kind: "sensorDamp", chance: 932, duration: 943, maxRange: 938, falloff: 950, multiplier: 947},
	{kind: "trackingDisrupt", chance: 933, duration: 944, maxRange: 940, falloff: 951, multiplier: 948},
	{kind: "targetPaint", chance: 935, duration: 945, maxRange: 941, falloff: 954, multiplier: 949},
}

// GetNPCEwarProfile reads an NPC type's electronic-warfare capabilities.
// Returns nil (no error) when the type carries none, which is the common case.
func GetNPCEwarProfile(ctx context.Context, db *sql.DB, typeID int64) (*NPCEwarProfile, error) {
	attrs, err := GetTypeAttributes(ctx, db, typeID)
	if err != nil {
		return nil, fmt.Errorf("GetNPCEwarProfile typeID=%d: %w", typeID, err)
	}
	var p NPCEwarProfile
	for _, set := range npcEwarAttrSets {
		chance := attrs[set.chance]
		if chance <= 0 {
			continue
		}
		m := NPCEwarModule{Kind: set.kind, Chance: chance}
		if set.duration != 0 {
			m.DurationMs = int64(attrs[set.duration])
		}
		if set.maxRange != 0 {
			m.MaxRange = attrs[set.maxRange]
		}
		if set.falloff != 0 {
			m.Falloff = attrs[set.falloff]
		}
		if set.multiplier != 0 {
			m.Multiplier = attrs[set.multiplier]
		}
		if set.amount != 0 {
			m.Amount = attrs[set.amount]
		}
		if set.strength != 0 {
			m.Strength = attrs[set.strength]
		}
		// Chance is 0-1 except some modifyTargetSpeedChance rows written as whole percents (50, 100),
		// so 10 or more is divided by 100. Values just over 1 mean always.
		if m.Chance >= 10 {
			m.Chance /= 100
		}
		// A chance with no duration would apply a debuff that never expires.
		if m.DurationMs <= 0 && set.duration != 0 {
			m.DurationMs = 20000
		}
		p.Modules = append(p.Modules, m)
	}
	if len(p.Modules) == 0 {
		return nil, nil
	}
	return &p, nil
}

// npcTurretTypeFromDamage picks a representative turret typeID from the NPC's damage profile
// and hull group so the client draws the right turret at the right size: EM/thermal gives a
// laser, kinetic/thermal a hybrid blaster, kinetic/explosive a projectile autocannon.
func npcTurretTypeFromDamage(em, explosive, kinetic, thermal float64, groupID int64) int64 {
	// Small turrets (frigates, destroyers).
	const (
		smallLaser      = 450 // Gatling Pulse Laser I
		smallBlaster    = 564 // Light Neutron Blaster I
		smallAutoCannon = 484 // 125mm Gatling AutoCannon I
	)
	// Medium turrets (cruisers, battlecruisers).
	const (
		medLaser      = 456 // Focused Medium Pulse Laser I
		medBlaster    = 568 // Heavy Neutron Blaster I
		medAutoCannon = 490 // 220mm Vulcan AutoCannon I
	)
	// Large turrets (battleships).
	const (
		largeLaser      = 462 // Mega Pulse Laser I
		largeBlaster    = 575 // Ion Blaster Cannon I
		largeAutoCannon = 496 // 800mm Repeating Artillery I
	)

	size := npcTurretSize(groupID)

	emTherm := em + thermal
	kinExpl := kinetic + explosive
	kinTherm := kinetic + thermal

	weaponClass := 0 // 0=laser, 1=hybrid, 2=projectile
	if kinetic > 0 && thermal > 0 && kinTherm > kinExpl && !(em > 0 && emTherm >= kinExpl && emTherm >= kinTherm) {
		weaponClass = 1
	} else if kinetic > 0 || explosive > 0 {
		if !(em > 0 && emTherm >= kinExpl && emTherm >= kinTherm) {
			weaponClass = 2
		}
	}

	turrets := [3][3]int64{
		{smallLaser, medLaser, largeLaser},
		{smallBlaster, medBlaster, largeBlaster},
		{smallAutoCannon, medAutoCannon, largeAutoCannon},
	}
	return turrets[weaponClass][size]
}

// npcTurretSize returns 0 (small), 1 (medium), or 2 (large) based on NPC groupID.
// NPC ship groups: frigates/destroyers = small, cruisers/BCs = medium, BS = large.
func npcTurretSize(groupID int64) int {
	switch groupID {
	// Battleships
	case 27, // Battleship (player)
		485, // Asteroid Angel Cartel Battleship
		486, // Asteroid Blood Raiders Battleship
		487, // Asteroid Guristas Battleship
		488, // Asteroid Sansha's Nation Battleship
		489, // Asteroid Serpentis Battleship
		490, // Deadspace Angel Cartel Battleship
		491, // Deadspace Blood Raiders Battleship
		492, // Deadspace Guristas Battleship
		493, // Deadspace Sansha's Nation Battleship
		494, // Deadspace Serpentis Battleship
		611, // Asteroid Rogue Drone Battleship
		755, // Asteroid Angel Cartel Commander Battleship
		756, // Asteroid Blood Raiders Commander Battleship
		757, // Asteroid Guristas Commander Battleship
		758, // Asteroid Sansha's Nation Commander Battleship
		759, // Asteroid Serpentis Commander Battleship
		30,  // Titan
		513, // Freighter
		547: // Carrier
		return 2
	// Cruisers, Battlecruisers
	case 26, // Cruiser (player)
		419, // Battlecruiser (player)
		474, // Asteroid Angel Cartel Cruiser
		475, // Asteroid Blood Raiders Cruiser
		476, // Asteroid Guristas Cruiser
		477, // Asteroid Sansha's Nation Cruiser
		478, // Asteroid Serpentis Cruiser
		479, // Deadspace Angel Cartel Cruiser
		480, // Deadspace Blood Raiders Cruiser
		481, // Deadspace Guristas Cruiser
		482, // Deadspace Sansha's Nation Cruiser
		483, // Deadspace Serpentis Cruiser
		595, // Asteroid Rogue Drone Cruiser
		752, // Asteroid Angel Cartel Commander Cruiser
		753, // Asteroid Blood Raiders Commander Cruiser
		754, // Asteroid Guristas Commander Cruiser
		790, // Asteroid Sansha's Nation Commander Cruiser
		791: // Asteroid Serpentis Commander Cruiser
		return 1
	default:
		// Frigates, destroyers, and anything unrecognized → small.
		return 0
	}
}

// GetNPCBounty returns the bounty value (attr 481) for an NPC typeID.
// Returns 0 if no bounty is defined.
func GetNPCBounty(ctx context.Context, db *sql.DB, typeID int64) float64 {
	attrs, err := GetTypeAttributes(ctx, db, typeID)
	if err != nil {
		return 0
	}
	return attrs[481] * rates.NPCBounty
}

// secBonusAttrID is entitySecurityStatusKillBonus: "How much security status is
// modified by for killing this entity. Depending on the entity, this may be a
// positive or negative amount."
const secBonusAttrID = 252

// secBonusCache memoises attribute 252 per NPC type for the process (dgmTypeAttributes is
// static), because it is read per rat death. It caches absence too: see GetNPCSecurityBonus.
var secBonusCache syncx.Map[int64, secBonusEntry] // typeID int64 -> secBonusEntry

type secBonusEntry struct {
	bonus   float64
	present bool
}

// InvalidateNPCSecurityBonus drops the cached attribute 252 for the given types (all when none
// are given). Every writer of attribute 252 calls it.
func InvalidateNPCSecurityBonus(typeIDs ...int64) {
	if len(typeIDs) == 0 {
		secBonusCache.Clear()
		return
	}
	for _, id := range typeIDs {
		secBonusCache.Delete(id)
	}
}

// GetNPCSecurityBonus returns dgmattribute 252 for an NPC typeID: the fraction of the killer's
// remaining gap to +5.0 that a kill is worth. present is false for a type without the attribute.
// An explicit zero (worth nothing) must differ from absence (flat fallback), so it queries the
// row directly: GetTypeAttributes' COALESCE folds the two.
func GetNPCSecurityBonus(ctx context.Context, db *sql.DB, typeID int64) (float64, bool) {
	if typeID <= 0 {
		return 0, false
	}
	if v, ok := secBonusCache.Load(typeID); ok {
		e := v
		return e.bonus, e.present
	}
	var bonus sql.NullFloat64
	err := db.QueryRowContext(ctx,
		`SELECT COALESCE(valueFloat, CAST(valueInt AS DOUBLE))
		   FROM dgmtypeattributes WHERE typeID = ? AND attributeID = ?`,
		typeID, secBonusAttrID).Scan(&bonus)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		secBonusCache.Store(typeID, secBonusEntry{})
		return 0, false
	case err != nil:
		// Do not cache a transient failure as an absence: one dropped connection would pin the rat
		// type to the fallback for the process.
		return 0, false
	}
	e := secBonusEntry{bonus: bonus.Float64, present: bonus.Valid}
	secBonusCache.Store(typeID, e)
	return e.bonus, e.present
}

// UpdateEntityLocation updates the locationID of an entity row, moving a ship to the
// station the character just docked at.
func UpdateEntityLocation(ctx context.Context, db *sql.DB, entityID, locationID int64) error {
	_, err := db.ExecContext(ctx,
		`UPDATE entity SET locationID=? WHERE itemID=?`, locationID, entityID)
	if err != nil {
		return fmt.Errorf("UpdateEntityLocation entityID=%d: %w", entityID, err)
	}
	return nil
}

// UpdateCharacterStation updates the stationID and location hierarchy on the character_
// row after docking.
func UpdateCharacterStation(ctx context.Context, db *sql.DB, charID, stationID, solarSystemID, constellationID, regionID int64) error {
	_, err := db.ExecContext(ctx,
		`UPDATE character_ SET stationID=?, solarSystemID=?, constellationID=?, regionID=? WHERE characterID=?`,
		stationID, solarSystemID, constellationID, regionID, charID)
	if err != nil {
		return fmt.Errorf("UpdateCharacterStation charID=%d: %w", charID, err)
	}
	return nil
}

// ClearCharacterStation sets stationID=0 (the in-space marker) and updates the solar-system
// hierarchy, at undock and at disconnect in space, so the next login spawns the character in
// their last solar system instead of back at the station.
func ClearCharacterStation(ctx context.Context, db *sql.DB, charID, solarSystemID, constellationID, regionID int64) error {
	_, err := db.ExecContext(ctx,
		`UPDATE character_ SET stationID=0, solarSystemID=?, constellationID=?, regionID=? WHERE characterID=?`,
		solarSystemID, constellationID, regionID, charID)
	if err != nil {
		return fmt.Errorf("ClearCharacterStation charID=%d: %w", charID, err)
	}
	return nil
}

// TeleportTargetKind identifies what sort of entity a /tr destination ID refers to.
type TeleportTargetKind int

const (
	TeleportTargetUnknown TeleportTargetKind = iota
	TeleportTargetSystem
	TeleportTargetStation
	TeleportTargetCelestial // anything in mapDenormalize (planet, moon, belt, gate, sun, …)
	TeleportTargetCharacter // a pilot (character row); the destination is the pilot's ship position
)

// TeleportTarget describes a /tr destination after resolving its ID.
type TeleportTarget struct {
	Kind            TeleportTargetKind
	ID              int64
	Name            string
	SolarSystemID   int64
	ConstellationID int64
	RegionID        int64
	X               float64
	Y               float64
	Z               float64
	Radius          float64 // celestial radius (0 for systems/stations)
	// ShipID is the live ship itemID for a Character target in space (0 when docked or unknown).
	// Prefer the live destiny manager's position: the DB x/y/z is written on warp completion and
	// is stale or zero mid-flight.
	ShipID int64
}

// ClassifyTeleportTarget resolves destID by probing mapSolarSystems → staStations →
// mapDenormalize. The first match wins. Returns TeleportTargetUnknown if none match.
func ClassifyTeleportTarget(ctx context.Context, db *sql.DB, destID int64) (*TeleportTarget, error) {
	// 1. Solar system?
	{
		t := &TeleportTarget{ID: destID, Kind: TeleportTargetSystem}
		err := db.QueryRowContext(ctx, `
			SELECT solarSystemName, constellationID, regionID
			  FROM mapSolarSystems WHERE solarSystemID = ?`, destID,
		).Scan(&t.Name, &t.ConstellationID, &t.RegionID)
		if err == nil {
			t.SolarSystemID = destID
			return t, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("ClassifyTeleportTarget system: %w", err)
		}
	}
	// 2. Station?
	{
		t := &TeleportTarget{ID: destID, Kind: TeleportTargetStation}
		err := db.QueryRowContext(ctx, `
			SELECT stationName, solarSystemID, constellationID, regionID,
			       COALESCE(x,0), COALESCE(y,0), COALESCE(z,0)
			  FROM staStations WHERE stationID = ?`, destID,
		).Scan(&t.Name, &t.SolarSystemID, &t.ConstellationID, &t.RegionID, &t.X, &t.Y, &t.Z)
		if err == nil {
			return t, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("ClassifyTeleportTarget station: %w", err)
		}
	}
	// 3. Celestial?
	{
		t := &TeleportTarget{ID: destID, Kind: TeleportTargetCelestial}
		var sysID int64
		err := db.QueryRowContext(ctx, `
			SELECT COALESCE(itemName,''),
			       solarSystemID,
			       COALESCE(x,0), COALESCE(y,0), COALESCE(z,0),
			       COALESCE(radius,1)
			  FROM mapDenormalize WHERE itemID = ?`, destID,
		).Scan(&t.Name, &sysID, &t.X, &t.Y, &t.Z, &t.Radius)
		if err == nil {
			t.SolarSystemID = sysID
			if sysID != 0 {
				cid, rid, locErr := GetSolarSystemLocation(ctx, db, sysID)
				if locErr == nil {
					t.ConstellationID = cid
					t.RegionID = rid
				}
			}
			return t, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("ClassifyTeleportTarget celestial: %w", err)
		}
	}
	// 4. Character?
	{
		t := &TeleportTarget{ID: destID, Kind: TeleportTargetCharacter}
		var stationID, sysID, cstID, regID int64
		err := db.QueryRowContext(ctx, `
			SELECT characterName, COALESCE(stationID,0), COALESCE(solarSystemID,0),
			       COALESCE(constellationID,0), COALESCE(regionID,0)
			  FROM character_ WHERE characterID = ?`, destID,
		).Scan(&t.Name, &stationID, &sysID, &cstID, &regID)
		if err == nil {
			t.SolarSystemID = sysID
			t.ConstellationID = cstID
			t.RegionID = regID
			// entity.locationID of a character is the station when docked and the ship itemID in
			// space; /tr lands next to the ship.
			if stationID == 0 {
				var shipID int64
				if err := db.QueryRowContext(ctx,
					`SELECT locationID FROM entity WHERE itemID = ?`, destID,
				).Scan(&shipID); err == nil && shipID > 0 {
					var x, y, z float64
					if err := db.QueryRowContext(ctx,
						`SELECT COALESCE(x,0), COALESCE(y,0), COALESCE(z,0)
						   FROM entity WHERE itemID = ?`, shipID,
					).Scan(&x, &y, &z); err == nil {
						t.X, t.Y, t.Z = x, y, z
					}
				}
			} else {
				// Docked: use the station coordinates, so /tr lands at the undock point.
				_ = db.QueryRowContext(ctx, `
					SELECT COALESCE(x,0), COALESCE(y,0), COALESCE(z,0)
					  FROM staStations WHERE stationID = ?`, stationID,
				).Scan(&t.X, &t.Y, &t.Z)
			}
			return t, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("ClassifyTeleportTarget character: %w", err)
		}
	}
	return &TeleportTarget{Kind: TeleportTargetUnknown, ID: destID}, nil
}

// GetStargateDestination returns the position, solar system, constellation and region IDs of
// the stargate gateID, the landing reference for StargateJump.
func GetStargateDestination(ctx context.Context, db *sql.DB, gateID int64) (x, y, z float64, solarSystemID, constellationID, regionID int64, err error) {
	err = db.QueryRowContext(ctx, `
		SELECT COALESCE(x,0), COALESCE(y,0), COALESCE(z,0),
		       solarSystemID,
		       COALESCE(constellationID,0),
		       COALESCE(regionID,0)
		  FROM mapDenormalize
		 WHERE itemID = ?`, gateID,
	).Scan(&x, &y, &z, &solarSystemID, &constellationID, &regionID)
	if errors.Is(err, sql.ErrNoRows) {
		err = fmt.Errorf("GetStargateDestination: gate %d not found", gateID)
	} else if err != nil {
		err = fmt.Errorf("GetStargateDestination: %w", err)
	}
	return
}

// NPCAIRanges are the movement and engagement ranges of an NPC type, read off the type for an
// NPC with no weapon profile (GetNPCWeaponProfile returns nil without damage, and carries these
// ranges only for armed types).
type NPCAIRanges struct {
	FlyRange     float64 // attr 416 entityFlyRange, the orbit distance
	AttackRange  float64 // attr 247 entityAttackRange
	ChaseMaxDist float64 // attr 665 entityChaseMaxDistance
}

// GetNPCAIRanges reads an NPC type's AI ranges from the cached type attributes.
func GetNPCAIRanges(ctx context.Context, db *sql.DB, typeID int64) (NPCAIRanges, error) {
	attrs, err := GetTypeAttributes(ctx, db, typeID)
	if err != nil {
		return NPCAIRanges{}, fmt.Errorf("GetNPCAIRanges typeID=%d: %w", typeID, err)
	}
	return NPCAIRanges{FlyRange: attrs[416], AttackRange: attrs[247], ChaseMaxDist: attrs[665]}, nil
}
