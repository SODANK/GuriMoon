package blue

import (
	"path"
	"strings"
)

// ── Scene composition ────────────────────────────────────────────────────────
//
// What the client draws for a model, and which parts of it Compose reports as the hull, is
// described in docs/blue-format.md#composing-the-hull. A stored localTransform is a stale cache,
// so LocalMatrix builds from scaling, rotation and translation when any is present.
// See docs/blue-format.md#matrices.

const tritbObject = 0

var nodeClasses = map[string]bool{"TriLODGroup": true, "TriTransform": true, "TriSplTransform": true}

var decorMeshes = map[string]bool{
	"zsprite.tri": true, "planetsphereshape.tri": true, "locatorobject.tri": true, "singlepixel.tri": true,
}

var decorNames = map[string]bool{"miniball": true, "lodsprite": true, "bluesprite": true, "__trail": true}

// Hull is one visible mesh of the LOD0 subtree.
type Hull struct {
	Name     string     `json:"name"`
	MeshPath string     `json:"mesh"`
	Matrix   Mat4       `json:"matrix"` // local → root frame (root S/R/T excluded)
	Diffuse  [4]float32 `json:"diffuse"`
	Texture  string     `json:"texture,omitempty"`
	Material string     `json:"material,omitempty"`
}

// Miniball is one authored collision ball, in the root frame.
type Miniball struct {
	Center Vec3    `json:"center"`
	Radius float64 `json:"radius"`
}

// Composed is the LOD0 hull set of a model.
type Composed struct {
	URL                  string     `json:"url3D"`
	RootClass            string     `json:"rootClass"`
	RootName             string     `json:"rootName"`
	RootScaling          Vec3       `json:"rootScaling"`
	RootTranslation      Vec3       `json:"rootTranslation"`
	RootRotation         Quat       `json:"rootRotation"`
	BoundingSphereRadius float64    `json:"boundingSphereRadius"`
	LODCount             int        `json:"lodCount"`
	Hulls                []Hull     `json:"hulls"`
	Miniballs            []Miniball `json:"miniballs"`
	Skipped              []string   `json:"skipped"`
	Warnings             []string   `json:"warnings,omitempty"`
}

// LocalMatrix is a transform node's local matrix as Trinity composes it.
func LocalMatrix(o *Object) Mat4 {
	if o.Has("scaling") || o.Has("rotation") || o.Has("translation") {
		return FromSRT(o.Vec3("scaling", Vec3{1, 1, 1}), o.Quat("rotation"), o.Vec3("translation", Vec3{}))
	}
	if m, ok := o.Mat4("localTransform"); ok {
		return m
	}
	return Identity()
}

// hullReason reports why a transform node is not a hull ("" when it is).
func hullReason(o *Object) string {
	if o.Class != "TriTransform" && o.Class != "TriSplTransform" {
		return "class"
	}
	if o.Int("transformBase", tritbObject) != tritbObject {
		return "transformBase"
	}
	if !o.Bool("display", true) {
		return "hidden"
	}
	model := o.Child("object")
	if model == nil || model.Class != "TriModel" {
		return "no model"
	}
	mesh := model.Str("vertices")
	if mesh == "" {
		return "no mesh"
	}
	if decorMeshes[strings.ToLower(path.Base(strings.ReplaceAll(mesh, `\`, "/")))] {
		return "decor mesh"
	}
	name := strings.ToLower(o.Str("name"))
	if decorNames[name] || strings.HasSuffix(name, "_lod3_turrets_and_damage") {
		return "decor name"
	}
	return ""
}

// Compose walks the LOD0 subtree and returns the hull set.
func Compose(f *File) *Composed {
	root := f.Root
	c := &Composed{
		RootClass:            root.Class,
		RootName:             root.Str("name"),
		RootScaling:          root.Vec3("scaling", Vec3{1, 1, 1}),
		RootTranslation:      root.Vec3("translation", Vec3{}),
		RootRotation:         root.Quat("rotation"),
		BoundingSphereRadius: root.Float("boundingSphereRadius", 0),
		Hulls:                []Hull{},
		Miniballs:            []Miniball{},
		Skipped:              []string{},
		Warnings:             f.Warnings,
	}
	lod0 := root
	if root.Class == "TriLODGroup" {
		levels := root.Objects("children")
		c.LODCount = len(levels)
		if len(levels) == 0 {
			return c
		}
		lod0 = levels[0]
	}
	c.walk(lod0, Identity())
	return c
}

func (c *Composed) walk(o *Object, parent Mat4) {
	if o == nil || !nodeClasses[o.Class] {
		return
	}
	world := LocalMatrix(o).Mul(parent)
	name := o.Str("name")
	if why := hullReason(o); why == "" {
		model := o.Child("object")
		h := Hull{Name: name, MeshPath: model.Str("vertices"), Matrix: world, Diffuse: [4]float32{1, 1, 1, 1}}
		if areas := model.Objects("areas"); len(areas) > 0 {
			a := areas[0]
			h.Material = a.Str("name")
			if mats := a.Objects("areaMaterials"); len(mats) > 0 {
				h.Diffuse = mats[0].Color("diffuse", h.Diffuse)
			}
			if texs := a.Objects("areaTextures"); len(texs) > 0 {
				h.Texture = texs[0].Str("pixels")
			}
		}
		c.Hulls = append(c.Hulls, h)
	} else if strings.EqualFold(name, "miniball") {
		// The guide mesh is a unit sphere of radius 0.5: the ball's radius is half the node's world
		// scale (row 0 of the world matrix).
		ax := Vec3{world[0][0], world[0][1], world[0][2]}
		c.Miniballs = append(c.Miniballs, Miniball{Center: world.Translation(), Radius: 0.5 * ax.Len()})
	} else if o.Class != "TriLODGroup" {
		c.Skipped = append(c.Skipped, name+" ("+why+")")
	}
	for _, k := range o.Objects("children") {
		c.walk(k, world)
	}
}
