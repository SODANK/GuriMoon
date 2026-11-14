package npc

// Destructible dungeon structures by invGroups.groupID: a dungeon object in one
// of these spawns as an immobile entity rather than a client-only rigid ball.
// A group list, not an HP predicate. See docs/npc-behaviour.md#structures.
var structureGroups = map[int64]bool{
	12:  true, // Cargo Container                  (categoryID 2)
	226: true, // Large Collidable Object          (categoryID 2)
	306: true, // Spawn Container
	307: true, // Construction Platform            (categoryID 2)
	319: true, // Large Collidable Structure
	340: true, // Secure Cargo Container           (categoryID 2)
	448: true, // Audit Log Secure Container       (categoryID 2)
	494: true, // Deadspace Overseer's Structure
	496: true, // Deadspace Overseer's Belongings
	649: true, // Freight Container                (categoryID 2)
	715: true, // Destructible Agents In Space
}

// Structures that open in place. These are the groups menusvc.py offers Open Cargo on.
var cargoGroups = map[int64]bool{
	12:  true,
	306: true,
	340: true,
	448: true,
	496: true,
	649: true,
}

// Armed structures. Destructible exactly like the ones above, but they shoot
// back. Behaviour needs no entry here: an armed hull with no maxVelocity is a
// turret and an unarmed one is inert (DeriveRole).
var sentryGroups = map[int64]bool{
	99:  true, // Sentry Gun
	180: true, // Protective Sentry Gun
	336: true, // Mobile Sentry Gun                (categoryID 2)
	383: true, // Destructible Sentry Gun
	495: true, // Deadspace Overseer's Sentry
}

// IsDestructibleStructure reports whether a groupID is an immobile dungeon
// structure that should spawn as a real entity rather than a rigid ball.
func IsDestructibleStructure(groupID int64) bool {
	return structureGroups[groupID] || sentryGroups[groupID]
}

// IsCargoStructure reports whether a dungeon structure group opens like a can.
func IsCargoStructure(groupID int64) bool { return cargoGroups[groupID] }

// IsSentryStructure reports whether a structure group is an armed one.
func IsSentryStructure(groupID int64) bool { return sentryGroups[groupID] }

// IsInertStructure reports whether a groupID is an unarmed structure: scenery
// with hitpoints that players can shoot but no NPC picks as a target of its
// own accord. Sentries are excluded, so an opposing NPC engages one.
func IsInertStructure(groupID int64) bool { return structureGroups[groupID] }
