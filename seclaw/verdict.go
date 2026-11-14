package seclaw

// KillFacts is everything the law needs to price one kill, as plain values. No entity,
// solar system or connection appears here: the rolebots abstract tier resolves fights
// with no grid, and an offline pilot can still be charged.
type KillFacts struct {
	KillerCharID     int64
	KillerCorpID     int64
	KillerAllianceID int64
	KillerShipTypeID int64

	VictimCharID     int64
	VictimCorpID     int64
	VictimAllianceID int64

	SystemID int64
	Security float64

	// VictimOutlaw is set when the victim's record is at or below OutlawStatus. An outlaw
	// is a legal target for everyone; the crimewatch lookup cannot answer this, because it
	// is per system and absent from a fight resolved with no grid.
	VictimOutlaw bool

	// Pod is set when the thing destroyed was the capsule rather than the ship.
	Pod bool

	// VictimIsStructure is set when the target is a player-owned structure (tower, its
	// slaves, Mobile Storage). VictimCharID is 0, so the corp, alliance, war and jurisdiction
	// rungs apply and the pilot-based rungs do not.
	VictimIsStructure bool
}

// Exemption names why a shot was lawful. The empty string means it was not. Callers
// log it, so a quiet log after a kill is distinguishable from a broken hook.
type Exemption string

const (
	ExemptSelf     Exemption = "self"
	ExemptCorp     Exemption = "corpmate"
	ExemptAlliance Exemption = "alliance-mate"
	ExemptWar      Exemption = "war target"
	ExemptFlagged  Exemption = "victim already a legal target"
	ExemptLawless  Exemption = "outside police jurisdiction"
	// ExemptEngaged names a shot at a pilot who had aggressed the shooter or the shooter's
	// corporation inside the crimewatch window.
	ExemptEngaged Exemption = "victim had aggressed the shooter"
)

// Deps are the live lookups the ladder needs. Set by seclawDeps in
// cmd/gurimoon/concord.go, called by ShotIsCrime. A nil lookup grants no exemption
// from its rung.
type Deps struct {
	// WarShotIsLegal mirrors the client's IsWarInHostileState arithmetic, so the overview
	// and CONCORD agree on who is a legal target.
	WarShotIsLegal func(atkCorp, atkAlliance, vicCorp, vicAlliance int64) bool

	// VictimIsLegalTgt reports whether the victim is already shootable: a CONCORD response
	// inbound on them, or a criminal timer of their own.
	VictimIsLegalTgt func(sysID, victimCharID int64) bool

	// KillRightHeld reports whether the shooter holds a live kill right against the
	// victim (killright.go).
	KillRightHeld func(killerCharID, victimCharID int64) bool

	// VictimAggressedKiller reports whether the victim holds a live crimewatch entry
	// against the killer or the killer's corporation in this system. Taking from a can or
	// wreck writes such an entry, so the owner may shoot the thief.
	VictimAggressedKiller func(sysID, victimCharID, killerCharID, killerCorpID int64) bool

	// SentryQualifies reports whether this security band has police at all.
	SentryQualifies func(sec float64) bool
}

// ShotIsCrime runs CONCORD's exemption ladder and reports whether the shot was
// unlawful, naming the exemption when it was not. The rung order decides which reason
// is reported when two apply. The function is pure: it runs more than once per fight.
func ShotIsCrime(f KillFacts, d Deps) (bool, Exemption) {
	// No killer, no victim, or a pilot shooting themselves (self-destruct, rats).
	if f.VictimIsStructure {
		if f.KillerCharID <= 0 || f.VictimCorpID <= 0 {
			return false, ExemptSelf
		}
	} else if f.KillerCharID <= 0 || f.VictimCharID <= 0 || f.KillerCharID == f.VictimCharID {
		return false, ExemptSelf
	}
	// Corpmates are exempt. Player corps only: NPC-corp membership is not consent.
	if f.KillerCorpID != 0 && f.KillerCorpID == f.VictimCorpID {
		return false, ExemptCorp
	}
	// Alliance-mates are exempt (the client paints them blue).
	if f.KillerAllianceID != 0 && f.KillerAllianceID == f.VictimAllianceID {
		return false, ExemptAlliance
	}
	// A war shot is legal.
	if d.WarShotIsLegal != nil && d.WarShotIsLegal(
		f.KillerCorpID, f.KillerAllianceID, f.VictimCorpID, f.VictimAllianceID) {
		return false, ExemptWar
	}
	// An outlaw is a legal target for everyone. Checked before crimewatch because it
	// needs neither Deps nor a system.
	if f.VictimOutlaw {
		return false, ExemptFlagged
	}
	// Shooting an already-legal target is legal. The aggressor was flagged when they
	// opened fire, so the defender's kill is exempt and the aggressor's is not.
	if !f.VictimIsStructure && d.VictimIsLegalTgt != nil && d.VictimIsLegalTgt(f.SystemID, f.VictimCharID) {
		return false, ExemptFlagged
	}
	// A kill right comes after the flag rung so retaliation mid-fight is reported as such.
	if !f.VictimIsStructure && d.KillRightHeld != nil && d.KillRightHeld(f.KillerCharID, f.VictimCharID) {
		return false, ExemptKillRight
	}
	// A pilot who aggressed the shooter or their corp is fair game while the entry lasts.
	// After the flag and kill-right rungs, before jurisdiction (the entry exists in every band).
	if !f.VictimIsStructure && d.VictimAggressedKiller != nil &&
		d.VictimAggressedKiller(f.SystemID, f.VictimCharID, f.KillerCharID, f.KillerCorpID) {
		return false, ExemptEngaged
	}
	// Empire only: low-sec and high-sec have police, null-sec has none.
	if d.SentryQualifies != nil && !d.SentryQualifies(f.Security) {
		return false, ExemptLawless
	}
	return true, ""
}
