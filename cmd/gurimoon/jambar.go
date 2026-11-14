package main

import (
	"gurimoon/destiny"
	"gurimoon/marshal"
	"gurimoon/safego"
	"gurimoon/server"
	"gurimoon/syncx"
	"time"
)

// Jam bars: the countdown rows shipui.py stacks above the HUD and the Jam/Scr/
// Msc rows target.py draws on a target box. Both run on OnJamStart/OnJamEnd,
// which the stock client raises only from its own godma EffectRepeater, so a
// victim is never shown another ship's effect. The server emits the events
// itself over the dogma-event slot of DoDestinyUpdate (destiny.BuildJamEvent).
// Both readers filter on the target, so events go to the whole grid.

// jamEmitAt records when a start event was last sent for an effect key, so a
// lease-based effect (warp scramble) can keep its bar alive without emitting on
// every combat tick. Entries are dropped when the effect stops.
var jamEmitAt syncx.Map[int64, time.Time] // effect key (int64) to time.Time

func noteJamEmitted(key int64)   { jamEmitAt.Store(key, time.Now()) }
func forgetJamEmitted(key int64) { jamEmitAt.Delete(key) }

// claimJamRefresh reports whether a bar of holdMs is stale enough to re-send
// (three quarters of the hold elapsed) and books the send if so. A refresh
// carries no action, and michelle logs an update holding only dogma events, so
// it is not sent every tick.
func claimJamRefresh(key int64, holdMs int64) bool {
	if t, ok := jamEmitAt.Load(key); ok {
		if time.Since(t) < time.Duration(holdMs)*3*time.Millisecond/4 {
			return false
		}
	}
	noteJamEmitted(key)
	return true
}

// broadcastJamEvent sends one jam start or stop to every player on the source's
// grid. moduleID only has to be stable for the life of the effect: the client
// keys bars on (source, module, target), so re-sending a start under the same
// triple refreshes the bar instead of stacking a second one.
func broadcastJamEvent(ss *destiny.SolarSystem, sourceID uint32, moduleID int64,
	targetID uint32, jamType string, durationMs int64, start bool) {

	if ss == nil || targetID == 0 {
		return
	}
	event := destiny.BuildJamEvent(start, sourceID, moduleID, targetID, jamType,
		server.Win32TimeNow(), durationMs)
	send := func(p *destiny.Entity) {
		if p == nil || p.Conn == nil {
			return
		}
		_ = p.SendFXStamped(func(stamp uint32) error {
			return destiny.SendActionListEvents(p.Conn, globalNodeID, p.AccountID, stamp,
				[]marshal.PyRep{event})
		})
	}
	for _, p := range ss.GetBubbleConnectedPlayersExcept(sourceID) {
		if !p.KnowsBall(sourceID) {
			continue
		}
		send(p)
	}
	// The source too, when it is a pilot: the target box he has open on the
	// victim draws the same row, which is how he sees his own EWAR holding.
	if src := ss.GetEntity(sourceID); src != nil && src.IsPlayer {
		send(src)
	}
}

// scheduleJamEventEnd closes a bar that has no stop event of its own. An area
// jam is the case: it picks its victims once and never cycles per victim, so
// nothing would ever clear their entry and a later CheckJam could resurrect an
// expired row.
func scheduleJamEventEnd(ss *destiny.SolarSystem, sourceID uint32, moduleID int64,
	targetID uint32, jamType string, durationMs int64) {

	if ss == nil || targetID == 0 || durationMs <= 0 {
		return
	}
	go func() {
		defer safego.Recover("scheduleJamEventEnd")
		select {
		case <-time.After(time.Duration(durationMs) * time.Millisecond):
		case <-ss.Done():
			return
		}
		broadcastJamEvent(ss, sourceID, moduleID, targetID, jamType, 0, false)
	}()
}
