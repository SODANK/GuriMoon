package marshal

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

// maxUnmarshalDepth bounds PyRep nesting across the whole decode, including
// substream re-entry. Real RMR payloads nest single digits; without a bound a
// stream of nested one-tuples is a stack overflow, which Go cannot recover.
const maxUnmarshalDepth = 100

// maxInflatedBytes bounds zlib decompression output. Frames arrive capped at
// net.MaxPacketSize (10 MB); without a bound a small compressed body can
// inflate to an arbitrarily large allocation.
const maxInflatedBytes = 64 << 20

// Unmarshal decodes an EVE marshal stream, raw (starting with 0x7E) or zlib-compressed.
// A compressed "special" packet has four zero bytes first and inflates from offset 12;
// a normal one inflates from offset 0.
func Unmarshal(body []byte) (PyRep, error) {
	return unmarshalNested(body, 0)
}

// unmarshalNested is Unmarshal with the nesting depth carried across
// substream re-entry, so recursion through opSubStream counts against the
// same budget as direct nesting.
func unmarshalNested(body []byte, depth int) (PyRep, error) {
	if len(body) == 0 {
		return nil, fmt.Errorf("unmarshal: empty body")
	}

	if body[0] != subStreamHeader {
		var err error
		if len(body) > 4 &&
			body[0] == 0 && body[1] == 0 && body[2] == 0 && body[3] == 0 {
			// Special packet: inflate from byte 12.
			if len(body) <= 12 {
				return nil, fmt.Errorf("unmarshal: special packet too short (%d bytes)", len(body))
			}
			body, err = inflate(body[12:])
		} else {
			body, err = inflate(body)
		}
		if err != nil {
			return nil, fmt.Errorf("unmarshal: inflate: %w", err)
		}
		if len(body) == 0 || body[0] != subStreamHeader {
			return nil, fmt.Errorf("unmarshal: not a valid marshal stream after decompression")
		}
	}

	// Skip the 0x7E header byte.
	body = body[1:]
	if len(body) < 4 {
		return nil, fmt.Errorf("unmarshal: stream too short for 4-byte preamble")
	}

	// Byte 0 is the save_count; bytes 1-3 are unknown (seem to always be 0).
	saveCount := body[0]
	body = body[4:]

	state := &unmarshalState{saveCount: saveCount, depth: depth}
	rep, _, err := unmarshalData(state, body)
	return rep, err
}

// unmarshalState tracks saved objects for Op_PySavedStreamElement back-references.
type unmarshalState struct {
	saveCount uint8
	saves     []PyRep
	depth     int
}

func inflate(data []byte) ([]byte, error) {
	r, err := zlib.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	out, err := io.ReadAll(io.LimitReader(r, maxInflatedBytes+1))
	if err != nil {
		return nil, err
	}
	if len(out) > maxInflatedBytes {
		return nil, fmt.Errorf("inflated payload exceeds %d bytes", maxInflatedBytes)
	}
	return out, nil
}

// unmarshalData decodes one PyRep from buf.
// Returns the decoded object and the number of bytes consumed.
func unmarshalData(state *unmarshalState, buf []byte) (PyRep, int, error) {
	if len(buf) == 0 {
		return nil, 0, fmt.Errorf("unmarshal: empty buffer")
	}
	if state.depth >= maxUnmarshalDepth {
		return nil, 0, fmt.Errorf("unmarshal: nesting exceeds depth %d", maxUnmarshalDepth)
	}
	state.depth++
	defer func() { state.depth-- }()

	rawOpcode := buf[0]
	buf = buf[1:]
	consumed := 1

	shouldSave := rawOpcode&saveMask != 0
	opcode := rawOpcode & opcodeMask

	var (
		rep PyRep
		n   int
		err error
	)

	switch opcode {

	// ── None ──────────────────────────────────────────────────────────────────
	case opNone:
		rep = PyNone{}

	// ── Integers ──────────────────────────────────────────────────────────────
	case opLongLong:
		if len(buf) < 8 {
			return nil, consumed, fmt.Errorf("opLongLong: need 8 bytes, have %d", len(buf))
		}
		rep = PyInt{Value: binary.LittleEndian.Uint64(buf[:8])}
		consumed += 8

	case opLong:
		if len(buf) < 4 {
			return nil, consumed, fmt.Errorf("opLong: need 4 bytes, have %d", len(buf))
		}
		// Sign-extend: the wire value is a signed int32 (EVEmu reads int32).
		// Negative values matter for e.g. market order ranges (rangeStation = -1).
		rep = PyInt{Value: uint64(int64(int32(binary.LittleEndian.Uint32(buf[:4]))))}
		consumed += 4

	case opSignedShort:
		if len(buf) < 2 {
			return nil, consumed, fmt.Errorf("opSignedShort: need 2 bytes, have %d", len(buf))
		}
		// Sign-extend into uint64 to preserve negative values.
		v := int16(binary.LittleEndian.Uint16(buf[:2]))
		rep = PyInt{Value: uint64(int64(v))}
		consumed += 2

	case opByte:
		if len(buf) < 1 {
			return nil, consumed, fmt.Errorf("opByte: need 1 byte")
		}
		// Sign-extend: the wire value is a signed int8 (EVEmu reads int8;
		// the marshaller only emits opByte for -128..127).
		rep = PyInt{Value: uint64(int64(int8(buf[0])))}
		consumed++

	case opInfinite:
		rep = PyInt{Value: math.MaxUint64}

	case opZeroInteger:
		rep = PyInt{Value: 0}

	case opOneInteger:
		rep = PyInt{Value: 1}

	// ── Floats ────────────────────────────────────────────────────────────────
	case opReal:
		if len(buf) < 8 {
			return nil, consumed, fmt.Errorf("opReal: need 8 bytes, have %d", len(buf))
		}
		rep = PyReal{Value: math.Float64frombits(binary.LittleEndian.Uint64(buf[:8]))}
		consumed += 8

	case opZeroReal:
		rep = PyReal{Value: 0.0}

	// ── Buffers ───────────────────────────────────────────────────────────────
	case opBuffer:
		dataLen, extra, err2 := readLength(buf)
		if err2 != nil {
			return nil, consumed, fmt.Errorf("opBuffer length: %w", err2)
		}
		buf = buf[extra:]
		consumed += extra
		if len(buf) < dataLen {
			return nil, consumed, fmt.Errorf("opBuffer: need %d bytes, have %d", dataLen, len(buf))
		}
		data := make([]byte, dataLen)
		copy(data, buf[:dataLen])
		rep = PyBuffer{Data: data}
		consumed += dataLen

	// ── Strings ───────────────────────────────────────────────────────────────
	case opEmptyString:
		rep = PyString{Value: ""}

	case opCharString:
		if len(buf) < 1 {
			return nil, consumed, fmt.Errorf("opCharString: need 1 byte")
		}
		rep = PyString{Value: string(buf[:1])}
		consumed++

	case opByteString: // type-1 string
		if len(buf) < 1 {
			return nil, consumed, fmt.Errorf("opByteString: missing length")
		}
		strLen := int(buf[0])
		buf = buf[1:]
		consumed++
		if len(buf) < strLen {
			return nil, consumed, fmt.Errorf("opByteString: need %d bytes, have %d", strLen, len(buf))
		}
		rep = PyString{Value: string(buf[:strLen]), IsType1: true}
		consumed += strLen

	case opByteString2: // type-2 string (most common)
		strLen, extra, err2 := readLength(buf)
		if err2 != nil {
			return nil, consumed, fmt.Errorf("opByteString2 length: %w", err2)
		}
		buf = buf[extra:]
		consumed += extra
		if len(buf) < strLen {
			return nil, consumed, fmt.Errorf("opByteString2: need %d bytes, have %d", strLen, len(buf))
		}
		rep = PyString{Value: string(buf[:strLen])}
		consumed += strLen

	case opStringTableItem:
		if len(buf) < 1 {
			return nil, consumed, fmt.Errorf("opStringTableItem: need 1 byte")
		}
		idx := buf[0]
		consumed++
		s := globalStringTable.Load().LookupIndex(idx)
		if s == "" {
			rep = PyString{Value: fmt.Sprintf("InvalidStringTableItem(%d)", idx)}
		} else {
			rep = PyString{Value: s}
		}

	case opUnicodeByteString:
		// 1-byte character count; actual bytes = count * 2 (UTF-16 LE).
		if len(buf) < 1 {
			return nil, consumed, fmt.Errorf("opUnicodeByteString: need 1 byte")
		}
		charCount := int(buf[0])
		buf = buf[1:]
		consumed++
		byteLen := charCount * 2
		if len(buf) < byteLen {
			return nil, consumed, fmt.Errorf("opUnicodeByteString: need %d bytes, have %d", byteLen, len(buf))
		}
		rep = PyString{Value: string(buf[:byteLen])}
		consumed += byteLen

	case opEmptyUnicodeString:
		rep = PyString{Value: ""}

	case opUnicodeCharString:
		// Single UTF-16 LE character (2 bytes).
		if len(buf) < 2 {
			return nil, consumed, fmt.Errorf("opUnicodeCharString: need 2 bytes, have %d", len(buf))
		}
		rep = PyString{Value: string(buf[:2])}
		consumed += 2

	case opUnicodeString:
		strLen, extra, err2 := readLength(buf)
		if err2 != nil {
			return nil, consumed, fmt.Errorf("opUnicodeString length: %w", err2)
		}
		buf = buf[extra:]
		consumed += extra
		if len(buf) < strLen {
			return nil, consumed, fmt.Errorf("opUnicodeString: need %d bytes, have %d", strLen, len(buf))
		}
		rep = PyString{Value: string(buf[:strLen])}
		consumed += strLen

	// ── Booleans ──────────────────────────────────────────────────────────────
	case opTrue:
		rep = PyBool{Value: true}

	case opFalse:
		rep = PyBool{Value: false}

	// ── Tuples ────────────────────────────────────────────────────────────────
	case opEmptyTuple:
		rep = PyTuple{}

	case opOneTuple:
		item, sz, err2 := unmarshalData(state, buf)
		if err2 != nil {
			return nil, consumed, fmt.Errorf("opOneTuple: %w", err2)
		}
		rep = PyTuple{Items: []PyRep{item}}
		consumed += sz

	case opTwoTuple:
		items := make([]PyRep, 2)
		pos := 0
		for i := 0; i < 2; i++ {
			item, sz, err2 := unmarshalData(state, buf[pos:])
			if err2 != nil {
				return nil, consumed, fmt.Errorf("opTwoTuple item %d: %w", i, err2)
			}
			items[i] = item
			pos += sz
		}
		rep = PyTuple{Items: items}
		consumed += pos

	case opTuple:
		count, extra, err2 := readLength(buf)
		if err2 != nil {
			return nil, consumed, fmt.Errorf("opTuple length: %w", err2)
		}
		buf = buf[extra:]
		consumed += extra
		rep, n, err = readSequence(state, buf, count)
		consumed += n
		if err == nil {
			rep = PyTuple{Items: rep.(PyList).Items}
		}

	// ── Lists ─────────────────────────────────────────────────────────────────
	case opEmptyList:
		rep = PyList{}

	case opOneList:
		item, sz, err2 := unmarshalData(state, buf)
		if err2 != nil {
			return nil, consumed, fmt.Errorf("opOneList: %w", err2)
		}
		rep = PyList{Items: []PyRep{item}}
		consumed += sz

	case opList:
		count, extra, err2 := readLength(buf)
		if err2 != nil {
			return nil, consumed, fmt.Errorf("opList length: %w", err2)
		}
		buf = buf[extra:]
		consumed += extra
		rep, n, err = readSequence(state, buf, count)
		consumed += n

	// ── Dicts ─────────────────────────────────────────────────────────────────
	case opDict:
		count, extra, err2 := readLength(buf)
		if err2 != nil {
			return nil, consumed, fmt.Errorf("opDict length: %w", err2)
		}
		buf = buf[extra:]
		consumed += extra
		rep, n, err = readDict(state, buf, count)
		consumed += n

	// ── Object ────────────────────────────────────────────────────────────────
	case opObject:
		typeRep, n1, err2 := unmarshalData(state, buf)
		if err2 != nil {
			return nil, consumed, fmt.Errorf("opObject type: %w", err2)
		}
		buf = buf[n1:]
		consumed += n1
		typeStr, ok := typeRep.(PyString)
		if !ok {
			return nil, consumed, fmt.Errorf("opObject: type is not a string (got %T)", typeRep)
		}
		args, n2, err2 := unmarshalData(state, buf)
		if err2 != nil {
			return nil, consumed, fmt.Errorf("opObject args: %w", err2)
		}
		rep = PyObject{Type: typeStr.Value, Args: args}
		consumed += n2

	// ── SubStruct ─────────────────────────────────────────────────────────────
	case opSubStruct:
		inner, sz, err2 := unmarshalData(state, buf)
		if err2 != nil {
			return nil, consumed, fmt.Errorf("opSubStruct: %w", err2)
		}
		rep = PySubStruct{Value: inner}
		consumed += sz

	// ── Saved stream element (back-reference) ─────────────────────────────────
	case opSavedStreamElement:
		if len(buf) < 1 {
			return nil, consumed, fmt.Errorf("opSavedStreamElement: need 1 byte")
		}
		idx := int(buf[0])
		consumed++
		if idx == 0 {
			return nil, consumed, fmt.Errorf("opSavedStreamElement: index 0 is invalid")
		}
		if idx > int(state.saveCount) {
			return nil, consumed, fmt.Errorf("opSavedStreamElement: index %d > saveCount %d", idx, state.saveCount)
		}
		if idx > len(state.saves) {
			return nil, consumed, fmt.Errorf("opSavedStreamElement: index %d not yet saved (%d saved so far)", idx, len(state.saves))
		}
		rep = state.saves[idx-1] // return same pointer; treat as read-only

	// ── Checksumed stream ─────────────────────────────────────────────────────
	case opChecksumedStream:
		if len(buf) < 4 {
			return nil, consumed, fmt.Errorf("opChecksumedStream: need 4 bytes, have %d", len(buf))
		}
		checksum := binary.LittleEndian.Uint32(buf[:4])
		buf = buf[4:]
		consumed += 4
		inner, sz, err2 := unmarshalData(state, buf)
		if err2 != nil {
			return nil, consumed, fmt.Errorf("opChecksumedStream inner: %w", err2)
		}
		rep = PyChecksumedStream{Checksum: checksum, Value: inner}
		consumed += sz

	// ── SubStream ─────────────────────────────────────────────────────────────
	case opSubStream:
		dataLen, extra, err2 := readLength(buf)
		if err2 != nil {
			return nil, consumed, fmt.Errorf("opSubStream length: %w", err2)
		}
		buf = buf[extra:]
		consumed += extra
		if len(buf) < dataLen {
			return nil, consumed, fmt.Errorf("opSubStream: need %d bytes, have %d", dataLen, len(buf))
		}
		raw := make([]byte, dataLen)
		copy(raw, buf[:dataLen])
		consumed += dataLen
		// Attempt to decode the nested stream; store raw bytes regardless.
		ss := &PySubStream{Data: raw}
		if inner, err2 := unmarshalNested(raw, state.depth); err2 == nil {
			ss.Decoded = inner
		}
		rep = ss

	// ── PackedRow ─────────────────────────────────────────────────────────────
	case opPackedRow:
		// Wire: header (any PyRep) + length-prefixed zero-compressed row data.
		header, n1, err2 := unmarshalData(state, buf)
		if err2 != nil {
			return nil, consumed, fmt.Errorf("opPackedRow header: %w", err2)
		}
		buf = buf[n1:]
		consumed += n1

		dataLen, extra, err2 := readLength(buf)
		if err2 != nil {
			return nil, consumed, fmt.Errorf("opPackedRow data length: %w", err2)
		}
		buf = buf[extra:]
		consumed += extra
		if len(buf) < dataLen {
			return nil, consumed, fmt.Errorf("opPackedRow: need %d bytes, have %d", dataLen, len(buf))
		}
		data := make([]byte, dataLen)
		copy(data, buf[:dataLen])
		rep = PyPackedRow{Header: header, Data: data}
		consumed += dataLen

	// ── PackedRowHeader ───────────────────────────────────────────────────────
	case opPackedRowHeader:
		// Wire: a 2-tuple (DBRowDescriptor, args) then rows terminated by two
		// consecutive opPackedTerminator (0x2D) markers.
		inner, sz, err2 := unmarshalData(state, buf)
		if err2 != nil {
			return nil, consumed, fmt.Errorf("opPackedRowHeader tuple: %w", err2)
		}
		buf = buf[sz:]
		consumed += sz

		t, ok := inner.(PyTuple)
		if !ok || len(t.Items) != 2 {
			return nil, consumed, fmt.Errorf("opPackedRowHeader: expected 2-tuple, got %T len=%d", inner, func() int {
				if tt, ok2 := inner.(PyTuple); ok2 {
					return len(tt.Items)
				}
				return -1
			}())
		}

		rh := PyPackedRowHeader{
			HeaderType: t.Items[0],
			Arguments:  t.Items[1],
		}

		terminators := 0
		for terminators < 2 && len(buf) > 0 {
			row, rowSz, err2 := unmarshalData(state, buf)
			if err2 != nil {
				break
			}
			buf = buf[rowSz:]
			consumed += rowSz
			if _, isNone := row.(PyNone); isNone {
				// opPackedTerminator (0x2D) is decoded as PyNone
				terminators++
				continue
			}
			terminators = 0
			rh.Rows = append(rh.Rows, row)
		}
		rep = rh

	// ── PackedResultSet ───────────────────────────────────────────────────────
	case opPackedResultSet:
		// Similar structure to PackedRowHeader; store header + treat rows as a list.
		inner, sz, err2 := unmarshalData(state, buf)
		if err2 != nil {
			return nil, consumed, fmt.Errorf("opPackedResultSet header: %w", err2)
		}
		buf = buf[sz:]
		consumed += sz

		rh := PyPackedRowHeader{} // reuse the struct
		if t, ok := inner.(PyTuple); ok && len(t.Items) == 2 {
			rh.HeaderType = t.Items[0]
			rh.Arguments = t.Items[1]
		}

		terminators := 0
		for terminators < 2 && len(buf) > 0 {
			row, rowSz, err2 := unmarshalData(state, buf)
			if err2 != nil {
				break
			}
			buf = buf[rowSz:]
			consumed += rowSz
			if _, isNone := row.(PyNone); isNone {
				terminators++
				continue
			}
			terminators = 0
			rh.Rows = append(rh.Rows, row)
		}
		rep = rh

	// ── PackedTerminator ──────────────────────────────────────────────────────
	case opPackedTerminator:
		// Sentinel byte inside packed row streams; surface as PyNone.
		rep = PyNone{}

	// ── Variable-length integer ───────────────────────────────────────────────
	case opVarInteger:
		if len(buf) < 1 {
			return nil, consumed, fmt.Errorf("opVarInteger: need at least 1 byte")
		}
		first := buf[0]
		consumed++
		switch first {
		case 0xFF:
			if len(buf) < 9 {
				return nil, consumed, fmt.Errorf("opVarInteger(8-byte): need 8 more bytes")
			}
			rep = PyInt{Value: binary.LittleEndian.Uint64(buf[1:9])}
			consumed += 8
		case 0xFE:
			if len(buf) < 5 {
				return nil, consumed, fmt.Errorf("opVarInteger(4-byte): need 4 more bytes")
			}
			rep = PyInt{Value: uint64(binary.LittleEndian.Uint32(buf[1:5]))}
			consumed += 4
		default:
			rep = PyInt{Value: uint64(first)}
		}

	default:
		return nil, consumed, fmt.Errorf("unmarshal: unhandled opcode 0x%02x", opcode)
	}

	if err != nil {
		return nil, consumed, err
	}

	if shouldSave && rep != nil {
		state.saves = append(state.saves, rep)
	}

	return rep, consumed, nil
}

// readLength reads the EVE length field: a single byte, or 0xFF followed by a 4-byte
// little-endian uint32 for lengths >= 255.
func readLength(buf []byte) (length int, consumed int, err error) {
	if len(buf) < 1 {
		return 0, 0, fmt.Errorf("readLength: buffer empty")
	}
	n := int(buf[0])
	consumed = 1
	if n == 0xFF {
		if len(buf) < 5 {
			return 0, 1, fmt.Errorf("readLength: extended form needs 4 bytes, have %d", len(buf)-1)
		}
		n = int(binary.LittleEndian.Uint32(buf[1:5]))
		consumed = 5
	}
	return n, consumed, nil
}

// readSequence reads count elements from buf, returning them as a PyList.
func readSequence(state *unmarshalState, buf []byte, count int) (PyRep, int, error) {
	// Each element consumes at least one byte, so a count beyond the remaining buffer is
	// malformed; reject it before it sizes the allocation below.
	if count > len(buf) {
		return nil, 0, fmt.Errorf("sequence count %d exceeds remaining %d bytes", count, len(buf))
	}
	items := make([]PyRep, 0, count)
	consumed := 0
	for i := 0; i < count; i++ {
		item, n, err := unmarshalData(state, buf[consumed:])
		if err != nil {
			return nil, consumed, fmt.Errorf("sequence item %d: %w", i, err)
		}
		items = append(items, item)
		consumed += n
	}
	return PyList{Items: items}, consumed, nil
}

// readDict reads count key-value pairs from buf.
// On the wire the order is: value first, then key (EVE reverses the natural order).
func readDict(state *unmarshalState, buf []byte, count int) (PyRep, int, error) {
	// Each pair consumes at least two bytes (value + key opcodes).
	if count > len(buf)/2 {
		return nil, 0, fmt.Errorf("dict count %d exceeds remaining %d bytes", count, len(buf))
	}
	entries := make([]DictEntry, 0, count)
	consumed := 0
	for i := 0; i < count; i++ {
		val, n1, err := unmarshalData(state, buf[consumed:])
		if err != nil {
			return nil, consumed, fmt.Errorf("dict item %d value: %w", i, err)
		}
		consumed += n1

		key, n2, err := unmarshalData(state, buf[consumed:])
		if err != nil {
			return nil, consumed, fmt.Errorf("dict item %d key: %w", i, err)
		}
		consumed += n2

		entries = append(entries, DictEntry{Key: key, Value: val})
	}
	return PyDict{Items: entries}, consumed, nil
}
