package npc

import (
	"testing"

	"gurimoon/destiny"
)

// Leash cases for StandardBrain.Think (#26). AggrRange 10 km gives a 15 km leash
// (npc.leashMult default 1.5); the NPC sits 20 km from spawn, past it. Positions
// passed to hostile are metres from spawn along X.
func TestThinkLeash(t *testing.T) {
	// Off the origin: a zero SpawnPos means "no spawn point" to the brain.
	const base = 100000
	spawn := destiny.Vec3{X: base}
	self := destiny.Vec3{X: base + 20000}
	hostile := func(id uint32, x float64) EntityState {
		p := destiny.Vec3{X: base + x}
		return EntityState{ID: id, Pos: p, IsPlayer: true, IsHostile: true, Distance: p.Sub(self).Len()}
	}
	brain := &StandardBrain{Behavior: &Behavior{MovementMode: "orbit", AutoAggro: true, TargetPreference: "nearestPlayer", MaxTargets: 3}}

	disengaged := func(ca CombatAction) map[uint32]bool {
		out := map[uint32]bool{}
		for _, c := range ca.Combat {
			if c.Type == CmdDisengage {
				out[c.TargetID] = true
			}
		}
		return out
	}

	t.Run("target outside leash is dropped", func(t *testing.T) {
		view := WorldView{
			Self:        EntityState{ID: 1, Pos: self},
			Nearby:      []EntityState{hostile(100, 30000)},
			ThreatTable: []ThreatEntry{{EntityID: 100, TotalDamage: 50}},
			Targets:     []uint32{100},
			AggrRange:   10000,
			SpawnPos:    spawn,
		}
		ca := brain.Think(view)
		if ca.Movement.Type != ActionGoto || ca.Movement.Pos != spawn {
			t.Fatalf("movement = %+v, want Goto spawn", ca.Movement)
		}
		if d := disengaged(ca); !d[100] || len(d) != 1 {
			t.Fatalf("disengaged = %v, want {100}", d)
		}
	})

	t.Run("target inside leash keeps the fight", func(t *testing.T) {
		view := WorldView{
			Self:        EntityState{ID: 1, Pos: self},
			Nearby:      []EntityState{hostile(100, 6000)},
			ThreatTable: []ThreatEntry{{EntityID: 100, TotalDamage: 50}},
			Targets:     []uint32{100},
			AggrRange:   10000,
			SpawnPos:    spawn,
		}
		ca := brain.Think(view)
		if d := disengaged(ca); len(d) != 0 {
			t.Fatalf("disengaged = %v, want none", d)
		}
		if ca.Movement.Type == ActionGoto && ca.Movement.Pos == spawn {
			t.Fatalf("NPC went home with a target inside its leash")
		}
	})

	t.Run("only locks outside the leash are dropped", func(t *testing.T) {
		view := WorldView{
			Self: EntityState{ID: 1, Pos: self},
			Nearby: []EntityState{
				hostile(100, 30000), // picked by threat, outside
				hostile(200, 12000), // locked, inside the leash
				hostile(300, 31000), // locked, outside
			},
			ThreatTable: []ThreatEntry{{EntityID: 100, TotalDamage: 50}},
			Targets:     []uint32{100, 200, 300},
			Locking:     400, // left the bubble
			AggrRange:   10000,
			SpawnPos:    spawn,
		}
		ca := brain.Think(view)
		d := disengaged(ca)
		for _, id := range []uint32{100, 300, 400} {
			if !d[id] {
				t.Errorf("target %d not disengaged (got %v)", id, d)
			}
		}
		if d[200] {
			t.Errorf("target 200 inside the leash was disengaged")
		}
	})
}

func TestQueueCombatDisengageFirst(t *testing.T) {
	pending := []CombatCmd{
		{Type: CmdLockTarget, TargetID: 100},
		{Type: CmdFireWeapons, TargetID: 100},
		{Type: CmdLockTarget, TargetID: 200},
	}
	got := queueCombat(pending, []CombatCmd{{Type: CmdDisengage, TargetID: 100}})
	want := []CombatCmd{
		{Type: CmdDisengage, TargetID: 100},
		{Type: CmdLockTarget, TargetID: 200},
	}
	if len(got) != len(want) {
		t.Fatalf("queue = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("queue = %+v, want %+v", got, want)
		}
	}
}
