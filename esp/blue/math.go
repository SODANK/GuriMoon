// Package blue reads the RMR-era client's Trinity ".blue" scene files and the
// ".tri" meshes they reference, and composes them the way the client does, so
// the ESP dungeon editor can draw a model exactly as the game renders it.
//
// Everything here is derived at runtime from the external client folder; the
// repository carries no CCP bytes. The format is described in docs/blue-format.md.
package blue

import "math"

// Vec3 is a Trinity TriVector.
type Vec3 struct{ X, Y, Z float64 }

// Quat is a Trinity TriQuaternion (x, y, z, w).
type Quat struct{ X, Y, Z, W float64 }

// Mat4 is a Trinity TriMatrix in D3D row-vector form: v' = v * M, translation in row 3, and a
// child's world matrix is local * parent. Its element order is glTF's column-major array, so
// GLTF() is a plain copy. See docs/blue-format.md#matrices.
type Mat4 [4][4]float64

// Identity returns the identity matrix.
func Identity() Mat4 {
	return Mat4{{1, 0, 0, 0}, {0, 1, 0, 0}, {0, 0, 1, 0}, {0, 0, 0, 1}}
}

// Mul returns a * b: a applied first, then b (local * parent).
func (a Mat4) Mul(b Mat4) Mat4 {
	var out Mat4
	for r := 0; r < 4; r++ {
		for c := 0; c < 4; c++ {
			var s float64
			for k := 0; k < 4; k++ {
				s += a[r][k] * b[k][c]
			}
			out[r][c] = s
		}
	}
	return out
}

// QuatMatrix is D3DXMatrixRotationQuaternion.
func QuatMatrix(q Quat) Mat4 {
	x, y, z, w := q.X, q.Y, q.Z, q.W
	return Mat4{
		{1 - 2*(y*y+z*z), 2 * (x*y + z*w), 2 * (x*z - y*w), 0},
		{2 * (x*y - z*w), 1 - 2*(x*x+z*z), 2 * (y*z + x*w), 0},
		{2 * (x*z + y*w), 2 * (y*z - x*w), 1 - 2*(x*x+y*y), 0},
		{0, 0, 0, 1},
	}
}

// FromSRT builds scale * rotation * translation, which is how Trinity derives
// a transform node's localTransform from its scaling/rotation/translation.
func FromSRT(s Vec3, q Quat, t Vec3) Mat4 {
	m := QuatMatrix(q)
	for c := 0; c < 3; c++ {
		m[0][c] *= s.X
		m[1][c] *= s.Y
		m[2][c] *= s.Z
	}
	m[3][0], m[3][1], m[3][2] = t.X, t.Y, t.Z
	return m
}

// RotationX/Y/Z are the D3DX rotation matrices (row-vector form).
func RotationX(a float64) Mat4 {
	c, s := math.Cos(a), math.Sin(a)
	return Mat4{{1, 0, 0, 0}, {0, c, s, 0}, {0, -s, c, 0}, {0, 0, 0, 1}}
}

func RotationY(a float64) Mat4 {
	c, s := math.Cos(a), math.Sin(a)
	return Mat4{{c, 0, -s, 0}, {0, 1, 0, 0}, {s, 0, c, 0}, {0, 0, 0, 1}}
}

func RotationZ(a float64) Mat4 {
	c, s := math.Cos(a), math.Sin(a)
	return Mat4{{c, s, 0, 0}, {-s, c, 0, 0}, {0, 0, 1, 0}, {0, 0, 0, 1}}
}

// YawPitchRoll is D3DXMatrixRotationYawPitchRoll: yaw about Y, pitch about X, roll about Z,
// roll applied first, angles in radians (what TriQuaternion.SetYawPitchRoll feeds
// SetStaticRotation).
func YawPitchRoll(yaw, pitch, roll float64) Mat4 {
	return RotationZ(roll).Mul(RotationX(pitch)).Mul(RotationY(yaw))
}

// TransformCoord transforms a point (w = 1).
func (m Mat4) TransformCoord(v Vec3) Vec3 {
	return Vec3{
		X: v.X*m[0][0] + v.Y*m[1][0] + v.Z*m[2][0] + m[3][0],
		Y: v.X*m[0][1] + v.Y*m[1][1] + v.Z*m[2][1] + m[3][1],
		Z: v.X*m[0][2] + v.Y*m[1][2] + v.Z*m[2][2] + m[3][2],
	}
}

// Translation returns row 3.
func (m Mat4) Translation() Vec3 { return Vec3{m[3][0], m[3][1], m[3][2]} }

// Det3 is the determinant of the upper 3x3. It is negative for a mirroring node.
func (m Mat4) Det3() float64 {
	return m[0][0]*(m[1][1]*m[2][2]-m[1][2]*m[2][1]) -
		m[0][1]*(m[1][0]*m[2][2]-m[1][2]*m[2][0]) +
		m[0][2]*(m[1][0]*m[2][1]-m[1][1]*m[2][0])
}

// GLTF returns the matrix as glTF's column-major 16-float array. No transpose:
// see the Mat4 doc comment.
func (m Mat4) GLTF() [16]float32 {
	var out [16]float32
	for r := 0; r < 4; r++ {
		for c := 0; c < 4; c++ {
			out[r*4+c] = float32(m[r][c])
		}
	}
	return out
}

// IsIdentity reports whether every element is within eps of the identity.
func (m Mat4) IsIdentity(eps float64) bool {
	id := Identity()
	for r := 0; r < 4; r++ {
		for c := 0; c < 4; c++ {
			if math.Abs(m[r][c]-id[r][c]) > eps {
				return false
			}
		}
	}
	return true
}

// Len returns the vector length.
func (v Vec3) Len() float64 { return math.Sqrt(v.X*v.X + v.Y*v.Y + v.Z*v.Z) }
