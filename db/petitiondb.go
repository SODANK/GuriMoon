package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Petitions: the in-game help desk (ui/shared/neocom/petition.py). A pilot
// files one against a category; it lands in the category's queue; a GM
// claims it, talks to the pilot through petition messages (comments are
// GM-only notes), escalates it to the senior queue or closes it; the pilot
// may cancel an unclaimed one and delete a closed one. Times are unix
// seconds in the tables and go to the client as win32 filetimes.

const (
	PetitionQueueGM       = 1
	PetitionQueueSeniorGM = 2

	// MaxPetitionsPerDay is const.maxPetitionsPerDay (eveConstants): the
	// client's own -2 refusal names the number.
	MaxPetitionsPerDay = 8
)

var ErrPetitionClosed = errors.New("petition is closed")

type Petition struct {
	PetitionID   int64
	PetitionerID int64
	OocCharID    int64
	AccountID    int64
	Subject      string
	Text         string
	CategoryID   int64
	Properties   map[string]string
	CombatLog    string
	ChatLog      string
	CreateDate   int64
	TouchDate    int64
	ClaimedBy    int64
	Closed       bool
	Deleted      bool
	EscalatesTo  int64 // next queue, 0 = none
	QueueID      int64
	Updated      bool // the petitioner wrote since the GM last did
	ClosedBy     int64
	CloseDate    int64
}

type PetitionMessage struct {
	MessageID  int64
	PetitionID int64
	SenderID   int64
	SentDate   int64
	Text       string
	Comment    bool
	Read       bool
}

type PetitionLogEntry struct {
	LogID       int64
	PetitionID  int64
	LogDateTime int64
	CharacterID int64
	UserName    string
	EventText   string
}

type PetitionCategory struct {
	CategoryID  int64
	DisplayName string
	Description string // "purpose::hint": the client shows the part before "::"
	QueueID     int64
	EscalatesTo int64
	Properties  []string // extra form fields, by label
}

type PetitionQueue struct {
	QueueID     int64
	QueueName   string
	EscalatesTo int64
}

func EnsurePetitionTables(ctx context.Context, db *sql.DB) error {
	stmts := []string{`
		CREATE TABLE IF NOT EXISTS petitions (
			petitionID   BIGINT NOT NULL AUTO_INCREMENT,
			petitionerID BIGINT NOT NULL,
			oocCharID    BIGINT NOT NULL DEFAULT 0,
			accountID    BIGINT NOT NULL DEFAULT 0,
			subject      VARCHAR(255) NOT NULL DEFAULT '',
			petitionText TEXT NOT NULL,
			categoryID   INT NOT NULL,
			properties   TEXT NULL,
			combatLog    MEDIUMTEXT NULL,
			chatLog      MEDIUMTEXT NULL,
			createDate   BIGINT NOT NULL,
			touchDate    BIGINT NOT NULL,
			claimedBy    BIGINT NOT NULL DEFAULT 0,
			closed       TINYINT NOT NULL DEFAULT 0,
			deleted      TINYINT NOT NULL DEFAULT 0,
			escalatesTo  INT NOT NULL DEFAULT 0,
			queueID      INT NOT NULL DEFAULT 1,
			updated      TINYINT NOT NULL DEFAULT 0,
			closedBy     BIGINT NOT NULL DEFAULT 0,
			closeDate    BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (petitionID),
			KEY idx_pet_petitioner (petitionerID),
			KEY idx_pet_queue (queueID, claimedBy, closed, deleted)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`, `
		CREATE TABLE IF NOT EXISTS petitionMessages (
			messageID  BIGINT NOT NULL AUTO_INCREMENT,
			petitionID BIGINT NOT NULL,
			senderID   BIGINT NOT NULL,
			sentDate   BIGINT NOT NULL,
			text       TEXT NOT NULL,
			comment    TINYINT NOT NULL DEFAULT 0,
			isRead     TINYINT NOT NULL DEFAULT 0,
			PRIMARY KEY (messageID),
			KEY idx_pm_petition (petitionID)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`, `
		CREATE TABLE IF NOT EXISTS petitionLog (
			logID       BIGINT NOT NULL AUTO_INCREMENT,
			petitionID  BIGINT NOT NULL,
			logDateTime BIGINT NOT NULL,
			characterID BIGINT NOT NULL DEFAULT 0,
			userName    VARCHAR(64) NOT NULL DEFAULT '',
			eventText   VARCHAR(255) NOT NULL DEFAULT '',
			PRIMARY KEY (logID),
			KEY idx_pl_petition (petitionID)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`, `
		CREATE TABLE IF NOT EXISTS petitionCategories (
			categoryID  INT NOT NULL,
			displayName VARCHAR(64) NOT NULL,
			description TEXT NOT NULL,
			queueID     INT NOT NULL DEFAULT 1,
			escalatesTo INT NOT NULL DEFAULT 2,
			properties  VARCHAR(255) NOT NULL DEFAULT '',
			PRIMARY KEY (categoryID)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`, `
		CREATE TABLE IF NOT EXISTS petitionQueues (
			queueID     INT NOT NULL,
			queueName   VARCHAR(64) NOT NULL,
			escalatesTo INT NOT NULL DEFAULT 0,
			PRIMARY KEY (queueID)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("EnsurePetitionTables: %w", err)
		}
	}
	// Seed once. Category ids 1 and -5 are avoided: the client opens a web
	// browser for those two instead of a form. 4 and 5 attach the chat log,
	// 7 skips the combat log (petition.py CreatePetition).
	var n int
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM petitionCategories`).Scan(&n)
	if n == 0 {
		seed := []PetitionCategory{
			{2, "Stuck", "I am stuck and cannot move, warp or dock::Say where you are and what you were doing.", 1, 2, []string{"Location"}},
			{3, "Game Play", "A question about how the game works::Ask it as precisely as you can.", 1, 2, nil},
			{4, "Harassment", "Another capsuleer is harassing me::Your recent chat is attached automatically.", 1, 2, []string{"Offending capsuleer"}},
			{5, "Scam / Theft", "I have been scammed or robbed::Your recent chat is attached automatically.", 1, 2, []string{"Offending capsuleer"}},
			{6, "Reimbursement", "I lost ships, items or ISK to a server problem::List what was lost and when.", 1, 2, []string{"Items lost", "Date and time"}},
			{7, "Technical", "Something in the game is broken::Describe how to make it happen again.", 1, 2, nil},
			{8, "Other", "Anything that fits nowhere else::", 1, 2, nil},
		}
		for _, c := range seed {
			_, _ = db.ExecContext(ctx, `
				INSERT IGNORE INTO petitionCategories (categoryID, displayName, description, queueID, escalatesTo, properties)
				VALUES (?, ?, ?, ?, ?, ?)`, c.CategoryID, c.DisplayName, c.Description, c.QueueID, c.EscalatesTo, strings.Join(c.Properties, ","))
		}
	}
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM petitionQueues`).Scan(&n)
	if n == 0 {
		_, _ = db.ExecContext(ctx, `INSERT IGNORE INTO petitionQueues (queueID, queueName, escalatesTo) VALUES (1, 'GM Queue', 2), (2, 'Senior GM Queue', 0)`)
	}
	return nil
}

func ListPetitionCategories(ctx context.Context, db *sql.DB) ([]PetitionCategory, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT categoryID, displayName, description, queueID, escalatesTo, properties FROM petitionCategories ORDER BY categoryID`)
	if err != nil {
		return nil, fmt.Errorf("ListPetitionCategories: %w", err)
	}
	defer rows.Close()
	var out []PetitionCategory
	for rows.Next() {
		var c PetitionCategory
		var props string
		if err := rows.Scan(&c.CategoryID, &c.DisplayName, &c.Description, &c.QueueID, &c.EscalatesTo, &props); err != nil {
			return nil, fmt.Errorf("ListPetitionCategories scan: %w", err)
		}
		if props != "" {
			c.Properties = strings.Split(props, ",")
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func GetPetitionCategory(ctx context.Context, db *sql.DB, categoryID int64) (*PetitionCategory, error) {
	cats, err := ListPetitionCategories(ctx, db)
	if err != nil {
		return nil, err
	}
	for i := range cats {
		if cats[i].CategoryID == categoryID {
			return &cats[i], nil
		}
	}
	return nil, nil
}

func ListPetitionQueues(ctx context.Context, db *sql.DB) ([]PetitionQueue, error) {
	rows, err := db.QueryContext(ctx, `SELECT queueID, queueName, escalatesTo FROM petitionQueues ORDER BY queueID`)
	if err != nil {
		return nil, fmt.Errorf("ListPetitionQueues: %w", err)
	}
	defer rows.Close()
	var out []PetitionQueue
	for rows.Next() {
		var q PetitionQueue
		if err := rows.Scan(&q.QueueID, &q.QueueName, &q.EscalatesTo); err != nil {
			return nil, fmt.Errorf("ListPetitionQueues scan: %w", err)
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// MayPetition is the client's four-way answer: 1 go ahead, -1 an open
// petition already sits in that category, -2 the daily quota is spent.
// (-3 no character and -4 bad out-of-character ID are decided by the caller,
// which knows the account.)
func MayPetition(ctx context.Context, db *sql.DB, petitionerID, categoryID int64, now time.Time) (int64, error) {
	var open int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM petitions WHERE petitionerID = ? AND categoryID = ? AND closed = 0 AND deleted = 0`,
		petitionerID, categoryID).Scan(&open); err != nil {
		return 0, fmt.Errorf("MayPetition: %w", err)
	}
	if open > 0 {
		return -1, nil
	}
	var today int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM petitions WHERE petitionerID = ? AND createDate > ?`,
		petitionerID, now.Add(-24*time.Hour).Unix()).Scan(&today); err != nil {
		return 0, fmt.Errorf("MayPetition quota: %w", err)
	}
	if today >= MaxPetitionsPerDay {
		return -2, nil
	}
	return 1, nil
}

func CreatePetition(ctx context.Context, db *sql.DB, p Petition) (int64, error) {
	var props []byte
	if len(p.Properties) > 0 {
		props, _ = json.Marshal(p.Properties)
	}
	now := time.Now().Unix()
	if p.CreateDate == 0 {
		p.CreateDate = now
	}
	if p.QueueID == 0 {
		p.QueueID = PetitionQueueGM
	}
	res, err := db.ExecContext(ctx, `
		INSERT INTO petitions
		  (petitionerID, oocCharID, accountID, subject, petitionText, categoryID, properties, combatLog, chatLog,
		   createDate, touchDate, escalatesTo, queueID)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.PetitionerID, p.OocCharID, p.AccountID, p.Subject, p.Text, p.CategoryID, nullIfEmpty(string(props)),
		nullIfEmpty(p.CombatLog), nullIfEmpty(p.ChatLog), p.CreateDate, p.CreateDate, p.EscalatesTo, p.QueueID)
	if err != nil {
		return 0, fmt.Errorf("CreatePetition: %w", err)
	}
	id, _ := res.LastInsertId()
	return id, nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

const petitionSelect = `
	SELECT petitionID, petitionerID, oocCharID, accountID, subject, petitionText, categoryID,
	       COALESCE(properties, ''), createDate, touchDate, claimedBy, closed, deleted, escalatesTo, queueID,
	       updated, closedBy, closeDate
	  FROM petitions `

func scanPetition(rows interface{ Scan(...any) error }) (Petition, error) {
	var p Petition
	var props string
	var closed, deleted, updated int
	if err := rows.Scan(&p.PetitionID, &p.PetitionerID, &p.OocCharID, &p.AccountID, &p.Subject, &p.Text, &p.CategoryID,
		&props, &p.CreateDate, &p.TouchDate, &p.ClaimedBy, &closed, &deleted, &p.EscalatesTo, &p.QueueID,
		&updated, &p.ClosedBy, &p.CloseDate); err != nil {
		return p, err
	}
	p.Closed, p.Deleted, p.Updated = closed != 0, deleted != 0, updated != 0
	if props != "" {
		_ = json.Unmarshal([]byte(props), &p.Properties)
	}
	return p, nil
}

func queryPetitions(ctx context.Context, db *sql.DB, where string, args ...any) ([]Petition, error) {
	rows, err := db.QueryContext(ctx, petitionSelect+where, args...)
	if err != nil {
		return nil, fmt.Errorf("queryPetitions: %w", err)
	}
	defer rows.Close()
	var out []Petition
	for rows.Next() {
		p, err := scanPetition(rows)
		if err != nil {
			return nil, fmt.Errorf("queryPetitions scan: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func GetPetition(ctx context.Context, db *sql.DB, petitionID int64) (*Petition, error) {
	ps, err := queryPetitions(ctx, db, `WHERE petitionID = ?`, petitionID)
	if err != nil || len(ps) == 0 {
		return nil, err
	}
	return &ps[0], nil
}

// ListMyPetitions is the pilot's own tab: everything they filed that they
// have not deleted, newest first.
func ListMyPetitions(ctx context.Context, db *sql.DB, petitionerID int64) ([]Petition, error) {
	return queryPetitions(ctx, db, `WHERE petitionerID = ? AND deleted = 0 ORDER BY petitionID DESC`, petitionerID)
}

func ListClaimedPetitions(ctx context.Context, db *sql.DB, claimerID int64) ([]Petition, error) {
	return queryPetitions(ctx, db, `WHERE claimedBy = ? AND closed = 0 AND deleted = 0 ORDER BY touchDate DESC`, claimerID)
}

func ListPetitionQueue(ctx context.Context, db *sql.DB, queueID int64) ([]Petition, error) {
	return queryPetitions(ctx, db, `WHERE queueID = ? AND claimedBy = 0 AND closed = 0 AND deleted = 0 ORDER BY createDate`, queueID)
}

// ListPetitionsForAdmin is the ESP list: open / claimed / closed.
func ListPetitionsForAdmin(ctx context.Context, db *sql.DB, filter string) ([]Petition, error) {
	switch filter {
	case "claimed":
		return queryPetitions(ctx, db, `WHERE claimedBy <> 0 AND closed = 0 AND deleted = 0 ORDER BY touchDate DESC`)
	case "closed":
		return queryPetitions(ctx, db, `WHERE closed = 1 ORDER BY closeDate DESC LIMIT 200`)
	case "all":
		return queryPetitions(ctx, db, `ORDER BY petitionID DESC LIMIT 200`)
	default:
		return queryPetitions(ctx, db, `WHERE claimedBy = 0 AND closed = 0 AND deleted = 0 ORDER BY createDate`)
	}
}

func CountPetitionsByState(ctx context.Context, db *sql.DB) (open, claimed, closed int64) {
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM petitions WHERE claimedBy = 0 AND closed = 0 AND deleted = 0`).Scan(&open)
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM petitions WHERE claimedBy <> 0 AND closed = 0 AND deleted = 0`).Scan(&claimed)
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM petitions WHERE closed = 1`).Scan(&closed)
	return
}

// ClaimPetition takes an unclaimed open petition; false when somebody else
// got there first.
func ClaimPetition(ctx context.Context, db *sql.DB, petitionID, claimerID int64) (bool, error) {
	res, err := db.ExecContext(ctx,
		`UPDATE petitions SET claimedBy = ?, touchDate = ? WHERE petitionID = ? AND claimedBy = 0 AND closed = 0 AND deleted = 0`,
		claimerID, time.Now().Unix(), petitionID)
	if err != nil {
		return false, fmt.Errorf("ClaimPetition: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func UnclaimPetition(ctx context.Context, db *sql.DB, petitionID int64) error {
	_, err := db.ExecContext(ctx, `UPDATE petitions SET claimedBy = 0, touchDate = ? WHERE petitionID = ?`, time.Now().Unix(), petitionID)
	if err != nil {
		return fmt.Errorf("UnclaimPetition: %w", err)
	}
	return nil
}

func EscalatePetition(ctx context.Context, db *sql.DB, petitionID, toQueue int64) error {
	_, err := db.ExecContext(ctx,
		`UPDATE petitions SET queueID = ?, claimedBy = 0, escalatesTo = 0, touchDate = ? WHERE petitionID = ?`,
		toQueue, time.Now().Unix(), petitionID)
	if err != nil {
		return fmt.Errorf("EscalatePetition: %w", err)
	}
	return nil
}

func ClosePetition(ctx context.Context, db *sql.DB, petitionID, closerID int64) error {
	now := time.Now().Unix()
	_, err := db.ExecContext(ctx,
		`UPDATE petitions SET closed = 1, closedBy = ?, closeDate = ?, touchDate = ? WHERE petitionID = ?`,
		closerID, now, now, petitionID)
	if err != nil {
		return fmt.Errorf("ClosePetition: %w", err)
	}
	return nil
}

// CancelPetition withdraws an unclaimed petition of the pilot's own.
func CancelPetition(ctx context.Context, db *sql.DB, petitionID, petitionerID int64) (bool, error) {
	now := time.Now().Unix()
	res, err := db.ExecContext(ctx,
		`UPDATE petitions SET deleted = 1, closed = 1, closeDate = ?, touchDate = ? WHERE petitionID = ? AND petitionerID = ? AND claimedBy = 0`,
		now, now, petitionID, petitionerID)
	if err != nil {
		return false, fmt.Errorf("CancelPetition: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// DeletePetition removes a closed petition from the pilot's list.
func DeletePetition(ctx context.Context, db *sql.DB, petitionID, petitionerID int64) (bool, error) {
	res, err := db.ExecContext(ctx,
		`UPDATE petitions SET deleted = 1 WHERE petitionID = ? AND petitionerID = ? AND closed = 1`, petitionID, petitionerID)
	if err != nil {
		return false, fmt.Errorf("DeletePetition: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SetPetitionUpdated flags who wrote last: true = the petitioner (the GM
// list shows "Updated"), false = the GM ("Waiting").
func SetPetitionUpdated(ctx context.Context, db *sql.DB, petitionID int64, byPetitioner bool) {
	u := 0
	if byPetitioner {
		u = 1
	}
	_, _ = db.ExecContext(ctx, `UPDATE petitions SET updated = ?, touchDate = ? WHERE petitionID = ?`, u, time.Now().Unix(), petitionID)
}

func AddPetitionMessage(ctx context.Context, db *sql.DB, petitionID, senderID int64, text string, comment bool) (int64, error) {
	c := 0
	if comment {
		c = 1
	}
	res, err := db.ExecContext(ctx,
		`INSERT INTO petitionMessages (petitionID, senderID, sentDate, text, comment) VALUES (?, ?, ?, ?, ?)`,
		petitionID, senderID, time.Now().Unix(), text, c)
	if err != nil {
		return 0, fmt.Errorf("AddPetitionMessage: %w", err)
	}
	id, _ := res.LastInsertId()
	return id, nil
}

func ListPetitionMessages(ctx context.Context, db *sql.DB, petitionID int64, includeComments bool) ([]PetitionMessage, error) {
	where := `WHERE petitionID = ?`
	if !includeComments {
		where += ` AND comment = 0`
	}
	rows, err := db.QueryContext(ctx,
		`SELECT messageID, petitionID, senderID, sentDate, text, comment, isRead FROM petitionMessages `+where+` ORDER BY messageID`, petitionID)
	if err != nil {
		return nil, fmt.Errorf("ListPetitionMessages: %w", err)
	}
	defer rows.Close()
	var out []PetitionMessage
	for rows.Next() {
		var m PetitionMessage
		var c, r int
		if err := rows.Scan(&m.MessageID, &m.PetitionID, &m.SenderID, &m.SentDate, &m.Text, &c, &r); err != nil {
			return nil, fmt.Errorf("ListPetitionMessages scan: %w", err)
		}
		m.Comment, m.Read = c != 0, r != 0
		out = append(out, m)
	}
	return out, rows.Err()
}

// ListUnreadPetitionMessages is what blinks the help button: messages on
// the pilot's own petitions, written by somebody else, not yet opened.
func ListUnreadPetitionMessages(ctx context.Context, db *sql.DB, petitionerID int64) ([]PetitionMessage, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT m.messageID, m.petitionID, m.senderID, m.sentDate, m.text, m.comment, m.isRead
		  FROM petitionMessages m JOIN petitions p ON p.petitionID = m.petitionID
		 WHERE p.petitionerID = ? AND p.deleted = 0 AND m.senderID <> ? AND m.comment = 0 AND m.isRead = 0
		 ORDER BY m.messageID`, petitionerID, petitionerID)
	if err != nil {
		return nil, fmt.Errorf("ListUnreadPetitionMessages: %w", err)
	}
	defer rows.Close()
	var out []PetitionMessage
	for rows.Next() {
		var m PetitionMessage
		var c, r int
		if err := rows.Scan(&m.MessageID, &m.PetitionID, &m.SenderID, &m.SentDate, &m.Text, &c, &r); err != nil {
			return nil, fmt.Errorf("ListUnreadPetitionMessages scan: %w", err)
		}
		m.Comment, m.Read = c != 0, r != 0
		out = append(out, m)
	}
	return out, rows.Err()
}

func MarkPetitionMessageRead(ctx context.Context, db *sql.DB, messageID int64) {
	_, _ = db.ExecContext(ctx, `UPDATE petitionMessages SET isRead = 1 WHERE messageID = ?`, messageID)
}

func AddPetitionLog(ctx context.Context, db *sql.DB, petitionID, charID int64, userName, eventText string) {
	if userName == "" && charID != 0 {
		_ = db.QueryRowContext(ctx, `SELECT characterName FROM character_ WHERE characterID = ?`, charID).Scan(&userName)
	}
	_, _ = db.ExecContext(ctx,
		`INSERT INTO petitionLog (petitionID, logDateTime, characterID, userName, eventText) VALUES (?, ?, ?, ?, ?)`,
		petitionID, time.Now().Unix(), charID, userName, eventText)
}

func ListPetitionLog(ctx context.Context, db *sql.DB, petitionID int64) ([]PetitionLogEntry, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT logID, petitionID, logDateTime, characterID, userName, eventText FROM petitionLog WHERE petitionID = ? ORDER BY logID`, petitionID)
	if err != nil {
		return nil, fmt.Errorf("ListPetitionLog: %w", err)
	}
	defer rows.Close()
	var out []PetitionLogEntry
	for rows.Next() {
		var e PetitionLogEntry
		if err := rows.Scan(&e.LogID, &e.PetitionID, &e.LogDateTime, &e.CharacterID, &e.UserName, &e.EventText); err != nil {
			return nil, fmt.Errorf("ListPetitionLog scan: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// CharacterBelongsToAccount is the -4 test for an out-of-character petition.
func CharacterBelongsToAccount(ctx context.Context, db *sql.DB, charID, accountID int64) bool {
	var n int
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM character_ WHERE characterID = ? AND accountID = ?`, charID, accountID).Scan(&n)
	return n > 0
}
