// Package petitioner implements the client's "petitioner" service: the in-game
// help desk (ui/shared/neocom/petition.py).
//
// Every row goes out as a util.KeyVal the client attribute-reads. The GM side is
// gated on the account role (ROLE_PETITIONEE, 256, the bit the client tests for its
// queue tabs), looked up per account. Pushes:
//   - OnNewPetition(petitionID, charID, subject, categoryID) to online GMs;
//   - OnAssignPetition(petitionID, assigneeID) and OnClosePetition(petitionID) to the petitioner;
//   - OnPetitionMessage(petitionID, charID, text, messageID) to the other party
//     (never for a GM comment).
package petitioner

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sync"
	"time"

	evedb "gurimoon/db"
	"gurimoon/esp"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
)

// Install wires the panel's hook and the registry the pushes use. Called
// once at boot by the game server.
func Install(sessReg *esp.SessionRegistry, sqldb *sql.DB) {
	pushReg = sessReg
	esp.PetitionEventHook = func(event string, petitionID, charID int64, text string, messageID int64) {
		switch event {
		case "message":
			notifyChar(charID, "OnPetitionMessage", marshal.PyTuple{Items: []marshal.PyRep{
				pyInt(petitionID), pyInt(esp.PetitionAdminSenderID), marshal.PyString{Value: text}, pyInt(messageID)}})
		case "assign":
			notifyChar(charID, "OnAssignPetition", marshal.PyTuple{Items: []marshal.PyRep{pyInt(petitionID), pyInt(esp.PetitionAdminSenderID)}})
		case "close":
			notifyChar(charID, "OnClosePetition", marshal.PyTuple{Items: []marshal.PyRep{pyInt(petitionID)}})
		}
	}
}

var pushReg *esp.SessionRegistry

// ── role cache ──────────────────────────────────────────────────────────────

var (
	roleMu    sync.Mutex
	roleCache = map[int64]struct {
		role  int64
		until time.Time
	}{}
)

func accountRole(ctx context.Context, sqldb *sql.DB, accountID int64) int64 {
	roleMu.Lock()
	if e, ok := roleCache[accountID]; ok && time.Now().Before(e.until) {
		roleMu.Unlock()
		return e.role
	}
	roleMu.Unlock()
	role, err := evedb.GetAccountRole(ctx, sqldb, accountID)
	if err != nil {
		return 0
	}
	roleMu.Lock()
	roleCache[accountID] = struct {
		role  int64
		until time.Time
	}{role, time.Now().Add(time.Minute)}
	roleMu.Unlock()
	return role
}

// ── wire helpers ────────────────────────────────────────────────────────────

func pyInt(v int64) marshal.PyRep { return marshal.PyInt{Value: uint64(v)} }

func pyBoolInt(b bool) marshal.PyRep {
	if b {
		return marshal.PyInt{Value: 1}
	}
	return marshal.PyInt{Value: 0}
}

func pyOptInt(v int64) marshal.PyRep {
	if v == 0 {
		return marshal.PyNone{}
	}
	return pyInt(v)
}

func keyVal(cols []string, vals []marshal.PyRep) marshal.PyRep {
	items := make([]marshal.DictEntry, len(cols))
	for i, c := range cols {
		items[i] = marshal.DictEntry{Key: marshal.PyString{Value: c}, Value: vals[i]}
	}
	return marshal.PyObject{Type: "util.KeyVal", Args: marshal.PyDict{Items: items}}
}

// PetitionCols is every attribute petition.py reads off a petition row.
var PetitionCols = []string{
	"petitionID", "petitionerID", "subject", "categoryID", "petition", "properties",
	"claimed", "closed", "deleted", "escalatesTo", "createDate", "touchDate", "updated", "queueID", "claimedBy",
}

func petitionRow(p evedb.Petition) marshal.PyRep {
	var props marshal.PyRep = marshal.PyNone{}
	if len(p.Properties) > 0 {
		d := marshal.PyDict{}
		for k, v := range p.Properties {
			d.Items = append(d.Items, marshal.DictEntry{Key: marshal.PyString{Value: k}, Value: marshal.PyString{Value: v}})
		}
		props = d
	}
	return keyVal(PetitionCols, []marshal.PyRep{
		pyInt(p.PetitionID), pyInt(p.PetitionerID), marshal.PyString{Value: p.Subject}, pyInt(p.CategoryID),
		marshal.PyString{Value: p.Text}, props,
		pyBoolInt(p.ClaimedBy != 0), pyBoolInt(p.Closed), pyBoolInt(p.Deleted), pyOptInt(p.EscalatesTo),
		pyInt(evedb.UnixToWin32(p.CreateDate)), pyInt(evedb.UnixToWin32(p.TouchDate)), pyBoolInt(p.Updated),
		pyInt(p.QueueID), pyInt(p.ClaimedBy),
	})
}

func petitionRows(ps []evedb.Petition) marshal.PyRep {
	items := make([]marshal.PyRep, 0, len(ps))
	for _, p := range ps {
		items = append(items, petitionRow(p))
	}
	return marshal.PyList{Items: items}
}

var MessageCols = []string{"messageID", "petitionID", "senderID", "sentDate", "text", "comment"}

func messageRow(m evedb.PetitionMessage) marshal.PyRep {
	return keyVal(MessageCols, []marshal.PyRep{
		pyInt(m.MessageID), pyInt(m.PetitionID), pyInt(m.SenderID), pyInt(evedb.UnixToWin32(m.SentDate)),
		marshal.PyString{Value: m.Text}, pyBoolInt(m.Comment),
	})
}

var LogCols = []string{"logID", "petitionID", "logDateTime", "characterID", "userName", "eventText"}

func logRow(e evedb.PetitionLogEntry) marshal.PyRep {
	return keyVal(LogCols, []marshal.PyRep{
		pyInt(e.LogID), pyInt(e.PetitionID), pyInt(evedb.UnixToWin32(e.LogDateTime)), pyOptInt(e.CharacterID),
		marshal.PyString{Value: e.UserName}, marshal.PyString{Value: e.EventText},
	})
}

var CategoryCols = []string{"categoryID", "displayName", "description", "queueID", "escalatesTo"}

func categoryRow(c evedb.PetitionCategory) marshal.PyRep {
	return keyVal(CategoryCols, []marshal.PyRep{
		pyInt(c.CategoryID), marshal.PyString{Value: c.DisplayName}, marshal.PyString{Value: c.Description},
		pyInt(c.QueueID), pyOptInt(c.EscalatesTo),
	})
}

func notifyChar(charID int64, event string, args marshal.PyRep) {
	if pushReg == nil || charID == 0 {
		return
	}
	server.NotifyCharacter(pushReg, charID, event, "charid", args)
}

// notifyGMs tells every online petitionee.
func notifyGMs(ctx context.Context, sqldb *sql.DB, event string, args marshal.PyRep) {
	if pushReg == nil {
		return
	}
	for _, s := range pushReg.Snapshot() {
		if s.Conn == nil || s.CharID == 0 || !evedb.IsPetitionee(accountRole(ctx, sqldb, s.AccountID)) {
			continue
		}
		if err := server.SendNotification(s.Conn, s.AccountID, event, "charid", args); err != nil {
			log.Printf("[%s] petition push %s: %v", s.Addr, event, err)
		}
	}
}

func userErr(c *evenet.Conn, pkt *evenet.PyPacket, accountID int64, text string) error {
	return server.SendCallException(c, pkt, accountID, server.BuildUserError("CustomError",
		map[string]marshal.PyRep{"error": marshal.PyString{Value: text}}))
}

func argInt(cs *server.CallStream, i int) int64 {
	if i < len(cs.ArgTuple.Items) {
		if v, ok := cs.ArgTuple.Items[i].(marshal.PyInt); ok {
			return int64(v.Value)
		}
	}
	return 0
}

func argStr(cs *server.CallStream, i int) string {
	if i < len(cs.ArgTuple.Items) {
		if v, ok := cs.ArgTuple.Items[i].(marshal.PyString); ok {
			return v.Value
		}
	}
	return ""
}

func argBool(cs *server.CallStream, i int) bool {
	if i < len(cs.ArgTuple.Items) {
		switch v := cs.ArgTuple.Items[i].(type) {
		case marshal.PyBool:
			return v.Value
		case marshal.PyInt:
			return v.Value != 0
		}
	}
	return false
}

func kwStr(cs *server.CallStream, key string) string {
	if cs.ArgDict == nil {
		return ""
	}
	for _, e := range cs.ArgDict.Items {
		if k, ok := e.Key.(marshal.PyString); ok && k.Value == key {
			if v, ok := e.Value.(marshal.PyString); ok {
				return v.Value
			}
		}
	}
	return ""
}

// ── service ─────────────────────────────────────────────────────────────────

// Handle dispatches a CallReq addressed to the "petitioner" service.
func Handle(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID int64, sqldb *sql.DB) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	none := marshal.PyNone{}
	if sqldb == nil {
		return server.SendCallRsp(c, pkt, accountID, marshal.PyList{})
	}
	isGM := evedb.IsPetitionee(accountRole(ctx, sqldb, accountID))
	gmOnly := func() error {
		return userErr(c, pkt, accountID, "You do not have the petitionee role.")
	}
	load := func(id int64) (*evedb.Petition, error) {
		p, err := evedb.GetPetition(ctx, sqldb, id)
		if err != nil {
			return nil, err
		}
		if p == nil {
			return nil, fmt.Errorf("petition %d not found", id)
		}
		return p, nil
	}

	switch cs.Method {

	// ── everyone ─────────────────────────────────────────────────────────
	case "GetCategories":
		cats, err := evedb.ListPetitionCategories(ctx, sqldb)
		if err != nil {
			log.Printf("[%s] petitioner.GetCategories: %v", addr, err)
		}
		items := make([]marshal.PyRep, 0, len(cats))
		for _, cat := range cats {
			items = append(items, categoryRow(cat))
		}
		return server.SendCallRsp(c, pkt, accountID, marshal.PyList{Items: items})

	case "GetCategoryName":
		cat, _ := evedb.GetPetitionCategory(ctx, sqldb, argInt(cs, 0))
		if cat == nil {
			return server.SendCallRsp(c, pkt, accountID, marshal.PyString{Value: "Unknown"})
		}
		return server.SendCallRsp(c, pkt, accountID, marshal.PyString{Value: cat.DisplayName})

	case "GetCategoryProperties":
		// Rows with .k, one extra form field each.
		cat, _ := evedb.GetPetitionCategory(ctx, sqldb, argInt(cs, 0))
		var items []marshal.PyRep
		if cat != nil {
			for _, k := range cat.Properties {
				items = append(items, keyVal([]string{"k"}, []marshal.PyRep{marshal.PyString{Value: k}}))
			}
		}
		return server.SendCallRsp(c, pkt, accountID, marshal.PyList{Items: items})

	case "MayPetition":
		categoryID, ooc := argInt(cs, 0), argInt(cs, 1)
		petitioner := charID
		if ooc != 0 {
			if !evedb.CharacterBelongsToAccount(ctx, sqldb, ooc, accountID) {
				return server.SendCallRsp(c, pkt, accountID, marshal.PyInt{Value: uint64(1<<64 - 4)})
			}
			petitioner = ooc
		}
		if petitioner == 0 {
			return server.SendCallRsp(c, pkt, accountID, marshal.PyInt{Value: uint64(1<<64 - 3)})
		}
		code, err := evedb.MayPetition(ctx, sqldb, petitioner, categoryID, time.Now())
		if err != nil {
			log.Printf("[%s] petitioner.MayPetition: %v", addr, err)
			code = 1
		}
		return server.SendCallRsp(c, pkt, accountID, marshal.PyInt{Value: uint64(code)})

	case "CreatePetition":
		// (subject, text, categoryID, properties, oocCharID, combatLog=, chatLog=)
		subject, text, categoryID, ooc := argStr(cs, 0), argStr(cs, 1), argInt(cs, 2), argInt(cs, 4)
		petitioner := charID
		if ooc != 0 {
			if !evedb.CharacterBelongsToAccount(ctx, sqldb, ooc, accountID) {
				return server.SendCallRsp(c, pkt, accountID, marshal.PyBool{Value: false})
			}
			petitioner = ooc
		}
		if petitioner == 0 || subject == "" {
			return server.SendCallRsp(c, pkt, accountID, marshal.PyBool{Value: false})
		}
		cat, _ := evedb.GetPetitionCategory(ctx, sqldb, categoryID)
		if cat == nil {
			return userErr(c, pkt, accountID, "That petition category does not exist.")
		}
		if code, _ := evedb.MayPetition(ctx, sqldb, petitioner, categoryID, time.Now()); code != 1 {
			return server.SendCallRsp(c, pkt, accountID, marshal.PyBool{Value: false})
		}
		props := map[string]string{}
		if len(cs.ArgTuple.Items) > 3 {
			if d, ok := cs.ArgTuple.Items[3].(marshal.PyDict); ok {
				for _, e := range d.Items {
					k, kok := e.Key.(marshal.PyString)
					v, vok := e.Value.(marshal.PyString)
					if kok && vok {
						props[k.Value] = v.Value
					}
				}
			}
		}
		if len(text) > 1000 {
			text = text[:1000]
		}
		id, err := evedb.CreatePetition(ctx, sqldb, evedb.Petition{
			PetitionerID: petitioner, OocCharID: ooc, AccountID: accountID, Subject: subject, Text: text,
			CategoryID: categoryID, Properties: props, CombatLog: kwStr(cs, "combatLog"), ChatLog: kwStr(cs, "chatLog"),
			QueueID: cat.QueueID, EscalatesTo: cat.EscalatesTo,
		})
		if err != nil {
			log.Printf("[%s] petitioner.CreatePetition: %v", addr, err)
			return server.SendCallRsp(c, pkt, accountID, marshal.PyBool{Value: false})
		}
		evedb.AddPetitionLog(ctx, sqldb, id, petitioner, "", "Petition created")
		log.Printf("[%s] → petitioner.CreatePetition #%d by char %d: %q (category %d)", addr, id, petitioner, subject, categoryID)
		if err := server.SendCallRsp(c, pkt, accountID, marshal.PyBool{Value: true}); err != nil {
			return err
		}
		notifyGMs(ctx, sqldb, "OnNewPetition", marshal.PyTuple{Items: []marshal.PyRep{
			pyInt(id), pyInt(petitioner), marshal.PyString{Value: subject}, pyInt(categoryID)}})
		return nil

	case "GetMyPetitions":
		ps, err := evedb.ListMyPetitions(ctx, sqldb, charID)
		if err != nil {
			log.Printf("[%s] petitioner.GetMyPetitions: %v", addr, err)
		}
		return server.SendCallRsp(c, pkt, accountID, petitionRows(ps))

	case "GetPetition":
		p, err := load(argInt(cs, 0))
		if err != nil {
			return userErr(c, pkt, accountID, err.Error())
		}
		if p.PetitionerID != charID && !isGM {
			return gmOnly()
		}
		return server.SendCallRsp(c, pkt, accountID, petitionRow(*p))

	case "GetPetitionMessages":
		p, err := load(argInt(cs, 0))
		if err != nil {
			return userErr(c, pkt, accountID, err.Error())
		}
		if p.PetitionerID != charID && !isGM {
			return gmOnly()
		}
		msgs, err := evedb.ListPetitionMessages(ctx, sqldb, p.PetitionID, isGM)
		if err != nil {
			log.Printf("[%s] petitioner.GetPetitionMessages: %v", addr, err)
		}
		items := make([]marshal.PyRep, 0, len(msgs))
		for _, m := range msgs {
			items = append(items, messageRow(m))
		}
		return server.SendCallRsp(c, pkt, accountID, marshal.PyList{Items: items})

	case "GetUnreadMessages":
		msgs, err := evedb.ListUnreadPetitionMessages(ctx, sqldb, charID)
		if err != nil {
			log.Printf("[%s] petitioner.GetUnreadMessages: %v", addr, err)
		}
		items := make([]marshal.PyRep, 0, len(msgs))
		for _, m := range msgs {
			items = append(items, messageRow(m))
		}
		return server.SendCallRsp(c, pkt, accountID, marshal.PyList{Items: items})

	case "MarkAsRead":
		evedb.MarkPetitionMessageRead(ctx, sqldb, argInt(cs, 0))
		return server.SendCallRsp(c, pkt, accountID, none)

	case "PetitionerChat":
		p, err := load(argInt(cs, 0))
		if err != nil {
			return userErr(c, pkt, accountID, err.Error())
		}
		if p.PetitionerID != charID {
			return userErr(c, pkt, accountID, "That is not your petition.")
		}
		if p.Closed {
			return server.SendCallException(c, pkt, accountID, server.BuildUserError("PetitionClosed", nil))
		}
		text := argStr(cs, 1)
		if text == "" {
			return server.SendCallRsp(c, pkt, accountID, none)
		}
		mid, err := evedb.AddPetitionMessage(ctx, sqldb, p.PetitionID, charID, text, false)
		if err != nil {
			return userErr(c, pkt, accountID, "The message could not be saved.")
		}
		evedb.SetPetitionUpdated(ctx, sqldb, p.PetitionID, true)
		evedb.AddPetitionLog(ctx, sqldb, p.PetitionID, charID, "", "Petitioner wrote")
		if err := server.SendCallRsp(c, pkt, accountID, none); err != nil {
			return err
		}
		args := marshal.PyTuple{Items: []marshal.PyRep{pyInt(p.PetitionID), pyInt(charID), marshal.PyString{Value: text}, pyInt(mid)}}
		if p.ClaimedBy != 0 {
			notifyChar(p.ClaimedBy, "OnPetitionMessage", args)
		} else {
			notifyGMs(ctx, sqldb, "OnPetitionMessage", args)
		}
		return nil

	case "CancelPetition":
		ok, err := evedb.CancelPetition(ctx, sqldb, argInt(cs, 0), charID)
		if err != nil {
			return userErr(c, pkt, accountID, "The petition could not be cancelled.")
		}
		if !ok {
			return userErr(c, pkt, accountID, "A claimed petition can only be closed by the GM working it.")
		}
		evedb.AddPetitionLog(ctx, sqldb, argInt(cs, 0), charID, "", "Cancelled by petitioner")
		return server.SendCallRsp(c, pkt, accountID, none)

	case "DeletePetition":
		ok, _ := evedb.DeletePetition(ctx, sqldb, argInt(cs, 0), charID)
		return server.SendCallRsp(c, pkt, accountID, pyBoolInt(ok))

	// ── GM desk ──────────────────────────────────────────────────────────
	case "GetQueues":
		if !isGM {
			return server.SendCallRsp(c, pkt, accountID, marshal.PyList{})
		}
		qs, err := evedb.ListPetitionQueues(ctx, sqldb)
		if err != nil {
			log.Printf("[%s] petitioner.GetQueues: %v", addr, err)
		}
		items := make([]marshal.PyRep, 0, len(qs))
		for _, q := range qs {
			items = append(items, keyVal([]string{"queueID", "queueName", "escalatesTo"},
				[]marshal.PyRep{pyInt(q.QueueID), marshal.PyString{Value: q.QueueName}, pyOptInt(q.EscalatesTo)}))
		}
		return server.SendCallRsp(c, pkt, accountID, marshal.PyList{Items: items})

	case "GetClaimedPetitions":
		if !isGM {
			return gmOnly()
		}
		ps, _ := evedb.ListClaimedPetitions(ctx, sqldb, charID)
		return server.SendCallRsp(c, pkt, accountID, petitionRows(ps))

	case "GetPetitionQueue":
		if !isGM {
			return gmOnly()
		}
		ps, _ := evedb.ListPetitionQueue(ctx, sqldb, argInt(cs, 0))
		return server.SendCallRsp(c, pkt, accountID, petitionRows(ps))

	case "GetPetitionLog":
		if !isGM {
			return gmOnly()
		}
		logs, _ := evedb.ListPetitionLog(ctx, sqldb, argInt(cs, 0))
		items := make([]marshal.PyRep, 0, len(logs))
		for _, e := range logs {
			items = append(items, logRow(e))
		}
		return server.SendCallRsp(c, pkt, accountID, marshal.PyList{Items: items})

	case "ClaimPetition":
		// Returns the petitionID when claimed now, 0 when somebody else has
		// it (the client tests > 0).
		if !isGM {
			return gmOnly()
		}
		id := argInt(cs, 0)
		ok, err := evedb.ClaimPetition(ctx, sqldb, id, charID)
		if err != nil || !ok {
			return server.SendCallRsp(c, pkt, accountID, marshal.PyInt{Value: 0})
		}
		evedb.AddPetitionLog(ctx, sqldb, id, charID, "", "Claimed")
		if p, _ := evedb.GetPetition(ctx, sqldb, id); p != nil {
			notifyChar(p.PetitionerID, "OnAssignPetition", marshal.PyTuple{Items: []marshal.PyRep{pyInt(id), pyInt(charID)}})
		}
		return server.SendCallRsp(c, pkt, accountID, pyInt(id))

	case "UnClaimPetition":
		if !isGM {
			return gmOnly()
		}
		id := argInt(cs, 0)
		_ = evedb.UnclaimPetition(ctx, sqldb, id)
		evedb.AddPetitionLog(ctx, sqldb, id, charID, "", "Unclaimed")
		if p, _ := evedb.GetPetition(ctx, sqldb, id); p != nil {
			notifyChar(p.PetitionerID, "OnAssignPetition", marshal.PyTuple{Items: []marshal.PyRep{pyInt(id), none}})
		}
		return server.SendCallRsp(c, pkt, accountID, none)

	case "EscalatePetition":
		if !isGM {
			return gmOnly()
		}
		id, to := argInt(cs, 0), argInt(cs, 1)
		if to == 0 {
			return userErr(c, pkt, accountID, "This petition cannot be escalated further.")
		}
		_ = evedb.EscalatePetition(ctx, sqldb, id, to)
		evedb.AddPetitionLog(ctx, sqldb, id, charID, "", fmt.Sprintf("Escalated to queue %d", to))
		return server.SendCallRsp(c, pkt, accountID, none)

	case "ClosePetition":
		if !isGM {
			return gmOnly()
		}
		id := argInt(cs, 0)
		_ = evedb.ClosePetition(ctx, sqldb, id, charID)
		evedb.AddPetitionLog(ctx, sqldb, id, charID, "", "Closed")
		if p, _ := evedb.GetPetition(ctx, sqldb, id); p != nil {
			notifyChar(p.PetitionerID, "OnClosePetition", marshal.PyTuple{Items: []marshal.PyRep{pyInt(id)}})
		}
		return server.SendCallRsp(c, pkt, accountID, none)

	case "PetitioneeChat":
		if !isGM {
			return gmOnly()
		}
		p, err := load(argInt(cs, 0))
		if err != nil {
			return userErr(c, pkt, accountID, err.Error())
		}
		if p.Closed {
			return server.SendCallException(c, pkt, accountID, server.BuildUserError("PetitionClosed", nil))
		}
		text, comment := argStr(cs, 1), argBool(cs, 2)
		if text == "" {
			return server.SendCallRsp(c, pkt, accountID, none)
		}
		mid, err := evedb.AddPetitionMessage(ctx, sqldb, p.PetitionID, charID, text, comment)
		if err != nil {
			return userErr(c, pkt, accountID, "The message could not be saved.")
		}
		if !comment {
			evedb.SetPetitionUpdated(ctx, sqldb, p.PetitionID, false)
		}
		evedb.AddPetitionLog(ctx, sqldb, p.PetitionID, charID, "", map[bool]string{true: "Comment added", false: "GM wrote"}[comment])
		if err := server.SendCallRsp(c, pkt, accountID, none); err != nil {
			return err
		}
		if !comment {
			notifyChar(p.PetitionerID, "OnPetitionMessage", marshal.PyTuple{Items: []marshal.PyRep{
				pyInt(p.PetitionID), pyInt(charID), marshal.PyString{Value: text}, pyInt(mid)}})
		}
		return nil

	default:
		server.RecordUnhandled(addr, "petitioner", cs, false)
		log.Printf("[%s] petitioner: unhandled method %q — sending exception", addr, cs.Method)
		return server.SendCallException(c, pkt, accountID, nil)
	}
}
