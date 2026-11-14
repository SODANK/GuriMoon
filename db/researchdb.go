package db

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"time"
)

// R&D seed data: agtResearchAgents (CCP SDE) restricted to this repo's type-4 agents, and
// field blueprint pools rebuilt from invention datacore requirements. See docs/rnd-agents.md.
//
//go:embed researchseed/agent_fields.csv
var seedAgentFields string

//go:embed researchseed/corp_fields.csv
var seedCorpFields string

//go:embed researchseed/field_blueprints.csv
var seedFieldBlueprints string

// EnsureResearchTables creates the R&D tables and seeds the static ones (agent fields, corp
// fields, patent pools) when empty.
func EnsureResearchTables(ctx context.Context, db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS agtresearchagents (
			agentID  INT UNSIGNED NOT NULL DEFAULT 0,
			typeID   INT UNSIGNED NOT NULL DEFAULT 0,
			PRIMARY KEY (agentID, typeID)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`,
		`CREATE TABLE IF NOT EXISTS crpnpccorporationresearchfields (
			skillID       INT NOT NULL DEFAULT 0,
			corporationID INT NOT NULL DEFAULT 0,
			supplierType  INT DEFAULT NULL,
			PRIMARY KEY (skillID, corporationID)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`,
		`CREATE TABLE IF NOT EXISTS rndfieldblueprints (
			skillID         INT UNSIGNED NOT NULL DEFAULT 0,
			blueprintTypeID INT UNSIGNED NOT NULL DEFAULT 0,
			PRIMARY KEY (skillID, blueprintTypeID)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`,
		`CREATE TABLE IF NOT EXISTS characterresearch (
			charID        BIGINT NOT NULL,
			agentID       BIGINT NOT NULL,
			skillTypeID   INT NOT NULL,
			pointsPerDay  DOUBLE NOT NULL DEFAULT 0,
			accruedPoints DOUBLE NOT NULL DEFAULT 0,
			accrualStart  BIGINT NOT NULL DEFAULT 0,
			startedAt     BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (charID, agentID)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("EnsureResearchTables: %w", err)
		}
	}

	seed := func(table, cols, csv string) error {
		var cnt int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&cnt); err != nil {
			return err
		}
		if cnt > 0 {
			return nil
		}
		var vals []string
		for _, line := range strings.Split(strings.TrimSpace(csv), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			vals = append(vals, "("+line+")")
		}
		if len(vals) == 0 {
			return nil
		}
		_, err := db.ExecContext(ctx, `INSERT IGNORE INTO `+table+` (`+cols+`) VALUES `+strings.Join(vals, ","))
		return err
	}
	if err := seed("agtresearchagents", "agentID, typeID", seedAgentFields); err != nil {
		return fmt.Errorf("EnsureResearchTables seed agtresearchagents: %w", err)
	}
	if err := seed("crpnpccorporationresearchfields", "skillID, corporationID", seedCorpFields); err != nil {
		return fmt.Errorf("EnsureResearchTables seed crpnpccorporationresearchfields: %w", err)
	}
	if err := seed("rndfieldblueprints", "skillID, blueprintTypeID", seedFieldBlueprints); err != nil {
		return fmt.Errorf("EnsureResearchTables seed rndfieldblueprints: %w", err)
	}
	return nil
}

// GetAgentResearchFields returns the research field skill typeIDs the agent
// offers, ordered by field name for stable dialogue button order.
func GetAgentResearchFields(ctx context.Context, db *sql.DB, agentID int64) ([]int64, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT r.typeID FROM agtresearchagents r
		LEFT JOIN invTypes t ON t.typeID = r.typeID
		WHERE r.agentID = ?
		ORDER BY t.typeName`, agentID)
	if err != nil {
		return nil, fmt.Errorf("GetAgentResearchFields: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// GetCorpResearchFields returns the research field skill typeIDs an NPC corp researches.
// Callers use it when an agent has no per-agent rows.
func GetCorpResearchFields(ctx context.Context, db *sql.DB, corporationID int64) ([]int64, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT f.skillID FROM crpnpccorporationresearchfields f
		LEFT JOIN invTypes t ON t.typeID = f.skillID
		WHERE f.corporationID = ?
		ORDER BY t.typeName`, corporationID)
	if err != nil {
		return nil, fmt.Errorf("GetCorpResearchFields: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// GetFieldBlueprints returns the T2 blueprint typeIDs in a research field's
// patent pool.
func GetFieldBlueprints(ctx context.Context, db *sql.DB, skillID int64) ([]int64, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT blueprintTypeID FROM rndfieldblueprints WHERE skillID = ? ORDER BY blueprintTypeID`, skillID)
	if err != nil {
		return nil, fmt.Errorf("GetFieldBlueprints: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// CharacterResearch is one active R&D project (one per char+agent pair). Points accrue lazily:
// CurrentPoints = accruedPoints + pointsPerDay x days since accrualStart. A rate change banks
// the balance and restarts the clock (UpdateResearchRate).
type CharacterResearch struct {
	CharID        int64
	AgentID       int64
	SkillTypeID   int64
	PointsPerDay  float64
	AccruedPoints float64
	AccrualStart  int64
	StartedAt     int64
}

// CurrentPoints returns the research point balance as of now.
func (r *CharacterResearch) CurrentPoints(now time.Time) float64 {
	elapsed := now.Unix() - r.AccrualStart
	if elapsed < 0 {
		elapsed = 0
	}
	return r.AccruedPoints + r.PointsPerDay*float64(elapsed)/86400.0
}

// GetCharacterResearchList returns every active research project for a
// character. agentFilter > 0 limits to that agent.
func GetCharacterResearchList(ctx context.Context, db *sql.DB, charID, agentFilter int64) ([]*CharacterResearch, error) {
	q := `SELECT charID, agentID, skillTypeID, pointsPerDay, accruedPoints, accrualStart, startedAt
		FROM characterresearch WHERE charID = ?`
	args := []any{charID}
	if agentFilter > 0 {
		q += ` AND agentID = ?`
		args = append(args, agentFilter)
	}
	rows, err := db.QueryContext(ctx, q+` ORDER BY agentID`, args...)
	if err != nil {
		return nil, fmt.Errorf("GetCharacterResearchList: %w", err)
	}
	defer rows.Close()
	var out []*CharacterResearch
	for rows.Next() {
		r := &CharacterResearch{}
		if err := rows.Scan(&r.CharID, &r.AgentID, &r.SkillTypeID, &r.PointsPerDay,
			&r.AccruedPoints, &r.AccrualStart, &r.StartedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetCharacterResearchForAgent returns the active project with one agent, or
// nil when none exists.
func GetCharacterResearchForAgent(ctx context.Context, db *sql.DB, charID, agentID int64) (*CharacterResearch, error) {
	r := &CharacterResearch{}
	err := db.QueryRowContext(ctx, `
		SELECT charID, agentID, skillTypeID, pointsPerDay, accruedPoints, accrualStart, startedAt
		FROM characterresearch WHERE charID = ? AND agentID = ?`, charID, agentID,
	).Scan(&r.CharID, &r.AgentID, &r.SkillTypeID, &r.PointsPerDay,
		&r.AccruedPoints, &r.AccrualStart, &r.StartedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("GetCharacterResearchForAgent: %w", err)
	}
	return r, nil
}

// CountCharacterResearch returns how many R&D projects the character runs.
func CountCharacterResearch(ctx context.Context, db *sql.DB, charID int64) (int, error) {
	var cnt int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM characterresearch WHERE charID = ?`, charID).Scan(&cnt)
	return cnt, err
}

// ResearchPointsPerDay returns rateMul x (1 + quality/100) x (agentLevel + fieldSkillLevel)^2.
// Every enrolment path (dialogue and modules) must use it so rates cannot drift.
func ResearchPointsPerDay(agent *AgentInfo, fieldSkillLevel int, rateMul float64) float64 {
	if agent == nil {
		return 0
	}
	qualityFactor := 1 + float64(agent.Quality)/100.0
	if qualityFactor < 0 {
		qualityFactor = 0
	}
	levelSum := float64(agent.Level + fieldSkillLevel)
	return rateMul * qualityFactor * levelSum * levelSum
}

// StartResearch begins a project with an agent and fails if one exists for the pair. It also
// opens the agent's patent round on the first collaborator (rndlottery.go), so every enrolment
// path must call it.
func StartResearch(ctx context.Context, db *sql.DB, charID, agentID, skillTypeID int64, pointsPerDay float64) error {
	now := time.Now()
	unix := now.Unix()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("StartResearch begin: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO characterresearch (charID, agentID, skillTypeID, pointsPerDay, accruedPoints, accrualStart, startedAt)
		VALUES (?, ?, ?, ?, 0, ?, ?)`, charID, agentID, skillTypeID, pointsPerDay, unix, unix); err != nil {
		return fmt.Errorf("StartResearch: %w", err)
	}
	if err := EnsureAgentRound(ctx, tx, agentID, now, GetPatentRoundDays(ctx, db)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("StartResearch commit: %w", err)
	}
	return nil
}

// StopResearch cancels the project with an agent. Accumulated points are forfeit.
func StopResearch(ctx context.Context, db *sql.DB, charID, agentID int64) error {
	_, err := db.ExecContext(ctx,
		`DELETE FROM characterresearch WHERE charID = ? AND agentID = ?`, charID, agentID)
	if err != nil {
		return fmt.Errorf("StopResearch: %w", err)
	}
	return nil
}

// ── ESP R&D browser queries ─────────────────────────────────────────────────

// ESPResearchField summarises one research field for the ESP R&D page.
type ESPResearchField struct {
	SkillID        int64
	FieldName      string
	AgentCount     int
	CorpCount      int
	BlueprintCount int
	ProjectCount   int
}

// ESPGetResearchFields lists every research field with agent/corp/patent-pool
// counts and the number of active player projects.
func ESPGetResearchFields(ctx context.Context, db *sql.DB) ([]ESPResearchField, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT r.typeID, COALESCE(t.typeName, ''),
		       COUNT(DISTINCT r.agentID), COUNT(DISTINCT a.corporationID),
		       (SELECT COUNT(*) FROM rndfieldblueprints b WHERE b.skillID = r.typeID),
		       (SELECT COUNT(*) FROM characterresearch cr WHERE cr.skillTypeID = r.typeID)
		FROM agtresearchagents r
		LEFT JOIN invTypes t ON t.typeID = r.typeID
		LEFT JOIN agtAgents a ON a.agentID = r.agentID
		GROUP BY r.typeID, t.typeName
		ORDER BY t.typeName`)
	if err != nil {
		return nil, fmt.Errorf("ESPGetResearchFields: %w", err)
	}
	defer rows.Close()
	var out []ESPResearchField
	for rows.Next() {
		var f ESPResearchField
		if err := rows.Scan(&f.SkillID, &f.FieldName, &f.AgentCount, &f.CorpCount,
			&f.BlueprintCount, &f.ProjectCount); err != nil {
			return nil, fmt.Errorf("ESPGetResearchFields scan: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ESPResearchProject is one active player R&D project for the ESP R&D page.
type ESPResearchProject struct {
	CharID       int64
	CharName     string
	AgentID      int64
	AgentName    string
	SkillID      int64
	FieldName    string
	PointsPerDay float64
	Points       float64
	StartedAt    time.Time
}

// ESPGetActiveResearch lists all active player research projects.
func ESPGetActiveResearch(ctx context.Context, db *sql.DB) ([]ESPResearchProject, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT cr.charID, COALESCE(c.characterName, ''), cr.agentID, COALESCE(an.ownerName, ''),
		       cr.skillTypeID, COALESCE(t.typeName, ''),
		       cr.pointsPerDay, cr.accruedPoints, cr.accrualStart, cr.startedAt
		FROM characterresearch cr
		LEFT JOIN character_ c ON c.characterID = cr.charID
		LEFT JOIN eveStaticOwners an ON an.ownerID = cr.agentID
		LEFT JOIN invTypes t ON t.typeID = cr.skillTypeID
		ORDER BY c.characterName, an.ownerName`)
	if err != nil {
		return nil, fmt.Errorf("ESPGetActiveResearch: %w", err)
	}
	defer rows.Close()
	now := time.Now()
	var out []ESPResearchProject
	for rows.Next() {
		var p ESPResearchProject
		var accrued float64
		var accrualStart, startedAt int64
		if err := rows.Scan(&p.CharID, &p.CharName, &p.AgentID, &p.AgentName,
			&p.SkillID, &p.FieldName, &p.PointsPerDay, &accrued, &accrualStart, &startedAt); err != nil {
			return nil, fmt.Errorf("ESPGetActiveResearch scan: %w", err)
		}
		r := CharacterResearch{PointsPerDay: p.PointsPerDay, AccruedPoints: accrued, AccrualStart: accrualStart}
		p.Points = r.CurrentPoints(now)
		p.StartedAt = time.Unix(startedAt, 0)
		out = append(out, p)
	}
	return out, rows.Err()
}

// ESPFieldBlueprint is one patent-pool entry for the ESP R&D field detail.
type ESPFieldBlueprint struct {
	BlueprintTypeID int64
	BlueprintName   string
	ProductTypeID   int64
	ProductName     string
}

// ESPGetFieldBlueprints lists a field's patent pool with product names.
func ESPGetFieldBlueprints(ctx context.Context, db *sql.DB, skillID int64) ([]ESPFieldBlueprint, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT b.blueprintTypeID, COALESCE(t.typeName, ''),
		       COALESCE(bt.productTypeID, 0), COALESCE(pt.typeName, '')
		FROM rndfieldblueprints b
		LEFT JOIN invTypes t ON t.typeID = b.blueprintTypeID
		LEFT JOIN invBlueprintTypes bt ON bt.blueprintTypeID = b.blueprintTypeID
		LEFT JOIN invTypes pt ON pt.typeID = bt.productTypeID
		WHERE b.skillID = ?
		ORDER BY t.typeName`, skillID)
	if err != nil {
		return nil, fmt.Errorf("ESPGetFieldBlueprints: %w", err)
	}
	defer rows.Close()
	var out []ESPFieldBlueprint
	for rows.Next() {
		var b ESPFieldBlueprint
		if err := rows.Scan(&b.BlueprintTypeID, &b.BlueprintName, &b.ProductTypeID, &b.ProductName); err != nil {
			return nil, fmt.Errorf("ESPGetFieldBlueprints scan: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ESPResearchAgent is one R&D agent offering a field, for the field detail.
type ESPResearchAgent struct {
	AgentID     int64
	AgentName   string
	Level       int
	Quality     int
	CorpName    string
	StationName string
}

// ESPGetFieldAgents lists the R&D agents offering a research field.
func ESPGetFieldAgents(ctx context.Context, db *sql.DB, skillID int64) ([]ESPResearchAgent, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT r.agentID, COALESCE(an.ownerName, ''), COALESCE(a.level, 0), COALESCE(a.quality, 0),
		       COALESCE(c.corporationName, COALESCE(co.ownerName, '')), COALESCE(s.stationName, '')
		FROM agtresearchagents r
		LEFT JOIN agtAgents a ON a.agentID = r.agentID
		LEFT JOIN eveStaticOwners an ON an.ownerID = r.agentID
		LEFT JOIN corporation c ON c.corporationID = a.corporationID
		LEFT JOIN eveStaticOwners co ON co.ownerID = a.corporationID
		LEFT JOIN staStations s ON s.stationID = a.stationID
		WHERE r.typeID = ?
		ORDER BY a.level DESC, a.quality DESC`, skillID)
	if err != nil {
		return nil, fmt.Errorf("ESPGetFieldAgents: %w", err)
	}
	defer rows.Close()
	var out []ESPResearchAgent
	for rows.Next() {
		var a ESPResearchAgent
		if err := rows.Scan(&a.AgentID, &a.AgentName, &a.Level, &a.Quality, &a.CorpName, &a.StationName); err != nil {
			return nil, fmt.Errorf("ESPGetFieldAgents scan: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// UpdateResearchRate banks the current point balance and restarts accrual at a new RP/day.
func UpdateResearchRate(ctx context.Context, db *sql.DB, r *CharacterResearch, newPointsPerDay float64) error {
	now := time.Now()
	banked := r.CurrentPoints(now)
	_, err := db.ExecContext(ctx, `
		UPDATE characterresearch SET pointsPerDay = ?, accruedPoints = ?, accrualStart = ?
		WHERE charID = ? AND agentID = ?`, newPointsPerDay, banked, now.Unix(), r.CharID, r.AgentID)
	if err != nil {
		return fmt.Errorf("UpdateResearchRate: %w", err)
	}
	r.PointsPerDay = newPointsPerDay
	r.AccruedPoints = banked
	r.AccrualStart = now.Unix()
	return nil
}
