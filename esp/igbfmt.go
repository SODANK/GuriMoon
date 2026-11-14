package esp

// Number formatting shared by the Galnet sites.

import (
	"fmt"
	"math"
)

// The direction colours of a price or count move.
const (
	igbUpColor   = "#46c36f"
	igbDownColor = "#e5534b"
)

// igbSign colours a fractional change.
func igbSign(frac float64) string {
	c := newsTextColor
	switch {
	case frac > 0.00005:
		c = igbUpColor
	case frac < -0.00005:
		c = igbDownColor
	}
	return "<font color=" + c + ">" + fmt.Sprintf("%+.1f%%", frac*100) + "</font>"
}

// igbWhole renders a figure rounded to a whole number with separators.
func igbWhole(v float64) string { return igbInt(int64(math.Round(v))) }
