package db

// Killboard persistence. Killmails are their own rows (head, attackers, items), written at kill
// time from the snapshot the mail builder uses, so the board never parses mail text. Names and
// ISK values are stored, not joined or recomputed. Retention lives in killretention.go.
// See docs/db-6-notes.md#killboard-storage.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Where a killmail came from: the core's player-death handlers, or a module with its own
// death path.
const (
	KillmailSourceCore   = 0
	KillmailSourceModule = 1
)

// EnsureKillmailTables creates the killboard tables if they are absent.
func EnsureKillmailTables(ctx context.Context, db *sql.DB) error {
	stmts := []struct{ name, ddl string }{
		{"killmails", `
			CREATE TABLE IF NOT EXISTS killmails (
				killID             BIGINT UNSIGNED  NOT NULL AUTO_INCREMENT,
				killTime           BIGINT UNSIGNED  NOT NULL,
				solarSystemID      INT UNSIGNED     NOT NULL DEFAULT 0,
				constellationID    INT UNSIGNED     NOT NULL DEFAULT 0,
				regionID           INT UNSIGNED     NOT NULL DEFAULT 0,
				systemName         VARCHAR(100)     NOT NULL DEFAULT '',
				security           DOUBLE           NOT NULL DEFAULT 0,
				posX               DOUBLE           NOT NULL DEFAULT 0,
				posY               DOUBLE           NOT NULL DEFAULT 0,
				posZ               DOUBLE           NOT NULL DEFAULT 0,
				victimCharID       INT UNSIGNED     NOT NULL DEFAULT 0,
				victimName         VARCHAR(85)      NOT NULL DEFAULT '',
				victimCorpID       INT UNSIGNED     NOT NULL DEFAULT 0,
				victimCorpName     VARCHAR(100)     NOT NULL DEFAULT '',
				victimAllianceID   INT UNSIGNED     NOT NULL DEFAULT 0,
				victimAllianceName VARCHAR(100)     NOT NULL DEFAULT '',
				shipTypeID         INT UNSIGNED     NOT NULL DEFAULT 0,
				shipTypeName       VARCHAR(100)     NOT NULL DEFAULT '',
				shipGroupID        INT UNSIGNED     NOT NULL DEFAULT 0,
				isPod              TINYINT UNSIGNED NOT NULL DEFAULT 0,
				damageTaken        DOUBLE           NOT NULL DEFAULT 0,
				attackerCount      SMALLINT UNSIGNED NOT NULL DEFAULT 0,
				finalCharID        INT UNSIGNED     NOT NULL DEFAULT 0,
				finalName          VARCHAR(85)      NOT NULL DEFAULT '',
				finalCorpID        INT UNSIGNED     NOT NULL DEFAULT 0,
				finalCorpName      VARCHAR(100)     NOT NULL DEFAULT '',
				finalShipTypeID    INT UNSIGNED     NOT NULL DEFAULT 0,
				finalIsNPC         TINYINT UNSIGNED NOT NULL DEFAULT 0,
				shipValue          DOUBLE           NOT NULL DEFAULT 0,
				droppedValue       DOUBLE           NOT NULL DEFAULT 0,
				destroyedValue     DOUBLE           NOT NULL DEFAULT 0,
				totalValue         DOUBLE           NOT NULL DEFAULT 0,
				source             TINYINT UNSIGNED NOT NULL DEFAULT 0,
				victimIsBot        TINYINT UNSIGNED NOT NULL DEFAULT 0,
				npcOnly            TINYINT UNSIGNED NOT NULL DEFAULT 0,
				locationID         BIGINT UNSIGNED  NOT NULL DEFAULT 0,
				locationName       VARCHAR(100)     NULL,
				locationGroupID    INT UNSIGNED     NOT NULL DEFAULT 0,
				locationDist       DOUBLE           NOT NULL DEFAULT 0,
				PRIMARY KEY (killID),
				INDEX idx_km_time      (killTime),
				INDEX idx_km_victim    (victimCharID, killTime),
				INDEX idx_km_vcorp     (victimCorpID, killTime),
				INDEX idx_km_valliance (victimAllianceID, killTime),
				INDEX idx_km_system    (solarSystemID, killTime),
				INDEX idx_km_region    (regionID, killTime),
				INDEX idx_km_shiptype  (shipTypeID, killTime),
				INDEX idx_km_value     (totalValue),
				INDEX idx_km_bot       (victimIsBot, killTime),
				INDEX idx_km_npc       (npcOnly, killTime)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`},
		{"killmail_attackers", `
			CREATE TABLE IF NOT EXISTS killmail_attackers (
				killID       BIGINT UNSIGNED   NOT NULL,
				seq          SMALLINT UNSIGNED NOT NULL,
				charID       INT UNSIGNED      NOT NULL DEFAULT 0,
				name         VARCHAR(85)       NOT NULL DEFAULT '',
				corpID       INT UNSIGNED      NOT NULL DEFAULT 0,
				corpName     VARCHAR(100)      NOT NULL DEFAULT '',
				allianceID   INT UNSIGNED      NOT NULL DEFAULT 0,
				allianceName VARCHAR(100)      NOT NULL DEFAULT '',
				secStatus    DOUBLE            NOT NULL DEFAULT 0,
				shipTypeID   INT UNSIGNED      NOT NULL DEFAULT 0,
				shipTypeName VARCHAR(100)      NOT NULL DEFAULT '',
				weaponTypeID INT UNSIGNED      NOT NULL DEFAULT 0,
				weaponName   VARCHAR(100)      NOT NULL DEFAULT '',
				damage       DOUBLE            NOT NULL DEFAULT 0,
				isNPC        TINYINT UNSIGNED  NOT NULL DEFAULT 0,
				finalBlow    TINYINT UNSIGNED  NOT NULL DEFAULT 0,
				lastHit      BIGINT UNSIGNED   NOT NULL DEFAULT 0,
				PRIMARY KEY (killID, seq),
				INDEX idx_kma_char     (charID, killID),
				INDEX idx_kma_corp     (corpID, killID),
				INDEX idx_kma_alliance (allianceID, killID)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`},
		{"killmail_items", `
			CREATE TABLE IF NOT EXISTS killmail_items (
				killID     BIGINT UNSIGNED   NOT NULL,
				seq        SMALLINT UNSIGNED NOT NULL,
				typeID     INT UNSIGNED      NOT NULL DEFAULT 0,
				typeName   VARCHAR(100)      NOT NULL DEFAULT '',
				flag       SMALLINT          NOT NULL DEFAULT 0,
				quantity   INT               NOT NULL DEFAULT 1,
				singleton  TINYINT UNSIGNED  NOT NULL DEFAULT 0,
				dropped    TINYINT UNSIGNED  NOT NULL DEFAULT 0,
				unitValue  DOUBLE            NOT NULL DEFAULT 0,
				totalValue DOUBLE            NOT NULL DEFAULT 0,
				PRIMARY KEY (killID, seq),
				INDEX idx_kmi_type (typeID)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`},
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s.ddl); err != nil {
			return fmt.Errorf("EnsureKillmailTables %s: %w", s.name, err)
		}
	}

	// Migration: add npcOnly column to existing killmails tables.
	var hasNPCOnly int
	_ = db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS
		 WHERE TABLE_SCHEMA = DATABASE()
		   AND TABLE_NAME   = 'killmails'
		   AND COLUMN_NAME  = 'npcOnly'`).Scan(&hasNPCOnly)
	if hasNPCOnly == 0 {
		if _, err := db.ExecContext(ctx, `
			ALTER TABLE killmails
			  ADD COLUMN npcOnly TINYINT UNSIGNED NOT NULL DEFAULT 0 AFTER victimIsBot,
			  ADD INDEX idx_km_npc (npcOnly, killTime)`); err != nil {
			return fmt.Errorf("EnsureKillmailTables add npcOnly: %w", err)
		}
		// Backfill: mark existing kills that had only NPC attackers.
		if _, err := db.ExecContext(ctx, `
			UPDATE killmails k SET k.npcOnly = 1
			 WHERE NOT EXISTS (
				SELECT 1 FROM killmail_attackers a
				 WHERE a.killID = k.killID AND a.isNPC = 0)`); err != nil {
			return fmt.Errorf("EnsureKillmailTables backfill npcOnly: %w", err)
		}
	}

	// Migration: kill-site columns (see killlocation.go). Existing rows stay NULL,
	// which is what BackfillKillmailLocations selects; new rows are never NULL.
	var hasLocation int
	_ = db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS
		 WHERE TABLE_SCHEMA = DATABASE()
		   AND TABLE_NAME   = 'killmails'
		   AND COLUMN_NAME  = 'locationName'`).Scan(&hasLocation)
	if hasLocation == 0 {
		if _, err := db.ExecContext(ctx, `
			ALTER TABLE killmails
			  ADD COLUMN locationID      BIGINT UNSIGNED NOT NULL DEFAULT 0,
			  ADD COLUMN locationName    VARCHAR(100)    NULL,
			  ADD COLUMN locationGroupID INT UNSIGNED    NOT NULL DEFAULT 0,
			  ADD COLUMN locationDist    DOUBLE          NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("EnsureKillmailTables add location: %w", err)
		}
	}

	return nil
}

// ---------------------------------------------------------------------------
// Write side
// ---------------------------------------------------------------------------

// KillmailAttacker is one involved party, already reduced to plain values.
// Mirrors destiny.KillHit plus Damage, so the core adapter is a field copy.
type KillmailAttacker struct {
	CharID       int64
	Name         string // "" → RecordKillmail resolves it
	CorpID       int64
	AllianceID   int64
	SecStatus    float64
	ShipTypeID   int64
	WeaponTypeID int64
	Damage       float64
	IsNPC        bool
	FinalBlow    bool
	Time         int64 // unix seconds of this attacker's last hit
}

// KillmailItem is one item that was aboard. It has no itemID: the entity row is gone when this
// is written.
type KillmailItem struct {
	TypeID    int64
	Flag      int64
	Quantity  int64
	Singleton bool
	Dropped   bool
}

// KillmailRecord is one death as the recorder wants it. Every field is a plain value, so a
// module with its own death path files a killmail with the same call as the core.
type KillmailRecord struct {
	Time             time.Time
	SolarSystemID    int64
	X, Y, Z          float64
	VictimCharID     int64
	VictimName       string
	VictimCorpID     int64
	VictimAllianceID int64
	ShipTypeID       int64
	IsPod            bool
	Attackers        []KillmailAttacker
	Items            []KillmailItem
	Source           int
	VictimIsBot      bool
}

// RecordKillmail writes one killmail (head, attackers, items) in a single transaction with every
// name and ISK value resolved now, and returns the killID. There is no idempotency key: the same
// pilot can lose two ships in a minute and both are real.
func RecordKillmail(ctx context.Context, db *sql.DB, rec KillmailRecord) (int64, error) {
	if rec.Time.IsZero() {
		rec.Time = time.Now()
	}

	res := newKillmailResolver(ctx, db)

	// Values: the hull plus every item type, in one pass.
	valueIDs := make([]int64, 0, len(rec.Items)+1)
	valueIDs = append(valueIDs, rec.ShipTypeID)
	for _, it := range rec.Items {
		valueIDs = append(valueIDs, it.TypeID)
	}
	values, err := TypeValues(ctx, db, valueIDs)
	if err != nil {
		// A pricing failure records the kill unpriced.
		values = map[int64]TypeValue{}
	}

	// Attackers: final blow first, then damage descending; that order is the stored seq.
	atk := append([]KillmailAttacker(nil), rec.Attackers...)
	sort.SliceStable(atk, func(i, j int) bool {
		if atk[i].FinalBlow != atk[j].FinalBlow {
			return atk[i].FinalBlow
		}
		if atk[i].Damage != atk[j].Damage {
			return atk[i].Damage > atk[j].Damage
		}
		return atk[i].Time > atk[j].Time
	})

	var damageTaken float64
	for i := range atk {
		damageTaken += atk[i].Damage
		if atk[i].Name == "" {
			if atk[i].IsNPC {
				atk[i].Name = res.typeName(atk[i].ShipTypeID)
			} else if atk[i].CharID != 0 {
				atk[i].Name = GetCharacterName(ctx, db, atk[i].CharID)
			}
		}
	}

	// The final-blow columns copy row seq 0 so the list renders the killer without a join. With no
	// reported final blow, seq 0 is the hardest hitter.
	var final KillmailAttacker
	if len(atk) > 0 {
		final = atk[0]
	}

	// A kill is NPC-only when no player (or bot) was among the attackers.
	npcOnly := true
	for _, a := range atk {
		if !a.IsNPC {
			npcOnly = false
			break
		}
	}

	shipVal := values[rec.ShipTypeID].Value
	var droppedVal, destroyedVal float64
	for _, it := range rec.Items {
		qty := it.Quantity
		if qty < 1 {
			qty = 1
		}
		line := values[it.TypeID].Value * float64(qty)
		if it.Dropped {
			droppedVal += line
		} else {
			destroyedVal += line
		}
	}

	sysName := GetSolarSystemName(ctx, db, rec.SolarSystemID)
	if sysName == "" {
		sysName = fmt.Sprintf("System %d", rec.SolarSystemID)
	}
	sysSec, _, _ := GetSystemSecurity(ctx, db, rec.SolarSystemID)
	constID, regionID := res.systemParents(rec.SolarSystemID)
	// Where in the system. Never NULL on a new row: an unresolved site is ''.
	site := resolveKillSite(ctx, db, rec.SolarSystemID, rec.X, rec.Y, rec.Z)

	victimName := rec.VictimName
	if victimName == "" && rec.VictimCharID != 0 {
		victimName = GetCharacterName(ctx, db, rec.VictimCharID)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("RecordKillmail begin: %w", err)
	}
	defer tx.Rollback()

	headRes, err := tx.ExecContext(ctx, `
		INSERT INTO killmails (
			killTime, solarSystemID, constellationID, regionID, systemName, security,
			posX, posY, posZ,
			victimCharID, victimName, victimCorpID, victimCorpName,
			victimAllianceID, victimAllianceName,
			shipTypeID, shipTypeName, shipGroupID, isPod,
			damageTaken, attackerCount,
			finalCharID, finalName, finalCorpID, finalCorpName, finalShipTypeID, finalIsNPC,
			shipValue, droppedValue, destroyedValue, totalValue, source, victimIsBot, npcOnly,
			locationID, locationName, locationGroupID, locationDist)
		VALUES (?,?,?,?,?,?, ?,?,?, ?,?,?,?, ?,?, ?,?,?,?, ?,?, ?,?,?,?,?,?, ?,?,?,?,?,?,?, ?,?,?,?)`,
		rec.Time.Unix(), rec.SolarSystemID, constID, regionID, sysName, sysSec,
		rec.X, rec.Y, rec.Z,
		rec.VictimCharID, victimName, rec.VictimCorpID, res.corpName(rec.VictimCorpID),
		rec.VictimAllianceID, res.allianceName(rec.VictimAllianceID),
		rec.ShipTypeID, res.typeName(rec.ShipTypeID), res.groupID(rec.ShipTypeID), boolInt(rec.IsPod),
		damageTaken, len(atk),
		final.CharID, final.Name, final.CorpID, res.corpName(final.CorpID), final.ShipTypeID, boolInt(final.IsNPC),
		shipVal, droppedVal, destroyedVal, shipVal+droppedVal+destroyedVal,
		rec.Source, boolInt(rec.VictimIsBot), boolInt(npcOnly),
		site.ID, site.Name, site.GroupID, site.Dist)
	if err != nil {
		return 0, fmt.Errorf("RecordKillmail head: %w", err)
	}
	killID, err := headRes.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("RecordKillmail killID: %w", err)
	}

	for i, a := range atk {
		lastHit := a.Time
		if lastHit == 0 {
			lastHit = rec.Time.Unix()
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO killmail_attackers (
				killID, seq, charID, name, corpID, corpName, allianceID, allianceName,
				secStatus, shipTypeID, shipTypeName, weaponTypeID, weaponName,
				damage, isNPC, finalBlow, lastHit)
			VALUES (?,?,?,?,?,?,?,?, ?,?,?,?,?, ?,?,?,?)`,
			killID, i, a.CharID, a.Name, a.CorpID, res.corpName(a.CorpID),
			a.AllianceID, res.allianceName(a.AllianceID),
			a.SecStatus, a.ShipTypeID, res.typeName(a.ShipTypeID),
			a.WeaponTypeID, res.typeName(a.WeaponTypeID),
			a.Damage, boolInt(a.IsNPC), boolInt(a.FinalBlow), lastHit); err != nil {
			return 0, fmt.Errorf("RecordKillmail attacker %d: %w", i, err)
		}
	}

	for i, it := range rec.Items {
		qty := it.Quantity
		if qty < 1 {
			qty = 1
		}
		unit := values[it.TypeID].Value
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO killmail_items (
				killID, seq, typeID, typeName, flag, quantity, singleton, dropped,
				unitValue, totalValue)
			VALUES (?,?,?,?,?,?,?,?,?,?)`,
			killID, i, it.TypeID, res.typeName(it.TypeID), it.Flag, qty,
			boolInt(it.Singleton), boolInt(it.Dropped), unit, unit*float64(qty)); err != nil {
			return 0, fmt.Errorf("RecordKillmail item %d: %w", i, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("RecordKillmail commit: %w", err)
	}
	// The star map's kill overlays read mapHistory, which the core feeds from its own player-death
	// handlers only. Module-sourced kills are counted here; core-sourced ones already are.
	if rec.Source == KillmailSourceModule {
		stat := StatShipKill
		if rec.IsPod {
			stat = StatPodKill
		}
		_ = RecordMapEvent(ctx, db, rec.SolarSystemID, stat, 1)
	}
	EmitWorldEvent(KillmailRecordedEvent{KillID: killID, Rec: rec, TotalValue: shipVal + droppedVal + destroyedVal})
	return killID, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// killmailResolver memoises the name lookups for one RecordKillmail call. A
// twenty-attacker kill is twenty corp lookups that are mostly the same two
// corps, and a full cargo hold is fifty type lookups.
type killmailResolver struct {
	ctx       context.Context
	db        *sql.DB
	types     map[int64]string
	groups    map[int64]int64
	corps     map[int64]string
	alliances map[int64]string
	parents   map[int64][2]int64
}

func newKillmailResolver(ctx context.Context, db *sql.DB) *killmailResolver {
	return &killmailResolver{
		ctx: ctx, db: db,
		types:     map[int64]string{},
		groups:    map[int64]int64{},
		corps:     map[int64]string{},
		alliances: map[int64]string{},
		parents:   map[int64][2]int64{},
	}
}

func (r *killmailResolver) typeName(typeID int64) string {
	if typeID == 0 {
		return ""
	}
	if n, ok := r.types[typeID]; ok {
		return n
	}
	n, _ := GetTypeName(r.ctx, r.db, typeID)
	if n == "" {
		n = fmt.Sprintf("Type %d", typeID)
	}
	r.types[typeID] = n
	return n
}

func (r *killmailResolver) groupID(typeID int64) int64 {
	if typeID == 0 {
		return 0
	}
	if g, ok := r.groups[typeID]; ok {
		return g
	}
	var g int64
	_ = r.db.QueryRowContext(r.ctx, `SELECT groupID FROM invTypes WHERE typeID = ?`, typeID).Scan(&g)
	r.groups[typeID] = g
	return g
}

func (r *killmailResolver) corpName(corpID int64) string {
	if corpID == 0 {
		return ""
	}
	if n, ok := r.corps[corpID]; ok {
		return n
	}
	n := GetCorporationName(r.ctx, r.db, corpID)
	r.corps[corpID] = n
	return n
}

func (r *killmailResolver) allianceName(allianceID int64) string {
	if allianceID == 0 {
		return ""
	}
	if n, ok := r.alliances[allianceID]; ok {
		return n
	}
	n, err := GetAllianceName(r.ctx, r.db, allianceID)
	if err != nil {
		n = ""
	}
	r.alliances[allianceID] = n
	return n
}

func (r *killmailResolver) systemParents(systemID int64) (constellationID, regionID int64) {
	if systemID == 0 {
		return 0, 0
	}
	if p, ok := r.parents[systemID]; ok {
		return p[0], p[1]
	}
	var c, rg sql.NullInt64
	_ = r.db.QueryRowContext(r.ctx,
		`SELECT constellationID, regionID FROM mapSolarSystems WHERE solarSystemID = ?`,
		systemID).Scan(&c, &rg)
	r.parents[systemID] = [2]int64{c.Int64, rg.Int64}
	return c.Int64, rg.Int64
}

// ---------------------------------------------------------------------------
// Read side
// ---------------------------------------------------------------------------

// KillmailRow is one line on the board: everything the list page renders, with
// no joins.
type KillmailRow struct {
	KillID             int64
	KillTime           time.Time
	SolarSystemID      int64
	RegionID           int64
	SystemName         string
	Security           float64
	VictimCharID       int64
	VictimName         string
	VictimCorpID       int64
	VictimCorpName     string
	VictimAllianceID   int64
	VictimAllianceName string
	ShipTypeID         int64
	ShipTypeName       string
	IsPod              bool
	DamageTaken        float64
	AttackerCount      int
	FinalCharID        int64
	FinalName          string
	FinalCorpID        int64
	FinalCorpName      string
	FinalAllianceID    int64 // from the final-blow attacker row; 0 when unallied
	FinalAllianceName  string
	FinalShipTypeID    int64
	FinalIsNPC         bool
	RegionName         string // resolved from mapRegions at read time
	ShipValue          float64
	DroppedValue       float64
	DestroyedValue     float64
	TotalValue         float64
	VictimIsBot        bool
	NPCOnly            bool
}

// KillmailAttackerRow is one stored involved party.
type KillmailAttackerRow struct {
	Seq          int
	CharID       int64
	Name         string
	CorpID       int64
	CorpName     string
	AllianceID   int64
	AllianceName string
	SecStatus    float64
	ShipTypeID   int64
	ShipTypeName string
	WeaponTypeID int64
	WeaponName   string
	Damage       float64
	IsNPC        bool
	FinalBlow    bool
	LastHit      time.Time
}

// KillmailItemRow is one stored item.
type KillmailItemRow struct {
	TypeID     int64
	TypeName   string
	Flag       int64
	Quantity   int64
	Singleton  bool
	Dropped    bool
	UnitValue  float64
	TotalValue float64
}

// Item flag bands, mirroring dogma/flags.go. Restated because db does not import dogma; these are
// stored values.
const (
	kmFlagCargo        int64 = 5
	kmFlagLoSlotFirst  int64 = 11
	kmFlagLoSlotLast   int64 = 18
	kmFlagMedSlotFirst int64 = 19
	kmFlagMedSlotLast  int64 = 26
	kmFlagHiSlotFirst  int64 = 27
	kmFlagHiSlotLast   int64 = 34
	kmFlagDroneBay     int64 = 87
	kmFlagRigFirst     int64 = 92
	kmFlagRigLast      int64 = 99
	kmFlagImplant      int64 = 89 // plugged implant; db/implantdb.go flagImplant
)

// Placement labels where the item sat: High, Mid, Low, Rig, Drone Bay, Cargo, Implant or Other. The slot
// index is not shown. The Rig band is recognised although this build has no rig slots.
func (i KillmailItemRow) Placement() string {
	switch {
	case i.Flag >= kmFlagHiSlotFirst && i.Flag <= kmFlagHiSlotLast:
		return "High"
	case i.Flag >= kmFlagMedSlotFirst && i.Flag <= kmFlagMedSlotLast:
		return "Mid"
	case i.Flag >= kmFlagLoSlotFirst && i.Flag <= kmFlagLoSlotLast:
		return "Low"
	case i.Flag >= kmFlagRigFirst && i.Flag <= kmFlagRigLast:
		return "Rig"
	case i.Flag == kmFlagDroneBay:
		return "Drone Bay"
	case i.Flag == kmFlagCargo:
		return "Cargo"
	case i.Flag == kmFlagImplant:
		return "Implant"
	default:
		return "Other"
	}
}

// PlacementRank orders items as the fitting window stacks them: high, mid, low, rig, then the
// holds. Do not sort on the raw flag: low slots are flags 11-18 and would sort first.
func (i KillmailItemRow) PlacementRank() int {
	switch {
	case i.Flag >= kmFlagHiSlotFirst && i.Flag <= kmFlagHiSlotLast:
		return 0
	case i.Flag >= kmFlagMedSlotFirst && i.Flag <= kmFlagMedSlotLast:
		return 1
	case i.Flag >= kmFlagLoSlotFirst && i.Flag <= kmFlagLoSlotLast:
		return 2
	case i.Flag >= kmFlagRigFirst && i.Flag <= kmFlagRigLast:
		return 3
	case i.Flag == kmFlagDroneBay:
		return 4
	case i.Flag == kmFlagCargo:
		return 5
	case i.Flag == kmFlagImplant:
		return 6
	default:
		return 7
	}
}

// KillmailDetail is one whole kill.
type KillmailDetail struct {
	KillmailRow
	ConstellationID  int64
	PosX, PosY, PosZ float64
	Source           int
	Attackers        []KillmailAttackerRow
	Items            []KillmailItemRow
}

// Dropped returns the items that survived into the wreck.
func (d *KillmailDetail) Dropped() []KillmailItemRow { return d.itemsWhere(true) }

// Destroyed returns the items that went up with the ship.
func (d *KillmailDetail) Destroyed() []KillmailItemRow { return d.itemsWhere(false) }

func (d *KillmailDetail) itemsWhere(dropped bool) []KillmailItemRow {
	var out []KillmailItemRow
	for _, it := range d.Items {
		if it.Dropped == dropped {
			out = append(out, it)
		}
	}
	sortKillmailItems(out)
	return out
}

// FittedItems returns every item, dropped and destroyed together, in fitting order. One list
// keeps a rack together; survival is a per-row property. Dropped and Destroyed remain for the
// admin panel, which splits by survival. See docs/db-6-notes.md#killboard-storage.
func (d *KillmailDetail) FittedItems() []KillmailItemRow {
	out := append([]KillmailItemRow(nil), d.Items...)
	sortKillmailItems(out)
	return out
}

// sortKillmailItems orders by slot band, then by value so a gun leads its ammunition. The sort is
// stable with a strict >, so ties keep their stored seq.
func sortKillmailItems(items []KillmailItemRow) {
	sort.SliceStable(items, func(i, j int) bool {
		ri, rj := items[i].PlacementRank(), items[j].PlacementRank()
		if ri != rj {
			return ri < rj
		}
		return items[i].TotalValue > items[j].TotalValue
	})
}

// KillmailFilter narrows the board.  A zero field means "do not filter on it".
type KillmailFilter struct {
	Since, Until                                 time.Time
	SolarSystemID, RegionID, ShipTypeID          int64
	VictimCharID, VictimCorpID, VictimAllianceID int64
	Involving                                    int64 // attacker OR victim (character)
	InvolvingCorp                                int64 // attacker corp only (the corp profile's kills feed)
	MinValue                                     float64
	Kind                                         string // "" | "ship" | "pod"
	NameLike                                     string // victim or attacker name
	IncludeNPCKills                              bool
	HideBots                                     bool // drop synthetic victims (victimIsBot = 1)
	War                                          *WarKillPair // only kills one party of a war took from the other
	Limit, Offset                                int
}

// where builds the shared predicate.  NPC-only kills (where every attacker
// was an NPC) are excluded unless asked for.
func (f KillmailFilter) where() (string, []any) {
	var cl []string
	var args []any
	add := func(s string, a ...any) {
		cl = append(cl, s)
		args = append(args, a...)
	}
	if !f.Since.IsZero() {
		add("k.killTime >= ?", f.Since.Unix())
	}
	if !f.Until.IsZero() {
		add("k.killTime <= ?", f.Until.Unix())
	}
	if f.SolarSystemID != 0 {
		add("k.solarSystemID = ?", f.SolarSystemID)
	}
	if f.RegionID != 0 {
		add("k.regionID = ?", f.RegionID)
	}
	if f.ShipTypeID != 0 {
		add("k.shipTypeID = ?", f.ShipTypeID)
	}
	if f.VictimCharID != 0 {
		add("k.victimCharID = ?", f.VictimCharID)
	}
	if f.VictimCorpID != 0 {
		add("k.victimCorpID = ?", f.VictimCorpID)
	}
	if f.VictimAllianceID != 0 {
		add("k.victimAllianceID = ?", f.VictimAllianceID)
	}
	if f.Involving != 0 {
		// Covered by idx_km_victim and idx_kma_char.
		add(`(k.victimCharID = ? OR EXISTS (
			SELECT 1 FROM killmail_attackers a
			 WHERE a.killID = k.killID AND a.charID = ?))`, f.Involving, f.Involving)
	}
	if f.InvolvingCorp != 0 {
		// Attacker side only: OR-ing in victimCorpID would fold the corporation's losses into its kill
		// feed. Covered by idx_kma_corp.
		add(`EXISTS (SELECT 1 FROM killmail_attackers a
			 WHERE a.killID = k.killID AND a.corpID = ?)`, f.InvolvingCorp)
	}
	if f.War != nil {
		s, a := f.War.where()
		add(s, a...)
	}
	if f.MinValue > 0 {
		add("k.totalValue >= ?", f.MinValue)
	}
	switch f.Kind {
	case "ship":
		add("k.isPod = 0")
	case "pod":
		add("k.isPod = 1")
	}
	if s := strings.TrimSpace(f.NameLike); s != "" {
		like := "%" + s + "%"
		add(`(k.victimName LIKE ? OR k.victimCorpName LIKE ? OR k.finalName LIKE ?)`, like, like, like)
	}
	if !f.IncludeNPCKills {
		add("k.npcOnly = 0")
	}
	if f.HideBots {
		add("k.victimIsBot = 0")
	}
	if len(cl) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(cl, " AND "), args
}

const killmailRowCols = `
	k.killID, k.killTime, k.solarSystemID, k.regionID, k.systemName, k.security,
	k.victimCharID, k.victimName, k.victimCorpID, k.victimCorpName,
	k.victimAllianceID, k.victimAllianceName,
	k.shipTypeID, k.shipTypeName, k.isPod, k.damageTaken, k.attackerCount,
	k.finalCharID, k.finalName, k.finalCorpID, k.finalCorpName,
	COALESCE((SELECT a.allianceID FROM killmail_attackers a
	           WHERE a.killID = k.killID AND a.finalBlow = 1 ORDER BY a.seq LIMIT 1), 0),
	COALESCE((SELECT a.allianceName FROM killmail_attackers a
	           WHERE a.killID = k.killID AND a.finalBlow = 1 ORDER BY a.seq LIMIT 1), ''),
	k.finalShipTypeID, k.finalIsNPC,
	COALESCE((SELECT mr.regionName FROM mapRegions mr WHERE mr.regionID = k.regionID), ''),
	k.shipValue, k.droppedValue, k.destroyedValue, k.totalValue, k.victimIsBot, k.npcOnly`

func scanKillmailRow(sc interface{ Scan(...any) error }) (KillmailRow, error) {
	var r KillmailRow
	var ts int64
	var isPod, npc, bot, npcOnly int
	err := sc.Scan(&r.KillID, &ts, &r.SolarSystemID, &r.RegionID, &r.SystemName, &r.Security,
		&r.VictimCharID, &r.VictimName, &r.VictimCorpID, &r.VictimCorpName,
		&r.VictimAllianceID, &r.VictimAllianceName,
		&r.ShipTypeID, &r.ShipTypeName, &isPod, &r.DamageTaken, &r.AttackerCount,
		&r.FinalCharID, &r.FinalName, &r.FinalCorpID, &r.FinalCorpName,
		&r.FinalAllianceID, &r.FinalAllianceName, &r.FinalShipTypeID, &npc,
		&r.RegionName,
		&r.ShipValue, &r.DroppedValue, &r.DestroyedValue, &r.TotalValue, &bot, &npcOnly)
	if err != nil {
		return r, err
	}
	r.KillTime = time.Unix(ts, 0)
	r.IsPod, r.FinalIsNPC, r.VictimIsBot, r.NPCOnly = isPod != 0, npc != 0, bot != 0, npcOnly != 0
	return r, nil
}

// ListKillmails returns the board, newest first.
func ListKillmails(ctx context.Context, db *sql.DB, f KillmailFilter) ([]KillmailRow, error) {
	where, args := f.where()
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}
	rows, err := db.QueryContext(ctx,
		`SELECT`+killmailRowCols+` FROM killmails k`+where+
			` ORDER BY k.killTime DESC, k.killID DESC LIMIT ? OFFSET ?`,
		append(args, limit, offset)...)
	if err != nil {
		return nil, fmt.Errorf("ListKillmails: %w", err)
	}
	defer rows.Close()
	var out []KillmailRow
	for rows.Next() {
		r, err := scanKillmailRow(rows)
		if err != nil {
			return nil, fmt.Errorf("ListKillmails scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListValuableKillmails returns the set ListKillmails would, ranked by ISK destroyed. The caller
// picks the window through the filter's Since. Ties break on killTime DESC.
func ListValuableKillmails(ctx context.Context, db *sql.DB, f KillmailFilter) ([]KillmailRow, error) {
	where, args := f.where()
	limit := f.Limit
	if limit <= 0 {
		limit = 10
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}
	rows, err := db.QueryContext(ctx,
		`SELECT`+killmailRowCols+` FROM killmails k`+where+
			` ORDER BY k.totalValue DESC, k.killTime DESC, k.killID DESC LIMIT ? OFFSET ?`,
		append(args, limit, offset)...)
	if err != nil {
		return nil, fmt.Errorf("ListValuableKillmails: %w", err)
	}
	defer rows.Close()
	var out []KillmailRow
	for rows.Next() {
		r, err := scanKillmailRow(rows)
		if err != nil {
			return nil, fmt.Errorf("ListValuableKillmails scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CountKillmails counts the same set ListKillmails would page through.
func CountKillmails(ctx context.Context, db *sql.DB, f KillmailFilter) (int64, error) {
	where, args := f.where()
	var n int64
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM killmails k`+where, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("CountKillmails: %w", err)
	}
	return n, nil
}

// GetKillmail returns one whole kill, or (nil, nil) if there is no such killID.
func GetKillmail(ctx context.Context, db *sql.DB, killID int64) (*KillmailDetail, error) {
	var d KillmailDetail
	row := db.QueryRowContext(ctx,
		`SELECT`+killmailRowCols+`, k.constellationID, k.posX, k.posY, k.posZ, k.source
		   FROM killmails k WHERE k.killID = ?`, killID)

	var ts int64
	var isPod, npc, bot, npcOnly int
	err := row.Scan(&d.KillID, &ts, &d.SolarSystemID, &d.RegionID, &d.SystemName, &d.Security,
		&d.VictimCharID, &d.VictimName, &d.VictimCorpID, &d.VictimCorpName,
		&d.VictimAllianceID, &d.VictimAllianceName,
		&d.ShipTypeID, &d.ShipTypeName, &isPod, &d.DamageTaken, &d.AttackerCount,
		&d.FinalCharID, &d.FinalName, &d.FinalCorpID, &d.FinalCorpName,
		&d.FinalAllianceID, &d.FinalAllianceName, &d.FinalShipTypeID, &npc,
		&d.RegionName,
		&d.ShipValue, &d.DroppedValue, &d.DestroyedValue, &d.TotalValue, &bot, &npcOnly,
		&d.ConstellationID, &d.PosX, &d.PosY, &d.PosZ, &d.Source)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("GetKillmail: %w", err)
	}
	d.KillTime = time.Unix(ts, 0)
	d.IsPod, d.FinalIsNPC, d.VictimIsBot, d.NPCOnly = isPod != 0, npc != 0, bot != 0, npcOnly != 0

	arows, err := db.QueryContext(ctx, `
		SELECT seq, charID, name, corpID, corpName, allianceID, allianceName,
		       secStatus, shipTypeID, shipTypeName, weaponTypeID, weaponName,
		       damage, isNPC, finalBlow, lastHit
		  FROM killmail_attackers WHERE killID = ? ORDER BY seq`, killID)
	if err != nil {
		return nil, fmt.Errorf("GetKillmail attackers: %w", err)
	}
	for arows.Next() {
		var a KillmailAttackerRow
		var isNPC, fb int
		var last int64
		if err := arows.Scan(&a.Seq, &a.CharID, &a.Name, &a.CorpID, &a.CorpName,
			&a.AllianceID, &a.AllianceName, &a.SecStatus, &a.ShipTypeID, &a.ShipTypeName,
			&a.WeaponTypeID, &a.WeaponName, &a.Damage, &isNPC, &fb, &last); err != nil {
			arows.Close()
			return nil, fmt.Errorf("GetKillmail attacker scan: %w", err)
		}
		a.IsNPC, a.FinalBlow = isNPC != 0, fb != 0
		a.LastHit = time.Unix(last, 0)
		d.Attackers = append(d.Attackers, a)
	}
	arows.Close()
	if err := arows.Err(); err != nil {
		return nil, fmt.Errorf("GetKillmail attacker rows: %w", err)
	}

	irows, err := db.QueryContext(ctx, `
		SELECT typeID, typeName, flag, quantity, singleton, dropped, unitValue, totalValue
		  FROM killmail_items WHERE killID = ? ORDER BY seq`, killID)
	if err != nil {
		return nil, fmt.Errorf("GetKillmail items: %w", err)
	}
	for irows.Next() {
		var it KillmailItemRow
		var single, dropped int
		if err := irows.Scan(&it.TypeID, &it.TypeName, &it.Flag, &it.Quantity,
			&single, &dropped, &it.UnitValue, &it.TotalValue); err != nil {
			irows.Close()
			return nil, fmt.Errorf("GetKillmail item scan: %w", err)
		}
		it.Singleton, it.Dropped = single != 0, dropped != 0
		d.Items = append(d.Items, it)
	}
	irows.Close()
	if err := irows.Err(); err != nil {
		return nil, fmt.Errorf("GetKillmail item rows: %w", err)
	}
	return &d, nil
}

// KillboardTotals is the headline pair: how many kills and how much ISK over a
// window.
func KillboardTotals(ctx context.Context, db *sql.DB, since time.Time, includeNPC bool) (kills int64, isk float64, err error) {
	return KillmailTotals(ctx, db, KillmailFilter{Since: since, IncludeNPCKills: includeNPC})
}

// KillmailTotals is the count and summed value of any filtered set.
func KillmailTotals(ctx context.Context, db *sql.DB, f KillmailFilter) (kills int64, isk float64, err error) {
	where, args := f.where()
	var v sql.NullFloat64
	err = db.QueryRowContext(ctx,
		`SELECT COUNT(*), SUM(k.totalValue) FROM killmails k`+where, args...).Scan(&kills, &v)
	if err != nil {
		return 0, 0, fmt.Errorf("KillboardTotals: %w", err)
	}
	return kills, v.Float64, nil
}

// KillboardLeader is one row of a leaderboard.
type KillboardLeader struct {
	CharID   int64
	Name     string
	CorpName string
	Count    int64
	ISK      float64
}

// TopKillers ranks pilots by kills over the window, one credit per killmail they appear on and
// NPCs excluded. Every attacker on the mail is credited, not only the final blow.
func TopKillers(ctx context.Context, db *sql.DB, since time.Time, limit int, includeNPC bool) ([]KillboardLeader, error) {
	npcClause := " AND k.npcOnly = 0"
	if includeNPC {
		npcClause = ""
	}
	if limit <= 0 {
		limit = 10
	}
	rows, err := db.QueryContext(ctx, `
		SELECT a.charID, MAX(a.name), MAX(a.corpName),
		       COUNT(DISTINCT a.killID), SUM(k.totalValue / k.attackerCount)
		  FROM killmail_attackers a
		  JOIN killmails k ON k.killID = a.killID
		 WHERE a.isNPC = 0 AND a.charID <> 0 AND k.killTime >= ?`+npcClause+`
		 GROUP BY a.charID
		 ORDER BY COUNT(DISTINCT a.killID) DESC, SUM(k.totalValue) DESC
		 LIMIT ?`, since.Unix(), limit)
	if err != nil {
		return nil, fmt.Errorf("TopKillers: %w", err)
	}
	defer rows.Close()
	return scanLeaders(rows, "TopKillers")
}

// TopLosses ranks pilots by what they lost over the window.
func TopLosses(ctx context.Context, db *sql.DB, since time.Time, limit int, includeNPC bool) ([]KillboardLeader, error) {
	npcClause := " AND k.npcOnly = 0"
	if includeNPC {
		npcClause = ""
	}
	if limit <= 0 {
		limit = 10
	}
	rows, err := db.QueryContext(ctx, `
		SELECT k.victimCharID, MAX(k.victimName), MAX(k.victimCorpName),
		       COUNT(*), SUM(k.totalValue)
		  FROM killmails k
		 WHERE k.victimCharID <> 0 AND k.killTime >= ?`+npcClause+`
		 GROUP BY k.victimCharID
		 ORDER BY SUM(k.totalValue) DESC, COUNT(*) DESC
		 LIMIT ?`, since.Unix(), limit)
	if err != nil {
		return nil, fmt.Errorf("TopLosses: %w", err)
	}
	defer rows.Close()
	return scanLeaders(rows, "TopLosses")
}

func scanLeaders(rows *sql.Rows, who string) ([]KillboardLeader, error) {
	var out []KillboardLeader
	for rows.Next() {
		var l KillboardLeader
		var name, corp sql.NullString
		var isk sql.NullFloat64
		if err := rows.Scan(&l.CharID, &name, &corp, &l.Count, &isk); err != nil {
			return nil, fmt.Errorf("%s scan: %w", who, err)
		}
		l.Name, l.CorpName, l.ISK = name.String, corp.String, isk.Float64
		out = append(out, l)
	}
	return out, rows.Err()
}
