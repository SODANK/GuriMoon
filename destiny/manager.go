package destiny

import (
	"math"
	"sync/atomic"
)

// navState describes the current navigation intent of the entity.
type navState int

const (
	navStop   navState = iota
	navGoto            // fly toward a fixed position
	navFollow          // chase another entity
	navOrbit           // circle another entity
	navWarp            // warp tunnel
)

// Manager is a single-entity destiny physics simulation.
// It tracks position, velocity, and navigation intent for one ship.
type Manager struct {
	EntityID uint32
	Stamp    uint32  // current destiny stamp, incremented by NextStamp()
	entity   *Entity // back-pointer to owning Entity (nil for standalone managers)

	Pos Vec3 // world position (metres)
	Vel Vec3 // current velocity (m/s)

	// immutable ship statistics
	mass            float64
	radius          float64
	agility         float64
	maxShipVelocity float64 // base max velocity (incl. skill bonuses, excl. modules)

	// solid makes a rigid ball collidable: BallDesc sends SubTypeRigidSolid. A plain
	// uint32 read atomically, since the dungeon editor flips it on a live entity from
	// another goroutine and Manager is copied by value.
	solid uint32

	// speedMods are multiplicative max-velocity modifiers from module effects
	// (afterburner/MWD boost >1, stasis web <1), keyed by source module itemID.
	// Effective max velocity = maxShipVelocity × Π(speedMods).
	speedMods map[int64]float64

	// speed fractions
	userSpeedFraction   float64 // last value set by SetSpeedFraction [0..1]
	activeSpeedFraction float64 // effective fraction (0 when stopped)

	// navigation
	nav        navState
	targetPos  Vec3    // GOTO / WARP destination
	followID   uint32  // FOLLOW / ORBIT target entity ID
	followDist float64 // FOLLOW / ORBIT preferred distance
	// followWire is the FOLLOW / ORBIT range as the client was told it: followDist
	// unless SetNavWireRange converted a surface range. The ball state must carry the
	// client's own number back, or a resync moves the ball by both radii.
	followWire float64

	// orbitNormal is the unit normal of the orbit plane, zero until the first Step
	// establishes it from the entity's velocity, as destiny.dll does.
	// See docs/destiny-dll-spec.md#orbit-plane.
	orbitNormal Vec3

	// warp-only extras
	warpEffectStamp uint32
	warpOwnerID     uint32
	warpExitDist    float64 // warp-exit radius in metres (targetRadius + buffer)
	warpDistance    float64 // total warp distance in metres (origin→dest)

	// TargetLookup resolves an entity ID to its current position.
	// Set by the caller to enable follow/orbit to track other entities.
	TargetLookup func(entityID uint32) (Vec3, bool)
}

// New creates a Manager for the given entity.
// mass is in kg, agility is the EVE inertia modifier, maxVelocity is m/s,
// radius is m.
func New(entityID uint32, mass, agility, maxVelocity, radius float64) *Manager {
	m := &Manager{
		EntityID:          entityID,
		mass:              mass,
		radius:            radius,
		agility:           agility,
		maxShipVelocity:   maxVelocity,
		userSpeedFraction: 1.0,
		nav:               navStop,
	}
	return m
}

// ── Navigation commands ───────────────────────────────────────────────────────

// SetPosition teleports the entity to pos with zero velocity.
func (m *Manager) SetPosition(pos Vec3) {
	m.Pos = pos
	m.Vel = Vec3{}
}

// Stop brings the entity to rest at once, velocity zeroed where it stands. For
// server-decided halts that are snapped on the wire anyway (warp landing, death,
// a scramble abort, an NPC Stop sent with SetBallPosition).
func (m *Manager) Stop() {
	m.nav = navStop
	m.activeSpeedFraction = 0
	m.Vel = Vec3{}
}

// StopCoast is the pilot's own Stop: zero thrust, coasting down on the same
// inertia curve destiny.dll uses, so the server's position matches the client's
// at rest. Stop's instant halt leaves the server short of the client by v*tau.
func (m *Manager) StopCoast() {
	m.nav = navStop
	m.activeSpeedFraction = 0
}

// stopRestSpeed is the speed (m/s) below which a coasting ball is put to rest.
const stopRestSpeed = 0.5

// GotoDirection commands the entity to fly toward target at full (user) speed.
func (m *Manager) GotoDirection(target Vec3) {
	m.nav = navGoto
	m.targetPos = target
	m.activeSpeedFraction = m.userSpeedFraction
}

// SetSpeedFraction adjusts the fractional speed [0..1] for the current task.
func (m *Manager) SetSpeedFraction(f float64) {
	if f < 0 {
		f = 0
	} else if f > 1 {
		f = 1
	}
	m.userSpeedFraction = f
	if m.nav != navStop {
		m.activeSpeedFraction = f
	}
}

// Follow commands the entity to chase followID and maintain followDist metres.
func (m *Manager) Follow(followID uint32, dist float64) {
	m.nav = navFollow
	m.followID = followID
	m.followDist = dist
	m.followWire = dist
	m.activeSpeedFraction = m.userSpeedFraction
}

// Orbit commands the entity to circle orbitID at dist metres.
func (m *Manager) Orbit(orbitID uint32, dist float64) {
	// Re-seed the orbit plane only for a new target: the client keeps its plane on a
	// re-issued Orbit or a changed radius. See docs/destiny-dll-spec.md#orbit-plane.
	if m.nav != navOrbit || m.followID != orbitID {
		m.orbitNormal = Vec3{}
	}
	m.nav = navOrbit
	m.followID = orbitID
	m.followDist = dist
	m.followWire = dist
	m.activeSpeedFraction = m.userSpeedFraction
}

// SetNavWireRange records the range the client was sent for the current
// Follow/Orbit when it differs from the centre distance the sim holds - the
// client's range is surface to surface. Call after Follow or Orbit.
func (m *Manager) SetNavWireRange(r float64) { m.followWire = r }

// IsChasing reports whether the entity is in Follow or Orbit. Not thread-safe;
// call via Entity.IsChasing.
func (m *Manager) IsChasing() bool {
	return m.nav == navFollow || m.nav == navOrbit
}

// speedModProduct returns the product of all active speed modifiers.
func (m *Manager) speedModProduct() float64 {
	p := 1.0
	for _, v := range m.speedMods {
		p *= v
	}
	return p
}

// SetSpeedModifier installs or replaces a multiplicative max-velocity modifier
// keyed by the source module itemID and returns the new effective max velocity.
// Not thread-safe; call via Entity.SetSpeedModifier.
func (m *Manager) SetSpeedModifier(key int64, mult float64) float64 {
	if mult <= 0.01 {
		mult = 0.01
	}
	if m.speedMods == nil {
		m.speedMods = make(map[int64]float64)
	}
	m.speedMods[key] = mult
	return m.EffectiveMaxVelocity()
}

// RemoveSpeedModifier removes a speed modifier and returns the new effective max
// velocity and whether a modifier was removed. Not thread-safe; call via
// Entity.RemoveSpeedModifier.
func (m *Manager) RemoveSpeedModifier(key int64) (float64, bool) {
	if _, ok := m.speedMods[key]; !ok {
		return m.EffectiveMaxVelocity(), false
	}
	delete(m.speedMods, key)
	return m.EffectiveMaxVelocity(), true
}

// EffectiveMaxVelocity is the ship's current max velocity after all module
// speed modifiers (webs, prop mods).
func (m *Manager) EffectiveMaxVelocity() float64 {
	return m.maxShipVelocity * m.speedModProduct()
}

// NavTargetID returns the FOLLOW or ORBIT target ball, or 0 when the nav mode has
// no target ball. Callers find navs a target's removal would leave dangling with it.
func (m *Manager) NavTargetID() uint32 {
	switch m.nav {
	case navFollow, navOrbit:
		return m.followID
	}
	return 0
}

// Step advances position by dt seconds on the current nav state: a simplified
// server-side integration for range checks and aiming. The client's destiny.dll
// runs the authoritative physics.
func (m *Manager) Step(dt float64) {
	if dt <= 0 {
		return
	}
	targetSpeed := m.EffectiveMaxVelocity() * m.activeSpeedFraction

	// Compute desired velocity vector for current nav mode.
	var desiredVel Vec3
	switch m.nav {
	case navGoto:
		desiredVel = m.desiredVelToward(m.targetPos, targetSpeed)
	case navFollow:
		if m.TargetLookup != nil {
			if tpos, ok := m.TargetLookup(m.followID); ok {
				desiredVel = m.desiredVelFollow(tpos, targetSpeed)
			}
		}
	case navOrbit:
		if m.TargetLookup != nil {
			if tpos, ok := m.TargetLookup(m.followID); ok {
				desiredVel = m.desiredVelOrbit(tpos, targetSpeed)
			}
		}
	case navStop:
		// Coasting down after StopCoast; desiredVel stays zero.
		if m.Vel.Len() < stopRestSpeed {
			m.Vel = Vec3{}
			return
		}
	default:
		return
	}

	// First-order lag toward the desired velocity with tau = mass*agility/1e6, the
	// client's own inertia curve. See docs/destiny-dll-spec.md#physics-model.
	tau := m.mass * m.agility / 1e6
	if tau < 0.1 {
		tau = 0.1 // floor for massless/zero-agility entities
	}
	decay := math.Exp(-dt / tau)
	m.Vel.X = desiredVel.X + (m.Vel.X-desiredVel.X)*decay
	m.Vel.Y = desiredVel.Y + (m.Vel.Y-desiredVel.Y)*decay
	m.Vel.Z = desiredVel.Z + (m.Vel.Z-desiredVel.Z)*decay

	// Advance position.
	m.Pos.X += m.Vel.X * dt
	m.Pos.Y += m.Vel.Y * dt
	m.Pos.Z += m.Vel.Z * dt

	// Snap to waypoint on arrival (navGoto only) to prevent oscillation.
	if m.nav == navGoto && m.Pos.Sub(m.targetPos).Len() < 1 {
		m.Pos = m.targetPos
		m.Vel = Vec3{}
	}
}

// desiredVelToward returns a velocity vector pointing from the entity toward
// target at the given speed. Returns zero within 1 metre of the target.
func (m *Manager) desiredVelToward(target Vec3, speed float64) Vec3 {
	diff := target.Sub(m.Pos)
	dist := diff.Len()
	if dist < 1 || speed <= 0 {
		return Vec3{}
	}
	return Vec3{
		X: diff.X / dist * speed,
		Y: diff.Y / dist * speed,
		Z: diff.Z / dist * speed,
	}
}

// desiredVelFollow returns the velocity for FollowBall: approach or back away until
// the ship sits followDist from the target's centre, holding inside a
// FollowBandWidth deadband. The client dll holds the same range, so the server
// stays near the client's keep-at-range result instead of the target's centre.
func (m *Manager) desiredVelFollow(tpos Vec3, speed float64) Vec3 {
	diff := m.Pos.Sub(tpos)
	dist := diff.Len()
	if math.Abs(dist-m.followDist) <= FollowBandWidth {
		return Vec3{}
	}
	if dist < 1 {
		// On the target's centre there is no outward direction; hold and let the
		// target's own motion open the gap.
		return Vec3{}
	}
	hold := tpos.Add(diff.Scale(m.followDist / dist))
	return m.desiredVelToward(hold, speed)
}

// orbitAxisFor returns a deterministic unit vector derived from an entity ID: the
// fallback orbit-plane seed for a ball commanded to orbit while at rest or flying
// straight at its target. Per-ID so a freshly spawned wave does not stack every
// member onto one circle, and stable across restarts.
func orbitAxisFor(entityID uint32) Vec3 {
	h := entityID*2654435761 + 0x9e3779b9
	theta := float64(h%4096) / 4096 * 2 * math.Pi
	phi := float64((h>>12)%2048)/2048*math.Pi - math.Pi/2
	return Vec3{
		X: math.Cos(phi) * math.Cos(theta),
		Y: math.Sin(phi),
		Z: math.Cos(phi) * math.Sin(theta),
	}
}

// orbitPlaneNormal returns the unit normal of the orbit plane, establishing it on
// first use from the entity's current velocity, the input destiny.dll uses. radial
// must be the unit vector from the orbit target out to the ship.
func (m *Manager) orbitPlaneNormal(radial Vec3) Vec3 {
	if !m.orbitNormal.IsZero() {
		return m.orbitNormal
	}
	// radial x velocity is normal to the plane of both, oriented so normal x radial
	// points along the current tangential velocity.
	n := radial.Cross(m.Vel)
	if n.LenSq() < 1e-12 {
		// At rest or heading straight at the target: no plane implied. Fall back to
		// the per-entity axis.
		n = radial.Cross(orbitAxisFor(m.EntityID))
	}
	if n.LenSq() < 1e-12 {
		// Radial is parallel to the fallback axis too; any perpendicular will do.
		n = radial.Cross(Vec3{Y: 1})
		if n.LenSq() < 1e-12 {
			n = Vec3{X: 1}
		}
	}
	m.orbitNormal = n.Normalize()
	return m.orbitNormal
}

// desiredVelOrbit returns a velocity vector for orbiting target at
// m.followDist. Falls back to approach when too far from orbit range.
func (m *Manager) desiredVelOrbit(target Vec3, speed float64) Vec3 {
	diff := m.Pos.Sub(target)
	dist := diff.Len()
	if dist < 1 {
		// On top of the target: push outward to establish the orbit.
		return Vec3{X: speed}
	}
	// Too far from orbit range: approach first.
	if dist > m.followDist*1.5 {
		return m.desiredVelToward(target, speed)
	}
	if speed <= 0 {
		return Vec3{}
	}

	// Radial unit vector (outward, from target to ship) and the in-plane
	// tangent, both in the orbit plane rather than a hardcoded XZ one.
	radial := diff.Scale(1 / dist)
	tangent := m.orbitPlaneNormal(radial).Cross(radial)
	if tangent.LenSq() < 1e-12 {
		// Radial has rotated onto the plane normal (only reachable if the
		// ball was teleported). Reseed the plane from where it is now.
		m.orbitNormal = Vec3{}
		tangent = m.orbitPlaneNormal(radial).Cross(radial)
		if tangent.LenSq() < 1e-12 {
			return Vec3{}
		}
	}
	tangent = tangent.Normalize()

	// Radial correction: up to 20% of speed budget to converge toward
	// desired orbit distance, so chasers can still close the gap.
	radialSpeed := 0.0
	radialErr := dist - m.followDist
	maxRadial := speed * 0.2
	if radialErr > 1 {
		// Too far out: move inward.
		radialSpeed = -math.Min(maxRadial, radialErr*0.5)
	} else if radialErr < -1 {
		// Too close: move outward.
		radialSpeed = math.Min(maxRadial, -radialErr*0.5)
	}

	// Tangential speed fills remaining budget.
	tangentialSpeed := speed
	if radialSpeed != 0 {
		tangentialSpeed = math.Sqrt(math.Max(0, speed*speed-radialSpeed*radialSpeed))
	}

	// No separate Y term: an in-plane orbit passes through the target's altitude by
	// construction. See docs/destiny-dll-spec.md#orbit-plane.
	return tangent.Scale(tangentialSpeed).Add(radial.Scale(radialSpeed))
}

// Physics constants for warp duration calculation.
const (
	WarpSpeedAUperSec = 3.0 // exported for use by SendWarpTo
	auInMeters        = 149_597_870_700.0
)

// SetEntity sets the back-pointer to the owning Entity so that NextStamp()
// can delegate to the Entity's wall-clock-aware stamp logic.
func (m *Manager) SetEntity(e *Entity) {
	m.entity = e
}

// NextStamp returns a wall-clock-aware stamp suitable for destiny updates.
// When an Entity back-pointer is set, delegates to Entity.NextStamp() so
// that player commands and NPC broadcasts share the same time-based logic.
// Falls back to Stamp++ for standalone managers (no entity).
func (m *Manager) NextStamp() uint32 {
	if m.entity != nil {
		return m.entity.NextStamp()
	}
	m.Stamp++
	return m.Stamp
}

// SendStamped delegates to Entity.SendStamped when an entity back-pointer is set
// (stamp allocation serialised with the socket write). Standalone managers have
// no concurrent broadcast senders and use an unserialised NextStamp.
func (m *Manager) SendStamped(send func(stamp uint32) error) error {
	if m.entity != nil {
		return m.entity.SendStamped(send)
	}
	m.Stamp++
	return send(m.Stamp)
}

// SendStampedUrgent is SendStamped without the budget ceiling (Entity.SendStampedUrgent
// has the exempt set). On a Manager it is the usual call: a Manager controls the
// ego ball, whose packets are the pilot's own nav echo, warp phases and death.
// Traffic about other balls goes per-recipient through Entity, where the ceiling lives.
func (m *Manager) SendStampedUrgent(send func(stamp uint32) error) error {
	if m.entity != nil {
		return m.entity.SendStampedUrgent(send)
	}
	m.Stamp++
	return send(m.Stamp)
}

// WarpPhases splits a warp to dest into a sequential align phase (ln(4)*tau, the
// time to reach the dll's 75% entry speed) and a tunnel phase modelled on the dll's
// decoded warp integrator. The dll owns the warp physics; the server only times it.
// See docs/destiny-dll-spec.md#warp-kinematics-decoded.
func (m *Manager) WarpPhases(dest Vec3) (align, tunnel, total float64) {
	const (
		warpBaseSpeed = 3.0 // m/s, dll warp entry speed
		warpRate      = 3.0 // 1/s, dll exponential accel/decel rate
	)
	dist := dest.Sub(m.Pos).Len()
	vMax := WarpSpeedAUperSec * auInMeters
	if speedCap := 1.5 * dist; speedCap < vMax {
		vMax = speedCap
	}
	if vMax < warpBaseSpeed*2 {
		vMax = warpBaseSpeed * 2 // degenerate short hop; keep logs sane
	}
	// The dll leaves warp when the decel curve crosses the ship's max velocity.
	// Floored at 100 m/s so a freighter does not stretch the modelled decel tail.
	vExit := m.EffectiveMaxVelocity()
	if vExit < 100 {
		vExit = 100
	}
	if vExit > vMax {
		vExit = vMax
	}
	tAccel := math.Log(vMax/warpBaseSpeed) / warpRate
	dAccel := (vMax - warpBaseSpeed) / warpRate
	tDecel := math.Log(vMax/vExit) / warpRate
	dDecel := (vMax - vExit) / warpRate
	tCruise := 0.0
	if d := dist - dAccel - dDecel; d > 0 {
		tCruise = d / vMax
	}
	align = math.Log(4) * m.mass * m.agility / 1e6
	if align < 2.0 {
		align = 2.0
	}
	tunnel = math.Ceil(tAccel+tCruise+tDecel) + 2 // +2s pad for stamp/apply latency
	if tunnel < 4 {
		tunnel = 4
	}
	total = align + tunnel
	return align, tunnel, total
}

// WarpTo begins a warp to dest.  effectStamp is the timestamp used in the
// warp effect tail of the binary ball encoding.  exitDist is the warp-exit
// radius in metres (typically targetRadius + 2500 m).
func (m *Manager) WarpTo(dest Vec3, effectStamp, ownerID uint32, exitDist float64) {
	m.nav = navWarp
	m.targetPos = dest
	m.warpEffectStamp = effectStamp
	m.warpOwnerID = ownerID
	m.warpExitDist = exitDist
	m.warpDistance = dest.Sub(m.Pos).Len()
	m.activeSpeedFraction = m.userSpeedFraction
	// Set velocity toward destination so ball encoding shows correct heading.
	dir := dest.Sub(m.Pos)
	if !dir.IsZero() {
		m.Vel = dir.Normalize().Scale(m.EffectiveMaxVelocity())
	}
}

// ── Getters (for diagnostic logging) ─────────────────────────────────────────

// IsMoving reports whether the entity is actively navigating (not stopped).
func (m *Manager) IsMoving() bool { return m.nav != navStop || !m.Vel.IsZero() }

func (m *Manager) Mass() float64            { return m.mass }
func (m *Manager) Agility() float64         { return m.agility }
func (m *Manager) MaxVelocity() float64     { return m.EffectiveMaxVelocity() }
func (m *Manager) BaseMaxVelocity() float64 { return m.maxShipVelocity }
func (m *Manager) Radius() float64          { return m.radius }
func (m *Manager) SpeedFraction() float64   { return m.activeSpeedFraction }

// SetSolid makes an immobile ball collidable (see SubTypeRigidSolid). It takes
// effect on the next BallDesc; a client already holding the ball keeps the old
// shape until the ball is sent again.
func (m *Manager) SetSolid(v bool) {
	var n uint32
	if v {
		n = 1
	}
	atomic.StoreUint32(&m.solid, n)
}

// ── Snapshot helpers ─────────────────────────────────────────────────────────

// CurrentBallMode maps the current navigation state to a BallMode value.
func (m *Manager) CurrentBallMode() BallMode {
	switch m.nav {
	case navGoto:
		return ModeGoto
	case navFollow:
		return ModeFollow
	case navOrbit:
		return ModeOrbit
	case navWarp:
		return ModeWarp
	default:
		return ModeStop
	}
}

// BallDesc returns a BallDesc snapshot of the current entity state.
// The caller must fill in CorpID, AllianceID, and Name before encoding.
func (m *Manager) BallDesc() BallDesc {
	// An immobile entity (no maxVelocity) goes out as a rigid ball, the shape of an
	// asteroid, never as a free ball. See docs/destiny-dll-spec.md#ball-subtypes-and-collision.
	if m.maxShipVelocity <= 0 {
		return BallDesc{
			EntityID: m.EntityID,
			Mode:     ModeRigid,
			SubType:  RigidSubType(atomic.LoadUint32(&m.solid) != 0),
			Pos:      m.Pos,
			Radius:   m.radius,
		}
	}
	// SpeedFraction is the pilot's slider, not the active fraction: a zero here turns
	// the client's autopilot off. See docs/destiny-dll-spec.md#shipsector-speedfraction-is-the-slider.
	bd := BallDesc{
		EntityID:      m.EntityID,
		Mode:          m.CurrentBallMode(),
		SubType:       13, // AddBallSubType_player; bit 0 → ShipSector present
		Pos:           m.Pos,
		Radius:        m.radius,
		Mass:          m.mass,
		MaxSpeed:      m.EffectiveMaxVelocity(),
		Vel:           m.Vel,
		Agility:       m.agility,
		SpeedFraction: m.userSpeedFraction,
	}
	switch m.nav {
	case navGoto:
		bd.TargetPos = m.targetPos
	case navFollow:
		bd.FollowID = m.followID
		bd.FollowRange = m.followWire
	case navOrbit:
		bd.OrbitID = m.followID
		bd.OrbitDist = m.followWire
	case navWarp:
		bd.TargetPos = m.targetPos
		bd.WarpEffectStamp = m.warpEffectStamp
		bd.WarpOwnerID = m.warpOwnerID
		bd.FollowRange = m.warpExitDist
	}
	return bd
}
