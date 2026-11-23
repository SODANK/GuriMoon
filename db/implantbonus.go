package db

import (
	"context"
	"database/sql"
	"time"

	"gurimoon/dogma"
	"gurimoon/syncx"
)

// Implant multipliers: the plugged-in implants folded through the same wrappers the skills
// use (GetWeaponCombatBonuses, GetTankSkillMultipliers, ...), so every caller sees them.
// ImplantMults is its own type: the rolebots module builds HullWeaponMults with a keyed
// literal, and a field added there reads 0 in its copies and zeroes a bot's weapons.
// The per-pilot read is cached for implantTTL and dropped by InvalidateImplants.
// See docs/IMPLANTS.md#core.

// ImplantSetState is one pirate set's standing on a pilot.
type ImplantSetState struct {
	Grade  dogma.ImplantGrade
	Pieces int     // pieces plugged, Omega included
	Omega  bool    // the Omega is in
	Factor float64 // product of every plugged piece's set multiplier
}

// ImplantMults is the folded effect of every implant plugged into a pilot.
type ImplantMults struct {
	// Turret damage: every turret, and per family (turret skill ID).
	TurretDmgAll                                                             float64
	TurretDmg                                                                map[int64]float64
	TurretCycle, TurretOptimal, TurretFalloff, TurretTracking, TurretCapNeed float64
	LauncherCycle, MissileVelocity, MissileFlightTime                        float64
	ShieldHP, ArmorHP, HullHP, ShieldRecharge                                float64
	ShieldBoost, ArmorRepAmount, RepairCycle                                 float64
	// RemoteRepCapNeed by emission skill (Shield Emission, Remote Armor
	// Repair, Energy Emission).
	RemoteRepCapNeed                                     map[int64]float64
	CapCapacity, CapRecharge                             float64
	Velocity, Agility                                    float64
	SigRadius, TargetRange                               float64
	MiningYield, MiningDroneYield, IceCycle, NosDuration float64
	// Fitting need by the required skill that selects a module (FittingNeed), and the ship's
	// CPU and powergrid output (FittingOutput). Read them through those methods: a zero
	// field or nil map means no implant.
	FittingCPUNeed, FittingPowerNeed map[int64]float64
	CPUOutput, PowerOutput           float64
	// WarpCapNeed scales the ship's warpCapacitorNeed (attr 153). Zero means no implant;
	// read it through orOne.
	WarpCapNeed float64
	// Sets is the standing of each pirate set with at least one piece in.
	Sets map[dogma.ImplantSet]ImplantSetState
	// Tags are the plugged implants' tags, for display and planners
	// (slot order is not guaranteed).
	Tags []dogma.ImplantTag
}

// IdentityImplantMults is the unaugmented pilot: nothing moves.
func IdentityImplantMults() ImplantMults {
	return ImplantMults{
		TurretDmgAll: 1, TurretDmg: map[int64]float64{},
		TurretCycle: 1, TurretOptimal: 1, TurretFalloff: 1, TurretTracking: 1, TurretCapNeed: 1,
		LauncherCycle: 1, MissileVelocity: 1, MissileFlightTime: 1,
		ShieldHP: 1, ArmorHP: 1, HullHP: 1, ShieldRecharge: 1,
		ShieldBoost: 1, ArmorRepAmount: 1, RepairCycle: 1,
		RemoteRepCapNeed: map[int64]float64{},
		CapCapacity:      1, CapRecharge: 1,
		Velocity: 1, Agility: 1, SigRadius: 1, TargetRange: 1,
		MiningYield: 1, MiningDroneYield: 1, IceCycle: 1, NosDuration: 1,
		FittingCPUNeed: map[int64]float64{}, FittingPowerNeed: map[int64]float64{},
		CPUOutput: 1, PowerOutput: 1, WarpCapNeed: 1,
		Sets: map[dogma.ImplantSet]ImplantSetState{},
	}
}

// IsIdentity reports a pilot whose implants move nothing on the ship.
func (m ImplantMults) IsIdentity() bool {
	return m.TurretDmgAll == 1 && len(m.TurretDmg) == 0 &&
		m.TurretCycle == 1 && m.TurretOptimal == 1 && m.TurretFalloff == 1 && m.TurretTracking == 1 && m.TurretCapNeed == 1 &&
		m.LauncherCycle == 1 && m.MissileVelocity == 1 && m.MissileFlightTime == 1 &&
		m.ShieldHP == 1 && m.ArmorHP == 1 && m.HullHP == 1 && m.ShieldRecharge == 1 &&
		m.ShieldBoost == 1 && m.ArmorRepAmount == 1 && m.RepairCycle == 1 && len(m.RemoteRepCapNeed) == 0 &&
		m.CapCapacity == 1 && m.CapRecharge == 1 && m.Velocity == 1 && m.Agility == 1 &&
		m.SigRadius == 1 && m.TargetRange == 1 &&
		m.MiningYield == 1 && m.MiningDroneYield == 1 && m.IceCycle == 1 && m.NosDuration == 1 &&
		len(m.FittingCPUNeed) == 0 && len(m.FittingPowerNeed) == 0 &&
		orOne(m.CPUOutput) == 1 && orOne(m.PowerOutput) == 1 && orOne(m.WarpCapNeed) == 1
}

// orOne reads an unset (zero) multiplier as 1.
func orOne(v float64) float64 {
	if v == 0 {
		return 1
	}
	return v
}

// FittingNeed is the CPU and powergrid need multiplier for a module whose direct required
// skills are reqSkills: every implant line naming one of them multiplies in.
func (m ImplantMults) FittingNeed(reqSkills map[int64]bool) (cpu, pg float64) {
	cpu, pg = 1, 1
	for s := range reqSkills {
		if v, ok := m.FittingCPUNeed[s]; ok {
			cpu *= v
		}
		if v, ok := m.FittingPowerNeed[s]; ok {
			pg *= v
		}
	}
	return cpu, pg
}

// FittingOutput is the ship CPU and powergrid output multiplier.
func (m ImplantMults) FittingOutput() (cpu, pg float64) {
	return orOne(m.CPUOutput), orOne(m.PowerOutput)
}

// TurretDamage is the damage multiplier for a turret requiring reqSkills:
// the every-turret term times each matching family term.
func (m ImplantMults) TurretDamage(reqSkills map[int64]bool) float64 {
	f := m.TurretDmgAll
	for s := range reqSkills {
		if v, ok := m.TurretDmg[s]; ok {
			f *= v
		}
	}
	return f
}

// RemoteRepCap is the capacitor-need multiplier for a remote repair module
// requiring reqSkills (one emission skill matches).
func (m ImplantMults) RemoteRepCap(reqSkills []int64) float64 {
	f := 1.0
	for _, s := range reqSkills {
		if v, ok := m.RemoteRepCapNeed[s]; ok {
			f *= v
		}
	}
	return f
}

// ImplantMultsFrom is the pure fold: each tag's folded channels multiply in, and a
// set piece's bonus is scaled by its set factor.
func ImplantMultsFrom(tags []dogma.ImplantTag) ImplantMults {
	m := IdentityImplantMults()
	m.Tags = tags
	// Sets first: the factor needs every piece before any piece is applied.
	byGradeMults := map[dogma.ImplantSet][]float64{}
	for _, t := range tags {
		if t.Set != dogma.ISNone && t.SetMult > 0 {
			byGradeMults[t.Set] = append(byGradeMults[t.Set], t.SetMult)
		}
	}
	for set, mults := range byGradeMults {
		st := ImplantSetState{Pieces: len(mults), Factor: dogma.ImplantSetFactor(mults)}
		for _, t := range tags {
			if t.Set == set {
				if t.Omega {
					st.Omega = true
				}
				if t.Grade > st.Grade {
					st.Grade = t.Grade
				}
			}
		}
		m.Sets[set] = st
	}
	for _, t := range tags {
		if t.Mindlink || t.Dummy {
			continue
		}
		scale := 1.0
		if t.Set != dogma.ISNone {
			scale = m.Sets[t.Set].Factor
		}
		for _, e := range t.Effects {
			if !e.Channel.Folded() {
				continue
			}
			f := 1 + e.Pct/100*scale
			m.apply(e.Channel, e.ReqSkill, f)
		}
	}
	return m
}

func (m *ImplantMults) apply(c dogma.ImplantChannel, reqSkill int64, f float64) {
	switch c {
	case dogma.ICTurretDamage:
		if reqSkill == 0 {
			m.TurretDmgAll *= f
		} else {
			if _, ok := m.TurretDmg[reqSkill]; !ok {
				m.TurretDmg[reqSkill] = 1
			}
			m.TurretDmg[reqSkill] *= f
		}
	case dogma.ICTurretCycle:
		m.TurretCycle *= f
	case dogma.ICTurretOptimal:
		m.TurretOptimal *= f
	case dogma.ICTurretFalloff:
		m.TurretFalloff *= f
	case dogma.ICTurretTracking:
		m.TurretTracking *= f
	case dogma.ICTurretCapNeed:
		m.TurretCapNeed *= f
	case dogma.ICLauncherCycle:
		m.LauncherCycle *= f
	case dogma.ICMissileVelocity:
		m.MissileVelocity *= f
	case dogma.ICMissileFlightTime:
		m.MissileFlightTime *= f
	case dogma.ICShieldHP:
		m.ShieldHP *= f
	case dogma.ICArmorHP:
		m.ArmorHP *= f
	case dogma.ICHullHP:
		m.HullHP *= f
	case dogma.ICShieldRecharge:
		m.ShieldRecharge *= f
	case dogma.ICShieldBoost:
		m.ShieldBoost *= f
	case dogma.ICArmorRepAmount:
		m.ArmorRepAmount *= f
	case dogma.ICRepairCycle:
		m.RepairCycle *= f
	case dogma.ICRemoteRepCapNeed:
		if _, ok := m.RemoteRepCapNeed[reqSkill]; !ok {
			m.RemoteRepCapNeed[reqSkill] = 1
		}
		m.RemoteRepCapNeed[reqSkill] *= f
	case dogma.ICCapCapacity:
		m.CapCapacity *= f
	case dogma.ICCapRecharge:
		m.CapRecharge *= f
	case dogma.ICVelocity:
		m.Velocity *= f
	case dogma.ICAgility:
		m.Agility *= f
	case dogma.ICSigRadius:
		m.SigRadius *= f
	case dogma.ICTargetRange:
		m.TargetRange *= f
	case dogma.ICMiningYield:
		m.MiningYield *= f
	case dogma.ICMiningDroneYield:
		m.MiningDroneYield *= f
	case dogma.ICIceCycle:
		m.IceCycle *= f
	case dogma.ICNosDuration:
		m.NosDuration *= f
	case dogma.ICFittingCPU:
		mulKeyed(&m.FittingCPUNeed, reqSkill, f)
	case dogma.ICFittingPower:
		mulKeyed(&m.FittingPowerNeed, reqSkill, f)
	case dogma.ICCPUOutput:
		m.CPUOutput = orOne(m.CPUOutput) * f
	case dogma.ICPowerOutput:
		m.PowerOutput = orOne(m.PowerOutput) * f
	case dogma.ICWarpCapNeed:
		m.WarpCapNeed = orOne(m.WarpCapNeed) * f
	}
}

// mulKeyed multiplies f into (*mp)[key], creating the map and the entry at 1.
func mulKeyed(mp *map[int64]float64, key int64, f float64) {
	if *mp == nil {
		*mp = map[int64]float64{}
	}
	if _, ok := (*mp)[key]; !ok {
		(*mp)[key] = 1
	}
	(*mp)[key] *= f
}

// ── per-pilot read, cached ──────────────────────────────────────────────────

const implantTTL = 60 * time.Second

type implantEntry struct {
	mults ImplantMults
	at    time.Time
}

var implantCache syncx.Map[int64, implantEntry] // charID → implantEntry

// InvalidateImplants drops a pilot's cached implant fold. Called by every
// write that changes the set: plug, unplug, pod death, clone jump.
func InvalidateImplants(charID int64) { implantCache.Delete(charID) }

// TagImplantType classifies one implant type from the static data.
func TagImplantType(ctx context.Context, db *sql.DB, typeID int64) (dogma.ImplantTag, error) {
	effects, err := GetTypeEffects(ctx, db, typeID)
	if err != nil {
		return dogma.ImplantTag{}, err
	}
	names := make([]string, 0, len(effects))
	for _, e := range effects {
		names = append(names, e.EffectName)
	}
	attrs, err := TypeAttrs(ctx, db, typeID)
	if err != nil {
		return dogma.ImplantTag{}, err
	}
	return dogma.TagImplant(names, attrs), nil
}

// GetCharImplantMults reads and folds charID's plugged implants. A failure returns
// identity and is not cached, so the next read retries.
func GetCharImplantMults(ctx context.Context, db *sql.DB, charID int64) ImplantMults {
	if db == nil || charID == 0 {
		return IdentityImplantMults()
	}
	now := time.Now()
	if v, ok := implantCache.Load(charID); ok {
		if e := v; now.Sub(e.at) < implantTTL {
			return e.mults
		}
	}
	rows, err := db.QueryContext(ctx,
		`SELECT typeID FROM entity WHERE locationID = ? AND flag = ?`, charID, flagImplant)
	if err != nil {
		return IdentityImplantMults()
	}
	var typeIDs []int64
	for rows.Next() {
		var t int64
		if err := rows.Scan(&t); err == nil {
			typeIDs = append(typeIDs, t)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return IdentityImplantMults()
	}
	tags := make([]dogma.ImplantTag, 0, len(typeIDs))
	for _, t := range typeIDs {
		tag, err := TagImplantType(ctx, db, t)
		if err != nil {
			return IdentityImplantMults()
		}
		tags = append(tags, tag)
	}
	m := ImplantMultsFrom(tags)
	implantCache.Store(charID, implantEntry{mults: m, at: now})
	return m
}

// GetMiningSkillMult is the pilot's whole laser-yield term: Mining and Astrogeology
// (+5%/level each) and the mining hardwirings. One call for every caller.
func GetMiningSkillMult(ctx context.Context, db *sql.DB, charID int64) float64 {
	if db == nil || charID == 0 {
		return 1
	}
	mining, _ := GetCharacterSkillLevel(ctx, db, charID, dogma.SkillMining)
	astro, _ := GetCharacterSkillLevel(ctx, db, charID, dogma.SkillAstrogeology)
	return (1 + float64(mining)*0.05) * (1 + float64(astro)*0.05) * GetCharImplantMults(ctx, db, charID).MiningYield
}

// GetIceCycleMult is the pilot's ice harvester cycle multiplier from implants
// (the Ice Harvesting skill carries no cycle bonus in this data).
func GetIceCycleMult(ctx context.Context, db *sql.DB, charID int64) float64 {
	return GetCharImplantMults(ctx, db, charID).IceCycle
}
