package destiny

import "testing"

// A hit from a ball records that ball, so a reader can tell apart two attackers flying the same type.
func TestRecordKillHitStoresAttackerEntity(t *testing.T) {
	victim := &Entity{ID: 1, IsPlayer: true}
	ratA := &Entity{ID: 100, Slim: SlimItem{TypeID: 555}}
	ratB := &Entity{ID: 101, Slim: SlimItem{TypeID: 555}}

	victim.RecordKillHit(ratA, 0, 10, false)
	victim.RecordKillHit(ratB, 0, 20, false)

	got := map[uint32]float64{}
	for _, h := range victim.KillHitsSnapshot() {
		got[h.EntityID] = h.Damage
	}
	if got[100] != 10 || got[101] != 20 || len(got) != 2 {
		t.Fatalf("hits by entity = %v, want {100:10 101:20}", got)
	}
}

// A raw hit that carries no entity (the drone and mine paths) keeps the one already on record.
func TestRecordKillHitRawInheritsEntity(t *testing.T) {
	victim := &Entity{ID: 1, IsPlayer: true}
	victim.RecordKillHitRaw(7, KillHit{CharID: 9, Damage: 5, EntityID: 42})
	victim.RecordKillHitRaw(7, KillHit{CharID: 9, Damage: 5})

	hits := victim.KillHitsSnapshot()
	if len(hits) != 1 {
		t.Fatalf("got %d hits, want 1", len(hits))
	}
	if hits[0].EntityID != 42 || hits[0].Damage != 10 {
		t.Fatalf("hit = %+v, want EntityID 42 and Damage 10", hits[0])
	}
}
