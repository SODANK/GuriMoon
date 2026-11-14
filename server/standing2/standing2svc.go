// Package standing2 implements the "standing2" service: kill rights, standings, NPC
// standings, security rating and standing transactions. Personal standings live in
// chrStandings, corporation standings (set by directors) in crpStandings.
package standing2

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"time"

	"gurimoon/corp"
	evedb "gurimoon/db"
	"gurimoon/logx"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
	"gurimoon/server/infocache"
)

// Handle dispatches a CallReq addressed to the "standing2" service.
func Handle(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, charID int64, sqldb *sql.DB) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	switch cs.Method {
	case "GetMyKillRights":
		return handleGetMyKillRights(ctx, c, addr, pkt, accountID, charID, sqldb)
	case "GetMyStandings":
		return handleGetMyStandings(ctx, c, addr, pkt, accountID, charID, sqldb)
	case "GetNPCNPCStandings":
		return handleGetNPCNPCStandings(ctx, c, addr, pkt, accountID, sqldb)
	case "GetNPCStandingsTo":
		return handleGetNPCStandingsTo(ctx, c, addr, cs, pkt, accountID, sqldb)
	case "GetSecurityRating":
		return handleGetSecurityRating(ctx, c, addr, cs, pkt, accountID, charID, sqldb)
	case "GetStandingTransactions":
		return handleGetStandingTransactions(ctx, c, addr, cs, pkt, accountID, sqldb)
	case "GetRelationships":
		return handleGetRelationships(c, addr, pkt, accountID)
	case "SetPlayerStanding":
		return handleSetStanding(ctx, c, addr, cs, pkt, accountID, charID, charID, sqldb)
	case "SetCorpStanding":
		return handleSetCorpStanding(ctx, c, addr, cs, pkt, accountID, charID, sqldb)
	case "GetStandingCompositions":
		return handleGetStandingCompositions(c, addr, cs, pkt, accountID)
	default:
		server.RecordUnhandled(addr, "standing2", cs, true)
		log.Printf("[%s] standing2: unhandled method %q — returning None", addr, cs.Method)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
}

// handleGetRelationships returns an empty relationships rowset; no war or standing
// relationships are served here. The client renders overview entries without colours.
func handleGetRelationships(c *evenet.Conn, addr string, pkt *evenet.PyPacket, accountID int64) error {
	log.Printf("[%s] → standing2.GetRelationships (empty)", addr)
	result := emptyStandingsRowset([]string{"fromID", "toID", "relationship"})
	return server.SendCallRsp(c, pkt, accountID, result)
}

// KillRightsProvider is set by cmd/gurimoon/killrights.go. GetMyKillRights calls it
// for the client's (killRightsCache, killedRightsCache) pair, rights held and rights
// held against. Nil answers two empty dicts.
var KillRightsProvider func(ctx context.Context, sqldb *sql.DB, charID int64) marshal.PyRep

// handleGetMyKillRights serves consider.PrimeKillRights.
func handleGetMyKillRights(ctx context.Context, c *evenet.Conn, addr string, pkt *evenet.PyPacket, accountID, charID int64, sqldb *sql.DB) error {
	var result marshal.PyRep = marshal.PyTuple{Items: []marshal.PyRep{marshal.PyDict{}, marshal.PyDict{}}}
	if KillRightsProvider != nil {
		result = KillRightsProvider(ctx, sqldb, charID)
	}
	log.Printf("[%s] → standing2.GetMyKillRights charID=%d", addr, charID)
	return server.SendCallRsp(c, pkt, accountID, result)
}

// handleGetMyStandings returns a 6-tuple of standings data.
//
//	[0] charStandings:    util.Rowset (toID, standing)             from chrStandings
//	[1] charPrime:        util.Rowset (ownerID, ownerName, typeID) always empty
//	[2] npcCharStandings: util.Rowset (fromID, standing)           from chrNPCStandings
//	[3] corpStandings:    util.Rowset (toID, standing)             from crpStandings
//	[4] corpPrime:        PyDict{}, falsy so the client skips its Hint
//	[5] npcCorpStandings: PyDict{}, no NPC to player-corp standings are tracked
//
// corpPrime stays empty: eveowners.Hint(None, rowset) needs row count = header length.
func handleGetMyStandings(ctx context.Context, c *evenet.Conn, addr string, pkt *evenet.PyPacket, accountID int64, charID int64, sqldb *sql.DB) error {
	var charStandings, npcCharStandings marshal.PyRep
	var corpStandings marshal.PyRep = marshal.PyDict{}

	if charID != 0 {
		if corpID, err := evedb.GetCharCorpID(ctx, sqldb, charID); err == nil && corpID != 0 {
			if rs, err := evedb.GetCorpStandings(ctx, sqldb, corpID); err != nil {
				log.Printf("[%s] standing2.GetMyStandings GetCorpStandings corp=%d: %v", addr, corpID, err)
			} else {
				corpStandings = rs
			}
		}
		cs, err := evedb.GetCharStandings(ctx, sqldb, charID)
		if err != nil {
			log.Printf("[%s] standing2.GetMyStandings GetCharStandings: %v", addr, err)
			cs = emptyStandingsRowset([]string{"toID", "standing"})
		}
		charStandings = cs

		npcCharStandings = buildNPCCharStandings(ctx, sqldb, charID, addr)
		recordHeldNPCStandings(charID, npcCharStandings)
	} else {
		charStandings = emptyStandingsRowset([]string{"toID", "standing"})
		npcCharStandings = emptyStandingsRowset([]string{"fromID", "standing"})
	}

	// charPrime is always empty.
	charPrime := emptyStandingsRowset([]string{"ownerID", "ownerName", "typeID"})

	result := marshal.PyTuple{Items: []marshal.PyRep{
		charStandings,
		charPrime,
		npcCharStandings,
		corpStandings,
		marshal.PyDict{}, // corpPrime
		marshal.PyDict{}, // npcCorpStandings
	}}
	log.Printf("[%s] → standing2.GetMyStandings charID=%d", addr, charID)
	return server.SendCallRsp(c, pkt, accountID, result)
}

// handleGetNPCNPCStandings returns every NPC-to-NPC standing from the npcStandings table.
func handleGetNPCNPCStandings(ctx context.Context, c *evenet.Conn, addr string, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	result, err := evedb.GetNPCStandings(ctx, sqldb)
	if err != nil {
		log.Printf("[%s] standing2.GetNPCNPCStandings: %v", addr, err)
		result = emptyStandingsRowset([]string{"fromID", "toID", "standing"})
	}
	log.Printf("[%s] → standing2.GetNPCNPCStandings", addr)
	return server.SendCallRsp(c, pkt, accountID, result)
}

// handleGetNPCStandingsTo returns every NPC entity's standing toward the entity in
// arg[0]. standingsvc.py iterates the result without a None check, so a failed lookup
// still yields a rowset.
func handleGetNPCStandingsTo(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	var toID int64
	if len(cs.ArgTuple.Items) >= 1 {
		if v, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
			toID = int64(v.Value)
		}
	}
	result, err := evedb.GetNPCStandingsTo(ctx, sqldb, toID)
	if err != nil {
		log.Printf("[%s] standing2.GetNPCStandingsTo toID=%d: %v", addr, toID, err)
		result = emptyStandingsRowset([]string{"fromID", "standing"})
	}
	log.Printf("[%s] → standing2.GetNPCStandingsTo toID=%d", addr, toID)
	return server.SendCallRsp(c, pkt, accountID, result)
}

// handleGetSecurityRating reads a character's security rating. infosvc.py passes the
// character being inspected; that argument wins over the session character.
func handleGetSecurityRating(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, charID int64, sqldb *sql.DB) error {
	targetID := charID
	if len(cs.ArgTuple.Items) >= 1 {
		if v, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok && v.Value != 0 {
			targetID = int64(v.Value)
		}
	}
	// Memoized per pilot. A miss is not cached (see infocache.Get), so the fall-back
	// to 0.0 below is a per-call decision.
	rep, err := infocache.Shared.Get("standing2.GetSecurityRating", targetID, infocache.TTLIdentity,
		func() (marshal.PyRep, error) {
			var rating float64
			if err := sqldb.QueryRowContext(ctx,
				`SELECT securityRating FROM character_ WHERE characterID = ?`, targetID,
			).Scan(&rating); err != nil {
				return nil, err
			}
			return marshal.PyReal{Value: rating}, nil
		})
	if err != nil {
		log.Printf("[%s] → standing2.GetSecurityRating charID=%d err: %v", addr, targetID, err)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyReal{Value: 0.0})
	}
	logx.Debugf("[%s] → standing2.GetSecurityRating charID=%d", addr, targetID)
	return server.SendCallRsp(c, pkt, accountID, rep)
}

// handleGetStandingTransactions returns a rowset of standing-change history. Args:
// (fromID, toID, direction, eventID, eventTypeID, eventDateTime); the last three are
// optional filters (None = unfiltered) and direction 1 pages backward in time.
func handleGetStandingTransactions(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	argInt := func(i int) int64 {
		if len(cs.ArgTuple.Items) > i {
			if v, ok := cs.ArgTuple.Items[i].(marshal.PyInt); ok {
				return int64(v.Value)
			}
		}
		return 0
	}
	argIntPtr := func(i int) *int64 {
		if len(cs.ArgTuple.Items) > i {
			if v, ok := cs.ArgTuple.Items[i].(marshal.PyInt); ok {
				n := int64(v.Value)
				return &n
			}
		}
		return nil
	}
	fromID := argInt(0)
	toID := argInt(1)
	direction := argInt(2)

	result, err := evedb.GetStandingTransactions(ctx, sqldb, fromID, toID, direction,
		argIntPtr(3), argIntPtr(4), argIntPtr(5))
	if err != nil {
		log.Printf("[%s] standing2.GetStandingTransactions: %v", addr, err)
		result = emptyStandingTransactionsRowset()
	}
	log.Printf("[%s] → standing2.GetStandingTransactions fromID=%d toID=%d dir=%d", addr, fromID, toID, direction)
	return server.SendCallRsp(c, pkt, accountID, result)
}

// handleSetStanding stores fromID's standing toward the entity in arg[0]. Wire args:
// (toID, standing, reason); standing is absolute in [-10, +10] and 0 removes it.
// The client ignores the return and gets no OnStandingSet push (see below).
func handleSetStanding(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID, fromID int64, sqldb *sql.DB) error {
	if fromID == 0 || len(cs.ArgTuple.Items) < 2 {
		log.Printf("[%s] standing2.%s: missing owner or args", addr, cs.Method)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	toIDRep, ok := cs.ArgTuple.Items[0].(marshal.PyInt)
	if !ok {
		log.Printf("[%s] standing2.%s: arg[0] is %T, want PyInt", addr, cs.Method, cs.ArgTuple.Items[0])
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	toID := int64(toIDRep.Value)
	var standing float64
	switch v := cs.ArgTuple.Items[1].(type) {
	case marshal.PyReal:
		standing = v.Value
	case marshal.PyInt:
		standing = float64(int64(v.Value))
	}
	if standing < -10 {
		standing = -10
	} else if standing > 10 {
		standing = 10
	}
	if err := evedb.SetPersonalStanding(ctx, sqldb, fromID, toID, standing); err != nil {
		log.Printf("[%s] standing2.%s fromID=%d toID=%d: %v", addr, cs.Method, fromID, toID, err)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	// No OnStandingSet push: the client's handler stores a bare list where every reader
	// wants a row, which breaks the standings tab until relog. See sendStandingNotification.
	log.Printf("[%s] → standing2.%s fromID=%d toID=%d standing=%.1f", addr, cs.Method, fromID, toID, standing)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// handleSetCorpStanding stores the caller's corporation's standing toward the owner in
// arg[0]; wire args (toID, standing, reason). The client only hides Set Standing from
// non-directors, so the Director/CEO gate is here. NPC corporations keep no standings.
// No OnStandingSet push. See docs/db-12-notes.md#corporation-standings.
func handleSetCorpStanding(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID int64, sqldb *sql.DB) error {
	deny := func(reason string) error {
		return server.SendCallException(c, pkt, accountID, server.BuildUserError("CrpAccessDenied",
			map[string]marshal.PyRep{"reason": marshal.PyString{Value: reason}}))
	}
	corpID, err := evedb.GetCharCorpID(ctx, sqldb, charID)
	if err != nil || corpID == 0 {
		log.Printf("[%s] standing2.SetCorpStanding: corp lookup charID=%d: %v", addr, charID, err)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	if npc, _ := evedb.IsNPCCorp(ctx, sqldb, corpID); npc {
		return deny("NPC corporations do not keep standings set by their members.")
	}
	if !mayManageCorpStandings(ctx, sqldb, corpID, charID) {
		return deny("Only directors can set corporation standings.")
	}
	if len(cs.ArgTuple.Items) < 2 {
		log.Printf("[%s] standing2.SetCorpStanding: want (toID, standing, reason), got %d args", addr, len(cs.ArgTuple.Items))
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	toIDRep, ok := cs.ArgTuple.Items[0].(marshal.PyInt)
	if !ok {
		log.Printf("[%s] standing2.SetCorpStanding: arg[0] is %T, want PyInt", addr, cs.ArgTuple.Items[0])
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	toID := int64(toIDRep.Value)
	if toID == 0 || toID == corpID {
		return deny("A corporation cannot set a standing toward itself.")
	}
	var standing float64
	switch v := cs.ArgTuple.Items[1].(type) {
	case marshal.PyReal:
		standing = v.Value
	case marshal.PyInt:
		standing = float64(int64(v.Value))
	}
	var reason string
	if len(cs.ArgTuple.Items) >= 3 {
		if s, ok := cs.ArgTuple.Items[2].(marshal.PyString); ok {
			reason = s.Value
		}
	}

	switch err := evedb.SetCorpStanding(ctx, sqldb, corpID, toID, standing, charID, reason); {
	case err == nil:
	case errors.Is(err, evedb.ErrMaxCorpStandings):
		return server.SendCallException(c, pkt, accountID, server.BuildUserError("MaxStandingsSet", nil))
	default:
		log.Printf("[%s] standing2.SetCorpStanding corp=%d toID=%d: %v", addr, corpID, toID, err)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	log.Printf("[%s] → standing2.SetCorpStanding corp=%d toID=%d standing=%.1f by charID=%d", addr, corpID, toID, standing, charID)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// mayManageCorpStandings is the Director-or-CEO gate for corp standings.
func mayManageCorpStandings(ctx context.Context, sqldb *sql.DB, corpID, charID int64) bool {
	var ceoID int64
	if err := sqldb.QueryRowContext(ctx,
		`SELECT ceoID FROM corporation WHERE corporationID = ?`, corpID).Scan(&ceoID); err == nil && ceoID == charID {
		return true
	}
	info, err := evedb.GetCorporationMemberInfo(ctx, sqldb, charID)
	if err != nil {
		return false
	}
	return corp.HasRole(info.Corprole, corp.RoleDirector)
}

// handleGetStandingCompositions returns an empty rowset: per-agent composition is not
// tracked, and the client renders "No composition information available".
func handleGetStandingCompositions(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64) error {
	log.Printf("[%s] → standing2.GetStandingCompositions (empty)", addr)
	return server.SendCallRsp(c, pkt, accountID, emptyStandingsRowset([]string{"ownerID", "standing"}))
}

// emptyStandingsRowset builds an empty util.Rowset with the given column names.
func emptyStandingsRowset(cols []string) marshal.PyRep {
	header := make([]marshal.PyRep, len(cols))
	for i, c := range cols {
		header[i] = marshal.PyString{Value: c}
	}
	return marshal.PyObject{
		Type: "util.Rowset",
		Args: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "header"}, Value: marshal.PyList{Items: header}},
			{Key: marshal.PyString{Value: "RowClass"}, Value: marshal.PyString{Value: "util.Row", IsType1: true}},
			{Key: marshal.PyString{Value: "lines"}, Value: marshal.PyList{}},
		}},
	}
}

// buildNPCCharStandings returns a util.Rowset of NPC faction standings for the
// character plus a synthesized CONCORD row carrying securityRating, which the
// character sheet reads (standingsvc.GetMySecurityRating).
func buildNPCCharStandings(ctx context.Context, sqldb *sql.DB, charID int64, addr string) marshal.PyRep {
	result, err := evedb.GetCharNPCStandingsWithConcord(ctx, sqldb, charID)
	if err != nil {
		log.Printf("[%s] standing2.GetMyStandings GetCharNPCStandingsWithConcord: %v", addr, err)
		return emptyStandingsRowset([]string{"fromID", "standing"})
	}
	return result
}

// emptyStandingTransactionsRowset returns an empty rowset with the
// chrStandingTransactions column layout the client expects.
func emptyStandingTransactionsRowset() marshal.PyRep {
	cols := []string{"eventID", "eventTypeID", "eventDateTime", "fromID", "toID",
		"modification", "originalFromID", "originalToID", "int_1", "int_2", "int_3", "msg"}
	return emptyStandingsRowset(cols)
}
