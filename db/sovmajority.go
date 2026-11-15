package db

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"gurimoon/syncx"
)

// Sovereignty by majority. Sov is recomputed from what stands in a 0.0 system: each online
// control tower scores 1 for its owner's current alliance, each outpost 2. The highest score holds
// the system, a tie keeps the incumbent, zero holds nothing. Anchored, onlining, reinforced and
// unanchoring towers score nothing, and systems held by an NPC faction are never contested.
// A holder changes only after the new majority has lasted sov.flipGraceHours (DecideSovFlip).
// Callers: InstallSovRecompute (every starbase lifecycle event), outpost commissioning,
// RecomputeSovForCorp (alliance join or leave), ReconcileAllSov (boot) and PromoteDueSov (the
// sov-grace periodic).
// See docs/db-8-notes.md#majority-sovereignty.

// Sov weights per qualifying structure.
const (
	SovTowerWeight   = 1
	SovOutpostWeight = 2
)

// SovOutpostIDFloor is the lowest stationID that counts as an outpost for sov: the three
// exported player outposts (61000001-3) and everything built above them.
const SovOutpostIDFloor = OutpostStationIDFloor - 3

// sovStationWhere is the test for a station that counts SovOutpostWeight toward sov: an outpost
// (stationID >= SovOutpostIDFloor, the one ? placeholder) or a conquerable station, held by
// whoever took it. CONCORD, the conquerables' first holder, is in no alliance and holds nothing.
const sovStationWhere = `(st.stationID >= ? OR st.stationTypeID IN
	(SELECT stationTypeID FROM staStationTypes WHERE conquerable = 1))`

// sovManual reports a hand-made row. The recompute writes the top contributing corporation and
// no character, so anything else came from the ESP sov painter (no corp, no character) or a GM's
// /sov claim (a character). The recompute replaces such a row only when a count names a holder.
func sovManual(rec SovRecord) bool { return !(rec.ClaimingCorpID != 0 && rec.ClaimingCharID == 0) }

// SovTally is one alliance's score in a system and the corporation contributing most to it
// (recorded as the claimant).
type SovTally struct {
	AllianceID int64
	Score      int
	TopCorpID  int64
	topCorpPts int
}

// PickSovHolder is the majority rule: the highest score holds. A tie keeps the incumbent if it
// is among the tied, else the lowest allianceID, so a tie never flips between reconciles. Zero
// scores hold nothing.
func PickSovHolder(tallies []SovTally, incumbent int64) int64 {
	best := 0
	for _, t := range tallies {
		if t.AllianceID != 0 && t.Score > best {
			best = t.Score
		}
	}
	if best <= 0 {
		return 0
	}
	var tied []int64
	for _, t := range tallies {
		if t.AllianceID != 0 && t.Score == best {
			if t.AllianceID == incumbent {
				return incumbent
			}
			tied = append(tied, t.AllianceID)
		}
	}
	sort.Slice(tied, func(i, j int) bool { return tied[i] < tied[j] })
	return tied[0]
}

// SovFlipGrace is how long a new majority must last before it takes a held system (setting
// sov.flipGraceHours, default 24, 0 flips at once).
func SovFlipGrace() time.Duration {
	return CachedSettingDuration("sov.flipGraceHours", time.Hour, 24, 0, 24*365)
}

// SovPending is a majority waiting out the flip grace: the alliance (0 is a loss to nobody) and
// the Win32 FILETIME it took the majority. Since 0 means nothing waits.
type SovPending struct {
	AllianceID int64
	Since      int64
}

// DecideSovFlip applies the flip grace to the majority's pick. flip says the system goes to
// winner now; next is the pending flip to store. An unheld system is claimed at once. A majority
// back with the holder clears the pending flip, a new challenger restarts the clock, and the same
// challenger flips once now is grace past its Since. Grace 0 flips at once.
func DecideSovFlip(owned bool, holder, winner int64, p SovPending, now int64, grace time.Duration) (flip bool, next SovPending) {
	switch {
	case !owned:
		return winner != 0, SovPending{}
	case winner == holder:
		return false, SovPending{}
	case grace <= 0:
		return true, SovPending{}
	case p.Since == 0 || p.AllianceID != winner:
		return false, SovPending{AllianceID: winner, Since: now}
	case time.Duration(now-p.Since)*100 >= grace: // FILETIME counts 100 ns
		return true, SovPending{}
	}
	return false, p
}

// SovTallies counts a system's qualifying structures by each owning corporation's current
// alliance, not the allianceID stamped on the tower when it was planted.
func SovTallies(ctx context.Context, db *sql.DB, systemID int64) ([]SovTally, error) {
	if err := ensureStarbaseTable(ctx, db); err != nil {
		return nil, err
	}
	by := map[int64]*SovTally{}
	add := func(allianceID, corpID int64, pts int) {
		if allianceID == 0 || pts <= 0 {
			return
		}
		t := by[allianceID]
		if t == nil {
			t = &SovTally{AllianceID: allianceID}
			by[allianceID] = t
		}
		t.Score += pts
		if pts > t.topCorpPts {
			t.topCorpPts, t.TopCorpID = pts, corpID
		}
	}
	rows, err := db.QueryContext(ctx, `
		SELECT COALESCE(co.allianceID, 0), s.ownerCorpID, COUNT(*)
		  FROM starbase s
		  JOIN corporation co ON co.corporationID = s.ownerCorpID
		 WHERE s.solarSystemID = ? AND s.state = ?
		 GROUP BY co.allianceID, s.ownerCorpID`, systemID, StarbaseStateOnline)
	if err != nil {
		return nil, fmt.Errorf("SovTallies towers: %w", err)
	}
	for rows.Next() {
		var a, c int64
		var n int
		if err := rows.Scan(&a, &c, &n); err != nil {
			rows.Close()
			return nil, err
		}
		add(a, c, n*SovTowerWeight)
	}
	rows.Close()
	orows, err := db.QueryContext(ctx, `
		SELECT COALESCE(co.allianceID, 0), st.corporationID, COUNT(*)
		  FROM staStations st
		  JOIN corporation co ON co.corporationID = st.corporationID
		 WHERE st.solarSystemID = ? AND `+sovStationWhere+`
		 GROUP BY co.allianceID, st.corporationID`, systemID, SovOutpostIDFloor)
	if err != nil {
		return nil, fmt.Errorf("SovTallies outposts: %w", err)
	}
	for orows.Next() {
		var a, c int64
		var n int
		if err := orows.Scan(&a, &c, &n); err != nil {
			orows.Close()
			return nil, err
		}
		add(a, c, n*SovOutpostWeight)
	}
	orows.Close()
	out := make([]SovTally, 0, len(by))
	for _, t := range by {
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AllianceID < out[j].AllianceID })
	return out, nil
}

// sovSystemLocks serialises recomputes per system so two events cannot interleave
// read-tally-write.
var sovSystemLocks syncx.Map[int64, *sync.Mutex] // systemID -> *sync.Mutex

// SovChange reports what a recompute did.
type SovChange struct {
	SystemID    int64
	OldAlliance int64
	NewAlliance int64
	Changed     bool
}

// SovChangeHook is set by a module to learn of sov flips. Called on its own goroutine behind a
// recover for every flip a recompute makes.
var SovChangeHook func(SovChange)

// RecomputeSystemSov applies the majority rule to one system. NPC-faction
// systems and empire space are left alone.
func RecomputeSystemSov(ctx context.Context, db *sql.DB, systemID int64) (SovChange, error) {
	ch := SovChange{SystemID: systemID}
	if systemID == 0 {
		return ch, nil
	}
	v, _ := sovSystemLocks.LoadOrStore(systemID, &sync.Mutex{})
	mu := v
	mu.Lock()
	defer mu.Unlock()

	factionID, sec, ok, err := GetSystemFactionAndSecurity(ctx, db, systemID)
	if err != nil || !ok {
		return ch, err
	}
	if sec >= 0.0 || factionID != 0 {
		return ch, nil // empire space, or an NPC faction's 0.0: not contested
	}
	cur, owned, err := GetSov(ctx, db, systemID)
	if err != nil {
		return ch, err
	}
	if owned {
		ch.OldAlliance = cur.AllianceID
	}
	tallies, err := SovTallies(ctx, db, systemID)
	if err != nil {
		return ch, err
	}
	counted := PickSovHolder(tallies, ch.OldAlliance)
	winner := counted
	if owned && counted == 0 && sovManual(cur) {
		winner = cur.AllianceID // a hand-painted holder stays until a count names another
	}
	pending := SovPending{AllianceID: cur.PendingAllianceID, Since: cur.PendingSince}
	flip, next := DecideSovFlip(owned, ch.OldAlliance, winner, pending, Win32Now(), SovFlipGrace())
	if !flip {
		ch.NewAlliance = ch.OldAlliance
		if owned && next != pending {
			if err := SetSovPending(ctx, db, systemID, next.AllianceID, next.Since); err != nil {
				return ch, err
			}
			if next.Since != 0 {
				log.Printf("[sov] system %d: alliance %d holds the majority, takes it from %d after %s",
					systemID, next.AllianceID, ch.OldAlliance, SovFlipGrace())
			} else {
				log.Printf("[sov] system %d: majority back with %d, pending flip to %d dropped",
					systemID, ch.OldAlliance, pending.AllianceID)
			}
		}
		if !owned || counted == 0 || counted != cur.AllianceID || !sovManual(cur) {
			return ch, nil
		}
		// A count confirms a hand-made holder: rewrite it as a recompute row below.
	}
	ch.NewAlliance = winner
	if winner == 0 {
		if _, err := ClearSov(ctx, db, systemID); err != nil {
			return ch, err
		}
		ch.Changed = true
	} else {
		var topCorp int64
		for _, t := range tallies {
			if t.AllianceID == winner {
				topCorp = t.TopCorpID
			}
		}
		if err := SetSov(ctx, db, systemID, winner, topCorp, 0); err != nil {
			return ch, err
		}
		ch.Changed = ch.OldAlliance != winner
	}
	if ch.Changed {
		log.Printf("[sov] system %d: alliance %d → %d (majority of qualifying structures)",
			systemID, ch.OldAlliance, ch.NewAlliance)
		if SovChangeHook != nil {
			go func(c SovChange) {
				defer func() { _ = recover() }()
				SovChangeHook(c)
			}(ch)
		}
	}
	return ch, nil
}

// RecomputeSovForCorp recomputes every system a corporation holds a tower or outpost in, which is
// what an alliance change moves. It also re-stamps the corporation's 0.0 towers with its current
// alliance, which StarbaseManagePermitted and the sov fuel discount read.
func RecomputeSovForCorp(ctx context.Context, db *sql.DB, corpID int64) {
	if corpID == 0 {
		return
	}
	if err := ensureStarbaseTable(ctx, db); err != nil {
		return
	}
	var alliance sql.NullInt64
	_ = db.QueryRowContext(ctx, `SELECT allianceID FROM corporation WHERE corporationID = ?`, corpID).Scan(&alliance)
	_, _ = db.ExecContext(ctx, `
		UPDATE starbase s JOIN mapSolarSystems m ON m.solarSystemID = s.solarSystemID
		   SET s.ownerAllianceID = ?
		 WHERE s.ownerCorpID = ? AND m.security < 0.0`, alliance.Int64, corpID)
	_, _ = db.ExecContext(ctx, `
		UPDATE starbaseStructures s JOIN mapSolarSystems m ON m.solarSystemID = s.solarSystemID
		   SET s.ownerAllianceID = ?
		 WHERE s.ownerCorpID = ? AND m.security < 0.0`, alliance.Int64, corpID)
	rows, err := db.QueryContext(ctx, `
		SELECT DISTINCT solarSystemID FROM starbase WHERE ownerCorpID = ?
		UNION
		SELECT DISTINCT solarSystemID FROM staStations st WHERE corporationID = ? AND `+sovStationWhere,
		corpID, corpID, SovOutpostIDFloor)
	if err != nil {
		log.Printf("[sov] corp %d systems: %v", corpID, err)
		return
	}
	var systems []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			systems = append(systems, id)
		}
	}
	rows.Close()
	for _, sys := range systems {
		if _, err := RecomputeSystemSov(ctx, db, sys); err != nil {
			log.Printf("[sov] recompute %d: %v", sys, err)
		}
	}
}

// ReconcileAllSov recomputes every system with a tower, an outpost or an existing claim. Run at
// boot so towers that changed state while the server was down settle before anyone looks.
func ReconcileAllSov(ctx context.Context, db *sql.DB) (changed int, err error) {
	if err := ensureStarbaseTable(ctx, db); err != nil {
		return 0, err
	}
	if err := ensureSolarSystemSovTable(ctx, db); err != nil {
		return 0, err
	}
	rows, err := db.QueryContext(ctx, `
		SELECT solarSystemID FROM starbase
		UNION SELECT solarSystemID FROM staStations st WHERE `+sovStationWhere+`
		UNION SELECT solarSystemID FROM solarSystemSov`, SovOutpostIDFloor)
	if err != nil {
		return 0, fmt.Errorf("ReconcileAllSov: %w", err)
	}
	var systems []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			systems = append(systems, id)
		}
	}
	rows.Close()
	for _, sys := range systems {
		if ctx.Err() != nil {
			return changed, ctx.Err()
		}
		c, err := RecomputeSystemSov(ctx, db, sys)
		if err != nil {
			log.Printf("[sov] reconcile %d: %v", sys, err)
			continue
		}
		if c.Changed {
			changed++
		}
	}
	return changed, nil
}

// PromoteDueSov recomputes every system whose pending flip has waited out the grace, so a flip
// lands without a starbase event. The sov-grace periodic in cmd/gurimoon/main.go runs it.
func PromoteDueSov(ctx context.Context, db *sql.DB) (changed int, err error) {
	before := Win32Now() - int64(SovFlipGrace()/100)
	systems, err := ListSovPendingDue(ctx, db, before)
	if err != nil {
		return 0, err
	}
	for _, sys := range systems {
		if ctx.Err() != nil {
			return changed, ctx.Err()
		}
		c, err := RecomputeSystemSov(ctx, db, sys)
		if err != nil {
			log.Printf("[sov] promote %d: %v", sys, err)
			continue
		}
		if c.Changed {
			changed++
		}
	}
	return changed, nil
}

var sovRecomputeOnce sync.Once

// InstallSovRecompute subscribes the majority rule to every starbase lifecycle event. Idempotent.
func InstallSovRecompute(db *sql.DB) {
	if db == nil {
		return
	}
	sovRecomputeOnce.Do(func() {
		SubscribeStarbaseEvents(func(ev StarbaseEvent) {
			switch ev.Kind {
			case StarbaseEventFuelLow, StarbaseEventStrontiumLow:
				return // nothing that changes a count
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if _, err := RecomputeSystemSov(ctx, db, ev.SolarSystemID); err != nil {
				log.Printf("[sov] recompute on %s/%d: %v", ev.Kind, ev.TowerID, err)
			}
		})
	})
}
