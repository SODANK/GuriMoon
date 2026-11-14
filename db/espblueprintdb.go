package db

// ESP Blueprint browser/editor queries. Three layers are edited here:
//
//   - invBlueprintTypes: the type, one row per blueprint type, shared by every copy. Production
//     time, waste factor, research times, max runs.
//   - tl2materialsfortypewithactivity: what a type consumes, per activity. Category 16 rows are
//     skill requirements and are never consumed.
//   - invBlueprints: one row per item in the game (ME, PE, copy flag, remaining runs).
//
// Editing a type changes every copy; editing an instance changes one player's item.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ESPActivityName maps a RAM activityID to its name. It duplicates ramactivities so a blueprint
// page needs no lookup per row.
var ESPActivityName = map[int64]string{
	0: "None",
	1: "Manufacturing",
	2: "Researching Technology",
	3: "Time Efficiency Research",
	4: "Material Efficiency Research",
	5: "Copying",
	6: "Duplicating",
	7: "Reverse Engineering",
	8: "Invention",
}

// ESPActivityShort is the same mapping at listing width, with the two research activities
// shortened to ME and PE. The long names belong on title= and on pages with room.
var ESPActivityShort = map[int64]string{
	0: "None",
	1: "Manufacturing",
	2: "Tech Research",
	3: "PE Research",
	4: "ME Research",
	5: "Copying",
	6: "Duplicating",
	7: "Reverse Eng.",
	8: "Invention",
}

// ESPActivityShortName is ESPActivityShort with a fallback for an activityID
// the shipped set does not name.
func ESPActivityShortName(id int64) string {
	if n := ESPActivityShort[id]; n != "" {
		return n
	}
	return fmt.Sprintf("Activity %d", id)
}

// ESPActivityManufacturing is the one activity that builds an item; every other activity is
// science. The factories page splits its job list on this single test.
const ESPActivityManufacturing = 1

// ESPEditableActivities are the activities the material editor offers, in the
// order the blueprint window shows them.
var ESPEditableActivities = []int64{1, 3, 4, 5, 8}

// ESPBlueprintRow is one line of the blueprint list.
type ESPBlueprintRow struct {
	BlueprintTypeID int64
	BlueprintName   string
	Published       int64
	Missing         bool // blueprint invType with no invBlueprintTypes row

	ProductTypeID   int64
	ProductName     string
	ProductGroupID  int64
	ProductGroup    string
	ProductCategory string
	PortionSize     int64

	TechLevel          int64
	ProductionTime     int64
	WasteFactor        int64
	MaxProductionLimit int64

	MaterialCount int64 // consumable materials for manufacturing
	Instances     int64 // copies of this blueprint that exist in the game
	ActiveJobs    int64
}

// ESPBlueprintFilter narrows the blueprint list.
type ESPBlueprintFilter struct {
	Search     string // blueprint or product name, or a typeID
	CategoryID int64  // product category
	TechLevel  int64  // 0 = any
	OwnedOnly  bool   // only types someone actually holds a copy of
	Missing    bool   // only blueprint invTypes with no invBlueprintTypes row
	Limit      int
}

// ESPBlueprintCategory is one product category with a blueprint count, for the
// list page's category filter.
type ESPBlueprintCategory struct {
	CategoryID int64
	Name       string
	Count      int64
}

// ESPBlueprintStats is the headline count row on the list page.
type ESPBlueprintStats struct {
	Types      int64
	Published  int64
	T2         int64
	Missing    int64 // blueprint invTypes with no invBlueprintTypes row
	Orphaned   int64 // invBlueprintTypes whose productTypeID does not exist
	Instances  int64 // blueprint items in the game
	Copies     int64 // of those, BPCs
	ActiveJobs int64
}

// blueprintSelect is shared by the list and the detail read so both agree on
// what a blueprint row looks like.
const blueprintSelect = `
		SELECT bt.blueprintTypeID,
		       COALESCE(bpt.typeName, ''), COALESCE(bpt.published, 0),
		       COALESCE(bt.productTypeID, 0),
		       COALESCE(pt.typeName, ''), COALESCE(pt.groupID, 0),
		       COALESCE(pg.groupName, ''), COALESCE(pc.categoryName, ''),
		       COALESCE(pt.portionSize, 1),
		       COALESCE(bt.techLevel, 1), COALESCE(bt.productionTime, 0),
		       COALESCE(bt.wasteFactor, 0), COALESCE(bt.maxProductionLimit, 0)
		  FROM invBlueprintTypes bt
		  LEFT JOIN invTypes bpt  ON bpt.typeID = bt.blueprintTypeID
		  LEFT JOIN invTypes pt   ON pt.typeID  = bt.productTypeID
		  LEFT JOIN invGroups pg  ON pg.groupID = pt.groupID
		  LEFT JOIN invCategories pc ON pc.categoryID = pg.categoryID`

// ESPListBlueprints returns blueprint types matching the filter. Per-row counts (materials,
// instances, jobs) come from three grouped follow-up queries, not correlated subqueries.
func ESPListBlueprints(ctx context.Context, db *sql.DB, f ESPBlueprintFilter) ([]ESPBlueprintRow, error) {
	if f.Missing {
		return espListMissingBlueprints(ctx, db, f)
	}

	var where []string
	var args []interface{}
	if s := strings.TrimSpace(f.Search); s != "" {
		where = append(where, `(bpt.typeName LIKE ? OR pt.typeName LIKE ? OR CAST(bt.blueprintTypeID AS CHAR) = ?)`)
		args = append(args, "%"+s+"%", "%"+s+"%", s)
	}
	if f.CategoryID != 0 {
		where = append(where, `pg.categoryID = ?`)
		args = append(args, f.CategoryID)
	}
	if f.TechLevel != 0 {
		where = append(where, `COALESCE(bt.techLevel, 1) = ?`)
		args = append(args, f.TechLevel)
	}
	if f.OwnedOnly {
		where = append(where, `EXISTS (SELECT 1 FROM entity e WHERE e.typeID = bt.blueprintTypeID)`)
	}

	query := blueprintSelect
	if len(where) > 0 {
		query += "\n WHERE " + strings.Join(where, " AND ")
	}
	query += "\n ORDER BY bpt.typeName"
	if f.Limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", f.Limit)
	}

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("ESPListBlueprints: %w", err)
	}
	defer rows.Close()

	var out []ESPBlueprintRow
	ids := make([]int64, 0, 256)
	for rows.Next() {
		var b ESPBlueprintRow
		if err := rows.Scan(&b.BlueprintTypeID, &b.BlueprintName, &b.Published,
			&b.ProductTypeID, &b.ProductName, &b.ProductGroupID,
			&b.ProductGroup, &b.ProductCategory, &b.PortionSize,
			&b.TechLevel, &b.ProductionTime, &b.WasteFactor, &b.MaxProductionLimit); err != nil {
			return nil, fmt.Errorf("ESPListBlueprints scan: %w", err)
		}
		if b.BlueprintName == "" {
			b.BlueprintName = fmt.Sprintf("typeID %d", b.BlueprintTypeID)
		}
		out = append(out, b)
		ids = append(ids, b.BlueprintTypeID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}

	mats, _ := espCountBy(ctx, db, `
		SELECT m.typeID, COUNT(*)
		  FROM tl2materialsfortypewithactivity m
		  JOIN invTypes t ON t.typeID = m.requiredTypeID
		  LEFT JOIN invGroups g ON g.groupID = t.groupID
		 WHERE m.activity = 1 AND m.damagePerJob >= 1 AND COALESCE(g.categoryID, 0) <> 16
		   AND m.typeID IN (`+idList(ids)+`)
		 GROUP BY m.typeID`)
	inst, _ := espCountBy(ctx, db, `
		SELECT typeID, COUNT(*) FROM entity
		 WHERE typeID IN (`+idList(ids)+`) GROUP BY typeID`)
	jobs, _ := espCountBy(ctx, db, `
		SELECT e.typeID, COUNT(*)
		  FROM ramJobs j JOIN entity e ON e.itemID = j.installedItemID
		 WHERE j.completedStatusID < 100 AND e.typeID IN (`+idList(ids)+`)
		 GROUP BY e.typeID`)

	for i := range out {
		out[i].MaterialCount = mats[out[i].BlueprintTypeID]
		out[i].Instances = inst[out[i].BlueprintTypeID]
		out[i].ActiveJobs = jobs[out[i].BlueprintTypeID]
	}
	return out, nil
}

// espListMissingBlueprints finds invTypes in the Blueprint category that have no
// invBlueprintTypes row. They exist as items but cannot be used.
func espListMissingBlueprints(ctx context.Context, db *sql.DB, f ESPBlueprintFilter) ([]ESPBlueprintRow, error) {
	var where []string
	var args []interface{}
	if s := strings.TrimSpace(f.Search); s != "" {
		where = append(where, `(t.typeName LIKE ? OR CAST(t.typeID AS CHAR) = ?)`)
		args = append(args, "%"+s+"%", s)
	}
	query := `
		SELECT t.typeID, t.typeName, COALESCE(t.published, 0), COALESCE(g.groupName, '')
		  FROM invTypes t
		  JOIN invGroups g ON g.groupID = t.groupID
		 WHERE g.categoryID = 9
		   AND NOT EXISTS (SELECT 1 FROM invBlueprintTypes bt WHERE bt.blueprintTypeID = t.typeID)`
	if len(where) > 0 {
		query += " AND " + strings.Join(where, " AND ")
	}
	query += " ORDER BY t.typeName"
	if f.Limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", f.Limit)
	}

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("espListMissingBlueprints: %w", err)
	}
	defer rows.Close()
	var out []ESPBlueprintRow
	for rows.Next() {
		var b ESPBlueprintRow
		b.Missing = true
		if err := rows.Scan(&b.BlueprintTypeID, &b.BlueprintName, &b.Published, &b.ProductGroup); err != nil {
			return nil, fmt.Errorf("espListMissingBlueprints scan: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// espCountBy runs a two-column "id, count" query into a map.
func espCountBy(ctx context.Context, db *sql.DB, query string, args ...interface{}) (map[int64]int64, error) {
	out := map[int64]int64{}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, n int64
		if err := rows.Scan(&id, &n); err != nil {
			return out, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// ESPBlueprintStatsRow returns the list page's headline counts.
func ESPBlueprintStatsRow(ctx context.Context, db *sql.DB) (ESPBlueprintStats, error) {
	var s ESPBlueprintStats
	err := db.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(SUM(COALESCE(t.published, 0) = 1), 0),
		       COALESCE(SUM(COALESCE(bt.techLevel, 1) = 2), 0),
		       COALESCE(SUM(p.typeID IS NULL), 0)
		  FROM invBlueprintTypes bt
		  LEFT JOIN invTypes t ON t.typeID = bt.blueprintTypeID
		  LEFT JOIN invTypes p ON p.typeID = bt.productTypeID`).
		Scan(&s.Types, &s.Published, &s.T2, &s.Orphaned)
	if err != nil {
		return s, fmt.Errorf("ESPBlueprintStatsRow types: %w", err)
	}
	_ = db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM invTypes t JOIN invGroups g ON g.groupID = t.groupID
		 WHERE g.categoryID = 9
		   AND NOT EXISTS (SELECT 1 FROM invBlueprintTypes bt WHERE bt.blueprintTypeID = t.typeID)`).
		Scan(&s.Missing)
	_ = db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(copy = 1), 0) FROM invBlueprints`).
		Scan(&s.Instances, &s.Copies)
	_ = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM ramJobs WHERE completedStatusID < 100`).Scan(&s.ActiveJobs)
	return s, nil
}

// ESPBlueprintCategories lists the product categories blueprints build into.
func ESPBlueprintCategories(ctx context.Context, db *sql.DB) ([]ESPBlueprintCategory, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT g.categoryID, COALESCE(c.categoryName, ''), COUNT(*)
		  FROM invBlueprintTypes bt
		  JOIN invTypes pt ON pt.typeID = bt.productTypeID
		  JOIN invGroups g ON g.groupID = pt.groupID
		  LEFT JOIN invCategories c ON c.categoryID = g.categoryID
		 GROUP BY g.categoryID, c.categoryName
		 ORDER BY c.categoryName`)
	if err != nil {
		return nil, fmt.Errorf("ESPBlueprintCategories: %w", err)
	}
	defer rows.Close()
	var out []ESPBlueprintCategory
	for rows.Next() {
		var c ESPBlueprintCategory
		if err := rows.Scan(&c.CategoryID, &c.Name, &c.Count); err != nil {
			return nil, fmt.Errorf("ESPBlueprintCategories scan: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ESPBlueprintMaterial is one row of tl2materialsfortypewithactivity.
type ESPBlueprintMaterial struct {
	Activity       int64
	ActivityName   string
	RequiredTypeID int64
	TypeName       string
	GroupName      string
	CategoryID     int64
	Quantity       int64
	DamagePerJob   float64
	Recycle        int64
}

// IsSkill reports whether this row is a skill requirement rather than a
// material. Category 16 rows are what the client renders under "Required
// Skills"; ConsumeMaterials skips them.
func (m ESPBlueprintMaterial) IsSkill() bool { return m.CategoryID == 16 }

// Consumed reports whether a manufacturing run actually eats this material.
// damagePerJob below 1 means the item must be present but survives the job.
func (m ESPBlueprintMaterial) Consumed() bool {
	return !m.IsSkill() && m.Recycle == 0 && m.DamagePerJob >= 1
}

// ESPBlueprintActivity groups the material rows of one activity.
type ESPBlueprintActivity struct {
	ActivityID   int64
	ActivityName string
	Materials    []ESPBlueprintMaterial
	Skills       []ESPBlueprintMaterial
}

// ESPBlueprintInstance is one blueprint item that exists in the game.
type ESPBlueprintInstance struct {
	BlueprintID       int64
	OwnerID           int64
	OwnerName         string
	LocationID        int64
	LocationName      string
	Flag              int64
	Quantity          int64
	MaterialLevel     int64
	ProductivityLevel int64
	Copy              int64
	RunsRemaining     int64
	JobID             int64
	JobActivity       int64
	HasRow            bool // an invBlueprints research row exists
}

// InProduction reports whether the item is locked inside a running job.
func (i ESPBlueprintInstance) InProduction() bool { return i.JobID != 0 }

// ESPBlueprintDetail is everything the blueprint editor renders for one type.
type ESPBlueprintDetail struct {
	ESPBlueprintRow

	ParentBlueprintTypeID int64
	ParentName            string
	Description           string
	Volume                float64
	BasePrice             float64
	GroupName             string

	ResearchProductivityTime   int64
	ResearchMaterialTime       int64
	ResearchCopyTime           int64
	ResearchTechTime           int64
	ProductivityModifier       int64
	MaterialModifier           int64
	ChanceOfReverseEngineering float64

	Activities []ESPBlueprintActivity
	Instances  []ESPBlueprintInstance
	Variants   []ESPBlueprintRow // T2 blueprints whose parent is this one
}

// ESPGetBlueprintDetail loads one blueprint type with its material rows and
// every copy of it that exists in the game.
func ESPGetBlueprintDetail(ctx context.Context, db *sql.DB, blueprintTypeID int64) (*ESPBlueprintDetail, error) {
	var d ESPBlueprintDetail
	var parent sql.NullInt64
	err := db.QueryRowContext(ctx, `
		SELECT bt.blueprintTypeID,
		       COALESCE(bpt.typeName, ''), COALESCE(bpt.published, 0),
		       COALESCE(bpt.description, ''), COALESCE(bpt.volume, 0),
		       COALESCE(bpt.basePrice, 0), COALESCE(bg.groupName, ''),
		       bt.parentBlueprintTypeID,
		       COALESCE(bt.productTypeID, 0),
		       COALESCE(pt.typeName, ''), COALESCE(pt.groupID, 0),
		       COALESCE(pg.groupName, ''), COALESCE(pc.categoryName, ''),
		       COALESCE(pt.portionSize, 1),
		       COALESCE(bt.techLevel, 1), COALESCE(bt.productionTime, 0),
		       COALESCE(bt.wasteFactor, 0), COALESCE(bt.maxProductionLimit, 0),
		       COALESCE(bt.researchProductivityTime, 0), COALESCE(bt.researchMaterialTime, 0),
		       COALESCE(bt.researchCopyTime, 0), COALESCE(bt.researchTechTime, 0),
		       COALESCE(bt.productivityModifier, 0), COALESCE(bt.materialModifier, 0),
		       COALESCE(bt.chanceOfReverseEngineering, 0)
		  FROM invBlueprintTypes bt
		  LEFT JOIN invTypes bpt  ON bpt.typeID = bt.blueprintTypeID
		  LEFT JOIN invGroups bg  ON bg.groupID = bpt.groupID
		  LEFT JOIN invTypes pt   ON pt.typeID  = bt.productTypeID
		  LEFT JOIN invGroups pg  ON pg.groupID = pt.groupID
		  LEFT JOIN invCategories pc ON pc.categoryID = pg.categoryID
		 WHERE bt.blueprintTypeID = ?`, blueprintTypeID).
		Scan(&d.BlueprintTypeID, &d.BlueprintName, &d.Published, &d.Description,
			&d.Volume, &d.BasePrice, &d.GroupName, &parent,
			&d.ProductTypeID, &d.ProductName, &d.ProductGroupID,
			&d.ProductGroup, &d.ProductCategory, &d.PortionSize,
			&d.TechLevel, &d.ProductionTime, &d.WasteFactor, &d.MaxProductionLimit,
			&d.ResearchProductivityTime, &d.ResearchMaterialTime,
			&d.ResearchCopyTime, &d.ResearchTechTime,
			&d.ProductivityModifier, &d.MaterialModifier, &d.ChanceOfReverseEngineering)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("blueprint type %d not found", blueprintTypeID)
		}
		return nil, fmt.Errorf("ESPGetBlueprintDetail: %w", err)
	}
	if d.BlueprintName == "" {
		d.BlueprintName = fmt.Sprintf("typeID %d", d.BlueprintTypeID)
	}
	if parent.Valid {
		d.ParentBlueprintTypeID = parent.Int64
		_ = db.QueryRowContext(ctx,
			`SELECT COALESCE(typeName, '') FROM invTypes WHERE typeID = ?`,
			parent.Int64).Scan(&d.ParentName)
	}

	acts, err := ESPGetBlueprintMaterials(ctx, db, blueprintTypeID)
	if err != nil {
		return nil, err
	}
	d.Activities = acts

	d.Instances, err = ESPGetBlueprintInstances(ctx, db, blueprintTypeID)
	if err != nil {
		return nil, err
	}
	d.MaterialCount = 0
	for _, a := range d.Activities {
		if a.ActivityID == 1 {
			for _, m := range a.Materials {
				if m.Consumed() {
					d.MaterialCount++
				}
			}
		}
	}

	// T2 blueprints that invent from this one.
	vrows, err := db.QueryContext(ctx, `
		SELECT bt.blueprintTypeID, COALESCE(t.typeName, ''), COALESCE(bt.techLevel, 1)
		  FROM invBlueprintTypes bt
		  LEFT JOIN invTypes t ON t.typeID = bt.blueprintTypeID
		 WHERE bt.parentBlueprintTypeID = ? AND bt.blueprintTypeID <> ?
		 ORDER BY t.typeName`, blueprintTypeID, blueprintTypeID)
	if err == nil {
		defer vrows.Close()
		for vrows.Next() {
			var v ESPBlueprintRow
			if err := vrows.Scan(&v.BlueprintTypeID, &v.BlueprintName, &v.TechLevel); err == nil {
				d.Variants = append(d.Variants, v)
			}
		}
	}
	return &d, nil
}

// ESPGetBlueprintMaterials returns every material/skill row of a blueprint
// type, grouped by activity in ESPEditableActivities order (unknown activities
// trail behind so nothing in the table is hidden from the editor).
func ESPGetBlueprintMaterials(ctx context.Context, db *sql.DB, blueprintTypeID int64) ([]ESPBlueprintActivity, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT m.activity, m.requiredTypeID, COALESCE(t.typeName, ''),
		       COALESCE(g.groupName, ''), COALESCE(g.categoryID, 0),
		       m.quantity, m.damagePerJob, m.recycle
		  FROM tl2materialsfortypewithactivity m
		  LEFT JOIN invTypes t  ON t.typeID  = m.requiredTypeID
		  LEFT JOIN invGroups g ON g.groupID = t.groupID
		 WHERE m.typeID = ?
		 ORDER BY m.activity, COALESCE(g.categoryID, 0), t.typeName`, blueprintTypeID)
	if err != nil {
		return nil, fmt.Errorf("ESPGetBlueprintMaterials: %w", err)
	}
	defer rows.Close()

	byActivity := map[int64]*ESPBlueprintActivity{}
	var order []int64
	get := func(id int64) *ESPBlueprintActivity {
		a := byActivity[id]
		if a == nil {
			name := ESPActivityName[id]
			if name == "" {
				name = fmt.Sprintf("Activity %d", id)
			}
			a = &ESPBlueprintActivity{ActivityID: id, ActivityName: name}
			byActivity[id] = a
			order = append(order, id)
		}
		return a
	}
	for _, id := range ESPEditableActivities {
		get(id)
	}

	for rows.Next() {
		var m ESPBlueprintMaterial
		if err := rows.Scan(&m.Activity, &m.RequiredTypeID, &m.TypeName,
			&m.GroupName, &m.CategoryID, &m.Quantity, &m.DamagePerJob, &m.Recycle); err != nil {
			return nil, fmt.Errorf("ESPGetBlueprintMaterials scan: %w", err)
		}
		if m.TypeName == "" {
			m.TypeName = fmt.Sprintf("typeID %d", m.RequiredTypeID)
		}
		m.ActivityName = ESPActivityName[m.Activity]
		a := get(m.Activity)
		if m.IsSkill() {
			a.Skills = append(a.Skills, m)
		} else {
			a.Materials = append(a.Materials, m)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]ESPBlueprintActivity, 0, len(order))
	for _, id := range order {
		out = append(out, *byActivity[id])
	}
	return out, nil
}

// ESPGetBlueprintInstances lists every item of this blueprint type in the game
// with its research state and the job it is locked into, if any.
func ESPGetBlueprintInstances(ctx context.Context, db *sql.DB, blueprintTypeID int64) ([]ESPBlueprintInstance, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT e.itemID, COALESCE(e.ownerID, 0), COALESCE(c.characterName, COALESCE(corp.corporationName, '')),
		       COALESCE(e.locationID, 0),
		       COALESCE(s.stationName, COALESCE(sys.solarSystemName, '')),
		       COALESCE(e.flag, 0), COALESCE(e.quantity, 1),
		       b.blueprintID,
		       COALESCE(b.materialLevel, 0), COALESCE(b.productivityLevel, 0),
		       COALESCE(b.copy, 0), COALESCE(b.licensedProductionRunsRemaining, -1),
		       COALESCE(j.jobID, 0), COALESCE(j.activityID, 0)
		  FROM entity e
		  LEFT JOIN invBlueprints b   ON b.blueprintID = e.itemID
		  LEFT JOIN character_ c      ON c.characterID = e.ownerID
		  LEFT JOIN corporation corp  ON corp.corporationID = e.ownerID
		  LEFT JOIN staStations s     ON s.stationID = e.locationID
		  LEFT JOIN mapSolarSystems sys ON sys.solarSystemID = e.locationID
		  LEFT JOIN ramJobs j ON j.installedItemID = e.itemID AND j.completedStatusID < 100
		 WHERE e.typeID = ?
		 ORDER BY e.itemID`, blueprintTypeID)
	if err != nil {
		return nil, fmt.Errorf("ESPGetBlueprintInstances: %w", err)
	}
	defer rows.Close()

	var out []ESPBlueprintInstance
	for rows.Next() {
		var i ESPBlueprintInstance
		var bpRow sql.NullInt64
		if err := rows.Scan(&i.BlueprintID, &i.OwnerID, &i.OwnerName, &i.LocationID,
			&i.LocationName, &i.Flag, &i.Quantity, &bpRow,
			&i.MaterialLevel, &i.ProductivityLevel, &i.Copy, &i.RunsRemaining,
			&i.JobID, &i.JobActivity); err != nil {
			return nil, fmt.Errorf("ESPGetBlueprintInstances scan: %w", err)
		}
		i.HasRow = bpRow.Valid
		out = append(out, i)
	}
	return out, rows.Err()
}

// ESPBlueprintTypeEdit carries the invBlueprintTypes fields the editor writes.
type ESPBlueprintTypeEdit struct {
	BlueprintTypeID            int64
	ParentBlueprintTypeID      int64 // 0 stores NULL
	ProductTypeID              int64
	ProductionTime             int64
	TechLevel                  int64
	ResearchProductivityTime   int64
	ResearchMaterialTime       int64
	ResearchCopyTime           int64
	ResearchTechTime           int64
	ProductivityModifier       int64
	MaterialModifier           int64
	WasteFactor                int64
	ChanceOfReverseEngineering float64
	MaxProductionLimit         int64
}

// ESPUpdateBlueprintType writes one invBlueprintTypes row. It refuses a productTypeID that does
// not exist, because a job on such a blueprint installs and then fails at delivery.
func ESPUpdateBlueprintType(ctx context.Context, db *sql.DB, e ESPBlueprintTypeEdit) error {
	if e.BlueprintTypeID == 0 {
		return fmt.Errorf("blueprintTypeID is required")
	}
	if e.ProductTypeID != 0 {
		var exists int
		if err := db.QueryRowContext(ctx,
			`SELECT 1 FROM invTypes WHERE typeID = ?`, e.ProductTypeID).Scan(&exists); err != nil {
			return fmt.Errorf("productTypeID %d is not an item type", e.ProductTypeID)
		}
	}
	if e.MaxProductionLimit < 0 {
		return fmt.Errorf("maxProductionLimit cannot be negative")
	}
	var parent interface{}
	if e.ParentBlueprintTypeID != 0 {
		parent = e.ParentBlueprintTypeID
	}
	_, err := db.ExecContext(ctx, `
		UPDATE invBlueprintTypes
		   SET parentBlueprintTypeID = ?, productTypeID = ?, productionTime = ?,
		       techLevel = ?, researchProductivityTime = ?, researchMaterialTime = ?,
		       researchCopyTime = ?, researchTechTime = ?, productivityModifier = ?,
		       materialModifier = ?, wasteFactor = ?, chanceOfReverseEngineering = ?,
		       maxProductionLimit = ?
		 WHERE blueprintTypeID = ?`,
		parent, e.ProductTypeID, e.ProductionTime, e.TechLevel,
		e.ResearchProductivityTime, e.ResearchMaterialTime, e.ResearchCopyTime,
		e.ResearchTechTime, e.ProductivityModifier, e.MaterialModifier,
		e.WasteFactor, e.ChanceOfReverseEngineering, e.MaxProductionLimit,
		e.BlueprintTypeID)
	if err != nil {
		return fmt.Errorf("ESPUpdateBlueprintType: %w", err)
	}
	return nil
}

// ESPCreateBlueprintType seeds an invBlueprintTypes row for a blueprint invType
// that has none, so the type stops being an inert item.
func ESPCreateBlueprintType(ctx context.Context, db *sql.DB, blueprintTypeID, productTypeID int64) error {
	if blueprintTypeID == 0 {
		return fmt.Errorf("blueprintTypeID is required")
	}
	var exists int
	if err := db.QueryRowContext(ctx,
		`SELECT 1 FROM invTypes WHERE typeID = ?`, blueprintTypeID).Scan(&exists); err != nil {
		return fmt.Errorf("typeID %d is not an item type", blueprintTypeID)
	}
	if productTypeID != 0 {
		if err := db.QueryRowContext(ctx,
			`SELECT 1 FROM invTypes WHERE typeID = ?`, productTypeID).Scan(&exists); err != nil {
			return fmt.Errorf("productTypeID %d is not an item type", productTypeID)
		}
	}
	_, err := db.ExecContext(ctx, `
		INSERT IGNORE INTO invBlueprintTypes
		  (blueprintTypeID, parentBlueprintTypeID, productTypeID, productionTime,
		   techLevel, researchProductivityTime, researchMaterialTime, researchCopyTime,
		   researchTechTime, productivityModifier, materialModifier, wasteFactor,
		   chanceOfReverseEngineering, maxProductionLimit)
		VALUES (?, NULL, ?, 3600, 1, 3600, 3600, 3600, 0, 1, 1, 10, 0, 10)`,
		blueprintTypeID, productTypeID)
	if err != nil {
		return fmt.Errorf("ESPCreateBlueprintType: %w", err)
	}
	return nil
}

// ESPUpsertBlueprintMaterial writes one material/skill requirement row.
func ESPUpsertBlueprintMaterial(ctx context.Context, db *sql.DB, blueprintTypeID, activity, requiredTypeID, quantity int64, damagePerJob float64, recycle int64) error {
	if blueprintTypeID == 0 || requiredTypeID == 0 {
		return fmt.Errorf("blueprint and required type are both required")
	}
	if quantity < 0 {
		return fmt.Errorf("quantity cannot be negative")
	}
	var exists int
	if err := db.QueryRowContext(ctx,
		`SELECT 1 FROM invTypes WHERE typeID = ?`, requiredTypeID).Scan(&exists); err != nil {
		return fmt.Errorf("requiredTypeID %d is not an item type", requiredTypeID)
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO tl2materialsfortypewithactivity
		  (typeID, activity, requiredTypeID, quantity, damagePerJob, recycle)
		VALUES (?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE quantity = VALUES(quantity),
		                        damagePerJob = VALUES(damagePerJob),
		                        recycle = VALUES(recycle)`,
		blueprintTypeID, activity, requiredTypeID, quantity, damagePerJob, recycle)
	if err != nil {
		return fmt.Errorf("ESPUpsertBlueprintMaterial: %w", err)
	}
	return nil
}

// ESPDeleteBlueprintMaterial removes one requirement row.
func ESPDeleteBlueprintMaterial(ctx context.Context, db *sql.DB, blueprintTypeID, activity, requiredTypeID int64) error {
	_, err := db.ExecContext(ctx, `
		DELETE FROM tl2materialsfortypewithactivity
		 WHERE typeID = ? AND activity = ? AND requiredTypeID = ?`,
		blueprintTypeID, activity, requiredTypeID)
	if err != nil {
		return fmt.Errorf("ESPDeleteBlueprintMaterial: %w", err)
	}
	return nil
}

// ESPUpdateBlueprintInstance writes one player's blueprint item: ME, PE, copy flag and runs. A
// blueprint locked in a running job is refused, because CompleteJob reads these fields at
// delivery. ME and PE have no upper cap. See docs/db-10-notes.md#blueprint-research-has-no-level-cap.
func ESPUpdateBlueprintInstance(ctx context.Context, db *sql.DB, blueprintID, me, pe, isCopy, runs int64) error {
	var jobID int64
	_ = db.QueryRowContext(ctx,
		`SELECT jobID FROM ramJobs WHERE installedItemID = ? AND completedStatusID < 100 LIMIT 1`,
		blueprintID).Scan(&jobID)
	if jobID != 0 {
		return fmt.Errorf("blueprint %d is installed in job %d — cancel the job first", blueprintID, jobID)
	}
	if isCopy != 0 {
		isCopy = 1
	}
	if isCopy == 0 {
		runs = -1 // originals have unlimited runs
	} else if runs <= 0 {
		return fmt.Errorf("a copy needs at least 1 licensed run")
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO invBlueprints (blueprintID, materialLevel, productivityLevel, copy, licensedProductionRunsRemaining)
		VALUES (?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE materialLevel = VALUES(materialLevel),
		                        productivityLevel = VALUES(productivityLevel),
		                        copy = VALUES(copy),
		                        licensedProductionRunsRemaining = VALUES(licensedProductionRunsRemaining)`,
		blueprintID, me, pe, isCopy, runs)
	if err != nil {
		return fmt.Errorf("ESPUpdateBlueprintInstance: %w", err)
	}
	return nil
}

// ESPTypeSearchRow is one hit of the item picker behind the material editor.
type ESPTypeSearchRow struct {
	TypeID    int64  `json:"id"`
	TypeName  string `json:"name"`
	GroupName string `json:"group"`
	Category  string `json:"category"`
}

// ESPSearchItemTypes backs the material picker's autocomplete.
func ESPSearchItemTypes(ctx context.Context, db *sql.DB, search string, limit int) ([]ESPTypeSearchRow, error) {
	search = strings.TrimSpace(search)
	if search == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 20
	}
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`
		SELECT t.typeID, t.typeName, COALESCE(g.groupName, ''), COALESCE(c.categoryName, '')
		  FROM invTypes t
		  LEFT JOIN invGroups g ON g.groupID = t.groupID
		  LEFT JOIN invCategories c ON c.categoryID = g.categoryID
		 WHERE t.typeName LIKE ? OR CAST(t.typeID AS CHAR) = ?
		 ORDER BY (t.typeName = ?) DESC, LENGTH(t.typeName), t.typeName
		 LIMIT %d`, limit), "%"+search+"%", search, search)
	if err != nil {
		return nil, fmt.Errorf("ESPSearchItemTypes: %w", err)
	}
	defer rows.Close()
	var out []ESPTypeSearchRow
	for rows.Next() {
		var t ESPTypeSearchRow
		if err := rows.Scan(&t.TypeID, &t.TypeName, &t.GroupName, &t.Category); err != nil {
			return nil, fmt.Errorf("ESPSearchItemTypes scan: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
