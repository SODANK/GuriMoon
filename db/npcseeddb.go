package db

// npcseeddb.go is the authoring layer for custom NPC types. An NPC ball is built at spawn from
// the invTypes row and the type's dgmTypeAttributes rows, read by GetNPCStats,
// GetEntityHPByType and GetNPCWeaponProfile. NPCTypeSpec is the write side for all three.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// NPCTypeSpec is the complete authorable definition of an NPC type: the invTypes row plus every
// dgmTypeAttributes value the server reads when it spawns and runs the entity.
type NPCTypeSpec struct {
	// ── invTypes row ────────────────────────────────────────────────────
	TypeID      int64
	TypeName    string
	GroupID     int64 // must be an invGroups row with categoryID = 11
	GraphicID   int64 // -> eveGraphics.url3D, the model the client loads
	RaceID      int64
	Description string
	Mass        float64 // kg
	Radius      float64 // m, collision and orbit geometry
	Volume      float64
	Capacity    float64
	Published   bool

	// ── Movement (attrs 508, 37, 70) ────────────────────────────────────
	// CruiseSpeed (508) is what the entity flies at; MaxVelocity (37) is the burst/chase stat and
	// only a fallback. See GetNPCStats.
	CruiseSpeed float64
	MaxVelocity float64
	Agility     float64

	// ── Tank (attrs 263, 265, 9, 479 + resonances) ──────────────────────
	ShieldHP         float64
	ArmorHP          float64
	StructureHP      float64
	ShieldRechargeMs float64
	// Resonances are multipliers in [0,1]: 1.0 = no resistance, 0.0 = immune.
	// Order is [EM, Explosive, Kinetic, Thermal].
	ShieldRes [4]float64
	ArmorRes  [4]float64
	HullRes   [4]float64

	// ── Weapon ──────────────────────────────────────────────────────────
	// WeaponKind is "turret", "missile" or "none". A missile NPC has attr 507 set; any other NPC
	// with damage is a turret NPC.
	WeaponKind           string
	DamageEM             float64
	DamageExp            float64
	DamageKin            float64
	DamageTherm          float64
	Multiplier           float64 // attr 64
	OptimalRange         float64 // attr 54
	Falloff              float64 // attr 158
	CycleTimeMs          float64 // attr 51
	TrackingSpeed        float64 // attr 160
	TurretSlots          float64 // attr 102, visible turret hardpoints
	WeaponSigRes         float64 // attr 620 optimalSigRadius (0 = derive by hull size)
	MissileTypeID        int64   // attr 507
	MissileMult          float64 // attr 212, used when damage lives on the missile type
	TurretTypeIDOverride int64   // attr 10001, explicit turret typeID that skips auto-derivation

	// ── AI ranges (attrs 552, 416, 247, 665, 475/476) ───────────────────
	SigRadius        float64
	FlyRange         float64
	AttackRange      float64
	ChaseMaxDist     float64
	AttackDelayMinMs float64
	AttackDelayMaxMs float64

	// ── Electronic warfare (npcEwarAttrSets) ────────────────────────────
	// Indexed in npcEwarAttrSets order. A kind with Chance 0 is off and its rows are deleted.
	Ewar [NPCEwarKindCount]NPCEwarSpec

	// ── Sensors (attrs 208-211) ─────────────────────────────────────────
	// One sensor type carries the strength. Player and drone ECM roll against it; with no
	// strength at all every ECM cycle jams the NPC.
	SensorAttr     int64 // 208 radar, 209 ladar, 210 magnetometric, 211 gravimetric
	SensorStrength float64

	// ── Reward (attrs 481, 252) ─────────────────────────────────────────
	// Bounty is the base before the config rate multiplier is applied.
	Bounty float64
	// SecStatusBonus is the fraction of the killer's gap to +5.0 a kill is worth. When
	// HasSecStatusBonus is false the row is absent and the flat fallback setting applies; an
	// explicit 0 means the kill is worth nothing (GetNPCSecurityBonus).
	SecStatusBonus    float64
	HasSecStatusBonus bool

	// CloneFromTypeID, on create, copies every attribute row of that type the spec does not
	// manage, so a clone keeps attributes the form has no field for.
	CloneFromTypeID int64
}

// NPCEwarSpec is one electronic-warfare kind on an NPC type, in attribute units.
type NPCEwarSpec struct {
	Chance     float64 // 0-1 per cycle; 0 = kind off
	DurationMs float64
	Range      float64 // m, optimal
	Falloff    float64 // m
	Effect     float64 // the kind's multiplier, amount or strength attribute (NPCEwarKind)
}

// Sensor strength attribute IDs, in the order the form lists them.
const (
	SensorRadar         int64 = 208
	SensorLadar         int64 = 209
	SensorMagnetometric int64 = 210
	SensorGravimetric   int64 = 211
)

// SensorAttrForRace returns the sensor type a race's hulls carry: Amarr radar, Minmatar ladar,
// Gallente magnetometric, Caldari and everyone else gravimetric.
func SensorAttrForRace(raceID int64) int64 {
	switch raceID {
	case 4:
		return SensorRadar
	case 2:
		return SensorLadar
	case 8:
		return SensorMagnetometric
	}
	return SensorGravimetric
}

// Attribute IDs this authoring layer owns. Saving a spec rewrites exactly these rows. A managed
// attribute whose spec value is zero has its row deleted, so the dogma default applies.
var npcSpecAttrIDs = func() []int64 {
	ids := []int64{
		9, 37, 51, 54, 64, 70, 102,
		109, 110, 111, 113, 114, 116, 117, 118,
		158, 160, 212, 247, 263, 265,
		267, 268, 269, 270, 271, 272, 273, 274,
		416, 475, 476, 479, 481, 507, 508, 552, 620, 665,
		10001,
		SensorRadar, SensorLadar, SensorMagnetometric, SensorGravimetric,
		secBonusAttrID,
	}
	for _, set := range npcEwarAttrSets {
		ids = append(ids, set.attrIDs()...)
	}
	return ids
}()

// attributes flattens the spec into the dgmTypeAttributes rows to write.
func (s *NPCTypeSpec) attributes() map[int64]float64 {
	a := map[int64]float64{
		// Movement.
		508: s.CruiseSpeed,
		37:  s.MaxVelocity,
		70:  s.Agility,

		// Tank.
		263: s.ShieldHP,
		265: s.ArmorHP,
		9:   s.StructureHP,
		479: s.ShieldRechargeMs,

		271: s.ShieldRes[0], 272: s.ShieldRes[1], 273: s.ShieldRes[2], 274: s.ShieldRes[3],
		267: s.ArmorRes[0], 268: s.ArmorRes[1], 269: s.ArmorRes[2], 270: s.ArmorRes[3],
		113: s.HullRes[0], 111: s.HullRes[1], 109: s.HullRes[2], 110: s.HullRes[3],

		// AI ranges.
		552: s.SigRadius,
		416: s.FlyRange,
		247: s.AttackRange,
		665: s.ChaseMaxDist,
		475: s.AttackDelayMinMs,
		476: s.AttackDelayMaxMs,

		// Reward.
		481: s.Bounty,
	}

	if s.WeaponKind != "none" {
		a[114] = s.DamageEM
		a[116] = s.DamageExp
		a[117] = s.DamageKin
		a[118] = s.DamageTherm
		a[64] = s.Multiplier
		a[54] = s.OptimalRange
		a[158] = s.Falloff
		a[51] = s.CycleTimeMs
		a[620] = s.WeaponSigRes
		if s.WeaponKind == "missile" {
			a[507] = float64(s.MissileTypeID)
			a[212] = s.MissileMult
		} else {
			a[160] = s.TrackingSpeed
			a[102] = s.TurretSlots
			a[10001] = float64(s.TurretTypeIDOverride)
		}
	}

	for i, set := range npcEwarAttrSets {
		e := s.Ewar[i]
		if e.Chance <= 0 {
			continue
		}
		a[set.chance] = e.Chance
		if set.duration != 0 {
			a[set.duration] = e.DurationMs
		}
		if set.maxRange != 0 {
			a[set.maxRange] = e.Range
		}
		if set.falloff != 0 {
			a[set.falloff] = e.Falloff
		}
		if id := set.effectAttr(); id != 0 {
			a[id] = e.Effect
		}
	}

	if s.SensorAttr >= SensorRadar && s.SensorAttr <= SensorGravimetric {
		a[s.SensorAttr] = s.SensorStrength
	}
	if s.HasSecStatusBonus {
		a[secBonusAttrID] = s.SecStatusBonus
	}
	return a
}

// keepsZero reports whether a managed attribute is written even at 0. Only attr 252 is: an
// explicit 0 there differs from absence.
func (s *NPCTypeSpec) keepsZero(attrID int64) bool {
	return attrID == secBonusAttrID && s.HasSecStatusBonus
}

// ESPLoadNPCSpec reads an existing type back into an editable spec.
func ESPLoadNPCSpec(ctx context.Context, db *sql.DB, typeID int64) (*NPCTypeSpec, error) {
	s := &NPCTypeSpec{TypeID: typeID}
	var published int64
	err := db.QueryRowContext(ctx, `
		SELECT t.typeName, t.groupID, COALESCE(t.graphicID, 0), COALESCE(t.raceID, 0),
		       COALESCE(t.description, ''), COALESCE(t.mass, 0), COALESCE(t.radius, 0),
		       COALESCE(t.volume, 0), COALESCE(t.capacity, 0), COALESCE(t.published, 0)
		  FROM invTypes t WHERE t.typeID = ?`, typeID,
	).Scan(&s.TypeName, &s.GroupID, &s.GraphicID, &s.RaceID, &s.Description,
		&s.Mass, &s.Radius, &s.Volume, &s.Capacity, &published)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("NPC type %d not found", typeID)
		}
		return nil, fmt.Errorf("ESPLoadNPCSpec: %w", err)
	}
	s.Published = published != 0

	attrs, err := GetTypeAttributes(ctx, db, typeID)
	if err != nil {
		return nil, fmt.Errorf("ESPLoadNPCSpec attributes: %w", err)
	}

	s.CruiseSpeed = attrs[508]
	s.MaxVelocity = attrs[37]
	s.Agility = attrs[70]

	s.ShieldHP = attrs[263]
	s.ArmorHP = attrs[265]
	s.StructureHP = attrs[9]
	s.ShieldRechargeMs = attrs[479]
	s.ShieldRes = [4]float64{
		attrOrDefault(attrs, 271, 1), attrOrDefault(attrs, 272, 1),
		attrOrDefault(attrs, 273, 1), attrOrDefault(attrs, 274, 1)}
	s.ArmorRes = [4]float64{
		attrOrDefault(attrs, 267, 1), attrOrDefault(attrs, 268, 1),
		attrOrDefault(attrs, 269, 1), attrOrDefault(attrs, 270, 1)}
	s.HullRes = [4]float64{
		attrOrDefault(attrs, 113, 1), attrOrDefault(attrs, 111, 1),
		attrOrDefault(attrs, 109, 1), attrOrDefault(attrs, 110, 1)}

	s.DamageEM = attrs[114]
	s.DamageExp = attrs[116]
	s.DamageKin = attrs[117]
	s.DamageTherm = attrs[118]
	s.Multiplier = attrs[64]
	s.OptimalRange = attrs[54]
	s.Falloff = attrs[158]
	s.CycleTimeMs = attrs[51]
	if s.CycleTimeMs == 0 {
		s.CycleTimeMs = attrs[73] // some SDE entities carry duration instead
	}
	s.TrackingSpeed = attrs[160]
	s.TurretSlots = attrs[102]
	s.WeaponSigRes = attrs[620]
	s.MissileTypeID = int64(attrs[507])
	s.MissileMult = attrs[212]
	s.TurretTypeIDOverride = int64(attrs[10001])

	switch {
	case s.MissileTypeID != 0:
		s.WeaponKind = "missile"
	case s.DamageEM+s.DamageExp+s.DamageKin+s.DamageTherm > 0:
		s.WeaponKind = "turret"
	default:
		s.WeaponKind = "none"
	}

	s.SigRadius = attrs[552]
	s.FlyRange = attrs[416]
	s.AttackRange = attrs[247]
	s.ChaseMaxDist = attrs[665]
	s.AttackDelayMinMs = attrs[475]
	s.AttackDelayMaxMs = attrs[476]
	s.Bounty = attrs[481]

	for i, set := range npcEwarAttrSets {
		chance := attrs[set.chance]
		if chance <= 0 {
			continue
		}
		// Same unit fix as GetNPCEwarProfile: some SDE rows hold a whole percent.
		if chance >= 10 {
			chance /= 100
		}
		e := NPCEwarSpec{Chance: chance}
		if set.duration != 0 {
			e.DurationMs = attrs[set.duration]
		}
		if set.maxRange != 0 {
			e.Range = attrs[set.maxRange]
		}
		if set.falloff != 0 {
			e.Falloff = attrs[set.falloff]
		}
		if id := set.effectAttr(); id != 0 {
			e.Effect = attrs[id]
		}
		s.Ewar[i] = e
	}

	// The strongest sensor type wins; a type with none gets its race's type at strength 0.
	s.SensorAttr = SensorAttrForRace(s.RaceID)
	for _, id := range []int64{SensorRadar, SensorLadar, SensorMagnetometric, SensorGravimetric} {
		if v := attrs[id]; v > s.SensorStrength {
			s.SensorAttr, s.SensorStrength = id, v
		}
	}

	if bonus, ok := GetNPCSecurityBonus(ctx, db, typeID); ok {
		s.SecStatusBonus, s.HasSecStatusBonus = bonus, true
	}

	return s, nil
}

// attrIDs lists every attribute an EWAR kind reads.
func (set ewarAttrSet) attrIDs() []int64 {
	var ids []int64
	for _, id := range []int64{set.chance, set.duration, set.maxRange, set.falloff,
		set.multiplier, set.amount, set.strength} {
		if id != 0 {
			ids = append(ids, id)
		}
	}
	return ids
}

// effectAttr is the kind's one effect-size attribute: multiplier, drain amount or scram strength.
// 0 for ECM, whose effect is the jam itself.
func (set ewarAttrSet) effectAttr() int64 {
	switch {
	case set.multiplier != 0:
		return set.multiplier
	case set.amount != 0:
		return set.amount
	}
	return set.strength
}

// ESPSaveNPCSpec creates or updates an NPC type and its managed attributes in one transaction.
// A zero spec.TypeID allocates a new ID in the custom band (>= 100000) and the typeID written
// is returned. A spec that fails validation returns NPCSpecErrors listing every problem.
func ESPSaveNPCSpec(ctx context.Context, db *sql.DB, s *NPCTypeSpec) (int64, error) {
	problems := s.Validate()
	problems = append(problems, validateNPCSpecRefs(ctx, db, s)...)
	if len(problems) > 0 {
		return 0, problems
	}

	// Two creates at once can both read the same MAX(typeID); the loser retries on the
	// duplicate key with a fresh allocation.
	for attempt := 0; ; attempt++ {
		typeID, err := saveNPCSpecTx(ctx, db, s)
		if err != nil && s.TypeID == 0 && attempt < 2 && isDuplicateKeyErr(err) {
			continue
		}
		if err != nil {
			return 0, err
		}
		// Drop the static caches for this type so the next combat or spawn read sees the edit.
		InvalidateTypeStatics(typeID)
		InvalidateNPCSecurityBonus(typeID)
		return typeID, nil
	}
}

func saveNPCSpecTx(ctx context.Context, db *sql.DB, s *NPCTypeSpec) (int64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("ESPSaveNPCSpec begin: %w", err)
	}
	defer tx.Rollback()

	published := int64(0)
	if s.Published {
		published = 1
	}

	typeID := s.TypeID
	if typeID == 0 {
		// The custom band starts above every SDE typeID, so an SDE re-import cannot collide with it.
		var maxID int64
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(typeID), 100000) FROM invTypes WHERE typeID >= 100000`,
		).Scan(&maxID); err != nil {
			return 0, fmt.Errorf("ESPSaveNPCSpec alloc: %w", err)
		}
		typeID = maxID + 1
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO invTypes (typeID, typeName, groupID, graphicID, raceID,
			                      description, mass, radius, volume, capacity, published)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			typeID, s.TypeName, s.GroupID, s.GraphicID, s.RaceID,
			s.Description, s.Mass, s.Radius, s.Volume, s.Capacity, published); err != nil {
			return 0, fmt.Errorf("ESPSaveNPCSpec insert: %w", err)
		}
		if s.CloneFromTypeID > 0 {
			// Copy what the form has no field for. The managed set is written below.
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO dgmTypeAttributes (typeID, attributeID, valueInt, valueFloat)
				SELECT ?, attributeID, valueInt, valueFloat
				  FROM dgmTypeAttributes
				 WHERE typeID = ? AND attributeID NOT IN (`+npcSpecAttrIDList+`)`,
				typeID, s.CloneFromTypeID); err != nil {
				return 0, fmt.Errorf("ESPSaveNPCSpec clone attrs: %w", err)
			}
		}
	} else {
		if _, err := tx.ExecContext(ctx, `
			UPDATE invTypes SET typeName=?, groupID=?, graphicID=?, raceID=?,
			       description=?, mass=?, radius=?, volume=?, capacity=?, published=?
			 WHERE typeID=?`,
			s.TypeName, s.GroupID, s.GraphicID, s.RaceID,
			s.Description, s.Mass, s.Radius, s.Volume, s.Capacity, published,
			typeID); err != nil {
			return 0, fmt.Errorf("ESPSaveNPCSpec update: %w", err)
		}
	}

	// Rewrite the managed attribute set. valueInt is written NULL, not 0: the dgmtypeattribs
	// cachable reads IF(valueInt IS NULL, valueFloat, valueInt), so a 0 would ship as 0.
	want := s.attributes()
	upsert, err := tx.PrepareContext(ctx, `
		INSERT INTO dgmTypeAttributes (typeID, attributeID, valueInt, valueFloat)
		VALUES (?, ?, NULL, ?)
		ON DUPLICATE KEY UPDATE valueInt = NULL, valueFloat = VALUES(valueFloat)`)
	if err != nil {
		return 0, fmt.Errorf("ESPSaveNPCSpec prepare: %w", err)
	}
	defer upsert.Close()

	del, err := tx.PrepareContext(ctx,
		`DELETE FROM dgmTypeAttributes WHERE typeID = ? AND attributeID = ?`)
	if err != nil {
		return 0, fmt.Errorf("ESPSaveNPCSpec prepare delete: %w", err)
	}
	defer del.Close()

	for _, attrID := range npcSpecAttrIDs {
		v, managed := want[attrID]
		if !managed || (v == 0 && !s.keepsZero(attrID)) {
			if _, err := del.ExecContext(ctx, typeID, attrID); err != nil {
				return 0, fmt.Errorf("ESPSaveNPCSpec delete attr %d: %w", attrID, err)
			}
			continue
		}
		if _, err := upsert.ExecContext(ctx, typeID, attrID, v); err != nil {
			return 0, fmt.Errorf("ESPSaveNPCSpec attr %d: %w", attrID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("ESPSaveNPCSpec commit: %w", err)
	}
	return typeID, nil
}

// npcSpecAttrIDList is npcSpecAttrIDs as a SQL list, for the clone copy's NOT IN.
var npcSpecAttrIDList = func() string {
	parts := make([]string, len(npcSpecAttrIDs))
	for i, id := range npcSpecAttrIDs {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(parts, ",")
}()

// ESPListCustomNPCTypes returns every authored NPC type, those in the custom typeID band.
func ESPListCustomNPCTypes(ctx context.Context, db *sql.DB) ([]ESPNPCTypeRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT t.typeID, t.typeName, t.groupID, COALESCE(g.groupName, ''),
		       COALESCE(t.graphicID, 0), COALESCE(t.raceID, 0), COALESCE(t.published, 0),
		       COALESCE(eg.icon, ''),
		       COALESCE(b.valueFloat, b.valueInt, 0)
		  FROM invTypes t
		  LEFT JOIN invGroups g ON g.groupID = t.groupID
		  LEFT JOIN eveGraphics eg ON eg.graphicID = t.graphicID
		  LEFT JOIN dgmTypeAttributes b ON b.typeID = t.typeID AND b.attributeID = 481
		 WHERE t.typeID >= 100000
		 ORDER BY t.typeID DESC LIMIT 200`)
	if err != nil {
		return nil, fmt.Errorf("ESPListCustomNPCTypes: %w", err)
	}
	defer rows.Close()

	var out []ESPNPCTypeRow
	for rows.Next() {
		var r ESPNPCTypeRow
		if err := rows.Scan(&r.TypeID, &r.TypeName, &r.GroupID, &r.GroupName,
			&r.GraphicID, &r.RaceID, &r.Published, &r.Icon, &r.Bounty); err != nil {
			return nil, fmt.Errorf("ESPListCustomNPCTypes scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ── Hull archetypes ─────────────────────────────────────────────────────────

// NPCArchetype is a starting point for a new NPC type: every attribute the server reads, with
// values in the right order of magnitude for the hull class. Resonances start at 1.0.
type NPCArchetype struct {
	Key   string
	Label string
	Note  string
	Spec  NPCTypeSpec
}

func baseSpec(mass, radius, cruise, agility float64) NPCTypeSpec {
	return NPCTypeSpec{
		Mass: mass, Radius: radius, Volume: radius * 10, Capacity: 0,
		CruiseSpeed: cruise, MaxVelocity: cruise, Agility: agility,
		ShieldRes:  [4]float64{1, 1, 1, 1},
		ArmorRes:   [4]float64{1, 1, 1, 1},
		HullRes:    [4]float64{1, 1, 1, 1},
		Multiplier: 1, MissileMult: 1,
		SensorAttr: SensorGravimetric,
		Published:  true,
	}
}

// setDamage splits a total damage-per-cycle figure across the four damage types using a named
// flavour, so an archetype arrives with a resolvable turret model.
func setDamage(s *NPCTypeSpec, total float64, flavour string) {
	f, ok := NPCDamageFlavourByKey(flavour)
	if !ok {
		return
	}
	s.DamageEM = f.EM * total
	s.DamageExp = f.Exp * total
	s.DamageKin = f.Kin * total
	s.DamageTherm = f.Therm * total
}

// NPCArchetypes returns the built-in hull presets, in display order.
func NPCArchetypes() []NPCArchetype {
	frigate := baseSpec(1_200_000, 30, 240, 3.2)
	frigate.ShieldHP, frigate.ArmorHP, frigate.StructureHP = 200, 200, 300
	frigate.ShieldRechargeMs = 125_000
	frigate.WeaponKind = "turret"
	frigate.OptimalRange, frigate.Falloff = 5_000, 3_000
	frigate.CycleTimeMs, frigate.TrackingSpeed, frigate.TurretSlots = 3_000, 0.2, 2
	frigate.SigRadius, frigate.WeaponSigRes = 35, 40
	frigate.FlyRange, frigate.AttackRange, frigate.ChaseMaxDist = 3_000, 12_000, 30_000
	frigate.AttackDelayMaxMs = 3_000
	setDamage(&frigate, 24, "laser")
	frigate.SensorStrength = 8
	frigate.Bounty = 15_000

	destroyer := baseSpec(1_800_000, 55, 215, 3.8)
	destroyer.ShieldHP, destroyer.ArmorHP, destroyer.StructureHP = 400, 450, 600
	destroyer.ShieldRechargeMs = 150_000
	destroyer.WeaponKind = "turret"
	destroyer.OptimalRange, destroyer.Falloff = 7_000, 5_000
	destroyer.CycleTimeMs, destroyer.TrackingSpeed, destroyer.TurretSlots = 3_500, 0.15, 3
	destroyer.SigRadius, destroyer.WeaponSigRes = 65, 40
	destroyer.FlyRange, destroyer.AttackRange, destroyer.ChaseMaxDist = 5_000, 18_000, 45_000
	destroyer.AttackDelayMaxMs = 3_000
	setDamage(&destroyer, 40, "autocannon")
	destroyer.SensorStrength = 10
	destroyer.Bounty = 45_000

	cruiser := baseSpec(12_000_000, 100, 180, 4.5)
	cruiser.ShieldHP, cruiser.ArmorHP, cruiser.StructureHP = 800, 900, 1_200
	cruiser.ShieldRechargeMs = 250_000
	cruiser.WeaponKind = "turret"
	cruiser.OptimalRange, cruiser.Falloff = 12_000, 8_000
	cruiser.CycleTimeMs, cruiser.TrackingSpeed, cruiser.TurretSlots = 4_000, 0.08, 3
	cruiser.SigRadius, cruiser.WeaponSigRes = 125, 125
	cruiser.FlyRange, cruiser.AttackRange, cruiser.ChaseMaxDist = 8_000, 25_000, 60_000
	cruiser.AttackDelayMaxMs = 4_000
	setDamage(&cruiser, 70, "laser")
	cruiser.SensorStrength = 14
	cruiser.Bounty = 200_000

	battlecruiser := baseSpec(35_000_000, 180, 150, 6)
	battlecruiser.ShieldHP, battlecruiser.ArmorHP, battlecruiser.StructureHP = 1_800, 2_200, 2_500
	battlecruiser.ShieldRechargeMs = 400_000
	battlecruiser.WeaponKind = "turret"
	battlecruiser.OptimalRange, battlecruiser.Falloff = 18_000, 11_000
	battlecruiser.CycleTimeMs, battlecruiser.TrackingSpeed, battlecruiser.TurretSlots = 5_000, 0.04, 4
	battlecruiser.SigRadius, battlecruiser.WeaponSigRes = 250, 125
	battlecruiser.FlyRange, battlecruiser.AttackRange, battlecruiser.ChaseMaxDist = 12_000, 40_000, 90_000
	battlecruiser.AttackDelayMaxMs = 4_000
	setDamage(&battlecruiser, 140, "blaster")
	battlecruiser.SensorStrength = 17
	battlecruiser.Bounty = 600_000

	battleship := baseSpec(100_000_000, 300, 120, 8)
	battleship.ShieldHP, battleship.ArmorHP, battleship.StructureHP = 4_000, 4_500, 5_000
	battleship.ShieldRechargeMs = 600_000
	battleship.WeaponKind = "turret"
	battleship.OptimalRange, battleship.Falloff = 25_000, 15_000
	battleship.CycleTimeMs, battleship.TrackingSpeed, battleship.TurretSlots = 6_000, 0.02, 4
	battleship.SigRadius, battleship.WeaponSigRes = 400, 400
	battleship.FlyRange, battleship.AttackRange, battleship.ChaseMaxDist = 15_000, 60_000, 150_000
	battleship.AttackDelayMaxMs = 5_000
	setDamage(&battleship, 240, "laser")
	battleship.SensorStrength = 20
	battleship.Bounty = 1_500_000

	hauler := baseSpec(20_000_000, 120, 150, 6)
	hauler.ShieldHP, hauler.ArmorHP, hauler.StructureHP = 500, 500, 900
	hauler.ShieldRechargeMs = 300_000
	hauler.WeaponKind = "none"
	hauler.SigRadius = 150
	hauler.FlyRange, hauler.AttackRange, hauler.ChaseMaxDist = 5_000, 10_000, 20_000
	hauler.SensorStrength = 10
	hauler.Bounty = 50_000

	sentry := baseSpec(2_000_000, 60, 0, 1)
	sentry.ShieldHP, sentry.ArmorHP, sentry.StructureHP = 1_000, 1_500, 2_000
	sentry.ShieldRechargeMs = 400_000
	sentry.WeaponKind = "turret"
	sentry.OptimalRange, sentry.Falloff = 30_000, 15_000
	sentry.CycleTimeMs, sentry.TrackingSpeed, sentry.TurretSlots = 5_000, 0.03, 2
	sentry.SigRadius, sentry.WeaponSigRes = 200, 125
	sentry.FlyRange, sentry.AttackRange, sentry.ChaseMaxDist = 0, 60_000, 60_000
	setDamage(&sentry, 120, "railgun")
	sentry.SensorStrength = 20
	sentry.Bounty = 100_000

	dreadnought := baseSpec(1_200_000_000, 1500, 60, 12)
	dreadnought.ShieldHP, dreadnought.ArmorHP, dreadnought.StructureHP = 10_000, 12_000, 15_000
	dreadnought.ShieldRechargeMs = 2_000_000
	dreadnought.WeaponKind = "turret"
	dreadnought.OptimalRange, dreadnought.Falloff = 40_000, 20_000
	dreadnought.CycleTimeMs, dreadnought.TrackingSpeed, dreadnought.TurretSlots = 8_000, 0.008, 3
	dreadnought.SigRadius, dreadnought.WeaponSigRes = 3200, 400
	dreadnought.FlyRange, dreadnought.AttackRange, dreadnought.ChaseMaxDist = 20_000, 80_000, 250_000
	dreadnought.AttackDelayMaxMs = 6_000
	setDamage(&dreadnought, 500, "laser")
	dreadnought.SensorStrength = 40
	dreadnought.Bounty = 5_000_000

	return []NPCArchetype{
		{"frigate", "Frigate", "Fast, short range, low HP.", frigate},
		{"destroyer", "Destroyer", "Frigate speed with more guns.", destroyer},
		{"cruiser", "Cruiser", "Mid-range workhorse.", cruiser},
		{"battlecruiser", "Battlecruiser", "Heavy sub-capital brawler.", battlecruiser},
		{"battleship", "Battleship", "Slow, long range, high HP.", battleship},
		{"dreadnought", "Dreadnought", "Capital-class. Very high HP, slow, long range, massive damage.", dreadnought},
		{"hauler", "Hauler (unarmed)", "No weapon profile — spawns as a passive target.", hauler},
		{"sentry", "Sentry (immobile)", "Cruise speed 0. The dungeon spawner treats maxVelocity=0 entities as rigid balls with no AI — give it a non-zero cruise speed if it must fight.", sentry},
	}
}

// NPCArchetypeByKey returns a copy of the named archetype spec.
func NPCArchetypeByKey(key string) (NPCTypeSpec, bool) {
	for _, a := range NPCArchetypes() {
		if a.Key == key {
			return a.Spec, true
		}
	}
	return NPCTypeSpec{}, false
}

// ── Damage flavours ─────────────────────────────────────────────────────────

// NPCDamageFlavour splits a total damage-per-cycle figure across the four damage types.
// npcTurretTypeFromDamage picks the turret model drawn on the NPC's hardpoints from the
// dominant pair.
type NPCDamageFlavour struct {
	Key    string
	Label  string
	Turret string // turret family the damage split resolves to
	EM     float64
	Exp    float64
	Kin    float64
	Therm  float64
}

// NPCDamageFlavours returns the built-in damage splits, in display order.
func NPCDamageFlavours() []NPCDamageFlavour {
	return []NPCDamageFlavour{
		{"laser", "Laser (Amarr / Blood / Sansha)", "Pulse laser", 0.6, 0, 0, 0.4},
		{"blaster", "Blaster (Gallente / Serpentis)", "Hybrid blaster", 0, 0, 0.5, 0.5},
		{"railgun", "Railgun (Caldari / Guristas)", "Hybrid blaster", 0, 0, 0.7, 0.3},
		{"autocannon", "Autocannon (Minmatar / Angel)", "Projectile autocannon", 0, 0.5, 0.5, 0},
		{"artillery", "Artillery (Minmatar / Angel)", "Projectile autocannon", 0, 0.6, 0.4, 0},
	}
}

// NPCDamageFlavourByKey returns the named split.
func NPCDamageFlavourByKey(key string) (NPCDamageFlavour, bool) {
	for _, f := range NPCDamageFlavours() {
		if f.Key == key {
			return f, true
		}
	}
	return NPCDamageFlavour{}, false
}

// ── Model catalogue ─────────────────────────────────────────────────────────

// ESPGraphicRow is one eveGraphics entry offered as a model choice: urlWeb holds the hull
// name, description the class, url3D the res: path of the model.
type ESPGraphicRow struct {
	GraphicID   int64
	Name        string // urlWeb
	Description string
	URL3D       string
	ExplosionID int64
	UsedBy      int64 // how many invTypes rows already use this graphic
	HasModel    bool  // a converted .glb was found on disk (filled by ESP)
}

// ESPListGraphics returns model-bearing eveGraphics rows. scope "ship" restricts to ship
// hulls; anything else returns every graphic with a url3D.
func ESPListGraphics(ctx context.Context, db *sql.DB, query, scope string) ([]ESPGraphicRow, error) {
	q := `
		SELECT g.graphicID, COALESCE(g.urlWeb, ''), COALESCE(g.description, ''),
		       COALESCE(g.url3D, ''), COALESCE(g.explosionID, 0),
		       (SELECT COUNT(*) FROM invTypes t WHERE t.graphicID = g.graphicID)
		  FROM eveGraphics g
		 WHERE g.url3D IS NOT NULL AND g.url3D <> ''`
	var args []any
	if scope == "ship" {
		q += ` AND g.url3D LIKE 'res:/Model/Ship/%'`
	}
	if query != "" {
		q += ` AND (g.urlWeb LIKE ? OR g.description LIKE ? OR g.url3D LIKE ?)`
		like := "%" + query + "%"
		args = append(args, like, like, like)
	}
	q += ` ORDER BY g.description, g.urlWeb LIMIT 600`

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("ESPListGraphics: %w", err)
	}
	defer rows.Close()

	var out []ESPGraphicRow
	for rows.Next() {
		var g ESPGraphicRow
		if err := rows.Scan(&g.GraphicID, &g.Name, &g.Description,
			&g.URL3D, &g.ExplosionID, &g.UsedBy); err != nil {
			return nil, fmt.Errorf("ESPListGraphics scan: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// ESPGetGraphic returns a single eveGraphics row for display next to a type.
func ESPGetGraphic(ctx context.Context, db *sql.DB, graphicID int64) (*ESPGraphicRow, error) {
	g := &ESPGraphicRow{GraphicID: graphicID}
	err := db.QueryRowContext(ctx, `
		SELECT COALESCE(urlWeb, ''), COALESCE(description, ''),
		       COALESCE(url3D, ''), COALESCE(explosionID, 0)
		  FROM eveGraphics WHERE graphicID = ?`, graphicID,
	).Scan(&g.Name, &g.Description, &g.URL3D, &g.ExplosionID)
	if err != nil {
		return nil, fmt.Errorf("ESPGetGraphic %d: %w", graphicID, err)
	}
	return g, nil
}

// ESPSearchMissileTypes finds charge types usable as an NPC's entity missile (attribute 507).
// Range is velocity x flight time off the charge, so only types carrying both are offered.
func ESPSearchMissileTypes(ctx context.Context, db *sql.DB, query string) ([]ESPItemRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT t.typeID, t.typeName, COALESCE(g.groupName, '')
		  FROM invTypes t
		  JOIN invGroups g ON g.groupID = t.groupID
		 WHERE g.categoryID = 8
		   AND t.typeName LIKE ?
		   AND EXISTS (SELECT 1 FROM dgmTypeAttributes a
		                WHERE a.typeID = t.typeID AND a.attributeID = 281)
		 ORDER BY t.typeName LIMIT 100`, "%"+query+"%")
	if err != nil {
		return nil, fmt.Errorf("ESPSearchMissileTypes: %w", err)
	}
	defer rows.Close()

	var out []ESPItemRow
	for rows.Next() {
		var r ESPItemRow
		if err := rows.Scan(&r.TypeID, &r.TypeName, &r.GroupName); err != nil {
			return nil, fmt.Errorf("ESPSearchMissileTypes scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
