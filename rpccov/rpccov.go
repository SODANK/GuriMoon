// Package rpccov is the runtime RPC coverage ledger: which client calls the server
// receives and what the dispatcher did with each.
//
// cmd/rpc-coverage computes the static half (every call the client can make). This
// package sees bound-object calls, which arrive with an empty service name. An entry
// keeps the worst outcome seen, in the order Handled, Stub, Fallthrough, Unhandled;
// Stub and Fallthrough both answer None, so the client fails silently.
//
// Calls are also kept in a per-connection ring so a client traceback from the alert
// service is stored with the calls that preceded it.
package rpccov

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"gurimoon/safego"
)

// Outcome describes what the dispatcher did with a call.
type Outcome uint8

const (
	// Handled means the call reached a real handler.
	Handled Outcome = iota
	// Stub means a stub service answered None without doing any work.
	Stub
	// Fallthrough means a bound-object call hit the default arm and got None.
	Fallthrough
	// Unhandled means no handler existed and the server raised an exception.
	Unhandled
)

// String renders an outcome for the JSON file and the ESP page.
func (o Outcome) String() string {
	switch o {
	case Stub:
		return "stub"
	case Fallthrough:
		return "fallthrough"
	case Unhandled:
		return "unhandled"
	default:
		return "handled"
	}
}

// parseOutcome is the inverse of String, used when reloading the JSON file.
func parseOutcome(s string) Outcome {
	switch s {
	case "stub":
		return Stub
	case "fallthrough":
		return Fallthrough
	case "unhandled":
		return Unhandled
	default:
		return Handled
	}
}

// BoundService is the service name recorded for bound-object calls.
const BoundService = "(bound)"

// Entry is the accumulated record for one service.method pair.
type Entry struct {
	Service   string    `json:"service"`
	Method    string    `json:"method"`
	Count     int64     `json:"count"`
	Outcome   string    `json:"outcome"` // worst outcome seen
	FirstSeen time.Time `json:"firstSeen"`
	LastSeen  time.Time `json:"lastSeen"`
}

// Key is the map key / report key for an entry.
func (e Entry) Key() string { return e.Service + "." + e.Method }

// Trace is a deduplicated client-side Python traceback.
type Trace struct {
	// Hash is the client's adler32 stack hash when it sent one, so identical exceptions
	// collapse onto one row across sessions.
	Hash uint64 `json:"hash"`
	// Culprit is the short exception text from the stackID tuple.
	Culprit string `json:"culprit"`
	// Text is the decoded traceback body.
	Text string `json:"text"`
	// Context is the last few RPCs on the reporting connection, oldest first.
	Context   []string  `json:"context"`
	Count     int64     `json:"count"`
	FirstSeen time.Time `json:"firstSeen"`
	LastSeen  time.Time `json:"lastSeen"`
}

// maxTraces caps the traceback table.
const maxTraces = 500

// maxEntries caps the call ledger. Keys are client-supplied strings, so the map and
// the saved file need a bound.
const maxEntries = 4096

// contextDepth is how many recent calls are kept per connection.
const contextDepth = 8

// snapshot is the on-disk file format.
type snapshot struct {
	SavedAt time.Time `json:"savedAt"`
	Entries []Entry   `json:"entries"`
	Traces  []Trace   `json:"traces"`
}

var (
	mu      sync.Mutex
	entries = map[string]*Entry{}
	traces  = map[uint64]*Trace{}
	// recent maps a connection address to its last contextDepth calls.
	recent = map[string][]string{}

	savePath string
)

// Record notes one call. addr identifies the connection for traceback attribution
// ("" for none). An empty service is recorded as BoundService.
func Record(addr, service, method string, outcome Outcome) {
	if method == "" {
		return
	}
	if service == "" {
		service = BoundService
	}
	now := time.Now()
	key := service + "." + method

	mu.Lock()
	defer mu.Unlock()

	e, ok := entries[key]
	if !ok {
		if len(entries) >= maxEntries {
			return
		}
		e = &Entry{Service: service, Method: method, FirstSeen: now, Outcome: Handled.String()}
		entries[key] = e
	}
	e.Count++
	e.LastSeen = now
	// Keep the worst outcome so an occasional silent None still shows as a gap.
	if outcome > parseOutcome(e.Outcome) {
		e.Outcome = outcome.String()
	}

	if addr != "" {
		r := append(recent[addr], key)
		if len(r) > contextDepth {
			r = r[len(r)-contextDepth:]
		}
		recent[addr] = r
	}
}

// Forget drops the recent-call ring for a disconnected connection.
func Forget(addr string) {
	if addr == "" {
		return
	}
	mu.Lock()
	delete(recent, addr)
	mu.Unlock()
}

// RecordTrace stores a decoded client traceback with the recent calls on addr's
// connection. A repeat of the same hash bumps the count and refreshes the context.
func RecordTrace(addr string, hash uint64, culprit, text string) {
	if text == "" && culprit == "" {
		return
	}
	now := time.Now()

	mu.Lock()
	defer mu.Unlock()

	// No client hash: hash the text so repeats still collapse.
	if hash == 0 {
		hash = fnv64(culprit + "\x00" + text)
	}

	ctx := append([]string(nil), recent[addr]...)

	t, ok := traces[hash]
	if !ok {
		if len(traces) >= maxTraces {
			evictOldestTraceLocked()
		}
		t = &Trace{Hash: hash, FirstSeen: now}
		traces[hash] = t
	}
	t.Count++
	t.LastSeen = now
	t.Context = ctx
	if culprit != "" {
		t.Culprit = culprit
	}
	// Keep the longest body; a later report can carry the full trace.
	if len(text) > len(t.Text) {
		t.Text = text
	}
}

// evictOldestTraceLocked removes the least recently seen traceback. Caller holds mu.
func evictOldestTraceLocked() {
	var oldestKey uint64
	var oldest time.Time
	first := true
	for k, t := range traces {
		if first || t.LastSeen.Before(oldest) {
			oldestKey, oldest, first = k, t.LastSeen, false
		}
	}
	if !first {
		delete(traces, oldestKey)
	}
}

// Entries returns a copy of the ledger, worst outcome first, then by count descending.
func Entries() []Entry {
	mu.Lock()
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		out = append(out, *e)
	}
	mu.Unlock()

	sort.Slice(out, func(i, j int) bool {
		oi, oj := parseOutcome(out[i].Outcome), parseOutcome(out[j].Outcome)
		if oi != oj {
			return oi > oj
		}
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Key() < out[j].Key()
	})
	return out
}

// Traces returns a copy of the traceback table, most recent first.
func Traces() []Trace {
	mu.Lock()
	out := make([]Trace, 0, len(traces))
	for _, t := range traces {
		out = append(out, *t)
	}
	mu.Unlock()

	sort.Slice(out, func(i, j int) bool { return out[i].LastSeen.After(out[j].LastSeen) })
	return out
}

// Stats summarises the ledger for the dashboard header.
type Stats struct {
	Distinct    int
	Total       int64
	Handled     int
	Stub        int
	Fallthrough int
	Unhandled   int
	Tracebacks  int
}

// Summary counts entries by outcome.
func Summary() Stats {
	var s Stats
	for _, e := range Entries() {
		s.Distinct++
		s.Total += e.Count
		switch parseOutcome(e.Outcome) {
		case Unhandled:
			s.Unhandled++
		case Fallthrough:
			s.Fallthrough++
		case Stub:
			s.Stub++
		default:
			s.Handled++
		}
	}
	s.Tracebacks = len(Traces())
	return s
}

// Init loads a saved ledger from path and starts periodic saves. A missing or
// corrupt file is ignored.
func Init(path string, every time.Duration) {
	mu.Lock()
	savePath = path
	mu.Unlock()

	_ = Load(path)

	if every > 0 {
		go func() {
			defer safego.Recover("Init")
			t := time.NewTicker(every)
			defer t.Stop()
			for range t.C {
				_ = Save(path)
			}
		}()
	}
}

// Load merges a saved ledger into the in-memory one.
func Load(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return err
	}

	mu.Lock()
	defer mu.Unlock()
	for _, e := range snap.Entries {
		e := e
		key := e.Key()
		if cur, ok := entries[key]; ok {
			cur.Count += e.Count
			if parseOutcome(e.Outcome) > parseOutcome(cur.Outcome) {
				cur.Outcome = e.Outcome
			}
			if e.FirstSeen.Before(cur.FirstSeen) {
				cur.FirstSeen = e.FirstSeen
			}
			if e.LastSeen.After(cur.LastSeen) {
				cur.LastSeen = e.LastSeen
			}
			continue
		}
		entries[key] = &e
	}
	for _, t := range snap.Traces {
		t := t
		if _, ok := traces[t.Hash]; !ok {
			traces[t.Hash] = &t
		}
	}
	return nil
}

// Save writes the ledger to path, creating the parent directory if needed.
func Save(path string) error {
	if path == "" {
		return nil
	}
	snap := snapshot{SavedAt: time.Now(), Entries: Entries(), Traces: Traces()}
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	// Write through a temp file so a crash cannot truncate the saved ledger.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Flush saves to the path given to Init.  Safe to call when Init never ran.
func Flush() error {
	mu.Lock()
	path := savePath
	mu.Unlock()
	return Save(path)
}

// Reset clears the ledger (the ESP page's reset button).
func Reset() {
	mu.Lock()
	entries = map[string]*Entry{}
	traces = map[uint64]*Trace{}
	recent = map[string][]string{}
	mu.Unlock()
}

// fnv64 is FNV-1a, the hash for tracebacks that arrive without a client hash.
func fnv64(s string) uint64 {
	const offset64 = 14695981039346656037
	const prime64 = 1099511628211
	h := uint64(offset64)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime64
	}
	return h
}
