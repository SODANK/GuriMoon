package db

// Write side of the corpRegistry bound object: corporation detail edits, member and
// title updates and the vote system (shares are in corpshares.go, item lockdown in
// corplockdown.go). Read helpers that only serve these writes live here too.
// Wire shapes come from the client scripts ui/services/corporation/bco_*.py and
// ui/shared/neocom/corporation/*.py.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"gurimoon/corp"
	"gurimoon/marshal"
)

// Vote types. The values are this database's eveConstants (voteCEO=0, voteWar=1,
// voteShares=2, voteKickMember=3, voteGeneral=4, voteItemLockdown=5, voteItemUnlock=6),
// which config.BulkData.constants hands the client. They are not the EVEmu ordering.
// See docs/db-7-notes.md#corporation-vote-types.
const (
	VoteCEO          = 0
	VoteWar          = 1
	VoteShares       = 2
	VoteKickMember   = 3
	VoteGeneral      = 4
	VoteItemLockdown = 5
	VoteItemUnlock   = 6
)

// Corp application statuses (client const crpApplication*).
const (
	AppAppliedByCharacter      = 0
	AppRenegotiatedByCharacter = 1
	AppAcceptedByCharacter     = 2
	AppRejectedByCharacter     = 3
	AppRejectedByCorporation   = 4
	AppRenegotiatedByCorp      = 5
	AppAcceptedByCorporation   = 6
)

// execIgnoreDupCol runs DDL and swallows the "duplicate column" error so the
// ALTER-based migration is idempotent on both MariaDB and MySQL.
func execIgnoreDupCol(ctx context.Context, db *sql.DB, stmt string) error {
	if _, err := db.ExecContext(ctx, stmt); err != nil {
		if strings.Contains(err.Error(), "Duplicate column") {
			return nil
		}
		return err
	}
	return nil
}

// EnsureCorpWriteTables creates the vote/lockdown tables and extends
// chrCorporationRoles with the member-row columns the corp window edits.
// Called once at startup.
func EnsureCorpWriteTables(ctx context.Context, db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS crpVoteCases (
		   voteCaseID    INT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
		   corporationID INT UNSIGNED NOT NULL,
		   voteCaseText  VARCHAR(500) NOT NULL DEFAULT '',
		   description   VARCHAR(1000) NOT NULL DEFAULT '',
		   voteType      INT NOT NULL DEFAULT 0,
		   startDateTime BIGINT NOT NULL DEFAULT 0,
		   endDateTime   BIGINT NOT NULL DEFAULT 0,
		   inEffect      TINYINT NOT NULL DEFAULT 0,
		   actedUpon     TINYINT NOT NULL DEFAULT 0,
		   timeActedUpon BIGINT NOT NULL DEFAULT 0,
		   timeRescended BIGINT NOT NULL DEFAULT 0,
		   INDEX idx_corp (corporationID)
		 ) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS crpVoteOptions (
		   voteCaseID INT UNSIGNED NOT NULL,
		   optionID   INT UNSIGNED NOT NULL,
		   optionText VARCHAR(200) NOT NULL DEFAULT '',
		   parameter  BIGINT NULL,
		   parameter1 BIGINT NULL,
		   parameter2 BIGINT NULL,
		   votesFor   INT NOT NULL DEFAULT 0,
		   PRIMARY KEY (voteCaseID, optionID)
		 ) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS crpVotes (
		   voteCaseID  INT UNSIGNED NOT NULL,
		   characterID BIGINT NOT NULL,
		   optionID    INT UNSIGNED NOT NULL,
		   PRIMARY KEY (voteCaseID, characterID)
		 ) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS crpLockedItems (
		   itemID        BIGINT NOT NULL PRIMARY KEY,
		   corporationID INT UNSIGNED NOT NULL,
		   typeID        INT UNSIGNED NOT NULL DEFAULT 0,
		   ownerID       BIGINT NOT NULL DEFAULT 0,
		   locationID    BIGINT NOT NULL DEFAULT 0,
		   INDEX idx_corp_loc (corporationID, locationID)
		 ) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("EnsureCorpWriteTables: %w", err)
		}
	}
	// crpTitles is created here because AddTitleRoles joins it on every login, before any
	// corporation window could have created it.
	if err := ensureCorpTitlesTable(ctx, db); err != nil {
		return fmt.Errorf("EnsureCorpWriteTables titles: %w", err)
	}
	// Member-row fields the client edits that the legacy 5-mask table lacks.
	for _, col := range []string{
		"grantableRoles BIGINT UNSIGNED NOT NULL DEFAULT 0",
		"grantableRolesAtHQ BIGINT UNSIGNED NOT NULL DEFAULT 0",
		"grantableRolesAtBase BIGINT UNSIGNED NOT NULL DEFAULT 0",
		"grantableRolesAtOther BIGINT UNSIGNED NOT NULL DEFAULT 0",
		"titleMask BIGINT UNSIGNED NOT NULL DEFAULT 0",
		"baseID BIGINT NULL",
		"divisionID BIGINT NULL",
		"squadronID BIGINT NULL",
	} {
		if err := execIgnoreDupCol(ctx, db,
			"ALTER TABLE chrCorporationRoles ADD COLUMN "+col); err != nil {
			return fmt.Errorf("EnsureCorpWriteTables alter: %w", err)
		}
	}
	// The stock schema caps division names at VARCHAR(11); the client allows longer, so widen.
	// Probed first: seven MODIFYs on corporation ran on every boot.
	for i := 1; i <= 7; i++ {
		col := fmt.Sprintf("division%d", i)
		if !columnNeedsModify(ctx, db, "corporation", col, true,
			func(ct string) bool { return ct == "varchar(50)" }) {
			continue
		}
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			`ALTER TABLE corporation MODIFY %s VARCHAR(50) NULL`, col)); err != nil {
			return fmt.Errorf("EnsureCorpWriteTables divisions: %w", err)
		}
	}
	// Not fatal: a failed repair leaves the treasury as it was and costs only the Shares tab.
	if err := EnsureCorpShareTreasury(ctx, db); err != nil {
		log.Printf("[shares] %v", err)
	}
	return nil
}

// ─── corporation detail updates ────────────────────────────────────────────

// UpdateCorporationDetails sets description/url/taxRate and returns the prior
// values for the OnCorporationChanged (old, new) dict.
func UpdateCorporationDetails(ctx context.Context, db *sql.DB, corpID int64, desc, url string, taxRate float64) (oldDesc, oldURL string, oldTax float64, err error) {
	var d, u sql.NullString
	err = db.QueryRowContext(ctx,
		`SELECT description, url, taxRate FROM corporation WHERE corporationID = ?`,
		corpID).Scan(&d, &u, &oldTax)
	if err != nil {
		return "", "", 0, fmt.Errorf("UpdateCorporationDetails read: %w", err)
	}
	oldDesc, oldURL = d.String, u.String
	_, err = db.ExecContext(ctx,
		`UPDATE corporation SET description = ?, url = ?, taxRate = ? WHERE corporationID = ?`,
		desc, url, taxRate, corpID)
	if err != nil {
		return "", "", 0, fmt.Errorf("UpdateCorporationDetails: %w", err)
	}
	return oldDesc, oldURL, oldTax, nil
}

// UpdateCorpLogo sets the six logo layer graphicIDs, returning old values.
func UpdateCorpLogo(ctx context.Context, db *sql.DB, corpID int64, vals [6]sql.NullInt64) (old [6]sql.NullInt64, err error) {
	err = db.QueryRowContext(ctx,
		`SELECT shape1, shape2, shape3, color1, color2, color3
		 FROM corporation WHERE corporationID = ?`, corpID,
	).Scan(&old[0], &old[1], &old[2], &old[3], &old[4], &old[5])
	if err != nil {
		return old, fmt.Errorf("UpdateCorpLogo read: %w", err)
	}
	_, err = db.ExecContext(ctx,
		`UPDATE corporation SET shape1=?, shape2=?, shape3=?, color1=?, color2=?, color3=?
		 WHERE corporationID = ?`,
		vals[0], vals[1], vals[2], vals[3], vals[4], vals[5], corpID)
	if err != nil {
		return old, fmt.Errorf("UpdateCorpLogo: %w", err)
	}
	return old, nil
}

// UpdateCorpDivisions sets the seven hangar-division names, returning the old
// names (with the client's default substituted for NULL).
func UpdateCorpDivisions(ctx context.Context, db *sql.DB, corpID int64, div [7]string) (old [7]string, err error) {
	var o [7]sql.NullString
	err = db.QueryRowContext(ctx,
		`SELECT division1, division2, division3, division4, division5, division6, division7
		 FROM corporation WHERE corporationID = ?`, corpID,
	).Scan(&o[0], &o[1], &o[2], &o[3], &o[4], &o[5], &o[6])
	if err != nil {
		return old, fmt.Errorf("UpdateCorpDivisions read: %w", err)
	}
	for i := range o {
		old[i] = o[i].String
	}
	_, err = db.ExecContext(ctx,
		`UPDATE corporation SET division1=?, division2=?, division3=?, division4=?,
		        division5=?, division6=?, division7=? WHERE corporationID = ?`,
		div[0], div[1], div[2], div[3], div[4], div[5], div[6], corpID)
	if err != nil {
		return old, fmt.Errorf("UpdateCorpDivisions: %w", err)
	}
	return old, nil
}

// UpdateCorpAbilities persists memberLimit/allowedMemberRaceIDs (derived from
// the CEO's leadership skills) and returns the old values.
func UpdateCorpAbilities(ctx context.Context, db *sql.DB, corpID, memberLimit, allowedRaces int64) (oldLimit, oldRaces int64, err error) {
	var races sql.NullInt64
	err = db.QueryRowContext(ctx,
		`SELECT memberLimit, allowedMemberRaceIDs FROM corporation WHERE corporationID = ?`,
		corpID).Scan(&oldLimit, &races)
	if err != nil {
		return 0, 0, fmt.Errorf("UpdateCorpAbilities read: %w", err)
	}
	oldRaces = races.Int64
	if oldLimit == memberLimit && oldRaces == allowedRaces {
		return oldLimit, oldRaces, nil
	}
	_, err = db.ExecContext(ctx,
		`UPDATE corporation SET memberLimit = ?, allowedMemberRaceIDs = ? WHERE corporationID = ?`,
		memberLimit, allowedRaces, corpID)
	if err != nil {
		return 0, 0, fmt.Errorf("UpdateCorpAbilities: %w", err)
	}
	return oldLimit, oldRaces, nil
}

// CorpMgmtSkills returns the levels of the five corp-management skills that
// determine member limit and allowed races.
type CorpMgmtSkills struct {
	CorpMgmt, Megacorp, EmpireControl, Sovereignty, EthnicRelations int
}

func GetCorpMgmtSkills(ctx context.Context, db *sql.DB, charID int64) CorpMgmtSkills {
	var s CorpMgmtSkills
	dst := map[int64]*int{
		3363:  &s.CorpMgmt,        // Corporation Management
		3731:  &s.Megacorp,        // Megacorp Management
		3732:  &s.EmpireControl,   // Empire Control
		12241: &s.Sovereignty,     // Sovereignty
		3368:  &s.EthnicRelations, // Ethnic Relations
	}
	rows, err := db.QueryContext(ctx, `
		SELECT e.typeID, COALESCE(lv.valueInt, 0)
		FROM entity e
		LEFT JOIN entity_attributes lv ON lv.itemID = e.itemID AND lv.attributeID = 280
		WHERE e.ownerID = ? AND e.flag IN (7, 61)
		  AND e.typeID IN (3363, 3731, 3732, 12241, 3368)`, charID)
	if err != nil {
		return s
	}
	defer rows.Close()
	for rows.Next() {
		var typeID, lvl int64
		if rows.Scan(&typeID, &lvl) == nil {
			if p := dst[typeID]; p != nil {
				*p = int(lvl)
			}
		}
	}
	return s
}

// RepairCEOGrantableRoles gives every corporation's CEO every grantable role, as SetCorpCEO and
// JoinCorporation now write them: rows from before held none, which the client's role window
// reads when it decides what the CEO may tick.
func RepairCEOGrantableRoles(ctx context.Context, db *sql.DB) error {
	all := ^uint64(0)
	_, err := db.ExecContext(ctx, `
		UPDATE chrCorporationRoles r
		  JOIN corporation c ON c.ceoID = r.characterID
		   SET r.grantableRoles = ?, r.grantableRolesAtHQ = ?, r.grantableRolesAtBase = ?, r.grantableRolesAtOther = ?
		 WHERE r.grantableRoles <> ? OR r.grantableRolesAtHQ <> ? OR r.grantableRolesAtBase <> ? OR r.grantableRolesAtOther <> ?`,
		all, all, all, all, all, all, all, all)
	if err != nil {
		return fmt.Errorf("RepairCEOGrantableRoles: %w", err)
	}
	return nil
}

// SetCorpCEO transfers the CEO seat, granting RoleAll to the new CEO's role
// row.  Returns the previous ceoID.
func SetCorpCEO(ctx context.Context, db *sql.DB, corpID, newCeoID int64) (oldCeoID int64, err error) {
	if err = db.QueryRowContext(ctx,
		`SELECT ceoID FROM corporation WHERE corporationID = ?`, corpID).Scan(&oldCeoID); err != nil {
		return 0, fmt.Errorf("SetCorpCEO read: %w", err)
	}
	if _, err = db.ExecContext(ctx,
		`UPDATE corporation SET ceoID = ? WHERE corporationID = ?`, newCeoID, corpID); err != nil {
		return 0, fmt.Errorf("SetCorpCEO: %w", err)
	}
	all := ^uint64(0)
	if _, err = db.ExecContext(ctx,
		`REPLACE INTO chrCorporationRoles (characterID, corprole, rolesAtAll, rolesAtBase, rolesAtHQ, rolesAtOther,
		   grantableRoles, grantableRolesAtHQ, grantableRolesAtBase, grantableRolesAtOther)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, newCeoID, all, all, all, all, all, all, all, all, all); err != nil {
		return 0, fmt.Errorf("SetCorpCEO grant: %w", err)
	}
	if oldCeoID != 0 && oldCeoID != newCeoID {
		EmitWorldEvent(CorpCEOChangedEvent{At: time.Now(), CorpID: corpID, OldCEOID: oldCeoID, NewCEOID: newCeoID})
	}
	return oldCeoID, nil
}

// MarkCorpDeleted flags a corporation as closed.
func MarkCorpDeleted(ctx context.Context, db *sql.DB, corpID int64) error {
	_, err := db.ExecContext(ctx,
		`UPDATE corporation SET deleted = 1 WHERE corporationID = ?`, corpID)
	if err == nil {
		NotifyCorpDeleted(corpID)
	}
	return err
}

var (
	corpDeletedMu   sync.RWMutex
	corpDeletedSubs []func(corpID int64)
)

// SubscribeCorpDeleted registers fn for every corporation closed by either path (the
// in-game sole-member resignation, MarkCorpDeleted, and the ESP delete, ESPDeleteCorp).
// esp.CorpDeletedHook is the game server's single slot for session pushes; modules use
// this one. fn runs on its own goroutine after the commit, behind a recover.
func SubscribeCorpDeleted(fn func(corpID int64)) {
	if fn == nil {
		return
	}
	corpDeletedMu.Lock()
	corpDeletedSubs = append(corpDeletedSubs, fn)
	corpDeletedMu.Unlock()
}

// NotifyCorpDeleted delivers corpID to every subscriber.
func NotifyCorpDeleted(corpID int64) {
	corpDeletedMu.RLock()
	subs := make([]func(int64), len(corpDeletedSubs))
	copy(subs, corpDeletedSubs)
	corpDeletedMu.RUnlock()
	for _, fn := range subs {
		go func(fn func(int64)) {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[corp] corp-deleted subscriber panicked on %d: %v", corpID, r)
				}
			}()
			fn(corpID)
		}(fn)
	}
}

// SuggestedTickers derives candidate ticker strings from a corporation name
// and filters out ones already taken.
func SuggestedTickers(ctx context.Context, db *sql.DB, name string) []string {
	words := strings.Fields(strings.ToUpper(name))
	var cands []string
	add := func(s string) {
		s = strings.Map(func(r rune) rune {
			if r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
				return r
			}
			return -1
		}, s)
		if len(s) < 2 {
			return
		}
		if len(s) > 5 {
			s = s[:5]
		}
		for _, c := range cands {
			if c == s {
				return
			}
		}
		cands = append(cands, s)
	}
	// Initials, then progressively longer prefixes of the first word.
	if len(words) > 1 {
		var ini strings.Builder
		for _, w := range words {
			ini.WriteByte(w[0])
		}
		add(ini.String())
	}
	if len(words) > 0 {
		w := words[0]
		for n := 2; n <= 5 && n <= len(w); n++ {
			add(w[:n])
		}
	}
	// Consonant skeleton of the whole name.
	add(strings.Map(func(r rune) rune {
		if strings.ContainsRune("AEIOU ", r) {
			return -1
		}
		return r
	}, strings.ToUpper(name)))

	var free []string
	for _, cand := range cands {
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM corporation WHERE tickerName = ?`, cand).Scan(&n); err == nil && n == 0 {
			free = append(free, cand)
		}
	}
	return free
}

// ─── members ───────────────────────────────────────────────────────────────

// CorpMemberUpdate carries UpdateMember's wire fields; nil = leave unchanged
// (the client sends None for untouched fields).
type CorpMemberUpdate struct {
	Title                               *string
	DivisionID, SquadronID              *int64
	Roles, GrantableRoles               *int64
	RolesAtHQ, GrantableRolesAtHQ       *int64
	RolesAtBase, GrantableRolesAtBase   *int64
	RolesAtOther, GrantableRolesAtOther *int64
	BaseID, TitleMask                   *int64
}

// UpdateCorpMemberRow applies a member edit.  Roles maps to corprole and
// rolesAtAll (the session mask); locational masks and the extended columns go
// to their own fields.
func UpdateCorpMemberRow(ctx context.Context, db *sql.DB, charID int64, u *CorpMemberUpdate) error {
	if _, err := db.ExecContext(ctx,
		`INSERT IGNORE INTO chrCorporationRoles (characterID, corprole, rolesAtAll, rolesAtBase, rolesAtHQ, rolesAtOther)
		 VALUES (?, 0, 0, 0, 0, 0)`, charID); err != nil {
		return fmt.Errorf("UpdateCorpMemberRow seed: %w", err)
	}
	if u.Title != nil {
		if _, err := db.ExecContext(ctx,
			`UPDATE character_ SET title = ? WHERE characterID = ?`, *u.Title, charID); err != nil {
			return fmt.Errorf("UpdateCorpMemberRow title: %w", err)
		}
	}
	set := []string{}
	args := []any{}
	col := func(name string, v *int64) {
		if v != nil {
			set = append(set, name+" = ?")
			args = append(args, uint64(*v))
		}
	}
	if u.Roles != nil {
		set = append(set, "corprole = ?", "rolesAtAll = ?")
		args = append(args, uint64(*u.Roles), uint64(*u.Roles))
	}
	col("rolesAtHQ", u.RolesAtHQ)
	col("rolesAtBase", u.RolesAtBase)
	col("rolesAtOther", u.RolesAtOther)
	col("grantableRoles", u.GrantableRoles)
	col("grantableRolesAtHQ", u.GrantableRolesAtHQ)
	col("grantableRolesAtBase", u.GrantableRolesAtBase)
	col("grantableRolesAtOther", u.GrantableRolesAtOther)
	col("titleMask", u.TitleMask)
	col("divisionID", u.DivisionID)
	col("squadronID", u.SquadronID)
	if u.BaseID != nil {
		set = append(set, "baseID = ?")
		args = append(args, *u.BaseID)
	}
	if len(set) == 0 {
		return nil
	}
	args = append(args, charID)
	_, err := db.ExecContext(ctx,
		`UPDATE chrCorporationRoles SET `+strings.Join(set, ", ")+` WHERE characterID = ?`, args...)
	if err != nil {
		return fmt.Errorf("UpdateCorpMemberRow: %w", err)
	}
	identityChanged(charID)
	return nil
}

// IsCorpBaseStation reports whether a station can be a member's Base: one of
// the corporation's offices, or its headquarters. The Edit Member dialog only
// offers offices (corp_dlg_edit_member.py fills its Base combo from
// GetMyCorporationsOffices), so anything else is a hand-built call.
func IsCorpBaseStation(ctx context.Context, db *sql.DB, corpID, stationID int64) bool {
	var n int
	if err := db.QueryRowContext(ctx, `
		SELECT (SELECT COUNT(*) FROM crpoffices WHERE corporationID = ? AND stationID = ?)
		     + (SELECT COUNT(*) FROM corporation WHERE corporationID = ? AND stationID = ?)`,
		corpID, stationID, corpID, stationID).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

// PreviousNPCCorp picks the corp a kicked/quitting character falls back to:
// their most recent NPC corp from employment history, else the School of
// Applied Knowledge.
func PreviousNPCCorp(ctx context.Context, db *sql.DB, charID int64) int64 {
	var corpID int64
	err := db.QueryRowContext(ctx, `
		SELECT e.corporationID FROM chrEmployment e
		JOIN crpNPCCorporations n ON n.corporationID = e.corporationID
		WHERE e.characterID = ?
		ORDER BY e.startDate DESC LIMIT 1`, charID).Scan(&corpID)
	if err != nil || corpID == 0 {
		return 1000044
	}
	return corpID
}

// ─── titles ────────────────────────────────────────────────────────────────

// TitleRow mirrors a crpTitles row / an UpdateTitles rowset line.
type TitleRow struct {
	TitleID   int64
	TitleName string
	// roles, grantableRoles, rolesAtHQ, grantableRolesAtHQ,
	// rolesAtBase, grantableRolesAtBase, rolesAtOther, grantableRolesAtOther
	Masks [8]uint64
}

// EnsureDefaultTitles seeds the eight power-of-two titles the client UI
// assumes exist (it walks titleID 1<<0 .. 1<<7 positionally).
// It runs on every title read (GetTitles), so a corporation already seeded in this
// process is skipped, and the seed itself is one statement instead of eight commits.
func EnsureDefaultTitles(ctx context.Context, db *sql.DB, corpID int64) error {
	if _, done := defaultTitlesSeeded.Load(corpID); done {
		return nil
	}
	if err := ensureCorpTitlesTable(ctx, db); err != nil {
		return err
	}
	var q strings.Builder
	q.WriteString(`INSERT IGNORE INTO crpTitles (corporationID, titleID, titleName) VALUES `)
	args := make([]any, 0, 24)
	for i := 0; i < 8; i++ {
		if i > 0 {
			q.WriteString(",")
		}
		q.WriteString("(?, ?, ?)")
		args = append(args, corpID, 1<<i, fmt.Sprintf("Title %d", i+1))
	}
	if _, err := db.ExecContext(ctx, q.String(), args...); err != nil {
		return fmt.Errorf("EnsureDefaultTitles: %w", err)
	}
	defaultTitlesSeeded.Store(corpID, struct{}{})
	return nil
}

// defaultTitlesSeeded holds the corporations EnsureDefaultTitles has seeded in this process.
var defaultTitlesSeeded sync.Map

// GetTitleRow reads one title for change-dict diffing; nil if absent.
func GetTitleRow(ctx context.Context, db *sql.DB, corpID, titleID int64) (*TitleRow, error) {
	t := &TitleRow{TitleID: titleID}
	err := db.QueryRowContext(ctx, `
		SELECT titleName, roles, grantableRoles, rolesAtHQ, grantableRolesAtHQ,
		       rolesAtBase, grantableRolesAtBase, rolesAtOther, grantableRolesAtOther
		FROM crpTitles WHERE corporationID = ? AND titleID = ?`, corpID, titleID,
	).Scan(&t.TitleName, &t.Masks[0], &t.Masks[1], &t.Masks[2], &t.Masks[3],
		&t.Masks[4], &t.Masks[5], &t.Masks[6], &t.Masks[7])
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("GetTitleRow: %w", err)
	}
	return t, nil
}

// UpsertTitle writes a title row.
func UpsertTitle(ctx context.Context, db *sql.DB, corpID int64, t TitleRow) error {
	_, err := db.ExecContext(ctx, `
		REPLACE INTO crpTitles
		  (corporationID, titleID, titleName, roles, grantableRoles,
		   rolesAtHQ, grantableRolesAtHQ, rolesAtBase, grantableRolesAtBase,
		   rolesAtOther, grantableRolesAtOther)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		corpID, t.TitleID, t.TitleName, t.Masks[0], t.Masks[1], t.Masks[2], t.Masks[3],
		t.Masks[4], t.Masks[5], t.Masks[6], t.Masks[7])
	if err != nil {
		return fmt.Errorf("UpsertTitle: %w", err)
	}
	return nil
}

// DeleteTitleRow removes a title.
func DeleteTitleRow(ctx context.Context, db *sql.DB, corpID, titleID int64) error {
	_, err := db.ExecContext(ctx,
		`DELETE FROM crpTitles WHERE corporationID = ? AND titleID = ?`, corpID, titleID)
	return err
}

// ─── votes ─────────────────────────────────────────────────────────────────

// VoteCaseOption is one InsertVoteCase option line.
type VoteCaseOption struct {
	Text                              string
	Parameter, Parameter1, Parameter2 sql.NullInt64
}

var voteCaseCols = []string{
	"voteCaseID", "corporationID", "voteCaseText", "description",
	"startDateTime", "endDateTime", "voteType",
}

var voteOptionCols = []string{
	"voteCaseID", "optionID", "optionText",
	"parameter", "parameter1", "parameter2", "votesFor",
}

// InsertVoteCaseRow stores a vote case with its options; optionIDs are the
// 0-based line index, matching the client's expectations for voteGeneral.
func InsertVoteCaseRow(ctx context.Context, db *sql.DB, corpID int64, text, desc string,
	voteType, start, end int64, options []VoteCaseOption) (int64, error) {
	res, err := db.ExecContext(ctx, `
		INSERT INTO crpVoteCases (corporationID, voteCaseText, description, voteType, startDateTime, endDateTime)
		VALUES (?, ?, ?, ?, ?, ?)`, corpID, text, desc, voteType, start, end)
	if err != nil {
		return 0, fmt.Errorf("InsertVoteCaseRow: %w", err)
	}
	caseID, _ := res.LastInsertId()
	for i, o := range options {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO crpVoteOptions (voteCaseID, optionID, optionText, parameter, parameter1, parameter2)
			VALUES (?, ?, ?, ?, ?, ?)`,
			caseID, i, o.Text, o.Parameter, o.Parameter1, o.Parameter2); err != nil {
			return 0, fmt.Errorf("InsertVoteCaseRow option %d: %w", i, err)
		}
	}
	return caseID, nil
}

// ErrAlreadyVoted refuses a second ballot on one case. The client never offers one, and
// OnCorporationVoteChanged's update arm indexes a row the voter's cache may not hold.
var ErrAlreadyVoted = errors.New("already voted")

// InsertVoteRow records a character's ballot, one per case.  The tally is not
// touched here: the caller runs RetallyOpenVotes, which weighs every ballot in
// shares.
func InsertVoteRow(ctx context.Context, db *sql.DB, voteCaseID, charID, optionID int64) error {
	res, err := db.ExecContext(ctx,
		`INSERT IGNORE INTO crpVotes (voteCaseID, characterID, optionID) VALUES (?, ?, ?)`,
		voteCaseID, charID, optionID)
	if err != nil {
		return fmt.Errorf("InsertVoteRow: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrAlreadyVoted
	}
	return nil
}

// VoteCaseWindow is what a ballot is checked against.
type VoteCaseWindow struct {
	CorpID, VoteType, Start, End int64
}

// GetVoteCaseWindow reads a case's owner, type and voting window.
func GetVoteCaseWindow(ctx context.Context, db *sql.DB, voteCaseID int64) (VoteCaseWindow, error) {
	var w VoteCaseWindow
	err := db.QueryRowContext(ctx,
		`SELECT corporationID, voteType, startDateTime, endDateTime FROM crpVoteCases WHERE voteCaseID = ?`,
		voteCaseID).Scan(&w.CorpID, &w.VoteType, &w.Start, &w.End)
	return w, err
}

// VoteOptionExists reports whether optionID is one of the case's options.
func VoteOptionExists(ctx context.Context, db *sql.DB, voteCaseID, optionID int64) bool {
	var n int
	_ = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM crpVoteOptions WHERE voteCaseID = ? AND optionID = ?`,
		voteCaseID, optionID).Scan(&n)
	return n > 0
}

// GetCorpVoteCases returns every vote case for a corp as a util.IndexRowset
// keyed on voteCaseID (the client caches it unwrapped and filters open/closed
// locally).
func GetCorpVoteCases(ctx context.Context, db *sql.DB, corpID int64) marshal.PyRep {
	rows, err := db.QueryContext(ctx, `
		SELECT voteCaseID, corporationID, voteCaseText, description, startDateTime, endDateTime, voteType
		FROM crpVoteCases WHERE corporationID = ?`, corpID)
	if err != nil {
		return makeIndexRowset("voteCaseID", voteCaseCols, nil)
	}
	defer rows.Close()
	var out [][]marshal.PyRep
	for rows.Next() {
		var id, cid, vt, start, end int64
		var text, desc string
		if rows.Scan(&id, &cid, &text, &desc, &start, &end, &vt) != nil {
			continue
		}
		out = append(out, []marshal.PyRep{
			intRep(id), intRep(cid), strRep(text), strRep(desc),
			intRep(start), intRep(end), intRep(vt),
		})
	}
	return makeIndexRowset("voteCaseID", voteCaseCols, out)
}

// GetCorpVoteCaseOptions returns a vote case's options as a util.IndexRowset
// keyed on optionID.
func GetCorpVoteCaseOptions(ctx context.Context, db *sql.DB, voteCaseID int64) marshal.PyRep {
	rows, err := db.QueryContext(ctx, `
		SELECT voteCaseID, optionID, optionText, parameter, parameter1, parameter2, votesFor
		FROM crpVoteOptions WHERE voteCaseID = ? ORDER BY optionID`, voteCaseID)
	if err != nil {
		return makeIndexRowset("optionID", voteOptionCols, nil)
	}
	defer rows.Close()
	var out [][]marshal.PyRep
	for rows.Next() {
		var caseID, optionID, votesFor int64
		var text string
		var p, p1, p2 sql.NullInt64
		if rows.Scan(&caseID, &optionID, &text, &p, &p1, &p2, &votesFor) != nil {
			continue
		}
		out = append(out, []marshal.PyRep{
			intRep(caseID), intRep(optionID), strRep(text),
			nullIntRep(p), nullIntRep(p1), nullIntRep(p2), intRep(votesFor),
		})
	}
	return makeIndexRowset("optionID", voteOptionCols, out)
}

// GetCorpVotesFor returns the votes cast in a case as a util.IndexRowset
// keyed on characterID.
func GetCorpVotesFor(ctx context.Context, db *sql.DB, voteCaseID int64) marshal.PyRep {
	cols := []string{"characterID", "optionID"}
	rows, err := db.QueryContext(ctx,
		`SELECT characterID, optionID FROM crpVotes WHERE voteCaseID = ?`, voteCaseID)
	if err != nil {
		return makeIndexRowset("characterID", cols, nil)
	}
	defer rows.Close()
	var out [][]marshal.PyRep
	for rows.Next() {
		var charID, optionID int64
		if rows.Scan(&charID, &optionID) != nil {
			continue
		}
		out = append(out, []marshal.PyRep{intRep(charID), intRep(optionID)})
	}
	return makeIndexRowset("characterID", cols, out)
}

var sanctionedActionCols = []string{
	"voteCaseID", "voteType", "inEffect", "actedUpon", "expires",
	"timeActedUpon", "timeRescended", "parameter", "parameter1", "parameter2", "optionID",
}

// SanctionedAction is a closed vote whose winning option carries a nonzero
// parameter (the "yes" side won).
type SanctionedAction struct {
	VoteCaseID, VoteType, OptionID    int64
	InEffect, ActedUpon               int64
	Expires, TimeActedUpon, TimeResc  int64
	Parameter, Parameter1, Parameter2 sql.NullInt64
}

// WinningOption returns the option with strictly the most shares behind it
// (pickWinningOption); ok is false for an empty ballot or a tie.
func WinningOption(ctx context.Context, db *sql.DB, voteCaseID int64) (optionID int64, param, p1, p2 sql.NullInt64, ok bool) {
	rows, err := db.QueryContext(ctx,
		`SELECT optionID, votesFor FROM crpVoteOptions WHERE voteCaseID = ?`, voteCaseID)
	if err != nil {
		return 0, param, p1, p2, false
	}
	var tally []voteTally
	for rows.Next() {
		var t voteTally
		if rows.Scan(&t.OptionID, &t.VotesFor) == nil {
			tally = append(tally, t)
		}
	}
	rows.Close()
	optionID, ok = pickWinningOption(tally)
	if !ok {
		return 0, param, p1, p2, false
	}
	err = db.QueryRowContext(ctx, `
		SELECT parameter, parameter1, parameter2
		FROM crpVoteOptions WHERE voteCaseID = ? AND optionID = ?`, voteCaseID, optionID,
	).Scan(&param, &p1, &p2)
	return optionID, param, p1, p2, err == nil
}

// GetCorpSanctionedActionRows lists a corp's sanctioned actions. A passed vote can be
// implemented for 30 days after voting closes.
func GetCorpSanctionedActionRows(ctx context.Context, db *sql.DB, corpID int64, now int64) ([]SanctionedAction, error) {
	const thirtyDays = int64(30 * 24 * 3600 * 10000000)
	rows, err := db.QueryContext(ctx, `
		SELECT voteCaseID, voteType, inEffect, actedUpon, endDateTime, timeActedUpon, timeRescended
		FROM crpVoteCases
		WHERE corporationID = ? AND endDateTime < ? AND voteType <> ?`,
		corpID, now, VoteGeneral)
	if err != nil {
		return nil, fmt.Errorf("GetCorpSanctionedActionRows: %w", err)
	}
	defer rows.Close()
	var out []SanctionedAction
	for rows.Next() {
		var a SanctionedAction
		var end int64
		if rows.Scan(&a.VoteCaseID, &a.VoteType, &a.InEffect, &a.ActedUpon,
			&end, &a.TimeActedUpon, &a.TimeResc) != nil {
			continue
		}
		optID, param, p1, p2, ok := WinningOption(ctx, db, a.VoteCaseID)
		if !ok || !param.Valid || param.Int64 == 0 {
			continue // the "no" side won, or no votes: nothing to sanction
		}
		a.OptionID, a.Parameter, a.Parameter1, a.Parameter2 = optID, param, p1, p2
		a.Expires = end + thirtyDays
		out = append(out, a)
	}
	return out, nil
}

// SanctionedActionsRep shapes the rows for the wire as a plain util.Rowset; the client
// re-indexes on voteCaseID itself.
func SanctionedActionsRep(actions []SanctionedAction) marshal.PyRep {
	var out [][]marshal.PyRep
	for _, a := range actions {
		out = append(out, []marshal.PyRep{
			intRep(a.VoteCaseID), intRep(a.VoteType), intRep(a.InEffect), intRep(a.ActedUpon),
			intRep(a.Expires), intRep(a.TimeActedUpon), intRep(a.TimeResc),
			nullIntRep(a.Parameter), nullIntRep(a.Parameter1), nullIntRep(a.Parameter2),
			intRep(a.OptionID),
		})
	}
	return makeRowset(sanctionedActionCols, out)
}

// GetVoteCaseInfo fetches what sanctioned-action execution needs.
func GetVoteCaseInfo(ctx context.Context, db *sql.DB, voteCaseID int64) (corpID, voteType int64, actedUpon bool, err error) {
	var acted int64
	err = db.QueryRowContext(ctx,
		`SELECT corporationID, voteType, actedUpon FROM crpVoteCases WHERE voteCaseID = ?`,
		voteCaseID).Scan(&corpID, &voteType, &acted)
	return corpID, voteType, acted != 0, err
}

// MarkSanctionedActionDone stamps a vote case implemented.
func MarkSanctionedActionDone(ctx context.Context, db *sql.DB, voteCaseID, now int64) error {
	_, err := db.ExecContext(ctx,
		`UPDATE crpVoteCases SET inEffect = 1, actedUpon = 1, timeActedUpon = ? WHERE voteCaseID = ?`,
		now, voteCaseID)
	return err
}

// Item lockdown lives in corplockdown.go.

// RepairCorpRoleConstants rewrites the corpRole* rows of eveConstants whose value was
// clamped to 4294967295. const.corpRole* is what the client compares masks against.
// See docs/db-7-notes.md#corporate-role-constants.
func RepairCorpRoleConstants(ctx context.Context, db *sql.DB) error {
	fixes := map[string]uint64{
		"corpRoleAccountCanTake6":     corp.RoleAccountCanTake6,
		"corpRoleAccountCanTake7":     corp.RoleAccountCanTake7,
		"corpRoleAccountCanQuery1":    corp.RoleAccountCanQuery1,
		"corpRoleAccountCanQuery2":    corp.RoleAccountCanQuery2,
		"corpRoleAccountCanQuery3":    corp.RoleAccountCanQuery3,
		"corpRoleAccountCanQuery4":    corp.RoleAccountCanQuery4,
		"corpRoleAccountCanQuery5":    corp.RoleAccountCanQuery5,
		"corpRoleAccountCanQuery6":    corp.RoleAccountCanQuery6,
		"corpRoleAccountCanQuery7":    corp.RoleAccountCanQuery7,
		"corpRoleEquipmentConfig":     corp.RoleEquipmentConfig,
		"corpRoleContainerCanTake1":   corp.RoleContainerCanTake1,
		"corpRoleContainerCanTake2":   corp.RoleContainerCanTake2,
		"corpRoleContainerCanTake3":   corp.RoleContainerCanTake3,
		"corpRoleContainerCanTake4":   corp.RoleContainerCanTake4,
		"corpRoleContainerCanTake5":   corp.RoleContainerCanTake5,
		"corpRoleContainerCanTake6":   corp.RoleContainerCanTake6,
		"corpRoleContainerCanTake7":   corp.RoleContainerCanTake7,
		"corpRoleCanRentOffice":       corp.RoleCanRentOffice,
		"corpRoleCanRentFactorySlot":  corp.RoleCanRentFactorySlot,
		"corpRoleCanRentResearchSlot": corp.RoleCanRentResearchSlot,
		"corpRoleJuniorAccountant":    corp.RoleJuniorAccountant,
		"corpRoleStarbaseConfig":      corp.RoleStarbaseConfig,
		"corpRoleTrader":              corp.RoleTrader,
		"corpRoleChatManager":         corp.RoleChatManager,
	}
	var fixed int
	for name, want := range fixes {
		// Only the clamped sentinel is rewritten; a value set by hand is left alone.
		res, err := db.ExecContext(ctx, `
			UPDATE eveConstants SET constantValue = ?
			WHERE constantID = ? AND constantValue = 4294967295`, want, name)
		if err != nil {
			return fmt.Errorf("RepairCorpRoleConstants %s: %w", name, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			fixed += int(n)
		}
	}
	if fixed > 0 {
		log.Printf("eveConstants: repaired %d truncated corpRole* values", fixed)
	}
	return nil
}
