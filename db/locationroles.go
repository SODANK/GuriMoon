package db

import (
	"context"
	"database/sql"
	"strings"
	"sync/atomic"
	"time"
)

// Location roles: roles that hold only at a place. A member row carries rolesAtAll, rolesAtHQ,
// rolesAtBase and rolesAtOther beside corprole (chrCorporationRoles); the client folds them in
// sessions.py. With server_settings corp.locationRoles = 1 the corporation-job gate and the
// office hangar gates fold them the same way for the station the action happens at. Off (the
// default) they read corprole alone.

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

// RolesAtStation folds a member's location roles for one station. Pure:
// stationID 0 (no place) or on = false answers corprole alone.
func RolesAtStation(info *CorpMemberInfo, stationID, hqID int64, on bool) uint64 {
	if info == nil {
		return 0
	}
	roles := info.Corprole
	if !on || stationID == 0 {
		return roles
	}
	roles |= info.RolesAtAll
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
	return RolesAtStation(info, stationID, hq, on)
}
