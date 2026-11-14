// Moon resource pools: what a Moon Harvesting Array can pull from each moon.
// The whole galaxy is rolled at boot from one world seed (SeedAllMoons) and stored.
// A quantity is batches per cycle, 1 to 3: moonmining.py harvests
// min(harvesterQuality, quantity) * 100 and the Arrays are quality 1/2/3.
// Classes follow the basePrice ladder; highsec carries the lowest class only.
// See docs/starbase-pos-investigation.md#13-moon-mining-phase-6-first-slice-resource-pools--survey.
package db

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MoonGenVersion is stamped on every moonSurvey row. Bump it when the generator
// changes: the boot pass regenerates every non-edited moon whose row is older.
const MoonGenVersion = 2

// Settings keys (server_settings).
const (
	MoonSettingProfile        = "moons.profile"
	MoonSettingWorldSeed      = "moons.worldSeed"
	MoonSettingSurveyTimeMult = "moons.surveyTimeMult"
)

// Moon material classes, indexed 0..4 (R4 to R64). TypeIDs are the 20 group-427 "Moon
// Materials" (16645 does not exist in this dump). Order inside a row is the order the
// affinity tables below index into.
const (
	MoonClassR4 = iota
	MoonClassR8
	MoonClassR16
	MoonClassR32
	MoonClassR64
)

var moonClasses = [5][4]int64{
	{16633, 16634, 16635, 16636}, // Hydrocarbons, Atmospheric Gases, Evaporite Deposits, Silicates
	{16637, 16638, 16639, 16640}, // Tungsten, Titanium, Scandium, Cobalt
	{16641, 16642, 16643, 16644}, // Chromium, Vanadium, Cadmium, Platinum
	{16646, 16647, 16648, 16649}, // Mercury, Caesium, Hafnium, Technetium
	{16650, 16651, 16652, 16653}, // Dysprosium, Neodymium, Promethium, Thulium
}

// MoonClassNames are the display labels for the classes.
var MoonClassNames = [5]string{"R4", "R8", "R16", "R32", "R64"}

// MoonClassOf returns a moon material's class, or -1 for anything else.
func MoonClassOf(typeID int64) int {
	for c, row := range moonClasses {
		for _, t := range row {
			if t == typeID {
				return c
			}
		}
	}
	return -1
}

// MoonClassTypes returns the four materials of a class.
func MoonClassTypes(class int) []int64 {
	if class < 0 || class >= len(moonClasses) {
		return nil
	}
	return append([]int64(nil), moonClasses[class][:]...)
}

// IsMoonMaterial reports whether typeID is one of the 20 harvestable materials.
func IsMoonMaterial(typeID int64) bool { return MoonClassOf(typeID) >= 0 }

// Races, as the home-race axis of R8 affinity.
const (
	raceNone = iota
	raceAmarr
	raceCaldari
	raceGallente
	raceMinmatar
)

// moonFactionRace maps a sovereign or rat faction onto the empire whose
// technology its space feeds. Pirate factions inherit the empire they splintered
// from; minor empires the one they serve.
var moonFactionRace = map[int64]int{
	500001: raceCaldari,  // Caldari State
	500002: raceMinmatar, // Minmatar Republic
	500003: raceAmarr,    // Amarr Empire
	500004: raceGallente, // Gallente Federation
	500007: raceAmarr,    // Ammatar Mandate
	500008: raceAmarr,    // Khanid Kingdom
	500009: raceGallente, // The Syndicate
	500010: raceCaldari,  // Guristas Pirates
	500011: raceMinmatar, // Angel Cartel
	500012: raceAmarr,    // The Blood Raider Covenant
	500015: raceMinmatar, // Thukker Tribe
	500019: raceAmarr,    // Sansha's Nation
	500020: raceGallente, // Serpentis
}

// moonRaceR8 is the racial-carbide chain: the R8 metal each race's T2
// components are built from.
var moonRaceR8 = map[int]int64{
	raceAmarr:    16637, // Tungsten
	raceCaldari:  16638, // Titanium
	raceMinmatar: 16639, // Scandium
	raceGallente: 16640, // Cobalt
}

// moonRatR32 ties the rare metals to the rat faction of a region. Guristas to
// Technetium is the sourced pairing; the rest are authored.
var moonRatR32 = map[int64]int64{
	500010: 16649, // Guristas → Technetium
	500011: 16648, // Angel Cartel → Hafnium
	500020: 16647, // Serpentis → Caesium
	500012: 16646, // Blood Raiders → Mercury
	500019: 16646, // Sansha's Nation → Mercury
}

// Security bands, cut on true security like db/marketsec.go.
const (
	moonBandHigh = iota
	moonBandLow
	moonBandNullShallow
	moonBandNullDeep
)

// MoonBandNames are the display labels for the bands.
var MoonBandNames = [4]string{"High", "Low", "Null", "Deep null"}

func moonBand(sec float64) int {
	switch {
	case sec >= 0.45:
		return moonBandHigh
	case sec > 0.0:
		return moonBandLow
	case sec > -0.5:
		return moonBandNullShallow
	default:
		return moonBandNullDeep
	}
}

// MoonGenProfile is the set of knobs one generation runs under.
type MoonGenProfile struct {
	Name string
	// EmptyPct is the chance a moon holds nothing at all, per band.
	EmptyPct [4]float64
	// SecondR4Pct is the chance a non-empty moon holds a second R4.
	SecondR4Pct float64
	// R8Pct is the chance a non-empty moon holds an R8, per band. Each higher
	// class rolls at half the chance of the one below it.
	R8Pct [4]float64
	// Affinity is the weight of a favoured material inside its class (the
	// others weigh 1): racial R8, constellation R16, rat-faction R32.
	Affinity int
	// Abundance2Pct / Abundance3Pct are the chances a material line carries 2 or 3
	// batches per cycle instead of 1. Only Moon Harvesting Array II/III can take more
	// than one, so classic leaves them at zero.
	Abundance2Pct float64
	Abundance3Pct float64
}

// MoonProfiles are the presets the ESP panel offers.
var MoonProfiles = map[string]MoonGenProfile{
	"classic": {
		Name:        "classic",
		EmptyPct:    [4]float64{60, 40, 32, 28},
		SecondR4Pct: 35,
		// R64 stays rare in every band.
		R8Pct:    [4]float64{0, 5, 9, 11},
		Affinity: 4,
	},
	"generous": {
		Name:          "generous",
		EmptyPct:      [4]float64{30, 0, 0, 0},
		SecondR4Pct:   50,
		R8Pct:         [4]float64{0, 14, 24, 30},
		Affinity:      4,
		Abundance2Pct: 10,
		Abundance3Pct: 2,
	},
}

// DefaultMoonProfile is used when no profile has been chosen.
const DefaultMoonProfile = "classic"

// MoonProfile returns a preset by name, falling back to the default.
func MoonProfile(name string) MoonGenProfile {
	if p, ok := MoonProfiles[name]; ok {
		return p
	}
	return MoonProfiles[DefaultMoonProfile]
}

// MoonProfileNames lists the presets in display order.
func MoonProfileNames() []string { return []string{"classic", "generous"} }

// MoonResource is one material line of a moon's pool.
type MoonResource struct {
	TypeID   int64
	Quantity int64
}

// MoonGenInput is everything about a moon the generator reads.
type MoonGenInput struct {
	WorldSeed       uint64
	MoonID          int64
	Security        float64 // the system's true security
	ConstellationID int64
	SovFactionID    int64 // system → constellation → region factionID, 0 = none
	RatFactionID    int64 // the region's dominant belt-rat faction, 0 = unknown
	Rerolls         int64 // per-moon re-roll counter (ESP "re-roll this moon")
}

// moonRNG is splitmix64.
type moonRNG struct{ s uint64 }

func (r *moonRNG) next() uint64 {
	r.s += 0x9E3779B97F4A7C15
	z := r.s
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}

func (r *moonRNG) intn(n int64) int64 {
	if n <= 0 {
		return 0
	}
	return int64(r.next() % uint64(n))
}

// chance rolls true p times in 100; p may be fractional.
func (r *moonRNG) chance(p float64) bool {
	if p <= 0 {
		return false
	}
	return float64(r.next()%1_000_000) < p*10_000
}

func moonMix(a, b uint64) uint64 {
	r := moonRNG{s: a ^ (b * 0xD6E8FEB86659FD93)}
	r.next()
	return r.next()
}

// moonConstellationVein is the R16 material a constellation favours. Every moon
// in the constellation derives it from the same two numbers, so the vein needs
// no storage and no coordination.
func moonConstellationVein(worldSeed uint64, constellationID int64) int64 {
	h := moonMix(worldSeed^0x5EED16, uint64(constellationID))
	return moonClasses[MoonClassR16][h%4]
}

// moonHomeRace resolves the racial axis: the sovereign faction where there is
// one, else the parent race of the region's rats (player nullsec has no sov
// faction in the static map).
func moonHomeRace(in MoonGenInput) int {
	if r, ok := moonFactionRace[in.SovFactionID]; ok {
		return r
	}
	return moonFactionRace[in.RatFactionID]
}

// GenerateMoonResources computes a moon's pool. Pure: no DB, no clock, no global
// rand. The same input always yields the same rows, so a lazy fill racing the
// bulk seed writes what the seed would have.
func GenerateMoonResources(in MoonGenInput, prof MoonGenProfile) []MoonResource {
	rng := moonRNG{s: moonMix(in.WorldSeed, uint64(in.MoonID)) ^ uint64(in.Rerolls)*0xA24BAED4963EE407}
	band := moonBand(in.Security)

	if rng.chance(prof.EmptyPct[band]) {
		return nil
	}

	var out []MoonResource
	picked := map[int64]bool{}
	abundance := func() int64 {
		switch {
		case rng.chance(prof.Abundance3Pct):
			return 3
		case rng.chance(prof.Abundance2Pct):
			return 2
		}
		return 1
	}
	pick := func(class int, favoured int64) {
		row := moonClasses[class]
		var weights [4]int64
		var total int64
		for i, t := range row {
			w := int64(1)
			if t == favoured && prof.Affinity > 1 {
				w = int64(prof.Affinity)
			}
			if picked[t] {
				w = 0
			}
			weights[i] = w
			total += w
		}
		if total == 0 {
			return
		}
		roll := rng.intn(total)
		for i, w := range weights {
			if roll < w {
				picked[row[i]] = true
				out = append(out, MoonResource{TypeID: row[i], Quantity: abundance()})
				return
			}
			roll -= w
		}
	}

	pick(MoonClassR4, 0)
	if rng.chance(prof.SecondR4Pct) {
		pick(MoonClassR4, 0)
	}

	// Each rare class gets one independent roll at half the chance of the one
	// below it. Highsec's R8Pct is 0, so its moons are survey flavour only.
	p := prof.R8Pct[band]
	pick8 := moonRaceR8[moonHomeRace(in)]
	if rng.chance(p) {
		pick(MoonClassR8, pick8)
	}
	if rng.chance(p / 2) {
		pick(MoonClassR16, moonConstellationVein(in.WorldSeed, in.ConstellationID))
	}
	if rng.chance(p / 4) {
		pick(MoonClassR32, moonRatR32[in.RatFactionID])
	}
	if rng.chance(p / 8) {
		pick(MoonClassR64, 0)
	}
	return out
}

// ── Storage ──────────────────────────────────────────────────────────────────

var moonTablesOnce sync.Once
var moonTablesErr error

// ensureMoonTables creates both tables. A moonSurvey row marks a moon as rolled, so a
// survey row with no resource rows is an empty moon and no survey row is a moon never rolled.
func ensureMoonTables(ctx context.Context, db *sql.DB) error {
	moonTablesOnce.Do(func() {
		for _, q := range []string{`
			CREATE TABLE IF NOT EXISTS moonResources (
			  moonID   INT UNSIGNED NOT NULL,
			  typeID   INT UNSIGNED NOT NULL,
			  quantity INT UNSIGNED NOT NULL,
			  PRIMARY KEY (moonID, typeID),
			  KEY idx_moonres_type (typeID)
			)`, `
			CREATE TABLE IF NOT EXISTS moonSurvey (
			  moonID        INT UNSIGNED NOT NULL PRIMARY KEY,
			  solarSystemID INT UNSIGNED NOT NULL,
			  genVersion    INT NOT NULL,
			  worldSeed     BIGINT UNSIGNED NOT NULL,
			  profile       VARCHAR(16) NOT NULL,
			  edited        TINYINT NOT NULL DEFAULT 0,
			  rerolls       INT NOT NULL DEFAULT 0,
			  generatedAt   BIGINT NOT NULL,
			  KEY idx_moonsurvey_system (solarSystemID)
			)`} {
			if _, err := db.ExecContext(ctx, q); err != nil {
				moonTablesErr = fmt.Errorf("ensureMoonTables: %w", err)
				return
			}
		}
		// Older tables lack the type index the census and material filter use.
		var n int
		if err := db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM information_schema.statistics
			 WHERE table_schema = DATABASE() AND table_name = 'moonResources'
			   AND index_name = 'idx_moonres_type'`).Scan(&n); err == nil && n == 0 {
			// 1061 duplicate key: the index already exists on whatever table the name resolves
			// to (a session TEMPORARY shadow in tests).
			if _, err := db.ExecContext(ctx,
				`ALTER TABLE moonResources ADD KEY idx_moonres_type (typeID)`); err != nil &&
				!strings.Contains(err.Error(), "1061") {
				moonTablesErr = fmt.Errorf("ensureMoonTables index: %w", err)
			}
		}
	})
	return moonTablesErr
}

// MoonGenSettings is the world seed and profile new generations run under.
type MoonGenSettings struct {
	WorldSeed uint64
	Profile   string
}

// GetMoonGenSettings reads the world seed and profile, minting a seed the first
// time. The mint is INSERT IGNORE + re-read, so two racing first readers agree.
func GetMoonGenSettings(ctx context.Context, db *sql.DB) (MoonGenSettings, error) {
	out := MoonGenSettings{Profile: DefaultMoonProfile}
	if v, ok, err := GetServerSetting(ctx, db, MoonSettingProfile); err != nil {
		return out, err
	} else if ok {
		if _, known := MoonProfiles[v]; known {
			out.Profile = v
		}
	}
	v, ok, err := GetServerSetting(ctx, db, MoonSettingWorldSeed)
	if err != nil {
		return out, err
	}
	if !ok {
		if _, err := db.ExecContext(ctx,
			`INSERT IGNORE INTO server_settings (name, value) VALUES (?, ?)`,
			MoonSettingWorldSeed, strconv.FormatUint(RandomMoonSeed(), 10)); err != nil {
			return out, fmt.Errorf("GetMoonGenSettings mint seed: %w", err)
		}
		if v, _, err = GetServerSetting(ctx, db, MoonSettingWorldSeed); err != nil {
			return out, err
		}
	}
	seed, perr := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
	if perr != nil {
		return out, fmt.Errorf("GetMoonGenSettings: bad %s %q", MoonSettingWorldSeed, v)
	}
	out.WorldSeed = seed
	return out, nil
}

// SetMoonGenSettings stores a world seed and profile.
func SetMoonGenSettings(ctx context.Context, db *sql.DB, s MoonGenSettings) error {
	if _, ok := MoonProfiles[s.Profile]; !ok {
		return fmt.Errorf("unknown moon profile %q", s.Profile)
	}
	if err := SetServerSetting(ctx, db, MoonSettingProfile, s.Profile); err != nil {
		return err
	}
	return SetServerSetting(ctx, db, MoonSettingWorldSeed, strconv.FormatUint(s.WorldSeed, 10))
}

// RandomMoonSeed returns a fresh world seed.
func RandomMoonSeed() uint64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return uint64(time.Now().UnixNano())
	}
	// Kept below 2^53 so the number survives a round trip through a browser
	// form or a JSON client unchanged.
	return binary.LittleEndian.Uint64(b[:]) & (1<<53 - 1)
}

// moonInputSelect reads every input the generator needs for moons, one row per
// moon. The rat faction comes separately (loadRegionRatFactions) so a missing
// spawn table degrades to "no R32 affinity" rather than failing the seed.
const moonInputSelect = `
	SELECT d.itemID, d.solarSystemID, COALESCE(s.security, 0), s.constellationID, s.regionID,
	       COALESCE(s.factionID, c.factionID, r.factionID, 0)
	  FROM mapDenormalize d
	  JOIN mapSolarSystems s ON s.solarSystemID = d.solarSystemID
	  LEFT JOIN mapConstellations c ON c.constellationID = s.constellationID
	  LEFT JOIN mapRegions r ON r.regionID = s.regionID
	 WHERE d.groupID = 8`

type moonInputRow struct {
	MoonGenInput
	SystemID int64
	RegionID int64
}

// loadRegionRatFactions returns each region's dominant belt-rat faction from
// npcRegionSpawns (heaviest summed weight, lowest factionID on a tie). An error
// answers an empty map: R32 affinity is flavour, not a reason to fail a seed.
func loadRegionRatFactions(ctx context.Context, db *sql.DB) map[int64]int64 {
	out := map[int64]int64{}
	rows, err := db.QueryContext(ctx, `
		SELECT regionID, factionID, SUM(weight) w
		  FROM npcRegionSpawns WHERE factionID <> 0
		 GROUP BY regionID, factionID`)
	if err != nil {
		return out
	}
	defer rows.Close()
	best := map[int64]int64{}
	for rows.Next() {
		var region, faction, w int64
		if rows.Scan(&region, &faction, &w) != nil {
			continue
		}
		cur, seen := out[region]
		if !seen || w > best[region] || (w == best[region] && faction < cur) {
			out[region] = faction
			best[region] = w
		}
	}
	return out
}

func loadMoonInputs(ctx context.Context, db *sql.DB, where string, args ...any) ([]moonInputRow, error) {
	rats := loadRegionRatFactions(ctx, db)
	rows, err := db.QueryContext(ctx, moonInputSelect+where, args...)
	if err != nil {
		return nil, fmt.Errorf("loadMoonInputs: %w", err)
	}
	defer rows.Close()
	var out []moonInputRow
	for rows.Next() {
		var r moonInputRow
		if err := rows.Scan(&r.MoonID, &r.SystemID, &r.Security, &r.ConstellationID,
			&r.RegionID, &r.SovFactionID); err != nil {
			return nil, fmt.Errorf("loadMoonInputs scan: %w", err)
		}
		r.RatFactionID = rats[r.RegionID]
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetMoonResources returns a moon's pool, generating and storing it if the moon
// has never been rolled (the boot seed normally got there first). An empty
// slice is a legitimately empty moon. Errors if moonID is not a moon.
func GetMoonResources(ctx context.Context, db *sql.DB, moonID int64) ([]MoonResource, error) {
	if err := ensureMoonTables(ctx, db); err != nil {
		return nil, err
	}
	var one int
	err := db.QueryRowContext(ctx, `SELECT 1 FROM moonSurvey WHERE moonID = ?`, moonID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		if err := generateOneMoon(ctx, db, moonID, 0, false); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, fmt.Errorf("GetMoonResources survey: %w", err)
	}
	return readMoonResources(ctx, db, moonID)
}

func readMoonResources(ctx context.Context, db *sql.DB, moonID int64) ([]MoonResource, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT typeID, quantity FROM moonResources
		 WHERE moonID = ? ORDER BY typeID`, moonID)
	if err != nil {
		return nil, fmt.Errorf("GetMoonResources: %w", err)
	}
	defer rows.Close()
	var out []MoonResource
	for rows.Next() {
		var r MoonResource
		if err := rows.Scan(&r.TypeID, &r.Quantity); err != nil {
			return nil, fmt.Errorf("GetMoonResources scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// generateOneMoon rolls a single moon under the current settings. replace
// discards whatever the moon held (re-roll / restore); without it the writes are
// INSERT IGNORE, so a lazy fill racing the bulk seed loses harmlessly.
func generateOneMoon(ctx context.Context, db *sql.DB, moonID, rerolls int64, replace bool) error {
	set, err := GetMoonGenSettings(ctx, db)
	if err != nil {
		return err
	}
	in, err := loadMoonInputs(ctx, db, " AND d.itemID = ?", moonID)
	if err != nil {
		return err
	}
	if len(in) == 0 {
		return fmt.Errorf("GetMoonResources: %d is not a moon", moonID)
	}
	row := in[0]
	row.WorldSeed = set.WorldSeed
	row.Rerolls = rerolls
	gen := GenerateMoonResources(row.MoonGenInput, MoonProfile(set.Profile))

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("generateOneMoon begin: %w", err)
	}
	defer tx.Rollback()
	verb := "INSERT IGNORE"
	if replace {
		if _, err := tx.ExecContext(ctx, `DELETE FROM moonResources WHERE moonID = ?`, moonID); err != nil {
			return fmt.Errorf("generateOneMoon clear: %w", err)
		}
		verb = "REPLACE"
	}
	edited := 0
	if rerolls > 0 {
		edited = 1 // a re-rolled moon is a hand decision; keep it through a rescramble
	}
	if _, err := tx.ExecContext(ctx, verb+` INTO moonSurvey
		(moonID, solarSystemID, genVersion, worldSeed, profile, edited, rerolls, generatedAt)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		moonID, row.SystemID, MoonGenVersion, set.WorldSeed, set.Profile, edited, rerolls,
		time.Now().Unix()); err != nil {
		return fmt.Errorf("generateOneMoon survey: %w", err)
	}
	for _, r := range gen {
		if _, err := tx.ExecContext(ctx, `
			INSERT IGNORE INTO moonResources (moonID, typeID, quantity) VALUES (?, ?, ?)`,
			moonID, r.TypeID, r.Quantity); err != nil {
			return fmt.Errorf("generateOneMoon store: %w", err)
		}
	}
	return tx.Commit()
}

// MoonSeedOptions narrows a bulk generation.
type MoonSeedOptions struct {
	// KeepEdited leaves hand-edited (and individually re-rolled) moons alone.
	KeepEdited bool
	// OnlyStale regenerates only moons with no survey row at MoonGenVersion (the boot
	// pass). Without it every targeted moon is re-rolled.
	OnlyStale bool
	// SolarSystemID restricts the pass to one system (0 = the whole galaxy).
	SolarSystemID int64
}

// MoonSeedReport says what a bulk generation did.
type MoonSeedReport struct {
	Moons     int // moons (re)generated
	Empty     int // of which empty
	Resources int // material lines written
	Skipped   int // kept: edited, or already current
	Took      time.Duration
}

var moonSeedMu sync.Mutex

// SeedAllMoons rolls moons in bulk under the current settings, in one transaction: a
// harvester's reader sees either the old pool or the new one, never a half. Towers are
// not protected: a re-roll can remove the material a running harvester is set to, which
// then yields nothing (MoonHarvestersOffPool counts those).
func SeedAllMoons(ctx context.Context, db *sql.DB, opts MoonSeedOptions) (MoonSeedReport, error) {
	moonSeedMu.Lock()
	defer moonSeedMu.Unlock()
	started := time.Now()
	var rep MoonSeedReport
	if err := ensureMoonTables(ctx, db); err != nil {
		return rep, err
	}
	set, err := GetMoonGenSettings(ctx, db)
	if err != nil {
		return rep, err
	}
	prof := MoonProfile(set.Profile)

	where, args := "", []any{}
	if opts.SolarSystemID != 0 {
		where, args = " AND d.solarSystemID = ?", []any{opts.SolarSystemID}
	}
	inputs, err := loadMoonInputs(ctx, db, where, args...)
	if err != nil {
		return rep, err
	}

	// What to leave alone.
	keep := map[int64]bool{}
	if opts.KeepEdited || opts.OnlyStale {
		q := `SELECT moonID, edited, genVersion FROM moonSurvey`
		if opts.SolarSystemID != 0 {
			q += ` WHERE solarSystemID = ?`
		}
		rows, err := db.QueryContext(ctx, q, args...)
		if err != nil {
			return rep, fmt.Errorf("SeedAllMoons survey scan: %w", err)
		}
		for rows.Next() {
			var id, edited, ver int64
			if err := rows.Scan(&id, &edited, &ver); err != nil {
				rows.Close()
				return rep, fmt.Errorf("SeedAllMoons survey row: %w", err)
			}
			if (opts.KeepEdited && edited != 0) || (opts.OnlyStale && ver == MoonGenVersion) {
				keep[id] = true
			}
		}
		rows.Close()
	}

	var targets []moonInputRow
	for _, in := range inputs {
		if keep[in.MoonID] {
			rep.Skipped++
			continue
		}
		targets = append(targets, in)
	}
	if len(targets) == 0 {
		rep.Took = time.Since(started)
		return rep, nil
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return rep, fmt.Errorf("SeedAllMoons begin: %w", err)
	}
	defer tx.Rollback()

	whole := opts.SolarSystemID == 0 && len(keep) == 0
	if whole {
		// Every moon is being rewritten: one DELETE beats per-system IN-lists.
		if _, err := tx.ExecContext(ctx, `DELETE FROM moonResources`); err != nil {
			return rep, fmt.Errorf("SeedAllMoons clear resources: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM moonSurvey`); err != nil {
			return rep, fmt.Errorf("SeedAllMoons clear survey: %w", err)
		}
	} else {
		for start := 0; start < len(targets); start += moonSeedBatch {
			end := min(start+moonSeedBatch, len(targets))
			ids := make([]any, 0, end-start)
			for _, t := range targets[start:end] {
				ids = append(ids, t.MoonID)
			}
			ph := placeholders(len(ids))
			if _, err := tx.ExecContext(ctx, `DELETE FROM moonResources WHERE moonID IN (`+ph+`)`, ids...); err != nil {
				return rep, fmt.Errorf("SeedAllMoons clear resources: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM moonSurvey WHERE moonID IN (`+ph+`)`, ids...); err != nil {
				return rep, fmt.Errorf("SeedAllMoons clear survey: %w", err)
			}
		}
	}

	now := time.Now().Unix()
	var survey, res []any
	flush := func(force bool) error {
		if len(survey) > 0 && (force || len(survey) >= moonSeedBatch*8) {
			n := len(survey) / 8
			if _, err := tx.ExecContext(ctx, `INSERT INTO moonSurvey
				(moonID, solarSystemID, genVersion, worldSeed, profile, edited, rerolls, generatedAt)
				VALUES `+rowPlaceholders(n, 8), survey...); err != nil {
				return fmt.Errorf("SeedAllMoons survey insert: %w", err)
			}
			survey = survey[:0]
		}
		if len(res) > 0 && (force || len(res) >= moonSeedBatch*3) {
			n := len(res) / 3
			if _, err := tx.ExecContext(ctx, `INSERT INTO moonResources
				(moonID, typeID, quantity) VALUES `+rowPlaceholders(n, 3), res...); err != nil {
				return fmt.Errorf("SeedAllMoons resource insert: %w", err)
			}
			res = res[:0]
		}
		return nil
	}
	for _, t := range targets {
		t.WorldSeed = set.WorldSeed
		gen := GenerateMoonResources(t.MoonGenInput, prof)
		survey = append(survey, t.MoonID, t.SystemID, MoonGenVersion, set.WorldSeed, set.Profile, 0, 0, now)
		for _, r := range gen {
			res = append(res, t.MoonID, r.TypeID, r.Quantity)
		}
		rep.Moons++
		rep.Resources += len(gen)
		if len(gen) == 0 {
			rep.Empty++
		}
		if err := flush(false); err != nil {
			return rep, err
		}
	}
	if err := flush(true); err != nil {
		return rep, err
	}
	if err := tx.Commit(); err != nil {
		return rep, fmt.Errorf("SeedAllMoons commit: %w", err)
	}
	rep.Took = time.Since(started)
	return rep, nil
}

const moonSeedBatch = 2000

// rowPlaceholders builds "(?,?,…),(?,?,…)" for n rows of w columns.
func rowPlaceholders(n, w int) string {
	one := "(" + strings.TrimSuffix(strings.Repeat("?,", w), ",") + ")"
	return strings.TrimSuffix(strings.Repeat(one+",", n), ",")
}

// MoonSeedNeeded reports whether the boot pass has work: any moon without a
// current-version survey row that is not hand-edited.
func MoonSeedNeeded(ctx context.Context, db *sql.DB) (bool, error) {
	if err := ensureMoonTables(ctx, db); err != nil {
		return false, err
	}
	var moons, current int64
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mapDenormalize WHERE groupID = 8`).Scan(&moons); err != nil {
		return false, fmt.Errorf("MoonSeedNeeded moons: %w", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM moonSurvey WHERE genVersion = ? OR edited <> 0`,
		MoonGenVersion).Scan(&current); err != nil {
		return false, fmt.Errorf("MoonSeedNeeded survey: %w", err)
	}
	return current < moons, nil
}
