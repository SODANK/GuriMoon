package charmgr

// Bounty portraits, drawn by the players' own clients. The in-space billboards
// fetch a Most Wanted face over HTTP (billboardMgr.py, img.eve.is/serv.asp), served
// from data/portraits; the server has no face renderer. The optional
// "bountyportraits" client patch (cmd/clientpatch) has a docked client ask
// GetPortraitJobs for pilots the store lacks, draw each with photosvc.GetPortrait
// and return the PNG through SubmitPortrait. A submission is accepted only from
// the lease holder, at the leased size, once. Pixels cannot be verified, hence the
// server_settings portraits.clientRender switch. State is memory only.

import (
	"context"
	"database/sql"
	"log"
	"sync"
	"time"

	evedb "gurimoon/db"
	"gurimoon/esp"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
)

const (
	// portraitJobSize is what the billboard asks for (s=256).
	portraitJobSize = 256
	// portraitJobBatch is how many faces one poll hands out.  Each render is
	// a visible hitch on the drawing client, so keep it small.
	portraitJobBatch = 2
	// portraitJobLease is how long a pilot stays reserved for one renderer.
	portraitJobLease = 2 * time.Minute
	// portraitJobFailCooldown parks a pilot a client reported it could not
	// draw (no appearance data, a render error) before anyone tries again.
	portraitJobFailCooldown = 6 * time.Hour
	// Poll hints returned to the client, in seconds: soon while there is work,
	// rarely when there is none or the feature is off.
	portraitPollBusy = 20
	portraitPollIdle = 300
	portraitPollOff  = 3600
)

type portraitLease struct {
	renderer int64
	until    time.Time
}

type portraitQueue struct {
	mu     sync.Mutex
	leased map[int64]portraitLease // target charID -> who is drawing it
	failed map[int64]time.Time     // target charID -> retry after
}

var portraitJobs = portraitQueue{
	leased: map[int64]portraitLease{},
	failed: map[int64]time.Time{},
}

// next hands renderer up to portraitJobBatch of candidates.  Any earlier lease
// the renderer still holds is released first: a client polls only once it has
// finished (or given up on) its last batch.
func (q *portraitQueue) next(renderer int64, candidates []int64, now time.Time) []int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	for id, l := range q.leased {
		if l.renderer == renderer || now.After(l.until) {
			delete(q.leased, id)
		}
	}
	for id, t := range q.failed {
		if now.After(t) {
			delete(q.failed, id)
		}
	}
	var out []int64
	for _, id := range candidates {
		if len(out) >= portraitJobBatch {
			break
		}
		if _, busy := q.leased[id]; busy {
			continue
		}
		if _, parked := q.failed[id]; parked {
			continue
		}
		q.leased[id] = portraitLease{renderer: renderer, until: now.Add(portraitJobLease)}
		out = append(out, id)
	}
	return out
}

// take consumes renderer's lease on target.  A submission that arrives a
// little after the lease ran out is still accepted as long as nobody else has
// been handed the pilot since.
func (q *portraitQueue) take(renderer, target int64) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	l, ok := q.leased[target]
	if !ok || l.renderer != renderer {
		return false
	}
	delete(q.leased, target)
	return true
}

func (q *portraitQueue) fail(target int64, now time.Time) {
	q.mu.Lock()
	q.failed[target] = now.Add(portraitJobFailCooldown)
	q.mu.Unlock()
}

// portraitRenderEnabled reads server_settings portraits.clientRender; anything
// but "0" (including no row) is on.
func portraitRenderEnabled(ctx context.Context, sqldb *sql.DB) bool {
	v, ok, err := evedb.GetServerSetting(ctx, sqldb, "portraits.clientRender")
	if err != nil || !ok {
		return true
	}
	return v != "0"
}

// portraitJobsRsp is GetPortraitJobs' answer: (charIDs, size, pollSeconds).
func portraitJobsRsp(ids []int64, poll int) marshal.PyRep {
	items := make([]marshal.PyRep, len(ids))
	for i, id := range ids {
		items[i] = marshal.PyInt{Value: uint64(id)}
	}
	return marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyList{Items: items},
		marshal.PyInt{Value: portraitJobSize},
		marshal.PyInt{Value: uint64(poll)},
	}}
}

// handleGetPortraitJobs: no args.  Answers (charIDs, size, pollSeconds).
func handleGetPortraitJobs(ctx context.Context, c *evenet.Conn, addr string, pkt *evenet.PyPacket, accountID, charID int64, sqldb *sql.DB) error {
	if charID <= 0 || !portraitRenderEnabled(ctx, sqldb) {
		return server.SendCallRsp(c, pkt, accountID, portraitJobsRsp(nil, portraitPollOff))
	}
	ids, err := evedb.TopBountyCharIDs(ctx, sqldb)
	if err != nil {
		log.Printf("[%s] charmgr.GetPortraitJobs db: %v", addr, err)
		return server.SendCallRsp(c, pkt, accountID, portraitJobsRsp(nil, portraitPollIdle))
	}
	var missing []int64
	for _, id := range ids {
		if !esp.HasStoredPortrait(id, portraitJobSize) {
			missing = append(missing, id)
		}
	}
	jobs := portraitJobs.next(charID, missing, time.Now())
	poll := portraitPollIdle
	if len(jobs) > 0 {
		poll = portraitPollBusy
		log.Printf("[%s] → charmgr.GetPortraitJobs renderer=%d jobs=%v (%d unrendered)", addr, charID, jobs, len(missing))
	}
	return server.SendCallRsp(c, pkt, accountID, portraitJobsRsp(jobs, poll))
}

// handleSubmitPortrait: (targetCharID, size, data).  data is the PNG as a
// string, or None when the client could not draw the pilot.  Answers a bool.
func handleSubmitPortrait(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID int64) error {
	args := cs.ArgTuple.Items
	if len(args) < 3 {
		log.Printf("[%s] charmgr.SubmitPortrait: need 3 args, got %d", addr, len(args))
		return server.SendCallException(c, pkt, accountID, nil)
	}
	target, ok1 := args[0].(marshal.PyInt)
	size, ok2 := args[1].(marshal.PyInt)
	if !ok1 || !ok2 {
		log.Printf("[%s] charmgr.SubmitPortrait: bad arg types %T, %T", addr, args[0], args[1])
		return server.SendCallException(c, pkt, accountID, nil)
	}
	targetID := int64(target.Value)
	if !portraitJobs.take(charID, targetID) {
		log.Printf("[%s] charmgr.SubmitPortrait: renderer=%d holds no lease on %d — refused", addr, charID, targetID)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyBool{Value: false})
	}

	var data []byte
	switch v := args[2].(type) {
	case marshal.PyString:
		data = []byte(v.Value)
	case marshal.PyBuffer:
		data = v.Data
	case marshal.PyNone:
		log.Printf("[%s] charmgr.SubmitPortrait: renderer=%d could not draw %d — parked %s", addr, charID, targetID, portraitJobFailCooldown)
		portraitJobs.fail(targetID, time.Now())
		return server.SendCallRsp(c, pkt, accountID, marshal.PyBool{Value: true})
	default:
		log.Printf("[%s] charmgr.SubmitPortrait: data is %T", addr, args[2])
		portraitJobs.fail(targetID, time.Now())
		return server.SendCallRsp(c, pkt, accountID, marshal.PyBool{Value: false})
	}

	if int64(size.Value) != portraitJobSize {
		log.Printf("[%s] charmgr.SubmitPortrait: renderer=%d sent size %d for %d, want %d", addr, charID, size.Value, targetID, portraitJobSize)
		portraitJobs.fail(targetID, time.Now())
		return server.SendCallRsp(c, pkt, accountID, marshal.PyBool{Value: false})
	}
	if err := esp.StoreClientPortrait(targetID, portraitJobSize, data); err != nil {
		log.Printf("[%s] charmgr.SubmitPortrait: renderer=%d target=%d: %v", addr, charID, targetID, err)
		portraitJobs.fail(targetID, time.Now())
		return server.SendCallRsp(c, pkt, accountID, marshal.PyBool{Value: false})
	}
	log.Printf("[%s] → charmgr.SubmitPortrait renderer=%d stored %d_%d.png (%d bytes)", addr, charID, targetID, portraitJobSize, len(data))
	return server.SendCallRsp(c, pkt, accountID, marshal.PyBool{Value: true})
}
