package esp

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
)

// Stargates panel (Landscape): builds a gate pair joining two existing systems and deletes
// pairs built here. Each gate is placed beside a planet or moon, on the side facing the other
// system (destiny.PlaceGate). Positions are always recomputed on the server from the form's
// anchors and offsets, never taken from the browser. Preview is a GET that writes nothing.
// See docs/stargates.md.

// StargatePairBuilder is set by the game server to write a pair and present both gates in any
// loaded system. Nil in a panel-only build, where Create is refused.
var StargatePairBuilder func(a, b evedb.StargateEnd) (evedb.StargatePair, error)

// StargatePairRemover is set by the game server to delete a custom pair and drop both balls.
// Nil in a panel-only build, where Delete is refused.
var StargatePairRemover func(gateID int64) (evedb.StargatePair, error)

// lightYearM converts galactic distances for the pair summary.
const lightYearM = 9_460_730_472_580_800.0

// gateAnchorOption is one planet or moon a gate may be placed by.
type gateAnchorOption struct {
	ItemID   int64
	Name     string
	Group    string
	Selected bool
}

// gateTypeOption is one stargate type choice.
type gateTypeOption struct {
	TypeID   int64
	TypeName string
	Selected bool
}

// gateSideView is one side of the create form. Prefix ("a" or "b") names its form fields.
type gateSideView struct {
	Prefix   string
	Info     evedb.StargateSystemInfo
	Types    []gateTypeOption
	Anchors  []gateAnchorOption
	TypeID   int64
	OffsetAU float64
	Pos      destiny.Vec3
	Problem  string // non-empty when the computed position fails the clearance check
	Placed   bool   // an anchor is chosen and Pos is valid
}

// gateSide is the parsed form input for one side.
type gateSide struct {
	systemID, anchorID, typeID int64
	offsetAU                   float64
}

func parseGateSide(v url.Values, p string) gateSide {
	g := gateSide{offsetAU: destiny.GateDefaultOffsetM / metersPerAU}
	g.systemID, _ = strconv.ParseInt(v.Get(p), 10, 64)
	g.anchorID, _ = strconv.ParseInt(v.Get(p+"Anchor"), 10, 64)
	g.typeID, _ = strconv.ParseInt(v.Get(p+"Type"), 10, 64)
	if f, err := strconv.ParseFloat(strings.TrimSpace(v.Get(p+"Offset")), 64); err == nil && f > 0 {
		g.offsetAU = f
	}
	return g
}

// gatePlan is a fully resolved pair, ready to show or to write.
type gatePlan struct {
	A, B          gateSideView
	DistanceLY    float64
	AlreadyJoined bool
	Ready         bool // both sides placed, clear, and the systems are not yet joined
}

// planGatePair loads both systems, fills the anchor and type choices, and places each gate whose
// anchor is chosen. It reports problems in the plan rather than as errors, so the form can
// show them next to the side they belong to.
func (s *Server) planGatePair(ctx context.Context, a, b gateSide) (*gatePlan, error) {
	if a.systemID == b.systemID {
		return nil, fmt.Errorf("a gate cannot join a system to itself")
	}
	plan := &gatePlan{}
	var cels [2][]evedb.CelestialEntity
	sides := [2]*gateSideView{&plan.A, &plan.B}
	in := [2]gateSide{a, b}
	for i, side := range in {
		info, ok, err := evedb.GetStargateSystemInfo(ctx, s.db, side.systemID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("unknown solar system %d", side.systemID)
		}
		c, err := evedb.GetSystemCelestials(ctx, s.db, side.systemID)
		if err != nil {
			return nil, err
		}
		cels[i] = c
		v := sides[i]
		v.Prefix = string(rune('a' + i))
		v.Info = info
		v.OffsetAU = side.offsetAU
		v.TypeID = side.typeID
		if v.TypeID == 0 {
			if v.TypeID, err = evedb.GateTypeForRegion(ctx, s.db, info.RegionID); err != nil {
				return nil, err
			}
		}
		for _, cel := range c {
			if cel.GroupID == 7 || cel.GroupID == 8 {
				v.Anchors = append(v.Anchors, gateAnchorOption{
					ItemID: cel.ItemID, Name: cel.Name, Group: celestialGroupNames[cel.GroupID],
					Selected: cel.ItemID == side.anchorID,
				})
			}
		}
	}
	galA := destiny.Vec3{X: plan.A.Info.X, Y: plan.A.Info.Y, Z: plan.A.Info.Z}
	galB := destiny.Vec3{X: plan.B.Info.X, Y: plan.B.Info.Y, Z: plan.B.Info.Z}
	plan.DistanceLY = galB.Sub(galA).Len() / lightYearM

	joined, err := evedb.SystemsJoined(ctx, s.db, a.systemID, b.systemID)
	if err != nil {
		return nil, err
	}
	plan.AlreadyJoined = joined

	gal := [2][2]destiny.Vec3{{galA, galB}, {galB, galA}}
	for i, side := range in {
		v := sides[i]
		if side.anchorID == 0 {
			continue
		}
		var anchor *evedb.CelestialEntity
		obstacles := make([]destiny.GateObstacle, 0, len(cels[i]))
		for j := range cels[i] {
			c := &cels[i][j]
			if c.ItemID == side.anchorID && (c.GroupID == 7 || c.GroupID == 8) {
				anchor = c
			}
			obstacles = append(obstacles, destiny.GateObstacle{
				ID: c.ItemID, Name: c.Name, Pos: destiny.Vec3{X: c.X, Y: c.Y, Z: c.Z}, Radius: c.Radius,
			})
		}
		if anchor == nil {
			v.Problem = "anchor is not a planet or moon of this system"
			continue
		}
		v.Pos = destiny.PlaceGate(destiny.Vec3{X: anchor.X, Y: anchor.Y, Z: anchor.Z}, anchor.Radius,
			gal[i][0], gal[i][1], side.offsetAU*metersPerAU)
		v.Placed = true
		if o, gap, bad := destiny.GateClearance(v.Pos, 5000, obstacles, destiny.GateMinClearanceM); bad {
			v.Problem = fmt.Sprintf("too close to %s (%.0f km gap, need %.0f km)",
				o.Name, gap/1000, destiny.GateMinClearanceM/1000)
		}
	}
	plan.Ready = plan.A.Placed && plan.B.Placed && plan.A.Problem == "" && plan.B.Problem == "" && !plan.AlreadyJoined
	return plan, nil
}

// handleStargates renders the list and the create form, and dispatches the POST actions.
func (s *Server) handleStargates(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		s.handleStargatePost(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	fail := func(err error) {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "stargates"})
	}

	pairs, err := evedb.ListCustomStargates(ctx, s.db, 0)
	if err != nil {
		fail(err)
		return
	}
	q := r.URL.Query()
	data := map[string]any{
		"Page":      "stargates",
		"Pairs":     pairs,
		"CanBuild":  StargatePairBuilder != nil,
		"CanRemove": StargatePairRemover != nil,
		"Msg":       q.Get("msg"),
		"Err":       q.Get("err"),
	}
	a, b := parseGateSide(q, "a"), parseGateSide(q, "b")
	if a.systemID != 0 && b.systemID != 0 {
		plan, err := s.planGatePair(ctx, a, b)
		if err != nil {
			data["Err"] = err.Error()
		} else {
			types, err := evedb.ListStargateTypes(ctx, s.db)
			if err != nil {
				fail(err)
				return
			}
			for _, side := range []*gateSideView{&plan.A, &plan.B} {
				for _, t := range types {
					side.Types = append(side.Types, gateTypeOption{
						TypeID: t.TypeID, TypeName: t.TypeName, Selected: t.TypeID == side.TypeID,
					})
				}
			}
			data["Plan"] = plan
		}
	}
	s.render(w, "stargates.html", data)
}

// handleStargatePost runs Create and Delete. Create re-plans from the submitted anchors so the
// written positions are the ones the server checked.
func (s *Server) handleStargatePost(w http.ResponseWriter, r *http.Request) {
	back := func(kind, msg string, keep url.Values) {
		v := url.Values{}
		for k, vals := range keep {
			v[k] = vals
		}
		v.Set(kind, msg)
		http.Redirect(w, r, "/stargates?"+v.Encode(), http.StatusSeeOther)
	}
	if err := r.ParseForm(); err != nil {
		back("err", "bad form: "+err.Error(), nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	switch r.FormValue("action") {
	case "create":
		keep := url.Values{}
		for _, k := range []string{"a", "aAnchor", "aType", "aOffset", "b", "bAnchor", "bType", "bOffset"} {
			keep.Set(k, r.FormValue(k))
		}
		if StargatePairBuilder == nil {
			back("err", "building gates needs the game server", keep)
			return
		}
		a, b := parseGateSide(r.PostForm, "a"), parseGateSide(r.PostForm, "b")
		plan, err := s.planGatePair(ctx, a, b)
		if err != nil {
			back("err", err.Error(), keep)
			return
		}
		if !plan.Ready {
			back("err", "the pair is not ready: pick both anchors and clear every problem shown", keep)
			return
		}
		pair, err := StargatePairBuilder(
			evedb.StargateEnd{SolarSystemID: a.systemID, AnchorID: a.anchorID, TypeID: plan.A.TypeID,
				X: plan.A.Pos.X, Y: plan.A.Pos.Y, Z: plan.A.Pos.Z},
			evedb.StargateEnd{SolarSystemID: b.systemID, AnchorID: b.anchorID, TypeID: plan.B.TypeID,
				X: plan.B.Pos.X, Y: plan.B.Pos.Y, Z: plan.B.Pos.Z},
		)
		if err != nil {
			back("err", err.Error(), keep)
			return
		}
		back("msg", fmt.Sprintf("Built %s ↔ %s (gates %d and %d).",
			plan.A.Info.Name, plan.B.Info.Name, pair.GateA, pair.GateB), nil)
	case "delete":
		if StargatePairRemover == nil {
			back("err", "deleting gates needs the game server", nil)
			return
		}
		gateID, _ := strconv.ParseInt(r.FormValue("gateID"), 10, 64)
		if gateID == 0 {
			back("err", "missing gateID", nil)
			return
		}
		pair, err := StargatePairRemover(gateID)
		if err != nil {
			back("err", err.Error(), nil)
			return
		}
		back("msg", fmt.Sprintf("Deleted gates %d and %d.", pair.GateA, pair.GateB), nil)
	default:
		back("err", "unknown action", nil)
	}
}
