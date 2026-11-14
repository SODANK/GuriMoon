package esp

import (
	"context"
	"encoding/binary"
	"image"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
)

// handleTypeIcon serves /typeicon/{typeID}[.png][?size=N] (default size 32), resolved as the
// client's uix.GetIcon does: eveGraphics.icon names a pre-extracted PNG (iconsDir); otherwise
// (ships, entities, drones) the client's snapshot cache/Pictures/Gids/<graphicID>_<size>.dds is
// converted to PNG.
func (s *Server) handleTypeIcon(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	trimmed := strings.TrimPrefix(r.URL.Path, "/typeicon/")
	trimmed = strings.TrimSuffix(trimmed, ".png")
	typeID, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil || typeID <= 0 {
		http.NotFound(w, r)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	graphicID, icon, err := evedb.ESPGetTypeGraphic(ctx, s.db, typeID)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	// 1: static icon from the extracted icons directory.
	if icon != "" && s.iconsDir != "" {
		// Normalize "res:/ui/Texture/Icons/09_07" or "09_07" → basename.
		name := icon
		if i := strings.LastIndexAny(name, "/\\"); i >= 0 {
			name = name[i+1:]
		}
		path := filepath.Join(s.iconsDir, name+".png")
		if _, err := os.Stat(path); err == nil {
			w.Header().Set("Content-Type", "image/png")
			w.Header().Set("Cache-Control", "public, max-age=86400")
			http.ServeFile(w, r, path)
			return
		}
	}

	// 2: client-rendered snapshot from the Gids cache.
	if s.gidsDir == "" || graphicID <= 0 {
		http.NotFound(w, r)
		return
	}

	want := int64(32)
	if v, err := strconv.ParseInt(r.URL.Query().Get("size"), 10, 64); err == nil && v > 0 {
		want = v
	}

	s.serveGidSnapshot(w, r, graphicID, want)
}

// decodeUncompressedDDS decodes an uncompressed 32-bit BGRA DDS file (the
// format the client writes to its Gids/Types snapshot caches) into an image.
func decodeUncompressedDDS(path string) (image.Image, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) < 128 || string(data[:4]) != "DDS " {
		return nil, os.ErrInvalid
	}
	height := int(binary.LittleEndian.Uint32(data[12:]))
	width := int(binary.LittleEndian.Uint32(data[16:]))
	pfFlags := binary.LittleEndian.Uint32(data[80:])
	bitCount := binary.LittleEndian.Uint32(data[88:])

	const ddpfFourCC = 0x4
	if pfFlags&ddpfFourCC != 0 || bitCount != 32 {
		return nil, os.ErrInvalid // compressed or non-32bpp: not expected here
	}
	if width <= 0 || height <= 0 || len(data) < 128+width*height*4 {
		return nil, os.ErrInvalid
	}

	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	src := data[128:]
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			o := (y*width + x) * 4
			d := img.PixOffset(x, y)
			// DDS stores BGRA little-endian.
			img.Pix[d+0] = src[o+2]
			img.Pix[d+1] = src[o+1]
			img.Pix[d+2] = src[o+0]
			img.Pix[d+3] = src[o+3]
		}
	}
	return img, nil
}
