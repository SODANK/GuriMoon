// Package contentpkg exports dungeons, mission templates, open-site spawners and the custom NPC
// types they use as one portable file (.gmpkg), and installs such a file on another server.
// The format and install rules are described in docs/content-packages.md.
package contentpkg

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// FormatVersion is the package layout version written to manifest.json.
const FormatVersion = 1

// ServerBuild names the client build the content was authored for.
const ServerBuild = "RMR 2.10.3913"

// CustomTypeFloor is the first typeID of the custom NPC type band (docs/npc-authoring.md).
const CustomTypeFloor = 100000

// FileExt is the package file extension.
const FileExt = ".gmpkg"

// Row is one table row by column name. Values are nil, int64, json.Number or string.
// A row with its own primary key carries its package key under KeyField; a column that points
// at another package row holds that row's key as "@<key>".
type Row map[string]any

// KeyField is the row field holding the row's package key.
const KeyField = "$key"

// Key returns the row's package key, or "" when it has none.
func (r Row) Key() string {
	s, _ := r[KeyField].(string)
	return s
}

// Package is a decoded .gmpkg file.
type Package struct {
	Manifest Manifest
	Dungeons []Dungeon
	Missions []Row
	Spawners []Row
	Types    []CustomType
	// Readme is README.md, shown to the admin before install. Empty means no file.
	Readme string
}

// Dungeon is one dungeon with all of its rooms, balls, ball loot and Dungeon Logic triggers.
type Dungeon struct {
	Dungeon  Row   `json:"dungeon"`
	Rooms    []Row `json:"rooms"`
	Objects  []Row `json:"objects"`
	Loot     []Row `json:"loot"`
	Triggers []Row `json:"triggers"`
}

// CustomType is one custom NPC type: its invTypes row, attribute and effect rows, and the NPC
// loot rows scoped to it.
type CustomType struct {
	Type       Row   `json:"type"`
	Attributes []Row `json:"attributes"`
	Effects    []Row `json:"effects,omitempty"`
	Loot       []Row `json:"loot,omitempty"`
	LootCaps   []Row `json:"lootCaps,omitempty"`
}

// Manifest is manifest.json: who made the package, what it holds and what base data it needs.
type Manifest struct {
	Format      int            `json:"format"`
	UUID        string         `json:"uuid"`
	Name        string         `json:"name"`
	Version     string         `json:"version"`
	Author      string         `json:"author,omitempty"`
	Description string         `json:"description,omitempty"`
	Created     string         `json:"created"`
	Build       string         `json:"build,omitempty"`
	Contents    []ContentEntry `json:"contents"`
	Requires    Requires       `json:"requires"`
}

// ContentEntry lists one top-level item of the package.
type ContentEntry struct {
	Kind string `json:"kind"` // dungeon, mission, spawner or npc
	Key  string `json:"key"`
	Name string `json:"name"`
	File string `json:"file"`
}

// Requires lists the base game data the package refers to by ID. The importer checks each one
// exists on the target server.
type Requires struct {
	Types    []BaseRef `json:"types,omitempty"`
	Groups   []BaseRef `json:"groups,omitempty"`
	Factions []BaseRef `json:"factions,omitempty"`
	Stations []BaseRef `json:"stations,omitempty"`
	Regions  []BaseRef `json:"regions,omitempty"`
}

// BaseRef is a base-data ID with the name it had on the exporting server.
type BaseRef struct {
	ID   int64  `json:"id"`
	Name string `json:"name,omitempty"`
}

// Counts returns how many rows of each kind the package holds.
func (p *Package) Counts() map[string]int {
	c := map[string]int{
		"dungeons": len(p.Dungeons), "missions": len(p.Missions),
		"spawners": len(p.Spawners), "npcs": len(p.Types),
	}
	for _, d := range p.Dungeons {
		c["rooms"] += len(d.Rooms)
		c["objects"] += len(d.Objects)
		c["triggers"] += len(d.Triggers)
	}
	return c
}

// NewUUID returns a random version 4 UUID.
func NewUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// validUUID reports whether s is a UUID in canonical form: 36 characters, lower-case hex and dashes.
func validUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !strings.ContainsRune("0123456789abcdef", c) {
				return false
			}
		}
	}
	return true
}

// keyPattern is the form of every package key: the table's prefix letter, the row's ID on the
// exporting server, and an optional _N suffix that keeps re-exported keys unique.
var keyPattern = regexp.MustCompile(`^[a-z][0-9]{1,10}(_[0-9]{1,4})?$`)

// refValue is the column value that points at the package row with the given key.
func refValue(key string) string { return "@" + key }

// refKey returns the key a column value points at, or "" when the value is not a reference.
func refKey(v any) string {
	s, ok := v.(string)
	if !ok || len(s) < 2 || s[0] != '@' {
		return ""
	}
	return s[1:]
}

// asInt reads an integer from a row value. ok is false for NULL and non-numeric values.
func asInt(v any) (int64, bool) {
	switch x := v.(type) {
	case int64:
		return x, true
	case int:
		return int64(x), true
	case float64:
		return int64(x), true
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return n, true
		}
		if f, err := x.Float64(); err == nil {
			return int64(f), true
		}
	case string:
		if n, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64); err == nil {
			return n, true
		}
	}
	return 0, false
}

// asString reads a row value as text; NULL reads as "".
func asString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case json.Number:
		return x.String()
	case int64:
		return strconv.FormatInt(x, 10)
	}
	return fmt.Sprint(v)
}

// splitIDs parses a comma-separated ID list such as dunSpawners.regionIDs.
func splitIDs(s string) []int64 {
	var out []int64
	for _, part := range strings.Split(s, ",") {
		if n, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64); err == nil && n != 0 {
			out = append(out, n)
		}
	}
	return out
}

// slug makes a short file-name-safe form of a name.
func slug(s string) string {
	var b strings.Builder
	dash := false
	for _, c := range strings.ToLower(s) {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
			b.WriteRune(c)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
		if b.Len() >= 40 {
			break
		}
	}
	return strings.Trim(b.String(), "-")
}

// Slug is the file-name-safe form of a package name.
func Slug(s string) string {
	if out := slug(s); out != "" {
		return out
	}
	return "package"
}
