package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"sync"

	"gurimoon/corp"
)

// Starbase access policy: what the tower window's Access, Force Field, Defense and Claim tabs
// set, and the predicates that read it. Every setting persists and round-trips, but the places
// that would refuse a pilot check server_settings starbase.enforceAccess (default off) first.
// Settings live in their own two tables, and a tower with no row reads DefaultStarbasePolicy.
// See docs/db-10-notes.md#starbase-access-policy.

// Access levels, the client's combo values (moonmining.py: Role 0, Corporation 1, Alliance 2).
// Each widens the last.
const (
	StarbaseAccessRole        = 0 // owner corp AND Config Starbase Equipment (or Director/CEO)
	StarbaseAccessCorporation = 1 // any member of the owner corp
	StarbaseAccessAlliance    = 2 // any member of the owner alliance
)

// Deploy actions, the keys the client's deployFlags dict is indexed with.
var StarbaseDeployActions = []string{"anchor", "unanchor", "online", "offline"}

// Structure actions. The client's three columns are viewput, viewputtake and use; "view" and
// "put" share the first and "take" is the second.
const (
	StructureActionView = "view"
	StructureActionPut  = "put"
	StructureActionTake = "take"
	StructureActionUse  = "use"
)

// SentrySettings is the Defense tab. A nil threshold is the client's unticked checkbox.
type SentrySettings struct {
	Standing     *float64
	Status       *float64
	StandingDrop bool
	StatusDrop   bool
	Aggression   bool
	War          bool
}

// StarbasePolicy is one tower's settings.
type StarbasePolicy struct {
	TowerID         int64
	FFAllowCorp     bool
	FFAllowAlliance bool
	Claim           bool
	Deploy          map[string]int // action to access level
	Sentry          SentrySettings
}

// StructureAccess is one slave structure's row on the Access tab.
type StructureAccess struct {
	StructureID int64
	TowerID     int64
	ViewPut     int
	ViewPutTake int
	Use         int
}

// StarbaseActor is the pilot a predicate is asked about.
type StarbaseActor struct {
	CharID     int64
	CorpID     int64
	AllianceID int64
	Roles      uint64
	CEO        bool
}

// DefaultStarbasePolicy is what a tower with no stored policy means. It reproduces
// StarbaseManagePermitted: deploy access at the alliance level for an alliance tower and the
// corp otherwise, force field open to the corp, sov claimed.
func DefaultStarbasePolicy(rec StarbaseRecord) StarbasePolicy {
	lvl := StarbaseAccessCorporation
	if rec.OwnerAllianceID != 0 {
		lvl = StarbaseAccessAlliance
	}
	p := StarbasePolicy{
		TowerID:     rec.EntityID,
		FFAllowCorp: true,
		Claim:       true,
		Deploy:      make(map[string]int, len(StarbaseDeployActions)),
	}
	for _, a := range StarbaseDeployActions {
		p.Deploy[a] = lvl
	}
	return p
}

// DefaultStructureAccess is a structure with no stored row: corp-wide on all
// three columns.
func DefaultStructureAccess(structureID, towerID int64) StructureAccess {
	return StructureAccess{StructureID: structureID, TowerID: towerID,
		ViewPut: StarbaseAccessCorporation, ViewPutTake: StarbaseAccessCorporation, Use: StarbaseAccessCorporation}
}

// ── predicates (pure) ────────────────────────────────────────────────────────

// hasStarbaseRole: Config Starbase Equipment, or Director, or the CEO.
func (a StarbaseActor) hasStarbaseRole() bool {
	return a.CEO || corp.HasRole(a.Roles, corp.RoleDirector) || corp.HasRole(a.Roles, corp.RoleStarbaseConfig)
}

// AccessLevelAllows reports whether a satisfies level for a structure owned
// by ownerCorp / ownerAlliance. An unknown level reads as the narrowest.
func AccessLevelAllows(level int, a StarbaseActor, ownerCorp, ownerAlliance int64) bool {
	sameCorp := a.CorpID != 0 && a.CorpID == ownerCorp
	switch level {
	case StarbaseAccessAlliance:
		if sameCorp {
			return true
		}
		return ownerAlliance != 0 && a.AllianceID == ownerAlliance
	case StarbaseAccessCorporation:
		return sameCorp
	default:
		return sameCorp && a.hasStarbaseRole()
	}
}

// CanDeploy reports whether a may anchor / unanchor / online / offline
// structures on the tower.
func CanDeploy(p StarbasePolicy, tower StarbaseRecord, a StarbaseActor, action string) bool {
	lvl, ok := p.Deploy[action]
	if !ok {
		lvl = StarbaseAccessRole
	}
	return AccessLevelAllows(lvl, a, tower.OwnerCorpID, tower.OwnerAllianceID)
}

// CanUseStructure reports whether a may perform action on a slave structure
// whose tower is owned as tower records.
func CanUseStructure(acc StructureAccess, tower StarbaseRecord, a StarbaseActor, action string) bool {
	var lvl int
	switch action {
	case StructureActionView, StructureActionPut:
		lvl = acc.ViewPut
	case StructureActionTake:
		lvl = acc.ViewPutTake
	case StructureActionUse:
		lvl = acc.Use
	default:
		return false
	}
	return AccessLevelAllows(lvl, a, tower.OwnerCorpID, tower.OwnerAllianceID)
}

// ForcefieldAdmits reports whether a may pass the tower's force field without the password.
func ForcefieldAdmits(p StarbasePolicy, tower StarbaseRecord, a StarbaseActor) bool {
	if p.FFAllowCorp && a.CorpID != 0 && a.CorpID == tower.OwnerCorpID {
		return true
	}
	return p.FFAllowAlliance && tower.OwnerAllianceID != 0 && a.AllianceID == tower.OwnerAllianceID
}

// SentryTarget is what a tower gun knows about a ship in range.
type SentryTarget struct {
	SameCorp         bool    // pilot is in the tower's corp
	SameAlliance     bool    // pilot is in the tower's alliance
	Standing         float64 // owner's standing toward the pilot, -10..10
	SecStatus        float64
	StandingDropping bool // owner's standing toward them fell recently
	SecDropping      bool // their security status fell recently
	Aggressed        bool // they attacked this starbase or its owners
	AtWar            bool // WarShotIsLegal between owner and pilot
}

// SentryShouldEngage is the Defense tab as one rule. The owner's own corp and alliance are
// never engaged, whatever the thresholds say.
func SentryShouldEngage(s SentrySettings, t SentryTarget) bool {
	if t.SameCorp || t.SameAlliance {
		return false
	}
	switch {
	case s.War && t.AtWar,
		s.Aggression && t.Aggressed,
		s.Standing != nil && t.Standing < *s.Standing,
		s.Status != nil && t.SecStatus < *s.Status,
		s.StandingDrop && t.StandingDropping,
		s.StatusDrop && t.SecDropping:
		return true
	}
	return false
}

// ── storage ──────────────────────────────────────────────────────────────────

var (
	starbasePolicyOnce sync.Once
	starbasePolicyErr  error
)

func ensureStarbasePolicyTables(ctx context.Context, db *sql.DB) error {
	starbasePolicyOnce.Do(func() {
		if _, err := db.ExecContext(ctx, `
			CREATE TABLE IF NOT EXISTS starbasePolicy (
			  towerID            INT UNSIGNED NOT NULL,
			  ffAllowCorp        TINYINT(1)   NOT NULL DEFAULT 1,
			  ffAllowAlliance    TINYINT(1)   NOT NULL DEFAULT 0,
			  claim              TINYINT(1)   NOT NULL DEFAULT 1,
			  deployAnchor       TINYINT      NOT NULL DEFAULT 1,
			  deployUnanchor     TINYINT      NOT NULL DEFAULT 1,
			  deployOnline       TINYINT      NOT NULL DEFAULT 1,
			  deployOffline      TINYINT      NOT NULL DEFAULT 1,
			  sentryStanding     DOUBLE       NULL,
			  sentryStatus       DOUBLE       NULL,
			  sentryStandingDrop TINYINT(1)   NOT NULL DEFAULT 0,
			  sentryStatusDrop   TINYINT(1)   NOT NULL DEFAULT 0,
			  sentryAggression   TINYINT(1)   NOT NULL DEFAULT 0,
			  sentryWar          TINYINT(1)   NOT NULL DEFAULT 0,
			  PRIMARY KEY (towerID)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`); err != nil {
			starbasePolicyErr = fmt.Errorf("starbasePolicy: %w", err)
			return
		}
		if _, err := db.ExecContext(ctx, `
			CREATE TABLE IF NOT EXISTS starbaseStructureAccess (
			  structureID  INT UNSIGNED NOT NULL,
			  towerID      INT UNSIGNED NOT NULL,
			  viewput      TINYINT      NOT NULL DEFAULT 1,
			  viewputtake  TINYINT      NOT NULL DEFAULT 1,
			  useAccess    TINYINT      NOT NULL DEFAULT 1,
			  PRIMARY KEY (structureID),
			  KEY idx_tower (towerID)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`); err != nil {
			starbasePolicyErr = fmt.Errorf("starbaseStructureAccess: %w", err)
		}
	})
	return starbasePolicyErr
}

// GetStarbasePolicy returns tower's stored policy, or its default.
func GetStarbasePolicy(ctx context.Context, db *sql.DB, tower StarbaseRecord) (StarbasePolicy, error) {
	p := DefaultStarbasePolicy(tower)
	if err := ensureStarbasePolicyTables(ctx, db); err != nil {
		return p, err
	}
	var anchor, unanchor, online, offline int
	var standing, status sql.NullFloat64
	err := db.QueryRowContext(ctx, `
		SELECT ffAllowCorp, ffAllowAlliance, claim,
		       deployAnchor, deployUnanchor, deployOnline, deployOffline,
		       sentryStanding, sentryStatus, sentryStandingDrop, sentryStatusDrop,
		       sentryAggression, sentryWar
		  FROM starbasePolicy WHERE towerID = ?`, tower.EntityID).Scan(
		&p.FFAllowCorp, &p.FFAllowAlliance, &p.Claim,
		&anchor, &unanchor, &online, &offline,
		&standing, &status, &p.Sentry.StandingDrop, &p.Sentry.StatusDrop,
		&p.Sentry.Aggression, &p.Sentry.War)
	if errors.Is(err, sql.ErrNoRows) {
		return p, nil
	}
	if err != nil {
		return DefaultStarbasePolicy(tower), fmt.Errorf("GetStarbasePolicy: %w", err)
	}
	p.Deploy = map[string]int{"anchor": anchor, "unanchor": unanchor, "online": online, "offline": offline}
	if standing.Valid {
		v := standing.Float64
		p.Sentry.Standing = &v
	}
	if status.Valid {
		v := status.Float64
		p.Sentry.Status = &v
	}
	return p, nil
}

// SaveStarbasePolicy writes the whole policy row.
func SaveStarbasePolicy(ctx context.Context, db *sql.DB, p StarbasePolicy) error {
	if err := ensureStarbasePolicyTables(ctx, db); err != nil {
		return err
	}
	lvl := func(action string) int { return clampAccessLevel(p.Deploy[action]) }
	nullable := func(f *float64) any {
		if f == nil {
			return nil
		}
		return *f
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO starbasePolicy (towerID, ffAllowCorp, ffAllowAlliance, claim,
		       deployAnchor, deployUnanchor, deployOnline, deployOffline,
		       sentryStanding, sentryStatus, sentryStandingDrop, sentryStatusDrop,
		       sentryAggression, sentryWar)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
		       ffAllowCorp = VALUES(ffAllowCorp), ffAllowAlliance = VALUES(ffAllowAlliance),
		       claim = VALUES(claim),
		       deployAnchor = VALUES(deployAnchor), deployUnanchor = VALUES(deployUnanchor),
		       deployOnline = VALUES(deployOnline), deployOffline = VALUES(deployOffline),
		       sentryStanding = VALUES(sentryStanding), sentryStatus = VALUES(sentryStatus),
		       sentryStandingDrop = VALUES(sentryStandingDrop), sentryStatusDrop = VALUES(sentryStatusDrop),
		       sentryAggression = VALUES(sentryAggression), sentryWar = VALUES(sentryWar)`,
		p.TowerID, p.FFAllowCorp, p.FFAllowAlliance, p.Claim,
		lvl("anchor"), lvl("unanchor"), lvl("online"), lvl("offline"),
		nullable(p.Sentry.Standing), nullable(p.Sentry.Status),
		p.Sentry.StandingDrop, p.Sentry.StatusDrop, p.Sentry.Aggression, p.Sentry.War)
	if err != nil {
		return fmt.Errorf("SaveStarbasePolicy: %w", err)
	}
	return nil
}

// ListStructureAccess returns the Access tab's rows: every slave structure on
// the tower, stored flags or the default.
func ListStructureAccess(ctx context.Context, db *sql.DB, towerID int64) ([]StructureAccess, error) {
	if err := ensureStarbasePolicyTables(ctx, db); err != nil {
		return nil, err
	}
	structs, err := ListStarbaseStructuresForTower(ctx, db, towerID)
	if err != nil {
		return nil, err
	}
	stored := map[int64]StructureAccess{}
	rows, err := db.QueryContext(ctx, `
		SELECT structureID, viewput, viewputtake, useAccess
		  FROM starbaseStructureAccess WHERE towerID = ?`, towerID)
	if err != nil {
		return nil, fmt.Errorf("ListStructureAccess: %w", err)
	}
	for rows.Next() {
		a := StructureAccess{TowerID: towerID}
		if err := rows.Scan(&a.StructureID, &a.ViewPut, &a.ViewPutTake, &a.Use); err != nil {
			rows.Close()
			return nil, fmt.Errorf("ListStructureAccess scan: %w", err)
		}
		stored[a.StructureID] = a
	}
	rows.Close()
	out := make([]StructureAccess, 0, len(structs))
	for _, s := range structs {
		if a, ok := stored[s.EntityID]; ok {
			out = append(out, a)
		} else {
			out = append(out, DefaultStructureAccess(s.EntityID, towerID))
		}
	}
	return out, nil
}

// GetStructureAccess returns one structure's row (default when unset).
func GetStructureAccess(ctx context.Context, db *sql.DB, structureID, towerID int64) (StructureAccess, error) {
	a := DefaultStructureAccess(structureID, towerID)
	if err := ensureStarbasePolicyTables(ctx, db); err != nil {
		return a, err
	}
	err := db.QueryRowContext(ctx, `
		SELECT viewput, viewputtake, useAccess FROM starbaseStructureAccess WHERE structureID = ?`,
		structureID).Scan(&a.ViewPut, &a.ViewPutTake, &a.Use)
	if errors.Is(err, sql.ErrNoRows) {
		return DefaultStructureAccess(structureID, towerID), nil
	}
	if err != nil {
		return DefaultStructureAccess(structureID, towerID), fmt.Errorf("GetStructureAccess: %w", err)
	}
	return a, nil
}

// SaveStructureAccess upserts rows for structures that belong to towerID. A row naming a
// structure on another tower is skipped, since the structureIDs come from the client.
func SaveStructureAccess(ctx context.Context, db *sql.DB, towerID int64, rows []StructureAccess) (int, error) {
	if err := ensureStarbasePolicyTables(ctx, db); err != nil {
		return 0, err
	}
	structs, err := ListStarbaseStructuresForTower(ctx, db, towerID)
	if err != nil {
		return 0, err
	}
	onTower := make(map[int64]bool, len(structs))
	for _, s := range structs {
		onTower[s.EntityID] = true
	}
	n := 0
	for _, a := range rows {
		if !onTower[a.StructureID] {
			continue
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO starbaseStructureAccess (structureID, towerID, viewput, viewputtake, useAccess)
			VALUES (?, ?, ?, ?, ?)
			ON DUPLICATE KEY UPDATE towerID = VALUES(towerID), viewput = VALUES(viewput),
			       viewputtake = VALUES(viewputtake), useAccess = VALUES(useAccess)`,
			a.StructureID, towerID, clampAccessLevel(a.ViewPut), clampAccessLevel(a.ViewPutTake),
			clampAccessLevel(a.Use)); err != nil {
			return n, fmt.Errorf("SaveStructureAccess: %w", err)
		}
		n++
	}
	return n, nil
}

// DeleteStarbasePolicy drops a tower's policy and every structure row under it, called when the
// tower goes.
func DeleteStarbasePolicy(ctx context.Context, db *sql.DB, towerID int64) error {
	if err := ensureStarbasePolicyTables(ctx, db); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM starbaseStructureAccess WHERE towerID = ?`, towerID); err != nil {
		return fmt.Errorf("DeleteStarbasePolicy structures: %w", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM starbasePolicy WHERE towerID = ?`, towerID); err != nil {
		return fmt.Errorf("DeleteStarbasePolicy: %w", err)
	}
	return nil
}

// DeleteStructureAccess drops one structure's row (structure unanchored).
func DeleteStructureAccess(ctx context.Context, db *sql.DB, structureID int64) error {
	if err := ensureStarbasePolicyTables(ctx, db); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, `DELETE FROM starbaseStructureAccess WHERE structureID = ?`, structureID)
	return err
}

// LoadStarbaseActor resolves charID's corp, alliance, roles and CEO seat.
func LoadStarbaseActor(ctx context.Context, db *sql.DB, charID int64) (StarbaseActor, error) {
	a := StarbaseActor{CharID: charID}
	_, corpID, allianceID, err := GetCharSovContext(ctx, db, charID)
	if err != nil {
		return a, err
	}
	a.CorpID, a.AllianceID = corpID, allianceID
	if info, err := GetCorporationMemberInfo(ctx, db, charID); err == nil && info != nil {
		a.Roles = info.Corprole
	}
	var ceo sql.NullInt64
	if err := db.QueryRowContext(ctx,
		`SELECT ceoID FROM corporation WHERE corporationID = ?`, corpID).Scan(&ceo); err == nil {
		a.CEO = ceo.Valid && ceo.Int64 == charID
	}
	return a, nil
}

// StarbaseAccessEnforced is server_settings starbase.enforceAccess (default off). Every call
// site that would refuse a pilot on this policy asks it first.
func StarbaseAccessEnforced(ctx context.Context, db *sql.DB) bool {
	v, ok, err := GetServerSetting(ctx, db, "starbase.enforceAccess")
	if err != nil || !ok {
		return false
	}
	b, _ := strconv.ParseBool(v)
	return b
}

func clampAccessLevel(l int) int {
	if l < StarbaseAccessRole || l > StarbaseAccessAlliance {
		return StarbaseAccessRole
	}
	return l
}
