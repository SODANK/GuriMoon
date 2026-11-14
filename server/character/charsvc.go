// Package character implements the "character" service: creation, selection, deletion, notes.
package character

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	evecache "gurimoon/cache"
	evedb "gurimoon/db"
	"gurimoon/esp"
	"gurimoon/logx"
	"gurimoon/marshal"
	"gurimoon/modapi"
	evenet "gurimoon/net"
	"gurimoon/safego"
	"gurimoon/server"
	"gurimoon/server/infocache"
	"gurimoon/server/station"
)

// win32MonthTicks is 30 days in 100-nanosecond ticks (30 * 86400 * 10^7).
const win32MonthTicks uint64 = 25920000000000

// win32TimeNow returns current UTC time as Win32 FILETIME (100-ns ticks since 1601-01-01).
func win32TimeNow() uint64 {
	return uint64(time.Now().UnixNano()/100) + 116444736000000000
}

const (
	globalNodeID      uint64 = 888444       // the server node ID
	hackFixedClientID uint64 = 130293001608 // client address of session change notifications
)

// Handle dispatches a CallReq addressed to the "character" service. charID and stationID point
// at the connection's state; SelectCharacterID sets them so later calls know the character and
// station.GetStationItemBits has a station. stationReg is the shared guest registry.
func Handle(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, charID *int64, stationID *int64, sqldb *sql.DB, store *evecache.Store, startBalance float64, sessReg *esp.SessionRegistry, stationReg *station.StationRegistry) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	switch cs.Method {
	case "GetCharactersToSelect":
		return handleGetCharactersToSelect(ctx, c, addr, cs, pkt, accountID, sqldb)
	case "GetCharacterToSelect":
		return handleGetCharacterToSelect(ctx, c, addr, cs, pkt, accountID, sqldb)
	case "SelectCharacterID":
		return handleSelectCharacterID(ctx, c, addr, cs, pkt, accountID, charID, stationID, sqldb, sessReg, stationReg)
	case "GetCharCreationInfo":
		return handleGetCharCreationInfo(ctx, c, addr, pkt, accountID, sqldb, store)
	case "GetAppearanceInfo":
		return handleGetAppearanceInfo(ctx, c, addr, pkt, accountID, sqldb, store)
	case "ValidateName":
		return handleValidateName(ctx, c, addr, cs, pkt, accountID, sqldb)
	case "CreateCharacter":
		return handleCreateCharacter(ctx, c, addr, cs, pkt, accountID, sqldb, store, startBalance)
	case "GetCloneTypeID":
		cloneTypeID, err := evedb.GetCloneTypeID(ctx, sqldb, *charID)
		if err != nil {
			log.Printf("[%s] → character.GetCloneTypeID ERROR: %v — falling back to 164", addr, err)
			cloneTypeID = 164 // Clone Grade Alpha
		}
		log.Printf("[%s] → character.GetCloneTypeID (%d)", addr, cloneTypeID)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyInt{Value: uint64(cloneTypeID)})
	case "GetHomeStation":
		// The station where the character's medical clone is installed.
		homeStationID, err := evedb.GetHomeStationID(ctx, sqldb, *charID)
		if err != nil || homeStationID == 0 {
			// Fallback to current station if not yet set.
			homeStationID = *stationID
		}
		log.Printf("[%s] → character.GetHomeStation (%d)", addr, homeStationID)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyInt{Value: uint64(homeStationID)})
	case "GetCharacterDescription":
		return handleGetCharacterDescription(ctx, c, addr, cs, pkt, accountID, sqldb)
	case "SetCharacterDescription":
		return handleSetCharacterDescription(ctx, c, addr, cs, pkt, accountID, *charID, sqldb)
	case "GetNote":
		return handleGetNote(ctx, c, addr, cs, pkt, accountID, *charID, sqldb)
	case "SetNote":
		return handleSetNote(ctx, c, addr, cs, pkt, accountID, *charID, sqldb)
	case "GetOwnerNoteLabels":
		return handleGetOwnerNoteLabels(ctx, c, addr, pkt, accountID, *charID, sqldb)
	case "AddOwnerNote":
		return handleAddOwnerNote(ctx, c, addr, cs, pkt, accountID, *charID, sqldb)
	case "EditOwnerNote":
		return handleEditOwnerNote(ctx, c, addr, cs, pkt, accountID, *charID, sqldb)
	case "GetOwnerNote":
		return handleGetOwnerNote(ctx, c, addr, cs, pkt, accountID, *charID, sqldb)
	case "RemoveOwnerNote":
		return handleRemoveOwnerNote(ctx, c, addr, cs, pkt, accountID, *charID, sqldb)
	case "GetCharacterInfo":
		return handleGetCharacterInfo(ctx, c, addr, pkt, accountID, sqldb)
	case "DeleteCharacter":
		return handleDeleteCharacter(ctx, c, addr, cs, pkt, accountID, sqldb)
	case "GetCharacterAppearanceList":
		return handleGetCharacterAppearanceList(ctx, c, addr, cs, pkt, accountID, sqldb)
	case "PrepareCharacterForDelete":
		return handlePrepareCharacterForDelete(ctx, c, addr, cs, pkt, accountID, sqldb)
	case "CancelCharacterDeletePrepare":
		return handleCancelCharacterDeletePrepare(ctx, c, addr, cs, pkt, accountID, sqldb)
	default:
		server.RecordUnhandled(addr, "character", cs, false)
		log.Printf("[%s] character: unhandled method %q — sending exception", addr, cs.Method)
		return server.SendCallException(c, pkt, accountID, nil)
	}
}

// handleGetCharCreationInfo returns a PyDict of util.CachedObject hints for the 20
// character-creation cachable objects.
func handleGetCharCreationInfo(ctx context.Context, c *evenet.Conn, addr string, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB, store *evecache.Store) error {
	hints := store.HintsForSet(ctx, sqldb, evecache.CharCreateCachables)
	log.Printf("[%s] → character.GetCharCreationInfo (%d hints)", addr, len(hints.Items))
	return server.SendCallRsp(c, pkt, accountID, hints)
}

// handleGetAppearanceInfo returns a PyDict of util.CachedObject hints for the 12 appearance
// cachable objects.
func handleGetAppearanceInfo(ctx context.Context, c *evenet.Conn, addr string, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB, store *evecache.Store) error {
	hints := store.HintsForSet(ctx, sqldb, evecache.AppearanceCachables)
	log.Printf("[%s] → character.GetAppearanceInfo (%d hints)", addr, len(hints.Items))
	return server.SendCallRsp(c, pkt, accountID, hints)
}

// handleValidateName checks whether a character name is available.
// Arg[0] is the name string.  Returns PyBool true if valid/available.
func handleValidateName(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	if len(cs.ArgTuple.Items) < 1 {
		log.Printf("[%s] character.ValidateName: missing name arg", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyBool{Value: false})
	}
	nameRep, ok := cs.ArgTuple.Items[0].(marshal.PyString)
	if !ok {
		log.Printf("[%s] character.ValidateName: arg[0] is %T, want PyString", addr, cs.ArgTuple.Items[0])
		return server.SendCallRsp(c, pkt, accountID, marshal.PyBool{Value: false})
	}
	name := nameRep.Value
	if name == "" {
		return server.SendCallRsp(c, pkt, accountID, marshal.PyBool{Value: false})
	}
	valid, err := evedb.ValidateCharName(ctx, sqldb, name)
	if err != nil {
		log.Printf("[%s] character.ValidateName db: %v", addr, err)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyBool{Value: false})
	}
	if !valid {
		// Portrait recustomization: the flagged character's own name must validate though it is taken.
		if flagged, ferr := evedb.GetRecustomizationsForAccount(ctx, sqldb, accountID); ferr == nil {
			for _, ri := range flagged {
				if strings.EqualFold(ri.CharacterName, name) {
					valid = true
					break
				}
			}
		}
	}
	log.Printf("[%s] → character.ValidateName %q → %v", addr, name, valid)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyBool{Value: valid})
}

// handleCreateCharacter parses the 14-element arg tuple from the client and inserts the
// character. Args: [0]=name [1]=bloodlineID [2]=genderID [3]=ancestryID [4]=schoolID
// [5]=departmentID [6]=fieldID [7]=specialityID [8..12]=intelligence, charisma, perception,
// memory, willpower [13]=appearance (PyDict).
func handleCreateCharacter(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB, store *evecache.Store, startBalance float64) error {
	args := cs.ArgTuple.Items
	if len(args) < 14 {
		log.Printf("[%s] character.CreateCharacter: expected 14 args, got %d", addr, len(args))
		return server.SendCallException(c, pkt, accountID, nil)
	}

	name, ok := args[0].(marshal.PyString)
	if !ok {
		log.Printf("[%s] character.CreateCharacter: arg[0] name is %T", addr, args[0])
		return server.SendCallException(c, pkt, accountID, nil)
	}

	getInt := func(i int) (int64, bool) {
		v, ok := args[i].(marshal.PyInt)
		if !ok {
			return 0, false
		}
		return int64(v.Value), true
	}

	var ints [12]int64
	for i := 0; i < 12; i++ {
		v, ok := getInt(i + 1)
		if !ok {
			log.Printf("[%s] character.CreateCharacter: arg[%d] is %T, want PyInt", addr, i+1, args[i+1])
			return server.SendCallException(c, pkt, accountID, nil)
		}
		ints[i] = v
	}

	appearanceRaw := parseAppearanceDict(args[13])

	// Portrait recustomization: a character flagged by /redoportrait makes this call only an
	// appearance edit. Bloodline and gender must match; name, attributes and education are discarded.
	if flagged, ferr := evedb.GetRecustomizationsForAccount(ctx, sqldb, accountID); ferr == nil && len(flagged) > 0 {
		bloodlineID, genderID := ints[0], ints[1]
		var match *evedb.RecustomizationInfo
		for i := range flagged {
			if flagged[i].BloodlineID == bloodlineID && flagged[i].Gender == genderID {
				match = &flagged[i]
				break
			}
		}
		if match == nil {
			ri := flagged[0]
			blName, berr := evedb.GetBloodlineName(ctx, sqldb, ri.BloodlineID)
			if berr != nil {
				blName = fmt.Sprintf("bloodline %d", ri.BloodlineID)
			}
			gender := "female"
			if ri.Gender != 0 {
				gender = "male"
			}
			log.Printf("[%s] character.CreateCharacter: recustomization bloodline/gender mismatch (picked %d/%d, need %d/%d for %q)",
				addr, bloodlineID, genderID, ri.BloodlineID, ri.Gender, ri.CharacterName)
			return server.SendCallException(c, pkt, accountID, server.BuildUserError(
				"CustomInfo", map[string]marshal.PyRep{
					"info": marshal.PyString{Value: fmt.Sprintf(
						"Portrait recustomization for %s: you must pick your original bloodline (%s) and gender (%s). Please restart character creation — only your portrait will be changed.",
						ri.CharacterName, blName, gender)},
				}))
		}
		if uerr := evedb.UpdateCharAppearance(ctx, sqldb, match.CharacterID, appearanceRaw); uerr != nil {
			log.Printf("[%s] character.CreateCharacter recustomization db: %v", addr, uerr)
			return server.SendCallException(c, pkt, accountID, nil)
		}
		if _, cerr := evedb.ClearRecustomization(ctx, sqldb, match.CharacterID); cerr != nil {
			log.Printf("[%s] character.CreateCharacter recustomization clear flag: %v", addr, cerr)
		}
		log.Printf("[%s] → character.CreateCharacter: recustomized portrait for %q (charID=%d) — no character created",
			addr, match.CharacterName, match.CharacterID)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyInt{Value: uint64(match.CharacterID)})
	}

	in := &evedb.CharCreationInput{
		Name:         name.Value,
		BloodlineID:  ints[0],
		GenderID:     ints[1],
		AncestryID:   ints[2],
		SchoolID:     ints[3],
		DepartmentID: ints[4],
		FieldID:      ints[5],
		SpecialityID: ints[6],
		Intelligence: ints[7],
		Charisma:     ints[8],
		Perception:   ints[9],
		Memory:       ints[10],
		Willpower:    ints[11],
		Appearance:   appearanceRaw,
	}

	charID, err := evedb.CreateCharacter(ctx, sqldb, accountID, in, startBalance)
	if err != nil {
		log.Printf("[%s] character.CreateCharacter db: %v", addr, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}

	// Drop the owners bulk so later logins see the new character; connected clients use GetOwnersEx.
	if store != nil {
		store.InvalidateOwners()
	}

	log.Printf("[%s] → character.CreateCharacter name=%q charID=%d", addr, name.Value, charID)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyInt{Value: uint64(charID)})
}

// handleGetCharacterDescription returns util.Row{description} for the charID in arg[0].
func handleGetCharacterDescription(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	if len(cs.ArgTuple.Items) < 1 {
		log.Printf("[%s] character.GetCharacterDescription: missing charID arg", addr)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	charIDArg, ok := cs.ArgTuple.Items[0].(marshal.PyInt)
	if !ok {
		log.Printf("[%s] character.GetCharacterDescription: arg[0] is %T, want PyInt", addr, cs.ArgTuple.Items[0])
		return server.SendCallException(c, pkt, accountID, nil)
	}
	desc, err := evedb.GetCharDescription(ctx, sqldb, int64(charIDArg.Value))
	if err != nil {
		log.Printf("[%s] character.GetCharacterDescription db: %v", addr, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	result := marshal.PyObject{
		Type: "util.Row",
		Args: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "header"}, Value: marshal.PyList{Items: []marshal.PyRep{marshal.PyString{Value: "description"}}}},
			{Key: marshal.PyString{Value: "line"}, Value: marshal.PyList{Items: []marshal.PyRep{marshal.PyString{Value: desc}}}},
		}},
	}
	log.Printf("[%s] → character.GetCharacterDescription charID=%d", addr, charIDArg.Value)
	return server.SendCallRsp(c, pkt, accountID, result)
}

// handleSetCharacterDescription updates the logged-in character's description (arg[0]) and
// returns None.
func handleSetCharacterDescription(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID int64, sqldb *sql.DB) error {
	var desc string
	if len(cs.ArgTuple.Items) >= 1 {
		if s, ok := cs.ArgTuple.Items[0].(marshal.PyString); ok {
			desc = s.Value
		}
	}
	if charID == 0 {
		log.Printf("[%s] character.SetCharacterDescription: no charID in session", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	if err := evedb.SetCharDescription(ctx, sqldb, charID, desc); err != nil {
		log.Printf("[%s] character.SetCharacterDescription db: %v", addr, err)
	}
	log.Printf("[%s] → character.SetCharacterDescription charID=%d", addr, charID)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// handleGetNote returns the note string (or None) for (charID, itemID).
func handleGetNote(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID int64, sqldb *sql.DB) error {
	if len(cs.ArgTuple.Items) < 1 {
		log.Printf("[%s] character.GetNote: missing itemID arg", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	itemIDArg, ok := cs.ArgTuple.Items[0].(marshal.PyInt)
	if !ok {
		log.Printf("[%s] character.GetNote: arg[0] is %T, want PyInt", addr, cs.ArgTuple.Items[0])
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	note, found, err := evedb.GetCharNote(ctx, sqldb, charID, int64(itemIDArg.Value))
	if err != nil {
		log.Printf("[%s] character.GetNote db: %v", addr, err)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	log.Printf("[%s] → character.GetNote itemID=%d found=%v", addr, itemIDArg.Value, found)
	if !found {
		// Empty string, never None: infosvc.SaveNote returns early on a None load, so a first note
		// would never save.
		return server.SendCallRsp(c, pkt, accountID, marshal.PyString{Value: ""})
	}
	return server.SendCallRsp(c, pkt, accountID, marshal.PyString{Value: note})
}

// handleSetNote upserts or deletes a note for (charID, itemID). Returns None.
func handleSetNote(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID int64, sqldb *sql.DB) error {
	if len(cs.ArgTuple.Items) < 2 {
		log.Printf("[%s] character.SetNote: expected (itemID, note), got %d args", addr, len(cs.ArgTuple.Items))
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	itemIDArg, ok := cs.ArgTuple.Items[0].(marshal.PyInt)
	if !ok {
		log.Printf("[%s] character.SetNote: arg[0] is %T, want PyInt", addr, cs.ArgTuple.Items[0])
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	var noteStr string
	switch v := cs.ArgTuple.Items[1].(type) {
	case marshal.PyString:
		noteStr = v.Value
	case marshal.PyNone:
		noteStr = ""
	default:
		log.Printf("[%s] character.SetNote: arg[1] is %T, ignoring", addr, cs.ArgTuple.Items[1])
	}
	if err := evedb.SetCharNote(ctx, sqldb, charID, int64(itemIDArg.Value), noteStr); err != nil {
		log.Printf("[%s] character.SetNote db: %v", addr, err)
	}
	log.Printf("[%s] → character.SetNote itemID=%d (len=%d)", addr, itemIDArg.Value, len(noteStr))
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// handleGetOwnerNoteLabels returns a util.Rowset(noteID, label) from chrOwnerNote.
func handleGetOwnerNoteLabels(ctx context.Context, c *evenet.Conn, addr string, pkt *evenet.PyPacket, accountID, charID int64, sqldb *sql.DB) error {
	result, err := evedb.GetOwnerNoteLabels(ctx, sqldb, charID)
	if err != nil {
		log.Printf("[%s] character.GetOwnerNoteLabels db: %v", addr, err)
	}
	log.Printf("[%s] → character.GetOwnerNoteLabels charID=%d", addr, charID)
	return server.SendCallRsp(c, pkt, accountID, result)
}

// handleAddOwnerNote inserts a note and returns its noteID. Args: (label, content).
func handleAddOwnerNote(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID int64, sqldb *sql.DB) error {
	if len(cs.ArgTuple.Items) < 2 {
		log.Printf("[%s] character.AddOwnerNote: expected (label, content), got %d args", addr, len(cs.ArgTuple.Items))
		return server.SendCallRsp(c, pkt, accountID, marshal.PyInt{Value: 0})
	}
	label, _ := cs.ArgTuple.Items[0].(marshal.PyString)
	content, _ := cs.ArgTuple.Items[1].(marshal.PyString)
	noteID, err := evedb.AddOwnerNote(ctx, sqldb, charID, label.Value, content.Value)
	if err != nil {
		log.Printf("[%s] character.AddOwnerNote db: %v", addr, err)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	log.Printf("[%s] → character.AddOwnerNote noteID=%d", addr, noteID)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyInt{Value: uint64(noteID)})
}

// handleEditOwnerNote updates a note and returns the noteID. The notepad sends (noteID, label,
// content) for a body edit and (noteID, label) for a rename; the 2-arg form must keep the body.
func handleEditOwnerNote(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID int64, sqldb *sql.DB) error {
	if len(cs.ArgTuple.Items) < 2 {
		log.Printf("[%s] character.EditOwnerNote: expected (noteID, label[, content]), got %d args", addr, len(cs.ArgTuple.Items))
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	noteIDArg, ok := cs.ArgTuple.Items[0].(marshal.PyInt)
	if !ok {
		log.Printf("[%s] character.EditOwnerNote: arg[0] is %T, want PyInt", addr, cs.ArgTuple.Items[0])
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	label, _ := cs.ArgTuple.Items[1].(marshal.PyString)
	var err error
	if len(cs.ArgTuple.Items) >= 3 {
		content, _ := cs.ArgTuple.Items[2].(marshal.PyString)
		err = evedb.EditOwnerNote(ctx, sqldb, charID, int64(noteIDArg.Value), label.Value, content.Value)
	} else {
		err = evedb.EditOwnerNoteLabel(ctx, sqldb, charID, int64(noteIDArg.Value), label.Value)
	}
	if err != nil {
		log.Printf("[%s] character.EditOwnerNote db: %v", addr, err)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	log.Printf("[%s] → character.EditOwnerNote noteID=%d (%d args)", addr, noteIDArg.Value, len(cs.ArgTuple.Items))
	return server.SendCallRsp(c, pkt, accountID, marshal.PyInt{Value: noteIDArg.Value})
}

// handleRemoveOwnerNote deletes a note. Args: (noteID). The notepad ignores the return value.
func handleRemoveOwnerNote(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID int64, sqldb *sql.DB) error {
	if len(cs.ArgTuple.Items) < 1 {
		log.Printf("[%s] character.RemoveOwnerNote: missing noteID arg", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	noteIDArg, ok := cs.ArgTuple.Items[0].(marshal.PyInt)
	if !ok {
		log.Printf("[%s] character.RemoveOwnerNote: arg[0] is %T, want PyInt", addr, cs.ArgTuple.Items[0])
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	if err := evedb.RemoveOwnerNote(ctx, sqldb, charID, int64(noteIDArg.Value)); err != nil {
		log.Printf("[%s] character.RemoveOwnerNote db: %v", addr, err)
	}
	log.Printf("[%s] → character.RemoveOwnerNote noteID=%d", addr, noteIDArg.Value)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// handleGetCharacterInfo returns a rowset of (characterID, characterName) for every character on
// the account. The character-select petition form uses it before any character session exists.
func handleGetCharacterInfo(ctx context.Context, c *evenet.Conn, addr string, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	result, err := evedb.GetAccountCharacterInfo(ctx, sqldb, accountID)
	if err != nil {
		log.Printf("[%s] character.GetCharacterInfo db: %v", addr, err)
	}
	log.Printf("[%s] → character.GetCharacterInfo accountID=%d", addr, accountID)
	return server.SendCallRsp(c, pkt, accountID, result)
}

// handleDeleteCharacter deletes a character. Args: (charID). The charsel screen treats a falsy
// return as success and refetches the list; a truthy return must be eve.Message arguments,
// ('CustomNotify', {'notify': text}).
func handleDeleteCharacter(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	fail := func(msg string) error {
		return server.SendCallRsp(c, pkt, accountID, marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyString{Value: "CustomNotify"},
			marshal.PyDict{Items: []marshal.DictEntry{
				{Key: marshal.PyString{Value: "notify"}, Value: marshal.PyString{Value: msg}},
			}},
		}})
	}
	if len(cs.ArgTuple.Items) < 1 {
		return fail("No character specified.")
	}
	charIDArg, ok := cs.ArgTuple.Items[0].(marshal.PyInt)
	if !ok {
		return fail("No character specified.")
	}
	delCharID := int64(charIDArg.Value)
	if err := evedb.VerifyCharOwnership(ctx, sqldb, accountID, delCharID); err != nil {
		log.Printf("[%s] character.DeleteCharacter: ownership check failed charID=%d: %v", addr, delCharID, err)
		return fail("That character does not belong to this account.")
	}

	// Honour the deletion countdown unless the delay is 0 (immediate mode).
	delayHours := 1.0
	if v, ok, _ := evedb.GetServerSetting(ctx, sqldb, "character.deleteDelayHours"); ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			delayHours = f
		}
	}
	if delayHours > 0 {
		deleteAt, hasPrepare, err := evedb.GetCharacterDeletePrepare(ctx, sqldb, delCharID)
		if err != nil {
			log.Printf("[%s] character.DeleteCharacter: GetCharacterDeletePrepare charID=%d: %v", addr, delCharID, err)
			return fail("Character deletion failed — please petition a GM.")
		}
		if !hasPrepare {
			return fail("Character has not been prepared for deletion.")
		}
		if deleteAt > win32TimeNow() {
			return fail("Character is still in its deletion countdown.")
		}
	}

	if err := evedb.DeleteCharacter(ctx, sqldb, delCharID); err != nil {
		log.Printf("[%s] character.DeleteCharacter: db error charID=%d: %v", addr, delCharID, err)
		return fail("Character deletion failed — please petition a GM.")
	}
	log.Printf("[%s] → character.DeleteCharacter charID=%d (deleted)", addr, delCharID)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// handleGetOwnerNote returns a util.Rowset(note) for one noteID. Args: (noteID).
func handleGetOwnerNote(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID int64, sqldb *sql.DB) error {
	if len(cs.ArgTuple.Items) < 1 {
		log.Printf("[%s] character.GetOwnerNote: missing noteID arg", addr)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	noteIDArg, ok := cs.ArgTuple.Items[0].(marshal.PyInt)
	if !ok {
		log.Printf("[%s] character.GetOwnerNote: arg[0] is %T, want PyInt", addr, cs.ArgTuple.Items[0])
		return server.SendCallException(c, pkt, accountID, nil)
	}
	result, err := evedb.GetOwnerNote(ctx, sqldb, charID, int64(noteIDArg.Value))
	if err != nil {
		log.Printf("[%s] character.GetOwnerNote db: %v", addr, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	log.Printf("[%s] → character.GetOwnerNote noteID=%d", addr, noteIDArg.Value)
	return server.SendCallRsp(c, pkt, accountID, result)
}

// handleGetCharacterAppearanceList returns a PyList of appearance Rowsets (PyNone for a missing
// char) for the charIDs in arg[0].
func handleGetCharacterAppearanceList(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	var charIDs []int64
	if len(cs.ArgTuple.Items) >= 1 {
		if list, ok := cs.ArgTuple.Items[0].(marshal.PyList); ok {
			for _, item := range list.Items {
				if id, ok := item.(marshal.PyInt); ok {
					charIDs = append(charIDs, int64(id.Value))
				}
			}
		}
	}
	// Memoized on the single-character form (one call per ball on a crowded grid). A face does not
	// change in a session, so the TTL is long. See package infocache.
	var result marshal.PyRep
	var err error
	if len(charIDs) == 1 {
		result, err = infocache.Shared.Get("character.GetCharacterAppearanceList", charIDs[0], infocache.TTLStatic,
			func() (marshal.PyRep, error) {
				return evedb.GetCharAppearanceList(ctx, sqldb, charIDs)
			})
	} else {
		result, err = evedb.GetCharAppearanceList(ctx, sqldb, charIDs)
	}
	if err != nil {
		log.Printf("[%s] character.GetCharacterAppearanceList db: %v", addr, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	logx.Debugf("[%s] → character.GetCharacterAppearanceList (%d chars)", addr, len(charIDs))
	return server.SendCallRsp(c, pkt, accountID, result)
}

// handlePrepareCharacterForDelete starts a timed deletion: it sets deletePrepareDateTime to now
// plus the delay and returns that FILETIME; DeleteCharacter deletes once it has passed. The delay
// is server_settings "character.deleteDelayHours" (default 1); 0 deletes immediately.
func handlePrepareCharacterForDelete(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	if len(cs.ArgTuple.Items) < 1 {
		log.Printf("[%s] character.PrepareCharacterForDelete: missing charID arg", addr)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	charIDArg, ok := cs.ArgTuple.Items[0].(marshal.PyInt)
	if !ok {
		log.Printf("[%s] character.PrepareCharacterForDelete: arg[0] is %T, want PyInt", addr, cs.ArgTuple.Items[0])
		return server.SendCallException(c, pkt, accountID, nil)
	}
	delCharID := int64(charIDArg.Value)

	if err := evedb.VerifyCharOwnership(ctx, sqldb, accountID, delCharID); err != nil {
		log.Printf("[%s] character.PrepareCharacterForDelete: ownership check failed charID=%d: %v", addr, delCharID, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}

	delayHours := 1.0
	if v, ok, _ := evedb.GetServerSetting(ctx, sqldb, "character.deleteDelayHours"); ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			delayHours = f
		}
	}

	const win32Second uint64 = 10_000_000
	if delayHours == 0 {
		// Delay 0: delete immediately.
		if err := evedb.DeleteCharacter(ctx, sqldb, delCharID); err != nil {
			log.Printf("[%s] character.PrepareCharacterForDelete: db error charID=%d: %v", addr, delCharID, err)
			return server.SendCallException(c, pkt, accountID, nil)
		}
		deleteTime := win32TimeNow() + 5*win32Second
		log.Printf("[%s] → character.PrepareCharacterForDelete charID=%d (deleted immediately, delay=0)", addr, delCharID)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyInt{Value: deleteTime})
	}

	delaySec := uint64(delayHours * 3600)
	deleteTime := win32TimeNow() + delaySec*win32Second
	if err := evedb.SetCharacterDeletePrepare(ctx, sqldb, delCharID, deleteTime); err != nil {
		log.Printf("[%s] character.PrepareCharacterForDelete: db error charID=%d: %v", addr, delCharID, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	log.Printf("[%s] → character.PrepareCharacterForDelete charID=%d deleteAt=%d (%.1fh from now)",
		addr, delCharID, deleteTime, delayHours)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyInt{Value: deleteTime})
}

// handleCancelCharacterDeletePrepare clears the deletion countdown.
func handleCancelCharacterDeletePrepare(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	if len(cs.ArgTuple.Items) < 1 {
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	charIDArg, ok := cs.ArgTuple.Items[0].(marshal.PyInt)
	if !ok {
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	delCharID := int64(charIDArg.Value)
	if err := evedb.VerifyCharOwnership(ctx, sqldb, accountID, delCharID); err != nil {
		log.Printf("[%s] character.CancelCharacterDeletePrepare: ownership check failed charID=%d: %v", addr, delCharID, err)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	if err := evedb.ClearCharacterDeletePrepare(ctx, sqldb, delCharID); err != nil {
		log.Printf("[%s] character.CancelCharacterDeletePrepare: db error charID=%d: %v", addr, delCharID, err)
	}
	log.Printf("[%s] → character.CancelCharacterDeletePrepare charID=%d (cleared)", addr, delCharID)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// parseAppearanceDict converts a PyDict or PyNone appearance argument into the map of int64 or
// float64 values evedb.CreateCharacter takes.
func parseAppearanceDict(v marshal.PyRep) map[string]interface{} {
	result := make(map[string]interface{})
	d, ok := v.(marshal.PyDict)
	if !ok {
		return result
	}
	for _, item := range d.Items {
		key, ok := item.Key.(marshal.PyString)
		if !ok {
			continue
		}
		switch val := item.Value.(type) {
		case marshal.PyInt:
			result[key.Value] = int64(val.Value)
		case marshal.PyReal:
			result[key.Value] = val.Value
		}
	}
	return result
}

// handleGetCharactersToSelect returns a util.Rowset of every character on the logged-in account.
func handleGetCharactersToSelect(ctx context.Context, c *evenet.Conn, addr string, _ *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	result, err := evedb.GetCharacterList(ctx, sqldb, accountID)
	if err != nil {
		log.Printf("[%s] character.GetCharactersToSelect db: %v", addr, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	log.Printf("[%s] → character.GetCharactersToSelect (accountID=%d)", addr, accountID)
	pushPortraitFix(ctx, c, addr, accountID, sqldb)
	pushNPCChatter(c, addr, accountID)
	return server.SendCallRsp(c, pkt, accountID, result)
}

// handleGetCharacterToSelect returns a util.Rowset with selection-screen detail for one character.
func handleGetCharacterToSelect(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	if len(cs.ArgTuple.Items) < 1 {
		log.Printf("[%s] character.GetCharacterToSelect: missing characterID arg", addr)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	charID, ok := cs.ArgTuple.Items[0].(marshal.PyInt)
	if !ok {
		log.Printf("[%s] character.GetCharacterToSelect: arg[0] is %T, want PyInt", addr, cs.ArgTuple.Items[0])
		return server.SendCallException(c, pkt, accountID, nil)
	}
	result, err := evedb.GetCharSelectInfo(ctx, sqldb, int64(charID.Value))
	if err != nil {
		log.Printf("[%s] character.GetCharacterToSelect db: %v", addr, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	log.Printf("[%s] → character.GetCharacterToSelect charID=%d", addr, charID.Value)
	return server.SendCallRsp(c, pkt, accountID, result)
}

// handleSelectCharacterID loads the character, sends the session changes and returns a NULL
// CallRsp. See docs/server-notes.md#character-login-sequence.
func handleSelectCharacterID(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, charID *int64, stationID *int64, sqldb *sql.DB, sessReg *esp.SessionRegistry, stationReg *station.StationRegistry) error {
	if len(cs.ArgTuple.Items) < 1 {
		log.Printf("[%s] character.SelectCharacterID: missing characterID arg", addr)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	charIDArg, ok := cs.ArgTuple.Items[0].(marshal.PyInt)
	if !ok {
		log.Printf("[%s] character.SelectCharacterID: arg[0] is %T, want PyInt", addr, cs.ArgTuple.Items[0])
		return server.SendCallException(c, pkt, accountID, nil)
	}
	// The client sends only the ID, so the account is checked here: a character on another
	// account is refused before anything is claimed or loaded.
	if accountID <= 0 || !evedb.CharacterBelongsToAccount(ctx, sqldb, int64(charIDArg.Value), accountID) {
		log.Printf("[%s] character.SelectCharacterID: charID %d is not on account %d — refused",
			addr, charIDArg.Value, accountID)
		return server.SendCallException(c, pkt, accountID, marshal.PyString{Value: "That character is not on this account"})
	}
	// Reject duplicate logins: ClaimCharID is atomic, so two connections cannot both win.
	if sessReg != nil {
		if !sessReg.ClaimCharID(addr, int64(charIDArg.Value)) {
			log.Printf("[%s] character.SelectCharacterID: charID %d already logged in elsewhere",
				addr, charIDArg.Value)
			return server.SendCallException(c, pkt, accountID, marshal.PyString{Value: "This character is already logged in"})
		}
	}
	// modapi.FireCharacterSelectGate lets a module veto selection (rolebots refuses possessed bot
	// characters).
	if err := modapi.FireCharacterSelectGate(ctx, sqldb, int64(charIDArg.Value)); err != nil {
		log.Printf("[%s] character.SelectCharacterID: charID %d refused by module gate: %v",
			addr, charIDArg.Value, err)
		return server.SendCallException(c, pkt, accountID, marshal.PyString{Value: err.Error()})
	}
	// Store the selected charID in the connection state for later service calls.
	if charID != nil {
		*charID = int64(charIDArg.Value)
	}

	// stationID is set below once LoadCharData succeeds.

	cd, err := evedb.LoadCharData(ctx, sqldb, int64(charIDArg.Value))
	if err != nil {
		log.Printf("[%s] character.SelectCharacterID LoadCharData: %v", addr, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}

	// Per-connection stationID, for the no-arg GetStationItemBits the client sends on station entry.
	if stationID != nil {
		*stationID = cd.StationID
	}

	// Register the character in the station registry for GetGuests.
	if stationReg != nil && cd.StationID != 0 {
		stationReg.Join(c, cd.StationID, int64(charIDArg.Value))
	}

	shipID, err := evedb.GetCurrentShipID(ctx, sqldb, int64(charIDArg.Value))
	if err != nil {
		log.Printf("[%s] character.SelectCharacterID GetCurrentShipID: %v (continuing)", addr, err)
	}

	// Seed a rookie ship when the station holds no non-capsule ship for the character.
	if cd.StationID != 0 {
		if rookieID, err := evedb.EnsureRookieShip(ctx, sqldb, int64(charIDArg.Value), cd.StationID); err != nil {
			log.Printf("[%s] character.SelectCharacterID EnsureRookieShip: %v", addr, err)
		} else if rookieID != 0 {
			log.Printf("[%s] character.SelectCharacterID: seeded rookie ship %d", addr, rookieID)
		}
	}

	corp, err := evedb.GetCorporationMemberInfo(ctx, sqldb, int64(charIDArg.Value))
	if err != nil {
		log.Printf("[%s] character.SelectCharacterID GetCorporationMemberInfo: %v (continuing)", addr, err)
		corp = &evedb.CorpMemberInfo{}
	}

	// The CallRsp goes first: the client holds the session locked until it resolves, and a
	// SessionChangeNotification arriving earlier is rejected by LockSession.
	if err := server.SendCallRsp(c, pkt, accountID, marshal.PyNone{}); err != nil {
		return err
	}

	// One SessionChangeNotification carries corp roles, character and location. A change that
	// leaves charid None sends the client back to character selection or the intro. See
	// docs/server-notes.md#character-login-sequence.
	if err := sendCharSelectSessionChange(c, addr, corp, cd, shipID); err != nil {
		return err
	}

	// OnSkillStartTraining is sent after a delay, below. Backfill learning attributes 164-168 first:
	// without them godma's GetTimeForTraining divides by zero on every skill right-click.
	if n, lerr := evedb.EnsureCharLearningAttrs(ctx, sqldb, int64(charIDArg.Value), 20); lerr != nil {
		log.Printf("[%s] SelectCharacterID EnsureCharLearningAttrs: %v", addr, lerr)
	} else if n > 0 {
		log.Printf("[%s] SelectCharacterID backfilled %d missing learning attributes", addr, n)
	}

	// AdvanceSkillQueue, not bare CompleteTrainingIfDue: completing a due training deletes the
	// endOfTraining row the next queue entry needs as its backdated start. DB-only, so safe here.
	if _, cerr := evedb.AdvanceSkillQueue(ctx, sqldb, int64(charIDArg.Value)); cerr != nil {
		log.Printf("[%s] SelectCharacterID AdvanceSkillQueue: %v", addr, cerr)
	}
	// Clear flag=61 (in training) skills with no chrSkillTraining row; the character sheet shows
	// them training forever.
	if _, oerr := evedb.ClearOrphanTrainingFlags(ctx, sqldb, int64(charIDArg.Value)); oerr != nil {
		log.Printf("[%s] SelectCharacterID ClearOrphanTrainingFlags: %v", addr, oerr)
	}

	if st, err := evedb.GetSkillTraining(ctx, sqldb, int64(charIDArg.Value)); err == nil && st != nil {
		snapCharID := int64(charIDArg.Value)
		snapAccountID := accountID
		go func() {
			defer safego.Recover("handleSelectCharacterID")
			time.Sleep(10 * time.Second)
			// Delayed 10 s so skillMgr is running, or the notification is dropped. Re-read at send time:
			// the row may have completed. See docs/server-notes.md#character-login-sequence.
			fctx, fcancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer fcancel()
			cur, rerr := evedb.GetSkillTraining(fctx, sqldb, snapCharID)
			if rerr != nil {
				log.Printf("[%s] sendOnSkillStartTraining (delayed) re-read: %v", addr, rerr)
				return
			}
			if cur == nil {
				log.Printf("[%s] sendOnSkillStartTraining (delayed) skipped — training already completed", addr)
				return
			}
			if err := sendOnSkillStartTraining(c, addr, snapAccountID, snapCharID, cur.SkillItemID, cur.EndOfTraining); err != nil {
				log.Printf("[%s] sendOnSkillStartTraining (delayed): %v", addr, err)
			}
		}()
	}
	var skillID int64

	log.Printf("[%s] → character.SelectCharacterID charID=%d ship=%d skill=%d (OnSkillStartTraining in 10s)", addr, charIDArg.Value, shipID, skillID)
	return nil
}

// sendSessionChangeNotification sends one SessionChangeNotification with the given changes dict.
func sendSessionChangeNotification(c *evenet.Conn, addr string, changes marshal.PyDict, hasChar bool) error {
	nodesOfInterest := marshal.PyList{}
	if hasChar {
		nodesOfInterest.Items = []marshal.PyRep{marshal.PyInt{Value: globalNodeID}}
	}

	payload := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: 0}, // always 0
			changes,
		}},
		nodesOfInterest,
	}}

	pkt := &evenet.PyPacket{
		TypeString: "macho.SessionChangeNotification",
		Type:       evenet.SessionChangeNotification,
		Source: evenet.PyAddress{
			Type:   evenet.AddrNode,
			TypeID: globalNodeID,
		},
		Dest: evenet.PyAddress{
			Type:   evenet.AddrClient,
			TypeID: hackFixedClientID,
		},
		UserID:  0,
		Payload: payload,
		NamedPayload: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "channel"}, Value: marshal.PyString{Value: "sessionchange"}},
		}},
	}

	b, err := marshal.Marshal(pkt.Encode())
	if err != nil {
		return err
	}
	return c.SendPacket(b)
}

// charSelectSessionChanges builds the character-select session change: corp roles, character
// identity and location in one dict. charid must stay in the same change as every other field:
// the client treats any change that leaves charid None as "go to character selection".
func charSelectSessionChanges(corp *evedb.CorpMemberInfo, cd *evedb.CharData, shipID int64) marshal.PyDict {
	none := marshal.PyNone{}
	change := func(newVal marshal.PyRep) marshal.PyTuple {
		return marshal.PyTuple{Items: []marshal.PyRep{none, newVal}}
	}

	entries := []marshal.DictEntry{
		{Key: marshal.PyString{Value: "corprole"}, Value: change(marshal.PyInt{Value: corp.Corprole})},
		{Key: marshal.PyString{Value: "rolesAtAll"}, Value: change(marshal.PyInt{Value: corp.RolesAtAll})},
		{Key: marshal.PyString{Value: "rolesAtBase"}, Value: change(marshal.PyInt{Value: corp.RolesAtBase})},
		{Key: marshal.PyString{Value: "rolesAtHQ"}, Value: change(marshal.PyInt{Value: corp.RolesAtHQ})},
		{Key: marshal.PyString{Value: "rolesAtOther"}, Value: change(marshal.PyInt{Value: corp.RolesAtOther})},
	}
	if corp.BaseID != 0 {
		// rolesAtBase apply only while stationid == session baseID (sessions.py SetAttributes), so the
		// member's assigned base must reach the session.
		entries = append(entries, marshal.DictEntry{
			Key: marshal.PyString{Value: "baseID"}, Value: change(marshal.PyInt{Value: uint64(corp.BaseID)}),
		})
	}

	entries = append(entries, []marshal.DictEntry{
		{Key: marshal.PyString{Value: "constellationid"}, Value: change(marshal.PyInt{Value: uint64(cd.ConstellationID)})},
		{Key: marshal.PyString{Value: "corpid"}, Value: change(marshal.PyInt{Value: uint64(cd.CorporationID)})},
		{Key: marshal.PyString{Value: "regionid"}, Value: change(marshal.PyInt{Value: uint64(cd.RegionID)})},
		// hqID is the corporation's own HQ: the client ORs rolesAtHQ into corprole only while
		// stationid == hqID.
		{Key: marshal.PyString{Value: "hqID"}, Value: change(marshal.PyInt{Value: uint64(cd.HQStationID)})},
		{Key: marshal.PyString{Value: "solarsystemid2"}, Value: change(marshal.PyInt{Value: uint64(cd.SolarSystemID)})},
		{Key: marshal.PyString{Value: "shipid"}, Value: change(marshal.PyInt{Value: uint64(shipID)})},
		{Key: marshal.PyString{Value: "charid"}, Value: change(marshal.PyInt{Value: uint64(cd.CharacterID)})},
		{Key: marshal.PyString{Value: "gangrole"}, Value: change(marshal.PyInt{Value: 0})},
	}...)
	if cd.AllianceID != 0 {
		entries = append(entries,
			marshal.DictEntry{Key: marshal.PyString{Value: "allianceid"}, Value: change(marshal.PyInt{Value: uint64(cd.AllianceID)})},
		)
	}

	if cd.StationID != 0 {
		entries = append(entries,
			marshal.DictEntry{Key: marshal.PyString{Value: "stationid"}, Value: change(marshal.PyInt{Value: uint64(cd.StationID)})},
			marshal.DictEntry{Key: marshal.PyString{Value: "locationid"}, Value: change(marshal.PyInt{Value: uint64(cd.StationID)})},
		)
	} else {
		entries = append(entries,
			marshal.DictEntry{Key: marshal.PyString{Value: "solarsystemid"}, Value: change(marshal.PyInt{Value: uint64(cd.SolarSystemID)})},
			marshal.DictEntry{Key: marshal.PyString{Value: "locationid"}, Value: change(marshal.PyInt{Value: uint64(cd.SolarSystemID)})},
		)
	}

	return marshal.PyDict{Items: entries}
}

// sendCharSelectSessionChange sends the one SessionChangeNotification of character selection.
func sendCharSelectSessionChange(c *evenet.Conn, addr string, corp *evedb.CorpMemberInfo, cd *evedb.CharData, shipID int64) error {
	log.Printf("[%s] → SessionChangeNotification (corp roles, charid=%d shipid=%d stationid=%d)",
		addr, cd.CharacterID, shipID, cd.StationID)
	return sendSessionChangeNotification(c, addr, charSelectSessionChanges(corp, cd, shipID), true)
}

// sendOnSkillStartTraining sends a macho.Notification for OnSkillStartTraining. Payload:
// ((0, SubStream((0, (1, (itemID, endOfTraining))))), None), where 1 is remoteObject.
func sendOnSkillStartTraining(c *evenet.Conn, addr string, accountID int64, charID int64, skillItemID int64, endOfTraining uint64) error {
	// args: (itemID, endOfTraining)
	args := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyInt{Value: uint64(skillItemID)},
		marshal.PyInt{Value: endOfTraining},
	}}

	// notification stream wrapper
	payload := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: 0},
			&marshal.PySubStream{Decoded: marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyInt{Value: 0},
				marshal.PyTuple{Items: []marshal.PyRep{
					marshal.PyInt{Value: 1}, // remoteObject
					args,
				}},
			}}},
		}},
		marshal.PyNone{},
	}}

	pkt := &evenet.PyPacket{
		TypeString: "macho.Notification",
		Type:       evenet.Notification,
		Source: evenet.PyAddress{
			Type:   evenet.AddrNode,
			TypeID: 111555, // separate node ID for notifications, not the session node
		},
		Dest: evenet.PyAddress{
			Type:        evenet.AddrBroadcast,
			Service:     "OnSkillStartTraining",
			BcastIDType: "charid",
		},
		UserID:  uint64(accountID),
		Payload: payload,
		// No named_payload "sn": missing means unsequenced delivery, while a constant sn stalls the
		// client's WaitForSequenceNumber for 60 s per notification once its counter passes it.
	}

	log.Printf("[%s] → OnSkillStartTraining charID=%d skillItemID=%d endOfTraining=%d",
		addr, charID, skillItemID, endOfTraining)
	b, err := marshal.Marshal(pkt.Encode())
	if err != nil {
		return err
	}
	return c.SendPacket(b)
}
