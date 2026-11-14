package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Secure containers (group 340), Audit Log Secure Containers (448) and the
// plain cargo containers (12) a pilot assembles in a hangar. Passwords and
// the ALSC configuration mask live in containerPasswords; every ALSC action
// is a containerAuditLog row the client's log viewer pages through.

const (
	GroupCargoContainer       = 12
	GroupSecureCargoContainer = 340
	GroupAuditLogContainer    = 448

	// SetPassword / RetrievePassword "which" (eveConstants SCCPasswordType*).
	ContainerPasswordGeneral = 1
	ContainerPasswordConfig  = 2

	// ALSC configuration bits (eveConstants ALSCPasswordNeededTo*).
	ALSCNeedPasswordToOpen    = 1
	ALSCNeedPasswordToLock    = 2
	ALSCNeedPasswordToUnlock  = 4
	ALSCNeedPasswordToViewLog = 8

	// Audit log actions (eveConstants ALSCAction*).
	ALSCActionAssemble      = 1
	ALSCActionRepackage     = 2
	ALSCActionSetName       = 3
	ALSCActionMove          = 4
	ALSCActionSetPassword   = 5
	ALSCActionAdd           = 6
	ALSCActionLock          = 7
	ALSCActionUnlock        = 8
	ALSCActionEnterPassword = 9
	ALSCActionConfigure     = 10

	// Item flags inside an ALSC (eveflags 63/64).
	FlagLocked   = 63
	FlagUnlocked = 64
)

var ErrContainerPasswordMismatch = errors.New("the old password does not match")

// IsContainerGroup reports whether a group is one of the assemblable
// hangar containers.
func IsContainerGroup(groupID int64) bool {
	return groupID == GroupCargoContainer || groupID == GroupSecureCargoContainer || groupID == GroupAuditLogContainer
}

// IsPasswordContainerGroup reports whether the group carries passwords.
func IsPasswordContainerGroup(groupID int64) bool {
	return groupID == GroupSecureCargoContainer || groupID == GroupAuditLogContainer
}

func EnsureContainerTables(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS containerPasswords (
			itemID     BIGINT NOT NULL,
			generalPw  VARCHAR(64) NOT NULL DEFAULT '',
			configPw   VARCHAR(64) NOT NULL DEFAULT '',
			alscConfig INT NOT NULL DEFAULT 0,
			PRIMARY KEY (itemID)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`); err != nil {
		return fmt.Errorf("EnsureContainerTables passwords: %w", err)
	}
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS containerAuditLog (
			logID       BIGINT NOT NULL AUTO_INCREMENT,
			itemID      BIGINT NOT NULL,
			logDateTime BIGINT NOT NULL,
			actorID     BIGINT NOT NULL,
			actionID    INT NOT NULL,
			arg1        BIGINT NOT NULL DEFAULT 0,
			arg2        BIGINT NOT NULL DEFAULT 0,
			flag        INT NOT NULL DEFAULT 0,
			locationID  BIGINT NOT NULL DEFAULT 0,
			inventoryMgrLocationID BIGINT NOT NULL DEFAULT 0,
			status      TINYINT NOT NULL DEFAULT 1,
			PRIMARY KEY (logID),
			KEY idx_calog_item (itemID, logID)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`); err != nil {
		return fmt.Errorf("EnsureContainerTables log: %w", err)
	}
	return nil
}

// ContainerSecurity is a container's stored secrets and configuration.
type ContainerSecurity struct {
	GeneralPw, ConfigPw string
	Config              int64
}

func GetContainerSecurity(ctx context.Context, db *sql.DB, itemID int64) (ContainerSecurity, error) {
	var s ContainerSecurity
	err := db.QueryRowContext(ctx,
		`SELECT generalPw, configPw, alscConfig FROM containerPasswords WHERE itemID = ?`, itemID,
	).Scan(&s.GeneralPw, &s.ConfigPw, &s.Config)
	if errors.Is(err, sql.ErrNoRows) {
		return s, nil
	}
	if err != nil {
		return s, fmt.Errorf("GetContainerSecurity: %w", err)
	}
	return s, nil
}

// SetContainerPassword writes one password; a container that already has
// one in that slot needs the old one to match.
func SetContainerPassword(ctx context.Context, db *sql.DB, itemID, which int64, oldPw, newPw string) error {
	s, err := GetContainerSecurity(ctx, db, itemID)
	if err != nil {
		return err
	}
	current, col := s.GeneralPw, "generalPw"
	if which == ContainerPasswordConfig {
		current, col = s.ConfigPw, "configPw"
	} else if which != ContainerPasswordGeneral {
		return fmt.Errorf("unknown password type %d", which)
	}
	if current != "" && current != oldPw {
		return ErrContainerPasswordMismatch
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO containerPasswords (itemID, `+col+`) VALUES (?, ?)
		ON DUPLICATE KEY UPDATE `+col+` = VALUES(`+col+`)`, itemID, newPw)
	if err != nil {
		return fmt.Errorf("SetContainerPassword: %w", err)
	}
	return nil
}

func SetALSCConfig(ctx context.Context, db *sql.DB, itemID, cfg int64) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO containerPasswords (itemID, alscConfig) VALUES (?, ?)
		ON DUPLICATE KEY UPDATE alscConfig = VALUES(alscConfig)`, itemID, cfg)
	if err != nil {
		return fmt.Errorf("SetALSCConfig: %w", err)
	}
	return nil
}

// DeleteContainerSecurity drops a destroyed container's secrets and log.
func DeleteContainerSecurity(ctx context.Context, db *sql.DB, itemID int64) {
	_, _ = db.ExecContext(ctx, `DELETE FROM containerPasswords WHERE itemID = ?`, itemID)
	_, _ = db.ExecContext(ctx, `DELETE FROM containerAuditLog WHERE itemID = ?`, itemID)
}

// ContainerLogEntry is one audit-log row, in the shape the client's
// AuditLogSecureContainerLogViewer attribute-reads.
type ContainerLogEntry struct {
	LogID                  int64
	ItemID                 int64
	LogDateTime            int64 // unix seconds
	ActorID                int64
	ActionID               int64
	Arg1, Arg2             int64
	Flag                   int64
	LocationID             int64
	InventoryMgrLocationID int64
	Status                 int64 // 1 success, 0 fail
}

func AppendContainerLog(ctx context.Context, db *sql.DB, e ContainerLogEntry) error {
	if e.LogDateTime == 0 {
		e.LogDateTime = time.Now().Unix()
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO containerAuditLog
		  (itemID, logDateTime, actorID, actionID, arg1, arg2, flag, locationID, inventoryMgrLocationID, status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ItemID, e.LogDateTime, e.ActorID, e.ActionID, e.Arg1, e.Arg2, e.Flag, e.LocationID, e.InventoryMgrLocationID, e.Status)
	if err != nil {
		return fmt.Errorf("AppendContainerLog: %w", err)
	}
	return nil
}

// ListContainerLog pages the log the way the client browses it: forward
// from fromLogID (rows after it, oldest first), or backward (rows before it,
// the 50 nearest, returned oldest first). fromDate, when non-zero, bounds the
// page on the same side. limit is the client's page (50).
func ListContainerLog(ctx context.Context, db *sql.DB, itemID int64, fromDate int64, fromLogID int64, forward bool, limit int) ([]ContainerLogEntry, error) {
	if limit <= 0 {
		limit = 50
	}
	where := `WHERE itemID = ?`
	args := []any{itemID}
	order := `ASC`
	if forward {
		if fromLogID > 0 {
			where += ` AND logID > ?`
			args = append(args, fromLogID)
		}
		if fromDate > 0 {
			where += ` AND logDateTime >= ?`
			args = append(args, fromDate)
		}
	} else {
		if fromLogID > 0 {
			where += ` AND logID < ?`
			args = append(args, fromLogID)
		}
		if fromDate > 0 {
			where += ` AND logDateTime <= ?`
			args = append(args, fromDate)
		}
		order = `DESC`
	}
	args = append(args, limit)
	rows, err := db.QueryContext(ctx, `
		SELECT logID, itemID, logDateTime, actorID, actionID, arg1, arg2, flag, locationID, inventoryMgrLocationID, status
		  FROM containerAuditLog `+where+` ORDER BY logID `+order+` LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("ListContainerLog: %w", err)
	}
	defer rows.Close()
	var out []ContainerLogEntry
	for rows.Next() {
		var e ContainerLogEntry
		if err := rows.Scan(&e.LogID, &e.ItemID, &e.LogDateTime, &e.ActorID, &e.ActionID, &e.Arg1, &e.Arg2,
			&e.Flag, &e.LocationID, &e.InventoryMgrLocationID, &e.Status); err != nil {
			return nil, fmt.Errorf("ListContainerLog scan: %w", err)
		}
		out = append(out, e)
	}
	if !forward {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out, rows.Err()
}

// GetContainerUsedVolume sums the volume of everything inside a container.
func GetContainerUsedVolume(ctx context.Context, db *sql.DB, containerID int64) (float64, error) {
	var used sql.NullFloat64
	err := db.QueryRowContext(ctx, `
		SELECT SUM(t.volume * e.quantity) FROM entity e JOIN invTypes t ON t.typeID = e.typeID
		 WHERE e.locationID = ?`, containerID).Scan(&used)
	if err != nil {
		return 0, fmt.Errorf("GetContainerUsedVolume: %w", err)
	}
	return used.Float64, nil
}

// ContainerTypeQuantities totals what a container holds, by typeID.
func ContainerTypeQuantities(ctx context.Context, db *sql.DB, containerID int64) (map[int64]int64, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT typeID, SUM(quantity) FROM entity WHERE locationID = ? GROUP BY typeID`, containerID)
	if err != nil {
		return nil, fmt.Errorf("ContainerTypeQuantities: %w", err)
	}
	defer rows.Close()
	out := map[int64]int64{}
	for rows.Next() {
		var typeID, qty int64
		if err := rows.Scan(&typeID, &qty); err != nil {
			return nil, fmt.Errorf("ContainerTypeQuantities scan: %w", err)
		}
		out[typeID] = qty
	}
	return out, rows.Err()
}

// SetItemFlag changes one item's flag in place (lock / unlock inside an ALSC).
func SetItemFlag(ctx context.Context, db *sql.DB, itemID, flag int64) error {
	if _, err := db.ExecContext(ctx, `UPDATE entity SET flag = ? WHERE itemID = ?`, flag, itemID); err != nil {
		return fmt.Errorf("SetItemFlag: %w", err)
	}
	return nil
}

// AssembleContainer turns a packaged container (singleton 0) into a real one. A stack of
// several is refused; the client only offers Assemble on a single packaged item.
func AssembleContainer(ctx context.Context, db *sql.DB, itemID int64) error {
	res, err := db.ExecContext(ctx,
		`UPDATE entity SET singleton = 1 WHERE itemID = ? AND singleton = 0 AND quantity = 1`, itemID)
	if err != nil {
		return fmt.Errorf("AssembleContainer: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("that container is already assembled, or is a stack")
	}
	return nil
}
