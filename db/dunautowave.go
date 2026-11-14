package db

import "sort"

// Auto waves: a held wave group no trigger names. It is released automatically in order:
// once the room is quiet, the lowest unreleased wave comes out, then the next when that
// one is dead. If the next wave in order is named by a trigger, nothing auto-releases and
// later waves wait. The core (dungeon_triggers.go) and the rolebots site replay both call
// NextAutoWave. See docs/cmd-misc-notes.md#dungeon-triggers.

// AutoWaveEvent labels a wave released by no trigger where a trigger event name would
// otherwise go (ESP, the rolebots site card).
const AutoWaveEvent = "auto"

// WaveHasTrigger reports whether any spawnWave trigger names the wave.
func WaveHasTrigger(trigs []TriggerRow, wave int64) bool {
	for _, t := range trigs {
		if t.Action == "spawnWave" && t.SpawnWave.Valid && t.SpawnWave.Int64 == wave {
			return true
		}
	}
	return false
}

// RoomWaveGroups is the distinct held wave groups (>= 1) among a room's objects,
// ascending.
func RoomWaveGroups(objs []ObjectRow) []int64 {
	seen := map[int64]bool{}
	var out []int64
	for _, o := range objs {
		if !o.SpawnWave.Valid || o.SpawnWave.Int64 <= 0 || seen[o.SpawnWave.Int64] {
			continue
		}
		seen[o.SpawnWave.Int64] = true
		out = append(out, o.SpawnWave.Int64)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// NextAutoWave is the wave a quiet room releases on its own: the lowest held wave group
// not yet released, if no spawnWave trigger names it. 0 when nothing is left or the next
// wave in order belongs to a trigger. waves need not be sorted.
func NextAutoWave(waves []int64, trigs []TriggerRow, released func(wave int64) bool) int64 {
	next := int64(0)
	for _, w := range waves {
		if w <= 0 || (released != nil && released(w)) {
			continue
		}
		if next == 0 || w < next {
			next = w
		}
	}
	if next == 0 || WaveHasTrigger(trigs, next) {
		return 0
	}
	return next
}

// AutoWaveRatsPending reports whether a room's auto chain still has rats to let out: walking the
// unreleased held waves upward, stopping at the first one a trigger names, any wave hasRats.
// The open-site clear rule waits on it, since a quiet room releases those waves by itself.
func AutoWaveRatsPending(waves []int64, trigs []TriggerRow, released, hasRats func(wave int64) bool) bool {
	var left []int64
	for _, w := range waves {
		if w > 0 && (released == nil || !released(w)) {
			left = append(left, w)
		}
	}
	sort.Slice(left, func(i, j int) bool { return left[i] < left[j] })
	for _, w := range left {
		if WaveHasTrigger(trigs, w) {
			return false
		}
		if hasRats(w) {
			return true
		}
	}
	return false
}
