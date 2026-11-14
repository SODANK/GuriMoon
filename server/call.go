// Package server contains shared helpers for service call handling.
package server

import (
	"fmt"
	"time"

	"gurimoon/bugtrack"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/rpccov"
)

// RecordUnhandled files a method that reached a service's default arm into the RPC
// coverage ledger (ESP /coverage). service is the wire service name, "" for bound-object
// calls. silent marks a service that answers None: the client sees success and renders
// an empty window, whereas an exception is at least visible.
func RecordUnhandled(addr, service string, cs *CallStream, silent bool) {
	if cs == nil {
		return
	}
	outcome := rpccov.Unhandled
	if silent {
		outcome = rpccov.Fallthrough
	}
	rpccov.Record(addr, service, cs.Method, outcome)
}

// NodeID is the macho node identifier used by this server instance.
const NodeID uint64 = 888444

// Win32TimeNow returns the current time as a Windows FILETIME (100-nanosecond
// intervals since 1601-01-01 UTC).
func Win32TimeNow() uint64 {
	return uint64((time.Now().Unix() + 11644473600) * 10000000)
}

// Win32TimeNowRep returns Win32TimeNow() as a marshal.PyInt ready for wire use.
func Win32TimeNowRep() marshal.PyInt {
	return marshal.PyInt{Value: Win32TimeNow()}
}

// CallStream is the decoded form of a macho.CallReq payload.
type CallStream struct {
	// RemoteObject is set when the call targets a numeric bound-object ID.
	RemoteObject uint64
	// RemoteObjectStr is set when the call names a service string directly.
	RemoteObjectStr string
	Method          string
	ArgTuple        marshal.PyTuple
	ArgDict         *marshal.PyDict
}

// DecodeCallStream parses the nested tuple / substream payload of a macho.CallReq.
//
//	pkt.Payload  = PyTuple[2]; [1] ignored
//	  [0] = PyTuple[2]
//	          [0] = int or string (ignored)
//	          [1] = PySubStream, decoded = PyTuple[4]
//	                  [0] = remoteObject: PyInt | PyString
//	                  [1] = method:       PyString
//	                  [2] = argTuple:     PyTuple
//	                  [3] = argDict:      PyDict | PyNone
func DecodeCallStream(pkt *evenet.PyPacket) (*CallStream, error) {
	outer, ok := pkt.Payload.(marshal.PyTuple)
	if !ok {
		return nil, fmt.Errorf("payload is %T, want PyTuple", pkt.Payload)
	}
	if len(outer.Items) < 1 {
		return nil, fmt.Errorf("outer tuple has %d items, want ≥1", len(outer.Items))
	}

	inner, ok := outer.Items[0].(marshal.PyTuple)
	if !ok {
		return nil, fmt.Errorf("payload[0] is %T, want PyTuple", outer.Items[0])
	}
	if len(inner.Items) < 2 {
		return nil, fmt.Errorf("inner tuple has %d items, want ≥2", len(inner.Items))
	}

	ss, ok := inner.Items[1].(*marshal.PySubStream)
	if !ok {
		return nil, fmt.Errorf("payload[0][1] is %T, want *PySubStream", inner.Items[1])
	}
	if ss.Decoded == nil {
		return nil, fmt.Errorf("substream decoded is nil")
	}

	body, ok := ss.Decoded.(marshal.PyTuple)
	if !ok {
		return nil, fmt.Errorf("substream body is %T, want PyTuple", ss.Decoded)
	}
	if len(body.Items) < 4 {
		return nil, fmt.Errorf("call body tuple has %d items, want 4", len(body.Items))
	}

	cs := &CallStream{}

	// body[0]: remoteObject, int or string
	switch v := body.Items[0].(type) {
	case marshal.PyInt:
		cs.RemoteObject = v.Value
	case marshal.PyString:
		cs.RemoteObjectStr = v.Value
	default:
		return nil, fmt.Errorf("body[0] (remoteObject) is %T, want PyInt or PyString", body.Items[0])
	}

	// body[1]: method name
	ms, ok := body.Items[1].(marshal.PyString)
	if !ok {
		return nil, fmt.Errorf("body[1] (method) is %T, want PyString", body.Items[1])
	}
	cs.Method = ms.Value

	// body[2]: argument tuple
	at, ok := body.Items[2].(marshal.PyTuple)
	if !ok {
		return nil, fmt.Errorf("body[2] (argTuple) is %T, want PyTuple", body.Items[2])
	}
	cs.ArgTuple = at

	// body[3]: optional argument dict (may be PyNone)
	if d, ok := body.Items[3].(marshal.PyDict); ok {
		cs.ArgDict = &d
	}

	return cs, nil
}

// SendCallRsp sends a macho.CallRsp wrapping result in a PySubStream inside a 1-tuple.
// The client's ObjectCallGPCS.FromPickle() unpickles payload[0], so it must be a
// marshaled buffer (PySubStream), not a raw Python object.
func SendCallRsp(c *evenet.Conn, req *evenet.PyPacket, accountID int64, result marshal.PyRep) error {
	rsp := &evenet.PyPacket{
		TypeString: "macho.CallRsp",
		Type:       evenet.CallRsp,
		Source:     req.Dest,
		Dest: evenet.PyAddress{
			Type:   evenet.AddrClient,
			TypeID: 130293001608, // HackFixedClientID
			CallID: req.Source.CallID,
		},
		UserID:  uint64(accountID),
		Payload: marshal.PyTuple{Items: []marshal.PyRep{&marshal.PySubStream{Decoded: result}}},
	}
	b, err := marshal.Marshal(rsp.Encode())
	if err != nil {
		return fmt.Errorf("marshal CallRsp: %w", err)
	}
	return c.SendPacket(b)
}

// SendNotification sends a macho.Notification in EVENotificationStream wire format.
// eventType is the notification name (e.g. "OnAccountChange"), bcastIDType the
// broadcast ID filter (e.g. "clientID"), args the tuple passed to the client handler.
func SendNotification(c *evenet.Conn, accountID int64, eventType, bcastIDType string, args marshal.PyRep) error {
	innerStream := &marshal.PySubStream{
		Decoded: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: 0},
			marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyInt{Value: 1},
				args,
			}},
		}},
	}
	payload := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: 0},
			innerStream,
		}},
		marshal.PyNone{},
	}}
	pkt := &evenet.PyPacket{
		TypeString: "macho.Notification",
		Type:       evenet.Notification,
		Source: evenet.PyAddress{
			Type:   evenet.AddrNode,
			TypeID: NodeID,
		},
		Dest: evenet.PyAddress{
			Type:        evenet.AddrBroadcast,
			Service:     eventType,
			BcastIDType: bcastIDType,
		},
		UserID:  uint64(accountID),
		Payload: payload,
		// No named_payload "sn": a missing sn is delivered at once, while a constant sn
		// blocks every notification after the first for 60 s (machoNet.WaitForSequenceNumber).
	}
	b, err := marshal.Marshal(pkt.Encode())
	if err != nil {
		return fmt.Errorf("marshal Notification: %w", err)
	}
	return c.SendPacket(b)
}

// SendSessionChange builds and sends a macho.SessionChangeNotification. Each entry's
// value is an (old, new) 2-tuple.
func SendSessionChange(c *evenet.Conn, accountID uint64, entries []marshal.DictEntry) error {
	payload := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: 0},
			marshal.PyDict{Items: entries},
		}},
		marshal.PyList{Items: []marshal.PyRep{marshal.PyInt{Value: NodeID}}},
	}}
	pkt := &evenet.PyPacket{
		TypeString: "macho.SessionChangeNotification",
		Type:       evenet.SessionChangeNotification,
		Source: evenet.PyAddress{
			Type:   evenet.AddrNode,
			TypeID: NodeID,
		},
		Dest: evenet.PyAddress{
			Type:   evenet.AddrClient,
			TypeID: 130293001608,
		},
		UserID:  accountID,
		Payload: payload,
		NamedPayload: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "channel"}, Value: marshal.PyString{Value: "sessionchange"}},
		}},
	}
	b, err := marshal.Marshal(pkt.Encode())
	if err != nil {
		return fmt.Errorf("marshal SessionChangeNotification: %w", err)
	}
	return c.SendPacket(b)
}

// SendCallException sends a macho.ErrorResponse for a failed CallReq. The payload
// matches macho_MachoException::Encode() with in_response_to = CALL_REQ (6) and
// exception_type = WRAPPEDEXCEPTION (2).
func SendCallException(c *evenet.Conn, req *evenet.PyPacket, accountID int64, exceptionPayload marshal.PyRep) error {
	if exceptionPayload == nil {
		// A nil payload is the generic failure, usually an unimplemented method or an
		// internal error. It is filed in the bug tracker (ESP /bugs) with the service.
		bugtrack.Report("exception", req.Dest.Service, "",
			fmt.Sprintf("generic exception sent for service %q (likely unhandled method or internal failure)", req.Dest.Service))
		// The payload must be a raisable object: ExceptionWrapperGPCS.CallDown re-raises it
		// verbatim. CustomError (ui/inflight/scanner.py) shows its 'error' value.
		where := req.Dest.Service
		if where == "" || where == "None" {
			where = "bound object"
		}
		exceptionPayload = BuildUserError("CustomError", map[string]marshal.PyRep{
			"error": marshal.PyString{Value: fmt.Sprintf(
				"The server could not complete that request (%s).", where)},
		})
	}
	payload := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyInt{Value: uint64(evenet.CallReq)}, // in_response_to
		marshal.PyInt{Value: 2},                      // WRAPPEDEXCEPTION
		marshal.PyTuple{Items: []marshal.PyRep{
			&marshal.PySubStream{Decoded: exceptionPayload},
		}},
	}}
	rsp := &evenet.PyPacket{
		TypeString: "macho.ErrorResponse",
		Type:       evenet.ErrorResponse,
		Source:     req.Dest,
		Dest: evenet.PyAddress{
			Type:   evenet.AddrClient,
			TypeID: 130293001608,
			CallID: req.Source.CallID,
		},
		UserID:  uint64(accountID),
		Payload: payload,
	}
	b, err := marshal.Marshal(rsp.Encode())
	if err != nil {
		return fmt.Errorf("marshal ErrorResponse: %w", err)
	}
	return c.SendPacket(b)
}

// BuildUserError constructs a ccp_exceptions.UserError PyObject for SendCallException.
// args must be (msg, dict) when there is a dict: log.LogException shows an escaped
// exception as eve.Message(*exc.args), and with args = (msg,) a message with
// %(name)s arguments shows nothing.
func BuildUserError(errorName string, dict map[string]marshal.PyRep) marshal.PyRep {
	var dictRep marshal.PyRep = marshal.PyNone{}
	args := []marshal.PyRep{marshal.PyString{Value: errorName}}
	if len(dict) > 0 {
		items := make([]marshal.DictEntry, 0, len(dict))
		for k, v := range dict {
			items = append(items, marshal.DictEntry{
				Key: marshal.PyString{Value: k}, Value: v,
			})
		}
		dictRep = marshal.PyDict{Items: items}
		args = append(args, dictRep)
	}
	return marshal.PyObject{
		Type: "ccp_exceptions.UserError",
		Args: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "args"}, Value: marshal.PyTuple{Items: args}},
			{Key: marshal.PyString{Value: "dict"}, Value: dictRep},
			{Key: marshal.PyString{Value: "msg"}, Value: marshal.PyString{Value: errorName}},
		}},
	}
}
