package marshal

// PackZeroCompressed compresses in using EVE's zero-compression scheme. Blocks are paired into
// opcode bytes: the low nibble is the first block's opcode, the high nibble the second's, then
// the data bytes of any data blocks follow in order.
//
//   Zero block (opcode 0x8-0xF): opcode-7 zero bytes.
//   Data block (opcode 0x0-0x7): 8-opcode non-zero bytes, appended after the opcode byte.
//
// Trailing zero blocks are dropped (the row consumer knows the expected length).
func PackZeroCompressed(in []byte) []byte {
	out := make([]byte, 0, len(in))

	type blk struct {
		isData    bool
		dataStart int
		dataCount byte
		opcode    byte
	}

	var pending *blk
	i := 0

	emitPair := func(curr blk) {
		// combine low nibble (pending) and high nibble (curr)
		out = append(out, (curr.opcode<<4)|pending.opcode)
		// write data bytes from pending block, if any
		if pending.isData {
			out = append(out, in[pending.dataStart:pending.dataStart+int(pending.dataCount)]...)
		}
		// write data bytes from current block, if any
		if curr.isData {
			out = append(out, in[curr.dataStart:curr.dataStart+int(curr.dataCount)]...)
		}
		pending = nil
	}

	for i < len(in) {
		if in[i] == 0 {
			count := byte(0)
			for i < len(in) && in[i] == 0 && count < 8 {
				count++
				i++
			}
			curr := blk{isData: false, opcode: 0x8 | (count - 1)}
			if pending != nil {
				emitPair(curr)
			} else {
				pending = &curr
			}
		} else {
			start := i
			count := byte(0)
			for i < len(in) && in[i] != 0 && count < 8 {
				count++
				i++
			}
			curr := blk{isData: true, dataStart: start, dataCount: count, opcode: 8 - count}
			if pending != nil {
				emitPair(curr)
			} else {
				pending = &curr
			}
		}
	}

	// Flush a pending data block (trailing zeros are silently dropped).
	if pending != nil && pending.isData {
		out = append(out, pending.opcode) // high nibble is 0 (harmless at EOF)
		out = append(out, in[pending.dataStart:pending.dataStart+int(pending.dataCount)]...)
	}

	return out
}

// UnpackZeroCompressed expands zero-compressed data produced by PackZeroCompressed.
//
// Each input byte carries two 4-bit opcodes (low nibble first, high nibble second).
// After each opcode byte the associated data bytes of any data-blocks follow.
func UnpackZeroCompressed(in []byte) []byte {
	out := make([]byte, 0, len(in)*2)

	haveLeftover := false
	var leftoverOpcode byte
	i := 0

	for haveLeftover || i < len(in) {
		var opcode byte
		if haveLeftover {
			opcode = leftoverOpcode
			haveLeftover = false
		} else {
			b := in[i]
			i++
			leftoverOpcode = b >> 4
			haveLeftover = true
			opcode = b & 0xF
		}

		if opcode < 8 {
			// data block: (8 - opcode) non-zero bytes follow in the input
			count := int(8 - opcode)
			for ; count > 0 && i < len(in); count-- {
				out = append(out, in[i])
				i++
			}
		} else {
			// zero block: (opcode - 8 + 1) zero bytes
			count := int(opcode-8) + 1
			for ; count > 0; count-- {
				out = append(out, 0)
			}
		}
	}

	return out
}
