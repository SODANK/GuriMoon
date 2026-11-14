package esp

import (
	"context"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
)

// Panel colour scheme: two layers (window, background) like the client's
// General Settings colour sliders. See docs/esp-theme.md.

const (
	themeWindowKey     = "esp.theme.window"
	themeBackgroundKey = "esp.theme.background"
)

// themeLayer is one colour layer: red, green, blue and opacity, each 0..255.
type themeLayer struct{ R, G, B, A int }

// themePreset is a named pair of layers offered in the presets dropdown.
type themePreset struct {
	Name               string
	Window, Background themeLayer
}

// themePresets lists the dropdown presets. The first entry is the default scheme.
var themePresets = []themePreset{
	{"ESP Red", themeLayer{39, 17, 17, 255}, themeLayer{23, 10, 10, 255}},
	{"Dark Opaque", themeLayer{42, 42, 42, 255}, themeLayer{16, 16, 16, 255}},
	{"Dark Transparent", themeLayer{42, 42, 42, 150}, themeLayer{12, 12, 14, 255}},
	{"Crimson", themeLayer{42, 42, 42, 200}, themeLayer{25, 0, 0, 255}},
	{"Amarr Gold", themeLayer{48, 38, 16, 255}, themeLayer{22, 17, 8, 255}},
	{"Caldari Blue", themeLayer{18, 30, 46, 255}, themeLayer{9, 14, 22, 255}},
	{"Gallente Green", themeLayer{16, 40, 30, 255}, themeLayer{8, 20, 15, 255}},
	{"Minmatar Rust", themeLayer{50, 26, 14, 255}, themeLayer{24, 13, 8, 255}},
}

func (l themeLayer) setting() string {
	return fmt.Sprintf("%d,%d,%d,%d", l.R, l.G, l.B, l.A)
}

func (l themeLayer) rgb() themeRGB {
	return themeRGB{float64(l.R), float64(l.G), float64(l.B)}
}

func loadThemeLayer(key string, def themeLayer) themeLayer {
	v := evedb.CachedSettingFloats(key,
		[]float64{float64(def.R), float64(def.G), float64(def.B), float64(def.A)}, 0, 255)
	return themeLayer{int(math.Round(v[0])), int(math.Round(v[1])), int(math.Round(v[2])), int(math.Round(v[3]))}
}

// loadTheme answers the saved window and background layers, else the default preset.
func loadTheme() (win, bg themeLayer) {
	return loadThemeLayer(themeWindowKey, themePresets[0].Window),
		loadThemeLayer(themeBackgroundKey, themePresets[0].Background)
}

// themeRGB is a colour with float channels on the 0..255 scale.
type themeRGB struct{ R, G, B float64 }

func (c themeRGB) hex() string {
	ch := func(v float64) int { return int(math.Round(math.Max(0, math.Min(255, v)))) }
	return fmt.Sprintf("#%02x%02x%02x", ch(c.R), ch(c.G), ch(c.B))
}

// themeMix answers a blended toward b by t (0 = a, 1 = b).
func themeMix(a, b themeRGB, t float64) themeRGB {
	return themeRGB{a.R + (b.R-a.R)*t, a.G + (b.G-a.G)*t, a.B + (b.B-a.B)*t}
}

// themeShift lightens a dark colour by d per channel and darkens a light one.
func themeShift(c themeRGB, d float64) themeRGB {
	if (0.299*c.R+0.587*c.G+0.114*c.B)/255 > 0.5 {
		d = -d
	}
	return themeRGB{c.R + d, c.G + d, c.B + d}
}

// themeAccent keeps the hue of c, scales its HSL saturation by satMul and sets
// its lightness to light. A grey c answers a grey accent.
func themeAccent(c themeRGB, satMul, light float64) themeRGB {
	r, g, b := c.R/255, c.G/255, c.B/255
	mx, mn := math.Max(r, math.Max(g, b)), math.Min(r, math.Min(g, b))
	l := (mx + mn) / 2
	var h, s float64
	if d := mx - mn; d > 0 {
		if l > 0.5 {
			s = d / (2 - mx - mn)
		} else {
			s = d / (mx + mn)
		}
		switch mx {
		case r:
			h = math.Mod((g-b)/d+6, 6)
		case g:
			h = (b-r)/d + 2
		default:
			h = (r-g)/d + 4
		}
		h /= 6
	}
	s = math.Min(1, s*satMul)
	q := light + s - light*s
	if light < 0.5 {
		q = light * (1 + s)
	}
	p := 2*light - q
	hue := func(t float64) float64 {
		t = math.Mod(t+1, 1)
		switch {
		case t < 1.0/6:
			return p + (q-p)*6*t
		case t < 0.5:
			return q
		case t < 2.0/3:
			return p + (q-p)*(2.0/3-t)*6
		}
		return p
	}
	return themeRGB{hue(h+1.0/3) * 255, hue(h) * 255, hue(h-1.0/3) * 255}
}

// themeVars derives the panel CSS variables from the two layers. The JS in
// templates/theme.html ports this function for the live preview; change both.
func themeVars(win, bg themeLayer) [][2]string {
	page := themeMix(themeRGB{}, bg.rgb(), float64(bg.A)/255)
	panel := themeMix(page, win.rgb(), float64(win.A)/255)
	btn := themeAccent(win.rgb(), 1.32, 0.245)
	return [][2]string{
		{"--bg", page.hex()},
		{"--win-deep", themeMix(page, panel, 0.5).hex()},
		{"--win", panel.hex()},
		{"--win-hover", themeShift(panel, 7).hex()},
		{"--win-sel", themeMix(panel, btn, 0.34).hex()},
		{"--line-soft", themeShift(panel, 9).hex()},
		{"--line", themeShift(panel, 17).hex()},
		{"--btn", btn.hex()},
		{"--btn-hi", themeAccent(win.rgb(), 1.32, 0.367).hex()},
		{"--accent", themeAccent(win.rgb(), 1.5, 0.596).hex()},
		{"--accent-hi", themeAccent(win.rgb(), 1.5, 0.725).hex()},
	}
}

// themeCSS answers the :root block layout.html puts ahead of its stylesheet.
func themeCSS() template.CSS {
	win, bg := loadTheme()
	var b strings.Builder
	b.WriteString(":root{")
	for _, kv := range themeVars(win, bg) {
		b.WriteString(kv[0] + ":" + kv[1] + ";")
	}
	b.WriteString("}")
	return template.CSS(b.String())
}

// handleTheme serves the colour scheme page and saves or resets the scheme on POST.
func (s *Server) handleTheme(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	if r.Method == http.MethodPost {
		msg := "Color scheme saved."
		win, bg := themePresets[0].Window, themePresets[0].Background
		if r.FormValue("action") == "reset" {
			msg = "Color scheme reset to " + themePresets[0].Name + "."
		} else {
			win, bg = themeFormLayer(r, "w"), themeFormLayer(r, "b")
		}
		if err := evedb.SetServerSetting(ctx, s.db, themeWindowKey, win.setting()); err != nil {
			msg = "Save failed: " + err.Error()
		} else if err := evedb.SetServerSetting(ctx, s.db, themeBackgroundKey, bg.setting()); err != nil {
			msg = "Save failed: " + err.Error()
		}
		http.Redirect(w, r, "/theme?msg="+url.QueryEscape(msg), http.StatusSeeOther)
		return
	}

	win, bg := loadTheme()
	s.render(w, "theme.html", map[string]any{
		"Page": "theme",
		"Layers": []themeFormSection{
			{"Window color", "w", win},
			{"Background color", "b", bg},
		},
		"Presets": themePresets,
		"Msg":     r.URL.Query().Get("msg"),
	})
}

// themeFormSection is one slider panel on the theme page; P prefixes its field names.
type themeFormSection struct {
	Title string
	P     string
	L     themeLayer
}

// themeFormLayer reads fields <prefix>R, G, B and A, each clamped to 0..255.
func themeFormLayer(r *http.Request, prefix string) themeLayer {
	ch := func(name string) int {
		v, err := strconv.Atoi(strings.TrimSpace(r.FormValue(prefix + name)))
		if err != nil {
			return 0
		}
		return max(0, min(255, v))
	}
	return themeLayer{ch("R"), ch("G"), ch("B"), ch("A")}
}
