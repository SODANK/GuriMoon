package db

import (
	"context"
	"database/sql"
	"strings"
	"sync/atomic"
	"time"

	"gurimoon/corp"
)

// Location roles: roles that hold only at a place. A member row carries rolesAtAll, rolesAtHQ,
// rolesAtBase and rolesAtOther beside corprole (chrCorporationRoles); the client folds them in
// sessions.py. The client's role window writes hangar and container roles only into the three
// location columns, so every gate reads them. With server_settings corp.locationRoles = 1 the
// corporation-job gate and the office hangar gates fold them for the station the action happens
// at (a place with no station, such as a starbase array, counts as Other). Off (the default) a
// location role holds everywhere.

// SettingCorpLocationRoles is the server_settings switch.
const SettingCorpLocationRoles = "corp.locationRoles"

var (
	locationRolesOn atomic.Bool
	locationRolesAt atomic.Int64 // unix seconds of the last read
)

// LocationRolesOn reports the switch, re-read at most every 30 seconds.
func LocationRolesOn(ctx context.Context, db *sql.DB) bool {
	now := time.Now().Unix()
	if now-locationRolesAt.Load() >= 30 {
		locationRolesAt.Store(now)
		if v, ok, err := GetServerSetting(ctx, db, SettingCorpLocationRoles); err == nil {
			locationRolesOn.Store(ok && strings.TrimSpace(v) == "1")
		}
	}
	return locationRolesOn.Load()
}

// RolesAtStation folds a member's location roles for one station. Pure. With on = false every
// location column counts everywhere; stationID 0 (no station) counts as Other.
func RolesAtStation(info *CorpMemberInfo, stationID, hqID int64, on bool) uint64 {
	if info == nil {
		return 0
	}
	roles := info.Corprole | info.RolesAtAll
	if !on {
		return roles | info.RolesAtHQ | info.RolesAtBase | info.RolesAtOther
	}
	if stationID == 0 {
		return roles | info.RolesAtOther
	}
	atBase := info.BaseID != 0 && stationID == info.BaseID
	atHQ := hqID != 0 && stationID == hqID
	if atBase {
		roles |= info.RolesAtBase
	}
	if atHQ {
		roles |= info.RolesAtHQ
	}
	if !atBase && !atHQ {
		roles |= info.RolesAtOther
	}
	return roles
}

// MemberRolesAt is a member's roles at a station of its corporation.
func MemberRolesAt(ctx context.Context, db *sql.DB, charID, corpID, stationID int64) uint64 {
	info, err := GetCorporationMemberInfo(ctx, db, charID)
	if err != nil || info == nil {
		return 0
	}
	on := LocationRolesOn(ctx, db)
	var hq int64
	if on && stationID != 0 {
		_ = db.QueryRowContext(ctx,
			`SELECT COALESCE(stationID, 0) FROM corporation WHERE corporationID = ?`, corpID).Scan(&hq)
	}
	roles := RolesAtStation(info, stationID, hq, on)
	// A Director holds every hangar and container role, as in the client; the module's filter
	// still has its say over the expansion.
	if roles&corp.RoleDirector != 0 {
		roles |= corp.LocationalMask()
		roles = FilterCorpRoles(ctx, db, charID, corpID, roles)
	}
	return roles
}
