package db

import (
	"testing"

	"gurimoon/dogma"
)

func TestImplantMultsFoldWarpCapNeed(t *testing.T) {
	if !IdentityImplantMults().IsIdentity() {
		t.Fatal("identity mults must report identity")
	}
	m := ImplantMultsFrom([]dogma.ImplantTag{{
		Slot:    8,
		Effects: []dogma.ImplantEffect{{Channel: dogma.ICWarpCapNeed, Pct: -6}},
	}})
	if got := orOne(m.WarpCapNeed); got < 0.9399 || got > 0.9401 {
		t.Fatalf("WarpCapNeed = %v, want 0.94", got)
	}
	if m.IsIdentity() {
		t.Fatal("a warp-cap hardwiring must not read as identity")
	}
}
