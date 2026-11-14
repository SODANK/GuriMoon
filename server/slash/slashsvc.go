// Package slash implements the "slash" service: GM commands typed in chat with a "/" prefix.
// The client sends one raw command line to SlashCmd. See docs/slash-commands.txt.
package slash

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"gurimoon/corp"
	evedb "gurimoon/db"
	"gurimoon/esp"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
	"gurimoon/server/lsc"
	"gurimoon/server/station"
)

// Teleporter exposes the in-space operations of a live game session; package main implements it.
// /tr, /goto and /translocate report "no in-flight session" when it is nil (ESP console).
type Teleporter interface {
	// Translocate teleports the caller to destID: a solar system, station or mapDenormalize celestial.
	Translocate(ctx context.Context, destID int64) (string, error)
	// Goto moves the caller's ship to (x, y, z) in the current solar system.
	Goto(ctx context.Context, x, y, z float64) (string, error)
	// SelfDestruct destroys the caller's ship or pod, triggering the death loop.
	SelfDestruct() (string, error)
}

// SceneReporter is optionally implemented by the Teleporter to print the server's distance to
// every ball on the caller's grid, to read against the overview's distance column.
// See docs/server-notes.md#slash-diagnostics.
type SceneReporter interface {
	Scene(filter string) (string, error)
}

// StampReporter is optionally implemented by the Teleporter to report the caller's destiny stamp
// stream: mints per second against the one-per-second budget, and the call sites spending them.
// See docs/server-notes.md#slash-diagnostics.
type StampReporter interface {
	Stamps() (string, error)
}

// GhostReporter is optionally implemented by the Teleporter to explain, per ball, why the caller's
// overview still draws it, or that the server has forgotten it.
// See docs/server-notes.md#slash-diagnostics.
type GhostReporter interface {
	Ghosts() (string, error)
}

// BallparkRefresher is optionally implemented by the Teleporter to clear stale overview entries.
// Plain prunes what the server knows is stale and reloads the overview; hard rebuilds the whole
// ballpark first, the only way to reach a ball the server has forgotten.
type BallparkRefresher interface {
	RefreshOverview(hard bool) (string, error)
}

// DupeReporter is optionally implemented by the Teleporter to report balls this client was sent
// an AddBalls for more than once. A non-empty report is a server bug.
// See docs/server-notes.md#slash-diagnostics.
type DupeReporter interface {
	Dupes() (string, error)
}

// GMModeSetter is optionally implemented by the Teleporter for /gm on|off: nothing may lock,
// approach or shoot the caller, and NPCs ignore them. It is a targeting rule, not invulnerability,
// and the reply must say so.
type GMModeSetter interface {
	SetGMMode(on bool) (string, error)
}

// GMCloakSetter is optionally implemented by the Teleporter for /gm cloak on [solid] | off: a
// standing cloak that movement, warp and modules never break. Unless solid, the caller's own ball
// shrinks so it collides with nothing. It turns /gm on as well; off leaves /gm alone.
type GMCloakSetter interface {
	SetGMCloak(on, ghost bool) (string, error)
}

// StarbaseNotifier is optionally implemented by the Teleporter to push live destiny updates when
// /pos* commands change the world. Each method returns a reply suffix, "" when nothing was
// pushed (system not loaded; the on-entry loader picks the change up).
type StarbaseNotifier interface {
	StarbasePlanted(entityID int64) string
	StarbaseStateChanged(entityID int64) string
	StarbaseRemoved(entityID, solarSystemID int64) string
}

// MoonScanner is optionally implemented by the Teleporter for /moonscan: read or generate the
// moon's resource pool and push OnMoonScanComplete to the caller. moonID 0 means the nearest moon.
type MoonScanner interface {
	MoonScan(moonID int64) (string, error)
}

// connState bundles the per-connection state slash commands read or update. Pointers let a
// command change live state, such as stationID after /moveto.
type connState struct {
	c          *evenet.Conn
	addr       string
	accountID  int64
	charID     int64
	stationID  *int64
	sqldb      *sql.DB
	tp         Teleporter               // nil when no live game session is attached
	sessReg    *esp.SessionRegistry     // nil when no session registry available
	stationReg *station.StationRegistry // nil when no guest registry available (ESP console)
	lscReg     *lsc.Registry            // nil when no chat registry available (ESP console)
}

// ModuleCommandState is what a module slash command may read. connState is unexported, so this
// is the stable subset.
type ModuleCommandState struct {
	DB        *sql.DB
	CharID    int64
	AccountID int64
}

// ModuleCommand is set by a compiled-in module's Init to claim slash commands the core does not
// recognise. dispatch calls it last; handled=false falls through to the Unknown-command reply.
var ModuleCommand func(ctx context.Context, st ModuleCommandState, cmd string, args []string) (result string, handled bool)

func Handle(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, charID int64, stationID *int64, sqldb *sql.DB, tp Teleporter, sessReg *esp.SessionRegistry, stationReg *station.StationRegistry, lscReg *lsc.Registry) error {
	switch cs.Method {
	case "SlashCmd":
		st := &connState{c: c, addr: addr, accountID: accountID, charID: charID, stationID: stationID, sqldb: sqldb, tp: tp, sessReg: sessReg, stationReg: stationReg, lscReg: lscReg}
		return handleSlashCmd(st, cs, pkt)
	default:
		log.Printf("[%s] slash.%s: unhandled — returning None", addr, cs.Method)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
}

func handleSlashCmd(st *connState, cs *server.CallStream, pkt *evenet.PyPacket) error {
	if len(cs.ArgTuple.Items) == 0 {
		return server.SendCallRsp(st.c, pkt, st.accountID, marshal.PyString{Value: "No command provided."})
	}
	rawStr, ok := cs.ArgTuple.Items[0].(marshal.PyString)
	if !ok {
		return server.SendCallRsp(st.c, pkt, st.accountID, marshal.PyString{Value: "Invalid command argument."})
	}

	// Strip leading / or # (the client always prepends one).
	cmdLine := strings.TrimSpace(rawStr.Value)
	if len(cmdLine) > 0 && (cmdLine[0] == '/' || cmdLine[0] == '#') {
		cmdLine = cmdLine[1:]
	}

	parts := strings.Fields(cmdLine)
	if len(parts) == 0 {
		return server.SendCallRsp(st.c, pkt, st.accountID, marshal.PyString{Value: "Empty command."})
	}
	cmd := strings.ToLower(parts[0])
	args := parts[1:]

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := dispatch(ctx, st, cmd, args)
	if err != nil {
		result = fmt.Sprintf("Error: %v", err)
	}

	log.Printf("[%s] slash: charID=%d cmd=%q → %q", st.addr, st.charID, cmd, result)
	// The reply string is shown in the character's LOG window.
	return server.SendCallRsp(st.c, pkt, st.accountID, marshal.PyString{Value: result})
}

// dispatch executes a GM command and returns the result text.
func dispatch(ctx context.Context, st *connState, cmd string, args []string) (string, error) {
	switch cmd {
	case "search":
		return cmdSearch(ctx, st.sqldb, args)
	case "giveisk":
		return cmdGiveISK(ctx, st, args)
	case "goto":
		return cmdGoto(ctx, st, args)
	case "translocate":
		return cmdTranslocate(ctx, st, args)
	case "tr":
		return cmdTR(ctx, st, args)
	case "giveallskill", "giveallskills", "allskills":
		return cmdGiveAllSkills(ctx, st.sqldb, st.charID, st.c, st.accountID)
	case "giveskill":
		return cmdGiveSkill(ctx, st.sqldb, st.charID, args, st.c, st.accountID)
	case "moveto":
		return cmdMoveTo(ctx, st, args)
	case "createitem":
		return cmdCreateItem(ctx, st, args)
	case "create":
		return cmdCreate(ctx, st, args)
	case "seedmarkets":
		return cmdSeedMarkets(ctx, st)
	case "seednpcmarkets":
		return cmdSeedNPCMarkets(ctx, st)
	case "setcorp":
		return cmdSetCorp(ctx, st, args)
	case "fit":
		return cmdFit(ctx, st, args)
	case "claim":
		return cmdClaim(ctx, st, args)
	case "unclaim":
		return cmdUnclaim(ctx, st, args)
	case "sov":
		return cmdSov(ctx, st, args)
	case "posplant":
		return cmdPosPlant(ctx, st, args)
	case "posonline":
		return cmdPosOnline(ctx, st, args)
	case "posremove":
		return cmdPosRemove(ctx, st, args)
	case "poslist":
		return cmdPosList(ctx, st, args)
	case "possave":
		return cmdPosSave(ctx, st, args)
	case "posspawn":
		return cmdPosSpawn(ctx, st, args)
	case "postemplates":
		return cmdPosTemplates(ctx, st, args)
	case "fuel":
		return cmdFuel(ctx, st, args)
	case "moonscan":
		return cmdMoonScan(ctx, st, args)
	case "gm":
		return cmdGM(st, args)
	case "scene":
		return cmdScene(st, args)
	case "stamps":
		return cmdStamps(st)
	case "ghosts":
		return cmdGhosts(st)
	case "refresh":
		return cmdRefresh(st, args)
	case "dupes":
		return cmdDupes(st)
	case "mission":
		return cmdMission(ctx, st, args)
	case "redoportrait":
		return cmdRedoPortrait(ctx, st, args)
	case "spawn":
		return "spawn: not yet implemented (requires NPC/space sim)", nil
	case "summon":
		return "summon: not yet implemented (requires item spawning)", nil
	case "roid":
		return "roid: not yet implemented (requires asteroid spawning)", nil
	case "growbelt":
		return "growbelt: not yet implemented (requires belt growth sim)", nil
	case "pop":
		return "pop: not yet implemented (requires live notification pipeline)", nil
	case "selfdestruct":
		if st.tp == nil {
			return "selfdestruct: not in space", nil
		}
		return st.tp.SelfDestruct()
	case "help", "?":
		return cmdHelp(), nil
	default:
		// Module commands get the last word before "Unknown command".
		if ModuleCommand != nil {
			if out, handled := ModuleCommand(ctx, ModuleCommandState{DB: st.sqldb, CharID: st.charID, AccountID: st.accountID}, cmd, args); handled {
				return out, nil
			}
		}
		return fmt.Sprintf("Unknown command '%s'. Type /help for a list.", cmd), nil
	}
}

// cmdRedoPortrait implements /redoportrait <character name or ID>. It flags the character so the
// next character-select hides it and the player redoes creation, which applies only the new
// appearance (db/recustomdb.go). "/redoportrait clear <name|ID>" unflags.
func cmdRedoPortrait(ctx context.Context, st *connState, args []string) (string, error) {
	if len(args) == 0 {
		return "Usage: /redoportrait <character name or ID>   (/redoportrait clear <name|ID> to unflag)", nil
	}
	clear := false
	if strings.EqualFold(args[0], "clear") {
		clear = true
		args = args[1:]
		if len(args) == 0 {
			return "Usage: /redoportrait clear <character name or ID>", nil
		}
	}

	var charID int64
	var charName string
	if id, err := strconv.ParseInt(args[0], 10, 64); err == nil && len(args) == 1 {
		charName = evedb.GetCharacterName(ctx, st.sqldb, id)
		if charName == "" {
			return fmt.Sprintf("No character with ID %d.", id), nil
		}
		charID = id
	} else {
		name := strings.Join(args, " ")
		id, canonical, err := evedb.FindPlayerCharByName(ctx, st.sqldb, name)
		if err != nil {
			return fmt.Sprintf("No player character named %q.", name), nil
		}
		charID, charName = id, canonical
	}

	if clear {
		removed, err := evedb.ClearRecustomization(ctx, st.sqldb, charID)
		if err != nil {
			return "", err
		}
		if !removed {
			return fmt.Sprintf("%s (%d) was not flagged for portrait redo.", charName, charID), nil
		}
		return fmt.Sprintf("Portrait redo flag cleared for %s (%d).", charName, charID), nil
	}

	if err := evedb.FlagRecustomization(ctx, st.sqldb, charID); err != nil {
		return "", err
	}
	return fmt.Sprintf(
		"%s (%d) flagged for portrait redo. At next login the character is hidden from selection; "+
			"the player re-enters character creation, must pick the same bloodline and gender, "+
			"and only the new portrait is saved — name, attributes and education picks are discarded.",
		charName, charID), nil
}

// cmdSearch implements /search <text>: invTypes names matching the text or regex.
func cmdSearch(ctx context.Context, sqldb *sql.DB, args []string) (string, error) {
	if len(args) == 0 {
		return "Usage: /search <text or regex>", nil
	}
	query := strings.Join(args, " ")

	matches, err := evedb.ItemSearch(ctx, sqldb, query)
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "Nothing found.", nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "%d match(es) found:\n", len(matches))
	limit := len(matches)
	if limit > 20 {
		limit = 20
	}
	for _, m := range matches[:limit] {
		fmt.Fprintf(&sb, "  %d: %s\n", m.TypeID, m.TypeName)
	}
	if len(matches) > 20 {
		fmt.Fprintf(&sb, "  … and %d more (refine your query)", len(matches)-20)
	}
	return sb.String(), nil
}

// cmdGiveISK implements /giveisk <entityID> <amount>; entityID 0 means the caller.
func cmdGiveISK(ctx context.Context, st *connState, args []string) (string, error) {
	if len(args) < 2 {
		return "Usage: /giveisk <characterID> <amount>   (use 0 for yourself)", nil
	}
	entityID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return "argument 1 should be an entity ID (0=self)", nil
	}
	amount, err := strconv.ParseFloat(args[1], 64)
	if err != nil {
		return "argument 2 should be an amount of ISK", nil
	}

	target := st.charID
	if entityID != 0 {
		target = entityID
	}

	if err := evedb.GiveISK(ctx, st.sqldb, target, amount); err != nil {
		return "", err
	}
	newBal, err := evedb.GetCharBalance(ctx, st.sqldb, target)
	if err != nil {
		return fmt.Sprintf("ISK granted (%.2f). Could not read new balance: %v", amount, err), nil
	}

	// Journal entry for GM donation.
	_ = evedb.InsertJournalEntry(ctx, st.sqldb, target, 10, target, target, amount, newBal, "")

	// Notify the target's client of the wallet change so it refreshes.
	if st.sessReg != nil {
		notifyArgs := marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyString{Value: "cash"},
			marshal.PyInt{Value: uint64(target)},
			marshal.PyReal{Value: newBal},
		}}
		server.NotifyCharacter(st.sessReg, target, "OnAccountChange", "clientID", notifyArgs)
	}

	return fmt.Sprintf("Gave %.2f ISK to characterID %d. New balance: %.2f ISK", amount, target, newBal), nil
}

// cmdGoto implements /goto <x> <y> <z>: move the caller's ship within the current system.
func cmdGoto(ctx context.Context, st *connState, args []string) (string, error) {
	if len(args) < 3 {
		return "Usage: /goto <x> <y> <z>", nil
	}
	coords := make([]float64, 3)
	for i, a := range args[:3] {
		v, err := strconv.ParseFloat(a, 64)
		if err != nil {
			return fmt.Sprintf("argument %d must be a number", i+1), nil
		}
		coords[i] = v
	}
	if st.tp == nil {
		return "/goto only works from an in-game session.", nil
	}
	return st.tp.Goto(ctx, coords[0], coords[1], coords[2])
}

// cmdScene implements /scene [filter]. See SceneReporter.
func cmdScene(st *connState, args []string) (string, error) {
	sr, ok := st.tp.(SceneReporter)
	if st.tp == nil || !ok {
		return "/scene only works from an in-game session.", nil
	}
	return sr.Scene(strings.ToLower(strings.Join(args, " ")))
}

// cmdGM implements /gm on|off and /gm cloak. See GMModeSetter.
//
// There is no bare /gm toggle: a toggle read as an assertion turns the mode off.
func cmdGM(st *connState, args []string) (string, error) {
	gs, ok := st.tp.(GMModeSetter)
	if st.tp == nil || !ok {
		return "/gm only works from an in-game session.", nil
	}
	const usage = "Usage: /gm on | /gm off | /gm cloak on [solid] | /gm cloak off"
	if len(args) < 1 {
		return usage, nil
	}
	switch strings.ToLower(args[0]) {
	case "on", "1", "true", "enable":
		return gs.SetGMMode(true)
	case "off", "0", "false", "disable":
		return gs.SetGMMode(false)
	case "cloak":
		cs, ok := st.tp.(GMCloakSetter)
		if !ok {
			return "/gm cloak only works from an in-game session.", nil
		}
		if len(args) < 2 {
			return usage, nil
		}
		switch strings.ToLower(args[1]) {
		case "on", "1", "true", "enable":
			// "solid" opts out of the 1 m ghost ball, which also rescales the model.
			solid := len(args) > 2 && strings.ToLower(args[2]) == "solid"
			return cs.SetGMCloak(true, !solid)
		case "off", "0", "false", "disable":
			return cs.SetGMCloak(false, false)
		}
		return usage, nil
	default:
		return usage, nil
	}
}

// cmdStamps reports the caller's stamp stream. See StampReporter.
func cmdStamps(st *connState) (string, error) {
	sr, ok := st.tp.(StampReporter)
	if st.tp == nil || !ok {
		return "/stamps only works from an in-game session.", nil
	}
	return sr.Stamps()
}

// cmdGhosts explains every ball in the caller's ballpark. See GhostReporter.
func cmdGhosts(st *connState) (string, error) {
	gr, ok := st.tp.(GhostReporter)
	if st.tp == nil || !ok {
		return "/ghosts only works from an in-game session.", nil
	}
	return gr.Ghosts()
}

// cmdRefresh implements /refresh [hard]. See BallparkRefresher.
func cmdRefresh(st *connState, args []string) (string, error) {
	rr, ok := st.tp.(BallparkRefresher)
	if st.tp == nil || !ok {
		return "/refresh only works from an in-game session.", nil
	}
	hard := len(args) > 0 && strings.EqualFold(args[0], "hard")
	if len(args) > 0 && !hard {
		return "Usage: /refresh [hard]", nil
	}
	return rr.RefreshOverview(hard)
}

// cmdDupes lists balls this client was sent twice. See DupeReporter.
func cmdDupes(st *connState) (string, error) {
	dr, ok := st.tp.(DupeReporter)
	if st.tp == nil || !ok {
		return "/dupes only works from an in-game session.", nil
	}
	return dr.Dupes()
}

// cmdTranslocate implements /translocate <entityID>, an alias for /tr me <entityID>.
func cmdTranslocate(ctx context.Context, st *connState, args []string) (string, error) {
	if len(args) < 1 {
		return "Usage: /translocate <entityID>", nil
	}
	destID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return "argument 1 should be an entity ID", nil
	}
	if st.tp == nil {
		return "/translocate only works from an in-game session.", nil
	}
	return st.tp.Translocate(ctx, destID)
}

// cmdTR implements /tr me [to] <entityID>. Only "me" is supported for <who>, and a stray "to"
// is tolerated.
func cmdTR(ctx context.Context, st *connState, args []string) (string, error) {
	if len(args) < 2 {
		return "Usage: /tr me <entityID>   (entityID may be a solar system, station, or celestial)", nil
	}
	if !strings.EqualFold(args[0], "me") {
		return fmt.Sprintf("/tr: only 'me' is supported for <who> (got %q)", args[0]), nil
	}
	// Skip a literal "to" if the user typed "/tr me to <id>".
	idArg := args[1]
	if strings.EqualFold(idArg, "to") && len(args) >= 3 {
		idArg = args[2]
	}
	destID, err := strconv.ParseInt(idArg, 10, 64)
	if err != nil {
		return fmt.Sprintf("/tr: %q is not a valid entity ID", idArg), nil
	}
	if st.tp == nil {
		return "/tr only works from an in-game session.", nil
	}
	return st.tp.Translocate(ctx, destID)
}

// cmdGiveAllSkills implements /giveallskill: every published skill at level 5.
func cmdGiveAllSkills(ctx context.Context, sqldb *sql.DB, charID int64, c *evenet.Conn, accountID int64) (string, error) {
	if charID == 0 {
		return "No character selected.", nil
	}
	n, err := evedb.GiveAllSkills(ctx, sqldb, charID)
	if err != nil {
		return "", err
	}

	// Fetch all skill itemIDs and send OnSkillTrained notifications.
	if c != nil {
		skillRows, err := sqldb.QueryContext(ctx,
			`SELECT itemID FROM entity WHERE typeID IN (SELECT t.typeID FROM invTypes t JOIN invGroups g ON g.groupID = t.groupID WHERE g.categoryID = 16) AND ownerID = ? AND flag IN (7, 61)`,
			charID)
		if err == nil {
			defer skillRows.Close()
			for skillRows.Next() {
				var itemID int64
				if err := skillRows.Scan(&itemID); err == nil {
					notifArgs := marshal.PyTuple{Items: []marshal.PyRep{
						marshal.PyInt{Value: uint64(itemID)},
					}}
					_ = server.SendNotification(c, accountID, "OnSkillTrained", "charid", notifArgs)
				}
			}
		}
	}

	return fmt.Sprintf("Granted %d skills at level 5. Relog or open skill sheet to see changes.", n), nil
}

// ─── /giveskill <typeID> <level> ─────────────────────────────────────────────

// cmdGiveSkill grants a single skill at a specific level (1-5).
// Use /search to find the skill's typeID first.
func cmdGiveSkill(ctx context.Context, sqldb *sql.DB, charID int64, args []string, c *evenet.Conn, accountID int64) (string, error) {
	if len(args) < 2 {
		return "Usage: /giveskill <typeID> <level>   (level 1-5, use /search to find typeID)", nil
	}
	if charID == 0 {
		return "No character selected.", nil
	}

	typeID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return "argument 1 should be a skill typeID", nil
	}

	level, err := strconv.Atoi(args[1])
	if err != nil || level < 1 || level > 5 {
		return "argument 2 should be a level between 1 and 5", nil
	}

	typeName, err := evedb.GiveSkill(ctx, sqldb, charID, typeID, level)
	if err != nil {
		return "", err
	}

	// Find the skill itemID and send OnSkillTrained notification.
	if c != nil {
		var itemID int64
		_ = sqldb.QueryRowContext(ctx,
			`SELECT itemID FROM entity WHERE typeID = ? AND ownerID = ? AND flag IN (7, 61) LIMIT 1`,
			typeID, charID).Scan(&itemID)
		if itemID > 0 {
			notifArgs := marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyInt{Value: uint64(itemID)},
			}}
			_ = server.SendNotification(c, accountID, "OnSkillTrained", "charid", notifArgs)
		}
	}

	return fmt.Sprintf("Granted %s (typeID %d) at level %d. Relog or open skill sheet to see changes.",
		typeName, typeID, level), nil
}

// ─── /moveto <stationID> ────────────────────────────────────────────────────

// cmdMoveTo teleports the character and active ship to another station: a DB update, then a
// session change so the client follows without a relog.
func cmdMoveTo(ctx context.Context, st *connState, args []string) (string, error) {
	if len(args) < 1 {
		return "Usage: /moveto <stationID>", nil
	}
	if st.charID == 0 {
		return "No character selected.", nil
	}
	if st.stationID == nil || *st.stationID == 0 {
		return "You must be docked to use /moveto.", nil
	}

	newStationID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return "argument 1 should be a station ID", nil
	}
	oldStationID := *st.stationID

	if newStationID == oldStationID {
		return "You are already at that station.", nil
	}

	// Look up the destination station's location hierarchy.
	destStation, err := evedb.GetStationLocation(ctx, st.sqldb, newStationID)
	if err != nil {
		return "", fmt.Errorf("destination station: %w", err)
	}

	// Look up the current station's location hierarchy (for the SCN old values).
	srcStation, err := evedb.GetStationLocation(ctx, st.sqldb, oldStationID)
	if err != nil {
		return "", fmt.Errorf("source station: %w", err)
	}

	// Get the character's active ship.
	shipID, err := evedb.GetCurrentShipID(ctx, st.sqldb, st.charID)
	if err != nil {
		return "", fmt.Errorf("get ship: %w", err)
	}

	// Move the ship entity to the new station.
	if shipID > 0 {
		if err := evedb.UpdateEntityLocation(ctx, st.sqldb, shipID, newStationID); err != nil {
			return "", fmt.Errorf("move ship: %w", err)
		}
	}
	// Update character's station and full location hierarchy.
	if err := evedb.UpdateCharacterStation(ctx, st.sqldb, st.charID,
		newStationID, destStation.SolarSystemID, destStation.ConstellationID, destStation.RegionID); err != nil {
		return "", fmt.Errorf("update char station: %w", err)
	}

	// Update the connection's live stationID.
	*st.stationID = newStationID

	// Move the guest-list entry so GetGuests reflects the new station.
	if st.stationReg != nil && st.c != nil {
		st.stationReg.Join(st.c, newStationID, st.charID)
	}

	// Swap Local chat when the teleport crosses systems, or the client keeps two Local tabs.
	if st.lscReg != nil && st.c != nil && srcStation.SolarSystemID != destStation.SolarSystemID {
		lsc.SwapSystemChannel(st.c, st.accountID, srcStation.SolarSystemID, destStation.SolarSystemID, st.lscReg, st.sqldb)
	}

	// ── Session change notification ─────────────────────────────────────
	if err := sendMoveToSessionChange(st, srcStation, destStation, shipID); err != nil {
		return fmt.Sprintf("Moved to %s (DB updated) but session change failed: %v — relog to sync.",
			destStation.StationName, err), nil
	}

	return fmt.Sprintf("Moved to %s (station %d, system %d). Relog if the client doesn't update.",
		destStation.StationName, newStationID, destStation.SolarSystemID), nil
}

func sendMoveToSessionChange(st *connState, src, dst *evedb.StationLocation, shipID int64) error {
	none := marshal.PyNone{}
	entries := []marshal.DictEntry{
		{Key: marshal.PyString{Value: "stationid"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(src.StationID)}, marshal.PyInt{Value: uint64(dst.StationID)},
		}}},
		{Key: marshal.PyString{Value: "solarsystemid2"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(src.SolarSystemID)}, marshal.PyInt{Value: uint64(dst.SolarSystemID)},
		}}},
		{Key: marshal.PyString{Value: "locationid"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(src.StationID)}, marshal.PyInt{Value: uint64(dst.StationID)},
		}}},
	}
	// solarsystemid is None to None when docked; sent so the client drops a stale value.
	entries = append(entries, marshal.DictEntry{
		Key: marshal.PyString{Value: "solarsystemid"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			none, none,
		}},
	})
	if src.ConstellationID != dst.ConstellationID {
		entries = append(entries, marshal.DictEntry{
			Key: marshal.PyString{Value: "constellationid"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyInt{Value: uint64(src.ConstellationID)}, marshal.PyInt{Value: uint64(dst.ConstellationID)},
			}},
		})
	}
	if src.RegionID != dst.RegionID {
		entries = append(entries, marshal.DictEntry{
			Key: marshal.PyString{Value: "regionid"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyInt{Value: uint64(src.RegionID)}, marshal.PyInt{Value: uint64(dst.RegionID)},
			}},
		})
	}
	if shipID > 0 {
		entries = append(entries, marshal.DictEntry{
			Key: marshal.PyString{Value: "shipid"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyInt{Value: uint64(shipID)}, marshal.PyInt{Value: uint64(shipID)},
			}},
		})
	}

	return sendSessionChange(st.c, uint64(st.accountID), entries)
}

// ─── /createitem <typeID> [quantity] ────────────────────────────────────────

// cmdCreateItem creates an item in the character's hangar at the current station.
func cmdCreateItem(ctx context.Context, st *connState, args []string) (string, error) {
	if len(args) < 1 {
		return "Usage: /createitem <typeID> [quantity]", nil
	}
	if st.charID == 0 {
		return "No character selected.", nil
	}
	if st.stationID == nil || *st.stationID == 0 {
		return "You must be docked to use /createitem.", nil
	}

	typeID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return "argument 1 should be a typeID (use /search to find it)", nil
	}

	quantity := 1
	if len(args) >= 2 {
		q, err := strconv.Atoi(args[1])
		if err != nil || q < 1 {
			return "argument 2 should be a positive quantity", nil
		}
		quantity = q
	}

	itemID, err := evedb.CreateItem(ctx, st.sqldb, typeID, st.charID, *st.stationID, quantity)
	if err != nil {
		return "", err
	}

	// OnItemChange needs old locationID 0 to mean a new item; None would read as a removal.
	er, err := evedb.GetEntityRow(ctx, st.sqldb, itemID)
	if err == nil && er != nil {
		notifArgs := marshal.PyTuple{Items: []marshal.PyRep{
			evedb.BuildEntityRow(er),
			marshal.PyDict{Items: []marshal.DictEntry{
				{Key: marshal.PyInt{Value: 3}, Value: marshal.PyInt{Value: 0}}, // ixLocationID: old=0 (new item)
			}},
		}}
		if nErr := server.SendNotification(st.c, st.accountID, "OnItemChange", "charid", notifArgs); nErr != nil {
			log.Printf("[%s] createitem OnItemChange: %v", st.addr, nErr)
		}
	}

	return fmt.Sprintf("Created item: typeID=%d qty=%d → entityID=%d in station %d hangar.",
		typeID, quantity, itemID, *st.stationID), nil
}

// ─── /create ─────────────────────────────────────────────────────────────────

// cmdCreate implements the info window's "GML: Create in cargo" button (infosvc.py
// DoCreateMaterials sends "/create <typeID> <qty>" per Bill-of-Materials line). Items go into
// the active ship's cargo, merging into an existing stack.
func cmdCreate(ctx context.Context, st *connState, args []string) (string, error) {
	if len(args) < 1 {
		return "Usage: /create <typeID> [quantity]", nil
	}
	if st.charID == 0 {
		return "No character selected.", nil
	}

	typeID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return "argument 1 should be a typeID (use /search to find it)", nil
	}
	quantity := int64(1)
	if len(args) >= 2 {
		q, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil || q < 1 {
			return "argument 2 should be a positive quantity", nil
		}
		quantity = q
	}

	shipID, err := evedb.GetCurrentShipID(ctx, st.sqldb, st.charID)
	if err != nil || shipID == 0 {
		return "You have no active ship to receive the items.", nil
	}

	// Merge into an existing cargo stack of the type when present.
	var itemID, oldQty int64
	err = st.sqldb.QueryRowContext(ctx, `
		SELECT itemID, quantity FROM entity
		 WHERE typeID = ? AND ownerID = ? AND locationID = ? AND flag = 5 AND singleton = 0
		 LIMIT 1`, typeID, st.charID, shipID).Scan(&itemID, &oldQty)
	merged := err == nil
	if merged {
		if _, err := st.sqldb.ExecContext(ctx,
			`UPDATE entity SET quantity = quantity + ? WHERE itemID = ?`, quantity, itemID); err != nil {
			return "", fmt.Errorf("cmdCreate merge: %w", err)
		}
	} else {
		itemID, err = evedb.CreateCargoItem(ctx, st.sqldb, typeID, st.charID, shipID, quantity)
		if err != nil {
			return "", err
		}
	}

	if er, erErr := evedb.GetEntityRow(ctx, st.sqldb, itemID); erErr == nil && er != nil {
		changeDict := marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyInt{Value: 3}, Value: marshal.PyInt{Value: 0}}, // ixLocationID: old=0 (new item)
		}}
		if merged {
			changeDict = marshal.PyDict{Items: []marshal.DictEntry{
				{Key: marshal.PyInt{Value: 7}, Value: marshal.PyInt{Value: uint64(oldQty)}}, // ixQuantity
			}}
		}
		if nErr := server.SendNotification(st.c, st.accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
			evedb.BuildEntityRow(er),
			changeDict,
		}}); nErr != nil {
			log.Printf("[%s] create OnItemChange: %v", st.addr, nErr)
		}
	}

	return fmt.Sprintf("Created typeID=%d qty=%d in ship %d cargo.", typeID, quantity, shipID), nil
}

// ─── /seedmarkets ────────────────────────────────────────────────────────────

// cmdSeedMarkets replaces the Seeder-NPC sell orders at the caller's station with one sell
// order per published market type.
func cmdSeedMarkets(_ context.Context, st *connState) (string, error) {
	if st.stationID == nil || *st.stationID == 0 {
		return "You must be docked to use /seedmarkets.", nil
	}
	stationID := *st.stationID

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	log.Printf("[%s] /seedmarkets: seeding station %d (charID=%d)", st.addr, stationID, st.charID)
	start := time.Now()
	deleted, inserted, err := evedb.SeedStationMarket(ctx, st.sqldb, stationID)
	if err != nil {
		return "", err
	}
	elapsed := time.Since(start).Truncate(time.Millisecond)
	log.Printf("[%s] /seedmarkets: station %d done in %s (deleted=%d, inserted=%d)",
		st.addr, stationID, elapsed, deleted, inserted)
	return fmt.Sprintf("Seeded station %d in %s: wiped %d old Seeder orders, inserted %d new.",
		stationID, elapsed, deleted, inserted), nil
}

// cmdSeedNPCMarkets regenerates every NPC market order from crpnpccorporationtrades: it purges
// all Seeder-NPC orders, then inserts corp-specific orders with dynamic pricing at every
// NPC-owned station.
func cmdSeedNPCMarkets(_ context.Context, st *connState) (string, error) {
	log.Printf("[%s] /seednpcmarkets: seeding NPC market orders (charID=%d)", st.addr, st.charID)
	// Long timeout: this rewrites most of market_orders.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	start := time.Now()
	deleted, inserted, err := evedb.SeedNPCMarketOrders(ctx, st.sqldb)
	if err != nil {
		return "", err
	}
	elapsed := time.Since(start).Truncate(time.Millisecond)
	log.Printf("[%s] /seednpcmarkets: done in %s (deleted=%d, inserted=%d)",
		st.addr, elapsed, deleted, inserted)
	return fmt.Sprintf("Seeded NPC markets in %s: purged %d old orders, inserted %d new.",
		elapsed, deleted, inserted), nil
}

// ─── /mission ────────────────────────────────────────────────────────────────

// cmdMission is the GM tool for mission testing: list, give <templateID>, complete, clear, info.
func cmdMission(ctx context.Context, st *connState, args []string) (string, error) {
	if len(args) == 0 {
		return `Usage:
  /mission list              — list enabled mission templates
  /mission give <templateID> — force-offer a mission template to yourself
  /mission complete          — force-complete your active mission (any agent)
  /mission clear             — fail all your active/offered missions
  /mission info              — show your current active mission`, nil
	}
	if st.charID == 0 {
		return "No character selected.", nil
	}

	sub := strings.ToLower(args[0])
	switch sub {
	case "list":
		return cmdMissionList(ctx, st)
	case "give":
		if len(args) < 2 {
			return "Usage: /mission give <templateID>", nil
		}
		templateID, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil {
			return "templateID must be a number", nil
		}
		return cmdMissionGive(ctx, st, templateID)
	case "complete":
		return cmdMissionComplete(ctx, st)
	case "clear":
		return cmdMissionClear(ctx, st)
	case "info":
		return cmdMissionInfo(ctx, st)
	default:
		return fmt.Sprintf("Unknown mission subcommand %q. Try /mission for help.", sub), nil
	}
}

func cmdMissionList(ctx context.Context, st *connState) (string, error) {
	templates, err := evedb.ESPGetAllMissionTemplates(ctx, st.sqldb)
	if err != nil {
		return "", err
	}
	if len(templates) == 0 {
		return "No mission templates found. Create some via ESP /missions.", nil
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d mission template(s):\n", len(templates))
	limit := len(templates)
	if limit > 30 {
		limit = 30
	}
	for _, t := range templates[:limit] {
		enabled := "ON"
		if !t.Enabled {
			enabled = "off"
		}
		fmt.Fprintf(&sb, "  %d: [L%d %s] %s (%s)\n", t.TemplateID, t.AgentLevel, t.MissionType, t.MissionName, enabled)
	}
	if len(templates) > 30 {
		fmt.Fprintf(&sb, "  … and %d more", len(templates)-30)
	}
	return sb.String(), nil
}

func cmdMissionGive(ctx context.Context, st *connState, templateID int64) (string, error) {
	tmpl, err := evedb.GetTemplateByID(ctx, st.sqldb, templateID)
	if err != nil {
		return "", fmt.Errorf("template %d not found: %w", templateID, err)
	}

	// Any agent at the player's station offers it.
	var agentID int64
	if st.stationID != nil && *st.stationID > 0 {
		err := st.sqldb.QueryRowContext(ctx,
			`SELECT agentID FROM agtAgents WHERE locationID = ? LIMIT 1`,
			*st.stationID).Scan(&agentID)
		if err != nil {
			// No agent at the station: agentID 0.
			agentID = 0
		}
	}

	// Compute rewards at quality=0 (multiplier=1.0).
	rewardISK, bonusISK, rewardLP := evedb.ComputeRewards(tmpl, 0)

	missionID, err := evedb.CreateMissionOffer(ctx, st.sqldb,
		st.charID, agentID, templateID,
		0, 0,       // destSystemID, destStationID
		0, 0,       // pickupStationID, dropoffStationID
		0, 0, 0,    // bookmarkX, bookmarkY, bookmarkZ
		rewardISK, bonusISK, rewardLP,
		tmpl.TargetCount)
	if err != nil {
		return "", err
	}

	// Auto-accept it.
	if err := evedb.AcceptMission(ctx, st.sqldb, missionID, tmpl.BonusTimeSec, tmpl.ExpiryDays); err != nil {
		return fmt.Sprintf("Mission %d offered but accept failed: %v", missionID, err), nil
	}

	return fmt.Sprintf("Mission %d created and accepted: [L%d %s] %s (agentID=%d, templateID=%d)",
		missionID, tmpl.AgentLevel, tmpl.MissionType, tmpl.MissionName, agentID, templateID), nil
}

func cmdMissionComplete(ctx context.Context, st *connState) (string, error) {
	// Find any accepted mission for this character.
	missions, err := evedb.GetAcceptedMissions(ctx, st.sqldb, st.charID)
	if err != nil {
		return "", err
	}
	if len(missions) == 0 {
		return "No active missions found.", nil
	}

	m := missions[0]
	if err := evedb.CompleteMission(ctx, st.sqldb, m.MissionID); err != nil {
		return "", err
	}

	// Grant rewards, the ISK less corp tax as an agent turn-in pays it.
	var paidISK float64
	if m.RewardISK > 0 {
		paidISK, _, _ = evedb.PayMissionISK(ctx, st.sqldb, st.charID, m.AgentID, evedb.RefAgentMissionReward, float64(m.RewardISK))
	}
	if m.RewardLP > 0 {
		_ = evedb.AddLoyaltyPoints(ctx, st.sqldb, st.charID, m.AgentID, m.RewardLP)
	}

	tmpl, _ := evedb.GetTemplateByID(ctx, st.sqldb, m.TemplateID)
	name := fmt.Sprintf("templateID=%d", m.TemplateID)
	if tmpl != nil {
		name = tmpl.MissionName
	}

	return fmt.Sprintf("Force-completed mission %d: %s. Granted %.0f ISK after corp tax (reward %d), %d LP.",
		m.MissionID, name, paidISK, m.RewardISK, m.RewardLP), nil
}

func cmdMissionClear(ctx context.Context, st *connState) (string, error) {
	res, err := st.sqldb.ExecContext(ctx,
		`UPDATE characterMissions SET state='failed'
		 WHERE charID = ? AND state IN ('offered','accepted')`, st.charID)
	if err != nil {
		return "", err
	}
	n, _ := res.RowsAffected()
	return fmt.Sprintf("Failed %d active mission(s).", n), nil
}

func cmdMissionInfo(ctx context.Context, st *connState) (string, error) {
	missions, err := evedb.GetAcceptedMissions(ctx, st.sqldb, st.charID)
	if err != nil {
		return "", err
	}
	offers, err := evedb.GetOfferedMissions(ctx, st.sqldb, st.charID)
	if err != nil {
		return "", err
	}
	if len(missions) == 0 && len(offers) == 0 {
		return "No active or offered missions.", nil
	}

	var sb strings.Builder
	if len(offers) > 0 {
		fmt.Fprintf(&sb, "Offered (%d):\n", len(offers))
		for _, m := range offers {
			tmpl, _ := evedb.GetTemplateByID(ctx, st.sqldb, m.TemplateID)
			name := fmt.Sprintf("templateID=%d", m.TemplateID)
			if tmpl != nil {
				name = tmpl.MissionName
			}
			fmt.Fprintf(&sb, "  #%d: %s (agent=%d)\n", m.MissionID, name, m.AgentID)
		}
	}
	if len(missions) > 0 {
		fmt.Fprintf(&sb, "Accepted (%d):\n", len(missions))
		for _, m := range missions {
			tmpl, _ := evedb.GetTemplateByID(ctx, st.sqldb, m.TemplateID)
			name := fmt.Sprintf("templateID=%d", m.TemplateID)
			mType := "unknown"
			if tmpl != nil {
				name = tmpl.MissionName
				mType = tmpl.MissionType
			}
			fmt.Fprintf(&sb, "  #%d: [%s] %s (agent=%d, kills=%d/%d, reward=%dISK %dLP)\n",
				m.MissionID, mType, name, m.AgentID, m.KillProgress, m.KillRequired, m.RewardISK, m.RewardLP)
		}
	}
	return sb.String(), nil
}

// ─── /fit <typeID> <slotFlag> ────────────────────────────────────────────────

func cmdFit(ctx context.Context, st *connState, args []string) (string, error) {
	if len(args) < 2 {
		return "Usage: /fit <typeID> <slotFlag>  (hi=11-18, med=19-26, lo=27-34)", nil
	}
	if st.charID == 0 {
		return "No character selected.", nil
	}
	typeID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return "argument 1 should be a module typeID (use /search to find it)", nil
	}
	slotFlag, err := strconv.Atoi(args[1])
	if err != nil || slotFlag < 11 || slotFlag > 34 {
		return "argument 2 should be a slot flag (hi=11-18, med=19-26, lo=27-34)", nil
	}
	shipID, err := evedb.GetCurrentShipID(ctx, st.sqldb, st.charID)
	if err != nil || shipID == 0 {
		return "Could not find your current ship.", nil
	}
	itemID, err := evedb.CreateFittedModule(ctx, st.sqldb, typeID, st.charID, shipID, slotFlag)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Fitted module: typeID=%d → entityID=%d in ship %d slot %d. Relog to see in module rack.",
		typeID, itemID, shipID, slotFlag), nil
}

// ─── /setcorp <corpID> ─────────────────────────────────────────────────────

// cmdSetCorp moves the character to another corporation (member counts, employment, roles) and
// sends a session change so the client updates at once.
func cmdSetCorp(ctx context.Context, st *connState, args []string) (string, error) {
	if len(args) < 1 {
		return "Usage: /setcorp <corporationID>", nil
	}
	if st.charID == 0 {
		return "No character selected.", nil
	}

	newCorpID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return "argument 1 should be a corporation ID", nil
	}

	// Get current corp.
	oldCorpID, err := evedb.GetCharCorpID(ctx, st.sqldb, st.charID)
	if err != nil {
		return "", err
	}
	if newCorpID == oldCorpID {
		return fmt.Sprintf("Already in corporation %d.", newCorpID), nil
	}

	// Validate that target corp exists.
	exists, err := evedb.CorpExists(ctx, st.sqldb, newCorpID)
	if err != nil {
		return "", err
	}
	if !exists {
		return fmt.Sprintf("Corporation %d does not exist.", newCorpID), nil
	}

	// Perform the full corp transfer (member counts, char row, employment, roles).
	roles, err := evedb.JoinCorporation(ctx, st.sqldb, st.charID, newCorpID, oldCorpID)
	if err != nil {
		return "", fmt.Errorf("join corp: %w", err)
	}

	// Send session change notification.
	if err := SendCorpSessionChange(st.c, oldCorpID, newCorpID, roles); err != nil {
		return fmt.Sprintf("Corp changed to %d (DB updated) but session change failed: %v — relog to sync.",
			newCorpID, err), nil
	}
	_ = SendCorpAllianceSessionChange(ctx, st.sqldb, st.c, oldCorpID, newCorpID)

	return fmt.Sprintf("Corporation changed: %d → %d. Relog if the client doesn't update.", oldCorpID, newCorpID), nil
}

// SendCorpSessionChange sends a SessionChangeNotification updating the
// client's corporation, roles, and account key. roles is the mask granted
// by JoinCorporation for the new corp.
func SendCorpSessionChange(c *evenet.Conn, oldCorpID, newCorpID int64, roles uint64) error {
	none := marshal.PyNone{}
	entries := []marshal.DictEntry{
		{Key: marshal.PyString{Value: "corpid"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(oldCorpID)}, marshal.PyInt{Value: uint64(newCorpID)},
		}}},
		{Key: marshal.PyString{Value: "corprole"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: 0}, marshal.PyInt{Value: roles},
		}}},
		{Key: marshal.PyString{Value: "rolesAtAll"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: 0}, marshal.PyInt{Value: roles},
		}}},
		{Key: marshal.PyString{Value: "rolesAtBase"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: 0}, marshal.PyInt{Value: roles},
		}}},
		{Key: marshal.PyString{Value: "rolesAtHQ"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: 0}, marshal.PyInt{Value: roles},
		}}},
		{Key: marshal.PyString{Value: "rolesAtOther"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: 0}, marshal.PyInt{Value: roles},
		}}},
		{Key: marshal.PyString{Value: "corpAccountKey"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			none, marshal.PyInt{Value: uint64(corp.CorpAccountKey)},
		}}},
	}
	return sendSessionChange(c, 0, entries)
}

// SendCorpAllianceSessionChange follows a corpid session change with the allianceid one when
// the two corporations sit in different alliances. "No alliance" travels as None, never 0.
// It sends nothing when the alliance is the same or a lookup fails.
func SendCorpAllianceSessionChange(ctx context.Context, db *sql.DB, c *evenet.Conn, oldCorpID, newCorpID int64) error {
	if c == nil || db == nil || oldCorpID == newCorpID {
		return nil
	}
	oldAlliance, err := evedb.GetCorpAllianceID(ctx, db, oldCorpID)
	if err != nil {
		return nil
	}
	newAlliance, err := evedb.GetCorpAllianceID(ctx, db, newCorpID)
	if err != nil || oldAlliance == newAlliance {
		return nil
	}
	return sendSessionChange(c, 0, []marshal.DictEntry{
		{Key: marshal.PyString{Value: "allianceid"}, Value: marshal.PyTuple{Items: []marshal.PyRep{
			allianceIDRep(oldAlliance), allianceIDRep(newAlliance),
		}}},
	})
}

// allianceIDRep is an alliance ID as a session value: None for no alliance.
func allianceIDRep(id int64) marshal.PyRep {
	if id == 0 {
		return marshal.PyNone{}
	}
	return marshal.PyInt{Value: uint64(id)}
}

// ─── shared SCN helper ─────────────────────────────────────────────────────

// sendSessionChange builds and sends a macho.SessionChangeNotification with
// the given dict entries.  Uses server.NodeID for the source address.
func sendSessionChange(c *evenet.Conn, accountID uint64, entries []marshal.DictEntry) error {
	payload := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: 0},
			marshal.PyDict{Items: entries},
		}},
		marshal.PyList{Items: []marshal.PyRep{marshal.PyInt{Value: server.NodeID}}},
	}}
	pkt := &evenet.PyPacket{
		TypeString: "macho.SessionChangeNotification",
		Type:       evenet.SessionChangeNotification,
		Source: evenet.PyAddress{
			Type:   evenet.AddrNode,
			TypeID: server.NodeID,
		},
		Dest: evenet.PyAddress{
			Type:   evenet.AddrClient,
			TypeID: 130293001608,
		},
		UserID:  accountID,
		Payload: payload,
		NamedPayload: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "channel"}, Value: marshal.PyString{Value: "sessionchange"}},
		}},
	}
	b, err := marshal.Marshal(pkt.Encode())
	if err != nil {
		return err
	}
	return c.SendPacket(b)
}

// Execute runs a slash command without an EVE client connection, for the ESP web console.
// Commands that need a live connection update the DB and skip the notification.
func Execute(ctx context.Context, sqldb *sql.DB, cmdLine string) (string, error) {
	cmdLine = strings.TrimSpace(cmdLine)
	if len(cmdLine) > 0 && (cmdLine[0] == '/' || cmdLine[0] == '#') {
		cmdLine = cmdLine[1:]
	}
	parts := strings.Fields(cmdLine)
	if len(parts) == 0 {
		return "Empty command.", nil
	}
	cmd := strings.ToLower(parts[0])
	args := parts[1:]

	// A minimal connState with no connection; commands that need st.c see nil.
	var stationID int64
	st := &connState{
		c:         nil,
		addr:      "ESP-console",
		accountID: 0,
		charID:    0,
		stationID: &stationID,
		sqldb:     sqldb,
	}

	result, err := dispatch(ctx, st, cmd, args)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), nil
	}
	return result, nil
}

// cmdHelp returns the list of available GM commands.
func cmdHelp() string {
	return `Available GM commands:
  /search <text>              - Search invTypes by name (regex ok)
  /giveisk <charID> <amount>  - Add ISK to a character (0=self)
  /giveallskill               - Give yourself all skills at level 5 (alias: /allskills)
  /giveskill <typeID> <level> - Give a specific skill at level 1-5
  /moveto <stationID>         - Teleport to station (DB + session change)
  /createitem <typeID> [qty]  - Spawn item in your hangar (use /search for typeID)
  /create <typeID> [qty]      - Spawn item in your ship cargo (info window "GML: Create in cargo")
  /seedmarkets                - Seed every published item at your current station (wipes old Seeder orders here)
  /seednpcmarkets             - Regenerate all NPC corp market orders globally (from crpnpccorporationtrades)
  /fit <typeID> <slotFlag>    - Fit module to ship (hi=11-18, med=19-26, lo=27-34)
  /setcorp <corpID>           - Change your corporation
  /claim [systemID]           - Claim 0.0 sovereignty for your alliance
  /unclaim [systemID]         - Release sovereignty held by your alliance
  /sov [systemID|list]        - Show sovereignty (current system, or your alliance's holdings)
  /posplant <typeID> <moonID> - Anchor a control tower at a moon (alliance sov required)
  /posonline <entityID>       - Bring an anchored starbase online
  /posremove <entityID>       - Remove a starbase (admin teardown)
  /poslist [systemID]         - List starbases in current or named system
  /fuel <days> <entityID>     - Load N days of fuel into a starbase bay
  /moonscan [moonID]          - Survey a moon (default: nearest) into the Moon Analysis tab
  /gm on | /gm off            - Nothing may lock, approach or shoot you; NPCs ignore you
  /scene [filter]             - Server's distance to every ball on your grid, to read against the overview
  /stamps                     - Your destiny stamp stream and which server call sites are spending it
  /ghosts                     - Explain every ball in your ballpark: why each is kept, or that it is stale
  /dupes                      - Balls this client was sent twice (a doubled overview row); should be empty
  /deadspace                  - Teleport to random isolated spot in system
  /goto <x> <y> <z>          - Snap to position in current system
  /translocate <entityID>     - Teleport to a system, station, or celestial
  /tr me <entityID>           - Same as /translocate
  /mission list|give|complete|clear|info - Mission GM tools
  /redoportrait <char name|ID> - Flag char for portrait redo at next login (prefix 'clear' to unflag)
  /spawn <typeID>             - Spawn NPC [stub]
  /gateflash                  - Fire gate animation on nearby stargates
  /roid <typeID> <radius>     - Spawn asteroid [stub]
  /growbelt                   - Trigger belt growth [stub]
  /pop <type> <key> <value>   - Send OnRemoteMessage [stub]`
}
