package dogma

// WichmannHill reimplements Python 2.3's random.WichmannHill generator. The 3913 client replays
// chance-based effect cycles (ECM, sensor jams, chance webs/scrams) by seeding it with the
// 3-tuple randomSeed from OnGodmaShipEffect, so the server must draw identical rolls.
type WichmannHill struct {
	x, y, z int64
}

// NewWichmannHill creates a generator from a 3-tuple seed. Each component
// must be >= 1 (the client assigns the tuple raw to the generator state).
func NewWichmannHill(x, y, z int64) *WichmannHill {
	if x < 1 {
		x = 1
	}
	if y < 1 {
		y = 1
	}
	if z < 1 {
		z = 1
	}
	return &WichmannHill{x: x, y: y, z: z}
}

// Seed returns the current state as the 3-tuple to put on the wire.
func (w *WichmannHill) Seed() (x, y, z int64) { return w.x, w.y, w.z }

// Random returns the next value in [0.0, 1.0), matching CPython 2.3
// WichmannHill.random() bit-for-bit.
func (w *WichmannHill) Random() float64 {
	w.x = (171 * w.x) % 30269
	w.y = (172 * w.y) % 30307
	w.z = (170 * w.z) % 30323
	r := float64(w.x)/30269.0 + float64(w.y)/30307.0 + float64(w.z)/30323.0
	return r - float64(int64(r)) // mod 1.0 (r < 3, always positive)
}
