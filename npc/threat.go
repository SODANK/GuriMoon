package npc

// threatTable tracks cumulative damage from attackers. Not concurrency-safe
// on its own; protected by NPCRecord.mu.
type threatTable struct {
	entries map[uint32]float64 // entityID -> cumulative damage
}

func newThreatTable() threatTable {
	return threatTable{entries: make(map[uint32]float64)}
}

// Record adds damage from an attacker.
func (t *threatTable) Record(attackerID uint32, damage float64) {
	t.entries[attackerID] += damage
}

// Top returns the entity with the highest cumulative damage. Returns 0 if empty.
func (t *threatTable) Top() uint32 {
	var topID uint32
	var topDmg float64
	for id, dmg := range t.entries {
		if dmg > topDmg {
			topDmg = dmg
			topID = id
		}
	}
	return topID
}

// Has reports whether the entity already has a threat entry.
func (t *threatTable) Has(entityID uint32) bool {
	_, ok := t.entries[entityID]
	return ok
}

// Clear removes an entity from the threat table (e.g. when they leave or die).
func (t *threatTable) Clear(entityID uint32) {
	delete(t.entries, entityID)
}

// Entries returns a snapshot of the threat table as ThreatEntry slice.
func (t *threatTable) Entries() []ThreatEntry {
	out := make([]ThreatEntry, 0, len(t.entries))
	for id, dmg := range t.entries {
		out = append(out, ThreatEntry{EntityID: id, TotalDamage: dmg})
	}
	return out
}
