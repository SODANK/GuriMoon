package slash

// POS template commands (db/starbasetemplatedb.go):
//
//	/possave <towerID> <name> [fuelDays]         capture a player-built POS
//	/posspawn <name> <moonID> [online] [fuelDays] stamp it at a moon, owned by your corp
//	/postemplates                                 list them
//
// A template is captured, never authored: players build the layout in game
// within the tower's anchor radius, and /possave records it relative to the
// tower so it can land at any moon.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	evedb "gurimoon/db"
)

func cmdPosSave(ctx context.Context, st *connState, args []string) (string, error) {
	if len(args) < 2 {
		return "Usage: /possave <towerID> <name> [fuelDays]", nil
	}
	towerID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return "towerID must be a number.", nil
	}
	fuel := 0.0
	name := args[1]
	if len(args) >= 3 {
		if f, err := strconv.ParseFloat(args[len(args)-1], 64); err == nil {
			fuel = f
			name = strings.Join(args[1:len(args)-1], " ")
		} else {
			name = strings.Join(args[1:], " ")
		}
	}
	tpl, err := evedb.CaptureStarbaseTemplate(ctx, st.sqldb, towerID, name, fuel, st.charID)
	if err != nil {
		return fmt.Sprintf("Could not save template: %v", err), nil
	}
	return fmt.Sprintf("Template %q saved from tower %d (%s): %d structure(s), %d link(s), %.1f fuel day(s).",
		tpl.Name, towerID, tpl.TowerTypeName, len(tpl.Structures), len(tpl.Links), tpl.FuelDays), nil
}

func cmdPosSpawn(ctx context.Context, st *connState, args []string) (string, error) {
	if len(args) < 2 {
		return "Usage: /posspawn <name> <moonID> [online] [fuelDays]", nil
	}
	// The name may contain spaces: the moonID is the first numeric argument
	// after it.
	moonAt := -1
	for i := 1; i < len(args); i++ {
		if _, err := strconv.ParseInt(args[i], 10, 64); err == nil {
			moonAt = i
			break
		}
	}
	if moonAt < 0 {
		return "Usage: /posspawn <name> <moonID> [online] [fuelDays]", nil
	}
	name := strings.Join(args[:moonAt], " ")
	moonID, _ := strconv.ParseInt(args[moonAt], 10, 64)
	opts := evedb.StampOptions{FuelDays: -1}
	for _, a := range args[moonAt+1:] {
		if strings.EqualFold(a, "online") {
			opts.Online = true
		} else if f, err := strconv.ParseFloat(a, 64); err == nil && f >= 0 {
			opts.FuelDays = f
		}
	}
	if st.charID == 0 {
		return "Command requires an in-game character.", nil
	}
	tpl, found, err := evedb.GetStarbaseTemplate(ctx, st.sqldb, 0, name)
	if err != nil {
		return "", err
	}
	if !found {
		return fmt.Sprintf("No template named %q (see /postemplates).", name), nil
	}
	_, corpID, allianceID, err := evedb.GetCharSovContext(ctx, st.sqldb, st.charID)
	if err != nil {
		return "", err
	}
	res, err := evedb.StampStarbaseTemplate(ctx, st.sqldb, tpl, evedb.PlantRequest{
		CorpID: corpID, AllianceID: allianceID, MoonID: moonID, ActorCharID: st.charID,
	}, opts, slashLive(st))
	if err != nil {
		var refusal *evedb.StarbaseAnchorRefusal
		switch {
		case errors.As(err, &refusal):
			return refusal.Error(), nil
		case errors.Is(err, evedb.ErrMoonNotFound):
			return fmt.Sprintf("Moon %d not found in mapDenormalize.", moonID), nil
		}
		return fmt.Sprintf("Stamp failed: %v", err), nil
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Stamped %q at %s: tower %d, %d structure(s), %d link(s).",
		tpl.Name, res.Plant.MoonName, res.Plant.EntityID, len(res.Structures), res.Links)
	if res.FuelCycles > 0 {
		fmt.Fprintf(&sb, " Fuelled for %d cycle(s).", res.FuelCycles)
	}
	if res.Online {
		sb.WriteString(" Online.")
	}
	sb.WriteString(res.Live)
	return sb.String(), nil
}

func cmdPosTemplates(ctx context.Context, st *connState, _ []string) (string, error) {
	tpls, err := evedb.ListStarbaseTemplates(ctx, st.sqldb)
	if err != nil {
		return "", err
	}
	if len(tpls) == 0 {
		return "No POS templates. Build a POS in game, then /possave <towerID> <name>.", nil
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d POS template(s):\n", len(tpls))
	for _, t := range tpls {
		fmt.Fprintf(&sb, "  %q  tower=%s  structures=%d  links=%d  fuelDays=%.1f\n",
			t.Name, t.TowerTypeName, len(t.Structures), len(t.Links), t.FuelDays)
	}
	return sb.String(), nil
}
