package db

// npcseedcheck.go validates an NPCTypeSpec before ESPSaveNPCSpec writes it, describes the EWAR
// kinds for the Create NPC form, and reads the context panels the form shows (group loot, spawn
// listings). Rules are listed in docs/npc-authoring.md.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// NPCSpecProblem is one validation failure. Tab names the Create NPC tab holding the field, so
// the form can open it.
type NPCSpecProblem struct {
	Tab string
	Msg string
}

// NPCSpecErrors is every problem found in a spec. ESPSaveNPCSpec returns it as its error.
type NPCSpecErrors []NPCSpecProblem

func (e NPCSpecErrors) Error() string {
	msgs := make([]string, len(e))
	for i, p := range e {
		msgs[i] = p.Msg
	}
	return strings.Join(msgs, "; ")
}

// Validate checks the spec on its own, without the database. validateNPCSpecRefs checks the IDs
// it points at.
func (s *NPCTypeSpec) Validate() NPCSpecErrors {
	var out NPCSpecErrors
	add := func(tab, format string, a ...any) {
		out = append(out, NPCSpecProblem{Tab: tab, Msg: fmt.Sprintf(format, a...)})
	}
	nonNeg := func(tab, label string, v float64) {
		if v < 0 {
			add(tab, "%s cannot be negative.", label)
		}
	}

	if strings.TrimSpace(s.TypeName) == "" {
		add("identity", "Name is required.")
	}
	if s.GroupID <= 0 {
		add("identity", "Pick a group.")
	}
	if s.GraphicID <= 0 {
		add("model", "Pick a model, or the client draws nothing for this NPC.")
	}

	if s.Mass <= 0 {
		add("movement", "Mass must be above 0.")
	}
	if s.Radius <= 0 {
		add("movement", "Radius must be above 0.")
	}
	nonNeg("movement", "Volume", s.Volume)
	nonNeg("movement", "Capacity", s.Capacity)
	nonNeg("movement", "Cruise speed", s.CruiseSpeed)
	nonNeg("movement", "Max velocity", s.MaxVelocity)
	nonNeg("movement", "Agility", s.Agility)

	if s.StructureHP <= 0 {
		add("defence", "Structure HP must be above 0.")
	}
	nonNeg("defence", "Shield HP", s.ShieldHP)
	nonNeg("defence", "Armor HP", s.ArmorHP)
	nonNeg("defence", "Shield recharge", s.ShieldRechargeMs)
	for name, layer := range map[string][4]float64{"Shield": s.ShieldRes, "Armor": s.ArmorRes, "Hull": s.HullRes} {
		for _, r := range layer {
			if r <= 0 || r > 1 {
				add("defence", "%s resists must be between 0%% and 99.99%%.", name)
				break
			}
		}
	}

	switch s.WeaponKind {
	case "turret", "missile":
		for label, v := range map[string]float64{
			"Damage":    s.DamageEM + s.DamageExp + s.DamageKin + s.DamageTherm,
			"EM damage": s.DamageEM, "Explosive damage": s.DamageExp,
			"Kinetic damage": s.DamageKin, "Thermal damage": s.DamageTherm,
			"Damage multiplier": s.Multiplier, "Optimal range": s.OptimalRange,
			"Falloff": s.Falloff, "Cycle time": s.CycleTimeMs,
			"Tracking speed": s.TrackingSpeed, "Turret hardpoints": s.TurretSlots,
			"Weapon signature resolution": s.WeaponSigRes, "Missile damage multiplier": s.MissileMult,
		} {
			nonNeg("weapons", label, v)
		}
		if s.CycleTimeMs <= 0 {
			add("weapons", "Cycle time must be above 0.")
		}
		if s.WeaponKind == "turret" && s.DamageEM+s.DamageExp+s.DamageKin+s.DamageTherm <= 0 {
			add("weapons", "A turret NPC needs some damage, or it spawns unarmed. Enter a damage total or pick Unarmed.")
		}
		if s.WeaponKind == "missile" && s.MissileTypeID <= 0 {
			add("weapons", "A missile NPC needs a missile type.")
		}
	case "none":
	default:
		add("weapons", "Unknown weapon kind %q.", s.WeaponKind)
	}

	for i, e := range s.Ewar {
		if e.Chance == 0 {
			continue
		}
		k := npcEwarKinds[i]
		if e.Chance < 0 || e.Chance > 1 {
			add("ewar", "%s chance must be between 0%% and 100%%.", k.Label)
		}
		if k.HasDuration && e.DurationMs <= 0 {
			add("ewar", "%s needs a duration.", k.Label)
		}
		if k.Key != "scram" && e.Range <= 0 {
			add("ewar", "%s needs a range.", k.Label)
		}
		if e.Range < 0 || e.Falloff < 0 {
			add("ewar", "%s range and falloff cannot be negative.", k.Label)
		}
		if k.EffectLabel != "" {
			if v := k.EffectToForm(e.Effect); v <= 0 || (k.EffectMax > 0 && v > k.EffectMax) {
				if k.EffectMax > 0 {
					add("ewar", "%s %s must be above 0 and at most %g.", k.Label, strings.ToLower(k.EffectLabel), k.EffectMax)
				} else {
					add("ewar", "%s %s must be above 0.", k.Label, strings.ToLower(k.EffectLabel))
				}
			}
		}
	}

	nonNeg("sensors", "Signature radius", s.SigRadius)
	nonNeg("sensors", "Sensor strength", s.SensorStrength)
	if s.SensorAttr < SensorRadar || s.SensorAttr > SensorGravimetric {
		add("sensors", "Pick a sensor type.")
	}

	nonNeg("behaviour", "Orbit range", s.FlyRange)
	nonNeg("behaviour", "Attack range", s.AttackRange)
	nonNeg("behaviour", "Chase range", s.ChaseMaxDist)
	nonNeg("behaviour", "First-shot delay", s.AttackDelayMinMs)
	nonNeg("behaviour", "First-shot delay", s.AttackDelayMaxMs)
	if s.AttackDelayMaxMs > 0 && s.AttackDelayMinMs > s.AttackDelayMaxMs {
		add("behaviour", "First-shot delay min is above max.")
	}

	nonNeg("rewards", "Bounty", s.Bounty)
	if s.HasSecStatusBonus && (s.SecStatusBonus < -1 || s.SecStatusBonus > 1) {
		add("rewards", "Security status gain must be between -100%% and 100%% of the gap.")
	}
	return out
}

// validateNPCSpecRefs checks that the IDs a spec points at exist and are the right kind.
func validateNPCSpecRefs(ctx context.Context, db *sql.DB, s *NPCTypeSpec) NPCSpecErrors {
	var out NPCSpecErrors
	exists := func(query string, args ...any) bool {
		var n int
		if err := db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
			return false
		}
		return n > 0
	}

	if s.GroupID > 0 && !exists(`SELECT COUNT(*) FROM invGroups WHERE groupID = ? AND categoryID = 11`, s.GroupID) {
		out = append(out, NPCSpecProblem{"identity", fmt.Sprintf("Group %d is not an entity group (category 11).", s.GroupID)})
	}
	if s.GraphicID > 0 && !exists(`SELECT COUNT(*) FROM eveGraphics WHERE graphicID = ?`, s.GraphicID) {
		out = append(out, NPCSpecProblem{"model", fmt.Sprintf("Graphic %d does not exist.", s.GraphicID)})
	}
	if s.WeaponKind == "missile" && s.MissileTypeID > 0 {
		if !exists(`SELECT COUNT(*) FROM invTypes t JOIN invGroups g ON g.groupID = t.groupID
		             WHERE t.typeID = ? AND g.categoryID = 8
		               AND EXISTS (SELECT 1 FROM dgmTypeAttributes a WHERE a.typeID = t.typeID AND a.attributeID = 281)`,
			s.MissileTypeID) {
			out = append(out, NPCSpecProblem{"weapons", fmt.Sprintf("Missile type %d is not a charge with a flight time.", s.MissileTypeID)})
		} else if s.DamageEM+s.DamageExp+s.DamageKin+s.DamageTherm <= 0 &&
			!exists(`SELECT COUNT(*) FROM dgmTypeAttributes
			          WHERE typeID = ? AND attributeID IN (114, 116, 117, 118)
			            AND COALESCE(NULLIF(valueFloat, 0), valueInt, 0) > 0`, s.MissileTypeID) {
			out = append(out, NPCSpecProblem{"weapons", "Neither the NPC nor its missile type carries any damage."})
		}
	}
	if s.WeaponKind == "turret" && s.TurretTypeIDOverride > 0 &&
		!exists(`SELECT COUNT(*) FROM invTypes WHERE typeID = ?`, s.TurretTypeIDOverride) {
		out = append(out, NPCSpecProblem{"weapons", fmt.Sprintf("Turret type %d does not exist.", s.TurretTypeIDOverride)})
	}
	return out
}

// ── EWAR kinds ──────────────────────────────────────────────────────────────

// NPCEwarKindCount is the number of EWAR kinds an NPC type can carry (len(npcEwarAttrSets)).
const NPCEwarKindCount = 7

// NPCEwarKind describes one EWAR kind for the Create NPC form. EffectLabel is "" for a kind with
// no effect value. The form shows the effect in EffectUnit; EffectToForm and EffectFromForm
// convert to and from the stored attribute.
type NPCEwarKind struct {
	Key         string
	Label       string
	Help        string
	HasDuration bool
	HasFalloff  bool
	EffectLabel string
	EffectUnit  string
	EffectHelp  string
	EffectMax   float64 // 0 = no upper bound
	Default     NPCEwarSpec
}

// npcEwarKinds is in npcEwarAttrSets order. TestNPCEwarKindsMatchAttrSets pins the pairing.
var npcEwarKinds = [NPCEwarKindCount]NPCEwarKind{
	{Key: "scram", Label: "Warp scrambler",
		Help:        "Stops the target warping while the NPC holds it. No duration: it lasts as long as the lock.",
		EffectLabel: "Strength", EffectUnit: "points",
		EffectHelp: "Warp scramble points (attr 105). Each warp core stabilizer cancels one.",
		Default:    NPCEwarSpec{Chance: 0.5, Range: 10_000, Effect: 1}},
	{Key: "web", Label: "Stasis webifier", HasDuration: true,
		Help:        "Slows the target. No falloff: full strength out to range.",
		EffectLabel: "Speed reduction", EffectUnit: "%", EffectMax: 95,
		EffectHelp: "How much of the target's speed is taken away (attr 20). Capped at 95%.",
		Default:    NPCEwarSpec{Chance: 0.5, DurationMs: 20_000, Range: 10_000, Effect: -50}},
	{Key: "jam", Label: "ECM jammer", HasDuration: true, HasFalloff: true,
		Help:    "Breaks the target's locks for the duration.",
		Default: NPCEwarSpec{Chance: 0.05, DurationMs: 20_000, Range: 30_000, Falloff: 15_000}},
	{Key: "capDrain", Label: "Energy neutralizer", HasDuration: true, HasFalloff: true,
		Help:        "Drains the target's capacitor each cycle.",
		EffectLabel: "Drain", EffectUnit: "GJ",
		EffectHelp: "Capacitor removed per cycle (attr 946).",
		Default:    NPCEwarSpec{Chance: 0.5, DurationMs: 20_000, Range: 15_000, Falloff: 5_000, Effect: 50}},
	{Key: "sensorDamp", Label: "Sensor dampener", HasDuration: true, HasFalloff: true,
		Help:        "Cuts the target's lock range and scan resolution.",
		EffectLabel: "Reduction", EffectUnit: "%", EffectMax: 99,
		EffectHelp: "How much lock range and scan resolution are cut (attr 947).",
		Default:    NPCEwarSpec{Chance: 0.25, DurationMs: 20_000, Range: 30_000, Falloff: 15_000, Effect: 0.75}},
	{Key: "trackingDisrupt", Label: "Tracking disruptor", HasDuration: true, HasFalloff: true,
		Help:        "Cuts the target's turret range and tracking.",
		EffectLabel: "Reduction", EffectUnit: "%", EffectMax: 99,
		EffectHelp: "How much turret optimal, falloff and tracking are cut (attr 948).",
		Default:    NPCEwarSpec{Chance: 0.25, DurationMs: 20_000, Range: 30_000, Falloff: 15_000, Effect: 0.75}},
	{Key: "targetPaint", Label: "Target painter", HasDuration: true, HasFalloff: true,
		Help:        "Grows the target's signature so it is easier to hit.",
		EffectLabel: "Signature increase", EffectUnit: "%",
		EffectHelp: "How much bigger the target's signature gets (attr 949).",
		Default:    NPCEwarSpec{Chance: 0.25, DurationMs: 20_000, Range: 30_000, Falloff: 15_000, Effect: 1.3}},
}

// NPCEwarKinds returns the EWAR kinds in the order NPCTypeSpec.Ewar is indexed.
func NPCEwarKinds() [NPCEwarKindCount]NPCEwarKind { return npcEwarKinds }

// EffectToForm converts the stored effect attribute to the unit the form shows. It reads the
// attribute the way applyNPCEwar does, so the form shows the effect the NPC actually has.
func (k NPCEwarKind) EffectToForm(raw float64) float64 {
	switch k.Key {
	case "web":
		// attr 20 is a negative percent (-75) or a fractional multiplier (0.25).
		switch {
		case raw < 0:
			return -raw
		case raw > 0 && raw <= 1:
			return (1 - raw) * 100
		}
		return 0
	case "sensorDamp", "trackingDisrupt":
		if raw > 0 && raw < 1 {
			return (1 - raw) * 100
		}
		return 0
	case "targetPaint":
		if raw > 1 {
			return (raw - 1) * 100
		}
		return 0
	}
	return raw
}

// EffectFromForm converts a form value back to the stored attribute.
func (k NPCEwarKind) EffectFromForm(v float64) float64 {
	switch k.Key {
	case "web":
		return -v
	case "sensorDamp", "trackingDisrupt":
		return 1 - v/100
	case "targetPaint":
		return 1 + v/100
	}
	return v
}

// ── Context panels ──────────────────────────────────────────────────────────

// ESPGroupLoot summarises the loot table a group gives every type in it.
type ESPGroupLoot struct {
	GroupID   int64
	GroupName string
	Entries   int64
	Cap       int64 // max drops per kill; 0 = no group cap row
}

// ESPGroupLootSummaries returns the loot summary of every entity group, keyed by groupID, so the
// form can show the inherited loot for whichever group is picked without a round trip.
func ESPGroupLootSummaries(ctx context.Context, db *sql.DB) (map[int64]ESPGroupLoot, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT g.groupID, g.groupName,
		       (SELECT COUNT(*) FROM npc_loot_entries e WHERE e.npcTypeID = 0 AND e.npcGroupID = g.groupID),
		       COALESCE((SELECT c.maxDrops FROM npc_loot_caps c WHERE c.npcTypeID = 0 AND c.npcGroupID = g.groupID), 0)
		  FROM invGroups g
		 WHERE g.categoryID = 11`)
	if err != nil {
		return nil, fmt.Errorf("ESPGroupLootSummaries: %w", err)
	}
	defer rows.Close()
	out := map[int64]ESPGroupLoot{}
	for rows.Next() {
		var l ESPGroupLoot
		if err := rows.Scan(&l.GroupID, &l.GroupName, &l.Entries, &l.Cap); err != nil {
			return nil, fmt.Errorf("ESPGroupLootSummaries scan: %w", err)
		}
		out[l.GroupID] = l
	}
	return out, rows.Err()
}

// ESPTypeSpawnListing is one region belt listing of an NPC type.
type ESPTypeSpawnListing struct {
	RegionID   int64
	RegionName string
	MaxSec     float64
	Weight     int
	ShipClass  string
	Role       string
	FactionID  int64
}

// ESPRegionSpawnsForType returns the region belt listings that roll the given type.
func ESPRegionSpawnsForType(ctx context.Context, db *sql.DB, typeID int64) ([]ESPTypeSpawnListing, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT n.regionID, COALESCE(r.regionName, ''), n.maxSec, n.weight, n.shipClass, n.role,
		       COALESCE(n.factionID, 0)
		  FROM npcRegionSpawns n
		  LEFT JOIN mapRegions r ON r.regionID = n.regionID
		 WHERE n.typeID = ?
		 ORDER BY r.regionName LIMIT 200`, typeID)
	if err != nil {
		return nil, fmt.Errorf("ESPRegionSpawnsForType: %w", err)
	}
	defer rows.Close()
	var out []ESPTypeSpawnListing
	for rows.Next() {
		var l ESPTypeSpawnListing
		if err := rows.Scan(&l.RegionID, &l.RegionName, &l.MaxSec, &l.Weight, &l.ShipClass, &l.Role, &l.FactionID); err != nil {
			return nil, fmt.Errorf("ESPRegionSpawnsForType scan: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
