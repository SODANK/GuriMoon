package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"

	"gurimoon/marshal"
)

// ChtModeSpeaker is the client's CHTMODE_SPEAKER access level. An ACL row at this mode or
// above on a character's own channel makes an owner a contact in People & Places
// (lscengine.GetBuddies).
const ChtModeSpeaker int64 = 2

// CharInfo holds the character details needed to compose LSC sender info.
type CharInfo struct {
	CharID   int64
	CharName string
	CorpID   int64
	TypeID   int64 // character typeID (bloodline specific)
	Role     int64 // account role bitmask
}

// GetCharInfo loads characterName, corporationID, typeID and account role. The error wraps
// sql.ErrNoRows if the character does not exist.
func GetCharInfo(ctx context.Context, db *sql.DB, charID int64) (*CharInfo, error) {
	row := db.QueryRowContext(ctx,
		`SELECT c.characterName, c.corporationID, c.typeID, COALESCE(a.role, 3)
		 FROM character_ c
		 LEFT JOIN account a ON c.accountID = a.accountID
		 WHERE c.characterID=?`, charID)
	ci := &CharInfo{CharID: charID}
	if err := row.Scan(&ci.CharName, &ci.CorpID, &ci.TypeID, &ci.Role); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("GetCharInfo(%d): %w", charID, err)
		}
		return nil, fmt.Errorf("GetCharInfo(%d): %w", charID, err)
	}
	return ci, nil
}

// ResolveChannelDisplayName returns the display name for a channel: "Local" for a solar
// system, "Corp" for a corporation, else the numeric ID. Corp channels go on the wire as a bare
// int, so the client prints this name verbatim. Table membership, not an ID range, makes a
// channel a corp channel. See docs/db-5-notes.md#lsc-channel-rows.
func ResolveChannelDisplayName(ctx context.Context, db *sql.DB, channelID int64) string {
	switch {
	case channelID >= 30000000 && channelID <= 31999999:
		return "Local"
	case channelID >= 1000000 && channelID <= 2999999:
		return "Corp"
	}
	if IsCorporationID(ctx, db, channelID) {
		return "Corp"
	}
	if IsAllianceID(ctx, db, channelID) {
		return "Alliance"
	}
	return fmt.Sprintf("%d", channelID)
}

// IsAllianceID reports whether channelID names a live row in the alliances table. Alliances
// and player corporations share one owner-ID counter, so only table membership tells them apart.
func IsAllianceID(ctx context.Context, db *sql.DB, id int64) bool {
	if id < MinPlayerEntityID {
		return false
	}
	var n int64
	if err := db.QueryRowContext(ctx,
		`SELECT 1 FROM alliances WHERE allianceID = ? AND deleted = 0`, id).Scan(&n); err != nil {
		return false
	}
	return true
}

// IsOwnerChannelID reports whether a channel ID is a corporation's or an alliance's own chat
// channel. An owner channel can sit above MinPlayerEntityID, inside the private-conversation
// band, so only table membership identifies it; a wrong answer renders as "Private Chat
// (alone)". See docs/db-5-notes.md#lsc-channel-rows.
func IsOwnerChannelID(ctx context.Context, db *sql.DB, id int64) bool {
	if id < MinPlayerEntityID {
		return false
	}
	return IsCorporationID(ctx, db, id) || IsAllianceID(ctx, db, id)
}

// IsCorporationID reports whether channelID names a row in the corporation table; it tells a
// player corp's chat channel from a character's own channel, which share the ID band.
func IsCorporationID(ctx context.Context, db *sql.DB, id int64) bool {
	if id == 0 {
		return false
	}
	var n int64
	if err := db.QueryRowContext(ctx,
		`SELECT 1 FROM corporation WHERE corporationID=?`, id).Scan(&n); err != nil {
		return false
	}
	return true
}

// EnsureChannel inserts a channel row if it does not exist, to auto-create corp and
// solar-system channels on demand. channelID and ownerID are equal for those. displayName must
// not be empty (the client calls .split() on it). memberless = 0 so the roster renders.
func EnsureChannel(ctx context.Context, db *sql.DB, channelID, ownerID int64, displayName string) error {
	// Upsert, not INSERT IGNORE: the update renames only a row still named after its
	// own ID, so a player-named channel is never renamed.
	_, err := db.ExecContext(ctx, `
		INSERT INTO channels
		  (channelID, ownerID, displayName, motd, comparisonKey,
		   memberless, password, mailingList, cspa, temporary, mode,
		   subscribed, estimatedMemberCount)
		VALUES (?, ?, ?, NULL, NULL, 0, NULL, 0, 0, 0, 3, 1, 0)
		ON DUPLICATE KEY UPDATE
		  displayName = IF(channels.displayName = CAST(channels.channelID AS CHAR),
		                   VALUES(displayName), channels.displayName)`,
		channelID, ownerID, displayName)
	if err != nil {
		return fmt.Errorf("EnsureChannel(%d): %w", channelID, err)
	}
	return nil
}

// UpsertChannelChar inserts or ignores a character's membership in a channel so
// QueryChannelChars returns them. allianceID is 0 and needs its sentinel row in
// alliance_shortnames.
func UpsertChannelChar(ctx context.Context, db *sql.DB, channelID, charID, corpID int64) error {
	_, err := db.ExecContext(ctx, `
		INSERT IGNORE INTO channelChars (channelID, charID, corpID, allianceID, role, extra)
		VALUES (?, ?, ?, 0, 0, 0)`,
		channelID, charID, corpID)
	if err != nil {
		return fmt.Errorf("UpsertChannelChar(%d,%d): %w", channelID, charID, err)
	}
	return nil
}

// DeleteChannelChar removes a character from one channel's membership (LeaveChannel).
func DeleteChannelChar(ctx context.Context, db *sql.DB, channelID, charID int64) error {
	_, err := db.ExecContext(ctx,
		`DELETE FROM channelChars WHERE channelID=? AND charID=?`,
		channelID, charID)
	if err != nil {
		return fmt.Errorf("DeleteChannelChar(%d,%d): %w", channelID, charID, err)
	}
	return nil
}

// DeleteAllChannelChars removes all channel memberships for a character, on disconnect.
func DeleteAllChannelChars(ctx context.Context, db *sql.DB, charID int64) error {
	_, err := db.ExecContext(ctx,
		`DELETE FROM channelChars WHERE charID=?`, charID)
	if err != nil {
		return fmt.Errorf("DeleteAllChannelChars(%d): %w", charID, err)
	}
	return nil
}

// lscChannelCols matches the GetChannels rowset header in LSCService.cpp.
var lscChannelCols = []string{
	"channelID", "ownerID", "displayName", "motd",
	"comparisonKey", "memberless", "password", "mailingList",
	"cspa", "temporary", "mode", "subscribed", "estimatedMemberCount",
}

// GetLSCChannels returns a util.Rowset with the 13-column LSCService::Handle_GetChannels
// header: the character's corp channel, alliance channel, solar-system channel and own private
// channel, synthesized when the DB has no row.
func GetLSCChannels(ctx context.Context, db *sql.DB, charID int64) (marshal.PyRep, error) {
	if charID == 0 {
		return makeRowset(lscChannelCols, nil), nil
	}

	// The character's corp, alliance and current solar system.
	var corpID, solarSystemID int64
	var allianceID sql.NullInt64
	if err := db.QueryRowContext(ctx,
		`SELECT c.corporationID, c.solarSystemID, co.allianceID
		   FROM character_ c
		   LEFT JOIN corporation co ON co.corporationID = c.corporationID
		  WHERE c.characterID=?`,
		charID).Scan(&corpID, &solarSystemID, &allianceID); err != nil {
		return nil, fmt.Errorf("GetLSCChannels: lookup char %d: %w", charID, err)
	}

	// The channel IDs the client needs. The alliance channel rides beside the corp one.
	want := make(map[int64]bool)
	want[charID] = true
	if corpID != 0 {
		want[corpID] = true
	}
	if allianceID.Int64 != 0 {
		want[allianceID.Int64] = true
	}
	if solarSystemID != 0 {
		want[solarSystemID] = true
	}

	// Query what the DB has for these channels.
	rows, err := db.QueryContext(ctx, `
		SELECT channelID, ownerID, displayName, motd,
		       comparisonKey, memberless, password, mailingList,
		       cspa, mode
		FROM channels
		WHERE channelID IN (?, ?, ?, ?)`, corpID, solarSystemID, charID, allianceID.Int64)
	if err != nil {
		return nil, fmt.Errorf("GetLSCChannels: %w", err)
	}
	defer rows.Close()

	found := make(map[int64]bool)
	var result [][]marshal.PyRep
	for rows.Next() {
		var (
			channelID   int64
			ownerID     int64
			displayName sql.NullString
			motd        sql.NullString
			compKey     sql.NullString
			memberless  int64
			password    sql.NullString
			mailingList int64
			cspa        int64
			mode        int64
		)
		if err := rows.Scan(
			&channelID, &ownerID, &displayName, &motd,
			&compKey, &memberless, &password, &mailingList,
			&cspa, &mode,
		); err != nil {
			return nil, fmt.Errorf("GetLSCChannels scan: %w", err)
		}
		found[channelID] = true
		// Override displayName for system, corp and alliance channels. The corp channel is the
		// caller's own corpID, so this is right for player corps too.
		if channelID >= 30000000 && channelID <= 31999999 {
			displayName.String = "Local"
			displayName.Valid = true
		} else if channelID == corpID || (channelID >= 1000000 && channelID <= 2999999) {
			displayName.String = "Corp"
			displayName.Valid = true
		} else if channelID != 0 && channelID == allianceID.Int64 {
			displayName.String = "Alliance"
			displayName.Valid = true
		}
		row := []marshal.PyRep{
			intRep(channelID), intRep(ownerID), nullStrRep(displayName), nullStrRep(motd),
			nullStrRep(compKey), intRep(memberless), nullStrRep(password), intRep(mailingList),
			intRep(cspa),
			marshal.PyInt{Value: 0}, // temporary: never stored, always false
			intRep(mode),
			marshal.PyInt{Value: 1}, // subscribed: always true
			marshal.PyInt{Value: 0}, // estimatedMemberCount: unknown
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("GetLSCChannels rows: %w", err)
	}
	// Close rows before running more queries so the connection returns to the pool.
	rows.Close()

	// Synthesize rows for channels not found in the DB (corp or system channels never created).
	for id := range want {
		if found[id] {
			continue
		}
		name := ResolveChannelDisplayName(ctx, db, id)
		log.Printf("GetLSCChannels: synthesizing channel %d displayName=%q (not in DB)", id, name)
		row := []marshal.PyRep{
			intRep(id), intRep(id),
			strRep(name),            // displayName
			marshal.PyNone{},        // motd
			marshal.PyNone{},        // comparisonKey
			marshal.PyInt{Value: 0}, // memberless: immediate mode so the roster renders
			marshal.PyNone{},        // password
			marshal.PyInt{Value: 0}, // mailingList
			marshal.PyInt{Value: 0}, // cspa
			marshal.PyInt{Value: 0}, // temporary
			marshal.PyInt{Value: 3}, // mode (default)
			marshal.PyInt{Value: 1}, // subscribed
			marshal.PyInt{Value: 0}, // estimatedMemberCount
		}
		result = append(result, row)
	}

	// Corp and alliance mailboxes: a mailingList row per owner, listed before the chat rows
	// (the client indexes this rowset by channelID, last row wins).
	// See docs/db-5-notes.md#lsc-channel-rows.
	var mailboxes [][]marshal.PyRep
	for _, id := range []int64{corpID, allianceID.Int64} {
		if id == 0 {
			continue
		}
		mailboxes = append(mailboxes, []marshal.PyRep{
			intRep(id), intRep(id),
			strRep(ResolveChannelDisplayName(ctx, db, id)), // displayName
			marshal.PyNone{},        // motd
			marshal.PyNone{},        // comparisonKey
			marshal.PyInt{Value: 1}, // memberless
			marshal.PyNone{},        // password
			marshal.PyInt{Value: 1}, // mailingList
			marshal.PyInt{Value: 0}, // cspa
			marshal.PyInt{Value: 0}, // temporary
			marshal.PyInt{Value: 3}, // mode
			marshal.PyInt{Value: 1}, // subscribed
			marshal.PyInt{Value: 0}, // estimatedMemberCount
		})
	}

	return makeRowset(lscChannelCols, append(mailboxes, result...)), nil
}

// QueryChannelInfo returns a util.Row for one channel, matching LSCDB::QueryChannelInfo
// (11 columns from the channels table).
func QueryChannelInfo(ctx context.Context, db *sql.DB, channelID int64) (marshal.PyRep, error) {
	// Column order matches LSCDB::QueryChannelInfo (11 columns, no 'mode'); the client may read
	// them positionally.
	row := db.QueryRowContext(ctx, `
		SELECT channelID, ownerID, displayName, motd,
		       comparisonKey, memberless, password,
		       mailingList, cspa, 0 AS temporary, 1 AS subscribed
		FROM channels WHERE channelID=?`, channelID)

	var (
		chID        int64
		ownerID     int64
		displayName sql.NullString
		motd        sql.NullString
		compKey     sql.NullString
		memberless  int64
		password    sql.NullString
		mailingList int64
		cspa        int64
		temporary   int64
		subscribed  int64
	)
	if err := row.Scan(&chID, &ownerID, &displayName, &motd,
		&compKey, &memberless, &password, &mailingList,
		&cspa, &temporary, &subscribed); err != nil {
		return nil, fmt.Errorf("QueryChannelInfo(%d): %w", channelID, err)
	}

	// Override displayName for system and corp channels: "Local" / "Corp".
	if channelID >= 30000000 && channelID <= 31999999 {
		displayName.String = "Local"
		displayName.Valid = true
	} else if channelID >= 1000000 && channelID <= 2999999 {
		displayName.String = "Corp"
		displayName.Valid = true
	} else if !displayName.Valid || displayName.String == "" {
		// Fallback for other channels with missing display names.
		displayName.String = ResolveChannelDisplayName(ctx, db, channelID)
		displayName.Valid = true
		_, _ = db.ExecContext(ctx,
			`UPDATE channels SET displayName = ? WHERE channelID = ? AND (displayName IS NULL OR displayName = '')`,
			displayName.String, channelID)
	}

	header := []marshal.PyRep{
		marshal.PyString{Value: "channelID"},
		marshal.PyString{Value: "ownerID"},
		marshal.PyString{Value: "displayName"},
		marshal.PyString{Value: "motd"},
		marshal.PyString{Value: "comparisonKey"},
		marshal.PyString{Value: "memberless"},
		marshal.PyString{Value: "password"},
		marshal.PyString{Value: "mailingList"},
		marshal.PyString{Value: "cspa"},
		marshal.PyString{Value: "temporary"},
		marshal.PyString{Value: "subscribed"},
	}
	// lscchannel.py Startup calls channelName.split(), so displayName is never NULL.
	displayRep := nullStrRep(displayName)
	if _, isNone := displayRep.(marshal.PyNone); isNone {
		displayRep = marshal.PyString{Value: fmt.Sprintf("%d", chID)}
	}
	line := []marshal.PyRep{
		intRep(chID), intRep(ownerID), displayRep, nullStrRep(motd),
		nullStrRep(compKey), intRep(memberless), nullStrRep(password), intRep(mailingList),
		intRep(cspa), intRep(temporary), intRep(subscribed),
	}
	return marshal.PyObject{
		Type: "util.Row",
		Args: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "header"}, Value: marshal.PyList{Items: header}},
			{Key: marshal.PyString{Value: "line"}, Value: marshal.PyList{Items: line}},
		}},
	}, nil
}

// QueryChannelMods returns a util.Rowset of access-control entries for a channel,
// matching LSCDB::QueryChannelMods.
func QueryChannelMods(ctx context.Context, db *sql.DB, channelID int64) (marshal.PyRep, error) {
	modsCols := []string{"accessor", "mode", "untilWhen", "originalMode", "admin", "reason"}
	rows, err := db.QueryContext(ctx, `
		SELECT accessor, mode, untilWhen, originalMode, admin, reason
		FROM channelMods WHERE channelID=?`, channelID)
	if err != nil {
		return makeRowset(modsCols, nil), nil
	}
	defer rows.Close()

	var result [][]marshal.PyRep
	haveDefault := false
	for rows.Next() {
		var (
			accessor     sql.NullInt64
			mode         int64
			untilWhen    sql.NullInt64
			originalMode sql.NullInt64
			admin        sql.NullString
			reason       sql.NullString
		)
		if err := rows.Scan(&accessor, &mode, &untilWhen, &originalMode, &admin, &reason); err != nil {
			continue
		}
		// accessor / originalMode are NULL for the channel default-access row.
		var accessorRep marshal.PyRep = marshal.PyNone{}
		if accessor.Valid {
			accessorRep = marshal.PyInt{Value: uint64(accessor.Int64)}
		} else {
			haveDefault = true
		}
		var untilRep marshal.PyRep = marshal.PyNone{}
		if untilWhen.Valid {
			untilRep = marshal.PyInt{Value: uint64(untilWhen.Int64)}
		}
		var origRep marshal.PyRep = marshal.PyNone{}
		if originalMode.Valid {
			origRep = marshal.PyInt{Value: uint64(originalMode.Int64)}
		}
		result = append(result, []marshal.PyRep{
			accessorRep, intRep(mode), untilRep, origRep,
			nullStrRep(admin), nullStrRep(reason),
		})
	}
	if !haveDefault {
		// Every channel needs a default-access row (accessor None, mode 3 CONVERSATIONALIST); with
		// none the client reads NOTSPECIFIED (-1) and neither sends nor receives.
		result = append(result, []marshal.PyRep{
			marshal.PyNone{},        // accessor (channel default)
			marshal.PyInt{Value: 3}, // CHTMODE_CONVERSATIONALIST
			marshal.PyNone{},        // untilWhen
			marshal.PyNone{},        // originalMode
			marshal.PyNone{},        // admin
			marshal.PyNone{},        // reason
		})
	}
	return makeRowset(modsCols, result), nil
}

// QueryChannelChars returns a util.Rowset of members for a channel (EVEmu
// ChannelJoinChannelChars, 7 columns): charID, corpID, mode, allianceID, warFactionID, role,
// extra. "extra" is a nested util.Row (ownerID, ownerName, typeID) for the member panel.
func QueryChannelChars(ctx context.Context, db *sql.DB, channelID int64) (marshal.PyRep, error) {
	charsCols := []string{"charID", "corpID", "mode", "allianceID", "warFactionID", "role", "extra"}
	rows, err := db.QueryContext(ctx, `
		SELECT cc.charID, cc.corpID, cc.allianceID, cc.role,
		       COALESCE(c.characterName,'') AS charName,
		       COALESCE(c.typeID, 1378) AS typeID
		FROM channelChars cc
		LEFT JOIN character_ c ON c.characterID = cc.charID
		WHERE cc.channelID=?`, channelID)
	if err != nil {
		return makeRowset(charsCols, nil), nil
	}
	defer rows.Close()

	var result [][]marshal.PyRep
	for rows.Next() {
		var (
			charID     int64
			corpID     int64
			allianceID int64
			role       int64
			charName   string
			typeID     int64
		)
		if err := rows.Scan(&charID, &corpID, &allianceID, &role, &charName, &typeID); err != nil {
			continue
		}
		// Nested util.Row for "extra": (ownerID, ownerName, typeID).
		extraHeader := marshal.PyList{Items: []marshal.PyRep{
			marshal.PyString{Value: "ownerID"},
			marshal.PyString{Value: "ownerName"},
			marshal.PyString{Value: "typeID"},
		}}
		extra := marshal.PyObject{
			Type: "util.Row",
			Args: marshal.PyDict{Items: []marshal.DictEntry{
				{Key: marshal.PyString{Value: "header"}, Value: extraHeader},
				{Key: marshal.PyString{Value: "line"}, Value: marshal.PyList{Items: []marshal.PyRep{
					intRep(charID),
					strRep(charName),
					intRep(typeID),
				}}},
			}},
		}

		result = append(result, []marshal.PyRep{
			intRep(charID), intRep(corpID),
			intRep(0), // mode
			intRep(allianceID),
			intRep(0), // warFactionID
			intRep(role),
			extra,
		})
	}
	return makeRowset(charsCols, result), nil
}

// EnsureChannelModsTable creates the channelMods ACL table and migrates the legacy EVEmu
// schema so negative modes (CHTMODE_DISALLOWED = -2) store and upserts work. accessor is
// NULL for the channel's default-access row.
func EnsureChannelModsTable(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS channelMods (
			channelID    BIGINT NOT NULL,
			accessor     BIGINT NULL,
			mode         INT NOT NULL DEFAULT 0,
			untilWhen    BIGINT NULL,
			originalMode INT NULL,
			admin        VARCHAR(100) NULL,
			reason       TEXT NULL,
			UNIQUE KEY channel_accessor (channelID, accessor)
		)`); err != nil {
		return fmt.Errorf("EnsureChannelModsTable: %w", err)
	}

	// Legacy dumps define mode/originalMode as UNSIGNED, which rejects block-list rows (-2).
	// Probed first, so a table already widened is not altered on every boot.
	signedInt := func(ct string) bool { return strings.HasPrefix(ct, "int") && !strings.Contains(ct, "unsigned") }
	if columnNeedsModify(ctx, db, "channelMods", "mode", false, signedInt) ||
		columnNeedsModify(ctx, db, "channelMods", "originalMode", true, signedInt) {
		if _, err := db.ExecContext(ctx, `
		ALTER TABLE channelMods
			MODIFY mode INT NOT NULL DEFAULT 0,
			MODIFY originalMode INT NULL`); err != nil {
			return fmt.Errorf("EnsureChannelModsTable: widen mode columns: %w", err)
		}
	}

	// Upserts key on (channelID, accessor); the legacy schema lacks that unique index.
	var haveKey int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.statistics
		WHERE table_schema = DATABASE()
		  AND table_name = 'channelMods'
		  AND index_name = 'channel_accessor'`).Scan(&haveKey); err != nil {
		return fmt.Errorf("EnsureChannelModsTable: check unique key: %w", err)
	}
	if haveKey == 0 {
		// Drop duplicate (channelID, accessor) rows first, keeping the newest.
		if _, err := db.ExecContext(ctx, `
			DELETE m1 FROM channelMods m1
			INNER JOIN channelMods m2
			   ON m1.channelID = m2.channelID
			  AND m1.accessor <=> m2.accessor
			  AND m1.id < m2.id`); err != nil {
			// The legacy table has an id column; a fresh table never reaches this branch.
			return fmt.Errorf("EnsureChannelModsTable: dedupe: %w", err)
		}
		if _, err := db.ExecContext(ctx,
			`ALTER TABLE channelMods ADD UNIQUE KEY channel_accessor (channelID, accessor)`); err != nil {
			return fmt.Errorf("EnsureChannelModsTable: add unique key: %w", err)
		}
	}

	// Flip chat channels (corp, local, player-made) to immediate mode: memberless = 1 hides the
	// member roster. Character channels (mailing lists) keep memberless = 1.
	if _, err := db.ExecContext(ctx,
		`UPDATE channels SET memberless=0 WHERE mailingList=0 AND memberless=1`); err != nil {
		return fmt.Errorf("EnsureChannelModsTable: memberless migration: %w", err)
	}

	// A corp channel travels as a bare int, so the client prints displayName as the tab title:
	// it must read "Corp", not the corporation's name.
	if _, err := db.ExecContext(ctx, `
		UPDATE channels c
		  JOIN corporation p ON p.corporationID = c.channelID
		   SET c.displayName = 'Corp'
		 WHERE c.displayName <> 'Corp'`); err != nil {
		return fmt.Errorf("EnsureChannelModsTable: corp displayName migration: %w", err)
	}
	return nil
}

// UpsertChannelMod inserts or updates an access-control entry. accessor and untilWhen may
// be nil (nil accessor = channel default access).
func UpsertChannelMod(ctx context.Context, db *sql.DB, channelID int64, accessor *int64, mode int64, untilWhen *int64, admin, reason string) error {
	if accessor == nil {
		// A UNIQUE key allows multiple NULLs, so upsert a NULL accessor by hand.
		if _, err := db.ExecContext(ctx,
			`DELETE FROM channelMods WHERE channelID=? AND accessor IS NULL`, channelID); err != nil {
			return fmt.Errorf("UpsertChannelMod(%d,NULL): %w", channelID, err)
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO channelMods (channelID, accessor, mode, untilWhen, originalMode, admin, reason)
			VALUES (?, NULL, ?, ?, NULL, ?, ?)`,
			channelID, mode, untilWhen, admin, reason); err != nil {
			return fmt.Errorf("UpsertChannelMod(%d,NULL): %w", channelID, err)
		}
		return nil
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO channelMods (channelID, accessor, mode, untilWhen, originalMode, admin, reason)
		VALUES (?, ?, ?, ?, NULL, ?, ?)
		ON DUPLICATE KEY UPDATE mode=VALUES(mode), untilWhen=VALUES(untilWhen),
		                        admin=VALUES(admin), reason=VALUES(reason)`,
		channelID, *accessor, mode, untilWhen, admin, reason); err != nil {
		return fmt.Errorf("UpsertChannelMod(%d,%d): %w", channelID, *accessor, err)
	}
	return nil
}

// GetBuddyList returns the character IDs on charID's buddy list: ACL rows on the character's
// personal channel (channelID == charID) with mode >= CHTMODE_SPEAKER (2), as
// lscengine.GetBuddies reads them.
func GetBuddyList(ctx context.Context, db *sql.DB, charID int64) ([]int64, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT accessor FROM channelMods
		 WHERE channelID=? AND accessor IS NOT NULL AND mode>=2`, charID)
	if err != nil {
		return nil, fmt.Errorf("GetBuddyList(%d): %w", charID, err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("GetBuddyList(%d): %w", charID, err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// AddAddressBookContact records ownerID in charID's addressbook by writing the SPEAKER ACL
// row that LSC.AccessControl writes for a buddy; the client splits that list into the Buddies
// and Agents tabs with util.IsNPC. Returns false when nothing was written: already listed, or
// blocked (mode DISALLOWED), which an automatic add never undoes.
func AddAddressBookContact(ctx context.Context, db *sql.DB, charID, ownerID int64) (bool, error) {
	if charID == 0 || ownerID == 0 || charID == ownerID {
		return false, nil
	}
	var mode int64
	err := db.QueryRowContext(ctx,
		`SELECT mode FROM channelMods WHERE channelID=? AND accessor=?`, charID, ownerID).Scan(&mode)
	if err == nil {
		return false, nil // already listed, or blocked
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("AddAddressBookContact(%d,%d): %w", charID, ownerID, err)
	}
	// Legacy dumps put a foreign key on channels.channelID. Re-create the personal channel in
	// its own shape (mailing list, memberless, mode 15), not EnsureChannel's chat-channel shape.
	if _, err := db.ExecContext(ctx,
		`INSERT IGNORE INTO channels
		 VALUES (?, ?, NULL, NULL, NULL, 1, NULL, 1, 100, 0, 15, 1, 0)`,
		charID, charID); err != nil {
		return false, fmt.Errorf("AddAddressBookContact(%d,%d): %w", charID, ownerID, err)
	}
	if err := UpsertChannelMod(ctx, db, charID, &ownerID, ChtModeSpeaker, nil, "", ""); err != nil {
		return false, err
	}
	return true, nil
}

// GetBuddyWatchers returns the character IDs that have charID on their buddy list (the
// inverse of GetBuddyList), the set to push OnContactLoggedOn/Off to. Rows on non-character
// channels never resolve to an online character.
func GetBuddyWatchers(ctx context.Context, db *sql.DB, charID int64) ([]int64, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT channelID FROM channelMods WHERE accessor=? AND mode>=2`, charID)
	if err != nil {
		return nil, fmt.Errorf("GetBuddyWatchers(%d): %w", charID, err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("GetBuddyWatchers(%d): %w", charID, err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// DeleteChannelMod removes an access-control entry (mode back to NOTSPECIFIED).
func DeleteChannelMod(ctx context.Context, db *sql.DB, channelID int64, accessor *int64) error {
	var err error
	if accessor == nil {
		_, err = db.ExecContext(ctx,
			`DELETE FROM channelMods WHERE channelID=? AND accessor IS NULL`, channelID)
	} else {
		_, err = db.ExecContext(ctx,
			`DELETE FROM channelMods WHERE channelID=? AND accessor=?`, channelID, *accessor)
	}
	if err != nil {
		return fmt.Errorf("DeleteChannelMod(%d): %w", channelID, err)
	}
	return nil
}

// Channel access modes the server tests, mirroring chat.CHTMODE_* in lscengine.py.
const (
	ChtModeCreator      int64 = 15
	ChtModeOperator     int64 = 7
	ChtModeNotSpecified int64 = -1
)

// ChannelModRow is one channelMods row as ResolveChannelAccess reads it. Accessor is nil on
// the channel's default-access row; UntilWhen is a Win32 FILETIME, 0 when the row never lapses.
type ChannelModRow struct {
	Accessor  *int64
	Mode      int64
	UntilWhen uint64
}

// ResolveChannelAccess returns charID's access mode on a channel, by the rule of the client's
// GetAccessInfo: the owner is the creator; else the character's own row wins, then the lowest
// mode among the rows for ids (corporation, alliance), then the default row. A row whose
// untilWhen has passed is ignored. No matching row is ChtModeNotSpecified.
func ResolveChannelAccess(ownerID, charID int64, rows []ChannelModRow, ids []int64, now uint64) int64 {
	if charID != 0 && ownerID == charID {
		return ChtModeCreator
	}
	public := ChtModeNotSpecified
	var other *int64
	for i := range rows {
		r := &rows[i]
		if r.UntilWhen != 0 && r.UntilWhen < now {
			continue
		}
		if r.Accessor == nil {
			public = r.Mode
			continue
		}
		if *r.Accessor == charID {
			return r.Mode
		}
		for _, id := range ids {
			if id != 0 && id == *r.Accessor && (other == nil || *other > r.Mode) {
				m := r.Mode
				other = &m
			}
		}
	}
	if other != nil {
		return *other
	}
	return public
}

// ChannelAccessMode loads a persisted channel's owner and ACL rows and resolves charID's access
// mode with ResolveChannelAccess. A channel with no row answers ChtModeNotSpecified.
func ChannelAccessMode(ctx context.Context, db *sql.DB, channelID, charID, corpID, allianceID int64) (int64, error) {
	var ownerID int64
	err := db.QueryRowContext(ctx,
		`SELECT ownerID FROM channels WHERE channelID=?`, channelID).Scan(&ownerID)
	if errors.Is(err, sql.ErrNoRows) {
		return ChtModeNotSpecified, nil
	}
	if err != nil {
		return ChtModeNotSpecified, fmt.Errorf("ChannelAccessMode(%d): %w", channelID, err)
	}
	rows, err := db.QueryContext(ctx,
		`SELECT accessor, mode, untilWhen FROM channelMods WHERE channelID=?`, channelID)
	if err != nil {
		return ChtModeNotSpecified, fmt.Errorf("ChannelAccessMode(%d): %w", channelID, err)
	}
	defer rows.Close()
	var mods []ChannelModRow
	for rows.Next() {
		var accessor, until sql.NullInt64
		var mode int64
		if err := rows.Scan(&accessor, &mode, &until); err != nil {
			return ChtModeNotSpecified, fmt.Errorf("ChannelAccessMode(%d): %w", channelID, err)
		}
		row := ChannelModRow{Mode: mode}
		if accessor.Valid {
			id := accessor.Int64
			row.Accessor = &id
		}
		if until.Valid && until.Int64 > 0 {
			row.UntilWhen = uint64(until.Int64)
		}
		mods = append(mods, row)
	}
	if err := rows.Err(); err != nil {
		return ChtModeNotSpecified, fmt.Errorf("ChannelAccessMode(%d): %w", channelID, err)
	}
	return ResolveChannelAccess(ownerID, charID, mods, []int64{corpID, allianceID}, win32TimeNow()), nil
}

// channelUpdatableCols whitelists the channels columns that Configure, RenameChannel and
// SetChannelMOTD may write.
var channelUpdatableCols = map[string]bool{
	"displayName": true, "motd": true, "password": true,
	"memberless": true, "cspa": true, "ownerID": true,
}

// UpdateChannelField sets a single whitelisted column on a channel row.
func UpdateChannelField(ctx context.Context, db *sql.DB, channelID int64, column string, value any) error {
	if !channelUpdatableCols[column] {
		return fmt.Errorf("UpdateChannelField: column %q not allowed", column)
	}
	_, err := db.ExecContext(ctx,
		fmt.Sprintf("UPDATE channels SET %s=? WHERE channelID=?", column),
		value, channelID)
	if err != nil {
		return fmt.Errorf("UpdateChannelField(%d,%s): %w", channelID, column, err)
	}
	return nil
}

// DeleteChannel removes a channel and all its membership / ACL rows.
func DeleteChannel(ctx context.Context, db *sql.DB, channelID int64) error {
	if _, err := db.ExecContext(ctx, `DELETE FROM channelChars WHERE channelID=?`, channelID); err != nil {
		return fmt.Errorf("DeleteChannel(%d) chars: %w", channelID, err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM channelMods WHERE channelID=?`, channelID); err != nil {
		return fmt.Errorf("DeleteChannel(%d) mods: %w", channelID, err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM channels WHERE channelID=?`, channelID); err != nil {
		return fmt.Errorf("DeleteChannel(%d): %w", channelID, err)
	}
	return nil
}

// GetCharGender returns the gender flag (0=female, 1=male) for a character.
func GetCharGender(ctx context.Context, db *sql.DB, charID int64) int64 {
	var gender int64
	_ = db.QueryRowContext(ctx,
		`SELECT gender FROM character_ WHERE characterID=?`, charID).Scan(&gender)
	return gender
}

// CleanupStalePrivateChannels removes private channel rows (channelID >= 140000000) that do
// not belong to a player character: NPC character channels and stale buddy-chat rows.
func CleanupStalePrivateChannels(ctx context.Context, db *sql.DB) error {
	// Corporation and alliance channels are keyed on the owner's ID, which can sit above the
	// private-chat floor. Deleting those drops a live corp's channel and roster.
	const keepOwners = `
		  AND channelID NOT IN (
		      SELECT characterID FROM character_ WHERE accountID IS NOT NULL
		  )
		  AND channelID NOT IN (SELECT corporationID FROM corporation)
		  AND channelID NOT IN (SELECT allianceID FROM alliances)`
	if _, err := db.ExecContext(ctx, `
		DELETE FROM channelChars
		WHERE channelID >= 140000000`+keepOwners); err != nil {
		return fmt.Errorf("CleanupStalePrivateChannels(channelChars): %w", err)
	}
	if _, err := db.ExecContext(ctx, `
		DELETE FROM channels
		WHERE channelID >= 140000000`+keepOwners); err != nil {
		return fmt.Errorf("CleanupStalePrivateChannels(channels): %w", err)
	}
	return nil
}

// RepairCharacterChannels re-creates missing private channels for player characters only
// (accountID non-NULL), with the column values CreateCharacter writes.
func RepairCharacterChannels(ctx context.Context, db *sql.DB) {
	res, err := db.ExecContext(ctx, `
		INSERT IGNORE INTO channels
		SELECT c.characterID, c.characterID, NULL, NULL, NULL, 1, NULL, 1, 100, 0, 15, 1, 0
		FROM character_ c
		WHERE c.accountID IS NOT NULL
		  AND c.characterID NOT IN (SELECT channelID FROM channels)`)
	if err != nil {
		log.Printf("RepairCharacterChannels: %v", err)
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("RepairCharacterChannels: restored %d character channel(s)", n)
	}
}
