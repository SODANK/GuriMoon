package npc

import (
	"time"

	"gurimoon/db"
)

// NPC dials an operator may change: server_settings rows, or config.json's
// "settings" block. All read through the in-memory settings cache, so they
// cost a map lookup on the hot path.

// npcLockSeconds is the flat time an NPC takes to lock a target before
// sensor dampeners stretch it (npc.lockTimeSec, default 3).
func npcLockSeconds() float64 {
	return db.CachedSettingFloat("npc.lockTimeSec", 3, 0.1, 60)
}

// npcLockCapSeconds caps a dampened NPC lock (npc.lockTimeCapSec, default 30)
// so a heavily damped NPC still locks eventually.
func npcLockCapSeconds() float64 {
	return db.CachedSettingFloat("npc.lockTimeCapSec", 30, 1, 600)
}

// npcLeashMult is the leash range as a multiple of aggro range: how far an
// NPC chases before giving up (npc.leashMult, default 1.5).
func npcLeashMult() float64 {
	return db.CachedSettingFloat("npc.leashMult", 1.5, 1, 20)
}

// fleeAlignDelay is how long a fleeing unarmed hauler aligns before it warps
// off, the player's kill window (npc.fleeAlignSec, default 12).
func fleeAlignDelay() time.Duration {
	return db.CachedSettingDuration("npc.fleeAlignSec", time.Second, 12, 1, 300)
}
