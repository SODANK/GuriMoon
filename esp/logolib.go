package esp

// Corporation logo layer art. A logo is three shape IDs and three colour IDs in `corporation`
// (eveGraphics rows into the paintshop library) that the client composites itself
// (uix.GetCorpLogo). A shape .blue is a 64x64 cell of a 256x256 res:/UI/Texture/LogoLibNN.dds
// atlas, which handleLogoShape serves so the picker shows the real art. The atlases are 8-bit
// palettised DDS, decoded by decodePalettedDDS. See docs/esp-2-notes.md#corporation-logo-art.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	evedb "gurimoon/db"
)

// logoColour is one paintshop layer colour.
type logoColour struct {
	GraphicID int64
	Name      string
	Hex       string // client diffuse, "" when the asset carries none
}

// logoColours are the 15 colour graphics the client ships, with the diffuse each paints a layer
// with (frozen from its LayerColor .blue files). "Thin Black" has no diffuse and is listed
// unusable, since a corporation may already store it.
var logoColours = []logoColour{
	{671, "Black Plain", "#202020"},
	{672, "Gold Plating", "#978059"},
	{673, "Gray Blue Shade", "#a9d1ff"},
	{674, "Grey Paint", "#ffffff"},
	{675, "Lead", "#494949"},
	{676, "Luminant Blue", "#5686ff"},
	{677, "Luminant Yellow", "#ffb33c"},
	{678, "Military Grey", "#919a99"},
	{679, "Orange Dangercolor", "#ff7800"},
	{680, "Red Shade Paint", "#970000"},
	{681, "Silver Plating", "#7e7f7e"},
	{682, "Thin Black", ""},
	{683, "Thin White", "#7e7f7e"},
	{684, "White Plain", "#e7e7e7"},
	{685, "Yellow Plain", "#ffb33c"},
}

// The shape ID range is db's, so the picker and the art server agree. logoShapeMin is shape 0,
// the empty plate.
const (
	logoShapeMin = evedb.LogoShapeMinID
	logoShapeMax = evedb.LogoShapeMaxID
	logoCellSize = 64
)

// logoColourHex returns the CSS colour a layer colour graphic paints with.
func logoColourHex(graphicID int64) string {
	for _, c := range logoColours {
		if c.GraphicID == graphicID {
			return c.Hex
		}
	}
	return ""
}

// logoColourName returns the client's name for a layer colour graphic.
func logoColourName(graphicID int64) string {
	for _, c := range logoColours {
		if c.GraphicID == graphicID {
			return c.Name
		}
	}
	return ""
}

// discoverLogoLibDDS finds the shape atlases in the client folder, returning atlas number to
// path. Loose res/ wins over the extracted archives, as in the client.
func discoverLogoLibDDS(clientPath string) map[int]string {
	if clientPath == "" {
		return nil
	}
	var dirs []string
	dirs = append(dirs,
		filepath.Join(clientPath, "res", "UI", "Texture"),
		filepath.Join(clientPath, "res", "Texture"))
	if matches, err := filepath.Glob(filepath.Join(clientPath, "extracted_res*")); err == nil {
		sort.Strings(matches)
		for _, m := range matches {
			dirs = append(dirs,
				filepath.Join(m, "res", "ui", "Texture"),
				filepath.Join(m, "res", "UI", "Texture"),
				filepath.Join(m, "res", "Texture"))
		}
	}

	found := make(map[int]string)
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			lower := strings.ToLower(name)
			if e.IsDir() || !strings.HasPrefix(lower, "logolib") || !strings.HasSuffix(lower, ".dds") {
				continue
			}
			n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(lower, "logolib"), ".dds"))
			if err != nil || n <= 0 {
				continue
			}
			if _, exists := found[n]; !exists {
				found[n] = filepath.Join(dir, name)
			}
		}
	}
	if len(found) == 0 {
		return nil
	}
	return found
}

// logoShapePlaceholders are shape files with no logo art (they point at an icon sheet). The
// atlas cells run contiguously past them, so every shape above sits three cells earlier than its
// number suggests.
var logoShapePlaceholders = map[int]bool{97: true, 98: true, 99: true}

// logoShapeCell maps a shape graphicID to its atlas number and cell rectangle.
// ok is false for the empty plate, for the placeholders, and for IDs whose file
// the client never shipped.
func logoShapeCell(graphicID int64) (atlas int, rect image.Rectangle, ok bool) {
	if graphicID <= logoShapeMin || graphicID > logoShapeMax {
		return 0, image.Rectangle{}, false
	}
	shape := int(graphicID - logoShapeMin) // 1..163
	if logoShapePlaceholders[shape] {
		return 0, image.Rectangle{}, false
	}
	ordinal := shape - 1
	if shape > 99 {
		ordinal = shape - 4
	}
	atlas = ordinal/16 + 1
	cell := ordinal % 16
	col, row := cell%4, cell/4
	rect = image.Rect(col*logoCellSize, row*logoCellSize,
		(col+1)*logoCellSize, (row+1)*logoCellSize)
	return atlas, rect, true
}

// handleLogoShape serves one corporation logo shape as a PNG.
// URL: /logoshape/{graphicID}[.png]
func (s *Server) handleLogoShape(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.TrimPrefix(r.URL.Path, "/logoshape/")
	trimmed = strings.TrimSuffix(trimmed, ".png")
	graphicID, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	atlas, rect, ok := logoShapeCell(graphicID)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if cached, hit := s.logoCache.Load(graphicID); hit {
		writePNG(w, cached)
		return
	}
	path, hasAtlas := s.logoLibDDS[atlas]
	if !hasAtlas {
		http.NotFound(w, r)
		return
	}
	img, err := decodePalettedDDS(path)
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
	s.logoCache.Store(graphicID, buf.Bytes())
	writePNG(w, buf.Bytes())
}

// writePNG sends a cached PNG body.
func writePNG(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(body)
}

// decodePalettedDDS decodes an 8-bit palettised DDS (the LogoLib format) into an NRGBA image.
// Other formats go to the shared DXT/masked-RGB decoders.
func decodePalettedDDS(path string) (image.Image, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	info, err := parseDDS(data)
	if err != nil {
		return nil, err
	}
	const ddpfPaletteIndexed8 = 0x20
	pfFlags := binary.LittleEndian.Uint32(data[80:])
	if pfFlags&ddpfPaletteIndexed8 == 0 || info.bitCount != 8 {
		switch info.format {
		case "DXT1", "DXT3", "DXT5":
			return decodeDXTImage(data[128:], info.width, info.height, info.format), nil
		default:
			return decodeMaskedRGBImage(data[128:], info.width, info.height, info), nil
		}
	}

	const paletteBytes = 256 * 4
	body := data[128:]
	if len(body) < paletteBytes+info.width*info.height {
		return nil, fmt.Errorf("%s: truncated palettised DDS", filepath.Base(path))
	}
	palette := body[:paletteBytes]
	indices := body[paletteBytes:]

	img := image.NewNRGBA(image.Rect(0, 0, info.width, info.height))
	for y := 0; y < info.height; y++ {
		for x := 0; x < info.width; x++ {
			e := int(indices[y*info.width+x]) * 4
			o := img.PixOffset(x, y)
			// Palette entries are stored R,G,B,A; the shapes are greyscale glyphs, so alpha is the mask.
			img.Pix[o+0] = palette[e+0]
			img.Pix[o+1] = palette[e+1]
			img.Pix[o+2] = palette[e+2]
			img.Pix[o+3] = palette[e+3]
		}
	}
	return img, nil
}

// logoLibAvailable reports whether shape art can be served; templates fall back to ID pickers
// without it.
func (s *Server) logoLibAvailable() bool { return len(s.logoLibDDS) > 0 }
