package destiny

import (
	"errors"
	"gurimoon/dogma"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"fmt"
	"log"
	"math"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ErrNoConn is returned by SendStamped and SendFXStamped when the entity has
// no connection (NPC, celestial or bot entity).
var ErrNoConn = errors.New("destiny: entity has no connection")

// ActiveEffect is a module effect currently running on an entity. SetState
// encodes the set as effectStates so a newly arriving client sees it.
type ActiveEffect struct {
	EntityID   uint32
	ModuleID   int64
	ModuleType int64
	TargetID   uint32
	OtherType  int64  // charge/ammo typeID (0 = none)
	GUID       string // FX GUID (e.g. "effects.Laser")
	IsOffensive bool
	DurationMs int64
	StartTime  uint64 // win32 filetime when the effect started
}

// HPState holds current and maximum hitpoints for the shield, armor and
// structure layers, in absolute HP. A zero MaxStructure means the entity has
// no HP tracking and its DamageState fractions are served as-is.
type HPState struct {
	MaxShield    float64
	CurShield    float64
	MaxArmor     float64
	CurArmor     float64
	MaxStructure float64
	CurStructure float64
	ShieldTau    float64 // shield recharge time constant (seconds)
}

// Entity represents a single entity (player ship, NPC, celestial) registered
// in a SolarSystem. It holds shared state that other goroutines may read
// (e.g. to build AddBalls for a new arrival).
type Entity struct {
	ID        uint32
	Manager   *Manager
	Slim      SlimItem
	Damage    DamageState
	HP        HPState      // mutable HP, protected by mu
	Conn      *evenet.Conn // nil for NPCs/celestials
	AccountID uint64
	Addr      string // remote address for logging
	CharName  string // character name (kill mails, chat, fallback ball name)
	ShipName  string // ship's entity.itemName, the preferred ball name for player ships
	// The client hints cfg.evelocations with ball.name on every AddBalls, so the
	// char name here would overwrite custom ship names on undock.
	BubbleID     int  // -1 = not in any bubble (in warp tunnel, docked)
	BubblePinned bool // true = skip tick-loop bubble ejection (force-placed by EnsureSameBubble)
	IsPlayer     bool

	// PlayerState is the per-connection state (destinyConn in main.go) of a
	// player entity, so destruction code running in an attacker's goroutine can
	// reach the victim's connection. Nil for NPCs and celestials.
	PlayerState interface{}

	// Resistances are the damage resonance multipliers (0.0 = immune, 1.0 = no resist).
	// Indexed by layer: 0=shield, 1=armor, 2=hull.
	// Within each: [em, explosive, kinetic, thermal].
	Resists [3][4]float64

	// NPCTurretTypeID is the turret module typeID for NPC weapon FX visuals.
	// Zero means no turret (missile NPC or unarmed). The synthetic module
	// itemIDs in Slim.Modules are derived from the entity ID at spawn time.
	NPCTurretTypeID int64

	// ActiveEffects holds the running module effects, keyed by module itemID.
	// Protected by mu; SetState builds effectStates from it.
	ActiveEffects map[int64]ActiveEffect // keyed by module itemID

	// Aggressors maps attacker entityID to the aggression time (Unix seconds).
	// Protected by mu. SetState encodes it as the aggressors dict, which the
	// client reads for red-boxing.
	Aggressors map[uint32]int64

	// killHits records every attacker that damaged this entity, keyed by entity
	// ID (or charID for drone hits whose controller has left). Read at death by
	// KillHitsSnapshot for the kill report. Protected by mu.
	killHits map[int64]KillHit

	// sys is the SolarSystem holding this entity (set by AddEntity, cleared by
	// RemoveEntity); only the combat-observer seam reads it. deathObserved makes
	// the bystander destroyed line fire once.
	sys           atomic.Pointer[SolarSystem]
	deathObserved atomic.Bool

	BaseStamp uint32    // stamp from last SetState sent to this entity's connection
	BaseTime  time.Time // wall-clock time of that SetState
	lastStamp uint32    // highest stamp returned by NextStamp (monotonicity guard)

	// wallBaseStamp/wallBaseTime are the wall-clock reference StampLead measures
	// against: written only by SetBaseStamp, never by NextStamp's recovery.
	// See docs/destiny-stamps.md#lead-and-debt. Protected by mu.
	wallBaseStamp uint32
	wallBaseTime  time.Time
	// lastRecoveryAt is when NextStamp last re-anchored BaseStamp; the debt that
	// leaves against the wall pair is forgiven from this instant
	// (forgivenDebtLocked). Zero = no recovery since the last SetState. Protected by mu.
	lastRecoveryAt time.Time

	// pendingActions is the coalesced queue of destiny actions awaiting a
	// headroom-gated flush (QueueAction, FlushQueuedActions). One entry per
	// (kind, ball) key, so the client receives final state. Protected by mu.
	pendingActions []pendingAction
	pendingSince   time.Time // when the oldest still-pending entry was queued
	lastFlushAt    time.Time // last drain, for the minFlushInterval packet floor

	// stampedFlushes/lastStampedFlushAt count the flushes that minted a stamp:
	// the cadence at which this client's grid steps forward. FX-only flushes do
	// not advance the simulation and are not counted. See docs/destiny-stamps.md#flush-gate.
	stampedFlushes     int
	lastStampedFlushAt time.Time

	// knownBalls is the set of ball IDs this player's client holds (filled by
	// SetState/AddBalls, pruned by RemoveBalls). Nil until the first SetState;
	// player entities only. Protected by mu.
	knownBalls map[uint32]struct{}
	stateSent  bool // true once the initial SetState has been delivered

	// dupAdds counts redundant AddBalls per ball and dupLogged records which have
	// been reported. See MarkBallsKnown.
	dupAdds   map[uint32]int
	dupLogged map[uint32]struct{}

	// noLockUntil is the post-warp landing grace deadline; target pickers treat
	// the entity as untargetable until then. Protected by mu.
	noLockUntil time.Time

	// NPCStandings holds per-character NPC faction standings (factionID→standing).
	// Loaded at undock/login for player entities. Protected by mu.
	NPCStandings map[int64]float64

	// scramblers tracks active warp-scramble strength applied to this entity,
	// keyed by the scrambling module's itemID. Warp is blocked while the total
	// strength is > 0. Protected by mu.
	scramblers map[int64]float64

	// ewarDebuffs holds chance-based EWAR multipliers on this entity (tracking
	// disruptor, sensor damp, target painter), keyed by the hostile module's
	// itemID and refreshed per cycle by the attacker's loop. Protected by mu.
	ewarDebuffs map[int64]EwarDebuff

	// gangBoost is the gang bonus baked into this entity's HP pools and read live
	// at other use sites (lock range, scan resolution, mining yield). The zero
	// value is the identity. Protected by mu; written only by ApplyGangBoost.
	gangBoost dogma.GangBoost

	// sigMod is a standing signature-radius multiplier from outside EWAR and
	// gangs (a control tower's Stealth Emitter Arrays); 0 reads as 1.
	// Protected by mu; written only by SetSigRadiusModifier.
	sigMod float64

	// implantSig is the pilot's implant signature term (a Halo set), set at
	// undock from db.GetCharImplantMults; 0 reads as 1. Kept apart from sigMod,
	// which the MWD bloom and Stealth Emitter arrays overwrite. Protected by mu.
	implantSig float64

	// warpPending is true between SendWarpTo (client starts aligning) and
	// ModeWarp (server enters the tunnel). The ship is still on grid, holds its
	// ballpark and stays targetable until IsInWarp(). Protected by mu.
	warpPending bool

	// gmMode is true while the pilot has /gm on: nothing may lock, approach or
	// shoot the entity and NPCs never see it. Unlike cloaked and noLockUntil it
	// is cleared by no module, gate or warp landing. Protected by mu.
	gmMode bool

	// cloaked is true while the entity has a gate or module cloak: hidden from
	// other ballparks, untargetable, broken by any movement input.
	// Protected by mu.
	cloaked bool

	// gmCloak marks cloaked as the GM's standing cloak (/gm cloak on), which no
	// movement, warp, module or gate timer breaks. gmGhost puts GMGhostRadius on
	// the wire for this ball. Only SetGMCloak writes them; protected by mu.
	gmCloak bool
	gmGhost bool

	// moduleCloak is the item ID of the cloaking device holding cloaked, 0 for none. Movement
	// does not break this cloak; the module stopping does. Only SetModuleCloak and
	// EndModuleCloak write it; protected by mu.
	moduleCloak int64

	// invulnerable marks a dungeon structure authored as scenery
	// (dunObjects.invulnerable): it locks and draws fire, but ApplyDamage never
	// takes HP off it. Protected by mu.
	invulnerable bool

	// shieldFloor, when > 0, is the fraction of max shield ApplyDamage will not
	// take the shield below; damage reaching it stops there and sets floorHit.
	// A control tower enters reinforcement on it atomically. Protected by mu.
	shieldFloor float64
	floorHit    bool

	// Capacitor: lazily evaluated exponential recharge (see InitCap).
	// capVal is the charge observed at capTime; capTau in seconds.
	// Protected by mu.
	capMax  float64
	capTau  float64
	capVal  float64
	capTime time.Time

	mu sync.Mutex // protects Manager reads from concurrent writes

	// wireMu serializes stamp allocation with the socket write to this entity's
	// connection, so stamp order equals socket order (a stamp below one already
	// written triggers the client's RequestReset). Lock order: wireMu, then mu.
	wireMu sync.Mutex

	// stampLog is a ring of recent NextStamp mints, dumped on the client's
	// UpdateStateRequest. A pointer allocated on first mint: most entities in a
	// system never mint and the inline array would cost each 2.5 KB. Protected by mu.
	stampLog    *[64]stampRecord
	stampLogIdx uint32

	// stampSources counts mints since this ballpark began, keyed by the call site
	// that asked (stampCaller); the ring is too short to say who is minting over
	// a session. Nil until the first mint. Protected by mu.
	stampSources map[string]int
	stampFirstAt time.Time
	stampMints   int
	// lastRecoveryLog rate-limits the over-budget report (recoveryLogEvery).
	lastRecoveryLog time.Time
	// stampRefusals counts sends over the budget ceiling (refused when
	// StampBudgetEnforced, counted otherwise); stampOverSources names their
	// call sites for StampReport.
	stampRefusals    int
	stampOverSources map[string]int
}

// stampLeadHardCap is how far the stamp stream may run ahead of the wall
// clock before SendStamped refuses: at a lead of 2 michelle holds three
// stamps and steps one extra tick for each stamp past two.
// See docs/destiny-stamps.md#budget-and-triage.
const stampLeadHardCap = 1

// ErrStampBudget is returned by SendStamped when a mint would make the client
// fast-forward; nothing is sent. A caller holding claimed balls must roll the
// claim back as on a write error. See docs/destiny-stamps.md#budget-and-triage.
var ErrStampBudget = errors.New("destiny: stamp budget exhausted")

// StampBudgetEnforced turns the ceiling from a counter into a refusal. Set
// from config at boot and read without synchronisation. Off until /stamps
// shows no over-ceiling sends; see docs/destiny-stamps.md#budget-and-triage.
var StampBudgetEnforced = false

// SendStamped mints the next stamp for this connection and runs send with it
// under wireMu, so stamp order equals socket order. Refuses with
// ErrStampBudget at stampLeadHardCap: ego-ball sends use SendStampedUrgent,
// other balls QueueAction. See docs/destiny-stamps.md#budget-and-triage.
func (e *Entity) SendStamped(send func(stamp uint32) error) error {
	if e.Conn == nil {
		return ErrNoConn
	}
	e.wireMu.Lock()
	defer e.wireMu.Unlock()
	e.mu.Lock()
	over := e.stampLead() >= stampLeadHardCap
	if over {
		e.stampRefusals++
		if e.stampOverSources == nil {
			e.stampOverSources = make(map[string]int, 4)
		}
		e.stampOverSources[stampCaller()]++
	}
	e.mu.Unlock()
	if over && StampBudgetEnforced {
		return ErrStampBudget
	}
	return send(e.NextStamp())
}

// SendStampedUrgent is SendStamped without the budget ceiling, for packets
// whose lateness costs more than a fast-forward: death placement, warp
// phases, SetState and pod handover. Every call is a greppable exemption.
// See docs/destiny-stamps.md#budget-and-triage.
func (e *Entity) SendStampedUrgent(send func(stamp uint32) error) error {
	if e.Conn == nil {
		return ErrNoConn
	}
	e.wireMu.Lock()
	defer e.wireMu.Unlock()
	return send(e.NextStamp())
}

// StampRefusals reports how many sends this entity refused (or would have
// refused) for budget; StampReport prints the call sites.
func (e *Entity) StampRefusals() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.stampRefusals
}

// LockWire/UnlockWire expose the wire mutex for call sites that need to
// send several stamped packets as one ordered sequence (e.g. a stop event
// followed by a RemoveBalls with a fresh stamp each).
func (e *Entity) LockWire()   { e.wireMu.Lock() }
func (e *Entity) UnlockWire() { e.wireMu.Unlock() }

// SendFXStamped runs send under the connection's current stamp, minting none.
// Only for packets whose every action michelle.RealFlushState applies without
// FindState: OnSpecialFX, OnDamageStateChange, OnGangDamageStateChange,
// OnShipStateUpdate, OnSlimItemChange, OnDroneStateChange.
// See docs/destiny-stamps.md#stamps-and-the-client-clock.
func (e *Entity) SendFXStamped(send func(stamp uint32) error) error {
	if e.Conn == nil {
		return ErrNoConn
	}
	e.wireMu.Lock()
	defer e.wireMu.Unlock()
	e.mu.Lock()
	stamp := e.lastStamp
	if e.Manager != nil && e.Manager.Stamp > stamp {
		stamp = e.Manager.Stamp
	}
	e.mu.Unlock()
	if stamp == 0 {
		// Nothing minted yet (fresh ballpark): mint normally.
		return send(e.NextStamp())
	}
	return send(stamp)
}

// ── Queued destiny actions ───────────────────────────────────────────────────
//
// Senders that can burst queue here instead of minting; FlushQueuedActions
// drains the queue as one packet under one stamp when the stream has
// wall-clock headroom. Entries coalesce by (kind, ball), so the client
// receives final state. See docs/destiny-stamps.md#queued-actions.

// pendingAction is one coalescable queued destiny action. Either action is
// pre-formed, or build renders it from the stamp the flush mints (AddBalls
// encodes that stamp inside its state blob).
type pendingAction struct {
	key    string // coalescing key, e.g. "move:700000038"
	ballID uint32 // ball the action refers to (purged when the ball is removed)
	action marshal.PyRep
	build  func(stamp uint32) marshal.PyRep
	// isAdd marks an entry that delivers the ball rather than describing one the
	// client holds. DropPendingAdd tests it: a removal racing a queued add cancels
	// both; a removal racing a queued movement must still go out.
	isAdd bool
	// isRemove marks a deferred RemoveBalls. The ball stays in the known set until
	// the packet is on the wire: the flush forgets it after a successful send and
	// re-queues the entry after a failed one. See docs/destiny-stamps.md#queued-actions.
	isRemove bool
	// batch, when non-nil, makes this a merging AddBalls: a second QueueAddBalls
	// under the same key appends instead of replacing, so the packet carries one
	// action for all the balls. See docs/destiny-stamps.md#queued-actions.
	batch *addBallsBatch
	// fx marks an action michelle.RealFlushState applies without FindState (see
	// SendFXStamped); the flush may drain it under a reused stamp while the
	// stamped partition waits. Set only by QueueFXAction.
	fx bool
}

// addBallsBatch is the payload of a merging AddBalls entry: the balls, their
// slims and their damage states, in delivery order. Guarded by the owner's mu.
type addBallsBatch struct {
	balls []BallDesc
	slims []SlimItem
	dmg   map[uint32]DamageState
}

func (b *addBallsBatch) has(id uint32) bool {
	for i := range b.balls {
		if b.balls[i].EntityID == id {
			return true
		}
	}
	return false
}

// drop removes one ball from the batch and reports whether the batch is now
// empty. The other balls in the batch keep their delivery.
func (b *addBallsBatch) drop(id uint32) (empty bool) {
	kb := b.balls[:0]
	for _, bd := range b.balls {
		if bd.EntityID != id {
			kb = append(kb, bd)
		}
	}
	b.balls = kb
	ks := b.slims[:0]
	for _, sl := range b.slims {
		if uint32(sl.BallID) != id {
			ks = append(ks, sl)
		}
	}
	b.slims = ks
	delete(b.dmg, id)
	return len(b.balls) == 0
}

// refers reports whether this entry concerns ballID: its own ball, or any ball
// in its batch. Every ID-matching helper on the queue goes through it.
func (pa *pendingAction) refers(ballID uint32) bool {
	if pa.ballID == ballID {
		return true
	}
	return pa.batch != nil && pa.batch.has(ballID)
}

// QueueAddBalls queues one ball for delivery, merging into any AddBalls already
// queued under the same key. A caller publishing many balls in one pass (a
// system filling in with projections, a bubble merge) spends one action and
// one stamp. dmg may be the zero DamageState for a ball with no HP.
func (e *Entity) QueueAddBalls(key string, bd BallDesc, slim SlimItem, dmg DamageState) {
	if e.Conn == nil {
		return
	}
	b := &addBallsBatch{
		balls: []BallDesc{bd},
		slims: []SlimItem{slim},
		dmg:   map[uint32]DamageState{bd.EntityID: dmg},
	}
	e.queue(pendingAction{
		key:    key,
		ballID: bd.EntityID,
		batch:  b,
		isAdd:  true,
		build: func(stamp uint32) marshal.PyRep {
			// Read under the entity lock: the flush calls build with mu
			// released, and another goroutine may be merging into this batch.
			e.mu.Lock()
			balls := append([]BallDesc(nil), b.balls...)
			slims := append([]SlimItem(nil), b.slims...)
			dm := make(map[uint32]DamageState, len(b.dmg))
			for k, v := range b.dmg {
				dm[k] = v
			}
			e.mu.Unlock()
			if len(balls) == 0 {
				return nil
			}
			return BuildAddBallsAction(stamp, balls, slims, dm)
		},
	})
}

// maxPendingHold bounds how long a queued action can be deferred for stamp
// headroom before it is sent anyway. Movement commands must eventually reach
// the client even when the stream runs hot.
const maxPendingHold = 3 * time.Second

// minFlushInterval is the floor on how often one connection's queue may be
// drained: it bounds packets where the StampLead gate bounds stamps. Kept
// below the 200 ms system tick. See docs/destiny-stamps.md#flush-gate.
const minFlushInterval = 150 * time.Millisecond

// QueueAction queues one destiny action for a headroom-gated batched send.
// An already-queued action with the same key is replaced in place (keeping
// its queue position, so relative order of distinct keys is stable).
// No-op for entities without a connection.
func (e *Entity) QueueAction(key string, ballID uint32, action marshal.PyRep) {
	if e.Conn == nil {
		return
	}
	e.queue(pendingAction{key: key, ballID: ballID, action: action})
}

// QueueFXAction queues a stamp-agnostic action (the michelle.RealFlushState
// set, see SendFXStamped) with QueueAction's last-wins coalescing;
// FlushQueuedActions may drain it under a reused stamp while the stamped
// partition waits for headroom. Damage state is the typical use.
func (e *Entity) QueueFXAction(key string, ballID uint32, action marshal.PyRep) {
	if e.Conn == nil {
		return
	}
	e.queue(pendingAction{key: key, ballID: ballID, action: action, fx: true})
}

// QueueActionFunc queues an action whose payload depends on the stamp it is
// finally sent under (AddBalls embeds the stamp in its state blob). Same
// coalescing rules as QueueAction.
func (e *Entity) QueueActionFunc(key string, ballID uint32, build func(stamp uint32) marshal.PyRep) {
	if e.Conn == nil || build == nil {
		return
	}
	e.queue(pendingAction{key: key, ballID: ballID, build: build})
}

// QueueAddActionFunc is QueueActionFunc for an action that delivers ballID to
// the client (an AddBalls). DropPendingAdd uses the distinction to tell an
// undelivered ball from a delivered one with a movement queued.
func (e *Entity) QueueAddActionFunc(key string, ballID uint32, build func(stamp uint32) marshal.PyRep) {
	if e.Conn == nil || build == nil {
		return
	}
	e.queue(pendingAction{key: key, ballID: ballID, build: build, isAdd: true})
}

// QueueRemoveBall queues a RemoveBalls for ballID without forgetting the ball
// up front; the flush retires it once the packet has left. Use it instead of
// ForgetBalls followed by QueueAction. See docs/destiny-stamps.md#queued-actions.
func (e *Entity) QueueRemoveBall(key string, ballID uint32) {
	if e.Conn == nil {
		return
	}
	e.queue(pendingAction{
		key:      key,
		ballID:   ballID,
		action:   BuildRemoveBallsAction([]uint32{ballID}),
		isRemove: true,
	})
}

// QueueEgoGoto queues the pilot's own GotoDirection echo, with its speed
// fraction, for the next headroom flush instead of minting for it. A later
// call replaces it. The flush drops the echo when the ball no longer flies to
// target: whatever changed the course sent its own echo. Follow it with
// FlushQueuedActions so a free stamp carries it at once.
// See docs/destiny-stamps.md#nav-click-burst.
func (e *Entity) QueueEgoGoto(target Vec3) {
	if e.Conn == nil || e.Manager == nil {
		return
	}
	// current reads the speed fraction, or false once the course has changed.
	current := func() (float64, bool) {
		e.mu.Lock()
		defer e.mu.Unlock()
		m := e.Manager
		if m == nil || m.nav != navGoto || m.targetPos != target {
			return 0, false
		}
		return m.activeSpeedFraction, true
	}
	id := marshal.PyInt{Value: uint64(e.Manager.EntityID)}
	// Two keys: one shared key would let the goto replace the speed fraction.
	e.QueueActionFunc("ego:speed", e.ID, func(uint32) marshal.PyRep {
		f, ok := current()
		if !ok {
			return nil
		}
		return BuildSimpleAction("SetSpeedFraction", id, marshal.PyReal{Value: f})
	})
	e.QueueActionFunc("ego:goto", e.ID, func(uint32) marshal.PyRep {
		if _, ok := current(); !ok {
			return nil
		}
		return BuildSimpleAction("GotoDirection", id,
			marshal.PyReal{Value: target.X}, marshal.PyReal{Value: target.Y}, marshal.PyReal{Value: target.Z})
	})
}

// DropPendingRemove cancels any queued removal for ballID and reports whether
// one was cancelled. A true return means the client still holds the ball and
// no AddBalls is needed to put it back on the grid.
func (e *Entity) DropPendingRemove(ballID uint32) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.dropPendingRemoveLocked(ballID)
}

// dropPendingRemoveLocked is DropPendingRemove without locking.
func (e *Entity) dropPendingRemoveLocked(ballID uint32) bool {
	dropped := false
	kept := e.pendingActions[:0]
	for _, pa := range e.pendingActions {
		if pa.isRemove && pa.ballID == ballID {
			dropped = true
			continue
		}
		kept = append(kept, pa)
	}
	e.pendingActions = kept
	return dropped
}

// queue appends or replaces one entry. A replacement takes the new entry
// whole, keeping only its predecessor's queue position, so the last writer's
// kind (isAdd, isRemove, fx) is the one delivered; the rm: key is written by
// both QueueRemoveBall and a plain QueueAction after ClaimKnownBalls.
func (e *Entity) queue(pa pendingAction) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range e.pendingActions {
		if e.pendingActions[i].key != pa.key {
			continue
		}
		// Two merging AddBalls under one key append: the second ball is not a
		// correction of the first.
		if cur := e.pendingActions[i].batch; cur != nil && pa.batch != nil {
			for j := range pa.batch.balls {
				id := pa.batch.balls[j].EntityID
				if cur.has(id) {
					continue
				}
				cur.balls = append(cur.balls, pa.batch.balls[j])
				cur.slims = append(cur.slims, pa.batch.slims[j])
				if d, ok := pa.batch.dmg[id]; ok {
					cur.dmg[id] = d
				}
			}
			return
		}
		e.pendingActions[i] = pa
		return
	}
	if len(e.pendingActions) == 0 {
		e.pendingSince = time.Now()
	}
	e.pendingActions = append(e.pendingActions, pa)
}

// DropPending removes every queued action referring to ballID and reports
// whether anything was dropped. A short-lived ball (a missile that impacts
// before its AddBalls left the queue) cancels itself with it. Prefer
// DropPendingAdd when the question is whether a removal must still go out.
func (e *Entity) DropPending(ballID uint32) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	before := len(e.pendingActions)
	e.purgePendingLocked([]uint32{ballID})
	return len(e.pendingActions) != before
}

// HasPendingAdd reports whether an AddBalls for ballID is still queued, so the
// client has not been told the ball exists. It destroys nothing: a caller
// whose ball must still be delivered (a missile, whose flight is drawn by its
// removal) can defer the removal instead.
func (e *Entity) HasPendingAdd(ballID uint32) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range e.pendingActions {
		if e.pendingActions[i].isAdd && e.pendingActions[i].refers(ballID) {
			return true
		}
	}
	return false
}

// DropPendingAdd removes every queued action referring to ballID and reports
// whether a queued add was among them. True means the client never got the
// ball, so the prompting removal must be dropped too (RemoveBalls for an
// unknown ball logs on the client); false means the removal must still go out.
func (e *Entity) DropPendingAdd(ballID uint32) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	hadAdd := false
	for i := range e.pendingActions {
		if e.pendingActions[i].isAdd && e.pendingActions[i].refers(ballID) {
			hadAdd = true
			break
		}
	}
	e.purgePendingLocked([]uint32{ballID})
	return hadAdd
}

// FlushQueuedActions drains the queue as one packet and reports whether one
// was sent. The stamped partition goes under one fresh stamp once stampLead()
// is negative, force is set or the queue is older than maxPendingHold; the fx
// partition otherwise goes alone under the reused stamp. See docs/destiny-stamps.md#flush-gate.
func (e *Entity) FlushQueuedActions(nodeID uint64, force bool) bool {
	if e.Conn == nil {
		return false
	}
	e.wireMu.Lock()
	defer e.wireMu.Unlock()

	e.mu.Lock()
	if len(e.pendingActions) == 0 {
		e.mu.Unlock()
		return false
	}
	if !force && time.Since(e.lastFlushAt) < minFlushInterval {
		e.mu.Unlock()
		return false
	}

	// Two partitions share one packet stream: stamped entries keep the one stamp
	// per wall-second contract, fx entries may ride a reused stamp every tick.
	stampedEligible := force || e.stampLead() < 0 || time.Since(e.pendingSince) >= maxPendingHold
	hasStamped := false
	var addPending map[uint32]struct{}
	for _, pa := range e.pendingActions {
		if pa.fx {
			continue
		}
		hasStamped = true
		if pa.isAdd {
			if addPending == nil {
				addPending = make(map[uint32]struct{}, 2)
			}
			addPending[pa.ballID] = struct{}{}
			if pa.batch != nil {
				// Every ball in the batch is undelivered, not only the keyed one; an fx
				// entry for any of them waits, or a damage state precedes its ball.
				for i := range pa.batch.balls {
					addPending[pa.batch.balls[i].EntityID] = struct{}{}
				}
			}
		}
	}

	var drained []pendingAction
	fxOnly := false
	if hasStamped && stampedEligible {
		// Combined flush: both partitions ride one packet under one fresh stamp.
		// Queue order is kept, so an AddBalls still precedes fx queued after it.
		drained = make([]pendingAction, len(e.pendingActions))
		copy(drained, e.pendingActions)
		e.pendingActions = e.pendingActions[:0]
	} else {
		// FX-only flush under a reused stamp. An fx entry whose ball has a queued
		// AddBalls stays behind; the stamped partition keeps its order and pendingSince.
		kept := e.pendingActions[:0]
		for _, pa := range e.pendingActions {
			if pa.fx {
				if _, waiting := addPending[pa.ballID]; !waiting {
					drained = append(drained, pa)
					continue
				}
			}
			kept = append(kept, pa)
		}
		e.pendingActions = kept
		if len(drained) == 0 {
			e.mu.Unlock()
			return false
		}
		fxOnly = true
	}
	e.lastFlushAt = time.Now()
	if !fxOnly {
		// A combined flush is the only drain that mints, and therefore the only
		// one that steps the client's simulation. See the field comment.
		e.stampedFlushes++
		e.lastStampedFlushAt = e.lastFlushAt
	}

	var stamp uint32
	if fxOnly {
		// Same computation as SendFXStamped: the connection's current stamp,
		// minting only on a fresh ballpark where nothing exists to reuse.
		stamp = e.lastStamp
		if e.Manager != nil && e.Manager.Stamp > stamp {
			stamp = e.Manager.Stamp
		}
	}
	e.mu.Unlock()

	if stamp == 0 {
		stamp = e.NextStamp()
	}
	actions := make([]marshal.PyRep, 0, len(drained))
	var retiring []uint32
	for _, pa := range drained {
		if pa.isRemove {
			// The ball may have been re-delivered or removed by another path since
			// queueing; send only for one the client is still believed to hold.
			if !e.KnowsBall(pa.ballID) {
				continue
			}
			retiring = append(retiring, pa.ballID)
		}
		if pa.build != nil {
			if a := pa.build(stamp); a != nil {
				actions = append(actions, a)
			}
			continue
		}
		actions = append(actions, pa.action)
	}
	if len(actions) == 0 {
		return false
	}
	if err := SendActionList(e.Conn, nodeID, e.AccountID, stamp, actions...); err != nil {
		// Put the batch back: nothing has been forgotten yet, so re-queueing restores
		// the pre-flush state and the next tick retries. A lost RemoveBalls is permanent.
		e.requeueFront(drained)
		log.Printf("[stamp] entity=%d queued-action flush (%d actions) failed, re-queued %d: %v",
			e.ID, len(actions), len(drained), err)
		return false
	}
	// Retire the balls only now: the removal is on the wire, so the known set
	// and the client agree again.
	if len(retiring) > 0 {
		e.ForgetBalls(retiring...)
	}
	return true
}

// requeueFront restores a drained batch to the head of the queue, preserving
// its relative order. An entry whose key has been re-queued in the meantime is
// dropped rather than restored: the newer entry describes the ball's current
// life and this one describes the one it had before the failed send.
func (e *Entity) requeueFront(drained []pendingAction) {
	if len(drained) == 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	live := make(map[string]struct{}, len(e.pendingActions))
	for _, pa := range e.pendingActions {
		live[pa.key] = struct{}{}
	}
	restored := make([]pendingAction, 0, len(drained)+len(e.pendingActions))
	for _, pa := range drained {
		if _, newer := live[pa.key]; newer {
			continue
		}
		restored = append(restored, pa)
	}
	if len(restored) == 0 {
		return
	}
	e.pendingActions = append(restored, e.pendingActions...)
	if e.pendingSince.IsZero() {
		e.pendingSince = time.Now()
	}
}

// purgePendingLocked drops queued actions that refer to any of the given
// balls (the client no longer holds them). Caller must hold e.mu.
func (e *Entity) purgePendingLocked(ids []uint32) {
	if len(e.pendingActions) == 0 || len(ids) == 0 {
		return
	}
	gone := make(map[uint32]struct{}, len(ids))
	for _, id := range ids {
		gone[id] = struct{}{}
	}
	kept := e.pendingActions[:0]
	for _, pa := range e.pendingActions {
		if pa.batch != nil {
			// Take the dead balls out of the batch rather than dropping the entry: the
			// others in it are unrelated deliveries sharing a packet.
			empty := false
			for id := range gone {
				if pa.batch.has(id) && pa.batch.drop(id) {
					empty = true
				}
			}
			if !empty {
				kept = append(kept, pa)
			}
			continue
		}
		if _, dead := gone[pa.ballID]; !dead {
			kept = append(kept, pa)
		}
	}
	e.pendingActions = kept
}

// StampLead reports how far the last minted stamp runs ahead of the wall-clock
// candidate: at or below zero a mint is free, above it another mint pushes the
// client's sim clock further ahead. Measured against the wall anchor with the
// recovery debt forgiven over time. See docs/destiny-stamps.md#lead-and-debt.
func (e *Entity) StampLead() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.stampLead()
}

// stampDebtForgiveEvery is how long a stream must go without an overshoot
// recovery for one stamp of recovery debt to be forgiven (forgivenDebtLocked).
// See docs/destiny-stamps.md#lead-and-debt.
const stampDebtForgiveEvery = 10 * time.Second

// stampLead is StampLead without locking. Caller must hold e.mu.
func (e *Entity) stampLead() int {
	floor := e.lastStamp
	if e.Manager != nil && e.Manager.Stamp > floor {
		floor = e.Manager.Stamp
	}
	baseStamp, baseTime := e.wallBaseStamp, e.wallBaseTime
	if baseTime.IsZero() {
		baseStamp, baseTime = e.BaseStamp, e.BaseTime
	}
	var wall uint32
	if !baseTime.IsZero() {
		wall = baseStamp + uint32(time.Since(baseTime).Seconds()) + 1
	} else {
		wall = baseStamp + 1
	}
	wall += e.forgivenDebtLocked(wall)
	return int(floor) - int(wall)
}

// forgivenDebtLocked is how far the wall reading may move toward the recovery
// anchor: the gap between the two anchors, capped by one stamp per
// stampDebtForgiveEvery since the last recovery. Zero with no recovery on
// record, or when the recovery anchor is not ahead. Caller must hold e.mu.
func (e *Entity) forgivenDebtLocked(wall uint32) uint32 {
	if e.lastRecoveryAt.IsZero() || e.BaseTime.IsZero() || e.wallBaseTime.IsZero() {
		return 0
	}
	client := e.BaseStamp + uint32(time.Since(e.BaseTime).Seconds()) + 1
	if client <= wall {
		return 0
	}
	gap := client - wall
	forgiven := uint32(time.Since(e.lastRecoveryAt) / stampDebtForgiveEvery)
	if forgiven < gap {
		return forgiven
	}
	return gap
}

// ResetKnownBalls replaces the known-ball set wholesale with the contents of
// a freshly sent SetState and marks the ballpark as initialized.
func (e *Entity) ResetKnownBalls(ids []uint32) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.knownBalls = make(map[uint32]struct{}, len(ids))
	for _, id := range ids {
		e.knownBalls[id] = struct{}{}
	}
	e.stateSent = true
	// Fresh ballpark: anything still queued describes the old one.
	e.pendingActions = nil
}

// MarkBallsKnown records balls delivered to this player via AddBalls. A ball
// already known means a redundant AddBalls went out, which doubles the ship's
// overview row on the client; it is counted and logged once per ball with the
// caller. Prefer ClaimUnknownBalls. See docs/destiny-stamps.md#duplicate-delivery.
func (e *Entity) MarkBallsKnown(ids ...uint32) {
	e.mu.Lock()
	if e.knownBalls == nil {
		e.knownBalls = make(map[uint32]struct{}, len(ids))
	}
	var report []uint32
	for _, id := range ids {
		if _, dup := e.knownBalls[id]; dup {
			if e.dupAdds == nil {
				e.dupAdds = make(map[uint32]int)
				e.dupLogged = make(map[uint32]struct{})
			}
			e.dupAdds[id]++
			if _, seen := e.dupLogged[id]; !seen {
				e.dupLogged[id] = struct{}{}
				report = append(report, id)
			}
		}
		e.knownBalls[id] = struct{}{}
	}
	self := e.ID
	e.mu.Unlock()

	// Logged with the lock released: stampCaller walks the stack and log writes
	// to a file, and neither belongs under the entity lock.
	if len(report) > 0 {
		caller := stampCaller()
		for _, id := range report {
			log.Printf("destiny: duplicate AddBalls for ball %d to player %d from %s "+
				"— the client already holds it, so this doubles its overview row",
				id, self, caller)
		}
	}
}

// DuplicateAddCount is how many redundant AddBalls this client has been sent,
// and for how many distinct balls.
func (e *Entity) DuplicateAddCount() (total, balls int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, n := range e.dupAdds {
		total += n
	}
	return total, len(e.dupAdds)
}

// DuplicateAddReport lists the balls this client has been sent more than once,
// worst first; empty when there are none.
func (e *Entity) DuplicateAddReport() string {
	type dupRow struct {
		id uint32
		n  int
	}
	e.mu.Lock()
	rows := make([]dupRow, 0, len(e.dupAdds))
	for id, n := range e.dupAdds {
		rows = append(rows, dupRow{id, n})
	}
	e.mu.Unlock()

	if len(rows) == 0 {
		return ""
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].n != rows[j].n {
			return rows[i].n > rows[j].n
		}
		return rows[i].id < rows[j].id
	})
	var b strings.Builder
	for _, r := range rows {
		fmt.Fprintf(&b, "  %d  x%d\n", r.id, r.n)
	}
	return b.String()
}

// ClaimUnknownBalls atomically returns the subset of ids not yet known to this
// client and marks them known, so two goroutines never both send AddBalls for
// one ball. It also purges anything queued for those balls and cancels a
// pending removal for a known one. See docs/destiny-stamps.md#queued-actions.
func (e *Entity) ClaimUnknownBalls(ids ...uint32) []uint32 {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.stateSent {
		return nil
	}
	if e.knownBalls == nil {
		e.knownBalls = make(map[uint32]struct{}, len(ids))
	}
	var claimed []uint32
	for _, id := range ids {
		// A pending removal for a ball being delivered again is cancelled and the
		// ball is not re-claimed: it never left the client.
		if e.dropPendingRemoveLocked(id) {
			continue
		}
		if _, ok := e.knownBalls[id]; !ok {
			e.knownBalls[id] = struct{}{}
			claimed = append(claimed, id)
		}
	}
	e.purgePendingLocked(claimed)
	return claimed
}

// ForgetBalls prunes balls removed from this player's client ballpark.
func (e *Entity) ForgetBalls(ids ...uint32) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, id := range ids {
		delete(e.knownBalls, id)
	}
	e.purgePendingLocked(ids)
}

// ClaimKnownBalls is the removal counterpart of ClaimUnknownBalls: it
// atomically returns the subset of ids the client holds and forgets them, so a
// RemoveBalls never names a ball the client does not hold.
func (e *Entity) ClaimKnownBalls(ids ...uint32) []uint32 {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.stateSent || e.knownBalls == nil {
		return nil
	}
	var claimed []uint32
	for _, id := range ids {
		if _, ok := e.knownBalls[id]; ok {
			delete(e.knownBalls, id)
			claimed = append(claimed, id)
		}
	}
	e.purgePendingLocked(claimed)
	return claimed
}

// KnowsBall reports whether this player's client holds the given ball.
// Returns false until the first SetState has been sent.
func (e *Entity) KnowsBall(id uint32) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.stateSent {
		return false
	}
	_, ok := e.knownBalls[id]
	return ok
}

// HasBallparkState reports whether this player's client has received its
// initial SetState (i.e. has a live ballpark to apply updates to).
func (e *Entity) HasBallparkState() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.stateSent
}

// ClearBallparkState marks this client as holding no ballpark (docked, podded
// or disconnected). Every deferred sender guards on KnowsBall or
// HasBallparkState, so a stale mate snapshot cannot deliver an update into a
// ballpark-less client. Undocking builds a fresh Entity.
func (e *Entity) ClearBallparkState() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.stateSent = false
	e.knownBalls = nil
	e.pendingActions = nil
	// Aggressors and killHits belong to the life that just ended; Aggressors is
	// re-encoded into every SetState the next life sends.
	e.Aggressors = nil
	e.killHits = nil
}

// KnownBallIDs returns a snapshot of all ball IDs this player's client
// currently holds in its ballpark. Thread-safe.
func (e *Entity) KnownBallIDs() []uint32 {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.knownBalls) == 0 {
		return nil
	}
	out := make([]uint32, 0, len(e.knownBalls))
	for id := range e.knownBalls {
		out = append(out, id)
	}
	return out
}

// ForceRedeliverBall removes a ball from the known set so the next
// ensureBallsKnown / ClaimUnknownBalls call will re-deliver it. Used when
// a previous AddBalls may have been silently rejected by the client.
func (e *Entity) ForceRedeliverBall(ballID uint32) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.knownBalls, ballID)
}

// TransferBallparkState copies the known-balls set and stamp state from
// another entity (typically the player's old ship after ship death). The
// old entity's ball is removed and the new entity's ball is added. This
// avoids a full SetState rebuild when transitioning between ship entities.
func (e *Entity) TransferBallparkState(from *Entity, oldBallID uint32) {
	from.mu.Lock()
	balls := make(map[uint32]struct{}, len(from.knownBalls))
	for k, v := range from.knownBalls {
		balls[k] = v
	}
	stamp := from.lastStamp
	baseStamp := from.BaseStamp
	baseTime := from.BaseTime
	wallStamp := from.wallBaseStamp
	wallTime := from.wallBaseTime
	recoveredAt := from.lastRecoveryAt
	from.mu.Unlock()

	// Remove old ship, add new pod.
	delete(balls, oldBallID)
	balls[e.ID] = struct{}{}

	e.mu.Lock()
	e.knownBalls = balls
	e.stateSent = true
	e.lastStamp = stamp
	e.BaseStamp = baseStamp
	e.BaseTime = baseTime
	e.wallBaseStamp = wallStamp
	e.wallBaseTime = wallTime
	e.lastRecoveryAt = recoveredAt
	e.mu.Unlock()
}

// SetNoLockUntil sets the post-warp landing grace deadline: NPCs must not
// lock this entity until the given time (the client is still playing the
// warp-deceleration slide after the server exits warp mode).
func (e *Entity) SetNoLockUntil(t time.Time) {
	e.mu.Lock()
	e.noLockUntil = t
	e.mu.Unlock()
}

// InLandingGrace reports whether the entity is still inside its post-warp
// landing grace window. Thread-safe.
func (e *Entity) InLandingGrace() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return time.Now().Before(e.noLockUntil)
}

// IsInWarp returns true if the entity's destiny manager is in warp mode.
// Thread-safe. Used by NPC AI to skip warping entities in their WorldView.
func (e *Entity) IsInWarp() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.Manager == nil {
		return false
	}
	return e.Manager.CurrentBallMode() == ModeWarp
}

// SetWarpPending marks (or clears) the alignment-phase warp flag. Set true
// after SendWarpTo, cleared when the server enters ModeWarp or warp is
// canceled. Thread-safe.
func (e *Entity) SetWarpPending(v bool) {
	e.mu.Lock()
	e.warpPending = v
	e.mu.Unlock()
}

// IsWarpPending reports whether the entity is in the alignment gap between
// the client receiving WarpTo and the server entering warp mode. Thread-safe.
func (e *Entity) IsWarpPending() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.warpPending
}

// SetCloaked sets (or clears) the gate/module cloak flag. Thread-safe.
func (e *Entity) SetCloaked(v bool) {
	e.mu.Lock()
	e.cloaked = v
	e.mu.Unlock()
}

// IsCloaked reports whether the entity is currently cloaked. Thread-safe.
func (e *Entity) IsCloaked() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cloaked
}

// SetModuleCloak records modID as the running cloaking device and sets the cloaked flag with
// it, so every picker and ensureBallsKnown treat the ship as cloaked. Thread-safe.
func (e *Entity) SetModuleCloak(modID int64) {
	e.mu.Lock()
	e.moduleCloak = modID
	e.cloaked = modID != 0 || e.cloaked
	e.mu.Unlock()
}

// ModuleCloak returns the item ID of the cloaking device holding the cloak, 0 for none.
// Thread-safe.
func (e *Entity) ModuleCloak() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.moduleCloak
}

// EndModuleCloak releases the cloak held by modID and reports whether it was the holder, so
// two goroutines ending the same cloak run the reveal once. The cloaked flag stays set: the
// reveal clears it as it tells the clients. Thread-safe.
func (e *Entity) EndModuleCloak(modID int64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if modID == 0 || e.moduleCloak != modID {
		return false
	}
	e.moduleCloak = 0
	return true
}

// GMGhostRadius is the radius (m) a ghosted GM-cloak ball carries on the wire.
// Not zero: destiny.dll divides by radius, and a zero-radius ball crashes the
// client at lock time.
const GMGhostRadius = 1.0

// SetGMCloak turns the GM standing cloak on or off. On sets the ordinary
// cloaked flag too, so every picker, ensureBallsKnown and the ball's cloak bit
// treat the pilot as cloaked with no further change; off clears both. ghost
// puts GMGhostRadius on the wire (see BallDesc). Thread-safe.
func (e *Entity) SetGMCloak(on, ghost bool) {
	e.mu.Lock()
	e.gmCloak = on
	e.gmGhost = on && ghost
	e.cloaked = on
	e.mu.Unlock()
}

// IsGMCloak reports whether the standing GM cloak is on. Thread-safe.
func (e *Entity) IsGMCloak() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.gmCloak
}

// IsGMGhost reports whether the GM cloak is ghosted (tiny wire radius).
// Thread-safe.
func (e *Entity) IsGMGhost() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.gmCloak && e.gmGhost
}

// SetGMMode sets (or clears) GM mode: /gm on. See the gmMode field.
// Thread-safe.
func (e *Entity) SetGMMode(v bool) {
	e.mu.Lock()
	e.gmMode = v
	e.mu.Unlock()
}

// IsGMMode reports whether the entity is in GM mode. Thread-safe.
func (e *Entity) IsGMMode() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.gmMode
}

// IsUntargetable reports whether nothing (player, drone or NPC) may lock,
// approach or shoot this entity: landing grace, a cloak, or GM mode. Not
// folded into InLandingGrace, which is also asked about the witness of a
// damage push. Warp state is asked separately, since callers act on it.
func (e *Entity) IsUntargetable() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.gmMode || e.cloaked || time.Now().Before(e.noLockUntil)
}

// SetInvulnerable marks the entity as indestructible scenery. Thread-safe.
func (e *Entity) SetInvulnerable(v bool) {
	e.mu.Lock()
	e.invulnerable = v
	e.mu.Unlock()
}

// SetShieldFloor arms (frac > 0) or clears (frac <= 0) the shield floor.
// Thread-safe.
func (e *Entity) SetShieldFloor(frac float64) {
	e.mu.Lock()
	e.shieldFloor = frac
	if frac <= 0 {
		e.floorHit = false
	}
	e.mu.Unlock()
}

// ShieldFloor returns the armed floor fraction (0 = none). Thread-safe.
func (e *Entity) ShieldFloor() float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.shieldFloor
}

// TakeShieldFloorHit reports once that damage stopped at the shield floor
// since the last call. Thread-safe.
func (e *Entity) TakeShieldFloorHit() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	hit := e.floorHit
	e.floorHit = false
	return hit
}

// IsInvulnerable reports whether ApplyDamage will refuse to hurt this entity.
// Thread-safe.
func (e *Entity) IsInvulnerable() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.invulnerable
}

// SetSpeedModifier installs a multiplicative max-velocity modifier on this
// entity's destiny manager (web debuff, prop-mod boost), keyed by the source
// module itemID. Returns the new effective max velocity. Thread-safe.
func (e *Entity) SetSpeedModifier(key int64, mult float64) float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.Manager == nil {
		return 0
	}
	return e.Manager.SetSpeedModifier(key, mult)
}

// RemoveSpeedModifier removes a speed modifier by source module itemID.
// Returns the new effective max velocity and whether anything was removed.
// Thread-safe.
func (e *Entity) RemoveSpeedModifier(key int64) (float64, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.Manager == nil {
		return 0, false
	}
	return e.Manager.RemoveSpeedModifier(key)
}

// EffectiveMaxVelocity returns the entity's current max velocity after all
// speed modifiers. Thread-safe.
func (e *Entity) EffectiveMaxVelocity() float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.Manager == nil {
		return 0
	}
	return e.Manager.EffectiveMaxVelocity()
}

// AddWarpScramble records warp-scramble strength applied by a module.
// Thread-safe.
func (e *Entity) AddWarpScramble(key int64, strength float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.scramblers == nil {
		e.scramblers = make(map[int64]float64)
	}
	e.scramblers[key] = strength
}

// RemoveWarpScramble removes a module's warp-scramble contribution.
// Returns whether anything was removed. Thread-safe.
func (e *Entity) RemoveWarpScramble(key int64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.scramblers[key]; !ok {
		return false
	}
	delete(e.scramblers, key)
	return true
}

// WarpScrambleTotal returns the summed scramble strength currently applied
// to this entity. > 0 means warp is blocked. Thread-safe.
func (e *Entity) WarpScrambleTotal() float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	total := 0.0
	for _, s := range e.scramblers {
		total += s
	}
	return total
}

// EwarDebuff holds the multipliers one hostile EWAR module applies to this
// entity for the current cycle. A zero field means "no change" and is treated
// as 1.0 when combined.
type EwarDebuff struct {
	TurretRangeMult    float64 // tracking disruptor: turret optimal range x
	TurretTrackingMult float64 // tracking disruptor: turret tracking speed x
	MaxTargetRangeMult float64 // sensor damp: max targeting range x
	ScanResolutionMult float64 // sensor damp: scan resolution x
	SigRadiusMult      float64 // target painter: signature radius x
}

// SetEwarDebuff installs (or refreshes) an EWAR debuff keyed by the hostile
// module's itemID. Thread-safe.
func (e *Entity) SetEwarDebuff(key int64, d EwarDebuff) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ewarDebuffs == nil {
		e.ewarDebuffs = make(map[int64]EwarDebuff)
	}
	e.ewarDebuffs[key] = d
}

// RemoveEwarDebuff removes a module's EWAR debuff. Returns whether anything
// was removed. Thread-safe.
func (e *Entity) RemoveEwarDebuff(key int64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.ewarDebuffs[key]; !ok {
		return false
	}
	delete(e.ewarDebuffs, key)
	return true
}

// EwarTotals returns the combined (multiplied) EWAR debuffs currently applied
// to this entity, with every field defaulting to 1.0. Thread-safe.
func (e *Entity) EwarTotals() EwarDebuff {
	e.mu.Lock()
	defer e.mu.Unlock()
	total := EwarDebuff{TurretRangeMult: 1, TurretTrackingMult: 1, MaxTargetRangeMult: 1, ScanResolutionMult: 1, SigRadiusMult: 1}
	for _, d := range e.ewarDebuffs {
		if d.TurretRangeMult > 0 {
			total.TurretRangeMult *= d.TurretRangeMult
		}
		if d.TurretTrackingMult > 0 {
			total.TurretTrackingMult *= d.TurretTrackingMult
		}
		if d.MaxTargetRangeMult > 0 {
			total.MaxTargetRangeMult *= d.MaxTargetRangeMult
		}
		if d.ScanResolutionMult > 0 {
			total.ScanResolutionMult *= d.ScanResolutionMult
		}
		if d.SigRadiusMult > 0 {
			total.SigRadiusMult *= d.SigRadiusMult
		}
	}
	return total
}

// GangBoost returns the gang bonus currently applied (identity when none).
// Thread-safe.
func (e *Entity) GangBoost() dogma.GangBoost {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.gangBoost
}

// ApplyGangBoost installs a new gang bonus and rescales the state that holds
// the old one baked in (HP pools and resonances) by new/old. Current HP moves
// by the same ratio, so the pool's fraction is preserved: joining a gang
// mid-fight heals nobody. Returns whether anything changed.
func (e *Entity) ApplyGangBoost(b dogma.GangBoost) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	b = b.Normalized()
	old := e.gangBoost.Normalized()
	if b == old {
		return false
	}
	r := b.Ratio(old)
	if s := r.Mult(dogma.GSShieldHP); s != 1 && e.HP.MaxShield > 0 {
		e.HP.MaxShield *= s
		e.HP.CurShield *= s
	}
	if a := r.Mult(dogma.GSArmorHP); a != 1 && e.HP.MaxArmor > 0 {
		e.HP.MaxArmor *= a
		e.HP.CurArmor *= a
	}
	// Resonances are baked the same way (Passive Defense, Shield
	// Harmonizing); SetBaseResists re-bakes them when the modules change.
	for d := 0; d < 4; d++ {
		e.Resists[0][d] *= r.Mult(dogma.GSShieldResonance)
		e.Resists[1][d] *= r.Mult(dogma.GSArmorResonance)
	}
	e.gangBoost = b
	return true
}

// SetBaseResists installs resonances rebuilt from the ship's hull and modules
// and multiplies in the gang bonus under the same lock. A caller that read
// GangBoost() and then wrote could land after a recompute had rescaled the
// old values, and bake in a boost the entity no longer records.
func (e *Entity) SetBaseResists(r [3][4]float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for d := 0; d < 4; d++ {
		r[0][d] *= e.gangBoost.Mult(dogma.GSShieldResonance)
		r[1][d] *= e.gangBoost.Mult(dogma.GSArmorResonance)
	}
	e.Resists = r
}

// SigRadiusMult is every live multiplier on this entity's signature radius:
// target painters on it (EwarTotals), Evasive Maneuvers in its gang, and the
// standing modifier (a control tower's Stealth Emitter Arrays).
// Thread-safe.
func (e *Entity) SigRadiusMult() float64 {
	m := e.EwarTotals().SigRadiusMult * e.GangBoost().Mult(dogma.GSSigRadius)
	e.mu.Lock()
	if e.sigMod > 0 {
		m *= e.sigMod
	}
	if e.implantSig > 0 {
		m *= e.implantSig
	}
	e.mu.Unlock()
	return m
}

// SetImplantSigRadiusMult sets the pilot's implant signature term (1 = none).
// Thread-safe.
func (e *Entity) SetImplantSigRadiusMult(m float64) {
	e.mu.Lock()
	e.implantSig = m
	e.mu.Unlock()
}

// SetSigRadiusModifier sets the standing signature multiplier (1 = none);
// read by SigRadiusMult at every signature reader. Thread-safe.
func (e *Entity) SetSigRadiusModifier(m float64) {
	e.mu.Lock()
	e.sigMod = m
	e.mu.Unlock()
}

// GetNPCStanding returns the player's standing toward a given NPC factionID.
// Returns the standing value and whether an entry exists. Thread-safe.
func (e *Entity) GetNPCStanding(factionID int64) (float64, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.NPCStandings == nil {
		return 0, false
	}
	v, ok := e.NPCStandings[factionID]
	return v, ok
}

// SetNPCStanding updates the in-memory NPC faction standing cache so that
// hostility changes take effect immediately without re-login. Thread-safe.
func (e *Entity) SetNPCStanding(factionID int64, value float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.NPCStandings == nil {
		e.NPCStandings = make(map[int64]float64)
	}
	e.NPCStandings[factionID] = value
}

// Pos returns the entity's current position, safe for concurrent access.
func (e *Entity) Pos() Vec3 {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.Manager == nil {
		return Vec3{}
	}
	return e.Manager.Pos
}

// Footprint returns the entity's collision geometry and whether it has one.
// Only a free ball does: the client overlap-resolves free balls against one
// another and leaves rigid ones (structures, celestials, asteroids) alone. A
// ball mid-warp reports none, whatever its manager's position says. Thread-safe.
func (e *Entity) Footprint() (BallFootprint, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.Manager == nil || e.Manager.maxShipVelocity <= 0 {
		return BallFootprint{}, false
	}
	if e.warpPending || e.Manager.CurrentBallMode() == ModeWarp {
		return BallFootprint{}, false
	}
	return BallFootprint{Pos: e.Manager.Pos, Radius: e.Manager.radius}, true
}

// IsChasing reports whether the entity's server nav is target-relative
// (Follow/Orbit). When the orbited player is itself chasing, server and client
// run mutually referential simulations that drift apart. Thread-safe.
func (e *Entity) IsChasing() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.Manager != nil && e.Manager.IsChasing()
}

// PredictedPos returns the position extrapolated ahead seconds along the
// current velocity. michelle applies a stamped packet when its sim clock
// reaches the stamp, so a position payload is built for where the ball will be
// at apply time. Thread-safe. See docs/destiny-stamps.md#death-snap-lead.
func (e *Entity) PredictedPos(ahead float64) Vec3 {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.Manager == nil {
		return Vec3{}
	}
	p, v := e.Manager.Pos, e.Manager.Vel
	return Vec3{X: p.X + v.X*ahead, Y: p.Y + v.Y*ahead, Z: p.Z + v.Z*ahead}
}

// SimulateAheadPos steps a copy of the entity's Manager forward by ahead
// seconds with the server integrator and returns where it ends up; lookup
// supplies a Follow/Orbit target's position at each step's elapsed time. The
// copy is stepped with e.mu released, so lookup may take other entities' locks.
func (e *Entity) SimulateAheadPos(ahead float64, lookup func(id uint32, elapsed float64) (Vec3, bool)) Vec3 {
	e.mu.Lock()
	if e.Manager == nil {
		e.mu.Unlock()
		return Vec3{}
	}
	sim := *e.Manager
	e.mu.Unlock()
	if ahead <= 0 {
		return sim.Pos
	}
	const dt = 0.1
	elapsed := 0.0
	sim.TargetLookup = func(id uint32) (Vec3, bool) {
		if lookup == nil {
			return Vec3{}, false
		}
		return lookup(id, elapsed)
	}
	for elapsed < ahead {
		step := math.Min(dt, ahead-elapsed)
		sim.Step(step)
		elapsed += step
	}
	return sim.Pos
}

// NextStampExcess reports how many seconds past the wall clock the next stamp
// minted on this stream would sit: 0 on the wall, n when earlier bursts pushed
// the floor n stamps ahead. michelle applies the packet that much later than
// a fresh one. Thread-safe. See docs/destiny-stamps.md#death-snap-lead.
func (e *Entity) NextStampExcess() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	var candidate uint32
	if !e.BaseTime.IsZero() {
		candidate = e.BaseStamp + uint32(time.Since(e.BaseTime).Seconds()) + 1
	} else {
		candidate = e.BaseStamp + 1
	}
	floor := e.lastStamp
	if e.Manager != nil && e.Manager.Stamp > floor {
		floor = e.Manager.Stamp
	}
	if floor < candidate {
		return 0
	}
	return int(floor) + 1 - int(candidate)
}

// BallDesc returns a snapshot of the entity's ball descriptor, safe for
// concurrent access. The caller should fill CorpID/Name before encoding.
func (e *Entity) BallDesc() BallDesc {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.Manager == nil {
		return BallDesc{EntityID: e.ID}
	}
	bd := e.Manager.BallDesc()
	bd.Cloaked = e.cloaked
	// Wire only: the Manager keeps the real radius, which warp exits, arrival
	// lanes and the server's own sims still read.
	if e.gmCloak && e.gmGhost {
		bd.Radius = GMGhostRadius
	}
	return bd
}

// BallName returns the name encoded in this entity's destiny ball: the ship's
// custom item name, else CharName (NPCs, pods, unnamed ships). The client hints
// cfg.evelocations with ball.name, which hangar and info windows display.
func (e *Entity) BallName() string {
	if e.ShipName != "" {
		return e.ShipName
	}
	return e.CharName
}

// SetBaseStamp sets the stamp baseline after a SetState: NextStamp's anchor
// and the StampLead wall anchor both, since a SetState re-aligns the client's
// sim clock with the stream. The recovery debt is cleared.
func (e *Entity) SetBaseStamp(stamp uint32, t time.Time) {
	e.mu.Lock()
	e.BaseStamp = stamp
	e.BaseTime = t
	e.wallBaseStamp = stamp
	e.wallBaseTime = t
	e.lastRecoveryAt = time.Time{} // both anchors agree again: no debt
	e.mu.Unlock()
}

// SetActiveEffect records a module effect as active on this entity.
func (e *Entity) SetActiveEffect(modItemID int64, ae ActiveEffect) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ActiveEffects == nil {
		e.ActiveEffects = make(map[int64]ActiveEffect)
	}
	e.ActiveEffects[modItemID] = ae
}

// ClearActiveEffect removes a module effect from this entity.
func (e *Entity) ClearActiveEffect(modItemID int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.ActiveEffects, modItemID)
}

// ClearAllActiveEffects removes all active effects from this entity.
func (e *Entity) ClearAllActiveEffects() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ActiveEffects = nil
}

// AddAggressor records an entity as actively attacking this entity.
func (e *Entity) AddAggressor(attackerID uint32, timestamp int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.Aggressors == nil {
		e.Aggressors = make(map[uint32]int64)
	}
	e.Aggressors[attackerID] = timestamp
}

// ClearAggressor removes a single aggressor from this entity.
func (e *Entity) ClearAggressor(attackerID uint32) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.Aggressors, attackerID)
}

// ClearAggressors removes all aggressors from this entity.
func (e *Entity) ClearAggressors() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.Aggressors = nil
}

// PruneAggressors drops aggression entries older than maxAge and reports how
// many went; 0 for an entity with nothing recorded. Nothing else removes a
// player's or drone's entry, and the map is encoded into every SetState.
func (e *Entity) PruneAggressors(maxAge time.Duration) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.Aggressors) == 0 {
		return 0
	}
	cutoff := time.Now().Add(-maxAge).Unix()
	n := 0
	for id, at := range e.Aggressors {
		if at < cutoff {
			delete(e.Aggressors, id)
			n++
		}
	}
	if len(e.Aggressors) == 0 {
		e.Aggressors = nil
	}
	return n
}

// ClearKillHits drops the per-attacker damage record. killHits is written on
// every hit and read once, at death, so a long-lived entity otherwise holds
// one KillHit per attacker for its whole life.
func (e *Entity) ClearKillHits() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.killHits = nil
}

// GetAggressors returns a snapshot of the aggressors map, safe for
// concurrent use.
func (e *Entity) GetAggressors() map[uint32]int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.Aggressors) == 0 {
		return nil
	}
	out := make(map[uint32]int64, len(e.Aggressors))
	for k, v := range e.Aggressors {
		out[k] = v
	}
	return out
}

// KillHit is a snapshot of one attacker's involvement in the destruction of
// this entity, captured at hit time so the death path can build the CONCORD
// kill report even after the attacker warps off or logs out.
type KillHit struct {
	Name         string  // attacker character name ("" → resolve at mail time)
	CharID       int64   // 0 for NPCs
	CorpID       int64   // player corp, or NPC faction corp
	AllianceID   int64   // 0 = no alliance
	SecStatus    float64 // attacker security status
	ShipTypeID   int64   // attacker's ship (or NPC/drone) typeID
	WeaponTypeID int64   // module/charge/drone typeID that dealt the hit
	Damage       float64 // total damage this attacker has dealt to this victim
	IsNPC        bool
	FinalBlow    bool
	Time         int64 // unix seconds of this attacker's most recent hit
}

// RecordKillHit stores/updates the attacker's kill-report snapshot on this
// (victim) entity. Called from every damage path right after ApplyDamage;
// damage is that call's applied return and finalBlow its destroyed return,
// which fires exactly once per entity. Thread-safe.
func (e *Entity) RecordKillHit(attacker *Entity, weaponTypeID int64, damage float64, finalBlow bool) {
	if attacker == nil {
		return
	}
	e.RecordKillHitRaw(int64(attacker.ID), KillHit{
		Name:         attacker.CharName,
		CharID:       attacker.Slim.CharID,
		CorpID:       attacker.Slim.CorpID,
		AllianceID:   attacker.Slim.AllianceID,
		SecStatus:    attacker.Slim.SecurityStatus,
		ShipTypeID:   attacker.Slim.TypeID,
		WeaponTypeID: weaponTypeID,
		Damage:       damage,
		IsNPC:        !attacker.IsPlayer,
		FinalBlow:    finalBlow,
	})
	ObserveCombat(nil, e, attacker, damage, finalBlow)
}

// RecordKillHitRaw merges one hit into this attacker's kill-report snapshot.
// Damage accumulates, a recorded final blow is sticky, and a field the new hit
// does not carry is inherited (the drone fallback path records a bare charID).
// Every caller must build a fresh KillHit per hit, or Damage double-counts.
func (e *Entity) RecordKillHitRaw(key int64, hit KillHit) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.killHits == nil {
		e.killHits = make(map[int64]KillHit)
	}
	if prev, ok := e.killHits[key]; ok {
		hit.Damage += prev.Damage
		if prev.FinalBlow {
			hit.FinalBlow = true
		}
		if hit.Name == "" {
			hit.Name = prev.Name
		}
		if hit.CharID == 0 {
			hit.CharID = prev.CharID
		}
		if hit.CorpID == 0 {
			hit.CorpID = prev.CorpID
		}
		if hit.AllianceID == 0 {
			hit.AllianceID = prev.AllianceID
		}
		if hit.SecStatus == 0 {
			hit.SecStatus = prev.SecStatus
		}
		if hit.ShipTypeID == 0 {
			hit.ShipTypeID = prev.ShipTypeID
		}
		if hit.WeaponTypeID == 0 {
			hit.WeaponTypeID = prev.WeaponTypeID
		}
	}
	if hit.Time == 0 {
		hit.Time = time.Now().Unix()
	}
	e.killHits[key] = hit
}

// KillHitsSnapshot returns all recorded attackers, final blow first, the rest
// ordered most-recent-hit first. Safe for concurrent use.
func (e *Entity) KillHitsSnapshot() []KillHit {
	e.mu.Lock()
	out := make([]KillHit, 0, len(e.killHits))
	for _, h := range e.killHits {
		out = append(out, h)
	}
	e.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].FinalBlow != out[j].FinalBlow {
			return out[i].FinalBlow
		}
		return out[i].Time > out[j].Time
	})
	return out
}

// GetActiveEffects returns a snapshot of all active effects, safe for
// concurrent use.
func (e *Entity) GetActiveEffects() []ActiveEffect {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.ActiveEffects) == 0 {
		return nil
	}
	out := make([]ActiveEffect, 0, len(e.ActiveEffects))
	for _, ae := range e.ActiveEffects {
		out = append(out, ae)
	}
	return out
}

// HPSnapshot returns a copy of the entity's current HP values, safe for
// concurrent access.
func (e *Entity) HPSnapshot() HPState {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.HP
}

// SetHP replaces the entity's HP state wholesale, under the same lock the tick
// loop's reads take: a persisted NPC's remaining health is restored after the
// ball is already in the system.
func (e *Entity) SetHP(hp HPState) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.HP = hp
}

// HasHP returns true if this entity has mutable HP tracking enabled.
func (e *Entity) HasHP() bool {
	return e.HP.MaxStructure > 0
}

// IsDestroyed returns true if structure HP has reached zero. Thread-safe.
func (e *Entity) IsDestroyed() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.HP.MaxStructure > 0 && e.HP.CurStructure <= 0
}

// ApplyDamage applies raw damage (after resistance) to the entity's HP layers
// in order: shield → armor → structure. Returns total damage actually applied
// and whether the entity was destroyed. Thread-safe.
func (e *Entity) ApplyDamage(em, explosive, kinetic, thermal float64) (applied float64, destroyed bool) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.HP.MaxStructure <= 0 {
		return 0, false // no HP tracking
	}
	if e.invulnerable {
		// Authored scenery: absorbs the shot and loses nothing. Zero applied makes
		// the caller's damage-state broadcast and kill bookkeeping no-ops.
		return 0, false
	}
	wasAlive := e.HP.CurStructure > 0

	// Shield floor: the shield may only lose what sits above the floor; the rest
	// is absorbed rather than carried into armor. See shieldFloor.
	if e.shieldFloor > 0 && e.HP.MaxShield > 0 {
		floor := e.shieldFloor * e.HP.MaxShield
		if e.HP.CurShield <= floor+1e-9 {
			e.floorHit = true
			return 0, false
		}
		var layerDmg float64
		raw := [4]float64{em, explosive, kinetic, thermal}
		for i, d := range raw {
			layerDmg += d * e.Resists[0][i]
		}
		if e.HP.CurShield-layerDmg <= floor {
			applied = e.HP.CurShield - floor
			e.HP.CurShield = floor
			e.floorHit = true
			return applied, false
		}
	}

	// Apply damage to each layer in order.
	layers := []struct {
		cur  *float64
		max  float64
		res  [4]float64 // em, expl, kin, therm resonance
	}{
		{&e.HP.CurShield, e.HP.MaxShield, e.Resists[0]},
		{&e.HP.CurArmor, e.HP.MaxArmor, e.Resists[1]},
		{&e.HP.CurStructure, e.HP.MaxStructure, e.Resists[2]},
	}

	rawDmg := [4]float64{em, explosive, kinetic, thermal}

	for _, layer := range layers {
		if *layer.cur <= 0 {
			continue
		}
		// Calculate effective damage against this layer's resistances.
		var layerDmg float64
		for i, d := range rawDmg {
			layerDmg += d * layer.res[i]
		}
		if layerDmg <= 0 {
			continue
		}
		if layerDmg >= *layer.cur {
			applied += *layer.cur
			// Overflow passes through to the next layer.
			overflow := layerDmg - *layer.cur
			*layer.cur = 0
			// Scale remaining raw damage by overflow ratio.
			if layerDmg > 0 {
				ratio := overflow / layerDmg
				for i := range rawDmg {
					rawDmg[i] *= ratio
				}
			}
		} else {
			*layer.cur -= layerDmg
			applied += layerDmg
			// All damage absorbed: zero remaining.
			for i := range rawDmg {
				rawDmg[i] = 0
			}
			break
		}
	}

	// Report destroyed only on the alive-to-dead transition, so concurrent hits
	// (two missiles landing the same tick) cannot trigger the death flow twice.
	destroyed = wasAlive && e.HP.CurStructure <= 0
	return applied, destroyed
}

// DamageStateFrac returns the current DamageState as fractions (0.0-1.0).
// Thread-safe.
func (e *Entity) DamageStateFrac() DamageState {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.HP.MaxStructure <= 0 {
		return e.Damage // static fallback
	}
	shield := 1.0
	if e.HP.MaxShield > 0 {
		shield = e.HP.CurShield / e.HP.MaxShield
	}
	armor := 1.0
	if e.HP.MaxArmor > 0 {
		armor = e.HP.CurArmor / e.HP.MaxArmor
	}
	structure := 1.0
	if e.HP.MaxStructure > 0 {
		structure = e.HP.CurStructure / e.HP.MaxStructure
	}
	// Wire tau is in client dogma units (ms / 5): michelle.GetDamageState divides
	// elapsed 100 ns ticks by dgmTauConstant and godma stores taus as ms / 5.
	tau := e.HP.ShieldTau * 1000.0 / 5.0
	if e.HP.ShieldTau <= 0 {
		tau = 1e9 // effectively no visible regen
	}
	return DamageState{
		Shield:    shield,
		Tau:       tau,
		Timestamp: 0, // caller should set to current win32 time
		Armor:     armor,
		Structure: structure,
	}
}

// RegenShield applies the shield regeneration formula over dt seconds:
// S(t) = Smax * (1 - (1 - sqrt(S0/Smax)) * exp(-dt / tau))^2, with tau =
// ShieldTau / 5 (the attribute is the total recharge time). Returns true if
// the shield changed enough to broadcast. Thread-safe.
func (e *Entity) RegenShield(dt float64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.HP.MaxShield <= 0 || e.HP.MaxStructure <= 0 {
		return false // no HP tracking
	}
	if e.HP.CurShield >= e.HP.MaxShield {
		return false // already full
	}
	if e.HP.CurStructure <= 0 {
		return false // destroyed
	}

	tau := e.HP.ShieldTau / 5.0
	if tau <= 0 {
		tau = 250.0 // fallback: 1250s total / 5
	}

	s0 := e.HP.CurShield
	sMax := e.HP.MaxShield

	sqrtFrac := math.Sqrt(s0 / sMax)
	decay := math.Exp(-dt / tau)
	newFrac := (1.0 - (1.0-sqrtFrac)*decay)
	newShield := sMax * newFrac * newFrac

	if newShield > sMax {
		newShield = sMax
	}
	if newShield < s0 {
		return false // shouldn't happen, but guard
	}
	// Only broadcast if change is meaningful (> 0.1% of max).
	if newShield-s0 < sMax*0.001 {
		return false
	}

	e.HP.CurShield = newShield
	return true
}

// RepairShield adds HP to the shield layer, clamped to max. Returns the
// updated HPState snapshot. Thread-safe.
func (e *Entity) RepairShield(amount float64) HPState {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.HP.CurShield += amount
	if e.HP.CurShield > e.HP.MaxShield {
		e.HP.CurShield = e.HP.MaxShield
	}
	return e.HP
}

// RepairArmor adds HP to the armor layer, clamped to max. Returns the
// updated HPState snapshot. Thread-safe.
func (e *Entity) RepairArmor(amount float64) HPState {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.HP.CurArmor += amount
	if e.HP.CurArmor > e.HP.MaxArmor {
		e.HP.CurArmor = e.HP.MaxArmor
	}
	return e.HP
}

// RepairStructure adds HP to the structure/hull layer, clamped to max.
// Returns the updated HPState snapshot. Thread-safe.
func (e *Entity) RepairStructure(amount float64) HPState {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.HP.CurStructure += amount
	if e.HP.CurStructure > e.HP.MaxStructure {
		e.HP.CurStructure = e.HP.MaxStructure
	}
	return e.HP
}

// ── Capacitor ────────────────────────────────────────────────────────────────
//
// The capacitor is evaluated lazily: the charge is extrapolated on demand with
// the client's exponential recharge formula for charged attributes,
// E(t) = Emax * (1 + (sqrt(E0/Emax) - 1) * exp(-dt/tau))^2, tau = rechargeMs/5.

// InitCap initializes the capacitor at full charge. maxCap is
// capacitorCapacity, rechargeMs the rechargeRate attribute in milliseconds.
// Thread-safe.
func (e *Entity) InitCap(maxCap, rechargeMs float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.capMax = maxCap
	e.capTau = rechargeMs / 5.0 / 1000.0 // seconds
	if e.capTau <= 0 {
		e.capTau = 25.0 // 125s recharge fallback
	}
	e.capVal = maxCap
	e.capTime = time.Now()
}

// SetCapFraction sets the capacitor charge to frac of its maximum, clamped to 0..1, and
// restarts the recharge curve from there. No-op on an uninitialized capacitor. Thread-safe.
func (e *Entity) SetCapFraction(frac float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.capMax <= 0 {
		return
	}
	if frac < 0 {
		frac = 0
	} else if frac > 1 {
		frac = 1
	}
	e.capVal = e.capMax * frac
	e.capTime = time.Now()
}

// HasCap reports whether the capacitor has been initialized. Thread-safe.
func (e *Entity) HasCap() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.capMax > 0
}

// capNow extrapolates the capacitor to the current time. Caller holds e.mu.
func (e *Entity) capNow() float64 {
	if e.capMax <= 0 {
		return 0
	}
	dt := time.Since(e.capTime).Seconds()
	sqrtFrac := math.Sqrt(e.capVal / e.capMax)
	f := 1.0 + (sqrtFrac-1.0)*math.Exp(-dt/e.capTau)
	v := e.capMax * f * f
	if v > e.capMax {
		v = e.capMax
	}
	return v
}

// CurrentCap returns the extrapolated capacitor charge and its maximum.
// Thread-safe.
func (e *Entity) CurrentCap() (cur, max float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.capNow(), e.capMax
}

// BoostCap adds amount to the capacitor (capacitor booster charge),
// clamped to max. Returns the new charge; no-op on uninitialized
// capacitors. Thread-safe.
func (e *Entity) BoostCap(amount float64) float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.capMax <= 0 {
		return 0
	}
	v := e.capNow() + amount
	if v > e.capMax {
		v = e.capMax
	}
	e.capVal = v
	e.capTime = time.Now()
	return v
}

// ConsumeCap deducts amount from the capacitor if enough charge is
// available. Returns the post-deduction charge and whether the deduction
// happened. An uninitialized capacitor always succeeds (cap-less entities,
// e.g. NPCs, are not throttled). Thread-safe.
func (e *Entity) ConsumeCap(amount float64) (newVal float64, ok bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.capMax <= 0 {
		return 0, true
	}
	cur := e.capNow()
	if amount > cur {
		// Re-stamp so the recharge curve continues from the observed value.
		e.capVal = cur
		e.capTime = time.Now()
		return cur, false
	}
	e.capVal = cur - amount
	e.capTime = time.Now()
	return e.capVal, true
}

// NextStamp returns a strictly increasing stamp for destiny updates to this
// entity's connection: the wall-clock candidate from the base anchor, or
// floor+1 when that is taken. A floor 2 or more past the candidate re-anchors
// the base (recovery). See docs/destiny-stamps.md#stamps-and-the-client-clock.
func (e *Entity) NextStamp() uint32 {
	e.mu.Lock()
	defer e.mu.Unlock()

	// Wall-clock candidate from the base anchor.
	var candidate uint32
	if !e.BaseTime.IsZero() {
		elapsed := uint32(time.Since(e.BaseTime).Seconds())
		candidate = e.BaseStamp + elapsed + 1
	} else {
		candidate = e.BaseStamp + 1
	}

	// Strict monotonicity: never reuse or regress. Each SendStamped call
	// gets its own stamp, even within the same wall-clock second.
	floor := e.lastStamp
	if e.Manager != nil && e.Manager.Stamp > floor {
		floor = e.Manager.Stamp
	}
	recovered := false
	if candidate <= floor {
		wallCandidate := candidate // save before modification
		candidate = floor + 1
		// Re-anchor when pushed 2 or more past the wall-clock candidate, so later
		// stamps track the advanced sim clock instead of re-pushing every call.
		if floor >= wallCandidate+2 {
			e.BaseStamp = floor
			e.BaseTime = time.Now()
			e.lastRecoveryAt = e.BaseTime
			recovered = true
		}
	}

	e.lastStamp = candidate
	if e.Manager != nil {
		e.Manager.Stamp = candidate
	}
	e.recordStamp(candidate, recovered)
	if recovered {
		e.logRecoveryLocked(floor, candidate)
	}
	return candidate
}

// recoveryLogEvery bounds how often an entity reports overshoot recovery;
// each report carries the recent rate and the top spenders.
const recoveryLogEvery = 10 * time.Second

// stampRateWindow is the span the overshoot report measures its rate over,
// read from the audit ring; a session average cannot show a burst.
const stampRateWindow = 10 * time.Second

// recentStampRateLocked reports mints per second over the trailing window, read
// out of the audit ring. When every entry the ring holds falls inside the
// window the count is a floor and the rate is computed over the span the ring
// covers; saturated says which. Caller holds e.mu.
func (e *Entity) recentStampRateLocked(now time.Time, window time.Duration) (rate float64, n int, saturated bool) {
	if e.stampLog == nil || e.stampMints == 0 {
		return 0, 0, false
	}
	held := int(e.stampLogIdx)
	if held > len(e.stampLog) {
		held = len(e.stampLog)
	}
	cutoff := now.Add(-window)
	oldest := now
	for i := 0; i < held; i++ {
		at := e.stampLog[i].at
		if at.Before(cutoff) {
			continue
		}
		n++
		if at.Before(oldest) {
			oldest = at
		}
	}
	if n == 0 {
		return 0, 0, false
	}
	saturated = n == held && held == len(e.stampLog)
	span := window.Seconds()
	if saturated {
		if s := now.Sub(oldest).Seconds(); s > 0 {
			span = s
		}
	}
	if span <= 0 {
		return 0, n, saturated
	}
	return float64(n) / span, n, saturated
}

// logRecoveryLocked prints a rate-limited overshoot report. Caller holds e.mu.
func (e *Entity) logRecoveryLocked(floor, minted uint32) {
	now := time.Now()
	if !e.lastRecoveryLog.IsZero() && now.Sub(e.lastRecoveryLog) < recoveryLogEvery {
		return
	}
	e.lastRecoveryLog = now

	rate, n, saturated := e.recentStampRateLocked(now, stampRateWindow)
	note := ""
	if saturated {
		note = " [ring full — floor]"
	}
	// Report the lead, which is what fired this; the rate answers a different
	// question and may read under budget while the lead is still carried.
	cadence := "never"
	if !e.lastStampedFlushAt.IsZero() {
		cadence = fmt.Sprintf("%.1fs ago", now.Sub(e.lastStampedFlushAt).Seconds())
	}
	log.Printf("[stamp] entity=%d lead=%+d (recovery at floor=%d minting=%d): %.2f stamps/s over last %s (%d mints%s, budget 1.00), last grid step %s — top spenders: %s",
		e.ID, e.stampLead(), floor, minted, rate, stampRateWindow, n, note, cadence, e.topStampSourcesLocked(3))
}

// topStampSourcesLocked renders the n call sites that minted most, worst first.
// Caller holds e.mu. Same data StampReport serves to /stamps, cut down to the
// one line a log can carry.
func (e *Entity) topStampSourcesLocked(n int) string {
	return topSources(e.stampSources, n)
}

// topOverSourcesLocked is topStampSourcesLocked over the ceiling-breaching
// mints instead of all of them. Caller holds e.mu.
func (e *Entity) topOverSourcesLocked(n int) string {
	return topSources(e.stampOverSources, n)
}

func topSources(counts map[string]int, n int) string {
	if len(counts) == 0 {
		return "(none recorded)"
	}
	type srcCount struct {
		src string
		n   int
	}
	all := make([]srcCount, 0, len(counts))
	for src, c := range counts {
		all = append(all, srcCount{src, c})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].n != all[j].n {
			return all[i].n > all[j].n
		}
		return all[i].src < all[j].src
	})
	if len(all) > n {
		all = all[:n]
	}
	parts := make([]string, len(all))
	for i, sc := range all {
		parts[i] = fmt.Sprintf("%s×%d", sc.src, sc.n)
	}
	return strings.Join(parts, ", ")
}

// stampRecord is one entry in the per-entity stamp audit ring.
type stampRecord struct {
	stamp     uint32
	at        time.Time
	recovered bool
}

// recordStamp appends to the audit ring and the per-caller tally. Caller must
// hold e.mu. Connection-less entities record too: an NPC or projection minting
// on its own Manager during a warp is what an investigation attributes.
func (e *Entity) recordStamp(stamp uint32, recovered bool) {
	e.stampMints++
	now := time.Now()
	if e.stampLog == nil {
		e.stampLog = new([64]stampRecord)
	}
	e.stampLog[e.stampLogIdx%uint32(len(e.stampLog))] = stampRecord{stamp: stamp, at: now, recovered: recovered}
	e.stampLogIdx++

	if e.stampSources == nil {
		e.stampSources = make(map[string]int, 8)
		e.stampFirstAt = now
	}
	e.stampSources[stampCaller()]++
}

// stampCaller names the code that asked for a stamp: the first frame outside
// this package, since the frames in between are always the same plumbing.
// Called under e.mu on every mint; runtime.Callers is cheap against a budget
// of one stamp per client per second.
func stampCaller() string {
	var pcs [12]uintptr
	// Skip runtime.Callers, stampCaller, recordStamp.
	n := runtime.Callers(3, pcs[:])
	if n == 0 {
		return "unknown"
	}
	frames := runtime.CallersFrames(pcs[:n])
	for {
		f, more := frames.Next()
		if f.Function == "" {
			break
		}
		// runtime. frames are the floor of the stack, not a caller: a flush driven by
		// this package's own tickLoop has no frame outside destiny at all.
		if !strings.Contains(f.Function, "gurimoon/destiny.") &&
			!strings.HasPrefix(f.Function, "runtime.") {
			// Trim to the last path segment: the package-qualified name plus file and
			// line is what a reader greps for.
			name := f.Function
			if i := strings.LastIndex(name, "/"); i >= 0 {
				name = name[i+1:]
			}
			return fmt.Sprintf("%s (%s:%d)", name, filepath.Base(f.File), f.Line)
		}
		if !more {
			break
		}
	}
	// Every frame was inside destiny: a flush driven by the system tick loop.
	return "destiny (tick/internal)"
}

// StampReport summarises this entity's stamp stream: the mint rate against the
// one-per-wall-second budget, the recent rate, the lead, the grid cadence, the
// refusals and the per-call-site tally, worst first.
// See docs/destiny-stamps.md#diagnostics.
func (e *Entity) StampReport() string {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.stampMints == 0 || e.stampFirstAt.IsZero() {
		return "no stamps minted for this ballpark yet."
	}
	wall := time.Since(e.stampFirstAt).Seconds()
	if wall < 1 {
		wall = 1
	}
	rate := float64(e.stampMints) / wall

	type srcCount struct {
		name string
		n    int
	}
	srcs := make([]srcCount, 0, len(e.stampSources))
	for k, v := range e.stampSources {
		srcs = append(srcs, srcCount{k, v})
	}
	sort.Slice(srcs, func(i, j int) bool {
		if srcs[i].n != srcs[j].n {
			return srcs[i].n > srcs[j].n
		}
		return srcs[i].name < srcs[j].name
	})

	var b strings.Builder
	fmt.Fprintf(&b, "stamp stream for entity %d over %.0fs:\n", e.ID, wall)
	fmt.Fprintf(&b, "  %d mints, %.2f/s (budget 1.00/s), lead %+d, sim clock %+.0fs vs wall\n",
		e.stampMints, rate, e.stampLead(), float64(e.stampMints)-wall)
	// The session average answers whether this ballpark has been healthy overall;
	// only the windowed rate can show a burst, so both lines are printed.
	if recent, n, saturated := e.recentStampRateLocked(time.Now(), stampRateWindow); n > 0 {
		note := ""
		if saturated {
			note = " [ring full — floor]"
		}
		fmt.Fprintf(&b, "  last %s: %d mints, %.2f/s%s\n", stampRateWindow, n, recent, note)
		if recent > 1.0 {
			fmt.Fprintf(&b, "  OVER BUDGET by %.2f/s right now — this is what stuttering looks like.\n", recent-1.0)
		}
	}
	if rate > 1.0 {
		fmt.Fprintf(&b, "  OVER BUDGET by %.2f/s for the session.\n", rate-1.0)
	}
	// Grid cadence: how often anything on the grid moves. A stream under budget
	// with its lead pinned at or above zero steps only on the maxPendingHold path.
	if e.stampedFlushes > 0 {
		since := time.Since(e.lastStampedFlushAt).Seconds()
		fmt.Fprintf(&b, "  grid cadence: %d steps, %.2f Hz avg, last %.1fs ago\n",
			e.stampedFlushes, float64(e.stampedFlushes)/wall, since)
		if since >= 2.0 {
			fmt.Fprintf(&b, "    grid has not advanced for %.1fs — other ships are frozen on this client.\n", since)
		}
	} else if len(e.pendingActions) > 0 {
		fmt.Fprintf(&b, "  grid cadence: NO stamped flush yet, %d actions queued.\n", len(e.pendingActions))
	}
	if e.stampRefusals > 0 {
		// A to-do list, not a health metric: each entry is a path that should queue or
		// use SendStampedUrgent. Clearing it is the precondition for StampBudgetEnforced.
		verb := "would be refused"
		if StampBudgetEnforced {
			verb = "REFUSED"
		}
		fmt.Fprintf(&b, "  %d sends over the ceiling (%s):\n", e.stampRefusals, verb)
		fmt.Fprintf(&b, "    %s\n", e.topOverSourcesLocked(5))
	}
	for _, s := range srcs {
		fmt.Fprintf(&b, "  %6d  %5.2f/s  %s\n", s.n, float64(s.n)/wall, s.name)
	}
	return b.String()
}

// StampAudit returns the recent stamp mint history, oldest first, as
// stamp(-age_ms) entries. A stamp reused across more than 1 s, or a jump of 3
// or more, points at what caused a client RequestReset.
func (e *Entity) StampAudit() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	// The ring is allocated on first mint. len() of a nil array pointer is legal
	// (a constant) but indexing one is not, so return early.
	if e.stampLog == nil {
		return ""
	}
	n := uint32(len(e.stampLog))
	start := uint32(0)
	if e.stampLogIdx > n {
		start = e.stampLogIdx - n
	}
	now := time.Now()
	var b strings.Builder
	for i := start; i < e.stampLogIdx; i++ {
		r := e.stampLog[i%n]
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%d(-%dms", r.stamp, now.Sub(r.at).Milliseconds())
		if r.recovered {
			b.WriteString(",R")
		}
		b.WriteByte(')')
	}
	return b.String()
}

// LastMintedStamp is the stream's current floor: the highest stamp NextStamp
// has handed out (or SyncStamp raised it to). A caller that remembers the
// value after its own send can tell later whether anything else has been
// stamped onto this stream since; equal means nothing has.
func (e *Entity) LastMintedStamp() uint32 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastStamp
}

// SyncStamp updates the entity's stamp floor from an external source (e.g.
// the player connection's wall-clock sync). Thread-safe.
func (e *Entity) SyncStamp(candidate uint32) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if candidate > e.lastStamp {
		e.lastStamp = candidate
	}
	if e.Manager != nil && candidate > e.Manager.Stamp {
		e.Manager.Stamp = candidate
	}
}

// NoteFloorMint records a stamp that was sent without NextStamp (a floor advance) in the
// mint audit and the per-call-site tally, so /stamps counts it. It does not move the
// floor; SyncStamp does that. Thread-safe.
func (e *Entity) NoteFloorMint(stamp uint32) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.recordStamp(stamp, false)
}
