// ESP Industry, Moons: the read side (census, region matrix, search, one moon's detail)
// and the hand-edit side of the moon resource pools.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// MoonMaterialInfo is one of the 20 materials with its display data.
type MoonMaterialInfo struct {
	TypeID int64
	Name   string
	Class  int
}

// MoonMaterials returns the 20 materials in class order, named from invTypes.
func MoonMaterials(ctx context.Context, db *sql.DB) ([]MoonMaterialInfo, error) {
	names := map[int64]string{}
	rows, err := db.QueryContext(ctx, `SELECT typeID, typeName FROM invTypes WHERE groupID = 427`)
	if err != nil {
		return nil, fmt.Errorf("MoonMaterials: %w", err)
	}
	for rows.Next() {
		var id int64
		var n string
		if rows.Scan(&id, &n) == nil {
			names[id] = n
		}
	}
	rows.Close()
	var out []MoonMaterialInfo
	for c, row := range moonClasses {
		for _, t := range row {
			n := names[t]
			if n == "" {
				n = fmt.Sprintf("type %d", t)
			}
			out = append(out, MoonMaterialInfo{TypeID: t, Name: n, Class: c})
		}
	}
	return out, nil
}

// MoonCensusRow is one material's spread across the galaxy.
type MoonCensusRow struct {
	MoonMaterialInfo
	Moons, High, Low, Null int64
	Abund2, Abund3         int64
}

// MoonCensus is the front page's state panel and material table.
type MoonCensus struct {
	Settings     MoonGenSettings
	GenVersion   int
	LastGen      int64 // unix, newest generatedAt
	TotalMoons   int64 // moons in the map
	Surveyed     int64 // moons with a survey row
	Stale        int64 // survey rows at an older genVersion
	Empty        int64
	Edited       int64
	WithClass    [5]int64 // moons holding at least one material of the class
	Towers       int64    // anchored towers on a moon
	OffPool      int64    // harvesters set to a material their moon lacks
	Materials    []MoonCensusRow
	ClassTotals  [5]int64 // material lines per class
	BandMoons    [4]int64 // surveyed moons per band (High, Low, Null, Deep)
	BandNonEmpty [4]int64
	BandEmpty    [4]int64
}

// GetMoonCensus computes the whole front panel; every query is one pass over moonResources or moonSurvey.
func GetMoonCensus(ctx context.Context, db *sql.DB) (*MoonCensus, error) {
	if err := ensureMoonTables(ctx, db); err != nil {
		return nil, err
	}
	c := &MoonCensus{GenVersion: MoonGenVersion}
	set, err := GetMoonGenSettings(ctx, db)
	if err != nil {
		return nil, err
	}
	c.Settings = set

	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mapDenormalize WHERE groupID = 8`).Scan(&c.TotalMoons); err != nil {
		return nil, fmt.Errorf("GetMoonCensus moons: %w", err)
	}
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(genVersion <> ?), 0), COALESCE(SUM(edited <> 0), 0),
		       COALESCE(MAX(generatedAt), 0)
		  FROM moonSurvey`, MoonGenVersion).Scan(&c.Surveyed, &c.Stale, &c.Edited, &c.LastGen); err != nil {
		return nil, fmt.Errorf("GetMoonCensus survey: %w", err)
	}

	// Per band: surveyed moons and how many hold anything.
	rows, err := db.QueryContext(ctx, `
		SELECT s.security, (r.moonID IS NOT NULL)
		  FROM moonSurvey ms
		  JOIN mapSolarSystems s ON s.solarSystemID = ms.solarSystemID
		  LEFT JOIN (SELECT DISTINCT moonID FROM moonResources) r ON r.moonID = ms.moonID`)
	if err != nil {
		return nil, fmt.Errorf("GetMoonCensus bands: %w", err)
	}
	for rows.Next() {
		var sec float64
		var has bool
		if rows.Scan(&sec, &has) != nil {
			continue
		}
		b := moonBand(sec)
		c.BandMoons[b]++
		if has {
			c.BandNonEmpty[b]++
		} else {
			c.Empty++
			c.BandEmpty[b]++
		}
	}
	rows.Close()

	mats, err := MoonMaterials(ctx, db)
	if err != nil {
		return nil, err
	}
	byType := map[int64]*MoonCensusRow{}
	for _, m := range mats {
		c.Materials = append(c.Materials, MoonCensusRow{MoonMaterialInfo: m})
	}
	for i := range c.Materials {
		byType[c.Materials[i].TypeID] = &c.Materials[i]
	}
	rows, err = db.QueryContext(ctx, `
		SELECT r.typeID, COUNT(*),
		       COALESCE(SUM(s.security >= 0.45), 0),
		       COALESCE(SUM(s.security > 0 AND s.security < 0.45), 0),
		       COALESCE(SUM(s.security <= 0), 0),
		       COALESCE(SUM(r.quantity = 2), 0),
		       COALESCE(SUM(r.quantity >= 3), 0)
		  FROM moonResources r
		  JOIN moonSurvey ms ON ms.moonID = r.moonID
		  JOIN mapSolarSystems s ON s.solarSystemID = ms.solarSystemID
		 GROUP BY r.typeID`)
	if err != nil {
		return nil, fmt.Errorf("GetMoonCensus materials: %w", err)
	}
	for rows.Next() {
		var id int64
		var r MoonCensusRow
		if rows.Scan(&id, &r.Moons, &r.High, &r.Low, &r.Null, &r.Abund2, &r.Abund3) != nil {
			continue
		}
		if dst, ok := byType[id]; ok {
			dst.Moons, dst.High, dst.Low, dst.Null, dst.Abund2, dst.Abund3 =
				r.Moons, r.High, r.Low, r.Null, r.Abund2, r.Abund3
			c.ClassTotals[dst.Class] += r.Moons
		}
	}
	rows.Close()
	// One slot per rare class, so a class's line count is its moon count. R4 can hold two
	// lines; any non-empty moon holds at least one.
	c.WithClass = c.ClassTotals
	c.WithClass[MoonClassR4] = c.Surveyed - c.Empty

	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM starbase WHERE moonID <> 0`).Scan(&c.Towers)
	c.OffPool, _ = MoonHarvestersOffPool(ctx, db)
	return c, nil
}

// MoonHarvestersOffPool counts harvesters set to a material their tower's moon does
// not hold, what a rescramble leaves behind.
func MoonHarvestersOffPool(ctx context.Context, db *sql.DB) (int64, error) {
	var n int64
	err := db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		  FROM starbaseStructures ss
		  JOIN starbase sb ON sb.entityID = ss.towerID
		  JOIN entity e ON e.itemID = ss.entityID
		  JOIN invTypes t ON t.typeID = e.typeID AND t.groupID = 416
		 WHERE ss.provisionTypeID <> 0
		   AND NOT EXISTS (SELECT 1 FROM moonResources r
		                    WHERE r.moonID = sb.moonID AND r.typeID = ss.provisionTypeID)`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("MoonHarvestersOffPool: %w", err)
	}
	return n, nil
}

// MoonRegionRow is one region of the region by class matrix.
type MoonRegionRow struct {
	RegionID   int64
	RegionName string
	MinSec     float64
	MaxSec     float64
	Moons      int64
	Empty      int64
	Class      [5]int64 // moons holding the class
	TopR8      string
	TopR16     string
	TopR32     string
	TopR64     string
	RatFaction string
}

// GetMoonRegionMatrix summarises every region's moons by class.
func GetMoonRegionMatrix(ctx context.Context, db *sql.DB) ([]MoonRegionRow, error) {
	if err := ensureMoonTables(ctx, db); err != nil {
		return nil, err
	}
	byID := map[int64]*MoonRegionRow{}
	rows, err := db.QueryContext(ctx, `
		SELECT s.regionID, COALESCE(rg.regionName, ''), MIN(s.security), MAX(s.security),
		       COUNT(*), COALESCE(SUM(r.moonID IS NULL), 0)
		  FROM moonSurvey ms
		  JOIN mapSolarSystems s ON s.solarSystemID = ms.solarSystemID
		  LEFT JOIN mapRegions rg ON rg.regionID = s.regionID
		  LEFT JOIN (SELECT DISTINCT moonID FROM moonResources) r ON r.moonID = ms.moonID
		 GROUP BY s.regionID, rg.regionName`)
	if err != nil {
		return nil, fmt.Errorf("GetMoonRegionMatrix: %w", err)
	}
	for rows.Next() {
		r := &MoonRegionRow{}
		if rows.Scan(&r.RegionID, &r.RegionName, &r.MinSec, &r.MaxSec, &r.Moons, &r.Empty) == nil {
			byID[r.RegionID] = r
		}
	}
	rows.Close()

	names := map[int64]string{}
	if mats, err := MoonMaterials(ctx, db); err == nil {
		for _, m := range mats {
			names[m.TypeID] = m.Name
		}
	}
	type top struct {
		n  int64
		id int64
	}
	tops := map[int64]*[5]top{}
	rows, err = db.QueryContext(ctx, `
		SELECT s.regionID, r.typeID, COUNT(*)
		  FROM moonResources r
		  JOIN moonSurvey ms ON ms.moonID = r.moonID
		  JOIN mapSolarSystems s ON s.solarSystemID = ms.solarSystemID
		 GROUP BY s.regionID, r.typeID`)
	if err != nil {
		return nil, fmt.Errorf("GetMoonRegionMatrix materials: %w", err)
	}
	for rows.Next() {
		var region, typeID, n int64
		if rows.Scan(&region, &typeID, &n) != nil {
			continue
		}
		rr := byID[region]
		cls := MoonClassOf(typeID)
		if rr == nil || cls < 0 {
			continue
		}
		if cls != MoonClassR4 {
			rr.Class[cls] += n
		}
		t := tops[region]
		if t == nil {
			t = &[5]top{}
			tops[region] = t
		}
		if n > t[cls].n || (n == t[cls].n && typeID < t[cls].id) {
			t[cls] = top{n, typeID}
		}
	}
	rows.Close()

	rats := loadRegionRatFactions(ctx, db)
	factionNames := map[int64]string{}
	if fr, err := db.QueryContext(ctx, `SELECT factionID, factionName FROM chrFactions`); err == nil {
		for fr.Next() {
			var id int64
			var n string
			if fr.Scan(&id, &n) == nil {
				factionNames[id] = n
			}
		}
		fr.Close()
	}

	out := make([]MoonRegionRow, 0, len(byID))
	for id, r := range byID {
		r.Class[MoonClassR4] = r.Moons - r.Empty
		if t := tops[id]; t != nil {
			r.TopR8, r.TopR16, r.TopR32, r.TopR64 = names[t[1].id], names[t[2].id], names[t[3].id], names[t[4].id]
		}
		r.RatFaction = factionNames[rats[id]]
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RegionName < out[j].RegionName })
	return out, nil
}

// MoonListFilter narrows the moon list. Zero values mean "any".
type MoonListFilter struct {
	System   string // system name prefix or numeric ID
	RegionID int64
	TypeID   int64
	MinClass int // -1 = any; else at least one material of this class or higher
	Band     int // -1 = any; else a moonBand index
	Towers   bool
	Empty    bool // only empty moons
	Page     int
}

// MoonListRow is one moon in the list.
type MoonListRow struct {
	MoonID     int64
	MoonName   string
	SystemID   int64
	SystemName string
	Security   float64
	RegionName string
	Edited     bool
	Resources  []MoonResourceView
	TowerID    int64
	TowerOwner string
}

// MoonResourceView is a material line with its display data.
type MoonResourceView struct {
	TypeID   int64
	Name     string
	Class    int
	Quantity int64
}

// MoonListPageSize is rows per page.
const MoonListPageSize = 100

// HasFilter reports whether the filter narrows anything; an unfiltered request shows
// no list.
func (f MoonListFilter) HasFilter() bool {
	return f.System != "" || f.RegionID != 0 || f.TypeID != 0 || f.MinClass >= 0 ||
		f.Band >= 0 || f.Towers || f.Empty
}

// ListMoons returns a page of moons matching f, plus the total match count.
func ListMoons(ctx context.Context, db *sql.DB, f MoonListFilter) ([]MoonListRow, int64, error) {
	if err := ensureMoonTables(ctx, db); err != nil {
		return nil, 0, err
	}
	var where []string
	var args []any
	if f.System != "" {
		var id int64
		if _, err := fmt.Sscan(f.System, &id); err == nil && id > 0 {
			where = append(where, "s.solarSystemID = ?")
			args = append(args, id)
		} else {
			where = append(where, "s.solarSystemName LIKE ?")
			args = append(args, f.System+"%")
		}
	}
	if f.RegionID != 0 {
		where = append(where, "s.regionID = ?")
		args = append(args, f.RegionID)
	}
	if f.TypeID != 0 {
		where = append(where, "EXISTS (SELECT 1 FROM moonResources x WHERE x.moonID = ms.moonID AND x.typeID = ?)")
		args = append(args, f.TypeID)
	}
	if f.MinClass == 0 {
		where = append(where, "EXISTS (SELECT 1 FROM moonResources x WHERE x.moonID = ms.moonID)")
	} else if f.MinClass > 0 && f.MinClass < len(moonClasses) {
		var ids []string
		for c := f.MinClass; c < len(moonClasses); c++ {
			for _, t := range moonClasses[c] {
				ids = append(ids, fmt.Sprint(t))
			}
		}
		where = append(where, "EXISTS (SELECT 1 FROM moonResources x WHERE x.moonID = ms.moonID AND x.typeID IN ("+
			strings.Join(ids, ",")+"))")
	}
	switch f.Band {
	case moonBandHigh:
		where = append(where, "s.security >= 0.45")
	case moonBandLow:
		where = append(where, "s.security > 0 AND s.security < 0.45")
	case moonBandNullShallow:
		where = append(where, "s.security <= 0 AND s.security > -0.5")
	case moonBandNullDeep:
		where = append(where, "s.security <= -0.5")
	}
	if f.Towers {
		where = append(where, "sb.entityID IS NOT NULL")
	}
	if f.Empty {
		where = append(where, "NOT EXISTS (SELECT 1 FROM moonResources x WHERE x.moonID = ms.moonID)")
	}
	cond := ""
	if len(where) > 0 {
		cond = " WHERE " + strings.Join(where, " AND ")
	}
	from := `
		  FROM moonSurvey ms
		  JOIN mapDenormalize d ON d.itemID = ms.moonID
		  JOIN mapSolarSystems s ON s.solarSystemID = ms.solarSystemID
		  LEFT JOIN mapRegions rg ON rg.regionID = s.regionID
		  LEFT JOIN starbase sb ON sb.moonID = ms.moonID
		  LEFT JOIN corporation co ON co.corporationID = sb.ownerCorpID`

	var total int64
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*)"+from+cond, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("ListMoons count: %w", err)
	}
	page := max(f.Page, 0)
	q := `SELECT ms.moonID, COALESCE(d.itemName, ''), s.solarSystemID, COALESCE(s.solarSystemName, ''),
	             COALESCE(s.security, 0), COALESCE(rg.regionName, ''), ms.edited,
	             COALESCE(sb.entityID, 0), COALESCE(co.corporationName, '')` + from + cond +
		` ORDER BY rg.regionName, s.solarSystemName, ms.moonID LIMIT ? OFFSET ?`
	rows, err := db.QueryContext(ctx, q, append(args, MoonListPageSize, page*MoonListPageSize)...)
	if err != nil {
		return nil, 0, fmt.Errorf("ListMoons: %w", err)
	}
	var out []MoonListRow
	idx := map[int64]int{}
	for rows.Next() {
		var r MoonListRow
		var edited int
		if err := rows.Scan(&r.MoonID, &r.MoonName, &r.SystemID, &r.SystemName, &r.Security,
			&r.RegionName, &edited, &r.TowerID, &r.TowerOwner); err != nil {
			rows.Close()
			return nil, 0, fmt.Errorf("ListMoons scan: %w", err)
		}
		r.Edited = edited != 0
		idx[r.MoonID] = len(out)
		out = append(out, r)
	}
	rows.Close()
	if len(out) == 0 {
		return out, total, nil
	}
	ids := make([]any, 0, len(out))
	for _, r := range out {
		ids = append(ids, r.MoonID)
	}
	views, err := moonResourceViews(ctx, db, ids)
	if err != nil {
		return nil, 0, err
	}
	for id, v := range views {
		out[idx[id]].Resources = v
	}
	return out, total, nil
}

func moonResourceViews(ctx context.Context, db *sql.DB, ids []any) (map[int64][]MoonResourceView, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT r.moonID, r.typeID, COALESCE(t.typeName, ''), r.quantity
		  FROM moonResources r LEFT JOIN invTypes t ON t.typeID = r.typeID
		 WHERE r.moonID IN (`+placeholders(len(ids))+`)`, ids...)
	if err != nil {
		return nil, fmt.Errorf("moonResourceViews: %w", err)
	}
	defer rows.Close()
	out := map[int64][]MoonResourceView{}
	for rows.Next() {
		var moonID int64
		var v MoonResourceView
		if err := rows.Scan(&moonID, &v.TypeID, &v.Name, &v.Quantity); err != nil {
			return nil, fmt.Errorf("moonResourceViews scan: %w", err)
		}
		v.Class = MoonClassOf(v.TypeID)
		out[moonID] = append(out[moonID], v)
	}
	for id := range out {
		s := out[id]
		sort.Slice(s, func(i, j int) bool {
			if s[i].Class != s[j].Class {
				return s[i].Class > s[j].Class
			}
			return s[i].TypeID < s[j].TypeID
		})
	}
	return out, rows.Err()
}

// MoonHarvesterView is one harvester on a moon's tower.
type MoonHarvesterView struct {
	EntityID     int64
	TypeName     string
	State        string
	MaterialID   int64
	MaterialName string
	OffPool      bool // set to a material the moon does not hold
}

// MoonDetail is one moon's page.
type MoonDetail struct {
	MoonID            int64
	MoonName          string
	SystemID          int64
	SystemName        string
	Security          float64
	ConstellationName string
	RegionName        string
	SovFaction        string
	RatFaction        string
	HomeR8            string // the R8 this moon's race favours
	VeinR16           string // the R16 this moon's constellation favours
	RatR32            string // the R32 this moon's rat faction favours
	Surveyed          bool
	GenVersion        int
	WorldSeed         uint64
	Profile           string
	Edited            bool
	Rerolls           int64
	GeneratedAt       int64
	Resources         []MoonResourceView
	TowerID           int64
	TowerType         string
	TowerOwner        string
	TowerState        string
	Harvesters        []MoonHarvesterView
}

// GetMoonDetail returns one moon's page, generating its pool if it has none.
func GetMoonDetail(ctx context.Context, db *sql.DB, moonID int64) (*MoonDetail, error) {
	if _, err := GetMoonResources(ctx, db, moonID); err != nil {
		return nil, err
	}
	in, err := loadMoonInputs(ctx, db, " AND d.itemID = ?", moonID)
	if err != nil || len(in) == 0 {
		return nil, fmt.Errorf("moon %d not found", moonID)
	}
	d := &MoonDetail{MoonID: moonID, SystemID: in[0].SystemID, Security: in[0].Security}
	var constID, regionID int64
	_ = db.QueryRowContext(ctx, `
		SELECT COALESCE(dn.itemName, ''), COALESCE(s.solarSystemName, ''), s.constellationID, s.regionID,
		       COALESCE(c.constellationName, ''), COALESCE(rg.regionName, '')
		  FROM mapDenormalize dn
		  JOIN mapSolarSystems s ON s.solarSystemID = dn.solarSystemID
		  LEFT JOIN mapConstellations c ON c.constellationID = s.constellationID
		  LEFT JOIN mapRegions rg ON rg.regionID = s.regionID
		 WHERE dn.itemID = ?`, moonID).Scan(&d.MoonName, &d.SystemName, &constID, &regionID,
		&d.ConstellationName, &d.RegionName)

	var edited int
	if err := db.QueryRowContext(ctx, `
		SELECT genVersion, worldSeed, profile, edited, rerolls, generatedAt
		  FROM moonSurvey WHERE moonID = ?`, moonID).Scan(&d.GenVersion, &d.WorldSeed, &d.Profile,
		&edited, &d.Rerolls, &d.GeneratedAt); err == nil {
		d.Surveyed = true
		d.Edited = edited != 0
	}

	names := map[int64]string{}
	if mats, err := MoonMaterials(ctx, db); err == nil {
		for _, m := range mats {
			names[m.TypeID] = m.Name
		}
	}
	factionName := func(id int64) string {
		if id == 0 {
			return ""
		}
		var n string
		_ = db.QueryRowContext(ctx, `SELECT factionName FROM chrFactions WHERE factionID = ?`, id).Scan(&n)
		return n
	}
	d.SovFaction = factionName(in[0].SovFactionID)
	d.RatFaction = factionName(in[0].RatFactionID)
	set, _ := GetMoonGenSettings(ctx, db)
	seed := d.WorldSeed
	if !d.Surveyed {
		seed = set.WorldSeed
	}
	d.HomeR8 = names[moonRaceR8[moonHomeRace(in[0].MoonGenInput)]]
	d.VeinR16 = names[moonConstellationVein(seed, constID)]
	d.RatR32 = names[moonRatR32[in[0].RatFactionID]]

	views, err := moonResourceViews(ctx, db, []any{moonID})
	if err != nil {
		return nil, err
	}
	d.Resources = views[moonID]
	has := map[int64]bool{}
	for _, r := range d.Resources {
		has[r.TypeID] = true
	}

	_ = db.QueryRowContext(ctx, `
		SELECT sb.entityID, COALESCE(t.typeName, ''), COALESCE(co.corporationName, ''), sb.state
		  FROM starbase sb
		  LEFT JOIN entity e ON e.itemID = sb.entityID
		  LEFT JOIN invTypes t ON t.typeID = e.typeID
		  LEFT JOIN corporation co ON co.corporationID = sb.ownerCorpID
		 WHERE sb.moonID = ? LIMIT 1`, moonID).Scan(&d.TowerID, &d.TowerType, &d.TowerOwner, &d.TowerState)
	if d.TowerID != 0 {
		rows, err := db.QueryContext(ctx, `
			SELECT ss.entityID, COALESCE(t.typeName, ''), ss.state, ss.provisionTypeID
			  FROM starbaseStructures ss
			  JOIN entity e ON e.itemID = ss.entityID
			  JOIN invTypes t ON t.typeID = e.typeID AND t.groupID = 416
			 WHERE ss.towerID = ? ORDER BY ss.entityID`, d.TowerID)
		if err == nil {
			for rows.Next() {
				var h MoonHarvesterView
				if rows.Scan(&h.EntityID, &h.TypeName, &h.State, &h.MaterialID) != nil {
					continue
				}
				h.MaterialName = names[h.MaterialID]
				h.OffPool = h.MaterialID != 0 && !has[h.MaterialID]
				d.Harvesters = append(d.Harvesters, h)
			}
			rows.Close()
		}
	}
	return d, nil
}

// MoonMaxQuantity is the largest batches-per-cycle a line can hold: no harvester has
// a quality above 3, so a 4 would read as 4 and harvest as 3.
const MoonMaxQuantity = 3

// SetMoonResource adds or changes one material line by hand and marks the moon
// edited, so a rescramble that keeps hand edits leaves it alone.
func SetMoonResource(ctx context.Context, db *sql.DB, moonID, typeID, qty int64) error {
	if !IsMoonMaterial(typeID) {
		return fmt.Errorf("type %d is not a moon material", typeID)
	}
	if qty < 1 || qty > MoonMaxQuantity {
		return fmt.Errorf("quantity must be 1–%d batches per cycle", MoonMaxQuantity)
	}
	if _, err := GetMoonResources(ctx, db, moonID); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO moonResources (moonID, typeID, quantity) VALUES (?, ?, ?)
		ON DUPLICATE KEY UPDATE quantity = VALUES(quantity)`, moonID, typeID, qty); err != nil {
		return fmt.Errorf("SetMoonResource: %w", err)
	}
	return markMoonEdited(ctx, db, moonID)
}

// DeleteMoonResource removes one material line by hand.
func DeleteMoonResource(ctx context.Context, db *sql.DB, moonID, typeID int64) error {
	if _, err := GetMoonResources(ctx, db, moonID); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx,
		`DELETE FROM moonResources WHERE moonID = ? AND typeID = ?`, moonID, typeID); err != nil {
		return fmt.Errorf("DeleteMoonResource: %w", err)
	}
	return markMoonEdited(ctx, db, moonID)
}

func markMoonEdited(ctx context.Context, db *sql.DB, moonID int64) error {
	if _, err := db.ExecContext(ctx,
		`UPDATE moonSurvey SET edited = 1 WHERE moonID = ?`, moonID); err != nil {
		return fmt.Errorf("markMoonEdited: %w", err)
	}
	return nil
}

// RerollMoon rolls one moon again under a new per-moon salt. The result is a
// hand decision, so the moon is marked edited.
func RerollMoon(ctx context.Context, db *sql.DB, moonID int64) error {
	if _, err := GetMoonResources(ctx, db, moonID); err != nil {
		return err
	}
	var rerolls int64
	_ = db.QueryRowContext(ctx, `SELECT rerolls FROM moonSurvey WHERE moonID = ?`, moonID).Scan(&rerolls)
	return generateOneMoon(ctx, db, moonID, rerolls+1, true)
}

// RestoreMoon puts a moon back to what the current world seed and profile
// generate for it, dropping hand edits and re-rolls.
func RestoreMoon(ctx context.Context, db *sql.DB, moonID int64) error {
	if err := ensureMoonTables(ctx, db); err != nil {
		return err
	}
	return generateOneMoon(ctx, db, moonID, 0, true)
}

// MoonRegionOption is a region for the moon list's filter.
type MoonRegionOption struct {
	RegionID   int64
	RegionName string
}

// ListMoonRegions returns every region that has moons, by name.
func ListMoonRegions(ctx context.Context, db *sql.DB) ([]MoonRegionOption, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT r.regionID, r.regionName FROM mapRegions r
		 WHERE EXISTS (SELECT 1 FROM mapDenormalize d WHERE d.regionID = r.regionID AND d.groupID = 8)
		 ORDER BY r.regionName`)
	if err != nil {
		return nil, fmt.Errorf("ListMoonRegions: %w", err)
	}
	defer rows.Close()
	var out []MoonRegionOption
	for rows.Next() {
		var o MoonRegionOption
		if rows.Scan(&o.RegionID, &o.RegionName) == nil {
			out = append(out, o)
		}
	}
	return out, rows.Err()
}
