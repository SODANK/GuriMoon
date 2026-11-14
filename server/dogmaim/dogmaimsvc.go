// Package dogmaim implements the "dogmaIM" service: one direct method
// (GetAttributeTypes) plus a bound-object interface for ship/char attribute
// queries. Bound-object calls arrive with an empty service name and are handled by
// the main dispatch loop.
package dogmaim

import (
	"context"
	"database/sql"
	"log"
	"time"

	evecache "gurimoon/cache"
	evedb "gurimoon/db"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
)

// Handle dispatches a CallReq addressed to the "dogmaIM" service.
func Handle(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB, cacheStore *evecache.Store) error {
	switch cs.Method {
	case "GetAttributeTypes":
		// GetAttributeTypes answers a cache hint for "dogmaIM.attributesByName"; the client
		// then fetches the data via objectCaching.GetCachableObject.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		e, err := cacheStore.Get(ctx, sqldb, "dogmaIM.attributesByName")
		if err != nil {
			log.Printf("[%s] dogmaIM.GetAttributeTypes cache: %v — returning None", addr, err)
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		log.Printf("[%s] → dogmaIM.GetAttributeTypes (CachedMethodCallResult)", addr)
		return server.SendCallRsp(c, pkt, accountID, evecache.CachedMethodCallResult(e))
	case "GetSkillPrerequisites":
		// Client ccutil.GetSkillPrerequisites caches a skill's prerequisite list.
		// Return a flat PyList of the required-skill typeIDs (requiredSkill1/2/3).
		var skillID int64
		if len(cs.ArgTuple.Items) >= 1 {
			if v, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
				skillID = int64(v.Value)
			}
		}
		var items []marshal.PyRep
		if skillID != 0 {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			reqs, err := evedb.GetRequiredSkills(ctx, sqldb, skillID)
			cancel()
			if err != nil {
				log.Printf("[%s] dogmaIM.GetSkillPrerequisites db: %v", addr, err)
			}
			for _, r := range reqs {
				items = append(items, marshal.PyInt{Value: uint64(r.SkillID)})
			}
		}
		log.Printf("[%s] → dogmaIM.GetSkillPrerequisites skillID=%d reqs=%d", addr, skillID, len(items))
		return server.SendCallRsp(c, pkt, accountID, marshal.PyList{Items: items})
	default:
		server.RecordUnhandled(addr, "dogmaIM", cs, true)
		log.Printf("[%s] dogmaIM: unhandled method %q — returning None", addr, cs.Method)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
}
