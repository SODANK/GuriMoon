package main

// The operator's dials over security status, held as atomic bit-patterns:
// they are read from every damage path, both death handlers and the module,
// and written by the module bridge while all of that runs.

import (
	"context"
	"database/sql"
	"log"
	"math"
	"strconv"
	"sync/atomic"

	evedb "gurimoon/db"
	"gurimoon/seclaw"
)

var (
	// Penalties, in percent of the distance to -10 (seclaw.LossFromGap).
	secAggroPenaltyPct    atomic.Uint64
	secShipKillPenaltyPct atomic.Uint64
	secPodKillPenaltyPct  atomic.Uint64
	secPodKillMutualMul   atomic.Uint64 // applied when the victim fought back
	secPoliceAggroPct     atomic.Uint64
	secPoliceKillPct      atomic.Uint64
	secAidPenaltyPct      atomic.Uint64
	secRatGainMultiplier  atomic.Uint64
	secRatGainFallback    atomic.Uint64
	secDecayPerHour       atomic.Uint64
	secDecayEnabled       atomic.Bool
	// Taking from another pilot's can or wreck aggresses the owner and their corp.
	secLootTheftAggro atomic.Bool
)

func secStoreF(a *atomic.Uint64, v float64) { a.Store(math.Float64bits(v)) }
func secLoadF(a *atomic.Uint64) float64     { return math.Float64frombits(a.Load()) }

// Live accessors. Everything downstream reads through these, so a setting
// change takes effect on the next charge rather than the next restart.
func aggressionPenaltyPct() float64  { return secLoadF(&secAggroPenaltyPct) }
func shipKillPenaltyPct() float64    { return secLoadF(&secShipKillPenaltyPct) }
func podKillPenaltyPct() float64     { return secLoadF(&secPodKillPenaltyPct) }
func podKillMutualMul() float64      { return secLoadF(&secPodKillMutualMul) }
func policeAggroPenaltyPct() float64 { return secLoadF(&secPoliceAggroPct) }
func policeKillPenaltyPct() float64  { return secLoadF(&secPoliceKillPct) }
func aidPenaltyPct() float64         { return secLoadF(&secAidPenaltyPct) }
func ratGainMultiplier() float64     { return secLoadF(&secRatGainMultiplier) }
func ratGainFallback() float64       { return secLoadF(&secRatGainFallback) }
func decayPerHour() float64          { return secLoadF(&secDecayPerHour) }
func decayEnabled() bool             { return secDecayEnabled.Load() }
func lootTheftAggroEnabled() bool    { return secLootTheftAggro.Load() }

// Setting keys, so an admin surface and the loader cannot disagree about spelling.
const (
	SettingAggressionPenaltyPct = "security.aggressionPenaltyPct"
	SettingShipKillPenaltyPct   = "security.shipKillPenaltyPct"
	SettingPodKillPenaltyPct    = "security.podKillPenaltyPct"
	SettingPodKillMutualMul     = "security.podKillMutualMul"
	SettingPoliceAggroPct       = "security.policeAggressionPenaltyPct"
	SettingPoliceKillPct        = "security.policeKillPenaltyPct"
	SettingAidPenaltyPct        = "security.aidPenaltyPct"
	SettingRatGainMultiplier    = "security.ratGainMultiplier"
	SettingRatGainFallback      = "security.ratGainFallback"
	SettingSecDecayEnabled      = "security.decayEnabled"
	SettingSecDecayPerHour      = "security.decayPerHour"
	SettingLootTheftAggression  = "security.lootTheftAggression"
)

// A pod kill on someone who shot at you first is still a pod kill, but it is
// the end of a fight rather than an execution. Half.
const defaultPodKillMutualMul = 0.5

// securitySettingsSnapshot is the live dials, keyed by their setting name, for
// an admin surface to render. Read through the same accessors everything else
// reads, so the panel cannot show a value the law is not using.
func securitySettingsSnapshot() map[string]float64 {
	b2f := func(b bool) float64 {
		if b {
			return 1
		}
		return 0
	}
	return map[string]float64{
		SettingLootTheftAggression:  b2f(lootTheftAggroEnabled()),
		SettingAggressionPenaltyPct: aggressionPenaltyPct(),
		SettingShipKillPenaltyPct:   shipKillPenaltyPct(),
		SettingPodKillPenaltyPct:    podKillPenaltyPct(),
		SettingPodKillMutualMul:     podKillMutualMul(),
		SettingPoliceAggroPct:       policeAggroPenaltyPct(),
		SettingPoliceKillPct:        policeKillPenaltyPct(),
		SettingAidPenaltyPct:        aidPenaltyPct(),
		SettingRatGainMultiplier:    ratGainMultiplier(),
		SettingRatGainFallback:      ratGainFallback(),
		SettingSecDecayPerHour:      decayPerHour(),
		SettingSecDecayEnabled:      b2f(decayEnabled()),
	}
}

// loadSecuritySettings seeds the defaults, then reads the dials. A percent is
// refused outside [0, 100]: a negative one would pay the pilot for the crime.
func loadSecuritySettings(ctx context.Context, sqldb *sql.DB) {
	// Seed the defaults first, so a DB read that fails leaves working law
	// rather than a shard where every kill is free.
	secStoreF(&secAggroPenaltyPct, seclaw.DefaultAggressionPenaltyPct)
	secStoreF(&secShipKillPenaltyPct, seclaw.DefaultShipKillPenaltyPct)
	secStoreF(&secPodKillPenaltyPct, seclaw.DefaultPodKillPenaltyPct)
	secStoreF(&secPodKillMutualMul, defaultPodKillMutualMul)
	secStoreF(&secPoliceAggroPct, seclaw.DefaultPoliceAggressionPenaltyPct)
	secStoreF(&secPoliceKillPct, seclaw.DefaultPoliceKillPenaltyPct)
	secStoreF(&secAidPenaltyPct, seclaw.DefaultAidPenaltyPct)
	secStoreF(&secRatGainMultiplier, 1.0)
	secStoreF(&secRatGainFallback, seclaw.DefaultRatGainFallback)
	secStoreF(&secDecayPerHour, seclaw.DefaultDecayPerHour)
	// Passive decay is off: a negative record is worked back up by killing rats.
	secDecayEnabled.Store(false)
	secLootTheftAggro.Store(true)

	if sqldb == nil {
		return
	}
	get := func(key string) (float64, bool) {
		s, ok, err := evedb.GetServerSetting(ctx, sqldb, key)
		if err != nil {
			log.Printf("[secstatus] setting %s: %v", key, err)
			return 0, false
		}
		if !ok {
			return 0, false
		}
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			log.Printf("[secstatus] setting %s = %q is not a number", key, s)
			return 0, false
		}
		return v, true
	}
	setRange := func(a *atomic.Uint64, key string, lo, hi float64) {
		v, ok := get(key)
		if !ok {
			return
		}
		if v < lo || v > hi {
			log.Printf("[secstatus] setting %s = %+.3f out of range [%.2f, %.2f] — ignored, keeping %+.3f",
				key, v, lo, hi, secLoadF(a))
			return
		}
		secStoreF(a, v)
	}

	setRange(&secAggroPenaltyPct, SettingAggressionPenaltyPct, 0, 100)
	setRange(&secShipKillPenaltyPct, SettingShipKillPenaltyPct, 0, 100)
	setRange(&secPodKillPenaltyPct, SettingPodKillPenaltyPct, 0, 100)
	setRange(&secPodKillMutualMul, SettingPodKillMutualMul, 0, 1)
	setRange(&secPoliceAggroPct, SettingPoliceAggroPct, 0, 100)
	setRange(&secPoliceKillPct, SettingPoliceKillPct, 0, 100)
	setRange(&secAidPenaltyPct, SettingAidPenaltyPct, 0, 100)
	setRange(&secRatGainMultiplier, SettingRatGainMultiplier, 0, 100)
	setRange(&secRatGainFallback, SettingRatGainFallback, 0, 5)
	setRange(&secDecayPerHour, SettingSecDecayPerHour, 0, 10)

	if v, ok := get(SettingSecDecayEnabled); ok {
		secDecayEnabled.Store(v != 0)
	}
	if v, ok := get(SettingLootTheftAggression); ok {
		secLootTheftAggro.Store(v != 0)
	}

	log.Printf("[secstatus] law loaded (%% of gap to -10): aggression %.2f, ship %.2f, pod %.2f (×%.2f mutual), "+
		"police %.2f aggro / %.2f kill, aid %.2f, rat gain ×%.2f (fallback %.3f), decay %+.3f/h enabled=%v, "+
		"loot theft aggression=%v",
		aggressionPenaltyPct(), shipKillPenaltyPct(), podKillPenaltyPct(), podKillMutualMul(),
		policeAggroPenaltyPct(), policeKillPenaltyPct(), aidPenaltyPct(), ratGainMultiplier(), ratGainFallback(),
		decayPerHour(), decayEnabled(), lootTheftAggroEnabled())
}
