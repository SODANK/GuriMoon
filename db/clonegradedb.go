package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"gurimoon/marshal"
)

// Medical clone grades: prices, the atomic purchase, and what a pod death costs.
// Design and price table: docs/medical-clones.md.
const (
	CloneGroupID          int64 = 23  // invGroups "Clone Grade"
	CloneGradeAlphaTypeID int64 = 164 // the free grade a pilot wakes in after a pod death
	attrSkillPointsSaved  int64 = 419 // dgmTypeAttributes: SP a grade protects
	RefCloneActivation          = 8   // market_reftypes "Clone Activation"
)

// CloneGradePrices is basePrice per clone grade: 400,000 × (SPsaved / 1M)^0.85,
// rounded to 1,000; Alpha is free. sql/invtypes.sql carries the same values;
// change both together (docs/medical-clones.md#prices).
var CloneGradePrices = map[int64]float64{
	165:   37000,    // Beta, 60k SP
	166:   52000,    // Gamma, 90k
	9917:  66000,    // Delta, 120k
	9919:  119000,   // Epsilon, 240k
	9921:  168000,   // Zeta, 360k
	9923:  214000,   // Eta, 480k
	9925:  386000,   // Theta, 960k
	9927:  545000,   // Iota, 1.44M
	9929:  696000,   // Kappa, 1.92M
	9931:  1255000,  // Lambda, 3.84M
	9933:  1772000,  // Mu, 5.76M
	9935:  2263000,  // Nu, 7.68M
	9937:  4078000,  // Xi, 15.36M
	9939:  5757000,  // Omicron, 23.04M
	16715: 7351000,  // Pi, 30.72M
	16718: 13251000, // Rho, 61.44M
	23734: 18703000, // Sigma, 92.16M
}

// MigrateCloneGradePrices writes CloneGradePrices into invTypes rows still at
// basePrice 0, so a price an admin set by hand survives. Runs once, guarded by
// server_migrations. Returns rows updated.
func MigrateCloneGradePrices(ctx context.Context, db *sql.DB) (int64, error) {
	const migrationName = "clone-grade-prices"
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS server_migrations (
			name    VARCHAR(64) NOT NULL,
			applied BIGINT NOT NULL,
			PRIMARY KEY (name)
		)`); err != nil {
		return 0, fmt.Errorf("MigrateCloneGradePrices meta table: %w", err)
	}
	var done int
	_ = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM server_migrations WHERE name = ?`, migrationName).Scan(&done)
	if done > 0 {
		return 0, nil
	}
	var total int64
	for typeID, price := range CloneGradePrices {
		res, err := db.ExecContext(ctx,
			`UPDATE invTypes SET basePrice = ? WHERE typeID = ? AND groupID = ? AND basePrice = 0`,
			price, typeID, CloneGroupID)
		if err != nil {
			return total, fmt.Errorf("MigrateCloneGradePrices typeID=%d: %w", typeID, err)
		}
		n, _ := res.RowsAffected()
		total += n
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO server_migrations (name, applied) VALUES (?, UNIX_TIMESTAMP())`,
		migrationName); err != nil {
		return total, fmt.Errorf("MigrateCloneGradePrices guard: %w", err)
	}
	return total, nil
}

// CloneGrade is one clone type as the medical bay sells it.
type CloneGrade struct {
	TypeID  int64
	GroupID int64
	Name    string
	Price   float64
	SPSaved int64
}

// errNotCloneGrade marks a typeID that is missing or outside group 23.
var errNotCloneGrade = errors.New("not a clone grade")

func readCloneGrade(ctx context.Context, q DBTX, typeID int64) (CloneGrade, error) {
	g := CloneGrade{TypeID: typeID}
	var sp sql.NullFloat64
	err := q.QueryRowContext(ctx, `
		SELECT t.groupID, t.typeName, t.basePrice,
		       COALESCE(a.valueInt, a.valueFloat)
		  FROM invTypes t
		  LEFT JOIN dgmTypeAttributes a ON a.typeID = t.typeID AND a.attributeID = ?
		 WHERE t.typeID = ?`, attrSkillPointsSaved, typeID,
	).Scan(&g.GroupID, &g.Name, &g.Price, &sp)
	if errors.Is(err, sql.ErrNoRows) {
		return g, errNotCloneGrade
	}
	if err != nil {
		return g, fmt.Errorf("readCloneGrade typeID=%d: %w", typeID, err)
	}
	if g.GroupID != CloneGroupID {
		return g, errNotCloneGrade
	}
	if sp.Valid {
		g.SPSaved = int64(sp.Float64)
	}
	return g, nil
}

// GetCloneGrade returns the clone grade typeID, or an error when typeID is not one.
func GetCloneGrade(ctx context.Context, db *sql.DB, typeID int64) (CloneGrade, error) {
	return readCloneGrade(ctx, db, typeID)
}

// BuyCloneGrade sells charID the clone grade typeID: it refuses a type outside
// group 23, a grade that saves no more SP than the current one
// (MedicalThisCloneIsWorse), and a wallet that cannot cover the price
// (NotEnoughMoney). The debit, the Clone Activation journal row and the new
// cloneTypeID commit together. The ISK is a sink with no recipient.
func BuyCloneGrade(ctx context.Context, db *sql.DB, charID, typeID int64) (price, newBal float64, err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("BuyCloneGrade begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	want, err := readCloneGrade(ctx, tx, typeID)
	if errors.Is(err, errNotCloneGrade) {
		return 0, 0, MsgErr("CustomInfo", map[string]marshal.PyRep{
			"info": marshal.PyString{Value: "That is not a clone grade."},
		}, fmt.Sprintf("typeID %d is not a clone grade", typeID))
	}
	if err != nil {
		return 0, 0, err
	}

	var curTypeID int64
	if err := tx.QueryRowContext(ctx,
		`SELECT cloneTypeID FROM character_ WHERE characterID = ? FOR UPDATE`, charID,
	).Scan(&curTypeID); err != nil {
		return 0, 0, fmt.Errorf("BuyCloneGrade current clone: %w", err)
	}
	var curSP int64
	if cur, cerr := readCloneGrade(ctx, tx, curTypeID); cerr == nil {
		curSP = cur.SPSaved
	} else if !errors.Is(cerr, errNotCloneGrade) {
		return 0, 0, cerr
	}
	if want.SPSaved <= curSP {
		return 0, 0, MsgErr("MedicalThisCloneIsWorse", nil,
			fmt.Sprintf("%s saves no more skill points than the current clone", want.Name))
	}

	ok, bal, err := TryChargeISK(ctx, tx, charID, want.Price)
	if err != nil {
		return 0, 0, err
	}
	if !ok {
		have, berr := readBalance(ctx, tx, charID)
		if berr != nil {
			return 0, 0, berr
		}
		return want.Price, have, NotEnoughMoneyErr(want.Price, have)
	}
	if want.Price > 0 {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO market_journal
			  (characterID, transDate, refTypeID, ownerID1, ownerID2, argID1,
			   accountID, amount, balance, reason, accountKey)
			VALUES (?, ?, ?, ?, ?, NULL, 0, ?, ?, ?, 1000)`,
			charID, win32TimeNow(), RefCloneActivation, charID, charID,
			-want.Price, bal, JournalReason(want.Name)); err != nil {
			return 0, 0, fmt.Errorf("BuyCloneGrade journal: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE character_ SET cloneTypeID = ? WHERE characterID = ?`, typeID, charID); err != nil {
		return 0, 0, fmt.Errorf("BuyCloneGrade set clone: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("BuyCloneGrade commit: %w", err)
	}
	return want.Price, bal, nil
}

// PodSkillLossEnabled is server_settings `clone.podSkillLoss` (default on). Off,
// a pod death still drops the pilot to an Alpha clone but takes no SP.
func PodSkillLossEnabled() bool {
	v, ok := CachedSetting("clone.podSkillLoss")
	if !ok {
		return true
	}
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil {
		return true
	}
	return b
}

// CloneDeathResult is what ApplyCloneDeath did. SPLost is 0 when the clone
// covered the pilot; the Skill fields are then empty.
type CloneDeathResult struct {
	CloneTypeID  int64 // the grade that was activated (and spent)
	CloneName    string
	SkillItemID  int64
	SkillTypeID  int64
	SkillName    string
	SPLost       int64
	OldSP, NewSP int64
	OldLevel     int64
	NewLevel     int64
}

// pickCloneLossSkill returns the index of the skill a pod death cuts: the one
// with the most SP, ties to the lower typeID. -1 for no skills.
func pickCloneLossSkill(skills []SkillItem) int {
	best := -1
	for i, s := range skills {
		if best < 0 || s.SkillPoints > skills[best].SkillPoints ||
			(s.SkillPoints == skills[best].SkillPoints && s.TypeID < skills[best].TypeID) {
			best = i
		}
	}
	return best
}

// cloneLossFor cuts one skill holding sp at level (rank tc) by excess, never
// below the SP of level-1, so one death costs at most one level. Returns the new
// SP, the new level and the SP taken.
func cloneLossFor(sp, level, tc, excess int64) (newSP, newLevel, lost int64) {
	if excess <= 0 || sp <= 0 {
		return sp, level, 0
	}
	floor := int64(0)
	if level > 1 {
		floor = int64(skillSPForLevel(tc, int(level-1)))
	}
	lost = sp - floor
	if lost > excess {
		lost = excess
	}
	if lost <= 0 {
		return sp, level, 0
	}
	newSP = sp - lost
	newLevel = level
	if level > 0 && newSP < int64(skillSPForLevel(tc, int(level))) {
		newLevel = level - 1
	}
	return newSP, newLevel, lost
}

// ApplyCloneDeath spends charID's clone on a pod death. When the pilot's total SP
// is above the grade's skillPointsSaved, the skill with the most SP loses the
// excess, at most down to its previous level. The clone drops to Alpha. Call it
// inside WithTrainingRetimed: training must be stopped so attribute 276 holds the
// banked SP, and the restart then aims at the new level.
func ApplyCloneDeath(ctx context.Context, db *sql.DB, charID int64) (CloneDeathResult, error) {
	var r CloneDeathResult
	cloneTypeID, err := GetCloneTypeID(ctx, db, charID)
	if err != nil {
		return r, err
	}
	r.CloneTypeID = cloneTypeID
	var capSP int64
	if g, gerr := readCloneGrade(ctx, db, cloneTypeID); gerr == nil {
		capSP, r.CloneName = g.SPSaved, g.Name
	} else if !errors.Is(gerr, errNotCloneGrade) {
		return r, gerr
	}

	var skill *SkillItem
	var tc int64
	if PodSkillLossEnabled() {
		items, err := GetCharacterSkillItems(ctx, db, charID)
		if err != nil {
			return r, err
		}
		skills := items[:0]
		var total int64
		for _, it := range items {
			if it.CategoryID == 16 {
				skills = append(skills, it)
				total += it.SkillPoints
			}
		}
		if excess := total - capSP; excess > 0 {
			if i := pickCloneLossSkill(skills); i >= 0 {
				s := skills[i]
				tc, err = GetSkillTimeConstant(ctx, db, s.TypeID)
				if err != nil {
					return r, err
				}
				newSP, newLevel, lost := cloneLossFor(s.SkillPoints, s.Level, tc, excess)
				if lost > 0 {
					skill = &s
					r.SkillItemID, r.SkillTypeID = s.ItemID, s.TypeID
					r.OldSP, r.NewSP, r.SPLost = s.SkillPoints, newSP, lost
					r.OldLevel, r.NewLevel = s.Level, newLevel
				}
			}
		}
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return r, fmt.Errorf("ApplyCloneDeath begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if skill != nil {
		for _, kv := range [][2]int64{{276, r.NewSP}, {280, r.NewLevel}} {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO entity_attributes (itemID, attributeID, valueInt, valueFloat)
				VALUES (?, ?, ?, NULL)
				ON DUPLICATE KEY UPDATE valueInt = VALUES(valueInt), valueFloat = NULL`,
				r.SkillItemID, kv[0], kv[1]); err != nil {
				return r, fmt.Errorf("ApplyCloneDeath skill attr %d: %w", kv[0], err)
			}
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE character_ SET cloneTypeID = ? WHERE characterID = ?`,
		CloneGradeAlphaTypeID, charID); err != nil {
		return r, fmt.Errorf("ApplyCloneDeath reset clone: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return r, fmt.Errorf("ApplyCloneDeath commit: %w", err)
	}
	if skill != nil {
		_ = db.QueryRowContext(ctx,
			`SELECT typeName FROM invTypes WHERE typeID = ?`, r.SkillTypeID).Scan(&r.SkillName)
	}
	return r, nil
}
