# NPC authoring (Create NPC)

The ESP **Create NPC** menu (`/npcs/new`, and the edit form on `/npc/{typeID}`) writes one NPC type: an `invTypes` row plus the `dgmTypeAttributes` rows the server reads when it spawns and runs the entity. Code: `esp/npcseed.go`, `esp/templates/partials.html` (`npcform`), `db/npcseeddb.go`, `db/npcseedcheck.go`.

## What each tab writes

The form shows people units (seconds, percent). `specFromForm` converts them to attribute units, and `NPCTypeSpec.attributes()` maps the spec onto attribute IDs. `npcSpecAttrIDs` is the managed set: a save rewrites exactly those rows, and a managed attribute at 0 has its row deleted so the runtime default applies. Attribute 252 is the one exception (see Rewards).

| Tab | Writes | Read by |
|---|---|---|
| Identity | `invTypes` name, group, race, description, published | client, `GetNPCStats` |
| Model | `invTypes.graphicID` | client (`eveGraphics.url3D`) |
| Movement | mass, radius, volume, capacity; 508 cruise speed, 37 max velocity, 70 agility | `GetNPCStats` |
| Defence | 263, 265, 9 HP; 479 shield recharge; resonances 271-274, 267-270, 113/111/109/110 | `GetEntityHPByType` |
| Weapons | 114/116/117/118 damage, 64 multiplier, 51 cycle, 54 optimal, 158 falloff, 620 signature resolution; turrets 160, 102, 10001; missiles 507, 212 | `GetNPCWeaponProfile` |
| Electronic warfare | the `npcEwarAttrSets` IDs of each ticked kind | `GetNPCEwarProfile`, `applyNPCEwar` |
| Sensors | 552 signature radius; one of 208-211 sensor strength | hit and lock math; ECM jam rolls |
| Behaviour | 416 orbit, 247 attack, 665 chase, 475/476 first-shot delay | `GetNPCWeaponProfile`, `GetNPCAIRanges` |
| Rewards | 481 bounty; 252 security status gain | `GetNPCBounty`, `GetNPCSecurityBonus` |

Units the form converts:

- **Resists** are entered as percent blocked and stored as resonance (`1 - pct/100`). A resonance of 0 reads as 1.0 at spawn (`attrOrDefault`), so 100% is refused; an untouchable NPC is the dungeon ball's Invulnerable flag.
- **EWAR chance** is a percent, stored as a 0-1 fraction. The effect field is stored in the attribute's own form: web speed reduction as a negative percent on attr 20, damp and tracking-disrupt reduction as a multiplier below 1, paint increase as a multiplier above 1, neutralizer GJ and scram points as is. `NPCEwarKind.EffectToForm` reads them the way `applyNPCEwar` does.
- **Sensor strength** is one value on one sensor type. With no strength at all, every ECM cycle jams the NPC.
- **Security status gain** is a percent of the killer's gap to +5.0, stored as a fraction. Blank leaves 252 absent, so the flat fallback applies (only when the bounty is above 0). An explicit 0 is written and means the kill is worth nothing.

## What is not on the type

- **Loot** comes from the group's loot table (`npc_loot_entries` with `npcGroupID`, cap in `npc_loot_caps`). The Identity tab shows what the picked group gives and links to `/loot/group/{id}`. Type-specific extras go on `/loot/npc/{typeID}`.
- **Faction, owner corporation and role** come from where the NPC spawns: a region belt listing (`npcRegionSpawns`), a dungeon (`dunDungeons.factionID`, `dunObjects.npcFactionID`), or `/spawn` (no faction). Commander and officer roles, and so their loot pools, come only from a belt listing.
- **Stance** (aggressive, defensive, passive) is per placement.

## Validation

`NPCTypeSpec.Validate` checks the spec on its own and `validateNPCSpecRefs` checks the IDs it points at. `ESPSaveNPCSpec` returns every problem as `NPCSpecErrors`, each tagged with the tab that holds the field, and the form opens the first one. The rules: name, entity group (category 11) and an existing graphic are required; structure HP, mass and radius above 0; nothing negative; resists under 100%; turrets need damage; missiles need a charge with a flight time (attr 281) and damage on the NPC or on the charge; a turret model override must exist; first-shot delay min not above max; a ticked EWAR kind needs a chance, a duration where it has one, a range (except scram) and an effect in bounds.

## Create, clone and stock types

- A new type gets the next ID in the custom band (100000 and up). Two creates racing for the same ID retry on the duplicate key.
- **Clone** (`/npcs/new?clone={typeID}`) loads the source into the form and, on create, copies every source attribute row outside the managed set in the same transaction, so attributes the menu has no field for survive.
- Saving a **stock type** (below 100000) needs the confirm box: the managed set is rewritten and anything the form leaves at 0 is deleted.

## Live effect

A save calls `InvalidateTypeStatics` and `InvalidateNPCSecurityBonus`, the `CacheInvalidator` hook (client bulk data) and the `NPCTypeChanged` hook, which drops the NPC manager's signature-radius and EWAR caches (`NPCManager.InvalidateTypeCaches`). The raw attribute editor on `/npc/{typeID}` does the same. NPCs already in space keep what they spawned with.

Region Spawns writes call the `RegionSpawnsChanged` hook, which reloads those regions' belt rosters (`SpawnerManager.ReloadRegionRosters`) and builds belt spawners in loaded systems of a region that had none.

## Runtime rules the form relies on

- An NPC with no weapon profile (unarmed or EWAR only) reads 416, 247 and 665 at spawn through `GetNPCAIRanges`: its orbit is the fly range and its aggro range the larger of attack and chase range.
- The dungeon clear census (`CountDungeonNPCObjects`, `RoomNPCObjectIDs`, `LoadDungeonRatCensus`) counts a type as a live NPC when attr 508 or 37 is above 0, the same test the dungeon spawn applies through `GetNPCStats`.
