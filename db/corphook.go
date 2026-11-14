package db

// CorpMembershipHook is called synchronously after every write that moves a character's
// corporationID (JoinCorporation, ESPUpdateCharacterCorp), old corporation first, and must not
// fail its caller. cmd/gurimoon installs it at boot (corpmembership.go) to refresh the session
// registry's CorpID and push OnCorporationMemberChanged to both corps.
var CorpMembershipHook func(charID, oldCorpID, newCorpID int64)

func fireCorpMembershipHook(charID, oldCorpID, newCorpID int64) {
	if CorpMembershipHook != nil && oldCorpID != newCorpID {
		CorpMembershipHook(charID, oldCorpID, newCorpID)
	}
}
