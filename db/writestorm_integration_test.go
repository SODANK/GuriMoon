package db

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/go-sql-driver/mysql"
)

// Integration checks for the write-volume fixes. They run only against a scratch
// database named in GURIMOON_TEST_DSN (e.g. "user:pass@tcp(127.0.0.1:3306)/gm_test");
// they create and drop their own tables.
func testDB(t *testing.T) *sql.DB {
	dsn := os.Getenv("GURIMOON_TEST_DSN")
	if dsn == "" {
		t.Skip("GURIMOON_TEST_DSN not set")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func comAlterTable(t *testing.T, db *sql.DB) int64 {
	var name string
	var n int64
	if err := db.QueryRow(`SHOW SESSION STATUS LIKE 'Com_alter_table'`).Scan(&name, &n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestIntegrationBugReportsBatchAndPrune(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	db.Exec(`DROP TABLE IF EXISTS bug_reports`)
	if err := EnsureBugReportsTable(ctx, db); err != nil {
		t.Fatal(err)
	}
	hits := []BugReportHit{
		{Signature: "aaaaaaaaaaaaaaaa", Category: "error", Sample: "first", Count: 2, When: 100},
		{Signature: "bbbbbbbbbbbbbbbb", Category: "error", Sample: "b", Count: 1, When: 50},
	}
	if err := UpsertBugReports(ctx, db, hits); err != nil {
		t.Fatal(err)
	}
	hits[0].Sample, hits[0].Count, hits[0].When = "second", 3, 200
	if err := UpsertBugReports(ctx, db, hits[:1]); err != nil {
		t.Fatal(err)
	}
	var sample string
	var count, last int64
	db.QueryRow(`SELECT sampleMessage, hitCount, lastSeen FROM bug_reports WHERE signature='aaaaaaaaaaaaaaaa'`).Scan(&sample, &count, &last)
	if sample != "first" || count != 5 || last != 200 {
		t.Fatalf("open row = %q/%d/%d, want first/5/200 (sample kept)", sample, count, last)
	}
	// A fixed row reopens and takes the new sample.
	db.Exec(`UPDATE bug_reports SET status='fixed' WHERE signature='aaaaaaaaaaaaaaaa'`)
	hits[0].Sample = "regressed"
	UpsertBugReports(ctx, db, hits[:1])
	var status string
	db.QueryRow(`SELECT sampleMessage, status FROM bug_reports WHERE signature='aaaaaaaaaaaaaaaa'`).Scan(&sample, &status)
	if sample != "regressed" || status != "open" {
		t.Fatalf("reopened row = %q/%q", sample, status)
	}
	// Keep 1: the older row goes.
	n, err := PruneBugReports(ctx, db, 1)
	if err != nil || n != 1 {
		t.Fatalf("prune = %d, %v; want 1", n, err)
	}
	if n, _ := PruneBugReports(ctx, db, 10); n != 0 {
		t.Fatalf("prune under keep deleted %d", n)
	}
	db.Exec(`DROP TABLE bug_reports`)
}

func TestIntegrationSchemaProbesSkipAlters(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	db.Exec(`DROP TABLE IF EXISTS agtServiceTexts`)
	db.Exec(`CREATE TABLE agtServiceTexts (divisionID INT NOT NULL DEFAULT 0, textKey VARCHAR(48) NOT NULL, body TEXT NOT NULL, PRIMARY KEY (divisionID, textKey))`)
	pk, err := primaryKeyColumns(ctx, db, "agtServiceTexts")
	if err != nil || len(pk) != 2 || pk[0] != "divisionid" || pk[1] != "textkey" {
		t.Fatalf("pk = %v, %v", pk, err)
	}
	db.Exec(`DROP TABLE IF EXISTS corp_probe_t`)
	db.Exec(`CREATE TABLE corp_probe_t (division1 VARCHAR(11) NULL, division2 VARCHAR(50) NULL, mode INT UNSIGNED NOT NULL DEFAULT 0, sg DOUBLE DEFAULT 0)`)
	is50 := func(ct string) bool { return ct == "varchar(50)" }
	if !columnNeedsModify(ctx, db, "corp_probe_t", "division1", true, is50) {
		t.Error("varchar(11) must need the MODIFY")
	}
	if columnNeedsModify(ctx, db, "corp_probe_t", "division2", true, is50) {
		t.Error("varchar(50) NULL must not need the MODIFY")
	}
	if columnNeedsModify(ctx, db, "corp_probe_t", "nope", true, is50) {
		t.Error("a missing column must not need a MODIFY")
	}
	if !missionModifyDoneCol(ctx, db, "corp_probe_t", "sg") {
		t.Error("DOUBLE column must count as done")
	}
	db.Exec(`DROP TABLE corp_probe_t`)
	db.Exec(`DROP TABLE agtServiceTexts`)
}

// missionModifyDoneCol is the DOUBLE probe missionModifyDone runs, on any table.
func missionModifyDoneCol(ctx context.Context, db *sql.DB, table, col string) bool {
	return !columnNeedsModify(ctx, db, table, col, true, func(ct string) bool { return ct == "double" })
}

func TestIntegrationBookmarkDDLRunsOnce(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	db.SetMaxOpenConns(1) // session status counters are per connection
	if err := ensureBookmarksTable(ctx, db); err != nil {
		t.Fatal(err)
	}
	before := comAlterTable(t, db)
	for i := 0; i < 5; i++ {
		if err := ensureBookmarksTable(ctx, db); err != nil {
			t.Fatal(err)
		}
	}
	if d := comAlterTable(t, db) - before; d != 0 {
		t.Fatalf("ensureBookmarksTable issued %d ALTERs after the first call", d)
	}
}

func TestIntegrationDefaultTitlesOnce(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	db.Exec(`DROP TABLE IF EXISTS crpTitles`)
	ensureCorpTitlesTableGuard = ensureGuard{} // the table was dropped under the guard
	defaultTitlesSeeded.Delete(int64(98000001))
	if err := EnsureDefaultTitles(ctx, db, 98000001); err != nil {
		t.Fatal(err)
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM crpTitles WHERE corporationID = 98000001`).Scan(&n)
	if n != 8 {
		t.Fatalf("seeded %d titles, want 8", n)
	}
	db.Exec(`DROP TABLE crpTitles`)
	ensureCorpTitlesTableGuard = ensureGuard{}
}
