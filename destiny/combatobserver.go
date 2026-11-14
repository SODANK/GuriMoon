package destiny

// Bystander combat log. ObserveCombat is the seam every landed hit passes through
// so pilots sharing a grid with a fight see it: Entity.RecordKillHit calls it for
// hits on players and rolebots, npc.NPCManager.NotifyDamage for hits on NPCs; the
// two never fire for one hit. Callers may hold an NPC record lock, so the observer
// must not take a SolarSystem or Entity lock and only enqueues.

// CombatObserver receives one landed hit. Nil by default (no bystander log); main
// installs it at boot. Called by ObserveCombat with a non-nil ss.
var CombatObserver func(ss *SolarSystem, victim, attacker *Entity, damage float64, destroyed bool)

// ObserveCombat reports a landed hit to the bystander log. ss may be nil; the
// victim's (then the attacker's) registered system is used. destroyed is
// reported once per victim however many killing calls race.
func ObserveCombat(ss *SolarSystem, victim, attacker *Entity, damage float64, destroyed bool) {
	obs := CombatObserver
	if obs == nil || victim == nil || attacker == nil || victim == attacker {
		return
	}
	if destroyed && !victim.deathObserved.CompareAndSwap(false, true) {
		destroyed = false
	}
	if damage <= 0 && !destroyed {
		return
	}
	if ss == nil {
		ss = victim.sys.Load()
	}
	if ss == nil {
		ss = attacker.sys.Load()
	}
	if ss == nil {
		return
	}
	obs(ss, victim, attacker, damage, destroyed)
}
