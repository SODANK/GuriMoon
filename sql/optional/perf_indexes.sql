-- Performance indexes for high-population servers (200+ concurrent pilots/bots).
--
-- THE SERVER APPLIES THESE ITSELF NOW. db/perfindex.go is the executable form
-- and EnsurePerfIndexes runs at boot, before the listener opens, because a
-- deploy that forgot to load this file failed invisibly: the server worked, it
-- just walked every row. This file stays as the readable record — the comments
-- are the measurement, and they are copied into perfindex.go verbatim. Loading
-- it by hand is still safe and still a no-op if the boot path has run.
--
-- Every index here was added because EXPLAIN showed type=ALL — a full table
-- scan — on a query that runs on a per-action hot path. The server is not I/O
-- bound (buffer pool hit rate measured at 99.99997%, the whole database fits in
-- RAM); it is bound by the number of rows those scans walk. Handler_read_rnd_next
-- stood at 353 million.
--
-- Secondary index creation on InnoDB is online: ALGORITHM=INPLACE, LOCK=NONE is
-- stated explicitly so the statement FAILS rather than silently falling back to
-- a table copy that would block the live server.
--
-- Idempotent: re-running is a no-op. Safe to run against a running server.

-- ---------------------------------------------------------------------------
-- market_orders (132,988 rows)
-- ---------------------------------------------------------------------------
-- PlaceBuyOrder/PlaceSellOrder count a character's live orders before every
-- placement (db/marketorders.go: "WHERE charID = ? AND orderState = 1"), and
-- nothing indexed charID at all — so each placement scanned the whole table.
-- With a roster of trader bots this was the single largest source of scanned
-- rows on the server.
ALTER TABLE market_orders
  ADD INDEX idx_mo_char_state (charID, orderState),
  ALGORITHM=INPLACE, LOCK=NONE;

-- The order-matching query filters region + type + bid + orderState + volRemaining
-- together (db/marketorders.go GetOrders/matching, db/marketdb.go:246). With only
-- the two single-column indexes the optimiser resorted to an index_merge
-- intersect of typeID and regionID — two B-tree descents and a row-ID
-- intersection per lookup. Leading with typeID (cardinality ~2000) rather than
-- regionID (cardinality 34) makes the first column selective.
ALTER TABLE market_orders
  ADD INDEX idx_mo_type_region_bid (typeID, regionID, bid, orderState, volRemaining),
  ALGORITHM=INPLACE, LOCK=NONE;

-- regionID_2 and typeID_2 are byte-for-byte duplicates of regionID and typeID.
-- A duplicate index is never chosen by the optimiser but is still maintained on
-- every INSERT, UPDATE and DELETE — pure write amplification on the hottest
-- write table in the schema. No foreign key depends on either.
ALTER TABLE market_orders DROP INDEX regionID_2;
ALTER TABLE market_orders DROP INDEX typeID_2;

-- ---------------------------------------------------------------------------
-- market_transactions (363,179 rows)
-- ---------------------------------------------------------------------------
-- The wallet's Transactions tab reads "WHERE clientID = ? ORDER BY
-- transactionDateTime DESC LIMIT 500" (db/marketdb.go:927) against a table whose
-- only index was PRIMARY(transactionID) plus a regionID index. Every wallet open
-- scanned 363k rows and then filesorted them. Carrying transactionDateTime as
-- the second column lets the index satisfy the ORDER BY as well as the lookup.
ALTER TABLE market_transactions
  ADD INDEX idx_mt_client_date (clientID, transactionDateTime),
  ALGORITHM=INPLACE, LOCK=NONE;

-- Price history aggregates over region + type + transactionType
-- (db/marketdb.go:396). regionID alone has cardinality 116 across 363k rows.
ALTER TABLE market_transactions
  ADD INDEX idx_mt_region_type (regionID, typeID, transactionType),
  ALGORITHM=INPLACE, LOCK=NONE;

-- ---------------------------------------------------------------------------
-- market_journal (127,384 rows)
-- ---------------------------------------------------------------------------
-- The table shipped with PRIMARY(refID) and nothing else, so every wallet
-- journal read and every character purge scanned it whole. ownerID1/ownerID2 are
-- the counterparty columns the journal is also searched by.
ALTER TABLE market_journal
  ADD INDEX idx_mj_char_date (characterID, transDate),
  ALGORITHM=INPLACE, LOCK=NONE;
ALTER TABLE market_journal
  ADD INDEX idx_mj_owner1 (ownerID1),
  ALGORITHM=INPLACE, LOCK=NONE;
ALTER TABLE market_journal
  ADD INDEX idx_mj_owner2 (ownerID2),
  ALGORITHM=INPLACE, LOCK=NONE;

-- ---------------------------------------------------------------------------
-- character_ (13,537 rows)
-- ---------------------------------------------------------------------------
-- The table carries thirteen foreign-key indexes on appearance columns nothing
-- ever queries by (accessoryID, beardID, costumeID, eyebrowsID, ...) and none at
-- all on the three location/affiliation columns that every corp roster, station
-- guest list and system population count filters on. The row count is small, but
-- these run per docking, per undock and on every ESP page load.
ALTER TABLE character_
  ADD INDEX idx_char_corp (corporationID),
  ALGORITHM=INPLACE, LOCK=NONE;
ALTER TABLE character_
  ADD INDEX idx_char_station (stationID),
  ALGORITHM=INPLACE, LOCK=NONE;
ALTER TABLE character_
  ADD INDEX idx_char_system (solarSystemID),
  ALGORITHM=INPLACE, LOCK=NONE;

-- ---------------------------------------------------------------------------
-- ramassemblylines (147,517 rows) — engine conversion
-- ---------------------------------------------------------------------------
-- This is a MyISAM table that the server WRITES at runtime: installing or
-- delivering an industry job updates nextFreeTime. MyISAM takes a table-level
-- lock for a write, so every concurrent industry action on the whole server
-- serialised on one lock. The other MyISAM tables in this schema are read-only
-- seed data and are left alone.
--
-- NOTE: unlike everything above, this one is a full table rebuild and DOES take
-- a lock for its duration (seconds, at this size). It also moves the table into
-- the InnoDB buffer pool, which is where the rest of the working set already is.
ALTER TABLE ramassemblylines ENGINE=InnoDB;
