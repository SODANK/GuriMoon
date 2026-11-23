package destiny

import (
	"math"
	"sort"
	"strconv"
	"sync/atomic"
	"time"

	"gurimoon/marshal"
)

// Collisions ("bumps"). The client's destiny.dll has an elastic collision
// routine, but in a client ballpark it only runs it for a massive ball whose
// client-side bubble holds an interactive ball, or for a ball in warp: in
// practice ships fly through ships and rocks at sub-warp speed. So the server
// resolves bumps itself in the tick loop and tells every client holding the
// ball its new velocity with SetBallVelocity. The client's own inertia curve
// then pulls the ball back onto its course, as the server's Manager.Step does.
//
// Bodies: free balls (ships, NPCs, pods, unpiloted hulls), solid rigid entities
// (dungeon structures flagged solid) and asteroids. Rigid bodies and asteroids
// have infinite mass. Not bodies: balls in warp or aligning to warp, cloaked or
// GM-ghosted balls, drones, wrecks, destroyed balls and balls outside a bubble.

const (
	// bumpPairCooldown is how long a pair that just bumped is left alone, so a
	// pair still touching after the kick is not re-bumped every tick.
	bumpPairCooldown = time.Second

	// bumpMinSeparation is the floor on the normal speed (m/s) two balls part at,
	// so a slow nudge still separates them instead of grinding through.
	bumpMinSeparation = 5.0

	// bumpMinApproach is the slowest normal closing speed (m/s) that counts as a
	// bump; slower contact is left alone.
	bumpMinApproach = 0.5

	// bumpDefaultRestitution is the share of closing speed kept on the way out
	// (1 = perfectly elastic).
	bumpDefaultRestitution = 0.6

	// bumpDefaultMass stands in for a Manager with no mass, so an unattributed
	// NPC still bumps like a ship rather than as an immovable wall.
	bumpDefaultMass = 1e6

	categoryDrone = 18
)

// BumpsEnabled reports whether the tick loop resolves collisions. Nil means on.
// Set once at startup from the space.collisions setting.
var BumpsEnabled func() bool

// BumpRestitution returns the restitution coefficient, clamped to [0, 1] by the
// caller. Nil means bumpDefaultRestitution. Set once at startup.
var BumpRestitution func() float64

// bumpBody is one ball's collision state for one tick, snapshotted under its
// entity lock so the pair tests run with no locks held.
type bumpBody struct {
	e       *Entity // nil for an asteroid
	id      uint32
	prev    Vec3 // position at the start of this tick
	pos     Vec3 // position after this tick's Step
	vel     Vec3
	radius  float64
	invMass float64 // 0 = immovable
	moved   bool
}

// bumpSnapshot returns this entity's collision body, or false when it does not
// collide this tick. prev is where the tick loop found it before Step.
func (e *Entity) bumpSnapshot(prev Vec3, moved bool) (bumpBody, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	m := e.Manager
	if m == nil || m.radius <= 0 || e.BubbleID < 0 {
		return bumpBody{}, false
	}
	if e.cloaked || (e.gmCloak && e.gmGhost) || e.warpPending || m.CurrentBallMode() == ModeWarp {
		return bumpBody{}, false
	}
	if e.HP.MaxStructure > 0 && e.HP.CurStructure <= 0 {
		return bumpBody{}, false
	}
	if e.Slim.CategoryID == categoryDrone {
		return bumpBody{}, false
	}
	b := bumpBody{e: e, id: e.ID, prev: prev, pos: m.Pos, vel: m.Vel, radius: m.radius, moved: moved}
	if !moved {
		b.prev = m.Pos
	}
	if m.maxShipVelocity <= 0 {
		// Rigid: only a solid one is an obstacle, and it never moves.
		if atomic.LoadUint32(&m.solid) == 0 {
			return bumpBody{}, false
		}
		b.vel = Vec3{}
		b.moved = false
		return b, true
	}
	mass := m.mass
	if mass <= 0 {
		mass = bumpDefaultMass
	}
	b.invMass = 1 / mass
	return b, true
}

// applyBump adds dv to the entity's velocity unless it has since left the
// states that collide (warped, cloaked, died). Returns the new velocity.
func (e *Entity) applyBump(dv Vec3) (Vec3, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	m := e.Manager
	if m == nil || m.maxShipVelocity <= 0 || e.warpPending || e.cloaked || m.CurrentBallMode() == ModeWarp {
		return Vec3{}, false
	}
	m.Vel = m.Vel.Add(dv)
	return m.Vel, true
}

// tickMover is an entity the tick loop stepped, with where it started.
type tickMover struct {
	e    *Entity
	prev Vec3
}

// bumpPairKey is an unordered pair of ball IDs.
type bumpPairKey struct{ a, b uint32 }

func makeBumpPairKey(a, b uint32) bumpPairKey {
	if a > b {
		a, b = b, a
	}
	return bumpPairKey{a, b}
}

// sweepContact tests a ball moving from a0 to a1 against one moving from b0 to
// b1 over the same tick, as spheres whose radii sum to r. It reports the first
// contact and the unit normal pointing from b to a there. Balls already
// overlapping at the start of the tick are not a contact: they are separating
// from an earlier bump or were placed on top of each other.
func sweepContact(a0, a1, b0, b1 Vec3, r float64) (Vec3, bool) {
	d0 := a0.Sub(b0)
	m := a1.Sub(b1).Sub(d0)
	c := d0.LenSq() - r*r
	if c <= 0 {
		return Vec3{}, false
	}
	a := m.LenSq()
	if a == 0 {
		return Vec3{}, false
	}
	bHalf := d0.Dot(m)
	if bHalf >= 0 {
		return Vec3{}, false // moving apart over the tick
	}
	disc := bHalf*bHalf - a*c
	if disc < 0 {
		return Vec3{}, false
	}
	t := (-bHalf - math.Sqrt(disc)) / a
	if t < 0 || t > 1 {
		return Vec3{}, false
	}
	n := d0.Add(m.Scale(t))
	l := n.Len()
	if l == 0 {
		return Vec3{}, false
	}
	return n.Scale(1 / l), true
}

// bumpImpulse returns the velocity changes of a and b for a contact with normal
// n (from b to a), or false when they are not closing along n fast enough.
func bumpImpulse(a, b *bumpBody, n Vec3, restitution float64) (Vec3, Vec3, bool) {
	inv := a.invMass + b.invMass
	if inv == 0 {
		return Vec3{}, Vec3{}, false
	}
	vn := a.vel.Sub(b.vel).Dot(n) // < 0: closing
	if vn > -bumpMinApproach {
		return Vec3{}, Vec3{}, false
	}
	out := math.Max(-vn*restitution, bumpMinSeparation)
	j := (out - vn) / inv
	return n.Scale(j * a.invMass), n.Scale(-j * b.invMass), true
}

// resolveBumps runs one tick of collisions for the entities the tick loop just
// stepped, against every other body in the system. Runs on the tick goroutine
// with no locks held.
func (ss *SolarSystem) resolveBumps(movers []tickMover, all []*Entity, now time.Time) {
	if len(movers) == 0 || (BumpsEnabled != nil && !BumpsEnabled()) {
		return
	}
	restitution := bumpDefaultRestitution
	if BumpRestitution != nil {
		restitution = math.Min(math.Max(BumpRestitution(), 0), 1)
	}

	moved := make(map[uint32]Vec3, len(movers))
	for _, mv := range movers {
		moved[mv.e.ID] = mv.prev
	}
	bodies := make([]bumpBody, 0, len(all))
	anyMoving := false
	for _, e := range all {
		prev, ok := moved[e.ID]
		b, ok2 := e.bumpSnapshot(prev, ok)
		if !ok2 {
			continue
		}
		anyMoving = anyMoving || b.moved
		bodies = append(bodies, b)
	}
	if !anyMoving {
		return
	}

	if ss.bumpCooldown == nil {
		ss.bumpCooldown = make(map[bumpPairKey]time.Time)
	}
	for k, until := range ss.bumpCooldown {
		if now.After(until) {
			delete(ss.bumpCooldown, k)
		}
	}
	dv := make(map[uint32]Vec3)
	bumped := func(a, b *bumpBody, n Vec3) {
		key := makeBumpPairKey(a.id, b.id)
		if until, ok := ss.bumpCooldown[key]; ok && now.Before(until) {
			return
		}
		da, db, ok := bumpImpulse(a, b, n, restitution)
		if !ok {
			return
		}
		ss.bumpCooldown[key] = now.Add(bumpPairCooldown)
		if a.invMass > 0 {
			dv[a.id] = dv[a.id].Add(da)
		}
		if b.invMass > 0 {
			dv[b.id] = dv[b.id].Add(db)
		}
	}

	// Ball against ball: sweep and prune on X over each body's swept extent.
	type span struct {
		lo, hi float64
		i      int
	}
	spans := make([]span, len(bodies))
	for i := range bodies {
		b := &bodies[i]
		spans[i] = span{math.Min(b.prev.X, b.pos.X) - b.radius, math.Max(b.prev.X, b.pos.X) + b.radius, i}
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].lo < spans[j].lo })
	for i := range spans {
		for j := i + 1; j < len(spans) && spans[j].lo <= spans[i].hi; j++ {
			a, b := &bodies[spans[i].i], &bodies[spans[j].i]
			if !a.moved && !b.moved {
				continue
			}
			if n, ok := sweepContact(a.prev, a.pos, b.prev, b.pos, a.radius+b.radius); ok {
				bumped(a, b, n)
			}
		}
	}

	// Ball against asteroid: rocks are rigid and never move.
	ss.mu.RLock()
	for i := range bodies {
		a := &bodies[i]
		if !a.moved || a.invMass == 0 {
			continue
		}
		reach := a.pos.Sub(a.prev).Len()
		for _, rock := range ss.AsteroidBalls {
			r := a.radius + rock.Radius
			if lim := r + reach; a.pos.Sub(rock.Pos).LenSq() > lim*lim {
				continue
			}
			if ss.AsteroidQty[rock.EntityID] <= 0 {
				continue
			}
			if n, ok := sweepContact(a.prev, a.pos, rock.Pos, rock.Pos, r); ok {
				bumped(a, &bumpBody{id: rock.EntityID, pos: rock.Pos, radius: rock.Radius}, n)
			}
		}
	}
	ss.mu.RUnlock()

	for i := range bodies {
		b := &bodies[i]
		d, ok := dv[b.id]
		if !ok || b.e == nil {
			continue
		}
		vel, ok := b.e.applyBump(d)
		if !ok {
			continue
		}
		ss.broadcastBump(b.e, vel)
	}
}

// broadcastBump queues the bumped ball's new velocity to every client holding
// it: its own pilot and the players in its bubble. Queued under one key per
// ball, so a ball bumped twice before the flush sends its latest velocity once.
func (ss *SolarSystem) broadcastBump(e *Entity, vel Vec3) {
	action := BuildSimpleAction("SetBallVelocity",
		marshal.PyInt{Value: uint64(e.ID)},
		marshal.PyReal{Value: vel.X}, marshal.PyReal{Value: vel.Y}, marshal.PyReal{Value: vel.Z})
	key := "bump:" + strconv.FormatUint(uint64(e.ID), 10)
	players := ss.GetBubbleConnectedPlayersExcept(e.ID)
	if e.IsPlayer && e.Conn != nil {
		players = append(players, e)
	}
	for _, p := range players {
		if p.Conn == nil || !p.KnowsBall(e.ID) {
			continue
		}
		p.QueueAction(key, e.ID, action)
	}
}
