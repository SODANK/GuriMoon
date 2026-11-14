// Package alert implements the "alert" service.
//
// BeanCount answers (None, None) so the client follows with a full stack trace
// (SendClientStackTraceAlert / SendProxyStackTraceAlert). BeanDelivery carries repeat
// reports and is acknowledged with None; the server records nothing in a DB.
package alert

import (
	"fmt"
	"log"

	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
)

// Handle dispatches a CallReq addressed to the "alert" service.
func Handle(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64) error {
	switch cs.Method {
	case "BeanCount":
		return handleBeanCount(c, addr, cs, pkt, accountID)
	case "BeanDelivery":
		return handleBeanDelivery(c, addr, cs, pkt, accountID)
	case "SendClientStackTraceAlert":
		return handleSendClientStackTraceAlert(c, addr, cs, pkt, accountID)
	case "SendProxyStackTraceAlert":
		return handleSendProxyStackTraceAlert(c, addr, cs, pkt, accountID)
	default:
		server.RecordUnhandled(addr, "alert", cs, false)
		log.Printf("[%s] alert: unhandled method %q — sending exception", addr, cs.Method)
		return server.SendCallException(c, pkt, accountID, nil)
	}
}

// handleBeanCount responds with (None, None) to request a full stack trace.
//
// When errorID is None, the client calls SendClientStackTraceAlert with the
// full exception stack trace, which we log server-side for debugging.
func handleBeanCount(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64) error {
	log.Printf("[%s] ← alert.BeanCount — requesting full stack trace from client", addr)
	for i, arg := range cs.ArgTuple.Items {
		log.Printf("[%s]   BeanCount arg[%d]: %s", addr, i, fmt.Sprintf("%T %+v", arg, arg))
	}
	// Return (None, None) so the client sends the full stack trace via
	// SendClientStackTraceAlert instead of just a hash.
	result := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyNone{}, // errorID=None → triggers SendClientStackTraceAlert
		marshal.PyNone{}, // loggingMode=None
	}}
	return server.SendCallRsp(c, pkt, accountID, result)
}

// handleSendClientStackTraceAlert receives the full client-side exception stack
// trace the client sends when BeanCount returns (None, None).
// Args: (stackID=(adler32_hash, exception_text), stackTrace, mode). The answer
// is the throttling tuple, see throttlingResult.
func handleSendClientStackTraceAlert(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64) error {
	log.Printf("[%s] ← alert.SendClientStackTraceAlert — FULL EXCEPTION FROM CLIENT:", addr)
	captureTrace(addr, cs)
	// The traceback arrives as a PyBuffer of raw bytes (some zlib-framed); logTrace
	// decodes it and logs the Python traceback verbatim.
	logTrace(addr, cs, log.Printf)
	return server.SendCallRsp(c, pkt, accountID, throttlingResult(cs))
}

// throttlingResult builds the (errorID, loggingMode) tuple for both stack-trace
// calls. The client stores the return value in its throttling table (alert.py
// __SendStackTraceAlert) and unpacks it on the next trace with the same hash, so
// None would raise "unpack non-sequence" inside the alert service. errorID is the
// client's own stack hash; loggingMode 0 sends repeats via BeanDelivery.
func throttlingResult(cs *server.CallStream) marshal.PyRep {
	var errorID marshal.PyRep = marshal.PyInt{Value: 34135}
	if len(cs.ArgTuple.Items) > 0 {
		if stackID, ok := cs.ArgTuple.Items[0].(marshal.PyTuple); ok && len(stackID.Items) > 0 {
			if _, isInt := stackID.Items[0].(marshal.PyInt); isInt {
				errorID = stackID.Items[0]
			}
		}
	}
	return marshal.PyTuple{Items: []marshal.PyRep{
		errorID,
		marshal.PyInt{Value: 0},
	}}
}

// handleSendProxyStackTraceAlert receives the full stack trace after BeanCount
// returned (None, None). Args: (stackID=(hash, culpritText), stackTrace, mode) +
// kwargs. The answer is the same throttling tuple.
func handleSendProxyStackTraceAlert(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64) error {
	log.Printf("[%s] ← alert.SendProxyStackTraceAlert — FULL EXCEPTION FROM CLIENT:", addr)
	captureTrace(addr, cs)
	logTrace(addr, cs, log.Printf)
	return server.SendCallRsp(c, pkt, accountID, throttlingResult(cs))
}

// handleBeanDelivery receives a client-side exception report, logs it and
// returns None.
func handleBeanDelivery(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64) error {
	log.Printf("[%s] ← alert.BeanDelivery — client delivering exception payload:", addr)
	// Repeat occurrences of an already-reported exception arrive here; capturing
	// them keeps the occurrence count on /coverage.
	captureTrace(addr, cs)
	// Surface the exception text from the arg tuple; args may be absent if the packet is malformed.
	if len(cs.ArgTuple.Items) > 0 {
		log.Printf("[%s]   payload: %s", addr, fmt.Sprintf("%T %+v", cs.ArgTuple.Items[0], cs.ArgTuple.Items[0]))
	}
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}
