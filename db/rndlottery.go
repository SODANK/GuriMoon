package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"time"
)

// ── R&D patent lottery ───────────────────────────────────────────────────────
// The payoff half of the R&D agent system: each agent runs a rolling round, each active
// project is an entry weighted by its RP, and the winner gets a Tech II BPO from the pool
// for their own field (rndfieldblueprints). A win is a pending rndPatentOffers row, not an
// agtOffers row. See docs/rnd-agents.md#the-patent-lottery-aug-2026.

// SyntheticPilot is set once at module init (db cannot import modapi) and reports whether
// charID is a generated pilot; such an entry's tickets are scaled by rnd.patentBotWeightPct.
// Nil in a plain build, which treats everybody as a person.
var SyntheticPilot func(charID int64) bool

func isSyntheticPilot(charID int64) bool {
	return SyntheticPilot != nil && SyntheticPilot(charID)
}

// Patent offer states.
const (
	PatentOfferPending = 0
	PatentOfferClaimed = 1
	PatentOfferExpired = 2
	PatentOfferVoided  = 3
)

// Draw outcomes, stored on rndPatentDraws.outcome.
const (
	PatentDrawWon  = "won"  // somebody held tickets and took a blueprint
	PatentDrawVoid = "void" // nobody was in the hat
)

const (
	settingPatentEnabled   = "rnd.patentLotteryEnabled"
	settingPatentRoundDays = "rnd.patentRoundDays"
	settingPatentClaimDays = "rnd.patentClaimDays"
	settingPatentBotWeight = "rnd.patentBotWeightPct"
	settingPatentMinRP     = "rnd.patentMinTickets"
	settingPatentNoDupes   = "rnd.patentAvoidDuplicates"
)

// Defaults. The bot weight is nonzero so a lottery on a bot-populated shard stays winnable;
// 0 is still a valid setting (GetPatentBotWeightPct).
const (
	DefaultPatentRoundDays    = 7
	DefaultPatentClaimDays    = 14
	DefaultPatentBotWeightPct = 25
)

// EnsureRnDLotteryTables creates the three lottery tables. Called from main() after
// EnsureResearchTables. rndAgentRounds is never bulk-seeded: StartResearch opens a round
// when somebody enrols and a draw with no entrants deletes it.
func EnsureRnDLotteryTables(ctx context.Context, db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS rndAgentRounds (
			agentID   BIGINT NOT NULL,
			roundNo   INT    NOT NULL DEFAULT 1,
			startedAt BIGINT NOT NULL DEFAULT 0,
			drawAt    BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (agentID),
			KEY idx_drawAt (drawAt)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`,
		// UNIQUE (agentID, roundNo) is the double-draw guard: two sweeps racing one due round
		// collide here.
		`CREATE TABLE IF NOT EXISTS rndPatentDraws (
			drawID          BIGINT NOT NULL AUTO_INCREMENT,
			agentID         BIGINT NOT NULL,
			roundNo         INT    NOT NULL DEFAULT 0,
			drawnAt         BIGINT NOT NULL DEFAULT 0,
			entrants        INT    NOT NULL DEFAULT 0,
			totalTickets    DOUBLE NOT NULL DEFAULT 0,
			winnerCharID    BIGINT NOT NULL DEFAULT 0,
			winnerTickets   DOUBLE NOT NULL DEFAULT 0,
			skillTypeID     INT    NOT NULL DEFAULT 0,
			blueprintTypeID INT    NOT NULL DEFAULT 0,
			synthetic       TINYINT(1) NOT NULL DEFAULT 0,
			outcome         VARCHAR(16) NOT NULL DEFAULT 'won',
			PRIMARY KEY (drawID),
			UNIQUE KEY uniq_agent_round (agentID, roundNo),
			KEY idx_drawnAt (drawnAt),
			KEY idx_winner (winnerCharID)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`,
		// offerID is a surrogate key: a winner who ignores the letter and wins again would collide
		// on (charID, agentID), and the ESP addresses one row.
		`CREATE TABLE IF NOT EXISTS rndPatentOffers (
			offerID         BIGINT NOT NULL AUTO_INCREMENT,
			charID          BIGINT NOT NULL,
			agentID         BIGINT NOT NULL,
			blueprintTypeID INT    NOT NULL DEFAULT 0,
			skillTypeID     INT    NOT NULL DEFAULT 0,
			drawID          BIGINT NOT NULL DEFAULT 0,
			wonAt           BIGINT NOT NULL DEFAULT 0,
			expiresAt       BIGINT NOT NULL DEFAULT 0,
			state           TINYINT NOT NULL DEFAULT 0,
			claimedAt       BIGINT NOT NULL DEFAULT 0,
			itemID          BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (offerID),
			KEY idx_char_state (charID, state),
			KEY idx_agent_state (agentID, state),
			KEY idx_expiry (state, expiresAt)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("EnsureRnDLotteryTables: %w", err)
		}
	}
	return nil
}

// ── settings ────────────────────────────────────────────────────────────────

// PatentLotteryEnabled reports whether the draw sweep runs at all.
func PatentLotteryEnabled(ctx context.Context, db *sql.DB) bool {
	return settingInt(ctx, db, settingPatentEnabled, 1, 0, 1) == 1
}

// SetPatentLotteryEnabled persists the master switch.
func SetPatentLotteryEnabled(ctx context.Context, db *sql.DB, on bool) error {
	v := "0"
	if on {
		v = "1"
	}
	return SetServerSetting(ctx, db, settingPatentEnabled, v)
}

// GetPatentRoundDays returns the length of one allocation round.
func GetPatentRoundDays(ctx context.Context, db *sql.DB) int {
	return settingInt(ctx, db, settingPatentRoundDays, DefaultPatentRoundDays, 1, 90)
}

// SetPatentRoundDays persists the round length. It applies to the next round each agent
// starts, never to a round already running.
func SetPatentRoundDays(ctx context.Context, db *sql.DB, days int) error {
	return SetServerSetting(ctx, db, settingPatentRoundDays, strconv.Itoa(clampInt(days, 1, 90)))
}

// GetPatentClaimDays returns how long a won patent waits at the agent before it
// lapses.
func GetPatentClaimDays(ctx context.Context, db *sql.DB) int {
	return settingInt(ctx, db, settingPatentClaimDays, DefaultPatentClaimDays, 1, 365)
}

// SetPatentClaimDays persists the claim window.
func SetPatentClaimDays(ctx context.Context, db *sql.DB, days int) error {
	return SetServerSetting(ctx, db, settingPatentClaimDays, strconv.Itoa(clampInt(days, 1, 365)))
}

// GetPatentBotWeightPct returns the percentage of its RP a synthetic pilot enters the draw
// with. At 0 bots hold no tickets but are still entrants, and their RP is wiped with
// everyone else's.
func GetPatentBotWeightPct(ctx context.Context, db *sql.DB) int {
	return settingInt(ctx, db, settingPatentBotWeight, DefaultPatentBotWeightPct, 0, 100)
}

// SetPatentBotWeightPct persists the synthetic-pilot ticket scale.
func SetPatentBotWeightPct(ctx context.Context, db *sql.DB, pct int) error {
	return SetServerSetting(ctx, db, settingPatentBotWeight, strconv.Itoa(clampInt(pct, 0, 100)))
}

// GetPatentMinTickets returns the RP floor below which an entry holds no tickets (default 0,
// no floor).
func GetPatentMinTickets(ctx context.Context, db *sql.DB) int {
	return settingInt(ctx, db, settingPatentMinRP, 0, 0, 1000000)
}

// SetPatentMinTickets persists the ticket floor.
func SetPatentMinTickets(ctx context.Context, db *sql.DB, rp int) error {
	return SetServerSetting(ctx, db, settingPatentMinRP, strconv.Itoa(clampInt(rp, 0, 1000000)))
}

// PatentAvoidDuplicates reports whether the prize pick skips blueprints the
// winner already owns.
func PatentAvoidDuplicates(ctx context.Context, db *sql.DB) bool {
	return settingInt(ctx, db, settingPatentNoDupes, 1, 0, 1) == 1
}

// SetPatentAvoidDuplicates persists the duplicate-avoidance switch.
func SetPatentAvoidDuplicates(ctx context.Context, db *sql.DB, on bool) error {
	v := "0"
	if on {
		v = "1"
	}
	return SetServerSetting(ctx, db, settingPatentNoDupes, v)
}

func clampInt(n, min, max int) int {
	if n < min {
		return min
	}
	if n > max {
		return max
	}
	return n
}

// ── types ───────────────────────────────────────────────────────────────────

// AgentRound is one agent's current allocation round.
type AgentRound struct {
	AgentID   int64
	RoundNo   int
	StartedAt int64
	DrawAt    int64
}

// PatentEntrant is one project in a draw.
type PatentEntrant struct {
	CharID      int64
	SkillTypeID int64
	Points      float64 // raw RP at draw time
	Tickets     float64 // after the synthetic scale and the floor; 0 = not in the hat
	Synthetic   bool
	DeadField   bool // their field has no patent pool, so there is nothing to win
}

// PatentDrawResult is what one draw did, for the sweep's announcement and the
// ESP log.
type PatentDrawResult struct {
	AgentID       int64
	RoundNo       int
	Outcome       string
	Entrants      int
	EntrantIDs    []int64 // everyone whose RP was wiped; each needs a journal invalidate
	TotalTickets  float64
	WinnerCharID  int64
	WinnerTickets float64
	Synthetic     bool
	SkillTypeID   int64
	BlueprintID   int64
	DrawID        int64
	OfferID       int64
	ExpiresAt     int64
}

// PatentOffer is a won blueprint waiting to be collected at the agent.
type PatentOffer struct {
	OfferID     int64
	CharID      int64
	AgentID     int64
	BlueprintID int64
	SkillTypeID int64
	DrawID      int64
	WonAt       int64
	ExpiresAt   int64
	State       int
	ClaimedAt   int64
	ItemID      int64
}

// patentOfferCols is the column list every offer query shares; scanPatentOffer
// unpacks exactly these, in this order.
const patentOfferCols = `offerID, charID, agentID, blueprintTypeID, skillTypeID,
	drawID, wonAt, expiresAt, state, claimedAt, itemID`

func scanPatentOffer(s rowScanner) (*PatentOffer, error) {
	p := &PatentOffer{}
	err := s.Scan(&p.OfferID, &p.CharID, &p.AgentID, &p.BlueprintID, &p.SkillTypeID,
		&p.DrawID, &p.WonAt, &p.ExpiresAt, &p.State, &p.ClaimedAt, &p.ItemID)
	if err != nil {
		return nil, err
	}
	return p, nil
}

// ── round bookkeeping ───────────────────────────────────────────────────────

// EnsureAgentRound opens an agent's first round if it has none. StartResearch, the single
// funnel for every enrolment, calls it. INSERT IGNORE keeps a second enrolee from resetting
// the clock the first is racing.
func EnsureAgentRound(ctx context.Context, db DBTX, agentID int64, now time.Time, roundDays int) error {
	if roundDays < 1 {
		roundDays = DefaultPatentRoundDays
	}
	start := now.Unix()
	_, err := db.ExecContext(ctx, `
		INSERT IGNORE INTO rndAgentRounds (agentID, roundNo, startedAt, drawAt)
		VALUES (?, 1, ?, ?)`, agentID, start, start+int64(roundDays)*86400)
	if err != nil {
		return fmt.Errorf("EnsureAgentRound: %w", err)
	}
	return nil
}

// GetAgentRound returns an agent's current round, or nil when it runs none.
func GetAgentRound(ctx context.Context, db *sql.DB, agentID int64) (*AgentRound, error) {
	r := &AgentRound{}
	err := db.QueryRowContext(ctx,
		`SELECT agentID, roundNo, startedAt, drawAt FROM rndAgentRounds WHERE agentID = ?`, agentID,
	).Scan(&r.AgentID, &r.RoundNo, &r.StartedAt, &r.DrawAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("GetAgentRound: %w", err)
	}
	return r, nil
}

// DueAgentRounds returns the agents whose round has closed, oldest first.
func DueAgentRounds(ctx context.Context, db *sql.DB, now int64, limit int) ([]int64, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := db.QueryContext(ctx,
		`SELECT agentID FROM rndAgentRounds WHERE drawAt <= ? ORDER BY drawAt LIMIT ?`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("DueAgentRounds: %w", err)
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

// ── the draw ────────────────────────────────────────────────────────────────

// RunPatentDraw closes one agent's round in one transaction: pick a winner weighted by
// research points, grant a patent from the winner's own field, wipe the agent's RP and open
// the next round. (nil, nil) means nothing to do: no round, not due, or already drawn.
func RunPatentDraw(ctx context.Context, db *sql.DB, agentID int64, now time.Time) (*PatentDrawResult, error) {
	roundDays := GetPatentRoundDays(ctx, db)
	claimDays := GetPatentClaimDays(ctx, db)
	botPct := GetPatentBotWeightPct(ctx, db)
	minRP := float64(GetPatentMinTickets(ctx, db))
	noDupes := PatentAvoidDuplicates(ctx, db)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("RunPatentDraw begin: %w", err)
	}
	defer tx.Rollback()

	var round AgentRound
	err = tx.QueryRowContext(ctx,
		`SELECT agentID, roundNo, startedAt, drawAt FROM rndAgentRounds WHERE agentID = ? FOR UPDATE`, agentID,
	).Scan(&round.AgentID, &round.RoundNo, &round.StartedAt, &round.DrawAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("RunPatentDraw round: %w", err)
	}
	nowUnix := now.Unix()
	if round.DrawAt > nowUnix {
		// A concurrent pass already drew this round and moved the clock on.
		return nil, nil
	}

	// The join drops projects whose character no longer exists. There is no accountID > 0
	// filter: it would also drop every bot, which the weight already handles.
	rows, err := tx.QueryContext(ctx, `
		SELECT cr.charID, cr.skillTypeID, cr.pointsPerDay, cr.accruedPoints, cr.accrualStart
		FROM characterresearch cr
		JOIN character_ c ON c.characterID = cr.charID
		WHERE cr.agentID = ?
		FOR UPDATE`, agentID)
	if err != nil {
		return nil, fmt.Errorf("RunPatentDraw entrants: %w", err)
	}
	var entrants []*PatentEntrant
	for rows.Next() {
		var e PatentEntrant
		var r CharacterResearch
		if err := rows.Scan(&e.CharID, &e.SkillTypeID, &r.PointsPerDay, &r.AccruedPoints, &r.AccrualStart); err != nil {
			rows.Close()
			return nil, fmt.Errorf("RunPatentDraw entrant scan: %w", err)
		}
		// The lazy accrual accessor, never a reimplementation of it.
		e.Points = r.CurrentPoints(now)
		entrants = append(entrants, &e)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("RunPatentDraw entrants: %w", err)
	}
	rows.Close()

	res := &PatentDrawResult{
		AgentID:  agentID,
		RoundNo:  round.RoundNo,
		Outcome:  PatentDrawVoid,
		Entrants: len(entrants),
	}

	// A field with an empty patent pool holds no tickets. This runs before the pick, or an
	// empty pool would void a round the other fields were competing in.
	live, err := fieldsWithPatents(ctx, tx, entrants)
	if err != nil {
		return nil, err
	}

	var total float64
	for _, e := range entrants {
		res.EntrantIDs = append(res.EntrantIDs, e.CharID)
		e.Synthetic = isSyntheticPilot(e.CharID)
		e.DeadField = !live[e.SkillTypeID]
		switch {
		case e.DeadField, e.Points < minRP:
			e.Tickets = 0
		case e.Synthetic:
			e.Tickets = e.Points * float64(botPct) / 100.0
		default:
			e.Tickets = e.Points
		}
		total += e.Tickets
	}
	res.TotalTickets = total

	if winner := pickWeightedEntrant(entrants, total); winner != nil {
		bpID, err := pickPatent(ctx, tx, winner, noDupes)
		if err != nil {
			return nil, err
		}
		if bpID != 0 {
			res.Outcome = PatentDrawWon
			res.WinnerCharID = winner.CharID
			res.WinnerTickets = winner.Tickets
			res.Synthetic = winner.Synthetic
			res.SkillTypeID = winner.SkillTypeID
			res.BlueprintID = bpID
		}
	}

	// The history row goes in first: UNIQUE (agentID, roundNo) makes a concurrent draw of the
	// same round collide here instead of granting a second blueprint.
	synth := 0
	if res.Synthetic {
		synth = 1
	}
	hist, err := tx.ExecContext(ctx, `
		INSERT INTO rndPatentDraws (agentID, roundNo, drawnAt, entrants, totalTickets,
			winnerCharID, winnerTickets, skillTypeID, blueprintTypeID, synthetic, outcome)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		agentID, round.RoundNo, nowUnix, res.Entrants, res.TotalTickets,
		res.WinnerCharID, res.WinnerTickets, res.SkillTypeID, res.BlueprintID, synth, res.Outcome)
	if err != nil {
		if isDuplicateKey(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("RunPatentDraw history: %w", err)
	}
	res.DrawID, _ = hist.LastInsertId()

	if res.Outcome == PatentDrawWon {
		res.ExpiresAt = nowUnix + int64(claimDays)*86400
		offer, err := tx.ExecContext(ctx, `
			INSERT INTO rndPatentOffers (charID, agentID, blueprintTypeID, skillTypeID,
				drawID, wonAt, expiresAt, state)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			res.WinnerCharID, agentID, res.BlueprintID, res.SkillTypeID,
			res.DrawID, nowUnix, res.ExpiresAt, PatentOfferPending)
		if err != nil {
			return nil, fmt.Errorf("RunPatentDraw offer: %w", err)
		}
		res.OfferID, _ = offer.LastInsertId()
	}

	if len(entrants) > 0 {
		// The wipe is an UPDATE so the project survives; accrualStart moves with the balance
		// because accrual is lazy (CharacterResearch.CurrentPoints). Bots are included.
		if _, err := tx.ExecContext(ctx,
			`UPDATE characterresearch SET accruedPoints = 0, accrualStart = ? WHERE agentID = ?`,
			nowUnix, agentID); err != nil {
			return nil, fmt.Errorf("RunPatentDraw wipe: %w", err)
		}
		// Advance from now, not drawAt + period, so a long outage costs one draw, not several.
		if _, err := tx.ExecContext(ctx, `
			UPDATE rndAgentRounds SET roundNo = roundNo + 1, startedAt = ?, drawAt = ?
			WHERE agentID = ?`, nowUnix, nowUnix+int64(roundDays)*86400, agentID); err != nil {
			return nil, fmt.Errorf("RunPatentDraw advance: %w", err)
		}
	} else if _, err := tx.ExecContext(ctx,
		`DELETE FROM rndAgentRounds WHERE agentID = ?`, agentID); err != nil {
		// Nobody collaborates with this agent: close the round; StartResearch reopens it on the
		// next enrolment.
		return nil, fmt.Errorf("RunPatentDraw close: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("RunPatentDraw commit: %w", err)
	}
	return res, nil
}

// ForcePatentDraw pulls an agent's round forward to now and draws it (the ESP "Draw now"
// button).
func ForcePatentDraw(ctx context.Context, db *sql.DB, agentID int64, now time.Time) (*PatentDrawResult, error) {
	if _, err := db.ExecContext(ctx,
		`UPDATE rndAgentRounds SET drawAt = ? WHERE agentID = ?`, now.Unix(), agentID); err != nil {
		return nil, fmt.Errorf("ForcePatentDraw: %w", err)
	}
	return RunPatentDraw(ctx, db, agentID, now)
}

// fieldsWithPatents returns which of the entrants' fields actually have a patent
// pool, in one query rather than one per entrant.
func fieldsWithPatents(ctx context.Context, tx *sql.Tx, entrants []*PatentEntrant) (map[int64]bool, error) {
	live := map[int64]bool{}
	seen := map[int64]bool{}
	var ids []any
	var marks []string
	for _, e := range entrants {
		if seen[e.SkillTypeID] {
			continue
		}
		seen[e.SkillTypeID] = true
		ids = append(ids, e.SkillTypeID)
		marks = append(marks, "?")
	}
	if len(ids) == 0 {
		return live, nil
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT skillID, COUNT(*) FROM rndfieldblueprints WHERE skillID IN (`+
			strings.Join(marks, ",")+`) GROUP BY skillID`, ids...)
	if err != nil {
		return nil, fmt.Errorf("fieldsWithPatents: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		if n > 0 {
			live[id] = true
		}
	}
	return live, rows.Err()
}

// pickWeightedEntrant draws one entrant with probability proportional to its
// tickets. Returns nil when nobody holds any.
func pickWeightedEntrant(entrants []*PatentEntrant, total float64) *PatentEntrant {
	if total <= 0 {
		return nil
	}
	roll := rand.Float64() * total
	var acc float64
	for _, e := range entrants {
		if e.Tickets <= 0 {
			continue
		}
		acc += e.Tickets
		if roll < acc {
			return e
		}
	}
	// Float drift on the last bucket: award the last ticket holder rather than void a won round.
	for i := len(entrants) - 1; i >= 0; i-- {
		if entrants[i].Tickets > 0 {
			return entrants[i]
		}
	}
	return nil
}

// pickPatent draws a blueprint from the winner's own field pool, preferring one
// they do not already hold. Returns 0 when the pool is empty (which the
// dead-field filter should already have prevented).
func pickPatent(ctx context.Context, tx *sql.Tx, winner *PatentEntrant, avoidDupes bool) (int64, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT blueprintTypeID FROM rndfieldblueprints WHERE skillID = ?`, winner.SkillTypeID)
	if err != nil {
		return 0, fmt.Errorf("pickPatent pool: %w", err)
	}
	var pool []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		pool = append(pool, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("pickPatent pool: %w", err)
	}
	if len(pool) == 0 {
		return 0, nil
	}

	if avoidDupes {
		marks := make([]string, len(pool))
		args := make([]any, 0, len(pool)+1)
		args = append(args, winner.CharID)
		for i, id := range pool {
			marks[i] = "?"
			args = append(args, id)
		}
		owned := map[int64]bool{}
		dr, err := tx.QueryContext(ctx,
			`SELECT DISTINCT typeID FROM entity WHERE ownerID = ? AND typeID IN (`+
				strings.Join(marks, ",")+`)`, args...)
		if err != nil {
			return 0, fmt.Errorf("pickPatent dupes: %w", err)
		}
		for dr.Next() {
			var id int64
			if err := dr.Scan(&id); err != nil {
				dr.Close()
				return 0, err
			}
			owned[id] = true
		}
		dr.Close()
		if err := dr.Err(); err != nil {
			return 0, fmt.Errorf("pickPatent dupes: %w", err)
		}
		var fresh []int64
		for _, id := range pool {
			if !owned[id] {
				fresh = append(fresh, id)
			}
		}
		// A winner who already holds the whole field still wins something.
		if len(fresh) > 0 {
			pool = fresh
		}
	}
	return pool[rand.Intn(len(pool))], nil
}

// isDuplicateKey reports whether err is a MySQL unique-constraint violation, matched on the
// message to keep db off the driver's typed errors.
func isDuplicateKey(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Error 1062")
}

// ── pending patents ─────────────────────────────────────────────────────────

// GetPendingPatentOffer returns the character's uncollected patent from one agent, or nil.
// Expiry is filtered here, so a lapsed patent simply stops being offered.
func GetPendingPatentOffer(ctx context.Context, db *sql.DB, charID, agentID int64) (*PatentOffer, error) {
	p, err := scanPatentOffer(db.QueryRowContext(ctx, `
		SELECT `+patentOfferCols+` FROM rndPatentOffers
		WHERE charID = ? AND agentID = ? AND state = ? AND expiresAt > ?
		ORDER BY offerID LIMIT 1`,
		charID, agentID, PatentOfferPending, time.Now().Unix()))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("GetPendingPatentOffer: %w", err)
	}
	return p, nil
}

// ListPendingPatentOffersForChar returns every uncollected patent a character
// holds. One query is the whole cost of the rolebot planner's check.
func ListPendingPatentOffersForChar(ctx context.Context, db *sql.DB, charID int64) ([]*PatentOffer, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT `+patentOfferCols+` FROM rndPatentOffers
		WHERE charID = ? AND state = ? AND expiresAt > ?
		ORDER BY expiresAt`, charID, PatentOfferPending, time.Now().Unix())
	if err != nil {
		return nil, fmt.Errorf("ListPendingPatentOffersForChar: %w", err)
	}
	defer rows.Close()
	var out []*PatentOffer
	for rows.Next() {
		p, err := scanPatentOffer(rows)
		if err != nil {
			return nil, fmt.Errorf("ListPendingPatentOffersForChar scan: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetPatentOffer returns one patent row in any state, for the ESP panel.
func GetPatentOffer(ctx context.Context, db *sql.DB, offerID int64) (*PatentOffer, error) {
	p, err := scanPatentOffer(db.QueryRowContext(ctx,
		`SELECT `+patentOfferCols+` FROM rndPatentOffers WHERE offerID = ?`, offerID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("GetPatentOffer: %w", err)
	}
	return p, nil
}

// ClaimPatentOffer creates the BPO in the winner's hangar at the agent's station and closes
// the offer. The station gate lives here so every caller is held to it. No transaction: the
// conditional UPDATE lets exactly one caller move a row out of pending.
// See docs/db-notes.md#patent-claim-and-draw-queries.
func ClaimPatentOffer(ctx context.Context, db *sql.DB, offerID, charID, atStationID int64, now time.Time) (itemID, typeID int64, err error) {
	p, err := GetPatentOffer(ctx, db, offerID)
	if err != nil {
		return 0, 0, err
	}
	if p == nil || p.CharID != charID {
		return 0, 0, fmt.Errorf("patent %d is not yours to collect", offerID)
	}
	if p.State != PatentOfferPending {
		return 0, 0, fmt.Errorf("patent %d has already been dealt with", offerID)
	}
	if p.ExpiresAt <= now.Unix() {
		return 0, 0, fmt.Errorf("patent %d has lapsed", offerID)
	}
	agent, err := GetAgentInfo(ctx, db, p.AgentID)
	if err != nil {
		return 0, 0, err
	}
	if atStationID != agent.StationID {
		return 0, 0, fmt.Errorf("patent %d must be collected at station %d", offerID, agent.StationID)
	}

	res, err := db.ExecContext(ctx,
		`UPDATE rndPatentOffers SET state = ?, claimedAt = ? WHERE offerID = ? AND state = ?`,
		PatentOfferClaimed, now.Unix(), offerID, PatentOfferPending)
	if err != nil {
		return 0, 0, fmt.Errorf("ClaimPatentOffer: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return 0, 0, fmt.Errorf("patent %d has already been collected", offerID)
	}

	// CreateItem gives a category-9 type singleton=1 and EnsureBlueprintRow writes ME 0, PE 0,
	// runs -1.
	itemID, err = SpawnItemInHangar(ctx, db, charID, agent.StationID, p.BlueprintID, 1)
	if err != nil {
		// Hand the patent back rather than losing it to a failed insert.
		_, _ = db.ExecContext(ctx,
			`UPDATE rndPatentOffers SET state = ?, claimedAt = 0 WHERE offerID = ?`,
			PatentOfferPending, offerID)
		return 0, 0, fmt.Errorf("ClaimPatentOffer spawn: %w", err)
	}
	_, _ = db.ExecContext(ctx, `UPDATE rndPatentOffers SET itemID = ? WHERE offerID = ?`, itemID, offerID)
	return itemID, p.BlueprintID, nil
}

// ExpirePatentOffers lapses uncollected patents and returns how many.
func ExpirePatentOffers(ctx context.Context, db *sql.DB, now int64) (int, error) {
	res, err := db.ExecContext(ctx,
		`UPDATE rndPatentOffers SET state = ? WHERE state = ? AND expiresAt <= ?`,
		PatentOfferExpired, PatentOfferPending, now)
	if err != nil {
		return 0, fmt.Errorf("ExpirePatentOffers: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// SetPatentOfferState is the ESP panel's void/restore lever.
func SetPatentOfferState(ctx context.Context, db *sql.DB, offerID int64, state int) error {
	_, err := db.ExecContext(ctx, `UPDATE rndPatentOffers SET state = ? WHERE offerID = ?`, state, offerID)
	if err != nil {
		return fmt.Errorf("SetPatentOfferState: %w", err)
	}
	return nil
}

// ExtendPatentOffer pushes a pending patent's deadline out by days from now and
// returns it to pending if it had already lapsed.
func ExtendPatentOffer(ctx context.Context, db *sql.DB, offerID int64, days int, now time.Time) error {
	if days < 1 {
		days = 1
	}
	_, err := db.ExecContext(ctx, `
		UPDATE rndPatentOffers SET expiresAt = ?, state = ?
		WHERE offerID = ? AND state IN (?, ?)`,
		now.Unix()+int64(days)*86400, PatentOfferPending, offerID,
		PatentOfferPending, PatentOfferExpired)
	if err != nil {
		return fmt.Errorf("ExtendPatentOffer: %w", err)
	}
	return nil
}

// ── lookups for callers outside the dialogue ────────────────────────────────

// ResearchAgentSite is ESPGetFieldAgents's routable sibling: a module planning
// a trip needs coordinates, not display names.
type ResearchAgentSite struct {
	AgentID       int64
	CorporationID int64
	StationID     int64
	SolarSystemID int64
	Level         int
	Quality       int
}

// ResearchAgentsForField returns the R&D agents offering a field, best first.
func ResearchAgentsForField(ctx context.Context, db *sql.DB, skillID int64) ([]ResearchAgentSite, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT r.agentID, COALESCE(a.corporationID, 0), COALESCE(a.stationID, 0),
		       COALESCE(s.solarSystemID, 0), COALESCE(a.level, 1), COALESCE(a.quality, 0)
		FROM agtresearchagents r
		JOIN agtAgents a ON a.agentID = r.agentID
		LEFT JOIN staStations s ON s.stationID = a.stationID
		WHERE r.typeID = ? AND a.stationID IS NOT NULL
		ORDER BY a.level DESC, a.quality DESC`, skillID)
	if err != nil {
		return nil, fmt.Errorf("ResearchAgentsForField: %w", err)
	}
	defer rows.Close()
	var out []ResearchAgentSite
	for rows.Next() {
		var s ResearchAgentSite
		if err := rows.Scan(&s.AgentID, &s.CorporationID, &s.StationID,
			&s.SolarSystemID, &s.Level, &s.Quality); err != nil {
			return nil, fmt.Errorf("ResearchAgentsForField scan: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// NonEmptyResearchFields returns the field skills that have a patent pool, the only fields
// where research can pay out.
func NonEmptyResearchFields(ctx context.Context, db *sql.DB) ([]int64, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT DISTINCT b.skillID FROM rndfieldblueprints b
		JOIN agtresearchagents r ON r.typeID = b.skillID`)
	if err != nil {
		return nil, fmt.Errorf("NonEmptyResearchFields: %w", err)
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

// GetBlueprintProduct returns what a blueprint type builds, or 0. It lets a prize be named
// by its product as well as its own name.
func GetBlueprintProduct(ctx context.Context, db *sql.DB, blueprintTypeID int64) (int64, error) {
	var productID sql.NullInt64
	err := db.QueryRowContext(ctx,
		`SELECT productTypeID FROM invBlueprintTypes WHERE blueprintTypeID = ?`, blueprintTypeID).Scan(&productID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("GetBlueprintProduct: %w", err)
	}
	return productID.Int64, nil
}

// FieldPatentCount returns how many blueprints a field's pool holds.
func FieldPatentCount(ctx context.Context, db *sql.DB, skillID int64) (int, error) {
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM rndfieldblueprints WHERE skillID = ?`, skillID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("FieldPatentCount: %w", err)
	}
	return n, nil
}

// ── ESP browser queries ─────────────────────────────────────────────────────

// ESPAgentRound is one live round for the ESP lottery table. Entrants and
// tickets are the live figures the next draw would use, so the panel and the
// draw cannot disagree about who is in the hat.
type ESPAgentRound struct {
	AgentID      int64
	AgentName    string
	StationName  string
	RoundNo      int
	StartedAt    time.Time
	DrawAt       time.Time
	Entrants     int
	BotEntrants  int
	TotalTickets float64
	DeadFields   int // entrants researching a field with no patent pool
}

// ESPListAgentRounds lists every open round with its live entrant figures.
func ESPListAgentRounds(ctx context.Context, db *sql.DB) ([]ESPAgentRound, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT r.agentID, COALESCE(an.ownerName, ''), COALESCE(s.stationName, ''),
		       r.roundNo, r.startedAt, r.drawAt
		FROM rndAgentRounds r
		LEFT JOIN eveStaticOwners an ON an.ownerID = r.agentID
		LEFT JOIN agtAgents a ON a.agentID = r.agentID
		LEFT JOIN staStations s ON s.stationID = a.stationID
		ORDER BY r.drawAt`)
	if err != nil {
		return nil, fmt.Errorf("ESPListAgentRounds: %w", err)
	}
	defer rows.Close()
	var out []ESPAgentRound
	for rows.Next() {
		var r ESPAgentRound
		var started, draw int64
		if err := rows.Scan(&r.AgentID, &r.AgentName, &r.StationName, &r.RoundNo, &started, &draw); err != nil {
			return nil, fmt.Errorf("ESPListAgentRounds scan: %w", err)
		}
		r.StartedAt = time.Unix(started, 0)
		r.DrawAt = time.Unix(draw, 0)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	botPct := float64(GetPatentBotWeightPct(ctx, db))
	minRP := float64(GetPatentMinTickets(ctx, db))
	now := time.Now()
	for i := range out {
		if err := fillRoundTickets(ctx, db, &out[i], botPct, minRP, now); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// fillRoundTickets computes one round's live entrant figures with the same
// arithmetic RunPatentDraw uses.
func fillRoundTickets(ctx context.Context, db *sql.DB, r *ESPAgentRound, botPct, minRP float64, now time.Time) error {
	rows, err := db.QueryContext(ctx, `
		SELECT cr.charID, cr.skillTypeID, cr.pointsPerDay, cr.accruedPoints, cr.accrualStart,
		       (SELECT COUNT(*) FROM rndfieldblueprints b WHERE b.skillID = cr.skillTypeID)
		FROM characterresearch cr
		JOIN character_ c ON c.characterID = cr.charID
		WHERE cr.agentID = ?`, r.AgentID)
	if err != nil {
		return fmt.Errorf("fillRoundTickets: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var charID, skillID int64
		var pool int
		var cr CharacterResearch
		if err := rows.Scan(&charID, &skillID, &cr.PointsPerDay, &cr.AccruedPoints, &cr.AccrualStart, &pool); err != nil {
			return fmt.Errorf("fillRoundTickets scan: %w", err)
		}
		r.Entrants++
		points := cr.CurrentPoints(now)
		synth := isSyntheticPilot(charID)
		if synth {
			r.BotEntrants++
		}
		if pool == 0 {
			r.DeadFields++
			continue
		}
		if points < minRP {
			continue
		}
		if synth {
			points = points * botPct / 100.0
		}
		r.TotalTickets += points
	}
	return rows.Err()
}

// ESPPatentDraw is one row of the draw log.
type ESPPatentDraw struct {
	DrawID        int64
	AgentID       int64
	AgentName     string
	RoundNo       int
	DrawnAt       time.Time
	Entrants      int
	TotalTickets  float64
	WinnerCharID  int64
	WinnerName    string
	WinnerTickets float64
	Synthetic     bool
	FieldName     string
	BlueprintID   int64
	BlueprintName string
	Outcome       string
}

// ESPListPatentDraws returns the most recent draws, newest first.
func ESPListPatentDraws(ctx context.Context, db *sql.DB, limit int) ([]ESPPatentDraw, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := db.QueryContext(ctx, `
		SELECT d.drawID, d.agentID, COALESCE(an.ownerName, ''), d.roundNo, d.drawnAt,
		       d.entrants, d.totalTickets, d.winnerCharID, COALESCE(c.characterName, ''),
		       d.winnerTickets, d.synthetic, COALESCE(ft.typeName, ''),
		       d.blueprintTypeID, COALESCE(bt.typeName, ''), d.outcome
		FROM rndPatentDraws d
		LEFT JOIN eveStaticOwners an ON an.ownerID = d.agentID
		LEFT JOIN character_ c ON c.characterID = d.winnerCharID
		LEFT JOIN invTypes ft ON ft.typeID = d.skillTypeID
		LEFT JOIN invTypes bt ON bt.typeID = d.blueprintTypeID
		ORDER BY d.drawnAt DESC, d.drawID DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("ESPListPatentDraws: %w", err)
	}
	defer rows.Close()
	var out []ESPPatentDraw
	for rows.Next() {
		var d ESPPatentDraw
		var drawnAt int64
		var synth int
		if err := rows.Scan(&d.DrawID, &d.AgentID, &d.AgentName, &d.RoundNo, &drawnAt,
			&d.Entrants, &d.TotalTickets, &d.WinnerCharID, &d.WinnerName,
			&d.WinnerTickets, &synth, &d.FieldName, &d.BlueprintID, &d.BlueprintName,
			&d.Outcome); err != nil {
			return nil, fmt.Errorf("ESPListPatentDraws scan: %w", err)
		}
		d.DrawnAt = time.Unix(drawnAt, 0)
		d.Synthetic = synth != 0
		out = append(out, d)
	}
	return out, rows.Err()
}

// ESPPendingPatent is one uncollected win for the ESP panel.
type ESPPendingPatent struct {
	OfferID       int64
	CharID        int64
	CharName      string
	AgentID       int64
	AgentName     string
	StationName   string
	BlueprintID   int64
	BlueprintName string
	FieldName     string
	WonAt         time.Time
	ExpiresAt     time.Time
	State         int
	Synthetic     bool
}

// ESPListPendingPatents lists uncollected wins, soonest to lapse first.
// includeClosed also returns claimed/expired/voided rows.
func ESPListPendingPatents(ctx context.Context, db *sql.DB, includeClosed bool, limit int) ([]ESPPendingPatent, error) {
	if limit <= 0 {
		limit = 200
	}
	where := `WHERE o.state = 0`
	if includeClosed {
		where = ``
	}
	rows, err := db.QueryContext(ctx, `
		SELECT o.offerID, o.charID, COALESCE(c.characterName, ''), o.agentID,
		       COALESCE(an.ownerName, ''), COALESCE(s.stationName, ''),
		       o.blueprintTypeID, COALESCE(bt.typeName, ''), COALESCE(ft.typeName, ''),
		       o.wonAt, o.expiresAt, o.state
		FROM rndPatentOffers o
		LEFT JOIN character_ c ON c.characterID = o.charID
		LEFT JOIN eveStaticOwners an ON an.ownerID = o.agentID
		LEFT JOIN agtAgents a ON a.agentID = o.agentID
		LEFT JOIN staStations s ON s.stationID = a.stationID
		LEFT JOIN invTypes bt ON bt.typeID = o.blueprintTypeID
		LEFT JOIN invTypes ft ON ft.typeID = o.skillTypeID
		`+where+`
		ORDER BY o.state, o.expiresAt
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("ESPListPendingPatents: %w", err)
	}
	defer rows.Close()
	var out []ESPPendingPatent
	for rows.Next() {
		var p ESPPendingPatent
		var won, exp int64
		if err := rows.Scan(&p.OfferID, &p.CharID, &p.CharName, &p.AgentID, &p.AgentName,
			&p.StationName, &p.BlueprintID, &p.BlueprintName, &p.FieldName,
			&won, &exp, &p.State); err != nil {
			return nil, fmt.Errorf("ESPListPendingPatents scan: %w", err)
		}
		p.WonAt = time.Unix(won, 0)
		p.ExpiresAt = time.Unix(exp, 0)
		p.Synthetic = isSyntheticPilot(p.CharID)
		out = append(out, p)
	}
	return out, rows.Err()
}

// AddFieldBlueprint puts a blueprint into a field's patent pool. The type must exist in
// invBlueprintTypes; techLevel is not checked, so an operator can add an item the
// invention-chain derivation cannot see.
func AddFieldBlueprint(ctx context.Context, db *sql.DB, skillID, blueprintTypeID int64) error {
	var ok int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM invBlueprintTypes WHERE blueprintTypeID = ?`, blueprintTypeID).Scan(&ok)
	if err != nil {
		return fmt.Errorf("AddFieldBlueprint: %w", err)
	}
	if ok == 0 {
		return fmt.Errorf("type %d is not a blueprint", blueprintTypeID)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT IGNORE INTO rndfieldblueprints (skillID, blueprintTypeID) VALUES (?, ?)`,
		skillID, blueprintTypeID); err != nil {
		return fmt.Errorf("AddFieldBlueprint: %w", err)
	}
	return nil
}

// RemoveFieldBlueprint takes a blueprint out of a field's patent pool.
func RemoveFieldBlueprint(ctx context.Context, db *sql.DB, skillID, blueprintTypeID int64) error {
	_, err := db.ExecContext(ctx,
		`DELETE FROM rndfieldblueprints WHERE skillID = ? AND blueprintTypeID = ?`,
		skillID, blueprintTypeID)
	if err != nil {
		return fmt.Errorf("RemoveFieldBlueprint: %w", err)
	}
	return nil
}
