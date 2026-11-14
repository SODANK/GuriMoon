// Dungeon logic triggers: event to action rules authored in the ESP Dungeon Logic tool
// (dunTriggers), evaluated per connection against its loaded room.
// Events: roomEnter, npcKilled, npcDamaged, roomCleared, gateActivated, itemPlaced.
// Actions: transmission, localChat, popup, spawnWave, music, completeDungeon.
// See docs/cmd-misc-notes.md#dungeon-triggers.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/npc"
	"gurimoon/server"
	"gurimoon/server/lsc"
)

// loadRoomTriggers replaces the connection's active trigger set with the
// given room's rows and resets once-only bookkeeping. Called from
// spawnDungeonRoom, so a room reload re-arms every trigger. objs is the room's
// full object list, read for its held wave groups (the auto-wave chain).
func loadRoomTriggers(ctx context.Context, sqldb *sql.DB, ds *destinyConn, roomID int64, objs []evedb.ObjectRow) {
	trigs, err := evedb.GetRoomTriggers(ctx, sqldb, roomID)
	if err != nil {
		log.Printf("dungeon triggers: load roomID=%d: %v", roomID, err)
		trigs = nil
	}
	for i := range trigs {
		normalizeTriggerIcon(ctx, sqldb, &trigs[i])
	}
	ds.dunTrigMu.Lock()
	ds.dunTriggers = trigs
	ds.dunTrigFired = make(map[int64]bool)
	ds.dunTrigRoomID = roomID
	ds.dunWaveGroups = evedb.RoomWaveGroups(objs)
	ds.dunWavesOut = make(map[int64]bool)
	ds.dunTrigMu.Unlock()
}

// normalizeTriggerIcon rewrites an authored transmission icon to a numeric graphicID with a
// non-empty icon, which is what transmissionMgr.py looks up. A typeID is translated by
// ResolveTransmissionIcon, an unresolvable value is cleared, and "netres:" URLs pass through.
func normalizeTriggerIcon(ctx context.Context, sqldb *sql.DB, t *evedb.TriggerRow) {
	icon := strings.TrimSpace(t.Icon.String)
	if icon == "" || strings.HasPrefix(icon, "netres:") {
		return
	}
	// The client only reads the first space-separated token.
	if i := strings.IndexByte(icon, ' '); i >= 0 {
		icon = icon[:i]
	}
	id, err := strconv.ParseInt(icon, 10, 64)
	if err != nil {
		log.Printf("dungeon triggers: trigger %d icon %q is not numeric or netres: — dropped", t.TriggerID, t.Icon.String)
		t.Icon = sql.NullString{}
		return
	}
	gid, ok := evedb.ResolveTransmissionIcon(ctx, sqldb, id)
	if !ok {
		log.Printf("dungeon triggers: trigger %d icon %d resolves to no usable eveGraphics row — dropped", t.TriggerID, id)
		t.Icon = sql.NullString{}
		return
	}
	if gid != id {
		log.Printf("dungeon triggers: trigger %d icon %d is a typeID — using graphicID %d", t.TriggerID, id, gid)
	}
	t.Icon = sql.NullString{String: strconv.FormatInt(gid, 10), Valid: true}
}

// fireDungeonTriggers runs the loaded room's triggers that match event. targetObjectID scopes
// npcKilled/npcDamaged/gateActivated/itemPlaced to one authored object; a trigger with none matches
// any. Safe from NPC tick goroutines. Reports whether a spawnWave trigger was taken, so the
// auto-wave chain stands back for it.
func fireDungeonTriggers(c *evenet.Conn, addr string, accountID int64,
	ds *destinyConn, event string, targetObjectID int64) bool {
	return fireDungeonTriggersIf(c, addr, accountID, ds, event, targetObjectID, nil)
}

// fireDungeonTriggersIf is fireDungeonTriggers with one more test a trigger must pass (nil for
// none). A trigger that fails it stays armed.
func fireDungeonTriggersIf(c *evenet.Conn, addr string, accountID int64,
	ds *destinyConn, event string, targetObjectID int64, match func(evedb.TriggerRow) bool) bool {

	ds.dunTrigMu.Lock()
	var due []evedb.TriggerRow
	waveQueued := false
	for _, t := range ds.dunTriggers {
		if t.Event != event {
			continue
		}
		if t.TargetObjectID.Valid && t.TargetObjectID.Int64 != 0 &&
			t.TargetObjectID.Int64 != targetObjectID {
			continue
		}
		if match != nil && !match(t) {
			continue
		}
		if t.OnceOnly && ds.dunTrigFired[t.TriggerID] {
			continue
		}
		// Only the first unfired spawnWave trigger runs per pass; later ones stay armed so chained
		// onceOnly roomCleared triggers release one wave per clear.
		if t.Action == "spawnWave" {
			if waveQueued {
				continue
			}
			waveQueued = true
		}
		if t.OnceOnly {
			ds.dunTrigFired[t.TriggerID] = true
		}
		due = append(due, t)
	}
	ds.dunTrigMu.Unlock()

	for _, t := range due {
		t := t
		if t.Action == "spawnWave" && t.SpawnWave.Valid && ds.dunOpenSiteID > 0 {
			openSiteWaveQueued(ds.dunOpenSiteID, t.RoomID, t.SpawnWave.Int64, time.Duration(t.DelayMs)*time.Millisecond)
		}
		run := func() {
			if err := runDungeonTriggerAction(c, accountID, ds, t, targetObjectID); err != nil {
				log.Printf("[%s] dungeon trigger %d (%s→%s): %v", addr, t.TriggerID, t.Event, t.Action, err)
			} else {
				log.Printf("[%s] dungeon trigger %d fired: %s → %s", addr, t.TriggerID, t.Event, t.Action)
			}
		}
		if t.DelayMs > 0 {
			time.AfterFunc(time.Duration(t.DelayMs)*time.Millisecond, run)
		} else {
			run()
		}
	}
	return waveQueued
}

// runDungeonTriggerAction executes one trigger's action. srcObjectID is the dungeon object that
// raised the event (0 for roomEnter/roomCleared); localChat speaks as its live NPC when it has one.
func runDungeonTriggerAction(c *evenet.Conn, accountID int64, ds *destinyConn, t evedb.TriggerRow, srcObjectID int64) error {
	body := t.Body.String
	switch t.Action {
	case "spawnWave":
		if !t.SpawnWave.Valid || t.SpawnWave.Int64 <= 0 {
			return nil
		}
		// Queued to the connection goroutine, which owns ds.dunObjects; this may run on an NPC tick
		// goroutine. A full channel drops the wave.
		select {
		case ds.dunWaveSpawn <- dunWaveReq{RoomID: t.RoomID, Wave: t.SpawnWave.Int64}:
		default:
			log.Printf("dungeon trigger %d: dunWaveSpawn channel full, wave %d dropped", t.TriggerID, t.SpawnWave.Int64)
		}
		return nil

	case "localChat":
		body = strings.TrimSpace(body)
		if body == "" || body == "/" {
			return nil
		}
		senderID, senderName, senderTypeID := dungeonChatSender(ds, t, srcObjectID)
		// The chat window renders inline <color=0xAARRGGBB> tags (lscchannel.py FormatTxt). Its "/emote "
		// and "/" checks are startswith tests, so the tag goes after those prefixes.
		tag := ""
		if col := strings.TrimPrefix(strings.TrimSpace(t.ChatColor.String), "#"); col != "" {
			tag = "<color=0xff" + col + ">"
		}
		if strings.HasPrefix(body, "/emote ") {
			body = "/emote " + tag + strings.TrimPrefix(body, "/emote ")
		} else {
			// Always a narrator line: FormatTxt renders a leading "/" without the "sender >" prefix. The "/"
			// is added here, and the sender still rides the wire so __Output can resolve the owner.
			body = "/" + tag + strings.TrimPrefix(body, "/")
		}
		return lsc.SendNPCChat(c, accountID, ds.sysID, senderID, senderName, senderTypeID, body)

	case "popup":
		args := marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyString{Value: "CustomInfo"},
			marshal.PyDict{Items: []marshal.DictEntry{
				{Key: marshal.PyString{Value: "info"}, Value: marshal.PyString{Value: body}},
			}},
		}}
		return server.SendNotification(c, accountID, "OnRemoteMessage", "clientID", args)

	case "music":
		args, err := dungeonMusicArgs(strings.TrimSpace(body))
		if err != nil {
			return err
		}
		return server.SendNotification(c, accountID, "OnRemoteExecute", "clientID", args)

	case "completeDungeon":
		return completeDungeonSite(c, accountID, ds)

	default: // "transmission"
		header := t.Header.String
		if header == "" {
			header = "AUTOMATED MESSAGE"
		}
		return sendDungeonTransmission(c, accountID, header, t.Icon.String, body, t.DeferUntilLanded)
	}
}

// dungeonMusicArgs is OnRemoteExecute's argument tuple for a music trigger: the call a gate makes
// for dunMusicUrl (accelerationGate.py), skipped for a pilot whose music is stopped or paused,
// since PlayFile would turn it back on. Python 2.3 source, so its lines must end in LF.
func dungeonMusicArgs(url string) (marshal.PyTuple, error) {
	if !evedb.ValidMusicURL(url) {
		return marshal.PyTuple{}, fmt.Errorf("track %q is not a res:/ sound path", url)
	}
	code := "if getattr(prefs, 'music', 'play') == 'play':\n" +
		"    eve.LocalSvc('audio').PlayFile('" + url + "', 2500)\n"
	return marshal.PyTuple{Items: []marshal.PyRep{marshal.PyString{Value: code}}}, nil
}

// dungeonChatSender picks the (ownerID, name, typeID) a localChat line speaks under. An authored
// name takes the synthetic DunChatOwnerBase ID so the client shows it; an unnamed trigger speaks
// as the live NPC that raised the event. See docs/cmd-misc-notes.md#dungeon-triggers.
func dungeonChatSender(ds *destinyConn, t evedb.TriggerRow, srcObjectID int64) (int64, string, int64) {
	name := strings.TrimSpace(t.SenderName.String)
	if name == "" {
		name = strings.TrimSpace(t.Header.String)
	}
	if name != "" {
		return evedb.DunChatOwnerBase + t.TriggerID, name, 1374
	}

	obj := srcObjectID
	if obj == 0 && t.TargetObjectID.Valid {
		obj = t.TargetObjectID.Int64
	}
	if obj != 0 && ds.solarSystem != nil {
		for eid, oid := range ds.dunObjSnapshot() {
			if oid != obj {
				continue
			}
			if e := ds.solarSystem.GetEntity(eid); e != nil && e.CharName != "" {
				return int64(eid), e.CharName, e.Slim.TypeID
			}
			break
		}
	}
	return evedb.DunChatOwnerBase + t.TriggerID, "Unknown Transmission", 1374
}

// sendDungeonTransmission delivers an OnIncomingTransmission popup. The
// client reads exactly transmission.header / .icon / .text off a util.KeyVal
// (transmissionMgr.py); deferred windows carry the "[d]" header prefix and
// surface when the warp indication stops.
func sendDungeonTransmission(c *evenet.Conn, accountID int64, header, icon, text string, deferred bool) error {
	if deferred && !strings.HasPrefix(header, "[d]") {
		header = "[d]" + header
	}
	var iconRep marshal.PyRep = marshal.PyString{Value: ""}
	if icon != "" {
		iconRep = marshal.PyString{Value: icon}
	}
	kv := marshal.PyObject{
		Type: "util.KeyVal",
		Args: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "header"}, Value: marshal.PyString{Value: header}},
			{Key: marshal.PyString{Value: "icon"}, Value: iconRep},
			{Key: marshal.PyString{Value: "text"}, Value: marshal.PyString{Value: text}},
		}},
	}
	args := marshal.PyTuple{Items: []marshal.PyRep{
		kv,
		marshal.PyInt{Value: 0}, // isAgentMission, ignored by the client
	}}
	return server.SendNotification(c, accountID, "OnIncomingTransmission", "clientID", args)
}

// fireDungeonNPCDeathTriggers routes an NPC death into npcKilled, then roomCleared when it was
// the room's last rat, then the auto-wave chain if no trigger released a wave. Called from the
// death broadcast with the dying NPC still registered. A structure's death is never a clear.
func fireDungeonNPCDeathTriggers(ds *destinyConn, npcMgr *npc.NPCManager, entityID uint32, objectID int64) {
	if ds == nil || ds.entity == nil || ds.entity.Conn == nil {
		return
	}
	c := ds.entity.Conn
	accountID := int64(ds.entity.AccountID)
	addr := "npc-death"

	waveQueued := fireDungeonTriggers(c, addr, accountID, ds, "npcKilled", objectID)

	if npcMgr == nil || dunEntityIsStructure(ds, entityID) || !dunRoomQuiet(ds, npcMgr, entityID) {
		return
	}
	if fireDungeonTriggers(c, addr, accountID, ds, "roomCleared", 0) {
		waveQueued = true
	}
	if !waveQueued {
		autoReleaseDungeonWave(ds, npcMgr, entityID, addr)
	}
}

// dunEntityIsStructure reports whether a dungeon ball is a destructible
// structure (npc/structures.go) rather than a rat.
func dunEntityIsStructure(ds *destinyConn, entityID uint32) bool {
	ss := ds.solarSystem
	if ss == nil {
		return false
	}
	e := ss.GetEntity(entityID)
	return e != nil && npc.IsDestructibleStructure(e.Slim.GroupID)
}

// dunRoomQuiet reports whether the loaded room has no living rat left, ignoring exclude (a
// dying NPC the manager still holds). Structures do not count, as in the killAll site clear.
func dunRoomQuiet(ds *destinyConn, npcMgr *npc.NPCManager, exclude uint32) bool {
	if npcMgr == nil {
		return false
	}
	for eid := range ds.dunObjSnapshot() {
		if eid == exclude || !npcMgr.IsNPC(eid) || dunEntityIsStructure(ds, eid) {
			continue
		}
		return false
	}
	return true
}

// autoReleaseDungeonWave lets the room's next auto wave out (db/dunautowave.go) when the room is
// quiet. Safe from NPC tick goroutines: the spawn is queued to the connection goroutine. The wave
// is marked out at queue time so a second quiet moment cannot release it twice or skip ahead.
func autoReleaseDungeonWave(ds *destinyConn, npcMgr *npc.NPCManager, exclude uint32, addr string) {
	if ds == nil || !dunRoomQuiet(ds, npcMgr, exclude) {
		return
	}
	site := dunSiteStateFor(ds, false)
	ds.dunTrigMu.Lock()
	roomID := ds.dunTrigRoomID
	wave := evedb.NextAutoWave(ds.dunWaveGroups, ds.dunTriggers, func(w int64) bool {
		return ds.dunWavesOut[w] || site.WaveReleased(roomID, w)
	})
	if wave > 0 {
		ds.dunWavesOut[wave] = true
	}
	ds.dunTrigMu.Unlock()
	if wave == 0 || roomID == 0 {
		return
	}
	select {
	case ds.dunWaveSpawn <- dunWaveReq{RoomID: roomID, Wave: wave}:
		openSiteWaveQueued(ds.dunOpenSiteID, roomID, wave, 0)
		log.Printf("[%s] dungeon room %d quiet — auto wave %d released", addr, roomID, wave)
	default:
		ds.dunTrigMu.Lock()
		delete(ds.dunWavesOut, wave)
		ds.dunTrigMu.Unlock()
		log.Printf("[%s] dungeon room %d: dunWaveSpawn channel full, auto wave %d not released", addr, roomID, wave)
	}
}

// noteDungeonWaveOut records that a wave of the loaded room is out, whoever
// released it, so the auto chain reads it as released even where no site state
// keeps a record (an ESP test spawn).
func noteDungeonWaveOut(ds *destinyConn, roomID, wave int64) {
	ds.dunTrigMu.Lock()
	if ds.dunTrigRoomID == roomID && ds.dunWavesOut != nil {
		ds.dunWavesOut[wave] = true
	}
	ds.dunTrigMu.Unlock()
}
