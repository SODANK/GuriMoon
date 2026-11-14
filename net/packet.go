package net

import (
	"errors"
	"fmt"

	"gurimoon/marshal"
)

// MachoMsgType matches MACHONETMSG_TYPE from packet_types.h.
type MachoMsgType int

const (
	AuthenticationReq               MachoMsgType = 0
	AuthenticationRsp               MachoMsgType = 1
	IdentificationReq               MachoMsgType = 2
	IdentificationRsp               MachoMsgType = 3
	CallReq                         MachoMsgType = 6
	CallRsp                         MachoMsgType = 7
	TransportClosed                 MachoMsgType = 8
	ResolveReq                      MachoMsgType = 10
	ResolveRsp                      MachoMsgType = 11
	Notification                    MachoMsgType = 12
	ErrorResponse                   MachoMsgType = 15
	SessionChangeNotification       MachoMsgType = 16
	SessionInitialStateNotification MachoMsgType = 18
	PingReq                         MachoMsgType = 20
	PingRsp                         MachoMsgType = 21
)

// AddrType matches PyAddress::AddrType from PyPacket.h.
type AddrType byte

const (
	AddrAny       AddrType = 'A'
	AddrNode      AddrType = 'N'
	AddrClient    AddrType = 'C'
	AddrBroadcast AddrType = 'B'
	AddrInvalid   AddrType = 'I'
)

// PyAddress is the decoded form of a macho.MachoAddress object.
type PyAddress struct {
	Type        AddrType
	TypeID      uint64 // nodeID or clientID
	CallID      uint64
	Service     string
	BcastIDType string // used for Broadcast addresses
}

// Decode parses a PyRep that represents a macho.MachoAddress object.
func (a *PyAddress) Decode(rep marshal.PyRep) error {
	obj, ok := rep.(marshal.PyObject)
	if !ok {
		return errors.New("pyaddress: expected PyObject")
	}
	args, ok := obj.Args.(marshal.PyTuple)
	if !ok {
		return errors.New("pyaddress: expected tuple arguments")
	}
	if len(args.Items) < 3 {
		return fmt.Errorf("pyaddress: too few items in address tuple: %d", len(args.Items))
	}
	typeStr, ok := args.Items[0].(marshal.PyString)
	if !ok || len(typeStr.Value) == 0 {
		return errors.New("pyaddress: expected string for address type item[0]")
	}
	a.Type = AddrType(typeStr.Value[0])

	switch a.Type {
	case AddrAny: // ("A", service, callID)
		if len(args.Items) != 3 {
			return fmt.Errorf("pyaddress Any: expected 3 items, got %d", len(args.Items))
		}
		if err := decodeService(args.Items[1], &a.Service); err != nil {
			return err
		}
		if err := decodeUint64(args.Items[2], &a.CallID); err != nil {
			return err
		}
	case AddrNode: // ("N", nodeID, service, callID)
		if len(args.Items) != 4 {
			return fmt.Errorf("pyaddress Node: expected 4 items, got %d", len(args.Items))
		}
		if err := decodeUint64(args.Items[1], &a.TypeID); err != nil {
			return err
		}
		if err := decodeService(args.Items[2], &a.Service); err != nil {
			return err
		}
		if err := decodeUint64(args.Items[3], &a.CallID); err != nil {
			return err
		}
	case AddrClient: // ("C", clientID, callID, service)
		if len(args.Items) != 4 {
			return fmt.Errorf("pyaddress Client: expected 4 items, got %d", len(args.Items))
		}
		if err := decodeUint64(args.Items[1], &a.TypeID); err != nil {
			return err
		}
		if err := decodeUint64(args.Items[2], &a.CallID); err != nil {
			return err
		}
		if err := decodeService(args.Items[3], &a.Service); err != nil {
			return err
		}
	case AddrBroadcast: // ("B", broadcastID, narrowcast, idtype)
		if len(args.Items) != 4 {
			return fmt.Errorf("pyaddress Broadcast: expected 4 items, got %d", len(args.Items))
		}
		if err := decodeService(args.Items[1], &a.Service); err != nil {
			return err
		}
		idt, ok := args.Items[3].(marshal.PyString)
		if !ok {
			return errors.New("pyaddress Broadcast: expected string for idtype item[3]")
		}
		a.BcastIDType = idt.Value
	default:
		return fmt.Errorf("pyaddress: unknown address type %c", a.Type)
	}
	return nil
}

// Encode builds a macho.MachoAddress PyObject from the address.
func (a *PyAddress) Encode() marshal.PyRep {
	var t marshal.PyTuple
	switch a.Type {
	case AddrAny:
		t.Items = []marshal.PyRep{
			marshal.PyString{Value: "A"},
			encodeService(a.Service),
			encodeUint64(a.CallID),
		}
	case AddrNode:
		t.Items = []marshal.PyRep{
			marshal.PyString{Value: "N"},
			marshal.PyInt{Value: a.TypeID},
			encodeService(a.Service),
			encodeUint64(a.CallID),
		}
	case AddrClient:
		t.Items = []marshal.PyRep{
			marshal.PyString{Value: "C"},
			marshal.PyInt{Value: a.TypeID},
			marshal.PyInt{Value: a.CallID},
			encodeService(a.Service),
		}
	case AddrBroadcast:
		t.Items = []marshal.PyRep{
			marshal.PyString{Value: "B"},
			encodeService(a.Service),
			marshal.PyList{}, // narrowcast: empty list
			marshal.PyString{Value: a.BcastIDType},
		}
	default:
		return marshal.PyNone{}
	}
	return marshal.PyObject{Type: "macho.MachoAddress", Args: t}
}

// PyPacket is the decoded wire packet (macho.MachoNetMsg).
// It mirrors the C++ PyPacket class from PyPacket.h.
type PyPacket struct {
	TypeString   string // object type name, e.g. "macho.MachoNetMsg"
	Type         MachoMsgType
	Source       PyAddress
	Dest         PyAddress
	UserID       uint64
	Payload      marshal.PyRep // tuple (or buffer), item [4]
	NamedPayload marshal.PyRep // dict or nil, item [5]
}

var validMsgTypes = map[MachoMsgType]bool{
	AuthenticationReq: true, AuthenticationRsp: true,
	IdentificationReq: true, IdentificationRsp: true,
	CallReq: true, CallRsp: true,
	TransportClosed: true,
	ResolveReq:      true, ResolveRsp: true,
	Notification:                    true,
	ErrorResponse:                   true,
	SessionChangeNotification:       true,
	SessionInitialStateNotification: true,
	PingReq:                         true, PingRsp: true,
}

// Decode parses the top-level PyRep emitted by EVEPresentation into a PyPacket.
// Ownership of rep is consumed.
func (p *PyPacket) Decode(rep marshal.PyRep) error {
	// Dragon wraps it in a checksumed stream, then sometimes a substream.
	if cs, ok := rep.(marshal.PyChecksumedStream); ok {
		rep = cs.Value
	}
	if ss, ok := rep.(*marshal.PySubStream); ok {
		if ss.Decoded == nil {
			return errors.New("pypacket: nil decoded substream")
		}
		rep = ss.Decoded
	}

	obj, ok := rep.(marshal.PyObject)
	if !ok {
		return fmt.Errorf("pypacket: expected PyObject, got %T", rep)
	}
	p.TypeString = obj.Type

	tuple, ok := obj.Args.(marshal.PyTuple)
	if !ok {
		return fmt.Errorf("pypacket: object args is not a PyTuple, got %T", obj.Args)
	}
	if len(tuple.Items) != 6 {
		return fmt.Errorf("pypacket: expected 6-element tuple, got %d", len(tuple.Items))
	}

	// item[0]: message type integer
	typer, ok := tuple.Items[0].(marshal.PyInt)
	if !ok {
		return fmt.Errorf("pypacket: item[0] is not PyInt, got %T", tuple.Items[0])
	}
	mt := MachoMsgType(typer.Value)
	if !validMsgTypes[mt] {
		return fmt.Errorf("pypacket: unknown message type %d", mt)
	}
	p.Type = mt

	// item[1]: source address
	if err := p.Source.Decode(tuple.Items[1]); err != nil {
		return fmt.Errorf("pypacket source: %w", err)
	}
	// item[2]: dest address
	if err := p.Dest.Decode(tuple.Items[2]); err != nil {
		return fmt.Errorf("pypacket dest: %w", err)
	}

	// item[3]: userID or None
	switch v := tuple.Items[3].(type) {
	case marshal.PyInt:
		p.UserID = v.Value
	case marshal.PyNone:
		p.UserID = 0
	default:
		return fmt.Errorf("pypacket: item[3] userID has unexpected type %T", tuple.Items[3])
	}

	// item[4]: payload (tuple or buffer)
	p.Payload = tuple.Items[4]

	// item[5]: named payload (dict or None)
	switch tuple.Items[5].(type) {
	case marshal.PyNone:
		p.NamedPayload = nil
	case marshal.PyDict:
		p.NamedPayload = tuple.Items[5]
	default:
		return fmt.Errorf("pypacket: item[5] named_payload has unexpected type %T", tuple.Items[5])
	}

	return nil
}

// Encode serializes the PyPacket back into a PyRep ready for marshaling.
func (p *PyPacket) Encode() marshal.PyRep {
	t := marshal.PyTuple{Items: make([]marshal.PyRep, 6)}
	t.Items[0] = marshal.PyInt{Value: uint64(p.Type)}
	t.Items[1] = p.Source.Encode()
	t.Items[2] = p.Dest.Encode()
	if p.UserID == 0 {
		t.Items[3] = marshal.PyNone{}
	} else {
		t.Items[3] = marshal.PyInt{Value: p.UserID}
	}
	t.Items[4] = p.Payload
	if p.NamedPayload == nil {
		t.Items[5] = marshal.PyNone{}
	} else {
		t.Items[5] = p.NamedPayload
	}
	typeStr := p.TypeString
	if typeStr == "" {
		typeStr = "macho.MachoNetMsg"
	}
	return marshal.PyObject{Type: typeStr, Args: t}
}

// ---- helpers ----------------------------------------------------------------

func decodeService(rep marshal.PyRep, out *string) error {
	switch v := rep.(type) {
	case marshal.PyString:
		*out = v.Value
	case marshal.PyNone:
		*out = ""
	default:
		return fmt.Errorf("pyaddress: expected string/None for service, got %T", rep)
	}
	return nil
}

func decodeUint64(rep marshal.PyRep, out *uint64) error {
	switch v := rep.(type) {
	case marshal.PyInt:
		*out = v.Value
	case marshal.PyNone:
		*out = 0
	default:
		return fmt.Errorf("pyaddress: expected int/None for id, got %T", rep)
	}
	return nil
}

func encodeService(s string) marshal.PyRep {
	if s == "" {
		return marshal.PyNone{}
	}
	return marshal.PyString{Value: s}
}

func encodeUint64(v uint64) marshal.PyRep {
	if v == 0 {
		return marshal.PyNone{}
	}
	return marshal.PyInt{Value: v}
}
