// Package gangsta implements the "gangsta" service (gang/fleet): membership, invites
// and gang queries on top of the in-memory gang.Registry. Method names follow the
// client (menusvc.py, lscengine.py, parklife/gang.py); the older short names (Leave,
// Kick, AcceptInvite) remain as aliases.
package gangsta

import (
	"log"

	"gurimoon/gang"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
)

// memberStateActive is the per-member state string parklife/gang.py stores; the
// client only compares it against 'Active'.
const memberStateActive = "Active"

// gangLeaderRole is const.gangLeaderRole. The client's leader-only menu options test
// eve.session.gangrole == const.gangLeaderRole, so gangrole must track leadership.
const gangLeaderRole = 1

// argInt extracts a PyInt from cs.ArgTuple at idx, or 0 when absent or not a PyInt.
func argInt(cs *server.CallStream, idx int) int64 {
	if idx >= len(cs.ArgTuple.Items) {
		return 0
	}
	if v, ok := cs.ArgTuple.Items[idx].(marshal.PyInt); ok {
		return int64(v.Value)
	}
	return 0
}

// ConnLookup resolves a character ID to its live connection and its own accountID,
// or (nil, 0). Cross-player notifications must carry the recipient's accountID as
// the packet userID, which the client applies without validating.
type ConnLookup func(int64) (*evenet.Conn, int64)

// NameLookup resolves a character ID to their name ("" if unknown).
type NameLookup func(int64) string

// SysNotify broadcasts a gang membership change to everyone in the character's solar
// system (OnCharGangChange / OnGangCorpChange). gangID 0 means left; corpID feeds the
// gangCorps insert on join (0 to skip).
type SysNotify func(charID, gangID, corpID int64)

// Handle dispatches a CallReq addressed to the "gangsta" service.
func Handle(
	c *evenet.Conn,
	addr string,
	cs *server.CallStream,
	pkt *evenet.PyPacket,
	accountID, charID, corpID int64,
	gangReg *gang.Registry,
	connLookup ConnLookup,
	nameOf NameLookup,
	sysNotify SysNotify,
) error {
	if gangReg == nil {
		log.Printf("[%s] → gangsta.%s (no registry, stub None)", addr, cs.Method)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	if nameOf == nil {
		nameOf = func(int64) string { return "" }
	}
	if sysNotify == nil {
		sysNotify = func(int64, int64, int64) {}
	}

	switch cs.Method {
	case "InviteCharacter":
		return handleInvite(c, addr, cs, pkt, accountID, charID, corpID, gangReg, connLookup, nameOf, sysNotify)

	case "AcceptInvitation", "AcceptInvite":
		return handleAccept(c, addr, cs, pkt, accountID, charID, corpID, gangReg, connLookup, sysNotify)

	case "RejectInvitation", "RejectInvite":
		gangID := argInt(cs, 0)
		if _, ok := gangReg.TakeInvitation(charID, gangID); ok {
			log.Printf("[%s] → gangsta.%s: %d rejected gang %d", addr, cs.Method, charID, gangID)
		}
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})

	case "LeaveGang", "Leave":
		return handleLeave(c, addr, cs, pkt, accountID, charID, gangReg, connLookup, sysNotify)

	case "DisbandGang", "Disband":
		return handleDisband(c, addr, cs, pkt, accountID, charID, gangReg, connLookup, sysNotify)

	case "KickMember", "Kick":
		return handleKick(c, addr, cs, pkt, accountID, charID, gangReg, connLookup, sysNotify)

	case "MakeLeader":
		return handleMakeLeader(c, addr, cs, pkt, accountID, charID, gangReg, connLookup)

	case "ClaimLeadership":
		gangID := gangReg.GetCharGang(charID)
		g := gangReg.GetGang(gangID)
		if g == nil || g.LeaderID != 0 {
			// Somebody already leads this gang; the client's menu predicate hides the option.
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		gangReg.SetLeader(charID)
		log.Printf("[%s] → gangsta.ClaimLeadership: %d now leads gang %d", addr, charID, gangID)
		sendGangRoleChange(c, accountID, 0, gangLeaderRole)
		broadcastGang(gangReg, gangID, connLookup, "OnGangLeader", marshal.PyTuple{
			Items: []marshal.PyRep{marshal.PyInt{Value: uint64(charID)}},
		})
		fireGangEvent(GangEvent{Kind: GangLeaderChanged, GangID: gangID, NewLeader: charID})
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})

	case "ResignLeadership":
		gangID := gangReg.ClearLeader(charID)
		if gangID == 0 {
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		log.Printf("[%s] → gangsta.ResignLeadership: gang %d has no leader", addr, gangID)
		sendGangRoleChange(c, accountID, gangLeaderRole, 0)
		broadcastGang(gangReg, gangID, connLookup, "OnGangLeader", marshal.PyTuple{
			Items: []marshal.PyRep{marshal.PyNone{}},
		})
		fireGangEvent(GangEvent{Kind: GangLeaderChanged, GangID: gangID, OldLeader: charID})
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})

	case "GetGangInfoForChar":
		return server.SendCallRsp(c, pkt, accountID, gangInfoForChar(charID, gangReg))

	case "GetGangInfo":
		gangID := gangReg.GetCharGang(charID)
		g := gangReg.GetGang(gangID)
		if g == nil {
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		result := marshal.PyObject{
			Type: "util.KeyVal",
			Args: marshal.PyDict{Items: []marshal.DictEntry{
				{Key: marshal.PyString{Value: "gangID"}, Value: marshal.PyInt{Value: uint64(g.ID)}},
				{Key: marshal.PyString{Value: "leaderID"}, Value: marshal.PyInt{Value: uint64(g.LeaderID)}},
			}},
		}
		return server.SendCallRsp(c, pkt, accountID, result)

	case "GetGangMembers":
		gangID := gangReg.GetCharGang(charID)
		g := gangReg.GetGang(gangID)
		if g == nil {
			return server.SendCallRsp(c, pkt, accountID, marshal.PyList{})
		}
		members := make([]marshal.PyRep, 0, len(g.Members))
		for _, m := range g.Members {
			members = append(members, marshal.PyObject{
				Type: "util.KeyVal",
				Args: marshal.PyDict{Items: []marshal.DictEntry{
					{Key: marshal.PyString{Value: "characterID"}, Value: marshal.PyInt{Value: uint64(m.CharID)}},
					{Key: marshal.PyString{Value: "corporationID"}, Value: marshal.PyInt{Value: uint64(m.CorpID)}},
				}},
			})
		}
		return server.SendCallRsp(c, pkt, accountID, marshal.PyList{Items: members})

	case "MakeBooster", "MoveCharacter", "AddToGang", "AssumeRole":
		log.Printf("[%s] → gangsta.%s (stub None)", addr, cs.Method)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})

	default:
		server.RecordUnhandled(addr, "gangsta", cs, true)
		log.Printf("[%s] gangsta: unhandled method %q — stub None", addr, cs.Method)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
}

// ─── method implementations ────────────────────────────────────────────────

func handleInvite(
	c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket,
	accountID, charID, corpID int64, gangReg *gang.Registry, connLookup ConnLookup,
	nameOf NameLookup, sysNotify SysNotify,
) error {
	targetCharID := argInt(cs, 0)
	if targetCharID == 0 {
		log.Printf("[%s] → gangsta.InviteCharacter: missing target", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}

	// A module may own this character (rolebots, modulegang.go); the probe is a cached
	// roster lookup, so a logged-off player falls through to the refusal below.
	if targetConn, _ := connLookup(targetCharID); targetConn == nil && ModuleOwnsCharacter(targetCharID) {
		return handleModuleInvite(c, addr, pkt, accountID, charID, corpID, targetCharID,
			gangReg, connLookup, nameOf, sysNotify)
	}

	// The client shows GangCandidateOffline as a UserError message box.
	targetConn, targetAcct := connLookup(targetCharID)
	if targetConn == nil {
		log.Printf("[%s] → gangsta.InviteCharacter: target %d offline", addr, targetCharID)
		return server.SendCallException(c, pkt, accountID, server.BuildUserError(
			"GangCandidateOffline", map[string]marshal.PyRep{
				"invitee": marshal.PyString{Value: nameOf(targetCharID)},
			}))
	}

	gangID := gangReg.GetCharGang(charID)
	newGang := gangID == 0
	if newGang {
		gangID = gangReg.CreateGang(charID, corpID)
		log.Printf("[%s] → gangsta.InviteCharacter: created gang %d for leader %d", addr, gangID, charID)
		// The inviter's own session must reflect the gang before parklife/gang.py accepts
		// gang state; forming a gang makes them leader, so gangrole changes too.
		sendGangSessionChange(c, accountID, 0, gangID, 0, gangLeaderRole)
		sysNotify(charID, gangID, corpID)
	}

	gangReg.AddInvitation(gangID, charID, targetCharID)

	// lscengine.OnGangInvite(leaderID, gangID, msgName, msgDict): the client renders
	// cfg.GetMessage(msgName, msgDict) as the accept/reject dialog.
	inviteArgs := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyInt{Value: uint64(charID)},
		marshal.PyInt{Value: uint64(gangID)},
		marshal.PyString{Value: "AskJoinGang"},
		marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "name"}, Value: marshal.PyString{Value: nameOf(charID)}},
		}},
	}}
	server.SendNotification(targetConn, targetAcct, "OnGangInvite", "charid", inviteArgs)

	log.Printf("[%s] → gangsta.InviteCharacter: invited %d to gang %d", addr, targetCharID, gangID)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

func handleAccept(
	c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket,
	accountID, charID, corpID int64, gangReg *gang.Registry, connLookup ConnLookup,
	sysNotify SysNotify,
) error {
	gangID := argInt(cs, 0)
	if gangID == 0 {
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	if _, ok := gangReg.TakeInvitation(charID, gangID); !ok {
		log.Printf("[%s] → gangsta.AcceptInvitation: no pending invite to gang %d for %d", addr, gangID, charID)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	if err := gangReg.AddMember(gangID, charID, corpID); err != nil {
		log.Printf("[%s] → gangsta.AcceptInvitation: %v", addr, err)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	gangReg.ClearInvitations(charID)

	// Tell the existing members before the joiner's session flips, so the joiner is
	// not counted twice by its own OnGangJoin.
	broadcastGangExcept(gangReg, gangID, charID, connLookup, "OnGangJoin", marshal.PyTuple{
		Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(charID)},
			marshal.PyString{Value: memberStateActive},
		},
	})
	sendGangSessionChange(c, accountID, 0, gangID, 0, 0)
	sysNotify(charID, gangID, corpID)

	log.Printf("[%s] → gangsta.AcceptInvitation: %d joined gang %d", addr, charID, gangID)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

func handleLeave(
	c *evenet.Conn, addr string, _ *server.CallStream, pkt *evenet.PyPacket,
	accountID, charID int64, gangReg *gang.Registry, connLookup ConnLookup,
	sysNotify SysNotify,
) error {
	gangID := gangReg.GetCharGang(charID)
	if gangID == 0 {
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	wasLeader := isLeader(gangReg, gangID, charID)

	// Snapshot the membership before removal so the notification reaches everyone,
	// including a dissolving remainder.
	before := memberIDs(gangReg, gangID)
	_, disbanded := gangReg.RemoveMember(charID)

	notifyGangLeave(gangID, charID, before, connLookup)
	sendGangSessionChange(c, accountID, gangID, 0, roleOf(wasLeader), 0)
	sysNotify(charID, 0, 0)

	if disbanded {
		// RemoveMember dissolves a gang once one member is left; that member's session
		// must be cleared too.
		clearSessionsFor(before, charID, gangReg, connLookup, sysNotify)
		log.Printf("[%s] → gangsta.LeaveGang: gang %d dissolved", addr, gangID)
	} else {
		if wasLeader {
			// Gang goes leaderless; remaining members get "Claim Leadership".
			broadcastGang(gangReg, gangID, connLookup, "OnGangLeader", marshal.PyTuple{
				Items: []marshal.PyRep{marshal.PyNone{}},
			})
		}
		log.Printf("[%s] → gangsta.LeaveGang: %d left gang %d", addr, charID, gangID)
	}
	fireMemberGone(GangMemberLeft, gangID, charID, 0, wasLeader, disbanded)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

func handleDisband(
	c *evenet.Conn, addr string, _ *server.CallStream, pkt *evenet.PyPacket,
	accountID, charID int64, gangReg *gang.Registry, connLookup ConnLookup,
	sysNotify SysNotify,
) error {
	gangID := gangReg.GetCharGang(charID)
	g := gangReg.GetGang(gangID)
	if g == nil {
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	if g.LeaderID != charID {
		log.Printf("[%s] → gangsta.DisbandGang: %d is not the leader of gang %d", addr, charID, gangID)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}

	members := gangReg.Disband(gangID)
	for _, memberID := range members {
		sysNotify(memberID, 0, 0)
		if memberID == charID {
			continue
		}
		if conn, acct := connLookup(memberID); conn != nil {
			server.SendNotification(conn, acct, "OnGangLeave", "charid", marshal.PyTuple{
				Items: []marshal.PyRep{marshal.PyInt{Value: uint64(memberID)}},
			})
			sendGangSessionChange(conn, acct, gangID, 0, 0, 0)
		}
	}
	sendGangSessionChange(c, accountID, gangID, 0, gangLeaderRole, 0)
	for _, memberID := range members {
		fireGangEvent(GangEvent{Kind: GangDisbanded, GangID: gangID, CharID: memberID,
			ByCharID: charID, Dissolved: true})
	}

	log.Printf("[%s] → gangsta.DisbandGang: gang %d disbanded (%d members)", addr, gangID, len(members))
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

func handleKick(
	c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket,
	accountID, charID int64, gangReg *gang.Registry, connLookup ConnLookup,
	sysNotify SysNotify,
) error {
	targetCharID := argInt(cs, 0)
	if targetCharID == 0 {
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}

	gangID := gangReg.GetCharGang(charID)
	g := gangReg.GetGang(gangID)
	if g == nil || g.LeaderID != charID {
		log.Printf("[%s] → gangsta.KickMember: %d is not the leader of gang %d", addr, charID, gangID)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	if _, ok := g.Members[targetCharID]; !ok {
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}

	before := memberIDs(gangReg, gangID)
	wasLeader := targetCharID == g.LeaderID // leader kicking themselves via "kickself"
	_, disbanded := gangReg.RemoveMember(targetCharID)

	notifyGangLeave(gangID, targetCharID, before, connLookup)
	if conn, acct := connLookup(targetCharID); conn != nil {
		sendGangSessionChange(conn, acct, gangID, 0, roleOf(wasLeader), 0)
	}
	sysNotify(targetCharID, 0, 0)
	if disbanded {
		clearSessionsFor(before, targetCharID, gangReg, connLookup, sysNotify)
	} else if wasLeader {
		broadcastGang(gangReg, gangID, connLookup, "OnGangLeader", marshal.PyTuple{
			Items: []marshal.PyRep{marshal.PyNone{}},
		})
	}

	log.Printf("[%s] → gangsta.KickMember: removed %d from gang %d", addr, targetCharID, gangID)
	fireMemberGone(GangMemberKicked, gangID, targetCharID, charID, wasLeader, disbanded)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

func handleMakeLeader(
	c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket,
	accountID, charID int64, gangReg *gang.Registry, connLookup ConnLookup,
) error {
	targetCharID := argInt(cs, 0)
	gangID := gangReg.GetCharGang(charID)
	g := gangReg.GetGang(gangID)
	if g == nil || g.LeaderID != charID {
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	if _, ok := g.Members[targetCharID]; !ok {
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}

	gangReg.SetLeader(targetCharID)
	log.Printf("[%s] → gangsta.MakeLeader: %d now leads gang %d", addr, targetCharID, gangID)
	sendGangRoleChange(c, accountID, gangLeaderRole, 0)
	if conn, acct := connLookup(targetCharID); conn != nil {
		sendGangRoleChange(conn, acct, 0, gangLeaderRole)
	}
	broadcastGang(gangReg, gangID, connLookup, "OnGangLeader", marshal.PyTuple{
		Items: []marshal.PyRep{marshal.PyInt{Value: uint64(targetCharID)}},
	})
	fireGangEvent(GangEvent{Kind: GangLeaderChanged, GangID: gangID, OldLeader: charID, NewLeader: targetCharID})
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// ─── result builders ───────────────────────────────────────────────────────

// gangInfoForChar builds the 4-tuple parklife/gang.py unpacks:
//
//	(gangID, leaderID, [(charID, state), ...], [(gangID, inviterID), ...])
//
// gangID 0 means not in a gang and the client skips the rest.
func gangInfoForChar(charID int64, gangReg *gang.Registry) marshal.PyRep {
	gangID := gangReg.GetCharGang(charID)
	g := gangReg.GetGang(gangID)

	var members []marshal.PyRep
	var leader marshal.PyRep = marshal.PyNone{}
	if g != nil {
		if g.LeaderID != 0 {
			leader = marshal.PyInt{Value: uint64(g.LeaderID)}
		}
		for memberID := range g.Members {
			members = append(members, marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyInt{Value: uint64(memberID)},
				marshal.PyString{Value: memberStateActive},
			}})
		}
	} else {
		gangID = 0
	}

	var invitations []marshal.PyRep
	for _, inv := range gangReg.GetInvitations(charID) {
		invitations = append(invitations, marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(inv.GangID)},
			marshal.PyInt{Value: uint64(inv.InviterID)},
		}})
	}

	return marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyInt{Value: uint64(gangID)},
		leader,
		marshal.PyList{Items: members},
		marshal.PyList{Items: invitations},
	}}
}

// ─── notification helpers ──────────────────────────────────────────────────

// sendGangSessionChange notifies the client that eve.session.gangid (and gangrole
// when it differs) changed. parklife/gang.py raises "Borked gang state!" whenever its
// cached gang differs from the session, so every join and leave must send it.
func sendGangSessionChange(c *evenet.Conn, accountID, oldGangID, newGangID, oldRole, newRole int64) {
	if c == nil {
		return
	}
	val := func(id int64) marshal.PyRep {
		if id == 0 {
			return marshal.PyNone{}
		}
		return marshal.PyInt{Value: uint64(id)}
	}
	entries := []marshal.DictEntry{
		{Key: marshal.PyString{Value: "gangid"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			val(oldGangID), val(newGangID),
		}}},
	}
	if oldRole != newRole {
		entries = append(entries, marshal.DictEntry{
			Key: marshal.PyString{Value: "gangrole"},
			Value: marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyInt{Value: uint64(oldRole)}, marshal.PyInt{Value: uint64(newRole)},
			}},
		})
	}
	if err := server.SendSessionChange(c, uint64(accountID), entries); err != nil {
		log.Printf("gangsta: gangid session change failed: %v", err)
	}
}

// sendGangRoleChange updates only eve.session.gangrole (leadership handoffs).
func sendGangRoleChange(c *evenet.Conn, accountID, oldRole, newRole int64) {
	if c == nil {
		return
	}
	err := server.SendSessionChange(c, uint64(accountID), []marshal.DictEntry{
		{Key: marshal.PyString{Value: "gangrole"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(oldRole)}, marshal.PyInt{Value: uint64(newRole)},
		}}},
	})
	if err != nil {
		log.Printf("gangsta: gangrole session change failed: %v", err)
	}
}

// roleOf maps "was leader" to the gangrole session value.
func roleOf(leader bool) int64 {
	if leader {
		return gangLeaderRole
	}
	return 0
}

// isLeader reports whether charID currently leads gangID.
func isLeader(gangReg *gang.Registry, gangID, charID int64) bool {
	g := gangReg.GetGang(gangID)
	return g != nil && g.LeaderID == charID
}

// memberIDs snapshots the current membership of a gang.
func memberIDs(gangReg *gang.Registry, gangID int64) []int64 {
	g := gangReg.GetGang(gangID)
	if g == nil {
		return nil
	}
	ids := make([]int64, 0, len(g.Members))
	for id := range g.Members {
		ids = append(ids, id)
	}
	return ids
}

// notifyGangLeave tells every member in the pre-removal snapshot (except the
// leaver) that charID is gone.
func notifyGangLeave(gangID, charID int64, before []int64, connLookup ConnLookup) {
	args := marshal.PyTuple{Items: []marshal.PyRep{marshal.PyInt{Value: uint64(charID)}}}
	for _, memberID := range before {
		if memberID == charID {
			continue
		}
		if conn, acct := connLookup(memberID); conn != nil {
			server.SendNotification(conn, acct, "OnGangLeave", "charid", args)
		}
	}
}

// clearSessionsFor resets eve.session.gangid for members of a gang the registry
// dissolved implicitly (last-member rule).
func clearSessionsFor(before []int64, skipCharID int64, gangReg *gang.Registry, connLookup ConnLookup, sysNotify SysNotify) {
	for _, memberID := range before {
		if memberID == skipCharID || gangReg.GetCharGang(memberID) != 0 {
			continue
		}
		if conn, acct := connLookup(memberID); conn != nil {
			sendGangSessionChange(conn, acct, 1, 0, gangLeaderRole, 0)
		}
		if sysNotify != nil {
			sysNotify(memberID, 0, 0)
		}
	}
}

// HandleDisconnect removes a disconnecting character from their gang and notifies the
// remaining members, like handleLeave without a CallRsp. Called from connection
// teardown.
func HandleDisconnect(charID int64, gangReg *gang.Registry, connLookup ConnLookup, sysNotify SysNotify) {
	if gangReg == nil || charID == 0 {
		return
	}
	if sysNotify == nil {
		sysNotify = func(int64, int64, int64) {}
	}
	// Drop pending invites: accept and reject are the only other paths that clear them.
	gangReg.ClearInvitations(charID)
	gangID := gangReg.GetCharGang(charID)
	if gangID == 0 {
		return
	}
	wasLeader := isLeader(gangReg, gangID, charID)
	before := memberIDs(gangReg, gangID)
	_, disbanded := gangReg.RemoveMember(charID)

	notifyGangLeave(gangID, charID, before, connLookup)
	sysNotify(charID, 0, 0)

	if disbanded {
		clearSessionsFor(before, charID, gangReg, connLookup, sysNotify)
		log.Printf("gangsta: gang %d dissolved (member %d disconnected)", gangID, charID)
		fireMemberGone(GangMemberDisconnected, gangID, charID, 0, wasLeader, true)
		return
	}
	if wasLeader {
		broadcastGang(gangReg, gangID, connLookup, "OnGangLeader", marshal.PyTuple{
			Items: []marshal.PyRep{marshal.PyNone{}},
		})
	}
	log.Printf("gangsta: %d removed from gang %d (disconnected)", charID, gangID)
	fireMemberGone(GangMemberDisconnected, gangID, charID, 0, wasLeader, false)
}

// fireMemberGone sends the event for a member who left, was kicked or disconnected, then
// a LeaderChanged to nobody when the leaver led a gang that still exists.
func fireMemberGone(kind GangEventKind, gangID, charID, byCharID int64, wasLeader, dissolved bool) {
	fireGangEvent(GangEvent{Kind: kind, GangID: gangID, CharID: charID, ByCharID: byCharID, Dissolved: dissolved})
	if wasLeader && !dissolved {
		fireGangEvent(GangEvent{Kind: GangLeaderChanged, GangID: gangID, OldLeader: charID})
	}
}

// broadcastGang sends a notification to every current member of a gang.
func broadcastGang(gangReg *gang.Registry, gangID int64, connLookup ConnLookup, event string, args marshal.PyRep) {
	broadcastGangExcept(gangReg, gangID, 0, connLookup, event, args)
}

// broadcastGangExcept is broadcastGang, skipping one character. Each member's
// packet carries that member's own accountID.
func broadcastGangExcept(gangReg *gang.Registry, gangID, skipCharID int64, connLookup ConnLookup, event string, args marshal.PyRep) {
	for _, memberID := range memberIDs(gangReg, gangID) {
		if memberID == skipCharID {
			continue
		}
		if conn, acct := connLookup(memberID); conn != nil {
			server.SendNotification(conn, acct, event, "charid", args)
		}
	}
}
