# Dungeon authoring

How dungeon rooms are authored and what the runtime does with the authored columns. Code:
`db/dungeondb.go`, `db/dunautowave.go`, `cmd/gurimoon/dungeon_triggers.go`,
`cmd/gurimoon/npccargo.go`, `npc/structures.go`, `esp/dungeon_api.go`, `esp/editor/`. Already
written up elsewhere and not repeated here:

- Room beacon seeding, the `solid` default, `hpScale`, `naturalPhenomena` default:
  `docs/db-5-notes.md#dungeon-schema-and-authoring-defaults`.
- Room anchoring, ball names, HP scale and collision byte at spawn: `docs/main-s7-notes.md`.
- Trigger events, actions, auto waves, structure death: `docs/cmd-misc-notes.md#dungeon-triggers`.
- Open sites: `docs/open-sites.md`. Structure group list: `docs/npc-behaviour.md#structures`.

## Tool split

The 3D editor (`/editor/`) owns layout: positions, drag and drop, and wave-group assignment
through `dunObjects.spawnWave`. The Dungeon Logic page (`/dungeon/{vid}`) owns behaviour:

- Logic triggers (`dunTriggers`): `roomEnter`, `npcDamaged`, `npcKilled`, `roomCleared`,
  `gateActivated`, `itemPlaced` (item X, quantity N, into container ball Y), each running a
  transmission popup, local-chat flavour text, a message box, `spawnWave` to release a held wave
  group, `music` to switch the track, or `completeDungeon` to finish the site. The `npc*` event
  names are stored wire values.
- Ball Setup: custom ball name (`dunObjects.ballName`), wave group, stance, faction, aggro,
  leash-to-object patrol orbits (`dunObjects.leashObjectID`, `orbitDistance`), HP scale, the
  invulnerable and solid flags for structures.
- Loot Setup: per-ball drops in `dunObjectLoot`, guaranteed at dropChance 1 and rolled on top of
  the type and group `npc_loot_entries` tables. The section asks which ball to set up and shows
  only that ball's card; the pick is kept in `sessionStorage` per dungeon and room. Copy and Paste on a ball's card move its list and
  item-count range to any ball in any dungeon. The clipboard is the source objectID in
  `localStorage`, and `db.PasteObjectLoot` replaces the target's rows in one transaction, refusing
  a source with no drops so a stale clipboard cannot wipe a list.
- Gate music, beacon descriptions (`dunObjects.description` to `slimItem.dunDescription`), the
  per-room phenomena toggle and `dunDungeons.dedLevel`.

Everything spawned in a room is a ball. The trigger engine runs per connection;
wave spawns queue through `ds.dunWaveSpawn` to the connection goroutine (`spawnDungeonWave`).

## Faction of structures

Ball Setup's `(default)` faction means "inherit the dungeon" for NPCs and "none" for anything in
the structure groups. An unowned wall or container therefore does not carry the site's faction,
feed its standings or pull faction-wide aggro when shot. An authored `dunObjects.npcFactionID` is
the only way a structure gets a side.

## Deadspace phenomena

`dunRooms.naturalPhenomena` (default on) is the deadspace warp block. While such a room is loaded
and the pilot is inside the pocket, warping to anything within `deadspacePocketRadius` is refused
with the client's `WarpingWithinGlobalDisruptors` message, which carries its own voice line.
Warping clear of the site works. Acceleration gates are exempt: they call `startPlayerWarp`
directly and bypass the RPC handlers where the check lives. The toggle is per room on the
Dungeon Logic page.

## Destructible structures

A dungeon object whose `invGroups.groupID` is in `npc/structures.go` spawns as an immobile entity
instead of a client-only rigid ball, so it locks, takes fire, dies and drops its `dunObjectLoot`.
The group is the discriminator, not `maxVelocity`: these types carry `hp`, `shieldCapacity` and
`armorHP` but no `maxVelocity`, and group 226 is categoryID 2 and never reaches the category 6 or
11 NPC branch.

- Behaviour is derived: an armed structure is a turret, an unarmed one inert. The authored
  `dunObjects.stance` sets only how willing it is to fight.
- `dunObjects.invulnerable` pins a structure as permanent scenery: `Entity.ApplyDamage` returns
  zero applied, so it still locks and draws fire and cannot be destroyed.
- Structures never count toward a `killAll` objective. `RoomNPCObjectIDs` and
  `CountDungeonNPCObjects` filter on attributeID 37 > 0, and the death path's site-clear census
  and `killAll` tally skip them by group. A mission may still name one as its `targetTypeID`.
- Toggles are per ball on the Dungeon Logic page's Ball Setup.

## Spawn containers

The groups the client offers Open Cargo on (`npc.IsCargoStructure`: Cargo Container 12, Spawn
Container 306, Secure Cargo Container 340, Audit Log Secure Container 448, Deadspace Overseer's
Belongings 496, Freight Container 649) open in place (`cmd/gurimoon/npccargo.go`). They have no
hacking, and a password on a secure type is not asked for.

- The first in-range bind rolls `rollNPCLoot`, the roll `spawnNPCLoot` uses for a death drop,
  into item rows whose `locationID` is the NPC entity ID. That ID is in a band
  (`destiny.NPCIDBase`, 1.5e9 and up) no real item uses; the rows are swept at boot because NPC
  IDs restart.
- They then list, stack and take like a loot can (`isNPCCargo` in List, StackAll and the
  item-source gate).
- Destroying the container after opening moves what is left into the death can
  (`npcCargoToDeathCan`). No second roll is made.
- A pilot can place items in one only while an `itemPlaced` trigger listens to it
  (`docs/cmd-misc-notes.md#dungeon-triggers`). GetCapacity answers the type's capacity, or 27,500 m3
  for the types that carry 0.

## Collision caveats

The SubType byte rules are in `docs/main-s7-notes.md#solid-balls`. A change must keep these true:

- The collision sphere is the ball radius, not the hull. A rigid ball with NULL radius goes out
  at 500 m while the editor displays the type radius.
- The server simulation models no collision, so a bounce is client versus server drift.
- A warp line that clips a solid ball drops the ship out of warp short of its landing.
- `solid` is edited in the editor inspector, Ball Setup (structures) and the objects table (any
  ball). Only overrides travel in content packages.
- Mission and open-site structures reach the wire through `applyDunSlimFields` and
  `Manager.SetSolid`, including the rolebots `BuildRoom` path. The two rigid-ball builders apply
  the same default.

## 3D editor

React and Three.js SPA at `/editor/` behind panel authentication, embedded in the binary from
`esp/editor/dist` with `go:embed`. Source is `esp/editor/` (Vite, react-three-fiber), talking to
the `/api/*` dungeon endpoints in `esp/dungeon_api.go`. After changing editor source, rebuild the
bundle and commit `dist/`:

```sh
cd esp/editor && npm install && npm run build
```

Deep link: `/editor/?dungeon=VID`.

**Writes.** Every ball write is one batch (`POST /api/objects/batch` to `db.ApplyObjectBatch`, one
transaction). `src/commands.ts` is the editor's only write path.

**Undo is built from the server's answer**, never from the editor's own copy:

- A delete returns exact row snapshots (NULLs and loot kept) and undo restores them at the same
  `objectID`. InnoDB never reuses an auto-increment value, and keeping the ID keeps leashes and
  `dunTriggers.targetObjectID` pointing at the ball.
- An edit returns the before-rows and undo overwrites with them (`replace`).
- A create's redo restores its snapshots.

**Copies** (duplicate, Ctrl+C and Ctrl+V, Build-tab patterns from a selection, prefab stamps) use
the `insert` op: a snapshot under a fresh ID, with `leashClientKey` rewiring leashes inside the
copied set. A room move refuses, with a 409, a ball that a trigger in its room targets.

**Prefabs** live in `dunPrefabs` (`db/dunprefabdb.go`) as snapshot JSON relative to the centroid;
triggers are not captured. `db.DuplicateRoom` copies balls, loot and triggers with IDs remapped.

**Per-viewer state.** The View menu, clipboard, recents and favourites are `localStorage`.

**Models.** The editor draws real ship and structure models from the `.blue` pipeline, falling
back to `.glb` files under `esp.modelsDirs`. Nebula skyboxes are auto-detected in a `nebulas/`
directory next to the first models directory. See `docs/blue-format.md#model-pipeline-and-the-editor-frame`.

## Seed data

A fresh install loads the shipped dungeons, open-site spawners and mission templates with the
rest of `sql/`. Each file is a full dump, schema plus rows, with IDs kept so mission templates,
spawners, loot and triggers still point at the right dungeon, room and ball:

| File | Table |
|---|---|
| `sql/dundungeons.sql` | `dunDungeons` |
| `sql/dunrooms.sql` | `dunRooms` |
| `sql/dunobjects.sql` | `dunObjects` |
| `sql/dunobjectloot.sql` | `dunObjectLoot` |
| `sql/duntriggers.sql` | `dunTriggers` (Dungeon Logic) |
| `sql/dunspawners.sql` | `dunSpawners` (open-site rules) |
| `sql/missiontemplates.sql` | `missionTemplates` |
| `sql/agtdivisionmissionweights.sql` | `agtDivisionMissionWeights` |

Runtime state is not shipped: `dunOpenSites` (the spawner makes new sites at boot),
`missionDungeonInstances`, `missionDungeonSiteState`, `missionDungeonWaveState`,
`characterMissions`. `dunSpawners.lastDespawnAt` ships as 0.

**Bake every change.** After editing dungeons, logic, spawners or missions, redump the changed
tables one row per insert, with column names, and drop the dump header, so the diff reads per row:

```sh
mysqldump -uroot -p --default-character-set=utf8mb4 --skip-extended-insert --complete-insert \
  --skip-dump-date --no-tablespaces --column-statistics=0 gurimoon_rmr dunobjects > sql/dunobjects.sql
```

`--column-statistics=0` is for the MySQL 8 client only; drop it with `mariadb-dump`, and drop
the leading sandbox line a MariaDB dump writes, because MySQL 8 refuses it. Set
`lastDespawnAt` back to 0 in `dunspawners.sql`. Keep each file's line endings.

To hand dungeons, missions and spawners to another server, export a content package from ESP
Admin > Content Packages; the editor's export and import links open that page. See
`docs/content-packages.md`.
