package db

import (
	"context"
	"database/sql"
	"math"
	"strings"

	"gurimoon/marshal"
)

// Standing transaction event types (eveconstants.sql eventStanding* values).
// The client's FmtStandingTransaction switches on them.
const (
	EventStandingAgentMissionCompleted    = 73
	EventStandingAgentMissionFailed       = 74
	EventStandingAgentMissionDeclined     = 75
	EventStandingCombatAggression         = 76
	EventStandingCombatShipKill           = 77
	EventStandingCombatPodKill            = 78
	EventStandingCombatAssistance         = 112
	EventStandingContrabandTrafficking    = 126
	EventStandingSlashSet                 = 84
	EventStandingPirateKillSecurityStatus = 89
	EventStandingPlayerSetStanding        = 65
)

// OwnerCONCORD is the CONCORD corporation ownerID. The client shows a
// character's standing from this owner as their security status.
const OwnerCONCORD = 1000125

// Social skills that modify effective standings (client cfg.py CanUseAgent,
// standingsvc.GetEffectiveStanding). Each level is worth a 0.4 bonus.
const (
	SkillDiplomacy           = 3357
	SkillConnections         = 3359
	SkillCriminalConnections = 3361
)

// Factions whose agents refuse everyone (client cfg.py CanUseAgent).
const (
	FactionJoveEmpire       = 500005
	FactionCONCORDAssembly  = 500006
	FactionSocietyOfThought = 500017
)

// criminalConnectionsFactions are the pirate factions Criminal Connections
// applies to (hardcoded in the client's cfg.py and standingsvc.py).
var criminalConnectionsFactions = map[int64]bool{
	500010: true, // Guristas Pirates
	500011: true, // Angel Cartel
	500012: true, // Blood Raider Covenant
	500019: true, // Sansha's Nation
	500020: true, // Serpentis
}

// StandingSkillBonus returns the social-skill standing bonus toward an NPC
// entity, mirroring client cfg.py CanUseAgent: Connections when the corp or
// faction standing is positive, Diplomacy when negative, Criminal Connections
// for the five pirate factions. The best bonus wins, 0.4 per level.
func StandingSkillBonus(ctx context.Context, db *sql.DB, charID int64, corpStanding, factionStanding float64, factionID int64) float64 {
	bonus := 0.0
	if corpStanding > 0.0 || factionStanding > 0.0 {
		if lvl, _ := GetCharacterSkillLevel(ctx, db, charID, SkillConnections); float64(lvl)*0.4 > bonus {
			bonus = float64(lvl) * 0.4
		}
	}
	if corpStanding < 0.0 || factionStanding < 0.0 {
		if lvl, _ := GetCharacterSkillLevel(ctx, db, charID, SkillDiplomacy); float64(lvl)*0.4 > bonus {
			bonus = float64(lvl) * 0.4
		}
	}
	if criminalConnectionsFactions[factionID] {
		if lvl, _ := GetCharacterSkillLevel(ctx, db, charID, SkillCriminalConnections); float64(lvl)*0.4 > bonus {
			bonus = float64(lvl) * 0.4
		}
	}
	return bonus
}

// ApplyStandingBonus folds a skill bonus into a raw standing with the client's
// formula (1 - (1 - s/10)(1 - b/10)) * 10. A zero bonus returns the standing.
func ApplyStandingBonus(standing, bonus float64) float64 {
	if bonus <= 0 {
		return standing
	}
	return (1.0 - (1.0-standing/10.0)*(1.0-bonus/10.0)) * 10.0
}

// AgentUseCheck is the result of CanUseAgent. Effective is the number the
// agent conversation window displays; Minimum is the lowest of the three.
type AgentUseCheck struct {
	Allowed   bool
	Required  float64
	Effective float64
	Minimum   float64
}

// CanUseAgent mirrors the client's util.CanUseAgent (cfg.py): access needs
// max(faction, corp, agent standing) >= (level-1)*2 + quality*0.05 and
// min > -2.0, with social-skill bonuses folded in. Level > 4 and Jove, CONCORD
// and Society of Conscious Thought agents refuse; research agents also need corp
// standing >= required-2. The station lobby runs the same formula client-side.
func CanUseAgent(ctx context.Context, db *sql.DB, charID int64, agent *AgentInfo) AgentUseCheck {
	chk := AgentUseCheck{Required: float64(agent.Level-1)*2.0 + float64(agent.Quality)*5.0/100.0}
	factionID, _ := GetAgentCorpFactionID(ctx, db, agent.CorporationID)
	if agent.Level > 4 || agent.CorporationID == OwnerCONCORD ||
		factionID == FactionJoveEmpire || factionID == FactionCONCORDAssembly || factionID == FactionSocietyOfThought {
		return chk
	}

	fac := GetNPCStandingToChar(ctx, db, charID, factionID)
	coc := GetNPCStandingToChar(ctx, db, charID, agent.CorporationID)
	cac := GetNPCStandingToChar(ctx, db, charID, agent.AgentID)
	if bonus := StandingSkillBonus(ctx, db, charID, coc, fac, factionID); bonus > 0 {
		fac = ApplyStandingBonus(fac, bonus)
		coc = ApplyStandingBonus(coc, bonus)
		cac = ApplyStandingBonus(cac, bonus)
	}
	chk.Effective = math.Max(fac, math.Max(coc, cac))
	chk.Minimum = math.Min(fac, math.Min(coc, cac))
	if chk.Effective >= chk.Required && chk.Minimum > -2.0 {
		if agent.AgentTypeID == AgentTypeResearch && coc < chk.Required-2.0 {
			return chk
		}
		chk.Allowed = true
	}
	return chk
}

// EnsureStandingTables creates the mutable standing tables if missing.
// chrNPCStandings and npcStandings come from the static dump; the tables
// created here start empty. Call once at startup.
// See docs/db-9-notes.md#standing-tables.
func EnsureStandingTables(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS chrStandings (
			characterID INT NOT NULL,
			toID        INT NOT NULL,
			standing    DOUBLE NOT NULL DEFAULT 0,
			PRIMARY KEY (characterID, toID)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS chrStandingChanges (
			changeID            BIGINT AUTO_INCREMENT PRIMARY KEY,
			transactionDateTime DATETIME NOT NULL,
			fromID              INT NOT NULL,
			toID                INT NOT NULL,
			actionID            INT NOT NULL DEFAULT 0,
			deltaStanding       DOUBLE NOT NULL DEFAULT 0,
			message             TEXT,
			isPersonal          TINYINT(1) NOT NULL DEFAULT 1,
			INDEX idx_toID (toID)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`); err != nil {
		return err
	}
	// Column names match the fields FmtStandingTransaction reads. eventDateTime is
	// a Win32 FILETIME; modification is displayed standing / 10 (the client renders
	// modification*100 as a percentage).
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS chrStandingTransactions (
			eventID        BIGINT AUTO_INCREMENT PRIMARY KEY,
			eventTypeID    INT NOT NULL,
			eventDateTime  BIGINT UNSIGNED NOT NULL,
			fromID         BIGINT NOT NULL,
			toID           BIGINT NOT NULL,
			modification   DOUBLE NOT NULL DEFAULT 0,
			originalFromID BIGINT NOT NULL,
			originalToID   BIGINT NOT NULL,
			int_1          BIGINT NOT NULL DEFAULT 0,
			int_2          BIGINT NOT NULL DEFAULT 0,
			int_3          BIGINT NOT NULL DEFAULT 0,
			msg            TEXT,
			INDEX idx_from_to (fromID, toID, eventID)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`); err != nil {
		return err
	}
	// Corporation standings live in corpstandingdb.go; this also moves legacy
	// corp-owned rows out of chrStandings.
	return EnsureCorpStandingTable(ctx, db)
}

// GetCharStandings returns a util.Rowset (toID, standing) of the character's
// standings toward other entities.
func GetCharStandings(ctx context.Context, db *sql.DB, charID int64) (marshal.PyRep, error) {
	return queryRows(ctx, db,
		[]string{"toID", "standing"},
		`SELECT toID, standing FROM chrStandings WHERE characterID = ?`, charID)
}

// GetCharNPCStandings returns a util.Rowset (fromID, standing) of NPC standings
// toward a character.
func GetCharNPCStandings(ctx context.Context, db *sql.DB, charID int64) (marshal.PyRep, error) {
	return queryRows(ctx, db,
		[]string{"fromID", "standing"},
		`SELECT fromID, standing FROM chrNPCStandings WHERE characterID = ?`, charID)
}

// GetCharNPCStandingsMap returns factionID to standing for a character's NPC
// standings, for hostility checks.
func GetCharNPCStandingsMap(ctx context.Context, db *sql.DB, charID int64) (map[int64]float64, error) {
	rows, err := db.QueryContext(ctx, `SELECT fromID, standing FROM chrNPCStandings WHERE characterID = ?`, charID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	m := make(map[int64]float64)
	for rows.Next() {
		var fromID int64
		var standing float64
		if err := rows.Scan(&fromID, &standing); err != nil {
			continue
		}
		m[fromID] = standing
	}
	return m, rows.Err()
}

// UpsertNPCStanding adds delta to a character's standing toward an NPC
// faction, inserting the row if absent, clamped to the [-10, 10] standing range.
// Returns the resulting standing.
func UpsertNPCStanding(ctx context.Context, db *sql.DB, charID, factionID int64, delta float64) (float64, error) {
	_, err := db.ExecContext(ctx,
		`INSERT INTO chrNPCStandings (characterID, fromID, standing) VALUES (?, ?, GREATEST(LEAST(?, 10), -10))
		 ON DUPLICATE KEY UPDATE standing = GREATEST(LEAST(standing + ?, 10), -10)`,
		charID, factionID, delta, delta)
	if err != nil {
		return 0, err
	}
	var standing float64
	err = db.QueryRowContext(ctx,
		`SELECT standing FROM chrNPCStandings WHERE characterID = ? AND fromID = ?`,
		charID, factionID).Scan(&standing)
	return standing, err
}

// UpsertPersonalStanding is UpsertNPCStanding on chrStandings (personal
// standing toward an entity such as an agent).
func UpsertPersonalStanding(ctx context.Context, db *sql.DB, charID, toID int64, delta float64) (float64, error) {
	_, err := db.ExecContext(ctx,
		`INSERT INTO chrStandings (characterID, toID, standing) VALUES (?, ?, GREATEST(LEAST(?, 10), -10))
		 ON DUPLICATE KEY UPDATE standing = GREATEST(LEAST(standing + ?, 10), -10)`,
		charID, toID, delta, delta)
	if err != nil {
		return 0, err
	}
	var standing float64
	err = db.QueryRowContext(ctx,
		`SELECT standing FROM chrStandings WHERE characterID = ? AND toID = ?`,
		charID, toID).Scan(&standing)
	return standing, err
}

// InsertStandingTransaction logs a standing change for the client's standing
// transactions window and security status log. fromID is the entity whose
// opinion changed (OwnerCONCORD for security status), toID the character.
// delta is in displayed units (-10..+10) and stored /10. int1..int3 are
// event-specific (combat: victim ownerID, solarSystemID, ship typeID); msg
// carries the mission name for agent events.
func InsertStandingTransaction(ctx context.Context, db *sql.DB, eventTypeID, fromID, toID int64, delta float64, int1, int2, int3 int64, msg string) error {
	_, err := db.ExecContext(ctx,
		`INSERT INTO chrStandingTransactions
		 (eventTypeID, eventDateTime, fromID, toID, modification, originalFromID, originalToID, int_1, int_2, int_3, msg)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		eventTypeID, win32TimeNow(), fromID, toID, delta/10.0, fromID, toID, int1, int2, int3, msg)
	return err
}

// GetNPCStandingToChar returns an NPC entity's (agent, corp, faction) standing
// toward a character: the chrNPCStandings direction the client's
// standingsvc.GetStanding(fromID=npc, toID=char) reads. Zero when unset.
func GetNPCStandingToChar(ctx context.Context, db rowQueryer, charID, fromID int64) float64 {
	var standing float64
	db.QueryRowContext(ctx,
		`SELECT standing FROM chrNPCStandings WHERE characterID = ? AND fromID = ?`,
		charID, fromID).Scan(&standing)
	return standing
}

// GetPersonalStanding returns the character's personal standing toward an
// entity (agent, corp), or 0 if none is recorded.
func GetPersonalStanding(ctx context.Context, db *sql.DB, charID, toID int64) float64 {
	var standing float64
	db.QueryRowContext(ctx,
		`SELECT standing FROM chrStandings WHERE characterID = ? AND toID = ?`,
		charID, toID).Scan(&standing)
	return standing
}

// GetNPCStandings returns a util.Rowset (fromID, toID, standing) of all
// NPC-to-NPC faction standings.
func GetNPCStandings(ctx context.Context, db *sql.DB) (marshal.PyRep, error) {
	return queryRows(ctx, db,
		[]string{"fromID", "toID", "standing"},
		`SELECT fromID, toID, standing FROM npcStandings`)
}

// GetNPCStandingsTo returns a util.Rowset (fromID, standing) of every NPC
// entity's standing toward a character or corporation. standingsvc.py
// GetStandingEntries iterates it for the Liked by / Disliked by tabs, so it
// is never None.
func GetNPCStandingsTo(ctx context.Context, db *sql.DB, toID int64) (marshal.PyRep, error) {
	return queryRows(ctx, db,
		[]string{"fromID", "standing"},
		`SELECT fromID, standing FROM chrNPCStandings WHERE characterID = ?`, toID)
}

// standingTransactionCols is the row shape the transactions window and
// FmtStandingTransaction read.
var standingTransactionCols = []string{"eventID", "eventTypeID", "eventDateTime",
	"fromID", "toID", "modification", "originalFromID", "originalToID",
	"int_1", "int_2", "int_3", "msg"}

// GetStandingTransactions returns a util.Rowset of standing-change history for
// the client's (fromID, toID, direction, eventID, eventTypeID, eventDateTime)
// call. Nil pointers mean unfiltered; direction 1 pages backward, anything else
// forward. Pages are 25 rows: the client hides "next" on a short page.
func GetStandingTransactions(ctx context.Context, db *sql.DB, fromID, toID, direction int64, eventID, eventTypeID, eventDateTime *int64) (marshal.PyRep, error) {
	var where []string
	var args []interface{}
	if fromID != 0 {
		where = append(where, "fromID = ?")
		args = append(args, fromID)
	}
	if toID != 0 {
		where = append(where, "toID = ?")
		args = append(args, toID)
	}
	if eventTypeID != nil {
		where = append(where, "eventTypeID = ?")
		args = append(args, *eventTypeID)
	}
	back := direction == 1
	if eventID != nil {
		if back {
			where = append(where, "eventID < ?")
		} else {
			where = append(where, "eventID > ?")
		}
		args = append(args, *eventID)
	}
	if eventDateTime != nil {
		if back {
			where = append(where, "eventDateTime <= ?")
		} else {
			where = append(where, "eventDateTime >= ?")
		}
		args = append(args, *eventDateTime)
	}
	q := `SELECT eventID, eventTypeID, eventDateTime, fromID, toID, modification,
	             originalFromID, originalToID, int_1, int_2, int_3, msg
	        FROM chrStandingTransactions`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	if back {
		q += " ORDER BY eventID DESC LIMIT 25"
	} else {
		q += " ORDER BY eventID ASC LIMIT 25"
	}
	return queryRows(ctx, db, standingTransactionCols, q, args...)
}

// GetCharNPCStandingsWithConcord returns the NPC standings rowset plus a
// synthesized OwnerCONCORD row carrying character_.securityRating. The
// character sheet reads security status from that row
// (standingsvc.GetMySecurityRating).
func GetCharNPCStandingsWithConcord(ctx context.Context, db *sql.DB, charID int64) (marshal.PyRep, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT fromID, standing FROM chrNPCStandings WHERE characterID = ? AND fromID <> ?`,
		charID, OwnerCONCORD)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var lines [][]marshal.PyRep
	for rows.Next() {
		var fromID int64
		var standing float64
		if err := rows.Scan(&fromID, &standing); err != nil {
			continue
		}
		lines = append(lines, []marshal.PyRep{
			marshal.PyInt{Value: uint64(fromID)},
			marshal.PyReal{Value: standing},
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var secRating float64
	_ = db.QueryRowContext(ctx,
		`SELECT COALESCE(securityRating, 0) FROM character_ WHERE characterID = ?`,
		charID).Scan(&secRating)
	lines = append(lines, []marshal.PyRep{
		marshal.PyInt{Value: uint64(OwnerCONCORD)},
		marshal.PyReal{Value: secRating},
	})
	return makeRowset([]string{"fromID", "standing"}, lines), nil
}

// SetPersonalStanding sets a character's standing toward toID to an absolute
// value (Standing2Service::Handle_SetPlayerStanding). Zero deletes the row, as
// the client's standingsvc drops falsy standings. Corporations use
// SetCorpStanding.
func SetPersonalStanding(ctx context.Context, db *sql.DB, ownerID, toID int64, standing float64) error {
	if standing == 0 {
		_, err := db.ExecContext(ctx,
			`DELETE FROM chrStandings WHERE characterID = ? AND toID = ?`, ownerID, toID)
		return err
	}
	_, err := db.ExecContext(ctx,
		`INSERT INTO chrStandings (characterID, toID, standing) VALUES (?, ?, ?)
		 ON DUPLICATE KEY UPDATE standing = ?`,
		ownerID, toID, standing, standing)
	return err
}

// SetNPCStandingAbs sets fromID's standing toward a character to an absolute value in
// chrNPCStandings, the table the character sheet's Liked by / Disliked by tabs read. Zero
// deletes the row. UpsertNPCStanding is the additive form.
func SetNPCStandingAbs(ctx context.Context, db *sql.DB, charID, fromID int64, standing float64) error {
	if standing == 0 {
		_, err := db.ExecContext(ctx,
			`DELETE FROM chrNPCStandings WHERE characterID = ? AND fromID = ?`, charID, fromID)
		return err
	}
	_, err := db.ExecContext(ctx,
		`INSERT INTO chrNPCStandings (characterID, fromID, standing) VALUES (?, ?, ?)
		 ON DUPLICATE KEY UPDATE standing = ?`,
		charID, fromID, standing, standing)
	return err
}

// GetAgentMapStandings returns the NPC standings for the star map's agent
// overlay (map.py STARMODE_FRIENDS_AGENT): a util.Rowset (fromID, rank) with
// rank = standing / 10, which the client multiplies by 10 to display.
func GetAgentMapStandings(ctx context.Context, db *sql.DB, charID int64) (marshal.PyRep, error) {
	cols := []string{"fromID", "rank"}
	rows, err := db.QueryContext(ctx,
		`SELECT fromID, standing FROM chrNPCStandings WHERE characterID = ?`, charID)
	if err != nil {
		return makeRowset(cols, nil), err
	}
	defer rows.Close()
	var lines [][]marshal.PyRep
	for rows.Next() {
		var fromID int64
		var standing float64
		if rows.Scan(&fromID, &standing) != nil {
			continue
		}
		lines = append(lines, []marshal.PyRep{
			marshal.PyInt{Value: uint64(fromID)},
			marshal.PyReal{Value: standing / 10.0},
		})
	}
	return makeRowset(cols, lines), rows.Err()
}
