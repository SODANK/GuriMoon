package db

// Player-character portrait data for the admin panel. A portrait is not an image on the server:
// character_ stores the appearance columns (twelve item IDs, rotations, camera, sixteen morphs)
// and the client renders the face from them (esp/portraitstore.go holds the picture side). The
// nullable item slots (accessory, beard, deco, lipstick, makeup) store 0 as NULL because the
// client's face.py DoSetItem raises KeyError on item ID 0; the seven NOT NULL slots refuse a
// zero. Legal IDs depend on (bloodlineID, gender) through the chrBL* tables
// (ESPGetAllAppearanceOptions in espdb.go).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ESPCharAppearance is every character_ column that feeds the portrait
// renderer.  Item IDs use 0 for "none"; the nullable slots store that as SQL
// NULL on write and read back as 0.
type ESPCharAppearance struct {
	// Item slots.
	AccessoryID  int64
	BeardID      int64
	CostumeID    int64
	DecoID       int64
	EyebrowsID   int64
	EyesID       int64
	HairID       int64
	LipstickID   int64
	MakeupID     int64
	SkinID       int64
	BackgroundID int64
	LightID      int64

	// Framing.
	HeadRotation1 float64
	HeadRotation2 float64
	HeadRotation3 float64
	EyeRotation1  float64
	EyeRotation2  float64
	EyeRotation3  float64
	CamPos1       float64
	CamPos2       float64
	CamPos3       float64

	// Face morphs: four blend targets, each pulled east/north/south/west.
	Morph1e, Morph1n, Morph1s, Morph1w float64
	Morph2e, Morph2n, Morph2s, Morph2w float64
	Morph3e, Morph3n, Morph3s, Morph3w float64
	Morph4e, Morph4n, Morph4s, Morph4w float64
}

// espRequiredSlots are the NOT NULL item columns.  A face without them does
// not render, so ESPSetCharAppearance refuses a zero instead of writing one.
var espRequiredSlots = []struct {
	label string
	get   func(*ESPCharAppearance) int64
}{
	{"Costume", func(a *ESPCharAppearance) int64 { return a.CostumeID }},
	{"Eyebrows", func(a *ESPCharAppearance) int64 { return a.EyebrowsID }},
	{"Eyes", func(a *ESPCharAppearance) int64 { return a.EyesID }},
	{"Hair", func(a *ESPCharAppearance) int64 { return a.HairID }},
	{"Skin", func(a *ESPCharAppearance) int64 { return a.SkinID }},
	{"Background", func(a *ESPCharAppearance) int64 { return a.BackgroundID }},
	{"Light", func(a *ESPCharAppearance) int64 { return a.LightID }},
}

// ESPGetCharAppearance loads the portrait columns for a player character.
func ESPGetCharAppearance(ctx context.Context, db *sql.DB, charID int64) (*ESPCharAppearance, error) {
	var (
		a                                                  ESPCharAppearance
		accessoryID, beardID, decoID, lipstickID, makeupID sql.NullInt64
		costumeID, eyebrowsID, eyesID, hairID, skinID      sql.NullInt64
		backgroundID, lightID                              sql.NullInt64
		m1e, m1n, m1s, m1w, m2e, m2n, m2s, m2w             sql.NullFloat64
		m3e, m3n, m3s, m3w, m4e, m4n, m4s, m4w             sql.NullFloat64
	)
	err := db.QueryRowContext(ctx, `
		SELECT accessoryID, beardID, costumeID, decoID, eyebrowsID, eyesID, hairID,
		       lipstickID, makeupID, skinID, backgroundID, lightID,
		       headRotation1, headRotation2, headRotation3,
		       eyeRotation1, eyeRotation2, eyeRotation3,
		       camPos1, camPos2, camPos3,
		       morph1e, morph1n, morph1s, morph1w,
		       morph2e, morph2n, morph2s, morph2w,
		       morph3e, morph3n, morph3s, morph3w,
		       morph4e, morph4n, morph4s, morph4w
		FROM character_ WHERE characterID = ?`, charID,
	).Scan(&accessoryID, &beardID, &costumeID, &decoID, &eyebrowsID, &eyesID, &hairID,
		&lipstickID, &makeupID, &skinID, &backgroundID, &lightID,
		&a.HeadRotation1, &a.HeadRotation2, &a.HeadRotation3,
		&a.EyeRotation1, &a.EyeRotation2, &a.EyeRotation3,
		&a.CamPos1, &a.CamPos2, &a.CamPos3,
		&m1e, &m1n, &m1s, &m1w, &m2e, &m2n, &m2s, &m2w,
		&m3e, &m3n, &m3s, &m3w, &m4e, &m4n, &m4s, &m4w)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("character %d not found", charID)
		}
		return nil, fmt.Errorf("ESPGetCharAppearance %d: %w", charID, err)
	}

	a.AccessoryID = accessoryID.Int64
	a.BeardID = beardID.Int64
	a.CostumeID = costumeID.Int64
	a.DecoID = decoID.Int64
	a.EyebrowsID = eyebrowsID.Int64
	a.EyesID = eyesID.Int64
	a.HairID = hairID.Int64
	a.LipstickID = lipstickID.Int64
	a.MakeupID = makeupID.Int64
	a.SkinID = skinID.Int64
	a.BackgroundID = backgroundID.Int64
	a.LightID = lightID.Int64
	a.Morph1e, a.Morph1n, a.Morph1s, a.Morph1w = m1e.Float64, m1n.Float64, m1s.Float64, m1w.Float64
	a.Morph2e, a.Morph2n, a.Morph2s, a.Morph2w = m2e.Float64, m2n.Float64, m2s.Float64, m2w.Float64
	a.Morph3e, a.Morph3n, a.Morph3s, a.Morph3w = m3e.Float64, m3n.Float64, m3s.Float64, m3w.Float64
	a.Morph4e, a.Morph4n, a.Morph4s, a.Morph4w = m4e.Float64, m4n.Float64, m4s.Float64, m4w.Float64
	return &a, nil
}

// ESPSetCharAppearance overwrites the portrait columns of an existing character. Nullable slots
// take SQL NULL for 0; the seven required slots refuse it. The foreign keys to the chr* master
// tables validate the rest: a bad ID returns a constraint error.
func ESPSetCharAppearance(ctx context.Context, db *sql.DB, charID int64, a *ESPCharAppearance) error {
	for _, slot := range espRequiredSlots {
		if slot.get(a) <= 0 {
			return fmt.Errorf("%s must be set — the client cannot render a face without it", slot.label)
		}
	}
	res, err := db.ExecContext(ctx, `
		UPDATE character_ SET
			accessoryID = ?, beardID = ?, costumeID = ?, decoID = ?, eyebrowsID = ?, eyesID = ?,
			hairID = ?, lipstickID = ?, makeupID = ?, skinID = ?, backgroundID = ?, lightID = ?,
			headRotation1 = ?, headRotation2 = ?, headRotation3 = ?,
			eyeRotation1 = ?, eyeRotation2 = ?, eyeRotation3 = ?,
			camPos1 = ?, camPos2 = ?, camPos3 = ?,
			morph1e = ?, morph1n = ?, morph1s = ?, morph1w = ?,
			morph2e = ?, morph2n = ?, morph2s = ?, morph2w = ?,
			morph3e = ?, morph3n = ?, morph3s = ?, morph3w = ?,
			morph4e = ?, morph4n = ?, morph4s = ?, morph4w = ?
		WHERE characterID = ?`,
		nullIfZero(a.AccessoryID), nullIfZero(a.BeardID), a.CostumeID, nullIfZero(a.DecoID),
		a.EyebrowsID, a.EyesID, a.HairID,
		nullIfZero(a.LipstickID), nullIfZero(a.MakeupID), a.SkinID, a.BackgroundID, a.LightID,
		a.HeadRotation1, a.HeadRotation2, a.HeadRotation3,
		a.EyeRotation1, a.EyeRotation2, a.EyeRotation3,
		a.CamPos1, a.CamPos2, a.CamPos3,
		a.Morph1e, a.Morph1n, a.Morph1s, a.Morph1w,
		a.Morph2e, a.Morph2n, a.Morph2s, a.Morph2w,
		a.Morph3e, a.Morph3n, a.Morph3s, a.Morph3w,
		a.Morph4e, a.Morph4n, a.Morph4s, a.Morph4w,
		charID)
	if err != nil {
		return fmt.Errorf("ESPSetCharAppearance %d: %w", charID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// MySQL reports 0 for matched-but-unchanged too, so check for a missing row explicitly.
		var one int
		if err := db.QueryRowContext(ctx,
			`SELECT 1 FROM character_ WHERE characterID = ?`, charID).Scan(&one); errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("character %d not found", charID)
		}
	}
	return nil
}

// ESPCharShip is the hull a character is currently sitting in.
type ESPCharShip struct {
	ItemID   int64
	TypeID   int64
	Name     string // itemName, or the type name when the ship is unnamed
	TypeName string
}

// ESPGetCharacterShip resolves the character's current ship. The character is an entity whose
// locationID is the ship (or the capsule after a pod-out), so the hull is one self-join away.
// Returns (nil, nil) when the character sits in nothing the entity table knows.
func ESPGetCharacterShip(ctx context.Context, db *sql.DB, charID int64) (*ESPCharShip, error) {
	var s ESPCharShip
	err := db.QueryRowContext(ctx, `
		SELECT ship.itemID, ship.typeID,
		       COALESCE(NULLIF(ship.itemName, ''), t.typeName, ''),
		       COALESCE(t.typeName, '')
		FROM entity c
		JOIN entity ship ON ship.itemID = c.locationID
		LEFT JOIN invTypes t ON t.typeID = ship.typeID
		WHERE c.itemID = ?`, charID).
		Scan(&s.ItemID, &s.TypeID, &s.Name, &s.TypeName)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("ESPGetCharacterShip %d: %w", charID, err)
	}
	return &s, nil
}

// ESPCharSkillTotals is the skill-point summary shown on the character page.
type ESPCharSkillTotals struct {
	TotalSP int64
	Known   int
	AtFive  int
}

// ESPGetCharSkillTotals sums the character's injected skills: attributeID 276 is skillPoints,
// 280 is skillLevel (the pair ESPGetCharacterSkills reads), counted in SQL.
func ESPGetCharSkillTotals(ctx context.Context, db *sql.DB, charID int64) (ESPCharSkillTotals, error) {
	var t ESPCharSkillTotals
	err := db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(COALESCE(sp.valueInt, 0)), 0),
		       COUNT(*),
		       COALESCE(SUM(COALESCE(lv.valueInt, 0) >= 5), 0)
		FROM entity e
		LEFT JOIN entity_attributes sp ON sp.itemID = e.itemID AND sp.attributeID = 276
		LEFT JOIN entity_attributes lv ON lv.itemID = e.itemID AND lv.attributeID = 280
		WHERE e.ownerID = ? AND e.flag IN (7, 61)`, charID).
		Scan(&t.TotalSP, &t.Known, &t.AtFive)
	if err != nil {
		return t, fmt.Errorf("ESPGetCharSkillTotals %d: %w", charID, err)
	}
	return t, nil
}
