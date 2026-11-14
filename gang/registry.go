package gang

import (
	"fmt"
	"sync"
)

// Member represents a gang member.
type Member struct {
	CharID int64
	CorpID int64
}

// Gang represents a fleet/gang.
type Gang struct {
	ID       int64
	LeaderID int64
	Members  map[int64]*Member // charID → member
	// TargetTags holds the shared overview target tags (itemID -> tag). The client sends ints 0-9 or
	// letter strings and expects them echoed back verbatim, so values are int64 or string.
	TargetTags map[int64]any
}

// Invitation is a pending gang invite awaiting the invitee's answer.
type Invitation struct {
	GangID    int64
	InviterID int64
	InviteeID int64
}

// Registry is a thread-safe in-memory gang membership tracker.
type Registry struct {
	mu      sync.RWMutex
	gangs   map[int64]*Gang        // gangID → gang
	chars   map[int64]int64        // charID → gangID
	invites map[int64][]Invitation // inviteeCharID → pending invites
	nextID  int64
}

// NewRegistry creates a new empty gang registry.
func NewRegistry() *Registry {
	return &Registry{
		gangs:   make(map[int64]*Gang),
		chars:   make(map[int64]int64),
		invites: make(map[int64][]Invitation),
		nextID:  1,
	}
}

// CreateGang creates a new gang with the given leader. Returns the new gang ID.
func (r *Registry) CreateGang(leaderCharID, leaderCorpID int64) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()

	id := r.nextID
	r.nextID++

	g := &Gang{
		ID:       id,
		LeaderID: leaderCharID,
		Members: map[int64]*Member{
			leaderCharID: {CharID: leaderCharID, CorpID: leaderCorpID},
		},
	}
	r.gangs[id] = g
	r.chars[leaderCharID] = id
	return id
}

// AddMember adds a character to an existing gang.
func (r *Registry) AddMember(gangID, charID, corpID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	g, ok := r.gangs[gangID]
	if !ok {
		return fmt.Errorf("gang %d not found", gangID)
	}
	g.Members[charID] = &Member{CharID: charID, CorpID: corpID}
	r.chars[charID] = gangID
	return nil
}

// RemoveMember removes a character from their gang. Returns the gang ID and whether the gang was
// disbanded (last member left). A leaving leader leaves the gang leaderless (LeaderID 0); the
// client offers Claim Leadership to the remaining members.
func (r *Registry) RemoveMember(charID int64) (gangID int64, disbanded bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	gangID, ok := r.chars[charID]
	if !ok {
		return 0, false
	}
	delete(r.chars, charID)

	g, ok := r.gangs[gangID]
	if !ok {
		return gangID, false
	}
	delete(g.Members, charID)

	if len(g.Members) <= 1 {
		for memberID := range g.Members {
			delete(r.chars, memberID)
		}
		delete(r.gangs, gangID)
		return gangID, true
	}

	if g.LeaderID == charID {
		g.LeaderID = 0
	}
	return gangID, false
}

// SetTargetTag records (or clears, when tag is nil) a shared overview target
// tag on the character's gang. Returns the gang ID, or 0 when the character
// is not in a gang.
func (r *Registry) SetTargetTag(charID, itemID int64, tag any) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()

	gangID := r.chars[charID]
	g := r.gangs[gangID]
	if g == nil {
		return 0
	}
	if tag == nil {
		delete(g.TargetTags, itemID)
		return gangID
	}
	if g.TargetTags == nil {
		g.TargetTags = make(map[int64]any)
	}
	g.TargetTags[itemID] = tag
	return gangID
}

// TargetTagsFor returns a copy of the target-tag map for the character's
// gang, or nil when the character is not in a gang.
func (r *Registry) TargetTagsFor(charID int64) map[int64]any {
	r.mu.RLock()
	defer r.mu.RUnlock()

	g := r.gangs[r.chars[charID]]
	if g == nil {
		return nil
	}
	out := make(map[int64]any, len(g.TargetTags))
	for k, v := range g.TargetTags {
		out[k] = v
	}
	return out
}

// GetCharGang returns the gangID for a character, or 0 if not in a gang.
func (r *Registry) GetCharGang(charID int64) int64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.chars[charID]
}

// GangIDs returns every live gang's ID.
func (r *Registry) GangIDs() []int64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]int64, 0, len(r.gangs))
	for id := range r.gangs {
		out = append(out, id)
	}
	return out
}

// GetGang returns a copy of the gang data, or nil if not found.
func (r *Registry) GetGang(gangID int64) *Gang {
	r.mu.RLock()
	defer r.mu.RUnlock()
	g, ok := r.gangs[gangID]
	if !ok {
		return nil
	}
	cp := &Gang{
		ID:       g.ID,
		LeaderID: g.LeaderID,
		Members:  make(map[int64]*Member, len(g.Members)),
	}
	for k, v := range g.Members {
		m := *v
		cp.Members[k] = &m
	}
	return cp
}

// BubbleGangData builds SetState gang data for the given character IDs.
// Returns gangCorps (unique gang-corp pairs) and charGangs (char-gang pairs)
// for all characters that are in a gang.
func (r *Registry) BubbleGangData(charIDs []int64) ([]GangCorpEntry, []CharGangEntry) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	seen := make(map[int64]bool)
	var gangCorps []GangCorpEntry
	var charGangs []CharGangEntry

	for _, charID := range charIDs {
		gangID := r.chars[charID]
		if gangID == 0 {
			continue
		}
		charGangs = append(charGangs, CharGangEntry{CharID: charID, GangID: gangID})

		if !seen[gangID] {
			seen[gangID] = true
			g := r.gangs[gangID]
			if g != nil {
				corpsSeen := make(map[int64]bool)
				for _, m := range g.Members {
					if !corpsSeen[m.CorpID] {
						corpsSeen[m.CorpID] = true
						gangCorps = append(gangCorps, GangCorpEntry{GangID: gangID, CorpID: m.CorpID})
					}
				}
			}
		}
	}

	return gangCorps, charGangs
}

// ─── invitations ───────────────────────────────────────────────────────────

// AddInvitation records a pending invite for the invitee. Duplicate invites
// to the same gang replace the earlier one.
func (r *Registry) AddInvitation(gangID, inviterID, inviteeID int64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	list := r.invites[inviteeID]
	for i := range list {
		if list[i].GangID == gangID {
			list[i].InviterID = inviterID
			r.invites[inviteeID] = list
			return
		}
	}
	r.invites[inviteeID] = append(list, Invitation{GangID: gangID, InviterID: inviterID, InviteeID: inviteeID})
}

// GetInvitations returns the pending invites for a character.
func (r *Registry) GetInvitations(charID int64) []Invitation {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Invitation, len(r.invites[charID]))
	copy(out, r.invites[charID])
	return out
}

// TakeInvitation removes and returns the pending invite for gangID. ok is
// false when no such invite exists.
func (r *Registry) TakeInvitation(charID, gangID int64) (inv Invitation, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	list := r.invites[charID]
	for i := range list {
		if list[i].GangID == gangID {
			inv = list[i]
			r.invites[charID] = append(list[:i], list[i+1:]...)
			if len(r.invites[charID]) == 0 {
				delete(r.invites, charID)
			}
			return inv, true
		}
	}
	return Invitation{}, false
}

// ClearInvitations drops every pending invite for a character.
func (r *Registry) ClearInvitations(charID int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.invites, charID)
}

// ─── leadership & disband ──────────────────────────────────────────────────

// SetLeader makes charID the leader of their gang. Returns the gang ID, or 0
// if the character is not in a gang.
func (r *Registry) SetLeader(charID int64) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()

	gangID := r.chars[charID]
	g := r.gangs[gangID]
	if g == nil {
		return 0
	}
	if _, ok := g.Members[charID]; !ok {
		return 0
	}
	g.LeaderID = charID
	return gangID
}

// ClearLeader drops the current leader of charID's gang, leaving it leaderless
// until somebody claims leadership. Returns the gang ID, or 0 if not in a gang.
func (r *Registry) ClearLeader(charID int64) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()

	gangID := r.chars[charID]
	g := r.gangs[gangID]
	if g == nil || g.LeaderID != charID {
		return 0
	}
	g.LeaderID = 0
	return gangID
}

// Disband removes every member and deletes the gang. Returns the gang ID and
// the character IDs that were in it.
func (r *Registry) Disband(gangID int64) (members []int64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	g := r.gangs[gangID]
	if g == nil {
		return nil
	}
	for charID := range g.Members {
		members = append(members, charID)
		delete(r.chars, charID)
	}
	delete(r.gangs, gangID)
	return members
}
