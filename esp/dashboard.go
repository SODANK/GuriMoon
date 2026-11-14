package esp

// Dashboard: the Overview tab's memoised figures and live vitals, and the Players tab's
// connection table. The database half is evedb.ESPDashboardOverviewLoad.
// See docs/station-admin.md#dashboard.

import (
	"context"
	"runtime"
	"sort"
	"time"

	evedb "gurimoon/db"
)

// dashOverviewTTL is how long the overview is served from memory. It is a dozen aggregate
// queries, so an open tab must not repeat them; "Refresh" bypasses it.
const dashOverviewTTL = 30 * time.Second

// dashOverviewBudget is how long one overview build may run. The build ignores the browser
// closing or reloading, because its result is shared with every viewer.
const dashOverviewBudget = 25 * time.Second

// NPCCount is set by the game server and reports how many NPCs are spawned. nil when no
// game server is attached.
var NPCCount func() int

// dashVitals is what the running process knows without a query. Read on every request.
type dashVitals struct {
	Uptime                             time.Duration
	Online, Docked, InSpace, Selecting int
	LoadedSystems, NPCsInSpace         int
	Goroutines                         int
	HeapMB                             float64
	DBOpen, DBInUse                    int
	DBWaits                            int64
	GoVersion                          string
	HasServer                          bool // a game server set the live-state hooks
}

// dashSitesLive is the in-memory half of the open-site panel.
type dashSitesLive struct {
	Loaded, LiveNPCs int
	Known            bool // OpenSiteLiveState is set
}

// dashOverview returns the memoised overview, rebuilding it when stale or when the caller
// asked for a fresh one. Without a database it is empty with one warning. The build runs on
// a context detached from the request, so a closed tab cannot fail panels for other viewers.
// A build that ran out of time is returned but not memoised, so the next request retries.
func (s *Server) dashOverview(ctx context.Context, force bool) *evedb.ESPDashboardOverview {
	if s.db == nil {
		return &evedb.ESPDashboardOverview{AsOfUnix: time.Now().Unix(), Warnings: []string{"no database attached"}}
	}
	s.dashMu.Lock()
	defer s.dashMu.Unlock()
	if !force && s.dashMemo != nil && time.Since(s.dashAt) < dashOverviewTTL {
		return s.dashMemo
	}
	bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dashOverviewBudget)
	defer cancel()
	o := evedb.ESPDashboardOverviewLoad(bctx, s.db)
	if bctx.Err() == nil {
		s.dashMemo, s.dashAt = o, time.Now()
	} else {
		s.dashMemo = nil
	}
	return o
}

// dashVitals reads the session registry, the live-state hooks and the runtime.
func (s *Server) dashVitals() dashVitals {
	v := dashVitals{
		Uptime:     time.Since(s.startTime),
		Goroutines: runtime.NumGoroutine(),
		GoVersion:  runtime.Version(),
		HasServer:  LoadedSystemCount != nil,
	}
	for _, sess := range s.sessions.Snapshot() {
		v.Online++
		switch {
		case sess.CharID == 0:
			v.Selecting++
		case sess.StationID != 0:
			v.Docked++
		default:
			v.InSpace++
		}
	}
	if LoadedSystemCount != nil {
		v.LoadedSystems = LoadedSystemCount()
	}
	if NPCCount != nil {
		v.NPCsInSpace = NPCCount()
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	v.HeapMB = float64(ms.HeapAlloc) / (1024 * 1024)
	if s.db != nil {
		st := s.db.Stats()
		v.DBOpen, v.DBInUse, v.DBWaits = st.OpenConnections, st.InUse, st.WaitCount
	}
	return v
}

// dashSitesLive folds the live open-site map into two counts.
func (s *Server) dashSitesLive() dashSitesLive {
	if OpenSiteLiveState == nil {
		return dashSitesLive{}
	}
	out := dashSitesLive{Known: true}
	for _, l := range OpenSiteLiveState() {
		if l.Loaded {
			out.Loaded++
		}
		out.LiveNPCs += l.LiveNPCs
	}
	return out
}

// dashboardPlayers resolves every session's station, system and corporation names and splits
// the count into docked, in space and selecting. Sorted by connect time, because Snapshot walks
// a map.
func (s *Server) dashboardPlayers(ctx context.Context) (rows []DashboardSession, docked, inSpace, selecting int) {
	sessions := s.sessions.Snapshot()
	sort.Slice(sessions, func(i, j int) bool {
		if sessions[i].ConnectedAt.Equal(sessions[j].ConnectedAt) {
			return sessions[i].Addr < sessions[j].Addr
		}
		return sessions[i].ConnectedAt.Before(sessions[j].ConnectedAt)
	})

	stationIDs := make([]int64, 0, len(sessions))
	systemIDs := make([]int64, 0, len(sessions))
	corpIDs := make([]int64, 0, len(sessions))
	for _, sess := range sessions {
		stationIDs = append(stationIDs, sess.StationID)
		systemIDs = append(systemIDs, sess.SolarSystemID)
		corpIDs = append(corpIDs, sess.CorpID)
	}
	var stationNames, systemNames, corpNames map[int64]string
	if s.db != nil {
		stationNames = evedb.ESPGetStationNames(ctx, s.db, stationIDs)
		systemNames = evedb.ESPGetSystemNames(ctx, s.db, systemIDs)
		corpNames = evedb.ESPGetCorpNames(ctx, s.db, corpIDs)
	}

	rows = make([]DashboardSession, 0, len(sessions))
	for _, sess := range sessions {
		switch {
		case sess.CharID == 0:
			selecting++
		case sess.StationID != 0:
			docked++
		default:
			inSpace++
		}
		rows = append(rows, DashboardSession{
			Session:     sess,
			StationName: stationNames[sess.StationID],
			SystemName:  systemNames[sess.SolarSystemID],
			CorpName:    corpNames[sess.CorpID],
		})
	}
	return rows, docked, inSpace, selecting
}
