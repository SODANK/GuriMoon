package gang

// WarpOrder is one gang warp (the client's "Warp Gang" entry): the leader's resolved destination,
// handed to every member with no connection of its own (a module's bots). The core cannot tell
// whether they are in space, so grid and system checks are the receiver's job.
type WarpOrder struct {
	GangID   int64
	LeaderID int64
	SystemID int64

	// TargetID is the celestial (or starbase ball, beacon, bookmark-resolved
	// object) the leader warped to; 0 for a point with no object behind it.
	TargetID int64
	// X/Y/Z is the point the warp is aimed at (the celestial's centre, not the landing).
	X, Y, Z float64
	// ExitDist is how far short of that point the leader's own warp stops
	// (target radius + warp-to range), before the arrival lane moves anyone.
	ExitDist float64

	LeaderX, LeaderY, LeaderZ float64

	Members []int64
}

// ModuleWarp is set by a module to receive the unconnected half of every gang warp. Nil: only
// connected pilots warp. Called on the leader's connection goroutine, so a receiver must not block.
var ModuleWarp func(WarpOrder)
