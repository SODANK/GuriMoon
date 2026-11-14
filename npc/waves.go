package npc

import (
	"math/rand"
	"strings"

	"gurimoon/db"
)

// Wave composition. A belt wave is rolled in three parts: the base wave from
// the normal and hauler listings, a commander promotion that replaces a slot,
// and an officer escalation that appends one and asks the region ledger. The
// squad tier (npcSquadTiers) sets the size deltas, the hull bias, the chances
// and a security floor that applies to the base pool only.
// See docs/npc-behaviour.md#belt-waves.

// WaveConfig is the settings snapshot a roll runs against. It is held on the
// SpawnerManager and refreshed at boot, so a belt seed never touches
// server_settings.
type WaveConfig struct {
	CommandersEnabled bool
	OfficersEnabled   bool
	// The two multipliers scale the per-tier chances. 1.0 is "as the tier table
	// authored it".
	CommanderChanceMult float64
	OfficerChanceMult   float64
	CommandersPerWave   int // how many slots a wave may promote, each rolled separately
	WaveHardCap         int // absolute ceiling after the tier's size deltas
	HullBiasMult        int // weight multiplier for a tier's favoured hull classes
}

// DefaultWaveConfig is what a roll uses before the settings are loaded, and
// what the tests build on. It matches the shipped server_settings defaults.
func DefaultWaveConfig() WaveConfig {
	return WaveConfig{
		CommandersEnabled:   true,
		OfficersEnabled:     true,
		CommanderChanceMult: 1.0,
		OfficerChanceMult:   1.0,
		CommandersPerWave:   db.DefaultCommandersPerWave,
		WaveHardCap:         db.DefaultWaveHardCap,
		HullBiasMult:        db.DefaultTierHullBiasMult,
	}
}

// WaveInput is everything one wave roll needs.
type WaveInput struct {
	Roster   []db.RegionSpawnRow
	Rule     *db.SpawnerRuleRow
	Tiers    []db.SquadTierRow
	Security float64
	Cfg      WaveConfig

	// ClaimOfficer is the region ledger gate: called last, at most once, and only
	// after an officer listing has been drawn. nil means never claim (the ESP
	// preview); PeekOfficer then reports what a claim would have done.
	ClaimOfficer func(row db.RegionSpawnRow) bool
	PeekOfficer  func() (allowed bool, reason string)
}

// WaveOutput is the rolled wave plus every decision that produced it, so the
// ESP preview can show the reasoning rather than only the result.
type WaveOutput struct {
	Tier         db.SquadTierRow
	TierFallback bool    // no tier row covered this security; the built-in normal tier was used
	EffectiveSec float64 // max(Security, Tier.SecFloor): the base pool's filter only
	BasePoolSize int
	CmdrPoolSize int
	OffPoolSize  int
	Rows         []db.RegionSpawnRow
	HasCommander bool
	HasOfficer   bool
	// OfficerDenied says why an officer roll that came up did not land. Empty
	// means it landed, or was never rolled at all.
	OfficerDenied string
}

// defaultNormalTier is the fallback when no authored tier covers a security.
// Zero deltas, zero shift, zero chances: a botched tier table degrades to
// ordinary belts rather than to empty ones.
var defaultNormalTier = db.SquadTierRow{TierName: "normal", SecMin: -1.0, SecMax: 1.1, Weight: 1}

// Reasons an officer roll can fail to land. Surfaced on the ESP preview.
const (
	officerDeniedNoPool  = "no officer listing is eligible at this security"
	officerDeniedLedger  = "region ledger: one is already alive, or the region is cooling down"
	officerDeniedNoClaim = "officer claims are unavailable"
)

// PickSquadTier draws a squad tier for a security by weight, not first match:
// several tiers may cover one band. Bands are inclusive at both ends, like
// GetSpawnerRuleForSecurity. A zero-weight row is never drawn; when nothing
// covers the security the built-in normal tier is returned with covered=false.
func PickSquadTier(tiers []db.SquadTierRow, security float64) (db.SquadTierRow, bool) {
	total := 0
	for _, t := range tiers {
		if security < t.SecMin || security > t.SecMax || t.Weight <= 0 {
			continue
		}
		total += t.Weight
	}
	if total <= 0 {
		return defaultNormalTier, false
	}
	roll := rand.Intn(total)
	for _, t := range tiers {
		if security < t.SecMin || security > t.SecMax || t.Weight <= 0 {
			continue
		}
		roll -= t.Weight
		if roll < 0 {
			return t, true
		}
	}
	// Unreachable while total is computed from the same predicate; kept so a
	// future edit to either loop cannot fall off the end.
	return defaultNormalTier, false
}

// rolePool filters the roster to rows of the given roles eligible at a
// security. An empty Role counts as "normal": one blank field in the region
// editor must not silently drop a listing out of every pool.
func rolePool(roster []db.RegionSpawnRow, security float64, roles ...string) []db.RegionSpawnRow {
	want := make(map[string]bool, len(roles))
	for _, r := range roles {
		want[r] = true
	}
	var out []db.RegionSpawnRow
	for _, r := range roster {
		if security > r.MaxSec+1e-9 {
			continue
		}
		role := r.Role
		if role == "" {
			role = "normal"
		}
		if want[role] {
			out = append(out, r)
		}
	}
	return out
}

// parseHullBias turns a tier's csv hull-class list into a set.
func parseHullBias(csv string) map[string]bool {
	if csv == "" {
		return nil
	}
	out := map[string]bool{}
	for _, part := range strings.Split(csv, ",") {
		if s := strings.ToLower(strings.TrimSpace(part)); s != "" {
			out[s] = true
		}
	}
	return out
}

// RollWave rolls one belt wave. See the file header for the three-part split.
func RollWave(in WaveInput) WaveOutput {
	var out WaveOutput
	tier, covered := PickSquadTier(in.Tiers, in.Security)
	out.Tier = tier
	out.TierFallback = !covered
	out.EffectiveSec = in.Security
	if tier.SecFloor > out.EffectiveSec {
		out.EffectiveSec = tier.SecFloor
	}

	if in.Rule == nil {
		return out
	}

	// -- base pool: normal and hauler, at the floored security --
	base := rolePool(in.Roster, out.EffectiveSec, "normal", "hauler")
	if len(base) == 0 {
		// The floor asked for hulls this region does not list that high. Fall
		// back to the true security rather than handing back an empty belt.
		out.EffectiveSec = in.Security
		base = rolePool(in.Roster, in.Security, "normal", "hauler")
	}
	if len(base) == 0 {
		return out
	}
	out.BasePoolSize = len(base)

	// -- how many --
	lo := in.Rule.MinShips + tier.SizeDeltaMin
	if lo < 0 {
		lo = 0
	}
	hi := in.Rule.MaxShips + tier.SizeDeltaMax
	if hi < lo {
		hi = lo
	}
	if cap := in.Cfg.WaveHardCap; cap > 0 && hi > cap {
		hi = cap
	}
	if lo > hi {
		lo = hi
	}
	n := lo
	if hi > lo {
		n += rand.Intn(hi - lo + 1)
	}

	bias := parseHullBias(tier.HullBias)
	out.Rows = make([]db.RegionSpawnRow, 0, n+1)
	for i := 0; i < n; i++ {
		out.Rows = append(out.Rows, weightedPickBiased(base, bias, in.Cfg.HullBiasMult))
	}

	// -- commander promotion: replaces a slot, never inflates the wave --
	// The tier's ship window stays the ceiling: a commander is a better rat, not an extra one.
	if in.Cfg.CommandersEnabled && in.Cfg.CommandersPerWave > 0 {
		// The commander pool is filtered at the TRUE security. It never sees
		// the tier's floor.
		cmdr := rolePool(in.Roster, in.Security, "commander")
		out.CmdrPoolSize = len(cmdr)
		if len(cmdr) > 0 {
			p := tier.CmdrChancePct * in.Cfg.CommanderChanceMult / 100
			promoted := 0
			for i := 0; i < in.Cfg.CommandersPerWave; i++ {
				if promoted >= len(out.Rows) && len(out.Rows) > 0 {
					break // every slot is already a commander
				}
				if rand.Float64() >= p {
					continue
				}
				pick := weightedPickBiased(cmdr, bias, in.Cfg.HullBiasMult)
				if len(out.Rows) == 0 {
					// An empty belt still earns its commander, or a 0-min high-sec rule would
					// swallow it.
					out.Rows = append(out.Rows, pick)
				} else {
					out.Rows[len(out.Rows)-1-promoted] = pick
				}
				promoted++
				out.HasCommander = true
			}
		}
	}

	// -- officer escalation: appends, and the region ledger has the last word --
	if in.Cfg.OfficersEnabled {
		p := tier.OfficerChancePct * in.Cfg.OfficerChanceMult / 100
		if p > 0 && rand.Float64() < p {
			off := rolePool(in.Roster, in.Security, "officer")
			out.OffPoolSize = len(off)
			if len(off) == 0 {
				out.OfficerDenied = officerDeniedNoPool
			} else {
				// Draw first, claim second: the ledger records which officer took the slot,
				// and a claim is never spent on a wave with nothing to place.
				pick := weightedPick(off)
				switch {
				case in.ClaimOfficer == nil:
					// Preview path: report what a claim would have done, take
					// nothing.
					if in.PeekOfficer == nil {
						out.OfficerDenied = officerDeniedNoClaim
						break
					}
					allowed, why := in.PeekOfficer()
					if !allowed {
						out.OfficerDenied = why
						break
					}
					out.Rows = append(out.Rows, pick)
					out.HasOfficer = true
				case in.ClaimOfficer(pick):
					out.Rows = append(out.Rows, pick)
					out.HasOfficer = true
				default:
					out.OfficerDenied = officerDeniedLedger
				}
			}
		}
	}

	return out
}

// weightedPick returns a random row from the pool, biased by Weight
// (rows with Weight <= 0 count as 1).
func weightedPick(pool []db.RegionSpawnRow) db.RegionSpawnRow {
	return weightedPickBiased(pool, nil, 1)
}

// weightedPickBiased is weightedPick with a tier's hull-class bias: a row whose
// ShipClass is in bias has its weight multiplied by mult. A nudge, not a gate.
func weightedPickBiased(pool []db.RegionSpawnRow, bias map[string]bool, mult int) db.RegionSpawnRow {
	if mult < 1 {
		mult = 1
	}
	weightOf := func(r db.RegionSpawnRow) int {
		w := r.Weight
		if w <= 0 {
			w = 1
		}
		if len(bias) > 0 && bias[strings.ToLower(r.ShipClass)] {
			w *= mult
		}
		return w
	}
	total := 0
	for _, r := range pool {
		total += weightOf(r)
	}
	roll := rand.Intn(total)
	for _, r := range pool {
		roll -= weightOf(r)
		if roll < 0 {
			return r
		}
	}
	return pool[len(pool)-1]
}
