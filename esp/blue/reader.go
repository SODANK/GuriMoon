package blue

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// ── The .blue container ──────────────────────────────────────────────────────
//
// Layout, type codes, list storage and the size-table oracle: see docs/blue-format.md#container-layout.
// The decoder is schema-driven; the size table decides where each object ends.

// Prop is one schema property.
type Prop struct {
	Name string
	Type byte
}

// Class is one schema entry.
type Class struct {
	Name  string
	NS    string
	Flags uint32
	Props []Prop
}

// File is a decoded .blue.
type File struct {
	RootClass string
	Strings   []string
	Classes   []Class
	Sizes     []uint32
	Root      *Object
	Heap      map[uint32]*Object // heap objects by their 06 ordinal
	Warnings  []string
}

// ErrUnknownType reports a schema type code the decoder has no width for.
type ErrUnknownType struct {
	Class, Prop string
	Code        byte
}

func (e ErrUnknownType) Error() string {
	return fmt.Sprintf("blue: %s.%s: unknown type code %#02x", e.Class, e.Prop, e.Code)
}

const (
	typeInt    = 0x01
	typeFloat  = 0x02
	typeDouble = 0x03
	typeBool   = 0x04
	typeStruct = 0x05
	typeRef    = 0x06
	typeString = 0x08
	typeTime   = 0x09
	typeRes    = 0x0c
	typeEnum   = 0x1f

	tagStruct = 0x05
	tagHeap   = 0x06
	tagExtern = 0x64
	tagRef    = 0x69
	tagNull   = 0x6a
	tagEnd    = 0xff
	tagClose  = 0xd3

	rootOffset = 0x64
)

type decoder struct {
	d    []byte
	f    *File
	heap map[uint32]*Object
}

// Parse decodes a .blue file.
func Parse(data []byte) (*File, error) {
	if len(data) < 0x70 || string(data[:5]) != "Blue\x00" {
		return nil, errors.New("blue: bad magic")
	}
	f := &File{Heap: map[uint32]*Object{}}
	dec := &decoder{d: data, f: f, heap: f.Heap}

	// Root class name from the header: u32 len + "trinity", three zero bytes,
	// then the NUL-terminated class name (0x13 for the "trinity" namespace).
	p := 5
	nsLen := int(binary.LittleEndian.Uint32(data[p:]))
	p += 4 + nsLen + 3
	if p >= rootOffset-1 {
		return nil, errors.New("blue: truncated header")
	}
	end := p
	for end < rootOffset-1 && data[end] != 0 {
		end++
	}
	f.RootClass = string(data[p:end])

	nStrings := int(binary.LittleEndian.Uint32(data[0x4c:]))
	nClasses := int(binary.LittleEndian.Uint32(data[0x54:]))
	nObjects := int(binary.LittleEndian.Uint32(data[0x58:]))
	if nObjects <= 0 || nObjects > len(data)/4 {
		return nil, fmt.Errorf("blue: implausible object count %d", nObjects)
	}

	strAt, err := findStringTable(data, nStrings)
	if err != nil {
		return nil, err
	}
	f.Strings = make([]string, 0, nStrings)
	for p := strAt; p < len(data); {
		l := int(binary.LittleEndian.Uint32(data[p:]))
		f.Strings = append(f.Strings, string(data[p+4:p+4+l]))
		p += 4 + l
	}

	sizesAt := strAt - 4*nObjects
	if sizesAt < rootOffset {
		return nil, errors.New("blue: size table overlaps header")
	}
	f.Sizes = make([]uint32, nObjects)
	for i := range f.Sizes {
		f.Sizes[i] = binary.LittleEndian.Uint32(data[sizesAt+4*i:])
	}

	if data[rootOffset] != tagStruct || binary.LittleEndian.Uint16(data[rootOffset+1:]) != 0 {
		return nil, fmt.Errorf("blue: root record not at %#x", rootOffset)
	}
	rootEnd := rootOffset + 1 + int(f.Sizes[0])
	if rootEnd > sizesAt {
		return nil, errors.New("blue: root size overruns schema")
	}
	if err := dec.readSchema(rootEnd, sizesAt, nClasses); err != nil {
		return nil, err
	}

	root, end, err := dec.tagged(rootOffset)
	if err != nil {
		return nil, err
	}
	obj, ok := root.(*Object)
	if !ok {
		return nil, errors.New("blue: root is not an object")
	}
	if obj.hasClose {
		end++
	}
	if end != rootEnd {
		return nil, fmt.Errorf("blue: root ends at %#x, size table says %#x", end, rootEnd)
	}
	f.Root = obj
	return f, nil
}

// findStringTable locates the start of the trailing string table: the unique
// offset from which exactly n length-prefixed strings reach end of file.
func findStringTable(d []byte, n int) (int, error) {
	for start := len(d) - 4; start > rootOffset; start-- {
		p, cnt := start, 0
		for p < len(d) && cnt <= n {
			if p+4 > len(d) {
				break
			}
			l := int(binary.LittleEndian.Uint32(d[p:]))
			if l < 0 || p+4+l > len(d) {
				break
			}
			p += 4 + l
			cnt++
		}
		if p == len(d) && cnt == n {
			return start, nil
		}
	}
	return 0, fmt.Errorf("blue: string table (%d strings) not found", n)
}

func (dec *decoder) readSchema(start, end, nClasses int) error {
	d, f := dec.d, dec.f
	p := start
	for c := 0; c < nClasses; c++ {
		if p+12 > end {
			return errors.New("blue: schema overruns size table")
		}
		nm := int(binary.LittleEndian.Uint16(d[p:]))
		ns := int(binary.LittleEndian.Uint16(d[p+2:]))
		flags := binary.LittleEndian.Uint32(d[p+4:])
		cnt := int(binary.LittleEndian.Uint32(d[p+8:]))
		p += 12
		if nm >= len(f.Strings) || ns >= len(f.Strings) || cnt > 1024 {
			return fmt.Errorf("blue: schema class %d out of range", c)
		}
		cl := Class{Name: f.Strings[nm], NS: f.Strings[ns], Flags: flags, Props: make([]Prop, 0, cnt)}
		for k := 0; k < cnt; k++ {
			if p+3 > end {
				return errors.New("blue: schema overruns size table")
			}
			pn := int(binary.LittleEndian.Uint16(d[p:]))
			if pn >= len(f.Strings) {
				return fmt.Errorf("blue: schema %s: property name out of range", cl.Name)
			}
			cl.Props = append(cl.Props, Prop{Name: f.Strings[pn], Type: d[p+2]})
			p += 3
		}
		f.Classes = append(f.Classes, cl)
	}
	if p != end {
		return fmt.Errorf("blue: schema ends at %#x, size table at %#x", p, end)
	}
	return nil
}

// tagged decodes one tagged value (a property value or list element) at p and
// returns it with the offset just past it.
func (dec *decoder) tagged(p int) (Value, int, error) {
	d := dec.d
	if p >= len(d) {
		return nil, p, errors.New("blue: truncated")
	}
	switch d[p] {
	case tagStruct:
		if p+4 > len(d) {
			return nil, p, errors.New("blue: truncated")
		}
		id := binary.LittleEndian.Uint16(d[p+1:])
		cls := d[p+3]
		o := &Object{ID: id, Tag: tagStruct}
		end, err := dec.body(p+4, p+1, cls, o)
		if err != nil {
			return nil, p, err
		}
		return o, end, nil
	case tagHeap:
		if p+8 > len(d) {
			return nil, p, errors.New("blue: truncated")
		}
		ord := binary.LittleEndian.Uint32(d[p+1:])
		id := binary.LittleEndian.Uint16(d[p+5:])
		cls := d[p+7]
		o := &Object{ID: id, Tag: tagHeap, Ordinal: ord}
		dec.heap[ord] = o // registered before the body: a child may refer back
		end, err := dec.body(p+8, p+5, cls, o)
		if err != nil {
			return nil, p, err
		}
		if o.hasClose || (int(id) >= len(dec.f.Sizes) && end < len(d) && d[end] == tagClose) {
			if end >= len(d) || d[end] != tagClose {
				return nil, p, fmt.Errorf("blue: heap object %d (%s) not closed at %#x", id, o.Class, end)
			}
			end++
		}
		return o, end, nil
	case tagExtern:
		if p+3 > len(d) {
			return nil, p, errors.New("blue: truncated")
		}
		s := int(binary.LittleEndian.Uint16(d[p+1:]))
		if s >= len(dec.f.Strings) {
			return nil, p, errors.New("blue: extern string index out of range")
		}
		return ExtRef{Path: dec.f.Strings[s]}, p + 3, nil
	case tagRef:
		if p+5 > len(d) {
			return nil, p, errors.New("blue: truncated")
		}
		ord := binary.LittleEndian.Uint32(d[p+1:])
		o := dec.heap[ord]
		if o == nil {
			return nil, p, fmt.Errorf("blue: reference to unknown heap ordinal %d at %#x", ord, p)
		}
		return o, p + 5, nil
	case tagNull:
		return nil, p + 1, nil
	}
	return nil, p, fmt.Errorf("blue: unknown value tag %#02x at %#x", d[p], p)
}

// body decodes an object's sparse property list (and List storage, and any raw
// payload) starting at p. idPos is the offset of the object's u16 id, which the
// size table measures from.
func (dec *decoder) body(p, idPos int, cls byte, o *Object) (int, error) {
	d, f := dec.d, dec.f
	if int(cls) >= len(f.Classes) {
		return p, fmt.Errorf("blue: object %d has class index %d of %d", o.ID, cls, len(f.Classes))
	}
	class := &f.Classes[cls]
	o.Class, o.NS = class.Name, class.NS

	// The size entry decides where the object ends and whether a d3 closes it.
	bodyEnd := -1
	if int(o.ID) < len(f.Sizes) {
		bodyEnd = idPos + int(f.Sizes[o.ID])
		if bodyEnd > len(d) {
			return p, fmt.Errorf("blue: %s#%d size entry overruns file", o.Class, o.ID)
		}
		if (o.Tag == tagHeap || o.ID == 0) && bodyEnd > 0 && d[bodyEnd-1] == tagClose {
			o.hasClose = true
			bodyEnd--
		}
	}

	idx := 0
	for {
		if p >= len(d) {
			return p, fmt.Errorf("blue: %s#%d truncated", o.Class, o.ID)
		}
		b := int(d[p])
		p++
		if b == tagEnd {
			break
		}
		idx += b
		if idx >= len(class.Props) {
			return p, fmt.Errorf("blue: %s#%d: property index %d past %d at %#x", o.Class, o.ID, idx, len(class.Props), p-1)
		}
		pr := class.Props[idx]
		idx++
		v, np, err := dec.value(p, pr.Type, class.Name, pr.Name)
		if err != nil {
			return p, err
		}
		if o.Props == nil {
			o.Props = map[string]Value{}
		}
		o.Props[pr.Name] = v
		p = np
	}

	if class.Name == "List" {
		if p+10 > len(d) {
			return p, fmt.Errorf("blue: List#%d storage truncated", o.ID)
		}
		ls := int(binary.LittleEndian.Uint16(d[p+2:]))
		count := int(binary.LittleEndian.Uint16(d[p+6:]))
		zero := binary.LittleEndian.Uint16(d[p+8:])
		if ls >= len(f.Strings) || f.Strings[ls] != "listdata" || zero != 0 {
			return p, fmt.Errorf("blue: List#%d: unexpected storage header at %#x", o.ID, p)
		}
		p += 10
		o.Items = make([]Value, 0, count)
		for i := 0; i < count; i++ {
			v, np, err := dec.tagged(p)
			if err != nil {
				return p, err
			}
			o.Items = append(o.Items, v)
			p = np
		}
	}

	if bodyEnd >= 0 && p != bodyEnd {
		if p > bodyEnd {
			return p, fmt.Errorf("blue: %s#%d overran its size entry (%#x > %#x)", o.Class, o.ID, p, bodyEnd)
		}
		o.Raw = d[p:bodyEnd]
		f.Warnings = append(f.Warnings, fmt.Sprintf("%s#%d: %d bytes of trailing payload", o.Class, o.ID, bodyEnd-p))
		p = bodyEnd
	}
	return p, nil
}

func (dec *decoder) value(p int, ty byte, cname, pname string) (Value, int, error) {
	d := dec.d
	need := func(n int) error {
		if p+n > len(d) {
			return fmt.Errorf("blue: %s.%s truncated", cname, pname)
		}
		return nil
	}
	switch ty {
	case typeInt:
		if err := need(4); err != nil {
			return nil, p, err
		}
		return int32(binary.LittleEndian.Uint32(d[p:])), p + 4, nil
	case typeFloat:
		if err := need(4); err != nil {
			return nil, p, err
		}
		return math.Float32frombits(binary.LittleEndian.Uint32(d[p:])), p + 4, nil
	case typeDouble:
		if err := need(8); err != nil {
			return nil, p, err
		}
		return math.Float64frombits(binary.LittleEndian.Uint64(d[p:])), p + 8, nil
	case typeBool:
		if err := need(1); err != nil {
			return nil, p, err
		}
		return d[p] != 0, p + 1, nil
	case typeString, typeRes:
		if err := need(2); err != nil {
			return nil, p, err
		}
		s := int(binary.LittleEndian.Uint16(d[p:]))
		if s >= len(dec.f.Strings) {
			return nil, p, fmt.Errorf("blue: %s.%s: string index out of range", cname, pname)
		}
		if ty == typeRes {
			return Res(dec.f.Strings[s]), p + 2, nil
		}
		return dec.f.Strings[s], p + 2, nil
	case typeTime:
		if err := need(8); err != nil {
			return nil, p, err
		}
		return int64(binary.LittleEndian.Uint64(d[p:])), p + 8, nil
	case typeEnum:
		if err := need(1); err != nil {
			return nil, p, err
		}
		return Enum(d[p]), p + 1, nil
	case typeStruct, typeRef:
		return dec.tagged(p)
	}
	return nil, p, ErrUnknownType{Class: cname, Prop: pname, Code: ty}
}
