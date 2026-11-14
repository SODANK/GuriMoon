// Package db: POS templates, a starbase layout captured from a player-built POS and stamped at
// another moon. A template is captured, never authored: the tower type, each structure's type
// and offset from the tower (so a layout lands at any moon), what each handles (provisionTypeID)
// and the Production-tab wiring. Stamping places the tower through PlantStarbaseFor, then writes
// every structure and the wiring in one transaction (an entity row without its overlay row
// reloads as a loose ball); a failed transaction removes the tower again.
// See docs/starbase-pos-investigation.md#20-pos-templates.
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
)

// TemplateStructure is one anchored structure in a template.
type TemplateStructure struct {
	Idx             int
	TypeID          int64
	TypeName        string
	DX, DY, DZ      float64 // offset from the tower, metres
	ProvisionTypeID int64
	Online          bool
}

// TemplateLink is one Production-tab wire, by structure index.
type TemplateLink struct {
	TypeID int64
	SrcIdx int
	DstIdx int
}

// StarbaseTemplate is a captured POS layout.
type StarbaseTemplate struct {
	ID            int64
	Name          string
	TowerTypeID   int64
	TowerTypeName string
	FuelDays      float64
	CreatedBy     int64
	CreatedAt     uint64
	Structures    []TemplateStructure
	Links         []TemplateLink
}

var (
	starbaseTemplateOnce sync.Once
	starbaseTemplateErr  error
)

func ensureStarbaseTemplateTables(ctx context.Context, db *sql.DB) error {
	starbaseTemplateOnce.Do(func() {
		for _, ddl := range []string{`
			CREATE TABLE IF NOT EXISTS starbaseTemplates (
			  templateID  INT UNSIGNED NOT NULL AUTO_INCREMENT,
			  name        VARCHAR(64)  NOT NULL,
			  towerTypeID INT UNSIGNED NOT NULL,
			  fuelDays    DOUBLE       NOT NULL DEFAULT 0,
			  createdBy   INT UNSIGNED NOT NULL DEFAULT 0,
			  createdAt   BIGINT(20)   NOT NULL DEFAULT 0,
			  PRIMARY KEY (templateID),
			  UNIQUE KEY uq_template_name (name)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`, `
			CREATE TABLE IF NOT EXISTS starbaseTemplateStructures (
			  templateID      INT UNSIGNED NOT NULL,
			  idx             INT UNSIGNED NOT NULL,
			  typeID          INT UNSIGNED NOT NULL,
			  dx              DOUBLE       NOT NULL,
			  dy              DOUBLE       NOT NULL,
			  dz              DOUBLE       NOT NULL,
			  provisionTypeID INT UNSIGNED NOT NULL DEFAULT 0,
			  online          TINYINT(1)   NOT NULL DEFAULT 0,
			  PRIMARY KEY (templateID, idx)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`, `
			CREATE TABLE IF NOT EXISTS starbaseTemplateLinks (
			  templateID INT UNSIGNED NOT NULL,
			  typeID     INT UNSIGNED NOT NULL,
			  srcIdx     INT UNSIGNED NOT NULL,
			  dstIdx     INT UNSIGNED NOT NULL,
			  PRIMARY KEY (templateID, typeID, srcIdx)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`,
		} {
			if _, err := db.ExecContext(ctx, ddl); err != nil {
				starbaseTemplateErr = fmt.Errorf("ensureStarbaseTemplateTables: %w", err)
				return
			}
		}
	})
	return starbaseTemplateErr
}

// BuildStarbaseTemplate turns a live tower, its structures and their wiring
// into a template. Pure. Structures are ordered by entityID so capture is
// deterministic; a wire whose ends are not both on this tower is dropped.
func BuildStarbaseTemplate(name string, tower StarbaseRecord, slaves []StarbaseRecord, links []StructureLink) StarbaseTemplate {
	tpl := StarbaseTemplate{Name: name, TowerTypeID: tower.TypeID, TowerTypeName: tower.ItemName}
	sorted := append([]StarbaseRecord(nil), slaves...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].EntityID < sorted[j].EntityID })
	idxOf := make(map[int64]int, len(sorted))
	for i, s := range sorted {
		idxOf[s.EntityID] = i
		tpl.Structures = append(tpl.Structures, TemplateStructure{
			Idx: i, TypeID: s.TypeID, TypeName: s.ItemName,
			DX: s.X - tower.X, DY: s.Y - tower.Y, DZ: s.Z - tower.Z,
			ProvisionTypeID: s.ProvisionTypeID,
			Online:          s.State == StarbaseStateOnline || s.State == StarbaseStateOnlining,
		})
	}
	for _, l := range links {
		src, okS := idxOf[l.SourceID]
		dst, okD := idxOf[l.DestID]
		if okS && okD {
			tpl.Links = append(tpl.Links, TemplateLink{TypeID: l.TypeID, SrcIdx: src, DstIdx: dst})
		}
	}
	sort.Slice(tpl.Links, func(i, j int) bool {
		if tpl.Links[i].SrcIdx != tpl.Links[j].SrcIdx {
			return tpl.Links[i].SrcIdx < tpl.Links[j].SrcIdx
		}
		return tpl.Links[i].TypeID < tpl.Links[j].TypeID
	})
	return tpl
}

// ValidateTemplateOffsets refuses a structure outside the tower's anchor
// radius: the client's own LocateControlTower would not bind it to the tower,
// so a stamped layout would have structures nobody can online. Pure.
func ValidateTemplateOffsets(tpl StarbaseTemplate, maxDist float64) error {
	for _, s := range tpl.Structures {
		d := math.Sqrt(s.DX*s.DX + s.DY*s.DY + s.DZ*s.DZ)
		if maxDist > 0 && d > maxDist {
			return fmt.Errorf("structure %d (type %d) sits %.0f m from the tower, beyond its %.0f m anchor radius",
				s.Idx, s.TypeID, d, maxDist)
		}
	}
	for _, l := range tpl.Links {
		if l.SrcIdx < 0 || l.SrcIdx >= len(tpl.Structures) || l.DstIdx < 0 || l.DstIdx >= len(tpl.Structures) {
			return fmt.Errorf("link %d→%d names a structure the template does not have", l.SrcIdx, l.DstIdx)
		}
	}
	return nil
}

// CaptureStarbaseTemplate snapshots a live tower as a template called name,
// replacing any template of that name.
func CaptureStarbaseTemplate(ctx context.Context, db *sql.DB, towerID int64, name string, fuelDays float64, createdBy int64) (StarbaseTemplate, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 64 {
		return StarbaseTemplate{}, fmt.Errorf("template name must be 1-64 characters")
	}
	tower, found, err := GetStarbase(ctx, db, towerID)
	if err != nil {
		return StarbaseTemplate{}, err
	}
	if !found {
		return StarbaseTemplate{}, fmt.Errorf("no control tower %d", towerID)
	}
	slaves, err := ListStarbaseStructuresForTower(ctx, db, towerID)
	if err != nil {
		return StarbaseTemplate{}, err
	}
	links, err := ListStructureLinks(ctx, db, towerID)
	if err != nil {
		return StarbaseTemplate{}, err
	}
	tpl := BuildStarbaseTemplate(name, tower, slaves, links)
	tpl.FuelDays = fuelDays
	tpl.CreatedBy = createdBy
	tpl.CreatedAt = win32TimeNow()
	if err := SaveStarbaseTemplate(ctx, db, &tpl); err != nil {
		return StarbaseTemplate{}, err
	}
	return tpl, nil
}

// SaveStarbaseTemplate writes tpl (replacing any template of the same name)
// and sets tpl.ID.
func SaveStarbaseTemplate(ctx context.Context, db *sql.DB, tpl *StarbaseTemplate) error {
	if err := ensureStarbaseTemplateTables(ctx, db); err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var old int64
	if err := tx.QueryRowContext(ctx, `SELECT templateID FROM starbaseTemplates WHERE name = ?`, tpl.Name).Scan(&old); err == nil {
		if err := deleteTemplateTx(ctx, tx, old); err != nil {
			return err
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO starbaseTemplates (name, towerTypeID, fuelDays, createdBy, createdAt)
		VALUES (?, ?, ?, ?, ?)`, tpl.Name, tpl.TowerTypeID, tpl.FuelDays, tpl.CreatedBy, tpl.CreatedAt)
	if err != nil {
		return fmt.Errorf("SaveStarbaseTemplate: %w", err)
	}
	tpl.ID, _ = res.LastInsertId()
	for _, s := range tpl.Structures {
		online := 0
		if s.Online {
			online = 1
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO starbaseTemplateStructures
			  (templateID, idx, typeID, dx, dy, dz, provisionTypeID, online)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			tpl.ID, s.Idx, s.TypeID, s.DX, s.DY, s.DZ, s.ProvisionTypeID, online); err != nil {
			return fmt.Errorf("SaveStarbaseTemplate structure: %w", err)
		}
	}
	for _, l := range tpl.Links {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO starbaseTemplateLinks (templateID, typeID, srcIdx, dstIdx)
			VALUES (?, ?, ?, ?)`, tpl.ID, l.TypeID, l.SrcIdx, l.DstIdx); err != nil {
			return fmt.Errorf("SaveStarbaseTemplate link: %w", err)
		}
	}
	return tx.Commit()
}

func deleteTemplateTx(ctx context.Context, tx *sql.Tx, id int64) error {
	for _, q := range []string{
		`DELETE FROM starbaseTemplateLinks WHERE templateID = ?`,
		`DELETE FROM starbaseTemplateStructures WHERE templateID = ?`,
		`DELETE FROM starbaseTemplates WHERE templateID = ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			return fmt.Errorf("delete template %d: %w", id, err)
		}
	}
	return nil
}

// DeleteStarbaseTemplate removes a template and its rows.
func DeleteStarbaseTemplate(ctx context.Context, db *sql.DB, id int64) error {
	if err := ensureStarbaseTemplateTables(ctx, db); err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := deleteTemplateTx(ctx, tx, id); err != nil {
		return err
	}
	return tx.Commit()
}

// ListStarbaseTemplates returns every template with its structures (links omitted; the list
// only needs counts and layout).
func ListStarbaseTemplates(ctx context.Context, db *sql.DB) ([]StarbaseTemplate, error) {
	if err := ensureStarbaseTemplateTables(ctx, db); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `
		SELECT t.templateID, t.name, t.towerTypeID, COALESCE(it.typeName, ''), t.fuelDays, t.createdBy, t.createdAt
		  FROM starbaseTemplates t LEFT JOIN invTypes it ON it.typeID = t.towerTypeID
		 ORDER BY t.name`)
	if err != nil {
		return nil, fmt.Errorf("ListStarbaseTemplates: %w", err)
	}
	var out []StarbaseTemplate
	for rows.Next() {
		var t StarbaseTemplate
		if err := rows.Scan(&t.ID, &t.Name, &t.TowerTypeID, &t.TowerTypeName, &t.FuelDays, &t.CreatedBy, &t.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, t)
	}
	rows.Close()
	for i := range out {
		if err := loadTemplateParts(ctx, db, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// GetStarbaseTemplate loads one template by ID or, when id is 0, by name.
func GetStarbaseTemplate(ctx context.Context, db *sql.DB, id int64, name string) (StarbaseTemplate, bool, error) {
	if err := ensureStarbaseTemplateTables(ctx, db); err != nil {
		return StarbaseTemplate{}, false, err
	}
	var t StarbaseTemplate
	q := `SELECT t.templateID, t.name, t.towerTypeID, COALESCE(it.typeName, ''), t.fuelDays, t.createdBy, t.createdAt
	        FROM starbaseTemplates t LEFT JOIN invTypes it ON it.typeID = t.towerTypeID `
	var err error
	if id != 0 {
		err = db.QueryRowContext(ctx, q+`WHERE t.templateID = ?`, id).Scan(
			&t.ID, &t.Name, &t.TowerTypeID, &t.TowerTypeName, &t.FuelDays, &t.CreatedBy, &t.CreatedAt)
	} else {
		err = db.QueryRowContext(ctx, q+`WHERE t.name = ?`, strings.TrimSpace(name)).Scan(
			&t.ID, &t.Name, &t.TowerTypeID, &t.TowerTypeName, &t.FuelDays, &t.CreatedBy, &t.CreatedAt)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return t, false, nil
	}
	if err != nil {
		return t, false, err
	}
	if err := loadTemplateParts(ctx, db, &t); err != nil {
		return t, false, err
	}
	return t, true, nil
}

func loadTemplateParts(ctx context.Context, db *sql.DB, t *StarbaseTemplate) error {
	rows, err := db.QueryContext(ctx, `
		SELECT s.idx, s.typeID, COALESCE(it.typeName, ''), s.dx, s.dy, s.dz, s.provisionTypeID, s.online
		  FROM starbaseTemplateStructures s LEFT JOIN invTypes it ON it.typeID = s.typeID
		 WHERE s.templateID = ? ORDER BY s.idx`, t.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var s TemplateStructure
		var online int
		if err := rows.Scan(&s.Idx, &s.TypeID, &s.TypeName, &s.DX, &s.DY, &s.DZ, &s.ProvisionTypeID, &online); err != nil {
			rows.Close()
			return err
		}
		s.Online = online != 0
		t.Structures = append(t.Structures, s)
	}
	rows.Close()
	lrows, err := db.QueryContext(ctx, `
		SELECT typeID, srcIdx, dstIdx FROM starbaseTemplateLinks WHERE templateID = ? ORDER BY srcIdx, typeID`, t.ID)
	if err != nil {
		return err
	}
	defer lrows.Close()
	for lrows.Next() {
		var l TemplateLink
		if err := lrows.Scan(&l.TypeID, &l.SrcIdx, &l.DstIdx); err != nil {
			return err
		}
		t.Links = append(t.Links, l)
	}
	return lrows.Err()
}

// StampOptions are the knobs a stamp takes beyond the placement.
type StampOptions struct {
	Online   bool    // bring the tower and the template's online structures up at once (GM)
	FuelDays float64 // < 0 = the template's own; 0 = none
}

// StampResult reports what a stamp built.
type StampResult struct {
	Plant       PlantResult
	Structures  []int64 // new entityIDs, in template index order
	Links       int
	FuelCycles  int64
	Online      bool
	Live        string
}

// StarbaseRefreshNotifier is optionally implemented by a StarbaseLiveNotifier
// to rebuild a whole tower cluster (tower, force field, structures) on a live
// grid after a stamp.
type StarbaseRefreshNotifier interface {
	StarbaseRefreshed(towerID int64) string
}

// ErrTemplateInvalid wraps a template that fails validation.
var ErrTemplateInvalid = errors.New("template invalid")

// StampStarbaseTemplate builds tpl at req.MoonID for req.CorpID/AllianceID.
// req.TypeID is ignored; the template names the tower.
func StampStarbaseTemplate(ctx context.Context, db *sql.DB, tpl StarbaseTemplate, req PlantRequest,
	opts StampOptions, live StarbaseLiveNotifier) (StampResult, error) {

	var res StampResult
	if err := ValidateTemplateOffsets(tpl, MaxStructureDistance(ctx, db, tpl.TowerTypeID)); err != nil {
		return res, fmt.Errorf("%w: %w", ErrTemplateInvalid, err)
	}
	for _, s := range tpl.Structures {
		ok, err := IsAnchorableStructureType(ctx, db, s.TypeID)
		if err != nil {
			return res, err
		}
		if !ok {
			return res, fmt.Errorf("%w: type %d cannot be anchored to a tower", ErrTemplateInvalid, s.TypeID)
		}
		if why := ForceFieldAnchorRefusal(ctx, db, tpl.TowerTypeID, s.TypeID, s.DX, s.DY, s.DZ); why != "" {
			return res, fmt.Errorf("%w: structure %d: %s", ErrTemplateInvalid, s.Idx, why)
		}
	}
	req.TypeID = tpl.TowerTypeID
	plant, err := PlantStarbaseFor(ctx, db, req, live)
	res.Plant = plant
	if err != nil {
		return res, err
	}
	tower, found, err := GetStarbase(ctx, db, plant.EntityID)
	if err != nil || !found {
		return res, fmt.Errorf("stamped tower %d vanished: %w", plant.EntityID, err)
	}
	ids, nLinks, err := insertTemplateStructures(ctx, db, tower, tpl)
	if err != nil {
		// Leave nothing behind: the tower goes the way /posremove sends it.
		_, _ = RemoveStarbaseFor(ctx, db, tower, live)
		return res, err
	}
	res.Structures, res.Links = ids, nLinks

	days := opts.FuelDays
	if days < 0 {
		days = tpl.FuelDays
	}
	if days > 0 {
		fr, err := FuelStarbaseFor(ctx, db, tower, days)
		if err != nil {
			return res, fmt.Errorf("stamped, but fuelling failed: %w", err)
		}
		res.FuelCycles = fr.Cycles
	}
	if opts.Online {
		if _, err := OnlineStarbase(ctx, db, tower.EntityID); err == nil {
			_, _ = CompleteStarbaseOnlining(ctx, db, tower.EntityID)
			for i, s := range tpl.Structures {
				if !s.Online {
					continue
				}
				if _, err := OnlineStructure(ctx, db, ids[i]); err == nil {
					_, _ = CompleteStructureOnlining(ctx, db, ids[i])
				}
			}
			res.Online = true
		}
	}
	if n, ok := liveOr(live).(StarbaseRefreshNotifier); ok && n != nil {
		res.Live = n.StarbaseRefreshed(tower.EntityID)
	}
	return res, nil
}

// insertTemplateStructures writes every structure (entity + overlay + name)
// and the remapped wiring in one transaction.
func insertTemplateStructures(ctx context.Context, db *sql.DB, tower StarbaseRecord, tpl StarbaseTemplate) ([]int64, int, error) {
	if err := ensureStarbaseStructureTable(ctx, db); err != nil {
		return nil, 0, err
	}
	now := win32TimeNow()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback()
	ids := make([]int64, len(tpl.Structures))
	for i, s := range tpl.Structures {
		var name string
		var groupID int64
		var catID sql.NullInt64
		if err := tx.QueryRowContext(ctx, `
			SELECT t.typeName, t.groupID, g.categoryID FROM invTypes t
			  LEFT JOIN invGroups g ON g.groupID = t.groupID WHERE t.typeID = ?`, s.TypeID,
		).Scan(&name, &groupID, &catID); err != nil {
			return nil, 0, fmt.Errorf("template structure type %d: %w", s.TypeID, err)
		}
		x, y, z := tower.X+s.DX, tower.Y+s.DY, tower.Z+s.DZ
		r, err := tx.ExecContext(ctx, `
			INSERT INTO entity
			  (itemName, typeID, ownerID, locationID, flag, contraband, singleton,
			   quantity, x, y, z, customInfo)
			VALUES (?, ?, ?, ?, 0, 0, 1, 1, ?, ?, ?, NULL)`,
			name, s.TypeID, tower.OwnerCorpID, tower.SolarSystemID, x, y, z)
		if err != nil {
			return nil, 0, fmt.Errorf("template structure entity: %w", err)
		}
		id, err := r.LastInsertId()
		if err != nil {
			return nil, 0, err
		}
		ids[i] = id
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO starbaseStructures
			  (entityID, solarSystemID, towerID, ownerCorpID, ownerAllianceID,
			   state, anchoredAt, onlinedAt, provisionTypeID)
			VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?)`,
			id, tower.SolarSystemID, tower.EntityID, tower.OwnerCorpID, tower.OwnerAllianceID,
			StarbaseStateAnchored, now, s.ProvisionTypeID); err != nil {
			return nil, 0, fmt.Errorf("template structure overlay: %w", err)
		}
		cid := int64(23)
		if catID.Valid && catID.Int64 != 0 {
			cid = catID.Int64
		}
		if _, err := tx.ExecContext(ctx,
			`REPLACE INTO eveNames (itemID, itemName, categoryID, groupID, typeID) VALUES (?, ?, ?, ?, ?)`,
			id, name, cid, groupID, s.TypeID); err != nil {
			return nil, 0, fmt.Errorf("template structure name: %w", err)
		}
	}
	n := 0
	for _, l := range tpl.Links {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO starbaseStructureLinks (towerID, typeID, sourceID, destID) VALUES (?, ?, ?, ?)`,
			tower.EntityID, l.TypeID, ids[l.SrcIdx], ids[l.DstIdx]); err != nil {
			return nil, 0, fmt.Errorf("template link: %w", err)
		}
		n++
	}
	if err := tx.Commit(); err != nil {
		return nil, 0, err
	}
	return ids, n, nil
}
