package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// Station management: the lobby's STATION MGMT. window for a player-owned station (an outpost or
// a conquered station). Access rules and cost modifiers apply while station.enforceManagement is
// on (default). See docs/station-management.md.

// StationDetails is the Station Details tab.
type StationDetails struct {
	StationID                int64
	SolarSystemID            int64
	OwnerCorpID              int64
	Name                     string
	Description              string
	Security                 float64
	DockingCostPerVolume     float64
	OfficeRentalCost         int64
	MaxShipVolumeDockable    int64
	ReprocessingStationsTake float64
	ReprocessingHangarFlag   int64
}

// ServiceAccessRule is one service's Access Control block.
type ServiceAccessRule struct {
	ServiceID           int64
	MinimumStanding     float64
	MinimumCharSecurity float64
	MaximumCharSecurity float64
	MinimumCorpSecurity float64
	MaximumCorpSecurity float64
}

// DefaultServiceAccessRule is a rule nobody wrote: open to everyone.
func DefaultServiceAccessRule(serviceID int64) ServiceAccessRule {
	return ServiceAccessRule{ServiceID: serviceID, MinimumStanding: -10,
		MinimumCharSecurity: -10, MaximumCharSecurity: 10,
		MinimumCorpSecurity: -10, MaximumCorpSecurity: 10}
}

// ServiceCostModifier is one service's Cost Modifiers block.
type ServiceCostModifier struct {
	ServiceID                    int64
	DiscountPerGoodStandingPoint float64
	SurchargePerBadStandingPoint float64
}

// StationService is one staServices row.
type StationService struct {
	ServiceID int64
	Name      string
}

// ── which stations ───────────────────────────────────────────────────────────

// StationManageableBy reports whether corpID may run stationID from this window: it owns the
// station and is a player corporation. reason names the refusal for the pilot.
func StationManageableBy(ctx context.Context, db *sql.DB, stationID, corpID int64) (ok bool, reason string) {
	owner := StationOwnerCorp(ctx, db, stationID)
	if owner == 0 || owner != corpID {
		return false, "Your corporation does not own this station."
	}
	if npc, _ := IsNPCCorp(ctx, db, owner); npc {
		return false, "NPC stations cannot be managed."
	}
	return true, ""
}

// ── reads ────────────────────────────────────────────────────────────────────

// GetStationDetails reads the Station Details tab.
func GetStationDetails(ctx context.Context, db *sql.DB, stationID int64) (StationDetails, error) {
	d := StationDetails{StationID: stationID}
	var sec sql.NullFloat64
	err := db.QueryRowContext(ctx, `
		SELECT s.solarSystemID, s.corporationID, s.stationName, COALESCE(op.description, ''),
		       ss.security, s.dockingCostPerVolume, s.officeRentalCost, s.maxShipVolumeDockable,
		       s.reprocessingStationsTake, s.reprocessingHangarFlag
		  FROM staStations s
		  LEFT JOIN staOperations op ON op.operationID = s.operationID
		  LEFT JOIN mapSolarSystems ss ON ss.solarSystemID = s.solarSystemID
		 WHERE s.stationID = ?`, stationID).Scan(
		&d.SolarSystemID, &d.OwnerCorpID, &d.Name, &d.Description, &sec,
		&d.DockingCostPerVolume, &d.OfficeRentalCost, &d.MaxShipVolumeDockable,
		&d.ReprocessingStationsTake, &d.ReprocessingHangarFlag)
	if err != nil {
		return d, fmt.Errorf("GetStationDetails %d: %w", stationID, err)
	}
	d.Security = sec.Float64
	return d, nil
}

// ListStationServices returns every staServices row. The client filters it
// by the station's own service mask.
func ListStationServices(ctx context.Context, db *sql.DB) ([]StationService, error) {
	rows, err := db.QueryContext(ctx, `SELECT serviceID, serviceName FROM staServices ORDER BY serviceID`)
	if err != nil {
		return nil, fmt.Errorf("ListStationServices: %w", err)
	}
	defer rows.Close()
	var out []StationService
	for rows.Next() {
		var s StationService
		if err := rows.Scan(&s.ServiceID, &s.Name); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

var (
	stationMgmtOnce sync.Once
	stationMgmtErr  error
)

func ensureStationMgmtTables(ctx context.Context, db *sql.DB) error {
	stationMgmtOnce.Do(func() {
		if _, err := db.ExecContext(ctx, `
			CREATE TABLE IF NOT EXISTS stationServiceAccessRules (
			  stationID           INT UNSIGNED NOT NULL,
			  serviceID           INT UNSIGNED NOT NULL,
			  minimumStanding     DOUBLE NOT NULL DEFAULT -10,
			  minimumCharSecurity DOUBLE NOT NULL DEFAULT -10,
			  maximumCharSecurity DOUBLE NOT NULL DEFAULT 10,
			  minimumCorpSecurity DOUBLE NOT NULL DEFAULT -10,
			  maximumCorpSecurity DOUBLE NOT NULL DEFAULT 10,
			  PRIMARY KEY (stationID, serviceID)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`); err != nil {
			stationMgmtErr = fmt.Errorf("stationServiceAccessRules: %w", err)
			return
		}
		if _, err := db.ExecContext(ctx, `
			CREATE TABLE IF NOT EXISTS stationServiceCostModifiers (
			  stationID                    INT UNSIGNED NOT NULL,
			  serviceID                    INT UNSIGNED NOT NULL,
			  discountPerGoodStandingPoint DOUBLE NOT NULL DEFAULT 0,
			  surchargePerBadStandingPoint DOUBLE NOT NULL DEFAULT 0,
			  PRIMARY KEY (stationID, serviceID)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`); err != nil {
			stationMgmtErr = fmt.Errorf("stationServiceCostModifiers: %w", err)
			return
		}
		// Offices tab "Publicly Available": no row = public. A private slot is never
		// handed to a renting corporation (freeOfficeSlotFlag).
		if _, err := db.ExecContext(ctx, `
			CREATE TABLE IF NOT EXISTS stationOfficeSlots (
			  stationID         INT UNSIGNED NOT NULL,
			  slotNumber        INT UNSIGNED NOT NULL,
			  publiclyAvailable TINYINT NOT NULL DEFAULT 1,
			  PRIMARY KEY (stationID, slotNumber)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`); err != nil {
			stationMgmtErr = fmt.Errorf("stationOfficeSlots: %w", err)
		}
	})
	return stationMgmtErr
}

// OfficeSlotsPublic returns each office slot's Publicly Available flag at
// stationID, keyed by slot number; a slot with no row is public.
func OfficeSlotsPublic(ctx context.Context, db *sql.DB, stationID int64) (map[int64]bool, error) {
	out := map[int64]bool{}
	if err := ensureStationMgmtTables(ctx, db); err != nil {
		return out, err
	}
	rows, err := db.QueryContext(ctx,
		`SELECT slotNumber, publiclyAvailable FROM stationOfficeSlots WHERE stationID = ?`, stationID)
	if err != nil {
		return out, fmt.Errorf("OfficeSlotsPublic: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var n, pub int64
		if err := rows.Scan(&n, &pub); err != nil {
			return out, err
		}
		out[n] = pub != 0
	}
	return out, rows.Err()
}

// GetServiceAccessRule returns a stored rule or the open default.
func GetServiceAccessRule(ctx context.Context, db *sql.DB, stationID, serviceID int64) (ServiceAccessRule, error) {
	r := DefaultServiceAccessRule(serviceID)
	if err := ensureStationMgmtTables(ctx, db); err != nil {
		return r, err
	}
	err := db.QueryRowContext(ctx, `
		SELECT minimumStanding, minimumCharSecurity, maximumCharSecurity,
		       minimumCorpSecurity, maximumCorpSecurity
		  FROM stationServiceAccessRules WHERE stationID = ? AND serviceID = ?`,
		stationID, serviceID).Scan(&r.MinimumStanding, &r.MinimumCharSecurity,
		&r.MaximumCharSecurity, &r.MinimumCorpSecurity, &r.MaximumCorpSecurity)
	if errors.Is(err, sql.ErrNoRows) {
		return DefaultServiceAccessRule(serviceID), nil
	}
	if err != nil {
		return DefaultServiceAccessRule(serviceID), fmt.Errorf("GetServiceAccessRule: %w", err)
	}
	return r, nil
}

// ListServiceCostModifiers returns one row per service (stored or 0/0); the client filters by
// the station's mask.
func ListServiceCostModifiers(ctx context.Context, db *sql.DB, stationID int64) ([]ServiceCostModifier, error) {
	if err := ensureStationMgmtTables(ctx, db); err != nil {
		return nil, err
	}
	svcs, err := ListStationServices(ctx, db)
	if err != nil {
		return nil, err
	}
	stored := map[int64]ServiceCostModifier{}
	rows, err := db.QueryContext(ctx, `
		SELECT serviceID, discountPerGoodStandingPoint, surchargePerBadStandingPoint
		  FROM stationServiceCostModifiers WHERE stationID = ?`, stationID)
	if err != nil {
		return nil, fmt.Errorf("ListServiceCostModifiers: %w", err)
	}
	for rows.Next() {
		var m ServiceCostModifier
		if err := rows.Scan(&m.ServiceID, &m.DiscountPerGoodStandingPoint, &m.SurchargePerBadStandingPoint); err != nil {
			rows.Close()
			return nil, err
		}
		stored[m.ServiceID] = m
	}
	rows.Close()
	out := make([]ServiceCostModifier, 0, len(svcs))
	for _, s := range svcs {
		if m, ok := stored[s.ServiceID]; ok {
			out = append(out, m)
		} else {
			out = append(out, ServiceCostModifier{ServiceID: s.ServiceID})
		}
	}
	return out, nil
}

// StationOffice is one rented office, for the Offices tab.
type StationOffice struct {
	OfficeID  int64
	CorpID    int64
	Flag      int64 // slot flag 70-85 (OfficeSlotFlag); 0 when the entity row is missing
	LastPaid  int64 // Win32 FILETIME of the latest rental bill (0 = none on record)
	PeriodISK float64
}

// ListStationOffices returns the offices rented at stationID with the date and amount of each
// one's latest rental bill (the Offices tab's start date and balance due).
func ListStationOffices(ctx context.Context, db *sql.DB, stationID int64) ([]StationOffice, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT o.itemID, o.corporationID,
		       COALESCE((SELECT e.flag FROM entity e WHERE e.itemID = o.itemID), 0),
		       COALESCE((SELECT MAX(b.issued) FROM bills b
		                  WHERE b.debtorID = o.corporationID AND b.externalID2 = o.stationID
		                    AND b.billTypeID = ?), 0),
		       COALESCE((SELECT b.amount FROM bills b
		                  WHERE b.debtorID = o.corporationID AND b.externalID2 = o.stationID
		                    AND b.billTypeID = ? ORDER BY b.issued DESC LIMIT 1), 0)
		  FROM crpoffices o
		 WHERE o.stationID = ?
		 ORDER BY o.itemID`, BillTypeRentalBill, BillTypeRentalBill, stationID)
	if err != nil {
		return nil, fmt.Errorf("ListStationOffices: %w", err)
	}
	defer rows.Close()
	var out []StationOffice
	for rows.Next() {
		var o StationOffice
		if err := rows.Scan(&o.OfficeID, &o.CorpID, &o.Flag, &o.LastPaid, &o.PeriodISK); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ── writes ───────────────────────────────────────────────────────────────────

// StationManagementUpdate is one OK/Apply of the window. Rules and Modifiers
// carry only what the client changed.
type StationManagementUpdate struct {
	Name                     string
	Description              string
	DockingCostPerVolume     float64
	OfficeRentalCost         int64
	ReprocessingStationsTake float64
	ReprocessingHangarFlag   int64
	Rules                    []ServiceAccessRule
	Modifiers                []ServiceCostModifier
	OfficeSlots              []OfficeSlotPublic
}

// OfficeSlotPublic is one Offices tab checkbox: slot number and Publicly Available.
type OfficeSlotPublic struct {
	Number int64
	Public bool
}

// SaveStationManagement applies an update. Returns whether the name or the
// description changed, so the caller knows which caches to drop.
func SaveStationManagement(ctx context.Context, db *sql.DB, stationID int64, u StationManagementUpdate) (renamed, redescribed bool, err error) {
	if err := ensureStationMgmtTables(ctx, db); err != nil {
		return false, false, err
	}
	cur, err := GetStationDetails(ctx, db, stationID)
	if err != nil {
		return false, false, err
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, false, fmt.Errorf("SaveStationManagement begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	name := clampStationName85(u.Name)
	if name == "" {
		name = cur.Name
	}
	take := clampFloat(u.ReprocessingStationsTake, 0, 1)
	docking := clampFloat(u.DockingCostPerVolume, 0, 1000)
	office := u.OfficeRentalCost
	if office < 0 {
		office = 0
	}
	flag := u.ReprocessingHangarFlag
	if flag == 0 {
		flag = cur.ReprocessingHangarFlag
	}
	// Clamped: STRICT_TRANS_TABLES makes an overlong name a rolled-back transaction, and the
	// client's own 100-character field exceeds the column.
	if _, err := tx.ExecContext(ctx, `
		UPDATE staStations SET stationName = ?, dockingCostPerVolume = ?, officeRentalCost = ?,
		       reprocessingStationsTake = ?, reprocessingHangarFlag = ?
		 WHERE stationID = ?`, name, docking, office, take, flag, stationID); err != nil {
		return false, false, fmt.Errorf("SaveStationManagement station: %w", err)
	}
	if name != cur.Name {
		// The ball's name and cfg.evelocations come from mapDenormalize; a station is two rows that
		// must not drift. See docs/db-6-notes.md#outposts.
		if _, err := tx.ExecContext(ctx,
			`UPDATE mapDenormalize SET itemName = ? WHERE itemID = ?`, name, stationID); err != nil {
			return false, false, fmt.Errorf("SaveStationManagement mapDenormalize: %w", err)
		}
		renamed = true
	}
	if u.Description != cur.Description {
		if err := setStationOwnDescriptionTx(ctx, tx, stationID, u.Description); err != nil {
			return false, false, err
		}
		redescribed = true
	}
	for _, r := range u.Rules {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO stationServiceAccessRules (stationID, serviceID, minimumStanding,
			       minimumCharSecurity, maximumCharSecurity, minimumCorpSecurity, maximumCorpSecurity)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON DUPLICATE KEY UPDATE minimumStanding = VALUES(minimumStanding),
			       minimumCharSecurity = VALUES(minimumCharSecurity), maximumCharSecurity = VALUES(maximumCharSecurity),
			       minimumCorpSecurity = VALUES(minimumCorpSecurity), maximumCorpSecurity = VALUES(maximumCorpSecurity)`,
			stationID, r.ServiceID, clampFloat(r.MinimumStanding, -10, 10),
			clampFloat(r.MinimumCharSecurity, -10, 10), clampFloat(r.MaximumCharSecurity, -10, 10),
			clampFloat(r.MinimumCorpSecurity, -10, 10), clampFloat(r.MaximumCorpSecurity, -10, 10)); err != nil {
			return false, false, fmt.Errorf("SaveStationManagement rule %d: %w", r.ServiceID, err)
		}
	}
	for _, m := range u.Modifiers {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO stationServiceCostModifiers (stationID, serviceID,
			       discountPerGoodStandingPoint, surchargePerBadStandingPoint)
			VALUES (?, ?, ?, ?)
			ON DUPLICATE KEY UPDATE discountPerGoodStandingPoint = VALUES(discountPerGoodStandingPoint),
			       surchargePerBadStandingPoint = VALUES(surchargePerBadStandingPoint)`,
			stationID, m.ServiceID, clampFloat(m.DiscountPerGoodStandingPoint, 0, 10),
			clampFloat(m.SurchargePerBadStandingPoint, 0, 10)); err != nil {
			return false, false, fmt.Errorf("SaveStationManagement cost %d: %w", m.ServiceID, err)
		}
	}
	capacity := stationOfficeCapacity(ctx, tx, stationID)
	for _, s := range u.OfficeSlots {
		if s.Number < 1 || s.Number > capacity {
			continue
		}
		pub := 0
		if s.Public {
			pub = 1
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO stationOfficeSlots (stationID, slotNumber, publiclyAvailable) VALUES (?, ?, ?)
			ON DUPLICATE KEY UPDATE publiclyAvailable = VALUES(publiclyAvailable)`,
			stationID, s.Number, pub); err != nil {
			return false, false, fmt.Errorf("SaveStationManagement office slot %d: %w", s.Number, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, false, fmt.Errorf("SaveStationManagement commit: %w", err)
	}
	return renamed, redescribed, nil
}

// setStationOwnDescriptionTx writes a description for this station alone. The description lives
// on the station's operation, which other stations share, so a shared operation is cloned into
// the custom band first (services copied), as ESPSetStationServices does.
// See docs/station-management.md#storage.
func setStationOwnDescriptionTx(ctx context.Context, tx *sql.Tx, stationID int64, desc string) error {
	var (
		curOp    sql.NullInt64
		opName   sql.NullString
		sharedBy int64
	)
	if err := tx.QueryRowContext(ctx, `
		SELECT s.operationID, op.operationName,
		       (SELECT COUNT(*) FROM staStations s2 WHERE s2.operationID = s.operationID)
		  FROM staStations s
		  LEFT JOIN staOperations op ON op.operationID = s.operationID
		 WHERE s.stationID = ?`, stationID).Scan(&curOp, &opName, &sharedBy); err != nil {
		return fmt.Errorf("station description lookup: %w", err)
	}
	target := curOp.Int64
	if !(curOp.Valid && curOp.Int64 >= CustomOperationBase && sharedBy == 1) {
		next, err := cloneOperation(ctx, tx, curOp, opName)
		if err != nil {
			return err
		}
		if curOp.Valid {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO staOperationServices (operationID, serviceID)
				SELECT ?, serviceID FROM staOperationServices WHERE operationID = ?`,
				next, curOp.Int64); err != nil {
				return fmt.Errorf("station description copy services: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE staStations SET operationID = ? WHERE stationID = ?`, next, stationID); err != nil {
			return fmt.Errorf("station description assign: %w", err)
		}
		if curOp.Valid {
			if err := deleteOrphanCustomOperationTx(ctx, tx, curOp.Int64); err != nil {
				return err
			}
		}
		target = next
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE staOperations SET description = ? WHERE operationID = ?`, desc, target); err != nil {
		return fmt.Errorf("station description write: %w", err)
	}
	return nil
}

// ── enforcement ──────────────────────────────────────────────────────────────

// Station service bits (staServices.serviceID, const.stationService*) that a
// player owner's access rules or cost modifiers act on.
const (
	StationServiceReprocessing      int64 = 16
	StationServiceCloning           int64 = 512
	StationServiceRepair            int64 = 4096
	StationServiceDocking           int64 = 2097152
	StationServiceOfficeRental      int64 = 4194304
	StationServiceJumpCloneFacility int64 = 8388608
)

// StationManagementEnforced is server_settings `station.enforceManagement`
// (default on). Off, a player owner's access rules and cost modifiers are
// stored but nothing refuses or reprices.
func StationManagementEnforced(ctx context.Context, db *sql.DB) bool {
	v, ok := CachedSetting("station.enforceManagement")
	if !ok {
		return true
	}
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil {
		return true
	}
	return b
}

// StationPatron is a pilot at a station as the station's rules see them.
// Managed is false when nothing the owner set applies: enforcement off, an
// NPC owner, or the pilot is in the owner corporation.
type StationPatron struct {
	OwnerCorpID int64
	CorpID      int64
	AllianceID  int64
	Managed     bool
	Standing    float64 // owner's standing toward the pilot; set when Managed
}

// ResolveStationPatron reads the owner and the pilot's corporation for
// stationID and decides whether the owner's rules apply to charID.
func ResolveStationPatron(ctx context.Context, db *sql.DB, stationID, charID int64) StationPatron {
	if db == nil || stationID == 0 || charID == 0 {
		return StationPatron{}
	}
	_, corpID, allianceID, _ := GetCharSovContext(ctx, db, charID)
	return resolvePatron(ctx, db, stationID, charID, corpID, allianceID)
}

// ResolveStationCorpPatron is ResolveStationPatron for a corporation as the
// customer (office rent): standing toward the corp, then its alliance.
func ResolveStationCorpPatron(ctx context.Context, db *sql.DB, stationID, corpID int64) StationPatron {
	if db == nil || stationID == 0 || corpID == 0 {
		return StationPatron{}
	}
	allianceID, _ := GetCorpAllianceID(ctx, db, corpID)
	return resolvePatron(ctx, db, stationID, 0, corpID, allianceID)
}

func resolvePatron(ctx context.Context, db *sql.DB, stationID, charID, corpID, allianceID int64) StationPatron {
	p := StationPatron{CorpID: corpID, AllianceID: allianceID}
	p.OwnerCorpID = StationOwnerCorp(ctx, db, stationID)
	if p.OwnerCorpID == 0 || p.CorpID == p.OwnerCorpID || !StationManagementEnforced(ctx, db) {
		return p
	}
	if npc, _ := IsNPCCorp(ctx, db, p.OwnerCorpID); npc {
		return p
	}
	p.Managed = true
	p.Standing = OwnerStandingToward(ctx, db, p.OwnerCorpID, charID, p.CorpID, p.AllianceID)
	return p
}

// StationServiceRefusal applies the owner's access rule for serviceID: "" to
// allow, else the refusal text. Open at NPC stations, to the owner's own
// corporation and with enforcement off.
func StationServiceRefusal(ctx context.Context, db *sql.DB, stationID, charID, serviceID int64) string {
	p := ResolveStationPatron(ctx, db, stationID, charID)
	if !p.Managed {
		return ""
	}
	rule, err := GetServiceAccessRule(ctx, db, stationID, serviceID)
	if err != nil {
		return ""
	}
	var sec float64
	_ = db.QueryRowContext(ctx, `SELECT securityRating FROM character_ WHERE characterID = ?`, charID).Scan(&sec)
	if ServiceAccessAllowed(rule, p.Standing, sec) {
		return ""
	}
	return "The owners of this station do not permit you to use this service."
}

// GetServiceCostModifier returns one service's stored modifier, or 0/0.
func GetServiceCostModifier(ctx context.Context, db *sql.DB, stationID, serviceID int64) ServiceCostModifier {
	m := ServiceCostModifier{ServiceID: serviceID}
	if err := ensureStationMgmtTables(ctx, db); err != nil {
		return m
	}
	_ = db.QueryRowContext(ctx, `
		SELECT discountPerGoodStandingPoint, surchargePerBadStandingPoint
		  FROM stationServiceCostModifiers WHERE stationID = ? AND serviceID = ?`,
		stationID, serviceID).Scan(&m.DiscountPerGoodStandingPoint, &m.SurchargePerBadStandingPoint)
	return m
}

// StationServiceFee is base after the owner's cost modifier for serviceID,
// keyed on the owner's standing toward charID. Unchanged at NPC stations, for
// the owner's own corporation and with enforcement off.
func StationServiceFee(ctx context.Context, db *sql.DB, stationID, charID, serviceID int64, base float64) float64 {
	return patronFee(ctx, db, ResolveStationPatron(ctx, db, stationID, charID), stationID, serviceID, base)
}

// StationServiceFeeForCorp is StationServiceFee with a corporation as the
// customer.
func StationServiceFeeForCorp(ctx context.Context, db *sql.DB, stationID, corpID, serviceID int64, base float64) float64 {
	return patronFee(ctx, db, ResolveStationCorpPatron(ctx, db, stationID, corpID), stationID, serviceID, base)
}

func patronFee(ctx context.Context, db *sql.DB, p StationPatron, stationID, serviceID int64, base float64) float64 {
	if !p.Managed || base <= 0 {
		return base
	}
	m := GetServiceCostModifier(ctx, db, stationID, serviceID)
	return AdjustCostForStanding(base, p.Standing, m.DiscountPerGoodStandingPoint, m.SurchargePerBadStandingPoint)
}

// StationServiceFeeFactor is StationServiceFee as a multiplier (1 = no
// change), for fees priced per unit.
func StationServiceFeeFactor(ctx context.Context, db *sql.DB, stationID, charID, serviceID int64) float64 {
	return StationServiceFee(ctx, db, stationID, charID, serviceID, 1)
}

// AdjustCostForStanding applies a discount or surcharge, both in percent per
// standing point: positive standing discounts, negative surcharges. Never
// negative. Shared by assembly lines and station cost modifiers.
func AdjustCostForStanding(cost, standing, discountPerGoodPoint, surchargePerBadPoint float64) float64 {
	if standing > 0 {
		cost *= 1.0 - discountPerGoodPoint*standing/100.0
	} else if standing < 0 {
		cost *= 1.0 + surchargePerBadPoint*(-standing)/100.0
	}
	if cost < 0 {
		cost = 0
	}
	return cost
}

// ServiceAccessAllowed is the access rule as one pure test: standing is the owner corporation's
// standing toward the pilot, charSec the pilot's security status. Corporation security is stored
// but not enforced. See docs/station-management.md#enforcement.
func ServiceAccessAllowed(r ServiceAccessRule, standing, charSec float64) bool {
	if standing < r.MinimumStanding {
		return false
	}
	return charSec >= r.MinimumCharSecurity && charSec <= r.MaximumCharSecurity
}

// OwnerStandingToward is the owner corporation's declared standing toward a
// pilot from crpStandings, most specific first: the pilot, their corporation,
// their alliance. 0 when none is declared.
func OwnerStandingToward(ctx context.Context, db *sql.DB, ownerCorpID, charID, corpID, allianceID int64) float64 {
	for _, to := range []int64{charID, corpID, allianceID} {
		if to == 0 {
			continue
		}
		var s float64
		if err := db.QueryRowContext(ctx,
			`SELECT standing FROM crpStandings WHERE corporationID = ? AND toID = ?`,
			ownerCorpID, to).Scan(&s); err == nil {
			return s
		}
	}
	return 0
}

func clampFloat(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// clampStationName85 fits staStations.stationName VARCHAR(85) without
// splitting a multi-byte character.
func clampStationName85(s string) string {
	r := []rune(s)
	if len(r) > 85 {
		r = r[:85]
	}
	return string(r)
}
