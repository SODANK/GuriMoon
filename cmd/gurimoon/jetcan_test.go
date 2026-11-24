package main

import (
	"testing"
	"time"

	evedb "gurimoon/db"
	"gurimoon/marshal"
	"gurimoon/server"
)

func kwargs(kv ...any) *marshal.PyDict {
	d := &marshal.PyDict{}
	for i := 0; i+1 < len(kv); i += 2 {
		d.Items = append(d.Items, marshal.DictEntry{
			Key: marshal.PyString{Value: kv[i].(string)}, Value: marshal.PyInt{Value: uint64(kv[i+1].(int))},
		})
	}
	return d
}

func args(vals ...marshal.PyRep) marshal.PyTuple { return marshal.PyTuple{Items: vals} }

func TestInvAddArgs(t *testing.T) {
	cases := []struct {
		name      string
		cs        server.CallStream
		qty, flag int64
	}{
		{"positional fit", server.CallStream{ArgTuple: args(marshal.PyInt{Value: 1}, marshal.PyNone{}, marshal.PyInt{Value: 27})}, 0, 27},
		{"positional qty", server.CallStream{ArgTuple: args(marshal.PyInt{Value: 1}, marshal.PyInt{Value: 40}, marshal.PyInt{Value: 5})}, 40, 5},
		// A drag out of a can: (itemID, sourceID) with keywords. The can's itemID must not
		// read as a quantity, and the keyword flag must not be lost.
		{"source + kwargs", server.CallStream{
			ArgTuple: args(marshal.PyInt{Value: 1}, marshal.PyInt{Value: 140000123}),
			ArgDict:  kwargs("qty", 15, "flag", 5)}, 15, 5},
		{"source + flag only", server.CallStream{
			ArgTuple: args(marshal.PyInt{Value: 1}, marshal.PyInt{Value: 140000123}),
			ArgDict:  kwargs("flag", 5)}, 0, 5},
		{"no kwargs, two args", server.CallStream{ArgTuple: args(marshal.PyInt{Value: 1}, marshal.PyInt{Value: 7})}, 7, 0},
	}
	for _, tc := range cases {
		qty, flag := invAddArgs(&tc.cs)
		if qty != tc.qty || flag != tc.flag {
			t.Errorf("%s: got qty=%d flag=%d, want qty=%d flag=%d", tc.name, qty, flag, tc.qty, tc.flag)
		}
	}
}

func TestLootExpiredPerKind(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	cut := evedb.LootCutoffs{
		Dropped:  now.Add(-2 * time.Hour).Unix(),
		Launched: now.Add(-48 * time.Hour).Unix(),
	}
	const jet, launched = 900001, 900002
	launchedCans.Store(launched, struct{}{})
	defer launchedCans.Delete(launched)

	threeHours := now.Add(-3 * time.Hour)
	if !lootExpired(jet, threeHours, cut) {
		t.Error("a 3h-old jetcan should have expired")
	}
	if lootExpired(launched, threeHours, cut) {
		t.Error("a 3h-old launched container should still be in space")
	}
	if !lootExpired(launched, now.Add(-49*time.Hour), cut) {
		t.Error("a 49h-old launched container should have expired")
	}
	if lootExpired(jet, now.Add(-time.Hour), cut) {
		t.Error("a 1h-old jetcan should still be in space")
	}
}

func TestLootTakeIsTheftFleetAndCorp(t *testing.T) {
	owner := lootOwner{CharID: 90000001, CorpID: 98000001}
	if lootTakeIsTheft(90000002, 98000002, owner, true) {
		t.Error("a fleetmate's take is not theft")
	}
	if lootTakeIsTheft(90000002, 98000001, owner, false) {
		t.Error("a corpmate's take is not theft")
	}
	if !lootTakeIsTheft(90000002, 98000002, owner, false) {
		t.Error("a stranger's take is theft")
	}
	if lootTakeIsTheft(90000002, 98000002, lootOwner{}, false) {
		t.Error("an NPC can is free to loot")
	}
}
