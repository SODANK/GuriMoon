package esp

// Science & Industry: the overview memo, the grouped job tabs and the group fragment. The page
// itself is handleFactories in factories.go; the aggregates are db/espsciindustrydb.go.

import (
	"context"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
)

const (
	// sciOverviewTTL is how long the overview is served from memory. It is a dozen aggregate
	// queries, so an open tab must not repeat them; "Refresh" bypasses it.
	sciOverviewTTL = 30 * time.Second

	sciGroupPageSize = 50
	sciGroupJobLimit = 200
)

// sciOverview returns the memoised overview, rebuilding it when stale or when
// the caller asked for a fresh one.
func (s *Server) sciOverview(ctx context.Context, force bool) (*evedb.ESPIndustryOverview, error) {
	s.sciMu.Lock()
	defer s.sciMu.Unlock()
	if !force && s.sciMemo != nil && time.Since(s.sciAt) < sciOverviewTTL {
		return s.sciMemo, nil
	}
	o, err := evedb.ESPIndustryOverviewLoad(ctx, s.db)
	if err != nil {
		return nil, err
	}
	s.sciMemo, s.sciAt = o, time.Now()
	return o, nil
}

// invalidateSciOverview drops the memo after a write, so the page an admin lands
// on after Rush shows what they just did.
func (s *Server) invalidateSciOverview() {
	s.sciMu.Lock()
	s.sciMemo = nil
	s.sciMu.Unlock()
}

// sciStatus keeps the grouped tabs to live jobs. Delivered history is large and has nothing to
// act on; the overview charts it instead.
func sciStatus(s string) string {
	switch s {
	case "running", "ready":
		return s
	}
	return "active"
}

// groupBaseFilter is the job filter a grouped tab and its fragments share. It
// reads FormValue so the same function serves a GET, a fragment fetch and the
// POST that rushes a group.
func groupBaseFilter(r *http.Request, view string) evedb.ESPJobFilter {
	f := evedb.ESPJobFilter{
		Status:   sciStatus(r.FormValue("status")),
		Search:   strings.TrimSpace(r.FormValue("q")),
		RegionID: parseInt64(r.FormValue("region")),
	}
	switch view {
	case "manufacturing":
		f.Class = "mfg"
	case "science":
		f.Class = "science"
		// The select only offers science activities, but a hand-edited URL
		// could ask for activity 1 here and get an empty tab.
		if a := parseInt64(r.FormValue("activity")); a != evedb.ESPActivityManufacturing {
			f.ActivityID = a
		}
	}
	return f
}

// sciScienceActivities is the activity choices for the Science tab: everything
// the blueprint window offers except manufacturing.
func sciScienceActivities() []activityChoice {
	var out []activityChoice
	for _, a := range blueprintActivityChoices() {
		if a.ID != evedb.ESPActivityManufacturing {
			out = append(out, a)
		}
	}
	return out
}

// fillGroupView adds a grouped tab's data to the page dict.
func (s *Server) fillGroupView(ctx context.Context, r *http.Request, view string, data map[string]any) error {
	q := r.URL.Query()
	by := evedb.ESPGroupBy(q.Get("by"))
	if view == "installers" {
		by = "installer"
	}
	sort := evedb.ESPGroupSort(q.Get("sort"))
	page := int(espQueryInt(q.Get("page"), 0))
	if page < 0 {
		page = 0
	}

	base := groupBaseFilter(r, view)
	res, err := evedb.ESPJobGroups(ctx, s.db, evedb.ESPGroupFilter{
		ESPJobFilter: base,
		By:           by,
		Sort:         sort,
		Offset:       page * sciGroupPageSize,
		Limit:        sciGroupPageSize,
	})
	if err != nil {
		return err
	}
	pages := int((res.TotalGroups + sciGroupPageSize - 1) / sciGroupPageSize)
	if pages < 1 {
		pages = 1
	}

	regions, _ := evedb.ESPListFactoryRegions(ctx, s.db)
	data["Regions"] = regions
	data["JobActs"] = sciScienceActivities()
	data["Groups"] = res.Groups
	data["TotalGroups"] = res.TotalGroups
	data["TotalJobs"] = res.TotalJobs
	data["By"] = by
	data["Sort"] = sort
	data["PageNo"] = page + 1
	data["PageCount"] = pages
	data["HasPrev"] = page > 0
	data["HasNext"] = page+1 < pages
	data["Prev"] = espPageQuery(q, page-1)
	data["Next"] = espPageQuery(q, page+1)
	// The filter the page is showing, as hidden fields: a group's "Rush all"
	// form must act on exactly the jobs the group row counted.
	filter := url.Values{
		"view":     {view},
		"status":   {base.Status},
		"q":        {base.Search},
		"region":   {strconv.FormatInt(base.RegionID, 10)},
		"activity": {strconv.FormatInt(base.ActivityID, 10)},
	}
	data["GroupFilter"] = filter
	// The group fragment's address up to the group's own id. template.URL because the query is
	// already encoded; escaping it again would turn every & into %26.
	fragment := url.Values{}
	for k, v := range filter {
		fragment[k] = v
	}
	fragment.Set("by", by)
	data["GroupBase"] = template.URL("/industry/factories/group?" + fragment.Encode())
	return nil
}

// handleFactoryGroup serves /industry/factories/group: the jobs under one group row, as an HTML
// fragment the Manufacturing, Science and Installers tabs load when a group is opened. The URL
// also works as a plain page link without JavaScript.
func (s *Server) handleFactoryGroup(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	view := r.FormValue("view")
	by := evedb.ESPGroupBy(r.FormValue("by"))
	jobs, err := evedb.ESPJobsInGroup(ctx, s.db, groupBaseFilter(r, view), by,
		parseInt64(r.FormValue("id")), parseInt64(r.FormValue("act")), sciGroupJobLimit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	tmpl, ok := s.tmpls["factories.html"]
	if !ok {
		http.Error(w, "template missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := tmpl.ExecuteTemplate(w, "groupjobs", map[string]any{
		"Jobs":      jobs,
		"Truncated": len(jobs) >= sciGroupJobLimit,
		"Limit":     sciGroupJobLimit,
		"By":        by,
		"Now":       time.Now().Unix(),
	}); err != nil {
		// Headers are gone by now; the fragment is simply cut short.
		return
	}
}
