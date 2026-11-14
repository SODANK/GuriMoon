package db

// ESP Factories queries: the live view of industry. Two tables carry the state.
//
//	ramassemblylines  one static row per line, keyed to a station by containerID. A line is busy
//	                  only by inference: an undelivered ramJobs row points at it.
//	ramJobs           the work. completedStatusID is the whole state machine: 0 running, 1 ready
//	                  to deliver, 101 delivered, 102 aborted.
//
// Times are Win32 FILETIMEs (100-ns intervals since 1601-01-01), as the client reads them off the
// wire; everything user-facing here converts.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// win32EpochOffset is the seconds between the FILETIME epoch (1601-01-01) and
// the Unix epoch.
const win32EpochOffset = 11644473600

// Win32ToUnix converts a Win32 FILETIME to Unix seconds (0 stays 0).
func Win32ToUnix(ft int64) int64 {
	if ft <= 0 {
		return 0
	}
	return ft/10000000 - win32EpochOffset
}

// UnixToWin32 converts Unix seconds to a Win32 FILETIME.
func UnixToWin32(sec int64) int64 {
	return (sec + win32EpochOffset) * 10000000
}

// Win32Now is the current time as a Win32 FILETIME.
func Win32Now() int64 { return UnixToWin32(time.Now().Unix()) }

// Job status IDs as stored in ramJobs.completedStatusID.
const (
	ESPJobRunning   = 0
	ESPJobReady     = 1
	ESPJobDelivered = 101
	ESPJobAborted   = 102
)

// ESPFactoryStats is the headline row on the factories page.
type ESPFactoryStats struct {
	Lines        int64
	Stations     int64
	BusyLines    int64
	Running      int64
	Ready        int64
	Delivered    int64
	Aborted      int64
	Overdue      int64 // past their end time but still flagged running
	Installers   int64
	Orphaned     int64 // jobs whose assembly line no longer exists
	NoStation    int64 // line containers that are neither a station nor a POS structure
	PosArrays    int64 // line containers that are POS arrays (no staStations row, a starbaseStructures one)
	NextDoneUnix int64

	// The same running/ready split per half of the page, so each listing can
	// state its own load without the reader subtracting one headline from
	// another.
	MfgRunning int64
	MfgReady   int64
	SciRunning int64
	SciReady   int64
}

// MfgActive and SciActive are the per-listing job counts.
func (s ESPFactoryStats) MfgActive() int64 { return s.MfgRunning + s.MfgReady }

// SciActive counts research, copy and invention jobs still holding a line.
func (s ESPFactoryStats) SciActive() int64 { return s.SciRunning + s.SciReady }

// Active is every job still holding an assembly line.
func (s ESPFactoryStats) Active() int64 { return s.Running + s.Ready }

// UtilPct is the share of assembly lines carrying an undelivered job, to two decimals: a shard
// runs tens of jobs against a hundred thousand shipped lines, so whole percents read as zero.
func (s ESPFactoryStats) UtilPct() float64 {
	if s.Lines <= 0 {
		return 0
	}
	return float64(s.BusyLines) * 100 / float64(s.Lines)
}

// BarPct is UtilPct widened for the meter: a utilisation under a tenth of a percent still draws
// a visible sliver whenever any line is in use.
func (s ESPFactoryStats) BarPct() float64 {
	p := s.UtilPct()
	if s.BusyLines > 0 && p < 1.5 {
		return 1.5
	}
	return p
}

// ESPJobRow is one job on the factories page.
type ESPJobRow struct {
	JobID          int64
	ActivityID     int64
	ActivityName   string
	ActivityShort  string
	StatusID       int64
	Status         string
	AssemblyLineID int64
	LineTypeID     int64
	LineTypeName   string

	InstallerID   int64
	InstallerName string

	InstalledItemID int64
	BlueprintTypeID int64
	BlueprintName   string
	MaterialLevel   int64
	ProductivityLev int64
	BlueprintFlag   int64

	OutputTypeID int64
	OutputName   string
	Runs         int64
	LicensedRuns int64

	StationID   int64
	StationName string
	SystemID    int64
	SystemName  string
	RegionName  string

	InstallTimeUnix int64
	BeginTimeUnix   int64
	EndTimeUnix     int64

	// Derived
	RemainingSec int64   // negative once overdue
	TotalSec     int64   // begin → end
	Progress     float64 // 0..1, clamped
	ProgressPct  int64   // Progress as whole percent, for the bar width
}

// Overdue reports a job the tick has not yet flipped to ready, because the server was down when
// it finished or the job ticker is not running.
func (j ESPJobRow) Overdue() bool {
	return j.StatusID == ESPJobRunning && j.EndTimeUnix > 0 && j.RemainingSec <= 0
}

// Active reports a job still holding its assembly line and its blueprint.
func (j ESPJobRow) Active() bool { return j.StatusID < 100 }

// Science reports a job that is not building an item (research, copying, invention). The
// factories page lists these separately from manufacturing.
func (j ESPJobRow) Science() bool { return j.ActivityID != ESPActivityManufacturing }

// DistinctOutput reports a job whose output differs from the installed blueprint. Research and
// copy activities write the blueprint's own typeID back as outputTypeID.
func (j ESPJobRow) DistinctOutput() bool {
	return j.OutputName != "" && j.OutputName != j.BlueprintName
}

// ShowLevels reports a job whose ME/PE numbers carry information: on a research job they are the
// levels being worked on and matter at 0; elsewhere a zero pair is noise.
func (j ESPJobRow) ShowLevels() bool {
	return j.ActivityID == 3 || j.ActivityID == 4 ||
		j.MaterialLevel != 0 || j.ProductivityLev != 0
}

// ESPJobFilter narrows the job list.
type ESPJobFilter struct {
	Status string // "active" (default), "running", "ready", "done", "aborted", "all"
	// Class splits the list as the page does: "mfg" is activity 1, "science" is everything else.
	// Testing against the one manufacturing ID keeps an unknown activityID in exactly one listing.
	Class      string
	ActivityID int64
	StationID  int64
	RegionID   int64
	CharID     int64
	Search     string // blueprint, product, installer or station name
	Limit      int

	// BlueprintTypeID narrows to one installed blueprint type, and RegionNone to jobs whose container
	// has no region (a POS array or a missing station), so a grouped row on the Science & Industry
	// page opens its jobs with the filter that built the group.
	BlueprintTypeID int64
	RegionNone      bool
}

// espJobFrom is the join set every job listing shares. The group roll-ups on the
// Science & Industry page read the same aliases, so a filter written against
// them works in both.
const espJobFrom = `
		  FROM ramJobs j
		  LEFT JOIN ramassemblylines al ON al.assemblyLineID = j.assemblyLineID
		  LEFT JOIN ramaltypes alt      ON alt.assemblyLineTypeID = al.assemblyLineTypeID
		  LEFT JOIN character_ c        ON c.characterID = j.installerID
		  LEFT JOIN entity e            ON e.itemID = j.installedItemID
		  LEFT JOIN invTypes bpt        ON bpt.typeID = e.typeID
		  LEFT JOIN invBlueprints b     ON b.blueprintID = j.installedItemID
		  LEFT JOIN invTypes ot         ON ot.typeID = j.outputTypeID
		  LEFT JOIN staStations st      ON st.stationID = j.containerID
		  LEFT JOIN mapSolarSystems sys ON sys.solarSystemID = j.installedInSolarSystemID
		  LEFT JOIN mapRegions reg      ON reg.regionID = st.regionID`

// espJobWhere turns a filter into WHERE terms and their arguments.
func espJobWhere(f ESPJobFilter) ([]string, []interface{}) {
	var where []string
	var args []interface{}

	switch f.Status {
	case "", "active":
		where = append(where, "j.completedStatusID < 100")
	case "running":
		where = append(where, "j.completedStatusID = 0")
	case "ready":
		where = append(where, "j.completedStatusID = 1")
	case "done":
		where = append(where, "j.completedStatusID = 101")
	case "aborted":
		where = append(where, "j.completedStatusID = 102")
	case "all":
		// no filter
	default:
		where = append(where, "j.completedStatusID < 100")
	}
	switch f.Class {
	case "mfg":
		where = append(where, "j.activityID = ?")
		args = append(args, int64(ESPActivityManufacturing))
	case "science":
		where = append(where, "j.activityID <> ?")
		args = append(args, int64(ESPActivityManufacturing))
	}
	if f.ActivityID != 0 {
		where = append(where, "j.activityID = ?")
		args = append(args, f.ActivityID)
	}
	if f.StationID != 0 {
		where = append(where, "j.containerID = ?")
		args = append(args, f.StationID)
	}
	if f.RegionID != 0 {
		where = append(where, "st.regionID = ?")
		args = append(args, f.RegionID)
	}
	if f.CharID != 0 {
		where = append(where, "j.installerID = ?")
		args = append(args, f.CharID)
	}
	if s := strings.TrimSpace(f.Search); s != "" {
		where = append(where, `(bpt.typeName LIKE ? OR ot.typeName LIKE ? OR c.characterName LIKE ?
		                        OR st.stationName LIKE ? OR CAST(j.jobID AS CHAR) = ?)`)
		like := "%" + s + "%"
		args = append(args, like, like, like, like, s)
	}
	if f.BlueprintTypeID != 0 {
		where = append(where, "e.typeID = ?")
		args = append(args, f.BlueprintTypeID)
	}
	if f.RegionNone {
		where = append(where, "COALESCE(st.regionID, 0) = 0")
	}
	return where, args
}

// ESPListJobs returns jobs matching the filter, soonest-finishing first for
// active jobs and newest-first for finished ones.
func ESPListJobs(ctx context.Context, db *sql.DB, f ESPJobFilter) ([]ESPJobRow, error) {
	where, args := espJobWhere(f)

	limit := f.Limit
	if limit <= 0 {
		limit = 300
	}
	order := "j.endProductionTime ASC"
	if f.Status == "done" || f.Status == "aborted" {
		order = "j.installTime DESC"
	}

	query := `
		SELECT j.jobID, j.activityID, j.completedStatusID,
		       j.assemblyLineID, COALESCE(al.assemblyLineTypeID, 0),
		       COALESCE(alt.assemblyLineTypeName, ''),
		       j.installerID, COALESCE(c.characterName, ''),
		       j.installedItemID, COALESCE(e.typeID, 0), COALESCE(bpt.typeName, ''),
		       COALESCE(b.materialLevel, 0), COALESCE(b.productivityLevel, 0),
		       COALESCE(e.flag, 0),
		       j.outputTypeID, COALESCE(ot.typeName, ''),
		       j.runs, j.licensedProductionRuns,
		       j.containerID, COALESCE(st.stationName, ''),
		       j.installedInSolarSystemID, COALESCE(sys.solarSystemName, ''),
		       COALESCE(reg.regionName, ''),
		       j.installTime, j.beginProductionTime, j.endProductionTime
		` + espJobFrom
	if len(where) > 0 {
		query += "\n WHERE " + strings.Join(where, " AND ")
	}
	query += fmt.Sprintf("\n ORDER BY %s LIMIT %d", order, limit)

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("ESPListJobs: %w", err)
	}
	defer rows.Close()

	now := time.Now().Unix()
	var out []ESPJobRow
	for rows.Next() {
		var j ESPJobRow
		var installFT, beginFT, endFT int64
		if err := rows.Scan(&j.JobID, &j.ActivityID, &j.StatusID,
			&j.AssemblyLineID, &j.LineTypeID, &j.LineTypeName,
			&j.InstallerID, &j.InstallerName,
			&j.InstalledItemID, &j.BlueprintTypeID, &j.BlueprintName,
			&j.MaterialLevel, &j.ProductivityLev, &j.BlueprintFlag,
			&j.OutputTypeID, &j.OutputName, &j.Runs, &j.LicensedRuns,
			&j.StationID, &j.StationName, &j.SystemID, &j.SystemName, &j.RegionName,
			&installFT, &beginFT, &endFT); err != nil {
			return nil, fmt.Errorf("ESPListJobs scan: %w", err)
		}
		j.ActivityName = ESPActivityName[j.ActivityID]
		if j.ActivityName == "" {
			j.ActivityName = fmt.Sprintf("Activity %d", j.ActivityID)
		}
		j.ActivityShort = ESPActivityShortName(j.ActivityID)
		j.Status = espJobStatusName(j.StatusID)
		if j.BlueprintName == "" {
			j.BlueprintName = fmt.Sprintf("item %d", j.InstalledItemID)
		}
		if j.InstallerName == "" {
			j.InstallerName = fmt.Sprintf("owner %d", j.InstallerID)
		}
		j.InstallTimeUnix = Win32ToUnix(installFT)
		j.BeginTimeUnix = Win32ToUnix(beginFT)
		j.EndTimeUnix = Win32ToUnix(endFT)
		if j.EndTimeUnix > 0 {
			j.RemainingSec = j.EndTimeUnix - now
		}
		if j.EndTimeUnix > j.BeginTimeUnix && j.BeginTimeUnix > 0 {
			j.TotalSec = j.EndTimeUnix - j.BeginTimeUnix
			j.Progress = float64(now-j.BeginTimeUnix) / float64(j.TotalSec)
		} else if j.StatusID >= ESPJobReady {
			j.Progress = 1
		}
		if j.Progress < 0 {
			j.Progress = 0
		}
		if j.Progress > 1 {
			j.Progress = 1
		}
		if j.StatusID >= 100 {
			j.Progress = 1
		}
		j.ProgressPct = int64(j.Progress*100 + 0.5)
		out = append(out, j)
	}
	return out, rows.Err()
}

// espJobStatusName maps completedStatusID to a label.
func espJobStatusName(id int64) string {
	switch id {
	case ESPJobRunning:
		return "Running"
	case ESPJobReady:
		return "Ready"
	case ESPJobDelivered:
		return "Delivered"
	case ESPJobAborted:
		return "Aborted"
	default:
		return fmt.Sprintf("Status %d", id)
	}
}

// ESPFactoryStatsRow returns the headline counts.
func ESPFactoryStatsRow(ctx context.Context, db *sql.DB) (ESPFactoryStats, error) {
	var s ESPFactoryStats
	err := db.QueryRowContext(ctx, `
		SELECT COUNT(*), COUNT(DISTINCT containerID) FROM ramassemblylines`).
		Scan(&s.Lines, &s.Stations)
	if err != nil {
		return s, fmt.Errorf("ESPFactoryStatsRow lines: %w", err)
	}
	now := Win32Now()
	_ = db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(completedStatusID = 0), 0),
		       COALESCE(SUM(completedStatusID = 1), 0),
		       COALESCE(SUM(completedStatusID = 101), 0),
		       COALESCE(SUM(completedStatusID = 102), 0),
		       COALESCE(SUM(completedStatusID = 0 AND endProductionTime > 0 AND endProductionTime <= ?), 0),
		       COUNT(DISTINCT CASE WHEN completedStatusID < 100 THEN installerID END),
		       COUNT(DISTINCT CASE WHEN completedStatusID < 100 THEN assemblyLineID END),
		       COALESCE(SUM(completedStatusID = 0 AND activityID = ?), 0),
		       COALESCE(SUM(completedStatusID = 1 AND activityID = ?), 0),
		       COALESCE(SUM(completedStatusID = 0 AND activityID <> ?), 0),
		       COALESCE(SUM(completedStatusID = 1 AND activityID <> ?), 0)
		  FROM ramJobs`, now,
		ESPActivityManufacturing, ESPActivityManufacturing,
		ESPActivityManufacturing, ESPActivityManufacturing).
		Scan(&s.Running, &s.Ready, &s.Delivered, &s.Aborted, &s.Overdue, &s.Installers, &s.BusyLines,
			&s.MfgRunning, &s.MfgReady, &s.SciRunning, &s.SciReady)

	var nextFT sql.NullInt64
	_ = db.QueryRowContext(ctx, `
		SELECT MIN(endProductionTime) FROM ramJobs
		 WHERE completedStatusID = 0 AND endProductionTime > ?`, now).Scan(&nextFT)
	if nextFT.Valid {
		s.NextDoneUnix = Win32ToUnix(nextFT.Int64)
	}

	_ = db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM ramJobs j
		 WHERE j.completedStatusID < 100
		   AND NOT EXISTS (SELECT 1 FROM ramassemblylines al WHERE al.assemblyLineID = j.assemblyLineID)`).
		Scan(&s.Orphaned)

	// Lines on a non-station container: a POS array is legitimate (SyncStructureAssemblyLines);
	// anything else has no station or structure behind it and the client cannot reach it.
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT CASE WHEN st.stationID IS NULL AND sb.entityID IS NOT NULL THEN al.containerID END),
		       COUNT(DISTINCT CASE WHEN st.stationID IS NULL AND sb.entityID IS NULL THEN al.containerID END)
		  FROM ramassemblylines al
		  LEFT JOIN staStations st        ON st.stationID = al.containerID
		  LEFT JOIN starbaseStructures sb ON sb.entityID  = al.containerID`).
		Scan(&s.PosArrays, &s.NoStation); err != nil {
		// No starbase table on this schema: treat every non-station container as dead.
		s.PosArrays = 0
		_ = db.QueryRowContext(ctx, `
			SELECT COUNT(DISTINCT al.containerID) FROM ramassemblylines al
			 WHERE NOT EXISTS (SELECT 1 FROM staStations s WHERE s.stationID = al.containerID)`).
			Scan(&s.NoStation)
	}
	return s, nil
}

// ESPFactoryStation is one station's industry capacity, for the station roll-up.
type ESPFactoryStation struct {
	StationID   int64
	StationName string
	SystemID    int64
	SystemName  string
	Security    float64
	RegionID    int64
	RegionName  string
	CorpID      int64
	CorpName    string
	Known       bool // the containerID resolves to a staStations row
	IsPOS       bool // not a station, but a POS structure's own array

	Lines     int64
	BusyLines int64
	Running   int64
	Ready     int64

	// Line counts per activity, ordered like ESPEditableActivities.
	Manufacturing int64
	TimeResearch  int64
	MatResearch   int64
	Copying       int64
	Invention     int64
	OtherLines    int64
}

// ESPStationFilter narrows the station roll-up.
type ESPStationFilter struct {
	Search   string
	RegionID int64
	BusyOnly bool
	Limit    int
}

// ESPListFactoryStations rolls assembly lines up per station, with the live job counts against
// each. Stations with no lines never appear.
func ESPListFactoryStations(ctx context.Context, db *sql.DB, f ESPStationFilter) ([]ESPFactoryStation, error) {
	var where []string
	var args []interface{}
	if s := strings.TrimSpace(f.Search); s != "" {
		where = append(where, `(st.stationName LIKE ? OR sys.solarSystemName LIKE ? OR CAST(al.containerID AS CHAR) = ?)`)
		like := "%" + s + "%"
		args = append(args, like, like, s)
	}
	if f.RegionID != 0 {
		where = append(where, "st.regionID = ?")
		args = append(args, f.RegionID)
	}

	limit := f.Limit
	if limit <= 0 {
		limit = 400
	}

	query := `
		SELECT COALESCE(al.containerID, 0), COALESCE(st.stationName, ''),
		       COALESCE(st.solarSystemID, 0), COALESCE(sys.solarSystemName, ''),
		       COALESCE(sys.security, 0),
		       COALESCE(st.regionID, 0), COALESCE(reg.regionName, ''),
		       COALESCE(st.corporationID, 0), COALESCE(corp.corporationName, ''),
		       COUNT(*),
		       COALESCE(SUM(COALESCE(al.activityID, 0) = 1), 0),
		       COALESCE(SUM(COALESCE(al.activityID, 0) = 3), 0),
		       COALESCE(SUM(COALESCE(al.activityID, 0) = 4), 0),
		       COALESCE(SUM(COALESCE(al.activityID, 0) = 5), 0),
		       COALESCE(SUM(COALESCE(al.activityID, 0) = 8), 0),
		       COALESCE(SUM(COALESCE(al.activityID, 0) NOT IN (1,3,4,5,8)), 0)
		  FROM ramassemblylines al
		  LEFT JOIN staStations st      ON st.stationID = al.containerID
		  LEFT JOIN mapSolarSystems sys ON sys.solarSystemID = st.solarSystemID
		  LEFT JOIN mapRegions reg      ON reg.regionID = st.regionID
		  LEFT JOIN corporation corp    ON corp.corporationID = st.corporationID`
	if len(where) > 0 {
		query += "\n WHERE " + strings.Join(where, " AND ")
	}
	query += fmt.Sprintf(`
		 GROUP BY al.containerID, st.stationName, st.solarSystemID, sys.solarSystemName,
		          sys.security, st.regionID, reg.regionName, st.corporationID, corp.corporationName
		 ORDER BY st.stationID IS NULL, st.stationName
		 LIMIT %d`, limit)

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("ESPListFactoryStations: %w", err)
	}
	defer rows.Close()

	var out []ESPFactoryStation
	ids := make([]int64, 0, 128)
	for rows.Next() {
		var s ESPFactoryStation
		if err := rows.Scan(&s.StationID, &s.StationName, &s.SystemID, &s.SystemName,
			&s.Security, &s.RegionID, &s.RegionName, &s.CorpID, &s.CorpName,
			&s.Lines, &s.Manufacturing, &s.TimeResearch, &s.MatResearch,
			&s.Copying, &s.Invention, &s.OtherLines); err != nil {
			return nil, fmt.Errorf("ESPListFactoryStations scan: %w", err)
		}
		s.Known = s.StationName != ""
		if !s.Known {
			s.StationName = fmt.Sprintf("container %d", s.StationID)
		}
		out = append(out, s)
		ids = append(ids, s.StationID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}

	running, _ := espCountBy(ctx, db, `
		SELECT containerID, COUNT(*) FROM ramJobs
		 WHERE completedStatusID = 0 AND containerID IN (`+idList(ids)+`)
		 GROUP BY containerID`)
	ready, _ := espCountBy(ctx, db, `
		SELECT containerID, COUNT(*) FROM ramJobs
		 WHERE completedStatusID = 1 AND containerID IN (`+idList(ids)+`)
		 GROUP BY containerID`)
	busy, _ := espCountBy(ctx, db, `
		SELECT containerID, COUNT(DISTINCT assemblyLineID) FROM ramJobs
		 WHERE completedStatusID < 100 AND containerID IN (`+idList(ids)+`)
		 GROUP BY containerID`)

	// Unknown containers that are POS arrays are working capacity; one lookup over the unknown IDs
	// tells them apart.
	pos := map[int64]bool{}
	var unknown []int64
	for _, s := range out {
		if !s.Known {
			unknown = append(unknown, s.StationID)
		}
	}
	if len(unknown) > 0 {
		if m, err := espCountBy(ctx, db, `
			SELECT entityID, COUNT(*) FROM starbaseStructures
			 WHERE entityID IN (`+idList(unknown)+`) GROUP BY entityID`); err == nil {
			for id := range m {
				pos[id] = true
			}
		}
	}

	filtered := out[:0]
	for _, s := range out {
		s.IsPOS = pos[s.StationID]
		s.Running = running[s.StationID]
		s.Ready = ready[s.StationID]
		s.BusyLines = busy[s.StationID]
		if f.BusyOnly && s.BusyLines == 0 {
			continue
		}
		filtered = append(filtered, s)
	}
	return filtered, nil
}

// ESPAssemblyLine is one slot on the station drill-down.
type ESPAssemblyLine struct {
	AssemblyLineID int64
	LineTypeID     int64
	LineTypeName   string
	ActivityID     int64
	ActivityName   string
	OwnerID        int64
	OwnerName      string
	CostInstall    float64
	CostPerHour    float64
	MinimumStand   float64
	RestrictionMsk int64
	TimeMultiplier float64
	MatMultiplier  float64

	// Live state
	JobID        int64
	JobStatus    string
	JobInstaller string
	JobBlueprint string
	JobEndUnix   int64
	QueuedJobs   int64
}

// Busy reports a line currently holding an undelivered job.
func (l ESPAssemblyLine) Busy() bool { return l.JobID != 0 }

// ESPListAssemblyLines returns every line at a station with the job occupying
// it. A line can carry more than one undelivered job (a ready job still sits on
// it until the player collects); the soonest-ending one is shown and the rest
// are counted in QueuedJobs.
func ESPListAssemblyLines(ctx context.Context, db *sql.DB, stationID int64) ([]ESPAssemblyLine, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT al.assemblyLineID, COALESCE(al.assemblyLineTypeID, 0),
		       COALESCE(alt.assemblyLineTypeName, ''),
		       COALESCE(al.activityID, 0),
		       COALESCE(al.ownerID, 0), COALESCE(corp.corporationName, ''),
		       COALESCE(al.costInstall, 0), COALESCE(al.costPerHour, 0),
		       COALESCE(al.minimumStanding, 0), COALESCE(al.restrictionMask, 0),
		       COALESCE(alt.baseTimeMultiplier, 1), COALESCE(alt.baseMaterialMultiplier, 1)
		  FROM ramassemblylines al
		  LEFT JOIN ramaltypes alt   ON alt.assemblyLineTypeID = al.assemblyLineTypeID
		  LEFT JOIN corporation corp ON corp.corporationID = al.ownerID
		 WHERE al.containerID = ?
		 ORDER BY COALESCE(al.activityID, 0), al.assemblyLineID`, stationID)
	if err != nil {
		return nil, fmt.Errorf("ESPListAssemblyLines: %w", err)
	}
	defer rows.Close()

	var out []ESPAssemblyLine
	ids := make([]int64, 0, 64)
	for rows.Next() {
		var l ESPAssemblyLine
		if err := rows.Scan(&l.AssemblyLineID, &l.LineTypeID, &l.LineTypeName,
			&l.ActivityID, &l.OwnerID, &l.OwnerName,
			&l.CostInstall, &l.CostPerHour, &l.MinimumStand, &l.RestrictionMsk,
			&l.TimeMultiplier, &l.MatMultiplier); err != nil {
			return nil, fmt.Errorf("ESPListAssemblyLines scan: %w", err)
		}
		l.ActivityName = ESPActivityName[l.ActivityID]
		if l.ActivityName == "" {
			l.ActivityName = fmt.Sprintf("Activity %d", l.ActivityID)
		}
		if l.LineTypeName == "" {
			l.LineTypeName = fmt.Sprintf("line type %d", l.LineTypeID)
		}
		out = append(out, l)
		ids = append(ids, l.AssemblyLineID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}

	// Occupying job per line: soonest end time wins.
	jrows, err := db.QueryContext(ctx, `
		SELECT j.assemblyLineID, j.jobID, j.completedStatusID, j.endProductionTime,
		       COALESCE(c.characterName, ''), COALESCE(t.typeName, '')
		  FROM ramJobs j
		  LEFT JOIN character_ c ON c.characterID = j.installerID
		  LEFT JOIN entity e     ON e.itemID = j.installedItemID
		  LEFT JOIN invTypes t   ON t.typeID = e.typeID
		 WHERE j.completedStatusID < 100 AND j.assemblyLineID IN (`+idList(ids)+`)
		 ORDER BY j.assemblyLineID, j.endProductionTime`)
	if err == nil {
		defer jrows.Close()
		byLine := map[int64]*ESPAssemblyLine{}
		for i := range out {
			byLine[out[i].AssemblyLineID] = &out[i]
		}
		for jrows.Next() {
			var lineID, jobID, status, endFT int64
			var installer, blueprint string
			if err := jrows.Scan(&lineID, &jobID, &status, &endFT, &installer, &blueprint); err != nil {
				continue
			}
			l := byLine[lineID]
			if l == nil {
				continue
			}
			if l.JobID == 0 {
				l.JobID = jobID
				l.JobStatus = espJobStatusName(status)
				l.JobEndUnix = Win32ToUnix(endFT)
				l.JobInstaller = installer
				l.JobBlueprint = blueprint
			} else {
				l.QueuedJobs++
			}
		}
	}
	return out, nil
}

// ESPFactoryRegion is one region in the factories page region filter.
type ESPFactoryRegion struct {
	RegionID   int64
	RegionName string
	Stations   int64
	Lines      int64
}

// ESPListFactoryRegions lists the regions that hold assembly lines.
func ESPListFactoryRegions(ctx context.Context, db *sql.DB) ([]ESPFactoryRegion, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT COALESCE(st.regionID, 0), COALESCE(reg.regionName, ''),
		       COUNT(DISTINCT al.containerID), COUNT(*)
		  FROM ramassemblylines al
		  JOIN staStations st  ON st.stationID = al.containerID
		  LEFT JOIN mapRegions reg ON reg.regionID = st.regionID
		 GROUP BY st.regionID, reg.regionName
		 ORDER BY reg.regionName`)
	if err != nil {
		return nil, fmt.Errorf("ESPListFactoryRegions: %w", err)
	}
	defer rows.Close()
	var out []ESPFactoryRegion
	for rows.Next() {
		var r ESPFactoryRegion
		if err := rows.Scan(&r.RegionID, &r.RegionName, &r.Stations, &r.Lines); err != nil {
			return nil, fmt.Errorf("ESPListFactoryRegions scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ESPStationHeader is the name row above a station's assembly lines.
type ESPStationHeader struct {
	StationID   int64
	StationName string
	SystemName  string
	RegionName  string
	Security    float64
	CorpName    string
	Known       bool // the ID resolves to a staStations row
}

// ESPGetFactoryStation loads the header for one station.
func ESPGetFactoryStation(ctx context.Context, db *sql.DB, stationID int64) (*ESPStationHeader, error) {
	var h ESPStationHeader
	h.StationID = stationID
	err := db.QueryRowContext(ctx, `
		SELECT COALESCE(st.stationName, ''), COALESCE(sys.solarSystemName, ''),
		       COALESCE(reg.regionName, ''), COALESCE(sys.security, 0),
		       COALESCE(corp.corporationName, '')
		  FROM staStations st
		  LEFT JOIN mapSolarSystems sys ON sys.solarSystemID = st.solarSystemID
		  LEFT JOIN mapRegions reg      ON reg.regionID = st.regionID
		  LEFT JOIN corporation corp    ON corp.corporationID = st.corporationID
		 WHERE st.stationID = ?`, stationID).
		Scan(&h.StationName, &h.SystemName, &h.RegionName, &h.Security, &h.CorpName)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			h.StationName = fmt.Sprintf("container %d", stationID)
			return &h, nil
		}
		return nil, fmt.Errorf("ESPGetFactoryStation: %w", err)
	}
	h.Known = true
	return &h, nil
}

// ESPRushJob pulls a running job's end time forward to now so the next tick flips it to ready.
// It does not deliver: the player collects the output in-game, which pushes the item to a
// connected client. See docs/db-8-notes.md#esp-job-actions.
func ESPRushJob(ctx context.Context, db *sql.DB, jobID int64) error {
	res, err := db.ExecContext(ctx, `
		UPDATE ramJobs SET endProductionTime = ?
		 WHERE jobID = ? AND completedStatusID = 0`, Win32Now(), jobID)
	if err != nil {
		return fmt.Errorf("ESPRushJob: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("job %d is not running", jobID)
	}
	// Flip it now rather than wait for the ticker, so the page shows the result.
	_, _ = db.ExecContext(ctx,
		`UPDATE ramJobs SET completedStatusID = 1 WHERE jobID = ? AND completedStatusID = 0`, jobID)
	return nil
}

// ESPSetJobEndTime moves a running job's end time by a number of seconds (negative pulls it in),
// to nudge a job rather than finish it.
func ESPSetJobEndTime(ctx context.Context, db *sql.DB, jobID, deltaSec int64) error {
	var endFT int64
	err := db.QueryRowContext(ctx,
		`SELECT endProductionTime FROM ramJobs WHERE jobID = ? AND completedStatusID = 0`, jobID).Scan(&endFT)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("job %d is not running", jobID)
		}
		return fmt.Errorf("ESPSetJobEndTime: %w", err)
	}
	newEnd := endFT + deltaSec*10000000
	if now := Win32Now(); newEnd < now {
		newEnd = now
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE ramJobs SET endProductionTime = ? WHERE jobID = ?`, newEnd, jobID); err != nil {
		return fmt.Errorf("ESPSetJobEndTime update: %w", err)
	}
	return nil
}

// ESPCancelJob aborts a job and hands the blueprint back to the installer's hangar. Consumed
// materials are gone, as with an in-game cancel.
func ESPCancelJob(ctx context.Context, db *sql.DB, jobID int64) error {
	var installerID int64
	if err := db.QueryRowContext(ctx,
		`SELECT installerID FROM ramJobs WHERE jobID = ?`, jobID).Scan(&installerID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("job %d not found", jobID)
		}
		return fmt.Errorf("ESPCancelJob load: %w", err)
	}
	if _, err := CompleteJob(ctx, db, jobID, installerID, true); err != nil {
		return err
	}
	return nil
}

// ESPReleaseJob deletes a job row and frees its blueprint back to the hangar flag. It is the
// escape for a job whose assembly line or station no longer exists, which can never be delivered
// or cancelled in-game and leaves its blueprint at flag 111.
func ESPReleaseJob(ctx context.Context, db *sql.DB, jobID int64) error {
	var installedItemID int64
	if err := db.QueryRowContext(ctx,
		`SELECT installedItemID FROM ramJobs WHERE jobID = ?`, jobID).Scan(&installedItemID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("job %d not found", jobID)
		}
		return fmt.Errorf("ESPReleaseJob load: %w", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ESPReleaseJob begin: %w", err)
	}
	defer tx.Rollback()
	if installedItemID != 0 {
		if err := MoveBlueprintToFlag(ctx, tx, installedItemID, 4); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM ramJobs WHERE jobID = ?`, jobID); err != nil {
		return fmt.Errorf("ESPReleaseJob delete: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("ESPReleaseJob commit: %w", err)
	}
	return nil
}

// ESPUpdateAssemblyLine writes one line's cost and standing restrictions.
func ESPUpdateAssemblyLine(ctx context.Context, db *sql.DB, lineID int64, costInstall, costPerHour, minimumStanding float64) error {
	if costInstall < 0 || costPerHour < 0 {
		return fmt.Errorf("costs cannot be negative")
	}
	res, err := db.ExecContext(ctx, `
		UPDATE ramassemblylines
		   SET costInstall = ?, costPerHour = ?, minimumStanding = ?
		 WHERE assemblyLineID = ?`, costInstall, costPerHour, minimumStanding, lineID)
	if err != nil {
		return fmt.Errorf("ESPUpdateAssemblyLine: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("assembly line %d not found", lineID)
	}
	return nil
}

// ESPSetStationLineCosts applies one cost pair to every line at a station,
// optionally only those of one activity.
func ESPSetStationLineCosts(ctx context.Context, db *sql.DB, stationID, activityID int64, costInstall, costPerHour float64) (int64, error) {
	if costInstall < 0 || costPerHour < 0 {
		return 0, fmt.Errorf("costs cannot be negative")
	}
	query := `UPDATE ramassemblylines SET costInstall = ?, costPerHour = ? WHERE containerID = ?`
	args := []interface{}{costInstall, costPerHour, stationID}
	if activityID != 0 {
		query += ` AND COALESCE(activityID, 0) = ?`
		args = append(args, activityID)
	}
	res, err := db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("ESPSetStationLineCosts: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
