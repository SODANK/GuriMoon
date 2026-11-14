package db

// War report: what the in-game browser's CONCORD Wars tab reads. Nothing is stored; a war's
// score and payment schedule are computed from the war row, the killmail tables and any open
// war bill. Side A is the attacker (wars.declaredByID) and B the defender (wars.againstID).
// Kills match on the corporation and alliance IDs a killmail snapshotted when it was written,
// never on current membership. A corp party matches on corp ID, an alliance party on alliance ID.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"
)

// Directions of a war killmail filter.
const (
	WarDirBoth     = 0 // every kill between the parties
	WarDirAKilledB = 1 // the attacker destroyed a defender ship
	WarDirBKilledA = 2 // the defender destroyed an attacker ship
)

// WarParty is one side of a war: a corporation, or an alliance.
type WarParty struct {
	ID       int64
	Alliance bool
}

func (p WarParty) victimClause() (string, []any) {
	if p.Alliance {
		return "k.victimAllianceID = ?", []any{p.ID}
	}
	return "k.victimCorpID = ?", []any{p.ID}
}

// attackerClause is an EXISTS rather than a join, so a four-pilot gang on one mail cannot
// multiply the row. The isNPC guard keeps NPC attackers from matching a faction's ID.
func (p WarParty) attackerClause() (string, []any) {
	col := "a.corpID"
	if p.Alliance {
		col = "a.allianceID"
	}
	return `EXISTS (SELECT 1 FROM killmail_attackers a
		 WHERE a.killID = k.killID AND a.isNPC = 0 AND ` + col + ` = ?)`, []any{p.ID}
}

// WarKillPair selects the killmails one war's parties took from each other.
type WarKillPair struct {
	A, B WarParty // A = attacker, B = defender
	Dir  int      // WarDir*
}

func (p WarKillPair) where() (string, []any) {
	vA, vAa := p.A.victimClause()
	aA, aAa := p.A.attackerClause()
	vB, vBa := p.B.victimClause()
	aB, aBa := p.B.attackerClause()
	aKilledB := func() (string, []any) {
		return "(" + vB + " AND " + aA + ")", append(append([]any{}, vBa...), aAa...)
	}
	bKilledA := func() (string, []any) {
		return "(" + vA + " AND " + aB + ")", append(append([]any{}, vAa...), aBa...)
	}
	switch p.Dir {
	case WarDirAKilledB:
		return aKilledB()
	case WarDirBKilledA:
		return bKilledA()
	}
	s1, a1 := aKilledB()
	s2, a2 := bKilledA()
	return "(" + s1 + " OR " + s2 + ")", append(a1, a2...)
}

// WarFileTimeToUnix converts a Win32 FILETIME to unix seconds; anything at or before 1970
// reads as 0.
func WarFileTimeToUnix(ft int64) int64 {
	if ft <= win32Epoch1970 {
		return 0
	}
	return (ft - win32Epoch1970) / 10_000_000
}

// WarWindow is the span in which a kill counts toward a war: from its arming (24 h after
// declaration) to timeFinished or 24 h after a retraction. Until is zero while the war is open.
func WarWindow(w War) (since, until time.Time) {
	since = time.Unix(WarFileTimeToUnix(w.TimeDeclared+int64(warHostileDelayTicks)), 0)
	switch {
	case w.TimeFinished.Valid:
		until = time.Unix(WarFileTimeToUnix(w.TimeFinished.Int64), 0)
	case w.Retracted.Valid:
		until = time.Unix(WarFileTimeToUnix(w.Retracted.Int64+int64(warHostileDelayTicks)), 0)
	}
	return since, until
}

// warPartyOf says whether an owner ID is an alliance. Unlike ClassifyWarOwner it ignores the
// deleted flag, since a dissolved alliance's killmails still carry its ID.
func warPartyOf(ctx context.Context, db *sql.DB, id int64) WarParty {
	var n int64
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM alliances WHERE allianceID = ?`, id).Scan(&n); err == nil && n > 0 {
		return WarParty{ID: id, Alliance: true}
	}
	return WarParty{ID: id}
}

// WarReport is a war plus everything needed to query its killmails.
type WarReport struct {
	War          War
	AttackerName string
	DefenderName string
	AttackerLogo int64 // corporation whose logo stands for the party (an alliance has no logo of its own here)
	DefenderLogo int64
	Pair         WarKillPair
	Since, Until time.Time
}

// LoadWarReport resolves one war, or returns (nil, nil) if there is no such war.
func LoadWarReport(ctx context.Context, db *sql.DB, warID int64) (*WarReport, error) {
	w, err := GetWar(ctx, db, warID)
	if err != nil || w == nil {
		return nil, err
	}
	r := &WarReport{War: *w}
	r.Pair = WarKillPair{A: warPartyOf(ctx, db, w.DeclaredByID), B: warPartyOf(ctx, db, w.AgainstID)}
	r.Since, r.Until = WarWindow(*w)
	r.AttackerName = warOwnerName(ctx, db, w.DeclaredByID)
	r.DefenderName = warOwnerName(ctx, db, w.AgainstID)
	r.AttackerLogo = warLogoCorp(ctx, db, r.Pair.A)
	r.DefenderLogo = warLogoCorp(ctx, db, r.Pair.B)
	return r, nil
}

// ReportForView builds the report of a war the list already resolved, saving
// the name and logo lookups.
func ReportForView(ctx context.Context, db *sql.DB, v WarView) WarReport {
	r := WarReport{War: v.War, AttackerName: v.DeclaredByName, DefenderName: v.AgainstName,
		AttackerLogo: v.DeclaredLogo, DefenderLogo: v.AgainstLogo}
	r.Pair = WarKillPair{A: warPartyOf(ctx, db, v.DeclaredByID), B: warPartyOf(ctx, db, v.AgainstID)}
	r.Since, r.Until = WarWindow(v.War)
	return r
}

// warLogoCorp is the corporation whose logo is drawn for a party. The client composites corp
// logos itself (corplogo:<id>) and cannot draw an alliance's, so the executor corp stands in.
func warLogoCorp(ctx context.Context, db *sql.DB, p WarParty) int64 {
	if !p.Alliance {
		return p.ID
	}
	var exec sql.NullInt64
	_ = db.QueryRowContext(ctx, `SELECT executorCorpID FROM alliances WHERE allianceID = ?`, p.ID).Scan(&exec)
	return exec.Int64
}

func warOwnerName(ctx context.Context, db *sql.DB, id int64) string {
	var name string
	if err := db.QueryRowContext(ctx, `
		SELECT COALESCE(
		  (SELECT allianceName FROM alliances WHERE allianceID = ?),
		  (SELECT corporationName FROM corporation WHERE corporationID = ?), '')`,
		id, id).Scan(&name); err != nil || name == "" {
		return fmt.Sprintf("owner %d", id)
	}
	return name
}

// Filter is the killmail filter for one direction of this war. Bots are not hidden: the record
// is already scoped to two named owners.
func (r WarReport) Filter(dir int) KillmailFilter {
	pair := r.Pair
	pair.Dir = dir
	return KillmailFilter{Since: r.Since, Until: r.Until, War: &pair}
}

// WarSideScore is what one side has destroyed.
type WarSideScore struct {
	Kills int64   // ships (pods included) it destroyed
	ISK   float64 // their assessed value
}

// WarScore is the header numbers of the War Report window: each side's kills and the ISK they
// destroyed. A's ISK is the total value of B's losses.
type WarScore struct{ A, B WarSideScore }

// Score runs the two directional totals.
func (r WarReport) Score(ctx context.Context, db *sql.DB) (WarScore, error) {
	var s WarScore
	var err error
	if s.A.Kills, s.A.ISK, err = KillmailTotals(ctx, db, r.Filter(WarDirAKilledB)); err != nil {
		return s, err
	}
	if s.B.Kills, s.B.ISK, err = KillmailTotals(ctx, db, r.Filter(WarDirBKilledA)); err != nil {
		return s, err
	}
	return s, nil
}

// ── Schedule ─────────────────────────────────────────────────────────────────

// WarState is where a war stands, in the words the page uses.
type WarState int

const (
	WarStateArming      WarState = iota // declared, not yet shootable (first 24 h)
	WarStateHostile                     // shootable
	WarStateWindingDown                 // retracted, still shootable until the cool-down ends
	WarStateEnded                       // finished
)

// WarOpenBill is the unpaid weekly fee, if one is outstanding.
type WarOpenBill struct {
	Amount  float64
	DueUnix int64 // unpaid past this and CONCORD ends the war
}

// WarSchedule is the war's calendar, all in unix seconds (0 = not applicable).
type WarSchedule struct {
	State        WarState
	Mutual       bool
	DeclaredUnix int64
	HostileFrom  int64 // declared + 24 h
	EndUnix      int64 // finished, or retracted + 24 h; 0 while the war is open-ended
	NextFeeUnix  int64 // when the sweep next issues a bill; 0 when no more fees will come
	WeeklyFee    float64
	PayerCorpID  int64 // the corp that is billed (the executor corp for an alliance); 0 = none
	PayerName    string
	OpenBill     *WarOpenBill
}

// deriveWarSchedule is the pure half of GetWarSchedule. lastBillFT is wars.lastBillTime (a
// FILETIME); now is unix seconds.
func deriveWarSchedule(w War, lastBillFT int64, open *WarOpenBill, now int64) WarSchedule {
	s := WarSchedule{
		Mutual:       w.Mutual != 0,
		DeclaredUnix: WarFileTimeToUnix(w.TimeDeclared),
		HostileFrom:  WarFileTimeToUnix(w.TimeDeclared + int64(warHostileDelayTicks)),
		OpenBill:     open,
	}
	retractEnd := int64(0)
	if w.Retracted.Valid {
		retractEnd = WarFileTimeToUnix(w.Retracted.Int64 + int64(warHostileDelayTicks))
	}
	switch {
	case w.TimeFinished.Valid:
		s.State = WarStateEnded
		s.EndUnix = WarFileTimeToUnix(w.TimeFinished.Int64)
	case w.Retracted.Valid && now >= retractEnd:
		// Cold already but the sweep has not stamped timeFinished yet; report what the war is.
		s.State = WarStateEnded
		s.EndUnix = retractEnd
	case w.Retracted.Valid:
		s.State = WarStateWindingDown
		s.EndUnix = retractEnd
	case now < s.HostileFrom:
		s.State = WarStateArming
	default:
		s.State = WarStateHostile
	}
	// No fee is issued for a war that is over, retracted or mutual, matching WarsDueForBilling.
	if s.State == WarStateHostile || s.State == WarStateArming {
		if w.Mutual == 0 {
			last := lastBillFT
			if last == 0 {
				last = w.TimeDeclared
			}
			s.NextFeeUnix = WarFileTimeToUnix(last + int64(WarBillPeriodTicks))
		}
	}
	if s.State == WarStateEnded {
		s.OpenBill = nil
	}
	return s
}

// GetWarSchedule reads the bookkeeping the page needs beyond the war row.
func GetWarSchedule(ctx context.Context, db *sql.DB, w War) (WarSchedule, error) {
	var last int64
	if err := db.QueryRowContext(ctx,
		`SELECT lastBillTime FROM wars WHERE warID = ?`, w.WarID).Scan(&last); err != nil {
		return WarSchedule{}, fmt.Errorf("GetWarSchedule: %w", err)
	}
	var open *WarOpenBill
	if err := EnsureBillTables(ctx, db); err == nil {
		var amt float64
		var due int64
		err := db.QueryRowContext(ctx, `
			SELECT amount, dueDateTime FROM bills
			 WHERE billTypeID = ? AND externalID2 = ? AND paid = 0
			 ORDER BY billID DESC LIMIT 1`, BillTypeWarBill, w.WarID).Scan(&amt, &due)
		if err == nil {
			open = &WarOpenBill{Amount: amt, DueUnix: WarFileTimeToUnix(due)}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return WarSchedule{}, fmt.Errorf("GetWarSchedule bill: %w", err)
		}
	}
	s := deriveWarSchedule(w, last, open, time.Now().Unix())
	s.WeeklyFee = WarWeeklyBillCost(ctx, db)

	// The payer is the declarer, or an alliance's executor corporation, as issueWarBill bills.
	s.PayerCorpID = w.DeclaredByID
	if warPartyOf(ctx, db, w.DeclaredByID).Alliance {
		s.PayerCorpID = 0
		_ = db.QueryRowContext(ctx,
			`SELECT executorCorpID FROM alliances WHERE allianceID = ?`, w.DeclaredByID).Scan(&s.PayerCorpID)
	}
	if s.PayerCorpID != 0 {
		s.PayerName = warOwnerName(ctx, db, s.PayerCorpID)
	}
	return s, nil
}

// ListWarsForRegistry returns the wars the registry lists: every undecided war (active) or the
// wars that finished in the last endedDays days (ended), newest first, capped at limit.
func ListWarsForRegistry(ctx context.Context, db *sql.DB, ended bool, endedDays, limit int) ([]WarView, error) {
	all, err := ListWars(ctx, db)
	if err != nil {
		return nil, err
	}
	cutoff := int64(win32TimeNow()) - int64(endedDays)*24*3600*10_000_000
	var out []WarView
	for _, v := range all {
		switch {
		case !ended && v.Active:
			out = append(out, v)
		case ended && !v.Active && v.TimeFinished.Int64 >= cutoff:
			out = append(out, v)
		}
	}
	if ended {
		sort.SliceStable(out, func(i, j int) bool {
			return out[i].TimeFinished.Int64 > out[j].TimeFinished.Int64
		})
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
