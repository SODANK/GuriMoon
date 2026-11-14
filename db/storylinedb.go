package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"
)

// ── Storyline missions ───────────────────────────────────────────────────────
//
// Storyline agents run the same offer/accept/complete pipeline as other agents but draw only
// templates flagged storyline = 1, and have nothing for a character who has not been invited.
// The invite is earned per faction. See docs/db-10-notes.md#storyline-invites.

// Storyline agent types in agtAgentTypes: 7 is the storyline agent, 6 the generic variant.
// Both are gated the same way.
const (
	AgentTypeGenericStoryline int64 = 6
	AgentTypeStoryline        int64 = 7
)

// IsStorylineAgentType reports whether agentTypeID is a storyline type. Those agents draw the
// storyline deck and refuse uninvited characters.
func IsStorylineAgentType(agentTypeID int64) bool {
	return agentTypeID == AgentTypeStoryline || agentTypeID == AgentTypeGenericStoryline
}

// settingStorylineThreshold is the server_settings key for the same-faction run length that
// earns an invite.
const settingStorylineThreshold = "storyline.missionThreshold"

// DefaultStorylineThreshold is the run length when the setting is unset. It is editable on the
// mission distribution page.
const DefaultStorylineThreshold = 12

// StorylineProgress is one character's run toward their next storyline invite.
// A character with no row has never completed a mission.
type StorylineProgress struct {
	CharID int64
	// FactionID is the faction of the agent whose mission they last finished;
	// Streak is how many in a row have been for that faction.
	FactionID int64
	Streak    int
	// PendingAgentID is the storyline agent waiting for them, 0 when none. The streak does not
	// advance while an invite is pending.
	PendingAgentID int64
	PendingSince   time.Time
}

// EnsureStorylineTables creates the storyline progress table.
func EnsureStorylineTables(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS chrStorylineProgress (
			charID         INT NOT NULL PRIMARY KEY,
			factionID      INT NOT NULL DEFAULT 0,
			streak         INT NOT NULL DEFAULT 0,
			pendingAgentID INT NOT NULL DEFAULT 0,
			pendingSince   BIGINT NOT NULL DEFAULT 0,
			INDEX idx_pending (pendingAgentID)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`)
	if err != nil {
		return fmt.Errorf("EnsureStorylineTables: %w", err)
	}
	return nil
}

// GetStorylineThreshold returns the configured run length, falling back to
// DefaultStorylineThreshold when unset or unparseable. A threshold below 1
// would make every mission a storyline trigger, so it is clamped.
func GetStorylineThreshold(ctx context.Context, db *sql.DB) int {
	v, ok, err := GetServerSetting(ctx, db, settingStorylineThreshold)
	if err != nil || !ok {
		return DefaultStorylineThreshold
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return DefaultStorylineThreshold
	}
	return n
}

// SetStorylineThreshold persists the run length.
func SetStorylineThreshold(ctx context.Context, db *sql.DB, n int) error {
	if n < 1 {
		n = 1
	}
	return SetServerSetting(ctx, db, settingStorylineThreshold, strconv.Itoa(n))
}

// GetStorylineProgress returns a character's run. A character with no row gets
// a zero-valued progress, which is the same thing as "no run yet".
func GetStorylineProgress(ctx context.Context, db *sql.DB, charID int64) (*StorylineProgress, error) {
	p := &StorylineProgress{CharID: charID}
	var since int64
	err := db.QueryRowContext(ctx, `
		SELECT factionID, streak, pendingAgentID, pendingSince
		FROM chrStorylineProgress WHERE charID = ?`, charID).
		Scan(&p.FactionID, &p.Streak, &p.PendingAgentID, &since)
	if errors.Is(err, sql.ErrNoRows) {
		return p, nil
	}
	if err != nil {
		return nil, fmt.Errorf("GetStorylineProgress: %w", err)
	}
	if since > 0 {
		p.PendingSince = time.Unix(since, 0)
	}
	return p, nil
}

// RecordMissionCompletion advances a character's run after a regular mission for an agent of
// factionID and returns the new streak. A different faction restarts the count at 1. The caller
// decides whether an invite is owed.
// See docs/db-10-notes.md#storyline-invites.
func RecordMissionCompletion(ctx context.Context, db *sql.DB, charID, factionID int64) (int, error) {
	if factionID == 0 {
		// An agent whose corporation has no faction leaves the existing run untouched.
		p, err := GetStorylineProgress(ctx, db, charID)
		if err != nil {
			return 0, err
		}
		return p.Streak, nil
	}
	p, err := GetStorylineProgress(ctx, db, charID)
	if err != nil {
		return 0, err
	}
	streak := 1
	if p.FactionID == factionID {
		streak = p.Streak + 1
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO chrStorylineProgress (charID, factionID, streak)
		VALUES (?, ?, ?)
		ON DUPLICATE KEY UPDATE factionID = VALUES(factionID), streak = VALUES(streak)`,
		charID, factionID, streak); err != nil {
		return 0, fmt.Errorf("RecordMissionCompletion: %w", err)
	}
	return streak, nil
}

// SetStorylinePending records that agentID has invited the character and zeroes the run.
func SetStorylinePending(ctx context.Context, db *sql.DB, charID, agentID, factionID int64) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO chrStorylineProgress (charID, factionID, streak, pendingAgentID, pendingSince)
		VALUES (?, ?, 0, ?, ?)
		ON DUPLICATE KEY UPDATE
		    factionID = VALUES(factionID), streak = 0,
		    pendingAgentID = VALUES(pendingAgentID), pendingSince = VALUES(pendingSince)`,
		charID, factionID, agentID, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("SetStorylinePending: %w", err)
	}
	return nil
}

// ClearStorylinePending drops the invite once the storyline mission is over,
// however it ended.
func ClearStorylinePending(ctx context.Context, db *sql.DB, charID int64) error {
	_, err := db.ExecContext(ctx, `
		UPDATE chrStorylineProgress SET pendingAgentID = 0, pendingSince = 0
		 WHERE charID = ?`, charID)
	if err != nil {
		return fmt.Errorf("ClearStorylinePending: %w", err)
	}
	return nil
}

// StorylineAgentPick is a storyline agent chosen to issue an invite.
type StorylineAgentPick struct {
	AgentID       int64
	AgentName     string
	CorporationID int64
	StationID     int64
	StationName   string
	SolarSystemID int64
	SystemName    string
	Level         int
}

// PickStorylineAgent finds a storyline agent of factionID to invite a character. Candidates rank
// by nearness to nearSystemID (system, constellation, region, anywhere), then by matching level,
// then randomly. A faction with no agent at that level still qualifies.
func PickStorylineAgent(ctx context.Context, db *sql.DB, factionID int64, level int, nearSystemID int64) (*StorylineAgentPick, error) {
	// Rank by distance to the player's last system, then level match, then RAND() to spread invites.
	row := db.QueryRowContext(ctx, `
		SELECT a.agentID, COALESCE(n.itemName,''), a.corporationID, COALESCE(a.stationID,0),
		       COALESCE(sn.itemName,''), COALESCE(m.solarSystemID,0), COALESCE(m.itemName,''),
		       COALESCE(a.level,1)
		FROM agtAgents a
		JOIN crpNPCCorporations c ON c.corporationID = a.corporationID
		LEFT JOIN staStations s   ON s.stationID = a.stationID
		LEFT JOIN mapDenormalize m ON m.itemID = s.solarSystemID
		LEFT JOIN eveNames n      ON n.itemID = a.agentID
		LEFT JOIN eveNames sn     ON sn.itemID = a.stationID
		WHERE c.factionID = ? AND a.agentTypeID IN (?, ?) AND a.stationID IS NOT NULL
		ORDER BY
		    CASE
		        WHEN m.solarSystemID = ?      THEN 0
		        WHEN m.constellationID = (SELECT constellationID FROM mapDenormalize WHERE itemID = ?) THEN 1
		        WHEN m.regionID = (SELECT regionID FROM mapDenormalize WHERE itemID = ?) THEN 2
		        ELSE 3
		    END,
		    CASE WHEN COALESCE(a.level,1) = ? THEN 0 ELSE 1 END,
		    RAND()
		LIMIT 1`,
		factionID, AgentTypeStoryline, AgentTypeGenericStoryline,
		nearSystemID, nearSystemID, nearSystemID, level)

	p := &StorylineAgentPick{}
	err := row.Scan(&p.AgentID, &p.AgentName, &p.CorporationID, &p.StationID,
		&p.StationName, &p.SolarSystemID, &p.SystemName, &p.Level)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("PickStorylineAgent: %w", err)
	}
	return p, nil
}

// ESPStorylineLevelRow is one agent level's storyline staffing and stock. Usable excludes kill
// templates the offer path rejects (no dungeonVID, or a dungeon with no rooms).
type ESPStorylineLevelRow struct {
	Level     int
	Agents    int
	Templates int
	Usable    int
}

// CountStorylineTemplates returns storyline template stock per agent level.
func CountStorylineTemplates(ctx context.Context, db *sql.DB) (map[int]ESPStorylineLevelRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT t.agentLevel, COUNT(*),
		       CAST(SUM(CASE WHEN t.missionType <> 'kill'
		                       OR (t.dungeonVID > 0
		                           AND EXISTS (SELECT 1 FROM dunRooms r WHERE r.dungeonVID = t.dungeonVID))
		                     THEN 1 ELSE 0 END) AS SIGNED)
		FROM missionTemplates t
		WHERE t.enabled = 1 AND COALESCE(t.storyline,0) = 1
		GROUP BY t.agentLevel`)
	if err != nil {
		return nil, fmt.Errorf("CountStorylineTemplates: %w", err)
	}
	defer rows.Close()
	out := map[int]ESPStorylineLevelRow{}
	for rows.Next() {
		var r ESPStorylineLevelRow
		if err := rows.Scan(&r.Level, &r.Templates, &r.Usable); err != nil {
			return nil, fmt.Errorf("CountStorylineTemplates scan: %w", err)
		}
		out[r.Level] = r
	}
	return out, rows.Err()
}

// StorylineFactionRow is one faction's storyline staffing, for the
// distribution page.
type StorylineFactionRow struct {
	FactionID   int64
	FactionName string
	Agents      int
	Levels      []int // distinct agent levels staffed, ascending
}

// CountStorylineAgentsByFaction returns storyline agent headcount per faction. A faction with
// none can never pay out a run.
func CountStorylineAgentsByFaction(ctx context.Context, db *sql.DB) ([]StorylineFactionRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT c.factionID, COALESCE(n.itemName,''), COUNT(*), COALESCE(a.level,1)
		FROM agtAgents a
		JOIN crpNPCCorporations c ON c.corporationID = a.corporationID
		LEFT JOIN eveNames n ON n.itemID = c.factionID
		WHERE a.agentTypeID IN (?, ?)
		GROUP BY c.factionID, n.itemName, a.level
		ORDER BY n.itemName, a.level`, AgentTypeStoryline, AgentTypeGenericStoryline)
	if err != nil {
		return nil, fmt.Errorf("CountStorylineAgentsByFaction: %w", err)
	}
	defer rows.Close()
	byFaction := map[int64]*StorylineFactionRow{}
	var order []int64
	for rows.Next() {
		var factionID int64
		var name string
		var n, level int
		if err := rows.Scan(&factionID, &name, &n, &level); err != nil {
			return nil, fmt.Errorf("CountStorylineAgentsByFaction scan: %w", err)
		}
		r, ok := byFaction[factionID]
		if !ok {
			r = &StorylineFactionRow{FactionID: factionID, FactionName: name}
			byFaction[factionID] = r
			order = append(order, factionID)
		}
		r.Agents += n
		r.Levels = append(r.Levels, level)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]StorylineFactionRow, 0, len(order))
	for _, id := range order {
		out = append(out, *byFaction[id])
	}
	return out, nil
}

// ESPStorylineOverview is the storyline panel on the mission distribution page.
type ESPStorylineOverview struct {
	Threshold int
	Agents    int
	Factions  []StorylineFactionRow
	Levels    []ESPStorylineLevelRow
	Templates int
	Usable    int
	Pending   int      // invites currently outstanding across all characters
	Warnings  []string // the ways a run can be earned and then go nowhere
}

// ESPGetStorylineOverview assembles the storyline panel. Each warning names a way an earned run
// fails to pay out.
func ESPGetStorylineOverview(ctx context.Context, db *sql.DB) (*ESPStorylineOverview, error) {
	o := &ESPStorylineOverview{Threshold: GetStorylineThreshold(ctx, db)}

	factions, err := CountStorylineAgentsByFaction(ctx, db)
	if err != nil {
		return nil, err
	}
	o.Factions = factions

	byLevel, err := CountStorylineTemplates(ctx, db)
	if err != nil {
		return nil, err
	}

	// Storyline agent headcount per level; a level with agents and no templates is a dead end.
	agentsByLevel := map[int]int{}
	rows, err := db.QueryContext(ctx, `
		SELECT COALESCE(level,1), COUNT(*) FROM agtAgents
		 WHERE agentTypeID IN (?, ?) GROUP BY level`,
		AgentTypeStoryline, AgentTypeGenericStoryline)
	if err != nil {
		return nil, fmt.Errorf("ESPGetStorylineOverview agents: %w", err)
	}
	for rows.Next() {
		var level, n int
		if err := rows.Scan(&level, &n); err != nil {
			rows.Close()
			return nil, fmt.Errorf("ESPGetStorylineOverview agents scan: %w", err)
		}
		agentsByLevel[level] = n
		o.Agents += n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ESPGetStorylineOverview agents: %w", err)
	}

	levelSet := map[int]bool{}
	for lvl := range agentsByLevel {
		levelSet[lvl] = true
	}
	for lvl := range byLevel {
		levelSet[lvl] = true
	}
	var levels []int
	for lvl := range levelSet {
		levels = append(levels, lvl)
	}
	sort.Ints(levels)
	for _, lvl := range levels {
		r := byLevel[lvl]
		r.Level = lvl
		r.Agents = agentsByLevel[lvl]
		o.Levels = append(o.Levels, r)
		o.Templates += r.Templates
		o.Usable += r.Usable
	}

	_ = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM chrStorylineProgress WHERE pendingAgentID > 0`).Scan(&o.Pending)

	// Warnings, in the order they bite.
	if o.Templates == 0 {
		o.Warnings = append(o.Warnings,
			"No storyline templates exist. Every run earned anywhere in the game ends with an agent who has nothing to give.")
	} else if o.Usable == 0 {
		o.Warnings = append(o.Warnings,
			"Every storyline template is unusable — a kill template with no dungeon, or a dungeon with no rooms. The offer path rejects all of them.")
	}
	if o.Agents == 0 {
		o.Warnings = append(o.Warnings,
			"No agents carry a storyline agent type, so no invite can ever be issued.")
	}
	for _, lvl := range o.Levels {
		if lvl.Agents > 0 && lvl.Usable == 0 {
			o.Warnings = append(o.Warnings, fmt.Sprintf(
				"Level %d has %d storyline agent(s) and no usable storyline template — an invited pilot gets turned away.",
				lvl.Level, lvl.Agents))
		}
	}
	return o, nil
}
