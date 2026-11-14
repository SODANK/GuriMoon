package db

// Skill prerequisites. A skill book carries requiredSkill1/2/3 (attrs 182/183/184) with
// levels (277/278/279). The client only greys out Train Skill when they are unmet, so the
// gate lives here and every training entry point runs through it.
// See docs/character-skills.md#skill-training.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// skillLevelRoman renders a skill level 1-5 as its roman numeral; out-of-range levels
// render as the bare number.
func skillLevelRoman(level int) string {
	switch level {
	case 1:
		return "I"
	case 2:
		return "II"
	case 3:
		return "III"
	case 4:
		return "IV"
	case 5:
		return "V"
	}
	return fmt.Sprintf("%d", level)
}

// SkillPrereqError reports that a skill cannot be trained because required skills are
// missing. Text is the client-facing list ("Navigation IV, Evasive Maneuvering III") for
// the SkillHasPrerequisites user error.
type SkillPrereqError struct {
	TypeID   int64
	TypeName string
	Missing  []SkillReq
	Text     string
}

func (e *SkillPrereqError) Error() string {
	return fmt.Sprintf("%s requires %s", e.TypeName, e.Text)
}

// AsSkillPrereqError unwraps err into a *SkillPrereqError, or returns nil for any other
// failure.
func AsSkillPrereqError(err error) *SkillPrereqError {
	var pe *SkillPrereqError
	if errors.As(err, &pe) {
		return pe
	}
	return nil
}

// ErrSkillMaxLevel is returned when a skill is already trained (or queued) to level 5.
var ErrSkillMaxLevel = errors.New("skill is already at level 5")

// GetCharacterSkillLevels returns the trained level of every skill injected into
// charID's brain, keyed by skill typeID. A book with no skillLevel attribute is level 0.
func GetCharacterSkillLevels(ctx context.Context, db *sql.DB, charID int64) (map[int64]int, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT e.typeID, COALESCE(lv.valueInt, 0)
		FROM entity e
		LEFT JOIN entity_attributes lv ON lv.itemID = e.itemID AND lv.attributeID = 280
		WHERE e.ownerID = ? AND e.flag IN (7, 61)`, charID)
	if err != nil {
		return nil, fmt.Errorf("GetCharacterSkillLevels: %w", err)
	}
	defer rows.Close()
	levels := make(map[int64]int)
	for rows.Next() {
		var typeID int64
		var lvl int
		if err := rows.Scan(&typeID, &lvl); err != nil {
			return nil, fmt.Errorf("GetCharacterSkillLevels scan: %w", err)
		}
		if lvl > levels[typeID] {
			levels[typeID] = lvl
		}
	}
	return levels, rows.Err()
}

// UnmetSkillReqs filters reqs down to the ones neither trained (have) nor promised by an
// earlier plan step (projected). Both maps may be nil.
func UnmetSkillReqs(reqs []SkillReq, have, projected map[int64]int) []SkillReq {
	var missing []SkillReq
	for _, r := range reqs {
		lvl := have[r.SkillID]
		if p := projected[r.SkillID]; p > lvl {
			lvl = p
		}
		if lvl < r.Level {
			missing = append(missing, r)
		}
	}
	return missing
}

// FormatSkillReqs renders reqs as "Navigation IV, Astrometrics III", resolving
// type names.
func FormatSkillReqs(ctx context.Context, db *sql.DB, reqs []SkillReq) string {
	parts := make([]string, 0, len(reqs))
	for _, r := range reqs {
		name, _ := GetTypeName(ctx, db, r.SkillID)
		if name == "" {
			name = fmt.Sprintf("type %d", r.SkillID)
		}
		parts = append(parts, name+" "+skillLevelRoman(r.Level))
	}
	return strings.Join(parts, ", ")
}

// GetSkillPrereqMap returns the prerequisites of every skill type (invGroups categoryID
// 16) in one query, keyed by skill typeID, for pages that walk the whole catalogue.
func GetSkillPrereqMap(ctx context.Context, db *sql.DB) (map[int64][]SkillReq, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT ta.typeID, ta.attributeID,
		       COALESCE(NULLIF(ta.valueFloat, 0), ta.valueInt, 0)
		FROM dgmTypeAttributes ta
		JOIN invTypes t  ON t.typeID = ta.typeID
		JOIN invGroups g ON g.groupID = t.groupID AND g.categoryID = 16
		WHERE ta.attributeID IN (182, 183, 184, 277, 278, 279)`)
	if err != nil {
		return nil, fmt.Errorf("GetSkillPrereqMap: %w", err)
	}
	defer rows.Close()
	// slot values per type: [skill1, skill2, skill3, lvl1, lvl2, lvl3]
	raw := make(map[int64][6]float64)
	slot := map[int64]int{182: 0, 183: 1, 184: 2, 277: 3, 278: 4, 279: 5}
	for rows.Next() {
		var typeID, attrID int64
		var val float64
		if err := rows.Scan(&typeID, &attrID, &val); err != nil {
			return nil, fmt.Errorf("GetSkillPrereqMap scan: %w", err)
		}
		v := raw[typeID]
		v[slot[attrID]] = val
		raw[typeID] = v
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make(map[int64][]SkillReq, len(raw))
	for typeID, v := range raw {
		var reqs []SkillReq
		for i := 0; i < 3; i++ {
			skillID := int64(v[i])
			if skillID <= 0 {
				continue
			}
			lvl := int(v[i+3])
			if lvl < 1 {
				lvl = 1
			}
			reqs = append(reqs, SkillReq{SkillID: skillID, Level: lvl})
		}
		if len(reqs) > 0 {
			out[typeID] = reqs
		}
	}
	return out, nil
}

// CheckSkillPrereqs verifies charID may train skill typeID. projected overlays levels
// promised but not yet trained (the active training's target and the queued levels), so a
// queue may depend on skills earlier in it. Returns a *SkillPrereqError naming what is
// missing, or nil.
func CheckSkillPrereqs(ctx context.Context, db *sql.DB, charID, typeID int64, projected map[int64]int) error {
	reqs, err := GetRequiredSkills(ctx, db, typeID)
	if err != nil {
		return err
	}
	if len(reqs) == 0 {
		return nil
	}
	have, err := GetCharacterSkillLevels(ctx, db, charID)
	if err != nil {
		return err
	}
	missing := UnmetSkillReqs(reqs, have, projected)
	if len(missing) == 0 {
		return nil
	}
	name, _ := GetTypeName(ctx, db, typeID)
	if name == "" {
		name = fmt.Sprintf("type %d", typeID)
	}
	return &SkillPrereqError{
		TypeID:   typeID,
		TypeName: name,
		Missing:  missing,
		Text:     FormatSkillReqs(ctx, db, missing),
	}
}
