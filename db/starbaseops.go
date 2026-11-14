package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math"
	"strconv"
	"sync"
)

// Headless starbase operations (plant, online, fuel, remove), callable with no pilot connection.
// The slash commands, ESP and the rolebots module share these bodies. They check the world
// (CheckStarbaseAnchorPolicy) and never who is asking; ownership is the caller's business.
// See docs/db-10-notes.md#starbase-ops-api.

// StarbaseLiveNotifier makes a change visible in a loaded system. Every method is best-effort
// and returns a short phrase for a reply, "" when the system is not loaded (the on-entry loader
// presents the change then). slash.StarbaseNotifier has the same method set.
type StarbaseLiveNotifier interface {
	StarbasePlanted(entityID int64) string
	StarbaseStateChanged(entityID int64) string
	StarbaseRemoved(entityID, solarSystemID int64) string
}

// StarbaseLive is the process-wide notifier main installs. Operations fall back to it when
// called without a notifier.
var StarbaseLive StarbaseLiveNotifier

func liveOr(n StarbaseLiveNotifier) StarbaseLiveNotifier {
	if n != nil {
		return n
	}
	return StarbaseLive
}

// ── lifecycle events ─────────────────────────────────────────────────────────

// Starbase event kinds.
const (
	StarbaseEventPlanted    = "planted"     // a tower now exists at a moon
	StarbaseEventOnline     = "online"      // onlining completed
	StarbaseEventOffline    = "offline"     // taken offline by a pilot
	StarbaseEventOutOfFuel  = "out-of-fuel" // offlined by the fuel sweep
	StarbaseEventFuelLow    = "fuel-low"    // HoursLeft crossed starbase.fuelLowHours
	StarbaseEventUnanchored = "unanchored"
	StarbaseEventRemoved    = "removed"
	StarbaseEventDestroyed  = "destroyed" // shot down in combat
	StarbaseEventReinforced = "reinforced" // shield shot to the threshold; strontium burning
	// StarbaseEventReinforceEnded fires when a reinforced tower comes back online and counts for
	// sovereignty again.
	StarbaseEventReinforceEnded = "reinforce-ended"
	// StarbaseEventStrontiumLow fires once per crossing of starbase.strontiumLowHours, when the
	// reinforcement the bay can buy falls below the threshold.
	StarbaseEventStrontiumLow = "strontium-low"
)

// StarbaseEvent is one lifecycle transition. HoursLeft is set on fuel-low.
type StarbaseEvent struct {
	Kind            string
	TowerID         int64
	SolarSystemID   int64
	OwnerCorpID     int64
	OwnerAllianceID int64
	HoursLeft       float64
}

var (
	starbaseSubsMu sync.RWMutex
	starbaseSubs   []func(StarbaseEvent)
)

// SubscribeStarbaseEvents registers fn for every lifecycle event. fn runs on its own goroutine
// and must not assume ordering against other events.
func SubscribeStarbaseEvents(fn func(StarbaseEvent)) {
	if fn == nil {
		return
	}
	starbaseSubsMu.Lock()
	starbaseSubs = append(starbaseSubs, fn)
	starbaseSubsMu.Unlock()
}

// EmitStarbaseEvent delivers ev to every subscriber. A panicking subscriber is logged and does
// not take the emitting path down.
func EmitStarbaseEvent(ev StarbaseEvent) {
	starbaseSubsMu.RLock()
	subs := make([]func(StarbaseEvent), len(starbaseSubs))
	copy(subs, starbaseSubs)
	starbaseSubsMu.RUnlock()
	for _, fn := range subs {
		go func(fn func(StarbaseEvent)) {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[pos] starbase event subscriber panicked on %s/%d: %v", ev.Kind, ev.TowerID, r)
				}
			}()
			fn(ev)
		}(fn)
	}
}

// StarbaseEventFor builds an event from a tower record.
func StarbaseEventFor(kind string, rec StarbaseRecord) StarbaseEvent {
	return StarbaseEvent{Kind: kind, TowerID: rec.EntityID, SolarSystemID: rec.SolarSystemID,
		OwnerCorpID: rec.OwnerCorpID, OwnerAllianceID: rec.OwnerAllianceID}
}

// ── plant ────────────────────────────────────────────────────────────────────

// PlantRequest asks for a tower of TypeID at MoonID for a corporation. AllianceID is the
// corp's current alliance (0 if none); the policy decides which alliance the tower records.
type PlantRequest struct {
	CorpID      int64
	AllianceID  int64
	TypeID      int64
	MoonID      int64
	ActorCharID int64 // recorded as the sov claimant; 0 is allowed
}

// PlantResult is what happened. A plant never changes sovereignty: an anchored tower scores
// nothing until online (sovmajority.go).
type PlantResult struct {
	EntityID      int64
	SolarSystemID int64
	MoonName      string
	Decision      StarbaseAnchorDecision
	Live          string
}

// ErrMoonNotFound is PlantStarbaseFor's answer for a moonID with no mapDenormalize row.
var ErrMoonNotFound = errors.New("moon not found")

// PlantStarbaseFor anchors a tower (state "anchored"; OnlineStarbaseFor brings it up). A
// placement refusal comes back unwrapped as *StarbaseAnchorRefusal.
func PlantStarbaseFor(ctx context.Context, db *sql.DB, req PlantRequest, live StarbaseLiveNotifier) (PlantResult, error) {
	var res PlantResult
	var sysID int64
	var name string
	err := db.QueryRowContext(ctx,
		`SELECT solarSystemID, itemName FROM mapDenormalize WHERE itemID = ?`, req.MoonID).Scan(&sysID, &name)
	if errors.Is(err, sql.ErrNoRows) {
		return res, ErrMoonNotFound
	}
	if err != nil {
		return res, fmt.Errorf("PlantStarbaseFor moon: %w", err)
	}
	res.SolarSystemID, res.MoonName = sysID, name

	decision, err := CheckStarbaseAnchorPolicy(ctx, db, sysID, req.CorpID, req.AllianceID)
	if err != nil {
		return res, err
	}
	res.Decision = decision

	entityID, err := PlantStarbase(ctx, db, req.TypeID, req.MoonID, req.CorpID, decision.AllianceID)
	if err != nil {
		return res, err
	}
	res.EntityID = entityID
	if n := liveOr(live); n != nil {
		res.Live = n.StarbasePlanted(entityID)
	}
	EmitStarbaseEvent(StarbaseEvent{Kind: StarbaseEventPlanted, TowerID: entityID, SolarSystemID: sysID,
		OwnerCorpID: req.CorpID, OwnerAllianceID: decision.AllianceID})
	return res, nil
}

// OnlineStarbaseFor starts onlining a tower. The "online" event fires when onlining completes
// (main's finishStarbaseOnlining), not here.
func OnlineStarbaseFor(ctx context.Context, db *sql.DB, towerID int64, live StarbaseLiveNotifier) (newState, liveMsg string, err error) {
	newState, err = OnlineStarbase(ctx, db, towerID)
	if err != nil {
		return "", "", err
	}
	if n := liveOr(live); n != nil {
		liveMsg = n.StarbaseStateChanged(towerID)
	}
	return newState, liveMsg, nil
}

// ── fuel ─────────────────────────────────────────────────────────────────────

// FuelLine is one fuel type loaded.
type FuelLine struct {
	TypeID   int64
	Qty      int64
	PerCycle int64
}

// FuelResult is what FuelStarbaseFor loaded. Lines is empty for a tower type
// with no online fuel bill (it runs for free).
type FuelResult struct {
	Cycles int64
	Lines  []FuelLine
}

// FuelStarbaseFor loads `days` of the tower's real fuel bill into its bay, the bill the fuel
// sweep burns. Liquid Ozone and Heavy Water load for a full grid (StarbaseFuelNeedsFullGrid),
// because the sweep bills them by the grid in use. No bay capacity check: nothing enforces one.
func FuelStarbaseFor(ctx context.Context, db *sql.DB, rec StarbaseRecord, days float64) (FuelResult, error) {
	var res FuelResult
	if days <= 0 {
		return res, fmt.Errorf("days must be positive")
	}
	needs, err := StarbaseFuelNeedsFullGrid(ctx, db, StarbaseFuelRow{
		EntityID: rec.EntityID, TypeID: rec.TypeID, SolarSystemID: rec.SolarSystemID,
		OwnerCorpID: rec.OwnerCorpID, OwnerAllianceID: rec.OwnerAllianceID,
	})
	if err != nil || len(needs) == 0 {
		return res, err
	}
	// Days become cycles through the tower's own cycle (posControlTowerPeriod), rounded up.
	cycleMs := StarbaseFuelCycleMs(ctx, db, rec.TypeID)
	res.Cycles = int64(math.Ceil(days * 24 * 3_600_000 / cycleMs))
	if res.Cycles < 1 {
		res.Cycles = 1
	}
	for _, need := range needs {
		qty := need.Quantity * res.Cycles
		if err := GrantFuelToStarbaseBay(ctx, db, rec.EntityID, need.TypeID, qty, rec.OwnerCorpID); err != nil {
			return res, err
		}
		res.Lines = append(res.Lines, FuelLine{TypeID: need.TypeID, Qty: qty, PerCycle: need.Quantity})
	}
	return res, nil
}

// StarbaseFuelHoursLeft is how long the tower's bay lasts at its current bill:
// the scarcest fuel type decides. ok is false for a tower with no bill.
func StarbaseFuelHoursLeft(ctx context.Context, db *sql.DB, r StarbaseFuelRow) (hours float64, ok bool, err error) {
	needs, err := StarbaseFuelNeeds(ctx, db, r)
	if err != nil || len(needs) == 0 {
		return 0, false, err
	}
	cycles := int64(math.MaxInt64)
	for _, need := range needs {
		if need.Quantity <= 0 {
			continue
		}
		var have sql.NullInt64
		if err := db.QueryRowContext(ctx,
			`SELECT SUM(quantity) FROM entity WHERE locationID = ? AND flag = 0 AND typeID = ?`,
			r.EntityID, need.TypeID).Scan(&have); err != nil {
			return 0, false, fmt.Errorf("StarbaseFuelHoursLeft: %w", err)
		}
		if c := have.Int64 / need.Quantity; c < cycles {
			cycles = c
		}
	}
	if cycles == math.MaxInt64 {
		return 0, false, nil
	}
	return float64(cycles) * StarbaseFuelCycleMs(ctx, db, r.TypeID) / 3_600_000, true, nil
}

// StarbaseFuelLowHours is server_settings `starbase.fuelLowHours` (default 24).
func StarbaseFuelLowHours(ctx context.Context, db *sql.DB) float64 {
	if v, ok, err := GetServerSetting(ctx, db, "starbase.fuelLowHours"); err == nil && ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			return f
		}
	}
	return 24
}

// ── remove ───────────────────────────────────────────────────────────────────

// RemoveResult is what RemoveStarbaseFor did beyond the delete.
type RemoveResult struct {
	SovReleased bool
	SovErr      error // non-fatal: the tower is gone, sov was left alone
	Live        string
	// SpilledCanID is the loot container the tower's bay contents were
	// dropped into (0 when the bay was empty or the spill failed).
	SpilledCanID int64
	SpillErr     error
}

// RemoveStarbaseFor deletes a tower outright (admin teardown, no unanchor sequence) and
// recomputes the system's sovereignty. The bay contents are spilled into a loot container first
// (SpillStarbaseBay).
func RemoveStarbaseFor(ctx context.Context, db *sql.DB, rec StarbaseRecord, live StarbaseLiveNotifier) (RemoveResult, error) {
	var res RemoveResult
	res.SpilledCanID, res.SpillErr = SpillStarbaseBay(ctx, db, rec)
	if err := RemoveStarbase(ctx, db, rec.EntityID); err != nil {
		return res, err
	}
	res.SovReleased, res.SovErr = DropSovIfLastStarbase(ctx, db, rec.SolarSystemID, rec.OwnerAllianceID)
	if n := liveOr(live); n != nil {
		res.Live = n.StarbaseRemoved(rec.EntityID, rec.SolarSystemID)
	}
	EmitStarbaseEvent(StarbaseEventFor(StarbaseEventRemoved, rec))
	return res, nil
}

// SpillStarbaseBay drops everything inside a tower's bays into a new loot container at the
// tower's position, owned by the tower's corporation, and returns it (0 when the bay was
// empty). Nested containers move whole.
func SpillStarbaseBay(ctx context.Context, db *sql.DB, rec StarbaseRecord) (int64, error) {
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entity WHERE locationID = ?`, rec.EntityID).Scan(&n); err != nil {
		return 0, fmt.Errorf("SpillStarbaseBay count: %w", err)
	}
	if n == 0 {
		return 0, nil
	}
	canID, err := CreateLootContainer(ctx, db, rec.OwnerCorpID, rec.SolarSystemID, rec.X+2500, rec.Y, rec.Z)
	if err != nil {
		return 0, err
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE entity SET locationID = ?, flag = 0 WHERE locationID = ?`, canID, rec.EntityID); err != nil {
		return canID, fmt.Errorf("SpillStarbaseBay move: %w", err)
	}
	return canID, nil
}
