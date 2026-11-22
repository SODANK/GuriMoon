package npc

import "testing"

func TestMaxOccupiedBelts(t *testing.T) {
	cases := map[int]int{0: 0, 1: 1, 2: 1, 3: 2, 4: 3, 5: 3, 6: 4, 8: 6, 10: 7, 20: 15}
	for n, want := range cases {
		if got := maxOccupiedBelts(n); got != want {
			t.Errorf("maxOccupiedBelts(%d) = %d, want %d", n, got, want)
		}
	}
}
