package esp

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"gurimoon/esp/blue"
)

// ── Models built from the client's own files ────────────────────────────────
//
// /api/model/{typeID}.glb reads the .blue and the .tri meshes it names from the client folder,
// composes them as the client does (esp/blue) and caches the glb under data/modelcache. The cache
// is derived data and can be deleted. The converter's tree is the fallback for anything the
// pipeline cannot read. See docs/blue-format.md#composing-the-hull.

const modelCacheDirName = "modelcache"

// modelCachePath is where a composed glb for url3D lives (built or not).
func (s *Server) modelCachePath(url3D string) string {
	sum := sha1.Sum([]byte(strings.ToLower(blue.CleanURL(url3D))))
	return filepath.Join(s.modelCacheDir, fmt.Sprintf("%s-v%d.glb", hex.EncodeToString(sum[:8]), blue.PipelineVersion))
}

// composedGLB returns the path of the composed glb for url3D, building it on
// first use. A build that fails is remembered until the next rescan so the
// editor does not retry it on every frame of a model picker.
func (s *Server) composedGLB(url3D string) (string, error) {
	if s.resFS == nil || s.modelCacheDir == "" {
		return "", fmt.Errorf("model pipeline not mounted")
	}
	url3D = blue.CleanURL(url3D)
	if url3D == "" {
		return "", fmt.Errorf("empty url3D")
	}
	path := s.modelCachePath(url3D)
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	s.modelFailMu.Lock()
	if msg, failed := s.modelFailures[url3D]; failed {
		s.modelFailMu.Unlock()
		return "", fmt.Errorf("%s", msg)
	}
	s.modelFailMu.Unlock()

	// One build per url3D at a time; later callers wait for the first.
	s.modelBuildMu.Lock()
	ch, running := s.modelBuilding[url3D]
	if !running {
		ch = make(chan struct{})
		s.modelBuilding[url3D] = ch
	}
	s.modelBuildMu.Unlock()
	if running {
		<-ch
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
		s.modelFailMu.Lock()
		msg := s.modelFailures[url3D]
		s.modelFailMu.Unlock()
		return "", fmt.Errorf("%s", msg)
	}
	defer func() {
		s.modelBuildMu.Lock()
		delete(s.modelBuilding, url3D)
		s.modelBuildMu.Unlock()
		close(ch)
	}()

	glb, _, err := blue.Build(s.resFS, url3D, blue.BuildOptions{})
	if err != nil {
		s.modelFailMu.Lock()
		s.modelFailures[url3D] = err.Error()
		s.modelFailMu.Unlock()
		return "", err
	}
	if err := os.MkdirAll(s.modelCacheDir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(s.modelCacheDir, "build-*.tmp")
	if err != nil {
		return "", err
	}
	if _, err := tmp.Write(glb); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		// A concurrent process may have won; a present file is still a hit.
		if _, statErr := os.Stat(path); statErr == nil {
			return path, nil
		}
		return "", err
	}
	return path, nil
}

// serveModel serves the composed glb for url3D, falling back to the
// converter's tree, then 404.
func (s *Server) serveModel(w http.ResponseWriter, r *http.Request, url3D string) {
	if p, err := s.composedGLB(url3D); err == nil {
		s.serveGLB(w, r, p)
		return
	} else if s.resFS != nil {
		s.logModelFallback(url3D, err)
	}
	if p := s.resolveGLB(url3D); p != "" {
		s.serveGLB(w, r, p)
		return
	}
	http.NotFound(w, r)
}

// logModelFallback logs one line per url3D, not one per request.
func (s *Server) logModelFallback(url3D string, err error) {
	s.modelFailMu.Lock()
	logged := s.modelFallbackLogged[url3D]
	s.modelFallbackLogged[url3D] = true
	s.modelFailMu.Unlock()
	if !logged {
		log.Printf("[model] %s: pipeline could not build it (%v); using converted glb if any", url3D, err)
	}
}

// hasModel reports whether some glb can be served for url3D.
func (s *Server) hasModel(url3D string) bool {
	if s.resFS != nil && s.resFS.Exists(blue.CleanURL(url3D)) {
		return true
	}
	return s.resolveGLB(url3D) != ""
}

// modelGeneration is the cache-busting number the editor appends to model
// URLs: the converter-tree generation and the pipeline version together, so
// either a rescan or a server upgrade refetches.
func (s *Server) modelGeneration() int64 {
	return s.modelSnapshot().gen*1000 + blue.PipelineVersion
}

// resetModelFailures forgets remembered build failures (a rescan).
func (s *Server) resetModelFailures() {
	s.modelFailMu.Lock()
	s.modelFailures = map[string]string{}
	s.modelFallbackLogged = map[string]bool{}
	s.modelFailMu.Unlock()
}

// purgeModelCache deletes every cached glb.
func (s *Server) purgeModelCache() int {
	if s.modelCacheDir == "" {
		return 0
	}
	entries, _ := os.ReadDir(s.modelCacheDir)
	n := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".glb") || strings.HasSuffix(e.Name(), ".tmp") {
			if os.Remove(filepath.Join(s.modelCacheDir, e.Name())) == nil {
				n++
			}
		}
	}
	return n
}

// modelPipelineStatus is the "blue" block of /api/models/status.
func (s *Server) modelPipelineStatus() map[string]any {
	st := map[string]any{"pipeline": blue.PipelineVersion, "mounted": s.resFS.Mounts(), "cacheDir": s.modelCacheDir}
	cached := 0
	if s.modelCacheDir != "" {
		entries, _ := os.ReadDir(s.modelCacheDir)
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".glb") {
				cached++
			}
		}
	}
	st["cached"] = cached
	s.modelFailMu.Lock()
	failures := make(map[string]string, len(s.modelFailures))
	for k, v := range s.modelFailures {
		failures[k] = v
	}
	s.modelFailMu.Unlock()
	st["failures"] = failures
	return st
}

// handleModelDebug prints what the pipeline composes for a url3D.
//
// GET /api/model/debug?url3D=res:/Model/...
func (s *Server) handleModelDebug(w http.ResponseWriter, r *http.Request) {
	url3D := blue.CleanURL(r.URL.Query().Get("url3D"))
	if url3D == "" {
		jsonError(w, "url3D is required", http.StatusBadRequest)
		return
	}
	if s.resFS == nil {
		jsonError(w, "model pipeline not mounted (esp.clientPath)", http.StatusNotFound)
		return
	}
	data, err := s.resFS.Read(url3D)
	if err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}
	f, err := blue.Parse(data)
	if err != nil {
		jsonError(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	c := blue.Compose(f)
	c.URL = url3D
	classes := make([]string, 0, len(f.Classes))
	for _, cl := range f.Classes {
		classes = append(classes, cl.Name)
	}
	sort.Strings(classes)
	out := map[string]any{
		"composed":  c,
		"classes":   classes,
		"objects":   len(f.Sizes),
		"cachePath": s.modelCachePath(url3D),
	}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", " ")
	_ = enc.Encode(out)
}

// modelPipelineState is the per-Server state the pipeline keeps.
type modelPipelineState struct {
	resFS               *blue.ResFS
	modelCacheDir       string
	modelBuildMu        sync.Mutex
	modelBuilding       map[string]chan struct{}
	modelFailMu         sync.Mutex
	modelFailures       map[string]string
	modelFallbackLogged map[string]bool
}

// initModelPipeline mounts the client folder for the pipeline.
func (s *Server) initModelPipeline(clientPath string) {
	s.modelBuilding = map[string]chan struct{}{}
	s.modelFailures = map[string]string{}
	s.modelFallbackLogged = map[string]bool{}
	fs, err := blue.Open(clientPath)
	if err != nil {
		log.Printf("ESP model pipeline: %v (serving converted glbs only)", err)
		return
	}
	if fs == nil {
		return
	}
	s.resFS = fs
	s.modelCacheDir = filepath.Join("data", modelCacheDirName)
	log.Printf("ESP model pipeline v%d: reading client models from %s; cache %s", blue.PipelineVersion, strings.Join(fs.Mounts(), ", "), s.modelCacheDir)
}
