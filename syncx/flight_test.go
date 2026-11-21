package syncx

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFlightCoalesces(t *testing.T) {
	var f Flight[string, int]
	var runs atomic.Int32
	release := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]int, 10)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v, _, _ := f.Do("k", func() (int, error) {
				runs.Add(1)
				<-release
				return 42, nil
			})
			results[i] = v
		}(i)
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if runs.Load() != 1 {
		t.Fatalf("fn ran %d times, want 1", runs.Load())
	}
	for i, v := range results {
		if v != 42 {
			t.Fatalf("result %d = %d", i, v)
		}
	}
	// A later call runs again.
	v, _, shared := f.Do("k", func() (int, error) { return 7, nil })
	if v != 7 || shared {
		t.Fatalf("second call = %d shared=%v", v, shared)
	}
}
