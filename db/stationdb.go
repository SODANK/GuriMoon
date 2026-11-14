package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"

	"gurimoon/marshal"
)

// StationItemBits holds the five values station.GetStationItemBits returns.
type StationItemBits struct {
	HangarGraphicID int64
	OwnerID         int64
	StationID       int64
	ServiceMask     int64
	StationTypeID   int64
}

// GetStationItemBits reads staStations, staStationTypes and staOperationServices.
// The reply tuple order is hangarGraphicID, ownerID, stationID, serviceMask,
// stationTypeID.
func GetStationItemBits(ctx context.Context, db *sql.DB, stationID int64) (*StationItemBits, error) {
	var bits StationItemBits
	var serviceMask sql.NullInt64
	// hangarGraphicID is NULL on the staStationTypes rows that describe stargate models.
	// It is read nullably: a scan error here would make every dock on that station fail.
	var hangarGraphic sql.NullInt64
	err := db.QueryRowContext(ctx, `
		SELECT
		  staStations.stationID,
		  staStations.stationTypeID,
		  COALESCE(
		      (SELECT c.corporationID FROM corporation c WHERE c.corporationID = staStations.corporationID),
		      1000125) AS ownerID,
		  staStationTypes.hangarGraphicID,
		  CAST(SUM(staOperationServices.serviceID) AS UNSIGNED) AS serviceMask
		FROM staStations
		LEFT JOIN staStationTypes ON staStations.stationTypeID = staStationTypes.stationTypeID
		LEFT JOIN staOperationServices ON staStations.operationID = staOperationServices.operationID
		WHERE staStations.stationID = ?
		GROUP BY staStations.stationID`, stationID,
	).Scan(&bits.StationID, &bits.StationTypeID, &bits.OwnerID, &hangarGraphic, &serviceMask)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("GetStationItemBits: station %d not found", stationID)
		}
		return nil, fmt.Errorf("GetStationItemBits: %w", err)
	}
	if hangarGraphic.Valid {
		bits.HangarGraphicID = hangarGraphic.Int64
	} else {
		log.Printf("[station] station %d type %d has no hangarGraphicID — the hangar will not render",
			stationID, bits.StationTypeID)
	}
	if serviceMask.Valid {
		bits.ServiceMask = serviceMask.Int64
	}
	return &bits, nil
}

// doGetStationCols is the SELECT column list GetStation scans.
var doGetStationCols = []string{
	"stationID", "security", "dockingCostPerVolume", "maxShipVolumeDockable",
	"officeRentalCost", "operationID", "stationTypeID", "ownerID",
	"solarSystemID", "constellationID", "regionID", "stationName",
	"x", "y", "z", "reprocessingEfficiency", "reprocessingStationsTake",
	"reprocessingHangarFlag", "description", "serviceMask",
}

// GetStation returns a util.KeyVal for the given stationID.
func GetStation(ctx context.Context, db *sql.DB, stationID int64) (marshal.PyRep, error) {
	var (
		stID, sec                        int64
		dockCostPerVol                   float64
		maxShipVol                       float64
		officeRental                     float64
		operationID, stTypeID, ownerID   int64
		solarSysID, constellID, regionID int64
		stName                           string
		x, y, z                          float64
		reproEff, reproTake              float64
		reproHangarFlag                  int64
		description                      sql.NullString
		serviceMask                      sql.NullInt64
	)
	err := db.QueryRowContext(ctx, `
		SELECT
		  staStations.stationID, staStations.security,
		  staStations.dockingCostPerVolume, staStations.maxShipVolumeDockable,
		  staStations.officeRentalCost, staStations.operationID,
		  staStations.stationTypeID,
		  COALESCE(
		      (SELECT c.corporationID FROM corporation c WHERE c.corporationID = staStations.corporationID),
		      1000125) AS ownerID,
		  staStations.solarSystemID, staStations.constellationID, staStations.regionID,
		  staStations.stationName, staStations.x, staStations.y, staStations.z,
		  staStations.reprocessingEfficiency, staStations.reprocessingStationsTake,
		  staStations.reprocessingHangarFlag,
		  staOperations.description,
		  CAST(SUM(staOperationServices.serviceID) AS UNSIGNED) AS serviceMask
		FROM staStations
		LEFT JOIN staOperations ON staStations.operationID = staOperations.operationID
		LEFT JOIN staOperationServices ON staStations.operationID = staOperationServices.operationID
		WHERE staStations.stationID = ?
		GROUP BY staStations.stationID`, stationID,
	).Scan(
		&stID, &sec,
		&dockCostPerVol, &maxShipVol,
		&officeRental, &operationID,
		&stTypeID, &ownerID,
		&solarSysID, &constellID, &regionID,
		&stName, &x, &y, &z,
		&reproEff, &reproTake,
		&reproHangarFlag,
		&description,
		&serviceMask,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("GetStation: station %d not found", stationID)
		}
		return nil, fmt.Errorf("GetStation: %w", err)
	}

	var smVal int64
	if serviceMask.Valid {
		smVal = serviceMask.Int64
	}

	vals := []marshal.PyRep{
		intRep(stID), intRep(sec),
		floatRep(dockCostPerVol), floatRep(maxShipVol),
		floatRep(officeRental), intRep(operationID),
		intRep(stTypeID), intRep(ownerID),
		intRep(solarSysID), intRep(constellID), intRep(regionID),
		strRep(stName), floatRep(x), floatRep(y), floatRep(z),
		floatRep(reproEff), floatRep(reproTake),
		intRep(reproHangarFlag),
		nullStrRep(description),
		intRep(smVal),
	}
	return makeKeyVal(doGetStationCols, vals), nil
}

// solarSystemCols is the SELECT column list GetSolarSystem scans.
var solarSystemCols = []string{
	"solarSystemID", "solarSystemName", "x", "y", "z", "radius",
	"security", "constellationID", "factionID",
	"sunTypeID", "regionID", "allianceID",
}

// SolarSystemRowChanged is told when a mapSolarSystems row's factionID or allianceID is
// rewritten, so the memoised stationSvc.GetSolarSystem reply is dropped. systemID 0 means
// a region-wide edit. Installed by main; nil does nothing.
var SolarSystemRowChanged func(systemID int64)

func solarSystemRowChanged(systemID int64) {
	if hook := SolarSystemRowChanged; hook != nil {
		hook(systemID)
	}
}

// GetSolarSystem returns a "dbrow.SolarSystemRow" typed row for solarSystemID.
func GetSolarSystem(ctx context.Context, db *sql.DB, solarSystemID int64) (marshal.PyRep, error) {
	var (
		ssID, sunTypeID, regionID, constellID int64
		ssName                                string
		x, y, z, radius, security             float64
		factionID, allianceID                 sql.NullInt64
	)
	err := db.QueryRowContext(ctx, `
		SELECT solarSystemID, solarSystemName, x, y, z, radius,
		       security, constellationID, factionID,
		       sunTypeID, regionID, allianceID
		FROM mapSolarSystems
		WHERE solarSystemID = ?`, solarSystemID,
	).Scan(
		&ssID, &ssName, &x, &y, &z, &radius,
		&security, &constellID, &factionID,
		&sunTypeID, &regionID, &allianceID,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("GetSolarSystem: solar system %d not found", solarSystemID)
		}
		return nil, fmt.Errorf("GetSolarSystem: %w", err)
	}

	vals := []marshal.PyRep{
		intRep(ssID), strRep(ssName),
		floatRep(x), floatRep(y), floatRep(z), floatRep(radius),
		floatRep(security), intRep(constellID), nullIntRep(factionID),
		intRep(sunTypeID), intRep(regionID), nullIntRep(allianceID),
	}
	return makeTypedRow("dbrow.SolarSystemRow", solarSystemCols, vals), nil
}

// GetSolarSystemEntityRow builds a util.Row (EntityRowObject) for a solar system from
// mapSolarSystems, since the entity table holds no solar systems: typeID 5, groupID 5,
// categoryID 2.
func GetSolarSystemEntityRow(ctx context.Context, db *sql.DB, solarSystemID int64) (*EntityRow, error) {
	var constellationID int64
	err := db.QueryRowContext(ctx,
		`SELECT constellationID FROM mapSolarSystems WHERE solarSystemID = ?`,
		solarSystemID,
	).Scan(&constellationID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetSolarSystemEntityRow: %w", err)
	}
	return &EntityRow{
		ItemID:     solarSystemID,
		TypeID:     5, // Solar System
		OwnerID:    1, // universe owner
		LocationID: constellationID,
		Flag:       0,
		GroupID:    5,
		CategoryID: 2, // Celestial
		Singleton:  1,
		Quantity:   1,
	}, nil
}

// UpdateStationOwner changes the owning corporation of a station, for a captured
// conquerable station.
func UpdateStationOwner(ctx context.Context, db *sql.DB, stationID, newCorpID int64) error {
	_, err := db.ExecContext(ctx,
		`UPDATE staStations SET corporationID = ? WHERE stationID = ?`,
		newCorpID, stationID)
	if err != nil {
		return fmt.Errorf("UpdateStationOwner stationID=%d: %w", stationID, err)
	}
	return nil
}

// agentCols is the column list of the client's agents.py cache.
var agentCols = []string{
	"agentID", "agentTypeID", "divisionID", "level",
	"stationID", "bloodlineID", "quality", "corporationID", "gender",
}

// GetAgents returns all agents as a util.Rowset for agentMgr.GetAgents; the client
// caches it and filters by stationID. quality is never None: storyline and research
// agents carry NULL in agtAgents, and the star map's agent overlay formats every agent
// with 'Q:%d', so a None stops its loading tasklet.
func GetAgents(ctx context.Context, db *sql.DB) (marshal.PyRep, error) {
	return queryRows(ctx, db, agentCols,
		`SELECT agentID, agentTypeID, divisionID, level,
		        stationID, bloodlineID, COALESCE(quality, 0), corporationID, gender
		 FROM agtAgents`)
}
