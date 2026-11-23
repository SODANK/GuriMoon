package main

import (
	"testing"

	"gurimoon/corp"
	evedb "gurimoon/db"
)

func TestRestrictToGrantable(t *testing.T) {
	i64 := func(v uint64) *int64 { n := int64(v); return &n }
	grantor := memberMasks{
		grant:  [4]uint64{corp.RoleTrader | corp.RoleDirector, corp.RoleHangarCanTake2, corp.RoleHangarCanTake3, 0},
		baseID: 60,
	}
	grantee := memberMasks{
		held:   [4]uint64{corp.RoleFactoryManager, 0, 0, 0},
		baseID: 60,
	}
	cases := []struct {
		name string
		u    evedb.CorpMemberUpdate
		ok   bool
	}{
		{"grant Trader", evedb.CorpMemberUpdate{Roles: i64(corp.RoleFactoryManager | corp.RoleTrader)}, true},
		{"unchanged roles pass", evedb.CorpMemberUpdate{Roles: i64(corp.RoleFactoryManager)}, true},
		{"revoke a non-grantable role", evedb.CorpMemberUpdate{Roles: i64(0)}, false},
		{"Director is never grantable", evedb.CorpMemberUpdate{Roles: i64(corp.RoleFactoryManager | corp.RoleDirector)}, false},
		{"take 2 at HQ", evedb.CorpMemberUpdate{RolesAtHQ: i64(corp.RoleHangarCanTake2)}, true},
		{"take 2 elsewhere is not grantable", evedb.CorpMemberUpdate{RolesAtOther: i64(corp.RoleHangarCanTake2)}, false},
		{"take 3 at a shared Base", evedb.CorpMemberUpdate{RolesAtBase: i64(corp.RoleHangarCanTake3)}, true},
		{"grantable masks are a Director's", evedb.CorpMemberUpdate{GrantableRoles: i64(corp.RoleTrader)}, false},
		{"titles are a Director's", evedb.CorpMemberUpdate{TitleMask: i64(1)}, false},
		{"the Base is a Director's", evedb.CorpMemberUpdate{BaseID: func() *int64 { n := int64(61); return &n }()}, false},
	}
	for _, c := range cases {
		u := c.u
		err := restrictToGrantable(grantor, grantee, &u)
		if (err == nil) != c.ok {
			t.Errorf("%s: err = %v, want ok=%v", c.name, err, c.ok)
		}
	}
	// Base roles are grantable only between members sharing a Base.
	elsewhere := grantee
	elsewhere.baseID = 61
	if err := restrictToGrantable(grantor, elsewhere, &evedb.CorpMemberUpdate{RolesAtBase: i64(corp.RoleHangarCanTake3)}); err == nil {
		t.Error("granted a Base role to a member of another Base")
	}
}
