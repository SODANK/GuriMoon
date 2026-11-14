package destiny

import (
	"math"
	"time"
)

// Warp arrival as seen from the destination grid: witnesses get the same WarpTo
// the departure grid gets, timed by the dll's own warp curve.
// See docs/destiny-arrival-lanes.md#warp-mirror-the-destination-grid.
const (
	// MinWarpDistM is the dll's warp-vs-goto cutoff: a WarpTo with less remaining
	// distance is converted to a plain GotoPoint at sub-warp speed. No handover
	// closer than this can produce a warp.
	MinWarpDistM = 1e10

	// WarpMirrorLeadSec is how far ahead of its landing a ship is handed to the grid
	// it arrives at: the flight time of the shortest warp the dll accepts,
	// (2/3)*ln(MinWarpDistM/2). See docs/destiny-arrival-lanes.md#warp-mirror-the-destination-grid.
	WarpMirrorLeadSec = 14.9

)

// GridExitSec is when a departing ball is off the grid: the dll's warp curve solved
// for BubbleRadius, ln(BubbleRadius+1)/3. Computed, not a rounded literal: 4.14
// leaves the ball inside the grid. A var because math.Log is not constant.
var GridExitSec = math.Log(BubbleRadius+1) / 3

// warpApplyLagSecs is how long after a WarpTo is sent a bystander's dll arms it: the
// packet, the stamp it waits for and the tick that applies it.
const warpApplyLagSecs = 2.0

// ObserverDepartSec is how long after its WarpTo is sent a departing ball stays in a
// bystander's ballpark: GridExitSec from the moment the bystander's dll arms the warp.
// Removing the ball sooner deletes it while it is still on grid.
var ObserverDepartSec = GridExitSec + warpApplyLagSecs

// WarpGateOpen reports whether the dll would arm a warp to dest on this tick: the
// velocity within 0.01 of the line to dest (1 - cos, about 8 degrees) and above 75% of
// max velocity. Not thread-safe; see Entity.AwaitWarpGate.
func (m *Manager) WarpGateOpen(dest Vec3) bool {
	maxV := m.EffectiveMaxVelocity()
	if maxV <= 0 || m.Vel.LenSq() <= 0.5625*maxV*maxV {
		return false
	}
	to := dest.Sub(m.Pos).Normalize()
	return math.Abs(1-to.Dot(m.Vel.Normalize())) < 0.01
}

// warpAlignLimit bounds the wait for the warp gate: swinging a full-speed velocity
// right round takes ln(8)*tau, and a ball that cannot reach the gate (a throttle under
// 75%, a web) must not hold its warp for ever.
func (m *Manager) warpAlignLimit() time.Duration {
	tau := m.mass * m.agility / SpaceFriction
	return time.Duration((math.Ceil(math.Log(8)*tau) + 3) * float64(time.Second))
}

// AwaitWarpGate blocks until the entity's ball, as the tick loop flies it, passes the
// dll's warp gate toward dest (Manager.WarpGateOpen), and returns how long that took:
// never less than floor, never more than the ball's align limit. ok is false when done
// closed first. A fixed from-rest align leaves a ship that was already moving another
// way still turning when its WarpTo goes out.
func (e *Entity) AwaitWarpGate(done <-chan struct{}, dest Vec3, floor time.Duration) (waited time.Duration, ok bool) {
	e.mu.Lock()
	limit := floor
	if e.Manager != nil {
		limit = e.Manager.warpAlignLimit()
	}
	e.mu.Unlock()
	return e.awaitWarpGate(done, dest, floor, limit, 100*time.Millisecond)
}

func (e *Entity) awaitWarpGate(done <-chan struct{}, dest Vec3, floor, limit, poll time.Duration) (time.Duration, bool) {
	if limit < floor {
		limit = floor
	}
	start := time.Now()
	tk := time.NewTicker(poll)
	defer tk.Stop()
	for {
		select {
		case <-done:
			return time.Since(start), false
		case <-tk.C:
		}
		waited := time.Since(start)
		if waited >= limit {
			return waited, true
		}
		if waited < floor {
			continue
		}
		e.mu.Lock()
		open := e.Manager != nil && e.Manager.WarpGateOpen(dest)
		e.mu.Unlock()
		if open {
			return waited, true
		}
	}
}

// WarpMirrorPoint returns where to publish a ship for the grid it is arriving at,
// and the velocity to publish it with, leadSec before it lands: on the approach
// radial, moving at maxVelocity so the dll arms the WarpTo at once. ok is false
// when the hop is shorter than MinWarpDistM; publish at rest at the landing then.
func WarpMirrorPoint(landing, origin Vec3, leadSec, maxVelocity float64) (pos, vel Vec3, ok bool) {
	approach := landing.Sub(origin)
	dist := approach.Len()
	if leadSec <= 0 || dist < MinWarpDistM || maxVelocity <= 0 {
		return landing, Vec3{}, false
	}
	dir := approach.Scale(1 / dist)

	// d = 2*e^(1.5*lead), floored at the dll's cutoff so a shortened lead cannot
	// produce a GotoPoint, and capped at the real hop.
	d := 2 * math.Exp(1.5*leadSec)
	if d < MinWarpDistM {
		d = MinWarpDistM
	}
	if d > dist {
		d = dist
	}
	return landing.Sub(dir.Scale(d)), dir.Scale(maxVelocity), true
}
