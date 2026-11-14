package dogma

// AttrMap is a convenience alias for attribute ID → float64 value maps.
type AttrMap = map[int64]float64

// GetAttr returns the value for attrID from m, or defaultVal if not present.
func GetAttr(m AttrMap, attrID int64, defaultVal float64) float64 {
	if v, ok := m[attrID]; ok {
		return v
	}
	return defaultVal
}

// SetDefault sets attrID in m to val only if the key is not already present.
func SetDefault(m AttrMap, attrID int64, val float64) {
	if _, ok := m[attrID]; !ok {
		m[attrID] = val
	}
}
