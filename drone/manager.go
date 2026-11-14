// Package drone implements player-controlled drones: the registry, the per-drone activity
// state machine (idle/approach/combat/mine/return) and the OnDroneStateChange contract the RMR
// client expects. Wire I/O goes through Callbacks, wired in main.go.
//
// Drones are ordinary destiny entities (IsPlayer=false, categoryID 18), so SetState, bubbles and
// warp arrival deliver their balls. Launch is the caller's job; Register takes a drone already in
// space. A drone has an owner (launcher) and a controller (commanding ship). Commands need the
// controller, RelinquishControl needs the owner, and a controller leaving the grid idles its
// drones and returns control to the owner. ReturnBay flies to the master and auto-scoops there.
package drone

import (
	"log"
	"math"
	"math/rand"
	"sync"
	"time"

	"gurimoon/destiny"
	"gurimoon/safego"
)

// Client activity-state constants (const.entity* in the RMR client).
// drone.py maps these to the status text in the drone window.
const (
	StateIdle        = 0 // "Idle": orbiting the master ship
	StateCombat      = 1 // "Fighting"
	StateMining      = 2 // "Mining"
	StateApproaching = 3 // "Approaching": closing on an engage target
	StateDeparting   = 4 // "Returning to the ship": return and orbit
	StateDeparting2  = 5 // "Returning to the ship": return to drone bay
	StatePursuit     = 6 // "Following"
	StateFleeing     = 7 // "Fleeing"
	StateOperating   = 8 // "Operating"
)

// Movement action for the broadcast callback (mirrors the NPC manager's
// Action shape so main.go can reuse the same destiny action encoding).
type ActionType int

const (
	ActFollow ActionType = iota
	ActOrbit
	ActGoto
	ActStop
)

type Action struct {
	Type     ActionType
	TargetID uint32
	Dist     float64
	Pos      destiny.Vec3
}

// GroupFighterDrone is invGroups groupID 549: carrier fighters, the only drone group that
// follows its controller through warp.
const GroupFighterDrone = 549

// Attributes holds the dogma-derived stats a drone fights and flies with.
type Attributes struct {
	MaxVelocity   float64 // attr 37
	Optimal       float64 // attr 54  maxRange
	Falloff       float64 // attr 158
	TrackingSpeed float64 // attr 160
	DmgEM         float64 // attr 114
	DmgExp        float64 // attr 116
	DmgKin        float64 // attr 117
	DmgTherm      float64 // attr 118
	DmgMult       float64 // attr 64
	CycleMs       int64   // attr 51 (speed)
	MiningAmount  float64 // attr 77 (mining drones)
	OrbitRange    float64 // combat orbit distance

	// Electronic-warfare drones (group 639) deal no damage; they run one of the four targeted
	// EWAR effects off the module attributes a fitted jammer/damp/disruptor/painter uses.
	// EwarKind is the dgmTypeEffects effect name; EwarAttrs is the type's raw attribute map.
	EwarKind  string
	EwarAttrs map[int64]float64
}

// Record is one live drone in space.
type Record struct {
	Entity   *destiny.Entity
	System   *destiny.SolarSystem
	ItemID   int64 // inventory itemID == ball ID
	TypeID   int64
	GroupID  int64
	TypeName string
	OwnerID  int64 // launching character

	mu                sync.Mutex
	ControllerID      int64  // controlling ship itemID
	ControllerOwnerID int64  // controlling character
	MasterID          uint32 // ball ID of the controlling ship
	Activity          int64
	TargetID          uint32 // engage/mine target ball ID
	RepeatMine        bool   // CmdMineRepeatedly
	ReturnBay         bool   // auto-scoop when reaching the master
	Warping           bool   // fighter warp-follow in progress; tick hands off
	Dead              bool
	NextCycle         time.Time // next weapon/mining cycle
	Attrs             Attributes

	lastMove Action // dedupe: last movement command issued
	moveSet  bool

	// notified holds the characters sent a droneState row for this drone, so a later change
	// (control lost, scooped, destroyed) still reaches a pilot who no longer owns or controls it.
	// Guarded by mu. See VisibleTo.
	notified map[int64]bool
}

// VisibleTo reports whether a pilot should receive droneState rows for this drone: they own
// it, control it (by character or ship), or already hold a row the change must update.
// See docs/support-notes.md#drone-state-rows.
func (r *Record) VisibleTo(charID, shipID int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.visibleToLocked(charID, shipID)
}

func (r *Record) visibleToLocked(charID, shipID int64) bool {
	if charID != 0 && (charID == r.OwnerID || charID == r.ControllerOwnerID || r.notified[charID]) {
		return true
	}
	return shipID != 0 && shipID == r.ControllerID
}

// MarkNotified records that a pilot now holds a row for this drone.
func (r *Record) MarkNotified(charID int64) {
	if charID == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.markNotifiedLocked(charID)
}

func (r *Record) markNotifiedLocked(charID int64) {
	if r.notified == nil {
		r.notified = map[int64]bool{}
	}
	r.notified[charID] = true
}

// Snapshot returns the mutable control/state fields under lock.
func (r *Record) Snapshot() (controllerID, controllerOwnerID, activity int64, masterID uint32, dead bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ControllerID, r.ControllerOwnerID, r.Activity, r.MasterID, r.Dead
}

// Target returns the drone's current engage/mine target ball ID under lock.
func (r *Record) Target() uint32 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.TargetID
}

// Callbacks are the wire-level hooks the manager calls; all are set up in
// main.go where the connection/broadcast helpers live.
type Callbacks struct {
	// Move broadcasts a drone movement command to bubble players.
	Move func(rec *Record, act Action)
	// Fire broadcasts a weapon cycle (FX + damage state + combat messages)
	// and routes target destruction (player death / NPC kill / entity remove).
	Fire func(rec *Record, target *destiny.Entity, applied float64, destroyed bool, hitQuality float64)
	// StopFire clears the repeating fire FX against targetID and, for an EWAR drone, lifts the
	// debuff it held on that target.
	StopFire func(rec *Record, targetID uint32)
	// Ewar runs one electronic-warfare cycle for drones whose Attrs carry an
	// EwarKind. Replaces Fire entirely for those types.
	Ewar func(rec *Record, target *destiny.Entity, dist float64)
	// State broadcasts OnDroneStateChange for this record. removed=true sends
	// the control-lost form (ownerID=0/controllerID=0/activity=None) which
	// deletes the row client-side.
	State func(rec *Record, removed bool)
	// Killed cleans up a destroyed drone: DB item delete, ball removal
	// broadcast (with explosion), registry removal already done by manager.
	Killed func(rec *Record, killerID uint32)
	// MineCycle executes one mining cycle against an asteroid ball: moves ore
	// into the master ship's cargo. Returns false when the asteroid is gone
	// or the cargo is full (drone returns home).
	MineCycle func(rec *Record, asteroidID uint32) bool
	// AutoScoop moves the drone item back into the controller's drone bay after a Return To
	// Drone Bay flight. Returns false if the scoop failed (bay full / ship gone); the drone then idles.
	AutoScoop func(rec *Record) bool
	// NotifyDamage feeds the NPC threat table so rats retaliate against drones.
	NotifyDamage func(targetID, attackerID uint32, applied float64)
}

// Manager owns all live drones and the AI tick loop.
type Manager struct {
	mu     sync.RWMutex
	drones map[int64]*Record // itemID → record

	cb         Callbacks
	damageRate float64 // config damage multiplier applied to drone damage
	running    bool
}

func NewManager(cb Callbacks, damageRate float64) *Manager {
	if damageRate <= 0 {
		damageRate = 1.0
	}
	return &Manager{
		drones:     make(map[int64]*Record),
		cb:         cb,
		damageRate: damageRate,
	}
}

// Register adds a freshly launched drone to the registry and starts the tick
// loop on first use. The caller has already created the entity, added it to
// the solar system, and broadcast AddBalls.
func (m *Manager) Register(rec *Record) {
	m.mu.Lock()
	m.drones[rec.ItemID] = rec
	if !m.running {
		m.running = true
		safego.Forever("drone.tickLoop", m.tickLoop)
	}
	m.mu.Unlock()
	if m.cb.State != nil {
		m.cb.State(rec, false)
	}
	log.Printf("[drone] launched %s (item=%d type=%d) owner=%d controller=%d in system %d",
		rec.TypeName, rec.ItemID, rec.TypeID, rec.OwnerID, rec.ControllerID, rec.System.ID)
}

// Get returns the record for a drone itemID, or nil.
func (m *Manager) Get(itemID int64) *Record {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.drones[itemID]
}

// IsDrone reports whether a ball entityID belongs to a live drone.
func (m *Manager) IsDrone(entityID uint32) bool {
	return m.Get(int64(entityID)) != nil
}

// Remove drops a drone from the registry (scoop or death). It does not touch the solar
// system entity; callers handle ball removal.
func (m *Manager) Remove(itemID int64) *Record {
	m.mu.Lock()
	rec := m.drones[itemID]
	delete(m.drones, itemID)
	m.mu.Unlock()
	return rec
}

// RemoveSystem drops every drone record tied to a solar system being torn down and returns
// them for DB reconciliation, so the dead system's object graph is not kept reachable and the
// tick loop stops driving its drones.
func (m *Manager) RemoveSystem(sysID int64) []*Record {
	m.mu.Lock()
	var removed []*Record
	for id, rec := range m.drones {
		if rec.System != nil && rec.System.ID == sysID {
			delete(m.drones, id)
			removed = append(removed, rec)
		}
	}
	m.mu.Unlock()
	return removed
}

// RowsFor builds the droneState rowset rows for one pilot's SetState payload: the drones in
// the system that pilot owns or controls, or already holds a row for (VisibleTo). Each pilot
// given a row is marked notified, so the drone's later changes reach them.
func (m *Manager) RowsFor(sysID, charID, shipID int64) []destiny.DroneStateRow {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var rows []destiny.DroneStateRow
	for _, rec := range m.drones {
		if rec.System == nil || rec.System.ID != sysID {
			continue
		}
		rec.mu.Lock()
		if !rec.visibleToLocked(charID, shipID) {
			rec.mu.Unlock()
			continue
		}
		rec.markNotifiedLocked(charID)
		rows = append(rows, destiny.DroneStateRow{
			DroneID:           rec.ItemID,
			OwnerID:           rec.OwnerID,
			ControllerID:      rec.ControllerID,
			ActivityState:     rec.Activity,
			TypeID:            rec.TypeID,
			ControllerOwnerID: rec.ControllerOwnerID,
		})
		rec.mu.Unlock()
	}
	return rows
}

// CountControlled returns how many live drones a ship currently controls.
func (m *Manager) CountControlled(shipID int64) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n := 0
	for _, rec := range m.drones {
		rec.mu.Lock()
		if rec.ControllerID == shipID && !rec.Dead {
			n++
		}
		rec.mu.Unlock()
	}
	return n
}

// ControlledBy returns all live drones controlled by shipID.
func (m *Manager) ControlledBy(shipID int64) []*Record {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*Record
	for _, rec := range m.drones {
		rec.mu.Lock()
		if rec.ControllerID == shipID && !rec.Dead {
			out = append(out, rec)
		}
		rec.mu.Unlock()
	}
	return out
}

// FightersControlledBy returns the live fighter drones controlled by shipID.
func (m *Manager) FightersControlledBy(shipID int64) []*Record {
	var out []*Record
	for _, rec := range m.ControlledBy(shipID) {
		if rec.GroupID == GroupFighterDrone {
			out = append(out, rec)
		}
	}
	return out
}

// BeginWarp marks a fighter as warp-following its controller: the tick loop
// leaves warping drones alone, movement is owned by the warp goroutine.
// Returns false if the drone is dead or already warping.
func (m *Manager) BeginWarp(rec *Record) bool {
	rec.mu.Lock()
	if rec.Dead || rec.Warping {
		rec.mu.Unlock()
		return false
	}
	prevTarget := rec.TargetID
	rec.Warping = true
	rec.Activity = StatePursuit // "Following" in the client drone window
	rec.TargetID = 0
	rec.ReturnBay = false
	rec.moveSet = false
	rec.mu.Unlock()
	if prevTarget != 0 && m.cb.StopFire != nil {
		m.cb.StopFire(rec, prevTarget)
	}
	if m.cb.State != nil {
		m.cb.State(rec, false)
	}
	return true
}

// EndWarp clears the warp-follow flag; the next tick resumes normal AI.
func (m *Manager) EndWarp(rec *Record) {
	rec.mu.Lock()
	if !rec.Warping {
		rec.mu.Unlock()
		return
	}
	rec.Warping = false
	rec.Activity = StateIdle
	rec.moveSet = false
	rec.mu.Unlock()
	if m.cb.State != nil {
		m.cb.State(rec, false)
	}
}

// HandleDestroyed routes a drone kill: marks the record dead, removes it
// from the registry, broadcasts the state-row removal, and runs the Killed
// callback for item/ball cleanup. Returns false if entityID is not a drone.
func (m *Manager) HandleDestroyed(entityID uint32, killerID uint32) bool {
	rec := m.Get(int64(entityID))
	if rec == nil {
		return false
	}
	rec.mu.Lock()
	if rec.Dead {
		rec.mu.Unlock()
		return true
	}
	rec.Dead = true
	tgt := rec.TargetID
	rec.mu.Unlock()

	if tgt != 0 && m.cb.StopFire != nil {
		m.cb.StopFire(rec, tgt)
	}
	m.Remove(rec.ItemID)
	if m.cb.State != nil {
		m.cb.State(rec, true)
	}
	if m.cb.Killed != nil {
		m.cb.Killed(rec, killerID)
	}
	log.Printf("[drone] %s (item=%d) destroyed by %d", rec.TypeName, rec.ItemID, killerID)
	return true
}

// ── Command API (entity service Cmd*) ───────────────────────────────────────
//
// All command funcs take the caller's ship+char so control is enforced, and return a failure
// map {droneID: (msgKey, msgDict)} per the client contract; empty means every drone accepted.

type CmdFailure struct {
	MsgKey string
	Info   string // becomes {'info': ...} for CustomInfo-style messages
}

func (m *Manager) command(droneIDs []int64, shipID int64, apply func(rec *Record) *CmdFailure) map[int64]CmdFailure {
	fails := make(map[int64]CmdFailure)
	for _, id := range droneIDs {
		rec := m.Get(id)
		if rec == nil {
			fails[id] = CmdFailure{MsgKey: "CustomInfo", Info: "That drone is not in space."}
			continue
		}
		rec.mu.Lock()
		controller := rec.ControllerID
		dead := rec.Dead
		warping := rec.Warping
		rec.mu.Unlock()
		if dead {
			fails[id] = CmdFailure{MsgKey: "CustomInfo", Info: "That drone is incapacitated."}
			continue
		}
		if warping {
			fails[id] = CmdFailure{MsgKey: "CustomInfo", Info: "That drone is in warp."}
			continue
		}
		if controller != shipID {
			fails[id] = CmdFailure{MsgKey: "CustomInfo", Info: "You do not control that drone."}
			continue
		}
		if f := apply(rec); f != nil {
			fails[id] = *f
		}
	}
	return fails
}

// Engage orders drones to attack targetID.
func (m *Manager) Engage(droneIDs []int64, shipID int64, targetID uint32) map[int64]CmdFailure {
	return m.command(droneIDs, shipID, func(rec *Record) *CmdFailure {
		target := rec.System.GetEntity(targetID)
		if target == nil || target.IsDestroyed() {
			return &CmdFailure{MsgKey: "CustomInfo", Info: "The target is no longer present."}
		}
		rec.mu.Lock()
		prevTarget := rec.TargetID
		rec.Activity = StateApproaching
		rec.TargetID = targetID
		rec.ReturnBay = false
		rec.NextCycle = time.Time{}
		rec.mu.Unlock()
		if prevTarget != 0 && prevTarget != targetID && m.cb.StopFire != nil {
			m.cb.StopFire(rec, prevTarget)
		}
		if m.cb.State != nil {
			m.cb.State(rec, false)
		}
		return nil
	})
}

// Mine orders mining drones at an asteroid ball.
func (m *Manager) Mine(droneIDs []int64, shipID int64, asteroidID uint32, repeat bool) map[int64]CmdFailure {
	return m.command(droneIDs, shipID, func(rec *Record) *CmdFailure {
		if rec.Attrs.MiningAmount <= 0 {
			return &CmdFailure{MsgKey: "CustomInfo", Info: "That drone cannot mine."}
		}
		if _, ok := rec.System.GetAsteroidPosition(asteroidID); !ok {
			return &CmdFailure{MsgKey: "CustomInfo", Info: "The asteroid is no longer present."}
		}
		rec.mu.Lock()
		rec.Activity = StateApproaching
		rec.TargetID = asteroidID
		rec.RepeatMine = repeat
		rec.ReturnBay = false
		rec.NextCycle = time.Time{}
		rec.mu.Unlock()
		if m.cb.State != nil {
			m.cb.State(rec, false)
		}
		return nil
	})
}

// ReturnHome orders drones back to orbit the master ship.
func (m *Manager) ReturnHome(droneIDs []int64, shipID int64) map[int64]CmdFailure {
	return m.command(droneIDs, shipID, func(rec *Record) *CmdFailure {
		m.startReturn(rec, false)
		return nil
	})
}

// ReturnBay orders drones back for an automatic scoop into the drone bay.
func (m *Manager) ReturnBay(droneIDs []int64, shipID int64) map[int64]CmdFailure {
	return m.command(droneIDs, shipID, func(rec *Record) *CmdFailure {
		m.startReturn(rec, true)
		return nil
	})
}

func (m *Manager) startReturn(rec *Record, toBay bool) {
	rec.mu.Lock()
	prevTarget := rec.TargetID
	rec.TargetID = 0
	rec.ReturnBay = toBay
	if toBay {
		rec.Activity = StateDeparting2
	} else {
		rec.Activity = StateDeparting
	}
	rec.mu.Unlock()
	if prevTarget != 0 && m.cb.StopFire != nil {
		m.cb.StopFire(rec, prevTarget)
	}
	if m.cb.State != nil {
		m.cb.State(rec, false)
	}
}

// DelegateControl hands drone control to another pilot's ship.
func (m *Manager) DelegateControl(droneIDs []int64, shipID int64, newControllerShipID, newControllerCharID int64, newMasterID uint32) map[int64]CmdFailure {
	return m.command(droneIDs, shipID, func(rec *Record) *CmdFailure {
		rec.mu.Lock()
		rec.ControllerID = newControllerShipID
		rec.ControllerOwnerID = newControllerCharID
		rec.MasterID = newMasterID
		rec.Activity = StateIdle
		rec.TargetID = 0
		rec.mu.Unlock()
		if m.cb.State != nil {
			m.cb.State(rec, false)
		}
		return nil
	})
}

// RelinquishControl returns control of owned drones to the owner's ship. The caller is the
// owner, who need not be the current controller.
func (m *Manager) RelinquishControl(droneIDs []int64, ownerCharID, ownerShipID int64, ownerMasterID uint32) map[int64]CmdFailure {
	fails := make(map[int64]CmdFailure)
	for _, id := range droneIDs {
		rec := m.Get(id)
		if rec == nil {
			fails[id] = CmdFailure{MsgKey: "CustomInfo", Info: "That drone is not in space."}
			continue
		}
		if rec.OwnerID != ownerCharID {
			fails[id] = CmdFailure{MsgKey: "CustomInfo", Info: "You do not own that drone."}
			continue
		}
		rec.mu.Lock()
		if rec.Warping {
			rec.mu.Unlock()
			fails[id] = CmdFailure{MsgKey: "CustomInfo", Info: "That drone is in warp."}
			continue
		}
		rec.ControllerID = ownerShipID
		rec.ControllerOwnerID = ownerCharID
		rec.MasterID = ownerMasterID
		rec.Activity = StateIdle
		rec.TargetID = 0
		rec.mu.Unlock()
		if m.cb.State != nil {
			m.cb.State(rec, false)
		}
	}
	return fails
}

// AbandonControl is called when a controller leaves the grid (dock, jump,
// logoff, death): every drone the ship controls goes idle in space, and its
// controller reverts to the owner (so the owner can command/scoop it later).
func (m *Manager) AbandonControl(shipID int64) {
	for _, rec := range m.ControlledBy(shipID) {
		rec.mu.Lock()
		prevTarget := rec.TargetID
		warping := rec.Warping
		rec.Activity = StateIdle
		rec.TargetID = 0
		rec.ReturnBay = false
		rec.MasterID = 0
		rec.mu.Unlock()
		if prevTarget != 0 && m.cb.StopFire != nil {
			m.cb.StopFire(rec, prevTarget)
		}
		// A fighter mid-warp-follow owns its own movement; don't broadcast a
		// Stop for a ball the clients see in warp.
		if !warping {
			m.issueMove(rec, Action{Type: ActStop})
		}
		if m.cb.State != nil {
			m.cb.State(rec, false)
		}
	}
}

// ReclaimControl is called when the owner returns to space in the same
// system: any of their idle, uncontrolled drones re-bind to the new ship.
func (m *Manager) ReclaimControl(ownerCharID, shipID int64, masterID uint32, sysID int64) {
	m.mu.RLock()
	var mine []*Record
	for _, rec := range m.drones {
		if rec.OwnerID == ownerCharID && rec.System != nil && rec.System.ID == sysID {
			mine = append(mine, rec)
		}
	}
	m.mu.RUnlock()
	for _, rec := range mine {
		rec.mu.Lock()
		if rec.MasterID == 0 && !rec.Dead {
			rec.ControllerID = shipID
			rec.ControllerOwnerID = ownerCharID
			rec.MasterID = masterID
			rec.mu.Unlock()
			if m.cb.State != nil {
				m.cb.State(rec, false)
			}
			continue
		}
		rec.mu.Unlock()
	}
}

// ── AI tick ─────────────────────────────────────────────────────────────────

func (m *Manager) tickLoop() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		m.mu.RLock()
		recs := make([]*Record, 0, len(m.drones))
		for _, rec := range m.drones {
			recs = append(recs, rec)
		}
		m.mu.RUnlock()
		for _, rec := range recs {
			m.tickDrone(rec)
		}
	}
}

func (m *Manager) issueMove(rec *Record, act Action) {
	rec.mu.Lock()
	if m.moveEqual(rec.lastMove, act) && rec.moveSet {
		rec.mu.Unlock()
		return
	}
	rec.lastMove = act
	rec.moveSet = true
	rec.mu.Unlock()

	// Apply server-side movement on the drone's own destiny manager.
	dm := rec.Entity.Manager
	switch act.Type {
	case ActFollow:
		dm.SetSpeedFraction(1.0)
		dm.Follow(act.TargetID, act.Dist)
	case ActOrbit:
		dm.SetSpeedFraction(1.0)
		dm.Orbit(act.TargetID, act.Dist)
	case ActGoto:
		dm.SetSpeedFraction(1.0)
		dm.GotoDirection(act.Pos)
	case ActStop:
		dm.Stop()
	}
	if m.cb.Move != nil {
		m.cb.Move(rec, act)
	}
}

func (m *Manager) moveEqual(a, b Action) bool {
	return a.Type == b.Type && a.TargetID == b.TargetID && a.Dist == b.Dist && a.Pos == b.Pos
}

func (m *Manager) tickDrone(rec *Record) {
	rec.mu.Lock()
	if rec.Dead {
		rec.mu.Unlock()
		return
	}
	if rec.Warping {
		// Fighter warp-follow: the warp goroutine owns movement and state
		// until EndWarp.
		rec.mu.Unlock()
		return
	}
	activity := rec.Activity
	targetID := rec.TargetID
	masterID := rec.MasterID
	returnBay := rec.ReturnBay
	repeatMine := rec.RepeatMine
	attrs := rec.Attrs
	nextCycle := rec.NextCycle
	rec.mu.Unlock()

	ss := rec.System
	dronePos := rec.Entity.Pos()

	// Resolve the master ship. A master mid-warp, docked or destroyed leaves the drone
	// temporarily uncontrolled: it goes idle in place.
	var master *destiny.Entity
	if masterID != 0 {
		master = ss.GetEntity(masterID)
		if master != nil && (master.IsDestroyed() || master.IsInWarp() || master.IsWarpPending() || master.BubbleID < 0) {
			master = nil
		}
	}
	if master == nil {
		if activity != StateIdle {
			rec.mu.Lock()
			prevTarget := rec.TargetID
			rec.Activity = StateIdle
			rec.TargetID = 0
			rec.ReturnBay = false
			rec.mu.Unlock()
			if prevTarget != 0 && m.cb.StopFire != nil {
				m.cb.StopFire(rec, prevTarget)
			}
			m.issueMove(rec, Action{Type: ActStop})
			if m.cb.State != nil {
				m.cb.State(rec, false)
			}
		}
		return
	}

	now := time.Now()

	switch activity {
	case StateIdle:
		// Master on another grid (fighter left behind by a canceled warp): hold position rather than
		// orbit a ball the destination bubble's clients never got.
		if master.BubbleID != rec.Entity.BubbleID {
			m.issueMove(rec, Action{Type: ActStop})
			return
		}
		// Orbit the master ship at close range.
		m.issueMove(rec, Action{Type: ActOrbit, TargetID: master.ID, Dist: 750})

	case StateApproaching:
		if attrs.MiningAmount > 0 {
			// Mining approach: fly at the asteroid, switch to Mining in range.
			astPos, ok := ss.GetAsteroidPosition(targetID)
			if !ok {
				m.startReturn(rec, false)
				return
			}
			dist := astPos.Sub(dronePos).Len()
			if dist <= 2500 {
				rec.mu.Lock()
				rec.Activity = StateMining
				rec.NextCycle = now.Add(m.cycleDuration(attrs))
				rec.mu.Unlock()
				m.issueMove(rec, Action{Type: ActStop})
				if m.cb.State != nil {
					m.cb.State(rec, false)
				}
				return
			}
			m.issueMove(rec, Action{Type: ActFollow, TargetID: targetID, Dist: 1500})
			return
		}
		target := ss.GetEntity(targetID)
		if target == nil || target.IsDestroyed() || target.IsInWarp() || target.BubbleID < 0 {
			m.startReturn(rec, false)
			return
		}
		dist := target.Pos().Sub(dronePos).Len()
		engageRange := m.engageRange(attrs)
		if dist <= engageRange {
			rec.mu.Lock()
			rec.Activity = StateCombat
			rec.NextCycle = now // fire immediately on arrival
			rec.mu.Unlock()
			m.issueMove(rec, Action{Type: ActOrbit, TargetID: targetID, Dist: attrs.OrbitRange})
			if m.cb.State != nil {
				m.cb.State(rec, false)
			}
			return
		}
		m.issueMove(rec, Action{Type: ActFollow, TargetID: targetID, Dist: attrs.OrbitRange})

	case StateCombat:
		target := ss.GetEntity(targetID)
		if target == nil || target.IsDestroyed() || target.IsInWarp() || target.BubbleID < 0 {
			if m.cb.StopFire != nil && targetID != 0 {
				m.cb.StopFire(rec, targetID)
			}
			m.startReturn(rec, false)
			return
		}
		dist := target.Pos().Sub(dronePos).Len()
		// Target slipped out of engage range: chase it again.
		if dist > m.engageRange(attrs)*1.5 {
			rec.mu.Lock()
			rec.Activity = StateApproaching
			rec.mu.Unlock()
			if m.cb.StopFire != nil {
				m.cb.StopFire(rec, targetID)
			}
			m.issueMove(rec, Action{Type: ActFollow, TargetID: targetID, Dist: attrs.OrbitRange})
			if m.cb.State != nil {
				m.cb.State(rec, false)
			}
			return
		}
		m.issueMove(rec, Action{Type: ActOrbit, TargetID: targetID, Dist: attrs.OrbitRange})
		if !nextCycle.After(now) {
			rec.mu.Lock()
			rec.NextCycle = now.Add(m.cycleDuration(attrs))
			rec.mu.Unlock()
			m.fireCycle(rec, target, dist)
		}

	case StateMining:
		astPos, ok := ss.GetAsteroidPosition(targetID)
		if !ok {
			m.startReturn(rec, false)
			return
		}
		dist := astPos.Sub(dronePos).Len()
		if dist > 3500 {
			// Drifted off the rock: close back in.
			m.issueMove(rec, Action{Type: ActFollow, TargetID: targetID, Dist: 1500})
			return
		}
		m.issueMove(rec, Action{Type: ActStop})
		if !nextCycle.After(now) {
			rec.mu.Lock()
			rec.NextCycle = now.Add(m.cycleDuration(attrs))
			rec.mu.Unlock()
			ok := false
			if m.cb.MineCycle != nil {
				ok = m.cb.MineCycle(rec, targetID)
			}
			if !ok || !repeatMine {
				m.startReturn(rec, false)
			}
		}

	case StateDeparting, StateDeparting2:
		dist := master.Pos().Sub(dronePos).Len()
		if dist > 1500 {
			m.issueMove(rec, Action{Type: ActFollow, TargetID: master.ID, Dist: 500})
			return
		}
		if returnBay && activity == StateDeparting2 {
			if m.cb.AutoScoop != nil && m.cb.AutoScoop(rec) {
				return // record removed by scoop path
			}
		}
		// Arrived: settle into idle orbit.
		rec.mu.Lock()
		rec.Activity = StateIdle
		rec.ReturnBay = false
		rec.mu.Unlock()
		m.issueMove(rec, Action{Type: ActOrbit, TargetID: master.ID, Dist: 750})
		if m.cb.State != nil {
			m.cb.State(rec, false)
		}

	default:
		// Unknown state: recover to idle.
		rec.mu.Lock()
		rec.Activity = StateIdle
		rec.mu.Unlock()
	}
}

// engageRange is how close a drone must be before it opens fire.
func (m *Manager) engageRange(a Attributes) float64 {
	r := a.Optimal + a.Falloff
	if r < 3000 {
		r = 3000
	}
	return r
}

func (m *Manager) cycleDuration(a Attributes) time.Duration {
	ms := a.CycleMs
	if ms <= 0 {
		if a.MiningAmount > 0 {
			ms = 60000
		} else {
			ms = 4000
		}
	}
	return time.Duration(ms) * time.Millisecond
}

// fireCycle rolls one weapon cycle against target and applies damage.
// Mirrors the player turret model: chance = 0.5^((dist-optimal)/falloff)²,
// one roll decides miss/hit/wrecking.
func (m *Manager) fireCycle(rec *Record, target *destiny.Entity, dist float64) {
	a := rec.Attrs
	// EWAR drones have no guns: one cycle is one electronic-warfare attempt,
	// rolled and applied by the server half, and nothing dies of it.
	if a.EwarKind != "" {
		if m.cb.Ewar != nil {
			m.cb.Ewar(rec, target, dist)
		}
		return
	}
	exponent := 0.0
	if a.Optimal > 0 && dist > a.Optimal && a.Falloff > 0 {
		f := (dist - a.Optimal) / a.Falloff
		exponent += f * f
	}
	hitChance := math.Pow(0.5, exponent)

	roll := rand.Float64()
	dmgMod := 0.0
	switch {
	case roll < 0.01*hitChance:
		dmgMod = 3.0
	case roll <= hitChance:
		dmgMod = roll + 0.49
	}

	var applied float64
	var destroyed bool
	if dmgMod > 0 {
		mult := a.DmgMult
		if mult <= 0 {
			mult = 1.0
		}
		applied, destroyed = target.ApplyDamage(
			a.DmgEM*mult*dmgMod*m.damageRate,
			a.DmgExp*mult*dmgMod*m.damageRate,
			a.DmgKin*mult*dmgMod*m.damageRate,
			a.DmgTherm*mult*dmgMod*m.damageRate,
		)
	}

	// Kill-report snapshot for the CONCORD mail on player deaths: credit the controlling ship
	// (the owner's), or a bare charID record if the master left the system.
	if target.IsPlayer && (applied > 0 || destroyed) {
		_, ctrlOwner, _, masterID, _ := rec.Snapshot()
		var master *destiny.Entity
		if rec.System != nil && masterID != 0 {
			master = rec.System.GetEntity(masterID)
		}
		if master != nil {
			target.RecordKillHit(master, rec.TypeID, applied, destroyed)
		} else if ctrlOwner != 0 {
			// Bare record: no name, corp, alliance or security status. RecordKillHitRaw keeps the fields
			// of an existing full snapshot for this attacker.
			target.RecordKillHitRaw(ctrlOwner, destiny.KillHit{
				CharID:       ctrlOwner,
				ShipTypeID:   rec.TypeID,
				WeaponTypeID: rec.TypeID,
				Damage:       applied,
				FinalBlow:    destroyed,
			})
		}
	}

	if applied > 0 && !target.IsPlayer && m.cb.NotifyDamage != nil {
		m.cb.NotifyDamage(target.ID, rec.Entity.ID, applied)
	}
	if m.cb.Fire != nil {
		m.cb.Fire(rec, target, applied, destroyed, dmgMod)
	}
	if destroyed {
		rec.mu.Lock()
		rec.TargetID = 0
		rec.Activity = StateIdle
		rec.mu.Unlock()
		if m.cb.StopFire != nil {
			m.cb.StopFire(rec, target.ID)
		}
		if m.cb.State != nil {
			m.cb.State(rec, false)
		}
	}
}
