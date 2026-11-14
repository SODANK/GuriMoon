// Package syncx holds typed wrappers over the sync package. See docs/go-idioms.md#typed-maps.
package syncx

import "sync"

// Map is a sync.Map with typed keys and values. The zero value is empty and ready to use, and
// a Map must not be copied after first use. A nil interface V round-trips as the zero V.
type Map[K comparable, V any] struct {
	m sync.Map
}

// Load returns the value stored for key and whether it was present.
func (m *Map[K, V]) Load(key K) (value V, ok bool) {
	v, ok := m.m.Load(key)
	value, _ = v.(V)
	return value, ok
}

// Store sets the value for key.
func (m *Map[K, V]) Store(key K, value V) { m.m.Store(key, value) }

// LoadOrStore returns the existing value for key if present. Otherwise it stores and returns
// value. loaded reports whether the value was already there.
func (m *Map[K, V]) LoadOrStore(key K, value V) (actual V, loaded bool) {
	v, loaded := m.m.LoadOrStore(key, value)
	actual, _ = v.(V)
	return actual, loaded
}

// LoadAndDelete deletes key and returns the value it held, if any.
func (m *Map[K, V]) LoadAndDelete(key K) (value V, loaded bool) {
	v, loaded := m.m.LoadAndDelete(key)
	value, _ = v.(V)
	return value, loaded
}

// Delete removes key.
func (m *Map[K, V]) Delete(key K) { m.m.Delete(key) }

// Swap stores value for key and returns the previous value, if any.
func (m *Map[K, V]) Swap(key K, value V) (previous V, loaded bool) {
	v, loaded := m.m.Swap(key, value)
	previous, _ = v.(V)
	return previous, loaded
}

// CompareAndDelete deletes key if its value equals old. V must be comparable at run time.
func (m *Map[K, V]) CompareAndDelete(key K, old V) (deleted bool) {
	return m.m.CompareAndDelete(key, old)
}

// CompareAndSwap stores new for key if its value equals old. V must be comparable at run time.
func (m *Map[K, V]) CompareAndSwap(key K, old, new V) (swapped bool) {
	return m.m.CompareAndSwap(key, old, new)
}

// Range calls f for each key and value, in no order, until f returns false. It has the
// semantics of sync.Map.Range.
func (m *Map[K, V]) Range(f func(key K, value V) bool) {
	m.m.Range(func(k, v any) bool {
		vv, _ := v.(V) // a nil interface value fails the assertion and stays the zero V
		return f(k.(K), vv)
	})
}

// Clear deletes every entry.
func (m *Map[K, V]) Clear() { m.m.Clear() }
