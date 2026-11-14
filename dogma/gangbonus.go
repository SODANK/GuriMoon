package dogma

import (
	"math"
	"sort"
	"strconv"
)

// Gang bonuses: the Leadership skill group (258), the five mindlink implants and the
// Warfare/Foreman Link modules (group 316).
//
// A bonus comes from the gang member with the best value for it and reaches every gang member
// in space in the same solar system as the source (the source included). Bonuses of one kind
// never stack: a Channel is one kind (each skill, each link type), and different channels that
// move the same stat multiply. The semantics are authored, as in hullbonus.go, because the data
// ships no dgmExpressions; the per-level percentage is read from the skill type (BonusAttr).

// GangStat names a stat a gang bonus moves.
type GangStat uint8

const (
	GSScanResolution GangStat = iota // Leadership ("gang targeting speed")
	GSMaxVelocity                    // Skirmish Warfare
	GSShieldHP                       // Siege Warfare
	GSArmorHP                        // Armored Warfare
	GSMaxTargetRange                 // Information Warfare
	GSMiningYield                    // Mining Foreman; Laser Optimization link

	// Link-only stats (GangLinkSpecs).
	GSShieldResonance     // Shield Harmonizing: all four shield resonances
	GSArmorResonance      // Passive Defense: all four armor resonances
	GSArmorRepCapNeed     // Damage Control
	GSArmorRepDuration    // Rapid Repair
	GSShieldBoostCapNeed  // Shield Efficiency
	GSShieldBoostDuration // Active Shielding
	GSSigRadius           // Evasive Maneuvers
	GSPropSpeedFactor     // Rapid Deployment: AB/MWD speedFactor
	GSPropJamRange        // Interdiction Maneuvers: scram/web range
	GSMiningDroneYield    // Drone Coordination
	GSIceHarvestDuration  // Ice Harvesting

	NumGangStats
)

var gangStatNames = [NumGangStats]string{
	GSScanResolution:      "scan resolution",
	GSMaxVelocity:         "max velocity",
	GSShieldHP:            "shield HP",
	GSArmorHP:             "armor HP",
	GSMaxTargetRange:      "targeting range",
	GSMiningYield:         "mining yield",
	GSShieldResonance:     "shield resonance",
	GSArmorResonance:      "armor resonance",
	GSArmorRepCapNeed:     "armor repair cap need",
	GSArmorRepDuration:    "armor repair cycle",
	GSShieldBoostCapNeed:  "shield boost cap need",
	GSShieldBoostDuration: "shield boost cycle",
	GSSigRadius:           "signature radius",
	GSPropSpeedFactor:     "AB/MWD speed boost",
	GSPropJamRange:        "scram/web range",
	GSMiningDroneYield:    "mining drone yield",
	GSIceHarvestDuration:  "ice harvester cycle",
}

// String is the stat's display name (ESP, logs).
func (s GangStat) String() string {
	if s < NumGangStats {
		return gangStatNames[s]
	}
	return "stat " + strconv.Itoa(int(s))
}

// Gang skill attributes. Named here, not in attributes.go, because two of these IDs carry a
// module meaning there (335 is a plate's flat armor add); on a skill type they are per-level %.
const (
	attrGangVelocityBonus       int64 = 315 // velocityBonus
	attrGangArmorHPBonus        int64 = 335 // armorHpBonus
	attrGangShieldCapacityBonus int64 = 337 // shieldCapacityBonus
	attrGangMiningAmountBonus   int64 = 434 // miningAmountBonus

	// AttrMindlinkBonus is the mindlink's "% increase to <skill> effectiveness": 50 on all five.
	AttrMindlinkBonus int64 = 884
)

// Gang skill and mindlink type IDs.
const (
	SkillLeadership         int64 = 3348
	SkillSkirmishWarfare    int64 = 3349
	SkillSiegeWarfare       int64 = 3350
	SkillArmoredWarfare     int64 = 20494
	SkillInformationWarfare int64 = 20495
	SkillMiningForeman      int64 = 22536

	MindlinkArmoredWarfare     int64 = 13209
	MindlinkSiegeWarfare       int64 = 21888
	MindlinkInformationWarfare int64 = 21889
	MindlinkSkirmishWarfare    int64 = 21890
	MindlinkMiningForeman      int64 = 22559
)

// GangSkillSpec is one gang-bonus skill.
type GangSkillSpec struct {
	SkillID   int64
	Stat      GangStat
	BonusAttr int64 // per-level % on the skill type
	// Mindlink multiplies this skill's bonus by (1 + mindlinkBonus/100); Leadership has none.
	// The mindlinks' flat 15 is not read. See docs/support-notes.md#gang-bonus-data-choices.
	Mindlink int64
}

// GangSkillSpecs lists every skill that projects a bonus onto the gang. Leadership's own SELF
// effect (698) is not read: the gang copy already reaches its source, so it would count twice.
var GangSkillSpecs = []GangSkillSpec{
	{SkillID: SkillLeadership, Stat: GSScanResolution, BonusAttr: AttrScanResolutionBonus},
	{SkillID: SkillSkirmishWarfare, Stat: GSMaxVelocity, BonusAttr: attrGangVelocityBonus, Mindlink: MindlinkSkirmishWarfare},
	{SkillID: SkillSiegeWarfare, Stat: GSShieldHP, BonusAttr: attrGangShieldCapacityBonus, Mindlink: MindlinkSiegeWarfare},
	{SkillID: SkillArmoredWarfare, Stat: GSArmorHP, BonusAttr: attrGangArmorHPBonus, Mindlink: MindlinkArmoredWarfare},
	{SkillID: SkillInformationWarfare, Stat: GSMaxTargetRange, BonusAttr: AttrMaxTargetRangeBonus, Mindlink: MindlinkInformationWarfare},
	{SkillID: SkillMiningForeman, Stat: GSMiningYield, BonusAttr: attrGangMiningAmountBonus, Mindlink: MindlinkMiningForeman},
}

// GangSkillIDs returns every skill a gang source can contribute through.
func GangSkillIDs() []int64 {
	out := make([]int64, 0, len(GangSkillSpecs))
	for _, s := range GangSkillSpecs {
		out = append(out, s.SkillID)
	}
	return out
}

// GangMindlinkIDs returns every implant type that scales a gang bonus.
func GangMindlinkIDs() []int64 {
	var out []int64
	for _, s := range GangSkillSpecs {
		if s.Mindlink != 0 {
			out = append(out, s.Mindlink)
		}
	}
	return out
}

// GangContribution is one source's offer on one channel.
type GangContribution struct {
	Channel      string // "skill:<typeID>" or "link:<typeID>"
	Stat         GangStat
	Pct          float64 // signed percent; + raises the stat, − lowers it
	SourceCharID int64
}

// GangBoost is the set of multipliers applied to one gang member. The zero
// value is the identity: an unset stat reads as 1.0, which is what a freshly
// built entity must mean before any recompute has run. Comparable with ==.
type GangBoost struct {
	mult [NumGangStats]float64
}

// Mult returns the multiplier for stat (1.0 when unset).
func (b GangBoost) Mult(s GangStat) float64 {
	if s >= NumGangStats || b.mult[s] == 0 {
		return 1
	}
	return b.mult[s]
}

// IsIdentity reports whether every stat is at 1.0.
func (b GangBoost) IsIdentity() bool {
	for s := GangStat(0); s < NumGangStats; s++ {
		if b.Mult(s) != 1 {
			return false
		}
	}
	return true
}

// Normalized returns b with every unset stat written out as 1.0, so two
// boosts that mean the same compare equal.
func (b GangBoost) Normalized() GangBoost {
	var out GangBoost
	for s := GangStat(0); s < NumGangStats; s++ {
		out.mult[s] = b.Mult(s)
	}
	return out
}

// WithMult returns b with stat s set to m (tests, callers composing a boost).
func (b GangBoost) WithMult(s GangStat, m float64) GangBoost {
	b = b.Normalized()
	if s < NumGangStats {
		b.mult[s] = m
	}
	return b
}

// Ratio returns the per-stat multiplier that turns a value boosted by `from` into one boosted
// by `to`, so state baked in under the old boost (HP pools) can be rescaled without a rebuild.
func (b GangBoost) Ratio(from GangBoost) GangBoost {
	var out GangBoost
	for s := GangStat(0); s < NumGangStats; s++ {
		out.mult[s] = b.Mult(s) / from.Mult(s)
	}
	return out
}

// SkillContributions returns the offers one pilot makes on every gang skill
// channel. levels is the pilot's trained levels (0 or absent = untrained),
// implants the implant types plugged in, perLevel the skill → per-level
// percentage and mindlinkPct the mindlink → percentage increase (50).
func SkillContributions(charID int64, levels map[int64]int, implants map[int64]bool,
	perLevel map[int64]float64, mindlinkPct map[int64]float64) []GangContribution {
	var out []GangContribution
	for _, spec := range GangSkillSpecs {
		lvl := levels[spec.SkillID]
		per := perLevel[spec.SkillID]
		if lvl <= 0 || per == 0 {
			continue
		}
		pct := per * float64(lvl)
		if spec.Mindlink != 0 && implants[spec.Mindlink] {
			pct *= 1 + mindlinkPct[spec.Mindlink]/100
		}
		out = append(out, GangContribution{
			Channel:      "skill:" + strconv.FormatInt(spec.SkillID, 10),
			Stat:         spec.Stat,
			Pct:          pct,
			SourceCharID: charID,
		})
	}
	return out
}

// ── Warfare & Foreman Links (group 316) ─────────────────────────────────────

// Link specialist skills and the leadership skills that scale link strength.
const (
	SkillSiegeWarfareSpecialist       int64 = 3351
	SkillInformationWarfareSpecialist int64 = 3352
	SkillSquadronCommand              int64 = 3354
	SkillArmoredWarfareSpecialist     int64 = 11569
	SkillSkirmishWarfareSpecialist    int64 = 11572
	SkillMiningDirector               int64 = 22552

	// TypeCommandProcessor raises how many links one ship may run.
	TypeCommandProcessor int64 = 11014

	// AttrCommandBonus is a link's base strength in percent, signed.
	AttrCommandBonus int64 = 833
	// AttrSquadronCommandBonus is Squadron Command's per-level % (10).
	AttrSquadronCommandBonus int64 = 850
	// AttrMaxGangModules is how many extra links a Command Processor allows.
	AttrMaxGangModules int64 = 435

	// GroupGangCoordinator holds every link AND the Command Processor.
	GroupGangCoordinator int64 = 316
)

// specialistPctPerLevel is the specialists' "100% bonus to effectiveness of <family> link
// modules per level". No attribute carries it; the description is the only statement.
const specialistPctPerLevel = 100.0

// GangLinkSpec is one link module.
type GangLinkSpec struct {
	TypeID     int64
	Stat       GangStat
	Specialist int64 // skill whose level multiplies strength by (1 + 1.0×lvl)
	Mindlink   int64 // implant that multiplies strength by (1 + mindlinkBonus%)
	Foreman    bool  // Mining Foreman link: Squadron Command does not apply
}

// GangLinkSpecs lists every link the server applies. Sensor Integrity (11052), Electronic
// Superiority (20406) and Recon Operation (20405) are absent, and Laser Optimization is read
// as a yield bonus. See docs/support-notes.md#gang-bonus-data-choices.
var GangLinkSpecs = []GangLinkSpec{
	{TypeID: 20409, Stat: GSArmorResonance, Specialist: SkillArmoredWarfareSpecialist, Mindlink: MindlinkArmoredWarfare},         // Passive Defense
	{TypeID: 20069, Stat: GSArmorRepCapNeed, Specialist: SkillArmoredWarfareSpecialist, Mindlink: MindlinkArmoredWarfare},        // Damage Control
	{TypeID: 22227, Stat: GSArmorRepDuration, Specialist: SkillArmoredWarfareSpecialist, Mindlink: MindlinkArmoredWarfare},       // Rapid Repair
	{TypeID: 20514, Stat: GSShieldResonance, Specialist: SkillSiegeWarfareSpecialist, Mindlink: MindlinkSiegeWarfare},            // Shield Harmonizing
	{TypeID: 20124, Stat: GSShieldBoostDuration, Specialist: SkillSiegeWarfareSpecialist, Mindlink: MindlinkSiegeWarfare},        // Active Shielding
	{TypeID: 22228, Stat: GSShieldBoostCapNeed, Specialist: SkillSiegeWarfareSpecialist, Mindlink: MindlinkSiegeWarfare},         // Shield Efficiency
	{TypeID: 20070, Stat: GSSigRadius, Specialist: SkillSkirmishWarfareSpecialist, Mindlink: MindlinkSkirmishWarfare},            // Evasive Maneuvers
	{TypeID: 20408, Stat: GSPropSpeedFactor, Specialist: SkillSkirmishWarfareSpecialist, Mindlink: MindlinkSkirmishWarfare},      // Rapid Deployment
	{TypeID: 11017, Stat: GSPropJamRange, Specialist: SkillSkirmishWarfareSpecialist, Mindlink: MindlinkSkirmishWarfare},         // Interdiction Maneuvers
	{TypeID: 22557, Stat: GSMiningYield, Specialist: SkillMiningDirector, Mindlink: MindlinkMiningForeman, Foreman: true},        // Laser Optimization
	{TypeID: 22555, Stat: GSMiningDroneYield, Specialist: SkillMiningDirector, Mindlink: MindlinkMiningForeman, Foreman: true},   // Drone Coordination
	{TypeID: 22553, Stat: GSIceHarvestDuration, Specialist: SkillMiningDirector, Mindlink: MindlinkMiningForeman, Foreman: true}, // Ice Harvesting
}

var gangLinkByType = func() map[int64]GangLinkSpec {
	m := make(map[int64]GangLinkSpec, len(GangLinkSpecs))
	for _, s := range GangLinkSpecs {
		m[s.TypeID] = s
	}
	return m
}()

// GangLinkSpecFor returns the spec for a link module type.
func GangLinkSpecFor(typeID int64) (GangLinkSpec, bool) {
	s, ok := gangLinkByType[typeID]
	return s, ok
}

// GangLinkTypeIDs returns every link type the server applies.
func GangLinkTypeIDs() []int64 {
	out := make([]int64, 0, len(GangLinkSpecs))
	for _, s := range GangLinkSpecs {
		out = append(out, s.TypeID)
	}
	return out
}

// GangSourceSkillIDs is every skill a gang source's data must carry: the
// bonus skills plus the ones that scale link strength.
func GangSourceSkillIDs() []int64 {
	out := GangSkillIDs()
	return append(out, SkillSiegeWarfareSpecialist, SkillInformationWarfareSpecialist,
		SkillSquadronCommand, SkillArmoredWarfareSpecialist, SkillSkirmishWarfareSpecialist,
		SkillMiningDirector)
}

// GangLinkStrength returns one running link's strength in percent: commandBonus ×
// (1 + 1.0×specialist lvl) × (1 + squadronPct×lvl/100) × (1 + mindlink%/100) × scale.
// squadronPct is Squadron Command's per-level percentage (attr 850 = 10) and applies to
// warfare links only.
func GangLinkStrength(spec GangLinkSpec, commandBonus float64, levels map[int64]int,
	implants map[int64]bool, squadronPct, mindlinkPct, scale float64) float64 {
	pct := commandBonus * (1 + specialistPctPerLevel/100*float64(levels[spec.Specialist]))
	if !spec.Foreman {
		pct *= 1 + squadronPct/100*float64(levels[SkillSquadronCommand])
	}
	if spec.Mindlink != 0 && implants[spec.Mindlink] {
		pct *= 1 + mindlinkPct/100
	}
	if scale > 0 {
		pct *= scale
	}
	return pct
}

// LinkContributions returns the offers of the links one pilot is running. maxActive caps how
// many count (1 + Command Processors): the strongest maxActive, since the client enforces
// no limit.
func LinkContributions(charID int64, activeLinks []int64, levels map[int64]int, implants map[int64]bool,
	commandBonus map[int64]float64, squadronPct float64, mindlinkPct map[int64]float64,
	scale float64, maxActive int) []GangContribution {
	var out []GangContribution
	seen := map[int64]bool{}
	for _, typeID := range activeLinks {
		spec, ok := gangLinkByType[typeID]
		if !ok || seen[typeID] {
			continue // two of one link on a ship are still one channel
		}
		seen[typeID] = true
		pct := GangLinkStrength(spec, commandBonus[typeID], levels, implants, squadronPct, mindlinkPct[spec.Mindlink], scale)
		if pct == 0 {
			continue
		}
		out = append(out, GangContribution{
			Channel:      "link:" + strconv.FormatInt(typeID, 10),
			Stat:         spec.Stat,
			Pct:          pct,
			SourceCharID: charID,
		})
	}
	if maxActive < 1 {
		maxActive = 1
	}
	if len(out) > maxActive {
		sort.SliceStable(out, func(i, j int) bool { return math.Abs(out[i].Pct) > math.Abs(out[j].Pct) })
		out = out[:maxActive]
	}
	return out
}

// ResolveGangBoost picks the strongest offer on each channel and multiplies the winners into
// one boost. "Strongest" is the largest magnitude, since a link that lowers a stat is
// strongest at its most negative. Ties go to the lower charID. winners is sorted by channel.
func ResolveGangBoost(contribs []GangContribution) (boost GangBoost, winners []GangContribution) {
	best := make(map[string]GangContribution)
	for _, c := range contribs {
		if c.Stat >= NumGangStats || c.Pct == 0 {
			continue
		}
		cur, ok := best[c.Channel]
		if !ok || math.Abs(c.Pct) > math.Abs(cur.Pct) ||
			(math.Abs(c.Pct) == math.Abs(cur.Pct) && c.SourceCharID < cur.SourceCharID) {
			best[c.Channel] = c
		}
	}
	boost = boost.Normalized()
	for _, c := range best {
		m := 1 + c.Pct/100
		if m < 0.01 {
			m = 0.01 // a resonance or cycle time can never reach zero
		}
		boost.mult[c.Stat] *= m
		winners = append(winners, c)
	}
	sort.Slice(winners, func(i, j int) bool { return winners[i].Channel < winners[j].Channel })
	return boost, winners
}
