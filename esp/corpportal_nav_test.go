package esp

import (
	"context"
	"strings"
	"testing"

	evedb "gurimoon/db"
)

func navKeys(site igbSite) string {
	var out []string
	for _, n := range site.Nav {
		out = append(out, n.Key)
	}
	return strings.Join(out, " ")
}

func TestViewerNavShowsARestrictedSectionToItsViewersOnly(t *testing.T) {
	saved := CorpPortalSections
	defer func() { CorpPortalSections = saved }()
	const chief = 7
	CorpPortalSections = []CorpPortalSection{
		{Key: "bulletins", Order: 2},
		{Key: "management", Order: 1, Visible: func(_ context.Context, corpID, charID int64) bool {
			return corpID == 98 && charID == chief
		}},
		{Key: "broken", Order: 3, Visible: func(context.Context, int64, int64) bool { panic("no database") }},
		{Key: "chair", Order: 2, Chartered: true},
	}
	corp := &evedb.ESPCorpFull{}
	corp.CorporationID = 98
	base := corpPortalSite(corp, false)
	if got, want := navKeys(base), "overview bulletins directory"; got != want {
		t.Errorf("menu with no viewer = %q, want %q", got, want)
	}
	for _, c := range []struct {
		name   string
		viewer int64
		want   string
	}{
		{"the chief, next to Overview", chief, "overview management bulletins directory"},
		{"a member", 8, "overview bulletins directory"},
		{"nobody", 0, "overview bulletins directory"},
	} {
		if got := navKeys(withViewerNav(context.Background(), base, corp, c.viewer)); got != c.want {
			t.Errorf("%s: menu = %q, want %q", c.name, got, c.want)
		}
	}
	if got := navKeys(corpPortalSite(corp, true)); strings.Contains(got, "management") {
		t.Errorf("the public profile shows a restricted section: %q", got)
	}
}
