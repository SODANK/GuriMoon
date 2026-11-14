package db

// Wars: the state behind the client's corp/alliance Wars tab and the overview's
// war targeting. The client reads a seven-column rowset (corp_ui_wars.py,
// war_cso_wars.py): timeDeclared is a NOT NULL Win32 FILETIME, retracted and
// timeFinished are None when unset (never 0), mutual is an int 0 or 1.
// See docs/corp-notes.md#wars.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gurimoon/marshal"
)

// warHostileDelayTicks is the client's 24-hour arming and cool-down window in
// FILETIME ticks (cfg.py: HOUR = 10000000 * 3600).
const warHostileDelayTicks = uint64(24) * 3600 * 10000000

// WarBillPeriodTicks is how often CONCORD bills the aggressor to keep a war
// going, in FILETIME ticks. The sweep and the IGB war page both read it.
const WarBillPeriodTicks = uint64(7) * 24 * 3600 * 10000000

// warCols is the header the client reads (corp_ui_wars.py:103,
// war_cso_wars.py:39-45). An OnWarChanged ADD must carry one entry per column
// or the client discards it.
var warCols = []string{
	"warID", "declaredByID", "againstID",
	"timeDeclared", "timeFinished", "retracted", "mutual",
}

// War is one row of the wars table.
type War struct {
	WarID        int64
	DeclaredByID int64
	AgainstID    int64
	TimeDeclared int64
	TimeFinished sql.NullInt64
	Retracted    sql.NullInt64
	Mutual       int64
}

// Hostile mirrors the client's cfg.IsWarInHostileState exactly, so the CONCORD
// legality gate agrees with what the client paints as shootable.
func (w War) Hostile(nowTicks uint64) bool {
	if w.TimeFinished.Valid {
		return false // finished wars are skipped before the test
	}
	if uint64(nowTicks)-uint64(w.TimeDeclared) < warHostileDelayTicks {
		return false
	}
	if w.Retracted.Valid && nowTicks-uint64(w.Retracted.Int64) >= warHostileDelayTicks {
		return false
	}
	return true
}

// warTablesOnce keeps the DDL off the GetWars read path (see ensureGuard); the
// client calls GetWars on every grid entry.
var warTablesOnce ensureGuard

// EnsureWarTables creates the wars table. lastBillTime is server bookkeeping
// and is never sent: the rowset selects exactly warCols.
func EnsureWarTables(ctx context.Context, db *sql.DB) error {
	return warTablesOnce.do(func() error { return ensureWarTables(ctx, db) })
}

func ensureWarTables(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS wars (
		  warID        INT UNSIGNED    NOT NULL AUTO_INCREMENT,
		  declaredByID INT UNSIGNED    NOT NULL,
		  againstID    INT UNSIGNED    NOT NULL,
		  timeDeclared BIGINT UNSIGNED NOT NULL,
		  timeFinished BIGINT UNSIGNED DEFAULT NULL,
		  retracted    BIGINT UNSIGNED DEFAULT NULL,
		  mutual       TINYINT UNSIGNED NOT NULL DEFAULT 0,
		  lastBillTime BIGINT UNSIGNED NOT NULL DEFAULT 0,
		  PRIMARY KEY (warID),
		  KEY declaredBy (declaredByID),
		  KEY against (againstID)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`)
	if err != nil {
		return fmt.Errorf("EnsureWarTables: %w", err)
	}
	return nil
}

// GetWarsRowset returns every war an owner is a party to, keyed by warID. The
// container must be a util.IndexRowset (war_cso_wars calls .itervalues() and
// OnWarChanged reads .header). ownerID is a call argument, not the bind owner.
func GetWarsRowset(ctx context.Context, db *sql.DB, ownerID int64) (marshal.PyRep, error) {
	if err := EnsureWarTables(ctx, db); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `
		SELECT warID, declaredByID, againstID, timeDeclared, timeFinished, retracted, mutual
		  FROM wars
		 WHERE declaredByID = ? OR againstID = ?
		 ORDER BY warID`, ownerID, ownerID)
	if err != nil {
		return nil, fmt.Errorf("GetWarsRowset: %w", err)
	}
	defer rows.Close()

	var out [][]marshal.PyRep
	for rows.Next() {
		w, err := scanWar(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, warRowReps(w))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return makeIndexRowset("warID", warCols, out), nil
}

func scanWar(rows *sql.Rows) (War, error) {
	var w War
	if err := rows.Scan(&w.WarID, &w.DeclaredByID, &w.AgainstID,
		&w.TimeDeclared, &w.TimeFinished, &w.Retracted, &w.Mutual); err != nil {
		return w, fmt.Errorf("scanWar: %w", err)
	}
	return w, nil
}

// warRowReps renders one war in warCols order with the None-vs-0 rules.
func warRowReps(w War) []marshal.PyRep {
	nullTime := func(v sql.NullInt64) marshal.PyRep {
		if !v.Valid {
			return marshal.PyNone{}
		}
		return marshal.PyInt{Value: uint64(v.Int64)}
	}
	return []marshal.PyRep{
		intRep(w.WarID), intRep(w.DeclaredByID), intRep(w.AgainstID),
		intRep(w.TimeDeclared), nullTime(w.TimeFinished), nullTime(w.Retracted),
		intRep(w.Mutual),
	}
}

// WarChangeReps returns the war row as (old, new) rep pairs for OnWarChanged.
// added: all olds None (client ADD). removed: all news None (client REMOVE).
// Otherwise both sides carry the current value; the client's strict reader
// raises on a dict whose every entry is (None, None).
func WarChangeReps(w War, added, removed bool) map[string][2]marshal.PyRep {
	cur := warRowReps(w)
	out := make(map[string][2]marshal.PyRep, len(warCols))
	for i, col := range warCols {
		oldV, newV := cur[i], cur[i]
		if added {
			oldV = marshal.PyNone{}
		}
		if removed {
			newV = marshal.PyNone{}
		}
		out[col] = [2]marshal.PyRep{oldV, newV}
	}
	return out
}

// WarColumns exposes the header order for the change-dict builders.
func WarColumns() []string { return warCols }

// GetWar reads one war.
func GetWar(ctx context.Context, db *sql.DB, warID int64) (*War, error) {
	if err := EnsureWarTables(ctx, db); err != nil {
		return nil, err
	}
	var w War
	err := db.QueryRowContext(ctx, `
		SELECT warID, declaredByID, againstID, timeDeclared, timeFinished, retracted, mutual
		  FROM wars WHERE warID = ?`, warID,
	).Scan(&w.WarID, &w.DeclaredByID, &w.AgainstID,
		&w.TimeDeclared, &w.TimeFinished, &w.Retracted, &w.Mutual)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("GetWar: %w", err)
	}
	return &w, nil
}

// WarOwnerKind classifies a war party.
type WarOwnerKind int

const (
	WarOwnerNone WarOwnerKind = iota
	WarOwnerCorp
	WarOwnerAlliance
)

// ClassifyWarOwner reports whether ownerID is a player corporation, a player
// alliance, or neither. Only player entities can be a war party; the ID
// arrives raw on the wire, so the client's picker filter is not enough.
func ClassifyWarOwner(ctx context.Context, db *sql.DB, ownerID int64) (WarOwnerKind, error) {
	if ownerID < MinPlayerEntityID {
		return WarOwnerNone, nil
	}
	var n int64
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM alliances WHERE allianceID = ? AND deleted = 0`, ownerID).Scan(&n); err != nil {
		return WarOwnerNone, fmt.Errorf("ClassifyWarOwner: %w", err)
	}
	if n > 0 {
		return WarOwnerAlliance, nil
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM corporation WHERE corporationID = ? AND deleted = 0`, ownerID).Scan(&n); err != nil {
		return WarOwnerNone, fmt.Errorf("ClassifyWarOwner: %w", err)
	}
	if n > 0 {
		return WarOwnerCorp, nil
	}
	return WarOwnerNone, nil
}

// FindActiveWar returns the undecided war between two owners, if any, in either
// direction (the AlreadyAtWarWithThem gate).
func FindActiveWar(ctx context.Context, db *sql.DB, a, b int64) (*War, error) {
	if err := EnsureWarTables(ctx, db); err != nil {
		return nil, err
	}
	var w War
	err := db.QueryRowContext(ctx, `
		SELECT warID, declaredByID, againstID, timeDeclared, timeFinished, retracted, mutual
		  FROM wars
		 WHERE timeFinished IS NULL
		   AND ((declaredByID = ? AND againstID = ?) OR (declaredByID = ? AND againstID = ?))
		 LIMIT 1`, a, b, b, a,
	).Scan(&w.WarID, &w.DeclaredByID, &w.AgainstID,
		&w.TimeDeclared, &w.TimeFinished, &w.Retracted, &w.Mutual)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("FindActiveWar: %w", err)
	}
	return &w, nil
}

// CountActiveDeclaredWars counts the undecided wars an owner has declared
// (the CrpTooManyWarDeclarations gate).
func CountActiveDeclaredWars(ctx context.Context, db *sql.DB, ownerID int64) (int64, error) {
	var n int64
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM wars WHERE declaredByID = ? AND timeFinished IS NULL`, ownerID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("CountActiveDeclaredWars: %w", err)
	}
	return n, nil
}

// InsertWar creates the war row. backdate declares it 24 hours in the past so
// it is hostile at once; nothing on the wire can shorten the arming delay.
func InsertWar(ctx context.Context, db *sql.DB, declaredByID, againstID int64, backdate bool) (*War, error) {
	if err := EnsureWarTables(ctx, db); err != nil {
		return nil, err
	}
	declared := win32TimeNow()
	if backdate {
		declared -= warHostileDelayTicks + 60*10000000 // 24h plus one minute of margin
	}
	res, err := db.ExecContext(ctx, `
		INSERT INTO wars (declaredByID, againstID, timeDeclared, mutual, lastBillTime)
		VALUES (?, ?, ?, 0, ?)`,
		declaredByID, againstID, declared, declared)
	if err != nil {
		return nil, fmt.Errorf("InsertWar: %w", err)
	}
	warID, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("InsertWar id: %w", err)
	}
	EmitWorldEvent(WarEvent{At: time.Now(), DeclaredByID: declaredByID, AgainstID: againstID})
	return &War{
		WarID: warID, DeclaredByID: declaredByID, AgainstID: againstID,
		TimeDeclared: int64(declared),
	}, nil
}

// RetractWar stamps the retraction. The war stays hostile for 24 more hours
// and the sweep finishes it after that.
func RetractWar(ctx context.Context, db *sql.DB, warID int64) error {
	res, err := db.ExecContext(ctx,
		`UPDATE wars SET retracted = ? WHERE warID = ? AND retracted IS NULL AND timeFinished IS NULL`,
		win32TimeNow(), warID)
	if err != nil {
		return fmt.Errorf("RetractWar: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("the war is already retracted or over")
	}
	return nil
}

// SetWarMutual flips the mutual flag. A mutual war is not billed.
func SetWarMutual(ctx context.Context, db *sql.DB, warID int64, mutual bool) error {
	m := int64(0)
	if mutual {
		m = 1
	}
	res, err := db.ExecContext(ctx,
		`UPDATE wars SET mutual = ? WHERE warID = ? AND timeFinished IS NULL`, m, warID)
	if err != nil {
		return fmt.Errorf("SetWarMutual: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no such active war")
	}
	return nil
}

// FinishWar stamps timeFinished. Idempotent: the sweep and an unpaid bill can
// both end the same war.
func FinishWar(ctx context.Context, db *sql.DB, warID int64) (bool, error) {
	res, err := db.ExecContext(ctx,
		`UPDATE wars SET timeFinished = ? WHERE warID = ? AND timeFinished IS NULL`,
		win32TimeNow(), warID)
	if err != nil {
		return false, fmt.Errorf("FinishWar: %w", err)
	}
	n, _ := res.RowsAffected()
	if n > 0 && WorldEventsWanted() {
		var by, against int64
		if db.QueryRowContext(ctx, `SELECT declaredByID, againstID FROM wars WHERE warID = ?`, warID).Scan(&by, &against) == nil {
			EmitWorldEvent(WarEvent{At: time.Now(), DeclaredByID: by, AgainstID: against, Ended: true})
		}
	}
	return n > 0, nil
}

// ExpiredRetractedWars returns active wars whose retraction cool-down has
// elapsed.
func ExpiredRetractedWars(ctx context.Context, db *sql.DB) ([]War, error) {
	if err := EnsureWarTables(ctx, db); err != nil {
		return nil, err
	}
	cutoff := win32TimeNow() - warHostileDelayTicks
	rows, err := db.QueryContext(ctx, `
		SELECT warID, declaredByID, againstID, timeDeclared, timeFinished, retracted, mutual
		  FROM wars
		 WHERE timeFinished IS NULL AND retracted IS NOT NULL AND retracted <= ?`, cutoff)
	if err != nil {
		return nil, fmt.Errorf("ExpiredRetractedWars: %w", err)
	}
	defer rows.Close()
	var out []War
	for rows.Next() {
		w, err := scanWar(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// WarsDueForBilling returns active, unretracted, non-mutual wars whose last
// bill is more than a week old.
func WarsDueForBilling(ctx context.Context, db *sql.DB) ([]War, error) {
	if err := EnsureWarTables(ctx, db); err != nil {
		return nil, err
	}
	cutoff := win32TimeNow() - WarBillPeriodTicks
	rows, err := db.QueryContext(ctx, `
		SELECT warID, declaredByID, againstID, timeDeclared, timeFinished, retracted, mutual
		  FROM wars
		 WHERE timeFinished IS NULL AND retracted IS NULL AND mutual = 0
		   AND lastBillTime <= ?`, cutoff)
	if err != nil {
		return nil, fmt.Errorf("WarsDueForBilling: %w", err)
	}
	defer rows.Close()
	var out []War
	for rows.Next() {
		w, err := scanWar(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// StampWarBilled records that this cycle's bill has been issued.
func StampWarBilled(ctx context.Context, db *sql.DB, warID int64) error {
	_, err := db.ExecContext(ctx,
		`UPDATE wars SET lastBillTime = ? WHERE warID = ?`, win32TimeNow(), warID)
	if err != nil {
		return fmt.Errorf("StampWarBilled: %w", err)
	}
	return nil
}

// ListWars returns every war with both party names resolved, for the ESP panel.
type WarView struct {
	War
	DeclaredByName string
	AgainstName    string
	DeclaredLogo   int64 // corporation whose logo stands for the party (an alliance uses its executor)
	AgainstLogo    int64
	HostileNow     bool
	Active         bool
}

func ListWars(ctx context.Context, db *sql.DB) ([]WarView, error) {
	if err := EnsureWarTables(ctx, db); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `
		SELECT w.warID, w.declaredByID, w.againstID, w.timeDeclared, w.timeFinished,
		       w.retracted, w.mutual,
		       COALESCE(dc.corporationName, da.allianceName, ''),
		       COALESCE(ac.corporationName, aa.allianceName, ''),
		       COALESCE(dc.corporationID, da.executorCorpID, 0),
		       COALESCE(ac.corporationID, aa.executorCorpID, 0)
		  FROM wars w
		  LEFT JOIN corporation dc ON dc.corporationID = w.declaredByID
		  LEFT JOIN alliances da ON da.allianceID = w.declaredByID
		  LEFT JOIN corporation ac ON ac.corporationID = w.againstID
		  LEFT JOIN alliances aa ON aa.allianceID = w.againstID
		 ORDER BY (w.timeFinished IS NOT NULL), w.warID DESC`)
	if err != nil {
		return nil, fmt.Errorf("ListWars: %w", err)
	}
	defer rows.Close()

	now := win32TimeNow()
	var out []WarView
	for rows.Next() {
		var v WarView
		if err := rows.Scan(&v.WarID, &v.DeclaredByID, &v.AgainstID,
			&v.TimeDeclared, &v.TimeFinished, &v.Retracted, &v.Mutual,
			&v.DeclaredByName, &v.AgainstName, &v.DeclaredLogo, &v.AgainstLogo); err != nil {
			return nil, fmt.Errorf("ListWars scan: %w", err)
		}
		v.HostileNow = v.Hostile(now)
		v.Active = !v.TimeFinished.Valid
		out = append(out, v)
	}
	return out, rows.Err()
}

// HostileWarBetween reports whether any war between the two owner sets is in
// the client's hostile state. The CONCORD gate passes each pilot's corp and
// alliance IDs because the client keys on `allianceID or corpID`.
func HostileWarBetween(ctx context.Context, db *sql.DB, sideA, sideB []int64) (bool, error) {
	var a, b []string
	for _, id := range sideA {
		if id > 0 {
			a = append(a, strconv.FormatInt(id, 10))
		}
	}
	for _, id := range sideB {
		if id > 0 {
			b = append(b, strconv.FormatInt(id, 10))
		}
	}
	if len(a) == 0 || len(b) == 0 {
		return false, nil
	}
	inA, inB := strings.Join(a, ","), strings.Join(b, ",")
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`
		SELECT warID, declaredByID, againstID, timeDeclared, timeFinished, retracted, mutual
		  FROM wars
		 WHERE timeFinished IS NULL
		   AND ((declaredByID IN (%s) AND againstID IN (%s))
		     OR (declaredByID IN (%s) AND againstID IN (%s)))`, inA, inB, inB, inA))
	if err != nil {
		return false, fmt.Errorf("HostileWarBetween: %w", err)
	}
	defer rows.Close()
	now := win32TimeNow()
	for rows.Next() {
		w, err := scanWar(rows)
		if err != nil {
			return false, err
		}
		if w.Hostile(now) {
			return true, nil
		}
	}
	return false, rows.Err()
}

// ---- Costs ----

// War cost settings. eveConstants carries refWarFee (49) and voteWar but no
// fee amount, so the amounts are server settings editable on the ESP wars page.
const (
	settingWarDeclarationCost = "war.declarationCost"
	settingWarWeeklyBillCost  = "war.weeklyBillCost"
	settingWarMaxDeclarations = "war.maxConcurrentDeclarations"

	DefaultWarDeclarationCost = 10000000.0 // one-off, at declaration
	DefaultWarWeeklyBillCost  = 2000000.0  // weekly CONCORD bill
	DefaultWarMaxDeclarations = 3
)

func warSettingFloat(ctx context.Context, db *sql.DB, key string, def float64) float64 {
	v, ok, err := GetServerSetting(ctx, db, key)
	if err != nil || !ok {
		return def
	}
	f, perr := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if perr != nil || f < 0 {
		return def
	}
	return f
}

// WarDeclarationCost is what GetCostOfStartingAWarAgainst quotes and what a
// declaration charges. It is always a number: FmtISK raises on None.
func WarDeclarationCost(ctx context.Context, db *sql.DB) float64 {
	return warSettingFloat(ctx, db, settingWarDeclarationCost, DefaultWarDeclarationCost)
}

// WarWeeklyBillCost is the recurring CONCORD bill for continuing the war.
func WarWeeklyBillCost(ctx context.Context, db *sql.DB) float64 {
	return warSettingFloat(ctx, db, settingWarWeeklyBillCost, DefaultWarWeeklyBillCost)
}

// WarMaxDeclarations caps concurrent outbound wars per owner.
func WarMaxDeclarations(ctx context.Context, db *sql.DB) int64 {
	return int64(warSettingFloat(ctx, db, settingWarMaxDeclarations, DefaultWarMaxDeclarations))
}

// SetWarCostSettings writes the three settings from the ESP page.
func SetWarCostSettings(ctx context.Context, db *sql.DB, declCost, weeklyCost float64, maxDecl int64) error {
	if err := SetServerSetting(ctx, db, settingWarDeclarationCost,
		strconv.FormatFloat(declCost, 'f', -1, 64)); err != nil {
		return err
	}
	if err := SetServerSetting(ctx, db, settingWarWeeklyBillCost,
		strconv.FormatFloat(weeklyCost, 'f', -1, 64)); err != nil {
		return err
	}
	return SetServerSetting(ctx, db, settingWarMaxDeclarations,
		strconv.FormatInt(maxDecl, 10))
}
