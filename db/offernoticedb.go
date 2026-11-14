package db

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"
)

// ---- Offer notices ----
//
// The notice sweep has an agent write once, at random, to a character who can already
// afford one of its offers. See docs/db-12-notes.md#offer-notices.

const (
	settingOfferNoticeChance   = "offers.noticeChancePct"
	settingOfferNoticeCooldown = "offers.noticeCooldownDays"
)

// Defaults for the notice sweep. The chance is rolled per character per sweep, not per
// offer, so a character hears about at most one offer at a time.
const (
	DefaultOfferNoticeChancePct   = 15
	DefaultOfferNoticeCooldownDay = 7
)

// EnsureOfferNoticeTable creates the per-character notice ledger, keyed on the pair
// and holding the time so an offer can come round again after the cooldown.
func EnsureOfferNoticeTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS chrOfferNotices (
			charID     INT NOT NULL,
			offerID    INT NOT NULL,
			notifiedAt BIGINT NOT NULL,
			PRIMARY KEY (charID, offerID)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`)
	if err != nil {
		return fmt.Errorf("EnsureOfferNoticeTable: %w", err)
	}
	return nil
}

// GetOfferNoticeChancePct returns the per-character per-sweep chance, 0-100; 0
// disables the sweep.
func GetOfferNoticeChancePct(ctx context.Context, db *sql.DB) int {
	return settingInt(ctx, db, settingOfferNoticeChance, DefaultOfferNoticeChancePct, 0, 100)
}

// SetOfferNoticeChancePct persists the sweep chance.
func SetOfferNoticeChancePct(ctx context.Context, db *sql.DB, pct int) error {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return SetServerSetting(ctx, db, settingOfferNoticeChance, strconv.Itoa(pct))
}

// GetOfferNoticeCooldownDays returns how long before the same offer may be
// mailed to the same character again.
func GetOfferNoticeCooldownDays(ctx context.Context, db *sql.DB) int {
	return settingInt(ctx, db, settingOfferNoticeCooldown, DefaultOfferNoticeCooldownDay, 1, 3650)
}

// SetOfferNoticeCooldownDays persists the re-notify cooldown.
func SetOfferNoticeCooldownDays(ctx context.Context, db *sql.DB, days int) error {
	if days < 1 {
		days = 1
	}
	return SetServerSetting(ctx, db, settingOfferNoticeCooldown, strconv.Itoa(days))
}

// settingInt reads an integer server setting, clamped, falling back to def.
func settingInt(ctx context.Context, db *sql.DB, name string, def, min, max int) int {
	v, ok, err := GetServerSetting(ctx, db, name)
	if err != nil || !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	if n < min {
		return min
	}
	if n > max {
		return max
	}
	return n
}

// OfferNoticeCandidate is one (character, agent, offer) the sweep may mail about: the
// character holds enough LP with the agent and enough ISK.
type OfferNoticeCandidate struct {
	CharID      int64
	AgentID     int64
	AgentName   string
	StationName string
	SystemName  string
	OfferID     int64
	OfferName   string
	LPCost      int
	ISKCost     int64
	MinStanding float64
}

// FindOfferNoticeCandidates returns every affordable, not-recently-mailed (character,
// agent, offer) triple. The availability clauses match GetOffersForAgent. The standing
// gate is left to the caller to keep the common minStanding 0 path join-free.
func FindOfferNoticeCandidates(ctx context.Context, db *sql.DB, cooldown time.Duration) ([]OfferNoticeCandidate, error) {
	since := time.Now().Add(-cooldown).Unix()
	rows, err := db.QueryContext(ctx, `
		SELECT lp.charID, lp.agentID, COALESCE(an.itemName,''),
		       COALESCE(sn.itemName,''), COALESCE(m.itemName,''),
		       o.offerID, o.offerName, o.lpCost, o.iskCost, o.minStanding
		FROM agentLoyaltyPoints lp
		JOIN agtAgents a          ON a.agentID = lp.agentID
		JOIN character_ ch        ON ch.characterID = lp.charID AND ch.accountID > 0
		LEFT JOIN crpNPCCorporations c ON c.corporationID = a.corporationID
		LEFT JOIN staStations s   ON s.stationID = a.stationID
		LEFT JOIN mapDenormalize m ON m.itemID = s.solarSystemID
		LEFT JOIN eveNames an     ON an.itemID = lp.agentID
		LEFT JOIN eveNames sn     ON sn.itemID = a.stationID
		JOIN agtOffers o
		  ON o.enabled = 1
		 AND (o.corporationID IS NULL OR o.corporationID = a.corporationID)
		 AND (o.allowedFactions IS NULL OR o.allowedFactions = ''
		      OR FIND_IN_SET(COALESCE(c.factionID,0), o.allowedFactions) > 0)
		 AND (o.allowedDivisions IS NULL OR o.allowedDivisions = ''
		      OR FIND_IN_SET(COALESCE(a.divisionID,0), o.allowedDivisions) > 0)
		WHERE lp.points >= o.lpCost
		  AND ch.balance >= o.iskCost
		  AND NOT EXISTS (SELECT 1 FROM chrOfferNotices n
		                   WHERE n.charID = lp.charID AND n.offerID = o.offerID
		                     AND n.notifiedAt > ?)`, since)
	if err != nil {
		return nil, fmt.Errorf("FindOfferNoticeCandidates: %w", err)
	}
	defer rows.Close()
	var out []OfferNoticeCandidate
	for rows.Next() {
		var c OfferNoticeCandidate
		if err := rows.Scan(&c.CharID, &c.AgentID, &c.AgentName, &c.StationName, &c.SystemName,
			&c.OfferID, &c.OfferName, &c.LPCost, &c.ISKCost, &c.MinStanding); err != nil {
			return nil, fmt.Errorf("FindOfferNoticeCandidates scan: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// RecordOfferNotice stamps a (character, offer) pair as mailed.
func RecordOfferNotice(ctx context.Context, db *sql.DB, charID, offerID int64) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO chrOfferNotices (charID, offerID, notifiedAt) VALUES (?, ?, ?)
		ON DUPLICATE KEY UPDATE notifiedAt = VALUES(notifiedAt)`,
		charID, offerID, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("RecordOfferNotice: %w", err)
	}
	return nil
}
