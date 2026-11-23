package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math"
	"strings"
	"time"

	"gurimoon/marshal"
	"gurimoon/rates"
	"gurimoon/seclaw"
	"gurimoon/syncx"
)

// CharData holds a loaded character record used by SelectCharacterID to send
// the character-session SessionChangeNotification.
type CharData struct {
	CharacterID     int64
	CharacterName   string
	CorporationID   int64
	AllianceID      int64
	StationID       int64
	SolarSystemID   int64
	ConstellationID int64
	RegionID        int64
	// HQStationID is the session's hqID. The client ORs rolesAtHQ into corprole only
	// while stationid == hqID (sessions.py SetAttributes).
	HQStationID int64
}

// Column headers sent to the client in the util.Rowset for GetCharactersToSelect.
var charListCols = []string{
	"characterID", "characterName", "deletePrepareDateTime", "gender",
	"accessoryID", "beardID", "costumeID", "decoID", "eyebrowsID", "eyesID", "hairID",
	"lipstickID", "makeupID", "skinID", "backgroundID", "lightID",
	"headRotation1", "headRotation2", "headRotation3",
	"eyeRotation1", "eyeRotation2", "eyeRotation3",
	"camPos1", "camPos2", "camPos3",
	"morph1e", "morph1n", "morph1s", "morph1w",
	"morph2e", "morph2n", "morph2s", "morph2w",
	"morph3e", "morph3n", "morph3s", "morph3w",
	"morph4e", "morph4n", "morph4s", "morph4w",
}

// Column headers for GetCharacterToSelect.
var charSelectInfoCols = []string{
	"bloodlineID", "gender", "bounty", "corporationID", "title", "startDateTime",
	"securityRating", "balance", "stationID", "solarSystemID", "constellationID", "regionID",
	"petitionMessage", "logonMinutes", "tickerName",
}

// makeRowset constructs a util.Rowset PyObject from column names and value rows.
// RowClass is an ident string (IsType1) like DBResultToRowset. "columns" duplicates
// "header": util.Rowset.__init__ sets self.columns, but PyObject deserialization
// sets __dict__ directly and skips it.
func makeRowset(cols []string, rows [][]marshal.PyRep) marshal.PyRep {
	header := make([]marshal.PyRep, len(cols))
	for i, c := range cols {
		header[i] = marshal.PyString{Value: c}
	}
	lines := make([]marshal.PyRep, len(rows))
	for i, row := range rows {
		items := make([]marshal.PyRep, len(row))
		copy(items, row)
		lines[i] = marshal.PyList{Items: items}
	}
	headerList := marshal.PyList{Items: header}
	return marshal.PyObject{
		Type: "util.Rowset",
		Args: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "header"}, Value: headerList},
			// IsType1 encodes as opByteString (0x02), the ident encoding DBResultToRowset uses.
			{Key: marshal.PyString{Value: "RowClass"}, Value: marshal.PyString{Value: "util.Row", IsType1: true}},
			{Key: marshal.PyString{Value: "lines"}, Value: marshal.PyList{Items: lines}},
			// columns aliases header in util.Rowset; client code reads asks.columns (RefreshJumps).
			{Key: marshal.PyString{Value: "columns"}, Value: headerList},
		}},
	}
}

func intRep(v int64) marshal.PyRep     { return marshal.PyInt{Value: uint64(v)} }
func floatRep(v float64) marshal.PyRep { return marshal.PyReal{Value: v} }
func strRep(v string) marshal.PyRep    { return marshal.PyString{Value: v} }

// makeKeyVal constructs a util.KeyVal PyObject from column names and values,
// mirroring the C++ DBRowToKeyVal helper in common/EVEDBUtils.cpp.
func makeKeyVal(cols []string, vals []marshal.PyRep) marshal.PyRep {
	items := make([]marshal.DictEntry, len(cols))
	for i, c := range cols {
		items[i] = marshal.DictEntry{Key: marshal.PyString{Value: c}, Value: vals[i]}
	}
	return marshal.PyObject{
		Type: "util.KeyVal",
		Args: marshal.PyDict{Items: items},
	}
}

// makeRow constructs a util.Row PyObject from column names and values,
// mirroring the C++ DBRowToRow helper in common/EVEDBUtils.cpp.
func makeRow(cols []string, vals []marshal.PyRep) marshal.PyRep {
	return makeTypedRow("util.Row", cols, vals)
}

// makeTypedRow constructs a Row PyObject with a custom type name, used for
// types like "dbrow.SolarSystemRow" that share the same wire format as util.Row
// but have a different type identifier.
func makeTypedRow(typeName string, cols []string, vals []marshal.PyRep) marshal.PyRep {
	header := make([]marshal.PyRep, len(cols))
	for i, c := range cols {
		header[i] = marshal.PyString{Value: c}
	}
	line := make([]marshal.PyRep, len(vals))
	copy(line, vals)
	return marshal.PyObject{
		Type: typeName,
		Args: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "header"}, Value: marshal.PyList{Items: header}},
			{Key: marshal.PyString{Value: "line"}, Value: marshal.PyList{Items: line}},
		}},
	}
}

// makeTupleSet constructs a Tuple[2] response mirroring DBResultToTupleSet:
//
//	[0] = PyList[colName, ...]
//	[1] = PyList[PyList[val, ...], ...]  (one inner list per row)
func makeTupleSet(cols []string, rows [][]marshal.PyRep) marshal.PyRep {
	if len(cols) == 0 {
		return marshal.PyTuple{}
	}
	header := make([]marshal.PyRep, len(cols))
	for i, c := range cols {
		header[i] = marshal.PyString{Value: c}
	}
	lines := make([]marshal.PyRep, len(rows))
	for i, row := range rows {
		items := make([]marshal.PyRep, len(row))
		copy(items, row)
		lines[i] = marshal.PyList{Items: items}
	}
	return marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyList{Items: header},
		marshal.PyList{Items: lines},
	}}
}

// makeRowList constructs a Tuple[2] response mirroring DBResultToRowList("util.Row"):
//
//	[0] = PyList[colName, ...]
//	[1] = PyList[util.Row{header, line}, ...]  (one util.Row per result row)
func makeRowList(cols []string, rows [][]marshal.PyRep) marshal.PyRep {
	if len(cols) == 0 {
		return marshal.PyTuple{}
	}
	header := make([]marshal.PyRep, len(cols))
	for i, c := range cols {
		header[i] = marshal.PyString{Value: c}
	}
	lines := make([]marshal.PyRep, len(rows))
	for i, row := range rows {
		lines[i] = makeRow(cols, row)
	}
	return marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyList{Items: header},
		marshal.PyList{Items: lines},
	}}
}

// makeIndexRowset constructs a util.IndexRowset PyObject,
// mirroring DBResultToIndexRowset in common/EVEDBUtils.cpp.
func makeIndexRowset(keyCol string, cols []string, rows [][]marshal.PyRep) marshal.PyRep {
	if len(cols) == 0 {
		return marshal.PyObject{Type: "util.IndexRowset", Args: marshal.PyDict{}}
	}
	header := make([]marshal.PyRep, len(cols))
	keyIdx := 0
	for i, c := range cols {
		header[i] = marshal.PyString{Value: c}
		if c == keyCol {
			keyIdx = i
		}
	}
	items := make([]marshal.DictEntry, len(rows))
	for i, row := range rows {
		vals := make([]marshal.PyRep, len(row))
		copy(vals, row)
		items[i] = marshal.DictEntry{Key: row[keyIdx], Value: marshal.PyList{Items: vals}}
	}
	return marshal.PyObject{
		Type: "util.IndexRowset",
		Args: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "RowClass"}, Value: marshal.PyString{Value: "util.Row", IsType1: true}},
			{Key: marshal.PyString{Value: "idName"}, Value: marshal.PyString{Value: keyCol}},
			{Key: marshal.PyString{Value: "header"}, Value: marshal.PyList{Items: header}},
			{Key: marshal.PyString{Value: "items"}, Value: marshal.PyDict{Items: items}},
		}},
	}
}

// morphRep returns PyNone for zero morph values (matching what the client
// expects for "no morph data") and PyReal for non-zero values.
func morphRep(v float64) marshal.PyRep {
	if v == 0 {
		return marshal.PyNone{}
	}
	return marshal.PyReal{Value: v}
}

func nullIntRep(v sql.NullInt64) marshal.PyRep {
	if !v.Valid {
		return marshal.PyNone{}
	}
	return marshal.PyInt{Value: uint64(v.Int64)}
}

func nullFloatRep(v sql.NullFloat64) marshal.PyRep {
	if !v.Valid {
		return marshal.PyNone{}
	}
	return marshal.PyReal{Value: v.Float64}
}

func nullStrRep(v sql.NullString) marshal.PyRep {
	if !v.Valid {
		return marshal.PyNone{}
	}
	return marshal.PyString{Value: v.String}
}

// EnsureCharacterDeleteSchema adds the deletePrepareDateTime column to
// character_ if it is missing. Follows the EnsureJournalSchema pattern.
func EnsureCharacterDeleteSchema(ctx context.Context, db *sql.DB) error {
	var n int64
	err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE()
		  AND TABLE_NAME   = 'character_'
		  AND COLUMN_NAME  = 'deletePrepareDateTime'`).Scan(&n)
	if err != nil {
		return fmt.Errorf("EnsureCharacterDeleteSchema probe: %w", err)
	}
	if n > 0 {
		return nil
	}
	if _, err := db.ExecContext(ctx,
		`ALTER TABLE character_ ADD COLUMN deletePrepareDateTime BIGINT NULL DEFAULT NULL`); err != nil {
		return fmt.Errorf("EnsureCharacterDeleteSchema alter: %w", err)
	}
	log.Printf("[db] EnsureCharacterDeleteSchema: added deletePrepareDateTime column")
	return nil
}

// SetCharacterDeletePrepare sets the deletion countdown timestamp.
func SetCharacterDeletePrepare(ctx context.Context, db *sql.DB, charID int64, filetime uint64) error {
	_, err := db.ExecContext(ctx,
		`UPDATE character_ SET deletePrepareDateTime = ? WHERE characterID = ?`,
		int64(filetime), charID)
	if err != nil {
		return fmt.Errorf("SetCharacterDeletePrepare: %w", err)
	}
	return nil
}

// ClearCharacterDeletePrepare cancels a pending deletion.
func ClearCharacterDeletePrepare(ctx context.Context, db *sql.DB, charID int64) error {
	_, err := db.ExecContext(ctx,
		`UPDATE character_ SET deletePrepareDateTime = NULL WHERE characterID = ?`, charID)
	if err != nil {
		return fmt.Errorf("ClearCharacterDeletePrepare: %w", err)
	}
	return nil
}

// GetCharacterDeletePrepare returns the deletion-countdown FILETIME for a
// character. ok is false when no deletion is pending (NULL column).
func GetCharacterDeletePrepare(ctx context.Context, db *sql.DB, charID int64) (uint64, bool, error) {
	var v sql.NullInt64
	err := db.QueryRowContext(ctx,
		`SELECT deletePrepareDateTime FROM character_ WHERE characterID = ?`,
		charID).Scan(&v)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("GetCharacterDeletePrepare: %w", err)
	}
	if !v.Valid {
		return 0, false, nil
	}
	return uint64(v.Int64), true, nil
}

// GetCharacterList returns a util.Rowset of all characters belonging to accountID,
// as required by character.GetCharactersToSelect.
func GetCharacterList(ctx context.Context, db *sql.DB, accountID int64) (marshal.PyRep, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT characterID, characterName,
		       COALESCE(deletePrepareDateTime, 0) AS deletePrepareDateTime, gender,
		       accessoryID, beardID, costumeID, decoID, eyebrowsID, eyesID, hairID,
		       lipstickID, makeupID, skinID, backgroundID, lightID,
		       headRotation1, headRotation2, headRotation3,
		       eyeRotation1, eyeRotation2, eyeRotation3,
		       camPos1, camPos2, camPos3,
		       morph1e, morph1n, morph1s, morph1w,
		       morph2e, morph2n, morph2s, morph2w,
		       morph3e, morph3n, morph3s, morph3w,
		       morph4e, morph4n, morph4s, morph4w
		FROM character_
		WHERE accountID = ?
		  AND characterID NOT IN (SELECT characterID FROM chrRecustomization)`, accountID)
	if err != nil {
		return nil, fmt.Errorf("GetCharacterList: %w", err)
	}
	defer rows.Close()

	var result [][]marshal.PyRep
	for rows.Next() {
		var (
			characterID, deletePrepareDateTime, gender  int64
			characterName                               string
			accessoryID, beardID, decoID                sql.NullInt64
			lipstickID, makeupID                        sql.NullInt64
			costumeID, eyebrowsID, eyesID               int64
			hairID, skinID, backgroundID, lightID       int64
			headRotation1, headRotation2, headRotation3 float64
			eyeRotation1, eyeRotation2, eyeRotation3    float64
			camPos1, camPos2, camPos3                   float64
			morph1e, morph1n, morph1s, morph1w          sql.NullFloat64
			morph2e, morph2n, morph2s, morph2w          sql.NullFloat64
			morph3e, morph3n, morph3s, morph3w          sql.NullFloat64
			morph4e, morph4n, morph4s, morph4w          sql.NullFloat64
		)
		if err := rows.Scan(
			&characterID, &characterName, &deletePrepareDateTime, &gender,
			&accessoryID, &beardID, &costumeID, &decoID, &eyebrowsID, &eyesID, &hairID,
			&lipstickID, &makeupID, &skinID, &backgroundID, &lightID,
			&headRotation1, &headRotation2, &headRotation3,
			&eyeRotation1, &eyeRotation2, &eyeRotation3,
			&camPos1, &camPos2, &camPos3,
			&morph1e, &morph1n, &morph1s, &morph1w,
			&morph2e, &morph2n, &morph2s, &morph2w,
			&morph3e, &morph3n, &morph3s, &morph3w,
			&morph4e, &morph4n, &morph4s, &morph4w,
		); err != nil {
			return nil, fmt.Errorf("GetCharacterList scan: %w", err)
		}
		row := []marshal.PyRep{
			intRep(characterID), strRep(characterName), intRep(deletePrepareDateTime), intRep(gender),
			nullIntRep(accessoryID), nullIntRep(beardID), intRep(costumeID), nullIntRep(decoID),
			intRep(eyebrowsID), intRep(eyesID), intRep(hairID),
			nullIntRep(lipstickID), nullIntRep(makeupID), intRep(skinID), intRep(backgroundID), intRep(lightID),
			floatRep(headRotation1), floatRep(headRotation2), floatRep(headRotation3),
			floatRep(eyeRotation1), floatRep(eyeRotation2), floatRep(eyeRotation3),
			floatRep(camPos1), floatRep(camPos2), floatRep(camPos3),
			nullFloatRep(morph1e), nullFloatRep(morph1n), nullFloatRep(morph1s), nullFloatRep(morph1w),
			nullFloatRep(morph2e), nullFloatRep(morph2n), nullFloatRep(morph2s), nullFloatRep(morph2w),
			nullFloatRep(morph3e), nullFloatRep(morph3n), nullFloatRep(morph3s), nullFloatRep(morph3w),
			nullFloatRep(morph4e), nullFloatRep(morph4n), nullFloatRep(morph4s), nullFloatRep(morph4w),
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("GetCharacterList rows: %w", err)
	}
	return makeRowset(charListCols, result), nil
}

// GetCharSelectInfo returns a util.Rowset with selection-screen detail for one
// character, as required by character.GetCharacterToSelect.
func GetCharSelectInfo(ctx context.Context, db *sql.DB, characterID int64) (marshal.PyRep, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT c.bloodlineID, c.gender, c.bounty, c.corporationID, c.title, c.startDateTime,
		       c.securityRating, c.balance, c.stationID, c.solarSystemID, c.constellationID, c.regionID,
		       c.petitionMessage, c.logonMinutes, corp.tickerName
		FROM character_ c
		LEFT JOIN corporation corp ON c.corporationID = corp.corporationID
		WHERE c.characterID = ?`, characterID)
	if err != nil {
		return nil, fmt.Errorf("GetCharSelectInfo: %w", err)
	}
	defer rows.Close()

	var result [][]marshal.PyRep
	for rows.Next() {
		var (
			bloodlineID, gender             int64
			bounty, securityRating, balance float64
			corporationID                   int64
			title, petitionMessage          string
			startDateTime                   int64
			stationID, solarSystemID        int64
			constellationID, regionID       int64
			logonMinutes                    int64
			tickerName                      sql.NullString
		)
		if err := rows.Scan(
			&bloodlineID, &gender, &bounty, &corporationID, &title, &startDateTime,
			&securityRating, &balance, &stationID, &solarSystemID, &constellationID, &regionID,
			&petitionMessage, &logonMinutes, &tickerName,
		); err != nil {
			return nil, fmt.Errorf("GetCharSelectInfo scan: %w", err)
		}
		row := []marshal.PyRep{
			intRep(bloodlineID), intRep(gender), floatRep(bounty), intRep(corporationID),
			strRep(title), intRep(startDateTime),
			floatRep(securityRating), floatRep(balance),
			intRep(stationID), intRep(solarSystemID), intRep(constellationID), intRep(regionID),
			strRep(petitionMessage), intRep(logonMinutes), nullStrRep(tickerName),
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("GetCharSelectInfo rows: %w", err)
	}
	return makeRowset(charSelectInfoCols, result), nil
}

// GetCurrentShipID returns the ship itemID for a character by looking up the
// character's locationID in the entity table.  Returns 0 if not found.
func GetCurrentShipID(ctx context.Context, db *sql.DB, characterID int64) (int64, error) {
	var locationID int64
	err := db.QueryRowContext(ctx,
		`SELECT locationID FROM entity WHERE itemID = ?`, characterID,
	).Scan(&locationID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("GetCurrentShipID: %w", err)
	}
	return locationID, nil
}

// ValidateCharName returns true when name is available (no existing character
// uses it) and false when it is already taken.  An error is returned only on
// a database failure.
func ValidateCharName(ctx context.Context, db *sql.DB, name string) (bool, error) {
	var exists int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM character_ WHERE characterName = ?`, name,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("ValidateCharName: %w", err)
	}
	return exists == 0, nil
}

// SchoolInfo holds the location/corp details fetched for a character's starting school.
type SchoolInfo struct {
	CorporationID   int64
	StationID       int64
	SolarSystemID   int64
	ConstellationID int64
	RegionID        int64
}

// GetSchoolInfo looks up the corporation and station for schoolID, joining
// chrSchools → corporation → staStations, mirroring
// CharacterDB::GetLocationCorporationFromSchool.
func GetSchoolInfo(ctx context.Context, db *sql.DB, schoolID int64) (*SchoolInfo, error) {
	var si SchoolInfo
	err := db.QueryRowContext(ctx, `
		SELECT s.corporationID, corp.stationID,
		       st.solarSystemID, st.constellationID, st.regionID
		FROM chrSchools s
		  LEFT JOIN corporation corp ON corp.corporationID = s.corporationID
		  LEFT JOIN staStations st   ON st.stationID = corp.stationID
		WHERE s.schoolID = ?`, schoolID,
	).Scan(&si.CorporationID, &si.StationID, &si.SolarSystemID, &si.ConstellationID, &si.RegionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("GetSchoolInfo: school %d not found", schoolID)
		}
		return nil, fmt.Errorf("GetSchoolInfo: %w", err)
	}
	return &si, nil
}

// BloodlineInfo holds the base attributes and typeID for a bloodline.
type BloodlineInfo struct {
	TypeID       int64
	RaceID       int64
	Intelligence int64
	Charisma     int64
	Perception   int64
	Memory       int64
	Willpower    int64
	SkillTypeID1 sql.NullInt64
	SkillTypeID2 sql.NullInt64
	ShipTypeID   int64
}

// GetBloodlineInfo looks up typeID, raceID, base attributes, and starting skills
// for bloodlineID, matching CharacterDB::GetAttributesFromBloodline and GetSkillsFromBloodline.
func GetBloodlineInfo(ctx context.Context, db *sql.DB, bloodlineID int64) (*BloodlineInfo, error) {
	var bi BloodlineInfo
	err := db.QueryRowContext(ctx, `
		SELECT bt.typeID,
		       b.raceID, b.intelligence, b.charisma, b.perception, b.memory, b.willpower,
		       b.skillTypeID1, b.skillTypeID2, b.shipTypeID
		FROM chrBloodlines b
		  LEFT JOIN bloodlineTypes bt ON bt.bloodlineID = b.bloodlineID
		WHERE b.bloodlineID = ?`, bloodlineID,
	).Scan(
		&bi.TypeID, &bi.RaceID,
		&bi.Intelligence, &bi.Charisma, &bi.Perception, &bi.Memory, &bi.Willpower,
		&bi.SkillTypeID1, &bi.SkillTypeID2, &bi.ShipTypeID,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("GetBloodlineInfo: bloodline %d not found", bloodlineID)
		}
		return nil, fmt.Errorf("GetBloodlineInfo: %w", err)
	}
	return &bi, nil
}

// AncestryInfo holds the attribute bonuses and starting skills for an ancestry.
type AncestryInfo struct {
	Intelligence sql.NullInt64
	Charisma     sql.NullInt64
	Perception   sql.NullInt64
	Memory       sql.NullInt64
	Willpower    sql.NullInt64
	SkillTypeID1 sql.NullInt64
	SkillTypeID2 sql.NullInt64
}

// GetAncestryInfo fetches attribute bonuses and skillTypeIDs for ancestryID,
// mirroring CharacterDB::GetAttributesFromAncestry and GetSkillsFromAncestry.
func GetAncestryInfo(ctx context.Context, db *sql.DB, ancestryID int64) (*AncestryInfo, error) {
	var ai AncestryInfo
	err := db.QueryRowContext(ctx, `
		SELECT intelligence, charisma, perception, memory, willpower,
		       skillTypeID1, skillTypeID2
		FROM chrAncestries WHERE ancestryID = ?`, ancestryID,
	).Scan(
		&ai.Intelligence, &ai.Charisma, &ai.Perception, &ai.Memory, &ai.Willpower,
		&ai.SkillTypeID1, &ai.SkillTypeID2,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("GetAncestryInfo: ancestry %d not found", ancestryID)
		}
		return nil, fmt.Errorf("GetAncestryInfo: %w", err)
	}
	return &ai, nil
}

// GetRaceSkill returns the skillTypeID1 for the race of a bloodline, used for
// the race starting skill (GetSkillsFromBloodline → chrRaces.skillTypeID1).
func GetRaceSkill(ctx context.Context, db *sql.DB, raceID int64) (sql.NullInt64, error) {
	var skillID sql.NullInt64
	err := db.QueryRowContext(ctx,
		`SELECT skillTypeID1 FROM chrRaces WHERE raceID = ?`, raceID,
	).Scan(&skillID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return sql.NullInt64{}, nil
		}
		return sql.NullInt64{}, fmt.Errorf("GetRaceSkill: %w", err)
	}
	return skillID, nil
}

// GetDepartmentSkills returns the up-to-3 starting skill typeIDs for departmentID,
// mirroring CharacterDB::GetSkillsFromDepartment.
func GetDepartmentSkills(ctx context.Context, db *sql.DB, departmentID int64) ([]int64, error) {
	var s1, s2, s3 sql.NullInt64
	err := db.QueryRowContext(ctx,
		`SELECT skillTypeID1, skillTypeID2, skillTypeID3 FROM chrDepartments WHERE departmentID = ?`,
		departmentID,
	).Scan(&s1, &s2, &s3)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetDepartmentSkills: %w", err)
	}
	return nullIntsToSlice(s1, s2, s3), nil
}

// GetFieldSkills returns the up-to-2 starting skill typeIDs for fieldID.
func GetFieldSkills(ctx context.Context, db *sql.DB, fieldID int64) ([]int64, error) {
	var s1, s2 sql.NullInt64
	err := db.QueryRowContext(ctx,
		`SELECT skillTypeID1, skillTypeID2 FROM chrFields WHERE fieldID = ?`, fieldID,
	).Scan(&s1, &s2)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetFieldSkills: %w", err)
	}
	return nullIntsToSlice(s1, s2), nil
}

// GetSpecialitySkills returns the up-to-2 starting skill typeIDs for specialityID.
func GetSpecialitySkills(ctx context.Context, db *sql.DB, specialityID int64) ([]int64, error) {
	var s1, s2 sql.NullInt64
	err := db.QueryRowContext(ctx,
		`SELECT skillTypeID1, skillTypeID2 FROM chrSpecialities WHERE specialityID = ?`, specialityID,
	).Scan(&s1, &s2)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetSpecialitySkills: %w", err)
	}
	return nullIntsToSlice(s1, s2), nil
}

// nullIntsToSlice converts nullable int64 values to a slice of non-null values.
func nullIntsToSlice(vals ...sql.NullInt64) []int64 {
	var out []int64
	for _, v := range vals {
		if v.Valid && v.Int64 != 0 {
			out = append(out, v.Int64)
		}
	}
	return out
}

// GetSkillTimeConstant returns a skill's skillTimeConstant (attributeID 275), the
// rank that sizes starting skill points. Rank is stored in valueFloat in the seed
// data, so both columns are coalesced. Returns 1 when the row is missing.
func GetSkillTimeConstant(ctx context.Context, db *sql.DB, typeID int64) (int64, error) {
	var tc sql.NullFloat64
	err := db.QueryRowContext(ctx,
		`SELECT COALESCE(valueFloat, CAST(valueInt AS DECIMAL(20,6)))
		   FROM dgmTypeAttributes WHERE typeID = ? AND attributeID = 275`,
		typeID,
	).Scan(&tc)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 1, nil
		}
		return 1, fmt.Errorf("GetSkillTimeConstant typeID=%d: %w", typeID, err)
	}
	if tc.Valid && tc.Float64 > 0 {
		return int64(tc.Float64), nil
	}
	return 1, nil
}

// CharCreationInput holds the parsed arguments from character.CreateCharacter.
type CharCreationInput struct {
	Name         string
	BloodlineID  int64
	GenderID     int64
	AncestryID   int64
	SchoolID     int64
	DepartmentID int64
	FieldID      int64
	SpecialityID int64
	Intelligence int64
	Charisma     int64
	Perception   int64
	Memory       int64
	Willpower    int64
	Appearance   map[string]interface{} // string → int64 or float64
}

// CreateCharacter inserts a new character and returns its characterID. The
// starting location is the school corporation's station (GetSchoolInfo).
func CreateCharacter(ctx context.Context, db *sql.DB, accountID int64, in *CharCreationInput, startBalance float64) (int64, error) {
	now := win32TimeNow()

	bi, err := GetBloodlineInfo(ctx, db, in.BloodlineID)
	if err != nil {
		return 0, err
	}

	si, err := GetSchoolInfo(ctx, db, in.SchoolID)
	if err != nil {
		return 0, err
	}

	// The client sends only the chargen bonus picks (5 points over the five
	// attributes); the stored base is bloodline base plus pick.
	intelligence := in.Intelligence
	charisma := in.Charisma
	perception := in.Perception
	memory := in.Memory
	willpower := in.Willpower

	app := parseAppearance(in.Appearance)

	res, err := db.ExecContext(ctx, `
		INSERT INTO entity
		  (itemName, typeID, ownerID, locationID, flag, contraband, singleton, quantity, x, y, z, customInfo)
		VALUES (?, ?, ?, ?, 57, 0, 1, 1, 0, 0, 0, NULL)`,
		in.Name, bi.TypeID, accountID, si.StationID,
	)
	if err != nil {
		return 0, fmt.Errorf("CreateCharacter entity insert: %w", err)
	}
	charID, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("CreateCharacter LastInsertId: %w", err)
	}

	// Create the starting capsule (typeID 670) and move the character into it, so
	// GetCurrentShipID answers the capsule rather than the station.
	if capsRes, capsErr := db.ExecContext(ctx, `
		INSERT INTO entity
		  (itemName, typeID, ownerID, locationID, flag, contraband, singleton, quantity, x, y, z, customInfo)
		VALUES (?, 670, ?, ?, 4, 0, 1, 1, 0, 0, 0, NULL)`,
		in.Name+"'s Capsule", charID, si.StationID,
	); capsErr == nil {
		if capsID, capsIDErr := capsRes.LastInsertId(); capsIDErr == nil && capsID != 0 {
			_, _ = db.ExecContext(ctx,
				`UPDATE entity SET locationID = ? WHERE itemID = ?`, capsID, charID)
		}
	}

	_, err = db.ExecContext(ctx, `
		INSERT INTO character_ (
			characterID, characterName, accountID, title, description, typeID,
			createDateTime, startDateTime,
			bounty, balance, securityRating, petitionMessage, logonMinutes,
			corporationID, corporationDateTime,
			stationID, solarSystemID, constellationID, regionID,
			raceID, bloodlineID, gender,
			accessoryID, beardID, costumeID, decoID, eyebrowsID, eyesID,
			hairID, lipstickID, makeupID, skinID, backgroundID, lightID,
			headRotation1, headRotation2, headRotation3,
			eyeRotation1, eyeRotation2, eyeRotation3,
			camPos1, camPos2, camPos3,
			morph1e, morph1n, morph1s, morph1w,
			morph2e, morph2n, morph2s, morph2w,
			morph3e, morph3n, morph3s, morph3w,
			morph4e, morph4n, morph4s, morph4w
		) VALUES (
			?, ?, ?, '', '', ?,
			?, ?,
			0, ?, 0, 0, 13,
			?, ?,
			?, ?, ?, ?,
			?, ?, ?,
			?, ?, ?, ?, ?, ?,
			?, ?, ?, ?, ?, ?,
			?, ?, ?,
			?, ?, ?,
			?, ?, ?,
			?, ?, ?, ?,
			?, ?, ?, ?,
			?, ?, ?, ?,
			?, ?, ?, ?
		)`,
		charID, in.Name, accountID, bi.TypeID,
		now, now,
		startBalance,
		si.CorporationID, now,
		si.StationID, si.SolarSystemID, si.ConstellationID, si.RegionID,
		bi.RaceID, in.BloodlineID, in.GenderID,
		app.accessoryID, app.beardID, app.costumeID, app.decoID, app.eyebrowsID, app.eyesID,
		app.hairID, app.lipstickID, app.makeupID, app.skinID, app.backgroundID, app.lightID,
		app.headRotation1, app.headRotation2, app.headRotation3,
		app.eyeRotation1, app.eyeRotation2, app.eyeRotation3,
		app.camPos1, app.camPos2, app.camPos3,
		app.morph1e, app.morph1n, app.morph1s, app.morph1w,
		app.morph2e, app.morph2n, app.morph2s, app.morph2w,
		app.morph3e, app.morph3n, app.morph3s, app.morph3w,
		app.morph4e, app.morph4n, app.morph4s, app.morph4w,
	)
	if err != nil {
		// Roll back the entity row on character_ insert failure.
		_, _ = db.ExecContext(ctx, `DELETE FROM entity WHERE itemID = ?`, charID)
		return 0, fmt.Errorf("CreateCharacter character_ insert: %w", err)
	}

	// Base attributes: bloodline base plus the chargen pick under 164-168 (charisma,
	// intelligence, memory, perception, willpower). 175-179 are the *Bonus IDs; never base.
	attrs := []struct {
		id  int64
		val int64
	}{
		{164, bi.Charisma + charisma},
		{165, bi.Intelligence + intelligence},
		{166, bi.Memory + memory},
		{167, bi.Perception + perception},
		{168, bi.Willpower + willpower},
	}
	for _, attr := range attrs {
		_, _ = db.ExecContext(ctx,
			`INSERT INTO entity_attributes (itemID, attributeID, valueInt) VALUES (?, ?, ?)`,
			charID, attr.id, attr.val)
	}

	// Set home station to school station for new characters.
	_, _ = db.ExecContext(ctx,
		`UPDATE character_ SET homeStationID = ? WHERE characterID = ?`,
		si.StationID, charID)

	// Initial channels row.
	_, _ = db.ExecContext(ctx,
		`INSERT INTO channels VALUES(?, ?, NULL, NULL, NULL, 1, NULL, 1, 100, 0, 15, 1, 0)`,
		charID, charID)

	// Initial employment record.
	_, _ = db.ExecContext(ctx,
		`INSERT INTO chrEmployment VALUES(?, ?, ?, 0)`,
		charID, si.CorporationID, now)

	// Initial empire faction standing based on race.
	if empireFaction := raceToEmpireFaction(bi.RaceID); empireFaction != 0 {
		_, _ = db.ExecContext(ctx,
			`INSERT INTO chrNPCStandings (characterID, fromID, standing) VALUES (?, ?, 1.00)`,
			charID, empireFaction)
	}

	// Collect starting skill typeIDs from all five sources.
	var skills []int64
	if bi.SkillTypeID1.Valid && bi.SkillTypeID1.Int64 != 0 {
		skills = append(skills, bi.SkillTypeID1.Int64)
	}
	if bi.SkillTypeID2.Valid && bi.SkillTypeID2.Int64 != 0 {
		skills = append(skills, bi.SkillTypeID2.Int64)
	}
	raceSkill, _ := GetRaceSkill(ctx, db, bi.RaceID)
	if raceSkill.Valid && raceSkill.Int64 != 0 {
		skills = append(skills, raceSkill.Int64)
	}
	ai, _ := GetAncestryInfo(ctx, db, in.AncestryID)
	if ai != nil {
		skills = append(skills, nullIntsToSlice(ai.SkillTypeID1, ai.SkillTypeID2)...)
	}
	deptSkills, _ := GetDepartmentSkills(ctx, db, in.DepartmentID)
	skills = append(skills, deptSkills...)
	fieldSkills, _ := GetFieldSkills(ctx, db, in.FieldID)
	skills = append(skills, fieldSkills...)
	specSkills, _ := GetSpecialitySkills(ctx, db, in.SpecialityID)
	skills = append(skills, specSkills...)

	// Every new character gets Spaceship Command (3327) at level 1.
	skills = append(skills, 3327)

	// A skill granted by several sources starts at that many levels, capped at 5.
	skillCount := make(map[int64]int)
	var skillOrder []int64
	for _, s := range skills {
		if s == 0 {
			continue
		}
		if skillCount[s] == 0 {
			skillOrder = append(skillOrder, s)
		}
		skillCount[s]++
	}
	// A Jove (race 16) starts at Spaceship Command V: the Jove hull skills require it.
	// See docs/character-skills.md#jove-starting-skills.
	if bi.RaceID == raceJove {
		if skillCount[skillSpaceshipCommand] == 0 {
			skillOrder = append(skillOrder, skillSpaceshipCommand)
		}
		skillCount[skillSpaceshipCommand] = 5
	}

	// Skills live in the character (locationID = charID, flag 7), never in the station:
	// godma.Prime needs the parent on session change.
	for _, skillType := range skillOrder {
		r, err := db.ExecContext(ctx, `
			INSERT INTO entity
			  (itemName, typeID, ownerID, locationID, flag, contraband, singleton, quantity, x, y, z, customInfo)
			VALUES ('Skill', ?, ?, ?, 7, 0, 1, 1, 0, 0, 0, NULL)`,
			skillType, charID, charID,
		)
		if err != nil {
			continue // skip skills that fail; non-fatal
		}
		skillItemID, err := r.LastInsertId()
		if err != nil {
			continue
		}
		level := skillCount[skillType]
		if level > 5 {
			level = 5
		}
		tc, _ := GetSkillTimeConstant(ctx, db, skillType)
		skillPoints := int64(skillSPForLevel(tc, level))
		// attributeID 280 = skillLevel, 276 = skillPoints.
		_, _ = db.ExecContext(ctx,
			`INSERT INTO entity_attributes (itemID, attributeID, valueInt, valueFloat) VALUES (?, 280, ?, NULL)`,
			skillItemID, level)
		_, _ = db.ExecContext(ctx,
			`INSERT INTO entity_attributes (itemID, attributeID, valueInt, valueFloat) VALUES (?, 276, ?, NULL)`,
			skillItemID, skillPoints)
	}

	// Write the five base stats again with INSERT IGNORE (164-168), for any row the
	// first pass did not create.
	type stat struct {
		attrID int64
		val    int64
	}
	charStats := []stat{
		{164, in.Charisma},
		{165, in.Intelligence},
		{166, in.Memory},
		{167, in.Perception},
		{168, in.Willpower},
	}
	// A zero stat is stored as 20: godma divides by (primary + secondary/2) for
	// training time, and a zero raises ZeroDivisionError in the skill menu.
	for _, s := range charStats {
		val := s.val
		if val <= 0 {
			val = 20
		}
		_, _ = db.ExecContext(ctx,
			`INSERT IGNORE INTO entity_attributes (itemID, attributeID, valueInt, valueFloat)
			 VALUES (?, ?, ?, NULL)`,
			charID, s.attrID, val)
	}

	return charID, nil
}

// win32TimeNow returns the current UTC time as a Win32 FILETIME (100-ns intervals
// since 1601-01-01), matching win32TimeNow() in cache/store.go.
func win32TimeNow() uint64 {
	return uint64(time.Now().UnixNano()/100) + 116444736000000000
}

// win32Epoch1970 is the Win32 FILETIME value of 1970-01-01.  Anything at or
// below it in a FILETIME column is unix seconds written by mistake.
const win32Epoch1970 = 116444736000000000

// RepairCharacterCorpStartDates sets character_.startDateTime, the date a character
// joined its current corporation, from that corp's chrEmployment row. The client
// reads it as corp membership age, never as a birth date. Rows at or below
// win32Epoch1970 are skipped; the WHERE makes later boots a no-op.
func RepairCharacterCorpStartDates(ctx context.Context, db *sql.DB) error {
	res, err := db.ExecContext(ctx, `
		UPDATE character_ c
		JOIN (
			SELECT characterID, corporationID, MAX(startDate) AS joined
			FROM chrEmployment
			GROUP BY characterID, corporationID
		) e ON e.characterID = c.characterID AND e.corporationID = c.corporationID
		SET c.startDateTime = e.joined
		WHERE e.joined > ? AND c.startDateTime <> e.joined`, win32Epoch1970)
	if err != nil {
		return fmt.Errorf("RepairCharacterCorpStartDates: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("RepairCharacterCorpStartDates: corrected %d character corp start dates", n)
	}
	return nil
}

// charAppearance holds the numeric appearance fields from CreateCharacter arg[13].
type charAppearance struct {
	costumeID                                   int64
	eyebrowsID                                  int64
	eyesID                                      int64
	hairID                                      int64
	skinID                                      int64
	backgroundID                                int64
	lightID                                     int64
	accessoryID                                 interface{}
	beardID                                     interface{}
	decoID                                      interface{}
	lipstickID                                  interface{}
	makeupID                                    interface{}
	headRotation1, headRotation2, headRotation3 float64
	eyeRotation1, eyeRotation2, eyeRotation3    float64
	camPos1, camPos2, camPos3                   float64
	morph1e, morph1n, morph1s, morph1w          interface{}
	morph2e, morph2n, morph2s, morph2w          interface{}
	morph3e, morph3n, morph3s, morph3w          interface{}
	morph4e, morph4n, morph4s, morph4w          interface{}
}

// parseAppearance converts the raw dict sent by the client into charAppearance,
// using NULL (nil) for fields that were not present or were set to the sentinel.
func parseAppearance(d map[string]interface{}) charAppearance {
	intF := func(key string) int64 {
		if v, ok := d[key]; ok {
			switch x := v.(type) {
			case int64:
				return x
			case float64:
				return int64(x)
			}
		}
		return 0
	}
	nullIntF := func(key string) interface{} {
		if v, ok := d[key]; ok {
			switch x := v.(type) {
			case int64:
				return x
			case float64:
				return int64(x)
			}
		}
		return nil
	}
	floatF := func(key string) float64 {
		if v, ok := d[key]; ok {
			switch x := v.(type) {
			case float64:
				return x
			case int64:
				return float64(x)
			}
		}
		return 0
	}
	nullFloatF := func(key string) interface{} {
		if v, ok := d[key]; ok {
			switch x := v.(type) {
			case float64:
				return x
			case int64:
				return float64(x)
			}
		}
		return nil
	}

	return charAppearance{
		costumeID:     intF("costumeID"),
		eyebrowsID:    intF("eyebrowsID"),
		eyesID:        intF("eyesID"),
		hairID:        intF("hairID"),
		skinID:        intF("skinID"),
		backgroundID:  intF("backgroundID"),
		lightID:       intF("lightID"),
		accessoryID:   nullIntF("accessoryID"),
		beardID:       nullIntF("beardID"),
		decoID:        nullIntF("decoID"),
		lipstickID:    nullIntF("lipstickID"),
		makeupID:      nullIntF("makeupID"),
		headRotation1: floatF("headRotation1"),
		headRotation2: floatF("headRotation2"),
		headRotation3: floatF("headRotation3"),
		eyeRotation1:  floatF("eyeRotation1"),
		eyeRotation2:  floatF("eyeRotation2"),
		eyeRotation3:  floatF("eyeRotation3"),
		camPos1:       floatF("camPos1"),
		camPos2:       floatF("camPos2"),
		camPos3:       floatF("camPos3"),
		morph1e:       nullFloatF("morph1e"), morph1n: nullFloatF("morph1n"),
		morph1s: nullFloatF("morph1s"), morph1w: nullFloatF("morph1w"),
		morph2e: nullFloatF("morph2e"), morph2n: nullFloatF("morph2n"),
		morph2s: nullFloatF("morph2s"), morph2w: nullFloatF("morph2w"),
		morph3e: nullFloatF("morph3e"), morph3n: nullFloatF("morph3n"),
		morph3s: nullFloatF("morph3s"), morph3w: nullFloatF("morph3w"),
		morph4e: nullFloatF("morph4e"), morph4n: nullFloatF("morph4n"),
		morph4s: nullFloatF("morph4s"), morph4w: nullFloatF("morph4w"),
	}
}

// GetFirstSkillItemID returns the itemID of the first skill entity owned by
// characterID (flag=7 = flagSkill), used by _SendInitialSkillTraining.
// Returns 0 if the character has no skill items.
func GetFirstSkillItemID(ctx context.Context, db *sql.DB, characterID int64) (int64, error) {
	var itemID int64
	err := db.QueryRowContext(ctx,
		`SELECT itemID FROM entity WHERE ownerID = ? AND flag = 7 LIMIT 1`,
		characterID,
	).Scan(&itemID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("GetFirstSkillItemID: %w", err)
	}
	return itemID, nil
}

// CorpMemberInfo holds a character's corporation role masks. Fields are uint64:
// unsigned bitmasks (0xFFFFFFFFFFFFFFFF for a director/CEO) stored as BIGINT
// UNSIGNED in chrCorporationRoles.
type CorpMemberInfo struct {
	Corprole     uint64
	RolesAtAll   uint64
	RolesAtBase  uint64
	RolesAtHQ    uint64
	RolesAtOther uint64
	BaseID       int64 // member's assigned base station (0 = none)
}

// rowQuerier is satisfied by both *sql.DB and *sql.Tx, so the title-role union
// can be reused by callers that are already inside a transaction.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// AddTitleRoles ORs into info the roles of every title in the character's
// titleMask (crpTitles). Every role gate and the login session read this struct,
// so the union lives here rather than flattened into chrCorporationRoles. Only
// the four held masks are unioned; the grantable masks stay on the member row.
func AddTitleRoles(ctx context.Context, q rowQuerier, characterID int64, info *CorpMemberInfo) error {
	var titleMask uint64
	var corpID int64
	err := q.QueryRowContext(ctx, `
		SELECT COALESCE(r.titleMask, 0), c.corporationID
		FROM character_ c
		LEFT JOIN chrCorporationRoles r ON r.characterID = c.characterID
		WHERE c.characterID = ?`, characterID).Scan(&titleMask, &corpID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("AddTitleRoles lookup: %w", err)
	}
	if titleMask == 0 || corpID == 0 {
		return nil
	}
	var roles, atHQ, atBase, atOther uint64
	err = q.QueryRowContext(ctx, `
		SELECT COALESCE(BIT_OR(roles), 0), COALESCE(BIT_OR(rolesAtHQ), 0),
		       COALESCE(BIT_OR(rolesAtBase), 0), COALESCE(BIT_OR(rolesAtOther), 0)
		FROM crpTitles
		WHERE corporationID = ? AND (titleID & ?) <> 0`, corpID, titleMask,
	).Scan(&roles, &atHQ, &atBase, &atOther)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("AddTitleRoles titles: %w", err)
	}
	info.Corprole |= roles
	info.RolesAtAll |= roles
	info.RolesAtHQ |= atHQ
	info.RolesAtBase |= atBase
	info.RolesAtOther |= atOther
	return nil
}

// GetCorporationMemberInfo queries chrCorporationRoles for the character's role
// fields and folds in the roles carried by the titles they hold.  Returns zero
// values if no row exists (mirrors the C++ fallback).
func GetCorporationMemberInfo(ctx context.Context, db *sql.DB, characterID int64) (*CorpMemberInfo, error) {
	info := &CorpMemberInfo{}
	err := db.QueryRowContext(ctx, `
		SELECT corprole, rolesAtAll, rolesAtBase, rolesAtHQ, rolesAtOther,
		       COALESCE(baseID, 0)
		FROM chrCorporationRoles
		WHERE characterID = ?`, characterID,
	).Scan(&info.Corprole, &info.RolesAtAll, &info.RolesAtBase, &info.RolesAtHQ, &info.RolesAtOther, &info.BaseID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return info, fmt.Errorf("GetCorporationMemberInfo: %w", err)
	}
	// No row → all zeros, matches the C++ fallback; a titled character can
	// still have one, so the union runs either way.
	if err := AddTitleRoles(ctx, db, characterID, info); err != nil {
		return info, err
	}
	if CorpRoleFilter != nil && (CorpRoleFilterApplies == nil || CorpRoleFilterApplies(characterID)) {
		var corpID int64
		if err := db.QueryRowContext(ctx,
			`SELECT corporationID FROM character_ WHERE characterID = ?`, characterID,
		).Scan(&corpID); err == nil && corpID != 0 {
			applyCorpRoleFilter(CorpRoleFilter, corpID, characterID, info)
		}
	}
	return info, nil
}

// CorpRoleFilter is set by a module to strip roles a member holds but may not
// use, from the member row or a title. Called last in GetCorporationMemberInfo,
// the read every server-side role gate goes through. Nil is the identity.
var CorpRoleFilter func(corpID, charID int64, roles uint64) uint64

// CorpRoleFilterApplies, set beside CorpRoleFilter, is called before the
// corporation lookup the filter needs; false skips the filter for that
// character (the rolebots module answers false for its own pilots). Nil = always ask.
var CorpRoleFilterApplies func(charID int64) bool

// FilterCorpRoles runs CorpRoleFilter over one role mask of a member of corpID, the same rule
// GetCorporationMemberInfo applies to the member row.
func FilterCorpRoles(_ context.Context, _ *sql.DB, charID, corpID int64, roles uint64) uint64 {
	if CorpRoleFilter == nil || corpID == 0 || (CorpRoleFilterApplies != nil && !CorpRoleFilterApplies(charID)) {
		return roles
	}
	return CorpRoleFilter(corpID, charID, roles)
}

func applyCorpRoleFilter(f func(corpID, charID int64, roles uint64) uint64, corpID, charID int64, info *CorpMemberInfo) {
	if f == nil || info == nil {
		return
	}
	info.Corprole = f(corpID, charID, info.Corprole)
	info.RolesAtAll = f(corpID, charID, info.RolesAtAll)
	info.RolesAtHQ = f(corpID, charID, info.RolesAtHQ)
	info.RolesAtBase = f(corpID, charID, info.RolesAtBase)
	info.RolesAtOther = f(corpID, charID, info.RolesAtOther)
}

// LoadCharData loads the minimal character fields needed to send the
// character-session SessionChangeNotification after SelectCharacterID.
func LoadCharData(ctx context.Context, db *sql.DB, characterID int64) (*CharData, error) {
	cd := &CharData{CharacterID: characterID}
	var allianceID, hqStationID sql.NullInt64
	err := db.QueryRowContext(ctx, `
		SELECT c.characterName, c.corporationID, c.stationID,
		       c.solarSystemID, c.constellationID, c.regionID,
		       co.allianceID, co.stationID
		FROM character_ c
		LEFT JOIN corporation co ON co.corporationID = c.corporationID
		WHERE c.characterID = ?`, characterID,
	).Scan(
		&cd.CharacterName, &cd.CorporationID,
		&cd.StationID, &cd.SolarSystemID, &cd.ConstellationID, &cd.RegionID,
		&allianceID, &hqStationID,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("LoadCharData: no character with id %d", characterID)
		}
		return nil, fmt.Errorf("LoadCharData: %w", err)
	}
	if allianceID.Valid {
		cd.AllianceID = allianceID.Int64
	}
	if hqStationID.Valid {
		cd.HQStationID = hqStationID.Int64
	}
	return cd, nil
}

// GetCharPublicInfo returns a util.KeyVal PyObject with public character data,
// mirroring CharacterDB::GetCharPublicInfo → DBRowToKeyVal.
// Falls back to eveStaticOwners for NPC characters not in the character_ table.
func GetCharPublicInfo(ctx context.Context, db *sql.DB, charID int64) (marshal.PyRep, error) {
	var (
		typeID              int64
		corporationID       int64
		raceID              int64
		bloodlineID         int64
		characterName       string
		createDateTime      int64
		gender              int64
		characterID         int64
		description         sql.NullString
		corporationDateTime int64
	)
	err := db.QueryRowContext(ctx, `
		SELECT typeID, corporationID, raceID, bloodlineID, characterName,
		       createDateTime, gender, characterID, description, corporationDateTime
		FROM character_
		WHERE characterID = ?`, charID).Scan(
		&typeID, &corporationID, &raceID, &bloodlineID, &characterName,
		&createDateTime, &gender, &characterID, &description, &corporationDateTime,
	)
	if errors.Is(err, sql.ErrNoRows) {
		// NPC fallback: look up in eveStaticOwners for NPC characters.
		return getCharPublicInfoNPC(ctx, db, charID)
	}
	if err != nil {
		return nil, fmt.Errorf("GetCharPublicInfo: %w", err)
	}
	// If character_ has no corp, check agtAgents as fallback (agent may
	// exist in both tables with corp only set in agtAgents).
	if corporationID == 0 {
		var agentCorpID, agentBloodlineID, agentGender, agentRaceID int64
		if scanErr := db.QueryRowContext(ctx,
			`SELECT a.corporationID, a.bloodlineID, a.gender, COALESCE(b.raceID, 0)
			 FROM agtAgents a
			 LEFT JOIN chrBloodlines b ON b.bloodlineID = a.bloodlineID
			 WHERE a.agentID = ?`, charID,
		).Scan(&agentCorpID, &agentBloodlineID, &agentGender, &agentRaceID); scanErr == nil {
			if agentCorpID != 0 {
				log.Printf("GetCharPublicInfo: char %d corpID=0 in character_, using agtAgents corpID=%d", charID, agentCorpID)
				corporationID = agentCorpID
			}
			if bloodlineID == 0 && agentBloodlineID != 0 {
				bloodlineID = agentBloodlineID
			}
			if raceID == 0 && agentRaceID != 0 {
				raceID = agentRaceID
			}
		}
	}
	return makeKeyVal(
		[]string{
			"typeID", "corporationID", "raceID", "bloodlineID", "characterName",
			"age", "createDateTime", "gender", "characterID", "description", "corporationDateTime",
		},
		[]marshal.PyRep{
			intRep(typeID), intRep(corporationID), intRep(raceID), intRep(bloodlineID),
			strRep(characterName), marshal.PyInt{Value: 0}, intRep(createDateTime),
			intRep(gender), intRep(characterID), nullStrRep(description), intRep(corporationDateTime),
		},
	), nil
}

// charTypeBloodlines maps character typeIDs (invTypes group 1) to
// {bloodlineID, raceID}: eveStaticOwners carries only a typeID, and each
// bloodline has its own character type.
var charTypeBloodlines = map[int64][2]int64{
	1373: {5, 4},   // CharacterAmarr
	1374: {6, 4},   // CharacterNiKunni
	1375: {2, 1},   // CharacterCivire
	1376: {1, 1},   // CharacterDeteis
	1377: {7, 8},   // CharacterGallente
	1378: {8, 8},   // CharacterIntaki
	1379: {3, 2},   // CharacterSebiestor
	1380: {4, 2},   // CharacterBrutor
	1381: {9, 16},  // CharacterStatic
	1382: {10, 16}, // CharacterModifier
	1383: {11, 1},  // CharacterAchura
	1384: {12, 8},  // CharacterJinMei
	1385: {13, 4},  // CharacterKhanid
	1386: {14, 2},  // CharacterVherokior
}

// getCharPublicInfoNPC builds a GetCharPublicInfo response for NPC characters
// that do not exist in the character_ table, using eveStaticOwners and
// agtAgents as data sources.
func getCharPublicInfoNPC(ctx context.Context, db *sql.DB, charID int64) (marshal.PyRep, error) {
	var ownerName string
	var typeID int64
	err := db.QueryRowContext(ctx,
		`SELECT ownerName, typeID FROM eveStaticOwners WHERE ownerID = ?`, charID,
	).Scan(&ownerName, &typeID)
	if err != nil {
		// For synthetic entity IDs (700M+), check NPC live/dungeon tables.
		if charID >= 700000000 {
			if rep, npcErr := getCharPublicInfoFromNPCTables(ctx, db, charID); npcErr == nil {
				return rep, nil
			}
		}
		return nil, fmt.Errorf("GetCharPublicInfo: char %d not found in character_ or eveStaticOwners", charID)
	}

	// Try to resolve corporation, bloodline, gender, and race from agtAgents.
	var corporationID, bloodlineID, gender, raceID int64
	_ = db.QueryRowContext(ctx,
		`SELECT a.corporationID, a.bloodlineID, a.gender, COALESCE(b.raceID, 0)
		 FROM agtAgents a
		 LEFT JOIN chrBloodlines b ON b.bloodlineID = a.bloodlineID
		 WHERE a.agentID = ?`, charID,
	).Scan(&corporationID, &bloodlineID, &gender, &raceID)

	// If the entity IS a corporation (typeID=2), its corp logo is itself.
	if typeID == 2 && corporationID == 0 {
		corporationID = charID
	}

	// Static NPC characters are not in agtAgents; their typeID pins the bloodline.
	// Gender is derived as getNPCAppearance does, so mesh and picked items agree.
	if bloodlineID == 0 {
		if bl, ok := charTypeBloodlines[typeID]; ok {
			bloodlineID, raceID = bl[0], bl[1]
			gender = charID % 2
		}
	}

	// bloodlineID 0 makes the client raise IndexError in ccutil.GetBloodline.
	if bloodlineID == 0 {
		bloodlineID = 1 // Deteis (Caldari)
		if raceID == 0 {
			raceID = 1
		}
	}

	return makeKeyVal(
		[]string{
			"typeID", "corporationID", "raceID", "bloodlineID", "characterName",
			"age", "createDateTime", "gender", "characterID", "description", "corporationDateTime",
		},
		[]marshal.PyRep{
			intRep(typeID), intRep(corporationID), intRep(raceID), intRep(bloodlineID),
			strRep(ownerName), marshal.PyInt{Value: 0}, intRep(0),
			intRep(gender), intRep(charID), marshal.PyNone{}, intRep(0),
		},
	), nil
}

// getCharPublicInfoFromNPCTables checks npcLiveEntities then npcDungeonEntities
// for a synthetic entity ID and builds a GetCharPublicInfo response.
func getCharPublicInfoFromNPCTables(ctx context.Context, db *sql.DB, charID int64) (marshal.PyRep, error) {
	entityID := uint32(charID)
	// Try belt NPCs first.
	live, err := GetLiveNPCByEntityID(ctx, db, entityID)
	if err == nil && live != nil {
		return buildCharPublicInfoFromNPC(live.TypeID, live.TypeName, live.CorpID, live.FactionID, charID), nil
	}
	// Try dungeon NPCs.
	dun, err := GetDungeonNPCByEntityID(ctx, db, entityID)
	if err == nil && dun != nil {
		return buildCharPublicInfoFromNPC(dun.TypeID, dun.TypeName, dun.CorpID, dun.FactionID, charID), nil
	}
	return nil, fmt.Errorf("entity %d not in NPC tables", charID)
}

// buildCharPublicInfoFromNPC constructs a GetCharPublicInfo KeyVal from NPC table data.
func buildCharPublicInfoFromNPC(typeID int64, typeName string, corpID, factionID, charID int64) marshal.PyRep {
	raceID := factionToRaceID(factionID)
	return makeKeyVal(
		[]string{
			"typeID", "corporationID", "raceID", "bloodlineID", "characterName",
			"age", "createDateTime", "gender", "characterID", "description", "corporationDateTime",
		},
		[]marshal.PyRep{
			intRep(typeID), intRep(corpID), intRep(raceID), intRep(1),
			strRep(typeName), marshal.PyInt{Value: 0}, intRep(0),
			intRep(0), intRep(charID), marshal.PyNone{}, intRep(0),
		},
	)
}

// factionToRaceID maps common pirate faction IDs to client race IDs.
func factionToRaceID(factionID int64) int64 {
	switch factionID {
	case 500001:
		return 1 // Caldari
	case 500002:
		return 2 // Minmatar
	case 500003:
		return 4 // Amarr
	case 500004:
		return 8 // Gallente
	default:
		return 1
	}
}

// GetCharPublicInfo3 returns a util.Rowset PyObject with extended public character info,
// mirroring CharacterDB::GetCharPublicInfo3 → DBResultToRowset.
func GetCharPublicInfo3(ctx context.Context, db *sql.DB, charID int64) (marshal.PyRep, error) {
	cols := []string{"bounty", "title", "startDateTime", "description", "corporationID"}
	var (
		bounty        sql.NullFloat64
		title         sql.NullString
		startDateTime int64
		description   sql.NullString
		corporationID int64
	)
	err := db.QueryRowContext(ctx, `
		SELECT bounty, title, startDateTime, description, corporationID
		FROM character_
		WHERE characterID = ?`, charID).Scan(
		&bounty, &title, &startDateTime, &description, &corporationID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		// infosvc indexes charinfo[0] for any player-band ID, so a deleted pilot gets
		// one blank row with the corporation from their last killmail.
		if charID < MinPlayerEntityID {
			return makeRowset(cols, nil), nil
		}
		id := KillProfileIdentity{CharID: charID}
		FillIdentityFromKillboard(ctx, db, &id)
		return makeRowset(cols, [][]marshal.PyRep{{
			marshal.PyReal{Value: 0}, marshal.PyNone{}, intRep(0), strRep(""), intRep(id.CorpID),
		}}), nil
	}
	if err != nil {
		return nil, fmt.Errorf("GetCharPublicInfo3: %w", err)
	}
	return makeRowset(cols, [][]marshal.PyRep{{
		nullFloatRep(bounty), nullStrRep(title), intRep(startDateTime),
		nullStrRep(description), intRep(corporationID),
	}}), nil
}

// AddToBounty deducts amount from the caller's balance and adds it to the
// target's bounty. Both operations run in a single transaction so either
// both succeed or neither does.
func AddToBounty(ctx context.Context, db *sql.DB, callerCharID, targetCharID int64, amount float64) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("AddToBounty begin: %w", err)
	}
	defer tx.Rollback()

	// Deduct ISK from caller.
	res, err := tx.ExecContext(ctx,
		`UPDATE character_ SET balance = balance - ? WHERE characterID = ? AND balance >= ?`,
		amount, callerCharID, amount)
	if err != nil {
		return fmt.Errorf("AddToBounty deduct: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("AddToBounty: insufficient balance")
	}

	// Add bounty to target.
	_, err = tx.ExecContext(ctx,
		`UPDATE character_ SET bounty = bounty + ? WHERE characterID = ?`,
		amount, targetCharID)
	if err != nil {
		return fmt.Errorf("AddToBounty credit: %w", err)
	}

	return tx.Commit()
}

// TopBountiesLimit is how deep the Most Wanted list goes. The client caps nothing
// (base_missions.py BrowseBounties), and the billboards draw at random from the
// same answer (billboardMgr.py).
const TopBountiesLimit = 100

// GetTopBounties returns a util.Rowset of the top TopBountiesLimit bounties,
// descending. Columns characterID, ownerName, bounty, online: userentry.py drives
// the online indicator off the last column. A nil isOnline reports everyone offline.
func GetTopBounties(ctx context.Context, db *sql.DB, isOnline func(charID int64) bool) (marshal.PyRep, error) {
	cols := []string{"characterID", "ownerName", "bounty", "online"}
	rows, err := db.QueryContext(ctx, `
		SELECT characterID, characterName, bounty
		FROM character_
		WHERE bounty > 0
		ORDER BY bounty DESC
		LIMIT ?`, TopBountiesLimit)
	if err != nil {
		return nil, fmt.Errorf("GetTopBounties: %w", err)
	}
	defer rows.Close()

	var result [][]marshal.PyRep
	for rows.Next() {
		var (
			charID int64
			name   string
			bounty float64
		)
		if err := rows.Scan(&charID, &name, &bounty); err != nil {
			return nil, fmt.Errorf("GetTopBounties scan: %w", err)
		}
		online := int64(0)
		if isOnline != nil && isOnline(charID) {
			online = 1
		}
		result = append(result, []marshal.PyRep{
			intRep(charID), strRep(name), floatRep(bounty), intRep(online),
		})
	}
	return makeRowset(cols, result), nil
}

// TopBountyCharIDs returns the characterIDs GetTopBounties would list, in the
// same order, without building a rowset.
func TopBountyCharIDs(ctx context.Context, db *sql.DB) ([]int64, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT characterID
		FROM character_
		WHERE bounty > 0
		ORDER BY bounty DESC
		LIMIT ?`, TopBountiesLimit)
	if err != nil {
		return nil, fmt.Errorf("TopBountyCharIDs: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("TopBountyCharIDs scan: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// infoWindowCharCols are the columns the client expects from GetInfoWindowDataForChar.
// The client accesses corpID, allianceID, title, and title1–title8 on the returned
// KeyVal, so all must be present to avoid AttributeError.
var infoWindowCharCols = []string{
	"corpID", "allianceID", "title",
	"title1", "title2", "title3", "title4",
	"title5", "title6", "title7", "title8",
}

// GetInfoWindowDataForChar returns a util.KeyVal with corpID, allianceID, title,
// and title1–title8 for the given characterID, mirroring
// CharacterDB::GetInfoWindowDataForChar → DBRowToKeyVal.
// Falls back to eveStaticOwners/agtAgents for NPC characters not in character_.
func GetInfoWindowDataForChar(ctx context.Context, db *sql.DB, charID int64) (marshal.PyRep, error) {
	var (
		corpID     int64
		allianceID sql.NullInt64
		title      sql.NullString
	)
	err := db.QueryRowContext(ctx, `
		SELECT c.corporationID AS corpID, co.allianceID, c.title
		FROM character_ c
		LEFT JOIN corporation co ON c.corporationID = co.corporationID
		WHERE c.characterID = ?`, charID).Scan(&corpID, &allianceID, &title)
	if errors.Is(err, sql.ErrNoRows) {
		// NPC fallback: try eveStaticOwners + agtAgents for corp association.
		return getInfoWindowDataNPC(ctx, db, charID)
	}
	if err != nil {
		return nil, fmt.Errorf("GetInfoWindowDataForChar: %w", err)
	}
	// If character_ has no corp, check agtAgents as fallback (agent may
	// exist in both tables with corp only set in agtAgents).
	if corpID == 0 {
		var agentCorpID int64
		if scanErr := db.QueryRowContext(ctx,
			`SELECT corporationID FROM agtAgents WHERE agentID = ?`, charID,
		).Scan(&agentCorpID); scanErr == nil && agentCorpID != 0 {
			log.Printf("GetInfoWindowDataForChar: char %d corpID=0 in character_, using agtAgents corpID=%d", charID, agentCorpID)
			corpID = agentCorpID
			_ = db.QueryRowContext(ctx,
				`SELECT allianceID FROM corporation WHERE corporationID = ?`, corpID,
			).Scan(&allianceID)
		}
	}
	vals := []marshal.PyRep{intRep(corpID), nullIntRep(allianceID), nullStrRep(title)}
	vals = append(vals, charTitleNames(ctx, db, charID, corpID, title.String)...)
	return makeKeyVal(infoWindowCharCols, vals), nil
}

// charTitleNames answers title1..title8 of the info window: the names of the
// held corporation titles, slot N being the title whose ID is bit N-1 of
// chrCorporationRoles.titleMask. infosvc joins title and title1..8 with commas,
// so a slot repeating the free-text title or an earlier slot is left out.
func charTitleNames(ctx context.Context, db *sql.DB, charID, corpID int64, text string) []marshal.PyRep {
	out := make([]marshal.PyRep, 8)
	for i := range out {
		out[i] = marshal.PyNone{}
	}
	if corpID == 0 {
		return out
	}
	var mask uint64
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(titleMask, 0) FROM chrCorporationRoles WHERE characterID = ?`, charID,
	).Scan(&mask); err != nil || mask == 0 {
		return out
	}
	rows, err := db.QueryContext(ctx, `
		SELECT titleID, titleName FROM crpTitles
		WHERE corporationID = ? AND (titleID & ?) <> 0`, corpID, mask)
	if err != nil {
		return out
	}
	defer rows.Close()
	names := make([]string, 8)
	for rows.Next() {
		var id uint64
		var name string
		if rows.Scan(&id, &name) != nil || name == "" {
			continue
		}
		for n := 0; n < 8; n++ {
			if id == 1<<uint(n) {
				names[n] = name
			}
		}
	}
	seen := map[string]bool{strings.ToLower(strings.TrimSpace(text)): true}
	for n, name := range names {
		key := strings.ToLower(strings.TrimSpace(name))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out[n] = marshal.PyString{Value: name}
	}
	return out
}

// getInfoWindowDataNPC builds an info-window KeyVal for NPC characters
// that are not in the character_ table.
func getInfoWindowDataNPC(ctx context.Context, db *sql.DB, charID int64) (marshal.PyRep, error) {
	// Try agtAgents first for NPC agent corp association.
	var corpID int64
	err := db.QueryRowContext(ctx,
		`SELECT corporationID FROM agtAgents WHERE agentID = ?`, charID,
	).Scan(&corpID)
	if err != nil {
		// Not an agent: verify the entity exists in eveStaticOwners.
		var ownerName string
		var typeID int64
		if scanErr := db.QueryRowContext(ctx,
			`SELECT ownerName, typeID FROM eveStaticOwners WHERE ownerID = ?`, charID,
		).Scan(&ownerName, &typeID); scanErr != nil {
			// For synthetic entity IDs (700M+), check NPC live/dungeon tables.
			if charID >= 700000000 {
				if rep, npcErr := getInfoWindowDataFromNPCTables(ctx, db, charID); npcErr == nil {
					return rep, nil
				}
			}
			return nil, fmt.Errorf("GetInfoWindowDataForChar: char %d not found", charID)
		}
		// If the entity IS a corporation (typeID=2), its corp logo is itself.
		if typeID == 2 {
			var allianceID sql.NullInt64
			_ = db.QueryRowContext(ctx,
				`SELECT allianceID FROM corporation WHERE corporationID = ?`, charID,
			).Scan(&allianceID)
			return makeKeyVal(infoWindowCharCols, []marshal.PyRep{
				intRep(charID), nullIntRep(allianceID), marshal.PyNone{},
				marshal.PyNone{}, marshal.PyNone{}, marshal.PyNone{}, marshal.PyNone{},
				marshal.PyNone{}, marshal.PyNone{}, marshal.PyNone{}, marshal.PyNone{},
			}), nil
		}
		// Try to find a corp where this NPC is the CEO or creator.
		var fallbackCorpID int64
		_ = db.QueryRowContext(ctx,
			`SELECT corporationID FROM corporation WHERE ceoID = ? OR creatorID = ? LIMIT 1`,
			charID, charID,
		).Scan(&fallbackCorpID)
		if fallbackCorpID != 0 {
			var allianceID sql.NullInt64
			_ = db.QueryRowContext(ctx,
				`SELECT allianceID FROM corporation WHERE corporationID = ?`, fallbackCorpID,
			).Scan(&allianceID)
			return makeKeyVal(infoWindowCharCols, []marshal.PyRep{
				intRep(fallbackCorpID), nullIntRep(allianceID), marshal.PyNone{},
				marshal.PyNone{}, marshal.PyNone{}, marshal.PyNone{}, marshal.PyNone{},
				marshal.PyNone{}, marshal.PyNone{}, marshal.PyNone{}, marshal.PyNone{},
			}), nil
		}
		// Entity exists but has no corp association.
		return makeKeyVal(infoWindowCharCols, []marshal.PyRep{
			intRep(0), marshal.PyNone{}, marshal.PyNone{},
			marshal.PyNone{}, marshal.PyNone{}, marshal.PyNone{}, marshal.PyNone{},
			marshal.PyNone{}, marshal.PyNone{}, marshal.PyNone{}, marshal.PyNone{},
		}), nil
	}

	// Look up alliance from the corp.
	var allianceID sql.NullInt64
	_ = db.QueryRowContext(ctx,
		`SELECT allianceID FROM corporation WHERE corporationID = ?`, corpID,
	).Scan(&allianceID)

	return makeKeyVal(infoWindowCharCols, []marshal.PyRep{
		intRep(corpID), nullIntRep(allianceID), marshal.PyNone{},
		marshal.PyNone{}, marshal.PyNone{}, marshal.PyNone{}, marshal.PyNone{},
		marshal.PyNone{}, marshal.PyNone{}, marshal.PyNone{}, marshal.PyNone{},
	}), nil
}

// getInfoWindowDataFromNPCTables checks npcLiveEntities then npcDungeonEntities
// for a synthetic entity ID and builds an info-window response.
func getInfoWindowDataFromNPCTables(ctx context.Context, db *sql.DB, charID int64) (marshal.PyRep, error) {
	entityID := uint32(charID)
	var corpID int64
	// Try belt NPCs first.
	live, err := GetLiveNPCByEntityID(ctx, db, entityID)
	if err == nil && live != nil {
		corpID = live.CorpID
	} else {
		// Try dungeon NPCs.
		dun, err := GetDungeonNPCByEntityID(ctx, db, entityID)
		if err != nil || dun == nil {
			return nil, fmt.Errorf("entity %d not in NPC tables", charID)
		}
		corpID = dun.CorpID
	}
	var allianceID sql.NullInt64
	_ = db.QueryRowContext(ctx,
		`SELECT allianceID FROM corporation WHERE corporationID = ?`, corpID,
	).Scan(&allianceID)
	return makeKeyVal(infoWindowCharCols, []marshal.PyRep{
		intRep(corpID), nullIntRep(allianceID), marshal.PyNone{},
		marshal.PyNone{}, marshal.PyNone{}, marshal.PyNone{}, marshal.PyNone{},
		marshal.PyNone{}, marshal.PyNone{}, marshal.PyNone{}, marshal.PyNone{},
	}), nil
}

// appearanceCols are the appearance field names from character_, matching the
// column order queried by CharacterDB::GetCharacterAppearance in the C++ server.
var appearanceCols = []string{
	"accessoryID", "beardID", "costumeID", "decoID", "eyebrowsID", "eyesID", "hairID",
	"lipstickID", "makeupID", "skinID", "backgroundID", "lightID",
	"headRotation1", "headRotation2", "headRotation3",
	"eyeRotation1", "eyeRotation2", "eyeRotation3",
	"camPos1", "camPos2", "camPos3",
	"morph1e", "morph1n", "morph1s", "morph1w",
	"morph2e", "morph2n", "morph2s", "morph2w",
	"morph3e", "morph3n", "morph3s", "morph3w",
	"morph4e", "morph4n", "morph4s", "morph4w",
}

// defaultAppearanceRow returns a single appearance row with all None values.
// This is used as a fallback for characters with no appearance data so that the
// client's PrimeCharAppearance doesn't crash indexing an empty rowset.
func defaultAppearanceRow() []marshal.PyRep {
	row := make([]marshal.PyRep, len(appearanceCols))
	for i := range row {
		row[i] = marshal.PyNone{}
	}
	return row
}

// GetCharDescription returns the description field for characterID.
func GetCharDescription(ctx context.Context, db *sql.DB, characterID int64) (string, error) {
	var description string
	err := db.QueryRowContext(ctx,
		`SELECT description FROM character_ WHERE characterID = ?`, characterID,
	).Scan(&description)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("GetCharDescription: %w", err)
	}
	return description, nil
}

// SetCharDescription updates the description for characterID.
func SetCharDescription(ctx context.Context, db *sql.DB, characterID int64, description string) error {
	_, err := db.ExecContext(ctx,
		`UPDATE character_ SET description = ? WHERE characterID = ?`, description, characterID)
	if err != nil {
		return fmt.Errorf("SetCharDescription: %w", err)
	}
	return nil
}

// GetCharNote returns the note text for (ownerID, itemID) from chrNotes.
// found=false means no note exists for that pair.
func GetCharNote(ctx context.Context, db *sql.DB, ownerID, itemID int64) (note string, found bool, err error) {
	scanErr := db.QueryRowContext(ctx,
		`SELECT note FROM chrNotes WHERE ownerID = ? AND itemID = ?`, ownerID, itemID,
	).Scan(&note)
	if scanErr != nil {
		if errors.Is(scanErr, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("GetCharNote: %w", scanErr)
	}
	return note, true, nil
}

// SetCharNote upserts or deletes a note in chrNotes, mirroring
// CharacterDB::SetNote.  An empty note deletes the row; non-empty upserts.
func SetCharNote(ctx context.Context, db *sql.DB, ownerID, itemID int64, note string) error {
	if note == "" {
		_, err := db.ExecContext(ctx,
			`DELETE FROM chrNotes WHERE ownerID = ? AND itemID = ? LIMIT 1`, ownerID, itemID)
		return err
	}
	_, err := db.ExecContext(ctx,
		`REPLACE INTO chrNotes (ownerID, itemID, note) VALUES (?, ?, ?)`, ownerID, itemID, note)
	return err
}

// GetOwnerNoteLabels returns a util.Rowset(noteID, label) for charID from
// chrOwnerNote, mirroring CharacterDB::GetOwnerNoteLabels.
func GetOwnerNoteLabels(ctx context.Context, db *sql.DB, charID int64) (marshal.PyRep, error) {
	cols := []string{"noteID", "label"}
	rows, err := db.QueryContext(ctx,
		`SELECT noteID, label FROM chrOwnerNote WHERE ownerID = ?`, charID)
	if err != nil {
		return makeRowset(cols, nil), fmt.Errorf("GetOwnerNoteLabels: %w", err)
	}
	defer rows.Close()
	var result [][]marshal.PyRep
	for rows.Next() {
		var noteID int64
		var label string
		if scanErr := rows.Scan(&noteID, &label); scanErr != nil {
			return nil, fmt.Errorf("GetOwnerNoteLabels scan: %w", scanErr)
		}
		result = append(result, []marshal.PyRep{intRep(noteID), strRep(label)})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("GetOwnerNoteLabels rows: %w", err)
	}
	return makeRowset(cols, result), nil
}

// GetOwnerNote returns a util.Rowset(note) for one noteID owned by charID,
// mirroring CharacterDB::GetOwnerNote.
func GetOwnerNote(ctx context.Context, db *sql.DB, charID, noteID int64) (marshal.PyRep, error) {
	cols := []string{"note"}
	rows, err := db.QueryContext(ctx,
		`SELECT note FROM chrOwnerNote WHERE ownerID = ? AND noteID = ?`, charID, noteID)
	if err != nil {
		return makeRowset(cols, nil), fmt.Errorf("GetOwnerNote: %w", err)
	}
	defer rows.Close()
	var result [][]marshal.PyRep
	for rows.Next() {
		var note string
		if scanErr := rows.Scan(&note); scanErr != nil {
			return nil, fmt.Errorf("GetOwnerNote scan: %w", scanErr)
		}
		result = append(result, []marshal.PyRep{strRep(note)})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("GetOwnerNote rows: %w", err)
	}
	return makeRowset(cols, result), nil
}

// AddOwnerNote inserts a new note into chrOwnerNote, mirroring
// CharacterDB::AddOwnerNote.  Returns the new noteID.
func AddOwnerNote(ctx context.Context, db *sql.DB, charID int64, label, content string) (int64, error) {
	res, err := db.ExecContext(ctx,
		`INSERT INTO chrOwnerNote (ownerID, label, note) VALUES (?, ?, ?)`, charID, label, content)
	if err != nil {
		return 0, fmt.Errorf("AddOwnerNote: %w", err)
	}
	return res.LastInsertId()
}

// EditOwnerNote updates a note's label and content in chrOwnerNote.  The
// client's notepad sends the label on every edit (GetOwnerNote reads it back
// with a type-prefix slice), so unlike the C++ server we persist it.
func EditOwnerNote(ctx context.Context, db *sql.DB, charID, noteID int64, label, content string) error {
	_, err := db.ExecContext(ctx,
		`UPDATE chrOwnerNote SET label = ?, note = ? WHERE ownerID = ? AND noteID = ?`,
		label, content, charID, noteID)
	if err != nil {
		return fmt.Errorf("EditOwnerNote: %w", err)
	}
	return nil
}

// EditOwnerNoteLabel renames a note without touching its body: the notepad's
// 2-argument EditOwnerNote(noteID, label) rename form.
func EditOwnerNoteLabel(ctx context.Context, db *sql.DB, charID, noteID int64, label string) error {
	_, err := db.ExecContext(ctx,
		`UPDATE chrOwnerNote SET label = ? WHERE ownerID = ? AND noteID = ?`,
		label, charID, noteID)
	if err != nil {
		return fmt.Errorf("EditOwnerNoteLabel: %w", err)
	}
	return nil
}

// RemoveOwnerNote deletes a note from chrOwnerNote, mirroring
// CharacterDB::RemoveOwnerNote.
func RemoveOwnerNote(ctx context.Context, db *sql.DB, charID, noteID int64) error {
	_, err := db.ExecContext(ctx,
		`DELETE FROM chrOwnerNote WHERE ownerID = ? AND noteID = ?`, charID, noteID)
	if err != nil {
		return fmt.Errorf("RemoveOwnerNote: %w", err)
	}
	return nil
}

// GetAccountCharacterInfo returns a util.Rowset(characterID, characterName) of
// every character on an account: character.GetCharacterInfo, called by the
// out-of-character petition form on the character-select screen.
func GetAccountCharacterInfo(ctx context.Context, db *sql.DB, accountID int64) (marshal.PyRep, error) {
	cols := []string{"characterID", "characterName"}
	rows, err := db.QueryContext(ctx,
		`SELECT characterID, characterName FROM character_ WHERE accountID = ?`, accountID)
	if err != nil {
		return makeRowset(cols, nil), fmt.Errorf("GetAccountCharacterInfo: %w", err)
	}
	defer rows.Close()
	var result [][]marshal.PyRep
	for rows.Next() {
		var id int64
		var name string
		if scanErr := rows.Scan(&id, &name); scanErr != nil {
			return nil, fmt.Errorf("GetAccountCharacterInfo scan: %w", scanErr)
		}
		result = append(result, []marshal.PyRep{intRep(id), strRep(name)})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("GetAccountCharacterInfo rows: %w", err)
	}
	return makeRowset(cols, result), nil
}

// GetCharAppearanceList returns a PyList of util.Rowset values (or PyNone for
// missing chars), one entry per requested charID, mirroring
// CharacterService::Handle_GetCharacterAppearanceList + GetCharacterAppearance.
func GetCharAppearanceList(ctx context.Context, db *sql.DB, charIDs []int64) (marshal.PyRep, error) {
	items := make([]marshal.PyRep, len(charIDs))
	for i, id := range charIDs {
		rowset, err := getCharAppearance(ctx, db, id)
		if err != nil {
			log.Printf("[APPEARANCE] charID=%d error: %v — returning default row", id, err)
		}
		if err != nil || rowset == nil {
			// Return a single row of all-None values so the client's
			// PrimeCharAppearance can index row 0 without an IndexError.
			items[i] = makeRowset(appearanceCols, [][]marshal.PyRep{defaultAppearanceRow()})
		} else {
			items[i] = rowset
		}
	}
	return marshal.PyList{Items: items}, nil
}

func getCharAppearance(ctx context.Context, db *sql.DB, charID int64) (marshal.PyRep, error) {
	// agtAppearance overrides come first: NPC agents may also have rows in character_.
	if rep, err := getNPCAppearanceOverride(ctx, db, charID); err != nil {
		return nil, err
	} else if rep != nil {
		return rep, nil
	}

	var (
		accessoryID, beardID, decoID                sql.NullInt64
		lipstickID, makeupID                        sql.NullInt64
		costumeID, eyebrowsID, eyesID               sql.NullInt64
		hairID, skinID                              sql.NullInt64
		backgroundID, lightID                       sql.NullInt64
		headRotation1, headRotation2, headRotation3 sql.NullFloat64
		eyeRotation1, eyeRotation2, eyeRotation3    sql.NullFloat64
		camPos1, camPos2, camPos3                   sql.NullFloat64
		morph1e, morph1n, morph1s, morph1w          sql.NullFloat64
		morph2e, morph2n, morph2s, morph2w          sql.NullFloat64
		morph3e, morph3n, morph3s, morph3w          sql.NullFloat64
		morph4e, morph4n, morph4s, morph4w          sql.NullFloat64
	)
	err := db.QueryRowContext(ctx, `
		SELECT accessoryID, beardID, costumeID, decoID, eyebrowsID, eyesID, hairID,
		       lipstickID, makeupID, skinID, backgroundID, lightID,
		       headRotation1, headRotation2, headRotation3,
		       eyeRotation1, eyeRotation2, eyeRotation3,
		       camPos1, camPos2, camPos3,
		       morph1e, morph1n, morph1s, morph1w,
		       morph2e, morph2n, morph2s, morph2w,
		       morph3e, morph3n, morph3s, morph3w,
		       morph4e, morph4n, morph4s, morph4w
		FROM character_ WHERE characterID = ?`, charID,
	).Scan(
		&accessoryID, &beardID, &costumeID, &decoID, &eyebrowsID, &eyesID, &hairID,
		&lipstickID, &makeupID, &skinID, &backgroundID, &lightID,
		&headRotation1, &headRotation2, &headRotation3,
		&eyeRotation1, &eyeRotation2, &eyeRotation3,
		&camPos1, &camPos2, &camPos3,
		&morph1e, &morph1n, &morph1s, &morph1w,
		&morph2e, &morph2n, &morph2s, &morph2w,
		&morph3e, &morph3n, &morph3s, &morph3w,
		&morph4e, &morph4n, &morph4s, &morph4w,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return getNPCAppearance(ctx, db, charID)
		}
		return nil, fmt.Errorf("getCharAppearance charID=%d: %w", charID, err)
	}
	row := []marshal.PyRep{
		nullIntRep(accessoryID), nullIntRep(beardID), nullIntRep(costumeID), nullIntRep(decoID),
		nullIntRep(eyebrowsID), nullIntRep(eyesID), nullIntRep(hairID),
		nullIntRep(lipstickID), nullIntRep(makeupID), nullIntRep(skinID), nullIntRep(backgroundID), nullIntRep(lightID),
		nullFloatRep(headRotation1), nullFloatRep(headRotation2), nullFloatRep(headRotation3),
		nullFloatRep(eyeRotation1), nullFloatRep(eyeRotation2), nullFloatRep(eyeRotation3),
		nullFloatRep(camPos1), nullFloatRep(camPos2), nullFloatRep(camPos3),
		nullFloatRep(morph1e), nullFloatRep(morph1n), nullFloatRep(morph1s), nullFloatRep(morph1w),
		nullFloatRep(morph2e), nullFloatRep(morph2n), nullFloatRep(morph2s), nullFloatRep(morph2w),
		nullFloatRep(morph3e), nullFloatRep(morph3n), nullFloatRep(morph3s), nullFloatRep(morph3w),
		nullFloatRep(morph4e), nullFloatRep(morph4n), nullFloatRep(morph4s), nullFloatRep(morph4w),
	}
	return makeRowset(appearanceCols, [][]marshal.PyRep{row}), nil
}

// getNPCAppearanceOverride checks the agtAppearance table for a portrait
// override set via the ESP admin. Returns (nil, nil) if no override exists.
// This is called early in getCharAppearance so that ESP overrides take
// priority even for agents that also have rows in the character_ table.
func getNPCAppearanceOverride(ctx context.Context, db *sql.DB, charID int64) (marshal.PyRep, error) {
	var oa struct {
		accessoryID, beardID, costumeID, decoID sql.NullInt64
		eyebrowsID, eyesID, hairID              sql.NullInt64
		lipstickID, makeupID, skinID            sql.NullInt64
		backgroundID, lightID                   sql.NullInt64
		hr1, hr2, hr3, er1, er2, er3            float64
		m1e, m1n, m1s, m1w                      float64
		m2e, m2n, m2s, m2w                      float64
		m3e, m3n, m3s, m3w                      float64
		m4e, m4n, m4s, m4w                      float64
	}
	err := db.QueryRowContext(ctx, `
		SELECT NULLIF(accessoryID,0), NULLIF(beardID,0), NULLIF(costumeID,0),
		       NULLIF(decoID,0), NULLIF(eyebrowsID,0), NULLIF(eyesID,0), NULLIF(hairID,0),
		       NULLIF(lipstickID,0), NULLIF(makeupID,0), NULLIF(skinID,0),
		       NULLIF(backgroundID,0), NULLIF(lightID,0),
		       headRotation1, headRotation2, headRotation3,
		       eyeRotation1, eyeRotation2, eyeRotation3,
		       morph1e, morph1n, morph1s, morph1w,
		       morph2e, morph2n, morph2s, morph2w,
		       morph3e, morph3n, morph3s, morph3w,
		       morph4e, morph4n, morph4s, morph4w
		FROM agtAppearance WHERE agentID = ?`, charID,
	).Scan(&oa.accessoryID, &oa.beardID, &oa.costumeID, &oa.decoID,
		&oa.eyebrowsID, &oa.eyesID, &oa.hairID,
		&oa.lipstickID, &oa.makeupID, &oa.skinID,
		&oa.backgroundID, &oa.lightID,
		&oa.hr1, &oa.hr2, &oa.hr3, &oa.er1, &oa.er2, &oa.er3,
		&oa.m1e, &oa.m1n, &oa.m1s, &oa.m1w,
		&oa.m2e, &oa.m2n, &oa.m2s, &oa.m2w,
		&oa.m3e, &oa.m3n, &oa.m3s, &oa.m3w,
		&oa.m4e, &oa.m4n, &oa.m4s, &oa.m4w)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("[NPC-APPEARANCE] charID=%d agtAppearance query error: %v", charID, err)
		}
		return nil, nil // no override row or query error; not fatal
	}
	log.Printf("[NPC-APPEARANCE] charID=%d using agtAppearance override", charID)
	row := []marshal.PyRep{
		nullIntRep(oa.accessoryID), nullIntRep(oa.beardID),
		nullIntRep(oa.costumeID), nullIntRep(oa.decoID),
		nullIntRep(oa.eyebrowsID), nullIntRep(oa.eyesID), nullIntRep(oa.hairID),
		nullIntRep(oa.lipstickID), nullIntRep(oa.makeupID), nullIntRep(oa.skinID),
		nullIntRep(oa.backgroundID), nullIntRep(oa.lightID),
		floatRep(oa.hr1), floatRep(oa.hr2), floatRep(oa.hr3),
		floatRep(oa.er1), floatRep(oa.er2), floatRep(oa.er3),
		floatRep(0), floatRep(0), floatRep(0), // camPos
		morphRep(oa.m1e), morphRep(oa.m1n), morphRep(oa.m1s), morphRep(oa.m1w),
		morphRep(oa.m2e), morphRep(oa.m2n), morphRep(oa.m2s), morphRep(oa.m2w),
		morphRep(oa.m3e), morphRep(oa.m3n), morphRep(oa.m3s), morphRep(oa.m3w),
		morphRep(oa.m4e), morphRep(oa.m4n), morphRep(oa.m4s), morphRep(oa.m4w),
	}
	return makeRowset(appearanceCols, [][]marshal.PyRep{row}), nil
}

// getNPCAppearance generates a deterministic appearance for an NPC agent by
// looking up valid appearance items from the chrBL* tables using the agent's
// bloodline and gender from agtAgents. Returns nil if the ID is not an agent.
func getNPCAppearance(ctx context.Context, db *sql.DB, charID int64) (marshal.PyRep, error) {
	var bloodlineID, gender int64
	err := db.QueryRowContext(ctx,
		`SELECT bloodlineID, gender FROM agtAgents WHERE agentID = ?`, charID,
	).Scan(&bloodlineID, &gender)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Not in agtAgents: a static NPC character's eveStaticOwners typeID pins the
			// bloodline; gender derived as in getCharPublicInfoNPC so mesh and costume agree.
			var typeID int64
			if serr := db.QueryRowContext(ctx,
				`SELECT typeID FROM eveStaticOwners WHERE ownerID = ?`, charID,
			).Scan(&typeID); serr == nil {
				if bl, ok := charTypeBloodlines[typeID]; ok {
					bloodlineID = bl[0]
				}
			}
			gender = charID % 2
			if bloodlineID == 0 {
				// Last resort so the client still gets a valid portrait
				// instead of crashing with bloodlineID=0 in ccutil.GetBloodline.
				bloodlineID = 1 // Deteis (Caldari)
			}
			log.Printf("[NPC-APPEARANCE] charID=%d not in agtAgents, using static bloodline=%d gender=%d", charID, bloodlineID, gender)
		} else {
			return nil, fmt.Errorf("getNPCAppearance agentID=%d: %w", charID, err)
		}
	}
	log.Printf("[NPC-APPEARANCE] charID=%d bloodline=%d gender=%d", charID, bloodlineID, gender)

	// Check for appearance overrides from agtAppearance (set via ESP admin).
	var oa struct {
		accessoryID, beardID, costumeID, decoID sql.NullInt64
		eyebrowsID, eyesID, hairID              sql.NullInt64
		lipstickID, makeupID, skinID            sql.NullInt64
		backgroundID, lightID                   sql.NullInt64
		hr1, hr2, hr3, er1, er2, er3            float64
		m1e, m1n, m1s, m1w                      float64
		m2e, m2n, m2s, m2w                      float64
		m3e, m3n, m3s, m3w                      float64
		m4e, m4n, m4s, m4w                      float64
	}
	oaErr := db.QueryRowContext(ctx, `
		SELECT NULLIF(accessoryID,0), NULLIF(beardID,0), NULLIF(costumeID,0),
		       NULLIF(decoID,0), NULLIF(eyebrowsID,0), NULLIF(eyesID,0), NULLIF(hairID,0),
		       NULLIF(lipstickID,0), NULLIF(makeupID,0), NULLIF(skinID,0),
		       NULLIF(backgroundID,0), NULLIF(lightID,0),
		       headRotation1, headRotation2, headRotation3,
		       eyeRotation1, eyeRotation2, eyeRotation3,
		       morph1e, morph1n, morph1s, morph1w,
		       morph2e, morph2n, morph2s, morph2w,
		       morph3e, morph3n, morph3s, morph3w,
		       morph4e, morph4n, morph4s, morph4w
		FROM agtAppearance WHERE agentID = ?`, charID,
	).Scan(&oa.accessoryID, &oa.beardID, &oa.costumeID, &oa.decoID,
		&oa.eyebrowsID, &oa.eyesID, &oa.hairID,
		&oa.lipstickID, &oa.makeupID, &oa.skinID,
		&oa.backgroundID, &oa.lightID,
		&oa.hr1, &oa.hr2, &oa.hr3, &oa.er1, &oa.er2, &oa.er3,
		&oa.m1e, &oa.m1n, &oa.m1s, &oa.m1w,
		&oa.m2e, &oa.m2n, &oa.m2s, &oa.m2w,
		&oa.m3e, &oa.m3n, &oa.m3s, &oa.m3w,
		&oa.m4e, &oa.m4n, &oa.m4s, &oa.m4w)
	if oaErr == nil {
		log.Printf("[NPC-APPEARANCE] charID=%d using agtAppearance override", charID)
		row := []marshal.PyRep{
			nullIntRep(oa.accessoryID), nullIntRep(oa.beardID),
			nullIntRep(oa.costumeID), nullIntRep(oa.decoID),
			nullIntRep(oa.eyebrowsID), nullIntRep(oa.eyesID), nullIntRep(oa.hairID),
			nullIntRep(oa.lipstickID), nullIntRep(oa.makeupID), nullIntRep(oa.skinID),
			nullIntRep(oa.backgroundID), nullIntRep(oa.lightID),
			floatRep(oa.hr1), floatRep(oa.hr2), floatRep(oa.hr3),
			floatRep(oa.er1), floatRep(oa.er2), floatRep(oa.er3),
			floatRep(0), floatRep(0), floatRep(0), // camPos
			morphRep(oa.m1e), morphRep(oa.m1n), morphRep(oa.m1s), morphRep(oa.m1w),
			morphRep(oa.m2e), morphRep(oa.m2n), morphRep(oa.m2s), morphRep(oa.m2w),
			morphRep(oa.m3e), morphRep(oa.m3n), morphRep(oa.m3s), morphRep(oa.m3w),
			morphRep(oa.m4e), morphRep(oa.m4n), morphRep(oa.m4s), morphRep(oa.m4w),
		}
		return makeRowset(appearanceCols, [][]marshal.PyRep{row}), nil
	}

	// pickOne queries a chrBL* table and deterministically picks one item.
	pickOne := func(label, query string, args ...interface{}) (sql.NullInt64, error) {
		rows, err := db.QueryContext(ctx, query, args...)
		if err != nil {
			return sql.NullInt64{}, err
		}
		defer rows.Close()
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return sql.NullInt64{}, err
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			return sql.NullInt64{}, err
		}
		if len(ids) == 0 {
			log.Printf("[NPC-APPEARANCE]   %s: no options found", label)
			return sql.NullInt64{}, nil
		}
		picked := ids[int(charID%int64(len(ids)))]
		log.Printf("[NPC-APPEARANCE]   %s: %d options, picked %d", label, len(ids), picked)
		return sql.NullInt64{Valid: true, Int64: picked}, nil
	}

	// Bloodline+gender specific appearance items.
	type blQuery struct {
		col   string
		table string
	}
	blQueries := []blQuery{
		{"accessoryID", "chrBLAccessories"},
		{"beardID", "chrBLBeards"},
		{"costumeID", "chrBLCostumes"},
		{"decoID", "chrBLDecos"},
		{"eyebrowsID", "chrBLEyebrows"},
		{"eyesID", "chrBLEyes"},
		{"hairID", "chrBLHairs"},
		{"lipstickID", "chrBLLipsticks"},
		{"makeupID", "chrBLMakeups"},
		{"skinID", "chrBLSkins"},
	}

	vals := make([]sql.NullInt64, len(blQueries))
	for i, q := range blQueries {
		vals[i], err = pickOne(
			q.col,
			fmt.Sprintf("SELECT %s FROM %s WHERE bloodlineID = ? AND gender = ? ORDER BY %s",
				q.col, q.table, q.col),
			bloodlineID, gender,
		)
		if err != nil {
			return nil, fmt.Errorf("getNPCAppearance %s: %w", q.table, err)
		}
	}

	// Background and light are not bloodline-specific.
	bgID, err := pickOne("backgroundID", "SELECT backgroundID FROM chrBLBackgrounds ORDER BY backgroundID")
	if err != nil {
		return nil, fmt.Errorf("getNPCAppearance backgrounds: %w", err)
	}
	lightID, err := pickOne("lightID", "SELECT lightID FROM chrBLLights ORDER BY lightID")
	if err != nil {
		return nil, fmt.Errorf("getNPCAppearance lights: %w", err)
	}

	// Build the row in the same column order as appearanceCols.
	row := []marshal.PyRep{
		nullIntRep(vals[0]), // accessoryID
		nullIntRep(vals[1]), // beardID
		nullIntRep(vals[2]), // costumeID
		nullIntRep(vals[3]), // decoID
		nullIntRep(vals[4]), // eyebrowsID
		nullIntRep(vals[5]), // eyesID
		nullIntRep(vals[6]), // hairID
		nullIntRep(vals[7]), // lipstickID
		nullIntRep(vals[8]), // makeupID
		nullIntRep(vals[9]), // skinID
		nullIntRep(bgID),    // backgroundID
		nullIntRep(lightID), // lightID
		floatRep(0),         // headRotation1
		floatRep(0),         // headRotation2
		floatRep(0),         // headRotation3
		floatRep(0),         // eyeRotation1
		floatRep(0),         // eyeRotation2
		floatRep(0),         // eyeRotation3
		floatRep(0),         // camPos1
		floatRep(0),         // camPos2
		floatRep(0),         // camPos3
		marshal.PyNone{},    // morph1e
		marshal.PyNone{},    // morph1n
		marshal.PyNone{},    // morph1s
		marshal.PyNone{},    // morph1w
		marshal.PyNone{},    // morph2e
		marshal.PyNone{},    // morph2n
		marshal.PyNone{},    // morph2s
		marshal.PyNone{},    // morph2w
		marshal.PyNone{},    // morph3e
		marshal.PyNone{},    // morph3n
		marshal.PyNone{},    // morph3s
		marshal.PyNone{},    // morph3w
		marshal.PyNone{},    // morph4e
		marshal.PyNone{},    // morph4n
		marshal.PyNone{},    // morph4s
		marshal.PyNone{},    // morph4w
	}
	return makeRowset(appearanceCols, [][]marshal.PyRep{row}), nil
}

// GetCharacterBalance returns the wallet ISK balance for characterID from
// the character_ table.  Returns 0 if the character doesn't exist.
func GetCharacterBalance(ctx context.Context, db *sql.DB, characterID int64) (float64, error) {
	var balance float64
	err := db.QueryRowContext(ctx,
		`SELECT balance FROM character_ WHERE characterID = ?`, characterID,
	).Scan(&balance)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("GetCharacterBalance: %w", err)
	}
	return balance, nil
}

// moveSecurityRating runs one security-rating UPDATE under a row lock and
// returns the rating before and after. expr is the new value in terms of the
// column; it is clamped to [seclaw.SecMin, seclaw.SecMax] here, and its own
// placeholders take args. The caller derives the applied delta as new-old.
func moveSecurityRating(ctx context.Context, db *sql.DB, op string, charID int64,
	expr string, args ...interface{}) (oldRating, newRating float64, err error) {

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("%s begin charID=%d: %w", op, charID, err)
	}
	defer tx.Rollback()

	if err := tx.QueryRowContext(ctx,
		`SELECT securityRating FROM character_ WHERE characterID = ? FOR UPDATE`, charID,
	).Scan(&oldRating); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, 0, fmt.Errorf("%s charID=%d: no such character", op, charID)
		}
		return 0, 0, fmt.Errorf("%s lock charID=%d: %w", op, charID, err)
	}
	stmt := `UPDATE character_ SET securityRating = GREATEST(LEAST(` + expr + `, ?), ?) WHERE characterID = ?`
	args = append(args, seclaw.SecMax, seclaw.SecMin, charID)
	if _, err := tx.ExecContext(ctx, stmt, args...); err != nil {
		return 0, 0, fmt.Errorf("%s charID=%d: %w", op, charID, err)
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT securityRating FROM character_ WHERE characterID = ?`, charID,
	).Scan(&newRating); err != nil {
		return 0, 0, fmt.Errorf("%s readback charID=%d: %w", op, charID, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("%s commit charID=%d: %w", op, charID, err)
	}
	return oldRating, newRating, nil
}

// UpdateSecurityRating adjusts a character's security rating by a flat delta,
// clamped to [-10, 10]: absolute writes only (a module's raw hit). Penalties go
// through PenalizeSecurityRatingFraction. See
// docs/character-skills.md#security-rating-writes.
func UpdateSecurityRating(ctx context.Context, db *sql.DB, charID int64, delta float64) (oldRating, newRating float64, err error) {
	return moveSecurityRating(ctx, db, "UpdateSecurityRating", charID, `securityRating + ?`, delta)
}

// AwardSecurityRatingFraction moves a character's rating frac of the way to the
// +10 ceiling (a rat kill: dgmattribute 252, seclaw.RatGain). A negative frac is
// applied flat. See docs/character-skills.md#security-rating-writes.
func AwardSecurityRatingFraction(ctx context.Context, db *sql.DB, charID int64, frac float64) (oldRating, newRating float64, err error) {
	if frac < 0 {
		return moveSecurityRating(ctx, db, "AwardSecurityRatingFraction", charID, `securityRating + ?`, frac)
	}
	return moveSecurityRating(ctx, db, "AwardSecurityRatingFraction", charID,
		`securityRating + (? - securityRating) * ?`, seclaw.SecMax, frac)
}

// PenalizeSecurityRatingFraction moves a character's rating frac of the way down
// to the -10 floor (seclaw.LossFromGap with frac = pct/100). The arithmetic runs
// inside the UPDATE so two charges on one tick cannot read the same value.
func PenalizeSecurityRatingFraction(ctx context.Context, db *sql.DB, charID int64, frac float64) (oldRating, newRating float64, err error) {
	return moveSecurityRating(ctx, db, "PenalizeSecurityRatingFraction", charID,
		`securityRating - (securityRating - ?) * ?`, seclaw.SecMin, frac)
}

// DecayNegativeSecurityRatings moves every negative record amount closer to
// zero, never past it, in one bulk statement, and reports the rows moved. It
// writes no chrStandingTransactions row.
func DecayNegativeSecurityRatings(ctx context.Context, db *sql.DB, amount float64) (int64, error) {
	if amount <= 0 {
		return 0, nil
	}
	res, err := db.ExecContext(ctx,
		`UPDATE character_ SET securityRating = LEAST(securityRating + ?, 0)
		  WHERE securityRating < 0`, amount)
	if err != nil {
		return 0, fmt.Errorf("DecayNegativeSecurityRatings: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return n, nil
}

// CreditBounty adds ISK to a character's wallet balance.
func CreditBounty(ctx context.Context, db *sql.DB, charID int64, amount float64) error {
	_, err := db.ExecContext(ctx,
		`UPDATE character_ SET balance = balance + ? WHERE characterID = ?`,
		amount, charID)
	if err != nil {
		return fmt.Errorf("CreditBounty charID=%d: %w", charID, err)
	}
	return nil
}

// InsertBountyJournal records a bounty prize journal row (refTypeID 17) in the
// wallet journalOwnerID. The client renders "<ownerID2> got bounty prize for
// killing <argID1>", a negative argID1 resolving to invTypes[-argID1].name, so
// pass ownerID2 = receiving character and argID1 = -npcTypeID.
func InsertBountyJournal(ctx context.Context, db *sql.DB, journalOwnerID, ownerID1, ownerID2, argID1 int64, amount, newBalance float64) error {
	_, err := db.ExecContext(ctx,
		`INSERT INTO market_journal
		   (characterID, transDate, refTypeID, ownerID1, ownerID2, argID1,
		    accountID, amount, balance, reason, accountKey)
		 VALUES (?, ?, 17, ?, ?, ?, 0, ?, ?, NULL, 1000)`,
		journalOwnerID, win32TimeNow(), ownerID1, ownerID2, argID1, amount, newBalance,
	)
	if err != nil {
		return fmt.Errorf("InsertBountyJournal ownerID=%d: %w", journalOwnerID, err)
	}
	return nil
}

// InsertJournalEntry records a wallet journal entry with the given ref type.
func InsertJournalEntry(ctx context.Context, db *sql.DB, charID int64, refTypeID int,
	ownerID1, ownerID2 int64, amount, newBalance float64, reason string) error {
	_, err := db.ExecContext(ctx,
		`INSERT INTO market_journal
		   (characterID, transDate, refTypeID, ownerID1, ownerID2, argID1,
		    accountID, amount, balance, reason, accountKey)
		 VALUES (?, ?, ?, ?, ?, NULL, 0, ?, ?, ?, 1000)`,
		charID, win32TimeNow(), refTypeID, ownerID1, ownerID2, amount, newBalance, reason,
	)
	if err != nil {
		return fmt.Errorf("InsertJournalEntry charID=%d refType=%d: %w", charID, refTypeID, err)
	}
	return nil
}

// VerifyCharOwnership returns an error if characterID does not belong to accountID,
// preventing one account from deleting another account's character.
func VerifyCharOwnership(ctx context.Context, db *sql.DB, accountID, characterID int64) error {
	var count int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM character_ WHERE characterID = ? AND accountID = ?`,
		characterID, accountID,
	).Scan(&count)
	if err != nil {
		return fmt.Errorf("VerifyCharOwnership: %w", err)
	}
	if count == 0 {
		return fmt.Errorf("character %d does not belong to account %d", characterID, accountID)
	}
	return nil
}

// DeleteCharacter removes a character and all associated data from the database,
// mirroring CharacterDB::DeleteCharacter in server/character/CharacterDB.cpp.
// Best-effort cleanup queries are tried in order; only the final character_ delete
// is treated as fatal.
func DeleteCharacter(ctx context.Context, db *sql.DB, characterID int64) error {
	// Best-effort removals: keep going when individual queries fail.
	bestEffort := func(query string, args ...interface{}) {
		_, _ = db.ExecContext(ctx, query, args...)
	}

	bestEffort(`DELETE FROM channelchars WHERE charID = ?`, characterID)
	bestEffort(`DELETE FROM channels WHERE channelID = ? OR ownerID = ?`, characterID, characterID)

	// Delete mail details before the mail headers.
	mailRows, err := db.QueryContext(ctx,
		`SELECT messageID FROM evemail WHERE channelID = ? OR senderID = ?`,
		characterID, characterID)
	if err == nil {
		var messageIDs []int64
		for mailRows.Next() {
			var mid int64
			if scanErr := mailRows.Scan(&mid); scanErr == nil {
				messageIDs = append(messageIDs, mid)
			}
		}
		_ = mailRows.Close()
		for _, mid := range messageIDs {
			bestEffort(`DELETE FROM evemailDetails WHERE messageID = ?`, mid)
		}
		if len(messageIDs) > 0 {
			bestEffort(`DELETE FROM evemail WHERE channelID = ? OR senderID = ?`, characterID, characterID)
		}
	}

	// Shares go back to each issuer's treasury rather than vanishing, or the
	// corporation's issued total stops adding up (corpshares.go).
	if _, err := ReturnSharesToIssuers(ctx, db, characterID); err != nil {
		log.Printf("DeleteCharacter %d shares: %v", characterID, err)
	}
	bestEffort(`DELETE FROM bookmarks WHERE ownerID = ?`, characterID)
	bestEffort(`DELETE FROM market_journal WHERE characterID = ?`, characterID)
	bestEffort(`DELETE FROM market_orders WHERE charID = ?`, characterID)
	bestEffort(`DELETE FROM market_transactions WHERE characterID = ?`, characterID)
	bestEffort(`DELETE FROM chrStandings WHERE characterID = ?`, characterID)
	bestEffort(`DELETE FROM chrNPCStandings WHERE characterID = ?`, characterID)
	bestEffort(`DELETE FROM chrEmployment WHERE characterID = ?`, characterID)
	bestEffort(`DELETE FROM chrCorporationRoles WHERE characterID = ?`, characterID)
	// Notes the pilot wrote (ownerID is an FK to character_) and notes others wrote
	// about them (itemID is not an FK; see EnsureNoteItemUnbound).
	bestEffort(`DELETE FROM chrNotes WHERE ownerID = ? OR itemID = ?`, characterID, characterID)
	bestEffort(`DELETE FROM entity WHERE itemID = ? OR ownerID = ?`, characterID, characterID)

	// Final delete: fatal on failure.
	if _, err := db.ExecContext(ctx,
		`DELETE FROM character_ WHERE characterID = ?`, characterID); err != nil {
		return fmt.Errorf("DeleteCharacter: %w", err)
	}
	return nil
}

// SkillItem holds one character skill entity row for use in invbroker List()
// and skillMgr.GetSkillsAndProgress.
type SkillItem struct {
	ItemID      int64
	TypeID      int64
	OwnerID     int64
	LocationID  int64
	Flag        int64 // 7 = flagSkill, 61 = flagSkillInTraining
	GroupID     int64
	CategoryID  int64
	Level       int64
	SkillPoints int64
}

// EntityRow holds entity + invtype/invgroup data for building util.Row objects
// (EntityRowObject in the C++ packets layer).
type EntityRow struct {
	ItemID     int64
	TypeID     int64
	OwnerID    int64
	LocationID int64
	Flag       int64
	GroupID    int64
	CategoryID int64
	Singleton  int64
	Quantity   int64
}

// GetEntityRow fetches one entity row for itemID joined with invtypes/invgroups
// for groupID/categoryID.  Returns nil, nil when the item does not exist.
func GetEntityRow(ctx context.Context, db *sql.DB, itemID int64) (*EntityRow, error) {
	var er EntityRow
	err := db.QueryRowContext(ctx, `
		SELECT e.itemID, e.typeID, e.ownerID, e.locationID, e.flag,
		       t.groupID, g.categoryID, e.singleton, e.quantity
		FROM entity e
		JOIN invTypes  t ON t.typeID  = e.typeID
		JOIN invGroups g ON g.groupID = t.groupID
		WHERE e.itemID = ?`, itemID,
	).Scan(&er.ItemID, &er.TypeID, &er.OwnerID, &er.LocationID, &er.Flag,
		&er.GroupID, &er.CategoryID, &er.Singleton, &er.Quantity)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetEntityRow: %w", err)
	}
	return &er, nil
}

// BuildEntityRow constructs a util.Row PyObject (EntityRowObject) from an
// EntityRow, matching the C++ EntityRowObject::Encode() wire format.
// Header order: [itemID, typeID, ownerID, locationID, flag, contraband,
// singleton, quantity, groupID, categoryID, customInfo]
func BuildEntityRow(er *EntityRow) marshal.PyRep {
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

// GetTypeAttributes returns the dgmTypeAttributes rows for typeID as a map of
// attributeID to value, the static base attributes apart from entity_attributes.
// The map is a fresh copy the caller may modify (computeShipDerivedAttrs layers
// overrides onto it); read-only callers use TypeAttrs (staticdata.go).
func GetTypeAttributes(ctx context.Context, db *sql.DB, typeID int64) (map[int64]float64, error) {
	attrs, err := TypeAttrs(ctx, db, typeID)
	if err != nil {
		return nil, err
	}
	return cloneAttrs(attrs), nil
}

// GetEntityAttributes returns all entity_attributes rows for itemID as a map of
// attributeID → float64 (combines valueFloat / valueInt columns).
func GetEntityAttributes(ctx context.Context, db *sql.DB, itemID int64) (map[int64]float64, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT attributeID, COALESCE(NULLIF(valueFloat, 0), CAST(valueInt AS DOUBLE), valueFloat)
		FROM entity_attributes WHERE itemID = ?`, itemID)
	if err != nil {
		return nil, fmt.Errorf("GetEntityAttributes: %w", err)
	}
	defer rows.Close()
	attrs := make(map[int64]float64)
	for rows.Next() {
		var attrID int64
		var val float64
		if err := rows.Scan(&attrID, &val); err != nil {
			return nil, fmt.Errorf("GetEntityAttributes scan: %w", err)
		}
		attrs[attrID] = val
	}
	return attrs, rows.Err()
}

// GetCharacterSkillItems returns every skill entity owned by characterID (flag 7
// or 61) with group, category, level (attr 280) and skill points (attr 276).
// Used by invbroker List() and skillMgr.GetSkillsAndProgress.
func GetCharacterSkillItems(ctx context.Context, db *sql.DB, characterID int64) ([]SkillItem, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT e.itemID, e.typeID, e.ownerID, e.locationID, e.flag,
		       t.groupID, g.categoryID,
		       COALESCE(ea_lv.valueInt, 1)   AS level,
		       COALESCE(ea_sp.valueInt, 250)  AS skillPoints
		FROM entity e
		JOIN invTypes  t  ON t.typeID  = e.typeID
		JOIN invGroups g  ON g.groupID = t.groupID
		LEFT JOIN entity_attributes ea_lv ON ea_lv.itemID = e.itemID AND ea_lv.attributeID = 280
		LEFT JOIN entity_attributes ea_sp ON ea_sp.itemID = e.itemID AND ea_sp.attributeID = 276
		WHERE e.ownerID = ? AND e.flag IN (7, 61)`, characterID,
	)
	if err != nil {
		return nil, fmt.Errorf("GetCharacterSkillItems: %w", err)
	}
	defer rows.Close()

	var items []SkillItem
	for rows.Next() {
		var it SkillItem
		if err := rows.Scan(&it.ItemID, &it.TypeID, &it.OwnerID, &it.LocationID, &it.Flag,
			&it.GroupID, &it.CategoryID, &it.Level, &it.SkillPoints); err != nil {
			return nil, fmt.Errorf("GetCharacterSkillItems scan: %w", err)
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

// GetCharacterSkillLevel returns the trained level (0-5) of a specific skill
// for a character. Returns 0 if the skill is not trained.
func GetCharacterSkillLevel(ctx context.Context, db *sql.DB, characterID, skillTypeID int64) (int, error) {
	var level int
	err := db.QueryRowContext(ctx, `
		SELECT COALESCE(ea.valueInt, 1)
		FROM entity e
		LEFT JOIN entity_attributes ea ON ea.itemID = e.itemID AND ea.attributeID = 280
		WHERE e.ownerID = ? AND e.typeID = ? AND e.flag IN (7, 61)
		LIMIT 1`, characterID, skillTypeID).Scan(&level)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("GetCharacterSkillLevel: %w", err)
	}
	return level, nil
}

// SkillReq is one required-skill prerequisite: a skill typeID and the minimum
// trained level needed.
type SkillReq struct {
	SkillID int64
	Level   int
}

// GetTypeName returns the invTypes.typeName for a typeID, or an empty string
// if the type is unknown.
func GetTypeName(ctx context.Context, db *sql.DB, typeID int64) (string, error) {
	var name string
	err := db.QueryRowContext(ctx,
		`SELECT typeName FROM invTypes WHERE typeID = ?`, typeID).Scan(&name)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("GetTypeName typeID=%d: %w", typeID, err)
	}
	return name, nil
}

// GetRequiredSkills returns the required-skill prerequisites for a type from
// dgmTypeAttributes: requiredSkill1/2/3 (attrs 182/183/184) paired with their
// levels (277/278/279).  Types with no requirements return an empty slice.
func GetRequiredSkills(ctx context.Context, db *sql.DB, typeID int64) ([]SkillReq, error) {
	attrs, err := GetTypeAttributes(ctx, db, typeID)
	if err != nil {
		return nil, fmt.Errorf("GetRequiredSkills: %w", err)
	}
	pairs := [3][2]int64{{182, 277}, {183, 278}, {184, 279}}
	var reqs []SkillReq
	for _, p := range pairs {
		skillID := int64(attrs[p[0]])
		if skillID <= 0 {
			continue
		}
		lvl := int(attrs[p[1]])
		if lvl < 1 {
			lvl = 1
		}
		reqs = append(reqs, SkillReq{SkillID: skillID, Level: lvl})
	}
	return reqs, nil
}

// EnsureRookieShip checks whether the character has any non-capsule ships at the
// given station.  If not, it seeds a race-appropriate rookie ship into the hangar
// and returns the new entity itemID.  Returns 0 if the character already has ships.
func EnsureRookieShip(ctx context.Context, db *sql.DB, charID, stationID int64) (int64, error) {
	// Count non-capsule ships at this station (typeID 670 = Capsule).
	var count int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entity WHERE ownerID = ? AND locationID = ? AND typeID != 670 AND singleton = 1`,
		charID, stationID,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("EnsureRookieShip count: %w", err)
	}
	if count > 0 {
		return 0, nil // already has ships
	}

	// Look up the character's raceID and name.
	var raceID int64
	var charName string
	err = db.QueryRowContext(ctx,
		`SELECT raceID, characterName FROM character_ WHERE characterID = ?`, charID,
	).Scan(&raceID, &charName)
	if err != nil {
		return 0, fmt.Errorf("EnsureRookieShip race lookup: %w", err)
	}

	// Map raceID → rookie ship typeID, civilian weapon typeID, and name.
	var shipTypeID int64
	var weaponTypeID int64
	var shipName string
	switch raceID {
	case 1: // Caldari → Ibis
		shipTypeID, weaponTypeID, shipName = 601, 3638, charName+"'s Ibis"
	case 2: // Minmatar → Reaper
		shipTypeID, weaponTypeID, shipName = 588, 3636, charName+"'s Reaper"
	case 4: // Amarr → Impairor
		shipTypeID, weaponTypeID, shipName = 596, 3634, charName+"'s Impairor"
	case 8: // Gallente → Velator
		shipTypeID, weaponTypeID, shipName = 606, 3640, charName+"'s Velator"
	default:
		shipTypeID, weaponTypeID, shipName = 606, 3640, charName+"'s Velator" // fallback
	}

	// Insert the rookie ship entity (same pattern as capsule creation in CreateCharacter).
	res, err := db.ExecContext(ctx, `
		INSERT INTO entity
		  (itemName, typeID, ownerID, locationID, flag, contraband, singleton, quantity, x, y, z, customInfo)
		VALUES (?, ?, ?, ?, 4, 0, 1, 1, 0, 0, 0, NULL)`,
		shipName, shipTypeID, charID, stationID,
	)
	if err != nil {
		return 0, fmt.Errorf("EnsureRookieShip insert: %w", err)
	}
	newID, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("EnsureRookieShip LastInsertId: %w", err)
	}

	// Fit civilian weapon in HiSlot0 (flag 27).
	_, err = db.ExecContext(ctx, `
		INSERT INTO entity
		  (itemName, typeID, ownerID, locationID, flag, contraband, singleton, quantity, x, y, z, customInfo)
		VALUES ('', ?, ?, ?, 27, 0, 1, 1, 0, 0, 0, NULL)`,
		weaponTypeID, charID, newID,
	)
	if err != nil {
		log.Printf("EnsureRookieShip: failed to fit weapon (typeID %d): %v", weaponTypeID, err)
	}

	// Fit Basic Miner in HiSlot1 (flag 28).
	_, err = db.ExecContext(ctx, `
		INSERT INTO entity
		  (itemName, typeID, ownerID, locationID, flag, contraband, singleton, quantity, x, y, z, customInfo)
		VALUES ('', 3651, ?, ?, 28, 0, 1, 1, 0, 0, 0, NULL)`,
		charID, newID,
	)
	if err != nil {
		log.Printf("EnsureRookieShip: failed to fit Basic Miner: %v", err)
	}

	return newID, nil
}

// SkillTraining holds an in-progress training entry for one character.
type SkillTraining struct {
	SkillItemID   int64
	TypeID        int64
	TargetLevel   int
	EndOfTraining uint64
}

// EnsureSkillTrainingTable creates the chrSkillTraining table if it does not
// already exist.  Call once at server startup after opening the DB.
func EnsureSkillTrainingTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS chrSkillTraining (
			characterID   INT UNSIGNED NOT NULL,
			skillItemID   INT UNSIGNED NOT NULL,
			typeID        INT UNSIGNED NOT NULL,
			targetLevel   TINYINT UNSIGNED NOT NULL,
			endOfTraining BIGINT UNSIGNED NOT NULL,
			PRIMARY KEY (characterID)
		) ENGINE=InnoDB`)
	return err
}

// GetSkillTraining returns the active training entry for charID, or nil if
// the character is not currently training anything.
func GetSkillTraining(ctx context.Context, db *sql.DB, charID int64) (*SkillTraining, error) {
	st := &SkillTraining{}
	err := db.QueryRowContext(ctx,
		`SELECT skillItemID, typeID, targetLevel, endOfTraining
		 FROM chrSkillTraining WHERE characterID = ?`, charID,
	).Scan(&st.SkillItemID, &st.TypeID, &st.TargetLevel, &st.EndOfTraining)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetSkillTraining: %w", err)
	}
	return st, nil
}

// skillSPForLevel returns cumulative skill-points needed to reach level.
// Formula: 250 * timeConstant * 32^((level-1)/2)
func skillSPForLevel(tc int64, level int) float64 {
	if level <= 0 {
		return 0
	}
	return float64(250*tc) * math.Pow(32.0, float64(level-1)/2.0)
}

// getCharStat reads one character attribute (e.g. intelligence=165) from
// entity_attributes, returning defVal when the row is absent.
func getCharStat(ctx context.Context, db *sql.DB, charID, attrID int64, defVal float64) float64 {
	var v sql.NullFloat64
	_ = db.QueryRowContext(ctx,
		`SELECT COALESCE(NULLIF(valueFloat, 0), CAST(valueInt AS DOUBLE), valueFloat)
		 FROM entity_attributes WHERE itemID = ? AND attributeID = ?`,
		charID, attrID,
	).Scan(&v)
	if v.Valid {
		return v.Float64
	}
	return defVal
}

// GetEffectiveCharAttr returns the effective value of a base attribute (164..168):
// (base + learning skill bonuses + implant bonuses) * (1 + 0.02*Learning level).
// Bonuses are the *Bonus attribute (baseAttrID + 11) on skills (times trained
// level) and plugged implants (flag 89, flat); Learning is typeID 3374. Base defaults to 20.
func GetEffectiveCharAttr(ctx context.Context, db *sql.DB, characterID, baseAttrID int64) float64 {
	base := getCharStat(ctx, db, characterID, baseAttrID, 20.0)
	bonusAttrID := baseAttrID + 11

	// Learning-skill bonuses: Σ bonus-per-level * trained level over the
	// character's skills that carry this *Bonus type attribute.
	var learnBonus sql.NullFloat64
	_ = db.QueryRowContext(ctx, `
		SELECT SUM(COALESCE(NULLIF(ta.valueFloat,0), ta.valueInt) * COALESCE(lv.valueInt,0))
		FROM entity e
		JOIN dgmTypeAttributes ta ON ta.typeID = e.typeID AND ta.attributeID = ?
		LEFT JOIN entity_attributes lv ON lv.itemID = e.itemID AND lv.attributeID = 280
		WHERE e.ownerID = ? AND e.flag IN (7, 61)`,
		bonusAttrID, characterID,
	).Scan(&learnBonus)

	// Implant bonuses: flat bonus from implants plugged into the character.
	var implBonus sql.NullFloat64
	_ = db.QueryRowContext(ctx, `
		SELECT SUM(COALESCE(NULLIF(ta.valueFloat,0), ta.valueInt))
		FROM entity e
		JOIN dgmTypeAttributes ta ON ta.typeID = e.typeID AND ta.attributeID = ?
		WHERE e.locationID = ? AND e.flag = 89`,
		bonusAttrID, characterID,
	).Scan(&implBonus)

	// Learning skill (3374) level → +2%/level on the whole attribute.
	var learningLevel sql.NullInt64
	_ = db.QueryRowContext(ctx, `
		SELECT lv.valueInt
		FROM entity e
		JOIN entity_attributes lv ON lv.itemID = e.itemID AND lv.attributeID = 280
		WHERE e.ownerID = ? AND e.typeID = 3374 AND e.flag IN (7, 61) LIMIT 1`,
		characterID,
	).Scan(&learningLevel)

	eff := base + learnBonus.Float64 + implBonus.Float64
	if learningLevel.Valid {
		eff *= 1.0 + 0.02*float64(learningLevel.Int64)
	}
	return eff
}

// skillSPRate returns the SP-per-minute rate for characterID training a skill
// with type attributes typeAttrs: primary + secondary/2 from typeAttrs[180]/[181],
// as effective attributes (GetEffectiveCharAttr), times rates.SkillTraining.
// Falls back to 30 SP/min when it computes below 1.
func skillSPRate(ctx context.Context, db *sql.DB, characterID int64, typeAttrs map[int64]float64) float64 {
	primaryAttrID := int64(typeAttrs[180])   // e.g. 165 = intelligence
	secondaryAttrID := int64(typeAttrs[181]) // e.g. 166 = memory

	primaryStat := GetEffectiveCharAttr(ctx, db, characterID, primaryAttrID)
	secondaryStat := 20.0
	if secondaryAttrID > 0 {
		secondaryStat = GetEffectiveCharAttr(ctx, db, characterID, secondaryAttrID)
	}
	spRate := primaryStat + secondaryStat/2.0
	if spRate < 1 {
		spRate = 30.0
	}
	return spRate * rates.SkillTraining
}

// StartTrainingSkill stops any training skill for characterID and starts
// skillItemID toward its next level from now. Returns the Win32 FILETIME of
// training completion.
func StartTrainingSkill(ctx context.Context, db *sql.DB, characterID, skillItemID int64) (uint64, error) {
	return startTrainingSkillAt(ctx, db, characterID, skillItemID, win32TimeNow())
}

// startTrainingSkillAt is StartTrainingSkill with an explicit start tick. The
// skill queue backdates a chained skill to the tick the previous one finished.
// See docs/character-skills.md#skill-training.
func startTrainingSkillAt(ctx context.Context, db *sql.DB, characterID, skillItemID int64, startTick uint64) (uint64, error) {
	// Fetch the skill entity: typeID, current level, current SP.
	var typeID int64
	var currentLevel, currentSP sql.NullInt64
	err := db.QueryRowContext(ctx, `
		SELECT e.typeID,
		       ea_lv.valueInt,
		       ea_sp.valueInt
		FROM entity e
		LEFT JOIN entity_attributes ea_lv ON ea_lv.itemID = e.itemID AND ea_lv.attributeID = 280
		LEFT JOIN entity_attributes ea_sp ON ea_sp.itemID = e.itemID AND ea_sp.attributeID = 276
		WHERE e.itemID = ? AND e.ownerID = ?`,
		skillItemID, characterID,
	).Scan(&typeID, &currentLevel, &currentSP)
	if err != nil {
		return 0, fmt.Errorf("StartTrainingSkill fetch skill: %w", err)
	}

	level := 0
	if currentLevel.Valid {
		level = int(currentLevel.Int64)
	}
	spCurrent := 0.0
	if currentSP.Valid {
		spCurrent = float64(currentSP.Int64)
	}

	targetLevel := level + 1
	if targetLevel > 5 {
		return 0, fmt.Errorf("StartTrainingSkill typeID=%d: %w", typeID, ErrSkillMaxLevel)
	}

	// Prerequisites are checked before anything is mutated, so a refused train
	// leaves the running training alone. See docs/character-skills.md#skill-training.
	if err := CheckSkillPrereqs(ctx, db, characterID, typeID, nil); err != nil {
		return 0, err
	}

	// Bank partial SP for any skill currently training before switching, so
	// switching skills does not silently discard progress on the old one.
	if _, err := StopTrainingSkill(ctx, db, characterID, 0); err != nil {
		return 0, fmt.Errorf("StartTrainingSkill bank previous: %w", err)
	}
	// Banking may have just written SP for this item (restarting the skill already
	// training), so re-read it; the snapshot above predates it.
	var bankedSP sql.NullInt64
	if err := db.QueryRowContext(ctx,
		`SELECT valueInt FROM entity_attributes WHERE itemID = ? AND attributeID = 276`,
		skillItemID).Scan(&bankedSP); err == nil && bankedSP.Valid && float64(bankedSP.Int64) > spCurrent {
		spCurrent = float64(bankedSP.Int64)
	}

	// Read training modifiers from dgmTypeAttributes.
	typeAttrs, err := GetTypeAttributes(ctx, db, typeID)
	if err != nil {
		return 0, fmt.Errorf("StartTrainingSkill type attrs: %w", err)
	}
	tc := int64(typeAttrs[275]) // skillTimeConstant
	if tc <= 0 {
		tc = 1
	}
	// SP rate (per minute) = primary + secondary/2 from the skill's learning attrs.
	spRate := skillSPRate(ctx, db, characterID, typeAttrs)

	// SP needed from current position to targetLevel.
	spNeeded := skillSPForLevel(tc, targetLevel) - spCurrent
	if spNeeded < 1 {
		spNeeded = 1
	}

	// 1 minute = 600,000,000 Win32 100-ns ticks.
	trainingTicks := uint64(spNeeded/spRate*60.0) * 10_000_000
	endOfTraining := startTick + trainingTicks
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("StartTrainingSkill begin: %w", err)
	}
	defer tx.Rollback()

	// Stop any previously-training skill for this character.
	if _, err := tx.ExecContext(ctx,
		`UPDATE entity SET flag = 7 WHERE ownerID = ? AND flag = 61`,
		characterID); err != nil {
		return 0, fmt.Errorf("StartTrainingSkill stop old: %w", err)
	}

	// Remove any stale training record.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM chrSkillTraining WHERE characterID = ?`, characterID); err != nil {
		return 0, fmt.Errorf("StartTrainingSkill delete old record: %w", err)
	}

	// Ensure initial level/SP attributes exist for a freshly-purchased book.
	if !currentLevel.Valid {
		if _, err := tx.ExecContext(ctx,
			`INSERT IGNORE INTO entity_attributes (itemID, attributeID, valueInt, valueFloat)
			 VALUES (?, 280, 0, NULL)`, skillItemID); err != nil {
			return 0, fmt.Errorf("StartTrainingSkill init level attr: %w", err)
		}
	}
	if !currentSP.Valid {
		if _, err := tx.ExecContext(ctx,
			`INSERT IGNORE INTO entity_attributes (itemID, attributeID, valueInt, valueFloat)
			 VALUES (?, 276, 0, NULL)`, skillItemID); err != nil {
			return 0, fmt.Errorf("StartTrainingSkill init sp attr: %w", err)
		}
	}

	// Flag 61 = in training; ownerID must match. singleton = 1 is required: godma's
	// UpdateItem skips a non-singleton non-charge item and never loads its attributes.
	if _, err := tx.ExecContext(ctx,
		`UPDATE entity SET flag = 61, singleton = 1, ownerID = ?, locationID = ? WHERE itemID = ? AND ownerID = ?`,
		characterID, characterID, skillItemID, characterID); err != nil {
		return 0, fmt.Errorf("StartTrainingSkill set flag: %w", err)
	}

	// Record training metadata.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO chrSkillTraining (characterID, skillItemID, typeID, targetLevel, endOfTraining)
		VALUES (?, ?, ?, ?, ?)`,
		characterID, skillItemID, typeID, targetLevel, endOfTraining); err != nil {
		return 0, fmt.Errorf("StartTrainingSkill insert record: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return endOfTraining, nil
}

// StopTrainingSkill aborts characterID's active training: SP earned so far is
// banked into attribute 276, the flag returns to 7 and the chrSkillTraining row
// is removed. A non-zero skillItemID stops only that skill. Returns the stopped
// itemID, or 0.
func StopTrainingSkill(ctx context.Context, db *sql.DB, characterID, skillItemID int64) (int64, error) {
	st, err := GetSkillTraining(ctx, db, characterID)
	if err != nil {
		return 0, err
	}
	if st == nil {
		return 0, nil
	}
	if skillItemID != 0 && st.SkillItemID != skillItemID {
		return 0, nil
	}

	// Banked SP = targetLevelSP - remainingMinutes * spRate, clamped to
	// [spAtStart, targetLevelSP]; the client's skillinfo.py progress math.
	typeAttrs, err := GetTypeAttributes(ctx, db, st.TypeID)
	if err != nil {
		return 0, fmt.Errorf("StopTrainingSkill type attrs: %w", err)
	}
	tc := int64(typeAttrs[275]) // skillTimeConstant
	if tc <= 0 {
		tc = 1
	}
	spRate := skillSPRate(ctx, db, characterID, typeAttrs)
	spTarget := skillSPForLevel(tc, st.TargetLevel)

	now := win32TimeNow()
	remainingMin := 0.0
	if st.EndOfTraining > now {
		remainingMin = float64(st.EndOfTraining-now) / (60.0 * 1e7)
	}
	banked := spTarget - remainingMin*spRate

	// Lower bound: the SP stored when this training began.
	var spStart sql.NullInt64
	_ = db.QueryRowContext(ctx,
		`SELECT valueInt FROM entity_attributes WHERE itemID = ? AND attributeID = 276`,
		st.SkillItemID).Scan(&spStart)
	lowerBound := 0.0
	if spStart.Valid {
		lowerBound = float64(spStart.Int64)
	}
	if banked < lowerBound {
		banked = lowerBound
	}
	if banked > spTarget {
		banked = spTarget
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("StopTrainingSkill begin: %w", err)
	}
	defer tx.Rollback()

	// Bank partial SP; level attribute 280 stays the current level.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO entity_attributes (itemID, attributeID, valueInt, valueFloat)
		VALUES (?, 276, ?, NULL)
		ON DUPLICATE KEY UPDATE valueInt = VALUES(valueInt)`,
		st.SkillItemID, int64(banked)); err != nil {
		return 0, fmt.Errorf("StopTrainingSkill sp: %w", err)
	}

	// Reset entity flag to flagSkill (7).
	if _, err := tx.ExecContext(ctx,
		`UPDATE entity SET flag = 7 WHERE itemID = ? AND ownerID = ?`,
		st.SkillItemID, characterID); err != nil {
		return 0, fmt.Errorf("StopTrainingSkill reset flag: %w", err)
	}

	// Remove the training record.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM chrSkillTraining WHERE characterID = ?`, characterID); err != nil {
		return 0, fmt.Errorf("StopTrainingSkill delete: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return st.SkillItemID, nil
}

// EnsureCharLearningAttrs sets any missing or zero row of the five base
// attributes (164..168) on the character entity to defaultVal; godma.py's
// GetTimeForTraining divides by them. Non-zero values are left alone. Returns
// the rows inserted or updated.
func EnsureCharLearningAttrs(ctx context.Context, db *sql.DB, charID int64, defaultVal int64) (int, error) {
	if defaultVal <= 0 {
		defaultVal = 20
	}
	n := 0
	for _, attrID := range []int64{164, 165, 166, 167, 168} {
		var val sql.NullInt64
		err := db.QueryRowContext(ctx,
			`SELECT valueInt FROM entity_attributes WHERE itemID = ? AND attributeID = ?`,
			charID, attrID,
		).Scan(&val)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return n, fmt.Errorf("EnsureCharLearningAttrs lookup attr=%d: %w", attrID, err)
		}
		if val.Valid && val.Int64 > 0 {
			continue
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO entity_attributes (itemID, attributeID, valueInt, valueFloat)
			VALUES (?, ?, ?, NULL)
			ON DUPLICATE KEY UPDATE valueInt = VALUES(valueInt)`,
			charID, attrID, defaultVal); err != nil {
			return n, fmt.Errorf("EnsureCharLearningAttrs set attr=%d: %w", attrID, err)
		}
		n++
	}
	return n, nil
}

// MigrateCharBaseAttrs rewrites every character's five base attributes
// (164..168) as bloodline base + chargen pick, reading a stored value <= 15 as
// the pick and anything larger as unknown (0), and deletes pick rows stored
// under 175-179. Runs once, guarded by server_migrations; returns characters migrated.
func MigrateCharBaseAttrs(ctx context.Context, db *sql.DB) (int, error) {
	const migrationName = "char-base-attrs-bloodline"
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS server_migrations (
			name    VARCHAR(64) NOT NULL,
			applied BIGINT NOT NULL,
			PRIMARY KEY (name)
		)`); err != nil {
		return 0, fmt.Errorf("MigrateCharBaseAttrs meta table: %w", err)
	}
	var done int
	_ = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM server_migrations WHERE name = ?`, migrationName).Scan(&done)
	if done > 0 {
		return 0, nil
	}

	// JOIN entity: character_ also holds NPC agents with no entity row, and
	// entity_attributes has a FK on entity.itemID.
	rows, err := db.QueryContext(ctx, `
		SELECT ch.characterID, b.charisma, b.intelligence, b.memory, b.perception, b.willpower
		FROM character_ ch
		JOIN entity e ON e.itemID = ch.characterID
		JOIN chrBloodlines b ON b.bloodlineID = ch.bloodlineID`)
	if err != nil {
		return 0, fmt.Errorf("MigrateCharBaseAttrs chars: %w", err)
	}
	type charBase struct {
		charID int64
		base   [5]int64 // order matches attrIDs below
	}
	var chars []charBase
	for rows.Next() {
		var cb charBase
		if err := rows.Scan(&cb.charID, &cb.base[0], &cb.base[1], &cb.base[2], &cb.base[3], &cb.base[4]); err != nil {
			rows.Close()
			return 0, fmt.Errorf("MigrateCharBaseAttrs scan: %w", err)
		}
		chars = append(chars, cb)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("MigrateCharBaseAttrs rows: %w", err)
	}

	attrIDs := [5]int64{164, 165, 166, 167, 168}
	for _, cb := range chars {
		for i, attrID := range attrIDs {
			var cur sql.NullInt64
			_ = db.QueryRowContext(ctx,
				`SELECT valueInt FROM entity_attributes WHERE itemID = ? AND attributeID = ?`,
				cb.charID, attrID).Scan(&cur)
			pick := int64(0)
			if cur.Valid && cur.Int64 > 0 && cur.Int64 <= 15 {
				pick = cur.Int64
			}
			if _, err := db.ExecContext(ctx, `
				INSERT INTO entity_attributes (itemID, attributeID, valueInt, valueFloat)
				VALUES (?, ?, ?, NULL)
				ON DUPLICATE KEY UPDATE valueInt = VALUES(valueInt), valueFloat = NULL`,
				cb.charID, attrID, cb.base[i]+pick); err != nil {
				return 0, fmt.Errorf("MigrateCharBaseAttrs set char=%d attr=%d: %w", cb.charID, attrID, err)
			}
		}
	}

	// Drop the duplicate pick rows written under the *Bonus IDs.
	if _, err := db.ExecContext(ctx, `
		DELETE ea FROM entity_attributes ea
		JOIN character_ ch ON ch.characterID = ea.itemID
		WHERE ea.attributeID BETWEEN 175 AND 179`); err != nil {
		return len(chars), fmt.Errorf("MigrateCharBaseAttrs delete 175-179: %w", err)
	}

	if _, err := db.ExecContext(ctx,
		`INSERT INTO server_migrations (name, applied) VALUES (?, ?)`,
		migrationName, win32TimeNow()); err != nil {
		return len(chars), fmt.Errorf("MigrateCharBaseAttrs mark applied: %w", err)
	}
	return len(chars), nil
}

// ClearOrphanTrainingFlags resets flag 61 to 7 on any skill of charID with no
// chrSkillTraining row; the client renders a stray 61 as in training with no
// ETA. Returns the number of skills repaired.
func ClearOrphanTrainingFlags(ctx context.Context, db *sql.DB, charID int64) (int64, error) {
	res, err := db.ExecContext(ctx, `
		UPDATE entity SET flag = 7
		 WHERE ownerID = ?
		   AND flag = 61
		   AND itemID NOT IN (
		     SELECT skillItemID FROM chrSkillTraining WHERE characterID = ?
		   )`, charID, charID)
	if err != nil {
		return 0, fmt.Errorf("ClearOrphanTrainingFlags: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// RepairSkillSingletons sets singleton = 1 on every skill in a brain (flag 7 or
// 61) still flagged packaged: godma's UpdateItem skips a non-singleton item and
// never loads its level/SP attributes. Run once at boot. Returns rows repaired.
func RepairSkillSingletons(ctx context.Context, db *sql.DB) (int64, error) {
	res, err := db.ExecContext(ctx, `
		UPDATE entity e
		  JOIN invTypes  t ON t.typeID  = e.typeID
		  JOIN invGroups g ON g.groupID = t.groupID
		   SET e.singleton = 1
		 WHERE e.flag IN (7, 61)
		   AND e.singleton = 0
		   AND g.categoryID = 16`)
	if err != nil {
		return 0, fmt.Errorf("RepairSkillSingletons: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// RepairChargeSingletons sets singleton = 0 on every charge (categoryID 8) with
// singleton = 1; charges are stackable and never assembled. Run once at boot.
// Returns rows repaired.
func RepairChargeSingletons(ctx context.Context, db *sql.DB) (int64, error) {
	res, err := db.ExecContext(ctx, `
		UPDATE entity e
		  JOIN invTypes  t ON t.typeID  = e.typeID
		  JOIN invGroups g ON g.groupID = t.groupID
		   SET e.singleton = 0
		 WHERE e.singleton = 1
		   AND g.categoryID = 8`)
	if err != nil {
		return 0, fmt.Errorf("RepairChargeSingletons: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// CompleteTrainingIfDue checks whether the active training for charID is
// finished (endOfTraining ≤ now).  If so: advances level/SP in entity_attributes,
// resets entity flag back to flagSkill (7), and removes the chrSkillTraining row.
// Returns the itemID of the skill that was advanced, or 0 if nothing was due.
func CompleteTrainingIfDue(ctx context.Context, db *sql.DB, charID int64) (int64, error) {
	st, err := GetSkillTraining(ctx, db, charID)
	if err != nil {
		return 0, err
	}
	if st == nil || st.EndOfTraining > win32TimeNow() {
		return 0, nil
	}

	tc, _ := GetSkillTimeConstant(ctx, db, st.TypeID)
	spCompleted := int64(skillSPForLevel(tc, st.TargetLevel))

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("CompleteTrainingIfDue begin: %w", err)
	}
	defer tx.Rollback()

	// Update level attribute (attrID 280).
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO entity_attributes (itemID, attributeID, valueInt, valueFloat)
		VALUES (?, 280, ?, NULL)
		ON DUPLICATE KEY UPDATE valueInt = VALUES(valueInt)`,
		st.SkillItemID, st.TargetLevel); err != nil {
		return 0, fmt.Errorf("CompleteTrainingIfDue level: %w", err)
	}

	// Update SP attribute (attrID 276).
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO entity_attributes (itemID, attributeID, valueInt, valueFloat)
		VALUES (?, 276, ?, NULL)
		ON DUPLICATE KEY UPDATE valueInt = VALUES(valueInt)`,
		st.SkillItemID, spCompleted); err != nil {
		return 0, fmt.Errorf("CompleteTrainingIfDue sp: %w", err)
	}

	// Reset entity flag to flagSkill (7).
	if _, err := tx.ExecContext(ctx,
		`UPDATE entity SET flag = 7 WHERE itemID = ? AND ownerID = ?`,
		st.SkillItemID, charID); err != nil {
		return 0, fmt.Errorf("CompleteTrainingIfDue reset flag: %w", err)
	}

	// Remove the training record.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM chrSkillTraining WHERE characterID = ?`, charID); err != nil {
		return 0, fmt.Errorf("CompleteTrainingIfDue delete: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return st.SkillItemID, nil
}

// GiveAllSkills grants every skill in invTypes (categoryID=16) to charID at
// level 5 with full skill points.  Skills the character already owns are
// updated to level 5; skills in training are left in training but their
// target level is raised.  Returns the number of skills inserted or updated.
func GiveAllSkills(ctx context.Context, db *sql.DB, charID int64) (int, error) {
	// Skills live in the character (locationID = charID, flag 7), never in its
	// container: godma.Prime raises KeyError on a parent CharGetInfo never listed.

	// All skill typeIDs (categoryID=16).
	rows, err := db.QueryContext(ctx, `
		SELECT t.typeID
		FROM invTypes  t
		JOIN invGroups g ON g.groupID = t.groupID
		WHERE g.categoryID = 16 AND t.published = 1`)
	if err != nil {
		return 0, fmt.Errorf("GiveAllSkills fetch types: %w", err)
	}
	var typeIDs []int64
	for rows.Next() {
		var tid int64
		if err := rows.Scan(&tid); err == nil {
			typeIDs = append(typeIDs, tid)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("GiveAllSkills scan types: %w", err)
	}

	count := 0
	for _, typeID := range typeIDs {
		tc, _ := GetSkillTimeConstant(ctx, db, typeID)
		sp5 := int64(skillSPForLevel(tc, 5))

		// Check if character already owns this skill type.
		var existingItemID sql.NullInt64
		_ = db.QueryRowContext(ctx,
			`SELECT itemID FROM entity WHERE typeID = ? AND ownerID = ? AND flag IN (7, 61) LIMIT 1`,
			typeID, charID,
		).Scan(&existingItemID)

		if existingItemID.Valid {
			// Existing skill: upsert level 5 and max SP.
			_, _ = db.ExecContext(ctx, `
				INSERT INTO entity_attributes (itemID, attributeID, valueInt, valueFloat)
				VALUES (?, 280, 5, NULL)
				ON DUPLICATE KEY UPDATE valueInt = 5`,
				existingItemID.Int64)
			_, _ = db.ExecContext(ctx, `
				INSERT INTO entity_attributes (itemID, attributeID, valueInt, valueFloat)
				VALUES (?, 276, ?, NULL)
				ON DUPLICATE KEY UPDATE valueInt = VALUES(valueInt)`,
				existingItemID.Int64, sp5)
		} else {
			// New skill: insert entity then attributes.
			r, err := db.ExecContext(ctx, `
				INSERT INTO entity
				  (itemName, typeID, ownerID, locationID, flag, contraband, singleton, quantity, x, y, z, customInfo)
				VALUES ('Skill', ?, ?, ?, 7, 0, 1, 1, 0, 0, 0, NULL)`,
				typeID, charID, charID)
			if err != nil {
				continue // non-fatal; skip this skill
			}
			itemID, err := r.LastInsertId()
			if err != nil {
				continue
			}
			_, _ = db.ExecContext(ctx,
				`INSERT IGNORE INTO entity_attributes (itemID, attributeID, valueInt, valueFloat) VALUES (?, 280, 5, NULL)`,
				itemID)
			_, _ = db.ExecContext(ctx,
				`INSERT IGNORE INTO entity_attributes (itemID, attributeID, valueInt, valueFloat) VALUES (?, 276, ?, NULL)`,
				itemID, sp5)
		}
		count++
	}
	return count, nil
}

// FixSkillLocationIDs corrects skill entities whose locationID was incorrectly
// set to a stationID instead of the ownerID (characterID).  Returns the number
// of rows updated.
func FixSkillLocationIDs(ctx context.Context, db *sql.DB) (int64, error) {
	r, err := db.ExecContext(ctx, `
		UPDATE entity SET locationID = ownerID
		WHERE flag IN (7, 61) AND locationID != ownerID
		  AND ownerID >= 140000000`)
	if err != nil {
		return 0, fmt.Errorf("FixSkillLocationIDs: %w", err)
	}
	return r.RowsAffected()
}

// FittedModule holds one fitted module entity row.
type FittedModule struct {
	ItemID     int64
	TypeID     int64
	OwnerID    int64
	LocationID int64 // shipID
	Flag       int64 // 11-34 for module slots
	GroupID    int64
	CategoryID int64
}

// GetFittedModules returns all module items fitted to a ship (flags 11-34:
// hi/med/lo slots).
func GetFittedModules(ctx context.Context, db *sql.DB, shipID int64) ([]FittedModule, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT e.itemID, e.typeID, e.ownerID, e.locationID, e.flag,
		       t.groupID, COALESCE(g.categoryID, 0)
		FROM entity e
		JOIN invTypes  t ON t.typeID  = e.typeID
		JOIN invGroups g ON g.groupID = t.groupID
		WHERE e.locationID = ? AND e.flag BETWEEN 11 AND 34
		  AND g.categoryID != 8`, shipID)
	if err != nil {
		return nil, fmt.Errorf("GetFittedModules: %w", err)
	}
	defer rows.Close()
	var out []FittedModule
	for rows.Next() {
		var m FittedModule
		if err := rows.Scan(&m.ItemID, &m.TypeID, &m.OwnerID, &m.LocationID, &m.Flag,
			&m.GroupID, &m.CategoryID); err != nil {
			return nil, fmt.Errorf("GetFittedModules scan: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetShipCPUPowerLoad sums the cpu (attr 50) and power (attr 30) of every online
// module fitted to a ship (flags 11-34) at its effective need: the superweapon
// CPU factor, the owner's fitting skills and implants and the hull's role bonuses
// (ModuleFittingNeedFrom). The online gate charges a new module through
// GetModuleFittingNeed; keep both on one fold.
func GetShipCPUPowerLoad(ctx context.Context, db *sql.DB, shipID int64) (cpu, pg float64, err error) {
	// entity_attributes attr 2 = persisted isOnline state.
	rows, err := db.QueryContext(ctx, `
		SELECT e.typeID, t2.groupID, COUNT(*)
		FROM entity e
		JOIN entity_attributes ea ON ea.itemID = e.itemID
		 AND ea.attributeID = 2 AND ea.valueInt = 1
		JOIN invTypes  t2 ON t2.typeID  = e.typeID
		JOIN invGroups g2 ON g2.groupID = t2.groupID
		WHERE e.locationID = ? AND e.flag BETWEEN 11 AND 34
		  AND g2.categoryID != 8
		GROUP BY e.typeID, t2.groupID`, shipID)
	if err != nil {
		return 0, 0, fmt.Errorf("GetShipCPUPowerLoad: %w", err)
	}
	type fitted struct{ typeID, groupID, n int64 }
	var mods []fitted
	for rows.Next() {
		var f fitted
		if err := rows.Scan(&f.typeID, &f.groupID, &f.n); err != nil {
			rows.Close()
			return 0, 0, fmt.Errorf("GetShipCPUPowerLoad scan: %w", err)
		}
		mods = append(mods, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("GetShipCPUPowerLoad rows: %w", err)
	}
	if len(mods) == 0 {
		return 0, 0, nil
	}
	// The pilot whose skills apply is the ship's owner.
	var ownerID, shipTypeID int64
	_ = db.QueryRowContext(ctx, `SELECT ownerID, typeID FROM entity WHERE itemID = ?`, shipID).Scan(&ownerID, &shipTypeID)
	levels, skillAttrs := needSkillReaders(ctx, db, ownerID)
	hull := GetHullCPUNeed(ctx, db, ownerID, shipTypeID)
	im := GetCharImplantMults(ctx, db, ownerID)
	for _, m := range mods {
		attrs, aerr := TypeAttrs(ctx, db, m.typeID)
		if aerr != nil {
			return 0, 0, fmt.Errorf("GetShipCPUPowerLoad attrs %d: %w", m.typeID, aerr)
		}
		c, p := ModuleFittingNeedFrom(attrs, m.groupID, levels, skillAttrs, hull, im)
		cpu += c * float64(m.n)
		pg += p * float64(m.n)
	}
	return cpu, pg, nil
}

// TypeEffect holds one dgmTypeEffects row.
type TypeEffect struct {
	EffectID    int64
	IsDefault   bool
	EffectName  string
	Description string
}

// GetTypeEffects returns all effects for a typeID from dgmTypeEffects joined
// with dgmEffects for effect name.
func GetTypeEffects(ctx context.Context, db *sql.DB, typeID int64) ([]TypeEffect, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT te.effectID, te.isDefault,
		       COALESCE(e.effectName, ''), COALESCE(e.description, '')
		FROM dgmTypeEffects te
		LEFT JOIN dgmEffects e ON e.effectID = te.effectID
		WHERE te.typeID = ?`, typeID)
	if err != nil {
		return nil, fmt.Errorf("GetTypeEffects: %w", err)
	}
	defer rows.Close()
	var out []TypeEffect
	for rows.Next() {
		var te TypeEffect
		if err := rows.Scan(&te.EffectID, &te.IsDefault, &te.EffectName, &te.Description); err != nil {
			return nil, fmt.Errorf("GetTypeEffects scan: %w", err)
		}
		out = append(out, te)
	}
	return out, rows.Err()
}

// CreateFittedModule inserts a module entity into the entity table fitted to
// the given ship at the specified slot flag.
func CreateFittedModule(ctx context.Context, db *sql.DB, typeID, ownerID, shipID int64, flag int) (int64, error) {
	var typeName string
	err := db.QueryRowContext(ctx,
		`SELECT typeName FROM invTypes WHERE typeID = ?`, typeID).Scan(&typeName)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("CreateFittedModule: typeID %d not found", typeID)
		}
		return 0, fmt.Errorf("CreateFittedModule lookup: %w", err)
	}

	res, err := db.ExecContext(ctx, `
		INSERT INTO entity
		  (itemName, typeID, ownerID, locationID, flag, contraband, singleton, quantity, x, y, z, customInfo)
		VALUES (?, ?, ?, ?, ?, 0, 1, 1, 0, 0, 0, NULL)`,
		typeName, typeID, ownerID, shipID, flag)
	if err != nil {
		return 0, fmt.Errorf("CreateFittedModule insert: %w", err)
	}
	itemID, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("CreateFittedModule LastInsertId: %w", err)
	}
	return itemID, nil
}

// CreateCargoItem inserts an item into ship cargo (flag=5).
func CreateCargoItem(ctx context.Context, db *sql.DB, typeID, ownerID, shipID, quantity int64) (int64, error) {
	var typeName string
	err := db.QueryRowContext(ctx,
		`SELECT typeName FROM invTypes WHERE typeID = ?`, typeID).Scan(&typeName)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("CreateCargoItem: typeID %d not found", typeID)
		}
		return 0, fmt.Errorf("CreateCargoItem lookup: %w", err)
	}

	res, err := db.ExecContext(ctx, `
		INSERT INTO entity
		  (itemName, typeID, ownerID, locationID, flag, contraband, singleton, quantity, x, y, z, customInfo)
		VALUES (?, ?, ?, ?, 5, 0, 0, ?, 0, 0, 0, NULL)`,
		typeName, typeID, ownerID, shipID, quantity)
	if err != nil {
		return 0, fmt.Errorf("CreateCargoItem insert: %w", err)
	}
	itemID, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("CreateCargoItem LastInsertId: %w", err)
	}
	return itemID, nil
}

// SplitStack splits off one unit from a stacked entity (quantity > 1).
// It decrements the original entity's quantity by 1 and inserts a new entity
// row for the single unit.  Returns the new entity's itemID.
// If the entity has quantity <= 1, it returns the original itemID unchanged.
func SplitStack(ctx context.Context, db *sql.DB, itemID int64) (int64, error) {
	var qty int64
	var typeName string
	var typeID, ownerID, locationID, flag int64
	err := db.QueryRowContext(ctx, `
		SELECT e.quantity, COALESCE(e.itemName,''), e.typeID, e.ownerID, e.locationID, e.flag
		FROM entity e WHERE e.itemID = ?`, itemID,
	).Scan(&qty, &typeName, &typeID, &ownerID, &locationID, &flag)
	if err != nil {
		return 0, fmt.Errorf("SplitStack lookup: %w", err)
	}
	if qty <= 1 {
		return itemID, nil // nothing to split
	}
	// Decrement quantity on the original stack.
	if _, err := db.ExecContext(ctx,
		`UPDATE entity SET quantity = quantity - 1 WHERE itemID = ?`, itemID); err != nil {
		return 0, fmt.Errorf("SplitStack decrement: %w", err)
	}
	// Insert a new entity row for the single split-off unit.
	res, err := db.ExecContext(ctx, `
		INSERT INTO entity
		  (itemName, typeID, ownerID, locationID, flag, contraband, singleton, quantity, x, y, z, customInfo)
		VALUES (?, ?, ?, ?, ?, 0, 0, 1, 0, 0, 0, NULL)`,
		typeName, typeID, ownerID, locationID, flag)
	if err != nil {
		return 0, fmt.Errorf("SplitStack insert: %w", err)
	}
	newID, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("SplitStack LastInsertId: %w", err)
	}
	return newID, nil
}

// SplitStackN splits n units off a stacked entity (quantity > n) into a new
// entity row and returns its itemID. When quantity <= n it returns the original
// itemID unchanged; the caller moves the whole stack.
func SplitStackN(ctx context.Context, db *sql.DB, itemID, n int64) (int64, error) {
	var qty int64
	var typeName string
	var typeID, ownerID, locationID, flag int64
	err := db.QueryRowContext(ctx, `
		SELECT e.quantity, COALESCE(e.itemName,''), e.typeID, e.ownerID, e.locationID, e.flag
		FROM entity e WHERE e.itemID = ?`, itemID,
	).Scan(&qty, &typeName, &typeID, &ownerID, &locationID, &flag)
	if err != nil {
		return 0, fmt.Errorf("SplitStackN lookup: %w", err)
	}
	if qty <= n {
		return itemID, nil // nothing to split; caller moves the whole stack
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE entity SET quantity = quantity - ? WHERE itemID = ?`, n, itemID); err != nil {
		return 0, fmt.Errorf("SplitStackN decrement: %w", err)
	}
	res, err := db.ExecContext(ctx, `
		INSERT INTO entity
		  (itemName, typeID, ownerID, locationID, flag, contraband, singleton, quantity, x, y, z, customInfo)
		VALUES (?, ?, ?, ?, ?, 0, 0, ?, 0, 0, 0, NULL)`,
		typeName, typeID, ownerID, locationID, flag, n)
	if err != nil {
		return 0, fmt.Errorf("SplitStackN insert: %w", err)
	}
	newID, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("SplitStackN LastInsertId: %w", err)
	}
	return newID, nil
}

// FitModuleToShip moves an existing entity onto a ship at the given slot flag,
// setting singleton=1, quantity=1 (fitted modules are always assembled).
func FitModuleToShip(ctx context.Context, db *sql.DB, itemID, shipID, flag int64) error {
	_, err := db.ExecContext(ctx,
		`UPDATE entity SET locationID=?, flag=?, singleton=1, quantity=1 WHERE itemID=?`,
		shipID, flag, itemID)
	if err != nil {
		return fmt.Errorf("FitModuleToShip: %w", err)
	}
	return nil
}

// UnfitModuleToHangar moves a fitted module to the station hangar (flag 4) as a
// packaged item and drops the persisted online state (attr 2).
func UnfitModuleToHangar(ctx context.Context, db *sql.DB, itemID, stationID int64) error {
	// Ships (category 6) and containers keep their singleton: the client shows a
	// non-singleton container as packaged. A separate read keeps invTypes/invGroups unlocked.
	var keepAssembled bool
	_ = db.QueryRowContext(ctx, `
		SELECT g.categoryID = 6 OR t.groupID IN (12, 340, 448, 649)
		FROM entity e
		JOIN invTypes t ON t.typeID = e.typeID
		JOIN invGroups g ON g.groupID = t.groupID
		WHERE e.itemID = ?`, itemID).Scan(&keepAssembled)
	q := `UPDATE entity SET locationID=?, flag=4, singleton=0 WHERE itemID=?`
	if keepAssembled {
		q = `UPDATE entity SET locationID=?, flag=4 WHERE itemID=?`
	}
	_, err := db.ExecContext(ctx, q, stationID, itemID)
	if err != nil {
		return fmt.Errorf("UnfitModuleToHangar: %w", err)
	}
	_, _ = db.ExecContext(ctx,
		`DELETE FROM entity_attributes WHERE itemID=? AND attributeID=2`, itemID)
	return nil
}

// GetModuleSlotType returns the slot type ("hi", "med", or "lo") for a module
// typeID by checking which power-slot effect it has in dgmTypeEffects.
// effectID 11 = loPower, 12 = hiPower, 13 = medPower.
func GetModuleSlotType(ctx context.Context, db *sql.DB, typeID int64) (string, error) {
	var effectID int64
	err := db.QueryRowContext(ctx, `
		SELECT effectID FROM dgmTypeEffects
		WHERE typeID = ? AND effectID IN (11, 12, 13)
		LIMIT 1`, typeID).Scan(&effectID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("GetModuleSlotType: typeID %d has no power slot effect", typeID)
		}
		return "", fmt.Errorf("GetModuleSlotType: %w", err)
	}
	switch effectID {
	case 12:
		return "hi", nil
	case 13:
		return "med", nil
	case 11:
		return "lo", nil
	}
	return "", fmt.Errorf("GetModuleSlotType: unexpected effectID %d", effectID)
}

// GetShipCargoItems returns a util.Rowset of items owned by charID at flag 5 or
// 87 in shipID: the flat list InventoryBound.List() answers for a cargo hold.
func GetShipCargoItems(ctx context.Context, db *sql.DB, charID, shipID int64) (marshal.PyRep, error) {
	log.Printf("[db] GetShipCargoItems charID=%d shipID=%d", charID, shipID)
	assetCols := []string{
		"itemID", "typeID", "ownerID", "locationID", "flag",
		"contraband", "singleton", "quantity", "groupID", "categoryID", "customInfo",
	}
	// Flag 5 = cargo hold, 87 = drone bay; the client's invCache.List(flag) filters
	// per view, so ListDroneBay() sees only the flag-87 rows.
	rows, err := db.QueryContext(ctx,
		`SELECT e.itemID, e.typeID, e.ownerID, e.locationID, e.flag,
		        e.contraband, e.singleton, e.quantity,
		        COALESCE(t.groupID, 0), COALESCE(g.categoryID, 0)
		   FROM entity e
		   LEFT JOIN invTypes t ON t.typeID = e.typeID
		   LEFT JOIN invGroups g ON g.groupID = t.groupID
		  WHERE e.ownerID = ? AND e.flag IN (5, 87) AND e.locationID = ?
		  ORDER BY t.typeID, e.itemID`,
		charID, shipID,
	)
	if err != nil {
		return nil, fmt.Errorf("GetShipCargoItems query: %w", err)
	}
	defer rows.Close()

	var result [][]marshal.PyRep
	for rows.Next() {
		var a AssetItem
		if err := rows.Scan(&a.ItemID, &a.TypeID, &a.OwnerID, &a.LocationID,
			&a.Flag, &a.Contraband, &a.Singleton, &a.Quantity,
			&a.GroupID, &a.CategoryID); err != nil {
			return nil, fmt.Errorf("GetShipCargoItems scan: %w", err)
		}
		result = append(result, []marshal.PyRep{
			intRep(a.ItemID), intRep(a.TypeID), intRep(a.OwnerID), intRep(a.LocationID),
			intRep(a.Flag), intRep(a.Contraband), intRep(a.Singleton), intRep(a.Quantity),
			intRep(a.GroupID), intRep(a.CategoryID), marshal.PyNone{},
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("GetShipCargoItems rows: %w", err)
	}
	log.Printf("[db] GetShipCargoItems charID=%d shipID=%d → %d items", charID, shipID, len(result))
	return makeRowset(assetCols, result), nil
}

// FindCargoItemByType looks for an existing cargo stack (flag=5) of the given
// typeID owned by ownerID inside the given shipID.  Returns (itemID, quantity)
// or (0, 0) when no stack exists.
func FindCargoItemByType(ctx context.Context, db *sql.DB, typeID, ownerID, shipID int64) (int64, int64, error) {
	var itemID, qty int64
	err := db.QueryRowContext(ctx,
		`SELECT itemID, quantity FROM entity
		  WHERE typeID = ? AND ownerID = ? AND locationID = ? AND flag = 5
		  LIMIT 1`,
		typeID, ownerID, shipID,
	).Scan(&itemID, &qty)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, 0, nil
		}
		return 0, 0, fmt.Errorf("FindCargoItemByType: %w", err)
	}
	return itemID, qty, nil
}

// UpdateCargoItemQty sets the quantity of an existing entity row.
func UpdateCargoItemQty(ctx context.Context, db DBTX, itemID, newQty int64) error {
	_, err := db.ExecContext(ctx,
		`UPDATE entity SET quantity = ? WHERE itemID = ?`, newQty, itemID)
	if err != nil {
		return fmt.Errorf("UpdateCargoItemQty: %w", err)
	}
	return nil
}

// MergeLandedStack folds a just-moved or created stack into an older
// non-singleton stack of the same type, container, flag and owner, keeping the
// older row. Returns nil when there is nothing to merge into; on a merge
// DeletedRows holds the swallowed row's snapshot for the caller's OnItemChange.
func MergeLandedStack(ctx context.Context, db *sql.DB, itemID int64) (*StackMergeResult, error) {
	er, err := GetEntityRow(ctx, db, itemID)
	if err != nil {
		return nil, fmt.Errorf("MergeLandedStack row %d: %w", itemID, err)
	}
	if er == nil || er.Singleton != 0 {
		return nil, nil
	}
	if GetItemAttrFloat(ctx, db, itemID, 3, 0) > 0 {
		return nil, nil // worn: its damage belongs to this item alone
	}
	var keeperID, keeperQty int64
	err = db.QueryRowContext(ctx,
		`SELECT itemID, quantity FROM entity
		  WHERE typeID = ? AND locationID = ? AND flag = ? AND ownerID = ?
		    AND singleton = 0 AND itemID <> ?`+notWornSQL("entity")+`
		  ORDER BY itemID LIMIT 1`,
		er.TypeID, er.LocationID, er.Flag, er.OwnerID, itemID,
	).Scan(&keeperID, &keeperQty)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("MergeLandedStack find keeper for %d: %w", itemID, err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE entity SET quantity = ? WHERE itemID = ?`, keeperQty+er.Quantity, keeperID); err != nil {
		return nil, fmt.Errorf("MergeLandedStack grow keeper %d: %w", keeperID, err)
	}
	if _, err := db.ExecContext(ctx,
		`DELETE FROM entity WHERE itemID = ?`, itemID); err != nil {
		return nil, fmt.Errorf("MergeLandedStack delete %d: %w", itemID, err)
	}
	return &StackMergeResult{
		KeptItemID:  keeperID,
		OldQty:      keeperQty,
		NewQty:      keeperQty + er.Quantity,
		DeletedIDs:  []int64{itemID},
		DeletedRows: []*EntityRow{er},
	}, nil
}

// StackMergeResult describes one merge: the surviving item got its quantity
// updated; all other items in the group were deleted.
type StackMergeResult struct {
	KeptItemID  int64        // item that received the combined quantity
	OldQty      int64        // quantity of KeptItemID before merge
	NewQty      int64        // quantity of KeptItemID after merge
	DeletedIDs  []int64      // items that were deleted (merged into KeptItemID)
	DeletedRows []*EntityRow // snapshot of deleted items before deletion
}

// StackAllItems merges non-singleton items of one typeID inside a container.
// ownerID <= 0 matches any owner (loot cans, POS bays); flag < 0 matches every
// flag, but merges stay within one (typeID, flag, ownerID) group. Returns the
// merges performed for the caller's client notifications.
func StackAllItems(ctx context.Context, db *sql.DB, ownerID, locationID int64, flag int64) ([]StackMergeResult, error) {
	q := `SELECT e.itemID, e.typeID, e.ownerID, e.locationID, e.flag,
	             COALESCE(t.groupID, 0), COALESCE(g.categoryID, 0),
	             e.singleton, e.quantity
	        FROM entity e
	        LEFT JOIN invTypes t ON t.typeID = e.typeID
	        LEFT JOIN invGroups g ON g.groupID = t.groupID
	       WHERE e.locationID = ? AND e.singleton = 0` + notWornSQL("e")
	args := []any{locationID}
	if ownerID > 0 {
		q += ` AND e.ownerID = ?`
		args = append(args, ownerID)
	}
	if flag >= 0 {
		q += ` AND e.flag = ?`
		args = append(args, flag)
	}
	q += ` ORDER BY e.itemID`

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("StackAllItems query: %w", err)
	}
	// Group key: same type, same flag, same owner.
	type stackKey struct{ typeID, flag, ownerID int64 }
	var order []stackKey
	groups := make(map[stackKey][]*EntityRow)
	for rows.Next() {
		var er EntityRow
		if err := rows.Scan(&er.ItemID, &er.TypeID, &er.OwnerID, &er.LocationID,
			&er.Flag, &er.GroupID, &er.CategoryID, &er.Singleton, &er.Quantity); err != nil {
			rows.Close()
			return nil, fmt.Errorf("StackAllItems scan: %w", err)
		}
		k := stackKey{er.TypeID, er.Flag, er.OwnerID}
		if _, seen := groups[k]; !seen {
			order = append(order, k)
		}
		groups[k] = append(groups[k], &er)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("StackAllItems rows: %w", err)
	}

	var results []StackMergeResult
	for _, k := range order {
		items := groups[k]
		if len(items) < 2 {
			continue
		}

		// First item keeps the combined quantity; the rest are deleted.
		keeper := items[0]
		oldQty := keeper.Quantity
		totalQty := oldQty
		var deletedIDs []int64
		var deletedRows []*EntityRow
		for _, it := range items[1:] {
			totalQty += it.Quantity
			deletedIDs = append(deletedIDs, it.ItemID)
			deletedRows = append(deletedRows, it)
		}

		// Update keeper quantity.
		if _, err := db.ExecContext(ctx,
			`UPDATE entity SET quantity = ? WHERE itemID = ?`, totalQty, keeper.ItemID); err != nil {
			return nil, fmt.Errorf("StackAllItems update keeper %d: %w", keeper.ItemID, err)
		}

		// Delete merged items.
		for _, did := range deletedIDs {
			if _, err := db.ExecContext(ctx,
				`DELETE FROM entity WHERE itemID = ?`, did); err != nil {
				return nil, fmt.Errorf("StackAllItems delete %d: %w", did, err)
			}
		}

		results = append(results, StackMergeResult{
			KeptItemID:  keeper.ItemID,
			OldQty:      oldQty,
			NewQty:      totalQty,
			DeletedIDs:  deletedIDs,
			DeletedRows: deletedRows,
		})
	}
	return results, nil
}

// GetTypeVolume returns the volume (m³ per unit) for a typeID from invTypes.
func GetTypeVolume(ctx context.Context, db *sql.DB, typeID int64) (float64, error) {
	var vol float64
	err := db.QueryRowContext(ctx,
		`SELECT volume FROM invTypes WHERE typeID = ?`, typeID).Scan(&vol)
	if err != nil {
		return 0, fmt.Errorf("GetTypeVolume: %w", err)
	}
	return vol, nil
}

// GetShipCargoCapacity returns the cargo capacity (m³) for a ship typeID from invTypes.
func GetShipCargoCapacity(ctx context.Context, db *sql.DB, shipTypeID int64) (float64, error) {
	var cap float64
	err := db.QueryRowContext(ctx,
		`SELECT capacity FROM invTypes WHERE typeID = ?`, shipTypeID).Scan(&cap)
	if err != nil {
		return 0, fmt.Errorf("GetShipCargoCapacity: %w", err)
	}
	return cap, nil
}

// GetCargoUsedVolume sums the total volume (m³) of all cargo items (flag=5)
// inside a ship, by joining with invTypes to get per-unit volume.
func GetCargoUsedVolume(ctx context.Context, db *sql.DB, charID, shipID int64) (float64, error) {
	var total sql.NullFloat64
	// A courier package's type volume is a 0.01 m³ placeholder; it weighs
	// what its contract carries (see ItemVolume).
	err := db.QueryRowContext(ctx, `
		SELECT SUM(e.quantity * IF(e.typeID = ?, COALESCE(cm.volume, 0), t.volume))
		FROM entity e
		JOIN invTypes t ON t.typeID = e.typeID
		LEFT JOIN couriermissions cm ON cm.craterID = e.itemID AND e.typeID = ?
		WHERE e.ownerID = ? AND e.locationID = ? AND e.flag = 5`,
		TypePlasticWrap, TypePlasticWrap, charID, shipID).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("GetCargoUsedVolume: %w", err)
	}
	if !total.Valid {
		return 0, nil
	}
	return total.Float64, nil
}

func FindFreeSlot(ctx context.Context, db *sql.DB, shipID int64, slotType string, maxSlots int64) (int64, error) {
	var lo int64
	switch slotType {
	case "lo":
		lo = 11
	case "med":
		lo = 19
	case "hi":
		lo = 27
	default:
		return 0, fmt.Errorf("FindFreeSlot: unknown slotType %q", slotType)
	}
	if maxSlots <= 0 {
		return 0, nil // ship has no slots of this type
	}
	hi := lo + maxSlots - 1
	rows, err := db.QueryContext(ctx,
		`SELECT flag FROM entity WHERE locationID = ? AND flag BETWEEN ? AND ?`,
		shipID, lo, hi)
	if err != nil {
		return 0, fmt.Errorf("FindFreeSlot: %w", err)
	}
	defer rows.Close()
	occupied := make(map[int64]bool)
	for rows.Next() {
		var f int64
		if err := rows.Scan(&f); err != nil {
			return 0, fmt.Errorf("FindFreeSlot scan: %w", err)
		}
		occupied[f] = true
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("FindFreeSlot rows: %w", err)
	}
	for f := lo; f <= hi; f++ {
		if !occupied[f] {
			return f, nil
		}
	}
	return 0, nil // all full
}

// IsModuleSlotOccupied reports whether a module (not a loaded charge) already
// sits at the given slot flag on the ship.
func IsModuleSlotOccupied(ctx context.Context, db *sql.DB, shipID, flag int64) (bool, error) {
	var n int64
	err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM entity e
		  JOIN invTypes t ON t.typeID = e.typeID
		  JOIN invGroups g ON g.groupID = t.groupID
		 WHERE e.locationID = ? AND e.flag = ? AND g.categoryID != 8`,
		shipID, flag).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("IsModuleSlotOccupied: %w", err)
	}
	return n > 0, nil
}

// GetShipHullSlots returns the number of high, medium, and low slots for a
// ship hull type from dgmTypeAttributes (attributeIDs 14, 13, 12).
func GetShipHullSlots(ctx context.Context, db *sql.DB, shipTypeID int64) (hi, med, lo int64, err error) {
	rows, err := db.QueryContext(ctx, `
		SELECT attributeID, COALESCE(NULLIF(valueFloat, 0), CAST(valueInt AS DOUBLE), valueFloat)
		FROM dgmTypeAttributes
		WHERE typeID = ? AND attributeID IN (12, 13, 14)`, shipTypeID)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("GetShipHullSlots: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var attrID int64
		var val float64
		if err := rows.Scan(&attrID, &val); err != nil {
			return 0, 0, 0, fmt.Errorf("GetShipHullSlots scan: %w", err)
		}
		switch attrID {
		case 14:
			hi = int64(val)
		case 13:
			med = int64(val)
		case 12:
			lo = int64(val)
		}
	}
	return hi, med, lo, rows.Err()
}

// GetShipCPUPGCapacity returns the CPU output (attrID 48) and power grid
// output (attrID 11) for a ship hull type from dgmTypeAttributes.
func GetShipCPUPGCapacity(ctx context.Context, db *sql.DB, shipTypeID int64) (cpuOutput, pgOutput float64, err error) {
	rows, err := db.QueryContext(ctx, `
		SELECT attributeID, COALESCE(NULLIF(valueFloat, 0), CAST(valueInt AS DOUBLE), valueFloat)
		FROM dgmTypeAttributes
		WHERE typeID = ? AND attributeID IN (11, 48)`, shipTypeID)
	if err != nil {
		return 0, 0, fmt.Errorf("GetShipCPUPGCapacity: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var attrID int64
		var val float64
		if err := rows.Scan(&attrID, &val); err != nil {
			return 0, 0, fmt.Errorf("GetShipCPUPGCapacity scan: %w", err)
		}
		switch attrID {
		case 48:
			cpuOutput = val
		case 11:
			pgOutput = val
		}
	}
	return cpuOutput, pgOutput, rows.Err()
}

// CountFittedTurretsLaunchers counts the number of fitted turret and launcher
// modules on a ship by checking dgmTypeEffects for effectID 42 (turretFitted /
// dogma.EffectTurretFitted) and 40 (launcherFitted / dogma.EffectLauncherFitted).
// Flags 11-34 = module slot range (dogma.FlagLoSlotFirst .. dogma.FlagHiSlotLast).
func CountFittedTurretsLaunchers(ctx context.Context, db *sql.DB, shipID int64) (turrets, launchers int64, err error) {
	rows, err := db.QueryContext(ctx, `
		SELECT dte.effectID, COUNT(*)
		FROM entity e
		JOIN dgmTypeEffects dte ON dte.typeID = e.typeID AND dte.effectID IN (40, 42)
		JOIN invTypes  t2 ON t2.typeID  = e.typeID
		JOIN invGroups g2 ON g2.groupID = t2.groupID
		WHERE e.locationID = ? AND e.flag BETWEEN 11 AND 34
		  AND g2.categoryID != 8
		GROUP BY dte.effectID`, shipID)
	if err != nil {
		return 0, 0, fmt.Errorf("CountFittedTurretsLaunchers: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var effID, cnt int64
		if err := rows.Scan(&effID, &cnt); err != nil {
			return 0, 0, fmt.Errorf("CountFittedTurretsLaunchers scan: %w", err)
		}
		switch effID {
		case 42:
			turrets = cnt
		case 40:
			launchers = cnt
		}
	}
	return turrets, launchers, rows.Err()
}

// IsModuleTurretOrLauncher checks if a module typeID uses a turret hardpoint
// (effectID 42) or launcher hardpoint (effectID 40).
func IsModuleTurretOrLauncher(ctx context.Context, db *sql.DB, typeID int64) (isTurret, isLauncher bool, err error) {
	rows, err := db.QueryContext(ctx, `
		SELECT effectID FROM dgmTypeEffects
		WHERE typeID = ? AND effectID IN (40, 42)`, typeID)
	if err != nil {
		return false, false, fmt.Errorf("IsModuleTurretOrLauncher: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var effID int64
		if err := rows.Scan(&effID); err != nil {
			return false, false, fmt.Errorf("IsModuleTurretOrLauncher scan: %w", err)
		}
		switch effID {
		case 42:
			isTurret = true
		case 40:
			isLauncher = true
		}
	}
	return isTurret, isLauncher, rows.Err()
}

// EffectInfo holds data from dgmEffects for a single effect looked up by name.
type EffectInfo struct {
	EffectID            int64
	Guid                string
	IsOffensive         bool
	DurationAttributeID int64
	// WarpSafe is dgmEffects.isWarpSafe: the effect keeps running through a warp. Set on the
	// hardeners, damage controls, repairers, capacitor and sensor boosters, tracking computers
	// and the covert ops cloak; clear on weapons, EWAR, mining, propulsion and the rest. Only
	// GetAllEffects fills it.
	WarpSafe bool

	// Chance-based effect metadata (client EffectRepeater contract,
	// DOGMA_MATH.md §8): per-cycle success rolls against range and/or
	// attacker-vs-defender sensor strengths.
	RangeChance        bool
	ElectronicChance   bool
	PropulsionChance   bool
	RangeAttributeID   int64
	FalloffAttributeID int64
}

// IsChanceBased reports whether the effect rolls per-cycle success.
func (ei EffectInfo) IsChanceBased() bool {
	return ei.RangeChance || ei.ElectronicChance || ei.PropulsionChance
}

// GetEffectByName looks up an effect by effectName in the dgmEffects table.
func GetEffectByName(ctx context.Context, db *sql.DB, effectName string) (*EffectInfo, error) {
	var ei EffectInfo
	var isOff int64
	err := db.QueryRowContext(ctx, `
		SELECT effectID, COALESCE(guid,''), COALESCE(isOffensive,0), COALESCE(durationAttributeID,0)
		FROM dgmEffects WHERE effectName = ?`, effectName).Scan(&ei.EffectID, &ei.Guid, &isOff, &ei.DurationAttributeID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetEffectByName: %w", err)
	}
	ei.IsOffensive = isOff != 0
	applyEffectOverrides(effectName, &ei)
	return &ei, nil
}

// EnsureCloneColumn adds the cloneTypeID column to character_ if it does not
// already exist.  MariaDB supports ADD COLUMN IF NOT EXISTS natively.
func EnsureCloneColumn(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		ALTER TABLE character_ ADD COLUMN IF NOT EXISTS cloneTypeID INT NOT NULL DEFAULT 164`)
	return err
}

// GetCloneTypeID returns the clone typeID for the given character.
func GetCloneTypeID(ctx context.Context, db *sql.DB, charID int64) (int64, error) {
	var typeID int64
	err := db.QueryRowContext(ctx,
		`SELECT cloneTypeID FROM character_ WHERE characterID = ?`, charID).Scan(&typeID)
	if err != nil {
		return 0, fmt.Errorf("GetCloneTypeID: %w", err)
	}
	return typeID, nil
}

// SetCloneTypeID updates the clone typeID for the given character.
func SetCloneTypeID(ctx context.Context, db *sql.DB, charID, typeID int64) error {
	_, err := db.ExecContext(ctx,
		`UPDATE character_ SET cloneTypeID = ? WHERE characterID = ?`,
		typeID, charID)
	if err != nil {
		return fmt.Errorf("SetCloneTypeID: %w", err)
	}
	return nil
}

// GetCloneBasePrice returns the basePrice from invTypes for the given typeID.
func GetCloneBasePrice(ctx context.Context, db *sql.DB, typeID int64) (float64, error) {
	var price float64
	err := db.QueryRowContext(ctx,
		`SELECT basePrice FROM invTypes WHERE typeID = ?`, typeID).Scan(&price)
	if err != nil {
		return 0, fmt.Errorf("GetCloneBasePrice: %w", err)
	}
	return price, nil
}

// GetStationTypeID returns the typeID of a station from staStations.
func GetStationTypeID(ctx context.Context, db *sql.DB, stationID int64) (int64, error) {
	var typeID int64
	err := db.QueryRowContext(ctx,
		`SELECT stationTypeID FROM staStations WHERE stationID = ?`, stationID).Scan(&typeID)
	if err != nil {
		return 0, fmt.Errorf("GetStationTypeID: %w", err)
	}
	return typeID, nil
}

// ---------------------------------------------------------------------------
// Charge / ammo helper functions
// ---------------------------------------------------------------------------

// GetChargeGroupsForModule returns the charge groupIDs a module accepts, from the
// chargeGroup1-5 attributes (604, 605, 606, 609, 610). Cached per module type:
// the answer is seed data, and ValidateChargeForModule calls this per
// (module, charge) pair.
func GetChargeGroupsForModule(ctx context.Context, db *sql.DB, moduleTypeID int64) ([]int64, error) {
	if v, ok := chargeGroupCache.Load(moduleTypeID); ok {
		cached := v
		// A copy: the caller gets a slice it may append to or sort.
		out := make([]int64, len(cached))
		copy(out, cached)
		return out, nil
	}
	groups, err := getChargeGroupsForModuleUncached(ctx, db, moduleTypeID)
	if err != nil {
		return nil, err
	}
	stored := make([]int64, len(groups))
	copy(stored, groups)
	chargeGroupCache.Store(moduleTypeID, stored)
	return groups, nil
}

// Static reference caches. invTypes/invGroups/dgmTypeAttributes are seed data the
// server never writes, so no TTL; an ESP edit of an item type calls InvalidateTypeInfo.
var (
	chargeGroupCache syncx.Map[int64, []int64] // moduleTypeID -> []int64
	groupCatCache    syncx.Map[int64, [2]int64] // typeID -> [2]int64{groupID, categoryID}
	chargeSizeCache  syncx.Map[int64, int64] // typeID -> int64
)

// InvalidateTypeInfo drops the static item-type caches. For an admin edit to
// invTypes, invGroups or dgmTypeAttributes; nothing in normal play needs it.
func InvalidateTypeInfo() {
	chargeGroupCache.Range(func(k int64, _ []int64) bool { chargeGroupCache.Delete(k); return true })
	groupCatCache.Range(func(k int64, _ [2]int64) bool { groupCatCache.Delete(k); return true })
	chargeSizeCache.Range(func(k int64, _ int64) bool { chargeSizeCache.Delete(k); return true })
}

func getChargeGroupsForModuleUncached(ctx context.Context, db *sql.DB, moduleTypeID int64) ([]int64, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT CAST(COALESCE(NULLIF(valueFloat,0), valueInt) AS SIGNED)
		FROM dgmTypeAttributes
		WHERE typeID = ? AND attributeID IN (604,605,606,609,610)`, moduleTypeID)
	if err != nil {
		return nil, fmt.Errorf("GetChargeGroupsForModule: %w", err)
	}
	defer rows.Close()
	var groups []int64
	for rows.Next() {
		var g int64
		if err := rows.Scan(&g); err != nil {
			return nil, fmt.Errorf("GetChargeGroupsForModule scan: %w", err)
		}
		if g != 0 {
			groups = append(groups, g)
		}
	}
	return groups, rows.Err()
}

// GetChargeSize returns the chargeSize attribute (128) for a typeID, 0 when
// unset. Cached like GetChargeGroupsForModule: seed data, and
// ValidateChargeForModule reads it twice per candidate charge.
func GetChargeSize(ctx context.Context, db *sql.DB, typeID int64) (int64, error) {
	if v, ok := chargeSizeCache.Load(typeID); ok {
		return v, nil
	}
	size, err := getChargeSizeUncached(ctx, db, typeID)
	if err != nil {
		return 0, err
	}
	chargeSizeCache.Store(typeID, size)
	return size, nil
}

func getChargeSizeUncached(ctx context.Context, db *sql.DB, typeID int64) (int64, error) {
	var size sql.NullFloat64
	err := db.QueryRowContext(ctx, `
		SELECT COALESCE(NULLIF(valueFloat,0), valueInt)
		FROM dgmTypeAttributes
		WHERE typeID = ? AND attributeID = 128`, typeID).Scan(&size)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("GetChargeSize: %w", err)
	}
	if !size.Valid {
		return 0, nil
	}
	return int64(size.Float64), nil
}

// GetModuleCapacity returns the volume capacity of a module (from invTypes.capacity).
func GetModuleCapacity(ctx context.Context, db *sql.DB, moduleTypeID int64) (float64, error) {
	var cap float64
	err := db.QueryRowContext(ctx,
		`SELECT capacity FROM invTypes WHERE typeID = ?`, moduleTypeID).Scan(&cap)
	if err != nil {
		return 0, fmt.Errorf("GetModuleCapacity: %w", err)
	}
	return cap, nil
}

// LoadedCharge holds info about a charge loaded in a module.
type LoadedCharge struct {
	ItemID   int64
	TypeID   int64
	Quantity int64
}

// GetLoadedCharge returns the charge entity loaded in a module slot (locationID=shipID, flag=slotFlag, categoryID=8).
// Returns nil, nil if no charge is loaded.
func GetLoadedCharge(ctx context.Context, db *sql.DB, shipID, slotFlag int64) (*LoadedCharge, error) {
	var lc LoadedCharge
	err := db.QueryRowContext(ctx, `
		SELECT e.itemID, e.typeID, e.quantity FROM entity e
		JOIN invTypes  t ON t.typeID  = e.typeID
		JOIN invGroups g ON g.groupID = t.groupID
		WHERE e.locationID = ? AND e.flag = ? AND g.categoryID = 8`,
		shipID, slotFlag).Scan(&lc.ItemID, &lc.TypeID, &lc.Quantity)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetLoadedCharge: %w", err)
	}
	return &lc, nil
}

// LoadChargeIntoModule sets a charge entity's location to the ship at the
// module's slot flag.  The EVE client expects charges to share the same
// locationID (shipID) and flag (slot) as their parent module, distinguished
// by categoryID=8 (charge).
func LoadChargeIntoModule(ctx context.Context, db *sql.DB, chargeItemID, shipID, slotFlag, qty int64) error {
	_, err := db.ExecContext(ctx,
		`UPDATE entity SET locationID=?, flag=?, quantity=? WHERE itemID=?`,
		shipID, slotFlag, qty, chargeItemID)
	if err != nil {
		return fmt.Errorf("LoadChargeIntoModule: %w", err)
	}
	return nil
}

// ModuleChargeUnits is how many units of chargeTypeID fill one module of
// moduleTypeID: floor(module capacity / charge volume), never below 1 (a laser
// holds one crystal). Every path that puts rounds into a slot clamps to this.
// The epsilon keeps an exact ratio from flooring one short (0.5 / 0.0025 = 200).
func ModuleChargeUnits(ctx context.Context, db *sql.DB, moduleTypeID, chargeTypeID int64) (int64, error) {
	modCap, err := GetModuleCapacity(ctx, db, moduleTypeID)
	if err != nil {
		return 0, err
	}
	chargeVol, err := GetTypeVolume(ctx, db, chargeTypeID)
	if err != nil {
		return 0, err
	}
	units := int64(1)
	if chargeVol > 0 {
		units = int64(modCap/chargeVol + 1e-9)
	}
	if units < 1 {
		units = 1
	}
	return units, nil
}

// RepairOverloadedCharges trims every loaded charge stack above its module's
// ModuleChargeUnits to a full load and moves the excess to the ship's cargo
// hold (merged into a loose stack of the same type and owner when one exists).
// Run at boot, before a client can cache the old number. Returns stacks fixed.
func RepairOverloadedCharges(ctx context.Context, db *sql.DB) (int, error) {
	type over struct {
		itemID, typeID, ownerID, shipID, qty, full int64
		typeName                                 string
	}
	rows, err := db.QueryContext(ctx, `
		SELECT c.itemID, c.typeID, c.ownerID, c.locationID, c.quantity, ct.typeName,
		       GREATEST(1, FLOOR(COALESCE(mt.capacity, 0) / ct.volume + 1e-9)) AS full
		FROM entity c
		JOIN invTypes  ct ON ct.typeID  = c.typeID
		JOIN invGroups cg ON cg.groupID = ct.groupID AND cg.categoryID = 8
		JOIN entity    m  ON m.locationID = c.locationID AND m.flag = c.flag AND m.itemID <> c.itemID
		JOIN invTypes  mt ON mt.typeID  = m.typeID
		JOIN invGroups mg ON mg.groupID = mt.groupID AND mg.categoryID <> 8
		WHERE c.flag BETWEEN 11 AND 34 AND ct.volume > 0
		  AND c.quantity > GREATEST(1, FLOOR(COALESCE(mt.capacity, 0) / ct.volume + 1e-9))`)
	if err != nil {
		return 0, fmt.Errorf("RepairOverloadedCharges query: %w", err)
	}
	var list []over
	seen := map[int64]bool{} // a slot holding two modules would list its charge twice
	for rows.Next() {
		var o over
		var full float64
		if err := rows.Scan(&o.itemID, &o.typeID, &o.ownerID, &o.shipID, &o.qty, &o.typeName, &full); err != nil {
			rows.Close()
			return 0, fmt.Errorf("RepairOverloadedCharges scan: %w", err)
		}
		o.full = int64(full)
		if seen[o.itemID] {
			continue
		}
		seen[o.itemID] = true
		list = append(list, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("RepairOverloadedCharges rows: %w", err)
	}

	fixed := 0
	for _, o := range list {
		excess := o.qty - o.full
		if excess <= 0 {
			continue
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fixed, fmt.Errorf("RepairOverloadedCharges begin: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE entity SET quantity = ? WHERE itemID = ?`, o.full, o.itemID); err != nil {
			tx.Rollback()
			return fixed, fmt.Errorf("RepairOverloadedCharges trim %d: %w", o.itemID, err)
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE entity SET quantity = quantity + ?
			WHERE locationID = ? AND flag = 5 AND typeID = ? AND ownerID = ? AND singleton = 0
			ORDER BY itemID LIMIT 1`,
			excess, o.shipID, o.typeID, o.ownerID)
		if err != nil {
			tx.Rollback()
			return fixed, fmt.Errorf("RepairOverloadedCharges merge %d: %w", o.itemID, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO entity
				  (itemName, typeID, ownerID, locationID, flag, contraband, singleton, quantity, x, y, z, customInfo)
				VALUES (?, ?, ?, ?, 5, 0, 0, ?, 0, 0, 0, NULL)`,
				o.typeName, o.typeID, o.ownerID, o.shipID, excess); err != nil {
				tx.Rollback()
				return fixed, fmt.Errorf("RepairOverloadedCharges spill %d: %w", o.itemID, err)
			}
		}
		if err := tx.Commit(); err != nil {
			return fixed, fmt.Errorf("RepairOverloadedCharges commit: %w", err)
		}
		fixed++
	}
	return fixed, nil
}

// UnloadChargeToCargoOrHangar moves a charge entity to cargo (flag=5) or hangar (flag=4).
func UnloadChargeToCargoOrHangar(ctx context.Context, db *sql.DB, chargeItemID, destLocationID int64, destFlag int) error {
	_, err := db.ExecContext(ctx,
		`UPDATE entity SET locationID=?, flag=?, singleton=0 WHERE itemID=?`,
		destLocationID, destFlag, chargeItemID)
	if err != nil {
		return fmt.Errorf("UnloadChargeToCargoOrHangar: %w", err)
	}
	return nil
}

// GetGroupAndCategory returns the groupID and categoryID for a typeID. Cached
// like GetChargeGroupsForModule: seed data, called per item in the fit, loot
// and market loops; the cache is bounded by the invTypes row count.
func GetGroupAndCategory(ctx context.Context, db *sql.DB, typeID int64) (groupID, categoryID int64, err error) {
	if v, ok := groupCatCache.Load(typeID); ok {
		p := v
		return p[0], p[1], nil
	}
	groupID, categoryID, err = getGroupAndCategoryUncached(ctx, db, typeID)
	if err != nil {
		return 0, 0, err
	}
	groupCatCache.Store(typeID, [2]int64{groupID, categoryID})
	return groupID, categoryID, nil
}

func getGroupAndCategoryUncached(ctx context.Context, db *sql.DB, typeID int64) (groupID, categoryID int64, err error) {
	err = db.QueryRowContext(ctx, `
		SELECT t.groupID, g.categoryID
		FROM invTypes t
		JOIN invGroups g ON g.groupID = t.groupID
		WHERE t.typeID = ?`, typeID).Scan(&groupID, &categoryID)
	if err != nil {
		return 0, 0, fmt.Errorf("GetGroupAndCategory: %w", err)
	}
	return groupID, categoryID, nil
}

// ValidateChargeForModule checks that a charge typeID is compatible with a
// module typeID: correct category (8=charge), matching chargeGroup, and
// matching chargeSize.  Returns nil if valid, descriptive error otherwise.
func ValidateChargeForModule(ctx context.Context, db *sql.DB, chargeTypeID, moduleTypeID int64) error {
	chargeGroupID, catID, err := GetGroupAndCategory(ctx, db, chargeTypeID)
	if err != nil {
		return fmt.Errorf("charge lookup: %w", err)
	}
	if catID != 8 {
		return fmt.Errorf("item is not a charge (categoryID=%d)", catID)
	}
	acceptedGroups, err := GetChargeGroupsForModule(ctx, db, moduleTypeID)
	if err != nil {
		return fmt.Errorf("module charge groups: %w", err)
	}
	// A module with no chargeGroup1..5 (attrs 604/605/606/609/610) takes no ammo;
	// an empty list is never "accepts anything" (cfg.IsChargeCompatible gates the same way).
	if len(acceptedGroups) == 0 {
		return fmt.Errorf("module type %d accepts no charges", moduleTypeID)
	}
	found := false
	for _, g := range acceptedGroups {
		if g == chargeGroupID {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("charge group %d not accepted by module (accepts %v)", chargeGroupID, acceptedGroups)
	}
	modChargeSize, err := GetChargeSize(ctx, db, moduleTypeID)
	if err != nil {
		return fmt.Errorf("module charge size: %w", err)
	}
	if modChargeSize > 0 {
		chargeSize, err := GetChargeSize(ctx, db, chargeTypeID)
		if err != nil {
			return fmt.Errorf("charge size: %w", err)
		}
		if chargeSize != modChargeSize {
			return fmt.Errorf("charge size %d does not match module size %d", chargeSize, modChargeSize)
		}
	}
	return nil
}

// FindModuleInSlot finds the module entity fitted in a specific slot flag on a ship.
// Charges (categoryID=8) share the same locationID+flag as their parent module,
// so we exclude them to avoid returning the charge instead of the module.
func FindModuleInSlot(ctx context.Context, db *sql.DB, shipID, flag int64) (*EntityRow, error) {
	var er EntityRow
	err := db.QueryRowContext(ctx, `
		SELECT e.itemID, e.typeID, e.ownerID, e.locationID, e.flag,
		       t.groupID, g.categoryID, e.singleton, e.quantity
		FROM entity e
		JOIN invTypes  t ON t.typeID  = e.typeID
		JOIN invGroups g ON g.groupID = t.groupID
		WHERE e.locationID = ? AND e.flag = ? AND g.categoryID != 8`, shipID, flag).Scan(
		&er.ItemID, &er.TypeID, &er.OwnerID, &er.LocationID, &er.Flag,
		&er.GroupID, &er.CategoryID, &er.Singleton, &er.Quantity)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("FindModuleInSlot: %w", err)
	}
	return &er, nil
}

// DeleteEntity removes an entity row by itemID.
func DeleteEntity(ctx context.Context, db DBTX, itemID int64) error {
	_, err := db.ExecContext(ctx, `DELETE FROM entity WHERE itemID = ?`, itemID)
	if err != nil {
		return fmt.Errorf("DeleteEntity: %w", err)
	}
	return nil
}

// ── Home station & capsule helpers (death loop) ─────────────────────────────

// EnsureHomeStationColumn adds character_.homeStationID if missing. Default 0
// means not yet set: the first dock fills it for existing characters, and
// CreateCharacter writes the school station.
func EnsureHomeStationColumn(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		ALTER TABLE character_
		ADD COLUMN IF NOT EXISTS homeStationID BIGINT NOT NULL DEFAULT 0`)
	if err != nil {
		return fmt.Errorf("EnsureHomeStationColumn: %w", err)
	}
	return nil
}

// GetHomeStationID returns the character's medical clone station (respawn
// location on pod death). Returns 0 if not yet set.
func GetHomeStationID(ctx context.Context, db *sql.DB, charID int64) (int64, error) {
	var id int64
	err := db.QueryRowContext(ctx,
		`SELECT homeStationID FROM character_ WHERE characterID = ?`, charID).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("GetHomeStationID: %w", err)
	}
	return id, nil
}

// SetHomeStationID updates the character's medical clone station.
func SetHomeStationID(ctx context.Context, db *sql.DB, charID, stationID int64) error {
	_, err := db.ExecContext(ctx,
		`UPDATE character_ SET homeStationID = ? WHERE characterID = ?`,
		stationID, charID)
	if err != nil {
		return fmt.Errorf("SetHomeStationID: %w", err)
	}
	return nil
}

// GetCharacterCapsuleID finds the character's capsule entity (typeID 670).
// Returns 0 if no capsule exists.
func GetCharacterCapsuleID(ctx context.Context, db *sql.DB, charID int64) (int64, error) {
	var itemID int64
	err := db.QueryRowContext(ctx,
		`SELECT itemID FROM entity WHERE ownerID = ? AND typeID = 670 LIMIT 1`, charID).Scan(&itemID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("GetCharacterCapsuleID: %w", err)
	}
	return itemID, nil
}

// CreateCapsuleEntity inserts a new capsule (typeID 670) entity owned by
// charID at the given locationID (station or solar system). Returns the new
// entity's itemID.
func CreateCapsuleEntity(ctx context.Context, db *sql.DB, charID, locationID int64, charName string) (int64, error) {
	res, err := db.ExecContext(ctx, `
		INSERT INTO entity
		  (itemName, typeID, ownerID, locationID, flag, contraband, singleton, quantity, x, y, z, customInfo)
		VALUES (?, 670, ?, ?, 4, 0, 1, 1, 0, 0, 0, NULL)`,
		charName+"'s Capsule", charID, locationID)
	if err != nil {
		return 0, fmt.Errorf("CreateCapsuleEntity: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("CreateCapsuleEntity LastInsertId: %w", err)
	}
	return id, nil
}

// capsuleUnpilotedSQL holds for capsule row c when no character is aboard it. Only a
// character entity (category Owner) counts as a pilot, with or without a character_ row:
// items left inside a pod (rolebots have fitted modules and loaded ore into theirs) are
// contents, not someone flying it.
const capsuleUnpilotedSQL = `NOT EXISTS (
	SELECT 1 FROM entity p
	JOIN invTypes pt ON pt.typeID = p.typeID
	JOIN invGroups pg ON pg.groupID = pt.groupID
	WHERE p.locationID = c.itemID AND pg.categoryID = 1)`

// CleanOrphanCapsules deletes capsule entities (typeID 670) no character is
// aboard (deleteCapsule). Safe to run at boot.
func CleanOrphanCapsules(ctx context.Context, db *sql.DB) (int64, error) {
	return CleanOrphanCapsulesUpTo(ctx, db, 0)
}

// CleanOrphanCapsulesUpTo is CleanOrphanCapsules limited to capsules with itemID <= maxItemID
// (0: no limit). A running server passes the entity high-water mark from its previous sweep:
// a capsule is created a moment before its pilot is moved into it (death, clone jump, a
// module's respawn), and only capsules older than one sweep interval are past that window.
func CleanOrphanCapsulesUpTo(ctx context.Context, db *sql.DB, maxItemID int64) (int64, error) {
	caps, err := selectCapsules(ctx, db, `
		SELECT c.itemID, c.locationID FROM entity c
		WHERE c.typeID = 670 AND (? = 0 OR c.itemID <= ?) AND `+capsuleUnpilotedSQL,
		maxItemID, maxItemID)
	if err != nil {
		return 0, fmt.Errorf("CleanOrphanCapsules: %w", err)
	}
	// One pod that will not delete does not hold up the rest; the first error is reported.
	var n int64
	var firstErr error
	for _, cp := range caps {
		ok, err := deleteCapsule(ctx, db, cp[0], cp[1])
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("CleanOrphanCapsules: %w", err)
			}
			continue
		}
		if ok {
			n++
		}
	}
	return n, firstErr
}

// MaxEntityItemID is the highest itemID in entity: the high-water mark CleanOrphanCapsulesUpTo
// is given one sweep later.
func MaxEntityItemID(ctx context.Context, db *sql.DB) (int64, error) {
	var id sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT MAX(itemID) FROM entity`).Scan(&id); err != nil {
		return 0, fmt.Errorf("MaxEntityItemID: %w", err)
	}
	return id.Int64, nil
}

// DeleteSpareCapsules deletes every capsule charID owns but is not aboard. A capsule is the
// pilot's active ship or nothing: it is never an asset, so one left behind by a board out of
// it, a clone jump or a module path that moved the pilot itself would sit in a hangar as an
// item. Returns the deleted rows, so a caller with a client connection can push their removal.
func DeleteSpareCapsules(ctx context.Context, db *sql.DB, charID int64) ([]*EntityRow, error) {
	if charID == 0 {
		return nil, nil
	}
	caps, err := selectCapsules(ctx, db, `
		SELECT c.itemID, c.locationID FROM entity c
		JOIN entity ch ON ch.itemID = ?
		WHERE c.ownerID = ? AND c.typeID = 670 AND c.itemID <> ch.locationID AND `+capsuleUnpilotedSQL,
		charID, charID)
	if err != nil {
		return nil, fmt.Errorf("DeleteSpareCapsules: %w", err)
	}
	var gone []*EntityRow
	for _, cp := range caps {
		er, _ := GetEntityRow(ctx, db, cp[0])
		ok, err := deleteCapsule(ctx, db, cp[0], cp[1])
		if err != nil {
			return gone, fmt.Errorf("DeleteSpareCapsules: %w", err)
		}
		if ok && er != nil {
			gone = append(gone, er)
		}
	}
	return gone, nil
}

// selectCapsules runs a query returning (itemID, locationID) capsule pairs.
func selectCapsules(ctx context.Context, db *sql.DB, query string, args ...any) ([][2]int64, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][2]int64
	for rows.Next() {
		var cp [2]int64
		if err := rows.Scan(&cp[0], &cp[1]); err != nil {
			return nil, err
		}
		out = append(out, cp)
	}
	return out, rows.Err()
}

// deleteCapsule deletes one unpiloted capsule and its entity_attributes rows. Anything inside
// it goes to the hangar of the station it sits in first, so nothing is lost with the pod. A
// capsule in space with contents is left alone (false): there is no hangar to put them in.
func deleteCapsule(ctx context.Context, db *sql.DB, capsuleID, locationID int64) (bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var contents int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entity WHERE locationID = ?`, capsuleID).Scan(&contents); err != nil {
		return false, fmt.Errorf("capsule %d contents: %w", capsuleID, err)
	}
	if contents > 0 {
		if locationID < stationIDBandFirst || locationID >= stationIDBandPastLast {
			return false, nil
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE entity e
			JOIN invTypes t ON t.typeID = e.typeID
			JOIN invGroups g ON g.groupID = t.groupID
			SET e.locationID = ?, e.flag = 4
			WHERE e.locationID = ? AND g.categoryID <> 1`,
			locationID, capsuleID); err != nil {
			return false, fmt.Errorf("capsule %d unload: %w", capsuleID, err)
		}
		// Anything still aboard (a type the join does not know) would be stranded
		// at a deleted location: keep the pod instead.
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM entity WHERE locationID = ?`, capsuleID).Scan(&contents); err != nil {
			return false, fmt.Errorf("capsule %d contents: %w", capsuleID, err)
		}
		if contents > 0 {
			return false, nil
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM entity_attributes WHERE itemID = ?`, capsuleID); err != nil {
		return false, fmt.Errorf("capsule %d attributes: %w", capsuleID, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM entity WHERE itemID = ?`, capsuleID); err != nil {
		return false, fmt.Errorf("capsule %d: %w", capsuleID, err)
	}
	return true, tx.Commit()
}

// MoveCharacterIntoShip updates the character entity's locationID to point
// at the given shipID (or capsuleID). This is how the game tracks which
// ship a character is currently aboard. Any capsule the pilot owns and is no
// longer aboard is deleted (DeleteSpareCapsules); that cleanup is best-effort,
// the periodic orphan sweep catches what it misses.
func MoveCharacterIntoShip(ctx context.Context, db *sql.DB, charID, shipID int64) error {
	_, err := db.ExecContext(ctx,
		`UPDATE entity SET locationID = ? WHERE itemID = ?`, shipID, charID)
	if err != nil {
		return fmt.Errorf("MoveCharacterIntoShip: %w", err)
	}
	if _, err := DeleteSpareCapsules(ctx, db, charID); err != nil {
		log.Printf("MoveCharacterIntoShip charID=%d: %v", charID, err)
	}
	return nil
}

// raceToEmpireFaction maps a character raceID to their empire's NPC factionID.
// raceJove is chrRaces.raceID for the Jove; skillSpaceshipCommand is the
// Spaceship Command skill every character is created with.
const (
	raceJove              = 16
	skillSpaceshipCommand = 3327
)

func raceToEmpireFaction(raceID int64) int64 {
	switch raceID {
	case 1:
		return 500001 // Caldari State
	case 2:
		return 500002 // Minmatar Republic
	case 4:
		return 500003 // Amarr Empire
	case 8:
		return 500004 // Gallente Federation
	default:
		return 0
	}
}
