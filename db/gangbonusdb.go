package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"gurimoon/dogma"
	"gurimoon/syncx"
)

// Gang bonus inputs: what each gang member could project onto the gang. A recompute runs on
// every membership change, undock and safety sweep, so per-pilot data is cached for
// gangSourceTTL (skill levels and mindlinks change rarely).

// GangSourceData is one pilot's gang-relevant skills and implants.
type GangSourceData struct {
	Levels   map[int64]int  // gang skill typeID → trained level
	Implants map[int64]bool // mindlink typeIDs plugged in
}

const gangSourceTTL = 60 * time.Second

type gangSourceEntry struct {
	data GangSourceData
	at   time.Time
}

var gangSourceCache syncx.Map[int64, gangSourceEntry] // charID → gangSourceEntry

// InvalidateGangSource drops a pilot's cached gang data (skill trained,
// implant plugged or unplugged).
func InvalidateGangSource(charID int64) { gangSourceCache.Delete(charID) }

// GetGangSourceData returns the gang skills and mindlinks of every charID,
// batching the misses into one skill query and one implant query. A charID
// with nothing relevant still gets an entry (empty maps), so it is cached too.
func GetGangSourceData(ctx context.Context, db *sql.DB, charIDs []int64) (map[int64]GangSourceData, error) {
	out := make(map[int64]GangSourceData, len(charIDs))
	now := time.Now()
	var miss []int64
	for _, id := range charIDs {
		if v, ok := gangSourceCache.Load(id); ok {
			if e := v; now.Sub(e.at) < gangSourceTTL {
				out[id] = e.data
				continue
			}
		}
		miss = append(miss, id)
	}
	if len(miss) == 0 {
		return out, nil
	}
	fresh := make(map[int64]GangSourceData, len(miss))
	for _, id := range miss {
		fresh[id] = GangSourceData{Levels: map[int64]int{}, Implants: map[int64]bool{}}
	}

	// NULL level means injected and untrained, which projects nothing (GetCharacterSkillLevel's
	// COALESCE(...,1) would give an untrained book a free level).
	charPH, charArgs := int64Placeholders(miss)
	skillPH, skillArgs := int64Placeholders(dogma.GangSourceSkillIDs())
	rows, err := db.QueryContext(ctx, `
		SELECT e.ownerID, e.typeID, COALESCE(lv.valueInt, 0)
		FROM entity e
		LEFT JOIN entity_attributes lv ON lv.itemID = e.itemID AND lv.attributeID = 280
		WHERE e.ownerID IN (`+charPH+`) AND e.flag IN (7, 61) AND e.typeID IN (`+skillPH+`)`,
		append(charArgs, skillArgs...)...)
	if err != nil {
		return nil, fmt.Errorf("GetGangSourceData skills: %w", err)
	}
	for rows.Next() {
		var owner, typeID int64
		var lvl int
		if err := rows.Scan(&owner, &typeID, &lvl); err != nil {
			rows.Close()
			return nil, fmt.Errorf("GetGangSourceData skills scan: %w", err)
		}
		if d, ok := fresh[owner]; ok && lvl > d.Levels[typeID] {
			d.Levels[typeID] = lvl
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("GetGangSourceData skills rows: %w", err)
	}

	linkPH, linkArgs := int64Placeholders(dogma.GangMindlinkIDs())
	rows, err = db.QueryContext(ctx, `
		SELECT locationID, typeID FROM entity
		WHERE locationID IN (`+charPH+`) AND flag = ? AND typeID IN (`+linkPH+`)`,
		append(append(charArgs, int64(flagImplant)), linkArgs...)...)
	if err != nil {
		return nil, fmt.Errorf("GetGangSourceData implants: %w", err)
	}
	for rows.Next() {
		var loc, typeID int64
		if err := rows.Scan(&loc, &typeID); err != nil {
			rows.Close()
			return nil, fmt.Errorf("GetGangSourceData implants scan: %w", err)
		}
		if d, ok := fresh[loc]; ok {
			d.Implants[typeID] = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("GetGangSourceData implants rows: %w", err)
	}

	for id, d := range fresh {
		gangSourceCache.Store(id, gangSourceEntry{data: d, at: now})
		out[id] = d
	}
	return out, nil
}

// GangSkillTables returns each gang skill's per-level percentage and each
// mindlink's percentage increase, read from the types' own attributes (static
// data, cached by TypeAttrs).
func GangSkillTables(ctx context.Context, db *sql.DB) (perLevel, mindlinkPct map[int64]float64, err error) {
	perLevel = make(map[int64]float64, len(dogma.GangSkillSpecs))
	mindlinkPct = make(map[int64]float64)
	for _, spec := range dogma.GangSkillSpecs {
		attrs, err := TypeAttrs(ctx, db, spec.SkillID)
		if err != nil {
			return nil, nil, fmt.Errorf("GangSkillTables skill %d: %w", spec.SkillID, err)
		}
		perLevel[spec.SkillID] = attrs[spec.BonusAttr]
		if spec.Mindlink != 0 {
			la, err := TypeAttrs(ctx, db, spec.Mindlink)
			if err != nil {
				return nil, nil, fmt.Errorf("GangSkillTables mindlink %d: %w", spec.Mindlink, err)
			}
			mindlinkPct[spec.Mindlink] = la[dogma.AttrMindlinkBonus]
		}
	}
	return perLevel, mindlinkPct, nil
}

// GangLinkTables returns each link's commandBonus and Squadron Command's
// per-level percentage (static data).
func GangLinkTables(ctx context.Context, db *sql.DB) (commandBonus map[int64]float64, squadronPct float64, err error) {
	commandBonus = make(map[int64]float64, len(dogma.GangLinkSpecs))
	for _, spec := range dogma.GangLinkSpecs {
		attrs, err := TypeAttrs(ctx, db, spec.TypeID)
		if err != nil {
			return nil, 0, fmt.Errorf("GangLinkTables link %d: %w", spec.TypeID, err)
		}
		commandBonus[spec.TypeID] = attrs[dogma.AttrCommandBonus]
	}
	sq, err := TypeAttrs(ctx, db, dogma.SkillSquadronCommand)
	if err != nil {
		return nil, 0, fmt.Errorf("GangLinkTables squadron: %w", err)
	}
	return commandBonus, sq[dogma.AttrSquadronCommandBonus], nil
}

// GangLinkStrengthScale is the server_settings knob `gang.linkStrengthScale` (default 1.0):
// a flat multiplier on every link's strength. Values <= 0 or unparseable read as 1.
func GangLinkStrengthScale(ctx context.Context, db *sql.DB) float64 {
	v, ok, err := GetServerSetting(ctx, db, "gang.linkStrengthScale")
	if err != nil || !ok {
		return 1
	}
	var f float64
	if _, err := fmt.Sscanf(v, "%g", &f); err != nil || f <= 0 {
		return 1
	}
	return f
}

// ShipGangLinkSlots returns how many links shipID may run at once: one, plus
// maxGangModules for every online module that carries it (Command Processor).
func ShipGangLinkSlots(ctx context.Context, db *sql.DB, shipID int64) (int, error) {
	var extra sql.NullFloat64
	err := db.QueryRowContext(ctx, `
		SELECT SUM(COALESCE(ta.valueFloat, ta.valueInt))
		FROM entity e
		JOIN entity_attributes ea ON ea.itemID = e.itemID AND ea.attributeID = 2 AND ea.valueInt = 1
		JOIN dgmTypeAttributes ta ON ta.typeID = e.typeID AND ta.attributeID = ?
		WHERE e.locationID = ? AND e.flag BETWEEN 11 AND 34`,
		dogma.AttrMaxGangModules, shipID).Scan(&extra)
	if err != nil {
		return 1, fmt.Errorf("ShipGangLinkSlots: %w", err)
	}
	return 1 + int(extra.Float64), nil
}

func int64Placeholders(ids []int64) (string, []any) {
	if len(ids) == 0 {
		return "NULL", nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return strings.TrimSuffix(strings.Repeat("?,", len(ids)), ","), args
}
