package gang

import (
	"sort"
	"sync"
	"time"

	"gurimoon/dogma"
)

// BoostBoard is the read side of gang bonuses, for code with a charID and no entity (attribute
// pushes, the rolebots ESP view). Live multipliers sit on destiny.Entity.GangBoost;
// cmd/gurimoon/gangboost.go writes both. It is in package gang so modules can import it.
type BoostBoard struct {
	mu      sync.RWMutex
	members map[int64]MemberBoost // charID → applied boost
	gangs   map[int64]GangView    // gangID → last resolve
}

// MemberBoost is what one gang member is currently receiving.
type MemberBoost struct {
	CharID        int64
	GangID        int64
	SolarSystemID int64
	Boost         dogma.GangBoost
}

// GangView is one gang's last resolve: the winning source per channel, per
// solar system (a bonus never crosses a system boundary), and every member's
// placement at the time.
type GangView struct {
	GangID  int64
	Winners map[int64][]dogma.GangContribution // solarSystemID → winners
	Members []MemberPlacement
	At      time.Time
}

// MemberPlacement is where a member was when the gang was last resolved.
// SolarSystemID 0 means not in space (docked, offline or loading), which
// neither gives nor receives a bonus.
type MemberPlacement struct {
	CharID        int64
	SolarSystemID int64
	Boost         dogma.GangBoost
}

// Boosts is the process-wide board.
var Boosts = &BoostBoard{
	members: make(map[int64]MemberBoost),
	gangs:   make(map[int64]GangView),
}

// ForChar returns the boost currently applied to charID (identity when none).
func (b *BoostBoard) ForChar(charID int64) dogma.GangBoost {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.members[charID].Boost
}

// Publish replaces gangID's view and the member entries it owns. A charID under gangID but absent
// from view.Members is removed, so a pilot who left stops reading as boosted. An entry owned by a
// different gang is left alone, so a late resolve of the old gang does not wipe a pilot who moved.
func (b *BoostBoard) Publish(view GangView) {
	b.mu.Lock()
	defer b.mu.Unlock()
	keep := make(map[int64]bool, len(view.Members))
	for _, m := range view.Members {
		keep[m.CharID] = true
		if m.SolarSystemID == 0 || m.Boost.IsIdentity() {
			if cur, ok := b.members[m.CharID]; ok && cur.GangID == view.GangID {
				delete(b.members, m.CharID)
			}
			continue
		}
		b.members[m.CharID] = MemberBoost{
			CharID: m.CharID, GangID: view.GangID,
			SolarSystemID: m.SolarSystemID, Boost: m.Boost,
		}
	}
	for id, cur := range b.members {
		if cur.GangID == view.GangID && !keep[id] {
			delete(b.members, id)
		}
	}
	b.gangs[view.GangID] = view
}

// Forget drops a gang (disbanded) and every entry it owns.
func (b *BoostBoard) Forget(gangID int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.gangs, gangID)
	for id, cur := range b.members {
		if cur.GangID == gangID {
			delete(b.members, id)
		}
	}
}

// View returns gangID's last resolve.
func (b *BoostBoard) View(gangID int64) (GangView, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	v, ok := b.gangs[gangID]
	return v, ok
}

// GangIDs returns every gang the board holds a view for, including one the registry has since
// dissolved (its members still need resetting).
func (b *BoostBoard) GangIDs() []int64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]int64, 0, len(b.gangs))
	for id := range b.gangs {
		out = append(out, id)
	}
	return out
}

// Snapshot returns every gang's last view, ordered by gangID.
func (b *BoostBoard) Snapshot() []GangView {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]GangView, 0, len(b.gangs))
	for _, v := range b.gangs {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GangID < out[j].GangID })
	return out
}
