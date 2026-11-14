package main

// Corporation share pushes, the client half of db/corpshares.go.
//
// The client routes OnShareChange(shareholderID, corporationID, change) by holder: wallet.py
// sends it to the personal wallet for the session character and to the corp wallet for the
// session corporation. A transfer is therefore two pushes, and a corporation holder is told
// through every online member.
//
// Open votes are re-weighed after anything that moves a share or a CEO; options whose tally
// moved go out as OnCorporationVoteCaseOptionChanged.

import (
	"context"
	"database/sql"
	"log"

	evedb "gurimoon/db"
	"gurimoon/esp"
	"gurimoon/marshal"
	"gurimoon/server"
)

// shareChangeDict builds one of the three forms bco_base.GetAddRemoveFromChange recognises:
// ADD (every old value None, every GetSharesByShareholder column present), REMOVE (every new
// value None), or an update of the shares column alone.
func shareChangeDict(corpID, oldN, newN int64) marshal.PyDict {
	none := marshal.PyNone{}
	corp := marshal.PyInt{Value: uint64(corpID)}
	switch {
	case oldN == 0:
		return marshal.PyDict{Items: []marshal.DictEntry{
			chg("corporationID", none, corp),
			chg("shares", none, marshal.PyInt{Value: uint64(newN)}),
		}}
	case newN == 0:
		return marshal.PyDict{Items: []marshal.DictEntry{
			chg("corporationID", corp, none),
			chg("shares", marshal.PyInt{Value: uint64(oldN)}, none),
		}}
	}
	return marshal.PyDict{Items: []marshal.DictEntry{
		chg("shares", marshal.PyInt{Value: uint64(oldN)}, marshal.PyInt{Value: uint64(newN)}),
	}}
}

// pushShareChange tells a holder that its holding of corpID moved.  A holder
// is either a pilot or a corporation and the two ID bands never meet, so both
// deliveries are attempted and at most one finds anybody.
func pushShareChange(sessReg *esp.SessionRegistry, holderID, corpID, oldN, newN int64) {
	if oldN == newN {
		return
	}
	args := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyInt{Value: uint64(holderID)},
		marshal.PyInt{Value: uint64(corpID)},
		shareChangeDict(corpID, oldN, newN),
	}}
	server.NotifyCharacter(sessReg, holderID, "OnShareChange", "clientID", args)
	notifyCorp(sessReg, holderID, "OnShareChange", args)
}

// announceShareChange pushes every holder delta, the corporation's issued
// total when it moved (the corp window's home tab prints it), and re-weighs
// the corporation's open votes.
func announceShareChange(ctx context.Context, sqldb *sql.DB, sessReg *esp.SessionRegistry,
	corpID int64, deltas []evedb.ShareDelta, oldTotal, newTotal int64) {
	for _, d := range deltas {
		pushShareChange(sessReg, d.HolderID, corpID, d.Old, d.New)
	}
	if oldTotal != newTotal {
		onCorporationChanged(sessReg, corpID, []marshal.DictEntry{
			chg("shares", marshal.PyInt{Value: uint64(oldTotal)}, marshal.PyInt{Value: uint64(newTotal)}),
		})
	}
	retallyShareVotes(ctx, sqldb, sessReg, corpID)
}

// retallyShareVotes re-weighs corpID's open votes and pushes the options
// whose share count moved.
func retallyShareVotes(ctx context.Context, sqldb *sql.DB, sessReg *esp.SessionRegistry, corpID int64) {
	changes, err := evedb.RetallyOpenVotes(ctx, sqldb, corpID, int64(server.Win32TimeNow()))
	if err != nil {
		log.Printf("[shares] retally corp=%d: %v", corpID, err)
	}
	for _, c := range changes {
		notifyCorp(sessReg, corpID, "OnCorporationVoteCaseOptionChanged", marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(corpID)},
			marshal.PyInt{Value: uint64(c.VoteCaseID)},
			marshal.PyInt{Value: uint64(c.OptionID)},
			marshal.PyDict{Items: []marshal.DictEntry{
				chg("votesFor", marshal.PyInt{Value: uint64(c.Old)}, marshal.PyInt{Value: uint64(c.New)}),
			}},
		}})
	}
}

// retallyAfterCEOChange re-weighs every vote a CEO change can move: the
// corporation's own (its treasury is voted by its CEO) and every corporation
// it holds a stake in.
func retallyAfterCEOChange(ctx context.Context, sqldb *sql.DB, sessReg *esp.SessionRegistry, corpID int64) {
	corps := []int64{corpID}
	if rows, err := sqldb.QueryContext(ctx,
		`SELECT corporationID FROM crpcharshares WHERE characterID = ? AND corporationID <> ?`,
		corpID, corpID); err == nil {
		for rows.Next() {
			var id int64
			if rows.Scan(&id) == nil {
				corps = append(corps, id)
			}
		}
		rows.Close()
	}
	for _, id := range corps {
		retallyShareVotes(ctx, sqldb, sessReg, id)
	}
}
