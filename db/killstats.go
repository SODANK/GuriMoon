package db

// Killboard profile aggregates for a pilot or corporation page. Everything reads the
// killmail tables, whose names, corps, hulls and ISK values are snapshots taken at kill time.
// A participant is credited once per killmail, and queries are not filtered by victimIsBot.
// See docs/db-5-notes.md#killboard-profile-aggregates.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// KillActor names whose record is being summarised. Exactly one field is set.
// The corporation case does not exclude NPCs: a player corp never has isNPC = 1 rows, and
// the filter would blank the record of a pirate faction.
type KillActor struct {
	CharID int64
	CorpID int64
}

// PilotActor and CorpActor are the constructors; they keep both fields from being set.
func PilotActor(charID int64) KillActor { return KillActor{CharID: charID} }
func CorpActor(corpID int64) KillActor  { return KillActor{CorpID: corpID} }

func (a KillActor) valid() bool { return a.CharID != 0 || a.CorpID != 0 }

// attackerPred is the "kills BY this actor" half, against killmail_attackers.
// Both forms are a leading-column index seek (idx_kma_char / idx_kma_corp).
func (a KillActor) attackerPred() (string, []any) {
	if a.CharID != 0 {
		// Implied by CharID <> 0; stated so both branches have the same plan shape.
		return "a.charID = ?", []any{a.CharID}
	}
	return "a.corpID = ?", []any{a.CorpID}
}

// victimPred is the "losses BY this actor" half, against killmails.
func (a KillActor) victimPred() (string, []any) {
	if a.CharID != 0 {
		return "k.victimCharID = ?", []any{a.CharID}
	}
	return "k.victimCorpID = ?", []any{a.CorpID}
}

// KillActorStats is the headline row of a profile page. Totals span the retention boundary:
// live killmail rows plus the rollup of synthetic kills already aged out (killretention.go).
// Archived records how much came from the rollup.
type KillActorStats struct {
	Kills        int64
	Losses       int64
	ISKDestroyed float64
	ISKLost      float64
	SoloKills    int64 // kills where this actor was the only party on the mail
	FinalBlows   int64
	LastKill     time.Time
	LastLoss     time.Time

	// Gang size over the kills whose mails still exist; the rollup keeps no attacker counts.
	// AvgGang divides by GangKills, not Kills.
	GangKills  int64
	GangPilots int64

	Archived *KillRollup // nil or empty when nothing of theirs has aged out
}

// AvgGang is the mean number of parties on this actor's kills, 0 with none.
func (s KillActorStats) AvgGang() float64 {
	if s.GangKills <= 0 {
		return 0
	}
	return float64(s.GangPilots) / float64(s.GangKills)
}

// ShipEfficiency is kills over kills+losses, counted in hulls rather than ISK.
// 0 for an empty record, like Efficiency.
func (s KillActorStats) ShipEfficiency() float64 {
	total := s.Kills + s.Losses
	if total <= 0 {
		return 0
	}
	return float64(s.Kills) / float64(total) * 100
}

// HasArchive reports whether any of these totals came from rolled-up kills
// whose individual mails no longer exist.
func (s KillActorStats) HasArchive() bool { return !s.Archived.Empty() }

// Efficiency is the share of ISK on this actor's mails that belonged to somebody else.
// An actor with no kills and no losses returns 0 rather than 100%.
func (s KillActorStats) Efficiency() float64 {
	total := s.ISKDestroyed + s.ISKLost
	if total <= 0 {
		return 0
	}
	return s.ISKDestroyed / total * 100
}

// HasRecord reports whether anything is known about this actor: "no kills yet" versus
// "no such pilot".
func (s KillActorStats) HasRecord() bool { return s.Kills > 0 || s.Losses > 0 }

// ActorKillStats loads the headline numbers for one pilot or corporation.
// since is optional; a zero time means the whole record.
func ActorKillStats(ctx context.Context, db *sql.DB, actor KillActor, since time.Time) (*KillActorStats, error) {
	out := &KillActorStats{}
	if !actor.valid() {
		return out, nil
	}

	// Kills: collapse to one row per killmail first, so a gang from one corp sums a kill's
	// value once. MAX() takes the per-kill value; for finalBlow it is an OR.
	atkPred, atkArgs := actor.attackerPred()
	killWhere, killArgs := andSince(atkPred, atkArgs, "k.killTime >= ?", since)
	err := db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(t.totalValue), 0),
		       COALESCE(SUM(t.solo), 0), COALESCE(SUM(t.fb), 0),
		       COALESCE(MAX(t.killTime), 0), COALESCE(SUM(t.atk), 0)
		  FROM (SELECT k.killID,
		               MAX(k.totalValue)          AS totalValue,
		               MAX(k.killTime)            AS killTime,
		               MAX(k.attackerCount = 1)   AS solo,
		               MAX(k.attackerCount)       AS atk,
		               MAX(a.finalBlow)           AS fb
		          FROM killmail_attackers a
		          JOIN killmails k ON k.killID = a.killID
		         WHERE `+killWhere+`
		         GROUP BY k.killID) t`, killArgs...).
		Scan(&out.Kills, &out.ISKDestroyed, &out.SoloKills, &out.FinalBlows, &scanUnix{&out.LastKill},
			&out.GangPilots)
	if err != nil {
		return nil, fmt.Errorf("ActorKillStats kills: %w", err)
	}
	out.GangKills = out.Kills

	// Losses: killmails is already one row per loss.
	vicPred, vicArgs := actor.victimPred()
	lossWhere, lossArgs := andSince(vicPred, vicArgs, "k.killTime >= ?", since)
	err = db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(k.totalValue), 0), COALESCE(MAX(k.killTime), 0)
		  FROM killmails k WHERE `+lossWhere, lossArgs...).
		Scan(&out.Losses, &out.ISKLost, &scanUnix{&out.LastLoss})
	if err != nil {
		return nil, fmt.Errorf("ActorKillStats losses: %w", err)
	}

	// Fold in the rollup for lifetime queries only: it has no time axis. The halves are disjoint
	// because the sweep credits the rollup and deletes the mails in one transaction.
	if since.IsZero() {
		roll, err := GetKillRollup(ctx, db, actor)
		if err != nil {
			// A profile without its archive is a smaller answer; the live half is already loaded.
			return out, nil
		}
		if !roll.Empty() {
			out.Archived = roll
			out.Kills += roll.Kills
			out.Losses += roll.Losses
			out.ISKDestroyed += roll.ISKDestroyed
			out.ISKLost += roll.ISKLost
			out.SoloKills += roll.SoloKills
			out.FinalBlows += roll.FinalBlows
			if roll.LastKill.After(out.LastKill) {
				out.LastKill = roll.LastKill
			}
			if roll.LastLoss.After(out.LastLoss) {
				out.LastLoss = roll.LastLoss
			}
		}
	}
	return out, nil
}

// KillTypeCount is one line of a "ships flown" / "ships killed" / "weapons
// used" breakdown.
type KillTypeCount struct {
	TypeID   int64
	TypeName string
	Count    int64
	ISK      float64
}

// KillTypeMode selects which breakdown ActorTypeBreakdown returns.
type KillTypeMode int

const (
	// TypeShipsFlown groups the actor's OWN hull across the kills they were on.
	TypeShipsFlown KillTypeMode = iota
	// TypeShipsKilled groups the victims' hulls across those same kills.
	TypeShipsKilled
	// TypeShipsLost groups the actor's own hulls across their losses.
	TypeShipsLost
	// TypeWeaponsUsed groups the module/charge/hull credited with their hits.
	TypeWeaponsUsed
)

// ActorTypeBreakdown returns the top type rows for one breakdown, commonest first.
// TypeShipsLost reads killmails alone; the other three read attacker rows. All four collapse
// per killmail before counting.
func ActorTypeBreakdown(ctx context.Context, db *sql.DB, actor KillActor,
	mode KillTypeMode, since time.Time, limit int) ([]KillTypeCount, error) {

	if !actor.valid() {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}

	var query string
	var args []any
	if mode == TypeShipsLost {
		pred, pargs := actor.victimPred()
		where, wargs := andSince(pred, pargs, "k.killTime >= ?", since)
		query = `
			SELECT k.shipTypeID, MAX(k.shipTypeName), COUNT(*), COALESCE(SUM(k.totalValue), 0)
			  FROM killmails k
			 WHERE ` + where + ` AND k.shipTypeID <> 0
			 GROUP BY k.shipTypeID
			 ORDER BY COUNT(*) DESC, SUM(k.totalValue) DESC
			 LIMIT ?`
		args = append(wargs, limit)
	} else {
		// Which column identifies the type of a row, and which names it.
		idCol, nameCol := "a.shipTypeID", "a.shipTypeName"
		switch mode {
		case TypeShipsKilled:
			idCol, nameCol = "k.shipTypeID", "k.shipTypeName"
		case TypeWeaponsUsed:
			idCol, nameCol = "a.weaponTypeID", "a.weaponName"
		}
		pred, pargs := actor.attackerPred()
		where, wargs := andSince(pred, pargs, "k.killTime >= ?", since)
		// The inner GROUP BY is (killID, type): one credit per kill per type.
		query = `
			SELECT t.typeID, MAX(t.typeName), COUNT(*), COALESCE(SUM(t.totalValue), 0)
			  FROM (SELECT ` + idCol + ` AS typeID, MAX(` + nameCol + `) AS typeName,
			               k.killID, MAX(k.totalValue) AS totalValue
			          FROM killmail_attackers a
			          JOIN killmails k ON k.killID = a.killID
			         WHERE ` + where + ` AND ` + idCol + ` <> 0
			         GROUP BY k.killID, ` + idCol + `) t
			 GROUP BY t.typeID
			 ORDER BY COUNT(*) DESC, SUM(t.totalValue) DESC
			 LIMIT ?`
		args = append(wargs, limit)
	}

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("ActorTypeBreakdown(%d): %w", mode, err)
	}
	defer rows.Close()

	var out []KillTypeCount
	for rows.Next() {
		var c KillTypeCount
		var name sql.NullString
		if err := rows.Scan(&c.TypeID, &name, &c.Count, &c.ISK); err != nil {
			return nil, fmt.Errorf("ActorTypeBreakdown scan: %w", err)
		}
		c.TypeName = name.String
		out = append(out, c)
	}
	return out, rows.Err()
}

// KillOpponent is one line of a "most killed" / "killed most by" table. CharID is 0 for an
// NPC, so rows group by (charID, name): grouping by the ID alone folds every NPC into one row.
// See docs/db-5-notes.md#killboard-profile-aggregates.
type KillOpponent struct {
	CharID   int64
	Name     string
	CorpID   int64
	CorpName string
	ShipID   int64 // a representative hull, for the row's icon
	IsNPC    bool
	Count    int64
	ISK      float64
}

// ActorTopVictims ranks who this actor has killed most.
func ActorTopVictims(ctx context.Context, db *sql.DB, actor KillActor, since time.Time, limit int) ([]KillOpponent, error) {
	if !actor.valid() {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}
	pred, pargs := actor.attackerPred()
	where, wargs := andSince(pred, pargs, "k.killTime >= ?", since)
	// Collapse to one row per kill first: a four-pilot gang from one corp must
	// not count its victim four times.
	rows, err := db.QueryContext(ctx, `
		SELECT t.victimCharID, MAX(t.victimName), MAX(t.victimCorpID), MAX(t.victimCorpName),
		       MAX(t.shipTypeID), COUNT(*), COALESCE(SUM(t.totalValue), 0)
		  FROM (SELECT k.killID, k.victimCharID,
		               MAX(k.victimName)     AS victimName,
		               MAX(k.victimCorpID)   AS victimCorpID,
		               MAX(k.victimCorpName) AS victimCorpName,
		               MAX(k.shipTypeID)     AS shipTypeID,
		               MAX(k.totalValue)     AS totalValue
		          FROM killmail_attackers a
		          JOIN killmails k ON k.killID = a.killID
		         WHERE `+where+` AND k.victimCharID <> 0
		         GROUP BY k.killID, k.victimCharID) t
		 GROUP BY t.victimCharID
		 ORDER BY COUNT(*) DESC, SUM(t.totalValue) DESC
		 LIMIT ?`, append(wargs, limit)...)
	if err != nil {
		return nil, fmt.Errorf("ActorTopVictims: %w", err)
	}
	defer rows.Close()

	var out []KillOpponent
	for rows.Next() {
		var o KillOpponent
		var name, corp sql.NullString
		if err := rows.Scan(&o.CharID, &name, &o.CorpID, &corp, &o.ShipID, &o.Count, &o.ISK); err != nil {
			return nil, fmt.Errorf("ActorTopVictims scan: %w", err)
		}
		o.Name, o.CorpName = name.String, corp.String
		out = append(out, o)
	}
	return out, rows.Err()
}

// ActorTopNemeses ranks who has killed this actor most, read off the attacker rows of their
// losses. NPCs come back with CharID 0 and IsNPC set, grouped by rat type.
func ActorTopNemeses(ctx context.Context, db *sql.DB, actor KillActor, since time.Time, limit int) ([]KillOpponent, error) {
	if !actor.valid() {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}
	pred, pargs := actor.victimPred()
	where, wargs := andSince(pred, pargs, "k.killTime >= ?", since)
	// Grouped by (charID, name): players separate on the ID, NPCs on the rat's name.
	rows, err := db.QueryContext(ctx, `
		SELECT t.charID, t.name, MAX(t.corpID), MAX(t.corpName), MAX(t.shipTypeID),
		       MAX(t.isNPC), COUNT(*), COALESCE(SUM(t.totalValue), 0)
		  FROM (SELECT a.charID, a.name,
		               MAX(a.corpID)      AS corpID,
		               MAX(a.corpName)    AS corpName,
		               MAX(a.shipTypeID)  AS shipTypeID,
		               MAX(a.isNPC)       AS isNPC,
		               k.killID, MAX(k.totalValue) AS totalValue
		          FROM killmails k
		          JOIN killmail_attackers a ON a.killID = k.killID
		         WHERE `+where+`
		         GROUP BY k.killID, a.charID, a.name) t
		 GROUP BY t.charID, t.name
		 ORDER BY COUNT(*) DESC, SUM(t.totalValue) DESC
		 LIMIT ?`, append(wargs, limit)...)
	if err != nil {
		return nil, fmt.Errorf("ActorTopNemeses: %w", err)
	}
	defer rows.Close()

	var out []KillOpponent
	for rows.Next() {
		var o KillOpponent
		var name, corp sql.NullString
		var npc int
		if err := rows.Scan(&o.CharID, &name, &o.CorpID, &corp, &o.ShipID, &npc, &o.Count, &o.ISK); err != nil {
			return nil, fmt.Errorf("ActorTopNemeses scan: %w", err)
		}
		o.Name, o.CorpName, o.IsNPC = name.String, corp.String, npc != 0
		out = append(out, o)
	}
	return out, rows.Err()
}

// KillSystemCount is one line of the "where they score" table.
type KillSystemCount struct {
	SystemID   int64
	Name       string
	RegionName string
	Security   float64
	Count      int64
	ISK        float64
}

// ActorTopSystems ranks the solar systems where this actor has scored kills. Attacker side
// only, collapsed to one row per killmail.
func ActorTopSystems(ctx context.Context, db *sql.DB, actor KillActor, since time.Time, limit int) ([]KillSystemCount, error) {
	if !actor.valid() {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}
	pred, pargs := actor.attackerPred()
	where, wargs := andSince(pred, pargs, "k.killTime >= ?", since)
	rows, err := db.QueryContext(ctx, `
		SELECT t.solarSystemID, MAX(t.systemName), MAX(t.security), MAX(mr.regionName),
		       COUNT(*), COALESCE(SUM(t.totalValue), 0)
		  FROM (SELECT k.killID, k.solarSystemID,
		               MAX(k.systemName) AS systemName,
		               MAX(k.security)   AS security,
		               MAX(k.regionID)   AS regionID,
		               MAX(k.totalValue) AS totalValue
		          FROM killmail_attackers a
		          JOIN killmails k ON k.killID = a.killID
		         WHERE `+where+` AND k.solarSystemID <> 0
		         GROUP BY k.killID, k.solarSystemID) t
		  LEFT JOIN mapRegions mr ON mr.regionID = t.regionID
		 GROUP BY t.solarSystemID
		 ORDER BY COUNT(*) DESC, SUM(t.totalValue) DESC
		 LIMIT ?`, append(wargs, limit)...)
	if err != nil {
		return nil, fmt.Errorf("ActorTopSystems: %w", err)
	}
	defer rows.Close()

	var out []KillSystemCount
	for rows.Next() {
		var c KillSystemCount
		var name, region sql.NullString
		var sec sql.NullFloat64
		if err := rows.Scan(&c.SystemID, &name, &sec, &region, &c.Count, &c.ISK); err != nil {
			return nil, fmt.Errorf("ActorTopSystems scan: %w", err)
		}
		c.Name, c.RegionName, c.Security = name.String, region.String, sec.Float64
		out = append(out, c)
	}
	return out, rows.Err()
}

// KillLocationCount is one line of the "where in the system" table.
type KillLocationCount struct {
	SystemID   int64
	SystemName string
	Security   float64
	LocationID int64 // 0 = deep space
	Name       string
	GroupID    int64 // mapDenormalize groupID of the celestial; 0 = deep space
	Count      int64
	ISK        float64
}

// ActorTopLocations ranks the sites (gate, belt, station) where this actor has scored kills;
// killlocation.go resolves the site at record time. Attacker side, one row per killmail.
// A site is keyed by (system, celestial) because deep space is locationID 0 in every system.
// Kills with locationName NULL or '' are left out.
func ActorTopLocations(ctx context.Context, db *sql.DB, actor KillActor, since time.Time, limit int) ([]KillLocationCount, error) {
	if !actor.valid() {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}
	pred, pargs := actor.attackerPred()
	where, wargs := andSince(pred, pargs, "k.killTime >= ?", since)
	rows, err := db.QueryContext(ctx, `
		SELECT t.solarSystemID, t.locationID, MAX(t.systemName), MAX(t.security),
		       MAX(t.locationName), MAX(t.locationGroupID),
		       COUNT(*), COALESCE(SUM(t.totalValue), 0)
		  FROM (SELECT k.killID, k.solarSystemID, k.locationID,
		               MAX(k.systemName)      AS systemName,
		               MAX(k.security)        AS security,
		               MAX(k.locationName)    AS locationName,
		               MAX(k.locationGroupID) AS locationGroupID,
		               MAX(k.totalValue)      AS totalValue
		          FROM killmail_attackers a
		          JOIN killmails k ON k.killID = a.killID
		         WHERE `+where+` AND k.solarSystemID <> 0 AND k.locationName <> ''
		         GROUP BY k.killID, k.solarSystemID, k.locationID) t
		 GROUP BY t.solarSystemID, t.locationID
		 ORDER BY COUNT(*) DESC, SUM(t.totalValue) DESC
		 LIMIT ?`, append(wargs, limit)...)
	if err != nil {
		return nil, fmt.Errorf("ActorTopLocations: %w", err)
	}
	defer rows.Close()

	var out []KillLocationCount
	for rows.Next() {
		var c KillLocationCount
		var sys, loc sql.NullString
		var sec sql.NullFloat64
		if err := rows.Scan(&c.SystemID, &c.LocationID, &sys, &sec, &loc, &c.GroupID, &c.Count, &c.ISK); err != nil {
			return nil, fmt.Errorf("ActorTopLocations scan: %w", err)
		}
		c.SystemName, c.Name, c.Security = sys.String, loc.String, sec.Float64
		out = append(out, c)
	}
	return out, rows.Err()
}

// CorpTopPilots ranks a corporation's own members by kills for the corp page. NPC rows are
// excluded: they have no charID to link to.
func CorpTopPilots(ctx context.Context, db *sql.DB, corpID int64, since time.Time, limit int) ([]KillboardLeader, error) {
	if corpID == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}
	where, args := andSince("a.corpID = ? AND a.charID <> 0 AND a.isNPC = 0", []any{corpID},
		"k.killTime >= ?", since)
	rows, err := db.QueryContext(ctx, `
		SELECT a.charID, MAX(a.name), MAX(a.corpName),
		       COUNT(DISTINCT a.killID), COALESCE(SUM(k.totalValue / GREATEST(k.attackerCount, 1)), 0)
		  FROM killmail_attackers a
		  JOIN killmails k ON k.killID = a.killID
		 WHERE `+where+`
		 GROUP BY a.charID
		 ORDER BY COUNT(DISTINCT a.killID) DESC, SUM(k.totalValue) DESC
		 LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, fmt.Errorf("CorpTopPilots: %w", err)
	}
	defer rows.Close()
	return scanLeaders(rows, "CorpTopPilots")
}

// KillDayCount is one bucket of the activity strip.
type KillDayCount struct {
	Day    time.Time
	Kills  int64
	Losses int64
}

// ActorActivity buckets kills and losses by day over the last n days, oldest first. Empty
// days are present as zeroes so the strip keeps its shape.
func ActorActivity(ctx context.Context, db *sql.DB, actor KillActor, days int) ([]KillDayCount, error) {
	if !actor.valid() {
		return nil, nil
	}
	if days <= 0 {
		days = 14
	}
	// Bucket by local midnight.
	start := time.Now().Truncate(24*time.Hour).AddDate(0, 0, -(days - 1))
	buckets := make([]KillDayCount, days)
	index := map[int64]int{}
	for i := range buckets {
		d := start.AddDate(0, 0, i)
		buckets[i].Day = d
		index[d.Unix()] = i
	}
	// One offset for every row, so SQL and Go agree where a day starts when the server is not UTC.
	_, offset := start.Zone()

	tally := func(query string, args []any, into func(i int, n int64)) error {
		rows, err := db.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var bucket, n int64
			if err := rows.Scan(&bucket, &n); err != nil {
				return err
			}
			if i, ok := index[bucket]; ok {
				into(i, n)
			}
		}
		return rows.Err()
	}

	// Local midnight of the kill in unix seconds, so it keys the same map.
	const bucketExpr = `FLOOR((k.killTime + ?) / 86400) * 86400 - ?`

	atkPred, atkArgs := actor.attackerPred()
	args := append([]any{offset, offset}, atkArgs...)
	args = append(args, start.Unix())
	if err := tally(`
		SELECT `+bucketExpr+` AS bucket, COUNT(DISTINCT a.killID)
		  FROM killmail_attackers a
		  JOIN killmails k ON k.killID = a.killID
		 WHERE `+atkPred+` AND k.killTime >= ?
		 GROUP BY bucket`, args,
		func(i int, n int64) { buckets[i].Kills = n }); err != nil {
		return nil, fmt.Errorf("ActorActivity kills: %w", err)
	}

	vicPred, vicArgs := actor.victimPred()
	args = append([]any{offset, offset}, vicArgs...)
	args = append(args, start.Unix())
	if err := tally(`
		SELECT `+bucketExpr+` AS bucket, COUNT(*)
		  FROM killmails k
		 WHERE `+vicPred+` AND k.killTime >= ?
		 GROUP BY bucket`, args,
		func(i int, n int64) { buckets[i].Losses = n }); err != nil {
		return nil, fmt.Errorf("ActorActivity losses: %w", err)
	}
	return buckets, nil
}

// KillProfileIdentity is the public header of a pilot page, rendered for any pilot. It has
// its own query because ESPGetCharacterDetail carries the account name and wallet balance.
type KillProfileIdentity struct {
	CharID       int64
	Name         string
	CorpID       int64
	CorpName     string
	AllianceID   int64
	AllianceName string
	SecStatus    float64
	Exists       bool
}

// GetKillProfileIdentity resolves a pilot's public affiliation. A character that no longer
// exists is not an error: the caller gets Exists false plus what the killmails recorded.
func GetKillProfileIdentity(ctx context.Context, db *sql.DB, charID int64) (*KillProfileIdentity, error) {
	out := &KillProfileIdentity{CharID: charID}
	if charID == 0 {
		return out, nil
	}
	var alliance, corpID sql.NullInt64
	var sec sql.NullFloat64
	var corpName, allianceName sql.NullString
	err := db.QueryRowContext(ctx, `
		SELECT c.characterName, c.corporationID, COALESCE(co.corporationName, ''),
		       co.allianceID, al.allianceName, c.securityRating
		  FROM character_ c
		  LEFT JOIN corporation co ON co.corporationID = c.corporationID
		  LEFT JOIN alliances al ON al.allianceID = co.allianceID
		 WHERE c.characterID = ?`, charID).
		Scan(&out.Name, &corpID, &corpName, &alliance, &allianceName, &sec)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return out, nil
	case err != nil:
		return nil, fmt.Errorf("GetKillProfileIdentity: %w", err)
	}
	out.Exists = true
	out.CorpID, out.SecStatus = corpID.Int64, sec.Float64
	out.CorpName, out.AllianceName = corpName.String, allianceName.String
	out.AllianceID = alliance.Int64
	return out, nil
}

// FillIdentityFromKillboard backfills a header for a deleted pilot from the most recent
// killmail that mentions them, as victim or attacker. Fields already set are left alone.
func FillIdentityFromKillboard(ctx context.Context, db *sql.DB, id *KillProfileIdentity) {
	if id == nil || id.CharID == 0 || id.Name != "" {
		return
	}
	var name, corp, alliance sql.NullString
	var corpID, allianceID sql.NullInt64
	// Victim rows first: they carry the alliance name; attacker rows only if allied then.
	err := db.QueryRowContext(ctx, `
		SELECT victimName, victimCorpID, victimCorpName, victimAllianceID, victimAllianceName
		  FROM killmails WHERE victimCharID = ?
		 ORDER BY killTime DESC LIMIT 1`, id.CharID).
		Scan(&name, &corpID, &corp, &allianceID, &alliance)
	if err != nil {
		err = db.QueryRowContext(ctx, `
			SELECT a.name, a.corpID, a.corpName, a.allianceID, a.allianceName
			  FROM killmail_attackers a
			  JOIN killmails k ON k.killID = a.killID
			 WHERE a.charID = ? ORDER BY k.killTime DESC LIMIT 1`, id.CharID).
			Scan(&name, &corpID, &corp, &allianceID, &alliance)
	}
	if err != nil {
		return
	}
	id.Name, id.CorpName, id.AllianceName = name.String, corp.String, alliance.String
	id.CorpID, id.AllianceID = corpID.Int64, allianceID.Int64
}

// ── shared helpers ───────────────────────────────────────────────────────────

// andSince appends the time bound to a predicate when one was asked for; the predicate and
// its arguments travel together.
func andSince(pred string, args []any, sinceExpr string, since time.Time) (string, []any) {
	if since.IsZero() {
		return pred, args
	}
	var b strings.Builder
	b.WriteString(pred)
	b.WriteString(" AND ")
	b.WriteString(sinceExpr)
	return b.String(), append(append([]any(nil), args...), since.Unix())
}

// scanUnix reads a unix-seconds column into a time.Time, mapping the 0 that COALESCE gives
// an empty aggregate to the zero time rather than 1970.
type scanUnix struct{ dst *time.Time }

func (s *scanUnix) Scan(v any) error {
	var n int64
	switch t := v.(type) {
	case nil:
		n = 0
	case int64:
		n = t
	case float64:
		n = int64(t)
	case []byte:
		fmt.Sscanf(string(t), "%d", &n)
	case string:
		fmt.Sscanf(t, "%d", &n)
	default:
		return fmt.Errorf("scanUnix: unexpected %T", v)
	}
	if n <= 0 {
		*s.dst = time.Time{}
		return nil
	}
	*s.dst = time.Unix(n, 0)
	return nil
}
