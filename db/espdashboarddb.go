package db

// ESP dashboard overview: one struct of server-wide figures for the landing page, read in
// parallel panels. Live state (sessions, loaded systems, open sites in memory) is added by the
// esp package; nothing here touches it. See docs/station-admin.md#dashboard.

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"
)

// ESPDashWindow is one ISK figure over the two dashboard windows.
type ESPDashWindow struct{ H24, D7 float64 }

// ESPDashCount is one count over the two dashboard windows.
type ESPDashCount struct{ H24, D7 int64 }

// ESPDashFlowLine is one classified wallet-journal line of the economy panel.
type ESPDashFlowLine struct {
	Key, Label string
	ISK        ESPDashWindow
	Rows       ESPDashCount
}

// ESPDashTopType is one invType row of a leaderboard.
type ESPDashTopType struct {
	TypeID int64
	Name   string
	ISK    float64
	Units  int64
}

// ESPDashNamedCount is one labelled count (a region, a starbase state).
type ESPDashNamedCount struct {
	Name string
	N    int64
}

// ESPDashEconomy is the ISK faucet and sink picture read from market_journal.
type ESPDashEconomy struct {
	Faucets, Sinks                                       []ESPDashFlowLine
	FaucetTotal, SinkTotal, Net, Transfers, Unclassified ESPDashWindow
}

// ESPDashboardOverview is everything the Overview tab shows that comes from the database.
type ESPDashboardOverview struct {
	AsOfUnix int64

	Population struct {
		Pilots, Accounts, PlayerCorps, Alliances int64
		NewPilots                                ESPDashCount
		PilotISK, CorpISK                        float64
	}
	Economy ESPDashEconomy
	Market  struct {
		ActiveOrders, BuyOrders, SellOrders, PlayerOrders, NPCOrders int64
		Trades                                                       ESPDashCount
		Volume                                                       ESPDashWindow
		TopTypes                                                     []ESPDashTopType
		Escrow                                                       ESPEscrowStats
		Courier                                                      ESPCourierStats
	}
	Industry struct {
		Stats      ESPFactoryStats
		LinesPct   float64 // busy lines as a percentage of every line
		Delivered  ESPDashCount
		Blueprints ESPBlueprintStats
	}
	Sites struct {
		Open, Cleared int64
		ByRegion      []ESPDashNamedCount
		SiteIDs       []int64
	}
	Combat struct {
		Kills, PodKills, NPCOnlyKills ESPDashCount
		ISKDestroyed                  ESPDashWindow
		TopKillers                    []KillboardLeader
	}
	Missions struct {
		Offered, Accepted int64
		Completed, Failed ESPDashCount
		RewardISK         ESPDashWindow
	}
	Mining struct {
		Asteroids AsteroidStats
		RegenDays int
		OreSold   ESPDashWindow
		OreUnits  ESPDashCount
	}
	Sovereignty struct {
		ClaimedSystems, ClaimingAlliances int64
		Towers                            int64
		TowersByState                     []ESPDashNamedCount
		Outposts                          int64
	}
	Skills struct {
		Training, FinishingIn24h, Idle, QueueRows int64
		TopSkills                                 []ESPDashTopType
	}
	Warnings []string
}

// ESPDashboardOverviewLoad gathers every overview panel at once. No panel is fatal: one that
// fails stays zero and is named in Warnings, so the page always renders.
func ESPDashboardOverviewLoad(ctx context.Context, db *sql.DB) *ESPDashboardOverview {
	nowT := time.Now()
	o := &ESPDashboardOverview{AsOfUnix: nowT.Unix()}
	nowFT := Win32Now()
	ft24, ft7 := nowFT-win32Day, nowFT-7*win32Day
	t24, t7 := nowT.Add(-24*time.Hour), nowT.Add(-7*24*time.Hour)

	type panel struct {
		name string
		load func() error
	}
	panels := []panel{
		{"population", func() error {
			p := &o.Population
			err := db.QueryRowContext(ctx, `
				SELECT COUNT(*), COALESCE(SUM(balance), 0),
				       COALESCE(SUM(createDateTime >= ?), 0), COALESCE(SUM(createDateTime >= ?), 0)
				  FROM character_ WHERE accountID > 0`, ft24, ft7).
				Scan(&p.Pilots, &p.PilotISK, &p.NewPilots.H24, &p.NewPilots.D7)
			if err != nil {
				return err
			}
			if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM account`).Scan(&p.Accounts); err != nil {
				return err
			}
			err = db.QueryRowContext(ctx, `
				SELECT COUNT(*), COALESCE(SUM(balance), 0) FROM corporation
				 WHERE deleted = 0 AND corporationID >= ?`, MinPlayerEntityID).
				Scan(&p.PlayerCorps, &p.CorpISK)
			if err != nil {
				return err
			}
			return db.QueryRowContext(ctx, `SELECT COUNT(*) FROM alliances WHERE deleted = 0`).Scan(&p.Alliances)
		}},
		{"economy", func() (err error) { o.Economy, err = ESPDashEconomyLoad(ctx, db, nowFT); return }},
		{"market orders", func() error {
			m := &o.Market
			err := db.QueryRowContext(ctx, `
				SELECT COUNT(*), COALESCE(SUM(bid = 1), 0), COALESCE(SUM(bid = 0), 0)
				  FROM market_orders WHERE orderState = 1 AND volRemaining > 0`).
				Scan(&m.ActiveOrders, &m.BuyOrders, &m.SellOrders)
			if err != nil {
				return err
			}
			err = db.QueryRowContext(ctx, `
				SELECT COUNT(*) FROM market_orders
				 WHERE orderState = 1 AND volRemaining > 0 AND charID IN (?, ?, ?)`,
				SeederCharID, AdminSeederCharID, ResaleSeederCharID).Scan(&m.NPCOrders)
			m.PlayerOrders = m.ActiveOrders - m.NPCOrders
			return err
		}},
		{"market trades", func() error {
			m := &o.Market
			return db.QueryRowContext(ctx, `
				SELECT COUNT(*), COALESCE(SUM(price * quantity), 0),
				       COALESCE(SUM(transactionDateTime >= ?), 0),
				       COALESCE(SUM(IF(transactionDateTime >= ?, price * quantity, 0)), 0)
				  FROM market_transactions
				 WHERE transactionType = 1 AND transactionDateTime >= ? AND stationID > 0`, ft24, ft24, ft7).
				Scan(&m.Trades.D7, &m.Volume.D7, &m.Trades.H24, &m.Volume.H24)
		}},
		{"top traded types", func() (err error) {
			o.Market.TopTypes, err = espDashTopTypes(ctx, db, `
				SELECT m.typeID, COALESCE(t.typeName, ''), SUM(m.price * m.quantity), SUM(m.quantity)
				  FROM market_transactions m
				  LEFT JOIN invTypes t ON t.typeID = m.typeID
				 WHERE m.transactionType = 1 AND m.transactionDateTime >= ? AND m.stationID > 0
				 GROUP BY m.typeID ORDER BY 3 DESC LIMIT 10`, ft7)
			return
		}},
		{"escrows and couriers", func() error {
			o.Market.Escrow = ESPGetEscrowStats(ctx, db)
			o.Market.Courier = ESPGetCourierStats(ctx, db)
			return nil
		}},
		{"industry", func() error {
			in := &o.Industry
			st, err := ESPFactoryStatsRow(ctx, db)
			if err != nil {
				return err
			}
			in.Stats = st
			if st.Lines > 0 {
				in.LinesPct = float64(st.BusyLines) * 100 / float64(st.Lines)
			}
			err = db.QueryRowContext(ctx, `
				SELECT COALESCE(SUM(endProductionTime >= ?), 0), COUNT(*)
				  FROM ramJobs WHERE completedStatusID = 101 AND endProductionTime >= ?`, ft24, ft7).
				Scan(&in.Delivered.H24, &in.Delivered.D7)
			if err != nil {
				return err
			}
			in.Blueprints, err = ESPBlueprintStatsRow(ctx, db)
			return err
		}},
		{"open sites", func() error {
			si := &o.Sites
			err := db.QueryRowContext(ctx, `
				SELECT COUNT(*), COALESCE(SUM(COALESCE(clearedAt, 0) > 0), 0) FROM dunOpenSites`).
				Scan(&si.Open, &si.Cleared)
			if err != nil {
				return err
			}
			si.ByRegion, err = espDashNamedCounts(ctx, db, `
				SELECT COALESCE(r.regionName, ''), COUNT(*)
				  FROM dunOpenSites o
				  LEFT JOIN mapSolarSystems s ON s.solarSystemID = o.solarSystemID
				  LEFT JOIN mapRegions r ON r.regionID = s.regionID
				 GROUP BY s.regionID, r.regionName ORDER BY 2 DESC LIMIT 6`)
			if err != nil {
				return err
			}
			rows, err := db.QueryContext(ctx, `SELECT siteID FROM dunOpenSites`)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var id int64
				if err := rows.Scan(&id); err != nil {
					return err
				}
				si.SiteIDs = append(si.SiteIDs, id)
			}
			return rows.Err()
		}},
		{"combat", func() error {
			c := &o.Combat
			var err error
			if c.Kills.H24, c.ISKDestroyed.H24, err = KillmailTotals(ctx, db, KillmailFilter{Since: t24}); err != nil {
				return err
			}
			if c.Kills.D7, c.ISKDestroyed.D7, err = KillmailTotals(ctx, db, KillmailFilter{Since: t7}); err != nil {
				return err
			}
			if c.PodKills.H24, _, err = KillmailTotals(ctx, db, KillmailFilter{Since: t24, Kind: "pod"}); err != nil {
				return err
			}
			if c.PodKills.D7, _, err = KillmailTotals(ctx, db, KillmailFilter{Since: t7, Kind: "pod"}); err != nil {
				return err
			}
			err = db.QueryRowContext(ctx, `
				SELECT COALESCE(SUM(killTime >= ?), 0), COUNT(*) FROM killmails
				 WHERE npcOnly = 1 AND killTime >= ?`, t24.Unix(), t7.Unix()).
				Scan(&c.NPCOnlyKills.H24, &c.NPCOnlyKills.D7)
			if err != nil {
				return err
			}
			c.TopKillers, err = TopKillers(ctx, db, t7, 5, false)
			return err
		}},
		{"missions", func() error {
			m := &o.Missions
			return db.QueryRowContext(ctx, `
				SELECT COALESCE(SUM(state = 'offered'), 0), COALESCE(SUM(state = 'accepted'), 0),
				       COALESCE(SUM(state = 'completed' AND completedAt >= NOW() - INTERVAL 1 DAY), 0),
				       COALESCE(SUM(state = 'completed' AND completedAt >= NOW() - INTERVAL 7 DAY), 0),
				       COALESCE(SUM(state = 'failed' AND completedAt >= NOW() - INTERVAL 1 DAY), 0),
				       COALESCE(SUM(state = 'failed' AND completedAt >= NOW() - INTERVAL 7 DAY), 0),
				       COALESCE(SUM(IF(state = 'completed' AND completedAt >= NOW() - INTERVAL 1 DAY,
				                       COALESCE(rewardISK, 0) + COALESCE(bonusISK, 0), 0)), 0),
				       COALESCE(SUM(IF(state = 'completed' AND completedAt >= NOW() - INTERVAL 7 DAY,
				                       COALESCE(rewardISK, 0) + COALESCE(bonusISK, 0), 0)), 0)
				  FROM characterMissions`).
				Scan(&m.Offered, &m.Accepted, &m.Completed.H24, &m.Completed.D7,
					&m.Failed.H24, &m.Failed.D7, &m.RewardISK.H24, &m.RewardISK.D7)
		}},
		{"mining", func() error {
			mi := &o.Mining
			mi.RegenDays = AsteroidRegenDays(ctx, db)
			st, err := GetAsteroidStats(ctx, db, mi.RegenDays)
			if err != nil {
				return err
			}
			mi.Asteroids = st
			// Ore is category 25; a sale to an NPC order counts because the seeder's buy row exists.
			return db.QueryRowContext(ctx, `
				SELECT COALESCE(SUM(IF(m.transactionDateTime >= ?, m.price * m.quantity, 0)), 0),
				       COALESCE(SUM(m.price * m.quantity), 0),
				       COALESCE(SUM(IF(m.transactionDateTime >= ?, m.quantity, 0)), 0),
				       COALESCE(SUM(m.quantity), 0)
				  FROM market_transactions m
				  JOIN invTypes t ON t.typeID = m.typeID
				  JOIN invGroups g ON g.groupID = t.groupID
				 WHERE g.categoryID = 25 AND m.transactionType = 1
				   AND m.transactionDateTime >= ? AND m.stationID > 0`, ft24, ft24, ft7).
				Scan(&mi.OreSold.H24, &mi.OreSold.D7, &mi.OreUnits.H24, &mi.OreUnits.D7)
		}},
		{"sovereignty", func() error {
			sv := &o.Sovereignty
			err := db.QueryRowContext(ctx, `
				SELECT COUNT(*), COUNT(DISTINCT allianceID) FROM solarSystemSov WHERE allianceID > 0`).
				Scan(&sv.ClaimedSystems, &sv.ClaimingAlliances)
			if err != nil {
				return err
			}
			err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM staStations WHERE stationID >= ?`,
				OutpostStationIDFloor-3).Scan(&sv.Outposts)
			if err != nil {
				return err
			}
			sv.TowersByState, err = espDashNamedCounts(ctx, db,
				`SELECT state, COUNT(*) FROM starbase GROUP BY state ORDER BY 2 DESC`)
			for _, c := range sv.TowersByState {
				sv.Towers += c.N
			}
			return err
		}},
		{"skills", func() error {
			sk := &o.Skills
			err := db.QueryRowContext(ctx, `
				SELECT COUNT(*), COALESCE(SUM(st.endOfTraining <= ?), 0)
				  FROM chrSkillTraining st
				  JOIN character_ c ON c.characterID = st.characterID
				 WHERE c.accountID > 0 AND st.endOfTraining > ?`, nowFT+win32Day, nowFT).
				Scan(&sk.Training, &sk.FinishingIn24h)
			if err != nil {
				return err
			}
			if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM chrSkillQueue`).Scan(&sk.QueueRows); err != nil {
				return err
			}
			sk.TopSkills, err = espDashTopTypes(ctx, db, `
				SELECT st.typeID, COALESCE(t.typeName, ''), 0, COUNT(*)
				  FROM chrSkillTraining st
				  JOIN character_ c ON c.characterID = st.characterID
				  LEFT JOIN invTypes t ON t.typeID = st.typeID
				 WHERE c.accountID > 0 AND st.endOfTraining > ?
				 GROUP BY st.typeID, t.typeName ORDER BY 4 DESC LIMIT 5`, nowFT)
			return err
		}},
	}

	warnings := make([]string, len(panels))
	var wg sync.WaitGroup
	for i, p := range panels {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A panic on this goroutine would take the process down; it becomes a warning instead.
			defer func() {
				if r := recover(); r != nil {
					warnings[i] = fmt.Sprintf("%s: panic: %v", p.name, r)
				}
			}()
			if err := p.load(); err != nil {
				warnings[i] = fmt.Sprintf("%s: %v", p.name, err)
			}
		}()
	}
	wg.Wait()
	for _, w := range warnings {
		if w != "" {
			o.Warnings = append(o.Warnings, w)
		}
	}
	// Idle pilots need two panels, so it is derived after both finish.
	if o.Population.Pilots > o.Skills.Training {
		o.Skills.Idle = o.Population.Pilots - o.Skills.Training
	}
	return o
}

// espDashTopTypes runs a leaderboard query whose columns are typeID, name, ISK, units.
func espDashTopTypes(ctx context.Context, db *sql.DB, query string, args ...any) ([]ESPDashTopType, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ESPDashTopType
	for rows.Next() {
		var r ESPDashTopType
		if err := rows.Scan(&r.TypeID, &r.Name, &r.ISK, &r.Units); err != nil {
			return nil, err
		}
		if r.Name == "" {
			r.Name = fmt.Sprintf("Type %d", r.TypeID)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// espDashNamedCounts runs a query whose columns are a label and a count.
func espDashNamedCounts(ctx context.Context, db *sql.DB, query string, args ...any) ([]ESPDashNamedCount, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ESPDashNamedCount
	for rows.Next() {
		var r ESPDashNamedCount
		if err := rows.Scan(&r.Name, &r.N); err != nil {
			return nil, err
		}
		if r.Name == "" {
			r.Name = "(unknown)"
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Economy

// espDashFlowLines is every journal line the economy panel knows, in display order. dir is
// "faucet" (ISK created), "sink" (ISK destroyed) or "neutral" (moved between wallets).
var espDashFlowLines = []struct{ key, label, dir string }{
	{"bounty", "Bounties", "faucet"},
	{"mission", "Mission rewards", "faucet"},
	{"mission_bonus", "Mission time bonuses", "faucet"},
	{"insurance_pay", "Insurance payouts", "faucet"},
	{"npc_sale", "Sales to NPC orders", "faucet"},
	{"gm_in", "GM grants", "faucet"},

	{"npc_buy", "Purchases from NPC orders", "sink"},
	{"skills", "Skill injection", "sink"},
	{"broker", "Broker fees", "sink"},
	{"sales_tax", "Sales tax", "sink"},
	{"job_fees", "Industry job fees", "sink"},
	{"rent", "Office and slot rent", "sink"},
	{"insurance_prem", "Insurance premiums", "sink"},
	{"repair", "Repairs", "sink"},
	{"clone", "Clone upgrades", "sink"},
	{"jump_clone", "Jump clone installs", "sink"},
	{"concord", "CONCORD and registration fees", "sink"},
	{"contraband", "Contraband fines", "sink"},
	{"agent_services", "Agent services", "sink"},
	{"lp_store", "LP store", "sink"},
	{"listing_fee", "Escrow listing fees", "sink"},
	{"gm_out", "GM removals", "sink"},

	{"transfers", "Transfers between wallets", "neutral"},
}

// espDashEconomyCaseSQL names each market_journal row. refType 2 writes ownerID1 as the buyer
// and ownerID2 as the seller on both journals (marketorders.go journal), so the NPC side of a
// trade is the owner below MinPlayerEntityID. %[2]d is the Genolution owner of a skill injection.
var espDashEconomyCaseSQL = fmt.Sprintf(`CASE
	WHEN refTypeID = 17 AND amount > 0 THEN 'bounty'
	WHEN refTypeID = 17 THEN 'transfers'
	WHEN refTypeID = 33 THEN 'mission'
	WHEN refTypeID = 34 THEN 'mission_bonus'
	WHEN refTypeID = 19 AND amount > 0 THEN 'insurance_pay'
	WHEN refTypeID = 19 THEN 'insurance_prem'
	WHEN refTypeID = 10 AND ownerID1 = characterID AND ownerID2 = characterID THEN IF(amount > 0, 'gm_in', 'gm_out')
	WHEN refTypeID = 10 THEN 'transfers'
	WHEN refTypeID = 2 AND (COALESCE(ownerID2, 0) = %[2]d OR reason LIKE 'Skill injection%%') THEN 'skills'
	WHEN refTypeID = 2 AND reason = 'Broker fee' THEN 'broker'
	WHEN refTypeID = 2 AND amount < 0 AND COALESCE(ownerID2, 0) < %[1]d THEN 'npc_buy'
	WHEN refTypeID = 2 AND amount > 0 AND COALESCE(ownerID1, 0) < %[1]d THEN 'npc_sale'
	WHEN refTypeID = 2 THEN 'transfers'
	WHEN refTypeID = 46 AND ownerID1 = characterID AND ownerID2 = characterID THEN 'clone'
	WHEN refTypeID = 46 THEN 'broker'
	WHEN refTypeID = 54 THEN 'sales_tax'
	WHEN refTypeID = 55 THEN 'jump_clone'
	WHEN refTypeID = 15 THEN 'repair'
	WHEN refTypeID BETWEEN 56 AND 62 THEN IF(COALESCE(ownerID2, 0) < %[1]d, 'job_fees', 'transfers')
	WHEN refTypeID IN (13, 14) THEN IF(COALESCE(ownerID2, 0) < %[1]d, 'rent', 'transfers')
	WHEN refTypeID IN (39, 48, 49, 50) THEN 'concord'
	WHEN refTypeID = 73 THEN 'contraband'
	WHEN refTypeID IN (27, 29) THEN 'agent_services'
	WHEN refTypeID = 35 THEN 'lp_store'
	WHEN refTypeID = 23 AND reason = 'Escrow listing fee' THEN 'listing_fee'
	WHEN refTypeID IN (1, 6, 22, 23, 37, 38, 42) THEN 'transfers'
	ELSE 'other'
END`, MinPlayerEntityID, 1000140)

// ESPDashEconomyLoad classifies the last seven days of market_journal into faucets, sinks and
// transfers, each over both windows. A transfer appears on two journals, so only its positive
// side is summed. Rows no line names land in Unclassified. Uses idx_mj_date.
func ESPDashEconomyLoad(ctx context.Context, db *sql.DB, nowFT int64) (ESPDashEconomy, error) {
	var e ESPDashEconomy
	ft24, ft7 := nowFT-win32Day, nowFT-7*win32Day
	rows, err := db.QueryContext(ctx, `
		SELECT line, amount < 0,
		       COALESCE(SUM(IF(transDate >= ?, ABS(amount), 0)), 0), COALESCE(SUM(ABS(amount)), 0),
		       COALESCE(SUM(transDate >= ?), 0), COUNT(*)
		  FROM (SELECT transDate, amount, `+espDashEconomyCaseSQL+` AS line
		          FROM market_journal WHERE transDate >= ?) x
		 GROUP BY line, amount < 0`, ft24, ft24, ft7)
	if err != nil {
		return e, fmt.Errorf("ESPDashEconomyLoad: %w", err)
	}
	defer rows.Close()
	byKey := map[string]*ESPDashFlowLine{}
	for rows.Next() {
		var key string
		var negative bool
		var isk ESPDashWindow
		var n ESPDashCount
		if err := rows.Scan(&key, &negative, &isk.H24, &isk.D7, &n.H24, &n.D7); err != nil {
			return e, fmt.Errorf("ESPDashEconomyLoad scan: %w", err)
		}
		switch key {
		case "other":
			e.Unclassified.H24 += isk.H24
			e.Unclassified.D7 += isk.D7
		case "transfers":
			if !negative {
				e.Transfers.H24 += isk.H24
				e.Transfers.D7 += isk.D7
			}
		default:
			l := byKey[key]
			if l == nil {
				l = &ESPDashFlowLine{Key: key}
				byKey[key] = l
			}
			l.ISK.H24 += isk.H24
			l.ISK.D7 += isk.D7
			l.Rows.H24 += n.H24
			l.Rows.D7 += n.D7
		}
	}
	if err := rows.Err(); err != nil {
		return e, fmt.Errorf("ESPDashEconomyLoad rows: %w", err)
	}
	for _, def := range espDashFlowLines {
		l := byKey[def.key]
		if l == nil {
			continue
		}
		l.Label = def.label
		switch def.dir {
		case "faucet":
			e.Faucets = append(e.Faucets, *l)
			e.FaucetTotal.H24 += l.ISK.H24
			e.FaucetTotal.D7 += l.ISK.D7
		case "sink":
			e.Sinks = append(e.Sinks, *l)
			e.SinkTotal.H24 += l.ISK.H24
			e.SinkTotal.D7 += l.ISK.D7
		}
	}
	e.Net.H24 = e.FaucetTotal.H24 - e.SinkTotal.H24
	e.Net.D7 = e.FaucetTotal.D7 - e.SinkTotal.D7
	return e, nil
}
