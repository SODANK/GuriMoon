package blue

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
)

// ── glb writer ───────────────────────────────────────────────────────────────
//
// One BIN buffer; per mesh node an index view (u32), an interleaved 32-byte vertex view (position,
// normal, first uv set) and a material carrying the area's diffuse colour, optionally with an
// embedded PNG. Node 0 is the model root: it carries the scene extras and the root's scaling and
// translation, and every hull hangs under it with its own matrix.
//
// Indices keep the client's winding. The game is left-handed and three.js right-handed; the
// editor crosses once, in esp/editor/src/coords.ts, so nothing is flipped here.

// GLBNode is one mesh to write.
type GLBNode struct {
	Name      string
	Matrix    [16]float32
	Mesh      *Mesh
	BaseColor [4]float32
	PNG       []byte // optional embedded base colour texture
}

// Scene is what WriteGLB serialises.
type Scene struct {
	Name       string
	RootMatrix [16]float32
	Extras     map[string]any
	Nodes      []GLBNode
}

func pad4(b *bytes.Buffer, fill byte) {
	for b.Len()%4 != 0 {
		b.WriteByte(fill)
	}
}

// WriteGLB serialises the scene as a binary glTF 2.0 file.
func WriteGLB(s Scene) ([]byte, error) {
	if len(s.Nodes) == 0 {
		return nil, errors.New("glb: no meshes")
	}
	var bin bytes.Buffer
	var bufferViews, accessors, meshes, materials, nodes, textures, images, samplers []map[string]any
	children := []int{}

	for _, n := range s.Nodes {
		m := n.Mesh
		tris := m.Triangles()
		if len(tris) == 0 || m.VertexCount == 0 {
			continue
		}
		// Index view.
		idxOff := bin.Len()
		var maxIdx uint32
		for _, i := range tris {
			if i > maxIdx {
				maxIdx = i
			}
			_ = binary.Write(&bin, binary.LittleEndian, i)
		}
		if int(maxIdx) >= m.VertexCount {
			return nil, errors.New("glb: index out of range in " + n.Name)
		}
		idxLen := bin.Len() - idxOff
		pad4(&bin, 0)
		bufferViews = append(bufferViews, map[string]any{"buffer": 0, "byteOffset": idxOff, "byteLength": idxLen, "target": 34963})
		idxView := len(bufferViews) - 1

		// Interleaved vertex view.
		vtxOff := bin.Len()
		minP := [3]float32{math.MaxFloat32, math.MaxFloat32, math.MaxFloat32}
		maxP := [3]float32{-math.MaxFloat32, -math.MaxFloat32, -math.MaxFloat32}
		for i := 0; i < m.VertexCount; i++ {
			for k := 0; k < 3; k++ {
				v := m.Positions[3*i+k]
				if v < minP[k] {
					minP[k] = v
				}
				if v > maxP[k] {
					maxP[k] = v
				}
				_ = binary.Write(&bin, binary.LittleEndian, v)
			}
			for k := 0; k < 3; k++ {
				_ = binary.Write(&bin, binary.LittleEndian, m.Normals[3*i+k])
			}
			_ = binary.Write(&bin, binary.LittleEndian, m.UVs[2*i])
			_ = binary.Write(&bin, binary.LittleEndian, m.UVs[2*i+1])
		}
		vtxLen := bin.Len() - vtxOff
		bufferViews = append(bufferViews, map[string]any{"buffer": 0, "byteOffset": vtxOff, "byteLength": vtxLen, "byteStride": 32, "target": 34962})
		vtxView := len(bufferViews) - 1

		accessors = append(accessors,
			map[string]any{"bufferView": idxView, "componentType": 5125, "count": len(tris), "type": "SCALAR", "min": []uint32{0}, "max": []uint32{maxIdx}},
			map[string]any{"bufferView": vtxView, "componentType": 5126, "count": m.VertexCount, "type": "VEC3", "byteOffset": 0, "min": minP[:], "max": maxP[:]},
			map[string]any{"bufferView": vtxView, "componentType": 5126, "count": m.VertexCount, "type": "VEC3", "byteOffset": 12},
			map[string]any{"bufferView": vtxView, "componentType": 5126, "count": m.VertexCount, "type": "VEC2", "byteOffset": 24},
		)
		a0 := len(accessors) - 4

		pbr := map[string]any{
			"baseColorFactor": []float32{n.BaseColor[0], n.BaseColor[1], n.BaseColor[2], 1},
			"metallicFactor":  0.2,
			"roughnessFactor": 0.8,
		}
		if len(n.PNG) > 0 {
			imgOff := bin.Len()
			bin.Write(n.PNG)
			imgLen := bin.Len() - imgOff
			pad4(&bin, 0)
			bufferViews = append(bufferViews, map[string]any{"buffer": 0, "byteOffset": imgOff, "byteLength": imgLen})
			images = append(images, map[string]any{"bufferView": len(bufferViews) - 1, "mimeType": "image/png"})
			if len(samplers) == 0 {
				samplers = append(samplers, map[string]any{"magFilter": 9729, "minFilter": 9987, "wrapS": 10497, "wrapT": 10497})
			}
			textures = append(textures, map[string]any{"source": len(images) - 1, "sampler": 0})
			pbr["baseColorTexture"] = map[string]any{"index": len(textures) - 1}
			pbr["baseColorFactor"] = []float32{1, 1, 1, 1}
		}
		materials = append(materials, map[string]any{"name": n.Name, "pbrMetallicRoughness": pbr, "doubleSided": true})
		meshes = append(meshes, map[string]any{
			"name": n.Name,
			"primitives": []map[string]any{{
				"attributes": map[string]any{"POSITION": a0 + 1, "NORMAL": a0 + 2, "TEXCOORD_0": a0 + 3},
				"indices":    a0,
				"material":   len(materials) - 1,
				"mode":       4,
			}},
		})
		nodes = append(nodes, map[string]any{"name": n.Name, "mesh": len(meshes) - 1, "matrix": n.Matrix[:]})
		children = append(children, len(nodes)) // +1 for the root node inserted below
	}
	if len(nodes) == 0 {
		return nil, errors.New("glb: every mesh was empty")
	}

	root := map[string]any{"name": s.Name, "children": children, "matrix": s.RootMatrix[:]}
	if s.Extras != nil {
		root["extras"] = s.Extras
	}
	allNodes := append([]map[string]any{root}, nodes...)

	doc := map[string]any{
		"asset":       map[string]any{"version": "2.0", "generator": "gurimoon-blue/v" + itoa(PipelineVersion)},
		"scene":       0,
		"scenes":      []map[string]any{{"nodes": []int{0}}},
		"nodes":       allNodes,
		"meshes":      meshes,
		"materials":   materials,
		"accessors":   accessors,
		"bufferViews": bufferViews,
		"buffers":     []map[string]any{{"byteLength": bin.Len()}},
	}
	if len(images) > 0 {
		doc["images"] = images
		doc["textures"] = textures
		doc["samplers"] = samplers
	}
	js, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	var jsb bytes.Buffer
	jsb.Write(js)
	pad4(&jsb, ' ')

	var out bytes.Buffer
	total := 12 + 8 + jsb.Len() + 8 + bin.Len()
	_ = binary.Write(&out, binary.LittleEndian, [3]uint32{0x46546C67, 2, uint32(total)})
	_ = binary.Write(&out, binary.LittleEndian, [2]uint32{uint32(jsb.Len()), 0x4E4F534A})
	out.Write(jsb.Bytes())
	_ = binary.Write(&out, binary.LittleEndian, [2]uint32{uint32(bin.Len()), 0x004E4942})
	out.Write(bin.Bytes())
	return out.Bytes(), nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
