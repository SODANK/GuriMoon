package keeper

import (
	"fmt"
	"log"

	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
)

// Handle dispatches a CallReq addressed to the "keeper" service.
func Handle(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64) error {
	switch cs.Method {
	case "GetLevelEditor":
		// Return a bound object reference for the LevelEditor.
		// bindID 4 is reserved for the level editor bound object.
		bindSpec := fmt.Sprintf("N=%d:4", server.NodeID)
		boundObj := marshal.PySubStruct{Value: &marshal.PySubStream{
			Decoded: marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyString{Value: bindSpec},
				marshal.PyInt{Value: 0},
			}},
		}}
		log.Printf("[%s] → keeper.GetLevelEditor → bind %q", addr, bindSpec)
		return server.SendCallRsp(c, pkt, accountID, boundObj)
	default:
		server.RecordUnhandled(addr, "keeper", cs, true)
		log.Printf("[%s] keeper: unhandled method %q — returning None", addr, cs.Method)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
}
