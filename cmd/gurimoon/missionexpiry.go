package main

import (
	"context"
	"database/sql"
	"log"
	"strings"
	"sync"
	"time"

	evedb "gurimoon/db"
	"gurimoon/esp"
	evenet "gurimoon/net"
	"gurimoon/npc"
	"gurimoon/tick"
)

// ── Mission expiry ───────────────────────────────────────────────────────────
//
// An accepted mission has its template's expiryDays (default 7) to be handed in. AcceptMission
// stamps characterMissions.expiresAt and this sweep fails what is past it like a quit: the standing
// hit, the site torn down, mission items taken back, and a letter from the agent (failText).

// missionExpirySweepTicks: expiry is measured in days, so a minute is plenty.
const missionExpirySweepTicks = 60

// missionExpiryDefaultLetter is the failure letter for a template with no
// failText. Mission placeholders apply.
const missionExpiryDefaultLetter = "{agentName} here.\n\n" +
	"You accepted this assignment and never saw it through, so I have given it to someone else. " +
	"That will not be forgotten."

func startMissionExpirySweep(tickMgr *tick.Manager) {
	var restamp sync.Once
	tickMgr.RegisterPeriodic("mission-expiry-sweep", missionExpirySweepTicks, func(uint64) bool {
		if globalDB == nil {
			return true
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		// Once per run: hold every accepted mission to its template's current
		// expiry, including rows accepted before the template carried one.
		restamp.Do(func() {
			if n, err := evedb.RestampMissionExpiry(ctx, globalDB, 0); err != nil {
				log.Printf("[mission-expiry] RestampMissionExpiry: %v", err)
			} else if n > 0 {
				log.Printf("[mission-expiry] re-dated %d accepted mission(s) to their template expiry", n)
			}
		})
		due, err := evedb.DueExpiredMissions(ctx, globalDB)
		if err != nil {
			log.Printf("[mission-expiry] DueExpiredMissions: %v", err)
			return true
		}
		for _, m := range due {
			expireMission(ctx, globalDB, m)
		}
		return true
	})
}

// expireMission fails one accepted mission and tears down everything it held.
func expireMission(ctx context.Context, sqldb *sql.DB, m *evedb.CharacterMission) {
	ok, err := evedb.ExpireMission(ctx, sqldb, m.MissionID)
	if err != nil {
		log.Printf("[mission-expiry] mission %d: %v", m.MissionID, err)
		return
	}
	if !ok {
		return // handed in or quit since the query
	}
	m.State = "failed"
	tmpl, _ := evedb.GetTemplateByID(ctx, sqldb, m.TemplateID)
	agent, _ := evedb.GetAgentInfo(ctx, sqldb, m.AgentID)
	conn, accountID := charConnLookup(m.CharID)

	// The site: rats out of space, the pocket off the owner's grid, the
	// persisted instance and its kill/wave state out of the database.
	despawned := 0
	if globalNPCMgr != nil {
		despawned = globalNPCMgr.DespawnMission(m.MissionID, "mission expired")
	}
	pokeMissionEnded(m.CharID, m.MissionID)
	cleanupMissionDungeon(ctx, sqldb, m.MissionID, "mission-expiry")

	missionName := ""
	if tmpl != nil {
		missionName = tmpl.MissionName
	}
	loss := 0.0
	if tmpl != nil {
		loss = tmpl.StandingLoss
	}
	applyAgentStanding(ctx, conn, accountID, sqldb, m.CharID, m.AgentID,
		m.AgentID, loss, evedb.EventStandingAgentMissionFailed, missionName)
	if tmpl != nil && tmpl.Storyline {
		if err := evedb.ClearStorylinePending(ctx, sqldb, m.CharID); err != nil {
			log.Printf("[mission-expiry] mission %d: ClearStorylinePending: %v", m.MissionID, err)
		}
	}
	if tmpl != nil {
		agentStationID := int64(0)
		if agent != nil {
			agentStationID = agent.StationID
		}
		playerStationID, _ := locatorTargetLocation(ctx, sqldb, m.CharID)
		removeMissionItems(ctx, conn, accountID, tmpl, m, m.CharID, agentStationID, playerStationID, sqldb)
	}

	// The letter, from the agent.
	body := missionExpiryDefaultLetter
	if tmpl != nil && strings.TrimSpace(tmpl.FailText) != "" {
		body = tmpl.FailText
	}
	mtp := evedb.BuildMissionTextParams(ctx, sqldb, m.CharID, m.AgentID, agent, tmpl, m)
	body = strings.ReplaceAll(evedb.SubstituteMissionText(body, mtp), "\n", "<br>")
	subject := "Mission failed"
	if missionName != "" {
		subject += ": " + missionName
	}
	created := int64(timeToFileTime(time.Now()))
	msgID, err := evedb.SendEveMail(ctx, sqldb, m.CharID, m.AgentID, subject, body, created)
	if err != nil {
		log.Printf("[mission-expiry] mission %d: mail: %v", m.MissionID, err)
	} else {
		pushMailNotification(m.CharID, msgID, m.AgentID, subject, created)
	}
	notifyAgentMissionChange(conn, accountID, "failed", m.AgentID)

	log.Printf("[mission-expiry] mission %d (%q) charID=%d agentID=%d expired: standing %+.2f, %d NPC(s) despawned",
		m.MissionID, missionName, m.CharID, m.AgentID, loss, despawned)
}

// pokeMissionEnded tells the owner's connection goroutine that a mission is
// over, so a pilot sitting in its pocket loses the scenery and gates as well
// as the rats. Only that goroutine may touch ds.dunObjects.
func pokeMissionEnded(charID, missionID int64) {
	if globalSessReg == nil || globalSysMgr == nil {
		return
	}
	s := globalSessReg.FindByCharID(charID)
	if s == nil || s.SolarSystemID == 0 {
		return
	}
	ss := globalSysMgr.Get(s.SolarSystemID)
	if ss == nil {
		return
	}
	e := ss.GetPlayerByCharID(charID)
	if e == nil {
		return
	}
	ds, ok := e.PlayerState.(*destinyConn)
	if !ok || ds == nil || ds.missionEnded == nil {
		return
	}
	select {
	case ds.missionEnded <- missionID:
	default:
	}
}

// endMissionPocket runs on the connection goroutine: if the loaded room
// belongs to the ended mission, clear it like a DungeonReset.
func endMissionPocket(c *evenet.Conn, addr string, accountID int64, ds *destinyConn,
	sessReg *esp.SessionRegistry, npcMgr *npc.NPCManager, sqldb *sql.DB, missionID int64) {
	if missionID <= 0 || ds.dunMissionID != missionID {
		return
	}
	n := clearDungeonBalls(c, addr, accountID, ds, npcMgr, sqldb)
	ds.dunRoomID = 0
	ds.dunMissionID = 0
	ds.dunNoWarp = false
	if sessReg != nil {
		sessReg.Update(addr, func(s *esp.Session) {
			s.DunRoomID = 0
		})
	}
	log.Printf("[%s] mission %d ended: pocket cleared (%d balls)", addr, missionID, n)
}
