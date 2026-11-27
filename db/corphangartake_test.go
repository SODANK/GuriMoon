package db

import (
	"testing"

	"gurimoon/corp"
)

func TestCorpHangarTakeRoleAllowed(t *testing.T) {
	cases := []struct {
		name  string
		roles uint64
		ceo   bool
		flag  int64
		want  bool
	}{
		{"take on division 1", corp.RoleHangarCanTake1, false, FlagCorpHangarDiv1, true},
		{"take on division 4", corp.RoleHangarCanTake4, false, FlagCorpSAGFirst + 2, true},
		{"query only", corp.RoleHangarCanQuery4, false, FlagCorpSAGFirst + 2, false},
		{"take on another division", corp.RoleHangarCanTake1, false, FlagCorpSAGFirst + 2, false},
		{"director", corp.RoleDirector, false, FlagCorpSAGLast, true},
		{"ceo", 0, true, FlagCorpSAGFirst, true},
		{"not a division", corp.RoleDirector, true, 5, false},
	}
	for _, c := range cases {
		if got := CorpHangarTakeRoleAllowed(c.roles, c.ceo, c.flag); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestReprocessOutputSlot(t *testing.T) {
	const char, station, corpID, office = 90000001, 60000004, 98000001, 1500
	own := &HangarItemInfo{OwnerID: char, LocationID: station, Flag: 5}
	if o, l, f := ReprocessOutputSlot(own, char, station); o != char || l != station || f != FlagCorpHangarDiv1 {
		t.Errorf("own ore: got %d/%d/%d", o, l, f)
	}
	corpStack := &HangarItemInfo{OwnerID: corpID, LocationID: office, Flag: FlagCorpSAGFirst + 2}
	if o, l, f := ReprocessOutputSlot(corpStack, char, station); o != corpID || l != office || f != FlagCorpSAGFirst+2 {
		t.Errorf("corp ore: got %d/%d/%d", o, l, f)
	}
}
