package esp

// The server's own alliance logo store.
//
// The client draws an alliance logo only from a fixed dict of 70 allianceIDs
// (ui/util/uix.py GetAllianceLogo); other IDs draw a fallback tile. This store is the other half:
// a real image per alliance, uploaded by a GM and shown wherever the server owns the pixels (ESP
// panel, in-game browser pages, killmail HTML). It follows the portrait store's shape:
// data/alliance_logos, the filename is the only metadata, magic bytes decide the format.
//
//	curl -u GMNAME:PASS -F "files=@1000123.png" http://host:26080/alliancelogo/ingest
import (
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// allianceLogoDir is the server-owned alliance logo directory. A deployment can relocate it
// before esp.New, like portraitStoreDir.
var allianceLogoDir = filepath.Join("data", "alliance_logos")

// maxAllianceLogoBytes caps one upload.
const maxAllianceLogoBytes = 4 << 20

// allianceLogoPath returns the stored logo for an alliance, or "" if none.
func allianceLogoPath(allianceID int64) string {
	for _, ext := range []string{"png", "jpg"} {
		p := filepath.Join(allianceLogoDir, strconv.FormatInt(allianceID, 10)+"."+ext)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// hasAllianceLogo reports whether a stored logo exists.  Templates use it.
func hasAllianceLogo(allianceID int64) bool { return allianceLogoPath(allianceID) != "" }

// saveAllianceLogo writes one logo, replacing any earlier copy in either format, so a stale
// .jpg never sits beside a new .png.
func saveAllianceLogo(allianceID int64, ext string, data []byte) error {
	if err := os.MkdirAll(allianceLogoDir, 0o755); err != nil {
		return err
	}
	for _, old := range []string{"png", "jpg"} {
		_ = os.Remove(filepath.Join(allianceLogoDir, strconv.FormatInt(allianceID, 10)+"."+old))
	}
	return os.WriteFile(filepath.Join(allianceLogoDir,
		strconv.FormatInt(allianceID, 10)+"."+ext), data, 0o644)
}

// dropAllianceLogo removes a stored logo.
func dropAllianceLogo(allianceID int64) (bool, error) {
	p := allianceLogoPath(allianceID)
	if p == "" {
		return false, nil
	}
	if err := os.Remove(p); err != nil {
		return false, err
	}
	return true, nil
}

// handleAllianceLogo serves a stored alliance logo.
// URL: /alliancelogo/{allianceID}[.png]
func (s *Server) handleAllianceLogo(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		s.handleAllianceLogoIngest(w, r)
		return
	}
	trimmed := strings.TrimPrefix(r.URL.Path, "/alliancelogo/")
	trimmed = strings.TrimSuffix(strings.TrimSuffix(trimmed, ".png"), ".jpg")
	allianceID, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil || allianceID <= 0 {
		http.NotFound(w, r)
		return
	}
	path := allianceLogoPath(allianceID)
	if path == "" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFile(w, r, path)
}

// handleAllianceLogoIngest receives uploads.  A form field "allianceID" names
// the target when one file is posted from an alliance page; otherwise the file
// name carries it, so a folder of <allianceID>.png can be handed over at once.
func (s *Server) handleAllianceLogoIngest(w http.ResponseWriter, r *http.Request) {
	mr, err := r.MultipartReader()
	if err != nil {
		http.Error(w, "expected a multipart upload: "+err.Error(), http.StatusBadRequest)
		return
	}
	res := ingestResult{
		StoreDir: allianceLogoDir,
		Item:     "logo",
		Owner:    "alliance",
		SkipHint: "not a PNG/JPEG, or no alliance ID in the file name",
	}
	var redirect string
	var target int64
	seen := make(map[int64]bool)

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
			v, _ := io.ReadAll(io.LimitReader(part, 512))
			switch part.FormName() {
			case "redirect":
				redirect = string(v)
			case "allianceID":
				target, _ = strconv.ParseInt(strings.TrimSpace(string(v)), 10, 64)
			}
			part.Close()
			continue
		}
		name := filepath.Base(strings.ReplaceAll(part.FileName(), `\`, "/"))
		allianceID := target
		if allianceID <= 0 {
			stem := strings.TrimSuffix(name, filepath.Ext(name))
			allianceID, _ = strconv.ParseInt(stem, 10, 64)
		}
		data, err := io.ReadAll(io.LimitReader(part, maxAllianceLogoBytes+1))
		part.Close()
		if err != nil {
			res.Errors = append(res.Errors, name+": "+err.Error())
			continue
		}
		if allianceID <= 0 || len(data) > maxAllianceLogoBytes {
			res.Skipped++
			continue
		}
		// The extension is a claim; the magic bytes are the fact.
		ext := sniffPortrait(data)
		if ext == "" {
			res.Skipped++
			continue
		}
		if err := saveAllianceLogo(allianceID, ext, data); err != nil {
			res.Errors = append(res.Errors, name+": "+err.Error())
			continue
		}
		res.Stored++
		seen[allianceID] = true
	}
	res.Chars = len(seen)
	log.Printf("ESP alliance logo ingest from %s: %d stored, %d skipped, %d alliance(s)",
		r.RemoteAddr, res.Stored, res.Skipped, res.Chars)
	finishIngest(w, r, res, redirect)
}

// moveAllianceLogo follows a stored logo onto a new allianceID when a renumber moves an alliance
// to a client logo slot. The store is keyed on the ID alone, so a logo left behind belongs to
// nobody.
func moveAllianceLogo(oldID, newID int64) error {
	src := allianceLogoPath(oldID)
	if src == "" {
		return nil
	}
	ext := strings.TrimPrefix(filepath.Ext(src), ".")
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := saveAllianceLogo(newID, ext, data); err != nil {
		return err
	}
	return os.Remove(src)
}
