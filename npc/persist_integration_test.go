package npc

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/go-sql-driver/mysql"
)

// TestIntegrationFlushState runs against the scratch database in GURIMOON_TEST_DSN.
func TestIntegrationFlushState(t *testing.T) {
	dsn := os.Getenv("GURIMOON_TEST_DSN")
	if dsn == "" {
		t.Skip("GURIMOON_TEST_DSN not set")
	}
	sqldb, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer sqldb.Close()
	sqldb.SetMaxOpenConns(1) // session counters are per connection
	for _, q := range []string{
		`DROP TABLE IF EXISTS npcLiveEntities`, `DROP TABLE IF EXISTS npcDungeonEntities`,
		`CREATE TABLE npcLiveEntities (entityID INT UNSIGNED PRIMARY KEY, posX DOUBLE NOT NULL DEFAULT 0, posY DOUBLE NOT NULL DEFAULT 0, posZ DOUBLE NOT NULL DEFAULT 0, shieldFrac DOUBLE NOT NULL DEFAULT 1, armorFrac DOUBLE NOT NULL DEFAULT 1, structureFrac DOUBLE NOT NULL DEFAULT 1)`,
		`CREATE TABLE npcDungeonEntities (entityID INT UNSIGNED PRIMARY KEY, shieldFrac DOUBLE NOT NULL DEFAULT 1, armorFrac DOUBLE NOT NULL DEFAULT 1, structureFrac DOUBLE NOT NULL DEFAULT 1)`,
		`INSERT INTO npcLiveEntities (entityID) VALUES (1)`,
		`INSERT INTO npcDungeonEntities (entityID) VALUES (2)`,
	} {
		if _, err := sqldb.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	defer sqldb.Exec(`DROP TABLE IF EXISTS npcLiveEntities`)
	defer sqldb.Exec(`DROP TABLE IF EXISTS npcDungeonEntities`)

	m := &NPCManager{npcs: map[uint32]*NPCRecord{}, sqldb: sqldb}
	m.npcs[1] = testRec(1, 25, 0)
	m.npcs[2] = testRec(2, 50, 9)
	for i := 0; i < 100; i++ {
		m.SyncHPToDB(1)
		m.SyncHPToDB(2)
	}
	m.markDirty(1, dirtyPos)

	status := func(v string) int64 {
		var name string
		var n int64
		sqldb.QueryRow(`SHOW SESSION STATUS LIKE '` + v + `'`).Scan(&name, &n)
		return n
	}
	updBefore, comBefore := status("Com_update"), status("Com_commit")
	n, err := m.FlushState(context.Background())
	if err != nil || n != 2 {
		t.Fatalf("FlushState = %d, %v; want 2", n, err)
	}
	// 200 hits and a move became two UPDATEs in one transaction (it was 201 commits).
	if d := status("Com_update") - updBefore; d != 2 {
		t.Errorf("flush ran %d UPDATEs, want 2", d)
	}
	if d := status("Com_commit") - comBefore; d != 1 {
		t.Errorf("flush made %d commits, want 1", d)
	}
	var s1, s2 float64
	sqldb.QueryRow(`SELECT shieldFrac FROM npcLiveEntities WHERE entityID=1`).Scan(&s1)
	sqldb.QueryRow(`SELECT shieldFrac FROM npcDungeonEntities WHERE entityID=2`).Scan(&s2)
	if s1 != 0.25 || s2 != 0.5 {
		t.Fatalf("shield = %v / %v, want 0.25 / 0.5", s1, s2)
	}
	if n, _ := m.FlushState(context.Background()); n != 0 {
		t.Fatalf("second flush wrote %d rows, want 0", n)
	}
}
