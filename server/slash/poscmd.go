package slash

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	evedb "gurimoon/db"
)

// cmdPosPlant implements /posplant <typeID> <moonID>. It anchors a control tower
// at a moon in a 0.0 system held by the caller's alliance and registers it in the
// starbase overlay table in the "anchored" state; /posonline brings it up.
func cmdPosPlant(ctx context.Context, st *connState, args []string) (string, error) {
	if len(args) < 2 {
		return "Usage: /posplant <typeID> <moonID>", nil
	}
	if st.charID == 0 {
		return "/posplant requires an in-game character.", nil
	}
	typeID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return "argument 1 should be a control tower typeID (use /search to find it)", nil
	}
	moonID, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		return "argument 2 should be a moon itemID", nil
	}

	_, corpID, allianceID, err := evedb.GetCharSovContext(ctx, st.sqldb, st.charID)
	if err != nil {
		return "", err
	}

	// db.PlantStarbaseFor does the whole operation (placement policy, plant, live push)
	// and is shared with other tower builders; only the wording is local.
	res, err := evedb.PlantStarbaseFor(ctx, st.sqldb, evedb.PlantRequest{
		CorpID: corpID, AllianceID: allianceID, TypeID: typeID, MoonID: moonID, ActorCharID: st.charID,
	}, slashLive(st))
	if errors.Is(err, evedb.ErrMoonNotFound) {
		return fmt.Sprintf("Moon %d not found in mapDenormalize.", moonID), nil
	}
	moonName := res.MoonName
	if err != nil {
		var refusal *evedb.StarbaseAnchorRefusal
		if !errors.As(err, &refusal) {
			return "", err
		}
		switch refusal.Reason {
		case evedb.AnchorRefuseSecurity:
			return fmt.Sprintf("Moon %s is in %.1f-sec space. Starbases can only be anchored below %.2f.",
				moonName, refusal.Security, refusal.MaxSecurity), nil
		}
		return refusal.Error(), nil
	}
	entityID := res.EntityID

	// Sovereignty follows the majority of online structures (db/sovmajority.go), so a
	// freshly anchored tower claims nothing yet.
	sovMsg := ""
	if res.Decision.Nullsec && res.Decision.AllianceID != 0 {
		sovMsg = " It will count toward your alliance's sovereignty here once online."
	}

	liveMsg := " Jump out and back to see it."
	if res.Live != "" {
		liveMsg = res.Live
	}

	return fmt.Sprintf(
		"Anchored starbase entityID=%d (typeID=%d) at moon %s. State: anchored.%s%s /posonline %d to bring it online.",
		entityID, typeID, moonName, sovMsg, liveMsg, entityID), nil
}

// cmdPosOnline implements /posonline <entityID>.  Transitions anchored→online.
func cmdPosOnline(ctx context.Context, st *connState, args []string) (string, error) {
	if len(args) < 1 {
		return "Usage: /posonline <entityID>   (use /poslist to find the ID)", nil
	}
	entityID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return "argument 1 should be a starbase entityID", nil
	}
	rec, ok, err := evedb.GetStarbase(ctx, st.sqldb, entityID)
	if err != nil {
		return "", err
	}
	if !ok {
		return fmt.Sprintf("No starbase with entityID=%d.", entityID), nil
	}
	if err := checkPosOwner(ctx, st, rec); err != "" {
		return err, nil
	}
	newState, liveMsg, oErr := evedb.OnlineStarbaseFor(ctx, st.sqldb, entityID, slashLive(st))
	if oErr != nil {
		return "", oErr
	}
	return fmt.Sprintf("Starbase %d is now %s.%s", entityID, newState, liveMsg), nil
}

// slashLive is the connection's own live notifier (its logs carry the
// caller's address), or nil to fall back on db.StarbaseLive.
func slashLive(st *connState) evedb.StarbaseLiveNotifier {
	if n, ok := st.tp.(StarbaseNotifier); ok {
		return n
	}
	return nil
}

// cmdFuel implements /fuel <days> <entityID>. It puts that many days of the tower's
// per-cycle fuel bill (evedb.StarbaseFuelNeeds, the same as the fuel sweep) into the
// structure bay. There is no capacity check: a GM fill may overflow the bay.
func cmdFuel(ctx context.Context, st *connState, args []string) (string, error) {
	if len(args) < 2 {
		return "Usage: /fuel <days> <entityID>   (use /poslist to find the ID)", nil
	}
	days, err := strconv.ParseFloat(args[0], 64)
	if err != nil || days <= 0 {
		return "argument 1 should be a positive number of days (0.5 works too)", nil
	}
	if days > 3650 {
		return "argument 1 is capped at 3650 days.", nil
	}
	entityID, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		return "argument 2 should be a starbase entityID", nil
	}
	rec, ok, err := evedb.GetStarbase(ctx, st.sqldb, entityID)
	if err != nil {
		return "", err
	}
	if !ok {
		return fmt.Sprintf("No starbase with entityID=%d.", entityID), nil
	}
	if msg := checkPosOwner(ctx, st, rec); msg != "" {
		return msg, nil
	}

	// db.FuelStarbaseFor: the fuel sweep's own bill, days → cycles rounded up.
	fuel, err := evedb.FuelStarbaseFor(ctx, st.sqldb, rec, days)
	if err != nil {
		return "", err
	}
	if len(fuel.Lines) == 0 {
		return fmt.Sprintf(
			"Starbase %d (typeID %d) has no online fuel requirements — it runs for free.",
			entityID, rec.TypeID), nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Loaded %s day(s) of fuel (%d cycle(s)) into starbase %d:\n",
		strconv.FormatFloat(days, 'f', -1, 64), fuel.Cycles, entityID)
	for _, line := range fuel.Lines {
		name, _ := evedb.GetTypeName(ctx, st.sqldb, line.TypeID)
		fmt.Fprintf(&sb, "  %d × %s (%d/cycle)\n",
			line.Qty, displayName(name, line.TypeID), line.PerCycle)
	}
	if rec.State != evedb.StarbaseStateOnline {
		fmt.Fprintf(&sb, "Tower state is %q — fuel only burns while online.", rec.State)
	}
	return sb.String(), nil
}

// cmdMoonScan implements /moonscan [moonID]. It reads (or generates) the moon's
// resource pool and pushes OnMoonScanComplete to the Moon Analysis tab; no argument
// scans the moon nearest the ship. GM/test stand-in for the survey-probe launch.
func cmdMoonScan(ctx context.Context, st *connState, args []string) (string, error) {
	scanner, ok := st.tp.(MoonScanner)
	if !ok {
		return "Moon scanning is not available on this connection.", nil
	}
	var moonID int64
	if len(args) >= 1 {
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			return "argument 1 should be a moon itemID (or omit it to scan the nearest moon)", nil
		}
		// Validate up front so a typo'd ID reads as "not a moon" instead of a
		// generation error.
		if _, _, found, lerr := lookupMoonGroup(ctx, st.sqldb, id); lerr != nil {
			return "", lerr
		} else if !found {
			return fmt.Sprintf("Item %d is not a moon.", id), nil
		}
		moonID = id
	}
	return scanner.MoonScan(moonID)
}

// lookupMoonGroup reports whether itemID is a moon (mapDenormalize groupID 8)
// and returns its system + name.
func lookupMoonGroup(ctx context.Context, sqldb *sql.DB, itemID int64) (sysID int64, name string, ok bool, err error) {
	err = sqldb.QueryRowContext(ctx,
		`SELECT solarSystemID, itemName FROM mapDenormalize WHERE itemID = ? AND groupID = 8`,
		itemID).Scan(&sysID, &name)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", false, nil
	}
	if err != nil {
		return 0, "", false, fmt.Errorf("lookupMoonGroup: %w", err)
	}
	return sysID, name, true, nil
}

// cmdPosRemove implements /posremove <entityID>. It deletes the tower outright,
// with no unanchor sequence.
func cmdPosRemove(ctx context.Context, st *connState, args []string) (string, error) {
	if len(args) < 1 {
		return "Usage: /posremove <entityID>", nil
	}
	entityID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return "argument 1 should be a starbase entityID", nil
	}
	rec, ok, err := evedb.GetStarbase(ctx, st.sqldb, entityID)
	if err != nil {
		return "", err
	}
	if !ok {
		return fmt.Sprintf("No starbase with entityID=%d.", entityID), nil
	}
	if err := checkPosOwner(ctx, st, rec); err != "" {
		return err, nil
	}
	// db.RemoveStarbaseFor: delete, release sov if it was the alliance's
	// last tower in the system, live push.
	res, err := evedb.RemoveStarbaseFor(ctx, st.sqldb, rec, slashLive(st))
	if err != nil {
		return "", err
	}
	sovMsg := ""
	if res.SovErr != nil {
		// Non-fatal: reported, and sov is left alone.
		sovMsg = fmt.Sprintf(" (could not release sovereignty: %v)", res.SovErr)
	} else if res.SovReleased {
		sysName, _ := evedb.GetSystemName(ctx, st.sqldb, rec.SolarSystemID)
		sovMsg = fmt.Sprintf(" Sovereignty released: %s (no remaining POSes).",
			displayName(sysName, rec.SolarSystemID))
	}

	liveMsg := " Jump out and back to clear it from the ballpark."
	if res.Live != "" {
		liveMsg = res.Live
	}

	return fmt.Sprintf("Starbase %d removed.%s%s", entityID, sovMsg, liveMsg), nil
}

// cmdPosList implements /poslist [systemID].  Defaults to caller's system.
func cmdPosList(ctx context.Context, st *connState, args []string) (string, error) {
	sysID, msg, err := resolveSystemArg(ctx, st.sqldb, st.charID, args)
	if err != nil {
		return "", err
	}
	if msg != "" {
		return msg, nil
	}
	recs, err := evedb.ListStarbasesForSystem(ctx, st.sqldb, sysID)
	if err != nil {
		return "", err
	}
	sysName, _ := evedb.GetSystemName(ctx, st.sqldb, sysID)
	if len(recs) == 0 {
		return fmt.Sprintf("%s (systemID %d) has no starbases.",
			displayName(sysName, sysID), sysID), nil
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s (systemID %d) — %d starbase(s):\n",
		displayName(sysName, sysID), sysID, len(recs))
	for _, r := range recs {
		moonName := "moon-" + strconv.FormatInt(r.MoonID, 10)
		if sysID, mn, ok, _ := lookupMoon(ctx, st.sqldb, r.MoonID); ok && sysID != 0 {
			moonName = mn
		}
		fmt.Fprintf(&sb, "  entityID=%d  typeID=%d  moon=%s  corp=%d  alliance=%d  state=%s\n",
			r.EntityID, r.TypeID, moonName, r.OwnerCorpID, r.OwnerAllianceID, r.State)
	}
	return sb.String(), nil
}

// lookupMoon returns the parent solarSystemID + itemName for a mapDenormalize
// row. ok=false if the itemID is missing.
func lookupMoon(ctx context.Context, sqldb *sql.DB, moonID int64) (sysID int64, name string, ok bool, err error) {
	err = sqldb.QueryRowContext(ctx,
		`SELECT solarSystemID, itemName FROM mapDenormalize WHERE itemID = ?`, moonID,
	).Scan(&sysID, &name)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", false, nil
	}
	if err != nil {
		return 0, "", false, fmt.Errorf("lookupMoon: %w", err)
	}
	return sysID, name, true, nil
}

// checkPosOwner returns a non-empty error message string if the caller is not
// allowed to manage this starbase. Returns "" on success. "Same alliance"
// counts as authorized: the admin-resolved path enforces no per-corp role
// checks.
func checkPosOwner(ctx context.Context, st *connState, rec evedb.StarbaseRecord) string {
	if st.charID == 0 {
		return "Command requires an in-game character."
	}
	_, corpID, allianceID, err := evedb.GetCharSovContext(ctx, st.sqldb, st.charID)
	if err != nil {
		return fmt.Sprintf("Error reading caller alliance: %v", err)
	}
	// Empire towers (no alliance) are corp property; 0.0 towers are managed
	// alliance-wide. db.StarbaseManagePermitted is the test the dogma effect path uses.
	if evedb.StarbaseManagePermitted(rec, corpID, allianceID) {
		return ""
	}
	if rec.OwnerAllianceID == 0 {
		return fmt.Sprintf("Starbase %d belongs to corpID %d; you are in corpID %d.",
			rec.EntityID, rec.OwnerCorpID, corpID)
	}
	return fmt.Sprintf("Starbase %d belongs to allianceID %d; you are in allianceID %d.",
		rec.EntityID, rec.OwnerAllianceID, allianceID)
}
