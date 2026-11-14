package main

import (
	"database/sql"

	"gurimoon/destiny"
	"gurimoon/esp"
	evenet "gurimoon/net"
	"gurimoon/npc"
	"gurimoon/server"
)

// callCtx is one bound-object call and the connection state its handler reads. handlePacket
// builds it per call; handleBoundCall and the bound<Method> handlers take it in place of a
// long parameter list. See docs/go-idioms.md#call-context.
type callCtx struct {
	c          *evenet.Conn
	addr       string
	cs         *server.CallStream
	pkt        *evenet.PyPacket
	accountID  int64
	charID     int64
	stationID  *int64
	agentBinds *agentBindState
	sqldb      *sql.DB
	ds         *destinyConn
	sysMgr     *destiny.SystemManager
	sessReg    *esp.SessionRegistry
	npcMgr     *npc.NPCManager
}
