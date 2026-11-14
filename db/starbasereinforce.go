// Package db: control tower reinforcement.
//
// A tower whose shield falls to starbase.reinforceShieldPct burns whole hours of its purpose-4
// strontium (invControlTowerResources) and is invulnerable until the bought time runs out.
// See docs/starbase-pos-investigation.md#18-reinforcement.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strconv"

	"gurimoon/syncx"
)

// ControlTowerPurposeReinforce is invControlTowerResourcePurposes 4.
const ControlTowerPurposeReinforce = 4

// StarbaseReinforceShieldPct is the shield percentage at which a tower enters
// reinforcement (server_settings starbase.reinforceShieldPct, default 25).
func StarbaseReinforceShieldPct(ctx context.Context, db *sql.DB) float64 {
	if v, ok, err := GetServerSetting(ctx, db, "starbase.reinforceShieldPct"); err == nil && ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 && f < 100 {
			return f
		}
	}
	return 25
}

// ReinforceHours is how many whole hours of reinforcement have strontium buys
// at ratePerHour.
func ReinforceHours(have, ratePerHour int64) int64 {
	if ratePerHour <= 0 || have <= 0 {
		return 0
	}
	return have / ratePerHour
}

// StarbaseReinforceNeeds returns the per-hour purpose-4 resources for a tower,
// with the same 25% sovereignty discount the fuel gets.
func StarbaseReinforceNeeds(ctx context.Context, db *sql.DB, r StarbaseFuelRow) ([]FuelNeed, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT resourceTypeID, COALESCE(quantity, 0)
		  FROM invControlTowerResources
		 WHERE controlTowerTypeID = ? AND purpose = ?
		 ORDER BY resourceTypeID`, r.TypeID, ControlTowerPurposeReinforce)
	if err != nil {
		return nil, fmt.Errorf("StarbaseReinforceNeeds: %w", err)
	}
	var needs []FuelNeed
	for rows.Next() {
		var n FuelNeed
		if err := rows.Scan(&n.TypeID, &n.Quantity); err != nil {
			rows.Close()
			return nil, err
		}
		needs = append(needs, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if r.OwnerAllianceID != 0 {
		if sov, owned, err := GetSov(ctx, db, r.SolarSystemID); err == nil && owned && sov.AllianceID == r.OwnerAllianceID {
			for i := range needs {
				needs[i].Quantity = int64(math.Ceil(float64(needs[i].Quantity) * 0.75))
			}
		}
	}
	return needs, nil
}

// bayQuantity sums a type's units in a structure's flag-0 bay.
func bayQuantity(ctx context.Context, db *sql.DB, structureID, typeID int64) (int64, error) {
	var n sql.NullInt64
	err := db.QueryRowContext(ctx, `
		SELECT SUM(quantity) FROM entity WHERE locationID = ? AND flag = 0 AND typeID = ?`,
		structureID, typeID).Scan(&n)
	return n.Int64, err
}

// EnterReinforcement burns whole hours of strontium from the tower's bay and sets
// state reinforced. hours = 0 means too little strontium; the tower is untouched.
func EnterReinforcement(ctx context.Context, db *sql.DB, rec StarbaseRecord) (hours int64, until uint64, err error) {
	if err := ensureStarbaseTable(ctx, db); err != nil {
		return 0, 0, err
	}
	needs, err := StarbaseReinforceNeeds(ctx, db, StarbaseFuelRow{
		EntityID: rec.EntityID, TypeID: rec.TypeID, SolarSystemID: rec.SolarSystemID,
		OwnerCorpID: rec.OwnerCorpID, OwnerAllianceID: rec.OwnerAllianceID,
	})
	if err != nil || len(needs) == 0 {
		return 0, 0, err
	}
	// The scarcest purpose-4 resource sets the number of hours.
	hours = math.MaxInt64
	for _, n := range needs {
		have, err := bayQuantity(ctx, db, rec.EntityID, n.TypeID)
		if err != nil {
			return 0, 0, err
		}
		if h := ReinforceHours(have, n.Quantity); h < hours {
			hours = h
		}
	}
	if hours <= 0 || hours == math.MaxInt64 {
		return 0, 0, nil
	}
	burn := make([]FuelNeed, 0, len(needs))
	for _, n := range needs {
		burn = append(burn, FuelNeed{TypeID: n.TypeID, Quantity: n.Quantity * hours})
	}
	if _, ok, err := ConsumeFromStarbaseBay(ctx, db, rec.EntityID, burn); err != nil || !ok {
		return 0, 0, err
	}
	until = win32TimeNow() + uint64(hours)*3600*10000000
	res, err := db.ExecContext(ctx, `
		UPDATE starbase SET state = ?, reinforceUntil = ?
		 WHERE entityID = ? AND state = ?`,
		StarbaseStateReinforced, until, rec.EntityID, StarbaseStateOnline)
	if err != nil {
		return 0, 0, fmt.Errorf("EnterReinforcement: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, 0, fmt.Errorf("tower %d is not online", rec.EntityID)
	}
	return hours, until, nil
}

// ExitReinforcement brings a reinforced tower back online and restarts its fuel
// clock from now (the fuel sweep does not bill a reinforced tower). ok is false
// when the tower was not reinforced.
func ExitReinforcement(ctx context.Context, db *sql.DB, entityID int64) (bool, error) {
	if err := ensureStarbaseTable(ctx, db); err != nil {
		return false, err
	}
	res, err := db.ExecContext(ctx, `
		UPDATE starbase SET state = ?, reinforceUntil = 0, fuelTick = ?
		 WHERE entityID = ? AND state = ?`,
		StarbaseStateOnline, win32TimeNow(), entityID, StarbaseStateReinforced)
	if err != nil {
		return false, fmt.Errorf("ExitReinforcement: %w", err)
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		// Back online counts in the sov recount (sovmajority.go); modules hear the timer end.
		if rec, ok, err := GetStarbase(ctx, db, entityID); err == nil && ok {
			EmitStarbaseEvent(StarbaseEventFor(StarbaseEventReinforceEnded, rec))
		}
	}
	return n > 0, nil
}

// StarbaseStrontiumLowHours is server_settings starbase.strontiumLowHours
// (default 12): fewer hours of reinforcement than this counts as low.
func StarbaseStrontiumLowHours(ctx context.Context, db *sql.DB) float64 {
	if v, ok, err := GetServerSetting(ctx, db, "starbase.strontiumLowHours"); err == nil && ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			return f
		}
	}
	return 12
}

// StarbaseReinforceHoursHeld is how many hours of reinforcement the tower's bay can
// buy now. ok is false for a tower type with no reinforcement bill.
func StarbaseReinforceHoursHeld(ctx context.Context, db *sql.DB, r StarbaseFuelRow) (hours int64, ok bool, err error) {
	needs, err := StarbaseReinforceNeeds(ctx, db, r)
	if err != nil || len(needs) == 0 {
		return 0, false, err
	}
	hours = math.MaxInt64
	for _, n := range needs {
		have, err := bayQuantity(ctx, db, r.EntityID, n.TypeID)
		if err != nil {
			return 0, false, err
		}
		if h := ReinforceHours(have, n.Quantity); h < hours {
			hours = h
		}
	}
	return hours, true, nil
}

var strontiumLowNotified syncx.Map[int64, struct{}] // towerID → struct{}

// CheckStarbaseStrontiumLow fires StarbaseEventStrontiumLow once per crossing of
// the threshold. The fuel sweep calls it beside the fuel-low check.
func CheckStarbaseStrontiumLow(ctx context.Context, db *sql.DB, r StarbaseFuelRow, lowHours float64) {
	hours, ok, err := StarbaseReinforceHoursHeld(ctx, db, r)
	if err != nil || !ok {
		return
	}
	if float64(hours) >= lowHours {
		strontiumLowNotified.Delete(r.EntityID)
		return
	}
	if _, already := strontiumLowNotified.LoadOrStore(r.EntityID, struct{}{}); already {
		return
	}
	EmitStarbaseEvent(StarbaseEvent{Kind: StarbaseEventStrontiumLow, TowerID: r.EntityID,
		SolarSystemID: r.SolarSystemID, OwnerCorpID: r.OwnerCorpID, OwnerAllianceID: r.OwnerAllianceID,
		HoursLeft: float64(hours)})
}

// ReinforcementDue reports whether a reinforced tower's time is up, using the lead
// PosStateAt uses so the server leaves state 3 before the client's timestamp passes.
func ReinforcementDue(rec StarbaseRecord, now uint64) bool {
	return rec.State == StarbaseStateReinforced && now+reinforceSlimLead >= rec.ReinforceUntil
}
