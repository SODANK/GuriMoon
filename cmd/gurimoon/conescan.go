package main

// Directional scanner: remote-park ConeScan(angle, rangeM, dx, dy, dz) from ui/inflight/scanner.py.
// The angle is the full cone in radians, the direction is the camera's forward vector, the range
// is metres (the client clamps it to 14.35 AU; the server sets no cap). A result the client cannot
// name wedges the d-scan window, so handleConeScan drops it.
// See docs/cmd-misc-notes.md#scanner-name-wedge.

import (
	"context"
	"database/sql"
	"log"
	"math"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
)

var coneScanCols = []string{"id", "typeID", "groupID"}

// coneScanCosHalf turns the client's full cone angle into the cosine of its half-angle; a full
// circle or wider disables the angular test. Degrees are also accepted: a value past 2*pi can only
// be degrees.
func coneScanCosHalf(angle float64) float64 {
	if angle > 2*math.Pi+1e-6 {
		angle = angle * math.Pi / 180
	}
	if angle >= 2*math.Pi-1e-6 {
		return -2
	}
	return math.Cos(angle / 2)
}

func handleConeScan(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB, ds *destinyConn) error {
	if ds.entity == nil || ds.solarSystem == nil || len(cs.ArgTuple.Items) < 5 {
		return server.SendCallRsp(c, pkt, accountID, marshal.PyList{})
	}
	angle := destinyExtractFloat(cs.ArgTuple.Items[0])
	rng := destinyExtractFloat(cs.ArgTuple.Items[1])
	x, y, z := destinyExtractXYZ(cs.ArgTuple.Items, 2)
	dir := destiny.Vec3{X: x, Y: y, Z: z}
	if dir.Len() < 1e-9 {
		dir = destiny.Vec3{Z: 1}
	}
	dir = dir.Normalize()
	if rng <= 0 {
		rng = 1
	}

	balls := ds.solarSystem.ConeScan(ds.entity.Pos(), dir, coneScanCosHalf(angle), rng, ds.entity.ID)
	balls = nameableScanBalls(addr, sqldb, balls)
	items := make([]marshal.PyRep, 0, len(balls))
	for _, b := range balls {
		items = append(items, keyValRep(coneScanCols, []marshal.PyRep{
			marshal.PyInt{Value: uint64(b.ID)},
			marshal.PyInt{Value: uint64(b.TypeID)},
			marshal.PyInt{Value: uint64(b.GroupID)},
		}))
	}
	log.Printf("[%s] → destiny.ConeScan angle=%.3f range=%.0f → %d ball(s)", addr, angle, rng, len(items))
	return server.SendCallRsp(c, pkt, accountID, marshal.PyList{Items: items})
}

// nameableScanBalls drops every non-asteroid result GetMultiLocationsEx
// cannot answer. On a lookup error it answers nothing rather than risk a
// result the client would choke on.
func nameableScanBalls(addr string, sqldb *sql.DB, balls []destiny.ScanBall) []destiny.ScanBall {
	if sqldb == nil || len(balls) == 0 {
		return balls
	}
	ids := make([]int64, 0, len(balls))
	for _, b := range balls {
		if !b.Asteroid {
			ids = append(ids, int64(b.ID))
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	known, err := evedb.LocatableIDs(ctx, sqldb, ids)
	if err != nil {
		log.Printf("[%s] ConeScan: name lookup: %v", addr, err)
		return nil
	}
	out := balls[:0]
	dropped := 0
	for _, b := range balls {
		if b.Asteroid || known[int64(b.ID)] {
			out = append(out, b)
		} else {
			dropped++
		}
	}
	if dropped > 0 {
		log.Printf("[%s] ConeScan: dropped %d result(s) with no nameable location", addr, dropped)
	}
	return out
}

// keyValRep builds a util.KeyVal the client attribute-reads.
func keyValRep(cols []string, vals []marshal.PyRep) marshal.PyRep {
	items := make([]marshal.DictEntry, len(cols))
	for i, col := range cols {
		items[i] = marshal.DictEntry{Key: marshal.PyString{Value: col}, Value: vals[i]}
	}
	return marshal.PyObject{Type: "util.KeyVal", Args: marshal.PyDict{Items: items}}
}
