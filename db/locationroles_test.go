package db

import "testing"

func TestRolesAtStation(t *testing.T) {
	const (
		all   = 1 << 0
		hq    = 1 << 1
		base  = 1 << 2
		other = 1 << 3
		glob  = 1 << 4
	)
	info := &CorpMemberInfo{Corprole: glob, RolesAtAll: all, RolesAtHQ: hq, RolesAtBase: base, RolesAtOther: other, BaseID: 20}
	cases := []struct {
		name    string
		station int64
		on      bool
		want    uint64
	}{
		{"off: every location role holds everywhere", 30, false, glob | all | hq | base | other},
		{"off with no station", 0, false, glob | all | hq | base | other},
		{"on at HQ", 10, true, glob | all | hq},
		{"on at Base", 20, true, glob | all | base},
		{"on elsewhere", 30, true, glob | all | other},
		{"on with no station counts as Other", 0, true, glob | all | other},
	}
	for _, c := range cases {
		if got := RolesAtStation(info, c.station, 10, c.on); got != c.want {
			t.Errorf("%s: got %#x, want %#x", c.name, got, c.want)
		}
	}
	if RolesAtStation(nil, 10, 10, true) != 0 {
		t.Error("nil info should hold nothing")
	}
}
