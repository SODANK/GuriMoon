package db

// Alliance membership, applications and relationships: the state behind the client's Alliance
// window. corporation.allianceID is the source of truth for membership; allianceMembers
// exists for chosenExecutorID, which that column cannot hold. Accepting an application does
// not join the corp; PromoteAcceptedApplications joins it after the delay.
// See docs/db-5-notes.md#alliance-membership-and-applications.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"

	"gurimoon/marshal"
)

// Alliance application states, mirroring const.allianceApplication*.
const (
	AllianceApplicationNew       = 1
	AllianceApplicationAccepted  = 2
	AllianceApplicationEffective = 3
	AllianceApplicationRejected  = 4
)

// Alliance relationship types, mirroring const.allianceRelationship*.
const (
	AllianceRelationshipNAP        = 1
	AllianceRelationshipFriend     = 2
	AllianceRelationshipCompetitor = 3
	AllianceRelationshipEnemy      = 4
)

// AllianceRelationshipName renders a relationship as the client's Standings tab labels it.
func AllianceRelationshipName(rel int64) string {
	switch rel {
	case AllianceRelationshipNAP:
		return "Non-Aggression Pact"
	case AllianceRelationshipFriend:
		return "Friend"
	case AllianceRelationshipCompetitor:
		return "Competitor"
	case AllianceRelationshipEnemy:
		return "Enemy"
	}
	return fmt.Sprintf("Unknown (%d)", rel)
}

// AllianceApplicationStateName renders an application state as the client labels it.
func AllianceApplicationStateName(state int64) string {
	switch state {
	case AllianceApplicationNew:
		return "Applied"
	case AllianceApplicationAccepted:
		return "Accepted"
	case AllianceApplicationEffective:
		return "Effective"
	case AllianceApplicationRejected:
		return "Rejected"
	}
	return fmt.Sprintf("Unknown (%d)", state)
}

// settingAllianceAcceptDelay is the server_settings key for how long an accepted
// application waits before the corporation joins.
const settingAllianceAcceptDelay = "alliance.acceptDelayHours"

// DefaultAllianceAcceptDelayHours matches the client's accept dialog, which raises
// AcceptedApplicationsTake24HoursToBecomeEffective.
const DefaultAllianceAcceptDelayHours = 24.0

// AllianceAcceptDelayHours reads the configured waiting period.
func AllianceAcceptDelayHours(ctx context.Context, db *sql.DB) float64 {
	v, ok, err := GetServerSetting(ctx, db, settingAllianceAcceptDelay)
	if err != nil || !ok {
		return DefaultAllianceAcceptDelayHours
	}
	hours, perr := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if perr != nil || hours < 0 {
		return DefaultAllianceAcceptDelayHours
	}
	return hours
}

// SetAllianceAcceptDelayHours writes the waiting period.
func SetAllianceAcceptDelayHours(ctx context.Context, db *sql.DB, hours float64) error {
	if hours < 0 {
		hours = 0
	}
	return SetServerSetting(ctx, db, settingAllianceAcceptDelay,
		strconv.FormatFloat(hours, 'f', -1, 64))
}

// EnsureAllianceTables creates the alliance side tables. Called from the alliance service
// paths and at start-up.
func EnsureAllianceTables(ctx context.Context, db *sql.DB) error {
	// Read paths call this per RPC (one per ball on undock), so the DDL runs once per process.
	if err := allianceTablesGuard.do(func() error { return ensureAllianceTablesDDL(ctx, db) }); err != nil {
		return err
	}
	// Member rows for corporations linked to an alliance before the table existed. Once per
	// process: the whole-table INSERT...SELECT is not free.
	allianceBackfillOnce.Do(func() {
		if _, err := db.ExecContext(ctx, `
			INSERT IGNORE INTO allianceMembers (allianceID, corporationID, chosenExecutorID, joinedAt)
			SELECT c.allianceID, c.corporationID, 0, 0
			  FROM corporation c
			 WHERE c.allianceID > 0`); err != nil {
			// Not fatal: the tables exist and the next boot retries the backfill.
			fmt.Printf("EnsureAllianceTables backfill: %v\n", err)
		}
		// Drop member rows whose corp left or whose alliance was deleted; they leak another
		// alliance's executor vote into the Members tab.
		_, _ = db.ExecContext(ctx, `
			DELETE m FROM allianceMembers m
			  LEFT JOIN corporation c ON c.corporationID = m.corporationID
			 WHERE c.corporationID IS NULL OR c.allianceID != m.allianceID`)
		// creatorCharID 0 crashes the client's Created By link (GetBloodlineByTypeID KeyError), so
		// alliances made from the ESP panel take the founding corp's CEO.
		_, _ = db.ExecContext(ctx, `
			UPDATE alliances a
			  JOIN corporation c ON c.corporationID = a.creatorCorpID
			   SET a.creatorCharID = c.ceoID
			 WHERE a.creatorCharID = 0 AND c.ceoID > 0`)
	})
	return nil
}

// allianceTablesGuard runs ensureAllianceTablesDDL once per process.
var allianceTablesGuard ensureGuard

func ensureAllianceTablesDDL(ctx context.Context, db *sql.DB) error {
	if err := ensureAlliancesTable(ctx, db); err != nil {
		return err
	}
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS allianceApplications (
		   allianceID          INT UNSIGNED NOT NULL,
		   corporationID       INT UNSIGNED NOT NULL,
		   applicationText     TEXT,
		   applicationDateTime BIGINT       NOT NULL DEFAULT 0,
		   state               INT          NOT NULL DEFAULT 1,
		   stateDateTime       BIGINT       NOT NULL DEFAULT 0,
		   deleted             TINYINT(1)   NOT NULL DEFAULT 0,
		   PRIMARY KEY (allianceID, corporationID),
		   KEY corp (corporationID)
		 ) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`,
		`CREATE TABLE IF NOT EXISTS allianceMembers (
		   allianceID       INT UNSIGNED NOT NULL,
		   corporationID    INT UNSIGNED NOT NULL,
		   chosenExecutorID INT UNSIGNED NOT NULL DEFAULT 0,
		   joinedAt         BIGINT       NOT NULL DEFAULT 0,
		   PRIMARY KEY (corporationID),
		   KEY alliance (allianceID)
		 ) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`,
		`CREATE TABLE IF NOT EXISTS allianceRelationships (
		   allianceID   INT UNSIGNED NOT NULL,
		   toID         INT UNSIGNED NOT NULL,
		   relationship INT          NOT NULL DEFAULT 0,
		   PRIMARY KEY (allianceID, toID)
		 ) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("EnsureAllianceTables: %w", err)
		}
	}
	return nil
}

var allianceBackfillOnce sync.Once

// ── Members ──────────────────────────────────────────────────────────────────

// allianceMemberCols are the columns all_ui_members.py reads off each row.
var allianceMemberCols = []string{"allianceID", "corporationID", "chosenExecutorID"}

// GetAllianceMembersRowset returns the member corporations as a util.IndexRowset keyed by
// corporationID (the client calls GetMembers()[corporationID]). chosenExecutorID goes out as
// None for no declared support: the Members tab prints "Secret" for it, and a 0 would render
// as an unresolvable owner.
func GetAllianceMembersRowset(ctx context.Context, db *sql.DB, allianceID int64) (marshal.PyRep, error) {
	if err := EnsureAllianceTables(ctx, db); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `
		SELECT c.corporationID, COALESCE(m.chosenExecutorID, 0)
		  FROM corporation c
		  LEFT JOIN allianceMembers m ON m.corporationID = c.corporationID
		 WHERE c.allianceID = ?
		 ORDER BY c.corporationID`, allianceID)
	if err != nil {
		return nil, fmt.Errorf("GetAllianceMembersRowset: %w", err)
	}
	defer rows.Close()

	var out [][]marshal.PyRep
	for rows.Next() {
		var corpID, chosen int64
		if err := rows.Scan(&corpID, &chosen); err != nil {
			return nil, fmt.Errorf("GetAllianceMembersRowset scan: %w", err)
		}
		chosenRep := marshal.PyRep(marshal.PyNone{})
		if chosen > 0 {
			chosenRep = intRep(chosen)
		}
		out = append(out, []marshal.PyRep{intRep(allianceID), intRep(corpID), chosenRep})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return makeIndexRowset("corporationID", allianceMemberCols, out), nil
}

// AllianceMember is one member corporation, for the panel.
type AllianceMember struct {
	CorporationID    int64
	CorporationName  string
	TickerName       string
	ChosenExecutorID int64
	ChosenName       string
	Pilots           int64
	IsExecutor       bool
}

// ListAllianceMembers returns the member corporations with executor support resolved.
func ListAllianceMembers(ctx context.Context, db *sql.DB, allianceID int64) ([]AllianceMember, error) {
	if err := EnsureAllianceTables(ctx, db); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `
		SELECT c.corporationID, c.corporationName, c.tickerName,
		       COALESCE(m.chosenExecutorID, 0), COALESCE(e.corporationName, ''),
		       (SELECT COUNT(*) FROM character_ ch
		         WHERE ch.corporationID = c.corporationID AND ch.accountID > 0),
		       (a.executorCorpID = c.corporationID)
		  FROM corporation c
		  JOIN alliances a ON a.allianceID = c.allianceID
		  LEFT JOIN allianceMembers m ON m.corporationID = c.corporationID
		  LEFT JOIN corporation e ON e.corporationID = m.chosenExecutorID
		 WHERE c.allianceID = ?
		 ORDER BY c.corporationName`, allianceID)
	if err != nil {
		return nil, fmt.Errorf("ListAllianceMembers: %w", err)
	}
	defer rows.Close()

	var out []AllianceMember
	for rows.Next() {
		var m AllianceMember
		var isExec int64
		if err := rows.Scan(&m.CorporationID, &m.CorporationName, &m.TickerName,
			&m.ChosenExecutorID, &m.ChosenName, &m.Pilots, &isExec); err != nil {
			return nil, fmt.Errorf("ListAllianceMembers scan: %w", err)
		}
		m.IsExecutor = isExec != 0
		out = append(out, m)
	}
	return out, rows.Err()
}

// JoinAlliance links a corporation to an alliance and gives it a member row.
// Returns the alliance's executor after the join.
func JoinAlliance(ctx context.Context, db *sql.DB, corpID, allianceID int64) (int64, error) {
	if err := EnsureAllianceTables(ctx, db); err != nil {
		return 0, err
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE corporation SET allianceID = ? WHERE corporationID = ?`,
		allianceID, corpID); err != nil {
		return 0, fmt.Errorf("JoinAlliance link: %w", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO allianceMembers (allianceID, corporationID, chosenExecutorID, joinedAt)
		VALUES (?, ?, 0, ?)
		ON DUPLICATE KEY UPDATE allianceID = VALUES(allianceID), joinedAt = VALUES(joinedAt),
		                        chosenExecutorID = 0`,
		allianceID, corpID, win32TimeNow()); err != nil {
		return 0, fmt.Errorf("JoinAlliance member row: %w", err)
	}
	_ = ESPRecountAlliance(ctx, db, allianceID)
	corpIdentityChanged(ctx, db, corpID)
	return RecomputeAllianceExecutor(ctx, db, allianceID)
}

// LeaveAlliance removes a corporation from its alliance. Returns the alliance it left and
// that alliance's executor afterwards.
func LeaveAlliance(ctx context.Context, db *sql.DB, corpID int64) (allianceID, executorID int64, err error) {
	if err := EnsureAllianceTables(ctx, db); err != nil {
		return 0, 0, err
	}
	if err := db.QueryRowContext(ctx,
		`SELECT allianceID FROM corporation WHERE corporationID = ?`, corpID,
	).Scan(&allianceID); err != nil {
		return 0, 0, fmt.Errorf("LeaveAlliance read: %w", err)
	}
	if allianceID == 0 {
		return 0, 0, nil
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE corporation SET allianceID = 0 WHERE corporationID = ?`, corpID); err != nil {
		return allianceID, 0, fmt.Errorf("LeaveAlliance unlink: %w", err)
	}
	if _, err := db.ExecContext(ctx,
		`DELETE FROM allianceMembers WHERE corporationID = ?`, corpID); err != nil {
		return allianceID, 0, fmt.Errorf("LeaveAlliance member row: %w", err)
	}
	// Votes cast for the leaver are cleared; nothing else clears them.
	if _, err := db.ExecContext(ctx,
		`UPDATE allianceMembers SET chosenExecutorID = 0
		  WHERE allianceID = ? AND chosenExecutorID = ?`, allianceID, corpID); err != nil {
		return allianceID, 0, fmt.Errorf("LeaveAlliance stale votes: %w", err)
	}
	// A leaving corp takes its application with it.
	if _, err := db.ExecContext(ctx,
		`DELETE FROM allianceApplications WHERE allianceID = ? AND corporationID = ?`,
		allianceID, corpID); err != nil {
		return allianceID, 0, fmt.Errorf("LeaveAlliance application: %w", err)
	}
	_ = ESPRecountAlliance(ctx, db, allianceID)
	executorID, err = RecomputeAllianceExecutor(ctx, db, allianceID)
	corpIdentityChanged(ctx, db, corpID)
	return allianceID, executorID, err
}

// DeclareExecutorSupport records which corporation a member backs for executor and returns
// the executor that produces.
func DeclareExecutorSupport(ctx context.Context, db *sql.DB, allianceID, corpID, supportedCorpID int64) (int64, error) {
	if err := EnsureAllianceTables(ctx, db); err != nil {
		return 0, err
	}
	var inAlliance int64
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM corporation WHERE corporationID = ? AND allianceID = ?`,
		supportedCorpID, allianceID).Scan(&inAlliance); err != nil {
		return 0, fmt.Errorf("DeclareExecutorSupport check: %w", err)
	}
	if inAlliance == 0 {
		return 0, fmt.Errorf("corporation %d is not a member of this alliance", supportedCorpID)
	}
	// The voter must be a member too, or the upsert writes a member row for a corp that is
	// elsewhere.
	var voterIn int64
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM corporation WHERE corporationID = ? AND allianceID = ?`,
		corpID, allianceID).Scan(&voterIn); err != nil {
		return 0, fmt.Errorf("DeclareExecutorSupport voter check: %w", err)
	}
	if voterIn == 0 {
		return 0, fmt.Errorf("the voting corporation is not a member of this alliance")
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO allianceMembers (allianceID, corporationID, chosenExecutorID, joinedAt)
		VALUES (?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE chosenExecutorID = VALUES(chosenExecutorID)`,
		allianceID, corpID, supportedCorpID, win32TimeNow()); err != nil {
		return 0, fmt.Errorf("DeclareExecutorSupport: %w", err)
	}
	return RecomputeAllianceExecutor(ctx, db, allianceID)
}

// ClearExecutorSupport drops a member's vote. The Members tab shows no vote as "Secret",
// which differs from backing nobody in particular.
func ClearExecutorSupport(ctx context.Context, db *sql.DB, corpID int64) error {
	if _, err := db.ExecContext(ctx,
		`UPDATE allianceMembers SET chosenExecutorID = 0 WHERE corporationID = ?`, corpID); err != nil {
		return fmt.Errorf("ClearExecutorSupport: %w", err)
	}
	return nil
}

// RecomputeAllianceExecutor picks the executor from the members' declared support, writes
// alliances.executorCorpID and returns it. One corp, one vote; the incumbent keeps the seat on
// a tie or with no votes. A dictatorial alliance is not counted.
// See docs/db-5-notes.md#alliance-membership-and-applications.
func RecomputeAllianceExecutor(ctx context.Context, db *sql.DB, allianceID int64) (int64, error) {
	var (
		incumbent   int64
		dictatorial int64
	)
	if err := db.QueryRowContext(ctx,
		`SELECT executorCorpID, dictatorial FROM alliances WHERE allianceID = ?`, allianceID,
	).Scan(&incumbent, &dictatorial); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("RecomputeAllianceExecutor read: %w", err)
	}
	if dictatorial != 0 {
		// The dictator keeps the seat only while their corporation is a member; otherwise the
		// alliance could never be administered again.
		var stillMember int64
		if incumbent > 0 {
			_ = db.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM corporation WHERE corporationID = ? AND allianceID = ?`,
				incumbent, allianceID).Scan(&stillMember)
		}
		if stillMember > 0 {
			return incumbent, nil
		}
		// Fall through to the election and fallback below.
	}

	rows, err := db.QueryContext(ctx, `
		SELECT m.chosenExecutorID, COUNT(*) AS votes
		  FROM allianceMembers m
		  JOIN corporation c ON c.corporationID = m.corporationID AND c.allianceID = m.allianceID
		  JOIN corporation v ON v.corporationID = m.chosenExecutorID AND v.allianceID = m.allianceID
		 WHERE m.allianceID = ? AND m.chosenExecutorID > 0
		 GROUP BY m.chosenExecutorID
		 ORDER BY votes DESC, m.chosenExecutorID ASC`, allianceID)
	if err != nil {
		return incumbent, fmt.Errorf("RecomputeAllianceExecutor votes: %w", err)
	}
	defer rows.Close()

	var (
		winner         int64
		topVotes       int64
		tied           bool
		incumbentVotes int64
	)
	for rows.Next() {
		var corpID, votes int64
		if err := rows.Scan(&corpID, &votes); err != nil {
			return incumbent, fmt.Errorf("RecomputeAllianceExecutor scan: %w", err)
		}
		if corpID == incumbent {
			incumbentVotes = votes
		}
		switch {
		case winner == 0:
			winner, topVotes = corpID, votes
		case votes == topVotes:
			tied = true
		}
	}
	if err := rows.Err(); err != nil {
		return incumbent, err
	}

	executor := winner
	switch {
	case winner == 0:
		executor = incumbent
	case tied && incumbentVotes == topVotes:
		executor = incumbent
	}
	// The seat must belong to a member.
	if executor > 0 {
		var member int64
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM corporation WHERE corporationID = ? AND allianceID = ?`,
			executor, allianceID).Scan(&member); err == nil && member == 0 {
			executor = 0
		}
	}
	if executor == 0 {
		// Fall back to the longest-standing member.
		_ = db.QueryRowContext(ctx, `
			SELECT c.corporationID FROM corporation c
			  LEFT JOIN allianceMembers m ON m.corporationID = c.corporationID
			 WHERE c.allianceID = ?
			 ORDER BY COALESCE(m.joinedAt, 0) ASC, c.corporationID ASC
			 LIMIT 1`, allianceID).Scan(&executor)
	}
	if executor != incumbent {
		if _, err := db.ExecContext(ctx,
			`UPDATE alliances SET executorCorpID = ? WHERE allianceID = ?`,
			executor, allianceID); err != nil {
			return incumbent, fmt.Errorf("RecomputeAllianceExecutor write: %w", err)
		}
	}
	return executor, nil
}

// ── Applications ─────────────────────────────────────────────────────────────

// allianceApplicationCols are the columns all_ui_applications.py reads.
var allianceApplicationCols = []string{
	"allianceID", "corporationID", "applicationText", "applicationDateTime", "state",
}

func scanApplications(rows *sql.Rows) ([][]marshal.PyRep, error) {
	defer rows.Close()
	var out [][]marshal.PyRep
	for rows.Next() {
		var (
			allianceID, corpID, when, state int64
			text                            sql.NullString
		)
		if err := rows.Scan(&allianceID, &corpID, &text, &when, &state); err != nil {
			return nil, fmt.Errorf("scanApplications: %w", err)
		}
		out = append(out, []marshal.PyRep{
			intRep(allianceID), intRep(corpID), nullStrRep(text), intRep(when), intRep(state),
		})
	}
	return out, rows.Err()
}

// GetAllianceApplicationsRowset returns the applications made to an alliance, keyed by
// corporationID (the alliance side of the Applications tab).
func GetAllianceApplicationsRowset(ctx context.Context, db *sql.DB, allianceID int64) (marshal.PyRep, error) {
	if err := EnsureAllianceTables(ctx, db); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `
		SELECT allianceID, corporationID, applicationText, applicationDateTime, state
		  FROM allianceApplications
		 WHERE allianceID = ? AND deleted = 0`, allianceID)
	if err != nil {
		return nil, fmt.Errorf("GetAllianceApplicationsRowset: %w", err)
	}
	lines, err := scanApplications(rows)
	if err != nil {
		return nil, err
	}
	return makeIndexRowset("corporationID", allianceApplicationCols, lines), nil
}

// GetCorpAllianceApplicationsRowset returns the applications a corporation has made, keyed by
// allianceID (the My Applications tab, reached through corpRegistry).
func GetCorpAllianceApplicationsRowset(ctx context.Context, db *sql.DB, corpID int64) (marshal.PyRep, error) {
	if err := EnsureAllianceTables(ctx, db); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `
		SELECT allianceID, corporationID, applicationText, applicationDateTime, state
		  FROM allianceApplications
		 WHERE corporationID = ? AND deleted = 0`, corpID)
	if err != nil {
		return nil, fmt.Errorf("GetCorpAllianceApplicationsRowset: %w", err)
	}
	lines, err := scanApplications(rows)
	if err != nil {
		return nil, err
	}
	return makeIndexRowset("allianceID", allianceApplicationCols, lines), nil
}

// AllianceApplication is one application row for the panel and the change pushes.
type AllianceApplication struct {
	AllianceID      int64
	AllianceName    string
	CorporationID   int64
	CorporationName string
	ApplicationText string
	AppliedAt       int64
	State           int64
}

// StateName renders the state the way the client labels it.
func (a AllianceApplication) StateName() string { return AllianceApplicationStateName(a.State) }

// ListAllianceApplications returns an alliance's applications for the panel.
func ListAllianceApplications(ctx context.Context, db *sql.DB, allianceID int64) ([]AllianceApplication, error) {
	if err := EnsureAllianceTables(ctx, db); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `
		SELECT a.allianceID, COALESCE(al.allianceName, ''), a.corporationID,
		       COALESCE(c.corporationName, ''), COALESCE(a.applicationText, ''),
		       a.applicationDateTime, a.state
		  FROM allianceApplications a
		  LEFT JOIN alliances al ON al.allianceID = a.allianceID
		  LEFT JOIN corporation c ON c.corporationID = a.corporationID
		 WHERE a.allianceID = ? AND a.deleted = 0
		 ORDER BY a.state, a.applicationDateTime DESC`, allianceID)
	if err != nil {
		return nil, fmt.Errorf("ListAllianceApplications: %w", err)
	}
	defer rows.Close()
	var out []AllianceApplication
	for rows.Next() {
		var a AllianceApplication
		if err := rows.Scan(&a.AllianceID, &a.AllianceName, &a.CorporationID,
			&a.CorporationName, &a.ApplicationText, &a.AppliedAt, &a.State); err != nil {
			return nil, fmt.Errorf("ListAllianceApplications scan: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetAllianceApplication reads one application.
func GetAllianceApplication(ctx context.Context, db *sql.DB, allianceID, corpID int64) (*AllianceApplication, error) {
	a := &AllianceApplication{AllianceID: allianceID, CorporationID: corpID}
	err := db.QueryRowContext(ctx, `
		SELECT COALESCE(applicationText, ''), applicationDateTime, state
		  FROM allianceApplications
		 WHERE allianceID = ? AND corporationID = ? AND deleted = 0`,
		allianceID, corpID).Scan(&a.ApplicationText, &a.AppliedAt, &a.State)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("GetAllianceApplication: %w", err)
	}
	return a, nil
}

// ApplyToJoinAlliance records a corporation's application after AllianceApplyRefusal
// (alliancerules.go) passes.
func ApplyToJoinAlliance(ctx context.Context, db *sql.DB, corpID, allianceID int64, text string) error {
	if err := AllianceApplyRefusal(ctx, db, corpID, allianceID); err != nil {
		return err
	}
	if len(text) > 1000 {
		text = text[:1000] // the client's dialog caps the box at 1000
	}
	now := win32TimeNow()
	_, err := db.ExecContext(ctx, `
		INSERT INTO allianceApplications
		  (allianceID, corporationID, applicationText, applicationDateTime, state, stateDateTime, deleted)
		VALUES (?, ?, ?, ?, ?, ?, 0)
		ON DUPLICATE KEY UPDATE applicationText = VALUES(applicationText),
		                        applicationDateTime = VALUES(applicationDateTime),
		                        state = VALUES(state),
		                        stateDateTime = VALUES(stateDateTime),
		                        deleted = 0`,
		allianceID, corpID, text, now, AllianceApplicationNew, now)
	if err != nil {
		return fmt.Errorf("ApplyToJoinAlliance: %w", err)
	}
	return nil
}

// DeleteAllianceApplication withdraws an application.
func DeleteAllianceApplication(ctx context.Context, db *sql.DB, corpID, allianceID int64) error {
	if err := EnsureAllianceTables(ctx, db); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx,
		`DELETE FROM allianceApplications WHERE allianceID = ? AND corporationID = ?`,
		allianceID, corpID)
	if err != nil {
		return fmt.Errorf("DeleteAllianceApplication: %w", err)
	}
	return nil
}

// UpdateAllianceApplication answers an application. Only New applications can be answered,
// and only with Accepted or Rejected, the two states the client dialog offers; another state
// would strand the row.
func UpdateAllianceApplication(ctx context.Context, db *sql.DB, allianceID, corpID int64, text string, state int64) error {
	if err := EnsureAllianceTables(ctx, db); err != nil {
		return err
	}
	if state != AllianceApplicationAccepted && state != AllianceApplicationRejected {
		return fmt.Errorf("an application can only be accepted or rejected")
	}
	app, err := GetAllianceApplication(ctx, db, allianceID, corpID)
	if err != nil {
		return err
	}
	if app == nil {
		return fmt.Errorf("no such application")
	}
	if app.State != AllianceApplicationNew {
		return fmt.Errorf("this application has already been answered")
	}
	if len(text) > 1000 {
		text = text[:1000]
	}
	if _, err := db.ExecContext(ctx, `
		UPDATE allianceApplications
		   SET state = ?, stateDateTime = ?, applicationText = ?
		 WHERE allianceID = ? AND corporationID = ?`,
		state, win32TimeNow(), text, allianceID, corpID); err != nil {
		return fmt.Errorf("UpdateAllianceApplication: %w", err)
	}
	return nil
}

// ForceApplicationDue makes an accepted application due now, so the next sweep completes
// the join and pushes the member change and the corp's session value.
func ForceApplicationDue(ctx context.Context, db *sql.DB, allianceID, corpID int64) error {
	res, err := db.ExecContext(ctx, `
		UPDATE allianceApplications SET stateDateTime = 0
		 WHERE allianceID = ? AND corporationID = ? AND state = ? AND deleted = 0`,
		allianceID, corpID, AllianceApplicationAccepted)
	if err != nil {
		return fmt.Errorf("ForceApplicationDue: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("only an accepted application can be made effective")
	}
	return nil
}

// PromotedApplication is one application PromoteAcceptedApplications moved from Accepted to
// Effective, with the join it performed.
type PromotedApplication struct {
	AllianceID     int64
	CorporationID  int64
	ExecutorCorpID int64
	// Wars declared against the corporation and handed to the alliance at the join
	// (AdoptCorpWars); the caller pushes the OnWarChanged pairs.
	Adopted []WarAdoption
}

// RejectedApplication is one acceptance the sweep turned into a rejection: the corporation
// joined something else, or went to war as declarer or with that alliance, while it waited.
type RejectedApplication struct {
	AllianceID    int64
	CorporationID int64
}

// PromoteAcceptedApplications completes every acceptance whose delayHours has elapsed: the
// corporation joins and the application becomes Effective. Applications that no longer
// qualify are rejected and returned second.
func PromoteAcceptedApplications(ctx context.Context, db *sql.DB, delayHours float64) ([]PromotedApplication, []RejectedApplication, error) {
	if err := EnsureAllianceTables(ctx, db); err != nil {
		return nil, nil, err
	}
	cutoff := win32TimeNow() - uint64(delayHours*3600*10000000)
	rows, err := db.QueryContext(ctx, `
		SELECT allianceID, corporationID
		  FROM allianceApplications
		 WHERE state = ? AND deleted = 0 AND stateDateTime <= ?`,
		AllianceApplicationAccepted, cutoff)
	if err != nil {
		return nil, nil, fmt.Errorf("PromoteAcceptedApplications: %w", err)
	}
	type pair struct{ allianceID, corpID int64 }
	var due []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.allianceID, &p.corpID); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("PromoteAcceptedApplications scan: %w", err)
		}
		due = append(due, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	var out []PromotedApplication
	var rejectedOut []RejectedApplication
	for _, p := range due {
		// The corp may have joined another alliance while it waited; two acceptances can sit in one
		// batch, and the first to promote takes the corp.
		var currentAlliance int64
		if err := db.QueryRowContext(ctx,
			`SELECT allianceID FROM corporation WHERE corporationID = ?`, p.corpID,
		).Scan(&currentAlliance); err != nil {
			log.Printf("alliance application sweep: corp %d: %v", p.corpID, err)
			continue
		}
		// The war gate is re-run at the join: a war declared during the wait would ride in or put
		// the alliance at war with its own member.
		if currentAlliance != 0 || allianceJoinWarRefusal(ctx, db, p.corpID, p.allianceID) != nil {
			if _, err := db.ExecContext(ctx,
				`UPDATE allianceApplications SET state = ?, stateDateTime = ?
				  WHERE allianceID = ? AND corporationID = ? AND state = ?`,
				AllianceApplicationRejected, win32TimeNow(), p.allianceID, p.corpID,
				AllianceApplicationAccepted); err == nil {
				rejectedOut = append(rejectedOut, RejectedApplication{
					AllianceID: p.allianceID, CorporationID: p.corpID,
				})
			}
			continue
		}
		executor, err := JoinAlliance(ctx, db, p.corpID, p.allianceID)
		if err != nil {
			log.Printf("alliance application sweep: join corp %d -> %d: %v", p.corpID, p.allianceID, err)
			continue
		}
		if _, err := db.ExecContext(ctx,
			`UPDATE allianceApplications SET state = ?, stateDateTime = ?
			  WHERE allianceID = ? AND corporationID = ? AND state = ?`,
			AllianceApplicationEffective, win32TimeNow(), p.allianceID, p.corpID,
			AllianceApplicationAccepted); err != nil {
			log.Printf("alliance application sweep: mark effective %d/%d: %v", p.allianceID, p.corpID, err)
			continue
		}
		adopted, err := AdoptCorpWars(ctx, db, p.corpID, p.allianceID)
		if err != nil {
			log.Printf("alliance application sweep: adopt wars corp %d -> %d: %v", p.corpID, p.allianceID, err)
		}
		out = append(out, PromotedApplication{
			AllianceID: p.allianceID, CorporationID: p.corpID, ExecutorCorpID: executor,
			Adopted: adopted,
		})
	}
	return out, rejectedOut, nil
}

// ── Relationships ────────────────────────────────────────────────────────────

// allianceRelationshipCols are the columns all_ui_relationships.py reads.
var allianceRelationshipCols = []string{"allianceID", "toID", "relationship"}

// GetAllianceRelationshipsRowset returns an alliance's declared relationships keyed by toID.
func GetAllianceRelationshipsRowset(ctx context.Context, db *sql.DB, allianceID int64) (marshal.PyRep, error) {
	if err := EnsureAllianceTables(ctx, db); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx,
		`SELECT toID, relationship FROM allianceRelationships WHERE allianceID = ?`, allianceID)
	if err != nil {
		return nil, fmt.Errorf("GetAllianceRelationshipsRowset: %w", err)
	}
	defer rows.Close()
	var out [][]marshal.PyRep
	for rows.Next() {
		var toID, rel int64
		if err := rows.Scan(&toID, &rel); err != nil {
			return nil, fmt.Errorf("GetAllianceRelationshipsRowset scan: %w", err)
		}
		out = append(out, []marshal.PyRep{intRep(allianceID), intRep(toID), intRep(rel)})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return makeIndexRowset("toID", allianceRelationshipCols, out), nil
}

// AllianceRelationship is one declared relationship, for the panel.
type AllianceRelationship struct {
	ToID         int64
	ToName       string
	Relationship int64
}

// TypeName renders the relationship as the client's tab labels it.
func (r AllianceRelationship) TypeName() string { return AllianceRelationshipName(r.Relationship) }

// ListAllianceRelationships returns the declared relationships with the other party's name.
func ListAllianceRelationships(ctx context.Context, db *sql.DB, allianceID int64) ([]AllianceRelationship, error) {
	if err := EnsureAllianceTables(ctx, db); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `
		SELECT r.toID, r.relationship, COALESCE(o.ownerName, n.itemName, '')
		  FROM allianceRelationships r
		  LEFT JOIN eveStaticOwners o ON o.ownerID = r.toID
		  LEFT JOIN eveNames n ON n.itemID = r.toID
		 WHERE r.allianceID = ?
		 ORDER BY r.relationship, r.toID`, allianceID)
	if err != nil {
		return nil, fmt.Errorf("ListAllianceRelationships: %w", err)
	}
	defer rows.Close()
	var out []AllianceRelationship
	for rows.Next() {
		var r AllianceRelationship
		if err := rows.Scan(&r.ToID, &r.Relationship, &r.ToName); err != nil {
			return nil, fmt.Errorf("ListAllianceRelationships scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetAllianceRelationship declares a relationship towards another owner.
func SetAllianceRelationship(ctx context.Context, db *sql.DB, allianceID, toID, relationship int64) error {
	if err := EnsureAllianceTables(ctx, db); err != nil {
		return err
	}
	switch relationship {
	case AllianceRelationshipNAP, AllianceRelationshipFriend,
		AllianceRelationshipCompetitor, AllianceRelationshipEnemy:
	default:
		return fmt.Errorf("unknown relationship type %d", relationship)
	}
	if toID == allianceID {
		return fmt.Errorf("an alliance cannot declare a relationship with itself")
	}
	// toID must resolve to an owner: the client renders each row through cfg.eveowners.Get(toID),
	// and an unresolvable ID blanks that Standings sub-tab for every member.
	var known int64
	if err := db.QueryRowContext(ctx, `
		SELECT (SELECT COUNT(*) FROM corporation WHERE corporationID = ?)
		     + (SELECT COUNT(*) FROM alliances WHERE allianceID = ? AND deleted = 0)`,
		toID, toID).Scan(&known); err != nil {
		return fmt.Errorf("SetAllianceRelationship lookup: %w", err)
	}
	if known == 0 {
		return fmt.Errorf("%d is not a corporation or alliance", toID)
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO allianceRelationships (allianceID, toID, relationship)
		VALUES (?, ?, ?)
		ON DUPLICATE KEY UPDATE relationship = VALUES(relationship)`,
		allianceID, toID, relationship)
	if err != nil {
		return fmt.Errorf("SetAllianceRelationship: %w", err)
	}
	allianceRelationshipsChanged(allianceID)
	return nil
}

// AllianceRelationshipsChanged is told when an alliance's allianceRelationships rows change,
// so the memoised alliance.GetRelationships reply is dropped. allianceID 0 means rows of
// several alliances changed. Installed by main; nil does nothing.
var AllianceRelationshipsChanged func(allianceID int64)

func allianceRelationshipsChanged(allianceID int64) {
	if hook := AllianceRelationshipsChanged; hook != nil {
		hook(allianceID)
	}
}

// DeleteAllianceRelationship drops a declared relationship.
func DeleteAllianceRelationship(ctx context.Context, db *sql.DB, allianceID, toID int64) error {
	if err := EnsureAllianceTables(ctx, db); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx,
		`DELETE FROM allianceRelationships WHERE allianceID = ? AND toID = ?`, allianceID, toID)
	if err != nil {
		return fmt.Errorf("DeleteAllianceRelationship: %w", err)
	}
	allianceRelationshipsChanged(allianceID)
	return nil
}

// ── Costs ────────────────────────────────────────────────────────────────────

// AllianceCreationCost is const.allianceCreationCost; the client quotes this number.
const AllianceCreationCost = 1000000000.0

// ChargeAllianceCreation takes the creation fee out of the founding corporation's wallet and
// writes the corp journal entry (refType 48, Alliance Registration Fee). The error can be
// handed to the player when the corp cannot afford it.
func ChargeAllianceCreation(ctx context.Context, db *sql.DB, corpID, charID int64) error {
	balance, err := GetCorpBalance(ctx, db, corpID)
	if err != nil {
		return err
	}
	if balance < AllianceCreationCost {
		return fmt.Errorf("your corporation needs %.0f ISK to found an alliance and has %.2f",
			AllianceCreationCost, balance)
	}
	if err := CreditCorpWallet(ctx, db, corpID, -AllianceCreationCost); err != nil {
		return err
	}
	newBalance := balance - AllianceCreationCost
	// refAllianceRegistrationFee = 48; ownerID2 is the Secure Commerce Commission. A corp wallet
	// journal row is a market_journal row whose characterID is the corporation.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO market_journal
		  (characterID, transDate, refTypeID, ownerID1, ownerID2, argID1,
		   accountID, amount, balance, reason, accountKey)
		VALUES (?, ?, 48, ?, 1000132, NULL, 0, ?, ?, ?, 1000)`,
		corpID, win32TimeNow(), corpID,
		-AllianceCreationCost, newBalance, "Alliance registration fee"); err != nil {
		return fmt.Errorf("ChargeAllianceCreation journal: %w", err)
	}
	return nil
}

// RefundAllianceCreation reverses ChargeAllianceCreation after a create that failed past the debit.
func RefundAllianceCreation(ctx context.Context, db *sql.DB, corpID int64) error {
	if err := CreditCorpWallet(ctx, db, corpID, AllianceCreationCost); err != nil {
		return err
	}
	balance, _ := GetCorpBalance(ctx, db, corpID)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO market_journal
		  (characterID, transDate, refTypeID, ownerID1, ownerID2, argID1,
		   accountID, amount, balance, reason, accountKey)
		VALUES (?, ?, 48, 1000132, ?, NULL, 0, ?, ?, ?, 1000)`,
		corpID, win32TimeNow(), corpID,
		AllianceCreationCost, balance, "Alliance registration fee refund"); err != nil {
		return fmt.Errorf("RefundAllianceCreation journal: %w", err)
	}
	return nil
}

// ── Details ──────────────────────────────────────────────────────────────────

// UpdateAllianceDetails writes the description and url edited in the Home tab and returns
// the previous values for the change push.
func UpdateAllianceDetails(ctx context.Context, db *sql.DB, allianceID int64, description, url string) (oldDesc, oldURL string, err error) {
	var d, u sql.NullString
	if err = db.QueryRowContext(ctx,
		`SELECT description, url FROM alliances WHERE allianceID = ?`, allianceID,
	).Scan(&d, &u); err != nil {
		return "", "", fmt.Errorf("UpdateAllianceDetails read: %w", err)
	}
	if _, err = db.ExecContext(ctx,
		`UPDATE alliances SET description = ?, url = ? WHERE allianceID = ?`,
		description, url, allianceID); err != nil {
		return "", "", fmt.Errorf("UpdateAllianceDetails: %w", err)
	}
	return d.String, u.String, nil
}

// SuggestedAllianceShortNames offers ticker candidates for a name, as SuggestedTickers does
// for corporations, for the short-name box of the client's create form.
func SuggestedAllianceShortNames(ctx context.Context, db *sql.DB, allianceName string) []string {
	words := strings.Fields(strings.ToUpper(allianceName))
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if len(s) < 2 || len(s) > 5 {
			return
		}
		for _, e := range out {
			if e == s {
				return
			}
		}
		var taken int64
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM alliances WHERE shortName = ?`, s).Scan(&taken); err != nil || taken > 0 {
			return
		}
		out = append(out, s)
	}
	// Initials first ("Wrecking Crew Alliance" -> "WCA"), then prefixes.
	var initials strings.Builder
	for _, w := range words {
		initials.WriteByte(w[0])
	}
	add(initials.String())
	for _, w := range words {
		if len(w) >= 5 {
			add(w[:5])
		}
		if len(w) >= 4 {
			add(w[:4])
		}
		if len(w) >= 3 {
			add(w[:3])
		}
	}
	stripped := strings.Map(func(r rune) rune {
		if r == ' ' {
			return -1
		}
		return r
	}, strings.ToUpper(allianceName))
	if len(stripped) >= 5 {
		add(stripped[:5])
	}
	return out
}
