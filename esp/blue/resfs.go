package blue

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ── Client resource lookup ───────────────────────────────────────────────────
//
// The client resolves "res:/..." through its own layering: loose files under
// <client>/res win over the .stuff archives. The ESP reads the same way, with
// the user's extracted archives (<client>/extracted_res*/res) in between so a
// path is found wherever it was unpacked. Nothing is copied or converted on
// the way in; every read is served straight from the client folder.

type layer interface {
	read(key string) ([]byte, bool, error) // key: lowercase, slash-separated, no "res/" prefix
	walk(fn func(key string))
	name() string
}

type dirLayer struct{ root string }

func (l dirLayer) name() string { return l.root }

func (l dirLayer) read(key string) ([]byte, bool, error) {
	p := filepath.Join(l.root, filepath.FromSlash(key))
	b, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return b, true, nil
}

func (l dirLayer) walk(fn func(key string)) {
	_ = filepath.WalkDir(l.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr
		}
		rel, err := filepath.Rel(l.root, p)
		if err == nil {
			fn(strings.ToLower(filepath.ToSlash(rel)))
		}
		return nil
	})
}

// stuffLayer is an EmbedFs archive, indexed once and read on demand:
// u32 count, then per entry { u32 size, u32 nameLen, name, NUL }, then every
// blob concatenated in index order.
type stuffLayer struct {
	path  string
	index map[string][2]int64 // key → {offset, size}
}

func (l *stuffLayer) name() string { return l.path }

func openStuff(path string) (*stuffLayer, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var hdr [4]byte
	if _, err := f.ReadAt(hdr[:], 0); err != nil {
		return nil, err
	}
	count := int(binary.LittleEndian.Uint32(hdr[:]))
	if count <= 0 || count > 1<<20 {
		return nil, fmt.Errorf("%s: implausible entry count %d", path, count)
	}
	// The index is small next to the blobs; read it in one go by growing a
	// window until every entry fits.
	l := &stuffLayer{path: path, index: make(map[string][2]int64, count)}
	type ent struct {
		key  string
		size int64
	}
	ents := make([]ent, 0, count)
	buf := make([]byte, 1<<20)
	n, _ := f.ReadAt(buf, 4)
	buf = buf[:n]
	pos := 0
	for i := 0; i < count; i++ {
		if pos+8 > len(buf) {
			more := make([]byte, len(buf)*2)
			copy(more, buf)
			n, _ := f.ReadAt(more[len(buf):], int64(4+len(buf)))
			buf = more[:len(buf)+n]
			if pos+8 > len(buf) {
				return nil, fmt.Errorf("%s: index truncated at entry %d", path, i)
			}
		}
		size := int64(binary.LittleEndian.Uint32(buf[pos:]))
		nameLen := int(binary.LittleEndian.Uint32(buf[pos+4:]))
		pos += 8
		for pos+nameLen+1 > len(buf) {
			more := make([]byte, len(buf)*2)
			copy(more, buf)
			n, _ := f.ReadAt(more[len(buf):], int64(4+len(buf)))
			if n == 0 {
				return nil, fmt.Errorf("%s: name truncated at entry %d", path, i)
			}
			buf = more[:len(buf)+n]
		}
		name := strings.ToLower(strings.ReplaceAll(string(buf[pos:pos+nameLen]), `\`, "/"))
		pos += nameLen + 1
		ents = append(ents, ent{strings.TrimPrefix(name, "res/"), size})
	}
	off := int64(4 + pos)
	for _, e := range ents {
		l.index[e.key] = [2]int64{off, e.size}
		off += e.size
	}
	return l, nil
}

func (l *stuffLayer) read(key string) ([]byte, bool, error) {
	ent, ok := l.index[key]
	if !ok {
		return nil, false, nil
	}
	f, err := os.Open(l.path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	b := make([]byte, ent[1])
	if _, err := f.ReadAt(b, ent[0]); err != nil {
		return nil, false, err
	}
	return b, true, nil
}

func (l *stuffLayer) walk(fn func(key string)) {
	for k := range l.index {
		fn(k)
	}
}

// ResFS reads client resources by "res:/" path.
type ResFS struct{ layers []layer }

// Open mounts <client>/res, every <client>/extracted_res*/res and every
// <client>/*.stuff, in that precedence. It returns nil, nil when the client
// folder holds nothing to mount.
func Open(clientPath string) (*ResFS, error) {
	if clientPath == "" {
		return nil, nil
	}
	fsys := &ResFS{}
	if st, err := os.Stat(filepath.Join(clientPath, "res")); err == nil && st.IsDir() {
		fsys.layers = append(fsys.layers, dirLayer{filepath.Join(clientPath, "res")})
	}
	extracted, _ := filepath.Glob(filepath.Join(clientPath, "extracted_res*"))
	sort.Strings(extracted)
	for _, d := range extracted {
		if st, err := os.Stat(filepath.Join(d, "res")); err == nil && st.IsDir() {
			fsys.layers = append(fsys.layers, dirLayer{filepath.Join(d, "res")})
		}
	}
	stuffs, _ := filepath.Glob(filepath.Join(clientPath, "*.stuff"))
	sort.Strings(stuffs)
	for _, p := range stuffs {
		l, err := openStuff(p)
		if err != nil {
			return nil, err
		}
		fsys.layers = append(fsys.layers, l)
	}
	if len(fsys.layers) == 0 {
		return nil, nil
	}
	return fsys, nil
}

// Mounts lists the mounted layers in precedence order.
func (r *ResFS) Mounts() []string {
	if r == nil {
		return nil
	}
	out := make([]string, len(r.layers))
	for i, l := range r.layers {
		out[i] = l.name()
	}
	return out
}

// Key normalises a "res:/Model/X.blue" path to the lookup key
// ("model/x.blue").
func Key(resPath string) string {
	p := strings.ReplaceAll(strings.TrimSpace(resPath), `\`, "/")
	if i := strings.Index(p, ":/"); i >= 0 && strings.EqualFold(p[:i], "res") {
		p = p[i+2:]
	}
	p = strings.TrimLeft(p, "/")
	if strings.HasPrefix(strings.ToLower(p), "res/") {
		p = p[4:]
	}
	return strings.ToLower(p)
}

// Read returns the resource's bytes, or fs.ErrNotExist.
func (r *ResFS) Read(resPath string) ([]byte, error) {
	if r == nil {
		return nil, fs.ErrNotExist
	}
	key := Key(resPath)
	for _, l := range r.layers {
		b, ok, err := l.read(key)
		if err != nil {
			return nil, err
		}
		if ok {
			return b, nil
		}
	}
	return nil, fmt.Errorf("%s: %w", resPath, fs.ErrNotExist)
}

// Exists reports whether the resource can be read.
func (r *ResFS) Exists(resPath string) bool {
	if r == nil {
		return false
	}
	key := Key(resPath)
	for _, l := range r.layers {
		if dl, ok := l.(dirLayer); ok {
			if _, err := os.Stat(filepath.Join(dl.root, filepath.FromSlash(key))); err == nil {
				return true
			}
			continue
		}
		if _, ok := l.(*stuffLayer).index[key]; ok {
			return true
		}
	}
	return false
}

// Walk calls fn once per distinct key under prefix (lowercase, e.g. "model/"),
// first layer wins.
func (r *ResFS) Walk(prefix string, fn func(key string)) {
	if r == nil {
		return
	}
	seen := map[string]bool{}
	prefix = strings.ToLower(prefix)
	for _, l := range r.layers {
		l.walk(func(k string) {
			if strings.HasPrefix(k, prefix) && !seen[k] {
				seen[k] = true
				fn(k)
			}
		})
	}
}
