package db

import (
	"encoding/binary"
	"fmt"
	"math"

	"gurimoon/marshal"
)

// ── Packed rows (blue.DBRow) ──────────────────────────────────────────────────
//
// Some client rowsets must be blue.DBRow, not util.Rowset: readers call row.__keys__, assign by
// column index or feed dbutil.RowList.append (ui/services/marketsvc.py GetBestMatchableBid),
// and a util.Row fails with AttributeError or "ValueError: Not DBRow".
// A blue.DBRow is a DBRowDescriptor plus one zero-compressed blob per row. The blob is not in
// declared order: fields are written largest first (8-, 4-, 2-, 1-byte, declared order inside
// each class), then every boolean as one bit. A wrong order decodes as garbage without an error.

// DBTYPE constants used in blue.DBRowDescriptor column descriptors.
const (
	dbtypeI2       = uint64(0x02) // int16
	dbtypeI4       = uint64(0x03) // int32
	dbtypeR4       = uint64(0x04) // float32
	dbtypeR8       = uint64(0x05) // float64
	dbtypeCY       = uint64(0x06) // currency (int64 * 10000)
	dbtypeBool     = uint64(0x0B) // single bit
	dbtypeI1       = uint64(0x10) // int8
	dbtypeUI1      = uint64(0x11) // uint8
	dbtypeUI2      = uint64(0x12) // uint16
	dbtypeUI4      = uint64(0x13) // uint32
	dbtypeI8       = uint64(0x14) // int64
	dbtypeUI8      = uint64(0x15) // uint64
	dbtypeFiletime = uint64(0x40) // Windows FILETIME (uint64)
)

// packedCol is one column of a DBRowDescriptor.
type packedCol struct {
	name   string
	dbtype uint64
}

// fieldBits reports the packed width of a DBTYPE in bits.
func (c packedCol) fieldBits() int {
	switch c.dbtype {
	case dbtypeI8, dbtypeUI8, dbtypeR8, dbtypeCY, dbtypeFiletime:
		return 64
	case dbtypeI4, dbtypeUI4, dbtypeR4:
		return 32
	case dbtypeI2, dbtypeUI2:
		return 16
	case dbtypeI1, dbtypeUI1:
		return 8
	case dbtypeBool:
		return 1
	}
	return 0
}

// packedDescriptor builds the blue.DBRowDescriptor for cols.
func packedDescriptor(cols []packedCol) marshal.PyPackedRowHeader {
	pairs := make([]marshal.PyRep, len(cols))
	for i, c := range cols {
		pairs[i] = marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyString{Value: c.name},
			marshal.PyInt{Value: c.dbtype},
		}}
	}
	return marshal.PyPackedRowHeader{
		HeaderType: marshal.PyString{Value: "blue.DBRowDescriptor", IsType1: true},
		Arguments: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyTuple{Items: pairs},
		}},
	}
}

// packedValue is one field value. Only the kinds the descriptors here use are
// accepted; anything else panics at build time rather than shipping a row the
// client decodes as garbage.
func packedFieldBytes(c packedCol, v any) uint64 {
	switch c.dbtype {
	case dbtypeCY:
		return uint64(int64(math.Round(toFloat(v) * 10000)))
	case dbtypeR8:
		return math.Float64bits(toFloat(v))
	case dbtypeR4:
		return uint64(math.Float32bits(float32(toFloat(v))))
	case dbtypeBool:
		if toInt(v) != 0 {
			return 1
		}
		return 0
	default:
		return uint64(toInt(v))
	}
}

func toFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case float32:
		return float64(x)
	case int64:
		return float64(x)
	case int:
		return float64(x)
	case bool:
		if x {
			return 1
		}
		return 0
	}
	panic(fmt.Sprintf("packrow: %T is not a number", v))
}

func toInt(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case float64:
		return int64(x)
	case bool:
		if x {
			return 1
		}
		return 0
	}
	panic(fmt.Sprintf("packrow: %T is not an integer", v))
}

// packRow encodes vals (in declared column order) into the zero-compressed
// blue.DBRow payload: size classes descending, then the bools bit-packed.
func packRow(cols []packedCol, vals []any) []byte {
	size := 0
	nbool := 0
	for _, c := range cols {
		if b := c.fieldBits(); b >= 8 {
			size += b / 8
		} else {
			nbool++
		}
	}
	raw := make([]byte, size+(nbool+7)/8)

	off := 0
	for _, bits := range []int{64, 32, 16, 8} {
		for i, c := range cols {
			if c.fieldBits() != bits {
				continue
			}
			word := packedFieldBytes(c, vals[i])
			switch bits {
			case 64:
				binary.LittleEndian.PutUint64(raw[off:], word)
			case 32:
				binary.LittleEndian.PutUint32(raw[off:], uint32(word))
			case 16:
				binary.LittleEndian.PutUint16(raw[off:], uint16(word))
			case 8:
				raw[off] = byte(word)
			}
			off += bits / 8
		}
	}
	bit := 0
	for i, c := range cols {
		if c.fieldBits() != 1 {
			continue
		}
		if packedFieldBytes(c, vals[i]) != 0 {
			raw[size+bit/8] |= 1 << uint(bit%8)
		}
		bit++
	}
	return marshal.PackZeroCompressed(raw)
}

// makePackedRowList builds a dbutil.RowList of blue.DBRows: an ObjectEx whose
// constructor argument is the (descriptor, []) pair dbutil.RowsInit reads its
// columns off, with the rows themselves as the object's list items.
func makePackedRowList(cols []packedCol, rows [][]any) marshal.PyRep {
	desc := packedDescriptor(cols)
	packed := make([]marshal.PyRep, len(rows))
	for i, vals := range rows {
		packed[i] = marshal.PyPackedRow{Header: desc, Data: packRow(cols, vals)}
	}
	return marshal.PyPackedRowHeader{
		HeaderType: marshal.PyString{Value: "dbutil.RowList", IsType1: true},
		Arguments: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyTuple{Items: []marshal.PyRep{desc, marshal.PyList{}}},
		}},
		Rows: packed,
	}
}
