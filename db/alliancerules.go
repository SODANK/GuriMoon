package db

// Alliance rules the client states but never checks, so the server is the only gate. Every
// refusal is a ClientMsgError with the client's own message key: founding, applying, joining
// (war adoption) and optional expulsion for unpaid maintenance. The rules live in db so the
// rolebots module's headless CEOs meet them too. See docs/db-10-notes.md#alliance-rules.

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
)

// Name and ticker bounds. The name lengths are the client's message limits (4 and 24); the
// ticker's 5 is the create form's maxlength and its minimum of 2 is the server's own.
const (
	AllianceNameMinLen   = 4
	AllianceNameMaxLen   = 24
	AllianceTickerMinLen = 2
	AllianceTickerMaxLen = 5
)

func isASCIIAlnum(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

// AllianceNameRefusal applies the shape rules to a proposed name: length, first and last
// character, then the whole. Allowed are ASCII letters, digits, single spaces and - ' . ; the
// name starts with a letter or digit and ends with one or a full stop. nil when acceptable.
func AllianceNameRefusal(name string) error {
	n := len([]rune(name))
	if n < AllianceNameMinLen {
		return MsgErr("AllianceNameInvalidMinLength", nil,
			fmt.Sprintf("an alliance name needs at least %d characters", AllianceNameMinLen))
	}
	if n > AllianceNameMaxLen {
		return MsgErr("AllianceNameInvalidMaxLength", nil,
			fmt.Sprintf("an alliance name may have at most %d characters", AllianceNameMaxLen))
	}
	runes := []rune(name)
	if !isASCIIAlnum(runes[0]) {
		return MsgErr("AllianceNameInvalidFirstChar", nil, "the first character of the alliance name is illegal")
	}
	if last := runes[n-1]; !isASCIIAlnum(last) && last != '.' {
		return MsgErr("AllianceNameInvalidLastChar", nil, "the last character of the alliance name is illegal")
	}
	for i, r := range runes {
		switch {
		case isASCIIAlnum(r), r == '-', r == '\'', r == '.':
		case r == ' ' && runes[i-1] != ' ':
		default:
			return MsgErr("AllianceNameInvalidSomeChar", nil, "the alliance name contains an illegal character")
		}
	}
	return nil
}

// AllianceTickerRefusal applies the shape rules to a proposed ticker: 2-5 ASCII letters,
// digits, - . or _, at least one letter or digit, no spaces. bracketMgr prints it as [TICK].
func AllianceTickerRefusal(ticker string) error {
	bad := MsgErr("AllianceShortNameInvalid", nil,
		fmt.Sprintf("an alliance ticker is %d-%d letters or digits", AllianceTickerMinLen, AllianceTickerMaxLen))
	n := len([]rune(ticker))
	if n < AllianceTickerMinLen || n > AllianceTickerMaxLen {
		return bad
	}
	alnum := false
	for _, r := range ticker {
		switch {
		case isASCIIAlnum(r):
			alnum = true
		case r == '-', r == '.', r == '_':
		default:
			return bad
		}
	}
	if !alnum {
		return bad
	}
	return nil
}

// AllianceNamesTaken checks the name and ticker against every alliance ever registered,
// dissolved ones included, so one owner name never maps to two alliances in cfg.eveowners.
// Comparison follows the column collation (case-insensitive).
func AllianceNamesTaken(ctx context.Context, db *sql.DB, name, ticker string) error {
	if err := ensureAlliancesTable(ctx, db); err != nil {
		return err
	}
	var n int64
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM alliances WHERE allianceName = ?`, name).Scan(&n); err != nil {
		return fmt.Errorf("AllianceNamesTaken name: %w", err)
	}
	if n > 0 {
		return MsgErr("AllianceNameInvalidTaken", nil, "that alliance name is already taken")
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM alliances WHERE shortName = ?`, ticker).Scan(&n); err != nil {
		return fmt.Errorf("AllianceNamesTaken ticker: %w", err)
	}
	if n > 0 {
		return MsgErr("AllianceShortNameInvalidTaken", nil, "that alliance ticker is already taken")
	}
	return nil
}

// AllianceCreateRefusal is every gate on founding that does not depend on who is asking (the
// CEO and not-already-allied checks stay with the caller): no war the corporation declared may
// be in progress, and the name and ticker must be well formed and free. Run it before any fee
// is charged. The war refusal uses the catalogue's key CanNotAdoptWarsAsCorpHasDesclaredWarsInProgress.
// AdoptCorpWars carries defensive wars over.
func AllianceCreateRefusal(ctx context.Context, db *sql.DB, corpID int64, name, ticker string) error {
	if err := AllianceNameRefusal(name); err != nil {
		return err
	}
	if err := AllianceTickerRefusal(ticker); err != nil {
		return err
	}
	if err := EnsureWarTables(ctx, db); err != nil {
		return err
	}
	if n, err := CountActiveDeclaredWars(ctx, db, corpID); err != nil {
		return err
	} else if n > 0 {
		return MsgErr("CanNotAdoptWarsAsCorpHasDesclaredWarsInProgress", nil,
			"the corporation has declared wars still in progress")
	}
	return AllianceNamesTaken(ctx, db, name, ticker)
}

// CorpAtWarWithAlliance reports an undecided war (timeFinished IS NULL; retracted wars still
// cooling count) between corpID and the alliance or any corporation in it, in either direction.
func CorpAtWarWithAlliance(ctx context.Context, db *sql.DB, corpID, allianceID int64) (bool, error) {
	if err := EnsureWarTables(ctx, db); err != nil {
		return false, err
	}
	var n int64
	err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM wars w
		 WHERE w.timeFinished IS NULL
		   AND ((w.declaredByID = ? AND (w.againstID = ?
		          OR w.againstID IN (SELECT corporationID FROM corporation WHERE allianceID = ?)))
		     OR (w.againstID = ? AND (w.declaredByID = ?
		          OR w.declaredByID IN (SELECT corporationID FROM corporation WHERE allianceID = ?))))`,
		corpID, allianceID, allianceID, corpID, allianceID, allianceID).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("CorpAtWarWithAlliance: %w", err)
	}
	return n > 0, nil
}

// AllianceApplyRefusal is every gate on a corporation applying to an alliance (the Director
// check stays with the caller), each with the client's key. The client itself checks only
// CantApplyTwiceToAlliance, against its cached applications.
func AllianceApplyRefusal(ctx context.Context, db *sql.DB, corpID, allianceID int64) error {
	if err := EnsureAllianceTables(ctx, db); err != nil {
		return err
	}
	var currentAlliance int64
	if err := db.QueryRowContext(ctx,
		`SELECT allianceID FROM corporation WHERE corporationID = ?`, corpID,
	).Scan(&currentAlliance); err != nil {
		return fmt.Errorf("AllianceApplyRefusal corp: %w", err)
	}
	if currentAlliance != 0 && currentAlliance == allianceID {
		return MsgErr("AllApplyFailIsMember", nil, "your corporation is already in this alliance")
	}
	if currentAlliance != 0 {
		return MsgErr("AllianceJoinFailCorpInAlliance", nil,
			"your corporation must leave its alliance before applying to another")
	}
	var exists int64
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM alliances WHERE allianceID = ? AND deleted = 0`, allianceID,
	).Scan(&exists); err != nil {
		return fmt.Errorf("AllianceApplyRefusal alliance: %w", err)
	}
	if exists == 0 {
		return MsgErr("AllianceNotFound", nil, "no such alliance")
	}
	if err := allianceJoinWarRefusal(ctx, db, corpID, allianceID); err != nil {
		return err
	}
	var open int64
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM allianceApplications
		 WHERE allianceID = ? AND corporationID = ? AND deleted = 0
		   AND state IN (?, ?)`,
		allianceID, corpID, AllianceApplicationNew, AllianceApplicationAccepted,
	).Scan(&open); err != nil {
		return fmt.Errorf("AllianceApplyRefusal existing: %w", err)
	}
	if open > 0 {
		return MsgErr("CantApplyTwiceToAlliance", nil, "your corporation has already applied to this alliance")
	}
	return nil
}

// allianceJoinWarRefusal is the war half of applying, re-run when an accepted application
// becomes effective: a corporation with wars of its own declaring cannot join, and neither can
// one at war with the alliance.
func allianceJoinWarRefusal(ctx context.Context, db *sql.DB, corpID, allianceID int64) error {
	if err := EnsureWarTables(ctx, db); err != nil {
		return err
	}
	if n, err := CountActiveDeclaredWars(ctx, db, corpID); err != nil {
		return err
	} else if n > 0 {
		return MsgErr("CantApplyToAllianceHaveDeclaredWars", nil,
			"your corporation has declared wars still in progress")
	}
	if at, err := CorpAtWarWithAlliance(ctx, db, corpID, allianceID); err != nil {
		return err
	} else if at {
		return MsgErr("CantApplyToAllianceYouAreAtWarWith", nil,
			"your corporation is at war with that alliance")
	}
	return nil
}

// WarAdoption is one war moved from a corporation to the alliance it joined. Old is the war
// against the corporation. New is the same war against the alliance, or nil when the declarer
// was already at war with that alliance and the corporation's war was finished instead.
type WarAdoption struct {
	Old War
	New *War
}

// AdoptCorpWars retargets every undecided war declared against corpID onto the alliance it has
// just joined or founded, in place, so warID, arming time and bill schedule carry on. Wars the
// corporation declared itself are not adopted; the callers refuse the join while any is open.
// See docs/db-10-notes.md#alliance-war-adoption.
func AdoptCorpWars(ctx context.Context, db *sql.DB, corpID, allianceID int64) ([]WarAdoption, error) {
	if err := EnsureWarTables(ctx, db); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `
		SELECT warID, declaredByID, againstID, timeDeclared, timeFinished, retracted, mutual
		  FROM wars WHERE againstID = ? AND timeFinished IS NULL`, corpID)
	if err != nil {
		return nil, fmt.Errorf("AdoptCorpWars: %w", err)
	}
	var wars []War
	for rows.Next() {
		w, err := scanWar(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		wars = append(wars, w)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var out []WarAdoption
	for _, w := range wars {
		// Merge instead of retargeting when the declarer is the alliance, a member, or already at war
		// with the alliance (directly or through its own alliance).
		var declarerAlliance int64
		_ = db.QueryRowContext(ctx,
			`SELECT allianceID FROM corporation WHERE corporationID = ?`, w.DeclaredByID,
		).Scan(&declarerAlliance)
		merge := w.DeclaredByID == allianceID || declarerAlliance == allianceID
		for _, side := range []int64{w.DeclaredByID, declarerAlliance} {
			if merge || side == 0 {
				continue
			}
			if existing, err := FindActiveWar(ctx, db, side, allianceID); err != nil {
				return out, err
			} else if existing != nil {
				merge = true
			}
		}
		if merge {
			if _, err := FinishWar(ctx, db, w.WarID); err != nil {
				return out, err
			}
			out = append(out, WarAdoption{Old: w})
			continue
		}
		res, err := db.ExecContext(ctx,
			`UPDATE wars SET againstID = ? WHERE warID = ? AND againstID = ? AND timeFinished IS NULL`,
			allianceID, w.WarID, corpID)
		if err != nil {
			return out, fmt.Errorf("AdoptCorpWars retarget %d: %w", w.WarID, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		moved := w
		moved.AgainstID = allianceID
		out = append(out, WarAdoption{Old: w, New: &moved})
	}
	return out, nil
}

// ── Unpaid maintenance ───────────────────────────────────────────────────────

// Alliance maintenance enforcement is off by default. The first sweep that sees the switch on
// stamps settingAllianceExpelSince and only bills due after it count; switching off clears the
// stamp.
const (
	settingAllianceExpelUnpaid = "alliance.expelUnpaidMaintenance"
	settingAllianceExpelSince  = "alliance.expelUnpaidSince"
)

// AllianceExpelUnpaidSince reports whether an overdue alliance maintenance
// bill expels its debtor, and the Win32 time from which bills count.
func AllianceExpelUnpaidSince(ctx context.Context, db *sql.DB) (since uint64, on bool) {
	v, ok, err := GetServerSetting(ctx, db, settingAllianceExpelUnpaid)
	on = err == nil && ok && strings.TrimSpace(v) != "" && strings.TrimSpace(v) != "0"
	sv, sok, _ := GetServerSetting(ctx, db, settingAllianceExpelSince)
	stamp, _ := strconv.ParseUint(strings.TrimSpace(sv), 10, 64)
	if !on {
		if sok && stamp != 0 {
			_ = SetServerSetting(ctx, db, settingAllianceExpelSince, "0")
		}
		return 0, false
	}
	if stamp == 0 {
		stamp = win32TimeNow()
		if err := SetServerSetting(ctx, db, settingAllianceExpelSince,
			strconv.FormatUint(stamp, 10)); err != nil {
			return 0, false // no stamp, no enforcement
		}
	}
	return stamp, true
}

// OverdueMaintenanceBills returns unpaid alliance maintenance bills past their
// due date that fell due at or after since.
func OverdueMaintenanceBills(ctx context.Context, db *sql.DB, since uint64) ([]Bill, error) {
	all, err := OverdueUnpaidBills(ctx, db, BillTypeAllianceMaintainance)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, b := range all {
		if uint64(b.DueDateTime) >= since {
			out = append(out, b)
		}
	}
	return out, nil
}
