// Package account implements the "authentication" service.
package account

import (
	"fmt"
	"log"

	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
)

// Handle dispatches a CallReq targeting the "authentication" service.
func Handle(c *evenet.Conn, addr string, cs *server.CallStream, req *evenet.PyPacket, accountID int64) error {
	switch cs.Method {
	case "Ping":
		return handlePing(c, addr, req, accountID)
	case "GetPostAuthenticationMessage":
		return handleGetPostAuthenticationMessage(c, addr, req, accountID)
	case "SetLanguageID":
		// gameui.py SetLanguage(key): the client stores prefs.languageID locally and
		// discards the return, so the call is only acknowledged.
		if len(cs.ArgTuple.Items) >= 1 {
			if lang, ok := cs.ArgTuple.Items[0].(marshal.PyString); ok {
				log.Printf("[%s] → authentication.SetLanguageID %q (ack)", addr, lang.Value)
			}
		}
		return server.SendCallRsp(c, req, accountID, marshal.PyNone{})
	default:
		server.RecordUnhandled(addr, "authentication", cs, false)
		log.Printf("[%s] authentication: unknown method %q — sending exception", addr, cs.Method)
		return server.SendCallException(c, req, accountID, nil)
	}
}

// handlePing returns Win32TimeNow() as a PyInt.
func handlePing(c *evenet.Conn, addr string, req *evenet.PyPacket, accountID int64) error {
	log.Printf("[%s] → authentication.Ping → %d", addr, server.Win32TimeNow())
	return server.SendCallRsp(c, req, accountID, marshal.PyInt{Value: server.Win32TimeNow()})
}

// handleGetPostAuthenticationMessage returns PyNone (no MOTD configured).
func handleGetPostAuthenticationMessage(c *evenet.Conn, addr string, req *evenet.PyPacket, accountID int64) error {
	log.Printf("[%s] → authentication.GetPostAuthenticationMessage → None", addr)
	return server.SendCallRsp(c, req, accountID, marshal.PyNone{})
}

// Keeps the fmt import referenced.
var _ = fmt.Errorf // satisfy import
