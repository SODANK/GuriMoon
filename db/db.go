// Package db provides the MariaDB database layer for EVEmu.
package db

import (
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// ErrBadCredentials is returned by DoLogin when the account is not found or
// the password does not match.
var ErrBadCredentials = errors.New("invalid login or password")

// PoolOptions sizes the connection pool (config.json database.*).
type PoolOptions struct {
	MaxOpen     int
	MaxIdle     int
	MaxLifetime time.Duration
	MaxIdleTime time.Duration
}

// DefaultPool is the pool Open uses: see the comment in OpenPool.
var DefaultPool = PoolOptions{MaxOpen: 96, MaxIdle: 96, MaxLifetime: 30 * time.Minute, MaxIdleTime: 10 * time.Minute}

// Open connects to the MariaDB database using the given DSN and verifies the
// connection with a Ping.
func Open(dsn string) (*sql.DB, error) {
	return OpenPool(dsn, DefaultPool)
}

// OpenPool is Open with an explicit pool size.
func OpenPool(dsn string, pool PoolOptions) (*sql.DB, error) {
	sqldb, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("db open: %w", err)
	}
	// Idle equals open: a lower idle cap makes the pool close and re-dial connections the
	// load already opened. MaxLifetime stays below MySQL wait_timeout; MaxIdleTime reclaims a quiet pool.
	sqldb.SetMaxOpenConns(pool.MaxOpen)
	sqldb.SetMaxIdleConns(pool.MaxIdle)
	sqldb.SetConnMaxLifetime(pool.MaxLifetime)
	sqldb.SetConnMaxIdleTime(pool.MaxIdleTime)
	if err := sqldb.Ping(); err != nil {
		sqldb.Close()
		return nil, fmt.Errorf("db ping: %w", err)
	}
	return sqldb, nil
}

// DoLogin validates the password (plaintext or lowercase hex MD5) and returns accountID and
// role. A bad login or password returns ErrBadCredentials. With autoCreate a missing account
// is created with an MD5-hashed password and defaultRole.
func DoLogin(ctx context.Context, sqldb *sql.DB, login, password string, autoCreate bool, defaultRole int64) (accountID, role int64, err error) {
	if login == "" || password == "" {
		return 0, 0, ErrBadCredentials
	}

	var stored string
	row := sqldb.QueryRowContext(ctx,
		`SELECT accountid, role, password FROM account WHERE accountname = ?`,
		login,
	)
	if err = row.Scan(&accountID, &role, &stored); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			if autoCreate {
				id, createErr := CreateAccount(ctx, sqldb, login, password, defaultRole)
				if createErr != nil {
					return 0, 0, fmt.Errorf("auto-create account: %w", createErr)
				}
				return id, defaultRole, nil
			}
			return 0, 0, ErrBadCredentials
		}
		return 0, 0, fmt.Errorf("db query: %w", err)
	}

	// Accept plaintext match or md5(password) hex match.
	md5sum := md5.Sum([]byte(password))
	md5hex := hex.EncodeToString(md5sum[:])
	if stored != password && stored != md5hex {
		return 0, 0, ErrBadCredentials
	}

	return accountID, role, nil
}

// CreateAccount inserts a new account row with an MD5-hashed password and
// the given role.  It returns the new accountID.
func CreateAccount(ctx context.Context, sqldb *sql.DB, login, password string, role int64) (int64, error) {
	md5sum := md5.Sum([]byte(password))
	md5hex := hex.EncodeToString(md5sum[:])
	res, err := sqldb.ExecContext(ctx,
		`INSERT INTO account (accountname, password, role) VALUES (?, ?, ?)`,
		login, md5hex, role,
	)
	if err != nil {
		return 0, fmt.Errorf("db insert account: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("db account LastInsertId: %w", err)
	}
	return id, nil
}
