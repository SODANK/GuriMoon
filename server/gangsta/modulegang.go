package gangsta

import (
	"log"

	"gurimoon/gang"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
)

// Fleet membership for pilots that have no connection: the seam a module (rolebots)
// uses to seat its own pilots in a player's gang. Inert until a module installs
// ModuleOwns and ModuleGangCandidate.
//
// handleInvite refuses a target with no live socket; a module pilot is connection-less
// but present. The helpers below repeat handleAccept's join minus the CallRsp and the
// joiner's gangid session change. They live in package gangsta because they need its
// unexported broadcast and session helpers, and a second copy of that wire format
// would drift.

// ModuleGangDeps is what the module-facing helpers use to reach the core.
type ModuleGangDeps struct {
	Registry   func() *gang.Registry
	ConnLookup ConnLookup
	SysNotify  SysNotify
}

var moduleGang ModuleGangDeps

// InstallModuleGang publishes the core's fleet plumbing for the exported helpers
// below. Called once from package main's init(); later calls overwrite.
func InstallModuleGang(d ModuleGangDeps) { moduleGang = d }

// ModuleGangCandidate is set by a module; handleInvite calls it for an invited pilot
// with no socket. It returns the pilot's corporation (for the gangCorps rowset) and a
// refusal: empty accepts, else the sentence shown to the inviter via CustomError.
var ModuleGangCandidate func(gangID, inviterCharID, targetCharID int64) (corpID int64, refusal string)

// ModuleGangJoined is set by a module. handleModuleInvite calls it after the pilot is
// a member and the humans have been told, so a slow module cannot delay the
// inviter's fleet window.
var ModuleGangJoined func(gangID, botCharID, inviterCharID, inviterCorpID int64)

// GangEventKind is what changed in a gang (ModuleGangEvent).
type GangEventKind int

const (
	GangMemberLeft GangEventKind = iota + 1
	GangMemberKicked
	GangMemberDisconnected
	GangDisbanded
	GangLeaderChanged
)

// GangEvent is one membership or leadership change. CharID is who left, was kicked or
// disconnected; ByCharID is the kicker or the disbanding leader. A LeaderChanged event
// carries OldLeader and NewLeader (0: the gang is leaderless). Dissolved is set when the
// registry deleted the gang.
type GangEvent struct {
	Kind                 GangEventKind
	GangID               int64
	CharID, ByCharID     int64
	OldLeader, NewLeader int64
	Dissolved            bool
}

// ModuleGangEvent is set by a module to hear leaves, kicks, disconnects, disbands and
// leadership changes made by players. It is called on the player's connection
// goroutine after the registry changed and the clients were told, so it must not
// block. The Bot* helpers below never fire it: the module made those changes itself.
var ModuleGangEvent func(GangEvent)

func fireGangEvent(ev GangEvent) {
	if ModuleGangEvent != nil {
		ModuleGangEvent(ev)
	}
}

// ModuleOwns is set by a module. handleInvite calls it for every target with no
// socket, so it must be a cached roster lookup. It answers ownership only; the
// policy question is ModuleGangCandidate.
var ModuleOwns func(charID int64) bool

// ModuleOwnsCharacter is the nil-safe form: without a module, nobody is owned.
func ModuleOwnsCharacter(charID int64) bool {
	return ModuleOwns != nil && ModuleGangCandidate != nil && charID > 0 && ModuleOwns(charID)
}

// handleModuleInvite performs the connection-less join and answers the inviter's
// CallReq. handleInvite calls it when the target has no socket and a module claims it.
// Existing members hear OnGangJoin before anything else, as in handleAccept.
func handleModuleInvite(
	c *evenet.Conn, addr string, pkt *evenet.PyPacket,
	accountID, charID, corpID, targetCharID int64,
	gangReg *gang.Registry, connLookup ConnLookup, nameOf NameLookup, sysNotify SysNotify,
) error {
	gangID := gangReg.GetCharGang(charID)

	// Ask the module before creating a gang, so a refusal does not leave the inviter
	// alone in a one-man gang.
	botCorpID, refusal := ModuleGangCandidate(gangID, charID, targetCharID)
	if refusal != "" {
		log.Printf("[%s] → gangsta.InviteCharacter: module declined %d: %s", addr, targetCharID, refusal)
		return server.SendCallException(c, pkt, accountID, server.BuildUserError(
			"CustomError", map[string]marshal.PyRep{
				"error": marshal.PyString{Value: refusal},
			}))
	}

	if gangID == 0 {
		gangID = gangReg.CreateGang(charID, corpID)
		log.Printf("[%s] → gangsta.InviteCharacter: created gang %d for leader %d", addr, gangID, charID)
		sendGangSessionChange(c, accountID, 0, gangID, 0, gangLeaderRole)
		sysNotify(charID, gangID, corpID)
	}

	if err := gangReg.AddMember(gangID, targetCharID, botCorpID); err != nil {
		log.Printf("[%s] → gangsta.InviteCharacter: module join failed: %v", addr, err)
		return server.SendCallException(c, pkt, accountID, server.BuildUserError(
			"GangCandidateOffline", map[string]marshal.PyRep{
				"invitee": marshal.PyString{Value: nameOf(targetCharID)},
			}))
	}

	broadcastGangExcept(gangReg, gangID, targetCharID, connLookup, "OnGangJoin", marshal.PyTuple{
		Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(targetCharID)},
			marshal.PyString{Value: memberStateActive},
		},
	})
	sysNotify(targetCharID, gangID, botCorpID)

	if ModuleGangJoined != nil {
		ModuleGangJoined(gangID, targetCharID, charID, corpID)
	}

	log.Printf("[%s] → gangsta.InviteCharacter: module pilot %d joined gang %d", addr, targetCharID, gangID)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// BotLeave removes a module pilot from its fleet: handleLeave without a CallRsp or
// session change. It returns the gang left and whether the gang dissolved. Bots keep
// a gang above the last-man threshold, so releasing them lets it collapse.
func BotLeave(botCharID int64) (int64, bool) {
	reg := moduleRegistry()
	if reg == nil || botCharID == 0 {
		return 0, false
	}
	connLookup := moduleConnLookup()
	sysNotify := moduleSysNotify()

	reg.ClearInvitations(botCharID)
	gangID := reg.GetCharGang(botCharID)
	if gangID == 0 {
		return 0, false
	}
	wasLeader := isLeader(reg, gangID, botCharID)
	before := memberIDs(reg, gangID)
	_, disbanded := reg.RemoveMember(botCharID)

	notifyGangLeave(gangID, botCharID, before, connLookup)
	sysNotify(botCharID, 0, 0)

	if disbanded {
		clearSessionsFor(before, botCharID, reg, connLookup, sysNotify)
		log.Printf("gangsta: gang %d dissolved (module pilot %d released)", gangID, botCharID)
		return gangID, true
	}
	if wasLeader {
		// A bot should not hold the seat, but the registry hands leadership out when a
		// leader leaves.
		broadcastGang(reg, gangID, connLookup, "OnGangLeader", marshal.PyTuple{
			Items: []marshal.PyRep{marshal.PyNone{}},
		})
	}
	log.Printf("gangsta: module pilot %d released from gang %d", botCharID, gangID)
	return gangID, false
}

// BotMembers snapshots a gang's membership for a module.
func BotMembers(gangID int64) []int64 {
	reg := moduleRegistry()
	if reg == nil || gangID == 0 {
		return nil
	}
	return memberIDs(reg, gangID)
}

// HumanMembersOnline reports the members of a gang that have a live connection. An
// empty answer means every remaining member is synthetic.
func HumanMembersOnline(gangID int64) []int64 {
	reg := moduleRegistry()
	if reg == nil || gangID == 0 {
		return nil
	}
	connLookup := moduleConnLookup()
	var live []int64
	for _, id := range memberIDs(reg, gangID) {
		if conn, _ := connLookup(id); conn != nil {
			live = append(live, id)
		}
	}
	return live
}

// GangOf reports which gang a character is in, or 0.
func GangOf(charID int64) int64 {
	reg := moduleRegistry()
	if reg == nil || charID == 0 {
		return 0
	}
	return reg.GetCharGang(charID)
}

// GangLeader reports who leads a gang, or 0 when it is leaderless (the registry
// clears LeaderID when a leader leaves so the client can offer Claim Leadership).
func GangLeader(gangID int64) int64 {
	reg := moduleRegistry()
	if reg == nil || gangID == 0 {
		return 0
	}
	g := reg.GetGang(gangID)
	if g == nil {
		return 0
	}
	return g.LeaderID
}

func moduleRegistry() *gang.Registry {
	if moduleGang.Registry == nil {
		return nil
	}
	return moduleGang.Registry()
}

func moduleConnLookup() ConnLookup {
	if moduleGang.ConnLookup == nil {
		return func(int64) (*evenet.Conn, int64) { return nil, 0 }
	}
	return moduleGang.ConnLookup
}

func moduleSysNotify() SysNotify {
	if moduleGang.SysNotify == nil {
		return func(int64, int64, int64) {}
	}
	return moduleGang.SysNotify
}

// ── fleets the module opens (the Corporate Portal's "Request a seat") ────────
//
// The helpers below give a fleet the module already runs a real gang so a human can
// join it. The invite is the only client step; the accept lands in handleAccept.

// BotCreateGang creates a real gang led by a module pilot: CreateGang plus the
// per-system notice, without the leader's session change.
func BotCreateGang(leaderBotID, corpID int64) int64 {
	reg := moduleRegistry()
	if reg == nil || leaderBotID == 0 {
		return 0
	}
	if existing := reg.GetCharGang(leaderBotID); existing != 0 {
		return 0
	}
	gangID := reg.CreateGang(leaderBotID, corpID)
	moduleSysNotify()(leaderBotID, gangID, corpID)
	log.Printf("gangsta: module pilot %d opened gang %d", leaderBotID, gangID)
	return gangID
}

// BotJoin puts a module pilot in a gang the module opened: handleModuleInvite
// without the invitation and the policy ask.
func BotJoin(gangID, botID, corpID int64) error {
	reg := moduleRegistry()
	if reg == nil || gangID == 0 || botID == 0 {
		return nil
	}
	if reg.GetCharGang(botID) == gangID {
		return nil
	}
	if err := reg.AddMember(gangID, botID, corpID); err != nil {
		return err
	}
	broadcastGangExcept(reg, gangID, botID, moduleConnLookup(), "OnGangJoin", marshal.PyTuple{
		Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(botID)},
			marshal.PyString{Value: memberStateActive},
		},
	})
	moduleSysNotify()(botID, gangID, corpID)
	return nil
}

// BotInvite has a module pilot invite a human into the gang it leads. The human's
// client shows the AskJoinGang dialog naming the commander. It returns false when the
// human has no connection.
func BotInvite(gangID, leaderBotID, humanID int64, leaderName string) bool {
	reg := moduleRegistry()
	if reg == nil || gangID == 0 || humanID == 0 {
		return false
	}
	conn, acct := moduleConnLookup()(humanID)
	if conn == nil {
		return false
	}
	reg.AddInvitation(gangID, leaderBotID, humanID)
	server.SendNotification(conn, acct, "OnGangInvite", "charid", marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyInt{Value: uint64(leaderBotID)},
		marshal.PyInt{Value: uint64(gangID)},
		marshal.PyString{Value: "AskJoinGang"},
		marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "name"}, Value: marshal.PyString{Value: leaderName}},
		}},
	}})
	log.Printf("gangsta: module pilot %d invited %d to gang %d", leaderBotID, humanID, gangID)
	return true
}

// BotMakeLeader hands a gang's leadership to a member, a module pilot or a human. Like
// handleMakeLeader, the old and new leader get the gangrole session change when they have
// a connection, so a human handed the gang gets the leader menus (Warp Gang, Regroup,
// Kick). Everyone hears OnGangLeader. No ModuleGangEvent fires.
func BotMakeLeader(gangID, newLeaderBotID int64) bool {
	reg := moduleRegistry()
	if reg == nil || gangID == 0 || newLeaderBotID == 0 {
		return false
	}
	if reg.GetCharGang(newLeaderBotID) != gangID {
		return false
	}
	var oldLeader int64
	if g := reg.GetGang(gangID); g != nil {
		oldLeader = g.LeaderID
	}
	if reg.SetLeader(newLeaderBotID) != gangID {
		return false
	}
	if oldLeader != newLeaderBotID {
		connLookup := moduleConnLookup()
		if oldLeader != 0 {
			if conn, acct := connLookup(oldLeader); conn != nil {
				sendGangRoleChange(conn, acct, gangLeaderRole, 0)
			}
		}
		if conn, acct := connLookup(newLeaderBotID); conn != nil {
			sendGangRoleChange(conn, acct, 0, gangLeaderRole)
		}
	}
	broadcastGang(reg, gangID, moduleConnLookup(), "OnGangLeader", marshal.PyTuple{
		Items: []marshal.PyRep{marshal.PyInt{Value: uint64(newLeaderBotID)}},
	})
	return true
}

// BotDisband dissolves a module-opened gang: handleDisband without the CallRsp.
// Every human aboard hears OnGangLeave and has the gangid session cleared.
func BotDisband(gangID int64) []int64 {
	reg := moduleRegistry()
	if reg == nil || gangID == 0 {
		return nil
	}
	connLookup := moduleConnLookup()
	sysNotify := moduleSysNotify()
	members := reg.Disband(gangID)
	for _, memberID := range members {
		sysNotify(memberID, 0, 0)
		if conn, acct := connLookup(memberID); conn != nil {
			server.SendNotification(conn, acct, "OnGangLeave", "charid", marshal.PyTuple{
				Items: []marshal.PyRep{marshal.PyInt{Value: uint64(memberID)}},
			})
			sendGangSessionChange(conn, acct, gangID, 0, 0, 0)
		}
	}
	if len(members) > 0 {
		log.Printf("gangsta: module gang %d disbanded (%d members)", gangID, len(members))
	}
	return members
}
