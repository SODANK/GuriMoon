package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Custom stargates are admin-built gate pairs joining two existing systems. A pair is two
// mapDenormalize rows (groupID 10), two mapJumps rows and two mapSolarSystemJumps rows, all
// written in one transaction and tracked in customStargates so a delete never touches shipped
// map data. itemIDs come from [CustomStargateIDFloor, CustomStargateIDCeil], inside the client's
// stargate band (minStargate..maxStargate, sql/eveconstants.sql) and above the shipped gates.
// See docs/stargates.md.
const (
	CustomStargateIDFloor = 59_000_000
	CustomStargateIDCeil  = 59_999_999

	// DefaultStargateTypeID is Stargate (Gallente 3), the type of most shipped nullsec gates.
	DefaultStargateTypeID = 3876

	stargateGroupID = 10
)

// EnsureCustomStargateTable creates the customStargates tracking table. Call once at startup
// after opening the DB.
func EnsureCustomStargateTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS customStargates (
			gateID          BIGINT PRIMARY KEY,
			pairGateID      BIGINT NOT NULL,
			solarSystemID   INT NOT NULL,
			toSolarSystemID INT NOT NULL,
			anchorID        BIGINT NOT NULL DEFAULT 0,
			createdAt       TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			INDEX idx_cgate_system (solarSystemID)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`)
	if err != nil {
		return fmt.Errorf("EnsureCustomStargateTable: %w", err)
	}
	return nil
}

// StargateSystemInfo is the mapSolarSystems data a gate row and the placement math need.
// X, Y, Z are the system's galactic position.
type StargateSystemInfo struct {
	SolarSystemID   int64
	Name            string
	ConstellationID int64
	RegionID        int64
	Security        float64
	X, Y, Z         float64
}

// GetStargateSystemInfo reads one system's name, parents, security and galactic position.
// ok is false when the system does not exist.
func GetStargateSystemInfo(ctx context.Context, db *sql.DB, solarSystemID int64) (StargateSystemInfo, bool, error) {
	return scanStargateSystemInfo(db.QueryRowContext(ctx, stargateSystemInfoQuery, solarSystemID))
}

const stargateSystemInfoQuery = `
	SELECT solarSystemID, solarSystemName,
	       COALESCE(constellationID, 0), COALESCE(regionID, 0), COALESCE(security, 0),
	       COALESCE(x, 0), COALESCE(y, 0), COALESCE(z, 0)
	  FROM mapSolarSystems
	 WHERE solarSystemID = ?`

func scanStargateSystemInfo(row *sql.Row) (StargateSystemInfo, bool, error) {
	var s StargateSystemInfo
	err := row.Scan(&s.SolarSystemID, &s.Name, &s.ConstellationID, &s.RegionID,
		&s.Security, &s.X, &s.Y, &s.Z)
	if errors.Is(err, sql.ErrNoRows) {
		return s, false, nil
	}
	if err != nil {
		return s, false, fmt.Errorf("GetStargateSystemInfo: %w", err)
	}
	return s, true, nil
}

// StargateEnd is one side of a new pair: the system, the celestial the gate is placed by, the
// gate's in-system position and its type.
type StargateEnd struct {
	SolarSystemID int64
	AnchorID      int64
	TypeID        int64
	X, Y, Z       float64
}

// StargatePair is a pair that InsertStargatePair wrote. GateA sits in A's system and jumps to
// GateB.
type StargatePair struct {
	GateA, GateB     int64
	SystemA, SystemB int64
}

// SystemsJoined reports whether mapSolarSystemJumps already joins the two systems. Its primary
// key allows one gate per system pair.
func SystemsJoined(ctx context.Context, db *sql.DB, a, b int64) (bool, error) {
	var n int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM mapSolarSystemJumps
		 WHERE (fromSolarSystemID = ? AND toSolarSystemID = ?)
		    OR (fromSolarSystemID = ? AND toSolarSystemID = ?)`,
		a, b, b, a).Scan(&n); err != nil {
		return false, fmt.Errorf("SystemsJoined: %w", err)
	}
	return n > 0, nil
}

// InsertStargatePair writes a new gate pair joining a.SolarSystemID and b.SolarSystemID in one
// transaction: two gate rows, the two mapJumps directions, the two mapSolarSystemJumps
// directions and the two tracking rows. It refuses a pair of systems already joined, a system
// joined to itself and a type that is not a stargate. Live systems are not touched here; the
// caller presents the new balls.
func InsertStargatePair(ctx context.Context, db *sql.DB, a, b StargateEnd) (StargatePair, error) {
	var out StargatePair
	if a.SolarSystemID == b.SolarSystemID {
		return out, fmt.Errorf("a gate cannot join a system to itself")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return out, fmt.Errorf("InsertStargatePair begin: %w", err)
	}
	defer tx.Rollback()

	sysA, okA, err := scanStargateSystemInfo(tx.QueryRowContext(ctx, stargateSystemInfoQuery, a.SolarSystemID))
	if err != nil {
		return out, err
	}
	sysB, okB, err := scanStargateSystemInfo(tx.QueryRowContext(ctx, stargateSystemInfoQuery, b.SolarSystemID))
	if err != nil {
		return out, err
	}
	if !okA || !okB {
		return out, fmt.Errorf("unknown solar system")
	}

	var joined int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM mapSolarSystemJumps
		 WHERE (fromSolarSystemID = ? AND toSolarSystemID = ?)
		    OR (fromSolarSystemID = ? AND toSolarSystemID = ?)`,
		sysA.SolarSystemID, sysB.SolarSystemID, sysB.SolarSystemID, sysA.SolarSystemID,
	).Scan(&joined); err != nil {
		return out, fmt.Errorf("InsertStargatePair pair check: %w", err)
	}
	if joined > 0 {
		return out, fmt.Errorf("%s and %s are already joined by a stargate", sysA.Name, sysB.Name)
	}

	radiusA, err := stargateTypeRadius(ctx, tx, a.TypeID)
	if err != nil {
		return out, err
	}
	radiusB, err := stargateTypeRadius(ctx, tx, b.TypeID)
	if err != nil {
		return out, err
	}

	// Locks mapDenormalize only: FOR UPDATE with a JOIN would lock the joined rows too.
	var gateA int64
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(itemID), ?) + 1
		  FROM mapDenormalize
		 WHERE itemID BETWEEN ? AND ?
		 FOR UPDATE`,
		CustomStargateIDFloor-1, CustomStargateIDFloor, CustomStargateIDCeil,
	).Scan(&gateA); err != nil {
		return out, fmt.Errorf("InsertStargatePair alloc: %w", err)
	}
	gateB := gateA + 1
	if gateB > CustomStargateIDCeil {
		return out, fmt.Errorf("InsertStargatePair: custom stargate ID band exhausted")
	}

	// Shipped gates carry a NULL orbitID and are named after the system they lead to.
	insertGate := func(id int64, end StargateEnd, sys, dest StargateSystemInfo, radius float64) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO mapDenormalize
				(itemID, typeID, groupID, solarSystemID, constellationID, regionID,
				 orbitID, x, y, z, radius, itemName, security)
			VALUES (?, ?, ?, ?, ?, ?, NULL, ?, ?, ?, ?, ?, ?)`,
			id, end.TypeID, stargateGroupID, sys.SolarSystemID, sys.ConstellationID, sys.RegionID,
			end.X, end.Y, end.Z, radius, "Stargate ("+dest.Name+")", sys.Security)
		if err != nil {
			return fmt.Errorf("InsertStargatePair mapDenormalize %d: %w", id, err)
		}
		return nil
	}
	if err := insertGate(gateA, a, sysA, sysB, radiusA); err != nil {
		return out, err
	}
	if err := insertGate(gateB, b, sysB, sysA, radiusB); err != nil {
		return out, err
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO mapJumps (stargateID, celestialID) VALUES (?, ?), (?, ?)`,
		gateA, gateB, gateB, gateA); err != nil {
		return out, fmt.Errorf("InsertStargatePair mapJumps: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO mapSolarSystemJumps
			(fromRegionID, fromConstellationID, fromSolarSystemID,
			 toSolarSystemID, toConstellationID, toRegionID)
		VALUES (?, ?, ?, ?, ?, ?), (?, ?, ?, ?, ?, ?)`,
		sysA.RegionID, sysA.ConstellationID, sysA.SolarSystemID,
		sysB.SolarSystemID, sysB.ConstellationID, sysB.RegionID,
		sysB.RegionID, sysB.ConstellationID, sysB.SolarSystemID,
		sysA.SolarSystemID, sysA.ConstellationID, sysA.RegionID,
	); err != nil {
		return out, fmt.Errorf("InsertStargatePair mapSolarSystemJumps: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO customStargates (gateID, pairGateID, solarSystemID, toSolarSystemID, anchorID)
		VALUES (?, ?, ?, ?, ?), (?, ?, ?, ?, ?)`,
		gateA, gateB, sysA.SolarSystemID, sysB.SolarSystemID, a.AnchorID,
		gateB, gateA, sysB.SolarSystemID, sysA.SolarSystemID, b.AnchorID,
	); err != nil {
		return out, fmt.Errorf("InsertStargatePair tracking: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return out, fmt.Errorf("InsertStargatePair commit: %w", err)
	}
	invalidateSharedCelestials(sysA.SolarSystemID)
	invalidateSharedCelestials(sysB.SolarSystemID)
	return StargatePair{GateA: gateA, GateB: gateB, SystemA: sysA.SolarSystemID, SystemB: sysB.SolarSystemID}, nil
}

// stargateTypeRadius returns the invTypes radius of a stargate type, refusing any other group.
func stargateTypeRadius(ctx context.Context, tx *sql.Tx, typeID int64) (float64, error) {
	var groupID int64
	var radius float64
	err := tx.QueryRowContext(ctx,
		`SELECT groupID, COALESCE(radius, 0) FROM invTypes WHERE typeID = ?`, typeID,
	).Scan(&groupID, &radius)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && groupID != stargateGroupID) {
		return 0, fmt.Errorf("type %d is not a stargate", typeID)
	}
	if err != nil {
		return 0, fmt.Errorf("stargateTypeRadius(%d): %w", typeID, err)
	}
	return radius, nil
}

// DeleteStargatePair removes a custom gate pair, given either gate, from every table
// InsertStargatePair wrote. It refuses a gate not tracked in customStargates, so shipped gates
// can never be deleted here. It returns the pair so the caller can drop the live balls.
func DeleteStargatePair(ctx context.Context, db *sql.DB, gateID int64) (StargatePair, error) {
	var out StargatePair
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return out, fmt.Errorf("DeleteStargatePair begin: %w", err)
	}
	defer tx.Rollback()

	err = tx.QueryRowContext(ctx, `
		SELECT gateID, pairGateID, solarSystemID, toSolarSystemID
		  FROM customStargates WHERE gateID = ? FOR UPDATE`, gateID,
	).Scan(&out.GateA, &out.GateB, &out.SystemA, &out.SystemB)
	if errors.Is(err, sql.ErrNoRows) {
		return out, fmt.Errorf("gate %d is not a custom stargate", gateID)
	}
	if err != nil {
		return out, fmt.Errorf("DeleteStargatePair lookup: %w", err)
	}

	steps := []struct {
		what  string
		query string
		args  []interface{}
	}{
		{"tracking", `DELETE FROM customStargates WHERE gateID IN (?, ?)`,
			[]interface{}{out.GateA, out.GateB}},
		{"mapJumps", `DELETE FROM mapJumps WHERE stargateID IN (?, ?)`,
			[]interface{}{out.GateA, out.GateB}},
		{"mapSolarSystemJumps", `
			DELETE FROM mapSolarSystemJumps
			 WHERE (fromSolarSystemID = ? AND toSolarSystemID = ?)
			    OR (fromSolarSystemID = ? AND toSolarSystemID = ?)`,
			[]interface{}{out.SystemA, out.SystemB, out.SystemB, out.SystemA}},
		{"mapDenormalize", `DELETE FROM mapDenormalize WHERE itemID IN (?, ?) AND groupID = 10`,
			[]interface{}{out.GateA, out.GateB}},
	}
	for _, s := range steps {
		if _, err := tx.ExecContext(ctx, s.query, s.args...); err != nil {
			return out, fmt.Errorf("DeleteStargatePair %s: %w", s.what, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return out, fmt.Errorf("DeleteStargatePair commit: %w", err)
	}
	invalidateSharedCelestials(out.SystemA)
	invalidateSharedCelestials(out.SystemB)
	return out, nil
}

// CustomStargateRow is one custom pair for the admin list, seen from its lower gateID.
type CustomStargateRow struct {
	GateA, GateB     int64
	SystemA, SystemB int64
	SystemAName      string
	SystemBName      string
	AnchorA, AnchorB int64
	AnchorAName      string
	AnchorBName      string
	TypeName         string
	CreatedAt        time.Time
}

// ListCustomStargates returns every custom pair once, newest first. solarSystemID 0 lists all
// pairs; otherwise only pairs with a gate in that system.
func ListCustomStargates(ctx context.Context, db *sql.DB, solarSystemID int64) ([]CustomStargateRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT a.gateID, a.pairGateID, a.solarSystemID, a.toSolarSystemID,
		       COALESCE(sa.solarSystemName, ''), COALESCE(sb.solarSystemName, ''),
		       a.anchorID, COALESCE(b.anchorID, 0),
		       COALESCE(na.itemName, ''), COALESCE(nb.itemName, ''),
		       COALESCE(t.typeName, ''), a.createdAt
		  FROM customStargates a
		  LEFT JOIN customStargates b ON b.gateID = a.pairGateID
		  LEFT JOIN mapSolarSystems sa ON sa.solarSystemID = a.solarSystemID
		  LEFT JOIN mapSolarSystems sb ON sb.solarSystemID = a.toSolarSystemID
		  LEFT JOIN mapDenormalize na ON na.itemID = a.anchorID
		  LEFT JOIN mapDenormalize nb ON nb.itemID = b.anchorID
		  LEFT JOIN mapDenormalize g ON g.itemID = a.gateID
		  LEFT JOIN invTypes t ON t.typeID = g.typeID
		 WHERE a.gateID < a.pairGateID
		   AND (? = 0 OR a.solarSystemID = ? OR a.toSolarSystemID = ?)
		 ORDER BY a.createdAt DESC, a.gateID DESC`,
		solarSystemID, solarSystemID, solarSystemID)
	if err != nil {
		return nil, fmt.Errorf("ListCustomStargates: %w", err)
	}
	defer rows.Close()
	var out []CustomStargateRow
	for rows.Next() {
		var r CustomStargateRow
		if err := rows.Scan(&r.GateA, &r.GateB, &r.SystemA, &r.SystemB,
			&r.SystemAName, &r.SystemBName, &r.AnchorA, &r.AnchorB,
			&r.AnchorAName, &r.AnchorBName, &r.TypeName, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("ListCustomStargates scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GateTypeForRegion returns the stargate type most used in a region, the default for a new
// gate there, or DefaultStargateTypeID when the region has no gates.
func GateTypeForRegion(ctx context.Context, db *sql.DB, regionID int64) (int64, error) {
	var typeID int64
	err := db.QueryRowContext(ctx, `
		SELECT typeID FROM mapDenormalize
		 WHERE regionID = ? AND groupID = 10
		 GROUP BY typeID
		 ORDER BY COUNT(*) DESC, typeID
		 LIMIT 1`, regionID).Scan(&typeID)
	if errors.Is(err, sql.ErrNoRows) {
		return DefaultStargateTypeID, nil
	}
	if err != nil {
		return 0, fmt.Errorf("GateTypeForRegion: %w", err)
	}
	return typeID, nil
}

// ListStargateTypes returns every stargate type (invTypes groupID 10) for the type dropdown.
func ListStargateTypes(ctx context.Context, db *sql.DB) ([]CelestialTypeOption, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT typeID, typeName, groupID FROM invTypes
		 WHERE groupID = 10 ORDER BY typeName`)
	if err != nil {
		return nil, fmt.Errorf("ListStargateTypes: %w", err)
	}
	defer rows.Close()
	var out []CelestialTypeOption
	for rows.Next() {
		var t CelestialTypeOption
		if err := rows.Scan(&t.TypeID, &t.TypeName, &t.GroupID); err != nil {
			return nil, fmt.Errorf("ListStargateTypes scan: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// StargatesPaired reports whether mapJumps sends fromGateID to toGateID, the check a
// StargateJump call passes before the ship leaves.
func StargatesPaired(ctx context.Context, db *sql.DB, fromGateID, toGateID int64) (bool, error) {
	var dest sql.NullInt64
	err := db.QueryRowContext(ctx,
		`SELECT celestialID FROM mapJumps WHERE stargateID = ?`, fromGateID).Scan(&dest)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("StargatesPaired: %w", err)
	}
	return dest.Valid && dest.Int64 == toGateID, nil
}
