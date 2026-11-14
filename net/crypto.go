// Everything in this file is DRAGON-mode only. The Blood client (MachoNetVersion 139,
// EVEVersionNumber 2.10) does no crypto negotiation: server and client swap VersionExchange
// tuples and then exchange PyPackets directly, with no CryptoRequest, Challenge or Ack.
// Kept for reference in case DRAGON support is needed.
package net

import (
	"fmt"

	"gurimoon/marshal"
)

// CryptoRequest is the second packet sent by the client (after VersionExchange): a 2-element
// PyTuple [keyVersion_string, keyParams_dict]. EVEmu supports only the "placebo" keyVersion.
type CryptoRequest struct {
	KeyVersion string
}

// Decode parses a CryptoRequest from a 2-element PyTuple.
func (c *CryptoRequest) Decode(rep marshal.PyRep) error {
	t, ok := rep.(marshal.PyTuple)
	if !ok || len(t.Items) < 1 {
		return fmt.Errorf("crypto request: expected tuple, got %T", rep)
	}
	s, ok := t.Items[0].(marshal.PyString)
	if !ok {
		return fmt.Errorf("crypto request: keyVersion is not a string, got %T", t.Items[0])
	}
	c.KeyVersion = s.Value
	return nil
}

// CryptoChallenge carries the client's credentials.
// Wire format: PySubStream containing a 2-tuple [clientChallenge, paramsDict].
type CryptoChallenge struct {
	ClientChallenge string
	Username        string
	LanguageID      string
	UserAffiliate   uint64
	// MachoVersion / boot fields from the client's own dict are available but
	// rarely needed for basic connection validation.
}

// Decode parses a CryptoChallenge from a *PySubStream.
func (c *CryptoChallenge) Decode(rep marshal.PyRep) error {
	ss, ok := rep.(*marshal.PySubStream)
	if !ok {
		return fmt.Errorf("crypto challenge: expected *PySubStream, got %T", rep)
	}
	if ss.Decoded == nil {
		return fmt.Errorf("crypto challenge: substream has no decoded content")
	}
	t, ok := ss.Decoded.(marshal.PyTuple)
	if !ok || len(t.Items) < 2 {
		return fmt.Errorf("crypto challenge: expected 2-tuple inside substream, got %T (len=%d)",
			ss.Decoded, tupleLen(ss.Decoded))
	}

	// item[0]: clientChallenge
	if s, ok := t.Items[0].(marshal.PyString); ok {
		c.ClientChallenge = s.Value
	}

	// item[1]: params dict
	d, ok := t.Items[1].(marshal.PyDict)
	if !ok {
		return fmt.Errorf("crypto challenge: params is not a PyDict, got %T", t.Items[1])
	}
	if v := dictGet(d, "user_name"); v != nil {
		if s, ok := v.(marshal.PyString); ok {
			c.Username = s.Value
		}
	}
	if v := dictGet(d, "user_languageid"); v != nil {
		if s, ok := v.(marshal.PyString); ok {
			c.LanguageID = s.Value
		}
	}
	if v := dictGet(d, "user_affiliateid"); v != nil {
		_ = decodeUint64(v, &c.UserAffiliate) // best-effort: an undecodable value leaves it 0
	}
	return nil
}

// BuildServerHandshake constructs the CryptoServerHandshake PyRep.
// Wire: PySubStream( 4-tuple [serverChallenge, func, context, responseDict] ).
// Mirrors the C++ CryptoServerHandshake::Encode() in EVEPresentation.cpp.
func BuildServerHandshake(machoVer uint64, bootVer float64, bootBuild uint64, codename, region string) marshal.PyRep {
	responseDict := marshal.PyDict{Items: []marshal.DictEntry{
		{Key: marshal.PyString{Value: "challenge_responsehash"}, Value: marshal.PyString{Value: "654"}},
		{Key: marshal.PyString{Value: "macho_version"}, Value: marshal.PyInt{Value: machoVer}},
		{Key: marshal.PyString{Value: "boot_version"}, Value: marshal.PyReal{Value: bootVer}},
		{Key: marshal.PyString{Value: "boot_build"}, Value: marshal.PyInt{Value: bootBuild}},
		{Key: marshal.PyString{Value: "boot_codename"}, Value: marshal.PyString{Value: codename}},
		{Key: marshal.PyString{Value: "boot_region"}, Value: marshal.PyString{Value: region}},
		{Key: marshal.PyString{Value: "cluster_usercount"}, Value: marshal.PyInt{Value: 2}},
		{Key: marshal.PyString{Value: "proxy_nodeid"}, Value: marshal.PyInt{Value: 0xFFAA}},
		{Key: marshal.PyString{Value: "user_logonqueueposition"}, Value: marshal.PyInt{Value: 1}},
	}}
	inner := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyString{Value: "hi"}, // serverChallenge
		marshal.PyString{Value: "1"},  // func (arbitrary Python snippet)
		marshal.PyDict{},              // context (empty)
		responseDict,
	}}
	return &marshal.PySubStream{Decoded: inner}
}

// BuildHandshakeAck constructs the session_init acknowledgement PyRep.
// Wire: PySubStream( dict{ "session_init": dict{ "userid": userID } } ).
// Mirrors the C++ HandshakeAck in EVEPresentation.cpp (CryptoHandshakeSent state).
func BuildHandshakeAck(userID uint64) marshal.PyRep {
	si := marshal.PyDict{Items: []marshal.DictEntry{
		{Key: marshal.PyString{Value: "userid"}, Value: marshal.PyInt{Value: userID}},
	}}
	d := marshal.PyDict{Items: []marshal.DictEntry{
		{Key: marshal.PyString{Value: "session_init"}, Value: si},
	}}
	return &marshal.PySubStream{Decoded: d}
}

// ---- helpers ----------------------------------------------------------------

// dictGet returns the value for key in d, or nil if not found.
func dictGet(d marshal.PyDict, key string) marshal.PyRep {
	for _, e := range d.Items {
		if s, ok := e.Key.(marshal.PyString); ok && s.Value == key {
			return e.Value
		}
	}
	return nil
}

func tupleLen(rep marshal.PyRep) int {
	if t, ok := rep.(marshal.PyTuple); ok {
		return len(t.Items)
	}
	return -1
}
