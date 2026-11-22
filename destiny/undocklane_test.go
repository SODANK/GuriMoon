package destiny

import (
	"math"
	"testing"
)

// Slots on one run line share layer and column; slot k+3 sits a row ahead of k.
func TestUndockSameRun(t *testing.T) {
	cases := []struct {
		a, b int
		want bool
	}{
		{0, 3, true},
		{0, 6, true},
		{1, 4, true},
		{35, 32, true},
		{0, 1, false},
		{0, 36, false}, // same column, next layer
		{0, UndockLaneMaxSlots, false},
		{-1, 2, false},
	}
	for _, c := range cases {
		if got := undockSameRun(c.a, c.b); got != c.want {
			t.Errorf("undockSameRun(%d, %d) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// The fourth ship out of a door does not get the seat on the first ship's run line.
func TestClaimUndockSkipsOccupiedRun(t *testing.T) {
	ClearUndockSlots()
	const obj = 60_000_001
	want := []int{0, 1, 2, UndockLaneSlotsPerLayer}
	for i, w := range want {
		if got := ClaimUndockSlot(obj, int64(100+i), 0); got != w {
			t.Fatalf("claim %d = slot %d, want %d", i, got, w)
		}
	}
}

// With every run line held, the claim falls back to the first free seat.
func TestClaimUndockFallsBackWhenEveryRunHeld(t *testing.T) {
	ClearUndockSlots()
	const obj = 60_000_002
	heads := UndockLaneColumns * UndockLaneLayerSpan
	for i := 0; i < heads; i++ {
		ClaimUndockSlot(obj, int64(200+i), 0)
	}
	if got := ClaimUndockSlot(obj, 999, 0); got != UndockLaneColumns {
		t.Fatalf("claim past every run head = slot %d, want %d", got, UndockLaneColumns)
	}
}

// A veto skips a seat, and a held seat the veto now refuses is given up.
func TestClaimUndockWhereVeto(t *testing.T) {
	ClearUndockSlots()
	const obj = 60_000_003
	notZero := func(s int) bool { return s != 0 }
	if got := ClaimUndockSlotWhere(obj, 1, 0, notZero); got != 1 {
		t.Fatalf("veto of slot 0 gave slot %d, want 1", got)
	}
	if got := ClaimUndockSlotWhere(obj, 1, 0, notZero); got != 1 {
		t.Fatalf("renewal gave slot %d, want 1 kept", got)
	}
	notOne := func(s int) bool { return s != 1 }
	if got := ClaimUndockSlotWhere(obj, 1, 0, notOne); got != 0 {
		t.Fatalf("held seat refused by veto gave slot %d, want 0", got)
	}
	never := func(int) bool { return false }
	if got := ClaimUndockSlotWhere(obj, 2, 0, never); got < 0 || got >= UndockLaneMaxSlots {
		t.Fatalf("an unsatisfiable veto gave slot %d, want an in-band seat anyway", got)
	}
}

// No two band seats are closer than half a column width, which clears two
// battleship-class hulls (about 480 m between centres).
func TestUndockLaneLatticeClearsBigHulls(t *testing.T) {
	if UndockLaneWidth/2 < 480 {
		t.Fatalf("half lane width %.0f m < 480 m", UndockLaneWidth/2)
	}
	dir := Vec3{X: 0.3, Y: 0.9, Z: -0.2}
	pts := make([]Vec3, UndockLaneMaxSlots)
	for s := range pts {
		pts[s] = UndockLaneOffset(dir, s)
	}
	minD := math.Inf(1)
	for i := range pts {
		for j := i + 1; j < len(pts); j++ {
			if d := pts[i].Sub(pts[j]).Len(); d < minD {
				minD = d
			}
		}
	}
	if minD < UndockLaneWidth/2-1e-6 {
		t.Fatalf("closest two band seats are %.1f m apart, want >= %.0f", minD, UndockLaneWidth/2)
	}
}

// Every seat, overflow included, lies inside the box UndockLaneMaxAxial and
// UndockLaneMaxLateral describe.
func TestUndockLaneBounds(t *testing.T) {
	dir := Vec3{X: 1}
	for s := 0; s < UndockLaneMaxSlots+UndockLaneOverflowSlots; s++ {
		p := UndockLaneOffset(dir, s)
		if p.X < -1e-6 || p.X > UndockLaneMaxAxial+1e-6 {
			t.Fatalf("slot %d axial %.1f outside [0, %.1f]", s, p.X, UndockLaneMaxAxial)
		}
		if lat := math.Hypot(p.Y, p.Z); lat > UndockLaneMaxLateral+1e-6 {
			t.Fatalf("slot %d lateral %.1f > %.1f", s, lat, UndockLaneMaxLateral)
		}
	}
}
