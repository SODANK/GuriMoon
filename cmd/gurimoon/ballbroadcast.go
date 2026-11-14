// Grid broadcast helpers for structures (starbases, outpost platforms). Everything one grid change
// touches goes out under one stamp, so a remove and its replacement add never straddle a tick.
// The audience rule lives here: a control tower, cyno beacon and outpost platform are system-wide,
// everything anchored around a tower goes only to pilots on its grid
// (destiny.StarbaseBallVisibleFrom), and removals and slim changes go to whoever holds the ball.
package main

import (
	"gurimoon/destiny"
	"gurimoon/marshal"
)

// fullDamageState is an undamaged structure: full shield/armor/structure and a
// tau large enough that the client's own regen extrapolation never moves the
// bars. The ball builders ship it; liveDamage swaps in the structure entity's
// real HP (posentity.go) wherever one exists.
func fullDamageState() destiny.DamageState {
	return destiny.DamageState{Shield: 1.0, Tau: 1.0e9, Armor: 1.0, Structure: 1.0}
}

// eachPlayer runs send for every connected player in the system and returns how many were
// reached. send reports whether it sent this player anything, since a grid-local ball reaches only
// the pilots on its grid.
func eachPlayer(ss *destiny.SolarSystem, send func(p *destiny.Entity) bool) int {
	if ss == nil {
		return 0
	}
	n := 0
	for _, p := range ss.GetAllPlayersInSystem() {
		if p.Conn == nil {
			continue
		}
		if send(p) {
			n++
		}
	}
	return n
}

// liveDamage is dmg, or the current HP for a structure that is a combat entity, so a re-presented
// ball does not flash back to full health.
func liveDamage(ss *destiny.SolarSystem, id uint32, dmg destiny.DamageState) destiny.DamageState {
	if e := ss.StructureEntity(id); e != nil {
		d := e.DamageStateFrac()
		if d.Tau <= 0 {
			d.Tau = dmg.Tau
		}
		return d
	}
	return dmg
}

// broadcastBallAdd presents one new ball to everyone who should hold it: the system for a control
// tower, the grid for anything anchored around one. The ball is marked known to each recipient so
// later damage pushes, gated on KnowsBall, reach pilots already on grid.
func broadcastBallAdd(ss *destiny.SolarSystem, bd destiny.BallDesc, slim destiny.SlimItem, dmg destiny.DamageState) int {
	dmg = liveDamage(ss, bd.EntityID, dmg)
	facing := ss.StructureFacingActions([]destiny.BallDesc{bd}, []destiny.SlimItem{slim})
	return eachPlayer(ss, func(p *destiny.Entity) bool {
		if !destiny.StarbaseBallVisibleFrom(bd, slim, p.Pos()) {
			return false
		}
		if err := p.SendStamped(func(stamp uint32) error {
			if err := destiny.SendAddBallsFull(p.Conn, globalNodeID, p.AccountID, stamp,
				[]destiny.BallDesc{bd}, []destiny.SlimItem{slim},
				map[uint32]destiny.DamageState{bd.EntityID: dmg}); err != nil {
				return err
			}
			return sendFacings(p, stamp, facing)
		}); err == nil {
			p.ClaimUnknownBalls(bd.EntityID)
		}
		return true
	})
}

// sendFacings follows a presentation with the SetBallVelocity actions that
// turn moon harvesters toward their moon (destiny/structurefacing.go), on the
// presentation's own stamp.
func sendFacings(p *destiny.Entity, stamp uint32, facing []marshal.PyRep) error {
	if len(facing) == 0 {
		return nil
	}
	return destiny.SendActionList(p.Conn, globalNodeID, p.AccountID, stamp, facing...)
}

// broadcastBallRemove drops balls from the grid of everyone who holds them. A
// grid-local structure was never sent to pilots elsewhere in the system, so
// the removal goes only where KnowsBall says the ball is.
func broadcastBallRemove(ss *destiny.SolarSystem, ids ...uint32) int {
	if len(ids) == 0 {
		return 0
	}
	return eachPlayer(ss, func(p *destiny.Entity) bool {
		var held []uint32
		for _, id := range ids {
			if p.KnowsBall(id) {
				held = append(held, id)
			}
		}
		if len(held) == 0 {
			return false
		}
		if err := p.SendStamped(func(stamp uint32) error {
			return destiny.SendRemoveBalls(p.Conn, globalNodeID, p.AccountID, stamp, held)
		}); err == nil {
			p.ForgetBalls(held...)
		}
		return true
	})
}

// broadcastBallSwap removes one ball and adds its replacement, in one stamped
// send so the two never straddle a tick. The removal goes to whoever holds the
// old ball, the add to whoever should hold the new one (broadcastBallAdd's
// audience rule).
func broadcastBallSwap(ss *destiny.SolarSystem, oldID uint32, bd destiny.BallDesc, slim destiny.SlimItem, dmg destiny.DamageState) int {
	dmg = liveDamage(ss, bd.EntityID, dmg)
	facing := ss.StructureFacingActions([]destiny.BallDesc{bd}, []destiny.SlimItem{slim})
	return eachPlayer(ss, func(p *destiny.Entity) bool {
		remove := p.KnowsBall(oldID)
		add := destiny.StarbaseBallVisibleFrom(bd, slim, p.Pos())
		if !remove && !add {
			return false
		}
		if err := p.SendStamped(func(stamp uint32) error {
			if remove {
				if err := destiny.SendRemoveBalls(p.Conn, globalNodeID, p.AccountID, stamp, []uint32{oldID}); err != nil {
					return err
				}
			}
			if !add {
				return nil
			}
			if err := destiny.SendAddBallsFull(p.Conn, globalNodeID, p.AccountID, stamp,
				[]destiny.BallDesc{bd}, []destiny.SlimItem{slim},
				map[uint32]destiny.DamageState{bd.EntityID: dmg}); err != nil {
				return err
			}
			return sendFacings(p, stamp, facing)
		}); err == nil {
			if remove && (oldID != bd.EntityID || !add) {
				p.ForgetBalls(oldID)
			}
			if add {
				p.ClaimUnknownBalls(bd.EntityID)
			}
		}
		return true
	})
}

// broadcastSlimChange pushes an updated SlimItem to everyone holding the ball. It is FX-stamped:
// OnSlimItemChange is in the client's stamp-agnostic set (RealFlushState applies it without the
// FindState/Evolve sync), so it must not spend a stamp.
func broadcastSlimChange(ss *destiny.SolarSystem, slim destiny.SlimItem) int {
	id := uint32(slim.ItemID)
	return eachPlayer(ss, func(p *destiny.Entity) bool {
		if !p.KnowsBall(id) {
			return false
		}
		p.SendFXStamped(func(stamp uint32) error {
			return destiny.SendSlimItemChange(p.Conn, globalNodeID, p.AccountID, stamp, slim)
		})
		return true
	})
}
