package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"gurimoon/marshal"
)

// ---- Jump clones ----
// A jump clone is a chrJumpClones row parked at a station. Its implants are
// ordinary implant entity rows (flag 89) whose locationID is the jumpCloneID;
// clone IDs start at 2,000,000,000 so they share that namespace with itemIDs.
// The active body's implants keep locationID = characterID.
// See docs/db-9-notes.md#jump-clones.

// InfomorphPsychologyTypeID caps installed clones at its trained level.
const InfomorphPsychologyTypeID = 24242

// Station services that draw the lobby's Medical button
// (station/base.py GetStationServiceInfo); any one opens the window whose Jump
// Clone tab is the only way to install a clone.
const (
	stationServiceCloning    = StationServiceCloning
	stationServiceSurgery    = 1024
	stationServiceDNATherapy = 2048
	stationMedicalServices   = stationServiceCloning | stationServiceSurgery | stationServiceDNATherapy
)

// refJumpCloneInstallationFee is const.refJumpCloneInstallationFee.
const refJumpCloneInstallationFee = 55

// JumpCloneMinStanding is config.json jumpClone.minStanding, set at boot.
var JumpCloneMinStanding = 8.0

// JumpCloneSettings are the levers on jump clones: the standing gate from
// config.json, the rest from server_settings.
type JumpCloneSettings struct {
	// MinStanding is the standing toward the station's owner an install needs; 0
	// turns the gate off. The owner's own members are never refused.
	MinStanding float64
	// InstallCost is paid to the station's owner (const.costJumpClone).
	InstallCost float64
	// CooldownHours between clone jumps (const.limitCloneJumpHours); 0 = none.
	CooldownHours float64
	// RefuseWhileTraining is the era rule JumpSkillInTraining. Off by default; see
	// docs/db-9-notes.md#jump-clones.
	RefuseWhileTraining bool
}

// GetJumpCloneSettings reads the jump clone settings.
func GetJumpCloneSettings(ctx context.Context, db *sql.DB) JumpCloneSettings {
	return JumpCloneSettings{
		MinStanding:         JumpCloneMinStanding,
		InstallCost:         settingFloat(ctx, db, "jumpclone.installCost", 100000, 0, 1e12),
		CooldownHours:       settingFloat(ctx, db, "jumpclone.cooldownHours", 24, 0, 24*365),
		RefuseWhileTraining: settingFloat(ctx, db, "jumpclone.refuseWhileTraining", 0, 0, 1) != 0,
	}
}

// EnsureJumpCloneTables creates the jump-clone persistence tables.
func EnsureJumpCloneTables(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS chrJumpClones (
			jumpCloneID BIGINT NOT NULL AUTO_INCREMENT,
			characterID BIGINT NOT NULL,
			locationID  BIGINT NOT NULL,
			createDate  BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (jumpCloneID),
			KEY idx_char (characterID)
		) AUTO_INCREMENT=2000000000`); err != nil {
		return fmt.Errorf("EnsureJumpCloneTables chrJumpClones: %w", err)
	}
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS chrCloneJumpTime (
			characterID  BIGINT NOT NULL,
			lastJumpDate BIGINT NOT NULL,
			PRIMARY KEY (characterID)
		)`); err != nil {
		return fmt.Errorf("EnsureJumpCloneTables chrCloneJumpTime: %w", err)
	}
	return nil
}

// JumpClone is one installed jump clone.
type JumpClone struct {
	JumpCloneID int64
	CharacterID int64
	LocationID  int64
}

// GetJumpClones returns all installed jump clones for charID.
func GetJumpClones(ctx context.Context, db *sql.DB, charID int64) ([]JumpClone, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT jumpCloneID, characterID, locationID FROM chrJumpClones WHERE characterID = ?`, charID)
	if err != nil {
		return nil, fmt.Errorf("GetJumpClones: %w", err)
	}
	defer rows.Close()
	var out []JumpClone
	for rows.Next() {
		var jc JumpClone
		if err := rows.Scan(&jc.JumpCloneID, &jc.CharacterID, &jc.LocationID); err != nil {
			return nil, fmt.Errorf("GetJumpClones scan: %w", err)
		}
		out = append(out, jc)
	}
	return out, rows.Err()
}

// JumpCloneImplant is one implant stored in a jump clone.
type JumpCloneImplant struct {
	JumpCloneID int64
	ItemID      int64
	TypeID      int64
}

// GetJumpCloneImplants returns the implants stored in all of charID's jump
// clones (entity rows whose locationID is a clone ID).
func GetJumpCloneImplants(ctx context.Context, db *sql.DB, charID int64) ([]JumpCloneImplant, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT e.locationID, e.itemID, e.typeID
		FROM entity e
		JOIN chrJumpClones jc ON jc.jumpCloneID = e.locationID
		WHERE jc.characterID = ? AND e.flag = ?`, charID, flagImplant)
	if err != nil {
		return nil, fmt.Errorf("GetJumpCloneImplants: %w", err)
	}
	defer rows.Close()
	var out []JumpCloneImplant
	for rows.Next() {
		var im JumpCloneImplant
		if err := rows.Scan(&im.JumpCloneID, &im.ItemID, &im.TypeID); err != nil {
			return nil, fmt.Errorf("GetJumpCloneImplants scan: %w", err)
		}
		out = append(out, im)
	}
	return out, rows.Err()
}

// GetCloneAtStation returns the jumpCloneID of charID's clone at stationID and
// how many implants it holds; 0 when no clone is there.
func GetCloneAtStation(ctx context.Context, db *sql.DB, charID, stationID int64) (int64, int, error) {
	var cloneID int64
	err := db.QueryRowContext(ctx,
		`SELECT jumpCloneID FROM chrJumpClones WHERE characterID = ? AND locationID = ? LIMIT 1`,
		charID, stationID).Scan(&cloneID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("GetCloneAtStation: %w", err)
	}
	var nImplants int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entity WHERE locationID = ? AND flag = ?`,
		cloneID, flagImplant).Scan(&nImplants); err != nil {
		return cloneID, 0, fmt.Errorf("GetCloneAtStation implant count: %w", err)
	}
	return cloneID, nImplants, nil
}

// jumpCloneInfoErr is a refusal the catalogue has no key for.
func jumpCloneInfoErr(text string) error {
	return MsgErr("CustomInfo", map[string]marshal.PyRep{"info": marshal.PyString{Value: text}}, text)
}

// jumpCloneStandingOK is the install standing rule: the owner's own members
// always pass, a zero minimum is no gate, otherwise standing must reach it.
func jumpCloneStandingOK(ownerID, pilotCorpID int64, standing, minStanding float64) bool {
	if minStanding <= 0 || ownerID == pilotCorpID {
		return true
	}
	return standing >= minStanding
}

// jumpCloneOwnerStanding is the station owner's standing toward the pilot as the
// client displays it: an NPC corporation's chrNPCStandings row with the
// social-skill bonus folded in, a player corporation's crpStandings.
func jumpCloneOwnerStanding(ctx context.Context, db *sql.DB, charID, pilotCorpID, ownerID int64) float64 {
	if npc, _ := IsNPCCorp(ctx, db, ownerID); npc {
		corp := GetNPCStandingToChar(ctx, db, charID, ownerID)
		factionID, _ := GetAgentCorpFactionID(ctx, db, ownerID)
		fac := GetNPCStandingToChar(ctx, db, charID, factionID)
		return ApplyStandingBonus(corp, StandingSkillBonus(ctx, db, charID, corp, fac, factionID))
	}
	allianceID, _ := GetCorpAllianceID(ctx, db, pilotCorpID)
	return OwnerStandingToward(ctx, db, ownerID, charID, pilotCorpID, allianceID)
}

// JumpCloneInstallRefusal checks every rule an install at stationID must meet
// and returns the station's bits for the fee. InstallJumpClone re-checks the
// clone count inside its transaction.
func JumpCloneInstallRefusal(ctx context.Context, db *sql.DB, charID, stationID int64, set JumpCloneSettings) (*StationItemBits, error) {
	bits, err := GetStationItemBits(ctx, db, stationID)
	if err != nil {
		return nil, err
	}
	if bits.ServiceMask&stationMedicalServices == 0 {
		return nil, jumpCloneInfoErr("This station has no medical facilities to install a jump clone in.")
	}
	if existing, _, err := GetCloneAtStation(ctx, db, charID, stationID); err != nil {
		return nil, err
	} else if existing != 0 {
		return nil, MsgErr("JumpAlreadyHaveStationClone", nil,
			"You already have a jump clone installed at this station.")
	}
	if err := jumpCloneCountRefusal(ctx, db, db, charID); err != nil {
		return nil, err
	}
	if reason := StationServiceRefusal(ctx, db, stationID, charID, StationServiceJumpCloneFacility); reason != "" {
		return nil, jumpCloneInfoErr(reason)
	}
	pilotCorpID, _ := GetCharCorpID(ctx, db, charID)
	if set.MinStanding > 0 && bits.OwnerID != pilotCorpID {
		standing := jumpCloneOwnerStanding(ctx, db, charID, pilotCorpID, bits.OwnerID)
		if !jumpCloneStandingOK(bits.OwnerID, pilotCorpID, standing, set.MinStanding) {
			owner := GetCorporationName(ctx, db, bits.OwnerID)
			if owner == "" {
				owner = "the station's owner"
			}
			return nil, jumpCloneInfoErr(fmt.Sprintf(
				"%s only installs jump clones for capsuleers it holds in high regard. You need a standing of %.1f with %s; yours is %.2f.",
				owner, set.MinStanding, owner, standing))
		}
	}
	return bits, nil
}

// jumpCloneCountRefusal enforces the Infomorph Psychology cap: one installed
// clone per trained level. q may be a transaction holding the character row.
func jumpCloneCountRefusal(ctx context.Context, skills *sql.DB, q DBTX, charID int64) error {
	level, _ := GetCharacterSkillLevel(ctx, skills, charID, InfomorphPsychologyTypeID)
	var count int
	if err := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM chrJumpClones WHERE characterID = ?`, charID).Scan(&count); err != nil {
		return fmt.Errorf("jump clone count: %w", err)
	}
	if count >= level {
		return MsgErr("JumpCharStoringMaxClones", map[string]marshal.PyRep{
			"have": marshal.PyInt{Value: uint64(count)},
			"max":  marshal.PyReal{Value: float64(level)},
		}, fmt.Sprintf("You cannot install any more clones: you have %d and Infomorph Psychology allows %d.", count, level))
	}
	return nil
}

// groupISK renders a whole-ISK amount with thousands separators for the
// client's NotEnoughMoney text, which prints the string as given.
func groupISK(v float64) string {
	s := strconv.FormatInt(int64(math.Round(v)), 10)
	neg := len(s) > 0 && s[0] == '-'
	if neg {
		s = s[1:]
	}
	out := make([]byte, 0, len(s)+len(s)/3)
	for i := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, s[i])
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

// InstallJumpClone installs an implantless clone for charID at stationID. After
// JumpCloneInstallRefusal, one transaction holding the character row re-checks
// the cap, pays the fee to the station's owner (refType 55 in both journals) and
// writes the clone. Returns the clone, the balance after, and the fee.
func InstallJumpClone(ctx context.Context, db *sql.DB, charID, stationID int64) (cloneID int64, balance, fee float64, err error) {
	set := GetJumpCloneSettings(ctx, db)
	bits, err := JumpCloneInstallRefusal(ctx, db, charID, stationID, set)
	if err != nil {
		return 0, 0, 0, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("InstallJumpClone begin: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	// The character row serialises racing installs (a double click) so neither
	// passes the cap the other is about to fill.
	if err = tx.QueryRowContext(ctx,
		`SELECT balance FROM character_ WHERE characterID = ? FOR UPDATE`, charID).Scan(&balance); err != nil {
		return 0, 0, 0, fmt.Errorf("InstallJumpClone lock: %w", err)
	}
	var here int
	if err = tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM chrJumpClones WHERE characterID = ? AND locationID = ?`,
		charID, stationID).Scan(&here); err != nil {
		return 0, 0, 0, fmt.Errorf("InstallJumpClone recheck: %w", err)
	}
	if here > 0 {
		err = MsgErr("JumpAlreadyHaveStationClone", nil, "You already have a jump clone installed at this station.")
		return 0, 0, 0, err
	}
	if err = jumpCloneCountRefusal(ctx, db, tx, charID); err != nil {
		return 0, 0, 0, err
	}
	fee = StationServiceFee(ctx, db, stationID, charID, StationServiceJumpCloneFacility, set.InstallCost)
	if fee > 0 {
		reason := "Jump clone installation at " + GetStationName(ctx, db, stationID)
		if balance, _, err = transferISKTx(ctx, tx, charID, bits.OwnerID, fee, refJumpCloneInstallationFee, reason); err != nil {
			if errors.Is(err, ErrInsufficientFunds) {
				var have float64
				_ = tx.QueryRowContext(ctx, `SELECT balance FROM character_ WHERE characterID = ?`, charID).Scan(&have)
				err = MsgErr("NotEnoughMoney", map[string]marshal.PyRep{
					"amount":  marshal.PyString{Value: groupISK(fee)},
					"balance": marshal.PyString{Value: groupISK(have)},
				}, fmt.Sprintf("Installing a jump clone costs %s ISK; you have %s.", groupISK(fee), groupISK(have)))
			}
			return 0, 0, 0, err
		}
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO chrJumpClones (characterID, locationID, createDate) VALUES (?, ?, ?)`,
		charID, stationID, win32TimeNow())
	if err != nil {
		return 0, 0, 0, fmt.Errorf("InstallJumpClone insert: %w", err)
	}
	if cloneID, err = res.LastInsertId(); err != nil {
		return 0, 0, 0, fmt.Errorf("InstallJumpClone id: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return 0, 0, 0, fmt.Errorf("InstallJumpClone commit: %w", err)
	}
	return cloneID, balance, fee, nil
}

// DestroyJumpClone removes charID's clone cloneID with any implants stored in
// it. The client offers Destroy only for the clone at the docked station, and
// so does the server (JumpCantDestroyNonLocalClone); stationID 0 skips that.
func DestroyJumpClone(ctx context.Context, db *sql.DB, charID, cloneID, stationID int64) error {
	var owner, location int64
	err := db.QueryRowContext(ctx,
		`SELECT characterID, locationID FROM chrJumpClones WHERE jumpCloneID = ?`, cloneID).Scan(&owner, &location)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && owner != charID) {
		// Another pilot's clone reads as no clone at all.
		return MsgErr("JumpCloneNotFound", nil, "That jump clone does not exist.")
	}
	if err != nil {
		return fmt.Errorf("DestroyJumpClone lookup: %w", err)
	}
	if stationID != 0 && location != stationID {
		where := GetStationName(ctx, db, location)
		return MsgErr("JumpCantDestroyNonLocalClone", map[string]marshal.PyRep{
			"where": marshal.PyString{Value: where},
		}, "That jump clone is not at this station but in "+where+".")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("DestroyJumpClone begin: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if err = destroyJumpCloneTx(ctx, tx, cloneID); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("DestroyJumpClone commit: %w", err)
	}
	return nil
}

func destroyJumpCloneTx(ctx context.Context, tx *sql.Tx, cloneID int64) error {
	if _, err := tx.ExecContext(ctx, `
		DELETE ea FROM entity_attributes ea
		JOIN entity e ON e.itemID = ea.itemID
		WHERE e.locationID = ? AND e.flag = ?`, cloneID, flagImplant); err != nil {
		return fmt.Errorf("DestroyJumpClone implant attrs: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM entity WHERE locationID = ? AND flag = ?`, cloneID, flagImplant); err != nil {
		return fmt.Errorf("DestroyJumpClone implants: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM chrJumpClones WHERE jumpCloneID = ?`, cloneID); err != nil {
		return fmt.Errorf("DestroyJumpClone: %w", err)
	}
	return nil
}

// GetLastCloneJump returns the Win32 FILETIME of charID's last clone jump
// (0 = never jumped).
func GetLastCloneJump(ctx context.Context, db *sql.DB, charID int64) (uint64, error) {
	var t uint64
	err := db.QueryRowContext(ctx,
		`SELECT lastJumpDate FROM chrCloneJumpTime WHERE characterID = ?`, charID).Scan(&t)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("GetLastCloneJump: %w", err)
	}
	return t, nil
}

// cloneJumpRemaining is how long a pilot who last jumped at last (FILETIME, 0 =
// never) must still wait at now with a cooldown of hours.
func cloneJumpRemaining(last, now uint64, hours float64) time.Duration {
	if last == 0 || hours <= 0 {
		return 0
	}
	ready := last + uint64(hours*3600*1e7)
	if now >= ready {
		return 0
	}
	return time.Duration(ready-now) * 100
}

// CloneJumpCooldownLeft is the wait before charID may clone jump again.
func CloneJumpCooldownLeft(ctx context.Context, db *sql.DB, charID int64, hours float64) (time.Duration, error) {
	last, err := GetLastCloneJump(ctx, db, charID)
	if err != nil {
		return 0, err
	}
	return cloneJumpRemaining(last, win32TimeNow(), hours), nil
}

// SplitCooldown breaks a wait into the hours and minutes JumpAttemptTooSoon
// prints, rounding minutes up so a pending wait never reads 0 and 0.
func SplitCooldown(d time.Duration) (hours, minutes int) {
	if d <= 0 {
		return 0, 0
	}
	total := int((d + time.Minute - 1) / time.Minute)
	return total / 60, total % 60
}

// CloneJumpSwap performs the body swap for a jump from fromStationID to
// destCloneID in one transaction: a new clone at the departure station takes the
// plugged-in implants, the destination clone's implants become active, that
// clone row is consumed and the jump timer is stamped. Returns the parked and
// woken implant IDs. The caller has validated the destination and timer.
func CloneJumpSwap(ctx context.Context, db *sql.DB, charID, fromStationID, destCloneID int64) (newCloneID int64, parked, woken []int64, err error) {
	// The active implant set changes hands: drop the folds built on it.
	defer InvalidateImplants(charID)
	defer InvalidateGangSource(charID)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("CloneJumpSwap begin: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if parked, err = implantIDsAt(ctx, tx, charID); err != nil {
		return 0, nil, nil, err
	}
	if woken, err = implantIDsAt(ctx, tx, destCloneID); err != nil {
		return 0, nil, nil, err
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO chrJumpClones (characterID, locationID, createDate) VALUES (?, ?, ?)`,
		charID, fromStationID, win32TimeNow())
	if err != nil {
		return 0, nil, nil, fmt.Errorf("CloneJumpSwap new clone: %w", err)
	}
	if newCloneID, err = res.LastInsertId(); err != nil {
		return 0, nil, nil, fmt.Errorf("CloneJumpSwap new clone id: %w", err)
	}
	// Park the active implants in the body left behind.
	if _, err = tx.ExecContext(ctx,
		`UPDATE entity SET locationID = ? WHERE locationID = ? AND flag = ?`,
		newCloneID, charID, flagImplant); err != nil {
		return 0, nil, nil, fmt.Errorf("CloneJumpSwap park implants: %w", err)
	}
	// Wake the destination clone's implants.
	if _, err = tx.ExecContext(ctx,
		`UPDATE entity SET locationID = ? WHERE locationID = ? AND flag = ?`,
		charID, destCloneID, flagImplant); err != nil {
		return 0, nil, nil, fmt.Errorf("CloneJumpSwap activate implants: %w", err)
	}
	if _, err = tx.ExecContext(ctx,
		`DELETE FROM chrJumpClones WHERE jumpCloneID = ?`, destCloneID); err != nil {
		return 0, nil, nil, fmt.Errorf("CloneJumpSwap consume clone: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `
		INSERT INTO chrCloneJumpTime (characterID, lastJumpDate) VALUES (?, ?)
		ON DUPLICATE KEY UPDATE lastJumpDate = VALUES(lastJumpDate)`,
		charID, win32TimeNow()); err != nil {
		return 0, nil, nil, fmt.Errorf("CloneJumpSwap stamp timer: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return 0, nil, nil, fmt.Errorf("CloneJumpSwap commit: %w", err)
	}
	return newCloneID, parked, woken, nil
}

func implantIDsAt(ctx context.Context, tx *sql.Tx, locationID int64) ([]int64, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT itemID FROM entity WHERE locationID = ? AND flag = ? FOR UPDATE`, locationID, flagImplant)
	if err != nil {
		return nil, fmt.Errorf("CloneJumpSwap implants at %d: %w", locationID, err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
