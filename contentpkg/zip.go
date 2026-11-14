package contentpkg

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Limits on a package file.
const (
	MaxPackageBytes = 64 << 20  // the .gmpkg file
	maxUnpacked     = 256 << 20 // all entries uncompressed
	maxEntryBytes   = 64 << 20  // one entry uncompressed
	maxEntries      = maxDungeons + maxMissions + maxSpawners + maxTypes + 8
	maxReadmeBytes  = 64 << 10
	maxJSONDepth    = 6
)

// encodeJSON renders a value as indented JSON with a trailing newline.
func encodeJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// assignFiles gives every item its file name in the zip and lists it in the manifest, in the
// order dungeons' NPC types, dungeons, missions, spawners.
func assignFiles(p *Package) {
	p.Manifest.Contents = p.Manifest.Contents[:0]
	used := map[string]bool{}
	add := func(kind, key, name string) {
		e := entry(kind, key, name)
		base := strings.TrimSuffix(e.File, ".json")
		for n := 2; used[e.File]; n++ {
			e.File = fmt.Sprintf("%s-%d.json", base, n)
		}
		used[e.File] = true
		p.Manifest.Contents = append(p.Manifest.Contents, e)
	}
	for _, ct := range p.Types {
		add("npc", ct.Type.Key(), asString(ct.Type["typeName"]))
	}
	for _, d := range p.Dungeons {
		add("dungeon", d.Dungeon.Key(), asString(d.Dungeon["dungeonVName"]))
	}
	for _, m := range p.Missions {
		add("mission", m.Key(), asString(m["missionName"]))
	}
	for _, s := range p.Spawners {
		add("spawner", s.Key(), asString(s["name"]))
	}
}

// Write writes the package as a .gmpkg zip: manifest.json, one JSON file per item, README.md
// when there is one, and install.sql generated from the same data.
func Write(w io.Writer, p *Package) error {
	assignFiles(p)
	if _, err := validate(p); err != nil {
		return err
	}
	zw := zip.NewWriter(w)
	now := time.Now()
	put := func(name string, data []byte) error {
		f, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: now})
		if err != nil {
			return err
		}
		_, err = f.Write(data)
		return err
	}
	write := func(name string, v any) error {
		data, err := encodeJSON(v)
		if err != nil {
			return err
		}
		return put(name, data)
	}
	if err := write("manifest.json", p.Manifest); err != nil {
		return err
	}
	i := 0
	next := func() string { i++; return p.Manifest.Contents[i-1].File }
	for _, ct := range p.Types {
		if err := write(next(), ct); err != nil {
			return err
		}
	}
	for _, d := range p.Dungeons {
		if err := write(next(), d); err != nil {
			return err
		}
	}
	for _, m := range p.Missions {
		if err := write(next(), m); err != nil {
			return err
		}
	}
	for _, s := range p.Spawners {
		if err := write(next(), s); err != nil {
			return err
		}
	}
	if p.Readme != "" {
		if err := put("README.md", []byte(p.Readme)); err != nil {
			return err
		}
	}
	var sqlBuf bytes.Buffer
	if err := WriteSQL(&sqlBuf, p); err != nil {
		return err
	}
	if err := put("install.sql", sqlBuf.Bytes()); err != nil {
		return err
	}
	return zw.Close()
}

// Read decodes and validates a .gmpkg file. The zip may hold manifest.json, install.sql,
// README.md, the folders and exactly the item files the manifest lists; anything else rejects
// the file. install.sql must be exactly what this server would generate from the JSON, so a
// package cannot carry SQL that differs from what the panel shows.
func Read(data []byte) (*Package, error) {
	if len(data) > MaxPackageBytes {
		return nil, fmt.Errorf("package is larger than %d MB", MaxPackageBytes>>20)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("not a package file: %w", err)
	}
	if len(zr.File) > maxEntries {
		return nil, fmt.Errorf("package has %d files, more than %d", len(zr.File), maxEntries)
	}
	if zr.Comment != "" {
		return nil, errors.New("package zip carries a comment")
	}
	files := map[string]*zip.File{}
	var total uint64
	for _, f := range zr.File {
		name := f.Name
		if files[name] != nil {
			return nil, fmt.Errorf("package holds %s twice", name)
		}
		switch {
		case name == "dungeons/" || name == "missions/" || name == "spawners/" || name == "npcs/":
			if !f.FileInfo().IsDir() {
				return nil, fmt.Errorf("package entry %s is not a folder", name)
			}
		case name == "manifest.json" || name == "install.sql" || name == "README.md" || filePattern.MatchString(name):
			if !f.Mode().IsRegular() {
				return nil, fmt.Errorf("package entry %s is not a plain file", name)
			}
		default:
			return nil, fmt.Errorf("package entry %q is not part of the format", name)
		}
		if f.Method != zip.Store && f.Method != zip.Deflate {
			return nil, fmt.Errorf("package entry %s uses compression method %d", name, f.Method)
		}
		if f.Flags&0x1 != 0 {
			return nil, fmt.Errorf("package entry %s is encrypted", name)
		}
		if f.UncompressedSize64 > maxEntryBytes {
			return nil, fmt.Errorf("package entry %s is larger than %d MB", name, maxEntryBytes>>20)
		}
		total += f.UncompressedSize64
		if total > maxUnpacked {
			return nil, fmt.Errorf("package unpacks to more than %d MB", maxUnpacked>>20)
		}
		files[name] = f
	}

	p := &Package{}
	mf := files["manifest.json"]
	if mf == nil {
		return nil, errors.New("package has no manifest.json")
	}
	if err := decodeEntry(mf, &p.Manifest); err != nil {
		return nil, err
	}
	used := map[string]bool{"manifest.json": true}
	for _, c := range p.Manifest.Contents {
		if !filePattern.MatchString(c.File) || kindFolder[c.Kind] == "" || !strings.HasPrefix(c.File, kindFolder[c.Kind]+"/") {
			return nil, fmt.Errorf("manifest file name %q for %s %s is not allowed", c.File, c.Kind, c.Key)
		}
		f := files[c.File]
		if f == nil {
			return nil, fmt.Errorf("manifest lists %s but the package does not hold it", c.File)
		}
		if used[c.File] {
			return nil, fmt.Errorf("manifest lists %s twice", c.File)
		}
		used[c.File] = true
		var key string
		switch c.Kind {
		case "npc":
			var ct CustomType
			if err := decodeEntry(f, &ct); err != nil {
				return nil, err
			}
			p.Types = append(p.Types, ct)
			key = ct.Type.Key()
		case "dungeon":
			var d Dungeon
			if err := decodeEntry(f, &d); err != nil {
				return nil, err
			}
			p.Dungeons = append(p.Dungeons, d)
			key = d.Dungeon.Key()
		case "mission":
			var r Row
			if err := decodeEntry(f, &r); err != nil {
				return nil, err
			}
			p.Missions = append(p.Missions, r)
			key = r.Key()
		case "spawner":
			var r Row
			if err := decodeEntry(f, &r); err != nil {
				return nil, err
			}
			p.Spawners = append(p.Spawners, r)
			key = r.Key()
		}
		if key != c.Key {
			return nil, fmt.Errorf("%s holds key %q but the manifest says %q", c.File, key, c.Key)
		}
	}
	if f := files["README.md"]; f != nil {
		used["README.md"] = true
		if f.UncompressedSize64 > maxReadmeBytes {
			return nil, fmt.Errorf("README.md is larger than %d KB", maxReadmeBytes>>10)
		}
		b, err := readEntry(f, maxReadmeBytes)
		if err != nil {
			return nil, err
		}
		if len(b) == 0 {
			return nil, errors.New("README.md is empty")
		}
		p.Readme = string(b)
	}
	sqlFile := files["install.sql"]
	if sqlFile == nil {
		return nil, errors.New("package has no install.sql")
	}
	used["install.sql"] = true
	for name, f := range files {
		if !used[name] && !f.FileInfo().IsDir() {
			return nil, fmt.Errorf("package holds %s, which the manifest does not list", name)
		}
	}
	if _, err := validate(p); err != nil {
		return nil, err
	}
	got, err := readEntry(sqlFile, maxEntryBytes)
	if err != nil {
		return nil, err
	}
	var want bytes.Buffer
	if err := WriteSQL(&want, p); err != nil {
		return nil, err
	}
	if !bytes.Equal(got, want.Bytes()) {
		return nil, errors.New("install.sql does not match the package data; the file was changed after export")
	}
	return p, nil
}

func readEntry(f *zip.File, limit int64) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", f.Name, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", f.Name, err)
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s is too large", f.Name)
	}
	return b, nil
}

// decodeEntry reads one JSON entry strictly: no unknown fields, no repeated keys, nothing after
// the value, numbers kept exact.
func decodeEntry(f *zip.File, target any) error {
	b, err := readEntry(f, maxEntryBytes)
	if err != nil {
		return err
	}
	if err := checkJSON(b); err != nil {
		return fmt.Errorf("%s: %w", f.Name, err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return fmt.Errorf("%s: %w", f.Name, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("%s: data after the JSON value", f.Name)
	}
	return nil
}

// checkJSON walks a JSON document and rejects repeated object keys and nesting deeper than the
// format uses.
func checkJSON(b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	type frame struct {
		object bool
		keys   map[string]bool
		key    bool // the next token in this object is a key
	}
	var stack []*frame
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		var top *frame
		if len(stack) > 0 {
			top = stack[len(stack)-1]
		}
		if top != nil && top.object && top.key {
			if d, ok := tok.(json.Delim); ok && d == '}' {
				stack = stack[:len(stack)-1]
				continue
			}
			k := tok.(string)
			if top.keys[k] {
				return fmt.Errorf("key %q appears twice in one object", k)
			}
			top.keys[k] = true
			top.key = false
			continue
		}
		if top != nil && top.object {
			top.key = true
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{':
				stack = append(stack, &frame{object: true, keys: map[string]bool{}, key: true})
			case '[':
				stack = append(stack, &frame{})
			case ']':
				stack = stack[:len(stack)-1]
			}
			if len(stack) > maxJSONDepth {
				return errors.New("JSON nests deeper than the format allows")
			}
		}
	}
}
