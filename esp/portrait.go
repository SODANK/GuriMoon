package esp

import (
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
)

// handlePortrait serves /portrait/{charID}[.png][?size=N] (default size 256) from two sources, in
// order: the server's own store (portraitStoreDir, portraitstore.go, filled by /portrait/ingest
// or /portrait/sync), then the client portrait cache under esp.clientPath. The store wins so a
// hand-over from a GM's client shows even where a stale local cache exists. The closest size is
// served; the client renders 64/128/228/256/322/506/692.
func (s *Server) handlePortrait(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	trimmed := strings.TrimPrefix(r.URL.Path, "/portrait/")
	trimmed = strings.TrimSuffix(trimmed, ".png")
	charID, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil || charID <= 0 {
		http.NotFound(w, r)
		return
	}

	want := int64(defaultPortraitSize)
	if v, err := strconv.ParseInt(r.URL.Query().Get("size"), 10, 64); err == nil && v > 0 {
		want = v
	}

	for _, dir := range []string{portraitStoreDir, s.portraitsDir} {
		if dir == "" {
			continue
		}
		name := pickPortrait(dir, charID, want)
		if name == "" {
			continue
		}
		if strings.HasSuffix(strings.ToLower(name), ".png") {
			w.Header().Set("Content-Type", "image/png")
		} else {
			w.Header().Set("Content-Type", "image/jpeg")
		}
		// Short cache: a re-ingest after an appearance edit shows without a hard refresh.
		w.Header().Set("Cache-Control", "private, max-age=60")
		http.ServeFile(w, r, filepath.Join(dir, name))
		return
	}
	http.NotFound(w, r)
}
