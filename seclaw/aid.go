package seclaw

// ExemptNotCriminal is the aid rule's reason for a lawful act: the target was neither a criminal
// nor an outlaw. It sits here, not with the shot-ladder exemptions, because the aid rule is separate.
const ExemptNotCriminal Exemption = "target not a criminal"

// AidFacts is what the law needs to price one act of remote assistance (shield transporter,
// armour projector, energy transfer). Like KillFacts it holds plain values only, so the rolebots
// module's unwatched tier prices the same act through the same function.
type AidFacts struct {
	AiderCharID     int64
	AiderCorpID     int64
	AiderAllianceID int64

	TargetCharID     int64
	TargetCorpID     int64
	TargetAllianceID int64
	// TargetSecStatus is the target's security record, -10 to +5.
	TargetSecStatus float64

	SystemID int64
	Security float64
}

// AidDeps are the two live lookups the aid rule needs, supplied by the caller. A nil lookup reads
// as "not flagged", so a caller that cannot answer does not make every repair a crime.
type AidDeps struct {
	// TargetIsLegalTgt reports whether the target is already shootable for everybody (a CONCORD
	// response inbound on them, or a criminal timer of their own). Same lookup as Deps.VictimIsLegalTgt.
	TargetIsLegalTgt func(sysID, targetCharID int64) bool

	// SentryQualifies reports whether this security band has police at all.
	SentryQualifies func(sec float64) bool
}

// AidIsCrime reports whether helping the target is an unlawful act and names the exemption when
// it was not. It mirrors the client's aid confirmation (consider.DoAidConfirmations): aiding a
// global criminal or outlaw in empire space is a crime; self, corp, alliance and null-sec are
// exempt, and NPC targets (no character ID) never reach the rungs. Pure: the caller acts on the verdict.
func AidIsCrime(f AidFacts, d AidDeps) (bool, Exemption) {
	if f.AiderCharID <= 0 || f.TargetCharID <= 0 || f.AiderCharID == f.TargetCharID {
		return false, ExemptSelf
	}
	if f.AiderCorpID != 0 && f.AiderCorpID == f.TargetCorpID {
		return false, ExemptCorp
	}
	if f.AiderAllianceID != 0 && f.AiderAllianceID == f.TargetAllianceID {
		return false, ExemptAlliance
	}
	if d.SentryQualifies != nil && !d.SentryQualifies(f.Security) {
		return false, ExemptLawless
	}
	if f.TargetSecStatus <= OutlawStatus {
		return true, ""
	}
	if d.TargetIsLegalTgt != nil && d.TargetIsLegalTgt(f.SystemID, f.TargetCharID) {
		return true, ""
	}
	return false, ExemptNotCriminal
}
