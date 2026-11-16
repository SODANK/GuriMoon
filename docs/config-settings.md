# Configuration and settings

`config.json` is read once at boot. `config.example.json` lists every key at its default.

## What config.json controls

- Server listen address, log files (`server.logFile`, `crashLogFile`, `logAppend`), socket and shutdown timeouts.
- Database credentials, pool size and timeouts (`database.maxOpenConns` and the rest).
- Authentication mode and starting ISK balance.
- Gameplay rate multipliers: skill training, bounties, mining yield, loot, damage, industry speed, `missionReward`, `loyaltyPoints`, `standingGain`, `ratStandingLoss`.
- NPC market dynamics (`npcMarket`, the supply and demand repricing simulation).
- ESP panel settings (`esp.pprof`, `esp.sessionDays`, `esp.staffRoles`, `esp.minRole`, `esp.allowedHosts`).

## Two tiers of settings

`server_settings` is a database table edited live in the ESP panel. It holds the dials. The `settings` block in `config.json` supplies a default for any `server_settings` key.

Values can be numbers, strings, booleans (stored as 1 or 0) or number arrays (stored comma-separated, used for the high, low and null market tiers).

Precedence: table row, then the `config.json` `settings` block, then the built-in default.

## Live dials

The server keeps an in-memory copy of the table and reloads it every 30 s, so a change made in the ESP panel takes effect within 30 s.

Dials exposed this way:

| Area | Keys |
|---|---|
| CONCORD | `concord.responseDelaySec` (five values), `concord.lingerSec` |
| Sentries and gates | `sentry.respawnMinutes`, `gate.cloakSec`, `gate.jumpFlashMs` (2500; how long a gate jump lets the JumpOut flash play before the system switch) |
| Fixed dungeons | `dungeon.fixedRespawnMinutes` (10; how long a killed object of an ESP-placed dungeon stays dead) |
| NPC | `npc.lockTimeSec`, `npc.lockTimeCapSec`, `npc.leashMult`, `npc.fleeAlignSec`  |
| Loot | `loot.playerDropChance`, `loot.containerHours` (wreck cans and corpses share it), `loot.npcAnyChance`, `loot.npcLuckyChance`, `loot.npcJackpotChance` |
| Security and starbases | `security.killRightDays`, `starbase.aggressionMemoryMinutes`, `sov.flipGraceHours` (24, 0 flips at once), `starbase.freezeFuelWhileDown` (on; towers burn no fuel while the server is down, read at boot) |
| Repair and reprocessing | `repair.costFactor`, `reprocessing.taxBase`, `reprocessing.taxPerStanding`, `reprocessing.skillYieldBase` |
| Player-owned stations | `station.enforceManagement` (on; owner access rules, cost modifiers, docking fee, private office slots), `clone.contractCost` (5600) |
| Medical clones | `clone.podSkillLoss` (on; a pod death above the clone's cap costs SP) |
| Missions | `missions.qualityStep`, `missions.qualityFloor`, `missions.jumpWeights`, `missions.defaultExpiryDays` |
| Corporations | `corp.memberLimitFormula` (a base plus four per-skill-level terms) |
| Market | the security-tier levers as `market.<name>` (see `config.example.json`), plus `market.npcBuyFactor` and `market.impactSpill` |
| Audio | `audio.npcChatter` (on) |
| Weapon reload | `weapons.reloadSeconds.projectile` (10), `.hybrid` (5), `.laser` (0), `.launcher` (10), `.capBooster` (10) |

If a market override puts `secDynBuyFloor` at or under `secTradeSellCap` in any tier, both revert to their defaults.

Seed-time market levers reach existing orders only through an ESP reseed.

## New Eden calendar

The year on every Galnet page and the corporation portal is the real year minus an offset . Month and day stay real.

| Key | Default | Effect |
|---|---|---|
| `calendar.ycYearOffset` | 1898 | Years subtracted from the real year. 1898 makes 2026 YC 128; 1893 makes it YC 133. |
| `calendar.ycYear` | 0 (off) | Pins the current year to this YC year. A date in last real year reads one year earlier, so history stays in order. Wins over the offset. |

Set either in the `settings` block of `config.json` or live in the ESP panel; both are read through the settings cache, so a change shows within 30 s. The client draws its own dates (employment history, mail, wallet) from a Win32 FILETIME, which cannot hold a year below 1601, so those keep the real-world year.

## Not exposed

These values are computed by the client or by destiny.dll too, so the server must not change them alone: broker fee and sales tax, the insurance ladder, corporation and alliance creation costs, the aggression timer, the outlaw threshold of -5, the warp curve, the bubble radius and tick rates.

## Listeners and `esp.allowedHosts`

`server.listen` and `esp.listen` default to `127.0.0.1` (ports 26000 and 26080) when the key is
omitted, and `config.example.json` sets all three listeners, `esp.newsListen` included, to
loopback. An omitted `newsListen` turns Galnet off. The client and its hosts-file entries
already point at loopback, so a wider bind is only for a server other people reach;
`SETUP.md` "Playing with friends" says what that exposes. Both HTTP servers refuse a
`Host` header that is not loopback, the listener's own host, `myeve.eve-online.com`,
`img.eve.is` or one of `esp.allowedHosts` (421), and refuse a form or API request whose `Origin`
or `Referer` names another site (403). A LAN name or IP a reader types goes in `allowedHosts`.
