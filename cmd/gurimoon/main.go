// GuriMoon is a full restoration server for EVE Online, built for the 2005-2006 Red Moon
// Rising client. It continues the initial work of EVEClassic, a Go rewrite of evemu.
//
// Usage:
//
//	go run ./cmd/gurimoon
//	go run ./cmd/gurimoon -config config.json -strings data/strings.txt
//
// Point an EVE client at the machine running this server.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"math"
	"math/rand"
	gonet "net"
	"os"
	"os/signal"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"gurimoon/bugtrack"
	evecache "gurimoon/cache"
	"gurimoon/config"
	"gurimoon/corp"
	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/dogma"
	"gurimoon/esp"
	"gurimoon/gang"
	"gurimoon/logx"
	"gurimoon/marshal"
	"gurimoon/modapi"
	_ "gurimoon/modules"
	evenet "gurimoon/net"
	"gurimoon/npc"
	"gurimoon/rates"
	"gurimoon/rpccov"
	"gurimoon/safego"
	"gurimoon/server"
	"gurimoon/server/account"
	"gurimoon/server/alert"
	"gurimoon/server/bookmark"
	"gurimoon/server/character"
	"gurimoon/server/charmgr"
	configsvc "gurimoon/server/config"
	"gurimoon/server/corpmgr"
	"gurimoon/server/dogmaim"
	"gurimoon/server/dungeon"
	"gurimoon/server/gangsta"
	"gurimoon/server/infocache"
	"gurimoon/server/insurance"
	"gurimoon/server/keeper"
	"gurimoon/server/lookup"
	"gurimoon/server/lsc"
	mapsvc "gurimoon/server/mapsvc"
	"gurimoon/server/market"
	"gurimoon/server/objectcaching"
	"gurimoon/server/petitioner"
	"gurimoon/server/posmgr"
	"gurimoon/server/ram"
	"gurimoon/server/repairsvc"
	"gurimoon/server/reprocessing"
	"gurimoon/server/slash"
	"gurimoon/server/standing2"
	"gurimoon/server/station"
	"gurimoon/server/stationsvc"
	"gurimoon/server/trademgr"
	"gurimoon/syncx"
	"gurimoon/tick"
	"gurimoon/trade"
)

// Version constants matching EVEVersion.h
const (
	machoVersion   uint64  = 139
	bootVersion    float64 = 2.10
	bootBuild      uint64  = 3913
	projectVersion         = "Blood"
	/*
		machoVersion   uint64  = 54
		bootVersion    float64 = 1.10
		bootBuild      uint64  = 1424
		projectVersion         = "Castor"
	*/

	// globalNodeID is the macho node ID used by this server instance.
	// Matches the value in PyServiceMgr (server/main.cpp).
	globalNodeID uint64 = 888444

	// win32Epoch is the offset (in 100-ns ticks) from the Win32 epoch
	// (1601-01-01) to the Unix epoch (1970-01-01).
	win32Epoch uint64 = 116444736000000000

	// posSnapLeadSecs is how far ahead, in seconds along current velocity, NPC SetBallPosition
	// resyncs are extrapolated. See docs/destiny-stamps.md#death-snap-lead.
	posSnapLeadSecs = 1.0

	// maxDeathSnapLeadSecs caps how far the NPC death point is carried forward for a killer whose
	// stamp stream runs ahead of the wall clock. See docs/destiny-stamps.md#death-snap-lead.
	maxDeathSnapLeadSecs = 3
)

// nextDunRigidID returns the next rigid dungeon ball entity ID (objects such
// as structures, gates and clouds). In ds.dunObjects an ID in that band is a
// client-only rigid ball (destiny.IsDunRigidBallID); anything else is an NPC
// entity.
func nextDunRigidID() uint32 {
	return destiny.NextDunRigidID()
}

// effectMap is the DB-driven effectName→EffectInfo map loaded at startup.
// Used by sendGodmaShipEffect and sendModuleFX to look up effectID, GUID,
// and isOffensive instead of hardcoded switches.
var effectMap map[string]evedb.EffectInfo

// resolveEffectName extracts an effect name from a PyString (name) or PyInt (effect ID).
// IDs resolve through effectMap; the RMR client sends numeric IDs for some module activations.
func resolveEffectName(arg marshal.PyRep) string {
	if s, ok := arg.(marshal.PyString); ok {
		return s.Value
	}
	if v, ok := arg.(marshal.PyInt); ok {
		effID := int64(v.Value)
		for name, info := range effectMap {
			if info.EffectID == effID {
				return name
			}
		}
	}
	return ""
}

// globalLSCReg is the process-wide local-chat registry, set once in main().
// Used by handlers (e.g. registerUndock for bot LSC presence) that would
// otherwise require threading the registry through every call site.
var globalLSCReg *lsc.Registry

// globalCacheStore is the process-wide bulk/cachable object store, set once in
// main(). Used by creation paths (character, corporation) to invalidate the
// owners bulk without threading the store through every dispatch chain.
var globalCacheStore *evecache.Store

// globalStationReg is the process-wide station guest registry, set once in
// main(). Used by dock/undock/teleport paths (e.g. handleBoundCall's Dock)
// that don't receive the registry as a parameter.
var globalStationReg *station.StationRegistry

// globalSpawnerMgr is the process-wide NPC spawner manager, set once in main().
// Used by registerUndock to load spawners when a system is first created.
var globalSpawnerMgr *npc.SpawnerManager

// globalCelestialCache holds preloaded system security/faction data and lazily
// caches celestials per region. Set once at boot.
var globalCelestialCache *evedb.CelestialCache

// globalNPCMgr is the process-wide NPC manager, set once in main().
// Used by registerUndock alongside globalSpawnerMgr for initial spawns.
var globalNPCMgr *npc.NPCManager

// globalGangReg is the process-wide in-memory gang registry, set once in main().
var globalGangReg *gang.Registry

// globalTradeReg is the process-wide player-trade session registry.
var globalTradeReg *trade.Registry

// globalSessReg mirrors main()'s sessReg. Written by location-transition paths (dock, gate
// jump, teleport, pod respawn) through updateSessionLocation, read by station- and
// system-filtered broadcasts.
var globalSessReg *esp.SessionRegistry

// updateSessionLocation refreshes the ESP session's location for addr.
func updateSessionLocation(addr string, stationID, solarSystemID int64) {
	if globalSessReg == nil {
		return
	}
	globalSessReg.Update(addr, func(s *esp.Session) {
		s.StationID = stationID
		s.SolarSystemID = solarSystemID
	})
}

// globalTickMgr / globalSysMgr let the starbase state machine schedule
// onlining completions and find live systems outside a connection context
// (set once in main before any connection is accepted).
var globalTickMgr *tick.Manager
var globalSysMgr *destiny.SystemManager

// globalDB is the shared DB handle for cleanup paths that run outside a
// connection context (system teardown, expiry sweeps). Set once in main.
var globalDB *sql.DB

// globalCharConns maps charID (int64) → charConn for online characters.
// Used by the gangsta service to send cross-player notifications.
var globalCharConns syncx.Map[int64, charConn]

// charConn pairs a live connection with the accountID of the account playing that character.
// A notification pushed to another player's socket must carry the recipient's accountID as the
// packet userID; the client applies it without validating.
type charConn struct {
	conn      *evenet.Conn
	accountID int64
}

// buildGangStateRep encodes the gang-state blob for GetGangState and OnGangStateChange: a
// util.KeyVal whose only client-read field is targetTags, a dict of itemID to overview tag
// (int 0-9 or letter string).
func buildGangStateRep(tags map[int64]any) marshal.PyRep {
	entries := make([]marshal.DictEntry, 0, len(tags))
	for id, tag := range tags {
		var rep marshal.PyRep
		switch v := tag.(type) {
		case string:
			rep = marshal.PyString{Value: v}
		case int64:
			rep = marshal.PyInt{Value: uint64(v)}
		default:
			continue
		}
		entries = append(entries, marshal.DictEntry{
			Key: marshal.PyInt{Value: uint64(id)}, Value: rep,
		})
	}
	return marshal.PyObject{
		Type: "util.KeyVal",
		Args: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "targetTags"}, Value: marshal.PyDict{Items: entries}},
		}},
	}
}

// charConnLookup resolves a character ID to their live connection and that
// character's own accountID. Returns (nil, 0) when offline.
// Shared by every service that pushes notifications to the other party.
func charConnLookup(charID int64) (*evenet.Conn, int64) {
	if v, ok := globalCharConns.Load(charID); ok {
		cc := v
		return cc.conn, cc.accountID
	}
	return nil, 0
}

// charIsOnline reports whether a character is present in the world, socket or not. Unlike
// charConnLookup, which answers whether a packet can be pushed, it also counts module-driven
// pilots, which the session registry carries and globalCharConns does not.
func charIsOnline(charID int64) bool {
	if charID == 0 {
		return false
	}
	if conn, _ := charConnLookup(charID); conn != nil {
		return true
	}
	return globalSessReg != nil && globalSessReg.IsPresent(charID)
}

// onlineCharIDs returns every character present in the world, by the rule of charIsOnline.
// globalCharConns is folded in rather than assumed to be a subset of the session registry.
func onlineCharIDs() []int64 {
	present := map[int64]struct{}{}
	if globalSessReg != nil {
		present = globalSessReg.PresentCharIDs()
	}
	globalCharConns.Range(func(id int64, _ charConn) bool {
		if id != 0 {
			present[id] = struct{}{}
		}
		return true
	})
	ids := make([]int64, 0, len(present))
	for id := range present {
		ids = append(ids, id)
	}
	return ids
}

// skillTimer tracks a pending skill-completion goroutine for one character.
type skillTimer struct {
	cancel context.CancelFunc
	conn   *evenet.Conn // connection that armed it
	end    uint64       // endOfTraining (Win32 FILETIME) this timer fires at
}

// skillTimers maps characterID → its active completion timer.  Guarded by
// skillTimersMu.  Ensures at most one pending completion goroutine per char.
var (
	skillTimers   = make(map[int64]*skillTimer)
	skillTimersMu sync.Mutex
)

// armSkillCompletionTimer schedules a goroutine that completes the character's due skill at
// endOfTraining and pushes OnSkillTrained. A pending timer with the same connection and end is
// kept; any other is cancelled and replaced.
func armSkillCompletionTimer(c *evenet.Conn, accountID, charID int64, endOfTraining uint64, sqldb *sql.DB) {
	if sqldb == nil || charID == 0 || endOfTraining == 0 {
		return
	}
	skillTimersMu.Lock()
	if t, ok := skillTimers[charID]; ok {
		if t.end == endOfTraining && t.conn == c {
			skillTimersMu.Unlock()
			return
		}
		t.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	skillTimers[charID] = &skillTimer{cancel: cancel, conn: c, end: endOfTraining}
	skillTimersMu.Unlock()

	go func() {
		// Runs outside any connection's dispatch chain, so a panic here would end the process.
		defer func() {
			if r := recover(); r != nil {
				stack := debug.Stack()
				log.Printf("PANIC in skill completion timer charID=%d: %v\n%s", charID, r, stack)
				bugtrack.ReportPanic("armSkillCompletionTimer", r, stack)
			}
		}()
		// Win32 FILETIME is 100-ns ticks; convert remaining ticks to a duration.
		var d time.Duration
		if now := server.Win32TimeNow(); endOfTraining > now {
			d = time.Duration(int64(endOfTraining-now) * 100)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(d):
		}
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		adv, err := evedb.AdvanceSkillQueue(cctx, sqldb, charID)
		ccancel()
		if err != nil {
			log.Printf("armSkillCompletionTimer charID=%d AdvanceSkillQueue: %v", charID, err)
		}
		skillTimersMu.Lock()
		if t, ok := skillTimers[charID]; ok && t.end == endOfTraining && t.conn == c {
			delete(skillTimers, charID)
		}
		skillTimersMu.Unlock()
		if adv != nil {
			// Notify after the map cleanup so the timer this may re-arm for a
			// queue-started skill is not clobbered.
			notifySkillQueueAdvance(c, accountID, charID, adv, sqldb)
		}
	}()
}

// notifySkillQueueAdvance pushes the client-facing fallout of an
// AdvanceSkillQueue call: OnSkillTrained (+item flag change) for every skill
// it completed, and OnSkillStartTraining (+flag change +completion timer)
// when it started the next queued skill.
func notifySkillQueueAdvance(c *evenet.Conn, accountID, charID int64, adv *evedb.SkillQueueAdvance, sqldb *sql.DB) {
	for _, itemID := range adv.Completed {
		notifyArgs := marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(itemID)},
		}}
		if err := server.SendNotification(c, accountID, "OnSkillTrained", "charid", notifyArgs); err != nil {
			log.Printf("notifySkillQueueAdvance charID=%d OnSkillTrained: %v", charID, err)
		} else {
			log.Printf("notifySkillQueueAdvance charID=%d skill %d trained → OnSkillTrained", charID, itemID)
		}
		sendSkillFlagChange(c, accountID, itemID, 61, 0, sqldb)
	}
	if adv.StartedItemID != 0 {
		notifyArgs := marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(adv.StartedItemID)},
			marshal.PyInt{Value: adv.StartedEnd},
		}}
		if err := server.SendNotification(c, accountID, "OnSkillStartTraining", "charid", notifyArgs); err != nil {
			log.Printf("notifySkillQueueAdvance charID=%d OnSkillStartTraining: %v", charID, err)
		}
		sendSkillFlagChange(c, accountID, adv.StartedItemID, 7, 0, sqldb)
		armSkillCompletionTimer(c, accountID, charID, adv.StartedEnd, sqldb)
	}
	if len(adv.Completed) > 0 {
		pushWeaponAttrsForChar(c, accountID, charID, sqldb)
	}
}

// skillSweepRunning guards against a slow skill-queue sweep overlapping itself.
var skillSweepRunning atomic.Bool

// killRetentionRunning does the same for the killboard retention sweep, whose
// first pass over an accumulated backlog can run for minutes.
var killRetentionRunning atomic.Bool

// walletRetentionRunning does the same for the wallet sweep, whose first pass can be a long run
// of batched deletes.
var walletRetentionRunning atomic.Bool

// historyRetentionRunning does the same for the closed-escrow and
// delivered-job sweep.
var historyRetentionRunning atomic.Bool

// asteroidFlushRunning does the same for the belt write-back.
var asteroidFlushRunning atomic.Bool

// sweepSkillQueues advances every character whose training is due or who is idle with a queue,
// online or not. For a logged-in pilot it also pushes the client-facing half and re-arms the
// completion timer. Completion times do not depend on when it runs.
// See docs/character-skills.md#skill-queue-advance.
func sweepSkillQueues(sqldb *sql.DB, sessReg *esp.SessionRegistry) {
	lctx, lcancel := context.WithTimeout(context.Background(), 15*time.Second)
	charIDs, err := evedb.DueSkillQueueCharacters(lctx, sqldb)
	lcancel()
	if err != nil {
		log.Printf("[skill-queue-sweep] DueSkillQueueCharacters: %v", err)
		return
	}
	// Per-character log lines only for pilots that were pushed to; the rest roll up into one line
	// per pass.
	advanced, completed := 0, 0
	for _, charID := range charIDs {
		// One context per character so a slow one cannot cancel the rest of a long first pass.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		adv, err := evedb.AdvanceSkillQueue(ctx, sqldb, charID)
		cancel()
		if err != nil {
			log.Printf("[skill-queue-sweep] charID=%d AdvanceSkillQueue: %v", charID, err)
			continue
		}
		if adv == nil || (len(adv.Completed) == 0 && adv.StartedItemID == 0) {
			continue // nothing was due after all, or the queue had nothing trainable left
		}
		advanced++
		completed += len(adv.Completed)
		if s := sessReg.FindByCharID(charID); s != nil && s.Conn != nil {
			notifySkillQueueAdvance(s.Conn, s.AccountID, charID, adv, sqldb)
			log.Printf("[skill-queue-sweep] charID=%d (online): %d skill(s) completed, now training item %d",
				charID, len(adv.Completed), adv.StartedItemID)
		}
	}
	if advanced > 0 {
		log.Printf("[skill-queue-sweep] advanced %d character(s), %d skill(s) completed", advanced, completed)
	}
}

// sendSkillFlagChange fetches the updated entity row for skillItemID and sends OnItemChange so
// the client updates the cached flag (flagSkill 7 <-> flagSkillInTraining 61). oldLocationID is
// the previous container (0 = unchanged); when the book moved, the old location must be in the
// change dict or the client's inventory view keeps it in the old container.
func sendSkillFlagChange(c *evenet.Conn, accountID int64, skillItemID int64, oldFlag int64, oldLocationID int64, sqldb *sql.DB) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	er, err := evedb.GetEntityRow(ctx, sqldb, skillItemID)
	if err != nil || er == nil {
		return
	}
	changeItems := []marshal.DictEntry{
		{Key: marshal.PyInt{Value: 4}, Value: marshal.PyInt{Value: uint64(oldFlag)}},
	}
	if oldLocationID != 0 && oldLocationID != er.LocationID {
		changeItems = append(changeItems, marshal.DictEntry{
			Key: marshal.PyInt{Value: 3}, Value: marshal.PyInt{Value: uint64(oldLocationID)}, // ixLocationID
		})
	}
	notifArgs := marshal.PyTuple{Items: []marshal.PyRep{
		evedb.BuildEntityRow(er),
		marshal.PyDict{Items: changeItems},
	}}
	_ = server.SendNotification(c, accountID, "OnItemChange", "charid", notifArgs)
}

// notifySkillInjected tells a live client about a skillbook the ESP just bought and injected.
// The new item's OnItemChange dict carries ixLocationID = None: godma keys off the entry's
// presence, then pulls the attributes with ItemGetInfo. The wallet push follows.
func notifySkillInjected(c *evenet.Conn, accountID, charID, skillItemID int64, newBalance float64, sqldb *sql.DB) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if er, err := evedb.GetEntityRow(ctx, sqldb, skillItemID); err == nil && er != nil {
		_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
			evedb.BuildEntityRow(er),
			marshal.PyDict{Items: []marshal.DictEntry{
				{Key: marshal.PyInt{Value: 3}, Value: marshal.PyNone{}}, // ixLocationID
			}},
		}})
	}
	_ = server.SendNotification(c, accountID, "OnAccountChange", "clientID", marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyString{Value: "cash"},
		marshal.PyInt{Value: uint64(charID)},
		marshal.PyReal{Value: newBalance},
	}})
}

// snapshotSkillItemState reads a skill item's location and flag before a
// training-start mutates them, so the follow-up OnItemChange can tell the
// client which container the skillbook left.
// trainingSkillItem returns the itemID of the skill charID is training now, or 0.
// Read before StartTrainingSkill so the switch can tell the client the old skill stopped.
func trainingSkillItem(sqldb *sql.DB, charID int64) int64 {
	if sqldb == nil || charID == 0 {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if st, err := evedb.GetSkillTraining(ctx, sqldb, charID); err == nil && st != nil {
		return st.SkillItemID
	}
	return 0
}

// notifySkillSwitchedOff tells the client that prevItemID stopped training because
// another skill was started. StartTrainingSkill banks it and resets its flag to 7 in
// the DB, but without this the client keeps it flagged in training next to the new
// one. Silent: the switch is the player's own action.
func notifySkillSwitchedOff(c *evenet.Conn, accountID, prevItemID, newItemID int64, sqldb *sql.DB) {
	if prevItemID == 0 || prevItemID == newItemID {
		return
	}
	_ = server.SendNotification(c, accountID, "OnSkillTrainingStopped", "charid", marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyInt{Value: uint64(prevItemID)},
		marshal.PyInt{Value: 1}, // silent
	}})
	sendSkillFlagChange(c, accountID, prevItemID, 61, 0, sqldb)
}

func snapshotSkillItemState(sqldb *sql.DB, skillItemID int64) (oldLocationID, oldFlag int64) {
	oldFlag = 7
	if sqldb == nil {
		return 0, oldFlag
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if er, err := evedb.GetEntityRow(ctx, sqldb, skillItemID); err == nil && er != nil {
		return er.LocationID, er.Flag
	}
	return 0, oldFlag
}

// itemDeletedNotifArgs builds the OnItemChange payload for a deleted item. Client subscribers
// iterate the change argument, so it must be a dict. Deletion is the row re-parented to the
// junkyard (location 6) with quantity 0, old location and quantity in the change dict.
func itemDeletedNotifArgs(er *evedb.EntityRow) marshal.PyTuple {
	gone := *er
	gone.LocationID = 6 // junkyard
	gone.Quantity = 0
	return marshal.PyTuple{Items: []marshal.PyRep{
		evedb.BuildEntityRow(&gone),
		marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyInt{Value: 3}, Value: marshal.PyInt{Value: uint64(er.LocationID)}}, // ixLocationID
			{Key: marshal.PyInt{Value: 7}, Value: marshal.PyInt{Value: uint64(er.Quantity)}},   // ixQuantity
		}},
	}}
}

// mergeLandedStack auto-stacks an item that just landed in a hangar, cargo hold or drone bay
// onto the stack of the same type already there, and tells the client. Only stackable landing
// flags are passed in; module slots keep their own charge rows.
func mergeLandedStack(ctx context.Context, c *evenet.Conn, addr string, accountID, itemID int64, sqldb *sql.DB) {
	if sqldb == nil || itemID == 0 {
		return
	}
	m, err := evedb.MergeLandedStack(ctx, sqldb, itemID)
	if err != nil {
		log.Printf("[%s] mergeLandedStack(%d): %v", addr, itemID, err)
		return
	}
	if m == nil {
		return
	}
	for _, delRow := range m.DeletedRows {
		_ = server.SendNotification(c, accountID, "OnItemChange", "charid", itemDeletedNotifArgs(delRow))
	}
	if keptER, _ := evedb.GetEntityRow(ctx, sqldb, m.KeptItemID); keptER != nil {
		_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
			evedb.BuildEntityRow(keptER),
			marshal.PyDict{Items: []marshal.DictEntry{
				{Key: marshal.PyInt{Value: 7}, Value: marshal.PyInt{Value: uint64(m.OldQty)}},
			}},
		}})
	}
	log.Printf("[%s] auto-stacked item %d into %d (%d → %d)", addr, m.DeletedIDs[0], m.KeptItemID, m.OldQty, m.NewQty)
}

// doCharAddImplant plugs implant itemID into charID (godma skill handler
// CharAddImplant).  Sends OnItemChange notifications for the plugged item and
// any slot-conflict implant that was destroyed.  Returns a user-facing error
// message, or "" on success.
func doCharAddImplant(c *evenet.Conn, addr string, accountID, charID, stationID, itemID int64, sqldb *sql.DB) string {
	if sqldb == nil || charID == 0 {
		return "not logged in"
	}
	if stationID == 0 {
		return "You must be docked in a station to plug in an implant."
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	before := charAttrSnapshot(ctx, sqldb, charID)
	var destroyed *evedb.EntityRow
	var pluggedID int64
	restarted, end, err := evedb.WithTrainingRetimed(ctx, sqldb, charID, func() error {
		var perr error
		destroyed, pluggedID, perr = evedb.PlugInImplant(ctx, sqldb, charID, itemID, stationID)
		return perr
	})
	// The running skill was stopped and restarted whatever fn did, so the
	// client is told either way; a refused plug restarts it at the old rate.
	afterImplantChange(ctx, c, accountID, charID, before, restarted, end, sqldb)
	if err != nil {
		log.Printf("[%s] CharAddImplant charID=%d item=%d: %v", addr, charID, itemID, err)
		return err.Error()
	}
	if destroyed != nil {
		_ = server.SendNotification(c, accountID, "OnItemChange", "charid", itemDeletedNotifArgs(destroyed))
	}
	if er, gerr := evedb.GetEntityRow(ctx, sqldb, pluggedID); gerr == nil && er != nil {
		change := marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyInt{Value: 3}, Value: marshal.PyInt{Value: uint64(stationID)}}, // old locationID
			{Key: marshal.PyInt{Value: 4}, Value: marshal.PyInt{Value: 4}},                 // old flag (hangar)
		}}
		_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
			evedb.BuildEntityRow(er), change,
		}})
	}
	log.Printf("[%s] → CharAddImplant charID=%d plugged=%d destroyedOld=%v", addr, charID, pluggedID, destroyed != nil)
	return ""
}

// doRemoveImplant unplugs (and destroys) implant itemID from charID and sends
// the item-deletion OnItemChange.  Returns a user-facing error message, or ""
// on success.
func doRemoveImplant(c *evenet.Conn, addr string, accountID, charID, itemID int64, sqldb *sql.DB) string {
	if sqldb == nil || charID == 0 {
		return "not logged in"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	before := charAttrSnapshot(ctx, sqldb, charID)
	var er *evedb.EntityRow
	restarted, end, err := evedb.WithTrainingRetimed(ctx, sqldb, charID, func() error {
		var uerr error
		er, uerr = evedb.UnplugImplant(ctx, sqldb, charID, itemID)
		return uerr
	})
	afterImplantChange(ctx, c, accountID, charID, before, restarted, end, sqldb)
	if err != nil {
		log.Printf("[%s] RemoveImplantFromCharacter charID=%d item=%d: %v", addr, charID, itemID, err)
		return err.Error()
	}
	_ = server.SendNotification(c, accountID, "OnItemChange", "charid", itemDeletedNotifArgs(er))
	log.Printf("[%s] → RemoveImplantFromCharacter charID=%d destroyed implant %d", addr, charID, itemID)
	return ""
}

// charAttrSnapshot reads the five effective character attributes (164-168)
// so a change can be pushed as old → new.
func charAttrSnapshot(ctx context.Context, sqldb *sql.DB, charID int64) map[int64]float64 {
	out := make(map[int64]float64, 5)
	for attrID := int64(164); attrID <= 168; attrID++ {
		out[attrID] = evedb.GetEffectiveCharAttr(ctx, sqldb, charID, attrID)
	}
	return out
}

// afterImplantChange pushes what a plug, unplug or pod death changes on the pilot: moved
// effective attributes (OnModuleAttributeChange on the character item) and the re-timed
// training (OnSkillStartTraining with the new end, plus the completion timer).
func afterImplantChange(ctx context.Context, c *evenet.Conn, accountID, charID int64,
	before map[int64]float64, restarted int64, end uint64, sqldb *sql.DB) {
	if c == nil || sqldb == nil || charID == 0 {
		return
	}
	for attrID := int64(164); attrID <= 168; attrID++ {
		now := evedb.GetEffectiveCharAttr(ctx, sqldb, charID, attrID)
		if old, ok := before[attrID]; !ok || old != now {
			sendModuleAttributeChange(c, accountID, charID, charID, attrID, now, before[attrID])
		}
	}
	shedFittingAfterImplantChange(ctx, c, accountID, charID, sqldb)
	// Turret and missile range hardwirings move the shown weapon numbers; fitting hardwirings
	// move the CPU/PG bars.
	pushWeaponAttrsForChar(c, accountID, charID, sqldb)
	if restarted != 0 {
		notifySkillQueueAdvance(c, accountID, charID,
			&evedb.SkillQueueAdvance{StartedItemID: restarted, StartedEnd: end}, sqldb)
	}
}

// shedFittingAfterImplantChange offlines modules on any of charID's ships that a lost fitting
// hardwiring left over CPU or powergrid (db.EnforceOwnerFittingLoads), and tells the client
// about the ones on the ship the pilot is in. pushWeaponAttrsForChar then sends the new load.
func shedFittingAfterImplantChange(ctx context.Context, c *evenet.Conn, accountID, charID int64, sqldb *sql.DB) {
	shed, err := evedb.EnforceOwnerFittingLoads(ctx, sqldb, charID)
	if err != nil {
		log.Printf("implant change charID=%d: fitting check: %v", charID, err)
	}
	if len(shed) == 0 {
		return
	}
	curShipID, _ := evedb.GetCurrentShipID(ctx, sqldb, charID)
	for shipID, mods := range shed {
		log.Printf("implant change charID=%d: ship %d over budget, offlined %v", charID, shipID, mods)
		if shipID != curShipID {
			continue
		}
		for _, modID := range mods {
			er, gerr := evedb.GetEntityRow(ctx, sqldb, modID)
			if gerr != nil || er == nil {
				continue
			}
			sendGodmaShipEffect(c, accountID, modID, er.TypeID, shipID, charID, 0, "online", false, 0, 0)
			sendModuleAttributeChange(c, accountID, charID, modID, dogma.AttrIsOnline, 0, 1)
		}
	}
}

// cancelSkillCompletionTimer stops any pending completion goroutine for charID
// that was armed by conn c.  Called on skill-training abort and connection
// teardown so a stale goroutine does not notify on a dead connection.
func cancelSkillCompletionTimer(c *evenet.Conn, charID int64) {
	skillTimersMu.Lock()
	defer skillTimersMu.Unlock()
	if t, ok := skillTimers[charID]; ok && (c == nil || t.conn == c) {
		t.cancel()
		delete(skillTimers, charID)
	}
}

func main() {
	// Queued-action flushes originate inside the destiny tick loop.
	destiny.FlushNodeID = globalNodeID

	cfgPath := flag.String("config", "config.json", "path to config.json")
	stringsPath := flag.String("strings", "data/strings.txt", "path to EVE string table file")
	cachePath := flag.String("cache", "data/cache", "path to directory of *.cache files")
	migrateIDs := flag.String("migrate-player-ids", "",
		`renumber player corps/alliances into the client's player ID range (>= 100000000): "plan" reports, "apply" rewrites`)
	flag.Parse()

	// config.json first: it names the log files. A config error before the
	// log is open reaches stderr only.
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	// Open the server log and tee all log output there. Truncated each run
	// unless server.logAppend is set.
	logFlags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if cfg.Server.LogAppend {
		logFlags = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}
	logFile, err := os.OpenFile(cfg.Server.LogFile, logFlags, 0o644)
	if err != nil {
		log.Fatalf("open %s: %v", cfg.Server.LogFile, err)
	}
	defer logFile.Close()
	// The bugtrack interceptor scans every log line for bug signals; it is
	// safe to attach before the database exists (events buffer until Init).
	log.SetOutput(io.MultiWriter(os.Stderr, logFile, bugtrack.Writer()))

	// Fatal runtime errors (unrecovered panic in an unguarded goroutine, out-of-memory, deadlock
	// detection) go to stderr without passing through the log package. Mirror them into their own
	// file, which must not share server.log's handle: the runtime writes at the fd's own offset.
	if crashFile, cErr := os.OpenFile(cfg.Server.CrashLogFile, logFlags, 0o644); cErr != nil {
		log.Printf("open %s: %v (fatal errors will only reach stderr)", cfg.Server.CrashLogFile, cErr)
	} else if sErr := debug.SetCrashOutput(crashFile, debug.CrashOptions{}); sErr != nil {
		log.Printf("SetCrashOutput: %v (fatal errors will only reach stderr)", sErr)
		crashFile.Close()
	}

	if err := marshal.LoadStringTable(*stringsPath); err != nil {
		log.Fatalf("load string table %q: %v", *stringsPath, err)
	}
	log.Printf("string table loaded from %s", *stringsPath)

	cacheStore := evecache.New()
	globalCacheStore = cacheStore
	if n, err := cacheStore.LoadDir(*cachePath); err != nil {
		log.Printf("cache: load files %q: %v (continuing)", *cachePath, err)
	} else if n > 0 {
		log.Printf("cache: loaded %d file entries from %q", n, *cachePath)
	}

	settingDefaults, _ := cfg.SettingDefaults() // validated by config.Load
	evedb.SetConfigSettingDefaults(settingDefaults)
	evenet.WriteTimeout = time.Duration(cfg.Server.WriteTimeoutSec) * time.Second
	shutdownWait = time.Duration(cfg.Server.ShutdownWaitSec) * time.Second
	shutdownFlushTimeout = time.Duration(cfg.Server.ShutdownFlushSec) * time.Second
	handshakeTimeout = time.Duration(cfg.Server.HandshakeTimeoutSec) * time.Second
	unauthIdleKick = time.Duration(cfg.Server.UnauthIdleKickSec) * time.Second
	evedb.SetRoleMasks(cfg.ESP.StaffRoles, cfg.Authentication.SlashRoles)
	if cfg.Authentication.AutoAccount && (evedb.IsStaffRole(cfg.Authentication.Role) || evedb.IsSlashRole(cfg.Authentication.Role)) {
		log.Printf("WARNING: authentication.autoaccount is on and authentication.role (%d) carries a staff bit: every new account gets GM access", cfg.Authentication.Role)
	}

	// Publish gameplay rate multipliers before anything else runs.
	rates.SkillTraining = cfg.Rates.SkillTraining
	rates.NPCBounty = cfg.Rates.NPCBounty
	rates.MiningYield = cfg.Rates.MiningYield
	rates.LootDrop = cfg.Rates.LootDrop
	rates.PlayerDamage = cfg.Rates.PlayerDamage
	rates.NPCDamage = cfg.Rates.NPCDamage
	rates.IndustrySpeed = cfg.Rates.IndustrySpeed
	rates.ProductionSpeed = cfg.Rates.ProductionSpeed
	rates.ResearchSpeed = cfg.Rates.ResearchSpeed
	rates.ResearchPoints = cfg.Rates.ResearchPoints
	rates.MissionReward = cfg.Rates.MissionReward
	rates.LoyaltyPoints = cfg.Rates.LoyaltyPoints
	rates.StandingGain = cfg.Rates.StandingGain
	rates.RatStandingLoss = cfg.Rates.RatStandingLoss
	if cfg.Rates != config.DefaultRates() {
		log.Printf("rates: skillTraining=%.2f npcBounty=%.2f miningYield=%.2f lootDrop=%.2f playerDamage=%.2f npcDamage=%.2f industrySpeed=%.2f productionSpeed=%.2f researchSpeed=%.2f researchPoints=%.2f missionReward=%.2f loyaltyPoints=%.2f standingGain=%.2f ratStandingLoss=%.2f",
			rates.SkillTraining, rates.NPCBounty, rates.MiningYield, rates.LootDrop,
			rates.PlayerDamage, rates.NPCDamage, rates.IndustrySpeed, rates.ProductionSpeed, rates.ResearchSpeed, rates.ResearchPoints,
			rates.MissionReward, rates.LoyaltyPoints, rates.StandingGain, rates.RatStandingLoss)
	}

	if !logx.SetLevel(cfg.Server.LogLevel) {
		log.Printf("config: unknown logLevel %q — staying at %q", cfg.Server.LogLevel, logx.LevelName())
	}
	if logx.Level() != logx.LevelInfo {
		log.Printf("log level: %s", logx.LevelName())
	}

	// Off by default; see destiny.StampBudgetEnforced. Logged when on because a refused send is
	// otherwise indistinguishable from a bug.
	destiny.StampBudgetEnforced = cfg.Server.EnforceStampBudget
	if destiny.StampBudgetEnforced {
		log.Printf("destiny: stamp budget ENFORCED — over-ceiling sends are refused, not just counted")
	}
	// Pointer so an omitted key keeps the default instead of reading as 0, which disables the
	// resync net.
	if cfg.Server.StampResyncLead != nil {
		stampLeadResyncThreshold = *cfg.Server.StampResyncLead
	}
	combatDebug = cfg.Server.CombatDebug
	npc.CombatDebug = cfg.Server.CombatDebug
	if combatDebug {
		log.Printf("combat: per-hit debug trace ON")
	}
	missileHardpointOffsets = cfg.Server.MissileHardpointOffsets
	if missileHardpointOffsets {
		log.Printf("missiles: per-slot hardpoint offsets ON (probe P1)")
	}
	if stampLeadResyncThreshold <= 0 {
		log.Printf("destiny: stamp-lead resync net DISABLED — a client whose sim clock runs ahead will not recover in-session")
	}

	ln, err := gonet.Listen("tcp", cfg.Server.Listen)
	if err != nil {
		log.Fatalf("listen %s: %v", cfg.Server.Listen, err)
	}
	log.Printf("GuriMoon listening on %s", ln.Addr())
	log.Printf("expect client: birthday=170472  macho=%d  version=%.2f  build=%d  project=%q",
		machoVersion, bootVersion, bootBuild, projectVersion)
	log.Printf("database: host=%s port=%d db=%s user=%s",
		cfg.Database.Host, cfg.Database.Port, cfg.Database.DB, cfg.Database.User)

	sqldb, err := evedb.OpenPool(cfg.Database.DSN(), evedb.PoolOptions{
		MaxOpen:     cfg.Database.MaxOpenConns,
		MaxIdle:     cfg.Database.MaxIdleConns,
		MaxLifetime: time.Duration(cfg.Database.ConnMaxLifetimeMinutes) * time.Minute,
		MaxIdleTime: time.Duration(cfg.Database.ConnMaxIdleMinutes) * time.Minute,
	})
	if err != nil {
		log.Fatalf("db connect: %v", err)
	}
	defer sqldb.Close()
	log.Printf("database connected")

	// Opt-in data migration: player corporations and alliances numbered below
	// evedb.MinPlayerEntityID read as static NPC entities to the client.
	if *migrateIDs != "" {
		apply := *migrateIDs == "apply"
		if !apply && *migrateIDs != "plan" {
			log.Fatalf("-migrate-player-ids: want \"plan\" or \"apply\", got %q", *migrateIDs)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		report, mErr := evedb.MigratePlayerEntityIDs(ctx, sqldb, apply)
		cancel()
		for _, line := range report {
			log.Printf("migrate-player-ids: %s", line)
		}
		if mErr != nil {
			log.Fatalf("migrate-player-ids: %v", mErr)
		}
		log.Printf("migrate-player-ids: done (%s)", *migrateIDs)
	}

	// Load the effect map from dgmEffects for DB-driven effect lookups.
	{
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		em, emErr := evedb.GetAllEffects(ctx, sqldb)
		cancel()
		if emErr != nil {
			log.Printf("WARNING: failed to load effect map: %v (using fallback)", emErr)
			effectMap = make(map[string]evedb.EffectInfo)
		} else {
			effectMap = em
		}
	}

	// Mines (mines.go): their table, and group 92 in every missile launcher's charge groups,
	// before the cachables are primed so dgmtypeattribs carries the new rows.
	if err := evedb.EnsureMineTables(context.Background(), sqldb); err != nil {
		log.Printf("EnsureMineTables: %v", err)
	}
	if n, err := evedb.EnsureMineLauncherCharges(context.Background(), sqldb); err != nil {
		log.Printf("EnsureMineLauncherCharges: %v", err)
	} else if n > 0 {
		log.Printf("[mines] %d launcher type(s) opened to mines", n)
	}

	// Pre-generate the login cachables so the first LoginHints call pays no generation latency.
	log.Printf("cache: priming login cachables from database…")
	cacheStore.Prime(context.Background(), sqldb)
	log.Printf("cache: prime complete (%d entries)", cacheStore.Len())

	// Start the central tick manager (1-second interval).
	tickMgr := tick.New(1 * time.Second)
	tickMgr.Start(context.Background())
	defer tickMgr.Stop()
	globalTickMgr = tickMgr
	runStarbaseBootReconcile(sqldb)
	runCynoBootReconcile(sqldb)
	startCynoExpirySweep(sqldb)
	startStarbaseOnliningSweep(tickMgr, sqldb)
	startStructureOnliningSweep(tickMgr, sqldb)
	startMoonHarvestSweep(tickMgr, sqldb)
	// POS combat: structures as entities (+HP persistence), reinforcement,
	// and the sentry/EW battery loop.
	installStructureEntities(sqldb, tickMgr)
	startStarbaseReinforceSweep(tickMgr, sqldb)
	startPOSGuns(tickMgr, sqldb)
	startRefinerySweep(tickMgr, sqldb)
	installPOSIndustryHooks(tickMgr, sqldb)
	startStarbaseFuelSweep(tickMgr, sqldb)
	startAggressionSweep(tickMgr)
	startLootExpirySweep(tickMgr)
	startLootStatsLog(tickMgr)
	startOfferNoticeSweep(tickMgr)
	startPatentDrawSweep(tickMgr)
	startLocatorSweep(tickMgr)
	startMissionExpirySweep(tickMgr)
	startResearchSlotSweep(tickMgr)
	startDunSiteSweep(tickMgr)
	startMapHistoryPurgeSweep(tickMgr, sqldb)
	startOrphanCapsuleSweep(tickMgr, sqldb)
	startConcordWatch(tickMgr, sqldb)
	startPoliceWatch(tickMgr, sqldb)
	startCustomsWatch(tickMgr, sqldb)
	startSentryWatch(tickMgr)
	startMineWatch(tickMgr, sqldb)
	loadSecuritySettings(context.Background(), sqldb)
	startSecStatusDecay(tickMgr, sqldb)
	// Arrival lanes hold a reservation per arrival for the length of the trip it was claimed for;
	// without this sweep a system keeps every point ever landed on.
	tickMgr.RegisterPeriodic("arrival-lane-sweep", 60, func(uint64) bool {
		destiny.SweepArrivalLanes()
		return true
	})

	// Celestial cache: preloads system security, faction and system-region maps at boot;
	// celestials are cached per region on first access.
	globalCelestialCache = evedb.NewCelestialCache()
	if err := globalCelestialCache.Preload(context.Background(), sqldb); err != nil {
		log.Printf("[boot] celestial cache preload failed: %v — will query per-system", err)
	}
	evedb.SharedCelestialCache = globalCelestialCache

	// NPC spawner manager. Spawners are loaded per system in registerUndock and ticked every 5 s.
	spawnerMgr := npc.NewSpawnerManager()
	globalSpawnerMgr = spawnerMgr

	// Generic key/value settings store (server_settings). Available to any
	// subsystem or module that needs restart-surviving runtime state.
	if err := evedb.EnsureServerSettingsTable(context.Background(), sqldb); err != nil {
		log.Fatalf("EnsureServerSettingsTable: %v", err)
	}
	// In-memory copy for hot-path dials (CONCORD delay, NPC lock time, loot
	// rolls, market levers): ESP edits land at once, raw SQL within 30 s.
	evedb.StartSettingsCache(context.Background(), sqldb, 30*time.Second)

	// Ensure the skill-training persistence table exists.
	if err := evedb.EnsureSkillTrainingTable(context.Background(), sqldb); err != nil {
		log.Fatalf("EnsureSkillTrainingTable: %v", err)
	}

	// Ensure the skill-queue table exists (managed via ESP; the 3913 client
	// has no queue UI of its own).
	if err := evedb.EnsureSkillQueueTable(context.Background(), sqldb); err != nil {
		log.Fatalf("EnsureSkillQueueTable: %v", err)
	}

	// Ensure the dungeon editor tables exist.
	if err := evedb.EnsureDungeonTables(context.Background(), sqldb); err != nil {
		log.Fatalf("EnsureDungeonTables: %v", err)
	}

	// Per-ball authored dungeon drops (mission items, scripted flavour loot).
	if err := evedb.EnsureDungeonLootTable(context.Background(), sqldb); err != nil {
		log.Fatalf("EnsureDungeonLootTable: %v", err)
	}

	// 3D editor prefabs (saved ball groups). Editor-only, so not fatal.
	if err := evedb.EnsureDungeonPrefabTable(context.Background(), sqldb); err != nil {
		log.Printf("EnsureDungeonPrefabTable: %v", err)
	}

	// Ensure the custom celestial tracking table exists (ESP Solar Systems).
	if err := evedb.EnsureOutpostTables(context.Background(), sqldb); err != nil {
		log.Fatalf("EnsureOutpostTables: %v", err)
	}

	if err := evedb.EnsureCustomCelestialTable(context.Background(), sqldb); err != nil {
		log.Fatalf("EnsureCustomCelestialTable: %v", err)
	}
	if err := evedb.EnsureCustomStargateTable(context.Background(), sqldb); err != nil {
		log.Fatalf("EnsureCustomStargateTable: %v", err)
	}

	// Ensure the news feed table exists.
	if err := evedb.EnsureNewsTable(context.Background(), sqldb); err != nil {
		log.Fatalf("EnsureNewsTable: %v", err)
	}

	// Ensure the Intergalactic Summit's tables exist (Galnet's board, esp/summit.go).
	if err := evedb.EnsureSummitTables(context.Background(), sqldb); err != nil {
		log.Fatalf("EnsureSummitTables: %v", err)
	}

	// Ensure the NPC trade price-override column exists.
	if err := evedb.EnsureNPCTradePriceColumn(context.Background(), sqldb); err != nil {
		log.Fatalf("EnsureNPCTradePriceColumn: %v", err)
	}

	// Ensure the NPC spawner tables exist.
	if err := evedb.EnsureSpawnerTables(context.Background(), sqldb); err != nil {
		log.Fatalf("EnsureSpawnerTables: %v", err)
	}
	// Runs after the dungeon and spawner tables exist: it renames
	// dunObjects.brainProfile to stance and drops the column everywhere else.
	if err := evedb.MigrateNPCStance(context.Background(), sqldb); err != nil {
		log.Fatalf("MigrateNPCStance: %v", err)
	}
	if err := evedb.TruncateLiveNPCs(context.Background(), sqldb); err != nil {
		log.Printf("[startup] TruncateLiveNPCs: %v", err)
	}
	// Belt rats are not truncated: a belt half cleared before a restart stays half cleared. Only
	// the materialised ball IDs go, since they were entity allocations of the last process.
	if err := evedb.ClearAllBeltRatEntities(context.Background(), sqldb); err != nil {
		log.Printf("[startup] ClearAllBeltRatEntities: %v", err)
	}
	// Belt rocks, next to belt rats. A missing table re-rolls every belt forever, so this is fatal.
	// No truncate: asteroid entity IDs are fixed offsets from destiny.AsteroidIDBase and stay valid
	// across a restart.
	if err := evedb.EnsureAsteroidTables(context.Background(), sqldb); err != nil {
		log.Fatalf("EnsureAsteroidTables: %v", err)
	}
	bootstrapAsteroidFields(sqldb)
	bootstrapMoonResources(sqldb)
	// Region rosters load after EnsureSpawnerTables so the rows its migrations write are in them.
	spawnerMgr.PreloadRosters(context.Background(), sqldb, globalCelestialCache.SystemRegion)
	// Squad tiers, the belt-spawn settings and the officer ledger. Live officer counts reset since
	// nothing is alive across a restart; the post-kill cool-down persists.
	spawnerMgr.PreloadOfficerLedger(context.Background(), sqldb)
	if err := evedb.EnsureDungeonNPCTable(context.Background(), sqldb); err != nil {
		log.Fatalf("EnsureDungeonNPCTable: %v", err)
	}

	// Ensure the mission system tables exist and seed division weights.
	// Must run before dungeon NPC cleanup so the instance tables exist.
	if err := evedb.EnsureMissionTables(context.Background(), sqldb); err != nil {
		log.Fatalf("EnsureMissionTables: %v", err)
	}
	if err := evedb.EnsureResearchTables(context.Background(), sqldb); err != nil {
		log.Fatalf("EnsureResearchTables: %v", err)
	}
	if err := evedb.EnsureRnDLotteryTables(context.Background(), sqldb); err != nil {
		log.Fatalf("EnsureRnDLotteryTables: %v", err)
	}
	if err := evedb.EnsureStorylineTables(context.Background(), sqldb); err != nil {
		log.Fatalf("EnsureStorylineTables: %v", err)
	}
	if err := evedb.EnsureOfferNoticeTable(context.Background(), sqldb); err != nil {
		log.Fatalf("EnsureOfferNoticeTable: %v", err)
	}

	// Agent side-services: distribution grid, locator jobs, dialogue texts.
	if err := evedb.EnsureAgentServiceTables(context.Background(), sqldb); err != nil {
		log.Fatalf("EnsureAgentServiceTables: %v", err)
	}
	locatorMailDelay = time.Duration(cfg.Agents.LocatorDelayMinutes) * time.Minute
	// config.json jumpClone.minStanding: the install standing gate.
	evedb.JumpCloneMinStanding = *cfg.JumpClone.MinStanding
	// Alliance applications / members / relationships.  This also backfills a
	// member row for every corporation already linked to an alliance, so an
	// alliance built before the table existed has a Members tab.
	if err := evedb.EnsureAllianceTables(context.Background(), sqldb); err != nil {
		log.Fatalf("EnsureAllianceTables: %v", err)
	}
	if err := evedb.EnsureWarTables(context.Background(), sqldb); err != nil {
		log.Fatalf("EnsureWarTables: %v", err)
	}
	// Bills also registers the billType*/folder constants the client
	// dereferences, and must run before the first bill row exists.
	if err := evedb.EnsureBillTables(context.Background(), sqldb); err != nil {
		log.Fatalf("EnsureBillTables: %v", err)
	}
	// Research facility slots (Duplicating / Reverse Engineering), see researchslots.go. Inert for
	// any client without the lobby-button patch.
	if err := evedb.EnsureResearchSlotTables(context.Background(), sqldb); err != nil {
		log.Fatalf("EnsureResearchSlotTables: %v", err)
	}
	if err := evedb.EnsureKillmailTables(context.Background(), sqldb); err != nil {
		log.Fatalf("EnsureKillmailTables: %v", err)
	}
	// Name the kill site of every killmail that predates the location columns,
	// from the position it already stores. Off the boot path; a no-op once done.
	go evedb.LogKillLocationBackfill(context.Background(), sqldb)
	// market_journal.reason ships as VARCHAR(43); under STRICT_TRANS_TABLES a reason carrying a
	// station or item name fails with error 1406 and rolls back its transaction (office rental,
	// skill injection).
	if err := evedb.EnsureJournalSchema(context.Background(), sqldb); err != nil {
		log.Fatalf("EnsureJournalSchema: %v", err)
	}
	// entity_attributes ships with a restrictive foreign key: deleting an item that has attribute
	// rows (a repackaged ship) fails with error 1451. The chrNotes itemID key is dropped because
	// notes on NPC agents have no entity row.
	if err := evedb.EnsureEntityChildCascade(context.Background(), sqldb); err != nil {
		log.Fatalf("EnsureEntityChildCascade: %v", err)
	}
	if err := evedb.EnsureNoteItemUnbound(context.Background(), sqldb); err != nil {
		log.Printf("EnsureNoteItemUnbound: %v", err)
	}
	if err := evedb.EnsureShipConfigTable(context.Background(), sqldb); err != nil {
		log.Printf("EnsureShipConfigTable: %v", err)
	}
	// A corporation, alliance or title change drops the pilot's memoised
	// Show Info answers (db/identitychanged.go).
	evedb.IdentityChanged = infocache.Shared.InvalidateID
	// A sov edit drops the memoised stationSvc.GetSolarSystem row; a
	// region-wide edit (0) clears the memo rather than enumerating systems.
	evedb.SolarSystemRowChanged = func(systemID int64) {
		if systemID == 0 {
			infocache.Shared.Clear()
			return
		}
		infocache.Shared.InvalidateID(systemID)
	}
	// A relationship write drops the memoised alliance.GetRelationships reply; 0 (an alliance
	// deleted from the ESP panel) clears the memo.
	evedb.AllianceRelationshipsChanged = func(allianceID int64) {
		if allianceID == 0 {
			infocache.Shared.Clear()
			return
		}
		infocache.Shared.InvalidateID(allianceID)
	}
	// CustomInfo and the client's own missing-message fallbacks are absent from the shipped
	// catalogue, so refusals that use them show nothing (clientmsg.go).
	if err := evedb.EnsureClientMessages(context.Background(), sqldb); err != nil {
		log.Printf("EnsureClientMessages: %v", err)
	}
	if err := evedb.EnsureCourierSchema(context.Background(), sqldb); err != nil {
		log.Fatalf("EnsureCourierSchema: %v", err)
	}
	if err := evedb.EnsureEscrowSchema(context.Background(), sqldb); err != nil {
		log.Fatalf("EnsureEscrowSchema: %v", err)
	}
	// Whether NPC corporations put the ore they buy back on their shelf (db/marketresale.go).
	// Read once here because PlaceSellOrder's match loop consults it. Off unless an operator
	// enables it.
	evedb.LoadNPCResaleSettings(context.Background(), sqldb)
	// Not fatal: the server runs fine on an unindexed mailbox, just slowly.
	if err := evedb.EnsureEntityIndexes(context.Background(), sqldb); err != nil {
		log.Printf("EnsureEntityIndexes: %v", err)
	}

	if err := evedb.EnsureMailIndexes(context.Background(), sqldb); err != nil {
		log.Printf("EnsureMailIndexes: %v", err)
	}
	// A mail sent with a raw Unix timestamp instead of a Win32 FILETIME is
	// small enough to wire-encode as a plain int instead of a long, which
	// crashes the client's inbox FmtDate. Reinterpret any such row in place.
	if err := evedb.RepairMailTimestamps(context.Background(), sqldb); err != nil {
		log.Printf("RepairMailTimestamps: %v", err)
	}
	// Where a synthetic pilot's kill record goes when its mails age out.  Not
	// fatal either: without the table the retention sweep refuses to delete
	// anything, which is the safe direction.
	if err := evedb.EnsureKillRollupSchema(context.Background(), sqldb); err != nil {
		log.Printf("EnsureKillRollupSchema: %v", err)
	}
	// Alliances are not rows in `entity`: the short-name lookup reads alliance_shortnames. Remove
	// stray alliance rows and settle the item allocator's AUTO_INCREMENT back onto real items.
	if err := evedb.RepairAllianceEntityRows(context.Background(), sqldb); err != nil {
		log.Printf("RepairAllianceEntityRows: %v", err)
	}
	// Charges loaded past their module's capacity (2 crystals in a laser, a 200-round magazine in
	// a gun holding 80): the excess goes to cargo.
	if n, err := evedb.RepairOverloadedCharges(context.Background(), sqldb); err != nil {
		log.Printf("RepairOverloadedCharges: %v", err)
	} else if n > 0 {
		log.Printf("RepairOverloadedCharges: trimmed %d overloaded charge stack(s) to cargo", n)
	}
	if n, err := evedb.CleanOrphanCapsules(context.Background(), sqldb); err != nil {
		log.Printf("CleanOrphanCapsules: %v", err)
	} else if n > 0 {
		log.Printf("CleanOrphanCapsules: deleted %d orphan capsule(s)", n)
	}
	// character_.startDateTime is the date the pilot joined its current corp. Rebuild it from
	// chrEmployment where the corp-change paths left it unset.
	if err := evedb.RepairCharacterCorpStartDates(context.Background(), sqldb); err != nil {
		log.Printf("RepairCharacterCorpStartDates: %v", err)
	}

	// Selective dungeon NPC cleanup: only delete records for missions no longer
	// active, keeping NPC records for accepted missions so they can be
	// re-spawned when the system loads.
	if n, err := evedb.DeleteDungeonNPCsForStaleMissions(context.Background(), sqldb); err != nil {
		log.Printf("[startup] DeleteDungeonNPCsForStaleMissions: %v", err)
	} else if n > 0 {
		log.Printf("[startup] DeleteDungeonNPCsForStaleMissions: removed %d stale rows", n)
	}
	// Clean up dungeon instances for missions no longer accepted.
	if n, err := evedb.CleanupStaleDungeonInstances(context.Background(), sqldb); err != nil {
		log.Printf("[startup] CleanupStaleDungeonInstances: %v", err)
	} else if n > 0 {
		log.Printf("[startup] CleanupStaleDungeonInstances: removed %d stale instances", n)
	}
	// Reset spawned flags so NPCs are re-spawned into memory.
	if err := evedb.ResetDungeonInstanceSpawned(context.Background(), sqldb); err != nil {
		log.Printf("[startup] ResetDungeonInstanceSpawned: %v", err)
	}
	// Set package-level DB for async site state persistence.
	dunSiteDB = sqldb
	globalDB = sqldb
	// Restore in-memory site state (killed objects, released waves) from DB.
	loadDungeonSiteStates(sqldb)

	// Ensure mutable standing tables exist (mission complete/decline writes).
	if err := evedb.EnsureStandingTables(context.Background(), sqldb); err != nil {
		log.Fatalf("EnsureStandingTables: %v", err)
	}

	// Ensure the wallet journal table exists (mission/bounty/market writes).
	if err := evedb.EnsureWalletJournalTable(context.Background(), sqldb); err != nil {
		log.Fatalf("EnsureWalletJournalTable: %v", err)
	}

	// Ensure market_transactions has a real owner column, separate from the
	// counterparty (clientID).
	if err := evedb.EnsureTransactionOwnerColumn(context.Background(), sqldb); err != nil {
		log.Fatalf("EnsureTransactionOwnerColumn: %v", err)
	}

	// market_history_old takes the daily price rollup (db/pricehistoryroll.go).
	// Non-fatal: without it the rollup fails and logs, and the chart keeps
	// reading raw rows.
	if err := evedb.EnsureMarketHistorySchema(context.Background(), sqldb); err != nil {
		log.Printf("EnsureMarketHistorySchema: %v (non-fatal)", err)
	}

	// Ensure the insurance table exists.
	if err := evedb.EnsureInsuranceTable(context.Background(), sqldb); err != nil {
		log.Fatalf("EnsureInsuranceTable: %v", err)
	}

	// Ensure the RAM/industry tables exist.
	if err := evedb.EnsureRAMTables(context.Background(), sqldb); err != nil {
		log.Fatalf("EnsureRAMTables: %v", err)
	}

	// Ensure the tutorial completion tracking table exists.
	if err := evedb.EnsureTutorialTables(context.Background(), sqldb); err != nil {
		log.Fatalf("EnsureTutorialTables: %v", err)
	}

	// Ensure the corp write-side tables (votes, lockdown, member-role
	// columns) exist.
	if err := evedb.EnsureCorpWriteTables(context.Background(), sqldb); err != nil {
		log.Fatalf("EnsureCorpWriteTables: %v", err)
	}
	if n, err := evedb.PruneLockedItems(context.Background(), sqldb); err != nil {
		log.Printf("PruneLockedItems: %v", err)
	} else if n > 0 {
		log.Printf("corp lockdown: dropped %d stale lock(s)", n)
	}

	// Undo the uint32 truncation the shipped eveConstants dump applied to
	// every corporation role bit above 2^31.  Not fatal: a stale constant
	// mis-labels the corp UI, it does not stop the server.
	if err := evedb.RepairCorpRoleConstants(context.Background(), sqldb); err != nil {
		log.Printf("RepairCorpRoleConstants: %v", err)
	}

	// Ensure the cloneTypeID column exists on character_.
	if err := evedb.EnsureCloneColumn(context.Background(), sqldb); err != nil {
		log.Fatalf("EnsureCloneColumn: %v", err)
	}

	// Ensure the homeStationID column exists on character_.
	if err := evedb.EnsureHomeStationColumn(context.Background(), sqldb); err != nil {
		log.Fatalf("EnsureHomeStationColumn: %v", err)
	}

	// Move chargen base attributes stored under bonus IDs 175-179 into base slots 164-168.
	// Idempotent; safe every startup.
	if n, err := evedb.MigrateCharBaseAttrs(context.Background(), sqldb); err != nil {
		log.Printf("MigrateCharBaseAttrs: %v (non-fatal)", err)
	} else if n > 0 {
		log.Printf("MigrateCharBaseAttrs: migrated %d base attribute rows", n)
	}

	// Reset over-granted corp roles: non-CEO members drop to default roles,
	// CEOs keep RoleAll. One-shot, guarded by server_migrations.
	if n, err := evedb.MigrateCorpMemberRoles(context.Background(), sqldb); err != nil {
		log.Printf("MigrateCorpMemberRoles: %v (non-fatal)", err)
	} else if n > 0 {
		log.Printf("MigrateCorpMemberRoles: reset %d role rows", n)
	}

	// Reconcile the NPC supply/demand table with the invTypes data we ship:
	// publish traded-but-unpublished items and drop rows for types that do not
	// exist here. One-shot, guarded by server_migrations.
	if pub, purged, err := evedb.MigrateNPCTradeSeedability(context.Background(), sqldb); err != nil {
		log.Printf("MigrateNPCTradeSeedability: %v (non-fatal)", err)
	} else if pub > 0 || purged > 0 {
		log.Printf("MigrateNPCTradeSeedability: published %d types, purged %d dead trade rows (reseed to apply)", pub, purged)
	}

	// Put meta/T2 module variants back in the market tree: give them their
	// parent type's market group and publish them. One-shot, guarded by
	// server_migrations.
	if regrouped, published, err := evedb.MigrateMarketVariantVisibility(context.Background(), sqldb); err != nil {
		log.Printf("MigrateMarketVariantVisibility: %v (non-fatal)", err)
	} else if regrouped > 0 || published > 0 {
		log.Printf("MigrateMarketVariantVisibility: regrouped %d variants, published %d", regrouped, published)
	}

	// Publish all items that sit in a real market group but are still
	// published = 0 in the seed data (T1 frigates, battleships, etc.).
	if n, err := evedb.MigratePublishMarketGroupedItems(context.Background(), sqldb); err != nil {
		log.Printf("MigratePublishMarketGroupedItems: %v (non-fatal)", err)
	} else if n > 0 {
		log.Printf("MigratePublishMarketGroupedItems: published %d items (reseed to apply)", n)
	}

	// Populate ship basePrices from blueprint mineral costs so that the
	// client's insurance window considers them insurable. One-shot, guarded
	// by server_migrations.
	if n, err := evedb.MigrateShipBasePrices(context.Background(), sqldb); err != nil {
		log.Printf("MigrateShipBasePrices: %v (non-fatal)", err)
	} else if n > 0 {
		log.Printf("MigrateShipBasePrices: set basePrice on %d ship types (reseed to apply)", n)
	}

	// Price the clone grades (sql/invtypes.sql carries the same values). One-shot,
	// guarded by server_migrations; a hand-set price is kept.
	if n, err := evedb.MigrateCloneGradePrices(context.Background(), sqldb); err != nil {
		log.Printf("MigrateCloneGradePrices: %v (non-fatal)", err)
	} else if n > 0 {
		log.Printf("MigrateCloneGradePrices: priced %d clone grades", n)
	}

	// Conquerable stations start under CONCORD, the neutral first holder (sql/stastations.sql
	// carries the same owners). One-shot, guarded by server_migrations; a station a real
	// corporation holds is kept.
	if n, err := evedb.MigrateConquerablesToCONCORD(context.Background(), sqldb); err != nil {
		log.Printf("MigrateConquerablesToCONCORD: %v (non-fatal)", err)
	} else if n > 0 {
		log.Printf("MigrateConquerablesToCONCORD: handed %d conquerable stations to CONCORD", n)
	}

	// Seed the ice refine table: the RMR dump ships ore reprocessing rows but none for ice.
	// One-shot, guarded by server_migrations.
	if n, err := evedb.MigrateIceReprocessing(context.Background(), sqldb); err != nil {
		log.Printf("MigrateIceReprocessing: %v (non-fatal)", err)
	} else if n > 0 {
		log.Printf("MigrateIceReprocessing: seeded %d ice refine rows", n)
	}

	// Ensure the jump-clone table exists.
	if err := evedb.EnsureJumpCloneTables(context.Background(), sqldb); err != nil {
		log.Fatalf("EnsureJumpCloneTables: %v", err)
	}

	// Ensure the bot character assignment table exists (managed via ESP).

	// Ensure the NPC loot authoring tables exist (managed via ESP).
	if err := evedb.EnsureNPCLootTables(context.Background(), sqldb); err != nil {
		log.Fatalf("EnsureNPCLootTables: %v", err)
	}

	// Ensure the automatic bug tracker table exists and start the collector
	// (log interceptor events buffered since startup drain from here).
	if err := evedb.EnsureBugReportsTable(context.Background(), sqldb); err != nil {
		log.Fatalf("EnsureBugReportsTable: %v", err)
	}
	bugtrack.Init(sqldb)

	// Widen legacy market_orders columns so player orders can store negative
	// ranges (rangeStation = -1) and ISK-sized escrow amounts.
	if err := evedb.EnsureMarketOrdersSchema(context.Background(), sqldb); err != nil {
		log.Fatalf("EnsureMarketOrdersSchema: %v", err)
	}

	// Apply the performance indexes (db/perfindex.go, the executable form of
	// sql/optional/perf_indexes*.sql). Runs after every Ensure* that creates a table and before the listener
	// opens, since the first pass on a large database is online DDL.
	if err := evedb.EnsurePerfIndexes(context.Background(), sqldb); err != nil {
		log.Printf("EnsurePerfIndexes: %v (non-fatal)", err)
	}

	// Fix skill entities whose locationID was incorrectly set to stationID
	// instead of characterID (ownerID).  Safe to run every startup.
	if n, err := evedb.FixSkillLocationIDs(context.Background(), sqldb); err != nil {
		log.Printf("FixSkillLocationIDs: %v (non-fatal)", err)
	} else if n > 0 {
		log.Printf("FixSkillLocationIDs: corrected %d skill entities", n)
	}

	// Remove stale character-private channel rows that were accumulated by
	// JoinChannels in past sessions, but preserve each character's own channel.
	if err := evedb.CleanupStalePrivateChannels(context.Background(), sqldb); err != nil {
		log.Printf("CleanupStalePrivateChannels: %v (non-fatal)", err)
	}

	// Re-create any character private channel that was deleted (channelID = characterID, per
	// chardb.go).
	evedb.RepairCharacterChannels(context.Background(), sqldb)

	// Chat ACL table (buddy list, block list, mutes, channel moderation).
	if err := evedb.EnsureChannelModsTable(context.Background(), sqldb); err != nil {
		log.Printf("EnsureChannelModsTable: %v (non-fatal)", err)
	}

	// Backfill persisted online state (attr 2) for modules fitted before online tracking existed:
	// they stay online.
	if err := evedb.EnsureFittedModuleOnlineState(context.Background(), sqldb); err != nil {
		log.Printf("EnsureFittedModuleOnlineState: %v (non-fatal)", err)
	}
	// ...then offline anything the backfill (or seeding/GM tools) left online
	// beyond the ship's CPU/powergrid budget.
	if n, err := evedb.EnforceBootFittingLoads(context.Background(), sqldb); err != nil {
		log.Printf("EnforceBootFittingLoads: %v (non-fatal)", err)
	} else if n > 0 {
		log.Printf("EnforceBootFittingLoads: offlined %d over-budget modules", n)
	}

	// Portrait recustomization flags set by /redoportrait.
	if err := evedb.EnsureRecustomizationTable(context.Background(), sqldb); err != nil {
		log.Printf("EnsureRecustomizationTable: %v (non-fatal)", err)
	}

	// Loot container persistence table.
	if err := evedb.EnsureLootTables(context.Background(), sqldb); err != nil {
		log.Fatalf("EnsureLootTables: %v", err)
	}
	// Drone space-state persistence table.
	if err := evedb.EnsureDroneTables(context.Background(), sqldb); err != nil {
		log.Fatalf("EnsureDroneTables: %v", err)
	}
	// Character deletion countdown column.
	if err := evedb.EnsureCharacterDeleteSchema(context.Background(), sqldb); err != nil {
		log.Fatalf("EnsureCharacterDeleteSchema: %v", err)
	}
	// Remove loot containers that are expired or have no tracking row
	// (legacy). Tracked, non-expired containers survive and reload with
	// their system.
	if err := evedb.CleanupSpaceLootContainers(context.Background(), sqldb, int64(lootContainerTTL().Seconds())); err != nil {
		log.Printf("CleanupSpaceLootContainers: %v (non-fatal)", err)
	}

	// Reconcile orphaned drones: scoop to bay if home ship exists, delete
	// otherwise. Must run before any system loads.
	if err := evedb.ReconcileSpaceDrones(context.Background(), sqldb); err != nil {
		log.Printf("ReconcileSpaceDrones: %v (non-fatal)", err)
	}

	// Return items from orphaned trade-escrow containers.
	if n, err := evedb.ReturnOrphanTradeContainers(context.Background(), sqldb); err != nil {
		log.Printf("ReturnOrphanTradeContainers: %v (non-fatal)", err)
	} else if n > 0 {
		log.Printf("[db] ReturnOrphanTradeContainers: recovered %d trade containers", n)
	}

	// Skills injected into a brain must be singletons or the client's godma
	// discards them (Godma:UpdateItem.skip3) and reads them as level 0.
	if n, err := evedb.RepairSkillSingletons(context.Background(), sqldb); err != nil {
		log.Printf("RepairSkillSingletons: %v (non-fatal)", err)
	} else if n > 0 {
		log.Printf("[db] RepairSkillSingletons: promoted %d injected skills to singleton", n)
	}

	// Charges are never singletons: fix any that slipped through (e.g. MultiAdd routing a charge
	// through FitModuleToShip).
	if n, err := evedb.RepairChargeSingletons(context.Background(), sqldb); err != nil {
		log.Printf("RepairChargeSingletons: %v (non-fatal)", err)
	} else if n > 0 {
		log.Printf("[db] RepairChargeSingletons: fixed %d charges with singleton=1", n)
	}

	// Market background sweeps (order expiry, daily price snapshot) start after the session
	// registry exists; see startMarketSweeps below.

	// lscReg is the shared in-memory chat channel registry.
	// Created once in main and threaded down so all connections share it.
	lscReg := lsc.NewRegistry()
	globalLSCReg = lscReg

	// Gang registry for fleet/gang membership tracking.
	globalGangReg = gang.NewRegistry()
	globalTradeReg = trade.NewRegistry()

	// stationReg is the shared in-memory station guest registry.
	// Tracks character presence at stations for GetGuests.
	stationReg := station.NewStationRegistry()
	globalStationReg = stationReg
	// Push live guest-list updates so an open station lobby (lobby.py) shows
	// arrivals/departures without re-polling GetGuests. Each recipient's
	// packet carries their own accountID.
	stationReg.SetPresenceNotify(func(stationID, charID int64, arrived bool) {
		event := "OnCharNoLongerInStation"
		if arrived {
			event = "OnCharNowInStation"
		}
		args := marshal.PyTuple{Items: []marshal.PyRep{marshal.PyInt{Value: uint64(charID)}}}
		for _, guestID := range stationReg.Guests(stationID) {
			if guestID == charID {
				continue
			}
			if conn, acct := charConnLookup(guestID); conn != nil {
				_ = server.SendNotification(conn, acct, event, "charid", args)
			}
		}
	})

	// sysMgr is the shared solar system registry for multi-player visibility.
	sysMgr := destiny.NewSystemManager()
	globalSysMgr = sysMgr

	// sessReg is the shared session registry for the ESP admin panel and
	// charID→connection lookups (e.g. mission kill-complete notifications).
	// Created before npcMgr so the NPC death closure can capture it.
	sessReg := esp.NewSessionRegistry()
	globalSessReg = sessReg

	// npcMgr manages spawned NPC entities and their AI tick loop.
	// Declared early so the missile launch closure can capture it (for NotifyDamage).
	var npcMgr *npc.NPCManager
	npcMgr = npc.NewManager(sqldb,
		// Movement broadcast.
		func(npcEntity *destiny.Entity, ss *destiny.SolarSystem, action npc.Action) {
			// Build the SetSpeedFraction + movement pair once and send it as one batched packet; the
			// client must not see SetSpeedFraction alone.
			npcID := marshal.PyInt{Value: uint64(npcEntity.ID)}
			ssfAction := marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyString{Value: "SetSpeedFraction"},
				marshal.PyTuple{Items: []marshal.PyRep{npcID, marshal.PyReal{Value: 1.0}}},
			}}

			var moveAction marshal.PyRep
			switch action.Type {
			case npc.ActionFollow:
				moveAction = marshal.PyTuple{Items: []marshal.PyRep{
					marshal.PyString{Value: "FollowBall"},
					marshal.PyTuple{Items: []marshal.PyRep{
						npcID,
						marshal.PyInt{Value: uint64(action.TargetID)},
						marshal.PyReal{Value: action.Distance},
					}},
				}}
			case npc.ActionOrbit:
				moveAction = marshal.PyTuple{Items: []marshal.PyRep{
					marshal.PyString{Value: "Orbit"},
					marshal.PyTuple{Items: []marshal.PyRep{
						npcID,
						marshal.PyInt{Value: uint64(action.TargetID)},
						marshal.PyReal{Value: action.Distance},
					}},
				}}
			case npc.ActionGoto:
				npcPos := npcEntity.Pos()
				dir := action.Pos.Sub(npcPos).Normalize()
				far := npcPos.Add(dir.Scale(1e15))
				moveAction = marshal.PyTuple{Items: []marshal.PyRep{
					marshal.PyString{Value: "GotoDirection"},
					marshal.PyTuple{Items: []marshal.PyRep{
						npcID,
						marshal.PyReal{Value: far.X},
						marshal.PyReal{Value: far.Y},
						marshal.PyReal{Value: far.Z},
					}},
				}}
			case npc.ActionStop:
				ssfAction = marshal.PyTuple{Items: []marshal.PyRep{
					marshal.PyString{Value: "SetSpeedFraction"},
					marshal.PyTuple{Items: []marshal.PyRep{npcID, marshal.PyReal{Value: 0.0}}},
				}}
				moveAction = marshal.PyTuple{Items: []marshal.PyRep{
					marshal.PyString{Value: "Stop"},
					marshal.PyTuple{Items: []marshal.PyRep{npcID}},
				}}
			case npc.ActionWarp:
				// Warp-in: use dedicated warp notification instead of action list.
				// WarpDest is the NPC's landing point, so exitRange is 0.
				players := ss.GetBubbleConnectedPlayersExcept(npcEntity.ID)
				for _, p := range players {
					if !ensureBallsKnown(p, npcEntity) {
						continue
					}
					p.SendStamped(func(stamp uint32) error {
						return destiny.SendWarpTo(p.Conn, globalNodeID, p.AccountID,
							stamp, npcEntity.ID,
							action.WarpDest.X, action.WarpDest.Y, action.WarpDest.Z,
							0, destiny.WarpSpeedAUperSec)
					})
				}
				return // warp uses its own packet path, skip normal action list
			case npc.ActionWarpEnd:
				// Warp complete: stop warp FX, stop the ball, snap position, as the player SendWarpEnd does.
				players := ss.GetBubbleConnectedPlayersExcept(npcEntity.ID)
				for _, p := range players {
					if !ensureBallsKnown(p, npcEntity) {
						continue
					}
					p.SendStamped(func(stamp uint32) error {
						return destiny.SendWarpEnd(p.Conn, globalNodeID, p.AccountID,
							stamp, npcEntity.ID,
							action.WarpDest.X, action.WarpDest.Y, action.WarpDest.Z)
					})
				}
				return // warp-end uses its own packet path
			}

			// Resolve the follow/orbit target so the known-ball gate can deliver it with the NPC.
			// Celestial targets resolve to nil; they are always in the client's SetState.
			var moveTarget *destiny.Entity
			if action.Type == npc.ActionFollow || action.Type == npc.ActionOrbit {
				moveTarget = ss.GetEntity(action.TargetID)
			}

			// Only Stop carries a position snap. See docs/main-s1a-notes.md#npc-movement-broadcast.
			var posAction marshal.PyRep
			if action.Type == npc.ActionStop {
				// Extrapolated by posSnapLeadSecs: the client applies the packet after its stamp, so a
				// current-position snap lands stale.
				syncPos := npcEntity.PredictedPos(posSnapLeadSecs)
				posAction = marshal.PyTuple{Items: []marshal.PyRep{
					marshal.PyString{Value: "SetBallPosition"},
					marshal.PyTuple{Items: []marshal.PyRep{
						npcID,
						marshal.PyReal{Value: syncPos.X},
						marshal.PyReal{Value: syncPos.Y},
						marshal.PyReal{Value: syncPos.Z},
					}},
				}}
			}

			// Queued, not sent: several NPCs re-deciding in one AI tick must not each mint a stamp, and
			// the batch is not flushed here (destiny.SolarSystem.tickLoop flushes every 200 ms).
			// See docs/main-s1a-notes.md#npc-movement-broadcast.
			npcKey := strconv.FormatUint(uint64(npcEntity.ID), 10)
			players := ss.GetBubbleConnectedPlayersExcept(npcEntity.ID)
			for _, p := range players {
				if !ensureBallsKnown(p, npcEntity, moveTarget) {
					continue
				}
				// An idle leash waypoint is skipped for a witness a stamp or more ahead of the wall; the patrol
				// re-aims later. Queued entries for the ball are not purged.
				// See docs/main-s1a-notes.md#npc-movement-broadcast.
				if action.Idle && p.StampLead() >= 1 {
					continue
				}
				if posAction != nil {
					p.QueueAction("pos:"+npcKey, npcEntity.ID, posAction)
				}
				p.QueueAction("ssf:"+npcKey, npcEntity.ID, ssfAction)
				p.QueueAction("move:"+npcKey, npcEntity.ID, moveAction)
			}
		},
		// Combat broadcast: an NPC fires weapons at a target. See docs/main-s1-notes.md#npc-fire-fx.
		func(npcEntity *destiny.Entity, ss *destiny.SolarSystem, targetEntity *destiny.Entity, applied float64, destroyed bool, hitQuality float64, cycleTimeMs int64) {
			// Skip a target already destroyed by an earlier callback; the killing blow
			// (`destroyed`) falls through to the death handling at the bottom.
			if targetEntity.IsDestroyed() && !destroyed {
				return
			}
			// OnSpecialFX needs a moduleID and moduleTypeID to attach the FX to a turret
			// hardpoint. ID fields are ints (0 when absent), the duration is a real.
			var modID, modTypeID int64
			var chargeTypeID int64
			// NPC turret fire always uses effects.Laser: it is the guid tactical.py accepts for the
			// red box. See docs/main-s1-notes.md#npc-fire-fx.
			fxGUID := dogma.FXGUIDLaser
			if npcEntity.NPCTurretTypeID != 0 && len(npcEntity.Slim.Modules) > 0 {
				// EntityShip.FitHardpoints keys turrets by the entity's own ball ID, so moduleID
				// must equal it for StandardWeapon.Start to find the turret.
				modID = int64(npcEntity.ID)
				modTypeID = npcEntity.Slim.Modules[0].TypeID
				chargeTypeID = npcChargeTypeID(npcEntity.NPCTurretTypeID)
			}
			players := ss.GetBubbleConnectedPlayersExcept(npcEntity.ID)
			log.Printf("[npc-fx] entityID=%d turretTypeID=%d modules=%d fxGUID=%s modID=%d modTypeID=%d chargeType=%d players=%d applied=%.1f",
				npcEntity.ID, npcEntity.NPCTurretTypeID, len(npcEntity.Slim.Modules), fxGUID, modID, modTypeID, chargeTypeID, len(players), applied)

			// The weapon cycle time is the FX duration; 4 s when unknown.
			durationMs := cycleTimeMs
			if durationMs <= 0 {
				durationMs = 4000
			}
			startTime := server.Win32TimeNow()

			// The fire FX repeats client-side, so the start event goes out only when a burst
			// begins or the target changes. See docs/main-s1-notes.md#npc-fire-fx.
			needFX := npcEntity.NPCTurretTypeID != 0 && len(npcEntity.Slim.Modules) > 0
			if needFX {
				for _, ae := range npcEntity.GetActiveEffects() {
					if ae.ModuleID != modID {
						continue
					}
					if ae.TargetID == targetEntity.ID {
						needFX = false // burst already running against this target
					} else {
						// Target switched: stop the old burst first.
						stopNPCFireFX(npcEntity, ss, ae.TargetID)
					}
					break
				}
			}

			var fxAction marshal.PyRep
			if needFX {
				ae := destiny.ActiveEffect{
					EntityID:    npcEntity.ID,
					ModuleID:    modID,
					ModuleType:  modTypeID,
					TargetID:    targetEntity.ID,
					OtherType:   chargeTypeID,
					GUID:        fxGUID,
					IsOffensive: true,
					DurationMs:  durationMs,
					StartTime:   startTime,
				}
				fxAction = destiny.BuildEffectAction(ae)
				// Tracked so players arriving later get the burst replayed (SetState effectStates,
				// ensureBallsKnown).
				npcEntity.SetActiveEffect(modID, ae)
			}

			// Deliver the NPC ball on its first hit. ensureBallsKnown skips known balls;
			// re-sending one overwrites the client's live nav state.
			if targetEntity.IsPlayer && targetEntity.Conn != nil {
				ensureBallsKnown(targetEntity, npcEntity)
			}

			// SetState builds the red box from the aggressor list.
			if targetEntity.IsPlayer {
				targetEntity.AddAggressor(npcEntity.ID, time.Now().Unix())
			}

			// Combat message only while the target shares the bubble: a warping player has no
			// ballpark and OnDamageMessage crashes the client.
			dmgID := hitQualityID(hitQuality)
			// The message names the NPC's turret, else its hull type.
			msgWeaponType := int64(npcEntity.NPCTurretTypeID)
			if msgWeaponType == 0 {
				msgWeaponType = npcEntity.Slim.TypeID
			}
			if targetEntity.IsPlayer && targetEntity.Conn != nil &&
				targetEntity.BubbleID >= 0 && targetEntity.BubbleID == npcEntity.BubbleID &&
				!targetEntity.IsInWarp() && !targetEntity.IsUntargetable() {
				sendCombatMessage(targetEntity.Conn, int64(targetEntity.AccountID), attackMsgTaken[dmgID], marshal.PyDict{Items: []marshal.DictEntry{
					{Key: marshal.PyString{Value: "source"}, Value: marshal.PyInt{Value: uint64(npcEntity.ID)}},
					{Key: marshal.PyString{Value: "weapon"}, Value: marshal.PyInt{Value: uint64(msgWeaponType)}},
					{Key: marshal.PyString{Value: "target"}, Value: marshal.PyInt{Value: uint64(targetEntity.ID)}},
					{Key: marshal.PyString{Value: "damage"}, Value: marshal.PyReal{Value: applied}},
				}})
			}

			// A miss sends only the FX, and only for the burst's first shot; the repeating
			// client effect covers the rest.
			if applied == 0 {
				if fxAction == nil {
					return
				}
				for _, p := range players {
					if p.IsInWarp() || p.IsUntargetable() {
						continue
					}
					if !ensureBallsKnown(p, npcEntity, targetEntity) {
						continue
					}
					p.SendFXStamped(func(stamp uint32) error {
						return destiny.SendActionList(p.Conn, globalNodeID, p.AccountID, stamp, fxAction)
					})
				}
				return
			}

			dmgState := targetEntity.DamageStateFrac()
			dmgState.Timestamp = server.Win32TimeNow()
			dmgAction := destiny.BuildSimpleAction("OnDamageStateChange",
				marshal.PyInt{Value: uint64(targetEntity.ID)},
				destiny.EncodeDamageState(dmgState),
			)
			for _, p := range players {
				if p.IsInWarp() || p.IsUntargetable() {
					continue
				}
				if !ensureBallsKnown(p, npcEntity, targetEntity) {
					continue
				}
				// The FX is sent directly to keep its order against stopNPCFireFX. Damage state is
				// queued (absolute, last wins per ball), so a volley costs one action per witness per tick.
				if fxAction != nil {
					p.SendFXStamped(func(stamp uint32) error {
						return destiny.SendActionList(p.Conn, globalNodeID, p.AccountID, stamp, fxAction)
					})
				}
				p.QueueFXAction("dmg:"+strconv.FormatUint(uint64(targetEntity.ID), 10), targetEntity.ID, dmgAction)
			}

			sendPlayerHPUpdate(targetEntity)

			if destroyed {
				if targetEntity.IsPlayer && targetEntity.Conn != nil && targetEntity.PlayerState != nil {
					queuePlayerDeath(targetEntity.PlayerState.(*destinyConn), targetEntity.Slim.TypeID == 670)
				} else if globalDroneMgr != nil && globalDroneMgr.HandleDestroyed(targetEntity.ID, npcEntity.ID) {
					// Player drone: the drone manager handles it.
				} else {
					npcDestroyEntity(ss, targetEntity, npcEntity.ID)
				}
			}
		},
		// Lock callback: an NPC acquired ("add") or dropped ("lost") a lock on a target.
		func(npcEntity *destiny.Entity, ss *destiny.SolarSystem, targetEntity *destiny.Entity, event string) {
			// Skip when either side was destroyed after the NPC manager released its lock.
			if npcEntity.IsDestroyed() || targetEntity.IsDestroyed() {
				return
			}
			if !targetEntity.IsPlayer || targetEntity.Conn == nil {
				// event tells the two directions of this message apart in the log.
				log.Printf("[npc] lock broadcast skipped: event=%s targetID=%d isPlayer=%v connNil=%v",
					event,
					targetEntity.ID, targetEntity.IsPlayer, targetEntity.Conn == nil)
				return
			}
			// "otheradd" shows the player's targeting UI that an NPC has them locked;
			// "otherlost" clears it.
			npcEvent := "otheradd"
			if event == "lost" {
				npcEvent = "otherlost"
				// Stop the fire FX so the client clears the red box; the aggressor entry only
				// affects later SetState payloads.
				stopNPCFireFX(npcEntity, ss, targetEntity.ID)
				targetEntity.ClearAggressor(npcEntity.ID)
			} else {
				// A yellow box on a ball the client does not hold is an orphaned overview entry,
				// so deliver the NPC first.
				ensureBallsKnown(targetEntity, npcEntity)
			}
			log.Printf("[npc] lock broadcast: npcID=%d → playerID=%d event=%s", npcEntity.ID, targetEntity.ID, npcEvent)
			onTargetEvent := marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyString{Value: "OnTarget"},
				marshal.PyString{Value: npcEvent},
				marshal.PyInt{Value: uint64(npcEntity.ID)},
				marshal.PyNone{},
			}}
			multiArgs := marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyList{Items: []marshal.PyRep{onTargetEvent}},
			}}
			if err := server.SendNotification(targetEntity.Conn, int64(targetEntity.AccountID), "OnMultiEvent", "clientID", multiArgs); err != nil {
				log.Printf("[npc] lock broadcast OnTarget to player %d: %v", targetEntity.ID, err)
			}
		},
		// Death callback: the NPC was destroyed by killerEntityID, bounty is its prize.
		func(npcEntity *destiny.Entity, ss *destiny.SolarSystem, killerEntityID uint32, bounty float64) {

			entityID := npcEntity.ID

			// Starmap overlay kill count.
			go func() {
				defer safego.Recover("main")
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				evedb.RecordMapEvent(ctx, sqldb, ss.ID, evedb.StatFactionKill, 1)
			}()

			// RemoveBalls does not clear the client's red box, so stop any active fire FX
			// first.
			for _, ae := range npcEntity.GetActiveEffects() {
				stopNPCFireFX(npcEntity, ss, ae.TargetID)
			}

			// Bounty and security status go to the killer, minus corp tax. The security award
			// runs even at bounty 0: CONCORD and sentries carry none. See docs/main-s1-notes.md#npc-death-callback.
			if killerEntityID != 0 {
				if killerEntity := ss.GetEntity(killerEntityID); killerEntity != nil && killerEntity.IsPlayer {
					killerCharID := killerEntity.Slim.CharID
					if killerCharID > 0 {
						ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
						defer cancel()

						var newBal float64
						if bounty > 0 {
							killerCorpID := killerEntity.Slim.CorpID
							taxRate := evedb.GetCorpTaxRate(ctx, sqldb, killerCorpID)
							playerShare := bounty * (1 - taxRate)
							corpShare := bounty * taxRate

							if err := evedb.CreditBounty(ctx, sqldb, killerCharID, playerShare); err != nil {
								log.Printf("[npc] CreditBounty charID=%d amount=%.1f: %v", killerCharID, playerShare, err)
							}
							if corpShare > 0 {
								if err := evedb.CreditCorpWallet(ctx, sqldb, killerCorpID, corpShare); err != nil {
									log.Printf("[npc] CreditCorpWallet corpID=%d amount=%.1f: %v", killerCorpID, corpShare, err)
								}
							}

							// Client format: "<ownerID2> got bounty prize for killing <argID1>"; a negative
							// argID1 is an invTypes lookup for the NPC name.
							payerID := npcEntity.Slim.WarFactionID
							if payerID == 0 {
								payerID = 1000125 // CONCORD
							}
							killedArg := -npcEntity.Slim.TypeID
							newBal, _ = evedb.GetCharacterBalance(ctx, sqldb, killerCharID)
							if err := evedb.InsertBountyJournal(ctx, sqldb, killerCharID, payerID, killerCharID, killedArg, playerShare, newBal); err != nil {
								log.Printf("[npc] InsertBountyJournal charID=%d: %v", killerCharID, err)
							}
							if corpShare > 0 {
								corpBal, _ := evedb.GetCorpBalance(ctx, sqldb, killerCorpID)
								if err := evedb.InsertBountyJournal(ctx, sqldb, killerCorpID, payerID, killerCorpID, killedArg, corpShare, corpBal); err != nil {
									log.Printf("[npc] InsertBountyJournal corpID=%d: %v", killerCorpID, err)
								}
							}

							log.Printf("[npc] bounty %.1f ISK to charID=%d (tax=%.0f%% corp=%d corpShare=%.1f)",
								playerShare, killerCharID, taxRate*100, killerCorpID, corpShare)
						}

						// Security status: a pirate pays its entitySecurityStatusKillBonus scaled by the
						// killer's distance from a perfect record; the police cost a penalty.
						awardOrPenaliseNPCKill(ctx, sqldb, killerEntity, killerCharID,
							npcEntity, ss, bounty)

						// World event for modules (db/worldevents.go), emitted while the NPC record still
						// holds its mission and open-site tags.
						if evedb.WorldEventsWanted() {
							npcFaction := npcEntity.Slim.WarFactionID
							if npcFaction == npc.FactionUnknown {
								npcFaction = 0
							}
							evedb.EmitWorldEvent(evedb.NPCKilledEvent{
								At:               time.Now(),
								KillerCharID:     killerCharID,
								KillerCorpID:     killerEntity.Slim.CorpID,
								KillerAllianceID: killerEntity.Slim.AllianceID,
								NPCTypeID:        npcEntity.Slim.TypeID,
								NPCGroupID:       npcEntity.Slim.GroupID,
								NPCFactionID:     npcFaction,
								SolarSystemID:    ss.ID,
								Bounty:           bounty,
								OpenSiteID:       npcMgr.GetOpenSiteID(entityID),
								MissionID:        npcMgr.GetMissionID(entityID),
								Structure:        npc.IsDestructibleStructure(npcEntity.Slim.GroupID),
							})
						}

						if bounty > 0 {
							// Standing loss against the NPC's faction; the Unknown faction has no standings.
							// rates.ratStandingLoss scales it and 0 turns it off.
							if factionID := npcEntity.Slim.WarFactionID; factionID != 0 && factionID != npc.FactionUnknown && rates.RatStandingLoss > 0 {
								delta := npcStandingDelta(npcEntity.Slim.GroupID) * rates.RatStandingLoss
								newStanding, err := evedb.UpsertNPCStanding(ctx, sqldb, killerCharID, factionID, delta)
								if err != nil {
									log.Printf("[npc] UpsertNPCStanding charID=%d faction=%d: %v", killerCharID, factionID, err)
								} else {
									killerEntity.SetNPCStanding(factionID, newStanding)
									log.Printf("[npc] standing charID=%d faction=%d delta=%.3f new=%.3f",
										killerCharID, factionID, delta, newStanding)
									if killerEntity.Conn != nil {
										sendStandingNotification(killerEntity.Conn, int64(killerEntity.AccountID), factionID, killerCharID, newStanding-delta, newStanding)
									}
								}
							}

							if killerEntity.Conn != nil {
								notifArgs := marshal.PyTuple{Items: []marshal.PyRep{
									marshal.PyString{Value: "cash"},
									marshal.PyInt{Value: uint64(killerCharID)},
									marshal.PyReal{Value: newBal},
								}}
								_ = server.SendNotification(killerEntity.Conn, int64(killerEntity.AccountID), "OnAccountChange", "charid", notifArgs)
							}
						}
					}
				}
			}

			// Mission kill tracking for NPCs that belong to a mission.
			if missionID := npcMgr.GetMissionID(entityID); missionID > 0 {
				go func(mid int64, eid uint32, typeID, groupID int64) {
					defer safego.Recover("main")
					mctx, mcancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer mcancel()
					m, merr := evedb.GetMissionByID(mctx, sqldb, mid)
					if merr != nil || m == nil {
						return
					}
					tmpl, terr := evedb.GetTemplateByID(mctx, sqldb, m.TemplateID)
					if terr != nil || tmpl == nil {
						return
					}
					// Structures carry the mission tag too: killAll ignores them, a targeted objective
					// counts only tmpl.TargetTypeID. See docs/main-s1-notes.md#mission-kill-tally.
					if tmpl.CompletionType == "killAll" {
						if npc.IsDestructibleStructure(groupID) {
							return
						}
					} else if tmpl.TargetTypeID > 0 && typeID != tmpl.TargetTypeID {
						return
					}
					progress, required, changed, err := evedb.UpdateKillProgress(mctx, sqldb, mid, 1)
					if err != nil {
						log.Printf("[npc] mission kill tracking missionID=%d entityID=%d: %v", mid, eid, err)
						return
					}
					if !changed {
						// Objective already met: kills past the goal are not logged or sent.
						return
					}
					log.Printf("[npc] mission kill missionID=%d entityID=%d typeID=%d progress=%d/%d", mid, eid, typeID, progress, required)
					// Objective complete: tell the client so its journal refreshes. Any `what` outside
					// the client's destroy lists refreshes open windows (OnAgentMissionChange).
					if required > 0 && progress == required {
						// killAll completes in completeMissionSiteIfClear; its tally is display only and can
						// over-count when a site respawns.
						if tmpl.CompletionType == "killAll" {
							return
						}
						if _, cerr := evedb.CompleteKillObjective(mctx, sqldb, mid); cerr != nil {
							log.Printf("[npc] mission %d objective flag: %v", mid, cerr)
						}
						if sess := sessReg.FindByCharID(m.CharID); sess != nil && sess.Conn != nil {
							notifyAgentMissionChange(sess.Conn, sess.AccountID, "progress", m.AgentID)
							log.Printf("[npc] mission %d objective complete → OnAgentMissionChange(progress) charID=%d agentID=%d",
								mid, m.CharID, m.AgentID)
						}
					}
				}(missionID, entityID, npcEntity.Slim.TypeID, npcEntity.Slim.GroupID)
			}

			// Dungeon triggers (npcKilled, roomCleared) go to the connection whose loaded room
			// owns this ball: the killer's first, else any bubble player holding it.
			if dunObjID := npcMgr.GetDunObjectID(entityID); dunObjID > 0 {
				// A mission-dungeon NPC killed this run is skipped when the room reloads (warp back
				// to the site).
				if mid := npcMgr.GetMissionID(entityID); mid > 0 {
					dunSiteFor(mid, true).MarkKilled(dunObjID)
				}
				// An open site records the kill for every visitor and re-checks whether it is
				// clear once the death triggers below have queued their waves.
				var reevalSiteID int64
				if sid := npcMgr.GetOpenSiteID(entityID); sid > 0 {
					if s := openSiteByID(sid); s != nil {
						s.state.MarkKilled(dunObjID)
						if s.fixed {
							// A fixed dungeon never clears; the kill books a respawn instead.
							fixedSiteKilled(s, dunObjID)
						} else {
							reevalSiteID = sid
						}
					}
				}
				var ownerDS *destinyConn
				if killerEntityID != 0 {
					if ke := ss.GetEntity(killerEntityID); ke != nil && ke.IsPlayer {
						if kds, ok := ke.PlayerState.(*destinyConn); ok && kds != nil {
							if kds.dunObjOwns(entityID) {
								ownerDS = kds
							}
						}
					}
				}
				if ownerDS == nil {
					for _, p := range ss.GetAllPlayersInSystem() {
						if pds, ok := p.PlayerState.(*destinyConn); ok && pds != nil {
							if pds.dunObjOwns(entityID) {
								ownerDS = pds
								break
							}
						}
					}
				}
				if ownerDS != nil {
					fireDungeonNPCDeathTriggers(ownerDS, npcMgr, entityID, dunObjID)
				}
				if reevalSiteID > 0 {
					safego.Go("reevaluateOpenSite", func() { reevaluateOpenSite(reevalSiteID) })
				}
				if ownerDS != nil {

					// killAll site clear: if this was the room's last living NPC, check off-path whether
					// the site is empty (no held waves, no deeper rooms) and complete the objective.
					if mid := npcMgr.GetMissionID(entityID); mid > 0 {
						ownerObjs := ownerDS.dunObjSnapshot()
						lastAlive := true
						for eid := range ownerObjs {
							if eid == entityID || !npcMgr.IsNPC(eid) {
								continue
							}
							// Destructible structures never block a site clear.
							if e := ss.GetEntity(eid); e != nil && npc.IsDestructibleStructure(e.Slim.GroupID) {
								continue
							}
							lastAlive = false
							break
						}
						if lastAlive {
							spawned := make(map[int64]bool, len(ownerObjs))
							for _, oid := range ownerObjs {
								spawned[oid] = true
							}
							go completeMissionSiteIfClear(sqldb, sessReg, mid, ownerDS.dunRoomID, spawned)
						}
					}
				}
			}

			// Loot is rolled and spawned asynchronously at the death position, captured before
			// RemoveEntity clears the ball. The can belongs to the killing player.
			npcDeathPos := npcEntity.Pos()
			// The death point is carried forward by the killer's stream lead so the wreck and
			// explosion land where the pilot saw the ship die. See docs/destiny-stamps.md#death-snap-lead.
			if killerEntityID != 0 {
				if ke := ss.GetEntity(killerEntityID); ke != nil && ke.IsPlayer && ke.Conn != nil {
					if excess := ke.NextStampExcess(); excess > 0 {
						if excess > maxDeathSnapLeadSecs {
							excess = maxDeathSnapLeadSecs
						}
						predicted := npcEntity.SimulateAheadPos(float64(excess), func(id uint32, t float64) (destiny.Vec3, bool) {
							te := ss.GetEntity(id)
							if te == nil {
								return destiny.Vec3{}, false
							}
							return te.PredictedPos(t), true
						})
						log.Printf("[npc-death] entity=%d stream %d stamp(s) past wall — death point carried %.0f m forward",
							entityID, excess, predicted.Sub(npcDeathPos).Len())
						npcDeathPos = predicted
					}
				}
			}
			killedRole := npcMgr.GetSpawnRole(entityID)
			lootOwnerID := npcEntity.Slim.OwnerID
			if killerEntityID != 0 {
				if ke := ss.GetEntity(killerEntityID); ke != nil && ke.IsPlayer && ke.Slim.CharID > 0 {
					lootOwnerID = ke.Slim.CharID
				}
			}
			// Server-side killer-to-victim range at death, to read against the range the pilot
			// was shooting at; a gap between them is client/server drift.
			if killerEntityID != 0 {
				if ke := ss.GetEntity(killerEntityID); ke != nil {
					kPos := ke.Pos()
					log.Printf("[npc-death] entity=%d type=%d deathPos=(%.0f,%.0f,%.0f) killer=%d killerPos=(%.0f,%.0f,%.0f) serverRange=%.0f m",
						entityID, npcEntity.Slim.TypeID,
						npcDeathPos.X, npcDeathPos.Y, npcDeathPos.Z,
						killerEntityID, kPos.X, kPos.Y, kPos.Z,
						npcDeathPos.Sub(kPos).Len())
				}
			}
			if npcCargoOpened(entityID) {
				// Opened in place before it died: what is left inside is the
				// drop (npccargo.go), never a second roll.
				go npcCargoToDeathCan(sqldb, ss, entityID, lootOwnerID, npcDeathPos)
			} else {
				go spawnNPCLoot(sqldb, ss, npcEntity.Slim.TypeID, npcEntity.Slim.GroupID,
					lootOwnerID, npcDeathPos, killedRole, npcMgr.GetDunObjectID(entityID))
			}

			// Remove the entity and send each witness one atomic Stop, position rewrite,
			// TerminalExplosion and RemoveBalls batch. See docs/destiny-space.md#terminal-remove.
			remaining := ss.RemoveEntity(entityID)
			for _, p := range remaining {
				if p.Conn == nil || !p.KnowsBall(entityID) {
					continue
				}
				// A witness whose nav target is this ball gets a leading Stop on the same stamp, sent urgent.
				// See docs/main-s1-notes.md#npc-removal-nav-reference.
				var leading []marshal.PyRep
				if p.Manager != nil && p.Manager.NavTargetID() == entityID {
					leading = append(leading, destiny.BuildSimpleAction("Stop", marshal.PyInt{Value: uint64(p.ID)}))
				}
				p.SendStampedUrgent(func(stamp uint32) error {
					return destiny.SendTerminalRemoveStop(p.Conn, globalNodeID, p.AccountID, stamp, entityID, npcDeathPos, npcEntity.BallDesc(), leading...)
				})
				p.ForgetBalls(entityID)
			}

			// RemoveEntity already dropped every pilot's lock on it (targetpurge.go).
			log.Printf("[npc] entity %d destroyed and removed from system %d (killer=%d bounty=%.0f)", entityID, ss.ID, killerEntityID, bounty)
		},
		// Missile callback: an NPC fires a volley at the target; impact is delayed.
		func(npcEntity *destiny.Entity, ss *destiny.SolarSystem, targetEntity *destiny.Entity, wp *evedb.NPCWeaponProfile, falloffFactor float64) {
			npcPos := npcEntity.Pos()
			targetPos := targetEntity.Pos()
			dist := targetPos.Sub(npcPos).Len()

			// Flight parameters from the missile type.
			velocity := 3000.0
			maxFlightTime := 15.0
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			mfp, mfpErr := evedb.GetMissileFlightParams(ctx, sqldb, wp.MissileTypeID)
			cancel()
			if mfpErr == nil && mfp != nil {
				velocity = mfp.Velocity
				maxFlightTime = mfp.MaxFlightTime
			}

			// Log-only estimate; impact timing comes from the flight sim in the goroutine
			// below.
			flightTime := dist / velocity
			if flightTime > maxFlightTime {
				flightTime = maxFlightTime
			}
			if flightTime < 0.5 {
				flightTime = 0.5
			}

			// One ball per launcher hardpoint (wp.TurretSlots). Damage is one roll per volley:
			// slot 0 carries the impact sim, the other balls are cosmetic.
			slots := wp.TurretSlots
			if slots < 1 {
				slots = 1
			}
			if slots > 8 {
				slots = 8
			}
			npcRadius := npcEntity.BallDesc().Radius
			npcCorpID := uint32(npcEntity.Slim.CorpID)

			// GroupID and CategoryID map the balls to spaceObject.Missile; one lookup per
			// volley.
			ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
			gID, cID, gcErr := evedb.GetTypeGroupCategory(ctx2, sqldb, wp.MissileTypeID)
			cancel2()
			if gcErr != nil {
				log.Printf("[npc] missile launch: GetTypeGroupCategory(%d) failed: %v", wp.MissileTypeID, gcErr)
			}

			// The launch FX red-boxes the victim (effects.MissileDeployment is accepted by
			// tactical.py). One start event per burst and target; stopNPCFireFX sends the stop.
			needFX := true
			for _, ae := range npcEntity.GetActiveEffects() {
				if ae.ModuleID != int64(npcEntity.ID) {
					continue
				}
				if ae.TargetID == targetEntity.ID {
					needFX = false
				} else {
					stopNPCFireFX(npcEntity, ss, ae.TargetID)
				}
				break
			}
			var fxAction marshal.PyRep
			if needFX {
				ae := destiny.ActiveEffect{
					EntityID:    npcEntity.ID,
					ModuleID:    int64(npcEntity.ID), // EntityShip keys modules by ball ID
					TargetID:    targetEntity.ID,
					OtherType:   wp.MissileTypeID,
					GUID:        dogma.FXGUIDMissileDeployment,
					IsOffensive: true,
					DurationMs:  wp.CycleTimeMs,
					StartTime:   server.Win32TimeNow(),
				}
				fxAction = destiny.BuildEffectAction(ae)
				npcEntity.SetActiveEffect(int64(npcEntity.ID), ae)
			}
			if targetEntity.IsPlayer {
				targetEntity.AddAggressor(npcEntity.ID, time.Now().Unix())
			}

			// No SetBallPosition for the NPC: the server Step() position diverges from the
			// client's orbit interpolation and snapping it each cycle shows as skipping.
			players := ss.GetBubbleConnectedPlayersExcept(npcEntity.ID)
			// Recipients are fixed at launch so removal matches: no RemoveBalls for players who
			// left or for missiles a new arrival never saw.
			launchPlayers := players

			// The launch FX is its own packet, sent with slot 0 only, so it is neither queued
			// behind nor purged with a missile ball.
			fxRecipient := func(p *destiny.Entity) {
				if fxAction == nil {
					return
				}
				p.SendFXStamped(func(stamp uint32) error {
					return destiny.SendActionList(p.Conn, globalNodeID, p.AccountID, stamp, fxAction)
				})
			}

			// launchOne spawns slot N's ball at its hardpoint offset and
			// schedules its removal; returns what the damage sim runs on.
			launchOne := func(slot int) (spawnPos destiny.Vec3, ballFlight float64, missileID uint32, fizzle bool) {
				spawnPos = npcPos.Add(missileHardpointOffset(npcPos, targetPos, npcRadius, slot))
				delta := targetPos.Sub(spawnPos)
				sdist := delta.Len()
				ballSpeed, ballFlight, removeAfter, fizzle := missileTiming(sdist, velocity, maxFlightTime)
				// Close shots get a lower maxVelocity at launch, never a Stop.
				// See docs/missile-timing.md#three-client-constraints.
				var vel destiny.Vec3
				if sdist > 0 {
					vel = delta.Scale(ballSpeed / sdist)
				}
				missileID = ss.NextMissileID()
				ball := destiny.BallDesc{
					EntityID:      missileID,
					Mode:          destiny.ModeMissile,
					SubType:       0x01,
					Pos:           spawnPos,
					Radius:        20,
					Mass:          1.0,
					CorpID:        npcCorpID,
					MaxSpeed:      ballSpeed,
					Vel:           vel,
					Agility:       1.0,
					SpeedFraction: 1.0,
					FollowID:      targetEntity.ID,
					FollowRange:   0,
				}
				slim := destiny.SlimItem{
					BallID:       int64(missileID),
					ItemID:       int64(missileID),
					TypeID:       wp.MissileTypeID,
					OwnerID:      npcEntity.Slim.CorpID,
					CorpID:       npcEntity.Slim.CorpID,
					WarFactionID: npcEntity.Slim.WarFactionID,
					GroupID:      gID,
					CategoryID:   cID,
				}
				dmg := map[uint32]destiny.DamageState{
					missileID: {Shield: 1.0, Tau: 1e9, Armor: 1.0, Structure: 1.0},
				}
				if combatDebug {
					log.Printf("[npc] missile launch: entityID=%d missileID=%d slot=%d missileType=%d → target=%d dist=%.0f flight=%.1fs (nominal %.1fs) ballVel=%.0f remove=+%.1fs",
						npcEntity.ID, missileID, slot, wp.MissileTypeID, targetEntity.ID, sdist, ballFlight, flightTime, ballSpeed, removeAfter.Seconds())
				}
				var pr func(*destiny.Entity)
				if slot == 0 {
					pr = fxRecipient
				}
				queueMissileLaunch(players, npcEntity, targetEntity, targetEntity.ID, ball, slim, dmg, pr)
				// The removal draws the flight (the client's collision curve) and runs on its own
				// clock.
				scheduleMissileRemove(launchPlayers, missileID, true, removeAfter)
				return spawnPos, ballFlight, missileID, fizzle
			}

			// Slot 0 fires now and carries the volley's damage timing; the
			// other tubes fire on their stagger, despawn-aware.
			volleySpawn, ballFlight, volleyMissileID, volleyFizzle := launchOne(0)
			for slot := 1; slot < slots; slot++ {
				slot := slot
				go func() {
					defer safego.Recover("main")
					time.Sleep(missileSlotStagger(slot))
					if npcEntity.IsDestroyed() || targetEntity.IsDestroyed() || ss.GetEntity(targetEntity.ID) == nil {
						return
					}
					launchOne(slot)
				}()
			}

			targetID := targetEntity.ID
			npcEntityID := npcEntity.ID
			emDmg := wp.Em * wp.Multiplier * falloffFactor * rates.NPCDamage
			expDmg := wp.Explosive * wp.Multiplier * falloffFactor * rates.NPCDamage
			kinDmg := wp.Kinetic * wp.Multiplier * falloffFactor * rates.NPCDamage
			thmDmg := wp.Thermal * wp.Multiplier * falloffFactor * rates.NPCDamage

			if volleyFizzle {
				// Out of range: the client detonates this volley in open space, so there is no chase
				// and no damage. See docs/missile-timing.md#out-of-range.
				if combatDebug {
					log.Printf("[npc] missile out of range: entityID=%d missileID=%d → target=%d dist=%.0f range=%.0f",
						npcEntity.ID, volleyMissileID, targetID, dist, velocity*maxFlightTime)
				}
				return
			}

			npcLaunchedAt := time.Now()
			go func() {
				defer safego.Recover("main")
				// Real charge velocity for the chase, as on the player path.
				tgt := waitForMissileImpact(ss, volleySpawn, velocity, maxFlightTime, targetID)
				if tgt == nil {
					// Target gone, warped off or out of fuel: no damage.
					return
				}
				if wait := time.Duration(ballFlight*float64(time.Second)) - time.Since(npcLaunchedAt); wait > 0 {
					time.Sleep(wait)
				}
				if ss.GetEntity(targetID) != tgt {
					return
				}

				if tgt.HasHP() {
					// Full damage: the sig/velocity explosion reduction is a later-era mechanic that
					// this build does not have.
					applied, destroyed := tgt.ApplyDamage(emDmg, expDmg, kinDmg, thmDmg)
					if combatDebug {
						log.Printf("[npc] missile impact: missileID=%d dealt %.1f dmg to %d (destroyed=%v)",
							volleyMissileID, applied, targetID, destroyed)
					}

					if tgt.IsPlayer {
						tgt.RecordKillHit(npcEntity, wp.MissileTypeID, applied, destroyed)
					}

					// Combat message only outside warp and landing grace: a player with no ballpark
					// crashes the client on OnDamageMessage.
					if tgt.IsPlayer && tgt.Conn != nil &&
						tgt.BubbleID >= 0 && !tgt.IsInWarp() && !tgt.IsUntargetable() {
						sendCombatMessage(tgt.Conn, int64(tgt.AccountID), attackMsgTaken[missileDamageID(1.0)], marshal.PyDict{Items: []marshal.DictEntry{
							{Key: marshal.PyString{Value: "source"}, Value: marshal.PyInt{Value: uint64(npcEntityID)}},
							{Key: marshal.PyString{Value: "weapon"}, Value: marshal.PyInt{Value: uint64(wp.MissileTypeID)}},
							{Key: marshal.PyString{Value: "target"}, Value: marshal.PyInt{Value: uint64(targetID)}},
							{Key: marshal.PyString{Value: "damage"}, Value: marshal.PyReal{Value: applied}},
						}})
					}

					// NPC-vs-NPC: feed the target's threat table.
					if !tgt.IsPlayer && applied > 0 {
						npcMgr.NotifyDamage(targetID, npcEntityID, applied)
						npcMgr.SyncHPToDB(targetID)
					}

					// Damage state to the players who saw the launch.
					dmgState := tgt.DamageStateFrac()
					dmgState.Timestamp = server.Win32TimeNow()
					for _, p := range launchPlayers {
						if p.Conn == nil || !p.KnowsBall(targetID) {
							continue
						}
						if p.IsInWarp() || p.IsUntargetable() {
							continue
						}
						p.SendFXStamped(func(stamp uint32) error {
							return destiny.SendDamageStateChange(p.Conn, globalNodeID, p.AccountID, stamp, targetID, dmgState)
						})
					}
					sendPlayerHPUpdate(tgt)

					if destroyed {
						if tgt.IsPlayer && tgt.Conn != nil && tgt.PlayerState != nil {
							queuePlayerDeath(tgt.PlayerState.(*destinyConn), tgt.Slim.TypeID == 670)
						} else if globalDroneMgr != nil && globalDroneMgr.HandleDestroyed(tgt.ID, npcEntityID) {
							// Player drone: the drone manager handles it.
						} else {
							npcDestroyEntity(ss, tgt, npcEntityID)
						}
					}
				}

			}()
		},
		// Position persist: stores a moving NPC's live position for system reloads. Sends nothing
		// to clients, since SetBallPosition ends their orbit. See docs/main-s1-notes.md#npc-position-persist.
		func(npcEntity *destiny.Entity, ss *destiny.SolarSystem, targets []*destiny.Entity) {
			pos := npcEntity.Pos()
			// Log the server-scene distance to the first target, to compare with what players
			// report.
			if len(targets) > 0 {
				tp := targets[0].Pos()
				log.Printf("[npc-pos] npc=%d pos=(%.0f,%.0f,%.0f) dist-to-target=%.0f",
					npcEntity.ID, pos.X, pos.Y, pos.Z, pos.Sub(tp).Len())
			}
			if sqldb != nil {
				go func(eid uint32, x, y, z float64) {
					defer safego.Recover("main")
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					if err := evedb.UpdateLiveNPCPosition(ctx, sqldb, eid, x, y, z); err != nil {
						log.Printf("[npc] UpdateLiveNPCPosition entityID=%d: %v", eid, err)
					}
				}(npcEntity.ID, pos.X, pos.Y, pos.Z)
			}
		},
		// Spawn notify: deliver the new NPC to nearby players. Players without a ballpark
		// are skipped (their SetState includes bubble NPCs); a full rebuild would reset client state.
		func(npcEntity *destiny.Entity, ss *destiny.SolarSystem) {
			players := ss.GetBubbleConnectedPlayersExcept(npcEntity.ID)
			for _, p := range players {
				ensureBallsKnown(p, npcEntity)
			}
		},
		// Stop fire: the NPC ceased firing (idle or out of range); clear the FX and red box.
		func(npcEntity *destiny.Entity, ss *destiny.SolarSystem, targetID uint32) {
			stopNPCFireFX(npcEntity, ss, targetID)
		},
	)

	// Despawn callback: an NPC left the grid under its own power (a hauler that warped out).
	// Does what death does about the ball and nothing about the kill: no explosion, bounty or wreck.
	npcMgr.SetDespawnBroadcast(func(npcEntity *destiny.Entity, ss *destiny.SolarSystem) {
		if ss == nil {
			return
		}
		entityID := npcEntity.ID

		// RemoveBalls does not clear the client's red box, so stop any fire FX the NPC was
		// still playing.
		for _, ae := range npcEntity.GetActiveEffects() {
			stopNPCFireFX(npcEntity, ss, ae.TargetID)
		}

		remaining := ss.RemoveEntity(entityID)
		for _, p := range remaining {
			if p.Conn == nil || !p.KnowsBall(entityID) {
				continue
			}
			// Same nav-reference rule as the death path. No position snap: there is no
			// explosion to mask one and the ball is in warp.
			if p.Manager != nil && p.Manager.NavTargetID() == entityID {
				p.SendStampedUrgent(func(stamp uint32) error {
					return destiny.SendSimpleAction(p.Conn, globalNodeID, p.AccountID,
						stamp, "Stop", marshal.PyInt{Value: uint64(p.ID)})
				})
			}
			p.SendStampedUrgent(func(stamp uint32) error {
				return destiny.SendRemoveBalls(p.Conn, globalNodeID, p.AccountID,
					stamp, []uint32{entityID})
			})
			p.ForgetBalls(entityID)
		}

		// RemoveEntity already dropped every pilot's lock on it (targetpurge.go).
		log.Printf("[npc] entity %d left system %d under its own power (despawned)", entityID, ss.ID)
	})

	// Electronic warfare: the npc package rolls each module's entity*Chance
	// attribute and decides when it fires; these hooks land it on the victim.
	npcMgr.SetEwarCallbacks(
		func(npcEntity *destiny.Entity, ss *destiny.SolarSystem, target *destiny.Entity, mod evedb.NPCEwarModule) {
			applyNPCEwar(npcMgr, npcEntity, ss, target, mod)
		},
		func(npcEntity *destiny.Entity, ss *destiny.SolarSystem, target *destiny.Entity, kind string) {
			clearNPCEwar(npcEntity, ss, target, kind)
		},
		func(npcEntity *destiny.Entity, ss *destiny.SolarSystem, target *destiny.Entity, kind string, holdMs int64) {
			refreshNPCEwarJamBar(npcEntity, ss, target, kind, holdMs)
		},
	)

	npcMgr.Standings = npc.LoadStandings(context.Background(), sqldb)

	// The spawner manager is told of NPC deaths through npcMgr.SpawnerMgr.
	npcMgr.SpawnerMgr = spawnerMgr
	globalNPCMgr = npcMgr
	spawnerMgr.StartTickLoop(npcMgr, sqldb)

	// Load the live spawner-placed open sites and start the per-minute spawner pass
	// (opensites.go).
	initOpenSites(sqldb, tickMgr)

	globalDroneMgr = initDroneManager(sqldb, npcMgr)

	// Set here because esp cannot import slash (import cycle).
	esp.SlashExecute = func(ctx context.Context, db *sql.DB, cmdLine string) (string, error) {
		return slash.Execute(ctx, db, cmdLine)
	}

	// esp.SkillQueueNotify is called when an ESP page advances a logged-in character's
	// queue; it pushes OnSkillTrained/OnSkillStartTraining and arms the completion timer.
	esp.SkillQueueNotify = func(sess *esp.Session, adv *evedb.SkillQueueAdvance) {
		if sess == nil || sess.Conn == nil || adv == nil {
			return
		}
		notifySkillQueueAdvance(sess.Conn, sess.AccountID, sess.CharID, adv, sqldb)
	}

	// esp.SkillInjectNotify is called after a skill purchase from the skill window so a
	// logged-in client sees the book without a relog.
	esp.SkillInjectNotify = func(sess *esp.Session, itemID int64, newBalance float64) {
		if sess == nil || sess.Conn == nil {
			return
		}
		notifySkillInjected(sess.Conn, sess.AccountID, sess.CharID, itemID, newBalance, sqldb)
	}

	// esp.MarketAdminNotify is called after an ESP market admin action: it nudges the
	// affected owners' order windows, then tells docked clients to re-query prices.
	esp.MarketAdminNotify = func(charIDs []int64) {
		// OnOwnOrderChanged takes exactly (order, reason) on the RMR client.
		args := marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyNone{},
			marshal.PyString{Value: "Change"},
		}}
		for _, id := range charIDs {
			server.NotifyCharacter(sessReg, id, "OnOwnOrderChanged", "charid", args)
		}
		broadcastEconomyChange(sessReg)
	}

	// Factions that field customs officers, so the contraband page can flag ban lists
	// nothing enforces.
	esp.CustomsPatrolFactions = make(map[int64]string, len(customsFactions))
	for id, f := range customsFactions {
		esp.CustomsPatrolFactions[id] = f.name
	}

	// Module init runs after the DB, tick manager and shared registries exist and before
	// the ESP server is constructed, so modules can register admin pages (esp.ModuleHandlers).
	if err := modapi.InitAll(modapi.Context{
		DB:       sqldb,
		NodeID:   globalNodeID,
		Tick:     tickMgr,
		LSC:      lscReg,
		Stations: stationReg,
		Systems:  sysMgr,
		NPC:      npcMgr,
		Spawner:  spawnerMgr,
		Sessions: sessReg,
	}, cfg.Modules); err != nil {
		log.Fatalf("module init: %v", err)
	}

	// RPC coverage ledger. The previous run's ledger is reloaded so the "never seen"
	// bucket survives rebuilds.
	if cfg.RPCCoveragePath != "-" {
		rpccov.Init(cfg.RPCCoveragePath, 5*time.Minute)
	}

	// ESP skill-shop markup over the market average; config.json can raise the
	// default.
	if cfg.ESP.SkillMarkup > 1 {
		evedb.SkillPriceMarkup = cfg.ESP.SkillMarkup
	}

	// Every ESP hook below is assigned before esp.New builds the server.
	startTime := time.Now()
	if cfg.ESP.Enabled {
		// NPC authoring writes invTypes and dgmTypeAttributes; this drops the bulkdata
		// cachables built from them so clients see edits without a restart.
		esp.CacheInvalidator = cacheStore.InvalidateTypeData
		// The spawner reads the belt wave timer on every belt; this sets its reseed
		// cooldown when Spawner Rules or Region Spawns change.
		esp.BeltWaveTimerChanged = spawnerMgr.SetReseedCooldown
		// Create NPC and the raw attribute editor: drop the NPC manager's per-type caches so the
		// next spawn reads the edited signature radius and EWAR loadout.
		esp.NPCTypeChanged = func(typeID int64) { npcMgr.InvalidateTypeCaches(typeID) }
		// Region Spawns writes: reload those regions' belt rosters so new listings roll without a
		// restart.
		esp.RegionSpawnsChanged = func(regionIDs []int64) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			spawnerMgr.ReloadRegionRosters(ctx, regionIDs...)
		}
		// Dungeon Spawners page: spawn-now, despawn and live site figures.
		installOpenSiteESPHooks(sqldb)
		// A panel corporation edit pushes the OnCorporationChanged the corp window's own
		// writes push, so open clients update in place.
		esp.CorpChangedHook = func(corpID int64, changes []esp.CorpFieldChange) {
			entries := make([]marshal.DictEntry, 0, len(changes))
			for _, ch := range changes {
				entries = append(entries, chg(ch.Column, espChangeRep(ch.Old), espChangeRep(ch.New)))
			}
			onCorporationChanged(sessReg, corpID, entries)
			for _, ch := range changes {
				if ch.Column == "ceoID" {
					retallyAfterCEOChange(context.Background(), sqldb, sessReg, corpID)
					break
				}
			}
		}
		// The shareholder GM tools push what the in-game transfer and issue paths
		// push.
		esp.CorpSharesChangedHook = func(corpID int64, deltas []evedb.ShareDelta, oldTotal, newTotal int64) {
			announceShareChange(context.Background(), sqldb, sessReg, corpID, deltas, oldTotal, newTotal)
		}
		// The belt regeneration button writes seeds only; a loaded system keeps its field
		// until it empties, and the panel reports how many are loaded.
		esp.LoadedSystemCount = func() int {
			if globalSysMgr == nil {
				return 0
			}
			return len(globalSysMgr.All())
		}
		// The dashboard's vitals strip counts NPCs in space.
		esp.NPCCount = func() int {
			if globalNPCMgr == nil {
				return 0
			}
			return globalNPCMgr.Count()
		}
		esp.OwnerNamesInvalidator = func() {
			if globalCacheStore != nil {
				globalCacheStore.InvalidateOwnerNames()
				infocache.Shared.Clear() // names live in the identity memo too
			}
		}
		// The station services editor rewrites staOperations/staOperationServices, which
		// map.GetStationExtraInfo caches for the process lifetime.
		esp.StationServicesInvalidator = mapsvc.InvalidateStationExtraInfo
		// Force Build and Remove on the Outposts page run the live paths and drop the same
		// caches a pilot's Build does.
		installOutpostESPHooks(sqldb, sysMgr)
		installStargateESPHooks(sqldb, sysMgr)
		installStarbaseTemplateESPHooks()
		// A module-driven pilot coming or going is a login to everyone watching: buddy
		// lists are primed once and then rely on these pushes.
		esp.PresenceChangedHook = func(charID int64, online bool) {
			go broadcastOnlineStatus(sqldb, charID, online)
		}
		// A corporation deleted from the panel: each online kicked pilot gets the corpid
		// session change an in-game expulsion sends, and allianceid to None if the corp was allied.
		esp.CorpDeletedHook = func(corpID, oldAllianceID int64, kicked []evedb.ESPCorpKick) {
			for _, k := range kicked {
				conn, _ := charConnLookup(k.CharID)
				if conn == nil {
					continue
				}
				_ = slash.SendCorpSessionChange(conn, corpID, k.NPCCorpID, k.Roles)
				if oldAllianceID > 0 {
					_ = sendAllianceSessionChange(conn, "", oldAllianceID, 0)
				}
			}
		}
		// "Make effective now" on the alliance page runs the timer's sweep, so join, member
		// push and session change come from one place.
		esp.AllianceSweepHook = func() { sweepAllianceApplications(sqldb, sessReg) }
		// Panel alliance edits push the OnAlliance*Changed the in-game paths push; the
		// client refreshes its cached alliance data only from these.
		esp.AllianceEventHook = func(kind string, allianceID, subjectID int64) {
			espAllianceRefresh(sqldb, sessReg, kind, allianceID, subjectID)
		}
		// Panel war and bill actions run the full RPC path: fee to CONCORD, OnWarChanged
		// pushes, CONCORD mail, journal rows.
		esp.WarDeclareHook = func(declaredBy, against int64, backdate bool) error {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			feeCorp := declaredBy
			if kind, _ := evedb.ClassifyWarOwner(ctx, sqldb, declaredBy); kind == evedb.WarOwnerAlliance {
				var executor int64
				if err := sqldb.QueryRowContext(ctx,
					`SELECT executorCorpID FROM alliances WHERE allianceID = ?`, declaredBy,
				).Scan(&executor); err != nil || executor == 0 {
					return fmt.Errorf("the declaring alliance has no executor corporation to pay the fee")
				}
				feeCorp = executor
			}
			_, err := declareWar(ctx, sqldb, sessReg, declaredBy, feeCorp, against, backdate)
			invalidateWarCache()
			return err
		}
		esp.WarRetractHook = func(warID int64) error {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			w, err := evedb.GetWar(ctx, sqldb, warID)
			if err != nil || w == nil {
				return fmt.Errorf("no such war")
			}
			err = retractWarFor(ctx, sqldb, sessReg, w.DeclaredByID, warID)
			invalidateWarCache()
			return err
		}
		esp.WarMutualHook = func(warID int64, mutual bool) error {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			w, err := evedb.GetWar(ctx, sqldb, warID)
			if err != nil || w == nil {
				return fmt.Errorf("no such war")
			}
			return setWarMutualFor(ctx, sqldb, sessReg, w.DeclaredByID, warID, mutual)
		}
		esp.WarFinishHook = func(warID int64, reason string) error {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			w, err := evedb.GetWar(ctx, sqldb, warID)
			if err != nil || w == nil {
				return fmt.Errorf("no such war")
			}
			if w.TimeFinished.Valid {
				return fmt.Errorf("that war is already over")
			}
			finishWarNow(ctx, sqldb, sessReg, *w, reason)
			invalidateWarCache()
			return nil
		}
		esp.BillPayHook = func(billID int64) error {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			bill, err := evedb.PayBill(ctx, sqldb, billID, 0)
			if err != nil {
				return err
			}
			announceBillPaid(ctx, sqldb, sessReg, bill)
			return nil
		}
		esp.BillWriteOffHook = func(billID int64) error {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			bill, err := evedb.GetBill(ctx, sqldb, billID)
			if err != nil || bill == nil {
				return fmt.Errorf("no such bill")
			}
			if err := evedb.WriteOffBill(ctx, sqldb, billID); err != nil {
				return err
			}
			pushBillReceived(sessReg, bill.DebtorID)
			return nil
		}
		// A forced patent draw takes the sweep's path, announcement included, so the winner
		// and the entrants are told.
		esp.RnDForceDrawHook = func(agentID int64) (string, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			res, err := evedb.ForcePatentDraw(ctx, sqldb, agentID, time.Now())
			if err != nil {
				return "", err
			}
			if res == nil {
				return "That agent has no allocation round open — nobody is collaborating with it.", nil
			}
			announcePatentDraw(ctx, sqldb, res)
			if res.Outcome != evedb.PatentDrawWon {
				return fmt.Sprintf("Round %d drawn: %d entrants, none holding tickets — no allocation. "+
					"Research points were still wiped and the next round is open.",
					res.RoundNo, res.Entrants), nil
			}
			bpName, _ := evedb.GetTypeName(ctx, sqldb, res.BlueprintID)
			winner := evedb.GetCharacterName(ctx, sqldb, res.WinnerCharID)
			return fmt.Sprintf("Round %d drawn: %d entrants, %.1f tickets. %s wins %s and has been mailed. "+
				"Everyone's research points with this agent are now zero.",
				res.RoundNo, res.Entrants, res.TotalTickets, winner, bpName), nil
		}
		// A corp moved in or out of an alliance from the panel: each online member gets the
		// new allianceid session change, or its client keeps binding the old alliance object.
		esp.AllianceMembershipHook = func(corpID, oldAllianceID, newAllianceID int64) {
			// Sovereignty follows the corporation's structures (db/sovmajority.go).
			go func() {
				defer safego.Recover("main")
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				evedb.RecomputeSovForCorp(ctx, sqldb, corpID)
			}()
			for _, sess := range sessReg.FindByCorpID(corpID) {
				if sess.Conn == nil {
					continue
				}
				if err := sendAllianceSessionChange(sess.Conn, sess.Addr, oldAllianceID, newAllianceID); err != nil {
					log.Printf("esp: alliance session change to %s: %v", sess.Addr, err)
				}
			}
		}
		// A panel HQ move pushes the same session change as the in-game Move HQ Here
		// button.
		esp.CorpHQChangedHook = func(corpID, oldHQ, newHQ int64) {
			pushHQSessionChange(sessReg, corpID, oldHQ, newHQ)
		}
		esp.LocatorDelayMinutes = cfg.Agents.LocatorDelayMinutes
		esp.SessionTTL = time.Duration(cfg.ESP.SessionDays) * 24 * time.Hour
		esp.PprofEnabled = *cfg.ESP.Pprof
		esp.AllowedHosts = cfg.ESP.AllowedHosts
		espSrv, espErr := esp.New(cfg.ESP.Listen, sqldb, sessReg, startTime, cfg.ESP.MinRole, cfg.ESP.ClientPath, cfg.ESP.ModelsDirs)
		if espErr != nil {
			log.Fatalf("ESP init: %v", espErr)
		}
		espSrv.Start()
	}

	// Client news feed server (char-select news, billboard ticker). Clients reach it by a
	// hosts-file redirect of myeve.eve-online.com, so it listens on port 80.
	if cfg.ESP.NewsListen != "" {
		esp.NewNewsServer(cfg.ESP.NewsListen, sqldb, sessReg).Start()
	}

	// Market sweeps: stale player order expiry, daily price-history snapshots and the
	// optional NPC price simulation. NPC orders are seeded on demand only.
	startMarketSweeps(sqldb, sessReg, cfg.NPCMarket)

	// Accepted alliance applications become effective after a delay (the client promises
	// 24 hours); the join happens in this sweep.
	startAllianceApplicationSweep(tickMgr, sqldb, sessReg)

	// Gang boost sweep: catches docking, cross-system warps and pod changes, which have
	// no event hook (gangboost.go).
	startGangBoostSweep(tickMgr)

	// A Tower Force Field tab edit re-presents the field ball (starbase.go).
	posmgr.ForcefieldChanged = refreshStarbaseForcefield
	// The headless starbase ops (db/starbaseops.go) push to live grids through the /pos*
	// methods; a connectionless slashTeleporter suffices (they read only addr, sqldb, sysMgr).
	evedb.StarbaseLive = &slashTeleporter{addr: "starbase-ops", sqldb: sqldb, sysMgr: sysMgr}
	// Every starbase lifecycle event recomputes its system's sovereignty
	// (db/sovmajority.go); the boot pass settles changes made while the server was down.
	evedb.InstallSovRecompute(sqldb)
	go func() {
		defer safego.Recover("main")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if n, err := evedb.ReconcileAllSov(ctx, sqldb); err != nil {
			log.Printf("[sov] boot reconcile: %v", err)
		} else if n > 0 {
			log.Printf("[sov] boot reconcile changed %d system(s)", n)
		}
	}()
	// A majority that has waited out sov.flipGraceHours takes the system even when no starbase
	// event arrives (db/sovmajority.go PromoteDueSov).
	tickMgr.RegisterPeriodic("sov-grace", 300, func(uint64) bool {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if n, err := evedb.PromoteDueSov(ctx, sqldb); err != nil {
			log.Printf("[sov] grace sweep: %v", err)
		} else if n > 0 {
			log.Printf("[sov] grace sweep flipped %d system(s)", n)
		}
		return true
	})

	// War lifecycle (retraction cool-downs, weekly bills, unpaid-bill enforcement) and the
	// recurring non-war bills (alliance maintenance, office rent).
	tickMgr.RegisterPeriodic("war-sweep", 600, func(uint64) bool {
		sweepWars(sqldb, sessReg)
		return true
	})
	tickMgr.RegisterPeriodic("bill-sweep", 3600, func(uint64) bool {
		sweepRecurringBills(sqldb, sessReg)
		return true
	})

	// Without the SCC module, open exchange orders hand their ISK and shares
	// back (db/shareescrow.go). The module starts itself as a boot hook.
	if !evedb.ShareMarketInstalled {
		releaseShareEscrow(sqldb)
	}

	// Intergalactic Summit key letters (summit.go).
	startSummit(sqldb)

	// Overlay modules that add files to package main start here
	// (modulehooks.go); nothing runs in a module-free build.
	runModuleBootHooks(sqldb, sessReg, tickMgr)

	// infocache.Get never evicts, so an ID asked about once stays for the process
	// lifetime. This drops entries older than 15 min, past the longest TTL.
	tickMgr.RegisterPeriodic("infocache-sweep", 600, func(uint64) bool {
		if n := infocache.Shared.Sweep(15 * time.Minute); n > 0 {
			log.Printf("[infocache] swept %d stale entries (%d live)", n, infocache.Shared.Len())
		}
		return true
	})

	// Hourly belt-field expiry for systems nobody loads; the freshness check at load covers
	// visited ones. See docs/db-6-notes.md#persistent-asteroid-belts.
	tickMgr.RegisterPeriodic("asteroid-regen", 3600, func(uint64) bool {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					stack := debug.Stack()
					log.Printf("PANIC in asteroid-regen: %v\n%s", r, stack)
					bugtrack.ReportPanic("sweepExpiredAsteroidFields", r, stack)
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			sweepExpiredAsteroidFields(ctx, sqldb)
		}()
		return true
	})

	// Asteroid write-back: mining marks a system dirty and this flushes one batched upsert
	// per system every 30 s, which bounds what a hard crash loses.
	tickMgr.RegisterPeriodic("asteroid-flush", 30, func(uint64) bool {
		if !asteroidFlushRunning.CompareAndSwap(false, true) {
			return true
		}
		go func() {
			defer asteroidFlushRunning.Store(false)
			defer func() {
				if r := recover(); r != nil {
					stack := debug.Stack()
					log.Printf("PANIC in asteroid-flush: %v\n%s", r, stack)
					bugtrack.ReportPanic("flushAsteroids", r, stack)
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			if systems, rocks := flushAllAsteroids(ctx, sqldb); systems > 0 {
				log.Printf("[asteroid-flush] wrote %d rocks across %d systems", rocks, systems)
			}
		}()
		return true
	})

	// Hourly: rolls synthetic losses past killboard.botRetentionDays into killboard_rollup and
	// removes them (player losses stay). Single-flight off the tick goroutine; the first pass is large.
	tickMgr.RegisterPeriodic("killboard-retention", 3600, func(uint64) bool {
		if !killRetentionRunning.CompareAndSwap(false, true) {
			return true
		}
		go func() {
			defer killRetentionRunning.Store(false)
			defer func() {
				if r := recover(); r != nil {
					stack := debug.Stack()
					log.Printf("PANIC in killboard-retention: %v\n%s", r, stack)
					bugtrack.ReportPanic("sweepKillboardRetention", r, stack)
				}
			}()
			// Bounded so a first pass over a long backlog can finish and a wedged one cannot
			// run forever.
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			days := evedb.KillRetentionDays(ctx, sqldb)
			res, err := evedb.SweepKillboardRetention(ctx, sqldb, days, 2000)
			evedb.LogKillRetention(res, err)
		}()
		return true
	})

	// Wallet retention (db/walletretention.go): bounds market_journal and
	// market_transactions, hourly.
	tickMgr.RegisterPeriodic("wallet-retention", 3600, func(uint64) bool {
		if !walletRetentionRunning.CompareAndSwap(false, true) {
			return true
		}
		go func() {
			defer walletRetentionRunning.Store(false)
			defer func() {
				if r := recover(); r != nil {
					stack := debug.Stack()
					log.Printf("PANIC in wallet-retention: %v\n%s", r, stack)
					bugtrack.ReportPanic("sweepWalletRetention", r, stack)
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			// Roll price history first: the transaction sweep deletes no row whose day is not
			// yet in market_history_old.
			roll, err := evedb.RollPriceHistory(ctx, sqldb)
			evedb.LogPriceHistoryRoll(roll, err)
			res, err := evedb.SweepWalletRetention(ctx, sqldb)
			evedb.LogWalletRetention(res, err)
		}()
		return true
	})

	// Closed escrows and delivered industry jobs (db/historyretention.go), hourly.
	tickMgr.RegisterPeriodic("history-retention", 3600, func(uint64) bool {
		if !historyRetentionRunning.CompareAndSwap(false, true) {
			return true
		}
		go func() {
			defer historyRetentionRunning.Store(false)
			defer func() {
				if r := recover(); r != nil {
					stack := debug.Stack()
					log.Printf("PANIC in history-retention: %v\n%s", r, stack)
					bugtrack.ReportPanic("sweepHistoryRetention", r, stack)
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			res, err := evedb.SweepHistoryRetention(ctx, sqldb)
			evedb.LogHistoryRetention(res, err)
		}()
		return true
	})

	// Skill queue: completes due training and starts the next queued skill for every
	// character, online or not (the per-connection timer covers online ones only).
	tickMgr.RegisterPeriodic("skill-queue-sweep", 60, func(uint64) bool {
		// Off the tick goroutine: periodics fire synchronously and the first pass after
		// downtime can be long. The flag drops a tick whose predecessor still runs.
		if !skillSweepRunning.CompareAndSwap(false, true) {
			return true
		}
		go func() {
			defer skillSweepRunning.Store(false)
			defer func() {
				if r := recover(); r != nil {
					stack := debug.Stack()
					log.Printf("PANIC in skill-queue-sweep: %v\n%s", r, stack)
					bugtrack.ReportPanic("sweepSkillQueues", r, stack)
				}
			}()
			sweepSkillQueues(sqldb, sessReg)
		}()
		return true
	})

	// RAM job sweep (30 s): running jobs past their end time become ready.
	tickMgr.RegisterPeriodic("ram-job-sweep", 30, func(uint64) bool {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		n, err := evedb.TickJobs(ctx, sqldb, server.Win32TimeNow())
		if err != nil {
			log.Printf("ramJobTicker: %v", err)
		} else if n > 0 {
			log.Printf("ramJobTicker: %d jobs ready for delivery", n)
		}
		return true
	})

	tickMgr.RegisterPeriodic("courier-sweep", 60, func(uint64) bool {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		expired, failed, err := evedb.SweepExpiredCouriers(ctx, sqldb)
		if err != nil {
			log.Printf("[courier-sweep] %v", err)
		} else if expired > 0 || failed > 0 {
			log.Printf("[courier-sweep] expired=%d failed=%d", expired, failed)
		}
		return true
	})

	tickMgr.RegisterPeriodic("escrow-sweep", 300, func(uint64) bool {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		expired, err := evedb.SweepExpiredEscrows(ctx, sqldb)
		if err != nil {
			log.Printf("[escrow-sweep] %v", err)
		} else if expired > 0 {
			log.Printf("[escrow-sweep] expired=%d", expired)
		}
		return true
	})

	// SIGINT/SIGTERM (Windows delivers a closed console window as SIGTERM): flush the RPC
	// ledger and close the listener. See docs/main-s1-notes.md#shutdown-order.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		defer safego.Recover("main")
		s := <-sig
		log.Printf("shutting down (%v received)", s)
		if err := rpccov.Flush(); err != nil {
			log.Printf("rpc coverage: final save: %v", err)
		}
		ln.Close()
	}()

	var acceptDelay time.Duration
	for {
		raw, err := ln.Accept()
		if err != nil {
			if errors.Is(err, gonet.ErrClosed) {
				break
			}
			// Transient failure (EMFILE under fd pressure): back off and keep accepting;
			// breaking would end new logins for the process lifetime.
			if acceptDelay == 0 {
				acceptDelay = 5 * time.Millisecond
			} else if acceptDelay *= 2; acceptDelay > time.Second {
				acceptDelay = time.Second
			}
			log.Printf("accept: %v — retrying in %v", err, acceptDelay)
			time.Sleep(acceptDelay)
			continue
		}
		acceptDelay = 0
		// Keepalive turns a half-open connection into a ReadPacket error, so the disconnect
		// defers run.
		if tc, ok := raw.(*gonet.TCPConn); ok {
			tc.SetKeepAlive(true)
			tc.SetKeepAlivePeriod(2 * time.Minute)
		}
		go handleConn(raw, sqldb, cacheStore, lscReg, stationReg, sysMgr, sessReg, cfg, tickMgr, npcMgr)
	}

	flushOnShutdown(sqldb, tickMgr)
	log.Printf("server stopped — log saved to server.log")
}

// ─── Market background sweeps ────────────────────────────────────────────────

// startMarketSweeps starts the market's background maintenance: player order expiry
// (10 min), daily price-history snapshots and, when npcMarket.Dynamics is set, the NPC
// supply/demand repricing every tickMinutes. NPC orders are seeded on demand only
// (ESP panel, /seednpcmarkets).
func startMarketSweeps(sqldb *sql.DB, sessReg *esp.SessionRegistry, npcMarket config.NPCMarketConfig) {
	go func() {
		defer safego.Recover("startMarketSweeps")
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			n, err := evedb.RecordDailyPriceSnapshots(ctx, sqldb)
			cancel()
			if err != nil {
				log.Printf("market: price snapshot: %v", err)
				continue
			}
			log.Printf("market: recorded %d price-history snapshots", n)
		}
	}()

	if npcMarket.Dynamics {
		go func() {
			defer safego.Recover("startMarketSweeps")
			ticker := time.NewTicker(time.Duration(npcMarket.TickMinutes) * time.Minute)
			defer ticker.Stop()
			for range ticker.C {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				repriced, restocked, err := evedb.TickNPCMarketDynamics(ctx, sqldb)
				cancel()
				if err != nil {
					log.Printf("market: NPC dynamics tick: %v", err)
					continue
				}
				log.Printf("market: NPC dynamics tick — repriced %d trade-good orders, restocked %d", repriced, restocked)
			}
		}()
	}

	// ExpireMarketOrders refunds bid escrow and returns unsold sell items; online owners
	// get OnOwnOrderChanged so their order windows refresh.
	go func() {
		defer safego.Recover("startMarketSweeps")
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			expired, err := evedb.ExpireMarketOrders(ctx, sqldb)
			cancel()
			if err != nil {
				log.Printf("market: expiry sweep: %v", err)
				continue
			}
			if len(expired) == 0 {
				continue
			}
			log.Printf("market: expired %d player orders", len(expired))
			args := marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyNone{},
				marshal.PyString{Value: "Expiry"},
			}}
			for _, e := range expired {
				server.NotifyCharacter(sessReg, e.CharID, "OnOwnOrderChanged", "charid", args)
			}
		}
	}()
}

// broadcastEconomyChange tells every docked client that market orders
// changed so open market windows re-query prices. Same OnOwnOrderChanged
// event the market service sends after a player trade.
func broadcastEconomyChange(sessReg *esp.SessionRegistry) {
	args := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyNone{},
		marshal.PyString{Value: "Change"},
	}}
	server.BroadcastNotificationExcept(sessReg, "", func(s *esp.Session) bool {
		return s.StationID != 0
	}, "OnOwnOrderChanged", "stationid", args)
}

func handleConn(raw gonet.Conn, sqldb *sql.DB, cacheStore *evecache.Store, lscReg *lsc.Registry, stationReg *station.StationRegistry, sysMgr *destiny.SystemManager, sessReg *esp.SessionRegistry, cfg *config.Config, tickMgr *tick.Manager, npcMgr *npc.NPCManager) {
	addr := raw.RemoteAddr().String()
	// ds and accountID are declared before the disconnect defers that read them and are
	// assigned after the handshake. Teardown order: docs/main-s2-notes.md#disconnect-teardown.
	var ds *destinyConn
	var accountID int64
	// connLog tags a line with the connection and, once logged in, its account and character.
	connLog := func() *slog.Logger {
		var charID int64
		if ds != nil {
			charID = ds.charID
		}
		return logx.Conn(addr, accountID, charID)
	}
	// Registered first so it runs last: a panic in the dispatch chain unwinds through the
	// cleanup defers below and ends only this connection. The stack goes to bugtrack.
	defer func() {
		if r := recover(); r != nil {
			stack := debug.Stack()
			connLog().Error("PANIC in connection handler", "panic", r)
			log.Printf("[%s] PANIC stack:\n%s", addr, stack)
			bugtrack.ReportPanic("handleConn", r, stack)
		}
	}()
	connLog().Info("connected")
	defer func() {
		raw.Close()
		sessReg.Unregister(addr)
		rpccov.Forget(addr)
		connLog().Info("disconnected")
	}()

	c := evenet.NewConn(raw)
	defer lsc.HandleDisconnect(c, lscReg, sqldb)
	defer stationReg.Remove(c)

	defer func() {
		if ds == nil {
			return
		}
		// The completion timer would write OnSkillTrained to this dead socket.
		if ds.charID != 0 {
			cancelSkillCompletionTimer(c, ds.charID)
			// Abort trades before the connection map drops the peer the abort still notifies.
			tctx, tcancel := context.WithTimeout(context.Background(), 5*time.Second)
			trademgr.AbortForCharacter(tctx, ds.charID, globalTradeReg, charConnLookup, sqldb)
			tcancel()
			globalCharConns.Delete(ds.charID)
			forgetContainerUnlocks(ds.charID)
			gangsta.HandleDisconnect(ds.charID, globalGangReg, charConnLookup,
				func(cid, gid, corp int64) { broadcastGangSysChange(sysMgr, cid, gid, corp) })
			go broadcastOnlineStatus(sqldb, ds.charID, false)
		}
		// Close first so background broadcasts (shield regen, tick loop) skip this connection.
		c.Close()
		// Dock, jump and death cancel the reconciler themselves; a raw disconnect would leave it
		// running and pinning the solar system.
		if ds.cancelReconcile != nil {
			ds.cancelReconcile()
			ds.cancelReconcile = nil
		}
		// A cyno field and a jump bridge end with their pilot's connection. See docs/jump-notes.md#cyno.
		if ds.charID != 0 {
			collapseCynoBeaconForChar(ds.charID, "disconnect")
			closeJumpBridgeForChar(ds.charID, "disconnect")
		}
		// Cancel weapon, mining and repair cycles as the dock path does. Cap boosters and
		// zero-cap weapons never cap out, so their loops would outlive the connection.
		for modID, cancelFn := range ds.cancelWeapon {
			cancelFn()
			delete(ds.cancelWeapon, modID)
		}
		for modID, sess := range ds.miningSessions {
			sess.cancel()
			delete(ds.miningSessions, modID)
		}
		ds.dropReloads()
		// A warp in flight and pending target locks hold ds, its entity and the solar system
		// until their timers end. Dock, jump and death cancel them; so does a disconnect.
		if ds.cancelWarp != nil {
			ds.cancelWarp()
			ds.cancelWarp = nil
		}
		ds.targetMu.Lock()
		for id, cancelFn := range ds.lockingTargets {
			cancelFn()
			delete(ds.lockingTargets, id)
		}
		ds.targetMu.Unlock()
		if ds.solarSystem != nil && ds.entity != nil {
			// Persist the in-space position and clear stationID so the next login resumes on grid.
			// Run now and again when the ship leaves space (a crash mid-linger keeps the first).
			persistInSpace := func() {
				if ds.dm == nil || ds.charID == 0 || ds.shipID == 0 {
					return
				}
				ctx := context.Background()
				pos := ds.dm.Pos
				if err := evedb.UpdateEntityPosition(ctx, sqldb, ds.shipID, pos.X, pos.Y, pos.Z); err != nil {
					log.Printf("[%s] disconnect: save ship position: %v", addr, err)
				}
				if err := evedb.UpdateCharacterStation(ctx, sqldb, ds.charID, 0, ds.sysID, ds.constellationID, ds.regionID); err != nil {
					log.Printf("[%s] disconnect: clear stationID: %v", addr, err)
				}
				if err := evedb.UpdateEntityLocation(ctx, sqldb, ds.shipID, ds.sysID); err != nil {
					log.Printf("[%s] disconnect: save ship locationID: %v", addr, err)
				}
				log.Printf("[%s] saved in-space position for char %d ship %d at (%.0f, %.0f, %.0f) in system %d",
					addr, ds.charID, ds.shipID, pos.X, pos.Y, pos.Z, ds.sysID)
			}
			persistInSpace()
			persistShipDamage(ds, sqldb)

			// Drones this ship controlled stay in space and revert to owner control.
			if globalDroneMgr != nil && ds.shipID != 0 {
				globalDroneMgr.AbandonControl(ds.shipID)
			}

			// leaveSpace is the removal half: now, or when a logoff linger ends
			// (logofflinger.go). A pod kill during the linger has already left space.
			leaveSpace := func() {
				if ds.solarSystem == nil || ds.entity == nil {
					return
				}
				persistInSpace()
				persistShipDamage(ds, sqldb)

				// Remove this connection's rigid dungeon balls from grid-mates; nobody else will.
				var dunRigid []uint32
				for eid := range ds.dunObjSnapshot() {
					if destiny.IsDunRigidBallID(eid) {
						dunRigid = append(dunRigid, eid)
					}
				}
				// An open site's scenery is shared and stays.
				broadcastDunRemoveBalls(ds, openSiteLocalBalls(ds.dunOpenSiteID, dunRigid))

				remaining := ds.solarSystem.RemoveEntity(ds.entity.ID)
				for _, mate := range remaining {
					if mate.Conn == nil || !mate.KnowsBall(ds.entity.ID) {
						continue
					}
					mate.SendStamped(func(stamp uint32) error {
						return destiny.SendRemoveBalls(mate.Conn, globalNodeID, mate.AccountID, stamp, []uint32{ds.entity.ID})
					})
					mate.ForgetBalls(ds.entity.ID)
				}
				releaseSystem(ds.sysMgr, ds.solarSystem, sqldb)
				// The socket is closed above. ClearBallparkState makes a sender that still holds this
				// Entity in a bubble-mate snapshot fail its KnowsBall guard instead of writing to it.
				ds.entity.ClearBallparkState()
			}
			if !startLogoffLinger(ds, sqldb, npcMgr, leaveSpace) {
				leaveSpace()
			}
		}
	}()

	// Bounds the handshake: ReadPacket has no timeout of its own. Cleared once the handshake
	// completes; TCP keepalive covers dead transport after that.
	raw.SetReadDeadline(time.Now().Add(handshakeTimeout))

	// ── Step 1: server sends its VersionExchange ─────────────────────────────
	// UserCount is item 2 of the tuple; login.py prints it as the player count on the login
	// screen. It counts every registered session, rolebots included.
	ourVE := evenet.VersionExchange{
		Birthday:       evenet.DefaultBirthday,
		MachoVersion:   machoVersion,
		UserCount:      uint64(sessReg.Count()),
		VersionNumber:  bootVersion,
		BuildVersion:   bootBuild,
		ProjectVersion: projectVersion,
	}
	if err := sendRep(c, ourVE.Encode()); err != nil {
		log.Printf("[%s] send VersionExchange: %v", addr, err)
		return
	}
	log.Printf("[%s] → VersionExchange (macho=%d ver=%.2f build=%d project=%q)",
		addr, machoVersion, bootVersion, bootBuild, projectVersion)

	// ── Step 2: receive client's VersionExchange ─────────────────────────────
	rep, err := readRep(c)
	if err != nil {
		log.Printf("[%s] read VersionExchange: %v", addr, err)
		return
	}
	var clientVE evenet.VersionExchange
	if err := clientVE.Decode(rep); err != nil {
		log.Printf("[%s] decode VersionExchange failed: %v — raw type: %T  value: %+v", addr, err, rep, rep)
		return
	}
	log.Printf("[%s] ← VersionExchange macho=%d ver=%.2f build=%d project=%q",
		addr, clientVE.MachoVersion, clientVE.VersionNumber, clientVE.BuildVersion, clientVE.ProjectVersion)

	// Mismatches are logged, not refused.
	if clientVE.Birthday != evenet.DefaultBirthday {
		log.Printf("[%s] WARNING: client birthday %d != expected %d", addr, clientVE.Birthday, evenet.DefaultBirthday)
	}
	if clientVE.MachoVersion != machoVersion {
		log.Printf("[%s] WARNING: client macho_version %d != expected %d", addr, clientVE.MachoVersion, machoVersion)
	}
	if clientVE.VersionNumber != bootVersion {
		log.Printf("[%s] WARNING: client version_number %.2f != expected %.2f", addr, clientVE.VersionNumber, bootVersion)
	}
	if clientVE.BuildVersion != bootBuild {
		log.Printf("[%s] WARNING: client build_version %d != expected %d", addr, clientVE.BuildVersion, bootBuild)
	}
	if clientVE.ProjectVersion != projectVersion {
		log.Printf("[%s] WARNING: client project_version %q != expected %q", addr, clientVE.ProjectVersion, projectVersion)
	}

	// No crypto in Blood mode.
	raw.SetReadDeadline(time.Time{})
	log.Printf("[%s] handshake complete, reading PyPackets", addr)

	// accountID is set by AuthenticationReq and charID by character.SelectCharacterID.
	// stationID is the character's station, so station.GetStationItemBits can serve the
	// no-arg form.
	var charID int64
	var stationID int64
	agentBinds := &agentBindState{}

	ds = &destinyConn{stamp: 1, sysMgr: sysMgr, dungeonRefresh: make(chan int64, 1), dunWaveSpawn: make(chan dunWaveReq, 8), dunRespawn: make(chan dunRespawnReq, 4), dockCheck: make(chan struct{}, 1), gangWarp: make(chan gangWarpReq, 1), missionEnded: make(chan int64, 4), playerDeath: make(chan playerDeathReq, 2), stampResync: make(chan struct{}, 1), warpPropModOff: make(chan uint64, 1), lastActivity: time.Now(), stationIDPtr: &stationID}
	// The checker goroutine and the station clearance slot end with the connection.
	defer cancelPendingDock(ds)

	// ds.dungeonRefresh is set on the session in the AuthenticationReq handler, after
	// Register(): Update() is a no-op before the session exists.

	// ── Step 3: read PyPackets until disconnect ───────────────────────────────

	// The blocking read runs in a goroutine so the loop below can also service the ESP
	// panel's dungeon-refresh commands.
	type pktResult struct {
		rep marshal.PyRep
		err error
	}
	pktCh := make(chan pktResult)
	// readerDone releases the reader's send when handleConn returns between packets;
	// otherwise it parks on pktCh.
	readerDone := make(chan struct{})
	defer close(readerDone)
	go func() {
		defer safego.Recover("handleConn")
		for {
			rep, err := readRep(c)
			select {
			case pktCh <- pktResult{rep, err}:
			case <-readerDone:
				return
			}
			if err != nil {
				return
			}
		}
	}()

	// Reaps a peer that completes the handshake and never authenticates; TCP keepalive
	// only detects dead transport.
	idleCheck := time.NewTicker(time.Minute)
	defer idleCheck.Stop()

	for {
		select {
		case res := <-pktCh:
			if res.err != nil {
				if !errors.Is(res.err, io.EOF) {
					connLog().Error("read failed", "err", res.err)
				}
				return
			}
			ds.lastActivity = time.Now()

			pkt := &evenet.PyPacket{}
			if err := pkt.Decode(res.rep); err != nil {
				log.Printf("[%s] ← (not a PyPacket: %T): %v", addr, res.rep, err)
				continue
			}
			logx.Tracef("[%s] ← PyPacket type=%-30s src=%c svc=%q → dst=%c svc=%q",
				addr, msgTypeName(pkt.Type),
				pkt.Source.Type, pkt.Source.Service,
				pkt.Dest.Type, pkt.Dest.Service)

			if err := handlePacket(c, addr, pkt, sqldb, &accountID, &charID, &stationID, agentBinds, cacheStore, lscReg, stationReg, ds, sysMgr, sessReg, cfg, npcMgr); err != nil {
				connLog().Error("handlePacket failed", "err", err)
				return
			}

		case roomID := <-ds.dungeonRefresh:
			if ds.dunRoomID > 0 && roomID == ds.dunRoomID {
				log.Printf("[%s] ESP dungeon refresh: respawning room %d", addr, roomID)
				// The open-site tag rides the refresh so the room keeps its instance frame and kill
				// tracking.
				ds.dunPendingOpenSite = ds.dunOpenSiteID
				if err := spawnDungeonRoom(c, addr, accountID, sqldb, ds, roomID, sessReg, npcMgr, ds.dunMissionID); err != nil {
					log.Printf("[%s] ESP dungeon refresh error: %v", addr, err)
				}
			}

		case req := <-ds.dunWaveSpawn:
			spawnDungeonWave(c, addr, accountID, sqldb, ds, npcMgr, req)

		case req := <-ds.dunRespawn:
			handleDunRespawn(c, addr, accountID, sqldb, ds, npcMgr, req)

		case <-ds.dockCheck:
			if err := tickPendingDock(c, addr, accountID, charID, &stationID, sqldb, ds); err != nil {
				log.Printf("[%s] dock: %v", addr, err)
				return
			}

		case req := <-ds.gangWarp:
			runGangWarp(c, addr, accountID, sqldb, ds, sessReg, npcMgr, req)

		case missionID := <-ds.missionEnded:
			endMissionPocket(c, addr, accountID, ds, sessReg, npcMgr, sqldb, missionID)

		case req := <-ds.playerDeath:
			if req.pod {
				handlePlayerPodKilled(ds, sqldb)
			} else {
				handlePlayerShipDestroyed(ds, sqldb, npcMgr)
			}

		case gen := <-ds.warpPropModOff:
			if gen == ds.warpGen {
				stopPropModsForWarp(c, addr, accountID, charID, ds, sqldb)
			}

		case <-ds.stampResync:
			// Re-check here: a SetState the pilot triggered (jump, undock) may have fixed the lead
			// since the reconciler read it.
			if ds.entity != nil && stampLeadResyncThreshold > 0 &&
				ds.entity.StampLead() > stampLeadResyncThreshold {
				log.Printf("[%s] stamp lead %d over threshold %d — forcing ballpark resync. %s",
					addr, ds.entity.StampLead(), stampLeadResyncThreshold, ds.entity.StampReport())
				resyncBallpark(c, addr, uint64(accountID), ds)
			}

		case <-idleCheck.C:
			if accountID == 0 && time.Since(ds.lastActivity) > unauthIdleKick {
				log.Printf("[%s] closing idle unauthenticated connection", addr)
				return
			}
		}
	}
}

func handlePacket(c *evenet.Conn, addr string, pkt *evenet.PyPacket, sqldb *sql.DB, accountID *int64, charID *int64, stationID *int64, agentBinds *agentBindState, cacheStore *evecache.Store, lscReg *lsc.Registry, stationReg *station.StationRegistry, ds *destinyConn, sysMgr *destiny.SystemManager, sessReg *esp.SessionRegistry, cfg *config.Config, npcMgr *npc.NPCManager) error {
	switch pkt.Type {
	case evenet.PingReq:
		rsp := &evenet.PyPacket{
			TypeString: pkt.TypeString,
			Type:       evenet.PingRsp,
			Source:     pkt.Dest,
			Dest:       pkt.Source,
			Payload:    marshal.PyTuple{},
		}
		log.Printf("[%s] → PingRsp", addr)
		return sendRep(c, rsp.Encode())

	case evenet.AuthenticationReq:
		login, password, langID, err := decodeAuthReq(pkt.Payload)
		if err != nil {
			log.Printf("[%s] bad AuthenticationReq payload: %v", addr, err)
			return sendLoginError(c, addr, pkt)
		}
		log.Printf("[%s] AuthenticationReq login=%q languageID=%q", addr, login, langID)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		id, role, err := evedb.DoLogin(ctx, sqldb, login, password, cfg.Authentication.AutoAccount, cfg.Authentication.Role)
		if err != nil {
			if errors.Is(err, evedb.ErrBadCredentials) {
				log.Printf("[%s] login rejected: %q", addr, login)
			} else {
				log.Printf("[%s] login db error: %v", addr, err)
			}
			return sendLoginError(c, addr, pkt)
		}
		log.Printf("[%s] login accepted: %q accountID=%d role=%d", addr, login, id, role)
		*accountID = id
		sessReg.Register(addr, &esp.Session{
			AccountID:      id,
			AccountName:    login,
			Conn:           c,
			Addr:           addr,
			ConnectedAt:    time.Now(),
			DungeonRefresh: ds.dungeonRefresh,
		})
		return sendLoginSuccess(c, addr, pkt, id, role, langID, sqldb, cacheStore, ds)

	case evenet.CallReq:
		// Nothing is dispatched before AuthenticationReq succeeds: every service keys on the
		// account and character this connection has not yet proven.
		if *accountID == 0 {
			log.Printf("[%s] CallReq svc=%q before login — refused", addr, pkt.Dest.Service)
			return server.SendCallException(c, pkt, 0, marshal.PyString{Value: "Not logged in"})
		}
		cs, err := server.DecodeCallStream(pkt)
		if err != nil {
			log.Printf("[%s] CallReq decode: %v — sending exception", addr, err)
			return server.SendCallException(c, pkt, *accountID, nil)
		}
		if logx.Enabled(logx.LevelDebug) {
			logx.Conn(addr, *accountID, *charID).Debug("CallReq", "svc", pkt.Dest.Service, "method", cs.Method)
		}
		// Ledgered as handled; stub and fallthrough arms downgrade the entry (rpccov, ESP /coverage).
		rpccov.Record(addr, pkt.Dest.Service, cs.Method, rpccov.Handled)

		// Every service answers MachoResolveObject and MachoBindObject; both are handled here,
		// before the per-service switch.
		if cs.Method == "MachoResolveObject" {
			log.Printf("[%s] → MachoResolveObject → nodeID=%d", addr, globalNodeID)
			return server.SendCallRsp(c, pkt, *accountID, marshal.PyInt{Value: globalNodeID})
		}
		if cs.Method == "MachoBindObject" {
			// The bind spec is the object's identity: machoNet keys bound objects by this string, so
			// binds sharing a spec share one object. agentMgr binds are per agent, so the agentID is
			// the bind number. See docs/main-s2-notes.md#bind-spec.
			bindNum := uint64(1)
			if pkt.Dest.Service == "agentMgr" && len(cs.ArgTuple.Items) >= 1 {
				if aid, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok && aid.Value > 0 {
					bindNum = aid.Value
				}
			}
			// allianceRegistry binds carry the tuple (allianceID, isMaster); the allianceID becomes
			// the bind number, only for an alliance that exists. See docs/corp-notes.md#alliance-binds.
			if pkt.Dest.Service == "allianceRegistry" && len(cs.ArgTuple.Items) >= 1 {
				if params, ok := cs.ArgTuple.Items[0].(marshal.PyTuple); ok && len(params.Items) >= 1 {
					if aid, ok := params.Items[0].(marshal.PyInt); ok && aid.Value > 0 &&
						allianceExists(sqldb, int64(aid.Value)) {
						bindNum = aid.Value
						rememberAllianceBind(int64(bindNum), int64(aid.Value))
					}
				}
			}
			bindSpec := fmt.Sprintf("N=%d:%d", globalNodeID, bindNum)
			boundObj := marshal.PySubStruct{Value: &marshal.PySubStream{
				Decoded: marshal.PyTuple{Items: []marshal.PyRep{
					marshal.PyString{Value: bindSpec},
					marshal.PyInt{Value: 0}, // no expiry
				}},
			}}
			// moniker.Bind unpacks the response as (boundObj, callResult); the inline call's result
			// is the second item.
			inlineResult := computeInlineBindResult(c, *accountID, addr, pkt.Dest.Service, cs, *charID, *stationID, sqldb, ds)
			boardHeld := false // set by an inline Board that moved the pilot
			var afterBind func() // runs once the bind reply is out (inline Eject / in-space Board)
			// The client builds fresh monikers per drone command, so entity Cmd* and ship
			// Drop/ScoopDrone arrive inlined here. They need the live result (Drop returns the
			// launched IDs, Cmd* the failure dict).
			if mth := inlineBindMethod(cs); mth != "" {
				if callTuple, ok := cs.ArgTuple.Items[1].(marshal.PyTuple); ok && len(callTuple.Items) >= 2 {
					if argsTuple, ok := callTuple.Items[1].(marshal.PyTuple); ok {
						switch {
						case pkt.Dest.Service == "entity" && isDroneCommandMethod(mth):
							inlineResult = handleDroneCommand(mth, argsTuple, ds, *charID)
						case pkt.Dest.Service == "ship" && mth == "Drop":
							var ids []int64
							if len(argsTuple.Items) >= 1 {
								ids = parseDroneIDs(argsTuple.Items[0])
							}
							var dropOwnerID int64
							if len(argsTuple.Items) >= 2 {
								if v, ok := argsTuple.Items[1].(marshal.PyInt); ok {
									dropOwnerID = int64(v.Value)
								}
							}
							structIDs, droneIDs := splitStructureDropIDs(sqldb, ds.shipID, ids)
							newIDs := launchStructuresForPlayer(ds, addr, *charID, dropOwnerID, structIDs, sqldb)
							newIDs = append(newIDs, launchDronesForPlayer(ds, *charID, droneIDs, sqldb)...)
							inlineResult = pyIntList(newIDs)
						case pkt.Dest.Service == "posMgr" && mth == "AnchorStructure":
							if itemID, x, y, z, ok := parseAnchorStructureArgs(argsTuple); ok {
								if err := anchorStructureForPlayer(ds, addr, *charID, itemID, x, y, z, sqldb); err != nil {
									// The client swallows an exception on a bind, so the refusal goes to the notify line
									// and the message log.
									log.Printf("[%s] posMgr.AnchorStructure(inline) %d: %v", addr, itemID, err)
									posRefusal(ds, "Cannot anchor", err)
								}
							}
							inlineResult = marshal.PyNone{}
						case pkt.Dest.Service == "missionMgr" && mth == "CreateCourierMission":
							ctxCr, cancelCr := context.WithTimeout(context.Background(), 15*time.Second)
							r, cerr := createCourierMission(ctxCr, addr, c, pkt, *accountID, *charID, *stationID, cs, sqldb)
							cancelCr()
							if cerr != nil {
								log.Printf("[%s] missionMgr.CreateCourierMission(inline) charID=%d: %v", addr, *charID, cerr)
								return server.SendCallException(c, pkt, *accountID, server.BuildUserError(
									"CustomError", map[string]marshal.PyRep{
										"error": marshal.PyString{Value: cerr.Error()},
									}))
							}
							inlineResult = r
						case pkt.Dest.Service == "ship" && (mth == "Eject" || mth == "Evacuate" ||
							(mth == "Board" && *stationID == 0)):
							// eject.go. The client builds a fresh ship moniker per call, so these
							// arrive here; a refusal raised on the bind reaches PerformSessionChange
							// and releases its 30 s lock.
							if mth != "Board" && *stationID != 0 {
								return server.SendCallException(c, pkt, *accountID,
									customError("You cannot eject while docked."))
							}
							refusal, after := shipSpaceCall(c, addr, *accountID, *charID, mth, argsTuple.Items, sqldb, ds)
							if refusal != nil {
								log.Printf("[%s] ship.%s(inline): refused charID=%d", addr, mth, *charID)
								return server.SendCallException(c, pkt, *accountID, refusal)
							}
							inlineResult = marshal.PyNone{}
							afterBind = after
						case pkt.Dest.Service == "ship" && (mth == "StoreVessel" || mth == "BoardStoredShip" ||
							mth == "LaunchFromContainer" || mth == "ScoopToSMA"):
							// smaship.go, same shape as Eject above.
							refusal, after := smaShipCall(c, addr, *accountID, *charID, *stationID, mth, argsTuple.Items, sqldb, ds)
							if refusal != nil {
								log.Printf("[%s] ship.%s(inline): refused charID=%d", addr, mth, *charID)
								return server.SendCallException(c, pkt, *accountID, refusal)
							}
							inlineResult = marshal.PyNone{}
							afterBind = after
						case pkt.Dest.Service == "ship" && mth == "Board":
							// Refuse the bind when the pilot lacks the hull's prerequisites; raising here stops the
							// board before the entity move below.
							if len(argsTuple.Items) >= 1 {
								if shipIDRep, ok := argsTuple.Items[0].(marshal.PyInt); ok {
									ctxB, cancelB := context.WithTimeout(context.Background(), 5*time.Second)
									uerr := boardRefusal(ctxB, sqldb, *charID, *stationID, int64(shipIDRep.Value))
									cancelB()
									if uerr != nil {
										log.Printf("[%s] ship.Board(inline): rejected shipID=%d charID=%d", addr, int64(shipIDRep.Value), *charID)
										return server.SendCallException(c, pkt, *accountID, uerr)
									}
									// The board and its SCN run before the bind reply (boardDockedShip).
									newShipID := int64(shipIDRep.Value)
									prevShipID, berr := boardDockedShip(c, addr, *accountID, *charID, newShipID, sqldb)
									if berr != nil {
										log.Printf("[%s] ship.Board(inline): %v", addr, berr)
									} else {
										boardHeld = true
									}
									log.Printf("[%s] → ship.MachoBindObject(Board) charID=%d shipID=%d→%d", addr, *charID, prevShipID, newShipID)
								}
							}
						case pkt.Dest.Service == "ship" && mth == "ScoopDrone":
							var ids []int64
							if len(argsTuple.Items) >= 1 {
								ids = parseDroneIDs(argsTuple.Items[0])
							}
							scoopDronesForPlayer(ds, *charID, ids, sqldb)
							inlineResult = marshal.PyNone{}
						case pkt.Dest.Service == "ship" && mth == "Scoop":
							scoopFromCall(ds, addr, *charID, argsTuple, sqldb)
							inlineResult = marshal.PyNone{}
						}
					}
				}
			}
			log.Printf("[%s] → MachoBindObject svc=%q → bindSpec=%q", addr, pkt.Dest.Service, bindSpec)
			bindRsp := marshal.PyTuple{Items: []marshal.PyRep{boundObj, inlineResult}}
			if boardHeld {
				// An inline Board's bind reply waits for godma's re-prime (boardhold.go).
				acct := *accountID
				holdBoardReply(c, func() { _ = server.SendCallRsp(c, pkt, acct, bindRsp) })
			} else if err := server.SendCallRsp(c, pkt, *accountID, bindRsp); err != nil {
				return err
			}
			if afterBind != nil {
				afterBind()
			}
			// Remember the bind so bound calls on this bind ID resolve to the agent.
			if pkt.Dest.Service == "agentMgr" && len(cs.ArgTuple.Items) >= 1 {
				if aid, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
					agentBinds.register(int64(aid.Value))
					log.Printf("[%s] → agentMgr.MachoBindObject agentID=%d bind=%q", addr, int64(aid.Value), bindSpec)
				}
			}
			// A ship bind with an inline Undock runs the undock side effects: destiny init, the
			// docked-to-space session change and registerUndock.
			if pkt.Dest.Service == "ship" && inlineBindMethod(cs) == "Undock" {
				initDestiny(addr, *charID, *stationID, sqldb, ds)
				log.Printf("[%s] → ship.MachoBindObject(Undock) charID=%d", addr, *charID)
				registerUndock(c, addr, uint64(*accountID), *charID, sqldb, ds)
				if ds.sysID != 0 {
					if err := sendUndockSessionChange(c, addr, uint64(*accountID), *stationID, ds.sysID, ds.shipID); err != nil {
						log.Printf("[%s] undock SCN: %v", addr, err)
					}
				}
				// SetState waits for beyonce.GetFormations. See docs/main-s8-notes.md#setstate-from-formations.
				// The pilot is in space now: clear stationID so slash commands see them undocked.
				*stationID = 0
				if sqldb != nil && *charID != 0 && ds.sysID != 0 {
					ctxU, cancelU := context.WithTimeout(context.Background(), 5*time.Second)
					if err := evedb.ClearCharacterStation(ctxU, sqldb, *charID, ds.sysID, ds.constellationID, ds.regionID); err != nil {
						log.Printf("[%s] undock ClearCharacterStation: %v", addr, err)
					}
					cancelU()
				}
			}
			// The keeper moniker inlines ActivateAccelerationGate into the bind; the gate effects
			// (FX, ship fling, next room) run after the bind response.
			if pkt.Dest.Service == "keeper" && inlineBindMethod(cs) == "ActivateAccelerationGate" {
				if callTuple, ok := cs.ArgTuple.Items[1].(marshal.PyTuple); ok && len(callTuple.Items) >= 2 {
					if argsTuple, ok := callTuple.Items[1].(marshal.PyTuple); ok && len(argsTuple.Items) >= 1 {
						if gateIDRep, ok := argsTuple.Items[0].(marshal.PyInt); ok {
							runActivateGate(c, addr, *accountID, uint32(gateIDRep.Value), sqldb, ds, sessReg, npcMgr)
						}
					}
				}
			}
			// The client inlines the first call on a fresh agent moniker into the bind, so a
			// mission-bookmark warp can arrive here. The warp runs after the bind response.
			if pkt.Dest.Service == "agentMgr" {
				if mth := inlineBindMethod(cs); mth == "GotoLocation" || mth == "WarpToLocation" {
					agentID := int64(0)
					if len(cs.ArgTuple.Items) >= 1 {
						if aid, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
							agentID = int64(aid.Value)
						}
					}
					if callTuple, ok := cs.ArgTuple.Items[1].(marshal.PyTuple); ok && len(callTuple.Items) >= 2 {
						if argsTuple, ok := callTuple.Items[1].(marshal.PyTuple); ok {
							locType, locNum, warpRange := parseWarpToLocationArgs(argsTuple)
							runAgentWarpToLocation(c, addr, *accountID, *charID, agentID, locType, locNum, warpRange, sqldb, ds, sessReg, npcMgr)
						}
					}
				}
			}
			// Inline AssembleShip sets singleton=1 and notifies the client so the hangar view
			// updates without a redock.
			if pkt.Dest.Service == "ship" && inlineBindMethod(cs) == "AssembleShip" {
				if callTuple, ok := cs.ArgTuple.Items[1].(marshal.PyTuple); ok && len(callTuple.Items) >= 2 {
					if argsTuple, ok := callTuple.Items[1].(marshal.PyTuple); ok && len(argsTuple.Items) >= 1 {
						var itemIDs []int64
						if lst, ok := argsTuple.Items[0].(marshal.PyList); ok {
							for _, it := range lst.Items {
								if iv, ok2 := it.(marshal.PyInt); ok2 {
									itemIDs = append(itemIDs, int64(iv.Value))
								}
							}
						} else if iv, ok := argsTuple.Items[0].(marshal.PyInt); ok {
							itemIDs = append(itemIDs, int64(iv.Value))
						}
						ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
						if uerr := assembleRefusal(ctx, sqldb, *charID, *stationID, itemIDs); uerr != nil {
							cancel()
							log.Printf("[%s] ship.AssembleShip(inline): refused items=%v", addr, itemIDs)
							return server.SendCallException(c, pkt, *accountID, uerr)
						}
						charName := evedb.GetCharacterName(ctx, sqldb, *charID)
						for _, id := range itemIDs {
							// Default ship name is "<character>'s <type>".
							var typeName string
							if er, err := evedb.GetEntityRow(ctx, sqldb, id); err == nil && er != nil {
								typeName, _ = evedb.GetTypeName(ctx, sqldb, er.TypeID)
							}
							shipName := ""
							if charName != "" && typeName != "" {
								shipName = charName + "'s " + typeName
							}
							_, err := sqldb.ExecContext(ctx,
								`UPDATE entity SET singleton = 1, itemName = ? WHERE itemID = ?`, shipName, id)
							if err != nil {
								log.Printf("[%s] ship.AssembleShip(inline): update entity itemID=%d: %v", addr, id, err)
								continue
							}
							er, err := evedb.GetEntityRow(ctx, sqldb, id)
							if err == nil && er != nil {
								_ = server.SendNotification(c, *accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
									evedb.BuildEntityRow(er),
									marshal.PyDict{Items: []marshal.DictEntry{
										{Key: marshal.PyInt{Value: 6}, Value: marshal.PyInt{Value: 0}}, // ixSingleton old=0
									}},
								}})
							}
						}
						cancel()
						log.Printf("[%s] → ship.MachoBindObject(AssembleShip) items=%v", addr, itemIDs)
					}
				}
			}
			// godma's skill handler inlines its first call into the bind, so a fresh session's first
			// Train Skill arrives here. The training side effects run after the bind response.
			if pkt.Dest.Service == "skillMgr" && sqldb != nil && *charID != 0 {
				switch inlineBindMethod(cs) {
				case "CharStartTrainingSkill":
					if callTuple, ok := cs.ArgTuple.Items[1].(marshal.PyTuple); ok && len(callTuple.Items) >= 2 {
						if argsTuple, ok := callTuple.Items[1].(marshal.PyTuple); ok && len(argsTuple.Items) >= 1 {
							if skillIDRep, ok := argsTuple.Items[0].(marshal.PyInt); ok {
								skillItemID := int64(skillIDRep.Value)
								oldLoc, oldFlag := snapshotSkillItemState(sqldb, skillItemID)
								prevItemID := trainingSkillItem(sqldb, *charID)
								ctxT, cancelT := context.WithTimeout(context.Background(), 10*time.Second)
								endOfTraining, err := evedb.StartTrainingSkill(ctxT, sqldb, *charID, skillItemID)
								cancelT()
								if err != nil {
									// The bind response has gone out, so the refusal rides a notification with the same
									// message key.
									log.Printf("[%s] skillMgr.MachoBindObject(CharStartTrainingSkill) refused: %v", addr, err)
									if pe := evedb.AsSkillPrereqError(err); pe != nil {
										_ = server.SendNotification(c, *accountID, "OnRemoteMessage", "clientID",
											marshal.PyTuple{Items: []marshal.PyRep{
												marshal.PyString{Value: "SkillHasPrerequisites"},
												marshal.PyDict{Items: []marshal.DictEntry{{
													Key:   marshal.PyString{Value: "requiredSkills"},
													Value: marshal.PyString{Value: pe.Text},
												}}},
											}})
									}
								} else {
									log.Printf("[%s] → skillMgr.MachoBindObject(CharStartTrainingSkill) charID=%d skillItemID=%d", addr, *charID, skillItemID)
									notifySkillSwitchedOff(c, *accountID, prevItemID, skillItemID, sqldb)
									notifyArgs := marshal.PyTuple{Items: []marshal.PyRep{
										marshal.PyInt{Value: uint64(skillItemID)},
										marshal.PyInt{Value: endOfTraining},
									}}
									if nErr := server.SendNotification(c, *accountID, "OnSkillStartTraining", "charid", notifyArgs); nErr != nil {
										log.Printf("[%s] skillMgr inline start notify: %v", addr, nErr)
									}
									armSkillCompletionTimer(c, *accountID, *charID, endOfTraining, sqldb)
									sendSkillFlagChange(c, *accountID, skillItemID, oldFlag, oldLoc, sqldb)
								}
							}
						}
					}
				case "CharStopTrainingSkill":
					skReq := int64(0)
					if callTuple, ok := cs.ArgTuple.Items[1].(marshal.PyTuple); ok && len(callTuple.Items) >= 2 {
						if argsTuple, ok := callTuple.Items[1].(marshal.PyTuple); ok && len(argsTuple.Items) >= 1 {
							if r, ok := argsTuple.Items[0].(marshal.PyInt); ok {
								skReq = int64(r.Value)
							}
						}
					}
					ctxT, cancelT := context.WithTimeout(context.Background(), 10*time.Second)
					stoppedItemID, err := evedb.StopTrainingSkill(ctxT, sqldb, *charID, skReq)
					cancelT()
					if err != nil {
						log.Printf("[%s] skillMgr.MachoBindObject(CharStopTrainingSkill) db: %v", addr, err)
					}
					cancelSkillCompletionTimer(c, *charID)
					if stoppedItemID != 0 {
						notifyArgs := marshal.PyTuple{Items: []marshal.PyRep{
							marshal.PyInt{Value: uint64(stoppedItemID)},
							marshal.PyInt{Value: 0}, // silent=0 → show "training stopped"
						}}
						if nErr := server.SendNotification(c, *accountID, "OnSkillTrainingStopped", "charid", notifyArgs); nErr != nil {
							log.Printf("[%s] skillMgr inline stop notify: %v", addr, nErr)
						}
						log.Printf("[%s] → skillMgr.MachoBindObject(CharStopTrainingSkill) charID=%d skill %d stopped", addr, *charID, stoppedItemID)
						sendSkillFlagChange(c, *accountID, stoppedItemID, 61, 0, sqldb)
					}
				case "GetEndOfTraining":
					// The result went out inline; re-arm the completion timer so training completes after a
					// restart.
					ctxT, cancelT := context.WithTimeout(context.Background(), 5*time.Second)
					if st, err := evedb.GetSkillTraining(ctxT, sqldb, *charID); err == nil && st != nil {
						armSkillCompletionTimer(c, *accountID, *charID, st.EndOfTraining, sqldb)
					}
					cancelT()
				case "CharAddImplant", "RemoveImplantFromCharacter":
					if callTuple, ok := cs.ArgTuple.Items[1].(marshal.PyTuple); ok && len(callTuple.Items) >= 2 {
						if argsTuple, ok := callTuple.Items[1].(marshal.PyTuple); ok && len(argsTuple.Items) >= 1 {
							if itemIDRep, ok := argsTuple.Items[0].(marshal.PyInt); ok {
								// The bind answer has gone out, so a refusal is pushed as its own message box.
								var implErr string
								if inlineBindMethod(cs) == "CharAddImplant" {
									implErr = doCharAddImplant(c, addr, *accountID, *charID, *stationID, int64(itemIDRep.Value), sqldb)
								} else {
									implErr = doRemoveImplant(c, addr, *accountID, *charID, int64(itemIDRep.Value), sqldb)
								}
								if implErr != "" {
									pushRemoteMessage(c, *accountID, "CustomInfo", map[string]marshal.PyRep{
										"info": marshal.PyString{Value: implErr},
									})
								}
							}
						}
					}
				}
			}
			return nil
		}

		switch pkt.Dest.Service {
		case "authentication":
			err := account.Handle(c, addr, cs, pkt, *accountID)
			// This Ping is the client's eve.SynchronizeClock() after cfg.GotInitData; it arms the
			// release of the login session state. See docs/login-session-gate.md#release-rule.
			if cs.Method == "Ping" {
				if ds.loginPingQuietTimer == nil {
					ds.loginPingQuietTimer = time.AfterFunc(loginPingQuiet, ds.releasePendingLoginSession)
				} else {
					ds.loginPingQuietTimer.Reset(loginPingQuiet)
				}
			}
			return err
		case "character":
			prevCharID := *charID
			err := character.Handle(c, addr, cs, pkt, *accountID, charID, stationID, sqldb, cacheStore, cfg.Character.StartBalance, sessReg, stationReg)
			// After SelectCharacterID: register the connection and fill the ESP session.
			if err == nil && *charID != 0 && *charID != prevCharID {
				// charID to connection map for cross-player notifications (gang invites).
				if prevCharID != 0 {
					globalCharConns.Delete(prevCharID)
				}
				globalCharConns.Store(*charID, charConn{conn: c, accountID: *accountID})
				go broadcastOnlineStatus(sqldb, *charID, true)
				// A ship left in space by the last logoff leaves first, so the row read
				// below holds its final position (logofflinger.go).
				endLogoffLinger(*charID)
				ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
				cd, cdErr := evedb.LoadCharData(ctx2, sqldb, *charID)
				cancel2()
				if cdErr == nil {
					sessReg.Update(addr, func(s *esp.Session) {
						s.CharID = cd.CharacterID
						s.CharName = cd.CharacterName
						s.CorpID = cd.CorporationID
						s.StationID = cd.StationID
						s.SolarSystemID = cd.SolarSystemID
						s.LastActivity = time.Now()
					})
					// A character in space (no station) gets destiny initialised and registered here, so
					// GetFormations and warp commands find a live Entity. SetState is not sent: the client
					// has no ballpark yet. See docs/main-s8-notes.md#setstate-from-formations.
					if cd.StationID == 0 && cd.SolarSystemID != 0 {
						initDestiny(addr, *charID, 0, sqldb, ds)
						if ds.dm != nil && ds.sysID != 0 {
							ds.spaceEntry = entryLogin
							registerUndock(c, addr, uint64(*accountID), *charID, sqldb, ds)
							log.Printf("[%s] auto-registered in-space character charID=%d sysID=%d (SetState deferred to GetFormations)", addr, *charID, ds.sysID)
						}
					}
				}
				// Fallback for an in-space login the block above did not cover (ds.dm still nil).
				if *stationID == 0 && ds.dm == nil {
					initDestiny(addr, *charID, 0, sqldb, ds)
					ds.spaceEntry = entryLogin
					registerUndock(c, addr, uint64(*accountID), *charID, sqldb, ds)
					log.Printf("[%s] login-in-space: charID=%d initialised in system %d", addr, *charID, ds.sysID)
				}
			}
			return err
		case "charmgr":
			return charmgr.Handle(c, addr, cs, pkt, *accountID, *charID, sqldb, charIsOnline)
		case "corpmgr":
			return corpmgr.Handle(c, addr, cs, pkt, *accountID, *charID, sqldb)
		case "alert":
			return alert.Handle(c, addr, cs, pkt, *accountID)
		case "objectCaching":
			return objectcaching.Handle(c, addr, cs, pkt, *accountID, cacheStore, sqldb)
		case "config":
			return configsvc.Handle(c, addr, cs, pkt, *accountID, sqldb)
		case "petitioner":
			return petitioner.Handle(c, addr, cs, pkt, *accountID, *charID, sqldb)
		case "station":
			return station.Handle(c, addr, cs, pkt, *accountID, *stationID, sqldb, stationReg)
		case "stationSvc":
			return stationsvc.Handle(c, addr, cs, pkt, *accountID, sqldb)
		case "posMgr":
			return posmgr.Handle(c, addr, cs, pkt, *accountID, *charID, sqldb)
		case "LSC":
			return lsc.Handle(c, addr, cs, pkt, *accountID, *charID, lscReg, sqldb)
		case "onlineStatus":
			return handleOnlineStatus(c, addr, cs, pkt, *accountID, *charID, lscReg, sqldb)
		case "skillMgr":
			return handleSkillMgr(c, addr, cs, pkt, *accountID, *charID, *stationID, sqldb)
		case "standing2":
			return standing2.Handle(c, addr, cs, pkt, *accountID, *charID, sqldb)
		case "gangsta":
			return gangsta.Handle(c, addr, cs, pkt, *accountID, *charID, ds.corpID, globalGangReg,
				charConnLookup,
				func(cid int64) string { return charNameOf(sqldb, cid) },
				func(cid, gid, corp int64) {
					broadcastGangSysChange(sysMgr, cid, gid, corp)
					// svc.gang builds activeBeacon only from OnJumpBeaconChange pushes, so a pilot joining a
					// fleet after a field was lit has no "Jump to member" entry. Replay live fields on join.
					if gid != 0 {
						replayJumpBeaconsToChar(cid)
					}
				})
		case "map":
			return mapsvc.Handle(c, addr, cs, pkt, *accountID, mapCaller(*charID, ds.corpID), sqldb)
		case "dogmaIM":
			return dogmaim.Handle(c, addr, cs, pkt, *accountID, sqldb, cacheStore)
		case "invbroker":
			return handleInvbroker(c, addr, cs, pkt, *accountID, *charID, sqldb, ds)
		case "account":
			return handleAccount(c, addr, cs, pkt, *accountID, *charID, sqldb, sessReg)
		case "bookmark":
			return bookmark.Handle(c, addr, cs, pkt, *accountID, *charID, bookmarkLocation(ds, stationID), sqldb)
		case "voucher":
			return bookmark.HandleVoucher(c, addr, cs, pkt, *accountID, sqldb)
		case "billMgr":
			return handleBillMgrReal(c, addr, cs, pkt, *accountID, *charID, sqldb, sessReg)
		case "corporationSvc":
			return handleCorporationSvc(c, addr, cs, pkt, *accountID, *charID, sqldb)
		case "tutorialSvc":
			return handleTutorialSvc(c, addr, cs, pkt, *accountID, *charID, sqldb)
		case "slash":
			// The client hides slash commands only from roles outside ROLE_SLASH; a modified client
			// sends them anyway. This gate covers the intercepted commands and slash.Handle.
			rctx, rcancel := context.WithTimeout(context.Background(), 2*time.Second)
			maySlash := evedb.MaySlash(rctx, sqldb, *accountID)
			rcancel()
			if !maySlash {
				log.Printf("[%s] slash: refused for account %d (role lacks a slash bit)", addr, *accountID)
				return server.SendCallRsp(c, pkt, *accountID, marshal.PyString{Value: "You are not authorised to use slash commands."})
			}
			if isSpawnCmd(cs) {
				return handleNPCSpawn(c, addr, cs, pkt, *accountID, *charID, sqldb, ds, npcMgr)
			}
			if isSlashCmd(cs, "deadspace") {
				return handleDeadspace(c, addr, cs, pkt, *accountID, sqldb, ds)
			}
			if isSlashCmd(cs, "gateflash") {
				return handleGateFlash(c, addr, cs, pkt, *accountID, ds)
			}
			if isSlashCmd(cs, "concord") {
				return handleConcordTest(c, addr, pkt, *accountID, *charID, sqldb, ds, npcMgr)
			}
			if isSlashCmd(cs, "police") {
				return handlePoliceTest(c, addr, cs, pkt, *accountID, *charID, ds)
			}
			if isSlashCmd(cs, "secstatus") {
				return handleSecStatusSet(c, addr, cs, pkt, *accountID, *charID, sqldb, ds)
			}
			if isSlashCmd(cs, "customs") {
				return handleCustomsTest(c, addr, pkt, *accountID, *charID, sqldb, ds, npcMgr)
			}
			tp := &slashTeleporter{
				c: c, addr: addr, accountID: *accountID, charID: *charID,
				stationID: stationID, sqldb: sqldb, ds: ds, sysMgr: sysMgr,
			}
			return slash.Handle(c, addr, cs, pkt, *accountID, *charID, stationID, sqldb, tp, sessReg, stationReg, lscReg)
		case "beyonce":
			return handleBeyonce(c, addr, cs, pkt, *accountID, *charID, *stationID, sqldb, ds, sysMgr)
		case "shipSvc":
			return handleShipSvc(c, addr, cs, pkt, *accountID, *charID, stationID, sqldb, ds, sysMgr)
		case "marketProxy":
			return market.Handle(c, addr, cs, pkt, *accountID, *charID, *stationID, ds.sysID, sqldb, sessReg)
		case "marketBroker":
			switch cs.Method {
			case "GetMarketRegion":
				// svc.market caches this and never reads it.
				log.Printf("[%s] → marketBroker.GetMarketRegion (None, dead client path)", addr)
				return server.SendCallRsp(c, pkt, *accountID, marshal.PyNone{})
			default:
				server.RecordUnhandled(addr, "marketBroker", cs, false)
				return server.SendCallException(c, pkt, *accountID, nil)
			}
		case "netres":
			switch cs.Method {
			case "GetResource":
				// ProxySvc('netres').GetResource(path) returns raw file bytes the client writes to disk
				// or feeds to PIL. The server ships no netres: resources, so an empty buffer is the answer
				// (the audio path writes a 0-byte file; image paths raise client-side).
				log.Printf("[%s] netres.GetResource: no resource store — returning empty buffer", addr)
				return server.SendCallRsp(c, pkt, *accountID, marshal.PyBuffer{})
			default:
				server.RecordUnhandled(addr, "netres", cs, false)
				return server.SendCallException(c, pkt, *accountID, nil)
			}
		case "machoNet":
			return handleMachoNetSvc(c, addr, cs, pkt, *accountID, sessReg)
		case "benchmark":
			switch cs.Method {
			case "SaveSession":
				// Dev/QA performance-data dump; the client ignores the return.
				log.Printf("[%s] → benchmark.SaveSession (ack)", addr)
				return server.SendCallRsp(c, pkt, *accountID, marshal.PyNone{})
			default:
				server.RecordUnhandled(addr, "benchmark", cs, false)
				return server.SendCallException(c, pkt, *accountID, nil)
			}
		case "aggressionMgr":
			switch cs.Method {
			case "CheckLootRightExceptions":
				// Same contract as the bound-moniker form: False makes the client show its
				// ConfirmStealItem dialog.
				log.Printf("[%s] → aggressionMgr.CheckLootRightExceptions → False", addr)
				return server.SendCallRsp(c, pkt, *accountID, marshal.PyBool{Value: false})
			default:
				server.RecordUnhandled(addr, "aggressionMgr", cs, false)
				log.Printf("[%s] CallReq: unhandled aggressionMgr method=%q — sending exception", addr, cs.Method)
				return server.SendCallException(c, pkt, *accountID, nil)
			}
		case "assetSvc":
			return handleAssetSvc(c, addr, cs, pkt, *accountID, *charID, sqldb)
		case "corpRegistry":
			return handleCorpRegistry(c, addr, cs, pkt, *accountID)
		case "allianceRegistry":
			return handleAllianceRegistry(c, addr, cs, pkt, *accountID, sqldb)
		case "industryMgr":
			return handleIndustryMgr(c, addr, cs, pkt, *accountID, *charID, sqldb)
		case "factory":
			// The slot methods normally arrive on the bound factoryMgr; a direct call gets the same
			// answer.
			if researchSlotMethods[cs.Method] {
				return handleResearchSlotCall(c, addr, cs, pkt, *accountID, *charID, stationID, ds, sqldb, sessReg)
			}
			return handleFactory(c, addr, cs, pkt, *accountID, *charID, sqldb)
		case "ram", "ramProxy":
			// The S&I Installations tab works undocked, so pass the ship's region; the docked
			// station is 0 in space.
			ramRegionID := int64(0)
			if ds != nil && *stationID == 0 {
				ramRegionID = ds.regionID
			}
			return ram.Handle(c, addr, cs, pkt, *accountID, *charID, *stationID, ramRegionID, sqldb)
		case "agentMgr":
			return handleAgentMgr(c, addr, cs, pkt, *accountID, *charID, *stationID, agentBinds, sqldb)
		case "lookupSvc":
			return lookup.Handle(c, addr, cs, pkt, *accountID, sqldb)
		case "dungeon", "dungeonMgr":
			if cs.Method == "EditObject" {
				return handleDungeonEditObjectWithSync(c, addr, cs, pkt, *accountID, sqldb, ds)
			}
			return dungeon.Handle(c, addr, cs, pkt, *accountID, sqldb)
		case "keeper":
			if cs.Method == "ActivateAccelerationGate" {
				return handleActivateGate(c, addr, cs, pkt, *accountID, sqldb, ds, sessReg, npcMgr)
			}
			return keeper.Handle(c, addr, cs, pkt, *accountID)
		case "scenario":
			return handleScenario(c, addr, cs, pkt, *accountID, sqldb, ds, sessReg, npcMgr)
		case "languageSvc":
			return handleLanguageSvc(c, addr, cs, pkt, *accountID, cfg.ESP.ClientPath)
		case "missionMgr":
			return handleMissionMgr(c, addr, cs, pkt, *accountID, *charID, *stationID, sqldb, sessReg)
		case "insuranceSvc":
			return insurance.Handle(c, addr, cs, pkt, *accountID, *charID, *stationID, sqldb)
		case "reprocessingSvc":
			return reprocessing.Handle(c, addr, cs, pkt, *accountID, *charID, *stationID, sqldb)
		case "repairSvc":
			return repairsvc.Handle(c, addr, cs, pkt, *accountID, *charID, sqldb)
		case "trademgr":
			return trademgr.Handle(c, addr, cs, pkt, *accountID, *charID, *stationID, globalTradeReg, charConnLookup, sqldb)
		case "jumpCloneSvc",
			"corpStationMgr", "corporation", "lang":
			log.Printf("[%s] CallReq: stub service %q method=%q — returning None", addr, pkt.Dest.Service, cs.Method)
			rpccov.Record(addr, pkt.Dest.Service, cs.Method, rpccov.Stub)
			return server.SendCallRsp(c, pkt, *accountID, marshal.PyNone{})
		case "":
			// Bound-object calls carry an empty service name; the method name identifies the call
			// (remoteObjectStr in C++), so dispatch is by method name.
			return handleBoundCall(&callCtx{c: c, addr: addr, cs: cs, pkt: pkt, accountID: *accountID, charID: *charID,
				stationID: stationID, agentBinds: agentBinds, sqldb: sqldb, ds: ds, sysMgr: sysMgr, sessReg: sessReg, npcMgr: npcMgr})
		default:
			log.Printf("[%s] CallReq: unhandled service %q method=%q — sending exception", addr, pkt.Dest.Service, cs.Method)
			rpccov.Record(addr, pkt.Dest.Service, cs.Method, rpccov.Unhandled)
			return server.SendCallException(c, pkt, *accountID, nil)
		}

	default:
		log.Printf("[%s] ← unhandled packet type=%s", addr, msgTypeName(pkt.Type))
		return nil
	}
}

// handleMachoNetSvc handles direct CallReqs to the "machoNet" service.
// (MachoResolveObject / MachoBindObject are intercepted before the service
// switch and never reach here.)
func handleMachoNetSvc(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sessReg *esp.SessionRegistry) error {
	switch cs.Method {
	case "GetClusterSessionStatistics":
		// map.py star-mode colouring unpacks (sol, sta, statDivisor): dicts keyed by
		// solarSystemID-30000000 and a nonzero divisor (0 divides by zero on the first non-empty
		// system). sol counts pilots in space, sta pilots docked.
		sol := marshal.PyDict{}
		sta := marshal.PyDict{}
		solCount := map[int64]int64{}
		staCount := map[int64]int64{}
		if sessReg != nil {
			for _, s := range sessReg.Snapshot() {
				if s.CharID == 0 || s.SolarSystemID == 0 {
					continue
				}
				key := s.SolarSystemID - 30000000
				if s.StationID != 0 {
					staCount[key]++
					solCount[key] += 0 // ensure the key exists; the client iterates sol's keys
				} else {
					solCount[key]++
				}
			}
		}
		for k, v := range solCount {
			sol.Items = append(sol.Items, marshal.DictEntry{
				Key: marshal.PyInt{Value: uint64(k)}, Value: marshal.PyInt{Value: uint64(v)}})
		}
		for k, v := range staCount {
			sta.Items = append(sta.Items, marshal.DictEntry{
				Key: marshal.PyInt{Value: uint64(k)}, Value: marshal.PyInt{Value: uint64(v)}})
		}
		log.Printf("[%s] → machoNet.GetClusterSessionStatistics (%d systems)", addr, len(solCount))
		return server.SendCallRsp(c, pkt, accountID, marshal.PyTuple{Items: []marshal.PyRep{
			sol, sta, marshal.PyInt{Value: 1},
		}})
	case "multiHostCluster":
		// browserutil asks whether this is a multi-host cluster: no.
		log.Printf("[%s] → machoNet.multiHostCluster → False", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyBool{Value: false})
	default:
		server.RecordUnhandled(addr, "machoNet", cs, false)
		log.Printf("[%s] CallReq: unhandled machoNet method=%q — sending exception", addr, cs.Method)
		return server.SendCallException(c, pkt, accountID, nil)
	}
}

// handleLanguageSvc handles calls to the "languageSvc" service. GetTextsForGroup
// (slides.py) must return a util.Rowset, not None: the client reads .header and .lines.
func handleLanguageSvc(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, clientPath string) error {
	var result marshal.PyRep
	switch cs.Method {
	case "GetTextsForGroup":
		// slides.py builds util.IndexRowset(rows.header, rows.lines, 'textLabel') from this. The
		// rows are the intro subtitles read from the client's introMovie.txt (introsubs.go);
		// with no client path or file the rowset is empty and the intro plays without subtitles.
		lines := loadIntroSubtitleRows(clientPath)
		cols := []string{"textLabel", "text"}
		header := make([]marshal.PyRep, len(cols))
		for i, col := range cols {
			header[i] = marshal.PyString{Value: col}
		}
		headerList := marshal.PyList{Items: header}
		result = marshal.PyObject{
			Type: "util.Rowset",
			Args: marshal.PyDict{Items: []marshal.DictEntry{
				{Key: marshal.PyString{Value: "header"}, Value: headerList},
				{Key: marshal.PyString{Value: "RowClass"}, Value: marshal.PyString{Value: "util.Row", IsType1: true}},
				{Key: marshal.PyString{Value: "lines"}, Value: marshal.PyList{Items: lines}},
				{Key: marshal.PyString{Value: "columns"}, Value: headerList},
			}},
		}
		log.Printf("[%s] → languageSvc.GetTextsForGroup (%d subtitle rows)", addr, len(lines))
	case "GetMessagesByLanguageID":
		// mlsUtil.LoadLanguagePack stores this and TR() falls back to the built-in text for a
		// missing (colID, keyID), so an empty dict yields English.
		result = marshal.PyDict{}
		log.Printf("[%s] → languageSvc.GetMessagesByLanguageID (empty dict)", addr)
	case "GetLanguages":
		// gameui.py: list of [languageID, languageName, translatedName].
		result = marshal.PyList{Items: []marshal.PyRep{
			marshal.PyList{Items: []marshal.PyRep{
				marshal.PyString{Value: "EN"}, marshal.PyString{Value: "English"}, marshal.PyString{Value: "English"},
			}},
		}}
		log.Printf("[%s] → languageSvc.GetLanguages", addr)
	default:
		log.Printf("[%s] → languageSvc.%s (stub → None)", addr, cs.Method)
		result = marshal.PyNone{}
	}
	return server.SendCallRsp(c, pkt, accountID, result)
}

// handleAgentMgr handles calls to the "agentMgr" service. GetAgents returns the agtAgents
// table for agents.py:__GetAllAgents, which fills the client's agent cache.
func handleAgentMgr(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID, stationID int64, agentBinds *agentBindState, sqldb *sql.DB) error {
	var result marshal.PyRep
	switch cs.Method {
	case "GetAgents":
		ctx := context.Background()
		rowset, err := evedb.GetAgents(ctx, sqldb)
		if err != nil {
			log.Printf("[%s] agentMgr.GetAgents DB error: %v", addr, err)
			return server.SendCallException(c, pkt, accountID, nil)
		}
		result = rowset
		log.Printf("[%s] → agentMgr.GetAgents", addr)

	case "GetMyJournalDetails":
		// Tuple(3): missions, offers, research lists. journal.py iterates them; None raises
		// TypeError.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		result = buildJournalDetails(ctx, sqldb, charID, 0)
		log.Printf("[%s] → agentMgr.GetMyJournalDetails charID=%d", addr, charID)

	case "DoAction":
		// Some client builds call DoAction on the service instead of the bound object; the
		// agentID is then the first argument.
		agentID := int64(0)
		actionID := 0
		actionText := ""
		if len(cs.ArgTuple.Items) >= 1 {
			if aid, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
				agentID = int64(aid.Value)
			}
		}
		if len(cs.ArgTuple.Items) >= 2 {
			switch v := cs.ArgTuple.Items[1].(type) {
			case marshal.PyInt:
				actionID = int(v.Value)
			case marshal.PyString:
				// Text typed into the conversation's name-entry form; the client passes non-numeric
				// input as str.
				actionText = v.Value
			}
		}
		// Without an agentID in the args, resolve it from this connection's agent binds.
		if agentID == 0 {
			agentID = agentBinds.resolve(cs)
		}
		log.Printf("[%s] → agentMgr.DoAction agentID=%d actionID=%d text=%q charID=%d", addr, agentID, actionID, actionText, charID)
		result = handleAgentDoAction(c, accountID, addr, agentID, charID, stationID, actionID, actionText, sqldb)

	case "ResetTutorial":
		// systemmenu.py Reset button for the agent tutorial; the client discards the return.
		// Clears the character's tutorial bookkeeping.
		if charID != 0 && sqldb != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if _, err := sqldb.ExecContext(ctx, `DELETE FROM chrTutorials WHERE characterID = ?`, charID); err != nil {
				log.Printf("[%s] agentMgr.ResetTutorial: %v", addr, err)
			}
			if _, err := sqldb.ExecContext(ctx, `DELETE FROM chrTutorialDisabled WHERE characterID = ?`, charID); err != nil {
				log.Printf("[%s] agentMgr.ResetTutorial re-enable: %v", addr, err)
			}
			cancel()
		}
		log.Printf("[%s] → agentMgr.ResetTutorial charID=%d", addr, charID)
		result = marshal.PyNone{}

	case "GetSolarSystemOfAgent":
		agentID := int64(0)
		if len(cs.ArgTuple.Items) >= 1 {
			if aid, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
				agentID = int64(aid.Value)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		agent, err := evedb.GetAgentInfo(ctx, sqldb, agentID)
		if err != nil {
			log.Printf("[%s] agentMgr.GetSolarSystemOfAgent agentID=%d: %v", addr, agentID, err)
			result = marshal.PyNone{}
		} else {
			result = marshal.PyInt{Value: uint64(agent.SolarSystemID)}
		}
		log.Printf("[%s] → agentMgr.GetSolarSystemOfAgent agentID=%d", addr, agentID)

	case "GetMissionJournalInfo":
		// journal.py Read Details on an accepted mission; agentID is the first argument.
		agentID := int64(0)
		if len(cs.ArgTuple.Items) >= 1 {
			if aid, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
				agentID = int64(aid.Value)
			}
		}
		if agentID == 0 {
			agentID = agentBinds.resolve(cs)
		}
		log.Printf("[%s] → agentMgr.GetMissionJournalInfo agentID=%d charID=%d", addr, agentID, charID)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		mission, err := evedb.GetActiveMission(ctx, sqldb, charID, agentID)
		switch {
		case err != nil || mission == nil:
			result = marshal.PyNone{}
		case mission.State == "accepted":
			tmpl, _ := evedb.GetTemplateByID(ctx, sqldb, mission.TemplateID)
			agent, _ := evedb.GetAgentInfo(ctx, sqldb, agentID)
			result = marshal.PyString{Value: buildMissionJournalHTML(ctx, sqldb, charID, mission, tmpl, agent)}
		case mission.State == "offered":
			// The journal lists offered missions as Not Accepted and Read Details asks here, so
			// serve the offer HTML.
			tmpl, _ := evedb.GetTemplateByID(ctx, sqldb, mission.TemplateID)
			agent, _ := evedb.GetAgentInfo(ctx, sqldb, agentID)
			result = marshal.PyString{Value: buildOfferJournalHTML(ctx, sqldb, charID, mission, tmpl, agent)}
		default:
			result = marshal.PyNone{}
		}

	case "GetOfferJournalInfo":
		// journal.py Read Details on an offered mission; agentID is the first argument.
		agentID := int64(0)
		if len(cs.ArgTuple.Items) >= 1 {
			if aid, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
				agentID = int64(aid.Value)
			}
		}
		if agentID == 0 {
			agentID = agentBinds.resolve(cs)
		}
		log.Printf("[%s] → agentMgr.GetOfferJournalInfo agentID=%d charID=%d", addr, agentID, charID)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		result = offerJournalInfoRep(ctx, sqldb, charID, agentID)

	default:
		log.Printf("[%s] CallReq: stub agentMgr method=%q — returning None", addr, cs.Method)
		result = marshal.PyNone{}
	}
	return server.SendCallRsp(c, pkt, accountID, result)
}

// buildAgentInfoServiceDetails answers agentMgr.GetInfoServiceDetails for the info
// window's Agent Info section (infosvc.py): a util.KeyVal of stationID, level, quality,
// effectiveQuality, loyaltyPoints, services and incompatible. Non-agent NPCs get None and
// the client skips the section.
func buildAgentInfoServiceDetails(addr string, sqldb *sql.DB, agentID, charID int64) marshal.PyRep {
	if agentID == 0 || sqldb == nil {
		return marshal.PyNone{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	agent, err := evedb.GetAgentInfo(ctx, sqldb, agentID)
	if err != nil {
		log.Printf("[%s] GetInfoServiceDetails agentID=%d: %v", addr, agentID, err)
		return marshal.PyNone{}
	}
	lp := 0
	var incompatible marshal.PyRep = marshal.PyNone{}
	if charID != 0 {
		lp, _ = evedb.GetLoyaltyPoints(ctx, sqldb, charID, agentID)
		// details.incompatible is the text infosvc.py shows when the agent will not work with
		// the character.
		if chk := evedb.CanUseAgent(ctx, sqldb, charID, agent); !chk.Allowed {
			if chk.Minimum <= -2.0 {
				incompatible = marshal.PyString{Value: "This agent refuses to deal with you because of your bad reputation."}
			} else {
				incompatible = marshal.PyString{Value: fmt.Sprintf(
					"This agent requires an effective standing of %.1f toward the agent, their corporation or faction; yours is %.1f.",
					chk.Required, chk.Effective)}
			}
		}
	}
	// details.services: research sections first, then the side-service blocks; the client
	// draws each (header, rows) section in list order.
	var serviceSections []marshal.PyRep
	if research, ok := buildAgentResearchServices(ctx, sqldb, agent, charID).(marshal.PyList); ok {
		serviceSections = append(serviceSections, research.Items...)
	}
	serviceSections = append(serviceSections, buildAgentServiceSections(ctx, sqldb, agent, charID)...)

	return marshal.PyObject{
		Type: "util.KeyVal",
		Args: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "stationID"}, Value: marshal.PyInt{Value: uint64(agent.StationID)}},
			{Key: marshal.PyString{Value: "level"}, Value: marshal.PyInt{Value: uint64(agent.Level)}},
			{Key: marshal.PyString{Value: "quality"}, Value: marshal.PyInt{Value: uint64(agent.Quality)}},
			// Served flat; connection and negotiation skills and standings do not modify it.
			{Key: marshal.PyString{Value: "effectiveQuality"}, Value: marshal.PyInt{Value: uint64(agent.Quality)}},
			{Key: marshal.PyString{Value: "loyaltyPoints"}, Value: marshal.PyInt{Value: uint64(lp)}},
			{Key: marshal.PyString{Value: "services"}, Value: marshal.PyList{Items: serviceSections}},
			{Key: marshal.PyString{Value: "incompatible"}, Value: incompatible},
		}},
	}
}

// buildAgentResearchServices builds details.services for R&D agents: (header, [(label,
// text), ...]) sections for infosvc.py's Agent Info tab (Research Services, Your Research,
// one Research Summary per field). Non-research agents get an empty list, which the
// client skips.
func buildAgentResearchServices(ctx context.Context, sqldb *sql.DB, agent *evedb.AgentInfo, charID int64) marshal.PyRep {
	if agent.AgentTypeID != evedb.AgentTypeResearch {
		return marshal.PyList{}
	}

	type fieldInfo struct {
		typeID int64
		name   string
	}
	var fields []fieldInfo
	for _, f := range agentResearchFieldsFor(ctx, sqldb, agent) {
		name, _ := evedb.GetTypeName(ctx, sqldb, f)
		if name == "" {
			continue
		}
		fields = append(fields, fieldInfo{typeID: f, name: name})
	}
	sort.Slice(fields, func(i, j int) bool { return fields[i].name < fields[j].name })
	if len(fields) == 0 {
		return marshal.PyList{}
	}

	row := func(label, text string) marshal.PyRep {
		return marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyString{Value: label},
			marshal.PyString{Value: text},
		}}
	}
	section := func(header string, rows ...marshal.PyRep) marshal.PyRep {
		return marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyString{Value: header},
			marshal.PyList{Items: rows},
		}}
	}

	var services []marshal.PyRep

	// "Research Services": the character's training in each field; these skills set the
	// collaboration rate.
	var skillParts []string
	for _, f := range fields {
		lvl := 0
		if charID != 0 {
			lvl, _ = evedb.GetCharacterSkillLevel(ctx, sqldb, charID, f.typeID)
		}
		if lvl > 0 {
			skillParts = append(skillParts, fmt.Sprintf("%s level %d", f.name, lvl))
		} else {
			skillParts = append(skillParts, fmt.Sprintf("%s (not trained)", f.name))
		}
	}
	services = append(services, section("Research Services",
		row("Relevant Skills", strings.Join(skillParts, ", "))))

	// "Your Research": active project with this agent, if any.
	if charID != 0 {
		if r, _ := evedb.GetCharacterResearchForAgent(ctx, sqldb, charID, agent.AgentID); r != nil {
			fieldName, _ := evedb.GetTypeName(ctx, sqldb, r.SkillTypeID)
			services = append(services, section("Your Research",
				row("Research Field", fieldName),
				row("Current Status", fmt.Sprintf("%.2f RP", r.CurrentPoints(time.Now()))),
				row("Research Rate", fmt.Sprintf("%.2f RP/day", r.PointsPerDay))))
		}
	}

	// One summary per field listing the Tech II blueprints its patent pool can award. A field
	// with no mapped pool has nothing to predict.
	for _, f := range fields {
		text := "Nothing easily predictable."
		if bps, _ := evedb.GetFieldBlueprints(ctx, sqldb, f.typeID); len(bps) > 0 {
			var names []string
			for _, bp := range bps {
				if n, _ := evedb.GetTypeName(ctx, sqldb, bp); n != "" {
					names = append(names, n)
				}
			}
			if len(names) > 0 {
				text = strings.Join(names, ", ")
			}
		}
		services = append(services, section(f.name+" Research Summary",
			row("Predictable Patents", text)))
	}

	return marshal.PyList{Items: services}
}

// agentPortraitClear is the blank-line run that pushes agentSays text past the bottom of
// the floated agent portrait. See docs/main-s2-notes.md#agent-portrait-clear.
const agentPortraitClear = "<br><br><br><br><br><br><br><br>"

// agentConversation builds the ((agentSays, dialogue), xtraInfo) response.
// agentSays is a plain string (not a tuple); the client renders it with %(agentSays)s.
func agentConversation(text string, buttons []marshal.PyRep, xtraInfo marshal.PyDict) marshal.PyRep {
	return marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyString{Value: agentPortraitClear + text},
			marshal.PyList{Items: buttons},
		}},
		xtraInfo,
	}}
}

// Agent dialog action IDs. The client formats them with %d in href links, so they must be
// integers; DoAction echoes them back.
const (
	agentActionRequestMission = 1
	agentActionAccept         = 2
	agentActionDecline        = 3
	agentActionComplete       = 4
	agentActionQuit           = 5
	agentActionViewMission    = 6
	agentActionQuitMission    = 7
	agentActionViewOffers     = 8
	agentActionMoreInfo       = 9

	// R&D agent (agentTypeID 4) research actions.
	agentActionViewResearch   = 20
	agentActionCancelResearch = 21

	// Patent actions use fixed IDs: a character holds at most one pending patent per agent. A value
	// above 1000/2000 collides with real agtOffers rows and above 100000 with the research base.
	agentActionViewPatent   = 22
	agentActionAcceptPatent = 23

	// Per-offer actions encode the offerID above these bases:
	// 1000+offerID = view offer detail, 2000+offerID = accept offer.
	agentActionOfferViewBase   = 1000
	agentActionOfferAcceptBase = 2000

	// Start-research actions encode the field skill typeID above this base
	// (field typeIDs are ~11433-11858). Checked before the offer bases.
	agentActionStartResearchBase = 100000
)

// agentButton builds a (actionID, caption) button tuple for agent dialog.
// The client unpacks as each[0]=actionID (int), each[1]=caption (string).
func agentButton(caption string, actionID int) marshal.PyRep {
	return marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyInt{Value: uint64(actionID)},
		marshal.PyString{Value: caption},
	}}
}

// missionButton is agentButton with an authored caption override. An empty override uses the
// generic caption.
func missionButton(custom, generic string, actionID int) marshal.PyRep {
	if c := strings.TrimSpace(custom); c != "" {
		generic = c
	}
	return agentButton(generic, actionID)
}

// missionOfferButtons is the choice set beside an offered mission. A nil tmpl (template since
// deleted) gets generic captions and no More Information. Generic captions come from the
// per-division dialogue table (GetAgentText).
func missionOfferButtons(ctx context.Context, sqldb *sql.DB, agent *evedb.AgentInfo, tmpl *evedb.MissionTemplate) []marshal.PyRep {
	var accept, decline, view, more string
	if tmpl != nil {
		accept, decline, view, more = tmpl.AcceptLabel, tmpl.DeclineLabel, tmpl.ViewMissionLabel, tmpl.MoreInfoLabel
	}
	div := int64(0)
	if agent != nil {
		div = agent.DivisionID
	}
	buttons := []marshal.PyRep{
		missionButton(accept, evedb.GetAgentText(ctx, sqldb, div, "shell.btn.accept"), agentActionAccept),
		missionButton(view, evedb.GetAgentText(ctx, sqldb, div, "shell.btn.viewmission"), agentActionViewMission),
		missionButton(decline, evedb.GetAgentText(ctx, sqldb, div, "shell.btn.decline"), agentActionDecline),
	}
	if tmpl != nil && tmpl.MoreInfoText != "" {
		buttons = append(buttons, missionButton(more, evedb.GetAgentText(ctx, sqldb, div, "shell.btn.moreinfo"), agentActionMoreInfo))
	}
	return buttons
}

// missionActiveButtons is the choice set shown beside an accepted mission.
func missionActiveButtons(ctx context.Context, sqldb *sql.DB, agent *evedb.AgentInfo, tmpl *evedb.MissionTemplate) []marshal.PyRep {
	var view, complete, quitMission, more string
	if tmpl != nil {
		view, complete, quitMission, more = tmpl.ViewMissionLabel, tmpl.CompleteLabel, tmpl.QuitMissionLabel, tmpl.MoreInfoLabel
	}
	div := int64(0)
	if agent != nil {
		div = agent.DivisionID
	}
	buttons := []marshal.PyRep{
		missionButton(view, evedb.GetAgentText(ctx, sqldb, div, "shell.btn.viewmission"), agentActionViewMission),
		missionButton(complete, evedb.GetAgentText(ctx, sqldb, div, "shell.btn.complete"), agentActionComplete),
		missionButton(quitMission, evedb.GetAgentText(ctx, sqldb, div, "shell.btn.quitmission"), agentActionQuitMission),
		shellBtn(ctx, sqldb, agent, "shell.btn.quit", agentActionQuit),
	}
	if tmpl != nil && tmpl.MoreInfoText != "" {
		buttons = append(buttons, missionButton(more, evedb.GetAgentText(ctx, sqldb, div, "shell.btn.moreinfo"), agentActionMoreInfo))
	}
	return buttons
}

// handleAgentDoAction implements the mission state machine for DoAction calls. agentID comes
// from the MachoBindObject first arg (agentBindState).
// The conversation window posts every non-initial state through a GET form and the client passes
// non-numeric input values as strings, so a typed locator name arrives in actionText. Exactly one
// of actionID and actionText is set.
func handleAgentDoAction(c *evenet.Conn, accountID int64, addr string, agentID, charID, stationID int64, actionID int, actionText string, sqldb *sql.DB) marshal.PyRep {
	if agentID == 0 || charID == 0 {
		return agentConversation("I have no missions available for you at this time.",
			nil, agentXtraInfo(agentID, 0, 0, 0))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// menusvc.py routes every NPC through agents.InteractWith, so lore NPCs missing from agtAgents
	// reach this call and get a not-an-agent reply.
	agent, err := evedb.GetAgentInfo(ctx, sqldb, agentID)
	if err != nil {
		log.Printf("[%s] DoAction: GetAgentInfo agentID=%d: %v", addr, agentID, err)
		return agentConversation("I am not an agent of any corporation — you must be mistaking me for someone else. I have no business with you.",
			nil, marshal.PyDict{})
	}

	lp, _ := evedb.GetLoyaltyPoints(ctx, sqldb, charID, agentID)

	// Same formula as the lobby's cfg.py util.CanUseAgent, so refusals match the willing and
	// not-willing split the client shows.
	if chk := evedb.CanUseAgent(ctx, sqldb, charID, agent); !chk.Allowed {
		log.Printf("[%s] DoAction: agent %d refuses charID=%d (level=%d required=%.2f effective=%.2f min=%.2f)",
			addr, agentID, charID, agent.Level, chk.Required, chk.Effective, chk.Minimum)
		if chk.Minimum <= -2.0 {
			return agentConversation(
				serviceText(ctx, sqldb, agent.DivisionID, "shell.standing.criminal", nil),
				nil, agentXtraInfo(agentID, 0, 0, lp))
		}
		return agentConversation(
			serviceText(ctx, sqldb, agent.DivisionID, "shell.standing.low", map[string]string{
				"required":  fmt.Sprintf("%.1f", chk.Required),
				"effective": fmt.Sprintf("%.1f", chk.Effective),
			}),
			nil, agentXtraInfo(agentID, 0, 0, lp))
	}

	// People & Places builds its Agents tab from the contact list (lscengine.GetBuddies and
	// util.IsNPC), so a willing agent is added to it.
	if added, err := evedb.AddAddressBookContact(ctx, sqldb, charID, agentID); err != nil {
		log.Printf("[%s] DoAction: AddAddressBookContact charID=%d agentID=%d: %v", addr, charID, agentID, err)
	} else if added {
		if err := lsc.SendAddressBookContact(c, accountID, charID, agentID); err != nil {
			log.Printf("[%s] DoAction: SendAddressBookContact: %v", addr, err)
		}
		log.Printf("[%s] → addressbook: agent %d added to charID=%d contacts", addr, agentID, charID)
	}

	// Agent services (locator, sec status repair, stubs), including a typed string while a locator
	// prompt is open, run before the mission machine.
	if isAgentServiceAction(actionID, actionText) {
		return handleAgentServiceAction(ctx, c, accountID, addr, agent, charID, actionID, actionText, lp, sqldb)
	}

	mission, err := evedb.GetActiveMission(ctx, sqldb, charID, agentID)
	if err != nil {
		log.Printf("[%s] DoAction: GetActiveMission: %v", addr, err)
	}

	// Research actions sit above the offer bases, so they are tested first.
	if actionID >= agentActionStartResearchBase {
		return agentStartResearch(ctx, c, accountID, addr, agent, charID, int64(actionID-agentActionStartResearchBase), lp, sqldb)
	}

	if actionID >= agentActionOfferAcceptBase {
		return agentOfferAccept(ctx, c, accountID, addr, agent, charID, stationID, lp, int64(actionID-agentActionOfferAcceptBase), sqldb)
	}
	if actionID >= agentActionOfferViewBase {
		return agentOfferView(ctx, sqldb, agent, charID, lp, int64(actionID-agentActionOfferViewBase))
	}

	switch actionID {
	case agentActionViewOffers:
		return agentOffersMenu(ctx, sqldb, agent, charID, lp)

	case agentActionViewResearch:
		return agentResearchMenu(ctx, addr, sqldb, agent, charID, lp, "")

	case agentActionViewPatent:
		return agentPatentView(ctx, addr, sqldb, agent, charID, lp)

	case agentActionAcceptPatent:
		return agentPatentAccept(ctx, c, accountID, addr, agent, charID, stationID, lp, sqldb)

	case agentActionCancelResearch:
		r, _ := evedb.GetCharacterResearchForAgent(ctx, sqldb, charID, agentID)
		if r == nil {
			return agentConversation("You have no research project with me to cancel.",
				[]marshal.PyRep{shellBtn(ctx, sqldb, agent, "shell.btn.quit", agentActionQuit)},
				agentXtraInfo(agentID, 0, 0, lp))
		}
		fieldName, _ := evedb.GetTypeName(ctx, sqldb, r.SkillTypeID)
		if err := evedb.StopResearch(ctx, sqldb, charID, agentID); err != nil {
			log.Printf("[%s] DoAction CancelResearch: %v", addr, err)
		}
		log.Printf("[%s] → DoAction CancelResearch charID=%d agentID=%d field=%d forfeited=%.2f RP",
			addr, charID, agentID, r.SkillTypeID, r.CurrentPoints(time.Now()))
		notifyJournalInvalidate(c, accountID, "research_cancelled")
		return agentConversation(
			fmt.Sprintf("Very well, I have terminated our joint %s research project. All accumulated research points are forfeit.", fieldName),
			[]marshal.PyRep{
				agentButton("View Research Options", agentActionViewResearch),
				shellBtn(ctx, sqldb, agent, "shell.btn.quit", agentActionQuit),
			},
			agentXtraInfo(agentID, 0, 0, lp))

	case agentActionMoreInfo:
		if mission == nil || (mission.State != "offered" && mission.State != "accepted") {
			return agentConversation("There is nothing I can tell you more about at this time.",
				[]marshal.PyRep{shellBtn(ctx, sqldb, agent, "shell.btn.requestmission", agentActionRequestMission)},
				agentXtraInfo(agentID, 0, 0, lp))
		}
		tmpl, _ := evedb.GetTemplateByID(ctx, sqldb, mission.TemplateID)
		text := "I have nothing further to add — the briefing covers everything you need to know."
		if tmpl != nil && tmpl.MoreInfoText != "" {
			text = tmpl.MoreInfoText
		}
		mtp := evedb.BuildMissionTextParams(ctx, sqldb, charID, agentID, agent, tmpl, mission)
		text = evedb.SubstituteMissionText(text, mtp)
		var buttons []marshal.PyRep
		if mission.State == "offered" {
			buttons = missionOfferButtons(ctx, sqldb, agent, tmpl)
		} else {
			buttons = missionActiveButtons(ctx, sqldb, agent, tmpl)
		}
		return agentConversation(text, buttons, agentMissionXtraInfo(mission, lp))
	case agentActionRequestMission:
		if mission != nil {
			return showActiveMission(ctx, addr, mission, agent, lp, sqldb)
		}
		if evedb.IsStorylineAgentType(agent.AgentTypeID) && !storylineInvitePending(ctx, sqldb, charID, agentID) {
			return storylineBrushOff(ctx, sqldb, agent, lp,
				len(availableOffersForChar(ctx, sqldb, charID, agentID, agent.CorporationID, agent.DivisionID)) > 0)
		}
		// Mission cap per rolling window; storyline agents are exempt above.
		// See docs/agent-services.md#mission-cap--cooldown.
		if onCD, availableAt, err := evedb.AgentMissionCooldown(ctx, sqldb, charID, agentID); err != nil {
			log.Printf("[%s] RequestMission: AgentMissionCooldown: %v", addr, err)
		} else if onCD {
			log.Printf("[%s] → RequestMission: charID=%d on cooldown with agent %d until %s",
				addr, charID, agentID, availableAt.Format("2006-01-02 15:04"))
			buttons := []marshal.PyRep{shellBtn(ctx, sqldb, agent, "shell.btn.quit", agentActionQuit)}
			if len(availableOffersForChar(ctx, sqldb, charID, agentID, agent.CorporationID, agent.DivisionID)) > 0 {
				buttons = append([]marshal.PyRep{shellBtn(ctx, sqldb, agent, "shell.btn.viewoffers", agentActionViewOffers)}, buttons...)
			}
			return agentConversation(serviceText(ctx, sqldb, agent.DivisionID, "shell.cooldown", map[string]string{
				"pilot":     evedb.GetCharacterName(ctx, sqldb, charID),
				"available": availableAt.Format("2006.01.02 15:04:05"),
				"cap":       strconv.Itoa(evedb.GetAgentMissionCap(ctx, sqldb)),
			}), buttons, agentXtraInfo(agentID, 0, 0, lp))
		}
		result := generateMissionOffer(ctx, addr, agent, charID, lp, sqldb)
		if m2, err := evedb.GetActiveMission(ctx, sqldb, charID, agentID); err == nil && m2 != nil && m2.State == "offered" {
			notifyAgentMissionChange(c, accountID, "offered", agentID)
		}
		return result

	case agentActionAccept:
		if mission == nil || mission.State != "offered" {
			return agentConversation("There is no pending offer to accept.",
				[]marshal.PyRep{shellBtn(ctx, sqldb, agent, "shell.btn.requestmission", agentActionRequestMission)},
				agentXtraInfo(agentID, 0, 0, lp))
		}
		tmpl, _ := evedb.GetTemplateByID(ctx, sqldb, mission.TemplateID)
		bonusTime, expiryDays := 0, evedb.MissionDefaultExpiryDays()
		if tmpl != nil {
			bonusTime, expiryDays = tmpl.BonusTimeSec, tmpl.ExpiryDays
		}
		if err := evedb.AcceptMission(ctx, sqldb, mission.MissionID, bonusTime, expiryDays); err != nil {
			log.Printf("[%s] DoAction Accept: %v", addr, err)
		}
		missionID := mission.MissionID
		mission, _ = evedb.GetMissionByID(ctx, sqldb, missionID)
		if mission == nil {
			log.Printf("[%s] DoAction Accept: GetMissionByID(%d) returned nil after accept", addr, missionID)
			return agentConversation("Mission accepted. Good luck out there.",
				[]marshal.PyRep{shellBtn(ctx, sqldb, agent, "shell.btn.quit", agentActionQuit)},
				agentXtraInfo(agentID, 0, 0, lp))
		}

		if tmpl != nil && tmpl.MissionType == "courier" && tmpl.CourierTypeID > 0 {
			pickupStn := mission.PickupStationID
			if pickupStn > 0 {
				if _, err := spawnMissionItem(ctx, c, accountID, charID, pickupStn, stationID, tmpl.CourierTypeID, tmpl.CourierQty, sqldb); err != nil {
					log.Printf("[%s] DoAction Accept: courier item spawn at pickup=%d: %v", addr, pickupStn, err)
				} else {
					log.Printf("[%s] DoAction Accept: spawned %d x typeID=%d at station=%d", addr, tmpl.CourierQty, tmpl.CourierTypeID, pickupStn)
				}
			} else {
				log.Printf("[%s] DoAction Accept: courier mission but pickupStationID=0, no items spawned", addr)
			}
		}

		if tmpl != nil && tmpl.MissionType == "kill" && tmpl.DungeonVID > 0 && mission.DestSystemID > 0 {
			roomIDs, rerr := evedb.GetDungeonRoomIDs(ctx, sqldb, tmpl.DungeonVID)
			if rerr == nil && len(roomIDs) > 0 {
				idx := tmpl.DungeonRoomIdx
				if idx < 1 || idx > len(roomIDs) {
					idx = 1
				}
				roomID := roomIDs[idx-1]
				if err := evedb.CreateDungeonInstance(ctx, sqldb, mission.MissionID, charID,
					mission.DestSystemID, tmpl.DungeonVID, roomID,
					mission.BookmarkX, mission.BookmarkY, mission.BookmarkZ); err != nil {
					log.Printf("[%s] DoAction Accept: CreateDungeonInstance: %v", addr, err)
				} else {
					log.Printf("[%s] DoAction Accept: dungeon instance created missionID=%d system=%d VID=%d room=%d",
						addr, mission.MissionID, mission.DestSystemID, tmpl.DungeonVID, roomID)
				}
			}
		}

		text := "Mission accepted. Good luck out there."
		if tmpl != nil && tmpl.AcceptText != "" {
			text = tmpl.AcceptText
		}
		mtp := evedb.BuildMissionTextParams(ctx, sqldb, charID, agentID, agent, tmpl, mission)
		text = evedb.SubstituteMissionText(text, mtp)
		log.Printf("[%s] → DoAction Accept missionID=%d charID=%d agentID=%d", addr, mission.MissionID, charID, agentID)
		notifyAgentMissionChange(c, accountID, "accepted", agentID)
		return agentConversation(text,
			[]marshal.PyRep{shellBtn(ctx, sqldb, agent, "shell.btn.quit", agentActionQuit)},
			agentMissionXtraInfo(mission, lp))

	case agentActionDecline:
		if mission == nil || mission.State != "offered" {
			return agentConversation("There is no pending offer to decline.",
				[]marshal.PyRep{shellBtn(ctx, sqldb, agent, "shell.btn.requestmission", agentActionRequestMission)},
				agentXtraInfo(agentID, 0, 0, lp))
		}
		// One decline per agent per missions.agentDeclineHours is free; another inside the window
		// costs the template's standing loss. Read before this decline is stamped.
		// See docs/agent-services.md#free-decline-window.
		penalized, _, derr := evedb.RecentAgentDecline(ctx, sqldb, charID, agentID)
		if derr != nil {
			log.Printf("[%s] DoAction Decline: %v", addr, derr)
			penalized = true
		}
		if err := evedb.DeclineMission(ctx, sqldb, mission.MissionID); err != nil {
			log.Printf("[%s] DoAction Decline: %v", addr, err)
		}
		tmpl, _ := evedb.GetTemplateByID(ctx, sqldb, mission.TemplateID)
		loss := 0.0
		if penalized {
			loss = -0.05
			if tmpl != nil {
				loss = tmpl.StandingLoss
			}
		}
		missionName := ""
		if tmpl != nil {
			missionName = tmpl.MissionName
		}
		applyAgentStanding(ctx, c, accountID, sqldb, charID, agentID,
			agentID, loss, evedb.EventStandingAgentMissionDeclined, missionName)
		if tmpl != nil && tmpl.Storyline {
			if err := evedb.ClearStorylinePending(ctx, sqldb, charID); err != nil {
				log.Printf("[%s] DoAction Decline: ClearStorylinePending: %v", addr, err)
			}
		}

		text := "I am disappointed. Come back when you are ready."
		if tmpl != nil && tmpl.DeclineText != "" {
			text = tmpl.DeclineText
		}
		mtp := evedb.BuildMissionTextParams(ctx, sqldb, charID, agentID, agent, tmpl, mission)
		text = evedb.SubstituteMissionText(text, mtp)
		if hours := evedb.GetAgentDeclineHours(ctx, sqldb); hours > 0 {
			key := "shell.decline.free"
			if penalized {
				key = "shell.decline.penalty"
			}
			text += "<br><br>" + serviceText(ctx, sqldb, agent.DivisionID, key, map[string]string{
				"pilot":     evedb.GetCharacterName(ctx, sqldb, charID),
				"available": time.Now().Add(time.Duration(hours) * time.Hour).Format("2006.01.02 15:04:05"),
			})
		}
		log.Printf("[%s] → DoAction Decline missionID=%d charID=%d standing %+.2f (penalized=%v)", addr, mission.MissionID, charID, loss, penalized)
		notifyAgentMissionChange(c, accountID, "declined", agentID)
		return agentConversation(text,
			[]marshal.PyRep{
				shellBtn(ctx, sqldb, agent, "shell.btn.requestmission", agentActionRequestMission),
				shellBtn(ctx, sqldb, agent, "shell.btn.quit", agentActionQuit),
			},
			agentXtraInfo(agentID, 0, 0, lp))

	case agentActionQuitMission:
		if mission == nil || mission.State != "accepted" {
			return agentConversation(serviceText(ctx, sqldb, agent.DivisionID, "shell.noactive", nil),
				[]marshal.PyRep{shellBtn(ctx, sqldb, agent, "shell.btn.requestmission", agentActionRequestMission)},
				agentXtraInfo(agentID, 0, 0, lp))
		}
		if err := evedb.FailMission(ctx, sqldb, mission.MissionID); err != nil {
			log.Printf("[%s] DoAction QuitMission: %v", addr, err)
		}
		cleanupMissionDungeon(ctx, sqldb, mission.MissionID, addr)
		tmpl, _ := evedb.GetTemplateByID(ctx, sqldb, mission.TemplateID)
		loss := -0.05
		if tmpl != nil {
			loss = tmpl.StandingLoss
		}
		missionName := ""
		if tmpl != nil {
			missionName = tmpl.MissionName
		}
		applyAgentStanding(ctx, c, accountID, sqldb, charID, agentID,
			agentID, loss, evedb.EventStandingAgentMissionFailed, missionName)

		// Quitting a storyline mission spends the invite.
		if tmpl != nil && tmpl.Storyline {
			if err := evedb.ClearStorylinePending(ctx, sqldb, charID); err != nil {
				log.Printf("[%s] DoAction QuitMission: ClearStorylinePending: %v", addr, err)
			}
		}

		if tmpl != nil {
			removeMissionItems(ctx, c, accountID, tmpl, mission, charID, agent.StationID, stationID, sqldb)
		}

		log.Printf("[%s] → DoAction QuitMission missionID=%d charID=%d standing %+.2f", addr, mission.MissionID, charID, loss)
		notifyAgentMissionChange(c, accountID, "quit", agentID)
		return agentConversation("I see. Mission abandoned. There will be consequences for your standing.",
			[]marshal.PyRep{
				shellBtn(ctx, sqldb, agent, "shell.btn.requestmission", agentActionRequestMission),
				shellBtn(ctx, sqldb, agent, "shell.btn.quit", agentActionQuit),
			},
			agentXtraInfo(agentID, 0, 0, lp))

	case agentActionComplete:
		if mission == nil || mission.State != "accepted" {
			return agentConversation(serviceText(ctx, sqldb, agent.DivisionID, "shell.noactive", nil),
				[]marshal.PyRep{shellBtn(ctx, sqldb, agent, "shell.btn.requestmission", agentActionRequestMission)},
				agentXtraInfo(agentID, 0, 0, lp))
		}
		tmpl, _ := evedb.GetTemplateByID(ctx, sqldb, mission.TemplateID)
		if tmpl == nil {
			return agentConversation("Mission data error.",
				[]marshal.PyRep{shellBtn(ctx, sqldb, agent, "shell.btn.quit", agentActionQuit)},
				agentXtraInfo(agentID, 0, 0, lp))
		}

		complete, reason := checkMissionComplete(ctx, tmpl, mission, charID, stationID, agent.StationID, sqldb)
		if !complete {
			return agentConversation(serviceText(ctx, sqldb, agent.DivisionID, "shell.objectives.incomplete",
				map[string]string{"reason": reason}),
				[]marshal.PyRep{shellBtn(ctx, sqldb, agent, "shell.btn.quit", agentActionQuit)},
				agentMissionXtraInfo(mission, lp))
		}

		if err := evedb.CompleteMission(ctx, sqldb, mission.MissionID); err != nil {
			log.Printf("[%s] DoAction Complete: %v", addr, err)
		}
		cleanupMissionDungeon(ctx, sqldb, mission.MissionID, addr)

		// Reward and time bonus are paid less the character's corp tax (evedb.PayMissionISK); a
		// rolebot turns in through this same case, so it pays the same tax.
		if mission.RewardISK > 0 {
			if _, _, err := evedb.PayMissionISK(ctx, sqldb, charID, agentID, evedb.RefAgentMissionReward, float64(mission.RewardISK)); err != nil {
				log.Printf("[%s] DoAction Complete reward: %v", addr, err)
			}
		}
		grantBonus := mission.BonusISK > 0 && mission.BonusDeadline.Valid && time.Now().Before(mission.BonusDeadline.Time)
		if grantBonus {
			if _, _, err := evedb.PayMissionISK(ctx, sqldb, charID, agentID, evedb.RefAgentMissionTimeBonus, float64(mission.BonusISK)); err != nil {
				log.Printf("[%s] DoAction Complete bonus: %v", addr, err)
			}
		}
		if (mission.RewardISK > 0 || grantBonus) && c != nil {
			if missionBal, err := evedb.GetCharacterBalance(ctx, sqldb, charID); err == nil {
				notifyArgs := marshal.PyTuple{Items: []marshal.PyRep{
					marshal.PyString{Value: "cash"},
					marshal.PyInt{Value: uint64(charID)},
					marshal.PyReal{Value: missionBal},
				}}
				_ = server.SendNotification(c, accountID, "OnAccountChange", "charid", notifyArgs)
			}
		}
		if mission.RewardLP > 0 {
			evedb.AddLoyaltyPoints(ctx, sqldb, charID, agentID, mission.RewardLP)
			lp += mission.RewardLP
		}
		// All three standings go into chrNPCStandings keyed by the NPC entity: standingsvc.GetStanding
		// reads npccharstandings by fromID (agent, corp or faction). Personal chrStandings rows leave the
		// character sheet and agent window at 0.00.
		applyAgentStanding(ctx, c, accountID, sqldb, charID, agentID,
			agentID, tmpl.StandingGain, evedb.EventStandingAgentMissionCompleted, tmpl.MissionName)
		log.Printf("[%s] standing: agent %d → char %d %+.3f", addr, agentID, charID, tmpl.StandingGain)
		if tmpl.CorpStandingGain != 0 && agent.CorporationID != 0 {
			applyAgentStanding(ctx, c, accountID, sqldb, charID, agentID,
				agent.CorporationID, tmpl.CorpStandingGain, evedb.EventStandingAgentMissionCompleted, tmpl.MissionName)
			log.Printf("[%s] standing: corp %d → char %d %+.3f", addr, agent.CorporationID, charID, tmpl.CorpStandingGain)
		} else {
			log.Printf("[%s] standing: corp skipped (corpStandingGain=%.3f corpID=%d)", addr, tmpl.CorpStandingGain, agent.CorporationID)
		}
		if tmpl.FactionStandingGain != 0 {
			if factionID, _ := evedb.GetAgentCorpFactionID(ctx, sqldb, agent.CorporationID); factionID != 0 {
				applyAgentStanding(ctx, c, accountID, sqldb, charID, agentID,
					factionID, tmpl.FactionStandingGain, evedb.EventStandingAgentMissionCompleted, tmpl.MissionName)
				log.Printf("[%s] standing: faction %d → char %d %+.3f", addr, factionID, charID, tmpl.FactionStandingGain)
			}
		}
		// The enemy faction takes the hit the briefing warns about; a positive authored value is
		// clamped to a loss.
		if tmpl.FactionStandingLoss != 0 && tmpl.EnemyFactionID > 0 && tmpl.EnemyFactionID != npc.FactionUnknown {
			loss := tmpl.FactionStandingLoss
			if loss > 0 {
				loss = -loss
			}
			applyAgentStanding(ctx, c, accountID, sqldb, charID, agentID,
				tmpl.EnemyFactionID, loss, evedb.EventStandingAgentMissionCompleted, tmpl.MissionName)
		}

		// A storyline mission is the payout, not a step toward the next: finishing one clears the invite.
		// See docs/main-s4-notes.md#storyline-run.
		if tmpl.Storyline {
			if err := evedb.ClearStorylinePending(ctx, sqldb, charID); err != nil {
				log.Printf("[%s] DoAction Complete: ClearStorylinePending: %v", addr, err)
			}
		} else {
			advanceStorylineRun(ctx, c, accountID, addr, sqldb, charID, agent)
		}

		removeMissionItems(ctx, c, accountID, tmpl, mission, charID, agent.StationID, stationID, sqldb)

		if tmpl.BaseRewardItemTypeID > 0 && tmpl.BaseRewardItemQty > 0 {
			if _, err := spawnMissionItem(ctx, c, accountID, charID, agent.StationID, stationID,
				tmpl.BaseRewardItemTypeID, tmpl.BaseRewardItemQty, sqldb); err != nil {
				log.Printf("[%s] DoAction Complete: base reward item spawn: %v", addr, err)
			}
		}
		if grantBonus && tmpl.BonusRewardItemTypeID > 0 && tmpl.BonusRewardItemQty > 0 {
			if _, err := spawnMissionItem(ctx, c, accountID, charID, agent.StationID, stationID,
				tmpl.BonusRewardItemTypeID, tmpl.BonusRewardItemQty, sqldb); err != nil {
				log.Printf("[%s] DoAction Complete: bonus reward item spawn: %v", addr, err)
			}
		}

		text := "Well done. Here is your reward."
		if tmpl.CompleteText != "" {
			text = tmpl.CompleteText
		}
		mtp := evedb.BuildMissionTextParams(ctx, sqldb, charID, agentID, agent, tmpl, mission)
		text = evedb.SubstituteMissionText(text, mtp)
		if grantBonus {
			text += "<br><br><b>Bonus reward granted!</b>"
		}
		log.Printf("[%s] → DoAction Complete missionID=%d charID=%d ISK=%d LP=%d", addr, mission.MissionID, charID, mission.RewardISK, mission.RewardLP)
		notifyAgentMissionChange(c, accountID, "completed", agentID)

		if tmpl.NextMissionID > 0 {
			nextTmpl, err := evedb.GetTemplateByID(ctx, sqldb, tmpl.NextMissionID)
			if err == nil && nextTmpl != nil && nextTmpl.Enabled {
				nextRewardISK, nextBonusISK, nextRewardLP := evedb.ComputeRewards(nextTmpl, agent.Quality)
				nextKillReq := missionKillRequirement(ctx, sqldb, nextTmpl)
				nextMissionID, cerr := evedb.CreateMissionOffer(ctx, sqldb, charID, agent.AgentID, nextTmpl.TemplateID,
					0, 0, 0, 0, 0, 0, 0,
					nextRewardISK, nextBonusISK, nextRewardLP, nextKillReq)
				if cerr == nil {
					log.Printf("[%s] → mission chain: completed %d → offered %d (tmpl %d)", addr, mission.MissionID, nextMissionID, nextTmpl.TemplateID)
					notifyAgentMissionChange(c, accountID, "offered", agentID)
					nextText := evedb.SubstituteMissionText(nextTmpl.BriefingText, mtp)
					text += "<br><br>" + nextText
					return agentConversation(text,
						[]marshal.PyRep{
							missionButton(nextTmpl.AcceptLabel, "Accept", agentActionAccept),
							missionButton(nextTmpl.DeclineLabel, "Decline", agentActionDecline),
						},
						agentXtraInfo(agentID, nextMissionID, 1, lp))
				}
			}
		}

		return agentConversation(text,
			[]marshal.PyRep{
				shellBtn(ctx, sqldb, agent, "shell.btn.requestmission", agentActionRequestMission),
				shellBtn(ctx, sqldb, agent, "shell.btn.quit", agentActionQuit),
			},
			agentXtraInfo(agentID, 0, 0, lp))

	case agentActionViewMission:
		if mission == nil {
			return agentConversation(serviceText(ctx, sqldb, agent.DivisionID, "shell.noactive", nil),
				[]marshal.PyRep{shellBtn(ctx, sqldb, agent, "shell.btn.requestmission", agentActionRequestMission)},
				agentXtraInfo(agentID, 0, 0, lp))
		}
		return showMissionDetails(ctx, addr, mission, agent, charID, lp, sqldb)

	default:
		// Initial conversation (actionID=0 or Quit re-open).
		if mission != nil {
			return showActiveMission(ctx, addr, mission, agent, lp, sqldb)
		}
		// R&D agents open on the research menu instead of mission patter.
		if agent.AgentTypeID == evedb.AgentTypeResearch {
			return agentResearchMenu(ctx, addr, sqldb, agent, charID, lp,
				"Greetings. I head a research project for my corporation's R&D division.")
		}
		hasOffers := len(availableOffersForChar(ctx, sqldb, charID, agentID, agent.CorporationID, agent.DivisionID)) > 0
		// An LP store is not part of the storyline gate, so an uninvited pilot still sees offers.
		if evedb.IsStorylineAgentType(agent.AgentTypeID) && !storylineInvitePending(ctx, sqldb, charID, agentID) {
			return storylineBrushOff(ctx, sqldb, agent, lp, hasOffers)
		}
		buttons := []marshal.PyRep{shellBtn(ctx, sqldb, agent, "shell.btn.requestmission", agentActionRequestMission)}
		if hasOffers {
			buttons = append(buttons, shellBtn(ctx, sqldb, agent, "shell.btn.viewoffers", agentActionViewOffers))
		}
		buttons = agentServiceButtons(ctx, sqldb, agent, charID, buttons)
		return agentConversation(serviceText(ctx, sqldb, agent.DivisionID, "shell.greeting", nil),
			buttons, agentXtraInfo(agentID, 0, 0, lp))
	}
}

// agentOffersMenu lists the agent's available LP-store offers as dialogue
// options.
func agentOffersMenu(ctx context.Context, sqldb *sql.DB, agent *evedb.AgentInfo, charID int64, lp int) marshal.PyRep {
	offers := availableOffersForChar(ctx, sqldb, charID, agent.AgentID, agent.CorporationID, agent.DivisionID)
	if len(offers) == 0 {
		return agentConversation(serviceText(ctx, sqldb, agent.DivisionID, "shell.offers.none", nil),
			[]marshal.PyRep{shellBtn(ctx, sqldb, agent, "shell.btn.quit", agentActionQuit)},
			agentXtraInfo(agent.AgentID, 0, 0, lp))
	}
	var buttons []marshal.PyRep
	for _, o := range offers {
		buttons = append(buttons, agentButton(o.OfferName, agentActionOfferViewBase+int(o.OfferID)))
	}
	buttons = append(buttons, shellBtn(ctx, sqldb, agent, "shell.btn.quit", agentActionQuit))
	text := serviceText(ctx, sqldb, agent.DivisionID, "shell.offers.intro",
		map[string]string{"lp": strconv.Itoa(lp)})
	return agentConversation(text, buttons, agentXtraInfo(agent.AgentID, 0, 0, lp))
}

// resolveOfferForAgent loads an offer and verifies it is available from this
// agent to this character (enabled, agent/corp scope, standing gate).
func resolveOfferForAgent(ctx context.Context, sqldb *sql.DB, agent *evedb.AgentInfo, charID, offerID int64) *evedb.AgentOffer {
	o, err := evedb.GetOfferByID(ctx, sqldb, offerID)
	if err != nil || o == nil || !o.Enabled {
		return nil
	}
	factionID, _ := evedb.GetAgentCorpFactionID(ctx, sqldb, agent.CorporationID)
	if !o.AvailableTo(agent.CorporationID, factionID, agent.DivisionID) {
		return nil
	}
	if o.MinStanding > 0 && evedb.GetNPCStandingToChar(ctx, sqldb, charID, agent.AgentID) < o.MinStanding {
		return nil
	}
	return o
}

// ── R&D agent research dialogue ───────────────────────────────────────────

// skillResearchProjectManagement grants one extra concurrent R&D agent per
// level; everyone gets one project without it.
const skillResearchProjectManagement = 12179

// researchPointsPerDay is the classic RP/day formula at this server's rate multiplier. The
// arithmetic lives in evedb so module enrolment, which never reaches the lazy rebase in
// agentResearchMenu, computes the same number.
func researchPointsPerDay(agent *evedb.AgentInfo, fieldSkillLevel int) float64 {
	return evedb.ResearchPointsPerDay(agent, fieldSkillLevel, rates.ResearchPoints)
}

// agentResearchFieldsFor returns the fields this agent offers, falling back
// to the corp's research fields when the agent has no per-agent rows.
func agentResearchFieldsFor(ctx context.Context, sqldb *sql.DB, agent *evedb.AgentInfo) []int64 {
	fields, err := evedb.GetAgentResearchFields(ctx, sqldb, agent.AgentID)
	if err == nil && len(fields) > 0 {
		return fields
	}
	fields, _ = evedb.GetCorpResearchFields(ctx, sqldb, agent.CorporationID)
	return fields
}

// agentResearchMenu is the R&D agent's main conversation: the active project
// status when one exists, otherwise the list of fields on offer. A stored
// RP/day that no longer matches the formula (field skill trained since) is
// rebased here, banking the accrued balance.
func agentResearchMenu(ctx context.Context, addr string, sqldb *sql.DB, agent *evedb.AgentInfo, charID int64, lp int, header string) marshal.PyRep {
	r, err := evedb.GetCharacterResearchForAgent(ctx, sqldb, charID, agent.AgentID)
	if err != nil {
		log.Printf("[%s] agentResearchMenu: %v", addr, err)
	}
	// The pending patent is loaded for both branches: a winner who cancelled their project can still
	// collect it.
	patent, err := evedb.GetPendingPatentOffer(ctx, sqldb, charID, agent.AgentID)
	if err != nil {
		log.Printf("[%s] agentResearchMenu patent: %v", addr, err)
	}
	patentBtn := func(buttons []marshal.PyRep) []marshal.PyRep {
		if patent == nil {
			return buttons
		}
		name, _ := evedb.GetTypeName(ctx, sqldb, patent.BlueprintID)
		if name == "" {
			name = "blueprint"
		}
		return append([]marshal.PyRep{
			agentButton("Collect Patent: "+name, agentActionViewPatent),
		}, buttons...)
	}
	patentLead := func(text string) string {
		if patent == nil {
			return text
		}
		return "<b>Your name came up in our last patent allocation.</b><br><br>" + text
	}

	if r != nil {
		fieldName, _ := evedb.GetTypeName(ctx, sqldb, r.SkillTypeID)
		lvl, _ := evedb.GetCharacterSkillLevel(ctx, sqldb, charID, r.SkillTypeID)
		if ppd := researchPointsPerDay(agent, lvl); math.Abs(ppd-r.PointsPerDay) > 1e-9 {
			if err := evedb.UpdateResearchRate(ctx, sqldb, r, ppd); err != nil {
				log.Printf("[%s] agentResearchMenu rebase: %v", addr, err)
			}
		}
		text := fmt.Sprintf(
			"We are collaborating on <b>%s</b> research.<br><br>"+
				"Accumulated research points: <b>%.2f</b><br>"+
				"Current rate: <b>%.2f RP/day</b><br><br>"+
				"Our combined research output determines your chances in my corporation's "+
				"technology patent allocations. Keep training <b>%s</b> to improve the rate.",
			fieldName, r.CurrentPoints(time.Now()), r.PointsPerDay, fieldName)
		text += researchRoundLine(ctx, sqldb, agent.AgentID, r.SkillTypeID, fieldName)
		return agentConversation(patentLead(text),
			patentBtn([]marshal.PyRep{
				agentButton("Cancel Research Project", agentActionCancelResearch),
				shellBtn(ctx, sqldb, agent, "shell.btn.requestmission", agentActionRequestMission),
				shellBtn(ctx, sqldb, agent, "shell.btn.quit", agentActionQuit),
			}),
			agentXtraInfo(agent.AgentID, 0, 0, lp))
	}

	fields := agentResearchFieldsFor(ctx, sqldb, agent)
	if len(fields) == 0 {
		return agentConversation(patentLead("My research program is not accepting outside collaborators at this time."),
			patentBtn([]marshal.PyRep{
				shellBtn(ctx, sqldb, agent, "shell.btn.requestmission", agentActionRequestMission),
				shellBtn(ctx, sqldb, agent, "shell.btn.quit", agentActionQuit),
			}),
			agentXtraInfo(agent.AgentID, 0, 0, lp))
	}

	text := header
	if text != "" {
		text += "<br><br>"
	}
	text += "I am able to conduct research in the following fields. The rate shown is what our collaboration would currently yield — it depends on my level and quality, and on your training in the field."
	var buttons []marshal.PyRep
	for _, f := range fields {
		name, _ := evedb.GetTypeName(ctx, sqldb, f)
		if name == "" {
			continue
		}
		lvl, _ := evedb.GetCharacterSkillLevel(ctx, sqldb, charID, f)
		caption := fmt.Sprintf("Research %s (%.2f RP/day)", name, researchPointsPerDay(agent, lvl))
		if lvl == 0 {
			caption = fmt.Sprintf("Research %s (requires the %s skill)", name, name)
		} else if n, err := evedb.FieldPatentCount(ctx, sqldb, f); err == nil && n == 0 {
			// A field with no patent pool can never pay out, so the caption says so.
			caption = fmt.Sprintf("Research %s (no patents allocated)", name)
		}
		buttons = append(buttons, agentButton(caption, agentActionStartResearchBase+int(f)))
	}
	buttons = append(buttons,
		shellBtn(ctx, sqldb, agent, "shell.btn.requestmission", agentActionRequestMission),
		shellBtn(ctx, sqldb, agent, "shell.btn.quit", agentActionQuit))
	return agentConversation(patentLead(text), patentBtn(buttons), agentXtraInfo(agent.AgentID, 0, 0, lp))
}

// researchRoundLine tells a collaborator when this agent's allocation round closes and whether
// their field has anything to allocate. The client has no patent UI, so this conversation is the
// only place the lottery is mentioned.
func researchRoundLine(ctx context.Context, sqldb *sql.DB, agentID, skillTypeID int64, fieldName string) string {
	var out string
	if n, err := evedb.FieldPatentCount(ctx, sqldb, skillTypeID); err == nil && n == 0 {
		out += fmt.Sprintf("<br><br><b>Note:</b> my corporation holds no patents in %s at present, "+
			"so this collaboration cannot win you one.", fieldName)
	}
	round, err := evedb.GetAgentRound(ctx, sqldb, agentID)
	if err != nil || round == nil {
		return out
	}
	left := time.Until(time.Unix(round.DrawAt, 0))
	switch {
	case left <= 0:
		out += "<br><br>The current allocation round is closing now."
	case left < 48*time.Hour:
		out += fmt.Sprintf("<br><br>The current allocation round closes in <b>%d hours</b>.",
			int(left.Hours())+1)
	default:
		out += fmt.Sprintf("<br><br>The current allocation round closes in <b>%d days</b>.",
			int(left.Hours()/24))
	}
	return out
}

// agentPatentView renders a won blueprint as an agent offer with no agtOffers row behind it, so
// buildLPOfferBlock cannot be reused. The prize was drawn from rndfieldblueprints.
func agentPatentView(ctx context.Context, addr string, sqldb *sql.DB, agent *evedb.AgentInfo, charID int64, lp int) marshal.PyRep {
	p, err := evedb.GetPendingPatentOffer(ctx, sqldb, charID, agent.AgentID)
	if err != nil {
		log.Printf("[%s] agentPatentView: %v", addr, err)
	}
	if p == nil {
		return agentResearchMenu(ctx, addr, sqldb, agent, charID, lp,
			"That patent is no longer available.")
	}
	bpName, _ := evedb.GetTypeName(ctx, sqldb, p.BlueprintID)
	fieldName, _ := evedb.GetTypeName(ctx, sqldb, p.SkillTypeID)
	productName := ""
	if productID, err := evedb.GetBlueprintProduct(ctx, sqldb, p.BlueprintID); err == nil && productID != 0 {
		productName, _ = evedb.GetTypeName(ctx, sqldb, productID)
	}

	text := fmt.Sprintf(
		"Our %s research programme has produced a patent, and the allocation fell to you.<br><br>"+
			"<b>%s</b><br>", fieldName, bpName)
	if productName != "" {
		text += fmt.Sprintf("Produces: <b>%s</b><br>", productName)
	}
	text += fmt.Sprintf("Awarded: %s<br>", time.Unix(p.WonAt, 0).UTC().Format("2006-01-02"))
	left := time.Until(time.Unix(p.ExpiresAt, 0))
	if left < 48*time.Hour {
		text += fmt.Sprintf("Must be collected within: <b>%d hours</b><br>", int(left.Hours())+1)
	} else {
		text += fmt.Sprintf("Must be collected within: <b>%d days</b><br>", int(left.Hours()/24))
	}
	text += "<br>There is no charge — the research points you spent on it were the price. " +
		"The original will be transferred to your hangar here."

	return agentConversation(text,
		[]marshal.PyRep{
			agentButton("Accept Patent", agentActionAcceptPatent),
			agentButton("Back", agentActionViewResearch),
			shellBtn(ctx, sqldb, agent, "shell.btn.quit", agentActionQuit),
		},
		agentXtraInfo(agent.AgentID, 0, 0, lp))
}

// agentPatentAccept hands the won BPO over, in the same shape as agentOfferAccept. ClaimPatentOffer
// enforces the station rule so a module claiming for a headless pilot meets it too; the check here
// only answers in prose.
func agentPatentAccept(ctx context.Context, c *evenet.Conn, accountID int64, addr string,
	agent *evedb.AgentInfo, charID, stationID int64, lp int, sqldb *sql.DB) marshal.PyRep {

	backButtons := []marshal.PyRep{
		agentButton("Back", agentActionViewResearch),
		shellBtn(ctx, sqldb, agent, "shell.btn.quit", agentActionQuit),
	}
	p, err := evedb.GetPendingPatentOffer(ctx, sqldb, charID, agent.AgentID)
	if err != nil {
		log.Printf("[%s] agentPatentAccept: %v", addr, err)
	}
	if p == nil {
		return agentResearchMenu(ctx, addr, sqldb, agent, charID, lp,
			"That patent is no longer available.")
	}
	if stationID != agent.StationID {
		return agentConversation("You must be docked at my station to collect the patent.",
			backButtons, agentXtraInfo(agent.AgentID, 0, 0, lp))
	}

	itemID, typeID, err := evedb.ClaimPatentOffer(ctx, sqldb, p.OfferID, charID, stationID, time.Now())
	if err != nil {
		log.Printf("[%s] agentPatentAccept offerID=%d charID=%d: %v", addr, p.OfferID, charID, err)
		return agentConversation("Something has gone wrong with the transfer. Speak to me again in a moment.",
			backButtons, agentXtraInfo(agent.AgentID, 0, 0, lp))
	}
	pushSpawnedItem(ctx, c, accountID, itemID, agent.StationID, stationID, sqldb)
	notifyJournalInvalidate(c, accountID, "research_patent_claimed")

	bpName, _ := evedb.GetTypeName(ctx, sqldb, typeID)
	log.Printf("[%s] → patent collected offerID=%d charID=%d agentID=%d typeID=%d itemID=%d",
		addr, p.OfferID, charID, agent.AgentID, typeID, itemID)
	return agentConversation(
		fmt.Sprintf("The <b>%s</b> is yours — it is in your hangar here. Use it well.", bpName),
		[]marshal.PyRep{
			agentButton("View Research Project", agentActionViewResearch),
			shellBtn(ctx, sqldb, agent, "shell.btn.quit", agentActionQuit),
		},
		agentXtraInfo(agent.AgentID, 0, 0, lp))
}

// agentStartResearch validates and starts a research project in one field.
func agentStartResearch(ctx context.Context, c *evenet.Conn, accountID int64, addr string, agent *evedb.AgentInfo, charID, skillTypeID int64, lp int, sqldb *sql.DB) marshal.PyRep {
	if agent.AgentTypeID != evedb.AgentTypeResearch {
		return agentConversation("I am not a research agent — you must be looking for one of my colleagues in the R&D division.",
			[]marshal.PyRep{shellBtn(ctx, sqldb, agent, "shell.btn.quit", agentActionQuit)},
			agentXtraInfo(agent.AgentID, 0, 0, lp))
	}
	if r, _ := evedb.GetCharacterResearchForAgent(ctx, sqldb, charID, agent.AgentID); r != nil {
		return agentResearchMenu(ctx, addr, sqldb, agent, charID, lp, "")
	}
	offered := false
	for _, f := range agentResearchFieldsFor(ctx, sqldb, agent) {
		if f == skillTypeID {
			offered = true
			break
		}
	}
	fieldName, _ := evedb.GetTypeName(ctx, sqldb, skillTypeID)
	if !offered || fieldName == "" {
		return agentResearchMenu(ctx, addr, sqldb, agent, charID, lp,
			"I do not conduct research in that field.")
	}
	lvl, _ := evedb.GetCharacterSkillLevel(ctx, sqldb, charID, skillTypeID)
	if lvl == 0 {
		return agentResearchMenu(ctx, addr, sqldb, agent, charID, lp,
			fmt.Sprintf("You lack the scientific background for that work — train the <b>%s</b> skill first.", fieldName))
	}
	// Concurrent project cap: 1, plus 1 per Research Project Management level.
	count, _ := evedb.CountCharacterResearch(ctx, sqldb, charID)
	rpmLevel, _ := evedb.GetCharacterSkillLevel(ctx, sqldb, charID, skillResearchProjectManagement)
	if count >= 1+rpmLevel {
		return agentResearchMenu(ctx, addr, sqldb, agent, charID, lp,
			"You are already managing as many research projects as you can handle. Train <b>Research Project Management</b> to run more, or cancel one of your existing projects.")
	}
	ppd := researchPointsPerDay(agent, lvl)
	if err := evedb.StartResearch(ctx, sqldb, charID, agent.AgentID, skillTypeID, ppd); err != nil {
		log.Printf("[%s] agentStartResearch charID=%d agentID=%d field=%d: %v", addr, charID, agent.AgentID, skillTypeID, err)
		return agentResearchMenu(ctx, addr, sqldb, agent, charID, lp,
			"Something went wrong setting up our project. Speak to me again in a moment.")
	}
	log.Printf("[%s] → research started charID=%d agentID=%d field=%d ppd=%.2f", addr, charID, agent.AgentID, skillTypeID, ppd)
	// Only this push invalidates the client's cached journal; without it the Research tab keeps its
	// first fetch.
	notifyJournalInvalidate(c, accountID, "research_started")
	return agentConversation(
		fmt.Sprintf("Excellent! We are now collaborating on <b>%s</b> research at <b>%.2f RP/day</b>. "+
			"Check our progress in your journal's Research tab. Our accumulated research points determine "+
			"your standing in my corporation's technology patent allocations.", fieldName, ppd),
		[]marshal.PyRep{
			agentButton("View Research Project", agentActionViewResearch),
			shellBtn(ctx, sqldb, agent, "shell.btn.quit", agentActionQuit),
		},
		agentXtraInfo(agent.AgentID, 0, 0, lp))
}

// agentOfferView shows one offer's full description, items and cost.
func agentOfferView(ctx context.Context, sqldb *sql.DB, agent *evedb.AgentInfo, charID int64, lp int, offerID int64) marshal.PyRep {
	o := resolveOfferForAgent(ctx, sqldb, agent, charID, offerID)
	if o == nil {
		return agentOffersMenu(ctx, sqldb, agent, charID, lp)
	}
	return agentConversation(buildLPOfferBlock(ctx, sqldb, o),
		[]marshal.PyRep{
			agentButton("Accept Offer", agentActionOfferAcceptBase+int(o.OfferID)),
			agentButton("Back to Offers", agentActionViewOffers),
			shellBtn(ctx, sqldb, agent, "shell.btn.quit", agentActionQuit),
		},
		agentXtraInfo(agent.AgentID, 0, 0, lp))
}

// agentOfferAccept validates and executes the offer exchange: cost items are
// taken from the hangar at the agent's station, ISK and LP are deducted, and
// reward items spawn in the same hangar.
func agentOfferAccept(ctx context.Context, c *evenet.Conn, accountID int64, addr string,
	agent *evedb.AgentInfo, charID, stationID int64, lp int, offerID int64, sqldb *sql.DB) marshal.PyRep {

	backButtons := []marshal.PyRep{
		agentButton("Back to Offers", agentActionViewOffers),
		shellBtn(ctx, sqldb, agent, "shell.btn.quit", agentActionQuit),
	}
	o := resolveOfferForAgent(ctx, sqldb, agent, charID, offerID)
	if o == nil {
		return agentOffersMenu(ctx, sqldb, agent, charID, lp)
	}

	// All item movement happens in the hangar at the agent's station.
	if stationID != agent.StationID {
		return agentConversation("You must be docked at my station to accept this offer.",
			backButtons, agentXtraInfo(agent.AgentID, 0, 0, lp))
	}
	if o.LPCost > 0 && lp < o.LPCost {
		return agentConversation(fmt.Sprintf("This offer requires %d loyalty points; you have %d.", o.LPCost, lp),
			backButtons, agentXtraInfo(agent.AgentID, 0, 0, lp))
	}
	if o.ISKCost > 0 {
		bal, err := evedb.GetCharacterBalance(ctx, sqldb, charID)
		if err != nil || bal < float64(o.ISKCost) {
			return agentConversation(fmt.Sprintf("This offer requires %s credits.", formatISK(o.ISKCost)),
				backButtons, agentXtraInfo(agent.AgentID, 0, 0, lp))
		}
	}
	for _, it := range o.Costs {
		have, err := evedb.CountItemInHangar(ctx, sqldb, charID, agent.StationID, it.TypeID)
		if err != nil || have < it.Quantity {
			name, _ := evedb.GetTypeName(ctx, sqldb, it.TypeID)
			return agentConversation(fmt.Sprintf("You need %d x %s in your hangar at this station; you have %d.",
				it.Quantity, name, have), backButtons, agentXtraInfo(agent.AgentID, 0, 0, lp))
		}
	}

	for _, it := range o.Costs {
		_, deltas, err := evedb.RemoveItemsFromHangar(ctx, sqldb, charID, agent.StationID, it.TypeID, it.Quantity)
		if err != nil {
			log.Printf("[%s] offer %d: remove cost item typeID=%d: %v", addr, o.OfferID, it.TypeID, err)
		}
		sendItemDeltas(c, accountID, agent.StationID, stationID, deltas)
	}
	if o.ISKCost > 0 {
		evedb.GiveISK(ctx, sqldb, charID, -float64(o.ISKCost))
		if bal, err := evedb.GetCharacterBalance(ctx, sqldb, charID); err == nil {
			_ = evedb.InsertJournalEntry(ctx, sqldb, charID, 35, charID, agent.AgentID, -float64(o.ISKCost), bal, o.OfferName)
			if c != nil {
				_ = server.SendNotification(c, accountID, "OnAccountChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
					marshal.PyString{Value: "cash"},
					marshal.PyInt{Value: uint64(charID)},
					marshal.PyReal{Value: bal},
				}})
			}
		}
	}
	if o.LPCost > 0 {
		evedb.AddLoyaltyPoints(ctx, sqldb, charID, agent.AgentID, -o.LPCost)
		lp -= o.LPCost
	}
	for _, it := range o.Rewards {
		if _, err := spawnMissionItem(ctx, c, accountID, charID, agent.StationID, stationID, it.TypeID, it.Quantity, sqldb); err != nil {
			log.Printf("[%s] offer %d: spawn reward typeID=%d: %v", addr, o.OfferID, it.TypeID, err)
		}
	}
	notifyAgentMissionChange(c, accountID, "offer_accepted", agent.AgentID)
	log.Printf("[%s] → offer accepted offerID=%d charID=%d agentID=%d lp=-%d isk=-%d",
		addr, o.OfferID, charID, agent.AgentID, o.LPCost, o.ISKCost)

	return agentConversation("A pleasure doing business with you. The items are in your hangar.",
		[]marshal.PyRep{
			shellBtn(ctx, sqldb, agent, "shell.btn.viewoffers", agentActionViewOffers),
			shellBtn(ctx, sqldb, agent, "shell.btn.quit", agentActionQuit),
		},
		agentXtraInfo(agent.AgentID, 0, 0, lp))
}

// missionKillRequirement returns the kill objective size for a kill-mission template. killAll
// with a dungeon counts every NPC-capable object across all rooms, held waves included; other
// completion types use the authored TargetCount.
func missionKillRequirement(ctx context.Context, sqldb *sql.DB, tmpl *evedb.MissionTemplate) int {
	if tmpl == nil || tmpl.MissionType != "kill" {
		return 0
	}
	if tmpl.CompletionType == "killAll" && tmpl.DungeonVID > 0 {
		if n, err := evedb.CountDungeonNPCObjects(ctx, sqldb, tmpl.DungeonVID); err == nil && n > 0 {
			return n
		}
	}
	if tmpl.TargetCount > 0 {
		return tmpl.TargetCount
	}
	return 1
}

// generateMissionOffer rolls a mission type, picks a template, generates
// destination, and creates the offer.
func generateMissionOffer(ctx context.Context, addr string, agent *evedb.AgentInfo, charID int64, lp int, sqldb *sql.DB) marshal.PyRep {
	weights, err := evedb.GetDivisionWeights(ctx, sqldb, agent.DivisionID)
	if err != nil {
		log.Printf("[%s] generateMissionOffer: GetDivisionWeights: %v", addr, err)
	}
	missionType := evedb.RollMissionType(weights)

	factionID, _ := evedb.GetAgentCorpFactionID(ctx, sqldb, agent.CorporationID)
	// Storyline agents draw only storyline templates and everyone else only regular ones; the kill
	// fallback below stays in the same deck.
	storyline := evedb.IsStorylineAgentType(agent.AgentTypeID)
	tmpl, err := evedb.GetRandomTemplate(ctx, sqldb, missionType, agent.Level, factionID, storyline)
	if err != nil {
		log.Printf("[%s] generateMissionOffer: GetRandomTemplate: %v", addr, err)
	}
	// A kill template with no dungeonVID or an empty dungeon never spawns anything, so it is
	// rejected and the fallback path runs.
	if tmpl != nil && tmpl.MissionType == "kill" {
		if tmpl.DungeonVID <= 0 {
			log.Printf("[%s] generateMissionOffer: kill template %d has no dungeonVID, skipping", addr, tmpl.TemplateID)
			tmpl = nil
		} else {
			roomIDs, _ := evedb.GetDungeonRoomIDs(ctx, sqldb, tmpl.DungeonVID)
			if len(roomIDs) == 0 {
				log.Printf("[%s] generateMissionOffer: template %d dungeon VID=%d has no rooms, skipping", addr, tmpl.TemplateID, tmpl.DungeonVID)
				tmpl = nil
			}
		}
	}
	if tmpl == nil {
		// No template for this type and level: fall back to a kill template.
		if missionType != "kill" {
			tmpl, _ = evedb.GetRandomTemplate(ctx, sqldb, "kill", agent.Level, factionID, storyline)
			if tmpl != nil && tmpl.MissionType == "kill" {
				if tmpl.DungeonVID <= 0 {
					log.Printf("[%s] generateMissionOffer: fallback kill template %d has no dungeonVID, skipping", addr, tmpl.TemplateID)
					tmpl = nil
				} else {
					roomIDs, _ := evedb.GetDungeonRoomIDs(ctx, sqldb, tmpl.DungeonVID)
					if len(roomIDs) == 0 {
						log.Printf("[%s] generateMissionOffer: fallback template %d dungeon VID=%d has no rooms, skipping", addr, tmpl.TemplateID, tmpl.DungeonVID)
						tmpl = nil
					}
				}
			}
		}
		if tmpl == nil {
			return agentConversation(serviceText(ctx, sqldb, agent.DivisionID, "shell.nomissions", nil),
				[]marshal.PyRep{shellBtn(ctx, sqldb, agent, "shell.btn.quit", agentActionQuit)},
				agentXtraInfo(agent.AgentID, 0, 0, lp))
		}
	}

	var destSystemID, destStationID int64
	var pickupStn, dropoffStn int64
	var bx, by, bz float64

	switch tmpl.MissionType {
	case "kill":
		// Weighted by jump distance. See docs/mission-notes.md#jump-weights.
		bands, _ := evedb.GetNearbySystemsByJumps(ctx, sqldb, agent.SolarSystemID, tmpl.MinSecurity, tmpl.MaxSecurity)
		destSystemID = evedb.PickNearbySystem(bands)
		if destSystemID == 0 {
			destSystemID = agent.SolarSystemID
		}
		bx = float64(rand.Int63n(1e12)) - 5e11
		by = float64(rand.Int63n(1e11)) - 5e10
		bz = float64(rand.Int63n(1e12)) - 5e11

	case "courier":
		pickupStn = resolveStationMode(ctx, tmpl.PickupMode, tmpl.PickupStationID, agent, tmpl, sqldb)
		dropoffStn = resolveStationMode(ctx, tmpl.DropoffMode, tmpl.DropoffStationID, agent, tmpl, sqldb)
		if pickupStn == 0 {
			pickupStn = agent.StationID
		}
		if dropoffStn == 0 {
			dropoffStn = agent.StationID
		}
		destStationID = dropoffStn

	case "trade", "mining":
		destStationID = agent.StationID
		destSystemID = agent.SolarSystemID
	}

	rewardISK, bonusISK, rewardLP := evedb.ComputeRewards(tmpl, agent.Quality)
	killRequired := missionKillRequirement(ctx, sqldb, tmpl)

	missionID, err := evedb.CreateMissionOffer(ctx, sqldb, charID, agent.AgentID, tmpl.TemplateID,
		destSystemID, destStationID, pickupStn, dropoffStn,
		bx, by, bz, rewardISK, bonusISK, rewardLP, killRequired)
	if err != nil {
		log.Printf("[%s] generateMissionOffer: CreateMissionOffer: %v", addr, err)
		return agentConversation(serviceText(ctx, sqldb, agent.DivisionID, "shell.nomissions", nil),
			[]marshal.PyRep{shellBtn(ctx, sqldb, agent, "shell.btn.quit", agentActionQuit)},
			agentXtraInfo(agent.AgentID, 0, 0, lp))
	}

	log.Printf("[%s] → generated mission offer id=%d type=%s tmpl=%d agent=%d", addr, missionID, tmpl.MissionType, tmpl.TemplateID, agent.AgentID)

	xtra := marshal.PyDict{Items: []marshal.DictEntry{
		{Key: marshal.PyString{Value: "agentID"}, Value: marshal.PyInt{Value: uint64(agent.AgentID)}},
		{Key: marshal.PyString{Value: "loyaltyPoints"}, Value: marshal.PyInt{Value: uint64(lp)}},
		{Key: marshal.PyString{Value: "offerID"}, Value: marshal.PyInt{Value: uint64(missionID)}},
		{Key: marshal.PyString{Value: "missionState"}, Value: marshal.PyInt{Value: 1}},
	}}
	if tmpl.EnemyFactionID > 0 && tmpl.EnemyFactionID != npc.FactionUnknown {
		xtra.Items = append(xtra.Items, marshal.DictEntry{
			Key: marshal.PyString{Value: "enemyFactionID"}, Value: marshal.PyInt{Value: uint64(tmpl.EnemyFactionID)},
		})
	}
	if destSystemID > 0 {
		xtra.Items = append(xtra.Items, marshal.DictEntry{
			Key: marshal.PyString{Value: "destSystemID"}, Value: marshal.PyInt{Value: uint64(destSystemID)},
		})
	}

	// The mission row does not exist yet, so the text params come from a stand-in.
	fakeMission := &evedb.CharacterMission{
		DestSystemID:  destSystemID,
		DestStationID: destStationID,
		RewardISK:     rewardISK,
		BonusISK:      bonusISK,
		RewardLP:      rewardLP,
	}
	mtp := evedb.BuildMissionTextParams(ctx, sqldb, charID, agent.AgentID, agent, tmpl, fakeMission)
	briefing := evedb.SubstituteMissionText(tmpl.BriefingText, mtp)

	return agentConversation(briefing, missionOfferButtons(ctx, sqldb, agent, tmpl), xtra)
}

// showActiveMission returns the conversation for an existing active mission.
func showActiveMission(ctx context.Context, addr string, mission *evedb.CharacterMission, agent *evedb.AgentInfo, lp int, sqldb *sql.DB) marshal.PyRep {
	tmpl, _ := evedb.GetTemplateByID(ctx, sqldb, mission.TemplateID)
	text := "Mission in progress."
	if tmpl != nil {
		text = tmpl.BriefingText
		mtp := evedb.BuildMissionTextParams(ctx, sqldb, mission.CharID, mission.AgentID, agent, tmpl, mission)
		text = evedb.SubstituteMissionText(text, mtp)
	}

	var buttons []marshal.PyRep
	switch mission.State {
	case "offered":
		buttons = missionOfferButtons(ctx, sqldb, agent, tmpl)
	case "accepted":
		buttons = missionActiveButtons(ctx, sqldb, agent, tmpl)
	}
	// Agent services stay reachable while a mission is running.
	buttons = agentServiceButtons(ctx, sqldb, agent, mission.CharID, buttons)

	return agentConversation(text, buttons, agentMissionXtraInfo(mission, lp))
}

// showMissionDetails returns the detailed mission view with objectives, rewards, and location.
func showMissionDetails(ctx context.Context, addr string, mission *evedb.CharacterMission, agent *evedb.AgentInfo, charID int64, lp int, sqldb *sql.DB) marshal.PyRep {
	tmpl, _ := evedb.GetTemplateByID(ctx, sqldb, mission.TemplateID)

	missionName := "Mission"
	if tmpl != nil && tmpl.MissionName != "" {
		missionName = tmpl.MissionName
	}
	var b strings.Builder
	// The conversation window's body is client-authored (agents.py), so the watermark rides on a
	// nested body tag: OnStart_body re-runs LoadBodyAttrs and paints the sprite.
	b.WriteString("<body" + missionWatermarkAttrs + ">")
	b.WriteString("<b><font size=18>")
	b.WriteString(missionName)
	b.WriteString("</font></b><br>")

	if agent != nil {
		agentName := evedb.GetAgentName(ctx, sqldb, agent.AgentID)
		if agentName != "" {
			b.WriteString("<font size=12>Agent: ")
			b.WriteString(agentName)
			b.WriteString("</font>")
		}
	}
	b.WriteString("<hr>")

	if tmpl != nil && tmpl.BriefingText != "" {
		mtp := evedb.BuildMissionTextParams(ctx, sqldb, charID, mission.AgentID, agent, tmpl, mission)
		b.WriteString(missionBriefingBlock(
			evedb.SubstituteMissionText(tmpl.BriefingText, mtp), tmpl.EnemyFactionID))
	}

	if tmpl != nil && tmpl.EnemyFactionID > 0 && tmpl.EnemyFactionID != npc.FactionUnknown {
		factionName := evedb.GetFactionName(ctx, sqldb, tmpl.EnemyFactionID)
		if factionName != "" {
			b.WriteString(`<p><font color="yellow"><i>WARNING: Completing this mission will result in a standings loss with `)
			b.WriteString(factionName)
			b.WriteString(`.</i></font></p>`)
		}
	}

	if mission.ExpiresAt.Valid {
		b.WriteString("<p><b>This mission expires at ")
		b.WriteString(mission.ExpiresAt.Time.Format("2006.01.02 15:04:05"))
		b.WriteString("</b></p>")
	}

	b.WriteString(`<hr><font size=14><b>Overview and Objectives</b></font><br>`)
	b.WriteString("The following objectives must be completed to finish the mission:<br><br>")

	if tmpl != nil {
		mtp := evedb.BuildMissionTextParams(ctx, sqldb, charID, agent.AgentID, agent, tmpl, mission)

		var obj strings.Builder

		switch tmpl.MissionType {
		case "kill":
			targetName := "targets"
			if tmpl.TargetTypeID > 0 {
				if n, err := evedb.GetTypeName(ctx, sqldb, tmpl.TargetTypeID); err == nil && n != "" {
					targetName = n
				}
			}
			obj.WriteString(`<font size=14><b>Kill Objective</b></font><br>`)
			killDone := mission.ObjectiveDone
			if killDone {
				obj.WriteString(`Destroy these targets: <font color=#00FF00>Objective completed</font><br>`)
			} else {
				obj.WriteString("Destroy these targets:<br>")
			}
			if mission.DestSystemID > 0 {
				sysName, _ := evedb.GetSystemName(ctx, sqldb, mission.DestSystemID)
				if sysName != "" {
					if sec, ok, err := evedb.GetSystemSecurity(ctx, sqldb, mission.DestSystemID); err == nil && ok {
						sysName = fmt.Sprintf("%s (%.1f)", sysName, sec)
					}
					obj.WriteString("Location ")
					obj.WriteString(systemLink(mission.DestSystemID, sysName))
					obj.WriteString("<br>")
				}
			}
			obj.WriteString("<br>")
			if tmpl.ObjectiveText != "" {
				obj.WriteString(evedb.SubstituteMissionText(tmpl.ObjectiveText, mtp))
				obj.WriteString("<br>")
			} else if tmpl.TargetTypeID > 0 {
				n := tmpl.TargetCount
				if n <= 0 {
					n = 1
				}
				obj.WriteString(fmt.Sprintf(`<img src="typeicon:%d" size=32> Target #1 %s %s<br>`,
					tmpl.TargetTypeID, countWord(n), targetName))
			} else {
				obj.WriteString(fmt.Sprintf("Destroy the %s and any NPCs guarding it.<br>", targetName))
			}
			if !killDone && tmpl.TargetCount > 0 {
				obj.WriteString(fmt.Sprintf("Progress: %d / %d<br>", mission.KillProgress, mission.KillRequired))
			}

			if tmpl.CompletionType == "killItem" && tmpl.KillItemTypeID > 0 {
				kiName := "item"
				if n, err := evedb.GetTypeName(ctx, sqldb, tmpl.KillItemTypeID); err == nil && n != "" {
					kiName = n
				}
				obj.WriteString(fmt.Sprintf(`<br><img src="typeicon:%d" size=32> Bring Item: %s x %d<br>`, tmpl.KillItemTypeID, kiName, tmpl.KillItemQty))
				dropStn := mission.DropoffStationID
				if tmpl.KillItemDropoffStationID > 0 {
					dropStn = tmpl.KillItemDropoffStationID
				}
				if dropStn > 0 {
					obj.WriteString(fmt.Sprintf("Drop-off: %s<br>", evedb.GetStationName(ctx, sqldb, dropStn)))
				}
			}

		case "courier":
			obj.WriteString(courierObjectiveBlock(ctx, sqldb, tmpl, mission, mtp))

		case "trade":
			itemName := "goods"
			typeID := tmpl.TradeTypeID
			if typeID > 0 {
				if n, err := evedb.GetTypeName(ctx, sqldb, typeID); err == nil && n != "" {
					itemName = n
				}
			}
			if typeID > 0 {
				obj.WriteString(fmt.Sprintf(`<img src="typeicon:%d" size=32> `, typeID))
			}
			if tmpl.ObjectiveText != "" {
				obj.WriteString(evedb.SubstituteMissionText(tmpl.ObjectiveText, mtp))
			} else {
				obj.WriteString(fmt.Sprintf("Deliver %d x %s to the agent.", tmpl.TradeQty, itemName))
			}
			obj.WriteString("<br>")

		case "mining":
			itemName := "ore"
			typeID := tmpl.MiningTypeID
			if typeID > 0 {
				if n, err := evedb.GetTypeName(ctx, sqldb, typeID); err == nil && n != "" {
					itemName = n
				}
			}
			if typeID > 0 {
				obj.WriteString(fmt.Sprintf(`<img src="typeicon:%d" size=32> `, typeID))
			}
			if tmpl.ObjectiveText != "" {
				obj.WriteString(evedb.SubstituteMissionText(tmpl.ObjectiveText, mtp))
			} else {
				obj.WriteString(fmt.Sprintf("Mine and deliver %d x %s.", tmpl.MiningQty, itemName))
			}
			obj.WriteString("<br>")
		}

		// Starmap left, objective details right.
		if sm := starmapImg(ctx, sqldb, mission.DestSystemID, mission.CharID); sm != "" {
			b.WriteString(`<table><tr><td valign=top>`)
			b.WriteString(sm)
			b.WriteString(`</td><td valign=top>`)
			b.WriteString(obj.String())
			b.WriteString(`</td></tr></table>`)
		} else {
			b.WriteString(obj.String())
		}
	}

	b.WriteString(`<hr><font size=14><b>Rewards</b></font><br>`)
	b.WriteString("The following rewards and bonuses will be yours if you complete this mission:<br><br>")
	if mission.RewardISK > 0 {
		b.WriteString(fmt.Sprintf("%s credits.<br>", formatISK(mission.RewardISK)))
	}
	if mission.RewardLP > 0 {
		b.WriteString(fmt.Sprintf("%d Loyalty Points.<br>", mission.RewardLP))
	}
	if tmpl != nil && tmpl.BaseRewardItemTypeID > 0 && tmpl.BaseRewardItemQty > 0 {
		riName := "item"
		if n, err := evedb.GetTypeName(ctx, sqldb, tmpl.BaseRewardItemTypeID); err == nil && n != "" {
			riName = n
		}
		b.WriteString(fmt.Sprintf(`<img src="typeicon:%d" size=32> %s x %d<br>`, tmpl.BaseRewardItemTypeID, riName, tmpl.BaseRewardItemQty))
	}

	if mission.BonusISK > 0 || (tmpl != nil && tmpl.BonusRewardItemTypeID > 0) {
		b.WriteString("<br>")
		if mission.BonusISK > 0 {
			if mission.BonusDeadline.Valid {
				b.WriteString(fmt.Sprintf("%s credits will be awarded to you as a bonus if you complete the mission before %s.<br>",
					formatISK(mission.BonusISK), mission.BonusDeadline.Time.Format("2006.01.02 15:04")))
			} else {
				b.WriteString(fmt.Sprintf("%s credits will be awarded to you as a bonus.<br>", formatISK(mission.BonusISK)))
			}
		}
		if tmpl != nil && tmpl.BonusRewardItemTypeID > 0 && tmpl.BonusRewardItemQty > 0 {
			biName := "item"
			if n, err := evedb.GetTypeName(ctx, sqldb, tmpl.BonusRewardItemTypeID); err == nil && n != "" {
				biName = n
			}
			b.WriteString(fmt.Sprintf(`<img src="typeicon:%d" size=32> %s x %d (bonus)<br>`, tmpl.BonusRewardItemTypeID, biName, tmpl.BonusRewardItemQty))
		}
	}

	var buttons []marshal.PyRep
	switch mission.State {
	case "offered":
		buttons = []marshal.PyRep{
			agentButton("Accept", agentActionAccept),
			agentButton("Decline", agentActionDecline),
		}
	case "accepted":
		buttons = []marshal.PyRep{
			agentButton("Complete Mission", agentActionComplete),
			agentButton("Quit Mission", agentActionQuitMission),
			shellBtn(ctx, sqldb, agent, "shell.btn.quit", agentActionQuit),
		}
	}

	return agentConversation(b.String(), buttons, agentMissionXtraInfo(mission, lp))
}

// countWord spells quantities up to twenty as words ("five Angel Predators"); larger values
// print as digits.
func countWord(n int) string {
	words := []string{"zero", "one", "two", "three", "four", "five", "six",
		"seven", "eight", "nine", "ten", "eleven", "twelve", "thirteen",
		"fourteen", "fifteen", "sixteen", "seventeen", "eighteen", "nineteen", "twenty"}
	if n >= 0 && n < len(words) {
		return words[n]
	}
	return fmt.Sprintf("%d", n)
}

// systemLink renders a solar system name as a showinfo link (typeID 5 = Solar System).
func systemLink(systemID int64, name string) string {
	return fmt.Sprintf(`<a href="showinfo:5//%d">%s</a>`, systemID, name)
}

// starmapImg renders the mission-journal 2D map image tag.
//
// ids must be constellation IDs: the client's <img src="starmap:"> handler (ui/control/bastard/
// components.py GetMap) calls Map2D.Draw(ids, 2, 3, size) and GetMapData drops any id that fails
// util.IsConstellation, so a region ID draws a blank map. The one mark is the character's own
// system, sysID::hint::color::sumin: all four fields must be present or GetMap discards it, only
// id and hint are read, and spaces in the hint are sent as '+' (Decanonicalize form-decodes each
// value). A mark renders only for a system the map draws, so a character outside the destination
// constellation adds their own constellation to ids; across a region boundary the map has no mark.
func starmapImg(ctx context.Context, sqldb *sql.DB, systemID, charID int64) string {
	destCon, destRegion, err := evedb.GetSolarSystemLocation(ctx, sqldb, systemID)
	if err != nil || destCon == 0 {
		return ""
	}
	plain := fmt.Sprintf(`<img src="starmap:ids=%d" size=200>`, destCon)

	curSystemID := charSolarSystemID(ctx, sqldb, charID)
	if curSystemID <= 0 {
		return plain
	}

	ids := fmt.Sprintf("%d", destCon)
	if !systemOnConstellationMap(ctx, sqldb, curSystemID, destCon) {
		curCon, curRegion, err := evedb.GetSolarSystemLocation(ctx, sqldb, curSystemID)
		if err != nil || curCon == 0 || curRegion != destRegion {
			return plain
		}
		ids = fmt.Sprintf("%d,%d", destCon, curCon)
	}

	return fmt.Sprintf(`<img src="starmap:ids=%s&marks=%d::You+are+here::FFFFFF::0" size=200>`,
		ids, curSystemID)
}

// systemOnConstellationMap reports whether a system appears on the 2D map drawn for a
// constellation, which is the precondition for marking it. Map2D.Draw plots the constellation's
// own systems plus the systems at the far end of any gate leaving it (GetConnectionData). Any
// other system resolves to no coordinates and _SetMarks abandons the mark list.
func systemOnConstellationMap(ctx context.Context, sqldb *sql.DB, systemID, constellationID int64) bool {
	var found int
	err := sqldb.QueryRowContext(ctx,
		`SELECT 1 FROM mapSolarSystems WHERE solarSystemID=? AND constellationID=?
		  UNION
		 SELECT 1 FROM mapSolarSystemJumps WHERE fromSolarSystemID=? AND toConstellationID=?
		 LIMIT 1`,
		systemID, constellationID, systemID, constellationID).Scan(&found)
	return err == nil && found == 1
}

// charSolarSystemID returns the character's current solar system, or 0 when it
// is unknown (docked characters still carry a solarSystemID).
func charSolarSystemID(ctx context.Context, sqldb *sql.DB, charID int64) int64 {
	if charID == 0 {
		return 0
	}
	var systemID sql.NullInt64
	if err := sqldb.QueryRowContext(ctx,
		`SELECT solarSystemID FROM character_ WHERE characterID=?`, charID).Scan(&systemID); err != nil {
		return 0
	}
	return systemID.Int64
}

// missionBriefingBlock renders the "Mission briefing" heading, the body and the enemy faction logo.
// The logo sits in its own table cell: for align=right the client parser (bastard/parserbase.py
// AddOverlay) assigns the right margin to attrs.left, so a floated image paints over the text.
func missionBriefingBlock(text string, factionID int64) string {
	var b strings.Builder
	b.WriteString(`<font size=14><b>Mission briefing</b></font><br>`)
	if factionID <= 0 || factionID == npc.FactionUnknown {
		b.WriteString("<p>")
		b.WriteString(text)
		b.WriteString("</p>")
		return b.String()
	}
	b.WriteString(`<table cellpadding=4><tr><td valign=top>`)
	b.WriteString(text)
	b.WriteString(`</td><td valign=top width=72>`)
	b.WriteString(fmt.Sprintf(`<img src="factionlogo:%d" size=64>`, factionID))
	b.WriteString(`</td></tr></table>`)
	return b.String()
}

// courierObjectiveBlock renders the legacy "Transport Objective" layout: the
// pickup and drop-off stations as showinfo links prefixed with the owning
// corp's logo, then the cargo line with the shipment's total volume.
func courierObjectiveBlock(ctx context.Context, sqldb *sql.DB,
	tmpl *evedb.MissionTemplate, mission *evedb.CharacterMission, mtp *evedb.MissionTextParams) string {

	var b strings.Builder
	b.WriteString(`<font size=14><b>Transport Objective</b></font><br>`)
	if tmpl.ObjectiveText != "" {
		b.WriteString(evedb.SubstituteMissionText(tmpl.ObjectiveText, mtp))
		b.WriteString("<br>")
	} else {
		b.WriteString("Transport these goods:<br>")
	}
	b.WriteString(`<table cellpadding=2>`)
	b.WriteString(stationObjectiveRow(ctx, sqldb, "Pickup<br>Location:", mission.PickupStationID))
	b.WriteString(stationObjectiveRow(ctx, sqldb, "Drop-off<br>Location:", mission.DropoffStationID))

	// Cargo row: "one unit of Transaction and Salary Logs (1.0 m³)".
	itemName := "cargo"
	if tmpl.CourierTypeID > 0 {
		if n, err := evedb.GetTypeName(ctx, sqldb, tmpl.CourierTypeID); err == nil && n != "" {
			itemName = n
		}
	}
	qty := tmpl.CourierQty
	if qty <= 0 {
		qty = 1
	}
	unit := "unit"
	if qty != 1 {
		unit = "units"
	}
	cargo := fmt.Sprintf("%s %s of %s", countWord(qty), unit, itemName)
	if tmpl.CourierTypeID > 0 {
		if vol, err := evedb.GetTypeVolume(ctx, sqldb, tmpl.CourierTypeID); err == nil && vol > 0 {
			// &sup3;: the client parser resolves named entities only, not &#179;.
			cargo += fmt.Sprintf(" (%.1f m&sup3;)", vol*float64(qty))
		}
	}
	b.WriteString(`<tr><td valign=middle width=36>`)
	if tmpl.CourierTypeID > 0 {
		b.WriteString(fmt.Sprintf(`<img src="typeicon:%d" size=32>`, tmpl.CourierTypeID))
	}
	b.WriteString(`</td><td valign=middle>Cargo:</td><td valign=middle>`)
	b.WriteString(cargo)
	b.WriteString(`</td></tr></table>`)
	return b.String()
}

// stationObjectiveRow renders one pickup or drop-off row: owning corp logo, label and the station
// as a showinfo link. The href needs the station typeID, so a failed station bits lookup falls
// back to a bold name.
func stationObjectiveRow(ctx context.Context, sqldb *sql.DB, label string, stationID int64) string {
	if stationID <= 0 {
		return ""
	}
	name := evedb.GetStationName(ctx, sqldb, stationID)
	if name == "" {
		return ""
	}
	logo := ""
	link := "<b>" + name + "</b>"
	if bits, err := evedb.GetStationItemBits(ctx, sqldb, stationID); err == nil {
		logo = fmt.Sprintf(`<img src="corplogo:%d" size=32>`, bits.OwnerID)
		link = fmt.Sprintf(`<a href="showinfo:%d//%d">%s</a>`, bits.StationTypeID, stationID, name)
	}
	return fmt.Sprintf(`<tr><td valign=middle width=36>%s</td><td valign=middle>%s</td><td valign=middle>%s</td></tr>`,
		logo, label, link)
}

// missionWatermarkAttrs paints the handshake sprite (icon 17_02) into the page background. It
// must sit on a <body> tag: only LoadBodyAttrs reads the background attribute. The image
// width/height are required for icon: backgrounds (the client leaves the sprite at width 0
// otherwise), no-repeat stops whole-window tiling, and the alpha in background-image-color dims
// it. A floated <img align=...> cannot do this: AddOverlay registers the image width as a text
// margin.
const missionWatermarkAttrs = ` background="icon:17_02" style="background-repeat: no-repeat;` +
	` background-image-width: 224; background-image-height: 224;` +
	` background-image-left: 8; background-image-top: 0;` +
	` background-image-color: #FFFFFF55"`

// missionHTMLHead opens the journal HTML document. The client browser window takes its caption
// from <TITLE>; link=#FFA800 matches the body attrs in agents.py.
func missionHTMLHead(title string) string {
	return fmt.Sprintf("<HTML><HEAD><TITLE>%s</TITLE></HEAD><BODY link=#FFA800%s>", title, missionWatermarkAttrs)
}

// formatISK formats an ISK value with comma separators.
func formatISK(isk int64) string {
	s := fmt.Sprintf("%d", isk)
	if len(s) <= 3 {
		return s
	}
	var result []byte
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			result = append(result, ',')
		}
		result = append(result, byte(c))
	}
	return string(result)
}

// missionJournalInfoRep builds the agentMgr.GetMissionJournalInfo result: the HTML string for the
// detail popup, or PyNone when the character has no live mission with this agent. Shared by the
// bound call and the inline MachoBindObject variant (journal.py inlines the call into the bind).
func missionJournalInfoRep(ctx context.Context, sqldb *sql.DB, charID, agentID int64) marshal.PyRep {
	mission, err := evedb.GetActiveMission(ctx, sqldb, charID, agentID)
	if err != nil || mission == nil || (mission.State != "accepted" && mission.State != "offered") {
		return marshal.PyNone{}
	}
	tmpl, _ := evedb.GetTemplateByID(ctx, sqldb, mission.TemplateID)
	agent, _ := evedb.GetAgentInfo(ctx, sqldb, agentID)
	if mission.State == "accepted" {
		return marshal.PyString{Value: buildMissionJournalHTML(ctx, sqldb, charID, mission, tmpl, agent)}
	}
	return marshal.PyString{Value: buildOfferJournalHTML(ctx, sqldb, charID, mission, tmpl, agent)}
}

// buildMissionJournalHTML generates the rich HTML displayed in the mission
// detail popup for an accepted mission (Journal → Read details).
func buildMissionJournalHTML(ctx context.Context, sqldb *sql.DB, charID int64,
	mission *evedb.CharacterMission, tmpl *evedb.MissionTemplate, agent *evedb.AgentInfo) string {

	missionName := "Mission"
	if tmpl != nil && tmpl.MissionName != "" {
		missionName = tmpl.MissionName
	}

	var b strings.Builder
	b.WriteString(missionHTMLHead(missionName))

	b.WriteString("<font size=20><b>")
	b.WriteString(missionName)
	b.WriteString("</b></font><br>")

	if agent != nil {
		agentName := evedb.GetAgentName(ctx, sqldb, agent.AgentID)
		if agentName != "" {
			b.WriteString("<font size=12>Agent: ")
			b.WriteString(agentName)
			b.WriteString("</font>")
		}
	}
	b.WriteString("<hr>")

	if tmpl != nil && tmpl.BriefingText != "" {
		mtp := evedb.BuildMissionTextParams(ctx, sqldb, charID, mission.AgentID, agent, tmpl, mission)
		b.WriteString(missionBriefingBlock(
			evedb.SubstituteMissionText(tmpl.BriefingText, mtp), tmpl.EnemyFactionID))
	}

	if tmpl != nil && tmpl.EnemyFactionID > 0 && tmpl.EnemyFactionID != npc.FactionUnknown {
		factionName := evedb.GetFactionName(ctx, sqldb, tmpl.EnemyFactionID)
		if factionName != "" {
			b.WriteString(`<p><font color="yellow"><i>WARNING: Completing this mission will result in a standings loss with `)
			b.WriteString(factionName)
			b.WriteString(`.</i></font></p>`)
		}
	}

	if mission.ExpiresAt.Valid {
		b.WriteString("<p><b>This mission expires at ")
		b.WriteString(mission.ExpiresAt.Time.Format("2006.01.02 15:04:05"))
		b.WriteString("</b></p>")
	}

	b.WriteString(`<hr><font size=14><b>Overview and Objectives</b></font><br>`)
	b.WriteString("The following objectives must be completed to finish the mission:<br><br>")

	if tmpl != nil {
		mtp := evedb.BuildMissionTextParams(ctx, sqldb, charID, mission.AgentID, agent, tmpl, mission)

		var obj strings.Builder

		switch tmpl.MissionType {
		case "kill":
			targetName := "targets"
			if tmpl.TargetTypeID > 0 {
				if n, err := evedb.GetTypeName(ctx, sqldb, tmpl.TargetTypeID); err == nil && n != "" {
					targetName = n
				}
			}
			obj.WriteString(`<font size=14><b>Kill Objective</b></font><br>`)
			killDone := mission.ObjectiveDone
			if killDone {
				obj.WriteString(`Destroy these targets: <font color=#00FF00>Objective completed</font><br>`)
			} else {
				obj.WriteString("Destroy these targets:<br>")
			}
			if mission.DestSystemID > 0 {
				sysName, _ := evedb.GetSystemName(ctx, sqldb, mission.DestSystemID)
				if sysName != "" {
					if sec, ok, err := evedb.GetSystemSecurity(ctx, sqldb, mission.DestSystemID); err == nil && ok {
						sysName = fmt.Sprintf("%s (%.1f)", sysName, sec)
					}
					obj.WriteString("Location ")
					obj.WriteString(systemLink(mission.DestSystemID, sysName))
					obj.WriteString("<br>")
				}
			}
			obj.WriteString("<br>")
			if tmpl.ObjectiveText != "" {
				obj.WriteString(evedb.SubstituteMissionText(tmpl.ObjectiveText, mtp))
				obj.WriteString("<br>")
			} else if tmpl.TargetTypeID > 0 {
				n := tmpl.TargetCount
				if n <= 0 {
					n = 1
				}
				obj.WriteString(fmt.Sprintf(`<img src="typeicon:%d" size=32> Target #1 %s %s<br>`,
					tmpl.TargetTypeID, countWord(n), targetName))
			} else {
				obj.WriteString(fmt.Sprintf("Destroy the %s and any NPCs guarding it.<br>", targetName))
			}
			if !killDone && tmpl.TargetCount > 0 {
				obj.WriteString(fmt.Sprintf("Progress: %d / %d<br>", mission.KillProgress, mission.KillRequired))
			}

			if tmpl.CompletionType == "killItem" && tmpl.KillItemTypeID > 0 {
				kiName := "item"
				if n, err := evedb.GetTypeName(ctx, sqldb, tmpl.KillItemTypeID); err == nil && n != "" {
					kiName = n
				}
				obj.WriteString(fmt.Sprintf(`<br><img src="typeicon:%d" size=32> Bring Item: %s x %d<br>`, tmpl.KillItemTypeID, kiName, tmpl.KillItemQty))
				dropStn := mission.DropoffStationID
				if tmpl.KillItemDropoffStationID > 0 {
					dropStn = tmpl.KillItemDropoffStationID
				}
				if dropStn > 0 {
					obj.WriteString(fmt.Sprintf("Drop-off: %s<br>", evedb.GetStationName(ctx, sqldb, dropStn)))
				}
			}

		case "courier":
			obj.WriteString(courierObjectiveBlock(ctx, sqldb, tmpl, mission, mtp))

		case "trade":
			itemName := "goods"
			typeID := tmpl.TradeTypeID
			if typeID > 0 {
				if n, err := evedb.GetTypeName(ctx, sqldb, typeID); err == nil && n != "" {
					itemName = n
				}
			}
			if typeID > 0 {
				obj.WriteString(fmt.Sprintf(`<img src="typeicon:%d" size=32> `, typeID))
			}
			if tmpl.ObjectiveText != "" {
				obj.WriteString(evedb.SubstituteMissionText(tmpl.ObjectiveText, mtp))
			} else {
				obj.WriteString(fmt.Sprintf("Deliver %d x %s to the agent.", tmpl.TradeQty, itemName))
			}
			obj.WriteString("<br>")

		case "mining":
			itemName := "ore"
			typeID := tmpl.MiningTypeID
			if typeID > 0 {
				if n, err := evedb.GetTypeName(ctx, sqldb, typeID); err == nil && n != "" {
					itemName = n
				}
			}
			if typeID > 0 {
				obj.WriteString(fmt.Sprintf(`<img src="typeicon:%d" size=32> `, typeID))
			}
			if tmpl.ObjectiveText != "" {
				obj.WriteString(evedb.SubstituteMissionText(tmpl.ObjectiveText, mtp))
			} else {
				obj.WriteString(fmt.Sprintf("Mine and deliver %d x %s.", tmpl.MiningQty, itemName))
			}
			obj.WriteString("<br>")
		}

		if sm := starmapImg(ctx, sqldb, mission.DestSystemID, mission.CharID); sm != "" {
			b.WriteString(`<table><tr><td valign=top>`)
			b.WriteString(sm)
			b.WriteString(`</td><td valign=top>`)
			b.WriteString(obj.String())
			b.WriteString(`</td></tr></table>`)
		} else {
			b.WriteString(obj.String())
		}
	}

	b.WriteString(`<hr><font size=14><b>Rewards</b></font><br>`)
	b.WriteString("The following rewards and bonuses will be yours if you complete this mission:<br><br>")
	if mission.RewardISK > 0 {
		b.WriteString(fmt.Sprintf("%s credits.<br>", formatISK(mission.RewardISK)))
	}
	if mission.RewardLP > 0 {
		b.WriteString(fmt.Sprintf("%d Loyalty Points.<br>", mission.RewardLP))
	}
	if tmpl != nil && tmpl.BaseRewardItemTypeID > 0 && tmpl.BaseRewardItemQty > 0 {
		riName := "item"
		if n, err := evedb.GetTypeName(ctx, sqldb, tmpl.BaseRewardItemTypeID); err == nil && n != "" {
			riName = n
		}
		b.WriteString(fmt.Sprintf(`<img src="typeicon:%d" size=32> %s x %d<br>`, tmpl.BaseRewardItemTypeID, riName, tmpl.BaseRewardItemQty))
	}

	if mission.BonusISK > 0 || (tmpl != nil && tmpl.BonusRewardItemTypeID > 0) {
		b.WriteString("<br>")
		if mission.BonusISK > 0 {
			if mission.BonusDeadline.Valid {
				b.WriteString(fmt.Sprintf("%s credits will be awarded to you as a bonus if you complete the mission before %s.<br>",
					formatISK(mission.BonusISK), mission.BonusDeadline.Time.Format("2006.01.02 15:04")))
			} else {
				b.WriteString(fmt.Sprintf("%s credits will be awarded to you as a bonus.<br>", formatISK(mission.BonusISK)))
			}
		}
		if tmpl != nil && tmpl.BonusRewardItemTypeID > 0 && tmpl.BonusRewardItemQty > 0 {
			biName := "item"
			if n, err := evedb.GetTypeName(ctx, sqldb, tmpl.BonusRewardItemTypeID); err == nil && n != "" {
				biName = n
			}
			b.WriteString(fmt.Sprintf(`<img src="typeicon:%d" size=32> %s x %d (bonus)<br>`, tmpl.BonusRewardItemTypeID, biName, tmpl.BonusRewardItemQty))
		}
	}

	b.WriteString("</BODY></HTML>")
	return b.String()
}

// buildOfferJournalHTML generates the rich HTML for an offered (not yet
// accepted) mission detail popup.
func buildOfferJournalHTML(ctx context.Context, sqldb *sql.DB, charID int64,
	mission *evedb.CharacterMission, tmpl *evedb.MissionTemplate, agent *evedb.AgentInfo) string {

	missionName := "Mission Offer"
	if tmpl != nil && tmpl.MissionName != "" {
		missionName = tmpl.MissionName
	}

	var b strings.Builder
	b.WriteString(missionHTMLHead(missionName))

	b.WriteString("<font size=20><b>")
	b.WriteString(missionName)
	b.WriteString("</b></font><br>")

	if agent != nil {
		agentName := evedb.GetAgentName(ctx, sqldb, agent.AgentID)
		if agentName != "" {
			b.WriteString("<font size=12>Agent: ")
			b.WriteString(agentName)
			b.WriteString("</font>")
		}
	}
	b.WriteString("<hr>")

	if tmpl != nil && tmpl.BriefingText != "" {
		mtp := evedb.BuildMissionTextParams(ctx, sqldb, charID, mission.AgentID, agent, tmpl, mission)
		b.WriteString(missionBriefingBlock(
			evedb.SubstituteMissionText(tmpl.BriefingText, mtp), tmpl.EnemyFactionID))
	}

	if tmpl != nil && tmpl.EnemyFactionID > 0 && tmpl.EnemyFactionID != npc.FactionUnknown {
		factionName := evedb.GetFactionName(ctx, sqldb, tmpl.EnemyFactionID)
		if factionName != "" {
			b.WriteString(`<p><font color="yellow"><i>WARNING: Completing this mission will result in a standings loss with `)
			b.WriteString(factionName)
			b.WriteString(`.</i></font></p>`)
		}
	}

	if tmpl != nil {
		b.WriteString(`<hr><font size=14><b>Overview and Objectives</b></font><br>`)
		b.WriteString("The following objectives must be completed to finish the mission:<br><br>")
		mtp := evedb.BuildMissionTextParams(ctx, sqldb, charID, mission.AgentID, agent, tmpl, mission)

		var obj strings.Builder

		switch tmpl.MissionType {
		case "kill":
			targetName := "targets"
			if tmpl.TargetTypeID > 0 {
				if n, err := evedb.GetTypeName(ctx, sqldb, tmpl.TargetTypeID); err == nil && n != "" {
					targetName = n
				}
			}
			obj.WriteString(`<font size=14><b>Kill Objective</b></font><br>`)
			obj.WriteString("Destroy these targets:<br>")
			if mission.DestSystemID > 0 {
				sysName, _ := evedb.GetSystemName(ctx, sqldb, mission.DestSystemID)
				if sysName != "" {
					if sec, ok, err := evedb.GetSystemSecurity(ctx, sqldb, mission.DestSystemID); err == nil && ok {
						sysName = fmt.Sprintf("%s (%.1f)", sysName, sec)
					}
					obj.WriteString("Location ")
					obj.WriteString(systemLink(mission.DestSystemID, sysName))
					obj.WriteString("<br>")
				}
			}
			obj.WriteString("<br>")
			if tmpl.ObjectiveText != "" {
				obj.WriteString(evedb.SubstituteMissionText(tmpl.ObjectiveText, mtp))
				obj.WriteString("<br>")
			} else if tmpl.TargetTypeID > 0 {
				n := tmpl.TargetCount
				if n <= 0 {
					n = 1
				}
				obj.WriteString(fmt.Sprintf(`<img src="typeicon:%d" size=32> Target #1 %s %s<br>`,
					tmpl.TargetTypeID, countWord(n), targetName))
			} else {
				obj.WriteString(fmt.Sprintf("Destroy the %s and any NPCs guarding it.<br>", targetName))
			}

		case "courier":
			obj.WriteString(courierObjectiveBlock(ctx, sqldb, tmpl, mission, mtp))

		case "trade":
			itemName := "goods"
			if tmpl.TradeTypeID > 0 {
				if n, err := evedb.GetTypeName(ctx, sqldb, tmpl.TradeTypeID); err == nil && n != "" {
					itemName = n
				}
			}
			if tmpl.ObjectiveText != "" {
				obj.WriteString(evedb.SubstituteMissionText(tmpl.ObjectiveText, mtp))
			} else {
				obj.WriteString(fmt.Sprintf("Deliver %d units of %s to the agent's station.", tmpl.TradeQty, itemName))
			}
			obj.WriteString("<br>")

		case "mining":
			itemName := "ore"
			if tmpl.MiningTypeID > 0 {
				if n, err := evedb.GetTypeName(ctx, sqldb, tmpl.MiningTypeID); err == nil && n != "" {
					itemName = n
				}
			}
			if tmpl.ObjectiveText != "" {
				obj.WriteString(evedb.SubstituteMissionText(tmpl.ObjectiveText, mtp))
			} else {
				obj.WriteString(fmt.Sprintf("Mine and deliver %d units of %s to the agent's station.", tmpl.MiningQty, itemName))
			}
			obj.WriteString("<br>")
		}

		if sm := starmapImg(ctx, sqldb, mission.DestSystemID, mission.CharID); sm != "" {
			b.WriteString(`<table><tr><td valign=top>`)
			b.WriteString(sm)
			b.WriteString(`</td><td valign=top>`)
			b.WriteString(obj.String())
			b.WriteString(`</td></tr></table>`)
		} else {
			b.WriteString(obj.String())
		}
	}

	b.WriteString(`<hr><font size=14><b>Rewards</b></font><br>`)
	b.WriteString("The following rewards and bonuses will be yours if you complete this mission:<br><br>")
	if mission.RewardISK > 0 {
		b.WriteString(fmt.Sprintf("%s credits.<br>", formatISK(mission.RewardISK)))
	}
	if mission.RewardLP > 0 {
		b.WriteString(fmt.Sprintf("%d Loyalty Points.<br>", mission.RewardLP))
	}
	if tmpl != nil && tmpl.BaseRewardItemTypeID > 0 && tmpl.BaseRewardItemQty > 0 {
		riName := "item"
		if n, err := evedb.GetTypeName(ctx, sqldb, tmpl.BaseRewardItemTypeID); err == nil && n != "" {
			riName = n
		}
		b.WriteString(fmt.Sprintf(`<img src="typeicon:%d" size=32> %s x %d<br>`, tmpl.BaseRewardItemTypeID, riName, tmpl.BaseRewardItemQty))
	}

	// The offer has no accept timestamp yet, so the bonus window is phrased as a duration.
	if mission.BonusISK > 0 || (tmpl != nil && tmpl.BonusRewardItemTypeID > 0) {
		b.WriteString("<br>")
		if mission.BonusISK > 0 {
			timeStr := ""
			if tmpl != nil && tmpl.BonusTimeSec > 0 {
				hours := tmpl.BonusTimeSec / 3600
				minutes := (tmpl.BonusTimeSec % 3600) / 60
				switch {
				case hours > 0 && minutes > 0:
					timeStr = fmt.Sprintf(" if you complete the mission within %d hours and %d minutes", hours, minutes)
				case hours > 0:
					timeStr = fmt.Sprintf(" if you complete the mission within %d hours", hours)
				default:
					timeStr = fmt.Sprintf(" if you complete the mission within %d minutes", minutes)
				}
			}
			b.WriteString(fmt.Sprintf("%s credits will be awarded to you as a bonus%s.<br>", formatISK(mission.BonusISK), timeStr))
		}
		if tmpl != nil && tmpl.BonusRewardItemTypeID > 0 && tmpl.BonusRewardItemQty > 0 {
			biName := "item"
			if n, err := evedb.GetTypeName(ctx, sqldb, tmpl.BonusRewardItemTypeID); err == nil && n != "" {
				biName = n
			}
			b.WriteString(fmt.Sprintf(`<img src="typeicon:%d" size=32> %s x %d (bonus)<br>`, tmpl.BonusRewardItemTypeID, biName, tmpl.BonusRewardItemQty))
		}
	}

	b.WriteString("</BODY></HTML>")
	return b.String()
}

// offerItemLine renders one offer item row: `<icon> one unit of Raven Navy Issue Blueprint
// (single run copy, ...)`.
func offerItemLine(ctx context.Context, sqldb *sql.DB, it evedb.OfferItem) string {
	name := "item"
	if n, err := evedb.GetTypeName(ctx, sqldb, it.TypeID); err == nil && n != "" {
		name = n
	}
	unit := "unit"
	if it.Quantity != 1 {
		unit = "units"
	}
	line := fmt.Sprintf(`<img src="typeicon:%d" size=32> %s %s of %s`,
		it.TypeID, countWord(it.Quantity), unit, name)
	if it.ExtraInfo != "" {
		line += " (" + it.ExtraInfo + ")"
	}
	return line + "<br>"
}

// buildLPOfferBlock renders one LP-store offer: description, expiry, offered items and cost.
func buildLPOfferBlock(ctx context.Context, sqldb *sql.DB, o *evedb.AgentOffer) string {
	var b strings.Builder

	b.WriteString("<font size=20><b>")
	b.WriteString(o.OfferName)
	b.WriteString("</b></font><br><br>")

	b.WriteString(`<font size=14><b>Offer Description</b></font><br>`)
	if o.Description != "" {
		b.WriteString("<p>")
		b.WriteString(o.Description)
		b.WriteString("</p>")
	}

	if o.ExpiresDays > 0 {
		b.WriteString("<p><b>This offer expires at ")
		b.WriteString(time.Now().Add(time.Duration(o.ExpiresDays) * 24 * time.Hour).Format("2006.01.02 15:04:05"))
		b.WriteString("</b></p>")
	}

	if len(o.Rewards) == 1 {
		b.WriteString(`<font size=14><b>Offered Item:</b></font><br>`)
		b.WriteString("This item is being offered to you by your agent:<br><br>")
	} else {
		b.WriteString(`<font size=14><b>Offered Items:</b></font><br>`)
		b.WriteString("These items are being offered to you by your agent:<br><br>")
	}
	for _, it := range o.Rewards {
		b.WriteString(offerItemLine(ctx, sqldb, it))
	}

	b.WriteString(`<br><font size=14><b>Cost:</b></font><br>`)
	b.WriteString("The agent is requesting the following as payment for the item:<br><br>")
	for _, it := range o.Costs {
		b.WriteString(offerItemLine(ctx, sqldb, it))
	}
	if o.ISKCost > 0 {
		b.WriteString(fmt.Sprintf("%s credits<br>", formatISK(o.ISKCost)))
	}
	if o.LPCost > 0 {
		b.WriteString(fmt.Sprintf("%d Loyalty Points<br>", o.LPCost))
	}
	return b.String()
}

// buildLPOfferJournalHTML renders the journal detail page for an agent's
// LP-store offers (one page per agent; multiple offers separated by rules).
func buildLPOfferJournalHTML(ctx context.Context, sqldb *sql.DB, agentID int64, offers []*evedb.AgentOffer) string {
	title := "Special Offers"
	if len(offers) == 1 {
		title = offers[0].OfferName
	} else if name := evedb.GetAgentName(ctx, sqldb, agentID); name != "" {
		title = "Special Offers - " + name
	}
	var b strings.Builder
	b.WriteString(missionHTMLHead(title))
	for i, o := range offers {
		if i > 0 {
			b.WriteString("<br><hr><br>")
		}
		b.WriteString(buildLPOfferBlock(ctx, sqldb, o))
	}
	b.WriteString("</BODY></HTML>")
	return b.String()
}

// offerJournalInfoRep resolves GetOfferJournalInfo for both the agentMgr
// service and the bound agent moniker: LP-store offers render first; if the
// agent has none, an offered (not yet accepted) mission renders instead.
func offerJournalInfoRep(ctx context.Context, sqldb *sql.DB, charID, agentID int64) marshal.PyRep {
	corpID, divID := int64(0), int64(0)
	if agent, err := evedb.GetAgentInfo(ctx, sqldb, agentID); err == nil {
		corpID, divID = agent.CorporationID, agent.DivisionID
	}
	if offers := availableOffersForChar(ctx, sqldb, charID, agentID, corpID, divID); len(offers) > 0 {
		return marshal.PyString{Value: buildLPOfferJournalHTML(ctx, sqldb, agentID, offers)}
	}
	mission, err := evedb.GetActiveMission(ctx, sqldb, charID, agentID)
	if err != nil || mission == nil || mission.State != "offered" {
		return marshal.PyNone{}
	}
	tmpl, _ := evedb.GetTemplateByID(ctx, sqldb, mission.TemplateID)
	agent, _ := evedb.GetAgentInfo(ctx, sqldb, agentID)
	return marshal.PyString{Value: buildOfferJournalHTML(ctx, sqldb, charID, mission, tmpl, agent)}
}

// agentXtraInfo builds a basic xtraInfo dict.
func agentXtraInfo(agentID int64, missionID, missionState int64, lp int) marshal.PyDict {
	items := []marshal.DictEntry{
		{Key: marshal.PyString{Value: "agentID"}, Value: marshal.PyInt{Value: uint64(agentID)}},
		{Key: marshal.PyString{Value: "loyaltyPoints"}, Value: marshal.PyInt{Value: uint64(lp)}},
	}
	if missionID > 0 {
		items = append(items,
			marshal.DictEntry{Key: marshal.PyString{Value: "missionID"}, Value: marshal.PyInt{Value: uint64(missionID)}},
			marshal.DictEntry{Key: marshal.PyString{Value: "missionState"}, Value: marshal.PyInt{Value: uint64(missionState)}},
		)
	}
	return marshal.PyDict{Items: items}
}

// agentMissionXtraInfo builds xtraInfo from a CharacterMission.
func agentMissionXtraInfo(m *evedb.CharacterMission, lp int) marshal.PyDict {
	state := int64(1) // offered
	if m.State == "accepted" {
		state = 2
	}
	items := []marshal.DictEntry{
		{Key: marshal.PyString{Value: "agentID"}, Value: marshal.PyInt{Value: uint64(m.AgentID)}},
		{Key: marshal.PyString{Value: "loyaltyPoints"}, Value: marshal.PyInt{Value: uint64(lp)}},
		{Key: marshal.PyString{Value: "missionID"}, Value: marshal.PyInt{Value: uint64(m.MissionID)}},
		{Key: marshal.PyString{Value: "missionState"}, Value: marshal.PyInt{Value: uint64(state)}},
	}
	if m.RewardISK > 0 {
		items = append(items, marshal.DictEntry{
			Key: marshal.PyString{Value: "rewardISK"}, Value: marshal.PyInt{Value: uint64(m.RewardISK)},
		})
	}
	if m.BonusISK > 0 {
		items = append(items, marshal.DictEntry{
			Key: marshal.PyString{Value: "bonusISK"}, Value: marshal.PyInt{Value: uint64(m.BonusISK)},
		})
	}
	if m.DestSystemID > 0 {
		items = append(items, marshal.DictEntry{
			Key: marshal.PyString{Value: "destSystemID"}, Value: marshal.PyInt{Value: uint64(m.DestSystemID)},
		})
	}
	return marshal.PyDict{Items: items}
}

// checkMissionComplete verifies completion conditions by mission type.
// Returns (complete, reason).  Item objectives count the station hangar plus
// the cargo holds of the character's ships docked at that station, so a courier
// run can be handed in without unloading first.
func checkMissionComplete(ctx context.Context, tmpl *evedb.MissionTemplate, mission *evedb.CharacterMission,
	charID, playerStationID, agentStationID int64, sqldb *sql.DB) (bool, string) {
	switch tmpl.MissionType {
	case "kill":
		switch tmpl.CompletionType {
		case "killItem":
			dropoff := resolveStationMode(ctx, tmpl.KillItemDropoffMode, tmpl.KillItemDropoffStationID, &evedb.AgentInfo{StationID: agentStationID, SolarSystemID: 0}, tmpl, sqldb)
			qty, _ := evedb.CountItemInHangar(ctx, sqldb, charID, dropoff, tmpl.KillItemTypeID)
			if qty < tmpl.KillItemQty {
				return false, fmt.Sprintf("Deliver %d x typeID %d to station %d (have %d).",
					tmpl.KillItemQty, tmpl.KillItemTypeID, dropoff, qty)
			}
			return true, ""
		default: // killAll, killTarget, killStructure
			// objectiveDone is the authority: the tally resets when the site respawns, so it only reports
			// progress toward an unfinished objective.
			if !mission.ObjectiveDone {
				return false, fmt.Sprintf("Kill progress: %d/%d.", mission.KillProgress, mission.KillRequired)
			}
			return true, ""
		}

	case "courier":
		dropoff := mission.DropoffStationID
		if dropoff == 0 {
			dropoff = agentStationID
		}
		qty, _ := evedb.CountItemInHangar(ctx, sqldb, charID, dropoff, tmpl.CourierTypeID)
		if qty < tmpl.CourierQty {
			return false, fmt.Sprintf("Deliver %d units to the destination hangar or keep them in your docked ship's cargo (have %d there).",
				tmpl.CourierQty, qty)
		}
		return true, ""

	case "trade":
		qty, _ := evedb.CountItemInHangar(ctx, sqldb, charID, agentStationID, tmpl.TradeTypeID)
		if qty < tmpl.TradeQty {
			return false, fmt.Sprintf("Bring %d units (have %d).", tmpl.TradeQty, qty)
		}
		return true, ""

	case "mining":
		qty, _ := evedb.CountItemInHangar(ctx, sqldb, charID, agentStationID, tmpl.MiningTypeID)
		if qty < tmpl.MiningQty {
			return false, fmt.Sprintf("Mine and deliver %d units (have %d).", tmpl.MiningQty, qty)
		}
		return true, ""
	}
	return false, "Unknown mission type."
}

// removeMissionItems removes courier/trade/mining/killItem items on completion
// and pushes OnItemChange notifications so the client's hangar and cargo views
// update immediately.  playerStationID is the station the character is docked
// in; item changes elsewhere are not notified.
func removeMissionItems(ctx context.Context, c *evenet.Conn, accountID int64, tmpl *evedb.MissionTemplate,
	mission *evedb.CharacterMission, charID, agentStationID, playerStationID int64, sqldb *sql.DB) {

	// take removes up to qty units of typeID, trying the stations in order; a mission can be
	// abandoned before the items reach the dropoff.
	take := func(typeID int64, qty int, stations ...int64) {
		if typeID <= 0 || qty <= 0 {
			return
		}
		seen := map[int64]bool{}
		remaining := qty
		for _, stationID := range stations {
			if remaining <= 0 || stationID <= 0 || seen[stationID] {
				continue
			}
			seen[stationID] = true
			removed, deltas, err := evedb.RemoveItemsFromHangar(ctx, sqldb, charID, stationID, typeID, remaining)
			if err != nil {
				log.Printf("removeMissionItems charID=%d station=%d typeID=%d: %v", charID, stationID, typeID, err)
			}
			remaining -= removed
			sendItemDeltas(c, accountID, stationID, playerStationID, deltas)
		}
		if remaining > 0 {
			log.Printf("removeMissionItems charID=%d typeID=%d: %d of %d units not found", charID, typeID, remaining, qty)
		}
	}

	switch tmpl.MissionType {
	case "kill":
		if tmpl.CompletionType == "killItem" && tmpl.KillItemTypeID > 0 {
			dropoff := resolveStationMode(ctx, tmpl.KillItemDropoffMode, tmpl.KillItemDropoffStationID,
				&evedb.AgentInfo{StationID: agentStationID}, tmpl, sqldb)
			take(tmpl.KillItemTypeID, tmpl.KillItemQty, dropoff, playerStationID)
		}
	case "courier":
		// Normally at the dropoff; on an abandoned mission the cargo may still
		// be sitting at the pickup station or wherever the player is docked.
		dropoff := mission.DropoffStationID
		if dropoff == 0 {
			dropoff = agentStationID
		}
		take(tmpl.CourierTypeID, tmpl.CourierQty, dropoff, mission.PickupStationID, playerStationID)
	case "trade":
		take(tmpl.TradeTypeID, tmpl.TradeQty, agentStationID, playerStationID)
	case "mining":
		take(tmpl.MiningTypeID, tmpl.MiningQty, agentStationID, playerStationID)
	}
}

// sendItemDeltas pushes one OnItemChange per entity change so the client
// updates its cached inventory without a redock.  itemStationID is where the
// items live: the client only tracks the inventories of the station it is
// docked in, so changes elsewhere are skipped.
func sendItemDeltas(c *evenet.Conn, accountID, itemStationID, playerStationID int64, deltas []evedb.ItemDelta) {
	if c == nil || len(deltas) == 0 || itemStationID != playerStationID {
		return
	}
	for _, d := range deltas {
		if d.Row == nil {
			continue
		}
		var change marshal.PyRep
		switch d.Kind {
		case evedb.ItemDeltaNew:
			// ixLocationID(3) old = 0 signals a brand-new item.
			change = marshal.PyDict{Items: []marshal.DictEntry{
				{Key: marshal.PyInt{Value: 3}, Value: marshal.PyInt{Value: 0}},
			}}
		case evedb.ItemDeltaQty:
			change = marshal.PyDict{Items: []marshal.DictEntry{
				{Key: marshal.PyInt{Value: 7}, Value: marshal.PyInt{Value: uint64(d.OldQty)}}, // ixQuantity
			}}
		default: // ItemDeltaRemoved: junkyard re-parent; the change must be a dict.
			_ = server.SendNotification(c, accountID, "OnItemChange", "charid", itemDeletedNotifArgs(d.Row))
			continue
		}
		_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
			evedb.BuildEntityRow(d.Row), change,
		}})
	}
}

// spawnMissionItem creates an item in charID's hangar at itemStationID and
// notifies the client so it appears without a redock.
func spawnMissionItem(ctx context.Context, c *evenet.Conn, accountID int64,
	charID, itemStationID, playerStationID, typeID int64, qty int, sqldb *sql.DB) (int64, error) {

	itemID, err := evedb.SpawnItemInHangar(ctx, sqldb, charID, itemStationID, typeID, qty)
	if err != nil {
		return 0, err
	}
	pushSpawnedItem(ctx, c, accountID, itemID, itemStationID, playerStationID, sqldb)
	return itemID, nil
}

// pushSpawnedItem tells the client about an item that already exists, so it
// appears in the hangar without a redock. spawnMissionItem owns the notification
// shape; callers that create the item elsewhere (the patent claim) call this.
func pushSpawnedItem(ctx context.Context, c *evenet.Conn, accountID,
	itemID, itemStationID, playerStationID int64, sqldb *sql.DB) {

	if er, gerr := evedb.GetEntityRow(ctx, sqldb, itemID); gerr == nil && er != nil {
		sendItemDeltas(c, accountID, itemStationID, playerStationID,
			[]evedb.ItemDelta{{Kind: evedb.ItemDeltaNew, Row: er}})
	}
}

// resolveStationMode picks a station based on mode: "agent" returns agent's station,
// "specific" returns the given stationID, "nearby" finds a random nearby station.
func resolveStationMode(ctx context.Context, mode string, specificID int64, agent *evedb.AgentInfo, tmpl *evedb.MissionTemplate, sqldb *sql.DB) int64 {
	switch mode {
	case "agent", "":
		return agent.StationID
	case "specific":
		if specificID > 0 {
			return specificID
		}
		return agent.StationID
	case "nearby":
		// Same jump-band weighting as the kill destinations. JumpBandOrder walks outward
		// from the rolled band; the agent's own station is excluded.
		bands, _ := evedb.GetNearbySystemsByJumps(ctx, sqldb, agent.SolarSystemID, tmpl.MinSecurity, tmpl.MaxSecurity)
		for _, depth := range evedb.JumpBandOrder(bands) {
			band := bands[depth]
			for _, i := range rand.Perm(len(band)) {
				stations, _ := evedb.GetStationsInSystem(ctx, sqldb, band[i])
				var eligible []int64
				for _, st := range stations {
					if st != agent.StationID {
						eligible = append(eligible, st)
					}
				}
				if len(eligible) > 0 {
					return eligible[rand.Intn(len(eligible))]
				}
			}
		}
		return agent.StationID
	}
	return agent.StationID
}

// timeToFileTime converts a time.Time to a Win32 FILETIME (100-ns ticks since 1601-01-01).
func timeToFileTime(t time.Time) uint64 {
	return uint64(t.UnixNano()/100) + 116444736000000000
}

// missionToJournalTuple builds a mission entry for GetMyJournalDetails.
// Client unpacks as: (missionState, importantMission, missionType, missionName, agentID, expirationTime, bookmarks)
// missionState: 0 allocated, 1 offered ("Not Accepted"), 2 accepted.
func missionToJournalTuple(ctx context.Context, m *evedb.CharacterMission, state int, sqldb *sql.DB) marshal.PyRep {
	tmpl, _ := evedb.GetTemplateByID(ctx, sqldb, m.TemplateID)
	name := "Mission"
	mType := "kill"
	if tmpl != nil {
		name = tmpl.MissionName
		mType = tmpl.MissionType
	}

	var expiresRep marshal.PyRep = marshal.PyNone{}
	if m.ExpiresAt.Valid {
		expiresRep = marshal.PyInt{Value: timeToFileTime(m.ExpiresAt.Time)}
	}

	// Location bookmarks only exist once the mission is accepted.
	bookmarks := marshal.PyList{}
	if state == 2 {
		bookmarks = missionBookmarks(ctx, m, name, sqldb)
	}

	return marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyInt{Value: uint64(state)},
		marshal.PyInt{Value: 0}, // importantMission
		marshal.PyString{Value: mType},
		marshal.PyString{Value: name},
		marshal.PyInt{Value: uint64(m.AgentID)},
		expiresRep,
		bookmarks,
	}}
}

// buildJournalDetails builds the (missions, offers, research) 3-tuple for
// GetMyJournalDetails. Offered missions ride the missions list with state 1; offers
// are the LP-store entries of every agent the character holds loyalty points with.
// agentFilter > 0 limits results to that agent (bound moniker refresh path).
func buildJournalDetails(ctx context.Context, sqldb *sql.DB, charID, agentFilter int64) marshal.PyRep {
	missionList := marshal.PyList{}
	offerList := marshal.PyList{}
	if charID != 0 && sqldb != nil {
		if accepted, err := evedb.GetAcceptedMissions(ctx, sqldb, charID); err == nil {
			for _, m := range accepted {
				if agentFilter > 0 && m.AgentID != agentFilter {
					continue
				}
				missionList.Items = append(missionList.Items, missionToJournalTuple(ctx, m, 2, sqldb))
			}
		}
		if offered, err := evedb.GetOfferedMissions(ctx, sqldb, charID); err == nil {
			for _, m := range offered {
				if agentFilter > 0 && m.AgentID != agentFilter {
					continue
				}
				missionList.Items = append(missionList.Items, missionToJournalTuple(ctx, m, 1, sqldb))
			}
		}
		if lpAgents, err := evedb.GetCharLPAgents(ctx, sqldb, charID); err == nil {
			for _, la := range lpAgents {
				if agentFilter > 0 && la.AgentID != agentFilter {
					continue
				}
				for _, o := range availableOffersForChar(ctx, sqldb, charID, la.AgentID, la.CorporationID, la.DivisionID) {
					var expiresRep marshal.PyRep = marshal.PyNone{}
					if o.ExpiresDays > 0 {
						expiresRep = marshal.PyInt{Value: timeToFileTime(time.Now().Add(time.Duration(o.ExpiresDays) * 24 * time.Hour))}
					}
					offerList.Items = append(offerList.Items, marshal.PyTuple{Items: []marshal.PyRep{
						marshal.PyInt{Value: uint64(la.AgentID)},
						marshal.PyString{Value: o.OfferName},
						marshal.PyInt{Value: uint64(o.LPCost)},
						expiresRep,
					}})
				}
			}
		}
	}
	// Research tab rows: journal.py unpacks (agentID, typeID, ppd, points)
	// and renders points/ppd with %-2.2f, so both must be reals.
	researchList := marshal.PyList{}
	if charID != 0 && sqldb != nil {
		if projects, err := evedb.GetCharacterResearchList(ctx, sqldb, charID, agentFilter); err == nil {
			now := time.Now()
			for _, r := range projects {
				researchList.Items = append(researchList.Items, marshal.PyTuple{Items: []marshal.PyRep{
					marshal.PyInt{Value: uint64(r.AgentID)},
					marshal.PyInt{Value: uint64(r.SkillTypeID)},
					marshal.PyReal{Value: r.PointsPerDay},
					marshal.PyReal{Value: r.CurrentPoints(now)},
				}})
			}
		}
	}
	return marshal.PyTuple{Items: []marshal.PyRep{
		missionList,  // missions (accepted + offered)
		offerList,    // offers (agent LP store)
		researchList, // research (R&D agent projects)
	}}
}

// advanceStorylineRun records one completed regular mission against the
// character's run and, at the threshold, mails the invite from a storyline agent
// of the same faction. See docs/main-s4-notes.md#storyline-run.
func advanceStorylineRun(ctx context.Context, c *evenet.Conn, accountID int64, addr string,
	sqldb *sql.DB, charID int64, agent *evedb.AgentInfo) {

	factionID, _ := evedb.GetAgentCorpFactionID(ctx, sqldb, agent.CorporationID)
	if factionID == 0 {
		return
	}
	streak, err := evedb.RecordMissionCompletion(ctx, sqldb, charID, factionID)
	if err != nil {
		log.Printf("[%s] storyline: RecordMissionCompletion charID=%d: %v", addr, charID, err)
		return
	}
	threshold := evedb.GetStorylineThreshold(ctx, sqldb)
	log.Printf("[%s] storyline: charID=%d faction=%d run %d/%d", addr, charID, factionID, streak, threshold)
	if streak < threshold {
		return
	}
	// One invite at a time: an outstanding one is not replaced.
	if p, err := evedb.GetStorylineProgress(ctx, sqldb, charID); err == nil && p.PendingAgentID != 0 {
		return
	}
	pick, err := evedb.PickStorylineAgent(ctx, sqldb, factionID, agent.Level, agent.SolarSystemID)
	if err != nil {
		log.Printf("[%s] storyline: PickStorylineAgent faction=%d: %v", addr, factionID, err)
		return
	}
	if pick == nil {
		log.Printf("[%s] storyline: faction %d has no storyline agent — run held at %d", addr, factionID, streak)
		return
	}
	if err := evedb.SetStorylinePending(ctx, sqldb, charID, pick.AgentID, factionID); err != nil {
		log.Printf("[%s] storyline: SetStorylinePending charID=%d: %v", addr, charID, err)
		return
	}
	sendStorylineInviteMail(ctx, sqldb, charID, pick)
	log.Printf("[%s] → storyline invite: charID=%d ← agent %d (%s) at %s",
		addr, charID, pick.AgentID, pick.AgentName, pick.StationName)
}

// sendStorylineInviteMail writes the agent's letter into the character's inbox.
// The sender is the agent, not CONCORD: the client renders the sender by owner
// lookup.
func sendStorylineInviteMail(ctx context.Context, sqldb *sql.DB, charID int64, pick *evedb.StorylineAgentPick) {
	where := pick.StationName
	if where == "" {
		where = "my station"
	}
	body := fmt.Sprintf(
		"Your recent work has been noted.<br><br>"+
			"I have an assignment that does not go through the usual channels, and I would "+
			"rather discuss it in person than commit the details to a letter. Come and see me "+
			"at <b>%s</b>%s when you are able.<br><br>"+
			"I will hold it open for you.<br><br>%s",
		where, systemSuffix(pick.SystemName), pick.AgentName)
	subject := "A special assignment"
	created := timeToFileTime(time.Now())

	msgID, err := evedb.SendEveMail(ctx, sqldb, charID, pick.AgentID, subject, body, int64(created))
	if err != nil {
		log.Printf("[storyline] invite mail to charID=%d: %v", charID, err)
		return
	}
	pushMailNotification(charID, msgID, pick.AgentID, subject, int64(created))
}

// systemSuffix renders " in <system>" for a mail body, or nothing when the
// system could not be resolved.
func systemSuffix(name string) string {
	if name == "" {
		return ""
	}
	return " in " + name
}

// pushMailNotification live-pushes OnMessage so an online recipient's inbox
// blinks without reopening it. Same contract LSC.Page uses for player mail.
func pushMailNotification(charID, msgID, senderID int64, subject string, created int64) {
	if globalLSCReg == nil {
		return
	}
	if msgID == 0 {
		return // suppressed mailbox: no row was written
	}
	args := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyList{Items: []marshal.PyRep{marshal.PyInt{Value: uint64(charID)}}},
		marshal.PyInt{Value: uint64(msgID)},
		marshal.PyInt{Value: uint64(senderID)},
		marshal.PyString{Value: subject},
		marshal.PyInt{Value: uint64(created)},
	}}
	for _, m := range globalLSCReg.OnlineRecipients(charID) {
		if m.CharID != charID {
			continue // OnlineRecipients also matches corp mailboxes
		}
		if err := server.SendNotification(m.Conn, m.AccountID, "OnMessage", "charid", args); err != nil {
			log.Printf("[mail] OnMessage push to charID=%d: %v", charID, err)
		}
	}
}

// storylineInvitePending reports whether this storyline agent is the one that
// invited the character. A storyline agent has nothing for anyone else: the
// invite is the whole gate, and it is earned by the run of missions counted in
// chrStorylineProgress, not by standing or level.
func storylineInvitePending(ctx context.Context, sqldb *sql.DB, charID, agentID int64) bool {
	p, err := evedb.GetStorylineProgress(ctx, sqldb, charID)
	if err != nil || p == nil {
		return false
	}
	return p.PendingAgentID == agentID
}

// storylineBrushOff is what a storyline agent says to a character who has not been
// sent to them. It is not the generic "no missions available" line, which reads as
// a broken agent.
func storylineBrushOff(ctx context.Context, sqldb *sql.DB, agent *evedb.AgentInfo, lp int, hasOffers bool) marshal.PyRep {
	var buttons []marshal.PyRep
	if hasOffers {
		buttons = append(buttons, shellBtn(ctx, sqldb, agent, "shell.btn.viewoffers", agentActionViewOffers))
	}
	buttons = append(buttons, shellBtn(ctx, sqldb, agent, "shell.btn.quit", agentActionQuit))
	return agentConversation(
		serviceText(ctx, sqldb, agent.DivisionID, "shell.storyline.brushoff", nil),
		buttons, agentXtraInfo(agent.AgentID, 0, 0, lp))
}

// availableOffersForChar returns the enabled offers an agent presents to a
// character, applying the minStanding gate. Which offers an agent presents is
// the authored corporation / faction / division allow-lists, not a binding to
// the agent itself.
func availableOffersForChar(ctx context.Context, sqldb *sql.DB, charID, agentID, corporationID, divisionID int64) []*evedb.AgentOffer {
	factionID, _ := evedb.GetAgentCorpFactionID(ctx, sqldb, corporationID)
	offers, err := evedb.GetOffersForAgent(ctx, sqldb, corporationID, factionID, divisionID)
	if err != nil {
		return nil
	}
	var out []*evedb.AgentOffer
	for _, o := range offers {
		if o.MinStanding > 0 && evedb.GetNPCStandingToChar(ctx, sqldb, charID, agentID) < o.MinStanding {
			continue
		}
		out = append(out, o)
	}
	return out
}

// notifyAgentMissionChange sends OnAgentMissionChange(what, agentID), then the
// whole-journal invalidate. what is offered/accepted/declined/completed/quit/failed.
// See docs/main-s4-notes.md#agent-mission-change.
func notifyAgentMissionChange(c *evenet.Conn, accountID int64, what string, agentID int64) {
	if c == nil {
		return
	}
	args := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyString{Value: what},
		marshal.PyInt{Value: uint64(agentID)},
	}}
	_ = server.SendNotification(c, accountID, "OnAgentMissionChange", "charid", args)
	notifyJournalInvalidate(c, accountID, what)
}

// notifyJournalInvalidate sends OnAgentMissionChange(what, None), the client's
// whole-journal invalidate (journal.py drops agentjournal and refetches). Sent alone
// for changes no agent popup tracks: agents.py passes on any `what` that begins
// with "research_".
func notifyJournalInvalidate(c *evenet.Conn, accountID int64, what string) {
	if c == nil {
		return
	}
	args := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyString{Value: what},
		marshal.PyNone{},
	}}
	_ = server.SendNotification(c, accountID, "OnAgentMissionChange", "charid", args)
}

// missionBookmarks builds the journal tuple's location bookmarks. Client consumers
// read attributes, so each is a util.KeyVal. The client echoes (locationType,
// locationNumber) back through GotoLocation/WarpToLocation; here that is
// (kind, missionID), and "dungeon" makes components.py mark the bookmark deadspace.
func missionBookmarks(ctx context.Context, m *evedb.CharacterMission, name string, sqldb *sql.DB) marshal.PyList {
	var rows []marshal.PyRep
	var created uint64
	if m.AcceptedAt.Valid {
		created = timeToFileTime(m.AcceptedAt.Time)
	}

	keyVal := func(entries []marshal.DictEntry) marshal.PyRep {
		return marshal.PyObject{Type: "util.KeyVal", Args: marshal.PyDict{Items: entries}}
	}
	kvInt := func(k string, v int64) marshal.DictEntry {
		return marshal.DictEntry{Key: marshal.PyString{Value: k}, Value: marshal.PyInt{Value: uint64(v)}}
	}
	kvStr := func(k, v string) marshal.DictEntry {
		return marshal.DictEntry{Key: marshal.PyString{Value: k}, Value: marshal.PyString{Value: v}}
	}
	kvReal := func(k string, v float64) marshal.DictEntry {
		return marshal.DictEntry{Key: marshal.PyString{Value: k}, Value: marshal.PyReal{Value: v}}
	}

	hasCoords := m.BookmarkX != 0 || m.BookmarkY != 0 || m.BookmarkZ != 0

	// Kill missions with deadspace coordinates.
	if hasCoords && m.DestSystemID > 0 {
		hint := fmt.Sprintf("%s (Deadspace)", name)
		rows = append(rows, keyVal([]marshal.DictEntry{
			kvInt("bookmarkID", m.MissionID),
			kvInt("ownerID", m.CharID),
			kvInt("itemID", m.DestSystemID),
			kvInt("typeID", 5), // Solar System
			{Key: marshal.PyString{Value: "flag"}, Value: marshal.PyNone{}},
			kvStr("memo", hint),
			kvStr("hint", hint),
			kvInt("created", int64(created)),
			kvReal("x", m.BookmarkX),
			kvReal("y", m.BookmarkY),
			kvReal("z", m.BookmarkZ),
			kvInt("locationID", m.DestSystemID),
			kvInt("solarsystemID", m.DestSystemID),
			kvInt("agentID", m.AgentID),
			kvStr("locationType", "dungeon"),
			kvInt("locationNumber", m.MissionID),
			kvInt("deadspace", 1),
		}))
	}

	// Courier/trade missions with destination station.
	if m.DestStationID > 0 && !hasCoords {
		var solarSystemID, stationTypeID int64
		var sx, sy, sz float64
		_ = sqldb.QueryRowContext(ctx, `
			SELECT solarSystemID, COALESCE(stationTypeID, 0),
			       COALESCE(x, 0), COALESCE(y, 0), COALESCE(z, 0)
			FROM staStations WHERE stationID = ?`, m.DestStationID,
		).Scan(&solarSystemID, &stationTypeID, &sx, &sy, &sz)
		hint := fmt.Sprintf("%s - Destination", name)
		rows = append(rows, keyVal([]marshal.DictEntry{
			kvInt("bookmarkID", m.MissionID),
			kvInt("ownerID", m.CharID),
			kvInt("itemID", m.DestStationID),
			kvInt("typeID", stationTypeID),
			{Key: marshal.PyString{Value: "flag"}, Value: marshal.PyNone{}},
			kvStr("memo", hint),
			kvStr("hint", hint),
			kvInt("created", int64(created)),
			kvReal("x", sx),
			kvReal("y", sy),
			kvReal("z", sz),
			kvInt("locationID", solarSystemID),
			kvInt("solarsystemID", solarSystemID),
			kvInt("agentID", m.AgentID),
			kvStr("locationType", "station"),
			kvInt("locationNumber", m.MissionID),
		}))
	}

	return marshal.PyList{Items: rows}
}

// runAgentWarpToLocation implements the agent moniker's GotoLocation / WarpToLocation
// (menusvc.py ApproachLocation/WarpToBookmark). The client echoes the bookmark's
// (locationType, locationNumber) pair; locationNumber is the missionID.
// The RPC response must already have been sent (startPlayerWarp contract).
func runAgentWarpToLocation(c *evenet.Conn, addr string, accountID, charID, agentID int64,
	locationType string, locationNumber int64, warpRange float64,
	sqldb *sql.DB, ds *destinyConn, sessReg *esp.SessionRegistry, npcMgr *npc.NPCManager) {

	if ds == nil || ds.dm == nil || ds.sysID == 0 || charID == 0 {
		log.Printf("[%s] WarpToLocation: not in space (charID=%d)", addr, charID)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	m, err := evedb.GetMissionByID(ctx, sqldb, locationNumber)
	if err != nil || m == nil || m.CharID != charID || m.State != "accepted" {
		log.Printf("[%s] WarpToLocation: no accepted mission %d for charID=%d (type=%q)",
			addr, locationNumber, charID, locationType)
		return
	}

	// Resolve the warp target: deadspace coords for kill missions, station
	// position for courier/trade destinations.
	var target destiny.Vec3
	destSystemID := m.DestSystemID
	hasCoords := m.BookmarkX != 0 || m.BookmarkY != 0 || m.BookmarkZ != 0
	switch {
	case hasCoords:
		target = destiny.Vec3{X: m.BookmarkX, Y: m.BookmarkY, Z: m.BookmarkZ}
	case m.DestStationID > 0:
		var sx, sy, sz float64
		if err := sqldb.QueryRowContext(ctx, `
			SELECT COALESCE(x,0), COALESCE(y,0), COALESCE(z,0), solarSystemID
			FROM staStations WHERE stationID = ?`, m.DestStationID,
		).Scan(&sx, &sy, &sz, &destSystemID); err != nil {
			log.Printf("[%s] WarpToLocation: station %d lookup: %v", addr, m.DestStationID, err)
			return
		}
		target = destiny.Vec3{X: sx, Y: sy, Z: sz}
	default:
		log.Printf("[%s] WarpToLocation: mission %d has no location", addr, m.MissionID)
		return
	}

	if destSystemID != 0 && destSystemID != ds.sysID {
		log.Printf("[%s] WarpToLocation: mission %d objective is in system %d, player in %d",
			addr, m.MissionID, destSystemID, ds.sysID)
		return
	}

	// Deadspace natural phenomena, the same rule as WarpToStuff. The RPC response has
	// gone out, so the refusal rides a notification with the same message key.
	if dungeonWarpBlocked(ds, target) {
		log.Printf("[%s] WarpToLocation: BLOCKED — deadspace natural phenomena (room %d, mission %d)",
			addr, ds.dunRoomID, m.MissionID)
		_ = server.SendNotification(c, accountID, "OnRemoteMessage", "clientID",
			marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyString{Value: "WarpingWithinGlobalDisruptors"},
				marshal.PyDict{},
			}})
		return
	}

	buffer := warpRange
	if hasCoords {
		// Deadspace coordinates are the site beacon: land at the range the pilot asked for,
		// or the deadspace buffer when none came with the call.
		buffer = siteLandingBuffer(warpRange, warpRange > 0)
	}
	if buffer < deadspaceLandingBuffer {
		buffer = deadspaceLandingBuffer
	}
	// Fixed grid threshold, independent of the landing buffer: this tests "already here".
	if dist := target.Sub(ds.dm.Pos).Len(); dist <= 25000 {
		// Already on grid: spawn the mission dungeon in place unless it is loaded.
		// dunObjCount()==0 means a warp departure stripped the site's balls, so it reloads.
		if ds.dunMissionID != m.MissionID || ds.dunObjCount() == 0 {
			if onArrive := missionDungeonOnArrive(ctx, c, addr, accountID, sqldb, ds, sessReg, npcMgr, m); onArrive != nil {
				log.Printf("[%s] WarpToLocation: mission %d already at location (dist=%.0f) — spawning dungeon in place", addr, m.MissionID, dist)
				onArrive()
				return
			}
		}
		log.Printf("[%s] WarpToLocation: mission %d already at location (dist=%.0f)", addr, m.MissionID, dist)
		return
	}

	// On arrival at a kill deadspace, spawn the mission's dungeon room with NPCs
	// tagged for kill tracking, unless the objective is done.
	onArrive := missionDungeonOnArrive(ctx, c, addr, accountID, sqldb, ds, sessReg, npcMgr, m)

	log.Printf("[%s] → agent.WarpToLocation missionID=%d type=%q target=(%.0f,%.0f,%.0f) range=%.0f spawn=%v",
		addr, m.MissionID, locationType, target.X, target.Y, target.Z, warpRange, onArrive != nil)
	startPlayerWarp(c, addr, accountID, sqldb, ds, ds.dm, target, 0, buffer, nil, onArrive)
}

// missionDungeonOnArrive returns a callback that spawns the mission's dungeon
// (room DungeonRoomIdx, NPCs tagged for kill tracking) and zeroes the kill tally.
// Returns nil when there is nothing to spawn: mission not accepted, no deadspace
// coordinates, not a kill mission with a dungeon, or the objective is done.
func missionDungeonOnArrive(ctx context.Context, c *evenet.Conn, addr string, accountID int64,
	sqldb *sql.DB, ds *destinyConn, sessReg *esp.SessionRegistry, npcMgr *npc.NPCManager,
	m *evedb.CharacterMission) func() {

	if m == nil || m.State != "accepted" {
		if m != nil {
			log.Printf("[%s] missionDungeonOnArrive: mission %d state=%q (need accepted), skipping dungeon", addr, m.MissionID, m.State)
		}
		return nil
	}
	if m.BookmarkX == 0 && m.BookmarkY == 0 && m.BookmarkZ == 0 {
		log.Printf("[%s] missionDungeonOnArrive: mission %d has no bookmark coordinates, skipping dungeon", addr, m.MissionID)
		return nil
	}
	tmpl, terr := evedb.GetTemplateByID(ctx, sqldb, m.TemplateID)
	if terr != nil || tmpl == nil || tmpl.MissionType != "kill" || tmpl.DungeonVID <= 0 {
		vid := int64(0)
		mt := ""
		if tmpl != nil {
			vid = tmpl.DungeonVID
			mt = tmpl.MissionType
		}
		log.Printf("[%s] missionDungeonOnArrive: mission %d template=%d type=%q dungeonVID=%d — no viable dungeon", addr, m.MissionID, m.TemplateID, mt, vid)
		return nil
	}
	// A finished objective with no live site has nothing to spawn; a live site still
	// reloads (warping off strips its rigid balls). The test is the durable objectiveDone flag.
	if m.ObjectiveDone && dunSiteFor(m.MissionID, false) == nil {
		log.Printf("[%s] missionDungeonOnArrive: mission %d objective already done and no active site, skipping dungeon", addr, m.MissionID)
		return nil
	}
	roomIDs, rerr := evedb.GetDungeonRoomIDs(ctx, sqldb, tmpl.DungeonVID)
	if rerr != nil || len(roomIDs) == 0 {
		log.Printf("[%s] missionDungeonOnArrive: mission %d dungeon VID=%d has no rooms (err=%v)", addr, m.MissionID, tmpl.DungeonVID, rerr)
		return nil
	}
	idx := tmpl.DungeonRoomIdx
	if idx < 1 || idx > len(roomIDs) {
		idx = 1
	}
	roomID := roomIDs[idx-1]
	missionID := m.MissionID
	return func() {
		rctx, rcancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer rcancel()
		// Fresh instance only: the tally restarts with the NPCs. A revisit within this
		// run keeps both the tally and the site's destroyed NPCs.
		if dunSiteFor(missionID, false) == nil {
			if err := evedb.ResetKillProgress(rctx, sqldb, missionID); err != nil {
				log.Printf("[%s] mission %d: %v", addr, missionID, err)
			}
		}
		// Stale gate travel from a previous dungeon must not shift this
		// room's anchor.
		ds.dunSpawnShift = destiny.Vec3{}
		if serr := spawnDungeonRoom(c, addr, accountID, sqldb, ds, roomID, sessReg, npcMgr, missionID); serr != nil {
			log.Printf("[%s] mission %d: spawnDungeonRoom %d: %v", addr, missionID, roomID, serr)
		}
		// Mark the dungeon instance as spawned so system-load doesn't
		// duplicate it if the system is torn down and reloaded.
		go func() {
			defer safego.Recover("missionDungeonOnArrive")
			dbCtx, dbCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer dbCancel()
			evedb.MarkDungeonInstanceSpawned(dbCtx, sqldb, missionID)
		}()
	}
}

// cleanupMissionDungeon removes the persistent dungeon instance, its site
// state (kills/waves), dungeon NPC DB records, and the in-memory site state
// for a mission that has reached a terminal state (completed, failed, quit).
func cleanupMissionDungeon(ctx context.Context, sqldb *sql.DB, missionID int64, logPrefix string) {
	if missionID <= 0 {
		return
	}
	dunSiteStates.Delete(missionID)
	if err := evedb.DeleteDungeonNPCsByMission(ctx, sqldb, missionID); err != nil {
		log.Printf("[%s] cleanupMissionDungeon: DeleteDungeonNPCsByMission %d: %v", logPrefix, missionID, err)
	}
	if err := evedb.DeleteDungeonInstance(ctx, sqldb, missionID); err != nil {
		log.Printf("[%s] cleanupMissionDungeon: DeleteDungeonInstance %d: %v", logPrefix, missionID, err)
	}
	log.Printf("[%s] cleanupMissionDungeon: cleaned up mission %d", logPrefix, missionID)
}

// completeMissionSiteIfClear finishes a killAll objective once the site is empty:
// no living NPCs (caller verified), no held wave groups unspawned in the room, and
// no NPC-capable objects in deeper rooms. The kill tally plays no part.
func completeMissionSiteIfClear(sqldb *sql.DB, sessReg *esp.SessionRegistry, missionID, roomID int64, spawned map[int64]bool) {
	defer safego.Recover("completeMissionSiteIfClear")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	m, err := evedb.GetMissionByID(ctx, sqldb, missionID)
	if err != nil || m == nil || m.State != "accepted" {
		return
	}
	tmpl, err := evedb.GetTemplateByID(ctx, sqldb, m.TemplateID)
	if err != nil || tmpl == nil || tmpl.CompletionType != "killAll" || tmpl.DungeonVID <= 0 {
		return
	}

	// Held wave groups: an NPC-capable object of this room that never spawned and is
	// not in the site's killed set means a trigger still has content to release.
	site := dunSiteFor(missionID, false)
	npcObjs, err := evedb.RoomNPCObjectIDs(ctx, sqldb, roomID)
	if err != nil {
		log.Printf("[mission] site-clear missionID=%d: %v", missionID, err)
		return
	}
	for _, oid := range npcObjs {
		if !spawned[oid] && !site.IsKilled(oid) {
			return
		}
	}

	// Deeper rooms with NPC content not yet destroyed: the site continues
	// past a gate.
	roomIDs, err := evedb.GetDungeonRoomIDs(ctx, sqldb, tmpl.DungeonVID)
	if err != nil {
		log.Printf("[mission] site-clear missionID=%d: %v", missionID, err)
		return
	}
	past := false
	for _, r := range roomIDs {
		if past {
			deeper, derr := evedb.RoomNPCObjectIDs(ctx, sqldb, r)
			if derr != nil {
				log.Printf("[mission] site-clear missionID=%d room %d: %v", missionID, r, derr)
				return
			}
			for _, oid := range deeper {
				if !site.IsKilled(oid) {
					return
				}
			}
		} else if r == roomID {
			past = true
		}
	}

	changed, err := evedb.CompleteKillObjective(ctx, sqldb, missionID)
	if err != nil {
		log.Printf("[mission] site-clear missionID=%d: %v", missionID, err)
		return
	}
	if !changed {
		return
	}
	log.Printf("[mission] missionID=%d site cleared → objective complete (charID=%d)", missionID, m.CharID)
	if sess := sessReg.FindByCharID(m.CharID); sess != nil && sess.Conn != nil {
		notifyAgentMissionChange(sess.Conn, sess.AccountID, "progress", m.AgentID)
	}
}

// parseWarpToLocationArgs extracts (locationType, locationNumber, warpRange)
// from a GotoLocation/WarpToLocation argument tuple.
// GotoLocation(locationType, locationNumber); WarpToLocation adds
// (warpRange, gang). warpRange defaults to 2500 (approach-distance landing).
func parseWarpToLocationArgs(args marshal.PyTuple) (locationType string, locationNumber int64, warpRange float64) {
	warpRange = 2500
	if len(args.Items) >= 1 {
		if s, ok := args.Items[0].(marshal.PyString); ok {
			locationType = s.Value
		}
	}
	if len(args.Items) >= 2 {
		if n, ok := args.Items[1].(marshal.PyInt); ok {
			locationNumber = int64(n.Value)
		}
	}
	if len(args.Items) >= 3 {
		switch v := args.Items[2].(type) {
		case marshal.PyReal:
			warpRange = v.Value
		case marshal.PyInt:
			warpRange = float64(v.Value)
		}
	}
	return
}

// handleMissionMgr handles calls to the "missionMgr" service.
// The client's base_missions.py iterates the result of GetAvailableCourierMissions,
// so returning None causes "TypeError: iteration over non-sequence" and a softlock.
func handleMissionMgr(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID, stationID int64, sqldb *sql.DB, sessReg *esp.SessionRegistry) error {
	var result marshal.PyRep
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	switch cs.Method {
	case "GetAvailableCourierMissions":
		result = buildAvailableCourierMissions(ctx, addr, sqldb, charID, stationID, cs.ArgTuple)
	case "GetAvailableEscrows":
		result = buildAvailableEscrows(ctx, addr, sqldb, charID, stationID, cs.ArgTuple)
	case "GetMyCourierMissions":
		result = buildAllMyCourierMissions(ctx, addr, sqldb, charID)
	case "AcceptCourierMission":
		return handleCourierAccept(ctx, addr, c, pkt, accountID, charID, cs, sqldb, sessReg)
	case "GetManifest":
		return handleCourierManifest(ctx, addr, c, pkt, accountID, cs, sqldb)
	case "CancelMission":
		return handleCourierCancel(ctx, addr, c, pkt, accountID, charID, cs, sqldb)
	case "DeliverCourierPackage":
		return handleCourierDeliver(ctx, addr, c, pkt, accountID, charID, cs, sqldb, sessReg)
	default:
		log.Printf("[%s] CallReq: stub missionMgr method=%q — returning None", addr, cs.Method)
		result = marshal.PyNone{}
	}
	return server.SendCallRsp(c, pkt, accountID, result)
}

// buildMyCourierMissions returns the accepted agent courier missions for a
// character as a list of util.KeyVal rows shaped for the journal's Courier
// Missions tab (journal.py ShowMyCourierMissions / missionentry.py).
func buildMyCourierMissions(ctx context.Context, addr string, sqldb *sql.DB, charID int64) marshal.PyRep {
	missions, err := evedb.GetAcceptedMissions(ctx, sqldb, charID)
	if err != nil {
		log.Printf("[%s] GetMyCourierMissions: %v", addr, err)
		return marshal.PyList{}
	}

	// stationLoc resolves a station's solar system and region once per call.
	type stnLoc struct{ solarSystemID, regionID int64 }
	locCache := map[int64]stnLoc{}
	stationLoc := func(stationID int64) stnLoc {
		if l, ok := locCache[stationID]; ok {
			return l
		}
		var l stnLoc
		if stationID > 0 {
			if err := sqldb.QueryRowContext(ctx, `
				SELECT COALESCE(solarSystemID,0), COALESCE(regionID,0)
				FROM staStations WHERE stationID = ?`, stationID,
			).Scan(&l.solarSystemID, &l.regionID); err != nil {
				log.Printf("GetMyCourierMissions: station %d lookup: %v", stationID, err)
			}
		}
		locCache[stationID] = l
		return l
	}

	rows := []marshal.PyRep{}
	for _, m := range missions {
		tmpl, terr := evedb.GetTemplateByID(ctx, sqldb, m.TemplateID)
		if terr != nil || tmpl == nil || tmpl.MissionType != "courier" {
			continue
		}
		pickup := stationLoc(m.PickupStationID)
		drop := stationLoc(m.DropoffStationID)

		var typeVolume float64
		if tmpl.CourierTypeID > 0 {
			if err := sqldb.QueryRowContext(ctx, `
				SELECT COALESCE(volume,0) FROM invTypes WHERE typeID = ?`, tmpl.CourierTypeID,
			).Scan(&typeVolume); err != nil {
				log.Printf("GetMyCourierMissions: invTypes %d volume: %v", tmpl.CourierTypeID, err)
			}
		}

		// The journal's countdown column rewrites itself every second from
		// dateExpires, so it must always be a valid future FILETIME.
		expires := time.Now().Add(7 * 24 * time.Hour)
		if m.ExpiresAt.Valid {
			expires = m.ExpiresAt.Time
		}

		agent, _ := evedb.GetAgentInfo(ctx, sqldb, m.AgentID)
		description := tmpl.BriefingText
		if agent != nil {
			mtp := evedb.BuildMissionTextParams(ctx, sqldb, charID, m.AgentID, agent, tmpl, m)
			description = evedb.SubstituteMissionText(description, mtp)
		}

		rows = append(rows, marshal.PyObject{
			Type: "util.KeyVal",
			Args: marshal.PyDict{Items: []marshal.DictEntry{
				{Key: marshal.PyString{Value: "missionID"}, Value: marshal.PyInt{Value: uint64(m.MissionID)}},
				{Key: marshal.PyString{Value: "issuerID"}, Value: marshal.PyInt{Value: uint64(m.AgentID)}},
				{Key: marshal.PyString{Value: "acceptorID"}, Value: marshal.PyInt{Value: uint64(charID)}},
				{Key: marshal.PyString{Value: "pickupStationID"}, Value: marshal.PyInt{Value: uint64(m.PickupStationID)}},
				{Key: marshal.PyString{Value: "dropStationID"}, Value: marshal.PyInt{Value: uint64(m.DropoffStationID)}},
				{Key: marshal.PyString{Value: "pickupSolarSystemID"}, Value: marshal.PyInt{Value: uint64(pickup.solarSystemID)}},
				{Key: marshal.PyString{Value: "dropSolarSystemID"}, Value: marshal.PyInt{Value: uint64(drop.solarSystemID)}},
				{Key: marshal.PyString{Value: "dropRegionID"}, Value: marshal.PyInt{Value: uint64(drop.regionID)}},
				{Key: marshal.PyString{Value: "totalReward"}, Value: marshal.PyInt{Value: uint64(m.RewardISK)}},
				{Key: marshal.PyString{Value: "acceptFee"}, Value: marshal.PyInt{Value: 0}},
				{Key: marshal.PyString{Value: "volume"}, Value: marshal.PyReal{Value: typeVolume * float64(tmpl.CourierQty)}},
				{Key: marshal.PyString{Value: "dateExpires"}, Value: marshal.PyInt{Value: timeToFileTime(expires)}},
				{Key: marshal.PyString{Value: "description"}, Value: marshal.PyString{Value: description}},
				{Key: marshal.PyString{Value: "craterID"}, Value: marshal.PyInt{Value: 0}},
			}},
		})
	}
	log.Printf("[%s] → missionMgr.GetMyCourierMissions charID=%d (%d rows)", addr, charID, len(rows))
	return marshal.PyList{Items: rows}
}

// emptyIndexRowset builds an empty util.IndexRowset with the given column header.
// The client can iterate data.items.iteritems() safely (empty dict → no rows).
func emptyIndexRowset(cols []string) marshal.PyRep {
	header := make([]marshal.PyRep, len(cols))
	for i, c := range cols {
		header[i] = marshal.PyString{Value: c}
	}
	return marshal.PyObject{
		Type: "util.IndexRowset",
		Args: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "header"}, Value: marshal.PyList{Items: header}},
			{Key: marshal.PyString{Value: "RowClass"}, Value: marshal.PyString{Value: "util.Row", IsType1: true}},
			{Key: marshal.PyString{Value: "idName"}, Value: marshal.PyString{Value: cols[0]}},
			{Key: marshal.PyString{Value: "items"}, Value: marshal.PyDict{}},
		}},
	}
}

// buildIndexRowset builds a util.IndexRowset with a populated items dict.
func buildIndexRowset(cols []string, items []marshal.DictEntry) marshal.PyRep {
	header := make([]marshal.PyRep, len(cols))
	for i, c := range cols {
		header[i] = marshal.PyString{Value: c}
	}
	return marshal.PyObject{
		Type: "util.IndexRowset",
		Args: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "header"}, Value: marshal.PyList{Items: header}},
			{Key: marshal.PyString{Value: "RowClass"}, Value: marshal.PyString{Value: "util.Row", IsType1: true}},
			{Key: marshal.PyString{Value: "idName"}, Value: marshal.PyString{Value: cols[0]}},
			{Key: marshal.PyString{Value: "items"}, Value: marshal.PyDict{Items: items}},
		}},
	}
}

// buildEntityRow builds a util.Row (EntityRowObject) from an EntityRow. Header:
// itemID, typeID, ownerID, locationID, flag, contraband, singleton, quantity,
// groupID, categoryID, customInfo.
func buildEntityRow(er *evedb.EntityRow) marshal.PyRep {
	return marshal.PyObject{
		Type: "util.Row",
		Args: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "header"}, Value: marshal.PyList{Items: []marshal.PyRep{
				marshal.PyString{Value: "itemID"},
				marshal.PyString{Value: "typeID"},
				marshal.PyString{Value: "ownerID"},
				marshal.PyString{Value: "locationID"},
				marshal.PyString{Value: "flag"},
				marshal.PyString{Value: "contraband"},
				marshal.PyString{Value: "singleton"},
				marshal.PyString{Value: "quantity"},
				marshal.PyString{Value: "groupID"},
				marshal.PyString{Value: "categoryID"},
				marshal.PyString{Value: "customInfo"},
			}}},
			{Key: marshal.PyString{Value: "line"}, Value: marshal.PyList{Items: []marshal.PyRep{
				marshal.PyInt{Value: uint64(er.ItemID)},
				marshal.PyInt{Value: uint64(er.TypeID)},
				marshal.PyInt{Value: uint64(er.OwnerID)},
				marshal.PyInt{Value: uint64(er.LocationID)},
				marshal.PyInt{Value: uint64(er.Flag)},
				marshal.PyInt{Value: 0}, // contraband = false
				marshal.PyInt{Value: uint64(er.Singleton)},
				marshal.PyInt{Value: uint64(er.Quantity)},
				marshal.PyInt{Value: uint64(er.GroupID)},
				marshal.PyInt{Value: uint64(er.CategoryID)},
				marshal.PyNone{}, // customInfo
			}}},
		}},
	}
}

// buildGetInfoEntry builds the 5-element PyList value for a Rsp_CommonGetInfo
// items dict entry, matching Rsp_CommonGetInfo_Entry::FastEncode():
// [itemID, invItem(util.Row), activeEffects{}, attributes{attrID:val}, time]
func buildGetInfoEntry(itemID int64, er *evedb.EntityRow, attrs map[int64]float64) marshal.PyRep {
	return buildGetInfoEntryWithEffects(itemID, er, attrs, nil)
}

// attrSkillLevel is dogma attribute 280 (skillLevel).
const attrSkillLevel = 280

// attrValueRep marshals a dogma attribute value. skillLevel goes out as an int: the
// client prints the raw value, so a float shows "LEVEL 1.0" in the skill window and
// "Train to level 2.0" in its menu.
func attrValueRep(attrID int64, val float64) marshal.PyRep {
	if attrID == attrSkillLevel && val >= 0 && val == math.Trunc(val) {
		return marshal.PyInt{Value: uint64(val)}
	}
	return marshal.PyReal{Value: val}
}

func buildGetInfoEntryWithEffects(itemID int64, er *evedb.EntityRow, attrs map[int64]float64, effects []marshal.DictEntry) marshal.PyRep {
	attrItems := make([]marshal.DictEntry, 0, len(attrs))
	for attrID, val := range attrs {
		attrItems = append(attrItems, marshal.DictEntry{
			Key:   marshal.PyInt{Value: uint64(attrID)},
			Value: attrValueRep(attrID, val),
		})
	}
	effectsDict := marshal.PyDict{}
	if len(effects) > 0 {
		effectsDict = marshal.PyDict{Items: effects}
	}
	return marshal.PyList{Items: []marshal.PyRep{
		marshal.PyInt{Value: uint64(itemID)}, // [0] itemID
		buildEntityRow(er),                   // [1] invItem
		effectsDict,                          // [2] activeEffects
		marshal.PyDict{Items: attrItems},     // [3] attributes
		// None: godma stamps the attributes with its own clock. A 0 seeds capacitor and shield
		// charge at the epoch, and the client then extrapolates both to full.
		marshal.PyNone{}, // [4] time
	}}
}

// dogmaIMInfoCols are the columns that Rsp_CommonGetInfo (util.IndexRowset)
// uses for both CharGetInfo and ShipGetInfo, matching
// packets/Inventory.h Rsp_CommonGetInfo_header.
var dogmaIMInfoCols = []string{"itemID", "invItem", "activeEffects", "attributes", "time"}

// invBrokerItemCols are the columns returned by InventoryBound.List(),
// matching InventoryDB::GetInventory columns.
var invBrokerItemCols = []string{
	"itemID", "typeID", "ownerID", "locationID", "flag",
	"contraband", "singleton", "quantity", "groupID", "categoryID", "customInfo",
}

// buildCharGetInfoRep returns the util.IndexRowset for dogmaIM.CharGetInfo: the
// character itself plus one entry per skill item and implant.
func buildCharGetInfoRep(addr string, charID int64, sqldb *sql.DB) marshal.PyRep {
	var itemsDict []marshal.DictEntry
	if charID != 0 && sqldb != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if charER, err := evedb.GetEntityRow(ctx, sqldb, charID); err != nil {
			log.Printf("[%s] bound.CharGetInfo GetEntityRow(char): %v", addr, err)
		} else if charER != nil {
			charAttrs, _ := evedb.GetEntityAttributes(ctx, sqldb, charID)
			if charAttrs == nil {
				charAttrs = make(map[int64]float64)
			}
			// The limits the skills set on the character itself; pushCharLimitAttrs re-sends
			// them when a skill finishes.
			for attrID, v := range charLimitAttrs(ctx, sqldb, charID) {
				charAttrs[attrID] = v
			}
			// Push effective character attributes 164..168: the client has no modifier engine and
			// feeds these raw into its SP ticker, so they must match what skillSPRate trains with.
			for attrID := int64(164); attrID <= 168; attrID++ {
				charAttrs[attrID] = evedb.GetEffectiveCharAttr(ctx, sqldb, charID, attrID)
			}
			// The blueprint window's "(You)" times (moduleattrs.go).
			for attrID, v := range industryCharAttrs(ctx, sqldb, charID) {
				charAttrs[attrID] = v
			}
			itemsDict = append(itemsDict, marshal.DictEntry{
				Key:   marshal.PyInt{Value: uint64(charID)},
				Value: buildGetInfoEntry(charID, charER, charAttrs),
			})
		}
		skills, skErr := evedb.GetCharacterSkillItems(ctx, sqldb, charID)
		if skErr != nil {
			log.Printf("[%s] bound.CharGetInfo GetCharacterSkillItems: %v", addr, skErr)
		}
		for _, sk := range skills {
			skER := &evedb.EntityRow{
				ItemID: sk.ItemID, TypeID: sk.TypeID,
				OwnerID: sk.OwnerID, LocationID: sk.LocationID,
				Flag: sk.Flag, GroupID: sk.GroupID, CategoryID: sk.CategoryID,
				Singleton: 1, Quantity: 1,
			}
			// Type-level attributes (skillTimeConstant 275, primaryAttribute 180,
			// secondaryAttribute 181) let the client compute SP targets; level and SP overlay them.
			skAttrs, _ := evedb.GetTypeAttributes(ctx, sqldb, sk.TypeID)
			if skAttrs == nil {
				skAttrs = make(map[int64]float64)
			}
			skAttrs[280] = float64(sk.Level)
			skAttrs[276] = float64(sk.SkillPoints)
			itemsDict = append(itemsDict, marshal.DictEntry{
				Key:   marshal.PyInt{Value: uint64(sk.ItemID)},
				Value: buildGetInfoEntry(sk.ItemID, skER, skAttrs),
			})
		}
		// Plugged-in implants (flag 89): godma shows these on the character sheet and
		// applies their attribute bonuses.
		implants, impErr := evedb.GetCharacterImplants(ctx, sqldb, charID)
		if impErr != nil {
			log.Printf("[%s] bound.CharGetInfo GetCharacterImplants: %v", addr, impErr)
		}
		for i := range implants {
			imp := &implants[i]
			impAttrs, _ := evedb.GetTypeAttributes(ctx, sqldb, imp.TypeID)
			if impAttrs == nil {
				impAttrs = make(map[int64]float64)
			}
			itemsDict = append(itemsDict, marshal.DictEntry{
				Key:   marshal.PyInt{Value: uint64(imp.ItemID)},
				Value: buildGetInfoEntry(imp.ItemID, imp, impAttrs),
			})
		}
	}
	log.Printf("[%s] → bound.CharGetInfo charID=%d items=%d", addr, charID, len(itemsDict))
	return buildIndexRowset(dogmaIMInfoCols, itemsDict)
}

// computeShipDerivedAttrs builds the ship's effective attribute map as ShipGetInfo
// reports it: type attrs plus entity overrides, recomputed cargo and CPU/PG outputs,
// skill, passive-module and hull-trait folds, runtime defaults, and turret/launcher
// slots left. Shared with the derived-stat pushes after fitting changes.
func computeShipDerivedAttrs(ctx context.Context, addr string, sqldb *sql.DB, charID, shipID int64, shipER *evedb.EntityRow) map[int64]float64 {
	// Start with type-level base attributes then let entity-specific overrides win.
	shipAttrs, _ := evedb.GetTypeAttributes(ctx, sqldb, shipER.TypeID)
	if shipAttrs == nil {
		shipAttrs = make(map[int64]float64)
	}
	if entityAttrs, _ := evedb.GetEntityAttributes(ctx, sqldb, shipID); entityAttrs != nil {
		for k, v := range entityAttrs {
			shipAttrs[k] = v
		}
	}
	// Recompute attribute 38 from base plus fitted cargo bonuses. Always overwrite: the
	// stored value is the unmodified hull base, so a fill-if-missing hides expanders.
	if cap, capErr := evedb.GetShipCargoCapacityFitted(ctx, sqldb, shipER.TypeID, shipID); capErr == nil && cap > 0 {
		shipAttrs[dogma.AttrCapacity] = cap
	}

	// Recompute cpuOutput (48) / powerOutput (11) from the capacity the online validation
	// enforces (GetShipCPUPGCapacityFitted). Always overwrite, or modules appear to online over grid.
	if cpuCap, pgCap, capErr := evedb.GetShipCPUPGCapacityFitted(ctx, sqldb, shipER.TypeID, shipID); capErr == nil {
		if cpuCap > 0 {
			shipAttrs[dogma.AttrCPUOutput] = cpuCap
		}
		if pgCap > 0 {
			shipAttrs[dogma.AttrPowerOutput] = pgCap
		}
	}
	// Sum CPU/PG load from fitted modules.
	cpuLoad, pgLoad, loadErr := evedb.GetShipCPUPowerLoad(ctx, sqldb, shipID)
	if loadErr != nil {
		log.Printf("[%s] bound.ShipGetInfo GetShipCPUPowerLoad: %v", addr, loadErr)
	}

	// Navigation skill velocity bonus (+5%/level) so the displayed
	// max velocity matches the destiny ball speed set at undock.
	if charID != 0 {
		if navLvl, nerr := evedb.GetCharacterSkillLevel(ctx, sqldb, charID, 3449); nerr == nil && navLvl > 0 {
			if v, exists := shipAttrs[dogma.AttrMaxVelocity]; exists && v > 0 {
				shipAttrs[dogma.AttrMaxVelocity] = v * (1.0 + float64(navLvl)*0.05)
			}
		}
		// Implants: the terms GetShipStats folds into the undock ball and AddTarget enforces
		// (velocity, agility, target range). Tank and cap implants arrive via the wrappers below.
		im := evedb.GetCharImplantMults(ctx, sqldb, charID)
		if v, exists := shipAttrs[dogma.AttrMaxVelocity]; exists && v > 0 && im.Velocity != 1 {
			shipAttrs[dogma.AttrMaxVelocity] = v * im.Velocity
		}
		if v, exists := shipAttrs[dogma.AttrAgility]; exists && v > 0 && im.Agility != 1 {
			shipAttrs[dogma.AttrAgility] = v * im.Agility
		}
		if v, exists := shipAttrs[dogma.AttrMaxTargetRange]; exists && v > 0 && im.TargetRange != 1 {
			shipAttrs[dogma.AttrMaxTargetRange] = v * im.TargetRange
		}
	}

	// Flat HP bonuses from fitted tank modules, applied before the shieldCharge default
	// below.
	if shieldAdd, armorAdd, bonErr := evedb.GetShipFittingHPBonuses(ctx, sqldb, shipID); bonErr == nil {
		if shieldAdd > 0 {
			shipAttrs[dogma.AttrShieldCapacity] = dogma.GetAttr(shipAttrs, dogma.AttrShieldCapacity, 0) + shieldAdd
		}
		if armorAdd > 0 {
			shipAttrs[dogma.AttrArmorHP] = dogma.GetAttr(shipAttrs, dogma.AttrArmorHP, 0) + armorAdd
		}
	}

	// Tank skills multiply the pools and shorten shield recharge, matching the combat
	// entity HP.
	if shieldSkill, armorSkill, hullSkill, rechargeSkill, tskErr := evedb.GetTankSkillMultipliers(ctx, sqldb, charID); tskErr == nil {
		if v, exists := shipAttrs[dogma.AttrShieldCapacity]; exists && shieldSkill != 1.0 {
			shipAttrs[dogma.AttrShieldCapacity] = v * shieldSkill
		}
		if v, exists := shipAttrs[dogma.AttrArmorHP]; exists && armorSkill != 1.0 {
			shipAttrs[dogma.AttrArmorHP] = v * armorSkill
		}
		if v, exists := shipAttrs[dogma.AttrHP]; exists && hullSkill != 1.0 {
			shipAttrs[dogma.AttrHP] = v * hullSkill
		}
		if v, exists := shipAttrs[dogma.AttrShieldRechargeRate]; exists && rechargeSkill != 1.0 {
			shipAttrs[dogma.AttrShieldRechargeRate] = v * rechargeSkill
		}
	}

	// Capacitor skills, matching the entity cap state set at undock (registerUndock InitCap).
	if capSkill, capRechSkill, cskErr := evedb.GetCapacitorSkillMultipliers(ctx, sqldb, charID); cskErr == nil {
		if v, exists := shipAttrs[dogma.AttrCapacitorCapacity]; exists && capSkill != 1.0 {
			shipAttrs[dogma.AttrCapacitorCapacity] = v * capSkill
		}
		if v, exists := shipAttrs[dogma.AttrRechargeRate]; exists && capRechSkill != 1.0 {
			shipAttrs[dogma.AttrRechargeRate] = v * capRechSkill
		}
	}

	// Passive module bonuses (fitting outputs, cap/shield/structure multipliers,
	// navigation hull mods), matching the combat state applied at undock.
	if pb, pbErr := evedb.GetShipPassiveBonuses(ctx, sqldb, shipID); pbErr == nil {
		mulAttr := func(attrID int64, mult float64) {
			if v, exists := shipAttrs[attrID]; exists && mult != 1.0 {
				shipAttrs[attrID] = v * mult
			}
		}
		// CPU/PG output multipliers are absent: GetShipCPUPGCapacityFitted already folded
		// them into cpuOutput/powerOutput above, and applying them again double-counts.
		mulAttr(dogma.AttrCapacitorCapacity, pb.CapCapMult)
		if pb.CapAdd != 0 {
			shipAttrs[dogma.AttrCapacitorCapacity] = dogma.GetAttr(shipAttrs, dogma.AttrCapacitorCapacity, 0) + pb.CapAdd
		}
		mulAttr(dogma.AttrRechargeRate, pb.CapRechargeMult)
		mulAttr(dogma.AttrShieldCapacity, pb.ShieldCapMult)
		mulAttr(dogma.AttrShieldRechargeRate, pb.ShieldRechargeMult)
		mulAttr(dogma.AttrHP, pb.StructureMult)
		mulAttr(dogma.AttrMaxVelocity, pb.VelocityMult)
		mulAttr(dogma.AttrAgility, pb.AgilityMult)
		mulAttr(dogma.AttrScanResolution, pb.ScanResMult)
		// Signal amplifiers' lock range: the client folds it onto its own
		// ship, and AddTarget enforces it, so the readout carries it too.
		if pb.TargetRangeMult > 0 {
			mulAttr(dogma.AttrMaxTargetRange, pb.TargetRangeMult)
		}
	}

	// Targeting skill folds that AddTarget enforces at lock time (Signature Analysis,
	// Long Range Targeting). Transient modifiers (running sensor boosters, damps) stay out.
	if charID != 0 {
		if lvl, serr := evedb.GetCharacterSkillLevel(ctx, sqldb, charID, dogma.SkillSignatureAnalysis); serr == nil && lvl > 0 {
			saAttrs, _ := evedb.GetTypeAttributes(ctx, sqldb, dogma.SkillSignatureAnalysis)
			if bonus := saAttrs[dogma.AttrScanResolutionBonus]; bonus > 0 {
				if v, exists := shipAttrs[dogma.AttrScanResolution]; exists && v > 0 {
					shipAttrs[dogma.AttrScanResolution] = v * (1.0 + bonus/100.0*float64(lvl))
				}
			}
		}
		if lvl, serr := evedb.GetCharacterSkillLevel(ctx, sqldb, charID, dogma.SkillLongRangeTargeting); serr == nil && lvl > 0 {
			lrAttrs, _ := evedb.GetTypeAttributes(ctx, sqldb, dogma.SkillLongRangeTargeting)
			if bonus := lrAttrs[dogma.AttrMaxTargetRangeBonus]; bonus > 0 {
				if v, exists := shipAttrs[dogma.AttrMaxTargetRange]; exists && v > 0 {
					shipAttrs[dogma.AttrMaxTargetRange] = v * (1.0 + bonus/100.0*float64(lvl))
				}
			}
		}
	}

	// Hull bonuses (shield/armor HP, cap, velocity, resistances), matching the combat
	// entity state.
	if hullB, hbErr := evedb.GetShipHullBonuses(ctx, sqldb, charID, shipER.TypeID); hbErr == nil && len(hullB) > 0 {
		hm := hullB.StatMults()
		mulHull := func(attrID int64, mult float64) {
			if v, exists := shipAttrs[attrID]; exists && mult != 1.0 {
				shipAttrs[attrID] = v * mult
			}
		}
		mulHull(dogma.AttrShieldCapacity, hm.ShieldHP)
		mulHull(dogma.AttrArmorHP, hm.ArmorHP)
		mulHull(dogma.AttrCapacitorCapacity, hm.CapCapacity)
		mulHull(dogma.AttrRechargeRate, hm.CapRecharge)
		// Cargo is omitted: attr 38 above already includes the hull cargo bonus.
		mulHull(dogma.AttrMaxVelocity, hm.Velocity)
		hullRes := hullB.ResistMults()
		shieldRes := dogma.ShieldResonanceAttrs()
		armorRes := dogma.ArmorResonanceAttrs()
		for d := 0; d < 4; d++ {
			dogma.SetDefault(shipAttrs, shieldRes[d], 1.0)
			dogma.SetDefault(shipAttrs, armorRes[d], 1.0)
			mulHull(shieldRes[d], hullRes[0][d])
			mulHull(armorRes[d], hullRes[1][d])
		}
	} else if hbErr != nil {
		log.Printf("[%s] bound.ShipGetInfo GetShipHullBonuses: %v", addr, hbErr)
	}

	// Gang bonuses (gangboost.go) come from the board: this function has a charID, no entity.
	// The recompute publishes the board before it pushes. A docked pilot has no entry.
	if charID != 0 {
		if gb := gang.Boosts.ForChar(charID); !gb.IsIdentity() {
			mulGang := func(attrID int64, stat dogma.GangStat) {
				if v, exists := shipAttrs[attrID]; exists && v > 0 {
					shipAttrs[attrID] = v * gb.Mult(stat)
				}
			}
			mulGang(dogma.AttrShieldCapacity, dogma.GSShieldHP)
			mulGang(dogma.AttrArmorHP, dogma.GSArmorHP)
			mulGang(dogma.AttrMaxVelocity, dogma.GSMaxVelocity)
			mulGang(dogma.AttrMaxTargetRange, dogma.GSMaxTargetRange)
			mulGang(dogma.AttrScanResolution, dogma.GSScanResolution)
			// Signature radius is not folded here: pushEwarVictimStats multiplies this base by
			// Entity.SigRadiusMult(), which already carries the gang term.
			shieldRes := dogma.ShieldResonanceAttrs()
			armorRes := dogma.ArmorResonanceAttrs()
			for d := 0; d < 4; d++ {
				dogma.SetDefault(shipAttrs, shieldRes[d], 1.0)
				dogma.SetDefault(shipAttrs, armorRes[d], 1.0)
				mulGang(shieldRes[d], dogma.GSShieldResonance)
				mulGang(armorRes[d], dogma.GSArmorResonance)
			}
		}
	}

	// Inject runtime attribute defaults (healthy ship).
	// Only set if not already present so entity_attributes overrides are preserved.
	dogma.SetDefault(shipAttrs, dogma.AttrDamage, 0.0)
	dogma.SetDefault(shipAttrs, dogma.AttrArmorDamage, 0.0)
	if _, ok := shipAttrs[dogma.AttrShieldCharge]; !ok {
		if cap, exists := shipAttrs[dogma.AttrShieldCapacity]; exists {
			shipAttrs[dogma.AttrShieldCharge] = cap
		}
	}
	if _, ok := shipAttrs[dogma.AttrCharge]; !ok {
		if cap, exists := shipAttrs[dogma.AttrCapacitorCapacity]; exists {
			shipAttrs[dogma.AttrCharge] = cap
		}
	}
	dogma.SetDefault(shipAttrs, dogma.AttrCPULoad, cpuLoad)
	dogma.SetDefault(shipAttrs, dogma.AttrPowerLoad, pgLoad)

	// Default shield/armor/structure damage resonances to 1.0 (0% resist)
	// if not already present, so the fitting window gauges render.
	for _, resAttr := range dogma.AllResonanceAttrs() {
		dogma.SetDefault(shipAttrs, resAttr, 1.0)
	}
	// Compute turretSlotsLeft and launcherSlotsLeft by subtracting
	// fitted turret/launcher modules from hull maximums.
	fittedTurrets, fittedLaunchers, _ := evedb.CountFittedTurretsLaunchers(ctx, sqldb, shipID)
	if v, exists := shipAttrs[dogma.AttrTurretSlotsLeft]; exists {
		shipAttrs[dogma.AttrTurretSlotsLeft] = v - float64(fittedTurrets)
	}
	if v, exists := shipAttrs[dogma.AttrLauncherSlotsLeft]; exists {
		shipAttrs[dogma.AttrLauncherSlotsLeft] = v - float64(fittedLaunchers)
	}
	return shipAttrs
}

func buildShipGetInfoRep(addr string, charID int64, sqldb *sql.DB) marshal.PyRep {
	var itemsDict []marshal.DictEntry
	var shipID int64
	if charID != 0 && sqldb != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var err error
		shipID, err = evedb.GetCurrentShipID(ctx, sqldb, charID)
		if err != nil {
			log.Printf("[%s] bound.ShipGetInfo GetCurrentShipID: %v", addr, err)
		}
		if shipID != 0 {
			shipER, err := evedb.GetEntityRow(ctx, sqldb, shipID)
			if err != nil {
				log.Printf("[%s] bound.ShipGetInfo GetEntityRow(ship=%d): %v", addr, shipID, err)
			}
			if shipER != nil {
				shipAttrs := computeShipDerivedAttrs(ctx, addr, sqldb, charID, shipID, shipER)

				modules, modErr := evedb.GetFittedModules(ctx, sqldb, shipID)
				if modErr != nil {
					log.Printf("[%s] bound.ShipGetInfo GetFittedModules: %v", addr, modErr)
				}

				itemsDict = append(itemsDict, marshal.DictEntry{
					Key:   marshal.PyInt{Value: uint64(shipID)},
					Value: buildGetInfoEntry(shipID, shipER, shipAttrs),
				})

				// Persisted per-module online state (attr 2): offline modules get no online effect
				// entry, so the UI shows them greyed (godma online.isActive).
				onlineIDs, onErr := evedb.GetOnlineModuleIDs(ctx, sqldb, shipID)
				if onErr != nil {
					log.Printf("[%s] bound.ShipGetInfo GetOnlineModuleIDs: %v", addr, onErr)
				}

				// Include fitted modules with online effect so the fitting window works.
				needMults := evedb.ModuleNeedMultsResolver(ctx, sqldb, shipER.OwnerID, shipER.TypeID)
				for _, mod := range modules {
					modAttrs, _ := evedb.GetTypeAttributes(ctx, sqldb, mod.TypeID)
					if modAttrs == nil {
						modAttrs = make(map[int64]float64)
					}
					// Weapons go out with the numbers they fire with (skills, hull, fitting, loaded
					// charge): godma displays what it is sent. Running tracking computers push on activation.
					loadedCharge, _ := evedb.GetLoadedCharge(ctx, sqldb, shipID, mod.Flag)
					var loadedChargeType int64
					var chargeER *evedb.EntityRow
					if loadedCharge != nil {
						chargeER, _ = evedb.GetEntityRow(ctx, sqldb, loadedCharge.ItemID)
						if chargeER != nil {
							loadedChargeType = chargeER.TypeID
						}
					}
					modEff, chargeEff := evedb.GetEffectiveWeaponAttrs(ctx, sqldb, charID, shipID, shipER.TypeID, mod.TypeID, loadedChargeType, nil)
					for k, v := range modEff {
						modAttrs[k] = v
					}
					// Fitting skills lower each module's own CPU/PG need, the
					// same fold GetShipCPUPowerLoad sums into the ship's load.
					needCPU, needPG := needMults(mod.TypeID)
					evedb.ApplyModuleNeedMults(modAttrs, needCPU, needPG)
					// Inject invTypes.capacity as attribute 38 if missing (turrets need this for ammo).
					if _, ok := modAttrs[dogma.AttrCapacity]; !ok {
						modCap, mcErr := evedb.GetShipCargoCapacity(ctx, sqldb, mod.TypeID)
						if mcErr == nil && modCap > 0 {
							modAttrs[dogma.AttrCapacity] = modCap
						}
					}
					isOnline := onlineIDs[mod.ItemID]
					if isOnline {
						modAttrs[dogma.AttrIsOnline] = 1
					} else {
						modAttrs[dogma.AttrIsOnline] = 0
					}
					modER := &evedb.EntityRow{
						ItemID: mod.ItemID, TypeID: mod.TypeID,
						OwnerID: mod.OwnerID, LocationID: mod.LocationID,
						Flag: mod.Flag, GroupID: mod.GroupID,
						CategoryID: mod.CategoryID, Singleton: 1, Quantity: 1,
					}
					var effects []marshal.DictEntry
					if isOnline {
						// Active online effect entry. godma.RefreshItemEffects slices env = d[:6] and reads
						// d[7..10] as startTime/duration/repeat/randomSeed, so the layout is the 7-slot env
						// plus those four: [itemID, charID, shipID, target, other, area, effectID, startTime,
						// duration, repeat, seed]. It is not the 11-arg OnGodmaShipEffect layout.
						onlineArgs := marshal.PyList{Items: []marshal.PyRep{
							marshal.PyInt{Value: uint64(mod.ItemID)},         // [0] env: self
							marshal.PyInt{Value: uint64(charID)},             // [1] env: char
							marshal.PyInt{Value: uint64(shipID)},             // [2] env: ship
							marshal.PyNone{},                                 // [3] env: target
							marshal.PyNone{},                                 // [4] env: other
							marshal.PyList{},                                 // [5] env: area
							marshal.PyInt{Value: uint64(dogma.EffectOnline)}, // [6] env: effectID
							server.Win32TimeNowRep(),                         // [7] startTime
							marshal.PyInt{Value: 60000},                      // [8] duration
							marshal.PyNone{},                                 // [9] repeat
							marshal.PyNone{},                                 // [10] randomSeed
						}}
						effects = []marshal.DictEntry{
							{Key: marshal.PyInt{Value: uint64(dogma.EffectOnline)}, Value: onlineArgs},
						}
					}
					itemsDict = append(itemsDict, marshal.DictEntry{
						Key:   marshal.PyInt{Value: uint64(mod.ItemID)},
						Value: buildGetInfoEntryWithEffects(mod.ItemID, modER, modAttrs, effects),
					})

					// Loaded charge in this module.
					if loadedCharge != nil {
						if chargeER != nil {
							chargeAttrs, _ := evedb.GetTypeAttributes(ctx, sqldb, chargeER.TypeID)
							if chargeAttrs == nil {
								chargeAttrs = make(map[int64]float64)
							}
							for k, v := range chargeEff {
								chargeAttrs[k] = v
							}
							itemsDict = append(itemsDict, marshal.DictEntry{
								Key:   marshal.PyInt{Value: uint64(loadedCharge.ItemID)},
								Value: buildGetInfoEntry(loadedCharge.ItemID, chargeER, chargeAttrs),
							})
						}
					}
				}
			}
		}
	}
	log.Printf("[%s] → bound.ShipGetInfo charID=%d shipID=%d items=%d", addr, charID, shipID, len(itemsDict))
	return buildIndexRowset(dogmaIMInfoCols, itemsDict)
}

// Container enum values from common/packet_types.h.
const (
	containerGlobal = 10002 // station-wide inventory (used by S&I)
	containerHangar = 10004 // station hangar
)

// hangarBindID is the bound-object ID used for InventoryBound objects that
// represent a station hangar (flag 4).  Skills use bindID 2; hangar uses 3.
const hangarBindID = 3

// cargoBindID is the bound-object ID used for InventoryBound objects that
// represent a ship's cargo hold (flag 5).
const cargoBindID = 4

// officesBindID / membersBindID are the bind numbers used for the corp
// offices and members SparseRowsets' server-side bound objects
// (see bound.GetOffices / bound.GetMembers / bound.Fetch / bound.FetchByKey).
const officesBindID = 7
const membersBindID = 8

// agentBindState tracks the agentMgr bound objects a connection has created. The
// client keeps one moniker per agentID, so the agentID must be recoverable from an
// incoming bound call's bind ID; one "last bound" variable answers agent A with B's data.
type agentBindState struct {
	last  int64          // most recent bind, for callers that send no bind ID
	bound map[int64]bool // agentIDs with a live bind on this connection
}

func (s *agentBindState) register(agentID int64) {
	if agentID <= 0 {
		return
	}
	if s.bound == nil {
		s.bound = make(map[int64]bool)
	}
	s.bound[agentID] = true
	s.last = agentID
}

// resolve returns the agent a bound call targets: the call's bind ID when it
// names an agent bound on this connection, else the most recent bind.
func (s *agentBindState) resolve(cs *server.CallStream) int64 {
	if s == nil {
		return 0
	}
	if n := bindNumFromCallStream(cs); n > 0 && s.bound[n] {
		return n
	}
	return s.last
}

// bindNumFromCallStream extracts the numeric bind ID from a bound call's remote
// object: the integer RemoteObject or the trailing ":<num>" of a bind-spec string
// like "N=888444:1234". Returns 0 when unparsable.
func bindNumFromCallStream(cs *server.CallStream) int64 {
	if cs.RemoteObject != 0 {
		return int64(cs.RemoteObject)
	}
	idx := strings.LastIndex(cs.RemoteObjectStr, ":")
	if idx < 0 || idx+1 >= len(cs.RemoteObjectStr) {
		return 0
	}
	n, err := strconv.ParseInt(cs.RemoteObjectStr[idx+1:], 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// isOfficeContainerID reports whether itemID is a corp-office inventory container:
// a station's office folder (the 66M to 68M band the client's cfg.IsOfficeFolder
// tests) or a rented office (a crpoffices itemID). Both bind by their own itemID.
func isOfficeContainerID(sqldb *sql.DB, itemID int64) bool {
	if evedb.IsOfficeFolderID(itemID) {
		return true
	}
	if sqldb == nil || itemID < 100000000 {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return evedb.IsOfficeItemID(ctx, sqldb, itemID)
}

// inlineBindMethod returns the method name from MachoBindObject's second
// argument (the optional inline call tuple), or "" if none is present.
func inlineBindMethod(cs *server.CallStream) string {
	if len(cs.ArgTuple.Items) < 2 {
		return ""
	}
	callTuple, ok := cs.ArgTuple.Items[1].(marshal.PyTuple)
	if !ok || len(callTuple.Items) < 1 {
		return ""
	}
	methodStr, ok := callTuple.Items[0].(marshal.PyString)
	if !ok {
		return ""
	}
	return methodStr.Value
}

// courierBindStationID returns the pickup station from missionMgr's bind
// params. moniker.GetCourierMissionCreator(stationID) binds the service with
// ('courier', stationID); anything else (or a malformed tuple) yields 0.
func courierBindStationID(cs *server.CallStream) int64 {
	if len(cs.ArgTuple.Items) < 1 {
		return 0
	}
	params, ok := cs.ArgTuple.Items[0].(marshal.PyTuple)
	if !ok || len(params.Items) < 2 {
		return 0
	}
	kind, ok := params.Items[0].(marshal.PyString)
	if !ok || kind.Value != "courier" {
		return 0
	}
	sid, ok := params.Items[1].(marshal.PyInt)
	if !ok {
		return 0
	}
	return int64(sid.Value)
}

// computeInlineBindResult runs the optional inline call from MachoBindObject's second
// argument and returns its result. moniker.Bind unpacks the response as
// (boundObj, callResult), so a bind with no inline call returns PyNone.
func computeInlineBindResult(c *evenet.Conn, accountID int64, addr, svc string, cs *server.CallStream, charID, stationID int64, sqldb *sql.DB, ds *destinyConn) marshal.PyRep {
	if len(cs.ArgTuple.Items) < 2 {
		return marshal.PyNone{}
	}
	callTuple, ok := cs.ArgTuple.Items[1].(marshal.PyTuple)
	if !ok {
		// Second arg is None or not a tuple: no inline call.
		return marshal.PyNone{}
	}
	if len(callTuple.Items) < 1 {
		return marshal.PyNone{}
	}
	methodStr, ok := callTuple.Items[0].(marshal.PyString)
	if !ok {
		return marshal.PyNone{}
	}
	// warRegistry moniker: the corp Wars tab inlines its first GetWars into the bind.
	// The result is cached unwrapped and read with .itervalues(), so None breaks the
	// tab; failure answers an empty dict, like the bound-call arms.
	if svc == "warRegistry" && methodStr.Value == "GetWars" {
		var warOwnerID int64
		if len(callTuple.Items) >= 2 {
			if argsTuple, ok := callTuple.Items[1].(marshal.PyTuple); ok && len(argsTuple.Items) >= 1 {
				if v, ok := argsTuple.Items[0].(marshal.PyInt); ok {
					warOwnerID = int64(v.Value)
				}
			}
		}
		ctxW, cancelW := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelW()
		if rowset, err := cachedWarsRowset(ctxW, sqldb, warOwnerID); err == nil {
			log.Printf("[%s] computeInline warRegistry.GetWars ownerID=%d", addr, warOwnerID)
			return rowset
		}
		return marshal.PyDict{}
	}
	if svc == "warRegistry" && methodStr.Value == "GetRelationships" {
		// Not a client call on this object (it is an allianceRegistry method);
		// keep the harmless empty dict for anything that wanders in.
		log.Printf("[%s] computeInline warRegistry.GetRelationships → empty dict", addr)
		return marshal.PyDict{}
	}

	// The POS management window's moniker (Moniker('posMgr', solarsystemid))
	// inlines its first call into the bind the same way the repair shop does.
	if svc == "posMgr" && posmgr.IsMethod(methodStr.Value) {
		var inlineArgs []marshal.PyRep
		if len(callTuple.Items) >= 2 {
			if argsTuple, ok := callTuple.Items[1].(marshal.PyTuple); ok {
				inlineArgs = argsTuple.Items
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		r, handled, err := posmgr.Result(ctx, addr, methodStr.Value, inlineArgs, charID, sqldb)
		if err != nil {
			log.Printf("[%s] computeInline posMgr.%s: %v", addr, methodStr.Value, err)
			return marshal.PyNone{}
		}
		if handled {
			return r
		}
	}

	// The research facility window binds a fresh factoryMgr moniker per open, so its
	// first GetResearchFacilitySlots arrives inline; corpStationMgr slot-rental calls can
	// too. A refusal cannot be raised inside a bind: it is logged and answered idle.
	if (svc == "factory" || svc == "corpStationMgr") && researchSlotMethods[methodStr.Value] {
		var inlineArgs []marshal.PyRep
		if len(callTuple.Items) >= 2 {
			if argsTuple, ok := callTuple.Items[1].(marshal.PyTuple); ok {
				inlineArgs = argsTuple.Items
			}
		}
		env := researchCallEnv{c: c, addr: addr, accountID: accountID, charID: charID,
			stationID: stationID, sqldb: sqldb, sessReg: globalSessReg}
		if ds != nil {
			env.shipID = ds.shipID
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		rep, uerr := researchSlotResult(ctx, env, methodStr.Value, inlineArgs)
		if uerr != nil {
			log.Printf("[%s] computeInline %s.%s refused: %v", addr, svc, methodStr.Value, uerr.dict)
			return researchSlotIdle(methodStr.Value)
		}
		return rep
	}

	// The repair shop's moniker inlines its first GetDamageReports into the
	// bind, so the quote list must be computed here rather than deferred.
	if svc == "repairSvc" && repairsvc.IsMethod(methodStr.Value) {
		var inlineArgs []marshal.PyRep
		if len(callTuple.Items) >= 2 {
			if argsTuple, ok := callTuple.Items[1].(marshal.PyTuple); ok {
				inlineArgs = argsTuple.Items
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		r, changed, handled, err := repairsvc.Result(ctx, addr, methodStr.Value, inlineArgs, charID, sqldb)
		repairsvc.NotifyChanged(c, accountID, changed, sqldb)
		if err != nil {
			log.Printf("[%s] computeInline repairSvc.%s: %v", addr, methodStr.Value, err)
			return marshal.PyNone{}
		}
		if handled {
			repairsvc.PushBalance(c, accountID, charID, methodStr.Value, sqldb)
			return r
		}
	}

	switch svc {
	case "dogmaIM":
		switch methodStr.Value {
		case "ShipGetInfo":
			rep := buildShipGetInfoRep(addr, charID, sqldb)
			boardPrimeServed(c)
			return rep
		case "CharGetInfo":
			return buildCharGetInfoRep(addr, charID, sqldb)
		case "CheckSendLocationInfo":
			// Godma Prime pools this on in-space session changes; None is a
			// valid "nothing to send" result.
			log.Printf("[%s] computeInline dogmaIM.CheckSendLocationInfo → None", addr)
			return marshal.PyNone{}
		case "GetTargets":
			// godma iterates both answers at service start in space; a None raises there.
			return lockedTargetList(ds)
		case "GetTargetedBy", "GetTargeters":
			return ballIDList(lockersOf(ds))
		}
	case "assetSvc":
		switch methodStr.Value {
		case "GetLocations":
			// Returns the character's assets dict.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			r, err := evedb.GetCharAssets(ctx, sqldb, charID)
			if err != nil {
				log.Printf("[%s] computeInline assetSvc.GetLocations: %v", addr, err)
				return marshal.PyDict{}
			}
			return r
		}
	case "invbroker":
		switch methodStr.Value {
		case "GetInventoryFromId":
			// GetInventoryFromId(entityID, passive): the current ship binds as cargo (flag 5),
			// the cases below bind by itemID, anything else uses generic bind 2 (skills etc.).
			bindNum := uint64(2)
			if len(callTuple.Items) >= 2 {
				if argTuple, ok := callTuple.Items[1].(marshal.PyTuple); ok && len(argTuple.Items) >= 1 {
					if eid, ok := argTuple.Items[0].(marshal.PyInt); ok && sqldb != nil && charID != 0 {
						ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
						shipID, _ := evedb.GetCurrentShipID(ctx, sqldb, charID)
						cancel()
						switch {
						case shipID != 0 && int64(eid.Value) == shipID:
							bindNum = cargoBindID
						case isStructureBay(ds, int64(eid.Value)):
							// POS structure bay or outpost platform hold: bind by itemID so List/Add resolve
							// against the structure's contents.
							bindNum = eid.Value
						case isDockedHangarPlatform(sqldb, ds, charID, int64(eid.Value)):
							// A docked caller configuring a platform still in the hangar: the password dialogs
							// need a bind that names the item.
							bindNum = eid.Value
						case isOfficeContainerID(sqldb, int64(eid.Value)):
							// Office folder or rented office: bind by itemID so List/Add resolve against the
							// corp hangar.
							bindNum = eid.Value
						case isOwnedContainerBind(sqldb, charID, int64(eid.Value)):
							// An assembled container of the pilot's own (alsc.go): bind by itemID for List/Add
							// and the password calls.
							bindNum = eid.Value
						case func() bool {
							cx, cn := context.WithTimeout(context.Background(), 5*time.Second)
							defer cn()
							_, ok := evedb.CourierCrateEscrowID(cx, sqldb, int64(eid.Value))
							return ok
						}():
							// Courier package: bind by itemID so List resolves the escrow container's contents.
							bindNum = eid.Value
						default:
							if ds != nil && ds.solarSystem != nil {
								if _, isLoot := ds.solarSystem.GetLootContainerPosition(uint32(eid.Value)); isLoot {
									// Loot container: bind by itemID, as the non-inline GetInventoryFromId path does.
									bindNum = eid.Value
								} else if bindNPCCargo(ds, sqldb, int64(eid.Value)) {
									// Spawn container / Overseer's Belongings
									// opened in place (npccargo.go).
									bindNum = eid.Value
								}
							}
						}
					}
				}
			}
			invBindSpec := fmt.Sprintf("N=%d:%d", globalNodeID, bindNum)
			return marshal.PySubStruct{Value: &marshal.PySubStream{
				Decoded: marshal.PyTuple{Items: []marshal.PyRep{
					marshal.PyString{Value: invBindSpec},
					marshal.PyInt{Value: 0},
				}},
			}}
		case "GetInventory":
			// GetInventory(containerEnum): containerHangar and containerGlobal use hangarBindID.
			// callTuple is (method_name, arguments_tuple, dict_args); Items[1][0] is the container int.
			bindNum := uint64(2)
			log.Printf("[%s] computeInline invbroker.GetInventory callTuple=%+v", addr, callTuple.Items)
			if len(callTuple.Items) >= 2 {
				if argTuple, ok := callTuple.Items[1].(marshal.PyTuple); ok && len(argTuple.Items) >= 1 {
					log.Printf("[%s] computeInline invbroker.GetInventory arg[0]=%T %+v", addr, argTuple.Items[0], argTuple.Items[0])
					if pt, ok := argTuple.Items[0].(marshal.PyInt); ok {
						cv := int64(pt.Value)
						if cv == containerHangar || cv == containerGlobal {
							bindNum = hangarBindID
						}
						// param1 is an ownerID for containerHangar: the MEMBER HANGARS pick asks for
						// another pilot's floor. See docs/corp-notes.md#member-hangars.
						if cv == containerHangar && len(argTuple.Items) >= 2 {
							if ow, ok := argTuple.Items[1].(marshal.PyInt); ok {
								if b := memberHangarBindNum(addr, charID, int64(ow.Value), stationID, sqldb); b != 0 {
									bindNum = uint64(b)
								}
							}
						}
					}
				} else {
					log.Printf("[%s] computeInline invbroker.GetInventory Items[1] is %T (not PyTuple)", addr, callTuple.Items[1])
				}
			}
			log.Printf("[%s] computeInline invbroker.GetInventory → bindNum=%d", addr, bindNum)
			invBindSpec := fmt.Sprintf("N=%d:%d", globalNodeID, bindNum)
			return marshal.PySubStruct{Value: &marshal.PySubStream{
				Decoded: marshal.PyTuple{Items: []marshal.PyRep{
					marshal.PyString{Value: invBindSpec},
					marshal.PyInt{Value: 0},
				}},
			}}
		}
	case "skillMgr":
		switch methodStr.Value {
		case "GetEndOfTraining":
			// The godma skill handler moniker inlines its first call into MachoBindObject; the
			// real end-of-training time gives the character sheet its ETA on the first query.
			var endOfTraining uint64
			if sqldb != nil && charID != 0 {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if st, err := evedb.GetSkillTraining(ctx, sqldb, charID); err == nil && st != nil {
					endOfTraining = st.EndOfTraining
				}
			}
			if endOfTraining == 0 {
				endOfTraining = server.Win32TimeNow() + 25920000000000
			}
			return marshal.PyInt{Value: endOfTraining}
		case "GetSkillsAndProgress":
			// Return (rowset-of-skills, empty-queue-tuple) so the skill tab
			// populates on first open via MachoBindObject inline result.
			skillCols := []string{"typeID", "skillPoints", "flag", "level"}
			header := make([]marshal.PyRep, len(skillCols))
			for i, col := range skillCols {
				header[i] = marshal.PyString{Value: col}
			}
			var lines []marshal.PyRep
			if sqldb != nil && charID != 0 {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				skills, err := evedb.GetCharacterSkillItems(ctx, sqldb, charID)
				if err != nil {
					log.Printf("[%s] computeInline skillMgr.GetSkillsAndProgress: %v", addr, err)
				} else {
					for _, sk := range skills {
						lines = append(lines, marshal.PyList{Items: []marshal.PyRep{
							marshal.PyInt{Value: uint64(sk.TypeID)},
							marshal.PyInt{Value: uint64(sk.SkillPoints)},
							marshal.PyInt{Value: uint64(sk.Flag)},
							marshal.PyInt{Value: uint64(sk.Level)},
						}})
					}
				}
			}
			skillsRowset := marshal.PyObject{
				Type: "util.Rowset",
				Args: marshal.PyDict{Items: []marshal.DictEntry{
					{Key: marshal.PyString{Value: "header"}, Value: marshal.PyList{Items: header}},
					{Key: marshal.PyString{Value: "RowClass"}, Value: marshal.PyString{Value: "util.Row", IsType1: true}},
					{Key: marshal.PyString{Value: "lines"}, Value: marshal.PyList{Items: lines}},
				}},
			}
			log.Printf("[%s] computeInline skillMgr.GetSkillsAndProgress charID=%d skills=%d", addr, charID, len(lines))
			return marshal.PyTuple{Items: []marshal.PyRep{
				skillsRowset,
				marshal.PyTuple{},
			}}
		case "GetSkillQueue":
			log.Printf("[%s] computeInline skillMgr.GetSkillQueue (empty)", addr)
			return marshal.PyList{}
		}
	case "jumpCloneSvc":
		switch methodStr.Value {
		case "GetCloneState":
			// Client unpacks as (jumpClones, jumpCloneImplants).
			return buildCloneStateRep(addr, charID, sqldb)
		}
	case "corpRegistry":
		switch methodStr.Value {
		case "GetEveOwners":
			// The caller's corporation as eveowners lines: bco_members keeps the IDs as the
			// member list and copies each .line into cfg.eveowners. See docs/corp-notes.md#member-hangars.
			return corpEveOwnersRep(addr, charID, sqldb)
		}
	case "missionMgr":
		switch methodStr.Value {
		case "GetCourierOptions":
			// moniker.GetCourierMissionCreator(stationID) binds with ('courier', stationID) and
			// inlines this call. The panel reads .basicFee and .itemList, so None wedges the Missions tab.
			sid := courierBindStationID(cs)
			if sid == 0 {
				sid = stationID
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			r, err := evedb.GetCourierOptions(ctx, sqldb, charID, sid)
			if err != nil {
				log.Printf("[%s] computeInline missionMgr.GetCourierOptions charID=%d stationID=%d: %v", addr, charID, sid, err)
			} else {
				log.Printf("[%s] computeInline missionMgr.GetCourierOptions charID=%d stationID=%d", addr, charID, sid)
			}
			return r
		}
	case "agentMgr":
		// Extract agentID from MachoBindObject first arg.
		inlineAgentID := int64(0)
		if len(cs.ArgTuple.Items) >= 1 {
			if aid, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
				inlineAgentID = int64(aid.Value)
			}
		}
		switch methodStr.Value {
		case "DoAction":
			inlineActionID := 0
			inlineActionText := ""
			if len(callTuple.Items) >= 2 {
				if argsTuple, ok := callTuple.Items[1].(marshal.PyTuple); ok && len(argsTuple.Items) >= 1 {
					switch v := argsTuple.Items[0].(type) {
					case marshal.PyInt:
						inlineActionID = int(v.Value)
					case marshal.PyString:
						inlineActionText = v.Value
					}
				}
			}
			log.Printf("[%s] computeInline agentMgr.DoAction agentID=%d actionID=%d text=%q", addr, inlineAgentID, inlineActionID, inlineActionText)
			return handleAgentDoAction(c, accountID, addr, inlineAgentID, charID, stationID, inlineActionID, inlineActionText, sqldb)
		case "GotoLocation", "WarpToLocation":
			// menusvc.ApproachLocation / WarpToBookmark discard the return value; the warp runs
			// as a side effect after the bind response (see the inline-bind block in the CALL_REQ handler).
			return marshal.PyNone{}
		case "GetInfoServiceDetails":
			// infosvc.py opens the Agent Info section for any NPC character; None makes the
			// client skip it, which is right for lore NPCs that are not agents.
			log.Printf("[%s] computeInline agentMgr.GetInfoServiceDetails agentID=%d", addr, inlineAgentID)
			return buildAgentInfoServiceDetails(addr, sqldb, inlineAgentID, charID)
		case "GetMissionJournalInfo", "GetMyJournalDetails", "GetOfferJournalInfo":
			// journal.py binds the agent moniker with its first call inlined.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			log.Printf("[%s] computeInline agentMgr.%s agentID=%d charID=%d", addr, methodStr.Value, inlineAgentID, charID)
			switch methodStr.Value {
			case "GetMissionJournalInfo":
				return missionJournalInfoRep(ctx, sqldb, charID, inlineAgentID)
			case "GetMyJournalDetails":
				return buildJournalDetails(ctx, sqldb, charID, inlineAgentID)
			default:
				return offerJournalInfoRep(ctx, sqldb, charID, inlineAgentID)
			}
		}
	case "insuranceSvc":
		switch methodStr.Value {
		case "GetContracts":
			// base_insurance.py:146 GetContracts iterates the result.
			// Return insured items rowset for this character.
			log.Printf("[%s] computeInline insuranceSvc.GetContracts charID=%d", addr, charID)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			r, err := evedb.GetInsuredItems(ctx, sqldb, charID)
			if err != nil {
				log.Printf("[%s] computeInline insuranceSvc.GetContracts: %v", addr, err)
				return marshal.PyList{}
			}
			return r
		}
	case "reprocessingSvc":
		switch methodStr.Value {
		case "GetOptionsForItemTypes":
			// base_reprocessingplant.py:188 calls .iterkeys() on the result.
			// Return a dict of typeID → {isRecyclable, isRefinable} for each type.
			log.Printf("[%s] computeInline reprocessingSvc.GetOptionsForItemTypes", addr)
			return computeReprocessingOptions(sqldb, charID, callTuple)
		}
	}
	// Inline calls answered with None here and run by the MachoBindObject handler after
	// the bind response goes out (undock needs the bind acked first, gate activation the FX).
	if inlineHandledPostBind(svc, methodStr.Value) {
		return marshal.PyNone{}
	}
	// An unrecognized inline call returns None. Callers that unpack or iterate the result
	// (godma Prime's ShipGetInfo) crash on it and wedge godma's primingChannel, freezing all
	// godma notifications until the next successful prime, so it is logged loudly.
	log.Printf("[%s] computeInline UNHANDLED inline bind call svc=%q method=%q → returning None", addr, svc, methodStr.Value)
	return marshal.PyNone{}
}

// inlineHandledPostBind reports whether svc.method is an inline bind call that the
// MachoBindObject handler runs itself (after the bind response; Board before it), so
// None here is correct. Keep in sync with the inline blocks in the MachoBindObject
// branch of the dispatch loop.
func inlineHandledPostBind(svc, method string) bool {
	switch svc {
	case "ship":
		switch method {
		case "Undock", "Board", "Drop", "ScoopDrone", "Scoop", "AssembleShip", "Eject", "Evacuate",
			"StoreVessel", "BoardStoredShip", "LaunchFromContainer", "ScoopToSMA":
			return true
		}
	case "skillMgr":
		switch method {
		case "CharStartTrainingSkill", "CharStopTrainingSkill",
			"CharAddImplant", "RemoveImplantFromCharacter":
			return true
		}
	case "keeper":
		return method == "ActivateAccelerationGate"
	case "agentMgr":
		return method == "GotoLocation" || method == "WarpToLocation"
	case "posMgr":
		return method == "AnchorStructure"
	case "missionMgr":
		return method == "CreateCourierMission"
	case "entity":
		return isDroneCommandMethod(method)
	}
	return false
}

// computeReprocessingOptions builds the dict result for reprocessingSvc.GetOptionsForItemTypes
// on the inline bind path. Delegates to the reprocessing package so the two
// entry points share one implementation.
func computeReprocessingOptions(sqldb *sql.DB, charID int64, callTuple marshal.PyTuple) marshal.PyRep {
	// callTuple layout: (methodName, argsTuple, dictArgs)
	// argsTuple[0] is the itemtypes dict {typeID: 0, ...}
	result := marshal.PyDict{}
	if len(callTuple.Items) < 2 {
		return result
	}
	argsTuple, ok := callTuple.Items[1].(marshal.PyTuple)
	if !ok || len(argsTuple.Items) < 1 {
		return result
	}
	itemTypesDict, ok := argsTuple.Items[0].(marshal.PyDict)
	if !ok {
		return result
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return reprocessing.OptionsForItemTypes(ctx, sqldb, itemTypesDict)
}

// handleBoundCall handles CallReq packets directed at a bound service object (empty
// service name). There is no full bind registry, so it dispatches on method name; a
// name shared by two bound objects is told apart by the bind number.
// See docs/corp-notes.md#alliance-binds.
func handleBoundCall(cc *callCtx) error {
	c, addr, cs, pkt, accountID, charID, stationID, agentBinds, sqldb, ds, sysMgr, sessReg, npcMgr := cc.c, cc.addr, cc.cs, cc.pkt, cc.accountID, cc.charID, cc.stationID, cc.agentBinds, cc.sqldb, cc.ds, cc.sysMgr, cc.sessReg, cc.npcMgr
	// Trade sessions bind by their container ID and share method names (List, Add,
	// GetCapacity) with the inventory bound object, so they get first refusal.
	if globalTradeReg != nil && sqldb != nil && charID != 0 {
		if containerID := bindNumFromCallStream(cs); containerID > 0 {
			handled, err := trademgr.HandleBound(c, addr, cs, pkt, accountID, charID,
				containerID, globalTradeReg, charConnLookup, sqldb)
			if handled {
				return err
			}
		}
	}

	// Repair shop methods are unambiguous, so the bound object they arrive on
	// does not need to be identified.
	if repairsvc.IsMethod(cs.Method) && sqldb != nil && charID != 0 {
		return repairsvc.Handle(c, addr, cs, pkt, accountID, charID, sqldb)
	}

	// aggressionMgr moniker: consider.py asks before taking from a container it does not
	// own. No loot-right exceptions are tracked; False sends the client to ConfirmStealItem.
	if cs.Method == "CheckLootRightExceptions" {
		log.Printf("[%s] → bound.CheckLootRightExceptions → False", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyBool{Value: false})
	}

	// Alliance bound object: identified by its bind number, because its method names
	// collide with corpRegistry's. See docs/corp-notes.md#alliance-binds.
	if allianceBoundMethods[cs.Method] && sqldb != nil && charID != 0 {
		if allianceID := allianceBindLookup(bindNumFromCallStream(cs)); allianceID > 0 {
			return handleAllianceBound(c, addr, cs, pkt, accountID, charID, allianceID, sqldb, sessReg)
		}
	}

	// Alliance calls on the corporation's own bound object (apply, my applications,
	// withdraw, ticker suggestions); the names are unique.
	if corpAllianceMethods[cs.Method] && sqldb != nil && charID != 0 {
		return handleCorpAllianceCall(c, addr, cs, pkt, accountID, charID, sqldb, sessReg)
	}

	// War calls for a corporation with no alliance (bco_wars.py). The alliance bind
	// check above claims the alliance-side names first.
	if corpWarMethods[cs.Method] && sqldb != nil && charID != 0 {
		return handleCorpWarCall(c, addr, cs, pkt, accountID, charID, sqldb, sessReg)
	}

	// corpRegistry write methods (corp edits, titles, members, votes, shares, lockdown,
	// application offers). See corpwrites.go.
	if corpWriteMethods[cs.Method] {
		return handleCorpRegistryWrite(c, addr, cs, pkt, accountID, charID, sqldb, sessReg)
	}

	// Research facility slots (factoryMgr + corpStationMgr slot rental), unique names.
	// Ahead of the office methods because a personal rental needs no corporation.
	if researchSlotMethods[cs.Method] {
		return handleResearchSlotCall(c, addr, cs, pkt, accountID, charID, stationID, ds, sqldb, sessReg)
	}

	// corpStationMgr office rental / HQ / corp-junk methods, unique names. The station
	// is the caller's docked station (the moniker binds with session.stationid).
	if corpStationMgrMethods[cs.Method] {
		return handleCorpStationMgr(c, addr, cs, pkt, accountID, charID, stationID, sqldb, sessReg)
	}
	// The STATION MGMT. window (stationmgmt.go), same bound object.
	if stationMgmtMethods[cs.Method] {
		return handleStationManagement(c, addr, cs, pkt, accountID, charID, stationID, sqldb)
	}

	// posMgr bound-object calls (Moniker('posMgr', solarsystemid)) have unique method
	// names. AnchorStructure needs live system state, so it is handled here, not in posmgr.
	if cs.Method == "AnchorStructure" {
		if itemID, x, y, z, ok := parseAnchorStructureArgs(cs.ArgTuple); ok {
			if err := anchorStructureForPlayer(ds, addr, charID, itemID, x, y, z, sqldb); err != nil {
				log.Printf("[%s] posMgr.AnchorStructure %d: %v", addr, itemID, err)
				posRefusal(ds, "Cannot anchor", err)
			}
		}
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	if posmgr.IsMethod(cs.Method) {
		return posmgr.Handle(c, addr, cs, pkt, accountID, charID, sqldb)
	}

	switch cs.Method {
	case "CharGetInfo":
		return server.SendCallRsp(c, pkt, accountID, buildCharGetInfoRep(addr, charID, sqldb))

	case "ShipGetInfo":
		err := server.SendCallRsp(c, pkt, accountID, buildShipGetInfoRep(addr, charID, sqldb))
		boardPrimeServed(c)
		return err

	case "ListStations":
		return boundListStations(cc)

	case "ListStationBlueprintItems":
		return boundListStationBlueprintItems(cc)

	case "ListStationItems":
		return boundListStationItems(cc)

	case "GetLocations":
		// assetSvc bound: GetLocations(ownerID) → PyDict{stationID: Rowset}.
		var result marshal.PyRep = marshal.PyDict{}
		if charID != 0 && sqldb != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if r, err := evedb.GetCharAssets(ctx, sqldb, charID); err != nil {
				log.Printf("[%s] bound.GetLocations db: %v", addr, err)
			} else {
				result = r
			}
		}
		log.Printf("[%s] → bound.GetLocations charID=%d", addr, charID)
		return server.SendCallRsp(c, pkt, accountID, result)

	case "GetInventory":
		return boundGetInventory(cc)

	case "List":
		return boundList(cc)

	case "GetItem":
		return boundGetItem(cc)

	case "BreakPlasticWrap":
		return handleBreakPlasticWrap(c, addr, pkt, accountID, cs)
	case "MultiMerge":
		return boundMultiMerge(cc)

	case "SetPassword":
		return boundSetPassword(cc)

	// ── Secure containers / ALSC (alsc.go) ─────────────────────────────────
	case "EnterPassword":
		return handleContainerEnterPassword(c, addr, cs, pkt, accountID, charID, sqldb)
	case "ALSCConfigGet", "ALSCConfigSet":
		return handleALSCConfig(c, addr, cs, pkt, accountID, charID, sqldb)
	case "ALSCLogGet":
		return handleALSCLogGet(c, addr, cs, pkt, accountID, charID, sqldb)
	case "LockItem":
		return handleContainerLock(c, addr, cs, pkt, accountID, charID, sqldb, true)
	case "UnlockItem":
		return handleContainerLock(c, addr, cs, pkt, accountID, charID, sqldb, false)
	case "RetrievePassword":
		return handleContainerRetrievePassword(c, addr, cs, pkt, accountID, charID, sqldb)
	case "AssembleCargoContainer":
		return handleAssembleContainer(c, addr, cs, pkt, accountID, charID, sqldb)

	case "Build":
		// InventoryBound.Build([securityCode=...]) is the outpost platform's Build menu entry.
		// The bind number is the platform's itemID (set in GetInventoryFromId).
		return buildOutpostForPlayer(c, addr, pkt, accountID, charID,
			bindNumFromCallStream(cs), buildSecurityCode(cs), sqldb, ds)

	case "StackAll":
		return boundStackAll(cc)

	case "GetMoney":
		log.Printf("[%s] → bound.GetMoney (0.0)", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyReal{Value: 0.0})

	case "GetInventoryFromId":
		return boundGetInventoryFromId(cc)

	case "GetEveOwners":
		// corpRegistry bound: the caller's corporation as eveowners lines.
		return server.SendCallRsp(c, pkt, accountID, corpEveOwnersRep(addr, charID, sqldb))

	case "GetCorporation":
		return boundGetCorporation(cc)

	case "GetCorporateStationInfo":
		return boundGetCorporateStationInfo(cc)

	case "GangRegroup":
		// beyonce bound: the gang leader's "Regroup" (gangwarp.go).
		return handleGangRegroup(c, addr, pkt, accountID, ds)

	case "GotoDirection", "GotoBookmark", "ConeScan", "GetScans", "CombineScans", "GiveProbe",
		"Stop", "SetSpeedFraction", "UpdateStateRequest",
		"FollowBall", "Orbit", "WarpToStuff", "StargateJump", "GetRelativity":
		return handleDestinyBoundCall(cc)

	case "Board", "Undock", "AssembleShip", "Eject", "Evacuate",
		"StoreVessel", "BoardStoredShip", "LaunchFromContainer", "ScoopToSMA":
		// The EVE client binds the "ship" service and sends these via the bound
		// object (pkt.Dest.Service == ""), so we delegate to handleShipSvc.
		return handleShipSvc(c, addr, cs, pkt, accountID, charID, stationID, sqldb, ds, sysMgr)

	case "Drop":
		return boundDrop(cc)

	case "ScoopDrone":
		var scoopIDs []int64
		if len(cs.ArgTuple.Items) >= 1 {
			scoopIDs = parseDroneIDs(cs.ArgTuple.Items[0])
		}
		log.Printf("[%s] → ship.ScoopDrone %v", addr, scoopIDs)
		scoopDronesForPlayer(ds, charID, scoopIDs, sqldb)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})

	case "Scoop":
		// ship.Scoop(objectID[, password]) is "Scoop To Cargo Hold" on a free structure (starbase.go).
		scoopFromCall(ds, addr, charID, cs.ArgTuple, sqldb)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})

	case "CmdEngage", "CmdMine", "CmdMineRepeatedly", "CmdReturnHome",
		"CmdReturnBay", "CmdDelegateControl", "CmdRelinquishControl", "CmdUnanchor":
		// entity service drone commands (bound-object path).
		return server.SendCallRsp(c, pkt, accountID, handleDroneCommand(cs.Method, cs.ArgTuple, ds, charID))

	case "Dock", "TurboDock":
		return boundDock(cc)

	case "GetCloneState":
		// jumpCloneSvc bound: returns (jumpClones, jumpCloneImplants).
		return server.SendCallRsp(c, pkt, accountID, buildCloneStateRep(addr, charID, sqldb))

	case "InstallCloneInStation":
		return boundInstallCloneInStation(cc)

	case "CloneJump":
		return handleCloneJump(c, addr, cs, pkt, accountID, charID, stationID, ds, sqldb)

	case "DestroyInstalledClone":
		return boundDestroyInstalledClone(cc)

	case "GetShipCloneState", "GetStationCloneState":
		// Ship clone vat bays are not modelled: no clones. The ship configuration window
		// iterates the answer, so it is an empty rowset, never None.
		return server.SendCallRsp(c, pkt, accountID,
			makeInvRowset([]string{"jumpCloneID", "ownerID", "typeID", "locationID"}, nil))

	case "OfferShipCloneInstallation", "AcceptShipCloneInstallation", "CancelShipCloneInstallation":
		// clonejumpsvc.OfferShipCloneInstallation(charID) is drawn for any ship with a Clone Vat Bay
		// (23735). Ship clones are not modelled; an error keeps the offer from vanishing as a None would.
		return server.SendCallException(c, pkt, accountID, server.BuildUserError(
			"CustomInfo", map[string]marshal.PyRep{
				"info": marshal.PyString{Value: "This ship's clone vat bay is not operational."},
			}))

	case "GetPriceForClone":
		return server.SendCallRsp(c, pkt, accountID, marshal.PyInt{Value: 100000})

	case "GetInfoWindowDataForChar":
		return boundGetInfoWindowDataForChar(cc)

	case "GetTargets":
		targets := lockedTargetList(ds)
		log.Printf("[%s] → bound.GetTargets (%d targets)", addr, len(targets.Items))
		return server.SendCallRsp(c, pkt, accountID, targets)

	case "GetTargetedBy", "GetTargeters":
		targeters := lockersOf(ds)
		log.Printf("[%s] → bound.%s (%d targeters)", addr, cs.Method, len(targeters))
		return server.SendCallRsp(c, pkt, accountID, ballIDList(targeters))

	case "AddTarget":
		return boundAddTarget(cc)

	case "RemoveTarget":
		return boundRemoveTarget(cc)

	case "ClearTargets":
		return boundClearTargets(cc)

	case "Activate":
		return boundActivate(cc)

	case "Deactivate":
		return boundDeactivate(cc)

	case "CharStartTrainingSkill":
		return boundCharStartTrainingSkill(cc)

	case "CharStopTrainingSkill":
		return boundCharStopTrainingSkill(cc)

	case "GetEndOfTraining":
		return boundGetEndOfTraining(cc)

	case "AddCorporation":
		return handleBoundAddCorporation(c, addr, cs, pkt, accountID, charID, sqldb)

	case "CreateAlliance":
		return handleBoundCreateAlliance(c, addr, cs, pkt, accountID, charID, sqldb, sessReg)

	case "GetMembers":
		return boundGetMembers(cc)

	case "GetMember":
		corpID := boundCharCorpID(charID, sqldb)
		targetID := boundArgInt(cs, 0)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		row, err := evedb.GetCorpMember(ctx, sqldb, corpID, targetID)
		if err != nil {
			log.Printf("[%s] bound.GetMember db: %v", addr, err)
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		if row == nil {
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		log.Printf("[%s] → bound.GetMember corpID=%d char=%d", addr, corpID, targetID)
		return server.SendCallRsp(c, pkt, accountID, row)

	case "GetMemberIDsByQuery":
		return boundGetMemberIDsByQuery(cc)

	case "GetMemberIDsWithMoreThanAvgShares":
		// The kick-vote candidate pool: members whose shareholding exceeds
		// the per-member average.  Flat list of charIDs.
		corpID := boundCharCorpID(charID, sqldb)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ids := evedb.GetMemberIDsWithMoreThanAvgShares(ctx, sqldb, corpID)
		items := make([]marshal.PyRep, 0, len(ids))
		for _, id := range ids {
			items = append(items, marshal.PyInt{Value: uint64(id)})
		}
		log.Printf("[%s] → bound.GetMemberIDsWithMoreThanAvgShares corpID=%d (%d)", addr, corpID, len(ids))
		return server.SendCallRsp(c, pkt, accountID, marshal.PyList{Items: items})

	case "GetTitles":
		corpID := boundCharCorpID(charID, sqldb)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// The titles UI walks titleID 1<<0..1<<7 positionally, so the eight
		// default rows must exist before the first read.
		if err := evedb.EnsureDefaultTitles(ctx, sqldb, corpID); err != nil {
			log.Printf("[%s] EnsureDefaultTitles: %v", addr, err)
		}
		result, err := evedb.GetCorpTitles(ctx, sqldb, corpID)
		if err != nil {
			log.Printf("[%s] bound.GetTitles db: %v", addr, err)
		}
		log.Printf("[%s] → bound.GetTitles corpID=%d", addr, corpID)
		return server.SendCallRsp(c, pkt, accountID, result)

	case "GetRoles":
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		result, err := evedb.GetCorpRoles(ctx, sqldb)
		if err != nil {
			log.Printf("[%s] bound.GetRoles db: %v", addr, err)
		}
		log.Printf("[%s] → bound.GetRoles", addr)
		return server.SendCallRsp(c, pkt, accountID, result)

	case "GetRoleGroups":
		log.Printf("[%s] → bound.GetRoleGroups (empty)", addr)
		return server.SendCallRsp(c, pkt, accountID, evedb.GetCorpRoleGroups(context.Background(), sqldb))

	case "GetOffices":
		// Must be a util.SparseRowset: the client calls .Fetch() and .RegisterObjectChangeHandler()
		// on it (corp_ui_member_find.py, rowset.py). A plain util.Rowset raises in
		// CorporationUI.Initialize and leaves the corp window empty.
		corpID := boundCharCorpID(charID, sqldb)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		count := evedb.CountCorpOffices(ctx, sqldb, corpID)
		bindSpec := fmt.Sprintf("N=%d:%d", globalNodeID, officesBindID)
		log.Printf("[%s] → bound.GetOffices corpID=%d (sparse, %d rows, %q)", addr, corpID, count, bindSpec)
		return server.SendCallRsp(c, pkt, accountID, evedb.MakeOfficesSparseRowset(bindSpec, count))

	case "Fetch":
		// SparseRowset back-fill: Fetch(startPos, fetchSize) returns [(pk, line), ...]. The bind
		// suffix says which sparse rowset is paged (offices or members; see GetOffices / GetMembers).
		corpID := boundCharCorpID(charID, sqldb)
		startPos, fetchSize := boundArgInt(cs, 0), boundArgInt(cs, 1)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if strings.HasSuffix(cs.RemoteObjectStr, fmt.Sprintf(":%d", membersBindID)) {
			log.Printf("[%s] → bound.Fetch (members) corpID=%d start=%d size=%d", addr, corpID, startPos, fetchSize)
			return server.SendCallRsp(c, pkt, accountID, evedb.FetchCorpMembers(ctx, sqldb, corpID, startPos, fetchSize))
		}
		log.Printf("[%s] → bound.Fetch (offices) corpID=%d start=%d size=%d", addr, corpID, startPos, fetchSize)
		return server.SendCallRsp(c, pkt, accountID, evedb.FetchCorpOffices(ctx, sqldb, corpID, startPos, fetchSize))

	case "FetchByKey":
		return boundFetchByKey(cc)

	case "RegisterObjectChangeHandler", "UnregisterObjectChangeHandler":
		// Acknowledged only: no office change notifications are pushed.
		log.Printf("[%s] → bound.%s (ack)", addr, cs.Method)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})

	case "GetStations":
		corpID := boundCharCorpID(charID, sqldb)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		result, err := evedb.GetCorpStations(ctx, sqldb, corpID)
		if err != nil {
			log.Printf("[%s] bound.GetStations db: %v", addr, err)
		}
		log.Printf("[%s] → bound.GetStations corpID=%d", addr, corpID)
		return server.SendCallRsp(c, pkt, accountID, result)

	case "GetSharesByShareholder":
		// Arg 0 is the corpShares flag: 0 = held by the session character,
		// 1 = held by the session corp.
		shareholderID := charID
		if flag := boundArgInt(cs, 0); flag != 0 {
			shareholderID = boundCharCorpID(charID, sqldb)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		result, err := evedb.GetSharesByShareholder(ctx, sqldb, shareholderID)
		if err != nil {
			log.Printf("[%s] bound.GetSharesByShareholder db: %v", addr, err)
		}
		log.Printf("[%s] → bound.GetSharesByShareholder holder=%d", addr, shareholderID)
		return server.SendCallRsp(c, pkt, accountID, result)

	case "GetVotes":
		// Wire: (corpID, voteCaseID). Answers an IndexRowset keyed on characterID.
		caseID := boundArgInt(cs, 1)
		log.Printf("[%s] → bound.GetVotes case=%d", addr, caseID)
		return server.SendCallRsp(c, pkt, accountID,
			evedb.GetCorpVotesFor(context.Background(), sqldb, caseID))

	case "GetVoteCasesByCorporation":
		// Answers every case as an IndexRowset keyed on voteCaseID; the client
		// filters open and closed.
		voteCorpID := boundArgInt(cs, 0)
		if voteCorpID == 0 {
			voteCorpID = boundCharCorpID(charID, sqldb)
		}
		log.Printf("[%s] → bound.GetVoteCasesByCorporation corpID=%d", addr, voteCorpID)
		return server.SendCallRsp(c, pkt, accountID,
			evedb.GetCorpVoteCases(context.Background(), sqldb, voteCorpID))

	case "GetVoteCaseOptions":
		// Wire order is (corpID, voteCaseID), swapped from the local API; corpID
		// may be None.
		caseID := boundArgInt(cs, 1)
		log.Printf("[%s] → bound.GetVoteCaseOptions case=%d", addr, caseID)
		return server.SendCallRsp(c, pkt, accountID,
			evedb.GetCorpVoteCaseOptions(context.Background(), sqldb, caseID))

	case "GetSanctionedActionsByCorporation":
		// Answers a plain Rowset; the client re-indexes on voteCaseID and filters
		// inEffect.
		sancCorpID := boundArgInt(cs, 0)
		if sancCorpID == 0 {
			sancCorpID = boundCharCorpID(charID, sqldb)
		}
		actions, err := evedb.GetCorpSanctionedActionRows(context.Background(), sqldb,
			sancCorpID, int64(server.Win32TimeNow()))
		if err != nil {
			log.Printf("[%s] bound.GetSanctionedActionsByCorporation: %v", addr, err)
		}
		log.Printf("[%s] → bound.GetSanctionedActionsByCorporation corpID=%d (%d)", addr, sancCorpID, len(actions))
		return server.SendCallRsp(c, pkt, accountID, evedb.SanctionedActionsRep(actions))

	case "GetRentalDetailsCorp", "GetRentalDetailsPlayer":
		log.Printf("[%s] → bound.%s (empty)", addr, cs.Method)
		return server.SendCallRsp(c, pkt, accountID, evedb.GetCorpRentalDetails(context.Background(), sqldb))

	case "CanViewVotes":
		// Voters (shareholders, a CEO voting a corp's stake, the CEO) and the
		// corporation's Directors may view votes.
		viewCorpID := boundArgInt(cs, 0)
		ctxV, cancelV := context.WithTimeout(context.Background(), 5*time.Second)
		can := 0
		if evedb.CanCastVote(ctxV, sqldb, charID, viewCorpID) ||
			(boundCharCorpID(charID, sqldb) == viewCorpID && isDirectorOrCEO(ctxV, sqldb, viewCorpID, charID)) {
			can = 1
		}
		cancelV()
		log.Printf("[%s] → bound.CanViewVotes corpID=%d → %d", addr, viewCorpID, can)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyInt{Value: uint64(can)})

	case "MemberCanRunForCEO":
		// Only the client's Corporation Management skill gate applies.
		log.Printf("[%s] → bound.MemberCanRunForCEO (true)", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyInt{Value: 1})

	case "CanBeKickedOut":
		return boundCanBeKickedOut(cc)

	case "GetAlliance":
		return boundGetAlliance(cc)

	case "GetMyApplications":
		// Answers a util.Rowset of the caller's own applications.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		result, err := evedb.GetMyApplications(ctx, sqldb, charID)
		if err != nil {
			log.Printf("[%s] bound.GetMyApplications db: %v", addr, err)
			return server.SendCallException(c, pkt, accountID, nil)
		}
		log.Printf("[%s] → bound.GetMyApplications charID=%d", addr, charID)
		return server.SendCallRsp(c, pkt, accountID, result)

	case "InsertApplication":
		return boundInsertApplication(cc)

	case "GetApplications":
		// Directors list the applications to their own corporation; no corpID arg.
		var callerCorpID int64
		{
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if sqldb != nil {
				_ = sqldb.QueryRowContext(ctx,
					`SELECT corporationID FROM character_ WHERE characterID = ?`, charID,
				).Scan(&callerCorpID)
			}
			result, err := evedb.GetApplications(ctx, sqldb, callerCorpID)
			if err != nil {
				log.Printf("[%s] bound.GetApplications db: %v", addr, err)
				return server.SendCallException(c, pkt, accountID, nil)
			}
			log.Printf("[%s] → bound.GetApplications corpID=%d", addr, callerCorpID)
			return server.SendCallRsp(c, pkt, accountID, result)
		}

	case "DeleteApplication":
		return boundDeleteApplication(cc)

	case "UpdateApplication":
		return boundUpdateApplication(cc)

	case "MultiAdd":
		return boundMultiAdd(cc)

	case "Add":
		return boundAdd(cc)

	case "SetLabel":
		return boundSetLabel(cc)

	case "RunRefiningProcess":
		// POS Refining Array: a bare call on the array's bound inventory.
		return handleRunRefiningProcess(c, addr, cs, pkt, accountID, charID, sqldb, ds)

	case "GetCapacity":
		return boundGetCapacity(cc)

	case "RemoveItems":
		return boundRemoveItems(cc)

	case "RemoveCharges":
		return boundRemoveCharges(cc)

	case "Jettison":
		return boundJettison(cc)

	case "ReplaceCharges":
		return boundReplaceCharges(cc)

	case "ItemGetInfo":
		return boundItemGetInfo(cc)

	case "CheckSendLocationInfo":
		log.Printf("[%s] → bound.CheckSendLocationInfo (stub) charID=%d", addr, charID)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})

	// ── Keeper bound-object calls ──
	// With keeper advertised under mask "solarsystem" the client binds it, then
	// calls GetLevelEditor on the bound object.
	case "GetLevelEditor":
		return keeper.Handle(c, addr, cs, pkt, accountID)

	// Keeper bound-object call: activate a dungeon warp gate.
	case "ActivateAccelerationGate":
		return handleActivateGate(c, addr, cs, pkt, accountID, sqldb, ds, sessReg, npcMgr)

	// ── LevelEditor bound-object calls (bindID 4) ─────────────────────────
	case "EditDungeon":
		return handleEditDungeon(c, addr, cs, pkt, accountID, sqldb, ds, sessReg, npcMgr)
	case "PlayDungeon":
		return handleEditDungeon(c, addr, cs, pkt, accountID, sqldb, ds, sessReg, npcMgr)
	case "Reset":
		return handleDungeonReset(c, addr, pkt, accountID, ds, sessReg, npcMgr, sqldb)
	case "GotoRoom":
		return handleGotoRoom(c, addr, cs, pkt, accountID, sqldb, ds, sessReg, npcMgr)
	case "ObjectSelection":
		// Client sends the list of selected ball IDs when user clicks objects.
		ds.dunSelected = nil
		if len(cs.ArgTuple.Items) >= 1 {
			if lst, ok := cs.ArgTuple.Items[0].(marshal.PyList); ok {
				for _, item := range lst.Items {
					if v, ok := item.(marshal.PyInt); ok {
						ds.dunSelected = append(ds.dunSelected, uint32(v.Value))
					}
				}
			}
		}
		log.Printf("[%s] → bound.ObjectSelection → %d selected", addr, len(ds.dunSelected))
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})

	case "GetLockedItemLocations":
		// corpRegistry bound: flat list of stationIDs where the corp has locked
		// items; the client iterates it, so None crashes.
		lockCorpID := boundCharCorpID(charID, sqldb)
		ctxL, cancelL := context.WithTimeout(context.Background(), 5*time.Second)
		locIDs := evedb.GetLockedItemLocationIDs(ctxL, sqldb, lockCorpID)
		cancelL()
		items := make([]marshal.PyRep, 0, len(locIDs))
		for _, id := range locIDs {
			items = append(items, marshal.PyInt{Value: uint64(id)})
		}
		log.Printf("[%s] → bound.GetLockedItemLocations corpID=%d (%d)", addr, lockCorpID, len(items))
		return server.SendCallRsp(c, pkt, accountID, marshal.PyList{Items: items})

	case "GetWars":
		// warRegistry bound: a util.IndexRowset keyed by warID (war_cso_wars.py reads
		// .itervalues() and .header on OnWarChanged). ownerID is a call argument, not the bind owner.
		return handleGetWars(c, addr, cs, pkt, accountID, sqldb)

	case "GetCostOfStartingAWarAgainst":
		// Must be a number: FmtISK raises on None before either Declare War dialog renders.
		return handleGetWarCost(c, addr, pkt, accountID, sqldb)

	case "GetRelationships":
		return boundGetRelationships(cc)

	case "SetCloneTypeID":
		return boundSetCloneTypeID(cc)

	case "GetGangState":
		// Remote-park bound: michelle pulls the gang state lazily when session.gangid
		// is set. The client reads only gangState.targetTags (itemID to overview tag),
		// so the blob is a util.KeyVal holding that dict; None means no gang.
		if globalGangReg == nil || charID == 0 || globalGangReg.GetCharGang(charID) == 0 {
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		return server.SendCallRsp(c, pkt, accountID,
			buildGangStateRep(globalGangReg.TargetTagsFor(charID)))

	// Capital mobility: all three arrive on the remote-park bound object, not in
	// handleDestinyBoundCall.
	case "BeaconJump", "BridgeToMember", "JumpThrough":
		// No jump drive, bridge or portal trip under a cloaking device.
		if moduleCloakOn(ds) {
			log.Printf("[%s] bound.%s: BLOCKED — cloaked", addr, cs.Method)
			return clientMsgErr(c, pkt, accountID, "CantJumpWhileCloaked", nil)
		}
		switch cs.Method {
		case "BeaconJump":
			return handleBeaconJump(c, addr, cs, pkt, accountID, charID, ds, sqldb)
		case "BridgeToMember":
			return handleBridgeToMember(c, addr, cs, pkt, accountID, charID, ds, sqldb)
		}
		return handleJumpThrough(c, addr, cs, pkt, accountID, charID, ds, sqldb)

	case "GangTagTarget":
		return boundGangTagTarget(cc)

	case "DoStandingCheckForStationService":
		return boundDoStandingCheckForStationService(cc)

	case "Bind":
		// Some client paths call .Bind() through an ObjectConnection (bco_locations.py);
		// it arrives as a bound call named "Bind". Callers ignore the result.
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})

	case "GetPotentialHomeStations":
		return boundGetPotentialHomeStations(cc)

	case "SetHomeStation":
		return boundSetHomeStation(cc)

	case "DoAction":
		return boundDoAction(cc)

	case "GetInfoServiceDetails":
		// Agent bound: info window Agent Info section (infosvc.py).
		agentID := agentBinds.resolve(cs)
		log.Printf("[%s] → bound.GetInfoServiceDetails agentID=%d charID=%d", addr, agentID, charID)
		return server.SendCallRsp(c, pkt, accountID, buildAgentInfoServiceDetails(addr, sqldb, agentID, charID))

	case "GotoLocation", "WarpToLocation":
		// Agent bound: mission bookmark Approach / Warp to Location; menusvc.py sends
		// (locationType, locationNumber). Respond first (startPlayerWarp contract), then warp.
		agentID := agentBinds.resolve(cs)
		locType, locNum, warpRange := parseWarpToLocationArgs(cs.ArgTuple)
		if err := server.SendCallRsp(c, pkt, accountID, marshal.PyNone{}); err != nil {
			return err
		}
		runAgentWarpToLocation(c, addr, accountID, charID, agentID, locType, locNum, warpRange, sqldb, ds, sessReg, npcMgr)
		return nil

	case "GetCourierOptions":
		return boundGetCourierOptions(cc)

	case "CreateCourierMission":
		return boundCreateCourierMission(cc)

	case "GetMissionJournalInfo":
		// Agent bound: GetMissionJournalInfo() answers the HTML of the mission detail popup.
		agentID := agentBinds.resolve(cs)
		log.Printf("[%s] → bound.GetMissionJournalInfo agentID=%d charID=%d", addr, agentID, charID)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.SendCallRsp(c, pkt, accountID, missionJournalInfoRep(ctx, sqldb, charID, agentID))

	case "GetMyJournalDetails":
		// Agent bound: journal.py's per-agent refresh after OnAgentMissionChange
		// expects the (missions, offers, research) 3-tuple filtered to this agent.
		agentID := agentBinds.resolve(cs)
		log.Printf("[%s] → bound.GetMyJournalDetails agentID=%d charID=%d", addr, agentID, charID)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.SendCallRsp(c, pkt, accountID, buildJournalDetails(ctx, sqldb, charID, agentID))

	case "GetOfferJournalInfo":
		// Agent bound: GetOfferJournalInfo() answers the journal Offers tab detail:
		// LP-store offers first, else an offered mission's HTML.
		agentID := agentBinds.resolve(cs)
		log.Printf("[%s] → bound.GetOfferJournalInfo agentID=%d charID=%d", addr, agentID, charID)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.SendCallRsp(c, pkt, accountID, offerJournalInfoRep(ctx, sqldb, charID, agentID))

	// ── Insurance bound calls ──────────────────────────────────────────────
	case "GetContracts":
		return insurance.Handle(c, addr, cs, pkt, accountID, charID, derefStation(stationID), sqldb)
	case "GetContractForShip":
		return insurance.Handle(c, addr, cs, pkt, accountID, charID, derefStation(stationID), sqldb)
	case "InsureShip":
		return insurance.Handle(c, addr, cs, pkt, accountID, charID, derefStation(stationID), sqldb)
	case "UnInsureShip":
		return insurance.Handle(c, addr, cs, pkt, accountID, charID, derefStation(stationID), sqldb)
	case "GetInsuredItems":
		return insurance.Handle(c, addr, cs, pkt, accountID, charID, derefStation(stationID), sqldb)
	case "GetInsurancePrice":
		return insurance.Handle(c, addr, cs, pkt, accountID, charID, derefStation(stationID), sqldb)

	// ── Reprocessing bound calls ───────────────────────────────────────────
	case "GetReprocessingInfo":
		return reprocessing.Handle(c, addr, cs, pkt, accountID, charID, derefStation(stationID), sqldb)
	case "GetQuote":
		return reprocessing.Handle(c, addr, cs, pkt, accountID, charID, derefStation(stationID), sqldb)
	case "Reprocess":
		return reprocessing.Handle(c, addr, cs, pkt, accountID, charID, derefStation(stationID), sqldb)
	case "GetOptionsForItemTypes":
		return reprocessing.Handle(c, addr, cs, pkt, accountID, charID, derefStation(stationID), sqldb)

	case "GetSkillsAndProgress", "GetSkillQueue", "CharAddImplant", "RemoveImplantFromCharacter":
		return handleSkillMgr(c, addr, cs, pkt, accountID, charID, derefStation(stationID), sqldb)

	// ── Inventory odds and ends (inventory_misc.go, trash.go) ─────────────
	case "TrashItems":
		return handleTrashItems(c, addr, cs, pkt, accountID, charID, derefStation(stationID), sqldb)
	case "TakeOutTrash":
		// The GM menu's Take out trash on a hangar item (menusvc GetGMMenu). The
		// client only hides it, so staff is checked here; then the hangar trash rules apply.
		tctx, tcancel := context.WithTimeout(context.Background(), 3*time.Second)
		staff := sqldb != nil && evedb.IsStaffRole(evedb.AccountRoleCached(tctx, sqldb, accountID))
		tcancel()
		if !staff {
			return corpUserErr(c, pkt, accountID, "Only game masters can do that.")
		}
		return handleTrashItems(c, addr, cs, pkt, accountID, charID, derefStation(stationID), sqldb)
	case "ListDroneBay":
		return handleListDroneBay(c, addr, pkt, accountID, charID, sqldb, ds)
	case "RemoveChargeToCargo", "RemoveChargeToHangar":
		return handleRemoveChargeTo(c, addr, cs, pkt, accountID, charID)

	default:
		log.Printf("[%s] CallReq: bound-object call method=%q — returning None", addr, cs.Method)
		// A silent None keeps the answer well-formed; rpccov records it for /coverage.
		rpccov.Record(addr, rpccov.BoundService, cs.Method, rpccov.Fallthrough)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
}

func derefStation(stationID *int64) int64 {
	if stationID != nil {
		return *stationID
	}
	return 0
}

// clearDungeonBalls removes the pilot's dungeon balls from its ballpark without a
// call response and returns how many it removed. NPC entities are deregistered
// from npcMgr and the solar system.
func clearDungeonBalls(c *evenet.Conn, addr string, accountID int64, ds *destinyConn, npcMgr *npc.NPCManager, sqldb *sql.DB) int {
	return clearDungeonBallsExcept(c, addr, accountID, ds, npcMgr, sqldb, nil)
}

// clearDungeonBallsExcept is clearDungeonBalls leaving the entity IDs in keep
// untouched: not torn down and not removed from any ballpark. A room reloading
// over itself keeps its live NPCs this way; the caller re-registers them in
// ds.dunObjects.
func clearDungeonBallsExcept(c *evenet.Conn, addr string, accountID int64, ds *destinyConn, npcMgr *npc.NPCManager, sqldb *sql.DB, keep map[uint32]int64) int {
	objs := ds.dunObjSnapshot()
	if len(objs) == 0 || ds.dm == nil {
		return 0
	}
	ids := make([]uint32, 0, len(objs))
	for eid := range objs {
		if _, ok := keep[eid]; ok {
			continue
		}
		ids = append(ids, eid)
	}
	// An open site's NPCs and scenery belong to the site (opensites.go). They stay in
	// space and on other visitors' grids; only this client lets go of them.
	mateIDs := openSiteLocalBalls(ds.dunOpenSiteID, ids)
	var shared map[uint32]bool
	if len(mateIDs) < len(ids) {
		shared = make(map[uint32]bool, len(ids)-len(mateIDs))
		local := make(map[uint32]bool, len(mateIDs))
		for _, id := range mateIDs {
			local[id] = true
		}
		for _, id := range ids {
			if !local[id] {
				shared[id] = true
			}
		}
	}

	// Deregister NPCs before RemoveBalls so their brains stop and the bubble slot is
	// freed.
	if npcMgr != nil && ds.solarSystem != nil {
		for _, eid := range ids {
			if shared[eid] {
				continue
			}
			if npcMgr.IsNPC(eid) {
				npcMgr.Remove(eid)
				ds.solarSystem.RemoveEntity(eid)
				if sqldb != nil {
					go func(entityID uint32) {
						defer safego.Recover("clearDungeonBallsExcept")
						ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
						defer cancel()
						evedb.DeleteDungeonNPC(ctx, sqldb, entityID)
					}(eid)
				}
			}
		}
	}

	// Send RemoveBalls only for balls the client still holds; the client logs an error
	// for each ball removed twice.
	sendIDs := ids
	if ds.entity != nil {
		sendIDs = ds.entity.ClaimKnownBalls(ids...)
	}
	if len(sendIDs) > 0 {
		if _, err := ds.sendFloorAdvance(func(stamp uint32) error {
			return destiny.SendRemoveBalls(c, globalNodeID, uint64(accountID), stamp, sendIDs)
		}); err != nil {
			log.Printf("[%s] clearDungeonBalls: SendRemoveBalls: %v", addr, err)
		}
	}
	// Grid-mates received these balls too, so remove them there. Mates track their
	// own knownBalls, so pass the full set.
	broadcastDunRemoveBalls(ds, mateIDs)
	ds.dunObjMu.Lock()
	ds.dunObjects = make(map[uint32]int64, len(keep))
	for eid, oid := range keep {
		ds.dunObjects[eid] = oid
	}
	ds.dunObjMu.Unlock()
	ds.dunGateDir = nil
	// The pocket's phenomena leave with its balls; spawnDungeonRoom re-reads the flag.
	ds.dunNoWarp = false
	return len(ids)
}

// dunReanchorTolerance is how far a room's anchor may move between two loads of
// the same room and still count as the same placement. Warp landings are
// deterministic, so a re-entry reproduces the anchor; a larger move resets the room.
const dunReanchorTolerance = 100.0 // metres

// spawnDungeonRoom loads a dungeon room's objects and spawns them in the client's
// ballpark. The previous room's balls are cleared first; live NPCs survive a
// reload of the same room at the same anchor. missionID > 0 tags spawned NPCs for
// kill tracking.
func spawnDungeonRoom(c *evenet.Conn, addr string, accountID int64, sqldb *sql.DB, ds *destinyConn, roomID int64, sessReg *esp.SessionRegistry, npcMgr *npc.NPCManager, missionID ...int64) error {
	// Captured first: the reload-over-self check compares against the room loaded now.
	prevRoomID, prevOffset := ds.dunRoomID, ds.dunOffset

	// An open site's room arrives through the pending slot, consumed here so every
	// other room load leaves the connection outside any site (opensites.go).
	openSiteID := ds.dunPendingOpenSite
	ds.dunPendingOpenSite = 0
	osite := openSiteByID(openSiteID)
	if osite == nil || osite.isGone() {
		osite, openSiteID = nil, 0
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	objs, err := evedb.GetRoomObjects(ctx, sqldb, roomID)
	if err != nil {
		log.Printf("[%s] spawnDungeonRoom: GetRoomObjects roomID=%d: %v", addr, roomID, err)
		return nil
	}

	if ds.dunObjects == nil {
		ds.dunObjMu.Lock()
		ds.dunObjects = make(map[uint32]int64)
		ds.dunObjMu.Unlock()
	}

	// Anchor on the room's beacon; a mission room anchors on its instance. Computed
	// before the clear because the preserve decision uses it.
	// See docs/main-s7-notes.md#room-anchoring.
	roomMissionID := int64(0)
	if len(missionID) > 0 && missionID[0] > 0 {
		roomMissionID = missionID[0]
	}
	var offset destiny.Vec3
	anchoredOnInstance := false
	if roomMissionID > 0 {
		inst, ierr := evedb.GetDungeonInstanceByMission(ctx, sqldb, roomMissionID)
		switch {
		case ierr != nil:
			log.Printf("[%s] spawnDungeonRoom: GetDungeonInstanceByMission %d: %v", addr, roomMissionID, ierr)
		case inst != nil && inst.RoomID == roomID:
			offset = destiny.Vec3{X: inst.AnchorX, Y: inst.AnchorY, Z: inst.AnchorZ}
			anchoredOnInstance = true
		}
	}
	if !anchoredOnInstance {
		if ds.dunPendingAnchor != nil {
			// Warp in flight: use the pre-lane landing point (dunPendingAnchor), not the
			// ship's mid-warp position.
			offset = *ds.dunPendingAnchor
		} else if ds.dm != nil {
			offset = ds.dm.Pos
		}
	}
	var beaconLocal destiny.Vec3
	var hasBeacon bool
	if bx, by, bz, ok, berr := evedb.GetRoomBeacon(ctx, sqldb, roomID); berr == nil && ok {
		beaconLocal = destiny.Vec3{X: bx, Y: by, Z: bz}
		hasBeacon = true
		if anchoredOnInstance {
			// dunSpawnShift is a landing-frame offset for gate arrivals. The instance anchor
			// is absolute and spawnMissionDungeonsForSystem applies no shift, so none here.
			offset = offset.Sub(beaconLocal)
		} else {
			offset = offset.Add(ds.dunSpawnShift).Sub(beaconLocal)
		}
	}
	ds.dunSpawnShift = destiny.Vec3{}
	// An open site has one frame for everyone (openSite.roomFrame).
	if osite != nil && !anchoredOnInstance {
		offset = osite.roomFrame(roomID, offset, beaconLocal, hasBeacon)
	}

	// Re-entering the loaded room at the same anchor (a warp arrival, dunPendingMode)
	// keeps its live NPCs with their damage, aggro and mission tag. The ESP refresh and
	// GotoRoom always rebuild; a different room or anchor resets.
	keepNPCs := map[uint32]int64{}
	if ds.dunPendingMode && prevRoomID == roomID && npcMgr != nil && ds.solarSystem != nil &&
		offset.Sub(prevOffset).Len() <= dunReanchorTolerance {
		for eid, oid := range ds.dunObjSnapshot() {
			// Dead NPCs are gone from npcMgr and the system; their stale mapping is reset below.
			if npcMgr.IsNPC(eid) && ds.solarSystem.GetEntity(eid) != nil {
				keepNPCs[eid] = oid
			}
		}
	}

	// Clear the previous room, after the preserve decision above.
	if n := clearDungeonBallsExcept(c, addr, accountID, ds, npcMgr, sqldb, keepNPCs); n > 0 {
		log.Printf("[%s] → DungeonReset removed %d balls before room load", addr, n)
	}

	ds.dunOffset = offset
	ds.dunRoomID = roomID
	ds.dunSelected = nil
	// Phenomena for the room entered, read after the clear zeroes the previous room's flag.
	if phen, perr := evedb.GetRoomNaturalPhenomena(ctx, sqldb, roomID); perr != nil {
		log.Printf("[%s] spawnDungeonRoom: natural phenomena flag for room %d: %v", addr, roomID, perr)
		ds.dunNoWarp = true
	} else {
		ds.dunNoWarp = phen
	}

	// The warp depart sweep pulled preserved NPCs from the client's ballpark and the
	// spawn below skips them. Queue them on the pre-render batch with their current damage.
	if len(keepNPCs) > 0 && ds.solarSystem != nil {
		if ds.dunPendingDmg == nil {
			ds.dunPendingDmg = make(map[uint32]destiny.DamageState)
		}
		for eid := range keepNPCs {
			e := ds.solarSystem.GetEntity(eid)
			if e == nil {
				continue
			}
			bd := e.BallDesc()
			bd.CorpID = uint32(e.Slim.CorpID)
			bd.Name = e.BallName()
			// Strip the movement mode: an Orbit/Follow ball whose reference is not in this
			// batch aborts the whole AddBalls client-side, and the pre-render path does not
			// sanitize. Rigid balls keep their mode, since STOP would grow sectors their
			// SubType does not promise.
			if bd.Mode != destiny.ModeRigid {
				bd.Mode = destiny.ModeStop
			}
			bd.FollowID = 0
			bd.OrbitID = 0
			ds.dunPendingBalls = append(ds.dunPendingBalls, bd)
			ds.dunPendingSlims = append(ds.dunPendingSlims, e.Slim)
			if e.HasHP() {
				d := e.DamageStateFrac()
				d.Timestamp = server.Win32TimeNow()
				ds.dunPendingDmg[eid] = d
			} else {
				ds.dunPendingDmg[eid] = e.Damage
			}
		}
	}

	// Tell the ESP session which room this pilot views, for live refreshes.
	if sessReg != nil {
		sessReg.Update(addr, func(s *esp.Session) {
			s.DunRoomID = roomID
		})
	}

	// Gates point outward from the beacon (or the object centroid), and gate
	// activation moves the pilot along the same direction.
	roomAnchor := beaconLocal
	if !hasBeacon && len(objs) > 0 {
		for _, o := range objs {
			roomAnchor.X += o.X
			roomAnchor.Y += o.Y
			roomAnchor.Z += o.Z
		}
		roomAnchor = roomAnchor.Scale(1.0 / float64(len(objs)))
	}
	ds.dunRoomAnchor = roomAnchor

	// Resolve the dungeon's factionID to an NPC corporation for standing colours.
	var dunFactionID int64
	var dunCorpID int64
	_ = sqldb.QueryRowContext(ctx,
		`SELECT d.factionID FROM dunRooms r JOIN dunDungeons d ON d.dungeonVID = r.dungeonVID WHERE r.roomID = ?`,
		roomID).Scan(&dunFactionID)
	if dunFactionID > 0 {
		dunCorpID = evedb.GetFactionCorpID(ctx, sqldb, dunFactionID)
	}
	ds.dunFactionID = dunFactionID
	ds.dunCorpID = dunCorpID
	ds.dunMissionID = roomMissionID
	ds.dunOpenSiteID = openSiteID

	// Objects in a spawnWave group stay hidden until that wave is released. The site
	// record keeps destroyed objects gone and released waves spawned.
	site := dunSiteStateFor(ds, false)
	// Objects whose NPC survived the reload must not be spawned a second time.
	preserved := make(map[int64]bool, len(keepNPCs))
	for _, oid := range keepNPCs {
		preserved[oid] = true
	}
	initial := make([]evedb.ObjectRow, 0, len(objs))
	waveHeld, killedSkipped := 0, 0
	for _, o := range objs {
		if site.IsKilled(o.ObjectID) {
			killedSkipped++
			continue
		}
		if preserved[o.ObjectID] {
			continue
		}
		if o.SpawnWave.Valid && o.SpawnWave.Int64 > 0 && !site.WaveReleased(roomID, o.SpawnWave.Int64) {
			waveHeld++
			continue
		}
		// An open site's own beacon already stands on room 1's authored one.
		if osite != nil && hasBeacon && roomID == osite.firstRoomID && o.TypeID == evedb.DungeonBeaconTypeID &&
			o.X == beaconLocal.X && o.Y == beaconLocal.Y && o.Z == beaconLocal.Z {
			continue
		}
		initial = append(initial, o)
	}

	anchorSrc := "landing"
	if anchoredOnInstance {
		anchorSrc = "instance"
	} else if osite != nil {
		anchorSrc = fmt.Sprintf("open site %d", openSiteID)
	}
	log.Printf("[%s] → spawnDungeonRoom roomID=%d spawning %d objects (%d held for waves, %d already destroyed, %d live NPCs kept), anchor=%s origin=(%.0f, %.0f, %.0f)",
		addr, roomID, len(initial), waveHeld, killedSkipped, len(keepNPCs), anchorSrc, offset.X, offset.Y, offset.Z)

	// Pilots landing in an open site together build it once: the second waits and adopts.
	if osite != nil {
		osite.spawnMu.Lock()
	}
	spawnErr := spawnDungeonObjects(c, addr, accountID, sqldb, ds, npcMgr, initial, objs)
	if osite != nil {
		osite.spawnMu.Unlock()
	}

	// Arm the room's triggers and fire roomEnter. During a warp pre-render the warp
	// goroutine fires it with the landing batch. A room of auto-wave rats starts its chain here.
	loadRoomTriggers(ctx, sqldb, ds, roomID, objs)
	if ds.dunPendingMode {
		ds.dunPendingRoomEnter = true
	} else if !fireDungeonTriggers(c, addr, accountID, ds, "roomEnter", 0) {
		autoReleaseDungeonWave(ds, npcMgr, 0, addr)
	}
	return spawnErr
}

// spawnMissionDungeonsForSystem spawns NPCs for active mission dungeons on system
// load. They become system-level entities; rigid objects are added per connection
// when the owner warps to the bookmark.
func spawnMissionDungeonsForSystem(ss *destiny.SolarSystem, sqldb *sql.DB, npcMgr *npc.NPCManager, sysID int64, logPrefix string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	instances, err := evedb.GetDungeonInstancesForSystem(ctx, sqldb, sysID)
	if err != nil {
		log.Printf("[%s] spawnMissionDungeonsForSystem sysID=%d: %v", logPrefix, sysID, err)
		return
	}
	if len(instances) == 0 {
		return
	}

	for _, inst := range instances {
		// Verify mission is still accepted.
		m, merr := evedb.GetMissionByID(ctx, sqldb, inst.MissionID)
		if merr != nil || m == nil || m.State != "accepted" {
			// Stale instance: remove it.
			evedb.DeleteDungeonInstance(ctx, sqldb, inst.MissionID)
			continue
		}
		if m.ObjectiveDone {
			// Objective done, no need to spawn NPCs.
			continue
		}

		objs, oerr := evedb.GetRoomObjects(ctx, sqldb, inst.RoomID)
		if oerr != nil || len(objs) == 0 {
			log.Printf("[%s] spawnMissionDungeonsForSystem: mission %d room %d has no objects", logPrefix, inst.MissionID, inst.RoomID)
			continue
		}

		// Compute offset: anchor position minus beacon local position.
		offset := destiny.Vec3{X: inst.AnchorX, Y: inst.AnchorY, Z: inst.AnchorZ}
		if bx, by, bz, ok, berr := evedb.GetRoomBeacon(ctx, sqldb, inst.RoomID); berr == nil && ok {
			beaconLocal := destiny.Vec3{X: bx, Y: by, Z: bz}
			offset = offset.Sub(beaconLocal)
		}

		// Load site state to skip already-killed NPCs and handle released waves.
		site := dunSiteFor(inst.MissionID, true)

		// Resolve dungeon-level faction.
		var dunFactionID, dunCorpID int64
		_ = sqldb.QueryRowContext(ctx,
			`SELECT d.factionID FROM dunRooms r JOIN dunDungeons d ON d.dungeonVID = r.dungeonVID WHERE r.roomID = ?`,
			inst.RoomID).Scan(&dunFactionID)
		if dunFactionID > 0 {
			dunCorpID = evedb.GetFactionCorpID(ctx, sqldb, dunFactionID)
		}

		npcCount := 0
		for _, o := range objs {
			if site.IsKilled(o.ObjectID) {
				continue
			}
			// Wave-held objects stay hidden.
			if o.SpawnWave.Valid && o.SpawnWave.Int64 > 0 && !site.WaveReleased(inst.RoomID, o.SpawnWave.Int64) {
				continue
			}

			// Only spawn objects that become live entities: ships, entities,
			// and destructible structures. Pure scenery stays a per-connection
			// rigid ball added on warp-in.
			var groupID, categoryID int64
			if err := sqldb.QueryRowContext(ctx,
				`SELECT groupID, categoryID FROM invTypes JOIN invGroups USING(groupID) WHERE typeID = ?`,
				o.TypeID).Scan(&groupID, &categoryID); err != nil {
				continue
			}
			isStructure := npc.IsDestructibleStructure(groupID)
			if categoryID != 6 && categoryID != 11 && !isStructure {
				continue // rigid objects are spawned per-connection on warp-in
			}

			stats, statsErr := evedb.GetNPCStats(ctx, sqldb, o.TypeID)
			if statsErr != nil {
				continue
			}

			var npcHP *npc.NPCHPInit
			ehp, ehpErr := evedb.GetEntityHPByType(ctx, sqldb, o.TypeID)
			if ehpErr == nil {
				npcHP = &npc.NPCHPInit{
					ShieldHP: ehp.ShieldHP, ArmorHP: ehp.ArmorHP,
					StructureHP: ehp.StructureHP, ShieldTau: ehp.ShieldTau,
					ShieldRes: ehp.ShieldRes, ArmorRes: ehp.ArmorRes, HullRes: ehp.HullRes,
				}
				scaleDunHP(npcHP, o)
			}

			// A structure joins the system-level spawn so its damage and destruction persist
			// across warp-outs like a rat's. HP-less scenery stays a per-connection rigid ball.
			switch {
			case stats.MaxVelocity > 0:
			case isStructure && npcHP != nil && npcHP.StructureHP > 0:
			default:
				continue
			}

			weaponProfile, _ := evedb.GetNPCWeaponProfile(ctx, sqldb, o.TypeID)
			stance := npc.ParseStance(o.Stance.String)

			// An inert structure inherits nothing from the dungeon: no faction, standings or
			// faction-wide aggro when shot. Armed sentries keep the dungeon faction, since a
			// factionless sentry is hostile to everything (StandingsMap.IsHostile).
			objFactionID, objCorpID := dunFactionID, dunCorpID
			if npc.IsInertStructure(groupID) {
				// OwnerNone, not 0: SpawnNPC turns 0 ("no preference") into CONCORD.
				objFactionID, objCorpID = 0, npc.OwnerNone
			}
			if o.NPCFactionID.Valid && o.NPCFactionID.Int64 > 0 {
				objFactionID = o.NPCFactionID.Int64
				objCorpID = evedb.GetFactionCorpID(ctx, sqldb, o.NPCFactionID.Int64)
			}
			npcBounty := evedb.GetNPCBounty(ctx, sqldb, o.TypeID)

			spawnPos := destiny.Vec3{X: o.X + offset.X, Y: o.Y + offset.Y, Z: o.Z + offset.Z}
			spawnRadius := stats.Radius
			if isStructure && o.Radius.Valid && o.Radius.Float64 > 0 {
				spawnRadius = o.Radius.Float64
			}
			entity := npcMgr.SpawnNPC(ss, o.TypeID, dunBallName(o, stats.TypeName),
				spawnPos, stats.Mass, stats.Agility, stats.MaxVelocity, spawnRadius,
				stance, npcHP, weaponProfile, objCorpID, false, npcBounty, objFactionID, false)
			if entity == nil {
				continue
			}
			npc.SetGroupCategory(entity, stats.GroupID, stats.CategoryID)
			entity.SetInvulnerable(o.Invulnerable)
			npcMgr.SetMissionID(entity.ID, inst.MissionID)
			npcMgr.SetDunObjectID(entity.ID, o.ObjectID)
			applyDunSlimFields(entity, o)

			if o.AggressionRange.Valid {
				npcMgr.SetAggrRange(entity.ID, o.AggressionRange.Float64)
			}
			if o.LeashObjectID.Valid && o.LeashObjectID.Int64 > 0 {
				for _, lo := range objs {
					if lo.ObjectID != o.LeashObjectID.Int64 {
						continue
					}
					anchor := destiny.Vec3{X: lo.X + offset.X, Y: lo.Y + offset.Y, Z: lo.Z + offset.Z}
					orbitDist := 5000.0
					if o.OrbitDistance.Valid && o.OrbitDistance.Float64 > 0 {
						orbitDist = o.OrbitDistance.Float64
					}
					npcMgr.SetLeash(entity.ID, anchor, orbitDist)
					break
				}
			}

			// Persist to dungeon NPC table.
			go func(eid uint32, tid int64, tn string, pos destiny.Vec3,
				fid, cid int64, bounty float64, gid, catid, dobjID, drmID, midID int64) {
				defer safego.Recover("spawnMissionDungeonsForSystem")
				dbCtx, dbCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer dbCancel()
				evedb.InsertDungeonNPC(dbCtx, sqldb, eid, tid, tn, sysID,
					pos.X, pos.Y, pos.Z, fid, cid, bounty, gid, catid,
					dobjID, drmID, midID)
			}(entity.ID, o.TypeID, stats.TypeName, spawnPos,
				objFactionID, objCorpID, npcBounty, stats.GroupID, stats.CategoryID,
				o.ObjectID, o.RoomID, inst.MissionID)

			npcCount++
		}

		// Mark instance as spawned.
		evedb.MarkDungeonInstanceSpawned(ctx, sqldb, inst.MissionID)
		log.Printf("[%s] spawnMissionDungeonsForSystem: mission %d spawned %d NPCs in sysID=%d at (%.0f,%.0f,%.0f)",
			logPrefix, inst.MissionID, npcCount, sysID, inst.AnchorX, inst.AnchorY, inst.AnchorZ)
	}
}

// dunWaveReq asks the connection goroutine to spawn one wave group. RoomID is the
// room the trigger belonged to; requests for an unloaded room are dropped.
type dunWaveReq struct {
	RoomID int64
	Wave   int64
}

// playerDeathReq asks a victim's connection goroutine to run its own ship or pod
// death sequence. Attacker goroutines must not call the death handlers directly:
// they rewrite the victim's destinyConn, which only its own goroutine may touch.
type playerDeathReq struct {
	pod bool // true = capsule killed, false = ship destroyed
}

// queuePlayerDeath dispatches a death to the victim's connection goroutine.
// ApplyDamage reports the alive to dead transition once per entity, so at most a
// ship death and a pod death are in flight together.
func queuePlayerDeath(victimDS *destinyConn, pod bool) {
	if victimDS == nil || victimDS.playerDeath == nil {
		return
	}
	select {
	case victimDS.playerDeath <- playerDeathReq{pod: pod}:
	default:
		// Reachable only if the victim's goroutine is gone; teardown cleans up.
		log.Printf("queuePlayerDeath: channel full for charID=%d, death dropped", victimDS.charID)
	}
}

// spawnDungeonWave spawns the loaded room's dunObjects whose spawnWave group
// matches the request. It runs on the connection goroutine via ds.dunWaveSpawn,
// so touching ds.dunObjects is safe. Already-spawned objects are skipped, so a
// re-fired trigger is a no-op.
func spawnDungeonWave(c *evenet.Conn, addr string, accountID int64, sqldb *sql.DB, ds *destinyConn, npcMgr *npc.NPCManager, req dunWaveReq) {
	wave := req.Wave
	if ds.dunRoomID == 0 || wave <= 0 {
		return
	}
	if req.RoomID != ds.dunRoomID {
		log.Printf("[%s] spawnDungeonWave: dropping stale wave %d for room %d (loaded room %d)",
			addr, wave, req.RoomID, ds.dunRoomID)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	objs, err := evedb.GetRoomObjects(ctx, sqldb, ds.dunRoomID)
	if err != nil {
		log.Printf("[%s] spawnDungeonWave: GetRoomObjects roomID=%d: %v", addr, ds.dunRoomID, err)
		return
	}
	// Record the release so a room reload spawns this wave and destroyed members stay gone.
	site := dunSiteStateFor(ds, true)
	site.MarkWaveReleased(ds.dunRoomID, wave)
	noteDungeonWaveOut(ds, ds.dunRoomID, wave)
	// A released wave's rats rejoin an open site's clear rule and reopen a site counting down.
	if ds.dunOpenSiteID > 0 {
		defer openSiteWaveLanded(ds.dunOpenSiteID, req.RoomID, wave)
	}
	// A wave with no rat (scenery only, or every member dead) leaves the room quiet:
	// carry the auto chain on. Defers run last-first, so this runs after the spawn below.
	defer autoReleaseDungeonWave(ds, npcMgr, 0, addr)

	cur := ds.dunObjSnapshot()
	spawned := make(map[int64]bool, len(cur))
	for _, oid := range cur {
		spawned[oid] = true
	}
	var waveObjs []evedb.ObjectRow
	for _, o := range objs {
		if o.SpawnWave.Valid && o.SpawnWave.Int64 == wave && !spawned[o.ObjectID] && !site.IsKilled(o.ObjectID) {
			waveObjs = append(waveObjs, o)
		}
	}
	if len(waveObjs) == 0 {
		log.Printf("[%s] spawnDungeonWave: wave %d of room %d has no unspawned objects", addr, wave, ds.dunRoomID)
		return
	}
	log.Printf("[%s] → spawnDungeonWave roomID=%d wave=%d spawning %d objects", addr, ds.dunRoomID, wave, len(waveObjs))
	if s := openSiteByID(ds.dunOpenSiteID); s != nil {
		s.spawnMu.Lock()
		defer s.spawnMu.Unlock()
	}
	if err := spawnDungeonObjects(c, addr, accountID, sqldb, ds, npcMgr, waveObjs, objs); err != nil {
		log.Printf("[%s] spawnDungeonWave: %v", addr, err)
	}
}

// dunBallName returns the name to broadcast for a dungeon ball: the authored
// dunObjects.ballName, else the item type's name.
// See docs/main-s7-notes.md#ball-names.
func dunBallName(o evedb.ObjectRow, typeName string) string {
	if o.BallName.Valid {
		if name := strings.TrimSpace(o.BallName.String); name != "" {
			return name
		}
	}
	return typeName
}

// scaleDunHP applies the ball's authored dunObjects.hpScale to the shield, armour
// and structure a spawn starts with. Resists, recharge and the weapon profile
// stay the type's own. A nil npcHP stays nil.
// See docs/main-s7-notes.md#hp-scale.
func scaleDunHP(npcHP *npc.NPCHPInit, o evedb.ObjectRow) {
	if npcHP == nil {
		return
	}
	m := o.HPMultiplier()
	if m == 1 {
		return
	}
	npcHP.ShieldHP *= m
	npcHP.ArmorHP *= m
	npcHP.StructureHP *= m
}

// applyDunSlimFields stamps a live entity's SlimItem with the dungeon-authoring
// attributes the client reads off the slim item, including dunRotation (applied by
// SetStaticRotation in spaceObject.py). Stamping the entity's own Slim means every
// later send carries it: bubble entry for a second pilot and the adopt path.
func applyDunSlimFields(e *destiny.Entity, o evedb.ObjectRow) {
	if e == nil {
		return
	}
	e.Slim.DunObjectID = o.ObjectID
	e.Slim.DunRoomID = o.RoomID
	e.Slim.DunX, e.Slim.DunY, e.Slim.DunZ = o.X, o.Y, o.Z
	if o.Yaw.Valid || o.Pitch.Valid || o.Roll.Valid {
		yaw, pitch, roll := o.Yaw.Float64, o.Pitch.Float64, o.Roll.Float64
		e.Slim.DunYaw, e.Slim.DunPitch, e.Slim.DunRoll = &yaw, &pitch, &roll
	}
	if o.Radius.Valid {
		r := o.Radius.Float64
		e.Slim.DunRadius = &r
	}
	if o.AggressionRange.Valid {
		a := o.AggressionRange.Float64
		e.Slim.DunAggrRange = &a
	}
	if o.Description.Valid {
		e.Slim.DunDescription = o.Description.String
	}
	// Collision: an immobile structure goes out as a rigid ball, which collides only
	// when massive. Every structure spawn path (core and the rolebots headless room)
	// comes through here after SetGroupCategory.
	if e.Manager != nil {
		e.Manager.SetSolid(o.IsSolid(e.Slim.GroupID))
	}
}

// spawnDungeonObjects spawns the given dunObjects rows into the client's ballpark
// as NPCs or rigid balls, using the room context spawnDungeonRoom stored on ds
// (dunOffset, dunRoomAnchor, dunFactionID/dunCorpID, dunMissionID). allObjs is
// the whole room, since a leash anchor may be outside the batch.
func spawnDungeonObjects(c *evenet.Conn, addr string, accountID int64, sqldb *sql.DB, ds *destinyConn, npcMgr *npc.NPCManager, objs, allObjs []evedb.ObjectRow) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	offset := ds.dunOffset
	roomAnchor := ds.dunRoomAnchor
	dunFactionID := ds.dunFactionID
	dunCorpID := ds.dunCorpID

	var balls []destiny.BallDesc
	var slims []destiny.SlimItem
	dmg := make(map[uint32]destiny.DamageState)

	for _, o := range objs {
		radius := 500.0
		if o.Radius.Valid {
			radius = o.Radius.Float64
		}

		// Query group/category for this typeID.
		var groupID, categoryID int64
		row := sqldb.QueryRowContext(ctx,
			`SELECT groupID, categoryID FROM invTypes
			 JOIN invGroups USING(groupID)
			 WHERE typeID = ?`, o.TypeID)
		if err := row.Scan(&groupID, &categoryID); err != nil {
			log.Printf("[%s]   dungeon obj %d typeID=%d: invTypes lookup failed: %v", addr, o.ObjectID, o.TypeID, err)
			groupID = 0
			categoryID = 0
		}

		spawnPos := destiny.Vec3{X: o.X + offset.X, Y: o.Y + offset.Y, Z: o.Z + offset.Z}

		// Ships (categoryID 6), Entities (11) and destructible structures (Large Collidable
		// Objects/Structures, spawn containers, sentry guns) spawn as live NPCs. Structures
		// have HP but no maxVelocity, so as rigid balls they could not be locked or shot.
		isStructure := npc.IsDestructibleStructure(groupID)
		isNPCCategory := (categoryID == 6 || categoryID == 11 || isStructure) && ds.solarSystem != nil
		if isNPCCategory {
			stats, statsErr := evedb.GetNPCStats(ctx, sqldb, o.TypeID)
			if statsErr != nil {
				log.Printf("[%s]   dungeon NPC typeID=%d: GetNPCStats: %v", addr, o.TypeID, statsErr)
				goto rigidBall
			}

			var npcHP *npc.NPCHPInit
			ehp, ehpErr := evedb.GetEntityHPByType(ctx, sqldb, o.TypeID)
			if ehpErr == nil {
				npcHP = &npc.NPCHPInit{
					ShieldHP: ehp.ShieldHP, ArmorHP: ehp.ArmorHP,
					StructureHP: ehp.StructureHP, ShieldTau: ehp.ShieldTau,
					ShieldRes: ehp.ShieldRes, ArmorRes: ehp.ArmorRes, HullRes: ehp.HullRes,
				}
				scaleDunHP(npcHP, o)
			}

			switch {
			case stats.MaxVelocity > 0:
				// Ordinary mobile rat.
			case isStructure && npcHP != nil && npcHP.StructureHP > 0:
				// Immobile structure with real HP: a legitimate target.
			default:
				// maxVelocity 0 and nothing to shoot at: scenery.
				log.Printf("[%s]   dungeon obj %d typeID=%d: maxVelocity=0, spawning as rigid (not NPC)", addr, o.ObjectID, o.TypeID)
				goto rigidBall
			}

			weaponProfile, wpErr := evedb.GetNPCWeaponProfile(ctx, sqldb, o.TypeID)
			if wpErr != nil {
				log.Printf("[%s]   dungeon NPC typeID=%d: weapon profile error: %v", addr, o.TypeID, wpErr)
			} else if weaponProfile == nil {
				log.Printf("[%s]   dungeon NPC typeID=%d has no weapon profile (unarmed)", addr, o.TypeID)
			} else {
				log.Printf("[%s]   dungeon NPC typeID=%d weapon: missile=%d turret=%d dmg=%.1f+%.1f+%.1f+%.1f optimal=%.0f attackRange=%.0f cycle=%dms",
					addr, o.TypeID, weaponProfile.MissileTypeID, weaponProfile.TurretTypeID,
					weaponProfile.Em, weaponProfile.Explosive, weaponProfile.Kinetic, weaponProfile.Thermal,
					weaponProfile.OptimalRange, weaponProfile.AttackRange, weaponProfile.CycleTimeMs)
			}

			// The role comes from the hull (an armed structure is a turret, an
			// unarmed one inert); the author only picks the stance.
			stance := npc.ParseStance(o.Stance.String)

			// Per-object faction override. Otherwise NPCs and armed sentries take the dungeon
			// faction and inert structures none (see spawnMissionDungeonsForSystem).
			objFactionID, objCorpID := dunFactionID, dunCorpID
			if npc.IsInertStructure(groupID) {
				objFactionID, objCorpID = 0, npc.OwnerNone
			}
			if o.NPCFactionID.Valid && o.NPCFactionID.Int64 > 0 {
				objFactionID = o.NPCFactionID.Int64
				objCorpID = evedb.GetFactionCorpID(ctx, sqldb, o.NPCFactionID.Int64)
			}
			npcBounty := evedb.GetNPCBounty(ctx, sqldb, o.TypeID)

			// Adopt an NPC already spawned at system level (spawnMissionDungeonsForSystem)
			// instead of duplicating it. An open site's NPCs are adopted by site, never by mission.
			npcMissionID := ds.dunMissionID
			existingEID := npcMgr.FindEntityByDunObject(npcMissionID, o.ObjectID)
			if ds.dunOpenSiteID > 0 {
				existingEID = npcMgr.FindEntityByOpenSiteObject(ds.dunOpenSiteID, o.ObjectID)
			}
			if existingEID != 0 {
				existingEntity := ds.solarSystem.GetEntity(existingEID)
				if existingEntity != nil {
					// Adopt: map the entity into this connection's dunObjects and the client batch.
					ds.dunObjMu.Lock()
					ds.dunObjects[existingEID] = o.ObjectID
					ds.dunObjMu.Unlock()

					bd := existingEntity.BallDesc()
					bd.CorpID = uint32(existingEntity.Slim.CorpID)
					bd.Name = existingEntity.BallName()
					if bd.Mode != destiny.ModeRigid {
						bd.Mode = destiny.ModeStop
					}
					bd.FollowID = 0
					bd.OrbitID = 0
					balls = append(balls, bd)
					npcSlim := existingEntity.Slim
					npcSlim.DunObjectID = o.ObjectID
					npcSlim.DunRoomID = o.RoomID
					npcSlim.DunX = o.X
					npcSlim.DunY = o.Y
					npcSlim.DunZ = o.Z
					slims = append(slims, npcSlim)
					dmg[existingEID] = existingEntity.Damage

					if ds.entity != nil {
						ds.solarSystem.EnsureSameBubble(existingEID, ds.entity.ID)
					}
					// Re-apply the authored invulnerability; the editor may have changed it since the spawn.
					existingEntity.SetInvulnerable(o.Invulnerable)
					if existingEntity.Manager != nil {
						existingEntity.Manager.SetSolid(o.IsSolid(existingEntity.Slim.GroupID))
					}
					log.Printf("[%s]   dungeon NPC entityID=%d typeID=%d adopted (system-level spawn)", addr, existingEID, o.TypeID)
					continue
				}
			}

			// Structures honour the authored radius, so resizing a prop in the editor still
			// works. Rats take it from the type.
			spawnRadius := stats.Radius
			if isStructure && o.Radius.Valid && o.Radius.Float64 > 0 {
				spawnRadius = o.Radius.Float64
			}
			entity := npcMgr.SpawnNPC(ds.solarSystem, o.TypeID, dunBallName(o, stats.TypeName),
				spawnPos, stats.Mass, stats.Agility, stats.MaxVelocity, spawnRadius,
				stance, npcHP, weaponProfile, objCorpID, false, npcBounty, objFactionID, false)
			if entity == nil {
				continue
			}
			npc.SetGroupCategory(entity, stats.GroupID, stats.CategoryID)
			// Authored scenery: locks and takes fire, never loses HP.
			entity.SetInvulnerable(o.Invulnerable)
			// Tag the NPC with its mission for kill tracking. Structures carry it too: the
			// killAll census skips them by group, but a mission may name one as its targetTypeID.
			if npcMissionID > 0 {
				npcMgr.SetMissionID(entity.ID, npcMissionID)
			}
			if ds.dunOpenSiteID > 0 {
				npcMgr.SetOpenSiteID(entity.ID, ds.dunOpenSiteID)
			}
			npcMgr.SetDunObjectID(entity.ID, o.ObjectID)
			applyDunSlimFields(entity, o)

			// Persist dungeon NPC to DB.
			go func(eid uint32, tid int64, tn string, sysID int64, pos destiny.Vec3,
				fid, cid int64, bounty float64, gid, catid, dobjID, drmID, midID int64) {
				defer safego.Recover("spawnDungeonObjects")
				dbCtx, dbCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer dbCancel()
				if err := evedb.InsertDungeonNPC(dbCtx, sqldb, eid, tid, tn, sysID,
					pos.X, pos.Y, pos.Z, fid, cid, bounty, gid, catid,
					dobjID, drmID, midID); err != nil {
					log.Printf("[%s] InsertDungeonNPC entityID=%d: %v", addr, eid, err)
				}
			}(entity.ID, o.TypeID, stats.TypeName, int64(ds.solarSystem.ID), spawnPos,
				objFactionID, objCorpID, npcBounty, stats.GroupID, stats.CategoryID,
				o.ObjectID, o.RoomID, npcMissionID)

			// Apply per-object aggression range override from dungeon.
			if o.AggressionRange.Valid {
				npcMgr.SetAggrRange(entity.ID, o.AggressionRange.Float64)
			}

			// Leash: idle by circling another authored object in this room
			// instead of holding still at the spawn point.
			if o.LeashObjectID.Valid && o.LeashObjectID.Int64 > 0 {
				for _, lo := range allObjs {
					if lo.ObjectID != o.LeashObjectID.Int64 {
						continue
					}
					anchor := destiny.Vec3{X: lo.X + offset.X, Y: lo.Y + offset.Y, Z: lo.Z + offset.Z}
					orbitDist := 5000.0
					if o.OrbitDistance.Valid && o.OrbitDistance.Float64 > 0 {
						orbitDist = o.OrbitDistance.Float64
					}
					npcMgr.SetLeash(entity.ID, anchor, orbitDist)
					break
				}
			}

			// An NPC with a large offset can land in another bubble than the player. Force the
			// same bubble so its brain sees the player.
			if ds.entity != nil {
				ds.solarSystem.EnsureSameBubble(entity.ID, ds.entity.ID)
			}

			// Track in dunObjects for clearDungeonBalls cleanup.
			ds.dunObjMu.Lock()
			ds.dunObjects[entity.ID] = o.ObjectID
			ds.dunObjMu.Unlock()

			// Add NPC to the batch AddBallsFull packet.
			bd := entity.BallDesc()
			bd.CorpID = uint32(entity.Slim.CorpID)
			bd.Name = entity.BallName()
			balls = append(balls, bd)

			npcSlim := entity.Slim
			npcSlim.DunObjectID = o.ObjectID
			npcSlim.DunRoomID = o.RoomID
			npcSlim.DunX = o.X
			npcSlim.DunY = o.Y
			npcSlim.DunZ = o.Z
			slims = append(slims, npcSlim)
			dmg[entity.ID] = entity.Damage

			log.Printf("[%s]   dungeon NPC entityID=%d typeID=%d %s stance=%s",
				addr, entity.ID, o.TypeID, stats.TypeName, stance)
			continue
		}

	rigidBall:
		// Non-ship objects spawn as rigid balls. An open site's scenery keeps one ID per
		// object for every visitor, so two pilots hold one gate between them.
		var entityID uint32
		if s := openSiteByID(ds.dunOpenSiteID); s != nil {
			entityID = s.rigidID(o.ObjectID)
		} else {
			entityID = nextDunRigidID()
		}
		ds.dunObjMu.Lock()
		ds.dunObjects[entityID] = o.ObjectID
		ds.dunObjMu.Unlock()

		// Rigid dungeon objects: SubType 2, or 6 (massive) when solid so the client collides
		// ships against the ball's radius. The client's destiny parser rejects SubType 0.
		bd := destiny.BallDesc{
			EntityID: entityID,
			Mode:     destiny.ModeRigid,
			SubType:  destiny.RigidSubType(o.IsSolid(groupID)),
			Pos:      spawnPos,
			Radius:   radius,
			Name:     dunBallName(o, ""),
		}
		balls = append(balls, bd)

		slim := destiny.SlimItem{
			BallID:      int64(entityID),
			ItemID:      int64(entityID),
			TypeID:      o.TypeID,
			GroupID:     groupID,
			CategoryID:  categoryID,
			OwnerID:     1, // system owner
			DunObjectID: o.ObjectID,
			DunRoomID:   o.RoomID,
			DunX:        o.X,
			DunY:        o.Y,
			DunZ:        o.Z,
		}
		if o.Yaw.Valid {
			yaw, pitch, roll := o.Yaw.Float64, o.Pitch.Float64, o.Roll.Float64
			slim.DunYaw = &yaw
			slim.DunPitch = &pitch
			slim.DunRoll = &roll
		}
		if o.Radius.Valid {
			r := o.Radius.Float64
			slim.DunRadius = &r
		}
		if o.AggressionRange.Valid {
			a := o.AggressionRange.Float64
			slim.DunAggrRange = &a
		}
		if o.Description.Valid {
			// The client shows dunDescription as the Show Info description and bookmark hint
			// (menusvc.py, infosvc.py).
			slim.DunDescription = o.Description.String
		}
		if groupID == destiny.GroupWarpGate {
			// Aim the gate outward from the room anchor: SetStaticDirection rotates the model
			// along dunDirection, and runActivateGate reuses the cached direction for travel.
			dir := destiny.Vec3{X: o.X - roomAnchor.X, Y: o.Y - roomAnchor.Y, Z: o.Z - roomAnchor.Z}
			if dir.Len() < 1.0 {
				dir = destiny.Vec3{X: 0, Y: 0, Z: 1}
			}
			dir = dir.Normalize()
			slim.DunDirection = &[3]float64{dir.X, dir.Y, dir.Z}
			if o.MusicUrl.Valid {
				slim.DunMusicURL = o.MusicUrl.String
			}
			if ds.dunGateDir == nil {
				ds.dunGateDir = make(map[uint32]destiny.Vec3)
			}
			ds.dunGateDir[entityID] = dir
		}
		slims = append(slims, slim)
		dmg[entityID] = destiny.DamageState{Shield: 1.0, Tau: 100000, Armor: 1.0, Structure: 1.0}
	}

	if len(balls) > 0 {
		b := balls[0]
		log.Printf("[%s]   first ball entityID=%d pos=(%.0f, %.0f, %.0f) radius=%.0f", addr, b.EntityID, b.Pos.X, b.Pos.Y, b.Pos.Z, b.Radius)
	}

	// Rigid balls are client-only, so grid-mates get them only through this explicit
	// delivery. Gate activation stays owner-only: the room state lives on the spawning connection.
	broadcastDunRigidBalls(ds, balls, slims, dmg)

	if len(balls) > 0 && ds.dunPendingMode {
		// Warp-arrival pre-spawn: stash the room for the landing batch, which claims and
		// delivers everything at one stamp.
		ds.dunPendingBalls = append(ds.dunPendingBalls, balls...)
		ds.dunPendingSlims = append(ds.dunPendingSlims, slims...)
		if ds.dunPendingDmg == nil {
			ds.dunPendingDmg = make(map[uint32]destiny.DamageState)
		}
		for id, d := range dmg {
			ds.dunPendingDmg[id] = d
		}
		return nil
	}

	// Claim before sending, not after. A room re-spawned by a trigger, or re-entered while
	// the client holds its balls, would otherwise re-deliver them: a second AddBalls the
	// ballpark ignores and the overview shows as a second row. See Entity.MarkBallsKnown.
	balls, slims, dmg, ballIDs, claimed := claimUnknownBallsFor(ds.entity, balls, slims, dmg)
	if len(balls) > 0 && claimed && ds.entity.Conn != nil {
		// Queued like any other ball, one AddBalls for the batch keyed on its first ball: a wave
		// released on the landing or a kill must not mint past the stamp the death or landing
		// just took. See docs/destiny-stamps.md#budget-and-triage.
		addKey := "add:" + strconv.FormatUint(uint64(ballIDs[0]), 10)
		ds.entity.QueueAddActionFunc(addKey, ballIDs[0], func(stamp uint32) marshal.PyRep {
			return destiny.BuildAddBallsAction(stamp, balls, slims, dmg)
		})
		return nil
	}
	if len(balls) > 0 {
		// No ballpark yet, so the queue cannot deliver: send at synced stamp + 1 so the AddBalls
		// timestamp is at or just ahead of the client's clock (sendFloorAdvance).
		stamp, err := ds.sendFloorAdvance(func(stamp uint32) error {
			return destiny.SendAddBallsFull(c, globalNodeID, uint64(accountID), stamp, balls, slims, dmg)
		})
		if ds.entity != nil {
			switch {
			case err != nil && claimed:
				// Roll the claim back (as the reconciliation prune does for a failed RemoveBalls):
				// the server would otherwise believe the client holds balls it never received.
				ds.entity.ForgetBalls(ballIDs...)
			case err == nil && !claimed:
				// No ballpark yet, so nothing was claimed; the batch went out whole and this records it.
				ds.entity.MarkBallsKnown(ballIDs...)
			}
		}

		// Raise other bubble players' stamp floor above the AddBalls stamp. Floor only,
		// never re-anchor their wall clock (see sendFloorAdvance).
		if ds.solarSystem != nil && ds.entity != nil {
			// Connected mates only: SyncStamp pushes another entity's next mint into overshoot
			// recovery, which is wasted on a headless ball.
			mates := ds.solarSystem.GetBubbleConnectedPlayersExcept(ds.entity.ID)
			for _, mate := range mates {
				mate.SyncStamp(stamp)
			}
		}

		return err
	}
	return nil
}

// handleEditDungeon loads a dungeon room's objects from the DB and spawns
// them as rigid balls in the client's ballpark with dungeon SlimItem attributes.
// Called for both EditDungeon and PlayDungeon bound calls.
func handleEditDungeon(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB, ds *destinyConn, sessReg *esp.SessionRegistry, npcMgr *npc.NPCManager) error {
	// PlayDungeon(dungeonVID, selectedRoom) sends selectedRoom as a 1-based index into
	// the dungeon's room list, not a roomID.
	var dungeonVID int64
	var roomIndex int64 // 1-based index from client
	var roomID int64    // actual DB roomID

	// Extract dungeonVID from positional arg[0].
	if len(cs.ArgTuple.Items) >= 1 {
		if v, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
			dungeonVID = int64(v.Value)
		}
	}

	// Extract room selection from kwargs.
	if cs.ArgDict != nil {
		for _, e := range cs.ArgDict.Items {
			if k, ok := e.Key.(marshal.PyString); ok {
				switch k.Value {
				case "roomID":
					if v, ok := e.Value.(marshal.PyInt); ok {
						roomID = int64(v.Value)
					}
				case "selectedRoom":
					if v, ok := e.Value.(marshal.PyInt); ok {
						roomIndex = int64(v.Value)
					}
				case "dungeonVID":
					if v, ok := e.Value.(marshal.PyInt); ok {
						dungeonVID = int64(v.Value)
					}
				}
			}
		}
	}
	// Positional arg[1] is the room index (1-based).
	if roomID == 0 && roomIndex == 0 && len(cs.ArgTuple.Items) >= 2 {
		if v, ok := cs.ArgTuple.Items[1].(marshal.PyInt); ok {
			roomIndex = int64(v.Value)
		}
	}

	log.Printf("[%s] handleEditDungeon: dungeonVID=%d roomIndex=%d roomID=%d argTuple=%d items argDict=%v",
		addr, dungeonVID, roomIndex, roomID, len(cs.ArgTuple.Items), cs.ArgDict)

	// Resolve room index or missing roomID via the dungeon's room list.
	if dungeonVID != 0 && roomID == 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		rooms, err := evedb.GetDungeonRoomIDs(ctx, sqldb, dungeonVID)
		if err == nil && len(rooms) > 0 {
			idx := int(roomIndex) - 1 // convert 1-based to 0-based
			if idx < 0 || idx >= len(rooms) {
				idx = 0
			}
			roomID = rooms[idx]
			log.Printf("[%s] handleEditDungeon: resolved dungeonVID=%d roomIndex=%d → roomID=%d",
				addr, dungeonVID, roomIndex, roomID)
		} else if len(rooms) == 0 {
			log.Printf("[%s] handleEditDungeon: dungeonVID=%d has no rooms — nothing to spawn", addr, dungeonVID)
		}
	}

	if err := server.SendCallRsp(c, pkt, accountID, marshal.PyNone{}); err != nil {
		return err
	}
	return spawnDungeonRoom(c, addr, accountID, sqldb, ds, roomID, sessReg, npcMgr)
}

// handleGotoRoom switches the dungeon editor to a different room.
// Clears existing dungeon balls and spawns the new room's objects.
func handleGotoRoom(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB, ds *destinyConn, sessReg *esp.SessionRegistry, npcMgr *npc.NPCManager) error {
	// GotoRoom(dungeonVID, roomID) arrives as positional args.
	var roomID int64
	if cs.ArgDict != nil {
		for _, e := range cs.ArgDict.Items {
			if k, ok := e.Key.(marshal.PyString); ok && k.Value == "roomID" {
				if v, ok := e.Value.(marshal.PyInt); ok {
					roomID = int64(v.Value)
				}
			}
		}
	}
	if roomID == 0 && len(cs.ArgTuple.Items) >= 2 {
		if v, ok := cs.ArgTuple.Items[1].(marshal.PyInt); ok {
			roomID = int64(v.Value)
		}
	}

	log.Printf("[%s] → bound.GotoRoom roomID=%d", addr, roomID)
	if err := server.SendCallRsp(c, pkt, accountID, marshal.PyNone{}); err != nil {
		return err
	}
	return spawnDungeonRoom(c, addr, accountID, sqldb, ds, roomID, sessReg, npcMgr)
}

// dungeonRoomSpacing is how far past a gate the next room is anchored. It is
// AU-scale so a gate hop is a real warp: destiny.dll turns any warp under 1e10 m
// into a sub-warp GotoPoint (WarpTo, 0x10017bb0), which crawls and snaps on arrival.
const dungeonRoomSpacing = 1.5 * 149597870700.0 // 1.5 AU in metres

// handleActivateGate implements keeper.ActivateAccelerationGate, which the client
// calls inside PerformSessionChange('activate gate') (menusvc.py). The gate must be
// an object of the loaded room. Activation plays effects.WarpGateEffect, warps the
// ship along the gate's direction and loads the next room.
func handleActivateGate(c *evenet.Conn, addr string, cs *server.CallStream,
	pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB, ds *destinyConn,
	sessReg *esp.SessionRegistry, npcMgr *npc.NPCManager) error {

	var gateID uint32
	if len(cs.ArgTuple.Items) >= 1 {
		if v, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
			gateID = uint32(v.Value)
		}
	}
	// Ack first: the client wraps this call in a session change and waits for the
	// response. The travel arrives as destiny updates afterwards.
	if err := server.SendCallRsp(c, pkt, accountID, marshal.PyNone{}); err != nil {
		return err
	}
	runActivateGate(c, addr, accountID, gateID, sqldb, ds, sessReg, npcMgr)
	return nil
}

// runActivateGate performs the acceleration-gate side effects (FX, ship
// reposition, next-room load). The RPC response must already have been sent;
// failures are logged and leave the player where they are.
func runActivateGate(c *evenet.Conn, addr string, accountID int64, gateID uint32,
	sqldb *sql.DB, ds *destinyConn, sessReg *esp.SessionRegistry, npcMgr *npc.NPCManager) {

	if ds.dm == nil || ds.dunRoomID == 0 || ds.dunObjects == nil {
		log.Printf("[%s] → keeper.ActivateAccelerationGate gateID=%d — no dungeon room loaded", addr, gateID)
		return
	}
	objectID, ok := ds.dunObjGet(gateID)
	if !ok {
		log.Printf("[%s] → keeper.ActivateAccelerationGate gateID=%d — not a dungeon object", addr, gateID)
		return
	}
	// The gate of an open site that has despawned around the pilot leads
	// nowhere: its deeper rooms went with it.
	if ds.dunOpenSiteID > 0 && openSiteByID(ds.dunOpenSiteID) == nil {
		log.Printf("[%s] → keeper.ActivateAccelerationGate gateID=%d — open site %d has despawned", addr, gateID, ds.dunOpenSiteID)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Resolve the dungeon this room belongs to and pick the next room.
	var dungeonVID int64
	if err := sqldb.QueryRowContext(ctx,
		`SELECT dungeonVID FROM dunRooms WHERE roomID = ?`, ds.dunRoomID).Scan(&dungeonVID); err != nil {
		log.Printf("[%s] ActivateAccelerationGate: dungeonVID lookup roomID=%d: %v", addr, ds.dunRoomID, err)
		return
	}
	roomIDs, err := evedb.GetDungeonRoomIDs(ctx, sqldb, dungeonVID)
	if err != nil {
		log.Printf("[%s] ActivateAccelerationGate: GetDungeonRoomIDs vid=%d: %v", addr, dungeonVID, err)
		return
	}
	var nextRoomID int64
	for i, id := range roomIDs {
		if id == ds.dunRoomID && i+1 < len(roomIDs) {
			nextRoomID = roomIDs[i+1]
			break
		}
	}
	if nextRoomID == 0 {
		log.Printf("[%s] → keeper.ActivateAccelerationGate gateID=%d — last room, gate leads nowhere", addr, gateID)
		return
	}

	// Gate position and typeID for the FX and travel direction.
	shipPos := ds.dm.Pos
	gatePos := shipPos
	var gateTypeID int64
	if obj, objErr := evedb.GetObjectByID(ctx, sqldb, objectID); objErr == nil {
		gateTypeID = obj.TypeID
		gatePos = destiny.Vec3{
			X: obj.X + ds.dunOffset.X,
			Y: obj.Y + ds.dunOffset.Y,
			Z: obj.Z + ds.dunOffset.Z,
		}
	} else {
		log.Printf("[%s] ActivateAccelerationGate: GetObjectByID %d: %v", addr, objectID, objErr)
	}

	// Gate activation FX: effects.WarpGateEffect reads ballIDs = [gate, ship]
	// (accelerationGate.py), so sourceID is the gate and targetID the ship.
	fxArgs := []marshal.PyRep{
		marshal.PyInt{Value: uint64(gateID)},     // shipID (the gate)
		marshal.PyInt{Value: uint64(gateID)},     // moduleID
		marshal.PyInt{Value: uint64(gateTypeID)}, // moduleTypeID
		marshal.PyInt{Value: uint64(ds.dm.EntityID)},
		marshal.PyInt{Value: 0}, // otherTypeID
		marshal.PyList{},        // area
		marshal.PyString{Value: "effects.WarpGateEffect"},
		marshal.PyInt{Value: 0}, // isOffensive
		marshal.PyInt{Value: 1}, // start
		marshal.PyInt{Value: 1}, // active
	}
	ds.syncStamp()
	if err := ds.sendSelfStamped(func(fxStamp uint32) error {
		return destiny.SendSimpleAction(c, globalNodeID, uint64(accountID), fxStamp,
			"OnSpecialFX", fxArgs...)
	}); err != nil {
		log.Printf("[%s] ActivateAccelerationGate: FX: %v", addr, err)
	}
	broadcastFXAction(ds, "OnSpecialFX", fxArgs...)

	// Travel direction: the gate's authored outward direction cached at room spawn (the
	// vector the client's dunDirection aims the model along), else ship to gate.
	dir, haveDir := ds.dunGateDir[gateID]
	if !haveDir {
		dir = destiny.Vec3{X: gatePos.X - shipPos.X, Y: gatePos.Y - shipPos.Y, Z: gatePos.Z - shipPos.Z}
		if dir.Len() < 1.0 {
			dir = destiny.Vec3{X: 0, Y: 0, Z: 1}
		}
		dir = dir.Normalize()
	}

	// The next room is anchored dungeonRoomSpacing past the gate along its direction:
	// far enough for the tunnel animation and a fresh bubble.
	landing := gatePos.Add(dir.Scale(dungeonRoomSpacing))

	log.Printf("[%s] → keeper.ActivateAccelerationGate gateID=%d → room %d → %d (dungeon %d) landing=(%.0f,%.0f,%.0f)",
		addr, gateID, ds.dunRoomID, nextRoomID, dungeonVID, landing.X, landing.Y, landing.Z)

	// Dungeon logic: gateActivated triggers of the departure room.
	fireDungeonTriggers(c, addr, accountID, ds, "gateActivated", objectID)

	// Warp to the landing point, then spawn the next room with its beacon just ahead
	// of the ship (dunSpawnShift). The mission and open-site tags carry across gates.
	ds.dunSpawnShift = dir.Scale(deadspaceLandingBuffer)
	gateMissionID := ds.dunMissionID
	gateOpenSiteID := ds.dunOpenSiteID
	// Gate landing is exact: the ship stops deadspaceLandingBuffer short of the next room's beacon
	// along the gate's own direction, whatever angle it entered the warp at. The room anchor is that
	// point, and dunSpawnShift carries the beacon the same distance ahead.
	gateLanding := landing.Sub(dir.Scale(deadspaceLandingBuffer))
	startPlayerWarp(c, addr, accountID, sqldb, ds, ds.dm, landing, 0, deadspaceLandingBuffer, &gateLanding, func() {
		ds.dunPendingOpenSite = gateOpenSiteID
		if err := spawnDungeonRoom(c, addr, accountID, sqldb, ds, nextRoomID, sessReg, npcMgr, gateMissionID); err != nil {
			log.Printf("[%s] ActivateAccelerationGate: spawnDungeonRoom %d: %v", addr, nextRoomID, err)
		}
	})
}

// handleDungeonReset removes all dungeon balls from the client's ballpark.
func handleDungeonReset(c *evenet.Conn, addr string, pkt *evenet.PyPacket, accountID int64, ds *destinyConn, sessReg *esp.SessionRegistry, npcMgr *npc.NPCManager, sqldb *sql.DB) error {
	if n := clearDungeonBalls(c, addr, accountID, ds, npcMgr, sqldb); n > 0 {
		log.Printf("[%s] → DungeonReset removed %d balls", addr, n)
	} else {
		log.Printf("[%s] → DungeonReset (no dungeon objects)", addr)
	}
	ds.dunRoomID = 0
	ds.dunNoWarp = false
	if sessReg != nil {
		sessReg.Update(addr, func(s *esp.Session) {
			s.DunRoomID = 0
		})
	}
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// dunObjectEntityID looks up the ballpark entity ID for a given dungeon objectID.
func dunObjectEntityID(ds *destinyConn, objectID int64) (uint32, bool) {
	ds.dunObjMu.Lock()
	defer ds.dunObjMu.Unlock()
	for eid, oid := range ds.dunObjects {
		if oid == objectID {
			return eid, true
		}
	}
	return 0, false
}

// handleDungeonEditObjectWithSync intercepts dungeon.EditObject and moves the ball
// in the ballpark after the DB write, so dragging an object in the editor is visible.
func handleDungeonEditObjectWithSync(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB, ds *destinyConn) error {
	// Parse kwargs the same way dungeonsvc does.
	var objectID int64
	var newX, newY, newZ *float64
	if cs.ArgDict != nil {
		for _, e := range cs.ArgDict.Items {
			k, ok := e.Key.(marshal.PyString)
			if !ok {
				continue
			}
			switch k.Value {
			case "objectID":
				if v, ok := e.Value.(marshal.PyInt); ok {
					objectID = int64(v.Value)
				}
			case "x":
				v := pyRepToFloat(e.Value)
				newX = &v
			case "y":
				v := pyRepToFloat(e.Value)
				newY = &v
			case "z":
				v := pyRepToFloat(e.Value)
				newZ = &v
			}
		}
	}

	// Delegate to the existing dungeon handler for the DB update + call response.
	if err := dungeon.Handle(c, addr, cs, pkt, accountID, sqldb); err != nil {
		return err
	}

	// If position fields present AND we have this object in the ballpark, sync the ball.
	if newX == nil && newY == nil && newZ == nil {
		return nil // rotation/radius only: no ball position change
	}
	if ds == nil || ds.dm == nil {
		return nil
	}
	eid, ok := dunObjectEntityID(ds, objectID)
	if !ok {
		return nil // object not in current ballpark
	}

	// Compute ballpark position: dungeon-space coords + spawn offset.
	var bx, by, bz float64
	if newX != nil {
		bx = *newX + ds.dunOffset.X
	}
	if newY != nil {
		by = *newY + ds.dunOffset.Y
	}
	if newZ != nil {
		bz = *newZ + ds.dunOffset.Z
	}

	ds.syncStamp()
	if err := ds.sendSelfStamped(func(s uint32) error {
		return destiny.SendSimpleAction(c, globalNodeID, uint64(accountID), s,
			"SetBallPosition",
			marshal.PyInt{Value: uint64(eid)},
			marshal.PyReal{Value: bx},
			marshal.PyReal{Value: by},
			marshal.PyReal{Value: bz})
	}); err != nil {
		log.Printf("[%s] dungeon.EditObject: SetBallPosition entityID=%d FAILED: %v", addr, eid, err)
	}
	// Wall anchors are not advanced here (see sendFloorAdvance).
	return nil
}

// pyRepToFloat converts a PyRep (PyReal or PyInt) to float64.
func pyRepToFloat(r marshal.PyRep) float64 {
	switch v := r.(type) {
	case marshal.PyReal:
		return v.Value
	case marshal.PyInt:
		return float64(v.Value)
	}
	return 0
}

// handleScenario dispatches calls to the "scenario" service, which the client's
// dungeoneditor.py reaches through eve.RemoteSvc('scenario') (EditRoom, ResetD,
// PlayDungeon and others).
func handleScenario(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB, ds *destinyConn, sessReg *esp.SessionRegistry, npcMgr *npc.NPCManager) error {
	switch cs.Method {
	case "EditRoom":
		// EditRoom(dungeonVID, selectedRoom) arrives as positional args.
		log.Printf("[%s] ← scenario.EditRoom", addr)
		return handleEditDungeon(c, addr, cs, pkt, accountID, sqldb, ds, sessReg, npcMgr)
	case "PlayDungeon":
		log.Printf("[%s] ← scenario.PlayDungeon", addr)
		return handleEditDungeon(c, addr, cs, pkt, accountID, sqldb, ds, sessReg, npcMgr)
	case "ResetD":
		log.Printf("[%s] ← scenario.ResetD", addr)
		return handleDungeonReset(c, addr, pkt, accountID, ds, sessReg, npcMgr, sqldb)
	case "GotoRoom":
		return handleGotoRoom(c, addr, cs, pkt, accountID, sqldb, ds, sessReg, npcMgr)
	case "GetDunObjects":
		// Return list of ball IDs for all dungeon objects currently in the ballpark.
		cur := ds.dunObjSnapshot()
		items := make([]marshal.PyRep, 0, len(cur))
		for eid := range cur {
			items = append(items, marshal.PyInt{Value: uint64(eid)})
		}
		log.Printf("[%s] → scenario.GetDunObjects → %d objects", addr, len(items))
		return server.SendCallRsp(c, pkt, accountID, marshal.PyList{Items: items})
	case "GetSelObjects":
		// Return list of currently selected ball IDs.
		items := make([]marshal.PyRep, 0, len(ds.dunSelected))
		for _, eid := range ds.dunSelected {
			items = append(items, marshal.PyInt{Value: uint64(eid)})
		}
		log.Printf("[%s] → scenario.GetSelObjects → %d selected", addr, len(items))
		return server.SendCallRsp(c, pkt, accountID, marshal.PyList{Items: items})
	case "IsSelectedByObjID":
		// Check if a specific ball ID is in the selection.
		var queryID uint64
		if len(cs.ArgTuple.Items) >= 1 {
			if v, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
				queryID = v.Value
			}
		}
		selected := false
		for _, eid := range ds.dunSelected {
			if uint64(eid) == queryID {
				selected = true
				break
			}
		}
		return server.SendCallRsp(c, pkt, accountID, marshal.PyBool{Value: selected})
	case "SetSelectionByID":
		// Client sends a list of ball IDs to select.
		ds.dunSelected = nil
		if len(cs.ArgTuple.Items) >= 1 {
			if lst, ok := cs.ArgTuple.Items[0].(marshal.PyList); ok {
				for _, item := range lst.Items {
					if v, ok := item.(marshal.PyInt); ok {
						ds.dunSelected = append(ds.dunSelected, uint32(v.Value))
					}
				}
			}
		}
		log.Printf("[%s] → scenario.SetSelectionByID → %d selected", addr, len(ds.dunSelected))
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	case "DeleteSelected":
		return handleScenarioDeleteSelected(c, addr, pkt, accountID, sqldb, ds)
	case "DuplicateSelection":
		return handleScenarioDuplicateSelection(c, addr, cs, pkt, accountID, sqldb, ds)
	case "RefreshSelection":
		return handleScenarioRefreshSelection(c, addr, pkt, accountID, sqldb, ds)
	case "SetSelectedRadius":
		return handleScenarioSetSelectedRadius(c, addr, cs, pkt, accountID, sqldb, ds)
	case "SetRotate":
		return handleScenarioSetRotate(c, addr, cs, pkt, accountID, sqldb, ds)
	case "JitterSelection", "ArrangeSelection", "RotateSelected":
		// The client computes positions locally and calls EditObject per object, which the
		// EditObject sync already covers.
		log.Printf("[%s] → scenario.%s (client-driven, no-op)", addr, cs.Method)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	default:
		server.RecordUnhandled(addr, "scenario", cs, true)
		log.Printf("[%s] scenario: unhandled method %q — returning None", addr, cs.Method)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
}

// handleScenarioDeleteSelected removes selected dungeon objects from the DB
// and ballpark.
func handleScenarioDeleteSelected(c *evenet.Conn, addr string, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB, ds *destinyConn) error {
	if len(ds.dunSelected) == 0 {
		log.Printf("[%s] → scenario.DeleteSelected: nothing selected", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var removeIDs []uint32
	for _, eid := range ds.dunSelected {
		objectID, ok := ds.dunObjGet(eid)
		if !ok {
			continue
		}
		if err := evedb.RemoveObject(ctx, sqldb, objectID); err != nil {
			log.Printf("[%s] DeleteSelected: RemoveObject(%d): %v", addr, objectID, err)
			continue
		}
		removeIDs = append(removeIDs, eid)
		ds.dunObjMu.Lock()
		delete(ds.dunObjects, eid)
		ds.dunObjMu.Unlock()
	}
	ds.dunSelected = nil

	if len(removeIDs) > 0 && ds.dm != nil {
		if _, err := ds.sendFloorAdvance(func(stamp uint32) error {
			return destiny.SendRemoveBalls(c, globalNodeID, uint64(accountID), stamp, removeIDs)
		}); err != nil {
			log.Printf("[%s] DeleteSelected: SendRemoveBalls: %v", addr, err)
		}
		if ds.entity != nil {
			ds.entity.ForgetBalls(removeIDs...)
		}
		broadcastDunRemoveBalls(ds, removeIDs)
	}
	log.Printf("[%s] → scenario.DeleteSelected: removed %d objects", addr, len(removeIDs))
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// handleScenarioDuplicateSelection duplicates selected objects with an offset.
func handleScenarioDuplicateSelection(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB, ds *destinyConn) error {
	if len(ds.dunSelected) == 0 {
		log.Printf("[%s] → scenario.DuplicateSelection: nothing selected", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}

	// Parse args: DuplicateSelection(amount, offsetX, offsetY, offsetZ)
	amount := 1
	var offsetX, offsetY, offsetZ float64
	if len(cs.ArgTuple.Items) >= 1 {
		if v, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok && v.Value > 0 {
			amount = int(v.Value)
		}
	}
	if len(cs.ArgTuple.Items) >= 4 {
		offsetX = pyRepToFloat(cs.ArgTuple.Items[1])
		offsetY = pyRepToFloat(cs.ArgTuple.Items[2])
		offsetZ = pyRepToFloat(cs.ArgTuple.Items[3])
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var balls []destiny.BallDesc
	var slims []destiny.SlimItem
	dmg := make(map[uint32]destiny.DamageState)

	for _, eid := range ds.dunSelected {
		objectID, ok := ds.dunObjGet(eid)
		if !ok {
			continue
		}
		for i := 1; i <= amount; i++ {
			ox := offsetX * float64(i)
			oy := offsetY * float64(i)
			oz := offsetZ * float64(i)

			newID, err := evedb.CopyObject(ctx, sqldb, objectID, ox, oy, oz)
			if err != nil {
				log.Printf("[%s] DuplicateSelection: CopyObject(%d): %v", addr, objectID, err)
				continue
			}
			obj, err := evedb.GetObjectByID(ctx, sqldb, newID)
			if err != nil {
				log.Printf("[%s] DuplicateSelection: GetObjectByID(%d): %v", addr, newID, err)
				continue
			}

			newEID := nextDunRigidID()
			ds.dunObjMu.Lock()
			ds.dunObjects[newEID] = newID
			ds.dunObjMu.Unlock()

			radius := 500.0
			if obj.Radius.Valid {
				radius = obj.Radius.Float64
			}
			spawnPos := destiny.Vec3{
				X: obj.X + ds.dunOffset.X,
				Y: obj.Y + ds.dunOffset.Y,
				Z: obj.Z + ds.dunOffset.Z,
			}

			// Query group/category for this typeID.
			var groupID, categoryID int64
			row := sqldb.QueryRowContext(ctx,
				`SELECT groupID, categoryID FROM invTypes
				 JOIN invGroups USING(groupID)
				 WHERE typeID = ?`, obj.TypeID)
			if err := row.Scan(&groupID, &categoryID); err != nil {
				groupID = 0
				categoryID = 0
			}

			bd := destiny.BallDesc{
				EntityID: newEID,
				Mode:     destiny.ModeRigid,
				SubType:  destiny.RigidSubType(obj.IsSolid(groupID)),
				Pos:      spawnPos,
				Radius:   radius,
			}
			balls = append(balls, bd)

			slim := destiny.SlimItem{
				BallID:      int64(newEID),
				ItemID:      int64(newEID),
				TypeID:      obj.TypeID,
				GroupID:     groupID,
				CategoryID:  categoryID,
				OwnerID:     1,
				DunObjectID: newID,
				DunRoomID:   obj.RoomID,
				DunX:        obj.X,
				DunY:        obj.Y,
				DunZ:        obj.Z,
			}
			if obj.Yaw.Valid {
				yaw, pitch, roll := obj.Yaw.Float64, obj.Pitch.Float64, obj.Roll.Float64
				slim.DunYaw = &yaw
				slim.DunPitch = &pitch
				slim.DunRoll = &roll
			}
			if obj.Radius.Valid {
				r := obj.Radius.Float64
				slim.DunRadius = &r
			}
			if obj.AggressionRange.Valid {
				a := obj.AggressionRange.Float64
				slim.DunAggrRange = &a
			}
			slims = append(slims, slim)
			dmg[newEID] = destiny.DamageState{Shield: 1.0, Tau: 100000, Armor: 1.0, Structure: 1.0}
		}
	}

	if len(balls) > 0 && ds.dm != nil {
		if _, err := ds.sendFloorAdvance(func(stamp uint32) error {
			return destiny.SendAddBallsFull(c, globalNodeID, uint64(accountID), stamp, balls, slims, dmg)
		}); err != nil {
			log.Printf("[%s] DuplicateSelection: SendAddBallsFull: %v", addr, err)
		}
		if ds.entity != nil {
			ballIDs := make([]uint32, len(balls))
			for i, b := range balls {
				ballIDs[i] = b.EntityID
			}
			ds.entity.MarkBallsKnown(ballIDs...)
		}
	}
	log.Printf("[%s] → scenario.DuplicateSelection: created %d new objects", addr, len(balls))
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// handleScenarioRefreshSelection re-reads selected objects from DB and syncs
// ball positions.
func handleScenarioRefreshSelection(c *evenet.Conn, addr string, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB, ds *destinyConn) error {
	if len(ds.dunSelected) == 0 || ds.dm == nil {
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, eid := range ds.dunSelected {
		objectID, ok := ds.dunObjGet(eid)
		if !ok {
			continue
		}
		obj, err := evedb.GetObjectByID(ctx, sqldb, objectID)
		if err != nil {
			log.Printf("[%s] RefreshSelection: GetObjectByID(%d): %v", addr, objectID, err)
			continue
		}
		bx := obj.X + ds.dunOffset.X
		by := obj.Y + ds.dunOffset.Y
		bz := obj.Z + ds.dunOffset.Z

		ds.syncStamp()
		if err := ds.sendSelfStamped(func(stamp uint32) error {
			return destiny.SendSimpleAction(c, globalNodeID, uint64(accountID), stamp,
				"SetBallPosition",
				marshal.PyInt{Value: uint64(eid)},
				marshal.PyReal{Value: bx},
				marshal.PyReal{Value: by},
				marshal.PyReal{Value: bz})
		}); err != nil {
			log.Printf("[%s] RefreshSelection: SetBallPosition entityID=%d: %v", addr, eid, err)
		}
	}
	// Wall anchors are not advanced here (see sendFloorAdvance).
	log.Printf("[%s] → scenario.RefreshSelection: refreshed %d objects", addr, len(ds.dunSelected))
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// handleScenarioSetSelectedRadius updates the radius for all selected objects.
func handleScenarioSetSelectedRadius(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB, ds *destinyConn) error {
	var radius float64
	if len(cs.ArgTuple.Items) >= 1 {
		radius = pyRepToFloat(cs.ArgTuple.Items[0])
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	count := 0
	for _, eid := range ds.dunSelected {
		objectID, ok := ds.dunObjGet(eid)
		if !ok {
			continue
		}
		r := radius
		if err := evedb.EditObject(ctx, sqldb, objectID, evedb.EditObjectFields{Radius: &r}); err != nil {
			log.Printf("[%s] SetSelectedRadius: EditObject(%d): %v", addr, objectID, err)
			continue
		}
		count++
	}
	log.Printf("[%s] → scenario.SetSelectedRadius: radius=%.1f applied to %d objects", addr, radius, count)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// handleScenarioSetRotate updates yaw/pitch/roll for all selected objects.
func handleScenarioSetRotate(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB, ds *destinyConn) error {
	var yaw, pitch, roll float64
	if len(cs.ArgTuple.Items) >= 3 {
		yaw = pyRepToFloat(cs.ArgTuple.Items[0])
		pitch = pyRepToFloat(cs.ArgTuple.Items[1])
		roll = pyRepToFloat(cs.ArgTuple.Items[2])
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	count := 0
	for _, eid := range ds.dunSelected {
		objectID, ok := ds.dunObjGet(eid)
		if !ok {
			continue
		}
		y, p, r := yaw, pitch, roll
		if err := evedb.EditObject(ctx, sqldb, objectID, evedb.EditObjectFields{Yaw: &y, Pitch: &p, Roll: &r}); err != nil {
			log.Printf("[%s] SetRotate: EditObject(%d): %v", addr, objectID, err)
			continue
		}
		count++
	}
	log.Printf("[%s] → scenario.SetRotate: yaw=%.2f pitch=%.2f roll=%.2f applied to %d objects", addr, yaw, pitch, roll, count)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// makeInvRowset builds a util.Rowset for inventory/List-style responses.
func makeInvRowset(cols []string, rows [][]marshal.PyRep) marshal.PyRep {
	header := make([]marshal.PyRep, len(cols))
	for i, c := range cols {
		header[i] = marshal.PyString{Value: c}
	}
	lines := make([]marshal.PyRep, len(rows))
	for i, r := range rows {
		items := make([]marshal.PyRep, len(r))
		copy(items, r)
		lines[i] = marshal.PyList{Items: items}
	}
	return marshal.PyObject{
		Type: "util.Rowset",
		Args: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "header"}, Value: marshal.PyList{Items: header}},
			{Key: marshal.PyString{Value: "RowClass"}, Value: marshal.PyString{Value: "util.Row", IsType1: true}},
			{Key: marshal.PyString{Value: "lines"}, Value: marshal.PyList{Items: lines}},
		}},
	}
}

// handleBoundAddCorporation handles the AddCorporation bound-object call. The args
// are a 12-element tuple (corpName, tickerName, description, url, taxRate, shape1-3,
// color1-3, unused). It creates the corporation, joins the creator as CEO, moves
// the client's corpid by session change and returns the new corpID.
func handleBoundAddCorporation(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID int64, sqldb *sql.DB) error {
	if len(cs.ArgTuple.Items) < 11 {
		log.Printf("[%s] bound.AddCorporation: too few args (%d)", addr, len(cs.ArgTuple.Items))
		return server.SendCallException(c, pkt, accountID, nil)
	}
	args := cs.ArgTuple.Items

	// pyStr, not a PyString assertion: a typed description arrives as a PyBuffer (cp1252 str).
	corpName, ok1 := pyStr(args[0])
	tickerName, ok2 := pyStr(args[1])
	description, ok3 := pyStr(args[2])
	url, ok4 := pyStr(args[3])
	if !ok1 || !ok2 || !ok3 || !ok4 {
		log.Printf("[%s] bound.AddCorporation: bad string args", addr)
		return server.SendCallException(c, pkt, accountID, nil)
	}

	var taxRate float64
	switch v := args[4].(type) {
	case marshal.PyReal:
		taxRate = v.Value
	case marshal.PyInt:
		taxRate = float64(v.Value)
	}

	nullInt := func(rep marshal.PyRep) sql.NullInt64 {
		if iv, ok := rep.(marshal.PyInt); ok {
			return sql.NullInt64{Int64: int64(iv.Value), Valid: true}
		}
		return sql.NullInt64{}
	}
	shape1 := nullInt(args[5])
	shape2 := nullInt(args[6])
	shape3 := nullInt(args[7])
	color1 := nullInt(args[8])
	color2 := nullInt(args[9])
	color3 := nullInt(args[10])

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Read the character's current corp so we can decrement it.
	var oldCorpID int64
	if err := sqldb.QueryRowContext(ctx, `SELECT corporationID FROM character_ WHERE characterID = ?`, charID).Scan(&oldCorpID); err != nil {
		log.Printf("[%s] bound.AddCorporation: lookup oldCorpID: %v", addr, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}

	// The client's gate (bco_members.MemberCanCreateCorporation) only hides the button.
	// A sitting CEO would leave their corp headless, so the server refuses.
	if isCorpCEO(ctx, sqldb, oldCorpID, charID) {
		return clientMsgErr(c, pkt, accountID, "CEOCannotCreateCorporation", nil)
	}

	// const.corporationStartupCost is charged before anything is written, so a refused
	// fee leaves no half-made corporation; a failure after the charge refunds it.
	charged, balance, err := evedb.ChargeCorporationCreation(ctx, sqldb, charID)
	if err != nil {
		log.Printf("[%s] bound.AddCorporation: charge fee: %v", addr, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	if !charged {
		return clientMsgErr(c, pkt, accountID, "NotEnoughMoney", map[string]marshal.PyRep{
			"amount":  marshal.PyString{Value: commaGroups(int64(evedb.CorporationStartupCost))},
			"balance": marshal.PyString{Value: commaGroups(int64(balance))},
		})
	}
	refund := func() {
		bal, rerr := evedb.RefundCorporationCreation(ctx, sqldb, charID)
		if rerr != nil {
			log.Printf("[%s] bound.AddCorporation: refund failed: %v", addr, rerr)
			return
		}
		pushAccountChange(c, accountID, charID, bal)
	}

	corpID, err := evedb.AddCorporation(ctx, sqldb, charID,
		corpName, tickerName, description, url,
		taxRate, shape1, shape2, shape3, color1, color2, color3)
	if err != nil {
		log.Printf("[%s] bound.AddCorporation: AddCorporation: %v (corpID=%d)", addr, err, corpID)
		// A corp row that exists (a later step failed) keeps the fee: the founder can still
		// be moved into it.
		if corpID == 0 {
			refund()
		}
		return server.SendCallException(c, pkt, accountID, nil)
	}
	pushAccountChange(c, accountID, charID, balance)

	roles, err := evedb.JoinCorporation(ctx, sqldb, charID, corpID, oldCorpID)
	if err != nil {
		log.Printf("[%s] bound.AddCorporation: JoinCorporation: %v", addr, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}

	// Notify the client that the session corpid has changed.
	if err := sendCorpJoinSessionChange(c, addr, oldCorpID, corpID, roles); err != nil {
		log.Printf("[%s] bound.AddCorporation: session change notify: %v", addr, err)
	}
	// A founder who left an allied corporation drops its alliance.
	_ = slash.SendCorpAllianceSessionChange(ctx, sqldb, c, oldCorpID, corpID)

	// Drop the owners bulk so clients logging in later resolve the new corp;
	// connected clients take the GetOwnersEx / GetCorpTickerNamesEx miss path.
	if globalCacheStore != nil {
		globalCacheStore.InvalidateOwners()
	}

	log.Printf("[%s] → bound.AddCorporation: created corpID=%d %q ticker=%q by charID=%d", addr, corpID, corpName, tickerName, charID)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyInt{Value: uint64(corpID)})
}

// handleBoundCreateAlliance handles the CreateAlliance bound-object call:
// (allianceName, shortName[, description[, url]]). It creates the alliance records,
// links the CEO's corp and notifies every online member of the founding corporation.
// The client's gate only hides the button, so the CEO and fee checks are server-side.
func handleBoundCreateAlliance(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID int64, sqldb *sql.DB, sessReg *esp.SessionRegistry) error {
	if len(cs.ArgTuple.Items) < 2 {
		log.Printf("[%s] bound.CreateAlliance: too few args (%d)", addr, len(cs.ArgTuple.Items))
		return server.SendCallException(c, pkt, accountID, nil)
	}
	args := cs.ArgTuple.Items
	allianceName, ok1 := pyStr(args[0])
	shortName, ok2 := pyStr(args[1])
	if !ok1 || !ok2 {
		log.Printf("[%s] bound.CreateAlliance: bad string args", addr)
		return server.SendCallException(c, pkt, accountID, nil)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Look up the calling character's current corp and alliance.
	var corpID, currentAlliance int64
	if err := sqldb.QueryRowContext(ctx,
		`SELECT c.corporationID, c.allianceID
		   FROM character_ ch JOIN corporation c ON c.corporationID = ch.corporationID
		  WHERE ch.characterID = ?`, charID,
	).Scan(&corpID, &currentAlliance); err != nil {
		log.Printf("[%s] bound.CreateAlliance: lookup corpID: %v", addr, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	if !isCorpCEO(ctx, sqldb, corpID, charID) {
		return clientMsgErr(c, pkt, accountID, "OnlyActiveCEOCanCreateAlliance", nil)
	}
	if currentAlliance != 0 {
		return clientMsgErr(c, pkt, accountID, "AllianceCreateFailCorpInAlliance", nil)
	}
	// Declared wars, name and ticker (db/alliancerules.go) are checked before the fee,
	// so a refusal costs no charge and refund.
	if err := evedb.AllianceCreateRefusal(ctx, sqldb, corpID, allianceName, shortName); err != nil {
		log.Printf("[%s] bound.CreateAlliance %q [%s]: %v", addr, allianceName, shortName, err)
		return refuseErr(c, pkt, accountID, err)
	}

	// The client's create form (all_ui_home.py) sends description and url in
	// args 2 and 3; the alliance window's Home tab renders both.
	optStr := func(i int) string {
		if len(args) > i {
			if s, ok := pyStr(args[i]); ok {
				return s
			}
		}
		return ""
	}

	// const.allianceCreationCost, out of the founding corporation's wallet. Charged
	// before anything is written; a failure after the charge refunds it.
	if err := evedb.ChargeAllianceCreation(ctx, sqldb, corpID, charID); err != nil {
		log.Printf("[%s] bound.CreateAlliance: %v", addr, err)
		return corpUserErr(c, pkt, accountID, err.Error())
	}

	allianceID, err := evedb.CreateAlliance(ctx, sqldb, corpID, charID,
		allianceName, shortName, optStr(2), optStr(3))
	if err != nil {
		log.Printf("[%s] bound.CreateAlliance: %v (refunding fee)", addr, err)
		if rerr := evedb.RefundAllianceCreation(ctx, sqldb, corpID); rerr != nil {
			log.Printf("[%s] bound.CreateAlliance: refund failed: %v", addr, rerr)
		}
		return server.SendCallException(c, pkt, accountID, nil)
	}

	// Drop the owner and alliance-ticker bulks (cfg.eveowners feeds cfg.IsAlliance,
	// cfg.allianceshortnames the bracket ticker). Connected clients use the GetMulti*Ex miss path.
	if globalCacheStore != nil {
		globalCacheStore.InvalidateOwnerNames()
		infocache.Shared.Clear() // names live in the identity memo too
	}

	// db.CreateAlliance linked the whole corporation, so every online member
	// needs the new allianceid, not just the caller's socket.
	pushAllianceSession(sessReg, corpID, 0, allianceID)

	// Wars declared against the founder move to the new alliance, as at any
	// other join; the corp's own declared wars were refused above.
	if adopted, err := evedb.AdoptCorpWars(ctx, sqldb, corpID, allianceID); err != nil {
		log.Printf("[%s] bound.CreateAlliance: adopt wars: %v", addr, err)
	} else {
		announceWarAdoptions(ctx, sqldb, sessReg, adopted)
	}

	log.Printf("[%s] → bound.CreateAlliance: allianceID=%d %q ticker=%q corpID=%d charID=%d",
		addr, allianceID, allianceName, shortName, corpID, charID)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyInt{Value: uint64(allianceID)})
}

// sendAllianceSessionChange sends a SessionChangeNotification moving the client's
// allianceid session variable; 0 is sent as None, which every UI test and
// moniker.GetAlliance expect. all_cso.OnSessionChanged binds or drops the alliance
// object on the new value, so a join or leave is live.
func sendAllianceSessionChange(c *evenet.Conn, addr string, oldAllianceID, allianceID int64) error {
	idRep := func(id int64) marshal.PyRep {
		if id == 0 {
			return marshal.PyNone{}
		}
		return marshal.PyInt{Value: uint64(id)}
	}
	changes := marshal.PyDict{Items: []marshal.DictEntry{
		{Key: marshal.PyString{Value: "allianceid"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			idRep(oldAllianceID), idRep(allianceID),
		}}},
	}}
	payload := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: 0},
			changes,
		}},
		marshal.PyList{Items: []marshal.PyRep{marshal.PyInt{Value: globalNodeID}}},
	}}
	scn := &evenet.PyPacket{
		TypeString: "macho.SessionChangeNotification",
		Type:       evenet.SessionChangeNotification,
		Source: evenet.PyAddress{
			Type:   evenet.AddrNode,
			TypeID: globalNodeID,
		},
		Dest: evenet.PyAddress{
			Type:   evenet.AddrClient,
			TypeID: 130293001608,
		},
		UserID:  0,
		Payload: payload,
		NamedPayload: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "channel"}, Value: marshal.PyString{Value: "sessionchange"}},
		}},
	}
	b, err := marshal.Marshal(scn.Encode())
	if err != nil {
		return err
	}
	log.Printf("[%s] → SessionChangeNotification (allianceid %d→%d)", addr, oldAllianceID, allianceID)
	return c.SendPacket(b)
}

// sendHQSessionChange tells the client its corporation's hqID changed. hqID is a
// role-bearing session value: sessions.py recomputes corprole from it.
// See docs/corp-notes.md#hq-session.
func sendHQSessionChange(c *evenet.Conn, addr string, oldHQ, newHQ int64) error {
	idRep := func(id int64) marshal.PyRep {
		if id == 0 {
			return marshal.PyNone{}
		}
		return marshal.PyInt{Value: uint64(id)}
	}
	changes := marshal.PyDict{Items: []marshal.DictEntry{
		{Key: marshal.PyString{Value: "hqID"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			idRep(oldHQ), idRep(newHQ),
		}}},
	}}
	payload := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: 0},
			changes,
		}},
		marshal.PyList{Items: []marshal.PyRep{marshal.PyInt{Value: globalNodeID}}},
	}}
	scn := &evenet.PyPacket{
		TypeString: "macho.SessionChangeNotification",
		Type:       evenet.SessionChangeNotification,
		Source: evenet.PyAddress{
			Type:   evenet.AddrNode,
			TypeID: globalNodeID,
		},
		Dest: evenet.PyAddress{
			Type:   evenet.AddrClient,
			TypeID: 130293001608,
		},
		UserID:  0,
		Payload: payload,
		NamedPayload: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "channel"}, Value: marshal.PyString{Value: "sessionchange"}},
		}},
	}
	b, err := marshal.Marshal(scn.Encode())
	if err != nil {
		return err
	}
	log.Printf("[%s] → SessionChangeNotification (hqID %d→%d)", addr, oldHQ, newHQ)
	return c.SendPacket(b)
}

// sendCorpJoinSessionChange sends a SessionChangeNotification that updates
// the client's corpid and role bits after a corp join/create. Pass the role
// mask actually granted by JoinCorporation.
func sendCorpJoinSessionChange(c *evenet.Conn, addr string, oldCorpID, newCorpID int64, roles uint64) error {
	none := marshal.PyNone{}
	changes := marshal.PyDict{Items: []marshal.DictEntry{
		{Key: marshal.PyString{Value: "corpid"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(oldCorpID)}, marshal.PyInt{Value: uint64(newCorpID)},
		}}},
		{Key: marshal.PyString{Value: "corprole"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: 0}, marshal.PyInt{Value: roles},
		}}},
		{Key: marshal.PyString{Value: "rolesAtAll"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: 0}, marshal.PyInt{Value: roles},
		}}},
		{Key: marshal.PyString{Value: "rolesAtBase"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: 0}, marshal.PyInt{Value: roles},
		}}},
		{Key: marshal.PyString{Value: "rolesAtHQ"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: 0}, marshal.PyInt{Value: roles},
		}}},
		{Key: marshal.PyString{Value: "rolesAtOther"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: 0}, marshal.PyInt{Value: roles},
		}}},
		{Key: marshal.PyString{Value: "corpAccountKey"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			none, marshal.PyInt{Value: uint64(corp.CorpAccountKey)},
		}}},
	}}
	payload := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: 0},
			changes,
		}},
		marshal.PyList{Items: []marshal.PyRep{marshal.PyInt{Value: globalNodeID}}},
	}}
	scn := &evenet.PyPacket{
		TypeString: "macho.SessionChangeNotification",
		Type:       evenet.SessionChangeNotification,
		Source: evenet.PyAddress{
			Type:   evenet.AddrNode,
			TypeID: globalNodeID,
		},
		Dest: evenet.PyAddress{
			Type:   evenet.AddrClient,
			TypeID: 130293001608,
		},
		UserID:  0,
		Payload: payload,
		NamedPayload: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "channel"}, Value: marshal.PyString{Value: "sessionchange"}},
		}},
	}
	b, err := marshal.Marshal(scn.Encode())
	if err != nil {
		return err
	}
	log.Printf("[%s] → SessionChangeNotification (corp join corpid=%d→%d)", addr, oldCorpID, newCorpID)
	return c.SendPacket(b)
}

// handleInvbroker handles CallReq packets addressed to the "invbroker" service.
// It is not a bound-object service: direct calls trigger item loading.
// MachoBindObject is handled generically before this point.
func handleInvbroker(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, charID int64, sqldb *sql.DB, ds *destinyConn) error {
	switch cs.Method {
	case "GetInventoryFromId":
		// Args: (entityID, passive). The bind number is the cargo hold for the pilot's ship
		// and the itemID for any other container the pilot may open (bays, loot, offices).
		bindNum := uint64(2)
		if len(cs.ArgTuple.Items) >= 1 {
			if eid, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok && sqldb != nil && charID != 0 {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				shipID, _ := evedb.GetCurrentShipID(ctx, sqldb, charID)
				cancel()
				if shipID != 0 && int64(eid.Value) == shipID {
					bindNum = cargoBindID
				} else if isStructureBay(ds, int64(eid.Value)) {
					// POS bay or platform hold: bind by itemID, as loot does.
					bindNum = eid.Value
				} else if isDockedHangarPlatform(sqldb, ds, charID, int64(eid.Value)) {
					// Docked platform password dialogs: bind by itemID.
					bindNum = eid.Value
				} else if isOfficeContainerID(sqldb, int64(eid.Value)) {
					// Office folder or rented office: bind by itemID.
					bindNum = eid.Value
				} else if isOwnedContainerBind(sqldb, charID, int64(eid.Value)) {
					// The pilot's own assembled container (alsc.go).
					bindNum = eid.Value
				} else if ds != nil && ds.solarSystem != nil {
					if _, isLoot := ds.solarSystem.GetLootContainerPosition(uint32(eid.Value)); isLoot {
						bindNum = eid.Value
					} else if bindNPCCargo(ds, sqldb, int64(eid.Value)) {
						bindNum = eid.Value
					}
				}
			}
		}
		bindSpec := fmt.Sprintf("N=%d:%d", globalNodeID, bindNum)
		boundObj := marshal.PySubStruct{Value: &marshal.PySubStream{
			Decoded: marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyString{Value: bindSpec},
				marshal.PyInt{Value: 0},
			}},
		}}
		log.Printf("[%s] → invbroker.GetInventoryFromId → bind %q", addr, bindSpec)
		return server.SendCallRsp(c, pkt, accountID, boundObj)

	case "AssembleCargoContainer":
		// inventorymgr.AssembleCargoContainer(itemID, None, 0.0): "Assemble Container" on a
		// packaged can, secure can or ALSC (alsc.go).
		return handleAssembleContainer(c, addr, cs, pkt, accountID, charID, sqldb)

	case "AssembleConstructionPlatform":
		// inventorymgr.AssembleConstructionPlatform(itemID, password, ?, 0.0): four args where
		// the cargo-container sibling takes three. Only arg 0 is read.
		if len(cs.ArgTuple.Items) >= 1 && sqldb != nil && charID != 0 {
			if id, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
				actx, acancel := context.WithTimeout(context.Background(), 5*time.Second)
				err := assembleConstructionPlatform(actx, sqldb, charID, int64(id.Value))
				acancel()
				if err != nil {
					log.Printf("[%s] invbroker.AssembleConstructionPlatform: %v", addr, err)
					return server.SendCallException(c, pkt, accountID, server.BuildUserError(
						"CustomError", map[string]marshal.PyRep{
							"error": marshal.PyString{Value: err.Error()},
						}))
				}
				log.Printf("[%s] → invbroker.AssembleConstructionPlatform item=%d", addr, id.Value)
			}
		}
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})

	default:
		log.Printf("[%s] → invbroker.%s (None)", addr, cs.Method)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
}

// handleAssetSvc handles CallReq packets addressed to the "assetSvc" service.
// GetLocations returns a PyDict keyed by stationID whose values are util.Rowsets
// of the items the character owns at that location (flag 4, hangar).
func handleAssetSvc(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, charID int64, sqldb *sql.DB) error {
	switch cs.Method {
	case "GetLocations":
		var result marshal.PyRep = marshal.PyDict{}
		if charID != 0 && sqldb != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if r, err := evedb.GetCharAssets(ctx, sqldb, charID); err != nil {
				log.Printf("[%s] assetSvc.GetLocations db: %v", addr, err)
			} else {
				result = r
			}
		}
		log.Printf("[%s] → assetSvc.GetLocations charID=%d", addr, charID)
		return server.SendCallRsp(c, pkt, accountID, result)
	default:
		log.Printf("[%s] → assetSvc.%s (None)", addr, cs.Method)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
}

// handleCorpRegistry handles CallReq packets addressed to the "corpRegistry" service.
func handleCorpRegistry(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64) error {
	switch cs.Method {
	case "GetShares":
		// wallet.py ShowVouchers turns a falsy result into [] (`or []`) and then calls
		// .itervalues(), so the dict carries one key; its empty list value iterates zero times.
		log.Printf("[%s] → corpRegistry.GetShares (empty truthy dict)", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyDict{
			Items: []marshal.DictEntry{
				{Key: marshal.PyInt{Value: 0}, Value: marshal.PyList{}},
			},
		})
	default:
		log.Printf("[%s] → corpRegistry.%s (None)", addr, cs.Method)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
}

// handleIndustryMgr handles CallReq packets addressed to the "industryMgr" service.
func handleIndustryMgr(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID int64, sqldb *sql.DB) error {
	switch cs.Method {
	case "GetJobsByOwner":
		// Args: ownerID, completed (bool)
		ownerID := charID
		completed := false
		if len(cs.ArgTuple.Items) >= 1 {
			if v, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
				ownerID = int64(v.Value)
			}
		}
		if len(cs.ArgTuple.Items) >= 2 {
			if b, ok := cs.ArgTuple.Items[1].(marshal.PyBool); ok {
				completed = b.Value
			} else if v, ok := cs.ArgTuple.Items[1].(marshal.PyInt); ok {
				completed = v.Value != 0
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		result, err := evedb.GetJobs(ctx, sqldb, ownerID, completed, 0, 0)
		if err != nil {
			log.Printf("[%s] industryMgr.GetJobsByOwner: %v", addr, err)
		}
		log.Printf("[%s] → industryMgr.GetJobsByOwner ownerID=%d completed=%v", addr, ownerID, completed)
		return server.SendCallRsp(c, pkt, accountID, result)
	default:
		log.Printf("[%s] → industryMgr.%s (empty list)", addr, cs.Method)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyList{})
	}
}

// handleFactory handles CallReq packets addressed to the "factory" service.
// GetMaterialsForTypeWithActivity is called by blueprint info cards to show
// required materials for manufacturing/research activities.
func handleFactory(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID int64, sqldb *sql.DB) error {
	switch cs.Method {
	case "GetMaterialsForTypeWithActivity":
		if len(cs.ArgTuple.Items) < 2 {
			log.Printf("[%s] factory.GetMaterialsForTypeWithActivity: missing args", addr)
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		typeIDRep, ok1 := cs.ArgTuple.Items[0].(marshal.PyInt)
		activityIDRep, ok2 := cs.ArgTuple.Items[1].(marshal.PyInt)
		if !ok1 || !ok2 {
			log.Printf("[%s] factory.GetMaterialsForTypeWithActivity: bad arg types", addr)
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		typeID := int64(typeIDRep.Value)
		activityID := int64(activityIDRep.Value)
		log.Printf("[%s] → factory.GetMaterialsForTypeWithActivity typeID=%d activityID=%d", addr, typeID, activityID)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		result, err := evedb.GetMaterialsForTypeWithActivity(ctx, sqldb, typeID, activityID)
		if err != nil {
			log.Printf("[%s] factory.GetMaterialsForTypeWithActivity: %v", addr, err)
		}
		return server.SendCallRsp(c, pkt, accountID, result)

	case "GetBlueprintAttributes":
		if len(cs.ArgTuple.Items) < 1 {
			log.Printf("[%s] factory.GetBlueprintAttributes: missing args", addr)
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		bpIDRep, ok := cs.ArgTuple.Items[0].(marshal.PyInt)
		if !ok {
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		blueprintID := int64(bpIDRep.Value)
		log.Printf("[%s] → factory.GetBlueprintAttributes blueprintID=%d", addr, blueprintID)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		result, err := evedb.GetBlueprintAttributesRep(ctx, sqldb, blueprintID)
		if err != nil {
			log.Printf("[%s] factory.GetBlueprintAttributes: %v", addr, err)
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		return server.SendCallRsp(c, pkt, accountID, result)

	case "GetMaterialCompositionOfItemType":
		// Blueprint info window: base materials of the product type.
		if len(cs.ArgTuple.Items) < 1 {
			return server.SendCallRsp(c, pkt, accountID, marshal.PyList{})
		}
		typeIDRep, ok := cs.ArgTuple.Items[0].(marshal.PyInt)
		if !ok {
			return server.SendCallRsp(c, pkt, accountID, marshal.PyList{})
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		result, err := evedb.GetMaterialCompositionOfItemType(ctx, sqldb, int64(typeIDRep.Value))
		if err != nil {
			log.Printf("[%s] factory.GetMaterialCompositionOfItemType: %v", addr, err)
		}
		return server.SendCallRsp(c, pkt, accountID, result)

	case "GetBlueprintInformation":
		// Legacy wizard: reads .copy / .licensedProductionRunsRemaining off a Row.
		if len(cs.ArgTuple.Items) < 1 {
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		bpIDRep, ok := cs.ArgTuple.Items[0].(marshal.PyInt)
		if !ok {
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		result, err := evedb.GetBlueprintInformationRow(ctx, sqldb, int64(bpIDRep.Value))
		if err != nil {
			log.Printf("[%s] factory.GetBlueprintInformation: %v", addr, err)
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		return server.SendCallRsp(c, pkt, accountID, result)

	case "GetBlueprintInformationAtLocationWithFlag":
		// Legacy panels and the corp vote UI: blueprints at a location and flag, owned by
		// the character or, with forCorp, by the corporation.
		if len(cs.ArgTuple.Items) < 2 {
			return server.SendCallRsp(c, pkt, accountID, marshal.PyList{})
		}
		locRep, ok1 := cs.ArgTuple.Items[0].(marshal.PyInt)
		flagRep, ok2 := cs.ArgTuple.Items[1].(marshal.PyInt)
		if !ok1 || !ok2 {
			return server.SendCallRsp(c, pkt, accountID, marshal.PyList{})
		}
		forCorp := false
		if len(cs.ArgTuple.Items) >= 3 {
			switch v := cs.ArgTuple.Items[2].(type) {
			case marshal.PyBool:
				forCorp = v.Value
			case marshal.PyInt:
				forCorp = v.Value != 0
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		result, err := evedb.GetBlueprintInfoAtLocationWithFlag(ctx, sqldb, charID,
			int64(locRep.Value), int64(flagRep.Value), forCorp)
		if err != nil {
			log.Printf("[%s] factory.GetBlueprintInformationAtLocationWithFlag: %v", addr, err)
		}
		return server.SendCallRsp(c, pkt, accountID, result)

	// The legacy slot methods (InstallItemInSlot, GetFactorySlot, GetFactoryStatus, ...)
	// are routed to researchslots.go before this handler.

	default:
		// The slot-based Factory and Research Facility panels call GetFactorySlots and
		// GetResearchFacilitySlots and take len() of the answer, so None crashes them.
		server.RecordUnhandled(addr, "factory", cs, true)
		log.Printf("[%s] factory.%s: unhandled method — returning empty list", addr, cs.Method)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyList{})
	}
}

// handleAllianceRegistry handles calls to the "allianceRegistry" service.
// GetAlliance returns a util.Row from alliance_shortnames; GetAllianceMembers
// returns an empty rowset; all other methods return None.
func handleAllianceRegistry(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	switch cs.Method {
	case "GetAlliance":
		if len(cs.ArgTuple.Items) < 1 {
			log.Printf("[%s] allianceRegistry.GetAlliance: missing allianceID arg", addr)
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		allianceIDRep, ok := cs.ArgTuple.Items[0].(marshal.PyInt)
		if !ok {
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		allianceID := int64(allianceIDRep.Value)
		result, err := evedb.GetAllianceInfo(ctx, sqldb, allianceID)
		if err != nil {
			log.Printf("[%s] allianceRegistry.GetAlliance allianceID=%d: %v", addr, allianceID, err)
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		log.Printf("[%s] → allianceRegistry.GetAlliance allianceID=%d", addr, allianceID)
		return server.SendCallRsp(c, pkt, accountID, result)

	case "GetAllianceMembers":
		cols := []string{"corporationID", "startDate"}
		log.Printf("[%s] → allianceRegistry.GetAllianceMembers (empty rowset)", addr)
		return server.SendCallRsp(c, pkt, accountID, makeInvRowset(cols, nil))

	case "GetAlliances":
		cols := []string{"allianceID", "shortName"}
		rows, err := sqldb.QueryContext(ctx, `SELECT allianceID, shortName FROM alliance_shortnames`)
		if err != nil {
			log.Printf("[%s] allianceRegistry.GetAlliances db: %v", addr, err)
			return server.SendCallRsp(c, pkt, accountID, makeInvRowset(cols, nil))
		}
		defer rows.Close()
		var result [][]marshal.PyRep
		for rows.Next() {
			var allianceID int64
			var shortName string
			if err := rows.Scan(&allianceID, &shortName); err != nil {
				continue
			}
			result = append(result, []marshal.PyRep{
				marshal.PyInt{Value: uint64(allianceID)},
				marshal.PyString{Value: shortName},
			})
		}
		log.Printf("[%s] → allianceRegistry.GetAlliances (%d rows)", addr, len(result))
		return server.SendCallRsp(c, pkt, accountID, makeInvRowset(cols, result))

	case "GetRankedAlliances":
		// all_ui_rankings.ShowRankings iterates the result, so None fails. Columns match the
		// UI headers; rows are ordered by memberCount DESC.
		cols := []string{"allianceID", "allianceName", "executorCorpID", "shortName",
			"startDate", "memberCount", "dictatorial"}
		rows, err := sqldb.QueryContext(ctx, `
			SELECT allianceID, allianceName, executorCorpID, shortName,
			       startDate, memberCount, dictatorial
			  FROM alliances
			 WHERE deleted = 0
			 ORDER BY memberCount DESC, allianceID ASC`)
		if err != nil {
			log.Printf("[%s] allianceRegistry.GetRankedAlliances db: %v", addr, err)
			return server.SendCallRsp(c, pkt, accountID, makeInvRowset(cols, nil))
		}
		defer rows.Close()
		var result [][]marshal.PyRep
		for rows.Next() {
			var (
				allianceID, executorCorpID, startDate, memberCount, dictatorial int64
				allianceName, shortName                                         string
			)
			if err := rows.Scan(&allianceID, &allianceName, &executorCorpID,
				&shortName, &startDate, &memberCount, &dictatorial); err != nil {
				continue
			}
			result = append(result, []marshal.PyRep{
				marshal.PyInt{Value: uint64(allianceID)},
				marshal.PyString{Value: allianceName},
				marshal.PyInt{Value: uint64(executorCorpID)},
				marshal.PyString{Value: shortName},
				marshal.PyInt{Value: uint64(startDate)},
				marshal.PyInt{Value: uint64(memberCount)},
				marshal.PyInt{Value: uint64(dictatorial)},
			})
		}
		log.Printf("[%s] → allianceRegistry.GetRankedAlliances (%d rows)", addr, len(result))
		return server.SendCallRsp(c, pkt, accountID, makeInvRowset(cols, result))

	default:
		log.Printf("[%s] → allianceRegistry.%s (None)", addr, cs.Method)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
}

// handleAccount handles CallReq packets addressed to the "account" service.
func handleAccount(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, charID int64, sqldb *sql.DB, sessReg *esp.SessionRegistry) error {
	switch cs.Method {
	case "GiveCash", "GiveCashFromCorpAccount":
		// wallet.py GiveMoney / GiveCorpMoney: (ownerID, amount, reason). The client refreshes
		// balances from OnAccountChange pushes, so both wallets are notified.
		return handleGiveCash(c, addr, cs, pkt, accountID, charID, sqldb, sessReg,
			cs.Method == "GiveCashFromCorpAccount")
	case "GetCashBalance":
		// Arg[0]: 0 = personal wallet, 1 = corp wallet.
		corpWallet := false
		if len(cs.ArgTuple.Items) > 0 {
			if v, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok && v.Value != 0 {
				corpWallet = true
			} else if b, ok := cs.ArgTuple.Items[0].(marshal.PyBool); ok && b.Value {
				corpWallet = true
			}
		}
		var balance float64
		if charID != 0 && sqldb != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var err error
			if corpWallet {
				if corpID, cerr := evedb.GetCorporationIDForCharacter(ctx, sqldb, charID); cerr == nil {
					balance, err = evedb.GetCorpBalance(ctx, sqldb, corpID)
				} else {
					err = cerr
				}
			} else {
				balance, err = evedb.GetCharacterBalance(ctx, sqldb, charID)
			}
			if err != nil {
				log.Printf("[%s] account.GetCashBalance db: %v (returning 0)", addr, err)
			}
		}
		log.Printf("[%s] → account.GetCashBalance charID=%d corp=%v balance=%.2f", addr, charID, corpWallet, balance)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyReal{Value: balance})
	case "GetJournal":
		// Args: (accountKey, fromDate, refTypeID, corpAccount, refID, rev)
		// per client accountsvc.py. corpAccount=1 → corp wallet journal.
		corpAccount := false
		if len(cs.ArgTuple.Items) > 3 {
			if v, ok := cs.ArgTuple.Items[3].(marshal.PyInt); ok && v.Value != 0 {
				corpAccount = true
			} else if b, ok := cs.ArgTuple.Items[3].(marshal.PyBool); ok && b.Value {
				corpAccount = true
			}
		}
		var result marshal.PyRep = makeInvRowset([]string{
			"refID", "transDate", "refTypeID", "ownerID1", "ownerID2",
			"argID1", "accountKey", "amount", "balance", "reason",
		}, nil)
		if charID != 0 && sqldb != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			journalOwner := charID
			if corpAccount {
				if corpID, err := evedb.GetCorporationIDForCharacter(ctx, sqldb, charID); err == nil {
					journalOwner = corpID
				} else {
					log.Printf("[%s] account.GetJournal corp lookup: %v", addr, err)
				}
			}
			if r, err := evedb.GetCharJournal(ctx, sqldb, journalOwner); err != nil {
				log.Printf("[%s] account.GetJournal db: %v", addr, err)
			} else {
				result = r
			}
		}
		log.Printf("[%s] → account.GetJournal charID=%d corp=%v", addr, charID, corpAccount)
		return server.SendCallRsp(c, pkt, accountID, result)
	case "GetTransactions":
		// wallet.py:ShowTransactions iterates transactions; return rowset from market_transactions.
		var result marshal.PyRep = makeInvRowset([]string{
			"transactionID", "transactionDateTime", "typeID", "quantity",
			"price", "transactionType", "clientID", "regionID", "stationID",
		}, nil)
		if charID != 0 && sqldb != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if r, err := evedb.GetCharTransactions(ctx, sqldb, charID); err != nil {
				log.Printf("[%s] account.GetTransactions db: %v", addr, err)
			} else {
				result = r
			}
		}
		log.Printf("[%s] → account.GetTransactions charID=%d", addr, charID)
		return server.SendCallRsp(c, pkt, accountID, result)
	case "GetRefTypes":
		// Served from market_reftypes: the client journal drops every row whose refTypeID is
		// missing from this list (accountsvc.py). See docs/corp-notes.md#bills.
		log.Printf("[%s] → account.GetRefTypes (from market_reftypes)", addr)
		if sqldb != nil {
			ctxR, cancelR := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancelR()
			if rowset, err := evedb.GetRefTypesRowset(ctxR, sqldb); err == nil {
				return server.SendCallRsp(c, pkt, accountID, rowset)
			}
		}
		return server.SendCallRsp(c, pkt, accountID, makeInvRowset(
			[]string{"refTypeID", "refTypeText", "description"}, nil))
	case "GetKeyMap":
		log.Printf("[%s] → account.GetKeyMap", addr)
		keyMap := [][]marshal.PyRep{
			{marshal.PyInt{Value: 1000}, marshal.PyString{Value: "cash"}, marshal.PyString{Value: "Cash"}, marshal.PyString{Value: "Cash Account"}},
		}
		return server.SendCallRsp(c, pkt, accountID, makeInvRowset(
			[]string{"accountKey", "accountType", "accountName", "description"}, keyMap))
	default:
		log.Printf("[%s] → account.%s (None)", addr, cs.Method)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
}

// handleGiveCash implements account.GiveCash / GiveCashFromCorpAccount:
// (ownerID, amount, reason).  Moves ISK from the caller's personal (or corp)
// wallet to another character or corporation, writes both journal sides and
// pushes OnAccountChange to every affected online wallet.
func handleGiveCash(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID int64, sqldb *sql.DB, sessReg *esp.SessionRegistry, fromCorp bool) error {
	userErr := func(msg string) error {
		return server.SendCallException(c, pkt, accountID, server.BuildUserError(
			"CustomError", map[string]marshal.PyRep{"error": marshal.PyString{Value: msg}}))
	}
	if charID == 0 || sqldb == nil {
		return userErr("No character selected.")
	}
	if len(cs.ArgTuple.Items) < 2 {
		return userErr("Missing transfer arguments.")
	}
	targetRep, ok := cs.ArgTuple.Items[0].(marshal.PyInt)
	if !ok {
		return userErr("Invalid recipient.")
	}
	targetID := int64(targetRep.Value)
	var amount float64
	switch v := cs.ArgTuple.Items[1].(type) {
	case marshal.PyReal:
		amount = v.Value
	case marshal.PyInt:
		amount = float64(int64(v.Value))
	}
	var reason string
	if len(cs.ArgTuple.Items) >= 3 {
		if s, ok := cs.ArgTuple.Items[2].(marshal.PyString); ok {
			reason = s.Value
		}
	}
	if amount <= 0 {
		return userErr("The amount must be positive.")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Resolve the source wallet.
	srcOwnerID := charID
	refType := 10 // Player Donation
	if fromCorp {
		corpID, err := evedb.GetCharCorpID(ctx, sqldb, charID)
		if err != nil || corpID == 0 {
			return userErr("You are not in a corporation.")
		}
		srcOwnerID = corpID
		refType = 37 // Corporation Account Withdrawal
	}
	if targetID == srcOwnerID {
		return userErr("You cannot transfer money to the same wallet.")
	}

	// Classify the recipient: character first, then corporation.
	var targetIsChar bool
	var n int
	if err := sqldb.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM character_ WHERE characterID = ?`, targetID).Scan(&n); err == nil && n > 0 {
		targetIsChar = true
	} else if err := sqldb.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM corporation WHERE corporationID = ?`, targetID).Scan(&n); err != nil || n == 0 {
		return userErr("No such character or corporation.")
	}

	// Move the money: one transaction, both journals, and a debit that
	// refuses to take the wallet negative (evedb.TransferISK).
	srcBal, dstBal, err := evedb.TransferISK(ctx, sqldb, srcOwnerID, targetID, amount, refType, reason)
	if errors.Is(err, evedb.ErrInsufficientFunds) {
		return userErr("You do not have that much money.")
	}
	if err != nil {
		log.Printf("[%s] account.GiveCash transfer: %v", addr, err)
		return userErr("Transfer failed.")
	}

	// Refresh the sender's wallet display (personal or corp view).
	_ = server.SendNotification(c, accountID, "OnAccountChange", "clientID", marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyString{Value: "cash"},
		marshal.PyInt{Value: uint64(srcOwnerID)},
		marshal.PyReal{Value: srcBal},
	}})
	// And the recipient's, if a character is online.
	if targetIsChar && sessReg != nil {
		server.NotifyCharacter(sessReg, targetID, "OnAccountChange", "clientID", marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyString{Value: "cash"},
			marshal.PyInt{Value: uint64(targetID)},
			marshal.PyReal{Value: dstBal},
		}})
	}
	log.Printf("[%s] → account.%s src=%d dst=%d amount=%.2f (srcBal=%.2f)", addr, cs.Method, srcOwnerID, targetID, amount, srcBal)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

func handleOnlineStatus(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID int64, lscReg *lsc.Registry, sqldb *sql.DB) error {
	switch cs.Method {
	case "GetOnlineStatus":
		// Client onlinestatus.py falls back to this per-character call when a buddy is missing
		// from the GetInitialState cache. Presence, not connectedness: see charIsOnline.
		online := false
		if len(cs.ArgTuple.Items) >= 1 {
			if v, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
				online = charIsOnline(int64(v.Value))
			}
		}
		log.Printf("[%s] → onlineStatus.GetOnlineStatus → %v", addr, online)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyBool{Value: online})
	case "GetInitialState":
		// Rowset of the caller's buddies with their online flag. The client primes its cache
		// from this and afterwards relies on OnContactLoggedOn/Off pushes.
		var lines []marshal.PyRep
		if sqldb != nil && charID != 0 {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			buddies, err := evedb.GetBuddyList(ctx, sqldb, charID)
			cancel()
			if err != nil {
				log.Printf("[%s] onlineStatus.GetInitialState: %v", addr, err)
			}
			for _, buddyID := range buddies {
				lines = append(lines, marshal.PyList{Items: []marshal.PyRep{
					marshal.PyInt{Value: uint64(buddyID)},
					marshal.PyBool{Value: charIsOnline(buddyID)},
				}})
			}
		}
		log.Printf("[%s] → onlineStatus.GetInitialState (%d buddies)", addr, len(lines))
		onlineRowset := marshal.PyObject{
			Type: "util.Rowset",
			Args: marshal.PyDict{Items: []marshal.DictEntry{
				{Key: marshal.PyString{Value: "header"}, Value: marshal.PyList{Items: []marshal.PyRep{
					marshal.PyString{Value: "characterID"},
					marshal.PyString{Value: "online"},
				}}},
				{Key: marshal.PyString{Value: "RowClass"}, Value: marshal.PyString{Value: "util.Row", IsType1: true}},
				{Key: marshal.PyString{Value: "lines"}, Value: marshal.PyList{Items: lines}},
			}},
		}
		return server.SendCallRsp(c, pkt, accountID, onlineRowset)
	default:
		log.Printf("[%s] → onlineStatus.%s (None)", addr, cs.Method)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
}

// broadcastOnlineStatus pushes OnContactLoggedOn/Off for charID to every online
// character that has charID on their buddy list. It queries the DB: run it in a goroutine.
func broadcastOnlineStatus(sqldb *sql.DB, charID int64, online bool) {
	defer safego.Recover("broadcastOnlineStatus")
	if sqldb == nil || charID == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	watchers, err := evedb.GetBuddyWatchers(ctx, sqldb, charID)
	if err != nil {
		log.Printf("broadcastOnlineStatus charID=%d: %v", charID, err)
		return
	}
	event := "OnContactLoggedOff"
	if online {
		event = "OnContactLoggedOn"
	}
	args := marshal.PyTuple{Items: []marshal.PyRep{marshal.PyInt{Value: uint64(charID)}}}
	for _, watcherID := range watchers {
		if conn, acct := charConnLookup(watcherID); conn != nil {
			_ = server.SendNotification(conn, acct, event, "charid", args)
		}
	}
}

// handleSkillMgr handles CallReq packets addressed to the "skillMgr" service. Training
// start and stop go through evedb and push the OnSkill* events.
// See docs/character-skills.md#skill-training.
func handleSkillMgr(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, charID int64, stationID int64, sqldb *sql.DB) error {
	switch cs.Method {
	case "CharAddImplant", "RemoveImplantFromCharacter":
		if len(cs.ArgTuple.Items) < 1 {
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		itemIDRep, ok := cs.ArgTuple.Items[0].(marshal.PyInt)
		if !ok {
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		var errMsg string
		if cs.Method == "CharAddImplant" {
			errMsg = doCharAddImplant(c, addr, accountID, charID, stationID, int64(itemIDRep.Value), sqldb)
		} else {
			errMsg = doRemoveImplant(c, addr, accountID, charID, int64(itemIDRep.Value), sqldb)
		}
		if errMsg != "" {
			return server.SendCallException(c, pkt, accountID, server.BuildUserError(
				"CustomInfo", map[string]marshal.PyRep{
					"info": marshal.PyString{Value: errMsg},
				}))
		}
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	case "GetSkillQueue":
		log.Printf("[%s] → skillMgr.GetSkillQueue (empty list)", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyList{})
	case "GetSkillsAndProgress":
		// The reply is a 2-tuple: (rowset of skills, tuple of queue entries). Real skills are
		// returned so the OnSkillStartTraining itemIDs resolve in the client.
		skillCols := []string{"typeID", "skillPoints", "flag", "level"}
		header := make([]marshal.PyRep, len(skillCols))
		for i, col := range skillCols {
			header[i] = marshal.PyString{Value: col}
		}
		var lines []marshal.PyRep
		if charID != 0 && sqldb != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			// Finish any completed training and chain through the queue first; the client is
			// notified so "Skill Trained" shows even when the completion timer did not fire.
			if adv, cerr := evedb.AdvanceSkillQueue(ctx, sqldb, charID); cerr != nil {
				log.Printf("[%s] skillMgr.GetSkillsAndProgress AdvanceSkillQueue: %v", addr, cerr)
			} else if adv != nil {
				notifySkillQueueAdvance(c, accountID, charID, adv, sqldb)
			}
			// (Re)arm the completion timer for whatever is still training.
			if st, terr := evedb.GetSkillTraining(ctx, sqldb, charID); terr == nil && st != nil {
				armSkillCompletionTimer(c, accountID, charID, st.EndOfTraining, sqldb)
			}
			// Reset flag 61 entries with no chrSkillTraining row; the client renders them as
			// "in training, completion imminent".
			if n, oerr := evedb.ClearOrphanTrainingFlags(ctx, sqldb, charID); oerr != nil {
				log.Printf("[%s] skillMgr.GetSkillsAndProgress ClearOrphanTrainingFlags: %v", addr, oerr)
			} else if n > 0 {
				log.Printf("[%s] skillMgr.GetSkillsAndProgress cleared %d orphan training flags", addr, n)
			}
			skills, err := evedb.GetCharacterSkillItems(ctx, sqldb, charID)
			if err != nil {
				log.Printf("[%s] skillMgr.GetSkillsAndProgress db: %v (returning empty)", addr, err)
			} else {
				for _, sk := range skills {
					lines = append(lines, marshal.PyList{Items: []marshal.PyRep{
						marshal.PyInt{Value: uint64(sk.TypeID)},
						marshal.PyInt{Value: uint64(sk.SkillPoints)},
						marshal.PyInt{Value: uint64(sk.Flag)}, // flagSkill(7) or flagSkillInTraining(61)
						marshal.PyInt{Value: uint64(sk.Level)},
					}})
				}
			}
		}
		skillsRowset := marshal.PyObject{
			Type: "util.Rowset",
			Args: marshal.PyDict{Items: []marshal.DictEntry{
				{Key: marshal.PyString{Value: "header"}, Value: marshal.PyList{Items: header}},
				{Key: marshal.PyString{Value: "RowClass"}, Value: marshal.PyString{Value: "util.Row", IsType1: true}},
				{Key: marshal.PyString{Value: "lines"}, Value: marshal.PyList{Items: lines}},
			}},
		}
		log.Printf("[%s] → skillMgr.GetSkillsAndProgress charID=%d skills=%d", addr, charID, len(lines))
		return server.SendCallRsp(c, pkt, accountID, marshal.PyTuple{Items: []marshal.PyRep{
			skillsRowset,
			marshal.PyTuple{}, // Blood era has no skill queue
		}})
	case "CharStartTrainingSkill":
		// CharStartTrainingSkill(skillItemID) is the "Train Skill" menu entry. The flag is
		// updated in the DB and OnSkillStartTraining tells the client.
		if len(cs.ArgTuple.Items) < 1 {
			log.Printf("[%s] skillMgr.CharStartTrainingSkill: missing args", addr)
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		skillIDRep, ok := cs.ArgTuple.Items[0].(marshal.PyInt)
		if !ok {
			log.Printf("[%s] skillMgr.CharStartTrainingSkill: arg[0] not int: %T", addr, cs.ArgTuple.Items[0])
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		skillItemID := int64(skillIDRep.Value)
		log.Printf("[%s] → skillMgr.CharStartTrainingSkill charID=%d skillItemID=%d", addr, charID, skillItemID)
		oldLoc, oldFlag := snapshotSkillItemState(sqldb, skillItemID)

		var endOfTraining uint64
		if sqldb != nil && charID != 0 {
			prevItemID := trainingSkillItem(sqldb, charID)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var err error
			endOfTraining, err = evedb.StartTrainingSkill(ctx, sqldb, charID, skillItemID)
			if err != nil {
				log.Printf("[%s] skillMgr.CharStartTrainingSkill refused: %v", addr, err)
				return server.SendCallException(c, pkt, accountID, skillTrainErrorPayload(err))
			}
			notifySkillSwitchedOff(c, accountID, prevItemID, skillItemID, sqldb)
		} else {
			endOfTraining = server.Win32TimeNow() + 25920000000000
		}
		notifyArgs := marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(skillItemID)},
			marshal.PyInt{Value: endOfTraining},
		}}
		if err := server.SendNotification(c, accountID, "OnSkillStartTraining", "charid", notifyArgs); err != nil {
			log.Printf("[%s] skillMgr.CharStartTrainingSkill notify: %v", addr, err)
		}
		armSkillCompletionTimer(c, accountID, charID, endOfTraining, sqldb)
		sendSkillFlagChange(c, accountID, skillItemID, oldFlag, oldLoc, sqldb)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyInt{Value: endOfTraining})
	case "CharStopTrainingSkill":
		// Client "Abort Training" menu. Banks partial SP, resets the skill flag,
		// and notifies the client to clear the training state.
		var stoppedItemID int64
		if sqldb != nil && charID != 0 {
			skReq := int64(0)
			if len(cs.ArgTuple.Items) >= 1 {
				if r, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
					skReq = int64(r.Value)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			var err error
			stoppedItemID, err = evedb.StopTrainingSkill(ctx, sqldb, charID, skReq)
			cancel()
			if err != nil {
				log.Printf("[%s] skillMgr.CharStopTrainingSkill db: %v", addr, err)
			}
		}
		cancelSkillCompletionTimer(c, charID)
		if stoppedItemID != 0 {
			notifyArgs := marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyInt{Value: uint64(stoppedItemID)},
				marshal.PyInt{Value: 0}, // silent=0 → show "training stopped"
			}}
			if err := server.SendNotification(c, accountID, "OnSkillTrainingStopped", "charid", notifyArgs); err != nil {
				log.Printf("[%s] skillMgr.CharStopTrainingSkill notify: %v", addr, err)
			}
			log.Printf("[%s] → skillMgr.CharStopTrainingSkill charID=%d skill %d stopped", addr, charID, stoppedItemID)
			sendSkillFlagChange(c, accountID, stoppedItemID, 61, 0, sqldb)
		}
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	case "GetEndOfTraining":
		// Returns the Win32 timestamp when the currently training skill completes.
		var endOfTraining uint64
		if sqldb != nil && charID != 0 {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if st, err := evedb.GetSkillTraining(ctx, sqldb, charID); err == nil && st != nil {
				endOfTraining = st.EndOfTraining
			}
		}
		if endOfTraining == 0 {
			endOfTraining = server.Win32TimeNow() + 25920000000000
		} else {
			armSkillCompletionTimer(c, accountID, charID, endOfTraining, sqldb)
		}
		log.Printf("[%s] → skillMgr.GetEndOfTraining endOfTraining=%d", addr, endOfTraining)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyInt{Value: endOfTraining})
	default:
		log.Printf("[%s] → skillMgr.%s (None)", addr, cs.Method)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
}

// handleCorporationSvc handles CallReq packets addressed to the "corporationSvc" service. GetFactionInfo mirrors Handle_GetFactionInfo, an 8-tuple of dicts:
//
//	[0] factionIDbyNPCCorpID: corpID→factionID
//	[1] factionRegions:       factionID→[regionID,...]
//	[2] factionConstellations: factionID→[constellationID,...]
//	[3] factionSolarSystems:  factionID→[solarSystemID,...]
//	[4] factionRaces:         factionID→[raceID,...]
//	[5] factionStationCount:  factionID→count
//	[6] factionSolarSystemCount: factionID→count
//	[7] npcCorpInfo:          corpID→util.Row
func handleCorporationSvc(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID int64, sqldb *sql.DB) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	switch cs.Method {
	case "GetFactionInfo":
		// factionsvc.py caches this for the session; it drives faction logos, the Size/Extent
		// rows, the faction info window tabs and each agent's factionID.
		result, err := evedb.GetFactionInfo(ctx, sqldb)
		if err != nil {
			log.Printf("[%s] corporationSvc.GetFactionInfo db: %v", addr, err)
			// An exception here would break every info window for the session,
			// so degrade to the empty 8-tuple the client can still cope with.
			return server.SendCallRsp(c, pkt, accountID, marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyDict{}, marshal.PyDict{}, marshal.PyDict{}, marshal.PyDict{},
				marshal.PyDict{}, marshal.PyDict{}, marshal.PyDict{}, marshal.PyDict{},
			}})
		}
		log.Printf("[%s] → corporationSvc.GetFactionInfo", addr)
		return server.SendCallRsp(c, pkt, accountID, result)
	case "GetEmploymentRecord":
		// Arg[0] is the characterID to look up.  Fall back to the session charID
		// if no arg is provided.
		targetCharID := charID
		if len(cs.ArgTuple.Items) >= 1 {
			if id, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
				targetCharID = int64(id.Value)
			}
		}
		// Memoized on the long TTL: an employment history is append-only and a
		// grid full of pilots asks for one per ball. See package infocache.
		result, err := infocache.Shared.Get("corporationSvc.GetEmploymentRecord", targetCharID, infocache.TTLStatic,
			func() (marshal.PyRep, error) {
				return evedb.GetEmploymentRecord(ctx, sqldb, targetCharID)
			})
		if err != nil {
			log.Printf("[%s] corporationSvc.GetEmploymentRecord db: %v", addr, err)
			return server.SendCallException(c, pkt, accountID, nil)
		}
		logx.Debugf("[%s] → corporationSvc.GetEmploymentRecord charID=%d", addr, targetCharID)
		return server.SendCallRsp(c, pkt, accountID, result)
	case "GetCorpInfo":
		// infosvc.py GetWndData iterates the result as NPC market items (typeID, sellPrice, ...).
		// None and a corp Row both raise there, so the shape must be this rowset.
		var corpID int64
		if len(cs.ArgTuple.Items) >= 1 {
			if v, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
				corpID = int64(v.Value)
			}
		}
		result, err := evedb.GetCorpMarketInfo(ctx, sqldb, corpID)
		if err != nil {
			log.Printf("[%s] corporationSvc.GetCorpInfo corpID=%d db: %v", addr, corpID, err)
			return server.SendCallRsp(c, pkt, accountID, makeInvRowset(
				[]string{"typeID", "sellPrice", "buyPrice"}, nil))
		}
		log.Printf("[%s] → corporationSvc.GetCorpInfo corpID=%d", addr, corpID)
		return server.SendCallRsp(c, pkt, accountID, result)
	case "GetMembers":
		var corpID int64
		if len(cs.ArgTuple.Items) >= 1 {
			if v, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
				corpID = int64(v.Value)
			}
		}
		if corpID == 0 {
			log.Printf("[%s] corporationSvc.GetMembers: missing corpID arg", addr)
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		members, err := evedb.GetCorpMembers(ctx, sqldb, corpID)
		if err != nil {
			log.Printf("[%s] corporationSvc.GetMembers db: %v", addr, err)
			return server.SendCallException(c, pkt, accountID, nil)
		}
		log.Printf("[%s] → corporationSvc.GetMembers corpID=%d", addr, corpID)
		return server.SendCallRsp(c, pkt, accountID, members)
	case "GetNPCDivisions":
		// util.Rowset of NPC corp divisions (crpNPCDivisions); agents.py GetDivisions calls
		// .Index() on it, so None fails.
		divisions, err := evedb.GetNPCDivisions(ctx, sqldb)
		if err != nil {
			log.Printf("[%s] corporationSvc.GetNPCDivisions db: %v", addr, err)
			return server.SendCallException(c, pkt, accountID, nil)
		}
		log.Printf("[%s] → corporationSvc.GetNPCDivisions", addr)
		return server.SendCallRsp(c, pkt, accountID, divisions)
	default:
		log.Printf("[%s] → corporationSvc.%s (None)", addr, cs.Method)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
}

// handleTutorialSvc handles CallReq packets addressed to the "tutorialSvc" service.
// GetTutorials returns a util.Rowset of (tutorialID, tutorialVName, nextTutorialID) and
// GetCriterias one of (criteriaID, criteriaName, messageText, audioPath).
// See docs/main-s8-notes.md#tutorial-svc.
func handleTutorialSvc(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID int64, sqldb *sql.DB) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	switch cs.Method {
	case "GetTutorials":
		// The client indexes this rowset (tutorials[5]); an empty answer is a KeyError that
		// blocks station load. See docs/main-s8-notes.md#tutorial-svc.
		cols := []string{"tutorialID", "tutorialVName", "nextTutorialID"}
		result, err := evedb.QueryTutorials(ctx, sqldb, cols)
		if err != nil || isRowsetEmpty(result) {
			// Fallback: provide the tutorial ID the client expects.
			result = makeInvRowset(cols, [][]marshal.PyRep{{
				marshal.PyInt{Value: 5},
				marshal.PyString{Value: ""},
				marshal.PyNone{},
			}})
			log.Printf("[%s] → tutorialSvc.GetTutorials (stub tutorialID=5)", addr)
		} else {
			log.Printf("[%s] → tutorialSvc.GetTutorials (from DB)", addr)
		}
		return server.SendCallRsp(c, pkt, accountID, result)

	case "GetCriterias":
		cols := []string{"criteriaID", "criteriaName", "messageText", "audioPath"}
		rowset, err := evedb.QueryCriterias(ctx, sqldb, cols)
		if err != nil {
			log.Printf("[%s] tutorialSvc.GetCriterias: %v", addr, err)
			return server.SendCallRsp(c, pkt, accountID, makeInvRowset(cols, nil))
		}
		log.Printf("[%s] → tutorialSvc.GetCriterias", addr)
		return server.SendCallRsp(c, pkt, accountID, rowset)

	case "GetTutorialInfo":
		var tutorialID int64
		if len(cs.ArgTuple.Items) >= 1 {
			if v, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
				tutorialID = int64(v.Value)
			}
		}

		// Always serve tutorial data: None starves the client of pages and the popup never
		// ends. See docs/main-s8-notes.md#tutorial-svc.

		// Try real tutorial data from DB, fall back to stubs.
		tutorialRow, err := evedb.GetTutorialRow(ctx, sqldb, tutorialID)
		if err != nil || isRowsetEmpty(tutorialRow) {
			tutorialRow = makeInvRowset(
				[]string{"tutorialID", "tutorialVName", "nextTutorialID"},
				[][]marshal.PyRep{{
					marshal.PyInt{Value: uint64(tutorialID)},
					marshal.PyString{Value: ""},
					marshal.PyNone{},
				}},
			)
		}

		pageRow, err := evedb.GetTutorialPages(ctx, sqldb, tutorialID)
		if err != nil || isRowsetEmpty(pageRow) {
			pageRow = makeInvRowset(
				[]string{"pageID", "pageName", "text", "imagePath", "audioPath"},
				[][]marshal.PyRep{{
					marshal.PyInt{Value: 1},
					marshal.PyString{Value: ""},
					marshal.PyString{Value: " "},
					marshal.PyString{Value: ""},
					marshal.PyString{Value: ""},
				}},
			)
			log.Printf("[%s] → tutorialSvc.GetTutorialInfo tutorialID=%d (stub page)", addr, tutorialID)
		} else {
			log.Printf("[%s] → tutorialSvc.GetTutorialInfo tutorialID=%d (from DB)", addr, tutorialID)
		}

		criterias, _ := evedb.GetTutorialCriterias(ctx, sqldb, tutorialID)
		pageCriterias, _ := evedb.GetTutorialPageCriterias(ctx, sqldb, tutorialID)

		tutInfo := marshal.PyObject{
			Type: "util.KeyVal",
			Args: marshal.PyDict{Items: []marshal.DictEntry{
				{Key: marshal.PyString{Value: "tutorial"}, Value: tutorialRow},
				{Key: marshal.PyString{Value: "pages"}, Value: pageRow},
				{Key: marshal.PyString{Value: "criterias"}, Value: criterias},
				{Key: marshal.PyString{Value: "pagecriterias"}, Value: pageCriterias},
			}},
		}
		return server.SendCallRsp(c, pkt, accountID, tutInfo)

	case "GetTutorialVersionInfo":
		// (VID, languageID) answers the same util.KeyVal shape as GetTutorialInfo. None is
		// allowed, but a truthy answer needs one tutorial row and one page row.
		var tutorialID int64
		if len(cs.ArgTuple.Items) >= 1 {
			if v, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
				tutorialID = int64(v.Value)
			}
		}
		tutorialRow, terr := evedb.GetTutorialRow(ctx, sqldb, tutorialID)
		pageRow, perr := evedb.GetTutorialPages(ctx, sqldb, tutorialID)
		if terr != nil || perr != nil || isRowsetEmpty(tutorialRow) || isRowsetEmpty(pageRow) {
			log.Printf("[%s] → tutorialSvc.GetTutorialVersionInfo VID=%d (no data → None)", addr, tutorialID)
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		criterias, _ := evedb.GetTutorialCriterias(ctx, sqldb, tutorialID)
		pageCriterias, _ := evedb.GetTutorialPageCriterias(ctx, sqldb, tutorialID)
		log.Printf("[%s] → tutorialSvc.GetTutorialVersionInfo VID=%d (from DB)", addr, tutorialID)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyObject{
			Type: "util.KeyVal",
			Args: marshal.PyDict{Items: []marshal.DictEntry{
				{Key: marshal.PyString{Value: "tutorial"}, Value: tutorialRow},
				{Key: marshal.PyString{Value: "pages"}, Value: pageRow},
				{Key: marshal.PyString{Value: "criterias"}, Value: criterias},
				{Key: marshal.PyString{Value: "pagecriterias"}, Value: pageCriterias},
			}},
		})

	case "LogAborted":
		// LogAborted(tutorialID, pageNo, time) is the "No" answer of the continue-later dialog:
		// tutorials are disabled for the character and the choice is stored server-side.
		var tutorialID int64
		if len(cs.ArgTuple.Items) >= 1 {
			if v, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
				tutorialID = int64(v.Value)
			}
		}
		if charID != 0 {
			if err := evedb.DisableTutorials(ctx, sqldb, charID); err != nil {
				log.Printf("[%s] tutorialSvc.LogAborted DisableTutorials error: %v", addr, err)
			}
			if tutorialID != 0 {
				_ = evedb.MarkTutorialComplete(ctx, sqldb, charID, tutorialID)
			}
			log.Printf("[%s] → tutorialSvc.LogAborted charID=%d tutorialID=%d (tutorials disabled)", addr, charID, tutorialID)
		}
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})

	case "LogCompleted", "LogClosed", "LogAppClosed":
		// LogCompleted (sequence finished), LogClosed ("Yes", continue later) and LogAppClosed
		// (app closed mid-tutorial) all take (tutorialID, pageNo, time) and mark it complete.
		var tutorialID int64
		if len(cs.ArgTuple.Items) >= 1 {
			if v, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
				tutorialID = int64(v.Value)
			}
		}
		if charID != 0 && tutorialID != 0 {
			_ = evedb.MarkTutorialComplete(ctx, sqldb, charID, tutorialID)
		}
		log.Printf("[%s] → tutorialSvc.%s charID=%d tutorialID=%d", addr, cs.Method, charID, tutorialID)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})

	default:
		// Catch-all for unknown methods.  Auto-persist tutorial completion
		// for any unknown method whose first arg is a tutorialID int.
		log.Printf("[%s] → tutorialSvc.%s UNHANDLED (args: %d items)", addr, cs.Method, len(cs.ArgTuple.Items))
		var tutorialID int64
		if len(cs.ArgTuple.Items) >= 1 {
			if v, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
				tutorialID = int64(v.Value)
			}
		}
		if charID != 0 && tutorialID != 0 {
			if err := evedb.MarkTutorialComplete(ctx, sqldb, charID, tutorialID); err != nil {
				log.Printf("[%s] tutorialSvc.%s auto-complete ERROR charID=%d tutorialID=%d: %v", addr, cs.Method, charID, tutorialID, err)
			} else {
				log.Printf("[%s] → tutorialSvc.%s auto-marked charID=%d tutorialID=%d complete", addr, cs.Method, charID, tutorialID)
			}
		}
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
}

// isRowsetEmpty returns true if a util.Rowset PyObject has no data rows.
// It extracts the "lines" PyList from the rowset's Args dict and checks
// whether its Items slice is empty.
func isRowsetEmpty(r marshal.PyRep) bool {
	obj, ok := r.(marshal.PyObject)
	if !ok {
		return true
	}
	dict, ok := obj.Args.(marshal.PyDict)
	if !ok {
		return true
	}
	for _, entry := range dict.Items {
		key, ok := entry.Key.(marshal.PyString)
		if !ok || key.Value != "lines" {
			continue
		}
		list, ok := entry.Value.(marshal.PyList)
		if !ok {
			return true
		}
		return len(list.Items) == 0
	}
	return true
}

// msgTypeName returns a human-readable name for a MachoMsgType.
func msgTypeName(t evenet.MachoMsgType) string {
	names := map[evenet.MachoMsgType]string{
		evenet.AuthenticationReq:               "AUTHENTICATION_REQ",
		evenet.AuthenticationRsp:               "AUTHENTICATION_RSP",
		evenet.IdentificationReq:               "IDENTIFICATION_REQ",
		evenet.IdentificationRsp:               "IDENTIFICATION_RSP",
		evenet.CallReq:                         "CALL_REQ",
		evenet.CallRsp:                         "CALL_RSP",
		evenet.TransportClosed:                 "TRANSPORTCLOSED",
		evenet.ResolveReq:                      "RESOLVE_REQ",
		evenet.ResolveRsp:                      "RESOLVE_RSP",
		evenet.Notification:                    "NOTIFICATION",
		evenet.ErrorResponse:                   "ERRORRESPONSE",
		evenet.SessionChangeNotification:       "SESSIONCHANGENOTIFICATION",
		evenet.SessionInitialStateNotification: "SESSIONINITIALSTATENOTIFICATION",
		evenet.PingReq:                         "PING_REQ",
		evenet.PingRsp:                         "PING_RSP",
	}
	if n, ok := names[t]; ok {
		return n
	}
	return fmt.Sprintf("UNKNOWN(%d)", int(t))
}

// decodeAuthReq extracts login, password, and languageID from an AuthenticationReq
// payload.  The payload is a 3-tuple: (clientinfo dict, login string, password string).
// languageID is read from the clientinfo dict (key "languageID"); defaults to "en".
func decodeAuthReq(payload marshal.PyRep) (login, password, languageID string, err error) {
	t, ok := payload.(marshal.PyTuple)
	if !ok {
		return "", "", "", fmt.Errorf("expected tuple, got %T", payload)
	}
	if len(t.Items) < 3 {
		return "", "", "", fmt.Errorf("expected >=3 items, got %d", len(t.Items))
	}
	ls, ok := t.Items[1].(marshal.PyString)
	if !ok {
		return "", "", "", fmt.Errorf("login is not a string: %T", t.Items[1])
	}
	ps, ok := t.Items[2].(marshal.PyString)
	if !ok {
		return "", "", "", fmt.Errorf("password is not a string: %T", t.Items[2])
	}
	languageID = "en" // default
	if d, ok := t.Items[0].(marshal.PyDict); ok {
		for _, e := range d.Items {
			if k, ok := e.Key.(marshal.PyString); ok && k.Value == "languageID" {
				if v, ok := e.Value.(marshal.PyString); ok {
					languageID = v.Value
				}
			}
		}
	}
	return ls.Value, ps.Value, languageID, nil
}

// sendLoginSuccess sends macho.AuthenticationRsp, then arms the initial session state
// instead of sending it: the client primes cfg from the AuthenticationRsp payload, and a
// notification before that raises AttributeError 'effectOnline'. authentication.Ping
// releases it. See docs/login-session-gate.md.
func sendLoginSuccess(c *evenet.Conn, addr string, req *evenet.PyPacket, accountID, role int64, languageID string, sqldb *sql.DB, cacheStore *evecache.Store, ds *destinyConn) error {
	payload := marshal.PyTuple{Items: []marshal.PyRep{
		// [0] serverinfo
		marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "version.project"}, Value: marshal.PyString{Value: projectVersion}},
			{Key: marshal.PyString{Value: "version.build"}, Value: marshal.PyInt{Value: bootBuild}},
			{Key: marshal.PyString{Value: "version.number"}, Value: marshal.PyReal{Value: bootVersion}},
		}},
		// [1] accountID
		marshal.PyInt{Value: uint64(accountID)},
		// [2] role
		marshal.PyInt{Value: uint64(role)},
		// [3] substream: (cachables dict, services dict). The cachables come from the *.cache
		// files in the -cache dir; the services dict is buildServiceListDict.
		&marshal.PySubStream{Decoded: marshal.PyTuple{Items: []marshal.PyRep{
			cacheStore.LoginHints(context.Background(), sqldb),
			buildServiceListDict(),
		}}},
		// [4] proxyNodeID
		marshal.PyInt{Value: 10012002},
	}}
	rsp := &evenet.PyPacket{
		TypeString: "macho.AuthenticationRsp",
		Type:       evenet.AuthenticationRsp,
		Source:     evenet.PyAddress{Type: evenet.AddrAny},
		Dest: evenet.PyAddress{
			Type:   evenet.AddrClient,
			TypeID: 130293001608, // HackFixedClientID from Client.cpp
			CallID: req.Source.CallID,
		},
		UserID:  uint64(accountID),
		Payload: payload,
		NamedPayload: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "channel"}, Value: marshal.PyString{Value: "authentication"}},
		}},
	}
	log.Printf("[%s] → AuthenticationRsp accountID=%d role=%d", addr, accountID, role)
	if err := sendRep(c, rsp.Encode()); err != nil {
		return err
	}

	ds.pendingLoginSession = func() {
		if err := sendSessionChange(c, addr, addr, accountID, role, languageID); err != nil {
			log.Printf("[%s] deferred SessionInitialStateNotification: %v", addr, err)
		}
	}
	// Watchdog: a client that never pings would never reach character selection.
	// See docs/login-session-gate.md#release-rule.
	time.AfterFunc(loginSessionWatchdog, ds.releasePendingLoginSession)
	return nil
}

// loginSessionWatchdog bounds the wait for the client's post-prime authentication.Ping.
// Priming takes seconds on a cold client, so 30 s means it will not ping.
// See docs/login-session-gate.md#release-rule.
const loginSessionWatchdog = 30 * time.Second

// loginPingQuiet is how long after the last authentication.Ping the clock-sync burst
// is taken to be over. It is the only delay added to login.
const loginPingQuiet = 500 * time.Millisecond

// releasePendingLoginSession sends the deferred login SessionInitialStateNotification
// exactly once. The connection goroutine, the quiet timer and the watchdog may all call
// it; the Once decides which one sends.
func (ds *destinyConn) releasePendingLoginSession() {
	ds.pendingLoginSessionOnce.Do(func() {
		if ds.pendingLoginSession != nil {
			ds.pendingLoginSession()
		}
	})
}

// buildServiceListDict mirrors Client::_BuildServiceListDict: the services on this node.
// Values are None, or a location-hint string such as "solarsystem" or "station".
func buildServiceListDict() marshal.PyDict {
	svc := func(name string, hint marshal.PyRep) marshal.DictEntry {
		return marshal.DictEntry{Key: marshal.PyString{Value: name}, Value: hint}
	}
	none := marshal.PyNone{}
	return marshal.PyDict{Items: []marshal.DictEntry{
		svc("objectCaching", none),
		svc("lookupSvc", none),
		svc("beyonce", none),
		svc("standing2", none),
		svc("ram", none),
		svc("ramProxy", none),
		svc("lien", none),
		svc("voucher", none),
		svc("entity", none),
		svc("keeper", marshal.PyString{Value: "solarsystem"}),
		svc("agentMgr", none),
		svc("dogmaIM", none),
		svc("machoNet", none),
		svc("watchdog", none),
		svc("ship", none),
		svc("npcSvc", none),
		svc("cacheSvc", none),
		svc("market", none),
		svc("dungeon", none),
		svc("dungeonMgr", none),
		svc("scenario", none),
		svc("aggressionMgr", none),
		svc("sessionMgr", none),
		svc("LSC", marshal.PyString{Value: "location"}),
		svc("allianceRegistry", none),
		svc("bookmark", marshal.PyString{Value: "location"}),
		svc("invbroker", none),
		svc("character", none),
		svc("factory", none),
		svc("corpStationMgr", none),
		svc("authentication", none),
		svc("station", marshal.PyString{Value: "station"}),
		svc("slash", none),
		svc("charmgr", none),
		svc("stationSvc", none),
		svc("gangsta", none),
		svc("config", marshal.PyString{Value: "locationPreferred"}),
		svc("deadlineMgr", none),
		svc("billingMgr", none),
		svc("billMgr", none),
		svc("map", none),
		svc("posMgr", none),
		svc("lootSvc", none),
		svc("http", none),
		svc("gagger", none),
		svc("dataconfig", none),
		svc("DB", none),
		svc("i2", none),
		svc("account", marshal.PyString{Value: "location"}),
		svc("telnet", none),
		svc("alert", none),
		svc("director", none),
		svc("dogma", none),
		svc("pathfinder", none),
		svc("corporationSvc", none),
		svc("clones", none),
		svc("effectCompiler", none),
		svc("corpmgr", none),
		svc("warRegistry", none),
		svc("corpRegistry", none),
		svc("missionMgr", none),
		svc("userSvc", none),
		svc("counter", none),
		svc("petitioner", none),
		svc("debug", none),
		svc("shipSvc", none),
		svc("onlineStatus", none),
		svc("skillMgr", none),
	}}
}

// sendSessionChange sends the initial login state as SessionInitialStateNotification
// (type 18). A type-16 notification would advance the client's transport version counter
// and strand every later packet. The payload is a snapshot ({key: value}, nodesOfInterest).
// See docs/main-s8-notes.md#scn-type-18.
func sendSessionChange(c *evenet.Conn, addr, clientAddr string, accountID, role int64, languageID string) error {
	initialState := marshal.PyDict{Items: []marshal.DictEntry{
		{Key: marshal.PyString{Value: "userType"}, Value: marshal.PyInt{Value: 23}},
		{Key: marshal.PyString{Value: "userid"}, Value: marshal.PyInt{Value: uint64(accountID)}},
		{Key: marshal.PyString{Value: "address"}, Value: marshal.PyString{Value: clientAddr}},
		{Key: marshal.PyString{Value: "role"}, Value: marshal.PyInt{Value: uint64(role)}},
		{Key: marshal.PyString{Value: "languageID"}, Value: marshal.PyString{Value: languageID}},
	}}
	payload := marshal.PyTuple{Items: []marshal.PyRep{
		initialState,
		marshal.PyList{}, // nodesOfInterest, empty before a character exists
	}}
	pkt := &evenet.PyPacket{
		TypeString: "macho.SessionInitialStateNotification",
		Type:       evenet.SessionInitialStateNotification,
		Source: evenet.PyAddress{
			Type:   evenet.AddrNode,
			TypeID: 888444, // globalNodeID from server/main.cpp
		},
		Dest: evenet.PyAddress{
			Type:   evenet.AddrClient,
			TypeID: 130293001608, // HackFixedClientID
		},
		UserID:  0,
		Payload: payload,
		NamedPayload: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "channel"}, Value: marshal.PyString{Value: "sessionchange"}},
		}},
	}
	log.Printf("[%s] → SessionInitialStateNotification (userType=23 userid=%d role=%d languageID=%q)",
		addr, accountID, role, languageID)
	return sendRep(c, pkt.Encode())
}

// sendLoginError sends macho.ErrorResponse with LoginAuthFailed.
func sendLoginError(c *evenet.Conn, addr string, req *evenet.PyPacket) error {
	payload := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyInt{Value: 0},
		marshal.PyInt{Value: 2},
		marshal.PyTuple{Items: []marshal.PyRep{
			&marshal.PySubStream{Decoded: marshal.PyObject{
				Type: "ccp_exceptions.UserError",
				Args: marshal.PyDict{Items: []marshal.DictEntry{
					{Key: marshal.PyString{Value: "args"}, Value: marshal.PyTuple{Items: []marshal.PyRep{marshal.PyString{Value: "LoginAuthFailed"}}}},
					{Key: marshal.PyString{Value: "dict"}, Value: marshal.PyNone{}},
					{Key: marshal.PyString{Value: "msg"}, Value: marshal.PyString{Value: "LoginAuthFailed"}},
				}},
			}},
		}},
	}}
	namedPayload := marshal.PyDict{Items: []marshal.DictEntry{
		{Key: marshal.PyString{Value: "channel"}, Value: marshal.PyString{Value: "authentication"}},
	}}
	rsp := &evenet.PyPacket{
		TypeString: "macho.ErrorResponse",
		Type:       evenet.ErrorResponse,
		Source:     evenet.PyAddress{Type: evenet.AddrAny},
		Dest: evenet.PyAddress{
			Type:   evenet.AddrClient,
			TypeID: 130293001608,
			CallID: req.Source.CallID,
		},
		Payload:      payload,
		NamedPayload: namedPayload,
	}
	log.Printf("[%s] → ErrorResponse (LoginAuthFailed)", addr)
	return sendRep(c, rsp.Encode())
}

// mapCaller supplies the star map's overlay queries with the online character
// set they need: corp mates come from the live connection registry, gang mates
// from the gang registry.
func mapCaller(charID, corpID int64) mapsvc.Caller {
	return mapsvc.Caller{
		CharID: charID,
		CorpID: corpID,
		// Every present character: GetCorpMemberLocations intersects the list with
		// character_.corporationID in SQL, which decides membership, not a cached session CorpID.
		OnlineCorpMates: onlineCharIDs,
		GangMates: func() []int64 {
			if globalGangReg == nil {
				return nil
			}
			g := globalGangReg.GetGang(globalGangReg.GetCharGang(charID))
			if g == nil {
				return nil
			}
			ids := make([]int64, 0, len(g.Members))
			for id := range g.Members {
				ids = append(ids, id)
			}
			return ids
		},
		PilotCounts: func() (inSpace, docked map[int64]int64) {
			inSpace = make(map[int64]int64)
			docked = make(map[int64]int64)
			if globalSessReg == nil {
				return
			}
			for _, sess := range globalSessReg.Snapshot() {
				if sess.CharID == 0 || sess.SolarSystemID == 0 {
					continue
				}
				if sess.StationID != 0 {
					docked[sess.SolarSystemID]++
				} else {
					inSpace[sess.SolarSystemID]++
				}
			}
			return
		},
	}
}

// boundCharCorpID resolves the corporation a character belongs to, for
// corpRegistry bound calls that do not carry the corp ID as an argument.
func boundCharCorpID(charID int64, sqldb *sql.DB) int64 {
	if charID == 0 || sqldb == nil {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var corpID int64
	_ = sqldb.QueryRowContext(ctx,
		`SELECT corporationID FROM character_ WHERE characterID = ?`, charID).Scan(&corpID)
	return corpID
}

// boundArgInt reads a PyInt from a bound call's argument tuple, or 0.
func boundArgInt(cs *server.CallStream, idx int) int64 {
	if idx >= len(cs.ArgTuple.Items) {
		return 0
	}
	if v, ok := cs.ArgTuple.Items[idx].(marshal.PyInt); ok {
		return int64(v.Value)
	}
	return 0
}

// bookmarkLocation snapshots where the character is so the bookmark service
// can record "my current position" and pick a hangar for vouchers.
func bookmarkLocation(ds *destinyConn, stationID *int64) bookmark.Location {
	loc := bookmark.Location{}
	if stationID != nil {
		loc.StationID = *stationID
	}
	if ds == nil {
		return loc
	}
	loc.ShipID = ds.shipID
	loc.SysID = ds.sysID
	if ds.entity != nil && loc.StationID == 0 {
		p := ds.entity.Pos()
		loc.InSpace = true
		loc.X, loc.Y, loc.Z = p.X, p.Y, p.Z
	}
	return loc
}

// ── Destiny subsystem ─────────────────────────────────────────────────────────

// destinyConn holds per-connection destiny state.
type destinyConn struct {
	// pendingBallparkFrom, when set, is the entity whose ballpark state the
	// next registerUndock hands to the new one (an in-space ship swap).
	pendingBallparkFrom  *destiny.Entity
	pendingBallparkOldID uint32
	// spaceEntry records how the next registerUndock came about (outlawkos.go). Only the
	// login-in-space paths set it and registerUndock consumes it, so a stale reason never
	// reaches a later gate jump.
	spaceEntry spaceEntry

	dm              *destiny.Manager
	shipID          int64
	shipTypeID      int64 // typeID of the current ship (for SlimItem)
	shipGroupID     int64
	shipCategoryID  int64
	charID          int64 // character ID (for SlimItem ownerID/charID)
	corpID          int64
	allianceID      int64
	securityStatus  float64
	bounty          float64
	charName        string
	shipName        string // current ship's entity.itemName, encoded as the ball name
	stamp           uint32
	sysID           int64                   // solar system ID the ship is in
	constellationID int64                   // constellation the ship is in
	regionID        int64                   // region the ship is in
	solItem         marshal.PyRep           // util.Row for the solar system entity
	celestials      []evedb.CelestialEntity // all system celestials loaded at undock
	cancelWarp      context.CancelFunc      // cancels any in-flight warp goroutine
	// warpGen identifies the warp entitled to steer the ship. startPlayerWarp bumps it and
	// only a goroutine holding the current value may touch nav state when it exits.
	// See docs/main-s8-notes.md#connection-module-state.
	warpGen          uint64
	destinyBaseTime  time.Time // wall-clock time when last SetState was sent
	destinyBaseStamp uint32    // stamp used in the last SetState binary blob

	// Multi-player visibility state.
	solarSystem *destiny.SolarSystem   // shared system state (nil when docked)
	entity      *destiny.Entity        // this connection's entity in the registry
	sysMgr      *destiny.SystemManager // global system manager

	// Targeting state.
	targets        []uint32
	lockingTargets map[uint32]context.CancelFunc // targets being locked (timer pending)
	targetMu       sync.Mutex                    // protects targets + lockingTargets + jamUntil + cloakRecalUntil
	jamUntil       time.Time                     // ECM jam: locks broken and re-locking blocked until then
	// cloakRecalUntil ends the cloakingTargetingDelay after a cloaking device stops: no new
	// lock before it (cloak.go).
	cloakRecalUntil time.Time

	// carriedCharge is the capacitor and shield captured before a gate jump or teleport, for
	// the registerUndock that follows (shipcharge.go). Connection goroutine only.
	carriedCharge *shipCharge

	// Module activation state.
	activeModules  map[int64]*activeModule
	miningSessions map[int64]*miningSession
	cancelWeapon   map[int64]context.CancelFunc

	// immobilisers enforces siegeModeWarpStatus (852): module itemID to name for every
	// running module that pins the ship. Connection goroutine only.
	// See docs/main-s8-notes.md#connection-module-state.
	immobilisers map[int64]string

	// lastJumpAt is when this ship last used its jump drive, for jumpDriveDuration (869).
	// Kept in memory; a restart does not owe a cooldown.
	lastJumpAt time.Time

	// cynoExpiryTimer is the tick id (not the label) of the pending cyno-expire one-shot,
	// kept so tick.CancelOnce can cancel it when the field collapses early.
	cynoExpiryTimer uint64

	// moduleReadyAt enforces moduleReactivationDelay (669): module itemID to the time before
	// which it may not activate again, armed when a cycle ends. Guarded by moduleReadyMu.
	// See docs/main-s8-notes.md#connection-module-state.
	moduleReadyMu sync.Mutex
	moduleReadyAt map[int64]time.Time

	// reloads holds the charge-load replies waiting out a reload, keyed by module itemID.
	// Guarded by reloadMu, since the reload timer goroutine reads it (reload.go).
	reloadMu sync.Mutex
	reloads  map[int64]*pendingReload

	// Dungeon editor state. dunObjects is written on the connection goroutine and read by
	// NPC death, weapon cycles and missile flights, which go through dunObjMu (dunObjOwns,
	// dunObjSnapshot).
	dunObjMu   sync.Mutex
	dunObjects map[uint32]int64 // ballpark entity ID → dungeon objectID

	// While dunPendingMode is set, spawnDungeonObjects stashes new balls here instead of
	// sending AddBalls; the warp landing batch consumes them so the room arrives with the grid.
	dunPendingMode  bool
	dunPendingBalls []destiny.BallDesc
	dunPendingSlims []destiny.SlimItem
	dunPendingDmg   map[uint32]destiny.DamageState
	// dunPendingAnchor overrides the room anchor while the warp is in flight: the pre-render
	// spawn runs before dm.Pos reaches the landing, so the warp goroutine passes it.
	dunPendingAnchor *destiny.Vec3
	// dunPendingRoomEnter defers the room's roomEnter triggers from the mid-tunnel
	// pre-render spawn to the warp landing batch.
	dunPendingRoomEnter bool
	dunRoomID           int64 // currently loaded dungeon room ID
	// dunNoWarp mirrors dunRooms.naturalPhenomena for the loaded room: warp drives do not
	// work inside the pocket. Acceleration gates are exempt.
	dunNoWarp   bool
	dunSelected []uint32                // currently selected ball IDs in the editor
	dunOffset   destiny.Vec3            // player position offset applied at room spawn time
	dunGateDir  map[uint32]destiny.Vec3 // gate entity ID → outward unit direction (dunDirection)

	// dunSpawnShift offsets the next spawnDungeonRoom's beacon anchor from the player (gate
	// travel sets it so the beacon sits ahead of the ship). spawnDungeonRoom resets it.
	dunSpawnShift destiny.Vec3

	// Room spawn context captured by spawnDungeonRoom so spawnWave triggers
	// can spawn held wave groups later with identical placement and faction.
	dunRoomAnchor destiny.Vec3 // beacon/centroid gates aim outward from
	dunFactionID  int64        // dungeon-level NPC faction
	dunCorpID     int64        // corp resolved from dunFactionID
	dunMissionID  int64        // mission kill-tracking tag for spawned NPCs
	// dunOpenSiteID is the open site the loaded room belongs to (opensites.go): a spawner site or
	// a fixed dungeon (fixedsites.go), 0 for none. Only spawnDungeonRoom sets it, from dunPendingOpenSite; other loads clear it.
	dunOpenSiteID      int64
	dunPendingOpenSite int64

	// dunWaveSpawn carries wave-spawn requests from spawnWave triggers (which may fire on NPC
	// tick goroutines) to the connection goroutine. The roomID is captured at trigger time so
	// a delayed wave cannot land in a room the pilot has left.
	dunWaveSpawn chan dunWaveReq

	// dunRespawn carries a fixed dungeon's respawns (fixedsites.go) from the tick lane to the
	// connection goroutine, which spawns them only if the pilot is on that room's grid.
	dunRespawn chan dunRespawnReq

	// pendingDock is the pilot's open docking request (docking.go), and
	// dockCheck the poke its checker goroutine sends this connection's
	// goroutine twice a second while it is open.
	pendingDock *pendingDock
	dockCheck   chan struct{}

	// gangWarp carries a gang leader's resolved warp to this member's
	// connection goroutine (gangwarp.go), which starts it like the pilot's
	// own WarpToStuff would.
	gangWarp chan gangWarpReq

	// missionEnded carries the ID of a mission the expiry sweep just failed
	// (missionexpiry.go), so the pocket comes off this pilot's grid if they
	// are sitting in it.
	missionEnded chan int64

	// playerDeath carries ship/pod death events from attacker goroutines
	// (weapon cycles, missiles, drones, NPC combat) to this connection's
	// goroutine, which runs the death sequence against its own state.
	playerDeath chan playerDeathReq

	// stampResync carries a "your sim clock has run away, rebuild" request from the ballpark
	// reconciler to the connection goroutine, which owns destinyBaseStamp/destinyBaseTime.
	// Capacity 1, non-blocking send. See docs/main-s9-notes.md#stamp-lead-net.
	stampResync chan struct{}

	// egoSpeedEcho is the last SetSpeedFraction put on this pilot's own stream and the
	// stream's floor after it. See noteEgoSpeedEcho. Connection goroutine only.
	egoSpeedEcho egoSpeedEcho

	// warpPropModOff carries "the tunnel just opened" from the warp goroutine to the
	// connection goroutine, which switches the prop mods off (stopPropModsForWarp). The value
	// is the warp generation, so a request outlived by a newer warp is dropped.
	warpPropModOff chan uint64

	// Dungeon logic trigger state for the loaded room (dungeon_triggers.go), guarded by
	// dunTrigMu: roomEnter fires on the connection goroutine, npcKilled/roomCleared on NPC
	// tick goroutines. dunWaveGroups/dunWavesOut drive the auto-wave chain.
	dunTrigMu     sync.Mutex
	dunTriggers   []evedb.TriggerRow
	dunTrigFired  map[int64]bool
	dunTrigRoomID int64
	dunWaveGroups []int64
	dunWavesOut   map[int64]bool

	// dungeonRefresh: the ESP panel sends roomIDs here to respawn the dungeon room for the
	// connected player.
	dungeonRefresh chan int64

	// cancelReconcile stops the ballpark reconciliation goroutine when the
	// player docks, jumps, or disconnects.
	cancelReconcile context.CancelFunc

	// pendingLoginSession holds the login SessionInitialStateNotification until cfg is primed;
	// the Ping quiet timer (loginPingQuietTimer) or the watchdog releases it once.
	// See docs/login-session-gate.md.
	pendingLoginSession     func()
	pendingLoginSessionOnce sync.Once
	loginPingQuietTimer     *time.Timer

	// lastActivity is the wall-clock time of the last packet received on
	// this connection. Drives the pre-auth idle reaper in handleConn.
	lastActivity time.Time

	// setStateSent is true after the first SetState has been sent on this
	// connection. Prevents GetFormations from sending a redundant second
	// SetState that resets the client ballpark mid-NPC-warp.
	setStateSent bool

	// pendingGateEffect is set on gate jumps so GetFormations sends the GateActivity FX
	// after the SetState. pendingGateID is the destination gate's ball, which that effect
	// animates. See docs/main-s8-notes.md#setstate-from-formations.
	pendingGateEffect bool
	pendingGateID     int64

	// pendingUndockBurst is set by initDestiny when it takes the station branch
	// and kicks the ball out at max velocity. Deferred to GetFormations for the
	// same reason the gate flash is: the FX needs a live ballpark to land in.
	pendingUndockBurst bool

	// stationIDPtr points to the connection goroutine's stationID local.
	// Used by death handlers running in attacker goroutines to update the
	// victim's docked state on pod-kill respawn.
	stationIDPtr *int64
}

// ballName is the name encoded into this connection's own ship ball. The client hints
// cfg.evelocations with ball.name on every AddBalls and shows it for the ship, so it
// carries the ship's item name, not the pilot's, or each undock would overwrite a
// custom ship name.
func (ds *destinyConn) ballName() string {
	if ds.shipName != "" {
		return ds.shipName
	}
	return ds.charName
}

// dunObjOwns reports whether entityID is one of this connection's dungeon
// balls. Safe to call from any goroutine.
func (ds *destinyConn) dunObjOwns(entityID uint32) bool {
	ds.dunObjMu.Lock()
	defer ds.dunObjMu.Unlock()
	_, ok := ds.dunObjects[entityID]
	return ok
}

// dunObjCount reports how many dungeon balls this connection currently holds.
// Zero after a warp departure stripped the site off the grid.
func (ds *destinyConn) dunObjCount() int {
	ds.dunObjMu.Lock()
	defer ds.dunObjMu.Unlock()
	return len(ds.dunObjects)
}

// dunObjGet returns the dungeon objectID for a ballpark entity ID. Safe to
// call from any goroutine.
func (ds *destinyConn) dunObjGet(entityID uint32) (int64, bool) {
	ds.dunObjMu.Lock()
	defer ds.dunObjMu.Unlock()
	oid, ok := ds.dunObjects[entityID]
	return oid, ok
}

// dunObjSnapshot returns a copy of the dungeon-ball map for iteration off
// the connection goroutine (NPC death path).
func (ds *destinyConn) dunObjSnapshot() map[uint32]int64 {
	ds.dunObjMu.Lock()
	defer ds.dunObjMu.Unlock()
	out := make(map[uint32]int64, len(ds.dunObjects))
	for k, v := range ds.dunObjects {
		out[k] = v
	}
	return out
}

// miningSession tracks a running mining cycle goroutine. Deactivate sets stop so the
// loop finishes its current cycle (ore is granted per completed cycle) before shutting
// off; cancel hard-kills the loop when the same module is re-activated.
type miningSession struct {
	cancel context.CancelFunc
	stopCh chan struct{} // closed on Deactivate for instant cycle interrupt
}

// activeModule tracks a currently active module (e.g. mining laser firing).
type activeModule struct {
	itemID     int64
	typeID     int64
	targetID   uint32
	effectName string
	isActive   bool
}

// egoSpeedEcho remembers the last SetSpeedFraction echoed to a pilot's own ship, so a
// repeat can be recognised.
type egoSpeedEcho struct {
	entity *destiny.Entity // the stream it went out on; a new ship is a new stream
	stamp  uint32          // the stream's floor right after that send
	f      float64
}

// noteEgoSpeedEcho records that the ego was just sent SetSpeedFraction f.
func (ds *destinyConn) noteEgoSpeedEcho(f float64) {
	if ds.entity == nil {
		ds.egoSpeedEcho = egoSpeedEcho{}
		return
	}
	ds.egoSpeedEcho = egoSpeedEcho{entity: ds.entity, stamp: ds.entity.LastMintedStamp(), f: f}
}

// egoSpeedEchoRedundant reports whether echoing SetSpeedFraction f to the ego would change
// nothing: the last thing stamped onto this stream was an echo of the same fraction. Any
// other mint moves the stream floor and re-enables the echo.
// See docs/destiny-stamps.md#nav-click-burst.
func (ds *destinyConn) egoSpeedEchoRedundant(f float64) bool {
	e := ds.egoSpeedEcho
	return e.entity != nil && e.entity == ds.entity && e.f == f &&
		ds.entity.LastMintedStamp() == e.stamp
}

// syncStamp raises dm.Stamp to the client's clock: elapsed time since the last SetState,
// rounded up so the stamp is never behind. It never lowers dm.Stamp, which NPC broadcasts
// may have advanced.
func (ds *destinyConn) syncStamp() {
	if ds.dm == nil || ds.destinyBaseTime.IsZero() {
		return
	}
	elapsed := time.Since(ds.destinyBaseTime).Seconds()
	newStamp := ds.destinyBaseStamp + uint32(math.Ceil(elapsed))
	// Route through Entity when available so the update is serialized under Entity.mu
	// against NPC broadcast goroutines that also use Entity.NextStamp.
	if ds.entity != nil {
		ds.entity.SyncStamp(newStamp)
	} else if newStamp > ds.dm.Stamp {
		ds.dm.Stamp = newStamp
	}
}

// sendSelfStamped sends a stamped destiny packet on the pilot's own stream, serialized
// with every other sender to this connection (destiny.Entity.SendStampedUrgent). It is
// the urgent path for the ego ball only; traffic about other balls is per-recipient on
// Entity. See docs/main-s8-notes.md#floor-advance.
func (ds *destinyConn) sendSelfStamped(send func(stamp uint32) error) error {
	if ds.entity != nil {
		return ds.entity.SendStampedUrgent(send)
	}
	if ds.dm != nil {
		return ds.dm.SendStampedUrgent(send)
	}
	return nil
}

// sendSelfFXStamped is sendSelfStamped for packets carrying only stamp-agnostic actions
// (OnSpecialFX, OnDamageStateChange, OnSlimItemChange, OnDroneStateChange): it reuses the
// current stamp, so FX traffic does not advance the client's sim clock.
func (ds *destinyConn) sendSelfFXStamped(send func(stamp uint32) error) error {
	if ds.entity != nil {
		return ds.entity.SendFXStamped(send)
	}
	if ds.dm != nil {
		return ds.dm.SendStampedUrgent(send)
	}
	return nil
}

// sendFloorAdvance sends a self destiny update at Stamp+1 and raises the stamp floors to
// it under the entity wire lock. Used by AddBalls/RemoveBalls floor bumps (dungeon
// respawn, undock spawn) that must land at or ahead of the client's sim clock.
func (ds *destinyConn) sendFloorAdvance(send func(stamp uint32) error) (uint32, error) {
	if ds.entity != nil {
		ds.entity.LockWire()
		defer ds.entity.UnlockWire()
	}
	ds.syncStamp()
	stamp := ds.dm.Stamp + 1
	err := send(stamp)
	// The floors rise but the wall anchors do not move. See docs/main-s8-notes.md#floor-advance.
	if ds.entity != nil {
		ds.entity.SyncStamp(stamp)
		if err == nil {
			ds.entity.NoteFloorMint(stamp)
		}
	} else if stamp > ds.dm.Stamp {
		ds.dm.Stamp = stamp
	}
	return stamp, err
}

// The undock lane occupancy registry is destiny.ClaimUndockSlot, shared with the rolebots
// module; stargate jump landings share destiny's per-system arrival lane with warp landings.
// See docs/undock-geometry.md#occupancy and docs/destiny-arrival-lanes.md#reservations-and-occupancy.

// sanitizeSetStateBalls applies destiny.BallDescForObserver across an assembled SetState
// blob in place. A follow/orbit reference is satisfied exactly when the ball it names is in
// the blob. egoID, the recipient's own ship, is left alone (0 for none).
// See docs/main-s8-notes.md#setstate-sanitize.
func sanitizeSetStateBalls(balls []destiny.BallDesc, egoID uint32) {
	inBlob := make(map[uint32]struct{}, len(balls))
	for _, b := range balls {
		inBlob[b.EntityID] = struct{}{}
	}
	knows := func(ref uint32) bool {
		_, ok := inBlob[ref]
		return ok
	}
	for i := range balls {
		if balls[i].EntityID == egoID {
			continue
		}
		balls[i] = destiny.BallDescForObserver(balls[i], knows)
	}
}

// initDestiny fetches ship stats from the DB and creates (or replaces) the
// destiny Manager for the current character's ship.
func initDestiny(addr string, charID, stationID int64, sqldb *sql.DB, ds *destinyConn) {
	if charID == 0 || sqldb == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	shipID, err := evedb.GetCurrentShipID(ctx, sqldb, charID)
	if err != nil || shipID == 0 {
		log.Printf("[%s] destiny: GetCurrentShipID charID=%d: %v", addr, charID, err)
		return
	}
	stats, err := evedb.GetShipStats(ctx, sqldb, shipID, charID, stationID)
	if err != nil {
		log.Printf("[%s] destiny: GetShipStats shipID=%d: %v", addr, shipID, err)
		return
	}
	dm := destiny.New(uint32(shipID), stats.Mass, stats.Agility, stats.MaxVelocity, stats.Radius)
	dm.Stamp = ds.stamp

	undocked := false
	// Where the ball's position came from, for the init line below: undock, no-dock-entry or restore.
	undockOrigin, undockSlot := "restore", -1
	if stationID > 0 {
		// Undock from station: destiny.UndockPlacement (shared with rolebots) places the ship by
		// lane slot, claimed as the ship. See docs/undock-geometry.md#occupancy.
		undockSlot = destiny.ClaimUndockSlot(stationID, shipID, destiny.UndockLaneHold)
		pos, vel, goTo, ok := destiny.UndockPlacement(
			destiny.Vec3{X: stats.StationX, Y: stats.StationY, Z: stats.StationZ},
			destiny.Vec3{X: stats.DockEntryX, Y: stats.DockEntryY, Z: stats.DockEntryZ},
			destiny.Vec3{X: stats.DockOrientationX, Y: stats.DockOrientationY, Z: stats.DockOrientationZ},
			stats.Radius, stats.MaxVelocity, undockSlot,
		)
		if ok {
			dm.Pos = pos
			dm.Vel = vel
			// GOTO a point destiny.UndockRunM down the undock line so the ship leaves at full speed;
			// the waypoint travels in the ball description, so client and server stop together.
			dm.GotoDirection(goTo)
			// The GotoDirection is server-sim only: SetState bypasses OnBallparkCall, so the client's
			// booster trigger never fires. GetFormations pushes the burst.
			ds.pendingUndockBurst = true
			undocked = true
			undockOrigin = "undock"
		} else {
			undockOrigin = "no-dock-entry"
			// A station type with no authored dock entry would exit at the station centre, inside
			// the model, so fall through to the persisted position.
			log.Printf("[%s] destiny: station %d has no dock entry — undocking at last known position", addr, stationID)
		}
	}
	if !undocked {
		// Logged in already in space, or the station has no authored door: restore the persisted
		// position from the entity table, with no kick-out velocity.
		x, y, z, ok, perr := evedb.GetEntityPosition(ctx, sqldb, shipID)
		if perr != nil {
			log.Printf("[%s] destiny: GetEntityPosition shipID=%d: %v", addr, shipID, perr)
		}
		if ok {
			dm.Pos = destiny.Vec3{X: x, Y: y, Z: z}
		}
		dm.Vel = destiny.Vec3{}
		ds.pendingUndockBurst = false // logged in parked, nothing to burn
		// SolarSystemID comes from the character_ table since there's no station to derive it from.
		cd, cdErr := evedb.LoadCharData(ctx, sqldb, charID)
		if cdErr == nil && cd.SolarSystemID != 0 {
			stats.SolarSystemID = cd.SolarSystemID
		}
	}
	ds.dm = dm
	ds.shipID = shipID
	ds.shipTypeID = stats.TypeID
	ds.shipGroupID = stats.GroupID
	ds.shipCategoryID = stats.CategoryID
	ds.charID = charID
	ds.corpID = stats.CorpID
	ds.allianceID = stats.AllianceID
	ds.securityStatus = stats.SecurityStatus
	ds.bounty = stats.Bounty
	ds.charName = stats.CharName
	ds.shipName = stats.ShipName
	ds.sysID = stats.SolarSystemID
	if stats.SolarSystemID != 0 {
		constID, regID, locErr := evedb.GetSolarSystemLocation(ctx, sqldb, stats.SolarSystemID)
		if locErr == nil {
			ds.constellationID = constID
			ds.regionID = regID
		} else {
			log.Printf("[%s] destiny: GetSolarSystemLocation sysID=%d: %v", addr, stats.SolarSystemID, locErr)
		}
	}
	if stats.SolarSystemID != 0 {
		er, erErr := evedb.GetSolarSystemEntityRow(ctx, sqldb, stats.SolarSystemID)
		if erErr != nil {
			log.Printf("[%s] destiny: GetSolarSystemEntityRow solarSystemID=%d: %v", addr, stats.SolarSystemID, erErr)
			ds.solItem = marshal.PyNone{}
		} else if er != nil {
			ds.solItem = buildEntityRow(er)
		} else {
			ds.solItem = marshal.PyNone{}
		}
		celestials, celErr := globalCelestialCache.GetCelestials(ctx, sqldb, stats.SolarSystemID)
		if celErr != nil {
			log.Printf("[%s] destiny: GetCelestials solarSystemID=%d: %v", addr, stats.SolarSystemID, celErr)
		} else {
			ds.celestials = celestials
			log.Printf("[%s] destiny: loaded %d celestials for sysID=%d", addr, len(celestials), stats.SolarSystemID)
			// Safety: if the ship's persisted position is near origin but the
			// system's star is far away, the position is corrupt.  Snap to star.
			if dm.Pos.IsZero() || dm.Pos.Len() < 1000 {
				if starPos, _, starOK := findStarPosition(celestials); starOK && starPos.Len() > 1000 {
					landing := starLandingPoint(celestials)
					log.Printf("[%s] destiny: position near origin — snapping to star at (%.0f,%.0f,%.0f)", addr, landing.X, landing.Y, landing.Z)
					dm.Pos = landing
				}
			}
		}
	} else {
		ds.solItem = marshal.PyNone{}
	}
	// origin, slot and door record which arm placed the ball, its lane seat, and its distance
	// from the station centre against the station radius.
	door := ""
	if stationID > 0 {
		stationPos := destiny.Vec3{X: stats.StationX, Y: stats.StationY, Z: stats.StationZ}
		door = fmt.Sprintf(" slot=%d out=%.0fm stationR=%.0fm",
			undockSlot, dm.Pos.Sub(stationPos).Len(), stats.StationRadius)
	}
	log.Printf("[%s] destiny: init origin=%s shipID=%d typeID=%d mass=%.0f agility=%.2f maxV=%.0f pos=(%.0f,%.0f,%.0f) vel=(%.0f,%.0f,%.0f) sysID=%d%s",
		addr, undockOrigin, shipID, stats.TypeID, stats.Mass, stats.Agility, stats.MaxVelocity,
		dm.Pos.X, dm.Pos.Y, dm.Pos.Z, dm.Vel.X, dm.Vel.Y, dm.Vel.Z, stats.SolarSystemID, door)
}

// handleBeyonce handles CallReq packets for the "beyonce" solar-system service.
func handleBeyonce(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket,
	accountID, charID, stationID int64, sqldb *sql.DB, ds *destinyConn, sysMgr *destiny.SystemManager) error {
	switch cs.Method {
	case "GetFormations":
		if ds.dm == nil {
			initDestiny(addr, charID, stationID, sqldb, ds)
		}
		// Return a 2-tuple of named formation definitions (Diamond, Arrow).
		// Each entry is (name, tuple-of-4-offsets), each offset is (x, y, z).
		makeFormation := func(name string, offsets [][3]float64) marshal.PyRep {
			slots := make([]marshal.PyRep, len(offsets))
			for i, o := range offsets {
				slots[i] = marshal.PyTuple{Items: []marshal.PyRep{
					marshal.PyReal{Value: o[0]},
					marshal.PyReal{Value: o[1]},
					marshal.PyReal{Value: o[2]},
				}}
			}
			return marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyString{Value: name},
				marshal.PyTuple{Items: slots},
			}}
		}
		formations := marshal.PyTuple{Items: []marshal.PyRep{
			makeFormation("Diamond", [][3]float64{{100, 0, 0}, {0, 100, 0}, {-100, 0, 0}, {0, -100, 0}}),
			makeFormation("Arrow", [][3]float64{{100, 0, -50}, {50, 0, 0}, {-100, 0, -50}, {-50, 0, 0}}),
		}}
		log.Printf("[%s] → beyonce.GetFormations", addr)
		if err := server.SendCallRsp(c, pkt, accountID, formations); err != nil {
			return err
		}
		// The first SetState is sent here, not at undock: an earlier one races the session change.
		// Skipped when one was already sent. See docs/main-s8-notes.md#setstate-from-formations.
		if ds.dm != nil && !ds.setStateSent {
			sendSetState(c, addr, uint64(accountID), charID, ds)
			ds.setStateSent = true

			// New grid, new system: rebuild the overview from this ballpark so a stale row does not
			// carry over. See overviewrefresh.go.
			scheduleOverviewRebuild(ds, c, accountID)

			// Push current HP attributes so the client's godma cache does not keep the damage cached
			// from ShipGetInfo at character selection (for example after a station repair).
			sendPlayerHPUpdate(ds.entity)

			// Deferred undock thruster burst, consumed unconditionally so a stale flag cannot fire on
			// a later SetState.
			if ds.pendingUndockBurst {
				ds.pendingUndockBurst = false
				if !ds.pendingGateEffect {
					sendBoosterFX(ds, c, uint64(accountID), dogma.FXGUIDBoosterShort)
				}
			}

			// A standing GM cloak arrives in the ball's cloak bit; an undock also gets the shimmer FX
			// a gate arrival sends itself.
			if !ds.pendingGateEffect && ds.entity != nil && ds.entity.IsGMCloak() {
				sendSelfCloakFX(ds, c, uint64(accountID))
			}

			// Send the deferred GateActivity effect now that the client has a live ballpark; before
			// the SetState it races the beyonce bind.
			if ds.pendingGateEffect {
				ds.pendingGateEffect = false
				gateID := ds.pendingGateID
				ds.pendingGateID = 0
				ds.syncStamp()
				// effects.GateActivity animates the ball named by its first param, which must be the
				// gate; the ship is the fallback. See docs/main-s8-notes.md#setstate-from-formations.
				fxBallID := uint64(ds.dm.EntityID)
				if gateID != 0 {
					fxBallID = uint64(gateID)
				}
				gateFXArgs := []marshal.PyRep{
					marshal.PyInt{Value: fxBallID},
					marshal.PyNone{}, marshal.PyNone{}, marshal.PyNone{}, marshal.PyNone{},
					marshal.PyList{},
					marshal.PyString{Value: "effects.GateActivity"},
					marshal.PyInt{Value: 0}, marshal.PyInt{Value: 1}, marshal.PyInt{Value: 0},
				}
				ds.sendSelfFXStamped(func(gateStamp uint32) error {
					return destiny.SendSimpleAction(c, globalNodeID, uint64(accountID), gateStamp,
						"OnSpecialFX", gateFXArgs...)
				})
				// Pilots already at the destination gate get the flash too, anchored on the gate so it
				// survives the arrival cloak that keeps the ship off their grids.
				if gateID != 0 {
					broadcastFXAction(ds, "OnSpecialFX", gateFXArgs...)
				}
				// Gate cloak visual (CloakRegardless). OnSpecialFX is stamp-agnostic, so it rides the
				// current stamp like the gate flash above.
				ds.sendSelfFXStamped(func(cloakStamp uint32) error {
					return destiny.SendSimpleAction(c, globalNodeID, uint64(accountID), cloakStamp,
						"OnSpecialFX",
						marshal.PyInt{Value: uint64(ds.dm.EntityID)},
						marshal.PyNone{}, marshal.PyNone{}, marshal.PyNone{}, marshal.PyNone{},
						marshal.PyList{},
						marshal.PyString{Value: "effects.CloakRegardless"},
						marshal.PyInt{Value: 0}, marshal.PyInt{Value: 1}, marshal.PyInt{Value: 0},
					)
				})
			}
		}
		return nil
	default:
		log.Printf("[%s] → beyonce.%s (None)", addr, cs.Method)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
}

// handleShipSvc handles CallReq packets for the "shipSvc" service.
func handleShipSvc(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket,
	accountID, charID int64, stationID *int64, sqldb *sql.DB, ds *destinyConn, sysMgr *destiny.SystemManager) error {
	switch cs.Method {
	case "Eject", "Evacuate":
		// eject.go. A refusal must be an exception: only that releases the client's 30 s
		// session-change lock (sessions.py PerformSessionChange).
		if *stationID != 0 {
			return corpUserErr(c, pkt, accountID, "You cannot eject while docked.")
		}
		refusal, after := shipSpaceCall(c, addr, accountID, charID, cs.Method, cs.ArgTuple.Items, sqldb, ds)
		if refusal != nil {
			return server.SendCallException(c, pkt, accountID, refusal)
		}
		if err := server.SendCallRsp(c, pkt, accountID, marshal.PyNone{}); err != nil {
			return err
		}
		after()
		return nil
	case "Undock":
		initDestiny(addr, charID, *stationID, sqldb, ds)
		log.Printf("[%s] → shipSvc.Undock charID=%d", addr, charID)
		registerUndock(c, addr, uint64(accountID), charID, sqldb, ds)
		if err := server.SendCallRsp(c, pkt, accountID, marshal.PyNone{}); err != nil {
			return err
		}
		if ds.sysID != 0 {
			if err := sendUndockSessionChange(c, addr, uint64(accountID), *stationID, ds.sysID, ds.shipID); err != nil {
				log.Printf("[%s] undock SCN: %v", addr, err)
			}
		}
		// SetState is deferred to beyonce.GetFormations (see the inline undock path).
		// stationID is cleared so /tr and /goto see the pilot as undocked.
		*stationID = 0
		if sqldb != nil && charID != 0 && ds.sysID != 0 {
			ctxU, cancelU := context.WithTimeout(context.Background(), 5*time.Second)
			if err := evedb.ClearCharacterStation(ctxU, sqldb, charID, ds.sysID, ds.constellationID, ds.regionID); err != nil {
				log.Printf("[%s] undock ClearCharacterStation: %v", addr, err)
			}
			cancelU()
		}
		return nil

	case "Board":
		// In space the only boardable ships are unpiloted hulls (eject.go).
		if *stationID == 0 {
			refusal, after := shipSpaceCall(c, addr, accountID, charID, "Board", cs.ArgTuple.Items, sqldb, ds)
			if refusal != nil {
				return server.SendCallException(c, pkt, accountID, refusal)
			}
			if err := server.SendCallRsp(c, pkt, accountID, marshal.PyNone{}); err != nil {
				return err
			}
			after()
			return nil
		}
		// Board(shipID) is the "Make Active Ship" button. It moves the character into the new ship
		// (flagPilot 57) and sends an SCN for session.shipid.
		if len(cs.ArgTuple.Items) < 1 {
			log.Printf("[%s] → shipSvc.Board: missing shipID arg", addr)
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		newShipIDRep, ok := cs.ArgTuple.Items[0].(marshal.PyInt)
		if !ok {
			log.Printf("[%s] → shipSvc.Board: arg not int: %T", addr, cs.ArgTuple.Items[0])
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		newShipID := int64(newShipIDRep.Value)
		{
			ctxB, cancelB := context.WithTimeout(context.Background(), 5*time.Second)
			uerr := boardRefusal(ctxB, sqldb, charID, *stationID, newShipID)
			cancelB()
			if uerr != nil {
				log.Printf("[%s] shipSvc.Board: rejected shipID=%d charID=%d", addr, newShipID, charID)
				return server.SendCallException(c, pkt, accountID, uerr)
			}
		}
		// The SCN goes out before the reply (boardDockedShip).
		prevShipID, err := boardDockedShip(c, addr, accountID, charID, newShipID, sqldb)
		if err != nil {
			log.Printf("[%s] shipSvc.Board: %v", addr, err)
		}
		log.Printf("[%s] → shipSvc.Board shipID=%d charID=%d prevShipID=%d", addr, newShipID, charID, prevShipID)
		if err != nil {
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		// The reply waits for godma's re-prime (boardhold.go).
		holdBoardReply(c, func() { _ = server.SendCallRsp(c, pkt, accountID, marshal.PyNone{}) })
		return nil

	case "AssembleShip":
		// AssembleShip([itemID, ...]) sets singleton=1. The client sends one list of itemIDs.
		var itemIDs []int64
		if len(cs.ArgTuple.Items) >= 1 {
			if lst, ok := cs.ArgTuple.Items[0].(marshal.PyList); ok {
				for _, it := range lst.Items {
					if iv, ok2 := it.(marshal.PyInt); ok2 {
						itemIDs = append(itemIDs, int64(iv.Value))
					}
				}
			} else if iv, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
				itemIDs = append(itemIDs, int64(iv.Value))
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var assembleStation int64
		if stationID != nil {
			assembleStation = *stationID
		}
		if uerr := assembleRefusal(ctx, sqldb, charID, assembleStation, itemIDs); uerr != nil {
			log.Printf("[%s] shipSvc.AssembleShip: refused items=%v", addr, itemIDs)
			return server.SendCallException(c, pkt, accountID, uerr)
		}
		charName := evedb.GetCharacterName(ctx, sqldb, charID)
		for _, id := range itemIDs {
			var typeName string
			if er, err := evedb.GetEntityRow(ctx, sqldb, id); err == nil && er != nil {
				typeName, _ = evedb.GetTypeName(ctx, sqldb, er.TypeID)
			}
			shipName := ""
			if charName != "" && typeName != "" {
				shipName = charName + "'s " + typeName
			}
			_, err := sqldb.ExecContext(ctx,
				`UPDATE entity SET singleton = 1, itemName = ? WHERE itemID = ?`, shipName, id)
			if err != nil {
				log.Printf("[%s] shipSvc.AssembleShip: update entity itemID=%d: %v", addr, id, err)
				continue
			}
			// Tell the client the ship is now assembled (singleton 0 to 1).
			er, err := evedb.GetEntityRow(ctx, sqldb, id)
			if err == nil && er != nil {
				_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
					evedb.BuildEntityRow(er),
					marshal.PyDict{Items: []marshal.DictEntry{
						{Key: marshal.PyInt{Value: 6}, Value: marshal.PyInt{Value: 0}}, // ixSingleton old=0
					}},
				}})
			}
		}
		log.Printf("[%s] → shipSvc.AssembleShip items=%v", addr, itemIDs)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})

	case "GetShipConfiguration":
		// shipconfig.py reads conf['allowGangSMBUsage'] by subscript, so the key is always present. The flag
		// persists (db/shipconfigdb.go); nothing consumes it yet.
		allowGang := 0
		if sqldb != nil && ds != nil && ds.shipID != 0 {
			cctx, ccancel := context.WithTimeout(context.Background(), 3*time.Second)
			if on, _ := evedb.GetShipAllowGangSMB(cctx, sqldb, ds.shipID); on {
				allowGang = 1
			}
			ccancel()
		}
		log.Printf("[%s] → shipSvc.GetShipConfiguration", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "allowGangSMBUsage"}, Value: marshal.PyInt{Value: uint64(allowGang)}},
		}})

	case "StoreVessel", "BoardStoredShip", "LaunchFromContainer", "ScoopToSMA":
		return handleSMAShipCall(c, addr, cs, pkt, accountID, charID, *stationID, sqldb, ds)

	case "ConfigureShip":
		// shipconfig.AllowGangChange returns the whole conf dict with allowGangSMBUsage flipped.
		// Only the ship's owner may change it.
		if sqldb != nil && ds != nil && ds.shipID != 0 && len(cs.ArgTuple.Items) >= 1 {
			if conf, ok := cs.ArgTuple.Items[0].(marshal.PyDict); ok {
				for _, de := range conf.Items {
					if k, ok := de.Key.(marshal.PyString); !ok || k.Value != "allowGangSMBUsage" {
						continue
					}
					allow := false
					switch v := de.Value.(type) {
					case marshal.PyBool:
						allow = v.Value
					case marshal.PyInt:
						allow = v.Value != 0
					}
					cctx, ccancel := context.WithTimeout(context.Background(), 3*time.Second)
					if er, _ := evedb.GetEntityRow(cctx, sqldb, ds.shipID); er != nil && er.OwnerID == charID {
						if err := evedb.SetShipAllowGangSMB(cctx, sqldb, ds.shipID, allow); err != nil {
							log.Printf("[%s] shipSvc.ConfigureShip: %v", addr, err)
						}
					}
					ccancel()
				}
			}
		}
		log.Printf("[%s] → shipSvc.ConfigureShip charID=%d shipID=%d", addr, charID, ds.shipID)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})

	case "SelfDestruct":
		log.Printf("[%s] → shipSvc.SelfDestruct charID=%d shipID=%d typeID=%d", addr, charID, ds.shipID, ds.shipTypeID)
		if err := server.SendCallRsp(c, pkt, accountID, marshal.PyNone{}); err != nil {
			return err
		}
		if ds.entity == nil || ds.solarSystem == nil {
			return nil
		}
		if ds.shipTypeID == 670 {
			handlePlayerPodKilled(ds, sqldb)
		} else {
			handlePlayerShipDestroyed(ds, sqldb, globalNPCMgr)
		}
		return nil

	default:
		log.Printf("[%s] → shipSvc.%s (None)", addr, cs.Method)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
}

// sendShipIDSessionChange sends an SCN that updates only session.shipid (the Board path).
func sendShipIDSessionChange(c *evenet.Conn, addr string, oldShipID, newShipID int64) error {
	var oldVal marshal.PyRep
	if oldShipID != 0 {
		oldVal = marshal.PyInt{Value: uint64(oldShipID)}
	} else {
		oldVal = marshal.PyNone{}
	}
	changes := marshal.PyDict{Items: []marshal.DictEntry{
		{Key: marshal.PyString{Value: "shipid"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			oldVal,
			marshal.PyInt{Value: uint64(newShipID)},
		}}},
	}}
	payload := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: 0},
			changes,
		}},
		marshal.PyList{Items: []marshal.PyRep{marshal.PyInt{Value: globalNodeID}}},
	}}
	pkt := &evenet.PyPacket{
		TypeString: "macho.SessionChangeNotification",
		Type:       evenet.SessionChangeNotification,
		Source: evenet.PyAddress{
			Type:   evenet.AddrNode,
			TypeID: globalNodeID,
		},
		Dest: evenet.PyAddress{
			Type:   evenet.AddrClient,
			TypeID: 130293001608,
		},
		UserID:  0,
		Payload: payload,
		NamedPayload: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "channel"}, Value: marshal.PyString{Value: "sessionchange"}},
		}},
	}
	b, err := marshal.Marshal(pkt.Encode())
	if err != nil {
		return err
	}
	log.Printf("[%s] → SessionChangeNotification (shipid %d→%d)", addr, oldShipID, newShipID)
	return c.SendPacket(b)
}

// boardDockedShip moves the pilot into newShipID (flagPilot 57), deletes the
// capsule they leave and sends the shipid SCN. Call it before the Board reply:
// the hangar fits turrets from session.shipid as soon as Board returns
// (station base.py FitHardpoints), so a reply ahead of the SCN draws the old
// ship's guns on the new hull. On a failed move no SCN is sent.
//
// A capsule is the active ship or nothing, never a hangar item. Every capsule
// the pilot owns and is not aboard goes (the one just vacated, and any left
// by an earlier path), and the client is told: its hangar listing still holds
// the vacated capsule, which would otherwise stay on show as an item.
func boardDockedShip(c *evenet.Conn, addr string, accountID, charID, newShipID int64, sqldb *sql.DB) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	prevShipID, _ := evedb.GetCurrentShipID(ctx, sqldb, charID)
	if _, err := sqldb.ExecContext(ctx,
		`UPDATE entity SET locationID = ?, flag = 57 WHERE itemID = ?`,
		newShipID, charID); err != nil {
		return prevShipID, fmt.Errorf("update entity: %w", err)
	}
	gone, err := evedb.DeleteSpareCapsules(ctx, sqldb, charID)
	if err != nil {
		log.Printf("[%s] Board: DeleteSpareCapsules charID=%d: %v", addr, charID, err)
	}
	scnErr := sendShipIDSessionChange(c, addr, prevShipID, newShipID)
	// After the SCN, so the client has already left the capsule when it hears it is gone.
	if c != nil {
		for _, er := range gone {
			_ = server.SendNotification(c, accountID, "OnItemChange", "charid", itemDeletedNotifArgs(er))
		}
	}
	return prevShipID, scnErr
}

// sendUndockSessionChange sends an SCN that moves the client from docked to in space: stationid,
// solarsystemid, solarsystemid2, locationid and shipid.
func sendUndockSessionChange(c *evenet.Conn, addr string, accountID uint64, stationID, sysID, shipID int64) error {
	none := marshal.PyNone{}
	changes := marshal.PyDict{Items: []marshal.DictEntry{
		{Key: marshal.PyString{Value: "stationid"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(stationID)}, none,
		}}},
		{Key: marshal.PyString{Value: "solarsystemid"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			none, marshal.PyInt{Value: uint64(sysID)},
		}}},
		{Key: marshal.PyString{Value: "solarsystemid2"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(sysID)}, marshal.PyInt{Value: uint64(sysID)},
		}}},
		{Key: marshal.PyString{Value: "locationid"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(stationID)}, marshal.PyInt{Value: uint64(sysID)},
		}}},
		{Key: marshal.PyString{Value: "shipid"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(shipID)}, marshal.PyInt{Value: uint64(shipID)},
		}}},
	}}
	payload := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: 0},
			changes,
		}},
		marshal.PyList{Items: []marshal.PyRep{marshal.PyInt{Value: globalNodeID}}},
	}}
	pkt := &evenet.PyPacket{
		TypeString: "macho.SessionChangeNotification",
		Type:       evenet.SessionChangeNotification,
		Source: evenet.PyAddress{
			Type:   evenet.AddrNode,
			TypeID: globalNodeID,
		},
		Dest: evenet.PyAddress{
			Type:   evenet.AddrClient,
			TypeID: 130293001608,
		},
		UserID:  accountID,
		Payload: payload,
		NamedPayload: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "channel"}, Value: marshal.PyString{Value: "sessionchange"}},
		}},
	}
	b, err := marshal.Marshal(pkt.Encode())
	if err != nil {
		return err
	}
	log.Printf("[%s] → SessionChangeNotification (undock stationid=%d→None solarsystemid=%d shipid=%d)", addr, stationID, sysID, shipID)
	return c.SendPacket(b)
}

// sendDockSessionChange sends an SCN that moves the client from in space to docked. solarsystemid
// is cleared and stationid and locationid are set; solarsystemid2 stays on the current system.
func sendDockSessionChange(c *evenet.Conn, addr string, accountID uint64, sysID, stationID, shipID int64) error {
	none := marshal.PyNone{}
	changes := marshal.PyDict{Items: []marshal.DictEntry{
		{Key: marshal.PyString{Value: "stationid"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			none, marshal.PyInt{Value: uint64(stationID)},
		}}},
		{Key: marshal.PyString{Value: "solarsystemid"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(sysID)}, none,
		}}},
		{Key: marshal.PyString{Value: "solarsystemid2"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(sysID)}, marshal.PyInt{Value: uint64(sysID)},
		}}},
		{Key: marshal.PyString{Value: "locationid"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(sysID)}, marshal.PyInt{Value: uint64(stationID)},
		}}},
		{Key: marshal.PyString{Value: "shipid"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(shipID)}, marshal.PyInt{Value: uint64(shipID)},
		}}},
	}}
	payload := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: 0},
			changes,
		}},
		marshal.PyList{Items: []marshal.PyRep{marshal.PyInt{Value: globalNodeID}}},
	}}
	pkt := &evenet.PyPacket{
		TypeString: "macho.SessionChangeNotification",
		Type:       evenet.SessionChangeNotification,
		Source: evenet.PyAddress{
			Type:   evenet.AddrNode,
			TypeID: globalNodeID,
		},
		Dest: evenet.PyAddress{
			Type:   evenet.AddrClient,
			TypeID: 130293001608,
		},
		UserID:  accountID,
		Payload: payload,
		NamedPayload: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "channel"}, Value: marshal.PyString{Value: "sessionchange"}},
		}},
	}
	b, err := marshal.Marshal(pkt.Encode())
	if err != nil {
		return err
	}
	log.Printf("[%s] → SessionChangeNotification (dock solarsystemid=%d→None stationid=%d shipid=%d)", addr, sysID, stationID, shipID)
	return c.SendPacket(b)
}

// sendJumpSessionChange sends an SCN for a gate jump. It always carries constellationid and
// regionid so the session holds the full location hierarchy, even within one constellation.
func sendJumpSessionChange(c *evenet.Conn, addr string, accountID uint64, oldSysID, newSysID, oldConstID, newConstID, oldRegID, newRegID int64) error {
	entries := []marshal.DictEntry{
		{Key: marshal.PyString{Value: "solarsystemid"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(oldSysID)}, marshal.PyInt{Value: uint64(newSysID)},
		}}},
		{Key: marshal.PyString{Value: "solarsystemid2"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(oldSysID)}, marshal.PyInt{Value: uint64(newSysID)},
		}}},
		{Key: marshal.PyString{Value: "locationid"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(oldSysID)}, marshal.PyInt{Value: uint64(newSysID)},
		}}},
	}
	entries = append(entries, marshal.DictEntry{
		Key: marshal.PyString{Value: "constellationid"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(oldConstID)}, marshal.PyInt{Value: uint64(newConstID)},
		}},
	})
	entries = append(entries, marshal.DictEntry{
		Key: marshal.PyString{Value: "regionid"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(oldRegID)}, marshal.PyInt{Value: uint64(newRegID)},
		}},
	})
	changes := marshal.PyDict{Items: entries}
	payload := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: 0},
			changes,
		}},
		marshal.PyList{Items: []marshal.PyRep{marshal.PyInt{Value: globalNodeID}}},
	}}
	pkt := &evenet.PyPacket{
		TypeString: "macho.SessionChangeNotification",
		Type:       evenet.SessionChangeNotification,
		Source: evenet.PyAddress{
			Type:   evenet.AddrNode,
			TypeID: globalNodeID,
		},
		Dest: evenet.PyAddress{
			Type:   evenet.AddrClient,
			TypeID: 130293001608,
		},
		UserID:  accountID,
		Payload: payload,
		NamedPayload: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "channel"}, Value: marshal.PyString{Value: "sessionchange"}},
		}},
	}
	b, err := marshal.Marshal(pkt.Encode())
	if err != nil {
		return err
	}
	log.Printf("[%s] → SessionChangeNotification (jump solarsystemid=%d→%d const=%d→%d region=%d→%d)",
		addr, oldSysID, newSysID, oldConstID, newConstID, oldRegID, newRegID)
	return c.SendPacket(b)
}

// sendRespawnSessionChange sends an SCN that moves the client from space to the home station
// after pod death (dock and jump fields combined).
func sendRespawnSessionChange(c *evenet.Conn, addr string, accountID uint64, oldSysID, homeStationID, homeSysID, homeConstID, homeRegID, oldShipID, newShipID int64) error {
	none := marshal.PyNone{}
	entries := []marshal.DictEntry{
		{Key: marshal.PyString{Value: "stationid"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			none, marshal.PyInt{Value: uint64(homeStationID)},
		}}},
		{Key: marshal.PyString{Value: "solarsystemid"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(oldSysID)}, none,
		}}},
		{Key: marshal.PyString{Value: "solarsystemid2"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(oldSysID)}, marshal.PyInt{Value: uint64(homeSysID)},
		}}},
		{Key: marshal.PyString{Value: "locationid"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(oldSysID)}, marshal.PyInt{Value: uint64(homeStationID)},
		}}},
		{Key: marshal.PyString{Value: "constellationid"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(homeConstID)}, marshal.PyInt{Value: uint64(homeConstID)},
		}}},
		{Key: marshal.PyString{Value: "regionid"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(homeRegID)}, marshal.PyInt{Value: uint64(homeRegID)},
		}}},
		{Key: marshal.PyString{Value: "shipid"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(oldShipID)}, marshal.PyInt{Value: uint64(newShipID)},
		}}},
	}
	changes := marshal.PyDict{Items: entries}
	payload := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: 0},
			changes,
		}},
		marshal.PyList{Items: []marshal.PyRep{marshal.PyInt{Value: globalNodeID}}},
	}}
	pkt := &evenet.PyPacket{
		TypeString: "macho.SessionChangeNotification",
		Type:       evenet.SessionChangeNotification,
		Source: evenet.PyAddress{
			Type:   evenet.AddrNode,
			TypeID: globalNodeID,
		},
		Dest: evenet.PyAddress{
			Type:   evenet.AddrClient,
			TypeID: 130293001608,
		},
		UserID:  accountID,
		Payload: payload,
		NamedPayload: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "channel"}, Value: marshal.PyString{Value: "sessionchange"}},
		}},
	}
	b, err := marshal.Marshal(pkt.Encode())
	if err != nil {
		return err
	}
	log.Printf("[%s] → SessionChangeNotification (respawn station=%d system=%d ship=%d→%d)", addr, homeStationID, homeSysID, oldShipID, newShipID)
	return c.SendPacket(b)
}

// buildCloneStateRep builds the (jumpClones, jumpCloneImplants) tuple for jumpCloneSvc.GetCloneState.
// The client selects jumpCloneID/locationID from the first rowset and filters the second by jumpCloneID.
func buildCloneStateRep(addr string, charID int64, sqldb *sql.DB) marshal.PyRep {
	if charID == 0 || sqldb == nil {
		return marshal.PyTuple{Items: []marshal.PyRep{marshal.PyNone{}, marshal.PyNone{}}}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	clones, err := evedb.GetJumpClones(ctx, sqldb, charID)
	if err != nil {
		log.Printf("[%s] GetCloneState GetJumpClones: %v", addr, err)
	}
	cloneRows := make([][]marshal.PyRep, len(clones))
	for i, jc := range clones {
		cloneRows[i] = []marshal.PyRep{
			marshal.PyInt{Value: uint64(jc.JumpCloneID)},
			marshal.PyInt{Value: uint64(jc.CharacterID)},
			marshal.PyInt{Value: uint64(jc.LocationID)},
		}
	}
	implants, err := evedb.GetJumpCloneImplants(ctx, sqldb, charID)
	if err != nil {
		log.Printf("[%s] GetCloneState GetJumpCloneImplants: %v", addr, err)
	}
	implantRows := make([][]marshal.PyRep, len(implants))
	for i, im := range implants {
		implantRows[i] = []marshal.PyRep{
			marshal.PyInt{Value: uint64(im.JumpCloneID)},
			marshal.PyInt{Value: uint64(im.ItemID)},
			marshal.PyInt{Value: uint64(im.TypeID)},
		}
	}
	log.Printf("[%s] → GetCloneState charID=%d clones=%d implants=%d", addr, charID, len(clones), len(implants))
	return marshal.PyTuple{Items: []marshal.PyRep{
		makeInvRowset([]string{"jumpCloneID", "ownerID", "locationID"}, cloneRows),
		makeInvRowset([]string{"jumpCloneID", "itemID", "typeID"}, implantRows),
	}}
}

// sendJumpCloneInvalidatedAt tells the client to drop its cached clone state and refetch. At a
// station it also sends OnStationJumpCloneCacheInvalidated, because the client clears its
// stationJumpClones only on that event with locationID == session.stationid (clonejumpsvc.py).
func sendJumpCloneInvalidatedAt(c *evenet.Conn, accountID, stationID, charID int64) {
	_ = server.SendNotification(c, accountID, "OnJumpCloneCacheInvalidated", "charid", marshal.PyTuple{})
	if stationID != 0 {
		_ = server.SendNotification(c, accountID, "OnStationJumpCloneCacheInvalidated", "charid",
			marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyInt{Value: uint64(stationID)},
				marshal.PyInt{Value: uint64(charID)},
			}})
	}
}

// sendCloneJumpSessionChange moves a docked client to another station (both ends docked; the ship
// becomes the new capsule).
func sendCloneJumpSessionChange(c *evenet.Conn, addr string, accountID uint64, oldStationID, newStationID, oldSysID, newSysID, newConstID, newRegID, oldShipID, newShipID int64) error {
	entries := []marshal.DictEntry{
		{Key: marshal.PyString{Value: "stationid"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(oldStationID)}, marshal.PyInt{Value: uint64(newStationID)},
		}}},
		{Key: marshal.PyString{Value: "solarsystemid2"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(oldSysID)}, marshal.PyInt{Value: uint64(newSysID)},
		}}},
		{Key: marshal.PyString{Value: "locationid"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(oldStationID)}, marshal.PyInt{Value: uint64(newStationID)},
		}}},
		{Key: marshal.PyString{Value: "constellationid"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(newConstID)}, marshal.PyInt{Value: uint64(newConstID)},
		}}},
		{Key: marshal.PyString{Value: "regionid"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(newRegID)}, marshal.PyInt{Value: uint64(newRegID)},
		}}},
		{Key: marshal.PyString{Value: "shipid"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(oldShipID)}, marshal.PyInt{Value: uint64(newShipID)},
		}}},
	}
	changes := marshal.PyDict{Items: entries}
	payload := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: 0},
			changes,
		}},
		marshal.PyList{Items: []marshal.PyRep{marshal.PyInt{Value: globalNodeID}}},
	}}
	pkt := &evenet.PyPacket{
		TypeString: "macho.SessionChangeNotification",
		Type:       evenet.SessionChangeNotification,
		Source: evenet.PyAddress{
			Type:   evenet.AddrNode,
			TypeID: globalNodeID,
		},
		Dest: evenet.PyAddress{
			Type:   evenet.AddrClient,
			TypeID: 130293001608,
		},
		UserID:  accountID,
		Payload: payload,
		NamedPayload: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "channel"}, Value: marshal.PyString{Value: "sessionchange"}},
		}},
	}
	b, err := marshal.Marshal(pkt.Encode())
	if err != nil {
		return err
	}
	log.Printf("[%s] → SessionChangeNotification (clonejump station=%d→%d system=%d→%d ship=%d→%d)",
		addr, oldStationID, newStationID, oldSysID, newSysID, oldShipID, newShipID)
	return c.SendPacket(b)
}

// handleCloneJump implements jumpCloneSvc.CloneJump(destLocationID, checked): the character swaps
// bodies with the jump clone at the destination and leaves a clone with the current implants behind.
// Gates: docked, jumpclone.cooldownHours, optional not-while-training, one clone per station
// (checked=false raises the JumpCheckWillLoseExistingClone* prompts the client repeats with checked=true).
func handleCloneJump(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID int64, stationID *int64, ds *destinyConn, sqldb *sql.DB) error {
	userErr := func(msg string) error {
		return server.SendCallException(c, pkt, accountID, server.BuildUserError(
			"CustomInfo", map[string]marshal.PyRep{"info": marshal.PyString{Value: msg}}))
	}
	if sqldb == nil || charID == 0 {
		return userErr("Not logged in.")
	}
	if *stationID == 0 || ds.dm != nil {
		return userErr("You must be docked in a station to clone jump.")
	}
	if len(cs.ArgTuple.Items) < 1 {
		return userErr("Invalid clone jump request.")
	}
	destRep, ok := cs.ArgTuple.Items[0].(marshal.PyInt)
	if !ok {
		return userErr("Invalid clone jump request.")
	}
	destStationID := int64(destRep.Value)
	// The confirm flag is a Python bool: False on the first call, True after AskAreYouSure.
	checked := len(cs.ArgTuple.Items) >= 2 && pyTruthy(cs.ArgTuple.Items[1])
	fromStationID := *stationID
	if destStationID == fromStationID {
		return userErr("You are already at that location.")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	destCloneID, _, err := evedb.GetCloneAtStation(ctx, sqldb, charID, destStationID)
	if err != nil {
		return userErr(err.Error())
	}
	if destCloneID == 0 {
		return clientMsgErr(c, pkt, accountID, "JumpNoCloneAtDestination", nil)
	}

	set := evedb.GetJumpCloneSettings(ctx, sqldb)
	if wait, werr := evedb.CloneJumpCooldownLeft(ctx, sqldb, charID, set.CooldownHours); werr == nil && wait > 0 {
		h, m := evedb.SplitCooldown(wait)
		return clientMsgErr(c, pkt, accountID, "JumpAttemptTooSoon", map[string]marshal.PyRep{
			"hours":   marshal.PyInt{Value: uint64(h)},
			"minutes": marshal.PyInt{Value: uint64(m)},
		})
	}
	if set.RefuseWhileTraining {
		if st, _ := evedb.GetSkillTraining(ctx, sqldb, charID); st != nil {
			return clientMsgErr(c, pkt, accountID, "JumpSkillInTraining", nil)
		}
	}

	destLoc, err := evedb.GetStationLocation(ctx, sqldb, destStationID)
	if err != nil {
		return userErr("Destination station not found.")
	}

	// One clone per station: the jump leaves a clone at the departure station, so an existing one
	// there is destroyed once the client has confirmed.
	depCloneID, depImplants, err := evedb.GetCloneAtStation(ctx, sqldb, charID, fromStationID)
	if err != nil {
		return userErr(err.Error())
	}
	if depCloneID != 0 {
		if !checked {
			name := "JumpCheckWillLoseExistingClone"
			if depImplants > 0 {
				name = "JumpCheckWillLoseExistingCloneAndImplants"
			}
			return server.SendCallException(c, pkt, accountID, server.BuildUserError(name, nil))
		}
		if derr := evedb.DestroyJumpClone(ctx, sqldb, charID, depCloneID, fromStationID); derr != nil {
			return refuseErr(c, pkt, accountID, derr)
		}
	}

	// Body swap in the DB. The implant set changes the pilot's attributes, so the running skill is
	// banked at the old rate and restarted at the new one, as for plug, unplug and pod.
	before := charAttrSnapshot(ctx, sqldb, charID)
	var newCloneID int64
	var parked, woken []int64
	var swapErr error
	restarted, end, err := evedb.WithTrainingRetimed(ctx, sqldb, charID, func() error {
		newCloneID, parked, woken, swapErr = evedb.CloneJumpSwap(ctx, sqldb, charID, fromStationID, destCloneID)
		return swapErr
	})
	if swapErr != nil {
		log.Printf("[%s] CloneJumpSwap charID=%d: %v", addr, charID, swapErr)
		afterImplantChange(ctx, c, accountID, charID, before, restarted, end, sqldb)
		return userErr("Clone jump failed.")
	}
	if err != nil {
		// The swap committed and only the training restart failed; the queue sweep restarts the skill.
		log.Printf("[%s] CloneJump charID=%d retime: %v", addr, charID, err)
	}
	// godma holds implants as items located at the character, so each moved implant is sent with its
	// old location: parked ones left the character, woken ones came from the destination clone.
	pushImplantMove := func(itemID, oldLocationID int64) {
		er, gerr := evedb.GetEntityRow(ctx, sqldb, itemID)
		if gerr != nil || er == nil {
			return
		}
		_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
			evedb.BuildEntityRow(er),
			marshal.PyDict{Items: []marshal.DictEntry{
				{Key: marshal.PyInt{Value: 3}, Value: marshal.PyInt{Value: uint64(oldLocationID)}}, // ixLocationID
			}},
		}})
	}
	for _, id := range parked {
		pushImplantMove(id, charID)
	}
	for _, id := range woken {
		pushImplantMove(id, destCloneID)
	}
	afterImplantChange(ctx, c, accountID, charID, before, restarted, end, sqldb)
	log.Printf("[%s] CloneJump charID=%d left clone %d at %d (%d implants), woke %d implants",
		addr, charID, newCloneID, fromStationID, len(parked), len(woken))

	// The old body and its ship stay behind; the character wakes in a fresh capsule at the destination.
	oldShipID := ds.shipID
	newCapsuleID, err := evedb.CreateCapsuleEntity(ctx, sqldb, charID, destStationID, ds.charName)
	if err != nil {
		log.Printf("[%s] CloneJump CreateCapsuleEntity: %v", addr, err)
		return userErr("Clone jump failed.")
	}
	if err := evedb.MoveCharacterIntoShip(ctx, sqldb, charID, newCapsuleID); err != nil {
		log.Printf("[%s] CloneJump MoveCharacterIntoShip: %v", addr, err)
	}
	if err := evedb.UpdateCharacterStation(ctx, sqldb, charID, destStationID, destLoc.SolarSystemID, destLoc.ConstellationID, destLoc.RegionID); err != nil {
		log.Printf("[%s] CloneJump UpdateCharacterStation: %v", addr, err)
	}

	oldSysID := ds.sysID

	// Connection state: docked at the destination, in the capsule.
	*stationID = destStationID
	updateSessionLocation(addr, destStationID, destLoc.SolarSystemID)
	if globalStationReg != nil && charID != 0 {
		globalStationReg.Join(c, destStationID, charID)
	}
	ds.shipID = newCapsuleID
	ds.shipTypeID = 670
	ds.shipGroupID = 29
	ds.shipCategoryID = 6
	ds.shipName = ""
	ds.sysID = destLoc.SolarSystemID
	ds.constellationID = destLoc.ConstellationID
	ds.regionID = destLoc.RegionID
	ds.setStateSent = false

	// Reply before the session change so the call does not hang across the transition.
	if err := server.SendCallRsp(c, pkt, accountID, marshal.PyNone{}); err != nil {
		return err
	}

	// Swap local chat when changing systems.
	if oldSysID != destLoc.SolarSystemID {
		lsc.SwapSystemChannel(c, accountID, oldSysID, destLoc.SolarSystemID, globalLSCReg, sqldb)
	}

	if err := sendCloneJumpSessionChange(c, addr, uint64(accountID), fromStationID, destStationID,
		oldSysID, destLoc.SolarSystemID, destLoc.ConstellationID, destLoc.RegionID,
		oldShipID, newCapsuleID); err != nil {
		log.Printf("[%s] sendCloneJumpSessionChange: %v", addr, err)
	}
	// The client now sits in destStationID; invalidate that station's clone list too, since the jump
	// consumed a clone there.
	sendJumpCloneInvalidatedAt(c, accountID, destStationID, charID)
	log.Printf("[%s] → CloneJump charID=%d %d→%d (system %d→%d) capsule=%d",
		addr, charID, fromStationID, destStationID, oldSysID, destLoc.SolarSystemID, newCapsuleID)
	return nil
}

// loadSystemForConn reloads the celestials and solar system entity row for sysID into ds, on a jump.
func loadSystemForConn(addr string, sqldb *sql.DB, sysID int64, ds *destinyConn) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	celestials, err := globalCelestialCache.GetCelestials(ctx, sqldb, sysID)
	if err != nil {
		return fmt.Errorf("GetCelestials sysID=%d: %w", sysID, err)
	}
	ds.celestials = celestials
	er, erErr := evedb.GetSolarSystemEntityRow(ctx, sqldb, sysID)
	if erErr != nil {
		log.Printf("[%s] loadSystemForConn: GetSolarSystemEntityRow sysID=%d: %v", addr, sysID, erErr)
		ds.solItem = marshal.PyNone{}
	} else if er != nil {
		ds.solItem = buildEntityRow(er)
	} else {
		ds.solItem = marshal.PyNone{}
	}
	constID, regID, locErr := evedb.GetSolarSystemLocation(ctx, sqldb, sysID)
	if locErr == nil {
		ds.constellationID = constID
		ds.regionID = regID
	} else {
		log.Printf("[%s] loadSystemForConn: GetSolarSystemLocation sysID=%d: %v", addr, sysID, locErr)
	}
	log.Printf("[%s] loadSystemForConn: loaded %d celestials for sysID=%d", addr, len(celestials), sysID)
	return nil
}

// charNameOf resolves a character name for gang invite dialogs ("" if unknown).
func charNameOf(sqldb *sql.DB, charID int64) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if ci, err := evedb.GetCharInfo(ctx, sqldb, charID); err == nil {
		return ci.CharName
	}
	return ""
}

// broadcastGangSysChange tells every player in charID's system that the character's gang changed
// (michelle.OnCharGangChange, plus OnGangCorpChange on join). It is also the gang bonus trigger:
// every join, leave, kick, disband and disconnect passes through, and a leave names no gang, so
// all gangs are marked dirty.
func broadcastGangSysChange(sysMgr *destiny.SystemManager, charID, gangID, corpID int64) {
	if gangID != 0 {
		markGangDirty(gangID)
	} else {
		markAllGangsDirty()
	}
	if sysMgr == nil {
		return
	}
	for _, ss := range sysMgr.All() {
		e := ss.GetPlayerByCharID(charID)
		if e == nil {
			continue
		}
		gidRep := marshal.PyRep(marshal.PyNone{})
		if gangID != 0 {
			gidRep = marshal.PyInt{Value: uint64(gangID)}
		}
		charArgs := marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(ss.ID)},
			marshal.PyInt{Value: uint64(charID)},
			gidRep,
		}}
		var corpArgs marshal.PyRep
		if gangID != 0 && corpID != 0 {
			corpArgs = marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyInt{Value: uint64(ss.ID)},
				marshal.PyInt{Value: uint64(gangID)},
				marshal.PyInt{Value: uint64(corpID)},
				marshal.PyNone{}, // timestamp, never purged
			}}
		}
		for _, p := range ss.GetAllPlayersInSystem() {
			if p.Conn == nil || p.Conn.IsClosed() {
				continue
			}
			server.SendNotification(p.Conn, int64(p.AccountID), "OnCharGangChange", "charid", charArgs) //nolint:errcheck
			if corpArgs != nil {
				server.SendNotification(p.Conn, int64(p.AccountID), "OnGangCorpChange", "charid", corpArgs) //nolint:errcheck
			}
		}
		return
	}
}

// buildGangPyData queries the gang registry for all characters in charIDs and
// returns the gangCorps and charGangs PyRep lists for SetState.
func buildGangPyData(charIDs []int64) (marshal.PyRep, marshal.PyRep) {
	if globalGangReg == nil {
		return marshal.PyList{}, marshal.PyList{}
	}
	gc, cg := globalGangReg.BubbleGangData(charIDs)
	gcItems := make([]marshal.PyRep, len(gc))
	for i, e := range gc {
		gcItems[i] = marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(e.GangID)},
			marshal.PyInt{Value: uint64(e.CorpID)},
			marshal.PyNone{}, // timestamp placeholder
		}}
	}
	cgItems := make([]marshal.PyRep, len(cg))
	for i, e := range cg {
		cgItems[i] = marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(e.CharID)},
			marshal.PyInt{Value: uint64(e.GangID)},
		}}
	}
	return marshal.PyList{Items: gcItems}, marshal.PyList{Items: cgItems}
}

// stampLeadResyncThreshold is how far the stamp stream may run ahead of the wall clock before the
// ballpark reconciler forces a resync; 0 disables it. Set from config at boot, read unsynchronised.
// See docs/main-s9-notes.md#stamp-lead-net.
var stampLeadResyncThreshold = 120

// combatDebug re-enables the per-hit combat trace (weapon hits, missile aim/launch/impact, HP pushes).
// Set from config at boot (server.combatDebug) and read unsynchronised; npc.CombatDebug is set beside it.
var combatDebug bool

// missileHardpointOffsets moves each launcher's missile ball to its per-slot offset after the
// client's LaunchMissile snap (see queueMissileLaunch). Off by default and unverified on a live
// client. Set from config at boot (server.missileHardpointOffsets).
var missileHardpointOffsets bool

// resyncBallparkCooldown spaces forced resyncs on one connection. One resync re-anchors the wall
// clock; a second inside the window means a sender is still minting stamps and a rebuild would not help.
const resyncBallparkCooldown = 60 * time.Second

// resyncBallpark rebuilds the client's ballpark with sendSetState and replays its OnTarget locks.
// Callers: the client's michelle.RequestReset (UpdateStateRequest) and the reconciler when the
// stamp lead passes the threshold. SetState replaces the ballpark wholesale and the lock UI keys
// off balls in it, so the target replay must stay.
func resyncBallpark(c *evenet.Conn, addr string, accountID uint64, ds *destinyConn) {
	sendSetState(c, addr, accountID, ds.charID, ds)

	ds.targetMu.Lock()
	resendTargets := make([]uint32, len(ds.targets))
	copy(resendTargets, ds.targets)
	ds.targetMu.Unlock()
	for _, tid := range resendTargets {
		onTargetArgs := marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyString{Value: "add"},
			marshal.PyInt{Value: uint64(tid)},
			marshal.PyNone{},
		}}
		_ = server.SendNotification(c, int64(accountID), "OnTarget", "charid", onTargetArgs)
	}

	// A mid-system SetState empties michelle's slimItems, so its DoBallsAdded re-appends balls to an
	// overview that still lists them; the overview is rebuilt once the new ballpark lands (overviewrefresh.go).
	scheduleOverviewRebuild(ds, c, int64(accountID))
}

// sendSetState assembles and sends the destiny SetState for the character's ship: celestials,
// grid contents, bubble-mates and effect states. It takes LockWire and NextStamp directly instead
// of Entity.SendStamped, so the stamp budget never gates it; SetState re-anchors the wall clock
// (SetBaseStamp) and must not be refused for drift. See docs/destiny-stamps.md#stamps-and-the-client-clock.
func sendSetState(c *evenet.Conn, addr string, accountID uint64, charID int64, ds *destinyConn) {
	dm := ds.dm
	// The wire lock spans stamp allocation and the SetState write: a lower-stamped update landing
	// after SetState would trigger another RequestReset.
	if ds.entity != nil {
		ds.entity.LockWire()
		defer ds.entity.UnlockWire()
	}
	stamp := dm.NextStamp()

	// Record base time for stamp synchronization.
	ds.destinyBaseStamp = stamp
	ds.destinyBaseTime = time.Now()

	// Sync entity stamp fields so broadcasts to this connection use correct stamps.
	if ds.entity != nil {
		ds.entity.SetBaseStamp(stamp, ds.destinyBaseTime)
	}

	// Build the player ship ball descriptor.
	shipBD := dm.BallDesc()
	shipBD.CorpID = uint32(ds.corpID)
	shipBD.Name = ds.ballName()

	// Shared celestial data from the SolarSystem when present, else per-connection data.
	var celBalls []destiny.BallDesc
	var celSlims []destiny.SlimItem
	var celDmgMap map[uint32]destiny.DamageState
	if ds.solarSystem != nil {
		celBalls = ds.solarSystem.CelBalls
		celSlims = ds.solarSystem.CelSlims
		celDmgMap = ds.solarSystem.SnapshotCelDamage()
	} else {
		celBalls = makeCelestialBallDescs(ds.celestials)
		celSlims = makeCelestialSlimItems(ds.celestials)
		celDmgMap = make(map[uint32]destiny.DamageState, len(ds.celestials))
		celDmg := destiny.DamageState{Shield: 0.75, Tau: 100000, Armor: 0.95, Structure: 1.0}
		for _, cel := range ds.celestials {
			celDmgMap[uint32(cel.ItemID)] = celDmg
		}
	}

	// Bubble-mate ships (players only; NPCs are collected below).
	var mateBalls []destiny.BallDesc
	var mateSlims []destiny.SlimItem
	mateDmg := make(map[uint32]destiny.DamageState)
	if ds.solarSystem != nil && ds.entity != nil {
		mates := ds.solarSystem.GetBubblePlayersExcept(ds.entity.ID)
		for _, mate := range mates {
			if mate.IsCloaked() {
				continue
			}
			bd := mate.BallDesc()
			bd.CorpID = uint32(mate.Slim.CorpID)
			bd.Name = mate.BallName()
			mateBalls = append(mateBalls, bd)
			mateSlims = append(mateSlims, mate.Slim)
			if mate.HasHP() {
				mateDmg[mate.ID] = mate.DamageStateFrac()
			} else {
				mateDmg[mate.ID] = mate.Damage
			}
		}
	}

	// Collect NPC bots in this bubble (rats spawned at belts).
	var npcBalls []destiny.BallDesc
	var npcSlims []destiny.SlimItem
	npcDmg := make(map[uint32]destiny.DamageState)
	if ds.solarSystem != nil && ds.entity != nil {
		for _, n := range ds.solarSystem.GetBubbleNPCs(ds.entity.ID) {
			bd := n.BallDesc()
			bd.CorpID = uint32(n.Slim.CorpID)
			bd.Name = n.BallName()
			// Mid-warp balls are snapped for the whole blob in sanitizeSetStateBalls below, players included.
			npcBalls = append(npcBalls, bd)
			npcSlims = append(npcSlims, n.Slim)
			if n.HasHP() {
				d := n.DamageStateFrac()
				d.Timestamp = server.Win32TimeNow()
				npcDmg[n.ID] = d
			} else {
				npcDmg[n.ID] = n.Damage
			}
		}
	}

	// Collect only nearby asteroids (within bubble range of the player).
	var asteroidBalls []destiny.BallDesc
	var asteroidSlims []destiny.SlimItem
	var asteroidDmg map[uint32]destiny.DamageState
	if ds.solarSystem != nil {
		asteroidBalls, asteroidSlims, asteroidDmg = ds.solarSystem.NearbyAsteroids(dm.Pos)
	}

	// Control towers are system-wide; everything anchored around one is grid-local, like asteroids
	// (destiny.StarbaseBallVisibleFrom).
	var starbaseBalls []destiny.BallDesc
	var starbaseSlims []destiny.SlimItem
	if ds.solarSystem != nil {
		starbaseBalls, starbaseSlims = ds.solarSystem.VisibleStarbases(dm.Pos)
	}

	// Billboards near stargates, within bubble range only.
	var billboardBalls []destiny.BallDesc
	var billboardSlims []destiny.SlimItem
	var billboardDmg map[uint32]destiny.DamageState
	if ds.solarSystem != nil {
		billboardBalls, billboardSlims, billboardDmg = ds.solarSystem.NearbyBillboards(dm.Pos)
	}

	// Loot containers are grid-local like asteroids and billboards; an unfiltered list puts every can
	// in the system on this client's overview.
	var lootBalls []destiny.BallDesc
	var lootSlims []destiny.SlimItem
	var lootDmg map[uint32]destiny.DamageState
	if ds.solarSystem != nil {
		lootBalls, lootSlims, lootDmg = ds.solarSystem.NearbyLootContainers(dm.Pos)
	}

	// Combine all balls: celestials, asteroids, starbases, billboards, loot, then bubble-mates, NPCs, then player ship.
	allBalls := make([]destiny.BallDesc, 0, len(celBalls)+len(asteroidBalls)+len(starbaseBalls)+len(billboardBalls)+len(lootBalls)+len(mateBalls)+len(npcBalls)+1)
	allBalls = append(allBalls, celBalls...)
	allBalls = append(allBalls, asteroidBalls...)
	allBalls = append(allBalls, starbaseBalls...)
	allBalls = append(allBalls, billboardBalls...)
	allBalls = append(allBalls, lootBalls...)
	allBalls = append(allBalls, mateBalls...)
	allBalls = append(allBalls, npcBalls...)
	allBalls = append(allBalls, shipBD)
	sanitizeSetStateBalls(allBalls, shipBD.EntityID)
	blob := destiny.EncodeSetState(stamp, allBalls)

	// ds.entity is nil when SetState is reached without registerUndock (SelectCharacter to
	// GetFormations in space with a failed sysID lookup); the slim then carries no modules.
	var shipModules []destiny.ModuleRef
	if ds.entity != nil {
		shipModules = ds.entity.Slim.Modules
	}
	shipSlim := destiny.SlimItem{
		BallID:         ds.shipID,
		ItemID:         ds.shipID,
		TypeID:         ds.shipTypeID,
		GroupID:        ds.shipGroupID,
		CategoryID:     ds.shipCategoryID,
		OwnerID:        charID,
		CorpID:         ds.corpID,
		AllianceID:     ds.allianceID,
		CharID:         charID,
		SecurityStatus: ds.securityStatus,
		Bounty:         ds.bounty,
		Modules:        shipModules,
	}
	allSlims := make([]destiny.SlimItem, 0, len(celSlims)+len(asteroidSlims)+len(starbaseSlims)+len(billboardSlims)+len(lootSlims)+len(mateSlims)+len(npcSlims)+1)
	allSlims = append(allSlims, celSlims...)
	allSlims = append(allSlims, asteroidSlims...)
	allSlims = append(allSlims, starbaseSlims...)
	allSlims = append(allSlims, billboardSlims...)
	allSlims = append(allSlims, lootSlims...)
	allSlims = append(allSlims, mateSlims...)
	allSlims = append(allSlims, npcSlims...)
	allSlims = append(allSlims, shipSlim)

	// Damage states.
	var starbaseDmg map[uint32]destiny.DamageState
	if ds.solarSystem != nil {
		starbaseDmg = ds.solarSystem.SnapshotStarbaseDamage()
	}
	dmgStates := make(map[uint32]destiny.DamageState, len(celDmgMap)+len(asteroidDmg)+len(starbaseDmg)+len(billboardDmg)+len(mateDmg)+len(npcDmg)+1)
	for id, d := range celDmgMap {
		dmgStates[id] = d
	}
	for id, d := range asteroidDmg {
		dmgStates[id] = d
	}
	for _, b := range starbaseBalls {
		if d, ok := starbaseDmg[b.EntityID]; ok {
			dmgStates[b.EntityID] = d
		}
	}
	for id, d := range billboardDmg {
		dmgStates[id] = d
	}
	for id, d := range lootDmg {
		dmgStates[id] = d
	}
	for id, d := range mateDmg {
		dmgStates[id] = d
	}
	for id, d := range npcDmg {
		dmgStates[id] = d
	}
	if ds.entity != nil && ds.entity.HasHP() {
		shipDmg := ds.entity.DamageStateFrac()
		shipDmg.Timestamp = server.Win32TimeNow()
		dmgStates[uint32(dm.EntityID)] = shipDmg
	} else if ds.entity != nil {
		dmgStates[uint32(dm.EntityID)] = ds.entity.Damage
	} else {
		dmgStates[uint32(dm.EntityID)] = fullDamageState()
	}

	solItem := ds.solItem
	if ds.solarSystem != nil && ds.solarSystem.SolItem != nil {
		solItem = ds.solarSystem.SolItem
	}
	if solItem == nil {
		solItem = marshal.PyNone{}
	}
	// Active effects for effectStates, the pilot's own modules first. A rebuild (UpdateStateRequest)
	// discards the client's ballpark while modules keep cycling, so own effects must be included or
	// the pilot's guns, miners and reps fire without visuals.
	var bubbleEffects []destiny.ActiveEffect
	if ds.entity != nil {
		bubbleEffects = append(bubbleEffects, ds.entity.GetActiveEffects()...)
	}
	if ds.solarSystem != nil && ds.entity != nil {
		for _, mate := range ds.solarSystem.GetBubblePlayersExcept(ds.entity.ID) {
			bubbleEffects = append(bubbleEffects, mate.GetActiveEffects()...)
		}
		for _, n := range ds.solarSystem.GetBubbleNPCs(ds.entity.ID) {
			bubbleEffects = append(bubbleEffects, n.GetActiveEffects()...)
		}
	}
	// Gather aggressors from the player's entity for red-box display.
	var playerAggressors map[uint32]int64
	if ds.entity != nil {
		playerAggressors = ds.entity.GetAggressors()
	}
	// Build gang data for all characters in the bubble.
	bubbleCharIDs := []int64{charID}
	if ds.solarSystem != nil && ds.entity != nil {
		for _, mate := range ds.solarSystem.GetBubblePlayersExcept(ds.entity.ID) {
			bubbleCharIDs = append(bubbleCharIDs, mate.Slim.CharID)
		}
	}
	gangCorps, charGangs := buildGangPyData(bubbleCharIDs)
	if err := destiny.SendSetState(c, globalNodeID, accountID,
		uint32(dm.EntityID), stamp, blob, allSlims, dmgStates, solItem, bubbleEffects, playerAggressors,
		aggressionSnapshotFor(ds.sysID), gangCorps, charGangs,
		droneRowsForSetState(ds.sysID, charID, int64(dm.EntityID))); err != nil {
		log.Printf("[%s] destiny SendSetState: %v", addr, err)
		return
	}
	// Turn the moon harvesters just presented toward their moon, on the SetState stamp (the wire lock is held).
	if ds.solarSystem != nil {
		if acts := ds.solarSystem.StructureFacingActions(starbaseBalls, starbaseSlims); len(acts) > 0 {
			if err := destiny.SendActionList(c, globalNodeID, accountID, stamp, acts...); err != nil {
				log.Printf("[%s] structure facings after SetState: %v", addr, err)
			}
		}
	}
	// Record the balls this client now holds; broadcasts naming other balls are gated on this set
	// (see ensureBallsKnown).
	if ds.entity != nil {
		known := make([]uint32, len(allBalls))
		for i, b := range allBalls {
			known[i] = b.EntityID
		}
		ds.entity.ResetKnownBalls(known)
	}
	log.Printf("[%s] → destiny SetState shipID=%d stamp=%d celestials=%d bubbleMates=%d npcs=%d effects=%d",
		addr, ds.shipID, stamp, len(celBalls), len(mateBalls), len(npcBalls), len(bubbleEffects))
}

// ballparkKeepReasons maps every ball the caller's grid legitimately holds to a word naming why.
// Celestials and towers always stay; asteroids, billboards, wrecks and POS structures stay within
// the landing bubble, the radius the arrival batch and the departure sweep use. The stale-ball
// prune and the /ghosts diagnostic both read it. The bool is false when there is no grid to ask about.
func ballparkKeepReasons(ds *destinyConn) (map[uint32]string, bool) {
	if ds == nil || ds.entity == nil || ds.solarSystem == nil || ds.dm == nil {
		return nil, false
	}
	ss := ds.solarSystem
	// Entity.Pos() is the synchronised read: the reconciler calls this from its own goroutine while
	// the system tick writes Manager.Pos.
	pos := ds.entity.Pos()
	rSq := destiny.BubbleRadius * destiny.BubbleRadius

	keep := map[uint32]string{ds.entity.ID: "self"}
	for _, b := range ss.CelBalls {
		keep[b.EntityID] = "celestial"
	}
	sbBalls, _ := ss.VisibleStarbases(pos)
	for _, b := range sbBalls {
		keep[b.EntityID] = "starbase"
	}
	// Everything sharing the landing bubble: players, NPCs, drones, missiles.
	for _, e := range ss.GetBubbleEntitiesExcept(ds.entity.ID) {
		keep[e.ID] = "in-bubble"
	}
	astBalls, _, _ := ss.NearbyAsteroids(pos)
	for _, b := range astBalls {
		keep[b.EntityID] = "asteroid"
	}
	bbBalls, _, _ := ss.NearbyBillboards(pos)
	for _, b := range bbBalls {
		keep[b.EntityID] = "billboard"
	}
	lootBalls, _, _ := ss.GetLootContainers()
	for _, b := range lootBalls {
		if b.Pos.Sub(pos).LenSq() <= rSq {
			keep[b.EntityID] = "loot"
		}
	}
	// Rigid dungeon balls of the room loaded for this player; they are client-only, so the scans above never see them.
	for eid := range ds.dunObjSnapshot() {
		keep[eid] = "dungeon"
	}
	return keep, true
}

// staleBallparkCandidates returns the balls this client holds that ballparkKeepReasons does not
// list, plus the ones spared because their entity is mid-warp. It does not claim from the known
// set, so the reconciler can see a ball stale twice before acting; callers that act at once use
// staleBallparkIDs. The spared list needs a bound: see docs/main-s9-notes.md#warp-spare-bound.
func staleBallparkCandidates(ds *destinyConn) (stale, warpSpared []uint32) {
	keep, ok := ballparkKeepReasons(ds)
	if !ok {
		return nil, nil
	}
	ss := ds.solarSystem
	for _, id := range ds.entity.KnownBallIDs() {
		if keep[id] != "" {
			continue
		}
		// A mid-warp ball reads stale for its whole tunnel: warp engage takes it out of its bubble before
		// its depart path sends the RemoveBalls that ends the warp-out animation. Pruning it would cut that short.
		if e := ss.GetEntity(id); e != nil && (e.IsInWarp() || e.IsWarpPending()) {
			warpSpared = append(warpSpared, id)
			continue
		}
		stale = append(stale, id)
	}
	return stale, warpSpared
}

// staleBallparkIDs is staleBallparkCandidates plus the claim: it drops the
// stale balls from the known set and returns the subset the client actually
// still held, ready to be put on the wire.
func staleBallparkIDs(ds *destinyConn) []uint32 {
	stale, _ := staleBallparkCandidates(ds)
	if len(stale) == 0 {
		return nil
	}
	return ds.entity.ClaimKnownBalls(stale...)
}

// buildWarpArrivalBalls collects dynamic entities (bubble-mates, NPCs,
// asteroids, billboards) at the warp destination, sanitizes mode references,
// and returns the data for batching into a single destiny update. Celestials
// and starbases persist through intra-system warp and are not included.
func buildWarpArrivalBalls(ds *destinyConn) ([]destiny.BallDesc, []destiny.SlimItem, map[uint32]destiny.DamageState) {
	if ds.solarSystem == nil || ds.entity == nil {
		return nil, nil, nil
	}
	dm := ds.dm

	var allBalls []destiny.BallDesc
	var allSlims []destiny.SlimItem
	allDmg := make(map[uint32]destiny.DamageState)

	// Pre-spawned dungeon room (warp-arrival pre-spawn). Its NPCs are already in the destination
	// bubble, so the bubble scan skips them; the pending slims carry the dungeon metadata the bubble entities lack.
	pendingIDs := make(map[uint32]bool, len(ds.dunPendingBalls))
	for _, b := range ds.dunPendingBalls {
		pendingIDs[b.EntityID] = true
	}

	// Bubble-mate players at destination.
	mates := ds.solarSystem.GetBubblePlayersExcept(ds.entity.ID)
	for _, mate := range mates {
		bd := mate.BallDesc()
		bd.CorpID = uint32(mate.Slim.CorpID)
		bd.Name = mate.BallName()
		allBalls = append(allBalls, bd)
		allSlims = append(allSlims, mate.Slim)
		if mate.HasHP() {
			allDmg[mate.ID] = mate.DamageStateFrac()
		} else {
			allDmg[mate.ID] = mate.Damage
		}
	}

	// NPCs in destination bubble.
	npcs := ds.solarSystem.GetBubbleNPCs(ds.entity.ID)
	for _, n := range npcs {
		if pendingIDs[n.ID] {
			continue
		}
		bd := n.BallDesc()
		bd.CorpID = uint32(n.Slim.CorpID)
		bd.Name = n.BallName()
		allBalls = append(allBalls, bd)
		allSlims = append(allSlims, n.Slim)
		if n.HasHP() {
			d := n.DamageStateFrac()
			d.Timestamp = server.Win32TimeNow()
			allDmg[n.ID] = d
		} else {
			allDmg[n.ID] = n.Damage
		}
	}

	// Nearby asteroids at landing position.
	astBalls, astSlims, astDmg := ds.solarSystem.NearbyAsteroids(dm.Pos)
	allBalls = append(allBalls, astBalls...)
	allSlims = append(allSlims, astSlims...)
	for id, d := range astDmg {
		allDmg[id] = d
	}

	// Nearby billboards at landing position.
	bbBalls, bbSlims, bbDmg := ds.solarSystem.NearbyBillboards(dm.Pos)
	allBalls = append(allBalls, bbBalls...)
	allSlims = append(allSlims, bbSlims...)
	for id, d := range bbDmg {
		allDmg[id] = d
	}

	// Loot containers inside the landing bubble. The depart sweep strips the cans of the grid being
	// left, so a pilot warping back to a half-looted room needs them re-sent.
	lootBalls, lootSlims, lootDmg := ds.solarSystem.GetLootContainers()
	lootRSq := destiny.BubbleRadius * destiny.BubbleRadius
	for i, lb := range lootBalls {
		if lb.Pos.Sub(dm.Pos).LenSq() > lootRSq {
			continue
		}
		allBalls = append(allBalls, lb)
		allSlims = append(allSlims, lootSlims[i])
		if d, ok := lootDmg[lb.EntityID]; ok {
			allDmg[lb.EntityID] = d
		}
	}

	// POS structures on the arrival grid (guns, arrays, hangars, force field). The tower is
	// system-wide and already held; the depart sweep strips these when the pilot leaves.
	sbBalls, sbSlims := ds.solarSystem.NearbyGridStarbases(dm.Pos)
	if len(sbBalls) > 0 {
		sbDmg := ds.solarSystem.SnapshotStarbaseDamage()
		for i, b := range sbBalls {
			if ds.entity.KnowsBall(b.EntityID) {
				continue
			}
			allBalls = append(allBalls, b)
			allSlims = append(allSlims, sbSlims[i])
			if d, ok := sbDmg[b.EntityID]; ok {
				allDmg[b.EntityID] = d
			} else {
				allDmg[b.EntityID] = fullDamageState()
			}
		}
	}

	// Consume the pre-spawned dungeon room.
	if len(ds.dunPendingBalls) > 0 {
		allBalls = append(allBalls, ds.dunPendingBalls...)
		allSlims = append(allSlims, ds.dunPendingSlims...)
		for id, d := range ds.dunPendingDmg {
			allDmg[id] = d
		}
		ds.dunPendingBalls = nil
		ds.dunPendingSlims = nil
		ds.dunPendingDmg = nil
	}

	if len(allBalls) == 0 {
		return nil, nil, nil
	}

	// BallDescForObserver demotes ModeWarp and any follow/orbit/missile reference the client cannot resolve
	// (BallNotInPark aborts the AddBalls). See docs/destiny-arrival-lanes.md#first-sight-balls.
	batchSet := make(map[uint32]struct{}, len(allBalls))
	for _, b := range allBalls {
		batchSet[b.EntityID] = struct{}{}
	}
	// A reference is satisfied by this batch or by what the client holds; only this site knows the
	// batch, hence the closure.
	knows := func(ref uint32) bool {
		if _, inBatch := batchSet[ref]; inBatch {
			return true
		}
		return ds.entity != nil && ds.entity.KnowsBall(ref)
	}
	for i := range allBalls {
		allBalls[i] = destiny.BallDescForObserver(allBalls[i], knows)
	}
	// Sort so dependencies (follow/orbit targets) appear before
	// referencing balls in the binary stream.
	allBalls, allSlims = sortBallsByDependency(allBalls, allSlims)

	return allBalls, allSlims, allDmg
}

// sendSetStateToEntity sends a fresh SetState to an existing player entity,
// rebuilding their full ballpark view. Used for cross-system jumps, undocking,
// initial spawn, and when a new player appears in system.
func sendSetStateToEntity(mate *destiny.Entity, ss *destiny.SolarSystem) {
	// Wire lock across stamp allocation → SetState write (see sendSetState).
	mate.LockWire()
	defer mate.UnlockWire()
	stamp := mate.NextStamp()
	now := time.Now()
	mate.SetBaseStamp(stamp, now)

	// Build the mate's own ship ball.
	shipBD := mate.BallDesc()
	shipBD.CorpID = uint32(mate.Slim.CorpID)
	shipBD.Name = mate.BallName()

	// The mate's bubble-mates (players only; NPCs are collected below).
	var mateBalls []destiny.BallDesc
	var mateSlims []destiny.SlimItem
	mateDmg := make(map[uint32]destiny.DamageState)
	others := ss.GetBubblePlayersExcept(mate.ID)
	for _, other := range others {
		bd := other.BallDesc()
		bd.CorpID = uint32(other.Slim.CorpID)
		bd.Name = other.BallName()
		mateBalls = append(mateBalls, bd)
		mateSlims = append(mateSlims, other.Slim)
		if other.HasHP() {
			mateDmg[other.ID] = other.DamageStateFrac()
		} else {
			mateDmg[other.ID] = other.Damage
		}
	}

	// NPCs in this mate's bubble.
	var npcBalls []destiny.BallDesc
	var npcSlims []destiny.SlimItem
	npcDmg := make(map[uint32]destiny.DamageState)
	for _, n := range ss.GetBubbleNPCs(mate.ID) {
		bd := n.BallDesc()
		bd.CorpID = uint32(n.Slim.CorpID)
		bd.Name = n.BallName()
		npcBalls = append(npcBalls, bd)
		npcSlims = append(npcSlims, n.Slim)
		if n.HasHP() {
			d := n.DamageStateFrac()
			d.Timestamp = server.Win32TimeNow()
			npcDmg[n.ID] = d
		} else {
			npcDmg[n.ID] = n.Damage
		}
	}

	// Only include asteroids near this mate's position.
	nearAstBalls, nearAstSlims, nearAstDmg := ss.NearbyAsteroids(mate.Pos())

	// Only include billboards near this mate's position.
	nearBBBalls, nearBBSlims, nearBBDmg := ss.NearbyBillboards(mate.Pos())

	// Loot containers are grid-local (see sendSetState).
	lootBalls, lootSlims, lootDmg := ss.NearbyLootContainers(mate.Pos())

	// Towers system-wide, the structures around them grid-local.
	sbBalls, sbSlims := ss.VisibleStarbases(mate.Pos())

	// Combine: celestials + nearby asteroids + starbases + nearby billboards + loot + bubble-mates + NPCs + own ship.
	allBalls := make([]destiny.BallDesc, 0, len(ss.CelBalls)+len(nearAstBalls)+len(sbBalls)+len(nearBBBalls)+len(lootBalls)+len(mateBalls)+len(npcBalls)+1)
	allBalls = append(allBalls, ss.CelBalls...)
	allBalls = append(allBalls, nearAstBalls...)
	allBalls = append(allBalls, sbBalls...)
	allBalls = append(allBalls, nearBBBalls...)
	allBalls = append(allBalls, lootBalls...)
	allBalls = append(allBalls, mateBalls...)
	allBalls = append(allBalls, npcBalls...)
	allBalls = append(allBalls, shipBD)
	sanitizeSetStateBalls(allBalls, shipBD.EntityID)
	blob := destiny.EncodeSetState(stamp, allBalls)

	allSlims := make([]destiny.SlimItem, 0, len(ss.CelSlims)+len(nearAstSlims)+len(sbSlims)+len(nearBBSlims)+len(lootSlims)+len(mateSlims)+len(npcSlims)+1)
	allSlims = append(allSlims, ss.CelSlims...)
	allSlims = append(allSlims, nearAstSlims...)
	allSlims = append(allSlims, sbSlims...)
	allSlims = append(allSlims, nearBBSlims...)
	allSlims = append(allSlims, lootSlims...)
	allSlims = append(allSlims, mateSlims...)
	allSlims = append(allSlims, npcSlims...)
	allSlims = append(allSlims, mate.Slim)

	celDmg := ss.SnapshotCelDamage()
	sbDmg := ss.SnapshotStarbaseDamage()
	dmgStates := make(map[uint32]destiny.DamageState, len(celDmg)+len(nearAstDmg)+len(sbDmg)+len(nearBBDmg)+len(mateDmg)+len(npcDmg)+1)
	for id, d := range celDmg {
		dmgStates[id] = d
	}
	for id, d := range nearAstDmg {
		dmgStates[id] = d
	}
	for _, b := range sbBalls {
		if d, ok := sbDmg[b.EntityID]; ok {
			dmgStates[b.EntityID] = d
		}
	}
	for id, d := range nearBBDmg {
		dmgStates[id] = d
	}
	for id, d := range lootDmg {
		dmgStates[id] = d
	}
	for id, d := range mateDmg {
		dmgStates[id] = d
	}
	for id, d := range npcDmg {
		dmgStates[id] = d
	}
	if mate.HasHP() {
		mateOwnDmg := mate.DamageStateFrac()
		mateOwnDmg.Timestamp = server.Win32TimeNow()
		dmgStates[mate.ID] = mateOwnDmg
	} else {
		dmgStates[mate.ID] = mate.Damage
	}

	solItem := marshal.PyRep(marshal.PyNone{})
	if ss.SolItem != nil {
		solItem = ss.SolItem
	}

	// Active effects for effectStates, the pilot's own modules first (see sendSetState), then the grid.
	bubbleEffects := append([]destiny.ActiveEffect(nil), mate.GetActiveEffects()...)
	for _, other := range ss.GetBubblePlayersExcept(mate.ID) {
		bubbleEffects = append(bubbleEffects, other.GetActiveEffects()...)
	}
	for _, n := range ss.GetBubbleNPCs(mate.ID) {
		bubbleEffects = append(bubbleEffects, n.GetActiveEffects()...)
	}
	mateAggressors := mate.GetAggressors()
	// Build gang data for bubble characters.
	mateCharIDs := []int64{mate.Slim.CharID}
	for _, other := range ss.GetBubblePlayersExcept(mate.ID) {
		mateCharIDs = append(mateCharIDs, other.Slim.CharID)
	}
	mateGangCorps, mateCharGangs := buildGangPyData(mateCharIDs)
	if err := destiny.SendSetState(mate.Conn, globalNodeID, mate.AccountID,
		mate.ID, stamp, blob, allSlims, dmgStates, solItem, bubbleEffects, mateAggressors,
		aggressionSnapshotFor(ss.ID), mateGangCorps, mateCharGangs,
		droneRowsForSetState(ss.ID, mate.Slim.CharID, int64(mate.ID))); err != nil {
		log.Printf("[%s] sendSetStateToEntity: %v", mate.Addr, err)
		return
	}
	// Moon harvesters face their moon (see sendSetState).
	if acts := ss.StructureFacingActions(sbBalls, sbSlims); len(acts) > 0 {
		if err := destiny.SendActionList(mate.Conn, globalNodeID, mate.AccountID, stamp, acts...); err != nil {
			log.Printf("[%s] structure facings after SetState: %v", mate.Addr, err)
		}
	}
	known := make([]uint32, len(allBalls))
	for i, b := range allBalls {
		known[i] = b.EntityID
	}
	mate.ResetKnownBalls(known)
	log.Printf("[%s] → SetState refresh entity=%d celestials=%d bubbleMates=%d effects=%d",
		mate.Addr, mate.ID, len(ss.CelBalls), len(mateBalls), len(bubbleEffects))
}

// gateCloakDuration is how long the arrival cloak lasts if the pilot does nothing. server_settings
// gate.cloakSec, default 60.
func gateCloakDuration() time.Duration {
	return evedb.CachedSettingDuration("gate.cloakSec", time.Second, 60, 5, 600)
}

// armGateCloakExpiry schedules the arrival cloak to lapse on its own. Without a timer a pilot who
// jumps in and touches nothing stays cloaked: invisible to bubble-mates, unlockable and ignored by NPCs.
// The callback runs on the tick goroutine and re-checks ds.entity == e first, because a second
// jump replaces the entity and a stale timer must not decloak the new ship.
func armGateCloakExpiry(ds *destinyConn) {
	if globalTickMgr == nil || ds == nil || ds.entity == nil {
		return
	}
	// A standing GM cloak has no expiry.
	if ds.entity.IsGMCloak() {
		return
	}
	e := ds.entity
	globalTickMgr.ScheduleOnce(fmt.Sprintf("gate-cloak-%d", e.ID),
		time.Now().Add(gateCloakDuration()), func() {
			if ds.entity != e || !e.IsCloaked() {
				return // jumped, docked or undocked again, or already decloaked
			}
			if e.Conn == nil || e.Conn.IsClosed() {
				return
			}
			log.Printf("[%s] gate cloak expired after %s", e.Addr, gateCloakDuration())
			breakGateCloak(ds)
		})
}

// breakGateCloak removes the gate cloak from ds and reveals the player to bubble-mates. Callers:
// movement handlers (Stop, Goto, Follow, Orbit, Warp), module activation and the expiry timer.
// No-op when the player is not cloaked.
func breakGateCloak(ds *destinyConn) {
	if ds.entity == nil || !ds.entity.IsCloaked() {
		return
	}
	// The GM's standing cloak never breaks here: the callers above ask about the arrival cloak.
	// /gm cloak off clears the flag first and then comes through here.
	if ds.entity.IsGMCloak() {
		return
	}
	// A cloaking device holds its cloak through movement; only the module stopping ends it
	// (disengageModuleCloak).
	if ds.entity.ModuleCloak() != 0 {
		return
	}
	revealCloakedShip(ds)
}

// revealCloakedShip clears the cloaked flag and shows the ship again: the pilot's own client
// gets UncloakBall and the uncloak effect, bubble-mates get the ball back and the effect. It is
// the reveal half of every cloak (gate, cloaking device, GM cloak off). No-op when the ship is
// not cloaked.
func revealCloakedShip(ds *destinyConn) {
	if ds.entity == nil || ds.dm == nil || !ds.entity.IsCloaked() {
		return
	}
	e := ds.entity
	e.SetCloaked(false)
	entityIDRep := marshal.PyInt{Value: uint64(ds.dm.EntityID)}

	// UncloakBall clears ball.isCloaked (the autopilot refuses StargateJump while it is set); the Uncloak FX and a
	// self-only BoosterStop go to self. See docs/engine-fx.md#gate-cloak.
	uncloakActions := []marshal.PyRep{
		destiny.BuildSimpleAction("UncloakBall", entityIDRep),
		destiny.BuildSimpleAction("OnSpecialFX",
			entityIDRep,
			marshal.PyNone{}, marshal.PyNone{}, marshal.PyNone{}, marshal.PyNone{},
			marshal.PyList{},
			marshal.PyString{Value: "effects.Uncloak"},
			marshal.PyInt{Value: 0}, marshal.PyInt{Value: 1}, marshal.PyInt{Value: 0},
		),
	}
	if ds.shipCategoryID == categoryShipBall {
		uncloakActions = append(uncloakActions, destiny.BuildSimpleAction("OnSpecialFX",
			entityIDRep,
			marshal.PyNone{}, marshal.PyNone{}, marshal.PyNone{}, marshal.PyNone{},
			marshal.PyList{},
			marshal.PyString{Value: dogma.FXGUIDBoosterStop},
			marshal.PyInt{Value: 0}, marshal.PyInt{Value: 1}, marshal.PyInt{Value: 0},
		))
	}
	ds.sendSelfStamped(func(stamp uint32) error {
		return destiny.SendActionList(e.Conn, globalNodeID, e.AccountID, stamp, uncloakActions...)
	})

	// Reveal to bubble-mates: AddBalls so they can see the ship, then the Uncloak FX.
	if ds.solarSystem != nil {
		mates := ds.solarSystem.GetBubbleConnectedPlayersExcept(e.ID)
		for _, mate := range mates {
			if mate.Conn.IsClosed() {
				continue
			}
			if !ensureBallsKnown(mate, e) {
				continue
			}
			mate.SendStamped(func(stamp uint32) error {
				return destiny.SendSimpleAction(mate.Conn, globalNodeID, mate.AccountID, stamp,
					"OnSpecialFX",
					entityIDRep,
					marshal.PyNone{}, marshal.PyNone{}, marshal.PyNone{}, marshal.PyNone{},
					marshal.PyList{},
					marshal.PyString{Value: "effects.Uncloak"},
					marshal.PyInt{Value: 0}, marshal.PyInt{Value: 1}, marshal.PyInt{Value: 0},
				)
			})
		}
	}
}

// deadspacePocketRadius is how far a loaded room's natural phenomena reach. A warp target inside
// it is a warp within the pocket and is refused; a target beyond it (station, planet, far gate) works.
const deadspacePocketRadius = 5_000_000.0 // metres

// inDeadspacePocket reports whether the player is sitting inside a loaded room whose phenomena are
// on. A room stays loaded on the connection after the pilot docks or jumps out so a site can be
// re-entered, so this is a position test and not a dunRoomID check.
func inDeadspacePocket(ds *destinyConn) bool {
	if ds == nil || !ds.dunNoWarp || ds.dm == nil || ds.dunRoomID == 0 {
		return false
	}
	anchor := ds.dunRoomAnchor.Add(ds.dunOffset)
	return ds.dm.Pos.Sub(anchor).Len() <= deadspacePocketRadius
}

// dungeonWarpBlocked reports whether the loaded room's natural phenomena refuse a warp to dest.
// Acceleration gates do not route through here.
func dungeonWarpBlocked(ds *destinyConn, dest destiny.Vec3) bool {
	if !inDeadspacePocket(ds) {
		return false
	}
	return dest.Sub(ds.dm.Pos).Len() <= deadspacePocketRadius
}

// warpNavLocked reports whether ds is inside the warp tunnel. The client keeps sending nav
// commands mid-warp, and echoing one drops the C++ ball out of ModeWarp while it keeps warp
// velocity, so the ship free-falls across the system at warp speed. Nav handlers refuse while this holds.
func warpNavLocked(ds *destinyConn) bool {
	return ds.entity != nil && ds.entity.IsInWarp()
}

// abortPendingWarp cancels a warp still aligning. Steering away or stopping before the tunnel
// engages aborts the warp, and the warp goroutine must be told or it engages on a ship pointed
// elsewhere. No-op once the tunnel is up (warpNavLocked rejects those commands first).
func abortPendingWarp(ds *destinyConn) {
	if ds.entity == nil || !ds.entity.IsWarpPending() {
		return
	}
	if ds.cancelWarp != nil {
		ds.cancelWarp()
		ds.cancelWarp = nil
	}
	ds.entity.SetWarpPending(false)
}

// broadcastAction sends a destiny action to all player entities in the same bubble as ds,
// excluding self. Nav actions coalesce through each recipient's queue (see broadcastActionList).
func broadcastAction(ds *destinyConn, funcName string, args ...marshal.PyRep) {
	broadcastActionList(ds, destiny.BuildSimpleAction(funcName, args...))
}

// broadcastFXAction sends a stamp-agnostic action (OnSpecialFX and friends) to every bubble mate
// without minting a stamp: michelle.RealFlushState applies these directly, so a stamp would only
// spend a client sim-second. See Entity.SendFXStamped and docs/destiny-stamps.md#stamps-and-the-client-clock.
func broadcastFXAction(ds *destinyConn, funcName string, args ...marshal.PyRep) {
	if ds.solarSystem == nil || ds.entity == nil {
		return
	}
	for _, mate := range ds.solarSystem.GetBubbleConnectedPlayersExcept(ds.entity.ID) {
		if mate.Conn.IsClosed() {
			continue
		}
		if !ensureBallsKnown(mate, ds.entity) {
			continue
		}
		if err := mate.SendFXStamped(func(stamp uint32) error {
			return destiny.SendSimpleAction(mate.Conn, globalNodeID, mate.AccountID, stamp, funcName, args...)
		}); err != nil {
			log.Printf("[%s] broadcastFXAction %s to entity %d: %v", ds.entity.Addr, funcName, mate.ID, err)
		}
	}
}

// categoryShipBall is invCategories.categoryID for Ship, the only hull class whose client object
// (spaceObject.Ship) implements BoosterUpdate.
const categoryShipBall = 6

// sendBoosterFX pushes a booster OnSpecialFX (guid) for the pilot's hull, to self and grid-wide.
// Only player hulls (categoryShipBall) have a booster curve to drive. Called at warp start, undock,
// warp landing and destiny.Stop, and breakGateCloak. See docs/engine-fx.md.
func sendBoosterFX(ds *destinyConn, c *evenet.Conn, accountID uint64, guid string) {
	if ds == nil || ds.dm == nil || ds.shipCategoryID != categoryShipBall {
		return
	}
	// Same 10-parameter OnSpecialFX shape as sendModuleFX. targetID and otherTypeID must be None, not 0:
	// fxSequencer.AddTrigger drops the trigger when a non-None targetID has no ball.
	args := []marshal.PyRep{
		marshal.PyInt{Value: uint64(ds.dm.EntityID)},
		marshal.PyNone{}, marshal.PyNone{}, marshal.PyNone{}, marshal.PyNone{},
		marshal.PyList{},
		marshal.PyString{Value: guid},
		marshal.PyInt{Value: 0}, marshal.PyInt{Value: 1}, marshal.PyInt{Value: 0},
	}
	if c != nil {
		if err := ds.sendSelfFXStamped(func(stamp uint32) error {
			return destiny.SendSimpleAction(c, globalNodeID, accountID, stamp, "OnSpecialFX", args...)
		}); err != nil {
			log.Printf("sendBoosterFX %s self: %v", guid, err)
		}
	}
	// A gate-cloaked ship is on no one else's grid (ensureBallsKnown skips cloaked entities), so the
	// broadcast would only send packets their fxSequencer drops.
	if ds.entity != nil && ds.entity.IsCloaked() {
		return
	}
	// Grid-wide, like sendModuleFX for every afterburner and MWD. See docs/engine-fx.md#grid-wide-broadcast.
	broadcastFXAction(ds, "OnSpecialFX", args...)
}

// navQueueKey derives the coalescing key for a nav action, mirroring the NPC movement broadcast
// keys. Stop, GotoDirection, FollowBall and Orbit share one "move" key (mutually exclusive states,
// so a later one replaces a queued earlier one). ok=false for anything else, which is sent directly.
func navQueueKey(action marshal.PyRep) (key string, ballID uint32, ok bool) {
	tup, isTup := action.(marshal.PyTuple)
	if !isTup || len(tup.Items) != 2 {
		return "", 0, false
	}
	name, isStr := tup.Items[0].(marshal.PyString)
	if !isStr {
		return "", 0, false
	}
	args, isArgs := tup.Items[1].(marshal.PyTuple)
	if !isArgs || len(args.Items) == 0 {
		return "", 0, false
	}
	idRep, isInt := args.Items[0].(marshal.PyInt)
	if !isInt {
		return "", 0, false
	}
	ballID = uint32(idRep.Value)
	switch name.Value {
	case "SetSpeedFraction":
		return "ssf:" + strconv.FormatUint(uint64(ballID), 10), ballID, true
	case "Stop", "GotoDirection", "FollowBall", "Orbit":
		return "move:" + strconv.FormatUint(uint64(ballID), 10), ballID, true
	case "SetMaxSpeed":
		// Absolute per-ball value, so queued entries coalesce to the final speed (shared with the webs key).
		return "spd:" + strconv.FormatUint(uint64(ballID), 10), ballID, true
	case "CloakMode":
		// Absolute per-ball state: a cloak toggled twice between flushes delivers only the final mode.
		return "cloak:" + strconv.FormatUint(uint64(ballID), 10), ballID, true
	}
	return "", 0, false
}

// broadcastActionList delivers several destiny actions to all bubble mates as one packet under one
// stamp per recipient. Nav actions are queued per recipient and coalesce per (kind, ball); anything
// else is sent directly, because queueing would reorder it against the nav stream.
// See docs/destiny-stamps.md#queued-actions.
func broadcastActionList(ds *destinyConn, actions ...marshal.PyRep) {
	if ds.solarSystem == nil || ds.entity == nil || len(actions) == 0 {
		return
	}

	// Key every action up front: a batch is queued whole or not at all, so it is never split across two packets.
	keys := make([]string, len(actions))
	balls := make([]uint32, len(actions))
	queueable := true
	for i, a := range actions {
		k, b, ok := navQueueKey(a)
		if !ok {
			queueable = false
			break
		}
		keys[i], balls[i] = k, b
	}

	mates := ds.solarSystem.GetBubbleConnectedPlayersExcept(ds.entity.ID)
	for _, mate := range mates {
		if mate.Conn.IsClosed() {
			continue
		}
		// Deliver our ball first if this mate doesn't hold it yet.
		if !ensureBallsKnown(mate, ds.entity) {
			continue
		}
		if queueable {
			for i, a := range actions {
				mate.QueueAction(keys[i], balls[i], a)
			}
			// No flush here: the 5 Hz system tick drains every player's queue, and flushing per event inside a
			// per-recipient loop costs a packet and a stamp per event per pilot. A manoeuvre reaches another pilot one tick later.
			continue
		}
		if err := mate.SendStamped(func(stamp uint32) error {
			return destiny.SendActionList(mate.Conn, globalNodeID, mate.AccountID, stamp, actions...)
		}); err != nil {
			log.Printf("[%s] broadcastActionList to entity %d: %v", ds.entity.Addr, mate.ID, err)
		}
	}
}

// sortBallsByDependency orders balls so that every follow/orbit/missile target precedes the ball
// referencing it: the client parses sequentially and a ball naming a not-yet-parsed one aborts the
// batch. Balls and slims are reordered together; the slices are modified in place.
func sortBallsByDependency(balls []destiny.BallDesc, slims []destiny.SlimItem) ([]destiny.BallDesc, []destiny.SlimItem) {
	if len(balls) <= 1 {
		return balls, slims
	}

	// Build index of balls in this batch.
	inBatch := make(map[uint32]int, len(balls))
	for i, b := range balls {
		inBatch[b.EntityID] = i
	}

	// Extract intra-batch dependencies.
	depOf := make(map[uint32]uint32) // entityID → depends-on entityID
	for _, b := range balls {
		var ref uint32
		switch b.Mode {
		case destiny.ModeFollow, destiny.ModeMissile, destiny.ModeWarp:
			ref = b.FollowID
		case destiny.ModeOrbit:
			ref = b.OrbitID
		}
		if ref != 0 {
			if _, ok := inBatch[ref]; ok {
				depOf[b.EntityID] = ref
			}
		}
	}

	if len(depOf) == 0 {
		return balls, slims // no intra-batch deps
	}

	// Kahn's algorithm for topological sort.
	dependents := make(map[uint32][]uint32)
	inDegree := make(map[uint32]int, len(balls))
	for id := range inBatch {
		inDegree[id] = 0
	}
	for id, dep := range depOf {
		dependents[dep] = append(dependents[dep], id)
		inDegree[id]++
	}

	queue := make([]uint32, 0, len(balls))
	for id, deg := range inDegree {
		if deg == 0 {
			queue = append(queue, id)
		}
	}

	order := make([]uint32, 0, len(balls))
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		order = append(order, id)
		for _, depID := range dependents[id] {
			inDegree[depID]--
			if inDegree[depID] == 0 {
				queue = append(queue, depID)
			}
		}
	}

	// Any remaining balls (cycles) get appended at the end.
	if len(order) < len(balls) {
		seen := make(map[uint32]bool, len(order))
		for _, id := range order {
			seen[id] = true
		}
		for _, b := range balls {
			if !seen[b.EntityID] {
				order = append(order, b.EntityID)
			}
		}
	}

	// Rebuild slices in sorted order.
	sortedBalls := make([]destiny.BallDesc, len(balls))
	sortedSlims := make([]destiny.SlimItem, len(slims))
	for i, id := range order {
		origIdx := inBatch[id]
		sortedBalls[i] = balls[origIdx]
		sortedSlims[i] = slims[origIdx]
	}

	return sortedBalls, sortedSlims
}

// ensureBallsKnown guarantees player p's client holds the given entities before any destiny update
// naming them is sent, delivering unknown ones in a catch-up AddBalls. Returns false when p has no
// ballpark yet; the caller skips the broadcast (the entities arrive in p's SetState).
func ensureBallsKnown(p *destiny.Entity, ents ...*destiny.Entity) bool {
	if p == nil || p.Conn == nil || !p.HasBallparkState() {
		return false
	}
	ids := make([]uint32, 0, len(ents))
	byID := make(map[uint32]*destiny.Entity, len(ents))
	for _, e := range ents {
		if e == nil || e.ID == p.ID {
			continue
		}
		if e.IsCloaked() {
			continue
		}
		if _, dup := byID[e.ID]; dup {
			continue
		}
		ids = append(ids, e.ID)
		byID[e.ID] = e
	}
	if len(ids) == 0 {
		return true
	}
	claimed := p.ClaimUnknownBalls(ids...)
	if len(claimed) == 0 {
		return true
	}
	balls := make([]destiny.BallDesc, 0, len(claimed))
	slims := make([]destiny.SlimItem, 0, len(claimed))
	dmg := make(map[uint32]destiny.DamageState, len(claimed))
	for _, id := range claimed {
		e := byID[id]
		bd := e.BallDesc()
		bd.CorpID = uint32(e.Slim.CorpID)
		bd.Name = e.BallName()
		// First-sight policy shared with every other builder of such a ball (destiny.BallDescForObserver).
		// See docs/destiny-arrival-lanes.md#first-sight-balls.
		bd = destiny.BallDescForObserver(bd, p.KnowsBall)
		balls = append(balls, bd)
		slims = append(slims, e.Slim)
		if e.HasHP() {
			d := e.DamageStateFrac()
			d.Timestamp = server.Win32TimeNow()
			dmg[id] = d
		} else {
			dmg[id] = e.Damage
		}
	}
	// Dependencies (follow/orbit targets) precede referencing balls: the client aborts on forward references.
	balls, slims = sortBallsByDependency(balls, slims)

	// AddBalls carries no effectStates, so the delivered entities' ongoing effects are replayed in the
	// same packet under the same stamp (one client sim-second per catch-up, not two). Weapon fire keeps
	// cycling and the red box appears for a pilot warping into a fight; the client keeps action order.
	var fxActions, jamEvents []marshal.PyRep
	for _, id := range claimed {
		effects := byID[id].GetActiveEffects()
		for _, ae := range effects {
			fxActions = append(fxActions, destiny.BuildEffectAction(ae))
		}
		// EWAR effects aimed at this pilot also yield the jam events for their bars.
		jamEvents = append(jamEvents, destiny.JamEventsForEffects(effects)...)
	}

	if err := p.SendStamped(func(stamp uint32) error {
		actions := make([]marshal.PyRep, 0, 1+len(fxActions))
		actions = append(actions, destiny.BuildAddBallsAction(stamp, balls, slims, dmg))
		actions = append(actions, fxActions...)
		return destiny.SendActionListEvents(p.Conn, globalNodeID, p.AccountID, stamp,
			jamEvents, actions...)
	}); err != nil {
		// Roll back the claim: a ball marked known but never delivered is never retried by the
		// reconciler. The FX share this packet, so the rollback covers them too.
		p.ForgetBalls(claimed...)
		log.Printf("[%s] ensureBallsKnown AddBalls to entity=%d: %v (rolled back %d claims)", p.Addr, p.ID, err, len(claimed))
		return false
	}
	log.Printf("[known] catch-up AddBalls to entity=%d balls=%v effects=%d", p.ID, claimed, len(fxActions))
	return true
}

// ballparkWakes maps a player's ball ID to the wake channel of their reconciler. The reconciler
// goroutine registers and removes its own entry.
var ballparkWakes syncx.Map[uint32, chan struct{}]

// wakeBallparkReconciler asks the player's reconciler to run now because their view just went
// stale. Non-blocking and safe for any ball ID.
func wakeBallparkReconciler(ballID uint32) {
	v, ok := ballparkWakes.Load(ballID)
	if !ok {
		return
	}
	select {
	case v <- struct{}{}:
	default: // a wake is already pending
	}
}

// startBallparkReconciliation runs a goroutine that reconciles the client's ballpark with its grid
// in both directions: the add pass delivers missing balls and the prune pass removes stale ones. It
// stops when ctx is canceled (dock, jump, disconnect). A ticker pass prunes a ball only after two
// consecutive stale reads; a wake prunes on the first. See docs/main-s9-notes.md#ballpark-reconciler.
func startBallparkReconciliation(ctx context.Context, ds *destinyConn, addr string) {
	defer safego.Recover("startBallparkReconciliation")
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	// Wake channel, registered by ball ID: its caller, the bubble-transition callback, is a per-system
	// closure holding an arbitrary mover entity, so a destinyConn field would be unreachable.
	dirty := make(chan struct{}, 1)
	if ds.entity != nil {
		ballparkWakes.Store(ds.entity.ID, dirty)
		defer ballparkWakes.Delete(ds.entity.ID)
	}

	// Balls that read stale on the previous ticker pass. Goroutine-local: one reconciler per connection.
	staleSeen := make(map[uint32]struct{})
	// Consecutive passes each ball has been spared for being mid-warp, capped at maxWarpSparePasses.
	// See docs/main-s9-notes.md#warp-spare-bound.
	warpSpares := make(map[uint32]int)
	var lastResync time.Time
	for {
		var certain bool
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-dirty:
			certain = true
		}

		ent := ds.entity
		ss := ds.solarSystem
		if ss == nil || ent == nil {
			return
		}
		// Skip during warp: the landing batch delivers balls, and a mid-warp AddBalls can abort the C++
		// parser. The landing grace covers the deceleration tail, when the client is ahead of what a prune computes.
		if ent.IsInWarp() || ent.IsWarpPending() || ent.InLandingGrace() {
			continue
		}

		// Stamp-clock net: past stampLeadResyncThreshold the resync goes to the connection goroutine, which owns
		// ds.destinyBaseStamp/Time. See docs/main-s9-notes.md#stamp-lead-net.
		if stampLeadResyncThreshold > 0 && ent.Conn != nil &&
			time.Since(lastResync) >= resyncBallparkCooldown &&
			ent.StampLead() > stampLeadResyncThreshold {
			lastResync = time.Now()
			select {
			case ds.stampResync <- struct{}{}:
			default: // one is already queued
			}
		}

		// Add pass: all grid-mates, NPCs and players. A dropped AddBalls leaves a ship invisible until the next warp or SetState.
		mates := ss.GetBubbleEntitiesExcept(ent.ID)
		var missing []*destiny.Entity
		for _, m := range mates {
			// A player mid-warp is off the grid state the client should hold; the landing batch re-delivers them.
			if m.IsPlayer && (m.IsInWarp() || m.IsWarpPending()) {
				continue
			}
			if !ent.KnowsBall(m.ID) {
				missing = append(missing, m)
			}
		}
		if len(missing) > 0 {
			log.Printf("[%s] reconciliation: delivering %d missing balls", addr, len(missing))
			ensureBallsKnown(ent, missing...)
		}

		// Prune pass. Synthetic ball IDs are excluded (reconcilePruneSpares): a client-only ball has no
		// system entity, so the keep set cannot vouch for it. See docs/main-s9-notes.md#prune-spares.
		candidates, spared := staleBallparkCandidates(ds)
		// Age the warp spares; one spared longer than maxWarpSparePasses becomes an ordinary candidate.
		if len(spared) > 0 || len(warpSpares) > 0 {
			next := make(map[uint32]int, len(spared))
			for _, id := range spared {
				n := warpSpares[id] + 1
				next[id] = n
				if n > maxWarpSparePasses {
					candidates = append(candidates, id)
				}
			}
			warpSpares = next
		}
		if len(candidates) > 0 {
			kept := candidates[:0]
			for _, id := range candidates {
				if !reconcilePruneSpares(id) {
					kept = append(kept, id)
				}
			}
			candidates = kept
		}
		var doomed []uint32
		if certain {
			doomed = candidates
			// A confirmed prune settles everything it saw, so nothing it spared carries into the next ticker pass.
			clear(staleSeen)
		} else {
			seen := make(map[uint32]struct{}, len(candidates))
			for _, id := range candidates {
				seen[id] = struct{}{}
				if _, twice := staleSeen[id]; twice {
					doomed = append(doomed, id)
				}
			}
			staleSeen = seen
		}
		if len(doomed) == 0 {
			continue
		}
		// One RemoveBalls under one stamp for the whole batch (a stamp is one client sim-second).
		// ClaimKnownBalls keeps only what the client still holds, so it never logs removal of a missing ball.
		doomed = ent.ClaimKnownBalls(doomed...)
		if len(doomed) == 0 {
			continue
		}
		log.Printf("[%s] reconciliation: pruning %d stale balls from ballpark: %v", addr, len(doomed), doomed)
		if err := ent.SendStamped(func(stamp uint32) error {
			return destiny.SendRemoveBalls(ent.Conn, globalNodeID, ent.AccountID, stamp, doomed)
		}); err != nil {
			// Roll the claim back, as ensureBallsKnown does: SendStamped refuses over the stamp budget, and a
			// claim that outlives its packet leaves the server believing balls gone that the client still holds.
			ent.MarkBallsKnown(doomed...)
			log.Printf("[%s] reconciliation: prune send failed, rolled back %d claims: %v", addr, len(doomed), err)
			continue
		}
		// A ticker pass prunes only what it saw last time, so this pass's sightings, minus what just went
		// out, are what the next one compares against.
		for _, id := range doomed {
			delete(staleSeen, id)
		}
	}
}

// stopNPCFireFX broadcasts OnSpecialFX(start=0, active=0) for npcEntity's weapon against targetID,
// clearing the client's red box (tactical.py drops the guid from its attackers list on active=0).
// The guid must match the start event's, so the stored active effect is consulted: Laser for turret
// NPCs, MissileDeployment for missile NPCs. Safe to call when no effect is active.
func stopNPCFireFX(npcEntity *destiny.Entity, ss *destiny.SolarSystem, targetID uint32) {
	guid := dogma.FXGUIDLaser
	var modTypeID, otherType int64
	if npcEntity.NPCTurretTypeID != 0 && len(npcEntity.Slim.Modules) > 0 {
		modTypeID = npcEntity.Slim.Modules[0].TypeID
		otherType = npcChargeTypeID(npcEntity.NPCTurretTypeID)
	}
	for _, ae := range npcEntity.GetActiveEffects() {
		if ae.ModuleID == int64(npcEntity.ID) && ae.GUID != "" {
			guid = ae.GUID
			modTypeID = ae.ModuleType
			otherType = ae.OtherType
			break
		}
	}
	npcEntity.ClearActiveEffect(int64(npcEntity.ID))
	// otherTypeID is PyNone when 0: turret.py SetAmmoColor calls cfg.invtypes.Get on it.
	var otherTypeRep marshal.PyRep = marshal.PyNone{}
	if otherType != 0 {
		otherTypeRep = marshal.PyInt{Value: uint64(otherType)}
	}
	fxStop := destiny.BuildSimpleAction("OnSpecialFX",
		marshal.PyInt{Value: uint64(npcEntity.ID)}, // entityID
		marshal.PyInt{Value: uint64(npcEntity.ID)}, // moduleID (EntityShip keys turrets by ball ID)
		marshal.PyInt{Value: uint64(modTypeID)},    // moduleTypeID
		marshal.PyInt{Value: uint64(targetID)},     // targetID
		otherTypeRep,                               // otherTypeID
		marshal.PyList{},                           // area
		marshal.PyString{Value: guid},              // effect_type
		marshal.PyInt{Value: 1},                    // isOffensive
		marshal.PyInt{Value: 0},                    // start
		marshal.PyInt{Value: 0},                    // active
	)
	for _, p := range ss.GetBubbleConnectedPlayersExcept(npcEntity.ID) {
		if !p.KnowsBall(npcEntity.ID) {
			continue
		}
		p.SendFXStamped(func(stamp uint32) error {
			return destiny.SendActionList(p.Conn, globalNodeID, p.AccountID, stamp, fxStop)
		})
	}
}

// isSpawnCmd checks whether a slash CallStream contains a "/spawn" command.
func isSpawnCmd(cs *server.CallStream) bool {
	if cs.Method != "SlashCmd" || len(cs.ArgTuple.Items) == 0 {
		return false
	}
	raw, ok := cs.ArgTuple.Items[0].(marshal.PyString)
	if !ok {
		return false
	}
	line := strings.TrimSpace(raw.Value)
	if len(line) > 0 && (line[0] == '/' || line[0] == '#') {
		line = line[1:]
	}
	parts := strings.Fields(line)
	return len(parts) > 0 && strings.ToLower(parts[0]) == "spawn"
}

// isSlashCmd checks whether a slash CallStream contains the given command name.
func isSlashCmd(cs *server.CallStream, name string) bool {
	if cs.Method != "SlashCmd" || len(cs.ArgTuple.Items) == 0 {
		return false
	}
	raw, ok := cs.ArgTuple.Items[0].(marshal.PyString)
	if !ok {
		return false
	}
	line := strings.TrimSpace(raw.Value)
	if len(line) > 0 && (line[0] == '/' || line[0] == '#') {
		line = line[1:]
	}
	parts := strings.Fields(line)
	return len(parts) > 0 && strings.ToLower(parts[0]) == name
}

// handleDeadspace processes "/deadspace": teleports the player to a random position
// 5 to 15 AU away in the current solar system.
func handleDeadspace(c *evenet.Conn, addr string, cs *server.CallStream,
	pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB, ds *destinyConn) error {

	if ds.dm == nil {
		return server.SendCallRsp(c, pkt, accountID,
			marshal.PyString{Value: "deadspace: you must be in space (undocked)."})
	}

	// A random direction and a distance of 5 to 15 AU from the current position.
	const au = 1.496e11 // 1 AU in meters
	dist := (5.0 + rand.Float64()*10.0) * au

	// Random unit vector via spherical coordinates.
	theta := rand.Float64() * 2 * math.Pi
	phi := math.Acos(2*rand.Float64() - 1)
	dx := math.Sin(phi) * math.Cos(theta)
	dy := math.Sin(phi) * math.Sin(theta)
	dz := math.Cos(phi)

	pos := destiny.Vec3{
		X: ds.dm.Pos.X + dx*dist,
		Y: ds.dm.Pos.Y + dy*dist,
		Z: ds.dm.Pos.Z + dz*dist,
	}

	// snapToPosition does the whole teleport: server position, client SetBallPosition/Stop,
	// bubble transfer (old bubble RemoveBalls, new bubble AddBalls, fresh SetState) and persistence.
	tp := &slashTeleporter{c: c, addr: addr, accountID: accountID, charID: ds.charID, sqldb: sqldb, ds: ds}
	tp.snapToPosition(pos)

	distAU := dist / au
	log.Printf("[%s] slash: /deadspace → jumped %.1f AU to (%.0f, %.0f, %.0f)", addr, distAU, pos.X, pos.Y, pos.Z)
	return server.SendCallRsp(c, pkt, accountID,
		marshal.PyString{Value: fmt.Sprintf("Jumped to deadspace (%.1f AU away). You are alone.", distAU)})
}

// handleGateFlash processes "/gateflash": fires the GateActivity effect on
// every stargate within bubble range of the player.
func handleGateFlash(c *evenet.Conn, addr string, cs *server.CallStream,
	pkt *evenet.PyPacket, accountID int64, ds *destinyConn) error {

	if ds.solarSystem == nil || ds.entity == nil || ds.dm == nil {
		return server.SendCallRsp(c, pkt, accountID,
			marshal.PyString{Value: "gateflash: you must be in space (undocked)."})
	}

	ss := ds.solarSystem
	playerPos := ds.dm.Pos
	rSq := destiny.BubbleRadius * destiny.BubbleRadius
	count := 0

	for i, slim := range ss.CelSlims {
		if slim.GroupID != 10 {
			continue
		}
		ball := ss.CelBalls[i]
		dx := ball.Pos.X - playerPos.X
		dy := ball.Pos.Y - playerPos.Y
		dz := ball.Pos.Z - playerPos.Z
		if dx*dx+dy*dy+dz*dz > rSq {
			continue
		}

		gateID := ball.EntityID
		ds.syncStamp()
		if err := ds.sendSelfStamped(func(stamp uint32) error {
			return destiny.SendSimpleAction(c, globalNodeID, uint64(accountID), stamp,
				"OnSpecialFX",
				marshal.PyInt{Value: uint64(gateID)},
				marshal.PyNone{}, marshal.PyNone{}, marshal.PyNone{}, marshal.PyNone{},
				marshal.PyList{},
				marshal.PyString{Value: "effects.GateActivity"},
				marshal.PyInt{Value: 0}, marshal.PyInt{Value: 1}, marshal.PyInt{Value: 0},
			)
		}); err != nil {
			log.Printf("[%s] gateflash: SendSimpleAction gate %d: %v", addr, gateID, err)
			continue
		}

		// Broadcast to bubble mates.
		mates := ss.GetBubbleConnectedPlayersExcept(ds.entity.ID)
		for _, mate := range mates {
			if mate.Conn.IsClosed() {
				continue
			}
			if err := mate.SendStamped(func(mStamp uint32) error {
				return destiny.SendSimpleAction(mate.Conn, globalNodeID, mate.AccountID, mStamp,
					"OnSpecialFX",
					marshal.PyInt{Value: uint64(gateID)},
					marshal.PyNone{}, marshal.PyNone{}, marshal.PyNone{}, marshal.PyNone{},
					marshal.PyList{},
					marshal.PyString{Value: "effects.GateActivity"},
					marshal.PyInt{Value: 0}, marshal.PyInt{Value: 1}, marshal.PyInt{Value: 0},
				)
			}); err != nil {
				log.Printf("[%s] gateflash: broadcast to entity %d: %v", addr, mate.ID, err)
			}
		}
		count++
	}

	log.Printf("[%s] slash: /gateflash → flashed %d gate(s)", addr, count)
	return server.SendCallRsp(c, pkt, accountID,
		marshal.PyString{Value: fmt.Sprintf("Flashed %d gate(s).", count)})
}

// handleNPCSpawn processes "/spawn <typeID> [stance]": spawns an NPC 10 km from the player.
// Stance is aggressive (default), defensive or passive.
func handleNPCSpawn(c *evenet.Conn, addr string, cs *server.CallStream,
	pkt *evenet.PyPacket, accountID int64, charID int64,
	sqldb *sql.DB, ds *destinyConn, npcMgr *npc.NPCManager) error {

	if ds.solarSystem == nil || ds.entity == nil || ds.dm == nil {
		return server.SendCallRsp(c, pkt, accountID,
			marshal.PyString{Value: "spawn: you must be in space (undocked) to spawn NPCs."})
	}

	// Parse typeID from args: "/spawn 587 [stance]"
	raw := cs.ArgTuple.Items[0].(marshal.PyString)
	line := strings.TrimSpace(raw.Value)
	if len(line) > 0 && (line[0] == '/' || line[0] == '#') {
		line = line[1:]
	}
	parts := strings.Fields(line)
	if len(parts) < 2 {
		return server.SendCallRsp(c, pkt, accountID,
			marshal.PyString{Value: "Usage: /spawn <typeID> [stance]  (stance: aggressive, defensive, passive)"})
	}
	var typeID int64
	if _, err := fmt.Sscanf(parts[1], "%d", &typeID); err != nil || typeID <= 0 {
		return server.SendCallRsp(c, pkt, accountID,
			marshal.PyString{Value: "spawn: invalid typeID — must be a positive integer."})
	}

	stance := npc.StanceAggressive
	if len(parts) >= 3 {
		if !npc.ValidStance(parts[2]) {
			return server.SendCallRsp(c, pkt, accountID,
				marshal.PyString{Value: "spawn: stance must be aggressive, defensive or passive."})
		}
		stance = npc.ParseStance(parts[2])
	}

	// Look up NPC stats.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stats, err := evedb.GetNPCStats(ctx, sqldb, typeID)
	if err != nil {
		log.Printf("[%s] spawn: GetNPCStats typeID=%d: %v", addr, typeID, err)
		return server.SendCallRsp(c, pkt, accountID,
			marshal.PyString{Value: fmt.Sprintf("spawn: typeID %d not found in database.", typeID)})
	}

	// Compute spawn position: player pos + 10km random offset.
	playerPos := ds.dm.Pos
	theta := rand.Float64() * 2 * math.Pi
	phi := rand.Float64() * math.Pi
	r := 10_000.0
	spawnPos := destiny.Vec3{
		X: playerPos.X + r*math.Sin(phi)*math.Cos(theta),
		Y: playerPos.Y + r*math.Sin(phi)*math.Sin(theta),
		Z: playerPos.Z + r*math.Cos(phi),
	}

	// Fetch NPC HP and resistances.
	var npcHP *npc.NPCHPInit
	ehp, ehpErr := evedb.GetEntityHPByType(ctx, sqldb, typeID)
	if ehpErr == nil {
		npcHP = &npc.NPCHPInit{
			ShieldHP: ehp.ShieldHP, ArmorHP: ehp.ArmorHP,
			StructureHP: ehp.StructureHP, ShieldTau: ehp.ShieldTau,
			ShieldRes: ehp.ShieldRes, ArmorRes: ehp.ArmorRes, HullRes: ehp.HullRes,
		}
	}

	// Fetch weapon profile.
	weaponProfile, wpErr := evedb.GetNPCWeaponProfile(ctx, sqldb, typeID)
	if wpErr != nil {
		log.Printf("[%s] spawn: GetNPCWeaponProfile typeID=%d: %v", addr, typeID, wpErr)
	}

	// Spawn the NPC.
	npcBounty := evedb.GetNPCBounty(ctx, sqldb, typeID)
	entity := npcMgr.SpawnNPC(ds.solarSystem, typeID, stats.TypeName,
		spawnPos, stats.Mass, stats.Agility, stats.MaxVelocity, stats.Radius,
		stance, npcHP, weaponProfile, 0, false, npcBounty, 0, false)
	if entity == nil {
		return server.SendCallRsp(c, pkt, accountID, marshal.PyString{Value: "spawn failed: system is shutting down"})
	}
	npc.SetGroupCategory(entity, stats.GroupID, stats.CategoryID)

	// Deliver the NPC to all bubble players via the known-ball gate.
	for _, p := range ds.solarSystem.GetBubbleConnectedPlayersExcept(entity.ID) {
		ensureBallsKnown(p, entity)
	}

	msg := fmt.Sprintf("Spawned %s (typeID=%d) entityID=%d stance=%s at %.0fkm",
		stats.TypeName, typeID, entity.ID, stance, r/1000)
	log.Printf("[%s] spawn: %s", addr, msg)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyString{Value: msg})
}

// registerUndock builds this connection's Entity and registers it in the shared SolarSystem,
// loading the system first if needed, then tells bubble-mates about the new ship. Undock,
// login in space, gate arrival, teleport and ship swap all come through here.
func registerUndock(c *evenet.Conn, addr string, accountID uint64, charID int64, sqldb *sql.DB, ds *destinyConn) {
	if ds.dm == nil || ds.sysID == 0 || ds.sysMgr == nil {
		return
	}

	// Keep the session's location current for station and system routed broadcasts.
	updateSessionLocation(addr, 0, ds.sysID)

	// Leaving the station: drop from the guest list so GetGuests stays accurate.
	if globalStationReg != nil && c != nil {
		globalStationReg.Remove(c)
	}

	// The world build is ensureSystemLoaded (systemload.go). The celestials and solar-system
	// entity row this connection already loaded are passed through.
	ss, ensErr := ensureSystemLoaded(ds.sysMgr, ds.sysID, sqldb, addr, ds.celestials, ds.solItem)
	if ensErr != nil {
		log.Printf("[%s] registerUndock: load sysID=%d: %v", addr, ds.sysID, ensErr)
		return
	}
	ds.solarSystem = ss

	entity := &destiny.Entity{
		ID:        uint32(ds.shipID),
		Manager:   ds.dm,
		AccountID: accountID,
		Conn:      c,
		Addr:      addr,
		CharName:  ds.charName,
		ShipName:  ds.shipName,
		BubbleID:  -1,
		IsPlayer:  true,
		Slim: destiny.SlimItem{
			BallID:         ds.shipID,
			ItemID:         ds.shipID,
			TypeID:         ds.shipTypeID,
			GroupID:        ds.shipGroupID,
			CategoryID:     ds.shipCategoryID,
			OwnerID:        charID,
			CorpID:         ds.corpID,
			AllianceID:     ds.allianceID,
			CharID:         charID,
			SecurityStatus: ds.securityStatus,
			Bounty:         ds.bounty,
		},
		Damage: fullDamageState(),
	}
	ds.dm.SetEntity(entity)
	// Spawn grace: NPCs cannot lock the ship while the client is still loading the scene.
	entity.SetNoLockUntil(time.Now().Add(12 * time.Second))

	// Initialize HP and resistances from ship type attributes, plus flat HP
	// bonuses from fitted tank modules (shield extenders, armor plates).
	if sqldb != nil && ds.shipTypeID != 0 {
		hpCtx, hpCancel := context.WithTimeout(context.Background(), 5*time.Second)
		// Hull bonuses ("Special Ability" traits): shield/armor HP and resistances, cap capacity and
		// recharge, per trained level of the hull's command skill.
		hullB, hullErr := evedb.GetShipHullBonuses(hpCtx, sqldb, charID, ds.shipTypeID)
		if hullErr != nil {
			log.Printf("[%s] registerUndock: GetShipHullBonuses: %v", addr, hullErr)
			hullB = nil
		}
		hullStats := hullB.StatMults()
		ehp, hpErr := evedb.GetEntityHPByType(hpCtx, sqldb, ds.shipTypeID)
		if hpErr == nil {
			shieldAdd, armorAdd, bonErr := evedb.GetShipFittingHPBonuses(hpCtx, sqldb, ds.shipID)
			if bonErr != nil {
				log.Printf("[%s] registerUndock: GetShipFittingHPBonuses: %v", addr, bonErr)
				shieldAdd, armorAdd = 0, 0
			}
			// Tank skills multiply the flat pools (extenders/plates included,
			// matching EVE modifier order) and shorten the shield recharge tau.
			shieldSkill, armorSkill, hullSkill, rechargeSkill, tskErr := evedb.GetTankSkillMultipliers(hpCtx, sqldb, charID)
			if tskErr != nil {
				log.Printf("[%s] registerUndock: GetTankSkillMultipliers: %v", addr, tskErr)
				shieldSkill, armorSkill, hullSkill, rechargeSkill = 1, 1, 1, 1
			}
			// Passive module multipliers: PDS/flux coils (shield pool +
			// recharge), bulkheads/cargoholds (structure).
			pb, pbErr := evedb.GetShipPassiveBonuses(hpCtx, sqldb, ds.shipID)
			if pbErr != nil {
				log.Printf("[%s] registerUndock: GetShipPassiveBonuses: %v", addr, pbErr)
				pb = &evedb.ShipPassiveBonuses{CPUMult: 1, PGMult: 1, CapCapMult: 1, CapRechargeMult: 1,
					ShieldCapMult: 1, ShieldRechargeMult: 1, StructureMult: 1, CargoMult: 1,
					VelocityMult: 1, AgilityMult: 1, ScanResMult: 1}
			}
			maxShield := (ehp.ShieldHP + shieldAdd) * shieldSkill * pb.ShieldCapMult * hullStats.ShieldHP
			maxArmor := (ehp.ArmorHP + armorAdd) * armorSkill * hullStats.ArmorHP
			maxStructure := ehp.StructureHP * hullSkill * pb.StructureMult
			entity.HP = destiny.HPState{
				MaxShield: maxShield, CurShield: maxShield,
				MaxArmor: maxArmor, CurArmor: maxArmor,
				MaxStructure: maxStructure, CurStructure: maxStructure,
				ShieldTau: ehp.ShieldTau * rechargeSkill * pb.ShieldRechargeMult,
			}
			// Restore persisted combat damage (written on dock, jump and relog). Shield starts full.
			if structDmg, armorDmg, dmgErr := evedb.GetShipCombatDamage(hpCtx, sqldb, ds.shipID); dmgErr != nil {
				log.Printf("[%s] registerUndock: GetShipCombatDamage: %v", addr, dmgErr)
			} else if structDmg > 0 || armorDmg > 0 {
				entity.HP.CurArmor = maxArmor - armorDmg
				if entity.HP.CurArmor < 0 {
					entity.HP.CurArmor = 0
				}
				entity.HP.CurStructure = maxStructure - structDmg
				// A wreck-level hull enters space with 1 structure left, never dead.
				if entity.HP.CurStructure < 1 {
					entity.HP.CurStructure = 1
				}
				log.Printf("[%s] registerUndock: restored damage ship %d (armor %.0f/%.0f, structure %.0f/%.0f)",
					addr, ds.shipID, entity.HP.CurArmor, maxArmor, entity.HP.CurStructure, maxStructure)
			}
			entity.Resists = [3][4]float64{ehp.ShieldRes, ehp.ArmorRes, ehp.HullRes}
			if mult, resErr := evedb.GetShipResistanceMultipliers(hpCtx, sqldb, ds.shipID, nil); resErr == nil {
				for l := 0; l < 3; l++ {
					for d := 0; d < 4; d++ {
						entity.Resists[l][d] *= mult[l][d]
					}
				}
			} else {
				log.Printf("[%s] registerUndock: GetShipResistanceMultipliers: %v", addr, resErr)
			}
			// Hull resistance bonuses (Merlin/Moa shield, Punisher/Maller
			// armor...) scale the shield and armor resonances.
			if len(hullB) > 0 {
				hullRes := hullB.ResistMults()
				for l := 0; l < 2; l++ {
					for d := 0; d < 4; d++ {
						entity.Resists[l][d] *= hullRes[l][d]
					}
				}
			}
			if shieldAdd > 0 || armorAdd > 0 {
				log.Printf("[%s] registerUndock: fitting HP bonuses shield+%.0f armor+%.0f", addr, shieldAdd, armorAdd)
			}
		}
		// Capacitor: full at undock, scaled by cap skills and fitted batteries, rechargers,
		// relays and PDS.
		if shipAttrs, capErr := evedb.GetTypeAttributes(hpCtx, sqldb, ds.shipTypeID); capErr == nil {
			if capMax := shipAttrs[dogma.AttrCapacitorCapacity]; capMax > 0 {
				rechargeMs := shipAttrs[dogma.AttrRechargeRate]
				if capSkill, capRechSkill, cskErr := evedb.GetCapacitorSkillMultipliers(hpCtx, sqldb, charID); cskErr == nil {
					capMax *= capSkill
					rechargeMs *= capRechSkill
				}
				if pb, pbErr := evedb.GetShipPassiveBonuses(hpCtx, sqldb, ds.shipID); pbErr == nil {
					capMax = capMax*pb.CapCapMult + pb.CapAdd
					rechargeMs *= pb.CapRechargeMult
				}
				capMax *= hullStats.CapCapacity
				rechargeMs *= hullStats.CapRecharge
				entity.InitCap(capMax, rechargeMs)
			}
		}
		hpCancel()
	}
	// A gate jump or teleport keeps the charge the ship left with; an undock starts full. The
	// client is told either way, or its gauge runs on from the last charge it knew.
	applyCarriedCharge(ds, entity)
	pushCapacitorCharge(c, int64(accountID), charID, ds, entity)

	// Populate SlimItem.Modules from fitted modules so the client renders turret models.
	if sqldb != nil {
		modCtx, modCancel := context.WithTimeout(context.Background(), 5*time.Second)
		fittedMods, modErr := evedb.GetFittedModules(modCtx, sqldb, ds.shipID)
		modCancel()
		if modErr == nil {
			for _, mod := range fittedMods {
				entity.Slim.Modules = append(entity.Slim.Modules, destiny.ModuleRef{
					ItemID: mod.ItemID,
					TypeID: mod.TypeID,
				})
			}
		}
	}

	// Wire up TargetLookup so Follow/Orbit can track other entities AND celestials.
	ds.dm.TargetLookup = playerNavTargetLookup(ss)

	// Load per-character NPC standings for standings-based hostility checks.
	if sqldb != nil {
		sCtx, sCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if standings, err := evedb.GetCharNPCStandingsMap(sCtx, sqldb, charID); err == nil {
			entity.NPCStandings = standings
		}
		// The Halo set: read once here, because an implant is plugged docked
		// and this entity does not outlive the undock.
		entity.SetImplantSigRadiusMult(evedb.GetCharImplantMults(sCtx, sqldb, charID).SigRadius)
		sCancel()
	}

	applyGMMode(charID, entity)
	ds.entity = entity
	entity.PlayerState = ds
	// A ship swap in space (Ship Maintenance Array) keeps the client's ballpark: the new entity
	// inherits the old one's known balls and stamps.
	if from := ds.pendingBallparkFrom; from != nil {
		entity.TransferBallparkState(from, ds.pendingBallparkOldID)
		ds.pendingBallparkFrom = nil
	}
	bubbleMates, undockMerged, addOK := ss.AddEntity(entity)
	if !addOK {
		// Lost the race against RemoveIfEmpty (system torn down between GetOrCreate and AddEntity):
		// register again on a fresh system.
		log.Printf("[%s] registerUndock: sysID=%d torn down mid-registration, retrying", addr, ds.sysID)
		registerUndock(c, addr, accountID, charID, sqldb, ds)
		return
	}

	// Bubble-mates learn of the new ship through incremental AddBalls: tactical.py ignores
	// DoBallClear, so a SetState here would duplicate overview entries.
	if undockMerged {
		// Bubble merge: cross-notify all players in the merged bubble.
		for _, p := range bubbleMates {
			if !p.IsPlayer || p.Conn == nil || p.Conn.IsClosed() {
				continue
			}
			ensureBallsKnown(p, bubbleMates...)
		}
		log.Printf("[%s] registerUndock: bubble merge, cross-notified %d entities", addr, len(bubbleMates))
	} else if len(bubbleMates) > 0 {
		for _, mate := range bubbleMates {
			ensureBallsKnown(mate, entity)
		}
		log.Printf("[%s] registerUndock: notified %d bubble-mates via AddBalls", addr, len(bubbleMates))
	}

	// Re-bind any of this character's abandoned drones in the system to the
	// new ship so they can be commanded and scooped again.
	if globalDroneMgr != nil && ds.shipID != 0 {
		globalDroneMgr.ReclaimControl(charID, ds.shipID, entity.ID, ds.sysID)
		// Update the tracking table so a crash after re-ship scoops to the
		// new ship, not the old one.
		go func() {
			defer safego.Recover("registerUndock")
			rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer rcancel()
			if err := evedb.RehomeDrones(rctx, sqldb, charID, ds.sysID, ds.shipID); err != nil {
				log.Printf("[drone] RehomeDrones charID=%d sysID=%d: %v", charID, ds.sysID, err)
			}
		}()
	}

	// Start ballpark reconciliation: it periodically re-sends NPC balls whose first delivery failed.
	if ds.cancelReconcile != nil {
		ds.cancelReconcile()
	}
	reconCtx, reconCancel := context.WithCancel(context.Background())
	ds.cancelReconcile = reconCancel
	go startBallparkReconciliation(reconCtx, ds, addr)

	// Sync the HUD S/A/S bars with the restored HP: godma's cached damage values go stale across a
	// docked repair. Sent directly because the undock no-lock grace suppresses sendPlayerHPUpdate.
	if hp := entity.HPSnapshot(); hp.MaxStructure > 0 {
		sendModuleAttributeChange(c, int64(accountID), charID, ds.shipID, 264, hp.CurShield, hp.MaxShield)
		sendModuleAttributeChange(c, int64(accountID), charID, ds.shipID, 266, hp.MaxArmor-hp.CurArmor, 0)
		sendModuleAttributeChange(c, int64(accountID), charID, ds.shipID, 3, hp.MaxStructure-hp.CurStructure, 0)
	}

	// Undock, login in space and every gate arrival pass through here, so this one call
	// re-resolves the pilot's gang bonuses for the new system.
	markCharGangDirty(charID)

	// Arrival rules (spaceentry.go): every way into space passes here. The entry reason only
	// sets the login grace and is consumed.
	entry := ds.spaceEntry
	ds.spaceEntry = entryMove
	onPilotEnteredSpace(entity, ss, sqldb, globalNPCMgr, entry)
}

// teardownSystemNPCs clears NPC and spawner state for a solar system just removed from the
// SystemManager (last player left), so the NPC tick and the spawner stop driving the discarded
// system and the recreated one loads its belts again.
func teardownSystemNPCs(sysID int64) {
	if globalNPCMgr != nil {
		globalNPCMgr.RemoveSystem(sysID)
	}
	if globalSpawnerMgr != nil {
		globalSpawnerMgr.UnloadSystem(sysID)
	}
	sentryForgetSystem(sysID)
	customsDespawn(sysID)
	reapAbandonedDrones(sysID)
}

// reapAbandonedDrones drops the drone registry's records for a torn-down system and returns
// each drone item to its last controller's drone bay in the DB. The registry is the only way
// back to a drone left in space, so an item left at its space location would be unreachable.
func reapAbandonedDrones(sysID int64) {
	if globalDroneMgr == nil {
		return
	}
	removed := globalDroneMgr.RemoveSystem(sysID)
	if len(removed) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, rec := range removed {
		controllerID, _, _, _, dead := rec.Snapshot()
		if dead || controllerID == 0 || globalDB == nil {
			continue
		}
		if err := evedb.ScoopDroneToBay(ctx, globalDB, rec.ItemID, controllerID); err != nil {
			log.Printf("[drone] teardown return %s (item=%d) to ship %d: %v",
				rec.TypeName, rec.ItemID, controllerID, err)
			continue
		}
		log.Printf("[drone] teardown: %s (item=%d) returned to bay of ship %d",
			rec.TypeName, rec.ItemID, controllerID)
	}
}

// persistShipDamage saves the live entity's armor and structure damage to the DB, so dock, gate
// jump and relog do not repair the ship and the station repair shop reads real damage.
// Shield is not persisted: it recharges passively.
func persistShipDamage(ds *destinyConn, sqldb *sql.DB) {
	if ds == nil || ds.entity == nil || ds.shipID == 0 || sqldb == nil {
		return
	}
	hp := ds.entity.HPSnapshot()
	if hp.MaxStructure <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := evedb.SaveShipCombatDamage(ctx, sqldb, ds.shipID,
		hp.MaxStructure-hp.CurStructure, hp.MaxArmor-hp.CurArmor); err != nil {
		log.Printf("persistShipDamage ship %d: %v", ds.shipID, err)
	}
}

// removeBallsBatched removes every ball in ids from ss and sends each player that held any of
// them a single RemoveBalls packet. Every stamped packet advances the client's sim clock by a
// second. See docs/destiny-stamps.md#stamps-and-the-client-clock.
// Returns the players still in the system, first-seen order, for the caller's per-player cleanup.
func removeBallsBatched(ss *destiny.SolarSystem, ids []uint32) []*destiny.Entity {
	var order []*destiny.Entity
	seen := make(map[*destiny.Entity]bool, len(ids))
	perPlayer := make(map[*destiny.Entity][]uint32, len(ids))
	for _, id := range ids {
		for _, p := range ss.RemoveEntity(id) {
			if !seen[p] {
				seen[p] = true
				order = append(order, p)
			}
			if p.Conn == nil || !p.KnowsBall(id) {
				continue
			}
			perPlayer[p] = append(perPlayer[p], id)
		}
	}
	for _, p := range order {
		batch := perPlayer[p]
		if len(batch) == 0 {
			continue
		}
		p.SendStamped(func(stamp uint32) error {
			return destiny.SendRemoveBalls(p.Conn, globalNodeID, p.AccountID, stamp, batch)
		})
		p.ForgetBalls(batch...)
	}
	return order
}

// unregisterDock removes this player from the shared system state and tells bubble-mates the
// ship is gone.
func unregisterDock(addr string, ds *destinyConn, sqldb *sql.DB) {
	if ds.solarSystem == nil || ds.entity == nil {
		return
	}
	persistShipDamage(ds, sqldb)
	// Drones left in space go idle and revert to owner control.
	if globalDroneMgr != nil && ds.shipID != 0 {
		globalDroneMgr.AbandonControl(ds.shipID)
	}
	remaining := ds.solarSystem.RemoveEntity(ds.entity.ID)
	for _, mate := range remaining {
		if mate.Conn == nil || !mate.KnowsBall(ds.entity.ID) {
			continue
		}
		mate.SendStamped(func(stamp uint32) error {
			return destiny.SendRemoveBalls(mate.Conn, globalNodeID, mate.AccountID, stamp, []uint32{ds.entity.ID})
		})
		mate.ForgetBalls(ds.entity.ID)
	}
	releaseSystem(ds.sysMgr, ds.solarSystem, sqldb)
	log.Printf("[%s] unregisterDock: removed entity %d, notified %d mates", addr, ds.entity.ID, len(remaining))
	if ds.cancelReconcile != nil {
		ds.cancelReconcile()
		ds.cancelReconcile = nil
	}
	// Other goroutines may hold this Entity in a bubble-mate snapshot and send against it later;
	// clearing its ballpark state makes their guards fire.
	ds.entity.ClearBallparkState()
	ds.solarSystem = nil
	ds.entity = nil
}

// ── /tr, /goto, /translocate teleport implementation ───────────────────────

// slashTeleporter implements slash.Teleporter on top of a live destinyConn. It is created
// per CallReq in handleCallReq and handed to slash.Handle.
type slashTeleporter struct {
	c         *evenet.Conn
	addr      string
	accountID int64
	charID    int64
	stationID *int64
	sqldb     *sql.DB
	ds        *destinyConn
	sysMgr    *destiny.SystemManager
}

// Translocate teleports the character to destID. destID can be a solar system,
// a station, or any mapDenormalize celestial. Behaviour depends on whether the
// character is currently docked.
func (t *slashTeleporter) Translocate(ctx context.Context, destID int64) (string, error) {
	target, err := evedb.ClassifyTeleportTarget(ctx, t.sqldb, destID)
	if err != nil {
		return "", err
	}
	switch target.Kind {
	case evedb.TeleportTargetUnknown:
		return fmt.Sprintf("/tr: ID %d is not a solar system, station, celestial, or character.", destID), nil
	case evedb.TeleportTargetStation:
		return t.toStation(ctx, target)
	case evedb.TeleportTargetSystem:
		return t.toSystem(ctx, target, destiny.Vec3{}) // land at sun (0,0,0)
	case evedb.TeleportTargetCelestial:
		return t.toCelestial(ctx, target)
	case evedb.TeleportTargetCharacter:
		return t.toCharacter(ctx, target)
	}
	return "/tr: internal error: unhandled target kind.", nil
}

// Goto moves the caller to (x, y, z) in the current solar system.  Requires
// the caller to be in space.
func (t *slashTeleporter) Goto(ctx context.Context, x, y, z float64) (string, error) {
	if t.ds == nil || t.ds.dm == nil || t.isDocked() {
		return "/goto: you must be in space.", nil
	}
	pos := destiny.Vec3{X: x, Y: y, Z: z}
	t.snapToPosition(pos)
	return fmt.Sprintf("/goto: snapped to (%.0f, %.0f, %.0f) in system %d.", x, y, z, t.ds.sysID), nil
}

// SelfDestruct destroys the caller's current ship or pod.
func (t *slashTeleporter) SelfDestruct() (string, error) {
	if t.ds == nil || t.ds.entity == nil || t.ds.solarSystem == nil {
		return "selfdestruct: you must be in space.", nil
	}
	if t.ds.shipTypeID == 670 {
		handlePlayerPodKilled(t.ds, t.sqldb)
		return "selfdestruct: pod destroyed, respawning at home station.", nil
	}
	handlePlayerShipDestroyed(t.ds, t.sqldb, globalNPCMgr)
	return "selfdestruct: ship destroyed, ejected into capsule.", nil
}

// ── /gm on|off ────────────────────────────────────────────────────────────

// gmModeChars holds the charIDs in GM mode (/gm on). destiny.Entity is rebuilt on undock and
// pod transitions, so this map is the durable half; applyGMMode restamps each new entity from it.
// See docs/main-s10-notes.md#gm-mode-durable.
var gmModeChars syncx.Map[int64, struct{}] // charID int64 → struct{}

// gmModeOn reports whether a character has /gm on.
func gmModeOn(charID int64) bool {
	_, ok := gmModeChars.Load(charID)
	return ok
}

// applyGMMode stamps a freshly built entity with its character's stored GM mode and cloak.
// Every site that assigns ds.entity calls it before the entity is registered.
func applyGMMode(charID int64, e *destiny.Entity) {
	if e != nil && gmModeOn(charID) {
		e.SetGMMode(true)
	}
	// Stamped before registration, so the first SetState already carries the cloak bit and the
	// ghost radius.
	if e != nil {
		if ghost, ok := gmCloakChars.Load(charID); ok {
			e.SetGMCloak(true, ghost)
		}
	}
}

// gmCloakChars holds the charIDs with the GM standing cloak on (/gm cloak on), mapped to
// whether it is ghosted. Durable half for the same reason as gmModeChars.
var gmCloakChars syncx.Map[int64, bool] // charID int64 → ghost bool

// sendSelfCloakFX plays the cloak shimmer on the pilot's own ship. The ball's cloak bit
// arrives in SetState, but the client draws the visual only from this effect.
func sendSelfCloakFX(ds *destinyConn, c *evenet.Conn, accountID uint64) {
	if ds == nil || ds.dm == nil || c == nil {
		return
	}
	ds.sendSelfStamped(func(stamp uint32) error {
		return destiny.SendSimpleAction(c, globalNodeID, accountID, stamp,
			"OnSpecialFX",
			marshal.PyInt{Value: uint64(ds.dm.EntityID)},
			marshal.PyNone{}, marshal.PyNone{}, marshal.PyNone{}, marshal.PyNone{},
			marshal.PyList{},
			marshal.PyString{Value: "effects.CloakRegardless"},
			marshal.PyInt{Value: 0}, marshal.PyInt{Value: 1}, marshal.PyInt{Value: 0},
		)
	})
}

// hideFromMates withdraws the pilot's ball from every bubble-mate who holds it, the inverse of
// breakGateCloak's reveal. Only balls the mate holds are removed (ClaimKnownBalls): removing an
// unknown ball can desync the C++ ballpark.
func hideFromMates(ds *destinyConn) {
	if ds == nil || ds.entity == nil || ds.solarSystem == nil {
		return
	}
	id := ds.entity.ID
	for _, mate := range ds.solarSystem.GetBubbleConnectedPlayersExcept(id) {
		if mate.Conn == nil {
			continue
		}
		known := mate.ClaimKnownBalls(id)
		if len(known) == 0 {
			continue
		}
		mate.QueueAction("rm:"+strconv.FormatUint(uint64(known[0]), 10), known[0],
			destiny.BuildRemoveBallsAction(known))
	}
}

// SetGMCloak implements slash.GMCloakSetter: /gm cloak on [solid] | off. Movement, warp and
// modules never break the cloak (breakGateCloak returns early on it), and it survives gates,
// docking and pods through gmCloakChars. On also sets /gm on; off leaves it set. ghost puts a
// 1 m radius on the pilot's own ball. See docs/main-s10-notes.md#gm-cloak.
func (t *slashTeleporter) SetGMCloak(on, ghost bool) (string, error) {
	if on {
		if _, err := t.SetGMMode(true); err != nil {
			return "", err
		}
		gmCloakChars.Store(t.charID, ghost)
	} else {
		gmCloakChars.Delete(t.charID)
	}

	ds := t.ds
	if t.isDocked() || ds == nil || ds.entity == nil || ds.solarSystem == nil || ds.dm == nil {
		if on {
			return "/gm cloak on: takes effect when you undock.", nil
		}
		return "/gm cloak off.", nil
	}
	e := ds.entity
	c, acct := e.Conn, uint64(e.AccountID)
	wasGhost := e.IsGMGhost()

	pushRadius := func(r float64) {
		// SetBallRadius is a native ballpark op: it needs a freshly minted stamp (sendSelfStamped,
		// not the FX variant).
		ds.sendSelfStamped(func(stamp uint32) error {
			return destiny.SendSetBallRadius(c, globalNodeID, acct, stamp, ds.dm.EntityID, r)
		})
	}

	if on {
		e.SetGMCloak(true, ghost)
		hideFromMates(ds)
		sendSelfCloakFX(ds, c, acct)
		switch {
		case ghost:
			pushRadius(destiny.GMGhostRadius)
		case wasGhost:
			pushRadius(ds.dm.Radius())
		}
		log.Printf("[%s] /gm cloak on: charID=%d ghost=%v", t.addr, t.charID, ghost)
		msg := "/gm cloak on: cloaked, untargetable, and the cloak never breaks — prop mods, warp and gates leave it alone."
		if ghost {
			msg += " Your ball is 1 m wide (no collision with other ships)."
		}
		return msg, nil
	}

	if wasGhost {
		pushRadius(ds.dm.Radius())
	}
	// SetGMCloak(false) clears the cloaked flag with it, which would make breakGateCloak a no-op;
	// restore the ordinary cloak so it runs the normal decloak and reveal.
	e.SetGMCloak(false, false)
	e.SetCloaked(true)
	breakGateCloak(ds)
	log.Printf("[%s] /gm cloak off: charID=%d", t.addr, t.charID)
	return "/gm cloak off: visible again. /gm on is still set — use /gm off to be targetable.", nil
}

// SetGMMode implements slash.GMModeSetter. Turning it on also sweeps the caller out of every
// NPC threat table (NPCManager.ForgetTarget), because the target pickers refuse only the next
// lock. Turning it off needs no sweep. See docs/main-s10-notes.md#gm-mode-durable.
func (t *slashTeleporter) SetGMMode(on bool) (string, error) {
	if on {
		gmModeChars.Store(t.charID, struct{}{})
	} else {
		gmModeChars.Delete(t.charID)
	}

	// Docked is valid: the flag is restamped onto the entity at undock, so a nil entity is not
	// an error.
	if t.ds != nil && t.ds.entity != nil {
		t.ds.entity.SetGMMode(on)
	}

	if !on {
		log.Printf("[%s] /gm off: charID=%d", t.addr, t.charID)
		return "/gm off: you are targetable again.", nil
	}

	cleared := 0
	if globalNPCMgr != nil && t.ds != nil && t.ds.entity != nil {
		cleared = globalNPCMgr.ForgetTarget(t.ds.entity.ID)
	}
	log.Printf("[%s] /gm on: charID=%d, cleared from %d NPC threat tables", t.addr, t.charID, cleared)

	msg := "/gm on: nothing can lock, approach or shoot you, and NPCs will not notice you."
	if cleared > 0 {
		msg += fmt.Sprintf(" Disengaged %d NPC(s) already on you.", cleared)
	}
	// This is a targeting rule, not invulnerability; the reply says so.
	msg += " You are not invulnerable — /selfdestruct and scripted damage still apply."
	return msg, nil
}

// Stamps reports the caller's destiny stamp stream and the call sites spending it.
// See slash.StampReporter.
func (t *slashTeleporter) Stamps() (string, error) {
	ds := t.ds
	if ds == nil || ds.entity == nil || t.isDocked() {
		return "/stamps: you must be in space (a docked client has no ballpark).", nil
	}
	return ds.entity.StampReport(), nil
}

// Dupes reports balls this client has been handed more than once. See slash.DupeReporter and
// destiny.Entity.MarkBallsKnown. A non-empty answer is a server bug; server.log names the caller.
func (t *slashTeleporter) Dupes() (string, error) {
	ds := t.ds
	if ds == nil || ds.entity == nil {
		return "/dupes: you must be in space (a docked client has no ballpark).", nil
	}
	total, balls := ds.entity.DuplicateAddCount()
	if total == 0 {
		return "/dupes: no ball has been added to this client twice. That is the correct answer.", nil
	}
	return fmt.Sprintf("/dupes: %d redundant AddBalls across %d ball(s) — each one is a doubled overview row\n%s"+
		"Every line above is a delivery path that marked a ball known instead of claiming it; "+
		"server.log names the caller.", total, balls, ds.entity.DuplicateAddReport()), nil
}

// Ghosts implements /ghosts: one line per stale or mid-warp-spared ball in the caller's ballpark,
// plus kept-ball counts by rule. See slash.GhostReporter. It reads ballparkKeepReasons, the set
// the reconciliation prune subtracts. A ball the server has forgotten has no line here.
func (t *slashTeleporter) Ghosts() (string, error) {
	ds := t.ds
	if ds == nil || ds.entity == nil || t.isDocked() {
		return "/ghosts: you must be in space.", nil
	}
	keep, ok := ballparkKeepReasons(ds)
	if !ok {
		return "/ghosts: no ballpark on this connection.", nil
	}
	ss := ds.solarSystem
	me := ds.entity.Pos()

	type row struct {
		id     uint32
		name   string
		dist   float64
		why    string
		stale  bool
		hasEnt bool
	}
	var rows []row
	keptBy := map[string]int{}
	var staleN, sparedN, total int
	for _, id := range ds.entity.KnownBallIDs() {
		total++
		r := row{id: id, why: keep[id]}
		if e := ss.GetEntity(id); e != nil {
			r.hasEnt = true
			r.name = e.BallName()
			r.dist = e.Pos().Sub(me).Len()
			if r.why == "" {
				switch {
				case e.IsInWarp():
					r.why = "SPARED in-warp"
					sparedN++
				case e.IsWarpPending():
					r.why = "SPARED warp-pending"
					sparedN++
				}
			}
		}
		if r.name == "" {
			r.name = "(no entity)"
		}
		if r.why == "" {
			r.why = "STALE"
			r.stale = true
			staleN++
		}
		if r.stale || strings.HasPrefix(r.why, "SPARED") {
			rows = append(rows, r)
		}
		keptBy[r.why]++
	}
	// Only the answer is listed: a line per furniture ball would push the explaining rows off the
	// chat window, so those are counted by rule.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].stale != rows[j].stale {
			return rows[i].stale
		}
		return rows[i].dist < rows[j].dist
	})

	var b strings.Builder
	fmt.Fprintf(&b, "/ghosts: %d ball(s) known to this client — %d stale, %d spared mid-warp\n",
		total, staleN, sparedN)
	for _, r := range rows {
		dist := "      -   "
		if r.hasEnt {
			dist = fmt.Sprintf("%9.0f m", r.dist)
		}
		fmt.Fprintf(&b, " %d  %-26s %s  %s\n", r.id, r.name, dist, r.why)
	}
	kinds := make([]string, 0, len(keptBy))
	for k := range keptBy {
		if k == "STALE" || strings.HasPrefix(k, "SPARED") {
			continue
		}
		kinds = append(kinds, fmt.Sprintf("%s %d", k, keptBy[k]))
	}
	sort.Strings(kinds)
	if len(kinds) > 0 {
		fmt.Fprintf(&b, "kept: %s\n", strings.Join(kinds, ", "))
	}
	if staleN > 0 {
		b.WriteString("STALE rows are queued for the prune: two consecutive 10s passes, so up to 20s.\n")
	}
	b.WriteString("An overview row with no line above is a ball the SERVER has forgotten — no prune can reach it.")
	return b.String(), nil
}

// Scene implements slash.SceneReporter: the server's distance to every ball on the grid, to be
// read against the client's overview distance column. The gap on a static (*) ball is this
// ship's own error, so subtracting it from a moving ball's gap leaves that ball's drift.
// Door lines report the undock and standoff lane occupancy of a nearby station.
func (t *slashTeleporter) Scene(filter string) (string, error) {
	ds := t.ds
	if ds == nil || ds.entity == nil || ds.dm == nil || ds.solarSystem == nil || t.isDocked() {
		return "/scene: you must be in space.", nil
	}
	me := ds.dm.Pos

	type row struct {
		id     uint32
		name   string
		dist   float64
		mode   string
		static bool // not moving, so both sides still agree on where it is
	}
	var rows, anchors []row
	for _, e := range ds.solarSystem.AllEntities() {
		if e == nil || e.ID == ds.entity.ID {
			continue
		}
		if !ds.entity.KnowsBall(e.ID) {
			continue
		}
		name := e.BallName()
		if name == "" {
			name = fmt.Sprintf("type %d", e.Slim.TypeID)
		}
		mode := "-"
		static := true
		if e.Manager != nil {
			mode = destinyModeName(e.Manager.CurrentBallMode())
			static = !e.Manager.IsMoving()
		}
		r := row{e.ID, name, e.Pos().Sub(me).Len(), mode, static}
		// Anchors are collected before the filter: a filtered sample with no anchor cannot be read.
		if r.static {
			anchors = append(anchors, r)
		}
		if filter != "" && !strings.Contains(strings.ToLower(name), filter) {
			continue
		}
		rows = append(rows, r)
	}
	// Loot containers are static ball descriptors, not entities. A wreck is placed at the death
	// position, so its own drift is what the comparison measures.
	if lootBalls, _, _ := ds.solarSystem.GetLootContainers(); len(lootBalls) > 0 {
		for _, bd := range lootBalls {
			if !ds.entity.KnowsBall(bd.EntityID) {
				continue
			}
			// Named as the client names it, so a filter typed off the
			// overview finds the row.
			r := row{bd.EntityID, "cargo container", bd.Pos.Sub(me).Len(), "loot", true}
			anchors = append(anchors, r)
			if filter != "" && !strings.Contains(r.name, filter) {
				continue
			}
			rows = append(rows, r)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].dist < rows[j].dist })
	sort.Slice(anchors, func(i, j int) bool { return anchors[i].dist < anchors[j].dist })

	var b strings.Builder
	fmt.Fprintf(&b, "/scene: ship at (%.0f, %.0f, %.0f) doing %.0f m/s, stamp lead %d, %d ball(s)\n",
		me.X, me.Y, me.Z, ds.dm.Vel.Len(), ds.entity.StampLead(), len(rows))
	for _, r := range rows {
		mark := " "
		if r.static {
			mark = "*"
		}
		fmt.Fprintf(&b, " %s%d  %-28s %9.0f m  %s\n", mark, r.id, r.name, r.dist, r.mode)
	}
	// Door lines: a ship placed unexpectedly near a station is either the lane's doing or not;
	// UndockLaneOccupancy reports the departure and standoff registries separately.
	for _, cel := range ds.celestials {
		if cel.GroupID != celGroupStation {
			continue
		}
		celPos := destiny.Vec3{X: cel.X, Y: cel.Y, Z: cel.Z}
		if celPos.Sub(me).Len() > sceneStationRange {
			continue
		}
		doorTaken, capacity := destiny.UndockLaneOccupancy(cel.ItemID)
		waitTaken, _ := destiny.UndockLaneOccupancy(destiny.StandoffLaneObject(cel.ItemID))
		fmt.Fprintf(&b, "door %d %s: %d/%d undock seats held, %d parked at the standoff, radius %.0f m\n",
			cel.ItemID, cel.Name, doorTaken, capacity, waitTaken, cel.Radius)
		if doorTaken >= capacity {
			b.WriteString("  lane FULL — further departures go to the overflow corridor, up to 26 km down the undock line\n")
		}
	}
	if len(anchors) > 0 {
		a := anchors[0]
		fmt.Fprintf(&b, "anchor: %d %s at %.0f m (* = static, both sides agree on it)\n", a.id, a.name, a.dist)
		b.WriteString("Read the anchor's overview distance first — that gap is this ship. A moving ball's gap minus it is that ball's own.")
	} else {
		b.WriteString("No static ball on grid, so this sample cannot separate this ship's error from the other ball's — stop the ship, or sample where a beacon or wreck is in range.")
	}
	return b.String(), nil
}

func destinyModeName(m destiny.BallMode) string {
	switch m {
	case destiny.ModeGoto:
		return "goto"
	case destiny.ModeStop:
		return "stop"
	case destiny.ModeOrbit:
		return "orbit"
	case destiny.ModeMissile:
		return "missile"
	case destiny.ModeRigid:
		return "rigid"
	}
	return fmt.Sprintf("mode%d", int(m))
}

func (t *slashTeleporter) isDocked() bool {
	return t.stationID != nil && *t.stationID != 0
}

// ── slash.StarbaseNotifier ─────────────────────────────────────────────────
//
// Live destiny push for the /pos* slash commands. All three are best-effort: if the system is
// not loaded, the on-entry loader (loadStarbasesIntoSystem) picks the change up from the DB.

// StarbasePlanted registers a freshly planted tower in the live system and
// broadcasts AddBalls to everyone there.
func (t *slashTeleporter) StarbasePlanted(entityID int64) string {
	if t.sqldb == nil || t.sysMgr == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rec, ok, err := evedb.GetStarbase(ctx, t.sqldb, entityID)
	if err != nil || !ok {
		log.Printf("[%s] StarbasePlanted %d: ok=%v err=%v", t.addr, entityID, ok, err)
		return ""
	}
	ss := t.sysMgr.Get(rec.SolarSystemID)
	if ss == nil {
		return ""
	}
	bd, slim, dmg := starbaseBallSlimDmg(rec)
	ss.AddStarbase(bd, slim, dmg)
	n := broadcastBallAdd(ss, bd, slim, dmg)
	log.Printf("[%s] starbase %d planted live in system %d (%d players notified)",
		t.addr, entityID, rec.SolarSystemID, n)
	return fmt.Sprintf(" Visible now to %d pilot(s) in system.", n)
}

// StarbaseStateChanged re-reads the tower and pushes OnSlimItemChange so the
// new posState (e.g. anchored→online) animates on clients in system.  Also
// arms the onlining completion when applicable.
func (t *slashTeleporter) StarbaseStateChanged(entityID int64) string {
	return starbasePushStateAndSchedule(t.addr, t.sqldb, t.sysMgr, entityID)
}

// StarbaseRemoved drops the tower from the live system and broadcasts
// RemoveBalls.  Called after the DB rows are gone, so it takes the system ID
// from the pre-delete record.
func (t *slashTeleporter) StarbaseRemoved(entityID, solarSystemID int64) string {
	if t.sysMgr == nil {
		return ""
	}
	ss := t.sysMgr.Get(solarSystemID)
	if ss == nil || !ss.RemoveStarbase(uint32(entityID)) {
		return ""
	}
	// The force field (if the tower was online) goes with it.
	ids := []uint32{uint32(entityID)}
	if ss.RemoveStarbase(starbaseForcefieldID(entityID)) {
		ids = append(ids, starbaseForcefieldID(entityID))
	}
	n := broadcastBallRemove(ss, ids...)
	return fmt.Sprintf(" Removed from ballpark for %d pilot(s) in system.", n)
}

// ── slash.MoonScanner ──────────────────────────────────────────────────────

// MoonScan implements /moonscan: resolves the target moon (0 = nearest to the ship), reads or
// lazily generates its resource pool, and pushes OnMoonScanComplete to the caller only. It is the
// same push a survey probe delivers (pushMoonScanResult, moonsurvey.go).
func (t *slashTeleporter) MoonScan(moonID int64) (string, error) {
	if t.sqldb == nil || t.c == nil {
		return "", fmt.Errorf("moonscan: no database or connection")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	moonName := fmt.Sprintf("moon %d", moonID)
	if moonID == 0 {
		if t.ds == nil || t.ds.solarSystem == nil || t.ds.entity == nil {
			return "/moonscan with no argument scans the nearest moon — undock first, or name a moonID.", nil
		}
		pos := t.ds.entity.Pos()
		id, name, dist, found, err := evedb.NearestMoon(ctx, t.sqldb, t.ds.sysID, pos.X, pos.Y, pos.Z)
		if err != nil {
			return "", err
		}
		if !found {
			return "This system has no moons.", nil
		}
		moonID = id
		moonName = fmt.Sprintf("%s (%.0f km away)", name, dist/1000)
	}

	res, err := evedb.GetMoonResources(ctx, t.sqldb, moonID)
	if err != nil {
		return "", err
	}
	if len(res) == 0 {
		// An empty rowset would die on the client's result[0], silently.
		return fmt.Sprintf("Survey of %s: no harvestable materials (empty moon).", moonName), nil
	}
	if err := pushMoonScanResult(t.c, t.accountID, moonID, res); err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Survey of %s — %d material(s), pushed to the Moon Analysis tab:\n", moonName, len(res))
	for _, r := range res {
		name, _ := evedb.GetTypeName(ctx, t.sqldb, r.TypeID)
		if name == "" {
			name = fmt.Sprintf("type %d", r.TypeID)
		}
		fmt.Fprintf(&b, "  %-22s %d\n", name, r.Quantity)
	}
	return b.String(), nil
}

// snapToPosition moves the ship to pos: server position, SetBallPosition and Stop to the client,
// and the bubble transfer (RemoveBalls to old mates, AddBalls to new ones). The caller's own
// ballpark is updated incrementally: a SetState rebuild would wipe known-balls and break later
// incremental AddBalls such as dungeon NPC spawns.
func (t *slashTeleporter) snapToPosition(pos destiny.Vec3) {
	dm := t.ds.dm

	// Departure state is captured before the move, to remove the old dynamic entities afterwards.
	var departNPCs []*destiny.Entity
	var departMates []*destiny.Entity
	departPos := dm.Pos
	if t.ds.solarSystem != nil && t.ds.entity != nil {
		departNPCs = t.ds.solarSystem.GetBubbleNPCs(t.ds.entity.ID)
		departMates = t.ds.solarSystem.GetBubblePlayersExcept(t.ds.entity.ID)
	}

	t.ds.syncStamp()
	dm.SetPosition(pos)
	dm.Stop()
	if err := dm.SendStampedUrgent(func(stamp uint32) error {
		return destiny.SendSimpleAction(t.c, globalNodeID, uint64(t.accountID), stamp,
			"SetBallPosition",
			marshal.PyInt{Value: uint64(dm.EntityID)},
			marshal.PyReal{Value: pos.X},
			marshal.PyReal{Value: pos.Y},
			marshal.PyReal{Value: pos.Z})
	}); err != nil {
		log.Printf("[%s] /tr SetBallPosition: %v", t.addr, err)
	}
	// Tell client to zero velocity so the ship doesn't keep drifting.
	if err := dm.SendStampedUrgent(func(stopStamp uint32) error {
		return destiny.SendSimpleAction(t.c, globalNodeID, uint64(t.accountID), stopStamp,
			"Stop",
			marshal.PyInt{Value: uint64(dm.EntityID)})
	}); err != nil {
		log.Printf("[%s] /tr Stop: %v", t.addr, err)
	}

	// Bubble transfer applies only when the player has a live Entity registered in a SolarSystem.
	if t.ds.solarSystem != nil && t.ds.entity != nil {
		oldBubbleID := t.ds.entity.BubbleID
		left, joined, trMerged := t.ds.solarSystem.TransferBubble(t.ds.entity.ID, pos)
		newBubbleID := t.ds.entity.BubbleID

		// Tell the old bubble that the player ship has disappeared.
		for _, mate := range left {
			if mate.Conn == nil || !mate.KnowsBall(t.ds.entity.ID) {
				continue
			}
			if err := mate.SendStamped(func(mateStamp uint32) error {
				return destiny.SendRemoveBalls(mate.Conn, globalNodeID, mate.AccountID, mateStamp,
					[]uint32{t.ds.entity.ID})
			}); err != nil {
				log.Printf("[%s] /tr bubble-left RemoveBalls: %v", t.addr, err)
			}
			mate.ForgetBalls(t.ds.entity.ID)
		}
		// Tell the new bubble about the arrival.
		if trMerged {
			for _, p := range joined {
				if !p.IsPlayer || p.Conn == nil || p.Conn.IsClosed() {
					continue
				}
				ensureBallsKnown(p, joined...)
			}
		} else {
			for _, mate := range joined {
				ensureBallsKnown(mate, t.ds.entity)
			}
		}

		// Incrementally update the player's own ballpark when the bubble
		// changed: remove departure entities, add arrival entities.
		if oldBubbleID != newBubbleID {
			// Remove departure dynamic entities from the player's ballpark.
			var removeIDs []uint32
			for _, n := range departNPCs {
				if t.ds.entity.KnowsBall(n.ID) {
					removeIDs = append(removeIDs, n.ID)
				}
			}
			for _, m := range departMates {
				if t.ds.entity.KnowsBall(m.ID) {
					removeIDs = append(removeIDs, m.ID)
				}
			}
			astBalls, _, _ := t.ds.solarSystem.NearbyAsteroids(departPos)
			for _, ab := range astBalls {
				if t.ds.entity.KnowsBall(ab.EntityID) {
					removeIDs = append(removeIDs, ab.EntityID)
				}
			}
			bbBalls, _, _ := t.ds.solarSystem.NearbyBillboards(departPos)
			for _, bb := range bbBalls {
				if t.ds.entity.KnowsBall(bb.EntityID) {
					removeIDs = append(removeIDs, bb.EntityID)
				}
			}
			sbBalls, _ := t.ds.solarSystem.NearbyGridStarbases(departPos)
			for _, sb := range sbBalls {
				if t.ds.entity.KnowsBall(sb.EntityID) {
					removeIDs = append(removeIDs, sb.EntityID)
				}
			}
			if len(removeIDs) > 0 {
				t.ds.syncStamp()
				if err := dm.SendStampedUrgent(func(rmStamp uint32) error {
					return destiny.SendRemoveBalls(t.c, globalNodeID, uint64(t.accountID), rmStamp, removeIDs)
				}); err != nil {
					log.Printf("[%s] /tr self RemoveBalls depart: %v", t.addr, err)
				}
				t.ds.entity.ForgetBalls(removeIDs...)
			}

			// Clear target locks: old targets are no longer on grid.
			if len(t.ds.targets) > 0 {
				t.ds.targets = nil
				server.SendNotification(t.c, int64(t.accountID), "OnTarget", "charid",
					marshal.PyTuple{Items: []marshal.PyRep{marshal.PyString{Value: "clear"}}})
			}

			// Add arrival dynamic entities (NPCs, mates, asteroids, billboards).
			trArrBalls, trArrSlims, trArrDmg := buildWarpArrivalBalls(t.ds)
			if len(trArrBalls) > 0 {
				t.ds.syncStamp()
				trFacings := t.ds.solarSystem.StructureFacingActions(trArrBalls, trArrSlims)
				if err := dm.SendStampedUrgent(func(trStamp uint32) error {
					if err := destiny.SendAddBallsFull(t.c, globalNodeID, uint64(t.accountID), trStamp,
						trArrBalls, trArrSlims, trArrDmg); err != nil {
						return err
					}
					if len(trFacings) > 0 {
						return destiny.SendActionList(t.c, globalNodeID, uint64(t.accountID), trStamp, trFacings...)
					}
					return nil
				}); err != nil {
					log.Printf("[%s] /tr arrival AddBalls: %v", t.addr, err)
				}
				ids := make([]uint32, len(trArrBalls))
				for i, b := range trArrBalls {
					ids[i] = b.EntityID
				}
				t.ds.entity.MarkBallsKnown(ids...)
			}
			log.Printf("[%s] /tr bubble transfer %d→%d: removed %d, added arrival balls",
				t.addr, oldBubbleID, newBubbleID, len(removeIDs))
		}
	}

	// Persist ship position in the background.
	if t.sqldb != nil && t.ds.shipID != 0 {
		go func(id int64, p destiny.Vec3) {
			defer safego.Recover("snapToPosition")
			pctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := evedb.UpdateEntityPosition(pctx, t.sqldb, id, p.X, p.Y, p.Z); err != nil {
				log.Printf("[%s] /tr persist position: %v", t.addr, err)
			}
		}(t.ds.shipID, pos)
	}
}

// toCelestial teleports to a mapDenormalize entity.  Cross-system targets first
// jump, then snap onto the celestial.
func (t *slashTeleporter) toCelestial(ctx context.Context, target *evedb.TeleportTarget) (string, error) {
	if t.isDocked() {
		return "/tr: undock first to teleport to a celestial.", nil
	}
	if t.ds == nil || t.ds.dm == nil {
		return "/tr: no live ship state — undock and try again.", nil
	}
	celPos := destiny.Vec3{X: target.X, Y: target.Y, Z: target.Z}
	// Landing offset: just outside the celestial (mirrors warp landing buffer).
	radius := target.Radius
	if radius < 1 {
		radius = 1
	}
	landing := warpLandingPoint(t.ds.dm.Pos, celPos, radius+2500)
	if target.SolarSystemID != 0 && target.SolarSystemID != t.ds.sysID {
		// Cross-system: jump first, then snap onto the celestial.
		if _, err := t.crossSystemJump(ctx, target.SolarSystemID, target.ConstellationID, target.RegionID, landing); err != nil {
			return "", err
		}
		return fmt.Sprintf("/tr: jumped to %s (system %d) and snapped to %s.",
			target.Name, target.SolarSystemID, target.Name), nil
	}
	t.snapToPosition(landing)
	return fmt.Sprintf("/tr: snapped to %s (item %d).", target.Name, target.ID), nil
}

// toCharacter teleports next to another pilot's ship, about 5 km away. A target in another
// solar system is jumped to first. A docked target is refused.
func (t *slashTeleporter) toCharacter(ctx context.Context, target *evedb.TeleportTarget) (string, error) {
	if t.isDocked() {
		return "/tr: undock first to teleport to a pilot.", nil
	}
	if t.ds == nil || t.ds.dm == nil {
		return "/tr: no live ship state — undock and try again.", nil
	}
	if target.SolarSystemID == 0 {
		return fmt.Sprintf("/tr: pilot %q has no solar system on record.", target.Name), nil
	}
	if target.X == 0 && target.Y == 0 && target.Z == 0 {
		return fmt.Sprintf("/tr: pilot %q appears to be docked or unlocated.", target.Name), nil
	}
	targetPos := destiny.Vec3{X: target.X, Y: target.Y, Z: target.Z}
	// Land ~5km away so we don't materialise inside their hull.
	landing := warpLandingPoint(t.ds.dm.Pos, targetPos, 5_000)
	if target.SolarSystemID != t.ds.sysID {
		if _, err := t.crossSystemJump(ctx, target.SolarSystemID, target.ConstellationID, target.RegionID, landing); err != nil {
			return "", err
		}
		return fmt.Sprintf("/tr: jumped to %s (system %d) and landed near %s.",
			target.Name, target.SolarSystemID, target.Name), nil
	}
	t.snapToPosition(landing)
	return fmt.Sprintf("/tr: landed ~5km from %s (charID %d).", target.Name, target.ID), nil
}

// toSystem teleports to a solar system, landing at landing (defaults to sun
// at 0,0,0 when zero).
func (t *slashTeleporter) toSystem(ctx context.Context, target *evedb.TeleportTarget, landing destiny.Vec3) (string, error) {
	if t.isDocked() {
		return "/tr: undock first to teleport between systems.", nil
	}
	if t.ds == nil || t.ds.dm == nil {
		return "/tr: no live ship state — undock and try again.", nil
	}
	if target.SolarSystemID == t.ds.sysID {
		// Already there: snap near the star instead of at origin.
		if landing.IsZero() {
			landing = starLandingPoint(t.ds.celestials)
		}
		t.snapToPosition(landing)
		return fmt.Sprintf("/tr: already in %s; snapped to sun.", target.Name), nil
	}
	if _, err := t.crossSystemJump(ctx, target.SolarSystemID, target.ConstellationID, target.RegionID, landing); err != nil {
		return "", err
	}
	return fmt.Sprintf("/tr: jumped to %s (system %d).", target.Name, target.ID), nil
}

// toStation docks the caller at the given station.  Works both when already
// docked (re-uses the /moveto code path effectively) and when in space.
func (t *slashTeleporter) toStation(ctx context.Context, target *evedb.TeleportTarget) (string, error) {
	if t.isDocked() {
		// Docked → docked: behave like /moveto.
		if *t.stationID == target.ID {
			return "/tr: you are already docked at that station.", nil
		}
		if t.ds == nil {
			return "/tr: no session state.", nil
		}
		oldStationID := *t.stationID
		// Move ship entity and update character row.
		shipID, _ := evedb.GetCurrentShipID(ctx, t.sqldb, t.charID)
		if shipID > 0 {
			if err := evedb.UpdateEntityLocation(ctx, t.sqldb, shipID, target.ID); err != nil {
				return "", fmt.Errorf("move ship: %w", err)
			}
		}
		if err := evedb.UpdateCharacterStation(ctx, t.sqldb, t.charID,
			target.ID, target.SolarSystemID, target.ConstellationID, target.RegionID); err != nil {
			return "", fmt.Errorf("update char station: %w", err)
		}
		*t.stationID = target.ID
		// Build the same SCN the /moveto path uses.
		src, err := evedb.GetStationLocation(ctx, t.sqldb, oldStationID)
		if err != nil {
			return "", fmt.Errorf("source station: %w", err)
		}
		dst := &evedb.StationLocation{
			StationID: target.ID, StationName: target.Name,
			SolarSystemID: target.SolarSystemID, ConstellationID: target.ConstellationID, RegionID: target.RegionID,
		}
		if err := sendMoveToSCNFromSlashTeleporter(t, src, dst, shipID); err != nil {
			return fmt.Sprintf("/tr: moved to %s (DB updated) but session change failed: %v — relog to sync.",
				target.Name, err), nil
		}
		return fmt.Sprintf("/tr: moved to %s (station %d).", target.Name, target.ID), nil
	}
	// In-space → docked: tear down destiny state, persist, send dock SCN.
	if t.ds == nil || t.ds.dm == nil {
		return "/tr: no live ship state.", nil
	}
	// If the station is in a different system, do the cross-system jump first
	// so the dock SCN's sysID matches the destination.
	if target.SolarSystemID != t.ds.sysID {
		if _, err := t.crossSystemJump(ctx, target.SolarSystemID, target.ConstellationID, target.RegionID,
			destiny.Vec3{X: target.X, Y: target.Y, Z: target.Z}); err != nil {
			return "", err
		}
	}
	shipID := t.ds.shipID
	if shipID > 0 {
		if err := evedb.UpdateEntityLocation(ctx, t.sqldb, shipID, target.ID); err != nil {
			log.Printf("[%s] /tr dock: UpdateEntityLocation: %v", t.addr, err)
		}
	}
	if err := evedb.UpdateCharacterStation(ctx, t.sqldb, t.charID, target.ID,
		target.SolarSystemID, target.ConstellationID, target.RegionID); err != nil {
		log.Printf("[%s] /tr dock: UpdateCharacterStation: %v", t.addr, err)
	}
	unregisterDock(t.addr, t.ds, t.sqldb)
	sysID := t.ds.sysID
	t.ds.dm = nil
	*t.stationID = target.ID
	if err := sendDockSessionChange(t.c, t.addr, uint64(t.accountID), sysID, target.ID, shipID); err != nil {
		return fmt.Sprintf("/tr: docked at %s (DB updated) but session change failed: %v — relog to sync.",
			target.Name, err), nil
	}
	return fmt.Sprintf("/tr: docked at %s (station %d).", target.Name, target.ID), nil
}

// sendMoveToSCNFromSlashTeleporter builds the SCN entries slash.cmdMoveTo would build, on this
// connection.
func sendMoveToSCNFromSlashTeleporter(t *slashTeleporter, src, dst *evedb.StationLocation, shipID int64) error {
	none := marshal.PyNone{}
	entries := []marshal.DictEntry{
		{Key: marshal.PyString{Value: "stationid"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(src.StationID)}, marshal.PyInt{Value: uint64(dst.StationID)},
		}}},
		{Key: marshal.PyString{Value: "solarsystemid2"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(src.SolarSystemID)}, marshal.PyInt{Value: uint64(dst.SolarSystemID)},
		}}},
		{Key: marshal.PyString{Value: "locationid"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(src.StationID)}, marshal.PyInt{Value: uint64(dst.StationID)},
		}}},
		{Key: marshal.PyString{Value: "solarsystemid"}, Value: marshal.PyTuple{Items: []marshal.PyRep{none, none}}},
	}
	if src.ConstellationID != dst.ConstellationID {
		entries = append(entries, marshal.DictEntry{
			Key: marshal.PyString{Value: "constellationid"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyInt{Value: uint64(src.ConstellationID)}, marshal.PyInt{Value: uint64(dst.ConstellationID)},
			}},
		})
	}
	if src.RegionID != dst.RegionID {
		entries = append(entries, marshal.DictEntry{
			Key: marshal.PyString{Value: "regionid"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyInt{Value: uint64(src.RegionID)}, marshal.PyInt{Value: uint64(dst.RegionID)},
			}},
		})
	}
	if shipID > 0 {
		entries = append(entries, marshal.DictEntry{
			Key: marshal.PyString{Value: "shipid"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyInt{Value: uint64(shipID)}, marshal.PyInt{Value: uint64(shipID)},
			}},
		})
	}
	return sendArbitrarySCN(t.c, uint64(t.accountID), entries)
}

// sendArbitrarySCN sends a SessionChangeNotification with the supplied entries.
func sendArbitrarySCN(c *evenet.Conn, accountID uint64, entries []marshal.DictEntry) error {
	payload := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: 0},
			marshal.PyDict{Items: entries},
		}},
		marshal.PyList{Items: []marshal.PyRep{marshal.PyInt{Value: globalNodeID}}},
	}}
	pkt := &evenet.PyPacket{
		TypeString: "macho.SessionChangeNotification",
		Type:       evenet.SessionChangeNotification,
		Source: evenet.PyAddress{
			Type:   evenet.AddrNode,
			TypeID: globalNodeID,
		},
		Dest: evenet.PyAddress{
			Type:   evenet.AddrClient,
			TypeID: 130293001608,
		},
		UserID:  accountID,
		Payload: payload,
		NamedPayload: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "channel"}, Value: marshal.PyString{Value: "sessionchange"}},
		}},
	}
	b, err := marshal.Marshal(pkt.Encode())
	if err != nil {
		return err
	}
	return c.SendPacket(b)
}

// crossSystemJump moves the caller from the current system into destSysID, landing at
// landingPos (zero means the sun). It follows the StargateJump flow without the gate flashes and
// timing phases.
func (t *slashTeleporter) crossSystemJump(ctx context.Context, destSysID, destConstID, destRegID int64, landingPos destiny.Vec3) (string, error) {
	if t.ds == nil || t.ds.dm == nil {
		return "", fmt.Errorf("crossSystemJump: no ship state")
	}
	dm := t.ds.dm

	// Drop from source bubble and notify mates. The ship keeps its capacitor and shield.
	captureShipCharge(t.ds)
	unregisterDock(t.addr, t.ds, t.sqldb)

	oldSysID := t.ds.sysID
	oldConstID := t.ds.constellationID
	oldRegID := t.ds.regionID

	if err := loadSystemForConn(t.addr, t.sqldb, destSysID, t.ds); err != nil {
		log.Printf("[%s] /tr loadSystem sysID=%d: %v", t.addr, destSysID, err)
	}
	// Resolve landing position after loading celestials so we can find the star.
	if landingPos.IsZero() {
		landingPos = starLandingPoint(t.ds.celestials)
	}
	dm.SetPosition(landingPos)
	dm.Stop()
	t.ds.sysID = destSysID
	t.ds.constellationID = destConstID
	t.ds.regionID = destRegID

	// Re-register in the destination system.
	registerUndock(t.c, t.addr, uint64(t.accountID), t.charID, t.sqldb, t.ds)

	// Persist the character location before the session change, so GetChannels does not read the
	// old system and rejoin its channel.
	pctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	if err := evedb.UpdateCharacterLocation(pctx, t.sqldb, t.charID, destSysID, destConstID, destRegID); err != nil {
		log.Printf("[%s] /tr persist location: %v", t.addr, err)
	}
	cancel()

	// Swap local chat channel: leave old system, join new system.
	lsc.SwapSystemChannel(t.c, t.accountID, oldSysID, destSysID, globalLSCReg, t.sqldb)

	if err := sendJumpSessionChange(t.c, t.addr, uint64(t.accountID),
		oldSysID, destSysID, oldConstID, destConstID, oldRegID, destRegID); err != nil {
		log.Printf("[%s] /tr jump SCN: %v", t.addr, err)
	}
	// Fresh SetState in new bubble.
	sendSetState(t.c, t.addr, uint64(t.accountID), t.charID, t.ds)

	// Persist position in the background.
	go func(shipID int64, p destiny.Vec3) {
		defer safego.Recover("crossSystemJump")
		pctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if shipID != 0 {
			if err := evedb.UpdateEntityPosition(pctx, t.sqldb, shipID, p.X, p.Y, p.Z); err != nil {
				log.Printf("[%s] /tr persist position: %v", t.addr, err)
			}
		}
	}(t.ds.shipID, landingPos)

	return fmt.Sprintf("jumped %d→%d", oldSysID, destSysID), nil
}

// registerConquerableStations creates Entity objects for each station celestial
// (groupID=15) in a nullsec system, giving them mutable HP so players can
// shoot them. The station remains in CelBalls/CelSlims for rendering.
func registerConquerableStations(ss *destiny.SolarSystem, celestials []evedb.CelestialEntity, sqldb *sql.DB, addr string) {
	count := 0
	for _, cel := range celestials {
		if cel.GroupID != 15 { // Station groupID
			continue
		}
		if registerConquerableStation(ss, cel, sqldb, addr) {
			count++
		}
	}
	if count > 0 {
		log.Printf("[%s] registered %d conquerable stations in nullsec system %d (sec=%.2f)",
			addr, count, ss.ID, ss.Security)
	}
}

// registerConquerableStation registers one station. It is separate from the loop because an
// outpost built mid-session must become shootable at once. The station type's own `conquerable`
// flag is the gate; only the outpost types and the Conquerable hulls carry it.
func registerConquerableStation(ss *destiny.SolarSystem, cel evedb.CelestialEntity, sqldb *sql.DB, addr string) bool {
	entityID := uint32(cel.ItemID)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	conquerable, err := evedb.IsStationTypeConquerable(ctx, sqldb, cel.TypeID)
	cancel()
	if err != nil {
		log.Printf("[%s] registerConquerableStation: stationID=%d typeID=%d conquerable query failed: %v",
			addr, cel.ItemID, cel.TypeID, err)
		return false
	}
	if !conquerable {
		return false
	}

	// Fetch station type HP and resistances.
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	ehp, err := evedb.GetEntityHPByType(ctx, sqldb, cel.TypeID)
	cancel()
	if err != nil {
		log.Printf("[%s] registerConquerableStation: stationID=%d typeID=%d HP query failed: %v",
			addr, cel.ItemID, cel.TypeID, err)
		return false
	}
	// Stations have massive HP: default generously if the DB returns tiny values.
	if ehp.ShieldHP <= 0 {
		ehp.ShieldHP = 1_000_000
	}
	if ehp.ArmorHP <= 0 {
		ehp.ArmorHP = 1_000_000
	}
	if ehp.StructureHP <= 0 {
		ehp.StructureHP = 1_000_000
	}

	entity := conquerableStationEntity(cel, ehp)
	ss.RegisterStationEntity(entity)
	// Update CelDamage so the station starts with full HP fractions.
	ss.SetCelDamage(entityID, entity.Damage)
	log.Printf("[%s] registered conquerable station %q (ID=%d typeID=%d) shield=%.0f armor=%.0f structure=%.0f",
		addr, cel.Name, cel.ItemID, cel.TypeID, ehp.ShieldHP, ehp.ArmorHP, ehp.StructureHP)
	return true
}

// celestialSubType maps a mapDenormalize groupID to the destiny SubType byte: 66 for stargates
// and stations (groups 10 and 15), 2 for everything else. Bodies are not IsMassive (0x04),
// because destiny.dll collides the ego ball with massive spheres even in ModeWarp.
// See docs/main-s10-notes.md#celestial-subtype.
func celestialSubType(groupID int64) uint8 {
	switch groupID {
	case 10, 15:
		return 66 // stations and stargates: Global|HasMiniBalls (docking geometry)
	default:
		return 2 // star, planet, moon, belt: Global, non-massive
	}
}

// makeCelestialBallDescs converts mapDenormalize rows into destiny BallDesc
// entries using ModeRigid and the appropriate SubType.
func makeCelestialBallDescs(entities []evedb.CelestialEntity) []destiny.BallDesc {
	out := make([]destiny.BallDesc, 0, len(entities))
	for _, e := range entities {
		bd := destiny.BallDesc{
			EntityID: uint32(e.ItemID),
			Mode:     destiny.ModeRigid,
			SubType:  celestialSubType(e.GroupID),
			Pos:      destiny.Vec3{X: e.X, Y: e.Y, Z: e.Z},
			Radius:   e.Radius,
			Name:     e.Name,
		}
		// Stations and stargates need one hard-coded miniball (collision geometry).
		if bd.SubType == 66 {
			bd.MiniBalls = []destiny.MiniBall{
				{X: -7701.181, Y: 8060.06, Z: 27878.9, Radius: 1639.241},
			}
		}
		out = append(out, bd)
	}
	return out
}

// starbaseBallSlimDmg builds the destiny triple for one starbase record, shared by the system
// loader and the live /posplant broadcast. The ball is SubType 6: the client silently rejects a
// SubType 66 ball of a non-station category in SetState. The model comes from the SlimItem typeID.
func starbaseBallSlimDmg(r evedb.StarbaseRecord) (destiny.BallDesc, destiny.SlimItem, destiny.DamageState) {
	bd := destiny.BallDesc{
		EntityID: uint32(r.EntityID),
		Mode:     destiny.ModeRigid,
		SubType:  6,
		Pos:      destiny.Vec3{X: r.X, Y: r.Y, Z: r.Z},
		Radius:   r.Radius,
		Name:     r.ItemName,
		CorpID:   uint32(r.OwnerCorpID),
	}
	slimNow := server.Win32TimeNow()
	posState := r.PosStateAt(slimNow)
	slim := destiny.SlimItem{
		BallID:     r.EntityID,
		ItemID:     r.EntityID,
		TypeID:     r.TypeID,
		GroupID:    r.GroupID,
		CategoryID: r.CategoryID,
		OwnerID:    r.OwnerCorpID,
		CorpID:     r.OwnerCorpID,
		AllianceID: r.OwnerAllianceID,
		// PosState/PosTimestamp come from the record together. Reinforced carries its future end time:
		// the client's GetStructureState raises on state 3 without one.
		PosState:     &posState,
		PosTimestamp: r.PosTimestampAt(slimNow),
		// Slaves only: the client plays the onlining FX (playerOwned.py:40-55) on the tower with this ID.
		ControlTowerID: r.ControlTowerID,
	}
	// The management window polls GetDamageState every second with no guards (moonmining.py:520),
	// so the tower is always in the damage map.
	dmg := fullDamageState()
	return bd, slim, dmg
}

// Force-field constants. The client finds the shield sphere by scanning slimItems for
// const.groupForceField with the tower's ownerID (moonmining.py GetForcefield) and scales the
// model to ball radius x2 (forceField.py). Type 16103 is the only type in group 411.
const (
	forcefieldTypeID  = 16103
	forcefieldGroupID = 411 // const.groupForceField (category 2, Celestial)
	forcefieldCatID   = 2
	// forcefieldHarmonic is the "Shield harmonic value" an online tower's field advertises
	// (destiny.dll ball getset table). moonmining.py reads any value outside {0, -1} as active.
	forcefieldHarmonic = 1
)

// starbaseForcefieldID returns the ephemeral ball ID of a tower's force field
// (ephemeralball.go: it never gets an entity row, and must stay under 2^31).
func starbaseForcefieldID(towerEntityID int64) uint32 {
	return ephemeralBallID(forcefieldBallBand, towerEntityID)
}

// starbaseForcefield builds the ball/slim/damage triple for an online tower's
// force-field sphere.  Radius comes from the tower type's shieldRadius dogma
// attribute (30 km for large towers in the RMR data), falling back to 30 km.
func starbaseForcefield(sqldb *sql.DB, r evedb.StarbaseRecord) (destiny.BallDesc, destiny.SlimItem, destiny.DamageState) {
	// Same radius the defense-anchoring gate measures against (evedb.ForceFieldAnchorRefusal).
	radius := evedb.DefaultTowerShieldRadius
	if sqldb != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		radius = evedb.TowerShieldRadius(ctx, sqldb, r.TypeID)
		cancel()
	}
	ffID := starbaseForcefieldID(r.EntityID)
	// Layout: ModeStop + SubType 1, the shape launched structures and ships use. ModeField desyncs
	// the client's ball parser. See docs/main-s10-notes.md#forcefield-ball-layout.
	bd := destiny.BallDesc{
		EntityID: ffID,
		Mode:     destiny.ModeStop,
		SubType:  1, // ShipSector present (zeroed velocity)
		Pos:      destiny.Vec3{X: r.X, Y: r.Y, Z: r.Z},
		Radius:   radius,
		Mass:     1.0e9,
		MaxSpeed: 1,
		Agility:  1,
		Name:     "Force Field",
		CorpID:   uint32(r.OwnerCorpID),
		Harmonic: forcefieldHarmonic,
	}
	// The ball's corpID/allianceID drive the Force Field tab's two "Allow ... members" checkboxes
	// (-1 = not allowed); the stored flags apply only with starbase.enforceAccess on.
	if sqldb != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if evedb.StarbaseAccessEnforced(ctx, sqldb) {
			if p, err := evedb.GetStarbasePolicy(ctx, sqldb, r); err == nil {
				if !p.FFAllowCorp {
					bd.CorpID = 0xFFFFFFFF
				}
				if p.FFAllowAlliance && r.OwnerAllianceID != 0 {
					bd.AllianceID = uint32(r.OwnerAllianceID)
				}
			}
		}
		cancel()
	}
	slim := destiny.SlimItem{
		BallID:     int64(ffID),
		ItemID:     int64(ffID),
		TypeID:     forcefieldTypeID,
		GroupID:    forcefieldGroupID,
		CategoryID: forcefieldCatID,
		OwnerID:    r.OwnerCorpID,
		CorpID:     r.OwnerCorpID,
		AllianceID: r.OwnerAllianceID,
	}
	dmg := fullDamageState()
	return bd, slim, dmg
}

// starbaseOnliningDelayMs returns the tower type's onliningDelay dogma
// attribute in milliseconds (client counts the same value down), with the
// RMR large-tower default as fallback.
func starbaseOnliningDelayMs(sqldb *sql.DB, typeID int64) float64 {
	const fallback = 1_800_000 // 30 min, matches attributeID 677 for towers
	if sqldb == nil {
		return fallback
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if v, ok, err := evedb.GetTypeAttributeFloat(ctx, sqldb, typeID, "onliningDelay"); err == nil && ok && v > 0 {
		return v
	}
	return fallback
}

// scheduleStarbaseOnliningCompletion arms the one-shot that finishes an onlining tower. Calling
// it twice for one tower is safe: the DB transition in CompleteStarbaseOnlining is guarded.
func scheduleStarbaseOnliningCompletion(sqldb *sql.DB, entityID int64, fireAt time.Time) {
	if globalTickMgr == nil {
		return
	}
	globalTickMgr.ScheduleOnce(fmt.Sprintf("pos-online-%d", entityID), fireAt, func() {
		finishStarbaseOnlining(sqldb, entityID)
	})
}

// scheduleStructureOnliningCompletion is the slave counterpart. Its timer key differs from the
// tower's, so the two never cancel each other.
func scheduleStructureOnliningCompletion(sqldb *sql.DB, entityID int64, fireAt time.Time) {
	if globalTickMgr == nil {
		return
	}
	globalTickMgr.ScheduleOnce(fmt.Sprintf("pos-struct-online-%d", entityID), fireAt, func() {
		finishStructureOnlining(sqldb, entityID)
	})
}

// finishStructureOnlining completes an onlining slave structure and pushes
// the new state. Unlike a tower there is no force field to raise.
func finishStructureOnlining(sqldb *sql.DB, entityID int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ok, err := evedb.CompleteStructureOnlining(ctx, sqldb, entityID)
	if err != nil {
		log.Printf("[pos] finishStructureOnlining %d: %v", entityID, err)
		return
	}
	if !ok {
		return // removed or state changed since scheduling
	}
	log.Printf("[pos] structure %d onlining complete", entityID)
	structurePushStateAndSchedule("pos", sqldb, globalSysMgr, entityID)
	// An assembly array or lab gains its industry lines the moment it is up.
	if err := evedb.SyncStructureAssemblyLines(ctx, sqldb, entityID); err != nil {
		log.Printf("[pos] assembly lines for %d: %v", entityID, err)
	}
}

// finishStarbaseOnlining flips onlining to online in the DB, then, if the system is live,
// updates the tower slim, spawns the force-field ball and broadcasts both.
func finishStarbaseOnlining(sqldb *sql.DB, entityID int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ok, err := evedb.CompleteStarbaseOnlining(ctx, sqldb, entityID)
	if err != nil {
		log.Printf("[pos] finishStarbaseOnlining %d: %v", entityID, err)
		return
	}
	if !ok {
		return // removed or state changed since scheduling
	}
	rec, found, err := evedb.GetStarbase(ctx, sqldb, entityID)
	if err != nil || !found {
		return
	}
	log.Printf("[pos] starbase %d onlining complete in system %d", entityID, rec.SolarSystemID)
	evedb.EmitStarbaseEvent(evedb.StarbaseEventFor(evedb.StarbaseEventOnline, rec))
	if globalSysMgr == nil {
		return
	}
	ss := globalSysMgr.Get(rec.SolarSystemID)
	if ss == nil {
		return // not loaded; on-entry loader will present the online state
	}
	_, slim, _ := starbaseBallSlimDmg(rec)
	ss.UpdateStarbaseSlim(uint32(entityID), slim)

	ffBd, ffSlim, ffDmg := starbaseForcefield(sqldb, rec)
	if _, exists := ss.GetStarbasePosition(ffBd.EntityID); !exists {
		ss.AddStarbase(ffBd, ffSlim, ffDmg)
	}

	// The tower's new posState goes to every pilot holding the tower (system-wide); the force field
	// only to pilots on its grid who do not hold it.
	for _, p := range ss.GetAllPlayersInSystem() {
		if p.Conn == nil {
			continue
		}
		slimTo := p.KnowsBall(uint32(entityID))
		ffTo := !p.KnowsBall(ffBd.EntityID) && destiny.StarbaseBallVisibleFrom(ffBd, ffSlim, p.Pos())
		if !slimTo && !ffTo {
			continue
		}
		if err := p.SendStamped(func(stamp uint32) error {
			if slimTo {
				if err := destiny.SendSlimItemChange(p.Conn, globalNodeID, p.AccountID, stamp, slim); err != nil {
					return err
				}
			}
			if !ffTo {
				return nil
			}
			return destiny.SendAddBallsFull(p.Conn, globalNodeID, p.AccountID, stamp,
				[]destiny.BallDesc{ffBd}, []destiny.SlimItem{ffSlim},
				map[uint32]destiny.DamageState{ffBd.EntityID: ffDmg})
		}); err == nil && ffTo {
			p.ClaimUnknownBalls(ffBd.EntityID)
		}
	}
}

// loadStarbasesIntoSystem loads the persisted starbases of sysID into ss so they appear in the
// ballpark. Errors are logged and do not block system loading.
func loadStarbasesIntoSystem(addr string, sqldb *sql.DB, ss *destiny.SolarSystem, sysID int64) {
	if sqldb == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	recs, err := evedb.ListStarbasesForSystem(ctx, sqldb, sysID)
	if err != nil {
		log.Printf("[%s] loadStarbasesIntoSystem sysID=%d: %v", addr, sysID, err)
		return
	}
	for _, r := range recs {
		// A tower with no moon shows "(not attached to moon)" and an empty Moon tab in the management
		// window; the nearest-moon resolve repairs it now that the system is loaded.
		if r.MoonID == 0 {
			if moonID, err := evedb.BackfillStarbaseMoon(ctx, sqldb, r.EntityID); err != nil {
				log.Printf("[%s] starbase %d moon backfill: %v", addr, r.EntityID, err)
			} else {
				r.MoonID = moonID
			}
		}
		// Onlining recovery: a completion one-shot lost to a restart is finished now (delay elapsed)
		// or re-armed for the remainder.
		if r.State == evedb.StarbaseStateOnlining {
			delayMs := starbaseOnliningDelayMs(sqldb, r.TypeID)
			elapsedMs := float64(server.Win32TimeNow()-r.OnlinedAt) / 10000.0
			if elapsedMs >= delayMs {
				if ok, err := evedb.CompleteStarbaseOnlining(ctx, sqldb, r.EntityID); err == nil && ok {
					r.State = evedb.StarbaseStateOnline
				}
			} else {
				scheduleStarbaseOnliningCompletion(sqldb, r.EntityID,
					time.Now().Add(time.Duration(delayMs-elapsedMs)*time.Millisecond))
			}
		}

		// Reinforcement whose timer died with the process: settle a past-due one before its slim is
		// built (a state-3 slim with a past timestamp breaks the client), re-arm the exit otherwise.
		if r.State == evedb.StarbaseStateReinforced {
			if evedb.ReinforcementDue(r, server.Win32TimeNow()) {
				if ok, err := evedb.ExitReinforcement(ctx, sqldb, r.EntityID); err == nil && ok {
					r.State, r.ReinforceUntil = evedb.StarbaseStateOnline, 0
				}
			} else {
				scheduleReinforceExit(sqldb, r.EntityID, r.ReinforceUntil)
			}
		}

		bd, slim, dmg := starbaseBallSlimDmg(r)
		ss.AddStarbase(bd, slim, dmg)
		if r.State == evedb.StarbaseStateOnline || r.State == evedb.StarbaseStateReinforced {
			ffBd, ffSlim, ffDmg := starbaseForcefield(sqldb, r)
			ss.AddStarbase(ffBd, ffSlim, ffDmg)
		}
		log.Printf("[%s] starbase loaded: entityID=%d typeID=%d groupID=%d categoryID=%d "+
			"pos=(%.2f,%.2f,%.2f) radius=%.1f name=%q owner=corp:%d ally:%d state=%s",
			addr, r.EntityID, r.TypeID, r.GroupID, r.CategoryID,
			r.X, r.Y, r.Z, r.Radius, r.ItemName, r.OwnerCorpID, r.OwnerAllianceID, r.State)
	}

	// Slave structures anchored to those towers use the same ball builder; the slim differs only by
	// ControlTowerID, which the client needs to raise onlining FX on the tower.
	slaves, err := evedb.ListStarbaseStructuresForSystem(ctx, sqldb, sysID)
	if err != nil {
		log.Printf("[%s] loadStarbasesIntoSystem structures sysID=%d: %v", addr, sysID, err)
	}
	for _, r := range slaves {
		if r.State == evedb.StarbaseStateOnlining {
			delayMs := starbaseOnliningDelayMs(sqldb, r.TypeID)
			elapsedMs := float64(server.Win32TimeNow()-r.OnlinedAt) / 10000.0
			if elapsedMs >= delayMs {
				if ok, err := evedb.CompleteStructureOnlining(ctx, sqldb, r.EntityID); err == nil && ok {
					r.State = evedb.StarbaseStateOnline
				}
			} else {
				scheduleStructureOnliningCompletion(sqldb, r.EntityID,
					time.Now().Add(time.Duration(delayMs-elapsedMs)*time.Millisecond))
			}
		}
		bd, slim, dmg := starbaseBallSlimDmg(r)
		ss.AddStarbase(bd, slim, dmg)
		log.Printf("[%s] starbase structure loaded: entityID=%d typeID=%d group=%d tower=%d state=%s",
			addr, r.EntityID, r.TypeID, r.GroupID, r.ControlTowerID, r.State)
	}

	// Launched but unanchored structures reload like platforms: the entity row survives a system
	// unload but the ball does not, and the Anchor menu hangs off the ball.
	free, err := evedb.ListFreeStructuresForSystem(ctx, sqldb, sysID)
	if err != nil {
		log.Printf("[%s] loadStarbasesIntoSystem free sysID=%d: %v", addr, sysID, err)
		return
	}
	for _, f := range free {
		allianceID, aErr := evedb.GetCorpAllianceID(ctx, sqldb, f.OwnerID)
		if aErr != nil {
			allianceID = 0
		}
		bd, slim, dmg := launchedStructureBallSlim(f.Structure,
			destiny.Vec3{X: f.X, Y: f.Y, Z: f.Z}, f.OwnerID, f.OwnerID, allianceID)
		ss.AddStarbase(bd, slim, dmg)
		log.Printf("[%s] unanchored structure loaded: itemID=%d typeID=%d group=%d owner=corp:%d",
			addr, f.Structure.ItemID, f.Structure.TypeID, f.Structure.GroupID, f.OwnerID)
	}
}

// makeCelestialSlimItems builds SlimItem descriptors for all solar system
// celestials.  Stations use the owning corporation from the DB; stargates
// default to CONCORD (1000125); everything else is owned by itemID 1 (the universe).
func makeCelestialSlimItems(entities []evedb.CelestialEntity) []destiny.SlimItem {
	out := make([]destiny.SlimItem, 0, len(entities))
	for _, e := range entities {
		ownerID := int64(0)
		if e.OwnerID != 0 {
			ownerID = e.OwnerID
		} else if e.GroupID == 15 {
			ownerID = 1000125 // default NPC corp for stations with missing corp
		} else if e.GroupID == 10 {
			ownerID = 1000125 // CONCORD for stargates
		}
		out = append(out, destiny.SlimItem{
			BallID:             e.ItemID,
			ItemID:             e.ItemID,
			TypeID:             e.TypeID,
			GroupID:            e.GroupID,
			CategoryID:         e.CategoryID,
			OwnerID:            ownerID,
			CorpID:             ownerID,
			JumpsToStargate:    e.DestStargateID,
			JumpsToSolarSystem: e.DestSolarSystemID,
		})
	}
	return out
}

// destinyExtractXYZ reads x, y, z floats starting at offset in items.
func destinyExtractXYZ(items []marshal.PyRep, offset int) (x, y, z float64) {
	if offset+2 < len(items) {
		x = destinyExtractFloat(items[offset])
		y = destinyExtractFloat(items[offset+1])
		z = destinyExtractFloat(items[offset+2])
	}
	return
}

func destinyExtractFloat(rep marshal.PyRep) float64 {
	switch v := rep.(type) {
	case marshal.PyReal:
		return v.Value
	case marshal.PyInt:
		return float64(v.Value)
	}
	return 0
}

// findStarPosition returns the position and radius of the star (groupID 6) in
// the given celestial list.  If no star is found, ok is false.
func findStarPosition(celestials []evedb.CelestialEntity) (pos destiny.Vec3, radius float64, ok bool) {
	for _, c := range celestials {
		if c.GroupID == 6 { // Sun / Star
			return destiny.Vec3{X: c.X, Y: c.Y, Z: c.Z}, c.Radius, true
		}
	}
	return destiny.Vec3{}, 0, false
}

// starLandingPoint returns a safe position near the system star, offset by the
// star's radius plus a 2500 m buffer along the +X axis.  Falls back to the raw
// starPos if the celestial list is empty.
func starLandingPoint(celestials []evedb.CelestialEntity) destiny.Vec3 {
	starPos, starRadius, ok := findStarPosition(celestials)
	if !ok {
		return starPos // zero position
	}
	// Offset along +X so the ship isn't inside the star.
	return destiny.Vec3{
		X: starPos.X + starRadius + 2500,
		Y: starPos.Y,
		Z: starPos.Z,
	}
}

// invGroups IDs for celestial bodies with static warp-in points.
const (
	celGroupStar   = 6
	celGroupPlanet = 7
	celGroupMoon   = 8
)

// celGroupStation is not a static warp-in group (a station gets the ordinary approach
// radial); /scene reports its door's lane occupancy by this group.
const celGroupStation = 15

// sceneStationRange is how close /scene must be to a station to print its lane
// occupancy. 100 km reads as parked at this door: a station radius reaches 56 km.
const sceneStationRange = 100_000.0

// Warp-in clearance beyond the star's surface, in metres. Planets and moons
// take their altitude from the body (evedb.PlanetWarpInAltitude /
// MoonWarpInAltitude).
const starWarpInBuffer = 50_000.0

// celestialWarpInPoint returns the fixed warp-in point for a star, planet or moon and the
// outward unit vector through it; ok is false for other types, which keep the
// approach-vector landing. The altitude scales with the body (db/celestialwarpin.go).
// See docs/main-s11-notes.md#static-warp-in-line.
func celestialWarpInPoint(celestials []evedb.CelestialEntity, cel *evedb.CelestialEntity) (point, outward destiny.Vec3, ok bool) {
	pos := destiny.Vec3{X: cel.X, Y: cel.Y, Z: cel.Z}
	var refPos destiny.Vec3
	var dist float64
	switch cel.GroupID {
	case celGroupMoon:
		parent := findCelestialByID(celestials, cel.OrbitID)
		if parent == nil {
			// No orbit parent recorded; fall back to the star.
			starPos, _, found := findStarPosition(celestials)
			if !found {
				return destiny.Vec3{}, destiny.Vec3{}, false
			}
			refPos = starPos
		} else {
			refPos = destiny.Vec3{X: parent.X, Y: parent.Y, Z: parent.Z}
		}
		dist = cel.Radius + evedb.MoonWarpInAltitude(cel.Radius)
	case celGroupPlanet:
		starPos, _, found := findStarPosition(celestials)
		if !found {
			return destiny.Vec3{}, destiny.Vec3{}, false
		}
		refPos = starPos
		dist = cel.Radius + evedb.PlanetWarpInAltitude(cel.Radius)
	case celGroupStar:
		// The star has no orbit parent; offset along +X like starLandingPoint.
		outward = destiny.Vec3{X: 1}
		return pos.Add(outward.Scale(cel.Radius*1.05 + starWarpInBuffer)), outward, true
	default:
		return destiny.Vec3{}, destiny.Vec3{}, false
	}
	outward = pos.Sub(refPos)
	if outward.IsZero() {
		outward = destiny.Vec3{X: 1}
	} else {
		outward = outward.Normalize()
	}
	return pos.Add(outward.Scale(dist)), outward, true
}

// warpPathBlocked reports whether the straight line from `from` to `to` passes through
// the sphere at center (surface radius plus a clearance margin). destiny.dll tests the
// ego ball against celestials even in ModeWarp, and a clipping line drops it out of warp.
// See docs/main-s11-notes.md#static-warp-in-line.
func warpPathBlocked(from, to, center destiny.Vec3, surfaceRadius float64) bool {
	margin := 0.01 * surfaceRadius
	if margin < 10_000 {
		margin = 10_000
	}
	r := surfaceRadius + margin
	seg := to.Sub(from)
	segLen := seg.Len()
	if segLen < 1 {
		return false
	}
	dir := seg.Scale(1 / segLen)
	rel := center.Sub(from)
	t := rel.X*dir.X + rel.Y*dir.Y + rel.Z*dir.Z
	if t < 0 {
		t = 0
	} else if t > segLen {
		t = segLen
	}
	closest := from.Add(dir.Scale(t))
	return closest.Sub(center).Len() < r
}

func findCelestialByID(celestials []evedb.CelestialEntity, itemID int64) *evedb.CelestialEntity {
	if itemID == 0 {
		return nil
	}
	for i := range celestials {
		if celestials[i].ItemID == itemID {
			return &celestials[i]
		}
	}
	return nil
}

// deadspaceLandingBuffer is how far short of a deadspace anchor a warp stops: close
// enough that the beacon is what you arrive at, far enough to be outside its model.
// Acceleration gates and deadspace bookmarks use it in place of the generic warp-to range.
const deadspaceLandingBuffer = 2500.0

// siteLandingBuffer is how far short of a dungeon beacon or deadspace bookmark a warp stops.
// The pilot's requested warp-to range wins; no range (autopilot, journal) or zero falls back to
// deadspaceLandingBuffer. Acceleration gates do not use this: their landing is fixed.
func siteLandingBuffer(requested float64, requestedSet bool) float64 {
	if requestedSet && requested > 0 {
		return requested
	}
	return deadspaceLandingBuffer
}

// defaultWarpToRange is the landing range of an item warp sent with no minRange, as the
// autopilot sends for gates; the client's own menu default is also 15 km.
const defaultWarpToRange = 15000.0

// observerWarpVisibleSecs is how long a warping ship stays in bystanders' ballparks:
// until its warp curve leaves the grid (destiny.GridExitSec), bounded by tunnelSecs.
// See docs/main-s11-notes.md#departure-removal-timing.
var observerWarpVisibleSecs = destiny.GridExitSec

// maxWarpSparePasses bounds how many consecutive reconciliation passes may spare a
// mid-warp ball from the prune; 12 passes of the 10 s sweep outlast any warp.
const maxWarpSparePasses = 12

// selfDepartureVisibleSecs bounds how long the warping pilot's view of the grid they left
// survives: GridExitSec plus selfDepartureMarginSecs, from the server's tunnel start.
// See docs/main-s11-notes.md#departure-removal-timing.
var selfDepartureVisibleSecs = destiny.GridExitSec + selfDepartureMarginSecs

// selfDepartureMarginSecs covers tick application, the per-tick warp gate and the
// uplink; see selfDepartureVisibleSecs.
const selfDepartureMarginSecs = 3.0

// selfDepartureHold is how long after the server's tunnel start the departure grid comes
// off the pilot's ballpark: selfDepartureVisibleSecs plus the align, bounded by the
// tunnel. The client may arm warp up to an align later than the server models.
// See docs/main-s11-notes.md#departure-removal-timing.
func selfDepartureHold(alignSecs, tunnelSecs float64) float64 {
	return math.Min(tunnelSecs, selfDepartureVisibleSecs+alignSecs)
}

// warpLandingPoint returns the warp landing position: offset from celestialPos
// toward shipPos by landingOffset metres (targetRadius + 2500 m).
// This ensures the player lands outside the celestial body.
func warpLandingPoint(shipPos, celestialPos destiny.Vec3, landingOffset float64) destiny.Vec3 {
	dir := shipPos.Sub(celestialPos)
	if dir.IsZero() {
		// Degenerate: the ship is inside the celestial; offset along +X.
		return destiny.Vec3{X: celestialPos.X + landingOffset, Y: celestialPos.Y, Z: celestialPos.Z}
	}
	return celestialPos.Add(dir.Normalize().Scale(landingOffset))
}

// sendRep marshals rep and writes it as a length-framed packet.
func sendRep(c *evenet.Conn, rep marshal.PyRep) error {
	b, err := marshal.Marshal(rep)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	return c.SendPacket(b)
}

// readRep reads one length-framed packet and unmarshals it.
func readRep(c *evenet.Conn) (marshal.PyRep, error) {
	raw, err := c.ReadPacket()
	if err != nil {
		return nil, err
	}
	return marshal.Unmarshal(raw)
}

// ── Module activation helpers ─────────────────────────────────────────────────

func sendGodmaShipEffect(c *evenet.Conn, accountID int64, itemID, typeID, shipID, charID int64, targetID uint32, effectName string, start bool, durationMs int64, repeat int64) {
	sendGodmaShipEffectSeeded(c, accountID, itemID, typeID, shipID, charID, targetID, effectName, start, durationMs, repeat, nil)
}

// sendGodmaShipEffectSeeded is sendGodmaShipEffect with an optional WichmannHill 3-tuple
// randomSeed (arg index 10). A seed plus a target makes the client replay chance-based
// cycles locally (DOGMA_MATH.md §8), so the server must roll identically.
func sendGodmaShipEffectSeeded(c *evenet.Conn, accountID int64, itemID, typeID, shipID, charID int64, targetID uint32, effectName string, start bool, durationMs int64, repeat int64, seed *[3]int64) {
	startVal := int64(0)
	activeVal := int64(0)
	if start {
		startVal = 1
		activeVal = 1
	}
	effectID := int64(0)
	if ei, ok := effectMap[effectName]; ok {
		effectID = ei.EffectID
	}
	if durationMs <= 0 {
		durationMs = 60000
	}
	var targetRep marshal.PyRep = marshal.PyNone{}
	if targetID != 0 {
		targetRep = marshal.PyInt{Value: uint64(targetID)}
	}
	env := marshal.PyList{Items: []marshal.PyRep{
		marshal.PyInt{Value: uint64(itemID)},
		marshal.PyInt{Value: uint64(charID)},
		marshal.PyInt{Value: uint64(shipID)},
		targetRep,
		marshal.PyNone{},
		marshal.PyList{},
		marshal.PyInt{Value: uint64(effectID)},
	}}
	var repeatRep marshal.PyRep = marshal.PyNone{}
	if repeat > 0 {
		repeatRep = marshal.PyInt{Value: uint64(repeat)}
	}
	// randomSeed must be a 3-tuple: the client assigns it raw to Python 2.3
	// WichmannHill._seed, whose state unpacks as (x, y, z).
	var seedRep marshal.PyRep = marshal.PyNone{}
	if seed != nil {
		seedRep = marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(seed[0])},
			marshal.PyInt{Value: uint64(seed[1])},
			marshal.PyInt{Value: uint64(seed[2])},
		}}
	}
	args := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyInt{Value: uint64(itemID)},
		marshal.PyInt{Value: uint64(effectID)},
		server.Win32TimeNowRep(),
		marshal.PyInt{Value: uint64(startVal)},
		marshal.PyInt{Value: uint64(activeVal)},
		env,
		server.Win32TimeNowRep(),
		marshal.PyInt{Value: uint64(durationMs)},
		repeatRep,
		seedRep,
		marshal.PyNone{},
	}}
	if err := server.SendNotification(c, accountID, "OnGodmaShipEffect", "charid", args); err != nil {
		log.Printf("sendGodmaShipEffect: %v", err)
	}
}

// isResistEffectName reports whether activating/deactivating this effect can
// change the ship's effective resistances: hardener and damage-control active
// effects, plus any module online/offline transition (passive resist modules
// contribute while online).
func isResistEffectName(name string) bool {
	switch name {
	case "online",
		"modifyActiveShieldResonanceAndNullifyPassiveResonance",
		"modifyActiveArmorResonanceAndNullifyPassiveResonance",
		"damageControl":
		return true
	}
	return false
}

// recalcShipResists recomputes a player entity's resistances from the hull's
// base resonances and the ship's online/active resist modules (hardeners,
// platings, amplifiers, damage controls). Called at undock and on resist
// module state changes.
func recalcShipResists(ds *destinyConn, sqldb *sql.DB) {
	if ds == nil || ds.entity == nil || sqldb == nil || ds.shipTypeID == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ehp, err := evedb.GetEntityHPByType(ctx, sqldb, ds.shipTypeID)
	if err != nil {
		return
	}
	active := make(map[int64]bool, len(ds.activeModules))
	for id, mod := range ds.activeModules {
		if mod != nil && mod.isActive {
			active[id] = true
		}
	}
	mult, err := evedb.GetShipResistanceMultipliers(ctx, sqldb, ds.shipID, active)
	if err != nil {
		log.Printf("recalcShipResists ship=%d: %v", ds.shipID, err)
		return
	}
	r := [3][4]float64{ehp.ShieldRes, ehp.ArmorRes, ehp.HullRes}
	for l := 0; l < 3; l++ {
		for d := 0; d < 4; d++ {
			r[l][d] *= mult[l][d]
		}
	}
	// Hull resistance bonuses, applied as registerUndock applies them.
	if hullB, hbErr := evedb.GetShipHullBonuses(ctx, sqldb, ds.charID, ds.shipTypeID); hbErr == nil && len(hullB) > 0 {
		hullRes := hullB.ResistMults()
		for l := 0; l < 2; l++ {
			for d := 0; d < 4; d++ {
				r[l][d] *= hullRes[l][d]
			}
		}
	}
	// SetBaseResists folds in the gang bonus under the entity lock.
	ds.entity.SetBaseResists(r)
}

// consumeCapForModule deducts capNeed from the player's capacitor and pushes the new
// charge to the client (attr 18; the client re-seeds its recharge extrapolation from it,
// DOGMA_MATH.md §2). Returns false when the capacitor cannot cover the cost.
func consumeCapForModule(c *evenet.Conn, accountID, charID int64, ds *destinyConn, capNeed float64) bool {
	if capNeed <= 0 || ds == nil || ds.entity == nil || !ds.entity.HasCap() {
		return true
	}
	newVal, ok := ds.entity.ConsumeCap(capNeed)
	if ok {
		sendModuleAttributeChange(c, accountID, charID, ds.shipID, dogma.AttrCharge, newVal, newVal+capNeed)
	}
	return ok
}

// capMaintLoop drains capacitor once per cycle for continuously running
// modules that have no dedicated cycle loop (prop mods, active hardeners,
// damage controls). On cap-out the module is force-deactivated server-side.
func capMaintLoop(ctx context.Context, c *evenet.Conn, addr string, accountID, charID int64, ds *destinyConn, modItemID, modTypeID int64, effectName string, durationMs int64, capNeed float64, sqldb *sql.DB) {
	defer safego.Recover("capMaintLoop")
	if durationMs <= 0 {
		durationMs = 5000
	}
	cycle := time.Duration(durationMs) * time.Millisecond
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(cycle):
		}
		if !consumeCapForModule(c, accountID, charID, ds, capNeed) {
			log.Printf("[%s] cap out: force-deactivating mod=%d (%s)", addr, modItemID, effectName)
			stopWeaponFX(c, addr, accountID, charID, ds, modItemID, modTypeID, 0, effectName, sqldb)
			if ds.entity != nil {
				if newSpeed, removed := ds.entity.RemoveSpeedModifier(modItemID); removed {
					broadcastSetMaxSpeed(ds, ds.entity, newSpeed)
					clearMWDBloom(ds.entity)
				}
			}
			if isResistEffectName(effectName) {
				recalcShipResists(ds, sqldb)
			}
			pushStatsAfterModuleChange(c, accountID, charID, ds, modTypeID, sqldb)
			disengageModuleCloak(ds, ds.entity, modItemID, modTypeID, sqldb)
			return
		}
	}
}

// sendModuleAttributeChange sends an OnModuleAttributeChange notification for
// the given item, typically the ship itself when cpuLoad/powerLoad change.
func sendModuleAttributeChange(c *evenet.Conn, accountID, charID, itemID, attributeID int64, newValue, oldValue float64) {
	args := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyInt{Value: uint64(charID)},
		marshal.PyInt{Value: uint64(itemID)},
		marshal.PyInt{Value: uint64(attributeID)},
		server.Win32TimeNowRep(),
		attrValueRep(attributeID, newValue),
		attrValueRep(attributeID, oldValue),
	}}
	if err := server.SendNotification(c, accountID, "OnModuleAttributeChange", "charid", args); err != nil {
		log.Printf("sendModuleAttributeChange: %v", err)
	}
}

// typeNameOrID returns the invTypes name for a typeID, or the numeric ID when the lookup
// fails, for error messages.
func typeNameOrID(ctx context.Context, sqldb *sql.DB, typeID int64) string {
	if name, err := evedb.GetTypeName(ctx, sqldb, typeID); err == nil && name != "" {
		return name
	}
	return fmt.Sprintf("type %d", typeID)
}

// chargeUnloadDest picks where an auto-unloaded charge goes: the ship's cargo hold when it
// fits, otherwise the station hangar when docked.
func chargeUnloadDest(ctx context.Context, sqldb *sql.DB, chargeER *evedb.EntityRow, shipID, stationID, charID int64) (int64, int) {
	if shipID != 0 {
		fits := true
		if shipER, _ := evedb.GetEntityRow(ctx, sqldb, shipID); shipER != nil && chargeER != nil {
			cargoCap, _ := evedb.GetShipCargoCapacityFitted(ctx, sqldb, shipER.TypeID, shipID)
			used, _ := evedb.GetCargoUsedVolume(ctx, sqldb, charID, shipID)
			vol, _ := evedb.GetTypeVolume(ctx, sqldb, chargeER.TypeID)
			fits = vol*float64(chargeER.Quantity) <= cargoCap-used
		}
		if fits || stationID == 0 {
			return shipID, 5
		}
	}
	if stationID != 0 {
		return stationID, 4
	}
	return shipID, 5
}

// skillTrainErrorPayload turns a StartTrainingSkill failure into the client's
// SkillHasPrerequisites or CharacterAlreadyKnowsSkill exception, else nil. A failed
// train is never answered with a fabricated OnSkillStartTraining: the client would show
// the refused skill training while the real one keeps running.
func skillTrainErrorPayload(err error) marshal.PyRep {
	if pe := evedb.AsSkillPrereqError(err); pe != nil {
		return server.BuildUserError("SkillHasPrerequisites", map[string]marshal.PyRep{
			"requiredSkills": marshal.PyString{Value: pe.Text},
		})
	}
	if errors.Is(err, evedb.ErrSkillMaxLevel) {
		return server.BuildUserError("CharacterAlreadyKnowsSkill", map[string]marshal.PyRep{
			"skillName": marshal.PyString{Value: "that skill"},
		})
	}
	return nil
}

// missingFitSkills returns a comma-separated "Skill Name IV" list of the
// prerequisites charID lacks for fitting the given module type, or "" when
// all are met. Basic Miner (3651) is exempt so rookies can always fit it.
func missingFitSkills(ctx context.Context, sqldb *sql.DB, charID, typeID int64) string {
	if charID == 0 || typeID == 3651 {
		return ""
	}
	reqs, err := evedb.GetRequiredSkills(ctx, sqldb, typeID)
	if err != nil {
		return ""
	}
	roman := [6]string{"", "I", "II", "III", "IV", "V"}
	var missing []string
	for _, req := range reqs {
		have, _ := evedb.GetCharacterSkillLevel(ctx, sqldb, charID, req.SkillID)
		if have < req.Level {
			lvl := ""
			if req.Level >= 1 && req.Level <= 5 {
				lvl = " " + roman[req.Level]
			}
			missing = append(missing, typeNameOrID(ctx, sqldb, req.SkillID)+lvl)
		}
	}
	return strings.Join(missing, ", ")
}

// boardSkillError returns a ShipHasSkillPrerequisites user error when charID
// lacks the skills to fly the ship entity shipID, or nil when boarding is
// allowed. Both Board paths gate on it: the client only greys out the "Make
// Active" entry.
func boardSkillError(ctx context.Context, sqldb *sql.DB, charID, shipID int64) marshal.PyRep {
	if sqldb == nil || charID == 0 || shipID == 0 {
		return nil
	}
	er, err := evedb.GetEntityRow(ctx, sqldb, shipID)
	if err != nil || er == nil {
		return nil
	}
	missing := missingFitSkills(ctx, sqldb, charID, er.TypeID)
	if missing == "" {
		return nil
	}
	return server.BuildUserError("ShipHasSkillPrerequisites", map[string]marshal.PyRep{
		"itemName":       marshal.PyString{Value: typeNameOrID(ctx, sqldb, er.TypeID)},
		"requiredSkills": marshal.PyString{Value: missing},
	})
}

// boardFacts is what boardRefusal reads from the database, split out so the
// ownership rules can be tested without one.
type boardFacts struct {
	ship          *evedb.EntityRow // nil: no such item
	currentShipID int64            // where the pilot sits now
	otherPilot    bool             // another character entity is inside the ship
}

// boardRefusalKey is the ownership half of boardRefusal: the client message
// key (and text, for CustomError) refusing the board, or "" when it may go on.
// A ship is boardable only by its owner, where the pilot is (this station's
// hangar at flag 4, or the ship they sit in), assembled, with the seat empty.
func boardRefusalKey(charID, stationID int64, f boardFacts) (key, text string) {
	er := f.ship
	if er == nil {
		return "CantShipNotPresent", ""
	}
	if er.OwnerID != charID {
		if er.Flag >= 116 && er.Flag <= 121 {
			// Divisions 2-7 exist only in an office: the client's own
			// refusal for it. Division 1 shares flag 4 with every hangar.
			return "CantPilotAShipInCorpHangar", ""
		}
		return "CustomError", "That ship belongs to someone else."
	}
	if er.CategoryID != 6 || er.Singleton == 0 {
		return "CustomError", "Only an assembled ship can be boarded."
	}
	if er.ItemID != f.currentShipID && (stationID == 0 || er.LocationID != stationID || er.Flag != 4) {
		return "CantShipNotPresent", ""
	}
	if f.otherPilot {
		return "CannotBoardAsPilot", ""
	}
	return "", ""
}

// assembleRefusal gates both AssembleShip paths (inline bind and bound): each
// itemID must be the pilot's own packaged ship in the station hangar. The
// client offers the entry only on such a ship but sends any itemID. Returns a
// UserError to raise, or nil when every ID may be assembled.
func assembleRefusal(ctx context.Context, sqldb *sql.DB, charID, stationID int64, itemIDs []int64) marshal.PyRep {
	for _, id := range itemIDs {
		er, err := evedb.GetEntityRow(ctx, sqldb, id)
		if err != nil {
			log.Printf("assembleRefusal: item %d: %v", id, err)
		}
		if er == nil || stationID == 0 || er.LocationID != stationID {
			return server.BuildUserError("CantShipNotPresent", nil)
		}
		if er.OwnerID != charID {
			if er.Flag >= 116 && er.Flag <= 121 {
				return server.BuildUserError("CantPilotAShipInCorpHangar", nil)
			}
			return server.BuildUserError("CustomError", map[string]marshal.PyRep{
				"error": marshal.PyString{Value: "That ship belongs to someone else."}})
		}
		if er.CategoryID != 6 {
			group, _ := evedb.ESPGetGroupName(ctx, sqldb, er.GroupID)
			return server.BuildUserError("AssembleShipsOnly", map[string]marshal.PyRep{
				"group": marshal.PyString{Value: group}})
		}
		if er.Flag != 4 {
			return server.BuildUserError("CantShipNotPresent", nil)
		}
	}
	return nil
}

// boardRefusal gates every Board path: ownership and presence first
// (boardRefusalKey), then the hull's skill prerequisites (boardSkillError).
// A failed lookup refuses.
func boardRefusal(ctx context.Context, sqldb *sql.DB, charID, stationID, shipID int64) marshal.PyRep {
	if sqldb == nil || charID == 0 {
		return nil
	}
	var f boardFacts
	er, err := evedb.GetEntityRow(ctx, sqldb, shipID)
	if err != nil {
		log.Printf("boardRefusal: ship %d: %v", shipID, err)
	}
	f.ship = er
	if er != nil {
		f.currentShipID, _ = evedb.GetCurrentShipID(ctx, sqldb, charID)
		var n int
		if err := sqldb.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM entity WHERE locationID = ? AND flag = 57 AND itemID <> ?`,
			shipID, charID).Scan(&n); err != nil {
			log.Printf("boardRefusal: pilot check ship %d: %v", shipID, err)
			n = 1
		}
		f.otherPilot = n > 0
	}
	switch key, text := boardRefusalKey(charID, stationID, f); key {
	case "":
	case "CustomError":
		return server.BuildUserError("CustomError", map[string]marshal.PyRep{
			"error": marshal.PyString{Value: text}})
	case "CantShipNotPresent":
		return server.BuildUserError("CantShipNotPresent", nil)
	case "CantPilotAShipInCorpHangar":
		return server.BuildUserError("CantPilotAShipInCorpHangar", nil)
	case "CannotBoardAsPilot":
		return server.BuildUserError("CannotBoardAsPilot", nil)
	default:
		return server.BuildUserError("CustomError", map[string]marshal.PyRep{
			"error": marshal.PyString{Value: "You cannot board that ship."}})
	}
	return boardSkillError(ctx, sqldb, charID, shipID)
}

// sendShipFittingStats pushes the ship's CPU/powergrid load and effective
// capacity after a fitting change. ShipGetInfo bakes skill and fitting-module
// bonuses into cpuOutput (48) / powerOutput (11), so the client cannot derive
// the new capacity when a co-processor, RCU, PDS or aux power core toggles.
func sendShipFittingStats(c *evenet.Conn, accountID, charID, shipID int64, sqldb *sql.DB) {
	if sqldb == nil || shipID == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cpuLoad, pgLoad, err := evedb.GetShipCPUPowerLoad(ctx, sqldb, shipID)
	if err != nil {
		log.Printf("sendShipFittingStats: load: %v", err)
		return
	}
	sendModuleAttributeChange(c, accountID, charID, shipID, dogma.AttrCPULoad, cpuLoad, 0)
	sendModuleAttributeChange(c, accountID, charID, shipID, dogma.AttrPowerLoad, pgLoad, 0)
	shipER, err := evedb.GetEntityRow(ctx, sqldb, shipID)
	if err != nil || shipER == nil {
		return
	}
	cpuCap, pgCap, err := evedb.GetShipCPUPGCapacityFitted(ctx, sqldb, shipER.TypeID, shipID)
	if err != nil {
		log.Printf("sendShipFittingStats: capacity: %v", err)
		return
	}
	sendModuleAttributeChange(c, accountID, charID, shipID, dogma.AttrCPUOutput, cpuCap, 0)
	sendModuleAttributeChange(c, accountID, charID, shipID, dogma.AttrPowerOutput, pgCap, 0)
	sendShipDerivedStats(c, accountID, charID, shipID, shipER, sqldb)
}

// sendShipDerivedStats pushes the fitting-window stat-panel attributes that
// move when modules fit/unfit or online/offline: HP pools, capacitor, recharge
// rates, velocity, agility, sensors and all twelve resonances. Values come from
// computeShipDerivedAttrs, the same math ShipGetInfo reports.
func sendShipDerivedStats(c *evenet.Conn, accountID, charID, shipID int64, shipER *evedb.EntityRow, sqldb *sql.DB) {
	if sqldb == nil || shipID == 0 || shipER == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	attrs := computeShipDerivedAttrs(ctx, "derived-stats", sqldb, charID, shipID, shipER)
	pushIDs := []int64{
		dogma.AttrCapacity,
		dogma.AttrHP,
		dogma.AttrShieldCapacity,
		dogma.AttrArmorHP,
		dogma.AttrCapacitorCapacity,
		dogma.AttrRechargeRate,
		dogma.AttrShieldRechargeRate,
		dogma.AttrMaxVelocity,
		dogma.AttrAgility,
		dogma.AttrScanResolution,
		dogma.AttrMaxTargetRange,
	}
	pushIDs = append(pushIDs, dogma.AllResonanceAttrs()...)
	for _, id := range pushIDs {
		if v, ok := attrs[id]; ok {
			sendModuleAttributeChange(c, accountID, charID, shipID, id, v, 0)
		}
	}
}

// broadcastSlimItemChange re-reads fitted modules from the DB, updates the
// entity's SlimItem, and broadcasts OnSlimItemChange to all bubble-mates so
// other players see turret model changes.
func broadcastSlimItemChange(c *evenet.Conn, accountID int64, ds *destinyConn, shipID int64, sqldb *sql.DB) {
	if ds == nil || ds.entity == nil || ds.solarSystem == nil || sqldb == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	fittedMods, err := evedb.GetFittedModules(ctx, sqldb, shipID)
	cancel()
	if err != nil {
		log.Printf("broadcastSlimItemChange: GetFittedModules: %v", err)
		return
	}
	mods := make([]destiny.ModuleRef, 0, len(fittedMods))
	for _, m := range fittedMods {
		mods = append(mods, destiny.ModuleRef{ItemID: m.ItemID, TypeID: m.TypeID})
	}
	ds.entity.Slim.Modules = mods
	mates := ds.solarSystem.GetBubbleConnectedPlayersExcept(ds.entity.ID)
	for _, mate := range mates {
		if err := mate.SendStamped(func(stamp uint32) error {
			return destiny.SendSlimItemChange(mate.Conn, globalNodeID, mate.AccountID, stamp, ds.entity.Slim)
		}); err != nil {
			log.Printf("broadcastSlimItemChange to mate %d: %v", mate.ID, err)
		}
	}
}

// moduleFXGUID resolves an effect name to the client FX GUID it draws with:
// the dgmEffects row, then the hand-written fallback for effects shipped with
// an empty GUID, then the normalisation onto a visual this build has a class
// for. The per-cycle module loops use it to find their jam bucket.
func moduleFXGUID(effectName string) string {
	guid := ""
	if ei, ok := effectMap[effectName]; ok {
		guid = ei.Guid
	}
	if guid == "" {
		guid, _ = dogma.FallbackGUID(effectName)
	}
	return dogma.NormalizeClientGUID(guid)
}

func sendModuleFX(c *evenet.Conn, addr string, accountID int64, ds *destinyConn, modItemID, modTypeID, modSlotFlag int64, targetID uint32, effectName string, start bool, durationMs int64, repeat int64, sqldb *sql.DB) {
	if ds.dm == nil {
		return
	}
	guid := moduleFXGUID(effectName)
	isOffensive := int64(0)
	ei, haveEffect := effectMap[effectName]
	if haveEffect && ei.IsOffensive {
		isOffensive = 1
	}
	// With no DB GUID the hand-written table supplies the offensive flag too.
	if !haveEffect || ei.Guid == "" {
		if _, fbOff := dogma.FallbackGUID(effectName); fbOff {
			isOffensive = 1
		}
	}
	// Some offensive effects (missiles) aren't flagged in effectMap; force it.
	if isOffensive == 0 {
		switch effectName {
		case "laserFired", "projectileFired", "hybridFired", "missileLaunching", "useMissiles":
			isOffensive = 1
		}
	}
	startVal := int64(0)
	if start {
		startVal = 1
	}
	if durationMs <= 0 {
		durationMs = 10000
	}
	// The loaded charge typeID tints the beam or picks the projectile model; a
	// modulated mining laser's crystal tints its beam too.
	var otherTypeID int64
	if sqldb != nil && start && (isOffensive == 1 || effectName == dogma.EffNameMiningLaser) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		chargeTypeID, chargeErr := evedb.GetChargeTypeID(ctx, sqldb, ds.shipID, modSlotFlag)
		cancel()
		if chargeErr == nil && chargeTypeID != 0 {
			otherTypeID = chargeTypeID
		}
	}
	ds.syncStamp()
	// otherTypeID must be PyNone when no charge is loaded: turret.py SetAmmoColor
	// tests `is not None` and then looks the value up, so an integer 0 raises.
	var otherTypeRep marshal.PyRep = marshal.PyNone{}
	if otherTypeID != 0 {
		otherTypeRep = marshal.PyInt{Value: uint64(otherTypeID)}
	}
	// targetID must be PyNone when untargeted: fxSequencer.AddTrigger drops the
	// whole trigger when a non-None targetID has no ball, and 0 never does.
	var targetRep marshal.PyRep = marshal.PyNone{}
	if targetID != 0 {
		targetRep = marshal.PyInt{Value: uint64(targetID)}
	}
	// Blood client (build 3913) OnSpecialFX uses 10 params.
	fxArgs := []marshal.PyRep{
		marshal.PyInt{Value: uint64(ds.dm.EntityID)}, // entityID (int)
		marshal.PyInt{Value: uint64(modItemID)},      // moduleID (int)
		marshal.PyInt{Value: uint64(modTypeID)},      // moduleTypeID (int)
		targetRep,                                    // targetID (None if untargeted)
		otherTypeRep,                                 // otherTypeID (None if no charge)
		marshal.PyList{},                             // area (listInt)
		marshal.PyString{Value: guid},                // effect_type (string)
		marshal.PyInt{Value: uint64(isOffensive)},    // isOffensive (int)
		marshal.PyInt{Value: uint64(startVal)},       // start (int)
		marshal.PyInt{Value: uint64(startVal)},       // active (int)
	}
	if startVal == 1 {
		// duration + repeat make the client cycle the effect (turret shots,
		// beam pulses) on its own until the stop event arrives.
		fxArgs = append(fxArgs,
			marshal.PyReal{Value: float64(durationMs)},
			marshal.PyInt{Value: destiny.FXRepeatCount},
		)
	}
	// Every player EWAR module passes through here, so this is where the victim's
	// jam bar is raised and dropped. The module itemID is the bar key, the same key
	// the attacker's godma uses for its local EffectRepeater, so one bar, not two.
	var jamEvents []marshal.PyRep
	if jt, ok := destiny.JamBucketForFXGUID(guid); ok && targetID != 0 {
		jamEvents = append(jamEvents, destiny.BuildJamEvent(start,
			ds.dm.EntityID, modItemID, targetID, jt,
			server.Win32TimeNow(), durationMs))
	}
	fxAction := destiny.BuildSimpleAction("OnSpecialFX", fxArgs...)
	if err := ds.sendSelfFXStamped(func(stamp uint32) error {
		return destiny.SendActionListEvents(c, globalNodeID, uint64(accountID), stamp,
			jamEvents, fxAction)
	}); err != nil {
		log.Printf("[%s] sendModuleFX self: %v", addr, err)
	}
	if ds.solarSystem != nil && ds.entity != nil {
		// The client fxSequencer drops FX for a target ball the mate does not hold:
		// deliver our ship, and a dynamic target, first.
		fxTarget := ds.solarSystem.GetEntity(targetID)
		mates := ds.solarSystem.GetBubbleConnectedPlayersExcept(ds.entity.ID)
		for _, mate := range mates {
			if !ensureBallsKnown(mate, ds.entity, fxTarget) {
				continue
			}
			if err := mate.SendFXStamped(func(mateStamp uint32) error {
				return destiny.SendActionListEvents(mate.Conn, globalNodeID, mate.AccountID, mateStamp,
					jamEvents, fxAction)
			}); err != nil {
				log.Printf("[%s] sendModuleFX to mate %d: %v", addr, mate.ID, err)
			}
		}
	}
}

// ── Mining cycle ──────────────────────────────────────────────────────────────

// miningCrystalState tracks the crystal loaded into a modulated miner for the
// lifetime of one mining session: yield boost while intact, volatility wear
// per cycle, shatter on reaching its hitpoints.
type miningCrystalState struct {
	itemID      int64
	typeID      int64
	yieldMult   float64
	getsDamaged bool
	volChance   float64
	volDamage   float64
	hp          float64
	damage      float64
	broken      bool
	used        bool // marked singleton this activation (crystalwear.go)
}

// removeAsteroidFromClients pulls a depleted asteroid out of every client
// ballpark in the system without an explosion. The server-side record stays
// for the respawn ticker.
func removeAsteroidFromClients(ss *destiny.SolarSystem, entityID uint32) {
	if ss == nil {
		return
	}
	for _, p := range ss.GetAllPlayersInSystem() {
		if p.Conn == nil || !p.KnowsBall(entityID) {
			continue
		}
		p.SendStamped(func(stamp uint32) error {
			return destiny.SendRemoveBalls(p.Conn, globalNodeID, p.AccountID, stamp, []uint32{entityID})
		})
		p.ForgetBalls(entityID)
	}
}

// broadcastAsteroidRadius pushes a mined rock's new size to every client that
// holds its ball. SetBallRadius is a native ballpark op, so each recipient
// needs its own fresh stamp (SendStamped, not SendFXStamped). Call only when
// UpdateAsteroidRadius reports a change.
func broadcastAsteroidRadius(ss *destiny.SolarSystem, entityID uint32, radius float64) {
	if ss == nil || radius <= 0 {
		return
	}
	for _, p := range ss.GetAllPlayersInSystem() {
		if p.Conn == nil || !p.KnowsBall(entityID) {
			continue
		}
		if err := p.SendStamped(func(stamp uint32) error {
			return destiny.SendSetBallRadius(p.Conn, globalNodeID, p.AccountID, stamp, entityID, radius)
		}); err != nil {
			log.Printf("broadcastAsteroidRadius asteroid=%d to entity=%d: %v", entityID, p.ID, err)
		}
	}
}

func miningCycleLoop(ctx context.Context, c *evenet.Conn, addr string, accountID int64, charID int64, ds *destinyConn, modItemID, modTypeID int64, targetID uint32, repeat int64, durationMs int64, miningAmountM3 float64, capNeed float64, crystal *miningCrystalState, stopCh <-chan struct{}, sqldb *sql.DB) {
	defer safego.Recover("miningCycleLoop")
	if durationMs <= 0 {
		durationMs = 60000 // 60s default
	}
	if miningAmountM3 <= 0 {
		miningAmountM3 = 40.0 // Miner I default
	}
	miningAmountM3 *= rates.MiningYield
	cycleDuration := time.Duration(durationMs) * time.Millisecond

	// Module range for the per-cycle proximity check (ship may drift/warp off).
	maxRange := 15000.0
	if sqldb != nil && modTypeID != 0 {
		rangeCtx, rangeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if attrs, err := evedb.GetTypeAttributes(rangeCtx, sqldb, modTypeID); err == nil {
			if r, ok := attrs[dogma.AttrMaxRange]; ok && r > 0 {
				maxRange = r
			}
		}
		rangeCancel()
	}

	// stopCycle ends the module: stop FX + effect cleanup. Shared by every
	// exit path so the client always sees a consistent shutdown.
	stopCycle := func(reason string) {
		log.Printf("[%s] mining stop: modItemID=%d target=%d (%s)", addr, modItemID, targetID, reason)
		// The stop must carry the start's targetID: fxSequencer's Trigger.__eq__ keys
		// on (shipID, moduleID, targetID, guid), and a stop carrying None finds no
		// trigger and leaves the beam drawing at the old rock.
		sendGodmaShipEffect(c, accountID, modItemID, modTypeID, ds.shipID, charID, targetID, "miningLaser", false, 0, 0)
		sendModuleFX(c, addr, accountID, ds, modItemID, modTypeID, 0, targetID, "miningLaser", false, 0, 0, sqldb)
		if ds.entity != nil {
			ds.entity.ClearActiveEffect(modItemID)
		}
	}

	log.Printf("[%s] mining start: modItemID=%d typeID=%d targetID=%d duration=%v yield=%.1fm³",
		addr, modItemID, modTypeID, targetID, cycleDuration, miningAmountM3)

	// doMiningYield performs one ore pull scaled by scale (1.0 for a full cycle,
	// the elapsed fraction on early deactivation). Returns the stop reason and flag.
	doMiningYield := func(scale float64) (reason string, stop bool) {
		oreTypeID := ds.solarSystem.GetAsteroidTypeID(targetID)
		if oreTypeID == 0 {
			return "asteroid gone", true
		}
		if ds.solarSystem.GetAsteroidQty(targetID) <= 0 {
			return "asteroid gone", true
		}

		// Cycle only yields if the ship is still within module range of the rock.
		if ds.dm != nil {
			if rockPos, ok := ds.solarSystem.GetAsteroidPosition(targetID); ok {
				dx := ds.dm.Pos.X - rockPos.X
				dy := ds.dm.Pos.Y - rockPos.Y
				dz := ds.dm.Pos.Z - rockPos.Z
				if math.Sqrt(dx*dx+dy*dy+dz*dz) > maxRange {
					return "out of range", true
				}
			}
		}

		// Look up ore volume to convert m³ yield into unit count.
		oreType := destiny.MinedOreType(oreTypeID)
		var oreVolume float64
		if sqldb != nil {
			volCtx, volCancel := context.WithTimeout(context.Background(), 5*time.Second)
			vol, volErr := evedb.GetTypeVolume(volCtx, sqldb, oreType)
			volCancel()
			if volErr == nil && vol > 0 {
				oreVolume = vol
			}
		}
		if oreVolume <= 0 {
			oreVolume = 0.1 // fallback to Veldspar volume
		}
		cycleYieldM3 := miningAmountM3 * scale
		if crystal != nil && !crystal.broken {
			cycleYieldM3 *= crystal.yieldMult
		}
		yieldUnits := int64(cycleYieldM3 / oreVolume)
		if yieldUnits <= 0 {
			yieldUnits = 1
		}

		// Check cargo capacity before mining.
		if sqldb != nil && ds.shipTypeID != 0 {
			capCtx, capCancel := context.WithTimeout(context.Background(), 5*time.Second)
			cargoCap, capErr := evedb.GetShipCargoCapacityFitted(capCtx, sqldb, ds.shipTypeID, ds.shipID)
			capCancel()
			if capErr == nil && cargoCap > 0 {
				usedCtx, usedCancel := context.WithTimeout(context.Background(), 5*time.Second)
				usedVol, usedErr := evedb.GetCargoUsedVolume(usedCtx, sqldb, charID, ds.shipID)
				usedCancel()
				if usedErr == nil {
					freeM3 := cargoCap - usedVol
					if freeM3 <= 0 {
						return fmt.Sprintf("cargo full (%.1f/%.1f m³)", usedVol, cargoCap), true
					}
					// Cap yield to available cargo space.
					maxUnits := int64(freeM3 / oreVolume)
					if maxUnits <= 0 {
						maxUnits = 1
					}
					if yieldUnits > maxUnits {
						yieldUnits = maxUnits
						log.Printf("[%s] mining cycle: capping yield to %d units (%.1f m³ free)", addr, yieldUnits, freeM3)
					}
				}
			}
		}

		actual := ds.solarSystem.DecrementAsteroidQty(targetID, yieldUnits)
		if newRadius, shrank := ds.solarSystem.UpdateAsteroidRadius(targetID); shrank {
			broadcastAsteroidRadius(ds.solarSystem, targetID, newRadius)
		}
		if actual <= 0 {
			ds.solarSystem.MarkAsteroidDepleted(targetID)
			removeAsteroidFromClients(ds.solarSystem, targetID)
			return "asteroid depleted", true
		}
		// Note depletion now; the partial yield below is still granted and the
		// loop stops right after instead of burning a ghost cycle on an empty rock.
		depleted := ds.solarSystem.GetAsteroidQty(targetID) <= 0
		if depleted {
			ds.solarSystem.MarkAsteroidDepleted(targetID)
		}

		// Crystal volatility: only a completed cycle may wear the crystal; a cut-short
		// cycle spends no crystal life.
		if scale >= 1.0 && crystal != nil {
			crystal.wear(c, addr, accountID, charID, sqldb, "mining")
		}
		log.Printf("[%s] mining cycle: yielding %d units (%.1f m³, scale=%.2f) of typeID=%d from asteroid %d",
			addr, actual, float64(actual)*oreVolume, scale, oreTypeID, targetID)
		if sqldb != nil {
			// Try to stack onto an existing cargo item of the same type.
			dbCtx, dbCancel := context.WithTimeout(context.Background(), 5*time.Second)
			existID, existQty, findErr := evedb.FindCargoItemByType(dbCtx, sqldb, oreType, charID, ds.shipID)
			dbCancel()
			var itemID int64
			// Change dict tells the client what the OLD value was:
			// {ixQuantity(7): oldQty} for a stack update, {ixLocationID(3): 0}
			// for a brand-new item appearing in the hold.
			var changeDict marshal.PyDict
			if findErr != nil {
				log.Printf("[%s] mining cycle: FindCargoItemByType: %v", addr, findErr)
			}
			if existID != 0 && findErr == nil {
				// Stack: update existing quantity.
				newQty := existQty + int64(actual)
				dbCtx2, dbCancel2 := context.WithTimeout(context.Background(), 5*time.Second)
				if err := evedb.UpdateCargoItemQty(dbCtx2, sqldb, existID, newQty); err != nil {
					log.Printf("[%s] mining cycle: UpdateCargoItemQty: %v", addr, err)
					dbCancel2()
					return "", false
				}
				dbCancel2()
				itemID = existID
				changeDict = marshal.PyDict{Items: []marshal.DictEntry{
					{Key: marshal.PyInt{Value: 7}, Value: marshal.PyInt{Value: uint64(existQty)}},
				}}
				log.Printf("[%s] mining cycle: stacked %d units onto item %d (new qty %d)", addr, actual, existID, newQty)
			} else {
				// No existing stack: create a new cargo item.
				dbCtx3, dbCancel3 := context.WithTimeout(context.Background(), 5*time.Second)
				newID, err := evedb.CreateCargoItem(dbCtx3, sqldb, oreType, charID, ds.shipID, int64(actual))
				dbCancel3()
				if err != nil {
					log.Printf("[%s] mining cycle: CreateCargoItem: %v", addr, err)
					return "", false
				}
				itemID = newID
				changeDict = marshal.PyDict{Items: []marshal.DictEntry{
					{Key: marshal.PyInt{Value: 3}, Value: marshal.PyInt{Value: 0}},
				}}
			}
			// Send OnItemChange so the client updates the cargo view.
			dbCtx4, dbCancel4 := context.WithTimeout(context.Background(), 5*time.Second)
			er, err := evedb.GetEntityRow(dbCtx4, sqldb, itemID)
			dbCancel4()
			if err == nil && er != nil && evedb.WorldEventsWanted() {
				// The ore is in the hold: tell modules (db/worldevents.go).
				evedb.EmitWorldEvent(evedb.OreMinedEvent{
					At: time.Now(), CharID: charID, CorpID: ds.corpID,
					TypeID: er.TypeID, GroupID: er.GroupID, Units: int64(actual),
					Volume: float64(actual) * oreVolume, SolarSystemID: ds.sysID,
				})
			}
			if err == nil && er != nil {
				onItemChangeArgs := marshal.PyTuple{Items: []marshal.PyRep{
					evedb.BuildEntityRow(er),
					changeDict,
				}}
				if notifErr := server.SendNotification(c, accountID, "OnItemChange", "charid", onItemChangeArgs); notifErr != nil {
					log.Printf("[%s] mining cycle: OnItemChange send error (client disconnected?): %v", addr, notifErr)
					return "connection lost", true
				}
			}
		}
		if depleted {
			removeAsteroidFromClients(ds.solarSystem, targetID)
			return "asteroid depleted (partial final cycle)", true
		}
		return "", false
	}

	for cycle := int64(0); ; cycle++ {
		cycleStart := time.Now()
		select {
		case <-ctx.Done():
			log.Printf("[%s] mining cycle cancelled modItemID=%d", addr, modItemID)
			return
		case <-stopCh:
			// Deactivated partway through a cycle: pay out ore for the fraction run. A
			// near-instant stop is not worth a DB round trip for a fractional unit.
			if elapsed := time.Since(cycleStart); ds.solarSystem != nil {
				if frac := elapsed.Seconds() / cycleDuration.Seconds(); frac > 0.02 {
					if frac > 1.0 {
						frac = 1.0
					}
					if reason, _ := doMiningYield(frac); reason != "" {
						log.Printf("[%s] mining partial-cycle stop (%.0f%%): %s", addr, frac*100, reason)
					}
				}
			}
			stopCycle("deactivated by pilot")
			return
		case <-time.After(cycleDuration):
		}
		if ds.solarSystem == nil {
			return
		}
		// The first cycle's capacitor cost was drained at activation; every
		// further cycle pays again or the module shuts down.
		if cycle > 0 && capNeed > 0 {
			if !consumeCapForModule(c, accountID, charID, ds, capNeed) {
				stopCycle("capacitor depleted")
				return
			}
		}

		reason, stop := doMiningYield(1.0)
		if stop {
			stopCycle(reason)
			return
		}
		if repeat == 0 {
			stopCycle("single cycle complete")
			return
		}
	}
}

// scannerCycle runs one cargo/ship scanner cycle: wait out the module
// duration, push the scan result to the scanning client and stop the effect.
// Both client handlers index bp.slimItems[shipID], so the result is sent only
// while the target is still on grid and visible. NPC targets scan empty.
//   - OnCargoScanComplete(shipID, cargoList): cargoList is a list of
//     (typeID, quantity) 2-tuples (shipui.py).
//   - OnShipScanComplete(shipID, capacitorCharge, hardwareList): cap in
//     percent, hardware entries (typeID, quantity) tuples (shipui.py).
func scannerCycle(c *evenet.Conn, addr string, accountID, charID int64, ds *destinyConn,
	modID, modTypeID int64, targetID uint32, effectName string, durationMs int64, sqldb *sql.DB) {
	defer safego.Recover("scannerCycle")

	if durationMs <= 0 {
		durationMs = 5000
	}
	time.Sleep(time.Duration(durationMs) * time.Millisecond)

	stop := func() {
		sendGodmaShipEffect(c, accountID, modID, modTypeID, ds.shipID, charID, targetID, effectName, false, 0, 0)
		sendModuleFX(c, addr, accountID, ds, modID, modTypeID, 0, targetID, effectName, false, 0, 0, sqldb)
		if ds.entity != nil {
			ds.entity.ClearActiveEffect(modID)
		}
	}
	defer stop()

	if ds.solarSystem == nil {
		return
	}
	tgt := ds.solarSystem.GetEntity(targetID)
	if tgt == nil || tgt.IsInWarp() || tgt.IsUntargetable() {
		return
	}

	// The ship behind the ball. A connected player's is on its connection; a
	// module projection (a rolebot) has no connection, and its ball ID is its
	// ship's itemID (SlimItem.BallID == ItemID for tracked entities).
	var tgtCharID, tgtShipID int64
	if tgt.IsPlayer {
		tgtCharID = tgt.Slim.CharID
		if pds, ok := tgt.PlayerState.(*destinyConn); ok && pds != nil {
			tgtShipID = pds.shipID
		}
		if tgtShipID == 0 {
			tgtShipID = tgt.Slim.ItemID
		}
	}

	switch effectName {
	case "cargoScan":
		var items []marshal.PyRep
		if tgtCharID != 0 && tgtShipID != 0 && sqldb != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			// Every row in the hold, whoever owns it: a corporation-issued
			// kit's cargo is corp-owned and the owner filter would hide it.
			cargo, err := evedb.GetShipCargoByShip(ctx, sqldb, tgtShipID)
			cancel()
			if err != nil {
				log.Printf("[%s] cargoScan: GetShipCargoByShip ship=%d: %v", addr, tgtShipID, err)
			}
			for _, it := range cargo {
				items = append(items, marshal.PyTuple{Items: []marshal.PyRep{
					marshal.PyInt{Value: uint64(it.TypeID)},
					marshal.PyInt{Value: uint64(it.Quantity)},
				}})
			}
		}
		log.Printf("[%s] → OnCargoScanComplete target=%d items=%d", addr, targetID, len(items))
		_ = server.SendNotification(c, accountID, "OnCargoScanComplete", "charid",
			marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyInt{Value: uint64(targetID)},
				marshal.PyList{Items: items},
			}})

	case "shipScan":
		capPct := 0.0
		if cur, max := tgt.CurrentCap(); max > 0 {
			capPct = cur / max * 100
		}
		var hardware []marshal.PyRep
		if tgtShipID != 0 && sqldb != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			mods, err := evedb.GetFittedModules(ctx, sqldb, tgtShipID)
			cancel()
			if err != nil {
				log.Printf("[%s] shipScan: GetFittedModules ship=%d: %v", addr, tgtShipID, err)
			}
			for _, m := range mods {
				hardware = append(hardware, marshal.PyTuple{Items: []marshal.PyRep{
					marshal.PyInt{Value: uint64(m.TypeID)},
					marshal.PyInt{Value: 1},
				}})
			}
		}
		log.Printf("[%s] → OnShipScanComplete target=%d cap=%.0f%% hardware=%d", addr, targetID, capPct, len(hardware))
		_ = server.SendNotification(c, accountID, "OnShipScanComplete", "charid",
			marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyInt{Value: uint64(targetID)},
				marshal.PyReal{Value: capPct},
				marshal.PyList{Items: hardware},
			}})
	}
}

// surveyScanLoop runs a survey scanner module: each cycle it pushes
// OnSurveyScanComplete with every non-depleted asteroid in scan range as
// [(ballID, typeID, quantity), ...], which the client's svc.surveyScan renders
// in the Survey Scan window. Push-only: the client never polls.
func surveyScanLoop(ctx context.Context, c *evenet.Conn, addr string, accountID, charID int64, ds *destinyConn, modItemID, modTypeID int64, durationMs int64, scanRange float64, capNeed float64, repeat int64, stopCh <-chan struct{}, sqldb *sql.DB) {
	defer safego.Recover("surveyScanLoop")
	if durationMs <= 0 {
		durationMs = 5000
	}
	if scanRange <= 0 {
		scanRange = 15000
	}
	cycleDuration := time.Duration(durationMs) * time.Millisecond

	stopScan := func(reason string) {
		log.Printf("[%s] survey scan stop: modItemID=%d (%s)", addr, modItemID, reason)
		sendGodmaShipEffect(c, accountID, modItemID, modTypeID, ds.shipID, charID, 0, dogma.EffNameSurveyScan, false, 0, 0)
		sendModuleFX(c, addr, accountID, ds, modItemID, modTypeID, 0, 0, dogma.EffNameSurveyScan, false, 0, 0, sqldb)
		if ds.entity != nil {
			ds.entity.ClearActiveEffect(modItemID)
		}
	}

	for cycle := int64(0); ; cycle++ {
		select {
		case <-ctx.Done():
			return
		case <-stopCh:
			stopScan("deactivated by pilot")
			return
		case <-time.After(cycleDuration):
		}
		if ds.solarSystem == nil || ds.dm == nil {
			return
		}
		if cycle > 0 && capNeed > 0 {
			if !consumeCapForModule(c, accountID, charID, ds, capNeed) {
				stopScan("capacitor depleted")
				return
			}
		}
		entries := ds.solarSystem.AsteroidsInRange(ds.dm.Pos, scanRange)
		items := make([]marshal.PyRep, len(entries))
		for i, en := range entries {
			items[i] = marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyInt{Value: uint64(en.EntityID)},
				marshal.PyInt{Value: uint64(en.TypeID)},
				marshal.PyInt{Value: uint64(en.Qty)},
			}}
		}
		scanArgs := marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyList{Items: items},
		}}
		if err := server.SendNotification(c, accountID, "OnSurveyScanComplete", "charid", scanArgs); err != nil {
			log.Printf("[%s] survey scan send (client disconnected?): %v", addr, err)
			return
		}
		log.Printf("[%s] survey scan: %d asteroids within %.0fm", addr, len(entries), scanRange)
		if repeat == 0 || cycle >= repeat {
			stopScan("scan complete")
			return
		}
	}
}

// resolveLockTarget looks up what a client's AddTarget id points at and
// returns its position. A false second return means nothing lockable is there
// (an id the server has no record of, or a dead entity); callers must answer
// with a UserError, since the client stops its targeting spinner only on one.
func resolveLockTarget(ds *destinyConn, sqldb *sql.DB, tid uint32) (destiny.Vec3, bool) {
	if ds.solarSystem == nil {
		return destiny.Vec3{}, false
	}
	if e := ds.solarSystem.GetEntity(tid); e != nil {
		if e.IsDestroyed() {
			return destiny.Vec3{}, false
		}
		return e.Pos(), true
	}
	if p, ok := ds.solarSystem.GetAsteroidPosition(tid); ok {
		return p, true
	}
	if bd, _, ok := ds.solarSystem.GetLootBall(tid); ok {
		return bd.Pos, true // a can or a corpse
	}
	if p, ok := ds.solarSystem.GetStarbasePosition(tid); ok {
		return p, true
	}
	// Celestial (gate, station, planet): position from map data.
	if sqldb != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		var x, y, z float64
		if err := sqldb.QueryRowContext(ctx,
			`SELECT COALESCE(x,0), COALESCE(y,0), COALESCE(z,0) FROM mapDenormalize WHERE itemID = ?`,
			int64(tid)).Scan(&x, &y, &z); err == nil {
			return destiny.Vec3{X: x, Y: y, Z: z}, true
		}
	}
	return destiny.Vec3{}, false
}

// playerNavTargetLookup is a player ship's Manager.TargetLookup: where the
// ball a Follow (Approach / Keep at Range) or Orbit is chasing currently is.
// A miss makes the server sim coast to a stop while the client flies on, so
// every ball a pilot can approach must be here, loot cans and corpses included.
func playerNavTargetLookup(ss *destiny.SolarSystem) func(uint32) (destiny.Vec3, bool) {
	return func(entityID uint32) (destiny.Vec3, bool) {
		if target := ss.GetEntity(entityID); target != nil {
			return target.Pos(), true
		}
		for i := range ss.CelBalls {
			if ss.CelBalls[i].EntityID == entityID {
				return ss.CelBalls[i].Pos, true
			}
		}
		for i := range ss.AsteroidBalls {
			if ss.AsteroidBalls[i].EntityID == entityID {
				return ss.AsteroidBalls[i].Pos, true
			}
		}
		if bd, _, ok := ss.GetLootBall(entityID); ok {
			return bd.Pos, true // a can or a corpse
		}
		for i := range ss.StarbaseBalls {
			if ss.StarbaseBalls[i].EntityID == entityID {
				return ss.StarbaseBalls[i].Pos, true
			}
		}
		return destiny.Vec3{}, false
	}
}

// playerNavTargetRadius is playerNavTargetLookup's radius twin: the ball radius
// of anything a pilot can Approach or Orbit, 0 when it is not found.
func playerNavTargetRadius(ss *destiny.SolarSystem, entityID uint32) float64 {
	if ss == nil {
		return 0
	}
	if target := ss.GetEntity(entityID); target != nil {
		return target.BallDesc().Radius
	}
	for i := range ss.CelBalls {
		if ss.CelBalls[i].EntityID == entityID {
			return ss.CelBalls[i].Radius
		}
	}
	for i := range ss.AsteroidBalls {
		if ss.AsteroidBalls[i].EntityID == entityID {
			return ss.AsteroidBalls[i].Radius
		}
	}
	if bd, _, ok := ss.GetLootBall(entityID); ok {
		return bd.Radius
	}
	for i := range ss.StarbaseBalls {
		if ss.StarbaseBalls[i].EntityID == entityID {
			return ss.StarbaseBalls[i].Radius
		}
	}
	return 0
}

// navCentreRange converts a FollowBall/Orbit range off the wire into what the
// server sim holds: the client's range is surface to surface, Manager.Follow
// and Manager.Orbit hold a centre distance (dockBurnIn converts the same way).
// See docs/main-s12-notes.md#nav-centre-range.
func navCentreRange(ds *destinyConn, targetID uint32, surface float64) float64 {
	r := 0.0
	if ds.dm != nil {
		r = ds.dm.Radius()
	}
	return surface + r + playerNavTargetRadius(ds.solarSystem, targetID)
}

// computeLockTime calculates the target lock duration from the ship's scan
// resolution and the target's signature radius, using the EVE formula.
func computeLockTime(scanResolution, sigRadius float64) time.Duration {
	asinh := math.Log(sigRadius + math.Sqrt(sigRadius*sigRadius+1))
	if asinh <= 0 {
		asinh = 1
	}
	lockTimeMs := 40_000_000.0 / scanResolution / (asinh * asinh)
	// Clamp between 1s and 180s.
	if lockTimeMs < 1000 {
		lockTimeMs = 1000
	}
	if lockTimeMs > 180_000 {
		lockTimeMs = 180_000
	}
	return time.Duration(lockTimeMs) * time.Millisecond
}

// ── Weapon damage cycle ──────────────────────────────────────────────────────

func weaponCycleLoop(ctx context.Context, c *evenet.Conn, addr string, accountID int64, charID int64, ds *destinyConn, modItemID, modTypeID, modSlotFlag int64, targetID uint32, effectName string, repeat int64, durationMs int64, dmgMult float64, optimalRange, falloff, trackingSpeed float64, hullPerType [4]float64, hullCapNeedMult float64, sqldb *sql.DB, npcMgr *npc.NPCManager) {
	// Runs as its own goroutine; a panic here would take the whole server down.
	defer func() {
		if r := recover(); r != nil {
			stack := debug.Stack()
			log.Printf("[%s] PANIC in weapon cycle loop: %v\n%s", addr, r, stack)
			bugtrack.ReportPanic("weaponCycleLoop", r, stack)
		}
	}()
	if durationMs <= 0 {
		durationMs = 5000 // 5s default cycle
	}
	cycleDuration := time.Duration(durationMs) * time.Millisecond

	isMissile := effectName == "missileLaunching" || effectName == "useMissiles"

	// Fetch weapon + charge damage profile once at start.
	var dmgProfile *evedb.WeaponDamageProfile
	var chargeTypeID int64
	// weaponCrystal is set for a loaded frequency crystal, which wears instead of being used up.
	var weaponCrystal *miningCrystalState
	if sqldb != nil && modTypeID != 0 {
		profCtx, profCancel := context.WithTimeout(context.Background(), 5*time.Second)
		chargeTypeID, _ = evedb.GetChargeTypeID(profCtx, sqldb, ds.shipID, modSlotFlag)
		if chargeTypeID != 0 {
			if g, _, gErr := evedb.GetTypeGroupCategory(profCtx, sqldb, chargeTypeID); gErr == nil && isFrequencyCrystalGroup(g) {
				if crysItemID, crysTypeID, cErr := evedb.GetChargeItem(profCtx, sqldb, ds.shipID, modSlotFlag); cErr == nil && crysItemID != 0 {
					if crysAttrs, aErr := evedb.GetTypeAttributes(profCtx, sqldb, crysTypeID); aErr == nil {
						weaponCrystal = newCrystalState(profCtx, sqldb, crysItemID, crysTypeID, crysAttrs)
					}
				}
			}
		}
		profCancel()

		log.Printf("[%s] weapon cycle init: modItemID=%d modTypeID=%d shipID=%d slotFlag=%d chargeTypeID=%d",
			addr, modItemID, modTypeID, ds.shipID, modSlotFlag, chargeTypeID)
		profCtx2, profCancel2 := context.WithTimeout(context.Background(), 5*time.Second)
		p, err := evedb.GetWeaponDamageProfile(profCtx2, sqldb, modTypeID, chargeTypeID)
		profCancel2()
		if err != nil {
			log.Printf("[%s] weapon cycle: failed to get damage profile for typeID=%d: %v", addr, modTypeID, err)
			return
		}
		dmgProfile = p
	}
	if dmgProfile == nil || dmgProfile.Total() <= 0 {
		log.Printf("[%s] weapon cycle: no damage for module typeID=%d chargeTypeID=%d, not cycling — sending stop FX", addr, modTypeID, chargeTypeID)
		stopWeaponFX(c, addr, accountID, charID, ds, modItemID, modTypeID, targetID, effectName, sqldb)
		return
	}
	// Fold in skill + damage-mod bonuses computed at activation time.
	if dmgMult > 0 && dmgMult != 1.0 {
		dmgProfile.Multiplier *= dmgMult
	}
	// Per-damage-type hull bonuses (e.g. Kestrel +10% kinetic missile damage
	// per Caldari Frigate level) scale the profile's individual components.
	if hullPerType != ([4]float64{}) && hullPerType != ([4]float64{1, 1, 1, 1}) {
		dmgProfile.Em *= hullPerType[0]
		dmgProfile.Explosive *= hullPerType[1]
		dmgProfile.Kinetic *= hullPerType[2]
		dmgProfile.Thermal *= hullPerType[3]
	}

	// Missile flight params, once per activation. The catalogue velocity and
	// flight time are unbonused; MissileRangeMults folds skill and hull bonuses in
	// here so the range test, the animation and the pursuit sim share one set.
	var missileFlightParams *evedb.MissileFlightParams
	if isMissile && chargeTypeID != 0 && sqldb != nil {
		mfpCtx, mfpCancel := context.WithTimeout(context.Background(), 5*time.Second)
		mfp, err := evedb.GetMissileFlightParams(mfpCtx, sqldb, chargeTypeID)
		if err != nil {
			log.Printf("[%s] weapon cycle: failed to get missile flight params for chargeTypeID=%d: %v", addr, chargeTypeID, err)
		} else {
			var launcherGroupID int64
			if modTypeID != 0 {
				launcherGroupID, _, _ = evedb.GetTypeGroupCategory(mfpCtx, sqldb, modTypeID)
			}
			velMult, ftMult := evedb.MissileRangeMults(mfpCtx, sqldb, charID, ds.shipTypeID, launcherGroupID)
			if velMult != 1.0 || ftMult != 1.0 {
				// Scale a copy: a pilot's bonuses written into a shared charge row would
				// raise the range of every launcher in the process.
				scaled := *mfp
				scaled.Velocity *= velMult
				scaled.MaxFlightTime *= ftMult
				mfp = &scaled
				if combatDebug {
					log.Printf("[%s] missile range bonuses: charge=%d vel×%.3f fuse×%.3f — %.0fm/s for %.2fs = %.0fm",
						addr, chargeTypeID, velMult, ftMult, mfp.Velocity, mfp.MaxFlightTime, mfp.Velocity*mfp.MaxFlightTime)
				}
			}
			missileFlightParams = mfp
		}
		mfpCancel()
	}

	// Turrets only: the target's signature radius and the weapon's own signature
	// resolution feed the tracking term.
	var targetSigRadius float64 = 125.0 // default subcap
	var weaponSigRadius float64 = 400.0 // default turret sig resolution
	if !isMissile && sqldb != nil {
		// Target signature from target's typeID.
		target := ds.solarSystem.GetEntity(targetID)
		if target != nil {
			sigCtx, sigCancel := context.WithTimeout(context.Background(), 3*time.Second)
			tgtAttrs, sigErr := evedb.GetTypeAttributes(sigCtx, sqldb, int64(target.Slim.TypeID))
			sigCancel()
			if sigErr == nil {
				if s, ok := tgtAttrs[dogma.AttrSignatureRadius]; ok && s > 0 {
					targetSigRadius = s
				}
			}
		}
		// Turrets carry attr 620 (optimalSigRadius), not 552; 552 is the fallback for
		// a module that only carries signatureRadius.
		if modTypeID != 0 {
			wpnCtx, wpnCancel := context.WithTimeout(context.Background(), 3*time.Second)
			wpnAttrs, wpnErr := evedb.GetTypeAttributes(wpnCtx, sqldb, modTypeID)
			wpnCancel()
			if wpnErr == nil {
				if s, ok := wpnAttrs[dogma.AttrOptimalSigRadius]; ok && s > 0 {
					weaponSigRadius = s
				} else if s, ok := wpnAttrs[dogma.AttrSignatureRadius]; ok && s > 0 {
					weaponSigRadius = s
				}
			}
		}
	}

	// Capacitor cost per cycle (attr 6). The first cycle was charged at
	// activation; each later cycle deducts before firing.
	capNeed := 0.0
	if sqldb != nil && modTypeID != 0 {
		capCtx, capCancel := context.WithTimeout(context.Background(), 3*time.Second)
		if wpnAttrs, err := evedb.GetTypeAttributes(capCtx, sqldb, modTypeID); err == nil {
			capNeed = wpnAttrs[dogma.AttrCapacitorNeed]
		}
		capCancel()
	}
	// Hull cap-need bonus (e.g. Punisher -10% small energy turret cap use
	// per Amarr Frigate level).
	if hullCapNeedMult > 0 && hullCapNeedMult != 1.0 {
		capNeed *= hullCapNeedMult
	}

	// Tracking state: previous positions and timestamp for angular velocity. Both
	// ship and target positions are kept so the transverse term uses the relative
	// velocity (target minus attacker), as the EVE tracking formula does.
	var lastTargetPos destiny.Vec3
	var lastShipPos destiny.Vec3
	var lastTargetTime time.Time

	log.Printf("[%s] weapon start: modItemID=%d typeID=%d targetID=%d duration=%v dmg=%.1f (em=%.1f expl=%.1f kin=%.1f therm=%.1f mult=%.2f) missile=%v optimal=%.0f falloff=%.0f tracking=%.4f",
		addr, modItemID, modTypeID, targetID, cycleDuration, dmgProfile.Total(),
		dmgProfile.Em, dmgProfile.Explosive, dmgProfile.Kinetic, dmgProfile.Thermal, dmgProfile.Multiplier, isMissile,
		optimalRange, falloff, trackingSpeed)

	// Per-slot launch stagger, missiles only: launchers activated in one click
	// would otherwise fire cycle 0 in lockstep and their balls would stack exactly.
	if isMissile {
		if d := missileSlotStagger(missileSlotIndex(modSlotFlag, modItemID)); d > 0 {
			select {
			case <-ctx.Done():
				log.Printf("[%s] weapon cycle cancelled modItemID=%d", addr, modItemID)
				return
			case <-time.After(d):
			}
		}
	}

	for cycle := int64(0); ; cycle++ {
		// A weapon fires at the start of its cycle; the activation path charged the
		// capacitor for this first shot. See docs/main-s12-notes.md#first-shot-timing.
		if cycle > 0 {
			select {
			case <-ctx.Done():
				log.Printf("[%s] weapon cycle cancelled modItemID=%d", addr, modItemID)
				return
			case <-time.After(cycleDuration):
			}
		} else if ctx.Err() != nil {
			// Deactivated between the activation handler and this goroutine.
			log.Printf("[%s] weapon cycle cancelled modItemID=%d", addr, modItemID)
			return
		}
		if ds.solarSystem == nil {
			return
		}

		// Capacitor drain for this cycle; cycle 0 was charged at activation.
		if cycle > 0 && !consumeCapForModule(c, accountID, charID, ds, capNeed) {
			log.Printf("[%s] weapon cycle: cap out, stopping modItemID=%d", addr, modItemID)
			stopWeaponFX(c, addr, accountID, charID, ds, modItemID, modTypeID, targetID, effectName, sqldb)
			return
		}

		// Stop firing if the target is no longer locked (manually unlocked
		// or broken by an ECM jam).
		ds.targetMu.Lock()
		stillLocked := false
		for _, t := range ds.targets {
			if t == targetID {
				stillLocked = true
				break
			}
		}
		ds.targetMu.Unlock()
		if !stillLocked {
			log.Printf("[%s] weapon cycle: target %d no longer locked, stopping", addr, targetID)
			stopWeaponFX(c, addr, accountID, charID, ds, modItemID, modTypeID, targetID, effectName, sqldb)
			return
		}

		target := ds.solarSystem.GetEntity(targetID)
		if target == nil {
			log.Printf("[%s] weapon cycle: target %d gone, stopping", addr, targetID)
			stopWeaponFX(c, addr, accountID, charID, ds, modItemID, modTypeID, targetID, effectName, sqldb)
			// Backstop for a removal path that skipped RemoveEntity: a lock left behind
			// answers the next AddTarget with "already targeting".
			if dropLockOn(ds, targetID) {
				sendTargetLost(c, accountID, targetID)
			}
			return
		}

		if !target.HasHP() {
			log.Printf("[%s] weapon cycle: target %d has no HP tracking, skipping", addr, targetID)
			continue
		}

		// --- Consume 1 unit of loaded ammo at launch; a crystal wears instead ---
		ammoOut := false
		if weaponCrystal != nil {
			ammoOut = weaponCrystal.wear(c, addr, accountID, charID, sqldb, "weapon")
		} else {
			_, ammoOut = consumeOneCharge(c, addr, accountID, charID, ds, modSlotFlag, sqldb)
		}

		if isMissile {
			// ── Missile path: spawn missile ball, wait for flight, then apply damage ──
			weaponApplyMissile(c, addr, accountID, charID, ds, modItemID, modTypeID, modSlotFlag, targetID, chargeTypeID, dmgProfile, missileFlightParams, sqldb, npcMgr)
		} else {
			// ── Turret path: tracking-based hit/miss + falloff damage ──
			// Combat-log messages name the turret module, not the loaded charge, as the
			// official client does; it formats %(weapon)s via str(), so the resolved type
			// name goes as a PyString. Missiles name the charge (weaponApplyMissile).
			weaponName := marshal.PyRep(marshal.PyInt{Value: uint64(modTypeID)})
			if sqldb != nil {
				wnCtx, wnCancel := context.WithTimeout(context.Background(), 3*time.Second)
				if name, err := evedb.GetTypeName(wnCtx, sqldb, modTypeID); err == nil && name != "" {
					weaponName = marshal.PyString{Value: name}
				}
				wnCancel()
			}

			// Distance from ship to target.
			shipPos := ds.dm.Pos
			targetPos := target.Pos()
			dx := shipPos.X - targetPos.X
			dy := shipPos.Y - targetPos.Y
			dz := shipPos.Z - targetPos.Z
			dist := math.Sqrt(dx*dx + dy*dy + dz*dz)

			// EWAR debuffs re-read every cycle: a tracking disruptor on the attacker cuts
			// turret optimal and tracking; a target painter inflates the target's signature.
			effOptimal, effTracking, effTargetSig := optimalRange, trackingSpeed, targetSigRadius
			if ds.entity != nil {
				ew := ds.entity.EwarTotals()
				effOptimal *= ew.TurretRangeMult
				effTracking *= ew.TurretTrackingMult
			}
			effTargetSig *= target.SigRadiusMult()

			// Combined hit chance: tracking and falloff terms share one exponent (the
			// client's GetAccuracy contract):
			// P = 0.5^[(w/(T*sigT/sigW))^2 + (max(0, d-optimal)/falloff)^2]
			exponent := 0.0
			var dbgAngular, dbgTransverse float64 // combatDebug only
			if effOptimal > 0 && dist > effOptimal && falloff > 0 {
				fterm := (dist - effOptimal) / falloff
				exponent += fterm * fterm
			}
			if effTracking > 0 {
				now := time.Now()
				if !lastTargetTime.IsZero() {
					dt := now.Sub(lastTargetTime).Seconds()
					rMag := dist
					if rMag > 0 && dt > 0 {
						// Relative velocity (target minus ship) from position deltas, so matching
						// the target's orbit lowers angular velocity, as in the EVE formula.
						vx := (targetPos.X - lastTargetPos.X - (shipPos.X - lastShipPos.X)) / dt
						vy := (targetPos.Y - lastTargetPos.Y - (shipPos.Y - lastShipPos.Y)) / dt
						vz := (targetPos.Z - lastTargetPos.Z - (shipPos.Z - lastShipPos.Z)) / dt
						// Radial direction (unit vector from ship to target).
						rdx, rdy, rdz := dx/rMag, dy/rMag, dz/rMag
						// |r_hat x v_rel| = |v_rel| sin(theta) = relative transverse speed, since
						// r_hat is a unit vector; angular velocity w = transverse_speed / r.
						cx := rdy*vz - rdz*vy
						cy := rdz*vx - rdx*vz
						cz := rdx*vy - rdy*vx
						transverseSpeed := math.Sqrt(cx*cx + cy*cy + cz*cz)
						angular := transverseSpeed / rMag
						dbgAngular, dbgTransverse = angular, transverseSpeed
						tterm := angular / (effTracking * effTargetSig / weaponSigRadius)
						exponent += tterm * tterm
					}
				}
				lastTargetPos = targetPos
				lastShipPos = shipPos
				lastTargetTime = now
			}
			hitChance := math.Pow(0.5, exponent)

			// One roll decides miss/hit/wrecking and the damage modifier: roll <= chance
			// hits at roll+0.49, so low-chance shots land weak; under 1% of chance is 3x.
			roll := rand.Float64()
			dmgMod := 0.0
			switch {
			case roll < 0.01*hitChance:
				dmgMod = 3.0
			case roll <= hitChance:
				dmgMod = roll + 0.49
			}

			if dmgMod == 0 {
				// Miss: pick a miss-tier message (IDs 0-2) by how close it was.
				dmgID := hitQualityID(hitChance * 0.49)
				if dmgID > 2 {
					dmgID = 2
				}
				if combatDebug {
					log.Printf("[%s] weapon cycle: MISS target %d (hitChance=%.3f dist=%.0f %s)", addr, targetID, hitChance, dist,
						turretGeometryDebug(ds, target, dbgAngular, dbgTransverse, effTracking))
				}
				sendCombatMessage(c, accountID, attackMsgGiven[dmgID], marshal.PyDict{Items: []marshal.DictEntry{
					{Key: marshal.PyString{Value: "weapon"}, Value: weaponName},
					{Key: marshal.PyString{Value: "target"}, Value: marshal.PyInt{Value: uint64(targetID)}},
					{Key: marshal.PyString{Value: "damage"}, Value: marshal.PyReal{Value: 0}},
				}})
			} else {
				// Hit: damage scaled by the roll-derived modifier.
				applied, destroyed := target.ApplyDamage(
					dmgProfile.Em*dmgProfile.Multiplier*dmgMod*rates.PlayerDamage,
					dmgProfile.Explosive*dmgProfile.Multiplier*dmgMod*rates.PlayerDamage,
					dmgProfile.Kinetic*dmgProfile.Multiplier*dmgMod*rates.PlayerDamage,
					dmgProfile.Thermal*dmgProfile.Multiplier*dmgMod*rates.PlayerDamage,
				)
				dmgID := hitQualityID(dmgMod)
				if combatDebug {
					log.Printf("[%s] weapon cycle: HIT target %d dmg=%.1f (hitChance=%.3f dmgMod=%.2f dist=%.0f destroyed=%v %s)",
						addr, targetID, applied, hitChance, dmgMod, dist, destroyed,
						turretGeometryDebug(ds, target, dbgAngular, dbgTransverse, effTracking))
				}

				sendCombatMessage(c, accountID, attackMsgGiven[dmgID], marshal.PyDict{Items: []marshal.DictEntry{
					{Key: marshal.PyString{Value: "weapon"}, Value: weaponName},
					{Key: marshal.PyString{Value: "target"}, Value: marshal.PyInt{Value: uint64(targetID)}},
					{Key: marshal.PyString{Value: "damage"}, Value: marshal.PyReal{Value: applied}},
				}})

				// Notify NPC manager if target is an NPC (feeds threat table).
				if ds.solarSystem != nil && ds.solarSystem.IsStationEntity(targetID) {
					// A conquerable station: fair game, no threat, standings or crime (conquest.go).
					noteStationHit(ds.solarSystem, target, ds.corpID, charID)
				} else if ds.solarSystem != nil && ds.solarSystem.IsStructureEntity(targetID) {
					// Player-owned structure: kill credit, the guns'
					// aggression memory, CONCORD, reinforcement.
					structureDamaged(ds.solarSystem, target, ds.entity, modTypeID, applied, destroyed, sqldb)
				} else if !target.IsPlayer && ds.entity != nil {
					npcMgr.NotifyDamage(targetID, ds.entity.ID, applied)
					npcMgr.SyncHPToDB(targetID)
					if objID := npcMgr.GetDunObjectID(targetID); objID > 0 {
						if ds.dunObjOwns(targetID) {
							fireDungeonTriggers(c, addr, accountID, ds, "npcDamaged", objID)
						}
					}
				} else if target.IsPlayer && ds.entity != nil {
					target.RecordKillHit(ds.entity, modTypeID, applied, destroyed)
					recordPlayerAggression(ds.entity, target, ds.solarSystem)
					maybeTriggerConcordResponse(ds.entity, target, ds.solarSystem, sqldb, npcMgr)
				}

				dmgState := target.DamageStateFrac()
				dmgState.Timestamp = server.Win32TimeNow()
				broadcastDamageState(ds, targetID, dmgState)

				// A conquerable station changes hands when its shield reaches 0 (conquerStation).
				if ds.solarSystem != nil && ds.solarSystem.IsStationEntity(targetID) && stationShieldDown(target) && ds.corpID != 0 {
					stopWeaponFX(c, addr, accountID, charID, ds, modItemID, modTypeID, targetID, effectName, sqldb)
					log.Printf("[%s] weapon cycle: station %d CONQUERED by corp %d", addr, targetID, ds.corpID)
					conquerStation(ds.solarSystem, target, ds.corpID, charID, sqldb)
					return
				}

				if destroyed {
					stopWeaponFX(c, addr, accountID, charID, ds, modItemID, modTypeID, targetID, effectName, sqldb)
					log.Printf("[%s] weapon cycle: target %d DESTROYED", addr, targetID)
					if target.IsPlayer && target.Conn != nil && target.PlayerState != nil {
						queuePlayerDeath(target.PlayerState.(*destinyConn), target.Slim.TypeID == 670)
					} else {
						// Route NPC kills through the manager so the death
						// path runs (bounty, mission progress, removal) with
						// this player credited as killer.
						var killerID uint32
						if ds.entity != nil {
							killerID = ds.entity.ID
						}
						if !npcMgr.Kill(targetID, killerID) {
							handleEntityDestroyed(ds, target)
						}
					}
					return
				}
			}
		}

		if ammoOut {
			stopWeaponFX(c, addr, accountID, charID, ds, modItemID, modTypeID, targetID, effectName, sqldb)
			return
		}

		if repeat > 0 {
			repeat--
			if repeat == 0 {
				stopWeaponFX(c, addr, accountID, charID, ds, modItemID, modTypeID, targetID, effectName, sqldb)
				return
			}
		}
	}
}

// Missile flight timing. The client hides the missile ball and draws the whole
// flight from the removal packet: a curve from wherever the ball is into the
// target, at the ball's maxVelocity, detonating within its 2 s clamp. These
// constants and missileTiming keep the ball short of the target at removal.
// See docs/missile-timing.md.
const (
	missileVisualLead    = 1 * time.Second
	missileVisibleFlight = 2 * time.Second
	missileMinBallSpeed  = 50.0 // a charge with no velocity attribute; the client divides by it
)

// missileTiming derives the missile ball's speed and the delay before its
// removal goes out, from the launch distance and the charge's real velocity.
// ballFlight is the time both the damage wait and the client's curve run on.
// ballSpeed governs the ball only; see docs/missile-timing.md#ball-speed.
func missileTiming(dist, velocity, maxFlightTime float64) (ballSpeed, ballFlight float64, removeAfter time.Duration, fizzle bool) {
	lead := missileVisualLead.Seconds()
	visible := missileVisibleFlight.Seconds()
	ballSpeed = velocity
	if ballSpeed <= 0 {
		// A charge with no velocity attribute would give the client a zero to
		// divide by; give it something harmless to fly at instead.
		ballSpeed = missileMinBallSpeed
	}
	if maxFlightTime > 0 && dist > ballSpeed*maxFlightTime {
		// Out of fuel: the client's detonation clamp is fed early enough to fire in
		// empty space at the charge's range. See docs/missile-timing.md#out-of-range.
		fuelRange := ballSpeed * maxFlightTime
		after := maxFlightTime - visible
		if after < lead {
			// A fuel burn shorter than lead plus the visible run cannot be flown at its
			// own speed and still die short; slow the ball, the detonation point holds.
			after = lead
		}
		ballSpeed = fuelRange / (after + visible)
		return ballSpeed, dist / ballSpeed, time.Duration(after * float64(time.Second)), true
	}
	ballFlight = dist / ballSpeed
	// A shot that would arrive before the removal is applied is flown slower.
	// Only ever slower: a faster ball could reach a target it should fall short of.
	if minFlight := 2 * lead; ballFlight < minFlight {
		ballFlight = minFlight
		// dist == 0 means the target is inside the launcher; there is no
		// distance to spread over, so the charge's own speed stands.
		if dist > 0 {
			ballSpeed = dist / ballFlight
		}
	}
	after := ballFlight - visible
	if after < lead {
		after = lead
	}
	return ballSpeed, ballFlight, time.Duration(after * float64(time.Second)), false
}

// missileDamageID maps a missile damage factor (0..1] to a hit-quality
// damageID: full damage reads as a solid hit, poorly-applied damage as a
// glancing one.
func missileDamageID(factor float64) int {
	return hitQualityID(0.5 + factor*0.5)
}

// weaponApplyMissile spawns a missile ball entity in space, waits for flight
// time, then applies damage on impact and removes the missile ball.
func weaponApplyMissile(c *evenet.Conn, addr string, accountID int64, charID int64, ds *destinyConn, modItemID, modTypeID, modSlotFlag int64, targetID uint32, chargeTypeID int64, dmgProfile *evedb.WeaponDamageProfile, mfp *evedb.MissileFlightParams, sqldb *sql.DB, npcMgr *npc.NPCManager) {
	if ds.solarSystem == nil || ds.dm == nil {
		return
	}

	target := ds.solarSystem.GetEntity(targetID)
	if target == nil {
		return
	}

	// Read ds.dm.Pos directly: the system tick loop keeps it current through
	// Manager.Step, and a Step here would advance the position twice.
	ds.syncStamp()
	shipPos := ds.dm.Pos
	targetPos := target.Pos()
	if targetPos.IsZero() {
		// Fallback: check celestial positions (stations).
		for _, cb := range ds.solarSystem.CelBalls {
			if cb.EntityID == targetID {
				targetPos = cb.Pos
				break
			}
		}
	}

	// Per-slot hardpoint: each launcher spawns its ball at its own offset, so a
	// volley's flights start from distinct points. Every downstream number
	// (distance, timing, velocity, the damage chase) runs from spawnPos.
	var shipRadius float64
	if ds.entity != nil {
		shipRadius = ds.entity.BallDesc().Radius
	}
	slotIndex := missileSlotIndex(modSlotFlag, modItemID)
	spawnPos := shipPos.Add(missileHardpointOffset(shipPos, targetPos, shipRadius, slotIndex))
	if combatDebug {
		log.Printf("[%s] missile aim: ship=(%.0f,%.0f,%.0f) target=(%.0f,%.0f,%.0f)",
			addr, shipPos.X, shipPos.Y, shipPos.Z, targetPos.X, targetPos.Y, targetPos.Z)
	}

	velocity := 3000.0    // default 3 km/s
	maxFlightTime := 15.0 // default 15s
	if mfp != nil {
		velocity = mfp.Velocity
		maxFlightTime = mfp.MaxFlightTime
	}
	dx := targetPos.X - spawnPos.X
	dy := targetPos.Y - spawnPos.Y
	dz := targetPos.Z - spawnPos.Z
	dist := math.Sqrt(dx*dx + dy*dy + dz*dz)
	flightTime := dist / velocity
	if flightTime > maxFlightTime {
		flightTime = maxFlightTime
	}
	if flightTime < 0.5 {
		flightTime = 0.5
	}
	// The ball flies at ballSpeed, not the catalogue velocity (missileTiming);
	// ballFlight is the timing both sides run on from here.
	ballSpeed, ballFlight, removeAfter, fizzle := missileTiming(dist, velocity, maxFlightTime)

	missileID := ds.solarSystem.NextMissileID()

	var velX, velY, velZ float64
	if dist > 0 {
		velX = (dx / dist) * ballSpeed
		velY = (dy / dist) * ballSpeed
		velZ = (dz / dist) * ballSpeed
	}

	missileBall := destiny.BallDesc{
		EntityID:      missileID,
		Mode:          destiny.ModeMissile,
		SubType:       0x01, // ship sector (has velocity)
		Pos:           spawnPos,
		Radius:        20,
		Mass:          1.0,
		CorpID:        uint32(ds.corpID),
		MaxSpeed:      ballSpeed,
		Vel:           destiny.Vec3{X: velX, Y: velY, Z: velZ},
		Agility:       1.0,
		SpeedFraction: 1.0,
		FollowID:      targetID,
		FollowRange:   0,
	}
	missileSlim := destiny.SlimItem{
		BallID:     int64(missileID),
		ItemID:     int64(missileID),
		TypeID:     chargeTypeID,
		OwnerID:    charID,
		CorpID:     ds.corpID,
		AllianceID: ds.allianceID,
		CharID:     charID,
	}
	// Set GroupID/CategoryID so the client maps the ball to spaceObject.Missile.
	if sqldb != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		gID, cID, gcErr := evedb.GetTypeGroupCategory(ctx, sqldb, chargeTypeID)
		cancel()
		if gcErr != nil {
			log.Printf("[%s] missile launch: GetTypeGroupCategory(%d) failed: %v — missile ball may not render on client", addr, chargeTypeID, gcErr)
		} else {
			missileSlim.GroupID = gID
			missileSlim.CategoryID = cID
		}
	}
	missileDmg := map[uint32]destiny.DamageState{
		missileID: {Shield: 1.0, Tau: 1e9, Armor: 1.0, Structure: 1.0},
	}

	if combatDebug {
		log.Printf("[%s] missile launch: missileID=%d chargeType=%d → target=%d dist=%.0f flight=%.1fs (nominal %.1fs) ballVel=%.0f remove=+%.1fs",
			addr, missileID, chargeTypeID, targetID, dist, ballFlight, flightTime, ballSpeed, removeAfter.Seconds())
	}

	// AddBalls and the native LaunchMissile go out in one state; the recipient
	// set is reused for the removal. See docs/missile-timing.md#delivery.
	launchPlayers := broadcastMissileLaunch(ds, missileBall, missileSlim, missileDmg, targetID)

	// The removal hands the flight over to the client's own collision curve
	// and runs on its own clock; the damage wait below finishes with it.
	scheduleMissileRemove(launchPlayers, missileID, true, removeAfter)

	if fizzle {
		// Out of range: the client detonates this one in open space at the range ring
		// (missileTiming); no chase, no damage. The miss is reported at the explosion.
		missWeaponType := chargeTypeID
		if missWeaponType == 0 {
			missWeaponType = modTypeID
		}
		if combatDebug {
			log.Printf("[%s] missile out of range: missileID=%d chargeType=%d → target=%d dist=%.0f range=%.0f",
				addr, missileID, chargeTypeID, targetID, dist, velocity*maxFlightTime)
		}
		go func() {
			defer safego.Recover("weaponApplyMissile")
			time.Sleep(removeAfter + missileVisibleFlight)
			sendCombatMessage(c, accountID, attackMsgGiven[0], marshal.PyDict{Items: []marshal.DictEntry{
				{Key: marshal.PyString{Value: "weapon"}, Value: marshal.PyInt{Value: uint64(missWeaponType)}},
				{Key: marshal.PyString{Value: "target"}, Value: marshal.PyInt{Value: uint64(targetID)}},
				{Key: marshal.PyString{Value: "damage"}, Value: marshal.PyReal{Value: 0}},
			}})
		}()
		return
	}

	launchedAt := time.Now()
	go func() {
		defer safego.Recover("weaponApplyMissile")
		if ds.solarSystem == nil {
			return
		}
		// Chased at the charge's real velocity: a pursuit at ballSpeed could not
		// catch a ship faster than the slowed ball. See docs/missile-timing.md#ball-speed.
		tgt := waitForMissileImpact(ds.solarSystem, spawnPos, velocity, maxFlightTime, targetID)
		if tgt == nil {
			// Target gone, warped off, or outran the charge: no damage, and the client's
			// curve bails on its own when the target ball leaves. Report the miss.
			missWeaponType := chargeTypeID
			if missWeaponType == 0 {
				missWeaponType = modTypeID
			}
			sendCombatMessage(c, accountID, attackMsgGiven[0], marshal.PyDict{Items: []marshal.DictEntry{
				{Key: marshal.PyString{Value: "weapon"}, Value: marshal.PyInt{Value: uint64(missWeaponType)}},
				{Key: marshal.PyString{Value: "target"}, Value: marshal.PyInt{Value: uint64(targetID)}},
				{Key: marshal.PyString{Value: "damage"}, Value: marshal.PyReal{Value: 0}},
			}})
			return
		}
		// Hold the hit until the client's missile actually arrives, so the
		// damage message and the explosion land together.
		if wait := time.Duration(ballFlight*float64(time.Second)) - time.Since(launchedAt); wait > 0 {
			time.Sleep(wait)
		}
		if ds.solarSystem == nil || ds.solarSystem.GetEntity(targetID) != tgt {
			// Died or left the grid during the hold.
			return
		}

		if tgt.HasHP() {
			// RMR-era missiles apply full damage: the signature/velocity explosion
			// formula is a later client's.
			applied, destroyed := tgt.ApplyDamage(
				dmgProfile.Em*dmgProfile.Multiplier*rates.PlayerDamage,
				dmgProfile.Explosive*dmgProfile.Multiplier*rates.PlayerDamage,
				dmgProfile.Kinetic*dmgProfile.Multiplier*rates.PlayerDamage,
				dmgProfile.Thermal*dmgProfile.Multiplier*rates.PlayerDamage,
			)
			if combatDebug {
				log.Printf("[%s] missile impact: missileID=%d dealt %.1f dmg to %d (destroyed=%v)", addr, missileID, applied, targetID, destroyed)
			}

			missileWeaponType := chargeTypeID
			if missileWeaponType == 0 {
				missileWeaponType = modTypeID
			}
			sendCombatMessage(c, accountID, attackMsgGiven[missileDamageID(1.0)], marshal.PyDict{Items: []marshal.DictEntry{
				{Key: marshal.PyString{Value: "weapon"}, Value: marshal.PyInt{Value: uint64(missileWeaponType)}},
				{Key: marshal.PyString{Value: "target"}, Value: marshal.PyInt{Value: uint64(targetID)}},
				{Key: marshal.PyString{Value: "damage"}, Value: marshal.PyReal{Value: applied}},
			}})

			// Notify NPC manager if target is an NPC (feeds threat table).
			if ds.solarSystem != nil && ds.solarSystem.IsStationEntity(targetID) {
				// A conquerable station: fair game, no threat, standings or crime (conquest.go).
				noteStationHit(ds.solarSystem, tgt, ds.corpID, charID)
			} else if ds.solarSystem != nil && ds.solarSystem.IsStructureEntity(targetID) {
				// Weapon on the kill report is the launcher, not the charge.
				structureDamaged(ds.solarSystem, tgt, ds.entity, modTypeID, applied, destroyed, sqldb)
			} else if !tgt.IsPlayer && ds.entity != nil {
				npcMgr.NotifyDamage(targetID, ds.entity.ID, applied)
				npcMgr.SyncHPToDB(targetID)
				if objID := npcMgr.GetDunObjectID(targetID); objID > 0 {
					if ds.dunObjOwns(targetID) {
						fireDungeonTriggers(c, addr, accountID, ds, "npcDamaged", objID)
					}
				}
			} else if tgt.IsPlayer && ds.entity != nil {
				// Weapon on the kill report is the launcher, not the charge
				// (matches the archived CONCORD mail format).
				tgt.RecordKillHit(ds.entity, modTypeID, applied, destroyed)
				recordPlayerAggression(ds.entity, tgt, ds.solarSystem)
				maybeTriggerConcordResponse(ds.entity, tgt, ds.solarSystem, sqldb, npcMgr)
			}

			dmgState := tgt.DamageStateFrac()
			dmgState.Timestamp = server.Win32TimeNow()
			broadcastDamageState(ds, targetID, dmgState)

			// A conquerable station changes hands when its shield reaches 0 (conquerStation).
			if ds.solarSystem != nil && ds.solarSystem.IsStationEntity(targetID) && stationShieldDown(tgt) && ds.corpID != 0 {
				log.Printf("[%s] missile impact: station %d CONQUERED by corp %d", addr, targetID, ds.corpID)
				conquerStation(ds.solarSystem, tgt, ds.corpID, charID, sqldb)
				return
			}

			if destroyed {
				log.Printf("[%s] missile impact: target %d DESTROYED", addr, targetID)
				if tgt.IsPlayer && tgt.Conn != nil && tgt.PlayerState != nil {
					queuePlayerDeath(tgt.PlayerState.(*destinyConn), tgt.Slim.TypeID == 670)
				} else {
					// Route NPC kills through the manager so the death path
					// runs (bounty, mission progress, removal) with this
					// player credited as killer.
					var killerID uint32
					if ds.entity != nil {
						killerID = ds.entity.ID
					}
					if !npcMgr.Kill(targetID, killerID) {
						handleEntityDestroyed(ds, tgt)
					}
				}
			}
		}

	}()
}

// broadcastAddBalls sends an AddBalls destiny update to self + all bubble mates.
func broadcastAddBalls(ds *destinyConn, balls []destiny.BallDesc, slims []destiny.SlimItem, dmgStates map[uint32]destiny.DamageState) {
	if ds.solarSystem == nil || ds.entity == nil {
		return
	}
	// Collect all players in the bubble (including self).
	var players []*destiny.Entity
	if ds.entity.Conn != nil {
		players = append(players, ds.entity)
	}
	mates := ds.solarSystem.GetBubbleConnectedPlayersExcept(ds.entity.ID)
	players = append(players, mates...)

	ids := make([]uint32, len(balls))
	for i, b := range balls {
		ids[i] = b.EntityID
	}
	// One queue entry for the whole batch, keyed on the first ball: the AddBalls
	// blob cannot be split per ball (sortBallsByDependency's forward references).
	addKey := "add:" + strconv.FormatUint(uint64(ids[0]), 10)
	for _, p := range players {
		if p.Conn == nil || !p.HasBallparkState() {
			continue
		}
		// Claim, not mark: a recipient that already holds a ball must not get a
		// second AddBalls for it. See docs/destiny-stamps.md#duplicate-delivery.
		newIDs := p.ClaimUnknownBalls(ids...)
		if len(newIDs) == 0 {
			continue
		}
		if len(newIDs) != len(ids) {
			// The batch goes out whole (members may reference each other); a partial
			// claim means two writers are racing for one ball set, so log it.
			log.Printf("broadcastAddBalls: partial claim for player %d — %d of %d balls were already known (%v)",
				p.ID, len(ids)-len(newIDs), len(ids), ids)
		}
		p.QueueAddActionFunc(addKey, ids[0], func(stamp uint32) marshal.PyRep {
			return destiny.BuildAddBallsAction(stamp, balls, slims, dmgStates)
		})
	}
}

// claimUnknownBallsFor narrows a freshly built ball batch to what this player's
// client does not already hold, claiming them in the same step so nothing else
// delivers them concurrently. claimed is false when the entity has no ballpark
// yet; the caller then marks after its send. See Entity.MarkBallsKnown.
func claimUnknownBallsFor(ent *destiny.Entity, balls []destiny.BallDesc, slims []destiny.SlimItem,
	dmg map[uint32]destiny.DamageState) (fb []destiny.BallDesc, fs []destiny.SlimItem,
	fd map[uint32]destiny.DamageState, ids []uint32, claimed bool) {

	ids = make([]uint32, len(balls))
	for i, b := range balls {
		ids[i] = b.EntityID
	}
	if ent == nil || !ent.HasBallparkState() {
		return balls, slims, dmg, ids, false
	}
	newIDs := ent.ClaimUnknownBalls(ids...)
	if len(newIDs) == len(ids) {
		return balls, slims, dmg, ids, true
	}
	if len(newIDs) == 0 {
		return nil, nil, nil, nil, true
	}
	isNew := make(map[uint32]bool, len(newIDs))
	for _, id := range newIDs {
		isNew[id] = true
	}
	fb = make([]destiny.BallDesc, 0, len(newIDs))
	fd = make(map[uint32]destiny.DamageState, len(newIDs))
	for _, b := range balls {
		if !isNew[b.EntityID] {
			continue
		}
		fb = append(fb, b)
		if d, ok := dmg[b.EntityID]; ok {
			fd[b.EntityID] = d
		}
	}
	for _, sl := range slims {
		if isNew[uint32(sl.BallID)] {
			fs = append(fs, sl)
		}
	}
	return fb, fs, fd, newIDs, true
}

// broadcastDunRigidBalls delivers a freshly spawned dungeon room's balls to the
// other players near the room. Proximity is checked against the first ball's
// position, since during a warp-arrival pre-spawn the owner is in no bubble.
// Per-recipient ClaimUnknownBalls dedupes against the regular entity fan-out.
func broadcastDunRigidBalls(ds *destinyConn, balls []destiny.BallDesc, slims []destiny.SlimItem, dmgStates map[uint32]destiny.DamageState) {
	ss := ds.solarSystem
	if ss == nil || len(balls) == 0 {
		return
	}
	anchor := balls[0].Pos
	var selfID uint32
	if ds.entity != nil {
		selfID = ds.entity.ID
	}
	for _, p := range ss.GetAllPlayersInSystem() {
		if p.ID == selfID || p.Conn == nil || p.Conn.IsClosed() || !p.HasBallparkState() {
			continue
		}
		if p.IsInWarp() {
			continue
		}
		if p.Pos().Sub(anchor).LenSq() > destiny.BubbleRadius*destiny.BubbleRadius {
			continue
		}
		ids := make([]uint32, len(balls))
		for i, b := range balls {
			ids[i] = b.EntityID
		}
		newIDs := p.ClaimUnknownBalls(ids...)
		if len(newIDs) == 0 {
			continue
		}
		isNew := make(map[uint32]bool, len(newIDs))
		for _, id := range newIDs {
			isNew[id] = true
		}
		fballs := make([]destiny.BallDesc, 0, len(newIDs))
		fslims := make([]destiny.SlimItem, 0, len(newIDs))
		fdmg := make(map[uint32]destiny.DamageState, len(newIDs))
		for i, b := range balls {
			if !isNew[b.EntityID] {
				continue
			}
			fballs = append(fballs, b)
			fslims = append(fslims, slims[i])
			if d, ok := dmgStates[b.EntityID]; ok {
				fdmg[b.EntityID] = d
			}
		}
		// Queued: these are another pilot's balls. See docs/destiny-stamps.md#budget-and-triage.
		addKey := "add:" + strconv.FormatUint(uint64(newIDs[0]), 10)
		p.QueueAddActionFunc(addKey, newIDs[0], func(stamp uint32) marshal.PyRep {
			return destiny.BuildAddBallsAction(stamp, fballs, fslims, fdmg)
		})
	}
}

// broadcastDunRemoveBalls clears a despawned dungeon room's balls from every
// other player who received them (room reload, scenario delete, owner
// disconnect). Filtered per recipient by KnowsBall, so the full ID set may be
// passed system-wide.
func broadcastDunRemoveBalls(ds *destinyConn, ids []uint32) {
	ss := ds.solarSystem
	if ss == nil || len(ids) == 0 {
		return
	}
	var selfID uint32
	if ds.entity != nil {
		selfID = ds.entity.ID
	}
	for _, p := range ss.GetAllPlayersInSystem() {
		if p.ID == selfID || p.Conn == nil || p.Conn.IsClosed() {
			continue
		}
		var known []uint32
		for _, id := range ids {
			if p.KnowsBall(id) {
				known = append(known, id)
			}
		}
		if len(known) == 0 {
			continue
		}
		p.SendStamped(func(stamp uint32) error {
			return destiny.SendRemoveBalls(p.Conn, globalNodeID, p.AccountID, stamp, known)
		})
		p.ForgetBalls(known...)
	}
}

// broadcastMissileLaunch sends the missile ball's AddBalls plus the native
// LaunchMissile action (destiny.BuildLaunchMissileAction) in one destiny
// state to self and bubble mates, and returns the recipient set, which the
// caller must reuse for the removal (scheduleMissileRemove).
func broadcastMissileLaunch(ds *destinyConn, ball destiny.BallDesc, slim destiny.SlimItem, dmgStates map[uint32]destiny.DamageState, targetID uint32) []*destiny.Entity {
	if ds.solarSystem == nil || ds.entity == nil {
		return nil
	}
	var players []*destiny.Entity
	if ds.entity.Conn != nil {
		players = append(players, ds.entity)
	}
	mates := ds.solarSystem.GetBubbleConnectedPlayersExcept(ds.entity.ID)
	players = append(players, mates...)

	targetEntity := ds.solarSystem.GetEntity(targetID)
	queueMissileLaunch(players, ds.entity, targetEntity, targetID, ball, slim, dmgStates, nil)
	return players
}

// broadcastRemoveBalls sends a RemoveBalls destiny update to self + all bubble mates.
func broadcastRemoveBalls(ds *destinyConn, entityIDs []uint32) {
	if ds.solarSystem == nil || ds.entity == nil {
		return
	}
	var players []*destiny.Entity
	if ds.entity.Conn != nil {
		players = append(players, ds.entity)
	}
	mates := ds.solarSystem.GetBubbleConnectedPlayersExcept(ds.entity.ID)
	players = append(players, mates...)

	for _, p := range players {
		if p.Conn == nil {
			continue
		}
		// Only balls this client holds: removing an unknown ball can desync the
		// ballpark. ClaimKnownBalls filters, forgets and purges queued actions first.
		known := p.ClaimKnownBalls(entityIDs...)
		if len(known) == 0 {
			continue
		}
		p.QueueAction("rm:"+strconv.FormatUint(uint64(known[0]), 10), known[0],
			destiny.BuildRemoveBallsAction(known))
	}
}

// waitForMissileImpact flies a virtual missile server-side: each tick it moves
// toward the target's live position at the charge's velocity, returning the
// target on closing to its ball radius, or nil when the target despawns, warps
// off, or maxFlightTime runs out.
func waitForMissileImpact(ss *destiny.SolarSystem, startPos destiny.Vec3, velocity, maxFlightTime float64, targetID uint32) *destiny.Entity {
	const dt = 0.1
	if velocity <= 0 {
		velocity = 3000
	}
	if maxFlightTime <= 0 {
		maxFlightTime = 15
	}
	pos := startPos
	for elapsed := 0.0; elapsed < maxFlightTime; elapsed += dt {
		time.Sleep(time.Duration(dt * float64(time.Second)))
		tgt := ss.GetEntity(targetID)
		if tgt == nil || tgt.IsInWarp() {
			return nil
		}
		tp := tgt.Pos()
		hitRadius := tgt.BallDesc().Radius
		if tp.IsZero() {
			// Stations and celestials have no destiny manager; their
			// position lives in the celestial ball list.
			for _, cb := range ss.CelBalls {
				if cb.EntityID == targetID {
					tp = cb.Pos
					hitRadius = cb.Radius
					break
				}
			}
			if tp.IsZero() {
				return tgt
			}
		}
		step := velocity * dt
		d := tp.Sub(pos)
		dist := d.Len()
		if dist <= hitRadius+step {
			return tgt
		}
		pos = pos.Add(d.Scale(step / dist))
	}
	return nil
}

// queueMissileRemove queues the TerminalExplosion + RemoveBalls pair for a
// missile ball on each recipient that holds it; shared by the player and NPC
// launch paths. It does not flush. Recipients whose AddBalls is still queued
// are returned for scheduleMissileRemove to retry. See docs/missile-timing.md.
func queueMissileRemove(players []*destiny.Entity, missileID uint32, explode bool) []*destiny.Entity {
	key := strconv.FormatUint(uint64(missileID), 10)
	boomAction := destiny.BuildSimpleAction("TerminalExplosion", marshal.PyInt{Value: uint64(missileID)})
	var waiting []*destiny.Entity
	for _, p := range players {
		if p.Conn == nil || !p.KnowsBall(missileID) {
			continue
		}
		// The add and the removal must not share a packet (docs/missile-timing.md);
		// a recipient whose add is still queued is handed back for a retry.
		if p.HasPendingAdd(missileID) {
			waiting = append(waiting, p)
			continue
		}
		// The explosion is queued ahead of the removal and the ball is retired by the
		// flush, not here. See docs/destiny-stamps.md#queued-actions.
		if explode {
			p.QueueAction("mslBoom:"+key, missileID, boomAction)
		}
		p.QueueRemoveBall("mslRm:"+key, missileID)
	}
	return waiting
}

// NPC turret fire always uses dogma.FXGUIDLaser: tactical.py red-boxes
// (threatAttackingMe) only the guids in its offensive list (Laser,
// MissileDeployment, WarpScramble, ScanStrengthBonusTarget, ModifyTargetSpeed).
// effects.Laser subclasses StandardWeapon; ammo tint comes from otherTypeID.

// npcChargeTypeID maps a turret typeID to a representative charge/ammo typeID
// so the client can determine beam color or projectile model for FX rendering.
func npcChargeTypeID(turretTypeID int64) int64 {
	switch turretTypeID {
	// Energy turrets: Multifrequency (S/M/L)
	case 450: // Gatling Pulse Laser I
		return 246 // Multifrequency S
	case 456: // Focused Medium Pulse Laser I
		return 254 // Multifrequency M
	case 462: // Mega Pulse Laser I
		return 262 // Multifrequency L
	// Projectile turrets: EMP (S/M/L)
	case 484: // 125mm Gatling AutoCannon I
		return 185 // EMP S
	case 490: // 220mm Vulcan AutoCannon I
		return 193 // EMP M
	case 496: // 800mm Repeating Artillery I
		return 201 // EMP L
	// Hybrid turrets: Antimatter (S/M/L)
	case 564: // Light Neutron Blaster I
		return 222 // Antimatter Charge S
	case 568: // Heavy Neutron Blaster I
		return 230 // Antimatter Charge M
	case 575: // Ion Blaster Cannon I
		return 238 // Antimatter Charge L
	default:
		return 246 // Multifrequency S
	}
}

// ── Combat message log helpers ──

// Hit quality label arrays indexed by damageID (0-8).
var attackMsgGiven = [9]string{"AttackMiss1", "AttackMiss2", "AttackMiss3", "AttackHit1", "AttackHit2", "AttackHit3", "AttackHit4", "AttackHit5", "AttackHit6"}
var attackMsgTaken = [9]string{"AttackMiss1R", "AttackMiss2R", "AttackMiss3R", "AttackHit1R", "AttackHit2R", "AttackHit3R", "AttackHit4R", "AttackHit5R", "AttackHit6R"}

// hitQualityID maps a hit quality modifier (0.0-3.0) to a damageID (0-8).
func hitQualityID(modifier float64) int {
	switch {
	case modifier >= 3.0:
		return 8 // wrecking
	case modifier >= 1.2501:
		return 7 // excellent
	case modifier >= 0.9999:
		return 6 // aims well
	case modifier >= 0.7501:
		return 5 // hits
	case modifier >= 0.6251:
		return 4 // lightly hits
	case modifier >= 0.4121:
		return 3 // barely scratches
	case modifier >= 0.3751:
		return 2 // glances off
	case modifier >= 0.2501:
		return 1 // barely misses
	default:
		return 0 // complete miss
	}
}

// sendCombatMessage sends an OnDamageMessage event to a player's combat log
// via OnMultiEvent notification.
func sendCombatMessage(c *evenet.Conn, accountID int64, label string, params marshal.PyDict) {
	event := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyString{Value: "OnDamageMessage"},
		marshal.PyString{Value: label},
		params,
	}}
	args := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyList{Items: []marshal.PyRep{event}},
	}}
	server.SendNotification(c, accountID, "OnMultiEvent", "charid", args)
}

// broadcastSetMaxSpeed pushes a ball's new maximum velocity to every player
// client tracking it (the owner plus bubble mates). SetMaxSpeed is a native
// ballpark op, so each recipient gets its own stamp-synced action.
func broadcastSetMaxSpeed(ds *destinyConn, target *destiny.Entity, newSpeed float64) {
	if ds == nil {
		return
	}
	broadcastSetMaxSpeedIn(ds.solarSystem, target, newSpeed)
}

// broadcastSetMaxSpeedIn is the same push for sources that are not a player
// connection: a webbing drone owns no destinyConn, only a system.
func broadcastSetMaxSpeedIn(ss *destiny.SolarSystem, target *destiny.Entity, newSpeed float64) {
	if ss == nil || target == nil || newSpeed <= 0 {
		return
	}
	var players []*destiny.Entity
	if target.Conn != nil {
		players = append(players, target)
	}
	players = append(players, ss.GetBubbleConnectedPlayersExcept(target.ID)...)
	// Queued, not minted per recipient: SetMaxSpeed is a per-ball absolute value,
	// so last-wins coalescing is exact. It rides the stamped partition.
	spdAction := destiny.BuildSimpleAction("SetMaxSpeed",
		marshal.PyInt{Value: uint64(target.ID)},
		marshal.PyReal{Value: newSpeed})
	for _, p := range players {
		if p.Conn == nil || !p.KnowsBall(target.ID) {
			continue
		}
		p.QueueAction("spd:"+strconv.FormatUint(uint64(target.ID), 10), target.ID, spdAction)
	}
}

// ewarCycleLoop holds a stasis-web or warp-scramble debuff on a target for as
// long as the module runs: applied once, re-validated every cycle (target and
// attacker alive, in range, target not warping). The debuff is removed on any
// exit; module FX stop only when the loop ends itself (Deactivate sends its own).
func ewarCycleLoop(ctx context.Context, c *evenet.Conn, addr string, accountID, charID int64,
	ds *destinyConn, modItemID, modTypeID int64, targetID uint32, effectName string,
	durationMs int64, modAttrs map[int64]float64, sqldb *sql.DB, npcMgr *npc.NPCManager) {
	defer safego.Recover("ewarCycleLoop")

	if ds.solarSystem == nil {
		return
	}
	if durationMs <= 0 {
		durationMs = 5000
	}
	cycleDuration := time.Duration(durationMs) * time.Millisecond
	maxRange := modAttrs[dogma.AttrMaxRange]
	if maxRange <= 0 {
		maxRange = 10000
	}
	// Interdiction Maneuvers in the attacker's gang. The client does not replay
	// this range (unlike the ECM family), so boosting it desyncs nothing.
	if ds.entity != nil {
		maxRange *= ds.entity.GangBoost().Mult(dogma.GSPropJamRange)
	}
	isWeb := effectName == "decreaseTargetSpeed" || effectName == "modifyTargetSpeed"
	isScram := effectName == "warpScramble"

	target := ds.solarSystem.GetEntity(targetID)
	if target == nil {
		stopWeaponFX(c, addr, accountID, charID, ds, modItemID, modTypeID, targetID, effectName, sqldb)
		return
	}
	// Range gate at activation, on the same 20% grace as the cycle re-check:
	// lock range is not module range.
	if ds.entity != nil {
		if d := ds.entity.Pos().Sub(target.Pos()).Len(); d > maxRange*1.2 {
			log.Printf("[%s] ewar: BLOCKED — target %d out of range (%.0f > %.0f)", addr, targetID, d, maxRange*1.2)
			stopWeaponFX(c, addr, accountID, charID, ds, modItemID, modTypeID, targetID, effectName, sqldb)
			return
		}
	}

	// ── apply the debuff ──────────────────────────────────────────────────
	switch {
	case isWeb:
		// speedFactor is a percentage change, e.g. -75 = x0.25.
		mult := 1.0 + modAttrs[dogma.AttrSpeedFactor]/100.0
		newSpeed := target.SetSpeedModifier(modItemID, mult)
		broadcastSetMaxSpeed(ds, target, newSpeed)
		log.Printf("[%s] web ON: mod=%d target=%d ×%.2f → %.0f m/s", addr, modItemID, targetID, mult, newSpeed)
	case isScram:
		strength := modAttrs[dogma.AttrWarpScrambleStrength]
		if strength <= 0 {
			strength = 1
		}
		target.AddWarpScramble(modItemID, strength)
		log.Printf("[%s] scram ON: mod=%d target=%d strength=%.0f", addr, modItemID, targetID, strength)
	default:
		return
	}
	// Activating hostile ewar is the aggression: the trio a gun hit runs
	// (player_ewar.go). See docs/main-s12-notes.md#ewar-aggression.
	noteEwarAggression := func(victim *destiny.Entity) {
		if victim.IsPlayer && ds.entity != nil {
			victim.AddAggressor(ds.entity.ID, time.Now().Unix())
			recordPlayerAggression(ds.entity, victim, ds.solarSystem)
			maybeTriggerConcordResponse(ds.entity, victim, ds.solarSystem, sqldb, npcMgr)
		}
	}
	noteEwarAggression(target)
	defer func() {
		if isWeb {
			if newSpeed, removed := target.RemoveSpeedModifier(modItemID); removed {
				broadcastSetMaxSpeed(ds, target, newSpeed)
				log.Printf("[%s] web OFF: mod=%d target=%d → %.0f m/s", addr, modItemID, targetID, newSpeed)
			}
		}
		if isScram && target.RemoveWarpScramble(modItemID) {
			log.Printf("[%s] scram OFF: mod=%d target=%d", addr, modItemID, targetID)
		}
	}()

	// ── maintain until deactivated or invalid ─────────────────────────────
	capNeed := modAttrs[dogma.AttrCapacitorNeed]
	for {
		select {
		case <-ctx.Done():
			// Deactivate (or re-activation) cancelled us; it sends its own FX stop.
			return
		case <-time.After(cycleDuration):
		}
		if ds.solarSystem == nil {
			return
		}
		// Capacitor drain; the first cycle was charged at activation.
		if !consumeCapForModule(c, accountID, charID, ds, capNeed) {
			log.Printf("[%s] ewar cycle: cap out, stopping modItemID=%d", addr, modItemID)
			stopWeaponFX(c, addr, accountID, charID, ds, modItemID, modTypeID, targetID, effectName, sqldb)
			return
		}
		tgt := ds.solarSystem.GetEntity(targetID)
		if tgt == nil || tgt.IsDestroyed() || tgt.IsInWarp() {
			log.Printf("[%s] ewar cycle: target %d gone/destroyed/warping — dropping effect", addr, targetID)
			stopWeaponFX(c, addr, accountID, charID, ds, modItemID, modTypeID, targetID, effectName, sqldb)
			return
		}
		if ds.entity == nil || ds.solarSystem.GetEntity(ds.entity.ID) == nil {
			return // attacker despawned; entity teardown clears debuffs via defer
		}
		// Range check with a 20% grace so a target drifting on the edge does
		// not flicker the effect on/off.
		diff := ds.entity.Pos().Sub(tgt.Pos())
		if diff.Len() > maxRange*1.2 {
			log.Printf("[%s] ewar cycle: target %d out of range (%.0f > %.0f)", addr, targetID, diff.Len(), maxRange*1.2)
			stopWeaponFX(c, addr, accountID, charID, ds, modItemID, modTypeID, targetID, effectName, sqldb)
			return
		}
		// Every held cycle keeps the victim's personal timer fresh, like a
		// gun that keeps cycling does.
		noteEwarAggression(tgt)
		// The jam bar is a countdown sized to one cycle, so re-raise it each cycle;
		// the client refreshes a bar when the same key comes round again.
		if jt, ok := destiny.JamBucketForFXGUID(moduleFXGUID(effectName)); ok {
			broadcastJamEvent(ds.solarSystem, ds.entity.ID, modItemID, targetID, jt, durationMs, true)
		}
	}
}

// jamPlayerTargets applies an ECM jam to a player victim: cancels pending
// locks, clears locked targets, notifies their client, and blocks re-locking
// for the jam cycle.
func jamPlayerTargets(victim *destiny.Entity, durationMs int64) {
	if victim == nil || victim.Conn == nil || victim.PlayerState == nil {
		return
	}
	vds, ok := victim.PlayerState.(*destinyConn)
	if !ok {
		return
	}
	vds.targetMu.Lock()
	for tid, cancelFn := range vds.lockingTargets {
		cancelFn()
		delete(vds.lockingTargets, tid)
	}
	vds.targets = nil
	vds.jamUntil = time.Now().Add(time.Duration(durationMs) * time.Millisecond)
	vds.targetMu.Unlock()
	_ = server.SendNotification(victim.Conn, int64(victim.AccountID), "OnTarget", "charid",
		marshal.PyTuple{Items: []marshal.PyRep{marshal.PyString{Value: "clear"}}})
}

// electronicJamPairs lists the ECM attribute pairs: attacker bonus attr (module) to
// defender sensor strength attr (ship), in the client's gravimetric, ladar,
// magnetometric, radar order (godma.py).
var electronicJamPairs = [4][2]int64{
	{238, 211}, // scanGravimetricStrengthBonus → scanGravimetricStrength
	{239, 209}, // scanLadarStrengthBonus → scanLadarStrength
	{240, 210}, // scanMagnetometricStrengthBonus → scanMagnetometricStrength
	{241, 208}, // scanRadarStrengthBonus → scanRadarStrength
}

// chanceEffectLoop runs a chance-based effect (ECM, damp, painter, disruptor) the
// way the client's EffectRepeater does: three WichmannHill rolls per cycle in the
// client's order (range, electronic, propulsion) with its formulas, so the seeded
// client replays the same outcomes (DOGMA_MATH.md section 8).
func chanceEffectLoop(ctx context.Context, c *evenet.Conn, addr string, accountID, charID int64,
	ds *destinyConn, modItemID, modTypeID int64, targetID uint32, effectName string,
	durationMs, repeat int64, modAttrs map[int64]float64, ei evedb.EffectInfo,
	rng *dogma.WichmannHill, sqldb *sql.DB, npcMgr *npc.NPCManager) {
	defer safego.Recover("chanceEffectLoop")

	if ds.solarSystem == nil {
		return
	}
	if durationMs <= 0 {
		durationMs = 5000
	}
	cycleDuration := time.Duration(durationMs) * time.Millisecond
	capNeed := modAttrs[dogma.AttrCapacitorNeed]

	// Non-jam debuff carried by this effect (tracking disruptor, sensor damp, target
	// painter). A missing attribute reads 0, which the consumer treats as 1.0.
	var debuff destiny.EwarDebuff
	hasDebuff := true
	switch effectName {
	case dogma.EffNameTrackingDisrupt:
		debuff = destiny.EwarDebuff{
			TurretRangeMult:    modAttrs[dogma.AttrMaxRangeMultiplier],
			TurretTrackingMult: modAttrs[dogma.AttrTrackingSpeedMultiplier],
		}
	case dogma.EffNameSensorDamp:
		debuff = destiny.EwarDebuff{
			MaxTargetRangeMult: modAttrs[dogma.AttrMaxTargetRangeMultiplier],
			ScanResolutionMult: modAttrs[dogma.AttrScanResolutionMultiplier],
		}
	case dogma.EffNameTargetPaint:
		debuff = destiny.EwarDebuff{SigRadiusMult: 1.0 + modAttrs[dogma.AttrSignatureRadiusBonus]/100.0}
	default:
		hasDebuff = false
	}
	defer func() {
		if hasDebuff && ds.solarSystem != nil {
			if tgt := ds.solarSystem.GetEntity(targetID); tgt != nil && tgt.RemoveEwarDebuff(modItemID) {
				log.Printf("[%s] ewar debuff OFF: mod=%d effect=%q target=%d", addr, modItemID, effectName, targetID)
				pushEwarVictimStats(tgt, sqldb)
			}
		}
	}()

	// Target sensor strengths from its type attributes (matches what the
	// client reads via ItemGetInfo).
	tgtAttrs := map[int64]float64{}
	if sqldb != nil {
		if tgt := ds.solarSystem.GetEntity(targetID); tgt != nil {
			taCtx, taCancel := context.WithTimeout(context.Background(), 3*time.Second)
			if a, err := evedb.GetTypeAttributes(taCtx, sqldb, int64(tgt.Slim.TypeID)); err == nil {
				tgtAttrs = a
			}
			taCancel()
		}
	}

	for cycle := int64(0); ; cycle++ {
		tgt := ds.solarSystem.GetEntity(targetID)
		if tgt == nil || tgt.IsDestroyed() || tgt.IsInWarp() {
			log.Printf("[%s] chance cycle: target %d gone/destroyed/warping — stopping", addr, targetID)
			stopWeaponFX(c, addr, accountID, charID, ds, modItemID, modTypeID, targetID, effectName, sqldb)
			return
		}
		// Activating ewar on a pilot is the aggression whether or not the roll lands;
		// ewarCycleLoop does the same for a point or a web.
		if tgt.IsPlayer && ds.entity != nil {
			tgt.AddAggressor(ds.entity.ID, time.Now().Unix())
			recordPlayerAggression(ds.entity, tgt, ds.solarSystem)
			maybeTriggerConcordResponse(ds.entity, tgt, ds.solarSystem, sqldb, npcMgr)
		}

		// Rolls happen at cycle start (client BroadcastStart); all three are always
		// consumed, in order.
		roll1 := rng.Random()
		roll2 := rng.Random()
		roll3 := rng.Random()
		_ = roll3 // propulsion: no chance-based propulsion modules in RMR data

		success := true
		if ei.RangeChance && ei.RangeAttributeID != 0 && ei.FalloffAttributeID != 0 && ds.entity != nil {
			distance := ds.entity.Pos().Sub(tgt.Pos()).Len()
			maxRange := modAttrs[ei.RangeAttributeID]
			falloff := modAttrs[ei.FalloffAttributeID]
			if distance > maxRange && falloff > 0 {
				chance := math.Pow(2, -((distance-maxRange)*(distance-maxRange))/(falloff*falloff))
				success = roll1 <= chance
			}
		}
		if ei.ElectronicChance && success {
			// Strongest target sensor among pairs with attacker bonus >= 1.0 (tie: higher
			// attack), then attack/defense (godma.py:944).
			var maxA, maxD float64
			havePair := false
			for _, pair := range electronicJamPairs {
				a := modAttrs[pair[0]]
				if a < 1.0 {
					continue
				}
				d := tgtAttrs[pair[1]]
				switch {
				case !havePair || d > maxD:
					maxA, maxD, havePair = a, d, true
				case d == maxD && a > maxA:
					maxA = a
				}
			}
			if havePair && maxD > 0 {
				success = roll2 <= maxA/maxD
			}
		}

		if success && ei.ElectronicChance {
			log.Printf("[%s] chance cycle %d: mod=%d JAM SUCCESS on %d for %dms", addr, cycle, modItemID, targetID, durationMs)
			if tgt.IsPlayer {
				jamPlayerTargets(tgt, durationMs)
			} else {
				npcMgr.JamTargets(targetID, time.Now().Add(cycleDuration))
			}
		} else {
			log.Printf("[%s] chance cycle %d: mod=%d effect=%q target=%d success=%v", addr, cycle, modItemID, effectName, targetID, success)
		}
		// The debuff holds for the cycle on success and drops on a miss. Consumers read
		// EwarTotals() live, so it lands at once.
		if hasDebuff {
			if success {
				tgt.SetEwarDebuff(modItemID, debuff)
				log.Printf("[%s] ewar debuff ON: mod=%d effect=%q target=%d %+v", addr, modItemID, effectName, targetID, debuff)
				pushEwarVictimStats(tgt, sqldb)
			} else if tgt.RemoveEwarDebuff(modItemID) {
				pushEwarVictimStats(tgt, sqldb)
			}
		}
		// The victim's indicator tracks the cycle, not the activation: raise or drop it
		// on this cycle's roll.
		if jt, ok := destiny.JamBucketForFXGUID(moduleFXGUID(effectName)); ok && ds.entity != nil {
			broadcastJamEvent(ds.solarSystem, ds.entity.ID, modItemID, targetID, jt, durationMs, success)
		}

		select {
		case <-ctx.Done():
			// Deactivate cancelled us; it sends its own FX stop.
			return
		case <-time.After(cycleDuration):
		}

		// Repeat accounting: >=1000 means repeat-forever (client contract).
		if repeat < 1000 {
			repeat--
			if repeat <= 0 {
				stopWeaponFX(c, addr, accountID, charID, ds, modItemID, modTypeID, targetID, effectName, sqldb)
				return
			}
		}
		// Capacitor for the next cycle (first was charged at activation).
		if !consumeCapForModule(c, accountID, charID, ds, capNeed) {
			log.Printf("[%s] chance cycle: cap out, stopping modItemID=%d", addr, modItemID)
			stopWeaponFX(c, addr, accountID, charID, ds, modItemID, modTypeID, targetID, effectName, sqldb)
			return
		}
	}
}

// stopPropModsForWarp switches every running afterburner and microwarpdrive off
// as the ship enters warp, the same forced stop a cap-out runs: FX and button go
// dark, the speed boost and MWD bloom come off, max velocity is pushed. A lower
// max speed mid-warp only moves the sub-warp exit speed. Connection goroutine.
func stopPropModsForWarp(c *evenet.Conn, addr string, accountID, charID int64, ds *destinyConn, sqldb *sql.DB) {
	for modID, mod := range ds.activeModules {
		if mod == nil || !isPropModEffectName(mod.effectName) {
			continue
		}
		if cancelFn, ok := ds.cancelWeapon[modID]; ok {
			cancelFn()
		}
		stopWeaponFX(c, addr, accountID, charID, ds, modID, mod.typeID, 0, mod.effectName, sqldb)
		if ds.entity != nil {
			if newSpeed, removed := ds.entity.RemoveSpeedModifier(modID); removed {
				broadcastSetMaxSpeed(ds, ds.entity, newSpeed)
				clearMWDBloom(ds.entity)
			}
		}
		log.Printf("[%s] warp: prop mod %d (%s) switched off", addr, modID, mod.effectName)
	}
}

// isPropModEffectName reports whether an effect is an afterburner or
// microwarpdrive: the set bound.Activate applies a speed boost for.
func isPropModEffectName(name string) bool {
	return name == "speedBoost" || name == "speedBoostMassAddition" || name == "speedBoostMassSigRad"
}

// turretGeometryDebug renders the server's view of a turret shot for the
// combatDebug hit/miss lines: angular velocity, transverse speed, tracking, and
// both balls' nav mode and speed, to read against what the pilot saw.
func turretGeometryDebug(ds *destinyConn, target *destiny.Entity, angular, transverse, tracking float64) string {
	own := "-"
	if ds.dm != nil {
		own = fmt.Sprintf("%s@%.0f", destinyModeName(ds.dm.CurrentBallMode()), ds.dm.Vel.Len())
	}
	bd := target.BallDesc()
	return fmt.Sprintf("ω=%.3f trk=%.3f vT=%.0f self=%s tgt=%s@%.0f",
		angular, tracking, transverse, own, destinyModeName(bd.Mode), bd.Vel.Len())
}

func stopWeaponFX(c *evenet.Conn, addr string, accountID int64, charID int64, ds *destinyConn, modItemID, modTypeID int64, targetID uint32, effectName string, sqldb *sql.DB) {
	sendGodmaShipEffect(c, accountID, modItemID, modTypeID, ds.shipID, charID, targetID, effectName, false, 0, 0)
	sendModuleFX(c, addr, accountID, ds, modItemID, modTypeID, 0, targetID, effectName, false, 0, 0, sqldb)
	if ds.entity != nil {
		ds.entity.ClearActiveEffect(modItemID)
	}
	// Clean up active module and cancel func so re-activation after reload works.
	if ds.activeModules != nil {
		delete(ds.activeModules, modItemID)
	}
	if ds.cancelWeapon != nil {
		delete(ds.cancelWeapon, modItemID)
	}
	// A link forced off (cap out, target loss) drops its bonus now.
	if _, isLink := dogma.GangLinkSpecFor(modTypeID); isLink {
		markCharGangDirty(charID)
	}
}

// ── Repair cycle loop ────────────────────────────────────────────────────────

func repairCycleLoop(ctx context.Context, c *evenet.Conn, addr string, accountID int64, charID int64, ds *destinyConn, modItemID, modTypeID int64, effectName string, durationMs int64, repairAmount, capNeed float64) {
	defer safego.Recover("repairCycleLoop")
	if durationMs <= 0 {
		durationMs = 5000
	}
	cycleDuration := time.Duration(durationMs) * time.Millisecond

	log.Printf("[%s] repair start: modItemID=%d typeID=%d effect=%q duration=%v amount=%.1f",
		addr, modItemID, modTypeID, effectName, cycleDuration, repairAmount)

	// Shield boosters heal at the start of each cycle; armor and hull repairers at
	// the end, so an interrupted cycle heals nothing.
	healsAtStart := effectName == "shieldBoosting"

	applyHeal := func(cycle int64) {
		switch effectName {
		case "shieldBoosting":
			ds.entity.RepairShield(repairAmount)
		case "armorRepair":
			ds.entity.RepairArmor(repairAmount)
		case "structureRepair":
			ds.entity.RepairStructure(repairAmount)
		}
		dmgState := ds.entity.DamageStateFrac()
		dmgState.Timestamp = server.Win32TimeNow()
		broadcastDamageState(ds, ds.entity.ID, dmgState)
		log.Printf("[%s] repair cycle %d: mod=%d effect=%q +%.1f HP", addr, cycle, modItemID, effectName, repairAmount)
	}

	for cycle := int64(0); ; cycle++ {
		// Capacitor drain; the first cycle was charged at activation.
		if cycle > 0 && !consumeCapForModule(c, accountID, charID, ds, capNeed) {
			log.Printf("[%s] repair cycle: cap out, stopping modItemID=%d", addr, modItemID)
			stopWeaponFX(c, addr, accountID, charID, ds, modItemID, modTypeID, 0, effectName, nil)
			return
		}
		if ds.entity == nil {
			return
		}
		if healsAtStart {
			applyHeal(cycle)
		}
		select {
		case <-ctx.Done():
			log.Printf("[%s] repair cycle cancelled modItemID=%d", addr, modItemID)
			stopWeaponFX(c, addr, accountID, charID, ds, modItemID, modTypeID, 0, effectName, nil)
			return
		case <-time.After(cycleDuration):
		}
		if ds.entity == nil {
			return
		}
		if !healsAtStart {
			applyHeal(cycle)
		}
	}
}

// broadcastDamageState sends OnDamageStateChange to every player who can see the
// entity: bubble mates, or the whole system for an entity in no bubble.
func broadcastDamageState(ds *destinyConn, entityID uint32, dmgState destiny.DamageState) {
	if ds == nil {
		return
	}
	broadcastDamageStateTo(ds.solarSystem, entityID, dmgState)
}

// broadcastDamageStateTo is broadcastDamageState against a named grid. The grid
// is a parameter because a doomsday through a cynosural field damages ships in a
// system the firing pilot is not in.
func broadcastDamageStateTo(ss *destiny.SolarSystem, entityID uint32, dmgState destiny.DamageState) {
	if ss == nil {
		return
	}
	target := ss.GetEntity(entityID)
	if target == nil {
		return
	}

	var players []*destiny.Entity
	if target.BubbleID < 0 {
		// No bubble (station entity): broadcast system-wide.
		players = ss.GetAllPlayersInSystem()
	} else {
		// Include the target itself if it's a player.
		if target.Conn != nil {
			players = append(players, target)
		}
		// Get bubble mates.
		mates := ss.GetBubbleConnectedPlayersExcept(entityID)
		players = append(players, mates...)
	}

	for _, p := range players {
		if p.Conn == nil || !p.KnowsBall(entityID) {
			continue
		}
		// A warping witness has no ballpark to apply this to. Landing grace and the gate
		// cloak are not reasons to skip: a pilot's own hull state must always arrive.
		if p.IsInWarp() {
			continue
		}
		// Not IsUntargetable: this asks about the witness, whose landing grace means its
		// ballpark is being rebuilt. A GM in /gm mode must keep seeing the fight.
		if p.ID != entityID && (p.InLandingGrace() || p.IsCloaked()) {
			continue
		}
		// Queued, not sent per hit: damage state is absolute, so last-wins coalescing per
		// ball is lossless and the fx partition drains at the 5 Hz tick.
		p.QueueFXAction("dmg:"+strconv.FormatUint(uint64(entityID), 10), entityID,
			destiny.BuildDamageStateChangeAction(entityID, dmgState))
	}

	// Update HUD S/A/S bars if target is a player.
	if target.IsPlayer && target.Conn != nil {
		sendPlayerHPUpdate(target)
	}

	// Also update CelDamage so new arrivals see the correct station HP.
	if ss.IsStationEntity(entityID) {
		ss.SetCelDamage(entityID, dmgState)
	}
	if ss.IsStructureEntity(entityID) {
		structureHPChanged(ss, target, dmgState)
	}
}

// hpPushInterval is the per-victim floor on godma HP pushes. Faster damage
// coalesces into one deferred push whose HP is read at fire time, so the last
// push of a volley carries the final state.
const hpPushInterval = 200 * time.Millisecond

type hpPushState struct {
	mu    sync.Mutex
	last  time.Time
	armed bool
}

// hpPushStates tracks the per-victim aggregator. Keyed by entity ID; the set
// is bounded by the connected-player population, so nothing prunes it.
var hpPushStates syncx.Map[uint32, *hpPushState]

// sendPlayerHPUpdate updates the victim's HUD shield/armor/structure bars.
// The 3D hull scarring is driven by OnDamageStateChange (destiny), but the
// HUD bars are driven by godma attributes. Rate-limited per victim: a volley
// from five rats produces one notification, not fifteen.
func sendPlayerHPUpdate(entity *destiny.Entity) {
	if entity == nil || entity.Conn == nil || !entity.IsPlayer {
		return
	}
	stI, _ := hpPushStates.LoadOrStore(entity.ID, &hpPushState{})
	st := stI
	st.mu.Lock()
	since := time.Since(st.last)
	if since < hpPushInterval {
		if !st.armed {
			st.armed = true
			time.AfterFunc(hpPushInterval-since, func() {
				st.mu.Lock()
				st.armed = false
				st.last = time.Now()
				st.mu.Unlock()
				pushPlayerHP(entity)
			})
		}
		st.mu.Unlock()
		return
	}
	st.last = time.Now()
	st.mu.Unlock()
	pushPlayerHP(entity)
}

// pushPlayerHP sends the three HP attributes (shieldCharge 264, armorDamage 266,
// damage 3) as one OnMultiEvent; godma's handler buckets OnModuleAttributeChange
// natively (see the dogma-event notes in destiny/notify.go).
func pushPlayerHP(entity *destiny.Entity) {
	if entity == nil || entity.Conn == nil || !entity.IsPlayer {
		return
	}
	// No warp, cloak or landing gate: godma attribute notifications carry no stamp
	// and never touch the ballpark, so a repairer under a gate cloak still moves the HUD.
	hp := entity.HPSnapshot()
	if hp.MaxStructure <= 0 {
		return
	}
	shipID := int64(entity.ID)
	charID := entity.Slim.CharID
	accountID := entity.AccountID

	if combatDebug {
		log.Printf("[hp-update] entityID=%d shield=%.1f/%.1f armor=%.1f/%.1f structure=%.1f/%.1f",
			entity.ID, hp.CurShield, hp.MaxShield, hp.CurArmor, hp.MaxArmor, hp.CurStructure, hp.MaxStructure)
	}

	ts := server.Win32TimeNowRep()
	ev := func(attrID int64, newV, oldV float64) marshal.PyRep {
		return marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyString{Value: "OnModuleAttributeChange"},
			marshal.PyInt{Value: uint64(charID)},
			marshal.PyInt{Value: uint64(shipID)},
			marshal.PyInt{Value: uint64(attrID)},
			ts,
			marshal.PyReal{Value: newV},
			marshal.PyReal{Value: oldV},
		}}
	}
	args := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyList{Items: []marshal.PyRep{
			ev(264, hp.CurShield, hp.MaxShield),
			ev(266, hp.MaxArmor-hp.CurArmor, 0),
			ev(3, hp.MaxStructure-hp.CurStructure, 0),
		}},
	}}
	if err := server.SendNotification(entity.Conn, int64(accountID), "OnMultiEvent", "charid", args); err != nil {
		log.Printf("pushPlayerHP: %v", err)
	}
}

// handleEntityDestroyed removes a destroyed entity from the system and notifies
// all players in the bubble.
func handleEntityDestroyed(ds *destinyConn, target *destiny.Entity) {
	if ds.solarSystem == nil {
		return
	}
	// Player-owned structures have their own death (posentity.go).
	if handleStructureDestroyed(ds.solarSystem, target, ds.entity) {
		return
	}

	// Player drones route through the drone manager (item delete + state-row
	// removal + explosion broadcast via its Killed callback).
	if globalDroneMgr != nil {
		var killerID uint32
		if ds.entity != nil {
			killerID = ds.entity.ID
		}
		if globalDroneMgr.HandleDestroyed(target.ID, killerID) {
			return
		}
	}

	entityID := target.ID

	// RemoveEntity returns the remaining players. TerminalExplosion batched with
	// RemoveBalls sets explodeOnRemove on the client ball, which plays the death FX.
	remaining := ds.solarSystem.RemoveEntity(entityID)
	for _, p := range remaining {
		if p.Conn == nil || !p.KnowsBall(entityID) {
			continue
		}
		// Urgent: a kill is the stamp budget's named exemption, one stamp per witness per
		// kill, and enforcement must never drop it.
		p.SendStampedUrgent(func(stamp uint32) error {
			return destiny.SendTerminalRemove(p.Conn, globalNodeID, p.AccountID, stamp, entityID)
		})
		p.ForgetBalls(entityID)
	}

	if target.IsPlayer {
		var killerID uint32
		if ds.entity != nil {
			killerID = ds.entity.ID
		}
		modapi.FireEntityRemoved(ds.solarSystem, entityID, killerID)
	}
	if target.Slim.GroupID == evedb.GroupMine {
		mineRemoved(ds.solarSystem.ID, entityID)
	}
	log.Printf("[destroy] entity %d removed from system %d", entityID, ds.solarSystem.ID)
}

// npcDestroyEntity removes a target an NPC killed, with the explosion. Called from
// the NPC combat broadcast callback.
func npcDestroyEntity(ss *destiny.SolarSystem, target *destiny.Entity, killerID uint32) {
	entityID := target.ID
	if handleStructureDestroyed(ss, target, ss.GetEntity(killerID)) {
		return
	}

	// Before RemoveEntity: the sweep lifts ewar and releases every rat's lock through
	// ss.GetEntity, which answers nil once the entity is gone. No manager lock is held here.
	if globalNPCMgr != nil {
		globalNPCMgr.ForgetTargetInSystem(ss, entityID)
	}

	// RemoveBalls with TerminalExplosion plays the death FX on the client (explodeOnRemove).
	remaining := ss.RemoveEntity(entityID)
	for _, p := range remaining {
		if p.Conn == nil || !p.KnowsBall(entityID) {
			continue
		}
		// Urgent: death class, as in handleEntityDestroyed.
		p.SendStampedUrgent(func(stamp uint32) error {
			return destiny.SendTerminalRemove(p.Conn, globalNodeID, p.AccountID, stamp, entityID)
		})
		p.ForgetBalls(entityID)
	}
	if target.IsPlayer {
		modapi.FireEntityRemoved(ss, entityID, killerID)
	}
	if target.Slim.GroupID == evedb.GroupMine {
		mineRemoved(ss.ID, entityID)
	}
	log.Printf("[npc-combat] target entity %d destroyed by NPC (killer=%d)", entityID, killerID)
}

// ── Player death loop ───────────────────────────────────────────────────────

// clearPlayerCombatState cancels all weapons, mining, warp, and targeting
// on the given destinyConn. Called when a player's ship or pod is destroyed.
func clearPlayerCombatState(ds *destinyConn) {
	// Cancel weapon goroutines.
	for modID, cancelFn := range ds.cancelWeapon {
		cancelFn()
		delete(ds.cancelWeapon, modID)
	}
	// Cancel mining sessions.
	for modID, sess := range ds.miningSessions {
		sess.cancel()
		delete(ds.miningSessions, modID)
	}
	ds.activeModules = nil
	// The ship is gone: answer its held reloads now.
	ds.finishReloadsNow()

	// Cancel warp.
	if ds.cancelWarp != nil {
		ds.cancelWarp()
		ds.cancelWarp = nil
	}
	if ds.entity != nil {
		ds.entity.SetWarpPending(false)
	}

	// Clear targets.
	ds.targetMu.Lock()
	for tid, cancelFn := range ds.lockingTargets {
		cancelFn()
		delete(ds.lockingTargets, tid)
	}
	ds.targets = nil
	ds.targetMu.Unlock()

	// Clear active effects on entity.
	if ds.entity != nil {
		ds.entity.ClearAllActiveEffects()
	}

	// Notify client to clear targeting UI.
	if ds.entity != nil && ds.entity.Conn != nil {
		clearArgs := marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyString{Value: "OnTarget"},
			marshal.PyString{Value: "clear"},
			marshal.PyNone{},
			marshal.PyNone{},
		}}
		multiArgs := marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyList{Items: []marshal.PyRep{clearArgs}},
		}}
		_ = server.SendNotification(ds.entity.Conn, int64(ds.entity.AccountID), "OnMultiEvent", "clientID", multiArgs)
	}
}

// killReport carries everything the kill-mail builder needs, snapshotted on
// the death path before the victim entity and its DB item rows are torn down.
type killReport struct {
	VictimCharID     int64
	VictimName       string
	VictimCorpID     int64
	VictimAllianceID int64
	ShipTypeID       int64
	SysID            int64
	Pos              destiny.Vec3
	Time             time.Time
	Hits             []destiny.KillHit
	DestroyedItems   []evedb.EntityRow
	DroppedItems     []evedb.EntityRow
}

// mailTextEscape escapes a value interpolated into a mail body. The body itself is
// markup (<br>, <b>) and is not escaped; the names inside it are player-chosen and
// the client HTML-parses the whole message.
func mailTextEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// sendKillReportMails builds the CONCORD kill report and mails it to the victim
// and the final-blow pilot. Runs in its own goroutine off the death path; every
// lookup is best-effort.
func sendKillReportMails(sqldb *sql.DB, km killReport) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Every name these three return goes into the HTML body, so they escape on the
	// way out.
	typeNames := make(map[int64]string)
	typeName := func(typeID int64) string {
		if typeID == 0 {
			return "Unknown"
		}
		if n, ok := typeNames[typeID]; ok {
			return n
		}
		n, _ := evedb.GetTypeName(ctx, sqldb, typeID)
		if n == "" {
			n = fmt.Sprintf("Unknown Type %d", typeID)
		}
		n = mailTextEscape(n)
		typeNames[typeID] = n
		return n
	}
	corpName := func(corpID int64) string {
		if corpID == 0 {
			return "NONE"
		}
		if n := evedb.GetCorporationName(ctx, sqldb, corpID); n != "" {
			return mailTextEscape(n)
		}
		return "NONE"
	}
	allianceName := func(allianceID int64) string {
		if allianceID == 0 {
			return "NONE"
		}
		if n, err := evedb.GetAllianceName(ctx, sqldb, allianceID); err == nil && n != "" {
			return mailTextEscape(n)
		}
		return "NONE"
	}

	sysName := evedb.GetSolarSystemName(ctx, sqldb, km.SysID)
	if sysName == "" {
		sysName = fmt.Sprintf("System %d", km.SysID)
	}
	sysSec, _, _ := evedb.GetSystemSecurity(ctx, sqldb, km.SysID)
	victimName := mailTextEscape(km.VictimName)

	// The client wraps the body in <html><body> and runs it through its HTML parser
	// (inbox.py SetValue(preformatted=1) -> LoadHTML). preformatted only expands tabs;
	// a raw newline collapses like any HTML whitespace, so every line break is a <br>.
	var b strings.Builder
	fmt.Fprintf(&b, "%s<br><br>", km.Time.UTC().Format("2006.01.02 15:04:05"))
	fmt.Fprintf(&b, "Victim: %s<br>", victimName)
	fmt.Fprintf(&b, "Alliance: %s<br>", allianceName(km.VictimAllianceID))
	fmt.Fprintf(&b, "Corp: %s<br>", corpName(km.VictimCorpID))
	fmt.Fprintf(&b, "Destroyed: %s<br>", typeName(km.ShipTypeID))
	fmt.Fprintf(&b, "System: %s<br>", sysName)
	fmt.Fprintf(&b, "Security: %.1f<br>", sysSec)
	b.WriteString("<br><b>Involved parties:</b><br>")
	for _, h := range km.Hits {
		// typeName() already escapes; the other two sources do not.
		name := mailTextEscape(h.Name)
		if name == "" {
			if h.IsNPC {
				name = typeName(h.ShipTypeID)
			} else if h.CharID != 0 {
				name = mailTextEscape(evedb.GetCharacterName(ctx, sqldb, h.CharID))
			}
		}
		if name == "" {
			name = "Unknown"
		}
		b.WriteString("<br>")
		if h.FinalBlow {
			fmt.Fprintf(&b, "Name: %s (laid the final blow)<br>", name)
		} else {
			fmt.Fprintf(&b, "Name: %s<br>", name)
		}
		fmt.Fprintf(&b, "Security: %.1f<br>", h.SecStatus)
		fmt.Fprintf(&b, "Alliance: %s<br>", allianceName(h.AllianceID))
		fmt.Fprintf(&b, "Corp: %s<br>", corpName(h.CorpID))
		fmt.Fprintf(&b, "Ship: %s<br>", typeName(h.ShipTypeID))
		fmt.Fprintf(&b, "Weapon: %s<br>", typeName(h.WeaponTypeID))
	}
	// Items sort fitted modules first, then drone bay, then cargo.
	flagRank := func(flag int64) int {
		switch flag {
		case dogma.FlagCargo:
			return 2
		case dogma.FlagDroneBay:
			return 1
		default:
			return 0
		}
	}
	writeItems := func(label string, items []evedb.EntityRow) {
		if len(items) == 0 {
			return
		}
		sort.SliceStable(items, func(i, j int) bool {
			return flagRank(items[i].Flag) < flagRank(items[j].Flag)
		})
		fmt.Fprintf(&b, "<br><b>%s:</b><br><br>", label)
		for _, it := range items {
			b.WriteString(typeName(it.TypeID))
			if it.Quantity > 1 {
				fmt.Fprintf(&b, ", Qty: %d", it.Quantity)
			}
			switch flagRank(it.Flag) {
			case 2:
				b.WriteString(" (Cargo)")
			case 1:
				b.WriteString(" (Drone Bay)")
			}
			b.WriteString("<br>")
		}
	}
	writeItems("Destroyed items", km.DestroyedItems)
	writeItems("Dropped items", km.DroppedItems)

	body := b.String()
	subject := "Report: Ship Destruction - " + km.VictimName
	created := int64(uint64(km.Time.UnixNano()/100) + 116444736000000000)

	recipients := []int64{km.VictimCharID}
	for _, h := range km.Hits {
		if h.FinalBlow && !h.IsNPC && h.CharID != 0 && h.CharID != km.VictimCharID {
			recipients = append(recipients, h.CharID)
		}
	}
	for _, rid := range recipients {
		msgID, err := evedb.SendKillMail(ctx, sqldb, rid, subject, body, created)
		if err != nil {
			log.Printf("[killmail] send to charID=%d: %v", rid, err)
			continue
		}
		pushMailNotification(rid, msgID, evedb.OwnerCONCORD, subject, created)
	}
	log.Printf("[killmail] %s (%s) destroyed in %s — %d involved, mailed %d recipients",
		km.VictimName, typeName(km.ShipTypeID), sysName, len(km.Hits), len(recipients))
}

// lootDropChance is the per-item probability that an item aboard a destroyed ship
// survives into the loot container: server_settings loot.playerDropChance, default
// 0.5, separate from the NPC loot gates. rates.lootDrop scales both.
func lootDropChance() float64 {
	return evedb.CachedSettingFloat("loot.playerDropChance", 0.5, 0, 1)
}

// spawnDeathLoot rolls every item aboard a destroyed ship (cargo, fitted modules,
// charges, drone bay) for survival, spawns a Cargo Container (typeID 23; this era
// has no wrecks) at the death position holding the survivors, and deletes the rest
// plus the hull from the DB. The container ball is broadcast and despawns on a timer.
func spawnDeathLoot(ctx context.Context, sqldb *sql.DB, ss *destiny.SolarSystem,
	victimCharID, shipDBID int64, deathPos destiny.Vec3) (dropped, destroyed []evedb.EntityRow) {
	dropped, destroyed = spawnContentsLoot(ctx, sqldb, ss, victimCharID, shipDBID, deathPos)
	// Remove the hull itself.
	if err := evedb.DeleteEntityDeep(ctx, sqldb, shipDBID); err != nil {
		log.Printf("[death-loot] DeleteEntityDeep ship=%d: %v", shipDBID, err)
	}
	return dropped, destroyed
}

// spawnContentsLoot rolls everything inside holderID (a ship, or a
// destroyed structure's bays) for survival, moves the survivors into a new
// loot container at pos owned by ownerID, and deletes the rest. The holder
// itself is left for the caller.
func spawnContentsLoot(ctx context.Context, sqldb *sql.DB, ss *destiny.SolarSystem,
	ownerID, holderID int64, deathPos destiny.Vec3) (dropped, destroyed []evedb.EntityRow) {
	victimCharID, shipDBID := ownerID, holderID
	children, err := evedb.GetShipChildItems(ctx, sqldb, shipDBID)
	if err != nil {
		log.Printf("[death-loot] GetShipChildItems holder=%d: %v", shipDBID, err)
	}

	var survivors []evedb.EntityRow
	for _, ch := range children {
		if rand.Float64() < lootDropChance()*rates.LootDrop {
			survivors = append(survivors, ch)
		} else {
			destroyed = append(destroyed, ch)
		}
	}

	var containerID int64
	if len(survivors) > 0 {
		containerID, err = evedb.CreateLootContainer(ctx, sqldb, victimCharID, ss.ID,
			deathPos.X, deathPos.Y, deathPos.Z)
		if err != nil {
			log.Printf("[death-loot] CreateLootContainer: %v", err)
			containerID = 0
		}
	}
	if containerID != 0 {
		for _, s := range survivors {
			if err := evedb.MoveItemIntoContainer(ctx, sqldb, s.ItemID, containerID); err != nil {
				log.Printf("[death-loot] MoveItemIntoContainer item=%d: %v", s.ItemID, err)
			}
		}
	} else {
		// No container (nothing survived or the insert failed): everything dies.
		destroyed = append(destroyed, survivors...)
		survivors = nil
	}
	for _, d := range destroyed {
		if err := evedb.DeleteEntityDeep(ctx, sqldb, d.ItemID); err != nil {
			log.Printf("[death-loot] DeleteEntityDeep item=%d: %v", d.ItemID, err)
		}
	}
	if containerID == 0 {
		return survivors, destroyed
	}
	log.Printf("[death-loot] container %d spawned at death of ship %d: %d dropped, %d destroyed",
		containerID, shipDBID, len(survivors), len(destroyed))
	spawnLootContainerBall(ss, sqldb, containerID, deathPos, victimCharID)
	return survivors, destroyed
}

// lootContainerBallSlim builds the BallDesc and SlimItem for a loot container.
// Shared by spawnLootContainerBall and loadLootContainersIntoSystem so the
// two paths cannot drift.
func lootContainerBallSlim(containerID int64, pos destiny.Vec3, ownerID int64) (destiny.BallDesc, destiny.SlimItem) {
	return destiny.BallDesc{
			EntityID: uint32(containerID), Mode: destiny.ModeRigid, SubType: 2,
			Pos: pos, Radius: 14,
		}, destiny.SlimItem{
			BallID: containerID, ItemID: containerID,
			TypeID: evedb.TypeCargoContainer, GroupID: 12, CategoryID: 2,
			OwnerID: ownerID,
		}
}

// loadLootContainersIntoSystem reloads persisted loot containers when a
// system is first entered. Containers are loaded with their real spawn time
// so the expiry sweep honours the original TTL. No per-can despawn goroutine
// is armed; the 60s sweep covers it.
func loadLootContainersIntoSystem(addr string, sqldb *sql.DB, ss *destiny.SolarSystem, sysID int64) {
	if sqldb == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cutoff := time.Now().Unix() - int64(lootContainerTTL().Seconds())
	containers, err := evedb.ListLootContainersForSystem(ctx, sqldb, sysID, cutoff)
	if err != nil {
		log.Printf("[%s] loadLootContainersIntoSystem sysID=%d: %v", addr, sysID, err)
		return
	}
	for _, c := range containers {
		pos := destiny.Vec3{X: c.X, Y: c.Y, Z: c.Z}
		bd, slim := lootContainerBallSlim(c.ContainerID, pos, c.OwnerID)
		if c.TypeID == evedb.TypeCorpse {
			bd, slim = corpseBallSlim(c.ContainerID, pos, c.OwnerID, c.Name)
		}
		ss.AddLootContainerAt(bd, slim, time.Unix(c.SpawnedAt, 0))
	}
	if len(containers) > 0 {
		log.Printf("[%s] loadLootContainersIntoSystem sysID=%d: restored %d containers", addr, sysID, len(containers))
	}
}

// spawnLootContainerBall registers a freshly created loot container's ball in
// the system, broadcasts it to every player, and arms the despawn timer.
// Shared by player ship death and NPC kill loot.
func spawnLootContainerBall(ss *destiny.SolarSystem, sqldb *sql.DB, containerID int64, pos destiny.Vec3, ownerID int64) {
	contBall, contSlim := lootContainerBallSlim(containerID, pos, ownerID)
	spawnLootBall(ss, sqldb, contBall, contSlim)
}

// spawnLootBall is spawnLootContainerBall for a prebuilt ball: a can, or a podded
// pilot's corpse (corpse.go), which shares the whole lifecycle.
func spawnLootBall(ss *destiny.SolarSystem, sqldb *sql.DB, contBall destiny.BallDesc, contSlim destiny.SlimItem) {
	containerID, pos := contSlim.ItemID, contBall.Pos
	ss.AddLootContainer(contBall, contSlim)
	contDmg := destiny.DamageState{Shield: 1.0, Tau: 1e9, Armor: 1.0, Structure: 1.0}
	// Grid-local delivery, range-gated like a respawned asteroid: the loot roll runs
	// off the death path, so the killer may be a grid away by the time the can exists.
	// Queued, not stamped: a can is another ball, and cans dropped in one tick share
	// one AddBalls (docs/destiny-stamps.md#budget-and-triage).
	rSq := destiny.BubbleRadius * destiny.BubbleRadius
	for _, p := range ss.GetAllPlayersInSystem() {
		if p.Conn == nil || !p.HasBallparkState() {
			continue
		}
		if p.IsInWarp() {
			continue
		}
		if p.Pos().Sub(pos).LenSq() > rSq {
			continue
		}
		if len(p.ClaimUnknownBalls(uint32(containerID))) == 0 {
			continue
		}
		p.QueueAddBalls("add:loot", contBall, contSlim, contDmg)
		// Pairs with the [npc-death] line: where the can went, by the server's reckoning.
		log.Printf("[npc-loot] container %d ball at (%.0f,%.0f,%.0f) — %.0f m from entity %d",
			containerID, pos.X, pos.Y, pos.Z, p.Pos().Sub(pos).Len(), p.ID)
	}

	// Despawn timer.
	go func() {
		defer safego.Recover("spawnLootBall")
		select {
		case <-time.After(lootContainerTTL()):
		case <-ss.Done():
			return
		}
		despawnLootContainer(ss, containerID, sqldb)
	}()
}

// npcLootDrop is one rolled loot line: a type and a quantity.
type npcLootDrop struct {
	typeID int64
	qty    int64
}

// rollNPCLoot rolls what one NPC carries: its authored per-ball drops, its
// role pick and the gated general haul. Shared by the death drop
// (spawnNPCLoot) and by opening a lootable dungeon structure in place
// (npccargo.go), so both roll the same tables the same way.
func rollNPCLoot(ctx context.Context, sqldb *sql.DB, npcTypeID, npcGroupID int64, killedRole string, dunObjectID int64) []npcLootDrop {
	// Only the belt spawner records a role; a dungeon rat takes the role its type has
	// in the belt tables, so a belt commander rolls the commander pool here too.
	if killedRole == "" && dunObjectID > 0 {
		role, err := evedb.BeltSpawnRoleForType(ctx, sqldb, npcTypeID)
		if err != nil {
			log.Printf("[npc-loot] BeltSpawnRoleForType type=%d: %v", npcTypeID, err)
		}
		killedRole = role
	}
	if killedRole == "" {
		killedRole = "normal"
	}

	entries, err := evedb.GetLootDropsForNPC(ctx, sqldb, npcTypeID, npcGroupID)
	if err != nil {
		log.Printf("[npc-loot] GetLootDropsForNPC type=%d group=%d: %v", npcTypeID, npcGroupID, err)
		return nil
	}

	// Per-ball dungeon drops are authored on the dunObjects row, so they are
	// looked up even when the NPC's type/group tables are empty.
	var ballLoot []evedb.DunObjectLootRow
	var ballMinDrops, ballMaxDrops int64
	if dunObjectID > 0 {
		ballLoot, err = evedb.GetObjectLoot(ctx, sqldb, dunObjectID)
		if err != nil {
			log.Printf("[npc-loot] GetObjectLoot object=%d: %v", dunObjectID, err)
		}
		if len(ballLoot) > 0 {
			ballMinDrops, ballMaxDrops, err = evedb.GetObjectLootDropRange(ctx, sqldb, dunObjectID)
			if err != nil {
				log.Printf("[npc-loot] GetObjectLootDropRange object=%d: %v", dunObjectID, err)
			}
		}
	}
	if len(entries) == 0 && len(ballLoot) == 0 {
		return nil
	}

	maxDrops, err := evedb.GetNPCLootCap(ctx, sqldb, npcTypeID, npcGroupID)
	if err != nil {
		log.Printf("[npc-loot] GetNPCLootCap type=%d group=%d: %v", npcTypeID, npcGroupID, err)
	}

	var rolled []npcLootDrop

	// Per-ball drops roll first on their own terms: an independent chance each, no haul
	// gate, class exclusion or cap. lootMin/MaxDrops clamp the count (evedb.RollBallLoot).
	if len(ballLoot) > 0 {
		rng := rand.New(rand.NewSource(rand.Int63()))
		for _, d := range evedb.RollBallLoot(ballLoot, ballMinDrops, ballMaxDrops, rng) {
			rolled = append(rolled, npcLootDrop{d.ItemTypeID, d.Qty})
		}
	}

	// The role pick, the bounty pick and the gated general haul: evedb.RollLootTables, the
	// roll the rolebots use for unwatched kills too. Counted in the [loot-stats] tally.
	drops := evedb.RollLootTables(entries, killedRole, maxDrops, evedb.GlobalLootRNG)
	evedb.NoteLootRoll(evedb.LootStatWatched, killedRole, drops)
	for _, d := range drops {
		rolled = append(rolled, npcLootDrop{d.ItemTypeID, d.Qty})
	}
	return rolled
}

// spawnNPCLoot rolls the loot tables for a killed NPC (rollNPCLoot) and, when
// anything drops, spawns a Cargo Container at the death position holding the
// items. Runs in its own goroutine off the death path.
func spawnNPCLoot(sqldb *sql.DB, ss *destiny.SolarSystem, npcTypeID, npcGroupID, ownerID int64, deathPos destiny.Vec3, killedRole string, dunObjectID int64) {
	defer safego.Recover("spawnNPCLoot")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rolled := rollNPCLoot(ctx, sqldb, npcTypeID, npcGroupID, killedRole, dunObjectID)
	if len(rolled) == 0 {
		return
	}

	containerID, err := evedb.CreateLootContainer(ctx, sqldb, ownerID, ss.ID,
		deathPos.X, deathPos.Y, deathPos.Z)
	if err != nil {
		log.Printf("[npc-loot] CreateLootContainer: %v", err)
		return
	}
	created := 0
	for _, d := range rolled {
		if err := evedb.CreateNPCLootItem(ctx, sqldb, containerID, ownerID, d.typeID, d.qty); err != nil {
			log.Printf("[npc-loot] CreateNPCLootItem type=%d qty=%d: %v", d.typeID, d.qty, err)
			continue
		}
		created++
	}
	if created == 0 {
		// Nothing made it in: do not leave an empty can in space.
		dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer dcancel()
		_ = evedb.DeleteLootContainer(dctx, sqldb, containerID)
		return
	}
	log.Printf("[npc-loot] container %d spawned for NPC type=%d group=%d dunObject=%d: %d of %d rolled drops",
		containerID, npcTypeID, npcGroupID, dunObjectID, created, len(rolled))
	spawnLootContainerBall(ss, sqldb, containerID, deathPos, ownerID)
}

// despawnLootContainer removes a loot container from space (RemoveBalls to every
// player that knows it) and deletes it and any contents from the DB. A second
// call is a no-op.
func despawnLootContainer(ss *destiny.SolarSystem, containerID int64, sqldb *sql.DB) {
	if !ss.RemoveLootContainer(uint32(containerID)) {
		return
	}
	removeLootBallFromGrid(ss, containerID)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := evedb.DeleteLootContainer(ctx, sqldb, containerID); err != nil {
		log.Printf("[death-loot] DeleteLootContainer %d: %v", containerID, err)
	}
	log.Printf("[death-loot] container %d despawned", containerID)
}

// lootContainerTTL is how long an unlooted kill-spawned container (wreck can,
// corpse) stays in space: server_settings loot.containerHours, default 2. One dial
// for both the per-container timer and the expiry sweep.
func lootContainerTTL() time.Duration {
	return evedb.CachedSettingDuration("loot.containerHours", time.Hour, 2, 0.1, 24*30)
}

// startLootStatsLog writes the loot roll tally to the log every 5 minutes and resets it
// ([loot-stats] lines, evedb.DrainLootStatsLines): kills, drop rate, items per kill, the top
// item groups and items, watched and unwatched apart. Nothing is logged for a quiet interval.
func startLootStatsLog(tickMgr *tick.Manager) {
	tickMgr.RegisterPeriodic("loot-stats", 300, func(uint64) bool {
		if globalDB == nil {
			return true
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, line := range evedb.DrainLootStatsLines(ctx, globalDB, 12, 10) {
			log.Printf("[loot-stats] %s", line)
		}
		return true
	})
}

// startLootExpirySweep despawns kill-spawned containers after lootContainerTTL even
// if never opened; the only other removal is a player looting the can empty, and
// every SetState pays for the slices. Every 60th pass also expires containers in
// unloaded systems directly in the DB.
func startLootExpirySweep(tickMgr *tick.Manager) {
	var passCount uint64
	tickMgr.RegisterPeriodic("loot-expiry-sweep", 60, func(uint64) bool {
		if globalSysMgr == nil || globalDB == nil {
			return true
		}
		cutoff := time.Now().Add(-lootContainerTTL())
		for _, ss := range globalSysMgr.All() {
			for _, id := range ss.LootContainersOlderThan(cutoff) {
				log.Printf("[death-loot] container %d expired (unlooted for %v)", id, lootContainerTTL())
				despawnLootContainer(ss, id, globalDB)
			}
		}
		// Every 60th pass (~hourly): expire containers in unloaded systems.
		passCount++
		if passCount%60 == 0 {
			loaded := make(map[int64]struct{})
			for _, ss := range globalSysMgr.All() {
				loaded[ss.ID] = struct{}{}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			n, err := evedb.DeleteExpiredLootContainers(ctx, globalDB, cutoff.Unix(), loaded)
			cancel()
			if err != nil {
				log.Printf("[death-loot] DeleteExpiredLootContainers: %v", err)
			} else if n > 0 {
				log.Printf("[death-loot] expired %d loot containers in unloaded systems", n)
			}
		}
		return true
	})
}

// startMapHistoryPurgeSweep deletes starmap history buckets older than 48h.
// Runs hourly (3600 ticks at 1s/tick).
func startMapHistoryPurgeSweep(tickMgr *tick.Manager, sqldb *sql.DB) {
	tickMgr.RegisterPeriodic("map-history-purge", 3600, func(uint64) bool {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := evedb.PurgeOldMapHistory(ctx, sqldb, 48); err != nil {
			log.Printf("[map] history purge: %v", err)
		}
		return true
	})
}

// orphanCapsuleSweepTicks is how often a running server deletes capsules nobody is aboard
// (10 minutes at 1s/tick). Boot does the same with no age limit.
const orphanCapsuleSweepTicks = 600

// startOrphanCapsuleSweep deletes capsules no character is aboard while the server runs, not
// only at boot. The core's own paths delete the capsule a pilot leaves (boardDockedShip,
// MoveCharacterIntoShip), but a module that moves its pilots with its own SQL (rolebots
// boarding hulls, respawning) can leave pods behind, and each one shows in a station hangar.
// Only capsules that already existed at the previous sweep are taken: one created a moment
// ago may be about to receive its pilot. The DB work runs off the tick lane.
func startOrphanCapsuleSweep(tickMgr *tick.Manager, sqldb *sql.DB) {
	if sqldb == nil {
		return
	}
	var running atomic.Bool
	var highWater int64 // entity MAX(itemID) at the previous sweep; 0 until the first
	tickMgr.RegisterPeriodic("orphan-capsule-sweep", orphanCapsuleSweepTicks, func(uint64) bool {
		if !running.CompareAndSwap(false, true) {
			return true
		}
		go func() {
			defer running.Store(false)
			defer safego.Recover("orphanCapsuleSweep")
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			next, err := evedb.MaxEntityItemID(ctx, sqldb)
			if err != nil {
				log.Printf("[capsule] orphan sweep: %v", err)
				return
			}
			if highWater > 0 {
				if n, err := evedb.CleanOrphanCapsulesUpTo(ctx, sqldb, highWater); err != nil {
					log.Printf("[capsule] orphan sweep: %v", err)
					return
				} else if n > 0 {
					log.Printf("[capsule] orphan sweep: deleted %d capsule(s) no pilot is aboard", n)
				}
			}
			highWater = next
		}()
		return true
	})
}

// despawnLootContainerIfEmpty removes a kill-spawned loot container from
// space once its last item has been taken. Only containers registered in the
// system's loot slices qualify, so player-dropped cans are never affected.
func despawnLootContainerIfEmpty(ds *destinyConn, sqldb *sql.DB, containerID int64) {
	if sqldb == nil || !isLootContainer(ds, containerID) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	empty, err := evedb.IsContainerEmpty(ctx, sqldb, containerID)
	if err != nil {
		log.Printf("[death-loot] IsContainerEmpty %d: %v", containerID, err)
		return
	}
	if empty {
		despawnLootContainer(ds.solarSystem, containerID, sqldb)
	}
}

// handlePlayerShipDestroyed handles the destruction of a player's ship
// (non-capsule). It removes the old ship, spawns a capsule at the death
// position, and transitions the player into the pod.
func handlePlayerShipDestroyed(ds *destinyConn, sqldb *sql.DB, npcMgr *npc.NPCManager) {
	cancelPendingDock(ds)
	if ds.solarSystem == nil || ds.entity == nil {
		return
	}
	c := ds.entity.Conn
	addr := ds.entity.Addr
	accountID := ds.entity.AccountID
	charID := ds.charID

	log.Printf("[death] player ship %d (type=%d) destroyed for charID=%d", ds.shipID, ds.shipTypeID, charID)

	// Record ship kill for starmap overlay.
	go func() {
		defer safego.Recover("handlePlayerShipDestroyed")
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		evedb.RecordMapEvent(ctx, sqldb, ds.sysID, evedb.StatShipKill, 1)
	}()

	// Snapshot the kill report inputs before the combat state and the entity go; item
	// lists are filled after the loot roll and the mail goes out in the background.
	km := killReport{
		VictimCharID:     charID,
		VictimName:       ds.charName,
		VictimCorpID:     ds.corpID,
		VictimAllianceID: ds.allianceID,
		ShipTypeID:       ds.shipTypeID,
		SysID:            ds.sysID,
		Time:             time.Now(),
		Hits:             ds.entity.KillHitsSnapshot(),
	}

	// Charged here, before clearPlayerCombatState drops the crimewatch state that says
	// who started the fight and the explosion sleep lets the killer warp off.
	chargeForPlayerDeathFromReport(sqldb, ds, km, false)

	// 1. Cancel all combat state.
	clearPlayerCombatState(ds)

	// The cynosural field dies with the ship that lit it, here while the system is
	// live and the grid still holds every ball; after RemoveEntity nobody could remove it.
	if ds.shipID != 0 {
		collapseCynoBeacon(ds.shipID, "ship destroyed")
		closeJumpBridge(ds.shipID, "ship destroyed")
	}

	// A destroyed ship loses control of its drones; they idle in space.
	if globalDroneMgr != nil && ds.shipID != 0 {
		globalDroneMgr.AbandonControl(ds.shipID)
	}

	// 2. Capture death position before removing entity.
	deathPos := ds.dm.Pos
	km.Pos = deathPos

	// 3. Remove destroyed ship from system with terminal explosion.
	oldShipID := ds.entity.ID
	oldEntity := ds.entity
	ss := ds.solarSystem

	// TerminalRemove to witnesses sets explodeOnRemove on the client ball, so the
	// death explosion plays during ball release.
	remaining := ss.RemoveEntity(oldShipID)
	modapi.FireEntityRemoved(ss, oldShipID, 0)
	for _, p := range remaining {
		if p.Conn == nil || !p.KnowsBall(oldShipID) {
			continue
		}
		// Urgent: death class; the witness must see the explosion where it happened.
		// See docs/destiny-stamps.md#death-snap-lead.
		p.SendStampedUrgent(func(stamp uint32) error {
			return destiny.SendTerminalRemove(p.Conn, globalNodeID, p.AccountID, stamp, oldShipID)
		})
		p.ForgetBalls(oldShipID)
	}

	// TerminalRemove to the dying player: their own ship explodes before the ball is
	// released. Urgent: ego and a death; without it the pilot watches a frozen grid.
	if c != nil {
		oldEntity.SendStampedUrgent(func(stamp uint32) error {
			return destiny.SendTerminalRemove(c, globalNodeID, accountID, stamp, oldShipID)
		})
	}

	// Delay to let the explosion animation play on the client before
	// spawning the pod and transitioning the session.
	time.Sleep(3 * time.Second)

	// 4. Insurance payout (background).
	oldShipDBID := ds.shipID
	oldShipTypeID := ds.shipTypeID
	go func() {
		defer safego.Recover("handlePlayerShipDestroyed")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		contract, err := evedb.GetContractForShip(ctx, sqldb, oldShipDBID)
		if err != nil || contract == nil {
			return
		}
		basePrice, _ := evedb.GetInsurancePrice(ctx, sqldb, oldShipTypeID)
		if basePrice <= 0 {
			return
		}
		// Fraction is a payout fraction in (0, 1]. Anything else is refused, not paid:
		// the column is a plain DOUBLE, and basePrice times an ISK amount mints ISK.
		if contract.Fraction <= 0 || contract.Fraction > 1 {
			log.Printf("[death] insurance contract %d has out-of-range fraction %v — no payout",
				contract.ContractID, contract.Fraction)
			return
		}
		payout := basePrice * contract.Fraction
		// The payout goes to the contract holder: a corp-insured hull pays the corp
		// wallet, never the pilot.
		payee := contract.OwnerID
		if payee == 0 {
			payee = charID
		}
		toCorp := payee != charID
		var newBal float64
		if toCorp {
			if err := evedb.CreditCorpWallet(ctx, sqldb, payee, payout); err != nil {
				log.Printf("[death] insurance CreditCorpWallet corpID=%d: %v", payee, err)
				return
			}
			newBal, _ = evedb.GetCorpBalance(ctx, sqldb, payee)
		} else {
			if err := evedb.GiveISK(ctx, sqldb, charID, payout); err != nil {
				log.Printf("[death] insurance GiveISK charID=%d: %v", charID, err)
				return
			}
			newBal, _ = evedb.GetCharacterBalance(ctx, sqldb, charID)
		}
		_ = evedb.UnInsureShip(ctx, sqldb, oldShipDBID)
		_ = evedb.InsertJournalEntry(ctx, sqldb, payee, 19, evedb.OwnerPendInsurance, payee, payout, newBal, "")
		log.Printf("[death] insurance payout %.0f ISK to ownerID=%d (ship type=%d)", payout, payee, oldShipTypeID)
		// Send payout notification mail.
		shipName, _ := evedb.GetTypeName(ctx, sqldb, oldShipTypeID)
		if shipName == "" {
			shipName = fmt.Sprintf("Ship (type %d)", oldShipTypeID)
		}
		payoutBody := fmt.Sprintf(
			"Dear Pilot,\n\nThis is to notify you that your %s has been destroyed. Your insurance contract has been paid out.\n\nPayout amount: %.2f ISK\n\nYour contract has now expired.\n\nFly safe.\n\n- Pend Insurance",
			shipName, payout)
		now := uint64(time.Now().UnixNano()/100) + 116444736000000000
		_ = evedb.SendInsuranceMail(ctx, sqldb, charID, "Insurance Payout", payoutBody, int64(now))
		// Only the personal wallet is pushed: OnAccountChange with "charid" repaints the
		// pilot's own balance, so a corp figure would show as theirs until relog.
		if c != nil && !toCorp {
			notifArgs := marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyString{Value: "cash"},
				marshal.PyInt{Value: uint64(charID)},
				marshal.PyReal{Value: newBal},
			}}
			_ = server.SendNotification(c, int64(accountID), "OnAccountChange", "charid", notifArgs)
		}
	}()

	// 5. DB: find or create capsule, move character into it.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	capsuleID, err := evedb.GetCharacterCapsuleID(ctx, sqldb, charID)
	if err != nil {
		log.Printf("[death] GetCharacterCapsuleID charID=%d: %v", charID, err)
	}
	if capsuleID == 0 {
		capsuleID, err = evedb.CreateCapsuleEntity(ctx, sqldb, charID, ds.sysID, ds.charName)
		if err != nil {
			log.Printf("[death] CreateCapsuleEntity charID=%d: %v", charID, err)
			// The pilot is stuck, but the kill still happened: file the record. Item lists
			// stay empty because the loot roll never ran.
			fileKillmail(sqldb, km, false)
			return
		}
	}
	if err := evedb.MoveCharacterIntoShip(ctx, sqldb, charID, capsuleID); err != nil {
		log.Printf("[death] MoveCharacterIntoShip charID=%d capsule=%d: %v", charID, capsuleID, err)
	}
	// Roll loot drops, spawn the death cargo container, and delete the hull
	// plus destroyed items from the DB.
	km.DroppedItems, km.DestroyedItems = spawnDeathLoot(ctx, sqldb, ss, charID, oldShipDBID, deathPos)
	// Fail courier contracts whose crate was aboard, so the issuer gets collateral
	// at once.
	if n := evedb.FailCourierCratesOnShip(ctx, sqldb, charID, oldShipDBID); n > 0 {
		log.Printf("[death] %d courier contract(s) failed — crate destroyed with ship %d", n, oldShipDBID)
	}
	// Killboard row, and the CONCORD kill report to victim + final-blow pilot.
	fileKillmail(sqldb, km, false)
	// Position capsule at death location.
	if err := evedb.UpdateEntityPosition(ctx, sqldb, capsuleID, deathPos.X, deathPos.Y, deathPos.Z); err != nil {
		log.Printf("[death] UpdateEntityPosition capsule=%d: %v", capsuleID, err)
	}

	// 6. Build capsule destiny state (mirrors registerUndock pattern).
	podStats, err := evedb.GetShipStats(ctx, sqldb, capsuleID, charID, 0)
	if err != nil {
		log.Printf("[death] GetShipStats capsule=%d: %v", capsuleID, err)
		// Fallback: minimal stats for capsule.
		podStats = &evedb.ShipStats{Mass: 32000, Radius: 30, MaxVelocity: 250, Agility: 0.00000001}
	}

	podDM := destiny.New(uint32(capsuleID), podStats.Mass, podStats.Agility, podStats.MaxVelocity, podStats.Radius)
	podDM.Pos = deathPos
	podDM.Stamp = ds.stamp

	podEntity := &destiny.Entity{
		ID:        uint32(capsuleID),
		Manager:   podDM,
		AccountID: accountID,
		Conn:      c,
		Addr:      addr,
		CharName:  ds.charName,
		BubbleID:  -1,
		IsPlayer:  true,
		Slim: destiny.SlimItem{
			BallID:         capsuleID,
			ItemID:         capsuleID,
			TypeID:         670,
			GroupID:        29, // Capsule
			CategoryID:     6,  // Ship
			OwnerID:        charID,
			CorpID:         ds.corpID,
			AllianceID:     ds.allianceID,
			CharID:         charID,
			SecurityStatus: ds.securityStatus,
			Bounty:         ds.bounty,
		},
		Damage: fullDamageState(),
	}
	podDM.SetEntity(podEntity)
	podEntity.PlayerState = ds

	// Initialize capsule HP.
	ehp, hpErr := evedb.GetEntityHPByType(ctx, sqldb, 670)
	if hpErr == nil {
		podEntity.HP = destiny.HPState{
			MaxShield: ehp.ShieldHP, CurShield: ehp.ShieldHP,
			MaxArmor: ehp.ArmorHP, CurArmor: ehp.ArmorHP,
			MaxStructure: ehp.StructureHP, CurStructure: ehp.StructureHP,
			ShieldTau: ehp.ShieldTau,
		}
		podEntity.Resists = [3][4]float64{ehp.ShieldRes, ehp.ArmorRes, ehp.HullRes}
	}

	// Wire up TargetLookup (same lookup as registerUndock).
	podDM.TargetLookup = playerNavTargetLookup(ss)

	// Transfer NPC standings from destroyed ship entity to pod.
	podEntity.NPCStandings = oldEntity.NPCStandings

	// 7. Transfer ballpark state (knownBalls, stamps) from old entity to pod
	// so the client's existing ball tracking stays intact.
	podEntity.TransferBallparkState(oldEntity, oldShipID)

	// 8. Update destinyConn to reflect capsule.
	ds.dm = podDM
	ds.shipID = capsuleID
	ds.shipTypeID = 670
	ds.shipGroupID = 29
	ds.shipCategoryID = 6
	ds.shipName = ""
	applyGMMode(ds.charID, podEntity)
	ds.entity = podEntity
	ds.setStateSent = true
	ds.destinyBaseStamp = podEntity.BaseStamp
	ds.destinyBaseTime = podEntity.BaseTime

	// 9. Register capsule in solar system and notify bubble mates.
	podBubbleMates, podMerged, podOK := ss.AddEntity(podEntity)
	if !podOK {
		// The system was torn down during the explosion delay. Re-acquire one so the pod
		// exists; celestial and NPC state reload on the next registerUndock into it.
		log.Printf("[death] sysID=%d torn down during pod spawn, re-acquiring", ss.ID)
		ss, _ = ds.sysMgr.GetOrCreate(ss.ID)
		ds.solarSystem = ss
		ss.StartTickLoop()
		podBubbleMates, podMerged, _ = ss.AddEntity(podEntity)
	}
	if podMerged {
		for _, p := range podBubbleMates {
			if !p.IsPlayer || p.Conn == nil || p.Conn.IsClosed() {
				continue
			}
			ensureBallsKnown(p, podBubbleMates...)
		}
	} else {
		for _, mate := range podBubbleMates {
			ensureBallsKnown(mate, podEntity)
		}
	}

	// 10. Send the pod ball and session change to the dying player; the old ship ball
	// went with TerminalRemove in step 3 and the delay let the explosion render.
	if c != nil {
		// AddBalls for the pod so it appears in the player's ballpark.
		podBD := podEntity.BallDesc()
		podBD.CorpID = uint32(podEntity.Slim.CorpID)
		podBD.Name = podEntity.BallName()
		podDmg := podEntity.DamageStateFrac()
		podDmg.Timestamp = server.Win32TimeNow()
		// Urgent: pod handover, a named exemption. Refusing the capsule's
		// own AddBalls leaves the pilot with no ball at all.
		if err := podEntity.SendStampedUrgent(func(addStamp uint32) error {
			return destiny.SendAddBallsFull(c, globalNodeID, accountID, addStamp,
				[]destiny.BallDesc{podBD},
				[]destiny.SlimItem{podEntity.Slim},
				map[uint32]destiny.DamageState{podEntity.ID: podDmg})
		}); err != nil {
			log.Printf("[death] SendAddBallsFull pod: %v", err)
		}

		// Session change tells client "your ship is now this pod".
		if err := sendShipIDSessionChange(c, addr, oldShipDBID, capsuleID); err != nil {
			log.Printf("[death] sendShipIDSessionChange: %v", err)
		}
	}

	log.Printf("[death] player charID=%d ejected into capsule %d at (%.0f, %.0f, %.0f)",
		charID, capsuleID, deathPos.X, deathPos.Y, deathPos.Z)
}

// handlePlayerPodKilled handles the destruction of a player's capsule.
// It removes the pod from space and respawns the player at their home station.
func handlePlayerPodKilled(ds *destinyConn, sqldb *sql.DB) {
	cancelPendingDock(ds)
	if ds.solarSystem == nil || ds.entity == nil {
		return
	}
	c := ds.entity.Conn
	addr := ds.entity.Addr
	accountID := ds.entity.AccountID
	charID := ds.charID

	log.Printf("[death] player pod %d killed for charID=%d", ds.shipID, charID)

	// The implants die with the pod, before the report so they are on it, and the
	// clone is spent (SP above its cap lost, back to Alpha; docs/medical-clones.md).
	// Both run under WithTrainingRetimed: training done so far is banked at the
	// augmented rate, and the restart aims at the skill's new level.
	var lostImplants []evedb.EntityRow
	var cloneDeath evedb.CloneDeathResult
	{
		ictx, icancel := context.WithTimeout(context.Background(), 10*time.Second)
		before := charAttrSnapshot(ictx, sqldb, charID)
		restarted, end, ierr := evedb.WithTrainingRetimed(ictx, sqldb, charID, func() error {
			var derr, cerr error
			lostImplants, derr = evedb.DestroyCharacterImplants(ictx, sqldb, charID)
			cloneDeath, cerr = evedb.ApplyCloneDeath(ictx, sqldb, charID)
			return errors.Join(derr, cerr)
		})
		if ierr != nil {
			log.Printf("[death] charID=%d implants/clone: %v", charID, ierr)
		}
		for i := range lostImplants {
			if c != nil {
				_ = server.SendNotification(c, int64(accountID), "OnItemChange", "charid", itemDeletedNotifArgs(&lostImplants[i]))
			}
		}
		if cloneDeath.SPLost > 0 {
			if c != nil {
				sendModuleAttributeChange(c, int64(accountID), charID, cloneDeath.SkillItemID, 276,
					float64(cloneDeath.NewSP), float64(cloneDeath.OldSP))
				if cloneDeath.NewLevel != cloneDeath.OldLevel {
					sendModuleAttributeChange(c, int64(accountID), charID, cloneDeath.SkillItemID, 280,
						float64(cloneDeath.NewLevel), float64(cloneDeath.OldLevel))
				}
			}
			log.Printf("[death] charID=%d clone %s spent: %d SP lost from %s (level %d -> %d)", charID,
				cloneDeath.CloneName, cloneDeath.SPLost, cloneDeath.SkillName, cloneDeath.OldLevel, cloneDeath.NewLevel)
		}
		afterImplantChange(ictx, c, int64(accountID), charID, before, restarted, end, sqldb)
		if len(lostImplants) > 0 {
			log.Printf("[death] charID=%d lost %d implants with the pod", charID, len(lostImplants))
		}
		icancel()
	}

	// Record pod kill for starmap overlay.
	go func() {
		defer safego.Recover("handlePlayerPodKilled")
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		evedb.RecordMapEvent(ctx, sqldb, ds.sysID, evedb.StatPodKill, 1)
	}()

	// Kill report and killboard row for the pod loss; the only items are the implants.
	// Snapshotted before clearPlayerCombatState tears the hit table down.
	podKM := killReport{
		VictimCharID:     charID,
		VictimName:       ds.charName,
		VictimCorpID:     ds.corpID,
		VictimAllianceID: ds.allianceID,
		ShipTypeID:       ds.shipTypeID,
		SysID:            ds.sysID,
		Pos:              ds.dm.Pos,
		Time:             time.Now(),
		Hits:             ds.entity.KillHitsSnapshot(),
		DestroyedItems:   lostImplants,
	}
	fileKillmail(sqldb, podKM, true)

	// Charged before clearPlayerCombatState: a pod kill is priced on whether the
	// victim ever shot back, which only the aggression table knows.
	chargeForPlayerDeathFromReport(sqldb, ds, podKM, true)

	// 1. Cancel all combat state.
	clearPlayerCombatState(ds)

	oldSysID := ds.sysID
	oldPodID := ds.shipID

	// 2. Send explosion FX to dying player before removing ball.
	podEntityID := ds.entity.ID
	ss := ds.solarSystem

	// TerminalRemove to witnesses: explosion plus ball removal.
	remaining := ss.RemoveEntity(podEntityID)
	modapi.FireEntityRemoved(ss, podEntityID, 0)
	for _, p := range remaining {
		if p.Conn == nil || !p.KnowsBall(podEntityID) {
			continue
		}
		// Urgent: death class, same as the ship-death witness loop.
		p.SendStampedUrgent(func(stamp uint32) error {
			return destiny.SendTerminalRemove(p.Conn, globalNodeID, p.AccountID, stamp, podEntityID)
		})
		p.ForgetBalls(podEntityID)
	}

	// TerminalRemove to the dying player: they see their pod explode. Urgent: ego
	// and a death, as on the ship path.
	if c != nil {
		ds.entity.SendStampedUrgent(func(stamp uint32) error {
			return destiny.SendTerminalRemove(c, globalNodeID, accountID, stamp, podEntityID)
		})
	}

	// The pilot stays behind: "<name>'s Frozen Corpse" where the pod blew,
	// sent after the pod's removal so a witness sees explosion, then body.
	go spawnPodCorpse(sqldb, ss, charID, ds.charName, podKM.Pos)

	// Delay to let the pod explosion animation play before respawning
	// at the home station.
	time.Sleep(3 * time.Second)

	// 3. Look up home station.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	homeStationID, err := evedb.GetHomeStationID(ctx, sqldb, charID)
	if err != nil || homeStationID == 0 {
		homeStationID = 60004420 // Todaki fallback
		log.Printf("[death] charID=%d has no homeStation, falling back to Todaki %d", charID, homeStationID)
	}

	homeLoc, err := evedb.GetStationLocation(ctx, sqldb, homeStationID)
	if err != nil {
		log.Printf("[death] GetStationLocation homeStation=%d: %v", homeStationID, err)
		return
	}

	// 4. Delete old pod, create fresh capsule at home station.
	if err := evedb.DeleteEntity(ctx, sqldb, oldPodID); err != nil {
		log.Printf("[death] DeleteEntity pod=%d: %v", oldPodID, err)
	}
	newCapsuleID, err := evedb.CreateCapsuleEntity(ctx, sqldb, charID, homeStationID, ds.charName)
	if err != nil {
		log.Printf("[death] CreateCapsuleEntity at homeStation=%d: %v", homeStationID, err)
		return
	}
	if err := evedb.MoveCharacterIntoShip(ctx, sqldb, charID, newCapsuleID); err != nil {
		log.Printf("[death] MoveCharacterIntoShip charID=%d capsule=%d: %v", charID, newCapsuleID, err)
	}

	// 5. Update character location to home station.
	if err := evedb.UpdateCharacterStation(ctx, sqldb, charID, homeStationID, homeLoc.SolarSystemID, homeLoc.ConstellationID, homeLoc.RegionID); err != nil {
		log.Printf("[death] UpdateCharacterStation charID=%d: %v", charID, err)
	}

	// 6. Ensure rookie ship at home station.
	if rookieID, err := evedb.EnsureRookieShip(ctx, sqldb, charID, homeStationID); err != nil {
		log.Printf("[death] EnsureRookieShip charID=%d: %v", charID, err)
	} else if rookieID != 0 {
		log.Printf("[death] seeded rookie ship %d at homeStation=%d", rookieID, homeStationID)
	}

	// 7. Clean up in-space state (mirrors unregisterDock).
	releaseSystem(ds.sysMgr, ss, sqldb)
	if ds.cancelReconcile != nil {
		ds.cancelReconcile()
		ds.cancelReconcile = nil
	}
	// As in unregisterDock: the pod is gone and the client is docked at home.
	if ds.entity != nil {
		ds.entity.ClearBallparkState()
	}
	ds.solarSystem = nil
	ds.entity = nil
	ds.dm = nil
	ds.setStateSent = false

	// 8. Update connection state for docked-at-home.
	if ds.stationIDPtr != nil {
		*ds.stationIDPtr = homeStationID
	}
	updateSessionLocation(addr, homeStationID, homeLoc.SolarSystemID)
	if globalStationReg != nil && c != nil && charID != 0 {
		globalStationReg.Join(c, homeStationID, charID)
	}
	ds.shipID = newCapsuleID
	ds.shipTypeID = 670
	ds.shipGroupID = 29
	ds.shipCategoryID = 6
	ds.shipName = ""
	ds.sysID = homeLoc.SolarSystemID
	ds.constellationID = homeLoc.ConstellationID
	ds.regionID = homeLoc.RegionID

	// Swap Local: leave the death system's channel, join the home system's, or the
	// client keeps two Local tabs. Location is already persisted (step 5).
	if c != nil && oldSysID != homeLoc.SolarSystemID {
		lsc.SwapSystemChannel(c, int64(accountID), oldSysID, homeLoc.SolarSystemID, globalLSCReg, sqldb)
	}

	// 10. Send the respawn session change: docked at the home station.
	if c != nil {
		if err := sendRespawnSessionChange(c, addr, accountID, oldSysID, homeStationID, homeLoc.SolarSystemID, homeLoc.ConstellationID, homeLoc.RegionID, oldPodID, newCapsuleID); err != nil {
			log.Printf("[death] sendRespawnSessionChange: %v", err)
		}
	}

	// 11. Tell the pilot which clone woke and what the death cost.
	if cloneDeath.CloneName != "" {
		stationName := evedb.GetStationName(ctx, sqldb, homeStationID)
		if cloneDeath.SPLost > 0 {
			pushRemoteMessage(c, int64(accountID), "CloneActivatedSPLostMsg", map[string]marshal.PyRep{
				"cloneTypeName":  marshal.PyString{Value: cloneDeath.CloneName},
				"stationname":    marshal.PyString{Value: stationName},
				"spLost":         marshal.PyString{Value: commaGroups(cloneDeath.SPLost)},
				"skillDecreased": marshal.PyString{Value: cloneDeath.SkillName},
			})
		} else {
			pushRemoteMessage(c, int64(accountID), "CloneActivatedMsg", map[string]marshal.PyRep{
				"cloneTypeName": marshal.PyString{Value: cloneDeath.CloneName},
				"stationname":   marshal.PyString{Value: stationName},
			})
		}
	}

	log.Printf("[death] player charID=%d respawned at homeStation=%d (system=%d) in capsule %d",
		charID, homeStationID, homeLoc.SolarSystemID, newCapsuleID)
}

// sendStandingNotification tells a client its standing from an NPC owner moved from
// oldStanding to newStanding. It sends OnStandingsModified, and only for a row the client
// already holds; OnStandingSet and a push for an absent row both leave a bare list where
// every reader wants a row (standingsvc.py). The client fetches an absent row at its next login.
func sendStandingNotification(c *evenet.Conn, accountID int64, fromID, charID int64, oldStanding, newStanding float64) {
	if c == nil || !standing2.ClientHoldsNPCStanding(charID, fromID) {
		return
	}
	m, minAbs, maxAbs, ok := standingModification(oldStanding, newStanding)
	if !ok {
		return
	}
	args := marshal.PyTuple{Items: []marshal.PyRep{marshal.PyList{Items: []marshal.PyRep{
		marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(fromID)},
			marshal.PyInt{Value: uint64(charID)},
			marshal.PyReal{Value: m},
			marshal.PyReal{Value: minAbs},
			marshal.PyReal{Value: maxAbs},
		}},
	}}}}
	if err := server.SendNotification(c, accountID, "OnStandingsModified", "charid", args); err != nil {
		log.Printf("standing push %d → char %d: %v", fromID, charID, err)
	}
}

// standingModification is the (m, minAbs, maxAbs) of an OnStandingsModified entry that lands
// the client's row on newStanding. The client applies m as a fraction of the gap and clamps to
// the bounds times ten, so a full step toward a bound set at the new value arrives exactly.
// ok is false when the standing did not move.
func standingModification(oldStanding, newStanding float64) (m, minAbs, maxAbs float64, ok bool) {
	switch {
	case newStanding > oldStanding:
		return 1, -1, newStanding / 10, true
	case newStanding < oldStanding:
		return -1, newStanding / 10, 1, true
	}
	return 0, 0, 0, false
}

// applyAgentStanding moves an NPC entity's standing toward a character. entityID
// is the side whose opinion changes (agent, its corporation, or its faction) and
// is the key the client indexes npccharstandings by; agentID only tags the
// transaction log.
func applyAgentStanding(ctx context.Context, c *evenet.Conn, accountID int64, sqldb *sql.DB,
	charID, agentID, entityID int64, delta float64, eventTypeID int64, missionName string) {

	// config.json rates.standingGain scales every mission-authored change.
	delta *= rates.StandingGain
	if entityID == 0 || delta == 0 {
		return
	}
	newStanding, err := evedb.UpsertNPCStanding(ctx, sqldb, charID, entityID, delta)
	if err != nil {
		log.Printf("standing: %d → char %d %+.3f: %v", entityID, charID, delta, err)
		return
	}
	evedb.InsertStandingTransaction(ctx, sqldb, eventTypeID,
		entityID, charID, delta, agentID, 0, 0, missionName)
	if c != nil {
		sendStandingNotification(c, accountID, entityID, charID, newStanding-delta, newStanding)
	}
}

// npcStandingDelta returns the standing loss for killing an NPC of the given ship
// group; larger hulls cost more. This is the per-kill hit every rat carries; a
// mission template's factionStandingLoss is applied once, on completion, on top.
func npcStandingDelta(groupID int64) float64 {
	switch groupID {
	case 25: // Frigate
		return -0.005
	case 26: // Destroyer
		return -0.01
	case 27: // Cruiser
		return -0.015
	case 28: // Battlecruiser
		return -0.02
	case 29: // Battleship
		return -0.025
	default:
		return -0.005
	}
}

// ── Agent offer notices ─────────────────────────────────────────────────────

// offerNoticeSweepTicks is the sweep cadence in one-second ticks. The roll is per
// character, so a tighter loop only makes the same mail arrive sooner.
const offerNoticeSweepTicks = 1800

// startOfferNoticeSweep wires the LP-store notice sweep: each cycle finds the
// offers each character could already take (LP with that agent and ISK in the
// wallet), rolls once per character, and has the agent write about one of them.
// Nothing else surfaces an offer to a pilot who has not opened the Offers tab.
func startOfferNoticeSweep(tickMgr *tick.Manager) {
	tickMgr.RegisterPeriodic("offer-notice-sweep", offerNoticeSweepTicks, func(uint64) bool {
		if globalDB == nil {
			return true
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		chance := evedb.GetOfferNoticeChancePct(ctx, globalDB)
		if chance <= 0 {
			return true
		}
		cooldown := time.Duration(evedb.GetOfferNoticeCooldownDays(ctx, globalDB)) * 24 * time.Hour
		cands, err := evedb.FindOfferNoticeCandidates(ctx, globalDB, cooldown)
		if err != nil {
			log.Printf("[offer-notice] FindOfferNoticeCandidates: %v", err)
			return true
		}
		if len(cands) == 0 {
			return true
		}

		// One roll and at most one letter per character: twenty affordable offers are
		// not twenty times the chance, and never twenty letters at once.
		byChar := map[int64][]evedb.OfferNoticeCandidate{}
		for _, c := range cands {
			byChar[c.CharID] = append(byChar[c.CharID], c)
		}
		sent := 0
		for charID, list := range byChar {
			if rand.Intn(100) >= chance {
				continue
			}
			// The standing gate lives in a different table; check it here so
			// a notice never advertises an offer the agent would refuse.
			eligible := list[:0:0]
			for _, c := range list {
				if c.MinStanding > 0 &&
					evedb.GetNPCStandingToChar(ctx, globalDB, charID, c.AgentID) < c.MinStanding {
					continue
				}
				eligible = append(eligible, c)
			}
			if len(eligible) == 0 {
				continue
			}
			pick := eligible[rand.Intn(len(eligible))]
			if sendOfferNoticeMail(ctx, globalDB, pick) {
				sent++
			}
		}
		if sent > 0 {
			log.Printf("[offer-notice] %d candidate offers across %d pilots → %d letters sent",
				len(cands), len(byChar), sent)
		}
		return true
	})
}

// commaGroups renders a whole number with thousands separators for mail bodies.
func commaGroups(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := ""
	if strings.HasPrefix(s, "-") {
		neg, s = "-", s[1:]
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return neg + b.String()
}

// sendOfferNoticeMail writes one agent's letter about an offer the character
// can already afford, and stamps the notice ledger so it does not come round
// again until the cooldown is up.
func sendOfferNoticeMail(ctx context.Context, sqldb *sql.DB, c evedb.OfferNoticeCandidate) bool {
	where := c.StationName
	if where == "" {
		where = "my station"
	}
	var price string
	switch {
	case c.LPCost > 0 && c.ISKCost > 0:
		price = fmt.Sprintf("%s loyalty points and %s ISK", commaGroups(int64(c.LPCost)), commaGroups(c.ISKCost))
	case c.LPCost > 0:
		price = fmt.Sprintf("%s loyalty points", commaGroups(int64(c.LPCost)))
	case c.ISKCost > 0:
		price = fmt.Sprintf("%s ISK", commaGroups(c.ISKCost))
	default:
		price = "nothing but your time"
	}
	body := fmt.Sprintf(
		"You have built up enough credit with me to be worth a letter.<br><br>"+
			"I can put <b>%s</b> your way for %s. You have that already &mdash; I checked "+
			"before writing.<br><br>"+
			"Come and see me at <b>%s</b>%s and ask about special offers. I will not hold it "+
			"open forever.<br><br>%s",
		c.OfferName, price, where, systemSuffix(c.SystemName), c.AgentName)
	subject := "Special offer: " + c.OfferName
	created := int64(timeToFileTime(time.Now()))

	msgID, err := evedb.SendEveMail(ctx, sqldb, c.CharID, c.AgentID, subject, body, created)
	if err != nil {
		log.Printf("[offer-notice] mail to charID=%d offer=%d: %v", c.CharID, c.OfferID, err)
		return false
	}
	if err := evedb.RecordOfferNotice(ctx, sqldb, c.CharID, c.OfferID); err != nil {
		log.Printf("[offer-notice] RecordOfferNotice charID=%d offer=%d: %v", c.CharID, c.OfferID, err)
	}
	pushMailNotification(c.CharID, msgID, c.AgentID, subject, created)
	log.Printf("[offer-notice] charID=%d ← agent %d about offer %d (%s)",
		c.CharID, c.AgentID, c.OfferID, c.OfferName)
	return true
}

// ── R&D patent lottery ──────────────────────────────────────────────────────

// patentDrawSweepTicks is how often due rounds are looked for, in one-second
// ticks: one indexed range scan over a small table, and "Draw now" stays prompt.
const patentDrawSweepTicks = 300

// startPatentDrawSweep wires the R&D patent allocation. Each cycle it lapses
// uncollected patents and closes every agent round whose clock has run out
// (db/rndlottery.go), then tells the people involved.
func startPatentDrawSweep(tickMgr *tick.Manager) {
	tickMgr.RegisterPeriodic("patent-draw-sweep", patentDrawSweepTicks, func(uint64) bool {
		if globalDB == nil {
			return true
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		if !evedb.PatentLotteryEnabled(ctx, globalDB) {
			return true
		}
		now := time.Now()
		if n, err := evedb.ExpirePatentOffers(ctx, globalDB, now.Unix()); err != nil {
			log.Printf("[patent] ExpirePatentOffers: %v", err)
		} else if n > 0 {
			log.Printf("[patent] %d uncollected patents lapsed", n)
		}

		due, err := evedb.DueAgentRounds(ctx, globalDB, now.Unix(), 200)
		if err != nil {
			log.Printf("[patent] DueAgentRounds: %v", err)
			return true
		}
		for _, agentID := range due {
			res, err := evedb.RunPatentDraw(ctx, globalDB, agentID, time.Now())
			if err != nil {
				log.Printf("[patent] draw agentID=%d: %v", agentID, err)
				continue
			}
			if res == nil {
				continue // not due after all, or another pass got there first
			}
			announcePatentDraw(ctx, globalDB, res)
		}
		return true
	})
}

// announcePatentDraw sends the winner's letter and a journal invalidate to every
// entrant, not only the winner: a draw zeroes the RP of everyone collaborating with
// that agent, and a docked loser would otherwise keep reading last round's number
// off a cached journal.
func announcePatentDraw(ctx context.Context, sqldb *sql.DB, res *evedb.PatentDrawResult) {
	if res == nil {
		return
	}
	if res.Outcome == evedb.PatentDrawWon {
		sendPatentWinMail(ctx, sqldb, res)
		log.Printf("[patent] agent %d round %d: %d entrants, %.1f tickets → charID=%d wins typeID=%d (bot=%v)",
			res.AgentID, res.RoundNo, res.Entrants, res.TotalTickets,
			res.WinnerCharID, res.BlueprintID, res.Synthetic)
	} else if res.Entrants > 0 {
		log.Printf("[patent] agent %d round %d: %d entrants but no tickets — no allocation",
			res.AgentID, res.RoundNo, res.Entrants)
	}
	for _, charID := range res.EntrantIDs {
		if globalLSCReg == nil {
			break
		}
		for _, m := range globalLSCReg.OnlineRecipients(charID) {
			if m.CharID != charID {
				continue // OnlineRecipients also matches corp mailboxes
			}
			// "research_" prefix: agents.py passes any `what` starting with it on to the
			// journal refetch, and no agent popup fires for something that is not a mission.
			notifyJournalInvalidate(m.Conn, m.AccountID, "research_patent")
		}
	}
}

// sendPatentWinMail writes the agent's letter to the winner. The sender is the
// agent, not CONCORD: the client renders a sender by owner lookup and the letter
// says where to go. A synthetic winner's mailbox is suppressed (SendEveMail answers
// messageID 0, nil error; pushMailNotification pushes nothing), so no bot branch.
func sendPatentWinMail(ctx context.Context, sqldb *sql.DB, res *evedb.PatentDrawResult) {
	agentName := evedb.GetAgentName(ctx, sqldb, res.AgentID)
	if agentName == "" {
		agentName = "your research contact"
	}
	bpName, _ := evedb.GetTypeName(ctx, sqldb, res.BlueprintID)
	if bpName == "" {
		bpName = "a technology patent"
	}
	fieldName, _ := evedb.GetTypeName(ctx, sqldb, res.SkillTypeID)

	where, systemName := "my station", ""
	if agent, err := evedb.GetAgentInfo(ctx, sqldb, res.AgentID); err == nil {
		if s := evedb.GetStationName(ctx, sqldb, agent.StationID); s != "" {
			where = s
		}
		systemName = evedb.GetSolarSystemName(ctx, sqldb, agent.SolarSystemID)
	}
	deadline := time.Unix(res.ExpiresAt, 0).UTC().Format("2006-01-02 15:04")

	body := fmt.Sprintf(
		"The patent allocation for our %s research programme has been decided, and it fell "+
			"to you.<br><br>"+
			"The original blueprint — <b>%s</b> — is being held in my name at <b>%s</b>%s. "+
			"Come and see me and I will sign it over; there is nothing further to pay.<br><br>"+
			"I can only hold it until <b>%s</b>. After that it goes back into the programme.<br><br>"+
			"Your accumulated research points went into this allocation and are spent. The next "+
			"round is already running.<br><br>%s",
		fieldName, bpName, where, systemSuffix(systemName), deadline, agentName)
	subject := "Patent allocation: " + bpName
	created := int64(timeToFileTime(time.Now()))

	msgID, err := evedb.SendEveMail(ctx, sqldb, res.WinnerCharID, res.AgentID, subject, body, created)
	if err != nil {
		log.Printf("[patent] win mail to charID=%d: %v", res.WinnerCharID, err)
		return
	}
	pushMailNotification(res.WinnerCharID, msgID, res.AgentID, subject, created)
}
