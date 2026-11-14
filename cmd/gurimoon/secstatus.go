package main

// Security status: the record CONCORD keeps on a pilot. Every path that moves
// it (a player's guns, a bot's guns, an abstract fight, a rat, the police)
// arrives at applyKillPenalty or applySecStatusGain, and both decide with
// seclaw, which CONCORD itself consults before dispatching.

import (
	"context"
	"database/sql"
	"log"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/marshal"
	"gurimoon/npc"
	"gurimoon/safego"
	"gurimoon/seclaw"
	"gurimoon/server/infocache"
	"gurimoon/syncx"
	"gurimoon/tick"
)

// publishSecStatus is the tail every security-status write shares: the info
// cache, the client's standing cache, the entity's slim, the connection's copy
// and the bubble broadcast that repaints Criminal and Dangerous in overviews.
// charEntity may be nil (offline, or no projection); the row has already moved.
func publishSecStatus(charEntity *destiny.Entity, charID int64, newRating float64) {
	// The info window memoises security status for 60 seconds
	// (standing2svc.handleGetSecurityRating); drop that copy first.
	infocache.Shared.InvalidateID(charID)

	if charEntity == nil {
		return
	}
	charEntity.Slim.SecurityStatus = newRating
	kds, ok := charEntity.PlayerState.(*destinyConn)
	if !ok {
		// A rolebot projection: an IsPlayer entity with no connection. The slim is
		// what other pilots see; there is no client to notify.
		return
	}
	oldRating := kds.securityStatus
	kds.securityStatus = newRating

	// Refresh the client's local standing cache (the character sheet reads
	// security status out of the CONCORD standing row).
	if charEntity.Conn != nil {
		sendStandingNotification(charEntity.Conn, int64(charEntity.AccountID),
			evedb.OwnerCONCORD, charID, oldRating, newRating)
	}

	// Push the updated slim to bubble-mates so overview/bracket flag checks
	// (Criminal < -5.0, Dangerous < -0.1) see the new value. A record that just
	// crossed the band's line brings the police (factionpolice.go).
	if kds.solarSystem != nil {
		policeCheck(kds.solarSystem, charEntity)
		for _, mate := range kds.solarSystem.GetBubbleConnectedPlayersExcept(charEntity.ID) {
			if err := mate.SendStamped(func(stamp uint32) error {
				return destiny.SendSlimItemChange(mate.Conn, globalNodeID, mate.AccountID, stamp, charEntity.Slim)
			}); err != nil {
				log.Printf("[secstatus] SendSlimItemChange to mate %d: %v", mate.ID, err)
			}
		}
	}
}

// applySecStatusChange moves a character's security status by delta, logs the
// standing transaction the character sheet renders (eventTypeID is an
// evedb.EventStanding* constant; subjectID int_1; shipTypeID int_3) and
// publishes it. The transaction records the delta applied after the SQL clamp.
func applySecStatusChange(ctx context.Context, sqldb *sql.DB, charEntity *destiny.Entity,
	charID int64, delta float64, eventTypeID, subjectID, systemID, shipTypeID int64) {

	oldRating, newRating, err := evedb.UpdateSecurityRating(ctx, sqldb, charID, delta)
	if err != nil {
		log.Printf("[secstatus] UpdateSecurityRating charID=%d delta=%+.3f: %v", charID, delta, err)
		return
	}
	applied := newRating - oldRating
	if err := evedb.InsertStandingTransaction(ctx, sqldb, eventTypeID, evedb.OwnerCONCORD, charID,
		applied, subjectID, systemID, shipTypeID, ""); err != nil {
		log.Printf("[secstatus] InsertStandingTransaction charID=%d: %v", charID, err)
	}
	publishSecStatus(charEntity, charID, newRating)
}

// applySecStatusPenalty takes pct percent of the pilot's distance down to -10
// (seclaw.LossFromGap), logs the transaction, publishes it and tells the pilot
// with the client's own notice (key and dict; key "" sends none). Returns the
// delta applied. The arithmetic runs inside the UPDATE.
func applySecStatusPenalty(ctx context.Context, sqldb *sql.DB, charEntity *destiny.Entity,
	charID int64, pct float64, eventTypeID, subjectID, systemID, shipTypeID int64,
	noticeKey string, notice map[string]marshal.PyRep) float64 {

	if pct <= 0 || charID <= 0 {
		return 0
	}
	oldRating, newRating, err := evedb.PenalizeSecurityRatingFraction(ctx, sqldb, charID, pct/100)
	if err != nil {
		log.Printf("[secstatus] PenalizeSecurityRatingFraction charID=%d pct=%.3f: %v", charID, pct, err)
		return 0
	}
	applied := newRating - oldRating
	if applied == 0 {
		// Already at the floor.
		return 0
	}
	if err := evedb.InsertStandingTransaction(ctx, sqldb, eventTypeID, evedb.OwnerCONCORD, charID,
		applied, subjectID, systemID, shipTypeID, ""); err != nil {
		log.Printf("[secstatus] InsertStandingTransaction charID=%d: %v", charID, err)
	}
	publishSecStatus(charEntity, charID, newRating)
	if noticeKey != "" {
		if c, acct := charConnLookup(charID); c != nil {
			d := map[string]marshal.PyRep{"penalty": marshal.PyReal{Value: applied}}
			for k, v := range notice {
				d[k] = v
			}
			pushRemoteMessage(c, acct, noticeKey, d)
		}
	}
	return applied
}

// applySecStatusGain awards a fraction of the pilot's remaining gap to +10, the
// shape a rat kill pays (seclaw.RatGain). The arithmetic runs inside the UPDATE
// so two rats dying on one tick cannot both read the same current value. A
// negative fraction is applied flat.
func applySecStatusGain(ctx context.Context, sqldb *sql.DB, charEntity *destiny.Entity,
	charID int64, fraction float64, eventTypeID, subjectID, systemID, shipTypeID int64) {

	if fraction == 0 {
		return
	}
	oldRating, newRating, err := evedb.AwardSecurityRatingFraction(ctx, sqldb, charID, fraction)
	if err != nil {
		log.Printf("[secstatus] AwardSecurityRatingFraction charID=%d frac=%+.5f: %v", charID, fraction, err)
		return
	}
	applied := newRating - oldRating
	if applied == 0 {
		// Already at the ceiling: nothing moved, so nothing is logged.
		return
	}
	if err := evedb.InsertStandingTransaction(ctx, sqldb, eventTypeID, evedb.OwnerCONCORD, charID,
		applied, subjectID, systemID, shipTypeID, ""); err != nil {
		log.Printf("[secstatus] InsertStandingTransaction charID=%d: %v", charID, err)
	}
	publishSecStatus(charEntity, charID, newRating)
}

// applyKillPenalty is the entry point for every security-status penalty a kill
// produces: the core death path, an embodied rolebot's guns and the module's
// abstract tier. Returns the delta applied and, when none was, the exemption.
// offender may be nil; the record still moves.
func applyKillPenalty(ctx context.Context, sqldb *sql.DB, offender *destiny.Entity,
	f seclaw.KillFacts) (float64, seclaw.Exemption) {

	crime, why := seclaw.ShotIsCrime(f, seclawDeps(sqldb))
	if !crime {
		if why == seclaw.ExemptKillRight {
			spendKillRightFor(ctx, sqldb, f)
		}
		return 0, why
	}
	// An unlawful kill also mints the victim's kill right (killrights.go): same
	// verdict, same site, every tier.
	grantKillRightFor(ctx, sqldb, f)

	pct := shipKillPenaltyPct()
	event := int64(evedb.EventStandingCombatShipKill)
	victim := marshal.PyString{Value: evedb.GetCharacterName(ctx, sqldb, f.VictimCharID)}
	key, notice := "SecurityStatusKillChange", map[string]marshal.PyRep{"item": victim}
	if f.Pod {
		event = evedb.EventStandingCombatPodKill
		pct = podKillPenaltyPct()
		key, notice = "SecurityStatusPodKillChange", map[string]marshal.PyRep{"targetName": victim}
		// A pod kill on a victim who shot at the killer or their corpmates ends a
		// fight and is discounted; the crimewatch table knows, the killmail does not.
		if victimEngaged(f.SystemID, f.VictimCharID, f.KillerCharID, f.KillerCorpID) {
			pct *= podKillMutualMul()
		}
	}
	delta := applySecStatusPenalty(ctx, sqldb, offender, f.KillerCharID, pct,
		event, f.VictimCharID, f.SystemID, f.KillerShipTypeID, key, notice)
	if delta == 0 {
		return 0, ""
	}

	kind := "ship"
	if f.Pod {
		kind = "pod"
	}
	log.Printf("[secstatus] %s kill: charID=%d killed %d in system %d sec=%.2f — %+.3f",
		kind, f.KillerCharID, f.VictimCharID, f.SystemID, f.Security, delta)
	return delta, ""
}

// isPoliceEntity reports whether a dead NPC was the law: CONCORD by its corp
// and faction, a sentry gun by being one of the system's own posts. Empire navy
// rats carry an empire faction corp too, so no faction heuristic is used.
func isPoliceEntity(ss *destiny.SolarSystem, dead *destiny.Entity) bool {
	if dead == nil || ss == nil {
		return false
	}
	if dead.Slim.CorpID == concordCorpDED || dead.Slim.WarFactionID == concordFactionID {
		return true
	}
	// An empire police squad ship (not the navy).
	if isPoliceSquadMember(dead.ID) {
		return true
	}
	// Sentry-gun case. This runs on every NPC death, so the hull type is checked
	// before taking the group lock; a sentry is always one of the four navy guns.
	switch dead.Slim.TypeID {
	case sentryTypeCaldari, sentryTypeMinmatar, sentryTypeAmarr, sentryTypeGallente:
	default:
		return false
	}
	g, ok := sentryGroups.Load(ss.ID)
	if !ok {
		return false
	}
	grp := g
	grp.mu.Lock()
	defer grp.mu.Unlock()
	for _, post := range grp.posts {
		if post.entityID != 0 && post.entityID == dead.ID {
			return true
		}
	}
	return false
}

// awardOrPenaliseNPCKill is the one place an NPC's death touches a pilot's
// record: a pirate pays its entitySecurityStatusKillBonus, CONCORD and sentry
// guns cost the police penalty. Not gated on bounty: police hulls carry none.
func awardOrPenaliseNPCKill(ctx context.Context, sqldb *sql.DB, killer *destiny.Entity,
	killerCharID int64, dead *destiny.Entity, ss *destiny.SolarSystem, bounty float64) {

	if killer == nil || dead == nil || ss == nil || killerCharID <= 0 {
		return
	}

	if isPoliceEntity(ss, dead) {
		delta := applySecStatusPenalty(ctx, sqldb, killer, killerCharID, policeKillPenaltyPct(),
			evedb.EventStandingCombatShipKill, dead.Slim.OwnerID, ss.ID, killer.Slim.TypeID,
			"SecurityStatusPoliceKillChange", nil)
		if delta == 0 {
			return
		}
		log.Printf("[secstatus] police kill: charID=%d destroyed %d (type %d) in system %d — %+.3f",
			killerCharID, dead.ID, dead.Slim.TypeID, ss.ID, delta)
		return
	}

	// The award subject is the rat's faction where it has one, so the
	// security tab names who the kill was against.
	subject := dead.Slim.WarFactionID
	if subject == 0 {
		subject = dead.Slim.OwnerID
	}
	fraction := npcSecFraction(ctx, sqldb, dead.Slim.TypeID, bounty)
	if fraction == 0 {
		return
	}
	applySecStatusGain(ctx, sqldb, killer, killerCharID, fraction,
		evedb.EventStandingPirateKillSecurityStatus, subject, ss.ID, dead.Slim.TypeID)
}

// npcSecFraction is what killing one NPC of this type is worth, as the fraction
// of the killer's remaining gap AwardSecurityRatingFraction consumes. An
// explicit 0 attribute pays nothing; the fallback fires only on an absent
// attribute, and only for a hull with a bounty (scenery carries neither).
func npcSecFraction(ctx context.Context, sqldb *sql.DB, typeID int64, bounty float64) float64 {
	bonus, present := evedb.GetNPCSecurityBonus(ctx, sqldb, typeID)
	if !present {
		if bounty <= 0 {
			return 0
		}
		bonus = ratGainFallback()
	}
	// The gap-scaling is left to AwardSecurityRatingFraction's own UPDATE, so
	// what travels is the fraction, not the absolute gain.
	return seclaw.RatFraction(bonus, ratGainMultiplier())
}

// policeAggroCharged dedups the police-aggression penalty to once per pilot per
// policeAggroWindow (unix second of the last charge). A per-shot charge would
// floor a clean pilot in a second. Pruned by sweepPoliceAggroCharges.
var policeAggroCharged syncx.Map[int64, int64] // charID int64 -> unix seconds

// policeAggroWindow is how long one engagement with the law lasts for charging
// purposes: the criminal-flag duration.
const policeAggroWindow = aggressionDuration

// chargePoliceAggression charges a pilot once for opening fire on the law.
func chargePoliceAggression(ctx context.Context, sqldb *sql.DB, ss *destiny.SolarSystem, charID int64) {
	if sqldb == nil || charID <= 0 || policeAggroPenaltyPct() <= 0 {
		return
	}
	now := time.Now().Unix()
	if prev, ok := policeAggroCharged.Load(charID); ok {
		if now-prev < int64(policeAggroWindow/time.Second) {
			return
		}
	}
	policeAggroCharged.Store(charID, now)

	var offender *destiny.Entity
	var sysID int64
	if ss != nil {
		offender = findPlayerEntityByChar(ss, charID)
		sysID = ss.ID
	}
	delta := applySecStatusPenalty(ctx, sqldb, offender, charID, policeAggroPenaltyPct(),
		evedb.EventStandingCombatAggression, evedb.OwnerCONCORD, sysID, 0,
		"SecurityStatusPoliceAggressionChange", nil)
	// Flagged to the gates' owner (every stargate slim carries the CONCORD corp), so
	// the client's spaceMgr.CanWarp asks WarpToStargateWhileFlagged for 15 minutes.
	if ss != nil {
		aggroTableFor(ss.ID).set(charID, evedb.OwnerCONCORD, now)
		broadcastAggression(ss)
	}
	log.Printf("[secstatus] police engagement: charID=%d in system %d — %+.3f", charID, sysID, delta)
}

// sweepPoliceAggroCharges drops expired engagement records. Called from the
// aggression sweep, which already runs on the right cadence for a map keyed by
// the same window it prunes on.
func sweepPoliceAggroCharges(now int64) {
	cutoff := now - int64(policeAggroWindow/time.Second)
	policeAggroCharged.Range(func(k int64, ts int64) bool {
		if ts < cutoff {
			policeAggroCharged.Delete(k)
		}
		return true
	})
}

// chargePoliceAggressors charges every pilot on the threat table of any of
// these police hulls. The threat table, not the hulls' hit tables: an NPC victim
// goes through npcMgr.NotifyDamage and never accumulates Entity.RecordKillHit
// entries. A threat entry survives the shooter warping off.
func chargePoliceAggressors(sqldb *sql.DB, ss *destiny.SolarSystem,
	npcMgr *npc.NPCManager, fleet []uint32) {

	if sqldb == nil || ss == nil || npcMgr == nil || len(fleet) == 0 || policeAggroPenaltyPct() <= 0 {
		return
	}
	now := time.Now().Unix()
	seen := map[int64]bool{}
	var charge []int64
	for _, id := range fleet {
		for _, attackerID := range npcMgr.ThreatAttackers(id) {
			atk := ss.GetEntity(attackerID)
			if atk == nil || !atk.IsPlayer {
				continue
			}
			charID := atk.Slim.CharID
			if charID <= 0 || seen[charID] {
				continue
			}
			if prev, ok := policeAggroCharged.Load(charID); ok &&
				now-prev < int64(policeAggroWindow/time.Second) {
				continue
			}
			seen[charID] = true
			charge = append(charge, charID)
		}
	}
	if len(charge) == 0 {
		return
	}
	go func() {
		defer safego.Recover("chargePoliceAggressors")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, charID := range charge {
			chargePoliceAggression(ctx, sqldb, ss, charID)
		}
	}()
}

// startSecStatusDecay forgives decayPerHour of every negative record every
// hour (CCP's standingBadSecurityMilliPointsForgotten) in one statement for the
// whole shard, with no standing transaction per pilot. Only connected or
// projected pilots get their in-memory copy refreshed; others read it at login.
func startSecStatusDecay(tickMgr *tick.Manager, sqldb *sql.DB) {
	if tickMgr == nil || sqldb == nil {
		return
	}
	tickMgr.RegisterPeriodic("sec-status-decay", 3600, func(uint64) bool {
		if !decayEnabled() {
			return true
		}
		amount := decayPerHour()
		if amount <= 0 {
			return true
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		n, err := evedb.DecayNegativeSecurityRatings(ctx, sqldb, amount)
		if err != nil {
			log.Printf("[secstatus] decay: %v", err)
			return true
		}
		if n == 0 {
			return true
		}
		log.Printf("[secstatus] decay: %d records forgiven %+.3f", n, amount)
		refreshLiveSecStatus(ctx, sqldb)
		return true
	})
}

// refreshLiveSecStatus re-reads and republishes security status for pilots who
// are in space, after a bulk write went round the per-character path.
func refreshLiveSecStatus(ctx context.Context, sqldb *sql.DB) {
	if globalSysMgr == nil {
		return
	}
	for _, ss := range globalSysMgr.All() {
		for _, p := range ss.GetAllPlayersInSystem() {
			charID := p.Slim.CharID
			if charID <= 0 || p.Slim.SecurityStatus >= 0 {
				// Only negatives moved.
				continue
			}
			rating, err := evedb.GetCharSecurityRating(ctx, sqldb, charID)
			if err != nil {
				continue
			}
			publishSecStatus(p, charID, rating)
		}
	}
}

// killerFromHits picks the pilot the law charges for a death: the final blow,
// else the most damage. NPC hits and the victim's own entry are skipped, so a
// pilot killed by rats, the police or self-destruct charges nobody.
func killerFromHits(hits []destiny.KillHit, victimCharID int64) (destiny.KillHit, bool) {
	var best destiny.KillHit
	found := false
	for _, h := range hits {
		if h.IsNPC || h.CharID <= 0 || h.CharID == victimCharID {
			continue
		}
		if h.FinalBlow {
			return h, true
		}
		if !found || h.Damage > best.Damage {
			best, found = h, true
		}
	}
	return best, found
}

// findPlayerEntityByChar locates a pilot's live entity in a system by walking
// the player list. KillHitsSnapshot does not carry the attacker's entity ID, and
// the entity is needed only to update the killer's slim; the row moves anyway.
func findPlayerEntityByChar(ss *destiny.SolarSystem, charID int64) *destiny.Entity {
	if ss == nil || charID <= 0 {
		return nil
	}
	for _, p := range ss.GetAllPlayersInSystem() {
		if p.Slim.CharID == charID {
			return p
		}
	}
	return nil
}

// chargeForPlayerDeathFromReport prices a player death off the kill-report
// snapshot. Both death handlers must call it before clearPlayerCombatState
// (the crimewatch table says whether the victim shot back) and before their
// explosion sleep. The offender's entity is resolved inside the goroutine.
func chargeForPlayerDeathFromReport(sqldb *sql.DB, ds *destinyConn, km killReport, pod bool) {
	// Repeated from the callers: a nil deref on a death path takes the victim's
	// connection goroutine down.
	if sqldb == nil || ds == nil || ds.solarSystem == nil {
		return
	}
	hit, ok := killerFromHits(km.Hits, km.VictimCharID)
	if !ok {
		// Killed by rats, by the police, or by their own self-destruct.
		// Nobody to charge.
		return
	}
	f := seclaw.KillFacts{
		KillerCharID:     hit.CharID,
		KillerCorpID:     hit.CorpID,
		KillerAllianceID: hit.AllianceID,
		KillerShipTypeID: hit.ShipTypeID,
		VictimCharID:     km.VictimCharID,
		VictimCorpID:     km.VictimCorpID,
		VictimAllianceID: km.VictimAllianceID,
		SystemID:         km.SysID,
		Security:         ds.solarSystem.Security,
		Pod:              pod,
	}
	ss := ds.solarSystem
	go func() {
		defer safego.Recover("chargeForPlayerDeathFromReport")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		applyKillPenalty(ctx, sqldb, findPlayerEntityByChar(ss, f.KillerCharID), f)
	}()
}
