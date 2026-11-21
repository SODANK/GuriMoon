package npc

import (
	"database/sql"
	"testing"

	"gurimoon/destiny"
)

func testRec(id uint32, curShield float64, dunObj int64) *NPCRecord {
	e := &destiny.Entity{ID: id}
	e.HP = destiny.HPState{MaxShield: 100, CurShield: curShield, MaxArmor: 100, CurArmor: 100, MaxStructure: 100, CurStructure: 100}
	return &NPCRecord{Entity: e, DunObjectID: dunObj}
}

func TestDirtyCoalescesAndSkipsGone(t *testing.T) {
	m := &NPCManager{npcs: map[uint32]*NPCRecord{}, sqldb: new(sql.DB)}
	m.npcs[1] = testRec(1, 40, 0)
	m.npcs[2] = testRec(2, 100, 7)
	dead := testRec(3, 100, 0)
	dead.Dead = true
	m.npcs[3] = dead

	for i := 0; i < 50; i++ { // fifty hits, one row
		m.SyncHPToDB(1)
	}
	m.markDirty(1, dirtyPos)
	m.SyncHPToDB(2)
	m.SyncHPToDB(3) // dead: skipped
	m.SyncHPToDB(4) // never existed: skipped
	m.SyncHPToDB(5)
	m.npcs[5] = testRec(5, 100, 0)
	m.Remove(5) // removed before the flush: forgotten

	if got := m.DirtyCount(); got != 4 {
		t.Fatalf("DirtyCount = %d, want 4 (1,2,3,4)", got)
	}
	rows := m.takeDirty()
	if len(rows) != 2 {
		t.Fatalf("takeDirty rows = %+v, want NPCs 1 and 2", rows)
	}
	byID := map[uint32]npcStateRow{}
	for _, r := range rows {
		byID[r.id] = r
	}
	if r := byID[1]; r.bits != dirtyHP|dirtyPos || r.dungeon || r.shield != 0.4 {
		t.Errorf("row 1 = %+v, want HP+pos, belt, shield 0.4", r)
	}
	if r := byID[2]; r.bits != dirtyHP || !r.dungeon {
		t.Errorf("row 2 = %+v, want HP only, dungeon", r)
	}
	if m.DirtyCount() != 0 {
		t.Error("takeDirty must clear the set")
	}
}

func TestMarkDirtyNoDB(t *testing.T) {
	m := &NPCManager{npcs: map[uint32]*NPCRecord{}}
	m.SyncHPToDB(1)
	if m.DirtyCount() != 0 {
		t.Error("without a database nothing is queued")
	}
}
