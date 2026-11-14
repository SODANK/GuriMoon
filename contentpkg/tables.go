package contentpkg

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// colKind is the kind of value a package column holds.
type colKind int

const (
	kInt    colKind = iota // integer in [min, max]
	kBool                  // 0 or 1
	kFloat                 // finite number in [min, max]
	kText                  // string of at most size characters
	kEnum                  // one of enum
	kRef                   // "@key" of another package row (scope says which)
	kType                  // base typeID, or "@key" of a package custom type
	kIDList                // comma-separated positive IDs, at most size characters
)

// refScope says which rows a kRef column may point at.
type refScope int

const (
	scopeParent  refScope = iota // the row that encloses this one (a room's dungeon)
	scopeUnit                    // a row of the same dungeon or custom type
	scopePackage                 // any row of the target table in the package
)

// colDef is one column a package may carry. Columns not listed are never exported and are
// rejected on import.
type colDef struct {
	name      string
	kind      colKind
	null      bool // NULL allowed
	required  bool // must be present
	min, max  float64
	size      int // kText and kIDList: maximum characters
	multiline bool
	enum      []string
	ref       string // kRef: key prefix of the target table
	scope     refScope
	zeroNone  bool // kRef: integer 0 also means "none"
	check     func(string) bool
}

// tableSpec describes one content table.
type tableSpec struct {
	name   string   // table name as the server code writes it
	pk     string   // own primary key column, "" for child rows keyed by their parent
	prefix string   // package key prefix for rows of this table
	cols   []colDef // every column a package row may carry
	late   []string // ref columns written after every row exists (leashes, chains)
	ignore []string // database columns that packages never carry
}

const (
	maxInt32  = 2147483647
	maxCoord  = 1e9  // metres from the room origin
	maxRange  = 1e9  // metres
	maxQty    = 1e9  // item quantities
	maxISK    = 1e15 // ISK amounts
	maxAttr   = 1e15 // attribute values
	textLimit = 65535
)

var (
	hexColor = regexp.MustCompile(`^[0-9A-Fa-f]{6}$`)
	digits   = regexp.MustCompile(`^[0-9]{1,10}$`)
	musicURL = regexp.MustCompile(`^res:/[A-Za-z0-9_./-]+\.(?i:ogg|wav|mp3)$`)
)

func intCol(name string, min, max float64) colDef {
	return colDef{name: name, kind: kInt, min: min, max: max}
}
func boolCol(name string) colDef { return colDef{name: name, kind: kBool} }
func floatCol(name string, min, max float64) colDef {
	return colDef{name: name, kind: kFloat, min: min, max: max}
}
func textCol(name string, size int) colDef { return colDef{name: name, kind: kText, size: size} }
func longText(name string, size int) colDef {
	return colDef{name: name, kind: kText, size: size, multiline: true}
}
func enumCol(name string, vals ...string) colDef { return colDef{name: name, kind: kEnum, enum: vals} }
func typeCol(name string) colDef                 { return colDef{name: name, kind: kType} }
func refCol(name, prefix string, scope refScope) colDef {
	return colDef{name: name, kind: kRef, ref: prefix, scope: scope}
}

func (c colDef) orNull() colDef { c.null = true; return c }
func (c colDef) must() colDef   { c.required = true; return c }
func (c colDef) noneZero() colDef {
	c.zeroNone, c.null = true, true
	return c
}
func (c colDef) with(f func(string) bool) colDef { c.check = f; return c }

var (
	tDungeon = &tableSpec{name: "dunDungeons", pk: "dungeonVID", prefix: "d", cols: []colDef{
		textCol("dungeonVName", 255).must(),
		intCol("factionID", 1, maxInt32).must(),
		intCol("version", 0, 1e6),
		intCol("status", 0, 100),
		intCol("dedLevel", 0, 10),
	}}
	tRoom = &tableSpec{name: "dunRooms", pk: "roomID", prefix: "r", cols: []colDef{
		refCol("dungeonVID", "d", scopeParent).must(),
		textCol("roomName", 255).must(),
		intCol("roomOrder", 0, 1000).orNull(),
		boolCol("naturalPhenomena"),
	}}
	tObject = &tableSpec{name: "dunObjects", pk: "objectID", prefix: "o", late: []string{"leashObjectID"}, cols: []colDef{
		refCol("roomID", "r", scopeUnit).must(),
		typeCol("typeID").must(),
		floatCol("x", -maxCoord, maxCoord), floatCol("y", -maxCoord, maxCoord), floatCol("z", -maxCoord, maxCoord),
		floatCol("yaw", -1e4, 1e4).orNull(), floatCol("pitch", -1e4, 1e4).orNull(), floatCol("roll", -1e4, 1e4).orNull(),
		floatCol("radius", 0, 1e8).orNull(),
		floatCol("aggressionRange", 0, maxRange).orNull(),
		enumCol("stance", "aggressive", "defensive", "passive").orNull(),
		floatCol("orbitDistance", 0, maxRange).orNull(),
		floatCol("followRange", 0, maxRange).orNull(),
		floatCol("leashRange", 0, maxRange).orNull(),
		intCol("npcFactionID", 0, maxInt32).orNull(),
		textCol("musicUrl", 255).orNull().with(musicURL.MatchString),
		refCol("leashObjectID", "o", scopeUnit).noneZero(),
		longText("description", 1000).orNull(),
		intCol("spawnWave", 0, 1000).orNull(),
		textCol("ballName", 100).orNull(),
		boolCol("invulnerable"),
		floatCol("hpScale", 0, 1000).orNull(),
		intCol("lootMinDrops", 0, 1000).orNull(),
		intCol("lootMaxDrops", 0, 1000).orNull(),
		colDef{name: "solid", kind: kBool, null: true},
	}}
	tObjLoot = &tableSpec{name: "dunObjectLoot", pk: "lootID", prefix: "l", cols: []colDef{
		refCol("objectID", "o", scopeUnit).must(),
		typeCol("itemTypeID").must(),
		intCol("minQty", 0, maxQty),
		intCol("maxQty", 0, maxQty),
		floatCol("dropChance", 0, 1),
	}}
	tTrigger = &tableSpec{name: "dunTriggers", pk: "triggerID", prefix: "t", cols: []colDef{
		refCol("roomID", "r", scopeUnit).must(),
		enumCol("event", "roomEnter", "npcKilled", "npcDamaged", "roomCleared", "gateActivated", "itemPlaced").must(),
		refCol("targetObjectID", "o", scopeUnit).noneZero(),
		enumCol("action", "transmission", "localChat", "popup", "spawnWave", "music", "completeDungeon").must(),
		textCol("header", 255).orNull(),
		textCol("icon", 64).orNull().with(digits.MatchString),
		longText("body", textLimit).orNull(),
		textCol("senderName", 128).orNull(),
		boolCol("deferUntilLanded"),
		boolCol("onceOnly"),
		intCol("delayMs", 0, 86400000),
		intCol("spawnWave", 0, 1000).orNull(),
		textCol("chatColor", 6).orNull().with(hexColor.MatchString),
		typeCol("itemTypeID").orNull(),
		intCol("itemQty", 1, maxQty),
	}}
	tSpawner = &tableSpec{name: "dunSpawners", pk: "spawnerID", prefix: "s", ignore: []string{"lastDespawnAt"}, cols: []colDef{
		textCol("name", 100).must(),
		refCol("dungeonVID", "d", scopePackage).must(),
		{name: "regionIDs", kind: kIDList, size: 2048},
		floatCol("secMin", -1, 1), floatCol("secMax", -1, 1),
		intCol("maxPerSystem", 1, 20),
		intCol("targetActive", 0, 500),
		intCol("respawnDelayMinutes", 0, 525600),
		boolCol("enabled"),
		enumCol("placement", "planet", "deadspace"),
		boolCol("beaconVisible"),
	}}
	tMission = &tableSpec{name: "missionTemplates", pk: "templateID", prefix: "m", late: []string{"nextMissionID"}, cols: []colDef{
		textCol("missionName", 255).must(),
		enumCol("missionType", "kill", "courier", "trade", "mining"),
		intCol("agentLevel", 1, 5),
		boolCol("storyline"),
		longText("briefingText", textLimit).must(),
		longText("objectiveText", textLimit).orNull(),
		longText("acceptText", textLimit).orNull(),
		longText("completeText", textLimit).orNull(),
		longText("failText", textLimit).orNull(),
		longText("declineText", textLimit).orNull(),
		longText("moreInfoText", textLimit).orNull(),
		textCol("acceptLabel", 64).orNull(), textCol("declineLabel", 64).orNull(),
		textCol("viewMissionLabel", 64).orNull(), textCol("moreInfoLabel", 64).orNull(),
		textCol("completeLabel", 64).orNull(), textCol("quitMissionLabel", 64).orNull(),
		refCol("dungeonVID", "d", scopePackage).noneZero(),
		intCol("dungeonRoomIdx", 0, 1000).orNull(),
		enumCol("completionType", "killAll", "killTarget", "killStructure", "killItem").orNull(),
		typeCol("targetTypeID").orNull(),
		intCol("targetCount", 0, 100000).orNull(),
		typeCol("killItemTypeID").orNull(),
		intCol("killItemQty", 0, maxQty).orNull(),
		enumCol("killItemDropoffMode", "agent", "specific", "nearby").orNull(),
		intCol("killItemDropoffStationID", 0, maxInt32).orNull(),
		intCol("enemyFactionID", 0, maxInt32).orNull(),
		typeCol("courierTypeID").orNull(),
		intCol("courierQty", 0, maxQty).orNull(),
		enumCol("pickupMode", "agent", "specific", "nearby").orNull(),
		intCol("pickupStationID", 0, maxInt32).orNull(),
		enumCol("dropoffMode", "agent", "specific", "nearby").orNull(),
		intCol("dropoffStationID", 0, maxInt32).orNull(),
		typeCol("tradeTypeID").orNull(),
		intCol("tradeQty", 0, maxQty).orNull(),
		typeCol("miningTypeID").orNull(),
		intCol("miningQty", 0, maxQty).orNull(),
		floatCol("minSecurity", -1, 1).orNull(), floatCol("maxSecurity", -1, 1).orNull(),
		intCol("baseRewardISK", 0, maxISK).orNull(),
		intCol("baseRewardLP", 0, 1e9).orNull(),
		intCol("baseBonusISK", 0, maxISK).orNull(),
		typeCol("baseRewardItemTypeID").orNull(),
		intCol("baseRewardItemQty", 0, maxQty).orNull(),
		typeCol("bonusRewardItemTypeID").orNull(),
		intCol("bonusRewardItemQty", 0, maxQty).orNull(),
		intCol("bonusTimeSec", 0, 1e8).orNull(),
		intCol("expiryDays", 0, 3650),
		floatCol("standingGain", -10, 10).orNull(), floatCol("corpStandingGain", -10, 10).orNull(),
		floatCol("factionStandingGain", -10, 10).orNull(), floatCol("standingLoss", -10, 10).orNull(),
		floatCol("factionStandingLoss", -10, 10).orNull(),
		colDef{name: "enabled", kind: kBool, null: true},
		colDef{name: "allowedFactions", kind: kIDList, size: 255, null: true},
		refCol("nextMissionID", "m", scopePackage).noneZero(),
	}}
	tType = &tableSpec{name: "invTypes", pk: "typeID", prefix: "n",
		ignore: []string{"portionSize", "basePrice", "marketGroupID", "chanceOfDuplicating"},
		cols: []colDef{
			textCol("typeName", 100).must(),
			intCol("groupID", 1, maxInt32).must(),
			longText("description", 10000),
			intCol("graphicID", 0, maxInt32).orNull(),
			floatCol("radius", 0, 1e8), floatCol("mass", 0, 1e15),
			floatCol("volume", 0, 1e15), floatCol("capacity", 0, 1e15),
			intCol("raceID", 0, 255).orNull(),
			boolCol("published"),
		}}
	tAttr = &tableSpec{name: "dgmTypeAttributes", cols: []colDef{
		refCol("typeID", "n", scopeParent).must(),
		intCol("attributeID", 1, maxInt32).must(),
		intCol("valueInt", -maxInt32, maxInt32).orNull(),
		floatCol("valueFloat", -maxAttr, maxAttr).orNull(),
	}}
	tEffect = &tableSpec{name: "dgmTypeEffects", cols: []colDef{
		refCol("typeID", "n", scopeParent).must(),
		intCol("effectID", 1, maxInt32).must(),
		intCol("isDefault", 0, 1).orNull(),
	}}
	tNPCLoot = &tableSpec{name: "npc_loot_entries", pk: "entryID", prefix: "e", cols: []colDef{
		refCol("npcTypeID", "n", scopeParent).must(),
		intCol("npcGroupID", 0, 0),
		typeCol("itemTypeID").must(),
		floatCol("dropChance", 0, 1),
		intCol("minQty", 0, maxQty),
		intCol("maxQty", 0, maxQty),
		enumCol("requireRole", "", "bounty"),
	}}
	tNPCCap = &tableSpec{name: "npc_loot_caps", cols: []colDef{
		refCol("npcTypeID", "n", scopeParent).must(),
		intCol("npcGroupID", 0, 0),
		intCol("maxDrops", 0, 1000).must(),
	}}
)

// keyedTables lists every table whose rows get a package key.
var keyedTables = []*tableSpec{tType, tNPCLoot, tDungeon, tRoom, tObject, tObjLoot, tTrigger, tMission, tSpawner}

// specByName finds a keyed table by name.
func specByName(name string) *tableSpec {
	for _, t := range keyedTables {
		if strings.EqualFold(t.name, name) {
			return t
		}
	}
	return nil
}

func prefixSpec(prefix string) *tableSpec {
	for _, t := range keyedTables {
		if t.prefix == prefix {
			return t
		}
	}
	return nil
}

// col returns the column definition by exact name.
func (t *tableSpec) col(name string) (colDef, bool) {
	for _, c := range t.cols {
		if c.name == name {
			return c, true
		}
	}
	return colDef{}, false
}

func (t *tableSpec) isLate(col string) bool {
	for _, c := range t.late {
		if c == col {
			return true
		}
	}
	return false
}

func (t *tableSpec) isIgnored(col string) bool {
	for _, c := range t.ignore {
		if strings.EqualFold(c, col) {
			return true
		}
	}
	return false
}

// typeCols lists a table's typeID columns.
func (t *tableSpec) typeCols() []string {
	var out []string
	for _, c := range t.cols {
		if c.kind == kType {
			out = append(out, c.name)
		}
	}
	return out
}

// queryer is satisfied by *sql.DB and *sql.Tx.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func columnKind(dbType string) byte {
	switch strings.ToUpper(dbType) {
	case "TINYINT", "SMALLINT", "MEDIUMINT", "INT", "INTEGER", "BIGINT", "YEAR",
		"UNSIGNED TINYINT", "UNSIGNED SMALLINT", "UNSIGNED MEDIUMINT", "UNSIGNED INT", "UNSIGNED BIGINT":
		return 'i'
	case "DOUBLE", "FLOAT", "DECIMAL", "REAL":
		return 'f'
	}
	return 's'
}

// tableColumns returns the names of a live table's columns.
func tableColumns(ctx context.Context, q queryer, table string) (map[string]bool, error) {
	rows, err := q.QueryContext(ctx, "SELECT * FROM "+table+" LIMIT 0")
	if err != nil {
		return nil, fmt.Errorf("columns of %s: %w", table, err)
	}
	defer rows.Close()
	names, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("columns of %s: %w", table, err)
	}
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[strings.ToLower(n)] = true
	}
	return out, nil
}

// tableExists reports whether a table can be read.
func tableExists(ctx context.Context, q queryer, table string) bool {
	rows, err := q.QueryContext(ctx, "SELECT 1 FROM "+table+" LIMIT 0")
	if err != nil {
		return false
	}
	rows.Close()
	return true
}

// readRows selects rows of a table and returns them as typed Rows: integers as int64, decimals
// as json.Number, everything else as string. where may be empty.
func readRows(ctx context.Context, q queryer, t *tableSpec, where string, args ...any) ([]Row, error) {
	query := "SELECT * FROM " + t.name
	if where != "" {
		query += " WHERE " + where
	}
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", t.name, err)
	}
	defer rows.Close()
	types, err := rows.ColumnTypes()
	if err != nil {
		return nil, err
	}
	var out []Row
	for rows.Next() {
		vals := make([]sql.NullString, len(types))
		ptrs := make([]any, len(types))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, fmt.Errorf("read %s: %w", t.name, err)
		}
		r := Row{}
		for i, ct := range types {
			if !vals[i].Valid {
				r[ct.Name()] = nil
				continue
			}
			s := vals[i].String
			switch columnKind(ct.DatabaseTypeName()) {
			case 'i':
				if n, err := strconv.ParseInt(s, 10, 64); err == nil {
					r[ct.Name()] = n
					continue
				}
			case 'f':
				if f, err := strconv.ParseFloat(s, 64); err == nil {
					r[ct.Name()] = json.Number(strconv.FormatFloat(f, 'g', -1, 64))
					continue
				}
			}
			r[ct.Name()] = s
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// idList renders IDs for an IN (...) clause.
func idList(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(parts, ",")
}
