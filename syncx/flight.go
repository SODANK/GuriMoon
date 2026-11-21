package syncx

import "sync"

// Flight coalesces concurrent calls for the same key: while one call for a key is
// running, later callers wait for it and share its result instead of doing the same
// work again. A minimal stand-in for golang.org/x/sync/singleflight, which this
// module does not depend on. The zero value is ready to use.
type Flight[K comparable, V any] struct {
	mu    sync.Mutex
	calls map[K]*flightCall[V]
}

type flightCall[V any] struct {
	done chan struct{}
	val  V
	err  error
}

// Do runs fn for key unless a call for key is already in flight, in which case it
// waits for that call and returns its result. shared reports whether the result came
// from another caller's run.
func (f *Flight[K, V]) Do(key K, fn func() (V, error)) (v V, err error, shared bool) {
	f.mu.Lock()
	if f.calls == nil {
		f.calls = make(map[K]*flightCall[V])
	}
	if c, ok := f.calls[key]; ok {
		f.mu.Unlock()
		<-c.done
		return c.val, c.err, true
	}
	c := &flightCall[V]{done: make(chan struct{})}
	f.calls[key] = c
	f.mu.Unlock()

	defer func() {
		f.mu.Lock()
		delete(f.calls, key)
		f.mu.Unlock()
		close(c.done)
	}()
	c.val, c.err = fn()
	return c.val, c.err, false
}
