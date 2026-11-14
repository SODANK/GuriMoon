// Channel-management handlers for the LSC service: Invite, AccessControl, Configure,
// RenameChannel, SetChannelMOTD, DestroyChannel, ForgetChannel. An exception or silent
// stub from any of them breaks the invite flow, the addressbook or the settings dialog.
//
// Invite must call ChatInvite on the invitee's client: the client exports it as a
// service call and skips PreCall_RoleCheck when boot.role is 'client'.
// AccessControl persists an ACL row and broadcasts OnLSC/AccessControl with the 6-tuple
// of the client's acl rowset header. Configure keywords arrive in the call's ArgDict.
package lsc

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sync/atomic"

	evedb "gurimoon/db"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
)

// Channel access modes, mirroring chat.CHTMODE_* in lscengine.py.
const (
	chtModeCreator           = 15
	chtModeOperator          = 7
	chtModeConversationalist = 3
	chtModeSpeaker           = 2
	chtModeListener          = 1
	chtModeNotSpecified      = -1
	chtModeDisallowed        = -2
)

// ── Caller access ────────────────────────────────────────────────────────────

// roleChatAdministrator is the client's ROLE_CHTADMINISTRATOR account bit. With ROLE_GMH it
// makes every channel operable (lscengine.GetAccessInfo).
const roleChatAdministrator int64 = 2097152

// Channel kinds accessMode tells apart.
const (
	chanPersisted = iota // a channels row; the ACL decides
	chanEphemeral        // registry-only private conversation
	chanGang             // registry-only fleet channel
)

// accessMode returns a caller's access mode on a channel. Staff and a character's own channel
// (the address book) are creator. An ephemeral channel is its recorded creator's; nobody
// operates a gang channel. A persisted channel answers aclMode, the stored owner and ACL rows.
func accessMode(role, chID, charID int64, kind int, ephemeralOwner int64, aclMode func() int64) int64 {
	if charID == 0 {
		return chtModeNotSpecified
	}
	if evedb.HasAnyRole(role, roleChatAdministrator|evedb.RoleGMH) || chID == charID {
		return chtModeCreator
	}
	switch kind {
	case chanGang:
		return chtModeConversationalist
	case chanEphemeral:
		if ephemeralOwner == charID {
			return chtModeCreator
		}
		return chtModeConversationalist
	}
	return aclMode()
}

// callerMode resolves accessMode for the character on this connection. The role is read by
// account, so a role change applies within the cache minute. A lookup failure answers
// chtModeNotSpecified, which passes no gate.
func callerMode(ctx context.Context, c *evenet.Conn, addr string, accountID, charID, chID int64, reg *Registry, sqldb *sql.DB) int64 {
	kind := chanPersisted
	switch {
	case isGangChannel(chID):
		kind = chanGang
	case isEphemeralChannel(ctx, sqldb, chID):
		kind = chanEphemeral
	}
	return accessMode(evedb.AccountRoleCached(ctx, sqldb, accountID), chID, charID, kind, reg.Owner(chID), func() int64 {
		var corpID int64
		if meta := reg.GetMeta(c); meta != nil {
			corpID = meta.CorpID
		}
		var allianceID int64
		if corpID != 0 {
			allianceID, _ = evedb.GetCorpAllianceID(ctx, sqldb, corpID)
		}
		mode, err := evedb.ChannelAccessMode(ctx, sqldb, chID, charID, corpID, allianceID)
		if err != nil {
			log.Printf("[%s] LSC: access lookup channel=%d char=%d: %v", addr, chID, charID, err)
			return chtModeNotSpecified
		}
		return mode
	})
}

// Refusal texts for the %(msg)s placeholder of the LSCCannot* messages.
const (
	msgNotOperator = "You are not an operator of this channel."
	msgNotCreator  = "Only the creator of this channel can do that."
)

// refuseChannel raises an LSCCannot* message, each of which takes one msg placeholder.
func refuseChannel(c *evenet.Conn, pkt *evenet.PyPacket, accountID int64, key, msg string) error {
	return server.SendCallException(c, pkt, accountID, server.BuildUserError(key, map[string]marshal.PyRep{
		"msg": marshal.PyString{Value: msg},
	}))
}

// configureKeys maps a Configure keyword to the message that refuses it, in the order the
// handler applies them.
var configureKeys = []struct{ kw, key string }{
	{"displayName", "LSCCannotRename"},
	{"motd", "LSCCannotSetMOTD"},
	{"newPassword", "LSCCannotSetPassword"},
	{"memberless", "LSCCannotSetMemberless"},
	{"cspa", "LSCCannotSetCSPA"},
	{"creator", "LSCCannotSetCreator"},
}

// ── OnLSC broadcast helpers ──────────────────────────────────────────────────

// buildOnLSCArgs constructs the 5-element args tuple for any OnLSC event:
// (channelID, memberCount, method, senderInfo, args).
func buildOnLSCArgs(channelID marshal.PyRep, memberCount int, meta *Member, method string, args marshal.PyTuple) marshal.PyRep {
	senderInfo := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyNone{},
		marshal.PyInt{Value: uint64(meta.CorpID)},
		marshal.PyList{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(meta.CharID)},
			marshal.PyString{Value: meta.CharName},
			marshal.PyInt{Value: uint64(meta.TypeID)},
		}},
		marshal.PyInt{Value: uint64(meta.Role)},
		marshal.PyInt{Value: 0},
	}}
	return marshal.PyTuple{Items: []marshal.PyRep{
		channelID,
		marshal.PyInt{Value: uint64(memberCount)},
		marshal.PyString{Value: method},
		senderInfo,
		args,
	}}
}

// broadcastOnLSC sends an OnLSC notification to every live member of chID.
func broadcastOnLSC(reg *Registry, chID int64, notifyArgs marshal.PyRep) {
	for _, m := range reg.Members(chID) {
		if m.Conn == nil {
			continue
		}
		notify := buildOnLSCNotification(notifyArgs, uint64(m.AccountID), reg.NextSN(m.Conn))
		if notify != nil {
			m.Conn.SendPacket(notify) //nolint:errcheck
		}
	}
}

// broadcastChannelEvent builds and broadcasts an OnLSC event originated by
// the connection's character to all members of the channel.
func broadcastChannelEvent(reg *Registry, c *evenet.Conn, chID int64, method string, args marshal.PyTuple) {
	meta := reg.GetMeta(c)
	if meta == nil || meta.CharID == 0 {
		return
	}
	count := reg.Count(chID)
	notifyArgs := buildOnLSCArgs(channelWire(chID), count, meta, method, args)
	broadcastOnLSC(reg, chID, notifyArgs)
}

// ── Server → client service calls ────────────────────────────────────────────

// hackFixedClientID is the client ID used in every CallRsp (see server.SendCallRsp);
// the client accepts it for inbound routing.
const hackFixedClientID uint64 = 130293001608

// clientCallSN numbers outgoing server-to-client calls so each CallReq carries a
// distinct callID. The client's CallRsp is ignored by handlePacket's default case.
var clientCallSN uint64

// sendClientServiceCall invokes an exported method on one of the client's own
// services (e.g. LSC.ChatInvite). The payload mirrors a client CallReq: a 3-element
// body makes ServiceCallGPCS.CallUp invoke method(*args) with no keywords.
func sendClientServiceCall(conn *evenet.Conn, targetAccountID uint64, service, method string, args marshal.PyTuple) error {
	callID := atomic.AddUint64(&clientCallSN, 1)
	body := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyInt{Value: 1}, // direct service call flag
		marshal.PyString{Value: method},
		args,
	}}
	payload := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: 0}, // ObjectCallGPCS "not an object call" flag
			&marshal.PySubStream{Decoded: body},
		}},
	}}
	pkt := &evenet.PyPacket{
		TypeString: "macho.CallReq",
		Type:       evenet.CallReq,
		Source: evenet.PyAddress{
			Type:    evenet.AddrNode,
			TypeID:  globalNodeID,
			Service: service,
			CallID:  callID,
		},
		Dest: evenet.PyAddress{
			Type:    evenet.AddrClient,
			TypeID:  hackFixedClientID,
			CallID:  callID,
			Service: service,
		},
		UserID:  targetAccountID,
		Payload: payload,
	}
	b, err := marshal.Marshal(pkt.Encode())
	if err != nil {
		return fmt.Errorf("sendClientServiceCall(%s.%s): %w", service, method, err)
	}
	return conn.SendPacket(b)
}

// ── Invite ────────────────────────────────────────────────────────────────────

// handleInvite implements LSC.Invite(otherID, channelID, displayName, addAllowed): it
// calls ChatInvite on the invitee's live client, which joins the channel and opens
// the conversation window.
func handleInvite(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID int64, reg *Registry, sqldb *sql.DB) error {
	args := cs.ArgTuple.Items
	if len(args) < 2 {
		log.Printf("[%s] LSC.Invite: expected ≥2 args, got %d", addr, len(args))
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	otherRep, ok := args[0].(marshal.PyInt)
	if !ok {
		log.Printf("[%s] LSC.Invite: otherID is %T", addr, args[0])
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	otherID := int64(otherRep.Value)
	chID, ok := decodeChannelID(args[1])
	if !ok {
		log.Printf("[%s] LSC.Invite: cannot decode channelID from %T", addr, args[1])
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	addAllowed := false
	if len(args) >= 4 {
		switch v := args[3].(type) {
		case marshal.PyBool:
			addAllowed = v.Value
		case marshal.PyInt:
			addAllowed = v.Value != 0
		}
	}

	meta := reg.GetMeta(c)
	if (meta == nil || meta.CharID == 0) && charID != 0 {
		if ci, err := evedb.GetCharInfo(ctx, sqldb, charID); err == nil {
			reg.SetMeta(c, accountID, charID, ci.CharName, ci.CorpID, ci.TypeID, ci.Role)
			meta = reg.GetMeta(c)
		}
	}
	invitorName := ""
	if meta != nil {
		invitorName = meta.CharName
	}

	target := reg.FindByCharID(otherID)
	if target == nil {
		log.Printf("[%s] LSC.Invite: charID %d is not online", addr, otherID)
		// Client displays the wrapped exception as a generic error dialog.
		return server.SendCallException(c, pkt, accountID, marshal.PyString{
			Value: "The character you are trying to invite is not online.",
		})
	}

	// Grant the invitee access on channels with a restrictive default.
	if addAllowed && !isEphemeralChannel(ctx, sqldb, chID) {
		if err := evedb.EnsureChannel(ctx, sqldb, chID, chID, evedb.ResolveChannelDisplayName(ctx, sqldb, chID)); err != nil {
			log.Printf("[%s] LSC.Invite: EnsureChannel: %v", addr, err)
		}
		if err := evedb.UpsertChannelMod(ctx, sqldb, chID, &otherID, chtModeConversationalist, nil, invitorName, "invited"); err != nil {
			log.Printf("[%s] LSC.Invite: UpsertChannelMod: %v", addr, err)
		}
	}

	gender := evedb.GetCharGender(ctx, sqldb, charID)
	inviteArgs := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyInt{Value: uint64(charID)},
		marshal.PyString{Value: invitorName},
		marshal.PyInt{Value: uint64(gender)},
		marshal.PyInt{Value: uint64(chID)},
	}}
	if err := sendClientServiceCall(target.Conn, uint64(target.AccountID), "LSC", "ChatInvite", inviteArgs); err != nil {
		log.Printf("[%s] LSC.Invite: ChatInvite to %d: %v", addr, otherID, err)
	}
	log.Printf("[%s] → LSC.Invite charID=%d invited %d to channel %d", addr, charID, otherID, chID)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// ── AccessControl ─────────────────────────────────────────────────────────────

// handleAccessControl implements LSC.AccessControl(channelID, accessor, mode[,
// untilWhen, reason]). CHTMODE_NOTSPECIFIED removes the entry and a None accessor sets
// the channel default. It backs the addressbook buddy and block lists, muting and the
// channel allowed, blocked and operator lists.
func handleAccessControl(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID int64, reg *Registry, sqldb *sql.DB) error {
	args := cs.ArgTuple.Items
	if len(args) < 3 {
		log.Printf("[%s] LSC.AccessControl: expected ≥3 args, got %d", addr, len(args))
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	chID, ok := decodeChannelID(args[0])
	if !ok {
		log.Printf("[%s] LSC.AccessControl: cannot decode channelID from %T", addr, args[0])
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}

	var accessor *int64
	if v, ok := args[1].(marshal.PyInt); ok {
		id := int64(v.Value)
		accessor = &id
	}
	modeRep, ok := args[2].(marshal.PyInt)
	if !ok {
		log.Printf("[%s] LSC.AccessControl: mode is %T", addr, args[2])
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	mode := int64(modeRep.Value) // sign-extended on unmarshal: -1, -2 survive

	var untilWhen *int64
	if len(args) >= 4 {
		if v, ok := args[3].(marshal.PyInt); ok {
			t := int64(v.Value)
			untilWhen = &t
		}
	}
	reason := ""
	if len(args) >= 5 {
		if v, ok := args[4].(marshal.PyString); ok {
			reason = v.Value
		}
	}

	// Operators edit the ACL, and nobody grants a mode above their own.
	have := callerMode(ctx, c, addr, accountID, charID, chID, reg, sqldb)
	if have < chtModeOperator || mode > have {
		log.Printf("[%s] LSC.AccessControl: refused channel=%d char=%d (mode %d, wants %d)", addr, chID, charID, have, mode)
		return refuseChannel(c, pkt, accountID, "LSCCannotAccessControl", msgNotOperator)
	}

	meta := reg.GetMeta(c)
	admin := ""
	if meta != nil {
		admin = meta.CharName
	}

	if mode == chtModeNotSpecified {
		if err := evedb.DeleteChannelMod(ctx, sqldb, chID, accessor); err != nil {
			log.Printf("[%s] LSC.AccessControl: DeleteChannelMod: %v", addr, err)
		}
	} else {
		// channelMods has a foreign key on channels.channelID, so the channel row must
		// exist first.
		if err := evedb.EnsureChannel(ctx, sqldb, chID, chID, evedb.ResolveChannelDisplayName(ctx, sqldb, chID)); err != nil {
			log.Printf("[%s] LSC.AccessControl: EnsureChannel: %v", addr, err)
		}
		if err := evedb.UpsertChannelMod(ctx, sqldb, chID, accessor, mode, untilWhen, admin, reason); err != nil {
			log.Printf("[%s] LSC.AccessControl: UpsertChannelMod: %v", addr, err)
		}
	}

	// Broadcast a 6-tuple matching the client acl rowset header:
	// (accessor, mode, untilWhen, originalMode, admin, reason).
	var accessorRep marshal.PyRep = marshal.PyNone{}
	if accessor != nil {
		accessorRep = marshal.PyInt{Value: uint64(*accessor)}
	}
	var untilRep marshal.PyRep = marshal.PyNone{}
	if untilWhen != nil {
		untilRep = marshal.PyInt{Value: uint64(*untilWhen)}
	}
	broadcastChannelEvent(reg, c, chID, "AccessControl", marshal.PyTuple{Items: []marshal.PyRep{
		accessorRep,
		marshal.PyInt{Value: uint64(mode)},
		untilRep,
		marshal.PyNone{}, // originalMode
		marshal.PyString{Value: admin},
		marshal.PyString{Value: reason},
	}})

	log.Printf("[%s] → LSC.AccessControl channel=%d accessor=%v mode=%d", addr, chID, accessor, mode)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// ── Configure / RenameChannel / SetChannelMOTD ───────────────────────────────

// dictGet returns the value for a string key in a PyDict, or nil.
func dictGet(d *marshal.PyDict, key string) marshal.PyRep {
	if d == nil {
		return nil
	}
	for _, e := range d.Items {
		if k, ok := e.Key.(marshal.PyString); ok && k.Value == key {
			return e.Value
		}
	}
	return nil
}

// repToString extracts a Go string from a PyString rep (empty otherwise).
func repToString(rep marshal.PyRep) string {
	if s, ok := rep.(marshal.PyString); ok {
		return s.Value
	}
	return ""
}

// handleConfigure implements LSC.Configure(channelID, **kw) from the channel-settings
// dialog: displayName, motd, newPassword, memberless, cspa, creator. Each accepted
// change is persisted and announced with its own OnLSC event.
func handleConfigure(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID int64, reg *Registry, sqldb *sql.DB) error {
	if len(cs.ArgTuple.Items) < 1 {
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	chID, ok := decodeChannelID(cs.ArgTuple.Items[0])
	if !ok {
		log.Printf("[%s] LSC.Configure: cannot decode channelID from %T", addr, cs.ArgTuple.Items[0])
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	kw := cs.ArgDict
	persist := !isEphemeralChannel(ctx, sqldb, chID)

	// Every keyword is checked before the first change: operators configure, the creator
	// alone hands the channel on.
	have := callerMode(ctx, c, addr, accountID, charID, chID, reg, sqldb)
	for _, k := range configureKeys {
		if dictGet(kw, k.kw) == nil {
			continue
		}
		if k.kw == "creator" && have < chtModeCreator {
			log.Printf("[%s] LSC.Configure: refused creator change channel=%d char=%d (mode %d)", addr, chID, charID, have)
			return refuseChannel(c, pkt, accountID, k.key, msgNotCreator)
		}
		if have < chtModeOperator {
			log.Printf("[%s] LSC.Configure: refused %s channel=%d char=%d (mode %d)", addr, k.kw, chID, charID, have)
			return refuseChannel(c, pkt, accountID, k.key, msgNotOperator)
		}
	}

	if v := dictGet(kw, "displayName"); v != nil {
		name := repToString(v)
		if persist {
			if err := evedb.UpdateChannelField(ctx, sqldb, chID, "displayName", name); err != nil {
				log.Printf("[%s] LSC.Configure displayName: %v", addr, err)
			}
		}
		broadcastChannelEvent(reg, c, chID, "RenameChannel", marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyString{Value: name},
		}})
	}
	if v := dictGet(kw, "motd"); v != nil {
		motd := repToString(v)
		if persist {
			if err := evedb.UpdateChannelField(ctx, sqldb, chID, "motd", motd); err != nil {
				log.Printf("[%s] LSC.Configure motd: %v", addr, err)
			}
		}
		broadcastChannelEvent(reg, c, chID, "SetChannelMOTD", marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyString{Value: motd},
		}})
	}
	if v := dictGet(kw, "newPassword"); v != nil {
		pw := repToString(v)
		var pwVal any
		var pwRep marshal.PyRep = marshal.PyNone{}
		if pw != "" {
			pwVal = pw
			pwRep = marshal.PyString{Value: pw}
		}
		if persist {
			if err := evedb.UpdateChannelField(ctx, sqldb, chID, "password", pwVal); err != nil {
				log.Printf("[%s] LSC.Configure password: %v", addr, err)
			}
		}
		broadcastChannelEvent(reg, c, chID, "SetChannelPassword", marshal.PyTuple{Items: []marshal.PyRep{pwRep}})
	}
	if v := dictGet(kw, "memberless"); v != nil {
		memberless := int64(0)
		if i, ok := v.(marshal.PyInt); ok {
			memberless = int64(i.Value)
		}
		if persist {
			if err := evedb.UpdateChannelField(ctx, sqldb, chID, "memberless", memberless); err != nil {
				log.Printf("[%s] LSC.Configure memberless: %v", addr, err)
			}
		}
		// Switching to immediate mode makes the client re-index args[1] as the member list,
		// so the current roster is always attached.
		broadcastChannelEvent(reg, c, chID, "SetChannelMemberless", marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(memberless)},
			membersToCharsRowset(reg.Members(chID)),
		}})
	}
	if v := dictGet(kw, "cspa"); v != nil {
		cspa := int64(0)
		if i, ok := v.(marshal.PyInt); ok {
			cspa = int64(i.Value)
		}
		if persist {
			if err := evedb.UpdateChannelField(ctx, sqldb, chID, "cspa", cspa); err != nil {
				log.Printf("[%s] LSC.Configure cspa: %v", addr, err)
			}
		}
		broadcastChannelEvent(reg, c, chID, "SetChannelCSPA", marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(cspa)},
		}})
	}
	if v := dictGet(kw, "creator"); v != nil {
		if i, ok := v.(marshal.PyInt); ok {
			if persist {
				if err := evedb.UpdateChannelField(ctx, sqldb, chID, "ownerID", int64(i.Value)); err != nil {
					log.Printf("[%s] LSC.Configure creator: %v", addr, err)
				}
			}
			broadcastChannelEvent(reg, c, chID, "SetChannelCreator", marshal.PyTuple{Items: []marshal.PyRep{i}})
		}
	}

	nkw := 0
	if kw != nil {
		nkw = len(kw.Items)
	}
	log.Printf("[%s] → LSC.Configure channel=%d (%d keyword(s))", addr, chID, nkw)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// handleRenameChannel implements LSC.RenameChannel(channelID, displayName).
func handleRenameChannel(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID int64, reg *Registry, sqldb *sql.DB) error {
	args := cs.ArgTuple.Items
	if len(args) < 2 {
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	chID, ok := decodeChannelID(args[0])
	if !ok {
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	if have := callerMode(ctx, c, addr, accountID, charID, chID, reg, sqldb); have < chtModeOperator {
		log.Printf("[%s] LSC.RenameChannel: refused channel=%d char=%d (mode %d)", addr, chID, charID, have)
		return refuseChannel(c, pkt, accountID, "LSCCannotRename", msgNotOperator)
	}
	name := repToString(args[1])
	if !isEphemeralChannel(ctx, sqldb, chID) {
		if err := evedb.UpdateChannelField(ctx, sqldb, chID, "displayName", name); err != nil {
			log.Printf("[%s] LSC.RenameChannel: %v", addr, err)
		}
	}
	broadcastChannelEvent(reg, c, chID, "RenameChannel", marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyString{Value: name},
	}})
	log.Printf("[%s] → LSC.RenameChannel channel=%d name=%q", addr, chID, name)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// handleSetChannelMOTD implements LSC.SetChannelMOTD(channelID, motd).
func handleSetChannelMOTD(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID int64, reg *Registry, sqldb *sql.DB) error {
	args := cs.ArgTuple.Items
	if len(args) < 2 {
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	chID, ok := decodeChannelID(args[0])
	if !ok {
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	if have := callerMode(ctx, c, addr, accountID, charID, chID, reg, sqldb); have < chtModeOperator {
		log.Printf("[%s] LSC.SetChannelMOTD: refused channel=%d char=%d (mode %d)", addr, chID, charID, have)
		return refuseChannel(c, pkt, accountID, "LSCCannotSetMOTD", msgNotOperator)
	}
	motd := repToString(args[1])
	if !isEphemeralChannel(ctx, sqldb, chID) {
		if err := evedb.UpdateChannelField(ctx, sqldb, chID, "motd", motd); err != nil {
			log.Printf("[%s] LSC.SetChannelMOTD: %v", addr, err)
		}
	}
	broadcastChannelEvent(reg, c, chID, "SetChannelMOTD", marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyString{Value: motd},
	}})
	log.Printf("[%s] → LSC.SetChannelMOTD channel=%d", addr, chID)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// ── DestroyChannel / ForgetChannel ───────────────────────────────────────────

// handleDestroyChannel implements LSC.DestroyChannel(channelID): every member gets
// OnLSC/DestroyChannel, the registry entry is cleared and the DB rows are removed.
func handleDestroyChannel(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID int64, reg *Registry, sqldb *sql.DB) error {
	if len(cs.ArgTuple.Items) < 1 {
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	chID, ok := decodeChannelID(cs.ArgTuple.Items[0])
	if !ok {
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	if have := callerMode(ctx, c, addr, accountID, charID, chID, reg, sqldb); have < chtModeCreator {
		log.Printf("[%s] LSC.DestroyChannel: refused channel=%d char=%d (mode %d)", addr, chID, charID, have)
		return refuseChannel(c, pkt, accountID, "LSCCannotDestroy", msgNotCreator)
	}

	members := reg.Members(chID)
	broadcastChannelEvent(reg, c, chID, "DestroyChannel", marshal.PyTuple{})
	for _, m := range members {
		if m.Conn != nil {
			reg.Leave(m.Conn, chID)
		}
	}
	if !isEphemeralChannel(ctx, sqldb, chID) {
		if err := evedb.DeleteChannel(ctx, sqldb, chID); err != nil {
			log.Printf("[%s] LSC.DestroyChannel: %v", addr, err)
		}
	}
	log.Printf("[%s] → LSC.DestroyChannel channel=%d (%d member(s) notified)", addr, chID, len(members))
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// handleForgetChannel implements LSC.ForgetChannel(channelID): drops this character's
// subscription row for a channel owned by someone else.
func handleForgetChannel(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, reg *Registry, sqldb *sql.DB) error {
	if len(cs.ArgTuple.Items) < 1 {
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	chID, ok := decodeChannelID(cs.ArgTuple.Items[0])
	if !ok {
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	meta := reg.GetMeta(c)
	if meta != nil && meta.CharID != 0 {
		reg.Leave(c, chID)
		if err := evedb.DeleteChannelChar(ctx, sqldb, chID, meta.CharID); err != nil {
			log.Printf("[%s] LSC.ForgetChannel: %v", addr, err)
		}
	}
	log.Printf("[%s] → LSC.ForgetChannel channel=%d", addr, chID)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}
