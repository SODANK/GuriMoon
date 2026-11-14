package esp

// Alliance logo art, cropped out of the client's own atlases.
//
// uix.GetAllianceLogo maps an allianceID to a tile name "NN_T" and hands it to uix.MapLogo with
// root res:/UI/Texture/Alliance/alliance, which sets rectWidth/rectHeight 128, rectLeft 128 for
// tiles 2 and 4 and rectTop 128 for tiles 3 and 4. Each allianceNN.dds is therefore a 256x256
// sheet of four 128px logos: 1=TL 2=TR 3=BL 4=BR. handleAllianceTile serves one tile.
//
// The atlases are DXT3 (the palettised LogoLib sheets are not), so the decoder in nebula.go
// covers them and this file only crops.

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// allianceTileSize is the edge of one logo in the 256x256 atlas (uix.MapLogo's
// rectWidth/rectHeight).
const allianceTileSize = 128

// discoverAllianceDDS finds the alliance atlases in the client folder,
// returning atlas number → path.  Loose res/ wins over the extracted archives,
// matching how the client itself resolves res: paths.
func discoverAllianceDDS(clientPath string) map[int]string {
	if clientPath == "" {
		return nil
	}
	var dirs []string
	dirs = append(dirs,
		filepath.Join(clientPath, "res", "UI", "Texture", "Alliance"),
		filepath.Join(clientPath, "res", "Texture", "Alliance"))
	if matches, err := filepath.Glob(filepath.Join(clientPath, "extracted_res*")); err == nil {
		sort.Strings(matches)
		for _, m := range matches {
			dirs = append(dirs,
				filepath.Join(m, "res", "ui", "Texture", "Alliance"),
				filepath.Join(m, "res", "UI", "Texture", "Alliance"),
				filepath.Join(m, "res", "Texture", "Alliance"))
		}
	}

	found := make(map[int]string)
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			lower := strings.ToLower(e.Name())
			if e.IsDir() || !strings.HasPrefix(lower, "alliance") || !strings.HasSuffix(lower, ".dds") {
				continue
			}
			n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(lower, "alliance"), ".dds"))
			if err != nil || n <= 0 {
				continue
			}
			if _, exists := found[n]; !exists {
				found[n] = filepath.Join(dir, e.Name())
			}
		}
	}
	if len(found) == 0 {
		return nil
	}
	return found
}

// parseAllianceTile splits a "NN_T" tile name into its atlas number and the
// rectangle uix.MapLogo would crop.
func parseAllianceTile(tile string) (atlas int, rect image.Rectangle, ok bool) {
	parts := strings.Split(tile, "_")
	if len(parts) != 2 {
		return 0, image.Rectangle{}, false
	}
	atlas, err := strconv.Atoi(parts[0])
	if err != nil || atlas <= 0 {
		return 0, image.Rectangle{}, false
	}
	num, err := strconv.Atoi(parts[1])
	if err != nil || num < 1 || num > 4 {
		return 0, image.Rectangle{}, false
	}
	left, top := 0, 0
	if num == 2 || num == 4 {
		left = allianceTileSize
	}
	if num > 2 {
		top = allianceTileSize
	}
	return atlas, image.Rect(left, top, left+allianceTileSize, top+allianceTileSize), true
}

// decodeFlatDDS decodes the top mip of a plain (non-cubemap) DDS.
func decodeFlatDDS(path string) (image.Image, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	info, err := parseDDS(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	need := faceByteSize(info.width, info.height, info)
	if 128+need > len(data) {
		return nil, fmt.Errorf("%s: truncated (need %d bytes, have %d)",
			filepath.Base(path), need, len(data)-128)
	}
	body := data[128 : 128+need]
	switch info.format {
	case "DXT1", "DXT3", "DXT5":
		return decodeDXTImage(body, info.width, info.height, info.format), nil
	default:
		return decodeMaskedRGBImage(body, info.width, info.height, info), nil
	}
}

// handleAllianceTile serves one alliance logo tile as a PNG.
// URL: /alliancetile/{NN_T}[.png]
func (s *Server) handleAllianceTile(w http.ResponseWriter, r *http.Request) {
	tile := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/alliancetile/"), ".png")
	atlas, rect, ok := parseAllianceTile(tile)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if cached, hit := s.allianceTileCache.Load(tile); hit {
		writePNG(w, cached)
		return
	}
	path, hasAtlas := s.allianceDDS[atlas]
	if !hasAtlas {
		http.NotFound(w, r)
		return
	}
	img, err := decodeFlatDDS(path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sub, ok := img.(interface {
		SubImage(image.Rectangle) image.Image
	})
	if !ok {
		http.Error(w, "atlas is not croppable", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, sub.SubImage(rect)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.allianceTileCache.Store(tile, buf.Bytes())
	writePNG(w, buf.Bytes())
}

// allianceTilesAvailable reports whether the client atlases were found.
func (s *Server) allianceTilesAvailable() bool { return len(s.allianceDDS) > 0 }
