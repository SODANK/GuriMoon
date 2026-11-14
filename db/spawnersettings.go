package db

import (
	"context"
	"database/sql"
	"strconv"
)

// ── Belt-spawn settings ──────────────────────────────────────────────────────
//
// The global dials over the per-tier numbers in npcSquadTiers: the tier table authors the
// texture, these scale it at once and hold the hard limits. The officer chances only decide how
// often one is asked for; the ledger decides whether it spawns.
// See docs/db-notes.md#squad-tiers-and-the-officer-ledger.

const (
	settingOfficersEnabled     = "spawner.officersEnabled"
	settingOfficerMaxPerRegion = "spawner.officerMaxAlivePerRegion"
	settingOfficerKillCooldown = "spawner.officerKillCooldownSec"
	settingOfficerChancePct    = "spawner.officerChancePct"
	settingCommandersEnabled   = "spawner.commandersEnabled"
	settingCommanderChancePct  = "spawner.commanderChancePct"
	settingCommandersPerWave   = "spawner.commandersPerWave"
	settingWaveHardCap         = "spawner.waveHardCap"
	settingTierHullBiasMult    = "spawner.tierHullBiasMult"
)

// Defaults. The two percent multipliers are 100 = "as the tier table authored
// it", so the shipped behaviour is whatever npcSquadTiers says and these are
// purely a live tuning dial.
const (
	DefaultOfficerMaxAlivePerRegion = 1
	DefaultOfficerKillCooldownSec   = 21600 // 6 hours
	DefaultSpawnChanceMultPct       = 100
	DefaultCommandersPerWave        = 1
	DefaultWaveHardCap              = 12
	DefaultTierHullBiasMult         = 3
)

// BeltSpawnSettings is every belt-spawn knob, read in one pass. Belt seeds run
// on a 5s ticker across every loaded system, so these are read once at boot
// into the spawner manager rather than per wave.
type BeltSpawnSettings struct {
	OfficersEnabled          bool
	OfficerMaxAlivePerRegion int
	OfficerKillCooldownSec   int
	OfficerChancePct         int // multiplier over the tier's own officerChancePct
	CommandersEnabled        bool
	CommanderChancePct       int // multiplier over the tier's own cmdrChancePct
	CommandersPerWave        int
	WaveHardCap              int
	TierHullBiasMult         int
}

// GetBeltSpawnSettings reads every belt-spawn setting, clamped to its range.
func GetBeltSpawnSettings(ctx context.Context, db *sql.DB) BeltSpawnSettings {
	return BeltSpawnSettings{
		OfficersEnabled:          settingInt(ctx, db, settingOfficersEnabled, 1, 0, 1) == 1,
		OfficerMaxAlivePerRegion: settingInt(ctx, db, settingOfficerMaxPerRegion, DefaultOfficerMaxAlivePerRegion, 0, 10),
		OfficerKillCooldownSec:   settingInt(ctx, db, settingOfficerKillCooldown, DefaultOfficerKillCooldownSec, 0, 604800),
		OfficerChancePct:         settingInt(ctx, db, settingOfficerChancePct, DefaultSpawnChanceMultPct, 0, 1000),
		CommandersEnabled:        settingInt(ctx, db, settingCommandersEnabled, 1, 0, 1) == 1,
		CommanderChancePct:       settingInt(ctx, db, settingCommanderChancePct, DefaultSpawnChanceMultPct, 0, 1000),
		CommandersPerWave:        settingInt(ctx, db, settingCommandersPerWave, DefaultCommandersPerWave, 0, 3),
		WaveHardCap:              settingInt(ctx, db, settingWaveHardCap, DefaultWaveHardCap, 1, 40),
		TierHullBiasMult:         settingInt(ctx, db, settingTierHullBiasMult, DefaultTierHullBiasMult, 1, 10),
	}
}

// SetBeltSpawnSettings persists every belt-spawn setting, clamped to the same
// ranges the getter reads them back through.
func SetBeltSpawnSettings(ctx context.Context, db *sql.DB, s BeltSpawnSettings) error {
	write := func(name string, v, min, max int) error {
		if v < min {
			v = min
		}
		if v > max {
			v = max
		}
		return SetServerSetting(ctx, db, name, strconv.Itoa(v))
	}
	boolVal := func(b bool) int {
		if b {
			return 1
		}
		return 0
	}
	pairs := []struct {
		name        string
		v, min, max int
	}{
		{settingOfficersEnabled, boolVal(s.OfficersEnabled), 0, 1},
		{settingOfficerMaxPerRegion, s.OfficerMaxAlivePerRegion, 0, 10},
		{settingOfficerKillCooldown, s.OfficerKillCooldownSec, 0, 604800},
		{settingOfficerChancePct, s.OfficerChancePct, 0, 1000},
		{settingCommandersEnabled, boolVal(s.CommandersEnabled), 0, 1},
		{settingCommanderChancePct, s.CommanderChancePct, 0, 1000},
		{settingCommandersPerWave, s.CommandersPerWave, 0, 3},
		{settingWaveHardCap, s.WaveHardCap, 1, 40},
		{settingTierHullBiasMult, s.TierHullBiasMult, 1, 10},
	}
	for _, p := range pairs {
		if err := write(p.name, p.v, p.min, p.max); err != nil {
			return err
		}
	}
	return nil
}
