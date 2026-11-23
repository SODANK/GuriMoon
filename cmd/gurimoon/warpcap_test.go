package main

import (
	"math"
	"testing"
)

// Rifter (587): mass 1,100,000 kg, warpCapacitorNeed 0.00000224, capacitor 200 GJ.
const (
	rifterMass    = 1_100_000.0
	rifterWarpCap = 0.00000224
)

func approx(a, b float64) bool { return math.Abs(a-b) <= 1e-9*math.Max(1, math.Abs(b)) }

func TestWarpCapPerMeterRifter(t *testing.T) {
	perAU := warpCapPerMeter(rifterMass, rifterWarpCap) * metersPerAU
	if !approx(perAU, 2.464) {
		t.Fatalf("Rifter cap per AU = %v, want 2.464 GJ", perAU)
	}
	// Warp Drive Operation V halves it.
	if got := warpCapPerMeter(rifterMass, rifterWarpCap*0.5) * metersPerAU; !approx(got, 1.232) {
		t.Fatalf("Rifter cap per AU at WDO V = %v, want 1.232 GJ", got)
	}
}

// An online plate's massAddition is paid for like hull mass: a 1600mm Reinforced Steel
// Plates I (11279, attr 796 = 3,750,000 kg) on a Rifter more than quadruples its warp cost.
func TestWarpCapMassWithPlate(t *testing.T) {
	const plate = 3_750_000.0
	perAU := warpCapPerMeter(rifterMass+plate, rifterWarpCap) * metersPerAU
	if !approx(perAU, (rifterMass+plate)*rifterWarpCap) {
		t.Fatalf("plated Rifter cap per AU = %v, want %v", perAU, (rifterMass+plate)*rifterWarpCap)
	}
	if !approx(perAU, 10.864) {
		t.Fatalf("plated Rifter cap per AU = %v, want 10.864 GJ", perAU)
	}
}

func TestWarpCapMassFallsBackToHull(t *testing.T) {
	ds := &destinyConn{}
	if got := warpCapMass(ds, nil, rifterMass); got != rifterMass {
		t.Fatalf("no DB: warpCapMass = %v, want hull %v", got, rifterMass)
	}
}

func TestWarpCapPerMeterFree(t *testing.T) {
	if warpCapPerMeter(0, rifterWarpCap) != 0 || warpCapPerMeter(rifterMass, 0) != 0 {
		t.Fatal("no mass or no warpCapacitorNeed must cost nothing")
	}
}

func TestPlanWarpCapCovered(t *testing.T) {
	perMeter := warpCapPerMeter(rifterMass, rifterWarpCap)
	p := planWarpCap(perMeter, 200, 10*metersPerAU)
	if p.short || p.refuse || !approx(p.cost, 24.64) {
		t.Fatalf("10 AU on full cap: %+v, want cost 24.64 and no shortening", p)
	}
}

func TestPlanWarpCapShortened(t *testing.T) {
	perMeter := warpCapPerMeter(rifterMass, rifterWarpCap)
	// 100 AU costs 246.4 GJ; 200 GJ carries the ship 200/2.464 AU.
	p := planWarpCap(perMeter, 200, 100*metersPerAU)
	if !p.short || p.refuse {
		t.Fatalf("100 AU on 200 GJ: %+v, want a shortened warp", p)
	}
	if !approx(p.cost, 200) {
		t.Fatalf("shortened warp drains %v, want the whole 200 GJ", p.cost)
	}
	if !approx(p.reach/metersPerAU, 200/2.464) {
		t.Fatalf("reach = %v AU, want %v", p.reach/metersPerAU, 200/2.464)
	}
}

func TestPlanWarpCapRefused(t *testing.T) {
	perMeter := warpCapPerMeter(rifterMass, rifterWarpCap)
	// Under the minimum warp distance's worth of charge.
	minCost := perMeter * minWarpDistanceM
	p := planWarpCap(perMeter, minCost*0.5, 5*metersPerAU)
	if !p.refuse || p.short || p.cost != 0 {
		t.Fatalf("near-empty cap: %+v, want refusal", p)
	}
	if !approx(p.need, minCost) || !approx(p.got, minCost*0.5) {
		t.Fatalf("refusal need/got = %v/%v, want %v/%v", p.need, p.got, minCost, minCost*0.5)
	}
}

func TestPlanWarpCapFree(t *testing.T) {
	if p := planWarpCap(0, 0, 10*metersPerAU); p != (warpCapPlan{}) {
		t.Fatalf("free warp planned %+v", p)
	}
	if p := planWarpCap(1, 0, 0); p != (warpCapPlan{}) {
		t.Fatalf("zero-length warp planned %+v", p)
	}
}
