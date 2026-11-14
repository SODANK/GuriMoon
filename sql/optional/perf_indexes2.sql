-- Round two. Where round one was reasoned from the code, this round was
-- measured: the server was sampled 120 times while live and the queries below
-- are the ones actually found in flight, ranked by how often a sample caught
-- them (which measures time spent, not call count).
--
-- Idempotent, online, safe against a running server.
--
-- THE SERVER APPLIES THESE ITSELF NOW — see the note at the top of
-- perf_indexes.sql. db/perfindex.go carries every statement below, comments
-- included; this file remains the readable record.

-- ---------------------------------------------------------------------------
-- ramassemblylines — 146,370 rows, EXPLAIN type=ALL
-- ---------------------------------------------------------------------------
-- FindAssemblyLineForActivity (db/ramdb.go:105) filters "containerID = ? AND
-- activityID = ?" and the table had NOTHING but PRIMARY(assemblyLineID). So
-- every factory-line pick scanned all 146k rows — and then, for each row that
-- survived the filter, ran the correlated ramJobs subquery in its ORDER BY.
-- Every bot that manufactures hits this. It was also MyISAM until round one,
-- so the scan held a table lock the whole way.
--
-- endProductionTime is not in this index and cannot be: it lives in ramJobs.
-- The idx_ramjobs_line index below is the other half — it turns that
-- correlated subquery from a scan per candidate row into a point lookup.
ALTER TABLE ramassemblylines
  ADD INDEX idx_ral_container_activity (containerID, activityID),
  ALGORITHM=INPLACE, LOCK=NONE;

ALTER TABLE ramJobs
  ADD INDEX idx_ramjobs_line (assemblyLineID, completedStatusID, endProductionTime),
  ALGORITHM=INPLACE, LOCK=NONE;

-- ---------------------------------------------------------------------------
-- market_transactions — the skill-price aggregate, type=ALL over 370,309 rows
-- ---------------------------------------------------------------------------
-- SkillMarketPrices (db/skillpurchasedb.go:98) aggregates every skill-category
-- buy transaction in the trailing window. It was the single most expensive
-- query on the server by sampled time. The round-one index leads with clientID
-- and so cannot serve it; this one leads with the two columns it actually
-- filters on and carries typeID/quantity/price so the aggregate never touches
-- the table.
ALTER TABLE market_transactions
  ADD INDEX idx_mt_type_date_agg (transactionType, transactionDateTime, typeID, quantity, price),
  ALGORITHM=INPLACE, LOCK=NONE;

-- ---------------------------------------------------------------------------
-- market_orders — the trade-route gap CTE
-- ---------------------------------------------------------------------------
-- rolebots/trade.go:672 groups the whole live order book by typeID to find the
-- types where a bid beats an ask. An aggregate over the entire market cannot be
-- narrowed by any index — but it can be made index-only. The round-one
-- composite lacks `price`, so the scan read 133,418 table rows to fetch it.
-- Leading with the two equality/range filters and following with typeID gives
-- the GROUP BY its order for free as well.
ALTER TABLE market_orders
  ADD INDEX idx_mo_gap (orderState, volRemaining, typeID, bid, price),
  ALGORITHM=INPLACE, LOCK=NONE;

-- ---------------------------------------------------------------------------
-- invTypes — the market catalogue lookup, type=ALL over 11,981 rows
-- ---------------------------------------------------------------------------
-- "WHERE groupID IN (...) AND published = 1 AND marketGroupID > 0 ORDER BY
-- typeName" appeared repeatedly in the sample. The table had only PRIMARY and
-- marketGroupID, and the optimiser chose neither. Small table, but this runs
-- constantly from fit generation and the build book.
ALTER TABLE invTypes
  ADD INDEX idx_invtypes_group_pub (groupID, published, marketGroupID, typeName),
  ALGORITHM=INPLACE, LOCK=NONE;
