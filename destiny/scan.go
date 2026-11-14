package destiny

import "math"

// ScanBall is one ball a scanner can report: enough for the client's
// directional-scan row (it reads id, typeID and groupID, and resolves the
// name itself) and for the probe scanner's position arithmetic.
type ScanBall struct {
	ID      uint32
	TypeID  int64
	GroupID int64
	Pos     Vec3
	// Asteroid marks a rock: the client names those from their type
	// ("Asteroid (Veldspar)") and never looks them up by ID.
	Asteroid bool
}

// GroupCosmicSignature is invGroups 502. A probe-only open site's marker
// ball carries it (type 19728 "Deadspace Signature"); it is found by a probe
// Analyze, never by the directional scanner or the overview.
const GroupCosmicSignature = 502

// InCone reports whether target lies within rng of from and inside the cone of
// half-angle acos(cosHalf) around dir (a unit vector). cosHalf <= -1 means no
// angular test (the client's 360 degree setting). A target exactly on the ship
// has no direction and is counted in.
func InCone(from, dir, target Vec3, cosHalf, rng float64) bool {
	d := target.Sub(from)
	distSq := d.LenSq()
	if distSq > rng*rng {
		return false
	}
	if cosHalf <= -1 {
		return true
	}
	dist := math.Sqrt(distSq)
	if dist < 1e-6 {
		return true
	}
	return d.Dot(dir)/dist >= cosHalf
}

// ConeScan is the directional scanner: every ball in the system within rng of from
// and inside the cone, less exclude (the scanning ship), cloaked pilots, ships
// mid-warp (their manager position is the departure point), cosmic signature
// markers (probe-only) and billboards (no row anywhere could name one).
func (ss *SolarSystem) ConeScan(from, dir Vec3, cosHalf, rng float64, exclude uint32) []ScanBall {
	var out []ScanBall
	seen := map[uint32]bool{exclude: true}
	add := func(id uint32, typeID, groupID int64, pos Vec3) bool {
		if seen[id] || groupID == GroupCosmicSignature || !InCone(from, dir, pos, cosHalf, rng) {
			return false
		}
		seen[id] = true
		out = append(out, ScanBall{ID: id, TypeID: typeID, GroupID: groupID, Pos: pos})
		return true
	}

	for _, e := range ss.AllEntities() {
		if e == nil || e.ID == exclude || e.IsCloaked() || e.IsInWarp() {
			continue
		}
		add(e.ID, e.Slim.TypeID, e.Slim.GroupID, e.Pos())
	}

	ss.mu.RLock()
	defer ss.mu.RUnlock()
	for i, bd := range ss.CelBalls {
		if i < len(ss.CelSlims) {
			add(bd.EntityID, ss.CelSlims[i].TypeID, ss.CelSlims[i].GroupID, bd.Pos)
		}
	}
	for i, bd := range ss.AsteroidBalls {
		if ss.AsteroidQty[bd.EntityID] <= 0 || i >= len(ss.AsteroidSlims) {
			continue
		}
		if add(bd.EntityID, ss.AsteroidSlims[i].TypeID, ss.AsteroidSlims[i].GroupID, bd.Pos) {
			out[len(out)-1].Asteroid = true
		}
	}
	for i, bd := range ss.StarbaseBalls {
		if i < len(ss.StarbaseSlims) {
			add(bd.EntityID, ss.StarbaseSlims[i].TypeID, ss.StarbaseSlims[i].GroupID, bd.Pos)
		}
	}
	for i, bd := range ss.LootBalls {
		if i < len(ss.LootSlims) {
			add(bd.EntityID, ss.LootSlims[i].TypeID, ss.LootSlims[i].GroupID, bd.Pos)
		}
	}
	return out
}
