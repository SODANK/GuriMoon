package npc

import (
	"gurimoon/db"
	"testing"
)

func TestEwarBanned(t *testing.T) {
	m := &NPCManager{}
	if m.ewarBanned("scram", 1.0) {
		t.Fatal("no rules loaded: nothing should be banned")
	}
	rules := []db.EwarBanRule{{Kind: "scram", SecMin: 0.9, SecMax: 1.0}}
	m.ewarBans.Store(&rules)

	cases := []struct {
		kind string
		sec  float64
		want bool
	}{
		{"scram", 1.0, true},
		{"scram", 0.9, true},
		{"scram", 0.86, true}, // displays as 0.9
		{"scram", 0.84, false}, // displays as 0.8
		{"scram", 0.5, false},
		{"scram", -0.4, false},
		{"web", 1.0, false},
	}
	for _, c := range cases {
		if got := m.ewarBanned(c.kind, c.sec); got != c.want {
			t.Errorf("ewarBanned(%q, %.2f) = %v, want %v", c.kind, c.sec, got, c.want)
		}
	}
}
