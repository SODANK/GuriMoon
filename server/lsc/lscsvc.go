// Package lsc implements the "LSC" chat and mail service: channel join and leave, chat
// broadcast through OnLSC notifications, and the evemail inbox.
//
// The Registry is shared process-wide, created in main and passed to Handle on every call.
package lsc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	evedb "gurimoon/db"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
)

// Handle dispatches a CallReq addressed to the "LSC" service. charID is the character logged
// in on this connection, 0 before one is selected.
func Handle(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, charID int64, reg *Registry, sqldb *sql.DB) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	switch cs.Method {
	case "GetChannels":
		return handleGetChannels(ctx, c, addr, pkt, accountID, charID, reg, sqldb)
	case "JoinChannels":
		return handleJoinChannels(ctx, c, addr, cs, pkt, accountID, charID, reg, sqldb)
	case "GetMyMessages":
		return handleGetMyMessages(ctx, c, addr, pkt, accountID, charID, sqldb)
	case "GetMembers":
		return handleGetMembers(ctx, c, addr, cs, pkt, accountID, reg, sqldb)
	case "SendMessage":
		return handleSendMessage(ctx, c, addr, cs, pkt, accountID, charID, reg, sqldb)
	case "LeaveChannels":
		return handleLeaveChannels(ctx, c, addr, cs, pkt, accountID, reg, sqldb)
	case "LeaveChannel":
		return handleLeaveChannel(ctx, c, addr, cs, pkt, accountID, reg, sqldb)
	case "CreateChannel":
		return handleCreateChannel(ctx, c, addr, cs, pkt, accountID, charID, reg, sqldb)
	case "Invite":
		return handleInvite(ctx, c, addr, cs, pkt, accountID, charID, reg, sqldb)
	case "AccessControl":
		return handleAccessControl(ctx, c, addr, cs, pkt, accountID, charID, reg, sqldb)
	case "Configure":
		return handleConfigure(ctx, c, addr, cs, pkt, accountID, charID, reg, sqldb)
	case "RenameChannel":
		return handleRenameChannel(ctx, c, addr, cs, pkt, accountID, charID, reg, sqldb)
	case "SetChannelMOTD":
		return handleSetChannelMOTD(ctx, c, addr, cs, pkt, accountID, charID, reg, sqldb)
	case "DestroyChannel":
		return handleDestroyChannel(ctx, c, addr, cs, pkt, accountID, charID, reg, sqldb)
	case "ForgetChannel":
		return handleForgetChannel(ctx, c, addr, cs, pkt, accountID, reg, sqldb)
	case "Page":
		return handlePage(ctx, c, addr, cs, pkt, accountID, charID, reg, sqldb)
	case "MarkMessagesRead":
		return handleMarkMessagesRead(ctx, c, addr, cs, pkt, accountID, charID, sqldb)
	case "DeleteMessages":
		return handleDeleteMessages(ctx, c, addr, cs, pkt, accountID, charID, sqldb)
	case "GetMessageDetails":
		return handleGetMessageDetails(ctx, c, addr, cs, pkt, accountID, charID, sqldb)
	default:
		server.RecordUnhandled(addr, "LSC", cs, false)
		log.Printf("[%s] LSC: unhandled method %q — sending exception", addr, cs.Method)
		return server.SendCallException(c, pkt, accountID, nil)
	}
}

// ── GetChannels ───────────────────────────────────────────────────────────────

// lscChannelCols are the 13 columns of the util.Rowset that GetChannels returns.
var lscChannelCols = []string{
	"channelID", "ownerID", "displayName", "motd",
	"comparisonKey", "memberless", "password", "mailingList",
	"cspa", "temporary", "mode", "subscribed", "estimatedMemberCount",
}

func handleGetChannels(ctx context.Context, c *evenet.Conn, addr string, pkt *evenet.PyPacket, accountID, charID int64, reg *Registry, sqldb *sql.DB) error {
	result, err := evedb.GetLSCChannels(ctx, sqldb, charID)
	if err != nil {
		log.Printf("[%s] LSC.GetChannels db: %v (returning empty)", addr, err)
		result = emptyChannelRowset()
	}

	// First GetChannels only: the client never calls JoinChannels for Corp and Local, so register
	// the player here. Later calls skip it; SwapSystemChannel handles system changes.
	if charID != 0 && reg != nil {
		if reg.GetMeta(c) == nil {
			if ci, err := evedb.GetCharInfo(ctx, sqldb, charID); err == nil {
				reg.SetMeta(c, accountID, charID, ci.CharName, ci.CorpID, ci.TypeID, ci.Role)
			}
		}
		meta := reg.GetMeta(c)
		if meta != nil && len(reg.GetChannels(c)) == 0 {
			// Corp, system and alliance channels: a channel the character is not registered in has no roster.
			var corpID, solarSystemID, allianceID int64
			_ = sqldb.QueryRowContext(ctx,
				`SELECT c.corporationID, c.solarSystemID, COALESCE(co.allianceID, 0)
				   FROM character_ c
				   LEFT JOIN corporation co ON co.corporationID = c.corporationID
				  WHERE c.characterID = ?`,
				charID).Scan(&corpID, &solarSystemID, &allianceID)

			for _, chID := range []int64{corpID, solarSystemID, allianceID} {
				if chID == 0 {
					continue
				}
				displayName := evedb.ResolveChannelDisplayName(ctx, sqldb, chID)
				evedb.EnsureChannel(ctx, sqldb, chID, chID, displayName)
				evedb.UpsertChannelChar(ctx, sqldb, chID, meta.CharID, meta.CorpID)

				// Atomically join and snapshot the pre-join membership.
				existingMembers, newCount := reg.JoinAndSnapshot(c, chID)

				// Notify existing members about new joiner.
				selfJoinArgs := buildJoinLeaveArgs(channelWire(chID), newCount, meta, "JoinChannel")
				for _, m := range existingMembers {
					if m.Conn == nil || m.Conn == c {
						continue
					}
					notify := buildOnLSCNotification(selfJoinArgs, uint64(m.AccountID), reg.NextSN(m.Conn))
					if notify != nil {
						m.Conn.SendPacket(notify) //nolint:errcheck
					}
				}

				// Backfill: tell the joiner about existing members and themselves.
				for _, m := range existingMembers {
					existArgs := buildJoinLeaveArgs(channelWire(chID), newCount, m, "JoinChannel")
					notify := buildOnLSCNotification(existArgs, uint64(accountID), reg.NextSN(c))
					if notify != nil {
						c.SendPacket(notify) //nolint:errcheck
					}
				}
				selfNotify := buildOnLSCNotification(selfJoinArgs, uint64(accountID), reg.NextSN(c))
				if selfNotify != nil {
					c.SendPacket(selfNotify) //nolint:errcheck
				}

				log.Printf("[%s] LSC.GetChannels: auto-joined channel %d (%s)", addr, chID, displayName)
			}
		}
	}

	log.Printf("[%s] → LSC.GetChannels (charID=%d)", addr, charID)
	return server.SendCallRsp(c, pkt, accountID, result)
}

// emptyChannelRowset builds an empty util.Rowset with the correct 13-column header.
func emptyChannelRowset() marshal.PyRep {
	header := make([]marshal.PyRep, len(lscChannelCols))
	for i, col := range lscChannelCols {
		header[i] = marshal.PyString{Value: col}
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

// ── JoinChannels ──────────────────────────────────────────────────────────────

// handleJoinChannels answers JoinChannels([channelID, ...]). A channel ID is a plain int or a
// typed ((type, N),) descriptor. The reply is a list of (channelID, ok, (info, mods, chars))
// 3-tuples; member notifications go out after it. See docs/server-notes.md#lsc-join-paths.
func handleJoinChannels(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID int64, reg *Registry, sqldb *sql.DB) error {
	// Populate registry meta if we have a charID.
	if charID != 0 && reg.GetMeta(c) == nil {
		if ci, err := evedb.GetCharInfo(ctx, sqldb, charID); err == nil {
			reg.SetMeta(c, accountID, charID, ci.CharName, ci.CorpID, ci.TypeID, ci.Role)
		} else {
			// Without char info, still register the member (default type and role) so rosters list it.
			reg.SetMeta(c, accountID, charID, "", 0, 1374, 3)
		}
	}

	// Extract channel IDs from args: ([channelID, ...],)
	rawIDs, typedIDs := parseChannelRequest(cs)
	channelIDs := deduplicateChannelIDs(rawIDs)
	log.Printf("[%s] ← LSC.JoinChannels requested %d channel(s): %v", addr, len(channelIDs), channelIDs)

	// The caller's corp and alliance, loaded once, to tell their mailboxes from their chat.
	var ownCorp, ownAlliance int64
	ownLoaded := false

	// Current-location snapshot for gating map-band joins, loaded on demand.
	var curSys, curConst, curReg int64
	locLoaded := false

	// Notifications sent after the reply, once the client has its channel panel.
	type pendingNotify struct {
		conn      *evenet.Conn
		data      []byte
		accountID uint64
	}
	var deferred []pendingNotify

	var results []marshal.PyRep
	for _, chID := range channelIDs {
		// Gang channels exist only in the registry. Wire keys are the typed (("gangid", N),) tuple.
		if isGangChannel(chID) {
			wire := channelWire(chID)
			existingMembers, newCount := reg.JoinAndSnapshot(c, chID)

			meta := reg.GetMeta(c)
			if meta != nil && meta.CharID != 0 {
				selfJoinArgs := buildJoinLeaveArgs(wire, newCount, meta, "JoinChannel")
				for _, m := range existingMembers {
					if m.Conn == nil || m.Conn == c {
						continue
					}
					notify := buildOnLSCNotification(selfJoinArgs, uint64(m.AccountID), reg.NextSN(m.Conn))
					if notify != nil {
						m.Conn.SendPacket(notify) //nolint:errcheck
					}
				}
				for _, m := range existingMembers {
					existArgs := buildJoinLeaveArgs(wire, newCount, m, "JoinChannel")
					notify := buildOnLSCNotification(existArgs, uint64(accountID), reg.NextSN(c))
					if notify != nil {
						deferred = append(deferred, pendingNotify{conn: c, data: notify, accountID: uint64(accountID)})
					}
				}
				selfNotify := buildOnLSCNotification(selfJoinArgs, uint64(accountID), reg.NextSN(c))
				if selfNotify != nil {
					deferred = append(deferred, pendingNotify{conn: c, data: selfNotify, accountID: uint64(accountID)})
				}
			}

			chars := membersToCharsRowset(reg.Members(chID))
			results = append(results, marshal.PyTuple{Items: []marshal.PyRep{
				wire,
				marshal.PyInt{Value: 1},
				marshal.PyTuple{Items: []marshal.PyRep{
					synthesizeGangChannelInfo(chID),
					defaultModsRowset(),
					chars,
				}},
			}})
			log.Printf("[%s] LSC.JoinChannels: joined gang channel %d (gang %d)", addr, chID, chID-gangChannelBase)
			continue
		}

		// Ephemeral channels (private chat) are registry-only: no EnsureChannel or UpsertChannelChar rows.
		if isEphemeralChannel(ctx, sqldb, chID) {
			existingMembers, newCount := reg.JoinAndSnapshot(c, chID)
			// The character's own channel (charID == channelID) is a mailing list, so the client draws no
			// chat tab for it. Private conversations use other ephemeral IDs.
			info := synthesizeChannelInfo(chID)
			chars := emptyCharsRowset()
			mods := defaultModsRowset()
			if chID == charID {
				info = synthesizeChannelInfoMailingList(chID)
				// The own channel's persisted ACL rows are the addressbook; serve them, not the default mods.
				// See docs/server-notes.md#lsc-join-paths.
				if m, err := evedb.QueryChannelMods(ctx, sqldb, chID); err == nil && m != nil {
					mods = m
				}
			} else {
				// Private conversation: the joiner needs the real roster, and existing members a JoinChannel event.
				meta := reg.GetMeta(c)
				if meta != nil && meta.CharID != 0 {
					wire := channelWire(chID)
					selfJoinArgs := buildJoinLeaveArgs(wire, newCount, meta, "JoinChannel")
					for _, m := range existingMembers {
						if m.Conn == nil || m.Conn == c {
							continue
						}
						notify := buildOnLSCNotification(selfJoinArgs, uint64(m.AccountID), reg.NextSN(m.Conn))
						if notify != nil {
							m.Conn.SendPacket(notify) //nolint:errcheck
						}
					}
					selfNotify := buildOnLSCNotification(selfJoinArgs, uint64(accountID), reg.NextSN(c))
					if selfNotify != nil {
						deferred = append(deferred, pendingNotify{conn: c, data: selfNotify, accountID: uint64(accountID)})
					}
				}
				chars = membersToCharsRowset(reg.Members(chID))
			}
			entry := marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyInt{Value: uint64(chID)},
				marshal.PyInt{Value: 1},
				marshal.PyTuple{Items: []marshal.PyRep{
					info,
					mods,
					chars,
				}},
			}}
			results = append(results, entry)
			continue
		}

		// A bare corp or alliance ID with no typed twin is a mailbox join, answered as a mailingList
		// row. See docs/server-notes.md#lsc-join-paths.
		if !typedIDs[chID] && charID != 0 && chID != charID {
			if !ownLoaded {
				ownLoaded = true
				_ = sqldb.QueryRowContext(ctx,
					`SELECT c.corporationID, COALESCE(co.allianceID, 0)
					   FROM character_ c
					   LEFT JOIN corporation co ON co.corporationID = c.corporationID
					  WHERE c.characterID = ?`,
					charID).Scan(&ownCorp, &ownAlliance)
			}
			if chID == ownCorp || (ownAlliance != 0 && chID == ownAlliance) {
				results = append(results, marshal.PyTuple{Items: []marshal.PyRep{
					marshal.PyInt{Value: uint64(chID)},
					marshal.PyInt{Value: 1},
					marshal.PyTuple{Items: []marshal.PyRep{
						mailboxChannelInfo(chID, evedb.ResolveChannelDisplayName(ctx, sqldb, chID)),
						defaultModsRowset(),
						emptyCharsRowset(),
					}},
				}})
				log.Printf("[%s] LSC.JoinChannels: mailbox %d", addr, chID)
				continue
			}
		}

		// Map channels (region, constellation, system) join only for the current location; refusing
		// with LSCCannotJoin makes the client drop stale ones. See docs/server-notes.md#lsc-join-paths.
		if chID >= 10_000_000 && chID < 40_000_000 && charID != 0 {
			if !locLoaded {
				locLoaded = true
				if err := sqldb.QueryRowContext(ctx,
					`SELECT solarSystemID, constellationID, regionID FROM character_ WHERE characterID=?`,
					charID).Scan(&curSys, &curConst, &curReg); err != nil {
					log.Printf("[%s] LSC.JoinChannels: location lookup: %v", addr, err)
				}
			}
			if chID != curSys && chID != curConst && chID != curReg {
				name := evedb.ResolveChannelDisplayName(ctx, sqldb, chID)
				if n, err := evedb.GetSystemName(ctx, sqldb, chID); err == nil && n != "" {
					name = n
				}
				results = append(results, marshal.PyTuple{Items: []marshal.PyRep{
					marshal.PyInt{Value: uint64(chID)},
					marshal.PyInt{Value: 0}, // ok=0: the client reads details as (msg, dict)
					marshal.PyTuple{Items: []marshal.PyRep{
						marshal.PyString{Value: "LSCCannotJoin"},
						marshal.PyDict{Items: []marshal.DictEntry{
							{Key: marshal.PyString{Value: "channel"}, Value: marshal.PyString{Value: name}},
						}},
					}},
				}})
				log.Printf("[%s] LSC.JoinChannels: refused stale map channel %d (%s) — char is in sys=%d const=%d reg=%d",
					addr, chID, name, curSys, curConst, curReg)
				continue
			}
		}

		// Auto-create the channel row in the DB if absent.
		displayName := evedb.ResolveChannelDisplayName(ctx, sqldb, chID)
		if err := evedb.EnsureChannel(ctx, sqldb, chID, chID, displayName); err != nil {
			log.Printf("[%s] LSC.JoinChannels: EnsureChannel(%d): %v", addr, chID, err)
		}

		info, err := evedb.QueryChannelInfo(ctx, sqldb, chID)
		if err != nil {
			log.Printf("[%s] LSC.JoinChannels: QueryChannelInfo(%d): %v, skipping", addr, chID, err)
			continue
		}
		mods, _ := evedb.QueryChannelMods(ctx, sqldb, chID)

		// Persist membership so this char appears in DB queries.
		meta := reg.GetMeta(c)
		if meta != nil && meta.CharID != 0 {
			if err := evedb.UpsertChannelChar(ctx, sqldb, chID, meta.CharID, meta.CorpID); err != nil {
				log.Printf("[%s] LSC.JoinChannels: UpsertChannelChar(%d,%d): %v", addr, chID, meta.CharID, err)
			}
		}

		// Atomically join and snapshot the pre-join membership.
		existingMembers, newCount := reg.JoinAndSnapshot(c, chID)

		// Defer notifications until after the response is sent.
		wire := channelWire(chID)
		if meta != nil && meta.CharID != 0 {
			selfJoinArgs := buildJoinLeaveArgs(wire, newCount, meta, "JoinChannel")

			// Existing members already have their panel, so notify them now.
			for _, m := range existingMembers {
				if m.Conn == nil || m.Conn == c {
					continue
				}
				notify := buildOnLSCNotification(selfJoinArgs, uint64(m.AccountID), reg.NextSN(m.Conn))
				if notify != nil {
					m.Conn.SendPacket(notify) //nolint:errcheck
				}
			}

			// Backfill and self-echo go after the reply, once the client's channel panel exists.
			for _, m := range existingMembers {
				existArgs := buildJoinLeaveArgs(wire, newCount, m, "JoinChannel")
				notify := buildOnLSCNotification(existArgs, uint64(accountID), reg.NextSN(c))
				if notify != nil {
					deferred = append(deferred, pendingNotify{conn: c, data: notify, accountID: uint64(accountID)})
				}
			}
			selfNotify := buildOnLSCNotification(selfJoinArgs, uint64(accountID), reg.NextSN(c))
			if selfNotify != nil {
				deferred = append(deferred, pendingNotify{conn: c, data: selfNotify, accountID: uint64(accountID)})
			}
		}

		// Build chars rowset from in-memory registry (includes self after Join).
		chars := membersToCharsRowset(reg.Members(chID))
		entry := marshal.PyTuple{Items: []marshal.PyRep{
			wire,                    // channelID, the client's dict key
			marshal.PyInt{Value: 1}, // OK flag
			marshal.PyTuple{Items: []marshal.PyRep{info, mods, chars}}, // details
		}}
		results = append(results, entry)
	}

	log.Printf("[%s] → LSC.JoinChannels (%d/%d channels joined)", addr, len(results), len(channelIDs))
	if err := server.SendCallRsp(c, pkt, accountID, marshal.PyList{Items: results}); err != nil {
		return err
	}

	// Deferred member notifications, now that the client has its channel panels.
	for _, d := range deferred {
		if err := d.conn.SendPacket(d.data); err != nil {
			log.Printf("[%s] LSC.JoinChannels: deferred notify: %v", addr, err)
		}
	}
	return nil
}

// ── CreateChannel ─────────────────────────────────────────────────────────────

// handleCreateChannel creates or opens a chat channel. lscengine.Invite unpacks the reply as
// (info, acl, memberList) and reads channelID off info, so it must be that 3-tuple. The first
// arg is None (fresh ephemeral ID), an int, a name string, or a typed descriptor.
func handleCreateChannel(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID int64, reg *Registry, sqldb *sql.DB) error {
	// Populate registry meta if we have a charID.
	if charID != 0 && reg.GetMeta(c) == nil {
		if ci, err := evedb.GetCharInfo(ctx, sqldb, charID); err == nil {
			reg.SetMeta(c, accountID, charID, ci.CharName, ci.CorpID, ci.TypeID, ci.Role)
		} else {
			// Without char info, still register the member (default type and role) so rosters list it.
			reg.SetMeta(c, accountID, charID, "", 0, 1374, 3)
		}
	}

	var chID int64
	var displayName string
	if len(cs.ArgTuple.Items) >= 1 {
		switch v := cs.ArgTuple.Items[0].(type) {
		case marshal.PyInt:
			chID = int64(v.Value)
		case marshal.PyString:
			displayName = v.Value
		case marshal.PyTuple:
			if id, ok := decodeChannelID(v); ok {
				chID = id
			}
		case marshal.PyNone:
			// allocate below
		}
	}
	if chID == 0 {
		// Ephemeral range (>= 140000000), so JoinChannels treats the channel as non-persistent.
		chID = nextEphemeralChannelID(charID)
	}
	if displayName == "" {
		displayName = fmt.Sprintf("Private Chat %d", chID)
	}

	log.Printf("[%s] → LSC.CreateChannel chID=%d name=%q", addr, chID, displayName)

	var info, mods, chars marshal.PyRep
	if isEphemeralChannel(ctx, sqldb, chID) {
		// Whoever opens an empty conversation is its creator for the management calls. The wire
		// ownerID stays the channel ID, so the client draws no operator menu on a private chat.
		if reg.Count(chID) == 0 {
			reg.ClaimOwner(chID, charID)
		}
		reg.Join(c, chID)
		info = synthesizeChannelInfo(chID)
		mods = defaultModsRowset()
		chars = emptyCharsRowset()
	} else {
		if err := evedb.EnsureChannel(ctx, sqldb, chID, chID, displayName); err != nil {
			log.Printf("[%s] LSC.CreateChannel EnsureChannel(%d): %v", addr, chID, err)
		}
		got, err := evedb.QueryChannelInfo(ctx, sqldb, chID)
		if err != nil {
			log.Printf("[%s] LSC.CreateChannel QueryChannelInfo(%d): %v — synthesizing", addr, chID, err)
			info = synthesizeChannelInfo(chID)
		} else {
			info = got
		}
		mods, _ = evedb.QueryChannelMods(ctx, sqldb, chID)
		if mods == nil {
			mods = defaultModsRowset()
		}
		meta := reg.GetMeta(c)
		if meta != nil && meta.CharID != 0 {
			if err := evedb.UpsertChannelChar(ctx, sqldb, chID, meta.CharID, meta.CorpID); err != nil {
				log.Printf("[%s] LSC.CreateChannel UpsertChannelChar(%d,%d): %v", addr, chID, meta.CharID, err)
			}
		}
		chars, _ = evedb.QueryChannelChars(ctx, sqldb, chID)
		if chars == nil {
			chars = emptyCharsRowset()
		}
		existingMembers, newCount := reg.JoinAndSnapshot(c, chID)
		// Members already in an existing named channel need the JoinChannel roster event.
		if meta := reg.GetMeta(c); meta != nil && meta.CharID != 0 {
			joinArgs := buildJoinLeaveArgs(channelWire(chID), newCount, meta, "JoinChannel")
			for _, m := range existingMembers {
				if m.Conn == nil || m.Conn == c {
					continue
				}
				notify := buildOnLSCNotification(joinArgs, uint64(m.AccountID), reg.NextSN(m.Conn))
				if notify != nil {
					m.Conn.SendPacket(notify) //nolint:errcheck
				}
			}
		}
	}

	result := marshal.PyTuple{Items: []marshal.PyRep{
		info,
		mods,
		chars,
	}}
	return server.SendCallRsp(c, pkt, accountID, result)
}

// ephemeralChannelCounter feeds nextEphemeralChannelID, which picks an ID in 140M..200M offset
// by charID so concurrent invites do not collide.
var ephemeralChannelCounter uint64

func nextEphemeralChannelID(charID int64) int64 {
	ephemeralChannelCounter++
	const base = int64(140000000)
	const span = int64(60000000)
	off := (charID + int64(ephemeralChannelCounter)) % span
	if off < 0 {
		off = -off
	}
	return base + off
}

// parseChannelList returns the channel IDs from the JoinChannels args (see parseChannelRequest).
func parseChannelList(cs *server.CallStream) []int64 {
	ids, _ := parseChannelRequest(cs)
	return ids
}

// parseChannelRequest is parseChannelList plus the set of IDs that arrived as a typed descriptor.
// Chat is requested as (("corpid", N),) beside the bare N; the mailbox as the bare N alone.
func parseChannelRequest(cs *server.CallStream) ([]int64, map[int64]bool) {
	if len(cs.ArgTuple.Items) < 1 {
		return nil, nil
	}
	var items []marshal.PyRep
	switch v := cs.ArgTuple.Items[0].(type) {
	case marshal.PyList:
		items = v.Items
	case marshal.PyTuple:
		items = v.Items
	default:
		return nil, nil
	}

	var ids []int64
	typed := make(map[int64]bool)
	for _, item := range items {
		if id, ok := decodeChannelID(item); ok {
			ids = append(ids, id)
			if _, bare := item.(marshal.PyInt); !bare {
				typed[id] = true
			}
		}
	}
	return ids, typed
}

// ── Gang channels ─────────────────────────────────────────────────────────────

// gangChannelBase maps gang IDs into a private range above every real channel ID, so fleet chat
// cannot collide with static channels (gang 1 would be Rookie Help).
const gangChannelBase = int64(9_000_000_000)

// isGangChannel reports whether an internal channel ID is a mapped gang channel.
func isGangChannel(chID int64) bool {
	return chID >= gangChannelBase
}

// channelWire returns the wire key for an internal channel ID: a typed (("type", N),) tuple for
// gang, region, constellation and solar system channels, a plain PyInt otherwise. lscengine files
// a channel under the key the server returns, so the typed form goes in the JoinChannels reply
// and every OnLSC notification. See docs/server-notes.md#lsc-channel-keys.
func channelWire(chID int64) marshal.PyRep {
	var typ string
	var num int64
	switch {
	case isGangChannel(chID):
		typ, num = "gangid", chID-gangChannelBase
	case chID >= 10_000_000 && chID < 20_000_000:
		typ, num = "regionid", chID
	case chID >= 20_000_000 && chID < 30_000_000:
		typ, num = "constellationid", chID
	case chID >= 30_000_000 && chID < 40_000_000:
		typ, num = "solarsystemid2", chID
	default:
		return marshal.PyInt{Value: uint64(chID)}
	}
	return marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyString{Value: typ},
			marshal.PyInt{Value: uint64(num)},
		}},
	}}
}

// decodeChannelID extracts an internal channel ID from a plain int or a typed ((type, N),)
// descriptor. A "gangid" descriptor maps into the gang range, not to its bare number.
func decodeChannelID(rep marshal.PyRep) (int64, bool) {
	switch v := rep.(type) {
	case marshal.PyInt:
		return int64(v.Value), true
	case marshal.PyTuple:
		if typ, num, ok := decodeTypedDesc(v.Items); ok {
			if typ == "gangid" {
				return gangChannelBase + num, true
			}
			return num, true
		}
	case marshal.PyList:
		if typ, num, ok := decodeTypedDesc(v.Items); ok {
			if typ == "gangid" {
				return gangChannelBase + num, true
			}
			return num, true
		}
	}
	return 0, false
}

// decodeLeaveEntry decodes one LeaveChannels element: lscengine sends (channelID, announce)
// pairs, LeaveChannel sends the bare descriptor. Both are accepted.
func decodeLeaveEntry(rep marshal.PyRep) (int64, bool) {
	if id, ok := decodeChannelID(rep); ok {
		return id, true
	}
	var items []marshal.PyRep
	switch v := rep.(type) {
	case marshal.PyTuple:
		items = v.Items
	case marshal.PyList:
		items = v.Items
	default:
		return 0, false
	}
	if len(items) == 2 {
		return decodeChannelID(items[0])
	}
	return 0, false
}

// decodeTypedDesc unpacks a 1-element typed descriptor ((type_string, number),); the pair may
// be a tuple or list.
func decodeTypedDesc(items []marshal.PyRep) (string, int64, bool) {
	if len(items) != 1 {
		return "", 0, false
	}
	var pair []marshal.PyRep
	switch inner := items[0].(type) {
	case marshal.PyTuple:
		pair = inner.Items
	case marshal.PyList:
		pair = inner.Items
	default:
		return "", 0, false
	}
	if len(pair) != 2 {
		return "", 0, false
	}
	typ, ok := pair[0].(marshal.PyString)
	if !ok {
		return "", 0, false
	}
	num, ok := pair[1].(marshal.PyInt)
	if !ok {
		return "", 0, false
	}
	return typ.Value, int64(num.Value), true
}

// ── Ephemeral / dedup helpers ─────────────────────────────────────────────────

// isEphemeralChannel reports whether chID is a character private-chat channel (>= 140000000,
// not persisted). Corp and alliance IDs share that range, so IsOwnerChannelID decides.
// See docs/server-notes.md#lsc-join-paths.
func isEphemeralChannel(ctx context.Context, sqldb *sql.DB, chID int64) bool {
	if chID < 140000000 {
		return false
	}
	if sqldb != nil && evedb.IsOwnerChannelID(ctx, sqldb, chID) {
		return false
	}
	return true
}

// deduplicateChannelIDs removes duplicate channel IDs while preserving order.
func deduplicateChannelIDs(ids []int64) []int64 {
	seen := make(map[int64]struct{}, len(ids))
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// synthesizeChannelInfo builds a minimal util.Row for an ephemeral channel
// so the client gets a valid response without a DB query.
func synthesizeChannelInfo(chID int64) marshal.PyRep {
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
	line := []marshal.PyRep{
		marshal.PyInt{Value: uint64(chID)},               // channelID
		marshal.PyInt{Value: uint64(chID)},               // ownerID
		marshal.PyString{Value: fmt.Sprintf("%d", chID)}, // displayName
		marshal.PyNone{},                                 // motd
		marshal.PyNone{},                                 // comparisonKey
		marshal.PyInt{Value: 0},                          // memberless=0: immediate mode, the roster renders
		marshal.PyNone{},                                 // password
		marshal.PyInt{Value: 0},                          // mailingList
		marshal.PyInt{Value: 0},                          // cspa
		marshal.PyInt{Value: 1},                          // temporary
		marshal.PyInt{Value: 1},                          // subscribed
	}
	return marshal.PyObject{
		Type: "util.Row",
		Args: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "header"}, Value: marshal.PyList{Items: header}},
			{Key: marshal.PyString{Value: "line"}, Value: marshal.PyList{Items: line}},
		}},
	}
}

// synthesizeGangChannelInfo builds the channel info util.Row for a fleet channel. channelID is
// the typed tuple, matching the client's channel-dict key; lscengine.GetDisplayName derives
// "Gang" from it, so displayName is only a fallback.
func synthesizeGangChannelInfo(chID int64) marshal.PyRep {
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
	line := []marshal.PyRep{
		channelWire(chID),               // channelID (typed tuple)
		marshal.PyInt{Value: 1},         // ownerID: system
		marshal.PyString{Value: "Gang"}, // displayName
		marshal.PyNone{},                // motd
		marshal.PyNone{},                // comparisonKey
		marshal.PyInt{Value: 0},         // memberless=0: immediate mode, the roster renders
		marshal.PyNone{},                // password
		marshal.PyInt{Value: 0},         // mailingList
		marshal.PyInt{Value: 0},         // cspa
		marshal.PyInt{Value: 0},         // temporary
		marshal.PyInt{Value: 1},         // subscribed
	}
	return marshal.PyObject{
		Type: "util.Row",
		Args: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "header"}, Value: marshal.PyList{Items: header}},
			{Key: marshal.PyString{Value: "line"}, Value: marshal.PyList{Items: line}},
		}},
	}
}

// synthesizeChannelInfoMailingList builds the util.Row for the character's own channel:
// mailingList=1, cspa=100, mode=15, as character creation inserts it, so the client draws no
// chat tab.
func synthesizeChannelInfoMailingList(chID int64) marshal.PyRep {
	return mailboxChannelInfo(chID, fmt.Sprintf("%d", chID))
}

// mailboxChannelInfo is synthesizeChannelInfoMailingList with a display name,
// for the corp and alliance mailboxes.
func mailboxChannelInfo(chID int64, name string) marshal.PyRep {
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
	line := []marshal.PyRep{
		marshal.PyInt{Value: uint64(chID)}, // channelID
		marshal.PyInt{Value: uint64(chID)}, // ownerID
		marshal.PyString{Value: name},      // displayName
		marshal.PyNone{},                   // motd
		marshal.PyNone{},                   // comparisonKey
		marshal.PyInt{Value: 1},            // memberless
		marshal.PyNone{},                   // password
		marshal.PyInt{Value: 1},            // mailingList
		marshal.PyInt{Value: 100},          // cspa
		marshal.PyInt{Value: 0},            // temporary
		marshal.PyInt{Value: 1},            // subscribed
	}
	return marshal.PyObject{
		Type: "util.Row",
		Args: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "header"}, Value: marshal.PyList{Items: header}},
			{Key: marshal.PyString{Value: "line"}, Value: marshal.PyList{Items: line}},
		}},
	}
}

// defaultModsRowset returns a channelMods util.Rowset with only the default-access row
// (accessor None, CHTMODE_CONVERSATIONALIST). Without it GetAccessInfo yields NOTSPECIFIED (-1)
// and the client neither sends nor receives.
func defaultModsRowset() marshal.PyRep {
	cols := []string{"accessor", "mode", "untilWhen", "originalMode", "admin", "reason"}
	header := make([]marshal.PyRep, len(cols))
	for i, c := range cols {
		header[i] = marshal.PyString{Value: c}
	}
	defaultLine := marshal.PyList{Items: []marshal.PyRep{
		marshal.PyNone{},        // accessor (channel default)
		marshal.PyInt{Value: 3}, // CHTMODE_CONVERSATIONALIST
		marshal.PyNone{},        // untilWhen
		marshal.PyNone{},        // originalMode
		marshal.PyNone{},        // admin
		marshal.PyNone{},        // reason
	}}
	return marshal.PyObject{
		Type: "util.Rowset",
		Args: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "header"}, Value: marshal.PyList{Items: header}},
			{Key: marshal.PyString{Value: "RowClass"}, Value: marshal.PyString{Value: "util.Row", IsType1: true}},
			{Key: marshal.PyString{Value: "lines"}, Value: marshal.PyList{Items: []marshal.PyRep{defaultLine}}},
		}},
	}
}

// charsRowsetCols is the RMR 3913 channelChars header. lscengine.OnLSC writes 5-element lines
// into the memberList IndexRowset and Row resolves role/extra by header position, so a longer
// header breaks .role for members added by OnLSC. See docs/server-notes.md#lsc-member-rowset.
var charsRowsetCols = []string{"charID", "corpID", "allianceID", "role", "extra"}

// emptyCharsRowset returns an empty util.Rowset with the channelChars header.
func emptyCharsRowset() marshal.PyRep {
	header := make([]marshal.PyRep, len(charsRowsetCols))
	for i, c := range charsRowsetCols {
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

// ── SendMessage ───────────────────────────────────────────────────────────────

// handleSendMessage broadcasts a chat line to the channel's Registry members as an OnLSC
// notification. Args: (channelID, messageText), channelID per decodeChannelID. A leading "/"
// takes the command path instead.
func handleSendMessage(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID int64, reg *Registry, sqldb *sql.DB) error {
	args := cs.ArgTuple.Items
	if len(args) < 2 {
		log.Printf("[%s] LSC.SendMessage: expected 2 args, got %d", addr, len(args))
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}

	chID, ok := decodeChannelID(args[0])
	if !ok {
		log.Printf("[%s] LSC.SendMessage: cannot decode channelID from %T", addr, args[0])
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}

	msgStr, ok := args[1].(marshal.PyString)
	if !ok {
		log.Printf("[%s] LSC.SendMessage: message arg is %T", addr, args[1])
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	message := msgStr.Value

	// A leading "/" is a server command; its result is echoed to the sender only.
	if len(message) > 0 && message[0] == '/' {
		if !evedb.MaySlash(ctx, sqldb, accountID) {
			log.Printf("[%s] LSC.SendMessage: chat command refused for account %d (role lacks a slash bit)", addr, accountID)
			return sendChatEcho(c, addr, pkt, accountID, reg, chID, "You are not authorised to use slash commands.")
		}
		return handleGMCommand(ctx, c, addr, cs, pkt, accountID, charID, reg, sqldb, chID, message)
	}

	// Load sender metadata (may already be cached in registry).
	meta := reg.GetMeta(c)
	if meta == nil || meta.CharID == 0 {
		if charID != 0 {
			if ci, err := evedb.GetCharInfo(ctx, sqldb, charID); err == nil {
				reg.SetMeta(c, accountID, charID, ci.CharName, ci.CorpID, ci.TypeID, ci.Role)
			} else {
				// Without char info, keep a meta with the charID and default type and role.
				reg.SetMeta(c, accountID, charID, "", 0, 1374, 3)
			}
			meta = reg.GetMeta(c)
		}
	}
	var senderCharID int64
	var senderName string
	var corpID int64
	var typeID int64 = 1374 // default fallback: generic character typeID
	var role int64 = 3      // default: ROLE_LOGIN|ROLE_PLAYER
	if meta != nil {
		senderCharID = meta.CharID
		senderName = meta.CharName
		corpID = meta.CorpID
		if meta.TypeID != 0 {
			typeID = meta.TypeID
		}
		if meta.Role != 0 {
			role = meta.Role
		}
	}

	members := reg.Members(chID)
	memberCount := len(members)
	log.Printf("[%s] → LSC.SendMessage channel=%d sender=%q charID=%d corpID=%d typeID=%d members=%d msg=%q",
		addr, chID, senderName, senderCharID, corpID, typeID, memberCount, message)

	// OnLSC_SendMessage payload: (channelID, memberCount, "SendMessage", senderInfo, (message,)),
	// senderInfo = (None, corpID, [senderID, senderName, senderType], role, corpRole).
	notifyArgs := buildSendMessageArgs(args[0], memberCount, senderCharID, senderName, corpID, typeID, role, message)

	// Built per member so each notification carries that member's UserID.
	senderIsMember := false
	for _, m := range members {
		if m.Conn == nil {
			continue // synthetic (bot) member has no connection
		}
		if m.Conn == c {
			senderIsMember = true
		}
		log.Printf("[%s] LSC.SendMessage: → %s accountID=%d", addr, m.Conn.RemoteAddr(), m.AccountID)
		notify := buildOnLSCNotification(notifyArgs, uint64(m.AccountID), reg.NextSN(m.Conn))
		if notify == nil {
			log.Printf("[%s] LSC.SendMessage: notification build failed, skipping", addr)
			continue
		}
		if err := m.Conn.SendPacket(notify); err != nil {
			log.Printf("[%s] LSC.SendMessage: send to conn: %v", addr, err)
		}
	}
	// Hand every plain-text line to ModuleChat (modulechat.go), set by the rolebots module; fleet
	// chat is where a player orders a bot. It runs on the connection goroutine, so it only enqueues.
	if ModuleChat != nil {
		ModuleChat(newChatMessage(chID, senderCharID, corpID, senderName, message))
	}

	// If the sender is not a registered member, echo to them too.
	if !senderIsMember {
		notify := buildOnLSCNotification(notifyArgs, uint64(accountID), reg.NextSN(c))
		if notify != nil {
			if err := c.SendPacket(notify); err != nil {
				log.Printf("[%s] LSC.SendMessage: send to self: %v", addr, err)
			}
		}
	}

	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// ── GM commands ───────────────────────────────────────────────────────────────

// handleGMCommand processes "/" chat commands.  It performs the action and
// echoes a result message back to the sender on the same channel.
func handleGMCommand(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID int64, reg *Registry, sqldb *sql.DB, chID int64, cmd string) error {
	switch {
	case cmd == "/giveallskill" || cmd == "/giveallskills" || cmd == "/allskills":
		if charID == 0 {
			return sendChatEcho(c, addr, pkt, accountID, reg, chID, "ERROR: no character selected")
		}
		n, err := evedb.GiveAllSkills(ctx, sqldb, charID)
		if err != nil {
			log.Printf("[%s] /giveallskill charID=%d: %v", addr, charID, err)
			return sendChatEcho(c, addr, pkt, accountID, reg, chID, fmt.Sprintf("ERROR: %v", err))
		}
		log.Printf("[%s] /giveallskill charID=%d granted %d skills", addr, charID, n)
		return sendChatEcho(c, addr, pkt, accountID, reg, chID,
			fmt.Sprintf("Granted %d skills at level 5. Relog to see changes.", n))

	default:
		return sendChatEcho(c, addr, pkt, accountID, reg, chID,
			fmt.Sprintf("Unknown command: %s", cmd))
	}
}

// sendChatEcho sends a chat notification back only to the requesting connection
// so the player sees the server response in the same channel.
func sendChatEcho(c *evenet.Conn, addr string, pkt *evenet.PyPacket, accountID int64, reg *Registry, chID int64, text string) error {
	// channelWire round-trips through decodeChannelID (typed tuple for gang channels).
	notifyArgs := buildSendMessageArgs(channelWire(chID), 1, 0, "*Server*", 0, 1374, 3, text)
	notify := buildOnLSCNotification(notifyArgs, uint64(accountID), reg.NextSN(c))
	if notify == nil {
		return nil
	}
	return c.SendPacket(notify)
}

// The invTypes "Character <bloodline>" band (group 1). infosvc.GetBloodlineByTypeID keys its dict
// on these, so any owner the client treats as a character needs a typeID in this range.
const (
	firstCharacterTypeID = 1373
	lastCharacterTypeID  = 1386
	defaultNPCChatTypeID = 1374
)

// SendNPCChat delivers a chat line from a named non-player sender to one connection, for
// dungeon triggers that make NPCs talk in Local. senderID must be non-zero and unknown to the
// client's owner cache, and senderTypeID a character bloodline type.
// See docs/server-notes.md#lsc-npc-chat.
func SendNPCChat(c *evenet.Conn, accountID int64, chID int64, senderID int64, senderName string, senderTypeID int64, text string) error {
	if senderTypeID < firstCharacterTypeID || senderTypeID > lastCharacterTypeID {
		senderTypeID = defaultNPCChatTypeID
	}
	notifyArgs := buildSendMessageArgs(channelWire(chID), 1, senderID, senderName, 0, senderTypeID, 3, text)
	notify := buildOnLSCNotification(notifyArgs, uint64(accountID), 0)
	if notify == nil {
		return nil
	}
	return c.SendPacket(notify)
}

// SendAddressBookContact pushes an OnLSC AccessControl event for a contact the server added to
// charID's addressbook, since the client caches the ACL rowset from JoinChannels. It is addressed
// with the channel key the client filed the channel under.
// See docs/server-notes.md#lsc-address-book-push.
func SendAddressBookContact(c *evenet.Conn, accountID, charID, ownerID int64) error {
	if c == nil || charID == 0 || ownerID == 0 {
		return nil
	}
	args := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyInt{Value: uint64(ownerID)},        // accessor
		marshal.PyInt{Value: uint64(chtModeSpeaker)}, // mode
		marshal.PyNone{},            // untilWhen
		marshal.PyNone{},            // originalMode
		marshal.PyString{Value: ""}, // admin
		marshal.PyString{Value: ""}, // reason
	}}
	senderInfo := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyNone{},                     // whoAllianceID
		marshal.PyInt{Value: 0},              // whoCorpID
		marshal.PyInt{Value: uint64(charID)}, // who (int form, no EOL hint)
		marshal.PyInt{Value: 3},              // whoRole
		marshal.PyInt{Value: 0},              // whoCorpRole
	}}
	notifyArgs := marshal.PyTuple{Items: []marshal.PyRep{
		channelWire(charID),
		marshal.PyInt{Value: 1},
		marshal.PyString{Value: "AccessControl"},
		senderInfo,
		args,
	}}
	notify := buildOnLSCNotification(notifyArgs, uint64(accountID), 0)
	if notify == nil {
		return nil
	}
	return c.SendPacket(notify)
}

// buildSendMessageArgs constructs the 5-element args tuple for an
// OnLSC_SendMessage notification:
//
//	(channelID, memberCount, "SendMessage", senderInfo, (message,))
//
// The message gets resetChatLinkStyle (linkstyle.go) so a wrapped link does not tint the rest.
func buildSendMessageArgs(channelID marshal.PyRep, memberCount int, senderID int64, senderName string, corpID, typeID, role int64, message string) marshal.PyRep {
	message = resetChatLinkStyle(message, role)
	senderInfo := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyNone{},                     // whoAllianceID, always None
		marshal.PyInt{Value: uint64(corpID)}, // whoCorpID
		marshal.PyList{Items: []marshal.PyRep{ // who: [senderID, senderName, senderType]
			marshal.PyInt{Value: uint64(senderID)},
			marshal.PyString{Value: senderName},
			marshal.PyInt{Value: uint64(typeID)}, // senderType = character typeID
		}},
		marshal.PyInt{Value: uint64(role)}, // whoRole (from account.role)
		marshal.PyInt{Value: 0},            // whoCorpRole
	}}

	return marshal.PyTuple{Items: []marshal.PyRep{
		channelID, // channelID (encoded form as sent by client)
		marshal.PyInt{Value: uint64(memberCount)},
		marshal.PyString{Value: "SendMessage"},
		senderInfo,
		marshal.PyTuple{Items: []marshal.PyRep{marshal.PyString{Value: message}}},
	}}
}

// buildOnLSCNotification wraps args in a macho.Notification to the OnLSC multicast and returns
// the serialized bytes. userID is the receiving account. sn is unused: OnLSC notifications
// carry no named_payload.
func buildOnLSCNotification(args marshal.PyRep, userID, sn uint64) []byte {
	innerStream := &marshal.PySubStream{
		Decoded: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: 0},
			marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyInt{Value: 1}, // remoteObject
				args,
			}},
		}},
	}

	payload := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: 0},
			innerStream,
		}},
		marshal.PyNone{},
	}}

	_ = sn // sn is unused
	pkt := &evenet.PyPacket{
		TypeString: "macho.Notification",
		Type:       evenet.Notification,
		Source: evenet.PyAddress{
			Type:   evenet.AddrNode,
			TypeID: globalNodeID,
		},
		Dest: evenet.PyAddress{
			Type:        evenet.AddrBroadcast,
			Service:     "OnLSC",
			BcastIDType: "*multicastID",
		},
		UserID:  userID,
		Payload: payload,
		// No named_payload.
	}

	b, err := marshal.Marshal(pkt.Encode())
	if err != nil {
		log.Printf("buildOnLSCNotification: marshal error: %v", err)
		return nil
	}
	if lscWireDump {
		log.Printf("buildOnLSCNotification: userID=%d sn=%d len=%d bytes: %x", userID, sn, len(b), b)
	}
	return b
}

// lscWireDump enables the raw OnLSC wire dump, one log line per notification per recipient.
// Set EVE_LSC_WIRE_DUMP when working on the OnLSC wire format.
var lscWireDump = os.Getenv("EVE_LSC_WIRE_DUMP") != ""

// globalNodeID matches the server's node ID used in all notifications.
const globalNodeID uint64 = 888444

// ── LeaveChannels / LeaveChannel ─────────────────────────────────────────────

// handleLeaveChannels handles a bulk channel-leave request.
// Args: ([channelDescriptor, ...], unsubscribe)
func handleLeaveChannels(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, reg *Registry, sqldb *sql.DB) error {
	if len(cs.ArgTuple.Items) < 1 {
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	var items []marshal.PyRep
	switch v := cs.ArgTuple.Items[0].(type) {
	case marshal.PyList:
		items = v.Items
	case marshal.PyTuple:
		items = v.Items
	}
	meta := reg.GetMeta(c)
	for _, item := range items {
		if id, ok := decodeLeaveEntry(item); ok {
			leaveAndBroadcast(c, id, meta, reg, ctx, sqldb)
			log.Printf("[%s] → LSC.LeaveChannels channel=%d", addr, id)
		} else {
			log.Printf("[%s] LSC.LeaveChannels: undecodable entry %v", addr, item)
		}
	}
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// handleLeaveChannel handles a single channel-leave.
// Args: (channelDescriptor, unsubscribe)
func handleLeaveChannel(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, reg *Registry, sqldb *sql.DB) error {
	if len(cs.ArgTuple.Items) < 1 {
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	if id, ok := decodeLeaveEntry(cs.ArgTuple.Items[0]); ok {
		meta := reg.GetMeta(c)
		leaveAndBroadcast(c, id, meta, reg, ctx, sqldb)
		log.Printf("[%s] → LSC.LeaveChannel channel=%d", addr, id)
	}
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// leaveAndBroadcast removes c from chID in the registry, notifies remaining
// members with an OnLSC_LeaveChannel notification, and deletes the DB row.
func leaveAndBroadcast(c *evenet.Conn, chID int64, meta *Member, reg *Registry, ctx context.Context, sqldb *sql.DB) {
	before, newCount := reg.LeaveAndSnapshot(c, chID)
	if meta == nil || meta.CharID == 0 {
		return
	}
	notifyArgs := buildJoinLeaveArgs(channelWire(chID), newCount, meta, "LeaveChannel")
	for _, m := range before {
		if m.Conn == nil || m.Conn == c {
			continue
		}
		notify := buildOnLSCNotification(notifyArgs, uint64(m.AccountID), reg.NextSN(m.Conn))
		if notify == nil {
			continue
		}
		m.Conn.SendPacket(notify) //nolint:errcheck
	}
	if isGangChannel(chID) {
		return // in-memory only, no DB row
	}
	if err := evedb.DeleteChannelChar(ctx, sqldb, chID, meta.CharID); err != nil {
		log.Printf("leaveAndBroadcast: DeleteChannelChar(%d,%d): %v", chID, meta.CharID, err)
	}
}

// HandleDisconnect is called when a client connection closes.  It broadcasts
// OnLSC_LeaveChannel to remaining members of every channel the connection was
// in, removes it from the registry, and cleans up its DB rows.
func HandleDisconnect(conn *evenet.Conn, reg *Registry, sqldb *sql.DB) {
	meta := reg.GetMeta(conn)
	channels := reg.GetChannels(conn)

	if meta != nil && meta.CharID != 0 && len(channels) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		for _, chID := range channels {
			before, newCount := reg.LeaveAndSnapshot(conn, chID)
			notifyArgs := buildJoinLeaveArgs(channelWire(chID), newCount, meta, "LeaveChannel")
			for _, m := range before {
				if m.Conn == nil || m.Conn == conn {
					continue
				}
				notify := buildOnLSCNotification(notifyArgs, uint64(m.AccountID), reg.NextSN(m.Conn))
				if notify == nil {
					continue
				}
				m.Conn.SendPacket(notify) //nolint:errcheck
			}
		}

		if err := evedb.DeleteAllChannelChars(ctx, sqldb, meta.CharID); err != nil {
			log.Printf("HandleDisconnect: DeleteAllChannelChars(%d): %v", meta.CharID, err)
		}
	}

	reg.Remove(conn)
}

// buildJoinLeaveArgs constructs the 5-element args tuple for OnLSC_JoinChannel
// or OnLSC_LeaveChannel:  (channelID, memberCount, method, senderInfo, ())
func buildJoinLeaveArgs(channelID marshal.PyRep, memberCount int, meta *Member, method string) marshal.PyRep {
	return buildOnLSCArgs(channelID, memberCount, meta, method, marshal.PyTuple{})
}

// ── GetMyMessages ─────────────────────────────────────────────────────────────

var evemailHeader = []marshal.PyRep{
	marshal.PyString{Value: "channelID"},
	marshal.PyString{Value: "messageID"},
	marshal.PyString{Value: "senderID"},
	marshal.PyString{Value: "subject"},
	marshal.PyString{Value: "created"},
	marshal.PyString{Value: "read"},
}

func emptyMailRowset() marshal.PyRep {
	return marshal.PyObject{
		Type: "util.Rowset",
		Args: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "header"}, Value: marshal.PyList{Items: evemailHeader}},
			{Key: marshal.PyString{Value: "RowClass"}, Value: marshal.PyString{Value: "util.Row", IsType1: true}},
			{Key: marshal.PyString{Value: "lines"}, Value: marshal.PyList{}},
		}},
	}
}

// handleGetMyMessages returns the character's inbox from evemail: personal, corp and alliance
// mailboxes in one rowset. See docs/server-notes.md#lsc-mailboxes.
func handleGetMyMessages(ctx context.Context, c *evenet.Conn, addr string, pkt *evenet.PyPacket, accountID, charID int64, sqldb *sql.DB) error {
	if charID == 0 {
		log.Printf("[%s] → LSC.GetMyMessages (no charID, empty)", addr)
		return server.SendCallRsp(c, pkt, accountID, emptyMailRowset())
	}

	// The client splits the rowset by channelID. Shared rows take read state from evemailRead; -1
	// stands for a mailbox the pilot lacks.
	scope := loadMailScope(ctx, sqldb, charID)
	corpBox, allyBox := int64(-1), int64(-1)
	if scope.corpID != 0 {
		corpBox = scope.corpID
	}
	if scope.allianceID != 0 {
		allyBox = scope.allianceID
	}
	_ = ensureEvemailRead(ctx, sqldb)
	rows, err := sqldb.QueryContext(ctx, `
		SELECT e.channelID, e.messageID, e.senderID, e.subject, e.created,
		       CASE WHEN e.channelID = ? THEN e.`+"`read`"+`
		            ELSE (r.messageID IS NOT NULL) END
		  FROM evemail e
		  LEFT JOIN evemailRead r ON r.messageID = e.messageID AND r.characterID = ?
		 WHERE e.channelID IN (?, ?, ?)
		 ORDER BY e.messageID DESC`,
		charID, charID, charID, corpBox, allyBox)
	if err != nil {
		log.Printf("[%s] LSC.GetMyMessages db: %v (returning empty)", addr, err)
		return server.SendCallRsp(c, pkt, accountID, emptyMailRowset())
	}
	defer rows.Close()

	var lines []marshal.PyRep
	for rows.Next() {
		var channelID, messageID, senderID, created, read int64
		var subject string
		if err := rows.Scan(&channelID, &messageID, &senderID, &subject, &created, &read); err != nil {
			continue
		}
		lines = append(lines, marshal.PyList{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(channelID)},
			marshal.PyInt{Value: uint64(messageID)},
			marshal.PyInt{Value: uint64(senderID)},
			marshal.PyString{Value: subject},
			marshal.PyInt{Value: uint64(created)},
			marshal.PyInt{Value: uint64(read)},
		}})
	}
	if lines == nil {
		lines = []marshal.PyRep{}
	}

	result := marshal.PyObject{
		Type: "util.Rowset",
		Args: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "header"}, Value: marshal.PyList{Items: evemailHeader}},
			{Key: marshal.PyString{Value: "RowClass"}, Value: marshal.PyString{Value: "util.Row", IsType1: true}},
			{Key: marshal.PyString{Value: "lines"}, Value: marshal.PyList{Items: lines}},
		}},
	}
	log.Printf("[%s] → LSC.GetMyMessages charID=%d rows=%d", addr, charID, len(lines))
	return server.SendCallRsp(c, pkt, accountID, result)
}

// ── Page (send mail) ──────────────────────────────────────────────────────────

// handlePage sends mail from the current character to one or more recipients.
// Client calls: LSC.Page(recipients, subject, body)
func handlePage(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID int64, reg *Registry, sqldb *sql.DB) error {
	args := cs.ArgTuple.Items
	if len(args) < 3 {
		log.Printf("[%s] LSC.Page: expected 3 args, got %d", addr, len(args))
		return server.SendCallException(c, pkt, accountID, nil)
	}

	// Parse recipients: a PyList or PyTuple of charIDs.
	var recipients []int64
	switch v := args[0].(type) {
	case marshal.PyList:
		for _, item := range v.Items {
			if id, ok := item.(marshal.PyInt); ok {
				recipients = append(recipients, int64(id.Value))
			}
		}
	case marshal.PyTuple:
		for _, item := range v.Items {
			if id, ok := item.(marshal.PyInt); ok {
				recipients = append(recipients, int64(id.Value))
			}
		}
	case marshal.PyInt:
		recipients = append(recipients, int64(v.Value))
	}
	if len(recipients) == 0 {
		log.Printf("[%s] LSC.Page: no valid recipients in %T", addr, args[0])
		return server.SendCallException(c, pkt, accountID, nil)
	}

	// Parse subject.
	var subject string
	if v, ok := args[1].(marshal.PyString); ok {
		subject = v.Value
	}

	// Parse body.
	var body string
	if v, ok := args[2].(marshal.PyString); ok {
		body = v.Value
	}

	// A corp or alliance mailbox takes posts from its own members only. Refused recipients are
	// dropped; if none remain the pilot gets a CustomInfo error.
	scope := loadMailScope(ctx, sqldb, charID)
	allowed := recipients[:0:0]
	for _, id := range recipients {
		if mayPostTo(ctx, sqldb, scope, id) {
			allowed = append(allowed, id)
		} else {
			log.Printf("[%s] LSC.Page: char %d refused posting to mailbox %d", addr, charID, id)
		}
	}
	if len(allowed) == 0 {
		return server.SendCallException(c, pkt, accountID, server.BuildUserError(
			"CustomInfo", map[string]marshal.PyRep{
				"info": marshal.PyString{Value: "You can only post to your own corporation's or alliance's mailbox."},
			}))
	}
	recipients = allowed

	created := server.Win32TimeNow()
	var lines []marshal.PyRep

	for _, recipientID := range recipients {
		res, err := sqldb.ExecContext(ctx,
			`INSERT INTO evemail (channelID, senderID, subject, created, `+"`read`"+`) VALUES (?, ?, ?, ?, 0)`,
			recipientID, charID, subject, created)
		if err != nil {
			log.Printf("[%s] LSC.Page: INSERT evemail: %v", addr, err)
			continue
		}
		messageID, err := res.LastInsertId()
		if err != nil {
			log.Printf("[%s] LSC.Page: LastInsertId: %v", addr, err)
			continue
		}

		_, err = sqldb.ExecContext(ctx,
			`INSERT INTO evemailDetails (messageID, mimeTypeID, attachment) VALUES (?, 1, ?)`,
			messageID, body)
		if err != nil {
			log.Printf("[%s] LSC.Page: INSERT evemailDetails: %v", addr, err)
		}

		lines = append(lines, marshal.PyList{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(recipientID)},
			marshal.PyInt{Value: uint64(messageID)},
			marshal.PyInt{Value: uint64(charID)},
			marshal.PyString{Value: subject},
			marshal.PyInt{Value: created},
			marshal.PyInt{Value: 0},
		}})

		// Live-push OnMessage(recipients, messageID, senderID, subject, created) per stored row; the
		// client inbox picks the blink box by matching the recipient against session charid/corpid.
		if reg != nil {
			onMsgArgs := marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyList{Items: []marshal.PyRep{marshal.PyInt{Value: uint64(recipientID)}}},
				marshal.PyInt{Value: uint64(messageID)},
				marshal.PyInt{Value: uint64(charID)},
				marshal.PyString{Value: subject},
				marshal.PyInt{Value: created},
			}}
			// An alliance mailbox reaches members through their corps; Member holds the corpID only.
			match := []int64{recipientID}
			if recipientID == scope.allianceID && scope.allianceID != 0 {
				match = append(match, allianceCorpIDs(ctx, sqldb, recipientID)...)
			}
			for _, m := range reg.OnlineRecipients(match...) {
				if err := server.SendNotification(m.Conn, m.AccountID, "OnMessage", "charid", onMsgArgs); err != nil {
					log.Printf("[%s] LSC.Page: OnMessage push to char %d: %v", addr, m.CharID, err)
				}
			}
		}
	}

	if lines == nil {
		lines = []marshal.PyRep{}
	}
	result := marshal.PyObject{
		Type: "util.Rowset",
		Args: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "header"}, Value: marshal.PyList{Items: evemailHeader}},
			{Key: marshal.PyString{Value: "RowClass"}, Value: marshal.PyString{Value: "util.Row", IsType1: true}},
			{Key: marshal.PyString{Value: "lines"}, Value: marshal.PyList{Items: lines}},
		}},
	}
	log.Printf("[%s] → LSC.Page from=%d recipients=%v subject=%q", addr, charID, recipients, subject)
	return server.SendCallRsp(c, pkt, accountID, result)
}

// ── MarkMessagesRead ──────────────────────────────────────────────────────────

func handleMarkMessagesRead(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID int64, sqldb *sql.DB) error {
	ids := extractMessageIDs(cs.ArgTuple.Items)
	for _, mid := range ids {
		_, _ = sqldb.ExecContext(ctx,
			`UPDATE evemail SET `+"`read`"+`=1 WHERE messageID = ? AND channelID = ?`,
			mid, charID)
	}
	// Corp and alliance rows are shared, so their read state is per pilot.
	markSharedRead(ctx, sqldb, loadMailScope(ctx, sqldb, charID), ids)
	log.Printf("[%s] → LSC.MarkMessagesRead charID=%d ids=%v", addr, charID, ids)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// ── DeleteMessages ────────────────────────────────────────────────────────────

func handleDeleteMessages(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID int64, sqldb *sql.DB) error {
	// Wire shape is (channelID, messageIDs): the mailbox comes first.
	args := cs.ArgTuple.Items
	if len(args) >= 2 {
		args = args[1:]
	}
	ids := extractMessageIDs(args)
	for _, mid := range ids {
		_, _ = sqldb.ExecContext(ctx, `DELETE FROM evemailDetails WHERE messageID = ?`, mid)
		_, _ = sqldb.ExecContext(ctx, `DELETE FROM evemail WHERE messageID = ? AND channelID = ?`, mid, charID)
	}
	log.Printf("[%s] → LSC.DeleteMessages charID=%d ids=%v", addr, charID, ids)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// ── GetMessageDetails ─────────────────────────────────────────────────────────

// handleGetMessageDetails answers LSC.GetMessageDetails(mailboxID, messageID) with
// (msg, attachments). msg needs .senderID/.created/.body; the client renders only
// 'x-eve/bookmark' attachments and handlePage stores none, so the list is empty.
func handleGetMessageDetails(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID int64, sqldb *sql.DB) error {
	args := cs.ArgTuple.Items
	if len(args) < 2 {
		log.Printf("[%s] LSC.GetMessageDetails: expected 2 args, got %d", addr, len(args))
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	mailboxID, ok := args[0].(marshal.PyInt)
	if !ok {
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	msgID, ok := args[1].(marshal.PyInt)
	if !ok {
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	// Read through a mailbox the pilot belongs to. Other mailing lists are unchecked.
	if charID != 0 {
		scope := loadMailScope(ctx, sqldb, charID)
		mb := int64(mailboxID.Value)
		if !scope.ownsMailbox(mb) && (classifyMailbox(ctx, sqldb, mb) != mailboxOther || isCharacterID(ctx, sqldb, mb)) {
			log.Printf("[%s] LSC.GetMessageDetails: char %d refused mailbox %d", addr, charID, mb)
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
	}

	var senderID, created int64
	var subject string
	err := sqldb.QueryRowContext(ctx,
		`SELECT senderID, created, subject FROM evemail WHERE messageID = ? AND channelID = ?`,
		int64(msgID.Value), int64(mailboxID.Value)).Scan(&senderID, &created, &subject)
	if err != nil {
		log.Printf("[%s] LSC.GetMessageDetails(mailbox=%d, msg=%d): %v", addr, mailboxID.Value, msgID.Value, err)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}

	// mimeTypeID 1 is the message body (see handlePage).
	var body string
	if err := sqldb.QueryRowContext(ctx,
		`SELECT attachment FROM evemailDetails WHERE messageID = ? AND mimeTypeID = 1`,
		int64(msgID.Value)).Scan(&body); err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("[%s] LSC.GetMessageDetails(%d): body: %v", addr, msgID.Value, err)
	}

	msg := marshal.PyObject{
		Type: "util.KeyVal",
		Args: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "messageID"}, Value: marshal.PyInt{Value: msgID.Value}},
			{Key: marshal.PyString{Value: "senderID"}, Value: marshal.PyInt{Value: uint64(senderID)}},
			{Key: marshal.PyString{Value: "created"}, Value: marshal.PyInt{Value: uint64(created)}},
			{Key: marshal.PyString{Value: "subject"}, Value: marshal.PyString{Value: subject}},
			{Key: marshal.PyString{Value: "body"}, Value: marshal.PyString{Value: body}},
		}},
	}
	result := marshal.PyTuple{Items: []marshal.PyRep{msg, marshal.PyList{}}}
	log.Printf("[%s] → LSC.GetMessageDetails mailbox=%d msgID=%d bodyLen=%d", addr, mailboxID.Value, msgID.Value, len(body))
	return server.SendCallRsp(c, pkt, accountID, result)
}

// extractMessageIDs pulls int IDs from the first arg (list/tuple of ints).
func extractMessageIDs(args []marshal.PyRep) []int64 {
	if len(args) < 1 {
		return nil
	}
	var ids []int64
	switch v := args[0].(type) {
	case marshal.PyList:
		for _, item := range v.Items {
			if id, ok := item.(marshal.PyInt); ok {
				ids = append(ids, int64(id.Value))
			}
		}
	case marshal.PyTuple:
		for _, item := range v.Items {
			if id, ok := item.(marshal.PyInt); ok {
				ids = append(ids, int64(id.Value))
			}
		}
	case marshal.PyInt:
		ids = append(ids, int64(v.Value))
	}
	return ids
}

// ── GetMembers ────────────────────────────────────────────────────────────────

// handleGetMembers returns the channel roster from the in-memory registry (online players, not
// DB rows). menusvc.__GetInviteMenu reaches it through lscengine.GetMembers.
// Args: (channelDescriptor,).
func handleGetMembers(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, reg *Registry, sqldb *sql.DB) error {
	if len(cs.ArgTuple.Items) < 1 {
		return server.SendCallRsp(c, pkt, accountID, emptyCharsRowset())
	}
	chID, ok := decodeChannelID(cs.ArgTuple.Items[0])
	if !ok {
		log.Printf("[%s] LSC.GetMembers: cannot decode channelID from %T", addr, cs.ArgTuple.Items[0])
		return server.SendCallRsp(c, pkt, accountID, emptyCharsRowset())
	}
	chars := membersToCharsRowset(reg.Members(chID))
	log.Printf("[%s] → LSC.GetMembers channel=%d", addr, chID)
	return server.SendCallRsp(c, pkt, accountID, chars)
}

// membersToCharsRowset converts registry members to the channelChars util.Rowset: charID, corpID,
// allianceID, role, extra. extra is a plain [ownerID, ownerName, typeID] list, not a util.Row:
// cfg.eveowners.Hint len()s and slices it. See docs/server-notes.md#lsc-member-rowset.
func membersToCharsRowset(members []*Member) marshal.PyRep {
	headerList := make([]marshal.PyRep, len(charsRowsetCols))
	for i, col := range charsRowsetCols {
		headerList[i] = marshal.PyString{Value: col}
	}
	var lines []marshal.PyRep
	for _, m := range members {
		if m.CharID == 0 {
			continue
		}
		typeID := m.TypeID
		if typeID == 0 {
			typeID = 1378 // fallback character typeID
		}
		extra := marshal.PyList{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(m.CharID)},
			marshal.PyString{Value: m.CharName},
			marshal.PyInt{Value: uint64(typeID)},
		}}

		row := marshal.PyList{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(m.CharID)},
			marshal.PyInt{Value: uint64(m.CorpID)},
			marshal.PyNone{}, // allianceID (None = no alliance, matches OnLSC rows)
			marshal.PyInt{Value: uint64(m.Role)},
			extra,
		}}
		lines = append(lines, row)
	}
	if lines == nil {
		lines = []marshal.PyRep{}
	}
	header := marshal.PyList{Items: headerList}
	return marshal.PyObject{
		Type: "util.Rowset",
		Args: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "header"}, Value: header},
			{Key: marshal.PyString{Value: "RowClass"}, Value: marshal.PyString{Value: "util.Row", IsType1: true}},
			{Key: marshal.PyString{Value: "lines"}, Value: marshal.PyList{Items: lines}},
			{Key: marshal.PyString{Value: "columns"}, Value: header},
		}},
	}
}

// ── System channel swap ──────────────────────────────────────────────────────

// SwapSystemChannel handles the server-side leave/join of the local (solar
// system) chat channel when a player jumps between systems.  It leaves the
// old system channel with proper notifications and joins the new one, so the
// client sees a clean channel switch instead of accumulating tabs.
func SwapSystemChannel(c *evenet.Conn, accountID int64, oldSysID, newSysID int64, reg *Registry, sqldb *sql.DB) {
	if reg == nil {
		return
	}
	meta := reg.GetMeta(c)
	if meta == nil || meta.CharID == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Leave old system channel.
	if oldSysID != 0 {
		leaveAndBroadcast(c, oldSysID, meta, reg, ctx, sqldb)

		// leaveAndBroadcast notifies only other members; the client needs its own LeaveChannel to
		// close the tab.
		selfLeaveArgs := buildJoinLeaveArgs(channelWire(oldSysID), 0, meta, "LeaveChannel")
		selfLeave := buildOnLSCNotification(selfLeaveArgs, uint64(accountID), reg.NextSN(c))
		if selfLeave != nil {
			c.SendPacket(selfLeave) //nolint:errcheck
		}

		log.Printf("[lsc] SwapSystemChannel: left old system channel %d (charID=%d)", oldSysID, meta.CharID)
	}

	// Join new system channel.
	if newSysID != 0 {
		displayName := evedb.ResolveChannelDisplayName(ctx, sqldb, newSysID)
		if err := evedb.EnsureChannel(ctx, sqldb, newSysID, newSysID, displayName); err != nil {
			log.Printf("[lsc] SwapSystemChannel: EnsureChannel(%d): %v", newSysID, err)
		}
		if err := evedb.UpsertChannelChar(ctx, sqldb, newSysID, meta.CharID, meta.CorpID); err != nil {
			log.Printf("[lsc] SwapSystemChannel: UpsertChannelChar(%d,%d): %v", newSysID, meta.CharID, err)
		}

		// No self-notification here: the client's own JoinChannels, triggered by the session change,
		// creates the tab and backfills members. A self-join sent here duplicates the Local tab.
		existingMembers, newCount := reg.JoinAndSnapshot(c, newSysID)

		// Notify existing members about the new joiner so their roster updates.
		selfJoinArgs := buildJoinLeaveArgs(channelWire(newSysID), newCount, meta, "JoinChannel")
		for _, m := range existingMembers {
			if m.Conn == nil || m.Conn == c {
				continue
			}
			notify := buildOnLSCNotification(selfJoinArgs, uint64(m.AccountID), reg.NextSN(m.Conn))
			if notify != nil {
				m.Conn.SendPacket(notify) //nolint:errcheck
			}
		}

		log.Printf("[lsc] SwapSystemChannel: joined new system channel %d (charID=%d)", newSysID, meta.CharID)
	}
}

// ── compile-time import check ─────────────────────────────────────────────────

var _ = fmt.Sprintf // keep fmt import satisfied
