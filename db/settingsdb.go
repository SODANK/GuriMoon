package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gurimoon/safego"
)

// ---- Server settings ----
//
// server_settings is a key/value store for runtime state that survives restarts. A row
// wins; with no row the value comes from config.json's "settings" block
// (SetConfigSettingDefaults); with neither, the caller's own Go default applies.

// EnsureServerSettingsTable creates the server_settings table.
func EnsureServerSettingsTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS server_settings (
			name  VARCHAR(64) NOT NULL,
			value VARCHAR(255) NOT NULL,
			PRIMARY KEY (name)
		)`)
	if err != nil {
		return fmt.Errorf("EnsureServerSettingsTable: %w", err)
	}
	return nil
}

// GetServerSetting returns the value for name. ok is false when there is no row and
// config.json supplies no default, so the caller falls back to its own.
func GetServerSetting(ctx context.Context, db *sql.DB, name string) (value string, ok bool, err error) {
	err = db.QueryRowContext(ctx,
		`SELECT value FROM server_settings WHERE name = ?`, name).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		if v, ok := configSettingDefault(name); ok {
			return v, true, nil
		}
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("GetServerSetting %q: %w", name, err)
	}
	return value, true, nil
}

// SetServerSetting writes (or overwrites) a setting and patches the settings cache.
func SetServerSetting(ctx context.Context, db *sql.DB, name, value string) error {
	if _, err := db.ExecContext(ctx, `
		INSERT INTO server_settings (name, value) VALUES (?, ?)
		ON DUPLICATE KEY UPDATE value = VALUES(value)`, name, value); err != nil {
		return fmt.Errorf("SetServerSetting %q: %w", name, err)
	}
	settingsCachePut(name, value)
	return nil
}

// ---- config.json defaults ----

// settingStateKeys are server_settings rows written as bookkeeping (one-shot markers,
// cursors, timestamps), not dials. A config.json default for one would read as already
// done on a fresh database, so the settings block refuses them.
var settingStateKeys = map[string]bool{
	"agents.servicesSeeded":        true,
	"alliance.expelUnpaidSince":    true,
	"courier.journalRetagged":      true,
	"market.historyRolledThrough":  true,
	"market.tradeGoodBidBudgetAt":  true,
	"scc.bootstrapped":             true,
	"spawner.officerSecFixApplied": true,
	"starbase.fuelSweepAt":         true,
}

var configSettings atomic.Pointer[map[string]string]

// SetConfigSettingDefaults installs config.json's "settings" block: the value answered
// for any server_settings key with no row. Called once from main before any setting is
// read. Bookkeeping keys are dropped with a log line.
func SetConfigSettingDefaults(m map[string]string) {
	clean := make(map[string]string, len(m))
	for k, v := range m {
		if settingStateKeys[k] {
			log.Printf("config: settings.%s is server bookkeeping, not a dial — ignored", k)
			continue
		}
		clean[k] = v
	}
	configSettings.Store(&clean)
	if len(clean) > 0 {
		keys := make([]string, 0, len(clean))
		for k := range clean {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		log.Printf("config: %d setting default(s) from config.json: %s", len(keys), strings.Join(keys, ", "))
	}
}

func configSettingDefault(name string) (string, bool) {
	m := configSettings.Load()
	if m == nil {
		return "", false
	}
	v, ok := (*m)[name]
	return v, ok
}

// ---- Cached reads (hot paths) ----
//
// GetServerSetting is one query per call, too costly for a per-hit read. The cache holds
// every row, reloaded by StartSettingsCache and patched in place by SetServerSetting.
// Before the first load it answers config.json defaults, then the caller's.

var (
	settingsCache   atomic.Pointer[map[string]string]
	settingsCacheMu sync.Mutex // serialises copy-on-write updates
)

// StartSettingsCache loads server_settings into memory and re-reads it every interval
// until ctx ends.
func StartSettingsCache(ctx context.Context, db *sql.DB, interval time.Duration) {
	reloadSettingsCache(ctx, db)
	go func() {
		defer safego.Recover("StartSettingsCache")
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				reloadSettingsCache(ctx, db)
			}
		}
	}()
}

func reloadSettingsCache(ctx context.Context, db *sql.DB) {
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := db.QueryContext(qctx, `SELECT name, value FROM server_settings`)
	if err != nil {
		log.Printf("settings cache: %v", err)
		return
	}
	defer rows.Close()
	m := make(map[string]string, 256)
	for rows.Next() {
		var k, v string
		if rows.Scan(&k, &v) == nil {
			m[k] = v
		}
	}
	if rows.Err() != nil {
		return
	}
	settingsCacheMu.Lock()
	settingsCache.Store(&m)
	settingsCacheMu.Unlock()
}

func settingsCachePut(name, value string) {
	settingsCacheMu.Lock()
	defer settingsCacheMu.Unlock()
	old := settingsCache.Load()
	if old == nil {
		return // not started; the first load will see the row
	}
	m := make(map[string]string, len(*old)+1)
	for k, v := range *old {
		m[k] = v
	}
	m[name] = value
	settingsCache.Store(&m)
}

// CachedSetting answers name from the cache: table row, else config.json default.
// ok is false when neither has it.
func CachedSetting(name string) (string, bool) {
	if m := settingsCache.Load(); m != nil {
		if v, ok := (*m)[name]; ok {
			return v, true
		}
	}
	return configSettingDefault(name)
}

// CachedSettingFloat is CachedSetting parsed as a float and clamped to [min, max];
// def when absent or unparseable.
func CachedSettingFloat(name string, def, min, max float64) float64 {
	v, ok := CachedSetting(name)
	if !ok {
		return def
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		return def
	}
	if f < min {
		f = min
	}
	if f > max {
		f = max
	}
	return f
}

// CachedSettingDuration reads a number of units (e.g. seconds) and returns it as a
// duration, clamped like CachedSettingFloat.
func CachedSettingDuration(name string, unit time.Duration, def, min, max float64) time.Duration {
	return time.Duration(CachedSettingFloat(name, def, min, max) * float64(unit))
}

// CachedSettingFloats reads a comma-separated list of exactly len(def) numbers, each
// clamped to [min, max]. Anything else answers def.
func CachedSettingFloats(name string, def []float64, min, max float64) []float64 {
	v, ok := CachedSetting(name)
	if !ok {
		return def
	}
	parts := strings.Split(v, ",")
	if len(parts) != len(def) {
		return def
	}
	out := make([]float64, len(parts))
	for i, p := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil {
			return def
		}
		if f < min {
			f = min
		}
		if f > max {
			f = max
		}
		out[i] = f
	}
	return out
}
