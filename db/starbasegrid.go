package db

// A control tower's power grid and CPU against what its structures draw. Every structure carries
// power (30) and cpu (50); a tower carries powerOutput (11) and cpuOutput (48). The client's
// management window sums those over online structures (moonmining.py UpdateHeader) for its
// gauges, and its Fuel tab bills Liquid Ozone (purpose 2) and Heavy Water (purpose 3) at
// quantity * consumption / supply of the tower's grid: a tower running nothing burns none, one at
// its limit burns the listed amount. Purpose 1 is a flat bill; purpose 4 (strontium) is drawn
// only by reinforcement.

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strconv"
)

// Tower resource purposes (invControlTowerResourcePurposes); 4 is
// ControlTowerPurposeReinforce.
const (
	TowerPurposeOnline = 1
	TowerPurposePower  = 2
	TowerPurposeCPU    = 3
)

// StarbaseGrid is one tower's budget and what its structures draw from it.
type StarbaseGrid struct {
	PowerOutput, CPUOutput float64
	PowerUsed, CPUUsed     float64
}

// PowerFrac is the share of the power grid in use, clamped to [0, 1]. A tower
// with no grid at all reads as unused.
func (g StarbaseGrid) PowerFrac() float64 { return gridFrac(g.PowerUsed, g.PowerOutput) }

// CPUFrac is the share of the CPU in use, clamped to [0, 1].
func (g StarbaseGrid) CPUFrac() float64 { return gridFrac(g.CPUUsed, g.CPUOutput) }

func gridFrac(used, out float64) float64 {
	if out <= 0 || used <= 0 {
		return 0
	}
	return math.Min(used/out, 1)
}

// Shortfall names what is missing to bring up one more structure drawing
// power and cpu: "" when it fits, else "power" or "cpu" (power first, the
// order the window draws its gauges).
func (g StarbaseGrid) Shortfall(power, cpu float64) string {
	const eps = 1e-6
	if power > 0 && g.PowerUsed+power > g.PowerOutput+eps {
		return "power"
	}
	if cpu > 0 && g.CPUUsed+cpu > g.CPUOutput+eps {
		return "cpu"
	}
	return ""
}

// GetStarbaseGrid reads a tower's output and the draw of every structure anchored to it that is
// online or onlining. An onlining structure counts, since it has already claimed its share; that
// stops two structures being brought up into the same last megawatt.
func GetStarbaseGrid(ctx context.Context, db *sql.DB, towerID, towerTypeID int64) (StarbaseGrid, error) {
	var g StarbaseGrid
	if err := ensureStarbaseStructureTable(ctx, db); err != nil {
		return g, err
	}
	out, err := GetTypeAttributes(ctx, db, towerTypeID)
	if err != nil {
		return g, fmt.Errorf("GetStarbaseGrid tower attrs: %w", err)
	}
	g.PowerOutput, g.CPUOutput = out[11], out[48]
	// Boosters (Auxiliary Power and Shield Generation arrays) carry their own powerOutput (11) /
	// cpuOutput (48) and add to the grid only while online. Onlining ones draw but do not yet supply.
	pw, cp, err := towerBoosterOutputs(ctx, db, towerID)
	if err != nil {
		return g, err
	}
	g.PowerOutput += FoldGridOutputs(pw)
	g.CPUOutput += FoldGridOutputs(cp)
	if err := db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(COALESCE(pw.valueFloat, pw.valueInt)), 0),
		       COALESCE(SUM(COALESCE(cp.valueFloat, cp.valueInt)), 0)
		  FROM starbaseStructures s
		  JOIN entity e ON e.itemID = s.entityID
		  LEFT JOIN dgmTypeAttributes pw ON pw.typeID = e.typeID AND pw.attributeID = 30
		  LEFT JOIN dgmTypeAttributes cp ON cp.typeID = e.typeID AND cp.attributeID = 50
		 WHERE s.towerID = ? AND s.state IN (?, ?)`,
		towerID, StarbaseStateOnline, StarbaseStateOnlining,
	).Scan(&g.PowerUsed, &g.CPUUsed); err != nil {
		return g, fmt.Errorf("GetStarbaseGrid draw: %w", err)
	}
	return g, nil
}

// FoldGridOutputs sums the power (or CPU) outputs of a tower's online boosters,
// strongest first, each one weaker by the stacking penalty, so a wall of the
// same array is not a straight multiple. Pure.
func FoldGridOutputs(vals []float64) float64 {
	v := make([]float64, 0, len(vals))
	for _, x := range vals {
		if x > 0 {
			v = append(v, x)
		}
	}
	sort.Sort(sort.Reverse(sort.Float64Slice(v)))
	total := 0.0
	for i, x := range v {
		total += x * StackingPenalty(i)
	}
	return total
}

// towerBoosterOutputs lists the power and CPU outputs (attrs 11 / 48) of every
// online structure anchored to a tower, one entry per structure carrying one.
func towerBoosterOutputs(ctx context.Context, db *sql.DB, towerID int64) (power, cpu []float64, err error) {
	rows, err := db.QueryContext(ctx, `
		SELECT COALESCE(pw.valueFloat, pw.valueInt, 0), COALESCE(cp.valueFloat, cp.valueInt, 0)
		  FROM starbaseStructures s
		  JOIN entity e ON e.itemID = s.entityID
		  LEFT JOIN dgmTypeAttributes pw ON pw.typeID = e.typeID AND pw.attributeID = 11
		  LEFT JOIN dgmTypeAttributes cp ON cp.typeID = e.typeID AND cp.attributeID = 48
		 WHERE s.towerID = ? AND s.state = ?
		   AND (pw.attributeID IS NOT NULL OR cp.attributeID IS NOT NULL)`,
		towerID, StarbaseStateOnline)
	if err != nil {
		return nil, nil, fmt.Errorf("GetStarbaseGrid boosters: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p, c float64
		if err := rows.Scan(&p, &c); err != nil {
			return nil, nil, err
		}
		if p > 0 {
			power = append(power, p)
		}
		if c > 0 {
			cpu = append(cpu, c)
		}
	}
	return power, cpu, rows.Err()
}

// StarbaseGridEnforced is server_settings starbase.enforceGrid (default on): whether a
// structure the tower cannot power or compute for is refused at onlining.
func StarbaseGridEnforced(ctx context.Context, db *sql.DB) bool {
	v, ok, err := GetServerSetting(ctx, db, "starbase.enforceGrid")
	if err != nil || !ok {
		return true
	}
	b, perr := strconv.ParseBool(v)
	return perr != nil || b
}

// getControlTowerBill returns the tower type's resource rows for the given
// purposes.
func getControlTowerBill(ctx context.Context, db *sql.DB, towerTypeID int64, purposes ...int64) ([]ControlTowerResource, error) {
	if len(purposes) == 0 {
		return nil, nil
	}
	q := `
		SELECT controlTowerTypeID, resourceTypeID,
		       COALESCE(purpose, 0), COALESCE(quantity, 0),
		       minSecurityLevel, factionID
		  FROM invControlTowerResources
		 WHERE controlTowerTypeID = ? AND purpose IN (` + placeholders(len(purposes)) + `)
		 ORDER BY purpose, resourceTypeID`
	args := []interface{}{towerTypeID}
	for _, p := range purposes {
		args = append(args, p)
	}
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("getControlTowerBill: %w", err)
	}
	defer rows.Close()
	var out []ControlTowerResource
	for rows.Next() {
		var r ControlTowerResource
		if err := rows.Scan(&r.ControlTowerTypeID, &r.ResourceTypeID, &r.Purpose, &r.Quantity,
			&r.MinSecurityLevel, &r.FactionID); err != nil {
			return nil, fmt.Errorf("getControlTowerBill scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// fuelBillQuantity is one row's per-cycle draw: the listed quantity, scaled by the sov discount
// and, for the power and CPU fuels, by the share of the grid in use, rounded up as the client
// rounds the number it prints. Pure.
func fuelBillQuantity(base int64, purpose int64, discount bool, grid StarbaseGrid) int64 {
	q := float64(base)
	if discount {
		q *= 0.75
	}
	switch purpose {
	case TowerPurposePower:
		q *= grid.PowerFrac()
	case TowerPurposeCPU:
		q *= grid.CPUFrac()
	}
	if q <= 0 {
		return 0
	}
	// A hair of tolerance so 150 × 0.75 × (n/n) does not round 112.5000001
	// up to 113.
	return int64(math.Ceil(q - 1e-9))
}
