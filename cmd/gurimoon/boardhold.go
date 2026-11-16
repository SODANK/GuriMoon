package main

// Board reply hold. The hangar removes the old hull, then FitHardpoints asks
// godma for the new ship, and godma GetItem polls a running prime once a second
// (station base.py, godma.py). Board sends its shipid SCN first, so godma is
// mid-prime when the reply lands and the hangar shows no ship for a second. The
// reply waits until the prime's ShipGetInfo is answered, plus a grace for the
// calls Prime sends with it, so godma is ready when the hangar asks.

import (
	"sync"
	"time"

	evenet "gurimoon/net"
	"gurimoon/safego"
	"gurimoon/syncx"
)

const (
	// boardPrimeGrace runs from the ShipGetInfo answer to the Board reply. It
	// covers the inventory calls godma Prime sends alongside ShipGetInfo.
	boardPrimeGrace = 150 * time.Millisecond
	// boardHoldMax caps the hold when no ShipGetInfo comes. Keep it under the
	// client's one-second prime poll.
	boardHoldMax = 900 * time.Millisecond
)

// pendingBoard is one held Board reply. fire sends it at most once.
type pendingBoard struct {
	once  sync.Once
	reply func()
}

// boardHolds is the held Board reply per connection.
var boardHolds syncx.Map[*evenet.Conn, *pendingBoard]

func (pb *pendingBoard) fire(c *evenet.Conn) {
	pb.once.Do(func() {
		boardHolds.CompareAndDelete(c, pb)
		if !c.IsClosed() {
			pb.reply()
		}
	})
}

// holdBoardReply sends reply boardPrimeGrace after boardPrimeServed(c), or at
// boardHoldMax. A reply still held for c goes out first.
func holdBoardReply(c *evenet.Conn, reply func()) {
	pb := &pendingBoard{reply: reply}
	if prev, ok := boardHolds.Swap(c, pb); ok {
		prev.fire(c)
	}
	boardHoldAfter(boardHoldMax, func() { pb.fire(c) })
}

// boardPrimeServed marks godma's ShipGetInfo answered on c and releases a held
// Board reply after boardPrimeGrace. Call it wherever ShipGetInfo is answered.
func boardPrimeServed(c *evenet.Conn) {
	if pb, ok := boardHolds.Load(c); ok {
		boardHoldAfter(boardPrimeGrace, func() { pb.fire(c) })
	}
}

func boardHoldAfter(d time.Duration, fn func()) {
	time.AfterFunc(d, func() {
		defer safego.Recover("boardhold")
		fn()
	})
}
