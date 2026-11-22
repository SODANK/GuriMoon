package db

import (
	"context"
	"database/sql"
	"fmt"
)

// NPC EWAR ban rules: security bands where NPC rats may not use a given kind of
// electronic warfare. Edited on the ESP's NPC Spawner Rules page; the NPC manager
// holds a copy and reloads it on every edit. Law enforcement (CONCORD, empire
// police and navy) ignores them.

// EwarBanKind is one NPC EWAR kind a rule can ban, with its admin-page label.
type EwarBanKind struct {
	Kind  string
	Label string
}

// EwarBanKinds lists every kind GetNPCEwarProfile produces, in page order.
var EwarBanKinds = []EwarBanKind{
	{"scram", "Warp disruptor / scrambler"},
	{"web", "Stasis webifier"},
	{"jam", "ECM jammer"},
	{"sensorDamp", "Sensor dampener"},
	{"trackingDisrupt", "Tracking disruptor"},
	{"targetPaint", "Target painter"},
	{"capDrain", "Energy neutralizer"},
}

// IsEwarBanKind reports whether kind is one of EwarBanKinds.
func IsEwarBanKind(kind string) bool {
	for _, k := range EwarBanKinds {
		if k.Kind == kind {
			return true
		}
	}
	return false
}

// EwarBanRule is one row of npcEwarBanRules: NPCs may not use Kind in systems
// whose displayed security (rounded to 0.1) lies in [SecMin, SecMax].
type EwarBanRule struct {
	RuleID      int64
	RuleName    string
	Kind        string
	SecMin      float64
	SecMax      float64
	Description string
}

// ensureEwarBanRules creates npcEwarBanRules and seeds the default rule (no warp
// disruption in 0.9 and 1.0) only when the table is new, so an admin who deletes
// the default keeps it deleted across restarts.
func ensureEwarBanRules(ctx context.Context, db *sql.DB) error {
	var exists int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.TABLES
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'npcEwarBanRules'`).Scan(&exists); err != nil {
		return fmt.Errorf("ensureEwarBanRules: %w", err)
	}
	if exists > 0 {
		return nil
	}
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE npcEwarBanRules (
			ruleID      INT PRIMARY KEY AUTO_INCREMENT,
			ruleName    VARCHAR(128) NOT NULL,
			ewarKind    VARCHAR(32) NOT NULL,
			secMin      DOUBLE NOT NULL DEFAULT 0.9,
			secMax      DOUBLE NOT NULL DEFAULT 1.0,
			description VARCHAR(255) DEFAULT NULL
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`); err != nil {
		return fmt.Errorf("ensureEwarBanRules create: %w", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO npcEwarBanRules (ruleName, ewarKind, secMin, secMax, description) VALUES (?, ?, ?, ?, ?)`,
		"No warp disruption", "scram", 0.9, 1.0,
		"Rats never warp disrupt or scramble in 0.9 and 1.0 space"); err != nil {
		return fmt.Errorf("ensureEwarBanRules seed: %w", err)
	}
	return nil
}

// GetAllEwarBanRules returns every EWAR ban rule, safest band first.
func GetAllEwarBanRules(ctx context.Context, db *sql.DB) ([]EwarBanRule, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT ruleID, ruleName, ewarKind, secMin, secMax, COALESCE(description, '')
		FROM npcEwarBanRules ORDER BY secMin DESC, ruleID`)
	if err != nil {
		return nil, fmt.Errorf("GetAllEwarBanRules: %w", err)
	}
	defer rows.Close()
	var out []EwarBanRule
	for rows.Next() {
		var r EwarBanRule
		if err := rows.Scan(&r.RuleID, &r.RuleName, &r.Kind, &r.SecMin, &r.SecMax, &r.Description); err != nil {
			return nil, fmt.Errorf("GetAllEwarBanRules scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CreateEwarBanRule adds a rule and returns its ID.
func CreateEwarBanRule(ctx context.Context, db *sql.DB, r EwarBanRule) (int64, error) {
	res, err := db.ExecContext(ctx,
		`INSERT INTO npcEwarBanRules (ruleName, ewarKind, secMin, secMax, description) VALUES (?, ?, ?, ?, ?)`,
		r.RuleName, r.Kind, r.SecMin, r.SecMax, r.Description)
	if err != nil {
		return 0, fmt.Errorf("CreateEwarBanRule: %w", err)
	}
	return res.LastInsertId()
}

// UpdateEwarBanRule rewrites a rule.
func UpdateEwarBanRule(ctx context.Context, db *sql.DB, r EwarBanRule) error {
	if _, err := db.ExecContext(ctx,
		`UPDATE npcEwarBanRules SET ruleName=?, ewarKind=?, secMin=?, secMax=?, description=? WHERE ruleID=?`,
		r.RuleName, r.Kind, r.SecMin, r.SecMax, r.Description, r.RuleID); err != nil {
		return fmt.Errorf("UpdateEwarBanRule: %w", err)
	}
	return nil
}

// DeleteEwarBanRule removes a rule.
func DeleteEwarBanRule(ctx context.Context, db *sql.DB, ruleID int64) error {
	if _, err := db.ExecContext(ctx, `DELETE FROM npcEwarBanRules WHERE ruleID = ?`, ruleID); err != nil {
		return fmt.Errorf("DeleteEwarBanRule: %w", err)
	}
	return nil
}
