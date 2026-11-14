package db

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Client role bits (common/script/sys/service.py). The role is not kept per connection
// (DoLogin reads it once for the session change), so a GM-only gate looks it up by account.
const (
	RoleLogin      = 1
	RolePlayer     = 2
	RoleGML        = 8
	RoleGMH        = 16
	RoleAdmin      = 32
	RolePetitionee = 256
	RoleLegioneer  = 262144
)

// Role masks for the two staff surfaces. A role passes when it carries any bit of the mask,
// never by numeric comparison: the auto-account role (1073741826) is larger than every GM bit and
// holds none. DefaultSlashRoles is the client's ROLE_SLASH plus GMH and ADMIN; DefaultStaffRoles
// is the GM trio.
const (
	DefaultStaffRoles = RoleGML | RoleGMH | RoleAdmin
	DefaultSlashRoles = RoleGML | RoleGMH | RoleAdmin | RoleLegioneer
)

var staffRoles, slashRoles atomic.Int64

func init() {
	staffRoles.Store(DefaultStaffRoles)
	slashRoles.Store(DefaultSlashRoles)
}

// SetRoleMasks installs the configured masks (esp.staffRoles,
// authentication.slashRoles); 0 keeps the default.
func SetRoleMasks(staff, slash int64) {
	if staff != 0 {
		staffRoles.Store(staff)
	}
	if slash != 0 {
		slashRoles.Store(slash)
	}
}

// HasAnyRole reports whether role carries at least one bit of mask.
func HasAnyRole(role, mask int64) bool { return role&mask != 0 }

// IsStaffRole reports whether a role may use the ESP admin panel.
func IsStaffRole(role int64) bool { return HasAnyRole(role, staffRoles.Load()) }

// IsSlashRole reports whether a role may run slash commands.
func IsSlashRole(role int64) bool { return HasAnyRole(role, slashRoles.Load()) }

var (
	accountRoleMu    sync.Mutex
	accountRoleCache = map[int64]accountRoleEntry{}
)

type accountRoleEntry struct {
	role  int64
	until time.Time
}

// AccountRoleCached is GetAccountRole behind a one-minute cache, for gates
// on hot paths (every slash command, every chat line starting with "/").
// A lookup failure answers 0, which passes no gate.
func AccountRoleCached(ctx context.Context, db *sql.DB, accountID int64) int64 {
	if accountID <= 0 {
		return 0
	}
	now := time.Now()
	accountRoleMu.Lock()
	if e, ok := accountRoleCache[accountID]; ok && now.Before(e.until) {
		accountRoleMu.Unlock()
		return e.role
	}
	accountRoleMu.Unlock()
	role, err := GetAccountRole(ctx, db, accountID)
	if err != nil {
		return 0
	}
	accountRoleMu.Lock()
	if len(accountRoleCache) > 4096 {
		for id, e := range accountRoleCache {
			if now.After(e.until) {
				delete(accountRoleCache, id)
			}
		}
	}
	accountRoleCache[accountID] = accountRoleEntry{role, now.Add(time.Minute)}
	accountRoleMu.Unlock()
	return role
}

// MaySlash reports whether the account may run slash commands. The client
// only hides the commands from other roles; this is the gate.
func MaySlash(ctx context.Context, db *sql.DB, accountID int64) bool {
	return IsSlashRole(AccountRoleCached(ctx, db, accountID))
}

// GetAccountRole reads the role mask of an account.
func GetAccountRole(ctx context.Context, db *sql.DB, accountID int64) (int64, error) {
	var role int64
	err := db.QueryRowContext(ctx, `SELECT role FROM account WHERE accountid = ?`, accountID).Scan(&role)
	if err != nil {
		return 0, fmt.Errorf("GetAccountRole: %w", err)
	}
	return role, nil
}

// IsPetitionee reports whether a role may work the petition desk, the same test as the
// client's petition window GM tabs.
func IsPetitionee(role int64) bool {
	return role&RolePetitionee == RolePetitionee
}
