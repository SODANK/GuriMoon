package marshal

import (
	"encoding/binary"
	"fmt"
	"math"
)

// Marshal encodes rep into the EVE wire format: the 0x7E stream header, a 4-byte preamble
// (save_count byte + 3 zero bytes) as Unmarshal expects, then the value. The output is not
// zlib-compressed; the client accepts uncompressed streams.
func Marshal(rep PyRep) ([]byte, error) {
	out := make([]byte, 0, 64)
	out = append(out, subStreamHeader)
	out = append(out, 0, 0, 0, 0) // save_count (0) + 3 unknown bytes
	return marshalRep(out, rep)
}

// marshalRep appends the wire encoding of rep to out and returns the result.
func marshalRep(out []byte, rep PyRep) ([]byte, error) {
	switch v := rep.(type) {

	case PyNone:
		out = append(out, opNone)

	case PyInt:
		out = appendInt(out, v.Value)

	case PyReal:
		if v.Value == 0.0 {
			out = append(out, opZeroReal)
		} else {
			out = append(out, opReal)
			out = binary.LittleEndian.AppendUint64(out, math.Float64bits(v.Value))
		}

	case PyBool:
		if v.Value {
			out = append(out, opTrue)
		} else {
			out = append(out, opFalse)
		}

	case PyBuffer:
		out = append(out, opBuffer)
		out = appendLength(out, len(v.Data))
		out = append(out, v.Data...)

	case PyString:
		out = appendString(out, v)

	case PyTuple:
		out = appendTupleHeader(out, len(v.Items))
		var err error
		for _, item := range v.Items {
			out, err = marshalRep(out, item)
			if err != nil {
				return nil, err
			}
		}

	case PyList:
		out = appendListHeader(out, len(v.Items))
		var err error
		for _, item := range v.Items {
			out, err = marshalRep(out, item)
			if err != nil {
				return nil, err
			}
		}

	case PyDict:
		out = append(out, opDict)
		out = appendLength(out, len(v.Items))
		var err error
		for _, entry := range v.Items {
			// Wire order: value first, then key.
			out, err = marshalRep(out, entry.Value)
			if err != nil {
				return nil, err
			}
			out, err = marshalRep(out, entry.Key)
			if err != nil {
				return nil, err
			}
		}

	case PyObject:
		out = append(out, opObject)
		var err error
		out, err = marshalRep(out, PyString{Value: v.Type})
		if err != nil {
			return nil, err
		}
		out, err = marshalRep(out, v.Args)
		if err != nil {
			return nil, err
		}

	case PySubStruct:
		out = append(out, opSubStruct)
		var err error
		out, err = marshalRep(out, v.Value)
		if err != nil {
			return nil, err
		}

	case *PySubStream:
		out = append(out, opSubStream)
		if v.Decoded != nil {
			inner, err := Marshal(v.Decoded)
			if err != nil {
				return nil, fmt.Errorf("PySubStream re-marshal: %w", err)
			}
			out = appendLength(out, len(inner))
			out = append(out, inner...)
		} else if len(v.Data) > 0 {
			out = appendLength(out, len(v.Data))
			out = append(out, v.Data...)
		} else {
			out = append(out, 0) // empty substream
		}

	case PyChecksumedStream:
		out = append(out, opChecksumedStream)
		out = binary.LittleEndian.AppendUint32(out, v.Checksum)
		var err error
		out, err = marshalRep(out, v.Value)
		if err != nil {
			return nil, err
		}

	case PyPackedRow:
		out = append(out, opPackedRow)
		var err error
		out, err = marshalRep(out, v.Header)
		if err != nil {
			return nil, err
		}
		out = appendLength(out, len(v.Data))
		out = append(out, v.Data...)

	case PyPackedRowHeader:
		out = append(out, opPackedRowHeader)
		// Emit the 2-tuple (HeaderType, Arguments) inline.
		out = append(out, opTwoTuple)
		var err error
		out, err = marshalRep(out, v.HeaderType)
		if err != nil {
			return nil, err
		}
		out, err = marshalRep(out, v.Arguments)
		if err != nil {
			return nil, err
		}
		for _, row := range v.Rows {
			out, err = marshalRep(out, row)
			if err != nil {
				return nil, err
			}
		}
		// Two terminator bytes close the row list.
		out = append(out, opPackedTerminator, opPackedTerminator)

	default:
		return nil, fmt.Errorf("marshal: unknown type %T", rep)
	}

	return out, nil
}

// appendInt writes the most compact encoding for a uint64 integer.
func appendInt(out []byte, v uint64) []byte {
	switch {
	case v > 0x7FFFFFFF:
		// opLong/opSignedShort are signed on the wire, so anything past int32 range (including uint64
		// two's-complement negatives) uses the 8-byte form.
		out = append(out, opLongLong)
		out = binary.LittleEndian.AppendUint64(out, v)
	case v > 0x7FFF:
		out = append(out, opLong)
		out = binary.LittleEndian.AppendUint32(out, uint32(v))
	case v > 1:
		out = append(out, opSignedShort)
		out = binary.LittleEndian.AppendUint16(out, uint16(v))
	case v == 1:
		out = append(out, opOneInteger)
	default: // 0
		out = append(out, opZeroInteger)
	}
	return out
}

// appendString writes the most compact string encoding.
func appendString(out []byte, s PyString) []byte {
	if s.IsType1 {
		out = append(out, opByteString)
		out = appendLength(out, len(s.Value))
		out = append(out, s.Value...)
		return out
	}
	switch len(s.Value) {
	case 0:
		out = append(out, opEmptyString)
	case 1:
		out = append(out, opCharString, s.Value[0])
	default:
		if idx, ok := globalStringTable.Load().LookupString(s.Value); ok {
			out = append(out, opStringTableItem, idx)
		} else if len(s.Value) < 0xFF {
			// Short string: opByteString2 with 1-byte length (C++ EVEMarshal behavior).
			out = append(out, opByteString2, byte(len(s.Value)))
			out = append(out, s.Value...)
		} else {
			// Long string (>= 255 bytes): opUnicodeString with extended length, as C++ EVEMarshal.cpp does;
			// the Blood-era client does not support extended-length opByteString2.
			out = append(out, opUnicodeString)
			out = appendLength(out, len(s.Value))
			out = append(out, s.Value...)
		}
	}
	return out
}

// appendTupleHeader writes the opcode (and length if needed) for a tuple.
// The caller is responsible for writing the individual items afterward.
func appendTupleHeader(out []byte, count int) []byte {
	switch count {
	case 0:
		out = append(out, opEmptyTuple)
	case 1:
		out = append(out, opOneTuple)
	case 2:
		out = append(out, opTwoTuple)
	default:
		out = append(out, opTuple)
		out = appendLength(out, count)
	}
	return out
}

// appendListHeader writes the opcode (and length if needed) for a list.
func appendListHeader(out []byte, count int) []byte {
	switch count {
	case 0:
		out = append(out, opEmptyList)
	case 1:
		out = append(out, opOneList)
	default:
		out = append(out, opList)
		out = appendLength(out, count)
	}
	return out
}

// appendLength writes the EVE-style length field.
// Lengths < 255 are written as a single byte; ≥ 255 are written as 0xFF + uint32 LE.
func appendLength(out []byte, n int) []byte {
	if n >= 0xFF {
		out = append(out, 0xFF)
		out = binary.LittleEndian.AppendUint32(out, uint32(n))
	} else {
		out = append(out, byte(n))
	}
	return out
}
