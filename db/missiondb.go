package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math"
	"math/rand"
	"regexp"
	"strconv"
	"strings"

	"gurimoon/rates"
)

// EnsureMissionTables creates mission system tables if they do not exist.
// Call once at server startup.
func EnsureMissionTables(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS agtDivisionMissionWeights (
			divisionID      INT NOT NULL,
			killWeight      INT NOT NULL DEFAULT 50,
			courierWeight   INT NOT NULL DEFAULT 50,
			tradeWeight     INT NOT NULL DEFAULT 0,
			miningWeight    INT NOT NULL DEFAULT 0,
			PRIMARY KEY (divisionID)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`)
	if err != nil {
		return fmt.Errorf("EnsureMissionTables agtDivisionMissionWeights: %w", err)
	}

	// Seed division weights if empty.
	var cnt int
	db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agtDivisionMissionWeights`).Scan(&cnt)
	if cnt == 0 {
		if _, err := db.ExecContext(ctx, `INSERT IGNORE INTO agtDivisionMissionWeights (divisionID, killWeight, courierWeight, tradeWeight, miningWeight) VALUES
			(1,  5,  90,  5,  0), (2,  50, 50,  0,  0), (3,  50, 50,  0,  0),
			(4,  5,  90,  5,  0), (5,  40, 30, 10, 20), (6,  97,  3,  0,  0),
			(7,  5,  90,  5,  0), (8,  5,  45, 50,  0), (9,  85, 15,  0,  0),
			(10, 95,  5,  0,  0), (11, 50, 50,  0,  0), (12, 10, 10, 10, 70),
			(13,  5, 45, 50,  0), (14,  5, 10,  5, 80), (15, 50, 50,  0,  0),
			(16, 10, 10, 10, 70), (17, 50, 50,  0,  0), (18, 50, 50,  0,  0),
			(19, 90,  5,  5,  0), (20,  5, 90,  5,  0), (21, 95,  5,  0,  0)`); err != nil {
			log.Printf("[db] EnsureMissionTables: best-effort write failed: %v", err)
		}
	}

	_, err = db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS missionTemplates (
			templateID      INT AUTO_INCREMENT PRIMARY KEY,
			missionName     VARCHAR(255) NOT NULL,
			missionType     ENUM('kill','courier','trade','mining') NOT NULL DEFAULT 'kill',
			agentLevel      INT NOT NULL DEFAULT 1,
			briefingText    TEXT NOT NULL,
			acceptText      TEXT DEFAULT NULL,
			completeText    TEXT DEFAULT NULL,
			declineText     TEXT DEFAULT NULL,
			dungeonVID      INT DEFAULT NULL,
			dungeonRoomIdx  INT DEFAULT 1,
			completionType  ENUM('killAll','killTarget','killStructure') DEFAULT 'killAll',
			targetTypeID    INT DEFAULT NULL,
			targetCount     INT DEFAULT 1,
			enemyFactionID  INT DEFAULT NULL,
			courierTypeID   INT DEFAULT NULL,
			courierQty      INT DEFAULT 1,
			pickupMode      ENUM('agent','specific','nearby') DEFAULT 'agent',
			pickupStationID INT DEFAULT NULL,
			dropoffMode     ENUM('agent','specific','nearby') DEFAULT 'nearby',
			dropoffStationID INT DEFAULT NULL,
			tradeTypeID     INT DEFAULT NULL,
			tradeQty        INT DEFAULT 1,
			miningTypeID    INT DEFAULT NULL,
			miningQty       INT DEFAULT 1,
			minSecurity     FLOAT DEFAULT NULL,
			maxSecurity     FLOAT DEFAULT NULL,
			baseRewardISK   BIGINT DEFAULT 0,
			baseRewardLP    INT DEFAULT 0,
			baseBonusISK    BIGINT DEFAULT 0,
			bonusTimeSec    INT DEFAULT 0,
			standingGain    FLOAT DEFAULT 0.1,
			corpStandingGain FLOAT DEFAULT 0.02,
			factionStandingGain FLOAT DEFAULT 0.01,
			standingLoss    FLOAT DEFAULT -0.05,
			factionStandingLoss FLOAT DEFAULT -0.02,
			enabled         TINYINT(1) DEFAULT 1
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`)
	if err != nil {
		return fmt.Errorf("EnsureMissionTables missionTemplates: %w", err)
	}

	// Add new columns to missionTemplates if they don't exist (idempotent).
	alterStmts := []string{
		`ALTER TABLE missionTemplates MODIFY completionType ENUM('killAll','killTarget','killStructure','killItem') DEFAULT 'killAll'`,
		`ALTER TABLE missionTemplates ADD COLUMN objectiveText TEXT DEFAULT NULL AFTER briefingText`,
		`ALTER TABLE missionTemplates ADD COLUMN killItemTypeID INT DEFAULT NULL AFTER targetCount`,
		`ALTER TABLE missionTemplates ADD COLUMN killItemQty INT DEFAULT 1 AFTER killItemTypeID`,
		`ALTER TABLE missionTemplates ADD COLUMN killItemDropoffMode ENUM('agent','specific','nearby') DEFAULT 'agent' AFTER killItemQty`,
		`ALTER TABLE missionTemplates ADD COLUMN killItemDropoffStationID INT DEFAULT NULL AFTER killItemDropoffMode`,
		`ALTER TABLE missionTemplates ADD COLUMN baseRewardItemTypeID INT DEFAULT NULL AFTER baseBonusISK`,
		`ALTER TABLE missionTemplates ADD COLUMN baseRewardItemQty INT DEFAULT 1 AFTER baseRewardItemTypeID`,
		`ALTER TABLE missionTemplates ADD COLUMN bonusRewardItemTypeID INT DEFAULT NULL AFTER baseRewardItemQty`,
		`ALTER TABLE missionTemplates ADD COLUMN bonusRewardItemQty INT DEFAULT 1 AFTER bonusRewardItemTypeID`,
		`ALTER TABLE missionTemplates ADD COLUMN allowedFactions VARCHAR(255) DEFAULT NULL AFTER enabled`,
		`ALTER TABLE missionTemplates ADD COLUMN nextMissionID INT DEFAULT NULL AFTER allowedFactions`,
		// Faction standing is authored apart from corp standing: the agent's faction gains
		// factionStandingGain, and enemyFactionID takes factionStandingLoss (stored negative).
		`ALTER TABLE missionTemplates ADD COLUMN factionStandingGain FLOAT DEFAULT 0.01 AFTER corpStandingGain`,
		`ALTER TABLE missionTemplates ADD COLUMN factionStandingLoss FLOAT DEFAULT -0.02 AFTER standingLoss`,
		// Agent conversation "More Information" button text; empty = no button.
		`ALTER TABLE missionTemplates ADD COLUMN moreInfoText TEXT DEFAULT NULL AFTER declineText`,
		// Conversation button captions; empty or NULL falls back to the generic caption.
		`ALTER TABLE missionTemplates ADD COLUMN acceptLabel VARCHAR(64) DEFAULT NULL AFTER moreInfoText`,
		`ALTER TABLE missionTemplates ADD COLUMN declineLabel VARCHAR(64) DEFAULT NULL AFTER acceptLabel`,
		`ALTER TABLE missionTemplates ADD COLUMN viewMissionLabel VARCHAR(64) DEFAULT NULL AFTER declineLabel`,
		`ALTER TABLE missionTemplates ADD COLUMN moreInfoLabel VARCHAR(64) DEFAULT NULL AFTER viewMissionLabel`,
		`ALTER TABLE missionTemplates ADD COLUMN completeLabel VARCHAR(64) DEFAULT NULL AFTER moreInfoLabel`,
		`ALTER TABLE missionTemplates ADD COLUMN quitMissionLabel VARCHAR(64) DEFAULT NULL AFTER completeLabel`,
		// The five standing fields are DOUBLE: FLOAT is float32 and reads an authored 0.01 back
		// as 0.0099999. See docs/mission-notes.md#standing-fields.
		`ALTER TABLE missionTemplates MODIFY standingGain DOUBLE DEFAULT 0`,
		`ALTER TABLE missionTemplates MODIFY corpStandingGain DOUBLE DEFAULT 0`,
		`ALTER TABLE missionTemplates MODIFY factionStandingGain DOUBLE DEFAULT 0`,
		`ALTER TABLE missionTemplates MODIFY standingLoss DOUBLE DEFAULT 0`,
		`ALTER TABLE missionTemplates MODIFY factionStandingLoss DOUBLE DEFAULT 0`,
		// The storyline deck: a storyline template is drawn only by storyline agents, so this
		// flag splits the template pool into two decks.
		`ALTER TABLE missionTemplates ADD COLUMN storyline TINYINT(1) NOT NULL DEFAULT 0 AFTER agentLevel`,
		// An accepted mission not handed in within expiryDays fails; the agent mails failText
		// (empty = the generic letter).
		`ALTER TABLE missionTemplates ADD COLUMN expiryDays INT NOT NULL DEFAULT 7 AFTER bonusTimeSec`,
		`ALTER TABLE missionTemplates ADD COLUMN failText TEXT DEFAULT NULL AFTER completeText`,
	}
	for _, stmt := range alterStmts {
		if missionModifyDone(ctx, db, stmt) {
			continue
		}
		db.ExecContext(ctx, stmt) // ignore errors (column may already exist)
	}

	// Widening FLOAT to DOUBLE keeps the float32 artifact, so round the stored values once.
	// The WHERE clause makes this a no-op on later boots.
	if _, err := db.ExecContext(ctx, `
		UPDATE missionTemplates SET
		    standingGain        = ROUND(standingGain,4),
		    corpStandingGain    = ROUND(corpStandingGain,4),
		    factionStandingGain = ROUND(factionStandingGain,4),
		    standingLoss        = ROUND(standingLoss,4),
		    factionStandingLoss = ROUND(factionStandingLoss,4)
		 WHERE standingGain        <> ROUND(standingGain,4)
		    OR corpStandingGain    <> ROUND(corpStandingGain,4)
		    OR factionStandingGain <> ROUND(factionStandingGain,4)
		    OR standingLoss        <> ROUND(standingLoss,4)
		    OR factionStandingLoss <> ROUND(factionStandingLoss,4)`); err != nil {
		log.Printf("[db] EnsureMissionTables: best-effort write failed: %v", err)
	}

	_, err = db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS characterMissions (
			missionID       BIGINT AUTO_INCREMENT PRIMARY KEY,
			charID          INT NOT NULL,
			agentID         INT NOT NULL,
			templateID      INT NOT NULL,
			state           ENUM('offered','accepted','completed','failed','declined') NOT NULL DEFAULT 'offered',
			destSystemID    INT DEFAULT NULL,
			destStationID   INT DEFAULT NULL,
			pickupStationID INT DEFAULT NULL,
			dropoffStationID INT DEFAULT NULL,
			bookmarkX       DOUBLE DEFAULT NULL,
			bookmarkY       DOUBLE DEFAULT NULL,
			bookmarkZ       DOUBLE DEFAULT NULL,
			rewardISK       BIGINT DEFAULT 0,
			rewardLP        INT DEFAULT 0,
			bonusISK        BIGINT DEFAULT 0,
			killProgress    INT DEFAULT 0,
			killRequired    INT DEFAULT 0,
			acceptedAt      DATETIME DEFAULT NULL,
			expiresAt       DATETIME DEFAULT NULL,
			completedAt     DATETIME DEFAULT NULL,
			bonusDeadline   DATETIME DEFAULT NULL,
			INDEX idx_char_agent (charID, agentID),
			INDEX idx_char_state (charID, state)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`)
	if err != nil {
		return fmt.Errorf("EnsureMissionTables characterMissions: %w", err)
	}

	// objectiveDone is the durable record that a kill objective was met: a fresh run resets the
	// kill tally and respawns the site, and must still know the objective is finished.
	for _, stmt := range []string{
		`ALTER TABLE characterMissions ADD COLUMN objectiveDone TINYINT(1) NOT NULL DEFAULT 0 AFTER killRequired`,
		// declinedAt drives the free-decline window (RecentAgentDecline, agentservicesdb.go).
		`ALTER TABLE characterMissions ADD COLUMN declinedAt DATETIME DEFAULT NULL AFTER completedAt`,
	} {
		db.ExecContext(ctx, stmt) // ignore errors (column may already exist)
	}

	_, err = db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS agentLoyaltyPoints (
			charID      INT NOT NULL,
			agentID     INT NOT NULL,
			points      INT NOT NULL DEFAULT 0,
			PRIMARY KEY (charID, agentID)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`)
	if err != nil {
		return fmt.Errorf("EnsureMissionTables agentLoyaltyPoints: %w", err)
	}

	// Agent LP-store special offers (journal Offers tab), authored via ESP. The static dump's own
	// agtOffers has other columns and would be kept by CREATE IF NOT EXISTS, so it is moved aside once.
	if err := retireStaticAgtOffers(ctx, db); err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS agtOffers (
			offerID       INT AUTO_INCREMENT PRIMARY KEY,
			offerName     VARCHAR(255) NOT NULL,
			description   TEXT,
			agentID       INT DEFAULT NULL,
			corporationID INT DEFAULT NULL,
			lpCost        INT NOT NULL DEFAULT 0,
			iskCost       BIGINT NOT NULL DEFAULT 0,
			minStanding   FLOAT NOT NULL DEFAULT 0,
			expiresDays   INT NOT NULL DEFAULT 0,
			enabled       TINYINT(1) NOT NULL DEFAULT 1
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`)
	if err != nil {
		return fmt.Errorf("EnsureMissionTables agtOffers: %w", err)
	}
	// Tables created before the description column existed lack it; the CREATE
	// above is a no-op for them. Ignore the duplicate-column error otherwise.
	db.ExecContext(ctx, `ALTER TABLE agtOffers ADD COLUMN description TEXT AFTER offerName`)
	// Availability is two comma-separated allow-lists (FIND_IN_SET), empty = no restriction.
	// agentID no longer scopes an offer; the column stays for legacy rows.
	db.ExecContext(ctx, `ALTER TABLE agtOffers ADD COLUMN allowedFactions VARCHAR(255) DEFAULT NULL AFTER corporationID`)
	db.ExecContext(ctx, `ALTER TABLE agtOffers ADD COLUMN allowedDivisions VARCHAR(255) DEFAULT NULL AFTER allowedFactions`)
	_, err = db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS agtOfferItems (
			entryID   INT AUTO_INCREMENT PRIMARY KEY,
			offerID   INT NOT NULL,
			role      ENUM('reward','cost') NOT NULL DEFAULT 'reward',
			typeID    INT NOT NULL,
			quantity  INT NOT NULL DEFAULT 1,
			extraInfo VARCHAR(255) DEFAULT NULL,
			INDEX idx_offer (offerID)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`)
	if err != nil {
		return fmt.Errorf("EnsureMissionTables agtOfferItems: %w", err)
	}

	// Persistent mission dungeon instances: which accepted kill missions have dungeons and
	// where, kept across restarts so the dungeon respawns when the system loads.
	_, err = db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS missionDungeonInstances (
			missionID     INT NOT NULL PRIMARY KEY,
			charID        INT NOT NULL,
			solarSystemID INT NOT NULL,
			dungeonVID    INT NOT NULL,
			roomID        INT NOT NULL,
			anchorX       DOUBLE NOT NULL DEFAULT 0,
			anchorY       DOUBLE NOT NULL DEFAULT 0,
			anchorZ       DOUBLE NOT NULL DEFAULT 0,
			spawned       TINYINT(1) NOT NULL DEFAULT 0,
			createdAt     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			INDEX idx_system (solarSystemID),
			INDEX idx_char (charID)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`)
	if err != nil {
		return fmt.Errorf("EnsureMissionTables missionDungeonInstances: %w", err)
	}

	// Per-NPC kill state and wave release state for mission dungeons.
	// Lets site progress survive server restarts.
	_, err = db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS missionDungeonSiteState (
			missionID   INT NOT NULL,
			objectID    BIGINT NOT NULL,
			killed      TINYINT(1) NOT NULL DEFAULT 0,
			PRIMARY KEY (missionID, objectID),
			INDEX idx_mission (missionID)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`)
	if err != nil {
		return fmt.Errorf("EnsureMissionTables missionDungeonSiteState: %w", err)
	}
	_, err = db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS missionDungeonWaveState (
			missionID   INT NOT NULL,
			roomID      INT NOT NULL,
			waveGroup   INT NOT NULL,
			released    TINYINT(1) NOT NULL DEFAULT 1,
			PRIMARY KEY (missionID, roomID, waveGroup)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`)
	if err != nil {
		return fmt.Errorf("EnsureMissionTables missionDungeonWaveState: %w", err)
	}

	return nil
}

// ── Mission template types ──────────────────────────────────────────────────

// MissionTemplate is a mission definition authored via ESP.
type MissionTemplate struct {
	TemplateID  int64
	MissionName string
	MissionType string // kill, courier, trade, mining
	AgentLevel  int
	// Storyline templates are the storyline agents' exclusive deck.
	Storyline     bool
	BriefingText  string
	ObjectiveText string
	AcceptText    string
	CompleteText  string
	// FailText is the body of the letter the agent sends when the mission
	// expires. Empty = the generic letter.
	FailText     string
	DeclineText  string
	MoreInfoText string
	// Conversation button captions. Empty = the generic caption, so an
	// unauthored template still reads "Accept" / "Decline" / …
	AcceptLabel              string
	DeclineLabel             string
	ViewMissionLabel         string
	MoreInfoLabel            string
	CompleteLabel            string
	QuitMissionLabel         string
	DungeonVID               int64
	DungeonRoomIdx           int
	CompletionType           string // killAll, killTarget, killStructure, killItem
	TargetTypeID             int64
	TargetCount              int
	KillItemTypeID           int64
	KillItemQty              int
	KillItemDropoffMode      string // agent, specific, nearby
	KillItemDropoffStationID int64
	EnemyFactionID           int64
	CourierTypeID            int64
	CourierQty               int
	PickupMode               string // agent, specific, nearby
	PickupStationID          int64
	DropoffMode              string // agent, specific, nearby
	DropoffStationID         int64
	TradeTypeID              int64
	TradeQty                 int
	MiningTypeID             int64
	MiningQty                int
	MinSecurity              sql.NullFloat64
	MaxSecurity              sql.NullFloat64
	BaseRewardISK            int64
	BaseRewardLP             int
	BaseBonusISK             int64
	BaseRewardItemTypeID     int64
	BaseRewardItemQty        int
	BonusRewardItemTypeID    int64
	BonusRewardItemQty       int
	BonusTimeSec             int
	// ExpiryDays is how long an accepted mission has before it fails.
	ExpiryDays       int
	StandingGain     float64
	CorpStandingGain float64
	// FactionStandingGain is applied to the agent's faction on completion.
	FactionStandingGain float64
	StandingLoss        float64
	// FactionStandingLoss is applied to EnemyFactionID on completion (the hit the briefing
	// warns about). Negative.
	FactionStandingLoss float64
	AllowedFactions     string // comma-separated faction IDs, empty = all
	NextMissionID       int64
	Enabled             bool
}

// CharacterMission is an active or completed mission.
type CharacterMission struct {
	MissionID        int64
	CharID           int64
	AgentID          int64
	TemplateID       int64
	State            string // offered, accepted, completed, failed, declined
	DestSystemID     int64
	DestStationID    int64
	PickupStationID  int64
	DropoffStationID int64
	BookmarkX        float64
	BookmarkY        float64
	BookmarkZ        float64
	RewardISK        int64
	RewardLP         int
	BonusISK         int64
	KillProgress     int
	KillRequired     int
	ObjectiveDone    bool
	AcceptedAt       sql.NullTime
	ExpiresAt        sql.NullTime
	CompletedAt      sql.NullTime
	BonusDeadline    sql.NullTime
}

// AgentInfo holds agent details needed for mission generation.
type AgentInfo struct {
	AgentID       int64
	DivisionID    int64
	Level         int
	Quality       int
	AgentTypeID   int64
	CorporationID int64
	StationID     int64
	SolarSystemID int64
}

// AgentTypeResearch is agtAgentTypes.agentTypeID for R&D agents.
const AgentTypeResearch = 4

// DivisionWeights holds the mission type distribution for a division.
type DivisionWeights struct {
	DivisionID    int64
	KillWeight    int
	CourierWeight int
	TradeWeight   int
	MiningWeight  int
}

// ── Agent info ──────────────────────────────────────────────────────────────

// GetAgentInfo loads agent details needed for mission generation.
func GetAgentInfo(ctx context.Context, db *sql.DB, agentID int64) (*AgentInfo, error) {
	a := &AgentInfo{}
	err := db.QueryRowContext(ctx, `
		SELECT a.agentID, COALESCE(a.divisionID, 0), COALESCE(a.level, 1),
		       COALESCE(a.quality, 0), COALESCE(a.agentTypeID, 2), COALESCE(a.corporationID, 0),
		       COALESCE(a.stationID, 0), COALESCE(s.solarSystemID, 0)
		FROM agtAgents a
		LEFT JOIN staStations s ON s.stationID = a.stationID
		WHERE a.agentID = ?`, agentID,
	).Scan(&a.AgentID, &a.DivisionID, &a.Level, &a.Quality, &a.AgentTypeID,
		&a.CorporationID, &a.StationID, &a.SolarSystemID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("agent %d not found", agentID)
		}
		return nil, fmt.Errorf("GetAgentInfo: %w", err)
	}
	return a, nil
}

// GetAgentCorpFactionID returns the factionID of an NPC corporation from crpNPCCorporations,
// the map the client's faction service uses, so server standing checks agree with the client.
// chrFactions.corporationID is only each faction's flagship corp and does not work. A
// factionID passed directly is accepted.
func GetAgentCorpFactionID(ctx context.Context, db rowQueryer, corpID int64) (int64, error) {
	var factionID sql.NullInt64
	err := db.QueryRowContext(ctx,
		`SELECT COALESCE(
			(SELECT factionID FROM crpNPCCorporations WHERE corporationID = ?),
			(SELECT ownerID FROM eveStaticOwners WHERE ownerID = ? AND typeID = 30)
		)`, corpID, corpID).Scan(&factionID)
	if err != nil || !factionID.Valid {
		return 0, nil
	}
	return factionID.Int64, nil
}

// ── Division weights ────────────────────────────────────────────────────────

// GetDivisionWeights returns the mission type weights for a division.
func GetDivisionWeights(ctx context.Context, db *sql.DB, divisionID int64) (*DivisionWeights, error) {
	w := &DivisionWeights{}
	err := db.QueryRowContext(ctx, `
		SELECT divisionID, killWeight, courierWeight, tradeWeight, miningWeight
		FROM agtDivisionMissionWeights WHERE divisionID = ?`, divisionID,
	).Scan(&w.DivisionID, &w.KillWeight, &w.CourierWeight, &w.TradeWeight, &w.MiningWeight)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Default 50/50 kill/courier
			return &DivisionWeights{DivisionID: divisionID, KillWeight: 50, CourierWeight: 50}, nil
		}
		return nil, fmt.Errorf("GetDivisionWeights: %w", err)
	}
	return w, nil
}

// RollMissionType picks a random mission type based on division weights.
func RollMissionType(w *DivisionWeights) string {
	if w == nil {
		return "kill"
	}
	total := w.KillWeight + w.CourierWeight + w.TradeWeight + w.MiningWeight
	if total <= 0 {
		return "kill"
	}
	roll := rand.Intn(total)
	if roll < w.KillWeight {
		return "kill"
	}
	roll -= w.KillWeight
	if roll < w.CourierWeight {
		return "courier"
	}
	roll -= w.CourierWeight
	if roll < w.TradeWeight {
		return "trade"
	}
	return "mining"
}

// ── Mission template queries ────────────────────────────────────────────────

// GetRandomTemplate picks a random enabled template matching type and level.
// agentFactionID filters by allowedFactions (0 = no filter). storyline selects the deck by
// equality: storyline agents draw only storyline templates, everyone else only regular ones.
func GetRandomTemplate(ctx context.Context, db *sql.DB, missionType string, agentLevel int, agentFactionID int64, storyline bool) (*MissionTemplate, error) {
	t := &MissionTemplate{}
	query := `
		SELECT templateID, missionName, missionType, agentLevel,
		       COALESCE(storyline,0),
		       briefingText, COALESCE(objectiveText,''),
		       COALESCE(acceptText,''), COALESCE(completeText,''), COALESCE(failText,''), COALESCE(declineText,''),
		       COALESCE(moreInfoText,''),
		       COALESCE(acceptLabel,''), COALESCE(declineLabel,''),
		       COALESCE(viewMissionLabel,''), COALESCE(moreInfoLabel,''),
		       COALESCE(completeLabel,''), COALESCE(quitMissionLabel,''),
		       COALESCE(dungeonVID,0), COALESCE(dungeonRoomIdx,1),
		       COALESCE(completionType,'killAll'), COALESCE(targetTypeID,0), COALESCE(targetCount,1),
		       COALESCE(killItemTypeID,0), COALESCE(killItemQty,1),
		       COALESCE(killItemDropoffMode,'agent'), COALESCE(killItemDropoffStationID,0),
		       COALESCE(enemyFactionID,0),
		       COALESCE(courierTypeID,0), COALESCE(courierQty,1),
		       COALESCE(pickupMode,'agent'), COALESCE(pickupStationID,0),
		       COALESCE(dropoffMode,'nearby'), COALESCE(dropoffStationID,0),
		       COALESCE(tradeTypeID,0), COALESCE(tradeQty,1),
		       COALESCE(miningTypeID,0), COALESCE(miningQty,1),
		       minSecurity, maxSecurity,
		       COALESCE(baseRewardISK,0), COALESCE(baseRewardLP,0),
		       COALESCE(baseBonusISK,0),
		       COALESCE(baseRewardItemTypeID,0), COALESCE(baseRewardItemQty,1),
		       COALESCE(bonusRewardItemTypeID,0), COALESCE(bonusRewardItemQty,1),
		       COALESCE(bonusTimeSec,0), COALESCE(expiryDays,7),
		       COALESCE(standingGain,0), COALESCE(corpStandingGain,0),
		       COALESCE(factionStandingGain,0),
		       COALESCE(standingLoss,0), COALESCE(factionStandingLoss,0),
		       COALESCE(allowedFactions,''), COALESCE(nextMissionID,0),
		       COALESCE(enabled,1)
		FROM missionTemplates
		WHERE missionType = ? AND agentLevel = ? AND enabled = 1
		  AND COALESCE(storyline,0) = ?`

	var args []any
	args = append(args, missionType, agentLevel, boolToInt(storyline))
	if agentFactionID > 0 {
		query += ` AND (allowedFactions IS NULL OR allowedFactions = '' OR FIND_IN_SET(?, allowedFactions) > 0)`
		args = append(args, agentFactionID)
	}
	query += ` ORDER BY RAND() LIMIT 1`

	err := db.QueryRowContext(ctx, query, args...).Scan(
		&t.TemplateID, &t.MissionName, &t.MissionType, &t.AgentLevel,
		&t.Storyline,
		&t.BriefingText, &t.ObjectiveText,
		&t.AcceptText, &t.CompleteText, &t.FailText, &t.DeclineText,
		&t.MoreInfoText,
		&t.AcceptLabel, &t.DeclineLabel,
		&t.ViewMissionLabel, &t.MoreInfoLabel,
		&t.CompleteLabel, &t.QuitMissionLabel,
		&t.DungeonVID, &t.DungeonRoomIdx,
		&t.CompletionType, &t.TargetTypeID, &t.TargetCount,
		&t.KillItemTypeID, &t.KillItemQty,
		&t.KillItemDropoffMode, &t.KillItemDropoffStationID,
		&t.EnemyFactionID,
		&t.CourierTypeID, &t.CourierQty,
		&t.PickupMode, &t.PickupStationID,
		&t.DropoffMode, &t.DropoffStationID,
		&t.TradeTypeID, &t.TradeQty,
		&t.MiningTypeID, &t.MiningQty,
		&t.MinSecurity, &t.MaxSecurity,
		&t.BaseRewardISK, &t.BaseRewardLP,
		&t.BaseBonusISK,
		&t.BaseRewardItemTypeID, &t.BaseRewardItemQty,
		&t.BonusRewardItemTypeID, &t.BonusRewardItemQty,
		&t.BonusTimeSec, &t.ExpiryDays,
		&t.StandingGain, &t.CorpStandingGain,
		&t.FactionStandingGain,
		&t.StandingLoss, &t.FactionStandingLoss,
		&t.AllowedFactions, &t.NextMissionID,
		&t.Enabled)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetRandomTemplate: %w", err)
	}
	return t, nil
}

// GetTemplateByID loads a specific mission template.
func GetTemplateByID(ctx context.Context, db *sql.DB, templateID int64) (*MissionTemplate, error) {
	t := &MissionTemplate{}
	err := db.QueryRowContext(ctx, `
		SELECT templateID, missionName, missionType, agentLevel,
		       COALESCE(storyline,0),
		       briefingText, COALESCE(objectiveText,''),
		       COALESCE(acceptText,''), COALESCE(completeText,''), COALESCE(failText,''), COALESCE(declineText,''),
		       COALESCE(moreInfoText,''),
		       COALESCE(acceptLabel,''), COALESCE(declineLabel,''),
		       COALESCE(viewMissionLabel,''), COALESCE(moreInfoLabel,''),
		       COALESCE(completeLabel,''), COALESCE(quitMissionLabel,''),
		       COALESCE(dungeonVID,0), COALESCE(dungeonRoomIdx,1),
		       COALESCE(completionType,'killAll'), COALESCE(targetTypeID,0), COALESCE(targetCount,1),
		       COALESCE(killItemTypeID,0), COALESCE(killItemQty,1),
		       COALESCE(killItemDropoffMode,'agent'), COALESCE(killItemDropoffStationID,0),
		       COALESCE(enemyFactionID,0),
		       COALESCE(courierTypeID,0), COALESCE(courierQty,1),
		       COALESCE(pickupMode,'agent'), COALESCE(pickupStationID,0),
		       COALESCE(dropoffMode,'nearby'), COALESCE(dropoffStationID,0),
		       COALESCE(tradeTypeID,0), COALESCE(tradeQty,1),
		       COALESCE(miningTypeID,0), COALESCE(miningQty,1),
		       minSecurity, maxSecurity,
		       COALESCE(baseRewardISK,0), COALESCE(baseRewardLP,0),
		       COALESCE(baseBonusISK,0),
		       COALESCE(baseRewardItemTypeID,0), COALESCE(baseRewardItemQty,1),
		       COALESCE(bonusRewardItemTypeID,0), COALESCE(bonusRewardItemQty,1),
		       COALESCE(bonusTimeSec,0), COALESCE(expiryDays,7),
		       COALESCE(standingGain,0), COALESCE(corpStandingGain,0),
		       COALESCE(factionStandingGain,0),
		       COALESCE(standingLoss,0), COALESCE(factionStandingLoss,0),
		       COALESCE(allowedFactions,''), COALESCE(nextMissionID,0),
		       COALESCE(enabled,1)
		FROM missionTemplates WHERE templateID = ?`, templateID,
	).Scan(&t.TemplateID, &t.MissionName, &t.MissionType, &t.AgentLevel,
		&t.Storyline,
		&t.BriefingText, &t.ObjectiveText,
		&t.AcceptText, &t.CompleteText, &t.FailText, &t.DeclineText,
		&t.MoreInfoText,
		&t.AcceptLabel, &t.DeclineLabel,
		&t.ViewMissionLabel, &t.MoreInfoLabel,
		&t.CompleteLabel, &t.QuitMissionLabel,
		&t.DungeonVID, &t.DungeonRoomIdx,
		&t.CompletionType, &t.TargetTypeID, &t.TargetCount,
		&t.KillItemTypeID, &t.KillItemQty,
		&t.KillItemDropoffMode, &t.KillItemDropoffStationID,
		&t.EnemyFactionID,
		&t.CourierTypeID, &t.CourierQty,
		&t.PickupMode, &t.PickupStationID,
		&t.DropoffMode, &t.DropoffStationID,
		&t.TradeTypeID, &t.TradeQty,
		&t.MiningTypeID, &t.MiningQty,
		&t.MinSecurity, &t.MaxSecurity,
		&t.BaseRewardISK, &t.BaseRewardLP,
		&t.BaseBonusISK,
		&t.BaseRewardItemTypeID, &t.BaseRewardItemQty,
		&t.BonusRewardItemTypeID, &t.BonusRewardItemQty,
		&t.BonusTimeSec, &t.ExpiryDays,
		&t.StandingGain, &t.CorpStandingGain,
		&t.FactionStandingGain,
		&t.StandingLoss, &t.FactionStandingLoss,
		&t.AllowedFactions, &t.NextMissionID,
		&t.Enabled)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetTemplateByID: %w", err)
	}
	return t, nil
}

// ── Character mission CRUD ──────────────────────────────────────────────────

// GetActiveMission returns the active (offered or accepted) mission for
// a character with a specific agent. Returns nil if none.
func GetActiveMission(ctx context.Context, db *sql.DB, charID, agentID int64) (*CharacterMission, error) {
	m := &CharacterMission{}
	err := db.QueryRowContext(ctx, `
		SELECT missionID, charID, agentID, templateID, state,
		       COALESCE(destSystemID,0), COALESCE(destStationID,0),
		       COALESCE(pickupStationID,0), COALESCE(dropoffStationID,0),
		       COALESCE(bookmarkX,0), COALESCE(bookmarkY,0), COALESCE(bookmarkZ,0),
		       COALESCE(rewardISK,0), COALESCE(rewardLP,0), COALESCE(bonusISK,0),
		       COALESCE(killProgress,0), COALESCE(killRequired,0),
		       COALESCE(objectiveDone,0),
		       acceptedAt, expiresAt, completedAt, bonusDeadline
		FROM characterMissions
		WHERE charID = ? AND agentID = ? AND state IN ('offered','accepted')
		LIMIT 1`, charID, agentID,
	).Scan(&m.MissionID, &m.CharID, &m.AgentID, &m.TemplateID, &m.State,
		&m.DestSystemID, &m.DestStationID,
		&m.PickupStationID, &m.DropoffStationID,
		&m.BookmarkX, &m.BookmarkY, &m.BookmarkZ,
		&m.RewardISK, &m.RewardLP, &m.BonusISK,
		&m.KillProgress, &m.KillRequired, &m.ObjectiveDone,
		&m.AcceptedAt, &m.ExpiresAt, &m.CompletedAt, &m.BonusDeadline)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetActiveMission: %w", err)
	}
	return m, nil
}

// GetMissionByID loads a specific character mission.
func GetMissionByID(ctx context.Context, db *sql.DB, missionID int64) (*CharacterMission, error) {
	m := &CharacterMission{}
	err := db.QueryRowContext(ctx, `
		SELECT missionID, charID, agentID, templateID, state,
		       COALESCE(destSystemID,0), COALESCE(destStationID,0),
		       COALESCE(pickupStationID,0), COALESCE(dropoffStationID,0),
		       COALESCE(bookmarkX,0), COALESCE(bookmarkY,0), COALESCE(bookmarkZ,0),
		       COALESCE(rewardISK,0), COALESCE(rewardLP,0), COALESCE(bonusISK,0),
		       COALESCE(killProgress,0), COALESCE(killRequired,0),
		       COALESCE(objectiveDone,0),
		       acceptedAt, expiresAt, completedAt, bonusDeadline
		FROM characterMissions WHERE missionID = ?`, missionID,
	).Scan(&m.MissionID, &m.CharID, &m.AgentID, &m.TemplateID, &m.State,
		&m.DestSystemID, &m.DestStationID,
		&m.PickupStationID, &m.DropoffStationID,
		&m.BookmarkX, &m.BookmarkY, &m.BookmarkZ,
		&m.RewardISK, &m.RewardLP, &m.BonusISK,
		&m.KillProgress, &m.KillRequired, &m.ObjectiveDone,
		&m.AcceptedAt, &m.ExpiresAt, &m.CompletedAt, &m.BonusDeadline)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetMissionByID: %w", err)
	}
	return m, nil
}

// CreateMissionOffer inserts a new character mission in 'offered' state.
func CreateMissionOffer(ctx context.Context, db *sql.DB, charID, agentID, templateID int64,
	destSystemID, destStationID int64,
	pickupStationID, dropoffStationID int64,
	bookmarkX, bookmarkY, bookmarkZ float64,
	rewardISK, bonusISK int64, rewardLP int,
	killRequired int,
) (int64, error) {
	res, err := db.ExecContext(ctx, `
		INSERT INTO characterMissions
		  (charID, agentID, templateID, state,
		   destSystemID, destStationID, pickupStationID, dropoffStationID,
		   bookmarkX, bookmarkY, bookmarkZ,
		   rewardISK, rewardLP, bonusISK, killRequired)
		VALUES (?, ?, ?, 'offered',
		        ?, ?, ?, ?,
		        ?, ?, ?,
		        ?, ?, ?, ?)`,
		charID, agentID, templateID,
		destSystemID, destStationID, pickupStationID, dropoffStationID,
		bookmarkX, bookmarkY, bookmarkZ,
		rewardISK, rewardLP, bonusISK, killRequired)
	if err != nil {
		return 0, fmt.Errorf("CreateMissionOffer: %w", err)
	}
	return res.LastInsertId()
}

// DefaultMissionExpiryDays is the expiry of a template saved without one.
// server_settings missions.defaultExpiryDays overrides it (MissionDefaultExpiryDays).
const DefaultMissionExpiryDays = 7

// MissionDefaultExpiryDays is the live default expiry, in days.
func MissionDefaultExpiryDays() int {
	return int(CachedSettingFloat("missions.defaultExpiryDays", DefaultMissionExpiryDays, 1, 365))
}

// ClampMissionExpiryDays keeps an authored expiry at one day or more. There is
// no "never": the journal's countdown column needs a real future date.
func ClampMissionExpiryDays(days int) int {
	if days < 1 {
		return MissionDefaultExpiryDays()
	}
	return days
}

// AcceptMission transitions a mission from offered to accepted. expiryDays is
// the template's; expiresAt is what the journal counts down to and what the
// expiry sweep fails the mission on.
func AcceptMission(ctx context.Context, db *sql.DB, missionID int64, bonusTimeSec, expiryDays int) error {
	bonusExpr := "NULL"
	if bonusTimeSec > 0 {
		bonusExpr = fmt.Sprintf("DATE_ADD(NOW(), INTERVAL %d SECOND)", bonusTimeSec)
	}
	_, err := db.ExecContext(ctx, fmt.Sprintf(`
		UPDATE characterMissions
		SET state = 'accepted', acceptedAt = NOW(),
		    expiresAt = DATE_ADD(NOW(), INTERVAL %d DAY),
		    bonusDeadline = %s
		WHERE missionID = ? AND state = 'offered'`, ClampMissionExpiryDays(expiryDays), bonusExpr), missionID)
	if err != nil {
		return fmt.Errorf("AcceptMission: %w", err)
	}
	return nil
}

// RestampMissionExpiry recomputes expiresAt = acceptedAt + the template's expiryDays for
// accepted missions of templateID (0 = every template). Run on a template save, so an edit
// reaches missions in flight, and at boot for rows accepted without an expiresAt.
func RestampMissionExpiry(ctx context.Context, db *sql.DB, templateID int64) (int64, error) {
	res, err := db.ExecContext(ctx, `
		UPDATE characterMissions cm
		JOIN missionTemplates t ON t.templateID = cm.templateID
		SET cm.expiresAt = DATE_ADD(COALESCE(cm.acceptedAt, NOW()),
		                            INTERVAL IF(t.expiryDays < 1, ?, t.expiryDays) DAY)
		WHERE cm.state = 'accepted' AND (? = 0 OR cm.templateID = ?)`,
		MissionDefaultExpiryDays(), templateID, templateID)
	if err != nil {
		return 0, fmt.Errorf("RestampMissionExpiry: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// DueExpiredMissions returns accepted missions past their expiresAt.
func DueExpiredMissions(ctx context.Context, db *sql.DB) ([]*CharacterMission, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT missionID, charID, agentID, templateID, state,
		       COALESCE(destSystemID,0), COALESCE(destStationID,0),
		       COALESCE(pickupStationID,0), COALESCE(dropoffStationID,0),
		       COALESCE(bookmarkX,0), COALESCE(bookmarkY,0), COALESCE(bookmarkZ,0),
		       COALESCE(rewardISK,0), COALESCE(rewardLP,0), COALESCE(bonusISK,0),
		       COALESCE(killProgress,0), COALESCE(killRequired,0),
		       COALESCE(objectiveDone,0),
		       acceptedAt, expiresAt, completedAt, bonusDeadline
		FROM characterMissions
		WHERE state = 'accepted' AND expiresAt IS NOT NULL AND expiresAt <= NOW()
		ORDER BY missionID`)
	if err != nil {
		return nil, fmt.Errorf("DueExpiredMissions: %w", err)
	}
	defer rows.Close()
	var result []*CharacterMission
	for rows.Next() {
		m := &CharacterMission{}
		if err := rows.Scan(&m.MissionID, &m.CharID, &m.AgentID, &m.TemplateID, &m.State,
			&m.DestSystemID, &m.DestStationID,
			&m.PickupStationID, &m.DropoffStationID,
			&m.BookmarkX, &m.BookmarkY, &m.BookmarkZ,
			&m.RewardISK, &m.RewardLP, &m.BonusISK,
			&m.KillProgress, &m.KillRequired, &m.ObjectiveDone,
			&m.AcceptedAt, &m.ExpiresAt, &m.CompletedAt, &m.BonusDeadline); err != nil {
			return nil, fmt.Errorf("DueExpiredMissions scan: %w", err)
		}
		result = append(result, m)
	}
	return result, rows.Err()
}

// ExpireMission fails an accepted mission. It reports whether this call made
// the change, so a mission handed in (or quit) in the same instant is never
// failed and mailed on top of it.
func ExpireMission(ctx context.Context, db *sql.DB, missionID int64) (bool, error) {
	res, err := db.ExecContext(ctx,
		`UPDATE characterMissions SET state = 'failed', completedAt = NOW()
		  WHERE missionID = ? AND state = 'accepted'`, missionID)
	if err != nil {
		return false, fmt.Errorf("ExpireMission: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("ExpireMission affected: %w", err)
	}
	return n > 0, nil
}

// DeclineMission transitions a mission from offered to declined.
func DeclineMission(ctx context.Context, db *sql.DB, missionID int64) error {
	_, err := db.ExecContext(ctx,
		`UPDATE characterMissions SET state = 'declined', declinedAt = NOW() WHERE missionID = ? AND state = 'offered'`,
		missionID)
	if err != nil {
		return fmt.Errorf("DeclineMission: %w", err)
	}
	return nil
}

// CompleteMission transitions a mission from accepted to completed.
func CompleteMission(ctx context.Context, db *sql.DB, missionID int64) error {
	_, err := db.ExecContext(ctx,
		`UPDATE characterMissions SET state = 'completed', completedAt = NOW()
		 WHERE missionID = ? AND state = 'accepted'`, missionID)
	if err != nil {
		return fmt.Errorf("CompleteMission: %w", err)
	}
	return nil
}

// FailMission transitions a mission to failed state.
func FailMission(ctx context.Context, db *sql.DB, missionID int64) error {
	_, err := db.ExecContext(ctx,
		`UPDATE characterMissions SET state = 'failed' WHERE missionID = ?`, missionID)
	if err != nil {
		return fmt.Errorf("FailMission: %w", err)
	}
	return nil
}

// UpdateKillProgress increments a mission's kill progress, capped at the required count.
// Returns progress, required and whether the call advanced the counter; kills past the
// objective leave the row untouched so callers skip re-notifying the client.
func UpdateKillProgress(ctx context.Context, db *sql.DB, missionID int64, increment int) (progress, required int, changed bool, err error) {
	res, err := db.ExecContext(ctx,
		`UPDATE characterMissions
		    SET killProgress = LEAST(killProgress + ?, killRequired)
		  WHERE missionID = ? AND killProgress < killRequired`,
		increment, missionID)
	if err != nil {
		return 0, 0, false, fmt.Errorf("UpdateKillProgress: %w", err)
	}
	if n, aerr := res.RowsAffected(); aerr == nil && n > 0 {
		changed = true
	}
	err = db.QueryRowContext(ctx,
		`SELECT killProgress, killRequired FROM characterMissions WHERE missionID = ?`,
		missionID).Scan(&progress, &required)
	if err != nil {
		return 0, 0, false, fmt.Errorf("UpdateKillProgress read: %w", err)
	}
	return progress, required, changed, nil
}

// CompleteKillObjective marks a kill mission's objective done regardless of the tally (killAll
// completes when the site is empty). The durable objectiveDone flag stops a later visit or
// server run from respawning the site. Returns whether the row changed.
func CompleteKillObjective(ctx context.Context, db *sql.DB, missionID int64) (bool, error) {
	res, err := db.ExecContext(ctx,
		`UPDATE characterMissions SET killProgress = killRequired, objectiveDone = 1
		  WHERE missionID = ? AND objectiveDone = 0`, missionID)
	if err != nil {
		return false, fmt.Errorf("CompleteKillObjective: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("CompleteKillObjective affected: %w", err)
	}
	return n > 0, nil
}

// ResetKillProgress zeroes the kill tally when a mission's dungeon spawns fresh (site
// re-warp, or NPCs respawned after a restart), so the journal count matches the NPCs
// present. A finished objective is never reset.
func ResetKillProgress(ctx context.Context, db *sql.DB, missionID int64) error {
	_, err := db.ExecContext(ctx,
		`UPDATE characterMissions SET killProgress = 0
		  WHERE missionID = ? AND objectiveDone = 0`, missionID)
	if err != nil {
		return fmt.Errorf("ResetKillProgress: %w", err)
	}
	return nil
}

// GetAcceptedMissions returns all accepted missions for a character.
func GetAcceptedMissions(ctx context.Context, db *sql.DB, charID int64) ([]*CharacterMission, error) {
	return queryCharMissions(ctx, db, charID, "accepted")
}

// GetOfferedMissions returns all offered missions for a character.
func GetOfferedMissions(ctx context.Context, db *sql.DB, charID int64) ([]*CharacterMission, error) {
	return queryCharMissions(ctx, db, charID, "offered")
}

func queryCharMissions(ctx context.Context, db *sql.DB, charID int64, state string) ([]*CharacterMission, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT missionID, charID, agentID, templateID, state,
		       COALESCE(destSystemID,0), COALESCE(destStationID,0),
		       COALESCE(pickupStationID,0), COALESCE(dropoffStationID,0),
		       COALESCE(bookmarkX,0), COALESCE(bookmarkY,0), COALESCE(bookmarkZ,0),
		       COALESCE(rewardISK,0), COALESCE(rewardLP,0), COALESCE(bonusISK,0),
		       COALESCE(killProgress,0), COALESCE(killRequired,0),
		       COALESCE(objectiveDone,0),
		       acceptedAt, expiresAt, completedAt, bonusDeadline
		FROM characterMissions
		WHERE charID = ? AND state = ?
		ORDER BY missionID`, charID, state)
	if err != nil {
		return nil, fmt.Errorf("queryCharMissions: %w", err)
	}
	defer rows.Close()

	var result []*CharacterMission
	for rows.Next() {
		m := &CharacterMission{}
		if err := rows.Scan(&m.MissionID, &m.CharID, &m.AgentID, &m.TemplateID, &m.State,
			&m.DestSystemID, &m.DestStationID,
			&m.PickupStationID, &m.DropoffStationID,
			&m.BookmarkX, &m.BookmarkY, &m.BookmarkZ,
			&m.RewardISK, &m.RewardLP, &m.BonusISK,
			&m.KillProgress, &m.KillRequired, &m.ObjectiveDone,
			&m.AcceptedAt, &m.ExpiresAt, &m.CompletedAt, &m.BonusDeadline); err != nil {
			return nil, fmt.Errorf("queryCharMissions scan: %w", err)
		}
		result = append(result, m)
	}
	return result, rows.Err()
}

// GetMissionsInSystemForChar returns accepted kill missions for a char in a system, to decide
// whether a player warping in gets dungeon spawns.
func GetMissionsInSystemForChar(ctx context.Context, db *sql.DB, charID, systemID int64) ([]*CharacterMission, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT missionID, charID, agentID, templateID, state,
		       COALESCE(destSystemID,0), COALESCE(destStationID,0),
		       COALESCE(pickupStationID,0), COALESCE(dropoffStationID,0),
		       COALESCE(bookmarkX,0), COALESCE(bookmarkY,0), COALESCE(bookmarkZ,0),
		       COALESCE(rewardISK,0), COALESCE(rewardLP,0), COALESCE(bonusISK,0),
		       COALESCE(killProgress,0), COALESCE(killRequired,0),
		       COALESCE(objectiveDone,0),
		       acceptedAt, expiresAt, completedAt, bonusDeadline
		FROM characterMissions
		WHERE charID = ? AND destSystemID = ? AND state = 'accepted'`, charID, systemID)
	if err != nil {
		return nil, fmt.Errorf("GetMissionsInSystemForChar: %w", err)
	}
	defer rows.Close()

	var result []*CharacterMission
	for rows.Next() {
		m := &CharacterMission{}
		if err := rows.Scan(&m.MissionID, &m.CharID, &m.AgentID, &m.TemplateID, &m.State,
			&m.DestSystemID, &m.DestStationID,
			&m.PickupStationID, &m.DropoffStationID,
			&m.BookmarkX, &m.BookmarkY, &m.BookmarkZ,
			&m.RewardISK, &m.RewardLP, &m.BonusISK,
			&m.KillProgress, &m.KillRequired, &m.ObjectiveDone,
			&m.AcceptedAt, &m.ExpiresAt, &m.CompletedAt, &m.BonusDeadline); err != nil {
			return nil, fmt.Errorf("GetMissionsInSystemForChar scan: %w", err)
		}
		result = append(result, m)
	}
	return result, rows.Err()
}

// ── Mission dungeon instances ────────────────────────────────────────────────

// DungeonInstance tracks a persistent mission dungeon in a solar system.
type DungeonInstance struct {
	MissionID     int64
	CharID        int64
	SolarSystemID int64
	DungeonVID    int64
	RoomID        int64
	AnchorX       float64
	AnchorY       float64
	AnchorZ       float64
	Spawned       bool
}

// CreateDungeonInstance records a persistent dungeon instance for a mission.
func CreateDungeonInstance(ctx context.Context, db *sql.DB, missionID, charID, solarSystemID, dungeonVID, roomID int64, x, y, z float64) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO missionDungeonInstances
		  (missionID, charID, solarSystemID, dungeonVID, roomID, anchorX, anchorY, anchorZ)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
		  solarSystemID=VALUES(solarSystemID), dungeonVID=VALUES(dungeonVID),
		  roomID=VALUES(roomID), anchorX=VALUES(anchorX), anchorY=VALUES(anchorY), anchorZ=VALUES(anchorZ)`,
		missionID, charID, solarSystemID, dungeonVID, roomID, x, y, z)
	if err != nil {
		return fmt.Errorf("CreateDungeonInstance: %w", err)
	}
	return nil
}

// GetDungeonInstanceByMission returns the dungeon instance for a mission, or nil.
func GetDungeonInstanceByMission(ctx context.Context, db *sql.DB, missionID int64) (*DungeonInstance, error) {
	d := &DungeonInstance{}
	err := db.QueryRowContext(ctx, `
		SELECT missionID, charID, solarSystemID, dungeonVID, roomID,
		       anchorX, anchorY, anchorZ, spawned
		FROM missionDungeonInstances WHERE missionID = ?`, missionID,
	).Scan(&d.MissionID, &d.CharID, &d.SolarSystemID, &d.DungeonVID, &d.RoomID,
		&d.AnchorX, &d.AnchorY, &d.AnchorZ, &d.Spawned)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetDungeonInstanceByMission: %w", err)
	}
	return d, nil
}

// GetDungeonInstancesForSystem returns all active dungeon instances in a solar system.
func GetDungeonInstancesForSystem(ctx context.Context, db *sql.DB, solarSystemID int64) ([]*DungeonInstance, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT missionID, charID, solarSystemID, dungeonVID, roomID,
		       anchorX, anchorY, anchorZ, spawned
		FROM missionDungeonInstances WHERE solarSystemID = ?`, solarSystemID)
	if err != nil {
		return nil, fmt.Errorf("GetDungeonInstancesForSystem: %w", err)
	}
	defer rows.Close()
	var result []*DungeonInstance
	for rows.Next() {
		d := &DungeonInstance{}
		if err := rows.Scan(&d.MissionID, &d.CharID, &d.SolarSystemID, &d.DungeonVID, &d.RoomID,
			&d.AnchorX, &d.AnchorY, &d.AnchorZ, &d.Spawned); err != nil {
			return nil, fmt.Errorf("GetDungeonInstancesForSystem scan: %w", err)
		}
		result = append(result, d)
	}
	return result, rows.Err()
}

// GetAllDungeonInstances returns all dungeon instances (for startup loading).
func GetAllDungeonInstances(ctx context.Context, db *sql.DB) ([]*DungeonInstance, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT missionID, charID, solarSystemID, dungeonVID, roomID,
		       anchorX, anchorY, anchorZ, spawned
		FROM missionDungeonInstances`)
	if err != nil {
		return nil, fmt.Errorf("GetAllDungeonInstances: %w", err)
	}
	defer rows.Close()
	var result []*DungeonInstance
	for rows.Next() {
		d := &DungeonInstance{}
		if err := rows.Scan(&d.MissionID, &d.CharID, &d.SolarSystemID, &d.DungeonVID, &d.RoomID,
			&d.AnchorX, &d.AnchorY, &d.AnchorZ, &d.Spawned); err != nil {
			return nil, fmt.Errorf("GetAllDungeonInstances scan: %w", err)
		}
		result = append(result, d)
	}
	return result, rows.Err()
}

// MarkDungeonInstanceSpawned sets spawned=1 for a dungeon instance.
func MarkDungeonInstanceSpawned(ctx context.Context, db *sql.DB, missionID int64) error {
	_, err := db.ExecContext(ctx,
		`UPDATE missionDungeonInstances SET spawned = 1 WHERE missionID = ?`, missionID)
	if err != nil {
		return fmt.Errorf("MarkDungeonInstanceSpawned: %w", err)
	}
	return nil
}

// DeleteDungeonInstance removes a dungeon instance and its site state.
func DeleteDungeonInstance(ctx context.Context, db *sql.DB, missionID int64) error {
	if _, err := db.ExecContext(ctx, `DELETE FROM missionDungeonSiteState WHERE missionID = ?`, missionID); err != nil {
		log.Printf("[db] DeleteDungeonInstance: best-effort write failed: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM missionDungeonWaveState WHERE missionID = ?`, missionID); err != nil {
		log.Printf("[db] DeleteDungeonInstance: best-effort write failed: %v", err)
	}
	_, err := db.ExecContext(ctx,
		`DELETE FROM missionDungeonInstances WHERE missionID = ?`, missionID)
	if err != nil {
		return fmt.Errorf("DeleteDungeonInstance: %w", err)
	}
	return nil
}

// DeleteDungeonNPCsByMission removes all dungeon NPC rows for a given mission.
func DeleteDungeonNPCsByMission(ctx context.Context, db *sql.DB, missionID int64) error {
	_, err := db.ExecContext(ctx, `DELETE FROM npcDungeonEntities WHERE missionID = ?`, missionID)
	return err
}

// CleanupStaleDungeonInstances deletes dungeon instances whose missions are no
// longer in 'accepted' state. Called at startup to garbage-collect instances
// for completed/failed/expired missions.
func CleanupStaleDungeonInstances(ctx context.Context, db *sql.DB) (int64, error) {
	res, err := db.ExecContext(ctx, `
		DELETE di FROM missionDungeonInstances di
		LEFT JOIN characterMissions cm ON cm.missionID = di.missionID
		WHERE cm.missionID IS NULL OR cm.state != 'accepted'`)
	if err != nil {
		return 0, fmt.Errorf("CleanupStaleDungeonInstances: %w", err)
	}
	n, _ := res.RowsAffected()
	// Also clean up orphaned site state rows.
	if _, err := db.ExecContext(ctx, `
		DELETE ss FROM missionDungeonSiteState ss
		LEFT JOIN missionDungeonInstances di ON di.missionID = ss.missionID
		WHERE di.missionID IS NULL`); err != nil {
		log.Printf("[db] CleanupStaleDungeonInstances: best-effort write failed: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		DELETE ws FROM missionDungeonWaveState ws
		LEFT JOIN missionDungeonInstances di ON di.missionID = ws.missionID
		WHERE di.missionID IS NULL`); err != nil {
		log.Printf("[db] CleanupStaleDungeonInstances: best-effort write failed: %v", err)
	}
	return n, nil
}

// ResetDungeonInstanceSpawned sets spawned=0 on all instances. Called at
// startup so that system-load re-spawns NPCs into memory.
func ResetDungeonInstanceSpawned(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `UPDATE missionDungeonInstances SET spawned = 0`)
	return err
}

// DeleteDungeonNPCsForStaleMissions removes npcDungeonEntities rows for
// missions no longer in 'accepted' state. Called at startup instead of a
// blanket TRUNCATE so active missions keep their NPC records.
func DeleteDungeonNPCsForStaleMissions(ctx context.Context, db *sql.DB) (int64, error) {
	res, err := db.ExecContext(ctx, `
		DELETE nde FROM npcDungeonEntities nde
		LEFT JOIN characterMissions cm ON cm.missionID = nde.missionID
		WHERE nde.missionID > 0 AND (cm.missionID IS NULL OR cm.state != 'accepted')`)
	if err != nil {
		return 0, fmt.Errorf("DeleteDungeonNPCsForStaleMissions: %w", err)
	}
	n, _ := res.RowsAffected()
	// Also delete orphan rows with missionID = 0 (ESP test spawns).
	if _, err := db.ExecContext(ctx, `DELETE FROM npcDungeonEntities WHERE missionID = 0`); err != nil {
		log.Printf("[db] DeleteDungeonNPCsForStaleMissions: best-effort write failed: %v", err)
	}
	return n, nil
}

// ── Site state persistence ──────────────────────────────────────────────────

// PersistDungeonKill records a dungeon object as killed for a mission.
func PersistDungeonKill(ctx context.Context, db *sql.DB, missionID, objectID int64) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO missionDungeonSiteState (missionID, objectID, killed)
		VALUES (?, ?, 1)
		ON DUPLICATE KEY UPDATE killed = 1`, missionID, objectID)
	return err
}

// PersistDungeonWaveRelease records a wave group as released for a mission room.
func PersistDungeonWaveRelease(ctx context.Context, db *sql.DB, missionID, roomID, waveGroup int64) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO missionDungeonWaveState (missionID, roomID, waveGroup, released)
		VALUES (?, ?, ?, 1)
		ON DUPLICATE KEY UPDATE released = 1`, missionID, roomID, waveGroup)
	return err
}

// LoadDungeonSiteKills returns all killed objectIDs for a mission.
func LoadDungeonSiteKills(ctx context.Context, db *sql.DB, missionID int64) ([]int64, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT objectID FROM missionDungeonSiteState WHERE missionID = ? AND killed = 1`, missionID)
	if err != nil {
		return nil, fmt.Errorf("LoadDungeonSiteKills: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// LoadDungeonSiteWaves returns released wave groups for a mission, keyed by roomID.
func LoadDungeonSiteWaves(ctx context.Context, db *sql.DB, missionID int64) (map[int64][]int64, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT roomID, waveGroup FROM missionDungeonWaveState WHERE missionID = ? AND released = 1`, missionID)
	if err != nil {
		return nil, fmt.Errorf("LoadDungeonSiteWaves: %w", err)
	}
	defer rows.Close()
	result := make(map[int64][]int64)
	for rows.Next() {
		var roomID, wg int64
		if err := rows.Scan(&roomID, &wg); err != nil {
			return nil, err
		}
		result[roomID] = append(result[roomID], wg)
	}
	return result, rows.Err()
}

// ── Loyalty Points ──────────────────────────────────────────────────────────

// GetLoyaltyPoints returns the LP balance for a character with an agent.
func GetLoyaltyPoints(ctx context.Context, db *sql.DB, charID, agentID int64) (int, error) {
	var points int
	err := db.QueryRowContext(ctx,
		`SELECT COALESCE(points, 0) FROM agentLoyaltyPoints WHERE charID = ? AND agentID = ?`,
		charID, agentID).Scan(&points)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("GetLoyaltyPoints: %w", err)
	}
	return points, nil
}

// AddLoyaltyPoints adds LP to a character's balance with an agent.
func AddLoyaltyPoints(ctx context.Context, db *sql.DB, charID, agentID int64, amount int) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO agentLoyaltyPoints (charID, agentID, points) VALUES (?, ?, ?)
		ON DUPLICATE KEY UPDATE points = points + ?`,
		charID, agentID, amount, amount)
	if err != nil {
		return fmt.Errorf("AddLoyaltyPoints: %w", err)
	}
	return nil
}

// ── Destination generation ──────────────────────────────────────────────────

// NearbyJumpBands is the number of jump-distance buckets GetNearbySystemsByJumps
// returns: index 0 is the agent's own system, index N is N jumps out.
const NearbyJumpBands = 4

// MissionJumpWeights are relative weights over the jump bands, rolled like the division
// mission weights. See docs/mission-notes.md#jump-weights.
var MissionJumpWeights = [NearbyJumpBands]int{20, 35, 30, 15}

// missionJumpWeights answers the live weights: server_settings
// missions.jumpWeights (four numbers, own system out to 3 jumps), else
// MissionJumpWeights. All-zero falls back to the defaults.
func missionJumpWeights() [NearbyJumpBands]int {
	def := make([]float64, NearbyJumpBands)
	for i, w := range MissionJumpWeights {
		def[i] = float64(w)
	}
	var out [NearbyJumpBands]int
	sum := 0
	for i, f := range CachedSettingFloats("missions.jumpWeights", def, 0, 1e6) {
		out[i] = int(f)
		sum += out[i]
	}
	if sum == 0 {
		return MissionJumpWeights
	}
	return out
}

// GetNearbySystemsByJumps returns the solar systems within NearbyJumpBands-1 jumps of
// originSystemID, bucketed by jump distance (index 0 is the origin, 1 its neighbours). Each
// bucket is filtered by the security range, so any bucket can come back empty.
func GetNearbySystemsByJumps(ctx context.Context, db *sql.DB, originSystemID int64, minSec, maxSec sql.NullFloat64) ([][]int64, error) {
	bands := make([][]int64, NearbyJumpBands)
	bands[0] = []int64{originSystemID}

	// BFS over mapSolarSystemJumps, one query per depth rather than per system.
	visited := map[int64]bool{originSystemID: true}
	frontier := []int64{originSystemID}
	for depth := 1; depth < NearbyJumpBands && len(frontier) > 0; depth++ {
		args := make([]any, len(frontier))
		for i, id := range frontier {
			args[i] = id
		}
		rows, err := db.QueryContext(ctx,
			`SELECT DISTINCT toSolarSystemID FROM mapSolarSystemJumps
			 WHERE fromSolarSystemID IN (`+placeholders(len(frontier))+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("GetNearbySystemsByJumps: %w", err)
		}
		var next []int64
		for rows.Next() {
			var neighborID int64
			if err := rows.Scan(&neighborID); err != nil {
				rows.Close()
				return nil, err
			}
			if !visited[neighborID] {
				visited[neighborID] = true
				next = append(next, neighborID)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("GetNearbySystemsByJumps: %w", err)
		}
		bands[depth] = next
		frontier = next
	}

	if !minSec.Valid && !maxSec.Valid {
		return bands, nil
	}

	// One security lookup for the whole neighbourhood, then filter each band.
	all := make([]any, 0, len(visited))
	for id := range visited {
		all = append(all, id)
	}
	if len(all) == 0 {
		return bands, nil
	}
	rows, err := db.QueryContext(ctx,
		`SELECT solarSystemID, security FROM mapSolarSystems
		 WHERE solarSystemID IN (`+placeholders(len(all))+`)`, all...)
	if err != nil {
		return nil, fmt.Errorf("GetNearbySystemsByJumps security: %w", err)
	}
	defer rows.Close()
	security := make(map[int64]float64, len(all))
	for rows.Next() {
		var id int64
		var sec float64
		if err := rows.Scan(&id, &sec); err != nil {
			return nil, err
		}
		security[id] = sec
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("GetNearbySystemsByJumps security: %w", err)
	}

	for depth, band := range bands {
		kept := band[:0]
		for _, sysID := range band {
			sec, ok := security[sysID]
			if !ok {
				continue
			}
			if minSec.Valid && sec < minSec.Float64 {
				continue
			}
			if maxSec.Valid && sec > maxSec.Float64 {
				continue
			}
			kept = append(kept, sysID)
		}
		bands[depth] = kept
	}
	return bands, nil
}

// RollJumpDistance picks a jump-distance band by MissionJumpWeights, skipping
// bands with no candidates so their weight is redistributed over the bands
// that do have systems.  Returns -1 when every band is empty.
func RollJumpDistance(bands [][]int64) int {
	weights := missionJumpWeights()
	total := 0
	for depth, band := range bands {
		if depth < NearbyJumpBands && len(band) > 0 {
			total += weights[depth]
		}
	}
	if total <= 0 {
		return -1
	}
	roll := rand.Intn(total)
	for depth, band := range bands {
		if depth >= NearbyJumpBands || len(band) == 0 {
			continue
		}
		if roll < weights[depth] {
			return depth
		}
		roll -= weights[depth]
	}
	return -1
}

// JumpBandOrder returns every non-empty band index ordered by distance from a rolled band:
// the rolled band first, then its nearest neighbours outward. Callers that need a system with
// something in it (a station) walk this order.
func JumpBandOrder(bands [][]int64) []int {
	start := RollJumpDistance(bands)
	if start < 0 {
		return nil
	}
	var order []int
	for offset := 0; offset < NearbyJumpBands; offset++ {
		for _, depth := range [2]int{start + offset, start - offset} {
			if depth < 0 || depth >= len(bands) || len(bands[depth]) == 0 {
				continue
			}
			order = append(order, depth)
			if offset == 0 {
				break // start+0 and start-0 are the same band
			}
		}
	}
	return order
}

// PickNearbySystem rolls a jump distance and returns a random system at that
// distance.  Returns 0 when no band holds an eligible system.
func PickNearbySystem(bands [][]int64) int64 {
	depth := RollJumpDistance(bands)
	if depth < 0 {
		return 0
	}
	band := bands[depth]
	return band[rand.Intn(len(band))]
}

// GetStationsInSystem returns stationIDs in a solar system.
func GetStationsInSystem(ctx context.Context, db *sql.DB, systemID int64) ([]int64, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT stationID FROM staStations WHERE solarSystemID = ?`, systemID)
	if err != nil {
		return nil, fmt.Errorf("GetStationsInSystem: %w", err)
	}
	defer rows.Close()

	var result []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		result = append(result, id)
	}
	return result, rows.Err()
}

// ── Reward computation ──────────────────────────────────────────────────────

// QualityMultiplier computes the reward multiplier from agent quality.
// quality ranges from roughly -20 to +20.
// Result: 1.0 + quality*step, floored. step is server_settings
// missions.qualityStep (default 0.05), the floor missions.qualityFloor (0.1).
func QualityMultiplier(quality int) float64 {
	m := 1.0 + float64(quality)*CachedSettingFloat("missions.qualityStep", 0.05, 0, 1)
	if floor := CachedSettingFloat("missions.qualityFloor", 0.1, 0, 10); m < floor {
		m = floor
	}
	return m
}

// ComputeRewards applies the quality multiplier and config.json's
// rates.missionReward / rates.loyaltyPoints to base rewards.
func ComputeRewards(t *MissionTemplate, quality int) (rewardISK, bonusISK int64, rewardLP int) {
	mult := QualityMultiplier(quality)
	iskMult := mult * rates.MissionReward
	lpMult := mult * rates.LoyaltyPoints
	rewardISK = int64(math.Max(float64(t.BaseRewardISK)*iskMult, 1))
	bonusISK = int64(math.Max(float64(t.BaseBonusISK)*iskMult, 1))
	rewardLP = int(math.Max(float64(t.BaseRewardLP)*lpMult, 1))
	if t.BaseRewardISK == 0 || rates.MissionReward == 0 {
		rewardISK = 0
	}
	if t.BaseBonusISK == 0 || rates.MissionReward == 0 {
		bonusISK = 0
	}
	if t.BaseRewardLP == 0 || rates.LoyaltyPoints == 0 {
		rewardLP = 0
	}
	return
}

// ── Hangar item helpers (for courier/trade missions) ────────────────────────

// ItemDeltaKind classifies an entity change made by mission item bookkeeping.
type ItemDeltaKind int

const (
	ItemDeltaNew     ItemDeltaKind = iota // item created
	ItemDeltaQty                          // stack quantity changed
	ItemDeltaRemoved                      // stack deleted
)

// ItemDelta records one entity change so the caller can send the matching
// OnItemChange notification. Row holds the post-change row for New/Qty and the
// pre-delete row for Removed. OldQty is only meaningful for Qty.
type ItemDelta struct {
	Kind   ItemDeltaKind
	Row    *EntityRow
	OldQty int64
}

// missionItemStacksSQL selects the stacks of a type that count toward a mission objective at
// a station: the station hangar (flag 4), then the cargo of docked ships (flag 5).
// Params: ownerID, typeID, stationID, ownerID, stationID.
const missionItemStacksSQL = `
	SELECT e.itemID, e.quantity
	FROM entity e
	LEFT JOIN entity s ON s.itemID = e.locationID
	WHERE e.ownerID = ? AND e.typeID = ?
	  AND ( (e.flag = 4 AND e.locationID = ?)
	     OR (e.flag = 5 AND s.ownerID = ? AND s.locationID = ?) )
	ORDER BY (e.flag = 4) DESC, e.itemID`

// CountItemInHangar returns how many of typeID a character has at stationID: the station
// hangar plus the cargo of docked ships.
func CountItemInHangar(ctx context.Context, db *sql.DB, charID, stationID, typeID int64) (int, error) {
	rows, err := db.QueryContext(ctx, missionItemStacksSQL, charID, typeID, stationID, charID, stationID)
	if err != nil {
		return 0, fmt.Errorf("CountItemInHangar: %w", err)
	}
	defer rows.Close()

	total := 0
	for rows.Next() {
		var itemID int64
		var qty int
		if err := rows.Scan(&itemID, &qty); err != nil {
			return 0, fmt.Errorf("CountItemInHangar scan: %w", err)
		}
		total += qty
	}
	return total, rows.Err()
}

// RemoveItemsFromHangar removes qty of typeID from a character's holdings at stationID,
// hangar first then docked ships' cargo, FIFO by itemID. Returns the amount removed and the
// per-entity changes for the client.
func RemoveItemsFromHangar(ctx context.Context, db *sql.DB, charID, stationID, typeID int64, qty int) (int, []ItemDelta, error) {
	rows, err := db.QueryContext(ctx, missionItemStacksSQL, charID, typeID, stationID, charID, stationID)
	if err != nil {
		return 0, nil, fmt.Errorf("RemoveItemsFromHangar query: %w", err)
	}
	defer rows.Close()

	remaining := qty
	removed := 0
	type stack struct {
		itemID   int64
		quantity int
	}
	var stacks []stack
	for rows.Next() {
		var s stack
		if err := rows.Scan(&s.itemID, &s.quantity); err != nil {
			return 0, nil, err
		}
		stacks = append(stacks, s)
	}
	rows.Close()

	var deltas []ItemDelta
	for _, s := range stacks {
		if remaining <= 0 {
			break
		}
		// Snapshot the row first: a deleted stack cannot be re-read, and the client needs the old
		// quantity for a partial take.
		er, _ := GetEntityRow(ctx, db, s.itemID)
		if s.quantity <= remaining {
			if _, err := db.ExecContext(ctx, `DELETE FROM entity WHERE itemID = ?`, s.itemID); err != nil {
				return removed, deltas, fmt.Errorf("RemoveItemsFromHangar delete %d: %w", s.itemID, err)
			}
			remaining -= s.quantity
			removed += s.quantity
			if er != nil {
				deltas = append(deltas, ItemDelta{Kind: ItemDeltaRemoved, Row: er})
			}
		} else {
			if _, err := db.ExecContext(ctx,
				`UPDATE entity SET quantity = quantity - ? WHERE itemID = ?`, remaining, s.itemID); err != nil {
				return removed, deltas, fmt.Errorf("RemoveItemsFromHangar update %d: %w", s.itemID, err)
			}
			removed += remaining
			remaining = 0
			if er != nil {
				oldQty := er.Quantity
				if newER, gerr := GetEntityRow(ctx, db, s.itemID); gerr == nil && newER != nil {
					er = newER
				}
				deltas = append(deltas, ItemDelta{Kind: ItemDeltaQty, Row: er, OldQty: oldQty})
			}
		}
	}
	return removed, deltas, nil
}

// SpawnItemInHangar creates a new item in a character's hangar at stationID and returns its
// itemID, through CreateItem for the itemName and singleton flag (rewards can be ships or
// blueprints).
func SpawnItemInHangar(ctx context.Context, db *sql.DB, charID, stationID, typeID int64, qty int) (int64, error) {
	itemID, err := CreateItem(ctx, db, typeID, charID, stationID, qty)
	if err != nil {
		return 0, fmt.Errorf("SpawnItemInHangar: %w", err)
	}
	return itemID, nil
}

// ── Name lookups for mission text substitution ──────────────────────────────

// GetCharacterName returns the character name for a charID.
func GetCharacterName(ctx context.Context, db *sql.DB, charID int64) string {
	var name string
	err := db.QueryRowContext(ctx,
		`SELECT characterName FROM character_ WHERE characterID = ?`, charID).Scan(&name)
	if err != nil {
		return ""
	}
	return name
}

// GetAgentName returns the agent's owner name.
func GetAgentName(ctx context.Context, db *sql.DB, agentID int64) string {
	var name string
	err := db.QueryRowContext(ctx,
		`SELECT COALESCE(o.ownerName, '') FROM agtAgents a
		 LEFT JOIN eveStaticOwners o ON o.ownerID = a.agentID
		 WHERE a.agentID = ?`, agentID).Scan(&name)
	if err != nil || name == "" {
		// Fallback: try character_ table (agents are NPCs stored as characters).
		db.QueryRowContext(ctx,
			`SELECT characterName FROM character_ WHERE characterID = ?`, agentID).Scan(&name)
	}
	return name
}

// GetFactionName returns a faction's name from eveStaticOwners.
func GetFactionName(ctx context.Context, db *sql.DB, factionID int64) string {
	var name string
	db.QueryRowContext(ctx,
		`SELECT ownerName FROM eveStaticOwners WHERE ownerID = ? AND typeID IN (30, 500)`,
		factionID).Scan(&name)
	return name
}

// GetStationName returns a station's name.
func GetStationName(ctx context.Context, db *sql.DB, stationID int64) string {
	var name string
	db.QueryRowContext(ctx,
		`SELECT stationName FROM staStations WHERE stationID = ?`, stationID).Scan(&name)
	return name
}

// ── Mission text substitution ───────────────────────────────────────────────

// MissionTextParams holds values for mission text placeholder substitution.
type MissionTextParams struct {
	CharName    string
	AgentName   string
	SystemName  string
	StationName string
	ItemName    string
	ItemQty     int
	RewardISK   int64
	BonusISK    int64
	RewardLP    int
}

// SubstituteMissionText replaces {placeholder} tokens in mission text: charName, agentName,
// systemName, stationName, itemName, itemQty, rewardISK, bonusISK and rewardLP. Names match
// case-insensitively; an unknown placeholder is left untouched.
func SubstituteMissionText(text string, p *MissionTextParams) string {
	if !strings.Contains(text, "{") {
		return text
	}
	vals := map[string]string{
		"charname":    p.CharName,
		"agentname":   p.AgentName,
		"systemname":  p.SystemName,
		"stationname": p.StationName,
		"itemname":    p.ItemName,
		"itemqty":     strconv.Itoa(p.ItemQty),
		"rewardisk":   strconv.FormatInt(p.RewardISK, 10),
		"bonusisk":    strconv.FormatInt(p.BonusISK, 10),
		"rewardlp":    strconv.Itoa(p.RewardLP),
	}
	return missionPlaceholderRE.ReplaceAllStringFunc(text, func(m string) string {
		if v, ok := vals[strings.ToLower(m[1:len(m)-1])]; ok {
			return v
		}
		return m
	})
}

// missionPlaceholderRE matches a single {word} placeholder.
var missionPlaceholderRE = regexp.MustCompile(`\{[A-Za-z]+\}`)

// BuildMissionTextParams creates substitution params from available mission context.
func BuildMissionTextParams(ctx context.Context, db *sql.DB, charID, agentID int64,
	agent *AgentInfo, tmpl *MissionTemplate, mission *CharacterMission) *MissionTextParams {
	p := &MissionTextParams{}
	p.CharName = GetCharacterName(ctx, db, charID)
	p.AgentName = GetAgentName(ctx, db, agentID)

	// System/station names from mission instance if available, else agent.
	if mission != nil && mission.DestSystemID > 0 {
		p.SystemName, _ = GetSystemName(ctx, db, mission.DestSystemID)
	} else if agent != nil && agent.SolarSystemID > 0 {
		p.SystemName, _ = GetSystemName(ctx, db, agent.SolarSystemID)
	}
	if mission != nil && mission.DestStationID > 0 {
		p.StationName = GetStationName(ctx, db, mission.DestStationID)
	} else if agent != nil && agent.StationID > 0 {
		p.StationName = GetStationName(ctx, db, agent.StationID)
	}

	// Item name/qty depends on mission type.
	if tmpl != nil {
		switch tmpl.MissionType {
		case "courier":
			if tmpl.CourierTypeID > 0 {
				p.ItemName, _ = GetTypeName(ctx, db, tmpl.CourierTypeID)
				p.ItemQty = tmpl.CourierQty
			}
		case "trade":
			if tmpl.TradeTypeID > 0 {
				p.ItemName, _ = GetTypeName(ctx, db, tmpl.TradeTypeID)
				p.ItemQty = tmpl.TradeQty
			}
		case "mining":
			if tmpl.MiningTypeID > 0 {
				p.ItemName, _ = GetTypeName(ctx, db, tmpl.MiningTypeID)
				p.ItemQty = tmpl.MiningQty
			}
		case "kill":
			if tmpl.CompletionType == "killItem" && tmpl.KillItemTypeID > 0 {
				p.ItemName, _ = GetTypeName(ctx, db, tmpl.KillItemTypeID)
				p.ItemQty = tmpl.KillItemQty
			}
		}
	}

	if mission != nil {
		p.RewardISK = mission.RewardISK
		p.BonusISK = mission.BonusISK
		p.RewardLP = mission.RewardLP
	}

	return p
}

// missionModifyDone reports whether one of EnsureMissionTables' MODIFY statements is
// already in force, so it is not re-issued on every boot. ADD COLUMN statements fail
// harmlessly on an existing column and are left alone.
func missionModifyDone(ctx context.Context, db *sql.DB, stmt string) bool {
	const prefix = "ALTER TABLE missionTemplates MODIFY "
	if !strings.HasPrefix(stmt, prefix) {
		return false
	}
	rest := stmt[len(prefix):]
	col := rest[:strings.IndexByte(rest, ' ')]
	if col == "completionType" {
		return !columnNeedsModify(ctx, db, "missionTemplates", col, true, func(ct string) bool {
			return ct == "enum('killall','killtarget','killstructure','killitem')"
		})
	}
	return !columnNeedsModify(ctx, db, "missionTemplates", col, true,
		func(ct string) bool { return ct == "double" })
}
