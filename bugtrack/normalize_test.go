package bugtrack

import "testing"

func TestNormalizeCollapsesVaryingParts(t *testing.T) {
	same := [][2]string{
		{`2026/11/21 10:00:01 [127.0.0.1:5123] unhandled method "GetFoo" on 0xc000123abc`,
			`2026/11/22 11:30:59 [10.0.0.7:61000] unhandled method "GetBar" on 0xc0009f0010`},
		{`panic at entity 1234 hash 9f86d081884c7d65 failed`,
			`panic at entity 98 hash 3c59dc048e885024 failed`},
		{`error loading 'Some Pilot' corp 7`, `error loading 'Another Name' corp 12345`},
	}
	for _, p := range same {
		a, b := normalize(p[0]), normalize(p[1])
		if a != b {
			t.Errorf("want one signature:\n  %q -> %q\n  %q -> %q", p[0], a, p[1], b)
		}
	}
	// Words made of hex letters are not IDs.
	if got := normalize("failed to decode facade"); got != "failed to decode facade" {
		t.Errorf("plain words changed: %q", got)
	}
	// Different messages stay different.
	if normalize("error: dock failed") == normalize("error: undock failed") {
		t.Error("distinct messages collapsed")
	}
	// An unmatched quote is left alone.
	if got := normalize(`can't find 12`); got != `can't find #` {
		t.Errorf("unmatched quote: %q", got)
	}
}
