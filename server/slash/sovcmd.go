package slash

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	evedb "gurimoon/db"
)

// resolveSystemArg picks the solar system a sov command targets: args[0] parsed
// as a system ID, else the caller's current system (from character_). It returns
// (systemID, "") or (0, message) when there is neither.
func resolveSystemArg(ctx context.Context, sqldb *sql.DB, charID int64, args []string) (int64, string, error) {
	if len(args) >= 1 {
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			return 0, fmt.Sprintf("argument 1 should be a solarSystemID (got %q)", args[0]), nil
		}
		return id, "", nil
	}
	if charID == 0 {
		return 0, "No solarSystemID given and no character selected.", nil
	}
	sysID, _, _, err := evedb.GetCharSovContext(ctx, sqldb, charID)
	if err != nil {
		return 0, "", err
	}
	if sysID == 0 {
		return 0, "Your character has no current solar system. Pass a solarSystemID explicitly.", nil
	}
	return sysID, "", nil
}

// cmdClaim implements /claim: claim the caller's current 0.0 system for the
// caller's alliance.
func cmdClaim(ctx context.Context, st *connState, args []string) (string, error) {
	if st.charID == 0 {
		return "/claim requires an in-game character.", nil
	}
	sysID, msg, err := resolveSystemArg(ctx, st.sqldb, st.charID, args)
	if err != nil {
		return "", err
	}
	if msg != "" {
		return msg, nil
	}

	_, corpID, allianceID, err := evedb.GetCharSovContext(ctx, st.sqldb, st.charID)
	if err != nil {
		return "", err
	}
	if allianceID == 0 {
		return "Your corporation is not in an alliance. Sovereignty is claimed by alliances.", nil
	}

	sec, ok, err := evedb.GetSystemSecurity(ctx, st.sqldb, sysID)
	if err != nil {
		return "", err
	}
	if !ok {
		return fmt.Sprintf("Solar system %d does not exist.", sysID), nil
	}
	if sec >= 0.0 {
		sysName, _ := evedb.GetSystemName(ctx, st.sqldb, sysID)
		return fmt.Sprintf("%s (sec %.2f) is not 0.0 space. Sovereignty can only be claimed in lawless systems.",
			displayName(sysName, sysID), sec), nil
	}

	// Reject hostile re-claim unless the current owner is the same alliance.
	existing, owned, err := evedb.GetSov(ctx, st.sqldb, sysID)
	if err != nil {
		return "", err
	}
	if owned && existing.AllianceID != allianceID {
		curName, _ := evedb.GetAllianceName(ctx, st.sqldb, existing.AllianceID)
		return fmt.Sprintf("System %d is already claimed by %s (allianceID %d). It must be unclaimed first.",
			sysID, displayName(curName, existing.AllianceID), existing.AllianceID), nil
	}

	if err := evedb.SetSov(ctx, st.sqldb, sysID, allianceID, corpID, st.charID); err != nil {
		return "", err
	}

	sysName, _ := evedb.GetSystemName(ctx, st.sqldb, sysID)
	allyName, _ := evedb.GetAllianceName(ctx, st.sqldb, allianceID)
	return fmt.Sprintf("Sovereignty claimed: %s (sec %.2f) → %s (allianceID %d, corpID %d).",
		displayName(sysName, sysID), sec, displayName(allyName, allianceID), allianceID, corpID), nil
}

// cmdUnclaim implements /unclaim [systemID]: drop sov on the named (or current)
// system. Allowed when the caller's alliance owns it, or when no alliance holds it (no-op).
func cmdUnclaim(ctx context.Context, st *connState, args []string) (string, error) {
	if st.charID == 0 {
		return "/unclaim requires an in-game character.", nil
	}
	sysID, msg, err := resolveSystemArg(ctx, st.sqldb, st.charID, args)
	if err != nil {
		return "", err
	}
	if msg != "" {
		return msg, nil
	}

	_, _, allianceID, err := evedb.GetCharSovContext(ctx, st.sqldb, st.charID)
	if err != nil {
		return "", err
	}

	existing, owned, err := evedb.GetSov(ctx, st.sqldb, sysID)
	if err != nil {
		return "", err
	}
	if !owned {
		return fmt.Sprintf("System %d is not currently claimed.", sysID), nil
	}
	if existing.AllianceID != allianceID {
		curName, _ := evedb.GetAllianceName(ctx, st.sqldb, existing.AllianceID)
		return fmt.Sprintf("System %d is held by %s (allianceID %d), not your alliance.",
			sysID, displayName(curName, existing.AllianceID), existing.AllianceID), nil
	}

	n, err := evedb.ClearSov(ctx, st.sqldb, sysID)
	if err != nil {
		return "", err
	}
	sysName, _ := evedb.GetSystemName(ctx, st.sqldb, sysID)
	return fmt.Sprintf("Sovereignty released: %s (%d rows).", displayName(sysName, sysID), n), nil
}

// cmdSov implements /sov [systemID|list], reporting ownership:
//
//	/sov            ownership of the caller's current system
//	/sov <systemID> ownership of that system
//	/sov list       systems claimed by the caller's alliance
func cmdSov(ctx context.Context, st *connState, args []string) (string, error) {
	if len(args) >= 1 && strings.EqualFold(args[0], "list") {
		return cmdSovList(ctx, st)
	}

	sysID, msg, err := resolveSystemArg(ctx, st.sqldb, st.charID, args)
	if err != nil {
		return "", err
	}
	if msg != "" {
		return msg, nil
	}

	sysName, _ := evedb.GetSystemName(ctx, st.sqldb, sysID)
	sec, secOK, err := evedb.GetSystemSecurity(ctx, st.sqldb, sysID)
	if err != nil {
		return "", err
	}
	header := fmt.Sprintf("%s (systemID %d)", displayName(sysName, sysID), sysID)
	if secOK {
		header += fmt.Sprintf(", sec %.2f", sec)
	}

	rec, owned, err := evedb.GetSov(ctx, st.sqldb, sysID)
	if err != nil {
		return "", err
	}
	if !owned {
		if secOK && sec >= 0.0 {
			return fmt.Sprintf("%s — empire space, not claimable.", header), nil
		}
		return fmt.Sprintf("%s — unclaimed.", header), nil
	}

	allyName, _ := evedb.GetAllianceName(ctx, st.sqldb, rec.AllianceID)
	return fmt.Sprintf("%s — held by %s (allianceID %d), claimed by corpID %d, charID %d.",
		header, displayName(allyName, rec.AllianceID), rec.AllianceID, rec.ClaimingCorpID, rec.ClaimingCharID), nil
}

func cmdSovList(ctx context.Context, st *connState) (string, error) {
	if st.charID == 0 {
		return "/sov list requires an in-game character.", nil
	}
	_, _, allianceID, err := evedb.GetCharSovContext(ctx, st.sqldb, st.charID)
	if err != nil {
		return "", err
	}
	if allianceID == 0 {
		return "Your corporation is not in an alliance.", nil
	}
	ids, err := evedb.ListSovForAlliance(ctx, st.sqldb, allianceID)
	if err != nil {
		return "", err
	}
	allyName, _ := evedb.GetAllianceName(ctx, st.sqldb, allianceID)
	if len(ids) == 0 {
		return fmt.Sprintf("%s (allianceID %d) holds no systems.",
			displayName(allyName, allianceID), allianceID), nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "%s (allianceID %d) holds %d system(s):\n",
		displayName(allyName, allianceID), allianceID, len(ids))
	limit := len(ids)
	if limit > 30 {
		limit = 30
	}
	for _, id := range ids[:limit] {
		name, _ := evedb.GetSystemName(ctx, st.sqldb, id)
		fmt.Fprintf(&sb, "  %d  %s\n", id, name)
	}
	if len(ids) > 30 {
		fmt.Fprintf(&sb, "  … and %d more\n", len(ids)-30)
	}
	return sb.String(), nil
}

// displayName returns name if non-empty, otherwise a "#id" placeholder.
func displayName(name string, id int64) string {
	if name == "" {
		return fmt.Sprintf("#%d", id)
	}
	return name
}
