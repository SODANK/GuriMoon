package marshal

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
)

// StringTable maps between 1-based byte indices and string values used by
// Op_PyStringTableItem.  EVE ships a text file (one string per line) that both
// client and server must agree on.
type StringTable struct {
	byIndex []string        // index 0 == table entry 1 (1-based on the wire)
	byValue map[string]byte // value → 1-based wire index
}

// globalStringTable is used during (un)marshaling. It starts empty; LoadStringTable or
// SetStringTable populates it before traffic. Atomic because SetStringTable can race with
// connection goroutines that are already (un)marshaling.
var globalStringTable atomic.Pointer[StringTable]

func init() {
	globalStringTable.Store(&StringTable{byValue: make(map[string]byte)})
}

// LoadStringTable replaces the global string table by reading path (one entry
// per line, UTF-8, up to 255 entries).
func LoadStringTable(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("LoadStringTable: %w", err)
	}
	defer f.Close()

	var entries []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		entries = append(entries, strings.TrimRight(sc.Text(), "\r\n"))
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("LoadStringTable scan: %w", err)
	}

	SetStringTable(entries)
	return nil
}

// SetStringTable replaces the global string table with the supplied slice.
// The first entry gets wire index 1.  At most 255 entries are used.
func SetStringTable(entries []string) {
	t := &StringTable{
		byIndex: entries,
		byValue: make(map[string]byte, len(entries)),
	}
	for i, s := range entries {
		if i >= 0xFF {
			break
		}
		t.byValue[s] = byte(i + 1) // 1-based
	}
	globalStringTable.Store(t)
}

// LookupIndex returns the string for a 1-based wire index, or "" if not found.
func (t *StringTable) LookupIndex(idx byte) string {
	if idx == 0 || int(idx) > len(t.byIndex) {
		return ""
	}
	return t.byIndex[idx-1]
}

// LookupString returns the 1-based wire index for s, or (0, false) if absent.
func (t *StringTable) LookupString(s string) (byte, bool) {
	idx, ok := t.byValue[s]
	return idx, ok
}
