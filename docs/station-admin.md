# ESP panel: access, stations and corporation addresses

Code: `esp/auth.go`, `esp/igbguard.go`, `esp/corpslug.go`, `db/corppageurl.go`, station pages in
`esp/` backed by `db/espstationdb.go` and `db/espstationservicesdb.go`. The panel runs on `:26080`
with Basic Auth (or the login cookie, `docs/esp-3-notes.md#panel-login-cookie`).

## Access

`Server.roleAllowed` requires a bit of `esp.staffRoles` (default GML|GMH|ADMIN = 56) and a role at
or above the numeric `esp.minRole`. A numeric test cannot mean "is a GM"
(`docs/esp-3-notes.md#role-test`, `docs/slash-commands-notes.md#role-gate`).

- `authentication.role` defaults to 3 when omitted, which holds no staff bit. Boot logs a warning
  when `autoaccount` would hand out a staff bit.
- The in-game browser's sites are served by the same package on the news server
  (`docs/galnet-igb.md`). Dungeon editor usage is in `DUNGEON_EDITOR_GUIDE.txt`.

## In-game browser guard

`esp/igbguard.go` wraps the whole mux. The admin pages (scripts, CSS masks, forms in table cells)
break the client's renderer, so a request with the `EVE-minibrowser` agent never reaches them:

- `/corporation/<id>` answers a zero `Refresh` to the corporation's Galnet page (Corporate Portal
  profile, CONCORD dossier for an NPC corporation).
- Any other panel path answers a short "open this on the desktop" page.
- Image routes are untouched.

## Corporation addresses

A corporation's url is its name on Galnet, for example
`http://myeve.eve-online.com/Saang-Foundation`. `db/corppageurl.go` `CorpSlug` keeps alphanumerics,
drops apostrophes and turns other runs into one hyphen.

- The news server's catch-all serves it in place (`esp/corpslug.go`, called from `redispatch`), so
  the address bar keeps the clean form. Lookup is case-folded and slugifies the request too, so
  `/saang foundation` resolves. Registered paths always win.
- The stamp falls back to the ID form (`/corp/profile?id=`) when the slug is empty, belongs to a
  site (`GalnetPathTaken`, set by `NewNewsServer`; nil means no news server and no stamp) or is
  already another corporation's.
- Stamped at creation. An ESP rename carries the url along only while it is still an address the
  server handed out (`isStampedCorpURL`).
- `MigrateCorpPageURLs` (news server `Start`) moves every remaining `/corporation/<id>` url once,
  guarded on the old value.

## Dashboard

`/` is the server overview: population, ISK created and destroyed, the order book and trades,
industry, open dungeon sites, kills, missions, mining, sovereignty and structures, skills and
process vitals. `ESPDashboardOverviewLoad` (`db/espdashboarddb.go`) reads every panel at once; a
failing panel is a warning on the page, never an error page. The result is memoised for 30 s
(`dashOverview`, `esp/dashboard.go`) and `?refresh=1` bypasses it. The vitals strip (sessions,
loaded systems, NPCs in space through `esp.NPCCount`, runtime, connection pool) is read on every
request.

Time windows are 24 h and 7 d only. Wallet retention keeps the newest rows per character
(`docs/db-8-notes.md#wallet-retention`), so a longer window would undercount busy wallets. The
economy panel classifies `market_journal` by reference type (`ESPDashEconomyLoad`): a refType-2
row carries the buyer in `ownerID1` and the seller in `ownerID2`, and an owner below
`MinPlayerEntityID` is the NPC side. A transfer between two wallets is summed on its positive side
only. Rows no line names are reported as unclassified, so a new reference type shows up instead of
vanishing. The query needs `idx_mj_date` (`db/perfindex.go`). A rolebot counts as a player
everywhere on the page.

`/?view=players` is the connection table; `/sessions` redirects to `/`. It resolves the session
registry's raw IDs (station, system, corporation) to names, splits the count into docked, in space
and selecting, and sorts by connect time. `SessionRegistry.Snapshot` walks a map, so the page sorts
or the table reshuffles on each refresh.

## Station browser

`/stations` opens on every station, alphabetical.

- Server-side filters, deep-linkable: text search over name, system, constellation and region
  (`?q=`), plus region and constellation dropdowns (`?region=`, `?constellation=`).
- Client-side over the rendered rows: a quick-filter box, an A to Z bar and click-to-sort headers.
  A round trip per keystroke over the full list is unusable.
- Sorting keeps one record per row. Numeric columns read a `data-v` attribute, because security is
  rounded for display and counts sit beside a "3 live" badge that breaks parsing the cell text.
- Each row carries its region and constellation names, or the client-side search cannot match them.
- `ESPListStations` computes the per-station counts (berthed pilots, agents, live orders) from
  derived tables. A correlated subquery per row would rescan `character_`, `agtAgents` and
  `market_orders` thousands of times.

Counting rules for every ESP station page:

- NPC agents are rows of `character_` with a `stationID`. Pilots are `accountID > 0`; an unfiltered
  docked count returns one entry per agent (about 10k) and tracks the agent population.
- Active market orders are `orderState = 1`, which every insert path writes and `marketdb.go`
  selects on. `orderState = 0` matches nothing.
- `DockedCount` counts players berthed at the station whether or not they are online. The "live"
  badge comes from the session registry, the only source of who is connected.

## Station services

`/station/<id>` ticks services on and off. A station's services belong to its operation, and an
edit moves the station onto its own cloned operation (`CustomOperationBase`, 10000). The mechanism
and the client's two readers are in `docs/db-10-notes.md#station-services`. Additional rules:

- `map.GetStationExtraInfo` returns `(stations, opServices, services)` and is cached for the life of
  the process. An edit calls `mapsvc.InvalidateStationExtraInfo` through
  `esp.StationServicesInvalidator`.
- `eve.stationItem.serviceMask` (the sum of the station's `serviceID`s, read from
  `GetStationItemBits` once at dock) drives the lobby buttons, the right-click refining test and the
  station-management window. `GetStation`'s `serviceMask` column drives Show Info's Services list.
  `manufacturing.CheckStationService` and the star map overlay use the joined 3-tuple instead.
- All 51 shipped operations carry at least one service, and the editor refuses an empty service set.
- Both editor paths delete an orphaned clone when a station moves off it.

## Station models

`/station/<id>` swaps a station's hull. The swap writes `staStations.stationTypeID` and
`mapDenormalize.typeID` in one transaction (`docs/db-9-notes.md#station-type-swap`). Additional
points:

- 59 station types exist. `db.ModelLabel` derives a family and hull name from `url3D`, since the
  shipped names are generic ("Station ( Gallente 3 )"):
  `res:/Model/Station/Gallente/gs3/gs3.blue` reads as "Station / Gallente · gs3". A trailing
  directory that repeats the file name is dropped.
- Types in group 10 (stargate models) and ship models are hidden behind the picker's "Station group
  only" box.
- The hangar graphic (`staStationTypes.hangarGraphicID`) and the undock offset follow the swap
  through the type row. The hangar graphic is NULL on the 7 stargate-model rows; `ESPSetStationType`
  refuses them and the picker greys the tile. `GetStationItemBits` scans the column nullably, so a
  hand-edited row cannot make a station undockable.
- The picker thumbnails from the client's Gids snapshot cache and falls back to an offscreen
  `.glb` render (`renderModelThumb`, the shared-context renderer) for graphics that cache never
  drew. A type with no converted model is marked, not hidden.
- A client already loaded into the system keeps the old model until the system reloads.
