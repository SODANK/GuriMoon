package destiny

import (
	"math"
	"testing"
	"time"
)

func TestSweepContactHeadOn(t *testing.T) {
	// a runs at b along +X; radii sum 100. Starts 500 apart, ends 300 past.
	n, ok := sweepContact(Vec3{X: -500}, Vec3{X: 300}, Vec3{}, Vec3{}, 100)
	if !ok {
		t.Fatal("head-on pass-through not detected")
	}
	if math.Abs(n.X+1) > 1e-9 || math.Abs(n.Y) > 1e-9 || math.Abs(n.Z) > 1e-9 {
		t.Fatalf("normal = %+v, want -X (from b to a)", n)
	}
}

func TestSweepContactMiss(t *testing.T) {
	if _, ok := sweepContact(Vec3{X: -500, Y: 150}, Vec3{X: 500, Y: 150}, Vec3{}, Vec3{}, 100); ok {
		t.Fatal("pass 150 m off-centre with radii 100 reported a contact")
	}
	if _, ok := sweepContact(Vec3{X: -500}, Vec3{X: -200}, Vec3{}, Vec3{}, 100); ok {
		t.Fatal("stopping short reported a contact")
	}
}

func TestSweepContactIgnoresStartOverlap(t *testing.T) {
	if _, ok := sweepContact(Vec3{X: -50}, Vec3{X: 50}, Vec3{}, Vec3{}, 100); ok {
		t.Fatal("balls already overlapping at tick start reported a contact")
	}
}

func TestBumpImpulseEqualMasses(t *testing.T) {
	a := bumpBody{vel: Vec3{X: 100}, invMass: 1e-6}
	b := bumpBody{vel: Vec3{}, invMass: 1e-6}
	n := Vec3{X: -1} // from b to a
	da, db, ok := bumpImpulse(&a, &b, n, 1)
	if !ok {
		t.Fatal("closing pair not bumped")
	}
	// Perfectly elastic, equal masses: velocities swap.
	if va := a.vel.Add(da); math.Abs(va.X) > 1e-6 {
		t.Fatalf("a after = %+v, want 0", va)
	}
	if vb := b.vel.Add(db); math.Abs(vb.X-100) > 1e-6 {
		t.Fatalf("b after = %+v, want 100", vb)
	}
}

func TestBumpImpulseWallAndFloor(t *testing.T) {
	a := bumpBody{vel: Vec3{X: 2}, invMass: 1e-6}
	wall := bumpBody{invMass: 0}
	da, db, ok := bumpImpulse(&a, &wall, Vec3{X: -1}, 0.6)
	if !ok {
		t.Fatal("ship into wall not bumped")
	}
	if !db.IsZero() {
		t.Fatalf("immovable wall moved: %+v", db)
	}
	// 2 m/s * 0.6 is under the floor: it leaves at bumpMinSeparation.
	if va := a.vel.Add(da); math.Abs(va.X+bumpMinSeparation) > 1e-9 {
		t.Fatalf("a after = %+v, want -%v", va, bumpMinSeparation)
	}
	// Moving apart: no bump.
	a.vel = Vec3{X: -10}
	if _, _, ok := bumpImpulse(&a, &wall, Vec3{X: -1}, 0.6); ok {
		t.Fatal("separating pair bumped")
	}
}

func testShip(id uint32, pos Vec3, radius float64) *Entity {
	m := New(id, 1e6, 1, 300, radius)
	m.Pos = pos
	return &Entity{ID: id, Manager: m, BubbleID: 0}
}

func TestResolveBumpsShipIntoShip(t *testing.T) {
	ss := NewSolarSystem(9_200_001)
	a := testShip(1, Vec3{X: -120}, 50)
	b := testShip(2, Vec3{}, 50)
	a.Manager.Vel = Vec3{X: 100}
	movers := []tickMover{{e: a, prev: Vec3{X: -150}}}
	a.Manager.Pos = Vec3{X: -95} // stepped into b this tick
	ss.resolveBumps(movers, []*Entity{a, b}, time.Now())
	// Equal masses, restitution 0.6: a keeps 20 m/s, b takes 80; they part at 60.
	if math.Abs(a.Manager.Vel.X-20) > 1e-6 || math.Abs(b.Manager.Vel.X-80) > 1e-6 {
		t.Fatalf("after bump a=%+v b=%+v, want 20 and 80", a.Manager.Vel, b.Manager.Vel)
	}
	// The pair is on cooldown: a second pass in the same instant changes nothing.
	va := a.Manager.Vel
	a.Manager.Vel = Vec3{X: 100}
	ss.resolveBumps(movers, []*Entity{a, b}, time.Now())
	if a.Manager.Vel != (Vec3{X: 100}) {
		t.Fatalf("pair re-bumped inside cooldown (first bump left %+v)", va)
	}
}

func TestResolveBumpsSkipsWarpAndCloak(t *testing.T) {
	ss := NewSolarSystem(9_200_002)
	a := testShip(1, Vec3{X: -95}, 50)
	b := testShip(2, Vec3{}, 50)
	a.Manager.Vel = Vec3{X: 100}
	a.cloaked = true
	ss.resolveBumps([]tickMover{{e: a, prev: Vec3{X: -150}}}, []*Entity{a, b}, time.Now())
	if a.Manager.Vel != (Vec3{X: 100}) || !b.Manager.Vel.IsZero() {
		t.Fatal("cloaked ship bumped")
	}
}

func TestResolveBumpsAsteroid(t *testing.T) {
	ss := NewSolarSystem(9_200_003)
	ss.AsteroidBalls = []BallDesc{{EntityID: 77, Mode: ModeRigid, Pos: Vec3{}, Radius: 1000}}
	ss.AsteroidQty = map[uint32]int64{77: 100}
	a := testShip(1, Vec3{X: -1020}, 50)
	a.Manager.Vel = Vec3{X: 100}
	ss.resolveBumps([]tickMover{{e: a, prev: Vec3{X: -1100}}}, []*Entity{a}, time.Now())
	if math.Abs(a.Manager.Vel.X+60) > 1e-6 {
		t.Fatalf("after rock bump vel = %+v, want -60 (restitution 0.6)", a.Manager.Vel)
	}
	// A mined-out rock is gone.
	ss.AsteroidQty[77] = 0
	ss.bumpCooldown = nil
	a.Manager.Vel = Vec3{X: 100}
	ss.resolveBumps([]tickMover{{e: a, prev: Vec3{X: -1100}}}, []*Entity{a}, time.Now())
	if a.Manager.Vel.X != 100 {
		t.Fatal("depleted rock still bumps")
	}
}
