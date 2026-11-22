package npc

import (
	"context"
	"gurimoon/db"
	"gurimoon/seclaw"
	"log"
)

// EWAR ban rules (npcEwarBanRules): security bands where rats may not use a kind
// of electronic warfare. The default forbids warp disruption in 0.9 and 1.0.
// tickEwar consults the in-memory copy; ReloadEwarBans refreshes it at boot and
// after every ESP edit.

// ReloadEwarBans reads npcEwarBanRules into the manager. On error the previous
// set stays in force.
func (m *NPCManager) ReloadEwarBans(ctx context.Context) error {
	if m.sqldb == nil {
		return nil
	}
	rules, err := db.GetAllEwarBanRules(ctx, m.sqldb)
	if err != nil {
		return err
	}
	m.ewarBans.Store(&rules)
	log.Printf("[npc-ewar] %d EWAR ban rule(s) loaded", len(rules))
	return nil
}

// ewarBanned reports whether a rule forbids kind at this security. Security is
// compared as the client displays it (rounded to 0.1), so a 0.86 system counts
// as 0.9.
func (m *NPCManager) ewarBanned(kind string, security float64) bool {
	p := m.ewarBans.Load()
	if p == nil {
		return false
	}
	band := seclaw.SecBand(security)
	for _, r := range *p {
		if r.Kind == kind && band >= r.SecMin-1e-9 && band <= r.SecMax+1e-9 {
			return true
		}
	}
	return false
}
