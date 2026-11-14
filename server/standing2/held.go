package standing2

import (
	"gurimoon/marshal"
	"gurimoon/syncx"
)

// heldNPCStandings records, per character, the NPC owners whose standing row the client
// received from its last GetMyStandings. The client fetches standings once per session and
// OnStandingsModified can only move a row it already holds: for an absent row the client
// stores a bare list that every reader then fails on (standingsvc.py).
var heldNPCStandings syncx.Map[int64, map[int64]struct{}] // charID int64 -> map[int64]struct{}

// recordHeldNPCStandings replaces the character's set from an npcCharStandings rowset, whose
// first column is fromID.
func recordHeldNPCStandings(charID int64, rowset marshal.PyRep) {
	if charID == 0 {
		return
	}
	heldNPCStandings.Store(charID, rowsetFirstColumn(rowset))
}

// ClientHoldsNPCStanding reports whether the character's client holds a standing row from
// fromID, so that a standing push can move it.
func ClientHoldsNPCStanding(charID, fromID int64) bool {
	v, ok := heldNPCStandings.Load(charID)
	if !ok {
		return false
	}
	_, held := v[fromID]
	return held
}

// rowsetFirstColumn collects the integer first column of a util.Rowset's lines.
func rowsetFirstColumn(rowset marshal.PyRep) map[int64]struct{} {
	out := map[int64]struct{}{}
	obj, ok := rowset.(marshal.PyObject)
	if !ok {
		return out
	}
	args, ok := obj.Args.(marshal.PyDict)
	if !ok {
		return out
	}
	for _, e := range args.Items {
		if k, ok := e.Key.(marshal.PyString); !ok || k.Value != "lines" {
			continue
		}
		lines, ok := e.Value.(marshal.PyList)
		if !ok {
			return out
		}
		for _, line := range lines.Items {
			var cells []marshal.PyRep
			switch l := line.(type) {
			case marshal.PyList:
				cells = l.Items
			case marshal.PyTuple:
				cells = l.Items
			}
			if len(cells) == 0 {
				continue
			}
			if id, ok := cells[0].(marshal.PyInt); ok {
				out[int64(id.Value)] = struct{}{}
			}
		}
	}
	return out
}
