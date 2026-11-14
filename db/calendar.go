package db

import (
	"fmt"
	"time"
)

// ---- New Eden calendar ----
//
// Galnet pages print the real year minus an offset. calendar.ycYearOffset (default 1898, so
// 2026 is YC 128) sets it; calendar.ycYear, when above 0, pins today to that YC year and
// the offset follows the real clock, so older dates still read earlier. Month and day stay
// real. docs/config-settings.md#new-eden-calendar.

// DefaultYCYearOffset puts 2026 at YC 128.
const DefaultYCYearOffset = 1898

// YCYearOffset is the number of years subtracted from a real year, as of now.
func YCYearOffset(now time.Time) int {
	if fixed := CachedSettingFloat("calendar.ycYear", 0, 0, 9999); fixed >= 1 {
		return now.UTC().Year() - int(fixed)
	}
	return int(CachedSettingFloat("calendar.ycYearOffset", DefaultYCYearOffset, 0, 9999))
}

// YCYear is the New Eden year of t.
func YCYear(t time.Time) int { return t.UTC().Year() - YCYearOffset(time.Now()) }

// YCDate renders t as "YC 128-10-03". Dashes, not dots: the in-game browser's size-2 font
// drops "." and ":".
func YCDate(t time.Time) string {
	t = t.UTC()
	return fmt.Sprintf("YC %d%s", YCYear(t), t.Format("-01-02"))
}
