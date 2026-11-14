package marshal

// Wire opcodes from EVEMarshalOpcodes.h. Each occupies bits 0-5 of the opcode byte; bit 6
// (saveMask) marks the object for later back-reference; bit 7 (unknownMask) is unused.
const (
	opNone        byte = 0x01
	opByteString  byte = 0x02 // type-1 string: 1-byte length prefix
	opLongLong    byte = 0x03 // uint64, little-endian
	opLong        byte = 0x04 // uint32, little-endian
	opSignedShort byte = 0x05 // int16, little-endian
	opByte        byte = 0x06 // uint8
	opInfinite    byte = 0x07 // infinity sentinel
	opZeroInteger byte = 0x08 // integer 0
	opOneInteger  byte = 0x09 // integer 1
	opReal        byte = 0x0A // float64, little-endian
	opZeroReal    byte = 0x0B // float64 0.0
	// 0x0C unused
	opBuffer            byte = 0x0D // raw bytes: 1-byte length (0xFF → 4-byte ext)
	opEmptyString       byte = 0x0E // ""
	opCharString        byte = 0x0F // single ASCII character
	opByteString2       byte = 0x10 // type-2 string: 1-byte length (0xFF → 4-byte ext)
	opStringTableItem   byte = 0x11 // 1-byte index into the string table
	opUnicodeByteString byte = 0x12 // UTF-16 LE: 1-byte char count (byte count = count*2)
	// 0x13 unhandled
	opTuple  byte = 0x14 // variable tuple: 1-byte count (0xFF → 4-byte ext)
	opList   byte = 0x15 // variable list:  1-byte count (0xFF → 4-byte ext)
	opDict   byte = 0x16 // dict: 1-byte count; each entry is value then key
	opObject byte = 0x17 // Python object: type-string then args
	// 0x18 unhandled
	opSubStruct byte = 0x19 // nested object (remote object spec)
	// 0x1A unhandled
	opSavedStreamElement byte = 0x1B // back-reference: 1-byte 1-based index into saves
	opChecksumedStream   byte = 0x1C // 4-byte checksum then one child object
	// 0x1D-0x1E unused
	opTrue  byte = 0x1F
	opFalse byte = 0x20
	// 0x21 unhandled
	opPackedRowHeader    byte = 0x22 // DBRowDescriptor + packed row data
	opPackedResultSet    byte = 0x23 // like PackedRowHeader
	opEmptyTuple         byte = 0x24 // 0-element tuple
	opOneTuple           byte = 0x25 // 1-element tuple (no length byte)
	opEmptyList          byte = 0x26 // 0-element list
	opOneList            byte = 0x27 // 1-element list (no length byte)
	opEmptyUnicodeString byte = 0x28 // L""
	opUnicodeCharString  byte = 0x29 // single UTF-16 character (2 bytes)
	opPackedRow          byte = 0x2A // DBRowDescriptor header + zero-compressed data
	opSubStream          byte = 0x2B // nested marshal stream: length then bytes
	opTwoTuple           byte = 0x2C // 2-element tuple (no length byte)
	opPackedTerminator   byte = 0x2D // row-stream sentinel (decoded as PyNone)
	opUnicodeString      byte = 0x2E // UTF-16 string: 1-byte len (0xFF → 4-byte ext)
	opVarInteger         byte = 0x2F // variable-length integer

	opcodeMask  byte = 0x3F // lower 6 bits are the actual opcode
	saveMask    byte = 0x40 // bit 6: save this object for later reference
	unknownMask byte = 0x80 // bit 7: purpose unknown

	subStreamHeader byte = 0x7E // '~', first byte of every marshal stream
)
