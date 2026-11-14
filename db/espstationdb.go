package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ---- ESP Station detail page ----

// ESPStationDetail is the full detail for a single station.
type ESPStationDetail struct {
	StationID     int64
	StationName   string
	Security      int64
	DockingCost   float64
	MaxShipVolume int64
	OfficeRental  int64

	OperationID   int64
	OperationName string
	OperationDesc string

	StationTypeID int64
	TypeName      string
	Conquerable   bool
	OfficeSlots   int64

	CorporationID int64
	CorpName      string

	SolarSystemID     int64
	SystemName        string
	SystemSecurity    float64
	ConstellationID   int64
	ConstellationName string
	RegionID          int64
	RegionName        string

	X, Y, Z float64

	ReprocEfficiency float64
	ReprocTake       float64
	ReprocHangarFlag int64

	ServiceMask int64
	DockedCount int64
}

// ESPGetStationDetail returns everything on the station detail header, or nil
// when the station does not exist.
func ESPGetStationDetail(ctx context.Context, db *sql.DB, stationID int64) (*ESPStationDetail, error) {
	var d ESPStationDetail
	var serviceMask sql.NullInt64
	var officeSlots sql.NullInt64
	err := db.QueryRowContext(ctx, `
		SELECT s.stationID, s.stationName, s.security,
		       s.dockingCostPerVolume, s.maxShipVolumeDockable, s.officeRentalCost,
		       COALESCE(s.operationID, 0), COALESCE(op.operationName, ''), COALESCE(op.description, ''),
		       s.stationTypeID, COALESCE(t.typeName, ''),
		       COALESCE(st.conquerable, 0), st.officeSlots,
		       s.corporationID,
		       COALESCE(corp.corporationName, COALESCE(eso.ownerName, '')),
		       s.solarSystemID, COALESCE(sys.solarSystemName, ''), COALESCE(sys.security, 0),
		       s.constellationID, COALESCE(con.constellationName, ''),
		       s.regionID, COALESCE(rg.regionName, ''),
		       s.x, s.y, s.z,
		       s.reprocessingEfficiency, s.reprocessingStationsTake, s.reprocessingHangarFlag,
		       CAST(COALESCE((SELECT SUM(os.serviceID) FROM staOperationServices os
		                      WHERE os.operationID = s.operationID), 0) AS SIGNED),
		       (SELECT COUNT(*) FROM character_ c
		        WHERE c.stationID = s.stationID AND c.accountID > 0)
		FROM staStations s
		LEFT JOIN staOperations op ON op.operationID = s.operationID
		LEFT JOIN invTypes t ON t.typeID = s.stationTypeID
		LEFT JOIN staStationTypes st ON st.stationTypeID = s.stationTypeID
		LEFT JOIN corporation corp ON corp.corporationID = s.corporationID
		LEFT JOIN eveStaticOwners eso ON eso.ownerID = s.corporationID
		LEFT JOIN mapSolarSystems sys ON sys.solarSystemID = s.solarSystemID
		LEFT JOIN mapConstellations con ON con.constellationID = s.constellationID
		LEFT JOIN mapRegions rg ON rg.regionID = s.regionID
		WHERE s.stationID = ?`, stationID,
	).Scan(
		&d.StationID, &d.StationName, &d.Security,
		&d.DockingCost, &d.MaxShipVolume, &d.OfficeRental,
		&d.OperationID, &d.OperationName, &d.OperationDesc,
		&d.StationTypeID, &d.TypeName,
		&d.Conquerable, &officeSlots,
		&d.CorporationID, &d.CorpName,
		&d.SolarSystemID, &d.SystemName, &d.SystemSecurity,
		&d.ConstellationID, &d.ConstellationName,
		&d.RegionID, &d.RegionName,
		&d.X, &d.Y, &d.Z,
		&d.ReprocEfficiency, &d.ReprocTake, &d.ReprocHangarFlag,
		&serviceMask, &d.DockedCount,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("ESPGetStationDetail: %w", err)
	}
	if serviceMask.Valid {
		d.ServiceMask = serviceMask.Int64
	}
	if officeSlots.Valid {
		d.OfficeSlots = officeSlots.Int64
	}
	return &d, nil
}

// ---- Station models ----
// A station's hull is its type's graphic (staStations.stationTypeID to
// invTypes.graphicID to eveGraphics.url3D). Space draws the ball from
// mapDenormalize.typeID, so a swap writes both columns.
// See docs/db-9-notes.md#station-type-swap.

// ESPStationTypeOption is one selectable station model.
type ESPStationTypeOption struct {
	StationTypeID int64
	TypeName      string
	GroupID       int64
	GroupName     string
	GraphicID     int64
	URL3D         string
	Conquerable   bool
	OfficeSlots   int64
	DockEntryX    float64
	DockEntryY    float64
	DockEntryZ    float64
	InUse         int64 // stations currently using this type

	// Docked-view graphics are type-level (GetStationItemBits joins staStationTypes
	// on stationTypeID) and follow a swap. Zero means the column is NULL.
	HangarGraphicID     int64
	DockingBayGraphicID int64

	// Derived from URL3D for display; the shipped type names are generic.
	ModelFamily string // "Station / Gallente"
	ModelName   string // "gs3"
	HasModel    bool   // a converted .glb exists (filled in by the ESP layer)
}

// ESPListStationTypes returns every station type that can be assigned, with
// its model path and how many stations already wear it.
func ESPListStationTypes(ctx context.Context, db *sql.DB) ([]ESPStationTypeOption, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT st.stationTypeID, COALESCE(t.typeName, ''),
		       COALESCE(t.groupID, 0), COALESCE(g.groupName, ''),
		       COALESCE(t.graphicID, 0), COALESCE(gr.url3D, ''),
		       st.conquerable, COALESCE(st.officeSlots, 0),
		       st.dockEntryX, st.dockEntryY, st.dockEntryZ,
		       COALESCE(u.cnt, 0),
		       COALESCE(st.hangarGraphicID, 0), COALESCE(st.dockingBayGraphicID, 0)
		FROM staStationTypes st
		LEFT JOIN invTypes t ON t.typeID = st.stationTypeID
		LEFT JOIN invGroups g ON g.groupID = t.groupID
		LEFT JOIN eveGraphics gr ON gr.graphicID = t.graphicID
		LEFT JOIN (SELECT stationTypeID, COUNT(*) cnt FROM staStations
		           GROUP BY stationTypeID) u ON u.stationTypeID = st.stationTypeID
		ORDER BY COALESCE(t.typeName, CAST(st.stationTypeID AS CHAR))`)
	if err != nil {
		return nil, fmt.Errorf("ESPListStationTypes: %w", err)
	}
	defer rows.Close()
	var out []ESPStationTypeOption
	for rows.Next() {
		var o ESPStationTypeOption
		if err := rows.Scan(&o.StationTypeID, &o.TypeName,
			&o.GroupID, &o.GroupName, &o.GraphicID, &o.URL3D,
			&o.Conquerable, &o.OfficeSlots,
			&o.DockEntryX, &o.DockEntryY, &o.DockEntryZ, &o.InUse,
			&o.HangarGraphicID, &o.DockingBayGraphicID); err != nil {
			return nil, fmt.Errorf("ESPListStationTypes scan: %w", err)
		}
		o.ModelFamily, o.ModelName = ModelLabel(o.URL3D)
		if o.TypeName == "" {
			o.TypeName = fmt.Sprintf("Type %d", o.StationTypeID)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ModelLabel splits a url3D into a family and a model name for display. A
// trailing directory that repeats the file name is dropped.
//
//	res:/Model/Station/Gallente/gs3/gs3.blue  -> "Station / Gallente", "gs3"
//	res:/Model/Station/Amarr/as_mining.blue   -> "Station / Amarr",    "as_mining"
func ModelLabel(url3D string) (family, name string) {
	p := strings.TrimPrefix(url3D, "res:/")
	p = strings.TrimPrefix(p, "Model/")
	p = strings.TrimSuffix(p, ".blue")
	if p == "" {
		return "", ""
	}
	parts := strings.Split(strings.Trim(strings.ReplaceAll(p, "\\", "/"), "/"), "/")
	name = parts[len(parts)-1]
	dirs := parts[:len(parts)-1]
	if n := len(dirs); n > 0 && strings.EqualFold(dirs[n-1], name) {
		dirs = dirs[:n-1]
	}
	for i, d := range dirs {
		if d != "" {
			dirs[i] = strings.ToUpper(d[:1]) + d[1:]
		}
	}
	return strings.Join(dirs, " / "), name
}

// ESPSetStationType swaps a station's model. It writes staStations.stationTypeID
// and mapDenormalize.typeID in one transaction, leaves mapDenormalize.groupID
// alone, and refuses a type with a NULL hangarGraphicID.
// See docs/db-9-notes.md#station-type-swap.
func ESPSetStationType(ctx context.Context, db *sql.DB, stationID, stationTypeID int64) error {
	var hangarGraphic sql.NullInt64
	err := db.QueryRowContext(ctx,
		`SELECT hangarGraphicID FROM staStationTypes WHERE stationTypeID = ?`,
		stationTypeID).Scan(&hangarGraphic)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("station type %d has no staStationTypes row", stationTypeID)
	}
	if err != nil {
		return fmt.Errorf("ESPSetStationType lookup: %w", err)
	}
	if !hangarGraphic.Valid {
		return fmt.Errorf("station type %d has no hangar graphic (staStationTypes.hangarGraphicID "+
			"is NULL) — a station wearing it cannot be docked into", stationTypeID)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ESPSetStationType begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`UPDATE staStations SET stationTypeID = ? WHERE stationID = ?`, stationTypeID, stationID)
	if err != nil {
		return fmt.Errorf("ESPSetStationType staStations: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// RowsAffected is 0 for an unchanged value too, so confirm the station exists.
		var found int
		if err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM staStations WHERE stationID = ?`, stationID).Scan(&found); err != nil {
			return fmt.Errorf("station %d not found", stationID)
		}
	}
	// The ball's radius is read from this row and the client sizes the render
	// bounds from it, so it moves with the model.
	if _, err := tx.ExecContext(ctx,
		`UPDATE mapDenormalize d JOIN invTypes t ON t.typeID = ?
		 SET d.typeID = t.typeID, d.radius = IF(t.radius > 0, t.radius, d.radius)
		 WHERE d.itemID = ?`, stationTypeID, stationID); err != nil {
		return fmt.Errorf("ESPSetStationType mapDenormalize: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("ESPSetStationType commit: %w", err)
	}
	return nil
}

// ---- ESP Station browser (the /stations index) ----

// ESPStationListRow is one row of the station browser. The page filters
// client-side over the rendered rows, so a row carries every field it is
// searched by (region and constellation names included).
type ESPStationListRow struct {
	StationID         int64
	StationName       string
	SolarSystemID     int64
	SystemName        string
	Security          float64
	ConstellationID   int64
	ConstellationName string
	RegionID          int64
	RegionName        string
	CorporationID     int64
	CorpName          string
	OperationName     string
	Conquerable       bool
	// Players berthed here, online or not. NPC agents also live in character_ with
	// a stationID, so the count filters accountID > 0.
	DockedCount int64
	AgentCount  int64
	OrderCount  int64
}

// ESPStationBrowseFilter narrows the station browser. The zero value lists
// every station.
type ESPStationBrowseFilter struct {
	Query           string // matched against station, system, constellation and region names
	RegionID        int64
	ConstellationID int64
}

// ESPListStations returns stations in alphabetical order, narrowed by f.
// Per-station counts come from derived tables: a correlated subquery per row
// over the whole game would rescan character_, agtAgents and market_orders.
func ESPListStations(ctx context.Context, db *sql.DB, f ESPStationBrowseFilter) ([]ESPStationListRow, error) {
	query := `
		SELECT s.stationID, s.stationName,
		       s.solarSystemID, COALESCE(sys.solarSystemName, ''), COALESCE(sys.security, 0),
		       s.constellationID, COALESCE(con.constellationName, ''),
		       s.regionID, COALESCE(rg.regionName, ''),
		       s.corporationID, COALESCE(corp.corporationName, COALESCE(eso.ownerName, '')),
		       COALESCE(op.operationName, ''), COALESCE(st.conquerable, 0),
		       COALESCE(d.cnt, 0), COALESCE(ag.cnt, 0), COALESCE(mo.cnt, 0)
		FROM staStations s
		LEFT JOIN mapSolarSystems sys ON sys.solarSystemID = s.solarSystemID
		LEFT JOIN mapConstellations con ON con.constellationID = s.constellationID
		LEFT JOIN mapRegions rg ON rg.regionID = s.regionID
		LEFT JOIN corporation corp ON corp.corporationID = s.corporationID
		LEFT JOIN eveStaticOwners eso ON eso.ownerID = s.corporationID
		LEFT JOIN staOperations op ON op.operationID = s.operationID
		LEFT JOIN staStationTypes st ON st.stationTypeID = s.stationTypeID
		LEFT JOIN (SELECT stationID, COUNT(*) cnt FROM character_
		           WHERE accountID > 0 GROUP BY stationID) d
		       ON d.stationID = s.stationID
		LEFT JOIN (SELECT stationID, COUNT(*) cnt FROM agtAgents GROUP BY stationID) ag
		       ON ag.stationID = s.stationID
		LEFT JOIN (SELECT stationID, COUNT(*) cnt FROM market_orders
		           WHERE orderState = 1 AND volRemaining > 0 GROUP BY stationID) mo
		       ON mo.stationID = s.stationID
		WHERE 1=1`
	var args []any
	if f.RegionID > 0 {
		query += ` AND s.regionID = ?`
		args = append(args, f.RegionID)
	}
	if f.ConstellationID > 0 {
		query += ` AND s.constellationID = ?`
		args = append(args, f.ConstellationID)
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		query += ` AND (s.stationName LIKE ? OR sys.solarSystemName LIKE ?
		            OR con.constellationName LIKE ? OR rg.regionName LIKE ?)`
		like := "%" + q + "%"
		args = append(args, like, like, like, like)
	}
	query += ` ORDER BY s.stationName`

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("ESPListStations: %w", err)
	}
	defer rows.Close()

	var results []ESPStationListRow
	for rows.Next() {
		var r ESPStationListRow
		if err := rows.Scan(&r.StationID, &r.StationName,
			&r.SolarSystemID, &r.SystemName, &r.Security,
			&r.ConstellationID, &r.ConstellationName,
			&r.RegionID, &r.RegionName,
			&r.CorporationID, &r.CorpName,
			&r.OperationName, &r.Conquerable,
			&r.DockedCount, &r.AgentCount, &r.OrderCount); err != nil {
			return nil, fmt.Errorf("ESPListStations scan: %w", err)
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// ESPStationPlaceOption is a region or constellation in the station browser's
// filter dropdowns, with the number of stations it holds.
type ESPStationPlaceOption struct {
	ID       int64
	Name     string
	RegionID int64 // constellations only, for the region select
	Stations int64
}

// ESPStationRegions lists regions that hold at least one station.
func ESPStationRegions(ctx context.Context, db *sql.DB) ([]ESPStationPlaceOption, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT s.regionID, COALESCE(rg.regionName, ''), COUNT(*)
		FROM staStations s
		LEFT JOIN mapRegions rg ON rg.regionID = s.regionID
		GROUP BY s.regionID, rg.regionName
		ORDER BY rg.regionName`)
	if err != nil {
		return nil, fmt.Errorf("ESPStationRegions: %w", err)
	}
	defer rows.Close()
	var out []ESPStationPlaceOption
	for rows.Next() {
		var o ESPStationPlaceOption
		if err := rows.Scan(&o.ID, &o.Name, &o.Stations); err != nil {
			return nil, fmt.Errorf("ESPStationRegions scan: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ESPStationConstellations lists constellations that hold at least one station.
func ESPStationConstellations(ctx context.Context, db *sql.DB) ([]ESPStationPlaceOption, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT s.constellationID, COALESCE(con.constellationName, ''), s.regionID, COUNT(*)
		FROM staStations s
		LEFT JOIN mapConstellations con ON con.constellationID = s.constellationID
		GROUP BY s.constellationID, con.constellationName, s.regionID
		ORDER BY con.constellationName`)
	if err != nil {
		return nil, fmt.Errorf("ESPStationConstellations: %w", err)
	}
	defer rows.Close()
	var out []ESPStationPlaceOption
	for rows.Next() {
		var o ESPStationPlaceOption
		if err := rows.Scan(&o.ID, &o.Name, &o.RegionID, &o.Stations); err != nil {
			return nil, fmt.Errorf("ESPStationConstellations scan: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ESPStationServiceRow is one service provided by a station's operation.
type ESPStationServiceRow struct {
	ServiceID   int64
	ServiceName string
	Description string
}

// ESPGetStationServices lists the services granted by an operationID.
func ESPGetStationServices(ctx context.Context, db *sql.DB, operationID int64) ([]ESPStationServiceRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT sv.serviceID, sv.serviceName, sv.description
		FROM staOperationServices os
		JOIN staServices sv ON sv.serviceID = os.serviceID
		WHERE os.operationID = ?
		ORDER BY sv.serviceName`, operationID)
	if err != nil {
		return nil, fmt.Errorf("ESPGetStationServices: %w", err)
	}
	defer rows.Close()
	var results []ESPStationServiceRow
	for rows.Next() {
		var r ESPStationServiceRow
		if err := rows.Scan(&r.ServiceID, &r.ServiceName, &r.Description); err != nil {
			return nil, fmt.Errorf("ESPGetStationServices scan: %w", err)
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// ESPOperationOption is a staOperations row for the operation dropdown,
// with its service names pre-joined for display.
type ESPOperationOption struct {
	OperationID   int64
	OperationName string
	Services      string
}

// ESPListStationOperations returns all operations with their service lists.
func ESPListStationOperations(ctx context.Context, db *sql.DB) ([]ESPOperationOption, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT op.operationID, op.operationName,
		       COALESCE(GROUP_CONCAT(sv.serviceName ORDER BY sv.serviceName SEPARATOR ', '), '')
		FROM staOperations op
		LEFT JOIN staOperationServices os ON os.operationID = op.operationID
		LEFT JOIN staServices sv ON sv.serviceID = os.serviceID
		GROUP BY op.operationID, op.operationName
		ORDER BY op.operationName`)
	if err != nil {
		return nil, fmt.Errorf("ESPListStationOperations: %w", err)
	}
	defer rows.Close()
	var results []ESPOperationOption
	for rows.Next() {
		var r ESPOperationOption
		if err := rows.Scan(&r.OperationID, &r.OperationName, &r.Services); err != nil {
			return nil, fmt.Errorf("ESPListStationOperations scan: %w", err)
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// ESPDockedCharacter is a character currently docked at the station.
type ESPDockedCharacter struct {
	CharacterID int64
	Name        string
	CorpName    string
	Balance     float64
	Online      bool // filled by the caller from the session registry
}

// ESPGetStationDockedChars lists characters whose home stationID matches.
func ESPGetStationDockedChars(ctx context.Context, db *sql.DB, stationID int64) ([]ESPDockedCharacter, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT c.characterID, c.characterName,
		       COALESCE(corp.corporationName, ''), c.balance
		FROM character_ c
		LEFT JOIN corporation corp ON corp.corporationID = c.corporationID
		WHERE c.stationID = ? AND c.accountID > 0
		ORDER BY c.characterName
		LIMIT 200`, stationID)
	if err != nil {
		return nil, fmt.Errorf("ESPGetStationDockedChars: %w", err)
	}
	defer rows.Close()
	var results []ESPDockedCharacter
	for rows.Next() {
		var r ESPDockedCharacter
		if err := rows.Scan(&r.CharacterID, &r.Name, &r.CorpName, &r.Balance); err != nil {
			return nil, fmt.Errorf("ESPGetStationDockedChars scan: %w", err)
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// ESPStationMarketStats summarizes live market orders at a station.
type ESPStationMarketStats struct {
	SellOrders    int64
	BuyOrders     int64
	DistinctTypes int64
}

// ESPGetStationMarketStats counts active orders at the station. Active is
// orderState = 1, the value every insert path writes.
func ESPGetStationMarketStats(ctx context.Context, db *sql.DB, stationID int64) (ESPStationMarketStats, error) {
	var s ESPStationMarketStats
	err := db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(bid = 0), 0), COALESCE(SUM(bid = 1), 0), COUNT(DISTINCT typeID)
		FROM market_orders
		WHERE stationID = ? AND orderState = 1 AND volRemaining > 0`, stationID,
	).Scan(&s.SellOrders, &s.BuyOrders, &s.DistinctTypes)
	if err != nil {
		return s, fmt.Errorf("ESPGetStationMarketStats: %w", err)
	}
	return s, nil
}

// ESPStationOrderRow is one market order shown on the station page.
type ESPStationOrderRow struct {
	OrderID      int64
	TypeID       int64
	TypeName     string
	IsBuy        bool
	Price        float64
	VolRemaining int64
	VolEntered   int64
	OwnerName    string
}

// ESPGetStationOrders lists active orders at the station, sells then buys.
func ESPGetStationOrders(ctx context.Context, db *sql.DB, stationID int64, limit int) ([]ESPStationOrderRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT o.orderID, o.typeID, COALESCE(t.typeName, ''), o.bid,
		       o.price, o.volRemaining, o.volEntered,
		       COALESCE(c.characterName, COALESCE(eso.ownerName, CAST(o.charID AS CHAR)))
		FROM market_orders o
		LEFT JOIN invTypes t ON t.typeID = o.typeID
		LEFT JOIN character_ c ON c.characterID = o.charID
		LEFT JOIN eveStaticOwners eso ON eso.ownerID = o.charID
		WHERE o.stationID = ? AND o.orderState = 1 AND o.volRemaining > 0
		ORDER BY o.bid, t.typeName, o.price
		LIMIT ?`, stationID, limit)
	if err != nil {
		return nil, fmt.Errorf("ESPGetStationOrders: %w", err)
	}
	defer rows.Close()
	var results []ESPStationOrderRow
	for rows.Next() {
		var r ESPStationOrderRow
		var bid int64
		if err := rows.Scan(&r.OrderID, &r.TypeID, &r.TypeName, &bid,
			&r.Price, &r.VolRemaining, &r.VolEntered, &r.OwnerName); err != nil {
			return nil, fmt.Errorf("ESPGetStationOrders scan: %w", err)
		}
		r.IsBuy = bid != 0
		results = append(results, r)
	}
	return results, rows.Err()
}

// ESPStationAssetOwnerRow aggregates stored items at a station per owner.
type ESPStationAssetOwnerRow struct {
	OwnerID   int64
	OwnerName string
	ItemCount int64
	IsChar    bool
}

// ESPGetStationAssetOwners returns the top owners by item count with items
// located at the station (entity.locationID = stationID).
func ESPGetStationAssetOwners(ctx context.Context, db *sql.DB, stationID int64, limit int) ([]ESPStationAssetOwnerRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT e.ownerID,
		       COALESCE(c.characterName, COALESCE(eso.ownerName, CAST(e.ownerID AS CHAR))),
		       COUNT(*) AS itemCount,
		       (c.characterID IS NOT NULL)
		FROM entity e
		LEFT JOIN character_ c ON c.characterID = e.ownerID
		LEFT JOIN eveStaticOwners eso ON eso.ownerID = e.ownerID
		WHERE e.locationID = ?
		GROUP BY e.ownerID
		ORDER BY itemCount DESC
		LIMIT ?`, stationID, limit)
	if err != nil {
		return nil, fmt.Errorf("ESPGetStationAssetOwners: %w", err)
	}
	defer rows.Close()
	var results []ESPStationAssetOwnerRow
	for rows.Next() {
		var r ESPStationAssetOwnerRow
		if err := rows.Scan(&r.OwnerID, &r.OwnerName, &r.ItemCount, &r.IsChar); err != nil {
			return nil, fmt.Errorf("ESPGetStationAssetOwners scan: %w", err)
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// ESPStationUpdate carries the editable staStations fields from the ESP form.
type ESPStationUpdate struct {
	StationName      string
	CorporationID    int64
	OperationID      int64
	Security         int64
	DockingCost      float64
	MaxShipVolume    int64
	OfficeRental     int64
	ReprocEfficiency float64
	ReprocTake       float64
}

// ESPUpdateStation writes the editable fields back to staStations. A name
// change also syncs eveNames and mapDenormalize so itemID to name lookups
// agree with the station row.
func ESPUpdateStation(ctx context.Context, db *sql.DB, stationID int64, u ESPStationUpdate) error {
	name := strings.TrimSpace(u.StationName)
	if name == "" {
		return fmt.Errorf("ESPUpdateStation: station name cannot be empty")
	}
	res, err := db.ExecContext(ctx, `
		UPDATE staStations SET
		  stationName = ?, corporationID = ?, operationID = ?, security = ?,
		  dockingCostPerVolume = ?, maxShipVolumeDockable = ?, officeRentalCost = ?,
		  reprocessingEfficiency = ?, reprocessingStationsTake = ?
		WHERE stationID = ?`,
		name, u.CorporationID, u.OperationID, u.Security,
		u.DockingCost, u.MaxShipVolume, u.OfficeRental,
		u.ReprocEfficiency, u.ReprocTake, stationID)
	if err != nil {
		return fmt.Errorf("ESPUpdateStation: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// RowsAffected is 0 for both "no such station" and "nothing changed"; an
		// existence probe tells them apart.
		var exists int64
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM staStations WHERE stationID = ?`, stationID).Scan(&exists); err == nil && exists == 0 {
			return fmt.Errorf("ESPUpdateStation: station %d not found", stationID)
		}
	}
	// Best-effort name sync: these tables may lack a row for a station.
	if _, err := db.ExecContext(ctx,
		`UPDATE eveNames SET itemName = ? WHERE itemID = ?`, name, stationID); err != nil {
		return fmt.Errorf("ESPUpdateStation eveNames: %w", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE mapDenormalize SET itemName = ? WHERE itemID = ?`, name, stationID); err != nil {
		return fmt.Errorf("ESPUpdateStation mapDenormalize: %w", err)
	}
	return nil
}
