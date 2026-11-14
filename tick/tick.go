// Package tick is the central background timer framework. One goroutine drives a
// time.Ticker, and callers register periodic or one-shot callbacks.
//
// The ticker only keeps time and hands due work to lanes. A lane is a serial queue with
// its own goroutine: its work runs one at a time in the order it came due, and
// different lanes run concurrently. RegisterPeriodic and ScheduleOnce share the default
// lane, so those callbacks never overlap; a slow one cannot stop the clock.
//
// A periodic is single-flight: if it comes due while its previous run is queued or
// running, the new run is dropped, so a callback never runs back to back to catch up.
package tick

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultLane is the lane RegisterPeriodic and ScheduleOnce use.
const DefaultLane = ""

// Callback is invoked every N ticks. Return false to self-unregister.
type Callback func(tickNum uint64) bool

type periodic struct {
	id         uint64
	label      string
	lane       string
	everyTicks uint64
	fn         Callback

	// queued is set from the moment a run is handed to its lane until that
	// run has finished; it is the single-flight guard.
	queued atomic.Bool
	// removed is set by Unregister. A run already queued when its periodic
	// is unregistered must not fire.
	removed atomic.Bool
}

type oneshot struct {
	label string
	at    time.Time
	fn    func()
}

// job is one unit of lane work: a periodic run or a one-shot.
type job struct {
	p       *periodic
	tickNum uint64
	o       *oneshot
}

type lane struct {
	mu      sync.Mutex
	queue   []job
	running bool
}

// Manager is the central tick manager.
type Manager struct {
	interval time.Duration

	mu        sync.Mutex
	nextID    uint64
	periodics map[uint64]*periodic
	oneshots  map[uint64]*oneshot
	lanes     map[string]*lane
	cancel    context.CancelFunc
	loopDone  chan struct{} // closed when the loop goroutine returns
	running   bool

	// inflight counts lane goroutines, so Wait can tell when every callback
	// handed out so far has returned.
	inflight sync.WaitGroup
}

// New creates a Manager that ticks at the given interval.
func New(interval time.Duration) *Manager {
	return &Manager{
		interval:  interval,
		periodics: make(map[uint64]*periodic),
		oneshots:  make(map[uint64]*oneshot),
		lanes:     make(map[string]*lane),
	}
}

// Start begins the tick loop in a background goroutine.
// The provided ctx can be used for external cancellation.
func (m *Manager) Start(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running {
		return
	}
	loopCtx, cancel := context.WithCancel(ctx)
	m.cancel = cancel
	m.running = true
	done := make(chan struct{})
	m.loopDone = done
	go func() {
		defer close(done)
		m.loop(loopCtx)
	}()
}

// Stop halts the tick loop and returns once the loop can hand out no more work, so a
// following Wait sees every callback. Callbacks already handed to a lane still run.
func (m *Manager) Stop() {
	m.mu.Lock()
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	done := m.loopDone
	m.loopDone = nil
	m.running = false
	m.mu.Unlock()
	if done != nil {
		<-done // fireTick takes m.mu, so the wait is outside it
	}
}

// Wait blocks until every callback handed out so far has returned, or until
// timeout passes. It reports whether everything finished. Call it after Stop,
// or new work may keep arriving.
func (m *Manager) Wait(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		m.inflight.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// RegisterPeriodic adds a callback that fires every everyTicks ticks, on the
// default lane. Returns an ID that can be passed to Unregister.
func (m *Manager) RegisterPeriodic(label string, everyTicks uint64, fn Callback) uint64 {
	return m.RegisterPeriodicLane(DefaultLane, label, everyTicks, fn)
}

// RegisterPeriodicLane is RegisterPeriodic on a named lane. Lanes run concurrently, so
// give a callback its own lane only when everything it touches is safe against every
// other lane (state already guarded for the connection goroutines).
func (m *Manager) RegisterPeriodicLane(laneName, label string, everyTicks uint64, fn Callback) uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	id := m.nextID
	m.periodics[id] = &periodic{id: id, label: label, lane: laneName, everyTicks: everyTicks, fn: fn}
	return id
}

// Unregister removes a periodic callback by ID.
func (m *Manager) Unregister(id uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.periodics[id]; ok {
		p.removed.Store(true)
		delete(m.periodics, id)
	}
}

// ScheduleOnce schedules a function to fire once at or after the given time,
// on the default lane. Returns an ID that can be passed to CancelOnce.
func (m *Manager) ScheduleOnce(label string, at time.Time, fn func()) uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	id := m.nextID
	m.oneshots[id] = &oneshot{label: label, at: at, fn: fn}
	return id
}

// CancelOnce removes a one-shot timer by ID.
func (m *Manager) CancelOnce(id uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.oneshots, id)
}

func (m *Manager) loop(ctx context.Context) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	var tickNum uint64
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tickNum++
			m.fireTick(tickNum)
		}
	}
}

// fireTick hands this tick's due work to its lanes. It never runs a callback
// itself, so it returns in microseconds whatever the callbacks cost.
func (m *Manager) fireTick(tickNum uint64) {
	m.mu.Lock()
	var due []*periodic
	for _, p := range m.periodics {
		if p.everyTicks == 0 || tickNum%p.everyTicks == 0 {
			due = append(due, p)
		}
	}
	now := time.Now()
	var ready []*oneshot
	for id, o := range m.oneshots {
		if !now.Before(o.at) {
			ready = append(ready, o)
			delete(m.oneshots, id)
		}
	}
	m.mu.Unlock()

	// Periodics before one-shots, as the single tick goroutine ran them.
	for _, p := range due {
		if !p.queued.CompareAndSwap(false, true) {
			continue // previous run still queued or running
		}
		m.enqueue(p.lane, job{p: p, tickNum: tickNum})
	}
	for _, o := range ready {
		m.enqueue(DefaultLane, job{o: o})
	}
}

func (m *Manager) enqueue(laneName string, j job) {
	m.mu.Lock()
	l := m.lanes[laneName]
	if l == nil {
		l = &lane{}
		m.lanes[laneName] = l
	}
	m.mu.Unlock()

	l.mu.Lock()
	l.queue = append(l.queue, j)
	start := !l.running
	if start {
		l.running = true
		m.inflight.Add(1)
	}
	l.mu.Unlock()
	if start {
		go m.drain(l)
	}
}

// drain runs a lane's queue until it is empty, then lets the goroutine go.
// A lane with nothing due holds no goroutine.
func (m *Manager) drain(l *lane) {
	defer m.inflight.Done()
	for {
		l.mu.Lock()
		if len(l.queue) == 0 {
			l.running = false
			l.mu.Unlock()
			return
		}
		j := l.queue[0]
		l.queue[0] = job{}
		l.queue = l.queue[1:]
		l.mu.Unlock()

		if j.o != nil {
			m.safeCallOneshot(j.o)
			continue
		}
		p := j.p
		if !p.removed.Load() && !m.safeCallPeriodic(p, j.tickNum) {
			m.Unregister(p.id)
		}
		// Cleared last, after any Unregister, so a periodic that asked to
		// stop cannot be queued again in between.
		p.queued.Store(false)
	}
}

func (m *Manager) safeCallPeriodic(p *periodic, tickNum uint64) (keepGoing bool) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("tick: periodic %q panicked: %v", p.label, r)
			keepGoing = false
		}
	}()
	return p.fn(tickNum)
}

func (m *Manager) safeCallOneshot(o *oneshot) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("tick: oneshot %q panicked: %v", o.label, r)
		}
	}()
	o.fn()
}
