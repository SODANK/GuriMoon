// store.go: in-memory cache store and on-demand DB generation.
//
// Entries are generated from the database on first request and kept in memory for the
// server's lifetime. Generation runs the catalog SQL, builds the PyRep container (TupleSet,
// Rowset, RowsetTuple or IndexRowset), marshals it, keeps the zlib (RFC 1950) bytes when they
// are smaller, and stamps the CRC32 of the stored bytes as the version and Win32TimeNow() as
// the timestamp.
package cache

import (
	"bytes"
	"compress/zlib"
	"context"
	"database/sql"
	"fmt"
	"hash/crc32"
	"log"
	"math"
	"strings"
	"sync"
	"time"

	"gurimoon/marshal"
)

// win32Epoch is the number of 100-nanosecond intervals between
// 1601-01-01 00:00:00 UTC and 1970-01-01 00:00:00 UTC.
const win32Epoch uint64 = 116444736000000000

// Store holds generated/loaded cache entries in memory (write-once per objectID).
type Store struct {
	mu      sync.RWMutex
	entries map[string]*Entry

	// Coalescing state for the debounced name invalidations. Separate mutex:
	// the timer's callback takes mu, so it must not already hold it.
	debounceMu sync.Mutex
	debounce   map[string]*time.Timer
}

// New returns an empty Store.
func New() *Store {
	return &Store{entries: make(map[string]*Entry)}
}

// Get returns the Entry for objectID, generating it from the database the first
// time it is requested.  Returns an error if objectID is not in the catalog or
// the SQL query fails.
func (s *Store) Get(ctx context.Context, db *sql.DB, objectID string) (*Entry, error) {
	s.mu.RLock()
	e, ok := s.entries[objectID]
	s.mu.RUnlock()
	if ok {
		return e, nil
	}

	e, err := generate(ctx, db, objectID)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	s.entries[objectID] = e
	s.mu.Unlock()
	return e, nil
}

// Invalidate drops the named entries so the next Get regenerates them from the database.
// Admin tools that write SDE tables (the ESP NPC authoring pages write invTypes and
// dgmTypeAttributes) call it. Clients holding the old version fetch the new one at their next
// login, when the regenerated timestamp no longer matches their cached hint.
func (s *Store) Invalidate(objectIDs ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range objectIDs {
		delete(s.entries, id)
	}
}

// InvalidateOwners drops the owners bulk so later logins see new characters or corporations;
// connected clients resolve new IDs through the cfg.eveowners miss path (config.GetOwnersEx).
// The bulk currently holds neither, so the call matters only if its composition (catalog.go)
// changes. See docs/support-notes.md#cachable-invalidation.
func (s *Store) InvalidateOwners() {
	s.invalidateAfter(nameInvalidationWindow, "config.BulkData.owners")
}

// InvalidateOwnerNames drops both name bulks an owner rename or alliance create touches.
// cfg.eveowners (Show Info, cfg.IsAlliance) and cfg.allianceshortnames ([TICK] on ship
// brackets, parklife/bracketMgr.py) are separate cachables; dropping one leaves the client
// half-updated.
func (s *Store) InvalidateOwnerNames() {
	s.invalidateAfter(nameInvalidationWindow,
		"config.BulkData.owners",
		"config.BulkData.allianceshortnames",
	)
}

// nameInvalidationWindow is how long a name invalidation waits for further ones, so a burst
// of renames regenerates the name bulks once. See docs/support-notes.md#cachable-invalidation.
const nameInvalidationWindow = 30 * time.Second

// invalidateAfter drops objectIDs once no further request for the same set has
// arrived for d. Each distinct set coalesces independently; a repeat request
// restarts that set's timer rather than queueing a second drop.
func (s *Store) invalidateAfter(d time.Duration, objectIDs ...string) {
	key := strings.Join(objectIDs, "\x00")

	s.debounceMu.Lock()
	defer s.debounceMu.Unlock()
	if s.debounce == nil {
		s.debounce = make(map[string]*time.Timer)
	}
	if t, ok := s.debounce[key]; ok {
		// Still pending: push it out. A burst therefore fires once, when the
		// burst stops, rather than once per member of it.
		t.Reset(d)
		return
	}
	s.debounce[key] = time.AfterFunc(d, func() {
		s.debounceMu.Lock()
		delete(s.debounce, key)
		s.debounceMu.Unlock()
		s.Invalidate(objectIDs...)
	})
}

// FlushInvalidations fires any pending debounced invalidation immediately.
// For shutdown, tests, and admin paths that want the panel's next read to be
// authoritative rather than up to nameInvalidationWindow stale.
func (s *Store) FlushInvalidations() {
	s.debounceMu.Lock()
	timers := make([]*time.Timer, 0, len(s.debounce))
	for _, t := range s.debounce {
		timers = append(timers, t)
	}
	s.debounceMu.Unlock()
	for _, t := range timers {
		t.Reset(0)
	}
}

// BuildTupleSet exposes the generic SQL→DBResultToTupleSet encoder for
// handlers outside the cache (the config.GetMulti*Ex single-table fetchers
// must produce rows in the same shape and column order as the bulk objects).
func BuildTupleSet(rows *sql.Rows) (marshal.PyRep, error) {
	return buildTupleSet(rows)
}

// InvalidateTypeData drops the cachables derived from invTypes and
// dgmTypeAttributes.
func (s *Store) InvalidateTypeData() {
	s.Invalidate(
		"config.BulkData.types",
		"config.BulkData.dgmtypeattribs",
	)
}

// LoadDir pre-populates entries from *.cache files in dir (optional dev shortcut).
// Already-cached entries are not overwritten.  Returns the number of files loaded.
func (s *Store) LoadDir(dir string) (int, error) {
	loaded, err := loadDir(dir)
	if err != nil {
		return 0, err
	}
	s.mu.Lock()
	for k, v := range loaded {
		if _, dup := s.entries[k]; !dup {
			s.entries[k] = v
		}
	}
	s.mu.Unlock()
	return len(loaded), nil
}

// LoginHints returns cachable hints for the login cachable objects only, for
// AuthenticationRsp.cachables; hinting charCreationInfo.* makes the client raise
// ErrMessageNotFound. Every entry goes through Get, so an invalidated one is regenerated,
// never omitted. See docs/support-notes.md#login-cachable-hints.
func (s *Store) LoginHints(ctx context.Context, db *sql.DB) marshal.PyDict {
	subset := make(map[string]*Entry, len(loginCachables))
	for _, id := range loginCachables {
		e, err := s.Get(ctx, db, id)
		if err != nil {
			log.Printf("cache: login cachable %q unavailable: %v", id, err)
			continue
		}
		subset[id] = e
	}
	return BuildHints(subset)
}

// Len returns the number of entries currently held in the store.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.entries)
}

// HintsForSet generates an entry (if not cached) for each item in the set and returns a
// PyDict keyed by item.ShortKey of util.CachedObject hints, for hCharCreateCachables and
// hAppearanceCachables. Missing entries are skipped.
func (s *Store) HintsForSet(ctx context.Context, db *sql.DB, set []HintSetEntry) marshal.PyDict {
	d := marshal.PyDict{}
	for _, item := range set {
		e, err := s.Get(ctx, db, item.ObjectID)
		if err != nil {
			continue
		}
		d.Items = append(d.Items, marshal.DictEntry{
			Key:   marshal.PyString{Value: item.ShortKey},
			Value: Hint(e),
		})
	}
	return d
}

// Prime generates every login cachable object from the database into memory so the first
// login pays no per-object query latency. A failed object is logged and does not abort the rest.
func (s *Store) Prime(ctx context.Context, db *sql.DB) {
	for _, id := range loginCachables {
		if _, err := s.Get(ctx, db, id); err != nil {
			// SDE tables may not be present yet; log and continue. The client gets an empty cachables
			// dict for any object that could not be generated.
			_ = err // logged inside Get → generate
		}
	}
}

// ── DB generation ────────────────────────────────────────────────────────────

// generate queries the database for objectID and returns a ready-to-serve Entry.
func generate(ctx context.Context, db *sql.DB, objectID string) (*Entry, error) {
	cat, ok := catalog[objectID]
	if !ok {
		return nil, fmt.Errorf("cache: unknown cachable object %q", objectID)
	}

	rows, err := db.QueryContext(ctx, cat.query)
	if err != nil {
		return nil, fmt.Errorf("cache: query %q: %w", objectID, err)
	}
	defer rows.Close()

	var rep marshal.PyRep
	switch cat.kind {
	case kindTupleSet:
		rep, err = buildTupleSet(rows)
	case kindRowset:
		rep, err = buildRowset(rows)
	case kindRowsetTuple:
		rep, err = buildRowsetTuple(rows)
	case kindIndexRowset:
		rep, err = buildIndexRowset(cat.key, rows)
	default:
		return nil, fmt.Errorf("cache: unknown result kind for %q", objectID)
	}
	if err != nil {
		return nil, fmt.Errorf("cache: build %q: %w", objectID, err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cache: rows %q: %w", objectID, err)
	}

	data, err := marshal.Marshal(rep)
	if err != nil {
		return nil, fmt.Errorf("cache: marshal %q: %w", objectID, err)
	}

	// Try zlib compression (RFC 1950); use the compressed bytes only if they are smaller.
	if compressed := zlibCompress(data); compressed != nil && len(compressed) < len(data) {
		data = compressed
	}

	return &Entry{
		ObjectID:  objectID,
		Timestamp: win32TimeNow(),
		Version:   crc32.ChecksumIEEE(data),
		Data:      data,
	}, nil
}

// ── Row-builder helpers ───────────────────────────────────────────────────────

// buildTupleSet produces PyTuple([col_names_list, rows_list_of_lists]).
func buildTupleSet(rows *sql.Rows) (marshal.PyRep, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	colReps := make([]marshal.PyRep, len(cols))
	for i, c := range cols {
		colReps[i] = marshal.PyString{Value: c}
	}

	var rowList []marshal.PyRep
	for rows.Next() {
		vals, err := scanRow(rows, len(cols))
		if err != nil {
			return nil, err
		}
		items := make([]marshal.PyRep, len(vals))
		for i, v := range vals {
			items[i] = goValToPy(v)
		}
		rowList = append(rowList, marshal.PyList{Items: items})
	}
	if rowList == nil {
		rowList = []marshal.PyRep{}
	}

	return marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyList{Items: colReps},
		marshal.PyList{Items: rowList},
	}}, nil
}

// buildRowset produces PyObject("util.Rowset", {header, RowClass, lines}).
func buildRowset(rows *sql.Rows) (marshal.PyRep, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	header := make([]marshal.PyRep, len(cols))
	for i, c := range cols {
		header[i] = marshal.PyString{Value: c}
	}

	var lines []marshal.PyRep
	for rows.Next() {
		vals, err := scanRow(rows, len(cols))
		if err != nil {
			return nil, err
		}
		items := make([]marshal.PyRep, len(vals))
		for i, v := range vals {
			items[i] = goValToPy(v)
		}
		lines = append(lines, marshal.PyList{Items: items})
	}
	if lines == nil {
		lines = []marshal.PyRep{}
	}

	return marshal.PyObject{
		Type: "util.Rowset",
		Args: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "header"}, Value: marshal.PyList{Items: header}},
			// IsType1=true encodes as Op_PyByteString (0x02), the ident form of the RowClass name.
			{Key: marshal.PyString{Value: "RowClass"}, Value: marshal.PyString{Value: "util.Row", IsType1: true}},
			{Key: marshal.PyString{Value: "lines"}, Value: marshal.PyList{Items: lines}},
		}},
	}, nil
}

// buildRowsetTuple produces PyTuple([None, util.Rowset]).
func buildRowsetTuple(rows *sql.Rows) (marshal.PyRep, error) {
	rs, err := buildRowset(rows)
	if err != nil {
		return nil, err
	}
	return marshal.PyTuple{Items: []marshal.PyRep{marshal.PyNone{}, rs}}, nil
}

// buildIndexRowset produces PyObject("util.IndexRowset", {idName, header, RowClass, items}).
func buildIndexRowset(key string, rows *sql.Rows) (marshal.PyRep, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	header := make([]marshal.PyRep, len(cols))
	keyIdx := -1
	for i, c := range cols {
		header[i] = marshal.PyString{Value: c}
		if c == key {
			keyIdx = i
		}
	}
	if keyIdx == -1 {
		return nil, fmt.Errorf("key column %q not found in result set", key)
	}

	items := marshal.PyDict{}
	for rows.Next() {
		vals, err := scanRow(rows, len(cols))
		if err != nil {
			return nil, err
		}
		rowReps := make([]marshal.PyRep, len(vals))
		for i, v := range vals {
			rowReps[i] = goValToPy(v)
		}
		items.Items = append(items.Items, marshal.DictEntry{
			Key:   goValToPy(vals[keyIdx]),
			Value: marshal.PyList{Items: rowReps},
		})
	}

	return marshal.PyObject{
		Type: "util.IndexRowset",
		Args: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "RowClass"}, Value: marshal.PyString{Value: "util.Row", IsType1: true}},
			{Key: marshal.PyString{Value: "idName"}, Value: marshal.PyString{Value: key}},
			{Key: marshal.PyString{Value: "header"}, Value: marshal.PyList{Items: header}},
			{Key: marshal.PyString{Value: "items"}, Value: items},
		}},
	}, nil
}

// ── Low-level helpers ─────────────────────────────────────────────────────────

// scanRow scans the current row into a slice of interface{} values.
func scanRow(rows *sql.Rows, n int) ([]interface{}, error) {
	vals := make([]interface{}, n)
	ptrs := make([]interface{}, n)
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	return vals, rows.Scan(ptrs...)
}

// goValToPy converts a Go value returned by sql.Rows.Scan to a PyRep.
func goValToPy(v interface{}) marshal.PyRep {
	if v == nil {
		return marshal.PyNone{}
	}
	switch val := v.(type) {
	case int64:
		return marshal.PyInt{Value: uint64(val)}
	case int32:
		return marshal.PyInt{Value: uint64(val)}
	case int16:
		return marshal.PyInt{Value: uint64(val)}
	case int8:
		return marshal.PyInt{Value: uint64(val)}
	case uint64:
		return marshal.PyInt{Value: val}
	case uint32:
		return marshal.PyInt{Value: uint64(val)}
	case float64:
		// MySQL IF()/COALESCE() return DOUBLE even for integer columns; whole-number floats become
		// PyInt because the Python 2 client uses them as list indices and dict keys.
		if !math.IsNaN(val) && !math.IsInf(val, 0) && val == math.Trunc(val) {
			return marshal.PyInt{Value: uint64(int64(val))}
		}
		return marshal.PyReal{Value: val}
	case float32:
		f := float64(val)
		if !math.IsNaN(f) && !math.IsInf(f, 0) && f == math.Trunc(f) {
			return marshal.PyInt{Value: uint64(int64(f))}
		}
		return marshal.PyReal{Value: f}
	case bool:
		return marshal.PyBool{Value: val}
	case []byte:
		return marshal.PyString{Value: string(val)}
	case string:
		return marshal.PyString{Value: val}
	default:
		return marshal.PyString{Value: fmt.Sprintf("%v", v)}
	}
}

// zlibCompress compresses data with zlib (RFC 1950). Returns nil if compression fails.
func zlibCompress(data []byte) []byte {
	var buf bytes.Buffer
	w := zlib.NewWriter(&buf)
	if _, err := w.Write(data); err != nil {
		return nil
	}
	if err := w.Close(); err != nil {
		return nil
	}
	return buf.Bytes()
}

// win32TimeNow returns the current time as Windows FILETIME (100-ns intervals since
// 1601-01-01 00:00:00 UTC).
func win32TimeNow() uint64 {
	return uint64(time.Now().UnixNano()/100) + win32Epoch
}
