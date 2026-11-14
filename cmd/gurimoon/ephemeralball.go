package main

// Ephemeral ball IDs: balls with no entity row (force field, cyno beacon, open-site beacon) take an
// ID folded from their source item into a per-kind band, above db.DunChatOwnerBase and below 2^31
// because the client reads ball IDs as signed. See docs/cmd-misc-notes.md#ephemeral-ball-ids.
const (
	ephemeralBallSpan = 60_000_000

	forcefieldBallBand = 2_010_000_000 // .. 2_069_999_999
	cynoBeaconBallBand = 2_080_000_000 // .. 2_139_999_999, under 2^31-1

	// openSiteBallBand holds the beacons of spawner-placed open dungeon sites
	// (opensites.go), in the 10M gap between the force field and cyno bands.
	// A site ID is folded in modulo openSiteBallSpan.
	openSiteBallBand = 2_070_000_000 // .. 2_079_999_999
	openSiteBallSpan = 10_000_000
)

// ephemeralBallID is a source item's ball ID in one band.
func ephemeralBallID(band, sourceID int64) uint32 {
	off := sourceID % ephemeralBallSpan
	if off < 0 {
		off += ephemeralBallSpan
	}
	return uint32(band + off)
}
