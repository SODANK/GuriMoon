package blue

// Value is one decoded property value: int32, float32, float64, bool, string,
// Res, int64, Enum, *Object, ExtRef, or nil (a null reference).
type Value = any

// Res is a resource path ("res:/Model/...") that the client resolves through its resource
// manager rather than a plain name.
type Res string

// Enum is a one-byte enumerated value (TriShader.cull, TriPass blend modes).
type Enum uint8

// ExtRef is a reference to another .blue file (TriArea.shader and the like).
type ExtRef struct{ Path string }

// Object is one decoded object.
type Object struct {
	ID      uint16
	Class   string
	NS      string
	Tag     byte   // tagStruct (inline) or tagHeap
	Ordinal uint32 // heap ordinal (Tag == tagHeap)
	Props   map[string]Value
	Items   []Value // List elements (Class == "List")
	Raw     []byte  // trailing payload the schema does not describe

	hasClose bool
}

// Get returns a property or nil.
func (o *Object) Get(name string) Value {
	if o == nil || o.Props == nil {
		return nil
	}
	return o.Props[name]
}

// Has reports whether the property was stored.
func (o *Object) Has(name string) bool {
	if o == nil || o.Props == nil {
		return false
	}
	_, ok := o.Props[name]
	return ok
}

// Float returns a float32/float64/int property as float64, or def.
func (o *Object) Float(name string, def float64) float64 {
	switch v := o.Get(name).(type) {
	case float32:
		return float64(v)
	case float64:
		return v
	case int32:
		return float64(v)
	}
	return def
}

// Int returns an int32/enum/bool property as int, or def.
func (o *Object) Int(name string, def int) int {
	switch v := o.Get(name).(type) {
	case int32:
		return int(v)
	case Enum:
		return int(v)
	case bool:
		if v {
			return 1
		}
		return 0
	}
	return def
}

// Bool returns a bool property, or def.
func (o *Object) Bool(name string, def bool) bool {
	if v, ok := o.Get(name).(bool); ok {
		return v
	}
	return def
}

// Str returns a string or resource property, or "".
func (o *Object) Str(name string) string {
	switch v := o.Get(name).(type) {
	case string:
		return v
	case Res:
		return string(v)
	}
	return ""
}

// Child returns an object-valued property, or nil.
func (o *Object) Child(name string) *Object {
	v, _ := o.Get(name).(*Object)
	return v
}

// List returns the elements of a List-valued property (nil when absent).
func (o *Object) List(name string) []Value {
	c := o.Child(name)
	if c == nil {
		return nil
	}
	return c.Items
}

// Objects returns the *Object elements of a List-valued property.
func (o *Object) Objects(name string) []*Object {
	var out []*Object
	for _, v := range o.List(name) {
		if c, ok := v.(*Object); ok {
			out = append(out, c)
		}
	}
	return out
}

// Vec3 returns a TriVector property, each component defaulting to def's.
func (o *Object) Vec3(name string, def Vec3) Vec3 {
	c := o.Child(name)
	if c == nil {
		return def
	}
	return Vec3{c.Float("x", def.X), c.Float("y", def.Y), c.Float("z", def.Z)}
}

// Quat returns a TriQuaternion property, identity when absent.
func (o *Object) Quat(name string) Quat {
	c := o.Child(name)
	if c == nil {
		return Quat{0, 0, 0, 1}
	}
	return Quat{c.Float("x", 0), c.Float("y", 0), c.Float("z", 0), c.Float("w", 1)}
}

// Mat4 returns a TriMatrix property (absent elements default to identity).
func (o *Object) Mat4(name string) (Mat4, bool) {
	c := o.Child(name)
	if c == nil {
		return Identity(), false
	}
	m := Identity()
	names := [4][4]string{
		{"_11", "_12", "_13", "_14"}, {"_21", "_22", "_23", "_24"},
		{"_31", "_32", "_33", "_34"}, {"_41", "_42", "_43", "_44"},
	}
	for r := 0; r < 4; r++ {
		for col := 0; col < 4; col++ {
			m[r][col] = c.Float(names[r][col], m[r][col])
		}
	}
	return m, true
}

// Color returns a TriColor property as RGBA, or def.
func (o *Object) Color(name string, def [4]float32) [4]float32 {
	c := o.Child(name)
	if c == nil {
		return def
	}
	return [4]float32{
		float32(c.Float("r", float64(def[0]))), float32(c.Float("g", float64(def[1]))),
		float32(c.Float("b", float64(def[2]))), float32(c.Float("a", float64(def[3]))),
	}
}
