package destiny

import (
	"math/rand"
	"sync"
	"time"
)

// Docking clearance: one registry for every ship asking a station to let it in,
// players and rolebot projections alike. A request gets a grant time; the dock
// completes once the ship is inside RangeM and past it. See docs/destiny-arrival-lanes.md#docking-clearance.

// DockClearanceConfig carries the server_settings levers (docking.*).
type DockClearanceConfig struct {
	MinDelay time.Duration // shortest request → grant
	MaxDelay time.Duration // longest request → grant
	Spacing  time.Duration // minimum gap between two grants at one station
	RangeM   float64       // surface distance a ship must be inside to dock
	WarpInM  float64       // warp-to-dock landing, metres off the surface
	Timeout  time.Duration // a request not completed by then lapses
}

// DefaultDockClearanceConfig is what a server with no docking.* rows runs.
func DefaultDockClearanceConfig() DockClearanceConfig {
	return DockClearanceConfig{
		MinDelay: 4 * time.Second,
		MaxDelay: 6 * time.Second,
		Spacing:  1500 * time.Millisecond,
		RangeM:   2500,
		WarpInM:  15000,
		Timeout:  300 * time.Second, // a battleship burns 12.5 km in ~2 min
	}
}

type dockRequest struct {
	stationID   int64
	requestedAt time.Time
	grantAt     time.Time
}

type dockClearanceRegistry struct {
	mu        sync.Mutex
	cfg       DockClearanceConfig
	byShip    map[int64]dockRequest
	lastGrant map[int64]time.Time // stationID → latest grant handed out
	rnd       func() float64      // [0,1); injectable for tests
}

func newDockClearanceRegistry() *dockClearanceRegistry {
	return &dockClearanceRegistry{
		cfg:       DefaultDockClearanceConfig(),
		byShip:    make(map[int64]dockRequest),
		lastGrant: make(map[int64]time.Time),
		rnd:       rand.Float64,
	}
}

var dockClearance = newDockClearanceRegistry()

// SetDockClearanceConfig installs new settings. Non-positive fields keep the
// default, and MaxDelay is raised to MinDelay if it was set below it.
func SetDockClearanceConfig(cfg DockClearanceConfig) {
	def := DefaultDockClearanceConfig()
	if cfg.MinDelay < 0 {
		cfg.MinDelay = def.MinDelay
	}
	if cfg.MaxDelay < cfg.MinDelay {
		cfg.MaxDelay = cfg.MinDelay
	}
	if cfg.Spacing < 0 {
		cfg.Spacing = def.Spacing
	}
	if cfg.RangeM <= 0 {
		cfg.RangeM = def.RangeM
	}
	if cfg.WarpInM <= 0 {
		cfg.WarpInM = def.WarpInM
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = def.Timeout
	}
	dockClearance.mu.Lock()
	dockClearance.cfg = cfg
	dockClearance.mu.Unlock()
}

// DockClearanceSettings returns the settings in force.
func DockClearanceSettings() DockClearanceConfig {
	dockClearance.mu.Lock()
	defer dockClearance.mu.Unlock()
	return dockClearance.cfg
}

// RequestDockClearance files shipID's request to dock at stationID and returns
// when clearance is granted. A ship that already holds a request for the same
// station gets the same grant back; one holding a request elsewhere has it
// replaced.
func RequestDockClearance(stationID, shipID int64, now time.Time) time.Time {
	return dockClearance.request(stationID, shipID, now)
}

// DockClearanceRequest reports the request shipID holds, if any.
func DockClearanceRequest(shipID int64) (stationID int64, requestedAt, grantAt time.Time, ok bool) {
	dockClearance.mu.Lock()
	defer dockClearance.mu.Unlock()
	r, ok := dockClearance.byShip[shipID]
	return r.stationID, r.requestedAt, r.grantAt, ok
}

// ReleaseDockClearance drops shipID's request: it docked, or it gave up.
func ReleaseDockClearance(shipID int64) {
	dockClearance.mu.Lock()
	delete(dockClearance.byShip, shipID)
	dockClearance.mu.Unlock()
}

func (g *dockClearanceRegistry) request(stationID, shipID int64, now time.Time) time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sweepLocked(now)
	if r, ok := g.byShip[shipID]; ok && r.stationID == stationID {
		return r.grantAt
	}
	delay := g.cfg.MinDelay
	if span := g.cfg.MaxDelay - g.cfg.MinDelay; span > 0 {
		delay += time.Duration(g.rnd() * float64(span))
	}
	grant := now.Add(delay)
	if last, ok := g.lastGrant[stationID]; ok {
		if next := last.Add(g.cfg.Spacing); next.After(grant) {
			grant = next
		}
	}
	g.lastGrant[stationID] = grant
	g.byShip[shipID] = dockRequest{stationID: stationID, requestedAt: now, grantAt: grant}
	return grant
}

// sweepLocked drops requests past Timeout and station queues whose last grant
// is long gone (it can no longer delay anybody).
func (g *dockClearanceRegistry) sweepLocked(now time.Time) {
	for ship, r := range g.byShip {
		if now.Sub(r.requestedAt) > g.cfg.Timeout {
			delete(g.byShip, ship)
		}
	}
	for st, last := range g.lastGrant {
		if now.Sub(last) > g.cfg.Spacing+time.Minute {
			delete(g.lastGrant, st)
		}
	}
}
