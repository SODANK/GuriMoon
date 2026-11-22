package destiny

import (
	"math"
	"testing"
	"time"
)

// warpPos walks the warp line from the tunnel entry to the landing point (#25).
func TestWarpPosAlongTunnel(t *testing.T) {
	m := New(1, 1e6, 0.5, 300, 50)
	origin := Vec3{X: 1e9, Y: 2e9, Z: 3e9}
	m.Pos = origin
	dest := origin.Add(Vec3{X: 20 * auInMeters}) // 20 AU along +X
	const exit = 2500.0
	m.WarpTo(dest, 0, 0, exit)
	end := dest.Sub(Vec3{X: exit}) // landing point: pulled back toward the origin

	start := m.warpStartedAt
	at := func(sec float64) Vec3 { return m.warpPos(start.Add(time.Duration(sec * float64(time.Second)))) }

	if p := at(0); p.Sub(origin).Len() > 1e-3 {
		t.Fatalf("t=0: %v, want origin %v", p, origin)
	}

	_, tA, tC, tD := m.warpProfile(end.Sub(origin).Len())
	total := tA + tC + tD
	prev := -1.0
	for i := 0; i <= 100; i++ {
		p := at(total * float64(i) / 100)
		if math.Abs(p.Y-origin.Y) > 1e-3 || math.Abs(p.Z-origin.Z) > 1e-3 {
			t.Fatalf("step %d left the warp line: %v", i, p)
		}
		d := p.Sub(origin).Len()
		if d < prev {
			t.Fatalf("step %d moved backwards: %.0f < %.0f", i, d, prev)
		}
		prev = d
	}

	if p := at(total + 60); p.Sub(end).Len() > 1 {
		t.Fatalf("after the warp: %v, want landing point %v (off by %.1f m)", p, end, p.Sub(end).Len())
	}

	// Mid-cruise is far from both ends: the old behaviour (Pos frozen at origin) fails here.
	if mid := at(tA + tC/2); mid.Sub(origin).Len() < 5*auInMeters || end.Sub(mid).Len() < 5*auInMeters {
		t.Fatalf("mid-cruise %v is not mid-warp", mid)
	}

	// Not warping: plain Pos.
	m.Stop()
	if p := m.warpPos(time.Now()); p != m.Pos {
		t.Fatalf("stopped: warpPos %v != Pos %v", p, m.Pos)
	}
}
