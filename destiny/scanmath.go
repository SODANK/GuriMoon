package destiny

import (
	"math"
	"math/rand"
	"sort"
)

// The classic probe scanner reduced to its arithmetic: a pilot launches probes of
// one class, selects at least probesInGroup of them and Analyzes against one ship
// group; every ship of that group covered by a probe is reported with a signal that
// grows with the probes covering it and a deviation that shrinks the same way.

// ProbeSpec is one probe as the scan sees it: where it is, how far it
// reaches, and the deviation band and group size of its class.
type ProbeSpec struct {
	Pos       Vec3
	RangeM    float64
	MinDev    float64 // metres, attribute 787 minScanDeviation
	MaxDev    float64 // metres, attribute 788 maxScanDeviation
	GroupSize int     // attribute 794 probesInGroup
	// Outside inverts coverage: the probe sees what lies beyond RangeM. The Observator
	// Deep Space Probe scans everything outside its scan range (its description); it
	// is the only class with probesInGroup 1.
	Outside bool
}

// Covers reports whether the probe sees a target at pos.
func (p ProbeSpec) Covers(pos Vec3) bool {
	in := pos.Sub(p.Pos).LenSq() <= p.RangeM*p.RangeM
	return in != p.Outside
}

// ScanTarget is a candidate ball.
type ScanTarget struct {
	ID  uint32
	Pos Vec3
}

// ScanResult is one row of the combined result: the client reads it as
// (id, signal, pos, accuracy).
type ScanResult struct {
	ID       uint32
	Signal   float64 // percent, 0..100
	Pos      Vec3    // reported (deviated) position
	Accuracy float64 // the deviation radius the reported position lies within
}

// CombineProbeScan lists every target at least one probe covers. Signal is the
// share of the probe group covering it; deviation falls linearly from MaxDev to
// MinDev, taken from the coarsest probe so click order cannot decide accuracy.
// The offset is seeded by seed^ID so a repeated GetScans reports the same point.
func CombineProbeScan(probes []ProbeSpec, targets []ScanTarget, seed int64) []ScanResult {
	if len(probes) == 0 {
		return nil
	}
	groupSize := probes[0].GroupSize
	if groupSize < 1 {
		groupSize = 1
	}
	var out []ScanResult
	for _, t := range targets {
		n := 0
		for _, p := range probes {
			if p.Covers(t.Pos) {
				n++
			}
		}
		if n == 0 {
			continue
		}
		if n > groupSize {
			n = groupSize
		}
		signal := 100 * float64(n) / float64(groupSize)
		dev := 0.0
		for _, p := range probes {
			dev = math.Max(dev, probeDeviation(p, n, groupSize))
		}
		rng := rand.New(rand.NewSource(seed ^ int64(t.ID)<<16))
		out = append(out, ScanResult{
			ID:       t.ID,
			Signal:   math.Round(signal),
			Pos:      t.Pos.Add(randomOffset(rng, dev)),
			Accuracy: dev,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// probeDeviation interpolates the class's deviation band by coverage.
func probeDeviation(p ProbeSpec, covering, groupSize int) float64 {
	if groupSize <= 1 {
		return math.Max(p.MinDev, 0)
	}
	frac := float64(covering-1) / float64(groupSize-1)
	dev := p.MaxDev - (p.MaxDev-p.MinDev)*frac
	if dev < p.MinDev {
		dev = p.MinDev
	}
	if dev < 0 {
		dev = 0
	}
	return dev
}

// randomOffset is a uniformly directed vector of length in [0, radius].
func randomOffset(rng *rand.Rand, radius float64) Vec3 {
	if radius <= 0 {
		return Vec3{}
	}
	// Marsaglia: a uniform point on the sphere, scaled by a uniform radius.
	var x, y, s float64
	for {
		x, y = 2*rng.Float64()-1, 2*rng.Float64()-1
		s = x*x + y*y
		if s > 0 && s < 1 {
			break
		}
	}
	k := 2 * math.Sqrt(1-s)
	dir := Vec3{X: x * k, Y: y * k, Z: 1 - 2*s}
	return dir.Scale(rng.Float64() * radius)
}
