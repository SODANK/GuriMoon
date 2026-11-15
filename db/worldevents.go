package db

// World events: a read-only feed of things that happened in the world, for modules that observe
// play without patching the paths that produce it (GMMordu's Network is the first reader). It is
// a bus because the producers live in files an overlay module may not patch and readers are
// optional.
//
// Delivery is asynchronous on one queue goroutine. The queue is bounded; an event that does not
// fit is dropped and counted (WorldEventsDropped) and never blocks the emitter, so subscribers
// see emission order but must tolerate gaps. With no subscriber an emit is one atomic load and
// the goroutine never starts. See docs/db-11-notes.md#world-event-bus.

import (
	"log"
	"sync"
	"sync/atomic"
	"time"

	"gurimoon/safego"
)

// WorldEvent is one of the *Event types below. Subscribers type-switch on it.
type WorldEvent interface{ worldEvent() }

// NPCKilledEvent: an NPC (rat, structure, sentry) was destroyed by a pilot. The killer is the
// pilot behind the kill (a drone kill names the controller). Bounty is what was paid (0 when
// none).
type NPCKilledEvent struct {
	At               time.Time
	KillerCharID     int64
	KillerCorpID     int64
	KillerAllianceID int64
	NPCTypeID        int64
	NPCGroupID       int64
	NPCFactionID     int64 // 0 when the NPC carries no faction
	SolarSystemID    int64
	Bounty           float64
	OpenSiteID       int64 // dunOpenSites row the NPC belonged to, 0 if none
	MissionID        int64
	Structure        bool
	Abstract         bool // emitted by a module's simulated tier, not live space
}

// OreMinedEvent: ore, ice or other mined material landed in a pilot's hold.
type OreMinedEvent struct {
	At            time.Time
	CharID        int64
	CorpID        int64
	TypeID        int64
	GroupID       int64
	Units         int64
	Volume        float64 // m³ of Units
	SolarSystemID int64
	Drone         bool
	Abstract      bool
}

// MarketSellFillEvent: a seller's sell order filled against a resting buy order. One event per
// fill. BuyerOwnerID is the order's owner: the NPC corporation for a seeded bid, the corporation
// for a corp order, else the character.
type MarketSellFillEvent struct {
	At               time.Time
	SellerID         int64
	BuyerOwnerID     int64
	BuyerIsNPCSeeder bool
	TypeID           int64
	Quantity         int64
	Price            float64
	StationID        int64
	SolarSystemID    int64
	RegionID         int64
	Abstract         bool
}

// KillmailRecordedEvent: a capsuleer (or synthetic pilot) loss was committed to
// the killboard. Rec is the record as filed.
type KillmailRecordedEvent struct {
	KillID     int64
	Rec        KillmailRecord
	TotalValue float64 // ship, dropped and destroyed items at the killboard's prices
}

// CorpCEOChangedEvent: a corporation's CEO seat changed hands.
type CorpCEOChangedEvent struct {
	At                 time.Time
	CorpID             int64
	OldCEOID, NewCEOID int64
}

// WarEvent: a war was declared (Ended false) or finished. The parties are corporations or
// alliances.
type WarEvent struct {
	At                      time.Time
	DeclaredByID, AgainstID int64
	Ended                   bool
}

// CorpDividendPaidEvent: a corporation paid a dividend to its shareholders.
type CorpDividendPaidEvent struct {
	At     time.Time
	CorpID int64
	Total  float64
}

// OfficeEvictedEvent: a corporation lost an office for unpaid rent.
type OfficeEvictedEvent struct {
	At        time.Time
	CorpID    int64
	StationID int64
}

// OutpostCompletedEvent: an outpost platform finished building into a station.
type OutpostCompletedEvent struct {
	At          time.Time
	CorpID      int64
	StationID   int64
	StationName string
}

// StationConqueredEvent: a conquerable station changed hands, its shield taken to 0
// (conquerStation in cmd/gurimoon). AttackerChar is 0 for a capture with no pilot to name.
type StationConqueredEvent struct {
	At           time.Time
	StationID    int64
	SystemID     int64
	OldCorp      int64
	NewCorp      int64
	AttackerChar int64
}

// StationUnderAttackEvent: a conquerable station ball took a hit, at most once a minute per station
// (noteStationHit in cmd/gurimoon). ShieldFrac is the shield left, 0 to 1.
type StationUnderAttackEvent struct {
	At           time.Time
	StationID    int64
	SystemID     int64
	OwnerCorp    int64
	AttackerCorp int64
	AttackerChar int64
	ShieldFrac   float64
}

// OpenSiteClearedEvent: every rat of a dungeon spawner's open site is dead.
// Who killed them rides the NPCKilled events carrying OpenSiteID.
type OpenSiteClearedEvent struct {
	At               time.Time
	SiteID           int64
	DungeonVID       int64
	DungeonFactionID int64
	DEDLevel         int
	SolarSystemID    int64
}

// IndustryDeliveredEvent: an S&I job's output was delivered to its installer.
type IndustryDeliveredEvent struct {
	At            time.Time
	CharID        int64 // installer
	CorpID        int64 // the corporation for a corporation job, else 0
	ActivityID    int
	ProductTypeID int64
	Units         int64
	StationID     int64 // the container the job ran in
	SolarSystemID int64
	Abstract      bool
}

// StargatePairChangedEvent: an admin built or removed a custom stargate pair, so the jump
// graph changed. A module that caches routes rebuilds them (docs/stargates.md).
type StargatePairChangedEvent struct {
	At               time.Time
	GateA, GateB     int64
	SystemA, SystemB int64
	Removed          bool
}

func (StargatePairChangedEvent) worldEvent() {}

func (NPCKilledEvent) worldEvent()          {}
func (OreMinedEvent) worldEvent()           {}
func (MarketSellFillEvent) worldEvent()     {}
func (KillmailRecordedEvent) worldEvent()   {}
func (OpenSiteClearedEvent) worldEvent()    {}
func (IndustryDeliveredEvent) worldEvent()  {}
func (CorpCEOChangedEvent) worldEvent()     {}
func (WarEvent) worldEvent()                {}
func (CorpDividendPaidEvent) worldEvent()   {}
func (OfficeEvictedEvent) worldEvent()      {}
func (OutpostCompletedEvent) worldEvent()   {}
func (StationConqueredEvent) worldEvent()   {}
func (StationUnderAttackEvent) worldEvent() {}

// worldEventQueueSize bounds the backlog, sized to ride out a fleet fight's death burst. A
// reader this far behind is broken; dropping beats stalling the game.
const worldEventQueueSize = 8192

var (
	worldSubsMu   sync.RWMutex
	worldSubs     []func(WorldEvent)
	worldSubCount atomic.Int32
	worldDropped  atomic.Int64
	worldQueue    chan WorldEvent
	worldStart    sync.Once
)

// SubscribeWorldEvents registers fn for every world event. fn runs on the
// shared queue goroutine: it must return quickly and hand anything slow (a DB
// transaction per event is fine; a network call is not) to its own worker.
// A panic is logged and the event skipped for that subscriber only.
func SubscribeWorldEvents(fn func(WorldEvent)) {
	if fn == nil {
		return
	}
	worldStart.Do(func() {
		worldQueue = make(chan WorldEvent, worldEventQueueSize)
		safego.Forever("db.runWorldEvents", func() { runWorldEvents(worldQueue) })
	})
	worldSubsMu.Lock()
	worldSubs = append(worldSubs, fn)
	worldSubsMu.Unlock()
	worldSubCount.Add(1)
}

// WorldEventsDropped is how many events the bounded queue has refused since
// boot. Non-zero means a subscriber is too slow.
func WorldEventsDropped() int64 { return worldDropped.Load() }

// EmitWorldEvent queues ev for every subscriber. Never blocks.
func EmitWorldEvent(ev WorldEvent) {
	if ev == nil || worldSubCount.Load() == 0 {
		return
	}
	select {
	case worldQueue <- ev:
	default:
		if worldDropped.Add(1)%1000 == 1 {
			log.Printf("[worldevents] queue full, dropping events (%d dropped so far)", worldDropped.Load())
		}
	}
}

// WorldEventsWanted reports whether anyone listens. An emitter whose event is
// costly to build (a DB lookup for a region or faction) checks this first.
func WorldEventsWanted() bool { return worldSubCount.Load() > 0 }

func runWorldEvents(q <-chan WorldEvent) {
	for ev := range q {
		worldSubsMu.RLock()
		subs := worldSubs
		worldSubsMu.RUnlock()
		for _, fn := range subs {
			deliverWorldEvent(fn, ev)
		}
	}
}

func deliverWorldEvent(fn func(WorldEvent), ev WorldEvent) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[worldevents] subscriber panicked on %T: %v", ev, r)
		}
	}()
	fn(ev)
}
