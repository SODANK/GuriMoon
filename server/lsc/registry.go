package lsc

import (
	"sync"

	evenet "gurimoon/net"
)

// Member holds per-connection info for building sender info in OnLSC notifications.
type Member struct {
	Conn      *evenet.Conn
	AccountID int64
	CharID    int64
	CharName  string
	CorpID    int64
	TypeID    int64 // character typeID for portrait display
	Role      int64
	CorpRole  int64
	nextSN    uint64 // per-connection notification sequence number
}

// Registry tracks in-memory channel membership across all connected clients.
// It is safe for concurrent use by multiple goroutines.
type Registry struct {
	mu sync.RWMutex

	// channels maps channelID → current members of that channel.
	channels map[int64][]*Member

	// connMeta maps conn → member metadata (shared pointer reused across channels).
	connMeta map[*evenet.Conn]*Member

	// connChannels maps conn → slice of channelIDs the connection belongs to.
	connChannels map[*evenet.Conn][]int64

	// owners maps an unpersisted channel to the character that created it. The entry
	// goes when the channel empties.
	owners map[int64]int64
}

// NewRegistry creates an empty Registry.
func NewRegistry() *Registry {
	return &Registry{
		channels:     make(map[int64][]*Member),
		connMeta:     make(map[*evenet.Conn]*Member),
		connChannels: make(map[*evenet.Conn][]int64),
		owners:       make(map[int64]int64),
	}
}

// ClaimOwner records charID as the creator of an unpersisted channel that has none yet.
func (r *Registry) ClaimOwner(channelID, charID int64) {
	if charID == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, taken := r.owners[channelID]; !taken {
		r.owners[channelID] = charID
	}
}

// Owner returns the creator of an unpersisted channel, 0 when none is recorded.
func (r *Registry) Owner(channelID int64) int64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.owners[channelID]
}

// SetMeta stores character metadata for a connection.
// Called once at character select so subsequent Join / broadcast operations
// have the sender name, corp, etc. available.
func (r *Registry) SetMeta(conn *evenet.Conn, accountID, charID int64, charName string, corpID, typeID, role int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.connMeta[conn]
	if m == nil {
		m = &Member{Conn: conn}
		r.connMeta[conn] = m
	}
	m.AccountID = accountID
	m.CharID = charID
	m.CharName = charName
	m.CorpID = corpID
	m.TypeID = typeID
	m.Role = role
}

// OnlineRecipients returns the connected members whose character or corp matches
// any of the given mailbox IDs, for evemail OnMessage fan-out. An alliance mailbox
// is fanned out by passing the member corpIDs (handlePage), not an allianceID.
func (r *Registry) OnlineRecipients(mailboxIDs ...int64) []*Member {
	r.mu.RLock()
	defer r.mu.RUnlock()
	want := make(map[int64]bool, len(mailboxIDs))
	for _, id := range mailboxIDs {
		want[id] = true
	}
	var out []*Member
	for _, m := range r.connMeta {
		if m.Conn == nil {
			continue
		}
		if want[m.CharID] || want[m.CorpID] {
			out = append(out, m)
		}
	}
	return out
}

// Join adds the connection to a channel and returns the new member count.
// If the connection is already a member, it is a no-op and returns the current count.
func (r *Registry) Join(conn *evenet.Conn, channelID int64) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Ensure a meta entry exists (may be empty if SetMeta was not called yet).
	m := r.connMeta[conn]
	if m == nil {
		m = &Member{Conn: conn}
		r.connMeta[conn] = m
	}

	// Idempotent: skip if already a member.
	for _, existing := range r.channels[channelID] {
		if existing.Conn == conn {
			return len(r.channels[channelID])
		}
	}

	r.channels[channelID] = append(r.channels[channelID], m)
	r.connChannels[conn] = append(r.connChannels[conn], channelID)
	return len(r.channels[channelID])
}

// JoinAndSnapshot atomically adds the connection to a channel and returns the
// members present before the join plus the member count after it. Use it instead
// of Members() then Join(), which lets concurrent joiners miss each other's roster event.
func (r *Registry) JoinAndSnapshot(conn *evenet.Conn, channelID int64) (existing []*Member, newCount int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	members := r.channels[channelID]
	existing = make([]*Member, len(members))
	copy(existing, members)

	m := r.connMeta[conn]
	if m == nil {
		m = &Member{Conn: conn}
		r.connMeta[conn] = m
	}
	for _, e := range members {
		if e.Conn == conn {
			return existing, len(members)
		}
	}
	r.channels[channelID] = append(r.channels[channelID], m)
	r.connChannels[conn] = append(r.connChannels[conn], channelID)
	return existing, len(r.channels[channelID])
}

// Leave removes the connection from a single channel.
func (r *Registry) Leave(conn *evenet.Conn, channelID int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.leaveChannel(conn, channelID)
}

// LeaveAndSnapshot atomically removes the connection from a channel and returns
// the members present before the leave plus the member count after it (atomic with
// the mutation, see JoinAndSnapshot).
func (r *Registry) LeaveAndSnapshot(conn *evenet.Conn, channelID int64) (before []*Member, newCount int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	members := r.channels[channelID]
	before = make([]*Member, len(members))
	copy(before, members)
	r.leaveChannel(conn, channelID)
	return before, len(r.channels[channelID])
}

// Remove removes the connection from all channels (called on disconnect).
func (r *Registry) Remove(conn *evenet.Conn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ch := range r.connChannels[conn] {
		members := r.channels[ch]
		for i, m := range members {
			if m.Conn == conn {
				r.channels[ch] = append(members[:i], members[i+1:]...)
				break
			}
		}
		if len(r.channels[ch]) == 0 {
			delete(r.channels, ch)
			delete(r.owners, ch)
		}
	}
	delete(r.connChannels, conn)
	delete(r.connMeta, conn)
}

// Members returns a snapshot of the current members of a channel (may be empty).
func (r *Registry) Members(channelID int64) []*Member {
	r.mu.RLock()
	defer r.mu.RUnlock()
	members := r.channels[channelID]
	result := make([]*Member, len(members))
	copy(result, members)
	return result
}

// Count returns the current member count for a channel.
func (r *Registry) Count(channelID int64) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.channels[channelID])
}

// GetChannels returns a snapshot of all channel IDs that conn currently belongs to.
// Returns a nil/empty slice if the connection is not in any channel.
func (r *Registry) GetChannels(conn *evenet.Conn) []int64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	chs := r.connChannels[conn]
	result := make([]int64, len(chs))
	copy(result, chs)
	return result
}

// FindByCharID returns the member metadata for an online character, or nil
// if that character has no live connection.  Used by Invite to locate the
// invitee's connection.
func (r *Registry) FindByCharID(charID int64) *Member {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, m := range r.connMeta {
		if m.CharID == charID && m.Conn != nil {
			return m
		}
	}
	return nil
}

// GetMeta returns the stored metadata for a connection, or nil.
func (r *Registry) GetMeta(conn *evenet.Conn) *Member {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.connMeta[conn]
}

// NextSN atomically returns the next sequence number for a connection's
// outgoing notifications and increments the counter.
// Thread-safe: uses the registry write lock.
func (r *Registry) NextSN(conn *evenet.Conn) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.connMeta[conn]
	if m == nil {
		return 0
	}
	sn := m.nextSN
	m.nextSN++
	return sn
}

// JoinBot adds a synthetic (no-Conn) member to a channel. Idempotent on
// (channelID, charID). Returns the new member count. The client moves its roster
// only on OnLSC JoinChannel / LeaveChannel events, so those go out to the live
// members after the registry lock is released.
func (r *Registry) JoinBot(channelID, charID, accountID, corpID, typeID, role int64, charName string) int {
	r.mu.Lock()
	for _, existing := range r.channels[channelID] {
		if existing.Conn == nil && existing.CharID == charID {
			n := len(r.channels[channelID])
			r.mu.Unlock()
			return n
		}
	}
	m := &Member{
		Conn:      nil,
		AccountID: accountID,
		CharID:    charID,
		CharName:  charName,
		CorpID:    corpID,
		TypeID:    typeID,
		Role:      role,
	}
	r.channels[channelID] = append(r.channels[channelID], m)
	count := len(r.channels[channelID])
	audience := liveMembersLocked(r.channels[channelID])
	r.mu.Unlock()

	r.announceMembership(channelID, count, m, "JoinChannel", audience)
	return count
}

// LeaveBot removes a synthetic (no-Conn) bot member from a channel by charID.
// Idempotent. Returns the new member count.
func (r *Registry) LeaveBot(channelID, charID int64) int {
	r.mu.Lock()
	members := r.channels[channelID]
	var gone *Member
	for i, m := range members {
		if m.Conn == nil && m.CharID == charID {
			gone = m
			r.channels[channelID] = append(members[:i], members[i+1:]...)
			break
		}
	}
	if len(r.channels[channelID]) == 0 {
		delete(r.channels, channelID)
		delete(r.owners, channelID)
	}
	count := len(r.channels[channelID])
	audience := liveMembersLocked(r.channels[channelID])
	r.mu.Unlock()

	if gone != nil {
		r.announceMembership(channelID, count, gone, "LeaveChannel", audience)
	}
	return count
}

// liveMembersLocked snapshots the members of a channel that have a connection
// to send to. Caller must hold r.mu.
func liveMembersLocked(members []*Member) []*Member {
	var out []*Member
	for _, m := range members {
		if m.Conn != nil {
			out = append(out, m)
		}
	}
	return out
}

// announceMembership pushes an OnLSC JoinChannel / LeaveChannel roster event about
// subject to every connection in audience. Call with r.mu released: NextSN takes it.
func (r *Registry) announceMembership(channelID int64, count int, subject *Member, method string, audience []*Member) {
	if len(audience) == 0 || subject.CharID == 0 {
		return
	}
	args := buildJoinLeaveArgs(channelWire(channelID), count, subject, method)
	for _, m := range audience {
		notify := buildOnLSCNotification(args, uint64(m.AccountID), r.NextSN(m.Conn))
		if notify == nil {
			continue
		}
		m.Conn.SendPacket(notify) //nolint:errcheck
	}
}

// leaveChannel is the internal (lock-held) helper.
func (r *Registry) leaveChannel(conn *evenet.Conn, channelID int64) {
	members := r.channels[channelID]
	for i, m := range members {
		if m.Conn == conn {
			r.channels[channelID] = append(members[:i], members[i+1:]...)
			break
		}
	}
	if len(r.channels[channelID]) == 0 {
		delete(r.channels, channelID)
		delete(r.owners, channelID)
	}
	chs := r.connChannels[conn]
	for i, ch := range chs {
		if ch == channelID {
			r.connChannels[conn] = append(chs[:i], chs[i+1:]...)
			break
		}
	}
}
