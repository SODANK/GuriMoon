package db

// Science & Industry overview (ESP /industry/factories): the aggregate half of the page. Every
// number the overview panels draw and the grouped roll-up behind the Manufacturing and Science
// tabs live here; per-job rows come from ESPListJobs, and a group opens through the filter that
// built it so a group row's count matches the jobs under it. Nothing is stored: a line is busy
// when an undelivered ramJobs row points at it, so figures read completedStatusID < 100
// (indexed). The 14-day throughput reads delivered history and leans on idx_ramjobs_status_end
// and idx_ramjobs_install. Each panel is its own query and falls back to empty; failures are
// listed on the page.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	"gurimoon/safego"
)

const (
	win32Second = int64(10000000)
	win32Hour   = 3600 * win32Second
	win32Day    = 24 * win32Hour

	// OverviewHistHours is how far ahead the finish histogram looks.
	OverviewHistHours = 24
	// OverviewDays is the throughput window, today included.
	OverviewDays = 14
)

// ESPSplit is a count split the way the page splits everything: manufacturing
// against every other activity.
type ESPSplit struct{ Mfg, Sci int64 }

// Total is both halves.
func (s ESPSplit) Total() int64 { return s.Mfg + s.Sci }

// ESPActivityStat is one activity's row in the breakdown.
type ESPActivityStat struct {
	ActivityID int64
	Name       string
	Short      string
	Running    int64
	Ready      int64
	Runs       int64
	LinesBusy  int64 // distinct lines carrying one of its jobs
	LinesTotal int64 // lines of that activity that exist

	// Bar widths, precomputed against the largest row so the template does no
	// arithmetic.
	RunningPct float64
	ReadyPct   float64
}

// Active is every job of the activity still holding a line.
func (a ESPActivityStat) Active() int64 { return a.Running + a.Ready }

// UtilPct is the share of the activity's lines in use.
func (a ESPActivityStat) UtilPct() float64 {
	if a.LinesTotal <= 0 {
		return 0
	}
	return float64(a.LinesBusy) * 100 / float64(a.LinesTotal)
}

// ESPHistBucket is one hour of the finish histogram.
type ESPHistBucket struct {
	Hour     int // 0 = finishing within the next hour
	Mfg, Sci int64

	MfgPct, SciPct float64 // heights, % of the tallest bucket
}

// Total is both halves of the hour.
func (b ESPHistBucket) Total() int64 { return b.Mfg + b.Sci }

// ESPDayPoint is one UTC day of throughput.
type ESPDayPoint struct {
	Day                    string // 01-02
	Today                  bool
	InstMfg, InstSci       int64
	DelMfg, DelSci         int64
	InstPct, DelPct        float64 // total heights, % of the busiest day
	InstMfgPct, InstSciPct float64
	DelMfgPct, DelSciPct   float64
}

// ESPTopItem is a product (manufacturing) or a blueprint worked on (science).
type ESPTopItem struct {
	TypeID        int64
	Name          string
	ActivityID    int64
	ActivityShort string
	Jobs          int64
	Runs          int64
	Installers    int64
}

// ESPTopInstaller is one pilot's active load.
type ESPTopInstaller struct {
	CharID   int64
	Name     string
	Corp     string
	Jobs     int64
	Runs     int64
	Lines    int64
	Mfg, Sci int64
	CorpJobs int64
}

// ESPTopCorp is a corporation's members' combined load.
type ESPTopCorp struct {
	CorpID     int64
	Name       string
	Jobs       int64
	Runs       int64
	Installers int64
	CorpJobs   int64 // jobs installed for the corporation rather than the pilot
}

// ESPHotRegion is a region's line utilisation.
type ESPHotRegion struct {
	RegionID   int64
	Name       string
	LinesBusy  int64
	LinesTotal int64
	Jobs       int64
}

// UtilPct is the share of the region's lines in use.
func (r ESPHotRegion) UtilPct() float64 {
	if r.LinesTotal <= 0 {
		return 0
	}
	return float64(r.LinesBusy) * 100 / float64(r.LinesTotal)
}

// ESPHotStation is a station's line utilisation.
type ESPHotStation struct {
	StationID  int64
	Name       string
	System     string
	Region     string
	Security   float64
	LinesBusy  int64
	LinesTotal int64
	Jobs       int64
	Known      bool
}

// UtilPct is the share of the station's lines in use.
func (s ESPHotStation) UtilPct() float64 {
	if s.LinesTotal <= 0 {
		return 0
	}
	return float64(s.LinesBusy) * 100 / float64(s.LinesTotal)
}

// ESPResearchStats is the Research Facility (Duplicating / Reverse Engineering)
// panel. Those jobs live in their own tables, never in ramJobs.
type ESPResearchStats struct {
	Present       bool // the tables exist and answered
	SlotsRented   int64
	RentalStation int64
	DupRunning    int64
	REngRunning   int64
	DupDone7d     int64
	REngDone7d    int64
}

// ESPIndustryOverview is everything the overview tab draws.
type ESPIndustryOverview struct {
	AsOfUnix int64
	Stats    ESPFactoryStats

	CorpJobs, PersonalJobs int64
	PosJobs, StationJobs   int64

	Activities []ESPActivityStat

	Hist    []ESPHistBucket
	Backlog ESPSplit // ready, or running and past its end time
	Later   ESPSplit // running, finishing beyond the histogram window

	Days []ESPDayPoint

	TopProducts   []ESPTopItem
	TopTargets    []ESPTopItem
	TopInstallers []ESPTopInstaller
	TopCorps      []ESPTopCorp
	Regions       []ESPHotRegion
	Stations      []ESPHotStation

	Research ESPResearchStats

	// Warnings names every panel whose query failed.
	Warnings []string
}

// CorpSharePct is the share of active jobs installed for a corporation.
func (o *ESPIndustryOverview) CorpSharePct() float64 {
	t := o.CorpJobs + o.PersonalJobs
	if t <= 0 {
		return 0
	}
	return float64(o.CorpJobs) * 100 / float64(t)
}

// PosSharePct is the share of active jobs running on a POS array.
func (o *ESPIndustryOverview) PosSharePct() float64 {
	t := o.PosJobs + o.StationJobs
	if t <= 0 {
		return 0
	}
	return float64(o.PosJobs) * 100 / float64(t)
}

// ---------------------------------------------------------------------------
// Pure bucketing, kept apart from the SQL so it can be tested.

// espHistRow is one (hour, class) cell as the histogram query returns it.
type espHistRow struct {
	Hour int64
	Mfg  bool
	N    int64
}

// espBuildHistogram lays cells into OverviewHistHours buckets and sizes the
// bars. A cell outside the window is dropped: the query bounds it, but a clock
// tick between the two statements could still land one on the edge.
func espBuildHistogram(rows []espHistRow) []ESPHistBucket {
	out := make([]ESPHistBucket, OverviewHistHours)
	for i := range out {
		out[i].Hour = i
	}
	for _, r := range rows {
		if r.Hour < 0 || r.Hour >= OverviewHistHours {
			continue
		}
		if r.Mfg {
			out[r.Hour].Mfg += r.N
		} else {
			out[r.Hour].Sci += r.N
		}
	}
	var max int64
	for _, b := range out {
		if t := b.Total(); t > max {
			max = t
		}
	}
	if max > 0 {
		for i := range out {
			out[i].MfgPct = float64(out[i].Mfg) * 100 / float64(max)
			out[i].SciPct = float64(out[i].Sci) * 100 / float64(max)
		}
	}
	return out
}

// espDayRow is one (day index, class) cell of the throughput queries.
type espDayRow struct {
	Idx int64
	Mfg bool
	N   int64
}

// espBuildDays lays installs and deliveries into OverviewDays UTC days ending
// on the day of now, oldest first, and sizes the bars against the busiest.
func espBuildDays(now time.Time, installs, delivered []espDayRow) []ESPDayPoint {
	now = now.UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	out := make([]ESPDayPoint, OverviewDays)
	for i := range out {
		d := today.AddDate(0, 0, i-(OverviewDays-1))
		out[i].Day = d.Format("01-02")
		out[i].Today = i == OverviewDays-1
	}
	fill := func(rows []espDayRow, mfg, sci func(*ESPDayPoint) *int64) {
		for _, r := range rows {
			if r.Idx < 0 || r.Idx >= OverviewDays {
				continue
			}
			if r.Mfg {
				*mfg(&out[r.Idx]) += r.N
			} else {
				*sci(&out[r.Idx]) += r.N
			}
		}
	}
	fill(installs, func(p *ESPDayPoint) *int64 { return &p.InstMfg }, func(p *ESPDayPoint) *int64 { return &p.InstSci })
	fill(delivered, func(p *ESPDayPoint) *int64 { return &p.DelMfg }, func(p *ESPDayPoint) *int64 { return &p.DelSci })

	var maxInst, maxDel int64
	for _, p := range out {
		if t := p.InstMfg + p.InstSci; t > maxInst {
			maxInst = t
		}
		if t := p.DelMfg + p.DelSci; t > maxDel {
			maxDel = t
		}
	}
	for i := range out {
		p := &out[i]
		if maxInst > 0 {
			p.InstMfgPct = float64(p.InstMfg) * 100 / float64(maxInst)
			p.InstSciPct = float64(p.InstSci) * 100 / float64(maxInst)
			p.InstPct = p.InstMfgPct + p.InstSciPct
		}
		if maxDel > 0 {
			p.DelMfgPct = float64(p.DelMfg) * 100 / float64(maxDel)
			p.DelSciPct = float64(p.DelSci) * 100 / float64(maxDel)
			p.DelPct = p.DelMfgPct + p.DelSciPct
		}
	}
	return out
}

// espWinStart is the Win32 time of UTC midnight OverviewDays-1 days before now.
func espWinStart(now time.Time) int64 {
	now = now.UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	return UnixToWin32(today.AddDate(0, 0, -(OverviewDays - 1)).Unix())
}

// ---------------------------------------------------------------------------
// Overview

// ESPIndustryOverviewLoad gathers every overview panel. Only the headline stats
// can fail the call; any other panel that errors is left empty and named in
// Warnings.
func ESPIndustryOverviewLoad(ctx context.Context, db *sql.DB) (*ESPIndustryOverview, error) {
	stats, err := ESPFactoryStatsRow(ctx, db)
	if err != nil {
		return nil, err
	}
	nowT := time.Now()
	o := &ESPIndustryOverview{AsOfUnix: nowT.Unix(), Stats: stats}
	now := Win32Now()

	// Every panel below is an independent read that fills its own fields of o, so they run at once
	// and the page costs the slowest panel. Warnings keep panel order.
	type panel struct {
		name string
		load func() error
	}
	panels := []panel{
		// Corp vs personal, POS vs station: one scan of the active jobs. ownerID 0 means the installer;
		// a corp job carries the corporation there.
		{"job ownership", func() error {
			return db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(COALESCE(ownerID, 0) NOT IN (0, installerID)), 0),
		       COALESCE(SUM(COALESCE(ownerID, 0) IN (0, installerID)), 0),
		       COALESCE(SUM(containerID NOT BETWEEN 60000000 AND 63999999), 0),
		       COALESCE(SUM(containerID BETWEEN 60000000 AND 63999999), 0)
		  FROM ramJobs WHERE completedStatusID < 100`).
				Scan(&o.CorpJobs, &o.PersonalJobs, &o.PosJobs, &o.StationJobs)
		}},
		{"activity breakdown", func() (err error) { o.Activities, err = espActivityBreakdown(ctx, db); return }},
		{"finish timeline", func() (err error) { o.Hist, o.Backlog, o.Later, err = espFinishHistogram(ctx, db, now); return }},
		{"throughput", func() (err error) { o.Days, err = espThroughput(ctx, db, nowT); return }},
		{"top products", func() (err error) { o.TopProducts, err = espTopItems(ctx, db, true); return }},
		{"top science targets", func() (err error) { o.TopTargets, err = espTopItems(ctx, db, false); return }},
		{"top installers", func() (err error) { o.TopInstallers, err = espTopInstallers(ctx, db, 12); return }},
		{"top corporations", func() (err error) { o.TopCorps, err = espTopCorps(ctx, db, 10); return }},
		{"hot regions", func() (err error) { o.Regions, err = espHotRegions(ctx, db, 12); return }},
		{"hot stations", func() (err error) { o.Stations, err = espHotStations(ctx, db, 15); return }},
		{"research facility", func() error { o.Research = espResearchStats(ctx, db, now); return nil }},
	}
	warnings := make([]string, len(panels))
	var wg sync.WaitGroup
	for i, p := range panels {
		wg.Add(1)
		go func() {
			defer safego.Recover("ESPIndustryOverviewLoad")
			defer wg.Done()
			// net/http would recover a panic on the handler goroutine but not on this one; a panicking
			// panel becomes a warning.
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
	return o, nil
}

// espActivityBreakdown is jobs and lines per activity. Lines come from
// ramassemblylines, so an activity with capacity and no job still has its row.
func espActivityBreakdown(ctx context.Context, db *sql.DB) ([]ESPActivityStat, error) {
	byAct := map[int64]*ESPActivityStat{}
	get := func(id int64) *ESPActivityStat {
		a := byAct[id]
		if a == nil {
			a = &ESPActivityStat{ActivityID: id, Name: ESPActivityName[id], Short: ESPActivityShortName(id)}
			if a.Name == "" {
				a.Name = fmt.Sprintf("Activity %d", id)
			}
			byAct[id] = a
		}
		return a
	}

	rows, err := db.QueryContext(ctx, `
		SELECT activityID, COALESCE(SUM(completedStatusID = 0), 0), COALESCE(SUM(completedStatusID = 1), 0),
		       COALESCE(SUM(runs), 0), COUNT(DISTINCT assemblyLineID)
		  FROM ramJobs WHERE completedStatusID < 100 GROUP BY activityID`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var running, ready, runs, lines int64
		if err := rows.Scan(&id, &running, &ready, &runs, &lines); err != nil {
			rows.Close()
			return nil, err
		}
		a := get(id)
		a.Running, a.Ready, a.Runs, a.LinesBusy = running, ready, runs, lines
	}
	rows.Close()

	lrows, err := db.QueryContext(ctx, `
		SELECT COALESCE(activityID, 0), COUNT(*) FROM ramassemblylines GROUP BY activityID`)
	if err != nil {
		return nil, err
	}
	for lrows.Next() {
		var id, n int64
		if err := lrows.Scan(&id, &n); err != nil {
			lrows.Close()
			return nil, err
		}
		get(id).LinesTotal = n
	}
	lrows.Close()

	// The five working activities first, in the blueprint window's order, then
	// anything else that turned up.
	var out []ESPActivityStat
	seen := map[int64]bool{}
	for _, id := range ESPEditableActivities {
		if a := byAct[id]; a != nil {
			out = append(out, *a)
			seen[id] = true
		}
	}
	for id, a := range byAct {
		if !seen[id] && (a.Active() > 0 || a.LinesTotal > 0) {
			out = append(out, *a)
		}
	}
	var max int64
	for _, a := range out {
		if a.Active() > max {
			max = a.Active()
		}
	}
	if max > 0 {
		for i := range out {
			out[i].RunningPct = float64(out[i].Running) * 100 / float64(max)
			out[i].ReadyPct = float64(out[i].Ready) * 100 / float64(max)
		}
	}
	return out, nil
}

// espFinishHistogram buckets running jobs by the hour they finish in, over the
// next OverviewHistHours, and counts the two ends the histogram cannot show:
// jobs already waiting to be collected, and jobs finishing past the window.
func espFinishHistogram(ctx context.Context, db *sql.DB, now int64) ([]ESPHistBucket, ESPSplit, ESPSplit, error) {
	var back, later ESPSplit
	rows, err := db.QueryContext(ctx, `
		SELECT FLOOR((endProductionTime - ?) / ?), activityID = 1, COUNT(*)
		  FROM ramJobs
		 WHERE completedStatusID = 0 AND endProductionTime > ? AND endProductionTime <= ?
		 GROUP BY 1, 2`, now, win32Hour, now, now+OverviewHistHours*win32Hour)
	if err != nil {
		return nil, back, later, err
	}
	var cells []espHistRow
	for rows.Next() {
		var c espHistRow
		var mfg int64
		if err := rows.Scan(&c.Hour, &mfg, &c.N); err != nil {
			rows.Close()
			return nil, back, later, err
		}
		c.Mfg = mfg != 0
		cells = append(cells, c)
	}
	rows.Close()

	err = db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(activityID = 1 AND (completedStatusID = 1 OR endProductionTime <= ?)), 0),
		       COALESCE(SUM(activityID <> 1 AND (completedStatusID = 1 OR endProductionTime <= ?)), 0),
		       COALESCE(SUM(activityID = 1 AND completedStatusID = 0 AND endProductionTime > ?), 0),
		       COALESCE(SUM(activityID <> 1 AND completedStatusID = 0 AND endProductionTime > ?), 0)
		  FROM ramJobs WHERE completedStatusID < 100`,
		now, now, now+OverviewHistHours*win32Hour, now+OverviewHistHours*win32Hour).
		Scan(&back.Mfg, &back.Sci, &later.Mfg, &later.Sci)
	if err != nil {
		return nil, back, later, err
	}
	return espBuildHistogram(cells), back, later, nil
}

// espThroughput counts installs and deliveries per UTC day. ramJobs keeps no pickup time, so a
// delivered job is dated by its end time, the earliest it could have been collected.
func espThroughput(ctx context.Context, db *sql.DB, nowT time.Time) ([]ESPDayPoint, error) {
	start := espWinStart(nowT)
	now := UnixToWin32(nowT.Unix())
	read := func(q string, args ...interface{}) ([]espDayRow, error) {
		rows, err := db.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []espDayRow
		for rows.Next() {
			var r espDayRow
			var mfg int64
			if err := rows.Scan(&r.Idx, &mfg, &r.N); err != nil {
				return nil, err
			}
			r.Mfg = mfg != 0
			out = append(out, r)
		}
		return out, rows.Err()
	}
	installs, err := read(`
		SELECT FLOOR((installTime - ?) / ?), activityID = 1, COUNT(*)
		  FROM ramJobs WHERE installTime >= ? AND installTime <= ?
		 GROUP BY 1, 2`, start, win32Day, start, now)
	if err != nil {
		return nil, err
	}
	delivered, err := read(`
		SELECT FLOOR((endProductionTime - ?) / ?), activityID = 1, COUNT(*)
		  FROM ramJobs
		 WHERE completedStatusID = 101 AND endProductionTime >= ? AND endProductionTime <= ?
		 GROUP BY 1, 2`, start, win32Day, start, now)
	if err != nil {
		return nil, err
	}
	return espBuildDays(nowT, installs, delivered), nil
}

// espTopItems ranks what is being built (manufacturing, by output item) or
// worked on (science, by blueprint and activity).
func espTopItems(ctx context.Context, db *sql.DB, manufacturing bool) ([]ESPTopItem, error) {
	var q string
	if manufacturing {
		q = `
			SELECT j.outputTypeID, COALESCE(MIN(ot.typeName), CONCAT('type ', j.outputTypeID)), 1,
			       COUNT(*), COALESCE(SUM(j.runs), 0), COUNT(DISTINCT j.installerID)
			  FROM ramJobs j
			  LEFT JOIN invTypes ot ON ot.typeID = j.outputTypeID
			 WHERE j.completedStatusID < 100 AND j.activityID = 1
			 GROUP BY j.outputTypeID
			 ORDER BY COUNT(*) DESC, j.outputTypeID LIMIT 12`
	} else {
		q = `
			SELECT COALESCE(e.typeID, 0), COALESCE(MIN(bpt.typeName), 'unknown blueprint'), j.activityID,
			       COUNT(*), COALESCE(SUM(j.runs), 0), COUNT(DISTINCT j.installerID)
			  FROM ramJobs j
			  LEFT JOIN entity e     ON e.itemID = j.installedItemID
			  LEFT JOIN invTypes bpt ON bpt.typeID = e.typeID
			 WHERE j.completedStatusID < 100 AND j.activityID <> 1
			 GROUP BY COALESCE(e.typeID, 0), j.activityID
			 ORDER BY COUNT(*) DESC, COALESCE(e.typeID, 0) LIMIT 12`
	}
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ESPTopItem
	for rows.Next() {
		var it ESPTopItem
		if err := rows.Scan(&it.TypeID, &it.Name, &it.ActivityID, &it.Jobs, &it.Runs, &it.Installers); err != nil {
			return nil, err
		}
		it.ActivityShort = ESPActivityShortName(it.ActivityID)
		out = append(out, it)
	}
	return out, rows.Err()
}

func espTopInstallers(ctx context.Context, db *sql.DB, limit int) ([]ESPTopInstaller, error) {
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`
		SELECT j.installerID, COALESCE(MIN(c.characterName), CONCAT('owner ', j.installerID)),
		       COALESCE(MIN(corp.corporationName), ''),
		       COUNT(*), COALESCE(SUM(j.runs), 0), COUNT(DISTINCT j.assemblyLineID),
		       COALESCE(SUM(j.activityID = 1), 0), COALESCE(SUM(j.activityID <> 1), 0),
		       COALESCE(SUM(COALESCE(j.ownerID, 0) NOT IN (0, j.installerID)), 0)
		  FROM ramJobs j
		  LEFT JOIN character_ c   ON c.characterID = j.installerID
		  LEFT JOIN corporation corp ON corp.corporationID = c.corporationID
		 WHERE j.completedStatusID < 100
		 GROUP BY j.installerID
		 ORDER BY COUNT(*) DESC, j.installerID LIMIT %d`, limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ESPTopInstaller
	for rows.Next() {
		var t ESPTopInstaller
		if err := rows.Scan(&t.CharID, &t.Name, &t.Corp, &t.Jobs, &t.Runs, &t.Lines,
			&t.Mfg, &t.Sci, &t.CorpJobs); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func espTopCorps(ctx context.Context, db *sql.DB, limit int) ([]ESPTopCorp, error) {
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`
		SELECT COALESCE(c.corporationID, 0), COALESCE(MIN(corp.corporationName), 'unaffiliated'),
		       COUNT(*), COALESCE(SUM(j.runs), 0), COUNT(DISTINCT j.installerID),
		       COALESCE(SUM(COALESCE(j.ownerID, 0) NOT IN (0, j.installerID)), 0)
		  FROM ramJobs j
		  LEFT JOIN character_ c     ON c.characterID = j.installerID
		  LEFT JOIN corporation corp ON corp.corporationID = c.corporationID
		 WHERE j.completedStatusID < 100
		 GROUP BY COALESCE(c.corporationID, 0)
		 ORDER BY COUNT(*) DESC, COALESCE(c.corporationID, 0) LIMIT %d`, limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ESPTopCorp
	for rows.Next() {
		var t ESPTopCorp
		if err := rows.Scan(&t.CorpID, &t.Name, &t.Jobs, &t.Runs, &t.Installers, &t.CorpJobs); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// espHotRegions ranks regions by lines in use. Region comes from the station,
// so POS arrays are not in any region and do not appear here.
func espHotRegions(ctx context.Context, db *sql.DB, limit int) ([]ESPHotRegion, error) {
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`
		SELECT COALESCE(st.regionID, 0), COALESCE(MIN(reg.regionName), ''),
		       COUNT(DISTINCT j.assemblyLineID), COUNT(*)
		  FROM ramJobs j
		  JOIN staStations st      ON st.stationID = j.containerID
		  LEFT JOIN mapRegions reg ON reg.regionID = st.regionID
		 WHERE j.completedStatusID < 100
		 GROUP BY st.regionID
		 ORDER BY COUNT(DISTINCT j.assemblyLineID) DESC, st.regionID LIMIT %d`, limit))
	if err != nil {
		return nil, err
	}
	var out []ESPHotRegion
	ids := make([]int64, 0, limit)
	for rows.Next() {
		var r ESPHotRegion
		if err := rows.Scan(&r.RegionID, &r.Name, &r.LinesBusy, &r.Jobs); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, r)
		ids = append(ids, r.RegionID)
	}
	rows.Close()
	if len(out) == 0 {
		return out, nil
	}
	lines, err := espCountBy(ctx, db, `
		SELECT st.regionID, COUNT(*) FROM ramassemblylines al
		  JOIN staStations st ON st.stationID = al.containerID
		 WHERE st.regionID IN (`+idList(ids)+`) GROUP BY st.regionID`)
	if err != nil {
		return out, err
	}
	for i := range out {
		out[i].LinesTotal = lines[out[i].RegionID]
	}
	return out, nil
}

// espHotStations ranks stations (and POS arrays) by lines in use.
func espHotStations(ctx context.Context, db *sql.DB, limit int) ([]ESPHotStation, error) {
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`
		SELECT j.containerID, COALESCE(MIN(st.stationName), ''), COALESCE(MIN(sys.solarSystemName), ''),
		       COALESCE(MIN(reg.regionName), ''), COALESCE(MIN(sys.security), 0),
		       COUNT(DISTINCT j.assemblyLineID), COUNT(*)
		  FROM ramJobs j
		  LEFT JOIN staStations st      ON st.stationID = j.containerID
		  LEFT JOIN mapSolarSystems sys ON sys.solarSystemID = st.solarSystemID
		  LEFT JOIN mapRegions reg      ON reg.regionID = st.regionID
		 WHERE j.completedStatusID < 100
		 GROUP BY j.containerID
		 ORDER BY COUNT(DISTINCT j.assemblyLineID) DESC, j.containerID LIMIT %d`, limit))
	if err != nil {
		return nil, err
	}
	var out []ESPHotStation
	ids := make([]int64, 0, limit)
	for rows.Next() {
		var s ESPHotStation
		if err := rows.Scan(&s.StationID, &s.Name, &s.System, &s.Region, &s.Security,
			&s.LinesBusy, &s.Jobs); err != nil {
			rows.Close()
			return nil, err
		}
		s.Known = s.Name != ""
		if !s.Known {
			s.Name = fmt.Sprintf("POS array / container %d", s.StationID)
		}
		out = append(out, s)
		ids = append(ids, s.StationID)
	}
	rows.Close()
	if len(out) == 0 {
		return out, nil
	}
	lines, err := espCountBy(ctx, db, `
		SELECT containerID, COUNT(*) FROM ramassemblylines
		 WHERE containerID IN (`+idList(ids)+`) GROUP BY containerID`)
	if err != nil {
		return out, err
	}
	for i := range out {
		out[i].LinesTotal = lines[out[i].StationID]
	}
	return out, nil
}

// espResearchStats reads the Research Facility tables. They are created by their
// own Ensure function, so a failure here means "no facility yet", not an error
// worth a warning.
func espResearchStats(ctx context.Context, db *sql.DB, now int64) ESPResearchStats {
	var r ESPResearchStats
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*), COUNT(DISTINCT stationID) FROM researchSlotRentals WHERE expiresAt > ?`, now).
		Scan(&r.SlotsRented, &r.RentalStation); err != nil {
		return r
	}
	r.Present = true
	week := now - 7*win32Day
	rows, err := db.QueryContext(ctx, `
		SELECT activityID, COALESCE(SUM(status = 0), 0),
		       COALESCE(SUM(status = 1 AND completedAt >= ?), 0)
		  FROM researchSlotJobs GROUP BY activityID`, week)
	if err != nil {
		return r
	}
	defer rows.Close()
	for rows.Next() {
		var act, running, done int64
		if err := rows.Scan(&act, &running, &done); err != nil {
			return r
		}
		switch act {
		case 6:
			r.DupRunning, r.DupDone7d = running, done
		case 7:
			r.REngRunning, r.REngDone7d = running, done
		}
	}
	return r
}

// ---------------------------------------------------------------------------
// Grouped job roll-up (Manufacturing / Science tabs)

// ESPGroupFilter is a job filter plus how to group it and page through it.
type ESPGroupFilter struct {
	ESPJobFilter
	By     string // blueprint (default), station, installer, region
	Sort   string // jobs (default), runs, soonest, name
	Offset int
	Limit  int
}

// ESPJobGroup is one roll-up row.
type ESPJobGroup struct {
	By       string
	ID       int64 // blueprint typeID, stationID, charID or regionID
	Activity int64 // blueprint groups only
	Label    string
	Sub      string

	Jobs, Runs int64
	Ready      int64
	Installers int64
	Stations   int64

	FirstEndUnix, LastEndUnix int64
	AvgProgress               float64
	ProgressPct               int64
	NextInSec                 int64 // soonest finish from now, negative once past
}

// Running is the jobs not yet ready.
func (g ESPJobGroup) Running() int64 { return g.Jobs - g.Ready }

// ActivityShort names the activity of a blueprint group; other groupings span
// activities and have none.
func (g ESPJobGroup) ActivityShort() string {
	if g.By != "blueprint" {
		return ""
	}
	return ESPActivityShortName(g.Activity)
}

// Key is the group's identity as it travels in a URL.
func (g ESPJobGroup) Key() string { return fmt.Sprintf("%d:%d", g.ID, g.Activity) }

// ESPGroupPage is one page of groups and the totals behind it.
type ESPGroupPage struct {
	Groups      []ESPJobGroup
	TotalGroups int64
	TotalJobs   int64
}

type espGroupDef struct {
	by       string // normalised name
	groupBy  string
	id       string
	activity string
	label    string
	sub      string
}

// espGroupDefs is the whitelist of ways to group. The values are spliced into
// SQL, so nothing from a request ever reaches them: the request names a key and
// espGroupDef resolves it.
var espGroupDefs = map[string]espGroupDef{
	"blueprint": {
		by: "blueprint", groupBy: "COALESCE(e.typeID, 0), j.activityID",
		id: "COALESCE(e.typeID, 0)", activity: "j.activityID",
		label: "COALESCE(MIN(bpt.typeName), 'unknown blueprint')",
		// A manufacturing group names its product; every research and copy
		// activity writes the blueprint back as the output, which adds nothing.
		sub: "COALESCE(MIN(CASE WHEN j.activityID = 1 THEN ot.typeName END), '')",
	},
	"station": {
		by: "station", groupBy: "j.containerID",
		id: "j.containerID", activity: "0",
		label: "COALESCE(MIN(st.stationName), CONCAT('POS array / container ', j.containerID))",
		sub:   "COALESCE(MIN(sys.solarSystemName), '')",
	},
	"installer": {
		by: "installer", groupBy: "j.installerID",
		id: "j.installerID", activity: "0",
		label: "COALESCE(MIN(c.characterName), CONCAT('owner ', j.installerID))",
		sub:   "''",
	},
	"region": {
		by: "region", groupBy: "COALESCE(st.regionID, 0)",
		id: "COALESCE(st.regionID, 0)", activity: "0",
		label: "COALESCE(MIN(reg.regionName), 'No region (POS array)')",
		sub:   "''",
	},
}

// espGroupSorts is the whitelist of orderings, for the same reason.
var espGroupSorts = map[string]string{
	"jobs":    "COUNT(*) DESC, glabel",
	"runs":    "SUM(j.runs) DESC, glabel",
	"soonest": "MIN(j.endProductionTime) ASC, glabel",
	"name":    "glabel ASC",
}

// ESPGroupBy resolves a request's group key, defaulting to blueprint.
func ESPGroupBy(by string) string {
	if d, ok := espGroupDefs[by]; ok {
		return d.by
	}
	return "blueprint"
}

// ESPGroupSort resolves a request's sort key, defaulting to jobs.
func ESPGroupSort(s string) string {
	if _, ok := espGroupSorts[s]; ok {
		return s
	}
	return "jobs"
}

// ESPJobGroups rolls the filtered jobs up into groups.
func ESPJobGroups(ctx context.Context, db *sql.DB, f ESPGroupFilter) (*ESPGroupPage, error) {
	def := espGroupDefs[ESPGroupBy(f.By)]
	order := espGroupSorts[ESPGroupSort(f.Sort)]
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	where, args := espJobWhere(f.ESPJobFilter)
	whereSQL := ""
	if len(where) > 0 {
		whereSQL = "\n WHERE " + strings.Join(where, " AND ")
	}
	now := Win32Now()

	page := &ESPGroupPage{}
	countQ := `SELECT COUNT(*), COALESCE(SUM(n), 0) FROM (SELECT COUNT(*) AS n ` + espJobFrom +
		whereSQL + "\n GROUP BY " + def.groupBy + ") g"
	if err := db.QueryRowContext(ctx, countQ, args...).Scan(&page.TotalGroups, &page.TotalJobs); err != nil {
		return nil, fmt.Errorf("ESPJobGroups count: %w", err)
	}

	// Ready counts as fully progressed; a running job is how far its own span has
	// run, clamped. Win32 units cancel, so now is used raw.
	progress := fmt.Sprintf(`CASE WHEN j.completedStatusID >= 1 THEN 1
		WHEN j.endProductionTime > j.beginProductionTime AND j.beginProductionTime > 0
		  THEN LEAST(1, GREATEST(0, (%d - j.beginProductionTime) / (j.endProductionTime - j.beginProductionTime)))
		ELSE 0 END`, now)

	q := fmt.Sprintf(`
		SELECT %s, %s, %s AS glabel, %s,
		       COUNT(*), COALESCE(SUM(j.runs), 0), COALESCE(SUM(j.completedStatusID = 1), 0),
		       COUNT(DISTINCT j.installerID), COUNT(DISTINCT j.containerID),
		       MIN(j.endProductionTime), MAX(j.endProductionTime), AVG(%s)
		%s%s
		 GROUP BY %s
		 ORDER BY %s
		 LIMIT %d OFFSET %d`,
		def.id, def.activity, def.label, def.sub, progress,
		espJobFrom, whereSQL, def.groupBy, order, limit, f.Offset)
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("ESPJobGroups: %w", err)
	}
	defer rows.Close()
	nowUnix := time.Now().Unix()
	for rows.Next() {
		g := ESPJobGroup{By: def.by}
		var firstFT, lastFT int64
		if err := rows.Scan(&g.ID, &g.Activity, &g.Label, &g.Sub,
			&g.Jobs, &g.Runs, &g.Ready, &g.Installers, &g.Stations,
			&firstFT, &lastFT, &g.AvgProgress); err != nil {
			return nil, fmt.Errorf("ESPJobGroups scan: %w", err)
		}
		g.FirstEndUnix = Win32ToUnix(firstFT)
		g.LastEndUnix = Win32ToUnix(lastFT)
		if g.FirstEndUnix > 0 {
			g.NextInSec = g.FirstEndUnix - nowUnix
		}
		g.ProgressPct = int64(g.AvgProgress*100 + 0.5)
		page.Groups = append(page.Groups, g)
	}
	return page, rows.Err()
}

// ESPJobsInGroup lists a group's own jobs, soonest-finishing first, with the
// same base filter that built the group.
func ESPJobsInGroup(ctx context.Context, db *sql.DB, base ESPJobFilter, by string, id, activity int64, limit int) ([]ESPJobRow, error) {
	f := base
	switch ESPGroupBy(by) {
	case "blueprint":
		f.BlueprintTypeID = id
		f.ActivityID = activity
	case "station":
		f.StationID = id
	case "installer":
		f.CharID = id
	case "region":
		if id == 0 {
			f.RegionNone = true
		} else {
			f.RegionID = id
		}
	}
	if limit <= 0 {
		limit = 200
	}
	f.Limit = limit
	return ESPListJobs(ctx, db, f)
}

// ESPRushGroup rushes every running job in a group, returning how many it moved.
// It is ESPRushJob in a loop: nothing is delivered, the installers collect in-game.
func ESPRushGroup(ctx context.Context, db *sql.DB, base ESPJobFilter, by string, id, activity int64) (int, error) {
	base.Status = "running"
	jobs, err := ESPJobsInGroup(ctx, db, base, by, id, activity, 5000)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, j := range jobs {
		if err := ESPRushJob(ctx, db, j.JobID); err == nil {
			n++
		}
	}
	return n, nil
}
