// Command audiorestore rebuilds the RMR client's unplayable space-audio assets from
// audio the client already ships (no CCP asset is committed here).
//
//	go run ./cmd/audiorestore                 # dry run
//	go run ./cmd/audiorestore -apply          # write
//	go run ./cmd/audiorestore -client "D:\EVE"  # override esp.clientPath
//
// Run sql/zz_10_fix_audio_ambience.sql alongside it: that repoints eveGraphics.urlSound
// at the station wrappers written here, and either half alone is inert. Output goes to
// new paths under <client>/res/Sound (a disk overlay); deleting that folder undoes it.
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"gurimoon/config"
)

// ── EmbedFs (.stuff) ────────────────────────────────────────────────────────
//
// Layout: u32 entryCount, then per entry { u32 dataSize, u32 pathLen, path,
// NUL }, then every blob concatenated in index order.

type archive map[string][]byte // key: lowercased, forward-slashed path

func readStuff(path string) (archive, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(raw) < 4 {
		return nil, fmt.Errorf("%s: too short", path)
	}
	count := binary.LittleEndian.Uint32(raw)
	type ent struct {
		name string
		size uint32
	}
	ents := make([]ent, 0, count)
	pos := 4
	for i := uint32(0); i < count; i++ {
		if pos+8 > len(raw) {
			return nil, fmt.Errorf("%s: index truncated at entry %d", path, i)
		}
		size := binary.LittleEndian.Uint32(raw[pos:])
		nameLen := binary.LittleEndian.Uint32(raw[pos+4:])
		pos += 8
		if pos+int(nameLen)+1 > len(raw) {
			return nil, fmt.Errorf("%s: name truncated at entry %d", path, i)
		}
		name := string(raw[pos : pos+int(nameLen)])
		pos += int(nameLen) + 1 // trailing NUL
		ents = append(ents, ent{strings.ToLower(strings.ReplaceAll(name, `\`, "/")), size})
	}
	out := make(archive, len(ents))
	for _, e := range ents {
		if pos+int(e.size) > len(raw) {
			return nil, fmt.Errorf("%s: blob truncated at %s", path, e.name)
		}
		out[e.name] = raw[pos : pos+int(e.size)]
		pos += int(e.size)
	}
	return out, nil
}

// ── station ambience ────────────────────────────────────────────────────────

// The template is a working TriTransform+SoundNode pair whose two media strings are
// swapped for ours at equal length. .blue strings are a u32-LE length plus inline
// bytes, so an equal-length swap needs no length or offset fixup. buildStation enforces
// the sizes.
const (
	tplPath = "res/sound/effect/infestedwreck.blue"
	tplName = "infiltratorstation03bST.ogg"                     // 27
	tplFull = "res:/Sound/Effect/infiltratorstation03bST.ogg"    // 45
	oggDir  = "res:/Sound/Effect/"                               // 18
)

// key -> source ogg under res/sound/station/hangar/. Keys are two characters so every
// generated basename is len(tplName). Station types 06/09/10/14/18/20 are unused, so
// they get no wrapper.
var stationSounds = map[string]string{
	"fl": "stationflame01.ogg",
	"01": "station01.ogg", "02": "station02.ogg", "03": "station03.ogg",
	"04": "station04.ogg", "05": "station05.ogg", "07": "station07.ogg",
	"08": "station08.ogg", "11": "station11.ogg", "12": "station12.ogg",
	"13": "station13.ogg", "15": "station15.ogg", "16": "station16.ogg",
	"17": "station17.ogg", "19": "station19.ogg",
}

// buildStation returns the patched wrapper plus the relative paths for it and for the
// ogg it references.
func buildStation(ar archive, key, srcOgg string) (blue []byte, bluePath, oggPath string, ogg []byte, err error) {
	tpl, ok := ar[tplPath]
	if !ok {
		return nil, "", "", nil, fmt.Errorf("template %s missing", tplPath)
	}
	src := "res/sound/station/hangar/" + srcOgg
	ogg, ok = ar[src]
	if !ok {
		return nil, "", "", nil, fmt.Errorf("source ogg %s missing", src)
	}
	newName := fmt.Sprintf("station_ambience_%s_hum.ogg", key)
	newFull := oggDir + newName
	if len(newName) != len(tplName) || len(newFull) != len(tplFull) {
		return nil, "", "", nil, fmt.Errorf("length mismatch: %q(%d/%d) %q(%d/%d)",
			newName, len(newName), len(tplName), newFull, len(newFull), len(tplFull))
	}
	// Replace the long form first; the bare name then occurs only standalone.
	out := []byte(strings.Replace(string(tpl), tplFull, newFull, 1))
	out = []byte(strings.Replace(string(out), tplName, newName, 1))
	if len(out) != len(tpl) {
		return nil, "", "", nil, fmt.Errorf("%s: size changed %d -> %d", key, len(tpl), len(out))
	}
	if err := checkPrefix(out, newFull); err != nil {
		return nil, "", "", nil, fmt.Errorf("%s: %w", key, err)
	}
	return out,
		fmt.Sprintf("res/Sound/Station/StationAmb%s.blue", strings.ToUpper(key)),
		"res/Sound/Effect/" + newName,
		ogg, nil
}

// checkPrefix verifies the u32-LE length before s equals len(s), which makes an
// equal-length swap safe.
func checkPrefix(data []byte, s string) error {
	i := strings.Index(string(data), s)
	if i < 4 {
		return fmt.Errorf("string %q not found (or no room for prefix)", s)
	}
	if got := binary.LittleEndian.Uint32(data[i-4:]); int(got) != len(s) {
		return fmt.Errorf("length prefix for %q is %d, want %d", s, got, len(s))
	}
	return nil
}

// ── FX media ────────────────────────────────────────────────────────────────

// The .blue files are intact but their media exists in no archive, so writing a target
// path is a pure addition and no .blue needs editing.
var fxDrops = []struct{ target, source, note string }{
	{"res/Sound/Effect/warpfx01.wav", "res/sound/global/warpdirectional.wav", "warpCrack — warp tear"},
	{"res/Sound/Jumpgate/aj1.wav", "res/sound/jumpgate/jumpgatemixb01c.wav", "jumpgate — gate jump"},
	{"res/Sound/Thruster/small01.wav", "res/sound/thruster/thruster02loopa.wav", "missile — flight loop"},
	{"res/Sound/Effect/flies02bst.ogg", "res/sound/effect/flies06l.ogg", "buzz — (unreferenced)"},
	// The client ships one .mp3, so these two targets get WAV content under an .mp3 name.
	// fxRepoints below covers a decoder that does not sniff.
	{"res/Sound/Effect/airflight06ast.mp3", "res/sound/global/warpon.wav", "warpWind — warp loop [container mismatch]"},
	{"res/Sound/Effect/warp02ast.mp3", "res/sound/global/warpentry.wav", "warpStart — warp entry [container mismatch]"},
}

// For the two .mp3-named targets: a correctly named .wav plus a loose .blue repointed
// at it by the same equal-length swap. It takes effect only if loose files shadow
// archived ones.
var fxRepoints = []struct{ blue, oldName, newName, source string }{
	{"res/sound/effect/warpwind.blue", "airflight06ast.mp3", "warpwindloop01.wav", "res/sound/global/warpon.wav"},
	{"res/sound/effect/warpstart.blue", "warp02ast.mp3", "warpstrt1.wav", "res/sound/global/warpentry.wav"},
}

func buildRepoint(ar archive, bluePath, oldName, newName string) ([]byte, error) {
	d, ok := ar[bluePath]
	if !ok {
		return nil, fmt.Errorf("%s missing", bluePath)
	}
	if len(oldName) != len(newName) {
		return nil, fmt.Errorf("%s: %q/%q length %d != %d", bluePath, oldName, newName, len(oldName), len(newName))
	}
	oldFull, newFull := oggDir+oldName, oggDir+newName
	if n := strings.Count(string(d), oldFull); n != 1 {
		return nil, fmt.Errorf("%s: %d occurrences of %q, want 1", bluePath, n, oldFull)
	}
	out := []byte(strings.Replace(string(d), oldFull, newFull, 1))
	out = []byte(strings.Replace(string(out), oldName, newName, 1))
	if len(out) != len(d) {
		return nil, fmt.Errorf("%s: size changed", bluePath)
	}
	if err := checkPrefix(out, newFull); err != nil {
		return nil, fmt.Errorf("%s: %w", bluePath, err)
	}
	return out, nil
}

// ── main ────────────────────────────────────────────────────────────────────

type write struct {
	rel  string
	data []byte
	note string
}

func main() {
	apply := flag.Bool("apply", false, "write changes (default: dry-run report)")
	cfgPath := flag.String("config", "config.json", "config.json path")
	clientFlag := flag.String("client", "", "client folder (default: esp.clientPath from config)")
	flag.Parse()

	client := *clientFlag
	if client == "" {
		cfg, err := config.Load(*cfgPath)
		if err != nil {
			log.Fatalf("need -client, or a readable config: %v", err)
		}
		client = cfg.ESP.ClientPath
	}
	if client == "" {
		log.Fatal("no client path: set esp.clientPath in config.json or pass -client")
	}
	ar, err := readStuff(filepath.Join(client, "resSND.stuff"))
	if err != nil {
		log.Fatalf("read resSND.stuff: %v", err)
	}
	fmt.Printf("client %s — resSND.stuff: %d entries\n\n", client, len(ar))

	var plan []write

	fmt.Println("station ambience wrappers:")
	keys := make([]string, 0, len(stationSounds))
	for k := range stationSounds {
		keys = append(keys, k)
	}
	sortStrings(keys)
	for _, k := range keys {
		blue, bluePath, oggPath, ogg, err := buildStation(ar, k, stationSounds[k])
		if err != nil {
			log.Fatalf("station %s: %v", k, err)
		}
		plan = append(plan, write{bluePath, blue, ""}, write{oggPath, ogg, ""})
		fmt.Printf("  %-2s %-22s -> %-38s (%.0f KB)\n", k, stationSounds[k], bluePath, float64(len(ogg))/1024)
	}

	fmt.Println("\nFX media (substitutions — the originals are in no archive):")
	for _, d := range fxDrops {
		src, ok := ar[d.source]
		if !ok {
			log.Fatalf("source %s missing", d.source)
		}
		if _, clash := ar[strings.ToLower(d.target)]; clash {
			log.Fatalf("%s exists in the archive — writing it would shadow shipped data", d.target)
		}
		plan = append(plan, write{d.target, src, d.note})
		fmt.Printf("  %-38s <- %-26s %s\n", d.target, filepath.Base(d.source), d.note)
	}

	fmt.Println("\nFX .blue repoints (fallback for the two .mp3-named targets):")
	for _, r := range fxRepoints {
		out, err := buildRepoint(ar, r.blue, r.oldName, r.newName)
		if err != nil {
			log.Fatalf("repoint: %v", err)
		}
		src, ok := ar[r.source]
		if !ok {
			log.Fatalf("source %s missing", r.source)
		}
		plan = append(plan,
			write{r.blue, out, ""},
			write{"res/Sound/Effect/" + r.newName, src, ""})
		fmt.Printf("  %-30s %s -> %s\n", filepath.Base(r.blue), r.oldName, r.newName)
	}

	skipped := 0
	final := plan[:0]
	for _, w := range plan {
		if _, err := os.Stat(filepath.Join(client, filepath.FromSlash(w.rel))); err == nil {
			skipped++
			continue
		}
		final = append(final, w)
	}
	fmt.Printf("\n%d files to write, %d already present (left alone)\n", len(final), skipped)
	if !*apply {
		fmt.Println("DRY RUN — rerun with -apply to write")
		return
	}
	for _, w := range final {
		dst := filepath.Join(client, filepath.FromSlash(w.rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			log.Fatalf("mkdir %s: %v", filepath.Dir(dst), err)
		}
		if err := os.WriteFile(dst, w.data, 0o644); err != nil {
			log.Fatalf("write %s: %v", dst, err)
		}
	}
	fmt.Printf("wrote %d files\nnow load sql/zz_10_fix_audio_ambience.sql and restart the server\n", len(final))
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
