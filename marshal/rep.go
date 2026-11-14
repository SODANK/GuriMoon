package marshal

// PyRep is the sealed interface for all EVE Python-marshal representation types.
// The isPyRep() method is unexported so only types in this package can implement it.
type PyRep interface {
	isPyRep()
}

// PyNone represents Python None / the packed-row terminator sentinel.
type PyNone struct{}

// PyInt represents all integer types (byte, short, long, longlong).
// All values are stored as uint64 regardless of wire width.
// Note: signed shorts read from the wire are sign-extended into uint64.
type PyInt struct{ Value uint64 }

// PyReal represents a Python float (float64).
type PyReal struct{ Value float64 }

// PyBool represents Python True/False.
type PyBool struct{ Value bool }

// PyBuffer represents a raw byte buffer (Op_PyBuffer).
type PyBuffer struct{ Data []byte }

// PyString represents a Python string.
// IsType1 distinguishes Op_PyByteString (type-1) from Op_PyByteString2 (type-2).
// The difference only affects encoding; the value is the same.
type PyString struct {
	Value   string
	IsType1 bool
}

// DictEntry is a single key-value pair in a PyDict.
// A slice is used (rather than a map) to preserve insertion order and because
// PyRep is not comparable (it is an interface containing pointer types).
type DictEntry struct {
	Key   PyRep
	Value PyRep
}

// PyTuple represents a Python tuple.
type PyTuple struct{ Items []PyRep }

// PyList represents a Python list.
type PyList struct{ Items []PyRep }

// PyDict represents a Python dict.  Order is preserved as received on the wire.
type PyDict struct{ Items []DictEntry }

// PyObject represents a Python object: a type-name string plus an arguments value.
type PyObject struct {
	Type string
	Args PyRep
}

// PySubStruct wraps a single nested PyRep (Op_PySubStruct, remote object spec).
type PySubStruct struct{ Value PyRep }

// PySubStream is a nested marshal stream (Op_PySubStream).
// After unmarshaling, Decoded holds the parsed inner object.
// Data holds the original raw bytes and is used for re-marshaling if Decoded is nil.
type PySubStream struct {
	Data    []byte // raw bytes of the nested stream (including its 0x7E header)
	Decoded PyRep  // parsed form; populated by Unmarshal
}

// PyChecksumedStream wraps an object with a 32-bit checksum prefix.
type PyChecksumedStream struct {
	Checksum uint32
	Value    PyRep
}

// PyPackedRow is a single database row stored as zero-compressed binary data.
// Header is the DBRowDescriptor that describes the column layout.
// Data holds the already zero-compressed row bytes.
type PyPackedRow struct {
	Header PyRep
	Data   []byte
}

// PyPackedRowHeader is a full rowset containing a descriptor and a list of rows.
type PyPackedRowHeader struct {
	HeaderType PyRep
	Arguments  PyRep
	Rows       []PyRep
}

// Ensure all concrete types satisfy PyRep.
func (PyNone) isPyRep()             {}
func (PyInt) isPyRep()              {}
func (PyReal) isPyRep()             {}
func (PyBool) isPyRep()             {}
func (PyBuffer) isPyRep()           {}
func (PyString) isPyRep()           {}
func (PyTuple) isPyRep()            {}
func (PyList) isPyRep()             {}
func (PyDict) isPyRep()             {}
func (PyObject) isPyRep()           {}
func (PySubStruct) isPyRep()        {}
func (*PySubStream) isPyRep()       {}
func (PyChecksumedStream) isPyRep() {}
func (PyPackedRow) isPyRep()        {}
func (PyPackedRowHeader) isPyRep()  {}
