package destiny

// NearBall is a ball found by SolarSystem.NearestBallWithin.
type NearBall struct {
	ID     uint32
	TypeID int64
	// Dist is the gap between the two surfaces in metres, 0 when they overlap.
	Dist float64
}

// NearestBallWithin returns the ball whose surface lies nearest to the surface of a sphere of
// the given radius at pos, among the balls within rng metres of it, and false when none is.
// It walks every kind of ball a system holds: entities (ships, NPCs, drones, mines), celestials
// and stations, asteroids with ore left, loot containers and starbase structures. Skipped: the
// ball exclude, destroyed entities, entities in or entering warp, and pilots in GM mode or under
// the GM cloak, who must not disturb what they watch. Safe for concurrent use.
func (ss *SolarSystem) NearestBallWithin(pos Vec3, radius, rng float64, exclude uint32) (NearBall, bool) {
	var best NearBall
	found := false
	consider := func(id uint32, typeID int64, p Vec3, r float64) {
		gap := p.Sub(pos).Len() - radius - r
		if gap > rng {
			return
		}
		if gap < 0 {
			gap = 0
		}
		if !found || gap < best.Dist {
			best, found = NearBall{ID: id, TypeID: typeID, Dist: gap}, true
		}
	}

	for _, e := range ss.AllEntities() {
		if e.ID == exclude || e.Manager == nil || e.IsDestroyed() || e.IsGMMode() || e.IsGMCloak() ||
			e.IsInWarp() || e.IsWarpPending() {
			continue
		}
		bd := e.BallDesc()
		consider(e.ID, e.Slim.TypeID, bd.Pos, bd.Radius)
	}

	ss.mu.RLock()
	defer ss.mu.RUnlock()
	slices := []struct {
		balls []BallDesc
		slims []SlimItem
	}{
		{ss.CelBalls, ss.CelSlims},
		{ss.LootBalls, ss.LootSlims},
		{ss.StarbaseBalls, ss.StarbaseSlims},
	}
	for _, s := range slices {
		for i, bd := range s.balls {
			if bd.EntityID == exclude {
				continue
			}
			var typeID int64
			if i < len(s.slims) {
				typeID = s.slims[i].TypeID
			}
			consider(bd.EntityID, typeID, bd.Pos, bd.Radius)
		}
	}
	for i, bd := range ss.AsteroidBalls {
		if bd.EntityID == exclude || ss.AsteroidQty[bd.EntityID] <= 0 {
			continue
		}
		var typeID int64
		if i < len(ss.AsteroidSlims) {
			typeID = ss.AsteroidSlims[i].TypeID
		}
		consider(bd.EntityID, typeID, bd.Pos, bd.Radius)
	}
	return best, found
}
