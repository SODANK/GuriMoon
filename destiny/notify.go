package destiny

import (
	"gurimoon/dogma"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"log"
	"math"
)

// ModuleRef is a fitted module reference sent in slimItem.modules so the
// client can attach turret models to ship hardpoints in space.
type ModuleRef struct {
	ItemID int64
	TypeID int64
}

// SlimItem describes a slim entity visible to the client in a solar system.
type SlimItem struct {
	BallID     int64 // destiny ball ID; must equal ItemID for tracked entities
	ItemID     int64
	TypeID     int64
	GroupID    int64
	CategoryID int64
	OwnerID    int64
	CorpID     int64
	AllianceID    int64
	WarFactionID  int64 // faction ID for NPC faction ships (e.g. Angel Cartel)
	CharID        int64 // non-zero for player ships
	SecurityStatus float64 // character security status (-10 to +5)
	Bounty         float64 // ISK bounty on the character's head
	// Stargate jump destination (non-zero for groupID==10 stargates only).
	JumpsToStargate    int64
	JumpsToSolarSystem int64

	// Fitted modules (high-slot weapons/turrets). The client iterates this
	// list in FitHardpoints() to attach turret 3D models to hardpoint locators.
	Modules []ModuleRef

	// Player-Owned Structure fields (category 23 only). PosState is a pointer so
	// non-structure slims omit the keys; the client's SlimItem.__getattr__ turns a
	// missing key into None. posState (parklife/playerOwned.py): 0 unanchored,
	// 1 anchored, 2 onlining, 3 reinforced, 4 online, 5 operating. posTimestamp is
	// Win32 FILETIME: for 0/1/2 the past time the transition started (0 = None);
	// for 3/5 it must be a future end time or GetStructureState returns None.
	PosState       *int64
	PosTimestamp   uint64
	ControlTowerID int64 // slave structures: itemID of the owning tower (0 = omit; needed for onlining FX)

	// Dungeon editor fields (zero DunObjectID = not a dungeon object).
	DunObjectID  int64
	DunRoomID    int64
	DunX, DunY, DunZ float64
	DunYaw       *float64 // nil = no rotation
	DunPitch     *float64
	DunRoll      *float64
	DunRadius    *float64 // nil = no custom radius
	DunAggrRange *float64 // nil = no aggression range

	// Acceleration-gate attributes (groupID 366 only). DunDirection is the unit
	// vector the model points along (spaceObject.py SetStaticDirection).
	// accelerationGate.py reads slimItem.dunMusicUrl unconditionally, so the key
	// is always emitted for gates, PyNone when empty.
	DunDirection *[3]float64
	DunMusicURL  string

	// Free-text description the client shows as the Show Info description and
	// bookmark hint (menusvc.py, infosvc.py), authored per dungeon object. Empty
	// is omitted.
	DunDescription string
}

// GroupWarpGate / GroupBeacon are the invGroups IDs for acceleration gates
// and dungeon warp-in beacons (verified against the RMR data dump).
const (
	GroupWarpGate = 366
	GroupBeacon   = 310
)

// DroneStateRow is one row of the droneState rowset in SetState, mirrored by
// OnDroneStateChange; michelle indexes rows by droneID for the DroneView
// window. The header is exactly these 6 columns, no solarSystemID: util.Row
// reads positionally and the client's OnDroneStateChange inserts 6-element
// lines, so an extra column shifts every read.
type DroneStateRow struct {
	DroneID           int64
	OwnerID           int64
	ControllerID      int64
	ActivityState     int64
	TypeID            int64
	ControllerOwnerID int64
}

// encodeDroneStateRowset builds the util.Rowset for the SetState droneState key.
func encodeDroneStateRowset(rows []DroneStateRow) marshal.PyRep {
	lines := make([]marshal.PyRep, len(rows))
	for i, r := range rows {
		lines[i] = marshal.PyList{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(r.DroneID)},
			marshal.PyInt{Value: uint64(r.OwnerID)},
			marshal.PyInt{Value: uint64(r.ControllerID)},
			marshal.PyInt{Value: uint64(r.ActivityState)},
			marshal.PyInt{Value: uint64(r.TypeID)},
			marshal.PyInt{Value: uint64(r.ControllerOwnerID)},
		}}
	}
	return marshal.PyObject{
		Type: "util.Rowset",
		Args: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "header"}, Value: marshal.PyList{Items: []marshal.PyRep{
				marshal.PyString{Value: "droneID"},
				marshal.PyString{Value: "ownerID"},
				marshal.PyString{Value: "controllerID"},
				marshal.PyString{Value: "activityState"},
				marshal.PyString{Value: "typeID"},
				marshal.PyString{Value: "controllerOwnerID"},
			}}},
			{Key: marshal.PyString{Value: "RowClass"}, Value: marshal.PyString{Value: "util.Row", IsType1: true}},
			{Key: marshal.PyString{Value: "lines"}, Value: marshal.PyList{Items: lines}},
		}},
	}
}

// BuildDroneStateAction builds an OnDroneStateChange destiny action. The
// client's michelle.OnDroneStateChange updates its stateByDroneID index and
// deletes a row whose ownerID and controllerID both mismatch the session.
func BuildDroneStateAction(droneID, ownerID, controllerID int64, activityState marshal.PyRep, typeID, controllerOwnerID int64) marshal.PyRep {
	return BuildSimpleAction("OnDroneStateChange",
		marshal.PyInt{Value: uint64(droneID)},
		marshal.PyInt{Value: uint64(ownerID)},
		marshal.PyInt{Value: uint64(controllerID)},
		activityState,
		marshal.PyInt{Value: uint64(typeID)},
		marshal.PyInt{Value: uint64(controllerOwnerID)},
	)
}

// DamageState holds the shield/armor/structure fractions for one entity. Tau
// is in client dogma units (milliseconds / 5); the client extrapolates shield
// regen from it. The client ignores Timestamp and stamps with its own clock.
type DamageState struct {
	Shield    float64
	Tau       float64
	Timestamp uint64
	Armor     float64
	Structure float64
}

// SendSetState sends a SetState destiny notification. binaryState is the
// output of EncodeSetState, slims the entities visible in the bubble, solItem
// a util.Row for the solar system (marshal.PyNone{} as fallback) and
// activeEffects the running module effects in the bubble (nil for none).
func SendSetState(
	c *evenet.Conn,
	nodeID, accountID uint64,
	ego, stamp uint32,
	binaryState []byte,
	slims []SlimItem,
	dmgStates map[uint32]DamageState,
	solItem marshal.PyRep,
	activeEffects []ActiveEffect,
	aggressors map[uint32]int64,
	charAggressors map[int64]map[int64]int64,
	gangCorps, charGangs marshal.PyRep,
	droneRows []DroneStateRow,
) error {
	slimReps := make([]marshal.PyRep, len(slims))
	for i, s := range slims {
		slimReps[i] = encodeSlimItem(s)
	}

	// damageState: {entityID: ((shield, tau, ts), armor, structure), ...}
	dmgEntries := make([]marshal.DictEntry, 0, len(dmgStates))
	for id, ds := range dmgStates {
		dmgEntries = append(dmgEntries, marshal.DictEntry{
			Key:   marshal.PyInt{Value: uint64(id)},
			Value: encodeDamageState(ds),
		})
	}
	dmgMap := marshal.PyDict{Items: dmgEntries}

	// aggressors: {aggressorID: {victimID: FILETIME}}. michelle.GetAggressionState
	// looks up aggressors[charID][session.charid|session.corpid|None].
	aggrDict := marshal.PyDict{}
	if len(aggressors) > 0 || len(charAggressors) > 0 {
		entries := make([]marshal.DictEntry, 0, len(aggressors)+len(charAggressors))
		for entityID, ts := range aggressors {
			// Convert Unix seconds → Win32 FILETIME.
			winTS := uint64(ts+11644473600) * 10000000
			// Value is a dict {None: timestamp} for global criminal flag.
			entries = append(entries, marshal.DictEntry{
				Key: marshal.PyInt{Value: uint64(entityID)},
				Value: marshal.PyDict{Items: []marshal.DictEntry{
					{Key: marshal.PyNone{}, Value: marshal.PyInt{Value: winTS}},
				}},
			})
		}
		// Player-vs-player crimewatch entries, keyed by aggressor charID.
		// Victim key 0 encodes as None (the global criminal flag).
		entries = append(entries, EncodeCharAggressors(charAggressors)...)
		aggrDict = marshal.PyDict{Items: entries}
	}

	// Default gang data to empty lists if nil.
	if gangCorps == nil {
		gangCorps = marshal.PyList{}
	}
	if charGangs == nil {
		charGangs = marshal.PyList{}
	}

	keyval := marshal.PyObject{
		Type: "util.KeyVal",
		Args: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "stamp"}, Value: marshal.PyInt{Value: uint64(stamp)}},
			{Key: marshal.PyString{Value: "gangCorps"}, Value: gangCorps},
			{Key: marshal.PyString{Value: "aggressors"}, Value: aggrDict},
			{Key: marshal.PyString{Value: "state"}, Value: marshal.PyBuffer{Data: binaryState}},
			{Key: marshal.PyString{Value: "ego"}, Value: marshal.PyInt{Value: uint64(ego)}},
			{Key: marshal.PyString{Value: "damageState"}, Value: dmgMap},
			{Key: marshal.PyString{Value: "droneState"}, Value: encodeDroneStateRowset(droneRows)},
			{Key: marshal.PyString{Value: "slims"}, Value: marshal.PyList{Items: slimReps}},
			{Key: marshal.PyString{Value: "solItem"}, Value: solItem},
			{Key: marshal.PyString{Value: "effectStates"}, Value: encodeEffectStates(activeEffects)},
			{Key: marshal.PyString{Value: "charGangs"}, Value: charGangs},
		}},
	}

	action := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyString{Value: "SetState"},
		marshal.PyTuple{Items: []marshal.PyRep{keyval}},
	}}

	// effectStates restores the beams and turret fire of a grid seen for the first
	// time; the same list restores the jam bars those effects imply.
	return sendDestinyUpdatesEvents(c, nodeID, accountID, stamp, action, false,
		JamEventsForEffects(activeEffects))
}


// BuildAddBallsAction constructs an AddBalls action tuple (with slims and
// damage states) without sending it. Use this when you need to bundle the
// AddBalls with other actions in a SendActionList call.
func BuildAddBallsAction(stamp uint32, balls []BallDesc, slims []SlimItem, dmgStates map[uint32]DamageState) marshal.PyTuple {
	blob := EncodeAddBalls(stamp, balls)

	slimReps := make([]marshal.PyRep, len(slims))
	for i, s := range slims {
		slimReps[i] = encodeSlimItem(s)
	}

	dmgEntries := make([]marshal.DictEntry, 0, len(dmgStates))
	for id, ds := range dmgStates {
		dmgEntries = append(dmgEntries, marshal.DictEntry{
			Key:   marshal.PyInt{Value: uint64(id)},
			Value: encodeDamageState(ds),
		})
	}

	return marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyString{Value: "AddBalls"},
		marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyBuffer{Data: blob},
				marshal.PyList{Items: slimReps},
				marshal.PyDict{Items: dmgEntries},
			}},
		}},
	}}
}

// SendAddBallsFull sends an AddBalls destiny action that includes the full
// (state, slims, damageDict) triple the client expects for new entities.
// This is used when a new player appears in the bubble.
func SendAddBallsFull(
	c *evenet.Conn,
	nodeID, accountID uint64,
	stamp uint32,
	balls []BallDesc,
	slims []SlimItem,
	dmgStates map[uint32]DamageState,
) error {
	action := BuildAddBallsAction(stamp, balls, slims, dmgStates)
	return sendDestinyUpdates(c, nodeID, accountID, stamp, action, false)
}

// SendDamageStateChange sends an OnDamageStateChange destiny action that
// updates the client's view of an entity's shield/armor/structure. Used by
// the combat layer after applying damage from a weapon hit.
func SendDamageStateChange(
	c *evenet.Conn,
	nodeID, accountID uint64,
	stamp uint32,
	entityID uint32,
	d DamageState,
) error {
	return SendSimpleAction(c, nodeID, accountID, stamp,
		"OnDamageStateChange",
		marshal.PyInt{Value: uint64(entityID)},
		encodeDamageState(d),
	)
}

// BuildRemoveBallsAction forms the RemoveBalls action tuple for a set of
// balls. Each stamped packet costs the receiving client a simulation second,
// so a caller tearing down several balls builds one action for all of them.
// Exported so a caller can hand it to Entity.QueueAction.
func BuildRemoveBallsAction(ballIDs []uint32) marshal.PyRep {
	ids := make([]marshal.PyRep, len(ballIDs))
	for i, id := range ballIDs {
		ids[i] = marshal.PyInt{Value: uint64(id)}
	}
	return marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyString{Value: "RemoveBalls"},
		marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyList{Items: ids},
		}},
	}}
}

// SendRemoveBalls sends a RemoveBalls destiny action to remove the specified
// entity IDs from the client's ballpark.
func SendRemoveBalls(
	c *evenet.Conn,
	nodeID, accountID uint64,
	stamp uint32,
	ballIDs []uint32,
) error {
	return sendDestinyUpdates(c, nodeID, accountID, stamp, BuildRemoveBallsAction(ballIDs), false)
}

// SendSimpleAction sends a single destiny action (GotoDirection, Stop,
// SetSpeedFraction, SetBallPosition...). args are the action tuple's arguments.
func SendSimpleAction(
	c *evenet.Conn,
	nodeID, accountID uint64,
	stamp uint32,
	funcName string,
	args ...marshal.PyRep,
) error {
	action := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyString{Value: funcName},
		marshal.PyTuple{Items: args},
	}}
	return sendDestinyUpdates(c, nodeID, accountID, stamp, action, false)
}

// BuildDamageStateChangeAction forms the OnDamageStateChange action tuple.
// OnDamageStateChange is in the client's RealFlushState set (stamp-agnostic)
// and carries absolute state, so it is the canonical Entity.QueueFXAction
// payload: last-wins per ball collapses a volley into one entry.
func BuildDamageStateChangeAction(entityID uint32, d DamageState) marshal.PyRep {
	return BuildSimpleAction("OnDamageStateChange",
		marshal.PyInt{Value: uint64(entityID)},
		encodeDamageState(d),
	)
}

// EncodeCharAggressors converts a crimewatch aggression table
// (aggressorCharID → victimKey → Unix seconds; victimKey 0 = global criminal)
// into the dict entries michelle.py expects: {charID: {victimID|None: FILETIME}}.
// Shared by the SetState bag and the OnAggressionChange notification.
func EncodeCharAggressors(charAggressors map[int64]map[int64]int64) []marshal.DictEntry {
	entries := make([]marshal.DictEntry, 0, len(charAggressors))
	for aggrCharID, victims := range charAggressors {
		vicEntries := make([]marshal.DictEntry, 0, len(victims))
		for victimKey, ts := range victims {
			winTS := uint64(ts+11644473600) * 10000000
			var key marshal.PyRep = marshal.PyInt{Value: uint64(victimKey)}
			if victimKey == 0 {
				key = marshal.PyNone{}
			}
			vicEntries = append(vicEntries, marshal.DictEntry{
				Key: key, Value: marshal.PyInt{Value: winTS},
			})
		}
		entries = append(entries, marshal.DictEntry{
			Key:   marshal.PyInt{Value: uint64(aggrCharID)},
			Value: marshal.PyDict{Items: vicEntries},
		})
	}
	return entries
}

// SendSlimItemChange sends an OnSlimItemChange destiny action when an entity's
// SlimItem changed (modules fitted in space). The client handler is
// michelle.OnSlimItemChange(itemID, newSlimDict).
func SendSlimItemChange(
	c *evenet.Conn,
	nodeID, accountID uint64,
	stamp uint32,
	slim SlimItem,
) error {
	return SendSimpleAction(c, nodeID, accountID, stamp,
		"OnSlimItemChange",
		marshal.PyInt{Value: uint64(slim.ItemID)},
		encodeSlimItem(slim),
	)
}

// SendSetMaxSpeed sends a SetMaxSpeed destiny action, changing a ball's
// maximum velocity live in the client's C++ ballpark (verified present in the
// Blood 3913 destiny.dll export strings). Used for stasis webs and prop mods.
// Like all native ballpark ops it requires a stamp-synced action.
func SendSetMaxSpeed(c *evenet.Conn, nodeID, accountID uint64, stamp, entityID uint32, speed float64) error {
	return SendSimpleAction(c, nodeID, accountID, stamp,
		"SetMaxSpeed",
		marshal.PyInt{Value: uint64(entityID)},
		marshal.PyReal{Value: speed})
}

// SendSetBallRadius sends a SetBallRadius destiny action, resizing a ball live
// (a mined asteroid shrinking). michelle routes it through the native ballpark
// op and spaceMgr rescales the model. It sits behind FindState(eventStamp), so
// it must carry a freshly minted stamp (SendStamped, never SendFXStamped).
func SendSetBallRadius(c *evenet.Conn, nodeID, accountID uint64, stamp, entityID uint32, radius float64) error {
	return SendSimpleAction(c, nodeID, accountID, stamp,
		"SetBallRadius",
		marshal.PyInt{Value: uint64(entityID)},
		marshal.PyReal{Value: radius})
}

// SendGotoDirection sends a GotoDirection destiny action. destiny.dll stores
// (x,y,z) as an absolute goto target, not a direction: pass a far absolute
// target along the heading (shipPos + dir*1e15), as the NPC, drone and player
// goto paths do.
func SendGotoDirection(c *evenet.Conn, nodeID, accountID uint64, stamp, entityID uint32, x, y, z float64) error {
	err := SendSimpleAction(c, nodeID, accountID, stamp,
		"GotoDirection",
		marshal.PyInt{Value: uint64(entityID)},
		marshal.PyReal{Value: x}, marshal.PyReal{Value: y}, marshal.PyReal{Value: z})
	if err != nil {
		log.Printf("destiny.SendGotoDirection: FAILED: %v", err)
	}
	return err
}

// SendWarpTo sends the warp as one batched update: SetSpeedFraction 1, WarpTo
// (dest, exitRange, speed), OnSpecialFX(effects.Warping). dest is the landing
// point with exitRangeM 0, or the celestial with a real range; warpSpeedAU is
// converted to the dll's 0.1 AU/s units. See docs/destiny-space.md#warpto-arguments.
func SendWarpTo(
	c *evenet.Conn,
	nodeID, accountID uint64,
	stamp uint32,
	entityID uint32,
	destX, destY, destZ float64,
	exitRangeM float64,
	warpSpeedAU float64,
) error {
	actions := BuildWarpToActions(entityID, destX, destY, destZ, exitRangeM, warpSpeedAU)
	stamped := make([]marshal.PyRep, len(actions))
	for i, a := range actions {
		stamped[i] = marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(stamp)}, a,
		}}
	}
	return sendActionList(c, nodeID, accountID, stamped, false)
}

// BuildWarpToActions returns the three actions SendWarpTo sends, so a caller
// can put them on Entity's coalescing queue instead of minting a stamp. Queue
// each under its own key: the queue coalesces by key, and three under one key
// collapse to the last. See docs/destiny-space.md#warpto-arguments.
func BuildWarpToActions(
	entityID uint32,
	destX, destY, destZ float64,
	exitRangeM float64,
	warpSpeedAU float64,
) []marshal.PyRep {
	ssfAction := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyString{Value: "SetSpeedFraction"},
		marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(entityID)},
			marshal.PyReal{Value: 1.0},
		}},
	}}
	speedUnits := int64(math.Round(warpSpeedAU * 10)) // dll units: 0.1 AU/s
	if speedUnits < 1 {
		speedUnits = 1
	}
	warpAction := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyString{Value: "WarpTo"},
		marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(entityID)},
			marshal.PyReal{Value: destX},
			marshal.PyReal{Value: destY},
			marshal.PyReal{Value: destZ},
			marshal.PyReal{Value: exitRangeM},          // metres pulled back toward ship
			marshal.PyInt{Value: uint64(speedUnits)},   // warp speed, 0.1 AU/s units
		}},
	}}
	fxAction := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyString{Value: "OnSpecialFX"},
		marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(entityID)},
			marshal.PyNone{}, // moduleID
			marshal.PyNone{}, // moduleTypeID
			marshal.PyNone{}, // targetID
			marshal.PyNone{}, // otherTypeID
			marshal.PyList{}, // area
			marshal.PyString{Value: "effects.Warping"},
			marshal.PyInt{Value: 0}, // isOffensive
			marshal.PyInt{Value: 1}, // start=1
			marshal.PyInt{Value: 0}, // active=0
		}},
	}}
	return []marshal.PyRep{ssfAction, warpAction, fxAction}
}

// SendJumpOut sends a batched Stop + OnSpecialFX(effects.JumpOut) destiny
// notification, matching C++ DestinyManager::SendJumpOut. usingGateID is the
// ID of the gate the ship is entering (used by the client for the exit flash).
func SendJumpOut(
	c *evenet.Conn,
	nodeID, accountID uint64,
	stamp, entityID, usingGateID uint32,
) error {
	stopAction := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyString{Value: "Stop"},
		marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(entityID)},
		}},
	}}
	fxAction := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyString{Value: "OnSpecialFX"},
		marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(entityID)},
			marshal.PyNone{},
			marshal.PyNone{},
			marshal.PyInt{Value: uint64(usingGateID)},
			marshal.PyNone{},
			marshal.PyList{},
			marshal.PyString{Value: "effects.JumpOut"},
			marshal.PyInt{Value: 0},
			marshal.PyInt{Value: 1},
			marshal.PyInt{Value: 0},
		}},
	}}
	return sendActionList(c, nodeID, accountID, []marshal.PyRep{
		marshal.PyTuple{Items: []marshal.PyRep{marshal.PyInt{Value: uint64(stamp)}, stopAction}},
		marshal.PyTuple{Items: []marshal.PyRep{marshal.PyInt{Value: uint64(stamp)}, fxAction}},
	}, false)
}

// SendWarpEnd sends SetBallPosition only, after the ball's natural warp exit
// (WarpPhases models the dll's align gate so the server timeline stays behind
// the client's). No Stop and no OnSpecialFX stop: both end the dll's warp
// mid-decel. See docs/destiny-space.md#warp-landing.
func SendWarpEnd(
	c *evenet.Conn,
	nodeID, accountID uint64,
	stamp, entityID uint32,
	landingX, landingY, landingZ float64,
) error {
	setBallPosAction := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyString{Value: "SetBallPosition"},
		marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(entityID)},
			marshal.PyReal{Value: landingX},
			marshal.PyReal{Value: landingY},
			marshal.PyReal{Value: landingZ},
		}},
	}}
	return sendActionList(c, nodeID, accountID, []marshal.PyRep{
		marshal.PyTuple{Items: []marshal.PyRep{marshal.PyInt{Value: uint64(stamp)}, setBallPosAction}},
	}, false)
}

// SendWarpLandingBatch sends the arrival bubble's AddBalls and, when snapPos,
// a SetBallPosition for the landing ball at one stamp, so the client applies
// both together. snapPos is false after a real warp and true only for the
// sub-1e10 m goto-fallback hop; see docs/destiny-space.md#warp-landing.
func SendWarpLandingBatch(
	c *evenet.Conn,
	nodeID, accountID uint64,
	stamp, entityID uint32,
	x, y, z float64,
	snapPos bool,
	balls []BallDesc,
	slims []SlimItem,
	dmgStates map[uint32]DamageState,
) error {
	var pairs []marshal.PyRep

	if snapPos {
		setBallPosAction := marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyString{Value: "SetBallPosition"},
			marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyInt{Value: uint64(entityID)},
				marshal.PyReal{Value: x},
				marshal.PyReal{Value: y},
				marshal.PyReal{Value: z},
			}},
		}}
		pairs = append(pairs, marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(stamp)}, setBallPosAction,
		}})
	}

	if len(balls) > 0 {
		addBallsAction := BuildAddBallsAction(stamp, balls, slims, dmgStates)
		pairs = append(pairs, marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(stamp)}, addBallsAction,
		}})
	}

	if len(pairs) == 0 {
		return nil
	}

	return sendActionList(c, nodeID, accountID, pairs, false)
}

// sendDestinyUpdates wraps a single destiny action in the full Notification
// wire format and sends it to the client.
func sendDestinyUpdates(
	c *evenet.Conn,
	nodeID, accountID uint64,
	stamp uint32,
	action marshal.PyRep,
	waitForBubble bool,
) error {
	return sendDestinyUpdatesEvents(c, nodeID, accountID, stamp, action, waitForBubble, nil)
}

// sendDestinyUpdatesEvents is sendDestinyUpdates with the dogma-event list
// populated. See the dogma-event block above BuildJamEvent.
func sendDestinyUpdatesEvents(
	c *evenet.Conn,
	nodeID, accountID uint64,
	stamp uint32,
	action marshal.PyRep,
	waitForBubble bool,
	events []marshal.PyRep,
) error {
	// DoDestinyAction = PyTuple[2]: (stamp, action_tuple)
	destinyAction := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyInt{Value: uint64(stamp)},
		action,
	}}

	// DoDestinyUpdateMain = PyTuple[3]: ([actions], waitForBubble, [events])
	updateMain := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyList{Items: []marshal.PyRep{destinyAction}},
		marshal.PyBool{Value: waitForBubble},
		marshal.PyList{Items: events},
	}}

	// Inner substream: Tuple{0, Tuple{remoteObject=1, DoDestinyUpdateMain}}; the
	// client calls DoDestinyUpdate(*args).
	innerStream := &marshal.PySubStream{
		Decoded: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: 0},
			marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyInt{Value: 1},
				updateMain, // = (updates_list, False, events_list)
			}},
		}},
	}

	// Outer payload: PyTuple[2]{ PyTuple[2]{0, substream}, PyNone }
	payload := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: 0},
			innerStream,
		}},
		marshal.PyNone{},
	}}

	// Extract action name for logging
	actionName := "unknown"
	if tuple, ok := action.(marshal.PyTuple); ok && len(tuple.Items) > 0 {
		if str, ok := tuple.Items[0].(marshal.PyString); ok {
			actionName = str.Value
		}
	}

	pkt := &evenet.PyPacket{
		TypeString: "macho.Notification",
		Type:       evenet.Notification,
		Source: evenet.PyAddress{
			Type:   evenet.AddrNode,
			TypeID: nodeID,
		},
		Dest: evenet.PyAddress{
			Type:        evenet.AddrBroadcast,
			Service:     "DoDestinyUpdate",
			BcastIDType: "clientID",
		},
		UserID:  accountID,
		Payload: payload,
	}

	b, err := marshal.Marshal(pkt.Encode())
	if err != nil {
		log.Printf("destiny.sendDestinyUpdates: marshal FAILED action=%s: %v", actionName, err)
		return err
	}

	err = c.SendPacket(b)
	if err != nil {
		log.Printf("destiny.sendDestinyUpdates: SendPacket FAILED action=%s: %v", actionName, err)
	}
	return err
}

// SendActionList wraps pre-formed action tuples in the full destiny
// Notification wire format and delivers them as a single batched packet.
// Each action is a marshal.PyTuple{funcName, PyTuple{args...}}.
// All actions share the same stamp and are delivered atomically.
func SendActionList(
	c *evenet.Conn,
	nodeID, accountID uint64,
	stamp uint32,
	actions ...marshal.PyRep,
) error {
	pairs := make([]marshal.PyRep, len(actions))
	for i, a := range actions {
		pairs[i] = marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(stamp)},
			a,
		}}
	}
	return sendActionList(c, nodeID, accountID, pairs, false)
}

// SendActionListEvents is SendActionList with dogma events in the same packet;
// see the dogma-event block above BuildJamEvent.
func SendActionListEvents(
	c *evenet.Conn,
	nodeID, accountID uint64,
	stamp uint32,
	events []marshal.PyRep,
	actions ...marshal.PyRep,
) error {
	pairs := make([]marshal.PyRep, len(actions))
	for i, a := range actions {
		pairs[i] = marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(stamp)},
			a,
		}}
	}
	return sendActionListEvents(c, nodeID, accountID, pairs, false, events)
}

// ── Dogma events riding a destiny update ──────────────────────────────────────
//
// A DoDestinyUpdate is (actions, waitForBubble, dogmaMessages). michelle
// scatters a non-empty third slot as OnMultiEvent and godma re-scatters every
// event it does not handle itself, so a jam event named here lands on the UI
// service registered for it. See docs/destiny-space.md#dogma-events.

// Jam buckets. The 3913 client knows exactly these three: shipui.py labels
// them Scrambling / Jamming / Miscellaneous and target.py Scr / Jam / Msc; any
// other string renders as "Nameless?". One bar per bucket, showing whichever
// source ends last.
const (
	JamTypeElectronic = "electronic"
	JamTypePropulsion = "propulsion"
	JamTypeOther      = "unknown"
)

// BuildJamEvent builds an OnJamStart (start) or OnJamEnd dogma event. The
// client filters on targetBallID itself, so it is broadcast like any FX.
// startTime is a Win32 filetime, durationMs milliseconds; the client dedupes on
// (sourceBallID, moduleID, targetBallID), so a repeated start refreshes the bar.
func BuildJamEvent(start bool, sourceBallID uint32, moduleID int64,
	targetBallID uint32, jamType string, startTime uint64, durationMs int64) marshal.PyRep {

	name := "OnJamEnd"
	if start {
		name = "OnJamStart"
	}
	items := []marshal.PyRep{
		marshal.PyString{Value: name},
		marshal.PyInt{Value: uint64(sourceBallID)},
		marshal.PyInt{Value: uint64(moduleID)},
		marshal.PyInt{Value: uint64(targetBallID)},
		marshal.PyString{Value: jamType},
	}
	if start {
		items = append(items,
			marshal.PyInt{Value: startTime},
			marshal.PyInt{Value: uint64(durationMs)},
		)
	}
	return marshal.PyTuple{Items: items}
}

// JamBucketForFXGUID maps a client effect GUID onto its jam bucket; false for
// anything that is not electronic warfare. Keyed on the GUID because that is
// what an ActiveEffect carries. Only ECM and scrambling have their own words;
// webs, damps, disruptors, painters and neuts share Miscellaneous. Smartbombs are damage.
func JamBucketForFXGUID(guid string) (string, bool) {
	switch guid {
	case dogma.FXGUIDECMJam, dogma.FXGUIDECMActivate:
		return JamTypeElectronic, true
	case dogma.FXGUIDWarpScramble:
		return JamTypePropulsion, true
	case dogma.FXGUIDEwarTargetBeam, dogma.FXGUIDTargetPaint,
		dogma.FXGUIDEnergyVampire, dogma.FXGUIDModifyTargetSpeed:
		return JamTypeOther, true
	}
	return "", false
}

// JamEventsForEffects rebuilds the jam bars implied by a set of running
// effects. Untargeted effects are skipped: an area jam has no ball to hang a
// bar on, and its victims get their events when they are picked. Both client
// readers filter on the target themselves, so this can be handed to anyone.
func JamEventsForEffects(effects []ActiveEffect) []marshal.PyRep {
	var out []marshal.PyRep
	for _, ae := range effects {
		if ae.TargetID == 0 {
			continue
		}
		bucket, ok := JamBucketForFXGUID(ae.GUID)
		if !ok {
			continue
		}
		out = append(out, BuildJamEvent(true, ae.EntityID, ae.ModuleID,
			ae.TargetID, bucket, ae.StartTime, ae.DurationMs))
	}
	return out
}

// ── internal helpers ──────────────────────────────────────────────────────────

// sendActionList wraps pre-formed (stamp, action) pair tuples in the full
// destiny Notification wire format and delivers them in one packet.
func sendActionList(c *evenet.Conn, nodeID, accountID uint64, pairs []marshal.PyRep, waitForBubble bool) error {
	return sendActionListEvents(c, nodeID, accountID, pairs, waitForBubble, nil)
}

// sendActionListEvents is sendActionList with the DoDestinyUpdate dogma-event
// list populated.
func sendActionListEvents(c *evenet.Conn, nodeID, accountID uint64, pairs []marshal.PyRep, waitForBubble bool, events []marshal.PyRep) error {
	// Extract action names for logging
	actionNames := make([]string, 0, len(pairs))
	for _, pair := range pairs {
		if tuple, ok := pair.(marshal.PyTuple); ok && len(tuple.Items) > 1 {
			if actionTuple, ok := tuple.Items[1].(marshal.PyTuple); ok && len(actionTuple.Items) > 0 {
				if str, ok := actionTuple.Items[0].(marshal.PyString); ok {
					actionNames = append(actionNames, str.Value)
				}
			}
		}
	}

	updateMain := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyList{Items: pairs},
		marshal.PyBool{Value: waitForBubble},
		marshal.PyList{Items: events},
	}}
	innerStream := &marshal.PySubStream{
		Decoded: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: 0},
			marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyInt{Value: 1},
				updateMain,
			}},
		}},
	}
	payload := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: 0},
			innerStream,
		}},
		marshal.PyNone{},
	}}
	pkt := &evenet.PyPacket{
		TypeString: "macho.Notification",
		Type:       evenet.Notification,
		Source: evenet.PyAddress{
			Type:   evenet.AddrNode,
			TypeID: nodeID,
		},
		Dest: evenet.PyAddress{
			Type:        evenet.AddrBroadcast,
			Service:     "DoDestinyUpdate",
			BcastIDType: "clientID",
		},
		UserID:  accountID,
		Payload: payload,
	}
	b, err := marshal.Marshal(pkt.Encode())
	if err != nil {
		log.Printf("destiny.sendActionList: marshal FAILED actions=%v: %v", actionNames, err)
		return err
	}

	err = c.SendPacket(b)
	if err != nil {
		log.Printf("destiny.sendActionList: SendPacket FAILED actions=%v: %v", actionNames, err)
	}
	return err
}

// encodeEffectStates converts active effects into OnSpecialFX-format tuples
// for the effectStates list in SetState. The client's Park.SetState() iterates
// this list and calls OnSpecialFX for each entry, making ongoing effects
// (weapon fire, mining beams) visible to newly arriving players.
func encodeEffectStates(effects []ActiveEffect) marshal.PyRep {
	if len(effects) == 0 {
		return marshal.PyList{}
	}
	items := make([]marshal.PyRep, len(effects))
	for i, ae := range effects {
		items[i] = EncodeActiveEffect(ae)
	}
	return marshal.PyList{Items: items}
}

// FXRepeatCount is the repeat value sent with repeating effects. fxSequencer
// schedules effect.Repeat() every duration ms for repeat cycles, so a large
// count means "until an explicit stop": turret fire cycles client-side.
const FXRepeatCount = 1000

// BuildEffectAction wraps an active effect as an ("OnSpecialFX", args)
// destiny action for use with SendActionList.
func BuildEffectAction(ae ActiveEffect) marshal.PyRep {
	return marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyString{Value: "OnSpecialFX"},
		EncodeActiveEffect(ae),
	}}
}

// EncodeActiveEffect builds the OnSpecialFX argument tuple for an ongoing
// effect: 12 params (the 3913 fxSequencer takes optional duration/repeat after
// the 10 required; the 13-param variant is a later build). duration drives the
// client's shot cadence.
func EncodeActiveEffect(ae ActiveEffect) marshal.PyTuple {
	offVal := int64(0)
	if ae.IsOffensive {
		offVal = 1
	}
	durationMs := ae.DurationMs
	if durationMs <= 0 {
		durationMs = 4000
	}
	// otherTypeID must be PyNone when 0: turret.py SetAmmoColor crashes on
	// cfg.invtypes.Get(0).
	var otherTypeRep marshal.PyRep = marshal.PyNone{}
	if ae.OtherType != 0 {
		otherTypeRep = marshal.PyInt{Value: uint64(ae.OtherType)}
	}
	// targetID must be PyNone when untargeted: fxSequencer.AddTrigger drops the
	// whole trigger when a non-None targetID has no ball, and 0 never does.
	var targetRep marshal.PyRep = marshal.PyNone{}
	if ae.TargetID != 0 {
		targetRep = marshal.PyInt{Value: uint64(ae.TargetID)}
	}
	return marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyInt{Value: uint64(ae.EntityID)},
		marshal.PyInt{Value: uint64(ae.ModuleID)},
		marshal.PyInt{Value: uint64(ae.ModuleType)},
		targetRep,
		otherTypeRep,
		marshal.PyList{},
		marshal.PyString{Value: ae.GUID},
		marshal.PyInt{Value: uint64(offVal)},
		marshal.PyInt{Value: 1}, // start (effect is active)
		marshal.PyInt{Value: 1}, // active
		marshal.PyReal{Value: float64(durationMs)},
		marshal.PyInt{Value: FXRepeatCount},
	}}
}

// encodeModules converts the Modules slice to a PyList of (itemID, typeID)
// tuples that the client's FitHardpoints() iterates to attach turret models.
func encodeModules(mods []ModuleRef) marshal.PyRep {
	if len(mods) == 0 {
		return marshal.PyList{}
	}
	items := make([]marshal.PyRep, len(mods))
	for i, m := range mods {
		items[i] = marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(m.ItemID)},
			marshal.PyInt{Value: uint64(m.TypeID)},
		}}
	}
	return marshal.PyList{Items: items}
}

func encodeSlimItem(s SlimItem) marshal.PyRep {
	// ballID must equal itemID for entities tracked in the destiny ball park;
	// fall back to itemID if the caller left BallID at zero.
	ballID := s.BallID
	if ballID == 0 {
		ballID = s.ItemID
	}
	entries := []marshal.DictEntry{
		{Key: marshal.PyString{Value: "ballID"}, Value: marshal.PyInt{Value: uint64(ballID)}},
		{Key: marshal.PyString{Value: "itemID"}, Value: marshal.PyInt{Value: uint64(s.ItemID)}},
		{Key: marshal.PyString{Value: "typeID"}, Value: marshal.PyInt{Value: uint64(s.TypeID)}},
		{Key: marshal.PyString{Value: "groupID"}, Value: marshal.PyInt{Value: uint64(s.GroupID)}},
		{Key: marshal.PyString{Value: "categoryID"}, Value: marshal.PyInt{Value: uint64(s.CategoryID)}},
		{Key: marshal.PyString{Value: "ownerID"}, Value: marshal.PyInt{Value: uint64(s.OwnerID)}},
		{Key: marshal.PyString{Value: "corpID"}, Value: marshal.PyInt{Value: uint64(s.CorpID)}},
		{Key: marshal.PyString{Value: "allianceID"}, Value: marshal.PyInt{Value: uint64(s.AllianceID)}},
		{Key: marshal.PyString{Value: "warFactionID"}, Value: marshal.PyInt{Value: uint64(s.WarFactionID)}},
		{Key: marshal.PyString{Value: "charID"}, Value: marshal.PyInt{Value: uint64(s.CharID)}},
		{Key: marshal.PyString{Value: "bounty"}, Value: marshal.PyReal{Value: s.Bounty}},
		{Key: marshal.PyString{Value: "modules"}, Value: encodeModules(s.Modules)},
		{Key: marshal.PyString{Value: "securityStatus"}, Value: marshal.PyReal{Value: s.SecurityStatus}},
	}
	// POS structure state, emitted only for structures (PosState set).
	if s.PosState != nil {
		var tsRep marshal.PyRep = marshal.PyNone{}
		if s.PosTimestamp != 0 {
			tsRep = marshal.PyInt{Value: s.PosTimestamp}
		}
		entries = append(entries,
			marshal.DictEntry{Key: marshal.PyString{Value: "posState"}, Value: marshal.PyInt{Value: uint64(*s.PosState)}},
			marshal.DictEntry{Key: marshal.PyString{Value: "posTimestamp"}, Value: tsRep},
		)
	}
	if s.ControlTowerID != 0 {
		entries = append(entries, marshal.DictEntry{
			Key:   marshal.PyString{Value: "controlTowerID"},
			Value: marshal.PyInt{Value: uint64(s.ControlTowerID)},
		})
	}
	// Dungeon editor attributes, emitted when DunObjectID is non-zero.
	if s.DunObjectID != 0 {
		entries = append(entries,
			marshal.DictEntry{Key: marshal.PyString{Value: "dunObjectID"}, Value: marshal.PyInt{Value: uint64(s.DunObjectID)}},
			marshal.DictEntry{Key: marshal.PyString{Value: "dunRoomID"}, Value: marshal.PyInt{Value: uint64(s.DunRoomID)}},
			marshal.DictEntry{Key: marshal.PyString{Value: "dunX"}, Value: marshal.PyReal{Value: s.DunX}},
			marshal.DictEntry{Key: marshal.PyString{Value: "dunY"}, Value: marshal.PyReal{Value: s.DunY}},
			marshal.DictEntry{Key: marshal.PyString{Value: "dunZ"}, Value: marshal.PyReal{Value: s.DunZ}},
		)
		if s.DunYaw != nil {
			entries = append(entries, marshal.DictEntry{
				Key: marshal.PyString{Value: "dunRotation"},
				Value: marshal.PyTuple{Items: []marshal.PyRep{
					marshal.PyReal{Value: *s.DunYaw},
					marshal.PyReal{Value: *s.DunPitch},
					marshal.PyReal{Value: *s.DunRoll},
				}},
			})
		} else {
			entries = append(entries, marshal.DictEntry{
				Key:   marshal.PyString{Value: "dunRotation"},
				Value: marshal.PyNone{},
			})
		}
		if s.DunRadius != nil {
			entries = append(entries, marshal.DictEntry{
				Key:   marshal.PyString{Value: "dunRadius"},
				Value: marshal.PyReal{Value: *s.DunRadius},
			})
		}
		if s.DunAggrRange != nil {
			entries = append(entries, marshal.DictEntry{
				Key: marshal.PyString{Value: "dunGuardCommands"},
				Value: marshal.PyObject{
					Type: "util.KeyVal",
					Args: marshal.PyDict{Items: []marshal.DictEntry{
						{Key: marshal.PyString{Value: "aggressionRange"}, Value: marshal.PyReal{Value: *s.DunAggrRange}},
					}},
				},
			})
		}
		if s.DunDescription != "" {
			entries = append(entries, marshal.DictEntry{
				Key:   marshal.PyString{Value: "dunDescription"},
				Value: marshal.PyString{Value: s.DunDescription},
			})
		}
		if s.DunDirection != nil {
			entries = append(entries, marshal.DictEntry{
				Key: marshal.PyString{Value: "dunDirection"},
				Value: marshal.PyTuple{Items: []marshal.PyRep{
					marshal.PyReal{Value: s.DunDirection[0]},
					marshal.PyReal{Value: s.DunDirection[1]},
					marshal.PyReal{Value: s.DunDirection[2]},
				}},
			})
		}
		if s.GroupID == GroupWarpGate {
			// accelerationGate.py reads slimItem.dunMusicUrl without a getattr default;
			// the key must exist on every gate slim.
			var musicRep marshal.PyRep = marshal.PyNone{}
			if s.DunMusicURL != "" {
				musicRep = marshal.PyString{Value: s.DunMusicURL}
			}
			entries = append(entries, marshal.DictEntry{
				Key:   marshal.PyString{Value: "dunMusicUrl"},
				Value: musicRep,
			})
		}
	}

	// Stargates carry a "jumps" util.Rowset (toCelestialID, locationID) the
	// client uses to derive destinationPath.
	if s.JumpsToSolarSystem != 0 {
		row := marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(s.JumpsToStargate)},
			marshal.PyInt{Value: uint64(s.JumpsToSolarSystem)},
		}}
		jumpsRowset := marshal.PyObject{
			Type: "util.Rowset",
			Args: marshal.PyDict{Items: []marshal.DictEntry{
				{Key: marshal.PyString{Value: "header"}, Value: marshal.PyList{Items: []marshal.PyRep{
					marshal.PyString{Value: "toCelestialID"},
					marshal.PyString{Value: "locationID"},
				}}},
				{Key: marshal.PyString{Value: "RowClass"}, Value: marshal.PyString{Value: "util.Row", IsType1: true}},
				{Key: marshal.PyString{Value: "lines"}, Value: marshal.PyList{Items: []marshal.PyRep{row}}},
			}},
		}
		entries = append(entries, marshal.DictEntry{
			Key:   marshal.PyString{Value: "jumps"},
			Value: jumpsRowset,
		})
	}
	return marshal.PyObject{
		Type: "foo.SlimItem",
		Args: marshal.PyDict{Items: entries},
	}
}

// BuildSimpleAction builds a destiny action tuple without sending it.
// Use with SendActionList to batch multiple actions in a single packet.
func BuildSimpleAction(funcName string, args ...marshal.PyRep) marshal.PyRep {
	return marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyString{Value: funcName},
		marshal.PyTuple{Items: args},
	}}
}

// BuildLaunchMissileAction builds the native LaunchMissile ballpark action,
// format "iiibb": (missileID, targetID, ownerID, aimAtTarget, setFree). The
// dll snaps the missile onto the owner's own position and aims it. Send it in
// the same action list as, and after, the missile's AddBalls. See docs/destiny-space.md#launch-missile.
func BuildLaunchMissileAction(missileID, targetID, ownerID uint32) marshal.PyRep {
	return BuildSimpleAction("LaunchMissile",
		marshal.PyInt{Value: uint64(missileID)},
		marshal.PyInt{Value: uint64(targetID)},
		marshal.PyInt{Value: uint64(ownerID)},
		marshal.PyInt{Value: 1},
		marshal.PyInt{Value: 1},
	)
}

func encodeDamageState(d DamageState) marshal.PyRep {
	return EncodeDamageState(d)
}

// EncodeDamageState encodes a DamageState as the PyRep tuple the client expects.
func EncodeDamageState(d DamageState) marshal.PyRep {
	return marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyReal{Value: d.Shield},
			marshal.PyReal{Value: d.Tau},
			marshal.PyInt{Value: d.Timestamp},
		}},
		marshal.PyReal{Value: d.Armor},
		marshal.PyReal{Value: d.Structure},
	}}
}

// SendMissileImpact sends TerminalExplosion + RemoveBalls for a missile. No
// SetBallPosition: the visible flight is the client's DoCollision curve from
// the ball's current position into the target, and snapping the ball onto the
// target first collapses it to a flash. See docs/destiny-space.md#terminal-remove.
func SendMissileImpact(
	c *evenet.Conn,
	nodeID, accountID uint64,
	stamp uint32,
	missileID uint32,
) error {
	return SendTerminalRemove(c, nodeID, accountID, stamp, missileID)
}

// SendTerminalRemove sends the batched TerminalExplosion + RemoveBalls pair for
// a ball that explodes on removal (missiles, ships). The client plays a death
// explosion only when the RemoveBalls batch carries a TerminalExplosion for
// the same ball; a RemoveBalls alone makes it vanish silently.
func SendTerminalRemove(
	c *evenet.Conn,
	nodeID, accountID uint64,
	stamp uint32,
	ballID uint32,
) error {
	terminalExplosionAction := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyString{Value: "TerminalExplosion"},
		marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(ballID)},
		}},
	}}
	removeBallsAction := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyString{Value: "RemoveBalls"},
		marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyList{Items: []marshal.PyRep{
				marshal.PyInt{Value: uint64(ballID)},
			}},
		}},
	}}
	return sendActionList(c, nodeID, accountID, []marshal.PyRep{
		marshal.PyTuple{Items: []marshal.PyRep{marshal.PyInt{Value: uint64(stamp)}, terminalExplosionAction}},
		marshal.PyTuple{Items: []marshal.PyRep{marshal.PyInt{Value: uint64(stamp)}, removeBallsAction}},
	}, false)
}

// BuildBallStateAction builds an AddBalls action carrying ball state only (no
// slims, no damage states): the one teleport primitive. The client merges the
// state blob into the ballpark and an empty slim list does nothing else, so a
// re-sent header overwrites position, velocity and mode in place. See docs/destiny-space.md#terminal-remove.
func BuildBallStateAction(stamp uint32, balls []BallDesc) marshal.PyRep {
	return BuildAddBallsAction(stamp, balls, nil, nil)
}

// SendTerminalRemoveStop is SendTerminalRemove with a ball-state rewrite
// prepended at the same stamp, for NPC deaths: the client removes an exploding
// ball only after terminalExplosionDelay, so the ball is parked at the death
// position first. leading rides the same stamp ahead. See docs/destiny-space.md#terminal-remove.
func SendTerminalRemoveStop(
	c *evenet.Conn,
	nodeID, accountID uint64,
	stamp uint32,
	ballID uint32,
	deathPos Vec3,
	ball BallDesc,
	leading ...marshal.PyRep,
) error {
	stamped := func(a marshal.PyRep) marshal.PyRep {
		return marshal.PyTuple{Items: []marshal.PyRep{marshal.PyInt{Value: uint64(stamp)}, a}}
	}
	// A rigid ball never moved, so there is nothing to park; rewriting it to STOP
	// would also grow a MassSector its SubType promises not to carry.
	rigid := ball.Mode == ModeRigid
	if rigid && len(leading) == 0 {
		return SendTerminalRemove(c, nodeID, accountID, stamp, ballID)
	}
	ball.EntityID = ballID
	ball.Pos = deathPos
	ball.Vel = Vec3{}
	ball.Mode = ModeStop
	ball.SpeedFraction = 0
	ball.FollowID = 0
	ball.FollowRange = 0
	stateAction := BuildBallStateAction(stamp, []BallDesc{ball})
	stopAction := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyString{Value: "Stop"},
		marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(ballID)},
		}},
	}}
	terminalExplosionAction := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyString{Value: "TerminalExplosion"},
		marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(ballID)},
		}},
	}}
	removeBallsAction := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyString{Value: "RemoveBalls"},
		marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyList{Items: []marshal.PyRep{
				marshal.PyInt{Value: uint64(ballID)},
			}},
		}},
	}}
	// Stop first, then the state rewrite: leaving the moving state recomputes the
	// position from the client's interpolator and would discard a prior rewrite.
	actions := make([]marshal.PyRep, 0, len(leading)+4)
	for _, a := range leading {
		actions = append(actions, stamped(a))
	}
	if !rigid {
		actions = append(actions, stamped(stopAction), stamped(stateAction))
	}
	actions = append(actions, stamped(terminalExplosionAction), stamped(removeBallsAction))
	return sendActionList(c, nodeID, accountID, actions, false)
}

// SendSettleUnpiloted turns a piloted ship ball into an unpiloted hull in place,
// at one stamp: Stop, zero velocity, snap to pos, clear the interactive flag
// (menusvc checkShipBusy reads it, so "Board Ship" appears). The ball is never
// removed, so the hull does not flicker. The new slim (no charID) goes
// separately on the FX stamp (SendSlimItemChange): it is stamp-agnostic.
func SendSettleUnpiloted(
	c *evenet.Conn,
	nodeID, accountID uint64,
	stamp uint32,
	entityID uint32,
	pos Vec3,
) error {
	id := marshal.PyInt{Value: uint64(entityID)}
	actions := []marshal.PyRep{
		BuildSimpleAction("Stop", id),
		BuildSimpleAction("SetBallVelocity", id,
			marshal.PyReal{Value: 0}, marshal.PyReal{Value: 0}, marshal.PyReal{Value: 0}),
		BuildSimpleAction("SetBallPosition", id,
			marshal.PyReal{Value: pos.X}, marshal.PyReal{Value: pos.Y}, marshal.PyReal{Value: pos.Z}),
		BuildSimpleAction("SetBallInteractive", id, marshal.PyInt{Value: 0}),
	}
	pairs := make([]marshal.PyRep, 0, len(actions))
	for _, a := range actions {
		pairs = append(pairs, marshal.PyTuple{Items: []marshal.PyRep{marshal.PyInt{Value: uint64(stamp)}, a}})
	}
	return sendActionList(c, nodeID, accountID, pairs, false)
}
