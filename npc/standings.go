package npc

import (
	"context"
	"database/sql"
	"log"
	"sync"
)

// StandingsMap is an in-memory cache of faction-to-faction standings from
// npcStandings, plus a corp-to-faction lookup from crpNPCCorporations. Used by
// NPC AI to decide hostility.
type StandingsMap struct {
	mu          sync.RWMutex
	table       map[[2]int64]float64 // [fromFaction, toFaction] -> standing
	corpFaction map[int64]int64      // NPC corpID -> factionID
}

// Get returns the standing between two factions. Returns 0 if not found.
func (s *StandingsMap) Get(from, to int64) float64 {
	s.mu.RLock()
	v := s.table[[2]int64{from, to}]
	s.mu.RUnlock()
	return v
}

// CorpFaction returns the factionID for an NPC corp, or 0 if not an NPC corp.
func (s *StandingsMap) CorpFaction(corpID int64) int64 {
	s.mu.RLock()
	f := s.corpFaction[corpID]
	s.mu.RUnlock()
	return f
}

// IsNPCCorp returns true if the given corpID is in the NPC corporation cache.
func (s *StandingsMap) IsNPCCorp(corpID int64) bool {
	return s.CorpFaction(corpID) != 0
}

// IsHostile reports whether faction from is hostile to faction to: never for
// the same faction, always when either is 0 (no faction), else standing < 0.
func (s *StandingsMap) IsHostile(from, to int64) bool {
	if from == to {
		return false
	}
	if from == 0 || to == 0 {
		return true
	}
	return s.Get(from, to) < 0
}

// LoadStandings reads all rows from npcStandings and crpNPCCorporations into
// an in-memory map.
func LoadStandings(ctx context.Context, db *sql.DB) *StandingsMap {
	sm := &StandingsMap{
		table:       make(map[[2]int64]float64),
		corpFaction: make(map[int64]int64),
	}

	// Faction-to-faction standings.
	rows, err := db.QueryContext(ctx, `SELECT fromID, toID, standing FROM npcStandings`)
	if err != nil {
		log.Printf("[npc] LoadStandings: query failed: %v", err)
		return sm
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var from, to int64
		var standing float64
		if err := rows.Scan(&from, &to, &standing); err != nil {
			log.Printf("[npc] LoadStandings: scan error: %v", err)
			continue
		}
		sm.table[[2]int64{from, to}] = standing
		count++
	}
	log.Printf("[npc] LoadStandings: loaded %d faction standing rows", count)

	// NPC corp to faction mapping.
	corpRows, err := db.QueryContext(ctx, `SELECT corporationID, factionID FROM crpNPCCorporations`)
	if err != nil {
		log.Printf("[npc] LoadStandings: crpNPCCorporations query failed: %v", err)
		return sm
	}
	defer corpRows.Close()

	corpCount := 0
	for corpRows.Next() {
		var corpID, factionID int64
		if err := corpRows.Scan(&corpID, &factionID); err != nil {
			log.Printf("[npc] LoadStandings: crpNPCCorporations scan error: %v", err)
			continue
		}
		sm.corpFaction[corpID] = factionID
		corpCount++
	}
	log.Printf("[npc] LoadStandings: loaded %d NPC corp→faction mappings", corpCount)

	return sm
}
