package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"strings"

	"gurimoon/rates"
)

// ── ESP result types (plain Go structs, not PyRep) ──────────────────────────

// ESPAccount is an account row for the ESP admin panel.
type ESPAccount struct {
	AccountID   int64
	AccountName string
	Role        int64
	CharCount   int
}

// ESPCharacterSummary is a character row for search results.
type ESPCharacterSummary struct {
	CharacterID   int64
	CharacterName string
	CorporationID int64
	CorpName      string
	Balance       float64
	StationID     int64
	StationName   string

	// Skill training / queue summary (populated by the list queries).
	TrainingName  string // typeName of actively training skill ("" = idle)
	TrainingLevel int    // target level of active training
	TrainingEnd   uint64 // endOfTraining filetime (0 = idle)
	QueueDepth    int    // number of queued entries

	// IsBot: the character is driven by the rolebots sim (rolebot_characters),
	// main-roster pilots handed to it included. IsBotAccount: the character lives on
	// a generated RoleBot<N> account, i.e. synthetic population.
	IsBot        bool
	IsBotAccount bool
}

// ESPCharacterDetail is the full character detail for the view page.
type ESPCharacterDetail struct {
	CharacterID     int64
	CharacterName   string
	AccountID       int64
	AccountName     string
	CorporationID   int64
	CorpName        string
	Balance         float64
	SecurityRating  float64
	StationID       int64
	StationName     string
	SolarSystemID   int64
	ConstellationID int64
	RegionID        int64
	Gender          int64
	BloodlineID     int64
	RaceID          int64

	// Display context for the character profile page.
	Title           string
	Bounty          float64
	LogonMinutes    int64
	CreateDateTime  uint64 // Windows filetime, 0 when never set
	SolarSystemName string
	RegionName      string
	BloodlineName   string
	RaceName        string
}

// GenderName renders character_.gender the way the client does: 1 is male,
// 0 female.  Agents use the same column with the same meaning.
func (d *ESPCharacterDetail) GenderName() string {
	if d.Gender == 1 {
		return "Male"
	}
	return "Female"
}

// ESPAccountCharacter is a sibling character on the same account.
type ESPAccountCharacter struct {
	CharacterID   int64
	CharacterName string
	CorpName      string
	Balance       float64
}

// ESPSkillEntry is a single skill row.
type ESPSkillEntry struct {
	TypeID      int64
	TypeName    string
	Level       int
	SkillPoints int64
}

// ESPCorpDetail is a corporation detail row.
type ESPCorpDetail struct {
	CorporationID   int64
	CorporationName string
	TickerName      string
	CEOID           int64
	CEOName         string
	StationID       int64
	MemberCount     int
	TaxRate         float64
}

// ESPCorpMember is a member row in a corporation.
type ESPCorpMember struct {
	CharacterID   int64
	CharacterName string
	StationID     int64
	StationName   string
	Balance       float64
}

// ESPStation is a station search result.
type ESPStation struct {
	StationID     int64
	StationName   string
	SolarSystemID int64
	SystemName    string
	DockedCount   int
}

// ── ESP query functions ─────────────────────────────────────────────────────

// ESPListAccounts returns accounts, optionally filtered by name search.
func ESPListAccounts(ctx context.Context, db *sql.DB, search string) ([]ESPAccount, error) {
	var rows *sql.Rows
	var err error
	if search != "" {
		rows, err = db.QueryContext(ctx, `
			SELECT a.accountid, a.accountname, a.role,
			       (SELECT COUNT(*) FROM character_ c WHERE c.accountID = a.accountid) AS charcount
			FROM account a
			WHERE a.accountname LIKE ?
			ORDER BY a.accountid
			LIMIT 100`, "%"+search+"%")
	} else {
		rows, err = db.QueryContext(ctx, `
			SELECT a.accountid, a.accountname, a.role,
			       (SELECT COUNT(*) FROM character_ c WHERE c.accountID = a.accountid) AS charcount
			FROM account a
			ORDER BY a.accountid
			LIMIT 100`)
	}
	if err != nil {
		return nil, fmt.Errorf("ESPListAccounts: %w", err)
	}
	defer rows.Close()

	var results []ESPAccount
	for rows.Next() {
		var a ESPAccount
		if err := rows.Scan(&a.AccountID, &a.AccountName, &a.Role, &a.CharCount); err != nil {
			return nil, fmt.Errorf("ESPListAccounts scan: %w", err)
		}
		results = append(results, a)
	}
	return results, rows.Err()
}

// ESPCharacterFilter narrows and pages the character list. Search is a name
// substring or an exact ID; CorpID and StationID narrow to one corporation or
// station. A rendered page always sets Limit.
type ESPCharacterFilter struct {
	Search    string
	CorpID    int64
	StationID int64
	ShowBots  bool // when false (default), exclude characters on RoleBot<N> accounts
	Limit     int  // 0 = no limit; callers that render should always set one
	Offset    int
}

// botAccountsDerived is the set of generated rolebot accounts: "RoleBot" followed
// by digits, prefix case-insensitive (the module's botSeqOf rule). Keyed on the
// account, not rolebot_characters: a main-roster pilot handed to the sim stays listed.
const botAccountsDerived = `(SELECT accountID FROM account
	WHERE LOWER(accountName) REGEXP '^rolebot[0-9]+$')`

// where builds the shared WHERE clause and its arguments. accountID > 0 is the
// player/NPC separator and is not optional: NPC agents live in character_ too,
// so an unfiltered list returns one row per agent.
func (f ESPCharacterFilter) where() (string, []interface{}) {
	clauses := []string{"c.accountID > 0"}
	var args []interface{}
	if s := strings.TrimSpace(f.Search); s != "" {
		clauses = append(clauses, "(c.characterName LIKE ? OR CAST(c.characterID AS CHAR) = ?)")
		args = append(args, "%"+s+"%", s)
	}
	if f.CorpID > 0 {
		clauses = append(clauses, "c.corporationID = ?")
		args = append(args, f.CorpID)
	}
	if f.StationID > 0 {
		clauses = append(clauses, "c.stationID = ?")
		args = append(args, f.StationID)
	}
	if !f.ShowBots {
		clauses = append(clauses, "rba.accountID IS NULL")
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

// ESPListCharacters returns one page of player characters, name-ordered.
func ESPListCharacters(ctx context.Context, db *sql.DB, f ESPCharacterFilter) ([]ESPCharacterSummary, error) {
	where, args := f.where()

	// The skill-queue depth is a derived table, not a correlated subquery: a subquery
	// per row turns one scan of chrSkillQueue into one per character.
	query := `
		SELECT c.characterID, c.characterName, c.corporationID,
		       COALESCE(corp.corporationName, ''), c.balance,
		       c.stationID, COALESCE(s.stationName, ''),
		       COALESCE(t2.typeName, ''), COALESCE(st.targetLevel, 0),
		       COALESCE(st.endOfTraining, 0),
		       COALESCE(sq.cnt, 0),
		       IF(rb.characterID IS NOT NULL, 1, 0),
		       IF(rba.accountID IS NOT NULL, 1, 0)
		FROM character_ c
		LEFT JOIN corporation corp ON corp.corporationID = c.corporationID
		LEFT JOIN staStations s ON s.stationID = c.stationID
		LEFT JOIN chrSkillTraining st ON st.characterID = c.characterID
		LEFT JOIN invTypes t2 ON t2.typeID = st.typeID
		LEFT JOIN (SELECT characterID, COUNT(*) cnt FROM chrSkillQueue GROUP BY characterID) sq
		          ON sq.characterID = c.characterID
		LEFT JOIN rolebot_characters rb ON rb.characterID = c.characterID
		LEFT JOIN ` + botAccountsDerived + ` rba ON rba.accountID = c.accountID` + where + `
		ORDER BY c.characterName`
	if f.Limit > 0 {
		query += " LIMIT ? OFFSET ?"
		args = append(args, f.Limit, f.Offset)
	}

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("ESPListCharacters: %w", err)
	}
	defer rows.Close()

	var results []ESPCharacterSummary
	for rows.Next() {
		var ch ESPCharacterSummary
		if err := rows.Scan(&ch.CharacterID, &ch.CharacterName, &ch.CorporationID,
			&ch.CorpName, &ch.Balance, &ch.StationID, &ch.StationName,
			&ch.TrainingName, &ch.TrainingLevel, &ch.TrainingEnd, &ch.QueueDepth,
			&ch.IsBot, &ch.IsBotAccount); err != nil {
			return nil, fmt.Errorf("ESPListCharacters scan: %w", err)
		}
		results = append(results, ch)
	}
	return results, rows.Err()
}

// ESPCountCharacters counts the rows ESPListCharacters would return unpaged. It
// repeats only the FROM/WHERE the filter touches, none of the display joins, so
// the count is an index read.
func ESPCountCharacters(ctx context.Context, db *sql.DB, f ESPCharacterFilter) (int64, error) {
	where, args := f.where()
	var n int64
	// The bot-account JOIN is needed when bots are hidden (the default),
	// because the WHERE references rba.accountID.
	err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM character_ c LEFT JOIN "+botAccountsDerived+
			" rba ON rba.accountID = c.accountID"+where, args...).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("ESPCountCharacters: %w", err)
	}
	return n, nil
}

// ESPGetCharacterDetail loads full character detail by ID.
func ESPGetCharacterDetail(ctx context.Context, db *sql.DB, charID int64) (*ESPCharacterDetail, error) {
	d := &ESPCharacterDetail{}
	err := db.QueryRowContext(ctx, `
		SELECT c.characterID, c.characterName, COALESCE(c.accountID, 0), COALESCE(a.accountName, ''),
		       c.corporationID, COALESCE(corp.corporationName, ''), c.balance, c.securityRating,
		       c.stationID, COALESCE(s.stationName, ''),
		       c.solarSystemID, c.constellationID, c.regionID,
		       c.gender, c.bloodlineID, c.raceID,
		       c.title, c.bounty, c.logonMinutes, c.createDateTime,
		       COALESCE(sys.solarSystemName, ''), COALESCE(rg.regionName, ''),
		       COALESCE(bl.bloodlineName, ''), COALESCE(rc.raceName, '')
		FROM character_ c
		LEFT JOIN account a ON a.accountID = c.accountID
		LEFT JOIN corporation corp ON corp.corporationID = c.corporationID
		LEFT JOIN staStations s ON s.stationID = c.stationID
		LEFT JOIN mapSolarSystems sys ON sys.solarSystemID = c.solarSystemID
		LEFT JOIN mapRegions rg ON rg.regionID = c.regionID
		LEFT JOIN chrBloodlines bl ON bl.bloodlineID = c.bloodlineID
		LEFT JOIN chrRaces rc ON rc.raceID = c.raceID
		WHERE c.characterID = ?`, charID,
	).Scan(&d.CharacterID, &d.CharacterName, &d.AccountID, &d.AccountName,
		&d.CorporationID, &d.CorpName, &d.Balance, &d.SecurityRating,
		&d.StationID, &d.StationName,
		&d.SolarSystemID, &d.ConstellationID, &d.RegionID,
		&d.Gender, &d.BloodlineID, &d.RaceID,
		&d.Title, &d.Bounty, &d.LogonMinutes, &d.CreateDateTime,
		&d.SolarSystemName, &d.RegionName, &d.BloodlineName, &d.RaceName)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("character %d not found", charID)
		}
		return nil, fmt.Errorf("ESPGetCharacterDetail: %w", err)
	}
	return d, nil
}

// ESPGetAccountCharacters returns all characters on the given account.
func ESPGetAccountCharacters(ctx context.Context, db *sql.DB, accountID int64) ([]ESPAccountCharacter, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT c.characterID, c.characterName,
		       COALESCE(corp.corporationName, ''), c.balance
		FROM character_ c
		LEFT JOIN corporation corp ON corp.corporationID = c.corporationID
		WHERE c.accountID = ?
		ORDER BY c.characterName`, accountID)
	if err != nil {
		return nil, fmt.Errorf("ESPGetAccountCharacters: %w", err)
	}
	defer rows.Close()

	var results []ESPAccountCharacter
	for rows.Next() {
		var ch ESPAccountCharacter
		if err := rows.Scan(&ch.CharacterID, &ch.CharacterName, &ch.CorpName, &ch.Balance); err != nil {
			return nil, fmt.Errorf("ESPGetAccountCharacters scan: %w", err)
		}
		results = append(results, ch)
	}
	return results, rows.Err()
}

// ESPJournalEntry is one wallet journal row for the character page, with the refType named.
type ESPJournalEntry struct {
	RefID     int64
	TransDate int64
	RefType   string
	Amount    float64
	Balance   float64
	Reason    string
}

// ESPGetCharacterJournal returns the character's newest wallet journal rows, newest first.
func ESPGetCharacterJournal(ctx context.Context, db *sql.DB, charID int64, limit int) ([]ESPJournalEntry, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT j.refID, COALESCE(j.transDate, 0),
		       COALESCE(rt.refTypeText, CONCAT('refType ', j.refTypeID)),
		       j.amount, j.balance, COALESCE(j.reason, '')
		FROM market_journal j
		LEFT JOIN market_reftypes rt ON rt.refTypeID = j.refTypeID
		WHERE j.characterID = ?
		ORDER BY j.transDate DESC, j.refID DESC
		LIMIT ?`, charID, limit)
	if err != nil {
		return nil, fmt.Errorf("ESPGetCharacterJournal: %w", err)
	}
	defer rows.Close()

	var results []ESPJournalEntry
	for rows.Next() {
		var e ESPJournalEntry
		if err := rows.Scan(&e.RefID, &e.TransDate, &e.RefType, &e.Amount, &e.Balance, &e.Reason); err != nil {
			return nil, fmt.Errorf("ESPGetCharacterJournal scan: %w", err)
		}
		results = append(results, e)
	}
	return results, rows.Err()
}

// ESPGetCharacterSkills returns all skills owned by the character.
func ESPGetCharacterSkills(ctx context.Context, db *sql.DB, charID int64) ([]ESPSkillEntry, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT e.typeID, t.typeName,
		       COALESCE(ea_lv.valueInt, 0) AS skillLevel,
		       COALESCE(ea_sp.valueInt, 0) AS skillPoints
		FROM entity e
		JOIN invTypes t ON t.typeID = e.typeID
		LEFT JOIN entity_attributes ea_lv ON ea_lv.itemID = e.itemID AND ea_lv.attributeID = 280
		LEFT JOIN entity_attributes ea_sp ON ea_sp.itemID = e.itemID AND ea_sp.attributeID = 276
		WHERE e.ownerID = ? AND e.flag IN (7, 61)
		ORDER BY t.typeName`, charID)
	if err != nil {
		return nil, fmt.Errorf("ESPGetCharacterSkills: %w", err)
	}
	defer rows.Close()

	var results []ESPSkillEntry
	for rows.Next() {
		var sk ESPSkillEntry
		if err := rows.Scan(&sk.TypeID, &sk.TypeName, &sk.Level, &sk.SkillPoints); err != nil {
			return nil, fmt.Errorf("ESPGetCharacterSkills scan: %w", err)
		}
		results = append(results, sk)
	}
	return results, rows.Err()
}

// ESPSkillCatalogueRow is one skill type in the full catalogue (every
// published skill in the game), annotated with the character's training state
// when the skill is injected.
type ESPSkillCatalogueRow struct {
	TypeID          int64
	TypeName        string
	Description     string // invTypes.description
	GroupID         int64
	GroupName       string
	Rank            int64 // skillTimeConstant (attr 275)
	PrimaryAttrID   int64 // attr 180 (165=int, 166=mem, 167=per, 168=wil, 164=cha)
	SecondaryAttrID int64 // attr 181
	Known           bool  // character owns the skill book (injected)
	Level           int
	SkillPoints     int64
}

// ESPGetSkillCatalogue returns every published skill type (invGroups
// categoryID 16) grouped by skill group, LEFT-joined against charID's trained
// skills.  Unknown skills come back with Known=false, Level/SP zero.
func ESPGetSkillCatalogue(ctx context.Context, db *sql.DB, charID int64) ([]ESPSkillCatalogueRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT t.typeID, t.typeName, COALESCE(t.description, ''),
		       g.groupID, g.groupName,
		       COALESCE(NULLIF(tc.valueFloat, 0), tc.valueInt, 1),
		       COALESCE(NULLIF(pa.valueFloat, 0), pa.valueInt, 0),
		       COALESCE(NULLIF(sa.valueFloat, 0), sa.valueInt, 0),
		       e.itemID IS NOT NULL,
		       COALESCE(lv.valueInt, 0), COALESCE(sp.valueInt, 0)
		FROM invTypes t
		JOIN invGroups g ON g.groupID = t.groupID AND g.categoryID = 16
		LEFT JOIN dgmTypeAttributes tc ON tc.typeID = t.typeID AND tc.attributeID = 275
		LEFT JOIN dgmTypeAttributes pa ON pa.typeID = t.typeID AND pa.attributeID = 180
		LEFT JOIN dgmTypeAttributes sa ON sa.typeID = t.typeID AND sa.attributeID = 181
		LEFT JOIN entity e ON e.ownerID = ? AND e.typeID = t.typeID AND e.flag IN (7, 61)
		LEFT JOIN entity_attributes lv ON lv.itemID = e.itemID AND lv.attributeID = 280
		LEFT JOIN entity_attributes sp ON sp.itemID = e.itemID AND sp.attributeID = 276
		WHERE t.published = 1
		ORDER BY g.groupName, t.typeName`, charID)
	if err != nil {
		return nil, fmt.Errorf("ESPGetSkillCatalogue: %w", err)
	}
	defer rows.Close()

	var out []ESPSkillCatalogueRow
	for rows.Next() {
		var r ESPSkillCatalogueRow
		var rank, pri, sec float64
		if err := rows.Scan(&r.TypeID, &r.TypeName, &r.Description,
			&r.GroupID, &r.GroupName,
			&rank, &pri, &sec, &r.Known, &r.Level, &r.SkillPoints); err != nil {
			return nil, fmt.Errorf("ESPGetSkillCatalogue scan: %w", err)
		}
		r.Rank = int64(rank)
		if r.Rank < 1 {
			r.Rank = 1
		}
		r.PrimaryAttrID = int64(pri)
		r.SecondaryAttrID = int64(sec)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ESPEffectiveLearningStats returns the character's five effective learning
// attributes (charisma 164 to willpower 168) keyed by attributeID, including
// learning-skill, implant, and Learning-multiplier bonuses.
func ESPEffectiveLearningStats(ctx context.Context, db *sql.DB, charID int64) map[int64]float64 {
	stats := make(map[int64]float64, 5)
	for attrID := int64(164); attrID <= 168; attrID++ {
		stats[attrID] = GetEffectiveCharAttr(ctx, db, charID, attrID)
	}
	return stats
}

// ESPSkillSPPerMinute computes the SP/min training rate for a skill with the
// given primary/secondary learning attributes from a stats map produced by
// ESPEffectiveLearningStats, including the configured skill-training rate.
func ESPSkillSPPerMinute(stats map[int64]float64, primaryAttrID, secondaryAttrID int64) float64 {
	primary := stats[primaryAttrID]
	secondary := 20.0
	if secondaryAttrID > 0 {
		secondary = stats[secondaryAttrID]
	}
	rate := primary + secondary/2.0
	if rate < 1 {
		rate = 30.0
	}
	return rate * rates.SkillTraining
}

// ESPSkillSPForLevel exposes the SP-for-level curve (250 * rank * 32^((n-1)/2)).
func ESPSkillSPForLevel(rank int64, level int) float64 {
	return skillSPForLevel(rank, level)
}

// ESPGetCharacterItemCount returns the number of items owned by the character.
func ESPGetCharacterItemCount(ctx context.Context, db *sql.DB, charID int64) (int, error) {
	var count int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entity WHERE ownerID = ?`, charID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("ESPGetCharacterItemCount: %w", err)
	}
	return count, nil
}

// ESPCorp is a corporationID+name pair for dropdowns.
type ESPCorp struct {
	CorporationID int64
	CorpName      string
}

// ESPGetAllCorps returns all corporations for dropdown selectors.
func ESPGetAllCorps(ctx context.Context, db *sql.DB) ([]ESPCorp, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT corporationID, corporationName FROM corporation ORDER BY corporationName`)
	if err != nil {
		return nil, fmt.Errorf("ESPGetAllCorps: %w", err)
	}
	defer rows.Close()

	var results []ESPCorp
	for rows.Next() {
		var c ESPCorp
		if err := rows.Scan(&c.CorporationID, &c.CorpName); err != nil {
			return nil, fmt.Errorf("ESPGetAllCorps scan: %w", err)
		}
		results = append(results, c)
	}
	return results, rows.Err()
}

// ESPUpdateCharacterCorp updates a character's corporationID in character_ and
// inserts a chrEmployment row. All three date columns are Win32 FILETIME, never
// unix seconds (the client reads those as the year 1601). startDateTime is the
// join date of the current corporation; createDateTime is the birth date, untouched.
func ESPUpdateCharacterCorp(ctx context.Context, db *sql.DB, charID, newCorpID int64) error {
	now := win32TimeNow()
	var oldCorpID int64
	_ = db.QueryRowContext(ctx,
		`SELECT corporationID FROM character_ WHERE characterID = ?`, charID).Scan(&oldCorpID)
	_, err := db.ExecContext(ctx,
		`UPDATE character_ SET corporationID = ?, corporationDateTime = ?, startDateTime = ? WHERE characterID = ?`,
		newCorpID, now, now, charID)
	if err != nil {
		return fmt.Errorf("ESPUpdateCharacterCorp: %w", err)
	}
	// Insert employment record.
	_, _ = db.ExecContext(ctx,
		`INSERT INTO chrEmployment (characterID, corporationID, startDate, deleted) VALUES (?, ?, ?, 0)`,
		charID, newCorpID, now)
	fireCorpMembershipHook(charID, oldCorpID, newCorpID)
	identityChanged(charID)
	return nil
}

// ESPGetCorpDetail loads corporation detail by ID.
func ESPGetCorpDetail(ctx context.Context, db *sql.DB, corpID int64) (*ESPCorpDetail, error) {
	d := &ESPCorpDetail{}
	err := db.QueryRowContext(ctx, `
		SELECT corp.corporationID, corp.corporationName, corp.tickerName,
		       corp.ceoID, COALESCE(c.characterName, ''),
		       corp.stationID,
		       (SELECT COUNT(*) FROM character_ ch WHERE ch.corporationID = corp.corporationID),
		       corp.taxRate
		FROM corporation corp
		LEFT JOIN character_ c ON c.characterID = corp.ceoID
		WHERE corp.corporationID = ?`, corpID,
	).Scan(&d.CorporationID, &d.CorporationName, &d.TickerName,
		&d.CEOID, &d.CEOName, &d.StationID, &d.MemberCount, &d.TaxRate)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("corporation %d not found", corpID)
		}
		return nil, fmt.Errorf("ESPGetCorpDetail: %w", err)
	}
	return d, nil
}

// ESPGetCorpMembers returns a corporation's player members. accountID > 0 is the
// player/NPC separator: NPC agents live in character_ with a corporationID too,
// so without it an NPC corp's member list is its agent roster.
func ESPGetCorpMembers(ctx context.Context, db *sql.DB, corpID int64) ([]ESPCorpMember, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT c.characterID, c.characterName, c.stationID,
		       COALESCE(s.stationName, ''), c.balance
		FROM character_ c
		LEFT JOIN staStations s ON s.stationID = c.stationID
		WHERE c.corporationID = ? AND c.accountID > 0
		ORDER BY c.characterName
		LIMIT 200`, corpID)
	if err != nil {
		return nil, fmt.Errorf("ESPGetCorpMembers: %w", err)
	}
	defer rows.Close()

	var results []ESPCorpMember
	for rows.Next() {
		var m ESPCorpMember
		if err := rows.Scan(&m.CharacterID, &m.CharacterName, &m.StationID, &m.StationName, &m.Balance); err != nil {
			return nil, fmt.Errorf("ESPGetCorpMembers scan: %w", err)
		}
		results = append(results, m)
	}
	return results, rows.Err()
}

// ── ESP Dungeon types ────────────────────────────────────────────────────────

// ESPFaction is a faction entry for dropdowns.
type ESPFaction struct {
	FactionID   int64
	FactionName string
}

// ESPDungeonSummary is a dungeon row for the list page.
type ESPDungeonSummary struct {
	DungeonVID   int64
	DungeonVName string
	FactionID    int64
	FactionName  string
	Version      int64
	Status       int64
	RoomCount    int
}

// ESPDungeonDetail is the full dungeon detail for the view page.
type ESPDungeonDetail struct {
	DungeonVID   int64
	DungeonVName string
	FactionID    int64
	FactionName  string
	Version      int64
	Status       int64
	DEDLevel     int64 // 0 = unrated
	Rooms        []ESPRoomDetail
}

// ESPRoomDetail is a room with its objects and logic triggers.
type ESPRoomDetail struct {
	RoomID           int64
	DungeonVID       int64
	RoomName         string
	NaturalPhenomena bool
	Objects          []ESPObjectRow
	Triggers         []TriggerRow
}

// ESPObjectRow is a dungeon object with type name resolved.
type ESPObjectRow struct {
	ObjectID         int64
	RoomID           int64
	TypeID           int64
	TypeName         string
	X, Y, Z          float64
	Yaw, Pitch, Roll float64
	Radius           float64
	AggressionRange  float64
	Stance           string // "" = aggressive (the default)
	CategoryID       int64
	GroupID          int64
	NPCFactionID     int64
	NPCFactionName   string
	MusicUrl         string
	LeashObjectID    int64
	OrbitDistance    float64
	Description      string
	SpawnWave        int64
	BallName         string
	Invulnerable     bool
	HPScale          float64 // 1 = the type's own HP
	Solid            bool    // collidable, resolved (override, else DunSolidByDefault)
	SolidAuthored    bool    // Solid is the author's override, not the type default
	SolidDefault     bool    // what the type default alone would be
	LootMinDrops     int64   // 0 = no floor on authored drops
	LootMaxDrops     int64   // 0 = no cap on authored drops
	Loot             []DunObjectLootRow
}

// ESPDETypeRow is an item type row for the dungeon editor item browser.
type ESPDETypeRow struct {
	TypeID       int64
	TypeName     string
	GroupID      int64
	GroupName    string
	CategoryID   int64
	CategoryName string
	Radius       float64
	Seedable     bool    // passes SeedNPCMarketOrders' published/marketGroupID filter
	Bounty       float64 // entityKillBounty (attr 481), unscaled by rates; 0 = none
}

// ESPDECategory is a category for the dungeon editor browser.
type ESPDECategory struct {
	CategoryID   int64
	CategoryName string
}

// ESPDEGroup is a group for the dungeon editor browser.
type ESPDEGroup struct {
	GroupID    int64
	GroupName  string
	CategoryID int64
}

// ── ESP Dungeon query functions ──────────────────────────────────────────────

// ESPGetAllFactions returns all factions from eveStaticOwners for dropdowns.
func ESPGetAllFactions(ctx context.Context, db *sql.DB) ([]ESPFaction, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT ownerID, ownerName FROM eveStaticOwners WHERE typeID IN (30, 500) ORDER BY ownerName`)
	if err != nil {
		return nil, fmt.Errorf("ESPGetAllFactions: %w", err)
	}
	defer rows.Close()

	var results []ESPFaction
	for rows.Next() {
		var f ESPFaction
		if err := rows.Scan(&f.FactionID, &f.FactionName); err != nil {
			return nil, fmt.Errorf("ESPGetAllFactions scan: %w", err)
		}
		results = append(results, f)
	}
	return results, rows.Err()
}

// ESPNPCDivision is one NPC corporation division: the axis agtAgents.divisionID
// keys, shared by the mission type weights and the offer allow-lists.
type ESPNPCDivision struct {
	DivisionID   int64
	DivisionName string
	Agents       int // mission agents staffing it, for the offer editor's sense of reach
}

// ESPGetNPCDivisions returns every division with its mission-agent headcount.
// A division nobody staffs can still be ticked on an offer, so the count is
// shown rather than the row hidden.
func ESPGetNPCDivisions(ctx context.Context, db *sql.DB) ([]ESPNPCDivision, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT d.divisionID, d.divisionName, COALESCE(a.n, 0)
		FROM crpNPCDivisions d
		LEFT JOIN (SELECT divisionID, COUNT(*) AS n FROM agtAgents
		           WHERE agentTypeID NOT IN (1, ?) GROUP BY divisionID) a
		       ON a.divisionID = d.divisionID
		ORDER BY d.divisionName`, AgentTypeResearch)
	if err != nil {
		return nil, fmt.Errorf("ESPGetNPCDivisions: %w", err)
	}
	defer rows.Close()
	var out []ESPNPCDivision
	for rows.Next() {
		var d ESPNPCDivision
		if err := rows.Scan(&d.DivisionID, &d.DivisionName, &d.Agents); err != nil {
			return nil, fmt.Errorf("ESPGetNPCDivisions scan: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ESPAgentCorporation is an NPC corporation that staffs agents, the only kind an
// offer can be narrowed to.
type ESPAgentCorporation struct {
	CorporationID int64
	CorpName      string
	FactionID     int64
	Agents        int
}

// ESPGetAgentCorporations lists NPC corporations with at least one mission
// agent, with their faction, for the offer editor's corporation narrowing.
func ESPGetAgentCorporations(ctx context.Context, db *sql.DB) ([]ESPAgentCorporation, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT a.corporationID, COALESCE(n.itemName, ''), COALESCE(c.factionID, 0), COUNT(*)
		FROM agtAgents a
		LEFT JOIN crpNPCCorporations c ON c.corporationID = a.corporationID
		LEFT JOIN eveNames n ON n.itemID = a.corporationID
		WHERE a.agentTypeID NOT IN (1, ?) AND a.corporationID > 0
		GROUP BY a.corporationID, n.itemName, c.factionID
		ORDER BY n.itemName`, AgentTypeResearch)
	if err != nil {
		return nil, fmt.Errorf("ESPGetAgentCorporations: %w", err)
	}
	defer rows.Close()
	var out []ESPAgentCorporation
	for rows.Next() {
		var c ESPAgentCorporation
		if err := rows.Scan(&c.CorporationID, &c.CorpName, &c.FactionID, &c.Agents); err != nil {
			return nil, fmt.Errorf("ESPGetAgentCorporations scan: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ESPGetAllDungeons returns all dungeons with room counts and faction names, optionally filtered by factionID.
func ESPGetAllDungeons(ctx context.Context, db *sql.DB, factionID int64) ([]ESPDungeonSummary, error) {
	var rows *sql.Rows
	var err error
	if factionID > 0 {
		rows, err = db.QueryContext(ctx, `
			SELECT d.dungeonVID, d.dungeonVName, d.factionID, COALESCE(f.ownerName, ''), d.version, d.status,
			       (SELECT COUNT(*) FROM dunRooms r WHERE r.dungeonVID = d.dungeonVID) AS roomCount
			FROM dunDungeons d
			LEFT JOIN eveStaticOwners f ON f.ownerID = d.factionID AND f.typeID IN (30, 500)
			WHERE d.factionID = ?
			ORDER BY d.dungeonVName
			LIMIT 500`, factionID)
	} else {
		rows, err = db.QueryContext(ctx, `
			SELECT d.dungeonVID, d.dungeonVName, d.factionID, COALESCE(f.ownerName, ''), d.version, d.status,
			       (SELECT COUNT(*) FROM dunRooms r WHERE r.dungeonVID = d.dungeonVID) AS roomCount
			FROM dunDungeons d
			LEFT JOIN eveStaticOwners f ON f.ownerID = d.factionID AND f.typeID IN (30, 500)
			ORDER BY d.dungeonVName
			LIMIT 500`)
	}
	if err != nil {
		return nil, fmt.Errorf("ESPGetAllDungeons: %w", err)
	}
	defer rows.Close()

	var results []ESPDungeonSummary
	for rows.Next() {
		var d ESPDungeonSummary
		if err := rows.Scan(&d.DungeonVID, &d.DungeonVName, &d.FactionID, &d.FactionName, &d.Version, &d.Status, &d.RoomCount); err != nil {
			return nil, fmt.Errorf("ESPGetAllDungeons scan: %w", err)
		}
		results = append(results, d)
	}
	return results, rows.Err()
}

// ESPGetDungeonFactions returns distinct factions used by dungeons, with names resolved.
func ESPGetDungeonFactions(ctx context.Context, db *sql.DB) ([]ESPFaction, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT DISTINCT d.factionID, COALESCE(f.ownerName, CONCAT('Faction ', d.factionID))
		FROM dunDungeons d
		LEFT JOIN eveStaticOwners f ON f.ownerID = d.factionID AND f.typeID IN (30, 500)
		ORDER BY COALESCE(f.ownerName, CONCAT('Faction ', d.factionID))`)
	if err != nil {
		return nil, fmt.Errorf("ESPGetDungeonFactions: %w", err)
	}
	defer rows.Close()

	var results []ESPFaction
	for rows.Next() {
		var f ESPFaction
		if err := rows.Scan(&f.FactionID, &f.FactionName); err != nil {
			return nil, fmt.Errorf("ESPGetDungeonFactions scan: %w", err)
		}
		results = append(results, f)
	}
	return results, rows.Err()
}

// ESPGetDungeonDetail loads a dungeon with all rooms and objects.
func ESPGetDungeonDetail(ctx context.Context, db *sql.DB, dungeonVID int64) (*ESPDungeonDetail, error) {
	d := &ESPDungeonDetail{}
	err := db.QueryRowContext(ctx, `
		SELECT d.dungeonVID, d.dungeonVName, d.factionID, COALESCE(f.ownerName, ''), d.version, d.status,
		       COALESCE(d.dedLevel, 0)
		FROM dunDungeons d
		LEFT JOIN eveStaticOwners f ON f.ownerID = d.factionID AND f.typeID IN (30, 500)
		WHERE d.dungeonVID = ?`,
		dungeonVID).Scan(&d.DungeonVID, &d.DungeonVName, &d.FactionID, &d.FactionName, &d.Version, &d.Status,
		&d.DEDLevel)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("dungeon %d not found", dungeonVID)
		}
		return nil, fmt.Errorf("ESPGetDungeonDetail: %w", err)
	}

	// Load rooms.
	roomRows, err := db.QueryContext(ctx,
		`SELECT roomID, dungeonVID, roomName, COALESCE(naturalPhenomena, 1)
		 FROM dunRooms WHERE dungeonVID = ? ORDER BY COALESCE(roomOrder, roomID), roomID`,
		dungeonVID)
	if err != nil {
		return nil, fmt.Errorf("ESPGetDungeonDetail rooms: %w", err)
	}
	defer roomRows.Close()

	for roomRows.Next() {
		var rm ESPRoomDetail
		var phenomena int
		if err := roomRows.Scan(&rm.RoomID, &rm.DungeonVID, &rm.RoomName, &phenomena); err != nil {
			return nil, fmt.Errorf("ESPGetDungeonDetail room scan: %w", err)
		}
		rm.NaturalPhenomena = phenomena != 0
		d.Rooms = append(d.Rooms, rm)
	}
	roomRows.Close()

	// Load objects per room with type names.
	for i := range d.Rooms {
		objs, err := queryESPObjects(ctx, db, `o.roomID = ?`, d.Rooms[i].RoomID)
		if err != nil {
			return nil, fmt.Errorf("ESPGetDungeonDetail objects: %w", err)
		}
		d.Rooms[i].Objects = objs

		// Authored per-ball drops, one query for the whole room.
		lootByObj, lerr := GetRoomObjectLoot(ctx, db, d.Rooms[i].RoomID)
		if lerr != nil {
			return nil, fmt.Errorf("ESPGetDungeonDetail loot: %w", lerr)
		}
		for j := range d.Rooms[i].Objects {
			d.Rooms[i].Objects[j].Loot = lootByObj[d.Rooms[i].Objects[j].ObjectID]
		}

		trigs, terr := GetRoomTriggers(ctx, db, d.Rooms[i].RoomID)
		if terr != nil {
			return nil, fmt.Errorf("ESPGetDungeonDetail triggers: %w", terr)
		}
		d.Rooms[i].Triggers = trigs
	}

	return d, nil
}

// espObjectSelect is the editor's view of a dunObjects row: authored columns
// with NULLs flattened, plus the type, group and faction names it is drawn with.
const espObjectSelect = `
	SELECT o.objectID, o.roomID, o.typeID, COALESCE(t.typeName, ''),
	       o.x, o.y, o.z,
	       COALESCE(o.yaw, 0), COALESCE(o.pitch, 0), COALESCE(o.roll, 0),
	       COALESCE(o.radius, t.radius, 0), COALESCE(o.aggressionRange, 0),
	       COALESCE(o.stance, ''),
	       COALESCE(g.categoryID, 0), COALESCE(t.groupID, 0),
	       COALESCE(o.npcFactionID, 0), COALESCE(nf.ownerName, ''),
	       COALESCE(o.musicUrl, ''),
	       COALESCE(o.leashObjectID, 0), COALESCE(o.orbitDistance, 0),
	       COALESCE(o.description, ''), COALESCE(o.spawnWave, 0),
	       COALESCE(o.ballName, ''), o.invulnerable, COALESCE(o.hpScale, 1), o.solid,
	       COALESCE(o.lootMinDrops, 0), COALESCE(o.lootMaxDrops, 0)
	FROM dunObjects o
	LEFT JOIN invTypes t ON t.typeID = o.typeID
	LEFT JOIN invGroups g ON g.groupID = t.groupID
	LEFT JOIN eveStaticOwners nf ON nf.ownerID = o.npcFactionID AND nf.typeID IN (30, 500)`

// queryESPObjects runs espObjectSelect with a WHERE clause, ordered by objectID.
// Loot is not filled in.
func queryESPObjects(ctx context.Context, q DBTX, where string, args ...any) ([]ESPObjectRow, error) {
	rows, err := q.QueryContext(ctx, espObjectSelect+` WHERE `+where+` ORDER BY o.objectID`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ESPObjectRow
	for rows.Next() {
		var obj ESPObjectRow
		var solid sql.NullBool
		if err := rows.Scan(&obj.ObjectID, &obj.RoomID, &obj.TypeID, &obj.TypeName,
			&obj.X, &obj.Y, &obj.Z,
			&obj.Yaw, &obj.Pitch, &obj.Roll,
			&obj.Radius, &obj.AggressionRange, &obj.Stance,
			&obj.CategoryID, &obj.GroupID,
			&obj.NPCFactionID, &obj.NPCFactionName,
			&obj.MusicUrl,
			&obj.LeashObjectID, &obj.OrbitDistance, &obj.Description, &obj.SpawnWave,
			&obj.BallName, &obj.Invulnerable, &obj.HPScale, &solid, &obj.LootMinDrops, &obj.LootMaxDrops); err != nil {
			return nil, fmt.Errorf("object scan: %w", err)
		}
		obj.SolidDefault = DunSolidByDefault(obj.TypeID, obj.GroupID)
		obj.Solid = obj.SolidDefault
		if solid.Valid {
			obj.Solid, obj.SolidAuthored = solid.Bool, true
		}
		out = append(out, obj)
	}
	return out, rows.Err()
}

// ESPGetObjectsByID returns the editor view of the given dunObjects rows.
func ESPGetObjectsByID(ctx context.Context, q DBTX, ids []int64) ([]ESPObjectRow, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ph, args := int64Placeholders(ids)
	objs, err := queryESPObjects(ctx, q, `o.objectID IN (`+ph+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("ESPGetObjectsByID: %w", err)
	}
	return objs, nil
}

// Type-search scopes for ESPSearchTypes. The dungeon editor wants only things
// that can be placed in space; the market and contraband tools work on the other
// half of the item tree (modules, charges, commodities) and keep its exclusions off.
const (
	TypeScopeSpace  = "space"  // dungeon editor: no skills/modules/blueprints/...
	TypeScopeMarket = "market" // anything an NPC order can carry: published, in a market group
	TypeScopeAll    = "all"    // every invTypes row
)

// ESPSearchDETypes searches invTypes by name for the dungeon editor item browser,
// excluding non-space categories.
func ESPSearchDETypes(ctx context.Context, db *sql.DB, query string) ([]ESPDETypeRow, error) {
	return ESPSearchTypes(ctx, db, query, TypeScopeSpace)
}

// ESPSearchTypes searches invTypes by name, restricted by scope.  An unknown
// scope is treated as TypeScopeSpace so existing callers keep their behaviour.
func ESPSearchTypes(ctx context.Context, db *sql.DB, query, scope string) ([]ESPDETypeRow, error) {
	where := `COALESCE(g.categoryID, 0) NOT IN (` + excludedDECategories + `)`
	order := `t.typeName`
	switch scope {
	case TypeScopeMarket, TypeScopeAll:
		// No category filter here. Placeable rows (published, in a market group) sort first;
		// variants parked at marketGroupID = 0 until MigrateMarketVariantVisibility stay listed.
		where = `1`
		if scope == TypeScopeMarket {
			order = `seedable DESC, t.typeName`
		}
	}
	rows, err := db.QueryContext(ctx, `
		SELECT t.typeID, t.typeName, COALESCE(t.groupID, 0),
		       COALESCE(g.groupName, ''), COALESCE(g.categoryID, 0),
		       COALESCE(c.categoryName, ''), COALESCE(t.radius, 100),
		       COALESCE(t.published = 1 AND t.marketGroupID IS NOT NULL, 0) AS seedable,
		       COALESCE(b.valueInt, b.valueFloat, 0) AS bounty
		FROM invTypes t
		LEFT JOIN invGroups g ON g.groupID = t.groupID
		LEFT JOIN invCategories c ON c.categoryID = g.categoryID
		LEFT JOIN dgmTypeAttributes b ON b.typeID = t.typeID AND b.attributeID = 481
		WHERE t.typeName LIKE ? AND `+where+`
		ORDER BY `+order+`
		LIMIT 200`, "%"+query+"%")
	if err != nil {
		return nil, fmt.Errorf("ESPSearchTypes: %w", err)
	}
	defer rows.Close()

	var results []ESPDETypeRow
	for rows.Next() {
		var r ESPDETypeRow
		if err := rows.Scan(&r.TypeID, &r.TypeName, &r.GroupID, &r.GroupName, &r.CategoryID,
			&r.CategoryName, &r.Radius, &r.Seedable, &r.Bounty); err != nil {
			return nil, fmt.Errorf("ESPSearchTypes scan: %w", err)
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// ESPGetDECategories returns item categories for the dungeon editor browser,
// excluding non-space categories.
func ESPGetDECategories(ctx context.Context, db *sql.DB) ([]ESPDECategory, error) {
	rows, err := db.QueryContext(ctx, `SELECT categoryID, categoryName FROM invCategories WHERE categoryID NOT IN (`+excludedDECategories+`) ORDER BY categoryName`)
	if err != nil {
		return nil, fmt.Errorf("ESPGetDECategories: %w", err)
	}
	defer rows.Close()

	var results []ESPDECategory
	for rows.Next() {
		var c ESPDECategory
		if err := rows.Scan(&c.CategoryID, &c.CategoryName); err != nil {
			return nil, fmt.Errorf("ESPGetDECategories scan: %w", err)
		}
		results = append(results, c)
	}
	return results, rows.Err()
}

// ESPGetDEGroups returns groups within a category for the dungeon editor browser.
func ESPGetDEGroups(ctx context.Context, db *sql.DB, categoryID int64) ([]ESPDEGroup, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT groupID, groupName, categoryID FROM invGroups WHERE categoryID = ? ORDER BY groupName`,
		categoryID)
	if err != nil {
		return nil, fmt.Errorf("ESPGetDEGroups: %w", err)
	}
	defer rows.Close()

	var results []ESPDEGroup
	for rows.Next() {
		var g ESPDEGroup
		if err := rows.Scan(&g.GroupID, &g.GroupName, &g.CategoryID); err != nil {
			return nil, fmt.Errorf("ESPGetDEGroups scan: %w", err)
		}
		results = append(results, g)
	}
	return results, rows.Err()
}

// ESPGetDETypes returns types within a group for the dungeon editor browser,
// excluding non-space categories.
func ESPGetDETypes(ctx context.Context, db *sql.DB, groupID int64) ([]ESPDETypeRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT t.typeID, t.typeName, COALESCE(t.groupID, 0),
		       COALESCE(g.groupName, ''), COALESCE(g.categoryID, 0),
		       COALESCE(c.categoryName, ''), COALESCE(t.radius, 100)
		FROM invTypes t
		LEFT JOIN invGroups g ON g.groupID = t.groupID
		LEFT JOIN invCategories c ON c.categoryID = g.categoryID
		WHERE t.groupID = ? AND COALESCE(g.categoryID, 0) NOT IN (`+excludedDECategories+`)
		ORDER BY t.typeName
		LIMIT 500`, groupID)
	if err != nil {
		return nil, fmt.Errorf("ESPGetDETypes: %w", err)
	}
	defer rows.Close()

	var results []ESPDETypeRow
	for rows.Next() {
		var r ESPDETypeRow
		if err := rows.Scan(&r.TypeID, &r.TypeName, &r.GroupID, &r.GroupName, &r.CategoryID, &r.CategoryName, &r.Radius); err != nil {
			return nil, fmt.Errorf("ESPGetDETypes scan: %w", err)
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// ── ESP NPC Browser types ─────────────────────────────────────────────────────

// ESPNPCTypeRow is a search result row for NPC types.
type ESPNPCTypeRow struct {
	TypeID    int64
	TypeName  string
	GroupID   int64
	GroupName string
	GraphicID int64
	RaceID    int64
	Published int64
	Icon      string // from eveGraphics.icon (e.g. "09_07")
	Bounty    float64
}

// ESPNPCTypeDetail is the full detail for a single NPC type.
type ESPNPCTypeDetail struct {
	TypeID       int64
	TypeName     string
	GroupID      int64
	GroupName    string
	CategoryID   int64
	CategoryName string
	Description  string
	Mass         float64
	Radius       float64
	Volume       float64
	Capacity     float64
	RaceID       int64
	GraphicID    int64
	Published    int64
	Icon         string // from eveGraphics.icon
}

// ESPNPCAttribute is a single dgmTypeAttributes row with name resolved.
type ESPNPCAttribute struct {
	AttributeID   int64
	AttributeName string
	Description   string
	Value         float64
}

// ESPNPCGroup is a ship group for filter dropdowns.
type ESPNPCGroup struct {
	GroupID   int64
	GroupName string
}

// ── ESP NPC Browser query functions ──────────────────────────────────────────

// ESPSearchNPCTypes searches invTypes for NPC entity types (categoryID=11).
// sortBy "bounty" orders by kill bounty (attribute 481) descending; anything
// else orders by type name.
func ESPSearchNPCTypes(ctx context.Context, db *sql.DB, query string, groupID int64, sortBy string) ([]ESPNPCTypeRow, error) {
	q := `
		SELECT t.typeID, t.typeName, t.groupID, g.groupName,
		       COALESCE(t.graphicID, 0), COALESCE(t.raceID, 0), COALESCE(t.published, 0),
		       COALESCE(eg.icon, ''),
		       COALESCE(b.valueInt, b.valueFloat, 0) AS bounty
		FROM invTypes t
		JOIN invGroups g ON g.groupID = t.groupID
		LEFT JOIN eveGraphics eg ON eg.graphicID = t.graphicID
		LEFT JOIN dgmTypeAttributes b ON b.typeID = t.typeID AND b.attributeID = 481
		WHERE t.typeName LIKE ? AND g.categoryID = 11`
	args := []any{"%" + query + "%"}
	if groupID > 0 {
		q += ` AND t.groupID = ?`
		args = append(args, groupID)
	}
	if sortBy == "bounty" {
		q += ` ORDER BY bounty DESC, t.typeName LIMIT 100`
	} else {
		q += ` ORDER BY t.typeName LIMIT 100`
	}
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("ESPSearchNPCTypes: %w", err)
	}
	defer rows.Close()

	var results []ESPNPCTypeRow
	for rows.Next() {
		var r ESPNPCTypeRow
		if err := rows.Scan(&r.TypeID, &r.TypeName, &r.GroupID, &r.GroupName,
			&r.GraphicID, &r.RaceID, &r.Published, &r.Icon, &r.Bounty); err != nil {
			return nil, fmt.Errorf("ESPSearchNPCTypes scan: %w", err)
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// ESPGetNPCType loads full detail for a single NPC typeID.
func ESPGetNPCType(ctx context.Context, db *sql.DB, typeID int64) (*ESPNPCTypeDetail, error) {
	d := &ESPNPCTypeDetail{}
	err := db.QueryRowContext(ctx, `
		SELECT t.typeID, t.typeName, t.groupID, COALESCE(g.groupName, ''),
		       COALESCE(g.categoryID, 0), COALESCE(c.categoryName, ''),
		       COALESCE(t.description, ''), COALESCE(t.mass, 0), COALESCE(t.radius, 0),
		       COALESCE(t.volume, 0), COALESCE(t.capacity, 0),
		       COALESCE(t.raceID, 0), COALESCE(t.graphicID, 0),
		       COALESCE(t.published, 0),
		       COALESCE(eg.icon, '')
		FROM invTypes t
		LEFT JOIN invGroups g ON g.groupID = t.groupID
		LEFT JOIN invCategories c ON c.categoryID = g.categoryID
		LEFT JOIN eveGraphics eg ON eg.graphicID = t.graphicID
		WHERE t.typeID = ?`, typeID,
	).Scan(&d.TypeID, &d.TypeName, &d.GroupID, &d.GroupName,
		&d.CategoryID, &d.CategoryName, &d.Description,
		&d.Mass, &d.Radius, &d.Volume, &d.Capacity,
		&d.RaceID, &d.GraphicID, &d.Published, &d.Icon)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("NPC type %d not found", typeID)
		}
		return nil, fmt.Errorf("ESPGetNPCType: %w", err)
	}
	return d, nil
}

// ESPGetTypeAttributes returns all dgmTypeAttributes for a typeID with attribute names.
func ESPGetTypeAttributes(ctx context.Context, db *sql.DB, typeID int64) ([]ESPNPCAttribute, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT ta.attributeID, COALESCE(at.attributeName, '?'), COALESCE(at.description, ''),
		       COALESCE(NULLIF(ta.valueFloat, 0), CAST(ta.valueInt AS DOUBLE), ta.valueFloat)
		FROM dgmTypeAttributes ta
		LEFT JOIN dgmAttributeTypes at ON at.attributeID = ta.attributeID
		WHERE ta.typeID = ?
		ORDER BY ta.attributeID`, typeID)
	if err != nil {
		return nil, fmt.Errorf("ESPGetTypeAttributes: %w", err)
	}
	defer rows.Close()

	var results []ESPNPCAttribute
	for rows.Next() {
		var a ESPNPCAttribute
		if err := rows.Scan(&a.AttributeID, &a.AttributeName, &a.Description, &a.Value); err != nil {
			return nil, fmt.Errorf("ESPGetTypeAttributes scan: %w", err)
		}
		results = append(results, a)
	}
	return results, rows.Err()
}

// ESPGetNPCGroups returns all entity groups (categoryID=11) for filter dropdowns.
func ESPGetNPCGroups(ctx context.Context, db *sql.DB) ([]ESPNPCGroup, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT groupID, groupName FROM invGroups WHERE categoryID = 11 ORDER BY groupName`)
	if err != nil {
		return nil, fmt.Errorf("ESPGetNPCGroups: %w", err)
	}
	defer rows.Close()

	var results []ESPNPCGroup
	for rows.Next() {
		var g ESPNPCGroup
		if err := rows.Scan(&g.GroupID, &g.GroupName); err != nil {
			return nil, fmt.Errorf("ESPGetNPCGroups scan: %w", err)
		}
		results = append(results, g)
	}
	return results, rows.Err()
}

// ESPUpdateTypeAttribute updates or inserts a dgmTypeAttributes row. valueInt is
// written NULL, not 0: the config.BulkData.dgmtypeattribs cachable selects
// IF(valueInt IS NULL, valueFloat, valueInt), so a zero valueInt ships the
// attribute to clients as 0 whatever valueFloat holds.
func ESPUpdateTypeAttribute(ctx context.Context, db *sql.DB, typeID, attributeID int64, value float64) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO dgmTypeAttributes (typeID, attributeID, valueInt, valueFloat)
		VALUES (?, ?, NULL, ?)
		ON DUPLICATE KEY UPDATE valueInt = NULL, valueFloat = ?`, typeID, attributeID, value, value)
	if err != nil {
		return fmt.Errorf("ESPUpdateTypeAttribute: %w", err)
	}
	InvalidateTypeStatics(typeID)
	InvalidateNPCSecurityBonus(typeID)
	return nil
}

// ESPDeleteTypeAttribute removes a dgmTypeAttributes row.
func ESPDeleteTypeAttribute(ctx context.Context, db *sql.DB, typeID, attributeID int64) error {
	_, err := db.ExecContext(ctx, `
		DELETE FROM dgmTypeAttributes WHERE typeID = ? AND attributeID = ?`, typeID, attributeID)
	if err != nil {
		return fmt.Errorf("ESPDeleteTypeAttribute: %w", err)
	}
	InvalidateTypeStatics(typeID)
	InvalidateNPCSecurityBonus(typeID)
	return nil
}

// ── ESP Agent Browser types ─────────────────────────────────────────────────

// ESPAgentRow is a search result row for agents.
type ESPAgentRow struct {
	AgentID       int64
	AgentName     string
	AgentTypeID   int64
	DivisionID    int64
	Level         int64
	StationID     int64
	StationName   string
	BloodlineID   int64
	Quality       int64
	CorporationID int64
	CorpName      string
	Gender        int64
}

// ESPAgentDetail is the full detail for a single agent.
type ESPAgentDetail struct {
	AgentID       int64
	AgentName     string
	AgentTypeID   int64
	DivisionID    int64
	DivisionName  string
	Level         int64
	StationID     int64
	StationName   string
	SystemName    string
	BloodlineID   int64
	BloodlineName string
	Quality       int64
	CorporationID int64
	CorpName      string
	Gender        int64
}

// ESPAgentDivision is a crpNPCDivisions row for filter dropdowns.
type ESPAgentDivision struct {
	DivisionID   int64
	DivisionName string
}

// ESPAgentBloodline is a chrBloodlines row for dropdowns.
type ESPAgentBloodline struct {
	BloodlineID   int64
	BloodlineName string
}

// ESPAgentCorp is an NPC corporation used by agents for dropdowns.
type ESPAgentCorp struct {
	CorporationID int64
	CorpName      string
}

// ESPAgentStation is a station that has agents assigned, for dropdowns.
type ESPAgentStation struct {
	StationID   int64
	StationName string
}

// ESPAgentType is a distinct agent type value from the agents table.
type ESPAgentType struct {
	AgentTypeID int64
}

// ── ESP Agent Browser query functions ───────────────────────────────────────

// ESPSearchAgents searches agtAgents with optional filters.
func ESPSearchAgents(ctx context.Context, db *sql.DB, corpID, divisionID, stationID int64) ([]ESPAgentRow, error) {
	query := `
		SELECT a.agentID, COALESCE(an.ownerName, ''),
		       COALESCE(a.agentTypeID, 0), COALESCE(a.divisionID, 0), COALESCE(a.level, 0),
		       COALESCE(a.stationID, 0), COALESCE(s.stationName, ''),
		       COALESCE(a.bloodlineID, 0), COALESCE(a.quality, 0), COALESCE(a.corporationID, 0),
		       COALESCE(c.corporationName, COALESCE(eso.ownerName, '')), COALESCE(a.gender, 0)
		FROM agtAgents a
		LEFT JOIN eveStaticOwners an ON an.ownerID = a.agentID
		LEFT JOIN staStations s ON s.stationID = a.stationID
		LEFT JOIN corporation c ON c.corporationID = a.corporationID
		LEFT JOIN eveStaticOwners eso ON eso.ownerID = a.corporationID
		WHERE 1=1`
	var args []any

	if corpID > 0 {
		query += ` AND a.corporationID = ?`
		args = append(args, corpID)
	}
	if divisionID > 0 {
		query += ` AND a.divisionID = ?`
		args = append(args, divisionID)
	}
	if stationID > 0 {
		query += ` AND a.stationID = ?`
		args = append(args, stationID)
	}
	query += ` ORDER BY a.agentID LIMIT 200`

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("ESPSearchAgents: %w", err)
	}
	defer rows.Close()

	var results []ESPAgentRow
	for rows.Next() {
		var r ESPAgentRow
		if err := rows.Scan(&r.AgentID, &r.AgentName,
			&r.AgentTypeID, &r.DivisionID, &r.Level,
			&r.StationID, &r.StationName, &r.BloodlineID, &r.Quality,
			&r.CorporationID, &r.CorpName, &r.Gender); err != nil {
			return nil, fmt.Errorf("ESPSearchAgents scan: %w", err)
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// ESPGetAgentCorps returns all NPC corporations that have agents assigned.
func ESPGetAgentCorps(ctx context.Context, db *sql.DB) ([]ESPAgentCorp, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT DISTINCT a.corporationID,
		       COALESCE(c.corporationName, COALESCE(eso.ownerName, CAST(a.corporationID AS CHAR)))
		FROM agtAgents a
		LEFT JOIN corporation c ON c.corporationID = a.corporationID
		LEFT JOIN eveStaticOwners eso ON eso.ownerID = a.corporationID
		ORDER BY 2`)
	if err != nil {
		return nil, fmt.Errorf("ESPGetAgentCorps: %w", err)
	}
	defer rows.Close()

	var results []ESPAgentCorp
	for rows.Next() {
		var r ESPAgentCorp
		if err := rows.Scan(&r.CorporationID, &r.CorpName); err != nil {
			return nil, fmt.Errorf("ESPGetAgentCorps scan: %w", err)
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// ESPGetAgentStations returns stations that have agents assigned,
// optionally filtered by corporationID.
func ESPGetAgentStations(ctx context.Context, db *sql.DB, corpID ...int64) ([]ESPAgentStation, error) {
	var rows *sql.Rows
	var err error
	if len(corpID) > 0 && corpID[0] > 0 {
		rows, err = db.QueryContext(ctx, `
			SELECT DISTINCT a.stationID, COALESCE(s.stationName, CAST(a.stationID AS CHAR))
			FROM agtAgents a
			LEFT JOIN staStations s ON s.stationID = a.stationID
			WHERE a.stationID IS NOT NULL AND a.corporationID = ?
			ORDER BY 2`, corpID[0])
	} else {
		rows, err = db.QueryContext(ctx, `
			SELECT DISTINCT a.stationID, COALESCE(s.stationName, CAST(a.stationID AS CHAR))
			FROM agtAgents a
			LEFT JOIN staStations s ON s.stationID = a.stationID
			WHERE a.stationID IS NOT NULL
			ORDER BY 2`)
	}
	if err != nil {
		return nil, fmt.Errorf("ESPGetAgentStations: %w", err)
	}
	defer rows.Close()

	var results []ESPAgentStation
	for rows.Next() {
		var r ESPAgentStation
		if err := rows.Scan(&r.StationID, &r.StationName); err != nil {
			return nil, fmt.Errorf("ESPGetAgentStations scan: %w", err)
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// ESPGetAgentTypes returns the distinct agentTypeID values from the agents table.
func ESPGetAgentTypes(ctx context.Context, db *sql.DB) ([]ESPAgentType, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT DISTINCT agentTypeID FROM agtAgents ORDER BY agentTypeID`)
	if err != nil {
		return nil, fmt.Errorf("ESPGetAgentTypes: %w", err)
	}
	defer rows.Close()

	var results []ESPAgentType
	for rows.Next() {
		var t ESPAgentType
		if err := rows.Scan(&t.AgentTypeID); err != nil {
			return nil, fmt.Errorf("ESPGetAgentTypes scan: %w", err)
		}
		results = append(results, t)
	}
	return results, rows.Err()
}

// ESPGetAgent loads full detail for a single agent by agentID.
func ESPGetAgent(ctx context.Context, db *sql.DB, agentID int64) (*ESPAgentDetail, error) {
	d := &ESPAgentDetail{}
	err := db.QueryRowContext(ctx, `
		SELECT a.agentID, COALESCE(an.ownerName, ''),
		       COALESCE(a.agentTypeID, 0), COALESCE(a.divisionID, 0),
		       COALESCE(nd.divisionName, ''),
		       COALESCE(a.level, 0), COALESCE(a.stationID, 0),
		       COALESCE(s.stationName, ''),
		       COALESCE(sys.solarSystemName, ''),
		       COALESCE(a.bloodlineID, 0),
		       COALESCE(bl.bloodlineName, ''),
		       COALESCE(a.quality, 0), COALESCE(a.corporationID, 0),
		       COALESCE(c.corporationName, COALESCE(eso.ownerName, '')),
		       COALESCE(a.gender, 0)
		FROM agtAgents a
		LEFT JOIN eveStaticOwners an ON an.ownerID = a.agentID
		LEFT JOIN staStations s ON s.stationID = a.stationID
		LEFT JOIN mapSolarSystems sys ON sys.solarSystemID = s.solarSystemID
		LEFT JOIN crpNPCDivisions nd ON nd.divisionID = a.divisionID
		LEFT JOIN chrBloodlines bl ON bl.bloodlineID = a.bloodlineID
		LEFT JOIN corporation c ON c.corporationID = a.corporationID
		LEFT JOIN eveStaticOwners eso ON eso.ownerID = a.corporationID
		WHERE a.agentID = ?`, agentID,
	).Scan(&d.AgentID, &d.AgentName, &d.AgentTypeID, &d.DivisionID, &d.DivisionName,
		&d.Level, &d.StationID, &d.StationName, &d.SystemName,
		&d.BloodlineID, &d.BloodlineName,
		&d.Quality, &d.CorporationID, &d.CorpName, &d.Gender)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("agent %d not found", agentID)
		}
		return nil, fmt.Errorf("ESPGetAgent: %w", err)
	}
	return d, nil
}

// ESPUpdateAgent updates all editable fields of an agtAgents row.
func ESPUpdateAgent(ctx context.Context, db *sql.DB, agentID, agentTypeID, divisionID, level, stationID, bloodlineID, quality, corporationID, gender int64) error {
	_, err := db.ExecContext(ctx, `
		UPDATE agtAgents SET agentTypeID=?, divisionID=?, level=?, stationID=?,
		       bloodlineID=?, quality=?, corporationID=?, gender=?
		WHERE agentID=?`,
		agentTypeID, divisionID, level, stationID, bloodlineID, quality, corporationID, gender, agentID)
	if err != nil {
		return fmt.Errorf("ESPUpdateAgent: %w", err)
	}
	return nil
}

// ESPUpdateAgentIdentity updates bloodlineID and gender on an agtAgents row.
func ESPUpdateAgentIdentity(ctx context.Context, db *sql.DB, agentID, bloodlineID, gender int64) error {
	_, err := db.ExecContext(ctx,
		`UPDATE agtAgents SET bloodlineID=?, gender=? WHERE agentID=?`,
		bloodlineID, gender, agentID)
	return err
}

// ESPGetAgentDivisions returns all NPC divisions for dropdown selectors.
func ESPGetAgentDivisions(ctx context.Context, db *sql.DB) ([]ESPAgentDivision, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT divisionID, divisionName FROM crpNPCDivisions ORDER BY divisionName`)
	if err != nil {
		return nil, fmt.Errorf("ESPGetAgentDivisions: %w", err)
	}
	defer rows.Close()

	var results []ESPAgentDivision
	for rows.Next() {
		var d ESPAgentDivision
		if err := rows.Scan(&d.DivisionID, &d.DivisionName); err != nil {
			return nil, fmt.Errorf("ESPGetAgentDivisions scan: %w", err)
		}
		results = append(results, d)
	}
	return results, rows.Err()
}

// ESPGetAgentBloodlines returns all bloodlines for dropdown selectors.
func ESPGetAgentBloodlines(ctx context.Context, db *sql.DB) ([]ESPAgentBloodline, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT bloodlineID, bloodlineName FROM chrBloodlines ORDER BY bloodlineName`)
	if err != nil {
		return nil, fmt.Errorf("ESPGetAgentBloodlines: %w", err)
	}
	defer rows.Close()

	var results []ESPAgentBloodline
	for rows.Next() {
		var b ESPAgentBloodline
		if err := rows.Scan(&b.BloodlineID, &b.BloodlineName); err != nil {
			return nil, fmt.Errorf("ESPGetAgentBloodlines scan: %w", err)
		}
		results = append(results, b)
	}
	return results, rows.Err()
}

// ── ESP Agent Appearance ────────────────────────────────────────────────────

// ESPAgentAppearance holds the 12 item IDs + rotation values + 16 morphs for an agent portrait.
type ESPAgentAppearance struct {
	AccessoryID   int64
	BeardID       int64
	CostumeID     int64
	DecoID        int64
	EyebrowsID    int64
	EyesID        int64
	HairID        int64
	LipstickID    int64
	MakeupID      int64
	SkinID        int64
	BackgroundID  int64
	LightID       int64
	HeadRotation1 float64
	HeadRotation2 float64
	HeadRotation3 float64
	EyeRotation1  float64
	EyeRotation2  float64
	EyeRotation3  float64
	Morph1e       float64
	Morph1n       float64
	Morph1s       float64
	Morph1w       float64
	Morph2e       float64
	Morph2n       float64
	Morph2s       float64
	Morph2w       float64
	Morph3e       float64
	Morph3n       float64
	Morph3s       float64
	Morph3w       float64
	Morph4e       float64
	Morph4n       float64
	Morph4s       float64
	Morph4w       float64
	IsOverride    bool // true if loaded from agtAppearance, false if computed
}

// ESPAppearanceOption is an ID+Name pair from a chr* master table.
type ESPAppearanceOption struct {
	ID   int64
	Name string
}

// ESPEnsureAppearanceTable creates the agtAppearance table if it doesn't exist,
// and adds morph columns if they are missing (for upgrades from older schema).
func ESPEnsureAppearanceTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS agtAppearance (
			agentID       INT PRIMARY KEY,
			accessoryID   INT, beardID INT, costumeID INT, decoID INT,
			eyebrowsID    INT, eyesID INT, hairID INT,
			lipstickID    INT, makeupID INT, skinID INT,
			backgroundID  INT, lightID INT,
			headRotation1 DOUBLE NOT NULL DEFAULT 0,
			headRotation2 DOUBLE NOT NULL DEFAULT 0,
			headRotation3 DOUBLE NOT NULL DEFAULT 0,
			eyeRotation1  DOUBLE NOT NULL DEFAULT 0,
			eyeRotation2  DOUBLE NOT NULL DEFAULT 0,
			eyeRotation3  DOUBLE NOT NULL DEFAULT 0,
			morph1e DOUBLE NOT NULL DEFAULT 0, morph1n DOUBLE NOT NULL DEFAULT 0,
			morph1s DOUBLE NOT NULL DEFAULT 0, morph1w DOUBLE NOT NULL DEFAULT 0,
			morph2e DOUBLE NOT NULL DEFAULT 0, morph2n DOUBLE NOT NULL DEFAULT 0,
			morph2s DOUBLE NOT NULL DEFAULT 0, morph2w DOUBLE NOT NULL DEFAULT 0,
			morph3e DOUBLE NOT NULL DEFAULT 0, morph3n DOUBLE NOT NULL DEFAULT 0,
			morph3s DOUBLE NOT NULL DEFAULT 0, morph3w DOUBLE NOT NULL DEFAULT 0,
			morph4e DOUBLE NOT NULL DEFAULT 0, morph4n DOUBLE NOT NULL DEFAULT 0,
			morph4s DOUBLE NOT NULL DEFAULT 0, morph4w DOUBLE NOT NULL DEFAULT 0
		)`)
	if err != nil {
		return err
	}
	// Add morph columns if upgrading from older schema.
	morphCols := []string{
		"morph1e", "morph1n", "morph1s", "morph1w",
		"morph2e", "morph2n", "morph2s", "morph2w",
		"morph3e", "morph3n", "morph3s", "morph3w",
		"morph4e", "morph4n", "morph4s", "morph4w",
	}
	for _, col := range morphCols {
		db.ExecContext(ctx, fmt.Sprintf(
			"ALTER TABLE agtAppearance ADD COLUMN %s DOUBLE NOT NULL DEFAULT 0", col))
	}
	return nil
}

// ESPGetAgentAppearance returns the agent's current portrait appearance.
// Checks agtAppearance for overrides first, then computes the deterministic
// default from chrBL* tables using the same algorithm as getNPCAppearance.
func ESPGetAgentAppearance(ctx context.Context, db *sql.DB, agentID, bloodlineID, gender int64) (*ESPAgentAppearance, error) {
	// Try the override table first.
	a := &ESPAgentAppearance{}
	err := db.QueryRowContext(ctx, `
		SELECT COALESCE(accessoryID,0), COALESCE(beardID,0), COALESCE(costumeID,0),
		       COALESCE(decoID,0), COALESCE(eyebrowsID,0), COALESCE(eyesID,0),
		       COALESCE(hairID,0), COALESCE(lipstickID,0), COALESCE(makeupID,0),
		       COALESCE(skinID,0), COALESCE(backgroundID,0), COALESCE(lightID,0),
		       headRotation1, headRotation2, headRotation3,
		       eyeRotation1, eyeRotation2, eyeRotation3,
		       morph1e, morph1n, morph1s, morph1w,
		       morph2e, morph2n, morph2s, morph2w,
		       morph3e, morph3n, morph3s, morph3w,
		       morph4e, morph4n, morph4s, morph4w
		FROM agtAppearance WHERE agentID = ?`, agentID,
	).Scan(&a.AccessoryID, &a.BeardID, &a.CostumeID, &a.DecoID,
		&a.EyebrowsID, &a.EyesID, &a.HairID, &a.LipstickID,
		&a.MakeupID, &a.SkinID, &a.BackgroundID, &a.LightID,
		&a.HeadRotation1, &a.HeadRotation2, &a.HeadRotation3,
		&a.EyeRotation1, &a.EyeRotation2, &a.EyeRotation3,
		&a.Morph1e, &a.Morph1n, &a.Morph1s, &a.Morph1w,
		&a.Morph2e, &a.Morph2n, &a.Morph2s, &a.Morph2w,
		&a.Morph3e, &a.Morph3n, &a.Morph3s, &a.Morph3w,
		&a.Morph4e, &a.Morph4n, &a.Morph4s, &a.Morph4w)
	if err == nil {
		a.IsOverride = true
		return a, nil
	}

	// Compute deterministic defaults (same logic as getNPCAppearance in chardb.go).
	pickOne := func(query string, args ...interface{}) int64 {
		rows, err := db.QueryContext(ctx, query, args...)
		if err != nil {
			return 0
		}
		defer rows.Close()
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err == nil {
				ids = append(ids, id)
			}
		}
		if len(ids) == 0 {
			return 0
		}
		return ids[int(agentID%int64(len(ids)))]
	}

	a.AccessoryID = pickOne("SELECT accessoryID FROM chrBLAccessories WHERE bloodlineID=? AND gender=? ORDER BY accessoryID", bloodlineID, gender)
	a.BeardID = pickOne("SELECT beardID FROM chrBLBeards WHERE bloodlineID=? AND gender=? ORDER BY beardID", bloodlineID, gender)
	a.CostumeID = pickOne("SELECT costumeID FROM chrBLCostumes WHERE bloodlineID=? AND gender=? ORDER BY costumeID", bloodlineID, gender)
	a.DecoID = pickOne("SELECT decoID FROM chrBLDecos WHERE bloodlineID=? AND gender=? ORDER BY decoID", bloodlineID, gender)
	a.EyebrowsID = pickOne("SELECT eyebrowsID FROM chrBLEyebrows WHERE bloodlineID=? AND gender=? ORDER BY eyebrowsID", bloodlineID, gender)
	a.EyesID = pickOne("SELECT eyesID FROM chrBLEyes WHERE bloodlineID=? AND gender=? ORDER BY eyesID", bloodlineID, gender)
	a.HairID = pickOne("SELECT hairID FROM chrBLHairs WHERE bloodlineID=? AND gender=? ORDER BY hairID", bloodlineID, gender)
	a.LipstickID = pickOne("SELECT lipstickID FROM chrBLLipsticks WHERE bloodlineID=? AND gender=? ORDER BY lipstickID", bloodlineID, gender)
	a.MakeupID = pickOne("SELECT makeupID FROM chrBLMakeups WHERE bloodlineID=? AND gender=? ORDER BY makeupID", bloodlineID, gender)
	a.SkinID = pickOne("SELECT skinID FROM chrBLSkins WHERE bloodlineID=? AND gender=? ORDER BY skinID", bloodlineID, gender)
	a.BackgroundID = pickOne("SELECT backgroundID FROM chrBLBackgrounds ORDER BY backgroundID")
	a.LightID = pickOne("SELECT lightID FROM chrBLLights ORDER BY lightID")
	return a, nil
}

// nullIfZero returns nil (SQL NULL) for zero item IDs, otherwise the value.
// The EVE client's face.py DoSetItem crashes with KeyError on item ID 0.
func nullIfZero(v int64) interface{} {
	if v == 0 {
		return nil
	}
	return v
}

// ESPUpdateAgentAppearance saves appearance overrides for an agent.
// Zero item IDs are stored as SQL NULL to avoid client KeyError on ID 0.
func ESPUpdateAgentAppearance(ctx context.Context, db *sql.DB, agentID int64, a *ESPAgentAppearance) error {
	_, err := db.ExecContext(ctx, `
		REPLACE INTO agtAppearance (agentID,
			accessoryID, beardID, costumeID, decoID, eyebrowsID, eyesID, hairID,
			lipstickID, makeupID, skinID, backgroundID, lightID,
			headRotation1, headRotation2, headRotation3,
			eyeRotation1, eyeRotation2, eyeRotation3,
			morph1e, morph1n, morph1s, morph1w,
			morph2e, morph2n, morph2s, morph2w,
			morph3e, morph3n, morph3s, morph3w,
			morph4e, morph4n, morph4s, morph4w)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		        ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		agentID,
		nullIfZero(a.AccessoryID), nullIfZero(a.BeardID), nullIfZero(a.CostumeID), nullIfZero(a.DecoID),
		nullIfZero(a.EyebrowsID), nullIfZero(a.EyesID), nullIfZero(a.HairID),
		nullIfZero(a.LipstickID), nullIfZero(a.MakeupID), nullIfZero(a.SkinID),
		nullIfZero(a.BackgroundID), nullIfZero(a.LightID),
		a.HeadRotation1, a.HeadRotation2, a.HeadRotation3,
		a.EyeRotation1, a.EyeRotation2, a.EyeRotation3,
		a.Morph1e, a.Morph1n, a.Morph1s, a.Morph1w,
		a.Morph2e, a.Morph2n, a.Morph2s, a.Morph2w,
		a.Morph3e, a.Morph3n, a.Morph3s, a.Morph3w,
		a.Morph4e, a.Morph4n, a.Morph4s, a.Morph4w)
	return err
}

// espQueryOptions runs a query returning (id, name) pairs into ESPAppearanceOption.
func espQueryOptions(ctx context.Context, db *sql.DB, query string, args ...interface{}) []ESPAppearanceOption {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var results []ESPAppearanceOption
	for rows.Next() {
		var o ESPAppearanceOption
		if err := rows.Scan(&o.ID, &o.Name); err == nil {
			results = append(results, o)
		}
	}
	return results
}

// ESPGetAllAppearanceOptions returns appearance options valid for the given
// bloodline+gender. The 10 item slots are filtered through the chrBL* tables;
// background and light are global.
func ESPGetAllAppearanceOptions(ctx context.Context, db *sql.DB, bloodlineID, gender int64) map[string][]ESPAppearanceOption {
	type blSlot struct {
		key     string
		blTable string // chrBL* table
		idCol   string // shared ID column
		master  string // chr* master table with names
		nameCol string // name column in master
	}
	blSlots := []blSlot{
		{"accessory", "chrBLAccessories", "accessoryID", "chrAccessories", "accessoryName"},
		{"beard", "chrBLBeards", "beardID", "chrBeards", "beardName"},
		{"costume", "chrBLCostumes", "costumeID", "chrCostumes", "costumeName"},
		{"deco", "chrBLDecos", "decoID", "chrDecos", "decoName"},
		{"eyebrows", "chrBLEyebrows", "eyebrowsID", "chrEyebrows", "eyebrowsName"},
		{"eyes", "chrBLEyes", "eyesID", "chrEyes", "eyesName"},
		{"hair", "chrBLHairs", "hairID", "chrHairs", "hairName"},
		{"lipstick", "chrBLLipsticks", "lipstickID", "chrLipsticks", "lipstickName"},
		{"makeup", "chrBLMakeups", "makeupID", "chrMakeups", "makeupName"},
		{"skin", "chrBLSkins", "skinID", "chrSkins", "skinName"},
	}

	result := make(map[string][]ESPAppearanceOption, 12)
	for _, s := range blSlots {
		q := fmt.Sprintf(
			"SELECT bl.%s, COALESCE(m.%s, CAST(bl.%s AS CHAR)) FROM %s bl LEFT JOIN %s m ON m.%s = bl.%s WHERE bl.bloodlineID = ? AND bl.gender = ? ORDER BY m.%s, bl.%s",
			s.idCol, s.nameCol, s.idCol, s.blTable, s.master, s.idCol, s.idCol, s.nameCol, s.idCol)
		result[s.key] = espQueryOptions(ctx, db, q, bloodlineID, gender)
	}

	// Background and light are not bloodline/gender specific.
	result["background"] = espQueryOptions(ctx, db,
		"SELECT bl.backgroundID, COALESCE(m.backgroundName, CAST(bl.backgroundID AS CHAR)) FROM chrBLBackgrounds bl LEFT JOIN chrBackgrounds m ON m.backgroundID = bl.backgroundID ORDER BY m.backgroundName, bl.backgroundID")
	result["light"] = espQueryOptions(ctx, db,
		"SELECT bl.lightID, COALESCE(m.lightName, CAST(bl.lightID AS CHAR)) FROM chrBLLights bl LEFT JOIN chrLights m ON m.lightID = bl.lightID ORDER BY m.lightName, bl.lightID")

	return result
}

// ESPNextAgentID returns MAX(agentID)+1 from agtAgents.
func ESPNextAgentID(ctx context.Context, db *sql.DB) int64 {
	var maxID int64
	_ = db.QueryRowContext(ctx, `SELECT COALESCE(MAX(agentID), 0) FROM agtAgents`).Scan(&maxID)
	return maxID + 1
}

// ESPCreateAgent inserts a new agent into agtAgents and registers the name in eveStaticOwners.
func ESPCreateAgent(ctx context.Context, db *sql.DB, agentID int64, name string, agentTypeID, divisionID, level, stationID, bloodlineID, quality, corporationID, gender int64) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO agtAgents (agentID, agentTypeID, divisionID, level, stationID, bloodlineID, quality, corporationID, gender)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		agentID, agentTypeID, divisionID, level, stationID, bloodlineID, quality, corporationID, gender)
	if err != nil {
		return fmt.Errorf("ESPCreateAgent: %w", err)
	}
	// Register the agent's name so it resolves everywhere (typeID 1375 = NPC character).
	_, _ = db.ExecContext(ctx,
		`REPLACE INTO eveStaticOwners (ownerID, ownerName, typeID) VALUES (?, ?, 1375)`,
		agentID, name)
	return nil
}

// espPortraitTarget is one portrait-owning ID plus the bloodline/gender pair the
// client renders its mesh with. Appearance items must come from the chrBL* rows
// for exactly that pair, or the mesh and the costume/hair items disagree.
type espPortraitTarget struct {
	id          int64
	bloodlineID int64
	gender      int64
}

// ESPScrambleAllAgentPortraits randomizes portrait data for all agents.
// Bloodline and gender are preserved. If randomizeMorphs is true, morph values
// are randomized in [0.0, 1.0]; otherwise they stay at 0.0.
// Returns count of agents updated.
func ESPScrambleAllAgentPortraits(ctx context.Context, db *sql.DB, randomizeMorphs, randomizeHeadRotation, randomizeEyeRotation bool) (int, error) {
	rows, err := db.QueryContext(ctx, `SELECT agentID, bloodlineID, gender FROM agtAgents`)
	if err != nil {
		return 0, fmt.Errorf("ESPScrambleAllAgentPortraits agents: %w", err)
	}
	var agents []espPortraitTarget
	for rows.Next() {
		var a espPortraitTarget
		if err := rows.Scan(&a.id, &a.bloodlineID, &a.gender); err != nil {
			rows.Close()
			return 0, fmt.Errorf("ESPScrambleAllAgentPortraits scan: %w", err)
		}
		agents = append(agents, a)
	}
	rows.Close()

	n, err := espScramblePortraits(ctx, db, agents, randomizeMorphs, randomizeHeadRotation, randomizeEyeRotation)
	if err != nil {
		return n, fmt.Errorf("ESPScrambleAllAgentPortraits: %w", err)
	}
	return n, nil
}

// espGetNPCCEOs returns the NPC corporation CEOs that own a portrait. A CEO has
// no agtAgents row: bloodline comes from the eveStaticOwners character typeID
// (1373-1386) and gender is ceoID%2, as getNPCAppearance derives it. Player CEOs
// (character_ rows) and CEOs that are also agents are skipped.
func espGetNPCCEOs(ctx context.Context, db *sql.DB) ([]espPortraitTarget, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT DISTINCT c.ceoID, o.typeID
		FROM corporation c
		JOIN eveStaticOwners o ON o.ownerID = c.ceoID
		WHERE c.ceoID > 0
		  AND NOT EXISTS (SELECT 1 FROM character_ ch WHERE ch.characterID = c.ceoID)
		  AND NOT EXISTS (SELECT 1 FROM agtAgents  a  WHERE a.agentID     = c.ceoID)
		ORDER BY c.ceoID`)
	if err != nil {
		return nil, fmt.Errorf("espGetNPCCEOs: %w", err)
	}
	defer rows.Close()

	var ceos []espPortraitTarget
	for rows.Next() {
		var ceoID, typeID int64
		if err := rows.Scan(&ceoID, &typeID); err != nil {
			return nil, fmt.Errorf("espGetNPCCEOs scan: %w", err)
		}
		t := espPortraitTarget{id: ceoID, gender: ceoID % 2}
		if bl, ok := charTypeBloodlines[typeID]; ok {
			t.bloodlineID = bl[0]
		} else {
			// Same last-resort the runtime path uses when the typeID is not a
			// known character type, so the scramble matches the rendered mesh.
			t.bloodlineID = 1 // Deteis (Caldari)
		}
		ceos = append(ceos, t)
	}
	return ceos, rows.Err()
}

// ESPScrambleAllCEOPortraits randomizes portrait data for every NPC corporation
// CEO. Bloodline (from the character typeID) and derived gender are preserved.
// Returns count of CEOs updated.
func ESPScrambleAllCEOPortraits(ctx context.Context, db *sql.DB, randomizeMorphs, randomizeHeadRotation, randomizeEyeRotation bool) (int, error) {
	ceos, err := espGetNPCCEOs(ctx, db)
	if err != nil {
		return 0, err
	}
	n, err := espScramblePortraits(ctx, db, ceos, randomizeMorphs, randomizeHeadRotation, randomizeEyeRotation)
	if err != nil {
		return n, fmt.Errorf("ESPScrambleAllCEOPortraits: %w", err)
	}
	return n, nil
}

// espScramblePortraits writes a random agtAppearance override for every target,
// picking each slot from the chrBL* rows valid for that target's bloodline and
// gender. Shared by the agent and CEO scramblers.
func espScramblePortraits(ctx context.Context, db *sql.DB, targets []espPortraitTarget, randomizeMorphs, randomizeHeadRotation, randomizeEyeRotation bool) (int, error) {
	// Cache valid options per (bloodlineID, gender) pair.
	type blKey struct {
		bloodlineID, gender int64
	}
	type slotOptions struct {
		accessory, beard, costume, deco []int64
		eyebrows, eyes, hair            []int64
		lipstick, makeup, skin          []int64
	}
	optCache := make(map[blKey]*slotOptions)

	queryIDs := func(query string, args ...interface{}) []int64 {
		r, err := db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil
		}
		defer r.Close()
		var ids []int64
		for r.Next() {
			var id int64
			if r.Scan(&id) == nil && id != 0 {
				ids = append(ids, id)
			}
		}
		return ids
	}

	getOptions := func(bl, g int64) *slotOptions {
		key := blKey{bl, g}
		if cached, ok := optCache[key]; ok {
			return cached
		}
		opts := &slotOptions{
			accessory: queryIDs("SELECT accessoryID FROM chrBLAccessories WHERE bloodlineID=? AND gender=? ORDER BY accessoryID", bl, g),
			beard:     queryIDs("SELECT beardID FROM chrBLBeards WHERE bloodlineID=? AND gender=? ORDER BY beardID", bl, g),
			costume:   queryIDs("SELECT costumeID FROM chrBLCostumes WHERE bloodlineID=? AND gender=? ORDER BY costumeID", bl, g),
			deco:      queryIDs("SELECT decoID FROM chrBLDecos WHERE bloodlineID=? AND gender=? ORDER BY decoID", bl, g),
			eyebrows:  queryIDs("SELECT eyebrowsID FROM chrBLEyebrows WHERE bloodlineID=? AND gender=? ORDER BY eyebrowsID", bl, g),
			eyes:      queryIDs("SELECT eyesID FROM chrBLEyes WHERE bloodlineID=? AND gender=? ORDER BY eyesID", bl, g),
			hair:      queryIDs("SELECT hairID FROM chrBLHairs WHERE bloodlineID=? AND gender=? ORDER BY hairID", bl, g),
			lipstick:  queryIDs("SELECT lipstickID FROM chrBLLipsticks WHERE bloodlineID=? AND gender=? ORDER BY lipstickID", bl, g),
			makeup:    queryIDs("SELECT makeupID FROM chrBLMakeups WHERE bloodlineID=? AND gender=? ORDER BY makeupID", bl, g),
			skin:      queryIDs("SELECT skinID FROM chrBLSkins WHERE bloodlineID=? AND gender=? ORDER BY skinID", bl, g),
		}
		optCache[key] = opts
		return opts
	}

	// Global options (not bloodline/gender specific).
	backgrounds := queryIDs("SELECT backgroundID FROM chrBLBackgrounds ORDER BY backgroundID")
	lights := queryIDs("SELECT lightID FROM chrBLLights ORDER BY lightID")

	pickRand := func(ids []int64) int64 {
		if len(ids) == 0 {
			return 0
		}
		return ids[rand.Intn(len(ids))]
	}

	count := 0
	for _, ag := range targets {
		opts := getOptions(ag.bloodlineID, ag.gender)
		a := &ESPAgentAppearance{
			AccessoryID:  pickRand(opts.accessory),
			BeardID:      pickRand(opts.beard),
			CostumeID:    pickRand(opts.costume),
			DecoID:       pickRand(opts.deco),
			EyebrowsID:   pickRand(opts.eyebrows),
			EyesID:       pickRand(opts.eyes),
			HairID:       pickRand(opts.hair),
			LipstickID:   pickRand(opts.lipstick),
			MakeupID:     pickRand(opts.makeup),
			SkinID:       pickRand(opts.skin),
			BackgroundID: pickRand(backgrounds),
			LightID:      pickRand(lights),
		}
		if randomizeMorphs {
			a.Morph1e = rand.Float64()
			a.Morph1n = rand.Float64()
			a.Morph1s = rand.Float64()
			a.Morph1w = rand.Float64()
			a.Morph2e = rand.Float64()
			a.Morph2n = rand.Float64()
			a.Morph2s = rand.Float64()
			a.Morph2w = rand.Float64()
			a.Morph3e = rand.Float64()
			a.Morph3n = rand.Float64()
			a.Morph3s = rand.Float64()
			a.Morph3w = rand.Float64()
			a.Morph4e = rand.Float64()
			a.Morph4n = rand.Float64()
			a.Morph4s = rand.Float64()
			a.Morph4w = rand.Float64()
		}
		if randomizeHeadRotation {
			a.HeadRotation1 = rand.Float64()*2 - 1
			a.HeadRotation2 = rand.Float64()*2 - 1
			a.HeadRotation3 = rand.Float64()*2 - 1
		}
		if randomizeEyeRotation {
			a.EyeRotation1 = rand.Float64()*2 - 1
			a.EyeRotation2 = rand.Float64()*2 - 1
			a.EyeRotation3 = rand.Float64()*2 - 1
		}
		if err := ESPUpdateAgentAppearance(ctx, db, ag.id, a); err != nil {
			return count, fmt.Errorf("scramble portrait %d: %w", ag.id, err)
		}
		count++
	}
	return count, nil
}

// ESPNPCCorpOption is a corp ID+name pair for NPC corporation dropdowns.
type ESPNPCCorpOption struct {
	CorporationID int64
	CorpName      string
}

// ESPGetAllNPCCorps returns all NPC corporations for dropdown selectors.
func ESPGetAllNPCCorps(ctx context.Context, db *sql.DB) ([]ESPNPCCorpOption, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT c.corporationID,
		       COALESCE(corp.corporationName, COALESCE(eso.ownerName, CAST(c.corporationID AS CHAR)))
		FROM crpNPCCorporations c
		LEFT JOIN corporation corp ON corp.corporationID = c.corporationID
		LEFT JOIN eveStaticOwners eso ON eso.ownerID = c.corporationID
		ORDER BY 2`)
	if err != nil {
		return nil, fmt.Errorf("ESPGetAllNPCCorps: %w", err)
	}
	defer rows.Close()
	var results []ESPNPCCorpOption
	for rows.Next() {
		var r ESPNPCCorpOption
		if err := rows.Scan(&r.CorporationID, &r.CorpName); err != nil {
			return nil, fmt.Errorf("ESPGetAllNPCCorps scan: %w", err)
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// ESPSearchStations searches stations by name.
func ESPSearchStations(ctx context.Context, db *sql.DB, search string) ([]ESPStation, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT s.stationID, s.stationName, s.solarSystemID,
		       COALESCE(sys.solarSystemName, ''),
		       (SELECT COUNT(*) FROM character_ c
		        WHERE c.stationID = s.stationID AND c.accountID > 0) AS docked
		FROM staStations s
		LEFT JOIN mapSolarSystems sys ON sys.solarSystemID = s.solarSystemID
		WHERE s.stationName LIKE ?
		ORDER BY docked DESC, s.stationName
		LIMIT 50`, "%"+search+"%")
	if err != nil {
		return nil, fmt.Errorf("ESPSearchStations: %w", err)
	}
	defer rows.Close()

	var results []ESPStation
	for rows.Next() {
		var st ESPStation
		if err := rows.Scan(&st.StationID, &st.StationName, &st.SolarSystemID, &st.SystemName, &st.DockedCount); err != nil {
			return nil, fmt.Errorf("ESPSearchStations scan: %w", err)
		}
		results = append(results, st)
	}
	return results, rows.Err()
}

// ── Mission Template ESP functions ──────────────────────────────────────────

// ESPMissionTemplateSummary is a row in the mission template list.
type ESPMissionTemplateSummary struct {
	TemplateID  int64
	MissionName string
	MissionType string
	AgentLevel  int
	Enabled     bool
	Storyline   bool
}

// ESPMissionTemplateDetail is the full template for editing.
type ESPMissionTemplateDetail struct {
	TemplateID  int64
	MissionName string
	MissionType string
	AgentLevel  int
	// Storyline puts this template in the storyline agents' exclusive deck.
	Storyline     bool
	BriefingText  string
	ObjectiveText sql.NullString
	AcceptText    sql.NullString
	CompleteText  sql.NullString
	FailText      sql.NullString // the agent's letter when the mission expires
	DeclineText   sql.NullString
	MoreInfoText  sql.NullString

	// Player-facing captions for the conversation choices. NULL/empty =
	// the generic caption the server falls back to.
	AcceptLabel      sql.NullString
	DeclineLabel     sql.NullString
	ViewMissionLabel sql.NullString
	MoreInfoLabel    sql.NullString
	CompleteLabel    sql.NullString
	QuitMissionLabel sql.NullString

	DungeonVID               sql.NullInt64
	DungeonRoomIdx           int
	CompletionType           string
	TargetTypeID             sql.NullInt64
	TargetCount              int
	KillItemTypeID           sql.NullInt64
	KillItemQty              int
	KillItemDropoffMode      string
	KillItemDropoffStationID sql.NullInt64
	EnemyFactionID           sql.NullInt64

	CourierTypeID    sql.NullInt64
	CourierQty       int
	PickupMode       string
	PickupStationID  sql.NullInt64
	DropoffMode      string
	DropoffStationID sql.NullInt64

	TradeTypeID sql.NullInt64
	TradeQty    int

	MiningTypeID sql.NullInt64
	MiningQty    int

	MinSecurity sql.NullFloat64
	MaxSecurity sql.NullFloat64

	BaseRewardISK         int64
	BaseRewardLP          int
	BaseBonusISK          int64
	BaseRewardItemTypeID  sql.NullInt64
	BaseRewardItemQty     int
	BonusRewardItemTypeID sql.NullInt64
	BonusRewardItemQty    int
	BonusTimeSec          int
	ExpiryDays            int // days an accepted mission has before it fails
	StandingGain          float64
	CorpStandingGain      float64
	FactionStandingGain   float64
	StandingLoss          float64
	FactionStandingLoss   float64

	AllowedFactions sql.NullString
	NextMissionID   sql.NullInt64
	Enabled         bool
}

// ESPDivisionWeightRow is a row in the division weights editor.
type ESPDivisionWeightRow struct {
	DivisionID    int64
	DivisionName  string
	KillWeight    int
	CourierWeight int
	TradeWeight   int
	MiningWeight  int
}

// ESPDivisionTypeCell is one mission type's slice of a division's roll: Weight is
// the authored number, Pct its share of the roll, EffPct what the player sees
// once template coverage and generateMissionOffer's kill fallback apply.
type ESPDivisionTypeCell struct {
	Type      string
	Weight    int
	Pct       float64
	EffPct    float64
	Templates int   // usable enabled templates across the levels this division staffs
	Missing   []int // staffed agent levels with no usable template of this type
}

// ESPDivisionLevel is a mission-agent headcount at one agent level.
type ESPDivisionLevel struct {
	Level  int
	Agents int
}

// ESPDivisionDistRow is one division on the mission distribution page.
type ESPDivisionDistRow struct {
	DivisionID   int64
	DivisionName string
	HasRow       bool // false: no weights row; GetDivisionWeights falls back to 50/50 kill/courier
	Total        int  // sum of the four weights: the roll denominator
	Cells        []ESPDivisionTypeCell
	Agents       int
	Levels       []ESPDivisionLevel
	DeadPct      float64 // rolls that reach no template at all ("no missions available")
	Warnings     []string
}

// ESPTemplateLevelRow is one agent level's enabled mission template inventory.
// KillUsable excludes kill templates the offer path rejects: no dungeonVID, or a
// dungeon with no rooms.
type ESPTemplateLevelRow struct {
	Level        int
	Kill         int
	KillUsable   int
	KillUnusable int
	Courier      int
	Trade        int
	Mining       int
}

// ESPDistSummary is the server-wide picture above the division table.
type ESPDistSummary struct {
	Divisions      int
	NoRow          int // divisions running on the 50/50 fallback
	Agents         int
	AgentDivisions int
	Cells          []ESPDivisionTypeCell // agent-weighted mix across the server
	DeadPct        float64
	Inventory      []ESPTemplateLevelRow
	Gaps           int // divisions with at least one coverage warning
}

// ESPDungeonOption is a (VID, name) pair for dungeon dropdown.
type ESPDungeonOption struct {
	DungeonVID int64
	Name       string
}

// ESPGetAllMissionTemplates returns all mission templates for the list page.
func ESPGetAllMissionTemplates(ctx context.Context, db *sql.DB) ([]ESPMissionTemplateSummary, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT templateID, missionName, missionType, agentLevel, enabled, COALESCE(storyline,0)
		 FROM missionTemplates ORDER BY agentLevel, missionType, missionName`)
	if err != nil {
		return nil, fmt.Errorf("ESPGetAllMissionTemplates: %w", err)
	}
	defer rows.Close()
	var out []ESPMissionTemplateSummary
	for rows.Next() {
		var t ESPMissionTemplateSummary
		if err := rows.Scan(&t.TemplateID, &t.MissionName, &t.MissionType, &t.AgentLevel, &t.Enabled, &t.Storyline); err != nil {
			return nil, fmt.Errorf("ESPGetAllMissionTemplates scan: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ESPGetMissionTemplate returns a single template for editing.
func ESPGetMissionTemplate(ctx context.Context, db *sql.DB, id int64) (*ESPMissionTemplateDetail, error) {
	t := &ESPMissionTemplateDetail{}
	err := db.QueryRowContext(ctx,
		`SELECT templateID, missionName, missionType, agentLevel, COALESCE(storyline,0),
		        briefingText, objectiveText, acceptText, completeText, failText, declineText, moreInfoText,
		        acceptLabel, declineLabel, viewMissionLabel, moreInfoLabel, completeLabel, quitMissionLabel,
		        dungeonVID, dungeonRoomIdx, completionType, targetTypeID, targetCount,
		        killItemTypeID, killItemQty, COALESCE(killItemDropoffMode,'agent'), killItemDropoffStationID,
		        enemyFactionID,
		        courierTypeID, courierQty, pickupMode, pickupStationID, dropoffMode, dropoffStationID,
		        tradeTypeID, tradeQty,
		        miningTypeID, miningQty,
		        minSecurity, maxSecurity,
		        baseRewardISK, baseRewardLP, baseBonusISK,
		        baseRewardItemTypeID, COALESCE(baseRewardItemQty,1),
		        bonusRewardItemTypeID, COALESCE(bonusRewardItemQty,1),
		        bonusTimeSec, COALESCE(expiryDays,7),
		        COALESCE(standingGain,0), COALESCE(corpStandingGain,0), COALESCE(factionStandingGain,0),
		        COALESCE(standingLoss,0), COALESCE(factionStandingLoss,0),
		        allowedFactions, nextMissionID,
		        enabled
		 FROM missionTemplates WHERE templateID = ?`, id).Scan(
		&t.TemplateID, &t.MissionName, &t.MissionType, &t.AgentLevel, &t.Storyline,
		&t.BriefingText, &t.ObjectiveText, &t.AcceptText, &t.CompleteText, &t.FailText, &t.DeclineText, &t.MoreInfoText,
		&t.AcceptLabel, &t.DeclineLabel, &t.ViewMissionLabel, &t.MoreInfoLabel, &t.CompleteLabel, &t.QuitMissionLabel,
		&t.DungeonVID, &t.DungeonRoomIdx, &t.CompletionType, &t.TargetTypeID, &t.TargetCount,
		&t.KillItemTypeID, &t.KillItemQty, &t.KillItemDropoffMode, &t.KillItemDropoffStationID,
		&t.EnemyFactionID,
		&t.CourierTypeID, &t.CourierQty, &t.PickupMode, &t.PickupStationID, &t.DropoffMode, &t.DropoffStationID,
		&t.TradeTypeID, &t.TradeQty,
		&t.MiningTypeID, &t.MiningQty,
		&t.MinSecurity, &t.MaxSecurity,
		&t.BaseRewardISK, &t.BaseRewardLP, &t.BaseBonusISK,
		&t.BaseRewardItemTypeID, &t.BaseRewardItemQty,
		&t.BonusRewardItemTypeID, &t.BonusRewardItemQty,
		&t.BonusTimeSec, &t.ExpiryDays,
		&t.StandingGain, &t.CorpStandingGain, &t.FactionStandingGain,
		&t.StandingLoss, &t.FactionStandingLoss,
		&t.AllowedFactions, &t.NextMissionID,
		&t.Enabled)
	if err != nil {
		return nil, fmt.Errorf("ESPGetMissionTemplate: %w", err)
	}
	return t, nil
}

// ESPCreateMissionTemplate creates a new mission template and returns its ID.
func ESPCreateMissionTemplate(ctx context.Context, db *sql.DB, name, mType string, level int, storyline bool) (int64, error) {
	sl := 0
	if storyline {
		sl = 1
	}
	res, err := db.ExecContext(ctx,
		`INSERT INTO missionTemplates (missionName, missionType, agentLevel, storyline, briefingText)
		 VALUES (?, ?, ?, ?, 'New mission briefing.')`, name, mType, level, sl)
	if err != nil {
		return 0, fmt.Errorf("ESPCreateMissionTemplate: %w", err)
	}
	return res.LastInsertId()
}

// ESPUpdateMissionTemplate updates all fields of a mission template.
func ESPUpdateMissionTemplate(ctx context.Context, db *sql.DB, t *ESPMissionTemplateDetail) error {
	_, err := db.ExecContext(ctx,
		`UPDATE missionTemplates SET
		    missionName=?, missionType=?, agentLevel=?, storyline=?,
		    briefingText=?, objectiveText=?, acceptText=?, completeText=?, failText=?, declineText=?, moreInfoText=?,
		    acceptLabel=?, declineLabel=?, viewMissionLabel=?, moreInfoLabel=?, completeLabel=?, quitMissionLabel=?,
		    dungeonVID=?, dungeonRoomIdx=?, completionType=?, targetTypeID=?, targetCount=?,
		    killItemTypeID=?, killItemQty=?, killItemDropoffMode=?, killItemDropoffStationID=?,
		    enemyFactionID=?,
		    courierTypeID=?, courierQty=?, pickupMode=?, pickupStationID=?, dropoffMode=?, dropoffStationID=?,
		    tradeTypeID=?, tradeQty=?,
		    miningTypeID=?, miningQty=?,
		    minSecurity=?, maxSecurity=?,
		    baseRewardISK=?, baseRewardLP=?, baseBonusISK=?,
		    baseRewardItemTypeID=?, baseRewardItemQty=?,
		    bonusRewardItemTypeID=?, bonusRewardItemQty=?,
		    bonusTimeSec=?, expiryDays=?,
		    standingGain=?, corpStandingGain=?, factionStandingGain=?,
		    standingLoss=?, factionStandingLoss=?,
		    allowedFactions=?, nextMissionID=?,
		    enabled=?
		 WHERE templateID=?`,
		t.MissionName, t.MissionType, t.AgentLevel, t.Storyline,
		t.BriefingText, t.ObjectiveText, t.AcceptText, t.CompleteText, t.FailText, t.DeclineText, t.MoreInfoText,
		t.AcceptLabel, t.DeclineLabel, t.ViewMissionLabel, t.MoreInfoLabel, t.CompleteLabel, t.QuitMissionLabel,
		t.DungeonVID, t.DungeonRoomIdx, t.CompletionType, t.TargetTypeID, t.TargetCount,
		t.KillItemTypeID, t.KillItemQty, t.KillItemDropoffMode, t.KillItemDropoffStationID,
		t.EnemyFactionID,
		t.CourierTypeID, t.CourierQty, t.PickupMode, t.PickupStationID, t.DropoffMode, t.DropoffStationID,
		t.TradeTypeID, t.TradeQty,
		t.MiningTypeID, t.MiningQty,
		t.MinSecurity, t.MaxSecurity,
		t.BaseRewardISK, t.BaseRewardLP, t.BaseBonusISK,
		t.BaseRewardItemTypeID, t.BaseRewardItemQty,
		t.BonusRewardItemTypeID, t.BonusRewardItemQty,
		t.BonusTimeSec, ClampMissionExpiryDays(t.ExpiryDays),
		t.StandingGain, t.CorpStandingGain, t.FactionStandingGain,
		t.StandingLoss, t.FactionStandingLoss,
		t.AllowedFactions, t.NextMissionID,
		t.Enabled,
		t.TemplateID)
	if err != nil {
		return fmt.Errorf("ESPUpdateMissionTemplate: %w", err)
	}
	return nil
}

// ESPDeleteMissionTemplate deletes a mission template.
func ESPDeleteMissionTemplate(ctx context.Context, db *sql.DB, id int64) error {
	_, err := db.ExecContext(ctx, `DELETE FROM missionTemplates WHERE templateID = ?`, id)
	if err != nil {
		return fmt.Errorf("ESPDeleteMissionTemplate: %w", err)
	}
	return nil
}

// ESPMissionTypes is the mission type list in the order RollMissionType walks
// its weight ranges.
var ESPMissionTypes = []string{"kill", "courier", "trade", "mining"}

// ESPSetDivisionWeights writes one division's mission type weights, creating the
// row if the division never had one (a division with no row rolls on the 50/50
// kill/courier default in GetDivisionWeights).
func ESPSetDivisionWeights(ctx context.Context, db *sql.DB, divisionID int64, kill, courier, trade, mining int) error {
	_, err := db.ExecContext(ctx,
		`INSERT INTO agtDivisionMissionWeights
		        (divisionID, killWeight, courierWeight, tradeWeight, miningWeight)
		 VALUES (?, ?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE
		        killWeight=VALUES(killWeight), courierWeight=VALUES(courierWeight),
		        tradeWeight=VALUES(tradeWeight), miningWeight=VALUES(miningWeight)`,
		divisionID, kill, courier, trade, mining)
	if err != nil {
		return fmt.Errorf("ESPSetDivisionWeights: %w", err)
	}
	return nil
}

// ESPGetMissionDistribution builds the mission distribution view: every agent
// division with its authored weights, the mission agents it staffs, and the
// effective mix once template coverage applies. The mix mirrors
// generateMissionOffer's kill fallback; allowedFactions filtering is not modelled.
func ESPGetMissionDistribution(ctx context.Context, db *sql.DB) ([]ESPDivisionDistRow, ESPDistSummary, error) {
	var sum ESPDistSummary

	// Divisions.
	divRows, err := db.QueryContext(ctx,
		`SELECT divisionID, divisionName FROM crpNPCDivisions ORDER BY divisionID`)
	if err != nil {
		return nil, sum, fmt.Errorf("ESPGetMissionDistribution divisions: %w", err)
	}
	type division struct {
		id   int64
		name string
	}
	var divisions []division
	for divRows.Next() {
		var d division
		if err := divRows.Scan(&d.id, &d.name); err != nil {
			divRows.Close()
			return nil, sum, fmt.Errorf("ESPGetMissionDistribution divisions scan: %w", err)
		}
		divisions = append(divisions, d)
	}
	divRows.Close()
	if err := divRows.Err(); err != nil {
		return nil, sum, fmt.Errorf("ESPGetMissionDistribution divisions: %w", err)
	}

	// Authored weights.
	weights := map[int64]ESPDivisionWeightRow{}
	wRows, err := db.QueryContext(ctx,
		`SELECT divisionID, killWeight, courierWeight, tradeWeight, miningWeight
		 FROM agtDivisionMissionWeights`)
	if err != nil {
		return nil, sum, fmt.Errorf("ESPGetMissionDistribution weights: %w", err)
	}
	for wRows.Next() {
		var r ESPDivisionWeightRow
		if err := wRows.Scan(&r.DivisionID, &r.KillWeight, &r.CourierWeight, &r.TradeWeight, &r.MiningWeight); err != nil {
			wRows.Close()
			return nil, sum, fmt.Errorf("ESPGetMissionDistribution weights scan: %w", err)
		}
		weights[r.DivisionID] = r
	}
	wRows.Close()
	if err := wRows.Err(); err != nil {
		return nil, sum, fmt.Errorf("ESPGetMissionDistribution weights: %w", err)
	}

	// Mission agents per division and level. Non-agents and R&D agents never reach
	// generateMissionOffer; storyline agents serve their own deck, so neither counts.
	staff := map[int64]map[int]int{}
	aRows, err := db.QueryContext(ctx,
		`SELECT divisionID, level, COUNT(*)
		 FROM agtAgents
		 WHERE agentTypeID NOT IN (1, ?, ?, ?)
		 GROUP BY divisionID, level`, AgentTypeResearch, AgentTypeStoryline, AgentTypeGenericStoryline)
	if err != nil {
		return nil, sum, fmt.Errorf("ESPGetMissionDistribution agents: %w", err)
	}
	for aRows.Next() {
		var divID int64
		var level, n int
		if err := aRows.Scan(&divID, &level, &n); err != nil {
			aRows.Close()
			return nil, sum, fmt.Errorf("ESPGetMissionDistribution agents scan: %w", err)
		}
		if staff[divID] == nil {
			staff[divID] = map[int]int{}
		}
		staff[divID][level] += n
	}
	aRows.Close()
	if err := aRows.Err(); err != nil {
		return nil, sum, fmt.Errorf("ESPGetMissionDistribution agents: %w", err)
	}

	// Enabled templates per type and level. Kill templates the offer path rejects
	// (no dungeonVID, or a dungeon with no rooms) are counted separately.
	total := map[string]map[int]int{}
	usable := map[string]map[int]int{}
	for _, t := range ESPMissionTypes {
		total[t] = map[int]int{}
		usable[t] = map[int]int{}
	}
	tRows, err := db.QueryContext(ctx,
		`SELECT t.missionType, t.agentLevel, COUNT(*),
		        CAST(SUM(CASE WHEN t.missionType <> 'kill'
		                        OR (t.dungeonVID > 0
		                            AND EXISTS (SELECT 1 FROM dunRooms r WHERE r.dungeonVID = t.dungeonVID))
		                      THEN 1 ELSE 0 END) AS SIGNED)
		 FROM missionTemplates t
		 WHERE t.enabled = 1 AND COALESCE(t.storyline,0) = 0
		 GROUP BY t.missionType, t.agentLevel`)
	if err != nil {
		return nil, sum, fmt.Errorf("ESPGetMissionDistribution templates: %w", err)
	}
	for tRows.Next() {
		var mtype string
		var level, n, ok int
		if err := tRows.Scan(&mtype, &level, &n, &ok); err != nil {
			tRows.Close()
			return nil, sum, fmt.Errorf("ESPGetMissionDistribution templates scan: %w", err)
		}
		if total[mtype] == nil {
			continue
		}
		total[mtype][level] += n
		usable[mtype][level] += ok
	}
	tRows.Close()
	if err := tRows.Err(); err != nil {
		return nil, sum, fmt.Errorf("ESPGetMissionDistribution templates: %w", err)
	}

	// Template inventory grid over every level that has agents or templates.
	levelSet := map[int]bool{}
	for _, byLevel := range staff {
		for lvl := range byLevel {
			levelSet[lvl] = true
		}
	}
	for _, byLevel := range total {
		for lvl := range byLevel {
			levelSet[lvl] = true
		}
	}
	var levels []int
	for lvl := range levelSet {
		levels = append(levels, lvl)
	}
	sort.Ints(levels)
	for _, lvl := range levels {
		sum.Inventory = append(sum.Inventory, ESPTemplateLevelRow{
			Level:        lvl,
			Kill:         total["kill"][lvl],
			KillUsable:   usable["kill"][lvl],
			KillUnusable: total["kill"][lvl] - usable["kill"][lvl],
			Courier:      usable["courier"][lvl],
			Trade:        usable["trade"][lvl],
			Mining:       usable["mining"][lvl],
		})
	}

	// Server-wide effective mix, agent-weighted.
	globalEff := map[string]float64{}
	var globalDead, globalAgents float64

	var out []ESPDivisionDistRow
	for _, d := range divisions {
		w, hasRow := weights[d.id]
		if !hasRow {
			// Mirror the GetDivisionWeights fallback.
			w = ESPDivisionWeightRow{DivisionID: d.id, KillWeight: 50, CourierWeight: 50}
		}
		byType := map[string]int{
			"kill": w.KillWeight, "courier": w.CourierWeight,
			"trade": w.TradeWeight, "mining": w.MiningWeight,
		}
		row := ESPDivisionDistRow{
			DivisionID: d.id, DivisionName: d.name, HasRow: hasRow,
		}
		for _, t := range ESPMissionTypes {
			row.Total += byType[t]
		}

		// Staffed levels, ascending.
		var staffed []int
		for lvl, n := range staff[d.id] {
			if n > 0 {
				staffed = append(staffed, lvl)
			}
		}
		sort.Ints(staffed)
		for _, lvl := range staffed {
			row.Levels = append(row.Levels, ESPDivisionLevel{Level: lvl, Agents: staff[d.id][lvl]})
			row.Agents += staff[d.id][lvl]
		}

		// Effective mix: replay the roll per staffed level and apply the fallback chain.
		// A zero total means RollMissionType returns kill unconditionally.
		eff := map[string]float64{}
		var dead float64
		for _, lvl := range staffed {
			n := float64(staff[d.id][lvl])
			if row.Total <= 0 {
				if usable["kill"][lvl] > 0 {
					eff["kill"] += n
				} else {
					dead += n
				}
				continue
			}
			for _, t := range ESPMissionTypes {
				share := n * float64(byType[t]) / float64(row.Total)
				if share == 0 {
					continue
				}
				switch {
				case usable[t][lvl] > 0:
					eff[t] += share
				case t != "kill" && usable["kill"][lvl] > 0:
					eff["kill"] += share
				default:
					dead += share
				}
			}
		}

		for _, t := range ESPMissionTypes {
			cell := ESPDivisionTypeCell{Type: t, Weight: byType[t]}
			if row.Total > 0 {
				cell.Pct = float64(byType[t]) * 100 / float64(row.Total)
			} else if t == "kill" {
				cell.Pct = 100
			}
			if row.Agents > 0 {
				cell.EffPct = eff[t] * 100 / float64(row.Agents)
				for _, lvl := range staffed {
					if byType[t] > 0 && usable[t][lvl] == 0 {
						cell.Missing = append(cell.Missing, lvl)
					}
					cell.Templates += usable[t][lvl]
				}
			} else {
				// No agents: nothing rolls here, so the authored split is all
				// there is to show.
				cell.EffPct = cell.Pct
				for _, lvl := range levels {
					cell.Templates += usable[t][lvl]
				}
			}
			row.Cells = append(row.Cells, cell)
		}
		if row.Agents > 0 {
			row.DeadPct = dead * 100 / float64(row.Agents)
			globalAgents += float64(row.Agents)
			globalDead += dead
			for _, t := range ESPMissionTypes {
				globalEff[t] += eff[t]
			}
		}

		// Warnings, in the order a GM would act on them. Kept terse: a thin template
		// library gives nearly every division several.
		if row.Total <= 0 && row.Agents > 0 {
			row.Warnings = append(row.Warnings, "all weights zero → every roll is kill")
		}
		for _, cell := range row.Cells {
			if len(cell.Missing) == 0 {
				continue
			}
			outcome := "→ falls back to kill"
			if cell.Type == "kill" {
				outcome = "→ no mission"
			}
			row.Warnings = append(row.Warnings, fmt.Sprintf("%s %.0f%%: no template at %s %s",
				cell.Type, cell.Pct, joinLevels(cell.Missing), outcome))
		}
		for _, lvl := range staffed {
			if usable["kill"][lvl] == 0 && total["kill"][lvl] > 0 {
				row.Warnings = append(row.Warnings, fmt.Sprintf(
					"L%d: %d kill template(s), none with a dungeon that has rooms", lvl, total["kill"][lvl]))
			}
		}
		if row.DeadPct > 0 {
			row.Warnings = append(row.Warnings, fmt.Sprintf(
				"%.0f%% of offers answer \"no missions available\"", row.DeadPct))
		}
		if len(row.Warnings) > 0 {
			sum.Gaps++
		}

		sum.Divisions++
		if !hasRow {
			sum.NoRow++
		}
		sum.Agents += row.Agents
		if row.Agents > 0 {
			sum.AgentDivisions++
		}
		out = append(out, row)
	}

	for _, t := range ESPMissionTypes {
		cell := ESPDivisionTypeCell{Type: t}
		for _, lvl := range levels {
			cell.Templates += usable[t][lvl]
		}
		if globalAgents > 0 {
			cell.EffPct = globalEff[t] * 100 / globalAgents
			cell.Pct = cell.EffPct
		}
		sum.Cells = append(sum.Cells, cell)
	}
	if globalAgents > 0 {
		sum.DeadPct = globalDead * 100 / globalAgents
	}

	return out, sum, nil
}

// joinLevels renders a sorted level list as "L2-5" / "L1, L3-4": a division
// missing a type usually misses it at every staffed level, and the run form
// keeps that warning to one line.
func joinLevels(levels []int) string {
	if len(levels) == 0 {
		return ""
	}
	var parts []string
	start, prev := levels[0], levels[0]
	flush := func() {
		switch {
		case start == prev:
			parts = append(parts, "L"+strconv.Itoa(start))
		case prev == start+1:
			parts = append(parts, "L"+strconv.Itoa(start)+", L"+strconv.Itoa(prev))
		default:
			parts = append(parts, "L"+strconv.Itoa(start)+"–"+strconv.Itoa(prev))
		}
	}
	for _, lvl := range levels[1:] {
		if lvl == prev+1 {
			prev = lvl
			continue
		}
		flush()
		start, prev = lvl, lvl
	}
	flush()
	return strings.Join(parts, ", ")
}

// ESPGetDungeonOptions returns dungeon VID/name pairs for dropdowns.
func ESPGetDungeonOptions(ctx context.Context, db *sql.DB) ([]ESPDungeonOption, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT dungeonVID, dungeonVName FROM dunDungeons ORDER BY dungeonVName`)
	if err != nil {
		return nil, fmt.Errorf("ESPGetDungeonOptions: %w", err)
	}
	defer rows.Close()
	var out []ESPDungeonOption
	for rows.Next() {
		var d ESPDungeonOption
		if err := rows.Scan(&d.DungeonVID, &d.Name); err != nil {
			return nil, fmt.Errorf("ESPGetDungeonOptions scan: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ESPGetModelPath looks up typeID → graphicID → url3D for 3D model resolution.
func ESPGetModelPath(ctx context.Context, db *sql.DB, typeID int64) (string, error) {
	var url3D sql.NullString
	err := db.QueryRowContext(ctx, `
		SELECT g.url3D
		FROM invTypes t
		JOIN eveGraphics g ON g.graphicID = t.graphicID
		WHERE t.typeID = ?`, typeID).Scan(&url3D)
	if err != nil {
		return "", fmt.Errorf("ESPGetModelPath: %w", err)
	}
	if !url3D.Valid || url3D.String == "" {
		return "", fmt.Errorf("no url3D for typeID %d", typeID)
	}
	return url3D.String, nil
}

// ── ESP solar-system search / Faction Territory queries ────────────────────────────

// ESPSystemSearchRow is one solar system match for the system search boxes.
type ESPSystemSearchRow struct {
	SolarSystemID     int64
	SystemName        string
	Security          float64
	RegionID          int64
	RegionName        string
	ConstellationID   int64
	ConstellationName string
	BeltCount         int
}

// ESPSearchSolarSystems finds solar systems by name (substring match).
func ESPSearchSolarSystems(ctx context.Context, db *sql.DB, query string) ([]ESPSystemSearchRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT s.solarSystemID, s.solarSystemName, s.security,
		       s.regionID, COALESCE(r.regionName, ''),
		       s.constellationID, COALESCE(c.constellationName, ''),
		       (SELECT COUNT(*) FROM mapDenormalize d WHERE d.solarSystemID = s.solarSystemID AND d.groupID = 9)
		FROM mapSolarSystems s
		LEFT JOIN mapRegions r ON r.regionID = s.regionID
		LEFT JOIN mapConstellations c ON c.constellationID = s.constellationID
		WHERE s.solarSystemName LIKE ?
		ORDER BY s.solarSystemName LIMIT 25`, "%"+query+"%")
	if err != nil {
		return nil, fmt.Errorf("ESPSearchSolarSystems: %w", err)
	}
	defer rows.Close()
	var out []ESPSystemSearchRow
	for rows.Next() {
		var r ESPSystemSearchRow
		if err := rows.Scan(&r.SolarSystemID, &r.SystemName, &r.Security,
			&r.RegionID, &r.RegionName, &r.ConstellationID, &r.ConstellationName,
			&r.BeltCount); err != nil {
			return nil, fmt.Errorf("ESPSearchSolarSystems scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ESPGetSolarSystemInfo loads one system row (same shape as the search) by ID.
func ESPGetSolarSystemInfo(ctx context.Context, db *sql.DB, systemID int64) (*ESPSystemSearchRow, error) {
	r := &ESPSystemSearchRow{}
	err := db.QueryRowContext(ctx, `
		SELECT s.solarSystemID, s.solarSystemName, s.security,
		       s.regionID, COALESCE(rg.regionName, ''),
		       s.constellationID, COALESCE(c.constellationName, ''),
		       (SELECT COUNT(*) FROM mapDenormalize d WHERE d.solarSystemID = s.solarSystemID AND d.groupID = 9)
		FROM mapSolarSystems s
		LEFT JOIN mapRegions rg ON rg.regionID = s.regionID
		LEFT JOIN mapConstellations c ON c.constellationID = s.constellationID
		WHERE s.solarSystemID = ?`, systemID,
	).Scan(&r.SolarSystemID, &r.SystemName, &r.Security,
		&r.RegionID, &r.RegionName, &r.ConstellationID, &r.ConstellationName,
		&r.BeltCount)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("ESPGetSolarSystemInfo(%d): %w", systemID, err)
	}
	return r, nil
}

// ESPSystemBeltTypes lists the typeID of every groupID-9 celestial in a system
// in itemID order, the order the asteroid generator walks them, so a survey can
// tell Ice Fields (typeID 17774) from Asteroid Belts (15).
func ESPSystemBeltTypes(ctx context.Context, db *sql.DB, systemID int64) ([]int64, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT typeID FROM mapDenormalize
		 WHERE solarSystemID = ? AND groupID = 9
		 ORDER BY itemID`, systemID)
	if err != nil {
		return nil, fmt.Errorf("ESPSystemBeltTypes(%d): %w", systemID, err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var typeID int64
		if err := rows.Scan(&typeID); err != nil {
			return nil, fmt.Errorf("ESPSystemBeltTypes scan: %w", err)
		}
		out = append(out, typeID)
	}
	return out, rows.Err()
}

// ESPGetTypeNames resolves invTypes names for a set of typeIDs.
func ESPGetTypeNames(ctx context.Context, db *sql.DB, ids []int64) map[int64]string {
	out := make(map[int64]string, len(ids))
	if len(ids) == 0 {
		return out
	}
	seen := make(map[int64]bool, len(ids))
	placeholders := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		placeholders = append(placeholders, "?")
		args = append(args, id)
	}
	rows, err := db.QueryContext(ctx,
		`SELECT typeID, typeName FROM invTypes WHERE typeID IN (`+strings.Join(placeholders, ",")+`)`, args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var name string
		if rows.Scan(&id, &name) == nil {
			out[id] = name
		}
	}
	return out
}

// espIDList renders a de-duplicated ID set as an IN-clause placeholder string
// and its argument slice. Returns ok=false for an empty set, where "IN ()" is
// a syntax error rather than an empty result.
func espIDList(ids []int64) (string, []any, bool) {
	seen := make(map[int64]bool, len(ids))
	placeholders := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		if id == 0 || seen[id] {
			continue
		}
		seen[id] = true
		placeholders = append(placeholders, "?")
		args = append(args, id)
	}
	if len(args) == 0 {
		return "", nil, false
	}
	return strings.Join(placeholders, ","), args, true
}

// espNameMap runs a two-column id/name query and collects it into a map.
// Name lookups decorate a page; a failure blanks a label rather than failing
// the render, so errors are dropped.
func espNameMap(ctx context.Context, db *sql.DB, query string, args []any) map[int64]string {
	out := make(map[int64]string)
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var name string
		if rows.Scan(&id, &name) == nil {
			out[id] = name
		}
	}
	return out
}

// ESPGetStationNames resolves station names for a set of stationIDs.
func ESPGetStationNames(ctx context.Context, db *sql.DB, ids []int64) map[int64]string {
	in, args, ok := espIDList(ids)
	if !ok {
		return map[int64]string{}
	}
	return espNameMap(ctx, db,
		`SELECT stationID, stationName FROM staStations WHERE stationID IN (`+in+`)`, args)
}

// ESPGetSystemNames resolves solar system names for a set of solarSystemIDs.
func ESPGetSystemNames(ctx context.Context, db *sql.DB, ids []int64) map[int64]string {
	in, args, ok := espIDList(ids)
	if !ok {
		return map[int64]string{}
	}
	return espNameMap(ctx, db,
		`SELECT solarSystemID, solarSystemName FROM mapSolarSystems WHERE solarSystemID IN (`+in+`)`, args)
}

// ESPGetCorpNames resolves corporation names for a set of corporationIDs,
// falling back to eveStaticOwners for NPC corps that have no corporation row.
func ESPGetCorpNames(ctx context.Context, db *sql.DB, ids []int64) map[int64]string {
	in, args, ok := espIDList(ids)
	if !ok {
		return map[int64]string{}
	}
	out := espNameMap(ctx, db,
		`SELECT corporationID, corporationName FROM corporation WHERE corporationID IN (`+in+`)`, args)
	static := espNameMap(ctx, db,
		`SELECT ownerID, ownerName FROM eveStaticOwners WHERE ownerID IN (`+in+`)`, args)
	for id, name := range static {
		if out[id] == "" {
			out[id] = name
		}
	}
	return out
}

// ── ESP Item Browser queries ─────────────────────────────────────────────────

// ESPItemRow is one row of the ESP item search, with the icon resolved from
// eveGraphics via invTypes.graphicID.
type ESPItemRow struct {
	TypeID    int64
	TypeName  string
	GroupName string
	Icon      string
}

// ESPSearchItems searches invTypes by name (SQL RLIKE, matching the in-game
// /search behavior) and resolves group name and icon for display.
func ESPSearchItems(ctx context.Context, db *sql.DB, query string) ([]ESPItemRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT t.typeID, t.typeName, COALESCE(g.groupName, ''), COALESCE(eg.icon, '')
		FROM invTypes t
		LEFT JOIN invGroups g ON g.groupID = t.groupID
		LEFT JOIN eveGraphics eg ON eg.graphicID = t.graphicID
		WHERE t.typeName RLIKE ?
		ORDER BY t.typeName`, query)
	if err != nil {
		return nil, fmt.Errorf("ESPSearchItems: %w", err)
	}
	defer rows.Close()

	var results []ESPItemRow
	for rows.Next() {
		var r ESPItemRow
		if err := rows.Scan(&r.TypeID, &r.TypeName, &r.GroupName, &r.Icon); err != nil {
			return nil, fmt.Errorf("ESPSearchItems scan: %w", err)
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// ESPGetTypeGraphic returns the graphicID and 2D icon (if any) for a typeID.
func ESPGetTypeGraphic(ctx context.Context, db *sql.DB, typeID int64) (int64, string, error) {
	var graphicID int64
	var icon string
	err := db.QueryRowContext(ctx, `
		SELECT COALESCE(t.graphicID, 0), COALESCE(eg.icon, '')
		FROM invTypes t
		LEFT JOIN eveGraphics eg ON eg.graphicID = t.graphicID
		WHERE t.typeID = ?`, typeID).Scan(&graphicID, &icon)
	if err != nil {
		return 0, "", fmt.Errorf("ESPGetTypeGraphic: %w", err)
	}
	return graphicID, icon, nil
}

// ESPGetAccountDetail returns a single account row by ID.
func ESPGetAccountDetail(ctx context.Context, db *sql.DB, accountID int64) (*ESPAccount, error) {
	a := &ESPAccount{}
	err := db.QueryRowContext(ctx, `
		SELECT a.accountid, a.accountname, a.role,
		       (SELECT COUNT(*) FROM character_ c WHERE c.accountID = a.accountid) AS charcount
		FROM account a
		WHERE a.accountid = ?`, accountID).Scan(&a.AccountID, &a.AccountName, &a.Role, &a.CharCount)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("ESPGetAccountDetail: %w", err)
	}
	return a, nil
}

// ESPDeleteAccount deletes an account and all its characters.
func ESPDeleteAccount(ctx context.Context, db *sql.DB, accountID int64) error {
	// Find all characters on this account.
	rows, err := db.QueryContext(ctx,
		`SELECT characterID FROM character_ WHERE accountID = ?`, accountID)
	if err != nil {
		return fmt.Errorf("ESPDeleteAccount list chars: %w", err)
	}
	var charIDs []int64
	for rows.Next() {
		var cid int64
		if err := rows.Scan(&cid); err != nil {
			rows.Close()
			return fmt.Errorf("ESPDeleteAccount scan char: %w", err)
		}
		charIDs = append(charIDs, cid)
	}
	rows.Close()

	// Delete each character (cascading cleanup).
	for _, cid := range charIDs {
		if err := DeleteCharacter(ctx, db, cid); err != nil {
			return fmt.Errorf("ESPDeleteAccount delete char %d: %w", cid, err)
		}
	}

	// Delete the account row.
	if _, err := db.ExecContext(ctx,
		`DELETE FROM account WHERE accountid = ?`, accountID); err != nil {
		return fmt.Errorf("ESPDeleteAccount: %w", err)
	}
	return nil
}
