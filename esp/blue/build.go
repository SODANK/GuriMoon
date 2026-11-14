package blue

import (
	"fmt"
	"strings"
)

// PipelineVersion is baked into every glb and its cache key. Bump it whenever
// Compose or WriteGLB change what they produce, so cached files are rebuilt
// and browsers stop trusting what they hold.
const PipelineVersion = 1

// BuildOptions tunes Build.
type BuildOptions struct {
	// Textures embeds each hull's diffuse map (not implemented yet; reserved).
	Textures bool
}

// CleanURL strips the trailing " turretType" token spaceObject.LoadModel
// splits off url3D.
func CleanURL(url3D string) string {
	url3D = strings.TrimSpace(url3D)
	if i := strings.IndexByte(url3D, ' '); i > 0 {
		url3D = url3D[:i]
	}
	return url3D
}

// Build reads url3D's .blue and every LOD0 hull mesh through fs and writes a glb of the client's
// scene graph. The root node carries the model's scaling and translation and these extras:
//
//	pipeline             PipelineVersion
//	url3D                the cleaned path
//	rootRotation         [x,y,z,w] the root's authored rotation, not baked in (dunRotation replaces it)
//	boundingSphereRadius the LOD group's radius
//	miniballs            [{x,y,z,r}] authored collision balls, root frame
//	hulls                the hull names in node order
func Build(fs *ResFS, url3D string, opts BuildOptions) ([]byte, *Composed, error) {
	url3D = CleanURL(url3D)
	raw, err := fs.Read(url3D)
	if err != nil {
		return nil, nil, err
	}
	f, err := Parse(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", url3D, err)
	}
	c := Compose(f)
	c.URL = url3D
	if len(c.Hulls) == 0 {
		return nil, c, fmt.Errorf("%s: no hull mesh under LOD0 (%s)", url3D, c.RootClass)
	}

	scene := Scene{
		Name:       url3D,
		RootMatrix: FromSRT(c.RootScaling, Quat{0, 0, 0, 1}, c.RootTranslation).GLTF(),
	}
	meshes := map[string]*Mesh{}
	hullNames := []string{}
	for _, h := range c.Hulls {
		m, ok := meshes[h.MeshPath]
		if !ok {
			b, err := fs.Read(h.MeshPath)
			if err != nil {
				c.Warnings = append(c.Warnings, fmt.Sprintf("%s: %v", h.Name, err))
				continue
			}
			m, err = ParseTri(b)
			if err != nil {
				c.Warnings = append(c.Warnings, fmt.Sprintf("%s: %s: %v", h.Name, h.MeshPath, err))
				continue
			}
			meshes[h.MeshPath] = m
		}
		scene.Nodes = append(scene.Nodes, GLBNode{Name: h.Name, Matrix: h.Matrix.GLTF(), Mesh: m, BaseColor: h.Diffuse})
		hullNames = append(hullNames, h.Name)
	}
	if len(scene.Nodes) == 0 {
		return nil, c, fmt.Errorf("%s: no hull mesh could be read", url3D)
	}
	balls := make([][4]float64, 0, len(c.Miniballs))
	for _, b := range c.Miniballs {
		balls = append(balls, [4]float64{b.Center.X, b.Center.Y, b.Center.Z, b.Radius})
	}
	scene.Extras = map[string]any{
		"pipeline":             PipelineVersion,
		"url3D":                url3D,
		"rootClass":            c.RootClass,
		"rootRotation":         [4]float64{c.RootRotation.X, c.RootRotation.Y, c.RootRotation.Z, c.RootRotation.W},
		"boundingSphereRadius": c.BoundingSphereRadius,
		"miniballs":            balls,
		"hulls":                hullNames,
	}
	glb, err := WriteGLB(scene)
	if err != nil {
		return nil, c, fmt.Errorf("%s: %w", url3D, err)
	}
	return glb, c, nil
}
