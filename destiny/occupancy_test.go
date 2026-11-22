package destiny

import (
	"testing"
	"time"
)

// Each test uses its own sysID: arrivalLanes is process-global.

// A ship that claims a landing twice keeps one seat: the second claim replaces the first.
func TestClaimArrivalReplacesOwnReservation(t *testing.T) {
	const sys = 9_100_001
	base, dir := Vec3{}, Vec3{X: 1}
	_, c1 := ClaimArrivalPointEx(nil, sys, base, dir, 50, 7)
	_, c2 := ClaimArrivalPointEx(nil, sys, base, dir, 50, 7)
	if c1.Slot != 0 || c2.Slot != 0 {
		t.Fatalf("same owner slots = %d, %d; want 0, 0", c1.Slot, c2.Slot)
	}
	_, c3 := ClaimArrivalPointEx(nil, sys, base, dir, 50, 8)
	if c3.Slot != 1 {
		t.Fatalf("other owner slot = %d, want 1", c3.Slot)
	}
}

// An anonymous claim (owner 0) never replaces anything, as before owners existed.
func TestClaimArrivalOwnerZeroKeepsOldBehaviour(t *testing.T) {
	const sys = 9_100_002
	base, dir := Vec3{}, Vec3{X: 1}
	_, c1 := ClaimArrivalPointEx(nil, sys, base, dir, 50, 0)
	_, c2 := ClaimArrivalPointEx(nil, sys, base, dir, 50, 0)
	if c1.Slot != 0 || c2.Slot != 1 {
		t.Fatalf("anonymous slots = %d, %d; want 0, 1", c1.Slot, c2.Slot)
	}
}

// A landing still clear when looked at again stays where it was.
func TestRecheckArrivalKeepsClearPoint(t *testing.T) {
	const sys = 9_100_003
	base, dir := Vec3{}, Vec3{X: 1}
	p, _ := ClaimArrivalPointEx(nil, sys, base, dir, 50, 7)
	got, _, moved := RecheckArrivalPoint(nil, sys, p, base, dir, 50, 7)
	if moved || got != p {
		t.Fatalf("recheck = %v moved=%v, want %v unmoved", got, moved, p)
	}
	// Still one seat for owner 7: a third ship gets slot 1, not 2.
	_, c := ClaimArrivalPointEx(nil, sys, base, dir, 50, 9)
	if c.Slot != 1 {
		t.Fatalf("next owner slot = %d, want 1", c.Slot)
	}
}

// A landing another ship has since been given a reservation on moves off it.
func TestRecheckArrivalMovesOffForeignReservation(t *testing.T) {
	const sys = 9_100_004
	base, dir := Vec3{}, Vec3{X: 1}
	p, _ := ClaimArrivalPointEx(nil, sys, base, dir, 50, 7)
	// Owner 8 reserves exactly 7's point (as a writer that ignored the lane would).
	withLiveLane(sys, true, func(lane *arrivalLane, now time.Time) {
		lane.res = append(lane.res, arrivalReservation{
			at: BallFootprint{Pos: p, Radius: 50}, until: now.Add(arrivalLaneHold), owner: 8,
		})
	})
	got, _, moved := RecheckArrivalPoint(nil, sys, p, base, dir, 50, 7)
	if !moved || got == p {
		t.Fatalf("recheck = %v moved=%v, want a new point", got, moved)
	}
	if !PointClear(got, 50, []BallFootprint{{Pos: p, Radius: 50}}) {
		t.Fatalf("moved point %v still overlaps the foreign seat at %v", got, p)
	}
}

// The snapshot leaves out the asker's own reservation and keeps everyone else's.
func TestOccupancySnapshotExcludesOwner(t *testing.T) {
	const sys = 9_100_005
	base, dir := Vec3{}, Vec3{X: 1}
	ClaimArrivalPointEx(nil, sys, base, dir, 50, 7)
	ClaimArrivalPointEx(nil, sys, base, dir, 50, 8)
	if n := len(OccupancySnapshot(nil, sys, base, 50_000, 7)); n != 1 {
		t.Fatalf("snapshot for owner 7 has %d footprints, want 1", n)
	}
	if n := len(OccupancySnapshot(nil, sys, base, 50_000, 0)); n != 2 {
		t.Fatalf("anonymous snapshot has %d footprints, want 2", n)
	}
	if n := len(OccupancySnapshot(nil, 9_100_999, base, 50_000, 0)); n != 0 {
		t.Fatalf("snapshot of a system with no lane has %d footprints, want 0", n)
	}
}

// A ship in warp occupies its landing, not its departure point.
func TestOccupancyFootprintMidWarp(t *testing.T) {
	m := New(1, 1e6, 1, 300, 50)
	e := &Entity{ID: 1, Manager: m}
	fp, ok := e.OccupancyFootprint()
	if !ok || fp.Pos != (Vec3{}) || fp.Radius != 50 {
		t.Fatalf("stopped footprint = %+v ok=%v, want origin r50", fp, ok)
	}
	dest := Vec3{X: 1e9}
	m.WarpTo(dest, 1, 1, 0)
	fp, ok = e.OccupancyFootprint()
	if !ok || fp.Pos != dest {
		t.Fatalf("warping footprint = %+v ok=%v, want %v", fp, ok, dest)
	}
}

// PointClear uses radii plus ArrivalClearMargin, exclusive at the boundary.
func TestPointClear(t *testing.T) {
	other := []BallFootprint{{Pos: Vec3{}, Radius: 100}}
	sep := 50 + 100 + ArrivalClearMargin
	cases := []struct {
		d    float64
		want bool
	}{
		{sep - 1, false},
		{sep, true},
		{sep + 1, true},
		{0, false},
	}
	for _, c := range cases {
		if got := PointClear(Vec3{X: c.d}, 50, other); got != c.want {
			t.Errorf("PointClear at %.0f m = %v, want %v", c.d, got, c.want)
		}
	}
	if !PointClear(Vec3{}, 50, nil) {
		t.Errorf("PointClear with nothing around = false, want true")
	}
}
