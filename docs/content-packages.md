# Content packages

A content package (`.gmpkg`) carries dungeons, mission templates, open-site spawners and the
custom NPC types they use from one server to another. ESP **Admin > Content Packages**
(`/content/packages`, `esp/contentpkg.go`) and `cmd/contentpkg` both run package `contentpkg`.
The 3D editor has no export or import of its own: its **Export as package** and **Import
package** links open this page.

## What a package holds

Picking a mission adds its dungeon and every template its `nextMissionID` chain reaches. Picking
a spawner adds its dungeon. A dungeon brings its rooms, objects (`dunObjects`), object loot
(`dunObjectLoot`) and Dungeon Logic triggers (`dunTriggers`). Every custom type (typeID 100000
and up) used by an object, loot row, trigger or mission comes along with its `invTypes` row,
`dgmTypeAttributes`, `dgmTypeEffects` and the NPC loot rows scoped to that type
(`npc_loot_entries`, `npc_loot_caps`), and so does any custom type that loot drops.

Not carried: custom celestials (`customCelestials`, they are placed in a solar system), prefabs,
agents and LP offers, and runtime state (`dunOpenSites`, mission instances).

## File layout

The file is a zip:

| Entry | Content |
|---|---|
| `manifest.json` | format, package UUID, name, version, author, description, created, build, contents, requires |
| `npcs/<key>-<slug>.json` | one custom type: `type`, `attributes`, `effects`, `loot`, `lootCaps` |
| `dungeons/<key>-<slug>.json` | one dungeon: `dungeon`, `rooms`, `objects`, `loot`, `triggers` |
| `missions/<key>-<slug>.json` | one mission template row |
| `spawners/<key>-<slug>.json` | one spawner row |
| `README.md` | optional text the admin sees before installing |
| `install.sql` | the same install as a MySQL script (see below) |

Each row is a JSON object of column values. A row with its own primary key carries a `$key`
instead of the ID: the table's prefix letter and the row's ID on the exporting server (`d12`,
`r40`, `o206123`, `n100005`), with `_N` added when a re-export would repeat a key. A column that
points at another row holds `"@<key>"`. Base game data (item types, factions, stations, regions)
stays a plain ID; `requires` in the manifest lists every such ID with its name on the exporting
server.

| Prefix | Table | Prefix | Table |
|---|---|---|---|
| `d` | `dunDungeons` | `t` | `dunTriggers` |
| `r` | `dunRooms` | `m` | `missionTemplates` |
| `o` | `dunObjects` | `s` | `dunSpawners` |
| `l` | `dunObjectLoot` | `n` | `invTypes` (custom type) |
| `e` | `npc_loot_entries` | | |

## Validation

A package must match the format exactly; any deviation rejects the whole file before anything
is written. The column list per table, with each column's kind, range and nullability, is
`contentpkg/tables.go`; the rules are `contentpkg/validate.go` and the zip checks
`contentpkg/zip.go`.

- **Zip.** Only the entries in the table above, each item file listed once in the manifest and
  holding the key the manifest names. No other file, no traversal paths, no encryption, no
  compression other than store and deflate, size limits per entry and in total.
- **JSON.** Unknown fields, repeated keys, data after the value and deep nesting reject.
  Numbers are read exactly.
- **Rows.** A column the format does not list rejects, including primary keys and the columns
  packages never carry (`invTypes.basePrice`, `marketGroupID`, `portionSize`,
  `chanceOfDuplicating`, `dunSpawners.lastDespawnAt`). Every value has the kind its column
  expects: whole numbers in range, finite decimals in range, text of bounded length with no
  control characters (newlines only in long text), one of the allowed words for enums.
- **References.** A reference column takes a package key, never a raw ID, so a package cannot
  touch rows already on the server. A room points at its own dungeon, an object, loot row or
  trigger only at rows of the same dungeon, an attribute or NPC loot row only at its own type.
  A typeID column takes a base typeID below 100000 or a custom type key of the package. NPC loot
  is scoped to its type (`npcGroupID` 0), and its shares per role add up to at most 1.
- **Limits.** Spawners keep at most 500 sites and 20 per system; per package at most 100
  dungeons, 500 missions, 100 spawners and 200 custom types; per dungeon at most 50 rooms, 3000
  objects, 5000 loot rows and 1000 triggers.
- **Manifest.** Format 1 only, a lower-case UUID, a version of letters, digits and `._+-`. The
  contents list must name every item and nothing else; `requires` must list exactly the base IDs
  the rows use.
- **install.sql** must be byte for byte what this server generates from the JSON, so a file
  cannot carry a script that differs from what the panel shows.
- **Base data** (at install, against this server): every base type, faction, station, region,
  dogma attribute and effect, custom type graphic and transmission icon must exist, and a custom
  type's group must be an NPC group (category 11). A name that differs from the exporting
  server's is only a warning.

Export runs the same validation, so a server never writes a package another server would reject.
A new column on a packaged table is left out of exports (with a warning) until it is added to
`contentpkg/tables.go`.

The checks prove a file is well formed and safe to write, not that its content is fair. **Check
package** lists what it pays out (largest mission ISK and LP, highest custom NPC bounty, every
item it can hand out, live sites its spawners keep) for the admin to read before installing.

## Install, upgrade, uninstall

**Check package** runs the whole install in a transaction and rolls it back; its report is what
**Install** will do (auto-increment counters still advance). Install writes custom types first
(new typeIDs from `MAX(typeID) + 1` in the custom band), then dungeons (rooms get `roomOrder`
1..n in package order, so `dungeonRoomIdx` keeps pointing at the same room), then missions and
spawners, then the late links (`leashObjectID`, `nextMissionID`). By default new missions and
spawners install disabled.

The registry tables `contentPackages` and `contentPackageRows` record each installed package
and the row ID every key got. Installing a package whose UUID is installed upgrades it: rows
keep their IDs and their `enabled` state, new keys are added, keys the new version dropped are
deleted. A child row is reused only while it still belongs to the same parent. The preview warns
when an upgrade's name or author differs from the installed package.

Uninstall deletes exactly the registry's rows, limited to the package's own dungeons and custom
types. It is refused while a dungeon of the package has a mission instance or an uncleared open
site. A custom type still used by other content stays and is reported. Templates and spawners
outside the package that use one of its dungeons are reported; they lose their dungeon.

After an install or uninstall through ESP the panel drops the type caches for the custom types it
touched. After `cmd/contentpkg import` restart the server so it loads new custom types.

## install.sql

For a server without ESP or the command: it inserts every row with new IDs through
`LAST_INSERT_ID()`, links the late references, and records the registry rows so the panel can
uninstall it later. It is a fresh install only (no upgrade) and does not run the base-data
checks. Table names are lower-case, as in the `sql/` dumps.

## Command line

```sh
go run ./cmd/contentpkg catalog
go run ./cmd/contentpkg export -o outposts.gmpkg -missions 12,13 -name "Outposts" -version 1.0
go run ./cmd/contentpkg check outposts.gmpkg
go run ./cmd/contentpkg import -disabled outposts.gmpkg
go run ./cmd/contentpkg list
go run ./cmd/contentpkg uninstall <uuid>
go run ./cmd/contentpkg sql -o install.sql outposts.gmpkg
```

`-config` names the server config (default `config.json`) for the database settings.
