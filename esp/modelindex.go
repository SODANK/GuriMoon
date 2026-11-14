package esp

import (
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ---- Converted .glb model index ----
//
// eveGraphics.url3D points at a .blue in the client's archive. The offline converter writes a
// loosely matching tree of .glb files, so modelIndex scans the model directories once and
// resolves a url3D in explicit tiers, exact match first. See docs/esp-2-notes.md#glb-resolution.

// modelIndex is an immutable snapshot of the converted model tree. Swap the
// whole value under mu to rescan; readers hold no lock beyond the pointer read.
type modelIndex struct {
	// byDir maps a lowercase slash-separated directory (relative to a model
	// root) to the .glb file names it holds, sorted case-insensitively.
	byDir map[string][]string
	// dirPath maps that same lowercase directory to its absolute on-disk path.
	dirPath map[string]string
	// byBase maps a lowercase file basename (no .glb) to absolute paths,
	// shortest path first. Recovers models whose url3D lost its separators.
	byBase map[string][]string

	count   int       // total .glb files indexed
	scanned time.Time // when this snapshot was built
	gen     int64     // bumped on every rescan, busts browser caches
}

// buildModelIndex walks every model root and indexes the .glb files found.
func buildModelIndex(dirs []string, gen int64) *modelIndex {
	mi := &modelIndex{
		byDir:   make(map[string][]string),
		dirPath: make(map[string]string),
		byBase:  make(map[string][]string),
		scanned: time.Now(),
		gen:     gen,
	}
	for _, root := range dirs {
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil //nolint:nilerr // skip unreadable subtrees
			}
			name := d.Name()
			if !strings.HasSuffix(strings.ToLower(name), ".glb") {
				return nil
			}
			rel, relErr := filepath.Rel(root, filepath.Dir(path))
			if relErr != nil {
				return nil
			}
			key := indexDirKey(rel)
			mi.byDir[key] = append(mi.byDir[key], name)
			mi.dirPath[key] = filepath.Dir(path)

			base := strings.ToLower(name[:len(name)-len(".glb")])
			mi.byBase[base] = append(mi.byBase[base], path)
			mi.count++
			return nil
		})
	}
	for k, names := range mi.byDir {
		sort.Slice(names, func(i, j int) bool {
			return strings.ToLower(names[i]) < strings.ToLower(names[j])
		})
		mi.byDir[k] = names
	}
	for k, paths := range mi.byBase {
		sort.Slice(paths, func(i, j int) bool {
			if len(paths[i]) != len(paths[j]) {
				return len(paths[i]) < len(paths[j])
			}
			return paths[i] < paths[j]
		})
		mi.byBase[k] = paths
	}
	return mi
}

// indexDirKey normalises a relative directory to the map key form: lowercase,
// forward slashes, no leading "./".
func indexDirKey(rel string) string {
	rel = filepath.ToSlash(rel)
	rel = strings.TrimPrefix(rel, "./")
	if rel == "." {
		rel = ""
	}
	return strings.ToLower(rel)
}

// modelSnapshot returns the current index, building it lazily on first use.
func (s *Server) modelSnapshot() *modelIndex {
	s.modelsMu.RLock()
	mi := s.models
	s.modelsMu.RUnlock()
	if mi != nil {
		return mi
	}
	return s.rescanModels()
}

// rescanModels rebuilds the index from disk and returns the new snapshot. Requests in flight
// keep the old snapshot until it returns.
func (s *Server) rescanModels() *modelIndex {
	s.modelsMu.Lock()
	gen := int64(1)
	if s.models != nil {
		gen = s.models.gen + 1
	}
	s.modelsMu.Unlock()

	mi := buildModelIndex(s.modelsDirs, gen)

	s.modelsMu.Lock()
	s.models = mi
	s.modelsMu.Unlock()
	return mi
}

// resolveGLB maps an eveGraphics url3D to a converted .glb on disk, or "" when nothing matches.
// Tiers, first hit wins: exact name, <base>shape, shortest prefix in the directory, basename
// anywhere in the tree, then longest suffix for separator-less values.
// See docs/esp-2-notes.md#glb-resolution.
func (s *Server) resolveGLB(url3D string) string {
	if url3D == "" || len(s.modelsDirs) == 0 {
		return ""
	}
	mi := s.modelSnapshot()

	resPath := url3D
	for _, pre := range []string{"res:/", "res:\\"} {
		if len(resPath) >= len(pre) && strings.EqualFold(resPath[:len(pre)], pre) {
			resPath = resPath[len(pre):]
			break
		}
	}
	resPath = strings.ReplaceAll(resPath, "\\", "/")
	base := strings.ToLower(strings.TrimSuffix(filepath.Base(resPath), filepath.Ext(resPath)))
	if base == "" {
		return ""
	}
	dirKey := indexDirKey(path2dir(resPath))

	names, dirKnown := mi.byDir[dirKey]
	if dirKnown {
		dir := mi.dirPath[dirKey]
		// Tier 1 & 2: exact filename.
		for _, want := range []string{base + ".glb", base + "shape.glb"} {
			for _, n := range names {
				if strings.EqualFold(n, want) {
					return filepath.Join(dir, n)
				}
			}
		}
		// Tier 3: shortest prefix match.
		best := ""
		for _, n := range names {
			if !strings.HasPrefix(strings.ToLower(n), base) {
				continue
			}
			if best == "" || len(n) < len(best) {
				best = n
			}
		}
		if best != "" {
			return filepath.Join(dir, best)
		}
	}

	// Tier 4: basename lookup across the whole tree.
	for _, want := range []string{base, base + "shape"} {
		if paths := mi.byBase[want]; len(paths) > 0 {
			return paths[0]
		}
	}

	// Tier 5: separator-less url3D, attempted only when the stated directory is unknown, so a
	// well-formed path with a missing model resolves to nothing.
	if !dirKnown {
		const minSuffix = 3
		for i := 1; i+minSuffix <= len(base); i++ {
			suffix := base[i:]
			for _, want := range []string{suffix, suffix + "shape"} {
				if paths := mi.byBase[want]; len(paths) > 0 {
					return paths[0]
				}
			}
		}
	}
	return ""
}

// path2dir is filepath.Dir for an already slash-normalised path.
func path2dir(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[:i]
	}
	return ""
}

// serveGLB writes a resolved .glb with caching headers keyed to the index
// generation, so a rescan invalidates whatever the browser already holds.
func (s *Server) serveGLB(w http.ResponseWriter, r *http.Request, path string) {
	if _, err := os.Stat(path); err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "model/gltf-binary")
	// private: the panel is behind auth. Callers append the index generation
	// (?g=N) to the URL, so a rescan busts this without a short max-age.
	w.Header().Set("Cache-Control", "private, max-age=3600")
	http.ServeFile(w, r, path)
}

// handleModelsRescan rebuilds the model index from disk without a restart (the "Rescan models"
// button in the NPC model picker).
//
// POST /api/models/rescan
func (s *Server) handleModelsRescan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	mi := s.rescanModels()
	s.resetModelFailures()
	purged := 0
	if r.URL.Query().Get("purge") == "1" {
		purged = s.purgeModelCache()
	}
	log.Printf("ESP model index rescanned: %d .glb files across %d directories (%d cached builds purged)", mi.count, len(s.modelsDirs), purged)
	jsonOK(w, map[string]any{
		"files":       mi.count,
		"directories": len(mi.byDir),
		"roots":       s.modelsDirs,
		"generation":  s.modelGeneration(),
		"purged":      purged,
		"blue":        s.modelPipelineStatus(),
	})
}

// handleModelsStatus reports what the index currently holds.
//
// GET /api/models/status
func (s *Server) handleModelsStatus(w http.ResponseWriter, r *http.Request) {
	mi := s.modelSnapshot()
	jsonOK(w, map[string]any{
		"files":       mi.count,
		"directories": len(mi.byDir),
		"roots":       s.modelsDirs,
		"generation":  s.modelGeneration(),
		"scannedAt":   mi.scanned.Format(time.RFC3339),
		"blue":        s.modelPipelineStatus(),
	})
}
