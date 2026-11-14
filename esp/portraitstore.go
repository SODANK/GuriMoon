package esp

// The server's own store of rendered portraits (portraitStoreDir, default data/portraits, which
// the news server's billboard also reads). The server has no face renderer: the 3913 client
// draws a face from the appearance values in character_ and caches <charID>_<size>.png, and a
// picture reaches the store only when a machine that has one hands the file over.
//
// /portrait/<id> serves the store first and the client cache under esp.clientPath second; the
// client cache is read-only here. Ingest is POST /portrait/ingest (multipart) or POST
// /portrait/sync (copy from a co-located client cache), both behind authMiddleware. The file name
// is the only metadata. A file must parse as <charID>[_<size>].png|jpg, start with PNG or JPEG
// magic bytes (the extension is not trusted) and fit maxPortraitBytes, or it is counted and skipped.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// portraitStoreDir is the portrait directory. A deployment may relocate it before esp.New.
var portraitStoreDir = filepath.Join("data", "portraits")

const (
	// maxPortraitBytes caps one file.
	maxPortraitBytes = 8 << 20
	// maxIngestFiles and maxIngestBytes bound a whole directory upload.
	maxIngestFiles = 8192
	maxIngestBytes = 512 << 20
	// defaultPortraitSize is served when the caller names no size.
	defaultPortraitSize = 256
)

// portraitFile is one stored render.
type portraitFile struct {
	Name string // file name within its directory
	Size int64  // pixel size parsed from the name (0 = unsized)
}

// parsePortraitName splits "<charID>[_<size>].png" into its parts. Any other name is rejected.
func parsePortraitName(name string) (charID, size int64, ext string, ok bool) {
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, ".png"):
		ext = "png"
	case strings.HasSuffix(lower, ".jpg"), strings.HasSuffix(lower, ".jpeg"):
		ext = "jpg"
	default:
		return 0, 0, "", false
	}
	stem := lower[:strings.LastIndex(lower, ".")]
	idPart, sizePart := stem, ""
	if i := strings.IndexByte(stem, '_'); i >= 0 {
		idPart, sizePart = stem[:i], stem[i+1:]
	}
	charID, err := strconv.ParseInt(idPart, 10, 64)
	if err != nil || charID <= 0 {
		return 0, 0, "", false
	}
	if sizePart != "" {
		size, err = strconv.ParseInt(sizePart, 10, 64)
		if err != nil || size <= 0 || size > 4096 {
			return 0, 0, "", false
		}
	}
	return charID, size, ext, true
}

// sniffPortrait reports the real image type of a payload, ignoring the extension. Only PNG and
// JPEG are accepted.
func sniffPortrait(b []byte) string {
	switch {
	case len(b) >= 8 && string(b[:8]) == "\x89PNG\r\n\x1a\n":
		return "png"
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return "jpg"
	}
	return ""
}

// listPortraits returns every stored render for charID in a directory, smallest first. A
// missing directory has none.
func listPortraits(dir string, charID int64) []portraitFile {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []portraitFile
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		id, size, _, ok := parsePortraitName(e.Name())
		if !ok || id != charID {
			continue
		}
		out = append(out, portraitFile{Name: e.Name(), Size: size})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Size < out[j].Size })
	return out
}

// pickPortrait returns the stored render closest to want, preferring the larger one on a tie.
func pickPortrait(dir string, charID, want int64) string {
	files := listPortraits(dir, charID)
	if len(files) == 0 {
		return ""
	}
	best, bestDiff := "", int64(-1)
	for _, f := range files {
		diff := f.Size - want
		if diff < 0 {
			diff = -diff
		}
		if bestDiff < 0 || diff < bestDiff {
			best, bestDiff = f.Name, diff
		}
	}
	return best
}

// savePortrait writes one render into the store, replacing any earlier copy at
// the same character and size.
func savePortrait(charID, size int64, ext string, data []byte) error {
	if err := os.MkdirAll(portraitStoreDir, 0o755); err != nil {
		return err
	}
	name := strconv.FormatInt(charID, 10)
	if size > 0 {
		name += "_" + strconv.FormatInt(size, 10)
	}
	return os.WriteFile(filepath.Join(portraitStoreDir, name+"."+ext), data, 0o644)
}

// hasPortraitSource reports whether any portrait can be served: a non-empty store or a client
// cache. Templates use it to decide whether to emit <img> tags.
func (s *Server) hasPortraitSource() bool {
	if s.portraitsDir != "" {
		return true
	}
	entries, err := os.ReadDir(portraitStoreDir)
	return err == nil && len(entries) > 0
}

// ingestResult is the tally a hand-over reports. Item and Owner name what was handed over, since
// the alliance logo store (esp/alliancelogo.go) shares the endpoint shape.
type ingestResult struct {
	Stored   int      `json:"stored"`
	Skipped  int      `json:"skipped"`
	Chars    int      `json:"characters"`
	Errors   []string `json:"errors,omitempty"`
	StoreDir string   `json:"storeDir"`
	Item     string   `json:"-"` // default "portrait"
	Owner    string   `json:"-"` // default "character"
	SkipHint string   `json:"-"` // default "not a <charID>_<size> PNG/JPEG"
}

// summary renders the tally for a redirect message.
func (res ingestResult) summary() string {
	item, owner := res.Item, res.Owner
	if item == "" {
		item = "portrait"
	}
	if owner == "" {
		owner = "character"
	}
	hint := res.SkipHint
	if hint == "" {
		hint = "not a <charID>_<size> PNG/JPEG"
	}
	msg := fmt.Sprintf("%d %s(s) stored for %d %s(s) in %s",
		res.Stored, item, res.Chars, owner, res.StoreDir)
	if res.Skipped > 0 {
		msg += fmt.Sprintf("; %d file(s) skipped (%s)", res.Skipped, hint)
	}
	if len(res.Errors) > 0 {
		msg += " — " + strings.Join(res.Errors, "; ")
	}
	return msg
}

// finishIngest answers a hand-over: a redirect to the posting page when one was named, JSON
// otherwise. The destination must be a local path.
func finishIngest(w http.ResponseWriter, r *http.Request, res ingestResult, dest string) {
	if strings.HasPrefix(dest, "/") && !strings.HasPrefix(dest, "//") {
		sep := "?"
		if strings.Contains(dest, "?") {
			sep = "&"
		}
		http.Redirect(w, r, dest+sep+"msg="+url.QueryEscape(res.summary()), http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

// handlePortraitIngest receives rendered portraits over HTTP. The multipart body is streamed,
// not buffered, so a whole-cache upload stays off the heap.
func (s *Server) handlePortraitIngest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	mr, err := r.MultipartReader()
	if err != nil {
		http.Error(w, "expected a multipart upload: "+err.Error(), http.StatusBadRequest)
		return
	}

	res := ingestResult{StoreDir: portraitStoreDir}
	seen := make(map[int64]bool)
	var total int64
	var redirect string
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			res.Errors = append(res.Errors, "read: "+err.Error())
			break
		}
		if part.FileName() == "" {
			// Ordinary form field: only "redirect" is read, since r.FormValue cannot be used on a hand-streamed body.
			if part.FormName() == "redirect" {
				v, _ := io.ReadAll(io.LimitReader(part, 512))
				redirect = string(v)
			}
			part.Close()
			continue
		}
		if res.Stored+res.Skipped >= maxIngestFiles || total >= maxIngestBytes {
			res.Errors = append(res.Errors, "upload limit reached, remaining files ignored")
			part.Close()
			break
		}
		name := filepath.Base(strings.ReplaceAll(part.FileName(), `\`, "/"))
		charID, size, _, ok := parsePortraitName(name)
		if !ok {
			res.Skipped++
			part.Close()
			continue
		}
		data, err := io.ReadAll(io.LimitReader(part, maxPortraitBytes+1))
		part.Close()
		if err != nil {
			res.Errors = append(res.Errors, name+": "+err.Error())
			continue
		}
		if len(data) > maxPortraitBytes {
			res.Skipped++
			continue
		}
		// The extension is a claim; the magic bytes decide.
		ext := sniffPortrait(data)
		if ext == "" {
			res.Skipped++
			continue
		}
		if err := savePortrait(charID, size, ext, data); err != nil {
			res.Errors = append(res.Errors, name+": "+err.Error())
			continue
		}
		total += int64(len(data))
		res.Stored++
		seen[charID] = true
	}
	res.Chars = len(seen)
	log.Printf("ESP portrait ingest from %s: %d stored, %d skipped, %d characters",
		r.RemoteAddr, res.Stored, res.Skipped, res.Chars)
	finishIngest(w, r, res, redirect)
}

// handlePortraitSync copies renders out of the local client cache into the store. ?char=<id>
// limits it to one pilot.
func (s *Server) handlePortraitSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	res := ingestResult{StoreDir: portraitStoreDir}
	if s.portraitsDir == "" {
		res.Errors = append(res.Errors,
			"no client portrait cache on this host — set esp.clientPath, or upload the files instead")
		finishIngest(w, r, res, r.FormValue("redirect"))
		return
	}

	only, _ := strconv.ParseInt(r.FormValue("char"), 10, 64)
	entries, err := os.ReadDir(s.portraitsDir)
	if err != nil {
		res.Errors = append(res.Errors, err.Error())
		finishIngest(w, r, res, r.FormValue("redirect"))
		return
	}
	seen := make(map[int64]bool)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		charID, size, _, ok := parsePortraitName(e.Name())
		if !ok || (only > 0 && charID != only) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.portraitsDir, e.Name()))
		if err != nil || len(data) > maxPortraitBytes {
			res.Skipped++
			continue
		}
		ext := sniffPortrait(data)
		if ext == "" {
			res.Skipped++
			continue
		}
		if err := savePortrait(charID, size, ext, data); err != nil {
			res.Errors = append(res.Errors, e.Name()+": "+err.Error())
			continue
		}
		res.Stored++
		seen[charID] = true
	}
	res.Chars = len(seen)
	log.Printf("ESP portrait import from %s: %d stored, %d skipped, %d characters",
		s.portraitsDir, res.Stored, res.Skipped, res.Chars)
	finishIngest(w, r, res, r.FormValue("redirect"))
}

// HasStoredPortrait reports whether the store holds a render of charID at exactly size pixels.
// The bounty-portrait job queue (server/charmgr) uses it to pick pilots that need a render.
func HasStoredPortrait(charID, size int64) bool {
	for _, f := range listPortraits(portraitStoreDir, charID) {
		if f.Size == size {
			return true
		}
	}
	return false
}

// StoreClientPortrait files a render a game client drew on request (server/charmgr/
// portraitjobs.go). Only the lease holder may submit (checked by the job queue). The bytes must
// be a PNG within maxPortraitBytes that decodes to exactly the requested square size.
// See docs/esp-2-notes.md#portrait-store.
func StoreClientPortrait(charID, size int64, data []byte) error {
	if charID <= 0 || size <= 0 || size > 4096 {
		return fmt.Errorf("bad portrait key %d/%d", charID, size)
	}
	if len(data) > maxPortraitBytes {
		return fmt.Errorf("portrait is %d bytes, over the %d cap", len(data), maxPortraitBytes)
	}
	if sniffPortrait(data) != "png" {
		return fmt.Errorf("portrait is not a PNG")
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("portrait does not decode: %w", err)
	}
	if int64(cfg.Width) != size || int64(cfg.Height) != size {
		return fmt.Errorf("portrait is %dx%d, want %dx%d", cfg.Width, cfg.Height, size, size)
	}
	return savePortrait(charID, size, "png", data)
}

// dropStoredPortraits deletes the store's copies for one character, leaving the client cache alone.
func dropStoredPortraits(charID int64) (int, error) {
	files := listPortraits(portraitStoreDir, charID)
	dropped := 0
	for _, f := range files {
		if err := os.Remove(filepath.Join(portraitStoreDir, f.Name)); err != nil {
			return dropped, err
		}
		dropped++
	}
	return dropped, nil
}
