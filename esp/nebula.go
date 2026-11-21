package esp

// Nebula skybox serving straight from client data.
//
// The client ships nebula backgrounds as DDS cubemaps at res:/Texture/Nebula/<name>_cube[2].dds
// (DXT1/3/5 or uncompressed). This file finds them in the client folder (loose res/ overlay or any
// extracted_res* dump), decodes the requested face on demand and serves it as a PNG at
// /nebulas/{name}/{px|nx|py|ny|pz|nz}.png, the layout the editor's CubeTextureLoader expects.
// Decoded faces are cached in memory. A pre-converted PNG directory (nebulasDir) takes precedence.

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
	"strings"
)

// nebulaFaceNames is the DDS face storage order (+X -X +Y -Y +Z -Z), matching
// the px/nx/py/ny/pz/nz PNG naming used by the editor.
var nebulaFaceNames = []string{"px", "nx", "py", "ny", "pz", "nz"}

// discoverNebulaDDS scans the client folder for nebula DDS cubemaps and
// returns displayName → absolute DDS path. Sources checked, in order:
// loose res/Texture/Nebula, then every extracted_res*/res/Texture/Nebula.
// First occurrence of a name wins.
func discoverNebulaDDS(clientPath string) map[string]string {
	if clientPath == "" {
		return nil
	}

	var dirs []string
	dirs = append(dirs, filepath.Join(clientPath, "res", "Texture", "Nebula"))
	if matches, err := filepath.Glob(filepath.Join(clientPath, "extracted_res*")); err == nil {
		sort.Strings(matches)
		for _, m := range matches {
			dirs = append(dirs, filepath.Join(m, "res", "Texture", "Nebula"))
		}
	}

	found := make(map[string]string)
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(strings.ToLower(name), ".dds") {
				continue
			}
			display := nebulaDisplayName(name)
			if _, exists := found[display]; !exists {
				found[display] = filepath.Join(dir, name)
			}
		}
	}
	if len(found) == 0 {
		return nil
	}
	return found
}

// nebulaDisplayName derives a stable display name from a DDS filename:
// "electric_cube.dds" → "electric", "electric_cube2.dds" → "electric2"
// (the trailing digit is kept so variants don't collide).
func nebulaDisplayName(filename string) string {
	stem := strings.TrimSuffix(filename, filepath.Ext(filename))
	if i := strings.LastIndex(stem, "_cube"); i >= 0 {
		return stem[:i] + stem[i+len("_cube"):]
	}
	return stem
}

// handleNebulaFile serves /nebulas/{name}/{face}.png. Pre-converted PNGs in
// nebulasDir win; otherwise the face is decoded from the client DDS cubemap.
func (s *Server) handleNebulaFile(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimPrefix(r.URL.Path, "/nebulas/")
	if strings.Contains(rel, "..") || strings.Contains(rel, "\\") {
		http.NotFound(w, r)
		return
	}

	// Static pre-converted directory takes precedence.
	if s.nebulasDir != "" {
		p := filepath.Join(s.nebulasDir, filepath.FromSlash(rel))
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			http.ServeFile(w, r, p)
			return
		}
	}

	// Client DDS fallback: name/face.png
	parts := strings.Split(rel, "/")
	if len(parts) != 2 || !strings.HasSuffix(parts[1], ".png") {
		http.NotFound(w, r)
		return
	}
	name := parts[0]
	face := strings.TrimSuffix(parts[1], ".png")
	faceIdx := -1
	for i, f := range nebulaFaceNames {
		if f == face {
			faceIdx = i
			break
		}
	}
	ddsPath, ok := s.nebulaDDS[name]
	if faceIdx < 0 || !ok {
		http.NotFound(w, r)
		return
	}

	cacheKey := name + "/" + face
	if cached, ok := s.nebulaCache.Load(cacheKey); ok {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "max-age=86400")
		_, _ = w.Write(cached) // a failed write is the client hanging up
		return
	}

	pngBytes, err, _ := s.nebulaGen.Do(cacheKey, func() ([]byte, error) {
		b, err := decodeCubemapFacePNG(ddsPath, faceIdx)
		if err != nil {
			return nil, err
		}
		// Trim the encoder buffer's spare capacity: the cache keeps this forever.
		b = bytes.Clone(b)
		s.nebulaCache.Store(cacheKey, b)
		return b, nil
	})
	if err != nil {
		http.Error(w, fmt.Sprintf("nebula decode failed: %v", err), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "max-age=86400")
	w.Write(pngBytes)
}

// ── DDS cubemap decoding ────────────────────────────────────────────────────

type ddsInfo struct {
	width, height              int
	mipCount                   int
	format                     string // a DXT format or RGB
	cubemap                    bool
	faceCount                  int
	bitCount                   int
	rMask, gMask, bMask, aMask uint32
}

const (
	ddpfAlphaPixels = 0x1
	ddpfFourCC      = 0x4
	ddpfRGB         = 0x40

	ddsCaps2Cubemap = 0x200
)

func parseDDS(data []byte) (ddsInfo, error) {
	var info ddsInfo
	if len(data) < 128 || string(data[:4]) != "DDS " {
		return info, fmt.Errorf("not a DDS file")
	}
	le := binary.LittleEndian
	info.height = int(le.Uint32(data[12:]))
	info.width = int(le.Uint32(data[16:]))
	info.mipCount = int(le.Uint32(data[28:]))
	if info.mipCount < 1 {
		info.mipCount = 1
	}

	pfFlags := le.Uint32(data[80:])
	fourCC := string(data[84:88])
	info.bitCount = int(le.Uint32(data[88:]))
	info.rMask = le.Uint32(data[92:])
	info.gMask = le.Uint32(data[96:])
	info.bMask = le.Uint32(data[100:])
	info.aMask = le.Uint32(data[104:])

	caps2 := le.Uint32(data[112:])
	info.cubemap = caps2&ddsCaps2Cubemap != 0
	for bit := uint32(0x400); bit <= 0x8000; bit <<= 1 {
		if caps2&bit != 0 {
			info.faceCount++
		}
	}

	switch {
	case pfFlags&ddpfFourCC != 0:
		switch fourCC {
		case "DXT1", "DXT3", "DXT5":
			info.format = fourCC
		default:
			return info, fmt.Errorf("unsupported fourCC %q", fourCC)
		}
	case pfFlags&ddpfRGB != 0:
		info.format = "RGB"
	case info.bitCount > 0:
		// Alpha-only (DDPF_ALPHA) or luminance formats: the channel masks drive the generic masked
		// decode, and missing channels read as 255.
		info.format = "RGB"
	default:
		return info, fmt.Errorf("unsupported pixel format flags 0x%x", pfFlags)
	}
	return info, nil
}

// faceByteSize returns the byte size of a single mip level.
func faceByteSize(w, h int, info ddsInfo) int {
	switch info.format {
	case "DXT1":
		return ((w + 3) / 4) * ((h + 3) / 4) * 8
	case "DXT3", "DXT5":
		return ((w + 3) / 4) * ((h + 3) / 4) * 16
	default:
		return w * h * (info.bitCount / 8)
	}
}

// mipChainSize returns the total byte size of one face including all mips.
func mipChainSize(info ddsInfo) int {
	total := 0
	w, h := info.width, info.height
	for i := 0; i < info.mipCount; i++ {
		total += faceByteSize(w, h, info)
		if w > 1 {
			w /= 2
		}
		if h > 1 {
			h /= 2
		}
	}
	return total
}

// decodeCubemapFacePNG reads a DDS cubemap and returns face faceIdx (DDS
// storage order +X -X +Y -Y +Z -Z) encoded as PNG.
func decodeCubemapFacePNG(path string, faceIdx int) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	info, err := parseDDS(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	if !info.cubemap || info.faceCount != 6 {
		return nil, fmt.Errorf("%s: not a 6-face cubemap", filepath.Base(path))
	}

	offset := 128 + faceIdx*mipChainSize(info)
	need := faceByteSize(info.width, info.height, info)
	if offset+need > len(data) {
		return nil, fmt.Errorf("%s: truncated (need %d bytes at %d, have %d)",
			filepath.Base(path), need, offset, len(data))
	}
	faceData := data[offset : offset+need]

	var img *image.NRGBA
	switch info.format {
	case "DXT1", "DXT3", "DXT5":
		img = decodeDXTImage(faceData, info.width, info.height, info.format)
	default:
		img = decodeMaskedRGBImage(faceData, info.width, info.height, info)
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func rgb565(c uint16) (r, g, b uint8) {
	r = uint8((uint32(c>>11&0x1F) * 255) / 31)
	g = uint8((uint32(c>>5&0x3F) * 255) / 63)
	b = uint8((uint32(c&0x1F) * 255) / 31)
	return
}

// decodeDXT1Colors fills colors[0..3] and pixel alpha for an 8-byte DXT1
// color block. dxt1Alpha selects the 1-bit-transparency mode (only valid for
// standalone DXT1; DXT3/5 color blocks always use the 4-color mode).
func decodeDXT1Block(block []byte, dxt1Alpha bool, out *[16][4]uint8) {
	le := binary.LittleEndian
	c0 := le.Uint16(block[0:])
	c1 := le.Uint16(block[2:])
	lookup := le.Uint32(block[4:])

	r0, g0, b0 := rgb565(c0)
	r1, g1, b1 := rgb565(c1)

	var colors [4][4]uint8
	colors[0] = [4]uint8{r0, g0, b0, 255}
	colors[1] = [4]uint8{r1, g1, b1, 255}
	if c0 > c1 || !dxt1Alpha {
		colors[2] = [4]uint8{
			uint8((2*uint32(r0) + uint32(r1)) / 3),
			uint8((2*uint32(g0) + uint32(g1)) / 3),
			uint8((2*uint32(b0) + uint32(b1)) / 3), 255}
		colors[3] = [4]uint8{
			uint8((uint32(r0) + 2*uint32(r1)) / 3),
			uint8((uint32(g0) + 2*uint32(g1)) / 3),
			uint8((uint32(b0) + 2*uint32(b1)) / 3), 255}
	} else {
		colors[2] = [4]uint8{
			uint8((uint32(r0) + uint32(r1)) / 2),
			uint8((uint32(g0) + uint32(g1)) / 2),
			uint8((uint32(b0) + uint32(b1)) / 2), 255}
		colors[3] = [4]uint8{0, 0, 0, 0}
	}

	for i := 0; i < 16; i++ {
		out[i] = colors[(lookup>>(2*uint(i)))&0x3]
	}
}

func decodeDXTImage(data []byte, width, height int, format string) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	blockW := (width + 3) / 4
	blockH := (height + 3) / 4
	blockSize := 16
	if format == "DXT1" {
		blockSize = 8
	}

	var pixels [16][4]uint8
	offset := 0
	for by := 0; by < blockH; by++ {
		for bx := 0; bx < blockW; bx++ {
			block := data[offset : offset+blockSize]
			offset += blockSize

			switch format {
			case "DXT1":
				decodeDXT1Block(block, true, &pixels)
			case "DXT3":
				decodeDXT1Block(block[8:16], false, &pixels)
				alphaBits := binary.LittleEndian.Uint64(block[0:8])
				for i := 0; i < 16; i++ {
					pixels[i][3] = uint8((alphaBits>>(4*uint(i)))&0xF) * 17
				}
			case "DXT5":
				decodeDXT1Block(block[8:16], false, &pixels)
				a0, a1 := block[0], block[1]
				var table [8]uint8
				table[0], table[1] = a0, a1
				if a0 > a1 {
					for i := 1; i < 7; i++ {
						table[i+1] = uint8((uint32(7-i)*uint32(a0) + uint32(i)*uint32(a1)) / 7)
					}
				} else {
					for i := 1; i < 5; i++ {
						table[i+1] = uint8((uint32(5-i)*uint32(a0) + uint32(i)*uint32(a1)) / 5)
					}
					table[6] = 0
					table[7] = 255
				}
				var alphaBits uint64
				for i := 0; i < 6; i++ {
					alphaBits |= uint64(block[2+i]) << (8 * uint(i))
				}
				for i := 0; i < 16; i++ {
					pixels[i][3] = table[(alphaBits>>(3*uint(i)))&0x7]
				}
			}

			for py := 0; py < 4; py++ {
				for px := 0; px < 4; px++ {
					x, y := bx*4+px, by*4+py
					if x >= width || y >= height {
						continue
					}
					p := pixels[py*4+px]
					o := img.PixOffset(x, y)
					img.Pix[o+0] = p[0]
					img.Pix[o+1] = p[1]
					img.Pix[o+2] = p[2]
					img.Pix[o+3] = p[3]
				}
			}
		}
	}
	return img
}

// decodeMaskedRGBImage decodes uncompressed DDS pixel data using the header's
// channel bit masks (handles 16/24/32 bpp layouts).
func decodeMaskedRGBImage(data []byte, width, height int, info ddsInfo) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	bytePP := info.bitCount / 8

	maskShift := func(mask uint32) (shift, bits uint) {
		if mask == 0 {
			return 0, 0
		}
		for mask&1 == 0 {
			shift++
			mask >>= 1
		}
		for mask&1 == 1 {
			bits++
			mask >>= 1
		}
		return
	}
	rs, rb := maskShift(info.rMask)
	gs, gb := maskShift(info.gMask)
	bs, bb := maskShift(info.bMask)
	as, ab := maskShift(info.aMask)

	extract := func(val uint32, shift, bits uint) uint8 {
		if bits == 0 {
			return 255
		}
		raw := (val >> shift) & ((1 << bits) - 1)
		return uint8(raw * 255 / ((1 << bits) - 1))
	}

	offset := 0
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			var val uint32
			switch bytePP {
			case 4:
				val = binary.LittleEndian.Uint32(data[offset:])
			case 3:
				val = uint32(data[offset]) | uint32(data[offset+1])<<8 | uint32(data[offset+2])<<16
			case 2:
				val = uint32(binary.LittleEndian.Uint16(data[offset:]))
			default:
				val = uint32(data[offset])
			}
			offset += bytePP

			o := img.PixOffset(x, y)
			img.Pix[o+0] = extract(val, rs, rb)
			img.Pix[o+1] = extract(val, gs, gb)
			img.Pix[o+2] = extract(val, bs, bb)
			if info.aMask != 0 {
				img.Pix[o+3] = extract(val, as, ab)
			} else {
				img.Pix[o+3] = 255
			}
		}
	}
	return img
}

// nebulaNames returns the sorted union of pre-converted PNG dirs and
// client-DDS-derived nebula names.
func (s *Server) nebulaNames() []string {
	seen := make(map[string]bool)

	if s.nebulasDir != "" {
		if entries, err := os.ReadDir(s.nebulasDir); err == nil {
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				// Only count dirs with all 6 faces.
				valid := true
				for _, f := range nebulaFaceNames {
					if _, err := os.Stat(filepath.Join(s.nebulasDir, e.Name(), f+".png")); err != nil {
						valid = false
						break
					}
				}
				if valid {
					seen[e.Name()] = true
				}
			}
		}
	}
	for name := range s.nebulaDDS {
		seen[name] = true
	}

	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		return strings.ToLower(names[i]) < strings.ToLower(names[j])
	})
	return names
}
