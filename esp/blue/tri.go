package blue

import (
	"encoding/binary"
	"fmt"
	"math"
)

// ── .tri meshes ──────────────────────────────────────────────────────────────
//
// Version 0x0401 header, vertex and submesh layout: see docs/blue-format.md#tri-meshes.

const triVertexStart = 86

// Submesh is one primitive batch.
type Submesh struct {
	Type   uint32
	Fields [6]uint32
}

// Mesh is a parsed .tri.
type Mesh struct {
	Stride        int
	VertexCount   int
	BSphereRadius float32
	BSphereCenter [3]float32
	BoxMin        [3]float32
	BoxMax        [3]float32
	// Positions and Normals hold 3 floats per vertex, UVs 2. The glb writer builds
	// the interleaved copies.
	Positions []float32
	Normals   []float32
	UVs       []float32
	Submeshes []Submesh
	Indices   []uint16
}

// ParseTri decodes a version 0x0401 .tri.
func ParseTri(d []byte) (*Mesh, error) {
	if len(d) < triVertexStart {
		return nil, fmt.Errorf("tri: file too small (%d bytes)", len(d))
	}
	ver := binary.LittleEndian.Uint16(d)
	if ver != 0x0401 {
		return nil, fmt.Errorf("tri: unsupported version %#04x", ver)
	}
	u32 := func(off int) int { return int(binary.LittleEndian.Uint32(d[off:])) }
	f32 := func(off int) float32 { return math.Float32frombits(binary.LittleEndian.Uint32(d[off:])) }
	m := &Mesh{
		Stride:        u32(14),
		VertexCount:   u32(18),
		BSphereRadius: f32(30),
	}
	nSub, nIdx := u32(22), u32(26)
	for i := 0; i < 3; i++ {
		m.BSphereCenter[i] = f32(34 + 4*i)
		m.BoxMin[i] = f32(46 + 4*i)
		m.BoxMax[i] = f32(58 + 4*i)
	}
	vtxEnd, vtxStart, idxStart := u32(74), u32(78), u32(82)
	if m.Stride < 32 || m.Stride > 256 || vtxStart != triVertexStart {
		return nil, fmt.Errorf("tri: unexpected layout (stride %d, start %d)", m.Stride, vtxStart)
	}
	if vtxEnd != vtxStart+m.VertexCount*m.Stride || idxStart != vtxEnd+nSub*24 || idxStart+nIdx*2 != len(d) {
		return nil, fmt.Errorf("tri: section offsets disagree with counts")
	}
	m.Positions = make([]float32, 0, m.VertexCount*3)
	m.Normals = make([]float32, 0, m.VertexCount*3)
	m.UVs = make([]float32, 0, m.VertexCount*2)
	for i := 0; i < m.VertexCount; i++ {
		off := vtxStart + i*m.Stride
		for k := 0; k < 3; k++ {
			m.Positions = append(m.Positions, f32(off+4*k))
			m.Normals = append(m.Normals, f32(off+12+4*k))
		}
		m.UVs = append(m.UVs, f32(off+24), f32(off+28))
	}
	m.Submeshes = make([]Submesh, nSub)
	for s := 0; s < nSub; s++ {
		off := vtxEnd + s*24
		var sm Submesh
		for k := 0; k < 6; k++ {
			sm.Fields[k] = binary.LittleEndian.Uint32(d[off+4*k:])
		}
		sm.Type = sm.Fields[0]
		m.Submeshes[s] = sm
	}
	m.Indices = make([]uint16, nIdx)
	for i := 0; i < nIdx; i++ {
		m.Indices[i] = binary.LittleEndian.Uint16(d[idxStart+2*i:])
	}
	return m, nil
}

// Triangles flattens every submesh into a triangle list. Strips alternate
// winding and drop degenerate (restart) triangles; lists add their base vertex.
// Indices keep the client's winding, with no handedness change.
func (m *Mesh) Triangles() []uint32 {
	out := make([]uint32, 0, len(m.Indices))
	idx := m.Indices
	for _, sm := range m.Submeshes {
		switch sm.Type {
		case 5: // D3DPT_TRIANGLESTRIP
			start, count := int(sm.Fields[1]), int(sm.Fields[2])
			n := count + 2
			if start+n > len(idx) {
				n = len(idx) - start
			}
			for i := 0; i < n-2; i++ {
				i0, i1, i2 := idx[start+i], idx[start+i+1], idx[start+i+2]
				if i0 == i1 || i1 == i2 || i0 == i2 {
					continue
				}
				if i%2 == 0 {
					out = append(out, uint32(i0), uint32(i1), uint32(i2))
				} else {
					out = append(out, uint32(i0), uint32(i2), uint32(i1))
				}
			}
		case 4: // D3DPT_TRIANGLELIST
			base, start, count := sm.Fields[1], int(sm.Fields[4]), int(sm.Fields[5])
			for i := 0; i < count; i++ {
				off := start + i*3
				if off+2 >= len(idx) {
					break
				}
				out = append(out, uint32(idx[off])+base, uint32(idx[off+1])+base, uint32(idx[off+2])+base)
			}
		}
	}
	if len(out) == 0 {
		// No descriptors: treat the index buffer as a plain list.
		for i := 0; i+2 < len(idx); i += 3 {
			out = append(out, uint32(idx[i]), uint32(idx[i+1]), uint32(idx[i+2]))
		}
	}
	return out
}
