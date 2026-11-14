// Package objectcaching implements the "objectCaching" service.
//
// GetCachableObject returns a bulk-data object keyed by objectID. Entries are
// generated from the database on first request (via cache.Store) and kept in
// memory for the server's lifetime.
package objectcaching

import (
	"context"
	"database/sql"
	"log"
	"time"

	evecache "gurimoon/cache"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
)

// Handle dispatches a CallReq addressed to the "objectCaching" service.
func Handle(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, store *evecache.Store, db *sql.DB) error {
	switch cs.Method {
	case "GetCachableObject":
		return handleGetCachableObject(c, addr, cs, pkt, accountID, store, db)
	default:
		server.RecordUnhandled(addr, "objectCaching", cs, false)
		log.Printf("[%s] objectCaching: unhandled method %q — sending exception", addr, cs.Method)
		return server.SendCallException(c, pkt, accountID, nil)
	}
}

// handleGetCachableObject returns the objectCaching.CachedObject for the requested
// objectID, generating it from the database if not cached.
//
// Arg tuple (packets/Authentication.xmlp CallGetCachableObject):
//
//	[0] shared   (int)
//	[1] objectID (PyString or PyTuple)
//	[2] (timestamp, version) inner tuple
//	[3] nodeID   (int)
func handleGetCachableObject(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, store *evecache.Store, db *sql.DB) error {
	if len(cs.ArgTuple.Items) < 2 {
		log.Printf("[%s] objectCaching.GetCachableObject: arg tuple too short (%d items)", addr, len(cs.ArgTuple.Items))
		return server.SendCallException(c, pkt, accountID, nil)
	}

	// Items[1] is the objectID: a plain string for all config.BulkData.* objects.
	var objectID string
	switch v := cs.ArgTuple.Items[1].(type) {
	case marshal.PyString:
		objectID = v.Value
	default:
		log.Printf("[%s] objectCaching.GetCachableObject: objectID is %T (not a string), skipping", addr, cs.ArgTuple.Items[1])
		return server.SendCallException(c, pkt, accountID, nil)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	e, err := store.Get(ctx, db, objectID)
	if err != nil {
		log.Printf("[%s] objectCaching.GetCachableObject %q: %v — returning None", addr, objectID, err)
		// Return None: client falls back to its local copy.
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}

	log.Printf("[%s] → objectCaching.GetCachableObject %q (v=0x%x, %d bytes)", addr, objectID, e.Version, len(e.Data))
	return server.SendCallRsp(c, pkt, accountID, evecache.EncodeResponse(e))
}
