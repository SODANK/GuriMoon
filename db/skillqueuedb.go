package db

// Skill queue: an ordered list of (typeID, targetLevel) entries per character, edited through ESP
// and the in-game browser (the client has no queue UI). AdvanceSkillQueue starts each next entry
// backdated to the previous skill's end. See docs/character-skills.md#skill-training.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"
)

// EnsureSkillQueueTable creates chrSkillQueue if missing. Called once at startup.
func EnsureSkillQueueTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS chrSkillQueue (
			characterID   INT UNSIGNED NOT NULL,
			queuePosition INT UNSIGNED NOT NULL,
			typeID        INT UNSIGNED NOT NULL,
			targetLevel   TINYINT UNSIGNED NOT NULL,
			PRIMARY KEY (characterID, queuePosition)
		) ENGINE=InnoDB`)
	return err
}

// SkillQueueEntry is one queued training step for a character.
type SkillQueueEntry struct {
	Position    int
	TypeID      int64
	TypeName    string
	TargetLevel int
	EstEnd      uint64 // estimated completion (Win32 FILETIME); 0 = not computed
}

// FiletimeToTime converts a Win32 FILETIME tick count to a time.Time.
func FiletimeToTime(t uint64) time.Time {
	return time.Unix(int64(t/10_000_000)-11644473600, 0)
}

// GetSkillQueue returns the character's queued entries in order, with type
// names resolved.  ETAs are not filled in; use EstimateSkillQueueETAs.
func GetSkillQueue(ctx context.Context, db *sql.DB, charID int64) ([]SkillQueueEntry, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT q.queuePosition, q.typeID, COALESCE(t.typeName, CONCAT('type ', q.typeID)), q.targetLevel
		FROM chrSkillQueue q
		LEFT JOIN invTypes t ON t.typeID = q.typeID
		WHERE q.characterID = ?
		ORDER BY q.queuePosition`, charID)
	if err != nil {
		return nil, fmt.Errorf("GetSkillQueue: %w", err)
	}
	defer rows.Close()
	var out []SkillQueueEntry
	for rows.Next() {
		var e SkillQueueEntry
		if err := rows.Scan(&e.Position, &e.TypeID, &e.TypeName, &e.TargetLevel); err != nil {
			return nil, fmt.Errorf("GetSkillQueue scan: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// SkillTrainingInfo is the active training entry with its type name resolved,
// for display on ESP pages.
type SkillTrainingInfo struct {
	SkillTraining
	TypeName string
}

// GetSkillTrainingInfo returns the active training entry for charID with the
// skill's type name, or nil when nothing is training.
func GetSkillTrainingInfo(ctx context.Context, db *sql.DB, charID int64) (*SkillTrainingInfo, error) {
	st, err := GetSkillTraining(ctx, db, charID)
	if err != nil || st == nil {
		return nil, err
	}
	info := &SkillTrainingInfo{SkillTraining: *st}
	if err := db.QueryRowContext(ctx,
		`SELECT typeName FROM invTypes WHERE typeID = ?`, st.TypeID,
	).Scan(&info.TypeName); err != nil {
		info.TypeName = fmt.Sprintf("type %d", st.TypeID)
	}
	return info, nil
}

// skillLevelAndSP returns the character's current level and SP for a skill
// type, plus the skill entity itemID (0 if the character does not own it).
func skillLevelAndSP(ctx context.Context, db *sql.DB, charID, typeID int64) (itemID int64, level int, sp float64, err error) {
	var lv, spv sql.NullInt64
	err = db.QueryRowContext(ctx, `
		SELECT e.itemID, lv.valueInt, sp.valueInt
		FROM entity e
		LEFT JOIN entity_attributes lv ON lv.itemID = e.itemID AND lv.attributeID = 280
		LEFT JOIN entity_attributes sp ON sp.itemID = e.itemID AND sp.attributeID = 276
		WHERE e.ownerID = ? AND e.typeID = ? AND e.flag IN (7, 61)
		LIMIT 1`, charID, typeID,
	).Scan(&itemID, &lv, &spv)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, 0, nil
	}
	if err != nil {
		return 0, 0, 0, fmt.Errorf("skillLevelAndSP: %w", err)
	}
	return itemID, int(lv.Int64), float64(spv.Int64), nil
}

// AddSkillToQueue appends a step for typeID to charID's queue and returns its target level: one
// above the highest of the trained level, the active training's target and any queued target for
// the type.
func AddSkillToQueue(ctx context.Context, db *sql.DB, charID, typeID int64) (int, error) {
	itemID, level, _, err := skillLevelAndSP(ctx, db, charID, typeID)
	if err != nil {
		return 0, err
	}
	if itemID == 0 {
		return 0, fmt.Errorf("character does not own that skill")
	}
	floor := level

	// Active training for this same type raises the floor to its target.
	st, err := GetSkillTraining(ctx, db, charID)
	if err != nil {
		return 0, err
	}
	if st != nil && st.TypeID == typeID && st.TargetLevel > floor {
		floor = st.TargetLevel
	}

	// Existing queue entries for this type raise it further.
	var maxQueued sql.NullInt64
	if err := db.QueryRowContext(ctx,
		`SELECT MAX(targetLevel) FROM chrSkillQueue WHERE characterID = ? AND typeID = ?`,
		charID, typeID).Scan(&maxQueued); err != nil {
		return 0, fmt.Errorf("AddSkillToQueue max queued: %w", err)
	}
	if maxQueued.Valid && int(maxQueued.Int64) > floor {
		floor = int(maxQueued.Int64)
	}

	target := floor + 1
	if target > 5 {
		return 0, fmt.Errorf("skill is already trained or queued to level 5")
	}

	// Prerequisites are judged against projected levels (trained, training, queued), so a skill may
	// queue behind its own prerequisite but an unreachable one is refused.
	proj, err := projectedSkillLevels(ctx, db, charID)
	if err != nil {
		return 0, err
	}
	if err := CheckSkillPrereqs(ctx, db, charID, typeID, proj); err != nil {
		return 0, err
	}

	var maxPos sql.NullInt64
	if err := db.QueryRowContext(ctx,
		`SELECT MAX(queuePosition) FROM chrSkillQueue WHERE characterID = ?`,
		charID).Scan(&maxPos); err != nil {
		return 0, fmt.Errorf("AddSkillToQueue max pos: %w", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO chrSkillQueue (characterID, queuePosition, typeID, targetLevel)
		VALUES (?, ?, ?, ?)`,
		charID, maxPos.Int64+1, typeID, target); err != nil {
		return 0, fmt.Errorf("AddSkillToQueue insert: %w", err)
	}
	return target, nil
}

// RemoveSkillQueueEntry deletes the entry at position and closes the gap.
// Later entries for the same skill type whose target level chain is broken by
// the removal are dropped too (can't train level N+1 before N).
func RemoveSkillQueueEntry(ctx context.Context, db *sql.DB, charID int64, position int) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("RemoveSkillQueueEntry begin: %w", err)
	}
	defer tx.Rollback()

	var typeID int64
	var target int
	err = tx.QueryRowContext(ctx,
		`SELECT typeID, targetLevel FROM chrSkillQueue WHERE characterID = ? AND queuePosition = ?`,
		charID, position).Scan(&typeID, &target)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("RemoveSkillQueueEntry lookup: %w", err)
	}
	// Drop this entry and any higher-level entries of the same type (their
	// prerequisite level just disappeared).
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM chrSkillQueue
		WHERE characterID = ? AND typeID = ? AND targetLevel >= ?`,
		charID, typeID, target); err != nil {
		return fmt.Errorf("RemoveSkillQueueEntry delete: %w", err)
	}
	if err := compactSkillQueue(ctx, tx, charID); err != nil {
		return err
	}
	return tx.Commit()
}

// popSkillQueueEntry deletes exactly the entry at position and closes the gap. Unlike
// RemoveSkillQueueEntry it does not cascade: AdvanceSkillQueue consumes entries one at a time and
// re-judges later levels when they reach the head.
func popSkillQueueEntry(ctx context.Context, db *sql.DB, charID int64, position int) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("popSkillQueueEntry begin: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM chrSkillQueue WHERE characterID = ? AND queuePosition = ?`,
		charID, position); err != nil {
		return fmt.Errorf("popSkillQueueEntry delete: %w", err)
	}
	if err := compactSkillQueue(ctx, tx, charID); err != nil {
		return err
	}
	return tx.Commit()
}

// compactSkillQueue renumbers a character's queue positions to 1..N,
// preserving order.  Runs inside the caller's transaction.
func compactSkillQueue(ctx context.Context, tx *sql.Tx, charID int64) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT queuePosition FROM chrSkillQueue WHERE characterID = ? ORDER BY queuePosition`, charID)
	if err != nil {
		return fmt.Errorf("compactSkillQueue: %w", err)
	}
	var positions []int
	for rows.Next() {
		var p int
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return fmt.Errorf("compactSkillQueue scan: %w", err)
		}
		positions = append(positions, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for i, p := range positions {
		if p == i+1 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE chrSkillQueue SET queuePosition = ?
			WHERE characterID = ? AND queuePosition = ?`,
			i+1, charID, p); err != nil {
			return fmt.Errorf("compactSkillQueue renumber: %w", err)
		}
	}
	return nil
}

// projectedSkillLevels returns the level every skill will hold once the active
// training and the whole queue have run: the trained level, raised by the
// active training's target and by the highest queued target for that type.
func projectedSkillLevels(ctx context.Context, db *sql.DB, charID int64) (map[int64]int, error) {
	levels, err := GetCharacterSkillLevels(ctx, db, charID)
	if err != nil {
		return nil, err
	}
	st, err := GetSkillTraining(ctx, db, charID)
	if err != nil {
		return nil, err
	}
	if st != nil && st.TargetLevel > levels[st.TypeID] {
		levels[st.TypeID] = st.TargetLevel
	}
	rows, err := db.QueryContext(ctx,
		`SELECT typeID, MAX(targetLevel) FROM chrSkillQueue WHERE characterID = ? GROUP BY typeID`, charID)
	if err != nil {
		return nil, fmt.Errorf("projectedSkillLevels: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var typeID int64
		var lvl int
		if err := rows.Scan(&typeID, &lvl); err != nil {
			return nil, fmt.Errorf("projectedSkillLevels scan: %w", err)
		}
		if lvl > levels[typeID] {
			levels[typeID] = lvl
		}
	}
	return levels, rows.Err()
}

// ProjectedSkillLevels is projectedSkillLevels for callers outside the package
// (the ESP min-max planner starts from the queue's end state).
func ProjectedSkillLevels(ctx context.Context, db *sql.DB, charID int64) (map[int64]int, error) {
	return projectedSkillLevels(ctx, db, charID)
}

// skillQueueDeps loads the required-skill prerequisites of every skill type
// present in entries.
func skillQueueDeps(ctx context.Context, db *sql.DB, entries []SkillQueueEntry) (map[int64][]SkillReq, error) {
	deps := make(map[int64][]SkillReq)
	for _, e := range entries {
		if _, seen := deps[e.TypeID]; seen {
			continue
		}
		reqs, err := GetRequiredSkills(ctx, db, e.TypeID)
		if err != nil {
			return nil, err
		}
		deps[e.TypeID] = reqs
	}
	return deps, nil
}

// normalizeSkillQueue reorders entries so nothing trains before what it depends on, keeping the
// requested order otherwise. Each entry first pulls in front of itself its still-queued lower
// levels and any queued prerequisite the character has not trained. trained holds the levels
// already in the brain plus the active training's target.
func normalizeSkillQueue(entries []SkillQueueEntry, deps map[int64][]SkillReq, trained map[int64]int) []SkillQueueEntry {
	byType := make(map[int64][]int, len(entries))
	for i, e := range entries {
		byType[e.TypeID] = append(byType[e.TypeID], i)
	}
	for t := range byType {
		idxs := byType[t]
		sort.SliceStable(idxs, func(a, b int) bool {
			return entries[idxs[a]].TargetLevel < entries[idxs[b]].TargetLevel
		})
	}

	emitted := make([]bool, len(entries))
	visiting := make([]bool, len(entries))
	out := make([]SkillQueueEntry, 0, len(entries))

	var emit func(i int)
	emit = func(i int) {
		if emitted[i] || visiting[i] { // visiting: broken data with a dependency cycle
			return
		}
		visiting[i] = true
		e := entries[i]
		// Lower levels of the same skill.
		for _, j := range byType[e.TypeID] {
			if entries[j].TargetLevel < e.TargetLevel {
				emit(j)
			}
		}
		// Queued prerequisites that are not trained yet.
		for _, r := range deps[e.TypeID] {
			if trained[r.SkillID] >= r.Level {
				continue
			}
			for _, j := range byType[r.SkillID] {
				if entries[j].TargetLevel <= r.Level {
					emit(j)
				}
			}
		}
		visiting[i] = false
		if emitted[i] {
			return
		}
		emitted[i] = true
		out = append(out, e)
	}
	for i := range entries {
		emit(i)
	}
	return out
}

// writeSkillQueue replaces charID's queue rows with entries, numbered 1..N.
func writeSkillQueue(ctx context.Context, tx *sql.Tx, charID int64, entries []SkillQueueEntry) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM chrSkillQueue WHERE characterID = ?`, charID); err != nil {
		return fmt.Errorf("writeSkillQueue clear: %w", err)
	}
	for i, e := range entries {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO chrSkillQueue (characterID, queuePosition, typeID, targetLevel)
			VALUES (?, ?, ?, ?)`, charID, i+1, e.TypeID, e.TargetLevel); err != nil {
			return fmt.Errorf("writeSkillQueue insert: %w", err)
		}
	}
	return nil
}

// storeSkillQueueOrder normalizes entries and writes them back as the whole
// queue in one transaction.
func storeSkillQueueOrder(ctx context.Context, db *sql.DB, charID int64, entries []SkillQueueEntry) error {
	deps, err := skillQueueDeps(ctx, db, entries)
	if err != nil {
		return err
	}
	trained, err := GetCharacterSkillLevels(ctx, db, charID)
	if err != nil {
		return err
	}
	if st, err := GetSkillTraining(ctx, db, charID); err == nil && st != nil && st.TargetLevel > trained[st.TypeID] {
		trained[st.TypeID] = st.TargetLevel
	}
	ordered := normalizeSkillQueue(entries, deps, trained)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("storeSkillQueueOrder begin: %w", err)
	}
	defer tx.Rollback()
	if err := writeSkillQueue(ctx, tx, charID, ordered); err != nil {
		return err
	}
	return tx.Commit()
}

// skillChainBlock returns the half-open range [start, end) of the entry at idx and the contiguous
// same-skill level chain around it. A move shifts the whole chain so levels stay in order.
func skillChainBlock(entries []SkillQueueEntry, idx int) (int, int) {
	start, end := idx, idx
	for start > 0 && entries[start-1].TypeID == entries[start].TypeID &&
		entries[start-1].TargetLevel < entries[start].TargetLevel {
		start--
	}
	for end < len(entries)-1 && entries[end+1].TypeID == entries[end].TypeID &&
		entries[end+1].TargetLevel > entries[end].TargetLevel {
		end++
	}
	return start, end + 1
}

// MoveSkillQueueEntry moves the entry at position one slot up or down together with its
// contiguous same-skill level chain, then normalizes the queue. Out-of-range moves are no-ops.
func MoveSkillQueueEntry(ctx context.Context, db *sql.DB, charID int64, position int, up bool) error {
	entries, err := GetSkillQueue(ctx, db, charID)
	if err != nil {
		return err
	}
	idx := -1
	for i, e := range entries {
		if e.Position == position {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil
	}
	start, end := skillChainBlock(entries, idx)
	block := append([]SkillQueueEntry(nil), entries[start:end]...)
	rest := append(append([]SkillQueueEntry(nil), entries[:start]...), entries[end:]...)

	insertAt := start
	if up {
		if start == 0 {
			return nil
		}
		insertAt = start - 1
	} else {
		if end >= len(entries) {
			return nil
		}
		insertAt = start + 1
	}
	moved := make([]SkillQueueEntry, 0, len(entries))
	moved = append(moved, rest[:insertAt]...)
	moved = append(moved, block...)
	moved = append(moved, rest[insertAt:]...)
	return storeSkillQueueOrder(ctx, db, charID, moved)
}

// ReorderSkillQueue rewrites the queue in the sequence order lists, which must be a permutation
// of the current queuePositions (drag-and-drop UI). The result is normalized, so a drag ahead of
// a prerequisite pulls it along.
func ReorderSkillQueue(ctx context.Context, db *sql.DB, charID int64, order []int) error {
	entries, err := GetSkillQueue(ctx, db, charID)
	if err != nil {
		return err
	}
	byPos := make(map[int]SkillQueueEntry, len(entries))
	for _, e := range entries {
		byPos[e.Position] = e
	}
	if len(order) != len(entries) {
		return fmt.Errorf("reorder list has %d entries, queue has %d", len(order), len(entries))
	}
	seen := make(map[int]bool, len(order))
	reordered := make([]SkillQueueEntry, 0, len(order))
	for _, p := range order {
		e, ok := byPos[p]
		if !ok || seen[p] {
			return fmt.Errorf("reorder list is not a permutation of the queue")
		}
		seen[p] = true
		reordered = append(reordered, e)
	}
	return storeSkillQueueOrder(ctx, db, charID, reordered)
}

// NormalizeSkillQueue re-sorts an existing queue into a trainable order and returns how many
// entries changed position.
func NormalizeSkillQueue(ctx context.Context, db *sql.DB, charID int64) (int, error) {
	entries, err := GetSkillQueue(ctx, db, charID)
	if err != nil || len(entries) == 0 {
		return 0, err
	}
	before := make([]int64, len(entries))
	for i, e := range entries {
		before[i] = e.TypeID<<3 | int64(e.TargetLevel)
	}
	if err := storeSkillQueueOrder(ctx, db, charID, entries); err != nil {
		return 0, err
	}
	after, err := GetSkillQueue(ctx, db, charID)
	if err != nil {
		return 0, err
	}
	moved := 0
	for i := range after {
		if i >= len(before) || before[i] != after[i].TypeID<<3|int64(after[i].TargetLevel) {
			moved++
		}
	}
	return moved, nil
}

// ClearSkillQueue removes every queued entry for charID.
func ClearSkillQueue(ctx context.Context, db *sql.DB, charID int64) error {
	_, err := db.ExecContext(ctx,
		`DELETE FROM chrSkillQueue WHERE characterID = ?`, charID)
	return err
}

// SkillQueueAdvance reports what AdvanceSkillQueue did: the skills completed (several after
// downtime) and the training it left running, if it started one.
type SkillQueueAdvance struct {
	Completed     []int64 // skill entity itemIDs whose level was advanced
	StartedItemID int64   // newly-started training (0 = nothing new started)
	StartedEnd    uint64  // its endOfTraining tick
}

// DueSkillQueueCharacters returns characters whose queue needs advancing: a training past
// endOfTraining, or entries queued with nothing training. The caller feeds each ID to
// AdvanceSkillQueue, which re-checks under its lock.
func DueSkillQueueCharacters(ctx context.Context, db *sql.DB) ([]int64, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT characterID FROM chrSkillTraining WHERE endOfTraining <= ?
		UNION
		SELECT q.characterID FROM chrSkillQueue q
		LEFT JOIN chrSkillTraining t ON t.characterID = q.characterID
		WHERE t.characterID IS NULL`, win32TimeNow())
	if err != nil {
		return nil, fmt.Errorf("DueSkillQueueCharacters: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("DueSkillQueueCharacters scan: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// skillAdvanceLocks serialize AdvanceSkillQueue per character (sharded by charID); concurrent
// callers would advance one due training twice. See docs/character-skills.md#advance-lock.
var skillAdvanceLocks [64]sync.Mutex

// AdvanceSkillQueue completes any due training and chains through the queue: each finished
// skill's endOfTraining is the backdated start of the next entry, until a training is still in
// the future or the queue empties. Untrainable entries are dropped. It does nothing while a
// training is active and not due.
func AdvanceSkillQueue(ctx context.Context, db *sql.DB, charID int64) (*SkillQueueAdvance, error) {
	lock := &skillAdvanceLocks[uint64(charID)%uint64(len(skillAdvanceLocks))]
	lock.Lock()
	defer lock.Unlock()

	adv := &SkillQueueAdvance{}
	now := win32TimeNow()
	baseline := now

	for range 200 { // loop cap
		st, err := GetSkillTraining(ctx, db, charID)
		if err != nil {
			return adv, err
		}
		if st != nil {
			if st.EndOfTraining > now {
				return adv, nil // still training
			}
			baseline = st.EndOfTraining
			itemID, err := CompleteTrainingIfDue(ctx, db, charID)
			if err != nil {
				return adv, err
			}
			if itemID != 0 {
				adv.Completed = append(adv.Completed, itemID)
			}
			continue // re-check: training slot is now free
		}

		// Idle: pop the queue head.
		var pos, target int
		var typeID int64
		err = db.QueryRowContext(ctx, `
			SELECT queuePosition, typeID, targetLevel FROM chrSkillQueue
			WHERE characterID = ? ORDER BY queuePosition LIMIT 1`,
			charID).Scan(&pos, &typeID, &target)
		if errors.Is(err, sql.ErrNoRows) {
			return adv, nil // queue empty
		}
		if err != nil {
			return adv, fmt.Errorf("AdvanceSkillQueue head: %w", err)
		}

		itemID, level, _, err := skillLevelAndSP(ctx, db, charID, typeID)
		if err != nil {
			return adv, err
		}
		if itemID == 0 || level+1 != target {
			// Unowned skill, level already reached or a level gap: drop the entry and continue.
			if rmErr := popSkillQueueEntry(ctx, db, charID, pos); rmErr != nil {
				return adv, rmErr
			}
			continue
		}
		// Prerequisites are re-judged at the head, against what is trained by now.
		if pErr := CheckSkillPrereqs(ctx, db, charID, typeID, nil); pErr != nil {
			if AsSkillPrereqError(pErr) == nil {
				return adv, pErr
			}
			log.Printf("AdvanceSkillQueue charID=%d: dropping %v", charID, pErr)
			if rmErr := popSkillQueueEntry(ctx, db, charID, pos); rmErr != nil {
				return adv, rmErr
			}
			continue
		}

		end, err := startTrainingSkillAt(ctx, db, charID, itemID, baseline)
		if err != nil {
			return adv, fmt.Errorf("AdvanceSkillQueue start typeID=%d: %w", typeID, err)
		}
		if rmErr := popSkillQueueEntry(ctx, db, charID, pos); rmErr != nil {
			return adv, rmErr
		}
		if end > now {
			adv.StartedItemID = itemID
			adv.StartedEnd = end
			return adv, nil
		}
		// That one ended in the past too: the loop completes it and continues.
	}
	return adv, nil
}

// ---------------------------------------------------------------------------
// Min-max planner

// MinMaxPlanStep is one (skill, level) training step of a min-max plan.
type MinMaxPlanStep struct {
	TypeID      int64
	TypeName    string
	TargetLevel int
	Minutes     float64 // estimated training time for this step alone
}

// PlanMinMaxSkillQueue builds a cheapest-step-first plan over every injected skill, which orders
// all level 1s before level 2s and so on. baseline overrides starting levels per typeID (nil =
// trained; pass projected levels to append). maxLevel clamps to 1..5, maxSteps 0 = unlimited,
// maxStepMinutes 0 = no cap. Skills whose prerequisites the plan never meets are skipped.
func PlanMinMaxSkillQueue(ctx context.Context, db *sql.DB, charID int64, baseline map[int64]int,
	maxLevel, maxSteps int, maxStepMinutes float64) ([]MinMaxPlanStep, error) {
	catalogue, err := ESPGetSkillCatalogue(ctx, db, charID)
	if err != nil {
		return nil, err
	}
	return PlanMinMaxSkillQueueFrom(ctx, db, charID, catalogue, baseline, maxLevel, maxSteps, maxStepMinutes)
}

// PlanMinMaxSkillQueueFrom is PlanMinMaxSkillQueue over a catalogue the caller already holds
// (the rolebots module caches the static half).
func PlanMinMaxSkillQueueFrom(ctx context.Context, db *sql.DB, charID int64, catalogue []ESPSkillCatalogueRow,
	baseline map[int64]int, maxLevel, maxSteps int, maxStepMinutes float64) ([]MinMaxPlanStep, error) {

	if maxLevel < 1 {
		maxLevel = 1
	}
	if maxLevel > 5 {
		maxLevel = 5
	}
	stats := ESPEffectiveLearningStats(ctx, db, charID)
	prereqs, err := GetSkillPrereqMap(ctx, db)
	if err != nil {
		return nil, err
	}

	// Injected skills only.
	type cand struct {
		row  ESPSkillCatalogueRow
		next int // level the next step of this skill would train
		reqs []SkillReq
		rate float64 // SP per minute for this skill
	}
	levels := make(map[int64]int) // projected level per skill during planning
	var cands []*cand
	for _, row := range catalogue {
		if !row.Known {
			continue
		}
		lvl := row.Level
		if b, ok := baseline[row.TypeID]; ok && b > lvl {
			lvl = b
		}
		levels[row.TypeID] = lvl
		if lvl >= maxLevel {
			continue
		}
		cands = append(cands, &cand{
			row:  row,
			next: lvl + 1,
			reqs: prereqs[row.TypeID],
			rate: ESPSkillSPPerMinute(stats, row.PrimaryAttrID, row.SecondaryAttrID),
		})
	}

	// Step time: SP from the skill's banked SP, or the previous level's mark once the plan has
	// promised that level, to the next.
	stepMinutes := func(c *cand) float64 {
		spStart := float64(c.row.SkillPoints)
		if prev := ESPSkillSPForLevel(c.row.Rank, c.next-1); prev > spStart {
			spStart = prev
		}
		spNeeded := ESPSkillSPForLevel(c.row.Rank, c.next) - spStart
		if spNeeded < 1 {
			spNeeded = 1
		}
		return spNeeded / c.rate
	}

	var plan []MinMaxPlanStep
	for maxSteps <= 0 || len(plan) < maxSteps {
		var best *cand
		var bestMin float64
		for _, c := range cands {
			if c.next > maxLevel {
				continue
			}
			if len(UnmetSkillReqs(c.reqs, levels, nil)) > 0 {
				continue // prerequisites unmet; may become eligible later
			}
			m := stepMinutes(c)
			if maxStepMinutes > 0 && m > maxStepMinutes {
				continue
			}
			if best == nil || m < bestMin ||
				(m == bestMin && c.row.TypeName < best.row.TypeName) {
				best, bestMin = c, m
			}
		}
		if best == nil {
			break
		}
		plan = append(plan, MinMaxPlanStep{
			TypeID:      best.row.TypeID,
			TypeName:    best.row.TypeName,
			TargetLevel: best.next,
			Minutes:     bestMin,
		})
		levels[best.row.TypeID] = best.next
		best.next++
	}
	return plan, nil
}

// AppendSkillQueuePlan appends plan steps to charID's queue, skipping any step
// whose level is already trained, training or queued, then normalizes the
// result.  Returns the number of entries added.
func AppendSkillQueuePlan(ctx context.Context, db *sql.DB, charID int64, plan []MinMaxPlanStep) (int, error) {
	existing, err := GetSkillQueue(ctx, db, charID)
	if err != nil {
		return 0, err
	}
	proj, err := projectedSkillLevels(ctx, db, charID)
	if err != nil {
		return 0, err
	}
	entries := append([]SkillQueueEntry(nil), existing...)
	added := 0
	for _, s := range plan {
		if s.TargetLevel <= proj[s.TypeID] {
			continue
		}
		entries = append(entries, SkillQueueEntry{
			TypeID: s.TypeID, TypeName: s.TypeName, TargetLevel: s.TargetLevel,
		})
		proj[s.TypeID] = s.TargetLevel
		added++
	}
	if added == 0 {
		return 0, nil
	}
	return added, storeSkillQueueOrder(ctx, db, charID, entries)
}

// EstimateSkillQueueETAs fills in EstEnd for each entry as a chain from the active training's end
// (or now when idle). It uses current attributes, so queued Learning gains are not modeled.
func EstimateSkillQueueETAs(ctx context.Context, db *sql.DB, charID int64, entries []SkillQueueEntry) error {
	baseline := win32TimeNow()
	st, err := GetSkillTraining(ctx, db, charID)
	if err != nil {
		return err
	}
	if st != nil && st.EndOfTraining > baseline {
		baseline = st.EndOfTraining
	}

	// The effective learning attributes are the same for every entry; resolve them once.
	stats := ESPEffectiveLearningStats(ctx, db, charID)

	simSP := make(map[int64]float64)               // typeID → SP after earlier queue entries
	attrCache := make(map[int64]map[int64]float64) // typeID → dgm type attributes
	for i := range entries {
		e := &entries[i]
		typeAttrs, cached := attrCache[e.TypeID]
		if !cached {
			var err error
			typeAttrs, err = GetTypeAttributes(ctx, db, e.TypeID)
			if err != nil {
				return fmt.Errorf("EstimateSkillQueueETAs attrs typeID=%d: %w", e.TypeID, err)
			}
			attrCache[e.TypeID] = typeAttrs
		}
		tc := int64(typeAttrs[275])
		if tc <= 0 {
			tc = 1
		}
		spRate := ESPSkillSPPerMinute(stats, int64(typeAttrs[180]), int64(typeAttrs[181]))

		spStart, seen := simSP[e.TypeID]
		if !seen {
			_, level, sp, err := skillLevelAndSP(ctx, db, charID, e.TypeID)
			if err != nil {
				return err
			}
			spStart = sp
			if prev := skillSPForLevel(tc, e.TargetLevel-1); level < e.TargetLevel-1 && prev > spStart {
				// Level gap (a prerequisite entry was dropped, or active training supplies it): assume the
				// previous level completes first.
				spStart = prev
			}
		}
		spNeeded := skillSPForLevel(tc, e.TargetLevel) - spStart
		if spNeeded < 1 {
			spNeeded = 1
		}
		baseline += uint64(spNeeded/spRate*60.0) * 10_000_000
		e.EstEnd = baseline
		simSP[e.TypeID] = skillSPForLevel(tc, e.TargetLevel)
	}
	return nil
}
